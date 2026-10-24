package llm

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/tz"
)

// API Key（规格 T5 / T6）：sk-spark- + 40 位 base62，只存 sha256 十六进制，展示 sk-spark-… + 末 4 位；明文只在创建时返回一次。

const (
	KeyPrefix        = "sk-spark-"
	keyRandLen       = 40
	keyHintPrefix    = "sk-spark-…"
	maxKeyNameLen    = 32
	maxKeyModelSlugs = 32
	maxMonthlyCap    = 100_000_000         // 分（¥100 万）：防止 cap × 10^6 溢出的宽松上限
	revokedListAge   = 30 * 24 * time.Hour // 控制台列表不再显示撤销超过 30 天的 Key
	maxAdminReason   = 200
)

const base62 = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// GenerateKey 生成一把新 Key：明文、哈希（hex(sha256(plain))）、展示用 hint。
func GenerateKey() (plain, hash, hint string) {
	out := make([]byte, 0, len(KeyPrefix)+keyRandLen)
	out = append(out, KeyPrefix...)
	buf := make([]byte, 64)
	for len(out) < cap(out) {
		if _, err := rand.Read(buf); err != nil {
			panic(err)
		}
		for _, b := range buf {
			// 拒绝采样：248 = 62 × 4，丢弃 ≥ 248 的字节保证均匀
			if b < 248 && len(out) < cap(out) {
				out = append(out, base62[b%62])
			}
		}
	}
	plain = string(out)
	return plain, HashKey(plain), keyHintPrefix + plain[len(plain)-4:]
}

// HashKey 明文 → sha256 十六进制（llm_api_keys.key_hash）。
func HashKey(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// MonthOf 北京时间所属月份（llm_api_keys.month_ym 格式，如 2026-10）。
func MonthOf(t time.Time) string { return t.In(tz.CST).Format("2006-01") }

// KeyAllowsModel model_slugs 为空 = 全部模型。
func (k Key) KeyAllowsModel(slug string) bool {
	return len(k.ModelSlugs) == 0 || slices.Contains(k.ModelSlugs, slug)
}

// CurrentMonthCostMicro 本月（北京时间）已计消费（微分）；month_ym 不是当月视为 0（跨月由记账重置）。
func (k Key) CurrentMonthCostMicro(now time.Time) int64 {
	if k.MonthYM != MonthOf(now) {
		return 0
	}
	return k.MonthCostMicro
}

// MonthCapExceeded 规格 §6.1 步骤 5：month_cost_micro / 10^6 ≥ monthly_cap（分）。无上限永不超。
func (k Key) MonthCapExceeded(now time.Time) bool {
	return k.MonthlyCap != nil && k.CurrentMonthCostMicro(now) >= *k.MonthlyCap*MicroPerFen
}

// KeyInput 控制台创建 Key 的输入。ModelSlugs 空 = 全部模型；MonthlyCap 单位分，nil = 不限；ExpiresAt nil = 永不过期。
type KeyInput struct {
	Name       string
	ModelSlugs []string
	MonthlyCap *int64
	ExpiresAt  *time.Time
}

// KeyUpdate 控制台修改 Key：nil 字段不改。ModelSlugs 指向空切片 = 改为全部模型。
// 月上限三态：MonthlyCap 非 nil 设为该值；ClearMonthlyCap 取消上限；两者都不给不改（同时给 → llm_key_invalid）。
// 到期时间创建后不可改（规格 §10.1）。
type KeyUpdate struct {
	Name            *string
	Status          *string // active / disabled
	ModelSlugs      *[]string
	MonthlyCap      *int64
	ClearMonthlyCap bool
}

const keyCols = `id, user_id, name, key_hash, key_hint, status, model_slugs, monthly_cap, month_ym, month_cost_micro, expires_at,
	last_used_at, created_at, updated_at, revoked_at`

func scanKey(row pgx.Row) (Key, error) {
	var k Key
	err := row.Scan(&k.ID, &k.UserID, &k.Name, &k.KeyHash, &k.KeyHint, &k.Status, &k.ModelSlugs, &k.MonthlyCap, &k.MonthYM,
		&k.MonthCostMicro, &k.ExpiresAt, &k.LastUsedAt, &k.CreatedAt, &k.UpdatedAt, &k.RevokedAt)
	if db.IsNoRows(err) {
		return Key{}, apperr.ErrLLMKeyNotFound
	}
	if k.ModelSlugs == nil {
		k.ModelSlugs = []string{}
	}
	return k, err
}

func collectKeys(rows pgx.Rows, err error) ([]Key, error) {
	if err != nil {
		return nil, err
	}
	keys, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Key, error) { return scanKey(r) })
	if keys == nil && err == nil {
		keys = []Key{}
	}
	return keys, err
}

