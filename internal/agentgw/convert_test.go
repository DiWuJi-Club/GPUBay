package agentgw

import (
	"encoding/json"
	"testing"

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
