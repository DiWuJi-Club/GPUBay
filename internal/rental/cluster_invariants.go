package rental

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CheckClusterInvariants 核对实例集群的不变量（规格 §5.9；集成测试与真机验收后执行），违背返回 "cluster invariant N: …"：
//
//	① 每个 active 集群：在位成员 rank 互不相同且 < size、成员 system = 集群 system、成员都在集群 fabric 的机器上；
//	② active 集群的 subnet_index 唯一，vlan fabric 上 (fabric, vlan) 唯一；
//	③ 非系统集群：batch_id 下 kind='new' 的订单恰有 size 张，每张订单的实例要么是本集群成员，要么经 instance_transfers（可多跳）连到本集群成员；
//	④ 系统集群的成员（cluster_id 指向它的全部实例）没有任何订单；
//	⑤ failed 的非系统集群：全部成员（含换机前的旧实例）的订单都已全额退款。
func CheckClusterInvariants(ctx context.Context, pool *pgxpool.Pool) error {
	type check struct {
		n   int
		sql string
	}
	checks := []check{
		{1, `SELECT i.cluster_id || ' rank ' || i.cluster_rank FROM instances i JOIN instance_clusters c ON c.id = i.cluster_id
			WHERE c.state = 'active' AND i.state IN ('creating', 'running', 'stopped')
			GROUP BY i.cluster_id, i.cluster_rank, c.size HAVING count(*) > 1 OR i.cluster_rank >= c.size`},
		{1, `SELECT i.id || ' in ' || c.id FROM instances i JOIN instance_clusters c ON c.id = i.cluster_id JOIN machines m ON m.id = i.machine_id
			WHERE c.state = 'active' AND i.state IN ('creating', 'running', 'stopped')
			  AND (i.system <> c.system OR m.fabric_id IS DISTINCT FROM c.fabric_id)`},
		{2, `SELECT 'subnet ' || subnet_index FROM instance_clusters WHERE state = 'active' GROUP BY subnet_index HAVING count(*) > 1`},
		{2, `SELECT 'fabric ' || fabric_id || ' vlan ' || vlan FROM instance_clusters WHERE state = 'active' AND vlan IS NOT NULL
			GROUP BY fabric_id, vlan HAVING count(*) > 1`},
		{3, `SELECT c.id || ' has ' || (SELECT count(*) FROM orders o WHERE o.batch_id = c.batch_id AND o.kind = 'new') || ' new orders, size ' || c.size
			FROM instance_clusters c
			WHERE NOT c.system AND (c.batch_id IS NULL OR (SELECT count(*) FROM orders o WHERE o.batch_id = c.batch_id AND o.kind = 'new') <> c.size)`},
		{3, `WITH RECURSIVE fwd (cluster_id, order_id, id) AS (
				SELECT c.id, o.id, o.instance_id FROM instance_clusters c JOIN orders o ON o.batch_id = c.batch_id AND o.kind = 'new' WHERE NOT c.system
				UNION
				SELECT f.cluster_id, f.order_id, t.to_instance_id FROM fwd f JOIN instance_transfers t ON t.from_instance_id = f.id
			)
			SELECT f.cluster_id || ' order ' || f.order_id || ' not linked to a member' FROM fwd f LEFT JOIN instances i ON i.id = f.id
			GROUP BY f.cluster_id, f.order_id HAVING NOT bool_or(i.cluster_id IS NOT DISTINCT FROM f.cluster_id)`},
		{4, `SELECT c.id || ' order ' || o.id FROM instance_clusters c JOIN instances i ON i.cluster_id = c.id JOIN orders o ON o.instance_id = i.id
			WHERE c.system`},
		{5, `SELECT c.id || ' order ' || o.id || ' refunded ' || o.refunded_amount || '/' || o.amount
			FROM instance_clusters c JOIN instances i ON i.cluster_id = c.id JOIN orders o ON o.instance_id = i.id
			WHERE c.state = 'failed' AND NOT c.system AND o.refunded_amount < o.amount`},
	}
	for _, c := range checks {
		rows, err := pool.Query(ctx, c.sql)
		if err != nil {
			return fmt.Errorf("cluster invariant %d: %w", c.n, err)
		}
		bad, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return fmt.Errorf("cluster invariant %d: %w", c.n, err)
		}
		if len(bad) > 0 {
			return fmt.Errorf("cluster invariant %d: %v", c.n, bad)
		}
	}
	return nil
}
