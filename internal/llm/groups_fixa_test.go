package llm_test

import (
	"strings"
	"testing"

	"spark-cloud/internal/llm"
	"spark-cloud/internal/platform/apperr"
)

// 计划 13 终审 A 组修复的回归测试。

// ready 组的成员机器 faulty：Prober 立即判不健康（不等探测失败），Controller 随后整组 failed（M12）。
func TestProberReadyGroupMachineFaulty(t *testing.T) {
	ne := newNodeEnv(t)
	_, n := ne.readyGroup(2)
	ms := ne.members(n.ID)
	ne.exec(`UPDATE machines SET state = 'faulty' WHERE id = $1`, ms[1].MachineID)
	ne.probeTick()
	if got := ne.node(n.ID); got.State != llm.NodeUnhealthy || !strings.Contains(got.Error, "rank 1") || !strings.Contains(got.Error, "机器故障") {
		t.Fatalf("after probe %+v", got)
	}
	ne.tick()
	if got := ne.node(n.ID); got.State != llm.NodeFailed {
		t.Fatalf("after tick %+v", got)
	}
}

// 补组遇 cluster no_capacity（例如该 fabric 的 VLAN 用尽）：换下一个候选 fabric；全部失败 → 告警 llm_pool_short。
func TestProvisionGroupNoCapacityTriesNextFabric(t *testing.T) {
	t.Run("next fabric", func(t *testing.T) {
		ne := newNodeEnv(t)
		fa, fb := ne.fabric("cx7", "vlan"), ne.fabric("cx7", "vlan")
		ne.fabricMachine("A1", fa, "l2")
		ne.fabricMachine("A2", fa, "l2")
		ne.fabricMachine("B1", fb, "l2")
		ne.fabricMachine("B2", fb, "l2")
		m := ne.groupModel("qwen3-32b", 2, 1, ne.clusterVersion("1.1.0", 4, 360))
		ne.rental.placeClusterErrFn = func(call int) error {
			if call == 1 {
				return apperr.ClusterNoCapacity(2)
			}
			return nil
		}
		rep := ne.tick()
		if rep.Provisioned != 1 || len(ne.rental.clusterPlaceCalls()) != 2 || len(ne.nodesOf(m.ID)) != 1 {
			t.Fatalf("report %+v calls %d nodes %d", rep, len(ne.rental.clusterPlaceCalls()), len(ne.nodesOf(m.ID)))
		}
		if ne.alertCount(llm.AlertPoolShort) != 0 {
			t.Fatal("no pool short alert when the next fabric works")
		}
	})
	t.Run("all fail", func(t *testing.T) {
		ne := newNodeEnv(t)
		fa, fb := ne.fabric("cx7", "vlan"), ne.fabric("cx7", "vlan")
		ne.fabricMachine("A1", fa, "l2")
		ne.fabricMachine("A2", fa, "l2")
		ne.fabricMachine("B1", fb, "l2")
		ne.fabricMachine("B2", fb, "l2")
		m := ne.groupModel("qwen3-32b", 2, 1, ne.clusterVersion("1.1.0", 4, 360))
		ne.rental.placeClusterErrFn = func(int) error { return apperr.ClusterNoCapacity(2) }
		ne.tick()
		if len(ne.nodesOf(m.ID)) != 0 || len(ne.rental.clusterPlaceCalls()) != 2 {
			t.Fatalf("nodes %d calls %d", len(ne.nodesOf(m.ID)), len(ne.rental.clusterPlaceCalls()))
		}
		if ne.alertCount(llm.AlertPoolShort) != 1 {
			t.Fatalf("pool short alerts %d", ne.alertCount(llm.AlertPoolShort))
		}
	})
}
