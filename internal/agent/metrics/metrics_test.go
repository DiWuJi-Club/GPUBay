package metrics_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/agent/cmdrun"
	"spark-cloud/internal/agent/metrics"
)

func TestParseNvidiaSMIHandlesNA(t *testing.T) {
	u, tc, p := metrics.ParseNvidiaSMI("12, 45, 30.50\n")
	if u != 12 || tc != 45 || p != 30.5 {
		t.Fatalf("parsed = %v %v %v", u, tc, p)
	}
	u, tc, p = metrics.ParseNvidiaSMI("[N/A], 51, [N/A]\n")
	if u != 0 || tc != 51 || p != 0 {
		t.Fatalf("N/A = %v %v %v", u, tc, p)
	}
	u, tc, p = metrics.ParseNvidiaSMI("NaN, 41, Inf")
	if u != 0 || tc != 41 || p != 0 {
		t.Fatalf("NaN/Inf = %v %v %v", u, tc, p)
	}
	if u, tc, p = metrics.ParseNvidiaSMI(""); u != 0 || tc != 0 || p != 0 {
		t.Fatal("empty")
	}
}

func TestParseMeminfoDFLoadavg(t *testing.T) {
	total, used := metrics.ParseMeminfo([]byte("MemTotal:       131072000 kB\nMemFree:  1000 kB\nMemAvailable:   100000000 kB\n"))
	if total != 131072000*1024 || used != (131072000-100000000)*1024 {
		t.Fatalf("meminfo = %d %d", total, used)
	}
	dt, du := metrics.ParseDF("        1B-blocks       Used\n4000787030016 1234567\n")
	if dt != 4000787030016 || du != 1234567 {
		t.Fatalf("df = %d %d", dt, du)
	}
	if l := metrics.ParseLoadavg([]byte("0.52 0.40 0.30 1/900 1234\n")); l != 0.52 {
		t.Fatalf("load = %v", l)
	}
}

func TestCollectNeverFails(t *testing.T) {
	r := &cmdrun.Recorder{}
	r.Stub("nvidia-smi", "", errors.New("no gpu"))
	r.Stub("df -B1 --output=size,used /", "1B-blocks Used\n1000 900\n", nil)
	r.Stub("df -B1 --output=size,used /var/lib/spark-agent/mnt/ins-a", "1B-blocks Used\n2000 5\n", nil)
	files := map[string]string{"/proc/meminfo": "MemTotal: 100 kB\nMemAvailable: 40 kB\n", "/proc/loadavg": "1.25 0 0 1/1 1\n"}
	c := &metrics.Collector{Run: r, ReadFile: func(p string) ([]byte, error) {
		if s, ok := files[p]; ok {
			return []byte(s), nil
		}
		return nil, errors.New("missing")
	}, IsMountPoint: mounted}
	m, insts := c.Collect(context.Background(), []metrics.InstanceMount{{ID: "ins-a", DataDir: "/var/lib/spark-agent/mnt/ins-a"}})
	if len(insts) != 1 || insts[0] != (metrics.InstanceMetrics{InstanceID: "ins-a", DataDiskUsed: 5}) {
		t.Fatalf("instances = %+v", insts)
	}
	if m.GPUUtil != 0 || m.RootDiskTotal != 1000 || m.RootDiskUsed != 900 || m.DataDiskUsed != 5 || m.MemTotal != 100*1024 || m.MemUsed != 60*1024 || m.Load1 != 1.25 {
		t.Fatalf("metrics = %+v", m)
	}
	if m, insts := c.Collect(context.Background(), nil); m.DataDiskUsed != 0 || insts == nil || len(insts) != 0 {
		t.Fatal("no data mount must give 0 and an empty instance list")
	}
	if r.Lines() != "nvidia-smi --query-gpu=utilization.gpu,temperature.gpu,power.draw --format=csv,noheader,nounits\ndf -B1 --output=size,used /\ndf -B1 --output=size,used /var/lib/spark-agent/mnt/ins-a\nnvidia-smi --query-gpu=utilization.gpu,temperature.gpu,power.draw --format=csv,noheader,nounits\ndf -B1 --output=size,used /" {
		t.Fatalf("calls:\n%s", r.Lines())
	}
}

