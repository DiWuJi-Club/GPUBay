package adminapi

import (
	"context"
	"encoding/csv"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/httpx"
	"spark-cloud/internal/rental"
)

func (s *Server) machineRoutes(g chi.Router) {
	s.handle(g, http.MethodGet, "/machines", opsRoles, s.handleListMachines)
	s.handle(g, http.MethodPost, "/machines", opsRoles, s.handleRegisterMachine)
	s.handle(g, http.MethodPost, "/machines/import", opsRoles, s.handleImportMachines)
	s.handle(g, http.MethodGet, "/machines/{id}", opsRoles, s.handleMachine)
	s.handle(g, http.MethodPost, "/machines/{id}/maintenance", opsRoles, s.handleMaintenance)
	s.handle(g, http.MethodPost, "/machines/{id}/power-cycle", opsRoles, s.handlePowerCycle)
	s.handle(g, http.MethodPost, "/machines/{id}/enroll-command", opsRoles, s.handleEnrollCommand)
	s.handle(g, http.MethodPost, "/machines/{id}/revoke-enrollment", opsRoles, s.handleRevokeEnrollment)
	s.handle(g, http.MethodGet, "/pdus", opsRoles, s.handleListPDUs)
	s.handle(g, http.MethodPost, "/pdus", opsRoles, s.handleCreatePDU)
	s.handle(g, http.MethodPut, "/pdus/{id}", opsRoles, s.handleUpdatePDU)
	s.handle(g, http.MethodGet, "/alerts", allRoles, s.handleListAlerts)
	s.handle(g, http.MethodPost, "/alerts/{id}/resolve", allRoles, s.handleResolveAlert)
}

// parseBoolParam 解析可选的 true / false 查询参数。
func parseBoolParam(v, name string) (*bool, error) {
	switch v {
	case "":
		return nil, nil
	case "true":
		b := true
		return &b, nil
	case "false":
		b := false
		return &b, nil
	}
	return nil, apperr.BadRequest(name + " 只能是 true 或 false")
}

func (s *Server) handleListMachines(w http.ResponseWriter, r *http.Request) {
	p, err := pageOf(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	q := r.URL.Query()
	online, err := parseBoolParam(q.Get("online"), "online")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	f := fleet.MachineFilter{State: q.Get("state"), Online: online, Rack: q.Get("rack")}
	// group_id=none 是未分组机器（不能交给 queryInt）；其余必须是正整数
	if q.Get("group_id") == "none" {
		f.Ungrouped = true
	} else if f.GroupID, err = queryInt(r, "group_id"); err != nil {
		s.writeError(w, r, err)
		return
	}
	list, total, err := s.Fleet.ListMachines(r.Context(), f, p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page(list, total))
}

type machineRequest struct {
	Serial    string `json:"serial"`
	Rack      string `json:"rack"`
	Slot      string `json:"slot"`
	LanIP     string `json:"lan_ip"`
	ProductID int64  `json:"product_id"`
	PDUID     *int64 `json:"pdu_id"`
	PDUOutlet *int   `json:"pdu_outlet"`
}

func (s *Server) handleRegisterMachine(w http.ResponseWriter, r *http.Request) {
	var req machineRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	m, err := s.Fleet.RegisterAudited(r.Context(), s.actor(r), fleet.NewMachine{Serial: req.Serial, Rack: req.Rack, Slot: req.Slot,
		LanIP: req.LanIP, ProductID: req.ProductID, PDUID: req.PDUID, PDUOutlet: req.PDUOutlet})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, m)
}

var importHeader = []string{"serial", "rack", "slot", "lan_ip", "product_code", "pdu_name", "pdu_outlet"}

