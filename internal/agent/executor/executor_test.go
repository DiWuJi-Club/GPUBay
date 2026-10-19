package executor_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	diskfake "spark-cloud/internal/agent/disk/fake"
	"spark-cloud/internal/agent/executor"
	netfake "spark-cloud/internal/agent/netfilter/fake"
	"spark-cloud/internal/agent/runtime"
	rtfake "spark-cloud/internal/agent/runtime/fake"
	"spark-cloud/internal/agent/state"
	"spark-cloud/internal/pki"
)

// quiet 让测试输出保持干净（失败路径会打 ERROR 日志）。
var quiet = slog.New(slog.DiscardHandler)

type env struct {
	t    *testing.T
	ex   *executor.Executor
	rt   *rtfake.Runtime
	disk *diskfake.Disk
	net  *netfake.Netfilter
	st   *state.Store
	dir  string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	st, err := state.Open(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, rt: rtfake.New(), disk: diskfake.New(4000 << 30), net: netfake.New(), st: st, dir: dir}
	e.ex = executor.New(executor.Deps{Log: quiet, Runtime: e.rt, Disk: e.disk, Net: e.net, State: st, StateDir: dir, Bridge: "spark0",
		LocalIPs: func() []string { return []string{"127.0.0.1", "192.168.1.221"} }, MemTotal: func() uint64 { return 128 << 30 }}, true)
	e.ex.SetConfig(executor.Config{LanIP: "192.168.1.221", DNS: []string{"223.5.5.5"}, AllowCIDRs: []string{"10.0.9.0/24"}, BandwidthMbps: 200, DataSizeGiB: 2048})
	return e
}

func create(id string) executor.Command {
	return executor.Command{ID: "cmd-" + id, InstanceID: id, Type: "create_instance", ImageRef: "nvcr.io/x:1", UpstreamPubKey: "ssh-ed25519 AAAAtest", BandwidthMbps: 50, SSHPort: 2222, DataSizeGiB: 2048}
}

func (e *env) run(cmds ...executor.Command) []executor.ResultMsg {
	e.t.Helper()
	for _, c := range cmds {
		e.ex.Submit(c)
	}
	return e.ex.RunPending(context.Background(), nil)
}

func (e *env) phases() map[string]string {
	out := map[string]string{}
	for _, s := range e.ex.Report(context.Background()) {
		out[s.InstanceID] = s.Phase
	}
	return out
}

func TestCreateHappyPath(t *testing.T) {
	e := newEnv(t)
	res := e.run(create("ins-aaaaaaaa"))
	if len(res) != 1 || !res[0].OK || res[0].ID != "cmd-ins-aaaaaaaa" {
		t.Fatalf("result = %+v", res)
	}
	c := e.rt.Containers()["ins-aaaaaaaa"]
	if !c.Exists || !c.Running || c.Image != "nvcr.io/x:1" {
		t.Fatalf("container = %+v", c)
	}
	if created, mounted := e.disk.State("ins-aaaaaaaa"); !created || !mounted {
		t.Fatal("disk not created/mounted")
	}
	if e.net.Limits()["veth0abc"] != 50 || e.net.EnsuredCount() != 1 {
		t.Fatalf("net = %+v ensured=%d", e.net.Limits(), e.net.EnsuredCount())
	}
	ssh := filepath.Join(e.dir, "ssh", "ins-aaaaaaaa")
	ak, _ := os.ReadFile(filepath.Join(ssh, "authorized_keys"))
	if strings.TrimSpace(string(ak)) != "ssh-ed25519 AAAAtest" {
		t.Fatalf("authorized_keys = %q", ak)
	}
	init, _ := os.ReadFile(filepath.Join(ssh, "init.sh"))
	for _, want := range []string{"#!/bin/sh", "HostKey /.spark/hostkeys/ssh_host_ed25519_key", "AuthorizedKeysFile /.spark/authorized_keys", "StrictModes no", "PasswordAuthentication no", "KbdInteractiveAuthentication no", "PermitRootLogin prohibit-password", "exec /usr/sbin/sshd -D -e",
		"Subsystem sftp internal-sftp", "AllowTcpForwarding yes", "AllowStreamLocalForwarding no", "GatewayPorts no", "AllowAgentForwarding yes", "X11Forwarding no", "PermitTunnel no", "ClientAliveInterval 60", "ClientAliveCountMax 3", "MaxStartups 50:30:200", "MaxSessions 32"} {
		if !strings.Contains(string(init), want) {
			t.Fatalf("init.sh missing %q:\n%s", want, init)
		}
	}
	if st, err := os.Stat(filepath.Join(ssh, "init.sh")); err != nil || st.Mode().Perm() != 0o755 {
		t.Fatalf("init.sh perm = %v %v", st, err)
	}
	hk, err := os.ReadFile(filepath.Join(ssh, "hostkeys", "ssh_host_ed25519_key"))
	if err != nil || !strings.Contains(string(hk), "OPENSSH PRIVATE KEY") {
		t.Fatalf("hostkey = %v", err)
	}
	inst, ok := e.st.Instance("ins-aaaaaaaa")
	if !ok || inst.Desired != "running" || inst.BandwidthMbps != 50 {
		t.Fatalf("state = %+v", inst)
	}
	if e.phases()["ins-aaaaaaaa"] != "running" || e.ex.DataMount() != e.disk.MountDir("ins-aaaaaaaa") {
		t.Fatalf("phases = %v mount = %s", e.phases(), e.ex.DataMount())
	}
}

func TestCreateFailsOnLowDiskOrWrongLanIP(t *testing.T) {
	e := newEnv(t)
	e.disk.SetFree(2100 << 30) // 需要 2048 + 100 GiB
	res := e.run(create("ins-aaaaaaaa"))
	if res[0].OK || !strings.HasPrefix(res[0].Message, "磁盘空间不足") {
		t.Fatalf("low disk = %+v", res[0])
	}
	if created, _ := e.disk.State("ins-aaaaaaaa"); created {
		t.Fatal("disk created despite low space")
	}
	e.disk.SetFree(4000 << 30)
	e.ex.SetConfig(executor.Config{LanIP: "10.9.9.9", DNS: []string{"223.5.5.5"}, BandwidthMbps: 200, DataSizeGiB: 2048})
	res = e.run(create("ins-bbbbbbbb"))
	if res[0].OK || !strings.HasPrefix(res[0].Message, "内网 IP 不匹配") {
		t.Fatalf("lan ip = %+v", res[0])
	}
}

