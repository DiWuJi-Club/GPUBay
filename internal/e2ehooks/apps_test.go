package e2ehooks_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/e2ehooks"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/rental"
)

// /hooks/apps/seed 幂等地建四个 fake 用户应用、集群应用 vllm-cluster 与平台内部应用 vllm-node（另建 LLM Serving / ComfyUI 镜像，默认镜像不改名）；/hooks/apps/tick 每轮推进假时钟 5 秒
// 并驱动 Runner 把预装的应用部署到 running；/hooks/agent 的 images 写进机器的已缓存镜像。
func TestAppsSeedTickAndAgentImages(t *testing.T) {
	e := newEnvWith(t, true)
	ctx := context.Background()
	pool := e.a.Fleet.Pool()
	defaultImg := dbtest.ImageID(t, pool)
	before, err := e.a.Catalog.GetImage(ctx, pool, defaultImg)
	if err != nil {
		t.Fatal(err)
	}

	seed := func() map[string]map[string]any {
		t.Helper()
		items := e.must("POST", "/hooks/apps/seed", map[string]any{})["apps"].([]any)
		out := map[string]map[string]any{}
		for _, it := range items {
			m := it.(map[string]any)
			out[m["slug"].(string)] = m
		}
		return out
	}
	first := seed()
	wantPorts := map[string]float64{"vllm": 8000, "open-webui": 3000, "comfyui": 8188, "jupyterlab": 8888, "vllm-node": 8000, "vllm-cluster": 8000}
	if len(first) != 6 {
		t.Fatalf("seed = %v", first)
	}
	for slug, port := range wantPorts {
		if first[slug]["port"] != port || first[slug]["id"].(float64) <= 0 || first[slug]["version_id"].(float64) <= 0 {
			t.Fatalf("seed %s = %v", slug, first[slug])
		}
	}
	if again := seed(); !mapsEqual(first, again) {
		t.Fatalf("seed not idempotent: %v then %v", first, again)
	}
	imgs, err := e.a.Catalog.AdminImages(ctx)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]int{}
	for _, img := range imgs {
		names[img.Name]++
	}
	if names[e2ehooks.ImageLLMServing] != 1 || names[e2ehooks.ImageComfyUI] != 1 {
		t.Fatalf("images = %+v", imgs)
	}
	if after, _ := e.a.Catalog.GetImage(ctx, pool, defaultImg); after.Name != before.Name {
		t.Fatalf("default image renamed: %q → %q", before.Name, after.Name)
	}
	// comfyui 只兼容 ComfyUI 镜像；jupyterlab 兼容三者
	vers := func(slug string) appdeploy.Version {
		v, err := e.a.AppCatalog.GetVersion(ctx, int64(first[slug]["version_id"].(float64)))
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	if ids := vers("comfyui").ImageIDs; len(ids) != 1 || slices.Contains(ids, defaultImg) {
		t.Fatalf("comfyui images = %v", ids)
	}
	if ids := vers("jupyterlab").ImageIDs; len(ids) != 3 || !slices.Contains(ids, defaultImg) {
		t.Fatalf("jupyterlab images = %v", ids)
	}
	// 计划 13：vllm-node 1.1.0（cluster optional 2～4）、vllm-cluster 1.0.0（cluster required 2～4）
	if v := vers("vllm-node"); v.Version != "1.1.0" || v.Spec.Cluster == nil || v.Spec.Cluster.Mode != "optional" || v.Spec.TimeoutMinutes != 360 {
		t.Fatalf("vllm-node version = %s spec cluster %+v timeout %d", v.Version, v.Spec.Cluster, v.Spec.TimeoutMinutes)
	}
	if v := vers("vllm-cluster"); v.Version != "1.0.0" || v.Spec.Cluster == nil || v.Spec.Cluster.Mode != "required" ||
		v.Spec.Cluster.MinSize != 2 || v.Spec.Cluster.MaxSize != 4 {
		t.Fatalf("vllm-cluster version = %s spec cluster %+v", v.Version, v.Spec.Cluster)
	}

	// 预装 vLLM → 开通 → tick 直到 running
	ids := e.must("POST", "/hooks/machines", map[string]any{"count": 1})["ids"].([]any)
	mid := int64(ids[0].(float64))
	u := e.must("POST", "/hooks/users", map[string]any{"verified": true, "balance": 100000})
	vllm := int64(first["vllm"]["id"].(float64))
	_, inst, err := e.a.Rental.Place(ctx, rental.PlaceRequest{UserID: int64(u["id"].(float64)), ProductID: dbtest.ProductID(t, pool),
		ImageID: defaultImg, Duration: catalog.D1d, AppID: &vllm})
	if err != nil {
		t.Fatal(err)
	}
	e.must("POST", "/hooks/agent", map[string]any{})
	live, err := e.a.Apps.ListLive(ctx, inst.ID)
	if err != nil || len(live) != 1 {
		t.Fatalf("deployments = %v %v", live, err)
	}
	if code, _ := e.call("POST", "/hooks/apps/tick", map[string]any{"n": 0}); code != 400 {
		t.Fatalf("n=0 = %d, want 400", code)
	}
	t0 := e.clk.Now()
	out := e.must("POST", "/hooks/apps/tick", map[string]any{"n": 2})
	if out["rounds"].(float64) != 2 || out["processed"].(float64) < 2 || !e.clk.Now().Equal(t0.Add(2*appdeploy.PollInterval)) {
		t.Fatalf("tick = %v, clock advanced %v", out, e.clk.Now().Sub(t0))
	}
	state := ""
	for i := 0; i < 40 && state != appdeploy.StateRunning; i++ {
		e.must("POST", "/hooks/apps/tick", map[string]any{})
		d, err := e.a.Apps.Get(ctx, live[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		state = d.State
	}
	if state != appdeploy.StateRunning {
		t.Fatalf("deployment state = %s after ticks, want running", state)
	}

	// images：本次 StateReport 起带上已缓存镜像
	e.must("POST", "/hooks/agent", map[string]any{"machine_id": mid, "images": []string{"spark/comfyui:e2e"}})
	m, err := e.a.Fleet.Get(ctx, pool, mid)
	if err != nil || len(m.CachedImages) != 1 || m.CachedImages[0] != "spark/comfyui:e2e" || m.CachedImagesAt == nil {
		t.Fatalf("cached images = %v at %v (%v)", m.CachedImages, m.CachedImagesAt, err)
	}
}

func mapsEqual(a, b map[string]map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		w, ok := b[k]
		if !ok || len(v) != len(w) {
			return false
		}
		for kk, vv := range v {
			if w[kk] != vv {
				return false
			}
		}
	}
	return true
}

// fake 的 vllm-node / vllm-cluster spec 与 deploy/apps 里的真实 spec.json 相同（只去掉 min_data_free_gib）。
func TestFakeClusterSpecsMatchDeploy(t *testing.T) {
	for _, slug := range []string{"vllm-node", "vllm-cluster"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "apps", slug, "spec.json"))
		if err != nil {
			t.Fatal(err)
		}
		var want appdeploy.Spec
		if err := json.Unmarshal(raw, &want); err != nil {
			t.Fatal(err)
		}
		want.MinDataFreeGiB = 0
		got, ok := e2ehooks.FakeSpec(slug)
		if !ok || !reflect.DeepEqual(got, want) {
			t.Fatalf("%s fake spec differs from deploy/apps:\n got %+v\nwant %+v", slug, got, want)
		}
		if err := appdeploy.ValidateSpec(&got); err != nil {
			t.Fatalf("%s: %v", slug, err)
		}
	}
}
