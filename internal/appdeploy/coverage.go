package appdeploy

import (
	"context"
	"slices"
	"time"

	"spark-cloud/internal/platform/imageref"
)

// CoverageFreshness：机器的 cached_images 超过这么久没更新就算「未知」（Agent 每分钟上报一次）。
const CoverageFreshness = 5 * time.Minute

// Coverage 是某镜像在在线机器上的缓存覆盖：Cached / Online 台已缓存，Unknown 台上报过期或从未上报（旧 Agent）。
type Coverage struct {
	ImageID   int64  `json:"image_id"`
	ImageName string `json:"image_name"`
	Cached    int    `json:"cached"`
	Online    int    `json:"online"`
	Unknown   int    `json:"unknown"`
}

// VersionCoverage 按版本 image_ids 的顺序返回每个镜像的覆盖率；已删除的镜像跳过。
func (c *Catalog) VersionCoverage(ctx context.Context, v Version) ([]Coverage, error) {
	m, err := c.VersionsCoverage(ctx, []Version{v})
	if err != nil {
		return nil, err
	}
	return m[v.ID], nil
}

// VersionsCoverage 批量计算多个版本的覆盖率（键为版本 ID，每项按 image_ids 顺序、已删除的镜像跳过）。
// 查询次数固定：镜像一次、在线 / 未知台数一次、按去重后的镜像 ref 统计已缓存台数一次。
func (c *Catalog) VersionsCoverage(ctx context.Context, versions []Version) (map[int64][]Coverage, error) {
	out := make(map[int64][]Coverage, len(versions))
	var ids []int64
	for _, v := range versions {
		out[v.ID] = []Coverage{}
		ids = append(ids, v.ImageIDs...)
	}
	if len(ids) == 0 {
		return out, nil
	}
	type imageInfo struct{ name, key string }
	images := map[int64]imageInfo{}
	rows, err := c.pool.Query(ctx, `SELECT id, name, ref FROM images WHERE id = ANY($1)`, dedupIDs(ids))
	if err != nil {
		return nil, err
	}
	var keys []string
	for rows.Next() {
		var id int64
		var name, ref string
		if err := rows.Scan(&id, &name, &ref); err != nil {
			rows.Close()
			return nil, err
		}
		key := imageref.Key(ref) // 与 cached_images 同一形态（docker.io/library/ 折叠、补 :latest、截到 256 字节）
		images[id] = imageInfo{name: name, key: key}
		if !slices.Contains(keys, key) {
			keys = append(keys, key)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	since := c.clock.Now().Add(-CoverageFreshness)
	var online, unknown int
	if err := c.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE agent_online),
		count(*) FILTER (WHERE agent_online AND (cached_images_at IS NULL OR cached_images_at < $1)) FROM machines`, since).Scan(&online, &unknown); err != nil {
		return nil, err
	}
	cached := map[string]int{}
	rows, err = c.pool.Query(ctx, `SELECT k, count(m.id) FROM unnest($1::text[]) AS k
		LEFT JOIN machines m ON m.agent_online AND m.cached_images_at >= $2 AND k = ANY(m.cached_images) GROUP BY k`, keys, since)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			rows.Close()
			return nil, err
		}
		cached[k] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, v := range versions {
		for _, id := range v.ImageIDs {
			img, ok := images[id]
			if !ok {
				continue
			}
			out[v.ID] = append(out[v.ID], Coverage{ImageID: id, ImageName: img.name, Cached: cached[img.key], Online: online, Unknown: unknown})
		}
	}
	return out, nil
}
