package rental

import (
	"context"
	"strconv"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
)

// ReservedCountsByGroup 返回每个机组未开通预订（reserved）的实例数；下单时未选机组的不计。
func (s *Service) ReservedCountsByGroup(ctx context.Context) (map[int64]int, error) {
	rows, err := s.d.Pool.Query(ctx, `SELECT group_id, count(*) FROM instances WHERE state = 'reserved' AND group_id IS NOT NULL GROUP BY group_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int{}
	for rows.Next() {
		var gid int64
		var n int
		if err := rows.Scan(&gid, &n); err != nil {
			return nil, err
		}
		out[gid] = n
	}
	return out, rows.Err()
}

// AdminDeleteGroup 删除机组：组内不能有机器、不能有未终态实例引用（下单时选的机组）；终态实例的引用置 NULL。
func (s *Service) AdminDeleteGroup(ctx context.Context, actor audit.Actor, id int64) error {
	now := s.d.Clock.Now()
	return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		var busy bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM instances WHERE group_id = $1
			AND state IN ('reserved', 'creating', 'running', 'stopped', 'wiping'))`, id).Scan(&busy); err != nil {
			return err
		}
		if busy {
			return apperr.ErrGroupNotEmpty
		}
		if _, err := tx.Exec(ctx, `UPDATE instances SET group_id = NULL, updated_at = $2 WHERE group_id = $1`, id, now); err != nil {
			return err
		}
		if err := s.d.Fleet.DeleteGroupIn(ctx, tx, id); err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("group.delete", "group", strconv.FormatInt(id, 10), nil))
	})
}
