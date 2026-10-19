package rental

import (
	"context"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
)

// MaxPortsPerInstance 每个实例最多开放的 Web 端口数。
const MaxPortsPerInstance = 5

type Port struct {
	InstanceID    string    `json:"-"`
	Port          int       `json:"port"`
	Visibility    string    `json:"visibility"` // private / public
	ForcedPrivate bool      `json:"forced_private"`
	Note          string    `json:"note"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

const portCols = `instance_id, port, visibility, forced_private, note, created_at, updated_at`

func scanPort(row pgx.Row) (Port, error) {
	var p Port
	err := row.Scan(&p.InstanceID, &p.Port, &p.Visibility, &p.ForcedPrivate, &p.Note, &p.CreatedAt, &p.UpdatedAt)
	if db.IsNoRows(err) {
		return Port{}, apperr.ErrNotFound
	}
	return p, err
}

func validPort(port int) bool { return port >= 1 && port <= 65535 && port != 22 }

func validVisibility(v string) bool { return v == "private" || v == "public" }

func checkNote(note string) error {
	if utf8.RuneCountInString(note) > 32 {
		return apperr.BadRequest("备注不能超过 32 个字符")
	}
	if strings.IndexFunc(note, unicode.IsControl) >= 0 {
		return apperr.BadRequest("备注不能包含控制字符")
	}
	return nil
}

func (s *Service) ListPorts(ctx context.Context, instanceID string) ([]Port, error) {
	rows, err := s.d.Pool.Query(ctx, `SELECT `+portCols+` FROM instance_ports WHERE instance_id = $1 ORDER BY port`, instanceID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Port, error) { return scanPort(r) })
}

// lockOwned 锁实例行并校验归属；released / failed 不允许管理端口。
func lockOwned(ctx context.Context, tx pgx.Tx, userID int64, id string) (Instance, error) {
	inst, err := lockInstance(ctx, tx, id)
	if err != nil {
		return Instance{}, err
	}
	if inst.UserID != userID {
		return Instance{}, apperr.ErrNotFound
	}
	if inst.State == StateReleased || inst.State == StateFailed || inst.State == StateCancelled {
		return Instance{}, apperr.ErrInvalidState
	}
	return inst, nil
}

func (s *Service) AddPort(ctx context.Context, userID int64, id string, port int, visibility, note, ip string) (Port, error) {
	if !validPort(port) {
		return Port{}, apperr.ErrPortInvalid
	}
	if visibility == "" {
		visibility = "private"
	}
	if !validVisibility(visibility) {
		return Port{}, apperr.BadRequest("可见性只能是 private 或 public")
	}
	note = strings.TrimSpace(note)
	if err := checkNote(note); err != nil {
		return Port{}, err
	}
	now := s.d.Clock.Now()
	var out Port
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		if _, err := lockOwned(ctx, tx, userID, id); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM instance_ports WHERE instance_id = $1`, id).Scan(&n); err != nil {
			return err
		}
		if n >= MaxPortsPerInstance {
			return apperr.ErrPortLimit
		}
		p, err := scanPort(tx.QueryRow(ctx, `INSERT INTO instance_ports (instance_id, port, visibility, note, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $5) RETURNING `+portCols, id, port, visibility, note, now))
		if db.IsUniqueViolation(err) {
			return apperr.ErrPortExists
		}
		if err != nil {
			return err
		}
		out = p
		return audit.Record(ctx, tx, now, audit.Actor{UserID: userID, IP: ip}.Entry("instance.port_add", "instance", id,
			map[string]any{"port": port, "visibility": visibility, "note": note}))
	})
	return out, err
}

// UpdatePort 修改可见性 / 备注（nil 表示不改）；forced_private 的端口不能改回 public。
func (s *Service) UpdatePort(ctx context.Context, userID int64, id string, port int, visibility, note *string, ip string) (Port, error) {
	if visibility != nil && !validVisibility(*visibility) {
		return Port{}, apperr.BadRequest("可见性只能是 private 或 public")
	}
	if note != nil {
		trimmed := strings.TrimSpace(*note)
		note = &trimmed
		if err := checkNote(trimmed); err != nil {
			return Port{}, err
		}
	}
	now := s.d.Clock.Now()
	var out Port
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		if _, err := lockOwned(ctx, tx, userID, id); err != nil {
			return err
		}
		cur, err := scanPort(tx.QueryRow(ctx, `SELECT `+portCols+` FROM instance_ports WHERE instance_id = $1 AND port = $2 FOR UPDATE`, id, port))
		if err != nil {
			return err
		}
		if visibility != nil && *visibility == "public" && cur.ForcedPrivate {
			return apperr.ErrPortForcedPrivate
		}
		if visibility != nil {
			cur.Visibility = *visibility
		}
		if note != nil {
			cur.Note = *note
		}
		out, err = scanPort(tx.QueryRow(ctx, `UPDATE instance_ports SET visibility = $3, note = $4, updated_at = $5 WHERE instance_id = $1 AND port = $2 RETURNING `+portCols,
			id, port, cur.Visibility, cur.Note, now))
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, audit.Actor{UserID: userID, IP: ip}.Entry("instance.port_update", "instance", id,
			map[string]any{"port": port, "visibility": cur.Visibility, "note": cur.Note}))
	})
	return out, err
}

