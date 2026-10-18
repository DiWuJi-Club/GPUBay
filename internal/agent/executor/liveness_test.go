package executor_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	diskfake "spark-cloud/internal/agent/disk/fake"
	"spark-cloud/internal/agent/executor"
	"spark-cloud/internal/agent/runtime"
	rtfake "spark-cloud/internal/agent/runtime/fake"
)

// ---- I4：卡死的 dockerd 不能拖住上报 / 恢复 ----

func TestReportUnknownWhenDockerHangs(t *testing.T) {
	if executor.ReportTimeout != 30*time.Second {
		t.Fatalf("ReportTimeout = %v", executor.ReportTimeout)
	}
	e := newEnv(t)
	e.run(create("ins-aaaaaaaa"))
	executor.SetReportTimeoutForTest(e.ex, 100*time.Millisecond)
	e.rt.Block("list", true)
	start := time.Now()
	got := e.ex.Report(context.Background())
	if time.Since(start) > 2*time.Second {
		t.Fatalf("Report blocked for %v", time.Since(start))
	}
	if len(got) != 1 || got[0].InstanceID != "ins-aaaaaaaa" || got[0].Phase != "unknown" {
		t.Fatalf("report = %+v", got)
	}
}

// hangDisk 的 Mount 在 hang 为真时阻塞到 ctx 结束。
type hangDisk struct {
	*diskfake.Disk
	mu   sync.Mutex
	hang bool
}

func (d *hangDisk) setHang(v bool) { d.mu.Lock(); d.hang = v; d.mu.Unlock() }

func (d *hangDisk) Mount(ctx context.Context, id string) error {
	d.mu.Lock()
	h := d.hang
	d.mu.Unlock()
	if h {
		<-ctx.Done()
		return ctx.Err()
	}
	return d.Disk.Mount(ctx, id)
}

func TestRecoverStartTimeoutLeavesPendingAndRetries(t *testing.T) {
	if executor.RecoverStepTimeout != 2*time.Minute {
		t.Fatalf("RecoverStepTimeout = %v", executor.RecoverStepTimeout)
	}
	e := newEnv(t)
	e.run(create("ins-aaaaaaaa"))
	e.rt.Exit("ins-aaaaaaaa")
	executor.SetRecoverStepTimeoutForTest(e.ex, 100*time.Millisecond)
	e.rt.Block("start", true)
	start := time.Now()
	if err := e.ex.Recover(context.Background()); err == nil {
		t.Fatal("recover must report the hung start")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("Recover blocked for %v", time.Since(start))
	}
	if p := executor.RecoverPendingForTest(e.ex); len(p) != 1 {
		t.Fatalf("pending = %v", p)
	}
	e.rt.Block("start", false)
	stop := e.runLimitLoop()
	defer stop()
	waitFor(t, "container running", func() bool { return e.rt.Containers()["ins-aaaaaaaa"].Running })
	waitFor(t, "pending cleared", func() bool { return len(executor.RecoverPendingForTest(e.ex)) == 0 })
}

func TestRecoverMountTimeoutLeavesPendingAndRetries(t *testing.T) {
	e := newEnv(t)
	e.run(create("ins-aaaaaaaa"))
	e.rt.Exit("ins-aaaaaaaa")
	e.disk.Unmount("ins-aaaaaaaa")
	hd := &hangDisk{Disk: e.disk, hang: true}
	e.ex = executor.New(executor.Deps{Log: quiet, Runtime: e.rt, Disk: hd, Net: e.net, State: e.st, StateDir: e.dir, Bridge: "spark0",
		LocalIPs: func() []string { return []string{"192.168.1.221"} }, MemTotal: func() uint64 { return 128 << 30 }}, true)
	executor.SetRecoverStepTimeoutForTest(e.ex, 100*time.Millisecond)
	start := time.Now()
	if err := e.ex.Recover(context.Background()); err == nil {
		t.Fatal("recover must report the hung mount")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("Recover blocked for %v", time.Since(start))
	}
	if p := executor.RecoverPendingForTest(e.ex); len(p) != 1 || e.rt.Containers()["ins-aaaaaaaa"].Running {
		t.Fatalf("pending = %v", p)
	}
	hd.setHang(false)
	stop := e.runLimitLoop()
	defer stop()
	waitFor(t, "container running", func() bool { return e.rt.Containers()["ins-aaaaaaaa"].Running })
}

// deadlineRT 记录 Pull / Create 收到的 ctx 剩余时限。
type deadlineRT struct {
	*rtfake.Runtime
	mu            sync.Mutex
	pull, created time.Duration
}

