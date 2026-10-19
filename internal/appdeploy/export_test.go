package appdeploy

import (
	"context"
	"time"

	"spark-cloud/internal/platform/db"
)

// 测试用导出（只在 appdeploy_test 包里可见）。
var (
	OpenSecrets   = openSecrets
	SealSecrets   = sealSecrets
	RegisterPorts = registerPorts
)

func (s *Service) MarkFailed(ctx context.Context, q db.Querier, cur Deployment, code, msg string, now time.Time) error {
	return s.markFailed(ctx, q, cur, code, msg, now)
}

func (s *Service) MarkRemoved(ctx context.Context, q db.Querier, cur Deployment, code, detail string, now time.Time) error {
	return s.markRemoved(ctx, q, cur, code, detail, now)
}
