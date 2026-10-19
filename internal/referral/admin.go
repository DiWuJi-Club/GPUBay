package referral

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/csvx"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/paging"
)

// 后台裁决。加锁顺序与发放一致（见包注释）：users FOR KEY SHARE → referral_accounts(邀请人) FOR UPDATE →
// referral_bindings → referral_rewards。冻结与同一轮发放的互斥由账户锁保证：releaseOne 在账户锁内复核账户状态。

// adminRef 把系统身份（AdminID 为 0）记为 NULL。
func adminRef(actor audit.Actor) *int64 {
	if actor.AdminID == 0 {
		return nil
	}
	return &actor.AdminID
}

// Freeze 冻结邀请人：码不再可绑定、返利停止发放（继续计提）。
func (s *Service) Freeze(ctx context.Context, actor audit.Actor, userID int64, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return apperr.BadRequest("请填写冻结原因")
	}
	now := s.d.Clock.Now()
	return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		if err := lockUsersKeyShare(ctx, tx, userID); err != nil {
			return err
		}
		a, err := lockAccount(ctx, tx, userID)
		if err != nil {
			return err
		}
		if a.Status == AccountFrozen {
			return apperr.ErrReferralBindingState
		}
		if _, err := tx.Exec(ctx, `UPDATE referral_accounts SET status = 'frozen', frozen_reason = $2, frozen_by = $3, frozen_at = $4, updated_at = $4 WHERE user_id = $1`,
			userID, reason, adminRef(actor), now); err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("referral.freeze", "referral_account", strconv.FormatInt(userID, 10), map[string]any{"reason": reason}))
	})
}

// Unfreeze 解冻：冻结期间累计的到期返利在下一轮发放。
func (s *Service) Unfreeze(ctx context.Context, actor audit.Actor, userID int64) error {
	now := s.d.Clock.Now()
	return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		if err := lockUsersKeyShare(ctx, tx, userID); err != nil {
			return err
		}
		a, err := lockAccount(ctx, tx, userID)
		if err != nil {
			return err
		}
		if a.Status != AccountFrozen {
			return apperr.ErrReferralBindingState
		}
		if _, err := tx.Exec(ctx, `UPDATE referral_accounts SET status = 'active', frozen_reason = '', frozen_by = NULL, frozen_at = NULL, updated_at = $2 WHERE user_id = $1`, userID, now); err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("referral.unfreeze", "referral_account", strconv.FormatInt(userID, 10), map[string]any{"previous_reason": a.FrozenReason}))
	})
}

