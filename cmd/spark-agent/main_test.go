package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"spark-cloud/internal/agent/cmdrun"
)

func TestParseConfigFile(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("# 平台地址\nplatform: agent.example:9443\nserver_name: agent.example\n"), 0o600)
	cfg, err := loadAgentConfig(dir)
	if err != nil || cfg.Platform != "agent.example:9443" || cfg.ServerName != "agent.example" {
		t.Fatalf("cfg = %+v %v", cfg, err)
	}
	if _, err := loadAgentConfig(t.TempDir()); err == nil {
		t.Fatal("missing config accepted")
	}
}

func TestSelfcheckReportsEachItem(t *testing.T) {
	r := &cmdrun.Recorder{}
	r.Stub("docker version", "29.6.2\n", nil)
	r.Stub("docker info", `{"nvidia":{"path":"nvidia-container-runtime"}}`, nil)
	stubShared(r)
	r.Stub("iptables -w -S DOCKER-USER", "-N DOCKER-USER\n-A DOCKER-USER -j RETURN\n", nil)
	r.Stub("tc -V", "tc utility\n", nil)
	r.Stub("losetup -V", "losetup from util-linux\n", nil)
	r.Stub("ufw status", "", errors.New("not installed"))
	r.Stub("df", "Avail\n2000000000000\n", nil)
	var out bytes.Buffer
	if err := selfcheck(context.Background(), r, "/var/lib/spark-agent", &out); err != nil {
		t.Fatalf("selfcheck: %v\n%s", err, out.String())
	}
	for _, want := range []string{"ok: docker 29.6.2", "ok: nvidia runtime", "ok: iptables DOCKER-USER", "ok: tc", "ok: losetup", "ok: ufw", "ok: disk free",
		"ok: nproc 20", "ok: cgroup v2 cpu memory pids"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in:\n%s", want, out.String())
		}
	}
	r.Stub("docker info", `{}`, nil)
	r.Stub("ufw status", "Status: active\n", nil)
	r.Stub("df", "Avail\n1000\n", nil)
	out.Reset()
	if err := selfcheck(context.Background(), r, "/var/lib/spark-agent", &out); err == nil || !strings.Contains(out.String(), "FAIL: nvidia runtime") || !strings.Contains(out.String(), "FAIL: ufw") || !strings.Contains(out.String(), "FAIL: disk free") {
		t.Fatalf("expected failures, err=%v:\n%s", err, out.String())
	}
}

func TestVersionCommand(t *testing.T) {
	var out bytes.Buffer
	if err := run([]string{"version"}, &out); err != nil || !strings.Contains(out.String(), "spark-agent ") {
		t.Fatalf("version = %q %v", out.String(), err)
	}
	if err := run([]string{"bogus"}, &out); err == nil {
		t.Fatal("unknown subcommand accepted")
	}
}

func TestSelfcheckGPUViaRuntimeOrCDI(t *testing.T) {
	base := func() *cmdrun.Recorder {
		r := &cmdrun.Recorder{}
		r.Stub("df", "Avail\n2000000000000\n", nil)
		r.Stub("ufw status", "", errors.New("not installed"))
		stubShared(r)
		return r
	}
	cases := []struct {
		name    string
		stub    func(r *cmdrun.Recorder)
		want    string
		grepped bool
	}{
		{"runtime", func(r *cmdrun.Recorder) {
			r.Stub("docker info --format {{json .Runtimes}}", `{"nvidia":{"path":"nvidia-container-runtime"},"runc":{"path":"runc"}}`, nil)
		}, "ok: nvidia runtime", false},
		{"cdi only", func(r *cmdrun.Recorder) { // DGX Spark 实测：没有 nvidia 运行时，--gpus 走 CDI
			r.Stub("docker info --format {{json .Runtimes}}", `{"io.containerd.runc.v2":{"path":"runc"},"runc":{"path":"runc"}}`, nil)
			r.Stub("docker info --format {{json .CDISpecDirs}}", `["/etc/cdi","/var/run/cdi"]`+"\n", nil)
			r.Stub("grep", "/var/run/cdi/nvidia.yaml\n", &cmdrun.CmdError{Cmd: "grep", ExitCode: 2, Stderr: "grep: /etc/cdi: No such file or directory"})
		}, "ok: nvidia runtime CDI /var/run/cdi/nvidia.yaml", true},
		{"neither", func(r *cmdrun.Recorder) {
			r.Stub("docker info --format {{json .Runtimes}}", `{"runc":{"path":"runc"}}`, nil)
			r.Stub("docker info --format {{json .CDISpecDirs}}", `["/etc/cdi","/var/run/cdi"]`, nil)
			r.Stub("grep", "", &cmdrun.CmdError{Cmd: "grep", ExitCode: 1})
		}, "FAIL: nvidia runtime", true},
		{"cdi disabled", func(r *cmdrun.Recorder) {
			r.Stub("docker info --format {{json .Runtimes}}", `{"runc":{"path":"runc"}}`, nil)
			r.Stub("docker info --format {{json .CDISpecDirs}}", `[]`, nil)
		}, "FAIL: nvidia runtime", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := base()
			c.stub(r)
			var out bytes.Buffer
			err := selfcheck(context.Background(), r, "/var/lib/spark-agent", &out)
			if !strings.Contains(out.String(), c.want) || (err == nil) != strings.HasPrefix(c.want, "ok") {
				t.Fatalf("err=%v, want %q in:\n%s", err, c.want, out.String())
			}
			if c.grepped != strings.Contains(r.Lines(), "grep -rls --include=*.yaml --include=*.json nvidia.com/gpu /etc/cdi /var/run/cdi") {
				t.Fatalf("grep call mismatch (want %v):\n%s", c.grepped, r.Lines())
			}
			if !strings.Contains(out.String(), "ok: command nvidia-smi") {
				t.Fatalf("nvidia-smi item missing:\n%s", out.String())
			}
		})
	}
}

