// Package fake 在内存中模拟容器运行时，可注入失败、阻塞（dockerd 卡死）、容器意外退出、数据盘缺失。
package fake

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"sort"
	"sync"

	"spark-cloud/internal/agent/runtime"
)

type Runtime struct {
	mu         sync.Mutex
	containers map[string]*entry
	specs      map[string]runtime.Spec // 每个容器最近一次 Create 的完整 Spec（供断言配额）
	created    int                     // Create 次数，参与生成 FullID：重建的容器 ID 不同
	pulled     []string
	images     []string // Images 的返回值（SetImages 设置；nil 表示不上报）
	failNext   map[string]error
	blocked    map[string]bool
	ops        []string // 改动类调用的顺序记录（"start ins-x"），供断言调用顺序
	Veth       string
}

type entry struct {
	c runtime.Container
}

func New() *Runtime {
	return &Runtime{containers: map[string]*entry{}, specs: map[string]runtime.Spec{}, failNext: map[string]error{}, blocked: map[string]bool{}, Veth: "veth0abc"}
}

func (r *Runtime) FailNext(op string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failNext[op] = err
}

// Block 打开 / 关闭某个操作的阻塞：打开后该操作一直等到 ctx 结束并返回 ctx 的错误（模拟 dockerd 卡死）。
// op 与 FailNext 相同：network / pull / create / connect / start / stop / remove / inspect / list / veth / images。
func (r *Runtime) Block(op string, on bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.blocked[op] = on
}

// wait 在操作被 Block 时阻塞到 ctx 结束；不持锁等待。
func (r *Runtime) wait(ctx context.Context, op string) error {
	r.mu.Lock()
	b := r.blocked[op]
	r.mu.Unlock()
	if !b {
		return nil
	}
	<-ctx.Done()
	return ctx.Err()
}

func (r *Runtime) fail(op string) error {
	if err, ok := r.failNext[op]; ok {
		delete(r.failNext, op)
		return err
	}
	return nil
}

func (r *Runtime) record(op, id string) { r.ops = append(r.ops, op+" "+id) }

// Ops 返回改动类调用（create / connect / start / stop / remove / veth）的顺序记录。
func (r *Runtime) Ops() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ops...)
}

func (r *Runtime) EnsureNetwork(ctx context.Context) error {
	if err := r.wait(ctx, "network"); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.fail("network")
}

func (r *Runtime) Pull(ctx context.Context, ref string) error {
	if err := r.wait(ctx, "pull"); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.fail("pull"); err != nil {
		return err
	}
	r.pulled = append(r.pulled, ref)
	return nil
}

func (r *Runtime) Create(ctx context.Context, s runtime.Spec) error {
	if err := r.wait(ctx, "create"); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.record("create", s.InstanceID)
	if err := r.fail("create"); err != nil {
		return err
	}
	if _, ok := r.containers[s.InstanceID]; ok {
		return fmt.Errorf("Conflict. The container name %q is already in use", s.InstanceID)
	}
	r.created++
	sum := sha256.Sum256(fmt.Appendf(nil, "%s#%d", s.InstanceID, r.created))
	r.specs[s.InstanceID] = s
	r.containers[s.InstanceID] = &entry{c: runtime.Container{Exists: true, FullID: hex.EncodeToString(sum[:]), Image: s.ImageRef, Label: s.InstanceID, DataMounted: s.DataDir != "", DataSource: s.DataDir, Pid: 4242, Networks: []string{"spark0"}, PublishIPs: publishIPs(s.LanIP)}}
	return nil
}

func publishIPs(ip string) []string {
	if ip == "" {
		return nil
	}
	return []string{ip}
}

// ConnectNetwork 把网络加回容器的挂接列表；与 docker 一致，已挂接时报错。
func (r *Runtime) ConnectNetwork(ctx context.Context, id, network string) error {
	if err := r.wait(ctx, "connect"); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.record("connect", id)
	if err := r.fail("connect"); err != nil {
		return err
	}
	e, ok := r.containers[id]
	if !ok {
		return &notFound{id}
	}
	if slices.Contains(e.c.Networks, network) {
		return fmt.Errorf("Error response from daemon: endpoint with name %s already exists in network %s", id, network)
	}
	e.c.Networks = append(e.c.Networks, network)
	return nil
}

