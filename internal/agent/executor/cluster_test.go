package executor_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"spark-cloud/internal/agent/clusternet"
	clusternetfake "spark-cloud/internal/agent/clusternet/fake"
	"spark-cloud/internal/agent/executor"
	"spark-cloud/internal/clusterspec"
)

type clusterEnv struct {
	*env
	cn *clusternetfake.Net
}

// newClusterEnv：与 newEnv 相同，另接 fake 集群数据面（Deps.Cluster）。
func newClusterEnv(t *testing.T) *clusterEnv {
	t.Helper()
	e := newEnv(t)
	cn := clusternetfake.New()
	e.ex = executor.New(executor.Deps{Log: quiet, Runtime: e.rt, Disk: e.disk, Net: e.net, Cluster: cn, State: e.st, StateDir: e.dir, Bridge: "spark0",
		LocalIPs: func() []string { return []string{"127.0.0.1", "192.168.1.221"} }, MemTotal: func() uint64 { return 128 << 30 },
		NumCPU: func() int { return 20 }, CgroupRoot: filepath.Join(e.dir, "cgroup")}, true)
	e.ex.SetConfig(executor.Config{LanIP: "192.168.1.221", DNS: []string{"223.5.5.5"}, AllowCIDRs: []string{"10.0.9.0/24"}, BandwidthMbps: 200, DataSizeGiB: 2048})
	return &clusterEnv{env: e, cn: cn}
}

// routedSpec：两台 lan 集群，self 为 rank 的成员（rank 0 = ins-aaaaaaaa，rank 1 = ins-bbbbbbbb）。
func routedSpec(rank, version int) clusterspec.Spec {
	return clusterspec.Spec{ClusterID: "clu-abcdefghij", Version: version, Size: 2, Rank: rank, Mode: clusterspec.ModeRouted, Iface: "enP7s7",
		Subnet: "10.210.3.0/24", Members: []clusterspec.Member{
			{Rank: 0, InstanceID: "ins-aaaaaaaa", IP: "10.210.3.11", HostIP: "192.168.1.221"},
			{Rank: 1, InstanceID: "ins-bbbbbbbb", IP: "10.210.3.12", HostIP: "192.168.1.222"}}}
}

// otherRoutedSpec：本机另一个集群的成员 ins-dddddddd（rank 0）。
func otherRoutedSpec() clusterspec.Spec {
	return clusterspec.Spec{ClusterID: "clu-zzzzzzzzzz", Version: 1, Size: 2, Rank: 0, Mode: clusterspec.ModeRouted, Iface: "enP7s7",
		Subnet: "10.210.4.0/24", Members: []clusterspec.Member{
			{Rank: 0, InstanceID: "ins-dddddddd", IP: "10.210.4.11", HostIP: "192.168.1.221"},
			{Rank: 1, InstanceID: "ins-eeeeeeee", IP: "10.210.4.12", HostIP: "192.168.1.223"}}}
}

func macvlanSpec(rdma bool, version int) clusterspec.Spec {
	return clusterspec.Spec{ClusterID: "clu-abcdefghij", Version: version, Size: 2, Rank: 0, Mode: clusterspec.ModeMacvlan, Iface: "enp1s0f1np1",
		Subnet: "10.210.3.0/24", RDMA: rdma, Members: []clusterspec.Member{
			{Rank: 0, InstanceID: "ins-aaaaaaaa", IP: "10.210.3.11"},
			{Rank: 1, InstanceID: "ins-bbbbbbbb", IP: "10.210.3.12"}}}
}

func createCluster(id string, port int, s clusterspec.Spec) executor.Command {
	c := create(id)
	c.SSHPort, c.DataSizeGiB = port, 64 // 一台机器上放几个实例（fake 盘 4000 GiB）
	c.Cluster = &s
	return c
}

func plainCreate(id string, port int) executor.Command {
	c := create(id)
	c.SSHPort, c.DataSizeGiB = port, 64
	return c
}

func configure(id, cmdID string, s clusterspec.Spec) executor.Command {
	return executor.Command{ID: cmdID, InstanceID: id, Type: "configure_cluster", Cluster: &s}
}

func (e *clusterEnv) mustOK(cmds ...executor.Command) {
	e.t.Helper()
	for _, r := range e.run(cmds...) {
		if !r.OK {
			e.t.Fatalf("command %s failed: %s", r.ID, r.Message)
		}
	}
}

func (e *clusterEnv) clusterStates() map[string]string {
	out := map[string]string{}
	for _, s := range e.ex.Report(context.Background()) {
		out[s.InstanceID] = s.Cluster
	}
	return out
}

func (e *clusterEnv) file(id, name string) string {
	b, err := os.ReadFile(filepath.Join(e.dir, "ssh", id, name))
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(b)
}

func (e *clusterEnv) ensure() { executor.EnsureClustersForTest(e.ex, context.Background()) }

