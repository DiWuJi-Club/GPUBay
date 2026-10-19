package ingress_test

import (
	"spark-cloud/internal/access/upstream"
	"spark-cloud/internal/identity"
	"spark-cloud/internal/ingress"
	"spark-cloud/internal/rental"
)

var (
	_ ingress.SessionChecker = (*identity.Service)(nil)
	_ ingress.Dialer         = (*upstream.Pool)(nil)
	_ ingress.Alerts         = (*rental.Service)(nil)
)
