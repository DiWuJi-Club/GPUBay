package referral

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/wallet"
)

func pgCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// withRetry 对死锁检测（40P01）与序列化失败（40001）把整个事务重试一次。
func withRetry(fn func() error) error {
	err := fn()
	if c := pgCode(err); c == "40P01" || c == "40001" {
		err = fn()
	}
	return err
}

// ReleaseMaxPages：每轮最多翻 ReleaseMaxPages 页（每页 ReleaseBatch 行）。测试可经 release_export_test.go 改小。
const ReleaseMaxPages = 10

var releaseMaxPages = ReleaseMaxPages

type releaseOutcome int

const (
	releaseSkipped releaseOutcome = iota // 门槛不满足 / 转搁置 / 已被别人处理：本轮不动
	releaseDone                          // settled 或 void
	releaseRevoked                       // 同身份复查命中，绑定撤销、本行作废
)

// fpCache：一轮之内按邀请人缓存「邀请人 + 其未撤销被邀请人」的 verified 指纹（user → fps）。
// 同身份撤销提交后从中删去该被邀请人（已撤销的不再参与比较）。轮内新增的绑定 / 实名不在缓存里，由 OnKYCVerified 与下一轮兜底。
type fpCache map[int64]map[int64][]string

// Release 发放到期的待发放返利（规格 §4.4），返回本轮置为 settled / void 的行数。单行失败记日志并继续，全部错误合并返回。
// 候选在 SQL 里预筛「当前可发」（绑定 active、邀请人账户 active、邀请人 active 且已实名），按 (settle_after, order_id) 游标翻页，
// 每页 ReleaseBatch 行、每轮最多 releaseMaxPages 页：锁内复核跳过的行不会挡住后面可发的行。锁内复核仍是权威判定。
func (s *Service) Release(ctx context.Context) (int, error) {
	now := s.d.Clock.Now()
	type cand struct {
		order            string
		inviter, invitee int64
		settleAfter      time.Time
	}
	cache := fpCache{}
	released := 0
	var errs []error
	var afterAt *time.Time
	afterOrder := ""
	for page := 0; page < releaseMaxPages; page++ {
		// 只读 users 的状态列做预筛（权威判定仍经 UserLookup，在锁内）。
		rows, err := s.d.Pool.Query(ctx, `SELECT r.order_id, r.inviter_id, r.invitee_id, r.settle_after FROM referral_rewards r
			JOIN referral_bindings b ON b.id = r.binding_id AND b.status = 'active'
			JOIN referral_accounts a ON a.user_id = r.inviter_id AND a.status = 'active'
			JOIN users u ON u.id = r.inviter_id AND u.status = 'active' AND u.kyc_status = 'verified'
			WHERE r.status = 'pending' AND r.settle_after <= $1 AND ($2::timestamptz IS NULL OR (r.settle_after, r.order_id) > ($2, $3))
			ORDER BY r.settle_after, r.order_id LIMIT $4`, now, afterAt, afterOrder, ReleaseBatch)
		if err != nil {
			errs = append(errs, err)
			break
		}
		list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (cand, error) {
			var c cand
			err := r.Scan(&c.order, &c.inviter, &c.invitee, &c.settleAfter)
			return c, err
		})
		if err != nil {
			errs = append(errs, err)
			break
		}
		for _, c := range list {
			out := releaseSkipped
			err := withRetry(func() error {
				out = releaseSkipped
				return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
					var err error
					out, err = s.releaseOne(ctx, tx, now, cache, c.order, c.inviter, c.invitee)
					return err
				})
			})
			if err != nil {
				s.d.Log.Error("referral: release failed", "order", c.order, "err", err)
				errs = append(errs, fmt.Errorf("release %s: %w", c.order, err))
				continue
			}
			switch out {
			case releaseRevoked:
				delete(cache[c.inviter], c.invitee)
				released++
			case releaseDone:
				released++
			}
		}
		if len(list) < ReleaseBatch {
			break
		}
		last := list[len(list)-1]
		afterAt, afterOrder = &last.settleAfter, last.order
	}
	return released, errors.Join(errs...)
}