// lockBindingOwner 读绑定的邀请人与被邀请人（二者不变，加锁前读安全；状态在锁后由各操作的条件更新判定），
// 再按加锁顺序锁 users(邀请人, 被邀请人) → referral_accounts(邀请人)。绑定不存在 → apperr.ErrNotFound。
func lockBindingOwner(ctx context.Context, tx pgx.Tx, bindingID int64) error {
	var inviterID, inviteeID int64
	err := tx.QueryRow(ctx, `SELECT inviter_id, invitee_id FROM referral_bindings WHERE id = $1`, bindingID).Scan(&inviterID, &inviteeID)
	if db.IsNoRows(err) {
		return apperr.ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := lockUsersKeyShare(ctx, tx, inviterID, inviteeID); err != nil {
		return err
	}
	_, err = lockAccount(ctx, tx, inviterID)
	return err
}

// Approve 把搁置的绑定（same_device / kyc_check_failed）转为有效；待发放返利在下一轮正常发放。
// 写 reviewed_at：发放据此把此前的 referral.kyc_check_failed 审计视为已解决（kycCheckUnresolved）。
func (s *Service) Approve(ctx context.Context, actor audit.Actor, bindingID int64, note string) error {
	now := s.d.Clock.Now()
	note = strings.TrimSpace(note)
	return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		if err := lockBindingOwner(ctx, tx, bindingID); err != nil {
			return err
		}
		var status, prevHold string
		var inviterID, inviteeID int64
		var boundAt time.Time
		var reviewedAt *time.Time
		if err := tx.QueryRow(ctx, `SELECT status, hold_reason, inviter_id, invitee_id, bound_at, reviewed_at FROM referral_bindings WHERE id = $1 FOR UPDATE`, bindingID).
			Scan(&status, &prevHold, &inviterID, &inviteeID, &boundAt, &reviewedAt); err != nil {
			return err
		}
		if status != BindingHeld {
			return apperr.ErrReferralBindingState
		}
		// 审计留下通过时是否仍有未解决的实名同身份判定失败（后台队列同样显示 kyc_check_unresolved）
		since := boundAt
		if reviewedAt != nil {
			since = *reviewedAt
		}
		unresolved, err := kycCheckUnresolved(ctx, tx, inviterID, inviteeID, since)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE referral_bindings SET status = 'active', hold_reason = '', reviewed_by = $2, reviewed_at = $3, review_note = $4, updated_at = $3 WHERE id = $1`,
			bindingID, adminRef(actor), now, note); err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("referral.approve", "referral_binding", strconv.FormatInt(bindingID, 10),
			map[string]any{"note": note, "hold_reason": prevHold, "kyc_check_unresolved": unresolved}))
	})
}

// Revoke 撤销绑定（任意非 revoked 状态）：待发放全部作废，已发放不自动扣回（财务用调账）；返回作废金额。
// 与实名同身份自动撤销共用 revokeBinding（写 reviewed_by / reviewed_at、void 事件与审计）。
func (s *Service) Revoke(ctx context.Context, actor audit.Actor, bindingID int64, reason string) (int64, error) {
	if err := CheckRevokeReason(reason); err != nil {
		return 0, err
	}
	reason = strings.TrimSpace(reason)
	now := s.d.Clock.Now()
	var voided int64
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		if err := lockBindingOwner(ctx, tx, bindingID); err != nil {
			return err
		}
		var err error
		voided, err = s.revokeBinding(ctx, tx, now, bindingID, reason, actor, "", "referral.revoke")
		return err
	})
	return voided, err
}

// ---- 后台查询：概览 / 邀请人 / 绑定 / 返利明细 / 导出 / 用户详情区块 ----
// 只输出手机号、实名状态与绑定时的环境信息（IP / 设备 / UA），从不输出实名指纹或证件号。

type Summary struct {
	Pending      int64 `json:"pending"`
	MonthSettled int64 `json:"month_settled"`
	SettledNet   int64 `json:"settled_net"`
	Debt         int64 `json:"debt"`
	HeldCount    int   `json:"held_count"`
}

func (s *Service) Summary(ctx context.Context) (Summary, error) {
	var sum Summary
	err := s.d.Pool.QueryRow(ctx, `SELECT
		COALESCE((SELECT sum(reward) FROM referral_rewards WHERE status = 'pending'), 0),
		COALESCE((SELECT sum(amount) FROM wallet_ledger WHERE kind = 'referral_rebate' AND created_at >= $1), 0),
		COALESCE((SELECT sum(settled_amount - clawed_amount) FROM referral_rewards), 0),
		COALESCE((SELECT sum(debt) FROM referral_accounts), 0),
		(SELECT count(*) FROM referral_bindings WHERE status = 'held')`, s.monthStart()).
		Scan(&sum.Pending, &sum.MonthSettled, &sum.SettledNet, &sum.Debt, &sum.HeldCount)
	return sum, err
}

// MonthSettledNet 仪表盘卡片：本月发放净额（返利 − 扣回；扣回流水为负）。
func (s *Service) MonthSettledNet(ctx context.Context) (int64, error) {
	var n int64
	err := s.d.Pool.QueryRow(ctx, `SELECT COALESCE(sum(amount), 0) FROM wallet_ledger WHERE kind IN ('referral_rebate', 'referral_clawback') AND created_at >= $1`, s.monthStart()).Scan(&n)
	return n, err
}

// InviterFilter：Q 为手机号（尾号）或用户 ID；Status ∈ "" active frozen；Flag 为任一绑定含该风险标记；Sort ∈ "" settled invitees bound_at。
type InviterFilter struct{ Q, Status, Flag, Sort string }

type InviterRow struct {
	Account
	Phone       string     `json:"phone"`
	KYCStatus   string     `json:"kyc_status"`
	UserStatus  string     `json:"user_status"`
	Invitees    int        `json:"invitees"`
	HeldCount   int        `json:"held_count"`
	Pending     int64      `json:"pending"`
	SettledNet  int64      `json:"settled_net"`
	LastBoundAt *time.Time `json:"last_bound_at"`
}

var (
	// likeEscaper 转义 LIKE 的通配符（PostgreSQL 默认转义符为反斜杠）。
	likeEscaper  = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	flagValues   = []string{FlagSameIP, FlagSameDevice, FlagBurst, FlagSameIdentity}
	inviterSorts = map[string]string{
		"":         "x.settled_net DESC, x.user_id",
		"settled":  "x.settled_net DESC, x.user_id",
		"invitees": "x.invitees DESC, x.user_id",
		"bound_at": "x.last_bound_at DESC NULLS LAST, x.user_id",
	}
)

// inviterRowsSQL 每个邀请人一行（作为子查询 x），筛选与排序都作用在它的列上。
const inviterRowsSQL = `
	SELECT a.user_id, a.code, a.status, a.frozen_reason, a.debt, a.created_at, u.phone, u.kyc_status, u.status AS user_status,
		COALESCE(b.n, 0) AS invitees, COALESCE(b.held, 0) AS held_count, COALESCE(r.pending, 0) AS pending, COALESCE(r.net, 0) AS settled_net, b.last_bound_at
	FROM referral_accounts a JOIN users u ON u.id = a.user_id
	LEFT JOIN (SELECT inviter_id, count(*) AS n, count(*) FILTER (WHERE status = 'held') AS held, max(bound_at) AS last_bound_at
		FROM referral_bindings GROUP BY inviter_id) b ON b.inviter_id = a.user_id
	LEFT JOIN (SELECT inviter_id, sum(reward) FILTER (WHERE status = 'pending') AS pending, sum(settled_amount - clawed_amount) AS net
		FROM referral_rewards GROUP BY inviter_id) r ON r.inviter_id = a.user_id`

func (s *Service) AdminListInviters(ctx context.Context, f InviterFilter, p paging.Page) ([]InviterRow, int, error) {
	order, ok := inviterSorts[f.Sort]
	if !ok {
		return nil, 0, apperr.BadRequest("排序方式不正确")
	}
	if f.Status != "" && f.Status != AccountActive && f.Status != AccountFrozen {
		return nil, 0, apperr.BadRequest("状态不正确")
	}
	if f.Flag != "" && !slices.Contains(flagValues, f.Flag) {
		return nil, 0, apperr.BadRequest("标记不正确")
	}
	var w db.Where
	if q := strings.TrimSpace(f.Q); q != "" {
		tail := "%" + likeEscaper.Replace(q) // 尾号匹配；q 里的 % _ \ 按字面匹配
		if id, err := strconv.ParseInt(q, 10, 64); err == nil && len(q) < 11 {
			w.Add("(x.user_id = ? OR x.phone LIKE ?)", id, tail)
		} else {
			w.Add("x.phone LIKE ?", tail)
		}
	}
	if f.Status != "" {
		w.Add("x.status = ?", f.Status)
	}
	if f.Flag != "" {
		w.Add("EXISTS (SELECT 1 FROM referral_bindings fb WHERE fb.inviter_id = x.user_id AND ? = ANY(fb.risk_flags))", f.Flag)
	}
	from := ` FROM (` + inviterRowsSQL + `) x WHERE ` + w.SQL()
	var total int
	if err := s.d.Pool.QueryRow(ctx, `SELECT count(*)`+from, w.Args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	n := len(w.Args)
	rows, err := s.d.Pool.Query(ctx, `SELECT x.*`+from+` ORDER BY `+order+fmt.Sprintf(` LIMIT $%d OFFSET $%d`, n+1, n+2), append(w.Args, p.Limit(), p.Offset())...)
	if err != nil {
		return nil, 0, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (InviterRow, error) {
		var v InviterRow
		err := r.Scan(&v.UserID, &v.Code, &v.Status, &v.FrozenReason, &v.Debt, &v.CreatedAt, &v.Phone, &v.KYCStatus, &v.UserStatus,
			&v.Invitees, &v.HeldCount, &v.Pending, &v.SettledNet, &v.LastBoundAt)
		return v, err
	})
	return list, total, err
}

// Binding 是后台视角的绑定（JSON 标签与列同名）。KYCCheckUnresolved：邀请人或被邀请人有晚于 COALESCE(reviewed_at, bound_at)
// 的 referral.kyc_check_failed 审计（实名同身份判定失败、尚待人工核查），审核前须先核查。
// 判定与发放的 kycCheckUnresolved（release.go）必须一致：bindingSelect 里是它的 SQL 版本，
// 改任何一边都要同步另一边；TestKYCCheckUnresolvedPredicatesAgree 用同一组用例钉住两者。
type Binding struct {
	ID                 int64      `json:"id"`
	InviterID          int64      `json:"inviter_id"`
	InviteeID          int64      `json:"invitee_id"`
	InviterPhone       string     `json:"inviter_phone"`
	InviteePhone       string     `json:"invitee_phone"`
	Code               string     `json:"code"`
	Status             string     `json:"status"`
	RiskFlags          []string   `json:"risk_flags"`
	HoldReason         string     `json:"hold_reason"`
	RateVersionID      int64      `json:"rate_version_id"`
	BoundAt            time.Time  `json:"bound_at"`
	RebateUntil        time.Time  `json:"rebate_until"`
	IP                 string     `json:"ip"`
	DeviceID           string     `json:"device_id"`
	UserAgent          string     `json:"user_agent"`
	ReviewedBy         *int64     `json:"reviewed_by"`
	ReviewedAt         *time.Time `json:"reviewed_at"`
	ReviewNote         string     `json:"review_note"`
	Pending            int64      `json:"pending"`
	Contributed        int64      `json:"contributed"`
	KYCCheckUnresolved bool       `json:"kyc_check_unresolved"`
}

const bindingSelect = `SELECT b.id, b.inviter_id, b.invitee_id, iu.phone, eu.phone, b.code, b.status, b.risk_flags, b.hold_reason, b.rate_version_id, b.bound_at, b.rebate_until,
		b.ip, b.device_id, b.user_agent, b.reviewed_by, b.reviewed_at, b.review_note,
		COALESCE((SELECT sum(reward) FROM referral_rewards r WHERE r.binding_id = b.id AND r.status = 'pending'), 0),
		COALESCE((SELECT sum(reward) FROM referral_rewards r WHERE r.binding_id = b.id AND r.status <> 'void'), 0),
		-- 与 release.go 的 kycCheckUnresolved 同一判定（见 Binding 注释）
		EXISTS (SELECT 1 FROM audit_logs al WHERE al.target_type = 'user' AND al.target_id IN (b.inviter_id::text, b.invitee_id::text)
			AND al.action = 'referral.kyc_check_failed' AND al.created_at > COALESCE(b.reviewed_at, b.bound_at))
	FROM referral_bindings b JOIN users iu ON iu.id = b.inviter_id JOIN users eu ON eu.id = b.invitee_id`

func scanBinding(r pgx.Row) (Binding, error) {
	var b Binding
	err := r.Scan(&b.ID, &b.InviterID, &b.InviteeID, &b.InviterPhone, &b.InviteePhone, &b.Code, &b.Status, &b.RiskFlags, &b.HoldReason, &b.RateVersionID, &b.BoundAt, &b.RebateUntil,
		&b.IP, &b.DeviceID, &b.UserAgent, &b.ReviewedBy, &b.ReviewedAt, &b.ReviewNote, &b.Pending, &b.Contributed, &b.KYCCheckUnresolved)
	if b.RiskFlags == nil {
		b.RiskFlags = []string{}
	}
	return b, err
}

type BindingFilter struct {
	InviterID, InviteeID int64
	Status, Flag         string
	From, To             *time.Time // bound_at
}

func (f BindingFilter) where() (db.Where, error) {
	var w db.Where
	if f.Status != "" && f.Status != BindingActive && f.Status != BindingHeld && f.Status != BindingRevoked {
		return w, apperr.BadRequest("状态不正确")
	}
	if f.Flag != "" && !slices.Contains(flagValues, f.Flag) {
		return w, apperr.BadRequest("标记不正确")
	}
	if f.InviterID > 0 {
		w.Add("b.inviter_id = ?", f.InviterID)
	}
	if f.InviteeID > 0 {
		w.Add("b.invitee_id = ?", f.InviteeID)
	}
	if f.Status != "" {
		w.Add("b.status = ?", f.Status)
	}
	if f.Flag != "" {
		w.Add("? = ANY(b.risk_flags)", f.Flag)
	}
	if f.From != nil {
		w.Add("b.bound_at >= ?", *f.From)
	}
	if f.To != nil {
		w.Add("b.bound_at < ?", *f.To)
	}
	return w, nil
}

// AdminListBindings 按 bound_at 倒序；只看 held（待审核队列）时标记多的在前、同数量先到先审。
func (s *Service) AdminListBindings(ctx context.Context, f BindingFilter, p paging.Page) ([]Binding, int, error) {
	w, err := f.where()
	if err != nil {
		return nil, 0, err
	}
	var total int
	if err := s.d.Pool.QueryRow(ctx, `SELECT count(*) FROM referral_bindings b WHERE `+w.SQL(), w.Args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	n := len(w.Args)
	order := `b.bound_at DESC, b.id DESC`
	if f.Status == BindingHeld {
		order = `cardinality(b.risk_flags) DESC, b.bound_at, b.id`
	}
	rows, err := s.d.Pool.Query(ctx, bindingSelect+` WHERE `+w.SQL()+` ORDER BY `+order+fmt.Sprintf(` LIMIT $%d OFFSET $%d`, n+1, n+2), append(w.Args, p.Limit(), p.Offset())...)
	if err != nil {
		return nil, 0, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Binding, error) { return scanBinding(r) })
	return list, total, err
}

// exportRows 先数行（超过 csvx.MaxRows 返回 csvx.ErrTooLarge，此时还没写出任何行），再按 order 逐行回调。
func exportRows[T any](ctx context.Context, s *Service, countSQL, selectSQL string, args []any, scan func(pgx.Row) (T, error), fn func(T) error) error {
	var total int
	if err := s.d.Pool.QueryRow(ctx, countSQL, args...).Scan(&total); err != nil {
		return err
	}
	if total > csvx.MaxRows {
		return csvx.ErrTooLarge
	}
	rows, err := s.d.Pool.Query(ctx, selectSQL, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return err
		}
		if err := fn(v); err != nil {
			return err
		}
	}
	return rows.Err()
}

// ExportBindings 按 bound_at 正序导出。
func (s *Service) ExportBindings(ctx context.Context, f BindingFilter, fn func(Binding) error) error {
	w, err := f.where()
	if err != nil {
		return err
	}
	return exportRows(ctx, s, `SELECT count(*) FROM referral_bindings b WHERE `+w.SQL(), bindingSelect+` WHERE `+w.SQL()+` ORDER BY b.bound_at, b.id`, w.Args, scanBinding, fn)
}

type Event struct {
	ID             int64     `json:"id"`
	OrderID        string    `json:"order_id"`
	BindingID      int64     `json:"binding_id"`
	Kind           string    `json:"kind"`
	SourceLedgerID *int64    `json:"source_ledger_id"`
	WalletLedgerID *int64    `json:"wallet_ledger_id"`
	Amount         int64     `json:"amount"`
	Detail         string    `json:"detail"`
	CreatedAt      time.Time `json:"created_at"`
}

// InviterDetail 只带最近 InviterDetailRows 条绑定 / 返利；BindingsTotal / RewardsTotal 是全量条数，
// 超过时后台跳转分页列表（/referral/bindings?inviter_id=、/referral/rewards?inviter_id=）。
type InviterDetail struct {
	Account       Account       `json:"account"`
	Phone         string        `json:"phone"`
	Bindings      []Binding     `json:"bindings"`
	BindingsTotal int           `json:"bindings_total"`
	Rewards       []AdminReward `json:"rewards"`
	RewardsTotal  int           `json:"rewards_total"`
	Events        []Event       `json:"events"` // 最近 50 条
}

const InviterDetailRows = 100

// AdminInviter 邀请人详情；没有账户行（从未生成过邀请码）→ apperr.ErrNotFound。
func (s *Service) AdminInviter(ctx context.Context, userID int64) (InviterDetail, error) {
	var d InviterDetail
	a, err := scanAccount(s.d.Pool.QueryRow(ctx, `SELECT `+accountCols+` FROM referral_accounts WHERE user_id = $1`, userID))
	if db.IsNoRows(err) {
		return d, apperr.ErrNotFound
	}
	if err != nil {
		return d, err
	}
	d.Account = a
	u, err := s.d.Users.UserState(ctx, s.d.Pool, userID)
	if err != nil {
		return d, err
	}
	d.Phone = u.Phone
	if d.Bindings, d.BindingsTotal, err = s.AdminListBindings(ctx, BindingFilter{InviterID: userID}, paging.Page{Number: 1, Size: InviterDetailRows}); err != nil {
		return d, err
	}
	if d.Rewards, d.RewardsTotal, err = s.AdminListRewards(ctx, RewardFilter{InviterID: userID}, paging.Page{Number: 1, Size: InviterDetailRows}); err != nil {
		return d, err
	}
	rows, err := s.d.Pool.Query(ctx, `SELECT e.id, e.order_id, e.binding_id, e.kind, e.source_ledger_id, e.wallet_ledger_id, e.amount, e.detail, e.created_at
		FROM referral_events e JOIN referral_bindings b ON b.id = e.binding_id WHERE b.inviter_id = $1 ORDER BY e.id DESC LIMIT 50`, userID)
	if err != nil {
		return d, err
	}
	d.Events, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Event, error) {
		var e Event
		err := r.Scan(&e.ID, &e.OrderID, &e.BindingID, &e.Kind, &e.SourceLedgerID, &e.WalletLedgerID, &e.Amount, &e.Detail, &e.CreatedAt)
		return e, err
	})
	return d, err
}

type RewardFilter struct {
	InviterID, InviteeID int64
	Status               string
	From, To             *time.Time // paid_at
}

type AdminReward struct {
	Reward
	InviterPhone string `json:"inviter_phone"`
	InviteePhone string `json:"invitee_phone"`
}

func (f RewardFilter) where() (db.Where, error) {
	var w db.Where
	if f.Status != "" && f.Status != StatusPending && f.Status != StatusSettled && f.Status != StatusVoid {
		return w, apperr.BadRequest("状态不正确")
	}
	if f.InviterID > 0 {
		w.Add("r.inviter_id = ?", f.InviterID)
	}
	if f.InviteeID > 0 {
		w.Add("r.invitee_id = ?", f.InviteeID)
	}
	if f.Status != "" {
		w.Add("r.status = ?", f.Status)
	}
	if f.From != nil {
		w.Add("r.paid_at >= ?", *f.From)
	}
	if f.To != nil {
		w.Add("r.paid_at < ?", *f.To)
	}
	return w, nil
}

const adminRewardSelect = `SELECT r.order_id, r.binding_id, r.inviter_id, r.invitee_id, r.rate_version_id, r.rate_bp, r.order_amount, r.refunded_amount, r.reward_cap, r.reward, r.status, r.settle_after,
	r.settled_amount, r.clawed_amount, r.settled_at, r.paid_at, r.created_at, r.updated_at, iu.phone, eu.phone
	FROM referral_rewards r JOIN users iu ON iu.id = r.inviter_id JOIN users eu ON eu.id = r.invitee_id`

func scanAdminReward(row pgx.Row) (AdminReward, error) {
	var v AdminReward
	r := &v.Reward
	err := row.Scan(&r.OrderID, &r.BindingID, &r.InviterID, &r.InviteeID, &r.RateVersionID, &r.RateBP, &r.OrderAmount, &r.RefundedAmount, &r.RewardCap, &r.RewardAmt, &r.Status, &r.SettleAfter,
		&r.SettledAmount, &r.ClawedAmount, &r.SettledAt, &r.PaidAt, &r.CreatedAt, &r.UpdatedAt, &v.InviterPhone, &v.InviteePhone)
	return v, err
}

// AdminListRewards 按 paid_at 倒序。
func (s *Service) AdminListRewards(ctx context.Context, f RewardFilter, p paging.Page) ([]AdminReward, int, error) {
	w, err := f.where()
	if err != nil {
		return nil, 0, err
	}
	var total int
	if err := s.d.Pool.QueryRow(ctx, `SELECT count(*) FROM referral_rewards r WHERE `+w.SQL(), w.Args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	n := len(w.Args)
	rows, err := s.d.Pool.Query(ctx, adminRewardSelect+` WHERE `+w.SQL()+fmt.Sprintf(` ORDER BY r.paid_at DESC, r.order_id DESC LIMIT $%d OFFSET $%d`, n+1, n+2), append(w.Args, p.Limit(), p.Offset())...)
	if err != nil {
		return nil, 0, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (AdminReward, error) { return scanAdminReward(r) })
	return list, total, err
}

// ExportRewards 按 paid_at 正序导出。
func (s *Service) ExportRewards(ctx context.Context, f RewardFilter, fn func(AdminReward) error) error {
	w, err := f.where()
	if err != nil {
		return err
	}
	return exportRows(ctx, s, `SELECT count(*) FROM referral_rewards r WHERE `+w.SQL(), adminRewardSelect+` WHERE `+w.SQL()+` ORDER BY r.paid_at, r.order_id`, w.Args, scanAdminReward, fn)
}

type UserSummaryInviter struct {
	UserID        int64  `json:"user_id"`
	Phone         string `json:"phone"`
	BindingStatus string `json:"binding_status"`
}

type UserSummaryAccount struct {
	Code       string `json:"code"`
	Status     string `json:"status"`
	Invitees   int    `json:"invitees"`
	SettledNet int64  `json:"settled_net"`
	Pending    int64  `json:"pending"`
	Debt       int64  `json:"debt"`
}

type UserSummary struct {
	Inviter *UserSummaryInviter `json:"inviter,omitempty"`
	Account *UserSummaryAccount `json:"account,omitempty"`
}

// UserSummary 后台用户详情的「邀请」区块：作为被邀请人时的邀请人，作为邀请人时的统计；两者都没有时为空对象。
func (s *Service) UserSummary(ctx context.Context, userID int64) (UserSummary, error) {
	var us UserSummary
	var inv UserSummaryInviter
	err := s.d.Pool.QueryRow(ctx, `SELECT b.inviter_id, u.phone, b.status FROM referral_bindings b JOIN users u ON u.id = b.inviter_id WHERE b.invitee_id = $1`, userID).
		Scan(&inv.UserID, &inv.Phone, &inv.BindingStatus)
	switch {
	case err == nil:
		us.Inviter = &inv
	case !db.IsNoRows(err):
		return us, err
	}
	var acc UserSummaryAccount
	err = s.d.Pool.QueryRow(ctx, `SELECT a.code, a.status, a.debt,
			(SELECT count(*) FROM referral_bindings WHERE inviter_id = a.user_id),
			COALESCE((SELECT sum(settled_amount - clawed_amount) FROM referral_rewards WHERE inviter_id = a.user_id), 0),
			COALESCE((SELECT sum(reward) FROM referral_rewards WHERE inviter_id = a.user_id AND status = 'pending'), 0)
		FROM referral_accounts a WHERE a.user_id = $1`, userID).Scan(&acc.Code, &acc.Status, &acc.Debt, &acc.Invitees, &acc.SettledNet, &acc.Pending)
	switch {
	case err == nil:
		us.Account = &acc
	case !db.IsNoRows(err):
		return us, err
	}
	return us, nil
}

// 重扫天数范围（Rescan 与后台接口共用 CheckRescanDays）。
const (
	RescanMinDays = 1
	RescanMaxDays = 30
)

// CheckRescanDays 校验重扫天数；后台接口在二次验证之前调用，参数错误不消耗动态码。
func CheckRescanDays(days int) error {
	if days < RescanMinDays || days > RescanMaxDays {
		return apperr.BadRequest(fmt.Sprintf("重扫天数需在 %d～%d 之间", RescanMinDays, RescanMaxDays))
	}
	return nil
}

// CheckRevokeReason 校验撤销原因非空（Revoke 内同样校验）；后台接口在二次验证之前调用。
func CheckRevokeReason(reason string) error {
	if strings.TrimSpace(reason) == "" {
		return apperr.BadRequest("请填写撤销原因")
	}
	return nil
}