// SetNetworks 直接设置容器的网络挂接（nil 模拟 Docker 启动失败后丢掉挂接）。
func (r *Runtime) SetNetworks(id string, nets []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.containers[id]; ok {
		e.c.Networks = nets
	}
}

// SetPublishIPs 直接设置容器端口发布绑定的宿主机地址。
func (r *Runtime) SetPublishIPs(id string, ips []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.containers[id]; ok {
		e.c.PublishIPs = ips
	}
}

func (r *Runtime) Start(ctx context.Context, id string) error {
	if err := r.wait(ctx, "start"); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.record("start", id)
	if err := r.fail("start"); err != nil {
		return err
	}
	e, ok := r.containers[id]
	if !ok {
		return &notFound{id}
	}
	e.c.Running = true
	return nil
}

type notFound struct{ id string }

func (e *notFound) Error() string { return "No such container: " + e.id }

func (r *Runtime) Stop(ctx context.Context, id string) error {
	if err := r.wait(ctx, "stop"); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.record("stop", id)
	if err := r.fail("stop"); err != nil {
		return err
	}
	if e, ok := r.containers[id]; ok {
		e.c.Running = false
	}
	return nil
}

func (r *Runtime) Remove(ctx context.Context, id string) error {
	if err := r.wait(ctx, "remove"); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.record("remove", id)
	if err := r.fail("remove"); err != nil {
		return err
	}
	delete(r.containers, id)
	return nil
}

func (r *Runtime) Inspect(ctx context.Context, id string) (runtime.Container, error) {
	if err := r.wait(ctx, "inspect"); err != nil {
		return runtime.Container{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.fail("inspect"); err != nil {
		return runtime.Container{}, err
	}
	if e, ok := r.containers[id]; ok {
		return e.c, nil
	}
	return runtime.Container{}, nil
}

func (r *Runtime) List(ctx context.Context) ([]runtime.Container, error) {
	if err := r.wait(ctx, "list"); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.fail("list"); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(r.containers))
	for id := range r.containers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]runtime.Container, 0, len(ids))
	for _, id := range ids {
		out = append(out, r.containers[id].c)
	}
	return out, nil
}

// VethOf：容器未运行或没有任何网络挂接（docker start 成功但容器里没有 eth0）时失败。
func (r *Runtime) VethOf(ctx context.Context, id string) (string, error) {
	if err := r.wait(ctx, "veth"); err != nil {
		return "", err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.record("veth", id)
	if err := r.fail("veth"); err != nil {
		return "", err
	}
	e, ok := r.containers[id]
	if !ok || !e.c.Running {
		return "", &notFound{id}
	}
	if len(e.c.Networks) == 0 {
		return "", fmt.Errorf("container %s has no network interface (eth0 missing)", id)
	}
	return r.Veth, nil
}

// Exit 模拟容器意外退出。
func (r *Runtime) Exit(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.containers[id]; ok {
		e.c.Running = false
	}
}

func (r *Runtime) SetDataMounted(id string, mounted bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.containers[id]; ok {
		e.c.DataMounted = mounted
		if !mounted {
			e.c.DataSource = ""
		}
	}
}

// SetDataSource 模拟 /root/data 绑定到了别的宿主机目录（例如被手工重建）。
func (r *Runtime) SetDataSource(id, src string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.containers[id]; ok {
		e.c.DataMounted = src != ""
		e.c.DataSource = src
	}
}

// SetImages 设置 Images 的返回值；nil 模拟旧 Agent / 拿不到列表（StateReport 不带 images）。
func (r *Runtime) SetImages(imgs []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.images = slices.Clone(imgs)
}

func (r *Runtime) Images(ctx context.Context) ([]string, error) {
	if err := r.wait(ctx, "images"); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.fail("images"); err != nil {
		return nil, err
	}
	return slices.Clone(r.images), nil
}

// Spec 返回该容器最近一次 Create 收到的 Spec（容器删除后仍保留，便于断言）。
func (r *Runtime) Spec(id string) (runtime.Spec, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.specs[id]
	return s, ok
}

func (r *Runtime) Pulled() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.pulled...)
}

func (r *Runtime) Containers() map[string]runtime.Container {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]runtime.Container{}
	for id, e := range r.containers {
		out[id] = e.c
	}
	return out
}
