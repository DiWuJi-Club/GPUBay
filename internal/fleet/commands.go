package fleet

import (
	"context"
	"encoding/json"
	"errors"
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
)

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
}

const commandCols = `id, machine_id, instance_id, type, payload, status, result, attempts, created_at`

func scanCommand(row pgx.Row) (Command, error) {
	var c Command
	var payload []byte
	err := row.Scan(&c.ID, &c.MachineID, &c.InstanceID, &c.Type, &payload, &c.Status, &c.Result, &c.Attempts, &c.CreatedAt)
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
func (s *Service) CancelCommands(ctx context.Context, q db.Querier, instanceID, reason string) error {
	_, err := q.Exec(ctx, `UPDATE agent_commands SET status = 'failed', result = $2, updated_at = $3
		WHERE instance_id = $1 AND status IN ('pending', 'sent')`, instanceID, "cancelled: "+reason, s.clock.Now())
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
