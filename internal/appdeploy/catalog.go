package appdeploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/db"
)

// 应用与版本的状态（apps.status / app_versions.status 共用同一组取值）。
const (
	StatusDraft     = "draft"
	StatusPublished = "published"
	StatusRetired   = "retired"
)

var (
	appCategories = []string{"llm", "image", "notebook", "tool"}
	appIcons      = []string{"app", "llm", "chat", "image", "notebook", "tool", "code", "audio"}
	appStatuses   = []string{StatusDraft, StatusPublished, StatusRetired}
)

const (
	MaxChangelogLen = 1000
	maxNameLen      = 32
	maxSummaryLen   = 80
	maxDescLen      = 4000
)

// Catalog 维护应用目录：应用、版本（草稿 → 发布 → 下线）、发布时的镜像预拉取与缓存覆盖率。
type Catalog struct {
	pool   *pgxpool.Pool
	clock  clock.Clock
	fleet  *fleet.Service
	images *catalog.Service
	log    *slog.Logger
}

func NewCatalog(pool *pgxpool.Pool, clk clock.Clock, fleet *fleet.Service, images *catalog.Service, log *slog.Logger) *Catalog {
	return &Catalog{pool: pool, clock: clk, fleet: fleet, images: images, log: log}
}

const (
	appCols     = "id, slug, name, summary, description, category, icon, sort, status, created_at, updated_at"
	versionCols = "id, app_id, version, status, image_ids, spec, script, spec_hash, changelog, published_at, created_at, updated_at"
)

// qualify 给列清单加表别名前缀（"a." + 每一列）。
func qualify(cols, alias string) string {
	return alias + "." + strings.ReplaceAll(cols, ", ", ", "+alias+".")
}

func scanApp(row pgx.Row, extra ...any) (App, error) {
	var a App
	dest := append([]any{&a.ID, &a.Slug, &a.Name, &a.Summary, &a.Description, &a.Category, &a.Icon, &a.Sort, &a.Status, &a.CreatedAt, &a.UpdatedAt}, extra...)
	err := row.Scan(dest...)
	if db.IsNoRows(err) {
		return App{}, apperr.ErrNotFound
	}
	return a, err
}

func scanVersion(row pgx.Row, extra ...any) (Version, error) {
	var v Version
	var spec []byte
	dest := append([]any{&v.ID, &v.AppID, &v.Version, &v.Status, &v.ImageIDs, &spec, &v.Script, &v.SpecHash, &v.Changelog, &v.PublishedAt, &v.CreatedAt, &v.UpdatedAt}, extra...)
	err := row.Scan(dest...)
	if db.IsNoRows(err) {
		return Version{}, apperr.ErrNotFound
	}
	if err != nil {
		return Version{}, err
	}
	if err := json.Unmarshal(spec, &v.Spec); err != nil {
		return Version{}, fmt.Errorf("app_versions %d spec: %w", v.ID, err)
	}
	return v, nil
}

// ---- 应用 ----

type AppInput struct {
	Slug, Name, Summary, Description, Category, Icon string
	Sort                                             int
	Status                                           string
}

// normalizeApp 校验并补默认值；checkSlug 为 false 时（更新）忽略 Slug。
func normalizeApp(in AppInput, checkSlug bool) (AppInput, error) {
	in.Name = strings.TrimSpace(in.Name)
	in.Summary = strings.TrimSpace(in.Summary)
	in.Description = strings.TrimSpace(in.Description)
	if in.Icon == "" {
		in.Icon = "app"
	}
	if in.Status == "" {
		in.Status = StatusDraft
	}
	switch {
	case checkSlug && !ValidSlug(in.Slug):
		return in, apperr.BadRequest("slug 只能是小写字母开头的 2～32 位小写字母、数字或连字符")
	case in.Name == "" || runeLen(in.Name) > maxNameLen:
		return in, apperr.BadRequest("请填写应用名称（最多 32 个字符）")
	case runeLen(in.Summary) > maxSummaryLen:
		return in, apperr.BadRequest("简介最多 80 个字符")
	case runeLen(in.Description) > maxDescLen:
		return in, apperr.BadRequest("说明最多 4000 个字符")
	case !slices.Contains(appCategories, in.Category):
		return in, apperr.BadRequest("类别只能是 llm、image、notebook 或 tool")
	case !slices.Contains(appIcons, in.Icon):
		return in, apperr.BadRequest("图标只能是 " + strings.Join(appIcons, "、") + " 之一")
	case in.Sort < -10000 || in.Sort > 10000:
		return in, apperr.BadRequest("排序值需在 -10000～10000 之间")
	case !slices.Contains(appStatuses, in.Status):
		return in, apperr.BadRequest("状态只能是 draft、published 或 retired")
	}
	return in, nil
}

