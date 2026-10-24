package comfywf

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"spark-cloud/internal/access/upstream"
	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/comfywf/comfytest"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/dbtest"
)

// runEnv：目录环境 + 一个 running 的 comfyui 部署（已装 sdxl 版本）+ fake ComfyUI（拨号函数直连 httptest，不起 sshd）。
type runEnv struct {
	*catEnv
	fake *comfytest.Server
	runs *Runs
	dep  string
	ins  string
	uid  int64
	wf   Workflow
	ver  Version

	mu       sync.Mutex
	dials    []dialRec
	dialErr  error
	override func(w http.ResponseWriter, r *http.Request) bool // 返回 true = 已处理（敌意响应用例）
}

type dialRec struct {
	target upstream.Target
	port   int
}

func newRunEnv(t *testing.T) *runEnv {
	t.Helper()
	e := &runEnv{catEnv: newCatEnv(t), fake: comfytest.New()}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		o := e.override
		e.mu.Unlock()
		if o != nil && o(w, r) {
			return
		}
		e.fake.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	client := NewClientWithDialer(func(ctx context.Context, tg upstream.Target, port int) (net.Conn, error) {
		e.mu.Lock()
		e.dials = append(e.dials, dialRec{tg, port})
		derr := e.dialErr
		e.mu.Unlock()
		if derr != nil {
			return nil, derr
		}
		var d net.Dialer
		return d.DialContext(ctx, "tcp", ts.Listener.Addr().String())
	})
	apps := appdeploy.NewService(appdeploy.Deps{Pool: e.pool, Clock: e.clk})
	e.runs = NewRuns(e.pool, e.clk, e.cat, apps, client, slog.New(slog.NewTextHandler(io.Discard, nil)))
	e.runs.rnd = func() int64 { return 42 }
	e.dep = e.deployFixture()(appdeploy.StateRunning)
	if err := e.pool.QueryRow(e.ctx, `SELECT instance_id, user_id FROM app_deployments WHERE id = $1`, e.dep).Scan(&e.ins, &e.uid); err != nil {
		t.Fatal(err)
	}
	e.wf = e.workflow("sdxl", "")
	e.ver = e.published(e.wf.ID, "1.0.0")
	e.attach(e.dep, e.ver, WFStateInstalled)
	return e
}

func (e *runEnv) setOverride(o func(w http.ResponseWriter, r *http.Request) bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.override = o
}

func (e *runEnv) setDialErr(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.dialErr = err
}

func (e *runEnv) start(inputs map[string]any) (RunView, error) {
	if inputs == nil {
		inputs = map[string]any{"image": "spark-quickrun/abcdefabcdef.png"}
	}
	return e.runs.Start(e.ctx, StartRequest{UserID: e.uid, InstanceID: e.ins, DeploymentID: e.dep, WorkflowID: e.wf.ID, Inputs: inputs})
}

func (e *runEnv) mustStart() RunView {
	e.t.Helper()
	v, err := e.start(nil)
	if err != nil {
		e.t.Fatalf("Start: %v", err)
	}
	return v
}

func (e *runEnv) getReq(runID string) GetRequest {
	return GetRequest{UserID: e.uid, InstanceID: e.ins, DeploymentID: e.dep, WorkflowID: e.wf.ID, RunID: runID}
}

func (e *runEnv) get(runID string) RunView {
	e.t.Helper()
	v, err := e.runs.Get(e.ctx, e.getReq(runID))
	if err != nil {
		e.t.Fatalf("Get: %v", err)
	}
	return v
}

func (e *runEnv) upload(body []byte, size int64) (UploadResult, error) {
	return e.runs.Upload(e.ctx, UploadRequest{UserID: e.uid, InstanceID: e.ins, DeploymentID: e.dep, WorkflowID: e.wf.ID,
		Body: bytes.NewReader(body), Size: size})
}

func (e *runEnv) runState(id string) (string, *time.Time) {
	e.t.Helper()
	var st string
	var fin *time.Time
	if err := e.pool.QueryRow(e.ctx, `SELECT state, finished_at FROM comfy_workflow_runs WHERE id = $1`, id).Scan(&st, &fin); err != nil {
		e.t.Fatal(err)
	}
	return st, fin
}

func wantErr(t *testing.T, err error, code string, status int) {
	t.Helper()
	ae := apperr.As(err)
	if ae == nil || ae.Code != code || ae.Status != status {
		t.Fatalf("err = %v, want %s/%d", err, code, status)
	}
}

var (
	uploadNameRe = regexp.MustCompile(`^spark-quickrun/[a-z0-9]{12}\.(png|jpg|webp)$`)
	runIDRe      = regexp.MustCompile(`^run-[a-z0-9]{12}$`)
)

