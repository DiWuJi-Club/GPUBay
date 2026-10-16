package api

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"spark-cloud/internal/billing"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/paging"
	"spark-cloud/internal/plugins/pay"
)

const notifyPathPrefix = "/api/v1/pay/notify/"

type rechargeView struct {
	ID        string     `json:"id"`
	Channel   string     `json:"channel"`
	Amount    int64      `json:"amount"`
	Status    string     `json:"status"`
	QRCode    string     `json:"qr_code,omitempty"` // 仅待支付时返回
	ExpiresAt time.Time  `json:"expires_at"`
	PaidAt    *time.Time `json:"paid_at"`
	ClosedAt  *time.Time `json:"closed_at"`
	CreatedAt time.Time  `json:"created_at"`
}

func toRechargeView(r billing.Recharge) rechargeView {
	v := rechargeView{ID: r.ID, Channel: r.Channel, Amount: r.Amount, Status: r.Status, ExpiresAt: r.ExpiresAt,
		PaidAt: r.PaidAt, ClosedAt: r.ClosedAt, CreatedAt: r.CreatedAt}
	if r.Status == billing.StatusPending {
		v.QRCode = r.QRCode
	}
	return v
}

func (s *Server) handlePayChannels(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, list(s.Billing.Channels()))
}

func (s *Server) handleCreateRecharge(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Amount  int64  `json:"amount"`
		Channel string `json:"channel"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	rch, err := s.Billing.Create(r.Context(), currentUser(r).ID, req.Channel, req.Amount)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toRechargeView(rch))
}

func (s *Server) handleRecharge(w http.ResponseWriter, r *http.Request) {
	rch, err := s.Billing.Get(r.Context(), currentUser(r).ID, chi.URLParam(r, "id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toRechargeView(rch))
}

func (s *Server) handleRecharges(w http.ResponseWriter, r *http.Request) {
	p, err := paging.Parse(r.URL.Query())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	items, total, err := s.Billing.ListForUser(r.Context(), currentUser(r).ID, p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	views := make([]rechargeView, 0, len(items))
	for _, it := range items {
		views = append(views, toRechargeView(it))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": views, "total": total})
}

// handlePayNotify 接收支付渠道回调：无会话，由插件验签；响应格式由渠道决定。
func (s *Server) handlePayNotify(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	s.Billing.HandleNotify(r.Context(), chi.URLParam(r, "channel"), w, r)
}

// handleDevPay 仅开发环境：把自己的待支付充值单模拟为支付成功，走与回调相同的入账逻辑。
func (s *Server) handleDevPay(w http.ResponseWriter, r *http.Request) {
	uid := currentUser(r).ID
	rch, err := s.Billing.Get(r.Context(), uid, chi.URLParam(r, "id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if rch.Status != billing.StatusPending {
		s.writeError(w, r, apperr.ErrInvalidState)
		return
	}
	if err := s.Billing.Settle(r.Context(), rch.Channel, pay.Notification{
		OutTradeNo: rch.ID, ChannelTradeNo: "dev-" + rch.ID, Amount: rch.Amount, Paid: true,
	}); err != nil {
		s.writeError(w, r, err)
		return
	}
	if rch, err = s.Billing.Get(r.Context(), uid, rch.ID); err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toRechargeView(rch))
}
