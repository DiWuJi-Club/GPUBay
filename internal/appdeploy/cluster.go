// 集群部署（计划 13 规格 §6）：一次在集群全部在位成员上各建一条部署（rank0 = head、其余 = worker，变量相同），
// 之后整组重新部署 / 停止 / 启动 / 卸载。成员部署由 Runner 按单条部署照常推进；聚合状态见 cluster_status.go。
//
// 加锁顺序（规格 §14「多成员路径」）：instance_clusters 行 FOR UPDATE → 成员实例（ID 升序）→ app_deployments → app_cluster_deployments 行。
// 不调用 rental 的集群函数：集群行与成员直接按 SQL 读（成员 = cluster_id 匹配且 state IN rental.ClusterMemberStates）。

package appdeploy

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/randx"
	"spark-cloud/internal/rental"
)

// clusterOnly：版本只能部署在集群上（spec.cluster.mode = required）。
func clusterOnly(sp Spec) bool { return sp.Cluster != nil && sp.Cluster.Mode == ClusterModeRequired }

// roleFor rank0 = head，其余 = worker。
func roleFor(rank int) string {
	if rank == 0 {
		return RoleHead
	}
	return RoleWorker
}

// clusterRow 是集群操作需要的 instance_clusters 字段。
type clusterRow struct {
	ID     string
	UserID int64
	System bool
	Size   int
	State  string
}

// lockCluster 锁集群行；不存在 → cluster_not_found。
func lockCluster(ctx context.Context, tx pgx.Tx, id string) (clusterRow, error) {
	var c clusterRow
	err := tx.QueryRow(ctx, `SELECT id, user_id, system, size, state FROM instance_clusters WHERE id = $1 FOR UPDATE`, id).
		Scan(&c.ID, &c.UserID, &c.System, &c.Size, &c.State)
	if db.IsNoRows(err) {
		return clusterRow{}, apperr.ErrClusterNotFound
	}
	return c, err
}

// clusterVisible：用户须是集群主人；系统集群（Token 商城节点组）只允许系统账号。
func (s *Service) clusterVisible(c clusterRow, userID int64) bool {
	if c.UserID != userID {
		return false
	}
	return !c.System || (s.d.SystemUserID != 0 && userID == s.d.SystemUserID)
}

// member 是集群的一个在位成员实例（已加锁）。
type member struct {
	inst rental.Instance // 只填 ID / UserID / ImageID / State（部署路径只用到这些）
	rank int
}