// nproc 不足 / 读不出只告警、不让自检失败（只和已搁置的共享机有关）。
func TestSelfcheckNprocOnlyWarns(t *testing.T) {
	cases := []struct {
		name string
		stub func(r *cmdrun.Recorder)
		want string
	}{
		{"few cpus", func(r *cmdrun.Recorder) { r.Stub("nproc", "2\n", nil) }, "WARN: nproc: 只有 2 个 CPU，少于 4"},
		{"garbage", func(r *cmdrun.Recorder) { r.Stub("nproc", "x\n", nil) }, "WARN: nproc: 无法解析 nproc 输出"},
		{"nproc error", func(r *cmdrun.Recorder) { r.Stub("nproc", "", errors.New("boom")) }, "WARN: nproc: boom"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &cmdrun.Recorder{}
			r.Stub("docker info", `{"nvidia":{"path":"nvidia-container-runtime"}}`, nil)
			r.Stub("ufw status", "", errors.New("not installed"))
			r.Stub("df", "Avail\n2000000000000\n", nil)
			stubShared(r)
			c.stub(r)
			var out bytes.Buffer
			if err := selfcheck(context.Background(), r, "/var/lib/spark-agent", &out); err != nil {
				t.Fatalf("selfcheck failed on nproc warning: %v\n%s", err, out.String())
			}
			if !strings.Contains(out.String(), c.want) || strings.Contains(out.String(), "FAIL") {
				t.Fatalf("want %q, no FAIL, in:\n%s", c.want, out.String())
			}
		})
	}
}

// stubShared 给共享机相关的自检项（nproc、cgroup v2）打上 DGX Spark 的正常输出。
func stubShared(r *cmdrun.Recorder) {
	r.Stub("nproc", "20\n", nil)
	r.Stub("docker info --format {{.CgroupVersion}}", "2\n", nil)
	r.Stub("cat /sys/fs/cgroup/cgroup.controllers", "cpuset cpu io memory hugetlb pids rdma misc\n", nil)
}

func TestSelfcheckSharedMachineItems(t *testing.T) {
	cases := []struct {
		name string
		stub func(r *cmdrun.Recorder)
		want string
	}{
		{"ok", func(*cmdrun.Recorder) {}, ""},
		{"cgroup v1", func(r *cmdrun.Recorder) { r.Stub("docker info --format {{.CgroupVersion}}", "1\n", nil) }, "FAIL: cgroup v2: docker 的 Cgroup Version 为 \"1\"，需要 2"},
		{"no pids controller", func(r *cmdrun.Recorder) {
			r.Stub("cat /sys/fs/cgroup/cgroup.controllers", "cpuset cpu io memory\n", nil)
		}, "FAIL: cgroup v2: cgroup 缺少控制器 pids"},
		{"no controllers file", func(r *cmdrun.Recorder) {
			r.Stub("cat /sys/fs/cgroup/cgroup.controllers", "", &cmdrun.CmdError{Cmd: "cat", ExitCode: 1, Stderr: "No such file"})
		}, "FAIL: cgroup v2: 读取 /sys/fs/cgroup/cgroup.controllers 失败"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &cmdrun.Recorder{}
			r.Stub("docker info", `{"nvidia":{"path":"nvidia-container-runtime"}}`, nil)
			r.Stub("ufw status", "", errors.New("not installed"))
			r.Stub("df", "Avail\n2000000000000\n", nil)
			stubShared(r)
			c.stub(r)
			var out bytes.Buffer
			err := selfcheck(context.Background(), r, "/var/lib/spark-agent", &out)
			if c.want == "" {
				if err != nil || !strings.Contains(out.String(), "ok: nproc 20") || !strings.Contains(out.String(), "ok: cgroup v2") {
					t.Fatalf("err=%v:\n%s", err, out.String())
				}
				return
			}
			if err == nil || !strings.Contains(out.String(), c.want) {
				t.Fatalf("err=%v, want %q in:\n%s", err, c.want, out.String())
			}
		})
	}
}

