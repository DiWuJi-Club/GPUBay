package runtime_test

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/appdeploy/runtime"
)

type box struct {
	t      *testing.T
	root   string // 充当容器 /root/data
	dir    string // <root>/.spark-apps/testapp
	health *httptest.Server
}

func newBox(t *testing.T, app string, extra map[string]string) *box {
	t.Helper()
	root := t.TempDir()
	dir := appdeploy.AppDir(root, "testapp")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			fmt.Fprint(w, "ok")
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(hs.Close)
	os.WriteFile(filepath.Join(dir, "run.sh"), runtime.RunSH, 0o755)
	src, _ := os.ReadFile(filepath.Join("testdata", app))
	os.WriteFile(filepath.Join(dir, "app.sh"), src, 0o644)
	spec := appdeploy.Spec{Ports: []appdeploy.PortSpec{{Port: 18000, HealthPath: "/health"}}, TimeoutMinutes: 1}
	in := appdeploy.BundleInput{Slug: "testapp", InstanceID: "ins-test", AppRoot: root, SpecHash: "hash-1", Spec: spec,
		Env: map[string]string{"MODEL_ID": "Qwen/Qwen3-4B"}, HealthAddr: strings.TrimPrefix(hs.URL, "http://"), HFEndpoint: "https://hf-mirror.com", PipIndexURL: "https://pypi.tuna.tsinghua.edu.cn/simple"}
	for k, v := range extra {
		in.Env[k] = v
	}
	env := appdeploy.RenderEnv(appdeploy.ReservedEnv(in), []appdeploy.EnvKV{{Name: "MODEL_ID", Value: in.Env["MODEL_ID"]}})
	for k, v := range extra {
		env = append(env, []byte("export "+k+"="+appdeploy.ShellQuote(v)+"\n")...)
	}
	os.WriteFile(filepath.Join(dir, "env"), env, 0o600)
	b := &box{t: t, root: root, dir: dir, health: hs}
	t.Cleanup(func() {
		if _, err := os.Stat(b.dir); err == nil { // remove 之后目录已不在
			b.run("stop")
		}
	})
	return b
}

func (b *box) setEnv(name, value string) {
	p := filepath.Join(b.dir, "env")
	data, _ := os.ReadFile(p)
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	var out []string
	for _, l := range lines {
		if !strings.HasPrefix(l, "export "+name+"=") {
			out = append(out, l)
		}
	}
	out = append(out, "export "+name+"="+appdeploy.ShellQuote(value))
	os.WriteFile(p, []byte(strings.Join(out, "\n")+"\n"), 0o600)
}

func (b *box) run(args ...string) (string, int) {
	b.t.Helper()
	cmd := exec.Command("/bin/sh", append([]string{"run.sh"}, args...)...)
	cmd.Dir = b.dir
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		b.t.Fatalf("run %v: %v", args, err)
	}
	return string(out), code
}

type status struct {
	Seq        int64  `json:"seq"`
	State      string `json:"state"`
	Phase      string `json:"phase"`
	Progress   int    `json:"progress"`
	Message    string `json:"message"`
	Pid        int    `json:"pid"`
	ServicePid int    `json:"service_pid"`
	SpecHash   string `json:"spec_hash"`
	ExitCode   int    `json:"exit_code"`
}

func (b *box) status() (status, string) {
	b.t.Helper()
	out, _ := b.run("status")
	lines := strings.SplitN(out, "\n", 3)
	var st status
	if err := json.Unmarshal([]byte(lines[0]), &st); err != nil {
		b.t.Fatalf("status.json %q: %v", lines[0], err)
	}
	return st, out
}

