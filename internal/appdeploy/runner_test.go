package appdeploy_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"spark-cloud/internal/access/sshtest"
	"spark-cloud/internal/access/upstream"
	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/rental"
)

// ---- Runner 测试环境 ----

// syncBuf 是并发安全的日志缓冲（Runner 的多个 goroutine 同时写日志）。
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type runnerEnv struct {
	*env
	srv        *sshtest.Server
	up         *upstream.Pool
	svc        *appdeploy.Service
	r          *appdeploy.Runner
	cfg        appdeploy.RunnerConfig
	logs       *syncBuf
	healthAddr string
	healthFail atomic.Bool // true → /health 返回 500
}

// newRunnerEnv：Task 8 的 env + shell 模式 sshtest（每个实例的应用根目录 <Root>/<实例>/root/data）+ 上游池 + Runner。
// run.sh 用 setsid 脱离会话，sshtest.Close 杀不到：Cleanup 在宿主机上对每个应用目录执行 run.sh stop。
func newRunnerEnv(t *testing.T, machines int, tweak ...func(*appdeploy.RunnerConfig)) *runnerEnv {
	t.Helper()
	e := newEnv(t, machines)
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := sshtest.New(sshtest.Options{AuthorizedKey: signer.PublicKey(), Shell: true, Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	up := upstream.New(upstream.Config{Signer: signer, Port: srv.Port(), DialTimeout: 500 * time.Millisecond}, e.rental)
	t.Cleanup(up.Close)
	re := &runnerEnv{env: e, srv: srv, up: up, logs: &syncBuf{}}
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" && !re.healthFail.Load() {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(hs.Close)
	re.healthAddr = strings.TrimPrefix(hs.URL, "http://")
	srv.SetTarget(8000, re.healthAddr)
	root := srv.Root()
	appRootFor := func(id string) string { return filepath.Join(root, id, "root", "data") }
	re.svc = appdeploy.NewService(appdeploy.Deps{Pool: e.pool, Clock: e.clk, Box: e.box, Log: slog.New(slog.NewTextHandler(re.logs, nil)),
		Catalog: e.cat, Images: e.catalog, Rental: e.rental, Fleet: e.fleet, Upstream: up, AppRootFor: appRootFor})
	re.cfg = appdeploy.RunnerConfig{AppRootFor: appRootFor, HealthAddr: func(int) string { return re.healthAddr }}
	for _, f := range tweak {
		f(&re.cfg)
	}
	re.r = appdeploy.NewRunner(re.svc, up, re.cfg)
	t.Cleanup(func() { stopAllApps(root) })
	return re
}

// stopAllApps 在宿主机上对每个应用目录执行 run.sh stop（杀掉 setsid 出去的 _bg 与服务进程）。
func stopAllApps(root string) {
	scripts, _ := filepath.Glob(filepath.Join(root, "*", "root", "data", appdeploy.AppDirName, "*", "run.sh"))
	var wg sync.WaitGroup
	for _, s := range scripts {
		wg.Add(1)
		go func(s string) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			exec.CommandContext(ctx, "sh", s, "stop").Run() //nolint:errcheck
		}(s)
	}
	wg.Wait()
}

func (re *runnerEnv) root(instanceID string) string {
	return filepath.Join(re.srv.Root(), instanceID, "root", "data")
}

func (re *runnerEnv) dir(instanceID, slug string) string {
	return appdeploy.AppDir(re.root(instanceID), slug)
}

// instance 建一个 running 实例，连接目标指向 sshtest。
func (re *runnerEnv) instance() rental.Instance {
	re.t.Helper()
	inst := re.runningInstance()
	re.exec(`UPDATE machines SET lan_ip = '127.0.0.1' WHERE id = $1`, inst.MachineID)
	re.exec(`UPDATE instances SET ssh_host_pubkey = $2 WHERE id = $1`, inst.ID, re.srv.HostPubLine())
	return inst
}

func (re *runnerEnv) tick() int {
	re.t.Helper()
	n, locked, err := re.r.Tick(re.ctx)
	if err != nil || !locked {
		re.t.Fatalf("Tick = %d %v %v\nlogs:\n%s", n, locked, err, re.logs.String())
	}
	return n
}

// until 循环 Tick 直到部署进入 want（≤ 30 秒；每轮推进 PollInterval，并真实等 50ms 让容器内脚本推进）。
func (re *runnerEnv) until(id, want string) appdeploy.Deployment {
	re.t.Helper()
	return re.untilFn(id, want, func(d appdeploy.Deployment) bool { return d.State == want })
}

func (re *runnerEnv) untilFn(id, what string, ok func(appdeploy.Deployment) bool) appdeploy.Deployment {
	re.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		re.tick()
		d := re.dep(id)
		if ok(d) {
			return d
		}
		if time.Now().After(deadline) {
			re.t.Fatalf("deployment %s never reached %s: %+v\nlogs:\n%s", id, what, d, re.logs.String())
		}
		re.clk.Advance(appdeploy.PollInterval)
		time.Sleep(50 * time.Millisecond)
	}
}

