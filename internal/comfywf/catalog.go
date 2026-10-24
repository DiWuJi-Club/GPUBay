package comfywf

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/storage"
)

const (
	MaxChangelogLen = 1000
	maxNameLen      = 48
	maxSummaryLen   = 120
	maxDescLen      = 8000
	maxTags         = 8
	maxTagLen       = 16
	maxSort         = 10000
)

var wfStatuses = []string{StatusDraft, StatusPublished, StatusRetired}

// Catalog 维护工作流目录：工作流（展示信息 + 封面）与版本（草稿 → 发布 → 下线，发布后不可变）。
type Catalog struct {
	pool    *pgxpool.Pool
	clock   clock.Clock
	uploads *storage.Service
	log     *slog.Logger
}

func NewCatalog(pool *pgxpool.Pool, clk clock.Clock, uploads *storage.Service, log *slog.Logger) *Catalog {
	return &Catalog{pool: pool, clock: clk, uploads: uploads, log: log}
}

const wfCols = "id, slug, name, summary, description, category, tags, cover_key, sort, status, created_at, updated_at"

// verCols 是版本列（a 为表别名，可空）；download_gb 以文本读出（NUMERIC 原文，不经 float）。
func verCols(a string) string {
	if a != "" {
		a += "."
	}
	return fmt.Sprintf("%[1]sid, %[1]sworkflow_id, %[1]sversion, %[1]sstatus, %[1]sgraph, %[1]sapi_prompt, %[1]smanifest, "+
		"%[1]sdownload_gb::text, %[1]smanifest_hash, %[1]schangelog, %[1]spublished_at, %[1]screated_at, %[1]supdated_at", a)
}

// liveInstalls 是某工作流（外层别名 w）的「在用安装数」：deployment_workflows 里 state = installed、
// 且所属部署未终结（不是 removed / lost）的行数。市场页 installs、后台 installs_live、版本表安装数都用这个口径
// （规格 §10.1；pending / failed 的不算，已卸载 / 失联的部署的历史行也不算）。
const liveInstallsCond = `dw.state = 'installed' AND d.state NOT IN ('removed', 'lost')`

func scanWorkflow(row pgx.Row, extra ...any) (Workflow, error) {
	var w Workflow
	dest := append([]any{&w.ID, &w.Slug, &w.Name, &w.Summary, &w.Description, &w.Category, &w.Tags, &w.CoverKey, &w.Sort, &w.Status,
		&w.CreatedAt, &w.UpdatedAt}, extra...)
	err := row.Scan(dest...)
	if db.IsNoRows(err) {
		return Workflow{}, apperr.ErrNotFound
	}
	if w.Tags == nil {
		w.Tags = []string{}
	}
	w.HasCover = w.CoverKey != nil
	return w, err
}

// versionDest 返回 scanVersion 的扫描目标与扫描后的收尾（解析 JSON 列）。
func versionDest(v *Version) ([]any, func() error) {
	var graph, api, manifest []byte
	dest := []any{&v.ID, &v.WorkflowID, &v.Version, &v.Status, &graph, &api, &manifest, &v.DownloadGB, &v.ManifestHash, &v.Changelog,
		&v.PublishedAt, &v.CreatedAt, &v.UpdatedAt}
	return dest, func() error {
		// JSONB 读出的文本带空格（"a": 1, "b": 2）：压缩回紧凑形式，大小上限的复查与部署包里的文件都按紧凑形式算
		v.Graph = compactJSON(graph)
		if api != nil {
			v.APIPrompt = compactJSON(api)
		}
		if err := decodeNumber(manifest, &v.Manifest); err != nil {
			return fmt.Errorf("comfy_workflow_versions %d manifest: %w", v.ID, err)
		}
		return nil
	}
}

// compactJSON 去掉 JSON 里的无意义空白；不是合法 JSON 时原样返回（交给校验报错）。
func compactJSON(raw []byte) json.RawMessage {
	if raw == nil {
		return nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return json.RawMessage(raw)
	}
	return json.RawMessage(buf.Bytes())
}

func scanVersion(row pgx.Row) (Version, error) {
	var v Version
	dest, finish := versionDest(&v)
	err := row.Scan(dest...)
	if db.IsNoRows(err) {
		return Version{}, apperr.ErrNotFound
	}
	if err != nil {
		return Version{}, err
	}
	return v, finish()
}

func target(id int64) string { return strconv.FormatInt(id, 10) }

func runeLen(s string) int { return utf8.RuneCountInString(s) }

// ---- 工作流 ----

type WorkflowInput struct {
	Slug, Name, Summary, Description, Category string
	Tags                                       []string
	Sort                                       int
	Status                                     string // 空 = draft
}

