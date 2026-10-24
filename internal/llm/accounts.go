package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/paging"
	"spark-cloud/internal/platform/tz"
)

// 后台可设的按用户限额范围（规格 §10.2 PUT /llm/accounts/{user_id}；NULL = 默认 60 / 200,000 / 8）。
// 并发上限 512 与 vLLM MAX_NUM_SEQS 的上限一致；RPM / TPM 上限只防误填，远高于单机吞吐。
const (
	MaxRPMLimit         = 100_000
	MaxTPMLimit         = 100_000_000
	MaxConcurrencyLimit = 512
	maxBlockedReason    = 200
)

const accountCols = `user_id, unsettled_micro, carry_micro, debt, rpm_limit, tpm_limit, concurrency_limit, blocked, blocked_reason,
	created_at, updated_at`

func scanAccount(row pgx.Row) (Account, error) {
	var a Account
	err := row.Scan(&a.UserID, &a.UnsettledMicro, &a.CarryMicro, &a.Debt, &a.RPMLimit, &a.TPMLimit, &a.ConcurrencyLimit,
		&a.Blocked, &a.BlockedReason, &a.CreatedAt, &a.UpdatedAt)
	return a, err
}

// EnsureAccount 取用户的 llm_accounts 行（不存在则插入默认行），并 FOR UPDATE 加行锁——在事务 q 里调用时锁持有到事务结束，
// 这是记账 / 结算 / 后台改账户的第一把锁（规格 §14）。用户不存在 → apperr.ErrNotFound。
func (s *Service) EnsureAccount(ctx context.Context, q db.Querier, userID int64) (Account, error) {
	now := s.d.Clock.Now()
	if _, err := q.Exec(ctx, `INSERT INTO llm_accounts (user_id, created_at, updated_at) VALUES ($1, $2, $2)
		ON CONFLICT (user_id) DO NOTHING`, userID, now); err != nil {
		if db.IsForeignKeyViolation(err) {
			return Account{}, apperr.ErrNotFound
		}
		return Account{}, err
	}
	return scanAccount(q.QueryRow(ctx, `SELECT `+accountCols+` FROM llm_accounts WHERE user_id = $1 FOR UPDATE`, userID))
}

// AccountView 控制台 GET /llm/account（规格 §10.1，金额单位分；base_url 由接口层附加）：
//   - Unsettled = ceil(unsettled_micro / 10^6)（与预算预检 T2 同口径，宁可多显示 1 分）；
//   - TodayCost / MonthCost = 北京时间今天 / 本月的 Σ cost_micro（已结算部分取 llm_usage_daily、未结算部分取 llm_usage）
//     向下取整到分（与结算 floor(Σ / 10^6) 同口径；单次几厘的请求在合计不足 1 分时显示 0）；
//   - MonthTokens = 本月 Σ (prompt + completion)，口径同上；
//   - Limits 为解析后的限额（NULL → 默认值）；KYCVerified = users.kyc_status = 'verified'。
type AccountView struct {
	Debt          int64  `json:"debt"`
	Unsettled     int64  `json:"unsettled"`
	TodayCost     int64  `json:"today_cost"`
	MonthCost     int64  `json:"month_cost"`
	MonthTokens   int64  `json:"month_tokens"`
	Limits        Limits `json:"limits"`
	Blocked       bool   `json:"blocked"`
	BlockedReason string `json:"blocked_reason"`
	KYCVerified   bool   `json:"kyc_verified"`
}