// lockInstances 按 ID 升序锁实例行（去重），返回 id → 实例（只填 ID / UserID / ImageID / State / ClusterID / ClusterRank）。
func lockInstances(ctx context.Context, tx pgx.Tx, ids []string) (map[string]rental.Instance, error) {
	ids = slices.Clone(ids)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	out := map[string]rental.Instance{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := tx.Query(ctx, `SELECT id, user_id, image_id, state, cluster_id, cluster_rank FROM instances WHERE id = ANY($1) ORDER BY id FOR UPDATE`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var i rental.Instance
		if err := rows.Scan(&i.ID, &i.UserID, &i.ImageID, &i.State, &i.ClusterID, &i.ClusterRank); err != nil {
			return nil, err
		}
		out[i.ID] = i
	}
	return out, rows.Err()
}

// memberIDs 读集群在位成员的实例 ID（未加锁；调用方随后 lockInstances 并用 currentMembers 复核）。
func memberIDs(ctx context.Context, q db.Querier, clusterID string) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT id FROM instances WHERE cluster_id = $1 AND state = ANY($2) ORDER BY cluster_rank, id`, clusterID, rental.ClusterMemberStates)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// currentMembers 从已加锁的实例里取仍是集群在位成员的（单实例路径可能在加锁前改了状态），按 rank、ID 排序。
func currentMembers(clusterID string, locked map[string]rental.Instance) []member {
	var out []member
	for _, i := range locked {
		if i.ClusterID != nil && *i.ClusterID == clusterID && i.ClusterRank != nil && slices.Contains(rental.ClusterMemberStates, i.State) {
			out = append(out, member{inst: i, rank: *i.ClusterRank})
		}
	}
	slices.SortFunc(out, func(a, b member) int {
		if a.rank != b.rank {
			return a.rank - b.rank
		}
		if a.inst.ID < b.inst.ID {
			return -1
		}
		return 1
	})
	return out
}

// fullMembers：在位成员恰好是 rank 0..size-1 各一个。
func fullMembers(ms []member, size int) bool {
	if len(ms) != size {
		return false
	}
	for i, m := range ms {
		if m.rank != i {
			return false
		}
	}
	return true
}

// checkClusterSize 版本须支持集群（否则 app_cluster_unsupported），集群台数在 [min, max] 内（否则 app_cluster_size）。
func checkClusterSize(ver Version, size int) error {
	cs := ver.Spec.Cluster
	if cs == nil {
		return apperr.ErrAppClusterUnsupported
	}
	if size < cs.MinSize || size > cs.MaxSize {
		return apperr.AppClusterSize(cs.MinSize, cs.MaxSize)
	}
	return nil
}

// checkMemberSlot 成员实例上能否再部署该应用：镜像兼容、未达应用数上限、该应用没有活动部署、端口不冲突（同 Create）。
func (s *Service) checkMemberSlot(ctx context.Context, q db.Querier, inst rental.Instance, appID int64, ver Version) error {
	if err := s.checkImage(ctx, q, ver, inst.ImageID); err != nil {
		return err
	}
	var live int
	var exists bool
	if err := q.QueryRow(ctx, `SELECT count(*), COALESCE(bool_or(app_id = $2), false) FROM app_deployments WHERE instance_id = $1 AND state NOT IN ('removed', 'lost')`,
		inst.ID, appID).Scan(&live, &exists); err != nil {
		return err
	}
	if exists {
		return apperr.ErrAppExists
	}
	if live >= MaxAppsPerInstance {
		return apperr.ErrAppLimit
	}
	return s.checkPorts(ctx, q, inst.ID, ver.Spec.Ports, "")
}

// insertMember 插入一条 queued 的集群成员部署（同 insert，另写 cluster_deployment_id / cluster_role）。
func (s *Service) insertMember(ctx context.Context, q db.Querier, inst rental.Instance, userID, appID int64, ver Version, acdID, role string,
	plain, secret map[string]string, now time.Time) (Deployment, error) {
	if err := cancelStaleCleanup(ctx, q, inst.ID, appID, now); err != nil {
		return Deployment{}, err
	}
	return scanDeployment(q.QueryRow(ctx, `INSERT INTO app_deployments (id, instance_id, user_id, app_id, app_version_id, state, desired, env, secret_env_enc, port, spec_hash,
		next_poll_at, created_at, updated_at, cluster_deployment_id, cluster_role)
		VALUES ($1, $2, $3, $4, $5, 'queued', 'running', $6, $7, $8, $9, $10, $10, $10, $11, $12) RETURNING `+depCols,
		randx.ID("dep-", 12), inst.ID, userID, appID, ver.ID, mustJSON(plain), sealSecrets(s.d.Box, secret), ver.Spec.Ports[0].Port, ver.SpecHash, now, acdID, role))
}

// auditCluster 记集群部署操作的审计（目标为集群；只含集群部署 ID、应用 slug、版本 ID，不含变量值）。
func auditCluster(ctx context.Context, q db.Querier, now time.Time, actor audit.Actor, action, clusterID, acdID string, appID, versionID int64) error {
	var slug string
	if err := q.QueryRow(ctx, `SELECT slug FROM apps WHERE id = $1`, appID).Scan(&slug); err != nil {
		return err
	}
	return audit.Record(ctx, q, now, actor.Entry(action, "cluster", clusterID,
		map[string]any{"cluster_deployment": acdID, "app": slug, "version_id": versionID}))
}

// ---- 创建 ----

// CreateCluster 在集群全部在位成员上各建一条 queued 部署（规格 §6.2）：rank0 为 head、其余为 worker，变量相同。
// 校验：集群存在且归属（系统集群只允许系统账号；否则 cluster_not_found）、集群 active 且在位成员恰为 rank 0..size-1（否则 invalid_state）、
// 应用已上架（内部应用只允许系统账号）、版本支持集群（app_cluster_unsupported）且台数在范围内（app_cluster_size）、
// 集群上没有 active 的集群部署（app_cluster_exists）、每个成员镜像兼容 / 未达上限 / 无该应用 / 端口不冲突、变量合法。
// 事件每个成员部署一条，审计 cluster.app_deploy 一条。并发创建由 app_cluster_deployments_live_uniq 兜底。
func (s *Service) CreateCluster(ctx context.Context, req ClusterCreateRequest) (ClusterDeployment, error) {
	now := s.d.Clock.Now()
	var out ClusterDeployment
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		c, err := lockCluster(ctx, tx, req.ClusterID)
		if err != nil {
			return err
		}
		if !s.clusterVisible(c, req.UserID) {
			return apperr.ErrClusterNotFound
		}
		if c.State != rental.ClusterActive {
			return apperr.ErrInvalidState
		}
		ids, err := memberIDs(ctx, tx, c.ID)
		if err != nil {
			return err
		}
		locked, err := lockInstances(ctx, tx, ids)
		if err != nil {
			return err
		}
		members := currentMembers(c.ID, locked)
		if !fullMembers(members, c.Size) {
			return apperr.ErrInvalidState
		}
		isSystem := s.d.SystemUserID != 0 && req.UserID == s.d.SystemUserID
		app, ver, err := s.resolveVersion(ctx, tx, req.AppID, req.VersionID, isSystem)
		if err != nil {
			return err
		}
		if err := checkClusterSize(ver, c.Size); err != nil {
			return err
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM app_cluster_deployments WHERE cluster_id = $1 AND state = 'active')`, c.ID).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return apperr.ErrAppClusterExists
		}
		for _, m := range members {
			if err := s.checkMemberSlot(ctx, tx, m.inst, app.ID, ver); err != nil {
				return err
			}
		}
		plain, secret, err := ValidateEnv(ver.Spec, req.Env)
		if err != nil {
			return err
		}
		acdID := randx.ID("acd-", 12)
		if _, err := tx.Exec(ctx, `INSERT INTO app_cluster_deployments (id, cluster_id, user_id, app_id, app_version_id, state, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, 'active', $6, $6)`, acdID, c.ID, c.UserID, app.ID, ver.ID, now); err != nil {
			return err
		}
		for _, m := range members {
			role := roleFor(m.rank)
			d, err := s.insertMember(ctx, tx, m.inst, c.UserID, app.ID, ver, acdID, role, plain, secret, now)
			if err != nil {
				return err
			}
			if err := addEvent(ctx, tx, d.ID, "", StateQueued, fmt.Sprintf("集群部署：%s %s（%s，rank %d）", app.Name, ver.Version, role, m.rank), now); err != nil {
				return err
			}
		}
		if err := auditCluster(ctx, tx, now, audit.Actor{UserID: req.UserID, IP: req.IP}, "cluster.app_deploy", c.ID, acdID, app.ID, ver.ID); err != nil {
			return err
		}
		out, err = s.loadClusterDeployment(ctx, tx, acdID)
		return err
	})
	if db.IsUniqueViolation(err) {
		if db.ConstraintName(err) == "app_cluster_deployments_live_uniq" {
			return ClusterDeployment{}, apperr.ErrAppClusterExists
		}
		return ClusterDeployment{}, apperr.ErrAppExists // 并发的单实例部署占了某个成员上的同一应用（app_deployments_live_uniq）
	}
	if err != nil {
		return ClusterDeployment{}, err
	}
	return out, nil
}