// normalizeWorkflow 按规格 §2 的列注释校验并规范化（去空白、标签去空去重）；checkSlug 为 false 时（更新）忽略 Slug。
func normalizeWorkflow(in WorkflowInput, checkSlug bool) (WorkflowInput, error) {
	in.Slug = strings.TrimSpace(in.Slug)
	in.Name = strings.TrimSpace(in.Name)
	in.Summary = strings.TrimSpace(in.Summary)
	in.Description = strings.TrimSpace(in.Description)
	in.Category = strings.TrimSpace(in.Category)
	if in.Status == "" {
		in.Status = StatusDraft
	}
	tags := make([]string, 0, len(in.Tags))
	for _, t := range in.Tags {
		t = strings.TrimSpace(t)
		if t == "" || slices.Contains(tags, t) {
			continue
		}
		if runeLen(t) > maxTagLen {
			return in, apperr.BadRequest(fmt.Sprintf("标签「%s」超过 %d 个字符", t, maxTagLen))
		}
		tags = append(tags, t)
	}
	in.Tags = tags
	switch {
	case checkSlug && !ValidSlug(in.Slug):
		return in, apperr.BadRequest("slug 只能是小写字母开头的 2～48 位小写字母、数字或连字符")
	case in.Name == "" || runeLen(in.Name) > maxNameLen:
		return in, apperr.BadRequest(fmt.Sprintf("请填写工作流名称（最多 %d 个字符）", maxNameLen))
	case runeLen(in.Summary) > maxSummaryLen:
		return in, apperr.BadRequest(fmt.Sprintf("简介最多 %d 个字符", maxSummaryLen))
	case runeLen(in.Description) > maxDescLen:
		return in, apperr.BadRequest(fmt.Sprintf("说明最多 %d 个字符", maxDescLen))
	case !ValidCategory(in.Category):
		return in, apperr.BadRequest("分类只能是 " + strings.Join(Categories, "、") + " 之一")
	case len(in.Tags) > maxTags:
		return in, apperr.BadRequest(fmt.Sprintf("标签最多 %d 个", maxTags))
	case in.Sort < -maxSort || in.Sort > maxSort:
		return in, apperr.BadRequest("排序值需在 -10000～10000 之间")
	case !slices.Contains(wfStatuses, in.Status):
		return in, apperr.BadRequest("状态只能是 draft、published 或 retired")
	}
	return in, nil
}

var (
	errSlugExists    = apperr.New(apperr.ErrDuplicate.Code, "slug 已存在", apperr.ErrDuplicate.Status)
	errVersionExists = apperr.New(apperr.ErrDuplicate.Code, "版本号已存在", apperr.ErrDuplicate.Status)
)

func (c *Catalog) CreateWorkflow(ctx context.Context, actor audit.Actor, in WorkflowInput) (Workflow, error) {
	in, err := normalizeWorkflow(in, true)
	if err != nil {
		return Workflow{}, err
	}
	var out Workflow
	err = db.WithTx(ctx, c.pool, func(tx pgx.Tx) error {
		out, err = c.insertWorkflow(ctx, tx, actor, in, nil)
		return err
	})
	return out, err
}

// insertWorkflow 写入已规范化的工作流（coverKey 可为 nil）并记审计 workflow.create。
func (c *Catalog) insertWorkflow(ctx context.Context, tx pgx.Tx, actor audit.Actor, in WorkflowInput, coverKey *string) (Workflow, error) {
	now := c.clock.Now()
	w, err := scanWorkflow(tx.QueryRow(ctx, `INSERT INTO comfy_workflows (slug, name, summary, description, category, tags, cover_key, sort, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $10) RETURNING `+wfCols,
		in.Slug, in.Name, in.Summary, in.Description, in.Category, in.Tags, coverKey, in.Sort, in.Status, now))
	if db.IsUniqueViolation(err) {
		return Workflow{}, errSlugExists
	}
	if err != nil {
		return Workflow{}, err
	}
	return w, audit.Record(ctx, tx, now, actor.Entry("workflow.create", "workflow", target(w.ID), w))
}

// UpdateWorkflow 修改展示信息、排序与状态；slug 创建后不可改（in.Slug 被忽略）。
func (c *Catalog) UpdateWorkflow(ctx context.Context, actor audit.Actor, id int64, in WorkflowInput) (Workflow, error) {
	in, err := normalizeWorkflow(in, false)
	if err != nil {
		return Workflow{}, err
	}
	var out Workflow
	err = db.WithTx(ctx, c.pool, func(tx pgx.Tx) error {
		before, err := scanWorkflow(tx.QueryRow(ctx, `SELECT `+wfCols+` FROM comfy_workflows WHERE id = $1 FOR UPDATE`, id))
		if err != nil {
			return err
		}
		now := c.clock.Now()
		w, err := scanWorkflow(tx.QueryRow(ctx, `UPDATE comfy_workflows SET name = $2, summary = $3, description = $4, category = $5, tags = $6,
			sort = $7, status = $8, updated_at = $9 WHERE id = $1 RETURNING `+wfCols,
			id, in.Name, in.Summary, in.Description, in.Category, in.Tags, in.Sort, in.Status, now))
		if err != nil {
			return err
		}
		out = w
		return audit.Record(ctx, tx, now, actor.Entry("workflow.update", "workflow", target(id), map[string]any{"before": before, "after": w}))
	})
	return out, err
}