var errSlugExists = apperr.New(apperr.ErrDuplicate.Code, "slug 已存在", apperr.ErrDuplicate.Status)

func appTarget(id int64) string { return strconv.FormatInt(id, 10) }

func (c *Catalog) CreateApp(ctx context.Context, actor audit.Actor, in AppInput) (App, error) {
	in, err := normalizeApp(in, true)
	if err != nil {
		return App{}, err
	}
	var out App
	err = db.WithTx(ctx, c.pool, func(tx pgx.Tx) error {
		now := c.clock.Now()
		a, err := scanApp(tx.QueryRow(ctx, `INSERT INTO apps (slug, name, summary, description, category, icon, sort, status, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $9) RETURNING `+appCols,
			in.Slug, in.Name, in.Summary, in.Description, in.Category, in.Icon, in.Sort, in.Status, now))
		if db.IsUniqueViolation(err) {
			return errSlugExists
		}
		if err != nil {
			return err
		}
		out = a
		return audit.Record(ctx, tx, now, actor.Entry("app.create", "app", appTarget(a.ID), a))
	})
	return out, err
}

// UpdateApp 修改应用的展示信息与状态；slug 创建后不可改（in.Slug 被忽略）。
func (c *Catalog) UpdateApp(ctx context.Context, actor audit.Actor, id int64, in AppInput) (App, error) {
	in, err := normalizeApp(in, false)
	if err != nil {
		return App{}, err
	}
	var out App
	err = db.WithTx(ctx, c.pool, func(tx pgx.Tx) error {
		before, err := scanApp(tx.QueryRow(ctx, `SELECT `+appCols+` FROM apps WHERE id = $1 FOR UPDATE`, id))
		if err != nil {
			return err
		}
		now := c.clock.Now()
		a, err := scanApp(tx.QueryRow(ctx, `UPDATE apps SET name = $2, summary = $3, description = $4, category = $5, icon = $6, sort = $7, status = $8, updated_at = $9
			WHERE id = $1 RETURNING `+appCols, id, in.Name, in.Summary, in.Description, in.Category, in.Icon, in.Sort, in.Status, now))
		if err != nil {
			return err
		}
		out = a
		return audit.Record(ctx, tx, now, actor.Entry("app.update", "app", appTarget(id), map[string]any{"before": before, "after": a}))
	})
	return out, err
}

// DeleteApp 只能删除没有任何版本的草稿应用（已有版本的应用用 retired 下线）。
func (c *Catalog) DeleteApp(ctx context.Context, actor audit.Actor, id int64) error {
	return db.WithTx(ctx, c.pool, func(tx pgx.Tx) error {
		var status string
		var versions int
		err := tx.QueryRow(ctx, `SELECT status, (SELECT count(*) FROM app_versions WHERE app_id = $1) FROM apps WHERE id = $1 FOR UPDATE`, id).
			Scan(&status, &versions)
		if db.IsNoRows(err) {
			return apperr.ErrNotFound
		}
		if err != nil {
			return err
		}
		if status != StatusDraft || versions > 0 {
			return apperr.ErrInvalidState
		}
		var slug string
		if err := tx.QueryRow(ctx, `DELETE FROM apps WHERE id = $1 RETURNING slug`, id).Scan(&slug); err != nil {
			return err
		}
		return audit.Record(ctx, tx, c.clock.Now(), actor.Entry("app.delete", "app", appTarget(id), map[string]any{"slug": slug}))
	})
}

func (c *Catalog) GetApp(ctx context.Context, id int64) (App, error) {
	return scanApp(c.pool.QueryRow(ctx, `SELECT `+appCols+` FROM apps WHERE id = $1`, id))
}

func (c *Catalog) AppBySlug(ctx context.Context, slug string) (App, error) {
	return scanApp(c.pool.QueryRow(ctx, `SELECT `+appCols+` FROM apps WHERE slug = $1`, slug))
}