func TestRunsUpload(t *testing.T) {
	e := newRunEnv(t)
	res, err := e.upload(comfytest.OnePixelPNG, int64(len(comfytest.OnePixelPNG)))
	if err != nil {
		t.Fatal(err)
	}
	if !uploadNameRe.MatchString(res.Name) || !strings.HasSuffix(res.Name, ".png") {
		t.Fatalf("name = %q", res.Name)
	}
	ups := e.fake.Uploads()
	if len(ups) != 1 || ups[0].Subfolder != QuickRunSubfolder || ups[0].Overwrite != "true" || ups[0].Type != "input" ||
		ups[0].Size != len(comfytest.OnePixelPNG) || "spark-quickrun/"+ups[0].Filename != res.Name {
		t.Fatalf("uploads = %+v", ups)
	}
	if d := e.dials[0]; d.port != 8188 || d.target.InstanceID != e.ins || d.target.LanIP != "10.0.0.10" {
		t.Fatalf("dial = %+v", d)
	}

	jpeg := append([]byte{0xff, 0xd8, 0xff, 0xe0}, make([]byte, 64)...)
	if res, err := e.upload(jpeg, 0); err != nil || !strings.HasSuffix(res.Name, ".jpg") {
		t.Fatalf("jpeg: %v %v", res, err)
	}
	webp := append([]byte("RIFF\x00\x00\x00\x00WEBPVP8 "), make([]byte, 32)...)
	if res, err := e.upload(webp, 0); err != nil || !strings.HasSuffix(res.Name, ".webp") {
		t.Fatalf("webp: %v %v", res, err)
	}

	// 类型按内容判断
	for name, body := range map[string][]byte{"gif": []byte("GIF89a\x01\x00\x01\x00"), "text": []byte("hello"), "empty": nil,
		"svg": []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`)} {
		_, err := e.upload(body, 0)
		if ae := apperr.As(err); ae == nil || ae.Code != "unsupported_file_type" || ae.Status != 415 {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
	// 大小：声明的大小与实际读到的
	_, err = e.upload(comfytest.OnePixelPNG, MaxUploadBytes+1)
	wantErr(t, err, "file_too_large", 413)
	if !strings.Contains(apperr.As(err).Message, "16MB") {
		t.Fatalf("message = %q", apperr.As(err).Message)
	}
	big := append(append([]byte{}, comfytest.OnePixelPNG...), make([]byte, MaxUploadBytes)...)
	_, err = e.upload(big, 0)
	wantErr(t, err, "file_too_large", 413)
	if n := len(e.fake.Uploads()); n != 3 {
		t.Fatalf("uploads = %d, want 3 (rejected ones not proxied)", n)
	}

	// ComfyUI 返回的名字不合白名单
	for _, bad := range []string{"../x.png", "evil.png\"", "abcdefabcdef.gif", "ABCDEFABCDEF.png"} {
		e.fake.SetUploadName(bad)
		_, err = e.upload(comfytest.OnePixelPNG, 0)
		wantErr(t, err, "upstream_unreachable", 502)
	}
	e.fake.SetUploadName("")

	// 归属、安装状态、运行状态、连不上
	_, err = e.runs.Upload(e.ctx, UploadRequest{UserID: e.uid + 999, InstanceID: e.ins, DeploymentID: e.dep, WorkflowID: e.wf.ID,
		Body: bytes.NewReader(comfytest.OnePixelPNG)})
	wantErr(t, err, "not_found", 404)
	_, err = e.runs.Upload(e.ctx, UploadRequest{UserID: e.uid, InstanceID: "ins-other", DeploymentID: e.dep, WorkflowID: e.wf.ID,
		Body: bytes.NewReader(comfytest.OnePixelPNG)})
	wantErr(t, err, "not_found", 404)
	_, err = e.runs.Upload(e.ctx, UploadRequest{UserID: e.uid, InstanceID: e.ins, DeploymentID: e.dep, WorkflowID: e.wf.ID + 999,
		Body: bytes.NewReader(comfytest.OnePixelPNG)})
	wantErr(t, err, "workflow_not_found", 404)
	e.exec(`UPDATE deployment_workflows SET state = 'pending' WHERE deployment_id = $1`, e.dep)
	_, err = e.upload(comfytest.OnePixelPNG, 0)
	wantErr(t, err, "app_invalid_state", 409)
	if apperr.As(err).Message != "工作流尚未安装完成" {
		t.Fatalf("message = %q", apperr.As(err).Message)
	}
	e.exec(`UPDATE deployment_workflows SET state = 'installed' WHERE deployment_id = $1`, e.dep)
	e.exec(`UPDATE instances SET suspended_at = $2 WHERE id = $1`, e.ins, dbtest.T0)
	_, err = e.upload(comfytest.OnePixelPNG, 0)
	wantErr(t, err, "app_invalid_state", 409)
	e.exec(`UPDATE instances SET suspended_at = NULL WHERE id = $1`, e.ins)
	e.exec(`UPDATE app_deployments SET state = 'installing' WHERE id = $1`, e.dep)
	_, err = e.upload(comfytest.OnePixelPNG, 0)
	wantErr(t, err, "app_invalid_state", 409)
	e.exec(`UPDATE app_deployments SET state = 'running' WHERE id = $1`, e.dep)
	e.setDialErr(&upstream.DialError{InstanceID: e.ins, Err: errors.New("connection refused")})
	_, err = e.upload(comfytest.OnePixelPNG, 0)
	wantErr(t, err, "upstream_unreachable", 503)
}

func TestRunsUploadNeedsImageInput(t *testing.T) {
	e := newRunEnv(t)
	m := okManifest()
	m.Inputs = m.Inputs[:len(m.Inputs)-1] // 去掉 image
	v, err := e.cat.CreateVersion(e.ctx, e.actor(), e.wf.ID, VersionInput{Version: "1.1.0", Graph: fixture(t, "graph_sdxl_ok.json"),
		APIPrompt: fixture(t, "api_sdxl.json"), Manifest: m})
	if err != nil {
		t.Fatal(err)
	}
	e.exec(`UPDATE deployment_workflows SET workflow_version_id = $2 WHERE deployment_id = $1`, e.dep, v.ID)
	_, err = e.upload(comfytest.OnePixelPNG, 0)
	wantErr(t, err, "bad_request", 400)
}

func TestRunsStart(t *testing.T) {
	e := newRunEnv(t)
	v, err := e.start(map[string]any{"prompt": "a red fox", "steps": 30, "image": "spark-quickrun/abcdefabcdef.png"})
	if err != nil {
		t.Fatal(err)
	}
	ps := e.fake.Prompts()
	if len(ps) != 1 || !runIDRe.MatchString(v.ID) || v.State != RunQueued || v.PromptID != ps[0].ID || ps[0].ClientID != v.ID ||
		v.Seed == nil || *v.Seed != 42 || !v.CreatedAt.Equal(e.clk.Now()) || v.FinishedAt != nil || v.Files == nil {
		t.Fatalf("view = %+v, prompts = %+v", v, ps)
	}
	var prompt map[string]struct {
		Inputs map[string]any `json:"inputs"`
	}
	if err := json.Unmarshal(ps[0].Prompt, &prompt); err != nil {
		t.Fatal(err)
	}
	if prompt["6"].Inputs["text"] != "a red fox" || prompt["3"].Inputs["steps"] != 30.0 || prompt["3"].Inputs["seed"] != 42.0 ||
		prompt["10"].Inputs["image"] != "spark-quickrun/abcdefabcdef.png" || prompt["5"].Inputs["width"] != 1024.0 {
		t.Fatalf("prompt = %s", ps[0].Prompt)
	}
	var row Run
	if row, err = scanRun(e.pool.QueryRow(e.ctx, `SELECT `+runCols+` FROM comfy_workflow_runs WHERE id = $1`, v.ID)); err != nil {
		t.Fatal(err)
	}
	if row.DeploymentID != e.dep || row.UserID != e.uid || row.VersionID != e.ver.ID || row.PromptID != v.PromptID || *row.Seed != 42 ||
		row.State != RunQueued {
		t.Fatalf("row = %+v", row)
	}

	// 指定种子；没有随机源注入时 -1 → 随机
	e.fake.SetHistory(v.PromptID, comfytest.DoneN(1))
	if v2, err := e.start(map[string]any{"seed": 7, "image": "spark-quickrun/abcdefabcdef.png"}); err != nil || *v2.Seed != 7 {
		t.Fatalf("seed 7: %+v %v", v2, err)
	} else {
		e.fake.SetHistory(v2.PromptID, comfytest.DoneN(1))
	}
	e.runs.rnd = nil
	seeds := map[int64]bool{}
	for range 2 {
		v3, err := e.start(map[string]any{"seed": -1, "image": "spark-quickrun/abcdefabcdef.png"})
		if err != nil {
			t.Fatal(err)
		}
		if *v3.Seed < 0 || *v3.Seed >= 1<<53 {
			t.Fatalf("seed = %d", *v3.Seed)
		}
		seeds[*v3.Seed] = true
		e.fake.SetHistory(v3.PromptID, comfytest.DoneN(1))
	}
	if len(seeds) != 2 {
		t.Fatalf("random seeds not random: %v", seeds)
	}

	// 输入不合法：不提交
	n := len(e.fake.Prompts())
	_, err = e.start(map[string]any{"steps": 1000, "image": "spark-quickrun/abcdefabcdef.png"})
	wantErr(t, err, "workflow_input_invalid", 400)
	_, err = e.start(map[string]any{"image": "../../etc/passwd"})
	wantErr(t, err, "workflow_input_invalid", 400)
	if len(e.fake.Prompts()) != n {
		t.Fatal("invalid input was submitted")
	}
}

func TestRunsStartBusy(t *testing.T) {
	e := newRunEnv(t)
	a, b := e.mustStart(), e.mustStart()
	_, err := e.start(nil)
	wantErr(t, err, "workflow_busy", 429)

	// 达到上限时复查：a 已完成（没人轮询）→ 落成 done，放行
	e.fake.SetHistory(a.PromptID, comfytest.DoneN(1))
	c := e.mustStart()
	if st, fin := e.runState(a.ID); st != RunDone || fin == nil {
		t.Fatalf("a = %s %v", st, fin)
	}
	_, err = e.start(nil)
	wantErr(t, err, "workflow_busy", 429)

	// 超过 RunWaitMax 的 queued 不再计数，并在复查时落成 error（超时）
	e.clk.Advance(RunWaitMax + time.Second)
	e.mustStart()
	if st, _ := e.runState(b.ID); st != RunError {
		t.Fatalf("b = %s", st)
	}
	_ = c

	// 其它部署不受影响：计数按部署
	var other int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM comfy_workflow_runs WHERE deployment_id <> $1`, e.dep).Scan(&other); err != nil || other != 0 {
		t.Fatal(other, err)
	}
}

