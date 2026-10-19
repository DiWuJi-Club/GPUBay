package pdu

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/gosnmp/gosnmp"
)

type SNMPConfig struct {
	Address    string // host 或 host:port（默认端口 161）
	Community  string // 写 community
	OIDPrefix  string // 插座控制 OID 前缀，完整 OID 为 <前缀>.<插座号>
	CycleValue int    // 写入即断电重启的值
	OnValue    int    // 读到该值表示通电
	Timeout    time.Duration
}

// SNMP 通过 SNMP v2c 控制 PDU 插座。
type SNMP struct {
	cfg SNMPConfig
}

func NewSNMP(cfg SNMPConfig) *SNMP {
	if cfg.OIDPrefix == "" {
		cfg.OIDPrefix = DefaultOIDPrefix
	}
	if cfg.CycleValue == 0 {
		cfg.CycleValue = DefaultCycleValue
	}
	if cfg.OnValue == 0 {
		cfg.OnValue = DefaultOnValue
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 2 * time.Second
	}
	cfg.OIDPrefix = strings.TrimSuffix(cfg.OIDPrefix, ".")
	return &SNMP{cfg: cfg}
}

func (s *SNMP) oid(outlet int) string { return fmt.Sprintf("%s.%d", s.cfg.OIDPrefix, outlet) }

func (s *SNMP) connect(ctx context.Context) (*gosnmp.GoSNMP, error) {
	host, port := s.cfg.Address, uint16(161)
	if h, p, err := net.SplitHostPort(s.cfg.Address); err == nil {
		n, err := strconv.ParseUint(p, 10, 16)
		if err != nil {
			return nil, fmt.Errorf("pdu: 端口不正确 %q", s.cfg.Address)
		}
		host, port = h, uint16(n)
	}
	g := &gosnmp.GoSNMP{Target: host, Port: port, Community: s.cfg.Community, Version: gosnmp.Version2c,
		Timeout: s.cfg.Timeout, Retries: 0, Context: ctx}
	if err := g.Connect(); err != nil {
		return nil, fmt.Errorf("pdu: 连接 %s: %w", s.cfg.Address, err)
	}
	return g, nil
}

func (s *SNMP) PowerCycle(ctx context.Context, outlet int) error {
	if outlet < 1 {
		return fmt.Errorf("pdu: 插座号必须大于 0")
	}
	g, err := s.connect(ctx)
	if err != nil {
		return err
	}
	defer g.Conn.Close()
	oid := s.oid(outlet)
	res, err := g.Set([]gosnmp.SnmpPDU{{Name: oid, Type: gosnmp.Integer, Value: s.cfg.CycleValue}})
	if err != nil {
		return fmt.Errorf("pdu: set %s: %w", oid, err)
	}
	if res.Error != gosnmp.NoError {
		return fmt.Errorf("pdu: set %s: %s", oid, res.Error)
	}
	return nil
}

func (s *SNMP) Status(ctx context.Context, outlet int) (bool, error) {
	if outlet < 1 {
		return false, fmt.Errorf("pdu: 插座号必须大于 0")
	}
	g, err := s.connect(ctx)
	if err != nil {
		return false, err
	}
	defer g.Conn.Close()
	oid := s.oid(outlet)
	res, err := g.Get([]string{oid})
	if err != nil {
		return false, fmt.Errorf("pdu: get %s: %w", oid, err)
	}
	if res.Error != gosnmp.NoError || len(res.Variables) != 1 {
		return false, fmt.Errorf("pdu: get %s: %s", oid, res.Error)
	}
	v := res.Variables[0]
	if v.Type == gosnmp.NoSuchObject || v.Type == gosnmp.NoSuchInstance {
		return false, fmt.Errorf("pdu: 插座 %d 不存在（%s）", outlet, oid)
	}
	return gosnmp.ToBigInt(v.Value).Int64() == int64(s.cfg.OnValue), nil
}
