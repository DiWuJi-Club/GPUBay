package fleet

import (
	"context"
	"encoding/json"

	"spark-cloud/internal/platform/apperr"
)

// Agent 上报的容器阶段。
const (
	PhaseRunning     = "running"
	PhaseStopped     = "stopped"
	PhaseMissingData = "missing_data" // 容器在、数据盘未挂载
	PhaseUnknown     = "unknown"      // Agent 无法确认（docker inspect 出错）
)

type InstanceState struct {
	InstanceID string `json:"instance_id"`
	Phase      string `json:"phase"`
}

// StateReport 是 Agent 上报的本机实例集合，原文存入 machines.last_state_report。
type StateReport struct {
	Instances []InstanceState `json:"instances"`
}

// RunningIDs 返回报告中阶段为 running 的实例 ID。
func (r StateReport) RunningIDs() []string {
	var ids []string
	for _, s := range r.Instances {
		if s.Phase == PhaseRunning {
			ids = append(ids, s.InstanceID)
		}
	}
	return ids
}

// ConnOwner 是机器当前的连接归属：持有流的网关连接 id（无连接为空串）与证书指纹。
type ConnOwner struct {
	ConnID      string
	Fingerprint string
}

func (s *Service) MachineBySerial(ctx context.Context, serial string) (Machine, error) {
	return scanMachine(s.pool.QueryRow(ctx, `SELECT `+machineCols+` FROM machines WHERE serial = $1`, serial))
}

// AttachAgentConn 把机器的流归属到 connID（新连接顶掉旧归属），并在同一语句里把该机器所有 sent 指令的 sent_at 刷新为现在：
// 新流必定补推全部 sent 指令，超时从建流起重新计时，避免「心跳已上线、补推未发生」的窗口里清扫器按离线前的 sent_at 判超时。
func (s *Service) AttachAgentConn(ctx context.Context, machineID int64, connID string) error {
	var n int
	// 先按 id 锁指令行、再改机器行（m 的条件依赖 t，强制这个顺序）：与回报路径「指令 → 机器（置 faulty）」的加锁顺序一致。
	err := s.pool.QueryRow(ctx, `WITH l AS (
			SELECT id FROM agent_commands WHERE machine_id = $1 AND status = 'sent' ORDER BY id FOR UPDATE
		), t AS (
			UPDATE agent_commands SET sent_at = $3, updated_at = $3 FROM l WHERE agent_commands.id = l.id RETURNING agent_commands.id
		), m AS (
			UPDATE machines SET agent_conn_id = $2, agent_connected_at = $3, updated_at = $3
			WHERE id = $1 AND (SELECT count(*) FROM t) >= 0 RETURNING id
		)
		SELECT count(*) FROM m`, machineID, connID, s.clock.Now()).Scan(&n)
	if err != nil {
		return err
	}
	if n == 0 {
		return apperr.ErrNotFound
	}
	return nil
}

// DetachAgentConn 清除归属，但只清自己的：别的副本已接管时不动。
func (s *Service) DetachAgentConn(ctx context.Context, machineID int64, connID string) error {
	_, err := s.pool.Exec(ctx, `UPDATE machines SET agent_conn_id = NULL, updated_at = $3 WHERE id = $1 AND agent_conn_id = $2`,
		machineID, connID, s.clock.Now())
	return err
}

func (s *Service) ConnOwners(ctx context.Context, ids []int64) (map[int64]ConnOwner, error) {
	out := map[int64]ConnOwner{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT id, COALESCE(agent_conn_id, ''), cert_fingerprint FROM machines WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var o ConnOwner
		if err := rows.Scan(&id, &o.ConnID, &o.Fingerprint); err != nil {
			return nil, err
		}
		out[id] = o
	}
	return out, rows.Err()
}

// RecordMetrics 保存 Agent 版本与最近一份指标；metrics 为 nil 时只更新版本，version 为空时只更新指标。
func (s *Service) RecordMetrics(ctx context.Context, machineID int64, version string, metrics json.RawMessage) error {
	var m []byte
	if metrics != nil {
		m = metrics
	}
	_, err := s.pool.Exec(ctx, `UPDATE machines
		SET agent_version = CASE WHEN $2 = '' THEN agent_version ELSE $2 END,
		    last_metrics = COALESCE($3::jsonb, last_metrics), updated_at = $4
		WHERE id = $1`, machineID, version, m, s.clock.Now())
	return err
}

func (s *Service) RecordStateReport(ctx context.Context, machineID int64, r StateReport) error {
	if r.Instances == nil {
		r.Instances = []InstanceState{}
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `UPDATE machines SET last_state_report = $2, state_reported_at = $3, updated_at = $3 WHERE id = $1`,
		machineID, b, s.clock.Now())
	return err
}

// ReportsFor 返回 ids 中在线、且本次连接之后上报过的机器的最近一份 StateReport（清扫器兜底恢复计时用）。
func (s *Service) ReportsFor(ctx context.Context, ids []int64) (map[int64]StateReport, error) {
	out := map[int64]StateReport{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT id, last_state_report FROM machines
		WHERE id = ANY($1) AND agent_online AND last_state_report IS NOT NULL
		  AND agent_connected_at IS NOT NULL AND state_reported_at >= agent_connected_at`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		var r StateReport
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, err
		}
		out[id] = r
	}
	return out, rows.Err()
}