func TestRunsStartRateLimit(t *testing.T) {
	e := newRunEnv(t)
	for i := range RunsPerHour {
		e.exec(`INSERT INTO comfy_workflow_runs (id, deployment_id, user_id, workflow_version_id, prompt_id, state, created_at)
			VALUES ($1, $2, $3, $4, 'p', 'done', $5)`, fmt.Sprintf("run-%012d", i), e.dep, e.uid, e.ver.ID, e.clk.Now().Add(-30*time.Minute))
	}
	_, err := e.start(nil)
	wantErr(t, err, "rate_limited", 429)
	e.clk.Advance(31 * time.Minute)
	e.mustStart()
}

func TestRunsStartRejected(t *testing.T) {
	e := newRunEnv(t)
	e.fake.RejectPrompts("Value not in list " + strings.Repeat("长", 300))
	_, err := e.start(nil)
	wantErr(t, err, "workflow_prompt_rejected", 422)
	msg := strings.TrimPrefix(apperr.As(err).Message, "ComfyUI 拒绝了该任务：")
	if !strings.HasPrefix(msg, "Value not in list 长") || len([]rune(msg)) != 200 {
		t.Fatalf("message = %q (%d)", msg, len([]rune(msg)))
	}
	if e.count(`SELECT count(*) FROM comfy_workflow_runs`) != 0 {
		t.Fatal("rejected run recorded")
	}

	// 缺自定义节点：node_errors 为空，消息在 error.message / details
	e.fake.RejectPrompts("")
	e.fake.RejectMissingNode("FakeNodeType")
	_, err = e.start(nil)
	if ae := apperr.As(err); ae == nil || ae.Code != "workflow_prompt_rejected" ||
		ae.Message != "ComfyUI 拒绝了该任务：Node 'FakeNodeType' not found. The custom node may not be installed.：Node ID '#12'" {
		t.Fatalf("missing node: %v", err)
	}
	e.fake.RejectMissingNode("")

	// node_errors 为空时取 error.message；消息里的控制字符被替换
	e.setOverride(func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/prompt" {
			return false
		}
		w.WriteHeader(400)
		io.WriteString(w, `{"error":{"type":"prompt_no_outputs","message":"Prompt has no outputs\u0007","details":""},"node_errors":{}}`)
		return true
	})
	_, err = e.start(nil)
	if ae := apperr.As(err); ae == nil || ae.Message != "ComfyUI 拒绝了该任务：Prompt has no outputs" {
		t.Fatalf("err = %v", err)
	}

	// 其它状态码 / 坏的 prompt_id → 502
	for _, resp := range []struct {
		status int
		body   string
	}{{500, `oops`}, {200, `not json`}, {200, `{"prompt_id":"../../x"}`}, {200, `{"number":1}`}, {302, ``}} {
		e.setOverride(func(w http.ResponseWriter, r *http.Request) bool {
			if r.URL.Path != "/prompt" {
				return false
			}
			if resp.status == 302 {
				w.Header().Set("Location", "/history/x")
			}
			w.WriteHeader(resp.status)
			io.WriteString(w, resp.body)
			return true
		})
		_, err = e.start(nil)
		wantErr(t, err, "upstream_unreachable", 502)
	}
	// 响应超长
	e.setOverride(func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/prompt" {
			return false
		}
		io.WriteString(w, `{"prompt_id":"x","pad":"`+strings.Repeat("a", maxJSONBytes)+`"}`)
		return true
	})
	_, err = e.start(nil)
	wantErr(t, err, "upstream_unreachable", 502)
	if e.count(`SELECT count(*) FROM comfy_workflow_runs`) != 0 {
		t.Fatal("run recorded")
	}
}

