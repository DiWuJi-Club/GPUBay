package fleet

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/agent/features"
	"spark-cloud/internal/clusterspec"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
)

// 未就绪原因（后台机器详情「集群网络」卡）；网卡相关的原因取 clusterspec.ResolveIface 的错误文案。
const (
	reasonNoFabric     = "未加入集群网络"
	reasonOldAgent     = "Agent 不支持集群（请升级）"
	reasonNoNetInfo    = "尚未上报网卡"
	reasonBadIPv4      = "集群网卡的 IPv4 地址无效"
	reasonNetFailed    = "上次集群网络开通失败，等待 Agent 重新上报网卡" // SuspendClusterReady 之后、下次重算之前
	clusterNetL2       = "l2" // cx7 fabric 的 cluster_net：由物理接线保证同一二层
	maxNetIfaces       = 64
	maxNetIfaceAddress = 16
)

// evalClusterNet 计算机器的集群就绪判定（规格 §3.2）：ready / rdma / cluster_net / 未就绪原因。
// 管理网卡一律按 m.LanIP 在网卡 CIDR 里找（clusterspec.MgmtIface）：Hello 在 AgentConfig 之前发出，n.LanIface 可能为空，
// 不能因此让就绪翻转。
//   - lan：cluster_net = 集群网卡上 host IP 所在的 IPv4 前缀（routed 成员必须同一网段，分配按 (fabric_id, cluster_net) 分组）；rdma 恒 false；
//   - cx7：cluster_net = "l2"；rdma = fabric.rdma ∧ rdma_active ∧ uverbs ≠ ""。
func evalClusterNet(m Machine, f *Fabric, n *clusterspec.NetInfo) (ready, rdma bool, clusterNet, reason string) {
	switch {
	case f == nil:
		return false, false, "", reasonNoFabric
	case m.AgentFeatures&features.Cluster == 0:
		return false, false, "", reasonOldAgent
	case n == nil:
		return false, false, "", reasonNoNetInfo
	}
	iface, hostIP, err := clusterspec.ResolveIface(*n, f.Kind, m.ClusterIface, m.LanIP)
	if err != nil {
		return false, false, "", err.Error()
	}
	if f.Kind == FabricKindCX7 {
		return true, f.RDMA && iface.RDMAActive && iface.Uverbs != "", clusterNetL2, ""
	}
	clusterNet = ""
	for _, c := range iface.IPv4 { // host IP 所在的那条 CIDR（通常就是第一条）
		if p := clusterspec.NetPrefix(c); p != "" && bareAddr(c) == hostIP {
			clusterNet = p
			break
		}
	}
	if clusterNet == "" {
		clusterNet = clusterspec.NetPrefix(iface.IPv4[0])
	}
	if clusterNet == "" {
		return false, false, "", reasonBadIPv4
	}
	return true, false, clusterNet, ""
}

func bareAddr(cidr string) string {
	ip, _, _ := strings.Cut(cidr, "/")
	return ip
}

// recomputeClusterNet 在 tx 里按机器当前列（含所属 fabric）重算 cluster_ready / cluster_rdma / cluster_net，值变化才写。
// 调用方须已在本事务里锁住机器行（FOR NO KEY UPDATE），且读取在加锁之后（READ COMMITTED 下新语句能看到刚提交的 fabric 变化）。
func (s *Service) recomputeClusterNet(ctx context.Context, tx pgx.Tx, machineID int64) error {
	m, err := s.Get(ctx, tx, machineID)
	if err != nil {
		return err
	}
	ready, rdma, cnet, _ := evalClusterNet(m, m.fabric, m.NetInfo)
	if ready == m.ClusterReady && rdma == m.ClusterRDMA && cnet == m.ClusterNet {
		return nil
	}
	_, err = tx.Exec(ctx, `UPDATE machines SET cluster_ready = $2, cluster_rdma = $3, cluster_net = $4, updated_at = $5 WHERE id = $1`,
		machineID, ready, rdma, cnet, s.clock.Now())
	return err
}

