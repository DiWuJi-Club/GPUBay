// Package billing 管理用户充值：扫码充值单、支付回调与主动查询入账、超时关单、后台手动入账。
// 加锁顺序：recharge → wallet。
package billing

import (
	"context"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/paging"
	"spark-cloud/internal/platform/randx"
	"spark-cloud/internal/plugins/pay"
	"spark-cloud/internal/wallet"
)

const (
	ChannelManual = "manual"

	StatusPending = "pending"
	StatusPaid    = "paid"
	StatusClosed  = "closed"

	MinAmount  = 100         // 1 元
	MaxAmount  = 100_000_000 // 100 万元
	MaxPending = 5           // 同一用户同时有效的待支付单上限

	QRTTL      = 15 * time.Minute // 二维码有效期
	QueryAfter = time.Minute      // 创建满 1 分钟后开始主动查询
	CloseAfter = 2 * time.Hour    // 创建满 2 小时仍未支付则关单

	sweepBatch = 200
)

var (
	ErrTooManyPending = apperr.New("too_many_pending", "待支付的充值单过多，请先完成支付或 15 分钟后再试", 409)
	ErrPayUnavailable = apperr.New("pay_unavailable", "支付渠道暂时不可用，请稍后再试或换一种支付方式", 502)
)

type Recharge struct {
	ID             string     `json:"id"`
	UserID         int64      `json:"user_id"`
	Channel        string     `json:"channel"`
	Amount         int64      `json:"amount"`
	Status         string     `json:"status"`
	QRCode         string     `json:"qr_code,omitempty"`
	ChannelTradeNo string     `json:"channel_trade_no,omitempty"`
	OperatorID     *int64     `json:"operator_id,omitempty"`
	Remark         string     `json:"remark,omitempty"`
	ExpiresAt      time.Time  `json:"expires_at"`
	PaidAt         *time.Time `json:"paid_at"`
	ClosedAt       *time.Time `json:"closed_at"`
	CreatedAt      time.Time  `json:"created_at"`
}

// UserChecker 校验用户可以充值（账号正常且已实名）。
type UserChecker interface {
	CanRent(ctx context.Context, userID int64) error
}

// AlertRaiser 写后台告警（由 fleet 模块实现）。
type AlertRaiser interface {
	RaiseAlertOnce(ctx context.Context, q db.Querier, a fleet.Alert) error
}

type Deps struct {
	Pool          *pgxpool.Pool
	Clock         clock.Clock
	Log           *slog.Logger
	Wallet        *wallet.Service
	Users         UserChecker
	Alerts        AlertRaiser
	Providers     map[string]pay.Provider // 已启用的扫码渠道：wechat / alipay
	NotifyBaseURL string                  // 回调地址前缀，回调地址为 <base>/api/v1/pay/notify/<channel>
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

const rechargeCols = `id, user_id, channel, amount, status, qr_code, channel_trade_no, operator_id, remark,
	expires_at, paid_at, closed_at, created_at`

func scanRecharge(row pgx.Row) (Recharge, error) {
	var r Recharge
	err := row.Scan(&r.ID, &r.UserID, &r.Channel, &r.Amount, &r.Status, &r.QRCode, &r.ChannelTradeNo, &r.OperatorID, &r.Remark,
		&r.ExpiresAt, &r.PaidAt, &r.ClosedAt, &r.CreatedAt)
	if db.IsNoRows(err) {
		return Recharge{}, apperr.ErrNotFound
	}
	return r, err
}

func collectRecharges(rows pgx.Rows, err error) ([]Recharge, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Recharge, error) { return scanRecharge(r) })
}

