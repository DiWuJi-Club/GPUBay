// Package linux 是 clusternet.Net 的 Linux 实现（计划 13 规格 §3.2～§3.4）：
// sysfs 探测网卡与 RDMA；routed（lan）：veth + 宿主机 /32 路由 + SPARK-CLU；macvlan（cx7）：CX-7 口 / VLAN 子接口上的 macvlan；
// RDMA 设备与 RoCE v2 GID。所有命令经 cmdrun.Runner（测试用 cmdrun.Recorder 做快照），sysfs 读取经 Options.SysRoot。
//
// Apply 是差量、非破坏性的：Agent 重启后 Recover 会对所有 running 实例再调一次 Apply，正在跑的 NCCL / Ray 连接不能被打断——
// 容器内已有 spc0 时绝不 ip link del / add，只执行 replace / 幂等写。
package linux

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"spark-cloud/internal/agent/clusternet"
	"spark-cloud/internal/agent/cmdrun"
	"spark-cloud/internal/clusterspec"
)

// Options 配置 Net；零值字段取默认。
type Options struct {
	SysRoot  string        // 默认 "/sys"；测试指向 testdata
	DevRoot  string        // 默认 "/dev"
	ProcRoot string        // 默认 "/proc"：核对容器内 /dev/infiniband（<ProcRoot>/<pid>/root/dev/infiniband）
	GIDWait  time.Duration // 默认 5s：RoCE GID 表异步填充，每 200ms 查一次
	Log      *slog.Logger
}

// Net 实现 clusternet.Net。
type Net struct {
	run cmdrun.Runner
	opt Options

	mu         sync.Mutex
	lanIP      string                      // 最近一次 Probe 的 lanIP（非空时记下）：macvlan 绝不碰持有它的网卡
	lastRouted map[string]clusterspec.Spec // 最近一次 SyncRules 的输入：Check 据此比对 SPARK-CLU 全量
}

var _ clusternet.Net = (*Net)(nil)

// gidPoll 是等 RoCE GID 时的轮询间隔。
const gidPoll = 200 * time.Millisecond

// bridge 是普通实例网桥（跳转组第一条 -i spark0；与 executor.Deps.Bridge 默认值一致）。
const bridge = "spark0"

func New(run cmdrun.Runner, opt Options) *Net {
	if opt.SysRoot == "" {
		opt.SysRoot = "/sys"
	}
	if opt.DevRoot == "" {
		opt.DevRoot = "/dev"
	}
	if opt.ProcRoot == "" {
		opt.ProcRoot = "/proc"
	}
	if opt.GIDWait <= 0 {
		opt.GIDWait = 5 * time.Second
	}
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	return &Net{run: run, opt: opt}
}

// cerr 构造以 clusterspec.ErrPrefix 开头的错误（平台据此走集群网络失败路径、不判机器故障）。
func cerr(format string, args ...any) error {
	return fmt.Errorf(clusterspec.ErrPrefix+format, args...)
}

func (n *Net) ip(ctx context.Context, args ...string) ([]byte, error) {
	return n.run.Run(ctx, "ip", args...)
}

// ns 在容器 netns（nsenter -t <pid> -n）里执行命令；只切网络命名空间，用宿主机的 ip / sysctl。
func (n *Net) ns(ctx context.Context, pid int, name string, args ...string) ([]byte, error) {
	return n.run.Run(ctx, "nsenter", append([]string{"-t", strconv.Itoa(pid), "-n", name}, args...)...)
}

// steps 依次执行命令（每条 {name, args…}），第一条失败即返回（带 ErrPrefix）。
func (n *Net) steps(ctx context.Context, cmds ...[]string) error {
	for _, c := range cmds {
		if _, err := n.run.Run(ctx, c[0], c[1:]...); err != nil {
			return cerr("%w", err)
		}
	}
	return nil
}

func nsCmd(pid int, name string, args ...string) []string {
	return append([]string{"nsenter", "-t", strconv.Itoa(pid), "-n", name}, args...)
}

// missing：命令失败是因为对象不存在（网卡 / 路由），删除类操作据此忽略。
func missing(err error) bool {
	var ce *cmdrun.CmdError
	if !errors.As(err, &ce) {
		return false
	}
	for _, s := range []string{"Cannot find device", "does not exist", "No such process", "No such device"} {
		if strings.Contains(ce.Stderr, s) {
			return true
		}
	}
	return false
}

// linkExists：ip -o link show <dev> 成功即存在；对象不存在 → false；其它失败 → 错误。
func (n *Net) linkExists(ctx context.Context, dev string) (bool, string, error) {
	out, err := n.ip(ctx, "-o", "link", "show", dev)
	if err != nil {
		if missing(err) {
			return false, "", nil
		}
		return false, "", cerr("%w", err)
	}
	return true, string(out), nil
}

