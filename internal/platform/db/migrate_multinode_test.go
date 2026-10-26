package db_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pressly/goose/v3"

	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/migrations"
)

// 迁移 00026（计划 13：多机模型）：带存量数据 Up → 约束 → Down（三个拒绝条件）→ Up 往返。
func TestMigration00026RoundTrip(t *testing.T) {
	ctx := context.Background()
	sqlDB, err := sql.Open("pgx", dbtest.EmptyURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	p, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := sqlDB.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := sqlDB.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	isCheck := func(err error) bool {
		var pgErr *pgconn.PgError
		return errors.As(err, &pgErr) && pgErr.Code == "23514"
	}
	mustCheck := func(name, q string, args ...any) {
		t.Helper()
		if _, err := sqlDB.ExecContext(ctx, q, args...); !isCheck(err) {
			t.Fatalf("%s: err = %v, want check violation", name, err)
		}
	}
	mustUnique := func(name, q string, args ...any) {
		t.Helper()
		if _, err := sqlDB.ExecContext(ctx, q, args...); !db.IsUniqueViolation(err) {
			t.Fatalf("%s: err = %v, want unique violation", name, err)
		}
	}
	upByOne := func() {
		t.Helper()
		r, err := p.UpByOne(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if r.Source.Version != 26 {
			t.Fatalf("UpByOne applied version %d, want 26", r.Source.Version)
		}
	}

	if _, err := p.UpTo(ctx, 25); err != nil {
		t.Fatal(err)
	}
	// 存量数据
	var uid int64
	if err := sqlDB.QueryRowContext(ctx, `INSERT INTO users (phone, kind, status, created_at, updated_at)
		VALUES ('13800000001', 'personal', 'active', now(), now()) RETURNING id`).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO machines (serial, rack, slot, lan_ip, product_id, state, created_at, updated_at) VALUES
		('SN1', 'A', '1', '10.0.0.1', 1, 'idle', now(), now()),
		('SN2', 'A', '2', '10.0.0.2', 1, 'idle', now(), now()),
		('SN3', 'A', '3', '10.0.0.3', 1, 'idle', now(), now())`)
	exec(`INSERT INTO instances (id, user_id, machine_id, product_id, image_id, state, ssh_password_enc, created_at, updated_at) VALUES
		('ins-aaaaaaaa', $1, 1, 1, 1, 'running', 'x', now(), now()),
		('ins-bbbbbbbb', $1, 2, 1, 1, 'running', 'x', now(), now())`, uid)
	exec(`INSERT INTO apps (slug, name, category, status, created_at, updated_at) VALUES ('vllm-cluster', 'vLLM 集群', 'llm', 'published', now(), now())`)
	exec(`INSERT INTO app_versions (app_id, version, status, image_ids, spec, script, spec_hash, created_at, updated_at)
		VALUES (1, '1.0.0', 'published', '{1}', '{"ports":[{"port":8000}]}', 'true', 'h', now(), now())`)
	exec(`INSERT INTO app_deployments (id, instance_id, user_id, app_id, app_version_id, state, port, spec_hash, next_poll_at, created_at, updated_at)
		VALUES ('dep-000000000001', 'ins-aaaaaaaa', $1, 1, 1, 'running', 8000, 'h', now(), now(), now())`, uid)
	exec(`INSERT INTO llm_models (slug, name, kind, source_id, image_id, context_len, input_price, output_price, created_at, updated_at)
		VALUES ('qwen3-4b', 'Qwen3 4B', 'chat', 'Qwen/Qwen3-4B', 1, 32768, 100, 400, now(), now())`)
	exec(`INSERT INTO llm_nodes (id, model_id, machine_id, state, api_key_enc, created_at, updated_at)
		VALUES ('node-000000000001', 1, 3, 'ready', '\x00', now(), now())`)

	upByOne()

	// 存量默认值
	if n := count(`SELECT count(*) FROM machines WHERE fabric_id IS NULL AND cluster_iface = '' AND agent_features = 0
		AND net_info IS NULL AND net_info_at IS NULL AND NOT cluster_ready AND NOT cluster_rdma`); n != 3 {
		t.Fatalf("machine defaults = %d", n)
	}
	if n := count(`SELECT count(*) FROM llm_models WHERE nodes_per_replica = 1 AND tensor_parallel = 1 AND pipeline_parallel = 1`); n != 1 {
		t.Fatalf("model defaults = %d", n)
	}
	if n := count(`SELECT count(*) FROM llm_nodes WHERE size = 1 AND cluster_id IS NULL AND app_cluster_id IS NULL`); n != 1 {
		t.Fatalf("node defaults = %d", n)
	}
	if n := count(`SELECT count(*) FROM instances WHERE cluster_id IS NULL AND cluster_rank IS NULL`); n != 2 {
		t.Fatalf("instance defaults = %d", n)
	}
	if n := count(`SELECT count(*) FROM app_deployments WHERE cluster_deployment_id IS NULL AND cluster_role IS NULL`); n != 1 {
		t.Fatalf("deployment defaults = %d", n)
	}

	// cluster_fabrics：kind / isolation / rdma / vlan 组合
	const insFabric = `INSERT INTO cluster_fabrics (name, kind, isolation, rdma, mtu, vlan_min, vlan_max, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, now(), now())`
	exec(insFabric, "以太网", "lan", "routed", false, 0, nil, nil)
	exec(insFabric, "直连", "cx7", "direct", true, 9000, nil, nil)
	exec(insFabric, "交换机", "cx7", "vlan", true, 0, 100, 199)
	mustCheck("lan+direct", insFabric, "f1", "lan", "direct", false, 0, nil, nil)
	mustCheck("cx7+routed", insFabric, "f2", "cx7", "routed", false, 0, nil, nil)
	mustCheck("lan+rdma", insFabric, "f3", "lan", "routed", true, 0, nil, nil)
	mustCheck("vlan without range", insFabric, "f4", "cx7", "vlan", false, 0, nil, nil)
	mustCheck("vlan max < min", insFabric, "f5", "cx7", "vlan", false, 0, 200, 100)
	mustCheck("vlan out of range", insFabric, "f6", "cx7", "vlan", false, 0, 1, 100)
	mustCheck("direct with vlan", insFabric, "f7", "cx7", "direct", false, 0, 100, 199)
	mustCheck("mtu", insFabric, "f8", "lan", "routed", false, 1000, nil, nil)
	mustUnique("fabric name", insFabric, "以太网", "lan", "routed", false, 0, nil, nil)
	exec(`UPDATE machines SET fabric_id = 1, cluster_ready = true WHERE id IN (1, 2)`)

	// instance_clusters：同 subnet_index 只能一个 active；同 fabric 同 vlan 只能一个 active
	const insCluster = `INSERT INTO instance_clusters (id, user_id, size, fabric_id, mode, vlan, rdma, subnet_index, state, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, false, $7, $8, now(), now())`
	exec(insCluster, "clu-aaaaaaaaaa", uid, 2, 1, "routed", nil, 0, "active")
	mustUnique("subnet live", insCluster, "clu-bbbbbbbbbb", uid, 2, 1, "routed", nil, 0, "active")
	exec(insCluster, "clu-bbbbbbbbbb", uid, 2, 1, "routed", nil, 0, "released")
	exec(`UPDATE instance_clusters SET state = 'released' WHERE id = 'clu-aaaaaaaaaa'`)
	exec(insCluster, "clu-cccccccccc", uid, 2, 1, "routed", nil, 0, "active")
	exec(insCluster, "clu-dddddddddd", uid, 4, 3, "macvlan", 100, 1, "active")
	mustUnique("vlan live", insCluster, "clu-eeeeeeeeee", uid, 4, 3, "macvlan", 100, 2, "active")
	exec(insCluster, "clu-eeeeeeeeee", uid, 4, 3, "macvlan", 101, 2, "active")
	mustCheck("size 1", insCluster, "clu-ffffffffff", uid, 1, 1, "routed", nil, 3, "active")
	mustCheck("size 9", insCluster, "clu-ffffffffff", uid, 9, 1, "routed", nil, 3, "active")
	mustCheck("mode", insCluster, "clu-ffffffffff", uid, 2, 1, "bridge", nil, 3, "active")
	mustCheck("subnet 256", insCluster, "clu-ffffffffff", uid, 2, 1, "routed", nil, 256, "active")
	mustCheck("state", insCluster, "clu-ffffffffff", uid, 2, 1, "routed", nil, 3, "forming")

	// instances_cluster_pair
	mustCheck("cluster_id without rank", `UPDATE instances SET cluster_id = 'clu-cccccccccc' WHERE id = 'ins-aaaaaaaa'`)
	mustCheck("rank without cluster_id", `UPDATE instances SET cluster_rank = 0 WHERE id = 'ins-aaaaaaaa'`)
	mustCheck("rank 8", `UPDATE instances SET cluster_id = 'clu-cccccccccc', cluster_rank = 8 WHERE id = 'ins-aaaaaaaa'`)
	exec(`UPDATE instances SET cluster_id = 'clu-cccccccccc', cluster_rank = 0 WHERE id = 'ins-aaaaaaaa'`)
	exec(`UPDATE instances SET cluster_id = 'clu-cccccccccc', cluster_rank = 1 WHERE id = 'ins-bbbbbbbb'`)

	// app_cluster_deployments：一个集群只有一个 active；app_deployments_cluster_pair
	const insACD = `INSERT INTO app_cluster_deployments (id, cluster_id, user_id, app_id, app_version_id, state, created_at, updated_at)
		VALUES ($1, 'clu-cccccccccc', $2, 1, 1, $3, now(), now())`
	exec(insACD, "acd-000000000001", uid, "active")
	mustUnique("acd live", insACD, "acd-000000000002", uid, "active")
	exec(insACD, "acd-000000000002", uid, "removed")
	mustCheck("acd state", insACD, "acd-000000000003", uid, "running")
	mustCheck("dep cluster pair", `UPDATE app_deployments SET cluster_deployment_id = 'acd-000000000001' WHERE id = 'dep-000000000001'`)
	mustCheck("dep cluster role", `UPDATE app_deployments SET cluster_deployment_id = 'acd-000000000001', cluster_role = 'master' WHERE id = 'dep-000000000001'`)
	exec(`UPDATE app_deployments SET cluster_deployment_id = 'acd-000000000001', cluster_role = 'head' WHERE id = 'dep-000000000001'`)

	// llm_models_parallel
	exec(`UPDATE llm_models SET nodes_per_replica = 4, tensor_parallel = 2, pipeline_parallel = 2`)
	mustCheck("2 x 1 != 4", `UPDATE llm_models SET nodes_per_replica = 4, tensor_parallel = 2, pipeline_parallel = 1`)
	mustCheck("nodes 3", `UPDATE llm_models SET nodes_per_replica = 3, tensor_parallel = 3, pipeline_parallel = 1`)
	exec(`UPDATE llm_models SET nodes_per_replica = 2, tensor_parallel = 2, pipeline_parallel = 1`)

	// llm_nodes_group：size = 1 ⇔ cluster_id 为空
	const insNode = `INSERT INTO llm_nodes (id, model_id, machine_id, instance_id, state, api_key_enc, size, cluster_id, created_at, updated_at)
		VALUES ($1, 1, $2, $3, $4, '\x00', $5, $6, now(), now())`
	mustCheck("size 2 without cluster", insNode, "node-000000000002", 1, "ins-aaaaaaaa", "provisioning", 2, nil)
	mustCheck("size 1 with cluster", insNode, "node-000000000002", 1, "ins-aaaaaaaa", "provisioning", 1, "clu-cccccccccc")
	exec(insNode, "node-000000000002", 1, "ins-aaaaaaaa", "provisioning", 2, "clu-cccccccccc")
	exec(`UPDATE llm_nodes SET app_cluster_id = 'acd-000000000001' WHERE id = 'node-000000000002'`)

	// llm_node_members_machine_live_uniq
	const insMember = `INSERT INTO llm_node_members (node_id, rank, machine_id, instance_id, live) VALUES ($1, $2, $3, $4, $5)`
	exec(insMember, "node-000000000002", 0, 1, "ins-aaaaaaaa", true)
	exec(insMember, "node-000000000002", 1, 2, "ins-bbbbbbbb", true)
	mustUnique("member pk", insMember, "node-000000000002", 1, 3, "ins-bbbbbbbb", true)
	exec(`INSERT INTO llm_nodes (id, model_id, machine_id, state, api_key_enc, size, cluster_id, created_at, updated_at)
		VALUES ('node-000000000003', 1, 2, 'failed', '\x00', 2, 'clu-dddddddddd', now(), now())`)
	mustUnique("member machine live", insMember, "node-000000000003", 0, 2, "ins-bbbbbbbb", true)
	exec(insMember, "node-000000000003", 0, 2, "ins-bbbbbbbb", false)
	mustCheck("member rank 8", insMember, "node-000000000003", 8, 3, "ins-bbbbbbbb", false)

	// Down：有 active 集群 → 拒绝
	if _, err := p.Down(ctx); err == nil || !strings.Contains(err.Error(), "运行中的集群") {
		t.Fatalf("down with active cluster: err = %v", err)
	}
	exec(`UPDATE instance_clusters SET state = 'released', ended_at = now() WHERE state = 'active'`)
	// Down：有非终态的多机节点组 → 拒绝
	if _, err := p.Down(ctx); err == nil || !strings.Contains(err.Error(), "多机节点组") {
		t.Fatalf("down with group node: err = %v", err)
	}
	exec(`UPDATE llm_nodes SET state = 'removed' WHERE id = 'node-000000000002'`)
	// Down：有 active 集群应用 → 拒绝
	if _, err := p.Down(ctx); err == nil || !strings.Contains(err.Error(), "集群应用") {
		t.Fatalf("down with active cluster app: err = %v", err)
	}
	exec(`UPDATE app_cluster_deployments SET state = 'removed'`)
	if _, err := p.Down(ctx); err != nil {
		t.Fatal(err)
	}
	if n := count(`SELECT count(*) FROM information_schema.tables WHERE table_name IN
		('cluster_fabrics', 'instance_clusters', 'app_cluster_deployments', 'llm_node_members')`); n != 0 {
		t.Fatalf("tables after down = %d", n)
	}
	if n := count(`SELECT count(*) FROM information_schema.columns WHERE
		(table_name = 'machines' AND column_name IN ('fabric_id', 'cluster_iface', 'agent_features', 'net_info', 'net_info_at', 'cluster_ready', 'cluster_rdma'))
		OR (table_name = 'instances' AND column_name IN ('cluster_id', 'cluster_rank'))
		OR (table_name = 'app_deployments' AND column_name IN ('cluster_deployment_id', 'cluster_role'))
		OR (table_name = 'llm_models' AND column_name IN ('nodes_per_replica', 'tensor_parallel', 'pipeline_parallel'))
		OR (table_name = 'llm_nodes' AND column_name IN ('size', 'cluster_id', 'app_cluster_id'))`); n != 0 {
		t.Fatalf("columns after down = %d", n)
	}
	// 存量行保留
	if n := count(`SELECT count(*) FROM instances`) + count(`SELECT count(*) FROM app_deployments`) + count(`SELECT count(*) FROM llm_nodes`); n != 6 {
		t.Fatalf("rows after down = %d", n)
	}

	// 再 Up
	upByOne()
	exec(insFabric, "以太网", "lan", "routed", false, 0, nil, nil)
	if pending, err := p.HasPending(ctx); err != nil || pending {
		t.Fatalf("pending = %v, %v", pending, err)
	}
}