func (b *box) waitState(want string) status {
	b.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		st, _ := b.status()
		if st.State == want {
			return st
		}
		if time.Now().After(deadline) {
			b.t.Fatalf("state = %+v, want %s; log:\n%s", st, want, b.log())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (b *box) log() string {
	data, _ := os.ReadFile(filepath.Join(b.dir, "log"))
	return string(data)
}

func alive(pid int) bool { return pid > 0 && syscall.Kill(pid, 0) == nil }

func TestRunShSyntax(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-n")
	cmd.Stdin = strings.NewReader(string(runtime.RunSH))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sh -n: %v\n%s", err, out)
	}
	for _, f := range []string{"testdata/app_ok.sh", "testdata/app_fail.sh"} {
		if out, err := exec.Command("/bin/sh", "-n", f).CombinedOutput(); err != nil {
			t.Fatalf("%s: %v %s", f, err, out)
		}
	}
}

func TestRunShDeployInstallsStartsAndBecomesRunning(t *testing.T) {
	b := newBox(t, "app_ok.sh", nil)
	start := time.Now()
	out, code := b.run("deploy")
	if code != 0 || time.Since(start) > 3*time.Second {
		t.Fatalf("deploy = %d %q after %v (must return immediately)", code, out, time.Since(start))
	}
	st := b.waitState("running")
	if st.Phase != "health" || st.Progress != 100 || st.SpecHash != "hash-1" || st.Seq < 3 || !alive(st.ServicePid) {
		t.Fatalf("running status = %+v", st)
	}
	if _, err := os.Stat(filepath.Join(b.dir, "installed.marker")); err != nil {
		t.Fatal("install not executed")
	}
	// .installed = "<spec_hash> <env 文件 cksum>"：env 变了（如换 MODEL_ID）也要重跑 install
	if got, _ := os.ReadFile(filepath.Join(b.dir, ".installed")); !strings.HasPrefix(string(got), "hash-1 ") || len(strings.Fields(string(got))) != 2 {
		t.Fatalf(".installed = %q", got)
	}
	log := b.log()
	for _, s := range []string{"==== deploy #1 ", "downloading Qwen/Qwen3-4B", "started pid"} {
		if !strings.Contains(log, s) {
			t.Fatalf("log missing %q:\n%s", s, log)
		}
	}
	_, statusOut := b.status()
	if !strings.Contains(statusOut, "\nalive=1\n") || !strings.Contains(statusOut, "\nlog_size=") {
		t.Fatalf("status output = %q", statusOut)
	}
}

func TestRunShRedeploySkipsInstall(t *testing.T) {
	b := newBox(t, "app_ok.sh", nil)
	b.run("deploy")
	first := b.waitState("running")
	os.Remove(filepath.Join(b.dir, "installed.marker"))
	b.run("deploy")
	b.waitState("installing")
	second := b.waitState("running")
	if second.ServicePid == first.ServicePid || alive(first.ServicePid) {
		t.Fatalf("old service not replaced: first=%d second=%d", first.ServicePid, second.ServicePid)
	}
	if _, err := os.Stat(filepath.Join(b.dir, "installed.marker")); err == nil {
		t.Fatal("install re-executed although .installed matches")
	}
	if !strings.Contains(b.log(), "skip install") {
		t.Fatalf("log:\n%s", b.log())
	}
	// spec_hash 变化 → 重跑 install
	b.setEnv("SPARK_SPEC_HASH", "hash-2")
	b.run("deploy")
	b.waitState("installing")
	third := b.waitState("running")
	if _, err := os.Stat(filepath.Join(b.dir, "installed.marker")); err != nil || third.SpecHash != "hash-2" {
		t.Fatalf("install not re-run for new hash: %+v", third)
	}
	// 只改 env（同 spec_hash，换模型）→ 也要重跑 install，否则新模型不会下载
	os.Remove(filepath.Join(b.dir, "installed.marker"))
	b.setEnv("MODEL_ID", "Qwen/Qwen3-8B")
	b.run("deploy")
	b.waitState("installing")
	b.waitState("running")
	if got, err := os.ReadFile(filepath.Join(b.dir, "installed.marker")); err != nil || !strings.Contains(string(got), "Qwen3-8B") {
		t.Fatalf("install not re-run after env change: %q %v\n%s", got, err, b.log())
	}
}

func TestRunShInstallFailure(t *testing.T) {
	b := newBox(t, "app_fail.sh", nil)
	b.run("deploy")
	st := b.waitState("failed")
	if st.ExitCode != 3 || !strings.Contains(b.log(), "boom: cannot download") {
		t.Fatalf("failed status = %+v log=%s", st, b.log())
	}
	if _, err := os.Stat(filepath.Join(b.dir, ".installed")); err == nil {
		t.Fatal(".installed written after failed install")
	}
}

func TestRunShTimeoutAndStop(t *testing.T) {
	b := newBox(t, "app_ok.sh", nil)
	b.setEnv("SPARK_HEALTH_ADDR", "127.0.0.1:1") // 健康检查永远不过
	b.setEnv("SPARK_TIMEOUT_S", "2")
	b.run("deploy")
	st := b.waitState("failed")
	if st.Phase != "health" || !strings.Contains(st.Message, "timeout") {
		t.Fatalf("timeout status = %+v", st)
	}
	for deadline := time.Now().Add(5 * time.Second); alive(st.ServicePid); { // 状态先写、服务随后被杀
		if time.Now().After(deadline) {
			t.Fatalf("service %d still alive after timeout", st.ServicePid)
		}
		time.Sleep(50 * time.Millisecond)
	}
	// stop 杀整个进程组：start 里起的 sleep 也没了
	b2 := newBox(t, "app_ok.sh", nil)
	b2.run("deploy")
	running := b2.waitState("running")
	if _, code := b2.run("stop"); code != 0 {
		t.Fatalf("stop = %d", code)
	}
	st2, out := b2.status()
	if st2.State != "stopped" || alive(running.ServicePid) || !strings.Contains(out, "\nalive=0\n") {
		t.Fatalf("after stop = %+v alive=%v", st2, alive(running.ServicePid))
	}
	for _, f := range []string{".pgid", "service.pid"} { // stop 之后不留可能被复用的 pid
		if _, err := os.Stat(filepath.Join(b2.dir, f)); err == nil {
			t.Fatalf("%s left after stop", f)
		}
	}
}

func TestRunShDiskCheckAndRemove(t *testing.T) {
	b := newBox(t, "app_ok.sh", nil)
	b.setEnv("SPARK_MIN_FREE_GIB", "100")
	b.setEnv("SPARK_TEST_DF_FREE_GIB", "5")
	out, code := b.run("deploy")
	st, _ := b.status()
	if code != 2 || st.State != "failed" || !strings.Contains(st.Message, "5") || !strings.Contains(out, "disk") {
		t.Fatalf("disk check: code=%d out=%q status=%+v", code, out, st)
	}
	if _, code := b.run("remove"); code != 0 {
		t.Fatalf("remove = %d", code)
	}
	if _, err := os.Stat(b.dir); err == nil {
		t.Fatal("dir still exists after remove")
	}
	// Runner 的形式：从别的工作目录用绝对路径调用 remove
	b3 := newBox(t, "app_ok.sh", nil)
	b3.run("deploy")
	running := b3.waitState("running")
	cmd := exec.Command("/bin/sh", filepath.Join(b3.dir, "run.sh"), "remove")
	cmd.Dir = b3.root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("remove by absolute path: %v %s", err, out)
	}
	if _, err := os.Stat(b3.dir); err == nil || alive(running.ServicePid) {
		t.Fatalf("remove left dir or service %d", running.ServicePid)
	}
	if _, err := os.Stat(filepath.Join(b3.root, appdeploy.AppDirName)); err != nil {
		t.Fatalf("remove deleted the parent .spark-apps: %v", err)
	}
	// 目录已不在时 run.sh 本身也不在，sh 报错退出（非 0）：Task 9 的 Runner 把「目录不存在」当作已删除
	cmd = exec.Command("/bin/sh", filepath.Join(b3.dir, "run.sh"), "remove")
	cmd.Dir = b3.root
	if err := cmd.Run(); err == nil {
		t.Fatal("remove of a missing dir unexpectedly succeeded")
	}
	// status 对缺失状态文件返回 3
	dir2 := appdeploy.AppDir(t.TempDir(), "x")
	os.MkdirAll(dir2, 0o700)
	os.WriteFile(filepath.Join(dir2, "run.sh"), runtime.RunSH, 0o755)
	cmd = exec.Command("/bin/sh", "run.sh", "status")
	cmd.Dir = dir2
	if err := cmd.Run(); err == nil || err.(*exec.ExitError).ExitCode() != 3 {
		t.Fatalf("status without file = %v", err)
	}
}

