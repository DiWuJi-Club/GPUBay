package llm

import (
	"context"
	"errors"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
)

// ModelInput 是后台创建 / 修改模型的输入（规格 §10.2）。价格单位：分 / 百万 token。
// 零值补默认：Source → modelscope、MaxOutputTokens → min(8192, ContextLen)、DefaultMaxTokens → min(4096, MaxOutputTokens)、MaxConcurrency → 32。
// UpdateModel 忽略 Slug（创建后不可改）。NodeEnv 里值为空串的条目视为「用 spec 默认值」，不保存。
type ModelInput struct {
	Slug, Name, Description, Kind, Source, SourceID string
	ImageID                                         int64
	AppVersionID                                    *int64
	ContextLen, MaxOutputTokens, DefaultMaxTokens   int
	InputPrice, OutputPrice                         int64
	NodeEnv                                         map[string]string
	Replicas, MaxConcurrency, Sort                  int
	Features                                        []string
	// 计划 13（M11）：每副本机器数 ∈ {1, 2, 4}（0 → 1）；TensorParallel × PipelineParallel = NodesPerReplica（0 → TP = N / PP、PP = 1）。
	NodesPerReplica, TensorParallel, PipelineParallel int
}

// NodeReservedEnv 是 vllm-node 中由平台在建节点时填写的变量，node_env 不接受（规格 §3.1）。
// 规格列了五个；TASK 也由平台按 kind 填（embedding → embed），否则 kind 与 TASK 可能矛盾，故一并保留。
// 计划 13：TENSOR_PARALLEL_SIZE / PIPELINE_PARALLEL_SIZE 由模型的 tensor_parallel / pipeline_parallel 决定（只在 N > 1 时填写）。
var NodeReservedEnv = []string{"MODEL_ID", "MODEL_SOURCE", "SERVED_MODEL_NAME", "MAX_MODEL_LEN", "API_KEY", "TASK",
	"TENSOR_PARALLEL_SIZE", "PIPELINE_PARALLEL_SIZE"}

// ModelSources / ModelFeatures 是 source 与展示标签的取值集合。
var (
	ModelSources  = []string{"modelscope", "hf-mirror"}
	ModelFeatures = []string{"tools", "reasoning", "vision", "long-context"}
)

var (
	modelSlugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,63}$`)
	sourceIDRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*$`) // 两段都不能以 . - _ 开头（拒绝 ../x、-x 之类）；与 vllm-node spec 的 MODEL_ID 同一模式
)

const (
	maxModelNameLen         = 64
	maxModelDescLen         = 2000
	maxModelPrice           = 100_000_000 // 分 / 百万 token（¥100 万）：防止 tokens × price 溢出的宽松上限
	defaultMaxOutputTokens  = 8192
	defaultDefaultMaxTokens = 4096
	maxEchoEnvNameBytes     = 64
)

const modelCols = `id, slug, name, description, kind, source, source_id, image_id, app_version_id, context_len, max_output_tokens,
	default_max_tokens, input_price, output_price, node_env, replicas, max_concurrency, features, status, sort, created_at, updated_at,
	nodes_per_replica, tensor_parallel, pipeline_parallel`

func scanModel(row pgx.Row) (Model, error) {
	var m Model
	err := row.Scan(&m.ID, &m.Slug, &m.Name, &m.Description, &m.Kind, &m.Source, &m.SourceID, &m.ImageID, &m.AppVersionID,
		&m.ContextLen, &m.MaxOutputTokens, &m.DefaultMaxTokens, &m.InputPrice, &m.OutputPrice, &m.NodeEnv, &m.Replicas,
		&m.MaxConcurrency, &m.Features, &m.Status, &m.Sort, &m.CreatedAt, &m.UpdatedAt,
		&m.NodesPerReplica, &m.TensorParallel, &m.PipelineParallel)
	if db.IsNoRows(err) {
		return Model{}, apperr.ErrLLMModelNotFound
	}
	if m.NodeEnv == nil {
		m.NodeEnv = map[string]string{}
	}
	if m.Features == nil {
		m.Features = []string{}
	}
	return m, err
}

func collectModels(rows pgx.Rows, err error) ([]Model, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Model, error) { return scanModel(r) })
}

func modelTarget(id int64) string { return strconv.FormatInt(id, 10) }

