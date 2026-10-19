// Package referral 实现邀请返利（计划 8）：邀请码、注册时绑定、按被邀请人实付消费计提、延迟发放、退款扣回与欠账、风险标记与后台裁决。
//
// 边界：只写 referral_* 表；只读 wallet_ledger（扫描）、users / kyc_records / user_devices（经 UserLookup）、orders（经 OrderLookup）；
// 资金变动全部经 wallet.Apply / ApplyUpTo，每笔都有唯一 ref_id（referral/<order>、referral/<order>-c<n>）。
//
// 加锁顺序（规格 §14）：
//
//	Bind（identity 事务内）：users(被邀请人, INSERT 持有) → users(码主) FOR KEY SHARE → referral_accounts(码主) FOR SHARE → 插 referral_bindings
//	  （users(码主) 必须先于 referral_accounts 显式加锁：否则插绑定时的外键检查才对码主取 KEY SHARE，
//	  成了 accounts → users，与码主本人 OnKYCVerified 的 users FOR UPDATE → accounts FOR UPDATE 反向，会死锁。）
//	计提 / 退款处理：users(邀请人, 被邀请人) FOR KEY SHARE → referral_accounts(邀请人) FOR UPDATE → [幂等复查] → referral_rewards(订单) FOR UPDATE → [wallet_accounts(邀请人)，仅扣回]
//	  （users 同理须先于 accounts：插 referral_rewards / wallet_ledger 的外键检查会对 users 取 KEY SHARE。
//	  alreadyProcessed 在账户锁之后复查，并发处理同一行的后来者看到已处理即返回，不撞唯一约束。）
//	发放：users(邀请人, 被邀请人) FOR KEY SHARE → referral_accounts(邀请人) FOR UPDATE → referral_rewards(订单) FOR UPDATE SKIP LOCKED →
//	  [同身份复查命中：referral_bindings → referral_rewards(该绑定其余 pending) 作废] → wallet_accounts(邀请人)
//	  （users 同计提：写 wallet_ledger 的外键检查会对 users(邀请人) 取 KEY SHARE，须先于账户锁。）
//	冻结 / 解冻 / 撤销 / 通过：users(邀请人[, 被邀请人]) FOR KEY SHARE → referral_accounts(邀请人) FOR UPDATE → referral_bindings → referral_rewards
//	  （与发放同序；冻结与同一轮发放的互斥靠账户锁：releaseOne 在账户锁内复核账户 / 绑定状态。）
//	OnKYCVerified（identity 事务内）：users(本人, 已持有) → referral_accounts(邀请人) FOR UPDATE → referral_bindings → referral_rewards
//	  （可能锁两行 referral_accounts：先本人的邀请人，再本人自己的账户。安全，因为邀请关系是无环的森林
//	  ——绑定只在被邀请人注册时建立，邀请人必先存在——所有路径都按「祖先 → 后代」加锁，不会反向等待。）
//	rental / billing 的扣款 / 退款事务只锁 wallet_accounts，从不锁 referral 表 → 等待链不成环。
package referral

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/wallet"
)

const (
	CodeLen         = 8
	CodeAlphabet    = "0123456789ABCDEFGHJKMNPQRSTVWXYZ" // Crockford Base32，无 I L O U
	CodeRetries     = 5
	InviteStoreDays = 30 // 控制台 localStorage 归因窗口（R7）

	ScanLookback = time.Hour
	ScanLag      = 2 * time.Minute
	ScanBatch    = 500
	// SkipGrace：扣款的订单查不到 / 不属于付款人、退款找不到任何被绑定的扣款时，先按暂时跳过（钉住水位、每轮重试，
	// 留给数据修复的时间）；流水过了这么久仍如此则视为永久，写占位事件（扣款 void、退款 refund(0)），不再钉住水位。
	SkipGrace    = 24 * time.Hour
	ReleaseBatch = 200

	BurstThreshold    = 20
	BurstWindow       = 24 * time.Hour // burst：码主在此窗口内的绑定数（含本次）达到 BurstThreshold
	SameIPWindow      = 24 * time.Hour
	MaxDevicesPerUser = 10
	InviterWindow     = 30 * 24 * time.Hour // max_per_inviter_30d 的窗口
	MaxUserAgent      = 200

	// 费率版本 effective_from 允许的「过去」容差（表单从打开到提交的延迟）
	EffectiveTolerance = 5 * time.Minute
)

