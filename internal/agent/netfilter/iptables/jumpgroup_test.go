package iptables_test

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"spark-cloud/internal/agent/cmdrun"
	"spark-cloud/internal/agent/netfilter"
	"spark-cloud/internal/agent/netfilter/iptables"
)

// fakeTables 是有状态的 iptables 假实现：-S / -N / -A / -I / -D 与 iptables-restore --noflush（声明链即清空）。
// 每次写操作后检查 invariant（例如「DOCKER-USER 里始终有 SPARK-FWD 跳转」），模拟真机按位插入 / 删除的语义。
type fakeTables struct {
	t      *testing.T
	chains map[string][]string // 链 → 规则（不含 "-A <chain> " 前缀）
	order  []string
	writes []string
	check  func(f *fakeTables)
}

func newFakeTables(t *testing.T) *fakeTables {
	f := &fakeTables{t: t, chains: map[string][]string{}}
	f.newChain("INPUT")
	f.newChain("DOCKER-USER")
	return f
}

func (f *fakeTables) newChain(c string) {
	if _, ok := f.chains[c]; !ok {
		f.chains[c] = nil
		f.order = append(f.order, c)
	}
}

func (f *fakeTables) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return f.RunInput(ctx, nil, name, args...)
}

func (f *fakeTables) RunInput(_ context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	line := cmdrun.Line(name, args...)
	noChain := &cmdrun.CmdError{Cmd: line, ExitCode: 1, Stderr: "iptables: No chain/target/match by that name."}
	switch name {
	case "sysctl":
		return nil, nil
	case "iptables-restore":
		f.writes = append(f.writes, line)
		for _, l := range strings.Split(string(stdin), "\n") {
			switch {
			case strings.HasPrefix(l, ":"):
				c := strings.Fields(l[1:])[0]
				f.newChain(c)
				f.chains[c] = nil
			case strings.HasPrefix(l, "-A "):
				fs := strings.SplitN(l, " ", 3)
				f.chains[fs[1]] = append(f.chains[fs[1]], fs[2])
			}
		}
		f.after()
		return nil, nil
	case "iptables":
	default:
		f.t.Fatalf("unexpected command %s", line)
	}
	if len(args) < 2 || args[0] != "-w" {
		f.t.Fatalf("iptables without -w: %s", line)
	}
	op, chain, rest := args[1], args[2], args[3:]
	rules, ok := f.chains[chain]
	if op != "-N" && !ok {
		return nil, noChain
	}
	switch op {
	case "-S":
		var b strings.Builder
		if chain == "INPUT" {
			b.WriteString("-P INPUT ACCEPT\n")
		} else {
			b.WriteString("-N " + chain + "\n")
		}
		for _, r := range rules {
			b.WriteString("-A " + chain + " " + r + "\n")
		}
		return []byte(b.String()), nil
	case "-N":
		if ok {
			return nil, &cmdrun.CmdError{Cmd: line, ExitCode: 1, Stderr: "iptables: Chain already exists."}
		}
		f.newChain(chain)
	case "-A":
		f.chains[chain] = append(rules, strings.Join(rest, " "))
	case "-I":
		n, err := strconv.Atoi(rest[0])
		if err != nil || n < 1 || n > len(rules)+1 {
			return nil, &cmdrun.CmdError{Cmd: line, ExitCode: 1, Stderr: "iptables: Index of insertion too big."}
		}
		f.chains[chain] = slices.Insert(slices.Clone(rules), n-1, strings.Join(rest[1:], " "))
	case "-D":
		n, err := strconv.Atoi(rest[0])
		if err != nil || n < 1 || n > len(rules) {
			return nil, &cmdrun.CmdError{Cmd: line, ExitCode: 1, Stderr: "iptables: Index of deletion too big."}
		}
		f.chains[chain] = slices.Delete(slices.Clone(rules), n-1, n)
	default:
		f.t.Fatalf("unexpected iptables op %s", line)
	}
	f.writes = append(f.writes, line)
	f.after()
	return nil, nil
}

func (f *fakeTables) after() {
	if f.check != nil {
		f.check(f)
	}
}

func (f *fakeTables) head(chain string, n int) []string {
	r := f.chains[chain]
	if len(r) > n {
		r = r[:n]
	}
	return r
}

func joinJumps(j [][]string) []string {
	out := make([]string, len(j))
	for i, x := range j {
		out[i] = strings.Join(x, " ")
	}
	return out
}

func TestJumpGroupVars(t *testing.T) {
	if fmt.Sprint(iptables.DockerUserJumps) != "[[-i spark0 -j SPARK-FWD] [-i sch+ -j SPARK-CLU] [-o sch+ -j SPARK-CLU]]" ||
		fmt.Sprint(iptables.InputJumps) != "[[-i spark0 -j SPARK-IN] [-i sch+ -j SPARK-IN]]" || iptables.ChainClu != "SPARK-CLU" {
		t.Fatalf("jump groups = %v %v", iptables.DockerUserJumps, iptables.InputJumps)
	}
}

