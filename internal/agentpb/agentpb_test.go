package agentpb_test

import (
	"testing"

	"google.golang.org/protobuf/proto"

	"spark-cloud/internal/agentpb"
)

func TestCommandRoundTrip(t *testing.T) {
	in := &agentpb.PlatformMessage{Msg: &agentpb.PlatformMessage_Command{Command: &agentpb.Command{
		Id: "cmd-1", InstanceId: "ins-abcdefgh",
		Body: &agentpb.Command_Create{Create: &agentpb.CreateInstance{ImageRef: "nvcr.io/x:1", UpstreamPubkey: "ssh-ed25519 AAAA", BandwidthMbps: 200, SshPort: 2222, DataSizeGib: 2048}},
	}}}
	b, err := proto.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out agentpb.PlatformMessage
	if err := proto.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	c := out.GetCommand()
	if c.GetId() != "cmd-1" || c.GetCreate().GetSshPort() != 2222 || c.GetCreate().GetImageRef() != "nvcr.io/x:1" {
		t.Fatalf("round trip = %v", &out)
	}
	if got := agentpb.AgentGateway_ServiceDesc.ServiceName; got != "spark.agent.v1.AgentGateway" {
		t.Fatalf("service = %s", got)
	}
}

// 计划 13：集群字段与 ConfigureCluster 指令往返。
func TestClusterMessagesRoundTrip(t *testing.T) {
	spec := &agentpb.ClusterSpec{
		ClusterId: "clu-7f3k2m9q1a", Version: 2, Size: 2, Rank: 1, Mode: "routed", Iface: "enP7s7",
		Vlan: 0, Mtu: 9000, Subnet: "10.210.7.0/24", Rdma: true,
		Members: []*agentpb.ClusterMember{
			{Rank: 0, InstanceId: "ins-aaaaaaaa", Ip: "10.210.7.11", HostIp: "192.168.1.221"},
			{Rank: 1, InstanceId: "ins-bbbbbbbb", Ip: "10.210.7.12", HostIp: "192.168.1.222"},
		},
	}
	msgs := []*agentpb.PlatformMessage{
		{Msg: &agentpb.PlatformMessage_Command{Command: &agentpb.Command{Id: "cmd-2", InstanceId: "ins-bbbbbbbb",
			Body: &agentpb.Command_Create{Create: &agentpb.CreateInstance{ImageRef: "x:1", Cluster: spec}}}}},
		{Msg: &agentpb.PlatformMessage_Command{Command: &agentpb.Command{Id: "cmd-3", InstanceId: "ins-bbbbbbbb",
			Body: &agentpb.Command_ConfigureCluster{ConfigureCluster: &agentpb.ConfigureCluster{Cluster: spec}}}}},
	}
	for i, in := range msgs {
		b, err := proto.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		var out agentpb.PlatformMessage
		if err := proto.Unmarshal(b, &out); err != nil {
			t.Fatal(err)
		}
		c := out.GetCommand()
		got := c.GetCreate().GetCluster()
		if i == 1 {
			got = c.GetConfigureCluster().GetCluster()
		}
		if !proto.Equal(got, spec) || got.GetMembers()[1].GetHostIp() != "192.168.1.222" || got.GetMtu() != 9000 {
			t.Fatalf("msg %d round trip = %v", i, &out)
		}
	}

	net := &agentpb.NetInfo{LanIface: "enP7s7", Ifaces: []*agentpb.NetIface{{
		Name: "enp1s0f1np1", Mac: "aa:bb", Up: true, SpeedMbps: 200000, Mtu: 9000, Ipv4: []string{"10.1.0.1/24"},
		Driver: "mlx5_core", Pci: "0000:01:00.1", Wireless: false, RdmaDevice: "rocep1s0f1", RdmaActive: true, Uverbs: "uverbs1", LinkLayer: "Ethernet",
	}}}
	agentMsgs := []*agentpb.AgentMessage{
		{Msg: &agentpb.AgentMessage_Hello{Hello: &agentpb.Hello{AgentVersion: "v1", Features: 3, Net: net}}},
		{Msg: &agentpb.AgentMessage_State{State: &agentpb.StateReport{
			Instances: []*agentpb.InstanceState{{InstanceId: "ins-aaaaaaaa", Phase: "running", Cluster: "error: x"}}, Net: net}}},
	}
	for i, in := range agentMsgs {
		b, err := proto.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		var out agentpb.AgentMessage
		if err := proto.Unmarshal(b, &out); err != nil {
			t.Fatal(err)
		}
		got := out.GetHello().GetNet()
		if i == 1 {
			got = out.GetState().GetNet()
			if out.GetState().GetInstances()[0].GetCluster() != "error: x" {
				t.Fatalf("instance cluster = %v", &out)
			}
		} else if out.GetHello().GetFeatures() != 3 {
			t.Fatalf("features = %v", &out)
		}
		if !proto.Equal(got, net) || got.GetIfaces()[0].GetUverbs() != "uverbs1" {
			t.Fatalf("agent msg %d round trip = %v", i, &out)
		}
	}
}
