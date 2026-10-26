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

// 计划 13 新增的错误码（规格 §10）：code / HTTP 状态 / 文案。
func TestClusterErrors(t *testing.T) {
	cases := []struct {
		err    *Error
		code   string
		status int
		msg    string
	}{
		{ErrClusterNotFound, "cluster_not_found", 404, "集群不存在"},
		{ErrClusterMember, "cluster_member", 409, "该实例属于集群，请在集群页统一续费、设置自动续费或释放"},
		{ErrClusterSizeInvalid, "cluster_size_invalid", 400, "集群只支持 2 台或 4 台"},
		{ErrClusterBookingUnsupported, "cluster_booking_unsupported", 400, "集群暂不支持预订，请选择立即开通"},
		{ErrFabricNotFound, "fabric_not_found", 404, "集群网络不存在"},
		{ErrFabricInUse, "fabric_in_use", 409, "该集群网络下还有机器或运行中的集群"},
		{ErrFabricFull, "fabric_full", 409, "直连网络最多 2 台机器"},
		{FabricInvalid("vlan_min", "需在 2～4094 之间"), "fabric_invalid", 400, "vlan_min：需在 2～4094 之间"},
		{FabricInvalid("", "直连网络不能用 VLAN"), "fabric_invalid", 400, "直连网络不能用 VLAN"},
		{ErrAppClusterRequired, "app_cluster_required", 400, "该应用需要在集群上部署"},
		{ErrAppClusterUnsupported, "app_cluster_unsupported", 400, "该应用不支持集群部署"},
		{AppClusterSize(2, 4), "app_cluster_size", 400, "该应用支持 2～4 台的集群"},
		{ErrAppClusterExists, "app_cluster_exists", 409, "集群上已有集群应用，请先卸载"},
		{ErrAppClusterManaged, "app_cluster_managed", 409, "该部署属于集群应用，请在集群页操作"},
		{ErrAppClusterBusy, "app_cluster_busy", 409, "集群成员正在变更，请稍后重试"},
		{ClusterNoCapacity(4), "no_capacity", 409, "暂无可组成 4 台集群的空闲机器（需同一高速网络）"},
	}
	for _, c := range cases {
		if c.err.Code != c.code || c.err.Status != c.status || c.err.Message != c.msg {
			t.Errorf("%s = {%q %d %q}, want {%q %d %q}", c.code, c.err.Code, c.err.Status, c.err.Message, c.code, c.status, c.msg)
		}
	}
}