func remaining(ctx context.Context) time.Duration {
	if d, ok := ctx.Deadline(); ok {
		return time.Until(d)
	}
	return -1
}

func (r *deadlineRT) Pull(ctx context.Context, ref string) error {
	r.mu.Lock()
	r.pull = remaining(ctx)
	r.mu.Unlock()
	return r.Runtime.Pull(ctx, ref)
}

func (r *deadlineRT) Create(ctx context.Context, s runtime.Spec) error {
	r.mu.Lock()
	r.created = remaining(ctx)
	r.mu.Unlock()
	return r.Runtime.Create(ctx, s)
}

// 指令带显式时限（与平台指令超时一致），使 cmdrun 的 10 分钟兜底不误伤大镜像 pull / 隐式拉取镜像的 create。
func TestCommandsGetExplicitDeadlines(t *testing.T) {
	e := newEnv(t)
	dr := &deadlineRT{Runtime: e.rt}
	e.ex = executor.New(executor.Deps{Log: quiet, Runtime: dr, Disk: e.disk, Net: e.net, State: e.st, StateDir: e.dir, Bridge: "spark0",
		LocalIPs: func() []string { return []string{"192.168.1.221"} }, MemTotal: func() uint64 { return 128 << 30 }}, true)
	e.ex.SetConfig(executor.Config{LanIP: "192.168.1.221", DNS: []string{"223.5.5.5"}, BandwidthMbps: 200, DataSizeGiB: 2048})
	res := e.run(executor.Command{ID: "cmd-pull", Type: "pull_image", ImageRef: "nvcr.io/x:2"}, create("ins-aaaaaaaa"))
	if len(res) != 2 || !res[0].OK || !res[1].OK {
		t.Fatalf("results = %+v", res)
	}
	dr.mu.Lock()
	defer dr.mu.Unlock()
	if dr.pull < 59*time.Minute || dr.pull > 60*time.Minute {
		t.Fatalf("pull deadline = %v, want ~60m", dr.pull)
	}
	if dr.created < 14*time.Minute || dr.created > 15*time.Minute {
		t.Fatalf("create deadline = %v, want ~15m", dr.created)
	}
}

// ---- M4：重启后待重试启动的实例报 unknown，不触发平台 container_down ----

func TestReportUnknownWhileRecoverPending(t *testing.T) {
	e, fr := recoverEnv(t)
	if ph := e.phases()["ins-aaaaaaaa"]; ph != "unknown" {
		t.Fatalf("pending phase = %q, want unknown", ph)
	}
	fr.fail.Store(false)
	stop := e.runLimitLoop()
	defer stop()
	waitFor(t, "pending cleared", func() bool { return len(executor.RecoverPendingForTest(e.ex)) == 0 })
	if ph := e.phases()["ins-aaaaaaaa"]; ph != "running" {
		t.Fatalf("after retry phase = %q, want running", ph)
	}
}

// 超过宽限期仍启动不了的实例如实报 stopped，让平台告警（不能永远藏在 unknown 后面）。
func TestReportRecoverPendingBeyondGraceShowsStopped(t *testing.T) {
	if executor.RecoverGrace != 10*time.Minute {
		t.Fatalf("RecoverGrace = %v", executor.RecoverGrace)
	}
	e, _ := recoverEnv(t)
	executor.SetRecoverGraceForTest(e.ex, 50*time.Millisecond)
	waitFor(t, "grace expired", func() bool { return e.phases()["ins-aaaaaaaa"] == "stopped" })
	if p := executor.RecoverPendingForTest(e.ex); len(p) != 1 {
		t.Fatalf("still retried: pending = %v", p)
	}
}

// ---- B1：重试循环尊重 ctx ----

func TestRecoverRetryStopsWhenContextDone(t *testing.T) {
	e, fr := recoverEnv(t)
	fr.fail.Store(false)
	before := fr.starts.Load()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	executor.RetryRecoverPendingForTest(e.ex, ctx)
	if fr.starts.Load() != before || len(executor.RecoverPendingForTest(e.ex)) != 1 {
		t.Fatal("retry ran with a cancelled context")
	}
}

// ---- B2 ----

