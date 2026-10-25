package comfywf

import (
	"context"
	"errors"
	"io"
	"strings"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/storage"
)

// CoverPath 是控制台匿名取封面的路径（规格 §10.1 GET /api/v1/workflows/{slug}/cover）。
func CoverPath(slug string) string { return "/api/v1/workflows/" + slug + "/cover" }

// CoverURL 是视图里的 cover_url：无封面 → nil；有封面 → CoverPath + "?v=<封面文件键的前 8 位随机字符>"
// （换封面后 URL 随之改变，封面接口可以放心带长缓存；不暴露完整的 cover_key）。
func CoverURL(w Workflow) *string {
	if w.CoverKey == nil {
		return nil
	}
	u := CoverPath(w.Slug)
	key := *w.CoverKey
	if i := strings.LastIndexByte(key, '/'); i >= 0 && len(key)-i > 8 {
		u += "?v=" + key[i+1:i+9]
	}
	return &u
}

// SetCover 保存封面（storage 用途 workflow_cover，所有者 0；PNG / JPG / WebP，≤ 2MB，超限 413 file_too_large、
// 类型不对 415 unsupported_file_type）并替换工作流的 cover_key；旧封面文件在提交后删除（失败只记日志）。
// 审计 workflow.update（diff 带 cover）。
func (c *Catalog) SetCover(ctx context.Context, actor audit.Actor, wfID int64, r io.Reader) (Workflow, error) {
	if c.uploads == nil {
		return Workflow{}, errors.New("comfywf: 未配置文件存储")
	}
	if _, err := c.GetWorkflow(ctx, wfID); err != nil { // 不存在就不落文件
		return Workflow{}, err
	}
	up, err := c.uploads.Save(ctx, 0, storage.PurposeWorkflowCover, r)
	if err != nil {
		return Workflow{}, err
	}
	var out Workflow
	var old *string
	err = db.WithTx(ctx, c.pool, func(tx pgx.Tx) error {
		before, err := scanWorkflow(tx.QueryRow(ctx, `SELECT `+wfCols+` FROM comfy_workflows WHERE id = $1 FOR UPDATE`, wfID))
		if err != nil {
			return err
		}
		old = before.CoverKey
		now := c.clock.Now()
		if out, err = scanWorkflow(tx.QueryRow(ctx, `UPDATE comfy_workflows SET cover_key = $2, updated_at = $3 WHERE id = $1 RETURNING `+wfCols,
			wfID, up.Key, now)); err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("workflow.update", "workflow", target(wfID), map[string]any{
			"slug": out.Slug, "cover": map[string]any{"replaced": old != nil, "content_type": up.ContentType, "size": up.Size},
		}))
	})
	if err != nil {
		c.deleteUpload(context.WithoutCancel(ctx), up.Key)
		return Workflow{}, err
	}
	if old != nil {
		c.deleteUpload(context.WithoutCancel(ctx), *old)
	}
	return out, nil
}

// deleteUpload 删除封面文件与登记；失败只记日志（留下的孤儿文件无害）。
func (c *Catalog) deleteUpload(ctx context.Context, key string) {
	if c.uploads == nil {
		return
	}
	if err := c.uploads.Delete(ctx, key, storage.PurposeWorkflowCover); err != nil {
		c.log.Warn("workflow cover delete failed", "key", key, "err", err)
	}
}

// OpenCover 控制台取封面（匿名）：工作流不存在或是草稿 → workflow_not_found；没有封面 → not_found。调用方负责关闭。
func (c *Catalog) OpenCover(ctx context.Context, slug string) (storage.Upload, io.ReadCloser, error) {
	w, err := c.WorkflowBySlug(ctx, slug)
	if err == nil && w.Status == StatusDraft {
		return storage.Upload{}, nil, apperr.ErrWorkflowNotFound
	}
	if err != nil {
		return storage.Upload{}, nil, err
	}
	return c.openCover(ctx, w)
}

// OpenCoverByID 后台取封面（任意状态）：工作流不存在 → workflow_not_found；没有封面 → not_found。调用方负责关闭。
func (c *Catalog) OpenCoverByID(ctx context.Context, id int64) (storage.Upload, io.ReadCloser, error) {
	w, err := c.GetWorkflow(ctx, id)
	if err != nil {
		return storage.Upload{}, nil, err
	}
	return c.openCover(ctx, w)
}

func (c *Catalog) openCover(ctx context.Context, w Workflow) (storage.Upload, io.ReadCloser, error) {
	if w.CoverKey == nil || c.uploads == nil {
		return storage.Upload{}, nil, apperr.ErrNotFound
	}
	return c.uploads.Open(ctx, *w.CoverKey)
}