// validateModelInput 校验与节点应用无关的字段并就地规范化（trim、补默认、features 去重、node_env 去空值）。
// create 为 false（更新）时不看 Slug。vllm-node / 镜像 / node_env 白名单见 checkNodeParams。
func validateModelInput(in *ModelInput, create bool) error {
	in.Slug = strings.TrimSpace(in.Slug)
	in.Name = strings.TrimSpace(in.Name)
	in.Description = strings.TrimSpace(in.Description)
	in.SourceID = strings.TrimSpace(in.SourceID)
	if in.Source == "" {
		in.Source = ModelSources[0]
	}
	if in.MaxOutputTokens == 0 {
		in.MaxOutputTokens = min(defaultMaxOutputTokens, in.ContextLen)
	}
	if in.DefaultMaxTokens == 0 {
		in.DefaultMaxTokens = min(defaultDefaultMaxTokens, in.MaxOutputTokens)
	}
	if in.MaxConcurrency == 0 {
		in.MaxConcurrency = DefaultNodeConcurrency
	}
	if in.NodesPerReplica == 0 {
		in.NodesPerReplica = 1
	}
	if in.PipelineParallel == 0 {
		in.PipelineParallel = 1
	}
	if in.TensorParallel == 0 {
		in.TensorParallel = in.NodesPerReplica
		if in.PipelineParallel > 0 && in.NodesPerReplica%in.PipelineParallel == 0 {
			in.TensorParallel = in.NodesPerReplica / in.PipelineParallel
		}
	}
	features := []string{}
	for _, f := range in.Features {
		f = strings.TrimSpace(f)
		if !slices.Contains(ModelFeatures, f) {
			return apperr.LLMModelInvalid("features", "只能是 "+strings.Join(ModelFeatures, " / "))
		}
		if !slices.Contains(features, f) {
			features = append(features, f)
		}
	}
	in.Features = features
	env := map[string]string{}
	for k, v := range in.NodeEnv {
		if v != "" {
			env[k] = v
		}
	}
	in.NodeEnv = env

	switch {
	case create && !modelSlugRe.MatchString(in.Slug):
		return apperr.LLMModelInvalid("slug", "须为小写字母或数字开头的 2～64 位小写字母、数字、点或连字符")
	case in.Name == "" || utf8.RuneCountInString(in.Name) > maxModelNameLen:
		return apperr.LLMModelInvalid("name", "请填写名称（最多 64 个字符）")
	case utf8.RuneCountInString(in.Description) > maxModelDescLen:
		return apperr.LLMModelInvalid("description", "最多 2000 个字符")
	case in.Kind != KindChat && in.Kind != KindEmbedding:
		return apperr.LLMModelInvalid("kind", "只能是 chat 或 embedding")
	case !slices.Contains(ModelSources, in.Source):
		return apperr.LLMModelInvalid("source", "只能是 modelscope 或 hf-mirror")
	case !sourceIDRe.MatchString(in.SourceID):
		return apperr.LLMModelInvalid("source_id", "格式应为「组织/模型」，如 Qwen/Qwen3-32B")
	case in.ContextLen < 1024 || in.ContextLen > 1048576:
		return apperr.LLMModelInvalid("context_len", "需在 1024～1048576 之间")
	case in.MaxOutputTokens < 1 || in.MaxOutputTokens > 262144:
		return apperr.LLMModelInvalid("max_output_tokens", "需在 1～262144 之间")
	case in.MaxOutputTokens > in.ContextLen:
		return apperr.LLMModelInvalid("max_output_tokens", "不能大于上下文长度")
	case in.DefaultMaxTokens < 1 || in.DefaultMaxTokens > in.MaxOutputTokens:
		return apperr.LLMModelInvalid("default_max_tokens", "需在 1～最大输出 token 之间")
	case in.InputPrice < 0 || in.InputPrice > maxModelPrice:
		return apperr.LLMModelInvalid("input_price", "不能为负数，且不超过 ¥1,000,000 / 百万 token")
	case in.OutputPrice < 0 || in.OutputPrice > maxModelPrice:
		return apperr.LLMModelInvalid("output_price", "不能为负数，且不超过 ¥1,000,000 / 百万 token")
	case in.Replicas < 0 || in.Replicas > 200:
		return apperr.LLMModelInvalid("replicas", "需在 0～200 之间")
	case in.MaxConcurrency < 1 || in.MaxConcurrency > 512:
		return apperr.LLMModelInvalid("max_concurrency", "需在 1～512 之间")
	case in.Sort < -10000 || in.Sort > 10000:
		return apperr.LLMModelInvalid("sort", "需在 -10000～10000 之间")
	case !slices.Contains(NodesPerReplicaOptions, in.NodesPerReplica):
		return apperr.LLMModelInvalid("nodes_per_replica", "每副本机器数只能是 1、2 或 4")
	case in.TensorParallel < 1 || in.TensorParallel > 8 || in.PipelineParallel < 1 || in.PipelineParallel > 8:
		return apperr.LLMModelInvalid("tensor_parallel", "张量并行与流水线并行需在 1～8 之间")
	case in.TensorParallel*in.PipelineParallel != in.NodesPerReplica:
		return apperr.LLMModelInvalid("tensor_parallel", "张量并行 × 流水线并行必须等于每副本机器数")
	case in.Kind == KindEmbedding && in.NodesPerReplica != 1:
		return apperr.LLMModelInvalid("nodes_per_replica", "向量模型只能单机部署")
	}
	if in.ImageID <= 0 {
		return apperr.LLMModelInvalid("image_id", "请选择节点镜像")
	}
	return nil
}