func TestDuplicateCommandReplaysResult(t *testing.T) {
	e := newEnv(t)
	first := e.run(create("ins-aaaaaaaa"))
	e.rt.FailNext("create", errors.New("must not run again")) // 若真的重跑，结果会变成失败
	replay := e.run(create("ins-aaaaaaaa"))
	if len(replay) != 1 || replay[0] != first[0] || !replay[0].OK {
		t.Fatalf("replay = %+v want %+v", replay, first)
	}
	if more := e.ex.RunPending(context.Background(), nil); len(more) != 0 {
		t.Fatalf("replay returned twice: %+v", more)
	}
	// 自动模式下回放直接送到 Results()
	auto := executor.New(executor.Deps{Log: quiet, Runtime: e.rt, Disk: e.disk, Net: e.net, State: e.st, StateDir: e.dir,
		LocalIPs: func() []string { return []string{"192.168.1.221"} }, MemTotal: func() uint64 { return 128 << 30 }}, false)
	auto.Submit(create("ins-aaaaaaaa"))
	select {
	case r := <-auto.Results():
		if r != first[0] {
			t.Fatalf("auto replay = %+v", r)
		}
	case <-time.After(time.Second):
		t.Fatal("auto mode must replay to Results()")
	}
}

func TestCreateIsStepIdempotent(t *testing.T) {
	e := newEnv(t)
	e.run(create("ins-aaaaaaaa"))
	// 模拟 state.json 丢失后整条重跑：容器已存在且一致 → 不删不建
	_ = os.Remove(filepath.Join(e.dir, "state.json"))
	e2 := &env{t: t, rt: e.rt, disk: e.disk, net: e.net, dir: e.dir}
	e2.st, _ = state.Open(filepath.Join(e.dir, "state.json"))
	e2.ex = executor.New(executor.Deps{Log: quiet, Runtime: e.rt, Disk: e.disk, Net: e.net, State: e2.st, StateDir: e.dir, Bridge: "spark0",
		LocalIPs: func() []string { return []string{"192.168.1.221"} }, MemTotal: func() uint64 { return 128 << 30 }}, true)
	e2.ex.SetConfig(e.ex.Config())
	e.rt.FailNext("remove", errors.New("container must be reused"))
	if res := e2.run(create("ins-aaaaaaaa")); !res[0].OK {
		t.Fatalf("rerun = %+v", res[0])
	}
	// 重跑没有调用 Remove：注入的失败仍未被消费（这里顺带消费掉）
	if err := e.rt.Remove(context.Background(), "ins-aaaaaaaa"); err == nil || err.Error() != "container must be reused" {
		t.Fatalf("rerun removed the container (remove failure consumed): %v", err)
	}
	// 镜像不一致 → 删掉重建
	if err := e.rt.Remove(context.Background(), "ins-aaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	if err := e.rt.Create(context.Background(), runtimeSpec("ins-aaaaaaaa", "old:img")); err != nil {
		t.Fatal(err)
	}
	cmd := create("ins-aaaaaaaa")
	cmd.ID = "cmd-2"
	if res := e2.run(cmd); !res[0].OK || e.rt.Containers()["ins-aaaaaaaa"].Image != "nvcr.io/x:1" {
		t.Fatalf("recreate = %+v image=%s", res[0], e.rt.Containers()["ins-aaaaaaaa"].Image)
	}
}

func TestStartRequiresMount(t *testing.T) {
	e := newEnv(t)
	e.run(create("ins-aaaaaaaa"))
	_ = e.rt.Stop(context.Background(), "ins-aaaaaaaa")
	e.disk.Unmount("ins-aaaaaaaa")
	e.disk.FailNext("mount", errors.New("mount: wrong fs type"))
	res := e.run(executor.Command{ID: "cmd-start", InstanceID: "ins-aaaaaaaa", Type: "start_instance"})
	if res[0].OK || !strings.Contains(res[0].Message, "数据盘未挂载") {
		t.Fatalf("start = %+v", res[0])
	}
	if e.rt.Containers()["ins-aaaaaaaa"].Running {
		t.Fatal("container started without data disk")
	}
	if e.phases()["ins-aaaaaaaa"] != "missing_data" {
		t.Fatalf("phases = %v", e.phases())
	}
	// 挂载恢复后启动成功，限速重新设置
	res = e.run(executor.Command{ID: "cmd-start-2", InstanceID: "ins-aaaaaaaa", Type: "start_instance"})
	if !res[0].OK || !e.rt.Containers()["ins-aaaaaaaa"].Running || e.net.Limits()["veth0abc"] != 50 {
		t.Fatalf("start again = %+v", res[0])
	}
}

func TestStopWipeAndVerification(t *testing.T) {
	e := newEnv(t)
	e.run(create("ins-aaaaaaaa"))
	res := e.run(executor.Command{ID: "cmd-stop", InstanceID: "ins-aaaaaaaa", Type: "stop_instance"})
	if !res[0].OK || e.rt.Containers()["ins-aaaaaaaa"].Running || e.phases()["ins-aaaaaaaa"] != "stopped" {
		t.Fatalf("stop = %+v phases=%v", res[0], e.phases())
	}
	if inst, _ := e.st.Instance("ins-aaaaaaaa"); inst.Desired != "stopped" {
		t.Fatal("desired not updated")
	}
	// 容器不存在的 stop 视为成功
	if res := e.run(executor.Command{ID: "cmd-stop-x", InstanceID: "ins-nothere", Type: "stop_instance"}); !res[0].OK {
		t.Fatalf("stop missing = %+v", res[0])
	}
	e.disk.FailNext("destroy", errors.New("device busy"))
	res = e.run(executor.Command{ID: "cmd-wipe-1", InstanceID: "ins-aaaaaaaa", Type: "wipe_instance"})
	if res[0].OK || !strings.Contains(res[0].Message, "device busy") {
		t.Fatalf("wipe fail = %+v", res[0])
	}
	res = e.run(executor.Command{ID: "cmd-wipe-2", InstanceID: "ins-aaaaaaaa", Type: "wipe_instance"})
	if !res[0].OK {
		t.Fatalf("wipe = %+v", res[0])
	}
	if c := e.rt.Containers()["ins-aaaaaaaa"]; c.Exists {
		t.Fatal("container remains")
	}
	if ok, _ := e.disk.Exists(context.Background(), "ins-aaaaaaaa"); ok {
		t.Fatal("disk remains")
	}
	if _, err := os.Stat(filepath.Join(e.dir, "ssh", "ins-aaaaaaaa")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("ssh dir remains")
	}
	if _, ok := e.st.Instance("ins-aaaaaaaa"); ok || len(e.phases()) != 0 || e.ex.DataMount() != "" {
		t.Fatal("state remains")
	}
}

func TestPullQueueRunsAfterInstanceCommands(t *testing.T) {
	e := newEnv(t)
	res := e.run(executor.Command{ID: "cmd-pull", Type: "pull_image", ImageRef: "nvcr.io/x:2"}, create("ins-aaaaaaaa"))
	if len(res) != 2 || res[0].ID != "cmd-ins-aaaaaaaa" || res[1].ID != "cmd-pull" || !res[1].OK {
		t.Fatalf("order = %+v", res)
	}
	if p := e.rt.Pulled(); len(p) != 1 || p[0] != "nvcr.io/x:2" {
		t.Fatalf("pulled = %v", p)
	}
}

