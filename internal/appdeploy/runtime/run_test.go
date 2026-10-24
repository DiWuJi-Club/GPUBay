package runtime_test

import (
	"bytes"
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
	in     appdeploy.BundleInput // newBox 渲染 env 用的输入（不含 extra）
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
	b := &box{t: t, root: root, dir: dir, health: hs, in: in}
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
	deadline := time.Now().Add(60 * time.Second) // 正常几百毫秒；繁忙的开发机上新写的脚本首次 exec 可能要数秒
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
	for _, f := range []string{"testdata/app_ok.sh", "testdata/app_fail.sh", "testdata/app_env.sh", "testdata/app_disk.sh"} {
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

// image.env（镜像 ENV）先于 env 加载：镜像变量进 install / start 的环境；同名变量以 env 为准（平台保留变量、用户变量）；
// PATH 取镜像的，仍以应用目录打头（spark_status 可用）；值原样还原、不被求值。没有 image.env 时照旧（其余用例）。
func TestRunShImageEnvLoadedBeforeEnv(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "pwned")
	b := newBox(t, "app_env.sh", nil)
	imgPath := "/opt/img/bin:" + os.Getenv("PATH")
	kvs := []appdeploy.EnvKV{{Name: "CUDA_HOME", Value: "/usr/local/cuda"}, {Name: "PATH", Value: imgPath}, {Name: "MODEL_ID", Value: "from-image"},
		{Name: "SPARK_PORT", Value: "1"}, {Name: "HF_ENDPOINT", Value: "https://image.example"}, {Name: "TRITON_X", Value: "$(touch " + marker + ")'`x`"}}
	if err := os.WriteFile(filepath.Join(b.dir, "image.env"), appdeploy.RenderImageEnv(kvs), 0o600); err != nil {
		t.Fatal(err)
	}
	b.run("deploy")
	b.waitState("running")
	for _, f := range []string{"install.env", "start.env"} {
		data, err := os.ReadFile(filepath.Join(b.dir, f))
		if err != nil {
			t.Fatalf("%s: %v\n%s", f, err, b.log())
		}
		got := map[string]string{}
		for _, l := range strings.Split(string(data), "\n") {
			if k, v, ok := strings.Cut(l, "="); ok {
				got[k] = v
			}
		}
		want := map[string]string{"CUDA_HOME": "/usr/local/cuda", "MODEL_ID": "Qwen/Qwen3-4B", "SPARK_PORT": "18000",
			"HF_ENDPOINT": "https://hf-mirror.com", "TRITON_X": "$(touch " + marker + ")'`x`", "PATH": b.dir + ":" + imgPath}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s: %s = %q, want %q", f, k, got[k], v)
			}
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("image.env value was executed")
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
	for deadline := time.Now().Add(60 * time.Second); st.Phase != "download"; {
		if time.Now().After(deadline) {
			t.Fatalf("never reached download: %+v", st)
		}
		time.Sleep(50 * time.Millisecond)
		st, _ = b.status()
	}
	start := time.Now()
	if _, code := b.run("stop"); code != 0 || time.Since(start) > 15*time.Second { // 不等 install（INSTALL_SLEEP=30）跑完
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

// TestRunShDeployLock：上一次 deploy 的前台部分还在跑（Runner 的 exec 超时后命令仍在容器里）时，新的 deploy 不并发执行：
// 退出 75 + stderr「busy:」；持有者已死或锁超过 120 秒视为陈旧，照常部署。每次 deploy 换新日志，旧的留在 log.1。
func TestRunShDeployLock(t *testing.T) {
	b := newBox(t, "app_ok.sh", nil)
	holder := exec.Command("sleep", "60")
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { holder.Process.Kill(); holder.Wait() })
	lock := filepath.Join(b.dir, ".deploy.lock")
	setLock := func(pid int, at time.Time) {
		os.Remove(lock)
		if err := os.Symlink(fmt.Sprintf("%d %d", pid, at.Unix()), lock); err != nil {
			t.Fatal(err)
		}
	}
	setLock(holder.Process.Pid, time.Now())
	out, code := b.run("deploy")
	if code != 75 || !strings.Contains(out, "busy:") {
		t.Fatalf("deploy while locked = %d %q", code, out)
	}
	if _, err := os.Stat(filepath.Join(b.dir, "status.json")); err == nil {
		t.Fatal("locked deploy wrote status")
	}
	if _, err := os.Lstat(lock); err != nil {
		t.Fatalf("busy deploy removed someone else's lock: %v", err)
	}
	// 陈旧：持有者还活着但锁已超过 120 秒
	setLock(holder.Process.Pid, time.Now().Add(-200*time.Second))
	if out, code := b.run("deploy"); code != 0 {
		t.Fatalf("deploy over stale lock = %d %q", code, out)
	}
	b.waitState("running")
	if _, err := os.Lstat(lock); err == nil {
		t.Fatal("lock not released after deploy")
	}
	// 陈旧：持有者已死
	holder.Process.Kill()
	holder.Wait()
	setLock(holder.Process.Pid, time.Now())
	if out, code := b.run("deploy"); code != 0 {
		t.Fatalf("deploy over dead holder = %d %q", code, out)
	}
	b.waitState("running")
	old, _ := os.ReadFile(filepath.Join(b.dir, "log.1"))
	if !strings.Contains(string(old), "==== deploy #1") || strings.Contains(b.log(), "==== deploy #1") || !strings.Contains(b.log(), "==== deploy #2") {
		t.Fatalf("log not rotated per deploy:\nlog.1=%s\nlog=%s", old, b.log())
	}
	// 建不了锁（同名的不是符号链接）是真错误：退出 1，不是 75（Runner 据 75 只会无限重试）
	if err := os.WriteFile(lock, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, code := b.run("deploy"); code != 1 || !strings.Contains(out, "deploy lock: cannot create") {
		t.Fatalf("deploy with unusable lock path = %d %q", code, out)
	}
	os.Remove(lock)
	// 打破陈旧锁后不留临时名
	setLock(999999, time.Now().Add(-time.Hour))
	if out, code := b.run("deploy"); code != 0 {
		t.Fatalf("deploy over old lock = %d %q", code, out)
	}
	if left, _ := filepath.Glob(lock + ".stale.*"); len(left) != 0 {
		t.Fatalf("stale lock temp names left: %v", left)
	}
}

// 计划 11：install_key 第三段是 workflows/manifest.json 的 cksum —— 集合不变跳过 install，manifest 出现 / 变化都重跑。
func TestRunShInstallKeyFollowsWorkflowManifest(t *testing.T) {
	b := newBox(t, "app_ok.sh", nil)
	marker := filepath.Join(b.dir, "installed.marker")
	deploy := func() {
		t.Helper()
		os.Remove(marker)
		b.run("deploy")
		b.waitState("installing")
		b.waitState("running")
	}
	installed := func() bool { _, err := os.Stat(marker); return err == nil }
	deploy()
	if !installed() {
		t.Fatal("first deploy did not install")
	}
	got, _ := os.ReadFile(filepath.Join(b.dir, ".installed"))
	if parts := strings.Split(strings.TrimRight(string(got), "\n"), " "); len(parts) != 3 || parts[0] != "hash-1" || parts[2] != "" {
		t.Fatalf(".installed without manifest = %q", got)
	}
	deploy()
	if installed() {
		t.Fatal("install re-run with nothing changed")
	}
	os.MkdirAll(filepath.Join(b.dir, "workflows"), 0o700)
	writeManifest := func(body string) {
		os.WriteFile(filepath.Join(b.dir, "workflows", "manifest.json"), []byte(body), 0o644)
	}
	writeManifest(`{"workflows":[{"slug":"a"}]}`)
	deploy()
	if !installed() {
		t.Fatal("install not re-run after manifest appeared")
	}
	got, _ = os.ReadFile(filepath.Join(b.dir, ".installed"))
	if parts := strings.Fields(string(got)); len(parts) != 3 {
		t.Fatalf(".installed with manifest = %q", got)
	}
	deploy()
	if installed() {
		t.Fatal("install re-run with the same manifest")
	}
	writeManifest(`{"workflows":[{"slug":"a"},{"slug":"b"}]}`)
	deploy()
	if !installed() {
		t.Fatal("install not re-run after manifest changed")
	}
	// 集合清空（UploadCommand 删掉 workflows/）→ 也是变化
	os.RemoveAll(filepath.Join(b.dir, "workflows"))
	deploy()
	if !installed() {
		t.Fatal("install not re-run after workflows removed")
	}
}

// 升级兼容：计划 7 写下的两段式 .installed（「spec_hash env_cksum」）在没有 manifest 时仍算匹配，存量部署恢复时不重跑 install。
func TestRunShLegacyInstalledKeyStillMatches(t *testing.T) {
	b := newBox(t, "app_ok.sh", nil)
	user := []appdeploy.EnvKV{{Name: "MODEL_ID", Value: b.in.Env["MODEL_ID"]}}
	in := b.in
	in.GitMirror = "https://gh-proxy.com/" // 升级后的平台总带着镜像配置
	// 升级前：计划 7 的规则渲染的 env，及据它算出的两段式 .installed
	oldEnv := appdeploy.RenderEnv(plan7Reserved(in), user)
	cmd := exec.Command("/bin/sh", "-c", `cksum | awk '{print $1 "-" $2}'`)
	cmd.Stdin = bytes.NewReader(oldEnv)
	ck, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	legacy := []byte("hash-1 " + strings.TrimSpace(string(ck)) + "\n")
	// 升级后：平台按现在的规则重新渲染 env（不支持工作流的版本）并上传，覆盖旧文件
	newEnv := appdeploy.RenderEnv(appdeploy.ReservedEnv(in), user)
	if !bytes.Equal(newEnv, oldEnv) {
		t.Fatalf("non-workflow env changed after upgrade:\n%s\nplan 7:\n%s", newEnv, oldEnv)
	}
	os.WriteFile(filepath.Join(b.dir, "env"), newEnv, 0o600)
	os.WriteFile(filepath.Join(b.dir, ".installed"), legacy, 0o644)
	b.run("deploy")
	b.waitState("running")
	if _, err := os.Stat(filepath.Join(b.dir, "installed.marker")); err == nil {
		t.Fatalf("legacy .installed not honoured; log:\n%s", b.log())
	}
	if !strings.Contains(b.log(), "skip install") {
		t.Fatalf("log:\n%s", b.log())
	}
	// 但旧键 + 新出现的 manifest → 重跑
	os.WriteFile(filepath.Join(b.dir, ".installed"), legacy, 0o644)
	os.MkdirAll(filepath.Join(b.dir, "workflows"), 0o700)
	os.WriteFile(filepath.Join(b.dir, "workflows", "manifest.json"), []byte(`{}`), 0o644)
	b.run("deploy")
	b.waitState("installing")
	b.waitState("running")
	if _, err := os.Stat(filepath.Join(b.dir, "installed.marker")); err != nil {
		t.Fatal("legacy key + manifest must re-run install")
	}
}

// plan7Reserved 是计划 7 的保留变量表（逐项写死，不经 ReservedEnv）：升级兼容测试的「旧规则」。
func plan7Reserved(in appdeploy.BundleInput) []appdeploy.EnvKV {
	models := in.AppRoot + "/models"
	health := in.HealthAddr
	if health == "" {
		health = "127.0.0.1:" + strconv.Itoa(in.Spec.Ports[0].Port)
	}
	return []appdeploy.EnvKV{
		{Name: "SPARK_APP", Value: in.Slug}, {Name: "SPARK_APP_DIR", Value: appdeploy.AppDir(in.AppRoot, in.Slug)}, {Name: "SPARK_DATA_DIR", Value: in.AppRoot},
		{Name: "SPARK_PORT", Value: strconv.Itoa(in.Spec.Ports[0].Port)}, {Name: "SPARK_PORT_2", Value: ""}, {Name: "SPARK_INSTANCE_ID", Value: in.InstanceID},
		{Name: "SPARK_MODELS_DIR", Value: models}, {Name: "SPARK_SPEC_HASH", Value: in.SpecHash}, {Name: "SPARK_TIMEOUT_S", Value: strconv.Itoa(in.Spec.TimeoutMinutes * 60)},
		{Name: "SPARK_HEALTH_PATH", Value: in.Spec.Ports[0].HealthPath}, {Name: "SPARK_MIN_FREE_GIB", Value: strconv.Itoa(in.Spec.MinDataFreeGiB)}, {Name: "SPARK_HEALTH_ADDR", Value: health},
		{Name: "HF_ENDPOINT", Value: in.HFEndpoint}, {Name: "MODELSCOPE_CACHE", Value: models + "/modelscope"}, {Name: "HF_HOME", Value: models + "/hf"},
		{Name: "PIP_INDEX_URL", Value: in.PipIndexURL},
	}
}

// 计划 11 D7：install 失败前最后一次汇报的消息以 disk: 开头 → failed 状态沿用它（平台映射为错误码 disk）。
func TestRunShInstallDiskMessageKept(t *testing.T) {
	b := newBox(t, "app_disk.sh", nil)
	b.run("deploy")
	st := b.waitState("failed")
	if !strings.HasPrefix(st.Message, "disk:") || st.Message != "disk: 剩余 1GiB，需要 12GiB" || st.Phase != "download" || st.ExitCode != 2 {
		t.Fatalf("failed status = %+v", st)
	}
}

// disk: 消息里的双引号、反斜杠、中文原样保留（status.json 里是转义过的，取出时要还原，否则再次写入会被双重转义或截断）。
func TestRunShInstallDiskMessageWithQuotes(t *testing.T) {
	for _, msg := range []string{
		`disk: 剩余 "1GiB"，需要 12GiB`,
		`disk: path C:\data\models 不够`,
		`disk: a"b \ "c" 结尾\`,
		`disk: x","pid":1,"y":"z`, // 伪造 status.json 字段
	} {
		t.Run(msg, func(t *testing.T) {
			b := newBox(t, "app_disk.sh", map[string]string{"DISK_MSG": msg})
			b.run("deploy")
			st := b.waitState("failed")
			if st.Message != msg || st.Phase != "download" || st.ExitCode != 2 {
				t.Fatalf("failed status = %+v, want message %q", st, msg)
			}
		})
	}
}
