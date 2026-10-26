package appdeploy_test

import (
	"testing"

	"spark-cloud/internal/appdeploy"
)

// 计划 13 T1：app_deployments.cluster_deployment_id / cluster_role 经 depCols / scanDeployment 读出。
func TestDeploymentClusterColumns(t *testing.T) {
	e := newEnv(t, 1)
	a, v := e.publishedApp("vllm-x", goodSpec(), goodScript, []int64{e.imageID})
	inst := e.runningInstance()
	d := e.create(inst, a.ID, map[string]string{"MODEL_ID": "Qwen/Qwen3-8B"})
	if d.ClusterDeploymentID != nil || d.ClusterRole != "" {
		t.Fatalf("non-cluster deployment: %v %q", d.ClusterDeploymentID, d.ClusterRole)
	}
	e.exec(`INSERT INTO cluster_fabrics (name, kind, isolation, created_at, updated_at) VALUES ('以太网', 'lan', 'routed', now(), now())`)
	e.exec(`INSERT INTO instance_clusters (id, user_id, size, fabric_id, mode, rdma, subnet_index, state, created_at, updated_at)
		VALUES ('clu-aaaaaaaaaa', $1, 2, (SELECT id FROM cluster_fabrics), 'routed', false, 0, 'active', now(), now())`, e.userID)
	e.exec(`INSERT INTO app_cluster_deployments (id, cluster_id, user_id, app_id, app_version_id, state, created_at, updated_at)
		VALUES ('acd-000000000001', 'clu-aaaaaaaaaa', $1, $2, $3, $4, now(), now())`, e.userID, a.ID, v.ID, appdeploy.ClusterDepActive)
	e.exec(`UPDATE app_deployments SET cluster_deployment_id = 'acd-000000000001', cluster_role = $2 WHERE id = $1`, d.ID, appdeploy.RoleWorker)
	got := e.dep(d.ID)
	if got.ClusterDeploymentID == nil || *got.ClusterDeploymentID != "acd-000000000001" || got.ClusterRole != "worker" {
		t.Fatalf("cluster member deployment: %v %q", got.ClusterDeploymentID, got.ClusterRole)
	}
}
