// Package llm 实现 Token 商城（计划 9）的业务核心：模型目录、API Key、账户与限额、用量记账、结算、
// 节点 Controller / Prober、网关路由表与限流。OpenAI 兼容网关本身在 internal/llmgw。
//
// 资金只经 wallet.ApplyUpTo（结算）；网关从不写钱包。每次结算一个唯一 ref_id（结算 ID），wallet_ledger_ref_uniq 兜底重放。
//
// 加锁顺序（规格 §14，所有路径一律）：
//
//	| 路径                          | 顺序                                                                                              |
//	|-------------------------------|---------------------------------------------------------------------------------------------------|
//	| 控制台建 Key CreateKey         | llm_accounts(user) FOR UPDATE（无则先插）→ 插 llm_api_keys（计数 ≤ 20 在账户行锁下）            |
//	| 网关记账 RecordUsage           | llm_accounts(user) FOR UPDATE → 插 llm_usage → llm_api_keys(key)                                   |
//	| 结算 Settle                    | llm_accounts(user) FOR UPDATE → llm_usage（UPDATE 标记）→ llm_settlements → llm_usage_daily → wallet_accounts(user) |
//	| 建节点                         | machines(FOR NO KEY UPDATE SKIP LOCKED) → 插 instances / machine_leases（rental.PlaceSystem）→ 插 llm_nodes |
//	| 建组节点（计划 13）             | [rental.PlaceSystemCluster 自己的事务：machines → cluster-alloc → 集群 / 实例] → 插 llm_nodes + llm_node_members |
//	| 节点转终态                     | llm_nodes(node) FOR UPDATE → llm_node_members(node) live = false（同一事务）                         |
//	| Controller 推进 / Prober 回写  | llm_nodes(node) FOR UPDATE → [appdeploy / rental 调用在事务外]                                      |
//	| 后台模型 / Key / 账户操作       | 单表行锁                                                                                           |
//	| rental / billing / referral   | 不碰 llm 表                                                                                         |
//
// llm_accounts 之后只锁更细的行与 wallet_accounts，而其它模块只锁 wallet_accounts → 等待链不成环。
// Controller 对 appdeploy / rental 的调用在事务外（它们各自开事务），llm_nodes 行锁不与 instances 行锁嵌套。
// 结算单账户事务、节点事务遇 40P01 / 40001 重试一次。
package llm

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/access/upstream"
	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/secretbox"
	"spark-cloud/internal/rental"
	"spark-cloud/internal/wallet"
)

// WalletOps 由 *wallet.Service 满足。
type WalletOps interface {
	Balance(ctx context.Context, userID int64) (int64, error)
	ApplyUpTo(ctx context.Context, tx pgx.Tx, e wallet.Entry) (applied, after int64, err error)
}

// UserLookup 由 *identity.Service 满足。用户状态 / 实名（网关门槛）由本包在 Key 查询里直接 JOIN users 读取。
type UserLookup interface {
	CanRent(ctx context.Context, userID int64) error
}

// RentalOps 由 *rental.Service 满足：系统实例的开通 / 释放 / 查询与拨号目标（节点 Controller / Prober 用）。
type RentalOps interface {
	PlaceSystem(ctx context.Context, req rental.SystemPlaceRequest) (rental.Instance, error)
	ReleaseSystem(ctx context.Context, instanceID string) error
	Get(ctx context.Context, id string) (rental.Instance, error)
	UpstreamTarget(ctx context.Context, instanceID string) (upstream.Target, error)
	InstanceSSHPort(instanceID string) int // 容器 sshd 端口（计划 10 按实例端口）；路由表的拨号目标与 UpstreamTarget 一致
	ListLiveSystemInstances(ctx context.Context) ([]rental.Instance, error)
}

// AppOps 由 *appdeploy.Service 满足。
type AppOps interface {
	Create(ctx context.Context, req appdeploy.CreateRequest) (appdeploy.Deployment, error)
	Redeploy(ctx context.Context, req appdeploy.RedeployRequest) (appdeploy.Deployment, error)
	Get(ctx context.Context, depID string) (appdeploy.Deployment, error)
	Events(ctx context.Context, depID string, limit int) ([]appdeploy.Event, error)  // 节点详情的部署事件
	ListLive(ctx context.Context, instanceID string) ([]appdeploy.Deployment, error) // Create 撞 app_exists 时认领现有部署
}

