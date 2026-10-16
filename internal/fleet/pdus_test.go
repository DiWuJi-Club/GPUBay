package fleet_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/platform/secretbox"
	"spark-cloud/internal/plugins/pdu"
)

var ops = audit.Actor{AdminID: 3, IP: "10.0.0.3"}

func codeOf(err error) string {
	if ae := apperr.As(err); ae != nil {
		return ae.Code
	}
	if err != nil {
		return "raw: " + err.Error()
	}
	return ""
}

// configurePDU 让所有 PDU 都使用同一个 mock 驱动，并记录打开驱动时拿到的连接参数。
func configurePDU(t *testing.T, svc *fleet.Service, allowMock bool) (*pdu.Mock, *[]fleet.PDUConn) {
	t.Helper()
	box, err := secretbox.New(dbtest.Key)
	if err != nil {
		t.Fatal(err)
	}
	m := &pdu.Mock{}
	var conns []fleet.PDUConn
	svc.ConfigurePDU(fleet.PDUConfig{Box: box, AllowMock: allowMock,
		Open: func(c fleet.PDUConn) (pdu.Driver, error) { conns = append(conns, c); return m, nil }})
	return m, &conns
}

func snmpInput(name string) fleet.PDUInput {
	return fleet.PDUInput{Name: name, Driver: "snmp", Address: "10.0.9.1:161", Community: "s3cret-community"}
}

