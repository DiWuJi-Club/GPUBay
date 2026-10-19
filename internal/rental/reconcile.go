package rental

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
)

// InstanceIDPattern 是平台生成的实例 ID 形态（randx.ID("ins-", 8)）；Agent 上报的其他字符串一律不处理。
var InstanceIDPattern = regexp.MustCompile(`^ins-[a-z0-9]{8}$`)

// Reconcile 按设计 §3.2 的对账表处理一份 StateReport。每个动作在独立事务内执行；
// 有实例行的先 lockInstance，以锁内读到的状态重新判定；只有实例没有任何未完成指令时才入队 Stop / Wipe。
// 恢复计时不在这里：网关在本函数之后用 report.RunningIDs() 调 ResumeMachine。
func (s *Service) Reconcile(ctx context.Context, machineID int64, report fleet.StateReport) error {
	m, err := s.d.Fleet.Get(ctx, s.d.Pool, machineID)
	if err != nil {
		return err
	}
	var errs []error
	reported := map[string]bool{}
	invalid := false
	for _, st := range report.Instances {
		if !InstanceIDPattern.MatchString(st.InstanceID) {
			invalid = true
			continue
		}
		reported[st.InstanceID] = true
		if err := s.reconcileReported(ctx, m, st); err != nil {
			errs = append(errs, fmt.Errorf("instance %s: %w", st.InstanceID, err))
		}
	}
	if invalid {
		if err := s.d.Fleet.RaiseAlertOnce(ctx, s.d.Pool, fleet.Alert{Level: "warning", Kind: "invalid_report", MachineID: machineID,
			Message: AlertInvalidReport(m.Serial)}); err != nil {
			errs = append(errs, err)
		}
	}
	expected, err := collectInstances(s.d.Pool.Query(ctx, `SELECT `+instanceCols+` FROM instances
		WHERE machine_id = $1 AND state IN ('creating', 'running', 'stopped', 'wiping') ORDER BY id`, machineID))
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, inst := range expected {
		if reported[inst.ID] {
			continue
		}
		if err := s.reconcileMissing(ctx, m, inst.ID); err != nil {
			errs = append(errs, fmt.Errorf("instance %s: %w", inst.ID, err))
		}
	}
	return errors.Join(errs...)
}

func (s *Service) reconcileReported(ctx context.Context, m fleet.Machine, st fleet.InstanceState) error {
	return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		inst, err := lockInstance(ctx, tx, st.InstanceID)
		if errors.Is(err, apperr.ErrNotFound) {
			return s.wipeOrphan(ctx, tx, m, st.InstanceID)
		}
		if err != nil {
			return err
		}
		if inst.MachineID != m.ID || inst.State == StateReleased || inst.State == StateFailed {
			return s.wipeOrphan(ctx, tx, m, inst.ID)
		}
		if st.Phase == fleet.PhaseMissingData {
			return s.alertOnce(ctx, tx, "data_disk_missing", m.ID, inst.ID, AlertDataDiskMissing(m.Serial, inst.ID))
		}
		switch inst.State {
		case StateStopped:
			switch st.Phase {
			case fleet.PhaseRunning:
				return s.enqueueIfIdle(ctx, tx, m.ID, inst.ID, fleet.CmdStopInstance)
			case fleet.PhaseStopped, fleet.PhaseUnknown:
				return nil
			}
		case StateRunning:
			if st.Phase == fleet.PhaseRunning || st.Phase == fleet.PhaseUnknown || inst.Suspended() {
				return nil // running 的恢复计时与暂停中的告警都由 ResumeMachine 处理
			}
			return s.alertOnce(ctx, tx, "container_down", m.ID, inst.ID, AlertContainerDown(m.Serial, inst.ID))
		}
		return nil // creating / wiping：等指令结果
	})
}

// reconcileMissing 处理平台记录里有、Agent 没有上报的实例。
func (s *Service) reconcileMissing(ctx context.Context, m fleet.Machine, instanceID string) error {
	return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		inst, err := lockInstance(ctx, tx, instanceID)
		if err != nil {
			return err
		}
		switch inst.State {
		case StateRunning:
			if inst.Suspended() {
				return nil
			}
			return s.alertOnce(ctx, tx, "container_down", m.ID, inst.ID, AlertContainerDown(m.Serial, inst.ID))
		case StateStopped:
			return s.alertOnce(ctx, tx, "container_missing", m.ID, inst.ID, AlertContainerMissing(m.Serial, inst.ID))
		}
		return nil
	})
}

// wipeOrphan 对不属于本机当前实例的容器入队清理（instance_id 照填），并告警。
func (s *Service) wipeOrphan(ctx context.Context, tx pgx.Tx, m fleet.Machine, instanceID string) error {
	if err := s.enqueueIfIdle(ctx, tx, m.ID, instanceID, fleet.CmdWipeInstance); err != nil {
		return err
	}
	return s.alertOnce(ctx, tx, "orphan_container", m.ID, instanceID, AlertOrphanContainer(m.Serial, instanceID))
}

// enqueueIfIdle 只在实例没有任何未完成指令时入队，避免与刚入队的 start、仍在执行的 stop、上一轮的孤儿 wipe 打架。
func (s *Service) enqueueIfIdle(ctx context.Context, tx pgx.Tx, machineID int64, instanceID string, t fleet.CommandType) error {
	open, err := s.d.Fleet.HasAnyOpenCommand(ctx, tx, instanceID)
	if err != nil || open {
		return err
	}
	_, err = s.d.Fleet.Enqueue(ctx, tx, machineID, instanceID, t, nil)
	return err
}

func (s *Service) alertOnce(ctx context.Context, q db.Querier, kind string, machineID int64, instanceID, message string) error {
	level := "critical"
	if kind == "orphan_container" {
		level = "warning"
	}
	return s.d.Fleet.RaiseAlertOnce(ctx, q, fleet.Alert{Level: level, Kind: kind, MachineID: machineID, InstanceID: instanceID, Message: message})
}
