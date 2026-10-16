package fleet

import (
	"context"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/secretbox"
	"spark-cloud/internal/plugins/pdu"
)

type PDU struct {
	ID           int64     `json:"id"`
	Name         string    `json:"name"`
	Driver       string    `json:"driver"`
	Address      string    `json:"address"`
	HasCommunity bool      `json:"has_community"` // community 加密保存，接口只告诉是否已设置
	OIDPrefix    string    `json:"oid_prefix"`
	CycleValue   int       `json:"cycle_value"`
	OnValue      int       `json:"on_value"`
	CreatedAt    time.Time `json:"created_at"`
}

// PDUInput 是新建或修改 PDU 的参数；修改时 Community 为空表示保持原值。
type PDUInput struct {
	Name       string
	Driver     string // snmp / mock
	Address    string
	Community  string
	OIDPrefix  string
	CycleValue int
	OnValue    int
}

// PDUConn 是打开 PDU 驱动所需的参数（含解密后的 community）。
type PDUConn struct {
	Driver     string
	Address    string
	Community  string
	OIDPrefix  string
	CycleValue int
	OnValue    int
}

type PDUOpener func(PDUConn) (pdu.Driver, error)

type PDUConfig struct {
	Box       *secretbox.Box
	Open      PDUOpener
	AllowMock bool // 生产环境为 false：不允许创建或使用 driver=mock 的 PDU
}

// ConfigurePDU 设置 PDU 相关依赖；未配置时 PDU 操作返回错误。
func (s *Service) ConfigurePDU(c PDUConfig) { s.pdu = c }

var (
	oidRe       = regexp.MustCompile(`^\d+(\.\d+)+$`)
	errPDUSetup = fmt.Errorf("fleet: PDU 未配置")
)

const pduCols = `id, name, driver, address, community_enc IS NOT NULL, oid_prefix, cycle_value, on_value, created_at`

func scanPDU(row pgx.Row) (PDU, error) {
	var p PDU
	err := row.Scan(&p.ID, &p.Name, &p.Driver, &p.Address, &p.HasCommunity, &p.OIDPrefix, &p.CycleValue, &p.OnValue, &p.CreatedAt)
	if db.IsNoRows(err) {
		return PDU{}, apperr.ErrNotFound
	}
	return p, err
}

func (s *Service) normalizePDU(in PDUInput, creating bool) (PDUInput, error) {
	in.Name = strings.TrimSpace(in.Name)
	in.Address = strings.TrimSpace(in.Address)
	in.OIDPrefix = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(in.OIDPrefix), "."), ".")
	if in.Name == "" || utf8.RuneCountInString(in.Name) > 64 {
		return in, apperr.BadRequest("请填写 PDU 名称（最多 64 个字符）")
	}
	switch in.Driver {
	case "snmp":
	case "mock":
		if !s.pdu.AllowMock {
			return in, apperr.BadRequest("生产环境不能使用 mock PDU")
		}
	default:
		return in, apperr.BadRequest("PDU 驱动只能是 snmp 或 mock")
	}
	host := in.Address
	if h, _, err := net.SplitHostPort(in.Address); err == nil {
		host = h
	}
	if host == "" || strings.ContainsAny(host, " /") {
		return in, apperr.BadRequest("PDU 地址格式不正确")
	}
	if creating && in.Driver == "snmp" && in.Community == "" {
		return in, apperr.BadRequest("请填写 SNMP community")
	}
	if in.OIDPrefix == "" {
		in.OIDPrefix = pdu.DefaultOIDPrefix
	}
	if !oidRe.MatchString(in.OIDPrefix) {
		return in, apperr.BadRequest("OID 前缀格式不正确")
	}
	if in.CycleValue == 0 {
		in.CycleValue = pdu.DefaultCycleValue
	}
	if in.OnValue == 0 {
		in.OnValue = pdu.DefaultOnValue
	}
	return in, nil
}

func pduDiff(in PDUInput) map[string]any {
	return map[string]any{"name": in.Name, "driver": in.Driver, "address": in.Address, "oid_prefix": in.OIDPrefix,
		"cycle_value": in.CycleValue, "on_value": in.OnValue, "community_changed": in.Community != ""}
}

func (s *Service) ListPDUs(ctx context.Context) ([]PDU, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+pduCols+` FROM pdus ORDER BY name`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (PDU, error) { return scanPDU(r) })
}

func (s *Service) sealCommunity(c string) []byte {
	if c == "" {
		return nil
	}
	return s.pdu.Box.Seal([]byte(c))
}

