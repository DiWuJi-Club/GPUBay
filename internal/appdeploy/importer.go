package appdeploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	Version     string   `json:"version"`
	Images      []string `json:"images"`
	Changelog   string   `json:"changelog"`
}

type ImportResult struct {
	Slug    string
	Version string
	Created bool // true = 新建了应用；false = 只给已有应用加了版本
}

// ImportDir 按目录名顺序导入 dir 下每个 <slug>/{app.json, spec.json, app.sh}：slug 不存在 → 建 draft 应用 + draft 版本；
// 已存在 → 只建 draft 版本（版本号重复报错）。导入不发布。某个目录失败时返回错误，之前导入的保留。
func (c *Catalog) ImportDir(ctx context.Context, actor audit.Actor, dir string) ([]ImportResult, error) {
	entries, err := os.ReadDir(dir) // 已按文件名排序
	if err != nil {
		return nil, err
	}
	var out []ImportResult
	for _, ent := range entries {
		if !ent.IsDir() || strings.HasPrefix(ent.Name(), ".") {
			continue
		}
		slug := ent.Name()
		r, err := c.importApp(ctx, actor, filepath.Join(dir, slug), slug)
		if err != nil {
			return out, fmt.Errorf("%s: %w", slug, err)
		}
		out = append(out, r)
	}
	return out, nil
}

func (c *Catalog) importApp(ctx context.Context, actor audit.Actor, dir, slug string) (ImportResult, error) {
	read := func(name string) ([]byte, error) { return os.ReadFile(filepath.Join(dir, name)) }
	metaRaw, err := read("app.json")
	if err != nil {
		return ImportResult{}, err
	}
	specRaw, err := read("spec.json")
	if err != nil {
		return ImportResult{}, err
	}
	script, err := read("app.sh")
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
	// 先校验版本，避免版本非法时留下一个没有版本的新应用
	if _, _, err := c.validateVersionInput(ctx, c.pool, &in); err != nil {
		return ImportResult{}, err
	}
	res := ImportResult{Slug: slug, Version: meta.Version}
	app, err := c.AppBySlug(ctx, slug)
	if errors.Is(err, apperr.ErrNotFound) {
		app, err = c.CreateApp(ctx, actor, AppInput{Slug: slug, Name: meta.Name, Summary: meta.Summary, Description: meta.Description,
			Category: meta.Category, Icon: meta.Icon, Sort: meta.Sort, Status: StatusDraft})
		res.Created = err == nil
	}
	if err != nil {
		return ImportResult{}, err
	}
	if _, err := c.CreateVersion(ctx, actor, app.ID, in); err != nil {
		return ImportResult{}, err
	}
	return res, nil
}
