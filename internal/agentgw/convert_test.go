package agentgw

import (
	"encoding/json"
	"fmt"
	"math"
	"testing"

	"spark-cloud/internal/agentpb"
	"spark-cloud/internal/fleet"
)

func createCmd(t *testing.T, p fleet.CreateInstancePayload) fleet.Command {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return fleet.Command{ID: "c1", InstanceID: "i1", Type: fleet.CmdCreateInstance, Payload: b}
}

func TestToProtoCommandCreatePortAndShareSlots(t *testing.T) {
	cfg := Config{SSHPort: 2222, BandwidthMbps: 100, DataSizeGiB: 2048, UpstreamPubKey: "ssh-ed25519 AAA"}
	cases := []struct {
		name      string
		payload   fleet.CreateInstancePayload
		wantPort  uint32
		wantSlots uint32
	}{
		{"指令带端口与份数", fleet.CreateInstancePayload{ImageRef: "img:1", SSHPort: 2224, ShareSlots: 4}, 2224, 4},
		{"整机指令", fleet.CreateInstancePayload{ImageRef: "img:1", SSHPort: 2222, ShareSlots: 1}, 2222, 1},
		{"存量指令无端口退回配置", fleet.CreateInstancePayload{ImageRef: "img:1"}, 2222, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg, err := toProtoCommand(createCmd(t, tc.payload), cfg, "PEM")
			if err != nil {
				t.Fatal(err)
			}
			c := msg.GetCreate()
			if c == nil {
				t.Fatal("not a create command")
			}
			if c.GetSshPort() != tc.wantPort || c.GetShareSlots() != tc.wantSlots {
				t.Fatalf("port=%d slots=%d, want %d %d", c.GetSshPort(), c.GetShareSlots(), tc.wantPort, tc.wantSlots)
			}
			if c.GetImageRef() != "img:1" || c.GetHostKeyPem() != "PEM" || c.GetDataSizeGib() != 2048 || c.GetBandwidthMbps() != 100 {
				t.Fatalf("other fields changed: %+v", c)
			}
		})
	}
}

func TestCreateInstancePayloadJSONCompat(t *testing.T) {
	// 存量 pending 指令只有 image_ref，解析后端口与份数为 0。
	var p fleet.CreateInstancePayload
	if err := json.Unmarshal([]byte(`{"image_ref":"img:1"}`), &p); err != nil {
		t.Fatal(err)
	}
	if p.SSHPort != 0 || p.ShareSlots != 0 {
		t.Fatalf("%+v", p)
	}
	b, _ := json.Marshal(fleet.CreateInstancePayload{ImageRef: "img:1", SSHPort: 2223, ShareSlots: 2})
	if string(b) != `{"image_ref":"img:1","ssh_port":2223,"share_slots":2}` {
		t.Fatalf("%s", b)
	}
}

// last_metrics 的结构快照：机器级字段 + instances 数组（无实例时为 []，不是 null）。
func TestMetricsJSONInstancesSnapshot(t *testing.T) {
	m := &agentpb.Metrics{GpuUtil: 12, GpuTempC: 55, GpuPowerW: 30, MemTotal: 100, MemUsed: 40, RootDiskTotal: 1000, RootDiskUsed: 500, DataDiskUsed: 7, Load1: 0.5}
	got, err := metricsJSON(m, []*agentpb.InstanceMetrics{
		{InstanceId: "ins-aaaaaaaa", MemCurrent: 1 << 30, MemMax: 2 << 30, CpuPercent: 150.5, DataDiskUsed: 4096},
		{InstanceId: "ins-bbbbbbbb", CpuPercent: math.NaN()},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"gpu_util":12,"gpu_temp_c":55,"gpu_power_w":30,"mem_total":100,"mem_used":40,"root_disk_total":1000,"root_disk_used":500,` +
		`"data_disk_used":7,"load1":0.5,"instances":[` +
		`{"instance_id":"ins-aaaaaaaa","mem_current":1073741824,"mem_max":2147483648,"cpu_percent":150.5,"data_disk_used":4096},` +
		`{"instance_id":"ins-bbbbbbbb","mem_current":0,"mem_max":0,"cpu_percent":0,"data_disk_used":0}]}`
	if string(got) != want {
		t.Fatalf("metrics json =\n%s\nwant\n%s", got, want)
	}
	// 旧 Agent（心跳不带 instances）→ "instances":[]
	got, err = metricsJSON(&agentpb.Metrics{Load1: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"gpu_util":0,"gpu_temp_c":0,"gpu_power_w":0,"mem_total":0,"mem_used":0,"root_disk_total":0,"root_disk_used":0,"data_disk_used":0,"load1":1,"instances":[]}`; string(got) != want {
		t.Fatalf("old agent metrics json = %s", got)
	}
}

// 心跳 instances 落库有界：实例 ID 不合法的条目跳过，最多 16 条。
func TestMetricsJSONInstancesBounded(t *testing.T) {
	in := []*agentpb.InstanceMetrics{{InstanceId: ""}, {InstanceId: "ins-ABCDEFGH"}, {InstanceId: "ins-abc"}, {InstanceId: "../etc/passwd"},
		{InstanceId: "ins-abcdefgh1"}, {InstanceId: "xins-abcdefgh"}}
	for i := 0; i < 20; i++ {
		in = append(in, &agentpb.InstanceMetrics{InstanceId: fmt.Sprintf("ins-%08d", i), MemCurrent: uint64(i)})
	}
	got, err := metricsJSON(&agentpb.Metrics{}, in)
	if err != nil {
		t.Fatal(err)
	}
	var doc metricsDoc
	if err := json.Unmarshal(got, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Instances) != maxMetricsInstances || maxMetricsInstances != 16 {
		t.Fatalf("instances = %d, want 16", len(doc.Instances))
	}
	for i, x := range doc.Instances {
		if x.InstanceID != fmt.Sprintf("ins-%08d", i) || x.MemCurrent != uint64(i) {
			t.Fatalf("instances[%d] = %+v", i, x)
		}
	}
}
