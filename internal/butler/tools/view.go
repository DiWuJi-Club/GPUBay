package tools

import (
	"context"
	"errors"
	"fmt"
	"time"

	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/butler/guard"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/tz"
	"spark-cloud/internal/rental"
)

// 结果大小（与 butler.MaxToolResultBytes / MaxSmallResultBytes 相同；tools 不能导入 butler）。
const (
	largeLimit = 16384 // 日志、实例 / 部署 / 应用详情与列表
	smallLimit = 4096  // 其它
	textLimit  = 1024  // 单个自由文本字段（事件、备注、端口 note、env 值、消息）
	descLimit  = 4096  // 应用说明
)

// clean 净化并截断一个自由文本字段（来自用户输入或容器输出；规格 §6.1 数据层）。
func clean(s string, limit int) string { return guard.Truncate(guard.Sanitize(s), limit) }

// ts 把时间格式化为北京时间 RFC3339（模型面向中国用户，直接给 +08:00 免得换算出错）。
func ts(t time.Time) string { return t.In(tz.CST).Format(time.RFC3339) }

func tsp(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := ts(*t)
	return &s
}

// InstanceBrief 是 list_instances / get_instance 的实例视图：与控制台 api.instanceView 同口径的精简子集（裁定「api.instanceView 复用」）。
// 绝不含实例 SSH 密码。
type InstanceBrief struct {
	ID               string              `json:"id"`
	State            string              `json:"state"`
	ProductID        int64               `json:"product_id"`
	ProductName      string              `json:"product_name"`
	ImageID          int64               `json:"image_id"`
	ImageName        string              `json:"image_name"`
	RemainingSeconds int64               `json:"remaining_seconds"`
	ExpiresAt        *string             `json:"expires_at"`
	Suspended        bool                `json:"suspended"`
	ScheduledStartAt *string             `json:"scheduled_start_at"`
	AutoRenew        bool                `json:"auto_renew"`
	MachineLabel     string              `json:"machine_label"`
	Apps             []appdeploy.Summary `json:"apps_summary"`
}

// nameCache 在一次工具调用内去重商品 / 镜像 / 机器名查询。
type nameCache struct {
	d                          Deps
	products, images, machines map[int64]string
}

func newNameCache(d Deps) *nameCache {
	return &nameCache{d: d, products: map[int64]string{}, images: map[int64]string{}, machines: map[int64]string{}}
}

// lookup：查不到（已删除）显示空名称而不是报错，与 api.nameCache 一致。
func (c *nameCache) lookup(cache map[int64]string, id int64, fetch func() (string, error)) (string, error) {
	if n, ok := cache[id]; ok {
		return n, nil
	}
	n, err := fetch()
	if errors.Is(err, apperr.ErrNotFound) {
		n, err = "", nil
	}
	if err != nil {
		return "", err
	}
	cache[id] = n
	return n, nil
}

func (c *nameCache) product(ctx context.Context, id int64) (string, error) {
	return c.lookup(c.products, id, func() (string, error) { return c.d.Catalog.ProductName(ctx, id) })
}

func (c *nameCache) image(ctx context.Context, id int64) (string, error) {
	return c.lookup(c.images, id, func() (string, error) { return c.d.Catalog.ImageName(ctx, id) })
}

func (c *nameCache) machine(ctx context.Context, id int64) (string, error) {
	if c.d.MachineLabel == nil || id == 0 {
		return "", nil
	}
	return c.lookup(c.machines, id, func() (string, error) { return c.d.MachineLabel(ctx, id) })
}

// instanceBrief 组装精简视图；apps 为 nil 时输出空数组。
func instanceBrief(ctx context.Context, nc *nameCache, inst rental.Instance, apps []appdeploy.Summary, now time.Time) (InstanceBrief, error) {
	b := InstanceBrief{ID: inst.ID, State: inst.State, ProductID: inst.ProductID, ImageID: inst.ImageID,
		RemainingSeconds: int64(inst.Remaining(now) / time.Second), ExpiresAt: tsp(inst.ExpiresAt), Suspended: inst.Suspended(),
		ScheduledStartAt: tsp(inst.ScheduledStartAt), AutoRenew: inst.AutoRenew, Apps: []appdeploy.Summary{}}
	var err error
	if b.ProductName, err = nc.product(ctx, inst.ProductID); err != nil {
		return b, err
	}
	if b.ImageName, err = nc.image(ctx, inst.ImageID); err != nil {
		return b, err
	}
	if b.MachineLabel, err = nc.machine(ctx, inst.MachineID); err != nil {
		return b, err
	}
	for _, a := range apps {
		a.Name = clean(a.Name, textLimit)
		b.Apps = append(b.Apps, a)
	}
	return b, nil
}

// renewable 与 api.viewNamed 一致：只有这些状态有续费档位。
func renewable(state string) bool {
	switch state {
	case rental.StateReserved, rental.StateCreating, rental.StateRunning, rental.StateStopped:
		return true
	}
	return false
}

// sshCommand 与 api.sshCommand 相同：公网端口 22 时省略 -p。
func sshCommand(id, host string, port int) string {
	if port <= 0 || port == 22 {
		return fmt.Sprintf("ssh %s@%s", id, host)
	}
	return fmt.Sprintf("ssh -p %d %s@%s", port, id, host)
}