// Account 控制台账户概览。用户还没有 llm_accounts 行时返回全零 + 默认限额（不插行）；用户不存在 → apperr.ErrNotFound。
func (s *Service) Account(ctx context.Context, userID int64) (AccountView, error) {
	var (
		kyc string
		a   Account
	)
	err := s.d.Pool.QueryRow(ctx, `SELECT u.kyc_status, COALESCE(a.unsettled_micro, 0), COALESCE(a.debt, 0),
			a.rpm_limit, a.tpm_limit, a.concurrency_limit, COALESCE(a.blocked, false), COALESCE(a.blocked_reason, '')
		FROM users u LEFT JOIN llm_accounts a ON a.user_id = u.id WHERE u.id = $1`, userID).Scan(
		&kyc, &a.UnsettledMicro, &a.Debt, &a.RPMLimit, &a.TPMLimit, &a.ConcurrencyLimit, &a.Blocked, &a.BlockedReason)
	if db.IsNoRows(err) {
		return AccountView{}, apperr.ErrNotFound
	}
	if err != nil {
		return AccountView{}, err
	}
	now := s.d.Clock.Now()
	ms, ds := monthStart(now), tz.DayStart(now)
	var today, month, tokens int64
	if err := s.d.Pool.QueryRow(ctx, `WITH d AS (
			SELECT day, cost_micro, prompt_tokens + completion_tokens AS tok FROM llm_usage_daily WHERE user_id = $1 AND day >= $2::date),
		     g AS (
			SELECT created_at, cost_micro, prompt_tokens::bigint + completion_tokens AS tok FROM llm_usage
			WHERE user_id = $1 AND settlement_id IS NULL AND created_at >= $3)
		SELECT (COALESCE((SELECT sum(cost_micro) FROM d WHERE day >= $4::date), 0)
				+ COALESCE((SELECT sum(cost_micro) FROM g WHERE created_at >= $5), 0))::bigint,
			(COALESCE((SELECT sum(cost_micro) FROM d), 0) + COALESCE((SELECT sum(cost_micro) FROM g), 0))::bigint,
			(COALESCE((SELECT sum(tok) FROM d), 0) + COALESCE((SELECT sum(tok) FROM g), 0))::bigint`,
		userID, cstDate(ms), ms, cstDate(ds), ds).Scan(&today, &month, &tokens); err != nil {
		return AccountView{}, err
	}
	return AccountView{
		Debt: a.Debt, Unsettled: ceilFen(a.UnsettledMicro), TodayCost: floorFen(today), MonthCost: floorFen(month),
		MonthTokens: tokens, Limits: a.Limits(), Blocked: a.Blocked, BlockedReason: a.BlockedReason, KYCVerified: kyc == "verified",
	}, nil
}

// OptInt 是可清空的整数字段（PUT 的三态）：Set=false 不改；Set=true 且 Value=nil 恢复默认（写 NULL）；否则设为 *Value。
// JSON 解码时只要出现该键即 Set（null → Value=nil），所以接口层可以直接把请求体解进 AccountUpdate。
type OptInt struct {
	Set   bool
	Value *int
}

func (o *OptInt) UnmarshalJSON(b []byte) error {
	o.Set = true
	if string(b) == "null" {
		o.Value = nil
		return nil
	}
	var v int
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	o.Value = &v
	return nil
}

// AccountUpdate 后台 PUT /llm/accounts/{user_id}：没给的字段不改；限额给 null 恢复默认。
// Blocked=true 时停用原因必填（trim 后 1～200 字；已停用时可只改原因）；Blocked=false 解除停用并清空原因。
type AccountUpdate struct {
	RPMLimit         OptInt  `json:"rpm_limit"`
	TPMLimit         OptInt  `json:"tpm_limit"`
	ConcurrencyLimit OptInt  `json:"concurrency_limit"`
	Blocked          *bool   `json:"blocked"`
	BlockedReason    *string `json:"blocked_reason"`
}

func applyLimit(cur *int, o OptInt, field string, max int) (*int, error) {
	if !o.Set {
		return cur, nil
	}
	if o.Value == nil {
		return nil, nil
	}
	if *o.Value < 1 || *o.Value > max {
		return nil, apperr.LLMAccountInvalid(field, fmt.Sprintf("须在 1～%d 之间（留空恢复默认）", max))
	}
	v := *o.Value
	return &v, nil
}

// AdminUpdateAccount 后台改用户的限额 / 停用（账户行不存在则创建）；审计 llm.account.update{before, after}。
// 事务提交后本进程内该用户全部 Key 的鉴权缓存立即失效（InvalidateUser，见 authorize.go），其它进程最多 KeyCacheTTL 后生效。
func (s *Service) AdminUpdateAccount(ctx context.Context, actor audit.Actor, userID int64, in AccountUpdate) (Account, error) {
	var out Account
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		before, err := s.EnsureAccount(ctx, tx, userID)
		if err != nil {
			return err
		}
		next := before
		if next.RPMLimit, err = applyLimit(before.RPMLimit, in.RPMLimit, "rpm_limit", MaxRPMLimit); err != nil {
			return err
		}
		if next.TPMLimit, err = applyLimit(before.TPMLimit, in.TPMLimit, "tpm_limit", MaxTPMLimit); err != nil {
			return err
		}
		if next.ConcurrencyLimit, err = applyLimit(before.ConcurrencyLimit, in.ConcurrencyLimit, "concurrency_limit", MaxConcurrencyLimit); err != nil {
			return err
		}
		if in.Blocked != nil {
			next.Blocked = *in.Blocked
		}
		if next.Blocked {
			if in.BlockedReason != nil {
				next.BlockedReason = strings.TrimSpace(*in.BlockedReason)
			} else if !before.Blocked {
				next.BlockedReason = ""
			}
			if next.BlockedReason == "" || utf8.RuneCountInString(next.BlockedReason) > maxBlockedReason {
				return apperr.LLMAccountInvalid("blocked_reason", "停用须填写原因（最多 200 个字符）")
			}
		} else {
			next.BlockedReason = ""
		}
		now := s.d.Clock.Now()
		a, err := scanAccount(tx.QueryRow(ctx, `UPDATE llm_accounts SET rpm_limit = $2, tpm_limit = $3, concurrency_limit = $4,
				blocked = $5, blocked_reason = $6, updated_at = $7
			WHERE user_id = $1 RETURNING `+accountCols,
			userID, next.RPMLimit, next.TPMLimit, next.ConcurrencyLimit, next.Blocked, next.BlockedReason, now))
		if err != nil {
			return err
		}
		out = a
		return audit.Record(ctx, tx, now, actor.Entry("llm.account.update", "user", strconv.FormatInt(userID, 10),
			map[string]any{"before": before, "after": a}))
	})
	if err != nil {
		return Account{}, err
	}
	s.InvalidateUser(userID)
	return out, nil
}

