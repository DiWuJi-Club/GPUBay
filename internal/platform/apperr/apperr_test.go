package apperr

import "testing"

// 计划 11 新增的错误码（规格 §10）：code / HTTP 状态 / 文案。
func TestWorkflowErrors(t *testing.T) {
	cases := []struct {
		err    *Error
		code   string
		status int
		msg    string
	}{
		{ErrWorkflowNotFound, "workflow_not_found", 404, "工作流不存在或未上架"},
		{ErrWorkflowUnavailable, "workflow_unavailable", 409, "该工作流当前没有可用版本"},
		{ErrWorkflowExists, "workflow_exists", 409, "该工作流已安装在此 ComfyUI 上"},
		{ErrWorkflowLimit, "workflow_limit", 409, "每个 ComfyUI 最多安装 8 个工作流"},
		{ErrWorkflowAppUnsupported, "workflow_app_unsupported", 409, "当前 ComfyUI 应用版本不支持工作流市场"},
		{ErrWorkflowVersionFrozen, "workflow_version_frozen", 409, "已发布的工作流版本不能修改"},
		{ErrWorkflowBusy, "workflow_busy", 429, "ComfyUI 正在处理其它任务，请稍后再试"},
		{ErrUpstreamUnreachable, "upstream_unreachable", 503, "无法连接实例内的 ComfyUI"},
		{WorkflowVersionInvalid("manifest.models[2].repo", "格式不正确"), "workflow_version_invalid", 400, "manifest.models[2].repo：格式不正确"},
		{WorkflowPromptRejected("Value not in list"), "workflow_prompt_rejected", 422, "ComfyUI 拒绝了该任务：Value not in list"},
		{WorkflowInputInvalid("提示词", "必填"), "workflow_input_invalid", 400, "提示词：必填"},
	}
	for _, c := range cases {
		if c.err.Code != c.code || c.err.Status != c.status || c.err.Message != c.msg {
			t.Errorf("%s = {%q %d %q}, want {%q %d %q}", c.code, c.err.Code, c.err.Status, c.err.Message, c.code, c.status, c.msg)
		}
	}
}