func (s *Service) CreatePDU(ctx context.Context, actor audit.Actor, in PDUInput) (PDU, error) {
	if s.pdu.Box == nil {
		return PDU{}, errPDUSetup
	}
	in, err := s.normalizePDU(in, true)
	if err != nil {
		return PDU{}, err
	}
	now := s.clock.Now()
	var p PDU
	err = db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		p, err = scanPDU(tx.QueryRow(ctx, `INSERT INTO pdus (name, driver, address, community_enc, oid_prefix, cycle_value, on_value, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8) RETURNING `+pduCols,
			in.Name, in.Driver, in.Address, s.sealCommunity(in.Community), in.OIDPrefix, in.CycleValue, in.OnValue, now))
		if db.IsUniqueViolation(err) {
			return apperr.New("duplicate_name", "PDU 名称已存在", 409)
		}
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("pdu.create", "pdu", strconv.FormatInt(p.ID, 10), pduDiff(in)))
	})
	return p, err
}

// UpdatePDU 修改 PDU；Community 为空表示保持原值。
func (s *Service) UpdatePDU(ctx context.Context, actor audit.Actor, id int64, in PDUInput) (PDU, error) {
	if s.pdu.Box == nil {
		return PDU{}, errPDUSetup
	}
	in, err := s.normalizePDU(in, false)
	if err != nil {
		return PDU{}, err
	}
	now := s.clock.Now()
	var p PDU
	err = db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		var stored []byte
		if err := tx.QueryRow(ctx, `SELECT community_enc FROM pdus WHERE id = $1 FOR UPDATE`, id).Scan(&stored); db.IsNoRows(err) {
			return apperr.ErrNotFound
		} else if err != nil {
			return err
		}
		if in.Driver == "snmp" && in.Community == "" && stored == nil {
			return apperr.BadRequest("请填写 SNMP community")
		}
		var err error
		p, err = scanPDU(tx.QueryRow(ctx, `UPDATE pdus SET name = $2, driver = $3, address = $4,
				community_enc = COALESCE($5, community_enc), oid_prefix = $6, cycle_value = $7, on_value = $8, updated_at = $9
			WHERE id = $1 RETURNING `+pduCols,
			id, in.Name, in.Driver, in.Address, s.sealCommunity(in.Community), in.OIDPrefix, in.CycleValue, in.OnValue, now))
		if db.IsUniqueViolation(err) {
			return apperr.New("duplicate_name", "PDU 名称已存在", 409)
		}
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("pdu.update", "pdu", strconv.FormatInt(id, 10), pduDiff(in)))
	})
	return p, err
}

// PDUIDByName 按名称查 PDU（批量导入机器时使用）。
func (s *Service) PDUIDByName(ctx context.Context, q db.Querier, name string) (int64, error) {
	var id int64
	err := q.QueryRow(ctx, `SELECT id FROM pdus WHERE name = $1`, strings.TrimSpace(name)).Scan(&id)
	if db.IsNoRows(err) {
		return 0, apperr.ErrNotFound
	}
	return id, err
}

// PowerCycle 对机器所接的 PDU 插座断电重启。只由管理员手动触发；成功与失败都写审计。
func (s *Service) PowerCycle(ctx context.Context, actor audit.Actor, machineID int64) error {
	if s.pdu.Box == nil || s.pdu.Open == nil {
		return errPDUSetup
	}
	var pduID *int64
	var outlet *int
	err := s.pool.QueryRow(ctx, `SELECT pdu_id, pdu_outlet FROM machines WHERE id = $1`, machineID).Scan(&pduID, &outlet)
	if db.IsNoRows(err) {
		return apperr.ErrNotFound
	}
	if err != nil {
		return err
	}
	if pduID == nil || outlet == nil {
		return apperr.BadRequest("该机器未配置 PDU")
	}
	var conn PDUConn
	var enc []byte
	if err := s.pool.QueryRow(ctx, `SELECT driver, address, community_enc, oid_prefix, cycle_value, on_value FROM pdus WHERE id = $1`, *pduID).
		Scan(&conn.Driver, &conn.Address, &enc, &conn.OIDPrefix, &conn.CycleValue, &conn.OnValue); err != nil {
		return err
	}
	if conn.Driver == "mock" && !s.pdu.AllowMock {
		return apperr.BadRequest("生产环境不能使用 mock PDU，请先把该 PDU 改为 snmp")
	}
	if enc != nil {
		plain, err := s.pdu.Box.Open(enc)
		if err != nil {
			return fmt.Errorf("decrypt pdu community: %w", err)
		}
		conn.Community = string(plain)
	}
	drv, err := s.pdu.Open(conn)
	if err == nil {
		err = drv.PowerCycle(ctx, *outlet)
	}
	diff := map[string]any{"pdu_id": *pduID, "outlet": *outlet, "ok": err == nil}
	if err != nil {
		diff["error"] = err.Error()
	}
	if aerr := audit.Record(context.WithoutCancel(ctx), s.pool, s.clock.Now(),
		actor.Entry("machine.power_cycle", "machine", strconv.FormatInt(machineID, 10), diff)); aerr != nil {
		return aerr
	}
	if err != nil {
		return apperr.New("pdu_failed", "PDU 操作失败："+err.Error(), 502)
	}
	return nil
}