// DeleteWorkflow 只能删除没有任何版本的草稿工作流（其余用 retired 下线）；有封面时一并删除封面文件（失败只记日志）。
func (c *Catalog) DeleteWorkflow(ctx context.Context, actor audit.Actor, id int64) error {
	var cover *string
	err := db.WithTx(ctx, c.pool, func(tx pgx.Tx) error {
		var status, slug string
		var versions int
		err := tx.QueryRow(ctx, `SELECT status, slug, cover_key, (SELECT count(*) FROM comfy_workflow_versions WHERE workflow_id = $1)
			FROM comfy_workflows WHERE id = $1 FOR UPDATE`, id).Scan(&status, &slug, &cover, &versions)
		if db.IsNoRows(err) {
			return apperr.ErrNotFound
		}
		if err != nil {
			return err
		}
		if status != StatusDraft || versions > 0 {
			return apperr.ErrInvalidState
		}
		if _, err := tx.Exec(ctx, `DELETE FROM comfy_workflows WHERE id = $1`, id); err != nil {
			return err
		}
		return audit.Record(ctx, tx, c.clock.Now(), actor.Entry("workflow.delete", "workflow", target(id), map[string]any{"slug": slug}))
	})
	if err == nil && cover != nil {
		c.deleteUpload(ctx, *cover)
	}
	return err
}

// GetWorkflow 按 id 读工作流（任意状态）；不存在 → not_found。
func (c *Catalog) GetWorkflow(ctx context.Context, id int64) (Workflow, error) {
	return scanWorkflow(c.pool.QueryRow(ctx, `SELECT `+wfCols+` FROM comfy_workflows WHERE id = $1`, id))
}

// WorkflowBySlug 按 slug 读工作流（任意状态）；不存在 → not_found。
func (c *Catalog) WorkflowBySlug(ctx context.Context, slug string) (Workflow, error) {
	return scanWorkflow(c.pool.QueryRow(ctx, `SELECT `+wfCols+` FROM comfy_workflows WHERE slug = $1`, slug))
}

// WorkflowRow 是后台工作流列表的一行：版本数、在用安装数（口径见 liveInstallsCond）、近 7 日快速出图次数。
type WorkflowRow struct {
	Workflow
	Versions     int `json:"versions"`
	InstallsLive int `json:"installs_live"`
	Runs7d       int `json:"runs_7d"`
}

// ListAdmin 返回全部工作流（含 draft / retired），按 sort, id 排序。
func (c *Catalog) ListAdmin(ctx context.Context) ([]WorkflowRow, error) {
	rows, err := c.pool.Query(ctx, `SELECT `+qualify(wfCols, "w")+`,
		(SELECT count(*) FROM comfy_workflow_versions v WHERE v.workflow_id = w.id),
		(SELECT count(*) FROM deployment_workflows dw JOIN app_deployments d ON d.id = dw.deployment_id
			WHERE dw.workflow_id = w.id AND `+liveInstallsCond+`),
		(SELECT count(*) FROM comfy_workflow_runs r JOIN comfy_workflow_versions v ON v.id = r.workflow_version_id
			WHERE v.workflow_id = w.id AND r.created_at > $1)
		FROM comfy_workflows w ORDER BY w.sort, w.id`, c.clock.Now().Add(-7*24*time.Hour))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (WorkflowRow, error) {
		var row WorkflowRow
		var err error
		row.Workflow, err = scanWorkflow(r, &row.Versions, &row.InstallsLive, &row.Runs7d)
		return row, err
	})
}

// qualify 给列清单加表别名前缀。
func qualify(cols, alias string) string {
	return alias + "." + strings.ReplaceAll(cols, ", ", ", "+alias+".")
}

// ---- 用户侧目录 ----

// VersionSummary 是市场页卡片上的当前版本摘要（前端 WorkflowVersionSummary）。
type VersionSummary struct {
	ID         int64   `json:"id"`
	Version    string  `json:"version"`
	DownloadGB float64 `json:"download_gb"`
	Models     int     `json:"models"`
	Nodes      int     `json:"nodes"`
	QuickRun   bool    `json:"quick_run"`
	ComfyUIMin string  `json:"comfyui_min"`
}

// Summarize 从版本生成摘要。
func Summarize(v Version) VersionSummary {
	return VersionSummary{ID: v.ID, Version: v.Version, DownloadGB: v.DownloadGBValue(), Models: len(v.Manifest.Models),
		Nodes: len(v.Manifest.Nodes), QuickRun: len(v.APIPrompt) > 0, ComfyUIMin: v.Manifest.ComfyUIMin}
}

// CatalogBase 是市场条目与详情共有的工作流字段（不含 cover_key：只给 cover_url）。
type CatalogBase struct {
	ID       int64    `json:"id"`
	Slug     string   `json:"slug"`
	Name     string   `json:"name"`
	Summary  string   `json:"summary"`
	Category string   `json:"category"`
	Tags     []string `json:"tags"`
	CoverURL *string  `json:"cover_url"`
	Sort     int      `json:"sort"`
	Installs int      `json:"installs"`
}

func catalogBase(w Workflow, installs int) CatalogBase {
	return CatalogBase{ID: w.ID, Slug: w.Slug, Name: w.Name, Summary: w.Summary, Category: w.Category, Tags: w.Tags,
		CoverURL: CoverURL(w), Sort: w.Sort, Installs: installs}
}

