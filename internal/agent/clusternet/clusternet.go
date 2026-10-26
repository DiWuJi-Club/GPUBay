// Package clusternet 定义 Agent 的集群数据面接口（计划 13 规格 §3、§4.4）：
// 容器第二网卡 spc0（routed：veth + 宿主机 /32 路由 + SPARK-CLU；macvlan：CX-7 口 / VLAN 子接口）、
// RDMA 设备与 RoCE GID、网卡探测。实现：linux（clusternet/linux，T2）、fake（clusternet/fake，测试与 e2e）。
package clusternet

import (
	"context"
	"errors"

	"spark-cloud/internal/clusterspec"
)

// Net 是 Agent 的集群数据面。所有方法幂等；调用方（executor）负责串行化（opMu）。
type Net interface {
	// Probe 探测本机网卡（只读 sysfs）；lanIP 用来认出管理网卡（NetInfo.LanIface）。
	Probe(ctx context.Context, lanIP string) (clusterspec.NetInfo, error)
	// Devices 返回 docker create 要加的 --device：RDMA 时 /dev/infiniband/rdma_cm 与 /dev/infiniband/uverbsN；非 RDMA 返回 nil。
	Devices(s clusterspec.Spec) ([]string, error)
	// Apply 在容器（netns = pid）里建 / 修 spc0 并配好宿主机侧路由；RDMA 时查 RoCE v2 GID。
	Apply(ctx context.Context, instanceID string, pid int, s clusterspec.Spec) (clusterspec.Runtime, error)
	// Check 是 60 秒巡检：spc0 在且有地址、routed 的宿主机 /32 路由在、macvlan 父接口无地址等。
	// ok = false 表示需要修复（重新 Apply），此时 err 为问题描述（可展示，进 InstanceState.cluster 的 "error: …"）。
	Check(ctx context.Context, instanceID string, pid int, s clusterspec.Spec) (bool, error)
	// Teardown 清理该实例在宿主机上的集群网络（对端路由、veth / macvlan、无人使用的 VLAN 子接口）；remaining 是本机其余集群实例的 Spec。
	Teardown(ctx context.Context, instanceID string, s clusterspec.Spec, remaining []clusterspec.Spec) error
	// SyncRules 按本机全部 routed 成员重建 SPARK-CLU（实例 ID → 本机 routed 成员 Spec）并保证跳转在链首。
	SyncRules(ctx context.Context, routed map[string]clusterspec.Spec) error
}

// ErrNoGID：RDMA 集群的 spc0 已配好，但宿主机 GID 表里找不到对应的 RoCE v2 GID（Apply 同时返回 Runtime{RDMAActive: false}）；
// 调用方据此退回 TCP（NCCL_IB_DISABLE=1），不当作失败。linux 实现返回它，executor 用 errors.Is 判断。
var ErrNoGID = errors.New("clusternet: RoCE GID not found")