// 跳转组已按序在链首：只读不写（链首之后的其它规则不动）。
func TestJumpGroupNoopWhenInPlace(t *testing.T) {
	r := &cmdrun.Recorder{}
	nf := &iptables.Netfilter{Run: r}
	syncedChains(r)
	r.Stub("iptables -w -S INPUT", realInput+"-A INPUT -j ufw-before-input\n", nil)
	r.Stub("iptables -w -S DOCKER-USER", realDockerUser+"-A DOCKER-USER -j RETURN\n", nil)
	if err := nf.EnsureRules(context.Background(), netfilter.Rules{Bridge: "spark0"}); err != nil {
		t.Fatal(err)
	}
	for _, c := range r.Calls {
		if !strings.HasPrefix(c, "iptables -w -S ") && !strings.HasPrefix(c, "sysctl ") {
			t.Fatalf("write issued: %s\n%s", c, r.Lines())
		}
	}
}

// 存量机器：DOCKER-USER 只有旧的 SPARK-FWD 跳转（计划 13 之前的 Agent）、前面还被别的工具插了 ACCEPT。
// 结果：前 3 条按序为跳转组；调用序列先 -I 后 -D；任何时刻 SPARK-FWD 跳转都在。
func TestJumpGroupCoexists(t *testing.T) {
	f := newFakeTables(t)
	f.chains["DOCKER-USER"] = []string{"-j ACCEPT", "-i spark0 -j SPARK-FWD", "-j RETURN"}
	f.chains["INPUT"] = []string{"-i spark0 -j SPARK-IN"}
	f.check = func(f *fakeTables) {
		if !slices.Contains(f.chains["DOCKER-USER"], "-i spark0 -j SPARK-FWD") || !slices.Contains(f.chains["INPUT"], "-i spark0 -j SPARK-IN") {
			t.Fatalf("isolation jump missing after %s: %v", f.writes[len(f.writes)-1], f.chains)
		}
	}
	nf := &iptables.Netfilter{Run: f}
	if err := nf.EnsureRules(context.Background(), netfilter.Rules{Bridge: "spark0"}); err != nil {
		t.Fatal(err)
	}
	if got := f.head("DOCKER-USER", 3); !slices.Equal(got, joinJumps(iptables.DockerUserJumps)) {
		t.Fatalf("DOCKER-USER = %v", f.chains["DOCKER-USER"])
	}
	if got := f.chains["DOCKER-USER"]; !slices.Equal(got, append(joinJumps(iptables.DockerUserJumps), "-j ACCEPT", "-j RETURN")) {
		t.Fatalf("DOCKER-USER duplicates not removed: %v", got)
	}
	if got := f.chains["INPUT"]; !slices.Equal(got, joinJumps(iptables.InputJumps)) {
		t.Fatalf("INPUT = %v", got)
	}
	var du []string
	for _, w := range f.writes {
		if strings.Contains(w, " DOCKER-USER ") {
			du = append(du, w)
		}
	}
	want := []string{
		"iptables -w -I DOCKER-USER 1 -i spark0 -j SPARK-FWD",
		"iptables -w -I DOCKER-USER 2 -i sch+ -j SPARK-CLU",
		"iptables -w -I DOCKER-USER 3 -o sch+ -j SPARK-CLU",
		"iptables -w -D DOCKER-USER 5",
	}
	if !slices.Equal(du, want) {
		t.Fatalf("DOCKER-USER writes:\n%s", strings.Join(du, "\n"))
	}
	if got := f.chains["SPARK-CLU"]; !slices.Equal(got, []string{"-j DROP"}) {
		t.Fatalf("SPARK-CLU = %v", got)
	}
}

// 防「SPARK-FWD / SPARK-CLU / SPARK-IN 互抢第 1 位」：连续三次 EnsureRules 只有第一次有写操作。
func TestJumpGroupRepeatedRunsStable(t *testing.T) {
	for _, start := range [][]string{
		nil,
		{"-i spark0 -j SPARK-FWD"},
		{"-o sch+ -j SPARK-CLU", "-i sch+ -j SPARK-CLU", "-i spark0 -j SPARK-FWD"},
		{"-i sch+ -j SPARK-CLU", "-j ACCEPT", "-i spark0 -j SPARK-FWD", "-i spark0 -j SPARK-FWD"},
	} {
		f := newFakeTables(t)
		f.chains["DOCKER-USER"] = slices.Clone(start)
		nf := &iptables.Netfilter{Run: f}
		var counts []int
		for i := 0; i < 3; i++ {
			before := len(f.writes)
			if err := nf.EnsureRules(context.Background(), netfilter.Rules{Bridge: "spark0"}); err != nil {
				t.Fatal(err)
			}
			counts = append(counts, len(f.writes)-before)
		}
		if counts[0] == 0 || counts[1] != 0 || counts[2] != 0 {
			t.Fatalf("start %v: writes per run = %v\n%s", start, counts, strings.Join(f.writes, "\n"))
		}
		if got := f.head("DOCKER-USER", 3); !slices.Equal(got, joinJumps(iptables.DockerUserJumps)) {
			t.Fatalf("start %v: DOCKER-USER = %v", start, f.chains["DOCKER-USER"])
		}
		for _, j := range joinJumps(iptables.DockerUserJumps) {
			n := 0
			for _, r := range f.chains["DOCKER-USER"] {
				if r == j {
					n++
				}
			}
			if n != 1 {
				t.Fatalf("start %v: %q appears %d times: %v", start, j, n, f.chains["DOCKER-USER"])
			}
		}
	}
}

