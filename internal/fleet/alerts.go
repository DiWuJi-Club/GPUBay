package fleet

import (
	"context"

	"spark-cloud/internal/platform/db"
)

type Alert struct {
	Level      string // warning / critical
	Kind       string
	MachineID  int64 // 0 表示与具体机器无关
	InstanceID string
	Message    string
}

func (s *Service) RaiseAlert(ctx context.Context, q db.Querier, a Alert) error {
	var machineID *int64
	if a.MachineID != 0 {
		machineID = &a.MachineID
	}
	_, err := q.Exec(ctx, `INSERT INTO alerts (level, kind, machine_id, instance_id, message, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)`, a.Level, a.Kind, machineID, a.InstanceID, a.Message, s.clock.Now())
	return err
}
