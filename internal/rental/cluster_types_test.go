package rental_test

import (
	"slices"
	"testing"
	"time"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/rental"
)

// 计划 13 T1：instances.cluster_id / cluster_rank 经 instanceCols / scanInstance 读出；集群常量。
func TestInstanceClusterColumns(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	if inst.ClusterID != nil || inst.ClusterRank != nil {
		t.Fatalf("non-cluster instance: %v %v", inst.ClusterID, inst.ClusterRank)
	}
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO cluster_fabrics (name, kind, isolation, created_at, updated_at)
		VALUES ('以太网', 'lan', 'routed', now(), now())`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO instance_clusters (id, user_id, size, fabric_id, mode, rdma, subnet_index, state, created_at, updated_at)
		VALUES ('clu-aaaaaaaaaa', $1, 2, (SELECT id FROM cluster_fabrics), 'routed', false, 0, 'active', now(), now())`, e.userID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(e.ctx, `UPDATE instances SET cluster_id = 'clu-aaaaaaaaaa', cluster_rank = 1 WHERE id = $1`, inst.ID); err != nil {
		t.Fatal(err)
	}
	got := e.get(inst.ID)
	if got.ClusterID == nil || *got.ClusterID != "clu-aaaaaaaaaa" || got.ClusterRank == nil || *got.ClusterRank != 1 {
		t.Fatalf("cluster member: %v %v", got.ClusterID, got.ClusterRank)
	}

	if !slices.Equal(rental.ClusterSizes, []int{2, 4}) || rental.ClusterAlignSlack != 60*time.Second ||
		rental.ConfigureClusterCommandTimeout != 2*time.Minute {
		t.Fatal("cluster constants")
	}
	for _, s := range []string{rental.ClusterActive, rental.ClusterReleased, rental.ClusterFailed, rental.ClusterForming, rental.ClusterReady, rental.ClusterDegraded} {
		if s == "" {
			t.Fatal("empty cluster state")
		}
	}
}
