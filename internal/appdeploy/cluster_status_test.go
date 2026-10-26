package appdeploy_test

import (
	"testing"

	"spark-cloud/internal/appdeploy"
)

// mem 造一个成员：state / health / progress / phase，rank0 为 head；current = false 表示实例已不是集群当前成员。
func mem(rank int, state, health string, progress int, phase string, current bool) appdeploy.ClusterMemberDeployment {
	role := appdeploy.RoleWorker
	if rank == 0 {
		role = appdeploy.RoleHead
	}
	return appdeploy.ClusterMemberDeployment{Rank: rank, Role: role, Current: current,
		Deployment: appdeploy.Deployment{State: state, Health: health, Progress: progress, Phase: phase, ClusterRole: role}}
}

// 规格 §6.4 每一条规则一个用例（按规则顺序：前面的规则优先）。
func TestAggregateClusterTable(t *testing.T) {
	const (
		ok, bad, unk = appdeploy.HealthOK, appdeploy.HealthUnhealthy, appdeploy.HealthUnknown
	)
	cases := []struct {
		name                 string
		members              []appdeploy.ClusterMemberDeployment
		status, health, phase string
		progress             int
	}{
		{"任一 failed → failed（优先于 removing）",
			[]appdeploy.ClusterMemberDeployment{mem(0, appdeploy.StateRunning, ok, 100, "serve", true), mem(1, appdeploy.StateFailed, unk, 30, "download", true),
				mem(2, appdeploy.StateRemoving, unk, 0, "", true)},
			appdeploy.ClusterStatusFailed, unk, "serve", 0},
		{"任一 removing → removing",
			[]appdeploy.ClusterMemberDeployment{mem(0, appdeploy.StateRemoving, unk, 100, "", true), mem(1, appdeploy.StateRemoved, unk, 100, "", true)},
			appdeploy.ClusterStatusRemoving, unk, "", 100},
		{"全部 removed / lost → removed",
			[]appdeploy.ClusterMemberDeployment{mem(0, appdeploy.StateRemoved, unk, 100, "", true), mem(1, appdeploy.StateLost, unk, 0, "", false)},
			appdeploy.ClusterStatusRemoved, unk, "", 0},
		{"全部 running 且 ok → running",
			[]appdeploy.ClusterMemberDeployment{mem(0, appdeploy.StateRunning, ok, 100, "serve", true), mem(1, appdeploy.StateRunning, ok, 100, "worker", true)},
			appdeploy.ClusterStatusRunning, ok, "serve", 100},
		{"全部 running 但有 unhealthy → 不是 running（安装中），health unhealthy",
			[]appdeploy.ClusterMemberDeployment{mem(0, appdeploy.StateRunning, ok, 100, "serve", true), mem(1, appdeploy.StateRunning, bad, 100, "", true)},
			appdeploy.ClusterStatusInstalling, bad, "serve", 100},
		{"全部 stopped → stopped",
			[]appdeploy.ClusterMemberDeployment{mem(0, appdeploy.StateStopped, unk, 100, "", true), mem(1, appdeploy.StateStopped, unk, 100, "", true)},
			appdeploy.ClusterStatusStopped, unk, "", 100},
		{"有成员已不是当前成员 → stale",
			[]appdeploy.ClusterMemberDeployment{mem(0, appdeploy.StateRunning, ok, 100, "serve", true), mem(1, appdeploy.StateLost, unk, 100, "", false),
				mem(1, appdeploy.StateRunning, ok, 100, "", true)},
			appdeploy.ClusterStatusStale, unk, "serve", 100},
		{"成员没了（removed 且非当前）→ stale",
			[]appdeploy.ClusterMemberDeployment{mem(0, appdeploy.StateRunning, ok, 100, "serve", true), mem(1, appdeploy.StateRemoved, unk, 100, "", false)},
			appdeploy.ClusterStatusStale, unk, "serve", 100},
		{"其余 → installing：进度取最小、阶段取 head",
			[]appdeploy.ClusterMemberDeployment{mem(0, appdeploy.StateInstalling, unk, 40, "download", true), mem(1, appdeploy.StateQueued, unk, 0, "", true),
				mem(2, appdeploy.StateRunning, ok, 100, "worker", true)},
			appdeploy.ClusterStatusInstalling, unk, "download", 0},
		{"running 与 stopped 混合 → installing",
			[]appdeploy.ClusterMemberDeployment{mem(0, appdeploy.StateRunning, ok, 100, "serve", true), mem(1, appdeploy.StateStopped, unk, 100, "", true)},
			appdeploy.ClusterStatusInstalling, unk, "serve", 100},
		{"没有成员 → removed", nil, appdeploy.ClusterStatusRemoved, unk, "", 0},
	}
	for _, c := range cases {
		status, health, phase, progress := appdeploy.AggregateCluster(c.members)
		if status != c.status || health != c.health || phase != c.phase || progress != c.progress {
			t.Errorf("%s: got (%s, %s, %q, %d), want (%s, %s, %q, %d)", c.name, status, health, phase, progress, c.status, c.health, c.phase, c.progress)
		}
	}
}