// mounted 把所有数据目录都当成已挂载（测试里的路径并不存在）。
func mounted(string) bool { return true }

// 数据目录不是挂载点（数据盘没挂上）：df 会报根分区，所以该实例与机器级数据盘已用都记 0，且不跑 df。
func TestCollectUnmountedDataDirReportsZero(t *testing.T) {
	plain := t.TempDir() // 普通目录，与上级同一设备
	other := filepath.Join(t.TempDir(), "ins-b")
	r := &cmdrun.Recorder{}
	r.Stub("df -B1 --output=size,used /", "1B-blocks Used\n1000 900\n", nil)
	r.Stub("df -B1 --output=size,used "+plain, "1B-blocks Used\n1000 900\n", nil)
	r.Stub("df -B1 --output=size,used "+other, "1B-blocks Used\n2000 7\n", nil)
	c := &metrics.Collector{Run: r, ReadFile: os.ReadFile}
	m, insts := c.Collect(context.Background(), []metrics.InstanceMount{{ID: "ins-a", DataDir: plain}})
	if m.DataDiskUsed != 0 || len(insts) != 1 || insts[0].DataDiskUsed != 0 {
		t.Fatalf("unmounted dir: machine %d, instances %+v", m.DataDiskUsed, insts)
	}
	if strings.Contains(r.Lines(), plain) {
		t.Fatalf("df ran on unmounted dir:\n%s", r.Lines())
	}
	// 注入的检查：只有 other 算已挂载。
	c.IsMountPoint = func(dir string) bool { return dir == other }
	m, insts = c.Collect(context.Background(), []metrics.InstanceMount{{ID: "ins-a", DataDir: plain}, {ID: "ins-b", DataDir: other}})
	if m.DataDiskUsed != 7 || insts[0].DataDiskUsed != 0 || insts[1].DataDiskUsed != 7 {
		t.Fatalf("mixed: machine %d, instances %+v", m.DataDiskUsed, insts)
	}
}

func TestIsMountPoint(t *testing.T) {
	if metrics.IsMountPoint(t.TempDir()) {
		t.Fatal("ordinary temp dir reported as mount point")
	}
	if metrics.IsMountPoint(filepath.Join(t.TempDir(), "missing")) {
		t.Fatal("missing dir reported as mount point")
	}
}