// nodeVersion 取建节点用的 vllm-node 版本：versionID 给定 → 该应用下的已发布版本；nil → 最新已发布版本。
// 应用不存在 / 未上架 / 无已发布版本都给出明确的 llm_model_invalid（运营需先导入并发布 vllm-node）。
func (s *Service) nodeVersion(ctx context.Context, q db.Querier, versionID *int64) (appdeploy.Version, error) {
	app, err := s.d.AppCat.AppBySlug(ctx, NodeAppSlug)
	if errors.Is(err, apperr.ErrNotFound) {
		return appdeploy.Version{}, apperr.LLMModelInvalid("", "内部应用 "+NodeAppSlug+" 尚未导入，请先导入并发布")
	}
	if err != nil {
		return appdeploy.Version{}, err
	}
	if app.Status != appdeploy.StatusPublished {
		return appdeploy.Version{}, apperr.LLMModelInvalid("", "内部应用 "+NodeAppSlug+" 尚未上架，请先发布")
	}
	if versionID != nil {
		v, err := s.d.AppCat.GetVersion(ctx, *versionID)
		if errors.Is(err, apperr.ErrNotFound) || (err == nil && (v.AppID != app.ID || v.Status != appdeploy.StatusPublished)) {
			return appdeploy.Version{}, apperr.LLMModelInvalid("app_version_id", "不是 "+NodeAppSlug+" 的已发布版本")
		}
		return v, err
	}
	v, err := s.d.AppCat.LatestPublished(ctx, q, app.ID)
	if errors.Is(err, apperr.ErrAppUnavailable) {
		return appdeploy.Version{}, apperr.LLMModelInvalid("", "内部应用 "+NodeAppSlug+" 没有已发布的版本，请先发布")
	}
	return v, err
}

// checkNodeParams 校验与节点应用相关的字段（规格 §3.1）：vllm-node 版本可用、镜像 enabled 且在版本兼容列表内、
// node_env 只含 spec 声明且非平台保留的变量、值经 appdeploy.ValidateEnv。in 须已过 validateModelInput。
func (s *Service) checkNodeParams(ctx context.Context, q db.Querier, in ModelInput) error {
	ver, err := s.nodeVersion(ctx, q, in.AppVersionID)
	if err != nil {
		return err
	}
	if _, err := s.d.Images.GetImage(ctx, q, in.ImageID); err != nil {
		if apperr.As(err) != nil {
			return apperr.LLMModelInvalid("image_id", "镜像不存在或已下架")
		}
		return err
	}
	if !slices.Contains(ver.ImageIDs, in.ImageID) {
		return apperr.LLMModelInvalid("image_id", "不在 "+NodeAppSlug+" "+ver.Version+" 的兼容镜像内")
	}
	if n := in.NodesPerReplica; n > 1 {
		if c := ver.Spec.Cluster; c == nil || c.MaxSize < n || c.MinSize > n {
			return apperr.LLMModelInvalid("nodes_per_replica", NodeAppSlug+" "+ver.Version+" 不支持 "+strconv.Itoa(n)+" 台多机部署")
		}
	}
	return validateNodeEnv(ver.Spec, in.NodeEnv)
}

