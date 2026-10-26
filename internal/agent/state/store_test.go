package state_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"spark-cloud/internal/agent/state"
	"spark-cloud/internal/clusterspec"
)

func TestStoreResultsInstancesAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Result("c1"); ok {
		t.Fatal("empty store has results")
	}
	if err := s.PutResult("c1", state.Result{OK: false, Message: "boom"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutInstance(state.Instance{ID: "ins-aaaaaaaa", Desired: "running", ImageRef: "img:1", SSHPort: 2222, BandwidthMbps: 200, DataSizeGiB: 2048, LanIP: "10.0.0.10"}); err != nil {
		t.Fatal(err)
	}
	if un := s.UnsentResults(); len(un) != 1 || un[0].ID != "c1" || un[0].Message != "boom" {
		t.Fatalf("unsent = %+v", un)
	}
	if err := s.MarkSent("c1"); err != nil {
		t.Fatal(err)
	}
	// 重新打开读到同样内容
	s2, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if r, ok := s2.Result("c1"); !ok || r.Message != "boom" || !r.Sent {
		t.Fatalf("reopened result = %+v %v", r, ok)
	}
	if inst, ok := s2.Instance("ins-aaaaaaaa"); !ok || inst.Desired != "running" || inst.DataSizeGiB != 2048 {
		t.Fatalf("reopened instance = %+v", inst)
	}
	if len(s2.UnsentResults()) != 0 {
		t.Fatal("sent flag lost")
	}
	if err := s2.DeleteInstance("ins-aaaaaaaa"); err != nil || len(s2.Instances()) != 0 {
		t.Fatalf("delete = %v %v", err, s2.Instances())
	}
	// 只保留最近 N 条结果（生产 N=1000；测试把 N 调成 5，macOS 上每次 fsync 很慢），最早的被淘汰
	restore := state.SetMaxResultsForTest(5)
	defer restore()
	for i := 0; i < 8; i++ {
		if err := s2.PutResult(fmt.Sprintf("r%d", i), state.Result{OK: true}); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := s2.Result("r2"); ok {
		t.Fatal("oldest result not evicted")
	}
	if _, ok := s2.Result("r3"); !ok {
		t.Fatal("5th newest result must remain")
	}
	if _, ok := s2.Result("r7"); !ok {
		t.Fatal("newest result missing")
	}
	// 没有残留临时文件
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("dir = %v", entries)
	}
}

// 旧状态文件没有 share_slots：读出为 0；新写入的份数重开后保留。
func TestInstanceShareSlotsBackwardCompatible(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	old := `{"results":[],"instances":{"ins-aaaaaaaa":{"id":"ins-aaaaaaaa","desired":"running","image_ref":"img:1","lan_ip":"10.0.0.10","ssh_port":2222,"bandwidth_mbps":200,"data_size_gib":2048}}}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if inst, ok := s.Instance("ins-aaaaaaaa"); !ok || inst.ShareSlots != 0 || inst.DataSizeGiB != 2048 {
		t.Fatalf("old instance = %+v %v", inst, ok)
	}
	if err := s.PutInstance(state.Instance{ID: "ins-bbbbbbbb", Desired: "stopped", SSHPort: 2224, DataSizeGiB: 512, ShareSlots: 4}); err != nil {
		t.Fatal(err)
	}
	s2, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if inst, ok := s2.Instance("ins-bbbbbbbb"); !ok || inst.ShareSlots != 4 || inst.DataSizeGiB != 512 {
		t.Fatalf("reopened = %+v %v", inst, ok)
	}
}

// 计划 13：Cluster / ClusterRuntime / ClusterNote 往返；旧状态文件没有这些字段时读出为 nil / 空；
// 返回值是深拷贝，调用方改动不影响 Store。
func TestStateRoundTripCluster(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	old := `{"results":[],"instances":{"ins-aaaaaaaa":{"id":"ins-aaaaaaaa","desired":"running","image_ref":"img:1","lan_ip":"10.0.0.10","ssh_port":2222,"bandwidth_mbps":200,"data_size_gib":2048}}}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if inst, _ := s.Instance("ins-aaaaaaaa"); inst.Cluster != nil || inst.ClusterRuntime != nil || inst.ClusterNote != "" {
		t.Fatalf("old instance = %+v", inst)
	}
	spec := &clusterspec.Spec{ClusterID: "clu-abcdefghij", Version: 2, Size: 2, Rank: 1, Mode: clusterspec.ModeRouted, Iface: "eth0",
		Subnet: "10.210.3.0/24", Members: []clusterspec.Member{{Rank: 0, InstanceID: "ins-bbbbbbbb", IP: "10.210.3.11", HostIP: "10.0.0.11"}, {Rank: 1, InstanceID: "ins-cccccccc", IP: "10.210.3.12"}}}
	in := state.Instance{ID: "ins-cccccccc", Desired: "running", Cluster: spec, ClusterRuntime: &clusterspec.Runtime{HCA: "rocep1s0f1", GIDIndex: 3, RDMAActive: true}, ClusterNote: "RDMA 变更需重建实例"}
	if err := s.PutInstance(in); err != nil {
		t.Fatal(err)
	}
	spec.Members[0].IP = "mutated" // 调用方改自己的副本不影响 Store
	s2, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := s2.Instance("ins-cccccccc")
	if !ok || got.Cluster == nil || got.Cluster.Version != 2 || got.Cluster.Members[0].IP != "10.210.3.11" || got.Cluster.Members[0].HostIP != "10.0.0.11" ||
		got.ClusterRuntime == nil || *got.ClusterRuntime != (clusterspec.Runtime{HCA: "rocep1s0f1", GIDIndex: 3, RDMAActive: true}) || got.ClusterNote != "RDMA 变更需重建实例" {
		t.Fatalf("reopened = %+v %+v", got, got.Cluster)
	}
	got.Cluster.Members[0].IP = "mutated"
	got.ClusterRuntime.GIDIndex = 9
	if again, _ := s2.Instance("ins-cccccccc"); again.Cluster.Members[0].IP != "10.210.3.11" || again.ClusterRuntime.GIDIndex != 3 {
		t.Fatalf("store aliased caller copy: %+v", again.Cluster)
	}
	for _, i := range s2.Instances() {
		if i.Cluster != nil {
			i.Cluster.Members[1].IP = "mutated"
		}
	}
	if again, _ := s2.Instance("ins-cccccccc"); again.Cluster.Members[1].IP != "10.210.3.12" {
		t.Fatal("Instances() aliased store")
	}
}

// LanIPs：状态文件里实例记录的管理地址（去重、非空、有序），Agent 重启后 Probe 之前供集群网络认出管理网卡（终审 B-11）。
func TestStoreLanIPs(t *testing.T) {
	s, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := s.LanIPs(); len(got) != 0 {
		t.Fatalf("empty = %v", got)
	}
	for _, i := range []state.Instance{{ID: "ins-bbbbbbbb", LanIP: "192.168.1.50"}, {ID: "ins-aaaaaaaa", LanIP: "192.168.1.50"},
		{ID: "ins-cccccccc", LanIP: ""}, {ID: "ins-dddddddd", LanIP: "10.0.0.9"}} {
		if err := s.PutInstance(i); err != nil {
			t.Fatal(err)
		}
	}
	if got := fmt.Sprint(s.LanIPs()); got != "[10.0.0.9 192.168.1.50]" {
		t.Fatalf("LanIPs = %s", got)
	}
}
