// 锁顺序：instance → agent_commands → orders → wallet；machine → wallet。
// 所有事务按此顺序加行锁，避免并发路径之间死锁。

package rental

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/secretbox"
	"spark-cloud/internal/wallet"
)

const (
	StateCreating = "creating"
	StateRunning  = "running"
	StateStopped  = "stopped"
	StateWiping   = "wiping"
	StateReleased = "released"
	StateFailed   = "failed"

	CreateTimeout   = 10 * time.Minute // 开通超时，超时判失败并退款
	RetainAfterStop = 6 * time.Hour    // 到期停机后保留数据的时长
	OfflineTimeout  = 5 * time.Minute  // Agent 失联判定
)

type UserChecker interface {
	CanRent(ctx context.Context, userID int64) error
}

type Notifier interface {
	NotifyExpiry(ctx context.Context, userID int64, instanceID string, remaining time.Duration) error
}

type Deps struct {
	Pool     *pgxpool.Pool
	Clock    clock.Clock
	Box      *secretbox.Box
	Log      *slog.Logger
	Users    UserChecker
	Wallet   *wallet.Service
	Catalog  *catalog.Service
	Fleet    *fleet.Service
	Notifier Notifier
}

type Service struct {
	d Deps
}

func NewService(d Deps) *Service {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	return &Service{d: d}
}

type Instance struct {
	ID              string
	UserID          int64
	MachineID       int64
	ProductID       int64
	ImageID         int64
	State           string
	PendingSeconds  int64
	ExpiresAt       *time.Time
	StoppedAt       *time.Time
	SuspendedAt     *time.Time
	PausedRemaining *int64
	Reminded24h     bool
	Reminded1h      bool
	CreatedAt       time.Time
}

func (i Instance) Suspended() bool { return i.SuspendedAt != nil }

// Remaining 返回剩余可用时长；暂停中返回冻结的剩余时长；非运行状态返回 0。
func (i Instance) Remaining(now time.Time) time.Duration {
	if i.State != StateRunning {
		return 0
	}
	if i.SuspendedAt != nil && i.PausedRemaining != nil {
		return time.Duration(*i.PausedRemaining) * time.Second
	}
	if i.ExpiresAt == nil {
		return 0
	}
	if r := i.ExpiresAt.Sub(now); r > 0 {
		return r
	}
	return 0
}

const instanceCols = `id, user_id, machine_id, product_id, image_id, state, pending_seconds, expires_at,
	stopped_at, suspended_at, paused_remaining, reminded_24h, reminded_1h, created_at`

func scanInstance(row pgx.Row) (Instance, error) {
	var i Instance
	err := row.Scan(&i.ID, &i.UserID, &i.MachineID, &i.ProductID, &i.ImageID, &i.State, &i.PendingSeconds, &i.ExpiresAt,
		&i.StoppedAt, &i.SuspendedAt, &i.PausedRemaining, &i.Reminded24h, &i.Reminded1h, &i.CreatedAt)
	if db.IsNoRows(err) {
		return Instance{}, apperr.ErrNotFound
	}
	return i, err
}

func collectInstances(rows pgx.Rows, err error) ([]Instance, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Instance, error) { return scanInstance(r) })
}

func lockInstance(ctx context.Context, tx pgx.Tx, id string) (Instance, error) {
	return scanInstance(tx.QueryRow(ctx, `SELECT `+instanceCols+` FROM instances WHERE id = $1 FOR UPDATE`, id))
}

func recordEvent(ctx context.Context, q db.Querier, id, from, to, reason string, at time.Time) error {
	_, err := q.Exec(ctx, `INSERT INTO instance_events (instance_id, from_state, to_state, reason, at) VALUES ($1, $2, $3, $4, $5)`,
		id, from, to, reason, at)
	return err
}

// reminderFlags 计算到期提醒标记：剩余时长已不超过阈值的提醒视为「已发送」，避免短租期一开通就收到提醒。
func reminderFlags(remaining time.Duration) (r24, r1 bool) {
	return remaining <= 24*time.Hour, remaining <= time.Hour
}

func seconds(d catalog.Duration) int64 { return int64(d.Length() / time.Second) }
