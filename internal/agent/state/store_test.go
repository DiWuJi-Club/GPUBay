package state_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"spark-cloud/internal/agent/state"
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