// PlatformNodeEnv 是建节点时平台填写的 vllm-node 变量（NodeReservedEnv 全部）：
// MODEL_ID = source_id、MODEL_SOURCE = source、SERVED_MODEL_NAME = slug、MAX_MODEL_LEN = context_len、API_KEY = 节点密钥、
// TASK = embed（kind = embedding）/ generate。计划 13：只有 NodesPerReplica > 1 时才填 TENSOR_PARALLEL_SIZE / PIPELINE_PARALLEL_SIZE
// （N = 1 的输出与计划 9 逐项相同，存量节点的部署变量不变）。
func PlatformNodeEnv(m Model, apiKey string) map[string]string {
	task := "generate"
	if m.Kind == KindEmbedding {
		task = "embed"
	}
	out := map[string]string{
		"MODEL_ID":          m.SourceID,
		"MODEL_SOURCE":      m.Source,
		"SERVED_MODEL_NAME": m.Slug,
		"MAX_MODEL_LEN":     strconv.Itoa(m.ContextLen),
		"API_KEY":           apiKey,
		"TASK":              task,
	}
	if m.NodesPerReplica > 1 {
		out["TENSOR_PARALLEL_SIZE"] = strconv.Itoa(m.TensorParallel)
		out["PIPELINE_PARALLEL_SIZE"] = strconv.Itoa(m.PipelineParallel)
	}
	return out
}

// NodeEnv 是传给 appdeploy 的完整变量表 = 模型 node_env ∪ PlatformNodeEnv（同名时平台值优先）。
func NodeEnv(m Model, apiKey string) map[string]string {
	out := maps.Clone(m.NodeEnv)
	if out == nil {
		out = map[string]string{}
	}
	maps.Copy(out, PlatformNodeEnv(m, apiKey))
	return out
}

// validateNodeEnv node_env 白名单 = spec 声明的变量 − NodeReservedEnv（敏感变量也不接受：node_env 明文存库、后台可见）。
func validateNodeEnv(spec appdeploy.Spec, env map[string]string) error {
	declared := map[string]appdeploy.EnvSpec{}
	for _, e := range spec.Env {
		declared[e.Name] = e
	}
	for _, name := range slices.Sorted(maps.Keys(env)) {
		echo := name
		if len(echo) > maxEchoEnvNameBytes {
			echo = strings.ToValidUTF8(echo[:maxEchoEnvNameBytes], "") + "…"
		}
		if slices.Contains(NodeReservedEnv, name) {
			return apperr.LLMModelInvalid("node_env", echo+" 由平台填写，不能设置")
		}
		e, ok := declared[name]
		if !ok {
			return apperr.LLMModelInvalid("node_env", echo+" 不是 "+NodeAppSlug+" 声明的变量")
		}
		if e.Type == "secret" {
			return apperr.LLMModelInvalid("node_env", echo+" 是敏感变量，不能在模型参数里设置")
		}
	}
	sub := spec
	sub.Env = slices.DeleteFunc(slices.Clone(spec.Env), func(e appdeploy.EnvSpec) bool { return slices.Contains(NodeReservedEnv, e.Name) })
	if _, _, err := appdeploy.ValidateEnv(sub, env); err != nil {
		if ae := apperr.As(err); ae != nil {
			return apperr.LLMModelInvalid("node_env", ae.Message)
		}
		return err
	}
	return nil
}

