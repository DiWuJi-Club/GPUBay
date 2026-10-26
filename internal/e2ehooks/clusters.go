package e2ehooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"

	"spark-cloud/internal/agent/clusternet/fake"
	"spark-cloud/internal/agent/features"
	"spark-cloud/internal/audit"
	"spark-cloud/internal/clusterspec"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/randx"
)

// 计划 13 多机集群的 e2e 钩子：
//   - POST /hooks/fabrics {name?, kind, isolation?, rdma?, mtu?, vlan_min?, vlan_max?, description?} → {id, name}：
//     直接经 Fleet.CreateFabric 建 fabric（系统操作人）。name 缺省自动生成且唯一；isolation 缺省 lan → routed、cx7 → direct；
//     isolation = vlan 且未给范围时取 100～199。
//   - POST /hooks/machines 的 fabric_id / net / old_agent（见 machines 与 NetPreset）：登记 → fake Agent 接入（Hello 带能力位与网卡）→
//     UpdateMachine{SetFabric} 加入 fabric，返回时 cluster_ready 已按 fleet 规则算好。
//   - /hooks/agent 照常推进 Create / configure_cluster / Wipe（fake Agent 的执行器带 fake 集群数据面，报告里 cluster = ok）。
//   - POST /hooks/agent/cluster-net {machine_id, fail_apply?, check_ok?} → {ok}：改该机器 fake Agent 的集群数据面——
//     fail_apply 非空：之后的 Apply 都失败（Create 失败消息带 "cluster-net: " 前缀，平台走集群失败路径、不判机器故障），"" 恢复；
//     check_ok = false：巡检报告「spc0 缺失（fake）」（实例 cluster = "error: …"，集群降级、告警 cluster_net_error），true 恢复；
//     给了 check_ok 时钩子立即巡检一轮并推进 + 上报（同 /hooks/agent {machine_id}），返回时状态已落库。机器没有 fake Agent 时先接入。

// 网卡预设（/hooks/machines 的 net）。IPv4 一律 CIDR，与 lan_ip 同网段（e2e 机器的 lan_ip 都是 127.0.0.1）。
const (
	NetLAN     = "lan"      // 一块有线 eth0（= fakeagent.DefaultNetInfo）
	NetCX7     = "cx7"      // eth0 + CX-7 口 enp1s0f1np1（mlx5_core，up，无 IP，无 RDMA）
	NetCX7RDMA = "cx7-rdma" // 同 cx7，且 rdma_device = rocep1s0f1、rdma_active、uverbs0
)

// cx7RDMADevices 是 cx7-rdma 机器的 fake 集群数据面在 RDMA Spec 下返回的设备（docker --device）。
var cx7RDMADevices = []string{"/dev/infiniband/rdma_cm", "/dev/infiniband/uverbs0"}

// NetPreset 返回网卡预设的探测结果；"" = nil（fake Agent 用默认 eth0，与 lan 相同）；未知名字 → 错误。
func NetPreset(name, lanIP string) (*clusterspec.NetInfo, error) {
	eth0 := clusterspec.Iface{Name: "eth0", MAC: "02:00:00:00:00:01", Up: true, SpeedMbps: 1000, MTU: 1500,
		IPv4: []string{lanIP + "/24"}, Driver: "r8169", PCI: "0000:01:00.0"}
	cx7 := clusterspec.Iface{Name: "enp1s0f1np1", MAC: "02:00:00:00:01:01", Up: true, SpeedMbps: 200000, MTU: 9000,
		IPv4: []string{}, Driver: "mlx5_core", PCI: "0000:01:00.1", LinkLayer: "Ethernet"}
	switch name {
	case "":
		return nil, nil
	case NetLAN:
		return &clusterspec.NetInfo{Ifaces: []clusterspec.Iface{eth0}}, nil
	case NetCX7:
		return &clusterspec.NetInfo{Ifaces: []clusterspec.Iface{eth0, cx7}}, nil
	case NetCX7RDMA:
		cx7.RDMADevice, cx7.RDMAActive, cx7.Uverbs = "rocep1s0f1", true, "uverbs0"
		return &clusterspec.NetInfo{Ifaces: []clusterspec.Iface{eth0, cx7}}, nil
	}
	return nil, badRequest(fmt.Sprintf("net 只能是 %s / %s / %s", NetLAN, NetCX7, NetCX7RDMA))
}

// agentProfile 是机器的 fake Agent 配置（/hooks/machines 登记时给出）；Agent 被撤销接入后重新接入时沿用。
type agentProfile struct {
	net      string
	oldAgent bool
}

func (s *Server) setProfile(machineID int64, p agentProfile) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.profiles == nil {
		s.profiles = map[int64]agentProfile{}
	}
	s.profiles[machineID] = p
}

func (s *Server) profile(machineID int64) agentProfile {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.profiles[machineID]
}