// ---- 集群网卡（计划 13）：全部只告警，不让自检失败 ----

const (
	sysfs221   = "../../internal/agent/clusternet/linux/testdata/sysfs-221"
	sysfsSpark = "../../internal/agent/clusternet/linux/testdata/sysfs-spark"
)

// stubAllOK 给非集群自检项打上正常输出。
func stubAllOK(r *cmdrun.Recorder) {
	r.Stub("docker info", `{"nvidia":{"path":"nvidia-container-runtime"}}`, nil)
	r.Stub("ufw status", "", errors.New("not installed"))
	r.Stub("df", "Avail\n2000000000000\n", nil)
	stubShared(r)
}

func TestSelfcheckClusterSection(t *testing.T) {
	r := &cmdrun.Recorder{}
	stubAllOK(r)
	// 221 真机输出（2026-10-26）：管理网卡是 WiFi，有线口 enP7s7 未插线，没有 CX-7。
	r.Stub("ip -4 -o addr show", "1: lo    inet 127.0.0.1/8 scope host lo\\       valid_lft forever preferred_lft forever\n"+
		"7: wlP9s9    inet 192.168.1.221/23 brd 192.168.1.255 scope global dynamic noprefixroute wlP9s9\\       valid_lft 470777sec preferred_lft 470777sec\n"+
		"8: spark0    inet 10.200.0.1/24 brd 10.200.0.255 scope global spark0\\       valid_lft forever preferred_lft forever\n", nil)
	r.Stub("ip -4 route show", "default via 192.168.1.1 dev wlP9s9 proto dhcp src 192.168.1.221 metric 600 \n"+
		"10.200.0.0/24 dev spark0 proto kernel scope link src 10.200.0.1 \n192.168.0.0/23 dev wlP9s9 proto kernel scope link src 192.168.1.221 metric 600 \n", nil)
	r.Stub("ip -4 route show default", "default via 192.168.1.1 dev wlP9s9 proto dhcp src 192.168.1.221 metric 600 \n", nil)
	r.Stub("ip -6 -o addr show", "7: wlP9s9    inet6 fe80::244:5d6c:9aaa:7332/64 scope link noprefixroute \\       valid_lft forever preferred_lft forever\n", nil)
	r.Stub("nmcli -t -f DEVICE,STATE device", "wlP9s9:connected\nspark0:connected (externally)\nenP7s7:unavailable\n", nil)
	r.Stub("cat /proc/sys/net/ipv4/ip_forward", "1\n", nil)
	var out bytes.Buffer
	if err := selfcheckAt(context.Background(), r, "/var/lib/spark-agent", sysfs221, &out); err != nil {
		t.Fatalf("cluster warnings must not fail selfcheck: %v\n%s", err, out.String())
	}
	for _, want := range []string{
		"ok: iface enP7s7 up=false speed=0 driver=r8127 rdma=- active=false\n",
		"ok: iface wlP9s9 up=true speed=0 driver=mt7925e rdma=- active=false wireless=true\n",
		"WARN: 未发现 ConnectX-7 网卡，集群将只能使用普通以太网\n",
		"ok: ip_forward 1\n",
		"ok: 集群网段 10.210.0.0/16 无冲突\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "FAIL") || strings.Contains(out.String(), "NetworkManager 管理") {
		t.Fatalf("unexpected lines:\n%s", out.String())
	}
	// 探测失败（如读不到 sysfs）也只告警
	out.Reset()
	if err := selfcheckAt(context.Background(), r, "/var/lib/spark-agent", t.TempDir()+"/nope", &out); err != nil || !strings.Contains(out.String(), "WARN: 集群网卡: cluster-net: ") {
		t.Fatalf("probe failure: err=%v\n%s", err, out.String())
	}
}

