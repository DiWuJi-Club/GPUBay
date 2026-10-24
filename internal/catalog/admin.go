package catalog

import (
	"context"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
)

const MaxPrice = 100_000_000 // 单个租期价格上限（分）

type AdminPrice struct {
	Duration  Duration  `json:"duration"`
	Amount    int64     `json:"amount"`
	Enabled   bool      `json:"enabled"`
	UpdatedAt time.Time `json:"updated_at"`
}

type AdminProduct struct {
	Product
	Status string       `json:"status"`
	Prices []AdminPrice `json:"prices"`
}

// AdminProducts 返回全部商品及全部租期价格（含已停用的）。
func (s *Service) AdminProducts(ctx context.Context) ([]AdminProduct, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, code, name, spec, status FROM products ORDER BY id`)
	if err != nil {
		return nil, err
	}
	products, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (AdminProduct, error) {
		var p AdminProduct
		err := r.Scan(&p.ID, &p.Code, &p.Name, &p.Spec, &p.Status)
		return p, err
	})
	if err != nil {
		return nil, err
	}
	for i := range products {
		rows, err := s.pool.Query(ctx, `SELECT duration, amount, enabled, updated_at FROM prices WHERE product_id = $1
			ORDER BY array_position(ARRAY['1d', '1w', '1m'], duration)`, products[i].ID)
		if err != nil {
			return nil, err
		}
		if products[i].Prices, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (AdminPrice, error) {
			var p AdminPrice
			err := r.Scan(&p.Duration, &p.Amount, &p.Enabled, &p.UpdatedAt)
			return p, err
		}); err != nil {
			return nil, err
		}
	}
	return products, nil
}

// SetPrice 设置某商品某租期的价格与是否在售；价格变动写审计（含修改前后）。
func (s *Service) SetPrice(ctx context.Context, actor audit.Actor, productID int64, d Duration, amount int64, enabled bool) (AdminPrice, error) {
	if !d.Valid() {
		return AdminPrice{}, apperr.BadRequest("租期只能是 1d、1w 或 1m")
	}
	if amount <= 0 || amount > MaxPrice {
		return AdminPrice{}, apperr.BadRequest("价格需在 0.01 元到 100 万元之间")
	}
	var out AdminPrice
	err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		var before map[string]any
		var oldAmount int64
		var oldEnabled bool
		err := tx.QueryRow(ctx, `SELECT amount, enabled FROM prices WHERE product_id = $1 AND duration = $2 FOR UPDATE`,
			productID, string(d)).Scan(&oldAmount, &oldEnabled)
		switch {
		case err == nil:
			before = map[string]any{"amount": oldAmount, "enabled": oldEnabled}
		case !db.IsNoRows(err):
			return err
		}
		now := s.clock.Now()
		err = tx.QueryRow(ctx, `INSERT INTO prices (product_id, duration, amount, enabled, updated_at) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (product_id, duration) DO UPDATE SET amount = EXCLUDED.amount, enabled = EXCLUDED.enabled, updated_at = EXCLUDED.updated_at
			RETURNING duration, amount, enabled, updated_at`, productID, string(d), amount, enabled, now).
			Scan(&out.Duration, &out.Amount, &out.Enabled, &out.UpdatedAt)
		if db.IsForeignKeyViolation(err) {
			return apperr.ErrNotFound
		}
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("price.update", "product", strconv.FormatInt(productID, 10), map[string]any{
			"duration": d, "before": before, "after": map[string]any{"amount": amount, "enabled": enabled},
		}))
	})
	return out, err
}

type AdminImage struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Ref         string    `json:"ref"`
	Description string    `json:"description"`
	Enabled     bool      `json:"enabled"`
	Sort        int       `json:"sort"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type ImageInput struct {
	Name        string
	Ref         string
	Description string
	Enabled     bool
	Sort        int
}

func normalizeImage(in ImageInput) (ImageInput, error) {
	in.Name = strings.TrimSpace(in.Name)
	in.Ref = strings.TrimSpace(in.Ref)
	in.Description = strings.TrimSpace(in.Description)
	if in.Name == "" || utf8.RuneCountInString(in.Name) > 64 {
		return in, apperr.BadRequest("请填写镜像名称（最多 64 个字符）")
	}
	if in.Ref == "" || len(in.Ref) > 256 || strings.IndexFunc(in.Ref, unicode.IsSpace) >= 0 {
		return in, apperr.BadRequest("镜像地址不能为空且不能包含空白字符")
	}
	// 镜像地址会作为参数传给 docker，首字符必须是小写字母或数字，防止 "--privileged" 之类的选项注入
	if c := in.Ref[0]; !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
		return in, apperr.BadRequest("镜像地址必须以小写字母或数字开头")
	}
	if utf8.RuneCountInString(in.Description) > 500 {
		return in, apperr.BadRequest("镜像说明最多 500 个字符")
	}
	if in.Sort < -10000 || in.Sort > 10000 {
		return in, apperr.BadRequest("排序值需在 -10000～10000 之间")
	}
	return in, nil
}