// CreateModel 新建 draft 模型；审计 llm.model.create。
func (s *Service) CreateModel(ctx context.Context, actor audit.Actor, in ModelInput) (Model, error) {
	if err := validateModelInput(&in, true); err != nil {
		return Model{}, err
	}
	if err := s.checkNodeParams(ctx, s.d.Pool, in); err != nil {
		return Model{}, err
	}
	var out Model
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		now := s.d.Clock.Now()
		m, err := scanModel(tx.QueryRow(ctx, `INSERT INTO llm_models (slug, name, description, kind, source, source_id, image_id, app_version_id,
			context_len, max_output_tokens, default_max_tokens, input_price, output_price, node_env, replicas, max_concurrency, features,
			status, sort, created_at, updated_at, nodes_per_replica, tensor_parallel, pipeline_parallel)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $20, $21, $22, $23) RETURNING `+modelCols,
			in.Slug, in.Name, in.Description, in.Kind, in.Source, in.SourceID, in.ImageID, in.AppVersionID,
			in.ContextLen, in.MaxOutputTokens, in.DefaultMaxTokens, in.InputPrice, in.OutputPrice, in.NodeEnv, in.Replicas, in.MaxConcurrency,
			in.Features, ModelDraft, in.Sort, now, in.NodesPerReplica, in.TensorParallel, in.PipelineParallel))
		if db.IsUniqueViolation(err) {
			return apperr.ErrLLMModelSlugExists
		}
		if err != nil {
			return err
		}
		out = m
		return audit.Record(ctx, tx, now, actor.Entry("llm.model.create", "llm_model", modelTarget(m.ID), m))
	})
	return out, err
}