// CatalogItem 是 GET /api/v1/workflows 的一项（前端 WorkflowCatalogItem）。
type CatalogItem struct {
	CatalogBase
	Version VersionSummary `json:"version"`
}

// ListPublished 返回已上架（status = published 且有已发布版本）的工作流及其当前版本（published_at 最新）摘要与在用安装数，
// 按 sort, id 排序；category 为空 = 全部，否则须是固定分类之一。
func (c *Catalog) ListPublished(ctx context.Context, category string) ([]CatalogItem, error) {
	if category != "" && !ValidCategory(category) {
		return nil, apperr.BadRequest("分类无效")
	}
	rows, err := c.pool.Query(ctx, `SELECT DISTINCT ON (w.id) `+qualify(wfCols, "w")+`, `+verCols("v")+`,
		(SELECT count(*) FROM deployment_workflows dw JOIN app_deployments d ON d.id = dw.deployment_id
			WHERE dw.workflow_id = w.id AND `+liveInstallsCond+`)
		FROM comfy_workflows w JOIN comfy_workflow_versions v ON v.workflow_id = w.id AND v.status = 'published'
		WHERE w.status = 'published' AND ($1 = '' OR w.category = $1)
		ORDER BY w.id, v.published_at DESC, v.id DESC`, category)
	if err != nil {
		return nil, err
	}
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (CatalogItem, error) {
		var v Version
		var installs int
		dest, finish := versionDest(&v)
		w, err := scanWorkflow(r, append(dest, &installs)...)
		if err != nil {
			return CatalogItem{}, err
		}
		if err := finish(); err != nil {
			return CatalogItem{}, err
		}
		return CatalogItem{CatalogBase: catalogBase(w, installs), Version: Summarize(v)}, nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortStableFunc(items, func(x, y CatalogItem) int {
		if x.Sort != y.Sort {
			return x.Sort - y.Sort
		}
		return int(x.ID - y.ID)
	})
	return items, nil
}

// DetailVersion 是详情页的当前版本：摘要 + 清单（Manifest 本身不含 api_prompt）+ 更新说明。
type DetailVersion struct {
	VersionSummary
	Manifest    Manifest   `json:"manifest"`
	Changelog   string     `json:"changelog"`
	PublishedAt *time.Time `json:"published_at"`
}

// VersionBrief 是详情页的已发布版本列表项。
type VersionBrief struct {
	ID          int64      `json:"id"`
	Version     string     `json:"version"`
	PublishedAt *time.Time `json:"published_at"`
}

// Detail 是 GET /api/v1/workflows/{slug}（前端 WorkflowDetail）。
type Detail struct {
	CatalogBase
	Description string         `json:"description"`
	Version     DetailVersion  `json:"version"`
	Versions    []VersionBrief `json:"versions"`
}

// DetailBySlug 单个已上架工作流（与 ListPublished 同一口径）；不存在、未上架或没有已发布版本 → workflow_not_found。
func (c *Catalog) DetailBySlug(ctx context.Context, slug string) (Detail, error) {
	w, err := c.WorkflowBySlug(ctx, slug)
	if errors.Is(err, apperr.ErrNotFound) || (err == nil && w.Status != StatusPublished) {
		return Detail{}, apperr.ErrWorkflowNotFound
	}
	if err != nil {
		return Detail{}, err
	}
	v, err := c.LatestPublished(ctx, c.pool, w.ID)
	if errors.Is(err, apperr.ErrWorkflowUnavailable) {
		return Detail{}, apperr.ErrWorkflowNotFound
	}
	if err != nil {
		return Detail{}, err
	}
	installs, err := c.liveInstalls(ctx, w.ID)
	if err != nil {
		return Detail{}, err
	}
	rows, err := c.pool.Query(ctx, `SELECT id, version, published_at FROM comfy_workflow_versions
		WHERE workflow_id = $1 AND status = 'published' ORDER BY published_at DESC, id DESC`, w.ID)
	if err != nil {
		return Detail{}, err
	}
	briefs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (VersionBrief, error) {
		var b VersionBrief
		err := r.Scan(&b.ID, &b.Version, &b.PublishedAt)
		return b, err
	})
	if err != nil {
		return Detail{}, err
	}
	return Detail{CatalogBase: catalogBase(w, installs), Description: w.Description,
		Version:  DetailVersion{VersionSummary: Summarize(v), Manifest: v.Manifest, Changelog: v.Changelog, PublishedAt: v.PublishedAt},
		Versions: briefs}, nil
}

func (c *Catalog) liveInstalls(ctx context.Context, wfID int64) (int, error) {
	var n int
	err := c.pool.QueryRow(ctx, `SELECT count(*) FROM deployment_workflows dw JOIN app_deployments d ON d.id = dw.deployment_id
		WHERE dw.workflow_id = $1 AND `+liveInstallsCond, wfID).Scan(&n)
	return n, err
}

// ---- 版本 ----

