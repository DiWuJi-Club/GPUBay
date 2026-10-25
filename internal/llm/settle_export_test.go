package llm

import "context"

// SetSettleBeforeMarkHook 仅测试：settleAccount 读到 max_id 之后、UPDATE llm_usage 标记之前调用 f(userID)
// （模拟结算事务进行中另一个连接插入用量行）。
func SetSettleBeforeMarkHook(s *Service, f func(userID int64)) { s.settleBeforeMark = f }

// Purge 仅测试：单独跑一轮已结算旧明细清理，返回删除行数。
func Purge(ctx context.Context, s *Service) (int64, error) { return s.purge(ctx, s.d.Clock.Now()) }
