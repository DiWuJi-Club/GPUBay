// Package comfywf 实现 ComfyUI 工作流市场（计划 11）：工作流目录与版本、清单校验、
// 部署上的工作流集合（经 appdeploy 的钩子重新部署安装）与快速出图代理。appdeploy 不 import 本包。
package comfywf

import (
	"slices"
	"time"

	"spark-cloud/internal/storage"
)

const (
	MaxWorkflowsPerDeployment = 8
	MaxGraphBytes             = 262144
	MaxAPIPromptBytes         = 262144
	MaxModelsPerVersion       = 16
	MaxNodesPerVersion        = 8
	MaxInputs                 = 24
	MaxOutputs                = 4
	MaxRunsInFlight           = 2
	RunsPerHour               = 120
	MaxUploadBytes            = 16 << 20
	MaxViewBytes              = 64 << 20
	MaxCoverBytes             = storage.MaxCoverSize // 2MB：storage 的 workflow_cover 用途按它限制
	ProxyTimeout              = 30 * time.Second
	ViewTimeout               = 120 * time.Second
	RunWaitMax                = 30 * time.Minute
	// InstalledFilePrefix 平台放进 user/default/workflows/ 的文件名前缀（spark-<slug>.json）；app.sh 只清理带此前缀的旧文件。
	InstalledFilePrefix = "spark-"
	// ComfyPort 只用于 fake ComfyUI / 测试默认；真实端口取部署 spec.ports[0]。
	ComfyPort = 8188
	// DataDirName 容器内数据盘下的 ComfyUI 目录名（/root/data/comfyui）。
	DataDirName = "comfyui"
)

// 容器内（部署包 / app.sh 两边一致）的相对路径。
const (
	BundleDir          = "workflows"               // 部署包内目录
	BundleManifestName = "workflows/manifest.json" // 部署包内清单
)

// 部署上工作流的安装状态（deployment_workflows.state）。
const (
	WFStatePending   = "pending"
	WFStateInstalled = "installed"
	WFStateFailed    = "failed"
)

// 快速出图运行状态（comfy_workflow_runs.state）。RunRunning 只出现在接口响应里（查 /queue 得到），不落库。
const (
	RunQueued  = "queued"
	RunRunning = "running"
	RunDone    = "done"
	RunError   = "error"
)

// 工作流 / 版本上架状态。
const (
	StatusDraft     = "draft"
	StatusPublished = "published"
	StatusRetired   = "retired"
)

// 分类（D3）。
const (
	CategoryTxt2Img = "txt2img"
	CategoryImg2Img = "img2img"
	CategoryVideo   = "video"
	CategoryUpscale = "upscale"
	CategoryOther   = "other"
)

// Categories 固定分类，顺序即控制台标签顺序。
var Categories = []string{CategoryTxt2Img, CategoryImg2Img, CategoryVideo, CategoryUpscale, CategoryOther}

// ModelDirs 模型目标子目录白名单（/root/data/comfyui/models/<dir>，规格 §3.2）。
var ModelDirs = []string{
	"checkpoints", "diffusion_models", "unet", "text_encoders", "clip", "vae", "loras",
	"upscale_models", "controlnet", "clip_vision", "embeddings", "style_models", "vae_approx",
}

// GitHosts 自定义节点 Git 主机白名单（D5）。
var GitHosts = []string{"github.com", "gitee.com", "gitcode.com"}

// 输入类型（manifest.inputs[].type）。
const (
	InputText     = "text"
	InputTextarea = "textarea"
	InputInt      = "int"
	InputFloat    = "float"
	InputBool     = "bool"
	InputEnum     = "enum"
	InputSeed     = "seed"
	InputImage    = "image"
)

// InputTypes 全部输入类型。
var InputTypes = []string{InputText, InputTextarea, InputInt, InputFloat, InputBool, InputEnum, InputSeed, InputImage}

func ValidCategory(c string) bool { return slices.Contains(Categories, c) }
func ValidModelDir(d string) bool { return slices.Contains(ModelDirs, d) }
func ValidGitHost(h string) bool  { return slices.Contains(GitHosts, h) }