func TestRunsStartGuards(t *testing.T) {
	e := newRunEnv(t)
	// 版本没有 api_prompt
	m := okManifest()
	m.Inputs, m.Outputs = nil, nil
	v, err := e.cat.CreateVersion(e.ctx, e.actor(), e.wf.ID, VersionInput{Version: "0.9.0", Graph: fixture(t, "graph_sdxl_ok.json"), Manifest: m})
	if err != nil {
		t.Fatal(err)
	}
	e.exec(`UPDATE deployment_workflows SET workflow_version_id = $2 WHERE deployment_id = $1`, e.dep, v.ID)
	_, err = e.start(map[string]any{})
	wantErr(t, err, "bad_request", 400)
	e.exec(`UPDATE deployment_workflows SET workflow_version_id = $2 WHERE deployment_id = $1`, e.dep, e.ver.ID)

	for _, st := range []string{WFStatePending, WFStateFailed} {
		e.exec(`UPDATE deployment_workflows SET state = $2 WHERE deployment_id = $1`, e.dep, st)
		_, err = e.start(nil)
		wantErr(t, err, "app_invalid_state", 409)
	}
	e.exec(`UPDATE deployment_workflows SET state = 'installed' WHERE deployment_id = $1`, e.dep)
	_, err = e.runs.Start(e.ctx, StartRequest{UserID: e.uid, InstanceID: e.ins, DeploymentID: e.dep, WorkflowID: e.wf.ID + 1})
	wantErr(t, err, "workflow_not_found", 404)
	_, err = e.runs.Start(e.ctx, StartRequest{UserID: e.uid + 1, InstanceID: e.ins, DeploymentID: e.dep, WorkflowID: e.wf.ID})
	wantErr(t, err, "not_found", 404)
	e.exec(`UPDATE instances SET state = 'stopped' WHERE id = $1`, e.ins)
	_, err = e.start(nil)
	wantErr(t, err, "app_invalid_state", 409)
	if apperr.As(err).Message != "ComfyUI 未运行" {
		t.Fatalf("message = %q", apperr.As(err).Message)
	}
	e.exec(`UPDATE instances SET state = 'running' WHERE id = $1`, e.ins)
	e.setDialErr(&upstream.DialError{InstanceID: e.ins, Err: errors.New("no route")})
	_, err = e.start(nil)
	wantErr(t, err, "upstream_unreachable", 503)
	e.setDialErr(errors.New("容器内端口 8188 未监听"))
	_, err = e.start(nil)
	wantErr(t, err, "upstream_unreachable", 503)
	if len(e.fake.Prompts()) != 0 {
		t.Fatal("submitted")
	}
}

