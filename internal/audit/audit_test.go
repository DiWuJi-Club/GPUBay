package audit_test

import (
	"testing"

	"spark-cloud/internal/audit"
)

func TestActorEntry(t *testing.T) {
	e := audit.Actor{AdminID: 7, IP: "10.0.0.1"}.Entry("machine.power_cycle", "machine", "12", map[string]int{"outlet": 3})
	if e.ActorType != "admin" || e.ActorID != 7 || e.IP != "10.0.0.1" || e.Action != "machine.power_cycle" ||
		e.TargetType != "machine" || e.TargetID != "12" || e.Diff == nil {
		t.Fatalf("entry = %+v", e)
	}
	if e := (audit.Actor{}).Entry("admin.create", "admin", "1", nil); e.ActorType != "system" || e.ActorID != 0 {
		t.Fatalf("system entry = %+v", e)
	}
}