// parseMachineCSV 解析导入文件；能在解析阶段发现的错误（未知商品、未知 PDU、插座号格式）作为行错误返回。
func (s *Server) parseMachineCSV(ctx context.Context, text string) ([]fleet.ImportRow, []fleet.RowError, error) {
	rd := csv.NewReader(strings.NewReader(strings.TrimPrefix(text, "\uFEFF")))
	rd.FieldsPerRecord = -1
	rd.TrimLeadingSpace = true
	header, err := rd.Read()
	if err != nil || len(header) != len(importHeader) {
		return nil, nil, apperr.BadRequest("表头必须是 " + strings.Join(importHeader, ","))
	}
	for i, h := range header {
		if strings.ToLower(strings.TrimSpace(h)) != importHeader[i] {
			return nil, nil, apperr.BadRequest("表头必须是 " + strings.Join(importHeader, ","))
		}
	}
	products := map[string]int64{}
	pdus := map[string]int64{}
	var rows []fleet.ImportRow
	var rowErrs []fleet.RowError
	for line := 2; ; line++ {
		rec, err := rd.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		fail := func(msg string) { rowErrs = append(rowErrs, fleet.RowError{Line: line, Message: msg}) }
		if err != nil || len(rec) != len(importHeader) {
			fail("列数不正确或格式错误")
			continue
		}
		for i := range rec {
			rec[i] = strings.TrimSpace(rec[i])
		}
		code := rec[4]
		pid, ok := products[code]
		if !ok {
			pid, err = s.Catalog.ProductIDByCode(ctx, code)
			if errors.Is(err, apperr.ErrNotFound) {
				fail("商品 " + code + " 不存在")
				continue
			}
			if err != nil {
				return nil, nil, err
			}
			products[code] = pid
		}
		nm := fleet.NewMachine{Serial: rec[0], Rack: rec[1], Slot: rec[2], LanIP: rec[3], ProductID: pid}
		if rec[5] != "" || rec[6] != "" {
			id, ok := pdus[rec[5]]
			if !ok {
				id, err = s.Fleet.PDUIDByName(ctx, s.Fleet.Pool(), rec[5])
				if errors.Is(err, apperr.ErrNotFound) {
					fail("PDU " + rec[5] + " 不存在")
					continue
				}
				if err != nil {
					return nil, nil, err
				}
				pdus[rec[5]] = id
			}
			outlet, err := strconv.Atoi(rec[6])
			if err != nil {
				fail("PDU 插座号必须是数字")
				continue
			}
			nm.PDUID, nm.PDUOutlet = &id, &outlet
		}
		rows = append(rows, fleet.ImportRow{Line: line, Machine: nm})
	}
	return rows, rowErrs, nil
}