// applyProfile 把机器的网卡预设与能力位写进 fake Agent 的启动参数（net 已在登记时校验过）。
func applyProfile(p agentProfile, lanIP string, net **clusterspec.NetInfo, feats **uint32, cn **fake.Net) {
	if p.oldAgent {
		f := features.SharedSlots // 旧 Agent：不含 Cluster 位，永不入选集群（后台显示「Agent 不支持集群」）
		*feats = &f
	}
	info, _ := NetPreset(p.net, lanIP)
	*net = info
	if p.net == NetCX7RDMA {
		c := fake.New()
		c.SetDevices(cx7RDMADevices)
		c.SetRuntime(clusterspec.Runtime{HCA: "rocep1s0f1", GIDIndex: 3, RDMAActive: true})
		*cn = c
	}
}

// agentClusterNet 改机器 fake Agent 的集群数据面（见文件头）。
func (s *Server) agentClusterNet(ctx context.Context, body json.RawMessage) (any, error) {
	req, err := decode[struct {
		MachineID int64   `json:"machine_id"`
		FailApply *string `json:"fail_apply"`
		CheckOK   *bool   `json:"check_ok"`
	}](body)
	if err != nil {
		return nil, err
	}
	if req.MachineID <= 0 {
		return nil, badRequest("machine_id 必填")
	}
	ag, err := s.agentFor(ctx, req.MachineID)
	if err != nil {
		return nil, err
	}
	if req.FailApply != nil {
		var ferr error
		if *req.FailApply != "" {
			ferr = errors.New(*req.FailApply)
		}
		ag.ClusterNet().SetFailApply(ferr)
	}
	if req.CheckOK == nil {
		return map[string]bool{"ok": true}, nil
	}
	ag.ClusterNet().SetCheckResult(*req.CheckOK)
	// 步进式 fake Agent 的巡检只在 60 秒定时器上跑：立即巡检一轮，再像 /hooks/agent 一样推进并上报（实例 cluster 状态随报告落库）
	ag.Exec.PatrolClusters(ctx)
	if _, err := s.agent(ctx, json.RawMessage(fmt.Sprintf(`{"machine_id":%d}`, req.MachineID))); err != nil {
		return nil, err
	}
	return map[string]bool{"ok": true}, nil
}

var fabricSeq atomic.Int64

// fabrics 建一个 fabric：{name?, kind, isolation?, rdma?, mtu?, vlan_min?, vlan_max?, description?} → {id, name}。
func (s *Server) fabrics(ctx context.Context, body json.RawMessage) (any, error) {
	req, err := decode[struct {
		Name        string `json:"name"`
		Kind        string `json:"kind"`
		Isolation   string `json:"isolation"`
		RDMA        bool   `json:"rdma"`
		MTU         int    `json:"mtu"`
		VLANMin     *int   `json:"vlan_min"`
		VLANMax     *int   `json:"vlan_max"`
		Description string `json:"description"`
	}](body)
	if err != nil {
		return nil, err
	}
	if req.Name == "" {
		req.Name = fmt.Sprintf("e2e-%s-%s-%d", req.Kind, randx.Digits(4), fabricSeq.Add(1))
	}
	if req.Isolation == "" {
		switch req.Kind {
		case fleet.FabricKindLAN:
			req.Isolation = fleet.IsolationRouted
		case fleet.FabricKindCX7:
			req.Isolation = fleet.IsolationDirect
		}
	}
	if req.Isolation == fleet.IsolationVLAN && req.VLANMin == nil && req.VLANMax == nil {
		lo, hi := 100, 199
		req.VLANMin, req.VLANMax = &lo, &hi
	}
	f, err := s.App.Fleet.CreateFabric(ctx, audit.Actor{}, fleet.FabricInput{Name: req.Name, Kind: req.Kind, Isolation: req.Isolation,
		RDMA: req.RDMA, MTU: req.MTU, VLANMin: req.VLANMin, VLANMax: req.VLANMax, Description: req.Description})
	if err != nil {
		return nil, asBadRequest(err)
	}
	return map[string]any{"id": f.ID, "name": f.Name}, nil
}

// joinFabric 把刚登记的机器加入 fabric（UpdateMachine 同事务重算就绪判定），返回重算后的机器。
func (s *Server) joinFabric(ctx context.Context, machineID, fabricID int64) (fleet.Machine, error) {
	m, err := s.App.Fleet.UpdateMachine(ctx, audit.Actor{}, machineID, fleet.MachineUpdate{SetFabric: true, FabricID: &fabricID})
	if err != nil {
		return fleet.Machine{}, asBadRequest(err)
	}
	return m, nil
}

// asBadRequest 把业务错误（apperr，4xx）转成钩子的 400，其余原样返回（500）。
func asBadRequest(err error) error {
	var ae *apperr.Error
	if errors.As(err, &ae) && ae.Status < 500 {
		return badRequest(ae.Code + ": " + ae.Message)
	}
	return err
}
