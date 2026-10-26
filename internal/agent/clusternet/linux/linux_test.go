package linux

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"

	"spark-cloud/internal/agent/cmdrun"
	"spark-cloud/internal/clusterspec"
)

// 测试用实例 ID：rank r 的成员是 ids[r]；宿主机 IP 192.168.1.(221+r)。
var ids = []string{"ins-1234abcd", "ins-5678efgh", "ins-9abcijkl", "ins-0defmnop"}

func hostIP(rank int) string {
	return []string{"192.168.1.221", "192.168.1.222", "192.168.1.223", "192.168.1.224"}[rank]
}

// routedSpec：集群 clu-aaaaaaaaaa，子网 10.210.3.0/24，iface eth0；ranks 为在位成员（nil = 0..size-1）。
func routedSpec(rank, size int, ranks ...int) clusterspec.Spec {
	if ranks == nil {
		for r := 0; r < size; r++ {
			ranks = append(ranks, r)
		}
	}
	s := clusterspec.Spec{ClusterID: "clu-aaaaaaaaaa", Version: 1, Size: size, Rank: rank, Mode: clusterspec.ModeRouted, Iface: "eth0", Subnet: "10.210.3.0/24"}
	for _, r := range ranks {
		s.Members = append(s.Members, clusterspec.Member{Rank: r, InstanceID: ids[r], IP: memberIP(3, r), HostIP: hostIP(r)})
	}
	return s
}

// macvlanSpec：CX-7 口 enp1s0f1np1（夹具 sysfs-spark 里 rocep1s0f1 / uverbs1），vlan 0 = direct。
func macvlanSpec(rank, size, vlan int) clusterspec.Spec {
	s := clusterspec.Spec{ClusterID: "clu-bbbbbbbbbb", Version: 1, Size: size, Rank: rank, Mode: clusterspec.ModeMacvlan, Iface: "enp1s0f1np1", VLAN: vlan, MTU: 9000, Subnet: "10.210.3.0/24"}
	for r := 0; r < size; r++ {
		s.Members = append(s.Members, clusterspec.Member{Rank: r, InstanceID: ids[r], IP: memberIP(3, r)})
	}
	return s
}