func (s *Server) handleImportMachines(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CSV string `json:"csv"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	rows, rowErrs, err := s.parseMachineCSV(r.Context(), req.CSV)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	machines, err := s.Fleet.Import(r.Context(), s.actor(r), rows, rowErrs)
	var ie *fleet.ImportError
	if errors.As(err, &ie) {
		httpx.WriteJSON(w, http.StatusBadRequest, map[string]any{
			"code": "import_invalid", "message": "导入数据有误，未导入任何机器", "request_id": middleware.GetReqID(r.Context()),
			"errors": ie.Rows,
		})
		return
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, httpx.List(machines))
}

func (s *Server) handleMachine(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	m, err := s.Fleet.Get(r.Context(), s.Fleet.Pool(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	insts, err := s.Rental.ListInstancesByMachine(r.Context(), id, 21)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var current *instanceView
	history := make([]instanceView, 0, len(insts))
	for _, i := range insts {
		v := s.instanceView(i)
		switch {
		case current == nil && i.State != rental.StateReleased && i.State != rental.StateFailed && i.State != rental.StateCancelled:
			current = &v
		case len(history) < 20:
			history = append(history, v)
		}
	}
	var group *fleet.Group
	if m.GroupID != nil {
		g, err := s.Fleet.GetGroup(r.Context(), s.Fleet.Pool(), *m.GroupID)
		if err != nil && !errors.Is(err, apperr.ErrNotFound) {
			s.writeError(w, r, err)
			return
		}
		if err == nil {
			group = &g
		}
	}
	leases, err := s.Rental.MachineLeases(r.Context(), id, s.Clock.Now(), 60)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	upcoming, err := s.Rental.UpcomingBookingsForMachine(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"machine": m, "current_instance": current, "history": history,
		"group": group, "leases": leases, "upcoming_bookings": upcoming})
}

func (s *Server) handleMaintenance(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var req struct {
		On bool `json:"on"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.Rental.SetMachineMaintenance(r.Context(), s.actor(r), id, req.On); err != nil {
		s.writeError(w, r, err)
		return
	}
	m, err := s.Fleet.Get(r.Context(), s.Fleet.Pool(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	upcoming, err := s.Rental.UpcomingBookingsForMachine(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, struct {
		fleet.Machine
		UpcomingBookings int `json:"upcoming_bookings"`
	}{m, upcoming})
}

func (s *Server) handlePowerCycle(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.Fleet.PowerCycle(r.Context(), s.actor(r), id); err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.OK(w)
}

// handleEnrollCommand 签发一次性接入令牌并返回安装命令（只显示一次；令牌明文不进日志与审计）。
func (s *Server) handleEnrollCommand(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var req struct {
		TOTP string `json:"totp"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if s.Gateway == nil {
		s.writeError(w, r, apperr.New("gateway_unavailable", "未配置 Agent 网关", http.StatusInternalServerError))
		return
	}
	var tok fleet.EnrollToken
	if err := s.withStepUp(r, req.TOTP, func() (err error) {
		tok, err = s.Fleet.IssueEnrollToken(r.Context(), s.actor(r), id)
		return err
	}); err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"command": s.Gateway.InstallCommand(tok.Plain), "token_id": tok.ID, "expires_at": tok.ExpiresAt})
}

func (s *Server) handleRevokeEnrollment(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var req struct {
		TOTP string `json:"totp"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.withStepUp(r, req.TOTP, func() error { return s.Fleet.RevokeEnrollment(r.Context(), s.actor(r), id) }); err != nil {
		s.writeError(w, r, err)
		return
	}
	m, err := s.Fleet.Get(r.Context(), s.Fleet.Pool(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, m)
}

type pduRequest struct {
	Name       string `json:"name"`
	Driver     string `json:"driver"`
	Address    string `json:"address"`
	Community  string `json:"community"`
	OIDPrefix  string `json:"oid_prefix"`
	CycleValue int    `json:"cycle_value"`
	OnValue    int    `json:"on_value"`
}

func (p pduRequest) input() fleet.PDUInput {
	return fleet.PDUInput{Name: p.Name, Driver: p.Driver, Address: p.Address, Community: p.Community,
		OIDPrefix: p.OIDPrefix, CycleValue: p.CycleValue, OnValue: p.OnValue}
}

func (s *Server) handleListPDUs(w http.ResponseWriter, r *http.Request) {
	list, err := s.Fleet.ListPDUs(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.List(list))
}

func (s *Server) handleCreatePDU(w http.ResponseWriter, r *http.Request) {
	var req pduRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	p, err := s.Fleet.CreatePDU(r.Context(), s.actor(r), req.input())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, p)
}

func (s *Server) handleUpdatePDU(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var req pduRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	p, err := s.Fleet.UpdatePDU(r.Context(), s.actor(r), id, req.input())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, p)
}

func (s *Server) handleListAlerts(w http.ResponseWriter, r *http.Request) {
	p, err := pageOf(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	resolved, err := parseBoolParam(r.URL.Query().Get("resolved"), "resolved")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	list, total, err := s.Fleet.ListAlerts(r.Context(), resolved, p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page(list, total))
}

func (s *Server) handleResolveAlert(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.Fleet.ResolveAlert(r.Context(), s.actor(r), id); err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.OK(w)
}