// waitContainer 按真实时间等容器内状态满足 ok（不推进假时钟：部署超时按假时钟计，机器慢时不能被测试自己推超时）。
func (re *runnerEnv) waitContainer(dir string, ok func(appdeploy.Status) bool) {
	re.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		out, _ := exec.Command("sh", filepath.Join(dir, "run.sh"), "status").Output()
		if st, _, _, err := appdeploy.ParseStatusOutput(append(out, []byte("---\n")...)); err == nil && ok(st) {
			return
		}
		if time.Now().After(deadline) {
			re.t.Fatalf("container %s: status never matched: %s", dir, out)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// downloading：一轮 Tick 部署（queued → installing），等容器里的 install 汇报了 download 并打出 key= 行，
// 再推进一个 PollInterval 轮询一次；返回此时（phase = download）的部署。
func (re *runnerEnv) downloading(id, dir string) appdeploy.Deployment {
	re.t.Helper()
	re.tick()
	re.waitContainer(dir, func(st appdeploy.Status) bool {
		b, _ := os.ReadFile(filepath.Join(dir, "log"))
		return st.Phase == "download" && strings.Contains(string(b), "key=")
	})
	re.clk.Advance(appdeploy.PollInterval)
	re.tick()
	d := re.dep(id)
	if d.State != appdeploy.StateInstalling || d.Phase != "download" {
		re.t.Fatalf("not downloading: %+v\nlogs:\n%s", d, re.logs.String())
	}
	return d
}

func (re *runnerEnv) events(id string) []string {
	re.t.Helper()
	rows, err := re.pool.Query(re.ctx, `SELECT to_state FROM app_deploy_events WHERE deployment_id = $1 ORDER BY id`, id)
	if err != nil {
		re.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			re.t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func (re *runnerEnv) lastEventDetail(id string) string {
	re.t.Helper()
	var s string
	if err := re.pool.QueryRow(re.ctx, `SELECT detail FROM app_deploy_events WHERE deployment_id = $1 ORDER BY id DESC LIMIT 1`, id).Scan(&s); err != nil {
		re.t.Fatal(err)
	}
	return s
}

// servicePid 读容器目录里的 service.pid。
func (re *runnerEnv) servicePid(dir string) int {
	re.t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "service.pid"))
	if err != nil {
		re.t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 {
		re.t.Fatalf("service.pid = %q", b)
	}
	return pid
}

// hostStatus 在宿主机上执行 run.sh status 并解析（容器内实际状态）。
func hostStatus(t *testing.T, dir string) (appdeploy.Status, bool) {
	t.Helper()
	out, _ := exec.Command("sh", filepath.Join(dir, "run.sh"), "status").Output()
	st, alive, _, err := appdeploy.ParseStatusOutput(append(out, []byte("---\n")...))
	if err != nil {
		t.Fatalf("status %q: %v", out, err)
	}
	return st, alive
}

// waitGone 等进程（或 pid < 0 时的进程组）消失。
func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("process %d still alive", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

const runnerScript = `#!/bin/sh
set -e
case "$1" in
  install)
    spark_status download 50 "downloading $MODEL_ID ${API_KEY:-}"
    echo "key=${API_KEY:-none}"
    sleep "${INSTALL_SLEEP:-0}"
    ;;
  start)
    sleep 100000 </dev/null >/dev/null 2>&1 &
    echo $! > "$SPARK_APP_DIR/service.pid"
    ;;
  stop) ;;
esac
`

const failScript = `#!/bin/sh
case "$1" in
  install) echo "boom: cannot download" >&2; exit 3 ;;
  start) exit 1 ;;
esac
`

// runnerSpec：主端口（带 /health）、MODEL_ID、INSTALL_SLEEP（int）、API_KEY（secret，可不填）。
func runnerSpec(port int) appdeploy.Spec {
	return appdeploy.Spec{
		Ports: []appdeploy.PortSpec{{Port: port, HealthPath: "/health"}},
		Env: []appdeploy.EnvSpec{
			{Name: "MODEL_ID", Label: "模型", Type: "enum", Options: []string{"Qwen/Qwen3-8B", "Qwen/Qwen3-4B"}, Default: "Qwen/Qwen3-8B"},
			{Name: "INSTALL_SLEEP", Label: "安装耗时", Type: "int", Default: "0"},
			{Name: "API_KEY", Label: "接口密钥", Type: "secret"},
		},
	}
}

func (re *runnerEnv) app(slug string, port int) appdeploy.App {
	re.t.Helper()
	a, _ := re.publishedApp(slug, runnerSpec(port), runnerScript, []int64{re.imageID})
	return a
}

// ---- 用例 ----

func TestRunnerQueuedToRunningRegistersPort(t *testing.T) {
	re := newRunnerEnv(t, 1)
	a := re.app("vllm", 8000)
	inst := re.instance()
	d := re.create(inst, a.ID, nil)
	t0 := re.clk.Now()
	if n := re.tick(); n != 1 {
		t.Fatalf("processed = %d", n)
	}
	got := re.dep(d.ID)
	if got.State != appdeploy.StateInstalling || got.Attempt != 1 || got.StartedAt == nil || !got.StartedAt.Equal(t0) ||
		!got.NextPollAt.Equal(t0.Add(appdeploy.PollInterval)) || got.Phase != "prepare" {
		t.Fatalf("after first tick = %+v", got)
	}
	dir := re.dir(inst.ID, "vllm")
	for _, f := range []string{"run.sh", "app.sh", "spec.json", "env"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("bundle file %s: %v", f, err)
		}
	}
	fi, err := os.Stat(filepath.Join(dir, "env"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("env mode = %v %v", fi.Mode(), err)
	}
	envb, _ := os.ReadFile(filepath.Join(dir, "env"))
	if !strings.Contains(string(envb), "export SPARK_HEALTH_ADDR='"+re.healthAddr+"'") {
		t.Fatalf("env = %s", envb)
	}
	got = re.until(d.ID, appdeploy.StateRunning)
	now := re.clk.Now()
	if got.Health != appdeploy.HealthOK || got.Progress != 100 || got.FinishedAt == nil || !got.FinishedAt.Equal(now) ||
		!strings.Contains(got.LogTail, "downloading") || !got.NextPollAt.Equal(now.Add(appdeploy.HealthInterval)) || got.ErrorCode != "" {
		t.Fatalf("running = %+v", got)
	}
	var vis, note string
	var depID *string
	if err := re.pool.QueryRow(re.ctx, `SELECT visibility, note, deployment_id FROM instance_ports WHERE instance_id = $1 AND port = 8000`, inst.ID).
		Scan(&vis, &note, &depID); err != nil {
		t.Fatal(err)
	}
	if vis != "private" || note != "vllm" || depID == nil || *depID != d.ID {
		t.Fatalf("port row = %s %s %v", vis, note, depID)
	}
	if ev := re.events(d.ID); !slices.Equal(ev, []string{"queued", "installing", "running"}) {
		t.Fatalf("events = %v", ev)
	}
}

func TestRunnerInstallFailureAndRedeploy(t *testing.T) {
	re := newRunnerEnv(t, 1)
	a := re.env.app("vllm", appdeploy.StatusPublished)
	re.publish(a.ID, "1.0.0", runnerSpec(8000), failScript, []int64{re.imageID})
	inst := re.instance()
	d := re.create(inst, a.ID, nil)
	got := re.until(d.ID, appdeploy.StateFailed)
	if got.ErrorCode != appdeploy.ErrCodeScriptFailed || !strings.Contains(got.Message, "退出码 3") || !strings.Contains(got.LogTail, "boom") ||
		got.FinishedAt == nil || got.Phase != "install" {
		t.Fatalf("failed = %+v", got)
	}
	if detail := re.lastEventDetail(d.ID); !strings.HasPrefix(detail, "脚本执行失败：") {
		t.Fatalf("failed event = %q", detail)
	}
	v2 := re.publish(a.ID, "1.1.0", runnerSpec(8000), runnerScript, []int64{re.imageID})
	if _, err := re.svc.Redeploy(re.ctx, appdeploy.RedeployRequest{UserID: re.userID, InstanceID: inst.ID, DeploymentID: d.ID, VersionID: &v2.ID}); err != nil {
		t.Fatal(err)
	}
	got = re.until(d.ID, appdeploy.StateRunning)
	if got.Attempt != 2 || got.AppVersionID != v2.ID || got.ErrorCode != "" {
		t.Fatalf("redeployed = %+v", got)
	}
}

func TestRunnerUploadFailed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	re := newRunnerEnv(t, 1)
	a := re.app("vllm", 8000)
	inst := re.instance()
	base := filepath.Join(re.srv.Root(), inst.ID)
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(base, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(base, 0o700) })
	d := re.create(inst, a.ID, nil)
	re.tick()
	got := re.dep(d.ID)
	if got.State != appdeploy.StateFailed || got.ErrorCode != appdeploy.ErrCodeUploadFailed || !strings.HasPrefix(got.Message, "上传失败：mkdir") /* stderr 原文，随 locale 不同 */ || got.Attempt != 1 {
		t.Fatalf("upload failed = %+v", got)
	}
}

func TestRunnerUnreachableTenTimes(t *testing.T) {
	re := newRunnerEnv(t, 1)
	a := re.app("vllm", 8000)
	b := re.app("comfy", 8100)
	inst := re.instance()
	d1 := re.create(inst, a.ID, nil)
	d2 := re.create(inst, b.ID, nil)
	re.srv.Close()
	for i := 1; i < appdeploy.UnreachableFailAfter; i++ {
		now := re.clk.Now()
		re.tick()
		for _, id := range []string{d1.ID, d2.ID} {
			got := re.dep(id)
			// DialError 说明命令没有发出：attempt 回退到 0（没有执行过，卸载时不必清理）
			if got.State != appdeploy.StateQueued || got.UnreachableCount != i || got.Attempt != 0 || !got.NextPollAt.Equal(now.Add(appdeploy.UnreachableRetry)) {
				t.Fatalf("round %d: %+v", i, got)
			}
		}
		re.clk.Advance(appdeploy.UnreachableRetry)
	}
	re.tick()
	for _, id := range []string{d1.ID, d2.ID} {
		if got := re.dep(id); got.State != appdeploy.StateFailed || got.ErrorCode != appdeploy.ErrCodeUnreachable {
			t.Fatalf("after 10 = %+v", got)
		}
	}
	// 同一实例两个部署同一轮失败：告警按 kind + 文案去重（并按实例串行查重），只有一条
	alerts := func() int {
		return re.count(`SELECT count(*) FROM alerts WHERE kind = 'app_deploy_unreachable'`)
	}
	if n := alerts(); n != 1 {
		t.Fatalf("alerts after first failure = %d", n)
	}
	// 用户重新部署其中一个，再连续 10 次不可达：告警未处理前不重复
	if _, err := re.svc.Redeploy(re.ctx, appdeploy.RedeployRequest{UserID: re.userID, InstanceID: inst.ID, DeploymentID: d1.ID}); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= appdeploy.UnreachableFailAfter; i++ {
		re.tick()
		re.clk.Advance(appdeploy.UnreachableRetry)
	}
	if got := re.dep(d1.ID); got.State != appdeploy.StateFailed || got.ErrorCode != appdeploy.ErrCodeUnreachable {
		t.Fatalf("redeployed then unreachable = %+v", got)
	}
	if n := re.count(`SELECT count(*) FROM alerts WHERE kind = 'app_deploy_unreachable' AND instance_id = $1 AND machine_id = $2 AND message = $3`,
		inst.ID, inst.MachineID, rental.AlertAppDeployUnreachable(inst.ID)); n != 1 {
		t.Fatalf("alerts = %d", n)
	}
}

func TestRunnerTimeout(t *testing.T) {
	re := newRunnerEnv(t, 1)
	spec := runnerSpec(8000)
	spec.TimeoutMinutes = 1
	a, _ := re.publishedApp("vllm", spec, runnerScript, []int64{re.imageID})
	inst := re.instance()
	d := re.create(inst, a.ID, map[string]string{"INSTALL_SLEEP": "30"})
	dir := re.dir(inst.ID, "vllm")
	re.downloading(d.ID, dir)
	st, _ := hostStatus(t, dir)
	if st.Pid <= 1 {
		t.Fatalf("no _bg pgid in status: %+v", st)
	}
	re.clk.Advance(61 * time.Second)
	re.tick()
	got := re.dep(d.ID)
	if got.State != appdeploy.StateFailed || got.ErrorCode != appdeploy.ErrCodeTimeout || !strings.Contains(got.Message, "1 分钟") {
		t.Fatalf("timeout = %+v", got)
	}
	// Runner 超时后 exec 了 stop：_bg 进程组已死，容器内状态是 stopped（或 _bg 抢先写的 failed），alive=0
	waitGone(t, -st.Pid)
	if st, alive := hostStatus(t, dir); alive || (st.State != appdeploy.StateStopped && st.State != appdeploy.StateFailed) {
		t.Fatalf("container status = %+v alive=%v", st, alive)
	}
}

func TestRunnerStopStartRemove(t *testing.T) {
	re := newRunnerEnv(t, 1)
	a := re.app("vllm", 8000)
	inst := re.instance()
	d := re.create(inst, a.ID, nil)
	re.until(d.ID, appdeploy.StateRunning)
	dir := re.dir(inst.ID, "vllm")
	pid := re.servicePid(dir)
	if _, err := re.svc.Stop(re.ctx, re.userID, inst.ID, d.ID, "1.2.3.4"); err != nil {
		t.Fatal(err)
	}
	re.tick()
	if got := re.dep(d.ID); got.State != appdeploy.StateStopped || got.Health != appdeploy.HealthUnknown {
		t.Fatalf("stopped = %+v", got)
	}
	waitGone(t, pid)
	if n := re.count(`SELECT count(*) FROM instance_ports WHERE deployment_id = $1`, d.ID); n != 1 {
		t.Fatalf("ports after stop = %d", n)
	}
	if _, err := re.svc.Start(re.ctx, re.userID, inst.ID, d.ID, "1.2.3.4"); err != nil {
		t.Fatal(err)
	}
	got := re.until(d.ID, appdeploy.StateRunning)
	if !strings.Contains(got.LogTail, "skip install") || got.Attempt != 2 {
		t.Fatalf("restarted = %+v", got)
	}
	pid = re.servicePid(dir)
	if _, err := re.svc.Remove(re.ctx, re.userID, inst.ID, d.ID, "1.2.3.4"); err != nil {
		t.Fatal(err)
	}
	re.tick()
	if got := re.dep(d.ID); got.State != appdeploy.StateRemoved || got.CleanupPending {
		t.Fatalf("removed = %+v", got)
	}
	waitGone(t, pid)
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("app dir still there: %v", err)
	}
	if n := re.count(`SELECT count(*) FROM instance_ports WHERE instance_id = $1`, inst.ID); n != 0 {
		t.Fatalf("ports after remove = %d", n)
	}
	if logs := re.auditLogs("instance.port_delete", inst.ID); len(logs) != 1 {
		t.Fatalf("port_delete audits = %+v", logs)
	}
	if ev := re.events(d.ID); ev[len(ev)-1] != appdeploy.StateRemoved {
		t.Fatalf("events = %v", ev)
	}
}

func TestRunnerRemoveUnreachable(t *testing.T) {
	re := newRunnerEnv(t, 1)
	a := re.app("vllm", 8000)
	inst := re.instance()
	d := re.create(inst, a.ID, nil)
	re.until(d.ID, appdeploy.StateRunning)
	if _, err := re.svc.Remove(re.ctx, re.userID, inst.ID, d.ID, "1.2.3.4"); err != nil {
		t.Fatal(err)
	}
	re.srv.Close()
	for i := 1; i < appdeploy.UnreachableFailAfter; i++ {
		re.tick()
		if got := re.dep(d.ID); got.State != appdeploy.StateRemoving || got.UnreachableCount != i {
			t.Fatalf("round %d = %+v", i, got)
		}
		re.clk.Advance(appdeploy.UnreachableRetry)
	}
	re.tick()
	got := re.dep(d.ID)
	if got.State != appdeploy.StateRemoved || got.ErrorCode != appdeploy.ErrCodeUnreachable {
		t.Fatalf("removed = %+v", got)
	}
	if detail := re.lastEventDetail(d.ID); !strings.Contains(detail, "容器不可达，目录未清理") {
		t.Fatalf("event = %q", detail)
	}
	if n := re.count(`SELECT count(*) FROM instance_ports WHERE deployment_id = $1`, d.ID); n != 0 {
		t.Fatalf("ports = %d", n)
	}
	if n := re.count(`SELECT count(*) FROM alerts WHERE kind = 'app_deploy_unreachable'`); n != 0 {
		t.Fatalf("remove must not alert, alerts = %d", n)
	}
}

func TestRunnerCrashDetection(t *testing.T) {
	re := newRunnerEnv(t, 1)
	a := re.app("vllm", 8000)
	inst := re.instance()
	d := re.create(inst, a.ID, nil)
	re.until(d.ID, appdeploy.StateRunning)
	pid := re.servicePid(re.dir(inst.ID, "vllm"))
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitGone(t, pid)
	for i := 1; i < appdeploy.CrashedAfter; i++ {
		re.clk.Advance(appdeploy.HealthInterval)
		re.tick()
		if got := re.dep(d.ID); got.State != appdeploy.StateRunning || got.Health != appdeploy.HealthUnhealthy || got.CrashCount != i {
			t.Fatalf("round %d = %+v", i, got)
		}
	}
	re.clk.Advance(appdeploy.HealthInterval)
	re.tick()
	if got := re.dep(d.ID); got.State != appdeploy.StateFailed || got.ErrorCode != appdeploy.ErrCodeCrashed {
		t.Fatalf("crashed = %+v", got)
	}
}

func TestRunnerHealthHTTP(t *testing.T) {
	re := newRunnerEnv(t, 1)
	a := re.app("vllm", 8000)
	inst := re.instance()
	d := re.create(inst, a.ID, nil)
	re.until(d.ID, appdeploy.StateRunning)
	re.healthFail.Store(true) // 服务进程还在，但 GET /health 返回 500
	re.clk.Advance(appdeploy.HealthInterval)
	re.tick()
	if got := re.dep(d.ID); got.State != appdeploy.StateRunning || got.Health != appdeploy.HealthUnhealthy || got.CrashCount != 0 {
		t.Fatalf("unhealthy = %+v", got)
	}
	re.healthFail.Store(false)
	re.clk.Advance(appdeploy.HealthInterval)
	re.tick()
	if got := re.dep(d.ID); got.State != appdeploy.StateRunning || got.Health != appdeploy.HealthOK {
		t.Fatalf("healthy again = %+v", got)
	}
}

func TestRunnerSkipsWhenInstanceNotRunning(t *testing.T) {
	re := newRunnerEnv(t, 1)
	a := re.app("vllm", 8000)
	inst := re.instance()
	d := re.create(inst, a.ID, nil)
	re.exec(`UPDATE instances SET state = 'stopped' WHERE id = $1`, inst.ID)
	now := re.clk.Now()
	re.tick()
	if got := re.dep(d.ID); got.State != appdeploy.StateQueued || got.Attempt != 0 || !got.NextPollAt.Equal(now.Add(appdeploy.NotRunningRetry)) {
		t.Fatalf("stopped instance = %+v", got)
	}
	// 暂停中的 running 实例同样跳过
	re.exec(`UPDATE instances SET state = 'running', suspended_at = $2 WHERE id = $1`, inst.ID, re.clk.Now())
	re.clk.Advance(appdeploy.NotRunningRetry)
	re.tick()
	if got := re.dep(d.ID); got.State != appdeploy.StateQueued || got.Attempt != 0 {
		t.Fatalf("suspended instance = %+v", got)
	}
	if n := re.srv.ConnCount(); n != 0 || len(re.srv.ChannelOpens()) != 0 {
		t.Fatalf("exec happened: conns %d opens %v", n, re.srv.ChannelOpens())
	}
	re.exec(`UPDATE instances SET state = 'released', suspended_at = NULL WHERE id = $1`, inst.ID)
	re.clk.Advance(appdeploy.NotRunningRetry)
	re.tick()
	if got := re.dep(d.ID); got.State != appdeploy.StateRemoved || got.ErrorCode != appdeploy.ErrCodeInstanceGone {
		t.Fatalf("released instance = %+v", got)
	}
}

func TestRunnerDiscardsStaleWriteback(t *testing.T) {
	re := newRunnerEnv(t, 1)
	a := re.app("vllm", 8000)
	inst := re.instance()
	d := re.create(inst, a.ID, nil)
	var once sync.Once
	// exec 已完成、回写之前用户点了卸载（假时钟下 updated_at 不变，只有 row_version 能看出来）
	appdeploy.SetBeforeWriteback(re.r, func(appdeploy.Deployment) {
		once.Do(func() {
			if _, err := re.svc.Remove(re.ctx, re.userID, inst.ID, d.ID, "1.2.3.4"); err != nil {
				t.Errorf("Remove: %v", err)
			}
		})
	})
	re.tick()
	got := re.dep(d.ID)
	if got.State != appdeploy.StateRemoving || got.Attempt != 1 {
		t.Fatalf("after stale writeback = %+v", got)
	}
	if ev := re.events(d.ID); slices.Contains(ev, appdeploy.StateInstalling) {
		t.Fatalf("installing writeback was not discarded: %v", ev)
	}
	if !strings.Contains(re.logs.String(), "stale") {
		t.Fatalf("no stale log: %s", re.logs.String())
	}
	appdeploy.SetBeforeWriteback(re.r, nil)
	re.tick()
	if got := re.dep(d.ID); got.State != appdeploy.StateRemoved {
		t.Fatalf("removed = %+v", got)
	}
	if _, err := os.Stat(re.dir(inst.ID, "vllm")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("app dir still there: %v", err)
	}
}

func TestRunnerClaimsMigratedPort(t *testing.T) {
	re := newRunnerEnv(t, 2)
	a := re.app("vllm", 8000)
	inst := re.instance()
	d := re.create(inst, a.ID, nil)
	// 换机复制来的无主端口行
	re.exec(`INSERT INTO instance_ports (instance_id, port, visibility, note, created_at, updated_at) VALUES ($1, 8000, 'private', 'old', $2, $2)`, inst.ID, re.clk.Now())
	re.until(d.ID, appdeploy.StateRunning)
	var note string
	var depID *string
	if err := re.pool.QueryRow(re.ctx, `SELECT note, deployment_id FROM instance_ports WHERE instance_id = $1 AND port = 8000`, inst.ID).Scan(&note, &depID); err != nil {
		t.Fatal(err)
	}
	if note != "vllm" || depID == nil || *depID != d.ID || re.count(`SELECT count(*) FROM instance_ports WHERE instance_id = $1`, inst.ID) != 1 {
		t.Fatalf("claimed row = %q %v", note, depID)
	}
	// 端口行属于别的部署 → port_conflict
	b := re.app("comfy", 8100)
	inst2 := re.instance()
	d2 := re.create(inst2, a.ID, nil)
	other := re.create(inst2, b.ID, nil)
	re.exec(`UPDATE app_deployments SET state = 'stopped' WHERE id = $1`, other.ID)
	re.exec(`INSERT INTO instance_ports (instance_id, port, visibility, note, deployment_id, created_at, updated_at) VALUES ($1, 8000, 'private', 'comfy', $2, $3, $3)`,
		inst2.ID, other.ID, re.clk.Now())
	re.tick() // queued → installing
	dir2 := re.dir(inst2.ID, "vllm")
	re.waitContainer(dir2, func(st appdeploy.Status) bool { return st.State == appdeploy.StateRunning })
	pid := re.servicePid(dir2)
	re.clk.Advance(appdeploy.PollInterval)
	re.tick()
	got := re.dep(d2.ID)
	// failed(port_conflict) 已提交，随后 run.sh stop 停掉已经起来的服务；停成功后「待停止」标记清除（desired 回到 running）
	if got.State != appdeploy.StateFailed || got.ErrorCode != appdeploy.ErrCodePortConflict || got.Desired != appdeploy.DesiredRunning {
		t.Fatalf("conflict = %+v", got)
	}
	waitGone(t, pid)
	if st, alive := hostStatus(t, dir2); alive || st.State != appdeploy.StateStopped {
		t.Fatalf("container after conflict = %+v alive=%v", st, alive)
	}
	if n := re.count(`SELECT count(*) FROM instance_ports WHERE instance_id = $1 AND deployment_id = $2`, inst2.ID, d2.ID); n != 0 {
		t.Fatalf("conflicting deployment registered %d ports", n)
	}
}

func TestRunnerSeqRegressionIgnored(t *testing.T) {
	re := newRunnerEnv(t, 1)
	a := re.app("vllm", 8000)
	inst := re.instance()
	d := re.create(inst, a.ID, map[string]string{"INSTALL_SLEEP": "30"})
	got := re.downloading(d.ID, re.dir(inst.ID, "vllm"))
	if got.Progress != 50 || got.StatusSeq < 2 {
		t.Fatalf("download = %+v", got)
	}
	stale := `{"seq":1,"state":"installing","phase":"zzz","progress":7,"message":"old","pid":0,"service_pid":0,"spec_hash":"","exit_code":0,"at":""}` + "\n"
	if err := os.WriteFile(filepath.Join(re.dir(inst.ID, "vllm"), "status.json"), []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}
	re.clk.Advance(appdeploy.PollInterval)
	re.tick()
	if after := re.dep(d.ID); after.Phase != "download" || after.Progress != 50 || after.StatusSeq != got.StatusSeq || after.State != appdeploy.StateInstalling {
		t.Fatalf("regressed status applied: %+v", after)
	}
}

func TestRunnerRedactsSecrets(t *testing.T) {
	re := newRunnerEnv(t, 1)
	a := re.app("vllm", 8000)
	inst := re.instance()
	d := re.create(inst, a.ID, map[string]string{"API_KEY": "abcdefgh12", "INSTALL_SLEEP": "30"})
	got := re.downloading(d.ID, re.dir(inst.ID, "vllm"))
	if strings.Contains(got.LogTail, "abcdefgh12") || !strings.Contains(got.LogTail, "key=***") {
		t.Fatalf("log_tail not redacted: %q", got.LogTail)
	}
	if strings.Contains(got.Message, "abcdefgh12") || !strings.Contains(got.Message, "***") {
		t.Fatalf("message not redacted: %q", got.Message)
	}
	dir := re.dir(inst.ID, "vllm")
	// 容器里确实是明文（证明脱敏发生在平台侧）
	if b, _ := os.ReadFile(filepath.Join(dir, "log")); !strings.Contains(string(b), "key=abcdefgh12") {
		t.Fatalf("container log = %s", b)
	}
	// phase 同样脱敏
	st := `{"seq":999,"state":"installing","phase":"p-abcdefgh12","progress":60,"message":"m","pid":0,"service_pid":0,"spec_hash":"","exit_code":0,"at":""}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "status.json"), []byte(st), 0o644); err != nil {
		t.Fatal(err)
	}
	re.clk.Advance(appdeploy.PollInterval)
	re.tick()
	if got := re.dep(d.ID); got.Phase != "p-***" {
		t.Fatalf("phase not redacted: %+v", got)
	}
}

// TestRunnerRotatedSecretNotInLogTail：重新部署换了 secret 后，旧值不再出现在 log_tail（deploy 换新日志，旧的留在 log.1）。
func TestRunnerRotatedSecretNotInLogTail(t *testing.T) {
	re := newRunnerEnv(t, 1)
	a := re.app("vllm", 8000)
	inst := re.instance()
	d := re.create(inst, a.ID, map[string]string{"API_KEY": "old-secret-1"})
	re.until(d.ID, appdeploy.StateRunning)
	if _, err := re.svc.Redeploy(re.ctx, appdeploy.RedeployRequest{UserID: re.userID, InstanceID: inst.ID, DeploymentID: d.ID,
		Env: map[string]string{"API_KEY": "new-secret-2"}, EnvSet: true}); err != nil {
		t.Fatal(err)
	}
	got := re.until(d.ID, appdeploy.StateRunning)
	if strings.Contains(got.LogTail, "old-secret-1") || strings.Contains(got.LogTail, "new-secret-2") || !strings.Contains(got.LogTail, "key=***") {
		t.Fatalf("log_tail = %q", got.LogTail)
	}
	if b, _ := os.ReadFile(filepath.Join(re.dir(inst.ID, "vllm"), "log.1")); !strings.Contains(string(b), "key=old-secret-1") {
		t.Fatalf("previous log not kept in log.1: %s", b)
	}
}

func TestRunnerAdvisoryLock(t *testing.T) {
	re := newRunnerEnv(t, 1)
	conn, err := re.pool.Acquire(re.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(re.ctx, `SELECT pg_advisory_lock(hashtext('appdeploy'))`); err != nil {
		t.Fatal(err)
	}
	if n, locked, err := re.r.Tick(re.ctx); err != nil || locked || n != 0 {
		t.Fatalf("tick while locked = %d %v %v", n, locked, err)
	}
	if _, err := conn.Exec(re.ctx, `SELECT pg_advisory_unlock(hashtext('appdeploy'))`); err != nil {
		t.Fatal(err)
	}
	if _, locked, err := re.r.Tick(re.ctx); err != nil || !locked {
		t.Fatalf("tick after unlock = %v %v", locked, err)
	}
	// 互斥：Runner A 在一轮中途（回写之前）停住时，Runner B 的 Tick 拿不到锁、什么也不做；A 结束后 B 拿得到
	a := re.app("vllm", 8000)
	d := re.create(re.instance(), a.ID, nil)
	inside, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	appdeploy.SetBeforeWriteback(re.r, func(appdeploy.Deployment) {
		once.Do(func() { close(inside); <-release })
	})
	r2 := appdeploy.NewRunner(re.svc, re.up, re.cfg)
	type result struct {
		n      int
		locked bool
		err    error
	}
	resA := make(chan result, 1)
	go func() {
		n, locked, err := re.r.Tick(re.ctx)
		resA <- result{n, locked, err}
	}()
	<-inside
	re.clk.Advance(time.Hour) // 哪怕 B 看得到到点的部署
	if n, locked, err := r2.Tick(re.ctx); err != nil || locked || n != 0 {
		t.Fatalf("B while A holds the lock = %d %v %v", n, locked, err)
	}
	close(release)
	if r := <-resA; r.err != nil || !r.locked || r.n != 1 {
		t.Fatalf("A = %+v", r)
	}
	if got := re.dep(d.ID); got.State != appdeploy.StateInstalling {
		t.Fatalf("A's step = %+v", got)
	}
	if _, locked, err := r2.Tick(re.ctx); err != nil || !locked {
		t.Fatalf("B after A = %v %v", locked, err)
	}
	// 锁没有泄漏在连接池里
	var ok bool
	if err := conn.QueryRow(re.ctx, `SELECT pg_try_advisory_lock(hashtext('appdeploy'))`).Scan(&ok); err != nil || !ok {
		t.Fatalf("lock leaked: %v %v", ok, err)
	}
	conn.Exec(re.ctx, `SELECT pg_advisory_unlock(hashtext('appdeploy'))`) //nolint:errcheck
}

func TestRunnerConcurrencyCap(t *testing.T) {
	const n = 20
	re := newRunnerEnv(t, n, func(c *appdeploy.RunnerConfig) { c.MaxConcurrent = 4 })
	a := re.app("vllm", 8000)
	var ids []string
	for i := 0; i < n; i++ {
		ids = append(ids, re.create(re.instance(), a.ID, nil).ID)
	}
	if got := re.tick(); got != n {
		t.Fatalf("processed = %d", got)
	}
	if peak := re.r.Stats().PeakConcurrent; peak > 4 || peak < 2 {
		t.Fatalf("peak concurrent = %d", peak)
	}
	for _, id := range ids {
		if d := re.dep(id); d.State != appdeploy.StateInstalling {
			t.Fatalf("%s = %+v", id, d)
		}
	}
}

func TestRunnerCleanupPending(t *testing.T) {
	re := newRunnerEnv(t, 2)
	a := re.app("vllm", 8000)
	inst := re.instance()
	d := re.create(inst, a.ID, nil)
	re.until(d.ID, appdeploy.StateRunning)
	dir := re.dir(inst.ID, "vllm")
	pid := re.servicePid(dir)
	// 停机时卸载留下的待清理目录（实例恢复 running 后 Runner 清理）：服务进程也要停掉，不只是删目录
	re.exec(`UPDATE app_deployments SET state = 'removed', desired = 'removed', cleanup_pending = true, next_poll_at = $2 WHERE id = $1`, d.ID, re.clk.Now())
	re.tick()
	if got := re.dep(d.ID); got.CleanupPending || got.State != appdeploy.StateRemoved {
		t.Fatalf("after cleanup = %+v", got)
	}
	waitGone(t, pid)
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("app dir still there: %v", err)
	}
	// 同一实例上同一应用已有新的非终态部署：目录属于新部署，不清理，只清标记
	inst2 := re.instance()
	old := re.create(inst2, a.ID, nil)
	re.exec(`UPDATE app_deployments SET state = 'removed', desired = 'removed' WHERE id = $1`, old.ID)
	live := re.create(inst2, a.ID, nil)
	re.exec(`UPDATE app_deployments SET state = 'stopped' WHERE id = $1`, live.ID)
	re.exec(`UPDATE app_deployments SET cleanup_pending = true, next_poll_at = $2 WHERE id = $1`, old.ID, re.clk.Now())
	dir2 := re.dir(inst2.ID, "vllm")
	if err := os.MkdirAll(dir2, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir2, "marker"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	re.tick()
	if got := re.dep(old.ID); got.CleanupPending {
		t.Fatalf("old cleanup still pending = %+v", got)
	}
	if _, err := os.Stat(filepath.Join(dir2, "marker")); err != nil {
		t.Fatalf("live deployment's dir was cleaned: %v", err)
	}
}

func TestRunnerStatusMissingFails(t *testing.T) {
	re := newRunnerEnv(t, 1)
	a := re.app("vllm", 8000)
	inst := re.instance()
	d := re.create(inst, a.ID, nil)
	re.until(d.ID, appdeploy.StateRunning)
	// 用户在容器里删了 status.json：run.sh status 退出 3，组合命令必须保留这个退出码（不能被 tail 的 0 盖掉）
	if err := os.Remove(filepath.Join(re.dir(inst.ID, "vllm"), "status.json")); err != nil {
		t.Fatal(err)
	}
	re.clk.Advance(appdeploy.HealthInterval)
	re.tick()
	if got := re.dep(d.ID); got.State != appdeploy.StateFailed || got.ErrorCode != appdeploy.ErrCodeScriptFailed || got.Message != "状态文件缺失" {
		t.Fatalf("status missing = %+v", got)
	}
}

// waitLockWaiter 等到库里有会话在等行锁（pg_stat_activity.wait_event_type = 'Lock'）。
func waitLockWaiter(t *testing.T, re *runnerEnv) {
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		if err := re.pool.QueryRow(re.ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&n); err != nil {
			t.Errorf("pg_stat_activity: %v", err)
			return
		}
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Error("no session ever waited for a lock")
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestRunnerWritebackLockOrder：回写登记端口时，另一个事务按「实例 FOR UPDATE → 部署行」的顺序加锁（用户操作 / rental 钩子）。
// 回写若只锁部署行，插入 instance_ports 的外键检查要对实例行取 KEY SHARE，两边成环，PostgreSQL 报死锁。
func TestRunnerWritebackLockOrder(t *testing.T) {
	re := newRunnerEnv(t, 1)
	a := re.app("vllm", 8000)
	inst := re.instance()
	d := re.create(inst, a.ID, nil)
	re.tick() // queued → installing
	dir := re.dir(inst.ID, "vllm")
	re.waitContainer(dir, func(st appdeploy.Status) bool { return st.State == appdeploy.StateRunning })
	var once sync.Once
	var otherErr error
	done := make(chan struct{})
	appdeploy.SetBeforeWriteback(re.r, func(appdeploy.Deployment) {
		once.Do(func() {
			tx, err := re.pool.Begin(re.ctx)
			if err != nil {
				otherErr = err
				close(done)
				return
			}
			if _, err := tx.Exec(re.ctx, `SELECT 1 FROM instances WHERE id = $1 FOR UPDATE`, inst.ID); err != nil {
				otherErr = err
			}
			go func() {
				defer close(done)
				waitLockWaiter(t, re) // 回写事务已在等锁（修复前：持部署行、等实例行；修复后：等实例行）
				if _, err := tx.Exec(re.ctx, `SELECT 1 FROM app_deployments WHERE id = $1 FOR UPDATE`, d.ID); err != nil {
					otherErr = err
					tx.Rollback(re.ctx) //nolint:errcheck
					return
				}
				otherErr = tx.Commit(re.ctx)
			}()
		})
	})
	re.clk.Advance(appdeploy.PollInterval)
	re.tick()
	<-done
	if otherErr != nil {
		t.Fatalf("concurrent instance → deployment locker failed: %v", otherErr)
	}
	if got := re.dep(d.ID); got.State != appdeploy.StateRunning {
		t.Fatalf("running = %+v", got)
	}
}

// TestRunnerTimeoutWithBadStatus：status.json 被写坏（解析失败）时安装超时照样生效，并停掉容器里的进程。
func TestRunnerTimeoutWithBadStatus(t *testing.T) {
	re := newRunnerEnv(t, 1)
	spec := runnerSpec(8000)
	spec.TimeoutMinutes = 1
	a, _ := re.publishedApp("vllm", spec, runnerScript, []int64{re.imageID})
	inst := re.instance()
	d := re.create(inst, a.ID, map[string]string{"INSTALL_SLEEP": "30"})
	dir := re.dir(inst.ID, "vllm")
	re.downloading(d.ID, dir)
	st, _ := hostStatus(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "status.json"), []byte("garbage{\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	re.clk.Advance(61 * time.Second)
	re.tick()
	if got := re.dep(d.ID); got.State != appdeploy.StateFailed || got.ErrorCode != appdeploy.ErrCodeTimeout || got.Desired != appdeploy.DesiredRunning {
		t.Fatalf("timeout with bad status = %+v", got)
	}
	waitGone(t, -st.Pid)
	if _, alive := hostStatus(t, dir); alive {
		t.Fatal("service alive after timeout")
	}
}

// TestRunnerHealthParseFailureUnknown：running 时 status.json 读不懂 → health unknown，state 不变，按健康周期再看。
func TestRunnerHealthParseFailureUnknown(t *testing.T) {
	re := newRunnerEnv(t, 1)
	a := re.app("vllm", 8000)
	inst := re.instance()
	d := re.create(inst, a.ID, nil)
	re.until(d.ID, appdeploy.StateRunning)
	if err := os.WriteFile(filepath.Join(re.dir(inst.ID, "vllm"), "status.json"), []byte("garbage{\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	re.clk.Advance(appdeploy.HealthInterval)
	now := re.clk.Now()
	re.tick()
	if got := re.dep(d.ID); got.State != appdeploy.StateRunning || got.Health != appdeploy.HealthUnknown || !got.NextPollAt.Equal(now.Add(appdeploy.HealthInterval)) {
		t.Fatalf("parse failure = %+v", got)
	}
}

// TestRunnerFailedStopRetried：失败后 stop 没执行成功时，failed 行带「待停止」标记（desired = stopped），Runner 之后补做 stop。
func TestRunnerFailedStopRetried(t *testing.T) {
	re := newRunnerEnv(t, 2)
	a := re.app("vllm", 8000)
	// 1) 带标记的 failed 行：下一轮执行 run.sh stop，停掉服务后清标记
	inst := re.instance()
	d := re.create(inst, a.ID, nil)
	re.until(d.ID, appdeploy.StateRunning)
	pid := re.servicePid(re.dir(inst.ID, "vllm"))
	re.exec(`UPDATE app_deployments SET state = 'failed', desired = 'stopped', error_code = 'timeout', next_poll_at = $2 WHERE id = $1`, d.ID, re.clk.Now())
	now := re.clk.Now()
	re.tick()
	waitGone(t, pid)
	if got := re.dep(d.ID); got.State != appdeploy.StateFailed || got.Desired != appdeploy.DesiredRunning || !got.NextPollAt.Equal(now.Add(24*time.Hour)) {
		t.Fatalf("after retried stop = %+v", got)
	}
	// 2) 端口冲突标 failed 后容器连不上，stop 做不了：标记保留，30 秒后重试
	b := re.app("comfy", 8100)
	inst2 := re.instance()
	d2 := re.create(inst2, a.ID, nil)
	other := re.create(inst2, b.ID, nil)
	re.exec(`UPDATE app_deployments SET state = 'stopped' WHERE id = $1`, other.ID)
	re.exec(`INSERT INTO instance_ports (instance_id, port, visibility, note, deployment_id, created_at, updated_at) VALUES ($1, 8000, 'private', 'comfy', $2, $3, $3)`,
		inst2.ID, other.ID, re.clk.Now())
	re.tick()
	re.waitContainer(re.dir(inst2.ID, "vllm"), func(st appdeploy.Status) bool { return st.State == appdeploy.StateRunning })
	var once sync.Once
	appdeploy.SetBeforeWriteback(re.r, func(appdeploy.Deployment) { once.Do(re.srv.Close) })
	re.clk.Advance(appdeploy.PollInterval)
	now = re.clk.Now()
	re.tick()
	got := re.dep(d2.ID)
	if got.State != appdeploy.StateFailed || got.ErrorCode != appdeploy.ErrCodePortConflict || got.Desired != appdeploy.DesiredStopped ||
		!got.NextPollAt.Equal(now.Add(appdeploy.UnreachableRetry)) {
		t.Fatalf("failed with pending stop = %+v", got)
	}
	re.clk.Advance(appdeploy.UnreachableRetry)
	re.tick()
	if got := re.dep(d2.ID); got.Desired != appdeploy.DesiredStopped {
		t.Fatalf("pending stop dropped while unreachable = %+v", got)
	}
}

// TestRunnerDeployBusyKeepsQueued：上一次 deploy 还在容器里跑（run.sh 部署锁被活着的进程持有）→ 不失败、保持 queued，30 秒后再试。
func TestRunnerDeployBusyKeepsQueued(t *testing.T) {
	re := newRunnerEnv(t, 1)
	a := re.app("vllm", 8000)
	inst := re.instance()
	d := re.create(inst, a.ID, nil)
	dir := re.dir(inst.ID, "vllm")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	holder := exec.Command("sleep", "60")
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { holder.Process.Kill(); holder.Wait() })
	lock := filepath.Join(dir, ".deploy.lock")
	if err := os.Symlink(strconv.Itoa(holder.Process.Pid)+" "+strconv.FormatInt(time.Now().Unix(), 10), lock); err != nil {
		t.Fatal(err)
	}
	now := re.clk.Now()
	re.tick()
	if got := re.dep(d.ID); got.State != appdeploy.StateQueued || got.Attempt != 0 || got.ErrorCode != "" || !got.NextPollAt.Equal(now.Add(appdeploy.UnreachableRetry)) {
		t.Fatalf("busy deploy = %+v", got)
	}
	os.Remove(lock)
	re.clk.Advance(appdeploy.UnreachableRetry)
	re.tick()
	if got := re.dep(d.ID); got.State != appdeploy.StateInstalling || got.Attempt != 1 {
		t.Fatalf("after lock released = %+v", got)
	}
}
