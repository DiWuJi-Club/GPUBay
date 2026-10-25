package rental

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
)

// InstallPlan 是 PrepareInstall 返回、QueueInstall 消费的不透明预装计划（appdeploy 的类型，rental 不解释）。
type InstallPlan = any

// AppHooks 由 appdeploy 实现（计划 7）；所有方法在 rental 已持实例行锁的事务里调用，锁顺序 instance → app_deployments → instance_ports。
// 钩子不得因用户数据（变量、应用状态）让 rental 事务失败：用户数据只在 PrepareInstall（下单、扣款之前）校验。
type AppHooks interface {
	// OnInstanceRunning：实例进入 running（首次开通、续费恢复、换机后新实例）；installing / running / stopped 且 desired=running 的部署回 queued。
	OnInstanceRunning(ctx context.Context, tx pgx.Tx, instanceID string, now time.Time) error
	// OnInstanceGone：实例释放 / 洗机 / 开通失败 / 取消；非终态部署 → removed。reason 为中文短句。
	OnInstanceGone(ctx context.Context, tx pgx.Tx, instanceID, reason string, now time.Time) error
	// OnInstanceMigrated：换机；旧实例部署 → lost，复制到新实例（queued；用户停止的仍为 stopped），不迁移的部署的端口不留在新实例上。
	OnInstanceMigrated(ctx context.Context, tx pgx.Tx, oldID, newID string, now time.Time) error
	// PrepareInstall：下单预装校验（应用 / 镜像兼容 / 变量；计划 11：workflowIDs 非空时一并预装这些工作流），失败整单不下。
	PrepareInstall(ctx context.Context, q db.Querier, appID, imageID int64, env map[string]string, workflowIDs []int64) (InstallPlan, error)
	// QueueInstall：为一台刚插入的实例写一条 queued 部署。
	QueueInstall(ctx context.Context, tx pgx.Tx, inst Instance, plan InstallPlan, now time.Time) error
}

type noopAppHooks struct{}

func (noopAppHooks) OnInstanceRunning(context.Context, pgx.Tx, string, time.Time) error { return nil }
func (noopAppHooks) OnInstanceGone(context.Context, pgx.Tx, string, string, time.Time) error {
	return nil
}
func (noopAppHooks) OnInstanceMigrated(context.Context, pgx.Tx, string, string, time.Time) error {
	return nil
}
func (noopAppHooks) PrepareInstall(context.Context, db.Querier, int64, int64, map[string]string, []int64) (InstallPlan, error) {
	return nil, apperr.ErrAppNotFound
}
func (noopAppHooks) QueueInstall(context.Context, pgx.Tx, Instance, InstallPlan, time.Time) error {
	return nil
}

// SetAppHooks 接上应用部署钩子（app.go 在 appdeploy.NewService 之后调用）；nil = 不接（带 app_id 的下单报 app_not_found）。
func (s *Service) SetAppHooks(h AppHooks) { s.appHooks = h }

func (s *Service) apps() AppHooks {
	if s.appHooks == nil {
		return noopAppHooks{}
	}
	return s.appHooks
}
