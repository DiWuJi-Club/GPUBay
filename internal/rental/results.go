package rental

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/wallet"
)

// HandleCommandResult 处理 Agent 对一条指令的回报。重复回报、已取消指令的回报、与当前状态不符的回报都会被忽略。
func (s *Service) HandleCommandResult(ctx context.Context, commandID string, ok bool, result string) error {
	return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		// 先锁实例、再完成指令：与 Release/WipeStale/FailStuckCreating 的加锁顺序一致，避免死锁。
		instanceID, err := s.d.Fleet.CommandInstanceID(ctx, tx, commandID)
		if err != nil {
			return err
		}
		var inst Instance
		if instanceID != "" {
			inst, err = lockInstance(ctx, tx, instanceID)
			if err != nil && !errors.Is(err, apperr.ErrNotFound) { // 孤儿清理的实例可能从未在平台登记
				return err
			}
		}
		cmd, applied, err := s.d.Fleet.CompleteCommand(ctx, tx, commandID, ok, result)
		if err != nil {
			return err
		}
		if !applied || cmd.InstanceID == "" {
			return nil
		}
		now := s.d.Clock.Now()
		switch cmd.Type {
		case fleet.CmdCreateInstance:
			if inst.State != StateCreating {
				return nil
			}
			if ok {
				return s.startLease(ctx, tx, inst, StateCreating, "agent: created", now)
			}
			return s.failCreating(ctx, tx, inst, "开通失败: "+result, now)
		case fleet.CmdStartInstance:
			if inst.State != StateStopped {
				return nil
			}
			if ok {
				return s.startLease(ctx, tx, inst, StateStopped, "agent: started", now)
			}
			return s.d.Fleet.RaiseAlert(ctx, tx, fleet.Alert{Level: "critical", Kind: "start_failed",
				MachineID: inst.MachineID, InstanceID: inst.ID, Message: result})
		case fleet.CmdStopInstance:
			if !ok {
				return s.d.Fleet.RaiseAlert(ctx, tx, fleet.Alert{Level: "critical", Kind: "stop_failed",
					MachineID: inst.MachineID, InstanceID: inst.ID, Message: result})
			}
		case fleet.CmdWipeInstance:
			// 非 wiping，或者指令不在实例所属机器上（别的机器上报了该实例的容器）：都是孤儿清理，
			// 不改实例状态、不释放机器——否则原机器没洗就会被放给下一个租户。
			if inst.State != StateWiping || cmd.MachineID != inst.MachineID {
				if ok {
					return nil
				}
				m, err := s.d.Fleet.Get(ctx, tx, cmd.MachineID)
				if err != nil {
					return err
				}
				// 对账入队的孤儿清理失败：机器上留着别人的容器与数据盘，必须让运维知道
				return s.d.Fleet.RaiseAlertOnce(ctx, tx, fleet.Alert{Level: "critical", Kind: "orphan_wipe_failed",
					MachineID: cmd.MachineID, InstanceID: cmd.InstanceID, Message: AlertOrphanWipeFailed(m.Serial, cmd.InstanceID)})
			}
			if !ok {
				if err := s.d.Fleet.MarkFaulty(ctx, tx, inst.MachineID); err != nil {
					return err
				}
				return s.d.Fleet.RaiseAlert(ctx, tx, fleet.Alert{Level: "critical", Kind: "wipe_failed",
					MachineID: inst.MachineID, InstanceID: inst.ID, Message: result})
			}
			if _, err := tx.Exec(ctx, `UPDATE instances SET state = 'released', ssh_host_key_enc = NULL, updated_at = $2 WHERE id = $1`, inst.ID, now); err != nil {
				return err
			}
			if err := recordEvent(ctx, tx, inst.ID, StateWiping, StateReleased, "agent: wiped", now); err != nil {
				return err
			}
			if err := s.refreshLease(ctx, tx, inst.ID, now); err != nil {
				return err
			}
			return s.d.Fleet.Release(ctx, tx, inst.MachineID)
		}
		return nil
	})
}

// startLease 把实例转为 running，并从此刻开始计时 pending_seconds。
func (s *Service) startLease(ctx context.Context, tx pgx.Tx, inst Instance, from, reason string, now time.Time) error {
	expires := now.Add(time.Duration(inst.PendingSeconds) * time.Second)
	r24, r1 := reminderFlags(expires.Sub(now))
	if _, err := tx.Exec(ctx, `UPDATE instances SET state = 'running', expires_at = $2, pending_seconds = 0,
			stopped_at = NULL, reminded_24h = $3, reminded_1h = $4, updated_at = $5 WHERE id = $1`,
		inst.ID, expires, r24, r1, now); err != nil {
		return err
	}
	if err := recordEvent(ctx, tx, inst.ID, from, StateRunning, reason, now); err != nil {
		return err
	}
	return s.refreshLease(ctx, tx, inst.ID, now)
}

