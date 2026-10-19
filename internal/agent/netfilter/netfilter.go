// Package netfilter 抽象容器网络隔离与限速：生产 iptables + tc，测试 fake。
package netfilter

import "context"

type Rules struct {
	Bridge     string   // spark0
	AllowCIDRs []string // 白名单（Harbor、HF 镜像站、DNS 等）
}

type Netfilter interface {
	EnsureRules(ctx context.Context, r Rules) error
	Limit(ctx context.Context, veth string, mbps int) error
	// LimitApplied：veth 上下行限速（root tbf + ingress police）是否都在；容器被外部重启后换了新 veth，限速会丢。
	LimitApplied(ctx context.Context, veth string) (bool, error)
}
