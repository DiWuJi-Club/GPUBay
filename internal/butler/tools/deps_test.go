package tools

import (
	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/identity"
	"spark-cloud/internal/rental"
	"spark-cloud/internal/wallet"
)

// 编译期断言：真实服务满足小接口（业务包的方法签名一变这里就编译失败）。
var (
	_ RentalAPI  = (*rental.Service)(nil)
	_ AppsAPI    = (*appdeploy.Service)(nil)
	_ CatalogAPI = (*catalog.Service)(nil)
	_ WalletAPI  = (*wallet.Service)(nil)
	_ UsersAPI   = (*identity.Service)(nil)

	_ RentalAPI  = (*fakeRental)(nil)
	_ AppsAPI    = (*fakeApps)(nil)
	_ CatalogAPI = (*fakeCatalog)(nil)
	_ WalletAPI  = (*fakeWallet)(nil)
	_ UsersAPI   = (*fakeUsers)(nil)
)
