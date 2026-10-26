package fake_test

import (
	"context"
	"errors"
	"testing"

	"spark-cloud/internal/agent/clusternet"
	"spark-cloud/internal/agent/clusternet/fake"
	"spark-cloud/internal/clusterspec"
)

var _ clusternet.Net = (*fake.Net)(nil)

func spec(rdma bool) clusterspec.Spec {
	return clusterspec.Spec{ClusterID: "clu-aaaaaaaaaa", Version: 1, Size: 2, Rank: 0, Mode: clusterspec.ModeRouted,
		Iface: "eth0", Subnet: "10.210.0.0/24", RDMA: rdma, Members: []clusterspec.Member{
			{Rank: 0, InstanceID: "ins-aaaaaaaa", IP: "10.210.0.11"},
			{Rank: 1, InstanceID: "ins-bbbbbbbb", IP: "10.210.0.12", HostIP: "10.0.0.2"},
		}}
}

func TestFakeRecordsCalls(t *testing.T) {
	ctx := context.Background()
	n := fake.New()
	n.SetInfo(clusterspec.NetInfo{LanIface: "eth0", Ifaces: []clusterspec.Iface{{Name: "eth0", Up: true}}})
	info, err := n.Probe(ctx, "10.0.0.1")
	if err != nil || info.LanIface != "eth0" || len(info.Ifaces) != 1 {
		t.Fatalf("Probe = %+v, %v", info, err)
	}
	info.Ifaces[0].Name = "mutated" // 返回的是副本
	if again, _ := n.Probe(ctx, ""); again.Ifaces[0].Name != "eth0" {
		t.Fatal("Probe returned shared slice")
	}

	if devs, err := n.Devices(spec(false)); err != nil || devs != nil {
		t.Fatalf("Devices(non-rdma) = %v, %v", devs, err)
	}
	n.SetDevices([]string{"/dev/infiniband/rdma_cm", "/dev/infiniband/uverbs1"})
	if devs, err := n.Devices(spec(true)); err != nil || len(devs) != 2 {
		t.Fatalf("Devices(rdma) = %v, %v", devs, err)
	}

	n.SetRuntime(clusterspec.Runtime{HCA: "rocep1s0f1", GIDIndex: 3, RDMAActive: true})
	rt, err := n.Apply(ctx, "ins-aaaaaaaa", 42, spec(true))
	if err != nil || rt.GIDIndex != 3 {
		t.Fatalf("Apply = %+v, %v", rt, err)
	}
	if ok, err := n.Check(ctx, "ins-aaaaaaaa", 42, spec(true)); !ok || err != nil {
		t.Fatalf("Check default = %v, %v", ok, err)
	}
	n.SetCheckResult(false)
	if ok, err := n.Check(ctx, "ins-aaaaaaaa", 42, spec(true)); ok || !errors.Is(err, fake.ErrCheckFailed) {
		t.Fatalf("Check after SetCheckResult(false) = %v, %v", ok, err)
	}
	if err := n.Teardown(ctx, "ins-aaaaaaaa", spec(false), nil); err != nil {
		t.Fatal(err)
	}
	if err := n.SyncRules(ctx, map[string]clusterspec.Spec{"ins-aaaaaaaa": spec(false)}); err != nil {
		t.Fatal(err)
	}

	applies := n.Applies()
	if len(applies) != 1 || applies[0].InstanceID != "ins-aaaaaaaa" || applies[0].PID != 42 || !applies[0].Spec.RDMA {
		t.Fatalf("Applies = %+v", applies)
	}
	if td := n.Teardowns(); len(td) != 1 || td[0] != "ins-aaaaaaaa" {
		t.Fatalf("Teardowns = %v", td)
	}
	rules := n.Rules()
	if len(rules) != 1 || rules["ins-aaaaaaaa"].ClusterID != "clu-aaaaaaaaaa" {
		t.Fatalf("Rules = %v", rules)
	}
	rules["x"] = clusterspec.Spec{} // 返回的是副本
	if len(n.Rules()) != 1 {
		t.Fatal("Rules returned shared map")
	}
	if n.Checks() != 2 || n.SyncCount() != 1 {
		t.Fatalf("Checks = %d, SyncCount = %d", n.Checks(), n.SyncCount())
	}
	// SyncRules 记录最近一次
	if err := n.SyncRules(ctx, map[string]clusterspec.Spec{}); err != nil || len(n.Rules()) != 0 {
		t.Fatalf("second SyncRules: %v %v", n.Rules(), err)
	}
}

func TestFakeFailureInjection(t *testing.T) {
	ctx := context.Background()
	n := fake.New()
	boom := errors.New("boom")

	n.SetFailApply(clusternet.ErrNoGID)
	n.SetRuntime(clusterspec.Runtime{HCA: "rocep1s0f1"})
	rt, err := n.Apply(ctx, "ins-aaaaaaaa", 1, spec(true))
	if !errors.Is(err, clusternet.ErrNoGID) || rt.RDMAActive || rt.HCA != "rocep1s0f1" {
		t.Fatalf("Apply with ErrNoGID = %+v, %v", rt, err)
	}
	if len(n.Applies()) != 1 {
		t.Fatal("failed Apply not recorded")
	}
	n.SetFailApply(nil)
	if _, err := n.Apply(ctx, "ins-aaaaaaaa", 1, spec(true)); err != nil {
		t.Fatalf("Apply after clearing failure: %v", err)
	}

	n.SetFailCheck(boom)
	if ok, err := n.Check(ctx, "ins-aaaaaaaa", 1, spec(false)); ok || !errors.Is(err, boom) {
		t.Fatalf("Check = %v, %v", ok, err)
	}
	n.SetFailTeardown(boom)
	if err := n.Teardown(ctx, "ins-aaaaaaaa", spec(false), nil); !errors.Is(err, boom) {
		t.Fatalf("Teardown = %v", err)
	}
	n.SetFailDevices(boom)
	if _, err := n.Devices(spec(true)); !errors.Is(err, boom) {
		t.Fatalf("Devices = %v", err)
	}
	n.SetFailProbe(boom)
	if _, err := n.Probe(ctx, ""); !errors.Is(err, boom) {
		t.Fatalf("Probe = %v", err)
	}
	n.SetFailSyncRules(boom)
	if err := n.SyncRules(ctx, nil); !errors.Is(err, boom) {
		t.Fatalf("SyncRules = %v", err)
	}
}
