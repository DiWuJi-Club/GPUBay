package iptables_test

import (
	"context"
	"strings"
	"testing"

	"spark-cloud/internal/agent/cmdrun"
	"spark-cloud/internal/agent/netfilter"
	"spark-cloud/internal/agent/netfilter/iptables"
)

const restoreCmd = "iptables-restore --noflush -w"

func fwdRestoreBody(allow ...string) string {
	var b strings.Builder
	b.WriteString("*filter\n:SPARK-IN - [0:0]\n:SPARK-FWD - [0:0]\n")
	for _, r := range iptables.ExpectedInRules()[1:] {
		b.WriteString(r + "\n")
	}
	for _, r := range iptables.ExpectedFwdRules(allow)[1:] {
		b.WriteString(r + "\n")
	}
	return b.String() + "COMMIT\n"
}

func TestEnsureRulesFromScratchUsesSingleRestore(t *testing.T) {
	r := &cmdrun.Recorder{}
	nf := &iptables.Netfilter{Run: r}
	r.Stub("iptables -w -C", "", &cmdrun.CmdError{ExitCode: 1})
	r.Stub("iptables -w -S", "", &cmdrun.CmdError{ExitCode: 1, Stderr: "No chain/target/match by that name."})
	// 非规范输入（主机位）会被规整
	if err := nf.EnsureRules(context.Background(), netfilter.Rules{Bridge: "spark0", AllowCIDRs: []string{"10.0.9.7/24", "10.0.0.53/32"}}); err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"iptables -w -S SPARK-IN",
		"iptables -w -S SPARK-FWD",
		restoreCmd,
		"iptables -w -C INPUT -i spark0 -j SPARK-IN",
		"iptables -w -I INPUT 1 -i spark0 -j SPARK-IN",
		"iptables -w -C DOCKER-USER -i spark0 -j SPARK-FWD",
		"iptables -w -I DOCKER-USER 1 -i spark0 -j SPARK-FWD",
		"sysctl -q -w net.ipv6.conf.spark0.disable_ipv6=1",
	}, "\n")
	if r.Lines() != want {
		t.Fatalf("rules:\n got:\n%s\nwant:\n%s", r.Lines(), want)
	}
	wantIn := "*filter\n:SPARK-IN - [0:0]\n:SPARK-FWD - [0:0]\n" +
		"-A SPARK-IN -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT\n-A SPARK-IN -j DROP\n" +
		"-A SPARK-FWD -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT\n" +
		"-A SPARK-FWD -d 10.0.9.0/24 -j ACCEPT\n-A SPARK-FWD -d 10.0.0.53/32 -j ACCEPT\n" +
		"-A SPARK-FWD -d 10.0.0.0/8 -j DROP\n-A SPARK-FWD -d 172.16.0.0/12 -j DROP\n-A SPARK-FWD -d 192.168.0.0/16 -j DROP\n" +
		"-A SPARK-FWD -d 100.64.0.0/10 -j DROP\n-A SPARK-FWD -d 169.254.0.0/16 -j DROP\n-A SPARK-FWD -d 224.0.0.0/4 -j DROP\n" +
		"-A SPARK-FWD -j ACCEPT\nCOMMIT\n"
	if len(r.Inputs) != 1 || r.Inputs[0] != wantIn {
		t.Fatalf("restore input:\n%q\nwant\n%q", r.Inputs, wantIn)
	}
}

func TestEnsureRulesIsIdempotentWhenAlreadyCorrect(t *testing.T) {
	r := &cmdrun.Recorder{}
	nf := &iptables.Netfilter{Run: r}
	r.Stub("iptables -w -C", "", nil)
	r.Stub("iptables -w -S SPARK-FWD", strings.Join(iptables.ExpectedFwdRules([]string{"10.0.9.0/24"}), "\n")+"\n", nil)
	r.Stub("iptables -w -S SPARK-IN", strings.Join(iptables.ExpectedInRules(), "\n")+"\n", nil)
	if err := nf.EnsureRules(context.Background(), netfilter.Rules{Bridge: "spark0", AllowCIDRs: []string{"10.0.9.0/24"}}); err != nil {
		t.Fatal(err)
	}
	want := "iptables -w -S SPARK-IN\niptables -w -S SPARK-FWD\niptables -w -C INPUT -i spark0 -j SPARK-IN\niptables -w -C DOCKER-USER -i spark0 -j SPARK-FWD\n" +
		"sysctl -q -w net.ipv6.conf.spark0.disable_ipv6=1"
	if r.Lines() != want || len(r.Inputs) != 0 {
		t.Fatalf("idempotent:\n%s", r.Lines())
	}
	if got := iptables.ExpectedFwdRules(nil)[0]; got != "-N SPARK-FWD" {
		t.Fatalf("expected rules header = %q", got)
	}
}

