package linux

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/agent/clusternet"
	"spark-cloud/internal/agent/cmdrun"
	"spark-cloud/internal/clusterspec"
)

const sparkSys = "testdata/sysfs-spark"

func touch(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// procRoot 造一个 <root>/<pid>/root/dev/infiniband/{rdma_cm, uverbs…}（容器内 docker --device 建的设备节点）。
func procRoot(t *testing.T, pid int, uverbs ...string) string {
	root := t.TempDir()
	dir := filepath.Join(root, strconv.Itoa(pid), "root", "dev", "infiniband")
	touch(t, filepath.Join(dir, "rdma_cm"))
	for _, u := range uverbs {
		touch(t, filepath.Join(dir, u))
	}
	return root
}

func rdmaSpec(rank int) clusterspec.Spec {
	s := macvlanSpec(rank, 2, 0)
	s.RDMA = true
	return s
}

func TestFindGID(t *testing.T) {
	n := New(&cmdrun.Recorder{}, Options{SysRoot: sparkSys, Log: quiet()})
	if i, ok := n.findGID("rocep1s0f1", "10.210.3.12"); !ok || i != 3 { // gids/2 是同一地址的 RoCE v1，跳过
		t.Fatalf("gid = %d %v", i, ok)
	}
	if _, ok := n.findGID("rocep1s0f1", "10.210.3.11"); ok {
		t.Fatal("10.210.3.11 has no GID")
	}
	if _, ok := n.findGID("rocep1s0f0", "10.210.3.12"); ok {
		t.Fatal("rocep1s0f0 has no GID table")
	}
	if gidFor("10.210.3.12") != "0000:0000:0000:0000:0000:ffff:0ad2:030c" {
		t.Fatal(gidFor("10.210.3.12"))
	}
}

// RDMA 成功：spc0 配好后在 GID 表里找到 ::ffff:A_r 的 RoCE v2 索引。
func TestApplyRDMAFindsGID(t *testing.T) {
	r := &cmdrun.Recorder{}
	r.Stub("ip -4 route show default", defaultViaLan, nil)
	r.Stub("nsenter -t 778 -n ip -o -4 addr show dev spc0", "5: spc0    inet 10.210.3.12/24 scope global spc0\n", nil)
	n := New(r, Options{SysRoot: sparkSys, ProcRoot: procRoot(t, 778, "uverbs1"), Log: quiet()})
	rt, err := n.Apply(context.Background(), ids[1], 778, rdmaSpec(1))
	if err != nil || rt != (clusterspec.Runtime{HCA: "rocep1s0f1", GIDIndex: 3, RDMAActive: true}) {
		t.Fatalf("rt=%+v err=%v", rt, err)
	}
}

func TestGIDTimeout(t *testing.T) {
	r := &cmdrun.Recorder{}
	r.Stub("ip -4 route show default", defaultViaLan, nil)
	r.Stub("nsenter -t 777 -n ip -o -4 addr show dev spc0", "5: spc0    inet 10.210.3.11/24 scope global spc0\n", nil)
	n := New(r, Options{SysRoot: sparkSys, ProcRoot: procRoot(t, 777, "uverbs1"), GIDWait: 50 * time.Millisecond, Log: quiet()})
	start := time.Now()
	rt, err := n.Apply(context.Background(), ids[0], 777, rdmaSpec(0))
	if !errors.Is(err, clusternet.ErrNoGID) || rt.RDMAActive || rt.HCA != "rocep1s0f1" || !strings.HasPrefix(err.Error(), clusterspec.ErrPrefix) || !strings.Contains(err.Error(), "未找到 RoCE GID") {
		t.Fatalf("rt=%+v err=%v", rt, err)
	}
	if d := time.Since(start); d < 50*time.Millisecond || d > 2*time.Second {
		t.Fatalf("waited %v", d)
	}
	hasCall(t, r, "nsenter -t 777 -n ip link set spc0 up") // 网络本身已配好，只是 RDMA 退回 TCP
}

// 宿主机重启后 uverbsN 编号变化：容器里的 uverbs0 现在指向 rocep1s0f0 而不是 rocep1s0f1。
func TestApplyRDMAUverbsChanged(t *testing.T) {
	r := &cmdrun.Recorder{}
	r.Stub("ip -4 route show default", defaultViaLan, nil)
	r.Stub("nsenter -t 778 -n ip -o -4 addr show dev spc0", "5: spc0    inet 10.210.3.12/24 scope global spc0\n", nil)
	n := New(r, Options{SysRoot: sparkSys, ProcRoot: procRoot(t, 778, "uverbs0"), Log: quiet()})
	rt, err := n.Apply(context.Background(), ids[1], 778, rdmaSpec(1))
	if err == nil || !strings.Contains(err.Error(), "RDMA 设备编号变化，需重建实例") || !errors.Is(err, clusternet.ErrNoGID) || rt.RDMAActive {
		t.Fatalf("rt=%+v err=%v", rt, err)
	}
	hasCall(t, r, "nsenter -t 778 -n ip addr replace 10.210.3.12/24 dev spc0")
}

func TestDevices(t *testing.T) {
	dev := t.TempDir()
	touch(t, filepath.Join(dev, "infiniband", "rdma_cm"), filepath.Join(dev, "infiniband", "uverbs1"))
	n := New(&cmdrun.Recorder{}, Options{SysRoot: sparkSys, DevRoot: dev, Log: quiet()})
	got, err := n.Devices(rdmaSpec(0))
	if err != nil || !reflect.DeepEqual(got, []string{filepath.Join(dev, "infiniband", "rdma_cm"), filepath.Join(dev, "infiniband", "uverbs1")}) {
		t.Fatalf("devices = %v %v", got, err)
	}
	if got, err := n.Devices(macvlanSpec(0, 2, 0)); got != nil || err != nil {
		t.Fatalf("non-RDMA = %v %v", got, err)
	}
	// 设备文件缺失
	s := rdmaSpec(0)
	s.Iface = "enP2p1s0f1np1" // uverbs3，DevRoot 里没有
	if _, err := n.Devices(s); err == nil || !strings.HasPrefix(err.Error(), clusterspec.ErrPrefix) || !strings.Contains(err.Error(), "uverbs3") {
		t.Fatalf("missing = %v", err)
	}
	// 网卡没有 RDMA
	s.Iface = "enP7s7"
	if _, err := n.Devices(s); err == nil || !strings.HasPrefix(err.Error(), clusterspec.ErrPrefix) {
		t.Fatalf("no rdma = %v", err)
	}
	// 默认 DevRoot
	if New(&cmdrun.Recorder{}, Options{}).opt.DevRoot != "/dev" {
		t.Fatal("default DevRoot")
	}
}
