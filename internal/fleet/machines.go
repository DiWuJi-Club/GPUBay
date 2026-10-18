// Package fleet 管理物理机器：库存分配、Agent 心跳、指令队列与告警。
package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/db"
)

const (
	StateIdle        = "idle"
	StateAllocated   = "allocated"
	StateMaintenance = "maintenance"
	StateFaulty      = "faulty"
)

type Machine struct {
	ID          int64      `json:"id"`
	Serial      string     `json:"serial"`
	Rack        string     `json:"rack"`
	Slot        string     `json:"slot"`
	LanIP       string     `json:"lan_ip"`
	ProductID   int64      `json:"product_id"`
	State       string     `json:"state"`
	AgentOnline bool       `json:"agent_online"`
	LastSeenAt  *time.Time `json:"last_seen_at"`
	PDUID       *int64     `json:"pdu_id"`
	PDUOutlet   *int       `json:"pdu_outlet"`
	CreatedAt   time.Time  `json:"created_at"`

	// 计划 2：Agent 接入与连接状态。指纹全文不输出，只给前 16 位。
	CertFingerprint   string          `json:"-"`
	Enrolled          bool            `json:"enrolled"`
	FingerprintPrefix string          `json:"fingerprint_prefix"`
	Connected         bool            `json:"connected"`
	AgentVersion      string          `json:"agent_version"`
	AgentConnectedAt  *time.Time      `json:"agent_connected_at"`
	StateReportedAt   *time.Time      `json:"state_reported_at"`
	LastMetrics       json.RawMessage `json:"last_metrics"`
}

type NewMachine struct {
	Serial    string
	Rack      string
	Slot      string
	LanIP     string
	ProductID int64
	PDUID     *int64
	PDUOutlet *int
}

type Service struct {
	pool  *pgxpool.Pool
	clock clock.Clock
	pdu   PDUConfig
}

func NewService(pool *pgxpool.Pool, clk clock.Clock) *Service {
	return &Service{pool: pool, clock: clk}
}

const machineCols = `id, serial, rack, slot, lan_ip, product_id, state, agent_online, last_seen_at, pdu_id, pdu_outlet, created_at,
	cert_fingerprint, agent_conn_id, agent_version, agent_connected_at, state_reported_at, last_metrics`

func scanMachine(row pgx.Row) (Machine, error) {
	var m Machine
	var connID *string
	var metrics []byte
	err := row.Scan(&m.ID, &m.Serial, &m.Rack, &m.Slot, &m.LanIP, &m.ProductID, &m.State, &m.AgentOnline, &m.LastSeenAt,
		&m.PDUID, &m.PDUOutlet, &m.CreatedAt, &m.CertFingerprint, &connID, &m.AgentVersion, &m.AgentConnectedAt, &m.StateReportedAt, &metrics)
	if db.IsNoRows(err) {
		return Machine{}, apperr.ErrNotFound
	}
	m.Enrolled = m.CertFingerprint != ""
	if len(m.CertFingerprint) >= 16 {
		m.FingerprintPrefix = m.CertFingerprint[:16]
	}
	m.Connected = connID != nil
	if metrics != nil {
		m.LastMetrics = json.RawMessage(metrics)
	}
	return m, err
}

// Register 登记一台新机器，初始状态为维护中，需管理员确认后上线。
func (s *Service) Register(ctx context.Context, nm NewMachine) (Machine, error) {
	return s.register(ctx, s.pool, nm)
}

// register 校验并插入一台机器；q 可以是事务（批量导入时整批在一个事务内）。
func (s *Service) register(ctx context.Context, q db.Querier, nm NewMachine) (Machine, error) {
	nm.Serial = strings.TrimSpace(nm.Serial)
	if nm.Serial == "" {
		return Machine{}, apperr.BadRequest("请填写序列号")
	}
	if net.ParseIP(nm.LanIP) == nil {
		return Machine{}, apperr.BadRequest("内网 IP 格式不正确")
	}
	if (nm.PDUID == nil) != (nm.PDUOutlet == nil) {
		return Machine{}, apperr.BadRequest("PDU 与插座号需同时填写")
	}
	if nm.PDUOutlet != nil && (*nm.PDUOutlet < 1 || *nm.PDUOutlet > 256) {
		return Machine{}, apperr.BadRequest("PDU 插座号需在 1～256 之间")
	}
	now := s.clock.Now()
	m, err := scanMachine(q.QueryRow(ctx, `INSERT INTO machines (serial, rack, slot, lan_ip, product_id, pdu_id, pdu_outlet, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8) RETURNING `+machineCols,
		nm.Serial, nm.Rack, nm.Slot, nm.LanIP, nm.ProductID, nm.PDUID, nm.PDUOutlet, now))
	switch {
	case db.IsUniqueViolation(err) && db.ConstraintName(err) == "machines_pdu_outlet_uniq":
		return Machine{}, apperr.New("pdu_outlet_taken", "该 PDU 插座已被其他机器使用", 409)
	case db.IsUniqueViolation(err):
		return Machine{}, apperr.New("duplicate_serial", "序列号已存在", 409)
	case db.IsForeignKeyViolation(err) && db.ConstraintName(err) == "machines_pdu_id_fkey":
		return Machine{}, apperr.BadRequest("PDU 不存在")
	case db.IsForeignKeyViolation(err):
		return Machine{}, apperr.BadRequest("商品不存在")
	}
	return m, err
}