// ---- 整组操作 ----

// clusterOpState 是整组操作在事务里拿到的、已按锁序加锁的数据。
type clusterOpState struct {
	cluster clusterRow
	acd     ClusterDeployment          // 不含成员（fillClusterDeployment 之前的行）
	locked  map[string]rental.Instance // 在位成员 ∪ 该集群部署非终态 / lost 成员部署所在的实例
	members []member                   // 在位成员（rank、ID 序）
	deps    []Deployment               // 该集群部署的非终态与 lost 成员部署（已加锁，按 ID）
}

// clusterOp 按锁序加锁后在同一事务里执行 fn，返回改后的集群部署（含成员与聚合）。
// 集群部署不存在、不属于该用户（或系统集群而用户不是系统账号）→ not_found。
func (s *Service) clusterOp(ctx context.Context, userID int64, acdID string, fn func(tx pgx.Tx, st *clusterOpState, now time.Time) error) (ClusterDeployment, error) {
	now := s.d.Clock.Now()
	var out ClusterDeployment
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		pre, err := scanClusterDep(tx.QueryRow(ctx, `SELECT `+acdCols+` FROM app_cluster_deployments WHERE id = $1`, acdID))
		if err != nil {
			return err
		}
		c, err := lockCluster(ctx, tx, pre.ClusterID)
		if errors.Is(err, apperr.ErrClusterNotFound) {
			return apperr.ErrNotFound
		}
		if err != nil {
			return err
		}
		if pre.UserID != userID || !s.clusterVisible(c, userID) {
			return apperr.ErrNotFound
		}
		ids, err := memberIDs(ctx, tx, c.ID)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT DISTINCT instance_id FROM app_deployments WHERE cluster_deployment_id = $1 AND state <> 'removed'`, acdID)
		if err != nil {
			return err
		}
		depInsts, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		locked, err := lockInstances(ctx, tx, append(ids, depInsts...))
		if err != nil {
			return err
		}
		deps, err := collectDeployments(tx.Query(ctx, `SELECT `+depCols+` FROM app_deployments WHERE cluster_deployment_id = $1 AND state <> 'removed'
			ORDER BY id FOR UPDATE`, acdID))
		if err != nil {
			return err
		}
		acd, err := scanClusterDep(tx.QueryRow(ctx, `SELECT `+acdCols+` FROM app_cluster_deployments WHERE id = $1 FOR UPDATE`, acdID))
		if err != nil {
			return err
		}
		for _, d := range deps { // 加锁之前读的实例清单之后新出现的成员部署（换机复制）：实例未锁 → 让调用方重试
			if _, ok := locked[d.InstanceID]; !ok {
				return fmt.Errorf("appdeploy: cluster deployment %s changed during lock, retry", acdID)
			}
		}
		st := &clusterOpState{cluster: c, acd: acd, locked: locked, members: currentMembers(c.ID, locked), deps: deps}
		if err := fn(tx, st, now); err != nil {
			return err
		}
		out, err = s.loadClusterDeployment(ctx, tx, acdID)
		return err
	})
	if err != nil {
		return ClusterDeployment{}, err
	}
	return out, nil
}

// liveDeps 返回 st.deps 里的非终态部署。
func (st *clusterOpState) liveDeps() []Deployment {
	return slices.DeleteFunc(slices.Clone(st.deps), func(d Deployment) bool { return !d.Live() })
}

// RedeployCluster 整组重新部署（规格 §6.5）：VersionID nil = 最新已发布版本（与单实例 Redeploy 一致）；
// 显式指定集群部署当前的版本 = 原样重新部署（即使已下线）。EnvSet = false 时变量沿用（以 head 的为准），true 时按 mergeEnv 规则。
// 要求集群 active 且在位成员完整（invalid_state）、集群部署 active 且没有成员在 installing / removing（app_invalid_state）。
//   - 已不是集群当前成员的实例上的成员部署（换机后旧实例上的 lost、或尚未终态的）→ removed；
//   - 当前成员上缺部署的 rank（换机时未复制）→ 补建 queued 部署（同样校验镜像 / 上限 / 端口）；
//   - 其余成员部署 → queued（换版本 / 变量，attempt 不变）；已是 queued、同版本且未改变量的不动（Runner 可能正在上传）。
//
// 审计 cluster.app_redeploy 一条。
func (s *Service) RedeployCluster(ctx context.Context, req ClusterRedeployRequest) (ClusterDeployment, error) {
	return s.clusterOp(ctx, req.UserID, req.ClusterDeploymentID, func(tx pgx.Tx, st *clusterOpState, now time.Time) error {
		if st.acd.State != ClusterDepActive {
			return apperr.ErrAppInvalidState
		}
		if st.cluster.State != rental.ClusterActive || !fullMembers(st.members, st.cluster.Size) {
			return apperr.ErrInvalidState
		}
		current := map[string]member{}
		for _, m := range st.members {
			current[m.inst.ID] = m
		}
		byInst := map[string]Deployment{} // 当前成员上的非终态成员部署
		var head *Deployment
		for _, d := range st.liveDeps() {
			if _, ok := current[d.InstanceID]; !ok {
				continue
			}
			if d.State == StateInstalling || d.State == StateRemoving {
				return apperr.ErrAppInvalidState
			}
			byInst[d.InstanceID] = d
			if m := current[d.InstanceID]; head == nil || m.rank < current[head.InstanceID].rank {
				dd := d
				head = &dd
			}
		}
		base := head
		if base == nil && len(st.deps) > 0 { // 全员都换过机且旧部署都是 lost：沿用最新一条的变量
			b := slices.MaxFunc(st.deps, func(a, b Deployment) int { return a.CreatedAt.Compare(b.CreatedAt) })
			base = &b
		}
		var ver Version
		var err error
		if req.VersionID != nil && *req.VersionID == st.acd.AppVersionID {
			ver, err = scanVersion(tx.QueryRow(ctx, `SELECT `+versionCols+` FROM app_versions WHERE id = $1`, st.acd.AppVersionID))
		} else {
			ver, err = s.pickVersion(ctx, tx, st.acd.AppID, req.VersionID)
		}
		if err != nil {
			return err
		}
		if err := checkClusterSize(ver, st.cluster.Size); err != nil {
			return err
		}
		var oldPlain, oldSecret map[string]string
		if base != nil {
			oldPlain = base.Env
			if oldSecret, err = openSecrets(s.d.Box, base.SecretEnvEnc); err != nil {
				return err
			}
		}
		plain, secret, err := ValidateEnv(ver.Spec, mergeEnv(ver.Spec, oldPlain, oldSecret, req.Env, req.EnvSet))
		if err != nil {
			return err
		}
		name, err := appName(ctx, tx, st.acd.AppID)
		if err != nil {
			return err
		}
		actor := audit.Actor{UserID: req.UserID, IP: req.IP}
		// 已不是当前成员的实例上的成员部署：移除（换机后旧实例上的 lost 部署即「需要重新部署」的标记，到此消除）
		for _, d := range st.deps {
			if _, ok := current[d.InstanceID]; ok {
				continue
			}
			if err := s.setRemoved(ctx, tx, d, "", "集群重新部署：实例已不是集群成员", false, now, actor); err != nil {
				return err
			}
		}
		for _, m := range st.members {
			role := roleFor(m.rank)
			d, ok := byInst[m.inst.ID]
			if !ok { // 补建
				if err := s.checkMemberSlot(ctx, tx, m.inst, st.acd.AppID, ver); err != nil {
					return err
				}
				nd, err := s.insertMember(ctx, tx, m.inst, st.cluster.UserID, st.acd.AppID, ver, st.acd.ID, role, plain, secret, now)
				if err != nil {
					return err
				}
				if err := addEvent(ctx, tx, nd.ID, "", StateQueued, fmt.Sprintf("集群重新部署：补建成员部署（%s，rank %d）v%s", role, m.rank, ver.Version), now); err != nil {
					return err
				}
				continue
			}
			if d.State == StateQueued && d.AppVersionID == ver.ID && !req.EnvSet && d.ClusterRole == role {
				continue
			}
			if err := s.checkImage(ctx, tx, ver, m.inst.ImageID); err != nil {
				return err
			}
			if d.AppVersionID != ver.ID {
				oldVer, err := scanVersion(tx.QueryRow(ctx, `SELECT `+versionCols+` FROM app_versions WHERE id = $1`, d.AppVersionID))
				if err != nil {
					return err
				}
				if !slices.Equal(portNumbers(oldVer.Spec.Ports), portNumbers(ver.Spec.Ports)) {
					if err := s.checkPorts(ctx, tx, m.inst.ID, ver.Spec.Ports, d.ID); err != nil {
						return err
					}
					if err := deletePortsBecause(ctx, tx, d, "重新部署应用 "+name+"（端口变更）", now, actor); err != nil {
						return err
					}
				}
			}
			if _, err := tx.Exec(ctx, `UPDATE app_deployments SET state = 'queued', desired = 'running', app_version_id = $2, spec_hash = $3, port = $4,
				env = $5, secret_env_enc = $6, phase = '', progress = 0, message = '', health = 'unknown', error_code = '', crash_count = 0, unreachable_count = 0,
				next_poll_at = $7, finished_at = NULL, updated_at = $7, cluster_role = $8, row_version = row_version + 1 WHERE id = $1`,
				d.ID, ver.ID, ver.SpecHash, ver.Spec.Ports[0].Port, mustJSON(plain), sealSecrets(s.d.Box, secret), now, role); err != nil {
				return err
			}
			if err := addEvent(ctx, tx, d.ID, d.State, StateQueued, "集群重新部署 v"+ver.Version, now); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE app_cluster_deployments SET app_version_id = $2, updated_at = $3 WHERE id = $1`, st.acd.ID, ver.ID, now); err != nil {
			return err
		}
		return auditCluster(ctx, tx, now, actor, "cluster.app_redeploy", st.cluster.ID, st.acd.ID, st.acd.AppID, ver.ID)
	})
}

