package llm_test

import (
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/llm"
	"spark-cloud/internal/platform/apperr"
)

// 规格 §3.1 的校验矩阵：每个非法输入都是 llm_model_invalid（消息带字段名），重复 slug 是 llm_model_slug_exists。
func TestCreateModelValidationMatrix(t *testing.T) {
	e := newEnv(t)
	base := e.createModel("qwen3-4b")
	if base.Status != llm.ModelDraft || base.Slug != "qwen3-4b" || base.ImageID != e.imageID || base.MaxConcurrency != 32 {
		t.Fatalf("base model: %+v", base)
	}
	if len(base.Features) != 1 || base.Features[0] != "tools" || base.NodeEnv == nil {
		t.Fatalf("features / node_env: %+v %+v", base.Features, base.NodeEnv)
	}
	if e.auditCount("llm.model.create", strconv.FormatInt(base.ID, 10)) != 1 {
		t.Fatal("create audit missing")
	}

	disabled := e.insertImage("Old", "old:1", false) // 在兼容列表里但已下架
	e.exec(`UPDATE app_versions SET image_ids = image_ids || $2::bigint WHERE id = $1`, e.versionID, disabled)
	draftVer := e.addNodeVersion("2.0.0-draft", "draft", e.imageID)
	otherApp := e.queryInt(`INSERT INTO apps (slug, name, category, status, created_at, updated_at) VALUES ('other', 'Other', 'tool', 'published', $1, $1) RETURNING id`, e.clk.Now())
	otherVer := e.queryInt(`INSERT INTO app_versions (app_id, version, status, image_ids, spec, script, spec_hash, published_at, created_at, updated_at)
		VALUES ($1, '1.0.0', 'published', $2, '{"ports":[{"port":8000}]}', 'true', 'h', $3, $3, $3) RETURNING id`, otherApp, []int64{e.imageID}, e.clk.Now())

	cases := []struct {
		name  string
		mod   func(*llm.ModelInput)
		code  string
		field string // 消息前缀（字段名）；空 = 不检查
	}{
		{"slug uppercase", func(in *llm.ModelInput) { in.Slug = "Qwen3" }, "llm_model_invalid", "slug"},
		{"slug too short", func(in *llm.ModelInput) { in.Slug = "q" }, "llm_model_invalid", "slug"},
		{"slug leading dash", func(in *llm.ModelInput) { in.Slug = "-qwen" }, "llm_model_invalid", "slug"},
		{"slug too long", func(in *llm.ModelInput) { in.Slug = "q" + strings.Repeat("a", 64) }, "llm_model_invalid", "slug"},
		{"slug slash", func(in *llm.ModelInput) { in.Slug = "qwen/3" }, "llm_model_invalid", "slug"},
		{"slug duplicate", func(in *llm.ModelInput) { in.Slug = "qwen3-4b" }, "llm_model_slug_exists", ""},
		{"name empty", func(in *llm.ModelInput) { in.Name = "  " }, "llm_model_invalid", "name"},
		{"name too long", func(in *llm.ModelInput) { in.Name = strings.Repeat("模", 65) }, "llm_model_invalid", "name"},
		{"description too long", func(in *llm.ModelInput) { in.Description = strings.Repeat("x", 2001) }, "llm_model_invalid", "description"},
		{"kind", func(in *llm.ModelInput) { in.Kind = "image" }, "llm_model_invalid", "kind"},
		{"source", func(in *llm.ModelInput) { in.Source = "github" }, "llm_model_invalid", "source"},
		{"source_id no slash", func(in *llm.ModelInput) { in.SourceID = "Qwen3-4B" }, "llm_model_invalid", "source_id"},
		{"source_id space", func(in *llm.ModelInput) { in.SourceID = "Qwen/Qwen 3" }, "llm_model_invalid", "source_id"},
		{"source_id two slashes", func(in *llm.ModelInput) { in.SourceID = "a/b/c" }, "llm_model_invalid", "source_id"},
		{"source_id dot-dot org", func(in *llm.ModelInput) { in.SourceID = "../etc" }, "llm_model_invalid", "source_id"},
		{"source_id dot-dot repo", func(in *llm.ModelInput) { in.SourceID = "Qwen/.." }, "llm_model_invalid", "source_id"},
		{"source_id leading dash", func(in *llm.ModelInput) { in.SourceID = "-rf/x" }, "llm_model_invalid", "source_id"},
		{"source_id leading underscore", func(in *llm.ModelInput) { in.SourceID = "Qwen/_x" }, "llm_model_invalid", "source_id"},
		{"input_price negative", func(in *llm.ModelInput) { in.InputPrice = -1 }, "llm_model_invalid", "input_price"},
		{"output_price negative", func(in *llm.ModelInput) { in.OutputPrice = -1 }, "llm_model_invalid", "output_price"},
		{"context_len low", func(in *llm.ModelInput) { in.ContextLen = 1023 }, "llm_model_invalid", "context_len"},
		{"context_len high", func(in *llm.ModelInput) { in.ContextLen = 1048577 }, "llm_model_invalid", "context_len"},
		{"max_output_tokens high", func(in *llm.ModelInput) { in.ContextLen = 1048576; in.MaxOutputTokens = 262145 }, "llm_model_invalid", "max_output_tokens"},
		{"max_output_tokens > context_len", func(in *llm.ModelInput) { in.ContextLen = 4096; in.MaxOutputTokens = 8192 }, "llm_model_invalid", "max_output_tokens"},
		{"default_max_tokens > max_output", func(in *llm.ModelInput) { in.DefaultMaxTokens = 8193 }, "llm_model_invalid", "default_max_tokens"},
		{"replicas", func(in *llm.ModelInput) { in.Replicas = 201 }, "llm_model_invalid", "replicas"},
		{"replicas negative", func(in *llm.ModelInput) { in.Replicas = -1 }, "llm_model_invalid", "replicas"},
		{"max_concurrency", func(in *llm.ModelInput) { in.MaxConcurrency = 513 }, "llm_model_invalid", "max_concurrency"},
		{"sort", func(in *llm.ModelInput) { in.Sort = 10001 }, "llm_model_invalid", "sort"},
		{"feature unknown", func(in *llm.ModelInput) { in.Features = []string{"fast"} }, "llm_model_invalid", "features"},
		{"node_env not declared", func(in *llm.ModelInput) { in.NodeEnv = map[string]string{"FOO": "1"} }, "llm_model_invalid", "node_env"},
		{"node_env reserved MODEL_ID", func(in *llm.ModelInput) { in.NodeEnv = map[string]string{"MODEL_ID": "a/b"} }, "llm_model_invalid", "node_env"},
		{"node_env reserved API_KEY", func(in *llm.ModelInput) { in.NodeEnv = map[string]string{"API_KEY": strings.Repeat("k", 40)} }, "llm_model_invalid", "node_env"},
		{"node_env reserved MAX_MODEL_LEN", func(in *llm.ModelInput) { in.NodeEnv = map[string]string{"MAX_MODEL_LEN": "4096"} }, "llm_model_invalid", "node_env"},
		{"node_env reserved TASK", func(in *llm.ModelInput) { in.NodeEnv = map[string]string{"TASK": "embed"} }, "llm_model_invalid", "node_env"},
		{"node_env int out of range", func(in *llm.ModelInput) { in.NodeEnv = map[string]string{"MAX_NUM_SEQS": "0"} }, "llm_model_invalid", "node_env"},
		{"node_env enum", func(in *llm.ModelInput) { in.NodeEnv = map[string]string{"QUANTIZATION": "int4"} }, "llm_model_invalid", "node_env"},
		{"node_env pattern", func(in *llm.ModelInput) { in.NodeEnv = map[string]string{"GPU_MEMORY_UTILIZATION": "0.95x"} }, "llm_model_invalid", "node_env"},
		{"image not compatible", func(in *llm.ModelInput) { in.ImageID = e.otherImageID }, "llm_model_invalid", "image_id"},
		{"image disabled", func(in *llm.ModelInput) { in.ImageID = disabled }, "llm_model_invalid", "image_id"},
		{"image missing", func(in *llm.ModelInput) { in.ImageID = 999999 }, "llm_model_invalid", "image_id"},
		{"app_version draft", func(in *llm.ModelInput) { in.AppVersionID = &draftVer }, "llm_model_invalid", "app_version_id"},
		{"app_version other app", func(in *llm.ModelInput) { in.AppVersionID = &otherVer }, "llm_model_invalid", "app_version_id"},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := e.modelInput("m" + strconv.Itoa(i) + "-x")
			c.mod(&in)
			_, err := e.svc.CreateModel(e.ctx, e.admin(), in)
			if errCode(err) != c.code {
				t.Fatalf("err = %v, want %s", err, c.code)
			}
			if c.field != "" && !strings.HasPrefix(apperr.As(err).Message, c.field+"：") {
				t.Fatalf("message %q, want prefix %q", apperr.As(err).Message, c.field)
			}
		})
	}
	if n := e.queryInt(`SELECT count(*) FROM llm_models`); n != 1 {
		t.Fatalf("models = %d, want only the base one", n)
	}

	// 合法边界：零值补默认、node_env 白名单内的值、显式 app_version_id、embedding、hf-mirror
	ok := e.createModel("bge-m3", func(in *llm.ModelInput) {
		in.Kind, in.Source, in.SourceID = llm.KindEmbedding, "hf-mirror", "BAAI/bge-m3"
		in.ContextLen, in.MaxOutputTokens, in.DefaultMaxTokens, in.MaxConcurrency = 2048, 0, 0, 0
		in.OutputPrice, in.Features = 0, nil
		in.NodeEnv = map[string]string{"GPU_MEMORY_UTILIZATION": "0.5", "MAX_NUM_SEQS": "128", "QUANTIZATION": "fp8", "MODEL_SIZE_GB": "3", "TOOL_CALL_PARSER": ""}
		in.AppVersionID = &e.versionID
	})
	if ok.MaxOutputTokens != 2048 || ok.DefaultMaxTokens != 2048 || ok.MaxConcurrency != llm.DefaultNodeConcurrency {
		t.Fatalf("defaults: %+v", ok)
	}
	if ok.Features == nil || len(ok.Features) != 0 {
		t.Fatalf("features = %#v, want empty non-nil", ok.Features)
	}
	if len(ok.NodeEnv) != 4 || ok.NodeEnv["QUANTIZATION"] != "fp8" {
		t.Fatalf("node_env = %v (empty values dropped)", ok.NodeEnv)
	}
	if ok.AppVersionID == nil || *ok.AppVersionID != e.versionID {
		t.Fatalf("app_version_id = %v", ok.AppVersionID)
	}
	got, err := e.svc.ModelBySlug(e.ctx, e.pool, "bge-m3")
	if err != nil || got.ID != ok.ID || got.NodeEnv["MODEL_SIZE_GB"] != "3" || got.Kind != llm.KindEmbedding {
		t.Fatalf("ModelBySlug: %+v %v", got, err)
	}
	if _, err := e.svc.ModelBySlug(e.ctx, e.pool, "nope"); !errors.Is(err, apperr.ErrLLMModelNotFound) {
		t.Fatalf("ModelBySlug missing: %v", err)
	}

	// vllm-node 没有已发布版本 → 清楚的 llm_model_invalid
	e.exec(`UPDATE app_versions SET status = 'retired' WHERE app_id = $1`, e.appID)
	_, err = e.svc.CreateModel(e.ctx, e.admin(), e.modelInput("late-1"))
	if errCode(err) != "llm_model_invalid" || !strings.Contains(apperr.As(err).Message, llm.NodeAppSlug) {
		t.Fatalf("no published version: %v", err)
	}
	// vllm-node 不存在
	e.exec(`UPDATE apps SET slug = 'vllm-node-gone' WHERE id = $1`, e.appID)
	_, err = e.svc.CreateModel(e.ctx, e.admin(), e.modelInput("late-2"))
	if errCode(err) != "llm_model_invalid" || !strings.Contains(apperr.As(err).Message, llm.NodeAppSlug) {
		t.Fatalf("no vllm-node app: %v", err)
	}
}

