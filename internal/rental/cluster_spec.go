package rental

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/clusterspec"
	"spark-cloud/internal/platform/db"
)

// SubnetPool 返回集群网段池（Deps.ClusterSubnetPool；零值 → clusterspec.DefaultPool）。
func (s *Service) SubnetPool() netip.Prefix {
	if s.d.ClusterSubnetPool.IsValid() {
		return s.d.ClusterSubnetPool
	}
	return netip.MustParsePrefix(clusterspec.DefaultPool)
}

// clusterSubnet 返回集群的 /24。
func (s *Service) clusterSubnet(index int) (netip.Prefix, error) {
	return clusterspec.Subnet(s.SubnetPool(), index)
}

// specMember 是构造 ClusterSpec 用的一个在位成员（实例 + 所在机器的网卡信息）。
type specMember struct {
	Rank         int
	InstanceID   string
	MachineID    int64
	LanIP        string
	ClusterIface string
	NetInfo      []byte
}

// ClusterSpecFor 构造集群 clusterID 里 rank 成员的 ClusterSpec（规格 §5.3）：
// 读集群行 + fabric 类型 + 在位成员（ClusterMemberStates，按 rank）与成员机器的 net_info / cluster_iface / lan_ip；
// 成员集群网卡与宿主机 IP 用 clusterspec.ResolveIface（与 fleet 就绪判定同口径）；routed 模式每个成员带 host_ip。
// 成员缺位时 Members 只含在位成员、Size 不变。不加锁（调用方在需要时已持集群行锁）。
// Validate 失败（rank 不在位、网卡解析失败等）→ 返回错误，不下发坏 Spec。
func (s *Service) ClusterSpecFor(ctx context.Context, q db.Querier, clusterID string, rank int) (clusterspec.Spec, error) {
	c, err := scanCluster(q.QueryRow(ctx, `SELECT `+clusterCols+` FROM instance_clusters WHERE id = $1`, clusterID))
	if err != nil {
		return clusterspec.Spec{}, err
	}
	var kind string
	if err := q.QueryRow(ctx, `SELECT kind FROM cluster_fabrics WHERE id = $1`, c.FabricID).Scan(&kind); err != nil {
		return clusterspec.Spec{}, err
	}
	rows, err := q.Query(ctx, `SELECT i.cluster_rank, i.id, m.id, m.lan_ip, m.cluster_iface, m.net_info
		FROM instances i JOIN machines m ON m.id = i.machine_id
		WHERE i.cluster_id = $1 AND i.state = ANY($2) ORDER BY i.cluster_rank, i.id`, clusterID, ClusterMemberStates)
	if err != nil {
		return clusterspec.Spec{}, err
	}
	members, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (specMember, error) {
		var m specMember
		err := r.Scan(&m.Rank, &m.InstanceID, &m.MachineID, &m.LanIP, &m.ClusterIface, &m.NetInfo)
		return m, err
	})
	if err != nil {
		return clusterspec.Spec{}, err
	}
	subnet, err := s.clusterSubnet(c.SubnetIndex)
	if err != nil {
		return clusterspec.Spec{}, err
	}
	spec := clusterspec.Spec{ClusterID: c.ID, Version: c.Version, Size: c.Size, Rank: rank, Mode: c.Mode, MTU: c.MTU,
		Subnet: subnet.String(), RDMA: c.RDMA, Members: []clusterspec.Member{}}
	if c.VLAN != nil {
		spec.VLAN = *c.VLAN
	}
	for _, m := range members {
		mem := clusterspec.Member{Rank: m.Rank, InstanceID: m.InstanceID, IP: clusterspec.MemberIP(subnet, m.Rank).String()}
		// macvlan 模式只需要自己的父接口；routed 模式每个成员都要宿主机 IP（对端路由 via host_ip）
		if c.Mode != clusterspec.ModeRouted && m.Rank != rank {
			spec.Members = append(spec.Members, mem)
			continue
		}
		var n clusterspec.NetInfo
		if len(m.NetInfo) > 0 {
			if err := json.Unmarshal(m.NetInfo, &n); err != nil {
				return clusterspec.Spec{}, fmt.Errorf("machine %d net_info: %w", m.MachineID, err)
			}
		}
		iface, hostIP, err := clusterspec.ResolveIface(n, kind, m.ClusterIface, m.LanIP)
		if err != nil {
			return clusterspec.Spec{}, fmt.Errorf("集群 %s 成员 rank %d（机器 %d）的集群网卡：%w", clusterID, m.Rank, m.MachineID, err)
		}
		if c.Mode == clusterspec.ModeRouted {
			mem.HostIP = hostIP
		}
		if m.Rank == rank {
			spec.Iface = iface.Name
		}
		spec.Members = append(spec.Members, mem)
	}
	if err := spec.Validate(); err != nil {
		return clusterspec.Spec{}, fmt.Errorf("集群 %s rank %d：%w", clusterID, rank, err)
	}
	return spec, nil
}
