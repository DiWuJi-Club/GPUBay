package comfywf

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/storage"
)

// workflowJSON 是 deploy/workflows/<slug>/workflow.json：工作流信息 + 本次导入的版本号与更新说明（规格 §9 README 表）。
// 未知字段报错（拼错的字段不会被悄悄忽略）。comfyui_min 非空时覆盖 manifest.json 里的同名字段。
type workflowJSON struct {
	Name        string   `json:"name"`
	Summary     string   `json:"summary"`
	Description string   `json:"description"`
	Category    string   `json:"category"`
	Tags        []string `json:"tags"`
	Sort        int      `json:"sort"`
	Version     string   `json:"version"`
	Changelog   string   `json:"changelog"`
	ComfyUIMin  string   `json:"comfyui_min"`
}

// ImportResult 是一个子目录的导入结果：Skipped 非空 = 未导入（符号链接）；Err 非空 = 导入失败（其它目录照常继续）。
type ImportResult struct {
	Slug    string
	Version string
	Created bool // true = 新建了工作流（draft）；false = 只给已有工作流加了 draft 版本
	Skipped string
	Err     error
}

// maxImportMetaJSON 是 workflow.json / manifest.json 的大小上限（读之前先 stat）。
const maxImportMetaJSON = 65536

// coverNames 是目录里可选的封面文件名（最多一个）。
var coverNames = []string{"cover.png", "cover.jpg", "cover.jpeg", "cover.webp"}

// errSymlink 表示工作流目录或其中的文件是符号链接：跳过该目录（不跟随链接读任意文件进数据库）。
type errSymlink struct{ name string }

func (e errSymlink) Error() string { return e.name + " 是符号链接，已跳过" }

// ImportDir 按目录名顺序导入 dir 下每个 <slug>/{workflow.json, graph.json, api.json?, manifest.json, cover.*?}：
// slug 不存在 → 建 draft 工作流 + draft 版本并设封面；已存在 → 只加 draft 版本（展示信息与封面不动）。导入不发布。
// 符号链接的目录或文件跳过（Skipped）；某个目录失败（含版本号重复）记在该项的 Err 里并继续其它目录，
// 全部处理完后有失败时返回汇总错误。
func (c *Catalog) ImportDir(ctx context.Context, actor audit.Actor, dir string) ([]ImportResult, error) {
	entries, err := os.ReadDir(dir) // 已按文件名排序
	if err != nil {
		return nil, err
	}
	var out []ImportResult
	failed := 0
	for _, ent := range entries {
		slug := ent.Name()
		if ent.Type()&fs.ModeSymlink != 0 {
			out = append(out, ImportResult{Slug: slug, Skipped: errSymlink{slug}.Error()})
			continue
		}
		if !ent.IsDir() || strings.HasPrefix(slug, ".") {
			continue
		}
		r, err := c.importWorkflow(ctx, actor, filepath.Join(dir, slug), slug)
		var sl errSymlink
		switch {
		case errors.As(err, &sl):
			r = ImportResult{Slug: slug, Skipped: sl.Error()}
		case err != nil:
			r.Slug, r.Err = slug, err
			failed++
		}
		out = append(out, r)
	}
	if failed > 0 {
		return out, fmt.Errorf("%d 个工作流目录导入失败", failed)
	}
	return out, nil
}