// 状态机：draft → online 需要 replicas ≥ 1；online ↔ offline；不能回到 draft；每次变化一条 llm.model.status。
func TestModelStatusMachine(t *testing.T) {
	e := newEnv(t)
	m := e.createModel("qwen3-4b")
	target := strconv.FormatInt(m.ID, 10)

	_, err := e.svc.SetModelStatus(e.ctx, e.admin(), m.ID, llm.ModelOnline)
	if errCode(err) != "llm_model_invalid" || apperr.As(err).Message != "上线前请设置至少 1 个副本" {
		t.Fatalf("online with 0 replicas: %v", err)
	}
	if _, err := e.svc.SetModelStatus(e.ctx, e.admin(), m.ID, llm.ModelOffline); !errors.Is(err, apperr.ErrInvalidState) {
		t.Fatalf("draft → offline: %v", err)
	}
	if _, err := e.svc.SetModelStatus(e.ctx, e.admin(), m.ID, "bogus"); errCode(err) != "llm_model_invalid" {
		t.Fatalf("bogus status: %v", err)
	}
	if _, err := e.svc.SetModelStatus(e.ctx, e.admin(), 999999, llm.ModelOnline); !errors.Is(err, apperr.ErrLLMModelNotFound) {
		t.Fatalf("missing model: %v", err)
	}
	if online, _ := e.svc.ListOnlineModels(e.ctx); len(online) != 0 {
		t.Fatalf("online = %v", online)
	}

	in := e.modelInput("ignored")
	in.Replicas = 2
	if _, err := e.svc.UpdateModel(e.ctx, e.admin(), m.ID, in); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(time.Minute)
	m, err = e.svc.SetModelStatus(e.ctx, e.admin(), m.ID, llm.ModelOnline)
	if err != nil || m.Status != llm.ModelOnline || !m.UpdatedAt.Equal(e.clk.Now()) {
		t.Fatalf("online: %+v %v", m, err)
	}
	online, err := e.svc.ListOnlineModels(e.ctx)
	if err != nil || len(online) != 1 || online[0].ID != m.ID {
		t.Fatalf("ListOnlineModels: %v %v", online, err)
	}
	// 在线模型不能把副本改成 0
	in.Replicas = 0
	if _, err := e.svc.UpdateModel(e.ctx, e.admin(), m.ID, in); errCode(err) != "llm_model_invalid" {
		t.Fatalf("online replicas 0: %v", err)
	}
	// 只有 draft 能改 kind
	in.Replicas, in.Kind = 2, llm.KindEmbedding
	if _, err := e.svc.UpdateModel(e.ctx, e.admin(), m.ID, in); errCode(err) != "llm_model_invalid" || !strings.HasPrefix(apperr.As(err).Message, "kind：") {
		t.Fatalf("online kind change: %v", err)
	}
	in.Kind = llm.KindChat
	if _, err := e.svc.SetModelStatus(e.ctx, e.admin(), m.ID, llm.ModelDraft); !errors.Is(err, apperr.ErrInvalidState) {
		t.Fatalf("online → draft: %v", err)
	}

	if m, err = e.svc.SetModelStatus(e.ctx, e.admin(), m.ID, llm.ModelOffline); err != nil || m.Status != llm.ModelOffline {
		t.Fatalf("offline: %+v %v", m, err)
	}
	if online, _ := e.svc.ListOnlineModels(e.ctx); len(online) != 0 {
		t.Fatalf("online after offline = %v", online)
	}
	all, err := e.svc.ListModels(e.ctx)
	if err != nil || len(all) != 1 || all[0].Status != llm.ModelOffline {
		t.Fatalf("ListModels: %v %v", all, err)
	}
	// offline 时可以把副本调成 0（期望副本本来就按 0），再上线又要求 ≥ 1
	in.Replicas = 0
	if _, err := e.svc.UpdateModel(e.ctx, e.admin(), m.ID, in); err != nil {
		t.Fatalf("offline replicas 0: %v", err)
	}
	if _, err := e.svc.SetModelStatus(e.ctx, e.admin(), m.ID, llm.ModelOnline); errCode(err) != "llm_model_invalid" {
		t.Fatalf("offline → online with 0 replicas: %v", err)
	}
	in.Replicas = 1
	if _, err := e.svc.UpdateModel(e.ctx, e.admin(), m.ID, in); err != nil {
		t.Fatal(err)
	}
	if m, err = e.svc.SetModelStatus(e.ctx, e.admin(), m.ID, llm.ModelOnline); err != nil || m.Status != llm.ModelOnline {
		t.Fatalf("offline → online: %+v %v", m, err)
	}
	// 同状态：幂等，不写审计
	if _, err := e.svc.SetModelStatus(e.ctx, e.admin(), m.ID, llm.ModelOnline); err != nil {
		t.Fatalf("online → online: %v", err)
	}

	if n := e.auditCount("llm.model.status", target); n != 3 {
		t.Fatalf("status audits = %d, want 3", n)
	}
	d := e.lastAuditDiff("llm.model.status", target)
	if d["from"] != llm.ModelOffline || d["to"] != llm.ModelOnline {
		t.Fatalf("status diff = %v", d)
	}
}