func TestDriftOnNonEmptyChainRestoresAtomically(t *testing.T) {
	r := &cmdrun.Recorder{}
	nf := &iptables.Netfilter{Run: r}
	r.Stub("iptables -w -S SPARK-FWD", "-N SPARK-FWD\n-A SPARK-FWD -j ACCEPT\n", nil)
	r.Stub("iptables -w -S SPARK-IN", strings.Join(iptables.ExpectedInRules(), "\n")+"\n", nil)
	if err := nf.EnsureRules(context.Background(), netfilter.Rules{Bridge: "spark0", AllowCIDRs: []string{"10.0.9.0/24"}}); err != nil {
		t.Fatal(err)
	}
	if len(r.Inputs) != 1 || r.Inputs[0] != fwdRestoreBody("10.0.9.0/24") {
		t.Fatalf("restore input: %q", r.Inputs)
	}
	for _, c := range r.Calls {
		if strings.HasPrefix(c, "iptables -w -F") || strings.HasPrefix(c, "iptables -w -A") {
			t.Fatalf("non-atomic command issued: %s", c)
		}
	}
}

func TestRestoreFailureReturnsErrorAndIssuesNoPartialState(t *testing.T) {
	r := &cmdrun.Recorder{}
	nf := &iptables.Netfilter{Run: r}
	r.Stub("iptables -w -S", "", &cmdrun.CmdError{ExitCode: 1})
	r.Stub(restoreCmd, "", &cmdrun.CmdError{ExitCode: 2, Stderr: "boom"})
	err := nf.EnsureRules(context.Background(), netfilter.Rules{Bridge: "spark0"})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
	for _, c := range r.Calls {
		if strings.HasPrefix(c, "iptables -w -F") || strings.HasPrefix(c, "iptables -w -A") || strings.HasPrefix(c, "iptables -w -I") {
			t.Fatalf("partial state command after failed restore: %s", c)
		}
	}
}

func TestEnsureRulesAttemptsEveryStep(t *testing.T) {
	r := &cmdrun.Recorder{}
	nf := &iptables.Netfilter{Run: r}
	r.Stub("iptables -w -C", "", &cmdrun.CmdError{ExitCode: 1})
	r.Stub("iptables -w -S SPARK-FWD", strings.Join(iptables.ExpectedFwdRules(nil), "\n")+"\n", nil)
	r.Stub("iptables -w -S SPARK-IN", strings.Join(iptables.ExpectedInRules(), "\n")+"\n", nil)
	r.Stub("iptables -w -I DOCKER-USER", "", &cmdrun.CmdError{ExitCode: 1, Stderr: "Chain DOCKER-USER does not exist"})
	err := nf.EnsureRules(context.Background(), netfilter.Rules{Bridge: "spark0"})
	if err == nil || !strings.Contains(err.Error(), "DOCKER-USER") {
		t.Fatalf("err = %v", err)
	}
	for _, w := range []string{"iptables -w -I INPUT 1 -i spark0 -j SPARK-IN", "sysctl -q -w net.ipv6.conf.spark0.disable_ipv6=1"} {
		if !strings.Contains(r.Lines(), w) {
			t.Fatalf("missing %q in:\n%s", w, r.Lines())
		}
	}
}

