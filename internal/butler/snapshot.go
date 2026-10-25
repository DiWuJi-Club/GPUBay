package butler

import (
	"context"

	"spark-cloud/internal/butler/prompt"
	"spark-cloud/internal/rental"
)

// snapshot 每轮重新计算系统提示里的用户快照（D10）：手机尾号、实名状态、余额、各状态实例数、当前上下文实例、东八区时间。
// 任何一项取不到只记日志并标记未知（提示模型用工具查询），不让整轮失败。
func (s *Service) snapshot(ctx context.Context, userID int64, instanceID string) prompt.Snapshot {
	snap := prompt.Snapshot{InstanceID: instanceID, Now: s.d.Clock.Now()}
	if s.d.Users == nil {
		snap.UserUnknown = true
	} else if u, err := s.d.Users.GetUser(ctx, userID); err != nil {
		s.log.Warn("butler: snapshot user", "user", userID, "err", err)
		snap.UserUnknown = true
	} else {
		r := []rune(u.Phone)
		snap.PhoneTail = string(r[max(0, len(r)-4):])
		snap.KYC = u.KYCStatus
	}
	if s.d.Wallet == nil {
		snap.BalanceUnknown = true
	} else if b, err := s.d.Wallet.Balance(ctx, userID); err != nil {
		s.log.Warn("butler: snapshot balance", "user", userID, "err", err)
		snap.BalanceUnknown = true
	} else {
		snap.BalanceCents = b
	}
	if s.d.Rental == nil {
		snap.CountsUnknown = true
	} else if list, err := s.d.Rental.ListForUser(ctx, userID); err != nil {
		s.log.Warn("butler: snapshot instances", "user", userID, "err", err)
		snap.CountsUnknown = true
	} else {
		for _, inst := range list {
			switch inst.State {
			case rental.StateRunning:
				snap.Running++
			case rental.StateStopped:
				snap.Stopped++
			case rental.StateReserved:
				snap.Reserved++
			case rental.StateReleased, rental.StateCancelled:
			default:
				snap.Other++
			}
		}
	}
	return snap
}
