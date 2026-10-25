package llm_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/llm"
)

func TestProberThresholds(t *testing.T) {
	ne := newNodeEnv(t)
	ne.poolMachine("H1")
	m := ne.onlineModel("qwen3-4b", 1)
	ne.tick()
	n := ne.bringReady(m.ID)[0]
	alertMsg := "节点 " + n.ID + "（qwen3-4b）健康检查失败"

	// 连续 3 次失败 → unhealthy + 告警一次
	ne.probe.set(errProbe)
	for i := 1; i < llm.ProbeFailThreshold; i++ {
		ne.clk.Advance(10 * time.Second)
		ne.probeTick()
		got := ne.node(n.ID)
		if got.State != llm.NodeReady || got.ProbeFailCount != i || got.LastProbeAt == nil || !got.LastProbeAt.Equal(ne.clk.Now()) {
			t.Fatalf("fail %d: %+v", i, got)
		}
	}
	ne.clk.Advance(10 * time.Second)
	ne.probeTick()
	got := ne.node(n.ID)
	if got.State != llm.NodeUnhealthy || got.UnhealthySince == nil || !got.UnhealthySince.Equal(ne.clk.Now()) ||
		!strings.Contains(got.Error, "connection refused") {
		t.Fatalf("after threshold %+v", got)
	}
	ne.probeTick()
	ne.probeTick()
	var cnt int64 = ne.queryInt(`SELECT count(*) FROM alerts WHERE kind = $1 AND message = $2 AND machine_id = $3 AND instance_id = $4`,
		llm.AlertNodeUnhealthy, alertMsg, n.MachineID, *n.InstanceID)
	if cnt != 1 || ne.alertCount(llm.AlertNodeUnhealthy) != 1 {
		t.Fatalf("unhealthy alerts %d", cnt)
	}
	// 一次成功 → ready
	ne.probe.set(nil)
	ne.probeTick()
	got = ne.node(n.ID)
	if got.State != llm.NodeReady || got.ProbeFailCount != 0 || got.UnhealthySince != nil || got.Error != "" {
		t.Fatalf("recovered %+v", got)
	}

	// 部署 failed（如 crashed）→ 立即 unhealthy，不等 3 次
	ne.setDeployment(*n.DeploymentID, appdeploy.StateFailed, appdeploy.HealthUnknown, appdeploy.ErrCodeCrashed)
	ne.probeTick()
	if got := ne.node(n.ID); got.State != llm.NodeUnhealthy || !strings.Contains(got.Error, "服务进程已退出") {
		t.Fatalf("deployment failed: %+v", got)
	}
	ne.setDeployment(*n.DeploymentID, appdeploy.StateRunning, appdeploy.HealthOK, "")
	ne.probeTick()
	if ne.node(n.ID).State != llm.NodeReady {
		t.Fatal("not recovered after deployment running again")
	}

	// 实例暂停（机器失联）算失败，且不发 HTTP 探测
	ne.exec(`UPDATE instances SET suspended_at = $2 WHERE id = $1`, *n.InstanceID, ne.clk.Now())
	before := ne.probe.count()
	for i := 0; i < llm.ProbeFailThreshold; i++ {
		ne.probeTick()
	}
	if got := ne.node(n.ID); got.State != llm.NodeUnhealthy || !strings.Contains(got.Error, "暂停") {
		t.Fatalf("suspended: %+v", got)
	}
	if ne.probe.count() != before {
		t.Fatal("probed a suspended instance")
	}
	// 部署 health = unhealthy 也算一次失败
	ne.exec(`UPDATE instances SET suspended_at = NULL WHERE id = $1`, *n.InstanceID)
	ne.probeTick()
	if ne.node(n.ID).State != llm.NodeReady {
		t.Fatal("not recovered after resume")
	}
	ne.setDeployment(*n.DeploymentID, appdeploy.StateRunning, appdeploy.HealthUnhealthy, "")
	ne.probeTick()
	if got := ne.node(n.ID); got.State != llm.NodeReady || got.ProbeFailCount != 1 {
		t.Fatalf("deployment health unhealthy: %+v", got)
	}
	// ready 节点探测成功：清掉上次的错误
	ne.setDeployment(*n.DeploymentID, appdeploy.StateRunning, appdeploy.HealthOK, "")
	ne.probeTick()
	if got := ne.node(n.ID); got.Error != "" || got.ProbeFailCount != 0 {
		t.Fatalf("error not cleared: %+v", got)
	}
	if ne.alertCount(llm.AlertNodeUnhealthy) != 1 {
		t.Fatal("alert raised again while the first is unresolved")
	}
}

func TestProberPromotesProvisioning(t *testing.T) {
	ne := newNodeEnv(t)
	ne.poolMachine("H2")
	m := ne.onlineModel("qwen3-4b", 1)
	ne.tick()
	n := ne.nodesOf(m.ID)[0]
	ne.setInstance(*n.InstanceID, "running")
	ne.tick()
	dep := *ne.node(n.ID).DeploymentID
	ne.probeTick() // 部署 queued：不探测
	if ne.probe.count() != 0 {
		t.Fatal("probed a queued deployment")
	}
	ne.setDeployment(dep, appdeploy.StateRunning, appdeploy.HealthOK, "")
	ne.probeTick()
	if got := ne.node(n.ID); got.State != llm.NodeReady || got.LastReadyAt == nil {
		t.Fatalf("not promoted: %+v", got)
	}
}

func TestDefaultProbeHTTP(t *testing.T) {
	var gotAuth, gotPath string
	status := http.StatusOK
	body := `{"object":"list","data":[{"id":"qwen3-4b","object":"model"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	dial := func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", srv.Listener.Addr().String())
	}
	ctx := context.Background()
	if err := llm.ProbeModels(ctx, dial, "qwen3-4b", "nodekey"); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if gotAuth != "Bearer nodekey" || gotPath != "/v1/models" {
		t.Fatalf("auth %q path %q", gotAuth, gotPath)
	}
	if err := llm.ProbeModels(ctx, dial, "other", "nodekey"); err == nil {
		t.Fatal("slug not served should fail")
	}
	status = http.StatusUnauthorized
	if err := llm.ProbeModels(ctx, dial, "qwen3-4b", "nodekey"); err == nil {
		t.Fatal("401 should fail")
	}
	status, body = http.StatusOK, `not json`
	if err := llm.ProbeModels(ctx, dial, "qwen3-4b", "nodekey"); err == nil {
		t.Fatal("bad body should fail")
	}
	if err := llm.ProbeModels(ctx, func(context.Context) (net.Conn, error) { return nil, errProbe }, "qwen3-4b", "k"); err == nil {
		t.Fatal("dial error should fail")
	}
}