func TestCreateClusterDropsNetRaw(t *testing.T) {
	e := newClusterEnv(t)
	e.mustOK(createCluster("ins-aaaaaaaa", 2222, routedSpec(0, 1)), plainCreate("ins-cccccccc", 2224))
	sp, _ := e.rt.Spec("ins-aaaaaaaa")
	if !slices.Equal(sp.CapDrop, []string{"NET_RAW"}) || len(sp.Sysctls) != 1 || sp.Sysctls["net.ipv4.ping_group_range"] != "0 2147483647" || sp.Devices != nil {
		t.Fatalf("cluster spec = %+v", sp)
	}
	// 非集群实例：三项都为空（docker 命令行与改动前逐字相同，见 docker_test）
	plain, _ := e.rt.Spec("ins-cccccccc")
	if plain.CapDrop != nil || plain.Sysctls != nil || plain.Devices != nil {
		t.Fatalf("plain spec = %+v", plain)
	}

	// RDMA：--device 来自 Net.Devices
	r := newClusterEnv(t)
	r.cn.SetDevices([]string{"/dev/infiniband/rdma_cm", "/dev/infiniband/uverbs1"})
	r.cn.SetRuntime(clusterspec.Runtime{HCA: "rocep1s0f1", GIDIndex: 3, RDMAActive: true})
	r.mustOK(createCluster("ins-aaaaaaaa", 2222, macvlanSpec(true, 1)))
	sp, _ = r.rt.Spec("ins-aaaaaaaa")
	if !slices.Equal(sp.Devices, []string{"/dev/infiniband/rdma_cm", "/dev/infiniband/uverbs1"}) || !slices.Equal(sp.CapDrop, []string{"NET_RAW"}) {
		t.Fatalf("rdma spec = %+v", sp)
	}
}

func TestCreateClusterWritesFilesAndApplies(t *testing.T) {
	e := newClusterEnv(t)
	s := routedSpec(0, 1)
	e.mustOK(createCluster("ins-aaaaaaaa", 2222, s))
	if got, want := e.file("ins-aaaaaaaa", "cluster.json"), string(clusterspec.RenderJSON(s, clusterspec.Runtime{})); got != want {
		t.Fatalf("cluster.json = %s\nwant %s", got, want)
	}
	if got, want := e.file("ins-aaaaaaaa", "cluster.env"), string(clusterspec.RenderEnv(s, clusterspec.Runtime{})); got != want {
		t.Fatalf("cluster.env = %s\nwant %s", got, want)
	}
	if st, err := os.Stat(filepath.Join(e.dir, "ssh", "ins-aaaaaaaa", "cluster.env")); err != nil || st.Mode().Perm() != 0o644 {
		t.Fatalf("cluster.env perm = %v %v", st, err)
	}
	ap := e.cn.Applies()
	if len(ap) != 1 || ap[0].InstanceID != "ins-aaaaaaaa" || ap[0].PID != 4242 || ap[0].Spec.Version != 1 || len(ap[0].Spec.Members) != 2 {
		t.Fatalf("applies = %+v", ap)
	}
	if _, ok := e.cn.Rules()["ins-aaaaaaaa"]; !ok {
		t.Fatalf("rules = %+v", e.cn.Rules())
	}
	inst, _ := e.st.Instance("ins-aaaaaaaa")
	if inst.Cluster == nil || inst.Cluster.ClusterID != "clu-abcdefghij" || inst.ClusterRuntime == nil {
		t.Fatalf("state = %+v", inst)
	}
	if cs := e.clusterStates(); cs["ins-aaaaaaaa"] != "ok" {
		t.Fatalf("cluster states = %v", cs)
	}

	// macvlan 不进 SPARK-CLU；RDMA 的运行时（GID）写进 cluster.env / cluster.json
	m := newClusterEnv(t)
	rt := clusterspec.Runtime{HCA: "rocep1s0f1", GIDIndex: 3, RDMAActive: true}
	m.cn.SetDevices([]string{"/dev/infiniband/rdma_cm", "/dev/infiniband/uverbs1"})
	m.cn.SetRuntime(rt)
	ms := macvlanSpec(true, 1)
	m.mustOK(createCluster("ins-aaaaaaaa", 2222, ms))
	if got, want := m.file("ins-aaaaaaaa", "cluster.env"), string(clusterspec.RenderEnv(ms, rt)); got != want || !strings.Contains(got, "export NCCL_IB_GID_INDEX='3'\n") {
		t.Fatalf("rdma env = %s", got)
	}
	if got := m.file("ins-aaaaaaaa", "cluster.json"); got != string(clusterspec.RenderJSON(ms, rt)) {
		t.Fatalf("rdma json = %s", got)
	}
	if _, ok := m.cn.Rules()["ins-aaaaaaaa"]; ok {
		t.Fatalf("macvlan member must not be in SPARK-CLU: %+v", m.cn.Rules())
	}
	if inst, _ := m.st.Instance("ins-aaaaaaaa"); inst.ClusterRuntime == nil || *inst.ClusterRuntime != rt {
		t.Fatalf("runtime not persisted: %+v", inst.ClusterRuntime)
	}
}

