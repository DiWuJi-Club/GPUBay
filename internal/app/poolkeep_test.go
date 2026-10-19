package app_test

import (
	"testing"

	"spark-cloud/internal/app"
	"spark-cloud/internal/rental"
)

// 裁定 5：暂停（机器失联）仍是 running，上游连接池条目不丢；停机 / 释放才丢。
func TestPoolKeepIgnoresSuspend(t *testing.T) {
	keep := app.PoolKeep([]rental.IngressRoute{
		{InstanceID: "ins-run", State: rental.StateRunning},
		{InstanceID: "ins-sus", State: rental.StateRunning, Suspended: true},
		{InstanceID: "ins-stop", State: rental.StateStopped},
	})
	if !keep["ins-run"] || !keep["ins-sus"] || keep["ins-stop"] {
		t.Fatalf("keep = %v", keep)
	}
}