func (s *Service) DeletePort(ctx context.Context, userID int64, id string, port int, ip string) error {
	now := s.d.Clock.Now()
	return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		if _, err := lockOwned(ctx, tx, userID, id); err != nil {
			return err
		}
		if _, err := scanPort(tx.QueryRow(ctx, `DELETE FROM instance_ports WHERE instance_id = $1 AND port = $2 RETURNING `+portCols, id, port)); err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, audit.Actor{UserID: userID, IP: ip}.Entry("instance.port_delete", "instance", id, map[string]any{"port": port}))
	})
}

// AdminForcePortPrivate 后台处置：设为私有并禁止用户改回公开。
func (s *Service) AdminForcePortPrivate(ctx context.Context, actor audit.Actor, id string, port int, reason string) (Port, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return Port{}, apperr.BadRequest("请填写原因")
	}
	now := s.d.Clock.Now()
	var out Port
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		if _, err := lockInstance(ctx, tx, id); err != nil {
			return err
		}
		p, err := scanPort(tx.QueryRow(ctx, `UPDATE instance_ports SET visibility = 'private', forced_private = true, updated_at = $3
			WHERE instance_id = $1 AND port = $2 RETURNING `+portCols, id, port, now))
		if err != nil {
			return err
		}
		out = p
		return audit.Record(ctx, tx, now, actor.Entry("instance.port_force_private", "instance", id, map[string]any{"port": port, "reason": reason}))
	})
	return out, err
}

func (s *Service) AdminDeletePort(ctx context.Context, actor audit.Actor, id string, port int, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return apperr.BadRequest("请填写原因")
	}
	now := s.d.Clock.Now()
	return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		if _, err := lockInstance(ctx, tx, id); err != nil {
			return err
		}
		if _, err := scanPort(tx.QueryRow(ctx, `DELETE FROM instance_ports WHERE instance_id = $1 AND port = $2 RETURNING `+portCols, id, port)); err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("instance.port_delete", "instance", id, map[string]any{"port": port, "reason": reason}))
	})
}

// IngressRoute 是 Web 入口路由表的一行（非终态实例的每个端口）。
type IngressRoute struct {
	InstanceID string
	UserID     int64
	Port       int
	Visibility string
	State      string
	Suspended  bool
	LanIP      string
	HostPubkey string
	// ScheduledStartAt 仅预订（reserved）实例有值，入口状态页用来显示开始时间。
	ScheduledStartAt *time.Time
}

func (s *Service) ListIngressRoutes(ctx context.Context) ([]IngressRoute, error) {
	rows, err := s.d.Pool.Query(ctx, `SELECT p.instance_id, i.user_id, p.port, p.visibility, i.state, i.suspended_at IS NOT NULL, m.lan_ip, i.ssh_host_pubkey, i.scheduled_start_at
		FROM instance_ports p JOIN instances i ON i.id = p.instance_id JOIN machines m ON m.id = i.machine_id
		WHERE i.state IN ('reserved', 'creating', 'running', 'stopped') ORDER BY p.instance_id, p.port`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (IngressRoute, error) {
		var x IngressRoute
		err := r.Scan(&x.InstanceID, &x.UserID, &x.Port, &x.Visibility, &x.State, &x.Suspended, &x.LanIP, &x.HostPubkey, &x.ScheduledStartAt)
		return x, err
	})
}

// PortCounts 批量返回实例的端口数（列表视图用，避免 N+1）。
func (s *Service) PortCounts(ctx context.Context, instanceIDs []string) (map[string]int, error) {
	out := map[string]int{}
	if len(instanceIDs) == 0 {
		return out, nil
	}
	rows, err := s.d.Pool.Query(ctx, `SELECT instance_id, count(*) FROM instance_ports WHERE instance_id = ANY($1) GROUP BY instance_id`, instanceIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}
