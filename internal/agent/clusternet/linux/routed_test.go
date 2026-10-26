package linux

import (
	"context"
	"testing"

	"spark-cloud/internal/agent/cmdrun"
)

// 221 实测：`ip -4 route show root <subnet> dev <iface>` 不再打印 dev，末尾带一个空格。
const peerRoutes4of1 = "10.210.3.11 via 192.168.1.221 \n10.210.3.13 via 192.168.1.223 \n10.210.3.14 via 192.168.1.224 \n"

func TestApplyRoutedFirstTime(t *testing.T) {
	r := &cmdrun.Recorder{}
	stubNoSpc0(r, "4242")
	r.Stub("ip -o link show sch5678efgh", "", noDev("sch5678efgh"))
	n := newNet(r, Options{})
	rt, err := n.Apply(context.Background(), ids[1], 4242, routedSpec(1, 4))
	if err != nil || rt.RDMAActive || rt.HCA != "" {
		t.Fatalf("rt=%+v err=%v", rt, err)
	}
	assertLines(t, r, `
nsenter -t 4242 -n ip -o -4 addr show dev spc0
ip -o link show sch5678efgh
nsenter -t 4242 -n ip link del scc5678efgh
ip link add sch5678efgh mtu 1500 type veth peer name scc5678efgh mtu 1500
ip link set scc5678efgh netns 4242
nsenter -t 4242 -n ip link set scc5678efgh name spc0
nsenter -t 4242 -n ip addr replace 10.210.3.12/32 dev spc0
nsenter -t 4242 -n sysctl -w net.ipv6.conf.spc0.disable_ipv6=1
nsenter -t 4242 -n ip link set spc0 up
nsenter -t 4242 -n ip route replace 10.210.3.1/32 dev spc0 scope link
nsenter -t 4242 -n ip route replace 10.210.3.0/24 via 10.210.3.1 dev spc0
ip addr replace 10.210.3.1/32 dev sch5678efgh
ip link set sch5678efgh up
ip route replace 10.210.3.12/32 dev sch5678efgh
sysctl -w net.ipv4.conf.sch5678efgh.rp_filter=1 net.ipv6.conf.sch5678efgh.disable_ipv6=1
ip -4 route show root 10.210.3.0/24 dev eth0
ip route replace 10.210.3.11/32 via 192.168.1.221 dev eth0
ip route replace 10.210.3.13/32 via 192.168.1.223 dev eth0
ip route replace 10.210.3.14/32 via 192.168.1.224 dev eth0`)
}

// MTU 非零：veth 两端同 MTU。
func TestApplyRoutedMTU(t *testing.T) {
	r := &cmdrun.Recorder{}
	stubNoSpc0(r, "4242")
	r.Stub("ip -o link show sch5678efgh", "", noDev("sch5678efgh"))
	n := newNet(r, Options{})
	s := routedSpec(1, 2)
	s.MTU = 9000
	if _, err := n.Apply(context.Background(), ids[1], 4242, s); err != nil {
		t.Fatal(err)
	}
	hasCall(t, r, "ip link add sch5678efgh mtu 9000 type veth peer name scc5678efgh mtu 9000")
}

// Agent 重启后 Recover 再次 Apply：容器里已有 spc0 → 不产生任何网卡增删 / 移动 / 改名，也不重写已正确的对端路由。
func TestApplyIdempotentNoLinkChurn(t *testing.T) {
	churn := []string{"ip link add", "ip link del", " netns ", "name spc0", "route del", "via 192.168.1.22"}

	r := &cmdrun.Recorder{}
	r.Stub("nsenter -t 4242 -n ip -o -4 addr show dev spc0", `3: spc0    inet 10.210.3.12/32 scope global spc0\       valid_lft forever preferred_lft forever`+"\n", nil)
	r.Stub("ip -o link show sch5678efgh", "4: sch5678efgh@if3: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 qdisc noqueue state UP mode DEFAULT group default qlen 1000\n", nil)
	r.Stub("ip -4 route show root 10.210.3.0/24 dev eth0", peerRoutes4of1, nil)
	n := newNet(r, Options{})
	for i := 0; i < 2; i++ {
		if _, err := n.Apply(context.Background(), ids[1], 4242, routedSpec(1, 4)); err != nil {
			t.Fatal(err)
		}
	}
	noCall(t, r, churn...)
	hasCall(t, r, "nsenter -t 4242 -n ip addr replace 10.210.3.12/32 dev spc0")

	r = &cmdrun.Recorder{}
	r.Stub("nsenter -t 777 -n ip -o -4 addr show dev spc0", `5: spc0    inet 10.210.3.11/24 scope global spc0\       valid_lft forever preferred_lft forever`+"\n", nil)
	r.Stub("ip -4 route show default", "default via 192.168.1.1 dev enP7s7 proto dhcp src 192.168.1.50 metric 100 \n", nil)
	r.Stub("ip -o link show scv120", "9: scv120@enp1s0f1np1: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 9000 qdisc noqueue state UP mode DEFAULT group default qlen 1000\n", nil)
	n = newNet(r, Options{})
	for i := 0; i < 2; i++ {
		if _, err := n.Apply(context.Background(), ids[0], 777, macvlanSpec(0, 2, 120)); err != nil {
			t.Fatal(err)
		}
	}
	noCall(t, r, churn...)
	hasCall(t, r, "nsenter -t 777 -n ip addr replace 10.210.3.11/24 dev spc0")
	hasCall(t, r, "ip -4 addr flush dev scv120") // 父接口卫生每次都做
}

