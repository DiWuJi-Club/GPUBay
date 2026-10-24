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