// VersionInput 是建 / 改版本的输入。
//   - CopyFrom 只用于建版本：以本工作流的该版本为基底，Graph / APIPrompt 为空、Manifest 为零值时取基底的（APIPrompt 显式传 "null"
//     = 不要快速出图）；Changelog 不复制。
//   - Status 只用于修改已发布版本（空 = 不改，"retired" = 下线）；草稿的 Status 只能为空或 draft（发布走 PublishVersion）。
//   - 修改已发布版本时，Version / Graph / APIPrompt / Manifest 为空或零值 = 不改；给了就必须与当前内容相同（规范化后比较），
//     否则 workflow_version_frozen。Changelog 总是写入。
type VersionInput struct {
	Version   string
	Graph     json.RawMessage
	APIPrompt json.RawMessage
	Manifest  Manifest
	Changelog string
	CopyFrom  *int64
	Status    string
}

// ValidateResult 是版本校验结果（后台 ?validate=1 返回 {ok, manifest_hash, download_gb, warnings}）。
type ValidateResult struct {
	ManifestHash string   `json:"manifest_hash"`
	DownloadGB   float64  `json:"download_gb"`
	Warnings     []string `json:"warnings"`
}

func manifestIsZero(m Manifest) bool {
	return m.ComfyUIMin == "" && len(m.Models) == 0 && len(m.Nodes) == 0 && len(m.Inputs) == 0 && len(m.Outputs) == 0
}

// cloneManifest 深拷贝（ValidateManifest 会就地规范化，不能改到调用方的切片 / 指针）。
func cloneManifest(m Manifest) Manifest {
	b, err := json.Marshal(m)
	if err != nil {
		return m
	}
	var out Manifest
	if err := decodeNumber(b, &out); err != nil {
		return m
	}
	return out
}

// nullJSON 报告 raw 是否为空或 JSON null（api_prompt 的「无」）。
func nullJSON(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) == 0 || string(t) == "null"
}

func apiOrNil(raw json.RawMessage) json.RawMessage {
	if nullJSON(raw) {
		return nil
	}
	return raw
}

// validateVersionInput 完整校验版本（草稿保存也要过，规格 §3.3），就地规范化 in（Version / Changelog 去空白、Manifest 规范化、
// Graph / APIPrompt 压缩空白、APIPrompt 的 null → nil），返回将写入的 manifest_hash / download_gb 与提示。
func validateVersionInput(in *VersionInput) (ValidateResult, error) {
	in.Version = strings.TrimSpace(in.Version)
	in.Changelog = strings.TrimSpace(in.Changelog)
	in.Graph = compactJSON(in.Graph) // ComfyUI 导出的文件常带缩进：按紧凑形式计大小
	in.APIPrompt = compactJSON(apiOrNil(in.APIPrompt))
	if !ValidVersion(in.Version) {
		return ValidateResult{}, invalid("version", "只能由字母、数字、点和连字符组成（最多 32 个字符），且以字母或数字开头")
	}
	if len(bytes.TrimSpace(in.Graph)) == 0 {
		return ValidateResult{}, invalid("graph", "不能为空")
	}
	if runeLen(in.Changelog) > MaxChangelogLen {
		return ValidateResult{}, invalid("changelog", fmt.Sprintf("最多 %d 个字符", MaxChangelogLen))
	}
	m := cloneManifest(in.Manifest)
	if err := ValidateManifest(&m, in.Graph, in.APIPrompt); err != nil {
		return ValidateResult{}, err
	}
	in.Manifest = m
	return resultFor(in.Graph, in.APIPrompt, m), nil
}

func resultFor(graph, api json.RawMessage, m Manifest) ValidateResult {
	return ValidateResult{ManifestHash: ManifestHash(graph, api, m), DownloadGB: DownloadGB(m), Warnings: warnings(m, api)}
}

// warnings 是不阻止保存 / 发布的提示。
func warnings(m Manifest, api json.RawMessage) []string {
	out := []string{}
	if nullJSON(api) {
		out = append(out, "未提供 api_prompt：该版本不支持快速出图")
	}
	for i, md := range m.Models {
		p := fmt.Sprintf("manifest.models[%d]（%s）", i, md.Name)
		if md.SHA256 == "" {
			out = append(out, p+"未填 sha256：下载后不做校验")
		}
		if md.ModelScope == nil || md.HF == nil {
			out = append(out, p+"只有一个下载来源")
		}
	}
	if len(m.Nodes) > 0 {
		out = append(out, fmt.Sprintf("含 %d 个自定义节点：节点代码会在用户容器内以 root 执行，发布前请确认已审查钉住的 commit", len(m.Nodes)))
	}
	return out
}

// withCopy 按 in.CopyFrom（须属于 wfID）合成建版本的输入。
func (c *Catalog) withCopy(ctx context.Context, wfID int64, in VersionInput) (VersionInput, error) {
	if in.CopyFrom == nil {
		return in, nil
	}
	base, err := c.version(ctx, c.pool, wfID, *in.CopyFrom, false)
	if err != nil {
		return in, err
	}
	out := in
	if len(bytes.TrimSpace(in.Graph)) == 0 {
		out.Graph = base.Graph
	}
	if len(bytes.TrimSpace(in.APIPrompt)) == 0 {
		out.APIPrompt = base.APIPrompt
	}
	if manifestIsZero(in.Manifest) {
		out.Manifest = cloneManifest(base.Manifest)
	}
	return out, nil
}