func TestOverrideSkipsExecution(t *testing.T) {
	e := newEnv(t)
	e.ex.Submit(create("ins-aaaaaaaa"))
	res := e.ex.RunPending(context.Background(), &state.Result{OK: false, Message: "e2e: simulated failure"})
	if len(res) != 1 || res[0].OK || res[0].Message != "e2e: simulated failure" || len(e.rt.Containers()) != 0 {
		t.Fatalf("override = %+v containers=%d", res, len(e.rt.Containers()))
	}
	if r, ok := e.st.Result("cmd-ins-aaaaaaaa"); !ok || r.OK || r.Sent {
		t.Fatalf("override result must be persisted unsent: %+v %v", r, ok)
	}
}

func TestRecoverMountsBeforeStart(t *testing.T) {
	e := newEnv(t)
	e.run(create("ins-aaaaaaaa"))
	// 模拟重启：容器未运行、挂载丢失、规则丢失
	e.rt.Exit("ins-aaaaaaaa")
	e.disk.Unmount("ins-aaaaaaaa")
	if err := e.ex.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, mounted := e.disk.State("ins-aaaaaaaa"); !mounted || !e.rt.Containers()["ins-aaaaaaaa"].Running || e.net.EnsuredCount() != 2 {
		t.Fatalf("recover: mounted=%v running=%v ensured=%d", mounted, e.rt.Containers()["ins-aaaaaaaa"].Running, e.net.EnsuredCount())
	}
	// 挂载失败 → 不启动，报 missing_data
	e.rt.Exit("ins-aaaaaaaa")
	e.disk.Unmount("ins-aaaaaaaa")
	e.disk.FailNext("mount", errors.New("mount failed"))
	if err := e.ex.Recover(context.Background()); err == nil {
		t.Fatal("recover must report mount failure")
	}
	if e.rt.Containers()["ins-aaaaaaaa"].Running || e.phases()["ins-aaaaaaaa"] != "missing_data" {
		t.Fatalf("started without mount: phases=%v", e.phases())
	}
	// desired=stopped 的实例不启动
	_ = e.st.PutInstance(state.Instance{ID: "ins-aaaaaaaa", Desired: "stopped", DataSizeGiB: 2048})
	_ = e.disk.Mount(context.Background(), "ins-aaaaaaaa")
	_ = e.ex.Recover(context.Background())
	if e.rt.Containers()["ins-aaaaaaaa"].Running {
		t.Fatal("stopped instance started on recover")
	}
}

func TestReportPhases(t *testing.T) {
	e := newEnv(t)
	e.run(create("ins-aaaaaaaa"))
	e.rt.Exit("ins-aaaaaaaa")
	if e.phases()["ins-aaaaaaaa"] != "stopped" {
		t.Fatalf("exited = %v", e.phases())
	}
	e.disk.Unmount("ins-aaaaaaaa")
	if e.phases()["ins-aaaaaaaa"] != "missing_data" {
		t.Fatalf("unmounted = %v", e.phases())
	}
	e.rt.FailNext("list", errors.New("docker down"))
	if e.phases()["ins-aaaaaaaa"] != "unknown" {
		t.Fatalf("docker down = %v", e.phases())
	}
	// 手工起的带标签容器也会上报（孤儿）
	_ = e.rt.Create(context.Background(), runtimeSpec("ins-zzzzzzzz", "x"))
	if _, ok := e.phases()["ins-zzzzzzzz"]; !ok {
		t.Fatal("orphan not reported")
	}
}

func runtimeSpec(id, image string) runtime.Spec {
	return runtime.Spec{InstanceID: id, ImageRef: image, DataDir: "/fake/mnt/" + id}
}

// countingRuntime 统计 Create 调用次数，并可在 Create 内阻塞，模拟执行中途收到重复指令。
type countingRuntime struct {
	*rtfake.Runtime
	creates atomic.Int32
	mu      sync.Mutex
	spec    runtime.Spec // 最近一次 Create 的规格
	entered chan struct{}
	release chan struct{}
}

func (r *countingRuntime) Create(ctx context.Context, s runtime.Spec) error {
	r.creates.Add(1)
	r.mu.Lock()
	r.spec = s
	r.mu.Unlock()
	if r.entered != nil {
		r.entered <- struct{}{}
		<-r.release
	}
	return r.Runtime.Create(ctx, s)
}

func TestDuplicateCommandRunsAtMostOnce(t *testing.T) {
	// 步进模式：同一 ID 连续推两次只执行一次
	e := newEnv(t)
	cr := &countingRuntime{Runtime: e.rt}
	ex := executor.New(executor.Deps{Log: quiet, Runtime: cr, Disk: e.disk, Net: e.net, State: e.st, StateDir: e.dir,
		LocalIPs: func() []string { return []string{"192.168.1.221"} }, MemTotal: func() uint64 { return 128 << 30 }}, true)
	ex.SetConfig(e.ex.Config())
	ex.Submit(create("ins-aaaaaaaa"))
	ex.Submit(create("ins-aaaaaaaa"))
	if res := ex.RunPending(context.Background(), nil); len(res) != 1 || !res[0].OK || cr.creates.Load() != 1 {
		t.Fatalf("stepped dup = %+v creates=%d", res, cr.creates.Load())
	}

	// 自动模式：执行中途（结果尚未落盘）再推同一 ID，也只执行一次；完成后再推则回放
	e2 := newEnv(t)
	cr2 := &countingRuntime{Runtime: e2.rt, entered: make(chan struct{}), release: make(chan struct{})}
	auto := executor.New(executor.Deps{Log: quiet, Runtime: cr2, Disk: e2.disk, Net: e2.net, State: e2.st, StateDir: e2.dir,
		LocalIPs: func() []string { return []string{"192.168.1.221"} }, MemTotal: func() uint64 { return 128 << 30 }}, false)
	auto.SetConfig(e2.ex.Config())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go auto.Run(ctx)
	auto.Submit(create("ins-aaaaaaaa"))
	select {
	case <-cr2.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("create never started")
	}
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() { defer wg.Done(); auto.Submit(create("ins-aaaaaaaa")) }()
	}
	wg.Wait()
	close(cr2.release)
	first := <-auto.Results()
	if !first.OK {
		t.Fatalf("first = %+v", first)
	}
	auto.Submit(create("ins-aaaaaaaa"))
	select {
	case r := <-auto.Results():
		if r != first {
			t.Fatalf("replay = %+v want %+v", r, first)
		}
	case <-time.After(time.Second):
		t.Fatal("no replay")
	}
	select {
	case r := <-auto.Results():
		t.Fatalf("unexpected extra result %+v", r)
	case <-time.After(1500 * time.Millisecond): // 留足时间让误入队的重复指令暴露
	}
	if n := cr2.creates.Load(); n != 1 {
		t.Fatalf("create ran %d times", n)
	}
}