func writeCgroup(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// 多实例：机器级数据盘为各实例之和；每实例读 cgroup 内存与 CPU，max 字面量记 0，CPU 按两次采样差计算。
func TestCollectInstancesFromCgroup(t *testing.T) {
	root := t.TempDir()
	dirA := filepath.Join(root, "system.slice", "docker-aaaa.scope")
	dirB := filepath.Join(root, "system.slice", "docker-bbbb.scope")
	writeCgroup(t, dirA, map[string]string{"memory.current": "1073741824\n", "memory.max": "32212254720\n",
		"cpu.stat": "usage_usec 1000000\nuser_usec 800000\nsystem_usec 200000\n"})
	writeCgroup(t, dirB, map[string]string{"memory.current": "4096\n", "memory.max": "max\n", "cpu.stat": "usage_usec 50\n"})
	r := &cmdrun.Recorder{}
	r.Stub("df -B1 --output=size,used /mnt/ins-a", "1B-blocks Used\n2000 100\n", nil)
	r.Stub("df -B1 --output=size,used /mnt/ins-b", "1B-blocks Used\n2000 23\n", nil)
	now := time.Unix(1000, 0)
	c := &metrics.Collector{Run: r, ReadFile: os.ReadFile, Now: func() time.Time { return now }, IsMountPoint: mounted}
	mounts := []metrics.InstanceMount{
		{ID: "ins-a", DataDir: "/mnt/ins-a", CgroupDir: dirA},
		{ID: "ins-b", DataDir: "/mnt/ins-b", CgroupDir: dirB},
		{ID: "ins-c", DataDir: "", CgroupDir: filepath.Join(root, "missing")}, // 读不到：全 0
	}
	m, insts := c.Collect(context.Background(), mounts)
	if m.DataDiskUsed != 123 {
		t.Fatalf("machine data disk = %d", m.DataDiskUsed)
	}
	want := []metrics.InstanceMetrics{
		{InstanceID: "ins-a", MemCurrent: 1 << 30, MemMax: 32212254720, DataDiskUsed: 100},
		{InstanceID: "ins-b", MemCurrent: 4096, MemMax: 0, DataDiskUsed: 23},
		{InstanceID: "ins-c"},
	}
	if len(insts) != 3 || insts[0] != want[0] || insts[1] != want[1] || insts[2] != want[2] {
		t.Fatalf("first sample = %+v", insts)
	}
	// 10 秒后 ins-a 用了 25 秒 CPU（2.5 个核）→ 250%；ins-b 计数回退（容器重启）→ 0。
	now = now.Add(10 * time.Second)
	writeCgroup(t, dirA, map[string]string{"cpu.stat": "usage_usec 26000000\n"})
	writeCgroup(t, dirB, map[string]string{"cpu.stat": "usage_usec 10\n"})
	_, insts = c.Collect(context.Background(), mounts)
	if insts[0].CPUPercent != 250 || insts[1].CPUPercent != 0 {
		t.Fatalf("second sample = %+v", insts)
	}
	// ins-b 回退后的样本成为新基准：再 10 秒用 5 秒 → 50%。
	now = now.Add(10 * time.Second)
	writeCgroup(t, dirB, map[string]string{"cpu.stat": "usage_usec 5000010\n"})
	_, insts = c.Collect(context.Background(), mounts)
	if insts[1].CPUPercent != 50 || insts[0].CPUPercent != 0 {
		t.Fatalf("third sample = %+v", insts)
	}
	// ins-a 离开（洗机）后再回来：旧样本已丢弃，首个样本为 0。
	_, _ = c.Collect(context.Background(), mounts[1:])
	now = now.Add(10 * time.Second)
	writeCgroup(t, dirA, map[string]string{"cpu.stat": "usage_usec 99000000\n"})
	_, insts = c.Collect(context.Background(), mounts)
	if insts[0].CPUPercent != 0 {
		t.Fatalf("returning instance must restart from 0: %+v", insts[0])
	}
	// cgroup 目录变了（容器重建）：同样从 0 开始。
	now = now.Add(10 * time.Second)
	dirA2 := filepath.Join(root, "system.slice", "docker-cccc.scope")
	writeCgroup(t, dirA2, map[string]string{"cpu.stat": "usage_usec 99999999999\n"})
	mounts[0].CgroupDir = dirA2
	_, insts = c.Collect(context.Background(), mounts)
	if insts[0].CPUPercent != 0 || insts[0].MemCurrent != 0 {
		t.Fatalf("recreated container must restart from 0: %+v", insts[0])
	}
}

func TestParseCgroupFiles(t *testing.T) {
	if metrics.ParseCgroupValue([]byte("max\n")) != 0 || metrics.ParseCgroupValue([]byte("123\n")) != 123 || metrics.ParseCgroupValue(nil) != 0 {
		t.Fatal("cgroup value")
	}
	if v, ok := metrics.ParseCPUStatUsage([]byte("usage_usec 42\nuser_usec 40\n")); !ok || v != 42 {
		t.Fatalf("usage = %d %v", v, ok)
	}
	if _, ok := metrics.ParseCPUStatUsage([]byte("user_usec 40\n")); ok {
		t.Fatal("missing usage_usec must be !ok")
	}
}
