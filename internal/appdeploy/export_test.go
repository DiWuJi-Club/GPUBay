package appdeploy

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/platform/db"
)

// 测试用导出（只在 appdeploy_test 包里可见）。
var (
	OpenSecrets   = openSecrets
	SealSecrets   = sealSecrets
	RegisterPorts = registerPorts
)

// MarkFailed 在 q 是事务时直接用它，否则另开一个事务（markFailed 要求事务：OnTransition 在同一事务里调用）。
func (s *Service) MarkFailed(ctx context.Context, q db.Querier, cur Deployment, code, msg string, now time.Time) error {
	if tx, ok := q.(pgx.Tx); ok {
		return s.markFailed(ctx, tx, cur, code, msg, now)
	}
	return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error { return s.markFailed(ctx, tx, cur, code, msg, now) })
}

func (s *Service) MarkRemoved(ctx context.Context, q db.Querier, cur Deployment, code, detail string, now time.Time) error {
	return s.markRemoved(ctx, q, cur, code, detail, now)
}

// SetBeforeWriteback 设置 Runner 的测试钩子（exec 之后、回写事务之前调用）；nil 清除。
func SetBeforeWriteback(r *Runner, fn func(Deployment)) { r.beforeWriteback = fn }

// SetExecHook 设置 Runner 的 exec 注入钩子（返回非 nil 错误时不执行命令）；nil 清除。
func SetExecHook(r *Runner, fn func(cmd string) error) { r.execHook = fn }

// ImageEnvironCmd 是 Runner 读镜像环境的容器内命令。
func ImageEnvironCmd(path string) string { return imageEnvironCmd(path) }

// SetClusterOpHook 设置整组操作的测试钩子（读完成员部署所在实例清单之后、加实例锁之前调用；attempt 从 1 起）；nil 清除。
func SetClusterOpHook(s *Service, fn func(attempt int)) { s.clusterOpHook = fn }
