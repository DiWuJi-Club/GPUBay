package fleet_test

import (
	"encoding/json"
	"testing"

	"spark-cloud/internal/clusterspec"
	"spark-cloud/internal/fleet"
)

// 计划 13：非集群的载荷 / 状态报告 JSON 与改动前逐字节相同；集群字段往返。
func TestClusterPayloadJSON(t *testing.T) {
	b, _ := json.Marshal(fleet.CreateInstancePayload{ImageRef: "x:1", SSHPort: 2222})
	if string(b) != `{"image_ref":"x:1","ssh_port":2222}` {
		t.Fatalf("non-cluster payload = %s", b)
	}
	b, _ = json.Marshal(fleet.StateReport{Instances: []fleet.InstanceState{{InstanceID: "ins-aaaaaaaa", Phase: "running"}},
		Images: []string{"a"}, Net: &clusterspec.NetInfo{LanIface: "eth0"}})
	if string(b) != `{"instances":[{"instance_id":"ins-aaaaaaaa","phase":"running"}]}` {
		t.Fatalf("non-cluster report = %s", b)
	}
	b, _ = json.Marshal(fleet.InstanceState{InstanceID: "ins-aaaaaaaa", Phase: "running", Cluster: "ok"})
	if string(b) != `{"instance_id":"ins-aaaaaaaa","phase":"running","cluster":"ok"}` {
		t.Fatalf("cluster state = %s", b)
	}

	spec := clusterspec.Spec{ClusterID: "clu-aaaaaaaaaa", Version: 1, Size: 2, Rank: 0, Mode: clusterspec.ModeRouted,
		Iface: "eth0", Subnet: "10.210.0.0/24", Members: []clusterspec.Member{
			{Rank: 0, InstanceID: "ins-aaaaaaaa", IP: "10.210.0.11"},
			{Rank: 1, InstanceID: "ins-bbbbbbbb", IP: "10.210.0.12", HostIP: "10.0.0.2"},
		}}
	b, _ = json.Marshal(fleet.CreateInstancePayload{ImageRef: "x:1", Cluster: &spec})
	var back fleet.CreateInstancePayload
	if err := json.Unmarshal(b, &back); err != nil || back.Cluster == nil || back.Cluster.Members[1].HostIP != "10.0.0.2" {
		t.Fatalf("cluster payload round trip: %s, %+v, %v", b, back, err)
	}
	b, _ = json.Marshal(fleet.ConfigureClusterPayload{Cluster: spec})
	var cc fleet.ConfigureClusterPayload
	if err := json.Unmarshal(b, &cc); err != nil || cc.Cluster.ClusterID != spec.ClusterID || cc.Cluster.Validate() != nil {
		t.Fatalf("configure payload round trip: %s, %+v, %v", b, cc, err)
	}
	if fleet.CmdConfigureCluster != "configure_cluster" {
		t.Fatal(fleet.CmdConfigureCluster)
	}
}