func TestCreatePDUValidatesAndEncrypts(t *testing.T) {
	svc, pool, _ := newEnv(t)
	configurePDU(t, svc, false)
	ctx := context.Background()
	for _, c := range []struct {
		in   fleet.PDUInput
		want string
	}{
		{fleet.PDUInput{Driver: "snmp", Address: "10.0.9.1", Community: "x"}, "bad_request"},
		{fleet.PDUInput{Name: "A", Driver: "ipmi", Address: "10.0.9.1", Community: "x"}, "bad_request"},
		{fleet.PDUInput{Name: "A", Driver: "snmp", Address: "", Community: "x"}, "bad_request"},
		{fleet.PDUInput{Name: "A", Driver: "snmp", Address: "10.0.9.1"}, "bad_request"},
		{fleet.PDUInput{Name: "A", Driver: "snmp", Address: "10.0.9.1", Community: "x", OIDPrefix: "1.3.x"}, "bad_request"},
		{fleet.PDUInput{Name: "A", Driver: "mock", Address: "10.0.9.1"}, "bad_request"}, // 不允许 mock 时
	} {
		if _, err := svc.CreatePDU(ctx, ops, c.in); codeOf(err) != c.want {
			t.Fatalf("CreatePDU(%+v) = %v, want %s", c.in, err, c.want)
		}
	}
	p, err := svc.CreatePDU(ctx, ops, snmpInput("PDU-A01"))
	if err != nil {
		t.Fatal(err)
	}
	if p.OIDPrefix != pdu.DefaultOIDPrefix || p.CycleValue != 3 || p.OnValue != 1 || !p.HasCommunity {
		t.Fatalf("pdu = %+v", p)
	}
	if _, err := svc.CreatePDU(ctx, ops, snmpInput("PDU-A01")); codeOf(err) != "duplicate_name" {
		t.Fatalf("duplicate = %v", err)
	}
	var enc []byte
	_ = pool.QueryRow(ctx, `SELECT community_enc FROM pdus WHERE id = $1`, p.ID).Scan(&enc)
	if len(enc) == 0 || strings.Contains(string(enc), "s3cret-community") {
		t.Fatal("community must be stored encrypted")
	}
	list, err := svc.ListPDUs(ctx)
	if err != nil || len(list) != 1 || list[0].Name != "PDU-A01" {
		t.Fatalf("list = %+v, %v", list, err)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action = 'pdu.create' AND diff::text NOT LIKE '%s3cret%'`).Scan(&n)
	if n != 1 {
		t.Fatalf("pdu.create audit rows without secret = %d", n)
	}
}

func TestUpdatePDUKeepsCommunityWhenEmpty(t *testing.T) {
	svc, pool, _ := newEnv(t)
	_, conns := configurePDU(t, svc, true)
	ctx := context.Background()
	p, _ := svc.CreatePDU(ctx, ops, snmpInput("PDU-A01"))
	one := 1
	m, err := svc.Register(ctx, fleet.NewMachine{Serial: "SN1", LanIP: "10.0.1.1", ProductID: dbtest.ProductID(t, pool), PDUID: &p.ID, PDUOutlet: &one})
	if err != nil {
		t.Fatal(err)
	}
	in := snmpInput("PDU-A01-new")
	in.Community = ""
	in.CycleValue = 4
	if _, err := svc.UpdatePDU(ctx, ops, p.ID, in); err != nil {
		t.Fatal(err)
	}
	if err := svc.PowerCycle(ctx, ops, m.ID); err != nil {
		t.Fatal(err)
	}
	in.Community = "rotated"
	if _, err := svc.UpdatePDU(ctx, ops, p.ID, in); err != nil {
		t.Fatal(err)
	}
	if err := svc.PowerCycle(ctx, ops, m.ID); err != nil {
		t.Fatal(err)
	}
	c := *conns
	if len(c) != 2 || c[0].Community != "s3cret-community" || c[0].CycleValue != 4 || c[1].Community != "rotated" {
		t.Fatalf("conns = %+v", c)
	}
	if _, err := svc.UpdatePDU(ctx, ops, 9999, in); codeOf(err) != "not_found" {
		t.Fatalf("update missing = %v", err)
	}
}

func TestRegisterWithPDU(t *testing.T) {
	svc, pool, _ := newEnv(t)
	configurePDU(t, svc, true)
	ctx := context.Background()
	pid := dbtest.ProductID(t, pool)
	p, _ := svc.CreatePDU(ctx, ops, snmpInput("PDU-A01"))
	three, missing := 3, int64(9999)
	if _, err := svc.Register(ctx, fleet.NewMachine{Serial: "SN1", LanIP: "10.0.1.1", ProductID: pid, PDUOutlet: &three}); codeOf(err) != "bad_request" {
		t.Fatalf("outlet without pdu = %v", err)
	}
	if _, err := svc.Register(ctx, fleet.NewMachine{Serial: "SN1", LanIP: "10.0.1.1", ProductID: pid, PDUID: &missing, PDUOutlet: &three}); codeOf(err) != "bad_request" {
		t.Fatalf("unknown pdu = %v", err)
	}
	m, err := svc.Register(ctx, fleet.NewMachine{Serial: "SN1", LanIP: "10.0.1.1", ProductID: pid, PDUID: &p.ID, PDUOutlet: &three})
	if err != nil || m.PDUID == nil || *m.PDUID != p.ID || *m.PDUOutlet != 3 {
		t.Fatalf("machine = %+v, %v", m, err)
	}
	if _, err := svc.Register(ctx, fleet.NewMachine{Serial: "SN2", LanIP: "10.0.1.2", ProductID: pid, PDUID: &p.ID, PDUOutlet: &three}); codeOf(err) != "pdu_outlet_taken" {
		t.Fatalf("taken outlet = %v", err)
	}
}

func TestPowerCycle(t *testing.T) {
	svc, pool, _ := newEnv(t)
	m, _ := configurePDU(t, svc, true)
	ctx := context.Background()
	pid := dbtest.ProductID(t, pool)
	p, _ := svc.CreatePDU(ctx, ops, snmpInput("PDU-A01"))
	five := 5
	withPDU, _ := svc.Register(ctx, fleet.NewMachine{Serial: "SN1", LanIP: "10.0.1.1", ProductID: pid, PDUID: &p.ID, PDUOutlet: &five})
	bare, _ := svc.Register(ctx, fleet.NewMachine{Serial: "SN2", LanIP: "10.0.1.2", ProductID: pid})

	if err := svc.PowerCycle(ctx, ops, bare.ID); codeOf(err) != "bad_request" {
		t.Fatalf("machine without pdu = %v", err)
	}
	if err := svc.PowerCycle(ctx, ops, 9999); codeOf(err) != "not_found" {
		t.Fatalf("unknown machine = %v", err)
	}
	if err := svc.PowerCycle(ctx, ops, withPDU.ID); err != nil {
		t.Fatal(err)
	}
	if calls := m.Calls(); len(calls) != 1 || calls[0] != "cycle:5" {
		t.Fatalf("calls = %v", calls)
	}
	m.Fail = errors.New("timeout")
	if err := svc.PowerCycle(ctx, ops, withPDU.ID); codeOf(err) != "pdu_failed" {
		t.Fatalf("driver failure = %v", err)
	}
	var okN, failN int
	_ = pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE diff->>'ok' = 'true'), count(*) FILTER (WHERE diff->>'ok' = 'false')
		FROM audit_logs WHERE action = 'machine.power_cycle' AND actor_id = 3`).Scan(&okN, &failN)
	if okN != 1 || failN != 1 {
		t.Fatalf("audit ok=%d fail=%d", okN, failN)
	}
}