// Channels 返回已启用的扫码渠道（按名称排序）。
func (s *Service) Channels() []string {
	out := make([]string, 0, len(s.d.Providers))
	for name := range s.d.Providers {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func channelName(channel string) string {
	switch channel {
	case pay.ChannelWechat:
		return "微信"
	case pay.ChannelAlipay:
		return "支付宝"
	}
	return "手动"
}

// Create 创建扫码充值单：先落库（便于对账），再到渠道下单拿二维码。
func (s *Service) Create(ctx context.Context, userID int64, channel string, amount int64) (Recharge, error) {
	p, ok := s.d.Providers[channel]
	if !ok {
		return Recharge{}, apperr.BadRequest("不支持的支付方式")
	}
	if amount < MinAmount || amount > MaxAmount {
		return Recharge{}, apperr.BadRequest("充值金额需在 1 元到 100 万元之间")
	}
	if err := s.d.Users.CanRent(ctx, userID); err != nil {
		return Recharge{}, err
	}
	now := s.d.Clock.Now()
	var r Recharge
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		// 串行化同一用户的创建请求，保证待支付单上限不被并发绕过
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "recharge:"+strconv.FormatInt(userID, 10)); err != nil {
			return err
		}
		var pending int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM recharges WHERE user_id = $1 AND status = 'pending' AND expires_at > $2`,
			userID, now).Scan(&pending); err != nil {
			return err
		}
		if pending >= MaxPending {
			return ErrTooManyPending
		}
		var err error
		r, err = scanRecharge(tx.QueryRow(ctx, `INSERT INTO recharges (id, user_id, channel, amount, status, expires_at, created_at, updated_at)
			VALUES ($1, $2, $3, $4, 'pending', $5, $6, $6) RETURNING `+rechargeCols,
			randx.ID("rch-", 12), userID, channel, amount, now.Add(QRTTL), now))
		return err
	})
	if err != nil {
		return Recharge{}, err
	}
	qr, err := p.Create(ctx, pay.Recharge{ID: r.ID, Amount: amount, Subject: "Spark 算力余额充值", ExpiresAt: r.ExpiresAt,
		NotifyURL: s.d.NotifyBaseURL + "/api/v1/pay/notify/" + channel})
	if err != nil {
		s.d.Log.Error("pay create failed", "recharge", r.ID, "channel", channel, "err", err)
		closedAt := s.d.Clock.Now()
		if _, cerr := s.d.Pool.Exec(context.WithoutCancel(ctx), `UPDATE recharges SET status = 'closed', closed_at = $2,
				remark = '渠道下单失败', updated_at = $2 WHERE id = $1 AND status = 'pending'`, r.ID, closedAt); cerr != nil {
			s.d.Log.Error("close failed recharge", "recharge", r.ID, "err", cerr)
		}
		return Recharge{}, ErrPayUnavailable
	}
	if _, err := s.d.Pool.Exec(ctx, `UPDATE recharges SET qr_code = $2, updated_at = $3 WHERE id = $1`, r.ID, qr, now); err != nil {
		return Recharge{}, err
	}
	r.QRCode = qr
	return r, nil
}

// Get 返回用户自己的充值单。
func (s *Service) Get(ctx context.Context, userID int64, id string) (Recharge, error) {
	return scanRecharge(s.d.Pool.QueryRow(ctx, `SELECT `+rechargeCols+` FROM recharges WHERE id = $1 AND user_id = $2`, id, userID))
}

// ListForUser 按创建时间倒序分页返回用户的充值单。
func (s *Service) ListForUser(ctx context.Context, userID int64, p paging.Page) ([]Recharge, int, error) {
	var total int
	if err := s.d.Pool.QueryRow(ctx, `SELECT count(*) FROM recharges WHERE user_id = $1`, userID).Scan(&total); err != nil {
		return nil, 0, err
	}
	items, err := collectRecharges(s.d.Pool.Query(ctx, `SELECT `+rechargeCols+` FROM recharges WHERE user_id = $1
		ORDER BY created_at DESC, id LIMIT $2 OFFSET $3`, userID, p.Limit(), p.Offset()))
	return items, total, err
}

// ManualRecharge 后台手动入账（线下转账等），记为充值流水，渠道为 manual。
func (s *Service) ManualRecharge(ctx context.Context, actor audit.Actor, userID, amount int64, remark string) (Recharge, error) {
	remark = strings.TrimSpace(remark)
	if remark == "" {
		return Recharge{}, apperr.BadRequest("请填写入账说明（例如转账流水号）")
	}
	if amount < MinAmount || amount > MaxAmount {
		return Recharge{}, apperr.BadRequest("入账金额需在 1 元到 100 万元之间")
	}
	now := s.d.Clock.Now()
	var r Recharge
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		var err error
		r, err = scanRecharge(tx.QueryRow(ctx, `INSERT INTO recharges (id, user_id, channel, amount, status, operator_id, remark,
				expires_at, paid_at, created_at, updated_at)
			VALUES ($1, $2, 'manual', $3, 'paid', $4, $5, $6, $6, $6, $6) RETURNING `+rechargeCols,
			randx.ID("rch-", 12), userID, amount, actor.AdminID, remark, now))
		if db.IsForeignKeyViolation(err) {
			return apperr.ErrNotFound
		}
		if err != nil {
			return err
		}
		if _, err := s.d.Wallet.Apply(ctx, tx, wallet.Entry{UserID: userID, Amount: amount, Kind: wallet.KindRecharge,
			RefType: "recharge", RefID: r.ID, OperatorID: actor.AdminID, Remark: "手动入账：" + remark}); err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("wallet.manual_recharge", "user", strconv.FormatInt(userID, 10),
			map[string]any{"recharge_id": r.ID, "amount": amount, "remark": remark}))
	})
	return r, err
}