// UpdateModel 修改除 slug / status 外的全部字段；审计 llm.model.update{before, after}（改价在 diff 里）。
// 节点参数（image_id / app_version_id / node_env）只在变化时按 vllm-node 当前版本复核——改价不因目录变化被卡住；
// 这些参数只影响之后新建的节点（规格 §3.1）。online 模型要求 replicas ≥ 1（要停止服务请下线）。
func (s *Service) UpdateModel(ctx context.Context, actor audit.Actor, id int64, in ModelInput) (Model, error) {
	if err := validateModelInput(&in, false); err != nil {
		return Model{}, err
	}
	var out Model
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		before, err := scanModel(tx.QueryRow(ctx, `SELECT `+modelCols+` FROM llm_models WHERE id = $1 FOR UPDATE`, id))
		if err != nil {
			return err
		}
		if before.Status != ModelDraft && in.Kind != before.Kind {
			return apperr.LLMModelInvalid("kind", "只有草稿模型可以修改类型")
		}
		if before.Status == ModelOnline && in.Replicas < 1 {
			return apperr.LLMModelInvalid("replicas", "在线模型至少 1 个副本（要停止服务请下线）")
		}
		if in.NodesPerReplica != before.NodesPerReplica {
			var live bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM llm_nodes WHERE model_id = $1 AND state <> ALL($2))`,
				id, NodeTerminalStates).Scan(&live); err != nil {
				return err
			}
			if live {
				return apperr.LLMModelInvalid("nodes_per_replica", "请先把副本数调为 0 并等节点全部释放，再修改每副本机器数")
			}
		}
		nodeChanged := before.ImageID != in.ImageID || !equalInt64Ptr(before.AppVersionID, in.AppVersionID) || !maps.Equal(before.NodeEnv, in.NodeEnv) ||
			before.NodesPerReplica != in.NodesPerReplica
		if nodeChanged {
			if err := s.checkNodeParams(ctx, tx, in); err != nil {
				return err
			}
		}
		now := s.d.Clock.Now()
		m, err := scanModel(tx.QueryRow(ctx, `UPDATE llm_models SET name = $2, description = $3, kind = $4, source = $5, source_id = $6,
			image_id = $7, app_version_id = $8, context_len = $9, max_output_tokens = $10, default_max_tokens = $11, input_price = $12,
			output_price = $13, node_env = $14, replicas = $15, max_concurrency = $16, features = $17, sort = $18, updated_at = $19,
			nodes_per_replica = $20, tensor_parallel = $21, pipeline_parallel = $22
			WHERE id = $1 RETURNING `+modelCols,
			id, in.Name, in.Description, in.Kind, in.Source, in.SourceID, in.ImageID, in.AppVersionID, in.ContextLen, in.MaxOutputTokens,
			in.DefaultMaxTokens, in.InputPrice, in.OutputPrice, in.NodeEnv, in.Replicas, in.MaxConcurrency, in.Features, in.Sort, now,
			in.NodesPerReplica, in.TensorParallel, in.PipelineParallel))
		if err != nil {
			return err
		}
		out = m
		return audit.Record(ctx, tx, now, actor.Entry("llm.model.update", "llm_model", modelTarget(id), map[string]any{"before": before, "after": m}))
	})
	return out, err
}

func equalInt64Ptr(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// SetModelStatus 状态机（规格 §3.1 / T7）：draft → online、online → offline、offline → online；不能回到 draft。
// online 要求 replicas ≥ 1。同状态幂等（不写审计）。审计 llm.model.status{from, to}。
func (s *Service) SetModelStatus(ctx context.Context, actor audit.Actor, id int64, status string) (Model, error) {
	if status != ModelDraft && status != ModelOnline && status != ModelOffline {
		return Model{}, apperr.LLMModelInvalid("status", "只能是 online 或 offline")
	}
	var out Model
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		cur, err := scanModel(tx.QueryRow(ctx, `SELECT `+modelCols+` FROM llm_models WHERE id = $1 FOR UPDATE`, id))
		if err != nil {
			return err
		}
		if cur.Status == status {
			out = cur
			return nil
		}
		switch {
		case status == ModelOnline: // draft / offline → online
			if cur.Replicas < 1 {
				return apperr.LLMModelInvalid("", "上线前请设置至少 1 个副本")
			}
		case status == ModelOffline && cur.Status == ModelOnline:
		default:
			return apperr.ErrInvalidState
		}
		now := s.d.Clock.Now()
		m, err := scanModel(tx.QueryRow(ctx, `UPDATE llm_models SET status = $2, updated_at = $3 WHERE id = $1 RETURNING `+modelCols, id, status, now))
		if err != nil {
			return err
		}
		out = m
		return audit.Record(ctx, tx, now, actor.Entry("llm.model.status", "llm_model", modelTarget(id),
			map[string]any{"slug": m.Slug, "from": cur.Status, "to": status}))
	})
	return out, err
}

// DeleteModel 只删没有任何节点（含终态）与用量（明细 / 日汇总）的 draft 模型；否则 llm_model_in_use。审计 llm.model.delete。
func (s *Service) DeleteModel(ctx context.Context, actor audit.Actor, id int64) error {
	return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		var slug, status string
		var used bool
		err := tx.QueryRow(ctx, `SELECT slug, status,
			EXISTS (SELECT 1 FROM llm_nodes WHERE model_id = $1) OR EXISTS (SELECT 1 FROM llm_usage WHERE model_id = $1)
				OR EXISTS (SELECT 1 FROM llm_usage_daily WHERE model_id = $1)
			FROM llm_models WHERE id = $1 FOR UPDATE`, id).Scan(&slug, &status, &used)
		if db.IsNoRows(err) {
			return apperr.ErrLLMModelNotFound
		}
		if err != nil {
			return err
		}
		if status != ModelDraft || used {
			return apperr.ErrLLMModelInUse
		}
		if _, err := tx.Exec(ctx, `DELETE FROM llm_models WHERE id = $1`, id); err != nil {
			if db.IsForeignKeyViolation(err) { // 与并发插入的引用竞争：外键兜底
				return apperr.ErrLLMModelInUse
			}
			return err
		}
		return audit.Record(ctx, tx, s.d.Clock.Now(), actor.Entry("llm.model.delete", "llm_model", modelTarget(id), map[string]any{"slug": slug}))
	})
}

func (s *Service) GetModel(ctx context.Context, id int64) (Model, error) {
	return scanModel(s.d.Pool.QueryRow(ctx, `SELECT `+modelCols+` FROM llm_models WHERE id = $1`, id))
}

// ModelBySlug 按 slug 取模型（任何状态；调用方自己判断 online）。不存在 → llm_model_not_found。
func (s *Service) ModelBySlug(ctx context.Context, q db.Querier, slug string) (Model, error) {
	return scanModel(q.QueryRow(ctx, `SELECT `+modelCols+` FROM llm_models WHERE slug = $1`, slug))
}

// ListModels 后台：全部模型，按 sort, id。
func (s *Service) ListModels(ctx context.Context) ([]Model, error) {
	return collectModels(s.d.Pool.Query(ctx, `SELECT `+modelCols+` FROM llm_models ORDER BY sort, id`))
}

// ListOnlineModels 控制台 / 网关：只有 online，按 sort, id。
func (s *Service) ListOnlineModels(ctx context.Context) ([]Model, error) {
	return collectModels(s.d.Pool.Query(ctx, `SELECT `+modelCols+` FROM llm_models WHERE status = $1 ORDER BY sort, id`, ModelOnline))
}
