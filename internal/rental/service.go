// 各路径的实际加锁顺序（规格 §14）：
//   PlaceBatch / Place：idem 咨询锁 → [booking:<uid> 咨询锁] → [插 rental_batches] → machines(NO KEY UPDATE, SKIP LOCKED)
//     → 每台 savepoint 内 [machine 标 allocated] → 插实例（FK：machine_groups KEY SHARE）/ lease → orders → wallet → agent_commands
//     整个事务遇 40P01 / 40001 重试一次（withRetry）
//   ProvisionDue：instance → 自己的 lease → machine(NO KEY UPDATE) → [改派：machines SKIP LOCKED] → agent_commands
//   机器行锁一律 FOR NO KEY UPDATE：插入引用 machines(id) 的行会取 FOR KEY SHARE，与 FOR UPDATE 互斥、与 NO KEY UPDATE 不互斥。
//   后台改派：instance → lease → machines(SKIP LOCKED)
//   Renew / AdminExtend / 自动续费：[idem] → instance → 自己的 lease(读 cap) → orders → wallet → 更新 lease（不锁机器）
//   取消：instance → lease → agent_commands（CancelCommands）→ orders → wallet
//   HandleCommandResult：instance → agent_commands → lease(refreshLease) → machine(Release)
//   ExpireDue / WipeStale / Suspend / Resume / RefreshCapped / beginWipe：instance → lease → agent_commands
//   AdminDeleteGroup：machine_groups(FOR UPDATE) → instances(清 group_id) → machines(只读计数) → 删机组
//     下单插实例取机组行 KEY SHARE：先插的一方先拿到锁，另一方等它提交；删除方不锁机器，与下单不成环
//   AdminMigrate：原实例 → machines(SKIP LOCKED) → 新 lease → agent_commands → 旧 lease 收缩
// 持机器行锁的事务只会在插入 lease 撞到别的事务未提交的 lease 增长时等待；而让 lease 增长的路径都不等机器锁，等待链不成环。

package rental

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/pki"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/secretbox"
	"spark-cloud/internal/wallet"
)

const (
	StateCreating  = "creating"
	StateRunning   = "running"
	StateStopped   = "stopped"
	StateWiping    = "wiping"
	StateReleased  = "released"
	StateFailed    = "failed"
	StateReserved  = "reserved"  // 计划 6：已预订、未开通
	StateCancelled = "cancelled" // 计划 6：开通前取消（终态）

	CreateTimeout   = 20 * time.Minute // 开通超时（从 created_at 计），大镜像首次拉取可能较慢
	RetainAfterStop = 6 * time.Hour    // 到期停机后保留数据的时长
	OfflineTimeout  = 5 * time.Minute  // Agent 失联判定

	// 指令超时（从 sent_at 计，只对在线机器），由清扫器 FailTimedOutCommands 判定
	CreateCommandTimeout = 15 * time.Minute
	StartCommandTimeout  = 5 * time.Minute
	StopCommandTimeout   = 5 * time.Minute
	WipeCommandTimeout   = 30 * time.Minute
	PullCommandTimeout   = 60 * time.Minute
)

type UserChecker interface {
	CanRent(ctx context.Context, userID int64) error
}

type Notifier interface {
	NotifyExpiry(ctx context.Context, userID int64, instanceID string, remaining time.Duration) error
	// 计划 6：自动续费失败（reason：余额不足 / 机器已有后续预订）与预订被平台取消（reason：无法安排机器 / 账号不可租用 / 平台取消）
	NotifyAutoRenewFailed(ctx context.Context, userID int64, instanceID, reason string) error
	NotifyBookingCancelled(ctx context.Context, userID int64, instanceID, reason string) error
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
	// 计划 3
	Keys       KeyCounter                                 // 关闭密码登录前校验用户至少有一把公钥；nil 视为 0 把
	HostKeyGen func() (pub string, pem []byte, err error) // 实例主机密钥生成；nil → pki.GenerateInstanceHostKey
}

type KeyCounter interface {
	CountSSHKeys(ctx context.Context, userID int64) (int, error)
}

type Service struct {
	d Deps
}

func NewService(d Deps) *Service {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.HostKeyGen == nil {
		d.HostKeyGen = pki.GenerateInstanceHostKey
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
	// 计划 3：密码登录开关与平台生成的容器主机公钥（authorized_keys 一行；存量实例为空，首连信任后写回）
	PasswordAuth  bool
	SSHHostPubkey string
	// 计划 6：预订与自动续费
	ScheduledStartAt      *time.Time // 预订开始时刻；即时订单为 nil
	GroupID               *int64     // 下单时选的机组（改派范围）；nil = 不限
	BatchID               *string    // 一次多台的批次；单台旧接口为 nil
	AutoRenew             bool
	AutoRenewDuration     catalog.Duration // 自动续费档位（默认 = 首单档位）
	AutoRenewFailedAt     *time.Time
	AutoRenewFailedReason string // insufficient_balance / renew_blocked
	ReassignCount         int
	ProvisionedAt         *time.Time // 预订开通（reserved → creating）时刻
	CancelledAt           *time.Time
}

// Booked 表示这是一个预订（有预定开始时刻）的实例。
func (i Instance) Booked() bool { return i.ScheduledStartAt != nil }

func (i Instance) Suspended() bool { return i.SuspendedAt != nil }

// Remaining 返回剩余可用时长；暂停中返回冻结的剩余时长；预订实例在开始时刻之前返回整段预订时长（早开通不计费）；非运行状态返回 0。
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
	if i.ScheduledStartAt != nil && now.Before(*i.ScheduledStartAt) {
		return i.ExpiresAt.Sub(*i.ScheduledStartAt)
	}
	if r := i.ExpiresAt.Sub(now); r > 0 {
		return r
	}
	return 0
}

const instanceCols = `id, user_id, machine_id, product_id, image_id, state, pending_seconds, expires_at,
	stopped_at, suspended_at, paused_remaining, reminded_24h, reminded_1h, created_at, password_auth, ssh_host_pubkey,
	scheduled_start_at, group_id, batch_id, auto_renew, auto_renew_duration, auto_renew_failed_at, auto_renew_failed_reason,
	reassign_count, provisioned_at, cancelled_at`

func scanInstance(row pgx.Row) (Instance, error) {
	var i Instance
	var autoDur string
	err := row.Scan(&i.ID, &i.UserID, &i.MachineID, &i.ProductID, &i.ImageID, &i.State, &i.PendingSeconds, &i.ExpiresAt,
		&i.StoppedAt, &i.SuspendedAt, &i.PausedRemaining, &i.Reminded24h, &i.Reminded1h, &i.CreatedAt, &i.PasswordAuth, &i.SSHHostPubkey,
		&i.ScheduledStartAt, &i.GroupID, &i.BatchID, &i.AutoRenew, &autoDur, &i.AutoRenewFailedAt, &i.AutoRenewFailedReason,
		&i.ReassignCount, &i.ProvisionedAt, &i.CancelledAt)
	if db.IsNoRows(err) {
		return Instance{}, apperr.ErrNotFound
	}
	i.AutoRenewDuration = catalog.Duration(autoDur)
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
