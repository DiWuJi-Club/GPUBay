package tools

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/identity"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/secretbox"
	"spark-cloud/internal/rental"
	"spark-cloud/internal/wallet"
)

// Deps 是工具用到的业务服务。字段类型是本包定义的小接口（只含用到的方法），app.go 传真实的 *rental.Service 等；
// 单测传手写 fake。所有调用都走用户侧（*ForUser / 带 userID）的既有方法，工具不直接读写表（规格 §1.1 第 1 条）。
type Deps struct {
	Rental  RentalAPI
	Apps    AppsAPI
	Catalog CatalogAPI
	Wallet  WalletAPI
	Users   UsersAPI
	// Pool 只给 catalog.Quote 当 db.Querier（它不接受 nil）；写工具报价用。
	Pool *pgxpool.Pool
	// MachineLabel 返回机器展示名（与 api.views() 的 machine_label 同源：fleet.Get(...).Label()）；nil 时 machine_label 为空。
	MachineLabel func(ctx context.Context, machineID int64) (string, error)
	// PortURL 返回实例端口的 Web 访问地址（与 appdeploy 部署视图同一函数）；nil 时端口不带 url。
	PortURL func(instanceID string, port int) string
	// SSHHost / SSHPort 用于拼 ssh_command（与 api.Server.SSHHost / SSHPort 相同；端口 ≤ 0 视为 22）。
	SSHHost string
	SSHPort int
	Clock   clock.Clock
	// IngressDomain 是实例端口域名后缀（ins-*.<domain>）；Box 加解密写工具的 secret 变量（Task 6）。
	IngressDomain string
	Box           *secretbox.Box
}

// RentalAPI 是 *rental.Service 的子集（签名与 internal/rental 一致，deps_test.go 有编译期断言）。
type RentalAPI interface {
	ListForUser(ctx context.Context, userID int64) ([]rental.Instance, error)
	GetForUser(ctx context.Context, userID int64, id string) (rental.Instance, error)
	UserEvents(ctx context.Context, userID int64, id string) ([]rental.UserEvent, error)
	ListPorts(ctx context.Context, instanceID string) ([]rental.Port, error)
	RenewInfoFor(ctx context.Context, inst rental.Instance) (rental.RenewInfo, error)
	CancelQuote(ctx context.Context, userID int64, id string) (rental.CancelQuote, error)
	ListOrders(ctx context.Context, userID int64, limit int) ([]rental.Order, error)
	AvailableNow(ctx context.Context, productID int64) (int, error)
	Availability(ctx context.Context, q rental.AvailabilityQuery) (rental.Availability, error)
	// 写工具（Task 6）
	PlaceBatch(ctx context.Context, req rental.BatchRequest) (rental.BatchResult, error)
	RenewQuantity(ctx context.Context, userID int64, instanceID string, d catalog.Duration, quantity int, idempotencyKey string) (rental.Order, error)
	Release(ctx context.Context, userID int64, instanceID string) error
	Cancel(ctx context.Context, userID int64, id, ip string) (rental.Instance, error)
}

// AppsAPI 是 *appdeploy.Service 的子集。ListForInstance / Events 不校验归属：调用前必须先 GetForUser（实例或部署）。
type AppsAPI interface {
	CatalogViews(ctx context.Context, withDescription bool) ([]appdeploy.CatalogView, error)
	CatalogViewBySlug(ctx context.Context, slug string) (appdeploy.CatalogView, error)
	ListForInstance(ctx context.Context, instanceID string) ([]appdeploy.Deployment, error)
	Views(ctx context.Context, deps []appdeploy.Deployment) ([]appdeploy.View, error)
	Summaries(ctx context.Context, instanceIDs []string) (map[string][]appdeploy.Summary, error)
	GetForUser(ctx context.Context, userID int64, instanceID, depID string) (appdeploy.Deployment, error)
	Log(ctx context.Context, userID int64, instanceID, depID string) (appdeploy.LogResult, error)
	Events(ctx context.Context, depID string, limit int) ([]appdeploy.Event, error)
	// 写工具（Task 6）
	Create(ctx context.Context, req appdeploy.CreateRequest) (appdeploy.Deployment, error)
	Redeploy(ctx context.Context, req appdeploy.RedeployRequest) (appdeploy.Deployment, error)
	Stop(ctx context.Context, userID int64, instanceID, depID, ip string) (appdeploy.Deployment, error)
	Start(ctx context.Context, userID int64, instanceID, depID, ip string) (appdeploy.Deployment, error)
	Remove(ctx context.Context, userID int64, instanceID, depID, ip string) (appdeploy.Deployment, error)
}

// CatalogAPI 是 *catalog.Service 的子集。
type CatalogAPI interface {
	ListProducts(ctx context.Context) ([]catalog.ProductWithPrices, error)
	Quote(ctx context.Context, q db.Querier, productID int64, d catalog.Duration) (int64, error)
	ProductName(ctx context.Context, id int64) (string, error)
	ImageName(ctx context.Context, id int64) (string, error)
	ListImages(ctx context.Context) ([]catalog.Image, error)
}

// WalletAPI 是 *wallet.Service 的子集。
type WalletAPI interface {
	Balance(ctx context.Context, userID int64) (int64, error)
	ListLedger(ctx context.Context, userID, beforeID int64, limit int) ([]wallet.LedgerRow, error)
}

// UsersAPI 是 *identity.Service 的子集（Task 7 的用户快照用）。
type UsersAPI interface {
	GetUser(ctx context.Context, id int64) (identity.User, error)
}