const (
	EventAccrue     = "accrue"
	EventRefund     = "refund"
	EventSettle     = "settle"
	EventClawback   = "clawback"
	EventDebtAdd    = "debt_add"
	EventDebtOffset = "debt_offset"
	EventVoid       = "void"
	EventCapped     = "capped"

	StatusPending = "pending"
	StatusSettled = "settled"
	StatusVoid    = "void"

	BindingActive  = "active"
	BindingHeld    = "held"
	BindingRevoked = "revoked"

	AccountActive = "active"
	AccountFrozen = "frozen"

	FlagSameIP       = "same_ip"
	FlagSameDevice   = "same_device"
	FlagBurst        = "burst"
	FlagSameIdentity = "same_identity"

	HoldKYCCheckFailed = "kyc_check_failed" // hold_reason：实名同身份判定失败待人工核查（发放时转 held）

	RefType = "referral" // wallet_ledger.ref_type
)

type WalletOps interface {
	Apply(ctx context.Context, tx pgx.Tx, e wallet.Entry) (int64, error)
	ApplyUpTo(ctx context.Context, tx pgx.Tx, e wallet.Entry) (applied, after int64, err error)
}

// UserState 是 referral 需要知道的用户事实（由 identity 实现）。
type UserState struct {
	Status      string
	KYCStatus   string
	Phone       string
	LastLoginIP string
	LastLoginAt *time.Time
}

type UserLookup interface {
	UserState(ctx context.Context, q db.Querier, id int64) (UserState, error)
	HasDevice(ctx context.Context, q db.Querier, userID int64, deviceID string) (bool, error)
	// VerifiedFingerprints 返回每个用户全部 verified 且指纹非空的实名记录的指纹（去重；同一用户可能既有个人又有企业实名）；
	// 没有的用户不在 map 里。
	VerifiedFingerprints(ctx context.Context, q db.Querier, userIDs []int64) (map[int64][]string, error)
}

type OrderLookup interface {
	// OrderForReferral 返回订单的用户与（预订单的）计划开始时刻；不存在返回 apperr.ErrNotFound。
	OrderForReferral(ctx context.Context, q db.Querier, orderID string) (userID int64, scheduledStartAt *time.Time, err error)
}

type Deps struct {
	Pool   *pgxpool.Pool
	Clock  clock.Clock
	Log    *slog.Logger
	Wallet WalletOps
	Users  UserLookup
	Orders OrderLookup
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

type Account struct {
	UserID       int64     `json:"user_id"`
	Code         string    `json:"code"`
	Status       string    `json:"status"`
	FrozenReason string    `json:"frozen_reason"`
	Debt         int64     `json:"debt"`
	CreatedAt    time.Time `json:"created_at"`
}

const accountCols = `user_id, code, status, frozen_reason, debt, created_at`

func scanAccount(row pgx.Row) (Account, error) {
	var a Account
	err := row.Scan(&a.UserID, &a.Code, &a.Status, &a.FrozenReason, &a.Debt, &a.CreatedAt)
	return a, err
}

// lockAccount 按加锁顺序的第一步锁住邀请人账户行；没有账户行（从未生成过码，因而不可能有绑定）返回 ErrNotFound。
func lockAccount(ctx context.Context, tx pgx.Tx, userID int64) (Account, error) {
	a, err := scanAccount(tx.QueryRow(ctx, `SELECT `+accountCols+` FROM referral_accounts WHERE user_id = $1 FOR UPDATE`, userID))
	if db.IsNoRows(err) {
		return a, apperr.ErrNotFound
	}
	return a, err
}

// addEvent 追加一条事件；source 非 0 时作为幂等键。
func addEvent(ctx context.Context, tx pgx.Tx, now time.Time, order string, bindingID int64, kind string, source, ledger, amount int64, detail string) error {
	var src, led *int64
	if source != 0 {
		src = &source
	}
	if ledger != 0 {
		led = &ledger
	}
	_, err := tx.Exec(ctx, `INSERT INTO referral_events (order_id, binding_id, kind, source_ledger_id, wallet_ledger_id, amount, detail, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, order, bindingID, kind, src, led, amount, detail, now)
	return err
}
