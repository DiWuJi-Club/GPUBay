package client

import "time"

// SetMetricsTimeoutForTest 调短单次指标采集的上限；必须在 Run 之前调用。
func SetMetricsTimeoutForTest(c *Client, d time.Duration) { c.metricsTimeout = d }
