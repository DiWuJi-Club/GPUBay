//go:build linux

package linux

// 真机测试（需要 root）：SPARK_CLUSTERNET_REAL=1 时在一组临时 network namespace 里跑真实的 ip / nsenter / iptables，
// 不碰宿主机自己的网络与 iptables（所有命令经 `ip netns exec <host-ns>`；容器 = 另一个 netns 里的 sleep 进程）。
//
//	p13t2-h1 ──veth p13l1/p13l2── p13t2-h2      routed 的「机房内网」（192.168.77.0/24）
//	p13t2-h1 ──veth p13m1/p13m2── p13t2-h2      macvlan 的「CX-7 直连 / trunk」（vlan 120）
//	c1 / c3 是 h1 上的两个集群容器，c2 / c4 是 h2 上的
//
// 运行：GOOS=linux GOARCH=arm64 go test -c -o /tmp/cn.test ./internal/agent/clusternet/linux/ && sudo SPARK_CLUSTERNET_REAL=1 /tmp/cn.test -test.run Real -test.v

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"spark-cloud/internal/agent/cmdrun"
	"spark-cloud/internal/agent/netfilter"
	"spark-cloud/internal/agent/netfilter/iptables"
	"spark-cloud/internal/clusterspec"
)

// nsRunner 把每条命令放进 `ip netns exec <ns>` 执行，并记录原命令行（快照断言用）。
type nsRunner struct {
	ns    string
	mu    sync.Mutex
	calls []string
}

func (r *nsRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return r.RunInput(ctx, nil, name, args...)
}

func (r *nsRunner) RunInput(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	r.calls = append(r.calls, cmdrun.Line(name, args...))
	r.mu.Unlock()
	return cmdrun.Exec{}.RunInput(ctx, stdin, "ip", append([]string{"netns", "exec", r.ns, name}, args...)...)
}

func (r *nsRunner) reset() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.calls
	r.calls = nil
	return c
}

func realGate(t *testing.T) {
	if os.Getenv("SPARK_CLUSTERNET_REAL") != "1" {
		t.Skip("set SPARK_CLUSTERNET_REAL=1 (root, Linux) to run against the real kernel")
	}
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
}

func sh(t *testing.T, args ...string) string {
	t.Helper()
	out, err := cmdrun.Exec{}.Run(context.Background(), args[0], args[1:]...)
	if err != nil {
		t.Fatalf("%s: %v", strings.Join(args, " "), err)
	}
	return string(out)
}

func shOK(args ...string) bool {
	_, err := cmdrun.Exec{}.Run(context.Background(), args[0], args[1:]...)
	return err == nil
}

func inNS(ns string, args ...string) []string {
	return append([]string{"ip", "netns", "exec", ns}, args...)
}

