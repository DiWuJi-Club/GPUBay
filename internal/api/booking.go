package api

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/tz"
	"spark-cloud/internal/rental"
)

const defaultCalendarLimit = 120

// ipLimiter 进程内按 IP 的滑动一分钟限流（匿名的机组 / 日历 / 可用性端点）。
type ipLimiter struct {
	mu    sync.Mutex
	limit int
	hits  map[string][]time.Time
}

func (l *ipLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.hits == nil {
		l.hits = map[string][]time.Time{}
	}
	ts := l.hits[key]
	i := 0
	for i < len(ts) && now.Sub(ts[i]) >= time.Minute {
		i++
	}
	ts = ts[i:]
	if len(ts) >= l.limit {
		l.hits[key] = ts
		return false
	}
	l.hits[key] = append(ts, now)
	// 匿名端点按 IP 建键：顺手清掉一个已空的键，map 不会无界增长（每次调用摊销 O(1)）
	for k, v := range l.hits {
		if k != key && (len(v) == 0 || now.Sub(v[len(v)-1]) >= time.Minute) {
			delete(l.hits, k)
			break
		}
	}
	return true
}

func (s *Server) calendarRateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.calendarLim.allow(clientIP(r), s.Clock.Now()) {
			s.writeError(w, r, apperr.ErrRateLimited)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// optionalUserID 匿名端点里尽力识别当前用户（机器日历的 mine 标记）；没有或无效会话返回 0。
func (s *Server) optionalUserID(r *http.Request) int64 {
	c, err := r.Cookie(s.cookieName())
	if err != nil {
		return 0
	}
	u, err := s.Identity.Authenticate(r.Context(), c.Value)
	if err != nil {
		return 0
	}
	return u.ID
}

func queryInt64(r *http.Request, name string) (*int64, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return nil, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return nil, apperr.BadRequest(name + " 必须是正整数")
	}
	return &n, nil
}

// calendarWindow 解析 from（YYYY-MM-DD，北京时间；缺省今天）与 days（缺省 61）。
func (s *Server) calendarWindow(r *http.Request) (time.Time, int, error) {
	q := r.URL.Query()
	from := s.Clock.Now()
	if v := q.Get("from"); v != "" {
		t, err := time.ParseInLocation("2006-01-02", v, tz.CST)
		if err != nil {
			return time.Time{}, 0, apperr.BadRequest("from 的格式应为 2006-01-02")
		}
		from = t.UTC()
	}
	days := 61
	if v := q.Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return time.Time{}, 0, apperr.BadRequest("days 必须是整数")
		}
		days = n
	}
	return from, days, nil
}

type groupView struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Region       string `json:"region"`
	Description  string `json:"description"`
	MachineCount int    `json:"machine_count"`
	IdleNow      int    `json:"idle_now"`
}