func TestCreateClusterApplyFailureFailsCreate(t *testing.T) {
	e := newClusterEnv(t)
	e.mustOK(createCluster("ins-dddddddd", 2224, otherRoutedSpec())) // 本机另一个集群成员：清理后它的规则仍在
	e.cn.SetFailApply(errors.New("nsenter: cannot open /proc/4242/ns/net"))
	res := e.run(createCluster("ins-aaaaaaaa", 2222, routedSpec(0, 1)))
	if len(res) != 1 || res[0].OK || !strings.HasPrefix(res[0].Message, clusterspec.ErrPrefix) || !strings.Contains(res[0].Message, "nsenter") {
		t.Fatalf("result = %+v", res)
	}
	if !slices.Contains(e.cn.Teardowns(), "ins-aaaaaaaa") {
		t.Fatalf("teardowns = %v", e.cn.Teardowns())
	}
	rules := e.cn.Rules()
	if _, ok := rules["ins-aaaaaaaa"]; ok {
		t.Fatalf("failed member still in rules: %+v", rules)
	}
	if _, ok := rules["ins-dddddddd"]; !ok {
		t.Fatalf("other member dropped from rules: %+v", rules)
	}
	if c := e.rt.Containers()["ins-aaaaaaaa"]; c.Running {
		t.Fatal("container left running after cluster apply failure")
	}
	if inst, _ := e.st.Instance("ins-aaaaaaaa"); inst.Desired != "stopped" {
		t.Fatalf("desired = %q", inst.Desired)
	}

	// Devices 失败、Spec 非法、Agent 未接集群数据面：同样以 cluster-net: 开头
	d := newClusterEnv(t)
	d.cn.SetFailDevices(errors.New("uverbs device for rocep1s0f1 not found"))
	if res := d.run(createCluster("ins-aaaaaaaa", 2222, macvlanSpec(true, 1))); res[0].OK || !strings.HasPrefix(res[0].Message, clusterspec.ErrPrefix) {
		t.Fatalf("devices failure = %+v", res)
	}
	if len(d.rt.Containers()) != 0 {
		t.Fatalf("container created despite device failure: %v", d.rt.Containers())
	}
	bad := routedSpec(0, 1)
	bad.Subnet = "10.210.3.0/16"
	if res := d.run(createCluster("ins-aaaaaaaa", 2222, bad)); res[0].OK || !strings.HasPrefix(res[0].Message, clusterspec.ErrPrefix) {
		t.Fatalf("invalid spec = %+v", res)
	}
	if res := d.run(createCluster("ins-bbbbbbbb", 2222, routedSpec(0, 1))); res[0].OK || !strings.HasPrefix(res[0].Message, clusterspec.ErrPrefix) {
		t.Fatalf("spec for another instance accepted: %+v", res)
	}
	plain := newEnv(t) // Deps.Cluster == nil
	if res := plain.run(createCluster("ins-aaaaaaaa", 2222, routedSpec(0, 1))); res[0].OK || !strings.HasPrefix(res[0].Message, clusterspec.ErrPrefix) {
		t.Fatalf("unsupported = %+v", res)
	}
}

func TestStartApplyFailureKeepsRunning(t *testing.T) {
	e := newClusterEnv(t)
	e.mustOK(createCluster("ins-aaaaaaaa", 2222, routedSpec(0, 1)), executor.Command{ID: "stop-1", InstanceID: "ins-aaaaaaaa", Type: "stop_instance"})
	if cs := e.clusterStates(); cs["ins-aaaaaaaa"] != "pending" {
		t.Fatalf("stopped member = %v", cs)
	}
	e.cn.SetFailApply(errors.New("ip link add failed"))
	e.mustOK(executor.Command{ID: "start-1", InstanceID: "ins-aaaaaaaa", Type: "start_instance"})
	if !e.rt.Containers()["ins-aaaaaaaa"].Running {
		t.Fatal("start must keep the container running when cluster apply fails")
	}
	if cs := e.clusterStates(); cs["ins-aaaaaaaa"] != "error: ip link add failed" {
		t.Fatalf("cluster states = %v", cs)
	}
	if len(e.cn.Teardowns()) != 0 {
		t.Fatalf("start path must not tear down: %v", e.cn.Teardowns())
	}
}

func TestNoGIDFallsBackToTCP(t *testing.T) {
	e := newClusterEnv(t)
	e.cn.SetDevices([]string{"/dev/infiniband/rdma_cm", "/dev/infiniband/uverbs1"})
	e.cn.SetRuntime(clusterspec.Runtime{HCA: "rocep1s0f1", RDMAActive: false})
	e.cn.SetFailApply(clusternet.ErrNoGID)
	e.mustOK(createCluster("ins-aaaaaaaa", 2222, macvlanSpec(true, 1)))
	env := e.file("ins-aaaaaaaa", "cluster.env")
	if !strings.Contains(env, "export NCCL_IB_DISABLE='1'\n") || strings.Contains(env, "NCCL_IB_GID_INDEX") || !strings.Contains(env, "SPARK_CLUSTER_NETWORK='ethernet'") {
		t.Fatalf("env = %s", env)
	}
	if !strings.Contains(e.file("ins-aaaaaaaa", "cluster.json"), `"rdma": null`) {
		t.Fatalf("json = %s", e.file("ins-aaaaaaaa", "cluster.json"))
	}
	if cs := e.clusterStates(); cs["ins-aaaaaaaa"] != "error: 未找到 RoCE GID，已退回 TCP" {
		t.Fatalf("cluster states = %v", cs)
	}
	if len(e.cn.Teardowns()) != 0 || !e.rt.Containers()["ins-aaaaaaaa"].Running {
		t.Fatal("no-GID is a fallback, not a failure")
	}
}

