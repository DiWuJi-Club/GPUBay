package rental

import (
	"context"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/randx"
)

type PlaceRequest struct {
	UserID    int64
	ProductID int64
	ImageID   int64
	Duration  catalog.Duration
	// IdempotencyKey 非空时，同一用户重复提交同一键只会生成一笔订单，重试返回首次创建的订单与实例。
	IdempotencyKey string
}

// Place 新购一台整机：同一事务内分配机器、创建实例、扣款建单、写入开通指令。
// 租期从实例进入 running 时开始计时，在此之前记为 pending_seconds。
func (s *Service) Place(ctx context.Context, req PlaceRequest) (Order, Instance, error) {
	if !req.Duration.Valid() {
		return Order{}, Instance{}, apperr.BadRequest("租期不正确")
	}
	if err := checkIdempotencyKey(req.IdempotencyKey); err != nil {
		return Order{}, Instance{}, err
	}
	if err := s.d.Users.CanRent(ctx, req.UserID); err != nil {
		return Order{}, Instance{}, err
	}
	now := s.d.Clock.Now()
	var ord Order
	var inst Instance
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		if req.IdempotencyKey != "" {
			prev, found, err := lockIdempotencyKey(ctx, tx, req.UserID, req.IdempotencyKey)
			if err != nil {
				return err
			}
			if found {
				ord, inst, err = s.replayPlace(ctx, tx, prev)
				return err
			}
		}
		img, err := s.d.Catalog.GetImage(ctx, tx, req.ImageID)
		if err != nil {
			return err
		}
		m, err := s.d.Fleet.Allocate(ctx, tx, req.ProductID)
		if err != nil {
			return err
		}
		id := randx.ID("ins-", 8)
		inst, err = scanInstance(tx.QueryRow(ctx, `INSERT INTO instances
			(id, user_id, machine_id, product_id, image_id, state, pending_seconds, ssh_password_enc, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, 'creating', $6, $7, $8, $8) RETURNING `+instanceCols,
			id, req.UserID, m.ID, req.ProductID, img.ID, seconds(req.Duration), s.d.Box.Seal([]byte(randx.Password(16))), now))
		if err != nil {
			return err
		}
		if err := openLease(ctx, tx, inst, now); err != nil {
			return err
		}
		if ord, err = s.charge(ctx, tx, req.UserID, "new", id, req.ProductID, req.Duration, req.IdempotencyKey, now); err != nil {
			return err
		}
		if err := recordEvent(ctx, tx, id, "", StateCreating, "order "+ord.ID, now); err != nil {
			return err
		}
		_, err = s.d.Fleet.Enqueue(ctx, tx, m.ID, id, fleet.CmdCreateInstance, fleet.CreateInstancePayload{ImageRef: img.Ref})
		return err
	})
	if isIdempotencyViolation(err) {
		// 并发的重复请求已先提交：本事务已回滚，返回先提交的那笔订单。
		prev, found, ferr := findByIdempotencyKey(ctx, s.d.Pool, req.UserID, req.IdempotencyKey)
		if ferr != nil {
			return Order{}, Instance{}, ferr
		}
		if found {
			return s.replayPlace(ctx, s.d.Pool, prev)
		}
	}
	if err != nil {
		return Order{}, Instance{}, err
	}
	return ord, inst, nil
}

// replayPlace 返回幂等键对应的已有新购订单及其实例的当前状态；键被续费订单占用时报冲突。
func (s *Service) replayPlace(ctx context.Context, q db.Querier, prev Order) (Order, Instance, error) {
	if prev.Kind != "new" {
		return Order{}, Instance{}, errIdempotencyConflict
	}
	inst, err := scanInstance(q.QueryRow(ctx, `SELECT `+instanceCols+` FROM instances WHERE id = $1`, prev.InstanceID))
	if err != nil {
		return Order{}, Instance{}, err
	}
	return prev, inst, nil
}
