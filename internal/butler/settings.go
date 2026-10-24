package butler

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
)

const settingsCols = `enabled, daily_token_limit, max_turns, notice, updated_at, updated_by`

// settingsDiff 是审计 butler.settings 里新旧值的形态。
type settingsDiff struct {
	Enabled         bool   `json:"enabled"`
	DailyTokenLimit int64  `json:"daily_token_limit"`
	MaxTurns        int    `json:"max_turns"`
	Notice          string `json:"notice"`
}

func diffOf(v Settings) settingsDiff {
	return settingsDiff{Enabled: v.Enabled, DailyTokenLimit: v.DailyTokenLimit, MaxTurns: v.MaxTurns, Notice: v.Notice}
}

// loadSettings 读唯一一行设置；行不存在（不应发生）时返回默认值，不让管家因此整体不可用。
func loadSettings(ctx context.Context, q db.Querier, lock bool) (Settings, error) {
	sql := `SELECT ` + settingsCols + ` FROM butler_settings WHERE id = 1`
	if lock {
		sql += ` FOR UPDATE`
	}
	var v Settings
	err := q.QueryRow(ctx, sql).Scan(&v.Enabled, &v.DailyTokenLimit, &v.MaxTurns, &v.Notice, &v.UpdatedAt, &v.UpdatedBy)
	if db.IsNoRows(err) {
		return DefaultSettings(), nil
	}
	return v, err
}

// Settings 返回当前设置，带 SettingsCacheTTL（10 秒）的进程内缓存。
func (s *Store) Settings(ctx context.Context) (Settings, error) {
	now := s.clk.Now()
	s.setMu.Lock()
	if s.setOK {
		if age := now.Sub(s.setAt); age >= 0 && age < SettingsCacheTTL {
			v := s.setVal
			s.setMu.Unlock()
			return v, nil
		}
	}
	gen := s.setGen
	s.setMu.Unlock()

	v, err := loadSettings(ctx, s.pool, false)
	if err != nil {
		return Settings{}, err
	}
	s.setMu.Lock()
	// 读库期间若有失效 / 更新，不用这次读到的（可能更旧的）值覆盖缓存
	if s.setGen == gen {
		s.setVal, s.setAt, s.setOK = v, now, true
	}
	s.setMu.Unlock()
	return v, nil
}

// InvalidateSettings 清空本副本的设置缓存，下次 Settings 直接读库。
func (s *Store) InvalidateSettings() {
	s.setMu.Lock()
	s.setOK = false
	s.setGen++
	s.setMu.Unlock()
}

// validateSettings 校验后台提交的设置（与迁移的 CHECK 一致）；notice 去首尾空白后不超过 200 字。
func validateSettings(in Settings) (Settings, error) {
	in.Notice = strings.TrimSpace(in.Notice)
	switch {
	case in.DailyTokenLimit < MinDailyTokenLimit || in.DailyTokenLimit > MaxDailyTokenLimit:
		return in, apperr.BadRequest(fmt.Sprintf("每用户每日 token 限额需在 %d～%d 之间", MinDailyTokenLimit, MaxDailyTokenLimit))
	case in.MaxTurns < MinMaxTurns || in.MaxTurns > MaxMaxTurns:
		return in, apperr.BadRequest(fmt.Sprintf("每对话消息数上限需在 %d～%d 之间", MinMaxTurns, MaxMaxTurns))
	case utf8.RuneCountInString(in.Notice) > MaxNoticeRunes:
		return in, apperr.BadRequest(fmt.Sprintf("关闭说明不能超过 %d 字", MaxNoticeRunes))
	case !utf8.ValidString(in.Notice) || strings.ContainsRune(in.Notice, 0):
		return in, apperr.BadRequest("关闭说明含非法字符")
	}
	return in, nil
}

// UpdateSettings 校验并保存后台设置（enabled / daily_token_limit / max_turns / notice），
// 同一事务写审计 butler.settings（diff {old, new}）；成功后直接刷新本副本缓存。
func (s *Store) UpdateSettings(ctx context.Context, actor audit.Actor, in Settings) (Settings, error) {
	in, err := validateSettings(in)
	if err != nil {
		return Settings{}, err
	}
	now := s.clk.Now()
	var by *int64
	if actor.AdminID != 0 {
		by = &actor.AdminID
	}
	var v Settings
	err = s.withTx(ctx, func(tx pgx.Tx) error {
		old, err := loadSettings(ctx, tx, true)
		if err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `INSERT INTO butler_settings (id, enabled, daily_token_limit, max_turns, notice, updated_at, updated_by)
			VALUES (1, $1, $2, $3, $4, $5, $6)
			ON CONFLICT (id) DO UPDATE SET enabled = EXCLUDED.enabled, daily_token_limit = EXCLUDED.daily_token_limit,
				max_turns = EXCLUDED.max_turns, notice = EXCLUDED.notice, updated_at = EXCLUDED.updated_at, updated_by = EXCLUDED.updated_by
			RETURNING `+settingsCols,
			in.Enabled, in.DailyTokenLimit, in.MaxTurns, in.Notice, now, by).
			Scan(&v.Enabled, &v.DailyTokenLimit, &v.MaxTurns, &v.Notice, &v.UpdatedAt, &v.UpdatedBy)
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry(AuditSettings, "butler_settings", "1",
			map[string]any{"old": diffOf(old), "new": diffOf(v)}))
	})
	if err != nil {
		return Settings{}, err
	}
	s.setMu.Lock()
	s.setVal, s.setAt, s.setOK = v, now, true
	s.setGen++
	s.setMu.Unlock()
	return v, nil
}
