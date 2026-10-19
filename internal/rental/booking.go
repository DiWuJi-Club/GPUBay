package rental

import (
	"time"

	"spark-cloud/internal/catalog"
)

// 计划 6：档期预订、批次、取消与自动续费的参数（规格 §13）。全部是代码常量，不做配置。
const (
	MinBookingLead             = 2 * time.Hour       // 最早开始：下单后 2 小时
	BookingLeadTolerance       = 5 * time.Minute     // booking_too_soon 的容差（对话框打开到提交之间）
	MaxBookingAhead            = 60 * 24 * time.Hour // 最多提前 60 天
	MaxLeaseSpan               = 90 * 24 * time.Hour // 单次 duration × quantity 上限
	MaxMachinesPerOrder        = 8
	MaxOpenReservationsPerUser = 16               // 未开通（reserved）实例数
	PrepLead                   = time.Hour        // 开始前多久开通
	BookingGrace               = 30 * time.Minute // 开始后仍无机器 → 平台取消
	Tail                       = RetainAfterStop + WipeBuffer
	CancelCoolOff              = 15 * time.Minute // 下单后冷静期：全额退
	FreeCancelBefore           = 24 * time.Hour   // 距开始 ≥ 24h 全额退
	LateCancelRefundPercent    = 50               // 否则退 50%
	AutoRenewLead              = time.Hour        // 到期前 1 小时扣款
	AutoRenewRetry             = time.Minute      // 余额不足的重试间隔
	AutoRenewRetryBlocked      = 10 * time.Minute // renew_blocked 的重试间隔
)

// MaxQuantity 返回某档位在 90 天上限下最多能买几个单位。
func MaxQuantity(d catalog.Duration) int {
	if !d.Valid() {
		return 0
	}
	return int(MaxLeaseSpan / d.Length())
}
