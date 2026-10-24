package appdeploy_test

// 计划 11：Runner 侧扩展点（BundleExtra、OnTransition、SPARK_GIT_MIRROR）。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/appdeploy"
)

type transition struct {
	dep, from, to string
	at            time.Time
	inTx          bool
}

type workflowHooks struct {
	mu          sync.Mutex
	transitions []transition
	extraCalls  []string // "<部署>:<版本 ID>"
	extraErr    map[string]error
}

func (h *workflowHooks) install(d *appdeploy.Deps) {
	d.GitMirror = "https://gh-proxy.com/"
	d.OnTransition = func(ctx context.Context, tx pgx.Tx, dep appdeploy.Deployment, to string, now time.Time) error {
		// 必须在回写事务里：同一事务能看到刚写的状态
		var state string
		err := tx.QueryRow(ctx, `SELECT state FROM app_deployments WHERE id = $1`, dep.ID).Scan(&state)
		h.mu.Lock()
		defer h.mu.Unlock()
		h.transitions = append(h.transitions, transition{dep: dep.ID, from: dep.State, to: to, at: now, inTx: err == nil && state == to})
		return nil
	}
	d.BundleExtra = func(_ context.Context, dep appdeploy.Deployment, ver appdeploy.Version) ([]appdeploy.BundleFile, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.extraCalls = append(h.extraCalls, dep.ID+":"+id(ver.ID))
		if err := h.extraErr[dep.ID]; err != nil {
			return nil, err
		}
		return []appdeploy.BundleFile{
			{Name: "workflows/manifest.json", Mode: 0o644, Body: []byte(`{"workflows":[{"slug":"sdxl-txt2img"}]}`)},
			{Name: "workflows/sdxl-txt2img.json", Mode: 0o644, Body: []byte(`{"nodes":[]}`)},
		}, nil
	}
}

func (h *workflowHooks) snapshot() ([]transition, []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]transition(nil), h.transitions...), append([]string(nil), h.extraCalls...)
}

func TestRunnerWorkflowHooks(t *testing.T) {
	h := &workflowHooks{extraErr: map[string]error{}}
	re := newRunnerEnvDeps(t, 1, h.install)
	wfSpec := runnerSpec(8000)
	wfSpec.SupportsWorkflows = true
	comfy, comfyVer := re.publishedApp("comfyui", wfSpec, runnerScript, []int64{re.imageID})
	plain := re.app("vllm", 8001)
	inst := re.instance()

	// 支持工作流的版本：部署包带 BundleExtra 的文件；running 时 OnTransition(running) 一次，在回写事务里
	d := re.create(inst, comfy.ID, nil)
	re.until(d.ID, appdeploy.StateRunning)
	dir := re.dir(inst.ID, "comfyui")
	if got, _ := os.ReadFile(filepath.Join(dir, "workflows", "sdxl-txt2img.json")); string(got) != `{"nodes":[]}` {
		t.Fatalf("workflow file = %q", got)
	}
	if env, _ := os.ReadFile(filepath.Join(dir, "env")); !strings.Contains(string(env), "export SPARK_GIT_MIRROR='https://gh-proxy.com/'\n") {
		t.Fatalf("env = %s", env)
	}
	trs, calls := h.snapshot()
	if len(calls) != 1 || calls[0] != d.ID+":"+id(comfyVer.ID) {
		t.Fatalf("BundleExtra calls = %v", calls)
	}
	if len(trs) != 1 || trs[0].dep != d.ID || trs[0].from != appdeploy.StateInstalling || trs[0].to != appdeploy.StateRunning ||
		!trs[0].inTx || !trs[0].at.Equal(*re.dep(d.ID).FinishedAt) {
		t.Fatalf("transitions = %+v", trs)
	}

	// 不支持工作流的版本：不调 BundleExtra，包里没有 workflows/；OnTransition 照样调用（钩子自己决定是否关心）
	p := re.create(inst, plain.ID, nil)
	re.until(p.ID, appdeploy.StateRunning)
	if _, err := os.Stat(filepath.Join(re.dir(inst.ID, "vllm"), "workflows")); err == nil {
		t.Fatal("plain app got workflows/")
	}
	trs, calls = h.snapshot()
	if len(calls) != 1 || len(trs) != 2 || trs[1].dep != p.ID || trs[1].to != appdeploy.StateRunning {
		t.Fatalf("after plain deploy: calls %v transitions %+v", calls, trs)
	}

	// BundleExtra 出错 → 部署包生成失败（upload_failed，消息带原因）；markFailed 里 OnTransition(failed) 一次
	h.mu.Lock()
	h.extraErr[d.ID] = errors.New("workflow 7 version gone")
	h.mu.Unlock()
	if _, err := re.svc.Redeploy(re.ctx, appdeploy.RedeployRequest{UserID: re.userID, InstanceID: inst.ID, DeploymentID: d.ID}); err != nil {
		t.Fatal(err)
	}
	got := re.until(d.ID, appdeploy.StateFailed)
	if got.ErrorCode != appdeploy.ErrCodeUploadFailed || got.Message != "部署包生成失败：workflow 7 version gone" {
		t.Fatalf("failed = %+v", got)
	}
	trs, _ = h.snapshot()
	last := trs[len(trs)-1]
	if len(trs) != 3 || last.dep != d.ID || last.from != appdeploy.StateQueued || last.to != appdeploy.StateFailed || !last.inTx {
		t.Fatalf("transitions = %+v", trs)
	}
}