// ClusterRentalOps 是计划 13 多机节点组用的 rental 方法（*rental.Service 满足，计划 13 Task 6）：
// 在同一 fabric 的指定池机器上原子地建系统集群（每台一个系统实例，rank 按机器 ID 升序）/ 释放整组。
// 单列一个接口而不是并进 RentalOps：装配（app.go）在 Task 13，llm 包不依赖 rental 的实现进度。
type ClusterRentalOps interface {
	PlaceSystemCluster(ctx context.Context, req rental.SystemClusterRequest) (rental.SystemClusterResult, error)
	ReleaseSystemCluster(ctx context.Context, clusterID string) error // 已无在位成员 → nil；非系统集群 → ErrInvalidState
}

// ClusterAppOps 是计划 13 多机节点组用的 appdeploy 方法（*appdeploy.Service 满足，计划 13 Task 7）。
type ClusterAppOps interface {
	CreateCluster(ctx context.Context, req appdeploy.ClusterCreateRequest) (appdeploy.ClusterDeployment, error)
	GetClusterDeployment(ctx context.Context, id string) (appdeploy.ClusterDeployment, error)
	LiveClusterDeployment(ctx context.Context, clusterID string) (*appdeploy.ClusterDeployment, error) // 无 → nil, nil
	RedeployCluster(ctx context.Context, req appdeploy.ClusterRedeployRequest) (appdeploy.ClusterDeployment, error)
}

// AppCatalog 由 *appdeploy.Catalog 满足。
type AppCatalog interface {
	AppBySlug(ctx context.Context, slug string) (appdeploy.App, error)
	LatestPublished(ctx context.Context, q db.Querier, appID int64) (appdeploy.Version, error)
	GetVersion(ctx context.Context, id int64) (appdeploy.Version, error)
}

// ImageLookup 由 *catalog.Service 满足（校验模型镜像：存在且 enabled）。
type ImageLookup interface {
	GetImage(ctx context.Context, q db.Querier, id int64) (catalog.Image, error)
}

// FleetOps 由 *fleet.Service 满足。
type FleetOps interface {
	LLMPoolGroupID(ctx context.Context, q db.Querier) (int64, error)
	Get(ctx context.Context, q db.Querier, id int64) (fleet.Machine, error)
	RaiseAlert(ctx context.Context, q db.Querier, a fleet.Alert) error
	RaiseAlertOnce(ctx context.Context, q db.Querier, a fleet.Alert) error
}

type Deps struct {
	Pool         *pgxpool.Pool
	Clock        clock.Clock
	Log          *slog.Logger
	Box          *secretbox.Box // 节点密钥 llm_nodes.api_key_enc
	Wallet       WalletOps
	Users        UserLookup
	Rental       RentalOps
	Apps         AppOps
	AppCat       AppCatalog
	Images       ImageLookup
	Fleet        FleetOps
	Upstream     *upstream.Pool // 探测与转发
	SystemUserID int64          // app.go 从 users WHERE system AND phone = 'system:llm' 读出
	// 计划 13 多机节点组。nil → NewService 尝试从 Rental / Apps 断言（真实 *rental.Service / *appdeploy.Service 两者都满足）；
	// 仍为 nil 时 nodes_per_replica > 1 的模型不建组（Controller 记日志），其余行为不变。
	ClusterRental ClusterRentalOps
	ClusterApps   ClusterAppOps
}

type Service struct {
	d Deps
	// keyCache：Authorize 的 KeyCacheTTL 缓存与失效代数（见 authorize.go）。
	keyCache keyCache
	limiter  *Limiter
	router   *Router
	// settleBeforeMark 仅测试：settleAccount 读到 max_id 之后、标记用量之前调用（见 settle_export_test.go）。
	settleBeforeMark func(userID int64)
}

func NewService(d Deps) *Service {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Clock == nil {
		d.Clock = clock.Real{}
	}
	if d.ClusterRental == nil {
		d.ClusterRental, _ = d.Rental.(ClusterRentalOps)
	}
	if d.ClusterApps == nil {
		d.ClusterApps, _ = d.Apps.(ClusterAppOps)
	}
	s := &Service{d: d, limiter: NewLimiter()}
	s.router = NewRouter(s)
	return s
}

// Limiter 返回进程内的按用户限流器（网关用）。
func (s *Service) Limiter() *Limiter { return s.limiter }

// Router 返回进程内的节点路由表（网关选节点、Controller 读在途）。
func (s *Service) Router() *Router { return s.router }