func memberIP(c, rank int) string {
	return "10.210." + strconv.Itoa(c) + "." + strconv.Itoa(11+rank)
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newNet(r *cmdrun.Recorder, opt Options) *Net {
	opt.Log = quiet()
	return New(r, opt)
}

// 真机（221，iproute2 6.x）上对象不存在时的报错。
func noDev(name string) error {
	return &cmdrun.CmdError{Cmd: "ip", ExitCode: 1, Stderr: "Device \"" + name + "\" does not exist.\n"}
}

var cannotFind = &cmdrun.CmdError{Cmd: "ip", ExitCode: 1, Stderr: "Cannot find device \"x\"\n"}
var noSuchProcess = &cmdrun.CmdError{Cmd: "ip", ExitCode: 2, Stderr: "RTNETLINK answers: No such process\n"}

// 容器内 spc0 不存在（首次）。
func stubNoSpc0(r *cmdrun.Recorder, pid string) {
	r.Stub("nsenter -t "+pid+" -n ip -o -4 addr show dev spc0", "", noDev("spc0"))
	r.Stub("nsenter -t "+pid+" -n ip link del ", "", cannotFind)
}

func assertLines(t *testing.T, r *cmdrun.Recorder, want string) {
	t.Helper()
	if got := r.Lines(); got != strings.TrimSpace(want) {
		gl, wl := strings.Split(got, "\n"), strings.Split(strings.TrimSpace(want), "\n")
		for i := 0; i < max(len(gl), len(wl)); i++ {
			g, w := "", ""
			if i < len(gl) {
				g = gl[i]
			}
			if i < len(wl) {
				w = wl[i]
			}
			if g != w {
				t.Fatalf("line %d differs:\n got: %s\nwant: %s\n\nfull got:\n%s", i+1, g, w, got)
			}
		}
	}
}

func noCall(t *testing.T, r *cmdrun.Recorder, subs ...string) {
	t.Helper()
	for _, c := range r.Calls {
		for _, s := range subs {
			if strings.Contains(c, s) {
				t.Fatalf("unexpected call %q (contains %q)\nall:\n%s", c, s, r.Lines())
			}
		}
	}
}

func hasCall(t *testing.T, r *cmdrun.Recorder, line string) {
	t.Helper()
	for _, c := range r.Calls {
		if c == line {
			return
		}
	}
	t.Fatalf("missing call %q\nall:\n%s", line, r.Lines())
}

func TestApplyValidatesFirst(t *testing.T) {
	r := &cmdrun.Recorder{}
	n := newNet(r, Options{})
	s := routedSpec(1, 2)
	s.Subnet = "10.210.3.5/24"
	_, err := n.Apply(context.Background(), ids[1], 4242, s)
	if err == nil || !strings.HasPrefix(err.Error(), clusterspec.ErrPrefix) || len(r.Calls) != 0 {
		t.Fatalf("err=%v calls=%s", err, r.Lines())
	}
	if _, err := n.Apply(context.Background(), ids[1], 0, routedSpec(1, 2)); err == nil || len(r.Calls) != 0 {
		t.Fatalf("pid 0: err=%v calls=%s", err, r.Lines())
	}
}

// 命令失败：错误以 ErrPrefix 开头、带命令原文，后续步骤不再执行。
func TestApplyStopsOnFirstFailure(t *testing.T) {
	r := &cmdrun.Recorder{}
	stubNoSpc0(r, "4242")
	r.Stub("ip -o link show sch5678efgh", "", noDev("sch5678efgh"))
	r.Stub("ip link set scc5678efgh netns", "", &cmdrun.CmdError{Cmd: "ip link set scc5678efgh netns 4242", ExitCode: 2, Stderr: "RTNETLINK answers: Invalid argument"})
	n := newNet(r, Options{})
	_, err := n.Apply(context.Background(), ids[1], 4242, routedSpec(1, 2))
	if err == nil || !strings.HasPrefix(err.Error(), clusterspec.ErrPrefix) || !strings.Contains(err.Error(), "Invalid argument") {
		t.Fatalf("err = %v", err)
	}
	noCall(t, r, "name spc0", "addr replace")
}

func TestTeardownKeepsSharedVLAN(t *testing.T) {
	s := macvlanSpec(0, 2, 120)
	other := macvlanSpec(1, 2, 120)
	other.ClusterID = "clu-cccccccccc"
	r := &cmdrun.Recorder{}
	n := newNet(r, Options{})
	if err := n.Teardown(context.Background(), ids[0], s, []clusterspec.Spec{other}); err != nil {
		t.Fatal(err)
	}
	assertLines(t, r, "ip link del scm1234abcd")

	r.Reset()
	differentVLAN := macvlanSpec(1, 2, 121)
	if err := n.Teardown(context.Background(), ids[0], s, []clusterspec.Spec{differentVLAN, routedSpec(1, 2)}); err != nil {
		t.Fatal(err)
	}
	assertLines(t, r, "ip link del scm1234abcd\nip link del scv120")
}

func TestTeardownIgnoresMissing(t *testing.T) {
	r := &cmdrun.Recorder{}
	r.Stub("ip -4 route show root 10.210.3.0/24 dev eth0", "10.210.3.11 via 192.168.1.221 \n10.210.3.13 via 192.168.1.223 \n", nil)
	r.Stub("ip route del", "", noSuchProcess)
	r.Stub("ip link del", "", cannotFind)
	n := newNet(r, Options{})
	if err := n.Teardown(context.Background(), ids[1], routedSpec(1, 3), nil); err != nil {
		t.Fatal(err)
	}
	assertLines(t, r, `
ip -4 route show root 10.210.3.0/24 dev eth0
ip route del 10.210.3.11/32 dev eth0
ip route del 10.210.3.13/32 dev eth0
ip link del sch5678efgh`)

	// 路由列不出：按 Spec 的对端逐条删（不存在忽略）
	r = &cmdrun.Recorder{}
	r.Stub("ip -4 route show", "", &cmdrun.CmdError{ExitCode: 1, Stderr: "Cannot find device \"eth0\""})
	r.Stub("ip route del", "", noSuchProcess)
	n = newNet(r, Options{})
	if err := n.Teardown(context.Background(), ids[1], routedSpec(1, 3), nil); err != nil {
		t.Fatal(err)
	}
	hasCall(t, r, "ip route del 10.210.3.13/32 dev eth0")

	// 真正的失败要上报
	r = &cmdrun.Recorder{}
	r.Stub("ip link del", "", &cmdrun.CmdError{ExitCode: 2, Stderr: "RTNETLINK answers: Operation not permitted"})
	n = newNet(r, Options{})
	err := n.Teardown(context.Background(), ids[0], macvlanSpec(0, 2, 0), nil)
	var ce *cmdrun.CmdError
	if err == nil || !strings.HasPrefix(err.Error(), clusterspec.ErrPrefix) || !errors.As(err, &ce) {
		t.Fatalf("err = %v", err)
	}

	// 同一子网仍有本机实例在用：不删对端路由
	r = &cmdrun.Recorder{}
	n = newNet(r, Options{})
	if err := n.Teardown(context.Background(), ids[1], routedSpec(1, 3), []clusterspec.Spec{routedSpec(2, 3)}); err != nil {
		t.Fatal(err)
	}
	assertLines(t, r, "ip link del sch5678efgh")
}
