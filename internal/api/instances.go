package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/rental"
)

// idempotencyHeader 是下单与续费可选的幂等键请求头：同一用户重复提交同一键只扣款一次，重试返回首次的结果。
const idempotencyHeader = "Idempotency-Key"

type instanceView struct {
	ID               string     `json:"id"`
	State            string     `json:"state"`
	ProductID        int64      `json:"product_id"`
	ImageID          int64      `json:"image_id"`
	ProductName      string     `json:"product_name"`
	ImageName        string     `json:"image_name"`
	Suspended        bool       `json:"suspended"`
	ExpiresAt        *time.Time `json:"expires_at"`
	RemainingSeconds int64      `json:"remaining_seconds"`
	PendingSeconds   int64      `json:"pending_seconds"`
	RetainUntil      *time.Time `json:"retain_until"` // 停机后数据保留截止时间
	SSHCommand       string     `json:"ssh_command"`
	PasswordAuth     bool       `json:"password_auth"`
	SSHHost          string     `json:"ssh_host"`
	SSHPort          int        `json:"ssh_port"`
	PortsCount       int        `json:"ports_count"`
	// 计划 6
	ScheduledStartAt      *time.Time           `json:"scheduled_start_at"`
	ScheduledEndAt        *time.Time           `json:"scheduled_end_at"`
	CancellableUntil      *time.Time           `json:"cancellable_until"`
	CancellableSeconds    int64                `json:"cancellable_seconds"` // 按服务器时钟距取消截止的秒数（0 = 不可取消）；前端按拿到数据的时刻递减（终审 M3）
	GroupID               *int64               `json:"group_id"`
	GroupName             string               `json:"group_name"`
	MachineLabel          string               `json:"machine_label"`
	BatchID               *string              `json:"batch_id"`
	ReassignCount         int                  `json:"reassign_count"`
	AutoRenew             bool                 `json:"auto_renew"`
	AutoRenewDuration     string               `json:"auto_renew_duration"`
	AutoRenewFailedReason string               `json:"auto_renew_failed_reason"`
	RenewableUntil        *time.Time           `json:"renewable_until"`
	RenewOptions          []rental.RenewOption `json:"renew_options"`
	CreatedAt             time.Time            `json:"created_at"`
}

func (s *Server) view(i rental.Instance) instanceView {
	now := s.Clock.Now()
	v := instanceView{
		ID: i.ID, State: i.State, ProductID: i.ProductID, ImageID: i.ImageID, Suspended: i.Suspended(),
		ExpiresAt: i.ExpiresAt, RemainingSeconds: int64(i.Remaining(now) / time.Second),
		PendingSeconds: i.PendingSeconds, CreatedAt: i.CreatedAt,
	}
	v.PasswordAuth, v.SSHHost, v.SSHPort = i.PasswordAuth, s.SSHHost, s.sshPort()
	if i.State == rental.StateRunning {
		v.SSHCommand = sshCommand(i.ID, s.SSHHost, s.sshPort())
	}
	if i.State == rental.StateStopped && i.StoppedAt != nil && i.PendingSeconds == 0 {
		t := i.StoppedAt.Add(rental.RetainAfterStop)
		v.RetainUntil = &t
	}
	v.GroupID, v.BatchID, v.ReassignCount = i.GroupID, i.BatchID, i.ReassignCount
	v.AutoRenew, v.AutoRenewDuration, v.AutoRenewFailedReason = i.AutoRenew, string(i.AutoRenewDuration), i.AutoRenewFailedReason
	v.RenewOptions = []rental.RenewOption{} // 终态实例为空数组；可续状态在 viewNamed 里填
	if i.ScheduledStartAt != nil {
		v.ScheduledStartAt = i.ScheduledStartAt
		// 已开机（running / stopped）的到期以 expires_at 为准（续费、迟开通补偿都会改它）；开机前按开始时刻 + 待开始时长推算
		if (i.State == rental.StateRunning || i.State == rental.StateStopped) && i.ExpiresAt != nil {
			v.ScheduledEndAt = i.ExpiresAt
		} else {
			end := i.ScheduledStartAt.Add(time.Duration(i.PendingSeconds) * time.Second)
			v.ScheduledEndAt = &end
		}
		if i.State == rental.StateReserved {
			until := i.ScheduledStartAt.Add(-rental.PrepLead)
			v.CancellableUntil = &until
			v.CancellableSeconds = max(0, int64(until.Sub(now)/time.Second)) // 向下取整：按钮宁可早消失，不晚于后端截止
		}
	}
	return v
}

func (s *Server) sshPort() int {
	if s.SSHPort <= 0 {
		return 22
	}
	return s.SSHPort
}

// sshCommand：公网端口 22 时省略 -p。
func sshCommand(id, host string, port int) string {
	if port == 22 {
		return fmt.Sprintf("ssh %s@%s", id, host)
	}
	return fmt.Sprintf("ssh -p %d %s@%s", port, id, host)
}

// nameCache 在一次请求内去重商品 / 镜像名称查询（列表最多 100 条，通常只有 1 个商品、几个镜像）。
type nameCache struct {
	products, images map[int64]string
	machines, groups map[int64]string // 机器展示名、机组名
}

func newNameCache() *nameCache {
	return &nameCache{products: map[int64]string{}, images: map[int64]string{}, machines: map[int64]string{}, groups: map[int64]string{}}
}

