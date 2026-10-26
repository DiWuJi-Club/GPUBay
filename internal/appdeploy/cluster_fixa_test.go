package appdeploy_test

import (
	"fmt"
	"testing"

	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/audit"
)

// 计划 13 终审 A 组修复的回归测试。

// 后台单条强制卸载不能作用于集群成员部署（否则聚合永远 installing、再部署报 app_cluster_exists）；改走集群级强制卸载。
func TestAdminForceRemoveClusterMember(t *testing.T) {
	e := newEnv(t, 2)
	a, _ := e.publishedApp("vllm-cluster", clusterSpec(appdeploy.ClusterModeRequired, 2, 4), goodScript, []int64{e.imageID})
	cid, _ := e.newCluster(2)
	cd := e.createCluster(cid, a.ID, nil)
	for _, m := range cd.Members {
		e.setDep(m.Deployment.ID, appdeploy.StateRunning, appdeploy.DesiredRunning)
	}
	_, err := e.svc.AdminForceRemove(e.ctx, audit.Actor{AdminID: 7}, cd.Members[0].Deployment.ID, "违规")
	wantCode(t, "force remove member", err, "app_cluster_managed")
	if got := e.dep(cd.Members[0].Deployment.ID); got.State != appdeploy.StateRunning {
		t.Fatalf("member changed: %+v", got)
	}
	_, err = e.svc.AdminForceRemoveCluster(e.ctx, audit.Actor{AdminID: 7}, cd.ID, " ")
	wantCode(t, "empty reason", err, "bad_request")
	got, err := e.svc.AdminForceRemoveCluster(e.ctx, audit.Actor{AdminID: 7}, cd.ID, "违规")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != appdeploy.ClusterDepRemoved {
		t.Fatalf("after force remove = %+v", got)
	}
	for _, m := range got.Members {
		if m.Deployment.State != appdeploy.StateRemoving {
			t.Fatalf("member = %+v", m.Deployment)
		}
	}
	if e.count(`SELECT count(*) FROM audit_logs WHERE action = 'cluster.app_force_remove' AND target_id = $1`, cid) != 1 {
		t.Fatal("audit")
	}
	if e.count(`SELECT count(*) FROM app_deploy_events WHERE deployment_id = $1 AND detail = '平台卸载：违规'`, cd.Members[0].Deployment.ID) != 1 {
		t.Fatal("event detail")
	}
	// 系统集群也能强制卸载；不存在 → not_found
	_, err = e.svc.AdminForceRemoveCluster(e.ctx, audit.Actor{AdminID: 7}, "acd-nope", "x")
	wantCode(t, "missing", err, "not_found")
	// 卸载后可以重新部署
	for _, m := range got.Members {
		if err := e.svc.MarkRemoved(e.ctx, e.pool, e.dep(m.Deployment.ID), "", "已卸载", e.clk.Now()); err != nil {
			t.Fatal(err)
		}
	}
	e.createCluster(cid, a.ID, nil)
}

// 加锁期间成员部署清单变了（换机复制出新部署）：内部重试，持续变化 → 409 app_cluster_busy（不是 500）。
func TestClusterOpRetriesWhenMembersChange(t *testing.T) {
	e := newEnv(t, 8)
	a, _ := e.publishedApp("vllm-cluster", clusterSpec(appdeploy.ClusterModeRequired, 2, 4), goodScript, []int64{e.imageID})
	cid, _ := e.newCluster(2)
	cd := e.createCluster(cid, a.ID, nil)
	for _, m := range cd.Members {
		e.setDep(m.Deployment.ID, appdeploy.StateRunning, appdeploy.DesiredRunning)
	}
	n := 0
	copyMember := func() { // 模拟并发换机：把 rank1 的部署复制到一台新实例上（新实例不在加锁清单里）
		n++
		inst := e.runningInstance()
		e.exec(`INSERT INTO app_deployments SELECT (jsonb_populate_record(NULL::app_deployments,
			to_jsonb(d) || jsonb_build_object('id', $1::text, 'instance_id', $2::text))).* FROM app_deployments d WHERE id = $3`,
			fmt.Sprintf("dep-copy%05d", n), inst.ID, cd.Members[1].Deployment.ID)
	}
	appdeploy.SetClusterOpHook(e.svc, func(attempt int) {
		if attempt == 1 {
			copyMember()
		}
	})
	if _, err := e.svc.StopCluster(e.ctx, e.userID, cd.ID, ""); err != nil {
		t.Fatalf("stop with one concurrent change = %v", err)
	}
	appdeploy.SetClusterOpHook(e.svc, func(int) { copyMember() })
	_, err := e.svc.StartCluster(e.ctx, e.userID, cd.ID, "")
	wantCode(t, "persistent change", err, "app_cluster_busy")
	appdeploy.SetClusterOpHook(e.svc, nil)
}
