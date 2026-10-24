package comfywf

import (
	"encoding/json"
	"time"
)

// Workflow 是 comfy_workflows 一行。
type Workflow struct {
	ID          int64     `json:"id"`
	Slug        string    `json:"slug"`
	Name        string    `json:"name"`
	Summary     string    `json:"summary"`
	Description string    `json:"description"`
	Category    string    `json:"category"`
	Tags        []string  `json:"tags"`
	CoverKey    *string   `json:"-"` // 对外只给 cover_url（视图里拼）
	Sort        int       `json:"sort"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Version 是 comfy_workflow_versions 一行。内部类型：带 api_prompt 本体，不直接序列化到控制台（见视图）。
type Version struct {
	ID           int64           `json:"id"`
	WorkflowID   int64           `json:"workflow_id"`
	Version      string          `json:"version"`
	Status       string          `json:"status"`
	Graph        json.RawMessage `json:"graph"`
	APIPrompt    json.RawMessage `json:"api_prompt"` // nil = 不支持快速出图
	Manifest     Manifest        `json:"manifest"`
	DownloadGB   string          `json:"download_gb"` // NUMERIC(8,2) 原文（如 "6.90"），不经 float 往返
	ManifestHash string          `json:"manifest_hash"`
	Changelog    string          `json:"changelog"`
	PublishedAt  *time.Time      `json:"published_at"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
}

// Manifest 是版本的清单（comfy_workflow_versions.manifest，规格 §3.2），校验见 ValidateManifest。
type Manifest struct {
	ComfyUIMin string       `json:"comfyui_min,omitempty"`
	Models     []ModelSpec  `json:"models"`
	Nodes      []NodeSpec   `json:"nodes"`
	Inputs     []InputSpec  `json:"inputs"`
	Outputs    []OutputSpec `json:"outputs"`
}

// MarshalJSON 把 nil 切片写成 []：清单的 JSON 形状固定（manifest_hash、前端、app.sh 都不必区分 null 与空数组）。
func (m Manifest) MarshalJSON() ([]byte, error) {
	type plain Manifest
	p := plain(m)
	if p.Models == nil {
		p.Models = []ModelSpec{}
	}
	if p.Nodes == nil {
		p.Nodes = []NodeSpec{}
	}
	if p.Inputs == nil {
		p.Inputs = []InputSpec{}
	}
	if p.Outputs == nil {
		p.Outputs = []OutputSpec{}
	}
	return json.Marshal(p)
}

// ModelSpec 一个模型文件：放到 /root/data/comfyui/models/<Dir>/<Name>；ModelScope / HF 至少一个。
type ModelSpec struct {
	Name       string    `json:"name"`
	Dir        string    `json:"dir"`
	SizeGB     float64   `json:"size_gb"`
	ModelScope *RepoFile `json:"modelscope,omitempty"`
	HF         *RepoFile `json:"hf,omitempty"`
	SHA256     string    `json:"sha256"` // 空 = 不校验
}

type RepoFile struct {
	Repo string `json:"repo"`
	File string `json:"file"`
}

// NodeSpec 一个自定义节点：Git 地址 + 钉死的 40 位 commit；NodeTypes 用于 D16 的 graph 交叉校验。
type NodeSpec struct {
	Name      string   `json:"name"`
	Git       string   `json:"git"`
	Commit    string   `json:"commit"`
	NodeTypes []string `json:"node_types"`
}

// InputSpec 快速出图表单的一项：值写入 api_prompt[Node].inputs[Input]。
type InputSpec struct {
	Key     string   `json:"key"`
	Label   string   `json:"label"`
	Type    string   `json:"type"` // text / textarea / int / float / bool / enum / seed / image
	Node    string   `json:"node"`
	Input   string   `json:"input"`
	Default any      `json:"default,omitempty"`
	Min     *float64 `json:"min,omitempty"`
	Max     *float64 `json:"max,omitempty"`
	Step    *float64 `json:"step,omitempty"`
	MaxLen  int      `json:"max_len,omitempty"`
	Options []string `json:"options,omitempty"`
}

// OutputSpec 快速出图收集输出的节点（api_prompt 的键）。
type OutputSpec struct {
	Node string `json:"node"`
}

// DeploymentWorkflow 是 deployment_workflows 一行。
type DeploymentWorkflow struct {
	DeploymentID string
	WorkflowID   int64
	VersionID    int64
	State        string
	AddedAt      time.Time
	InstalledAt  *time.Time
}

// Run 是 comfy_workflow_runs 一行。
type Run struct {
	ID           string
	DeploymentID string
	UserID       int64
	VersionID    int64
	PromptID     string
	Seed         *int64
	State        string
	CreatedAt    time.Time
	FinishedAt   *time.Time
}
