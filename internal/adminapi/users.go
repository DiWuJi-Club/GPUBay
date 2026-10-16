package adminapi

import (
	"io"
	"net/http"
	"path"
	"strconv"

	"github.com/go-chi/chi/v5"

	"spark-cloud/internal/identity"
	"spark-cloud/internal/platform/httpx"
	"spark-cloud/internal/platform/paging"
	"spark-cloud/internal/rental"
)

func (s *Server) userRoutes(g chi.Router) {
	s.handle(g, http.MethodGet, "/users", allRoles, s.handleListUsers)
	s.handle(g, http.MethodGet, "/users/{id}", allRoles, s.handleUser)
	s.handle(g, http.MethodPost, "/users/{id}/ban", opsRoles, s.handleBan)
	s.handle(g, http.MethodPost, "/users/{id}/unban", opsRoles, s.handleUnban)
	s.handle(g, http.MethodGet, "/kyc/pending", opsRoles, s.handlePendingKYC)
	s.handle(g, http.MethodGet, "/kyc/{id}", opsRoles, s.handleKYCRecord)
	s.handle(g, http.MethodPost, "/kyc/{id}/review", opsRoles, s.handleReviewKYC)
	s.handle(g, http.MethodGet, "/files/*", opsRoles, s.handleFile)
	s.handle(g, http.MethodPost, "/users/{id}/adjust", financeRoles, s.handleAdjust)
	s.handle(g, http.MethodPost, "/users/{id}/manual-recharge", financeRoles, s.handleManualRecharge)
}

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	p, err := pageOf(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	q := r.URL.Query()
	list, total, err := s.Identity.AdminListUsers(r.Context(),
		identity.UserFilter{Q: q.Get("q"), KYCStatus: q.Get("kyc_status"), Status: q.Get("status")}, p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page(list, total))
}

func (s *Server) handleUser(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	ctx := r.Context()
	u, err := s.Identity.GetUser(ctx, id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	balance, err := s.Wallet.Balance(ctx, id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	insts, _, err := s.Rental.AdminListInstances(ctx, rental.InstanceFilter{UserID: id}, paging.Page{Number: 1, Size: 20})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	orders, err := s.Rental.ListOrders(ctx, id, 20)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	ledger, err := s.Wallet.ListLedger(ctx, id, 0, 20)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	kyc, err := s.Identity.UserKYCRecords(ctx, id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"user": u, "balance": balance, "instances": s.instanceViews(insts), "orders": orderViews(orders),
		"ledger": httpx.List(ledger)["items"], "kyc": httpx.List(kyc)["items"],
	})
}

func (s *Server) handleBan(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.Identity.Ban(r.Context(), s.actor(r), id, req.Reason); err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.OK(w)
}

func (s *Server) handleUnban(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.Identity.Unban(r.Context(), s.actor(r), id); err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.OK(w)
}

func (s *Server) handlePendingKYC(w http.ResponseWriter, r *http.Request) {
	p, err := pageOf(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	list, total, err := s.Identity.PendingEnterpriseKYC(r.Context(), p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page(list, total))
}

func (s *Server) handleKYCRecord(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	rec, err := s.Identity.GetKYCRecord(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, rec)
}

func (s *Server) handleReviewKYC(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var req struct {
		Approve bool   `json:"approve"`
		Reason  string `json:"reason"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.Identity.ReviewEnterpriseKYC(r.Context(), s.actor(r), id, req.Approve, req.Reason); err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.OK(w)
}

// handleFile 输出上传的文件（营业执照），供审核查看。
func (s *Server) handleFile(w http.ResponseWriter, r *http.Request) {
	up, rc, err := s.Uploads.Open(r.Context(), chi.URLParam(r, "*"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	defer rc.Close()
	h := w.Header()
	h.Set("Content-Type", up.ContentType)
	h.Set("Content-Length", strconv.FormatInt(up.Size, 10))
	h.Set("Content-Disposition", "inline")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, rc); err != nil {
		s.Log.Warn("serve upload aborted", "key", path.Clean(up.Key), "err", err)
	}
}

type moneyRequest struct {
	Amount int64  `json:"amount"`
	Remark string `json:"remark"`
	TOTP   string `json:"totp"`
}

// decodeMoney 解析调账 / 入账请求并完成二次验证。
func (s *Server) decodeMoney(w http.ResponseWriter, r *http.Request) (int64, moneyRequest, bool) {
	id, err := pathID(r, "id")
	if err == nil {
		var req moneyRequest
		if err = httpx.DecodeJSON(r, &req); err == nil {
			if err = s.stepUp(r, req.TOTP); err == nil {
				return id, req, true
			}
		}
	}
	s.writeError(w, r, err)
	return 0, moneyRequest{}, false
}

func (s *Server) handleAdjust(w http.ResponseWriter, r *http.Request) {
	id, req, ok := s.decodeMoney(w, r)
	if !ok {
		return
	}
	after, err := s.Wallet.AdminAdjust(r.Context(), currentSession(r).Admin.ID, id, req.Amount, req.Remark, s.actor(r).IP)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]int64{"balance": after})
}

func (s *Server) handleManualRecharge(w http.ResponseWriter, r *http.Request) {
	id, req, ok := s.decodeMoney(w, r)
	if !ok {
		return
	}
	rch, err := s.Billing.ManualRecharge(r.Context(), s.actor(r), id, req.Amount, req.Remark)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, rch)
}
