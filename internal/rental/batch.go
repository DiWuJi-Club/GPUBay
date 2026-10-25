package rental

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/randx"
	"spark-cloud/internal/platform/tz"
)

// BatchRequest 一次下单：count 台、同一商品 / 镜像 / 租期；StartAt 为 nil 表示立即开通，否则为预订（整点，北京时间）。
type BatchRequest struct {
	UserID            int64
	ProductID         int64
	ImageID           int64
	GroupID           *int64 // nil = 不限机组
	MachineID         *int64 // 指定机器（Count 须为 1）
	Count             int
	Duration          catalog.Duration
	Quantity          int // 租期单位数，0 视为 1
	StartAt           *time.Time
	AutoRenew         bool
	AutoRenewDuration catalog.Duration // 空 = Duration
	IdempotencyKey    string
	// 计划 7：开通时预装应用（nil = 不预装）；每台实例在同一事务里插入一条 queued 部署，校验失败整单不下
	AppID  *int64
	AppEnv map[string]string
	// 计划 11：随应用一并预装的 ComfyUI 工作流（须同时给 AppID；校验在 PrepareInstall）
	AppWorkflowIDs []int64
}

type Batch struct {
	ID        string    `json:"id"`
	UserID    int64     `json:"-"`
	Count     int       `json:"count"`
	CreatedAt time.Time `json:"created_at"`
}

// BatchResult：Orders[i] 对应 Instances[i]；Batch 在旧接口（POST /orders）路径为 nil。
type BatchResult struct {
	Batch     *Batch
	Orders    []Order
	Instances []Instance
}

// validateBatch 校验并规范化请求（规格 §6.1、裁定 B1），返回总秒数。
func validateBatch(req *BatchRequest, now time.Time) (int64, error) {
	if !req.Duration.Valid() {
		return 0, apperr.BadRequest("租期不正确")
	}
	if req.Quantity == 0 {
		req.Quantity = 1
	}
	if req.Quantity < 1 || req.Quantity > MaxQuantity(req.Duration) {
		return 0, apperr.ErrBookingTooLong
	}
	if req.Count < 1 || req.Count > MaxMachinesPerOrder {
		return 0, apperr.ErrCountInvalid
	}
	if req.MachineID != nil && req.Count != 1 {
		return 0, apperr.BadRequest("指定机器时只能租用 1 台")
	}
	if req.AutoRenewDuration == "" {
		req.AutoRenewDuration = req.Duration
	}
	if !req.AutoRenewDuration.Valid() {
		return 0, apperr.BadRequest("自动续费档位不正确")
	}
	if err := checkIdempotencyKey(req.IdempotencyKey); err != nil {
		return 0, err
	}
	if req.AppID == nil && len(req.AppEnv) > 0 {
		return 0, apperr.BadRequest("未选择预装应用时不能填写应用变量")
	}
	if req.AppID == nil && len(req.AppWorkflowIDs) > 0 {
		return 0, apperr.BadRequest("预装工作流需要同时选择 ComfyUI 应用")
	}
	if req.StartAt != nil {
		start := req.StartAt.UTC()
		if !tz.IsHour(start) {
			return 0, apperr.ErrBookingNotHour
		}
		if start.Before(now.Add(MinBookingLead - BookingLeadTolerance)) {
			return 0, apperr.ErrBookingTooSoon
		}
		if start.After(now.Add(MaxBookingAhead)) {
			return 0, apperr.ErrBookingTooFar
		}
		req.StartAt = &start
	}
	return seconds(req.Duration) * int64(req.Quantity), nil
}

// PlaceBatch 一次租 Count 台（规格 §6）：整个事务要么全部成功要么全部失败；建批次行；子订单键为 "<key>#<n>"。
func (s *Service) PlaceBatch(ctx context.Context, req BatchRequest) (BatchResult, error) {
	return s.placeBatch(ctx, req, true)
}