// CreateVersion 建草稿版本（完整校验；CopyFrom 见 VersionInput）；工作流不存在 → not_found，版本号重复 → duplicate。
func (c *Catalog) CreateVersion(ctx context.Context, actor audit.Actor, wfID int64, in VersionInput) (Version, error) {
	in, err := c.withCopy(ctx, wfID, in)
	if err != nil {
		return Version{}, err
	}
	res, err := validateVersionInput(&in)
	if err != nil {
		return Version{}, err
	}
	var out Version
	err = db.WithTx(ctx, c.pool, func(tx pgx.Tx) error {
		out, err = c.insertVersion(ctx, tx, actor, wfID, in, res)
		return err
	})
	return out, err
}

// insertVersion 写入已校验过的草稿版本（res 来自 validateVersionInput）并记审计 workflow.version.create。
func (c *Catalog) insertVersion(ctx context.Context, tx pgx.Tx, actor audit.Actor, wfID int64, in VersionInput, res ValidateResult) (Version, error) {
	manifest, err := json.Marshal(in.Manifest)
	if err != nil {
		return Version{}, err
	}
	now := c.clock.Now()
	v, err := scanVersion(tx.QueryRow(ctx, `INSERT INTO comfy_workflow_versions
		(workflow_id, version, status, graph, api_prompt, manifest, download_gb, manifest_hash, changelog, created_at, updated_at)
		VALUES ($1, $2, 'draft', $3, $4, $5, $6, $7, $8, $9, $9) RETURNING `+verCols(""),
		wfID, in.Version, []byte(in.Graph), jsonArg(in.APIPrompt), manifest, res.DownloadGB, res.ManifestHash, in.Changelog, now))
	switch {
	case db.IsForeignKeyViolation(err):
		return Version{}, apperr.ErrNotFound
	case db.IsUniqueViolation(err):
		return Version{}, errVersionExists
	case err != nil:
		return Version{}, err
	}
	return v, audit.Record(ctx, tx, now, actor.Entry("workflow.version.create", "workflow_version", target(v.ID), map[string]any{
		"workflow_id": wfID, "version": v.Version, "manifest_hash": v.ManifestHash, "copy_from": in.CopyFrom,
	}))
}

// jsonArg 把可空的 JSON 转成 SQL 参数（nil → NULL）。
func jsonArg(raw json.RawMessage) any {
	if raw == nil {
		return nil
	}
	return []byte(raw)
}

// ValidateVersion 只校验不保存（后台 POST …/versions?validate=1），规则同 CreateVersion。
func (c *Catalog) ValidateVersion(ctx context.Context, wfID int64, in VersionInput) (ValidateResult, error) {
	if _, err := c.GetWorkflow(ctx, wfID); err != nil {
		return ValidateResult{}, err
	}
	in, err := c.withCopy(ctx, wfID, in)
	if err != nil {
		return ValidateResult{}, err
	}
	return validateVersionInput(&in)
}

// UpdateVersion：草稿整体可改（完整校验）；已发布版本只能改 Changelog 或下线（Status = "retired"），见 VersionInput 与 publishedEdit；
// 已下线版本 → workflow_version_frozen。
func (c *Catalog) UpdateVersion(ctx context.Context, actor audit.Actor, wfID, vid int64, in VersionInput) (Version, error) {
	pre, err := c.version(ctx, c.pool, wfID, vid, false)
	if err != nil {
		return Version{}, err
	}
	var res ValidateResult
	if pre.Status == StatusDraft {
		if in.Status != "" && in.Status != StatusDraft {
			return Version{}, apperr.ErrInvalidState // 发布走 PublishVersion（需动态码）
		}
		if res, err = validateVersionInput(&in); err != nil {
			return Version{}, err
		}
	}
	var out Version
	err = db.WithTx(ctx, c.pool, func(tx pgx.Tx) error {
		cur, err := c.version(ctx, tx, wfID, vid, true)
		if err != nil {
			return err
		}
		now := c.clock.Now()
		var v Version
		switch cur.Status {
		case StatusDraft:
			if pre.Status != StatusDraft { // 没有回到 draft 的转换，不会发生
				return apperr.ErrInvalidState
			}
			manifest, err := json.Marshal(in.Manifest)
			if err != nil {
				return err
			}
			v, err = scanVersion(tx.QueryRow(ctx, `UPDATE comfy_workflow_versions SET version = $2, graph = $3, api_prompt = $4, manifest = $5,
				download_gb = $6, manifest_hash = $7, changelog = $8, updated_at = $9 WHERE id = $1 RETURNING `+verCols(""),
				vid, in.Version, []byte(in.Graph), jsonArg(in.APIPrompt), manifest, res.DownloadGB, res.ManifestHash, in.Changelog, now))
			if db.IsUniqueViolation(err) {
				return errVersionExists
			}
			if err != nil {
				return err
			}
		case StatusPublished:
			status, changelog, err := publishedEdit(cur, in)
			if err != nil {
				return err
			}
			if v, err = scanVersion(tx.QueryRow(ctx, `UPDATE comfy_workflow_versions SET changelog = $2, status = $3, updated_at = $4
				WHERE id = $1 RETURNING `+verCols(""), vid, changelog, status, now)); err != nil {
				return err
			}
		default:
			return apperr.ErrWorkflowVersionFrozen
		}
		out = v
		return audit.Record(ctx, tx, now, actor.Entry("workflow.version.update", "workflow_version", target(vid), map[string]any{
			"workflow_id": wfID,
			"before":      map[string]any{"version": cur.Version, "status": cur.Status, "manifest_hash": cur.ManifestHash, "changelog": cur.Changelog},
			"after":       map[string]any{"version": v.Version, "status": v.Status, "manifest_hash": v.ManifestHash, "changelog": v.Changelog},
		}))
	})
	return out, err
}

