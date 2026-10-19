package appdeploy

import (
	"context"
	"time"

	"spark-cloud/internal/platform/db"
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
	since := c.clock.Now().Add(-CoverageFreshness)
	out := make([]Coverage, 0, len(v.ImageIDs))
	for _, imageID := range v.ImageIDs {
		cv := Coverage{ImageID: imageID}
		var ref string
		err := c.pool.QueryRow(ctx, `SELECT name, ref FROM images WHERE id = $1`, imageID).Scan(&cv.ImageName, &ref)
		if db.IsNoRows(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		ref = imageref.Key(ref) // 与 cached_images 同一形态（docker.io/library/ 折叠、补 :latest、截到 256 字节）
		err = c.pool.QueryRow(ctx, `SELECT
			count(*) FILTER (WHERE agent_online) AS online,
			count(*) FILTER (WHERE agent_online AND cached_images_at >= $2 AND $1 = ANY(cached_images)) AS cached,
			count(*) FILTER (WHERE agent_online AND (cached_images_at IS NULL OR cached_images_at < $2)) AS unknown
			FROM machines`, ref, since).Scan(&cv.Online, &cv.Cached, &cv.Unknown)
		if err != nil {
			return nil, err
		}
		out = append(out, cv)
	}
	return out, nil
}
