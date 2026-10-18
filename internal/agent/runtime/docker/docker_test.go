package docker_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"spark-cloud/internal/agent/cmdrun"
	"spark-cloud/internal/agent/runtime"
	"spark-cloud/internal/agent/runtime/docker"
)

func newRT() (*docker.Runtime, *cmdrun.Recorder) {
	r := &cmdrun.Recorder{}
	return &docker.Runtime{Run: r, Bridge: "spark0", Subnet: "10.200.0.0/24"}, r
}

func TestEnsureNetworkCreatesOnce(t *testing.T) {
	rt, r := newRT()
	r.Stub("docker network inspect", "", &cmdrun.CmdError{ExitCode: 1, Stderr: "Error response from daemon: network spark0 not found"})
	if err := rt.EnsureNetwork(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := "docker network inspect --format {{.Id}} spark0\n" +
		"docker network create --driver bridge --subnet 10.200.0.0/24 --ipv6=false -o com.docker.network.bridge.name=spark0 -o com.docker.network.bridge.enable_icc=false -o com.docker.network.bridge.enable_ip_masquerade=true spark0"
	if r.Lines() != want {
		t.Fatalf("calls:\n%s", r.Lines())
	}
	r.Reset()
	r.Stub("docker network inspect", "abc123\n", nil)
	_ = rt.EnsureNetwork(context.Background())
	if r.Lines() != "docker network inspect --format {{.Id}} spark0" {
		t.Fatalf("existing network recreated:\n%s", r.Lines())
	}
}

func TestCreateCommandSnapshot(t *testing.T) {
	rt, r := newRT()
	spec := runtime.Spec{InstanceID: "ins-abcdefgh", ImageRef: "nvcr.io/nvidia/cuda:13.0.1-base", LanIP: "192.168.1.221", SSHPort: 2222,
		DNS: []string{"223.5.5.5", "119.29.29.29"}, SSHDir: "/var/lib/spark-agent/ssh/ins-abcdefgh", DataDir: "/var/lib/spark-agent/mnt/ins-abcdefgh",
		MemoryBytes: 128849018880, ShmBytes: 64424509440}
	if err := rt.Create(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	want := "docker create --name ins-abcdefgh --label spark.instance=ins-abcdefgh --gpus all " +
		"--shm-size 64424509440 --memory 128849018880 --memory-swap 128849018880 --pids-limit 32768 --oom-score-adj 500 " +
		"--cap-add IPC_LOCK --cap-add SYS_NICE --cap-add SYS_PTRACE --ulimit memlock=-1:-1 --ulimit stack=67108864:67108864 " +
		"--network spark0 --dns 223.5.5.5 --dns 119.29.29.29 --dns-search . --restart no " +
		"-p 192.168.1.221:2222:22/tcp " +
		"-v /var/lib/spark-agent/ssh/ins-abcdefgh:/.spark:ro -v /var/lib/spark-agent/mnt/ins-abcdefgh:/root/data " +
		"--entrypoint /.spark/init.sh nvcr.io/nvidia/cuda:13.0.1-base"
	if r.Lines() != want {
		t.Fatalf("create:\n got %s\nwant %s", r.Lines(), want)
	}
}

func TestStartStopRemovePull(t *testing.T) {
	rt, r := newRT()
	ctx := context.Background()
	_ = rt.Start(ctx, "ins-a")
	r.Stub("docker stop", "", &cmdrun.CmdError{ExitCode: 1, Stderr: "Error response from daemon: No such container: ins-a"})
	if err := rt.Stop(ctx, "ins-a"); err != nil {
		t.Fatalf("stop missing container must succeed: %v", err)
	}
	r.Stub("docker rm", "", &cmdrun.CmdError{ExitCode: 1, Stderr: "No such container"})
	if err := rt.Remove(ctx, "ins-a"); err != nil {
		t.Fatalf("remove missing must succeed: %v", err)
	}
	r.Stub("docker rm", "", &cmdrun.CmdError{ExitCode: 1, Stderr: "permission denied"})
	if err := rt.Remove(ctx, "ins-a"); err == nil {
		t.Fatal("other rm errors must surface")
	}
	_ = rt.Pull(ctx, "nvcr.io/x:1")
	if r.Lines() != "docker start ins-a\ndocker stop -t 30 ins-a\ndocker rm -f -v ins-a\ndocker rm -f -v ins-a\ndocker pull nvcr.io/x:1" {
		t.Fatalf("calls:\n%s", r.Lines())
	}
}

func TestInspectListAndVeth(t *testing.T) {
	rt, r := newRT()
	ctx := context.Background()
	r.Stub("docker inspect --type container --format {{json .}} ins-missing", "", &cmdrun.CmdError{ExitCode: 1, Stderr: "error: no such object: ins-missing"})
	c, err := rt.Inspect(ctx, "ins-missing")
	if err != nil || c.Exists {
		t.Fatalf("missing = %+v %v", c, err)
	}
	r.Stub("docker inspect --type container --format {{json .}} ins-a",
		`{"State":{"Running":true,"Pid":4242},"Config":{"Image":"nvcr.io/x:1","Labels":{"spark.instance":"ins-a"}},"Mounts":[{"Destination":"/root/data","Source":"/var/lib/spark-agent/mnt/ins-a"},{"Destination":"/.spark","Source":"/x"}]}`, nil)
	c, err = rt.Inspect(ctx, "ins-a")
	if err != nil || !c.Exists || !c.Running || c.Image != "nvcr.io/x:1" || c.Label != "ins-a" || !c.DataMounted || c.DataSource != "/var/lib/spark-agent/mnt/ins-a" || c.Pid != 4242 {
		t.Fatalf("inspect = %+v %v", c, err)
	}
	r.Stub("docker inspect --type container --format {{json .}} ins-b", `{"State":{"Running":false},"Config":{"Image":"img","Labels":{}},"Mounts":[]}`, nil)
	c, _ = rt.Inspect(ctx, "ins-b")
	if c.DataMounted || c.DataSource != "" || c.Label != "" || c.Running {
		t.Fatalf("inspect b = %+v", c)
	}
	r.Stub("docker inspect --type container --format {{json .}} ins-err", "", errors.New("docker daemon down"))
	if _, err := rt.Inspect(ctx, "ins-err"); err == nil {
		t.Fatal("daemon error must surface")
	}
	r.Stub("docker ps", `{"Names":"ins-a","State":"running","Labels":"spark.instance=ins-a,other=x","Image":"img"}`+"\n"+`{"Names":"ins-c","State":"exited","Labels":"spark.instance=ins-c","Image":"img"}`+"\n", nil)
	list, err := rt.List(ctx)
	if err != nil || len(list) != 2 || list[0].Label != "ins-a" || !list[0].Running || list[1].Label != "ins-c" || list[1].Running {
		t.Fatalf("list = %+v %v", list, err)
	}
	r.Reset()
	r.Stub("docker inspect --type container --format {{.State.Pid}} ins-a", "4242\n", nil)
	r.Stub("nsenter -t 4242 -n ip -o link show dev eth0", "16: eth0@if17: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 ...\n", nil)
	r.Stub("ip -o link show", "1: lo: <LOOPBACK> ...\n17: veth1a2b3c@if16: <BROADCAST,MULTICAST,UP> ...\n18: vethzzz@if20: ...\n", nil)
	veth, err := rt.VethOf(ctx, "ins-a")
	if err != nil || veth != "veth1a2b3c" {
		t.Fatalf("veth = %q %v", veth, err)
	}
	if r.Lines() != "docker inspect --type container --format {{.State.Pid}} ins-a\nnsenter -t 4242 -n ip -o link show dev eth0\nip -o link show" {
		t.Fatalf("calls:\n%s", r.Lines())
	}
}

// 容器 eth0 输出里没有 @ifN（不是 veth）时必须报错，不能拿空索引去匹配宿主机接口。
func TestVethOfRejectsNonVeth(t *testing.T) {
	rt, r := newRT()
	r.Stub("docker inspect --type container --format {{.State.Pid}} ins-a", "4242\n", nil)
	r.Stub("nsenter -t 4242 -n ip -o link show dev eth0", "16: eth0: <BROADCAST,MULTICAST,UP> ...\n", nil)
	if _, err := rt.VethOf(context.Background(), "ins-a"); err == nil {
		t.Fatal("eth0 without @ifN must be an error")
	}
	if strings.Contains(r.Lines(), "ip -o link show\n") || strings.HasSuffix(r.Lines(), "\nip -o link show") {
		t.Fatalf("must not scan host links:\n%s", r.Lines())
	}
}

func TestNotFoundLegacyWording(t *testing.T) {
	rt, r := newRT()
	r.Stub("docker network inspect", "", &cmdrun.CmdError{ExitCode: 1, Stderr: "No such network"})
	if err := rt.EnsureNetwork(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Lines(), "docker network create") {
		t.Fatalf("calls:\n%s", r.Lines())
	}
}

func TestCreateRejectsInvalidSpec(t *testing.T) {
	good := runtime.Spec{InstanceID: "ins-a", ImageRef: "img", LanIP: "192.168.1.221", SSHPort: 2222, SSHDir: "/s", DataDir: "/d", MemoryBytes: 1, ShmBytes: 1}
	muts := map[string]func(*runtime.Spec){
		"lanip": func(s *runtime.Spec) { s.LanIP = "" }, "port": func(s *runtime.Spec) { s.SSHPort = 0 },
		"image": func(s *runtime.Spec) { s.ImageRef = "" }, "sshdir": func(s *runtime.Spec) { s.SSHDir = "" },
		"datadir": func(s *runtime.Spec) { s.DataDir = "" }, "mem": func(s *runtime.Spec) { s.MemoryBytes = 0 },
		"shm": func(s *runtime.Spec) { s.ShmBytes = 0 }, "id": func(s *runtime.Spec) { s.InstanceID = "" },
	}
	for name, m := range muts {
		rt, r := newRT()
		s := good
		m(&s)
		if err := rt.Create(context.Background(), s); err == nil {
			t.Errorf("%s: want error", name)
		}
		if r.Lines() != "" {
			t.Errorf("%s: docker executed: %s", name, r.Lines())
		}
	}
}