// Runner（Task 9）的组合状态命令必须保留 run.sh status 的退出码：日志在、status.json 不在时
// 不能被 tail 的 0 吞掉，否则平台会把「状态文件缺失」当成仍在安装一直轮询。
func TestRunShStatusMissingKeepsExitCodeThroughTail(t *testing.T) {
	dir := appdeploy.AppDir(t.TempDir(), "x")
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "run.sh"), runtime.RunSH, 0o755)
	os.WriteFile(filepath.Join(dir, "log"), []byte("old log line\n"), 0o600)
	cmd := exec.Command("/bin/sh", "-c", "cd "+appdeploy.ShellQuote(dir)+" && sh run.sh status; rc=$?; echo ---; tail -c 65536 log 2>/dev/null; exit $rc")
	out, err := cmd.Output()
	ee, ok := err.(*exec.ExitError)
	if !ok || ee.ExitCode() != 3 {
		t.Fatalf("exit = %v, want 3; out=%q", err, out)
	}
	first, _, _ := strings.Cut(string(out), "\n")
	var m map[string]any
	if err := json.Unmarshal([]byte(first), &m); err != nil {
		t.Fatalf("first line %q: %v", first, err)
	}
	if _, has := m["state"]; has || m["error"] != "no status" {
		t.Fatalf("missing status must not carry a state: %q", first)
	}
	if !strings.Contains(string(out), "\n---\nold log line\n") || !strings.Contains(string(out), "\nlog_size=13\n") {
		t.Fatalf("out = %q", out)
	}
}