// StopCluster 整组停止：installing / running 的成员部署 desired = stopped（Runner 执行 run.sh stop）；没有可停的 → app_invalid_state。
func (s *Service) StopCluster(ctx context.Context, userID int64, id, ip string) (ClusterDeployment, error) {
	return s.clusterOp(ctx, userID, id, func(tx pgx.Tx, st *clusterOpState, now time.Time) error {
		if st.acd.State != ClusterDepActive {
			return apperr.ErrAppInvalidState
		}
		n := 0
		for _, d := range st.liveDeps() {
			if d.State != StateInstalling && d.State != StateRunning {
				continue
			}
			n++
			if d.Desired == DesiredStopped {
				continue
			}
			if _, err := tx.Exec(ctx, `UPDATE app_deployments SET desired = 'stopped', next_poll_at = $2, updated_at = $2, row_version = row_version + 1
				WHERE id = $1`, d.ID, now); err != nil {
				return err
			}
			if err := addEvent(ctx, tx, d.ID, d.State, d.State, "集群应用：请求停止", now); err != nil {
				return err
			}
		}
		if n == 0 {
			return apperr.ErrAppInvalidState
		}
		if _, err := tx.Exec(ctx, `UPDATE app_cluster_deployments SET updated_at = $2 WHERE id = $1`, st.acd.ID, now); err != nil {
			return err
		}
		return auditCluster(ctx, tx, now, audit.Actor{UserID: userID, IP: ip}, "cluster.app_stop", st.cluster.ID, st.acd.ID, st.acd.AppID, st.acd.AppVersionID)
	})
}

