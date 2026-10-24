package referral

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// SetReleaseMaxPages 改每轮翻页上限（测试单页行为）；返回恢复函数。
func SetReleaseMaxPages(n int) (restore func()) {
	old := releaseMaxPages
	releaseMaxPages = n
	return func() { releaseMaxPages = old }
}

// ReleaseOneForTest 直接跑一行的锁内复核与发放（绕过候选预筛）；返回是否推进到终态。
func (s *Service) ReleaseOneForTest(ctx context.Context, tx pgx.Tx, now time.Time, order string, inviter, invitee int64) (bool, error) {
	out, err := s.releaseOne(ctx, tx, now, fpCache{}, order, inviter, invitee)
	return out != releaseSkipped, err
}
