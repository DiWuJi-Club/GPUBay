package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/paging"
)

type Log struct {
	ID         int64           `json:"id"`
	ActorType  string          `json:"actor_type"`
	ActorID    int64           `json:"actor_id"`
	Action     string          `json:"action"`
	TargetType string          `json:"target_type"`
	TargetID   string          `json:"target_id"`
	Diff       json.RawMessage `json:"diff"`
	IP         string          `json:"ip"`
	CreatedAt  time.Time       `json:"created_at"`
}

type Filter struct {
	ActorID    int64
	Action     string
	TargetType string
	TargetID   string
	From       *time.Time
	To         *time.Time // 不含
}

// List 按时间倒序分页查询审计日志（只读；审计日志没有修改与删除接口）。
func List(ctx context.Context, q db.Querier, f Filter, p paging.Page) ([]Log, int, error) {
	var w db.Where
	if f.ActorID > 0 {
		w.Add("actor_id = ?", f.ActorID)
	}
	if f.Action != "" {
		w.Add("action = ?", f.Action)
	}
	if f.TargetType != "" {
		w.Add("target_type = ?", f.TargetType)
	}
	if f.TargetID != "" {
		w.Add("target_id = ?", f.TargetID)
	}
	if f.From != nil {
		w.Add("created_at >= ?", *f.From)
	}
	if f.To != nil {
		w.Add("created_at < ?", *f.To)
	}
	var total int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE `+w.SQL(), w.Args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	n := len(w.Args)
	rows, err := q.Query(ctx, `SELECT id, actor_type, actor_id, action, target_type, target_id, diff, ip, created_at
		FROM audit_logs WHERE `+w.SQL()+fmt.Sprintf(` ORDER BY created_at DESC, id DESC LIMIT $%d OFFSET $%d`, n+1, n+2),
		append(w.Args, p.Limit(), p.Offset())...)
	if err != nil {
		return nil, 0, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Log, error) {
		var l Log
		var diff []byte
		err := r.Scan(&l.ID, &l.ActorType, &l.ActorID, &l.Action, &l.TargetType, &l.TargetID, &diff, &l.IP, &l.CreatedAt)
		l.Diff = diff
		return l, err
	})
	return list, total, err
}
