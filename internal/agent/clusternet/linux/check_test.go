package linux

import (
	"context"
	"strings"
	"testing"

	"spark-cloud/internal/agent/cmdrun"
	"spark-cloud/internal/clusterspec"
)

// healthyRouted：rank 1 / size 4、pid 4242 的 routed 成员一切正常时各条只读命令的输出。
func healthyRouted(r *cmdrun.Recorder) {
	r.Stub("nsenter -t 4242 -n ip -o -4 addr show dev spc0", "3: spc0    inet 10.210.3.12/32 scope global spc0\n", nil)
	r.Stub("nsenter -t 4242 -n sysctl -n net.ipv6.conf.spc0.disable_ipv6", "1\n", nil)
	r.Stub("nsenter -t 4242 -n ip -4 route show 10.210.3.0/24 dev spc0", "10.210.3.0/24 via 10.210.3.1 \n", nil)
	r.Stub("ip -4 route show 10.210.3.12/32 dev sch5678efgh", "10.210.3.12 scope link \n", nil)
	r.Stub("ip -4 route show root 10.210.3.0/24 dev eth0", peerRoutes4of1, nil)
	r.Stub("iptables -w -S SPARK-CLU", strings.Join(ExpectedCluRules(map[string]clusterspec.Spec{ids[1]: routedSpec(1, 4)}), "\n")+"\n", nil)
	stubJumps(r)
}

// healthyMacvlan：rank 0、pid 777、vlan 120、RDMA 的 macvlan 成员一切正常。
func healthyMacvlan(r *cmdrun.Recorder) {
	r.Stub("nsenter -t 777 -n ip -o -4 addr show dev spc0", "5: spc0    inet 10.210.3.11/24 scope global spc0\n", nil)
	r.Stub("nsenter -t 777 -n sysctl -n net.ipv6.conf.spc0.disable_ipv6", "1\n", nil)
	r.Stub("ip -o link show scv120", "9: scv120@enp1s0f1np1: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 9000 qdisc noqueue state UP mode DEFAULT group default qlen 1000\n", nil)
	r.Stub("ip -o addr show dev", "", nil)
	r.Stub("sysctl -n net.ipv6.conf.", "1\n", nil)
}

