package appdeploy

import (
	"context"
	"time"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/rental"
)

// registerPorts 部署进入 running 时登记 spec 声明的端口（规格 §7；调用方持部署行锁）：
// 已有行且无主（换机复制来的 / 用户手动加的）→ 认领并把备注改成应用名；已是本部署的 → 不动；
// 不存在 → 插入私有端口。属于别的部署 → errCode = port_conflict；登记后总数 > 5 → errCode = port_limit。
// 先整体检查再写，errCode 非空时不写任何行（调用方据此 markFailed）。
func registerPorts(ctx context.Context, q db.Querier, d Deployment, ports []PortSpec, appName string, now time.Time) (errCode string, err error) {
	rows, err := q.Query(ctx, `SELECT port, deployment_id FROM instance_ports WHERE instance_id = $1`, d.InstanceID)
	if err != nil {
		return "", err
	}
	owner := map[int]*string{}
	for rows.Next() {
		var port int
		var dep *string
		if err := rows.Scan(&port, &dep); err != nil {
			rows.Close()
			return "", err
		}
		owner[port] = dep
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", err
	}
	added := 0
	for _, p := range ports {
		dep, exists := owner[p.Port]
		switch {
		case !exists:
			added++
		case dep != nil && *dep != d.ID:
			return ErrCodePortConflict, nil
		}
	}
	if len(owner)+added > rental.MaxPortsPerInstance {
		return ErrCodePortLimit, nil
	}
	for _, p := range ports {
		tag, err := q.Exec(ctx, `UPDATE instance_ports SET note = CASE WHEN deployment_id IS NULL THEN $4 ELSE note END, deployment_id = $3,
			updated_at = CASE WHEN deployment_id IS NULL THEN $5 ELSE updated_at END
			WHERE instance_id = $1 AND port = $2 AND (deployment_id IS NULL OR deployment_id = $3)`, d.InstanceID, p.Port, d.ID, appName, now)
		if err != nil {
			return "", err
		}
		if tag.RowsAffected() > 0 {
			continue
		}
		// 检查之后才出现的行（并发的用户加端口）：ON CONFLICT 不让事务因唯一键失败而中止
		tag, err = q.Exec(ctx, `INSERT INTO instance_ports (instance_id, port, visibility, note, deployment_id, created_at, updated_at)
			VALUES ($1, $2, 'private', $4, $3, $5, $5) ON CONFLICT (instance_id, port) DO NOTHING`, d.InstanceID, p.Port, d.ID, appName, now)
		if err != nil {
			return "", err
		}
		if tag.RowsAffected() == 0 {
			return ErrCodePortConflict, nil
		}
	}
	return "", nil
}

// deletePorts 卸载时删除部署登记的全部端口；每删一个以 actor（部署主人）审计 instance.port_delete，原因「卸载应用 <name>」。
func deletePorts(ctx context.Context, q db.Querier, d Deployment, appName string, now time.Time, actor audit.Actor) error {
	return deletePortsBecause(ctx, q, d, "卸载应用 "+appName, now, actor)
}

func deletePortsBecause(ctx context.Context, q db.Querier, d Deployment, reason string, now time.Time, actor audit.Actor) error {
	rows, err := q.Query(ctx, `DELETE FROM instance_ports WHERE deployment_id = $1 RETURNING port`, d.ID)
	if err != nil {
		return err
	}
	var ports []int
	for rows.Next() {
		var p int
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return err
		}
		ports = append(ports, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, p := range ports {
		if err := audit.Record(ctx, q, now, actor.Entry("instance.port_delete", "instance", d.InstanceID,
			map[string]any{"port": p, "deployment": d.ID, "reason": reason})); err != nil {
			return err
		}
	}
	return nil
}

// PortURLFor 返回部署主端口的访问 URL：主端口行存在（用户没删）且配置了 PortURL 时非空。
func (s *Service) PortURLFor(ctx context.Context, d Deployment) (string, error) {
	if s.d.PortURL == nil {
		return "", nil
	}
	var exists bool
	if err := s.d.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM instance_ports WHERE instance_id = $1 AND port = $2)`, d.InstanceID, d.Port).Scan(&exists); err != nil {
		return "", err
	}
	if !exists {
		return "", nil
	}
	return s.d.PortURL(d.InstanceID, d.Port), nil
}