// SPARK-CLU 不存在 → -N + -j DROP；已存在（有集群规则）→ 内容不动。
func TestEnsureRulesCreatesCluChain(t *testing.T) {
	f := newFakeTables(t)
	nf := &iptables.Netfilter{Run: f}
	if err := nf.EnsureRules(context.Background(), netfilter.Rules{Bridge: "spark0"}); err != nil {
		t.Fatal(err)
	}
	if got := f.chains["SPARK-CLU"]; !slices.Equal(got, []string{"-j DROP"}) {
		t.Fatalf("SPARK-CLU = %v", got)
	}
	custom := []string{"-m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT", "-s 10.210.3.11/32 -d 10.210.3.12/32 -i sch1234abcd -o eth0 -j ACCEPT", "-j DROP"}
	f.chains["SPARK-CLU"] = slices.Clone(custom)
	if err := nf.EnsureRules(context.Background(), netfilter.Rules{Bridge: "spark0"}); err != nil {
		t.Fatal(err)
	}
	if got := f.chains["SPARK-CLU"]; !slices.Equal(got, custom) {
		t.Fatalf("existing SPARK-CLU touched: %v", got)
	}
}

// SPARK-CLU 建不出来：SPARK-FWD / SPARK-IN 的隔离跳转照样保证，指向 SPARK-CLU 的跳转不插，错误上报。
func TestEnsureRulesCluFailureKeepsIsolationJumps(t *testing.T) {
	r := &cmdrun.Recorder{}
	nf := &iptables.Netfilter{Run: r}
	syncedChains(r)
	r.Stub("iptables -w -S SPARK-CLU", "", &cmdrun.CmdError{ExitCode: 1, Stderr: "iptables: No chain/target/match by that name."})
	r.Stub("iptables -w -N SPARK-CLU", "", &cmdrun.CmdError{ExitCode: 4, Stderr: "boom"})
	r.Stub("iptables -w -S INPUT", "-P INPUT ACCEPT\n", nil)
	r.Stub("iptables -w -S DOCKER-USER", "-N DOCKER-USER\n", nil)
	err := nf.EnsureRules(context.Background(), netfilter.Rules{Bridge: "spark0"})
	if err == nil || !strings.Contains(err.Error(), "SPARK-CLU") {
		t.Fatalf("err = %v", err)
	}
	l := r.Lines()
	if !strings.Contains(l, "iptables -w -I DOCKER-USER 1 -i spark0 -j SPARK-FWD") || !strings.Contains(l, "iptables -w -I INPUT 2 -i sch+ -j SPARK-IN") ||
		strings.Contains(l, "-j SPARK-CLU") && strings.Contains(l, "-I DOCKER-USER 2") {
		t.Fatalf("calls:\n%s", l)
	}
}

// EnsureJumpGroups（clusternet 的 SyncRules 也调用）：只维护跳转组，不碰链内容。
func TestEnsureJumpGroupsOnly(t *testing.T) {
	f := newFakeTables(t)
	f.newChain("SPARK-CLU")
	nf := &iptables.Netfilter{Run: f}
	if err := nf.EnsureJumpGroups(context.Background(), "spark0"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.chains["DOCKER-USER"], joinJumps(iptables.DockerUserJumps)) || !slices.Equal(f.chains["INPUT"], joinJumps(iptables.InputJumps)) {
		t.Fatalf("chains = %v", f.chains)
	}
	for _, w := range f.writes {
		if strings.Contains(w, "restore") || strings.Contains(w, "SPARK-CLU -") {
			t.Fatalf("chain content touched: %s", w)
		}
	}
	ok := iptables.JumpGroupInPlace(
		"-N DOCKER-USER\n-A DOCKER-USER -i spark0 -j SPARK-FWD\n-A DOCKER-USER -i sch+ -j SPARK-CLU\n-A DOCKER-USER -o sch+ -j SPARK-CLU\n-A DOCKER-USER -j RETURN\n",
		"DOCKER-USER", iptables.DockerUserJumps)
	bad := iptables.JumpGroupInPlace("-N DOCKER-USER\n-A DOCKER-USER -i sch+ -j SPARK-CLU\n-A DOCKER-USER -i spark0 -j SPARK-FWD\n", "DOCKER-USER", iptables.DockerUserJumps)
	if !ok || bad {
		t.Fatalf("JumpGroupInPlace ok=%v bad=%v", ok, bad)
	}
}