func TestEnsureRulesRejectsBadCIDRsBeforeAnyCommand(t *testing.T) {
	for _, c := range []string{"fd00::/8", "10.0.0.53", "garbage", "0.0.0.0/0", "::ffff:10.0.0.0/104"} {
		r := &cmdrun.Recorder{}
		nf := &iptables.Netfilter{Run: r}
		if err := nf.EnsureRules(context.Background(), netfilter.Rules{Bridge: "spark0", AllowCIDRs: []string{c}}); err == nil {
			t.Fatalf("%q accepted", c)
		}
		if len(r.Calls) != 0 {
			t.Fatalf("%q: commands issued: %s", c, r.Lines())
		}
	}
}

func TestLimitRejectsNonPositive(t *testing.T) {
	r := &cmdrun.Recorder{}
	nf := &iptables.Netfilter{Run: r}
	for _, m := range []int{0, -5} {
		if err := nf.Limit(context.Background(), "veth1", m); err == nil {
			t.Fatalf("mbps %d accepted", m)
		}
	}
	if len(r.Calls) != 0 {
		t.Fatal("commands issued")
	}
}

func TestLimitSnapshot(t *testing.T) {
	r := &cmdrun.Recorder{}
	nf := &iptables.Netfilter{Run: r}
	r.Stub("tc qdisc del", "", &cmdrun.CmdError{ExitCode: 2, Stderr: "Error: Cannot delete qdisc with handle of zero."})
	if err := nf.Limit(context.Background(), "veth1a2b3c", 200); err != nil {
		t.Fatal(err)
	}
	want := "tc qdisc replace dev veth1a2b3c root tbf rate 200mbit burst 256kbit latency 50ms\n" +
		"tc qdisc del dev veth1a2b3c ingress\n" +
		"tc qdisc add dev veth1a2b3c ingress\n" +
		"tc filter add dev veth1a2b3c parent ffff: protocol all u32 match u32 0 0 police rate 200mbit burst 256k drop flowid :1"
	if r.Lines() != want {
		t.Fatalf("tc:\n%s", r.Lines())
	}
}

func stubLimit(r *cmdrun.Recorder, root, ingress, filter string) {
	r.Stub("tc qdisc show dev veth1 root", root, nil)
	r.Stub("tc qdisc show dev veth1 ingress", ingress, nil)
	r.Stub("tc filter show dev veth1 parent ffff:", filter, nil)
}

const (
	tbfRoot    = "qdisc tbf 8006: root refcnt 21 rate 50Mbit burst 32Kb lat 50ms\n"
	noqueue    = "qdisc noqueue 0: root refcnt 2\n"
	ingressQ   = "qdisc ingress ffff: parent ffff:fff1 ----------------\n"
	policeFilt = "filter protocol all pref 49152 u32 chain 0 fh 800::800 order 2048 key ht 800 bkt 0 flowid :1\n  action order 1:  police 0x1 rate 50Mbit burst 256Kb mtu 2Kb action drop overhead 0b\n"
)

func TestLimitApplied(t *testing.T) {
	for _, c := range []struct {
		name                  string
		root, ingress, filter string
		want                  bool
	}{
		{"both present", tbfRoot, ingressQ, policeFilt, true},
		{"only noqueue", noqueue, "", "", false},
		{"tbf without ingress", tbfRoot, "", "", false},
		{"ingress without police", tbfRoot, ingressQ, "", false},
	} {
		r := &cmdrun.Recorder{}
		nf := &iptables.Netfilter{Run: r}
		stubLimit(r, c.root, c.ingress, c.filter)
		got, err := nf.LimitApplied(context.Background(), "veth1")
		if err != nil || got != c.want {
			t.Fatalf("%s: got %v, %v want %v", c.name, got, err, c.want)
		}
	}
}

func TestLimitAppliedPropagatesTcErrors(t *testing.T) {
	for _, failing := range []string{"tc qdisc show dev veth1 root", "tc qdisc show dev veth1 ingress", "tc filter show dev veth1 parent ffff:"} {
		r := &cmdrun.Recorder{}
		nf := &iptables.Netfilter{Run: r}
		stubLimit(r, tbfRoot, ingressQ, policeFilt)
		r.Stub(failing, "", &cmdrun.CmdError{ExitCode: 2, Stderr: "Cannot find device"})
		if _, err := nf.LimitApplied(context.Background(), "veth1"); err == nil {
			t.Fatalf("%q failure not reported", failing)
		}
	}
}
