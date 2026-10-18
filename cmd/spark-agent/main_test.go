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
	r.Stub("iptables -w -S DOCKER-USER", "-N DOCKER-USER\n-A DOCKER-USER -j RETURN\n", nil)
	r.Stub("tc -V", "tc utility\n", nil)
	r.Stub("losetup -V", "losetup from util-linux\n", nil)
	r.Stub("ufw status", "", errors.New("not installed"))
	r.Stub("df", "Avail\n2000000000000\n", nil)
	var out bytes.Buffer
	if err := selfcheck(context.Background(), r, "/var/lib/spark-agent", &out); err != nil {
		t.Fatalf("selfcheck: %v\n%s", err, out.String())
	}
	for _, want := range []string{"ok: docker 29.6.2", "ok: nvidia runtime", "ok: iptables DOCKER-USER", "ok: tc", "ok: losetup", "ok: ufw", "ok: disk free"} {
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
