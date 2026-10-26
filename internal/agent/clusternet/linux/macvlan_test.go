package linux

import (
	"context"
	"strings"
	"testing"

	"spark-cloud/internal/agent/cmdrun"
	"spark-cloud/internal/clusterspec"
)

const defaultViaLan = "default via 192.168.1.1 dev enP7s7 proto dhcp src 192.168.1.50 metric 100 \n"

func stubMacvlanFirst(r *cmdrun.Recorder) {
	r.Stub("ip -4 route show default", defaultViaLan, nil)
	stubNoSpc0(r, "777")
	r.Stub("ip -o link show scm1234abcd", "", noDev("scm1234abcd"))
}

func TestApplyMacvlanDirect(t *testing.T) {
	r := &cmdrun.Recorder{}
	stubMacvlanFirst(r)
	n := newNet(r, Options{})
	rt, err := n.Apply(context.Background(), ids[0], 777, macvlanSpec(0, 2, 0))
	if err != nil || rt != (clusterspec.Runtime{}) {
		t.Fatalf("rt=%+v err=%v", rt, err)
	}
	assertLines(t, r, `
ip -4 route show default
ip -4 -o addr show dev enp1s0f1np1
nmcli device set enp1s0f1np1 managed no
ip -4 addr flush dev enp1s0f1np1
sysctl -w net.ipv6.conf.enp1s0f1np1.disable_ipv6=1
ip link set enp1s0f1np1 mtu 9000
ip link set enp1s0f1np1 up
nsenter -t 777 -n ip -o -4 addr show dev spc0
ip -o link show scm1234abcd
nsenter -t 777 -n ip link del scm1234abcd
ip link add scm1234abcd link enp1s0f1np1 type macvlan mode bridge
ip link set scm1234abcd netns 777
nsenter -t 777 -n ip link set scm1234abcd name spc0
nsenter -t 777 -n ip addr replace 10.210.3.11/24 dev spc0
nsenter -t 777 -n sysctl -w net.ipv6.conf.spc0.disable_ipv6=1
nsenter -t 777 -n ip link set spc0 up`)
}

// nmcli 不存在或失败：忽略，照常配置。
func TestApplyMacvlanWithoutNmcli(t *testing.T) {
	r := &cmdrun.Recorder{}
	stubMacvlanFirst(r)
	r.Stub("nmcli", "", &cmdrun.CmdError{Cmd: "nmcli", ExitCode: -1, Stderr: "exec: \"nmcli\": executable file not found in $PATH"})
	n := newNet(r, Options{})
	if _, err := n.Apply(context.Background(), ids[0], 777, macvlanSpec(0, 2, 0)); err != nil {
		t.Fatal(err)
	}
	hasCall(t, r, "nsenter -t 777 -n ip link set spc0 up")
}

func TestApplyMacvlanVLAN(t *testing.T) {
	// VLAN 子接口不存在：建出来，macvlan 挂在它上面；两块网卡都做卫生
	r := &cmdrun.Recorder{}
	stubMacvlanFirst(r)
	r.Stub("ip -o link show scv120", "", noDev("scv120"))
	n := newNet(r, Options{})
	if _, err := n.Apply(context.Background(), ids[0], 777, macvlanSpec(0, 2, 120)); err != nil {
		t.Fatal(err)
	}
	assertLines(t, r, `
ip -4 route show default
ip -4 -o addr show dev enp1s0f1np1
ip -o link show scv120
ip link add link enp1s0f1np1 name scv120 type vlan id 120
nmcli device set enp1s0f1np1 managed no
ip -4 addr flush dev enp1s0f1np1
sysctl -w net.ipv6.conf.enp1s0f1np1.disable_ipv6=1
ip link set enp1s0f1np1 mtu 9000
ip link set enp1s0f1np1 up
ip -4 addr flush dev scv120
sysctl -w net.ipv6.conf.scv120.disable_ipv6=1
ip link set scv120 mtu 9000
ip link set scv120 up
nsenter -t 777 -n ip -o -4 addr show dev spc0
ip -o link show scm1234abcd
nsenter -t 777 -n ip link del scm1234abcd
ip link add scm1234abcd link scv120 type macvlan mode bridge
ip link set scm1234abcd netns 777
nsenter -t 777 -n ip link set scm1234abcd name spc0
nsenter -t 777 -n ip addr replace 10.210.3.11/24 dev spc0
nsenter -t 777 -n sysctl -w net.ipv6.conf.spc0.disable_ipv6=1
nsenter -t 777 -n ip link set spc0 up`)

	// VLAN 子接口已存在（本机另一个集群实例在用）：不重建
	r = &cmdrun.Recorder{}
	stubMacvlanFirst(r)
	r.Stub("ip -o link show scv120", "9: scv120@enp1s0f1np1: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 9000 qdisc noqueue state UP mode DEFAULT group default qlen 1000\n", nil)
	n = newNet(r, Options{})
	if _, err := n.Apply(context.Background(), ids[1], 777, macvlanSpec(1, 2, 120)); err != nil {
		t.Fatal(err)
	}
	noCall(t, r, "type vlan", "ip link del scv120")
	hasCall(t, r, "ip link add scm5678efgh link scv120 type macvlan mode bridge")
}

// 集群网卡是管理网卡（持有默认路由，或持有 Probe 记下的 lan IP）：拒绝，不执行任何写操作。
func TestApplyRefusesLanIface(t *testing.T) {
	writes := []string{"ip link add", "ip link set", "flush", "nmcli", "sysctl", "addr replace", "route replace", "nsenter"}

	r := &cmdrun.Recorder{}
	r.Stub("ip -4 route show default", "default via 192.168.1.1 dev enp1s0f1np1 proto dhcp src 192.168.1.50 metric 100 \n", nil)
	n := newNet(r, Options{})
	_, err := n.Apply(context.Background(), ids[0], 777, macvlanSpec(0, 2, 0))
	if err == nil || !strings.HasPrefix(err.Error(), clusterspec.ErrPrefix) || !strings.Contains(err.Error(), "集群网卡不能是管理网卡") {
		t.Fatalf("err = %v", err)
	}
	noCall(t, r, writes...)

	r = &cmdrun.Recorder{}
	r.Stub("ip -4 route show default", defaultViaLan, nil)
	r.Stub("ip -4 -o addr show", "", nil)
	r.Stub("ip -4 -o addr show dev enp1s0f1np1", `4: enp1s0f1np1    inet 192.168.10.50/24 brd 192.168.10.255 scope global enp1s0f1np1\       valid_lft forever preferred_lft forever`+"\n", nil)
	n = New(r, Options{SysRoot: "testdata/sysfs-spark", Log: quiet()})
	if _, err := n.Probe(context.Background(), "192.168.10.50"); err != nil {
		t.Fatal(err)
	}
	r.Reset()
	_, err = n.Apply(context.Background(), ids[0], 777, macvlanSpec(0, 2, 0))
	if err == nil || !strings.Contains(err.Error(), "集群网卡不能是管理网卡") || !strings.Contains(err.Error(), "192.168.10.50") {
		t.Fatalf("err = %v", err)
	}
	noCall(t, r, writes...)

	// routed 模式本来就走管理网卡，不拒绝
	r = &cmdrun.Recorder{}
	stubNoSpc0(r, "4242")
	r.Stub("ip -4 route show default", "default via 192.168.1.1 dev eth0 \n", nil)
	n = newNet(r, Options{})
	if _, err := n.Apply(context.Background(), ids[1], 4242, routedSpec(1, 2)); err != nil {
		t.Fatal(err)
	}
}