// failCreating 开通失败：实例失败、退款、机器隔离为故障、取消剩余指令并告警。
func (s *Service) failCreating(ctx context.Context, tx pgx.Tx, inst Instance, reason string, now time.Time) error {
	if _, err := tx.Exec(ctx, `UPDATE instances SET state = 'failed', pending_seconds = 0, ssh_host_key_enc = NULL, updated_at = $2 WHERE id = $1`, inst.ID, now); err != nil {
		return err
	}
	if err := recordEvent(ctx, tx, inst.ID, StateCreating, StateFailed, reason, now); err != nil {
		return err
	}
	if err := s.refreshLease(ctx, tx, inst.ID, now); err != nil {
		return err
	}
	if err := s.refundAll(ctx, tx, inst, now); err != nil {
		return err
	}
	if err := s.d.Fleet.MarkFaulty(ctx, tx, inst.MachineID); err != nil {
		return err
	}
	if err := s.d.Fleet.CancelCommands(ctx, tx, inst.ID, "instance failed"); err != nil {
		return err
	}
	return s.d.Fleet.RaiseAlert(ctx, tx, fleet.Alert{Level: "critical", Kind: "create_failed",
		MachineID: inst.MachineID, InstanceID: inst.ID, Message: reason})
}

// refundAll 开通失败退款：退还该实例自己所有已支付订单（新购与续费）尚未退还的部分；
// 若它是换机得到的新实例，再退还转入的剩余时长（以换机记录的秒数为上限，见 refundTransferred）。
// inst 须为加锁时读到的实例快照（state = creating，含 pending_seconds）。
func (s *Service) refundAll(ctx context.Context, tx pgx.Tx, inst Instance, now time.Time) error {
	rows, err := tx.Query(ctx, `SELECT id, amount - refunded_amount, duration FROM orders
		WHERE instance_id = $1 AND status IN ('paid', 'partially_refunded') ORDER BY created_at, id FOR UPDATE`, inst.ID)
	if err != nil {
		return err
	}
	type paid struct {
		id       string
		amount   int64
		duration catalog.Duration
	}
	orders, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (paid, error) {
		var p paid
		err := r.Scan(&p.id, &p.amount, &p.duration)
		return p, err
	})
	if err != nil {
		return err
	}
	var ownSecs int64
	for _, o := range orders {
		ownSecs += seconds(o.duration)
		if o.amount > 0 {
			if _, err := s.d.Wallet.Apply(ctx, tx, wallet.Entry{
				UserID: inst.UserID, Amount: o.amount, Kind: wallet.KindRefund, RefType: "order", RefID: o.id,
				Remark: "开通失败退款 " + inst.ID,
			}); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE orders SET status = 'refunded', refunded_amount = amount, refund_count = refund_count + 1,
			updated_at = $2 WHERE id = $1`, o.id, now); err != nil {
			return err
		}
	}
	// pending_seconds 里还可能有管理员赠送的时长（AdminExtend），这部分不能按钱退：
	// 转入时长以换机时记录的秒数为上限；不是换机得到的实例则没有转入时长。
	var transferSecs int64
	err = tx.QueryRow(ctx, `SELECT seconds FROM instance_transfers WHERE to_instance_id = $1`, inst.ID).Scan(&transferSecs)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	return s.refundTransferred(ctx, tx, inst, min(max(inst.PendingSeconds-ownSecs, 0), transferSecs), now)
}

// refundTransferred 换机得到的新实例开通失败：按转入的剩余时长 secs × 前身实例最近一笔可退订单的单价
// （不超过前身订单的可退上限）从新到旧退还前身实例的订单。非换机实例不做任何事。
// 前身实例的订单在最新实例（inst，调用方已加锁）的行锁保护下加锁修改。
func (s *Service) refundTransferred(ctx context.Context, tx pgx.Tx, inst Instance, secs int64, now time.Time) error {
	chain, err := instanceChain(ctx, tx, inst.ID)
	if err != nil {
		return err
	}
	preds := slices.DeleteFunc(chain, func(id string) bool { return id == inst.ID })
	if len(preds) == 0 {
		return nil
	}
	orders, err := refundableOrders(ctx, tx, preds, true)
	if err != nil {
		return err
	}
	snap := inst
	snap.PendingSeconds = secs
	q := quote(snap, orders, now)
	_, err = s.refundAcross(ctx, tx, inst.UserID, orders, q.Suggested, 0, "开通失败退款 "+inst.ID, now)
	return err
}
