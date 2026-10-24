package rental_test

import (
	"encoding/json"
	"testing"
	"time"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/rental"
)

func TestInstanceSSHPort(t *testing.T) {
	for _, c := range []struct{ base, want int }{{2222, 2222}, {2300, 2300}, {0, 2222}, {-1, 2222}} {
		if got := (rental.Instance{}).SSHPort(c.base); got != c.want {
			t.Fatalf("SSHPort(%d) = %d, want %d", c.base, got, c.want)
		}
	}
}

func TestServiceInstanceSSHPort(t *testing.T) {
	if got := rental.NewService(rental.Deps{SSHBasePort: testSSHBasePort}).InstanceSSHPort("ins-abcdefgh"); got != testSSHBasePort {
		t.Fatalf("InstanceSSHPort = %d, want %d", got, testSSHBasePort)
	}
	if got := rental.NewService(rental.Deps{}).InstanceSSHPort("ins-abcdefgh"); got != rental.DefaultSSHPort {
		t.Fatalf("InstanceSSHPort (no base) = %d, want %d", got, rental.DefaultSSHPort)
	}
}

// createPayloadOn 返回机器上唯一一条 Create 指令的原始 payload（解析成 map，检查线上字段名）。
func createPayloadOn(e *env, machineID int64) map[string]any {
	e.t.Helper()
	cmds, err := e.fleet.PendingCommands(e.ctx, machineID)
	if err != nil {
		e.t.Fatal(err)
	}
	var out map[string]any
	for _, c := range cmds {
		if c.Type != fleet.CmdCreateInstance {
			continue
		}
		if out != nil {
			e.t.Fatalf("more than one create command: %+v", cmds)
		}
		if err := json.Unmarshal(c.Payload, &out); err != nil {
			e.t.Fatal(err)
		}
	}
	if out == nil {
		e.t.Fatalf("no create command: %+v", cmds)
	}
	return out
}

func checkCreatePayload(t *testing.T, p map[string]any) {
	t.Helper()
	if p["ssh_port"] != float64(testSSHBasePort) || p["share_slots"] != float64(1) || p["image_ref"] == "" {
		t.Fatalf("create payload = %v, want ssh_port %d share_slots 1", p, testSSHBasePort)
	}
}

func TestCreatePayloadCarriesSSHPortImmediate(t *testing.T) {
	e := newEnv(t, 1)
	e.place(catalog.D1d)
	checkCreatePayload(t, createPayloadOn(e, e.machines[0]))
}

func TestCreatePayloadCarriesSSHPortBooking(t *testing.T) {
	e := newEnv(t, 1)
	e.book(dbtest.T0.Add(3 * time.Hour))
	e.clock.Advance(2 * time.Hour)
	e.heartbeatAll()
	n, err := e.svc.ProvisionDue(e.ctx)
	mustCount(t, n, err, 1, "provision at T-1h")
	checkCreatePayload(t, createPayloadOn(e, e.machines[0]))
}
