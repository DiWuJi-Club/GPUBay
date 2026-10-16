// Package catalog 管理可售商品、租期价格与系统镜像。
package catalog

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
)

type Duration string

const (
	D1d Duration = "1d"
	D1w Duration = "1w"
	D1m Duration = "1m"
)

func (d Duration) Valid() bool { return d.Length() > 0 }

// Length 返回租期时长；1m 按 30 天计。
func (d Duration) Length() time.Duration {
	switch d {
	case D1d:
		return 24 * time.Hour
	case D1w:
		return 7 * 24 * time.Hour
	case D1m:
		return 30 * 24 * time.Hour
	}
	return 0
}

type Product struct {
	ID   int64          `json:"id"`
	Code string         `json:"code"`
	Name string         `json:"name"`
	Spec map[string]any `json:"spec"`
}

type Price struct {
	Duration Duration `json:"duration"`
	Amount   int64    `json:"amount"`
}

type ProductWithPrices struct {
	Product
	Prices []Price `json:"prices"`
}

type Image struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Ref         string `json:"-"`
	Description string `json:"description"`
}

type Service struct {
	pool *pgxpool.Pool
}

func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

func (s *Service) ListProducts(ctx context.Context) ([]ProductWithPrices, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, code, name, spec FROM products WHERE status = 'active' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	products, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ProductWithPrices, error) {
		var p ProductWithPrices
		err := r.Scan(&p.ID, &p.Code, &p.Name, &p.Spec)
		return p, err
	})
	if err != nil {
		return nil, err
	}
	for i := range products {
		rows, err := s.pool.Query(ctx, `SELECT duration, amount FROM prices
			WHERE product_id = $1 AND enabled
			ORDER BY array_position(ARRAY['1d', '1w', '1m'], duration)`, products[i].ID)
		if err != nil {
			return nil, err
		}
		products[i].Prices, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Price, error) {
			var p Price
			err := r.Scan(&p.Duration, &p.Amount)
			return p, err
		})
		if err != nil {
			return nil, err
		}
	}
	return products, nil
}

// Quote 返回指定商品与租期的当前价格（分）。
func (s *Service) Quote(ctx context.Context, q db.Querier, productID int64, d Duration) (int64, error) {
	if !d.Valid() {
		return 0, apperr.BadRequest("租期不正确")
	}
	var amount int64
	err := q.QueryRow(ctx, `SELECT p.amount FROM prices p JOIN products pr ON pr.id = p.product_id
		WHERE p.product_id = $1 AND p.duration = $2 AND p.enabled AND pr.status = 'active'`, productID, string(d)).Scan(&amount)
	if db.IsNoRows(err) {
		return 0, apperr.BadRequest("该规格暂不支持此租期")
	}
	return amount, err
}

func (s *Service) GetImage(ctx context.Context, q db.Querier, id int64) (Image, error) {
	var img Image
	err := q.QueryRow(ctx, `SELECT id, name, ref, description FROM images WHERE id = $1 AND enabled`, id).
		Scan(&img.ID, &img.Name, &img.Ref, &img.Description)
	if db.IsNoRows(err) {
		return Image{}, apperr.BadRequest("镜像不存在或已下架")
	}
	return img, err
}

func (s *Service) ListImages(ctx context.Context) ([]Image, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, name, ref, description FROM images WHERE enabled ORDER BY sort, id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Image, error) {
		var img Image
		err := r.Scan(&img.ID, &img.Name, &img.Ref, &img.Description)
		return img, err
	})
}

// ImageRef 返回镜像地址，不要求镜像仍在售（换机时沿用用户原来的镜像）。
func (s *Service) ImageRef(ctx context.Context, q db.Querier, id int64) (string, error) {
	var ref string
	err := q.QueryRow(ctx, `SELECT ref FROM images WHERE id = $1`, id).Scan(&ref)
	if db.IsNoRows(err) {
		return "", apperr.ErrNotFound
	}
	return ref, err
}