func TestRunsGet(t *testing.T) {
	e := newRunEnv(t)
	run := e.mustStart()
	if v := e.get(run.ID); v.State != RunQueued || len(v.Files) != 0 || v.PromptID != run.PromptID || *v.Seed != 42 {
		t.Fatalf("queued: %+v", v)
	}
	e.fake.SetHistory(run.PromptID, comfytest.RunningState())
	if v := e.get(run.ID); v.State != RunRunning {
		t.Fatalf("running: %+v", v)
	}
	if st, _ := e.runState(run.ID); st != RunQueued {
		t.Fatalf("running is not persisted, got %s", st)
	}

	e.clk.Advance(time.Minute)
	e.fake.SetHistory(run.PromptID, comfytest.DoneState(comfytest.PNG("ComfyUI_00001_.png"),
		comfytest.File{Filename: "clip.mp4", Subfolder: "video", Type: "output"}, comfytest.File{Filename: "tmp.png", Type: "temp"}))
	v := e.get(run.ID)
	want := []FileView{{Index: 0, Filename: "ComfyUI_00001_.png", Kind: KindImage}, {Index: 1, Filename: "clip.mp4", Kind: KindVideo}}
	if v.State != RunDone || v.FinishedAt == nil || !v.FinishedAt.Equal(e.clk.Now()) || fmt.Sprint(v.Files) != fmt.Sprint(want) {
		t.Fatalf("done: %+v", v)
	}
	if st, fin := e.runState(run.ID); st != RunDone || !fin.Equal(e.clk.Now()) {
		t.Fatalf("row = %s %v", st, fin)
	}
	// 终态再查：finished_at 不变、文件仍从 history 取
	e.clk.Advance(time.Minute)
	if v2 := e.get(run.ID); !v2.FinishedAt.Equal(*v.FinishedAt) || len(v2.Files) != 2 {
		t.Fatalf("again: %+v", v2)
	}
	// history 丢了（ComfyUI 重启）/ 连不上 / 实例停了：只给缓存状态
	e.fake.Forget(run.PromptID)
	if v2 := e.get(run.ID); v2.State != RunDone || len(v2.Files) != 0 || v2.Files == nil {
		t.Fatalf("forgotten: %+v", v2)
	}
	e.fake.SetHistory(run.PromptID, comfytest.DoneN(1))
	e.setDialErr(&upstream.DialError{InstanceID: e.ins, Err: errors.New("down")})
	if v2 := e.get(run.ID); v2.State != RunDone || len(v2.Files) != 0 {
		t.Fatalf("unreachable: %+v", v2)
	}
	e.setDialErr(nil)
	e.exec(`UPDATE instances SET state = 'stopped' WHERE id = $1`, e.ins)
	if v2 := e.get(run.ID); v2.State != RunDone {
		t.Fatalf("stopped: %+v", v2)
	}
	e.exec(`UPDATE instances SET state = 'running' WHERE id = $1`, e.ins)

	// 出错
	bad := e.mustStart()
	e.fake.SetHistory(bad.PromptID, comfytest.ErrorState("CUDA out of memory"))
	if v := e.get(bad.ID); v.State != RunError || v.Message != "CUDA out of memory" || v.FinishedAt == nil || len(v.Files) != 0 {
		t.Fatalf("error: %+v", v)
	}
	if st, _ := e.runState(bad.ID); st != RunError {
		t.Fatalf("row = %s", st)
	}

	// 归属：别的用户、别的工作流、别的部署、不存在
	for _, req := range []GetRequest{
		{UserID: e.uid + 1, InstanceID: e.ins, DeploymentID: e.dep, WorkflowID: e.wf.ID, RunID: run.ID},
		{UserID: e.uid, InstanceID: e.ins, DeploymentID: e.dep, WorkflowID: e.wf.ID + 1, RunID: run.ID},
		{UserID: e.uid, InstanceID: e.ins, DeploymentID: "dep-nope", WorkflowID: e.wf.ID, RunID: run.ID},
		{UserID: e.uid, InstanceID: e.ins, DeploymentID: e.dep, WorkflowID: e.wf.ID, RunID: "run-nope"},
	} {
		_, err := e.runs.Get(e.ctx, req)
		wantErr(t, err, "not_found", 404)
	}

	// 未完成的运行：实例停了 → app_invalid_state；history 超长 / 不是 JSON → 502
	q := e.mustStart()
	e.exec(`UPDATE instances SET state = 'stopped' WHERE id = $1`, e.ins)
	_, err := e.runs.Get(e.ctx, e.getReq(q.ID))
	wantErr(t, err, "app_invalid_state", 409)
	e.exec(`UPDATE instances SET state = 'running' WHERE id = $1`, e.ins)
	for _, body := range []string{`{"x":"` + strings.Repeat("a", maxJSONBytes) + `"}`, `<html>`} {
		e.setOverride(func(w http.ResponseWriter, r *http.Request) bool {
			if !strings.HasPrefix(r.URL.Path, "/history/") {
				return false
			}
			io.WriteString(w, body)
			return true
		})
		_, err = e.runs.Get(e.ctx, e.getReq(q.ID))
		wantErr(t, err, "upstream_unreachable", 502)
	}
	// /queue 坏了：按排队中
	e.setOverride(func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/queue" {
			return false
		}
		io.WriteString(w, `[[[`)
		return true
	})
	if v := e.get(q.ID); v.State != RunQueued {
		t.Fatalf("bad queue: %+v", v)
	}
}

