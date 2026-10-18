package iptables_test

import (
	"context"
	"strings"
	"testing"

	"spark-cloud/internal/agent/cmdrun"
	"spark-cloud/internal/agent/netfilter"
	"spark-cloud/internal/agent/netfilter/iptables"
)

func TestEnsureRulesFromScratch(t *testing.T) {
	r := &cmdrun.Recorder{}
	nf := &iptables.Netfilter{Run: r}
	r.Stub("iptables -w -C", "", &cmdrun.CmdError{ExitCode: 1})
	r.Stub("iptables -w -S SPARK-FWD", "-N SPARK-FWD\n", nil)
	r.Stub("iptables -w -S SPARK-IN", "-N SPARK-IN\n", nil)
	if err := nf.EnsureRules(context.Background(), netfilter.Rules{Bridge: "spark0", AllowCIDRs: []string{"10.0.9.0/24", "10.0.0.53/32"}}); err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"iptables -w -N SPARK-FWD",
		"iptables -w -N SPARK-IN",
		"iptables -w -C DOCKER-USER -i spark0 -j SPARK-FWD",
		"iptables -w -I DOCKER-USER 1 -i spark0 -j SPARK-FWD",
		"iptables -w -C INPUT -i spark0 -j SPARK-IN",
		"iptables -w -I INPUT 1 -i spark0 -j SPARK-IN",
		"iptables -w -S SPARK-FWD",
		"iptables -w -F SPARK-FWD",
		"iptables -w -A SPARK-FWD -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT",
		"iptables -w -A SPARK-FWD -d 10.0.9.0/24 -j ACCEPT",
		"iptables -w -A SPARK-FWD -d 10.0.0.53/32 -j ACCEPT",
		"iptables -w -A SPARK-FWD -d 10.0.0.0/8 -j DROP",
		"iptables -w -A SPARK-FWD -d 172.16.0.0/12 -j DROP",
		"iptables -w -A SPARK-FWD -d 192.168.0.0/16 -j DROP",
		"iptables -w -A SPARK-FWD -d 100.64.0.0/10 -j DROP",
		"iptables -w -A SPARK-FWD -d 169.254.0.0/16 -j DROP",
		"iptables -w -A SPARK-FWD -d 224.0.0.0/4 -j DROP",
		"iptables -w -A SPARK-FWD -j ACCEPT",
		"iptables -w -S SPARK-IN",
		"iptables -w -F SPARK-IN",
		"iptables -w -A SPARK-IN -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT",
		"iptables -w -A SPARK-IN -j DROP",
		"sysctl -q -w net.ipv6.conf.spark0.disable_ipv6=1",
	}, "\n")
	if r.Lines() != want {
		t.Fatalf("rules:\n got:\n%s\nwant:\n%s", r.Lines(), want)
	}
}

func TestEnsureRulesIsIdempotentWhenAlreadyCorrect(t *testing.T) {
	r := &cmdrun.Recorder{}
	nf := &iptables.Netfilter{Run: r}
	r.Stub("iptables -w -N", "", &cmdrun.CmdError{ExitCode: 1, Stderr: "Chain already exists"})
	r.Stub("iptables -w -C", "", nil)
	r.Stub("iptables -w -S SPARK-FWD", strings.Join(iptables.ExpectedFwdRules([]string{"10.0.9.0/24"}), "\n")+"\n", nil)
	r.Stub("iptables -w -S SPARK-IN", strings.Join(iptables.ExpectedInRules(), "\n")+"\n", nil)
	if err := nf.EnsureRules(context.Background(), netfilter.Rules{Bridge: "spark0", AllowCIDRs: []string{"10.0.9.0/24"}}); err != nil {
		t.Fatal(err)
	}
	want := "iptables -w -N SPARK-FWD\niptables -w -N SPARK-IN\niptables -w -C DOCKER-USER -i spark0 -j SPARK-FWD\niptables -w -C INPUT -i spark0 -j SPARK-IN\n" +
		"iptables -w -S SPARK-FWD\niptables -w -S SPARK-IN\nsysctl -q -w net.ipv6.conf.spark0.disable_ipv6=1"
	if r.Lines() != want {
		t.Fatalf("idempotent:\n%s", r.Lines())
	}
	if got := iptables.ExpectedFwdRules(nil)[0]; got != "-N SPARK-FWD" {
		t.Fatalf("expected rules header = %q", got)
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
		"tc filter add dev veth1a2b3c parent ffff: protocol ip u32 match u32 0 0 police rate 200mbit burst 256k drop flowid :1"
	if r.Lines() != want {
		t.Fatalf("tc:\n%s", r.Lines())
	}
}
