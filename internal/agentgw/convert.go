package agentgw

import (
	"encoding/json"
	"fmt"
	"math"

	"spark-cloud/internal/agentpb"
	"spark-cloud/internal/fleet"
)

func toStateReport(r *agentpb.StateReport) fleet.StateReport {
	out := fleet.StateReport{Instances: []fleet.InstanceState{}}
	for _, s := range r.GetInstances() {
		out.Instances = append(out.Instances, fleet.InstanceState{InstanceID: s.GetInstanceId(), Phase: s.GetPhase()})
	}
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
}

func finite(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

// metricsJSON 把指标转成 JSON；NaN / Inf 置 0，否则 json.Marshal 会失败导致断流。
func metricsJSON(m *agentpb.Metrics) (json.RawMessage, error) {
	d := metricsDoc{GPUUtil: finite(m.GetGpuUtil()), GPUTempC: finite(m.GetGpuTempC()), GPUPowerW: finite(m.GetGpuPowerW()),
		MemTotal: m.GetMemTotal(), MemUsed: m.GetMemUsed(), RootDiskTotal: m.GetRootDiskTotal(), RootDiskUsed: m.GetRootDiskUsed(),
		DataDiskUsed: m.GetDataDiskUsed(), Load1: finite(m.GetLoad1())}
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
		out.Body = &agentpb.Command_Create{Create: &agentpb.CreateInstance{ImageRef: p.ImageRef, UpstreamPubkey: cfg.UpstreamPubKey,
			BandwidthMbps: uint32(cfg.BandwidthMbps), SshPort: uint32(cfg.SSHPort), DataSizeGib: uint32(cfg.DataSizeGiB), HostKeyPem: hostKeyPEM}}
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
