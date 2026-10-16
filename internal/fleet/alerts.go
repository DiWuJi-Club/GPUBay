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

// RaiseAlertOnce 与 RaiseAlert 相同，但已存在同 kind、同内容且未处理的告警时不再新增（避免重试导致刷屏）。
func (s *Service) RaiseAlertOnce(ctx context.Context, q db.Querier, a Alert) error {
	var exists bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM alerts WHERE kind = $1 AND message = $2 AND resolved_at IS NULL)`,
		a.Kind, a.Message).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	return s.RaiseAlert(ctx, q, a)
}
