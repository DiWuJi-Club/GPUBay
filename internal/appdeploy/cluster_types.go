package appdeploy

import "time"

// 计划 13：集群部署（app_cluster_deployments）——一次在集群全部成员上部署同一应用版本，rank0 = head、其余 = worker。
// 行为（CreateCluster / RedeployCluster / 聚合状态等）由 T7 实现。

// ClusterSupport 是版本 spec 的 cluster 段（规格 §6.1）。
type ClusterSupport struct {
	Mode        string `json:"mode"` // optional / required
	MinSize     int    `json:"min_size"`
	MaxSize     int    `json:"max_size"`
	WorkerPorts bool   `json:"worker_ports,omitempty"` // false：worker 部署不登记 instance_ports
}

// ClusterMaxSize 是 spec.cluster.max_size 的上限（与 instance_clusters.size 的表约束一致）。
const ClusterMaxSize = 8

const (
	ClusterModeOptional = "optional" // 单实例与集群都能部署
	ClusterModeRequired = "required" // 只能在集群上部署

	RoleHead   = "head"   // rank0
	RoleWorker = "worker" // 其余成员

	// app_cluster_deployments.state
	ClusterDepActive  = "active"
	ClusterDepRemoved = "removed"
)

// ClusterDeployment.Status：由成员部署聚合（规格 §6.4）。
const (
	ClusterStatusInstalling = "installing"
	ClusterStatusRunning    = "running"
	ClusterStatusFailed     = "failed"
	ClusterStatusStopped    = "stopped"
	ClusterStatusStale      = "stale" // 有成员已不是集群当前成员（换机），需要重新部署
	ClusterStatusRemoving   = "removing"
	ClusterStatusRemoved    = "removed"
)

// ClusterCreateRequest：在集群上部署应用（VersionID nil = 最新 published）。
type ClusterCreateRequest struct {
	UserID    int64
	ClusterID string
	AppID     int64
	VersionID *int64
	Env       map[string]string
	IP        string
}

// ClusterRedeployRequest：整组重新部署（可换版本 / 变量；EnvSet = false 时沿用原变量）。
type ClusterRedeployRequest struct {
	UserID              int64
	ClusterDeploymentID string
	VersionID           *int64
	Env                 map[string]string
	EnvSet              bool
	IP                  string
}

// ClusterMemberDeployment 是集群部署的一个成员部署。
type ClusterMemberDeployment struct {
	Rank       int
	Role       string
	InstanceID string
	Deployment Deployment
	Current    bool // 实例仍是集群当前成员
}

// ClusterDeployment 是一条集群部署及其聚合状态。
type ClusterDeployment struct {
	ID, ClusterID               string
	UserID, AppID, AppVersionID int64
	State                       string // active / removed
	Status, Health, Phase       string // 聚合（§6.4）
	Progress                    int
	Members                     []ClusterMemberDeployment
	CreatedAt, UpdatedAt        time.Time
}
