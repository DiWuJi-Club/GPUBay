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

// SetBeforeWriteback 设置 Runner 的测试钩子（exec 之后、回写事务之前调用）；nil 清除。
func SetBeforeWriteback(r *Runner, fn func(Deployment)) { r.beforeWriteback = fn }

// SetExecHook 设置 Runner 的 exec 注入钩子（返回非 nil 错误时不执行命令）；nil 清除。
func SetExecHook(r *Runner, fn func(cmd string) error) { r.execHook = fn }
