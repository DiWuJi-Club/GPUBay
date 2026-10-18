package executor_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	diskfake "spark-cloud/internal/agent/disk/fake"
	"spark-cloud/internal/agent/executor"
	netfake "spark-cloud/internal/agent/netfilter/fake"
	"spark-cloud/internal/agent/runtime"
	rtfake "spark-cloud/internal/agent/runtime/fake"
	"spark-cloud/internal/agent/state"
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
	for _, want := range []string{"#!/bin/sh", "HostKey /.spark/hostkeys/ssh_host_ed25519_key", "AuthorizedKeysFile /.spark/authorized_keys", "StrictModes no", "PasswordAuthentication no", "PermitRootLogin prohibit-password", "exec /usr/sbin/sshd -D -e"} {
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
	entered chan struct{}
	release chan struct{}
}

func (r *countingRuntime) Create(ctx context.Context, s runtime.Spec) error {
	r.creates.Add(1)
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
	case <-time.After(1500 * time.Millisecond): // 消费者 1 秒轮询一次，留足时间让误入队的重复指令暴露
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
	defer cancel()
	go e.ex.Run(ctx)
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
