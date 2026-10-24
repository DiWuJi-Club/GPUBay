package tools

// Labels 是 23 个工具的中文名（SSE tool / confirm 事件的 label、前端步骤行）；与 web/shared/src/labels.ts 的 butlerToolLabel 逐项一致（labels_test.go 核对）。
var Labels = map[string]string{
	"list_products": "查询套餐", "check_availability": "查询可租时段", "list_apps": "查询应用目录", "get_app": "查看应用详情",
	"list_instances": "查询实例列表", "get_instance": "查看实例详情", "get_instance_events": "查看实例动态", "list_instance_apps": "查询实例上的应用",
	"get_deployment_log": "读取部署日志", "get_deployment_events": "查看部署事件", "get_wallet": "查询钱包", "list_orders": "查询订单",
	"get_cancel_quote": "查询取消退款", "get_help": "查阅帮助", "generate_secret": "生成随机密钥",
	"place_order": "租用实例", "renew_instance": "续费实例", "release_instance": "释放实例", "cancel_booking": "取消预订",
	"deploy_app": "部署应用", "redeploy_app": "重新部署应用", "set_app_state": "停止 / 启动应用", "remove_app": "卸载应用",
}

// ReadToolNames / WriteToolNames 是规格 §4 固定的工具名（读 15、写 8）。
var (
	ReadToolNames = []string{"list_products", "check_availability", "list_apps", "get_app", "list_instances", "get_instance",
		"get_instance_events", "list_instance_apps", "get_deployment_log", "get_deployment_events", "get_wallet", "list_orders",
		"get_cancel_quote", "get_help", "generate_secret"}
	WriteToolNames = []string{"place_order", "renew_instance", "release_instance", "cancel_booking", "deploy_app", "redeploy_app",
		"set_app_state", "remove_app"}
)
