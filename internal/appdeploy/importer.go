package appdeploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
)

// appJSON 是 deploy/apps/<slug>/app.json：应用信息 + 本次导入的版本号、兼容镜像（images.name）与更新说明。
type appJSON struct {
	Name        string   `json:"name"`
	Summary     string   `json:"summary"`
	Description string   `json:"description"`
	Category    string   `json:"category"`
	Icon        string   `json:"icon"`
	Sort        int      `json:"sort"`
	Internal    bool     `json:"internal"` // 平台内部应用（只在新建应用时写入）
	Version     string   `json:"version"`
	Images      []string `json:"images"`
	Changelog   string   `json:"changelog"`
}

type ImportResult struct {
	Slug    string
	Version string
	Created bool   // true = 新建了应用；false = 只给已有应用加了版本
	Skipped string // 非空 = 未导入及原因（符号链接）
}

// maxImportJSON 是 app.json / spec.json 的大小上限（读之前先 stat）。
const maxImportJSON = 65536

// errSymlink 表示应用目录或其中的文件是符号链接：跳过该应用（不跟随链接读任意文件进数据库）。
type errSymlink struct{ name string }

func (e errSymlink) Error() string { return e.name + " 是符号链接，已跳过" }

// ImportDir 按目录名顺序导入 dir 下每个 <slug>/{app.json, spec.json, app.sh}：slug 不存在 → 建 draft 应用 + draft 版本；
// 已存在 → 只建 draft 版本（版本号重复报错）。导入不发布。符号链接的目录或文件跳过（结果里带 Skipped 原因）；
// 某个目录失败时返回错误，之前导入的保留。
func (c *Catalog) ImportDir(ctx context.Context, actor audit.Actor, dir string) ([]ImportResult, error) {
	entries, err := os.ReadDir(dir) // 已按文件名排序
	if err != nil {
		return nil, err
	}
	var out []ImportResult
	for _, ent := range entries {
		slug := ent.Name()
		if ent.Type()&fs.ModeSymlink != 0 {
			out = append(out, ImportResult{Slug: slug, Skipped: errSymlink{slug}.Error()})
			continue
		}
		if !ent.IsDir() || strings.HasPrefix(slug, ".") {
			continue
		}
		r, err := c.importApp(ctx, actor, filepath.Join(dir, slug), slug)
		var sl errSymlink
		if errors.As(err, &sl) {
			out = append(out, ImportResult{Slug: slug, Skipped: sl.Error()})
			continue
		}
		if err != nil {
			return out, fmt.Errorf("%s: %w", slug, err)
		}
		out = append(out, r)
	}
	return out, nil
}

// readImportFile 读应用目录下的普通文件：符号链接 → errSymlink；超过 limit 字节在读之前就拒绝。
func readImportFile(dir, name string, limit int64) ([]byte, error) {
	path := filepath.Join(dir, name)
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return nil, errSymlink{name}
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s 不是普通文件", name)
	}
	if fi.Size() > limit {
		return nil, fmt.Errorf("%s 超过 %d 字节", name, limit)
	}
	return os.ReadFile(path)
}

func (c *Catalog) importApp(ctx context.Context, actor audit.Actor, dir, slug string) (ImportResult, error) {
	metaRaw, err := readImportFile(dir, "app.json", maxImportJSON)
	if err != nil {
		return ImportResult{}, err
	}
	specRaw, err := readImportFile(dir, "spec.json", maxImportJSON)
	if err != nil {
		return ImportResult{}, err
	}
	script, err := readImportFile(dir, "app.sh", MaxScriptBytes)
	if err != nil {
		return ImportResult{}, err
	}
	var meta appJSON
	if err := json.Unmarshal(metaRaw, &meta); err != nil {
		return ImportResult{}, fmt.Errorf("app.json: %w", err)
	}
	var spec Spec
	if err := json.Unmarshal(specRaw, &spec); err != nil {
		return ImportResult{}, fmt.Errorf("spec.json: %w", err)
	}
	in := VersionInput{Version: meta.Version, Spec: spec, Script: string(script), Changelog: meta.Changelog}
	for _, name := range meta.Images {
		var id int64
		err := c.pool.QueryRow(ctx, `SELECT id FROM images WHERE name = $1 ORDER BY id LIMIT 1`, name).Scan(&id)
		if db.IsNoRows(err) {
			return ImportResult{}, fmt.Errorf("镜像 %q 未登记", name)
		}
		if err != nil {
			return ImportResult{}, err
		}
		in.ImageIDs = append(in.ImageIDs, id)
	}
	// 先校验版本（只校验这一次），避免版本非法时留下一个没有版本的新应用
	norm, hash, err := c.validateVersionInput(ctx, c.pool, &in)
	if err != nil {
		return ImportResult{}, err
	}
	res := ImportResult{Slug: slug, Version: meta.Version}
	app, err := c.AppBySlug(ctx, slug)
	if errors.Is(err, apperr.ErrNotFound) {
		app, err = c.CreateApp(ctx, actor, AppInput{Slug: slug, Name: meta.Name, Summary: meta.Summary, Description: meta.Description,
			Category: meta.Category, Icon: meta.Icon, Sort: meta.Sort, Status: StatusDraft, Internal: meta.Internal})
		res.Created = err == nil
	}
	if err != nil {
		return ImportResult{}, err
	}
	if _, err := c.insertVersion(ctx, actor, app.ID, in, norm, hash); err != nil {
		return ImportResult{}, err
	}
	return res, nil
}
