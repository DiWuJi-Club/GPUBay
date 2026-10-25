package rental

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/randx"
)

// AdminMigrate 把运行中实例的剩余时长转移到同产品的另一台空闲机器上新建的实例，原实例进入洗机。
// 不扣费、不新建订单；原订单通过 instance_transfers 关联。加锁顺序：原实例 → 新机器 → 指令。
func (s *Service) AdminMigrate(ctx context.Context, actor audit.Actor, instanceID, reason string) (Instance, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return Instance{}, apperr.BadRequest("请填写换机原因")
	}
	now := s.d.Clock.Now()
	var moved Instance
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		old, err := lockInstance(ctx, tx, instanceID)
		if err != nil {
			return err
		}
		if old.State != StateRunning {
			return apperr.ErrInvalidState
		}
		paid := int64(old.Remaining(now) / time.Second)
		if paid <= 0 {
			return apperr.New("invalid_state", "该实例已没有剩余时长，无需换机", 409)
		}
		// 新实例不是预订，开通后从 now 起算 pending_seconds。早开通的预订在 S 之前换机时，已付时长从 S 才开始计，
		// 所以把 now → S 这段（resumeBase）也给新实例，到期不早于 S + 已付时长（终审 M4）；
		// instance_transfers 只记已付的 paid（新实例开通失败按它退款，早开通的免费时段不折现）。
		remaining := int64(old.resumeBase(now).Sub(now)/time.Second) + paid
		ref, err := s.d.Catalog.ImageRef(ctx, tx, old.ImageID)
		if err != nil {
			return err
		}
		spec := pickSpec{ProductID: old.ProductID, Immediate: true}
		spec.Lower, spec.Upper = immediatePeriod(now, remaining)
		hostPub, hostPEM, err := s.d.HostKeyGen()
		if err != nil {
			return err
		}
		id := randx.ID("ins-", 8)
		var newMachine int64
		if _, err := placeOnMachines(ctx, tx, spec, 1, func(sp pgx.Tx, mid int64) error {
			m, err := s.d.Fleet.MarkAllocated(ctx, sp, mid)
			if err != nil {
				return err
			}
			moved, err = scanInstance(sp.QueryRow(ctx, `INSERT INTO instances
				(id, user_id, machine_id, product_id, image_id, state, pending_seconds, ssh_password_enc, password_auth, ssh_host_pubkey, ssh_host_key_enc,
				 auto_renew, auto_renew_duration, group_id, created_at, updated_at)
				VALUES ($1, $2, $3, $4, $5, 'creating', $6, $7, $8, $9, $10, $11, $12, $13, $14, $14) RETURNING `+instanceCols,
				id, old.UserID, m.ID, old.ProductID, old.ImageID, remaining, s.d.Box.Seal([]byte(randx.Password(16))), old.PasswordAuth, hostPub, s.d.Box.Seal(hostPEM),
				old.AutoRenew, string(old.AutoRenewDuration), m.GroupID, now))
			if err != nil {
				return err
			}
			if _, err := insertLease(ctx, sp, m.ID, id, spec.Lower, spec.Upper, "active", now); err != nil {
				return err
			}
			newMachine = m.ID
			return nil
		}); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO instance_ports (instance_id, port, visibility, forced_private, note, created_at, updated_at)
			SELECT $2, port, visibility, forced_private, note, $3, $3 FROM instance_ports WHERE instance_id = $1 ORDER BY port`, old.ID, id, now); err != nil {
			return err
		}
		// 端口已复制（deployment_id NULL），部署复制为新实例的 queued；必须在 beginWipe(old) 之前，否则旧部署会先被标 removed
		if err := s.apps().OnInstanceMigrated(ctx, tx, old.ID, id, now); err != nil {
			return err
		}
		if err := recordEvent(ctx, tx, id, "", StateCreating, "migrated from "+old.ID+": "+reason, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO instance_transfers (from_instance_id, to_instance_id, seconds, reason, admin_id, created_at)
			VALUES ($1, $2, $3, $4, $5, $6)`, old.ID, id, paid, reason, actor.AdminID, now); err != nil {
			return err
		}
		if err := s.enqueueCreate(ctx, tx, moved, ref); err != nil { // moved 即新实例（machine_id = newMachine）
			return err
		}
		if err := s.beginWipe(ctx, tx, old, "migrated to "+id+": "+reason, now); err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("instance.migrate", "instance", old.ID, map[string]any{
			"to_instance": id, "from_machine": old.MachineID, "to_machine": newMachine, "seconds": paid, "pending_seconds": remaining, "reason": reason,
		}))
	})
	return moved, err
}
