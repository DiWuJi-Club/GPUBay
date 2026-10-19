package fleet_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/dbtest"
)

func TestCommandLifecycleIsIdempotent(t *testing.T) {
	svc, pool, _ := newEnv(t)
	ctx := context.Background()
	mid := dbtest.CreateMachine(t, pool, "SN001")

	id, err := svc.Enqueue(ctx, pool, mid, "ins-abc", fleet.CmdCreateInstance, fleet.CreateInstancePayload{ImageRef: "img:1"})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := svc.PendingCommands(ctx, mid)
	if err != nil || len(pending) != 1 || pending[0].ID != id || pending[0].Type != fleet.CmdCreateInstance {
		t.Fatalf("pending = %+v, %v", pending, err)
	}
	var p fleet.CreateInstancePayload
	if err := json.Unmarshal(pending[0].Payload, &p); err != nil || p.ImageRef != "img:1" {
		t.Fatalf("payload = %s, %v", pending[0].Payload, err)
	}

	cmd, applied, err := svc.CompleteCommand(ctx, pool, id, true, "ok")
	if err != nil || !applied || cmd.Status != "done" || cmd.InstanceID != "ins-abc" {
		t.Fatalf("complete = %+v applied=%v err=%v", cmd, applied, err)
	}
	cmd, applied, err = svc.CompleteCommand(ctx, pool, id, false, "late failure")
	if err != nil || applied || cmd.Status != "done" {
		t.Fatalf("second complete = %+v applied=%v err=%v", cmd, applied, err)
	}
	if pending, _ := svc.PendingCommands(ctx, mid); len(pending) != 0 {
		t.Fatalf("pending after done = %+v", pending)
	}
}

func TestCancelCommands(t *testing.T) {
	svc, pool, _ := newEnv(t)
	ctx := context.Background()
	mid := dbtest.CreateMachine(t, pool, "SN001")
	id, _ := svc.Enqueue(ctx, pool, mid, "ins-abc", fleet.CmdStartInstance, nil)
	_, _ = svc.Enqueue(ctx, pool, mid, "ins-other", fleet.CmdStopInstance, nil)

	if err := svc.CancelCommands(ctx, pool, "ins-abc", "superseded"); err != nil {
		t.Fatal(err)
	}
	pending, _ := svc.PendingCommands(ctx, mid)
	if len(pending) != 1 || pending[0].InstanceID != "ins-other" {
		t.Fatalf("pending = %+v", pending)
	}
	if _, applied, _ := svc.CompleteCommand(ctx, pool, id, true, "ok"); applied {
		t.Fatal("cancelled command must not be applied")
	}
}

func TestHasOpenCommand(t *testing.T) {
	svc, pool, _ := newEnv(t)
	ctx := context.Background()
	mid := dbtest.CreateMachine(t, pool, "SN001")
	id, _ := svc.Enqueue(ctx, pool, mid, "ins-abc", fleet.CmdStartInstance, nil)
	if ok, err := svc.HasOpenCommand(ctx, pool, "ins-abc", fleet.CmdStartInstance); err != nil || !ok {
		t.Fatalf("open = %v, %v", ok, err)
	}
	if ok, _ := svc.HasOpenCommand(ctx, pool, "ins-abc", fleet.CmdStopInstance); ok {
		t.Fatal("other type must not match")
	}
	if _, _, err := svc.CompleteCommand(ctx, pool, id, false, "boom"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := svc.HasOpenCommand(ctx, pool, "ins-abc", fleet.CmdStartInstance); ok {
		t.Fatal("completed command must not be open")
	}
}

func TestCommandInstanceID(t *testing.T) {
	svc, pool, _ := newEnv(t)
	ctx := context.Background()
	mid := dbtest.CreateMachine(t, pool, "SN001")
	id, _ := svc.Enqueue(ctx, pool, mid, "ins-abc", fleet.CmdStartInstance, nil)
	if got, err := svc.CommandInstanceID(ctx, pool, id); err != nil || got != "ins-abc" {
		t.Fatalf("CommandInstanceID = %q, %v", got, err)
	}
	if _, err := svc.CommandInstanceID(ctx, pool, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("unknown id = %v, want not found", err)
	}
}