func (c *nameCache) lookup(cache map[int64]string, id int64, fetch func() (string, error)) (string, error) {
	if n, ok := cache[id]; ok {
		return n, nil
	}
	n, err := fetch()
	if errors.Is(err, apperr.ErrNotFound) || errors.Is(err, apperr.ErrGroupNotFound) { // 商品与镜像没有删除接口；兜底显示空名称而不是报错
		n, err = "", nil
	}
	if err != nil {
		return "", err
	}
	cache[id] = n
	return n, nil
}

// viewNamed 在基础视图上补上商品名与镜像名。
func (s *Server) viewNamed(ctx context.Context, i rental.Instance, nc *nameCache) (instanceView, error) {
	v := s.view(i)
	var err error
	if v.ProductName, err = nc.lookup(nc.products, i.ProductID, func() (string, error) { return s.Catalog.ProductName(ctx, i.ProductID) }); err != nil {
		return v, err
	}
	if v.ImageName, err = nc.lookup(nc.images, i.ImageID, func() (string, error) { return s.Catalog.ImageName(ctx, i.ImageID) }); err != nil {
		return v, err
	}
	if v.MachineLabel, err = nc.lookup(nc.machines, i.MachineID, func() (string, error) {
		m, err := s.Fleet.Get(ctx, s.Fleet.Pool(), i.MachineID)
		if err != nil {
			return "", err
		}
		return m.Label(), nil
	}); err != nil {
		return v, err
	}
	if i.GroupID != nil {
		if v.GroupName, err = nc.lookup(nc.groups, *i.GroupID, func() (string, error) {
			g, err := s.Fleet.GetGroup(ctx, s.Fleet.Pool(), *i.GroupID)
			if err != nil {
				return "", err
			}
			return g.Name, nil
		}); err != nil {
			return v, err
		}
	}
	switch i.State { // 与 RenewQuantity 允许的状态一致；其余状态保持空数组
	case rental.StateReserved, rental.StateCreating, rental.StateRunning, rental.StateStopped:
		info, err := s.Rental.RenewInfoFor(ctx, i)
		if err != nil {
			return v, err
		}
		v.RenewableUntil, v.RenewOptions = info.RenewableUntil, info.Options
	}
	return v, nil
}

func (s *Server) viewOne(ctx context.Context, i rental.Instance) (instanceView, error) {
	vs, err := s.views(ctx, []rental.Instance{i})
	if err != nil {
		return instanceView{}, err
	}
	return vs[0], nil
}

func (s *Server) views(ctx context.Context, insts []rental.Instance) ([]instanceView, error) {
	ids := make([]string, 0, len(insts))
	for _, i := range insts {
		ids = append(ids, i.ID)
	}
	counts, err := s.Rental.PortCounts(ctx, ids)
	if err != nil {
		return nil, err
	}
	nc := newNameCache()
	out := make([]instanceView, 0, len(insts))
	for _, i := range insts {
		v, err := s.viewNamed(ctx, i, nc)
		if err != nil {
			return nil, err
		}
		v.PortsCount = counts[i.ID]
		out = append(out, v)
	}
	return out, nil
}

func (s *Server) handlePlaceOrder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProductID int64  `json:"product_id"`
		Duration  string `json:"duration"`
		ImageID   int64  `json:"image_id"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	ord, inst, err := s.Rental.Place(r.Context(), rental.PlaceRequest{
		UserID: currentUser(r).ID, ProductID: req.ProductID, ImageID: req.ImageID, Duration: catalog.Duration(req.Duration),
		IdempotencyKey: r.Header.Get(idempotencyHeader),
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	v, err := s.viewOne(r.Context(), inst)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"order": ord, "instance": v})
}

func (s *Server) handleOrders(w http.ResponseWriter, r *http.Request) {
	orders, err := s.Rental.ListOrders(r.Context(), currentUser(r).ID, 50)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list(orders))
}

func (s *Server) handleInstances(w http.ResponseWriter, r *http.Request) {
	insts, err := s.Rental.ListForUser(r.Context(), currentUser(r).ID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	views, err := s.views(r.Context(), insts)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list(views))
}

func (s *Server) handleInstance(w http.ResponseWriter, r *http.Request) {
	inst, err := s.Rental.GetForUser(r.Context(), currentUser(r).ID, chi.URLParam(r, "id"))
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

func (s *Server) handlePassword(w http.ResponseWriter, r *http.Request) {
	pw, err := s.Rental.Password(r.Context(), currentUser(r).ID, chi.URLParam(r, "id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"password": pw})
}

func (s *Server) handleRenew(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Duration string `json:"duration"`
		Quantity int    `json:"quantity"` // 0 / 缺省 = 1
	}
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	ord, err := s.Rental.RenewQuantity(r.Context(), currentUser(r).ID, chi.URLParam(r, "id"), catalog.Duration(req.Duration), req.Quantity,
		r.Header.Get(idempotencyHeader))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"order": ord})
}

func (s *Server) handleRelease(w http.ResponseWriter, r *http.Request) {
	if err := s.Rental.Release(r.Context(), currentUser(r).ID, chi.URLParam(r, "id")); err != nil {
		s.writeError(w, r, err)
		return
	}
	ok(w)
}