// AppRow 是后台应用列表的一行：版本数与未终结（非 removed / lost）的部署数。
type AppRow struct {
	App
	Versions        int `json:"versions"`
	DeploymentsLive int `json:"deployments_live"`
}

func (c *Catalog) ListAppsAdmin(ctx context.Context) ([]AppRow, error) {
	rows, err := c.pool.Query(ctx, `SELECT `+qualify(appCols, "a")+`,
		(SELECT count(*) FROM app_versions v WHERE v.app_id = a.id),
		(SELECT count(*) FROM app_deployments d WHERE d.app_id = a.id AND d.state NOT IN ('removed', 'lost'))
		FROM apps a ORDER BY a.sort, a.id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (AppRow, error) {
		var row AppRow
		var err error
		row.App, err = scanApp(r, &row.Versions, &row.DeploymentsLive)
		return row, err
	})
}

// ---- 版本 ----

// VersionInput 是建 / 改版本的输入。CopyFrom 只用于建版本：以该版本的 spec / 脚本 / 镜像为基底，in 的非零字段覆盖。
// Status 只用于修改已发布版本（空 = 不改，"retired" = 下线）。
type VersionInput struct {
	Version   string
	ImageIDs  []int64
	Spec      Spec
	Script    string
	Changelog string
	CopyFrom  *int64
	Status    string
}

func specIsZero(s Spec) bool {
	return len(s.Ports) == 0 && s.TimeoutMinutes == 0 && s.MinDataFreeGiB == 0 && len(s.Env) == 0 && len(s.Links) == 0
}

// cloneSpec 深拷贝（ValidateSpec 会就地补默认值，不能改到调用方的切片）。
func cloneSpec(s Spec) Spec {
	b, _ := json.Marshal(s)
	var out Spec
	_ = json.Unmarshal(b, &out)
	return out
}

// canonical 是规范化（补默认值）后的 spec JSON；非法 spec 返回一个不会与任何合法 spec 相等的值。
func canonical(s Spec) string {
	cp := cloneSpec(s)
	if err := ValidateSpec(&cp); err != nil {
		return "\x00invalid: " + err.Error()
	}
	b, _ := json.Marshal(cp)
	return string(b)
}

func dedupIDs(ids []int64) []int64 {
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

// validateVersionInput 校验版本（草稿也要过），就地去重 in.ImageIDs，返回规范化后的 spec 与 SpecHash。
func (c *Catalog) validateVersionInput(ctx context.Context, q db.Querier, in *VersionInput) (Spec, string, error) {
	if !ValidVersion(in.Version) {
		return Spec{}, "", invalid("version", "只能由字母、数字、点和连字符组成（最多 32 个字符），且以字母或数字开头")
	}
	if in.Script == "" {
		return Spec{}, "", invalid("script", "脚本不能为空")
	}
	if len(in.Script) > MaxScriptBytes {
		return Spec{}, "", invalid("script", fmt.Sprintf("脚本不能超过 %d 字节", MaxScriptBytes))
	}
	spec := cloneSpec(in.Spec)
	if err := ValidateSpec(&spec); err != nil {
		return Spec{}, "", err
	}
	if err := CheckShellSyntax(ctx, in.Script); err != nil {
		return Spec{}, "", err
	}
	in.ImageIDs = dedupIDs(in.ImageIDs)
	if len(in.ImageIDs) == 0 {
		return Spec{}, "", invalid("image_ids", "至少选择一个镜像")
	}
	var n int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM images WHERE id = ANY($1) AND enabled`, in.ImageIDs).Scan(&n); err != nil {
		return Spec{}, "", err
	}
	if n != len(in.ImageIDs) {
		return Spec{}, "", invalid("image_ids", "镜像不存在或未启用")
	}
	if runeLen(in.Changelog) > MaxChangelogLen {
		return Spec{}, "", invalid("changelog", "最多 1000 个字符")
	}
	return spec, SpecHash(spec, in.Script), nil
}