// publishedEdit 是已发布版本的修改规则：给出的 Version / Graph / APIPrompt / Manifest 须与当前内容相同（规范化后比较），
// Status 只能为空 / published（不改）或 retired；返回修改后的状态与更新说明。
func publishedEdit(cur Version, in VersionInput) (string, string, error) {
	if v := strings.TrimSpace(in.Version); v != "" && v != cur.Version {
		return "", "", apperr.ErrWorkflowVersionFrozen
	}
	if len(bytes.TrimSpace(in.Graph)) > 0 && !bytes.Equal(canonical(in.Graph), canonical(cur.Graph)) {
		return "", "", apperr.ErrWorkflowVersionFrozen
	}
	if len(bytes.TrimSpace(in.APIPrompt)) > 0 && !bytes.Equal(canonical(apiOrNil(in.APIPrompt)), canonical(cur.APIPrompt)) {
		return "", "", apperr.ErrWorkflowVersionFrozen
	}
	if !manifestIsZero(in.Manifest) {
		m := cloneManifest(in.Manifest)
		normalize(&m)
		a, _ := json.Marshal(m)
		b, _ := json.Marshal(cur.Manifest)
		if !bytes.Equal(canonical(a), canonical(b)) {
			return "", "", apperr.ErrWorkflowVersionFrozen
		}
	}
	status := cur.Status
	switch in.Status {
	case "", StatusPublished:
	case StatusRetired:
		status = StatusRetired
	default:
		return "", "", apperr.ErrWorkflowVersionFrozen
	}
	changelog := strings.TrimSpace(in.Changelog)
	if runeLen(changelog) > MaxChangelogLen {
		return "", "", invalid("changelog", fmt.Sprintf("最多 %d 个字符", MaxChangelogLen))
	}
	return status, changelog, nil
}

// ValidateVersionUpdate 只校验不保存（后台 PUT …/versions/{vid}?validate=1），与 UpdateVersion 同一规则：草稿完整校验并返回将写入的值；
// 已发布版本按冻结规则判断并返回当前值；已下线版本 → workflow_version_frozen。
func (c *Catalog) ValidateVersionUpdate(ctx context.Context, wfID, vid int64, in VersionInput) (ValidateResult, error) {
	cur, err := c.version(ctx, c.pool, wfID, vid, false)
	if err != nil {
		return ValidateResult{}, err
	}
	switch cur.Status {
	case StatusDraft:
		if in.Status != "" && in.Status != StatusDraft {
			return ValidateResult{}, apperr.ErrInvalidState
		}
		return validateVersionInput(&in)
	case StatusPublished:
		if _, _, err := publishedEdit(cur, in); err != nil {
			return ValidateResult{}, err
		}
		return ValidateResult{ManifestHash: cur.ManifestHash, DownloadGB: cur.DownloadGBValue(), Warnings: warnings(cur.Manifest, cur.APIPrompt)}, nil
	default:
		return ValidateResult{}, apperr.ErrWorkflowVersionFrozen
	}
}

// PublishVersion 在行锁内按锁住的内容重新完整校验草稿（规则可能已更新），然后发布：status = published、published_at、
// 冻结 manifest_hash / download_gb（及规范化后的 manifest）；审计 workflow.version.publish（含 manifest_hash）。
// 非草稿 → invalid_state。TOTP 在 adminapi 层校验。
func (c *Catalog) PublishVersion(ctx context.Context, actor audit.Actor, wfID, vid int64) (Version, error) {
	var out Version
	err := db.WithTx(ctx, c.pool, func(tx pgx.Tx) error {
		cur, err := c.version(ctx, tx, wfID, vid, true)
		if err != nil {
			return err
		}
		if cur.Status != StatusDraft {
			return apperr.ErrInvalidState
		}
		in := VersionInput{Version: cur.Version, Graph: cur.Graph, APIPrompt: cur.APIPrompt, Manifest: cur.Manifest, Changelog: cur.Changelog}
		res, err := validateVersionInput(&in)
		if err != nil {
			return err
		}
		manifest, err := json.Marshal(in.Manifest)
		if err != nil {
			return err
		}
		now := c.clock.Now()
		if out, err = scanVersion(tx.QueryRow(ctx, `UPDATE comfy_workflow_versions SET status = 'published', published_at = $2, manifest = $3,
			manifest_hash = $4, download_gb = $5, updated_at = $2 WHERE id = $1 RETURNING `+verCols(""),
			vid, now, manifest, res.ManifestHash, res.DownloadGB)); err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("workflow.version.publish", "workflow_version", target(vid), map[string]any{
			"workflow_id": wfID, "version": out.Version, "manifest_hash": out.ManifestHash, "download_gb": res.DownloadGB,
		}))
	})
	return out, err
}

