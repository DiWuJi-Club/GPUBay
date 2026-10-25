package appdeploy

import "time"

type PortSpec struct {
	Port       int    `json:"port"`
	Label      string `json:"label,omitempty"`
	Path       string `json:"path,omitempty"`
	HealthPath string `json:"health_path,omitempty"` // 只在 Ports[0] 上有意义；空 = TCP 连通检查
}

type EnvSpec struct {
	Name     string   `json:"name"`
	Label    string   `json:"label,omitempty"`
	Type     string   `json:"type"` // string / int / bool / enum / secret
	Required bool     `json:"required,omitempty"`
	Options  []string `json:"options,omitempty"`
	Default  string   `json:"default,omitempty"`
	Pattern  string   `json:"pattern,omitempty"`
	MaxLen   int      `json:"max_len,omitempty"` // 0 = 256
	Min      *int64   `json:"min,omitempty"`
	Max      *int64   `json:"max,omitempty"`
	Help     string   `json:"help,omitempty"`
}

type LinkSpec struct {
	Label string `json:"label"`
	Path  string `json:"path"`
}

// Spec 是版本的结构化描述（app_versions.spec），校验规则见 ValidateSpec。
type Spec struct {
	Ports          []PortSpec `json:"ports"`
	TimeoutMinutes int        `json:"timeout_minutes,omitempty"`
	MinDataFreeGiB int        `json:"min_data_free_gib,omitempty"`
	Env            []EnvSpec  `json:"env,omitempty"`
	Links          []LinkSpec `json:"links,omitempty"`
	// SupportsWorkflows（计划 11）：该版本的 app.sh 认识部署包里的 workflows/（ComfyUI 工作流市场）；
	// 为真时 bundleFor 经 Deps.BundleExtra 追加工作流文件。false 省略，不改变既有版本的 spec_hash。
	SupportsWorkflows bool `json:"supports_workflows,omitempty"`
}

type App struct {
	ID          int64     `json:"id"`
	Slug        string    `json:"slug"`
	Name        string    `json:"name"`
	Summary     string    `json:"summary"`
	Description string    `json:"description"`
	Category    string    `json:"category"`
	Icon        string    `json:"icon"`
	Sort        int       `json:"sort"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type Version struct {
	ID          int64      `json:"id"`
	AppID       int64      `json:"app_id"`
	Version     string     `json:"version"`
	Status      string     `json:"status"`
	ImageIDs    []int64    `json:"image_ids"`
	Spec        Spec       `json:"spec"`
	Script      string     `json:"-"` // 单独接口返回
	SpecHash    string     `json:"spec_hash"`
	Changelog   string     `json:"changelog"`
	PublishedAt *time.Time `json:"published_at"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// Deployment 是 app_deployments 一行；对外视图见 view.go（不直接序列化本结构）。
type Deployment struct {
	ID               string
	InstanceID       string
	UserID           int64
	AppID            int64
	AppVersionID     int64
	State            string
	Desired          string
	Phase            string
	Progress         int
	Message          string
	Health           string
	ErrorCode        string
	Env              map[string]string
	SecretEnvEnc     []byte
	Port             int
	LogTail          string
	StatusSeq        int64
	Attempt          int
	SpecHash         string
	NextPollAt       time.Time
	LastPolledAt     *time.Time
	CleanupPending   bool
	UnreachableCount int
	CrashCount       int
	// RowVersion 是行的乐观并发版本号（app_deployments.row_version）：每个写本行的地方都 +1，
	// Runner 回写时按它比较（假时钟下 updated_at 会撞车，不能用来判断并发修改）。
	RowVersion int
	StartedAt  *time.Time
	FinishedAt *time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Live 表示部署仍占用 app_deployments_live_uniq（非终态）。
func (d Deployment) Live() bool { return d.State != StateRemoved && d.State != StateLost }

// WorkflowRef 是预装 / 换机时部署「应装工作流集合」的一项（deployment_workflows 的 workflow_id + workflow_version_id）。
// 由 Deps.ResolveWorkflows（comfywf 实现）给出，appdeploy 不解释其内容。
type WorkflowRef struct {
	WorkflowID, VersionID int64
}

type Event struct {
	ID           int64     `json:"id"`
	DeploymentID string    `json:"deployment_id"`
	At           time.Time `json:"at"`
	FromState    string    `json:"from_state"`
	ToState      string    `json:"to_state"`
	Detail       string    `json:"detail"`
}
