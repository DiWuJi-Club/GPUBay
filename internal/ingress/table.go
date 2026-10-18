package ingress

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"spark-cloud/internal/rental"
)

type Source interface {
	ListIngressRoutes(ctx context.Context) ([]rental.IngressRoute, error)
}

type routeKey struct {
	instance string
	port     int
}

// snapshot 是一次刷新的不可变结果；发布后不再修改，读者无需加锁。
type snapshot struct {
	by  map[routeKey]rental.IngressRoute
	all []rental.IngressRoute
}

// Table 是内存路由表：每 2 秒从数据库整表刷新（200 台规模一条 SQL）；刷新失败保留旧表。
// 每次刷新构建新快照后原子替换，Lookup / Routes 只读当前快照。
type Table struct {
	src Source
	log *slog.Logger
	cur atomic.Pointer[snapshot]
}

func NewTable(src Source, log *slog.Logger) *Table {
	if log == nil {
		log = slog.Default()
	}
	t := &Table{src: src, log: log}
	t.cur.Store(&snapshot{by: map[routeKey]rental.IngressRoute{}})
	return t
}

// Refresh 拉取全部路由并原子替换路由表；出错时返回错误且保留旧表。
func (t *Table) Refresh(ctx context.Context) error {
	routes, err := t.src.ListIngressRoutes(ctx)
	if err != nil {
		return err
	}
	routes = append([]rental.IngressRoute(nil), routes...)
	by := make(map[routeKey]rental.IngressRoute, len(routes))
	for _, r := range routes {
		by[routeKey{r.InstanceID, r.Port}] = r
	}
	t.cur.Store(&snapshot{by: by, all: routes})
	return nil
}

// Run 按 interval 刷新直到 ctx 结束（先立即刷新一次）；刷新失败记日志并保留旧表。
func (t *Table) Run(ctx context.Context, interval time.Duration) {
	refresh := func() {
		c, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if err := t.Refresh(c); err != nil && ctx.Err() == nil {
			t.log.Error("ingress route refresh failed; keeping previous table", "err", err)
		}
	}
	refresh()
	tk := time.NewTicker(interval)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			refresh()
		}
	}
}

func (t *Table) Lookup(instanceID string, port int) (rental.IngressRoute, bool) {
	r, ok := t.cur.Load().by[routeKey{instanceID, port}]
	return r, ok
}

// Routes 返回当前路由表的副本。
func (t *Table) Routes() []rental.IngressRoute {
	return append([]rental.IngressRoute(nil), t.cur.Load().all...)
}