func TestCreateRefusesWhenRulesFail(t *testing.T) {
	e := newEnv(t)
	e.net.FailNext("rules", errors.New("iptables-restore: locked"))
	res := e.run(create("ins-aaaaaaaa"))
	if res[0].OK || !strings.Contains(res[0].Message, "iptables-restore: locked") {
		t.Fatalf("create = %+v", res[0])
	}
	if c := e.rt.Containers()["ins-aaaaaaaa"]; c.Running {
		t.Fatal("container started without isolation rules")
	}
	// start 同样要求规则就绪
	cmd := create("ins-aaaaaaaa")
	cmd.ID = "cmd-2"
	e.run(cmd)
	_ = e.rt.Stop(context.Background(), "ins-aaaaaaaa")
	e.net.FailNext("rules", errors.New("iptables-restore: locked"))
	res = e.run(executor.Command{ID: "cmd-start", InstanceID: "ins-aaaaaaaa", Type: "start_instance"})
	if res[0].OK || e.rt.Containers()["ins-aaaaaaaa"].Running {
		t.Fatalf("start without rules = %+v", res[0])
	}
}

func TestRecoverFailsClosedWhenRulesFail(t *testing.T) {
	e := newEnv(t)
	e.run(create("ins-aaaaaaaa"))
	e.rt.Exit("ins-aaaaaaaa")
	e.disk.Unmount("ins-aaaaaaaa")
	e.net.FailNext("rules", errors.New("iptables: chain missing"))
	err := e.ex.Recover(context.Background())
	if err == nil || !strings.Contains(err.Error(), "ins-aaaaaaaa") || !strings.Contains(err.Error(), "iptables: chain missing") {
		t.Fatalf("recover err = %v", err)
	}
	if e.rt.Containers()["ins-aaaaaaaa"].Running {
		t.Fatal("container started although isolation rules failed")
	}
	if e.phases()["ins-aaaaaaaa"] != "stopped" {
		t.Fatalf("phases = %v", e.phases())
	}
	// 规则刷新恢复后自动重试恢复，实例被启动
	restore := executor.SetRulesIntervalForTest(e.ex, 10*time.Millisecond)
	defer restore()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); e.ex.Run(ctx) }()
	defer func() { cancel(); <-done }() // 等 Run（及其中的 Recover）退出，免得它还在写 TempDir 时被清理
	deadline := time.Now().Add(3 * time.Second)
	for !e.rt.Containers()["ins-aaaaaaaa"].Running {
		if time.Now().After(deadline) {
			t.Fatal("recover was not retried after rules recovered")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// errDisk 让 Mounted 返回错误：错误不能被当成「未挂载」吞掉，也不能当成已挂载。
type errDisk struct {
	*diskfake.Disk
	err error
}

func (d *errDisk) Mounted(context.Context, string) (bool, error) { return false, d.err }

func TestMountCheckErrorsAndDataBindAreEnforced(t *testing.T) {
	e := newEnv(t)
	e.run(create("ins-aaaaaaaa"))
	_ = e.rt.Stop(context.Background(), "ins-aaaaaaaa")
	ed := &errDisk{Disk: e.disk, err: errors.New("findmnt: exec failed")}
	ex := executor.New(executor.Deps{Log: quiet, Runtime: e.rt, Disk: ed, Net: e.net, State: e.st, StateDir: e.dir,
		LocalIPs: func() []string { return []string{"192.168.1.221"} }, MemTotal: func() uint64 { return 128 << 30 }}, true)
	ex.SetConfig(e.ex.Config())
	ex.Submit(executor.Command{ID: "cmd-start", InstanceID: "ins-aaaaaaaa", Type: "start_instance"})
	res := ex.RunPending(context.Background(), nil)
	if res[0].OK || !strings.Contains(res[0].Message, "findmnt: exec failed") || e.rt.Containers()["ins-aaaaaaaa"].Running {
		t.Fatalf("start with mount check error = %+v", res[0])
	}
	if p := ex.Report(context.Background()); len(p) != 1 || p[0].Phase != "unknown" {
		t.Fatalf("report with mount check error = %+v", p)
	}
	// 容器没有绑定 /root/data（例如被手工重建）→ 拒绝启动
	e.rt.SetDataMounted("ins-aaaaaaaa", false)
	res = e.run(executor.Command{ID: "cmd-start-2", InstanceID: "ins-aaaaaaaa", Type: "start_instance"})
	if res[0].OK || !strings.HasPrefix(res[0].Message, "数据盘未挂载") || e.rt.Containers()["ins-aaaaaaaa"].Running {
		t.Fatalf("start without data bind = %+v", res[0])
	}
}

func TestRecoverMountsStoppedInstances(t *testing.T) {
	e := newEnv(t)
	e.run(create("ins-aaaaaaaa"), executor.Command{ID: "cmd-stop", InstanceID: "ins-aaaaaaaa", Type: "stop_instance"})
	// 重启：挂载丢失；期望 stopped 的实例也要挂上，否则会被误报 missing_data
	e.disk.Unmount("ins-aaaaaaaa")
	if err := e.ex.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, mounted := e.disk.State("ins-aaaaaaaa"); !mounted || e.rt.Containers()["ins-aaaaaaaa"].Running || e.phases()["ins-aaaaaaaa"] != "stopped" {
		t.Fatalf("recover stopped: mounted=%v phases=%v", mounted, e.phases())
	}
}

func TestDataBindMustBeOwnDisk(t *testing.T) {
	e := newEnv(t)
	e.run(create("ins-aaaaaaaa"))
	_ = e.rt.Stop(context.Background(), "ins-aaaaaaaa")
	// /root/data 绑到了别处 → 拒绝启动
	e.rt.SetDataSource("ins-aaaaaaaa", "/fake/mnt/ins-bbbbbbbb")
	res := e.run(executor.Command{ID: "cmd-start", InstanceID: "ins-aaaaaaaa", Type: "start_instance"})
	if res[0].OK || !strings.HasPrefix(res[0].Message, "数据盘未挂载") || e.rt.Containers()["ins-aaaaaaaa"].Running {
		t.Fatalf("start with foreign data bind = %+v", res[0])
	}
	// create 重跑时不复用，删掉按正确来源重建
	cmd := create("ins-aaaaaaaa")
	cmd.ID = "cmd-2"
	res = e.run(cmd)
	c := e.rt.Containers()["ins-aaaaaaaa"]
	if !res[0].OK || !c.Running || c.DataSource != e.disk.MountDir("ins-aaaaaaaa") {
		t.Fatalf("recreate = %+v container=%+v", res[0], c)
	}
}

func TestCreateRejectsEmptyUpstreamKey(t *testing.T) {
	e := newEnv(t)
	cmd := create("ins-aaaaaaaa")
	cmd.UpstreamPubKey = "  "
	res := e.run(cmd)
	if res[0].OK || !strings.Contains(res[0].Message, "上游 SSH 公钥") || len(e.rt.Containers()) != 0 {
		t.Fatalf("empty key = %+v containers=%d", res[0], len(e.rt.Containers()))
	}
	if created, _ := e.disk.State("ins-aaaaaaaa"); created {
		t.Fatal("disk created despite missing key")
	}
}

func TestContainerMemoryAndShmLimits(t *testing.T) {
	for _, tc := range []struct{ mem, wantMem, wantShm uint64 }{
		{128 << 30, 120 << 30, 64 << 30},
		{12 << 30, 4 << 30, 4 << 30}, // shm = 6GiB 超过内存上限 4GiB → 封顶
	} {
		e := newEnv(t)
		cr := &countingRuntime{Runtime: e.rt}
		ex := executor.New(executor.Deps{Log: quiet, Runtime: cr, Disk: e.disk, Net: e.net, State: e.st, StateDir: e.dir,
			LocalIPs: func() []string { return []string{"192.168.1.221"} }, MemTotal: func() uint64 { return tc.mem }}, true)
		ex.SetConfig(e.ex.Config())
		ex.Submit(create("ins-aaaaaaaa"))
		if res := ex.RunPending(context.Background(), nil); !res[0].OK {
			t.Fatalf("create = %+v", res[0])
		}
		if cr.spec.MemoryBytes != int64(tc.wantMem) || cr.spec.ShmBytes != int64(tc.wantShm) {
			t.Fatalf("mem %d: spec memory=%d shm=%d", tc.mem, cr.spec.MemoryBytes, cr.spec.ShmBytes)
		}
	}
}

// 空闲时提交的实例指令立即被实例消费者取走，不会因为唤醒被 pull 消费者抢走而等兜底轮询（曾经约 1 秒）。
// 连续 5 条：共用唤醒通道时每条约一半概率被抢，全部及时的概率只有 1/32。
func TestIdleInstanceCommandStartsPromptly(t *testing.T) {
	e := newEnv(t)
	auto := executor.New(executor.Deps{Log: quiet, Runtime: e.rt, Disk: e.disk, Net: e.net, State: e.st, StateDir: e.dir,
		LocalIPs: func() []string { return []string{"192.168.1.221"} }, MemTotal: func() uint64 { return 128 << 30 }}, false)
	auto.SetConfig(e.ex.Config())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); auto.Run(ctx) }()
	defer func() { cancel(); <-done }()
	for i := range 5 {
		time.Sleep(20 * time.Millisecond) // 让两个消费者都进入空闲等待
		cmd := executor.Command{ID: fmt.Sprintf("cmd-stop-%d", i), InstanceID: "ins-aaaaaaaa", Type: "stop_instance"}
		start := time.Now()
		auto.Submit(cmd)
		select {
		case r := <-auto.Results():
			if r.ID != cmd.ID {
				t.Fatalf("result = %+v", r)
			}
			if d := time.Since(start); d > 500*time.Millisecond {
				t.Fatalf("command %d took %v", i, d)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("command %d never ran", i)
		}
	}
}

// runLimitLoop 以 10ms 周期跑 Run（步进模式也会跑限速巡检），返回停止函数。
func (e *env) runLimitLoop() (stop func()) {
	restore := executor.SetRulesIntervalForTest(e.ex, 10*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); e.ex.Run(ctx) }()
	return func() { cancel(); <-done; restore() }
}

