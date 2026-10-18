package rental

import "fmt"

// 告警正文必须稳定（不含数值），RaiseAlertOnce 按 kind + message 去重。
func AlertContainerDown(serial, instanceID string) string {
	return fmt.Sprintf("机器 %s 上实例 %s 的容器未在运行", serial, instanceID)
}
func AlertContainerMissing(serial, instanceID string) string {
	return fmt.Sprintf("机器 %s 上实例 %s 的容器已不存在", serial, instanceID)
}
func AlertDataDiskMissing(serial, instanceID string) string {
	return fmt.Sprintf("机器 %s 上实例 %s 的数据盘未挂载", serial, instanceID)
}
func AlertOrphanContainer(serial, instanceID string) string {
	return fmt.Sprintf("机器 %s 上存在不属于当前实例的容器 %s", serial, instanceID)
}
func AlertOrphanWipeFailed(serial, instanceID string) string {
	return fmt.Sprintf("机器 %s 上实例 %s 的残留容器清理失败", serial, instanceID)
}
func AlertInvalidReport(serial string) string {
	return fmt.Sprintf("机器 %s 上报了无法识别的实例标识", serial)
}
func AlertSSHBruteforce(instanceID string) string {
	return fmt.Sprintf("实例 %s 的 SSH 登录失败次数超过阈值，详情见日志", instanceID)
}
func AlertUpstreamUnreachable(serial, instanceID string) string {
	return fmt.Sprintf("机器 %s 上实例 %s 的 SSH 上游无法连接", serial, instanceID)
}