func TestRunsOpenFile(t *testing.T) {
	e := newRunEnv(t)
	run := e.mustStart()
	e.fake.SetHistory(run.PromptID, comfytest.DoneState(comfytest.PNG("图 1.png"),
		comfytest.File{Filename: "clip.mp4", Subfolder: "video", Type: "output"}))
	e.fake.SetView(comfytest.ViewConfig{ContentType: "text/html", Body: []byte("<script>alert(1)</script>")})
	f, err := e.runs.OpenFile(e.ctx, e.getReq(run.ID), 0, "")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(f.Body)
	f.Body.Close()
	if err != nil || string(body) != "<script>alert(1)</script>" || f.Length != int64(len(body)) || f.ContentType != "image/png" || f.Filename != "图 1.png" {
		t.Fatalf("file = %+v body %q err %v", f, body, err)
	}
	h := http.Header{}
	f.SetHeaders(h)
	if h.Get("Content-Type") != "image/png" || h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Content-Length") != "25" ||
		h.Get("Cache-Control") != "private, max-age=3600" || h.Get("Content-Disposition") != "inline; filename*=utf-8''%E5%9B%BE%201.png" ||
		h.Get("Content-Security-Policy") != "default-src 'none'; sandbox" || h.Get("Cross-Origin-Resource-Policy") != "same-origin" {
		t.Fatalf("headers = %v", h)
	}
	vs := e.fake.Views()
	if len(vs) != 1 || vs[0].Get("filename") != "图 1.png" || vs[0].Get("subfolder") != "" || vs[0].Get("type") != "output" {
		t.Fatalf("views = %v", vs)
	}

	e.fake.SetView(comfytest.ViewConfig{})
	f, err = e.runs.OpenFile(e.ctx, e.getReq(run.ID), 1, "")
	if err != nil {
		t.Fatal(err)
	}
	f.Body.Close()
	if f.ContentType != "video/mp4" || f.Disposition != "inline; filename=clip.mp4" || e.fake.Views()[1].Get("subfolder") != "video" {
		t.Fatalf("video = %+v", f)
	}

	for _, idx := range []int{2, -1} {
		_, err = e.runs.OpenFile(e.ctx, e.getReq(run.ID), idx, "")
		wantErr(t, err, "not_found", 404)
	}
	// 容器里文件已不在
	e.fake.SetView(comfytest.ViewConfig{Status: 404, Body: []byte("not found")})
	_, err = e.runs.OpenFile(e.ctx, e.getReq(run.ID), 0, "")
	wantErr(t, err, "not_found", 404)
	e.fake.SetView(comfytest.ViewConfig{Status: 500, Body: []byte("x")})
	_, err = e.runs.OpenFile(e.ctx, e.getReq(run.ID), 0, "")
	wantErr(t, err, "upstream_unreachable", 502)

	// 大小：Content-Length 超限 → 413；没有长度 → 读到上限后报错；恰好上限 → 正常
	e.fake.SetView(comfytest.ViewConfig{Size: MaxViewBytes + 1})
	_, err = e.runs.OpenFile(e.ctx, e.getReq(run.ID), 0, "")
	wantErr(t, err, "file_too_large", 413)
	e.fake.SetView(comfytest.ViewConfig{Size: MaxViewBytes + 1, NoLength: true})
	f, err = e.runs.OpenFile(e.ctx, e.getReq(run.ID), 0, "")
	if err != nil {
		t.Fatal(err)
	}
	n, err := io.Copy(io.Discard, f.Body)
	f.Body.Close()
	if f.Length != -1 || n != MaxViewBytes || !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("chunked over limit: len %d n %d err %v", f.Length, n, err)
	}
	e.fake.SetView(comfytest.ViewConfig{Size: MaxViewBytes, NoLength: true})
	f, err = e.runs.OpenFile(e.ctx, e.getReq(run.ID), 0, "")
	if err != nil {
		t.Fatal(err)
	}
	n, err = io.Copy(io.Discard, f.Body)
	f.Body.Close()
	if n != MaxViewBytes || err != nil {
		t.Fatalf("chunked at limit: n %d err %v", n, err)
	}

	// 未完成的运行没有文件；别人的运行 404；实例停了 → app_invalid_state
	q := e.mustStart()
	_, err = e.runs.OpenFile(e.ctx, e.getReq(q.ID), 0, "")
	wantErr(t, err, "not_found", 404)
	other := e.getReq(run.ID)
	other.UserID++
	_, err = e.runs.OpenFile(e.ctx, other, 0, "")
	wantErr(t, err, "not_found", 404)
	e.exec(`UPDATE instances SET state = 'stopped' WHERE id = $1`, e.ins)
	_, err = e.runs.OpenFile(e.ctx, e.getReq(run.ID), 0, "")
	wantErr(t, err, "app_invalid_state", 409)
	e.exec(`UPDATE instances SET state = 'running' WHERE id = $1`, e.ins)
	// 终态运行读不到 history：如实报连不上 / 坏响应（不是 404）
	e.setDialErr(&upstream.DialError{InstanceID: e.ins, Err: errors.New("down")})
	_, err = e.runs.OpenFile(e.ctx, e.getReq(run.ID), 0, "")
	wantErr(t, err, "upstream_unreachable", 503)
	e.setDialErr(nil)
	e.setOverride(func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasPrefix(r.URL.Path, "/history/") {
			return false
		}
		io.WriteString(w, `<html>`)
		return true
	})
	_, err = e.runs.OpenFile(e.ctx, e.getReq(run.ID), 0, "")
	wantErr(t, err, "upstream_unreachable", 502)
	e.setOverride(nil)
	// history 丢了（ComfyUI 重启）：文件拿不到 → 404
	e.fake.Forget(run.PromptID)
	_, err = e.runs.OpenFile(e.ctx, e.getReq(run.ID), 0, "")
	wantErr(t, err, "not_found", 404)
}