func (s *Service) placeBatch(ctx context.Context, req BatchRequest, withBatch bool) (BatchResult, error) {
	now := s.d.Clock.Now()
	secs, err := validateBatch(&req, now)
	if err != nil {
		return BatchResult{}, err
	}
	if err := s.d.Users.CanRent(ctx, req.UserID); err != nil {
		return BatchResult{}, err
	}
	var out BatchResult
	run := func() error {
		out = BatchResult{}
		return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
			if req.IdempotencyKey != "" {
				if err := lockIdempotency(ctx, tx, req.UserID, req.IdempotencyKey); err != nil {
					return err
				}
				prev, found, err := s.replayByKey(ctx, tx, req.UserID, req.IdempotencyKey, withBatch)
				if found || err != nil {
					out = prev
					return err
				}
			}
			if req.StartAt != nil {
				if err := s.reserveSlots(ctx, tx, req.UserID, req.Count); err != nil {
					return err
				}
			}
			img, err := s.d.Catalog.GetImage(ctx, tx, req.ImageID)
			if err != nil {
				return err
			}
			// 预装校验在挑机器、扣款之前：应用 / 镜像不兼容 / 变量错误整单失败（4xx）
			var plan InstallPlan
			if req.AppID != nil {
				if plan, err = s.apps().PrepareInstall(ctx, tx, *req.AppID, req.ImageID, req.AppEnv, req.AppWorkflowIDs); err != nil {
					return err
				}
			}
			if req.GroupID != nil {
				var visible bool
				if err := tx.QueryRow(ctx, `SELECT visible FROM machine_groups WHERE id = $1`, *req.GroupID).Scan(&visible); db.IsNoRows(err) || (err == nil && !visible) {
					return apperr.ErrGroupNotFound
				} else if err != nil {
					return err
				}
			}
			spec := pickSpec{ProductID: req.ProductID, GroupID: req.GroupID, MachineID: req.MachineID, Immediate: req.StartAt == nil}
			if spec.Immediate {
				spec.Lower, spec.Upper = immediatePeriod(now, secs)
			} else {
				spec.Lower, spec.Upper = bookingPeriod(*req.StartAt, secs)
			}
			// 批次行先插（instances.batch_id / orders.batch_id 外键引用它）；整单失败时随事务回滚
			var batchID *string
			if withBatch {
				id := randx.ID("bat-", 12)
				batchID = &id
				if _, err := tx.Exec(ctx, `INSERT INTO rental_batches (id, user_id, count, idempotency_key, created_at) VALUES ($1, $2, $3, NULLIF($4, ''), $5)`,
					id, req.UserID, req.Count, req.IdempotencyKey, now); err != nil {
					return err
				}
				out.Batch = &Batch{ID: id, UserID: req.UserID, Count: req.Count, CreatedAt: now}
			}
			var insts []Instance
			_, err = placeOnMachines(ctx, tx, spec, req.Count, func(sp pgx.Tx, mid int64) error {
				if spec.Immediate {
					if _, err := s.d.Fleet.MarkAllocated(ctx, sp, mid); err != nil {
						return err
					}
				}
				inst, err := s.insertInstance(ctx, sp, req, mid, secs, batchID, now)
				if err != nil {
					return err
				}
				status := "reserved"
				if spec.Immediate {
					status = "active"
				}
				if _, err := insertLease(ctx, sp, mid, inst.ID, spec.Lower, spec.Upper, status, now); err != nil {
					return err
				}
				if req.AppID != nil {
					if err := s.apps().QueueInstall(ctx, sp, inst, plan, now); err != nil {
						return err
					}
				}
				insts = append(insts, inst)
				return nil
			})
			if err != nil {
				if req.MachineID != nil && codeIs(err, "no_capacity") {
					return apperr.ErrMachineUnavailable
				}
				return err
			}
			for i, inst := range insts {
				key := req.IdempotencyKey
				if withBatch && key != "" {
					key = fmt.Sprintf("%s#%d", key, i+1)
				}
				ord, err := s.charge(ctx, tx, req.UserID, "new", inst.ID, req.ProductID, req.Duration, req.Quantity, false, batchID, key, now)
				if err != nil {
					return err
				}
				reason := "order " + ord.ID
				if req.StartAt != nil {
					reason += " scheduled " + tz.Format(*req.StartAt)
				}
				if err := recordEvent(ctx, tx, inst.ID, "", inst.State, reason, now); err != nil {
					return err
				}
				if spec.Immediate {
					if err := s.enqueueCreate(ctx, tx, inst, img.Ref); err != nil {
						return err
					}
				}
				out.Orders = append(out.Orders, ord)
			}
			// 顺序唯一：按实例 ID 排序（loadBatch 回放也按 id），避免「首单按挑机顺序、回放按 id」不一致
			sort.Slice(insts, func(i, j int) bool { return insts[i].ID < insts[j].ID })
			sort.Slice(out.Orders, func(i, j int) bool { return out.Orders[i].InstanceID < out.Orders[j].InstanceID })
			out.Instances = insts
			return nil
		})
	}
	err = withRetry(run)
	if isIdempotencyViolation(err) || isBatchIdempotencyViolation(err) {
		// 并发的重复请求已先提交：本事务已回滚，返回先提交的那笔。
		prev, found, ferr := s.replayByKey(ctx, s.d.Pool, req.UserID, req.IdempotencyKey, withBatch)
		if ferr != nil {
			return BatchResult{}, ferr
		}
		if found {
			return prev, nil
		}
	}
	if err != nil {
		return BatchResult{}, err
	}
	return out, nil
}

