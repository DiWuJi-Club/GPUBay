package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
)

type CommandType string

const (
	CmdCreateInstance CommandType = "create_instance"
	CmdStartInstance  CommandType = "start_instance"
	CmdStopInstance   CommandType = "stop_instance"
	CmdWipeInstance   CommandType = "wipe_instance"
	CmdPullImage      CommandType = "pull_image" // 镜像预拉取，instance_id 为空
)

type PullImagePayload struct {
	ImageRef string `json:"image_ref"`
}

type CreateInstancePayload struct {
	ImageRef string `json:"image_ref"`
}

type Command struct {
	ID         string
	MachineID  int64
	InstanceID string
	Type       CommandType
	Payload    json.RawMessage
	Status     string
	Result     string
	Attempts   int
	CreatedAt  time.Time
	SentAt     *time.Time
}

const commandCols = `id, machine_id, instance_id, type, payload, status, result, attempts, created_at, sent_at`

func scanCommand(row pgx.Row) (Command, error) {
	var c Command
	var payload []byte
	err := row.Scan(&c.ID, &c.MachineID, &c.InstanceID, &c.Type, &payload, &c.Status, &c.Result, &c.Attempts, &c.CreatedAt, &c.SentAt)
	if db.IsNoRows(err) {
		return Command{}, apperr.ErrNotFound
	}
	c.Payload = payload
	return c, err
}

// Enqueue 写入一条待投递给 Agent 的指令；通常在业务事务内调用。
func (s *Service) Enqueue(ctx context.Context, q db.Querier, machineID int64, instanceID string, t CommandType, payload any) (string, error) {
	body := []byte("{}")
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return "", err
		}
		body = b
	}
	id := uuid.NewString()
	now := s.clock.Now()
	_, err := q.Exec(ctx, `INSERT INTO agent_commands (id, machine_id, instance_id, type, payload, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $6)`, id, machineID, instanceID, string(t), body, now)
	if db.IsForeignKeyViolation(err) {
		return "", apperr.BadRequest("机器不存在")
	}
	return id, err
}

// PendingCommands 返回某台机器尚未完成的指令（按创建时间排序）。
func (s *Service) PendingCommands(ctx context.Context, machineID int64) ([]Command, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+commandCols+` FROM agent_commands
		WHERE machine_id = $1 AND status IN ('pending', 'sent') ORDER BY created_at, id`, machineID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Command, error) { return scanCommand(r) })
}

// CommandMachineID 读取指令所属机器 ID，未知指令返回 apperr.ErrNotFound（网关据此只接受本机指令的回报）。
func (s *Service) CommandMachineID(ctx context.Context, q db.Querier, id string) (int64, error) {
	var machineID int64
	err := q.QueryRow(ctx, `SELECT machine_id FROM agent_commands WHERE id = $1`, id).Scan(&machineID)
	if db.IsNoRows(err) {
		return 0, apperr.ErrNotFound
	}
	return machineID, err
}

// CommandInstanceID 不加锁地读取指令所属实例 ID（无实例时为空串），未知指令返回 apperr.ErrNotFound。
// 调用方据此先锁实例再完成指令，保持「instance → agent_commands」的全局加锁顺序。
func (s *Service) CommandInstanceID(ctx context.Context, q db.Querier, id string) (string, error) {
	var instanceID string
	err := q.QueryRow(ctx, `SELECT instance_id FROM agent_commands WHERE id = $1`, id).Scan(&instanceID)
	if db.IsNoRows(err) {
		return "", apperr.ErrNotFound
	}
	return instanceID, err
}

// CompleteCommand 记录指令结果。applied 为 false 表示该指令此前已完成或已取消，调用方应忽略本次回报。
func (s *Service) CompleteCommand(ctx context.Context, q db.Querier, id string, ok bool, result string) (Command, bool, error) {
	status := "failed"
	if ok {
		status = "done"
	}
	cmd, err := scanCommand(q.QueryRow(ctx, `UPDATE agent_commands SET status = $2, result = $3, updated_at = $4
		WHERE id = $1 AND status IN ('pending', 'sent') RETURNING `+commandCols, id, status, result, s.clock.Now()))
	if err == nil {
		return cmd, true, nil
	}
	if !errors.Is(err, apperr.ErrNotFound) {
		return Command{}, false, err
	}
	cmd, err = scanCommand(q.QueryRow(ctx, `SELECT `+commandCols+` FROM agent_commands WHERE id = $1`, id))
	if err != nil {
		return Command{}, false, err
	}
	return cmd, false, nil
}

// CancelCommands 把某实例所有未完成的指令标记为失败（被新指令取代时使用）。
// 多行更新先按 id 排序加锁，与 ClaimPending/TouchSent 的加锁顺序一致，避免互相死锁。
func (s *Service) CancelCommands(ctx context.Context, q db.Querier, instanceID, reason string) error {
	_, err := q.Exec(ctx, `WITH l AS (
			SELECT id FROM agent_commands WHERE instance_id = $1 AND status IN ('pending', 'sent') ORDER BY id FOR UPDATE
		)
		UPDATE agent_commands SET status = 'failed', result = $2, updated_at = $3 FROM l WHERE agent_commands.id = l.id`,
		instanceID, "cancelled: "+reason, s.clock.Now())
	return err
}

