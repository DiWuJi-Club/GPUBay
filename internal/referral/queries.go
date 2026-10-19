package referral

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/paging"
	"spark-cloud/internal/platform/tz"
	"spark-cloud/internal/wallet"
)

// 控制台邀请页的只读查询（规格 §10.1）。被邀请人只以脱敏手机号出现（R11），不返回 user id、实名信息或风险标记。

type Stats struct {
	Invitees         int   `json:"invitees"`
	VerifiedInvitees int   `json:"verified_invitees"`
	Pending          int64 `json:"pending"`
	SettledNet       int64 `json:"settled_net"`
	MonthSettled     int64 `json:"month_settled"`
	Debt             int64 `json:"debt"`
}

// RebateTerms 是给用户看的规则摘要：只含规格列出的字段，不带费率版本的 id / 备注 / 创建人。
type RebateTerms struct {
	RateBP           int   `json:"rate_bp"`
	PeriodDays       int   `json:"period_days"`
	HoldDays         int   `json:"hold_days"`
	MaxPerInvitee    int64 `json:"max_per_invitee"`
	MaxPerInviter30d int64 `json:"max_per_inviter_30d"`
}

type MeView struct {
	Code        string      `json:"code"`
	Link        string      `json:"link"`
	Status      string      `json:"status"`
	KYCVerified bool        `json:"kyc_verified"`
	Rebate      RebateTerms `json:"rebate"`
	Stats       Stats       `json:"stats"`
}

// monthStart 本月 1 日 0 点（Asia/Shanghai）。
func (s *Service) monthStart() time.Time {
	l := s.d.Clock.Now().In(tz.CST)
	return time.Date(l.Year(), l.Month(), 1, 0, 0, 0, 0, tz.CST)
}

// MonthSettledFor 本月已发放到该用户钱包的返利（Σ referral_rebate 流水）。
func (s *Service) MonthSettledFor(ctx context.Context, userID int64) (int64, error) {
	var n int64
	err := s.d.Pool.QueryRow(ctx, `SELECT COALESCE(sum(amount), 0) FROM wallet_ledger WHERE user_id = $1 AND kind = $2 AND created_at >= $3`,
		userID, string(wallet.KindReferralRebate), s.monthStart()).Scan(&n)
	return n, err
}

