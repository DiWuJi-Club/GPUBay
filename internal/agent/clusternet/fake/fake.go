// Package fake 是 clusternet.Net 的内存实现：记录调用、可注入失败（executor 单测、平台集成测试与 e2e 的 fake Agent 用）。
// 所有配置经带锁的 Set* 方法，所有记录经带锁的访问器读取（返回副本）。
package fake

import (
	"context"
	"errors"
	"slices"
	"sync"

	"spark-cloud/internal/agent/clusternet"
	"spark-cloud/internal/clusterspec"
)

// ErrCheckFailed 是 SetCheckResult(false) 时 Check 返回的问题描述。
var ErrCheckFailed = errors.New("spc0 缺失（fake）")

// ApplyCall 是一次 Apply 调用（失败的也记录）。
type ApplyCall struct {
	InstanceID string
	PID        int
	Spec       clusterspec.Spec
}

type Net struct {
	mu sync.Mutex

	info        clusterspec.NetInfo
	devs        []string
	runtime     clusterspec.Runtime
	checkResult bool

	failProbe, failDevices, failApply, failCheck, failTeardown, failSync error

	applies   []ApplyCall
	teardowns []string
	rules     map[string]clusterspec.Spec
	checks    int
	syncs     int
}

var _ clusternet.Net = (*Net)(nil)

// New 返回一个空的 fake：Probe 返回零值 NetInfo、Devices 返回 nil、Check 恒为 true、Apply 返回零值 Runtime。
func New() *Net { return &Net{checkResult: true} }

// ---- 配置 ----

// SetInfo 设置 Probe 的返回值。
func (n *Net) SetInfo(info clusterspec.NetInfo) { n.with(func() { n.info = copyInfo(info) }) }

// SetDevices 设置 RDMA Spec 下 Devices 的返回值。
func (n *Net) SetDevices(devs []string) { n.with(func() { n.devs = slices.Clone(devs) }) }

// SetRuntime 设置 Apply 的返回值（失败时也一并返回，便于模拟 ErrNoGID + RDMAActive=false）。
func (n *Net) SetRuntime(rt clusterspec.Runtime) { n.with(func() { n.runtime = rt }) }

// SetCheckResult 设置 Check 的结果（默认 true）；false 时 Check 返回 (false, ErrCheckFailed)。
func (n *Net) SetCheckResult(ok bool) { n.with(func() { n.checkResult = ok }) }

// SetFailProbe 等让对应方法持续返回 err，直到再以 nil 调用。
func (n *Net) SetFailProbe(err error)     { n.with(func() { n.failProbe = err }) }
func (n *Net) SetFailDevices(err error)   { n.with(func() { n.failDevices = err }) }
func (n *Net) SetFailApply(err error)     { n.with(func() { n.failApply = err }) }
func (n *Net) SetFailCheck(err error)     { n.with(func() { n.failCheck = err }) }
func (n *Net) SetFailTeardown(err error)  { n.with(func() { n.failTeardown = err }) }
func (n *Net) SetFailSyncRules(err error) { n.with(func() { n.failSync = err }) }

// Info 返回当前 Probe 配置的副本。
func (n *Net) Info() clusterspec.NetInfo {
	n.mu.Lock()
	defer n.mu.Unlock()
	return copyInfo(n.info)
}

// ---- 记录 ----

// Applies 返回全部 Apply 调用（按调用顺序）。
func (n *Net) Applies() []ApplyCall {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]ApplyCall, len(n.applies))
	for i, a := range n.applies {
		out[i] = ApplyCall{InstanceID: a.InstanceID, PID: a.PID, Spec: copySpec(a.Spec)}
	}
	return out
}

// Teardowns 返回 Teardown 过的实例 ID（按调用顺序，失败的也记录）。
func (n *Net) Teardowns() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.teardowns)
}

// Rules 返回最近一次成功 SyncRules 的参数副本（从未调用为 nil）。
func (n *Net) Rules() map[string]clusterspec.Spec {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.rules == nil {
		return nil
	}
	out := make(map[string]clusterspec.Spec, len(n.rules))
	for k, v := range n.rules {
		out[k] = copySpec(v)
	}
	return out
}

// Checks 返回 Check 的调用次数。
func (n *Net) Checks() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.checks
}

// SyncCount 返回成功 SyncRules 的次数。
func (n *Net) SyncCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.syncs
}

// ---- clusternet.Net ----

func (n *Net) Probe(context.Context, string) (clusterspec.NetInfo, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.failProbe != nil {
		return clusterspec.NetInfo{}, n.failProbe
	}
	return copyInfo(n.info), nil
}

func (n *Net) Devices(s clusterspec.Spec) ([]string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.failDevices != nil {
		return nil, n.failDevices
	}
	if !s.RDMA {
		return nil, nil
	}
	return slices.Clone(n.devs), nil
}

func (n *Net) Apply(_ context.Context, instanceID string, pid int, s clusterspec.Spec) (clusterspec.Runtime, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.applies = append(n.applies, ApplyCall{InstanceID: instanceID, PID: pid, Spec: copySpec(s)})
	return n.runtime, n.failApply
}

func (n *Net) Check(context.Context, string, int, clusterspec.Spec) (bool, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.checks++
	if n.failCheck != nil {
		return false, n.failCheck
	}
	if !n.checkResult {
		return false, ErrCheckFailed // 约定：ok = false 时 err 为问题描述
	}
	return true, nil
}

func (n *Net) Teardown(_ context.Context, instanceID string, _ clusterspec.Spec, _ []clusterspec.Spec) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.teardowns = append(n.teardowns, instanceID)
	return n.failTeardown
}

func (n *Net) SyncRules(_ context.Context, routed map[string]clusterspec.Spec) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.failSync != nil {
		return n.failSync
	}
	n.rules = make(map[string]clusterspec.Spec, len(routed))
	for k, v := range routed {
		n.rules[k] = copySpec(v)
	}
	n.syncs++
	return nil
}

func (n *Net) with(f func()) {
	n.mu.Lock()
	defer n.mu.Unlock()
	f()
}

func copySpec(s clusterspec.Spec) clusterspec.Spec {
	s.Members = slices.Clone(s.Members)
	return s
}

func copyInfo(i clusterspec.NetInfo) clusterspec.NetInfo {
	ifaces := make([]clusterspec.Iface, len(i.Ifaces))
	for k, f := range i.Ifaces {
		f.IPv4 = slices.Clone(f.IPv4)
		ifaces[k] = f
	}
	if i.Ifaces == nil {
		ifaces = nil
	}
	i.Ifaces = ifaces
	return i
}