func (s *Server) handleGroups(w http.ResponseWriter, r *http.Request) {
	productID, err := queryInt64(r, "product_id") // 可选：只算该商品的机器（终审 M1）
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	rows, err := s.Fleet.ListGroups(r.Context(), true, productID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out := make([]groupView, 0, len(rows))
	for _, g := range rows {
		out = append(out, groupView{ID: g.ID, Name: g.Name, Region: g.Region, Description: g.Description, MachineCount: g.MachineCount, IdleNow: g.IdleNow})
	}
	// now：服务器时间。预订对话框的「最早 / 最晚整点」按它算（e2e 的假时钟与浏览器时间可能相差数天）。
	writeJSON(w, http.StatusOK, map[string]any{"items": out, "now": s.Clock.Now()})
}

func pathInt64(r *http.Request, name string) (int64, error) {
	n, err := strconv.ParseInt(chi.URLParam(r, name), 10, 64)
	if err != nil || n <= 0 {
		return 0, apperr.BadRequest("ID 不正确")
	}
	return n, nil
}

func (s *Server) handleGroupMachines(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	productID, err := queryInt64(r, "product_id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	items, err := s.Rental.GroupMachines(r.Context(), id, productID, true)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list(items))
}

func (s *Server) handleGroupCalendar(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	from, days, err := s.calendarWindow(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	productID, err := queryInt64(r, "product_id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out, err := s.Rental.GroupCalendar(r.Context(), id, productID, from, days, true)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"days": out})
}

func (s *Server) handleMachineCalendar(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	from, days, err := s.calendarWindow(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out, err := s.Rental.MachineCalendar(r.Context(), id, s.optionalUserID(r), from, days, false) // 机器不在可见机组 → 404（服务层校验）
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"periods": out})
}

func parseStartAt(v string) (*time.Time, error) {
	if v == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return nil, apperr.BadRequest("start_at 的格式应为 RFC3339，例如 2026-10-20T10:00:00+08:00")
	}
	t = t.UTC()
	return &t, nil
}

func (s *Server) handleAvailability(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	productID, err := queryInt64(r, "product_id")
	if err != nil || productID == nil {
		s.writeError(w, r, apperr.BadRequest("product_id 必须是正整数"))
		return
	}
	groupID, err := queryInt64(r, "group_id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	machineID, err := queryInt64(r, "machine_id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	start, err := parseStartAt(q.Get("start_at"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	quantity := 0
	if v := q.Get("quantity"); v != "" {
		if quantity, err = strconv.Atoi(v); err != nil {
			s.writeError(w, r, apperr.BadRequest("quantity 必须是整数"))
			return
		}
	}
	if machineID != nil { // 与指定机器下单同一校验：未上架的机器探测不到空闲情况（终审 M2）
		if err := s.bookableMachine(r.Context(), *machineID, groupID); err != nil {
			s.writeError(w, r, err)
			return
		}
	}
	out, err := s.Rental.Availability(r.Context(), rental.AvailabilityQuery{ProductID: *productID, GroupID: groupID, MachineID: machineID,
		StartAt: start, Duration: catalog.Duration(q.Get("duration")), Quantity: quantity})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

type bookingRequest struct {
	ProductID         int64  `json:"product_id"`
	ImageID           int64  `json:"image_id"`
	GroupID           *int64 `json:"group_id"`
	MachineID         *int64 `json:"machine_id"`
	Count             int    `json:"count"`
	Duration          string `json:"duration"`
	Quantity          int    `json:"quantity"`
	StartAt           string `json:"start_at"`
	AutoRenew         bool   `json:"auto_renew"`
	AutoRenewDuration string `json:"auto_renew_duration"`
	// 计划 7：每台实例开通后自动部署的应用（可选），见 POST /orders
	AppID  *int64            `json:"app_id"`
	AppEnv map[string]string `json:"app_env"`
	// 计划 11：随应用一并预装的 ComfyUI 工作流（须同时给 app_id），见 POST /orders
	AppWorkflowIDs []int64 `json:"app_workflow_ids"`
}

func (s *Server) batchResult(ctx context.Context, res rental.BatchResult) (map[string]any, error) {
	views, err := s.views(ctx, res.Instances)
	if err != nil {
		return nil, err
	}
	return map[string]any{"batch": res.Batch, "orders": res.Orders, "instances": views}, nil
}

// bookableMachine 指定机器下单的前置检查：先看机器所在机组可见，再看 group_id 与之相同；任何不满足都是 ErrNotFound。
func (s *Server) bookableMachine(ctx context.Context, machineID int64, groupID *int64) error {
	if err := s.Rental.VisibleMachine(ctx, machineID); err != nil {
		return err
	}
	m, err := s.Fleet.Get(ctx, s.Fleet.Pool(), machineID)
	if err != nil {
		return err
	}
	if groupID == nil || m.GroupID == nil || *m.GroupID != *groupID {
		return apperr.ErrNotFound
	}
	return nil
}

func (s *Server) handleCreateBooking(w http.ResponseWriter, r *http.Request) {
	var req bookingRequest
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := checkAppWorkflowIDs(req.AppWorkflowIDs); err != nil {
		s.writeError(w, r, err)
		return
	}
	start, err := parseStartAt(req.StartAt)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	// 规格 §10.1：指定机器时 group_id 必须是该机器所在的可见机组（validateBatch 不管这条，后台改派与 rental 测试依赖它的宽松）。
	// 机器不存在 / 所在机组隐藏 / group_id 不对一律同一个 404，探测不出隐藏机器属于哪个机组。
	if req.MachineID != nil {
		if err := s.bookableMachine(r.Context(), *req.MachineID, req.GroupID); err != nil {
			s.writeError(w, r, err)
			return
		}
	}
	res, err := s.Rental.PlaceBatch(r.Context(), rental.BatchRequest{
		UserID: currentUser(r).ID, ProductID: req.ProductID, ImageID: req.ImageID, GroupID: req.GroupID, MachineID: req.MachineID,
		Count: req.Count, Duration: catalog.Duration(req.Duration), Quantity: req.Quantity, StartAt: start,
		AutoRenew: req.AutoRenew, AutoRenewDuration: catalog.Duration(req.AutoRenewDuration), IdempotencyKey: r.Header.Get(idempotencyHeader),
		AppID: req.AppID, AppEnv: req.AppEnv, AppWorkflowIDs: req.AppWorkflowIDs,
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out, err := s.batchResult(r.Context(), res)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) handleBatch(w http.ResponseWriter, r *http.Request) {
	res, err := s.Rental.GetBatch(r.Context(), currentUser(r).ID, chi.URLParam(r, "id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out, err := s.batchResult(r.Context(), res)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCancelQuote(w http.ResponseWriter, r *http.Request) {
	q, err := s.Rental.CancelQuote(r.Context(), currentUser(r).ID, chi.URLParam(r, "id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, q)
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	inst, err := s.Rental.Cancel(r.Context(), currentUser(r).ID, chi.URLParam(r, "id"), clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	v, err := s.viewOne(r.Context(), inst)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleAutoRenew(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled  bool   `json:"enabled"`
		Duration string `json:"duration"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	inst, err := s.Rental.SetAutoRenew(r.Context(), currentUser(r).ID, chi.URLParam(r, "id"), req.Enabled, catalog.Duration(req.Duration), clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	v, err := s.viewOne(r.Context(), inst)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	items, err := s.Rental.UserEvents(r.Context(), currentUser(r).ID, chi.URLParam(r, "id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list(items))
}
