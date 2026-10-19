// Package pdu 定义智能 PDU 插件：按插座断电重启与查询通电状态。
// DGX Spark 没有 BMC，断电重启是唯一的远程硬恢复手段，只由管理员手动触发。
package pdu

import "context"

// 默认按 APC PowerNet MIB：sPDUOutletCtl.<插座号>，写 3 为重启，读到 1 为通电。其他品牌在 PDU 配置里改。
const (
	DefaultOIDPrefix  = "1.3.6.1.4.1.318.1.1.4.4.2.1.3"
	DefaultCycleValue = 3
	DefaultOnValue    = 1
)

type Driver interface {
	PowerCycle(ctx context.Context, outlet int) error
	Status(ctx context.Context, outlet int) (on bool, err error)
}
