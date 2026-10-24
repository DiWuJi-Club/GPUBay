// Package llm 实现 Token 商城（计划 9）的业务核心：模型目录、API Key、账户与限额、用量记账、结算、
// 节点 Controller / Prober、网关路由表与限流。OpenAI 兼容网关本身在 internal/llmgw。
//
// 资金只经 wallet.ApplyUpTo（结算）；网关从不写钱包。每次结算一个唯一 ref_id（结算 ID），wallet_ledger_ref_uniq 兜底重放。
//
// 加锁顺序（规格 §14，所有路径一律）：
//
//	| 路径                          | 顺序                                                                                              |
//	|-------------------------------|---------------------------------------------------------------------------------------------------|
//	| 网关记账 RecordUsage           | llm_accounts(user) FOR UPDATE → 插 llm_usage → llm_api_keys(key)                                   |
//	| 结算 Settle                    | llm_accounts(user) FOR UPDATE → llm_usage（UPDATE 标记）→ llm_settlements → llm_usage_daily → wallet_accounts(user) |
//	| 建节点                         | machines(FOR NO KEY UPDATE SKIP LOCKED) → 插 instances / machine_leases（rental.PlaceSystem）→ 插 llm_nodes |
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

// RentalOps 由 *rental.Service 满足：PlaceSystem / ReleaseSystem / Get / UpstreamTarget。
// 占位：rental.SystemPlaceRequest 与 UpstreamTarget 在 Task 2 才有，届时替换为真实方法集。
type RentalOps interface{}

// AppOps 由 *appdeploy.Service 满足。
type AppOps interface {
	Create(ctx context.Context, req appdeploy.CreateRequest) (appdeploy.Deployment, error)
	Redeploy(ctx context.Context, req appdeploy.RedeployRequest) (appdeploy.Deployment, error)
	Get(ctx context.Context, depID string) (appdeploy.Deployment, error)
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

// FleetOps 由 *fleet.Service 满足（LLMPoolGroupID 在 Task 3 新增）。
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
}

type Service struct {
	d Deps
	// Task 6 / 9 增加：Key 缓存、余额缓存、Limiter、Router
}

func NewService(d Deps) *Service {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Clock == nil {
		d.Clock = clock.Real{}
	}
	return &Service{d: d}
}