// linux 实现把「设备编号变化 / 网卡无 RDMA / 未找到 GID」都包装成 ErrNoGID：同样退回 TCP，状态文案取错误本身。
func TestNoGIDStatusUsesApplyError(t *testing.T) {
	e := newClusterEnv(t)
	e.cn.SetDevices([]string{"/dev/infiniband/rdma_cm", "/dev/infiniband/uverbs1"})
	e.cn.SetRuntime(clusterspec.Runtime{HCA: "rocep1s0f1"})
	e.cn.SetFailApply(fmt.Errorf("%sRDMA 设备编号变化，需重建实例：%w", clusterspec.ErrPrefix, clusternet.ErrNoGID))
	e.mustOK(createCluster("ins-aaaaaaaa", 2222, macvlanSpec(true, 1)))
	if cs := e.clusterStates(); cs["ins-aaaaaaaa"] != "error: RDMA 设备编号变化，需重建实例" {
		t.Fatalf("cluster states = %v", cs)
	}
	if env := e.file("ins-aaaaaaaa", "cluster.env"); !strings.Contains(env, "export NCCL_IB_DISABLE='1'\n") {
		t.Fatalf("env = %s", env)
	}
}

// macvlan 成员运行中退出集群：拒绝（容器内 spc0 删不掉），state 不变；停机后可以退出。
func TestLeaveClusterMacvlanRunningRefused(t *testing.T) {
	e := newClusterEnv(t)
	e.mustOK(createCluster("ins-aaaaaaaa", 2222, macvlanSpec(false, 1)))
	res := e.run(configure("ins-aaaaaaaa", "cfg-leave", clusterspec.Spec{}))
	if res[0].OK || !strings.HasPrefix(res[0].Message, clusterspec.ErrPrefix) {
		t.Fatalf("leave while running = %+v", res)
	}
	if inst, _ := e.st.Instance("ins-aaaaaaaa"); inst.Cluster == nil || len(e.cn.Teardowns()) != 0 {
		t.Fatalf("state changed: %+v teardowns=%v", inst.Cluster, e.cn.Teardowns())
	}
	e.mustOK(executor.Command{ID: "stop-1", InstanceID: "ins-aaaaaaaa", Type: "stop_instance"}, configure("ins-aaaaaaaa", "cfg-leave-2", clusterspec.Spec{}))
	if inst, _ := e.st.Instance("ins-aaaaaaaa"); inst.Cluster != nil || !slices.Equal(e.cn.Teardowns(), []string{"ins-aaaaaaaa"}) {
		t.Fatalf("after stop: %+v teardowns=%v", inst.Cluster, e.cn.Teardowns())
	}
}

func TestConfigureClusterVersioning(t *testing.T) {
	e := newClusterEnv(t)
	e.mustOK(createCluster("ins-aaaaaaaa", 2222, routedSpec(0, 2)))
	// 新版本（rank 1 换机到 ins-cccccccc）：存 state、重写文件、差量 Apply、同步规则
	v3 := routedSpec(0, 3)
	v3.Members[1] = clusterspec.Member{Rank: 1, InstanceID: "ins-cccccccc", IP: "10.210.3.12", HostIP: "192.168.1.230"}
	e.mustOK(configure("ins-aaaaaaaa", "cfg-3", v3))
	ap := e.cn.Applies()
	if len(ap) != 2 || ap[1].Spec.Version != 3 || ap[1].Spec.Members[1].InstanceID != "ins-cccccccc" {
		t.Fatalf("applies = %+v", ap)
	}
	if r := e.cn.Rules()["ins-aaaaaaaa"]; r.Version != 3 {
		t.Fatalf("rules = %+v", e.cn.Rules())
	}
	if inst, _ := e.st.Instance("ins-aaaaaaaa"); inst.Cluster.Version != 3 {
		t.Fatalf("state version = %d", inst.Cluster.Version)
	}
	if got := e.file("ins-aaaaaaaa", "cluster.json"); !strings.Contains(got, "ins-cccccccc") || !strings.Contains(got, `"version": 3`) {
		t.Fatalf("json = %s", got)
	}
	// 旧版本：忽略（成功），不 Apply、不改 state
	e.mustOK(configure("ins-aaaaaaaa", "cfg-1", routedSpec(0, 1)))
	if len(e.cn.Applies()) != 2 {
		t.Fatalf("old version applied: %+v", e.cn.Applies())
	}
	if inst, _ := e.st.Instance("ins-aaaaaaaa"); inst.Cluster.Version != 3 {
		t.Fatalf("old version stored: %d", inst.Cluster.Version)
	}
	// 本地没有的实例：忽略（成功）
	e.mustOK(configure("ins-zzzzzzzz", "cfg-x", routedSpec(0, 4)))
	// 别的集群的 Spec：拒绝
	other := otherRoutedSpec()
	other.Members[0].InstanceID = "ins-aaaaaaaa"
	if res := e.run(configure("ins-aaaaaaaa", "cfg-o", other)); res[0].OK || !strings.HasPrefix(res[0].Message, clusterspec.ErrPrefix) {
		t.Fatalf("foreign cluster = %+v", res)
	}
	// 退出集群：Teardown + 删文件 + 清 state + 规则里不再有它
	e.mustOK(configure("ins-aaaaaaaa", "cfg-leave", clusterspec.Spec{}))
	if !slices.Equal(e.cn.Teardowns(), []string{"ins-aaaaaaaa"}) {
		t.Fatalf("teardowns = %v", e.cn.Teardowns())
	}
	for _, f := range []string{"cluster.json", "cluster.env"} {
		if _, err := os.Stat(filepath.Join(e.dir, "ssh", "ins-aaaaaaaa", f)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s not removed: %v", f, err)
		}
	}
	if inst, _ := e.st.Instance("ins-aaaaaaaa"); inst.Cluster != nil || inst.ClusterRuntime != nil {
		t.Fatalf("state not cleared: %+v", inst)
	}
	if _, ok := e.cn.Rules()["ins-aaaaaaaa"]; ok {
		t.Fatalf("rules = %+v", e.cn.Rules())
	}
	if cs := e.clusterStates(); cs["ins-aaaaaaaa"] != "" {
		t.Fatalf("left member status = %q", cs["ins-aaaaaaaa"])
	}
	if !e.rt.Containers()["ins-aaaaaaaa"].Running {
		t.Fatal("leaving the cluster must not stop the instance")
	}
}