// HasOpenCommand 报告某实例是否还有未完成（pending/sent）的指定类型指令。
func (s *Service) HasOpenCommand(ctx context.Context, q db.Querier, instanceID string, t CommandType) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_commands
		WHERE instance_id = $1 AND type = $2 AND status IN ('pending', 'sent'))`, instanceID, string(t)).Scan(&ok)
	return ok, err
}

// CommandsForInstance 返回实例的指令记录（新到旧，含已完成与失败的）。
func (s *Service) CommandsForInstance(ctx context.Context, instanceID string, limit int) ([]Command, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+commandCols+` FROM agent_commands
		WHERE instance_id = $1 ORDER BY created_at DESC, id LIMIT $2`, instanceID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Command, error) { return scanCommand(r) })
}

// ClaimedCommand 是刚被本副本认领的指令，ConnID 为认领时机器的归属连接。
type ClaimedCommand struct {
	Command
	ConnID string
}

// ClaimPending 原子认领本副本持有的流（connIDs）所属机器的 pending 指令：状态改为 sent、attempts+1、写 sent_at。
// 返回行里的 ConnID 若已不是调用方持有的流（SQL 期间被顶掉），调用方不推送，指令保持 sent 由补推处理。
// 先按 id 排序锁定指令行再更新，与 CancelCommands/TouchSent 的加锁顺序一致，避免互相死锁。
func (s *Service) ClaimPending(ctx context.Context, connIDs []string) ([]ClaimedCommand, error) {
	if len(connIDs) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `WITH l AS (
			SELECT c.id, m.agent_conn_id FROM agent_commands c JOIN machines m ON m.id = c.machine_id
			WHERE m.agent_conn_id = ANY($1) AND c.status = 'pending' ORDER BY c.id FOR UPDATE OF c
		)
		UPDATE agent_commands c
		SET status = 'sent', attempts = c.attempts + 1, sent_at = $2, updated_at = $2
		FROM l
		WHERE c.id = l.id
		RETURNING c.id, c.machine_id, c.instance_id, c.type, c.payload, c.status, c.result, c.attempts, c.created_at, c.sent_at, l.agent_conn_id`,
		connIDs, s.clock.Now())
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ClaimedCommand, error) {
		var c ClaimedCommand
		var payload []byte
		err := r.Scan(&c.ID, &c.MachineID, &c.InstanceID, &c.Type, &payload, &c.Status, &c.Result, &c.Attempts, &c.CreatedAt, &c.SentAt, &c.ConnID)
		c.Payload = payload
		return c, err
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// SentCommands 返回这些机器上已下发、尚未回报的指令（补推用），按机器、创建时间排序。
func (s *Service) SentCommands(ctx context.Context, machineIDs []int64) ([]Command, error) {
	if len(machineIDs) == 0 {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT `+commandCols+` FROM agent_commands
		WHERE machine_id = ANY($1) AND status = 'sent' ORDER BY machine_id, created_at, id`, machineIDs)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Command, error) { return scanCommand(r) })
}

// TouchSent 补推后刷新 sent_at，超时从头计时。多行更新先按 id 排序加锁（见 CancelCommands）。
func (s *Service) TouchSent(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `WITH l AS (
			SELECT id FROM agent_commands WHERE id = ANY($1) AND status = 'sent' ORDER BY id FOR UPDATE
		)
		UPDATE agent_commands SET sent_at = $2, updated_at = $2 FROM l WHERE agent_commands.id = l.id`, ids, s.clock.Now())
	return err
}

// SentBefore 返回在线机器上 sent_at 不晚于 before 的已下发指令（超时判定的候选，按类型的超时由调用方再筛）。
func (s *Service) SentBefore(ctx context.Context, before time.Time) ([]Command, error) {
	rows, err := s.pool.Query(ctx, `SELECT c.id, c.machine_id, c.instance_id, c.type, c.payload, c.status, c.result, c.attempts, c.created_at, c.sent_at
		FROM agent_commands c JOIN machines m ON m.id = c.machine_id
		WHERE c.status = 'sent' AND m.agent_online AND c.sent_at IS NOT NULL AND c.sent_at <= $1
		ORDER BY c.sent_at, c.id`, before)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Command, error) { return scanCommand(r) })
}

// HasAnyOpenCommand 报告某实例是否还有任何未完成（pending/sent）的指令，不限类型。
func (s *Service) HasAnyOpenCommand(ctx context.Context, q db.Querier, instanceID string) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_commands WHERE instance_id = $1 AND status IN ('pending', 'sent'))`, instanceID).Scan(&ok)
	return ok, err
}

// OnlineMachineIDs 返回当前 Agent 在线的全部机器 id。
func (s *Service) OnlineMachineIDs(ctx context.Context) ([]int64, error) {
	return s.queryIDs(ctx, `SELECT id FROM machines WHERE agent_online ORDER BY id`)
}

// BroadcastPullImage 向所有在线机器下发镜像预拉取；返回下发台数。
func (s *Service) BroadcastPullImage(ctx context.Context, ref string) (int, error) {
	ids, err := s.OnlineMachineIDs(ctx)
	if err != nil {
		return 0, err
	}
	err = db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		for _, id := range ids {
			if _, err := s.Enqueue(ctx, tx, id, "", CmdPullImage, PullImagePayload{ImageRef: ref}); err != nil {
				return err
			}
		}
		return nil
	})
	return len(ids), err
}
