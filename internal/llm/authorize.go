package llm

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"spark-cloud/internal/platform/db"
)

// AuthContext 是网关鉴权（规格 §6.1 步骤 2～3）通过后的上下文。
//
// 整个结构随 Key 缓存 KeyCacheTTL（30 秒）：Account 里的 debt / unsettled_micro / 限额 / blocked、Key 的 month_cost_micro
// 以及用户状态都可能落后最多 30 秒（规格 T6 接受：撤销 / 停用 / 封禁最多 30 秒后生效）。
// 网关的预算预检要另读 wallet.Balance（BalanceCacheTTL）；月上限与欠费按缓存值判断，并发打穿由结算的欠费兜底（§7）。
type AuthContext struct {
	Key                   Key
	UserID                int64
	Account               Account // 用户还没有 llm_accounts 行时为全零值（UserID 已填）
	UserStatus, KYCStatus string
	Limits                Limits // 解析后的 RPM / TPM / Concurrency（默认或账户覆盖）
	// LoadSeq 是这份数据查库时的装载序号（查库之前取号，见 AuthLoadSeq）：网关据此只扣减「装载之后本进程新记的用量」，
	// 补上缓存里 unsettled_micro 落后的部分（预算预检）。
	LoadSeq uint64
}

// AuthError 鉴权 / 门槛失败；网关直接转成 OpenAI 错误体 {"error":{"message","type","code"}}。
type AuthError struct {
	Status              int
	Type, Code, Message string
}

func (e *AuthError) Error() string { return fmt.Sprintf("%d %s: %s", e.Status, e.Code, e.Message) }

func errInvalidKey() *AuthError {
	return &AuthError{Status: http.StatusUnauthorized, Type: "authentication_error", Code: "invalid_api_key",
		Message: "API Key 无效、已停用、已撤销或已过期"}
}

// keyCacheEntry 缓存一次成功的库查询（不论 Key 状态）；过期时间按调用方传入的 now 计算。「不存在」不缓存。
type keyCacheEntry struct {
	ac      AuthContext
	expires time.Time
}

// keyCache 是 Authorize 的进程内缓存：key_hash → *keyCacheEntry。
//
// gen 是失效代数：InvalidateKey / InvalidateUser 先 gen+1 再删条目。Authorize 未命中时先读 gen、再查库、写入条目后复核 gen：
// 变了就删掉自己刚写的条目（CompareAndDelete）。于是「查库在提交前、写缓存在失效后」的旧数据不会留在缓存里——
// 调用方在事务提交后再失效，本进程内的撤销 / 停用 / 封禁立即生效（期间已在途的那一次查询仍可能用旧数据放行一次）。
type keyCache struct {
	m         sync.Map
	gen       atomic.Uint64
	loads     atomic.Uint64 // 装载序号（每次查库 +1）
	afterLoad func()        // 仅测试：查库之后、写缓存之前调用
}

// Authorize 按 Key 哈希鉴权（规格 §6.1 步骤 2～3）：Key（active、未过期）→ 用户（未封禁、已实名）→ 账户（未停用、无欠费）。
// 查询结果缓存 KeyCacheTTL，状态 / 过期 / 门槛每次按 now 重新判断。失败返回 *AuthError；数据库错误原样返回。
func (s *Service) Authorize(ctx context.Context, keyHash string, now time.Time) (AuthContext, error) {
	if keyHash == "" {
		return AuthContext{}, errInvalidKey()
	}
	c := &s.keyCache
	if v, ok := c.m.Load(keyHash); ok && now.Before(v.(*keyCacheEntry).expires) {
		ac := v.(*keyCacheEntry).ac
		return ac, checkAuth(ac, now)
	}
	gen := c.gen.Load()
	seq := c.loads.Add(1) // 查库之前取号：之后提交的用量一定 ≥ 这个号（之前提交的可能被重复扣减一次，偏保守）
	found, err := s.loadAuth(ctx, keyHash)
	if err != nil {
		return AuthContext{}, err
	}
	if c.afterLoad != nil {
		c.afterLoad()
	}
	if found == nil {
		c.m.Delete(keyHash)
		return AuthContext{}, errInvalidKey()
	}
	found.LoadSeq = seq
	e := &keyCacheEntry{ac: *found, expires: now.Add(KeyCacheTTL)}
	c.m.Store(keyHash, e)
	if c.gen.Load() != gen {
		c.m.CompareAndDelete(keyHash, e)
	}
	return e.ac, checkAuth(e.ac, now)
}

