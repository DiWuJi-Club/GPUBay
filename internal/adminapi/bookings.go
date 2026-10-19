package adminapi

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/httpx"
	"spark-cloud/internal/platform/tz"
	"spark-cloud/internal/rental"
)

func (s *Server) bookingRoutes(g chi.Router) {
	s.handle(g, http.MethodGet, "/bookings", allRoles, s.handleListBookings)
	s.handle(g, http.MethodGet, "/bookings/upcoming", allRoles, s.handleUpcomingBookings)
	s.handle(g, http.MethodPost, "/instances/{id}/cancel-booking", opsRoles, s.handleCancelBooking)
	s.handle(g, http.MethodPost, "/instances/{id}/reassign", opsRoles, s.handleReassign)
	s.handle(g, http.MethodGet, "/machines/{id}/calendar", opsRoles, s.handleMachineCalendar)
}

func (s *Server) handleListBookings(w http.ResponseWriter, r *http.Request) {
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
	gid, err := queryInt(r, "group_id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	mid, err := queryInt(r, "machine_id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	from, to, err := parseRange(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	list, total, err := s.Rental.AdminListBookings(r.Context(), rental.BookingFilter{State: r.URL.Query().Get("state"), UserID: uid, GroupID: gid, MachineID: mid, From: from, To: to}, p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	ids := make([]int64, 0, len(list))
	for _, i := range list {
		ids = append(ids, i.MachineID)
	}
	states, err := s.Rental.MachineStates(r.Context(), ids)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	views := make([]bookingView, 0, len(list))
	for _, i := range list {
		views = append(views, bookingView{instanceView: s.instanceView(i), MachineState: states[i.MachineID]})
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page(views, total))
}

// bookingView 预订列表行：实例视图加当前机器状态（faulty / maintenance 上的预订需要运维在宽限期前改派）。
type bookingView struct {
	instanceView
	MachineState string `json:"machine_state"`
}

type upcomingDay struct {
	Date  string `json:"date"`
	Count int    `json:"count"`
}

// upcoming 从今天（北京时间）起 days 天。
func (s *Server) upcoming(r *http.Request, days int) ([]upcomingDay, error) {
	today := tz.DayStart(s.Clock.Now())
	counts, err := s.Rental.UpcomingBookings(r.Context(), today, days)
	if err != nil {
		return nil, err
	}
	out := make([]upcomingDay, len(counts))
	for i, n := range counts {
		out[i] = upcomingDay{Date: today.Add(time.Duration(i) * 24 * time.Hour).In(tz.CST).Format("2006-01-02"), Count: n}
	}
	return out, nil
}

func (s *Server) handleUpcomingBookings(w http.ResponseWriter, r *http.Request) {
	days := 7
	if v := r.URL.Query().Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 61 {
			s.writeError(w, r, apperr.BadRequest("days 需在 1～61 之间"))
			return
		}
		days = n
	}
	out, err := s.upcoming(r, days)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"days": out})
}

// handleCancelBooking 后台取消预订（全额退款）：ops + 动态码。
func (s *Server) handleCancelBooking(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Reason string `json:"reason"`
		TOTP   string `json:"totp"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.stepUp(r, req.TOTP); err != nil {
		s.writeError(w, r, err)
		return
	}
	refunded, err := s.Rental.AdminCancelBooking(r.Context(), s.actor(r), chi.URLParam(r, "id"), req.Reason)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]int64{"refunded": refunded})
}

func (s *Server) handleReassign(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MachineID *int64 `json:"machine_id"`
		Reason    string `json:"reason"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	inst, err := s.Rental.AdminReassign(r.Context(), s.actor(r), chi.URLParam(r, "id"), req.MachineID, req.Reason)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, s.instanceView(inst))
}

func (s *Server) handleMachineCalendar(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if _, err := s.Fleet.Get(r.Context(), s.Fleet.Pool(), id); err != nil {
		s.writeError(w, r, err)
		return
	}
	q := r.URL.Query()
	from := s.Clock.Now()
	if v := q.Get("from"); v != "" {
		t, err := time.ParseInLocation("2006-01-02", v, tz.CST)
		if err != nil {
			s.writeError(w, r, apperr.BadRequest("from 的格式应为 2006-01-02"))
			return
		}
		from = t.UTC()
	}
	days := 61
	if v := q.Get("days"); v != "" {
		if days, err = strconv.Atoi(v); err != nil {
			s.writeError(w, r, apperr.BadRequest("days 必须是整数"))
			return
		}
	}
	out, err := s.Rental.MachineLeases(r.Context(), id, from, days)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"periods": out})
}