func TestRunShTCPHealthWhenNoPath(t *testing.T) {
	b := newBox(t, "app_ok.sh", nil)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	b.setEnv("SPARK_HEALTH_PATH", "")
	b.setEnv("SPARK_HEALTH_ADDR", "127.0.0.1:"+strconv.Itoa(ln.Addr().(*net.TCPAddr).Port))
	b.run("deploy")
	b.waitState("running")
}

func TestRunShEnvIsNotEvaluated(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "pwned")
	b := newBox(t, "app_ok.sh", map[string]string{"NOTE": "'; touch " + marker + "; '"})
	b.run("deploy")
	b.waitState("running")
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("env value was executed")
	}
}

func TestSparkStatusAtomicAndMonotonic(t *testing.T) {
	b := newBox(t, "app_ok.sh", nil)
	b.run("deploy")
	b.waitState("running")
	var last int64
	for i := 0; i < 20; i++ {
		cmd := exec.Command("/bin/sh", "-c", ". ./env; PATH=$SPARK_APP_DIR:$PATH spark_status install "+strconv.Itoa(i)+" \"step $i\"")
		cmd.Dir = b.dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("spark_status: %v %s", err, out)
		}
		st, _ := b.status()
		if st.Seq <= last || st.Progress != i || st.Phase != "install" {
			t.Fatalf("seq not monotonic: %+v (last %d)", st, last)
		}
		last = st.Seq
	}
	if entries, _ := filepath.Glob(filepath.Join(b.dir, "status.json.*")); len(entries) != 0 {
		t.Fatalf("temp files left: %v", entries)
	}
}

