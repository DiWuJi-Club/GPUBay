package rental

import "time"

// 计划 13：实例集群（instance_clusters；用户集群与 Token 商城节点组共用）的类型与常量。
// 行为（分配、视图、生命周期）由后续任务实现：cluster_place.go / cluster_view.go / cluster_spec.go（T6）、cluster_life.go 等（T10）。

// 集群状态：表里只有 active / released / failed；forming / ready / degraded 由成员派生（规格 §5.4）。
const (
	ClusterActive   = "active"
	ClusterReleased = "released"
	ClusterFailed   = "failed"
	ClusterForming  = "forming"
	ClusterReady    = "ready"
	ClusterDegraded = "degraded"
)

// ClusterSizes 是产品开放的集群台数（M1；表约束 2..8）。
var ClusterSizes = []int{2, 4}

// ClusterAlignSlack：成员到期时刻允许的差（只用于「到期不一致」提示与不变量）。
const ClusterAlignSlack = 60 * time.Second

// ClusterFormTimeout：组建期（ready_at 为空）超过它仍未就绪 → 整组中止。
const ClusterFormTimeout = 30 * time.Minute

// ClusterMemberStates 是「在位成员」的实例状态（ClusterSpecFor、视图与守卫共用）。
var ClusterMemberStates = []string{StateCreating, StateRunning, StateStopped}

// ConfigureClusterCommandTimeout 是 configure_cluster 指令的超时（与 Agent 侧一致）。
const ConfigureClusterCommandTimeout = 2 * time.Minute

// Cluster 是 instance_clusters 的一行。
type Cluster struct {
	ID          string     `json:"id"`
	UserID      int64      `json:"user_id"`
	System      bool       `json:"system"`
	Size        int        `json:"size"`
	FabricID    int64      `json:"fabric_id"`
	Mode        string     `json:"mode"`
	VLAN        *int       `json:"vlan"`
	RDMA        bool       `json:"rdma"`
	MTU         int        `json:"mtu"`
	SubnetIndex int        `json:"subnet_index"`
	Version     int        `json:"version"`
	State       string     `json:"state"`
	BatchID     *string    `json:"batch_id"`
	ReadyAt     *time.Time `json:"ready_at"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	EndedAt     *time.Time `json:"ended_at"`
}

// ClusterMemberView 是集群视图里的一个成员（活动成员，按 rank）。
type ClusterMemberView struct {
	Rank         int        `json:"rank"`
	InstanceID   string     `json:"instance_id"`
	State        string     `json:"state"`
	Suspended    bool       `json:"suspended"`
	MachineID    int64      `json:"machine_id"`
	MachineLabel string     `json:"machine_label"`
	IP           string     `json:"ip"`
	NetStatus    string     `json:"net_status"` // last_state_report 里该实例的 cluster 字段（"" / ok / pending / error: …）
	ExpiresAt    *time.Time `json:"expires_at"`
}

// ClusterView 是控制台 / 后台展示的集群。
type ClusterView struct {
	Cluster
	Status     string              `json:"status"`  // forming / ready / degraded / released / failed
	Network    string              `json:"network"` // rdma / ethernet / rdma_degraded
	FabricName string              `json:"fabric_name"`
	FabricKind string              `json:"fabric_kind"`
	Subnet     string              `json:"subnet"`
	HeadIP     string              `json:"head_ip"`
	Members    []ClusterMemberView `json:"members"`
}

// ClusterAvail 是某个台数的集群可用性：可组成的集群数与是否有 RDMA fabric 能组。
type ClusterAvail struct {
	Available int  `json:"available"`
	RDMA      bool `json:"rdma"`
}

// ClusterFilter 是后台集群列表的筛选条件（nil / "" = 不限）。
type ClusterFilter struct {
	State    string
	UserID   *int64
	FabricID *int64
	System   *bool
}

// SystemClusterRequest：Token 商城节点组（PlaceSystemCluster）——指定的池内机器（同一 fabric）各建一个系统实例。
type SystemClusterRequest struct {
	MachineIDs []int64
	GroupID    int64
	ImageID    int64
	Note       string
}

// SystemClusterResult：Instances[i].ClusterRank == i（rank 按机器 ID 升序分配）。
type SystemClusterResult struct {
	Cluster   Cluster
	Instances []Instance
}