// 删除：只有无节点、无用量的 draft；其它一律 llm_model_in_use；不存在 llm_model_not_found。
func TestDeleteModelConditions(t *testing.T) {
	e := newEnv(t)

	clean := e.createModel("clean-1")
	if err := e.svc.DeleteModel(e.ctx, e.admin(), clean.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.GetModel(e.ctx, clean.ID); !errors.Is(err, apperr.ErrLLMModelNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	if e.auditCount("llm.model.delete", strconv.FormatInt(clean.ID, 10)) != 1 {
		t.Fatal("delete audit missing")
	}
	if d := e.lastAuditDiff("llm.model.delete", strconv.FormatInt(clean.ID, 10)); d["slug"] != "clean-1" {
		t.Fatalf("delete diff = %v", d)
	}
	if err := e.svc.DeleteModel(e.ctx, e.admin(), clean.ID); !errors.Is(err, apperr.ErrLLMModelNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
	// slug 删除后可复用
	e.createModel("clean-1")

	online := e.onlineModel("online-1", 1)
	if err := e.svc.DeleteModel(e.ctx, e.admin(), online.ID); !errors.Is(err, apperr.ErrLLMModelInUse) {
		t.Fatalf("online: %v", err)
	}
	offline := e.onlineModel("offline-1", 1)
	if _, err := e.svc.SetModelStatus(e.ctx, e.admin(), offline.ID, llm.ModelOffline); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.DeleteModel(e.ctx, e.admin(), offline.ID); !errors.Is(err, apperr.ErrLLMModelInUse) {
		t.Fatalf("offline: %v", err)
	}

	withNode := e.createModel("node-1")
	e.rawNode("node-00000000test", withNode.ID, e.poolMachine("SN-DEL-1"), llm.NodeRemoved)
	if err := e.svc.DeleteModel(e.ctx, e.admin(), withNode.ID); !errors.Is(err, apperr.ErrLLMModelInUse) {
		t.Fatalf("with (removed) node: %v", err)
	}

	withUsage := e.createModel("usage-1")
	u := e.verifiedUser()
	e.rawUsage(u, e.rawKey(u), withUsage.ID, 1000)
	if err := e.svc.DeleteModel(e.ctx, e.admin(), withUsage.ID); !errors.Is(err, apperr.ErrLLMModelInUse) {
		t.Fatalf("with usage: %v", err)
	}

	withDaily := e.createModel("daily-1")
	e.exec(`INSERT INTO llm_usage_daily (day, user_id, model_id, requests) VALUES ('2026-01-01', $1, $2, 1)`, u, withDaily.ID)
	if err := e.svc.DeleteModel(e.ctx, e.admin(), withDaily.ID); !errors.Is(err, apperr.ErrLLMModelInUse) {
		t.Fatalf("with daily usage: %v", err)
	}

	if err := e.svc.DeleteModel(e.ctx, e.admin(), 999999); !errors.Is(err, apperr.ErrLLMModelNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if n := e.queryInt(`SELECT count(*) FROM audit_logs WHERE action = 'llm.model.delete'`); n != 1 {
		t.Fatalf("delete audits = %d, want 1", n)
	}
}

// 改价：审计 llm.model.update 带 before / after，价格差在 diff 里；slug 不可改；非法输入不落库；不存在 llm_model_not_found。
func TestUpdateModelAuditsPriceDiff(t *testing.T) {
	e := newEnv(t)
	m := e.createModel("qwen3-4b")
	target := strconv.FormatInt(m.ID, 10)

	e.clk.Advance(time.Hour)
	in := e.modelInput("renamed-slug")
	in.Name, in.InputPrice, in.OutputPrice = "Qwen3 4B Instruct", 150, 600
	in.NodeEnv = map[string]string{"REASONING_PARSER": "qwen3"}
	in.Features = []string{"reasoning", "tools", "reasoning"}
	got, err := e.svc.UpdateModel(e.ctx, e.admin(), m.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	if got.Slug != "qwen3-4b" || got.InputPrice != 150 || got.OutputPrice != 600 || got.Name != "Qwen3 4B Instruct" {
		t.Fatalf("updated: %+v", got)
	}
	if len(got.Features) != 2 || got.NodeEnv["REASONING_PARSER"] != "qwen3" {
		t.Fatalf("features / node_env: %v %v", got.Features, got.NodeEnv)
	}
	if !got.CreatedAt.Equal(m.CreatedAt) || !got.UpdatedAt.Equal(e.clk.Now()) {
		t.Fatalf("timestamps: %v %v", got.CreatedAt, got.UpdatedAt)
	}
	reread, err := e.svc.GetModel(e.ctx, m.ID)
	if err != nil || reread.InputPrice != 150 || reread.Slug != "qwen3-4b" {
		t.Fatalf("GetModel: %+v %v", reread, err)
	}

	if n := e.auditCount("llm.model.update", target); n != 1 {
		t.Fatalf("update audits = %d", n)
	}
	d := e.lastAuditDiff("llm.model.update", target)
	before, _ := d["before"].(map[string]any)
	after, _ := d["after"].(map[string]any)
	if before["input_price"] != float64(100) || after["input_price"] != float64(150) ||
		before["output_price"] != float64(400) || after["output_price"] != float64(600) {
		t.Fatalf("price diff: before=%v after=%v", before, after)
	}
	if before["name"] != "Qwen3 4B" || after["name"] != "Qwen3 4B Instruct" || after["slug"] != "qwen3-4b" {
		t.Fatalf("diff: before=%v after=%v", before, after)
	}

	// 非法输入：不改库、不写审计
	in.OutputPrice = -5
	if _, err := e.svc.UpdateModel(e.ctx, e.admin(), m.ID, in); errCode(err) != "llm_model_invalid" {
		t.Fatalf("negative price: %v", err)
	}
	in.OutputPrice = 600
	in.NodeEnv = map[string]string{"SERVED_MODEL_NAME": "x"}
	if _, err := e.svc.UpdateModel(e.ctx, e.admin(), m.ID, in); errCode(err) != "llm_model_invalid" {
		t.Fatalf("reserved env: %v", err)
	}
	if reread, _ := e.svc.GetModel(e.ctx, m.ID); reread.OutputPrice != 600 || reread.NodeEnv["REASONING_PARSER"] != "qwen3" {
		t.Fatalf("after rejected update: %+v", reread)
	}
	if n := e.auditCount("llm.model.update", target); n != 1 {
		t.Fatalf("update audits after rejects = %d", n)
	}
	in.NodeEnv = nil
	if _, err := e.svc.UpdateModel(e.ctx, e.admin(), 999999, in); !errors.Is(err, apperr.ErrLLMModelNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := e.svc.GetModel(e.ctx, 999999); !errors.Is(err, apperr.ErrLLMModelNotFound) {
		t.Fatalf("GetModel missing: %v", err)
	}
}

// draft 模型可以改 kind。
func TestUpdateModelKindWhileDraft(t *testing.T) {
	e := newEnv(t)
	m := e.createModel("bge-m3")
	in := e.modelInput("bge-m3")
	in.Kind = llm.KindEmbedding
	got, err := e.svc.UpdateModel(e.ctx, e.admin(), m.ID, in)
	if err != nil || got.Kind != llm.KindEmbedding {
		t.Fatalf("draft kind change: %+v %v", got, err)
	}
}

// 节点参数只在变化时复核：vllm-node 版本全部下架 / 镜像下架后，只改价仍成功；改 node_env 或 image 就要过校验。
func TestUpdateModelRevalidatesNodeParamsOnlyWhenChanged(t *testing.T) {
	e := newEnv(t)
	m := e.createModel("qwen3-4b", func(in *llm.ModelInput) { in.NodeEnv = map[string]string{"MAX_NUM_SEQS": "32"} })
	e.exec(`UPDATE app_versions SET status = 'retired' WHERE app_id = $1`, e.appID)
	e.exec(`UPDATE images SET enabled = false WHERE id = $1`, e.imageID)

	in := e.modelInput("qwen3-4b")
	in.NodeEnv = map[string]string{"MAX_NUM_SEQS": "32"}
	in.InputPrice = 999
	got, err := e.svc.UpdateModel(e.ctx, e.admin(), m.ID, in)
	if err != nil || got.InputPrice != 999 {
		t.Fatalf("price-only update: %+v %v", got, err)
	}
	in.NodeEnv = map[string]string{"MAX_NUM_SEQS": "48"}
	if _, err := e.svc.UpdateModel(e.ctx, e.admin(), m.ID, in); errCode(err) != "llm_model_invalid" || !strings.Contains(apperr.As(err).Message, llm.NodeAppSlug) {
		t.Fatalf("node_env change without published vllm-node: %v", err)
	}
	// 重新发布 vllm-node 后：镜像仍下架 → image_id 报错（node_env 变了触发整体复核）
	e.exec(`UPDATE app_versions SET status = 'published' WHERE id = $1`, e.versionID)
	if _, err := e.svc.UpdateModel(e.ctx, e.admin(), m.ID, in); errCode(err) != "llm_model_invalid" || !strings.HasPrefix(apperr.As(err).Message, "image_id：") {
		t.Fatalf("node_env change with disabled image: %v", err)
	}
	e.exec(`UPDATE images SET enabled = true WHERE id = $1`, e.imageID)
	if got, err := e.svc.UpdateModel(e.ctx, e.admin(), m.ID, in); err != nil || got.NodeEnv["MAX_NUM_SEQS"] != "48" {
		t.Fatalf("node_env change after recovery: %+v %v", got, err)
	}
	if n := e.auditCount("llm.model.update", strconv.FormatInt(m.ID, 10)); n != 2 {
		t.Fatalf("update audits = %d, want 2", n)
	}
}

// node_env 按 app_version_id 固定的版本的 spec 校验；未固定时按最新已发布版本。
func TestNodeEnvValidatedAgainstPinnedVersion(t *testing.T) {
	e := newEnv(t)
	full := e.versionID
	reduced := vllmNodeSpec(t)
	reduced.Env = slices.DeleteFunc(reduced.Env, func(v appdeploy.EnvSpec) bool { return v.Name == "QUANTIZATION" })
	e.clk.Advance(time.Minute)
	latest := e.addNodeVersionSpec("1.1.0", "published", reduced, e.imageID) // 最新版本不再声明 QUANTIZATION

	env := map[string]string{"QUANTIZATION": "fp8"}
	_, err := e.svc.CreateModel(e.ctx, e.admin(), func() llm.ModelInput { in := e.modelInput("a-1"); in.NodeEnv = env; return in }())
	if errCode(err) != "llm_model_invalid" || !strings.HasPrefix(apperr.As(err).Message, "node_env：") {
		t.Fatalf("latest version lacks QUANTIZATION: %v", err)
	}
	m := e.createModel("a-2", func(in *llm.ModelInput) { in.NodeEnv, in.AppVersionID = env, &full })
	if m.AppVersionID == nil || *m.AppVersionID != full || m.NodeEnv["QUANTIZATION"] != "fp8" {
		t.Fatalf("pinned full version: %+v", m)
	}
	// 改为固定到新版本 → 按新版本 spec 复核
	in := e.modelInput("a-2")
	in.NodeEnv, in.AppVersionID = env, &latest
	if _, err := e.svc.UpdateModel(e.ctx, e.admin(), m.ID, in); errCode(err) != "llm_model_invalid" || !strings.HasPrefix(apperr.As(err).Message, "node_env：") {
		t.Fatalf("repin to reduced version: %v", err)
	}
}

// 平台填写的节点变量与合并结果（平台值优先）。
func TestPlatformNodeEnv(t *testing.T) {
	chat := llm.Model{Slug: "qwen3-4b", Kind: llm.KindChat, Source: "modelscope", SourceID: "Qwen/Qwen3-4B", ContextLen: 32768,
		NodeEnv: map[string]string{"MAX_NUM_SEQS": "128", "TASK": "embed", "MODEL_ID": "evil/evil"}}
	key := strings.Repeat("k", 43)
	p := llm.PlatformNodeEnv(chat, key)
	want := map[string]string{"MODEL_ID": "Qwen/Qwen3-4B", "MODEL_SOURCE": "modelscope", "SERVED_MODEL_NAME": "qwen3-4b",
		"MAX_MODEL_LEN": "32768", "API_KEY": key, "TASK": "generate"}
	if !maps.Equal(p, want) {
		t.Fatalf("PlatformNodeEnv = %v", p)
	}
	keys := slices.Sorted(maps.Keys(p))
	if !slices.Equal(keys, slices.Sorted(slices.Values(llm.NodeReservedEnv))) {
		t.Fatalf("platform keys %v != NodeReservedEnv %v", keys, llm.NodeReservedEnv)
	}
	all := llm.NodeEnv(chat, key)
	if all["MAX_NUM_SEQS"] != "128" || all["TASK"] != "generate" || all["MODEL_ID"] != "Qwen/Qwen3-4B" || len(all) != 7 {
		t.Fatalf("NodeEnv = %v", all)
	}
	if chat.NodeEnv["TASK"] != "embed" {
		t.Fatal("NodeEnv mutated the model's node_env")
	}
	emb := llm.Model{Slug: "bge-m3", Kind: llm.KindEmbedding, Source: "hf-mirror", SourceID: "BAAI/bge-m3", ContextLen: 8192}
	if got := llm.NodeEnv(emb, key); got["TASK"] != "embed" || got["MODEL_SOURCE"] != "hf-mirror" || got["MAX_MODEL_LEN"] != "8192" || len(got) != 6 {
		t.Fatalf("embedding NodeEnv = %v", got)
	}
}
