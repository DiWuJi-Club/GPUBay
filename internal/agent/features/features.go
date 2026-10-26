// Package features 定义 Agent 在 Hello.features 里上报的能力位图，Agent 与网关共用。
package features

// SharedSlots（bit0）：Agent 支持 CreateInstance.share_slots，按份等分内存 / CPU / 数据盘配额，
// 可在一台机器上承载多个共享实例。共享机已搁置（计划 10 PoC no-go），平台目前不读取该位，见 docs/superpowers/followups-plan10.md。
const SharedSlots uint32 = 1 << 0

// Cluster（bit1，计划 13）：Agent 支持 CreateInstance.cluster 与 ConfigureCluster（集群第二网卡 spc0、
// cap-drop NET_RAW、RDMA 设备）。平台据 machines.agent_features 判定，只把集群成员分到带该位的机器
// （旧 Agent 会静默忽略 cluster 字段，必须门控）。
const Cluster uint32 = 1 << 1