// 容器内 install 失败（状态 failed 经 stepPoll → markFailed）同样触发 OnTransition(failed)；OnTransition 出错时回写回滚、下一轮重试。
func TestRunnerOnTransitionOnScriptFailure(t *testing.T) {
	h := &workflowHooks{extraErr: map[string]error{}}
	var fail sync.Mutex
	failing := true
	re := newRunnerEnvDeps(t, 1, func(d *appdeploy.Deps) {
		h.install(d)
		inner := d.OnTransition
		d.OnTransition = func(ctx context.Context, tx pgx.Tx, dep appdeploy.Deployment, to string, now time.Time) error {
			fail.Lock()
			f := failing
			failing = false
			fail.Unlock()
			if f {
				return errors.New("hook down")
			}
			return inner(ctx, tx, dep, to, now)
		}
	})
	a := re.env.app("comfyui", appdeploy.StatusPublished)
	spec := runnerSpec(8000)
	spec.SupportsWorkflows = true
	re.publish(a.ID, "1.0.0", spec, failScript, []int64{re.imageID})
	inst := re.instance()
	d := re.create(inst, a.ID, nil)
	deadline := time.Now().Add(30 * time.Second)
	for {
		_, locked, err := re.r.Tick(re.ctx)
		if !locked {
			t.Fatal("runner lock not taken")
		}
		if err == nil && re.dep(d.ID).State == appdeploy.StateFailed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never failed: %+v\n%s", re.dep(d.ID), re.logs.String())
		}
		re.clk.Advance(appdeploy.PollInterval)
		time.Sleep(50 * time.Millisecond)
	}
	got := re.dep(d.ID)
	if got.ErrorCode != appdeploy.ErrCodeScriptFailed {
		t.Fatalf("failed = %+v", got)
	}
	trs, _ := h.snapshot()
	if len(trs) != 1 || trs[0].dep != d.ID || trs[0].from != appdeploy.StateInstalling || trs[0].to != appdeploy.StateFailed || !trs[0].inTx {
		t.Fatalf("transitions = %+v", trs)
	}
	fail.Lock()
	defer fail.Unlock()
	if failing {
		t.Fatal("hook error path not exercised")
	}
	if logs := re.logs.String(); !strings.Contains(logs, "level=ERROR") || !strings.Contains(logs, "OnTransition") || !strings.Contains(logs, "deployment="+d.ID) {
		t.Fatalf("hook failure not logged at error level:\n%s", logs)
	}
}

// queued 部署的集合在本轮打包之后、回写之前变了（comfywf 对 queued 只改集合并 TouchDeployment）：本轮回写被丢弃，下一轮重新打包。
func TestRunnerRebundlesAfterTouch(t *testing.T) {
	h := &workflowHooks{extraErr: map[string]error{}}
	re := newRunnerEnvDeps(t, 1, h.install)
	spec := runnerSpec(8000)
	spec.SupportsWorkflows = true
	a, _ := re.publishedApp("comfyui", spec, runnerScript, []int64{re.imageID})
	inst := re.instance()
	d := re.create(inst, a.ID, nil)
	var once sync.Once
	appdeploy.SetBeforeWriteback(re.r, func(appdeploy.Deployment) {
		once.Do(func() {
			if err := appdeploy.TouchDeployment(re.ctx, re.pool, d.ID, re.clk.Now()); err != nil {
				t.Errorf("TouchDeployment: %v", err)
			}
		})
	})
	re.tick()
	if got := re.dep(d.ID); got.State != appdeploy.StateQueued {
		t.Fatalf("stale round not discarded: %+v", got)
	}
	if !strings.Contains(re.logs.String(), "stale writeback discarded") {
		t.Fatalf("no stale log:\n%s", re.logs.String())
	}
	if _, calls := h.snapshot(); len(calls) != 1 {
		t.Fatalf("BundleExtra calls after first round = %v", calls)
	}
	appdeploy.SetBeforeWriteback(re.r, nil)
	got := re.until(d.ID, appdeploy.StateRunning)
	trs, calls := h.snapshot()
	if len(calls) < 2 || len(trs) != 1 || trs[0].to != appdeploy.StateRunning {
		t.Fatalf("after re-bundle: calls %v transitions %+v (dep %+v)", calls, trs, got)
	}
}