// waitRounds 等 Run 再开始 n 轮规则刷新（EnsuredCount 增长 n）：此时至少有 n-1 轮完整跑完（重试、限速巡检都已执行），
// 负向断言（「没有发生」）据此成立，不依赖固定的 sleep。
func (e *env) waitRounds(n int) {
	e.t.Helper()
	base := e.net.EnsuredCount()
	waitFor(e.t, "refresh rounds", func() bool { return e.net.EnsuredCount() >= base+n })
}

func (e *env) waitLimit(veth string, want int) bool {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if v, ok := e.net.Limits()[veth]; ok && v == want {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

func TestEnsureLimitsReappliesWhenDropped(t *testing.T) {
	e := newEnv(t)
	e.run(create("ins-aaaaaaaa")) // BandwidthMbps=50，创建时已 Limit
	veth := e.rt.Veth
	if e.net.Limits()[veth] != 50 {
		t.Fatalf("limits after create = %v", e.net.Limits())
	}
	e.net.Drop(veth)
	stop := e.runLimitLoop()
	defer stop()
	if !e.waitLimit(veth, 50) {
		t.Fatalf("limit not reapplied: %v", e.net.Limits())
	}
}

func TestEnsureLimitsSkipsWhenStillApplied(t *testing.T) {
	e := newEnv(t)
	e.run(create("ins-aaaaaaaa"))
	before := e.net.LimitCount()
	stop := e.runLimitLoop()
	e.waitRounds(2)
	stop()
	if n := e.net.LimitCount(); n != before {
		t.Fatalf("Limit called %d extra times although the limit was in place", n-before)
	}
}

func TestEnsureLimitsIgnoresStoppedContainer(t *testing.T) {
	e := newEnv(t)
	e.run(create("ins-aaaaaaaa"))
	veth := e.rt.Veth
	_ = e.rt.Stop(context.Background(), "ins-aaaaaaaa")
	e.net.Drop(veth)
	stop := e.runLimitLoop()
	e.waitRounds(2)
	stop()
	if _, ok := e.net.Limits()[veth]; ok {
		t.Fatal("limit applied to a stopped container")
	}
	if e.rt.Containers()["ins-aaaaaaaa"].Running {
		t.Fatal("ensureLimits started a container")
	}
}

func TestEnsureLimitsSkipsWhileOpInProgress(t *testing.T) {
	e := newEnv(t)
	e.run(create("ins-aaaaaaaa"))
	veth := e.rt.Veth
	e.net.Drop(veth)
	unlock := executor.LockOpsForTest(e.ex)
	stop := e.runLimitLoop()
	defer stop()
	e.waitRounds(2)
	if _, ok := e.net.Limits()[veth]; ok {
		unlock()
		t.Fatal("limit applied while opMu held")
	}
	unlock()
	if !e.waitLimit(veth, 50) {
		t.Fatalf("limit not reapplied after opMu released: %v", e.net.Limits())
	}
}

func TestEnsureLimitsFallsBackToConfigBandwidth(t *testing.T) {
	e := newEnv(t)
	e.run(create("ins-aaaaaaaa"))
	veth := e.rt.Veth
	inst := e.st.Instances()[0]
	inst.BandwidthMbps = 0
	if err := e.st.PutInstance(inst); err != nil {
		t.Fatal(err)
	}
	e.net.Drop(veth)
	stop := e.runLimitLoop()
	defer stop()
	if !e.waitLimit(veth, 200) { // Config.BandwidthMbps
		t.Fatalf("limits = %v", e.net.Limits())
	}
}

// SetConfig 改变白名单后立即刷新规则，不等定时器；相同白名单不触发额外刷新。
func TestSetConfigAllowCIDRsChangeRefreshesRulesImmediately(t *testing.T) {
	e := newEnv(t)
	restore := executor.SetRulesIntervalForTest(e.ex, time.Hour)
	defer restore()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); e.ex.Run(ctx) }()
	defer func() { cancel(); <-done }()

	waitRules := func(want []string) bool {
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			if got := e.net.Rules().AllowCIDRs; slices.Equal(got, want) {
				return true
			}
			time.Sleep(5 * time.Millisecond)
		}
		return false
	}
	// newEnv 的初始配置（零值 -> 10.0.9.0/24）也算变化，先等它落地
	if !waitRules([]string{"10.0.9.0/24"}) {
		t.Fatalf("initial config not applied: %v", e.net.Rules())
	}
	cfg := e.ex.Config()
	cfg.AllowCIDRs = []string{"192.168.1.218/32"}
	e.ex.SetConfig(cfg)
	if !waitRules([]string{"192.168.1.218/32"}) {
		t.Fatalf("allow-list change not applied within 1s: %v", e.net.Rules())
	}

	n := e.net.EnsuredCount()
	e.ex.SetConfig(cfg)           // 相同白名单
	cfg.DNS = []string{"1.1.1.1"} // 非白名单字段变化
	cfg.BandwidthMbps = 99
	e.ex.SetConfig(cfg)
	time.Sleep(200 * time.Millisecond)
	if got := e.net.EnsuredCount(); got != n {
		t.Fatalf("EnsureRules called %d extra times without allow-list change", got-n)
	}
}

