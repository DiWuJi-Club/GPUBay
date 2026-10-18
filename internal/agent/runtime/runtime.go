// Package runtime 抽象容器运行时：生产 docker（exec docker CLI），测试 fake。
package runtime

import "context"

type Spec struct {
	InstanceID  string
	ImageRef    string
	LanIP       string // 容器 22 端口发布到 LanIP:SSHPort
	SSHPort     int
	DNS         []string
	SSHDir      string // 只读挂到 /.spark
	DataDir     string // 挂到 /root/data
	MemoryBytes int64  // --memory / --memory-swap
	ShmBytes    int64  // --shm-size
}

type Container struct {
	Exists      bool
	Running     bool
	Image       string
	Label       string // spark.instance 标签值
	DataMounted bool   // /root/data 有挂载
	Pid         int
}

type Runtime interface {
	EnsureNetwork(ctx context.Context) error
	Pull(ctx context.Context, ref string) error
	Create(ctx context.Context, spec Spec) error
	Start(ctx context.Context, id string) error
	Stop(ctx context.Context, id string) error
	Remove(ctx context.Context, id string) error
	Inspect(ctx context.Context, id string) (Container, error)
	List(ctx context.Context) ([]Container, error)
	VethOf(ctx context.Context, id string) (string, error)
}

const Label = "spark.instance"