func TestConfigureClusterStoppedInstance(t *testing.T) {
	e := newClusterEnv(t)
	e.mustOK(createCluster("ins-aaaaaaaa", 2222, routedSpec(0, 1)), executor.Command{ID: "stop-1", InstanceID: "ins-aaaaaaaa", Type: "stop_instance"})
	e.mustOK(configure("ins-aaaaaaaa", "cfg-2", routedSpec(0, 2)))
	if len(e.cn.Applies()) != 1 {
		t.Fatalf("stopped instance applied: %+v", e.cn.Applies())
	}
	if got := e.file("ins-aaaaaaaa", "cluster.json"); !strings.Contains(got, `"version": 2`) {
		t.Fatalf("json = %s", got)
	}
	if cs := e.clusterStates(); cs["ins-aaaaaaaa"] != "pending" {
		t.Fatalf("cluster states = %v", cs)
	}
	// 下次启动按新版本 Apply
	e.mustOK(executor.Command{ID: "start-1", InstanceID: "ins-aaaaaaaa", Type: "start_instance"})
	if ap := e.cn.Applies(); len(ap) != 2 || ap[1].Spec.Version != 2 {
		t.Fatalf("applies = %+v", ap)
	}
}

func TestConfigureClusterRDMAFlip(t *testing.T) {
	// 无 → 有：设备只能在 create 时给，文件按 RDMA=false 渲染，状态提示重建
	e := newClusterEnv(t)
	e.mustOK(createCluster("ins-aaaaaaaa", 2222, macvlanSpec(false, 1)))
	e.cn.SetRuntime(clusterspec.Runtime{HCA: "rocep1s0f1", GIDIndex: 3, RDMAActive: true})
	e.mustOK(configure("ins-aaaaaaaa", "cfg-2", macvlanSpec(true, 2)))
	if env := e.file("ins-aaaaaaaa", "cluster.env"); !strings.Contains(env, "export NCCL_IB_DISABLE='1'\n") {
		t.Fatalf("env = %s", env)
	}
	if inst, _ := e.st.Instance("ins-aaaaaaaa"); inst.Cluster.RDMA || inst.Cluster.Version != 2 {
		t.Fatalf("state = %+v", inst.Cluster)
	}
	if ap := e.cn.Applies(); ap[len(ap)-1].Spec.RDMA {
		t.Fatal("applied RDMA without devices")
	}
	if cs := e.clusterStates(); cs["ins-aaaaaaaa"] != "error: RDMA 变更需重建实例" {
		t.Fatalf("cluster states = %v", cs)
	}
	e.ensure() // 巡检不会把提示冲掉
	if cs := e.clusterStates(); cs["ins-aaaaaaaa"] != "error: RDMA 变更需重建实例" {
		t.Fatalf("after ensure = %v", cs)
	}

	// 有 → 无：退回 TCP，状态 ok
	r := newClusterEnv(t)
	r.cn.SetDevices([]string{"/dev/infiniband/rdma_cm", "/dev/infiniband/uverbs1"})
	r.cn.SetRuntime(clusterspec.Runtime{HCA: "rocep1s0f1", GIDIndex: 3, RDMAActive: true})
	r.mustOK(createCluster("ins-aaaaaaaa", 2222, macvlanSpec(true, 1)))
	r.cn.SetRuntime(clusterspec.Runtime{})
	r.mustOK(configure("ins-aaaaaaaa", "cfg-2", macvlanSpec(false, 2)))
	if env := r.file("ins-aaaaaaaa", "cluster.env"); !strings.Contains(env, "export NCCL_IB_DISABLE='1'\n") {
		t.Fatalf("env = %s", env)
	}
	if cs := r.clusterStates(); cs["ins-aaaaaaaa"] != "ok" {
		t.Fatalf("cluster states = %v", cs)
	}

	// 模式 / 网卡变更：只更新文件，不按新模式 Apply，状态提示重建
	m := newClusterEnv(t)
	m.mustOK(createCluster("ins-aaaaaaaa", 2222, macvlanSpec(false, 1)))
	moved := macvlanSpec(false, 2)
	moved.Iface = "enp1s0f0np0"
	m.mustOK(configure("ins-aaaaaaaa", "cfg-2", moved))
	if ap := m.cn.Applies(); len(ap) != 1 {
		t.Fatalf("applied with a different parent: %+v", ap)
	}
	if got := m.file("ins-aaaaaaaa", "cluster.json"); !strings.Contains(got, `"version": 2`) {
		t.Fatalf("json = %s", got)
	}
	if cs := m.clusterStates(); cs["ins-aaaaaaaa"] != "error: 集群网络模式变更需重建实例" {
		t.Fatalf("cluster states = %v", cs)
	}
}