func TestCheckDetectsEachProblem(t *testing.T) {
	rspec := routedSpec(1, 4)
	mspec := macvlanSpec(0, 2, 120)
	mspec.RDMA = true
	for _, c := range []struct {
		name     string
		macvlan  bool
		uverbs   string // 容器内 /dev/infiniband 的 uverbs（macvlan）
		stub     func(r *cmdrun.Recorder)
		want     string // err 里应含的描述；"" = ok
		mustCall string
	}{
		{name: "routed ok"},
		{name: "macvlan ok", macvlan: true},
		{name: "missing spc0", stub: func(r *cmdrun.Recorder) {
			r.Stub("nsenter -t 4242 -n ip -o -4 addr show dev spc0", "", noDev("spc0"))
		}, want: "容器内缺少 spc0"},
		{name: "spc0 without address", stub: func(r *cmdrun.Recorder) {
			r.Stub("nsenter -t 4242 -n ip -o -4 addr show dev spc0", "", nil)
		}, want: "spc0 缺少地址 10.210.3.12/32"},
		{name: "ipv6 on spc0", stub: func(r *cmdrun.Recorder) {
			r.Stub("nsenter -t 4242 -n sysctl -n net.ipv6.conf.spc0.disable_ipv6", "0\n", nil)
		}, want: "spc0 未关闭 IPv6"},
		{name: "missing host route", stub: func(r *cmdrun.Recorder) {
			r.Stub("ip -4 route show 10.210.3.12/32 dev sch5678efgh", "", nil)
		}, want: "缺少宿主机路由 10.210.3.12/32 dev sch5678efgh"},
		{name: "missing peer route", stub: func(r *cmdrun.Recorder) {
			r.Stub("ip -4 route show root 10.210.3.0/24 dev eth0", "10.210.3.11 via 192.168.1.221 \n10.210.3.14 via 192.168.1.224 \n", nil)
		}, want: "对端路由与期望不一致"},
		{name: "rules drift", stub: func(r *cmdrun.Recorder) {
			r.Stub("iptables -w -S SPARK-CLU", "-N SPARK-CLU\n-A SPARK-CLU -j DROP\n", nil)
		}, want: "SPARK-CLU 规则与期望不一致"},
		{name: "jump group displaced", stub: func(r *cmdrun.Recorder) {
			r.Stub("iptables -w -S DOCKER-USER", "-N DOCKER-USER\n-A DOCKER-USER -j ACCEPT\n"+strings.TrimPrefix(jumpsInPlace, "-N DOCKER-USER\n"), nil)
		}, want: "DOCKER-USER 跳转组不在链首"},
		{name: "missing vlan", macvlan: true, stub: func(r *cmdrun.Recorder) {
			r.Stub("ip -o link show scv120", "", noDev("scv120"))
		}, want: "VLAN 子接口 scv120 不存在"},
		{name: "vlan down", macvlan: true, stub: func(r *cmdrun.Recorder) {
			r.Stub("ip -o link show scv120", "9: scv120@enp1s0f1np1: <BROADCAST,MULTICAST> mtu 9000 qdisc noqueue state DOWN mode DEFAULT\n", nil)
		}, want: "VLAN 子接口 scv120 未启用"},
		{name: "parent got link-local address", macvlan: true, stub: func(r *cmdrun.Recorder) {
			r.Stub("ip -o addr show dev enp1s0f1np1", `4: enp1s0f1np1    inet 169.254.12.7/16 brd 169.254.255.255 scope link enp1s0f1np1\       valid_lft forever preferred_lft forever`+"\n", nil)
		}, want: "父接口 enp1s0f1np1 出现地址，已清除", mustCall: "ip -4 addr flush dev enp1s0f1np1"},
		{name: "vlan got ipv6 address", macvlan: true, stub: func(r *cmdrun.Recorder) {
			r.Stub("ip -o addr show dev scv120", `9: scv120    inet6 fe80::4db:b8ff:fec5:4910/64 scope link \       valid_lft forever preferred_lft forever`+"\n", nil)
		}, want: "父接口 scv120 出现地址，已清除", mustCall: "sysctl -w net.ipv6.conf.scv120.disable_ipv6=1"},
		{name: "parent ipv6 enabled", macvlan: true, stub: func(r *cmdrun.Recorder) {
			r.Stub("sysctl -n net.ipv6.conf.enp1s0f1np1.disable_ipv6", "0\n", nil)
		}, want: "父接口 enp1s0f1np1 未关闭 IPv6", mustCall: "sysctl -w net.ipv6.conf.enp1s0f1np1.disable_ipv6=1"},
		{name: "uverbs changed", macvlan: true, uverbs: "uverbs0", want: "RDMA 设备编号变化，需重建实例"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := &cmdrun.Recorder{}
			uv := c.uverbs
			if uv == "" {
				uv = "uverbs1"
			}
			n := New(r, Options{SysRoot: sparkSys, ProcRoot: procRoot(t, 777, uv), Log: quiet()})
			id, pid, s := ids[1], 4242, rspec
			if c.macvlan {
				healthyMacvlan(r)
				id, pid, s = ids[0], 777, mspec
			} else {
				healthyRouted(r)
			}
			if c.stub != nil {
				c.stub(r)
			}
			ok, err := n.Check(context.Background(), id, pid, s)
			if c.want == "" {
				if !ok || err != nil {
					t.Fatalf("ok=%v err=%v\n%s", ok, err, r.Lines())
				}
				for _, l := range r.Calls {
					if strings.Contains(l, "flush") || strings.Contains(l, "sysctl -w") || strings.Contains(l, "restore") || strings.Contains(l, " -I ") {
						t.Fatalf("healthy check wrote: %s", l)
					}
				}
				return
			}
			if ok || err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("ok=%v err=%v want %q", ok, err, c.want)
			}
			if c.mustCall != "" {
				hasCall(t, r, c.mustCall)
			}
		})
	}
}

// SyncRules 跑过之后，Check 按其全量输入比对 SPARK-CLU：本实例不在上次的输入里 → 不一致（executor 会重新 Apply + SyncRules）。
func TestCheckUsesLastSyncedRules(t *testing.T) {
	r := &cmdrun.Recorder{}
	healthyRouted(r)
	n := newNet(r, Options{})
	all := map[string]clusterspec.Spec{ids[1]: routedSpec(1, 4), "ins-zzzz0001": twoLocal()["ins-zzzz0001"]}
	r.Stub("iptables -w -S SPARK-CLU", strings.Join(ExpectedCluRules(all), "\n")+"\n", nil)
	if err := n.SyncRules(context.Background(), all); err != nil {
		t.Fatal(err)
	}
	if ok, err := n.Check(context.Background(), ids[1], 4242, routedSpec(1, 4)); !ok {
		t.Fatalf("full set: %v", err)
	}
	if err := n.SyncRules(context.Background(), map[string]clusterspec.Spec{"ins-zzzz0001": all["ins-zzzz0001"]}); err != nil {
		t.Fatal(err)
	}
	if ok, err := n.Check(context.Background(), ids[1], 4242, routedSpec(1, 4)); ok || !strings.Contains(err.Error(), "SPARK-CLU") {
		t.Fatalf("instance missing from last sync: ok=%v err=%v", ok, err)
	}
}