// TestRealProbe：在本机（root netns）只读探测，打印结果；221 上应看到 wlP9s9（无线、有 IPv4）与 enP7s7（未连接）。
func TestRealProbe(t *testing.T) {
	realGate(t)
	n := New(cmdrun.Exec{}, Options{Log: quiet()})
	info, err := n.Probe(context.Background(), os.Getenv("SPARK_LAN_IP"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range info.Ifaces {
		t.Logf("%+v", f)
	}
	t.Logf("lan_iface=%q", info.LanIface)
	if len(info.Ifaces) == 0 {
		t.Fatal("no physical interfaces found")
	}
	for _, f := range info.Ifaces {
		if excluded(f.Name) {
			t.Fatalf("excluded iface reported: %s", f.Name)
		}
	}
}

func TestRealNetns(t *testing.T) {
	realGate(t)
	ctx := context.Background()
	nss := []string{"p13t2-h1", "p13t2-h2", "p13t2-c1", "p13t2-c2", "p13t2-c3", "p13t2-c4"}
	var sleepers []*exec.Cmd
	cleanup := func() {
		for _, c := range sleepers {
			_ = c.Process.Kill()
			_, _ = c.Process.Wait()
		}
		for _, ns := range nss {
			_ = exec.Command("ip", "netns", "del", ns).Run()
		}
		_ = exec.Command("ip", "link", "del", "p13l1").Run()
		_ = exec.Command("ip", "link", "del", "p13m1").Run()
	}
	cleanup()
	t.Cleanup(cleanup)
	for _, ns := range nss {
		sh(t, "ip", "netns", "add", ns)
		sh(t, inNS(ns, "ip", "link", "set", "lo", "up")...)
	}
	for _, pair := range [][2]string{{"p13l1", "p13l2"}, {"p13m1", "p13m2"}} {
		sh(t, "ip", "link", "add", pair[0], "type", "veth", "peer", "name", pair[1])
		sh(t, "ip", "link", "set", pair[0], "netns", "p13t2-h1")
		sh(t, "ip", "link", "set", pair[1], "netns", "p13t2-h2")
	}
	for i, h := range []string{"p13t2-h1", "p13t2-h2"} {
		l, m := "p13l"+strconv.Itoa(i+1), "p13m"+strconv.Itoa(i+1)
		sh(t, inNS(h, "ip", "addr", "add", "192.168.77."+strconv.Itoa(i+1)+"/24", "dev", l)...)
		sh(t, inNS(h, "ip", "link", "set", l, "up")...)
		sh(t, inNS(h, "ip", "link", "set", m, "up")...)
		sh(t, inNS(h, "sysctl", "-q", "-w", "net.ipv4.ip_forward=1")...)
		sh(t, inNS(h, "ip", "link", "add", "spark0", "type", "dummy")...) // EnsureRules 要关它的 IPv6
		// 模拟 docker：FORWARD 默认 DROP，第一条跳 DOCKER-USER
		sh(t, inNS(h, "iptables", "-w", "-N", "DOCKER-USER")...)
		sh(t, inNS(h, "iptables", "-w", "-P", "FORWARD", "DROP")...)
		sh(t, inNS(h, "iptables", "-w", "-A", "FORWARD", "-j", "DOCKER-USER")...)
		sh(t, inNS(h, "iptables", "-w", "-A", "DOCKER-USER", "-j", "RETURN")...)
	}
	pids := map[string]int{}
	for _, c := range []string{"p13t2-c1", "p13t2-c2", "p13t2-c3", "p13t2-c4"} {
		cmd := exec.Command("ip", "netns", "exec", c, "sleep", "600")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		sleepers = append(sleepers, cmd)
		pids[c] = cmd.Process.Pid // ip netns exec 在 setns 后 execvp，pid 不变
	}
	time.Sleep(200 * time.Millisecond)

	r1, r2 := &nsRunner{ns: "p13t2-h1"}, &nsRunner{ns: "p13t2-h2"}
	n1, n2 := New(r1, Options{Log: quiet()}), New(r2, Options{Log: quiet()})
	nf1, nf2 := &iptables.Netfilter{Run: r1, Log: quiet()}, &iptables.Netfilter{Run: r2, Log: quiet()}
	for _, nf := range []*iptables.Netfilter{nf1, nf2} {
		if err := nf.EnsureRules(ctx, netfilter.Rules{Bridge: "spark0"}); err != nil {
			t.Fatal(err)
		}
	}

	// ---- routed ----
	idR := []string{"ins-real0000", "ins-real0001"}
	routed := func(rank int, iface string) clusterspec.Spec {
		return clusterspec.Spec{ClusterID: "clu-realrouted", Version: 1, Size: 2, Rank: rank, Mode: clusterspec.ModeRouted, Iface: iface, Subnet: "10.210.77.0/24",
			Members: []clusterspec.Member{
				{Rank: 0, InstanceID: idR[0], IP: "10.210.77.11", HostIP: "192.168.77.1"},
				{Rank: 1, InstanceID: idR[1], IP: "10.210.77.12", HostIP: "192.168.77.2"},
			}}
	}
	s1, s2 := routed(0, "p13l1"), routed(1, "p13l2")
	apply := func(n *Net, id string, pid int, s clusterspec.Spec) {
		t.Helper()
		if _, err := n.Apply(ctx, id, pid, s); err != nil {
			t.Fatalf("apply %s: %v", id, err)
		}
	}
	apply(n1, idR[0], pids["p13t2-c1"], s1)
	apply(n2, idR[1], pids["p13t2-c2"], s2)
	if err := n1.SyncRules(ctx, map[string]clusterspec.Spec{idR[0]: s1}); err != nil {
		t.Fatal(err)
	}
	if err := n2.SyncRules(ctx, map[string]clusterspec.Spec{idR[1]: s2}); err != nil {
		t.Fatal(err)
	}
	ping := func(from, to string) bool {
		return shOK(inNS(from, "ping", "-c", "1", "-W", "2", to)...)
	}
	if !ping("p13t2-c1", "10.210.77.12") || !ping("p13t2-c2", "10.210.77.11") {
		t.Logf("h1 SPARK-CLU:\n%s", sh(t, inNS("p13t2-h1", "iptables", "-w", "-S", "SPARK-CLU")...))
		t.Fatal("routed members cannot reach each other")
	}
	if ping("p13t2-c1", "10.210.77.1") {
		t.Fatal("container reached its host gateway (INPUT -i sch+ must drop)")
	}
	if ping("p13t2-c1", "192.168.77.2") {
		t.Fatal("container reached the peer host")
	}
	// iptables -S 的真实打印与期望逐字相同
	if got := strings.TrimSpace(sh(t, inNS("p13t2-h1", "iptables", "-w", "-S", "SPARK-CLU")...)); got != strings.Join(ExpectedCluRules(map[string]clusterspec.Spec{idR[0]: s1}), "\n") {
		t.Fatalf("SPARK-CLU real print:\n%s", got)
	}
	t.Logf("h1 DOCKER-USER:\n%s", sh(t, inNS("p13t2-h1", "iptables", "-w", "-S", "DOCKER-USER")...))
	if ok, err := n1.Check(ctx, idR[0], pids["p13t2-c1"], s1); !ok {
		t.Fatalf("check h1: %v", err)
	}

	// 再 Apply / SyncRules / EnsureRules：没有任何网卡增删、没有规则写入
	r1.reset()
	apply(n1, idR[0], pids["p13t2-c1"], s1)
	if err := n1.SyncRules(ctx, map[string]clusterspec.Spec{idR[0]: s1}); err != nil {
		t.Fatal(err)
	}
	if err := nf1.EnsureRules(ctx, netfilter.Rules{Bridge: "spark0"}); err != nil {
		t.Fatal(err)
	}
	for _, c := range r1.reset() {
		for _, bad := range []string{"ip link add", "ip link del", " netns ", "name spc0", "iptables-restore", "iptables -w -I", "iptables -w -D", "iptables -w -N", "route del"} {
			if strings.Contains(c, bad) {
				t.Fatalf("re-apply churn: %s", c)
			}
		}
	}
	if !ping("p13t2-c1", "10.210.77.12") {
		t.Fatal("connectivity lost after re-apply")
	}

	// Teardown：对端路由与 veth 消失，容器 spc0 随之消失；Check 报缺 spc0；再 Apply 恢复
	if err := n1.Teardown(ctx, idR[0], s1, nil); err != nil {
		t.Fatal(err)
	}
	if out := sh(t, inNS("p13t2-h1", "ip", "-4", "route", "show", "root", "10.210.77.0/24")...); strings.TrimSpace(out) != "" {
		t.Fatalf("routes left: %s", out)
	}
	if ok, err := n1.Check(ctx, idR[0], pids["p13t2-c1"], s1); ok || !strings.Contains(err.Error(), "缺少 spc0") {
		t.Fatalf("check after teardown: %v %v", ok, err)
	}
	apply(n1, idR[0], pids["p13t2-c1"], s1)
	if !ping("p13t2-c1", "10.210.77.12") {
		t.Fatal("connectivity not restored")
	}

	// ---- macvlan over VLAN 120 ----
	idM := []string{"ins-realm000", "ins-realm001"}
	mv := func(rank int, iface string) clusterspec.Spec {
		return clusterspec.Spec{ClusterID: "clu-realmacvla", Version: 1, Size: 2, Rank: rank, Mode: clusterspec.ModeMacvlan, Iface: iface, VLAN: 120, Subnet: "10.210.78.0/24",
			Members: []clusterspec.Member{{Rank: 0, InstanceID: idM[0], IP: "10.210.78.11"}, {Rank: 1, InstanceID: idM[1], IP: "10.210.78.12"}}}
	}
	m1, m2 := mv(0, "p13m1"), mv(1, "p13m2")
	apply(n1, idM[0], pids["p13t2-c3"], m1)
	apply(n2, idM[1], pids["p13t2-c4"], m2)
	if !ping("p13t2-c3", "10.210.78.12") {
		t.Fatal("macvlan members cannot reach each other")
	}
	if ok, err := n1.Check(ctx, idM[0], pids["p13t2-c3"], m1); !ok {
		t.Fatalf("check macvlan: %v", err)
	}
	r1.reset()
	apply(n1, idM[0], pids["p13t2-c3"], m1)
	for _, c := range r1.reset() {
		for _, bad := range []string{"ip link add", "ip link del", " netns ", "name spc0"} {
			if strings.Contains(c, bad) {
				t.Fatalf("macvlan re-apply churn: %s", c)
			}
		}
	}
	// 父接口被加了地址 → Check 清除并报告
	sh(t, inNS("p13t2-h1", "ip", "addr", "add", "169.254.9.9/16", "dev", "p13m1")...)
	if ok, err := n1.Check(ctx, idM[0], pids["p13t2-c3"], m1); ok || !strings.Contains(err.Error(), "父接口 p13m1 出现地址，已清除") {
		t.Fatalf("parent hygiene: %v %v", ok, err)
	}
	if out := sh(t, inNS("p13t2-h1", "ip", "-o", "addr", "show", "dev", "p13m1")...); strings.TrimSpace(out) != "" {
		t.Fatalf("parent still has address: %s", out)
	}
	if ok, err := n1.Check(ctx, idM[0], pids["p13t2-c3"], m1); !ok {
		t.Fatalf("check after hygiene: %v", err)
	}
	// Teardown 且本机没有别的 VLAN 120 使用者 → scv120 删除，容器内 spc0（其上的 macvlan）随之消失
	if err := n1.Teardown(ctx, idM[0], m1, []clusterspec.Spec{s1}); err != nil {
		t.Fatal(err)
	}
	if shOK(inNS("p13t2-h1", "ip", "link", "show", "scv120")...) {
		t.Fatal("scv120 left")
	}
	if shOK("nsenter", "-t", strconv.Itoa(pids["p13t2-c3"]), "-n", "ip", "link", "show", "spc0") {
		t.Fatal("macvlan spc0 survived VLAN removal")
	}
}