// withCopy 按 in.CopyFrom（须属于 appID）合成建版本的输入。
func (c *Catalog) withCopy(ctx context.Context, appID int64, in VersionInput) (VersionInput, error) {
	if in.CopyFrom == nil {
		return in, nil
	}
	base, err := scanVersion(c.pool.QueryRow(ctx, `SELECT `+versionCols+` FROM app_versions WHERE id = $1 AND app_id = $2`, *in.CopyFrom, appID))
	if err != nil {
		return in, err
	}
	out := in
	if len(in.ImageIDs) == 0 {
		out.ImageIDs = base.ImageIDs
	}
	if specIsZero(in.Spec) {
		out.Spec = base.Spec
	}
	if in.Script == "" {
		out.Script = base.Script
	}
	return out, nil
}

var errVersionExists = apperr.New(apperr.ErrDuplicate.Code, "版本号已存在", apperr.ErrDuplicate.Status)

func (c *Catalog) CreateVersion(ctx context.Context, actor audit.Actor, appID int64, in VersionInput) (Version, error) {
	in, err := c.withCopy(ctx, appID, in)
	if err != nil {
		return Version{}, err
	}
	spec, hash, err := c.validateVersionInput(ctx, c.pool, &in)
	if err != nil {
		return Version{}, err
	}
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return Version{}, err
	}
	var out Version
	err = db.WithTx(ctx, c.pool, func(tx pgx.Tx) error {
		now := c.clock.Now()
		v, err := scanVersion(tx.QueryRow(ctx, `INSERT INTO app_versions (app_id, version, status, image_ids, spec, script, spec_hash, changelog, created_at, updated_at)
			VALUES ($1, $2, 'draft', $3, $4, $5, $6, $7, $8, $8) RETURNING `+versionCols,
			appID, in.Version, in.ImageIDs, specJSON, in.Script, hash, in.Changelog, now))
		switch {
		case db.IsForeignKeyViolation(err):
			return apperr.ErrNotFound
		case db.IsUniqueViolation(err):
			return errVersionExists
		case err != nil:
			return err
		}
		out = v
		return audit.Record(ctx, tx, now, actor.Entry("app.version.create", "app_version", appTarget(v.ID), map[string]any{
			"app_id": appID, "version": v.Version, "spec_hash": hash, "image_ids": v.ImageIDs, "copy_from": in.CopyFrom,
		}))
	})
	return out, err
}

// ValidateVersion 只校验不保存（后台 `?validate=1`），返回将要写入的 spec_hash。
func (c *Catalog) ValidateVersion(ctx context.Context, appID int64, in VersionInput) (string, error) {
	in, err := c.withCopy(ctx, appID, in)
	if err != nil {
		return "", err
	}
	_, hash, err := c.validateVersionInput(ctx, c.pool, &in)
	return hash, err
}

// UpdateVersion：草稿整体可改；已发布版本只能改 Changelog 或下线（Status = "retired"），其它字段与当前值不同 → app_version_frozen；
// 已下线版本不能再改。
func (c *Catalog) UpdateVersion(ctx context.Context, actor audit.Actor, appID, vid int64, in VersionInput) (Version, error) {
	var out Version
	err := db.WithTx(ctx, c.pool, func(tx pgx.Tx) error {
		cur, err := scanVersion(tx.QueryRow(ctx, `SELECT `+versionCols+` FROM app_versions WHERE id = $1 AND app_id = $2 FOR UPDATE`, vid, appID))
		if err != nil {
			return err
		}
		now := c.clock.Now()
		var v Version
		switch cur.Status {
		case StatusDraft:
			if in.Status != "" && in.Status != StatusDraft {
				return apperr.ErrInvalidState // 发布走 PublishVersion（需动态码）
			}
			spec, hash, err := c.validateVersionInput(ctx, tx, &in)
			if err != nil {
				return err
			}
			specJSON, err := json.Marshal(spec)
			if err != nil {
				return err
			}
			v, err = scanVersion(tx.QueryRow(ctx, `UPDATE app_versions SET version = $2, image_ids = $3, spec = $4, script = $5, spec_hash = $6, changelog = $7, updated_at = $8
				WHERE id = $1 RETURNING `+versionCols, vid, in.Version, in.ImageIDs, specJSON, in.Script, hash, in.Changelog, now))
			if db.IsUniqueViolation(err) {
				return errVersionExists
			}
			if err != nil {
				return err
			}
		case StatusPublished:
			if in.Version != cur.Version || !slices.Equal(in.ImageIDs, cur.ImageIDs) || in.Script != cur.Script || canonical(in.Spec) != canonical(cur.Spec) {
				return apperr.ErrAppVersionFrozen
			}
			status := cur.Status
			switch in.Status {
			case "":
			case StatusRetired:
				status = StatusRetired
			default:
				return apperr.ErrAppVersionFrozen
			}
			if runeLen(in.Changelog) > MaxChangelogLen {
				return invalid("changelog", "最多 1000 个字符")
			}
			v, err = scanVersion(tx.QueryRow(ctx, `UPDATE app_versions SET changelog = $2, status = $3, updated_at = $4 WHERE id = $1 RETURNING `+versionCols,
				vid, in.Changelog, status, now))
			if err != nil {
				return err
			}
		default:
			return apperr.ErrAppVersionFrozen
		}
		out = v
		return audit.Record(ctx, tx, now, actor.Entry("app.version.update", "app_version", appTarget(vid), map[string]any{
			"app_id": appID,
			"before": map[string]any{"version": cur.Version, "status": cur.Status, "spec_hash": cur.SpecHash, "image_ids": cur.ImageIDs, "changelog": cur.Changelog},
			"after":  map[string]any{"version": v.Version, "status": v.Status, "spec_hash": v.SpecHash, "image_ids": v.ImageIDs, "changelog": v.Changelog},
		}))
	})
	return out, err
}