// flakyStart 让 Start 在 fail 为真时持续失败（fake 的 FailNext 只管一次），并统计 Start 调用次数。
type flakyStart struct {
	*rtfake.Runtime
	fail   atomic.Bool
	starts atomic.Int32
}

func (r *flakyStart) Start(ctx context.Context, id string) error {
	r.starts.Add(1)
	if r.fail.Load() {
		return errors.New("failed to bind host port: cannot assign requested address")
	}
	return r.Runtime.Start(ctx, id)
}

// recoverEnv：已创建并运行的实例 ins-aaaaaaaa，模拟重启后 Recover 因 Start 失败而进入待重试。
func recoverEnv(t *testing.T) (*env, *flakyStart) {
	t.Helper()
	e := newEnv(t)
	fr := &flakyStart{Runtime: e.rt}
	e.ex = executor.New(executor.Deps{Log: quiet, Runtime: fr, Disk: e.disk, Net: e.net, State: e.st, StateDir: e.dir, Bridge: "spark0",
		LocalIPs: func() []string { return []string{"192.168.1.221"} }, MemTotal: func() uint64 { return 128 << 30 }}, true)
	e.ex.SetConfig(executor.Config{LanIP: "192.168.1.221", DNS: []string{"223.5.5.5"}, AllowCIDRs: []string{"10.0.9.0/24"}, BandwidthMbps: 200, DataSizeGiB: 2048})
	if res := e.run(create("ins-aaaaaaaa")); !res[0].OK {
		t.Fatalf("create = %+v", res[0])
	}
	e.rt.Exit("ins-aaaaaaaa")
	fr.fail.Store(true)
	if err := e.ex.Recover(context.Background()); err == nil {
		t.Fatal("recover must report the start failure")
	}
	return e, fr
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRecoverStartFailureIsRetriedOnRefresh(t *testing.T) {
	e, fr := recoverEnv(t)
	if p := executor.RecoverPendingForTest(e.ex); len(p) != 1 || p[0] != "ins-aaaaaaaa" {
		t.Fatalf("pending = %v", p)
	}
	fr.fail.Store(false)
	e.net.Drop(e.rt.Veth)
	stop := e.runLimitLoop()
	defer stop()
	waitFor(t, "container running", func() bool { return e.rt.Containers()["ins-aaaaaaaa"].Running })
	waitFor(t, "pending cleared", func() bool { return len(executor.RecoverPendingForTest(e.ex)) == 0 })
	if !e.waitLimit(e.rt.Veth, 50) {
		t.Fatalf("limit not applied after retry: %v", e.net.Limits())
	}
}

func TestRecoverRetryKeepsPendingWhileFailing(t *testing.T) {
	e, fr := recoverEnv(t)
	before := fr.starts.Load()
	stop := e.runLimitLoop()
	defer stop()
	waitFor(t, "several retries", func() bool { return fr.starts.Load() >= before+3 })
	if p := executor.RecoverPendingForTest(e.ex); len(p) != 1 {
		t.Fatalf("pending = %v", p)
	}
	if e.rt.Containers()["ins-aaaaaaaa"].Running {
		t.Fatal("started despite failure")
	}
}

func TestRecoverMountFailureIsRetriedWithMount(t *testing.T) {
	e := newEnv(t)
	e.run(create("ins-aaaaaaaa"))
	e.rt.Exit("ins-aaaaaaaa")
	e.disk.Unmount("ins-aaaaaaaa")
	e.disk.FailNext("mount", errors.New("loop device busy"))
	if err := e.ex.Recover(context.Background()); err == nil {
		t.Fatal("recover must report mount failure")
	}
	if p := executor.RecoverPendingForTest(e.ex); len(p) != 1 {
		t.Fatalf("pending = %v", p)
	}
	stop := e.runLimitLoop()
	defer stop()
	waitFor(t, "container running", func() bool { return e.rt.Containers()["ins-aaaaaaaa"].Running })
	if _, mounted := e.disk.State("ins-aaaaaaaa"); !mounted {
		t.Fatal("retry did not mount the data disk")
	}
}

func TestRecoverPendingClearedByInstanceCommand(t *testing.T) {
	e, fr := recoverEnv(t)
	res := e.run(executor.Command{ID: "cmd-stop", InstanceID: "ins-aaaaaaaa", Type: "stop_instance"})
	if !res[0].OK {
		t.Fatalf("stop = %+v", res[0])
	}
	if p := executor.RecoverPendingForTest(e.ex); len(p) != 0 {
		t.Fatalf("pending after stop = %v", p)
	}
	fr.fail.Store(false)
	before := fr.starts.Load()
	stop := e.runLimitLoop()
	e.waitRounds(2)
	stop()
	if fr.starts.Load() != before || e.rt.Containers()["ins-aaaaaaaa"].Running {
		t.Fatal("retried an instance taken over by a stop command")
	}
}

func TestRecoverRetryDropsStoppedOrWipedInstances(t *testing.T) {
	// Desired 变为 stopped
	e, fr := recoverEnv(t)
	inst, _ := e.st.Instance("ins-aaaaaaaa")
	inst.Desired = "stopped"
	_ = e.st.PutInstance(inst)
	fr.fail.Store(false)
	before := fr.starts.Load()
	stop := e.runLimitLoop()
	waitFor(t, "pending cleared", func() bool { return len(executor.RecoverPendingForTest(e.ex)) == 0 })
	stop()
	if fr.starts.Load() != before || e.rt.Containers()["ins-aaaaaaaa"].Running {
		t.Fatal("stopped instance was started by retry")
	}
	// 记录被删除（wipe）
	e2, fr2 := recoverEnv(t)
	_ = e2.st.DeleteInstance("ins-aaaaaaaa")
	fr2.fail.Store(false)
	before = fr2.starts.Load()
	stop = e2.runLimitLoop()
	waitFor(t, "pending cleared", func() bool { return len(executor.RecoverPendingForTest(e2.ex)) == 0 })
	stop()
	if fr2.starts.Load() != before {
		t.Fatal("wiped instance was started by retry")
	}
}

// 回归保护：运行期间被外部停止的容器不在 pending 里，refresh 不会把它拉起来。
func TestRefreshDoesNotStartExternallyStoppedContainer(t *testing.T) {
	e, fr := recoverEnv(t)
	fr.fail.Store(false)
	e.rt.Exit("ins-aaaaaaaa")
	stop := e.runLimitLoop()
	waitFor(t, "pending drained", func() bool { return len(executor.RecoverPendingForTest(e.ex)) == 0 })
	stop()
	// 第一次重试已把它启动；现在模拟外部停止，pending 已空
	e.rt.Exit("ins-aaaaaaaa")
	before := fr.starts.Load()
	stop = e.runLimitLoop()
	e.waitRounds(2)
	stop()
	if fr.starts.Load() != before || e.rt.Containers()["ins-aaaaaaaa"].Running {
		t.Fatal("externally stopped container was restarted by refresh")
	}
}

func TestRecoverRetrySkipsWhileOpInProgress(t *testing.T) {
	e, fr := recoverEnv(t)
	fr.fail.Store(false)
	before := fr.starts.Load()
	unlock := executor.LockOpsForTest(e.ex)
	stop := e.runLimitLoop()
	defer stop()
	e.waitRounds(2)
	if fr.starts.Load() != before {
		unlock()
		t.Fatal("retry ran while opMu held")
	}
	unlock()
	waitFor(t, "container running after unlock", func() bool { return e.rt.Containers()["ins-aaaaaaaa"].Running })
}

// ---- B2：发布地址缺失 / 网络挂接丢失 / veth、限速失败的 fail closed ----

// startCounter 统计 Start 调用次数。
type startCounter struct {
	*rtfake.Runtime
	starts atomic.Int32
}

func (r *startCounter) Start(ctx context.Context, id string) error {
	r.starts.Add(1)
	return r.Runtime.Start(ctx, id)
}

// ipEnv：LocalIPs 返回值可变；容器已创建并停止。
func ipEnv(t *testing.T) (*env, *startCounter, *atomic.Value) {
	t.Helper()
	e := newEnv(t)
	sc := &startCounter{Runtime: e.rt}
	ips := &atomic.Value{}
	ips.Store([]string{"192.168.1.221"})
	e.ex = executor.New(executor.Deps{Log: quiet, Runtime: sc, Disk: e.disk, Net: e.net, State: e.st, StateDir: e.dir, Bridge: "spark0",
		LocalIPs: func() []string { return ips.Load().([]string) }, MemTotal: func() uint64 { return 128 << 30 }}, true)
	e.ex.SetConfig(executor.Config{LanIP: "192.168.1.221", DNS: []string{"223.5.5.5"}, AllowCIDRs: []string{"10.0.9.0/24"}, BandwidthMbps: 200, DataSizeGiB: 2048})
	if res := e.run(create("ins-aaaaaaaa")); !res[0].OK {
		t.Fatalf("create = %+v", res[0])
	}
	e.rt.Exit("ins-aaaaaaaa")
	sc.starts.Store(0)
	return e, sc, ips
}

func TestStartRefusedWhilePublishAddressMissing(t *testing.T) {
	e, sc, ips := ipEnv(t)
	ips.Store([]string{"127.0.0.1"}) // 重启后 WiFi 还没拿到地址
	if err := e.ex.Recover(context.Background()); err == nil || !strings.Contains(err.Error(), "192.168.1.221") {
		t.Fatalf("recover err = %v", err)
	}
	if sc.starts.Load() != 0 {
		t.Fatal("docker start must not be called while the publish address is missing")
	}
	if p := executor.RecoverPendingForTest(e.ex); len(p) != 1 {
		t.Fatalf("pending = %v", p)
	}
	ips.Store([]string{"127.0.0.1", "192.168.1.221"})
	stop := e.runLimitLoop()
	defer stop()
	waitFor(t, "container running", func() bool { return e.rt.Containers()["ins-aaaaaaaa"].Running })
	waitFor(t, "pending cleared", func() bool { return len(executor.RecoverPendingForTest(e.ex)) == 0 })
}

func TestStartRestoresLostNetworkAttachment(t *testing.T) {
	e := newEnv(t)
	e.run(create("ins-aaaaaaaa"))
	e.rt.Exit("ins-aaaaaaaa")
	e.rt.SetNetworks("ins-aaaaaaaa", nil)
	start := executor.Command{ID: "cmd-start", InstanceID: "ins-aaaaaaaa", Type: "start_instance"}
	if res := e.run(start); !res[0].OK {
		t.Fatalf("start = %+v", res[0])
	}
	c := e.rt.Containers()["ins-aaaaaaaa"]
	if !c.Running || len(c.Networks) != 1 || c.Networks[0] != "spark0" {
		t.Fatalf("container = %+v", c)
	}
}

func TestStartFailsWhenNetworkReconnectFails(t *testing.T) {
	e := newEnv(t)
	e.run(create("ins-aaaaaaaa"))
	e.rt.Exit("ins-aaaaaaaa")
	e.rt.SetNetworks("ins-aaaaaaaa", nil)
	e.rt.FailNext("connect", errors.New("network spark0 not found"))
	if res := e.run(executor.Command{ID: "cmd-start", InstanceID: "ins-aaaaaaaa", Type: "start_instance"}); res[0].OK {
		t.Fatal("start must fail when network attachment cannot be restored")
	}
	if e.rt.Containers()["ins-aaaaaaaa"].Running {
		t.Fatal("must not start without network")
	}
}

func TestStartStopsContainerWhenVethFails(t *testing.T) {
	e := newEnv(t)
	e.run(create("ins-aaaaaaaa"))
	e.rt.Exit("ins-aaaaaaaa")
	e.rt.FailNext("veth", errors.New("Device eth0 does not exist"))
	res := e.run(executor.Command{ID: "cmd-start", InstanceID: "ins-aaaaaaaa", Type: "start_instance"})
	if res[0].OK || !strings.Contains(res[0].Message, "eth0") {
		t.Fatalf("start = %+v", res[0])
	}
	if e.rt.Containers()["ins-aaaaaaaa"].Running {
		t.Fatal("container without veth must be stopped")
	}
}

func TestStartStopsContainerWhenLimitFails(t *testing.T) {
	e := newEnv(t)
	e.run(create("ins-aaaaaaaa"))
	e.rt.Exit("ins-aaaaaaaa")
	e.net.FailNext("limit", errors.New("tc failed"))
	res := e.run(executor.Command{ID: "cmd-start", InstanceID: "ins-aaaaaaaa", Type: "start_instance"})
	if res[0].OK || !strings.Contains(res[0].Message, "tc failed") {
		t.Fatalf("start = %+v", res[0])
	}
	if e.rt.Containers()["ins-aaaaaaaa"].Running {
		t.Fatal("container without limit must be stopped")
	}
}

func TestHostKeyFromPlatformIsAuthoritativeAndInitScriptRewritten(t *testing.T) {
	e := newEnv(t)
	_, pem1, err := pki.GenerateInstanceHostKey()
	if err != nil {
		t.Fatal(err)
	}
	cmd := create("ins-aaaaaaaa")
	cmd.HostKeyPEM = string(pem1)
	if res := e.run(cmd); len(res) != 1 || !res[0].OK {
		t.Fatalf("create = %+v", res)
	}
	keyPath := filepath.Join(e.dir, "ssh", "ins-aaaaaaaa", "hostkeys", "ssh_host_ed25519_key")
	assertHostKey(t, keyPath, pem1)
	// 重跑 create（步骤幂等）且平台换了一把钥匙：以平台为准，原子替换
	_, pem2, _ := pki.GenerateInstanceHostKey()
	cmd2 := cmd
	cmd2.ID, cmd2.HostKeyPEM = "cmd-again", string(pem2)
	if res := e.run(cmd2); len(res) != 1 || !res[0].OK {
		t.Fatalf("re-create = %+v", res)
	}
	assertHostKey(t, keyPath, pem2)
	// 现有密钥文件被清空：平台密钥写回
	os.WriteFile(keyPath, nil, 0o600)
	cmd3 := cmd2
	cmd3.ID = "cmd-again-2"
	if res := e.run(cmd3); len(res) != 1 || !res[0].OK {
		t.Fatalf("re-create = %+v", res)
	}
	assertHostKey(t, keyPath, pem2)
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(keyPath), ".*tmp*")); len(left) != 0 {
		t.Fatalf("temp files left behind: %v", left)
	}
	// 旧版 init.sh（没有 sftp 行）在 start 时被重写
	initPath := filepath.Join(e.dir, "ssh", "ins-aaaaaaaa", "init.sh")
	os.WriteFile(initPath, []byte("#!/bin/sh\nexec /usr/sbin/sshd -D\n"), 0o644)
	e.run(executor.Command{ID: "cmd-stop", InstanceID: "ins-aaaaaaaa", Type: "stop_instance"})
	e.run(executor.Command{ID: "cmd-start", InstanceID: "ins-aaaaaaaa", Type: "start_instance"})
	init, _ := os.ReadFile(initPath)
	if !strings.Contains(string(init), "Subsystem sftp internal-sftp") {
		t.Fatalf("init.sh not rewritten on start:\n%s", init)
	}
	if st, _ := os.Stat(initPath); st.Mode().Perm() != 0o755 {
		t.Fatalf("init.sh perm = %o", st.Mode().Perm())
	}
}

