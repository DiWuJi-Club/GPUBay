package agentgw

import (
	"encoding/json"
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