// 视频拖动进度条：单段 Range（bytes=a-b / bytes=a-）原样转给 /view，206 校验 Content-Range 后透传；其余 Range 不转发（完整 200）。
func TestRunsOpenFileRange(t *testing.T) {
	e := newRunEnv(t)
	run := e.mustStart()
	e.fake.SetHistory(run.PromptID, comfytest.DoneState(comfytest.File{Filename: "clip.mp4", Subfolder: "video", Type: "output"}))
	body := "0123456789abcdefghijklmnopqrstuvwxyz" // 36 字节
	e.fake.SetView(comfytest.ViewConfig{ContentType: "text/html", Body: []byte(body)})
	var gotRange []string
	var rmu sync.Mutex
	e.setOverride(func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/view" {
			rmu.Lock()
			gotRange = append(gotRange, r.Header.Get("Range"))
			rmu.Unlock()
		}
		return false
	})
	lastRange := func() string {
		rmu.Lock()
		defer rmu.Unlock()
		return gotRange[len(gotRange)-1]
	}
	open := func(rng string) (*File, string) {
		t.Helper()
		f, err := e.runs.OpenFile(e.ctx, e.getReq(run.ID), 0, rng)
		if err != nil {
			t.Fatalf("OpenFile(%q): %v", rng, err)
		}
		b, err := io.ReadAll(f.Body)
		f.Body.Close()
		if err != nil {
			t.Fatalf("read %q: %v", rng, err)
		}
		return f, string(b)
	}

	f, b := open("bytes=2-5")
	h := http.Header{}
	f.SetHeaders(h)
	if lastRange() != "bytes=2-5" || f.Status != http.StatusPartialContent || b != "2345" || f.Length != 4 ||
		h.Get("Content-Range") != "bytes 2-5/36" || h.Get("Accept-Ranges") != "bytes" || h.Get("Content-Length") != "4" ||
		h.Get("Content-Type") != "video/mp4" || h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Content-Security-Policy") != "default-src 'none'; sandbox" {
		t.Fatalf("range 2-5: %+v %q %v", f, b, h)
	}
	if f, b = open("bytes=30-"); lastRange() != "bytes=30-" || f.Status != 206 || b != "uvwxyz" || f.ContentRange != "bytes 30-35/36" {
		t.Fatalf("range 30-: %+v %q", f, b)
	}
	// 末尾超出文件长度：ComfyUI 截到文件尾
	if f, b = open("bytes=34-99"); f.Status != 206 || b != "yz" || f.ContentRange != "bytes 34-35/36" {
		t.Fatalf("range 34-99: %+v %q", f, b)
	}
	// 不转发：多段、后缀、反序、非 bytes、带空格 / 杂字符、超长数字
	for _, rng := range []string{"bytes=1-3,5-6", "bytes=-5", "bytes=5-1", "items=1-2", "bytes= 1-2", "bytes=1-2x", "bytes=1234567890123456789-"} {
		f, b = open(rng)
		h = http.Header{}
		f.SetHeaders(h)
		if lastRange() != "" || f.Status != http.StatusOK || b != body || h.Get("Content-Range") != "" || h.Get("Content-Length") != "36" {
			t.Fatalf("%q: forwarded %q, %+v %q", rng, lastRange(), f, b)
		}
	}
	// 起点在文件尾之后：ComfyUI 416 → 不带 Range 重取完整文件
	if f, b = open("bytes=36-"); f.Status != http.StatusOK || b != body || lastRange() != "" {
		t.Fatalf("unsatisfiable: %+v %q", f, b)
	}
	// ComfyUI 不支持 Range（回 200）：照常完整返回
	e.setOverride(func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/view" {
			return false
		}
		io.WriteString(w, body)
		return true
	})
	if f, b = open("bytes=2-5"); f.Status != http.StatusOK || b != body || f.ContentRange != "" {
		t.Fatalf("upstream ignores range: %+v %q", f, b)
	}
	// 敌意 / 错误的 206：Content-Range 与请求不符、缺失、长度不符 → 502
	for _, bad := range []struct{ cr, body string }{
		{"bytes 3-5/36", "345"},      // 起点不符
		{"bytes 2-9/36", "23456789"}, // 超出请求的终点
		{"", "2345"},                 // 缺 Content-Range
		{"bytes 2-5/*", "2345"},      // 总长未知
		{"bytes 2-5/5", "2345"},      // 终点 ≥ 总长
		{"bytes 2-5/36", "23"},       // Content-Length 不符
		{"bytes 2-5/36, 1", "2345"},
	} {
		e.setOverride(func(w http.ResponseWriter, r *http.Request) bool {
			if r.URL.Path != "/view" {
				return false
			}
			if bad.cr != "" {
				w.Header().Set("Content-Range", bad.cr)
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(bad.body)))
			w.WriteHeader(http.StatusPartialContent)
			io.WriteString(w, bad.body)
			return true
		})
		_, err := e.runs.OpenFile(e.ctx, e.getReq(run.ID), 0, "bytes=2-5")
		wantErr(t, err, "upstream_unreachable", 502)
	}
	// 文件总长超过上限：哪怕只取一小段也 413（与完整取一致）
	e.setOverride(nil)
	e.fake.SetView(comfytest.ViewConfig{Size: MaxViewBytes + 1})
	_, err := e.runs.OpenFile(e.ctx, e.getReq(run.ID), 0, "bytes=0-99")
	wantErr(t, err, "file_too_large", 413)
	e.fake.SetView(comfytest.ViewConfig{Size: MaxViewBytes})
	if f, b = open(fmt.Sprintf("bytes=%d-", MaxViewBytes-3)); f.Status != 206 || b != "xxx" ||
		f.ContentRange != fmt.Sprintf("bytes %d-%d/%d", MaxViewBytes-3, MaxViewBytes-1, MaxViewBytes) {
		t.Fatalf("tail of max file: %+v %q", f, b)
	}
}

func TestRunsLost(t *testing.T) {
	e := newRunEnv(t)
	run := e.mustStart()
	// 队列不可读：不能断定丢失
	e.fake.Forget(run.PromptID)
	e.setOverride(func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/queue" {
			return false
		}
		io.WriteString(w, `{"queue_running":[],"queue_pending":[],"pad":"`+strings.Repeat("a", maxQueueBytes)+`"}`)
		return true
	})
	if v := e.get(run.ID); v.State != RunQueued {
		t.Fatalf("unknown queue: %+v", v)
	}
	e.setOverride(nil)
	// history 空、队列里也没有 → 再读一次 history 仍空 → error「任务已丢失」
	e.clk.Advance(time.Minute)
	v := e.get(run.ID)
	if v.State != RunError || v.Message != lostMessage || v.FinishedAt == nil || !v.FinishedAt.Equal(e.clk.Now()) {
		t.Fatalf("lost: %+v", v)
	}
	if st, _ := e.runState(run.ID); st != RunError {
		t.Fatalf("row = %s", st)
	}
	// 终态 error 没有 history 消息时也给这句
	if v := e.get(run.ID); v.State != RunError || v.Message != lostMessage {
		t.Fatalf("again: %+v", v)
	}
	// 在 queue_pending 里：排队中
	p := e.mustStart()
	if v := e.get(p.ID); v.State != RunQueued {
		t.Fatalf("pending: %+v", v)
	}
	// 被中断
	e.fake.SetHistory(p.PromptID, comfytest.InterruptedState())
	if v := e.get(p.ID); v.State != RunError || v.Message != "任务已被中断" {
		t.Fatalf("interrupted: %+v", v)
	}
}