// imageRefs 按 ids 的顺序返回镜像 ref（不存在的跳过）。
func imageRefs(ctx context.Context, q db.Querier, ids []int64) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT ref FROM images WHERE id = ANY($1) ORDER BY array_position($1, id)`, ids)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// broadcast 对每个镜像向所有在线机器下发预拉取；失败只记日志。返回下发的机器台数与最后一个错误。
func (c *Catalog) broadcast(ctx context.Context, refs []string) (int, error) {
	sent := 0
	var lastErr error
	for _, ref := range refs {
		n, err := c.fleet.BroadcastPullImage(ctx, ref)
		if err != nil {
			c.log.Warn("app image prepull broadcast failed", "ref", ref, "err", err)
			lastErr = err
			continue
		}
		sent = max(sent, n)
	}
	return sent, lastErr
}

// PublishVersion 重新校验草稿（镜像可能已停用）并发布，然后向在线机器广播预拉取；返回发布后的版本与下发台数。
func (c *Catalog) PublishVersion(ctx context.Context, actor audit.Actor, appID, vid int64) (Version, int, error) {
	var out Version
	var refs []string
	err := db.WithTx(ctx, c.pool, func(tx pgx.Tx) error {
		cur, err := scanVersion(tx.QueryRow(ctx, `SELECT `+versionCols+` FROM app_versions WHERE id = $1 AND app_id = $2 FOR UPDATE`, vid, appID))
		if err != nil {
			return err
		}
		if cur.Status != StatusDraft {
			return apperr.ErrInvalidState
		}
		in := VersionInput{Version: cur.Version, ImageIDs: cur.ImageIDs, Spec: cur.Spec, Script: cur.Script, Changelog: cur.Changelog}
		_, hash, err := c.validateVersionInput(ctx, tx, &in)
		if err != nil {
			return err
		}
		now := c.clock.Now()
		if out, err = scanVersion(tx.QueryRow(ctx, `UPDATE app_versions SET status = 'published', published_at = $2, spec_hash = $3, updated_at = $2
			WHERE id = $1 RETURNING `+versionCols, vid, now, hash)); err != nil {
			return err
		}
		if refs, err = imageRefs(ctx, tx, out.ImageIDs); err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("app.version.publish", "app_version", appTarget(vid), map[string]any{
			"app_id": appID, "version": out.Version, "spec_hash": hash, "image_ids": out.ImageIDs,
		}))
	})
	if err != nil {
		return Version{}, 0, err
	}
	sent, _ := c.broadcast(ctx, refs)
	return out, sent, nil
}

// Prepull 再次向在线机器下发该版本全部镜像的预拉取；返回下发台数（全部失败时返回错误）。
func (c *Catalog) Prepull(ctx context.Context, actor audit.Actor, appID, vid int64) (int, error) {
	v, err := scanVersion(c.pool.QueryRow(ctx, `SELECT `+versionCols+` FROM app_versions WHERE id = $1 AND app_id = $2`, vid, appID))
	if err != nil {
		return 0, err
	}
	refs, err := imageRefs(ctx, c.pool, v.ImageIDs)
	if err != nil {
		return 0, err
	}
	sent, err := c.broadcast(ctx, refs)
	if sent == 0 && err != nil {
		return 0, err
	}
	if err := audit.Record(ctx, c.pool, c.clock.Now(), actor.Entry("app.version.prepull", "app_version", appTarget(vid), map[string]any{
		"app_id": appID, "images": refs, "machines": sent,
	})); err != nil {
		return sent, err
	}
	return sent, nil
}

// GetVersion 返回版本（含 Script）。
func (c *Catalog) GetVersion(ctx context.Context, id int64) (Version, error) {
	return scanVersion(c.pool.QueryRow(ctx, `SELECT `+versionCols+` FROM app_versions WHERE id = $1`, id))
}

// ListVersions 按创建倒序返回应用的全部版本。
func (c *Catalog) ListVersions(ctx context.Context, appID int64) ([]Version, error) {
	rows, err := c.pool.Query(ctx, `SELECT `+versionCols+` FROM app_versions WHERE app_id = $1 ORDER BY id DESC`, appID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Version, error) { return scanVersion(r) })
}

// LatestPublished 返回 published_at 最新的已发布版本（不是版本号字符串最大）；没有 → app_unavailable。
func (c *Catalog) LatestPublished(ctx context.Context, q db.Querier, appID int64) (Version, error) {
	v, err := scanVersion(q.QueryRow(ctx, `SELECT `+versionCols+` FROM app_versions WHERE app_id = $1 AND status = 'published'
		ORDER BY published_at DESC, id DESC LIMIT 1`, appID))
	if errors.Is(err, apperr.ErrNotFound) {
		return Version{}, apperr.ErrAppUnavailable
	}
	return v, err
}

// ---- 用户侧目录 ----

type ImageBrief struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type CatalogItem struct {
	App
	Version Version      `json:"version"`
	Images  []ImageBrief `json:"images"`
}

// ListPublished 返回已上架（apps.status = published 且有已发布版本）的应用及其最新已发布版本，按 sort, id 排序。
func (c *Catalog) ListPublished(ctx context.Context) ([]CatalogItem, error) {
	rows, err := c.pool.Query(ctx, `SELECT DISTINCT ON (a.id) `+qualify(appCols, "a")+`, `+qualify(versionCols, "v")+`
		FROM apps a JOIN app_versions v ON v.app_id = a.id AND v.status = 'published'
		WHERE a.status = 'published' ORDER BY a.id, v.published_at DESC, v.id DESC`)
	if err != nil {
		return nil, err
	}
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (CatalogItem, error) {
		var it CatalogItem
		var spec []byte
		v := &it.Version
		a, err := scanApp(r, &v.ID, &v.AppID, &v.Version, &v.Status, &v.ImageIDs, &spec, &v.Script, &v.SpecHash, &v.Changelog, &v.PublishedAt, &v.CreatedAt, &v.UpdatedAt)
		if err != nil {
			return it, err
		}
		it.App = a
		if err := json.Unmarshal(spec, &v.Spec); err != nil {
			return it, fmt.Errorf("app_versions %d spec: %w", v.ID, err)
		}
		return it, nil
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
	for i := range items {
		if items[i].Images, err = c.imageBriefs(ctx, items[i].Version.ImageIDs); err != nil {
			return nil, err
		}
	}
	return items, nil
}

// imageBriefs 按 ids 的顺序返回镜像 id / 名称（不存在的跳过）。
func (c *Catalog) imageBriefs(ctx context.Context, ids []int64) ([]ImageBrief, error) {
	rows, err := c.pool.Query(ctx, `SELECT id, name FROM images WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	found, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ImageBrief, error) {
		var b ImageBrief
		err := r.Scan(&b.ID, &b.Name)
		return b, err
	})
	if err != nil {
		return nil, err
	}
	out := make([]ImageBrief, 0, len(ids))
	for _, id := range ids {
		if i := slices.IndexFunc(found, func(b ImageBrief) bool { return b.ID == id }); i >= 0 {
			out = append(out, found[i])
		}
	}
	return out, nil
}
