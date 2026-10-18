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

func TestActorEntryResolvesUserAndSystem(t *testing.T) {
	cases := []struct {
		actor    audit.Actor
		wantType string
		wantID   int64
	}{
		{audit.Actor{AdminID: 7, IP: "1.1.1.1"}, "admin", 7},
		{audit.Actor{UserID: 42, IP: "2.2.2.2"}, "user", 42},
		{audit.Actor{AdminID: 7, UserID: 42}, "admin", 7},
		{audit.Actor{}, "system", 0},
	}
	for _, c := range cases {
		e := c.actor.Entry("x.y", "instance", "ins-1", nil)
		if e.ActorType != c.wantType || e.ActorID != c.wantID || e.IP != c.actor.IP || e.Action != "x.y" {
			t.Fatalf("%+v → %+v", c.actor, e)
		}
	}
}