// hasSpc0 观察容器内是否已有 spc0（nsenter … ip -o -4 addr show dev spc0），同时返回其地址输出。
func (n *Net) hasSpc0(ctx context.Context, pid int) (bool, string, error) {
	out, err := n.ns(ctx, pid, "ip", "-o", "-4", "addr", "show", "dev", clusterspec.IfName)
	if err != nil {
		if missing(err) {
			return false, "", nil
		}
		return false, "", cerr("无法进入容器网络命名空间：%w", err)
	}
	return true, string(out), nil
}

// hasAddr：`ip -o -4 addr show` 的输出里是否有 cidr（如 10.210.3.11/32）。
func hasAddr(out, cidr string) bool {
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		for i := 0; i+1 < len(f); i++ {
			if f[i] == "inet" && f[i+1] == cidr {
				return true
			}
		}
	}
	return false
}

// selfCIDR 返回本成员在 spc0 上的地址：routed /32、macvlan /24。
func selfCIDR(s clusterspec.Spec) string {
	if s.Mode == clusterspec.ModeRouted {
		return s.Self().IP + "/32"
	}
	return s.Self().IP + "/24"
}

// Apply：先 Validate；macvlan 拒绝管理网卡；再按模式差量配置；RDMA 时核对设备编号并查 RoCE v2 GID。
func (n *Net) Apply(ctx context.Context, instanceID string, pid int, s clusterspec.Spec) (clusterspec.Runtime, error) {
	if err := s.Validate(); err != nil {
		return clusterspec.Runtime{}, cerr("%w", err)
	}
	if pid <= 0 {
		return clusterspec.Runtime{}, cerr("容器未运行（pid %d）", pid)
	}
	switch s.Mode {
	case clusterspec.ModeRouted:
		if err := n.applyRouted(ctx, instanceID, pid, s); err != nil {
			return clusterspec.Runtime{}, err
		}
	case clusterspec.ModeMacvlan:
		if err := n.refuseMgmt(ctx, s.Iface); err != nil {
			return clusterspec.Runtime{}, err
		}
		if err := n.applyMacvlan(ctx, instanceID, pid, s); err != nil {
			return clusterspec.Runtime{}, err
		}
	}
	if !s.RDMA {
		return clusterspec.Runtime{}, nil
	}
	return n.applyRDMA(ctx, pid, s)
}

// Teardown 清理该实例在宿主机上的集群网络：对端 /32 路由（本机没有别的实例用同一子网时）、sch / scm 网卡、
// 无人使用的 VLAN 子接口。对象不存在一律忽略；其它失败合并返回。
// 注意：容器仍在运行时，macvlan 模式容器内的 spc0 由 netns 持有，这里删不到（停容器 / 删 VLAN 子接口时随之消失）。
func (n *Net) Teardown(ctx context.Context, instanceID string, s clusterspec.Spec, remaining []clusterspec.Spec) error {
	var errs []error
	del := func(args ...string) {
		if _, err := n.ip(ctx, args...); err != nil && !missing(err) {
			errs = append(errs, err)
		}
	}
	switch s.Mode {
	case clusterspec.ModeRouted:
		shared := false
		for _, o := range remaining {
			if o.Mode == clusterspec.ModeRouted && o.Subnet == s.Subnet && o.Iface == s.Iface {
				shared = true
			}
		}
		if sub, err := netip.ParsePrefix(s.Subnet); err == nil && s.Iface != "" && !shared {
			if cur, err := n.peerRoutes(ctx, sub.String(), s.Iface); err == nil {
				for _, dst := range sortedKeys(cur) {
					del("route", "del", dst, "dev", s.Iface)
				}
			} else {
				for _, p := range s.Peers() {
					del("route", "del", p.IP+"/32", "dev", s.Iface)
				}
			}
		}
		del("link", "del", clusterspec.HostVeth(instanceID))
	case clusterspec.ModeMacvlan:
		del("link", "del", clusterspec.MacvlanName(instanceID))
		if s.VLAN != 0 {
			used := false
			for _, o := range remaining {
				if o.Mode == clusterspec.ModeMacvlan && o.VLAN == s.VLAN && o.Iface == s.Iface {
					used = true
				}
			}
			if !used {
				del("link", "del", clusterspec.VLANDev(s.VLAN))
			}
		}
	}
	if len(errs) > 0 {
		return cerr("%w", errors.Join(errs...))
	}
	return nil
}
