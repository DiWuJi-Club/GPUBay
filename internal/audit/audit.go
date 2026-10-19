// Package audit 记录管理员与系统的关键操作。
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"spark-cloud/internal/platform/db"
)

type Entry struct {
	ActorType  string // admin / user / system
	ActorID    int64
	Action     string
	TargetType string
	TargetID   string
	Diff       any
	IP         string
}

func Record(ctx context.Context, q db.Querier, at time.Time, e Entry) error {
	diff := []byte("{}")
	if e.Diff != nil {
		b, err := json.Marshal(e.Diff)
		if err != nil {
			return fmt.Errorf("audit diff: %w", err)
		}
		diff = b
	}
	_, err := q.Exec(ctx, `INSERT INTO audit_logs (actor_type, actor_id, action, target_type, target_id, diff, ip, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		e.ActorType, e.ActorID, e.Action, e.TargetType, e.TargetID, diff, e.IP, at)
	return err
}

// Actor 标识发起后台操作的管理员与来源 IP；AdminID 为 0 表示命令行等系统操作。
type Actor struct {
	AdminID int64
	IP      string
}

// Entry 生成一条以该操作人为主体的审计记录。
func (a Actor) Entry(action, targetType, targetID string, diff any) Entry {
	actorType := "admin"
	if a.AdminID == 0 {
		actorType = "system"
	}
	return Entry{ActorType: actorType, ActorID: a.AdminID, Action: action, TargetType: targetType, TargetID: targetID, Diff: diff, IP: a.IP}
}
