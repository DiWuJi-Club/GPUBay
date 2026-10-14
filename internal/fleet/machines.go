// Package fleet 管理物理机器：库存分配、Agent 心跳、指令队列与告警。
package fleet

import (
	"context"
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
	CreatedAt   time.Time  `json:"created_at"`
}

type NewMachine struct {
	Serial    string
	Rack      string
	Slot      string
	LanIP     string
	ProductID int64
}

type Service struct {
	pool  *pgxpool.Pool
	clock clock.Clock
}

func NewService(pool *pgxpool.Pool, clk clock.Clock) *Service {
	return &Service{pool: pool, clock: clk}
}

const machineCols = `id, serial, rack, slot, lan_ip, product_id, state, agent_online, last_seen_at, created_at`

func scanMachine(row pgx.Row) (Machine, error) {
	var m Machine
	err := row.Scan(&m.ID, &m.Serial, &m.Rack, &m.Slot, &m.LanIP, &m.ProductID, &m.State, &m.AgentOnline, &m.LastSeenAt, &m.CreatedAt)
	if db.IsNoRows(err) {
		return Machine{}, apperr.ErrNotFound
	}
	return m, err
}

// Register 登记一台新机器，初始状态为维护中，需管理员确认后上线。
func (s *Service) Register(ctx context.Context, nm NewMachine) (Machine, error) {
	nm.Serial = strings.TrimSpace(nm.Serial)
	if nm.Serial == "" {
		return Machine{}, apperr.BadRequest("请填写序列号")
	}
	if net.ParseIP(nm.LanIP) == nil {
		return Machine{}, apperr.BadRequest("内网 IP 格式不正确")
	}
	now := s.clock.Now()
	m, err := scanMachine(s.pool.QueryRow(ctx, `INSERT INTO machines (serial, rack, slot, lan_ip, product_id, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $6) RETURNING `+machineCols,
		nm.Serial, nm.Rack, nm.Slot, nm.LanIP, nm.ProductID, now))
	if db.IsUniqueViolation(err) {
		return Machine{}, apperr.New("duplicate_serial", "序列号已存在", 409)
	}
	return m, err
}

func (s *Service) Get(ctx context.Context, q db.Querier, id int64) (Machine, error) {
	return scanMachine(q.QueryRow(ctx, `SELECT `+machineCols+` FROM machines WHERE id = $1`, id))
}

// SetMaintenance 进入维护（仅空闲或故障机器）或结束维护回到空闲（仅维护中或故障机器）。
func (s *Service) SetMaintenance(ctx context.Context, id int64, on bool) error {
	from, to := []string{StateIdle, StateFaulty}, StateMaintenance
	if !on {
		from, to = []string{StateMaintenance, StateFaulty}, StateIdle
	}
	tag, err := s.pool.Exec(ctx, `UPDATE machines SET state = $2, updated_at = $3 WHERE id = $1 AND state = ANY($4)`,
		id, to, s.clock.Now(), from)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		if _, err := s.Get(ctx, s.pool, id); err != nil {
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
func (s *Service) MarkOfflineStale(ctx context.Context, timeout time.Duration) ([]int64, error) {
	now := s.clock.Now()
	rows, err := s.pool.Query(ctx, `UPDATE machines SET agent_online = false, updated_at = $1
		WHERE agent_online AND (last_seen_at IS NULL OR last_seen_at < $2) RETURNING id`, now, now.Add(-timeout))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}