// app.sh 传进来的任意文本（引号、反斜杠、控制字符、非数字进度）不能把 status.json 弄成非法 JSON。
func TestSparkStatusEscapesInput(t *testing.T) {
	b := newBox(t, "app_ok.sh", nil)
	b.run("deploy")
	b.waitState("running")
	cmd := exec.Command("/bin/sh", "-c", `. ./env; PATH=$SPARK_APP_DIR:$PATH spark_status 'dl"x' abc "$(printf 'a"b\\c\td\033[1m')"`)
	cmd.Dir = b.dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("spark_status: %v %s", err, out)
	}
	st, _ := b.status()
	if st.Phase != `dl"x` || st.Progress != 0 || st.Message != `a"b\cd[1m` || st.State != "installing" {
		t.Fatalf("status = %+v", st)
	}
	cmd = exec.Command("/bin/sh", "-c", ". ./env; PATH=$SPARK_APP_DIR:$PATH spark_status download 250 big")
	cmd.Dir = b.dir
	cmd.Run()
	if st, _ := b.status(); st.Progress != 100 {
		t.Fatalf("progress not clamped: %+v", st)
	}
}

func waitDead(t *testing.T, what string, pid int) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); alive(pid); {
		if time.Now().After(deadline) {
			t.Fatalf("%s %d still alive", what, pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func groupGone(pgid int) bool { return pgid > 1 && syscall.Kill(-pgid, 0) != nil }

// deploy 刚返回就 stop：后台 _bg 还没来得及写 .pgid，也必须被杀掉，不能之后自己跑完变成 running。
func TestRunShStopRightAfterDeploy(t *testing.T) {
	b := newBox(t, "app_ok.sh", map[string]string{"INSTALL_SLEEP": "1"})
	if _, code := b.run("deploy"); code != 0 {
		t.Fatalf("deploy = %d", code)
	}
	pg, _ := os.ReadFile(filepath.Join(b.dir, ".pgid"))
	pgid, _ := strconv.Atoi(strings.TrimSpace(string(pg)))
	if _, code := b.run("stop"); code != 0 {
		t.Fatalf("stop = %d", code)
	}
	if pgid <= 1 || !groupGone(pgid) {
		t.Fatalf("deploy group %q still alive after stop", pg)
	}
	time.Sleep(2 * time.Second) // INSTALL_SLEEP=1：若 _bg 幸存，此时已装完并起了服务
	st, out := b.status()
	if st.State != "stopped" || !strings.Contains(out, "\nalive=0\n") {
		t.Fatalf("after deploy+stop: %+v\n%s", st, b.log())
	}
	if _, err := os.Stat(filepath.Join(b.dir, "installed.marker")); err == nil {
		t.Fatal("install finished after stop")
	}
}

func TestRunShStopDuringInstall(t *testing.T) {
	b := newBox(t, "app_ok.sh", map[string]string{"INSTALL_SLEEP": "30"})
	b.run("deploy")
	var st status
	for deadline := time.Now().Add(10 * time.Second); st.Phase != "download"; {
		if time.Now().After(deadline) {
			t.Fatalf("never reached download: %+v", st)
		}
		time.Sleep(50 * time.Millisecond)
		st, _ = b.status()
	}
	start := time.Now()
	if _, code := b.run("stop"); code != 0 || time.Since(start) > 5*time.Second {
		t.Fatalf("stop = %d after %v", code, time.Since(start))
	}
	if !groupGone(st.Pid) {
		t.Fatalf("install group %d still alive", st.Pid)
	}
	if st2, _ := b.status(); st2.State != "stopped" {
		t.Fatalf("after stop: %+v", st2)
	}
}

// start 之后的失败路径（超时、服务退出、start 失败）都要清掉整个进程组，包括服务派生的孙进程。
func TestRunShFailureReapsProcessGroup(t *testing.T) {
	for _, tc := range []struct{ mode, phase, msg string }{
		{"hang", "health", "timeout"},
		{"exit", "health", "服务进程已退出"},
		{"fail", "start", "start 失败"},
		{"setsid", "health", "timeout"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			b := newBox(t, "app_grandchild.sh", map[string]string{"GC_MODE": tc.mode})
			b.setEnv("SPARK_HEALTH_ADDR", "127.0.0.1:1")
			b.setEnv("SPARK_TIMEOUT_S", "2")
			b.run("deploy")
			st := b.waitState("failed")
			if st.Phase != tc.phase || !strings.Contains(st.Message, tc.msg) {
				t.Fatalf("status = %+v", st)
			}
			data, err := os.ReadFile(filepath.Join(b.dir, "grandchild.pid"))
			gc, _ := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil || gc <= 1 {
				t.Fatalf("grandchild.pid = %q %v", data, err)
			}
			t.Cleanup(func() { syscall.Kill(gc, syscall.SIGKILL) }) // 失败时别留下 600 秒的进程
			waitDead(t, "grandchild", gc)
			for deadline := time.Now().Add(5 * time.Second); !groupGone(st.Pid); {
				if time.Now().After(deadline) {
					t.Fatalf("group %d still alive", st.Pid)
				}
				time.Sleep(50 * time.Millisecond)
			}
			for _, f := range []string{".pgid", "service.pid"} {
				if _, err := os.Stat(filepath.Join(b.dir, f)); err == nil {
					t.Fatalf("%s left after failure", f)
				}
			}
			if _, out := b.status(); !strings.Contains(out, "\nalive=0\n") {
				t.Fatalf("status = %q", out)
			}
		})
	}
}

// 重新部署时 prepare 失败（磁盘不够）：旧服务先停掉，状态 failed 与实际一致。
func TestRunShRedeployPrepareFailureStopsOld(t *testing.T) {
	b := newBox(t, "app_ok.sh", nil)
	b.run("deploy")
	running := b.waitState("running")
	b.setEnv("SPARK_MIN_FREE_GIB", "100")
	b.setEnv("SPARK_TEST_DF_FREE_GIB", "5")
	if _, code := b.run("deploy"); code != 2 {
		t.Fatalf("redeploy = %d", code)
	}
	st, out := b.status()
	if st.State != "failed" || alive(running.ServicePid) || !strings.Contains(out, "\nalive=0\n") {
		t.Fatalf("after failed redeploy: %+v old alive=%v", st, alive(running.ServicePid))
	}
}

func TestRunShDeploySyntaxError(t *testing.T) {
	b := newBox(t, "app_ok.sh", nil)
	os.WriteFile(filepath.Join(b.dir, "app.sh"), []byte("case \"$1\" in\n  install) if then ;;\nesac\n"), 0o644)
	out, code := b.run("deploy")
	st, _ := b.status()
	if code != 2 || st.State != "failed" || st.Phase != "prepare" || !strings.Contains(st.Message, "语法错误") || !strings.Contains(out, "syntax") {
		t.Fatalf("syntax: code=%d out=%q status=%+v", code, out, st)
	}
}

func TestRunShRemoveRefusesOutsideAppsDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "elsewhere")
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "run.sh"), runtime.RunSH, 0o755)
	cmd := exec.Command("/bin/sh", "run.sh", "remove")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "refuse") {
		t.Fatalf("remove outside .spark-apps: %v %q", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "run.sh")); err != nil {
		t.Fatal("dir outside .spark-apps was removed")
	}
}

