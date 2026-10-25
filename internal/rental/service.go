// 各路径的实际加锁顺序（规格 §14）：
//   PlaceBatch / Place：idem 咨询锁 → [booking:<uid> 咨询锁] → [插 rental_batches] → machines(NO KEY UPDATE, SKIP LOCKED)
//     → 每台 savepoint 内 [machine 标 allocated] → 插实例（FK：machine_groups KEY SHARE）/ lease → orders → wallet → agent_commands
//     整个事务遇 40P01 / 40001 重试一次（withRetry）
//   ProvisionDue：instance → 自己的 lease → machine(NO KEY UPDATE) → [改派：machines SKIP LOCKED] → agent_commands
//   机器行锁一律 FOR NO KEY UPDATE：插入引用 machines(id) 的行会取 FOR KEY SHARE，与 FOR UPDATE 互斥、与 NO KEY UPDATE 不互斥。
//   后台改派：instance → lease → machines（自动挑选 SKIP LOCKED；指定机器 NO KEY UPDATE 等待）
//   Renew / AdminExtend / 自动续费：[idem] → instance → 自己的 lease(读 cap) → orders → wallet → 更新 lease（不锁机器）
//   取消：instance → lease → agent_commands（CancelCommands）→ orders → wallet
//   HandleCommandResult：instance → agent_commands → lease(refreshLease) → machine(Release)
//     预订首次开通成功（startBooked）：instance → agent_commands → [迟开通补偿：orders → wallet] → lease(refreshLease)
//     预订 Create 失败 / 超时（reassignAfterCreateFailure）：instance → [agent_commands] → 自己的 lease → 旧机器(MarkFaulty)
//       → 候选 machines(SKIP LOCKED) → agent_commands；过宽限期则平台取消：→ agent_commands → orders → wallet
//   ExpireDue / WipeStale / Suspend / Resume / RefreshCapped / beginWipe：instance → lease → agent_commands
//     Resume 被下一条预订封顶时补偿：instance → lease(refreshLease) → orders → wallet
//   AdminDeleteGroup：machine_groups(FOR UPDATE) → instances(清 group_id) → machines(只读计数) → 删机组
//     下单插实例取机组行 KEY SHARE：先插的一方先拿到锁，另一方等它提交；删除方不锁机器，与下单不成环
//   AdminMigrate：原实例 → machines(SKIP LOCKED) → 新 lease → agent_commands → 旧 lease 收缩
//   应用部署钩子（AppHooks，计划 7）：都在已持实例行锁之后调用，再锁 app_deployments → instance_ports（appdeploy 一律同序）
//   PlaceSystem（计划 9）：machines(NO KEY UPDATE，等待) → 插实例 / lease → agent_commands；不碰 orders / wallet
//   ReleaseSystem（计划 9）：与 Release 相同（instance → lease → agent_commands，经 beginWipe）
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
	// 计划 9：系统账号（users.system AND phone='system:llm'）的 ID，系统实例挂在它名下；0 = 未配置（PlaceSystem 报错）
	SystemUserID int64
	// 计划 10：容器 sshd 的基准端口（SPARK_UPSTREAM_SSH_PORT）；<= 0 视为 2222。Create 指令按 Instance.SSHPort 带端口。
	SSHBasePort int
}

type KeyCounter interface {
	CountSSHKeys(ctx context.Context, userID int64) (int, error)
}

type Service struct {
	d        Deps
	appHooks AppHooks // 计划 7：应用部署钩子（SetAppHooks）；nil = noop
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
	// 计划 9：平台节点的系统实例（PlaceSystem）：无订单、expires_at 恒为 NULL、不到期不计费，后台计费类操作一律拒绝
	System bool
}

// DefaultSSHPort 是未配置基准端口时容器 sshd 的端口。
const DefaultSSHPort = 2222

// SSHPort 返回实例容器 sshd 在机器上的端口（平台上游 SSH 拨号与 Create 指令共用）。
// 目前一台机器只有一个实例，恒为基准端口（base <= 0 时为 2222）；共享机落地后再加槽位偏移 base + slot（规格 §1.3 S5）。
func (i Instance) SSHPort(base int) int {
	if base <= 0 {
		return DefaultSSHPort
	}
	return base
}

// InstanceSSHPort 按实例 ID 给出容器 sshd 端口（Instance.SSHPort(Deps.SSHBasePort)），供只查了实例 ID 的上游拨号方
// （应用部署 Runner / 实时日志）与 AccessInfo、IngressRoute 保持一致。共享机落地后改为按实例槽位算。
func (s *Service) InstanceSSHPort(instanceID string) int {
	return Instance{ID: instanceID}.SSHPort(s.d.SSHBasePort)
}

// enqueueCreate 给实例所在机器入队 Create 指令：镜像地址由调用方解析，端口按 Instance.SSHPort，份数恒为 1（整机）。
// 所有开通路径（即时下单、预订开通、改派、换机）都经这里，保证指令形态一致。
func (s *Service) enqueueCreate(ctx context.Context, q db.Querier, inst Instance, imageRef string) error {
	_, err := s.d.Fleet.Enqueue(ctx, q, inst.MachineID, inst.ID, fleet.CmdCreateInstance,
		fleet.CreateInstancePayload{ImageRef: imageRef, SSHPort: inst.SSHPort(s.d.SSHBasePort), ShareSlots: 1})
	return err
}

// Booked 表示这是一个预订（有预定开始时刻）的实例。
func (i Instance) Booked() bool { return i.ScheduledStartAt != nil }

func (i Instance) Suspended() bool { return i.SuspendedAt != nil }

// resumeBase 暂停实例恢复计时的起点：预订实例在开始时刻之前恢复仍从开始时刻起算（早开通不计费，已付时段不被缩短）。
func (i Instance) resumeBase(now time.Time) time.Time {
	if i.Booked() && now.Before(*i.ScheduledStartAt) {
		return *i.ScheduledStartAt
	}
	return now
}

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
	reassign_count, provisioned_at, cancelled_at, system`

func scanInstance(row pgx.Row) (Instance, error) {
	var i Instance
	var autoDur string
	err := row.Scan(&i.ID, &i.UserID, &i.MachineID, &i.ProductID, &i.ImageID, &i.State, &i.PendingSeconds, &i.ExpiresAt,
		&i.StoppedAt, &i.SuspendedAt, &i.PausedRemaining, &i.Reminded24h, &i.Reminded1h, &i.CreatedAt, &i.PasswordAuth, &i.SSHHostPubkey,
		&i.ScheduledStartAt, &i.GroupID, &i.BatchID, &i.AutoRenew, &autoDur, &i.AutoRenewFailedAt, &i.AutoRenewFailedReason,
		&i.ReassignCount, &i.ProvisionedAt, &i.CancelledAt, &i.System)
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