// version 读工作流 wfID 下的版本 vid；forUpdate 时加行锁（须在事务内）。
func (c *Catalog) version(ctx context.Context, q db.Querier, wfID, vid int64, forUpdate bool) (Version, error) {
	sql := `SELECT ` + verCols("") + ` FROM comfy_workflow_versions WHERE id = $1 AND workflow_id = $2`
	if forUpdate {
		sql += ` FOR UPDATE`
	}
	return scanVersion(q.QueryRow(ctx, sql, vid, wfID))
}

// GetVersion 按版本 id 读（含 graph / api_prompt 本体）；不存在 → not_found。
func (c *Catalog) GetVersion(ctx context.Context, id int64) (Version, error) {
	return scanVersion(c.pool.QueryRow(ctx, `SELECT `+verCols("")+` FROM comfy_workflow_versions WHERE id = $1`, id))
}

// WorkflowVersion 读工作流 wfID 下的版本 vid（后台版本详情）；不存在或不属于该工作流 → not_found。
func (c *Catalog) WorkflowVersion(ctx context.Context, wfID, vid int64) (Version, error) {
	return c.version(ctx, c.pool, wfID, vid, false)
}

// ListVersions 按创建倒序返回工作流的全部版本（含 graph / api_prompt 本体；后台列表用 ListVersionRows）。
func (c *Catalog) ListVersions(ctx context.Context, wfID int64) ([]Version, error) {
	rows, err := c.pool.Query(ctx, `SELECT `+verCols("")+` FROM comfy_workflow_versions WHERE workflow_id = $1 ORDER BY id DESC`, wfID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Version, error) { return scanVersion(r) })
}

// VersionRow 是后台版本表的一行（不含 graph / api_prompt 本体）。Installs 口径同 liveInstallsCond（该版本上的）。
type VersionRow struct {
	ID           int64      `json:"id"`
	WorkflowID   int64      `json:"workflow_id"`
	Version      string     `json:"version"`
	Status       string     `json:"status"`
	DownloadGB   float64    `json:"download_gb"`
	ManifestHash string     `json:"manifest_hash"`
	Changelog    string     `json:"changelog"`
	Models       int        `json:"models"`
	Nodes        int        `json:"nodes"`
	QuickRun     bool       `json:"quick_run"`
	ComfyUIMin   string     `json:"comfyui_min"`
	Installs     int        `json:"installs"`
	PublishedAt  *time.Time `json:"published_at"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// ListVersionRows 按创建倒序返回工作流的版本表（后台 GET /workflows/{id}/versions）。
func (c *Catalog) ListVersionRows(ctx context.Context, wfID int64) ([]VersionRow, error) {
	rows, err := c.pool.Query(ctx, `SELECT v.id, v.workflow_id, v.version, v.status, v.download_gb::text, v.manifest_hash, v.changelog,
		jsonb_array_length(COALESCE(v.manifest->'models', '[]')), jsonb_array_length(COALESCE(v.manifest->'nodes', '[]')),
		v.api_prompt IS NOT NULL, COALESCE(v.manifest->>'comfyui_min', ''),
		(SELECT count(*) FROM deployment_workflows dw JOIN app_deployments d ON d.id = dw.deployment_id
			WHERE dw.workflow_version_id = v.id AND `+liveInstallsCond+`),
		v.published_at, v.created_at, v.updated_at
		FROM comfy_workflow_versions v WHERE v.workflow_id = $1 ORDER BY v.id DESC`, wfID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (VersionRow, error) {
		var row VersionRow
		var gb string
		err := r.Scan(&row.ID, &row.WorkflowID, &row.Version, &row.Status, &gb, &row.ManifestHash, &row.Changelog, &row.Models, &row.Nodes,
			&row.QuickRun, &row.ComfyUIMin, &row.Installs, &row.PublishedAt, &row.CreatedAt, &row.UpdatedAt)
		row.DownloadGB = Version{DownloadGB: gb}.DownloadGBValue()
		return row, err
	})
}

// LatestPublished 返回 published_at 最新的已发布版本（不是版本号字符串最大）；没有 → workflow_unavailable。
func (c *Catalog) LatestPublished(ctx context.Context, q db.Querier, wfID int64) (Version, error) {
	v, err := scanVersion(q.QueryRow(ctx, `SELECT `+verCols("")+` FROM comfy_workflow_versions WHERE workflow_id = $1 AND status = 'published'
		ORDER BY published_at DESC, id DESC LIMIT 1`, wfID))
	if errors.Is(err, apperr.ErrNotFound) {
		return Version{}, apperr.ErrWorkflowUnavailable
	}
	return v, err
}