const adminImageCols = `id, name, ref, description, enabled, sort, created_at, updated_at`

func scanAdminImage(row pgx.Row) (AdminImage, error) {
	var i AdminImage
	err := row.Scan(&i.ID, &i.Name, &i.Ref, &i.Description, &i.Enabled, &i.Sort, &i.CreatedAt, &i.UpdatedAt)
	if db.IsNoRows(err) {
		return AdminImage{}, apperr.ErrNotFound
	}
	return i, err
}

func (s *Service) AdminImages(ctx context.Context) ([]AdminImage, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+adminImageCols+` FROM images ORDER BY sort, id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (AdminImage, error) { return scanAdminImage(r) })
}

func (s *Service) CreateImage(ctx context.Context, actor audit.Actor, in ImageInput) (AdminImage, error) {
	in, err := normalizeImage(in)
	if err != nil {
		return AdminImage{}, err
	}
	var img AdminImage
	err = db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		now := s.clock.Now()
		var err error
		if img, err = scanAdminImage(tx.QueryRow(ctx, `INSERT INTO images (name, ref, description, enabled, sort, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $6) RETURNING `+adminImageCols, in.Name, in.Ref, in.Description, in.Enabled, in.Sort, now)); err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("image.create", "image", strconv.FormatInt(img.ID, 10), in))
	})
	return img, err
}

// ErrImageRefInUse：草稿或已发布的应用版本引用了该镜像时不能改镜像地址（部署按版本固定镜像，规格 D10）；停用仍允许。
var ErrImageRefInUse = apperr.New(apperr.ErrInvalidState.Code, "该镜像已被应用版本引用，不能修改镜像地址；请新建镜像，或先下线引用它的版本", apperr.ErrInvalidState.Status)

// UpdateImage 修改镜像；被未下线的应用版本引用时不能改 ref（见 ErrImageRefInUse）。
func (s *Service) UpdateImage(ctx context.Context, actor audit.Actor, id int64, in ImageInput) (AdminImage, error) {
	in, err := normalizeImage(in)
	if err != nil {
		return AdminImage{}, err
	}
	var img AdminImage
	err = db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		// 行锁与应用发布时的 FOR SHARE 互斥：检查「被引用」与改 ref 之间不会有新版本发布
		var curRef string
		if err := tx.QueryRow(ctx, `SELECT ref FROM images WHERE id = $1 FOR UPDATE`, id).Scan(&curRef); err != nil {
			if db.IsNoRows(err) {
				return apperr.ErrNotFound
			}
			return err
		}
		if in.Ref != curRef {
			var used bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM app_versions WHERE $1 = ANY(image_ids) AND status <> 'retired')`, id).Scan(&used); err != nil {
				return err
			}
			if used {
				return ErrImageRefInUse
			}
		}
		now := s.clock.Now()
		var err error
		if img, err = scanAdminImage(tx.QueryRow(ctx, `UPDATE images SET name = $2, ref = $3, description = $4, enabled = $5, sort = $6, updated_at = $7
			WHERE id = $1 RETURNING `+adminImageCols, id, in.Name, in.Ref, in.Description, in.Enabled, in.Sort, now)); err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("image.update", "image", strconv.FormatInt(id, 10), in))
	})
	return img, err
}
