// Package appdeploy 实现 AI 应用一键部署（计划 7）：应用目录、部署记录、容器内运行时（run.sh）与平台侧 Runner。
package appdeploy

import "time"

const (
	MaxAppsPerInstance    = 3
	PollInterval          = 5 * time.Second
	HealthInterval        = 30 * time.Second
	ExecUploadTimeout     = 60 * time.Second
	ExecStatusTimeout     = 20 * time.Second
	DefaultTimeoutMinutes = 60
	MaxTimeoutMinutes     = 180
	MaxLogTail            = 65536
	MaxBundleBytes        = 4 << 20 // 计划 11 D2：8 个工作流 × 256KB + 脚本
	MaxScriptBytes        = 65536
	MaxStatusBytes        = 16384
	MaxExecPerRound       = 16
	UnreachableFailAfter  = 10
	CrashedAfter          = 3
	DefaultAppRoot        = "/root/data"
	AppDirName            = ".spark-apps"
	ModelsDirName         = "models"
	MinSecretLen          = 8
	UserOpsPerHour        = 60
	MaxMessageLen         = 200
	NotRunningRetry       = 30 * time.Second
	UnreachableRetry      = 30 * time.Second
	advisoryLockName      = "appdeploy"
)

const (
	StateQueued     = "queued"
	StateInstalling = "installing"
	StateRunning    = "running"
	StateStopped    = "stopped"
	StateFailed     = "failed"
	StateRemoving   = "removing"
	StateRemoved    = "removed"
	StateLost       = "lost"

	DesiredRunning = "running"
	DesiredStopped = "stopped"
	DesiredRemoved = "removed"

	HealthUnknown   = "unknown"
	HealthOK        = "ok"
	HealthUnhealthy = "unhealthy"

	ErrCodeScriptFailed = "script_failed"
	ErrCodeTimeout      = "timeout"
	ErrCodeHealthFailed = "health_failed"
	ErrCodeUploadFailed = "upload_failed"
	ErrCodeUnreachable  = "unreachable"
	ErrCodePortConflict = "port_conflict"
	ErrCodePortLimit    = "port_limit"
	ErrCodeInstanceGone = "instance_gone"
	ErrCodeCrashed      = "crashed"
	ErrCodeDisk         = "disk"
)

// TerminalStates 不再被 Runner 处理、也不占 app_deployments_live_uniq。
var TerminalStates = []string{StateRemoved, StateLost}

// ErrorText 错误码的中文（事件 detail 与控制台）。
var ErrorText = map[string]string{
	ErrCodeScriptFailed: "脚本执行失败", ErrCodeTimeout: "部署超时", ErrCodeHealthFailed: "健康检查未通过", ErrCodeUploadFailed: "部署包上传失败",
	ErrCodeUnreachable: "无法连接容器", ErrCodePortConflict: "端口冲突", ErrCodePortLimit: "端口数量超限", ErrCodeInstanceGone: "实例已不存在",
	ErrCodeCrashed: "服务进程已退出", ErrCodeDisk: "数据盘空间不足",
}