func (s *Service) Get(ctx context.Context, q db.Querier, id int64) (Machine, error) {
	return scanMachine(q.QueryRow(ctx, `SELECT `+machineCols+` FROM machines WHERE id = $1`, id))
}

// SetMaintenance 进入维护（仅空闲或故障机器）或结束维护回到空闲（仅维护中或故障机器）。
func (s *Service) SetMaintenance(ctx context.Context, id int64, on bool) error {
	return s.SetMaintenanceIn(ctx, s.pool, id, on)
}

// SetMaintenanceIn 与 SetMaintenance 相同，但在调用方给定的连接或事务内执行。
func (s *Service) SetMaintenanceIn(ctx context.Context, q db.Querier, id int64, on bool) error {
	from, to := []string{StateIdle, StateFaulty}, StateMaintenance
	if !on {
		from, to = []string{StateMaintenance, StateFaulty}, StateIdle
	}
	tag, err := q.Exec(ctx, `UPDATE machines SET state = $2, updated_at = $3 WHERE id = $1 AND state = ANY($4)`,
		id, to, s.clock.Now(), from)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		if _, err := s.Get(ctx, q, id); err != nil {
			return err
		}
		return apperr.ErrInvalidState
	}
	return nil
}

// Allocate 在调用方事务内锁定一台空闲且在线的机器并标记为已分配。
func (s *Service) Allocate(ctx context.Context, tx pgx.Tx, productID int64) (Machine, error) {
	m, err := scanMachine(tx.QueryRow(ctx, `UPDATE machines SET state = 'allocated', updated_at = $2
		WHERE id = (
			SELECT id FROM machines
			WHERE product_id = $1 AND state = 'idle' AND agent_online
			ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED
		) RETURNING `+machineCols, productID, s.clock.Now()))
	if errors.Is(err, apperr.ErrNotFound) {
		return Machine{}, apperr.ErrNoCapacity
	}
	return m, err
}

// Release 把已分配的机器放回空闲；故障或维护中的机器保持原状态。
func (s *Service) Release(ctx context.Context, q db.Querier, machineID int64) error {
	_, err := q.Exec(ctx, `UPDATE machines SET state = 'idle', updated_at = $2 WHERE id = $1 AND state = 'allocated'`,
		machineID, s.clock.Now())
	return err
}

func (s *Service) MarkFaulty(ctx context.Context, q db.Querier, machineID int64) error {
	_, err := q.Exec(ctx, `UPDATE machines SET state = 'faulty', updated_at = $2 WHERE id = $1`, machineID, s.clock.Now())
	return err
}

func (s *Service) IdleCount(ctx context.Context, productID int64) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM machines WHERE product_id = $1 AND state = 'idle' AND agent_online`,
		productID).Scan(&n)
	return n, err
}

// Heartbeat 记录 Agent 心跳；返回值表示机器是否刚从离线恢复。
func (s *Service) Heartbeat(ctx context.Context, machineID int64) (bool, error) {
	now := s.clock.Now()
	var wasOnline bool
	err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT agent_online FROM machines WHERE id = $1 FOR UPDATE`, machineID).Scan(&wasOnline)
		if db.IsNoRows(err) {
			return apperr.ErrNotFound
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE machines SET agent_online = true, last_seen_at = $2, updated_at = $2 WHERE id = $1`, machineID, now)
		return err
	})
	if err != nil {
		return false, err
	}
	return !wasOnline, nil
}

// MarkOfflineStale 把超过 timeout 没有心跳的在线机器标记为离线，返回这些机器的 ID。
// 同时清空 state_reported_at：流若撑过了离线期（未重连），清扫器不能拿离线前的报告恢复计时，须等新的 StateReport。
func (s *Service) MarkOfflineStale(ctx context.Context, timeout time.Duration) ([]int64, error) {
	now := s.clock.Now()
	rows, err := s.pool.Query(ctx, `UPDATE machines SET agent_online = false, state_reported_at = NULL, updated_at = $1
		WHERE agent_online AND (last_seen_at IS NULL OR last_seen_at < $2) RETURNING id`, now, now.Add(-timeout))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

// OfflineMachineIDs 返回当前 Agent 离线的全部机器 id。
func (s *Service) OfflineMachineIDs(ctx context.Context) ([]int64, error) {
	return s.queryIDs(ctx, `SELECT id FROM machines WHERE NOT agent_online ORDER BY id`)
}

// OnlineAmong 返回 ids 中当前 Agent 在线的子集。
func (s *Service) OnlineAmong(ctx context.Context, ids []int64) ([]int64, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	return s.queryIDs(ctx, `SELECT id FROM machines WHERE agent_online AND id = ANY($1) ORDER BY id`, ids)
}

func (s *Service) queryIDs(ctx context.Context, sql string, args ...any) ([]int64, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// Pool 返回连接池，供需要传入 db.Querier 的只读调用使用。
func (s *Service) Pool() *pgxpool.Pool { return s.pool }
