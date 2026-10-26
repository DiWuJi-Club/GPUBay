package linux

import (
	"context"
	"strings"
	"testing"

	"spark-cloud/internal/agent/cmdrun"
	"spark-cloud/internal/clusterspec"
)

// 本机两个 routed 实例：ins-1234abcd 是 clu-aaaaaaaaaa（10.210.3.0/24，2 台）的 rank 0；
// ins-zzzz0001 是 clu-dddddddddd（10.210.4.0/24，3 台）的 rank 1。另有一个 macvlan 实例（不进 SPARK-CLU）。
func twoLocal() map[string]clusterspec.Spec {
	a := routedSpec(0, 2)
	b := clusterspec.Spec{ClusterID: "clu-dddddddddd", Version: 3, Size: 3, Rank: 1, Mode: clusterspec.ModeRouted, Iface: "eth0", Subnet: "10.210.4.0/24",
		Members: []clusterspec.Member{
			{Rank: 0, InstanceID: "ins-yyyy0000", IP: "10.210.4.11", HostIP: "192.168.1.230"},
			{Rank: 1, InstanceID: "ins-zzzz0001", IP: "10.210.4.12", HostIP: "192.168.1.221"},
			{Rank: 2, InstanceID: "ins-xxxx0002", IP: "10.210.4.13", HostIP: "192.168.1.231"},
		}}
	return map[string]clusterspec.Spec{"ins-zzzz0001": b, ids[0]: a, "ins-mmmm0003": macvlanSpec(0, 2, 0)}
}

// 期望规则的字符串与 221 真机 `iptables -S`（iptables v1.8.10 (nf_tables)）的打印格式逐字一致：
// 用 `-A X -i … -o … -s 10.210.3.11/32 -d 10.210.3.12 -j ACCEPT` 写入后，-S 打印为 -s -d -i -o -j 顺序、地址补 /32，
// conntrack 状态按 RELATED,ESTABLISHED 打印。
const wantCluRules = `-N SPARK-CLU
-A SPARK-CLU -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
-A SPARK-CLU -s 10.210.3.11/32 -d 10.210.3.12/32 -i sch1234abcd -o eth0 -j ACCEPT
-A SPARK-CLU -s 10.210.3.12/32 -d 10.210.3.11/32 -i eth0 -o sch1234abcd -j ACCEPT
-A SPARK-CLU -s 10.210.4.12/32 -d 10.210.4.11/32 -i schzzzz0001 -o eth0 -j ACCEPT
-A SPARK-CLU -s 10.210.4.11/32 -d 10.210.4.12/32 -i eth0 -o schzzzz0001 -j ACCEPT
-A SPARK-CLU -s 10.210.4.12/32 -d 10.210.4.13/32 -i schzzzz0001 -o eth0 -j ACCEPT
-A SPARK-CLU -s 10.210.4.13/32 -d 10.210.4.12/32 -i eth0 -o schzzzz0001 -j ACCEPT
-A SPARK-CLU -j DROP`

func TestExpectedCluRulesPrintOrder(t *testing.T) {
	if got := strings.Join(ExpectedCluRules(twoLocal()), "\n"); got != wantCluRules {
		t.Fatalf("rules:\n%s", got)
	}
	if got := strings.Join(ExpectedCluRules(nil), "\n"); got != "-N SPARK-CLU\n-A SPARK-CLU -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT\n-A SPARK-CLU -j DROP" {
		t.Fatalf("empty:\n%s", got)
	}
	if ChainClu != "SPARK-CLU" {
		t.Fatal(ChainClu)
	}
}

const jumpsInPlace = "-N DOCKER-USER\n-A DOCKER-USER -i spark0 -j SPARK-FWD\n-A DOCKER-USER -i sch+ -j SPARK-CLU\n-A DOCKER-USER -o sch+ -j SPARK-CLU\n"
const inputInPlace = "-P INPUT ACCEPT\n-A INPUT -i spark0 -j SPARK-IN\n-A INPUT -i sch+ -j SPARK-IN\n"

