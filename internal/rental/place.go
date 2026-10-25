package rental

import (
	"context"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/platform/db"
)

type PlaceRequest struct {
	UserID    int64
	ProductID int64
	ImageID   int64
	Duration  catalog.Duration
	// IdempotencyKey 非空时，同一用户重复提交同一键只会生成一笔订单，重试返回首次创建的订单与实例。
	IdempotencyKey string
	// 计划 7：开通时预装应用（见 BatchRequest.AppID / AppEnv）
	AppID  *int64
	AppEnv map[string]string
	// 计划 11：预装工作流（见 BatchRequest.AppWorkflowIDs）
	AppWorkflowIDs []int64
}

// Place 新购一台整机（旧接口 POST /orders）：等价于一台、一个单位、立即开通、不建批次的 PlaceBatch；
// 幂等键原样写在订单上，回放行为与计划 1 完全相同。
func (s *Service) Place(ctx context.Context, req PlaceRequest) (Order, Instance, error) {
	res, err := s.placeBatch(ctx, BatchRequest{UserID: req.UserID, ProductID: req.ProductID, ImageID: req.ImageID, Count: 1,
		Duration: req.Duration, Quantity: 1, IdempotencyKey: req.IdempotencyKey, AppID: req.AppID, AppEnv: req.AppEnv, AppWorkflowIDs: req.AppWorkflowIDs}, false)
	if err != nil {
		return Order{}, Instance{}, err
	}
	return res.Orders[0], res.Instances[0], nil
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
