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
}