// SuspendClusterReady 集群网络导致开通失败的机器暂不参与集群分配：cluster_ready / cluster_rdma / cluster_net 清空，
// 直到下一次重算（Agent 重连 Hello、上报的网卡变化、后台改 fabric / 集群网卡）。机器仍可单机出租。
// 调用方持实例行锁（与 MarkFaulty 同位置更新机器行）。
func (s *Service) SuspendClusterReady(ctx context.Context, q db.Querier, machineID int64) error {
	_, err := q.Exec(ctx, `UPDATE machines SET cluster_ready = false, cluster_rdma = false, cluster_net = '', updated_at = $2
		WHERE id = $1 AND cluster_ready`, machineID, s.clock.Now())
	return err
}

func lockMachine(ctx context.Context, tx pgx.Tx, id int64) error {
	var x int64
	err := tx.QueryRow(ctx, `SELECT id FROM machines WHERE id = $1 FOR NO KEY UPDATE`, id).Scan(&x)
	if db.IsNoRows(err) {
		return apperr.ErrNotFound
	}
	return err
}

// sanitizeNetInfo 限制网卡数与每块网卡的地址数（不信任 Agent 上报的规模），nil 切片写成空数组。
func sanitizeNetInfo(n clusterspec.NetInfo) clusterspec.NetInfo {
	if len(n.Ifaces) > maxNetIfaces {
		n.Ifaces = n.Ifaces[:maxNetIfaces]
	}
	out := make([]clusterspec.Iface, len(n.Ifaces))
	for i, f := range n.Ifaces {
		if len(f.IPv4) > maxNetIfaceAddress {
			f.IPv4 = f.IPv4[:maxNetIfaceAddress]
		}
		if f.IPv4 == nil {
			f.IPv4 = []string{}
		}
		out[i] = f
	}
	n.Ifaces = out
	return n
}

// RecordHello 写 Hello 带来的能力位（agent_features）与网卡探测（net 非 nil 时写 net_info / net_info_at），并重算就绪判定。
// 每次建流调用；旧 Agent features = 0、net = nil。
func (s *Service) RecordHello(ctx context.Context, machineID int64, feats uint32, net *clusterspec.NetInfo) error {
	var raw []byte
	if net != nil {
		var err error
		if raw, err = json.Marshal(sanitizeNetInfo(*net)); err != nil {
			return err
		}
	}
	now := s.clock.Now()
	return db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockMachine(ctx, tx, machineID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE machines SET
				agent_features = $2,
				net_info = COALESCE($3::jsonb, net_info),
				net_info_at = CASE WHEN $3::jsonb IS NULL THEN net_info_at ELSE $4 END
			WHERE id = $1`, machineID, int32(feats), raw, now); err != nil {
			return err
		}
		return s.recomputeClusterNet(ctx, tx, machineID)
	})
}

// RecordNetInfo 写 StateReport 带来的网卡探测并重算就绪判定；JSON 与现值相同时只刷新 net_info_at（每 60 秒一次）。
func (s *Service) RecordNetInfo(ctx context.Context, machineID int64, net clusterspec.NetInfo) error {
	raw, err := json.Marshal(sanitizeNetInfo(net))
	if err != nil {
		return err
	}
	now := s.clock.Now()
	return db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		var same bool
		err := tx.QueryRow(ctx, `SELECT COALESCE(net_info = $2::jsonb, false) FROM machines WHERE id = $1 FOR NO KEY UPDATE`, machineID, raw).Scan(&same)
		if db.IsNoRows(err) {
			return apperr.ErrNotFound
		}
		if err != nil {
			return err
		}
		if same {
			_, err := tx.Exec(ctx, `UPDATE machines SET net_info_at = $2 WHERE id = $1`, machineID, now)
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE machines SET net_info = $2::jsonb, net_info_at = $3, updated_at = $3 WHERE id = $1`, machineID, raw, now); err != nil {
			return err
		}
		return s.recomputeClusterNet(ctx, tx, machineID)
	})
}