// app.sh stop 卡住也不能拖垮 run.sh stop（Runner 的 exec 上限 20 秒）；stop 分支写成制表符缩进 + restart|stop) 也要认得。
func TestRunShStopTimeLimitsAppStop(t *testing.T) {
	b := newBox(t, "app_slowstop.sh", nil)
	b.run("deploy")
	running := b.waitState("running")
	start := time.Now()
	if _, code := b.run("stop"); code != 0 {
		t.Fatalf("stop = %d", code)
	}
	if d := time.Since(start); d > 12*time.Second {
		t.Fatalf("stop took %v", d)
	}
	if _, err := os.Stat(filepath.Join(b.dir, "stop.marker")); err != nil {
		t.Fatal("app.sh stop branch (tab + alternation) not executed")
	}
	if alive(running.ServicePid) {
		t.Fatal("service still alive")
	}
}

func TestSparkStatusTruncatesAndLogsLiterally(t *testing.T) {
	b := newBox(t, "app_ok.sh", nil)
	b.run("deploy")
	b.waitState("running")
	long := strings.Repeat("x", 5000)
	cmd := exec.Command("/bin/sh", "-c", `. ./env; PATH=$SPARK_APP_DIR:$PATH spark_status download 1 'back\cslash`+long+`'`)
	cmd.Dir = b.dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("spark_status: %v %s", err, out)
	}
	st, _ := b.status()
	if len(st.Message) > 1024 || !strings.HasPrefix(st.Message, `back\cslashxxx`) {
		t.Fatalf("message len %d prefix %q", len(st.Message), st.Message[:min(20, len(st.Message))])
	}
	if !strings.Contains(b.log(), `[spark_status] download 1% back\cslashxxx`) { // echo 会把 \c 当转义吞掉后文
		t.Fatalf("log line not literal:\n%s", b.log()[max(0, len(b.log())-300):])
	}
}