// 旧平台（没有 PEM）：本地生成；有效的现有密钥保留；为空或无效的现有密钥重新生成。
func TestHostKeyWithoutPlatformPEM(t *testing.T) {
	e := newEnv(t)
	e.run(create("ins-bbbbbbbb"))
	keyPath := filepath.Join(e.dir, "ssh", "ins-bbbbbbbb", "hostkeys", "ssh_host_ed25519_key")
	local, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("locally generated host key missing: %v", err)
	}
	assertHostKey(t, keyPath, local)
	again := create("ins-bbbbbbbb")
	again.ID = "cmd-again"
	if res := e.run(again); len(res) != 1 || !res[0].OK {
		t.Fatalf("re-create = %+v", res)
	}
	assertHostKey(t, keyPath, local) // 有效的现有密钥保留
	for i, broken := range []string{"", "-----BEGIN OPENSSH PRIVATE KEY-----\ngarbage\n-----END OPENSSH PRIVATE KEY-----\n"} {
		os.WriteFile(keyPath, []byte(broken), 0o600)
		c := create("ins-bbbbbbbb")
		c.ID = fmt.Sprintf("cmd-repair-%d", i)
		if res := e.run(c); len(res) != 1 || !res[0].OK {
			t.Fatalf("repair %d = %+v", i, res)
		}
		got, _ := os.ReadFile(keyPath)
		if string(got) == broken || string(got) == string(local) {
			t.Fatalf("repair %d: key not regenerated", i)
		}
		assertHostKey(t, keyPath, got)
	}
}

