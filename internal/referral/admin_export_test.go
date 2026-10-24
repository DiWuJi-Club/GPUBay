package referral

import (
	"context"
	"time"

	"spark-cloud/internal/platform/db"
)

// KYCCheckUnresolvedForTest 暴露发放用的判定，与后台列表的 SQL 版本对照（TestKYCCheckUnresolvedPredicatesAgree）。
func KYCCheckUnresolvedForTest(ctx context.Context, q db.Querier, inviterID, inviteeID int64, since time.Time) (bool, error) {
	return kycCheckUnresolved(ctx, q, inviterID, inviteeID, since)
}
