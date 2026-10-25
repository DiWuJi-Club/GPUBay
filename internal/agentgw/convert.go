package agentgw

import (
	"encoding/json"
	"fmt"
	"math"

	"spark-cloud/internal/agentpb"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/rental"
)

// maxMetricsInstances 是 last_metrics.instances 最多保留的条数（一台机器的实例数远小于它；防异常 Agent 撑大每次心跳的写入）。
const maxMetricsInstances = 16

func toStateReport(r *agentpb.StateReport) fleet.StateReport {
	out := fleet.StateReport{Instances: []fleet.InstanceState{}}
	for _, s := range r.GetInstances() {
		out.Instances = append(out.Instances, fleet.InstanceState{InstanceID: s.GetInstanceId(), Phase: s.GetPhase()})
	}
	// proto3 的 repeated 空列表在线上不编码：没有镜像的新 Agent 与旧 Agent 一样收到 nil，都不覆盖已有记录。
	out.Images = r.GetImages()
	return out
}

// metricsDoc 是写入 machines.last_metrics 的形态；字段名与前端 Metrics 类型一致。
type metricsDoc struct {
	GPUUtil       float64 `json:"gpu_util"`
	GPUTempC      float64 `json:"gpu_temp_c"`
	GPUPowerW     float64 `json:"gpu_power_w"`
	MemTotal      uint64  `json:"mem_total"`
	MemUsed       uint64  `json:"mem_used"`
	RootDiskTotal uint64  `json:"root_disk_total"`
	RootDiskUsed  uint64  `json:"root_disk_used"`
	DataDiskUsed  uint64  `json:"data_disk_used"`
	Load1         float64 `json:"load1"`
	// Instances 是每个实例容器的 cgroup 用量（计划 10）；旧 Agent 不发，写空数组（不是 null）。
	Instances []instanceMetricsDoc `json:"instances"`
}

// instanceMetricsDoc 是 last_metrics.instances 的元素。
type instanceMetricsDoc struct {
	InstanceID   string  `json:"instance_id"`
	MemCurrent   uint64  `json:"mem_current"`
	MemMax       uint64  `json:"mem_max"` // 0 = 不限
	CPUPercent   float64 `json:"cpu_percent"`
	DataDiskUsed uint64  `json:"data_disk_used"`
}

func finite(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

// metricsJSON 把机器指标与每实例指标转成 JSON；NaN / Inf 置 0，否则 json.Marshal 会失败导致断流。
// 每实例指标只收实例 ID 合法的条目，最多 maxMetricsInstances 条。
func metricsJSON(m *agentpb.Metrics, instances []*agentpb.InstanceMetrics) (json.RawMessage, error) {
	d := metricsDoc{GPUUtil: finite(m.GetGpuUtil()), GPUTempC: finite(m.GetGpuTempC()), GPUPowerW: finite(m.GetGpuPowerW()),
		MemTotal: m.GetMemTotal(), MemUsed: m.GetMemUsed(), RootDiskTotal: m.GetRootDiskTotal(), RootDiskUsed: m.GetRootDiskUsed(),
		DataDiskUsed: m.GetDataDiskUsed(), Load1: finite(m.GetLoad1()), Instances: make([]instanceMetricsDoc, 0, min(len(instances), maxMetricsInstances))}
	for _, x := range instances {
		if len(d.Instances) >= maxMetricsInstances {
			break
		}
		if !rental.InstanceIDPattern.MatchString(x.GetInstanceId()) { // 不是平台实例 ID 的条目不落库
			continue
		}
		d.Instances = append(d.Instances, instanceMetricsDoc{InstanceID: x.GetInstanceId(), MemCurrent: x.GetMemCurrent(), MemMax: x.GetMemMax(),
			CPUPercent: finite(x.GetCpuPercent()), DataDiskUsed: x.GetDataDiskUsed()})
	}
	return json.Marshal(d)
}

// toProtoCommand 把指令转成协议消息；hostKeyPEM 只用于 Create（由调用方从 HostKeys 读取，不经过指令 payload）。
func toProtoCommand(cmd fleet.Command, cfg Config, hostKeyPEM string) (*agentpb.Command, error) {
	out := &agentpb.Command{Id: cmd.ID, InstanceId: cmd.InstanceID}
	switch cmd.Type {
	case fleet.CmdCreateInstance:
		var p fleet.CreateInstancePayload
		if err := json.Unmarshal(cmd.Payload, &p); err != nil {
			return nil, err
		}
		// 端口优先取指令（平台按槽位算好）；为 0 的存量 pending 指令沿用全局配置。
		port := cfg.SSHPort
		if p.SSHPort > 0 {
			port = p.SSHPort
		}
		out.Body = &agentpb.Command_Create{Create: &agentpb.CreateInstance{ImageRef: p.ImageRef, UpstreamPubkey: cfg.UpstreamPubKey,
			BandwidthMbps: uint32(cfg.BandwidthMbps), SshPort: uint32(port), DataSizeGib: uint32(cfg.DataSizeGiB), HostKeyPem: hostKeyPEM,
			ShareSlots: uint32(max(0, p.ShareSlots))}}
	case fleet.CmdStartInstance:
		out.Body = &agentpb.Command_Start{Start: &agentpb.StartInstance{}}
	case fleet.CmdStopInstance:
		out.Body = &agentpb.Command_Stop{Stop: &agentpb.StopInstance{}}
	case fleet.CmdWipeInstance:
		out.Body = &agentpb.Command_Wipe{Wipe: &agentpb.WipeInstance{}}
	case fleet.CmdPullImage:
		var p fleet.PullImagePayload
		if err := json.Unmarshal(cmd.Payload, &p); err != nil {
			return nil, err
		}
		out.Body = &agentpb.Command_Pull{Pull: &agentpb.PullImage{ImageRef: p.ImageRef}}
	default:
		return nil, fmt.Errorf("unknown command type %q", cmd.Type)
	}
	return out, nil
}