func checkAuth(ac AuthContext, now time.Time) error {
	switch {
	case ac.Key.Status != KeyActive, ac.Key.ExpiresAt != nil && !now.Before(*ac.Key.ExpiresAt):
		return errInvalidKey()
	case ac.UserStatus == "banned":
		return &AuthError{Status: http.StatusForbidden, Type: "permission_error", Code: "account_banned", Message: "账号已被封禁"}
	case ac.KYCStatus != "verified":
		return &AuthError{Status: http.StatusForbidden, Type: "permission_error", Code: "kyc_required", Message: "请先在控制台完成实名认证"}
	case ac.Account.Blocked:
		msg := "Token 服务已被停用"
		if ac.Account.BlockedReason != "" {
			msg += "：" + ac.Account.BlockedReason
		}
		return &AuthError{Status: http.StatusForbidden, Type: "permission_error", Code: "account_blocked", Message: msg}
	case ac.Account.Debt > 0:
		return &AuthError{Status: http.StatusPaymentRequired, Type: "insufficient_quota", Code: "insufficient_quota",
			Message: fmt.Sprintf("账户有欠费 ¥%d.%02d，请充值后再试", ac.Account.Debt/100, ac.Account.Debt%100)}
	}
	return nil
}

// loadAuth 一条查询取 Key + 用户状态 + 账户（LEFT JOIN：账户行在首次建 Key 时插入，历史数据可能没有）。不存在返回 nil。
func (s *Service) loadAuth(ctx context.Context, keyHash string) (*AuthContext, error) {
	var ac AuthContext
	k := &ac.Key
	a := &ac.Account
	err := s.d.Pool.QueryRow(ctx, `SELECT k.id, k.user_id, k.name, k.key_hash, k.key_hint, k.status, k.model_slugs, k.monthly_cap, k.month_ym,
			k.month_cost_micro, k.expires_at, k.last_used_at, k.created_at, k.updated_at, k.revoked_at,
			u.status, u.kyc_status,
			COALESCE(a.unsettled_micro, 0), COALESCE(a.carry_micro, 0), COALESCE(a.debt, 0),
			a.rpm_limit, a.tpm_limit, a.concurrency_limit, COALESCE(a.blocked, false), COALESCE(a.blocked_reason, ''),
			COALESCE(a.created_at, k.created_at), COALESCE(a.updated_at, k.created_at)
		FROM llm_api_keys k
		JOIN users u ON u.id = k.user_id
		LEFT JOIN llm_accounts a ON a.user_id = k.user_id
		WHERE k.key_hash = $1`, keyHash).Scan(
		&k.ID, &k.UserID, &k.Name, &k.KeyHash, &k.KeyHint, &k.Status, &k.ModelSlugs, &k.MonthlyCap, &k.MonthYM,
		&k.MonthCostMicro, &k.ExpiresAt, &k.LastUsedAt, &k.CreatedAt, &k.UpdatedAt, &k.RevokedAt,
		&ac.UserStatus, &ac.KYCStatus,
		&a.UnsettledMicro, &a.CarryMicro, &a.Debt,
		&a.RPMLimit, &a.TPMLimit, &a.ConcurrencyLimit, &a.Blocked, &a.BlockedReason,
		&a.CreatedAt, &a.UpdatedAt)
	if db.IsNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if k.ModelSlugs == nil {
		k.ModelSlugs = []string{}
	}
	ac.UserID = k.UserID
	a.UserID = k.UserID
	ac.Limits = a.Limits()
	return &ac, nil
}

// AuthLoadSeq 返回当前装载序号。网关在记账成功后取它标记该笔用量：序号 ≥ AuthContext.LoadSeq 的用量
// 不在那份缓存的 unsettled_micro 里（或恰在边界上，被保守地多扣一次）。
func (s *Service) AuthLoadSeq() uint64 { return s.keyCache.loads.Load() }

// InvalidateKey 本进程内立即失效该哈希的缓存（控制台修改 / 撤销、后台停用时在事务提交后调用）；其它进程最多 KeyCacheTTL 后生效。
func (s *Service) InvalidateKey(hash string) {
	s.keyCache.gen.Add(1)
	s.keyCache.m.Delete(hash)
}

// InvalidateUser 本进程内立即失效该用户全部 Key 的缓存（账户限额 / 停用、欠费变化、封禁 / 解封、实名变化后在事务提交后调用）。
func (s *Service) InvalidateUser(userID int64) {
	s.keyCache.gen.Add(1)
	s.keyCache.m.Range(func(k, v any) bool {
		if v.(*keyCacheEntry).ac.UserID == userID {
			s.keyCache.m.CompareAndDelete(k, v)
		}
		return true
	})
}
