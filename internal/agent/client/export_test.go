package client

import (
	"time"

	"spark-cloud/internal/agent/executor"
	"spark-cloud/internal/agentpb"
)

// ToCommandForTest 暴露协议指令到执行器指令的转换。
func ToCommandForTest(cmd *agentpb.Command) executor.Command { return toCommand(cmd) }

// SetMetricsTimeoutForTest 调短单次指标采集的上限；必须在 Run 之前调用。
func SetMetricsTimeoutForTest(c *Client, d time.Duration) { c.metricsTimeout = d }