func (s *Service) stats(ctx context.Context, userID int64) (Stats, error) {
	var st Stats
	if err := s.d.Pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE u.kyc_status = 'verified')
		FROM referral_bindings b JOIN users u ON u.id = b.invitee_id WHERE b.inviter_id = $1`, userID).Scan(&st.Invitees, &st.VerifiedInvitees); err != nil {
		return st, err
	}
	if err := s.d.Pool.QueryRow(ctx, `SELECT COALESCE(sum(reward) FILTER (WHERE status = 'pending'), 0), COALESCE(sum(settled_amount - clawed_amount), 0)
		FROM referral_rewards WHERE inviter_id = $1`, userID).Scan(&st.Pending, &st.SettledNet); err != nil {
		return st, err
	}
	if err := s.d.Pool.QueryRow(ctx, `SELECT COALESCE((SELECT debt FROM referral_accounts WHERE user_id = $1), 0)`, userID).Scan(&st.Debt); err != nil {
		return st, err
	}
	var err error
	st.MonthSettled, err = s.MonthSettledFor(ctx, userID)
	return st, err
}

// Me 控制台邀请页：懒生成码。
func (s *Service) Me(ctx context.Context, userID int64, publicBaseURL string) (MeView, error) {
	a, err := s.EnsureCode(ctx, userID)
	if err != nil {
		return MeView{}, err
	}
	u, err := s.d.Users.UserState(ctx, s.d.Pool, userID)
	if err != nil {
		return MeView{}, err
	}
	v, err := s.CurrentConfig(ctx)
	if err != nil {
		return MeView{}, err
	}
	st, err := s.stats(ctx, userID)
	if err != nil {
		return MeView{}, err
	}
	return MeView{
		Code: a.Code, Link: publicBaseURL + "/?ic=" + a.Code, Status: a.Status, KYCVerified: u.KYCStatus == "verified",
		Rebate: RebateTerms{RateBP: v.RateBP, PeriodDays: v.PeriodDays, HoldDays: v.HoldDays, MaxPerInvitee: v.MaxPerInvitee, MaxPerInviter30d: v.MaxPerInviter30d},
		Stats:  st,
	}, nil
}

type InviteeView struct {
	PhoneMasked string    `json:"phone_masked"`
	BoundAt     time.Time `json:"bound_at"`
	Status      string    `json:"status"`
	KYCVerified bool      `json:"kyc_verified"`
	Contributed int64     `json:"contributed"`
	RebateUntil time.Time `json:"rebate_until"`
}

// Invitees 邀请人的被邀请人列表（bound_at 倒序）；contributed = 该绑定未作废返利之和。
func (s *Service) Invitees(ctx context.Context, userID int64, p paging.Page) ([]InviteeView, int, error) {
	var total int
	if err := s.d.Pool.QueryRow(ctx, `SELECT count(*) FROM referral_bindings WHERE inviter_id = $1`, userID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.d.Pool.Query(ctx, `SELECT u.phone, b.bound_at, b.status, u.kyc_status = 'verified',
			COALESCE((SELECT sum(reward) FROM referral_rewards r WHERE r.binding_id = b.id AND r.status <> 'void'), 0), b.rebate_until
		FROM referral_bindings b JOIN users u ON u.id = b.invitee_id WHERE b.inviter_id = $1 ORDER BY b.bound_at DESC, b.id DESC LIMIT $2 OFFSET $3`,
		userID, p.Limit(), p.Offset())
	if err != nil {
		return nil, 0, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (InviteeView, error) {
		var v InviteeView
		var phone string
		err := r.Scan(&phone, &v.BoundAt, &v.Status, &v.KYCVerified, &v.Contributed, &v.RebateUntil)
		v.PhoneMasked = MaskPhone(phone)
		return v, err
	})
	return list, total, err
}

type RewardView struct {
	OrderID        string     `json:"order_id"`
	InviteeMasked  string     `json:"invitee_masked"`
	PaidAt         time.Time  `json:"paid_at"`
	OrderAmount    int64      `json:"order_amount"`
	RefundedAmount int64      `json:"refunded_amount"`
	Reward         int64      `json:"reward"`
	Status         string     `json:"status"`
	SettledAt      *time.Time `json:"settled_at"`
	ClawedAmount   int64      `json:"clawed_amount"`
}

// Rewards 邀请人的返利明细（paid_at 倒序）；status 为空表示全部。
func (s *Service) Rewards(ctx context.Context, userID int64, status string, p paging.Page) ([]RewardView, int, error) {
	if status != "" && status != StatusPending && status != StatusSettled && status != StatusVoid {
		return nil, 0, apperr.BadRequest("状态不正确")
	}
	var total int
	if err := s.d.Pool.QueryRow(ctx, `SELECT count(*) FROM referral_rewards WHERE inviter_id = $1 AND ($2 = '' OR status = $2)`, userID, status).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.d.Pool.Query(ctx, `SELECT r.order_id, u.phone, r.paid_at, r.order_amount, r.refunded_amount, r.reward, r.status, r.settled_at, r.clawed_amount
		FROM referral_rewards r JOIN users u ON u.id = r.invitee_id WHERE r.inviter_id = $1 AND ($2 = '' OR r.status = $2)
		ORDER BY r.paid_at DESC, r.order_id DESC LIMIT $3 OFFSET $4`, userID, status, p.Limit(), p.Offset())
	if err != nil {
		return nil, 0, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (RewardView, error) {
		var v RewardView
		var phone string
		err := r.Scan(&v.OrderID, &phone, &v.PaidAt, &v.OrderAmount, &v.RefundedAmount, &v.Reward, &v.Status, &v.SettledAt, &v.ClawedAmount)
		v.InviteeMasked = MaskPhone(phone)
		return v, err
	})
	return list, total, err
}
