package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// invariant 是规格 §14 的一条不变量：若干条 SQL，每条返回违背明细（text，最多 5 行），无行即成立。
type invariant struct {
	n    int
	sqls []string
}

// 自然日一律按 Asia/Shanghai：(created_at AT TIME ZONE 'Asia/Shanghai')::date，与结算写 llm_usage_daily 的口径一致。
var invariants = []invariant{
	// 1. 每个用户：llm_accounts.unsettled_micro == Σ 未结算 llm_usage.cost_micro。
	{1, []string{`
		SELECT format('user %s: unsettled_micro=%s, Σ unsettled usage cost_micro=%s',
			coalesce(a.user_id, u.user_id), coalesce(a.unsettled_micro, 0), coalesce(u.s, 0))
		FROM llm_accounts a
		FULL JOIN (SELECT user_id, sum(cost_micro) AS s FROM llm_usage WHERE settlement_id IS NULL GROUP BY user_id) u
			ON u.user_id = a.user_id
		WHERE coalesce(a.unsettled_micro, 0) <> coalesce(u.s, 0)
		LIMIT 5`}},
	// 2. 每个用户：Σ llm_settlements.usage_amount × 10^6 + carry_micro == Σ 已结算 llm_usage.cost_micro。
	//    已结算明细超过 90 天会被清理，本条只在清理开始前（集成测试、真机验收）严格成立。
	{2, []string{`
		WITH s AS (SELECT user_id, sum(usage_amount) AS ua FROM llm_settlements GROUP BY user_id),
		     u AS (SELECT user_id, sum(cost_micro) AS c FROM llm_usage WHERE settlement_id IS NOT NULL GROUP BY user_id),
		     ids AS (SELECT user_id FROM llm_accounts UNION SELECT user_id FROM s UNION SELECT user_id FROM u)
		SELECT format('user %s: Σ usage_amount×10^6 + carry_micro=%s, Σ settled usage cost_micro=%s',
			ids.user_id, coalesce(s.ua, 0) * 1000000 + coalesce(a.carry_micro, 0), coalesce(u.c, 0))
		FROM ids
		LEFT JOIN llm_accounts a ON a.user_id = ids.user_id
		LEFT JOIN s ON s.user_id = ids.user_id
		LEFT JOIN u ON u.user_id = ids.user_id
		WHERE coalesce(s.ua, 0) * 1000000 + coalesce(a.carry_micro, 0) <> coalesce(u.c, 0)
		LIMIT 5`}},
	// 3. 每个用户：Σ |wallet_ledger(llm_pay).amount| == Σ llm_settlements.charged；debt == Σ usage_amount − Σ charged。
	{3, []string{`
		WITH l AS (SELECT user_id, sum(abs(amount)) AS paid FROM wallet_ledger WHERE kind = 'llm_pay' GROUP BY user_id),
		     s AS (SELECT user_id, sum(charged) AS ch, sum(usage_amount) AS ua FROM llm_settlements GROUP BY user_id),
		     ids AS (SELECT user_id FROM llm_accounts UNION SELECT user_id FROM l UNION SELECT user_id FROM s)
		SELECT format('user %s: Σ|llm_pay|=%s, Σ charged=%s, debt=%s, Σ usage_amount − Σ charged=%s',
			ids.user_id, coalesce(l.paid, 0), coalesce(s.ch, 0), coalesce(a.debt, 0), coalesce(s.ua, 0) - coalesce(s.ch, 0))
		FROM ids
		LEFT JOIN llm_accounts a ON a.user_id = ids.user_id
		LEFT JOIN l ON l.user_id = ids.user_id
		LEFT JOIN s ON s.user_id = ids.user_id
		WHERE coalesce(l.paid, 0) <> coalesce(s.ch, 0)
		   OR coalesce(a.debt, 0) <> coalesce(s.ua, 0) - coalesce(s.ch, 0)
		LIMIT 5`}},
	// 4. 每条结算：charged ≤ usage_amount + debt_amount；wallet_ledger_id IS NULL ⇔ charged = 0；
	//    有流水时流水即本结算的 llm_pay（ref_type='llm_settlement'、ref_id=结算 ID、同一用户、amount = −charged）。
	{4, []string{`
		SELECT format('settlement %s: usage_amount=%s, debt_amount=%s, charged=%s, wallet_ledger_id=%s, ledger=(%s, %s, %s, %s)',
			s.id, s.usage_amount, s.debt_amount, s.charged, coalesce(s.wallet_ledger_id::text, 'NULL'),
			coalesce(l.kind, '-'), coalesce(l.ref_type || ':' || l.ref_id, '-'), coalesce(l.user_id::text, '-'), coalesce(l.amount::text, '-'))
		FROM llm_settlements s
		LEFT JOIN wallet_ledger l ON l.id = s.wallet_ledger_id
		WHERE s.charged > s.usage_amount + s.debt_amount
		   OR (s.wallet_ledger_id IS NULL) <> (s.charged = 0)
		   OR (s.wallet_ledger_id IS NOT NULL AND (l.id IS NULL OR l.kind <> 'llm_pay' OR l.ref_type <> 'llm_settlement'
		       OR l.ref_id <> s.id::text OR l.user_id <> s.user_id OR l.amount <> -s.charged))
		ORDER BY s.id
		LIMIT 5`}},
	// 5. llm_usage_daily 各列 == 已结算 llm_usage 按 (day, user, model) 的聚合。
	//    只核对仍有已结算明细的最早一天及之后（更早的明细已按 90 天清理）。
	//    注意：清理按批删除，最早剩余的那一天可能只删了一部分——与第 2 条一样，本条只在清理开始前严格成立。
	{5, []string{`
		WITH agg AS (
			SELECT (created_at AT TIME ZONE 'Asia/Shanghai')::date AS day, user_id, model_id,
				count(*)::bigint AS requests, sum(prompt_tokens)::bigint AS p, sum(completion_tokens)::bigint AS c, sum(cost_micro)::bigint AS m
			FROM llm_usage WHERE settlement_id IS NOT NULL
			GROUP BY 1, 2, 3),
		     d AS (
			SELECT day, user_id, model_id, requests, prompt_tokens AS p, completion_tokens AS c, cost_micro AS m
			FROM llm_usage_daily WHERE day >= (SELECT min(day) FROM agg))
		SELECT format('day %s user %s model %s: daily=(%s, %s, %s, %s), settled usage=(%s, %s, %s, %s)',
			coalesce(d.day, agg.day), coalesce(d.user_id, agg.user_id), coalesce(d.model_id, agg.model_id),
			coalesce(d.requests, 0), coalesce(d.p, 0), coalesce(d.c, 0), coalesce(d.m, 0),
			coalesce(agg.requests, 0), coalesce(agg.p, 0), coalesce(agg.c, 0), coalesce(agg.m, 0))
		FROM d
		FULL JOIN agg ON agg.day = d.day AND agg.user_id = d.user_id AND agg.model_id = d.model_id
		WHERE (coalesce(d.requests, 0), coalesce(d.p, 0), coalesce(d.c, 0), coalesce(d.m, 0))
		      <> (coalesce(agg.requests, 0), coalesce(agg.p, 0), coalesce(agg.c, 0), coalesce(agg.m, 0))
		LIMIT 5`}},
	// 6. 每台机器最多一个非终态节点；每个非终态节点的实例存在、system=true、在同一台机器上且非终态
	//    （removing 除外：ReleaseSystem 已调用，实例可能先于节点进入 released）；ready 节点的部署 running
	//    （deployment_id 为空的 ready 节点只来自 e2e 钩子直接造的节点，不核对）。
	{6, []string{`
		SELECT format('machine %s: %s live nodes', machine_id, count(*))
		FROM llm_nodes WHERE state NOT IN ('removed', 'failed')
		GROUP BY machine_id HAVING count(*) > 1
		LIMIT 5`, `
		SELECT format('node %s (%s, machine %s): instance=%s state=%s system=%s machine=%s',
			n.id, n.state, n.machine_id, coalesce(n.instance_id, 'NULL'), coalesce(i.state, '-'),
			coalesce(i.system::text, '-'), coalesce(i.machine_id::text, '-'))
		FROM llm_nodes n
		LEFT JOIN instances i ON i.id = n.instance_id
		WHERE n.state NOT IN ('removed', 'failed')
		  AND (i.id IS NULL OR NOT i.system OR i.machine_id <> n.machine_id
		       OR (n.state <> 'removing' AND i.state IN ('released', 'failed', 'cancelled')))
		ORDER BY n.id
		LIMIT 5`, `
		SELECT format('node %s ready: deployment %s state=%s', n.id, n.deployment_id, coalesce(d.state, 'missing'))
		FROM llm_nodes n
		LEFT JOIN app_deployments d ON d.id = n.deployment_id
		WHERE n.state = 'ready' AND n.deployment_id IS NOT NULL AND d.state IS DISTINCT FROM 'running'
		ORDER BY n.id
		LIMIT 5`}},
	// 7. 有非终态节点的（池内）机器上没有非系统实例的 reserved / active lease。
	{7, []string{`
		SELECT format('machine %s: live node %s and non-system lease %s (instance %s, %s)',
			n.machine_id, n.id, l.id, l.instance_id, l.status)
		FROM llm_nodes n
		JOIN machine_leases l ON l.machine_id = n.machine_id AND l.status IN ('reserved', 'active')
		JOIN instances i ON i.id = l.instance_id
		WHERE n.state NOT IN ('removed', 'failed') AND NOT i.system
		ORDER BY l.id
		LIMIT 5`}},
	// 8.（计划 13）每个非终态组节点（size > 1）：live 成员恰 size 行、rank 为 0..size-1（主键保证 rank 不重）；
	//    成员实例存在、system、cluster_id = 节点.cluster_id、在成员行的机器上；node.instance_id / machine_id = rank0 成员。
	{8, []string{`
		SELECT format('node %s (%s): size=%s, live members=%s, ranks=[%s]', n.id, n.state, n.size, count(mm.rank),
			coalesce(string_agg(mm.rank::text, ',' ORDER BY mm.rank), ''))
		FROM llm_nodes n
		LEFT JOIN llm_node_members mm ON mm.node_id = n.id AND mm.live
		WHERE n.size > 1 AND n.state NOT IN ('removed', 'failed')
		GROUP BY n.id, n.state, n.size
		HAVING count(mm.rank) <> n.size OR coalesce(min(mm.rank), 0) <> 0 OR coalesce(max(mm.rank), n.size - 1) <> n.size - 1
		ORDER BY n.id
		LIMIT 5`, `
		SELECT format('node %s rank %s: instance %s system=%s cluster=%s (node cluster %s), machine %s (member machine %s), node head=%s/%s',
			n.id, mm.rank, mm.instance_id, coalesce(i.system::text, '-'), coalesce(i.cluster_id, 'NULL'), coalesce(n.cluster_id, 'NULL'),
			coalesce(i.machine_id::text, '-'), mm.machine_id, coalesce(n.instance_id, 'NULL'), n.machine_id)
		FROM llm_nodes n
		JOIN llm_node_members mm ON mm.node_id = n.id AND mm.live
		LEFT JOIN instances i ON i.id = mm.instance_id
		WHERE n.size > 1 AND n.state NOT IN ('removed', 'failed')
		  AND (i.id IS NULL OR NOT i.system OR i.cluster_id IS DISTINCT FROM n.cluster_id OR i.machine_id <> mm.machine_id
		       OR (mm.rank = 0 AND (n.instance_id IS DISTINCT FROM mm.instance_id OR n.machine_id <> mm.machine_id)))
		ORDER BY n.id, mm.rank
		LIMIT 5`}},
	// 9. 终态节点没有 live 成员。
	{9, []string{`
		SELECT format('node %s (%s): live member rank %s (machine %s)', n.id, n.state, mm.rank, mm.machine_id)
		FROM llm_nodes n
		JOIN llm_node_members mm ON mm.node_id = n.id AND mm.live
		WHERE n.state IN ('removed', 'failed')
		ORDER BY n.id, mm.rank
		LIMIT 5`}},
	// 10. ready 的组节点：集群部署 active，且每个 live 成员在该集群部署里的成员部署 running。
	{10, []string{`
		SELECT format('node %s ready: app cluster %s state=%s', n.id, coalesce(n.app_cluster_id, 'NULL'), coalesce(c.state, 'missing'))
		FROM llm_nodes n
		LEFT JOIN app_cluster_deployments c ON c.id = n.app_cluster_id
		WHERE n.state = 'ready' AND n.size > 1 AND c.state IS DISTINCT FROM 'active'
		ORDER BY n.id
		LIMIT 5`, `
		SELECT format('node %s ready: rank %s (instance %s) deployment %s', n.id, mm.rank, mm.instance_id,
			coalesce(string_agg(d.id || '=' || d.state, ','), 'missing'))
		FROM llm_nodes n
		JOIN llm_node_members mm ON mm.node_id = n.id AND mm.live
		LEFT JOIN app_deployments d ON d.instance_id = mm.instance_id AND d.cluster_deployment_id = n.app_cluster_id
			AND d.state NOT IN ('removed', 'lost')
		WHERE n.state = 'ready' AND n.size > 1 AND n.app_cluster_id IS NOT NULL
		GROUP BY n.id, mm.rank, mm.instance_id
		HAVING count(d.id) FILTER (WHERE d.state = 'running') <> 1 OR count(d.id) <> 1
		ORDER BY n.id, mm.rank
		LIMIT 5`}},
}

// CheckInvariants 核对规格 §14 的七条不变量与计划 13 §7.7 的三条（⑧～⑩）（集成测试与真机验收后执行）。
// 全部查询在同一个只读 REPEATABLE READ 快照里执行；违背的条目以 "invariant N: 明细" 返回（多条用 errors.Join 合并）。
func CheckInvariants(ctx context.Context, pool *pgxpool.Pool) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var errs []error
	for _, inv := range invariants {
		var details []string
		for _, q := range inv.sqls {
			rows, err := tx.Query(ctx, q)
			if err != nil {
				return fmt.Errorf("invariant %d: %w", inv.n, err)
			}
			d, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				return fmt.Errorf("invariant %d: %w", inv.n, err)
			}
			details = append(details, d...)
		}
		if len(details) > 0 {
			errs = append(errs, fmt.Errorf("invariant %d: %s", inv.n, strings.Join(details, "; ")))
		}
	}
	return errors.Join(errs...)
}