func TestConnectNetworkHappensBeforeStart(t *testing.T) {
	e := newEnv(t)
	e.run(create("ins-aaaaaaaa"))
	e.rt.Exit("ins-aaaaaaaa")
	e.rt.SetNetworks("ins-aaaaaaaa", nil)
	n := len(e.rt.Ops())
	if res := e.run(executor.Command{ID: "cmd-start", InstanceID: "ins-aaaaaaaa", Type: "start_instance"}); !res[0].OK {
		t.Fatalf("start = %+v", res[0])
	}
	ops := e.rt.Ops()[n:]
	ci, si := slices.Index(ops, "connect ins-aaaaaaaa"), slices.Index(ops, "start ins-aaaaaaaa")
	if ci < 0 || si < 0 || ci > si {
		t.Fatalf("ops = %v, want connect before start", ops)
	}
	// 网络仍挂接时不再 connect（docker 会报 endpoint already exists）
	e.rt.Exit("ins-aaaaaaaa")
	if res := e.run(executor.Command{ID: "cmd-start2", InstanceID: "ins-aaaaaaaa", Type: "start_instance"}); !res[0].OK {
		t.Fatalf("second start = %+v", res[0])
	}
}

func TestRecoverVethFailureStopsAndRetries(t *testing.T) {
	e := newEnv(t)
	e.run(create("ins-aaaaaaaa"))
	e.rt.Exit("ins-aaaaaaaa")
	e.rt.FailNext("veth", errors.New("Device eth0 does not exist"))
	if err := e.ex.Recover(context.Background()); err == nil || !strings.Contains(err.Error(), "eth0") {
		t.Fatalf("recover err = %v", err)
	}
	if e.rt.Containers()["ins-aaaaaaaa"].Running {
		t.Fatal("container without veth must be stopped")
	}
	if p := executor.RecoverPendingForTest(e.ex); len(p) != 1 {
		t.Fatalf("pending = %v", p)
	}
	stop := e.runLimitLoop()
	defer stop()
	waitFor(t, "container running", func() bool { return e.rt.Containers()["ins-aaaaaaaa"].Running })
	waitFor(t, "pending cleared", func() bool { return len(executor.RecoverPendingForTest(e.ex)) == 0 })
	if !e.waitLimit(e.rt.Veth, 50) {
		t.Fatalf("limit = %v", e.net.Limits())
	}
}

// ctxStop 的 Stop 在 ctx 已结束时失败（真实 docker CLI 的行为）。
type ctxStop struct{ *rtfake.Runtime }

func (r *ctxStop) Stop(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return r.Runtime.Stop(ctx, id)
}

// 启动后检查超时（ctx 已到期）时仍要能把容器停掉。
func TestStopAfterFailureSurvivesExpiredContext(t *testing.T) {
	e := newEnv(t)
	e.run(create("ins-aaaaaaaa"))
	e.rt.Exit("ins-aaaaaaaa")
	e.ex = executor.New(executor.Deps{Log: quiet, Runtime: &ctxStop{e.rt}, Disk: e.disk, Net: e.net, State: e.st, StateDir: e.dir, Bridge: "spark0",
		LocalIPs: func() []string { return []string{"192.168.1.221"} }, MemTotal: func() uint64 { return 128 << 30 }}, true)
	executor.SetRecoverStepTimeoutForTest(e.ex, 100*time.Millisecond)
	e.rt.Block("veth", true)
	if err := e.ex.Recover(context.Background()); err == nil {
		t.Fatal("recover must report the hung veth lookup")
	}
	if e.rt.Containers()["ins-aaaaaaaa"].Running {
		t.Fatal("container left running after failed start checks")
	}
}

func TestUnspecifiedPublishAddressIsAlwaysPresent(t *testing.T) {
	e, sc, ips := ipEnv(t)
	e.rt.SetPublishIPs("ins-aaaaaaaa", []string{"0.0.0.0", "::"})
	ips.Store([]string{"127.0.0.1"})
	if res := e.run(executor.Command{ID: "cmd-start", InstanceID: "ins-aaaaaaaa", Type: "start_instance"}); !res[0].OK {
		t.Fatalf("start = %+v", res[0])
	}
	if sc.starts.Load() != 1 {
		t.Fatalf("starts = %d", sc.starts.Load())
	}
}

// ---- A1：单个实例出错不影响其它实例的限速巡检 ----

func TestEnsureLimitsContinuesAfterInstanceError(t *testing.T) {
	e := newEnv(t)
	a, b := create("ins-aaaaaaaa"), create("ins-bbbbbbbb")
	a.DataSizeGiB, b.DataSizeGiB = 100, 100
	b.BandwidthMbps = 70
	if res := e.run(a, b); !res[0].OK || !res[1].OK {
		t.Fatalf("create = %+v", res)
	}
	veth := e.rt.Veth // fake 里两个容器共用一个 veth 名
	e.net.Drop(veth)
	e.rt.FailNext("inspect", errors.New("docker inspect failed")) // 第一个实例（按 ID 排序）出错
	executor.EnsureLimitsForTest(e.ex, context.Background())
	if got := e.net.Limits()[veth]; got != 70 {
		t.Fatalf("limit = %d, want 70 from the second instance", got)
	}
}