func TestEnsureClustersReapplies(t *testing.T) {
	e := newClusterEnv(t)
	e.mustOK(createCluster("ins-aaaaaaaa", 2222, routedSpec(0, 1)))
	e.ensure() // Check 通过、上轮 ok：不 Apply
	if n := len(e.cn.Applies()); n != 1 || e.cn.Checks() != 1 {
		t.Fatalf("applies=%d checks=%d", n, e.cn.Checks())
	}
	e.cn.SetCheckResult(false)
	e.ensure() // Check 不通过 → 重新 Apply；本轮如实回写问题（平台据此告警）
	if n := len(e.cn.Applies()); n != 2 {
		t.Fatalf("applies = %d", n)
	}
	if cs := e.clusterStates(); cs["ins-aaaaaaaa"] != "error: "+clusternetfake.ErrCheckFailed.Error() {
		t.Fatalf("cluster states = %v", cs)
	}
	e.cn.SetCheckResult(true)
	e.ensure() // 上轮 error → 再 Apply 一次，Check 通过 → ok
	if n := len(e.cn.Applies()); n != 3 {
		t.Fatalf("applies = %d", n)
	}
	if cs := e.clusterStates(); cs["ins-aaaaaaaa"] != "ok" {
		t.Fatalf("cluster states = %v", cs)
	}
	e.ensure()
	if n := len(e.cn.Applies()); n != 3 {
		t.Fatalf("steady state re-applied: %d", n)
	}
}

func TestEnsureClustersRetriesAfterError(t *testing.T) {
	e := newClusterEnv(t)
	e.mustOK(createCluster("ins-aaaaaaaa", 2222, routedSpec(0, 1)), executor.Command{ID: "stop-1", InstanceID: "ins-aaaaaaaa", Type: "stop_instance"})
	e.cn.SetFailApply(errors.New("transient"))
	e.mustOK(executor.Command{ID: "start-1", InstanceID: "ins-aaaaaaaa", Type: "start_instance"})
	if cs := e.clusterStates(); cs["ins-aaaaaaaa"] != "error: transient" {
		t.Fatalf("cluster states = %v", cs)
	}
	e.cn.SetFailApply(nil)
	before := len(e.cn.Applies())
	e.ensure() // Check 为 true，但上轮是 error → 仍重跑 Apply
	if len(e.cn.Applies()) != before+1 {
		t.Fatalf("applies = %d, want %d", len(e.cn.Applies()), before+1)
	}
	if cs := e.clusterStates(); cs["ins-aaaaaaaa"] != "ok" {
		t.Fatalf("cluster states = %v", cs)
	}
}

func TestEnsureClustersSkipsStoppedAndBusy(t *testing.T) {
	e := newClusterEnv(t)
	e.mustOK(createCluster("ins-aaaaaaaa", 2222, routedSpec(0, 1)))
	unlock := executor.LockOpsForTest(e.ex)
	e.ensure()
	unlock()
	if e.cn.Checks() != 0 {
		t.Fatal("ensure ran while an instance command held opMu")
	}
	e.rt.Exit("ins-aaaaaaaa") // 容器意外退出：不巡检、报 pending
	e.ensure()
	if e.cn.Checks() != 0 || len(e.cn.Applies()) != 1 {
		t.Fatalf("checks=%d applies=%d", e.cn.Checks(), len(e.cn.Applies()))
	}
	if cs := e.clusterStates(); cs["ins-aaaaaaaa"] != "pending" {
		t.Fatalf("cluster states = %v", cs)
	}
}

// Recover 对已在运行的容器重跑启动路径：Apply 收到同一 Spec（clusternet 保证差量、不重建 spc0），
// executor 不在 runtime 层做任何建 / 删容器或重接网络的动作。
func TestRecoverDoesNotRebuildLink(t *testing.T) {
	e := newClusterEnv(t)
	s := routedSpec(0, 1)
	e.mustOK(createCluster("ins-aaaaaaaa", 2222, s))
	opsBefore := len(e.rt.Ops())
	// 模拟 Agent 重启：新的执行器、同一状态文件，容器仍在运行
	e2 := executor.New(executor.Deps{Log: quiet, Runtime: e.rt, Disk: e.disk, Net: e.net, Cluster: e.cn, State: e.st, StateDir: e.dir, Bridge: "spark0",
		LocalIPs: func() []string { return []string{"192.168.1.221"} }, MemTotal: func() uint64 { return 128 << 30 }}, true)
	e2.SetConfig(e.ex.Config())
	for _, st := range e2.Report(context.Background()) {
		if st.InstanceID == "ins-aaaaaaaa" && st.Cluster != "pending" {
			t.Fatalf("before first apply after restart = %q", st.Cluster)
		}
	}
	if err := e2.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	ap := e.cn.Applies()
	if len(ap) != 2 || ap[1].InstanceID != "ins-aaaaaaaa" || ap[1].Spec.Version != s.Version || !slices.Equal(ap[1].Spec.Members, s.Members) {
		t.Fatalf("applies = %+v", ap)
	}
	for _, op := range e.rt.Ops()[opsBefore:] {
		if strings.HasPrefix(op, "create ") || strings.HasPrefix(op, "remove ") || strings.HasPrefix(op, "connect ") || strings.HasPrefix(op, "stop ") {
			t.Fatalf("recover touched the container: %v", e.rt.Ops()[opsBefore:])
		}
	}
	if len(e.cn.Teardowns()) != 0 {
		t.Fatalf("teardowns = %v", e.cn.Teardowns())
	}
	for _, st := range e2.Report(context.Background()) {
		if st.InstanceID == "ins-aaaaaaaa" && st.Cluster != "ok" {
			t.Fatalf("after recover = %q", st.Cluster)
		}
	}
}