func codeIs(err error, code string) bool {
	ae := apperr.As(err)
	return ae != nil && ae.Code == code
}

// insertInstance 插入一台新实例（即时 creating / 预订 reserved），密码与主机密钥在此生成。
func (s *Service) insertInstance(ctx context.Context, q db.Querier, req BatchRequest, machineID int64, secs int64, batchID *string, now time.Time) (Instance, error) {
	hostPub, hostPEM, err := s.d.HostKeyGen()
	if err != nil {
		return Instance{}, err
	}
	state := StateCreating
	if req.StartAt != nil {
		state = StateReserved
	}
	id := randx.ID("ins-", 8)
	return scanInstance(q.QueryRow(ctx, `INSERT INTO instances
		(id, user_id, machine_id, product_id, image_id, state, pending_seconds, ssh_password_enc, ssh_host_pubkey, ssh_host_key_enc,
		 scheduled_start_at, group_id, batch_id, auto_renew, auto_renew_duration, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $16) RETURNING `+instanceCols,
		id, req.UserID, machineID, req.ProductID, req.ImageID, state, secs, s.d.Box.Seal([]byte(randx.Password(16))), hostPub, s.d.Box.Seal(hostPEM),
		req.StartAt, req.GroupID, batchID, req.AutoRenew, string(req.AutoRenewDuration), now))
}

// reserveSlots 预订分支：按用户串行化（咨询锁）后检查未开通预订上限（裁定 B1：16 台）。
func (s *Service) reserveSlots(ctx context.Context, tx pgx.Tx, userID int64, count int) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "booking:"+strconv.FormatInt(userID, 10)); err != nil {
		return err
	}
	var open int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM instances WHERE user_id = $1 AND state = 'reserved'`, userID).Scan(&open); err != nil {
		return err
	}
	if open+count > MaxOpenReservationsPerUser {
		return apperr.ErrBookingLimit
	}
	return nil
}

// replayByKey 按幂等键回放（调用方已持咨询锁或在事务外兜底）：命中批次 → 只有 /bookings 形态可回放；
// 命中订单 → 只有 /orders 形态且是无批次的新购单可回放；其它组合冲突。
func (s *Service) replayByKey(ctx context.Context, q db.Querier, userID int64, key string, withBatch bool) (BatchResult, bool, error) {
	if b, found, err := findBatchByKey(ctx, q, userID, key); err != nil {
		return BatchResult{}, false, err
	} else if found {
		if !withBatch {
			return BatchResult{}, true, errIdempotencyConflict
		}
		res, err := s.loadBatch(ctx, q, b)
		return res, true, err
	}
	ord, found, err := findByIdempotencyKey(ctx, q, userID, key)
	if err != nil || !found {
		return BatchResult{}, false, err
	}
	if withBatch || ord.Kind != "new" || ord.BatchID != nil {
		return BatchResult{}, true, errIdempotencyConflict
	}
	o, inst, err := s.replayPlace(ctx, q, ord)
	if err != nil {
		return BatchResult{}, true, err
	}
	return BatchResult{Orders: []Order{o}, Instances: []Instance{inst}}, true, nil
}

// loadBatch 返回批次的订单与实例当前状态（按实例 ID 对齐）。
func (s *Service) loadBatch(ctx context.Context, q db.Querier, b Batch) (BatchResult, error) {
	rows, err := q.Query(ctx, `SELECT `+orderCols+` FROM orders WHERE batch_id = $1 AND kind = 'new' ORDER BY instance_id`, b.ID)
	if err != nil {
		return BatchResult{}, err
	}
	orders, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Order, error) { return scanOrder(r) })
	if err != nil {
		return BatchResult{}, err
	}
	insts, err := collectInstances(q.Query(ctx, `SELECT `+instanceCols+` FROM instances WHERE batch_id = $1 ORDER BY id`, b.ID))
	if err != nil {
		return BatchResult{}, err
	}
	if len(orders) != len(insts) {
		return BatchResult{}, errors.New("batch orders and instances out of sync")
	}
	return BatchResult{Batch: &b, Orders: orders, Instances: insts}, nil
}

// GetBatch 返回用户自己的批次。
func (s *Service) GetBatch(ctx context.Context, userID int64, id string) (BatchResult, error) {
	var b Batch
	err := s.d.Pool.QueryRow(ctx, `SELECT id, user_id, count, created_at FROM rental_batches WHERE id = $1 AND user_id = $2`, id, userID).
		Scan(&b.ID, &b.UserID, &b.Count, &b.CreatedAt)
	if db.IsNoRows(err) {
		return BatchResult{}, apperr.ErrNotFound
	}
	if err != nil {
		return BatchResult{}, err
	}
	return s.loadBatch(ctx, s.d.Pool, b)
}
