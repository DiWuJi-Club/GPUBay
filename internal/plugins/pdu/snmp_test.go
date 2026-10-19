package pdu_test

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/gosnmp/gosnmp"

	"spark-cloud/internal/plugins/pdu"
)

// agent 是本地 UDP 上的极简 SNMP v2c 代理：只接受指定 community，Get 返回当前值，Set 写入并回显。
type agent struct {
	addr      string
	community string
	mu        sync.Mutex
	values    map[string]int
	sets      []string
}

func startAgent(t *testing.T, community string, values map[string]int) *agent {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	a := &agent{addr: pc.LocalAddr().String(), community: community, values: values}
	go func() {
		buf := make([]byte, 65535)
		dec := &gosnmp.GoSNMP{}
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			p, err := dec.SnmpDecodePacket(buf[:n])
			if err != nil || p.Community != a.community {
				continue // community 错误时不应答，客户端超时
			}
			resp := *p
			resp.PDUType = gosnmp.GetResponse
			var vars []gosnmp.SnmpPDU
			a.mu.Lock()
			for _, v := range p.Variables {
				cur, ok := a.values[v.Name]
				if p.PDUType == gosnmp.SetRequest && ok {
					cur = v.Value.(int)
					a.values[v.Name] = cur
					a.sets = append(a.sets, v.Name)
				}
				if ok {
					vars = append(vars, gosnmp.SnmpPDU{Name: v.Name, Type: gosnmp.Integer, Value: cur})
				} else {
					vars = append(vars, gosnmp.SnmpPDU{Name: v.Name, Type: gosnmp.NoSuchInstance})
				}
			}
			a.mu.Unlock()
			resp.Variables = vars
			if b, err := resp.MarshalMsg(); err == nil {
				_, _ = pc.WriteTo(b, from)
			}
		}
	}()
	return a
}

func TestSNMPPowerCycleAndStatus(t *testing.T) {
	oid := ".1.3.6.1.4.1.318.1.1.4.4.2.1.3.5"
	a := startAgent(t, "private", map[string]int{oid: 1})
	d := pdu.NewSNMP(pdu.SNMPConfig{Address: a.addr, Community: "private", Timeout: 500 * time.Millisecond})
	ctx := context.Background()
	on, err := d.Status(ctx, 5)
	if err != nil || !on {
		t.Fatalf("status = %v, %v", on, err)
	}
	if err := d.PowerCycle(ctx, 5); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	got, sets := a.values[oid], append([]string(nil), a.sets...)
	a.mu.Unlock()
	if got != pdu.DefaultCycleValue || len(sets) != 1 || sets[0] != oid {
		t.Fatalf("value = %d sets = %v", got, sets)
	}
	if on, err := d.Status(ctx, 5); err != nil || on {
		t.Fatalf("status after cycle command = %v, %v (value 3 is not 'on')", on, err)
	}
	if _, err := d.Status(ctx, 9); err == nil {
		t.Fatal("unknown outlet must fail")
	}
	if err := d.PowerCycle(ctx, 0); err == nil {
		t.Fatal("outlet 0 must be rejected")
	}
}

func TestSNMPCustomOIDAndValues(t *testing.T) {
	oid := ".1.3.6.1.4.1.99999.1.2.7"
	a := startAgent(t, "c0mm", map[string]int{oid: 2})
	d := pdu.NewSNMP(pdu.SNMPConfig{Address: a.addr, Community: "c0mm", OIDPrefix: "1.3.6.1.4.1.99999.1.2", CycleValue: 4, OnValue: 2,
		Timeout: 500 * time.Millisecond})
	if on, err := d.Status(context.Background(), 7); err != nil || !on {
		t.Fatalf("status = %v, %v", on, err)
	}
	if err := d.PowerCycle(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.values[oid] != 4 {
		t.Fatalf("value = %d, want 4", a.values[oid])
	}
}

func TestSNMPWrongCommunityTimesOut(t *testing.T) {
	a := startAgent(t, "private", map[string]int{".1.3.6.1.4.1.318.1.1.4.4.2.1.3.1": 1})
	d := pdu.NewSNMP(pdu.SNMPConfig{Address: a.addr, Community: "public", Timeout: 200 * time.Millisecond})
	if err := d.PowerCycle(context.Background(), 1); err == nil {
		t.Fatal("wrong community must fail")
	}
}

func TestMockRecordsCalls(t *testing.T) {
	m := &pdu.Mock{}
	if err := m.PowerCycle(context.Background(), 3); err != nil {
		t.Fatal(err)
	}
	if calls := m.Calls(); len(calls) != 1 || calls[0] != "cycle:3" {
		t.Fatalf("calls = %v", calls)
	}
	if on, err := m.Status(context.Background(), 3); err != nil || !on {
		t.Fatalf("status = %v, %v", on, err)
	}
}