// AccountFilter 后台账户列表筛选：Q 为数字（用户 ID 精确或手机号子串）；Blocked 非 nil 按停用状态；Debt 只看欠费 > 0。
type AccountFilter struct {
	Q       string
	Blocked *bool
	Debt    bool
}

// AccountRow 后台账户列表一行（规格 §10.2 GET /llm/accounts）：账户行 + 手机号、有效 Key 数（不含 revoked）、
// 本月消费（分，floor，口径同 AccountView.MonthCost）、未结算（分，ceil）、解析后的限额（原始 *_limit 在 Account 里，NULL = 默认）。
type AccountRow struct {
	Account
	Phone     string `json:"phone"`
	KeyCount  int    `json:"key_count"`
	MonthCost int64  `json:"month_cost"`
	Unsettled int64  `json:"unsettled"`
	Limits    Limits `json:"limits"`
}

// AdminListAccounts 后台账户列表（有 llm_accounts 行的用户：建过 Key、记过账或被后台改过），最近变动的在前。
func (s *Service) AdminListAccounts(ctx context.Context, f AccountFilter, p paging.Page) ([]AccountRow, int, error) {
	var w db.Where
	if q := strings.TrimSpace(f.Q); q != "" {
		if !isDigits(q) {
			return nil, 0, apperr.BadRequest("q 只能是手机号（片段）或用户 ID")
		}
		w.Add("(a.user_id::text = ? OR u.phone LIKE ?)", q, "%"+q+"%")
	}
	if f.Blocked != nil {
		w.Add("a.blocked = ?", *f.Blocked)
	}
	if f.Debt {
		w.Add("a.debt > 0")
	}
	const from = ` FROM llm_accounts a JOIN users u ON u.id = a.user_id WHERE `
	var total int
	if err := s.d.Pool.QueryRow(ctx, `SELECT count(*)`+from+w.SQL(), w.Args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	ms := monthStart(s.d.Clock.Now())
	n := len(w.Args)
	rows, err := s.d.Pool.Query(ctx, fmt.Sprintf(`SELECT a.user_id, a.unsettled_micro, a.carry_micro, a.debt, a.rpm_limit, a.tpm_limit,
			a.concurrency_limit, a.blocked, a.blocked_reason, a.created_at, a.updated_at, u.phone,
			(SELECT count(*) FROM llm_api_keys k WHERE k.user_id = a.user_id AND k.status <> 'revoked')::int,
			(COALESCE((SELECT sum(d.cost_micro) FROM llm_usage_daily d WHERE d.user_id = a.user_id AND d.day >= $%d::date), 0)
				+ COALESCE((SELECT sum(g.cost_micro) FROM llm_usage g
					WHERE g.user_id = a.user_id AND g.settlement_id IS NULL AND g.created_at >= $%d), 0))::bigint`+
		from+w.SQL()+` ORDER BY a.updated_at DESC, a.user_id DESC LIMIT $%d OFFSET $%d`, n+1, n+2, n+3, n+4),
		append(w.Args, cstDate(ms), ms, p.Limit(), p.Offset())...)
	if err != nil {
		return nil, 0, err
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (AccountRow, error) {
		var r AccountRow
		a := &r.Account
		var month int64
		err := row.Scan(&a.UserID, &a.UnsettledMicro, &a.CarryMicro, &a.Debt, &a.RPMLimit, &a.TPMLimit, &a.ConcurrencyLimit,
			&a.Blocked, &a.BlockedReason, &a.CreatedAt, &a.UpdatedAt, &r.Phone, &r.KeyCount, &month)
		r.MonthCost, r.Unsettled, r.Limits = floorFen(month), ceilFen(a.UnsettledMicro), a.Limits()
		return r, err
	})
	if list == nil && err == nil {
		list = []AccountRow{}
	}
	return list, total, err
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}