// assertHostKey 断言私钥内容为 want、权限 0600，且 .pub 与之匹配。
func assertHostKey(t *testing.T, keyPath string, want []byte) {
	t.Helper()
	got, _ := os.ReadFile(keyPath)
	if string(got) != string(want) { // 不打印密钥内容
		t.Fatalf("host key mismatch (got %d bytes, want %d bytes)", len(got), len(want))
	}
	if st, _ := os.Stat(keyPath); st.Mode().Perm() != 0o600 {
		t.Fatalf("perm = %o", st.Mode().Perm())
	}
	signer, err := ssh.ParsePrivateKey(want)
	if err != nil || signer.PublicKey().Type() != ssh.KeyAlgoED25519 {
		t.Fatalf("host key not ed25519: %v", err)
	}
	pub, _ := os.ReadFile(keyPath + ".pub")
	if strings.TrimSpace(string(pub)) != strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))) {
		t.Fatalf(".pub = %q", pub)
	}
}

// 平台下发了密钥但无法解析 / 不是 ed25519：create 失败，不静默回落到本地生成，也不留下密钥文件。
func TestInvalidPlatformHostKeyFailsCreate(t *testing.T) {
	_, rsaPEM := rsaKeyPEM(t)
	for name, bad := range map[string]string{"garbage": "-----BEGIN OPENSSH PRIVATE KEY-----\nnot a key\n-----END OPENSSH PRIVATE KEY-----\n", "rsa": rsaPEM} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			cmd := create("ins-cccccccc")
			cmd.HostKeyPEM = bad
			res := e.run(cmd)
			if len(res) != 1 || res[0].OK || !strings.Contains(res[0].Message, "主机密钥") {
				t.Fatalf("create = %+v", res)
			}
			if strings.Contains(res[0].Message, "PRIVATE KEY") {
				t.Fatalf("error leaks key material: %q", res[0].Message)
			}
			if _, err := os.Stat(filepath.Join(e.dir, "ssh", "ins-cccccccc", "hostkeys", "ssh_host_ed25519_key")); !os.IsNotExist(err) {
				t.Fatalf("host key file should not exist: %v", err)
			}
		})
	}
}

// rsaKeyPEM 生成一把 OpenSSH 格式的 RSA 私钥（合法 PEM，但不是 ed25519）。
func rsaKeyPEM(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(k, "")
	if err != nil {
		t.Fatal(err)
	}
	return k, string(pem.EncodeToMemory(block))
}

func TestImagesReportedFromRuntime(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if got := e.ex.Images(ctx); got != nil {
		t.Fatalf("unset = %#v", got)
	}
	e.rt.SetImages([]string{"a:1"})
	if got := e.ex.Images(ctx); !slices.Equal(got, []string{"a:1"}) {
		t.Fatalf("images = %v", got)
	}
	e.rt.FailNext("images", errors.New("docker down"))
	if got := e.ex.Images(ctx); got != nil {
		t.Fatalf("on error = %v, want nil", got)
	}
	// dockerd 卡死：受 reportTimeout 约束，返回 nil
	executor.SetReportTimeoutForTest(e.ex, 50*time.Millisecond)
	e.rt.Block("images", true)
	start := time.Now()
	if got := e.ex.Images(ctx); got != nil || time.Since(start) > 2*time.Second {
		t.Fatalf("blocked = %v after %v", got, time.Since(start))
	}
}