func TestRecoverReappliesCluster(t *testing.T) {
	e := newClusterEnv(t)
	e.mustOK(createCluster("ins-aaaaaaaa", 2222, routedSpec(0, 1)))
	// 宿主机重启：容器停了、挂载丢了、文件被删（模拟）
	e.rt.Exit("ins-aaaaaaaa")
	e.disk.Unmount("ins-aaaaaaaa")
	_ = os.Remove(filepath.Join(e.dir, "ssh", "ins-aaaaaaaa", "cluster.env"))
	if err := e.ex.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !e.rt.Containers()["ins-aaaaaaaa"].Running || len(e.cn.Applies()) != 2 {
		t.Fatalf("running=%v applies=%d", e.rt.Containers()["ins-aaaaaaaa"].Running, len(e.cn.Applies()))
	}
	if got := e.file("ins-aaaaaaaa", "cluster.env"); !strings.HasPrefix(got, "export SPARK_CLUSTER_ID='clu-abcdefghij'\n") {
		t.Fatalf("env = %s", got)
	}
	if _, ok := e.cn.Rules()["ins-aaaaaaaa"]; !ok {
		t.Fatalf("rules = %+v", e.cn.Rules())
	}
}

func TestWipeTearsDownCluster(t *testing.T) {
	e := newClusterEnv(t)
	e.mustOK(createCluster("ins-aaaaaaaa", 2222, routedSpec(0, 1)), createCluster("ins-dddddddd", 2224, otherRoutedSpec()))
	e.cn.SetFailTeardown(errors.New("route del failed"))
	res := e.run(executor.Command{ID: "wipe-1", InstanceID: "ins-aaaaaaaa", Type: "wipe_instance"})
	if res[0].OK || !strings.Contains(res[0].Message, "route del failed") {
		t.Fatalf("wipe with teardown failure = %+v", res)
	}
	if _, ok := e.st.Instance("ins-aaaaaaaa"); !ok {
		t.Fatal("state deleted although teardown failed (retry would skip the cleanup)")
	}
	e.cn.SetFailTeardown(nil)
	e.mustOK(executor.Command{ID: "wipe-2", InstanceID: "ins-aaaaaaaa", Type: "wipe_instance"})
	if !slices.Equal(e.cn.Teardowns(), []string{"ins-aaaaaaaa", "ins-aaaaaaaa"}) {
		t.Fatalf("teardowns = %v", e.cn.Teardowns())
	}
	rules := e.cn.Rules()
	if _, ok := rules["ins-aaaaaaaa"]; ok {
		t.Fatalf("wiped member still in rules: %+v", rules)
	}
	if _, ok := rules["ins-dddddddd"]; !ok {
		t.Fatalf("other member dropped: %+v", rules)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "ssh", "ins-aaaaaaaa")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ssh dir = %v", err)
	}
	if _, ok := e.st.Instance("ins-aaaaaaaa"); ok {
		t.Fatal("state not deleted")
	}
	// SyncRules 失败同样计入洗机校验失败
	e.cn.SetFailSyncRules(errors.New("iptables-restore failed"))
	if res := e.run(executor.Command{ID: "wipe-3", InstanceID: "ins-dddddddd", Type: "wipe_instance"}); res[0].OK || !strings.Contains(res[0].Message, "iptables-restore failed") {
		t.Fatalf("wipe with sync failure = %+v", res)
	}
}

func TestReportClusterField(t *testing.T) {
	e := newClusterEnv(t)
	e.mustOK(plainCreate("ins-cccccccc", 2222), createCluster("ins-aaaaaaaa", 2224, routedSpec(0, 1)), createCluster("ins-dddddddd", 2226, otherRoutedSpec()),
		executor.Command{ID: "stop-d", InstanceID: "ins-dddddddd", Type: "stop_instance"})
	cs := e.clusterStates()
	if cs["ins-cccccccc"] != "" || cs["ins-aaaaaaaa"] != "ok" || cs["ins-dddddddd"] != "pending" {
		t.Fatalf("cluster states = %v", cs)
	}
	// 状态 ≤ 200 字节且是合法 UTF-8（长的中文错误被截断）
	e.cn.SetFailApply(errors.New(clusterspec.ErrPrefix + strings.Repeat("父接口出现地址", 40)))
	e.mustOK(executor.Command{ID: "start-d", InstanceID: "ins-dddddddd", Type: "start_instance"})
	got := e.clusterStates()["ins-dddddddd"]
	if !strings.HasPrefix(got, "error: 父接口") || len(got) > 200 || !utf8.ValidString(got) {
		t.Fatalf("long status = %q (%d bytes)", got, len(got))
	}
}