// CX-7 口受 NM 管理 / 有地址、ip_forward=0、本机地址或路由落在集群网段 → 各自 WARN，自检仍通过。
func TestSelfcheckNMManagedWarn(t *testing.T) {
	r := &cmdrun.Recorder{}
	stubAllOK(r)
	r.Stub("ip -4 -o addr show", "2: enP7s7    inet 192.168.1.50/24 brd 192.168.1.255 scope global enP7s7\\       valid_lft forever preferred_lft forever\n"+
		"5: enp1s0f1np1    inet 169.254.7.7/16 brd 169.254.255.255 scope link enp1s0f1np1\\       valid_lft forever preferred_lft forever\n"+
		"6: eth9    inet 10.210.9.2/24 scope global eth9\\       valid_lft forever preferred_lft forever\n"+
		"12: schabcd1234    inet 10.210.3.1/32 scope global schabcd1234\\       valid_lft forever preferred_lft forever\n", nil)
	r.Stub("ip -4 route show", "default via 192.168.1.1 dev enP7s7 proto dhcp src 192.168.1.50 metric 100 \n"+
		"10.0.0.0/8 via 192.168.1.254 dev enP7s7 \n"+ // 包住整个集群网段的大路由：集群的 /32 更具体，不算冲突
		"10.210.128.0/17 via 192.168.1.254 dev enP7s7 \n"+ // 落在集群网段里的路由
		"10.210.9.0/24 dev eth9 proto kernel scope link src 10.210.9.2 \n"+ // 与地址重复的直连路由只报地址
		"10.210.0.12 dev schabcd1234 scope link \n"+ // Agent 自己的对端路由不算冲突
		"192.168.1.0/24 dev enP7s7 proto kernel scope link src 192.168.1.50 \n", nil)
	r.Stub("ip -4 route show default", "default via 192.168.1.1 dev enP7s7 proto dhcp src 192.168.1.50 metric 100 \n", nil)
	r.Stub("ip -6 -o addr show", "5: enp1s0f1np1    inet6 fe80::966d:aeff:fe00:2/64 scope link \\       valid_lft forever preferred_lft forever\n"+
		"7: enP2p1s0f1np1    inet6 fe80::966d:aeff:fe00:4/64 scope link \\       valid_lft forever preferred_lft forever\n", nil)
	r.Stub("nmcli -t -f DEVICE,STATE device", "enP7s7:connected\nenp1s0f0np0:unmanaged\nenp1s0f1np1:connected\nenP2p1s0f0np0:unmanaged\nenP2p1s0f1np1:disconnected\nlo:unmanaged\n", nil)
	r.Stub("cat /proc/sys/net/ipv4/ip_forward", "0\n", nil)
	var out bytes.Buffer
	if err := selfcheckAt(context.Background(), r, "/var/lib/spark-agent", sysfsSpark, &out); err != nil {
		t.Fatalf("cluster warnings must not fail selfcheck: %v\n%s", err, out.String())
	}
	s := out.String()
	for _, want := range []string{
		"ok: iface enp1s0f1np1 up=true speed=200000 driver=mlx5_core rdma=rocep1s0f1 active=true\n",
		"ok: iface enp1s0f0np0 up=false speed=0 driver=mlx5_core rdma=rocep1s0f0 active=false\n",
		"WARN: enp1s0f1np1 上有地址 169.254.7.7/16 fe80::966d:aeff:fe00:2/64，作为集群网卡时 Agent 会清除\n",
		"WARN: enP2p1s0f1np1 上有地址 fe80::966d:aeff:fe00:4/64，作为集群网卡时 Agent 会清除\n",
		"WARN: enp1s0f1np1 受 NetworkManager 管理，NM 可能重新加地址；请执行 install.sh --cluster-iface enp1s0f1np1\n",
		"WARN: enP2p1s0f1np1 受 NetworkManager 管理，NM 可能重新加地址；请执行 install.sh --cluster-iface enP2p1s0f1np1\n",
		"WARN: ip_forward: /proc/sys/net/ipv4/ip_forward 为 0",
		"WARN: 集群网段 10.210.0.0/16: 与集群网段冲突：地址 10.210.9.2/24（eth9）；路由 10.210.128.0/17（enP7s7）\n",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in:\n%s", want, s)
		}
	}
	for _, bad := range []string{"未发现 ConnectX-7", "enp1s0f0np0 受 NetworkManager", "enP2p1s0f0np0 受 NetworkManager", "enP7s7 受 NetworkManager", "enP7s7 上有地址", "schabcd1234", "FAIL"} {
		if strings.Contains(s, bad) {
			t.Fatalf("unexpected %q in:\n%s", bad, s)
		}
	}
	// 没有 nmcli：跳过 NM 检查，不报受管
	r.Stub("nmcli", "", &cmdrun.CmdError{Cmd: "nmcli", ExitCode: 127, Stderr: "not found"})
	out.Reset()
	if err := selfcheckAt(context.Background(), r, "/var/lib/spark-agent", sysfsSpark, &out); err != nil || strings.Contains(out.String(), "受 NetworkManager 管理") || !strings.Contains(out.String(), "ok: NetworkManager 未运行，跳过集群网卡受管检查\n") {
		t.Fatalf("no nmcli: err=%v\n%s", err, out.String())
	}
}
