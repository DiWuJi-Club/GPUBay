// Package runtime 抽象容器运行时：生产 docker（exec docker CLI），测试 fake。
package runtime

import "context"

type Spec struct {
	InstanceID  string
	ImageRef    string
	LanIP       string // 容器 22 端口发布到 LanIP:SSHPort
	SSHPort     int
	DNS         []string
	SSHDir      string  // 只读挂到 /.spark
	DataDir     string  // 挂到 /root/data
	MemoryBytes int64   // --memory / --memory-swap
	ShmBytes    int64   // --shm-size
	CPUs        float64 // --cpus；0 = 不传（整机不限）
	PidsLimit   int     // --pids-limit，必须为正
	DropNice    bool    // 不加 SYS_NICE（共享机：租户不能抬高自己的调度优先级压制邻居）
	// 计划 13 集群成员（非集群容器三项都为空，命令行与改动前逐字相同）：
	CapDrop []string          // --cap-drop X（集群：NET_RAW，不能发伪造包 / 建 Raw Packet QP）
	Sysctls map[string]string // --sysctl k=v（按 key 排序；集群：net.ipv4.ping_group_range）
	Devices []string          // --device X（RDMA：/dev/infiniband/rdma_cm 与 uverbsN）
}

type Container struct {
	Exists bool
	// FullID：容器完整 ID（64 位十六进制，docker inspect 的 .Id），用来拼 cgroup 路径；List 不填。
	FullID      string
	Running     bool
	Image       string
	Label       string // spark.instance 标签值
	DataMounted bool   // /root/data 有挂载
	DataSource  string // /root/data 的宿主机来源目录（应为该实例自己的数据盘挂载点）
	Pid         int
	// Networks：容器当前挂接的网络名（.NetworkSettings.Networks 的键）。启动失败后 Docker 可能把它清空。
	Networks []string
	// PublishIPs：端口发布绑定的宿主机地址（.HostConfig.PortBindings 的 HostIp，去重、忽略空串）。
	PublishIPs []string
}

type Runtime interface {
	EnsureNetwork(ctx context.Context) error
	Pull(ctx context.Context, ref string) error
	Create(ctx context.Context, spec Spec) error
	Start(ctx context.Context, id string) error
	Stop(ctx context.Context, id string) error
	Remove(ctx context.Context, id string) error
	Inspect(ctx context.Context, id string) (Container, error)
	// List 只填 Exists/Running/Image/Label；Pid、DataMounted、DataSource 为零值，需要时用 Inspect。
	List(ctx context.Context) ([]Container, error)
	// ConnectNetwork 把容器重新挂接到指定网络（容器停止状态下也可执行）。
	ConnectNetwork(ctx context.Context, id, network string) error
	VethOf(ctx context.Context, id string) (string, error)
	// Images 返回本机已缓存的镜像 repository:tag（imageref.Normalize：去重排序、≤ 200 项、每项 ≤ 256 字节、不含 <none>）。
	Images(ctx context.Context) ([]string, error)
}

const Label = "spark.instance"
