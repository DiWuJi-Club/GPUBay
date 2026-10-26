package clusterspec

import (
	"encoding/json"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("%s mismatch\n--- got\n%s--- want\n%s", name, got, want)
	}
}

func TestRenderGolden(t *testing.T) {
	routed := validRouted()
	mac := validMacvlan()
	rt := Runtime{HCA: "rocep1s0f1", GIDIndex: 3, RDMAActive: true}
	golden(t, "env-routed.golden", RenderEnv(routed, Runtime{}))
	golden(t, "json-routed.golden", RenderJSON(routed, Runtime{}))
	golden(t, "env-rdma.golden", RenderEnv(mac, rt))
	golden(t, "json-rdma.golden", RenderJSON(mac, rt))

	// JSON 是合法 JSON，network / rdma 取值
	var doc map[string]any
	if err := json.Unmarshal(RenderJSON(mac, rt), &doc); err != nil {
		t.Fatal(err)
	}
	if doc["network"] != "rdma" || doc["rdma"] == nil || doc["head_ip"] != "10.210.0.11" || doc["ifname"] != "spc0" {
		t.Fatalf("rdma json = %v", doc)
	}
	if err := json.Unmarshal(RenderJSON(routed, Runtime{}), &doc); err != nil {
		t.Fatal(err)
	}
	if doc["network"] != "ethernet" || doc["rdma"] != nil || doc["ip"] != "10.210.7.12" {
		t.Fatalf("routed json = %v", doc)
	}
	// 容器内文件不泄露宿主机 IP
	if strings.Contains(string(RenderJSON(routed, Runtime{})), "192.168.") || strings.Contains(string(RenderEnv(routed, Runtime{})), "192.168.") {
		t.Fatal("host ip leaked into container files")
	}
	// spec 要 RDMA 但运行时没有（GID 未找到 / 设备不可用）→ 退回以太网
	noGID := RenderEnv(mac, Runtime{HCA: "rocep1s0f1"})
	if !strings.Contains(string(noGID), "NCCL_IB_DISABLE='1'\n") || strings.Contains(string(noGID), "NCCL_IB_HCA") ||
		!strings.Contains(string(noGID), "SPARK_CLUSTER_NETWORK='ethernet'\n") {
		t.Fatalf("no-gid env:\n%s", noGID)
	}
	if err := json.Unmarshal(RenderJSON(mac, Runtime{HCA: "rocep1s0f1"}), &doc); err != nil || doc["network"] != "ethernet" || doc["rdma"] != nil {
		t.Fatalf("no-gid json = %v, %v", doc, err)
	}
	// 运行时说 RDMA 可用但 spec 没要 → 仍是以太网
	if s := string(RenderEnv(routed, rt)); !strings.Contains(s, "NCCL_IB_DISABLE='1'\n") {
		t.Fatalf("routed with rdma runtime:\n%s", s)
	}
}

func TestRenderEnvQuoting(t *testing.T) {
	s := validRouted()
	s.ClusterID = "clu-it's"
	got := string(RenderEnv(s, Runtime{}))
	if !strings.Contains(got, `export SPARK_CLUSTER_ID='clu-it'\''s'`+"\n") {
		t.Fatalf("quoting:\n%s", got)
	}
	got = string(RenderEnv(validMacvlan(), Runtime{HCA: "a'b", RDMAActive: true}))
	if !strings.Contains(got, `export NCCL_IB_HCA='a'\''b'`+"\n") {
		t.Fatalf("hca quoting:\n%s", got)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Fatal("no trailing newline")
	}
	for _, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
		rest, ok := strings.CutPrefix(line, "export ")
		if !ok {
			t.Fatalf("line without export: %q", line)
		}
		k, v, ok := strings.Cut(rest, "=")
		if !ok || k == "" || !strings.HasPrefix(v, "'") || !strings.HasSuffix(v, "'") {
			t.Fatalf("bad line %q", line)
		}
	}
}

// 每行都是 export：. 之后子进程（ray start、vllm serve、torchrun）读得到变量；含单引号的值原样还原。
func TestRenderEnvSourcedExports(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	s := validMacvlan()
	rt := Runtime{HCA: "a'b c", GIDIndex: 3, RDMAActive: true}
	f := filepath.Join(t.TempDir(), "cluster.env")
	if err := os.WriteFile(f, RenderEnv(s, rt), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(sh, "-c", `. "$1"; sh -c 'printf "%s|%s|%s|%s" "$NCCL_SOCKET_IFNAME" "$VLLM_HOST_IP" "$NCCL_IB_HCA" "$MASTER_ADDR"'`, "sh", f).CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if got := string(out); got != "spc0|10.210.0.11|a'b c|10.210.0.11" {
		t.Fatalf("child env = %q", got)
	}
}
