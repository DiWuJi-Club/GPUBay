package llm

import (
	"context"
	"sync"

	"spark-cloud/internal/access/upstream"
	"spark-cloud/internal/fleet"
)

// 本文件是网关（internal/llmgw）需要的少量访问器：网关不直接持有连接池 / 钱包 / 告警依赖。

// Upstream 返回上游 SSH 连接池（网关每节点 Transport 的 DialContext 用）。
func (s *Service) Upstream() *upstream.Pool { return s.d.Upstream }

// WalletBalance 读用户钱包余额（分），网关预算预检用（网关自己再做 BalanceCacheTTL 的进程内缓存）。
func (s *Service) WalletBalance(ctx context.Context, userID int64) (int64, error) {
	return s.d.Wallet.Balance(ctx, userID)
}

// NoteNodeFailure 网关首字节前失败转移时给节点记一次探测失败（规格 §6.1 第 9 步）；只对 ready 节点生效，
// 计数达到 ProbeFailThreshold 后由 Prober 转 unhealthy。
func (s *Service) NoteNodeFailure(ctx context.Context, nodeID string) error {
	_, err := s.d.Pool.Exec(ctx, `UPDATE llm_nodes SET probe_fail_count = probe_fail_count + 1 WHERE id = $1 AND state = $2`,
		nodeID, NodeReady)
	return err
}

// AlertUsageRecordFailed 网关记账失败时发一次严重告警（文案稳定、不含数值；详情在日志里）。
func (s *Service) AlertUsageRecordFailed(ctx context.Context) error {
	if s.d.Fleet == nil {
		return nil
	}
	return s.d.Fleet.RaiseAlertOnce(ctx, s.d.Pool, fleet.Alert{Level: "critical", Kind: AlertUsageRecordFailed,
		Message: "Token 用量记账失败，详情见日志"})
}

// HasNode 报告节点当前是否在路由表内（网关回收已下线节点的 Transport 用）。
func (r *Router) HasNode(nodeID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.nodes[nodeID]
	return ok
}

// TryReserveEstimate 预算预检与在途估算登记在同一把锁内完成（规格 T2）：
// budget = balance − ceil(unsettled) − debt（不含在途估算），要求 budget − 当前在途估算 ≥ fen；
// 通过则在途估算 += fen 并返回 release（减回，幂等）。不通过返回 ok=false 与可用额（budget − 在途，可能为负）。
func (l *Limiter) TryReserveEstimate(userID int64, fen, budget int64) (release func(), available int64, ok bool) {
	l.mu.Lock()
	u := l.user(userID)
	available = budget - u.inflightEst
	if available < fen {
		l.mu.Unlock()
		return nil, available, false
	}
	u.inflightEst += fen
	l.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.user(userID).inflightEst -= fen
		})
	}, available, true
}