// releaseOne 处理一行。门槛不满足或转搁置时返回 releaseSkipped（下轮再看）。
// 加锁：users(邀请人, 被邀请人) FOR KEY SHARE → referral_accounts(邀请人) FOR UPDATE → referral_rewards(订单) FOR UPDATE SKIP LOCKED →
// [同身份撤销：referral_bindings → referral_rewards(该绑定其余 pending)] → wallet_accounts(邀请人)。
func (s *Service) releaseOne(ctx context.Context, tx pgx.Tx, now time.Time, cache fpCache, order string, inviterID, inviteeID int64) (releaseOutcome, error) {
	if err := lockUsersKeyShare(ctx, tx, inviterID, inviteeID); err != nil {
		return releaseSkipped, err
	}
	acc, err := lockAccount(ctx, tx, inviterID)
	if err != nil {
		return releaseSkipped, err
	}
	r, err := scanReward(tx.QueryRow(ctx, `SELECT `+rewardCols+` FROM referral_rewards WHERE order_id = $1 AND status = 'pending' AND settle_after <= $2 FOR UPDATE SKIP LOCKED`, order, now))
	if db.IsNoRows(err) {
		return releaseSkipped, nil
	}
	if err != nil {
		return releaseSkipped, err
	}
	var bindingStatus string
	var boundAt time.Time
	var reviewedAt *time.Time
	if err := tx.QueryRow(ctx, `SELECT status, bound_at, reviewed_at FROM referral_bindings WHERE id = $1`, r.BindingID).Scan(&bindingStatus, &boundAt, &reviewedAt); err != nil {
		return releaseSkipped, err
	}
	if bindingStatus != BindingActive || acc.Status != AccountActive {
		return releaseSkipped, nil
	}
	u, err := s.d.Users.UserState(ctx, tx, inviterID)
	if err != nil {
		return releaseSkipped, err
	}
	if u.Status != "active" || u.KYCStatus != "verified" {
		return releaseSkipped, nil
	}
	// 纵深防御：实名钩子失败只留审计、不让实名失败，可能漏掉同身份撤销 → 付款前按全部 verified 指纹复查。
	same, err := s.sameIdentityAtRelease(ctx, tx, cache, inviterID, inviteeID)
	if err != nil {
		return releaseSkipped, err
	}
	if same {
		s.d.Log.Warn("referral: same identity found at release, revoking binding", "order", order, "binding", r.BindingID)
		if err := s.autoRevoke(ctx, tx, now, r.BindingID); err != nil {
			return releaseSkipped, err
		}
		return releaseRevoked, nil // 本行已随绑定撤销作废
	}
	since := boundAt
	if reviewedAt != nil {
		since = *reviewedAt
	}
	held, err := kycCheckUnresolved(ctx, tx, inviterID, inviteeID, since)
	if err != nil {
		return releaseSkipped, err
	}
	if held {
		// 转 held 进后台待审核队列（与 same_device 搁置同一流程）：返利保持 pending；后台 approve 写 reviewed_at 后该审计即视为已解决。
		s.d.Log.Warn("referral: unresolved kyc_check_failed, holding binding", "order", order, "binding", r.BindingID)
		if _, err := tx.Exec(ctx, `UPDATE referral_bindings SET status = 'held', hold_reason = $2, updated_at = $3 WHERE id = $1 AND status = 'active'`,
			r.BindingID, HoldKYCCheckFailed, now); err != nil {
			return releaseSkipped, err
		}
		return releaseSkipped, audit.Record(ctx, tx, now, audit.Actor{}.Entry("referral.auto_hold", "referral_binding", strconv.FormatInt(r.BindingID, 10),
			map[string]any{"reason": HoldKYCCheckFailed, "order": order}))
	}
	// 重放保护（先于抵欠账）：该订单已发放过（有 referral/<order> 发放流水，或已有 settle 事件——全额抵欠时没有流水）
	// → 不再抵欠、不再入账，只把行改回 settled 并告警。不能靠捕获 Apply 的 ErrDuplicate：唯一冲突会让事务进入失败态（25P02）。
	var ledgerID int64
	err = tx.QueryRow(ctx, `SELECT id FROM wallet_ledger WHERE kind = 'referral_rebate' AND ref_type = $1 AND ref_id = $2`, RefType, order).Scan(&ledgerID)
	if err != nil && !db.IsNoRows(err) {
		return releaseSkipped, err
	}
	var settledBefore bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM referral_events WHERE order_id = $1 AND kind = 'settle')`, order).Scan(&settledBefore); err != nil {
		return releaseSkipped, err
	}
	if ledgerID != 0 || settledBefore {
		s.d.Log.Error("referral: ALERT pending reward was already released before; marking settled without offset or credit",
			"order", order, "ledger", ledgerID, "settled_before", settledBefore)
		if err := addEvent(ctx, tx, now, order, r.BindingID, EventSettle, 0, ledgerID, r.RewardAmt, "replay: already released"); err != nil {
			return releaseSkipped, err
		}
		_, err = tx.Exec(ctx, `UPDATE referral_rewards SET status = 'settled', settled_amount = reward, settled_at = $2, updated_at = $2 WHERE order_id = $1`, order, now)
		return releaseDone, err
	}
	if r.RewardAmt == 0 {
		if _, err := tx.Exec(ctx, `UPDATE referral_rewards SET status = 'void', updated_at = $2 WHERE order_id = $1`, order, now); err != nil {
			return releaseSkipped, err
		}
		return releaseDone, addEvent(ctx, tx, now, order, r.BindingID, EventVoid, 0, 0, 0, "zero after refund")
	}
	offset := min(acc.Debt, r.RewardAmt)
	if offset > 0 {
		if _, err := tx.Exec(ctx, `UPDATE referral_accounts SET debt = debt - $2, updated_at = $3 WHERE user_id = $1`, inviterID, offset, now); err != nil {
			return releaseSkipped, err
		}
		if err := addEvent(ctx, tx, now, order, r.BindingID, EventDebtOffset, 0, 0, offset, "offset debt"); err != nil {
			return releaseSkipped, err
		}
	}
	if credit := r.RewardAmt - offset; credit > 0 {
		masked, err := s.inviteeMasked(ctx, tx, r.InviteeID)
		if err != nil {
			return releaseSkipped, err
		}
		if _, err := s.d.Wallet.Apply(ctx, tx, wallet.Entry{UserID: inviterID, Amount: credit, Kind: wallet.KindReferralRebate, RefType: RefType, RefID: order,
			Remark: "邀请返利 " + masked + " 订单 " + order}); err != nil {
			return releaseSkipped, err
		}
		if err := tx.QueryRow(ctx, `SELECT id FROM wallet_ledger WHERE kind = 'referral_rebate' AND ref_type = $1 AND ref_id = $2`, RefType, order).Scan(&ledgerID); err != nil {
			return releaseSkipped, err
		}
	}
	if err := addEvent(ctx, tx, now, order, r.BindingID, EventSettle, 0, ledgerID, r.RewardAmt, fmt.Sprintf("offset=%d", offset)); err != nil {
		return releaseSkipped, err
	}
	_, err = tx.Exec(ctx, `UPDATE referral_rewards SET status = 'settled', settled_amount = reward, settled_at = $2, updated_at = $2 WHERE order_id = $1`, order, now)
	return releaseDone, err
}

// sameIdentityAtRelease：被邀请人的任一 verified 指纹等于邀请人或同邀请人其它未撤销被邀请人的任一 verified 指纹（与 OnKYCVerified 同一判定）。
// 指纹按邀请人在本轮内缓存（首次在持有 referral_accounts(邀请人) 锁时读入）。
func (s *Service) sameIdentityAtRelease(ctx context.Context, tx pgx.Tx, cache fpCache, inviterID, inviteeID int64) (bool, error) {
	group, ok := cache[inviterID]
	if !ok {
		rows, err := tx.Query(ctx, `SELECT invitee_id FROM referral_bindings WHERE inviter_id = $1 AND status <> 'revoked'`, inviterID)
		if err != nil {
			return false, err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
		if err != nil {
			return false, err
		}
		group, err = s.d.Users.VerifiedFingerprints(ctx, tx, append(ids, inviterID))
		if err != nil {
			return false, err
		}
		if group == nil {
			group = map[int64][]string{}
		}
		cache[inviterID] = group
	}
	mine := make(map[string]bool, len(group[inviteeID]))
	for _, fp := range group[inviteeID] {
		mine[fp] = true
	}
	if len(mine) == 0 {
		return false, nil
	}
	for uid, fps := range group {
		if uid == inviteeID {
			continue
		}
		for _, fp := range fps {
			if mine[fp] {
				return true, nil
			}
		}
	}
	return false, nil
}

// kycCheckUnresolved 报告邀请人或被邀请人是否有晚于 since（绑定的 reviewed_at，未复核时取 bound_at）的
// referral.kyc_check_failed 审计——实名同身份判定失败、尚待人工核查。绑定之前的失败与该绑定无关；
// 命中时 releaseOne 把绑定转 held（hold_reason=kyc_check_failed），后台裁决（通过 / 撤销）写 reviewed_at，之后同一条审计不再触发。
// 后台列表的 kyc_check_unresolved（admin.go bindingSelect）是同一判定的 SQL 版本：改这里须同步那边，
// TestKYCCheckUnresolvedPredicatesAgree 用同一组用例钉住两者。
func kycCheckUnresolved(ctx context.Context, q db.Querier, inviterID, inviteeID int64, since time.Time) (bool, error) {
	var held bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM audit_logs WHERE target_type = 'user' AND target_id IN ($1, $2)
		AND action = 'referral.kyc_check_failed' AND created_at > $3)`,
		strconv.FormatInt(inviterID, 10), strconv.FormatInt(inviteeID, 10), since).Scan(&held)
	return held, err
}