func TestInitScriptHasClusterSnippet(t *testing.T) {
	e := newClusterEnv(t)
	e.mustOK(createCluster("ins-aaaaaaaa", 2222, routedSpec(0, 1)))
	init := e.file("ins-aaaaaaaa", "init.sh")
	for _, want := range []string{
		"if [ -f /.spark/cluster.json ]; then",
		"ln -sfn /.spark/cluster.json /etc/spark-cluster.json",
		`printf '%s\n' '[ -f /.spark/cluster.env ] && . /.spark/cluster.env' > /etc/profile.d/spark-cluster.sh`,
		"rm -f /etc/spark-cluster.json /etc/profile.d/spark-cluster.sh",
	} {
		if !strings.Contains(init, want) {
			t.Fatalf("init.sh missing %q:\n%s", want, init)
		}
	}
	if strings.Index(init, "/.spark/cluster.json") > strings.Index(init, "exec /usr/sbin/sshd") {
		t.Fatalf("cluster snippet must run before exec sshd:\n%s", init)
	}
	if out, err := exec.Command("/bin/sh", "-n", filepath.Join(e.dir, "ssh", "ins-aaaaaaaa", "init.sh")).CombinedOutput(); err != nil {
		t.Fatalf("init.sh syntax: %v %s", err, out)
	}
	// 片段本身可执行：把路径换到临时目录跑一遍，profile 片段导入 cluster.env 后子进程读得到 export 的变量
	root := t.TempDir()
	start := strings.Index(init, "if [ -f /.spark/cluster.json ]; then")
	end := strings.Index(init[start:], "\nfi\n") + start + len("\nfi\n")
	snippet := strings.NewReplacer("/.spark/", root+"/spark/", "/etc/", root+"/etc/").Replace(init[start:end])
	_ = os.MkdirAll(filepath.Join(root, "spark"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "spark", "cluster.json"), []byte(e.file("ins-aaaaaaaa", "cluster.json")), 0o644)
	_ = os.WriteFile(filepath.Join(root, "spark", "cluster.env"), []byte(e.file("ins-aaaaaaaa", "cluster.env")), 0o644)
	if out, err := exec.Command("/bin/sh", "-e", "-c", snippet).CombinedOutput(); err != nil {
		t.Fatalf("snippet: %v %s", err, out)
	}
	if l, err := os.Readlink(filepath.Join(root, "etc", "spark-cluster.json")); err != nil || l != root+"/spark/cluster.json" {
		t.Fatalf("symlink = %q %v", l, err)
	}
	out, err := exec.Command("/bin/sh", "-c", `. "$1"; sh -c 'printf %s "$NCCL_SOCKET_IFNAME/$MASTER_ADDR"'`, "x", filepath.Join(root, "etc", "profile.d", "spark-cluster.sh")).CombinedOutput()
	if err != nil || !bytes.Equal(out, []byte("spc0/10.210.3.11")) {
		t.Fatalf("profile = %q %v", out, err)
	}
	// 退出集群后（cluster.json 不在）再跑：软链与 profile 片段被删
	_ = os.Remove(filepath.Join(root, "spark", "cluster.json"))
	if out, err := exec.Command("/bin/sh", "-e", "-c", snippet).CombinedOutput(); err != nil {
		t.Fatalf("snippet (left): %v %s", err, out)
	}
	for _, p := range []string{"etc/spark-cluster.json", "etc/profile.d/spark-cluster.sh"} {
		if _, err := os.Lstat(filepath.Join(root, p)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s not removed: %v", p, err)
		}
	}
}

func TestNetInfoAndClusterCapable(t *testing.T) {
	e := newClusterEnv(t)
	info := clusterspec.NetInfo{Ifaces: []clusterspec.Iface{{Name: "enP7s7", Up: true, IPv4: []string{"192.168.1.221/24"}}}}
	e.cn.SetInfo(info)
	if !e.ex.ClusterCapable() {
		t.Fatal("ClusterCapable = false with Deps.Cluster")
	}
	got := e.ex.NetInfo(context.Background())
	if got == nil || len(got.Ifaces) != 1 || got.Ifaces[0].Name != "enP7s7" {
		t.Fatalf("net info = %+v", got)
	}
	e.cn.SetFailProbe(errors.New("sysfs unreadable"))
	if got := e.ex.NetInfo(context.Background()); got != nil {
		t.Fatalf("probe failure must yield nil, got %+v", got)
	}
	plain := newEnv(t)
	if plain.ex.ClusterCapable() || plain.ex.NetInfo(context.Background()) != nil {
		t.Fatal("executor without Deps.Cluster must not report cluster capability or net info")
	}
}

// 恢复重试中的实例收到 ConfigureCluster：不把它移出恢复重试（ConfigureCluster 不启动容器）。
func TestConfigureClusterKeepsRecoverPending(t *testing.T) {
	e := newClusterEnv(t)
	e.mustOK(createCluster("ins-aaaaaaaa", 2222, routedSpec(0, 1)))
	e.rt.Exit("ins-aaaaaaaa")
	e.rt.FailNext("start", errors.New("cannot assign requested address"))
	if err := e.ex.Recover(context.Background()); err == nil {
		t.Fatal("recover must report the start failure")
	}
	e.mustOK(configure("ins-aaaaaaaa", "cfg-2", routedSpec(0, 2)))
	if p := executor.RecoverPendingForTest(e.ex); !slices.Equal(p, []string{"ins-aaaaaaaa"}) {
		t.Fatalf("recover pending = %v", p)
	}
}