func TestRunsTimeout(t *testing.T) {
	e := newRunEnv(t)
	a, b := e.mustStart(), e.mustStart()
	e.clk.Advance(RunWaitMax + time.Second)
	// 超时前已完成的仍是 done
	e.fake.SetHistory(b.PromptID, comfytest.DoneN(1))
	if v := e.get(b.ID); v.State != RunDone {
		t.Fatalf("b: %+v", v)
	}
	// 仍在排队 / 运行：超过 RunWaitMax → error
	e.fake.SetHistory(a.PromptID, comfytest.RunningState())
	if v := e.get(a.ID); v.State != RunError || v.Message != timeoutMessage {
		t.Fatalf("a: %+v", v)
	}
	// Start 时遇到的超时 queued 行也落成 error
	c := e.mustStart()
	e.clk.Advance(RunWaitMax + time.Second)
	e.mustStart()
	if st, _ := e.runState(c.ID); st != RunError {
		t.Fatalf("c = %s", st)
	}
}

func TestRunsStartConcurrent(t *testing.T) {
	e := newRunEnv(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	codes := map[string]int{}
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.start(nil)
			mu.Lock()
			codes[errCode(err)]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if codes[""] != MaxRunsInFlight || codes["workflow_busy"] != 6-MaxRunsInFlight {
		t.Fatalf("codes = %v", codes)
	}
	if n := e.count(`SELECT count(*) FROM comfy_workflow_runs`); n != MaxRunsInFlight {
		t.Fatalf("rows = %d", n)
	}
}

func TestRunsStartSlowRefresh(t *testing.T) {
	e := newRunEnv(t)
	old := refreshProbeTimeout
	refreshProbeTimeout = 200 * time.Millisecond
	t.Cleanup(func() { refreshProbeTimeout = old })
	e.mustStart()
	e.mustStart()
	e.setOverride(func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasPrefix(r.URL.Path, "/history/") {
			return false
		}
		<-r.Context().Done() // 卡住直到客户端放弃
		return true
	})
	t0 := time.Now()
	_, err := e.start(nil)
	wantErr(t, err, "workflow_busy", 429)
	if d := time.Since(t0); d > 3*time.Second {
		t.Fatalf("refresh took %v", d)
	}
}

type blockingReader struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
	data    *bytes.Reader
}

func (b *blockingReader) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	<-b.release
	return b.data.Read(p)
}

func TestRunsUploadConcurrency(t *testing.T) {
	e := newRunEnv(t)
	release := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, MaxUploadsPerUser)
	for range MaxUploadsPerUser {
		br := &blockingReader{started: make(chan struct{}), release: release, data: bytes.NewReader(comfytest.OnePixelPNG)}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.runs.Upload(e.ctx, UploadRequest{UserID: e.uid, InstanceID: e.ins, DeploymentID: e.dep, WorkflowID: e.wf.ID, Body: br})
			errs <- err
		}()
		<-br.started
	}
	_, err := e.upload(comfytest.OnePixelPNG, 0)
	wantErr(t, err, "rate_limited", 429)
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.upload(comfytest.OnePixelPNG, 0); err != nil {
		t.Fatal(err)
	}
	// 大文件流式写进 multipart：大小原样到达 ComfyUI
	big := append(append([]byte{}, comfytest.OnePixelPNG...), make([]byte, 3<<20)...)
	if _, err := e.upload(big, 0); err != nil {
		t.Fatal(err)
	}
	if ups := e.fake.Uploads(); ups[len(ups)-1].Size != len(big) {
		t.Fatalf("size = %d", ups[len(ups)-1].Size)
	}
}

func TestHistoryErrorMessageSanitized(t *testing.T) {
	body := `{"p":{"status":{"status_str":"error","completed":false,"messages":[["execution_error",{"exception_message":"bad\u202e\u0007 thing\u2066"}]]},"outputs":{}}}`
	h, err := ParseHistory([]byte(body), "p", nil)
	if err != nil || h.State != RunError || h.Message != "bad  thing" {
		t.Fatalf("h = %+v %v", h, err)
	}
}

func TestRejectMessage(t *testing.T) {
	for body, want := range map[string]string{
		`{"node_errors":{"12":{"errors":[{"message":"b"}]},"4":{"errors":[{"message":"Value not in list","details":"ckpt_name: 'x.safetensors' not in []"}]}}}`: "Value not in list：ckpt_name: 'x.safetensors' not in []",
		`{"error":"plain string"}`: "plain string",
		`{}`:                       "工作流校验失败",
		`garbage`:                  "工作流校验失败",
		`{"node_errors":{"4":{"errors":[]}},"error":{"message":"Prompt outputs failed validation"}}`:                    "Prompt outputs failed validation",
		`{"error":{"type":"no_prompt","message":"No prompt provided","details":"No prompt provided"},"node_errors":[]}`: "No prompt provided",
		`{"error":{"message":"a\u202eb\u200fc\td"}}`:                                                                    "abc d",
	} {
		if got := rejectMessage([]byte(body)); got != want {
			t.Errorf("rejectMessage(%s) = %q, want %q", body, got, want)
		}
	}
}
