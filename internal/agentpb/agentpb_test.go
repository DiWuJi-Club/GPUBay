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
