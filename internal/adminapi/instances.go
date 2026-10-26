package adminapi

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/httpx"
	"spark-cloud/internal/rental"
)

func (s *Server) instanceRoutes(g chi.Router) {
	s.handle(g, http.MethodGet, "/instances", allRoles, s.handleListInstances)
	s.handle(g, http.MethodGet, "/instances/{id}", allRoles, s.handleInstance)
	s.handle(g, http.MethodGet, "/instances/{id}/refund-quote", allRoles, s.handleRefundQuote)
	s.handle(g, http.MethodPost, "/instances/{id}/extend", opsRoles, s.handleExtend)
	s.handle(g, http.MethodPost, "/instances/{id}/migrate", opsRoles, s.handleMigrate)
	s.handle(g, http.MethodPost, "/instances/{id}/rewipe", opsRoles, s.handleRewipe)
	s.handle(g, http.MethodPost, "/instances/{id}/refund-release", financeRoles, s.handleRefundRelease)
	s.handle(g, http.MethodGet, "/instances/{id}/ssh-sessions", allRoles, s.handleInstanceSSHSessions)
	s.handle(g, http.MethodPost, "/instances/{id}/ports/{port}/force-private", opsRoles, s.handleForcePortPrivate)
	s.handle(g, http.MethodDelete, "/instances/{id}/ports/{port}", opsRoles, s.handleAdminDeletePort)
}

// queryInt 解析可选的正整数查询参数；缺省返回 0。
func queryInt(r *http.Request, name string) (int64, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0, apperr.BadRequest(name + " 必须是正整数")
	}
	return n, nil
}

func (s *Server) handleListInstances(w http.ResponseWriter, r *http.Request) {
	p, err := pageOf(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	uid, err := queryInt(r, "user_id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	mid, err := queryInt(r, "machine_id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	list, total, err := s.Rental.AdminListInstances(r.Context(), rental.InstanceFilter{State: r.URL.Query().Get("state"), UserID: uid, MachineID: mid}, p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page(s.instanceViews(list), total))
}

func (s *Server) handleInstance(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	inst, err := s.Rental.Get(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	events, err := s.Rental.InstanceEvents(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	orders, err := s.Rental.InstanceOrders(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	cmds, err := s.Fleet.CommandsForInstance(r.Context(), id, 50)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	ports, err := s.Rental.ListPorts(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	// 应用部署含已卸载 / 已丢失的历史（排障用），按创建时间
	deps, err := s.Apps.ListForInstance(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	apps, err := s.Apps.AdminViews(r.Context(), deps)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if events == nil {
		events = []rental.Event{}
	}
	cluster, err := s.instanceCluster(r.Context(), inst) // 计划 13：集群成员的 cluster 区块（非成员 null）
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"cluster": cluster,
		"instance": s.instanceView(inst), "events": events, "orders": orderViews(orders), "commands": commandViews(cmds),
		"ports": s.portViews(id, ports), "ssh_host_pubkey_fingerprint": hostKeyFingerprint(inst.SSHHostPubkey), "apps": apps,
	})
}

func (s *Server) handleRefundQuote(w http.ResponseWriter, r *http.Request) {
	q, err := s.Rental.RefundQuote(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, q)
}

func (s *Server) handleExtend(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Seconds int64  `json:"seconds"`
		Reason  string `json:"reason"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	inst, err := s.Rental.AdminExtend(r.Context(), s.actor(r), chi.URLParam(r, "id"), req.Seconds, req.Reason)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, s.instanceView(inst))
}

func (s *Server) handleMigrate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Reason string `json:"reason"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	inst, err := s.Rental.AdminMigrate(r.Context(), s.actor(r), chi.URLParam(r, "id"), req.Reason)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"instance": s.instanceView(inst)})
}

func (s *Server) handleRewipe(w http.ResponseWriter, r *http.Request) {
	if err := s.Rental.AdminRewipe(r.Context(), s.actor(r), chi.URLParam(r, "id")); err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.OK(w)
}

func (s *Server) handleRefundRelease(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Amount int64  `json:"amount"`
		Reason string `json:"reason"`
		TOTP   string `json:"totp"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	var refunded int64
	if err := s.withStepUp(r, req.TOTP, func() (err error) {
		refunded, err = s.Rental.RefundRelease(r.Context(), s.actor(r), chi.URLParam(r, "id"), req.Amount, req.Reason)
		return err
	}); err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]int64{"refunded": refunded})
}