func keyTarget(id int64) string { return strconv.FormatInt(id, 10) }

func normalizeKeyName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > maxKeyNameLen {
		return "", apperr.LLMKeyInvalid("name", "请填写名称（最多 32 个字符）")
	}
	return name, nil
}

func validateMonthlyCap(c *int64) error {
	if c != nil && (*c < 1 || *c > maxMonthlyCap) {
		return apperr.LLMKeyInvalid("monthly_cap", "月消费上限需在 ¥0.01～¥1,000,000 之间")
	}
	return nil
}

// normalizeKeySlugs trim + 去重，并要求每个 slug 是 online 或 offline 模型。
// draft 模型对用户不可见，按不存在处理；而只有 draft 模型能被删除，所以 Key 引用的模型永远存在。
func normalizeKeySlugs(ctx context.Context, q db.Querier, slugs []string) ([]string, error) {
	out := []string{}
	for _, s := range slugs {
		s = strings.TrimSpace(s)
		if s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	if len(out) > maxKeyModelSlugs {
		return nil, apperr.LLMKeyInvalid("model_slugs", "最多选择 32 个模型")
	}
	if len(out) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `SELECT slug FROM llm_models WHERE slug = ANY($1) AND status <> $2`, out, ModelDraft)
	if err != nil {
		return nil, err
	}
	found, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	for _, s := range out {
		if !slices.Contains(found, s) {
			return nil, apperr.LLMKeyInvalid("model_slugs", "模型 "+s+" 不存在")
		}
	}
	return out, nil
}