// 成员从 4 个变 3 个（rank 2 被释放）：只删 1 条对端路由，其余不动。
func TestApplyPeerRouteDiff(t *testing.T) {
	r := &cmdrun.Recorder{}
	r.Stub("nsenter -t 4242 -n ip -o -4 addr show dev spc0", "3: spc0    inet 10.210.3.12/32 scope global spc0\n", nil)
	r.Stub("ip -4 route show root 10.210.3.0/24 dev eth0", peerRoutes4of1, nil)
	n := newNet(r, Options{})
	if _, err := n.Apply(context.Background(), ids[1], 4242, routedSpec(1, 4, 0, 1, 3)); err != nil {
		t.Fatal(err)
	}
	hasCall(t, r, "ip route del 10.210.3.13/32 dev eth0")
	noCall(t, r, "route replace 10.210.3.11/32 via", "route replace 10.210.3.14/32 via", "route del 10.210.3.11", "route del 10.210.3.14")

	// 换机：rank 3 的宿主机 IP 变了 → 只 replace 这一条
	r = &cmdrun.Recorder{}
	r.Stub("nsenter -t 4242 -n ip -o -4 addr show dev spc0", "3: spc0    inet 10.210.3.12/32 scope global spc0\n", nil)
	r.Stub("ip -4 route show root 10.210.3.0/24 dev eth0", peerRoutes4of1, nil)
	n = newNet(r, Options{})
	s := routedSpec(1, 4)
	s.Members[3].HostIP = "192.168.1.230"
	if _, err := n.Apply(context.Background(), ids[1], 4242, s); err != nil {
		t.Fatal(err)
	}
	hasCall(t, r, "ip route replace 10.210.3.14/32 via 192.168.1.230 dev eth0")
	noCall(t, r, "route del", "via 192.168.1.221", "via 192.168.1.223")
}

// 容器里没有 spc0、宿主机残留 sch<x>（上次半途失败）：先删再建。
func TestApplyRemovesStaleHostLink(t *testing.T) {
	r := &cmdrun.Recorder{}
	stubNoSpc0(r, "4242")
	r.Stub("ip -o link show sch5678efgh", "4: sch5678efgh@scc5678efgh: <BROADCAST,MULTICAST,M-DOWN> mtu 1500 qdisc noop state DOWN mode DEFAULT group default qlen 1000\n", nil)
	n := newNet(r, Options{})
	if _, err := n.Apply(context.Background(), ids[1], 4242, routedSpec(1, 2)); err != nil {
		t.Fatal(err)
	}
	del, add := -1, -1
	for i, c := range r.Calls {
		switch c {
		case "ip link del sch5678efgh":
			del = i
		case "ip link add sch5678efgh mtu 1500 type veth peer name scc5678efgh mtu 1500":
			add = i
		}
	}
	if del < 0 || add < 0 || del > add {
		t.Fatalf("del=%d add=%d\n%s", del, add, r.Lines())
	}
}

// 进不了容器 netns（pid 已不存在）：报错，不建任何网卡。
func TestApplyNsenterFailure(t *testing.T) {
	r := &cmdrun.Recorder{}
	r.Stub("nsenter -t 4242", "", &cmdrun.CmdError{ExitCode: 1, Stderr: "nsenter: cannot open /proc/4242/ns/net: No such file or directory"})
	n := newNet(r, Options{})
	if _, err := n.Apply(context.Background(), ids[1], 4242, routedSpec(1, 2)); err == nil {
		t.Fatal("want error")
	}
	noCall(t, r, "ip link add")
}