// 没有 python3 时 TCP 探测走 curl telnet://：连上即健康，连接超时（rc 28 且 time_connect=0）不算健康。
func TestRunShCurlTCPProbe(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { time.Sleep(5 * time.Second); c.Close() }()
		}
	}()
	t.Run("connected", func(t *testing.T) {
		b := newBox(t, "app_ok.sh", map[string]string{"SPARK_TEST_TCP_PROBE": "curl"})
		b.setEnv("SPARK_HEALTH_PATH", "")
		b.setEnv("SPARK_HEALTH_ADDR", ln.Addr().String())
		b.run("deploy")
		b.waitState("running")
	})
	t.Run("connect-timeout", func(t *testing.T) {
		b := newBox(t, "app_ok.sh", map[string]string{"SPARK_TEST_TCP_PROBE": "curl"})
		b.setEnv("SPARK_HEALTH_PATH", "")
		b.setEnv("SPARK_HEALTH_ADDR", "10.255.255.1:80") // 不可路由：连接超时
		b.setEnv("SPARK_TIMEOUT_S", "2")
		b.run("deploy")
		if st := b.waitState("failed"); !strings.Contains(st.Message, "timeout") {
			t.Fatalf("status = %+v", st)
		}
	})
}

// 重新部署先停旧服务，但 deploy 也在 Runner 的 20 秒 exec 上限里：app.sh stop 卡住、服务不理 TERM 时 deploy 也要在 12 秒内返回。
func TestRunShRedeployTimeBudget(t *testing.T) {
	b := newBox(t, "app_slowstop.sh", map[string]string{"IGNORE_TERM": "1"})
	b.run("deploy")
	old := b.waitState("running")
	start := time.Now()
	if _, code := b.run("deploy"); code != 0 {
		t.Fatalf("redeploy = %d", code)
	}
	d := time.Since(start)
	t.Logf("redeploy took %v", d)
	if d > 12*time.Second {
		t.Fatalf("redeploy took %v", d)
	}
	waitDead(t, "old service", old.ServicePid)
	b.waitState("running")
}

// .pgid 可能陈旧 / 被复用成调用方自己的进程组：kill_group 不能把 run.sh 自己（以及 SSH 会话）一起杀掉。
func TestRunShKillGroupRefusesOwnGroup(t *testing.T) {
	b := newBox(t, "app_ok.sh", nil)
	cmd := exec.Command("/bin/sh", "-c", `echo $$ > .pgid; /bin/sh run.sh stop; echo "survived rc=$?"`)
	cmd.Dir = b.dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // 包装 shell 自己当组长，组号 = $$
	out, _ := cmd.CombinedOutput()
	if !strings.Contains(string(out), "survived rc=0") {
		t.Fatalf("stop killed its own process group: %q", out)
	}
	if st, _ := b.status(); st.State != "stopped" {
		t.Fatalf("status = %+v", st)
	}
}