func TestPowerCycleRejectsMockPDUWhenNotAllowed(t *testing.T) {
	svc, pool, _ := newEnv(t)
	configurePDU(t, svc, true)
	ctx := context.Background()
	p, err := svc.CreatePDU(ctx, ops, fleet.PDUInput{Name: "dev", Driver: "mock", Address: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	one := 1
	m, _ := svc.Register(ctx, fleet.NewMachine{Serial: "SN1", LanIP: "10.0.1.1", ProductID: dbtest.ProductID(t, pool), PDUID: &p.ID, PDUOutlet: &one})
	configurePDU(t, svc, false) // 换成生产配置
	if err := svc.PowerCycle(ctx, ops, m.ID); codeOf(err) != "bad_request" {
		t.Fatalf("mock pdu in prod = %v", err)
	}
}

func TestUpdatePDUMockToSNMPNeedsCommunity(t *testing.T) {
	svc, _, _ := newEnv(t)
	configurePDU(t, svc, true)
	ctx := context.Background()
	p, err := svc.CreatePDU(ctx, ops, fleet.PDUInput{Name: "dev", Driver: "mock", Address: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	in := snmpInput("dev")
	in.Community = ""
	if _, err := svc.UpdatePDU(ctx, ops, p.ID, in); codeOf(err) != "bad_request" {
		t.Fatalf("mock->snmp without community = %v", err)
	}
	in.Community = "now-set"
	got, err := svc.UpdatePDU(ctx, ops, p.ID, in)
	if err != nil || !got.HasCommunity || got.Driver != "snmp" {
		t.Fatalf("update = %+v, %v", got, err)
	}
}

func TestUpdatePDUAddressChangeNeedsCommunity(t *testing.T) {
	svc, _, _ := newEnv(t)
	configurePDU(t, svc, true)
	ctx := context.Background()
	p, err := svc.CreatePDU(ctx, ops, snmpInput("PDU-B01"))
	if err != nil {
		t.Fatal(err)
	}
	in := snmpInput("PDU-B01")
	in.Address = "10.0.200.99"
	in.Community = ""
	// 不能把已存的 community 发给新地址
	if _, err := svc.UpdatePDU(ctx, ops, p.ID, in); codeOf(err) != "bad_request" {
		t.Fatalf("address change without community = %v", err)
	}
	in.Community = "for-new-host"
	got, err := svc.UpdatePDU(ctx, ops, p.ID, in)
	if err != nil || got.Address != "10.0.200.99" {
		t.Fatalf("address change with community = %+v, %v", got, err)
	}
	in.Community = ""
	in.CycleValue = 7
	if _, err := svc.UpdatePDU(ctx, ops, p.ID, in); err != nil {
		t.Fatalf("same address without community = %v", err)
	}
}