func stubJumps(r *cmdrun.Recorder) {
	r.Stub("iptables -w -S DOCKER-USER", jumpsInPlace, nil)
	r.Stub("iptables -w -S INPUT", inputInPlace, nil)
}

func TestSyncRulesNoopWhenMatching(t *testing.T) {
	r := &cmdrun.Recorder{}
	r.Stub("iptables -w -S SPARK-CLU", wantCluRules+"\n", nil)
	stubJumps(r)
	n := newNet(r, Options{})
	for i := 0; i < 3; i++ {
		if err := n.SyncRules(context.Background(), twoLocal()); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range r.Calls {
		if !strings.HasPrefix(c, "iptables -w -S ") {
			t.Fatalf("write issued: %s\n%s", c, r.Lines())
		}
	}
	if len(r.Inputs) != 0 {
		t.Fatalf("restore: %q", r.Inputs)
	}
}

func TestSyncRulesRebuildsOnDrift(t *testing.T) {
	for _, cur := range []struct {
		out string
		err error
	}{
		{"-N SPARK-CLU\n-A SPARK-CLU -j DROP\n", nil},
		{"", &cmdrun.CmdError{ExitCode: 1, Stderr: "iptables: No chain/target/match by that name."}},
	} {
		r := &cmdrun.Recorder{}
		r.Stub("iptables -w -S SPARK-CLU", cur.out, cur.err)
		stubJumps(r)
		n := newNet(r, Options{})
		if err := n.SyncRules(context.Background(), twoLocal()); err != nil {
			t.Fatal(err)
		}
		want := "*filter\n:SPARK-CLU - [0:0]\n" + strings.Join(strings.Split(wantCluRules, "\n")[1:], "\n") + "\nCOMMIT\n"
		if len(r.Inputs) != 1 || r.Inputs[0] != want {
			t.Fatalf("restore input:\n%q\nwant\n%q", r.Inputs, want)
		}
		hasCall(t, r, "iptables-restore --noflush -w")
		for _, c := range r.Calls {
			if strings.HasPrefix(c, "iptables -w -F") || strings.HasPrefix(c, "iptables -w -A") {
				t.Fatalf("non-atomic: %s", c)
			}
		}
	}
}

// SyncRules 同时保证跳转组在链首（缺了就补）；坏 Spec 被跳过，不连累别的实例。
func TestSyncRulesEnsuresJumpsAndSkipsBadSpec(t *testing.T) {
	r := &cmdrun.Recorder{}
	r.Stub("iptables -w -S SPARK-CLU", wantCluRules+"\n", nil)
	r.Stub("iptables -w -S DOCKER-USER", "-N DOCKER-USER\n-A DOCKER-USER -i spark0 -j SPARK-FWD\n", nil)
	r.Stub("iptables -w -S INPUT", inputInPlace, nil)
	n := newNet(r, Options{})
	m := twoLocal()
	bad := routedSpec(1, 2)
	bad.Subnet = "garbage"
	m["ins-bad00000"] = bad
	if err := n.SyncRules(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if len(r.Inputs) != 0 {
		t.Fatalf("bad spec changed rules: %q", r.Inputs)
	}
	hasCall(t, r, "iptables -w -I DOCKER-USER 2 -i sch+ -j SPARK-CLU")
	hasCall(t, r, "iptables -w -I DOCKER-USER 3 -o sch+ -j SPARK-CLU")

	r = &cmdrun.Recorder{}
	r.Stub("iptables -w -S SPARK-CLU", wantCluRules+"\n", nil)
	r.Stub("iptables-restore", "", &cmdrun.CmdError{ExitCode: 2, Stderr: "boom"})
	r.Stub("iptables -w -S DOCKER-USER", "", &cmdrun.CmdError{ExitCode: 1, Stderr: "No chain"})
	n = newNet(r, Options{})
	if err := n.SyncRules(context.Background(), twoLocal()); err == nil || !strings.HasPrefix(err.Error(), clusterspec.ErrPrefix) {
		t.Fatalf("err = %v", err)
	}
}