// CreateKey 控制台创建 Key：要求 CanRent（已实名、未封禁）；每用户 active + disabled 最多 MaxKeysPerUser 个（revoked 不计）。
// 首次创建时插 llm_accounts 行，并以它的行锁串行化同一用户的计数（与 RecordUsage 同序：llm_accounts → llm_api_keys）。
// 审计 llm.key.create（user actor，diff 是 Key 视图：只有 hint，没有明文与哈希）。明文只在这里返回一次。
func (s *Service) CreateKey(ctx context.Context, userID int64, in KeyInput, ip string) (string, Key, error) {
	if err := s.d.Users.CanRent(ctx, userID); err != nil {
		return "", Key{}, err
	}
	name, err := normalizeKeyName(in.Name)
	if err != nil {
		return "", Key{}, err
	}
	if err := validateMonthlyCap(in.MonthlyCap); err != nil {
		return "", Key{}, err
	}
	now := s.d.Clock.Now()
	if in.ExpiresAt != nil && !in.ExpiresAt.After(now) {
		return "", Key{}, apperr.LLMKeyInvalid("expires_at", "到期时间须晚于现在")
	}
	slugs, err := normalizeKeySlugs(ctx, s.d.Pool, in.ModelSlugs)
	if err != nil {
		return "", Key{}, err
	}
	plain, hash, hint := GenerateKey()
	var out Key
	err = db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO llm_accounts (user_id, created_at, updated_at) VALUES ($1, $2, $2)
			ON CONFLICT (user_id) DO NOTHING`, userID, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT 1 FROM llm_accounts WHERE user_id = $1 FOR UPDATE`, userID); err != nil {
			return err
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM llm_api_keys WHERE user_id = $1 AND status IN ($2, $3)`,
			userID, KeyActive, KeyDisabled).Scan(&n); err != nil {
			return err
		}
		if n >= MaxKeysPerUser {
			return apperr.ErrLLMKeyLimit
		}
		k, err := scanKey(tx.QueryRow(ctx, `INSERT INTO llm_api_keys (user_id, name, key_hash, key_hint, model_slugs, monthly_cap, expires_at,
			created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8) RETURNING `+keyCols,
			userID, name, hash, hint, slugs, in.MonthlyCap, in.ExpiresAt, now))
		if err != nil {
			return err
		}
		out = k
		return audit.Record(ctx, tx, now, audit.Actor{UserID: userID, IP: ip}.Entry("llm.key.create", "llm_api_key", keyTarget(k.ID), k))
	})
	if err != nil {
		return "", Key{}, err
	}
	s.InvalidateKey(hash) // 不缓存「不存在」，这里只是保险
	return plain, out, nil
}

// UpdateKey 控制台修改自己的 Key（名称 / active↔disabled / 模型 / 月上限）；revoked 不可改（invalid_state）。
// 审计 llm.key.update{before, after}；本进程缓存立即失效，其它进程最多 KeyCacheTTL 后生效。
// 注意：后台停用（AdminDisableKey）同样是 disabled，用户可以自己重新启用；要强制停用请封禁账户（llm_accounts.blocked）。
func (s *Service) UpdateKey(ctx context.Context, userID, id int64, in KeyUpdate, ip string) (Key, error) {
	if in.Name != nil {
		name, err := normalizeKeyName(*in.Name)
		if err != nil {
			return Key{}, err
		}
		in.Name = &name
	}
	if in.Status != nil && *in.Status != KeyActive && *in.Status != KeyDisabled {
		return Key{}, apperr.LLMKeyInvalid("status", "只能是 active 或 disabled（撤销请删除）")
	}
	if in.MonthlyCap != nil && in.ClearMonthlyCap {
		return Key{}, apperr.LLMKeyInvalid("monthly_cap", "不能同时设置与取消")
	}
	if err := validateMonthlyCap(in.MonthlyCap); err != nil {
		return Key{}, err
	}
	if in.ModelSlugs != nil {
		slugs, err := normalizeKeySlugs(ctx, s.d.Pool, *in.ModelSlugs)
		if err != nil {
			return Key{}, err
		}
		in.ModelSlugs = &slugs
	}
	var out Key
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		before, err := scanKey(tx.QueryRow(ctx, `SELECT `+keyCols+` FROM llm_api_keys WHERE id = $1 AND user_id = $2 FOR UPDATE`, id, userID))
		if err != nil {
			return err
		}
		if before.Status == KeyRevoked {
			return apperr.ErrInvalidState
		}
		next := before
		if in.Name != nil {
			next.Name = *in.Name
		}
		if in.Status != nil {
			next.Status = *in.Status
		}
		if in.ModelSlugs != nil {
			next.ModelSlugs = *in.ModelSlugs
		}
		switch {
		case in.MonthlyCap != nil:
			next.MonthlyCap = in.MonthlyCap
		case in.ClearMonthlyCap:
			next.MonthlyCap = nil
		}
		now := s.d.Clock.Now()
		k, err := scanKey(tx.QueryRow(ctx, `UPDATE llm_api_keys SET name = $2, status = $3, model_slugs = $4, monthly_cap = $5, updated_at = $6
			WHERE id = $1 RETURNING `+keyCols, id, next.Name, next.Status, next.ModelSlugs, next.MonthlyCap, now))
		if err != nil {
			return err
		}
		out = k
		return audit.Record(ctx, tx, now, audit.Actor{UserID: userID, IP: ip}.Entry("llm.key.update", "llm_api_key", keyTarget(id),
			map[string]any{"before": before, "after": k}))
	})
	if err != nil {
		return Key{}, err
	}
	s.InvalidateKey(out.KeyHash)
	return out, nil
}

// RevokeKey 控制台撤销自己的 Key（不可恢复，行保留供明细引用）；已撤销幂等（不再记审计）。审计 llm.key.revoke。
func (s *Service) RevokeKey(ctx context.Context, userID, id int64, ip string) error {
	var hash string
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		cur, err := scanKey(tx.QueryRow(ctx, `SELECT `+keyCols+` FROM llm_api_keys WHERE id = $1 AND user_id = $2 FOR UPDATE`, id, userID))
		if err != nil {
			return err
		}
		hash = cur.KeyHash
		if cur.Status == KeyRevoked {
			return nil
		}
		now := s.d.Clock.Now()
		if _, err := tx.Exec(ctx, `UPDATE llm_api_keys SET status = $2, revoked_at = $3, updated_at = $3 WHERE id = $1`, id, KeyRevoked, now); err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, audit.Actor{UserID: userID, IP: ip}.Entry("llm.key.revoke", "llm_api_key", keyTarget(id),
			map[string]any{"name": cur.Name, "key_hint": cur.KeyHint, "from": cur.Status}))
	})
	if err != nil {
		return err
	}
	s.InvalidateKey(hash)
	return nil
}

// ListKeys 控制台：自己的 Key（不含撤销超过 30 天的），新的在前。
func (s *Service) ListKeys(ctx context.Context, userID int64) ([]Key, error) {
	return collectKeys(s.d.Pool.Query(ctx, `SELECT `+keyCols+` FROM llm_api_keys
		WHERE user_id = $1 AND (status <> $2 OR revoked_at IS NULL OR revoked_at >= $3)
		ORDER BY created_at DESC, id DESC`, userID, KeyRevoked, s.d.Clock.Now().Add(-revokedListAge)))
}

// AdminListKeys 后台：某用户的全部 Key（含已撤销），新的在前。
func (s *Service) AdminListKeys(ctx context.Context, userID int64) ([]Key, error) {
	return collectKeys(s.d.Pool.Query(ctx, `SELECT `+keyCols+` FROM llm_api_keys WHERE user_id = $1 ORDER BY created_at DESC, id DESC`, userID))
}

// AdminDisableKey 后台停用一把 Key（active → disabled；已停用不改状态但照记审计；已撤销 invalid_state）。reason 必填，写进审计 llm.key.admin_disable。
func (s *Service) AdminDisableKey(ctx context.Context, actor audit.Actor, id int64, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" || utf8.RuneCountInString(reason) > maxAdminReason {
		return apperr.LLMKeyInvalid("reason", "请填写原因（最多 200 个字符）")
	}
	var hash string
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		cur, err := scanKey(tx.QueryRow(ctx, `SELECT `+keyCols+` FROM llm_api_keys WHERE id = $1 FOR UPDATE`, id))
		if err != nil {
			return err
		}
		hash = cur.KeyHash
		if cur.Status == KeyRevoked {
			return apperr.ErrInvalidState
		}
		now := s.d.Clock.Now()
		if cur.Status != KeyDisabled {
			if _, err := tx.Exec(ctx, `UPDATE llm_api_keys SET status = $2, updated_at = $3 WHERE id = $1`, id, KeyDisabled, now); err != nil {
				return err
			}
		}
		// 已停用（用户自己停的或后台再次停用）也记审计：管理员的原因要留档
		return audit.Record(ctx, tx, now, actor.Entry("llm.key.admin_disable", "llm_api_key", keyTarget(id),
			map[string]any{"user_id": cur.UserID, "name": cur.Name, "key_hint": cur.KeyHint, "reason": reason, "from": cur.Status}))
	})
	if err != nil {
		return err
	}
	s.InvalidateKey(hash)
	return nil
}