// StartCluster 整组启动：stopped 的成员部署 → queued / desired running（同 Start）；没有已停止的 → app_invalid_state。
func (s *Service) StartCluster(ctx context.Context, userID int64, id, ip string) (ClusterDeployment, error) {
	return s.clusterOp(ctx, userID, id, func(tx pgx.Tx, st *clusterOpState, now time.Time) error {
		if st.acd.State != ClusterDepActive {
			return apperr.ErrAppInvalidState
		}
		n := 0
		for _, d := range st.liveDeps() {
			if d.State != StateStopped {
				continue
			}
			n++
			if _, err := tx.Exec(ctx, `UPDATE app_deployments SET state = 'queued', desired = 'running', phase = '', progress = 0, message = '',
				health = 'unknown', error_code = '', crash_count = 0, unreachable_count = 0, next_poll_at = $2, finished_at = NULL, updated_at = $2,
				row_version = row_version + 1 WHERE id = $1`, d.ID, now); err != nil {
				return err
			}
			if err := addEvent(ctx, tx, d.ID, d.State, StateQueued, "集群应用：启动", now); err != nil {
				return err
			}
		}
		if n == 0 {
			return apperr.ErrAppInvalidState
		}
		if _, err := tx.Exec(ctx, `UPDATE app_cluster_deployments SET updated_at = $2 WHERE id = $1`, st.acd.ID, now); err != nil {
			return err
		}
		return auditCluster(ctx, tx, now, audit.Actor{UserID: userID, IP: ip}, "cluster.app_start", st.cluster.ID, st.acd.ID, st.acd.AppID, st.acd.AppVersionID)
	})
}

// RemoveCluster 整组卸载：全部非终态成员部署按单实例卸载规则（remove）处理，集群部署置 removed；已 removed → 原样返回（幂等，不再审计）。
// 不要求集群仍 active（集群降级 / 结束时也能卸载）。
func (s *Service) RemoveCluster(ctx context.Context, userID int64, id, ip string) (ClusterDeployment, error) {
	return s.clusterOp(ctx, userID, id, func(tx pgx.Tx, st *clusterOpState, now time.Time) error {
		if st.acd.State == ClusterDepRemoved {
			return nil
		}
		actor := audit.Actor{UserID: userID, IP: ip}
		for _, d := range st.liveDeps() {
			if d.State == StateRemoving {
				continue
			}
			if _, err := s.remove(ctx, tx, st.locked[d.InstanceID].State, d, "", actor, now); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE app_cluster_deployments SET state = 'removed', updated_at = $2 WHERE id = $1`, st.acd.ID, now); err != nil {
			return err
		}
		return auditCluster(ctx, tx, now, actor, "cluster.app_remove", st.cluster.ID, st.acd.ID, st.acd.AppID, st.acd.AppVersionID)
	})
}