// readImportFile 读工作流目录下的普通文件：不存在且 optional → (nil, nil)；符号链接 → errSymlink；超过 limit 字节在读之前就拒绝。
func readImportFile(dir, name string, limit int64, optional bool) ([]byte, error) {
	path := filepath.Join(dir, name)
	fi, err := os.Lstat(path)
	if optional && errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
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

// decodeStrict 解析 JSON，未知字段或尾随内容报错；数字保留原文（json.Number）。
func decodeStrict(raw []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	d.UseNumber()
	if err := d.Decode(v); err != nil {
		return err
	}
	if d.More() {
		return errors.New("trailing data")
	}
	return nil
}

// readCover 找可选的封面文件（最多一个）；返回内容与文件名。
func readCover(dir string) ([]byte, string, error) {
	var data []byte
	var found string
	for _, name := range coverNames {
		b, err := readImportFile(dir, name, MaxCoverBytes, true)
		if err != nil {
			return nil, "", err
		}
		if b == nil {
			continue
		}
		if found != "" {
			return nil, "", fmt.Errorf("封面只能有一个（%s 与 %s）", found, name)
		}
		data, found = b, name
	}
	return data, found, nil
}

func (c *Catalog) importWorkflow(ctx context.Context, actor audit.Actor, dir, slug string) (ImportResult, error) {
	res := ImportResult{Slug: slug}
	metaRaw, err := readImportFile(dir, "workflow.json", maxImportMetaJSON, false)
	if err != nil {
		return res, err
	}
	graph, err := readImportFile(dir, "graph.json", 4*MaxGraphBytes, false) // 导出文件常带缩进：压缩后再按 MaxGraphBytes 校验
	if err != nil {
		return res, err
	}
	api, err := readImportFile(dir, "api.json", 4*MaxAPIPromptBytes, true)
	if err != nil {
		return res, err
	}
	manifestRaw, err := readImportFile(dir, "manifest.json", maxImportMetaJSON, false)
	if err != nil {
		return res, err
	}
	cover, coverName, err := readCover(dir)
	if err != nil {
		return res, err
	}
	var meta workflowJSON
	if err := decodeStrict(metaRaw, &meta); err != nil {
		return res, fmt.Errorf("workflow.json: %w", err)
	}
	var m Manifest
	if err := decodeStrict(manifestRaw, &m); err != nil {
		return res, fmt.Errorf("manifest.json: %w", err)
	}
	if s := strings.TrimSpace(meta.ComfyUIMin); s != "" {
		m.ComfyUIMin = s
	}
	res.Version = strings.TrimSpace(meta.Version)
	// 先校验版本（与工作流信息），避免版本非法时留下一个没有版本的新工作流
	in := VersionInput{Version: meta.Version, Graph: graph, APIPrompt: api, Manifest: m, Changelog: meta.Changelog}
	vres, err := validateVersionInput(&in)
	if err != nil {
		return res, err
	}
	existing, err := c.WorkflowBySlug(ctx, slug)
	if err == nil {
		err = db.WithTx(ctx, c.pool, func(tx pgx.Tx) error {
			_, err := c.insertVersion(ctx, tx, actor, existing.ID, in, vres)
			return err
		})
		return res, err
	}
	if !errors.Is(err, apperr.ErrNotFound) {
		return res, err
	}
	wf, err := normalizeWorkflow(WorkflowInput{Slug: slug, Name: meta.Name, Summary: meta.Summary, Description: meta.Description,
		Category: meta.Category, Tags: meta.Tags, Sort: meta.Sort, Status: StatusDraft}, true)
	if err != nil {
		return res, err
	}
	var coverKey *string
	if cover != nil {
		if c.uploads == nil {
			return res, errors.New("comfywf: 未配置文件存储，无法导入封面")
		}
		up, err := c.uploads.Save(ctx, 0, storage.PurposeWorkflowCover, bytes.NewReader(cover))
		if err != nil {
			return res, fmt.Errorf("%s: %w", coverName, err)
		}
		coverKey = &up.Key
	}
	err = db.WithTx(ctx, c.pool, func(tx pgx.Tx) error {
		w, err := c.insertWorkflow(ctx, tx, actor, wf, coverKey)
		if err != nil {
			return err
		}
		_, err = c.insertVersion(ctx, tx, actor, w.ID, in, vres)
		return err
	})
	if err != nil {
		if coverKey != nil {
			c.deleteUpload(context.WithoutCancel(ctx), *coverKey)
		}
		return res, err
	}
	res.Created = true
	return res, nil
}
