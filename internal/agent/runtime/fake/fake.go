// Package fake 在内存中模拟容器运行时，可注入失败、容器意外退出、数据盘缺失。
package fake

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"sync"

	"spark-cloud/internal/agent/runtime"
)

type Runtime struct {
	mu         sync.Mutex
	containers map[string]*entry
	pulled     []string
	failNext   map[string]error
	Veth       string
}

type entry struct {
	c runtime.Container
}

func New() *Runtime {
	return &Runtime{containers: map[string]*entry{}, failNext: map[string]error{}, Veth: "veth0abc"}
}

func (r *Runtime) FailNext(op string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failNext[op] = err
}

func (r *Runtime) fail(op string) error {
	if err, ok := r.failNext[op]; ok {
		delete(r.failNext, op)
		return err
	}
	return nil
}

func (r *Runtime) EnsureNetwork(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.fail("network")
}

func (r *Runtime) Pull(_ context.Context, ref string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.fail("pull"); err != nil {
		return err
	}
	r.pulled = append(r.pulled, ref)
	return nil
}

func (r *Runtime) Create(_ context.Context, s runtime.Spec) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.fail("create"); err != nil {
		return err
	}
	if _, ok := r.containers[s.InstanceID]; ok {
		return fmt.Errorf("Conflict. The container name %q is already in use", s.InstanceID)
	}
	r.containers[s.InstanceID] = &entry{c: runtime.Container{Exists: true, Image: s.ImageRef, Label: s.InstanceID, DataMounted: s.DataDir != "", DataSource: s.DataDir, Pid: 4242, Networks: []string{"spark0"}, PublishIPs: publishIPs(s.LanIP)}}
	return nil
}

func publishIPs(ip string) []string {
	if ip == "" {
		return nil
	}
	return []string{ip}
}

// ConnectNetwork 把网络加回容器的挂接列表（已挂接则不变）。
func (r *Runtime) ConnectNetwork(_ context.Context, id, network string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.fail("connect"); err != nil {
		return err
	}
	e, ok := r.containers[id]
	if !ok {
		return &notFound{id}
	}
	if !slices.Contains(e.c.Networks, network) {
		e.c.Networks = append(e.c.Networks, network)
	}
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

func (r *Runtime) Start(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
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

func (r *Runtime) Stop(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.fail("stop"); err != nil {
		return err
	}
	if e, ok := r.containers[id]; ok {
		e.c.Running = false
	}
	return nil
}

func (r *Runtime) Remove(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.fail("remove"); err != nil {
		return err
	}
	delete(r.containers, id)
	return nil
}

func (r *Runtime) Inspect(_ context.Context, id string) (runtime.Container, error) {
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

func (r *Runtime) List(context.Context) ([]runtime.Container, error) {
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

func (r *Runtime) VethOf(_ context.Context, id string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.fail("veth"); err != nil {
		return "", err
	}
	if e, ok := r.containers[id]; !ok || !e.c.Running {
		return "", &notFound{id}
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
