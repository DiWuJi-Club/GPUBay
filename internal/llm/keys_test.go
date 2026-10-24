package llm_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"spark-cloud/internal/llm"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/tz"
)

func TestGenerateKeyFormat(t *testing.T) {
	re := regexp.MustCompile(`^sk-spark-[A-Za-z0-9]{40}$`)
	hexRe := regexp.MustCompile(`^[0-9a-f]{64}$`)
	seen := map[string]bool{}
	alphabet := map[rune]bool{}
	for range 200 {
		plain, hash, hint := llm.GenerateKey()
		if !re.MatchString(plain) || len(plain) != len("sk-spark-")+40 {
			t.Fatalf("plain = %q", plain)
		}
		sum := sha256.Sum256([]byte(plain))
		if hash != hex.EncodeToString(sum[:]) || !hexRe.MatchString(hash) || llm.HashKey(plain) != hash {
			t.Fatalf("hash = %q", hash)
		}
		if hint != "sk-spark-…"+plain[len(plain)-4:] {
			t.Fatalf("hint = %q (plain %q)", hint, plain)
		}
		if seen[plain] {
			t.Fatalf("duplicate key %q", plain)
		}
		seen[plain] = true
		for _, r := range plain[len("sk-spark-"):] {
			alphabet[r] = true
		}
	}
	// 200 × 40 个字符应覆盖 base62 的绝大部分（大小写字母与数字都出现）
	if len(alphabet) < 55 {
		t.Fatalf("alphabet coverage %d < 55", len(alphabet))
	}
}

func TestCreateKeyRequiresKYCAndLimit(t *testing.T) {
	e := newEnv(t)
	e.onlineModel("qwen3-4b", 1)
	e.createModel("draft-model")

	// 未实名 / 封禁：CanRent 的错误原样返回，不建账户行
	u0 := e.unverifiedUser()
	if _, _, err := e.svc.CreateKey(e.ctx, u0, llm.KeyInput{Name: "a"}, "1.1.1.1"); errCode(err) != "kyc_required" {
		t.Fatalf("unverified: %v", err)
	}
	ub := e.verifiedUser()
	e.exec(`UPDATE users SET status = 'banned' WHERE id = $1`, ub)
	if _, _, err := e.svc.CreateKey(e.ctx, ub, llm.KeyInput{Name: "a"}, "1.1.1.1"); errCode(err) != "banned" {
		t.Fatalf("banned: %v", err)
	}
	if n := e.queryInt(`SELECT count(*) FROM llm_accounts WHERE user_id IN ($1, $2)`, u0, ub); n != 0 {
		t.Fatalf("accounts for rejected users = %d", n)
	}

	u := e.verifiedUser()
	past := e.clk.Now().Add(-time.Second)
	zero := int64(0)
	for _, c := range []struct {
		name string
		in   llm.KeyInput
	}{
		{"empty name", llm.KeyInput{Name: "  "}},
		{"long name", llm.KeyInput{Name: strings.Repeat("钥", 33)}},
		{"unknown slug", llm.KeyInput{Name: "a", ModelSlugs: []string{"nope"}}},
		{"draft slug", llm.KeyInput{Name: "a", ModelSlugs: []string{"draft-model"}}},
		{"zero cap", llm.KeyInput{Name: "a", MonthlyCap: &zero}},
		{"past expiry", llm.KeyInput{Name: "a", ExpiresAt: &past}},
	} {
		if _, _, err := e.svc.CreateKey(e.ctx, u, c.in, "1.1.1.1"); errCode(err) != "llm_key_invalid" {
			t.Fatalf("%s: %v", c.name, err)
		}
	}

	// 32 个字符的名称、去重的模型列表、上限与到期时间都保存下来
	cap1 := int64(500)
	exp := e.clk.Now().Add(24 * time.Hour)
	plain, k, err := e.svc.CreateKey(e.ctx, u, llm.KeyInput{Name: " " + strings.Repeat("钥", 32) + " ",
		ModelSlugs: []string{"qwen3-4b", " qwen3-4b"}, MonthlyCap: &cap1, ExpiresAt: &exp}, "1.2.3.4")
	if err != nil {
		t.Fatal(err)
	}
	if k.Name != strings.Repeat("钥", 32) || k.Status != llm.KeyActive || len(k.ModelSlugs) != 1 || k.ModelSlugs[0] != "qwen3-4b" ||
		k.MonthlyCap == nil || *k.MonthlyCap != 500 || k.ExpiresAt == nil || !k.ExpiresAt.Equal(exp) || k.UserID != u ||
		k.KeyHint != "sk-spark-…"+plain[len(plain)-4:] || k.KeyHash != llm.HashKey(plain) {
		t.Fatalf("key = %+v", k)
	}
	if n := e.queryInt(`SELECT count(*) FROM llm_accounts WHERE user_id = $1`, u); n != 1 {
		t.Fatalf("account rows = %d", n)
	}

	// 审计：user actor，diff 不含明文与哈希
	target := strconv.FormatInt(k.ID, 10)
	if e.auditCount("llm.key.create", target) != 1 {
		t.Fatal("no audit")
	}
	var actorType, ip string
	var actorID int64
	var diff []byte
	if err := e.pool.QueryRow(e.ctx, `SELECT actor_type, actor_id, ip, diff FROM audit_logs WHERE action = 'llm.key.create' AND target_id = $1`,
		target).Scan(&actorType, &actorID, &ip, &diff); err != nil {
		t.Fatal(err)
	}
	if actorType != "user" || actorID != u || ip != "1.2.3.4" {
		t.Fatalf("actor = %s %d %s", actorType, actorID, ip)
	}
	if strings.Contains(string(diff), plain) || strings.Contains(string(diff), plain[len("sk-spark-"):]) || strings.Contains(string(diff), k.KeyHash) {
		t.Fatalf("audit diff leaks key: %s", diff)
	}
	if !strings.Contains(string(diff), k.KeyHint) {
		t.Fatalf("audit diff without hint: %s", diff)
	}

	// 上限 20：active + disabled 计数，revoked 不计
	keys := []llm.Key{k}
	for range 19 {
		_, kk := e.createKey(u)
		keys = append(keys, kk)
	}
	if _, _, err := e.svc.CreateKey(e.ctx, u, llm.KeyInput{Name: "21st"}, "1.1.1.1"); !errors.Is(err, apperr.ErrLLMKeyLimit) {
		t.Fatalf("21st: %v", err)
	}
	disabled := llm.KeyDisabled
	if _, err := e.svc.UpdateKey(e.ctx, u, keys[1].ID, llm.KeyUpdate{Status: &disabled}, "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.svc.CreateKey(e.ctx, u, llm.KeyInput{Name: "21st"}, "1.1.1.1"); !errors.Is(err, apperr.ErrLLMKeyLimit) {
		t.Fatalf("disabled must count: %v", err)
	}
	if err := e.svc.RevokeKey(e.ctx, u, keys[2].ID, "1.1.1.1"); err != nil {
		t.Fatal(err)
	}
	e.createKey(u) // revoked 不计数：又能建一把
	if _, _, err := e.svc.CreateKey(e.ctx, u, llm.KeyInput{Name: "22nd"}, "1.1.1.1"); !errors.Is(err, apperr.ErrLLMKeyLimit) {
		t.Fatalf("after refill: %v", err)
	}
}

// 并发建 Key 不会越过 20 的上限（llm_accounts 行锁串行化计数）。
func TestCreateKeyConcurrentLimit(t *testing.T) {
	e := newEnv(t)
	u := e.verifiedUser()
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, limited := 0, 0
	for i := range 30 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := e.svc.CreateKey(e.ctx, u, llm.KeyInput{Name: "k" + strconv.Itoa(i)}, "")
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, apperr.ErrLLMKeyLimit):
				limited++
			default:
				t.Errorf("CreateKey: %v", err)
			}
		}()
	}
	wg.Wait()
	if ok != llm.MaxKeysPerUser || limited != 10 {
		t.Fatalf("ok %d limited %d", ok, limited)
	}
	if n := e.queryInt(`SELECT count(*) FROM llm_api_keys WHERE user_id = $1`, u); n != llm.MaxKeysPerUser {
		t.Fatalf("rows = %d", n)
	}
}

func TestKeyModelFilterAndMonthlyCap(t *testing.T) {
	e := newEnv(t)
	e.onlineModel("qwen3-4b", 1)
	e.onlineModel("other", 1)
	u := e.verifiedUser()
	cap1 := int64(1)
	plain, k := e.createKey(u, func(in *llm.KeyInput) { in.ModelSlugs = []string{"other"}; in.MonthlyCap = &cap1 })
	_, all := e.createKey(u)

	if !k.KeyAllowsModel("other") || k.KeyAllowsModel("qwen3-4b") {
		t.Fatalf("filtered key slugs %v", k.ModelSlugs)
	}
	if !all.KeyAllowsModel("qwen3-4b") || !all.KeyAllowsModel("anything") || all.MonthCapExceeded(e.clk.Now()) {
		t.Fatal("unrestricted key")
	}

	// T0 = 2026-01-01 08:00 北京时间：本月 2026-01
	now := e.clk.Now()
	if llm.MonthOf(now) != "2026-01" {
		t.Fatalf("MonthOf = %s", llm.MonthOf(now))
	}
	if k.MonthCapExceeded(now) {
		t.Fatal("fresh key over cap")
	}
	e.exec(`UPDATE llm_api_keys SET month_ym = '2026-01', month_cost_micro = 999999 WHERE id = $1`, k.ID)
	ac, err := e.svc.Authorize(e.ctx, llm.HashKey(plain), now)
	if err != nil {
		t.Fatal(err)
	}
	if ac.Key.MonthCostMicro != 999999 || ac.Key.MonthCapExceeded(now) || ac.Key.CurrentMonthCostMicro(now) != 999999 {
		t.Fatalf("999999 micro vs cap 1 fen: %+v", ac.Key)
	}
	e.exec(`UPDATE llm_api_keys SET month_cost_micro = 1000000 WHERE id = $1`, k.ID)
	e.svc.InvalidateKey(llm.HashKey(plain))
	ac, err = e.svc.Authorize(e.ctx, llm.HashKey(plain), now)
	if err != nil {
		t.Fatal(err)
	}
	if !ac.Key.MonthCapExceeded(now) {
		t.Fatal("1 fen spent vs cap 1 fen must be exceeded")
	}

	// 下月 1 日 00:00 北京时间 = 2026-01-31 16:00 UTC：计数归零
	boundary := time.Date(2026, 2, 1, 0, 0, 0, 0, tz.CST)
	e.clk.Advance(boundary.Sub(now) - time.Nanosecond)
	if !ac.Key.MonthCapExceeded(e.clk.Now()) {
		t.Fatal("1ns before month boundary still exceeded")
	}
	e.clk.Advance(time.Nanosecond)
	if llm.MonthOf(e.clk.Now()) != "2026-02" || ac.Key.MonthCapExceeded(e.clk.Now()) || ac.Key.CurrentMonthCostMicro(e.clk.Now()) != 0 {
		t.Fatal("new month must reset the cap")
	}

	// 无上限的 Key 花多少都不超
	e.exec(`UPDATE llm_api_keys SET month_ym = '2026-02', month_cost_micro = 1000000000000 WHERE id = $1`, all.ID)
	if got := e.keyByID(u, all.ID); got.MonthCapExceeded(e.clk.Now()) {
		t.Fatal("no cap")
	}
}

func (e *env) keyByID(userID, id int64) llm.Key {
	e.t.Helper()
	keys, err := e.svc.AdminListKeys(e.ctx, userID)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, k := range keys {
		if k.ID == id {
			return k
		}
	}
	e.t.Fatalf("key %d not found", id)
	return llm.Key{}
}

func TestUpdateRevokeAndListKeys(t *testing.T) {
	e := newEnv(t)
	e.onlineModel("qwen3-4b", 1)
	m := e.onlineModel("old", 1)
	if _, err := e.svc.SetModelStatus(e.ctx, e.admin(), m.ID, llm.ModelOffline); err != nil {
		t.Fatal(err)
	}
	u, other := e.verifiedUser(), e.verifiedUser()
	cap5 := int64(5)
	_, k := e.createKey(u, func(in *llm.KeyInput) { in.MonthlyCap = &cap5 })

	// 修改：名称 / 模型（offline 模型也可选）/ 上限；只改传了的字段
	name := " 新名字 "
	slugs := []string{"old", "qwen3-4b"}
	got, err := e.svc.UpdateKey(e.ctx, u, k.ID, llm.KeyUpdate{Name: &name, ModelSlugs: &slugs}, "5.5.5.5")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "新名字" || len(got.ModelSlugs) != 2 || got.MonthlyCap == nil || *got.MonthlyCap != 5 {
		t.Fatalf("updated = %+v", got)
	}
	got, err = e.svc.UpdateKey(e.ctx, u, k.ID, llm.KeyUpdate{ClearMonthlyCap: true, ModelSlugs: &[]string{}}, "5.5.5.5")
	if err != nil || got.MonthlyCap != nil || len(got.ModelSlugs) != 0 || got.ModelSlugs == nil {
		t.Fatalf("clear: %+v %v", got, err)
	}
	if e.auditCount("llm.key.update", strconv.FormatInt(k.ID, 10)) != 2 {
		t.Fatal("update audit")
	}
	bad := "revoked"
	for _, c := range []struct {
		name string
		in   llm.KeyUpdate
	}{
		{"status revoked", llm.KeyUpdate{Status: &bad}},
		{"cap and clear", llm.KeyUpdate{MonthlyCap: &cap5, ClearMonthlyCap: true}},
		{"unknown slug", llm.KeyUpdate{ModelSlugs: &[]string{"nope"}}},
	} {
		if _, err := e.svc.UpdateKey(e.ctx, u, k.ID, c.in, ""); errCode(err) != "llm_key_invalid" {
			t.Fatalf("%s: %v", c.name, err)
		}
	}
	// 别人的 Key → 不存在
	if _, err := e.svc.UpdateKey(e.ctx, other, k.ID, llm.KeyUpdate{Name: &name}, ""); !errors.Is(err, apperr.ErrLLMKeyNotFound) {
		t.Fatalf("other user update: %v", err)
	}
	if err := e.svc.RevokeKey(e.ctx, other, k.ID, ""); !errors.Is(err, apperr.ErrLLMKeyNotFound) {
		t.Fatalf("other user revoke: %v", err)
	}

	// 撤销：不可恢复；重复撤销幂等（只记一次审计）
	if err := e.svc.RevokeKey(e.ctx, u, k.ID, "5.5.5.5"); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.RevokeKey(e.ctx, u, k.ID, "5.5.5.5"); err != nil {
		t.Fatal(err)
	}
	if e.auditCount("llm.key.revoke", strconv.FormatInt(k.ID, 10)) != 1 {
		t.Fatal("revoke audit")
	}
	active := llm.KeyActive
	if _, err := e.svc.UpdateKey(e.ctx, u, k.ID, llm.KeyUpdate{Status: &active}, ""); !errors.Is(err, apperr.ErrInvalidState) {
		t.Fatalf("re-activate revoked: %v", err)
	}

	// 列表：revoked 30 天内仍显示，超过 30 天不显示（后台始终显示）
	_, k2 := e.createKey(u)
	list, err := e.svc.ListKeys(e.ctx, u)
	if err != nil || len(list) != 2 || list[0].ID != k2.ID || list[1].Status != llm.KeyRevoked || list[1].RevokedAt == nil {
		t.Fatalf("list = %+v %v", list, err)
	}
	e.clk.Advance(30*24*time.Hour + time.Second)
	if list, _ := e.svc.ListKeys(e.ctx, u); len(list) != 1 || list[0].ID != k2.ID {
		t.Fatalf("list after 30d = %+v", list)
	}
	if list, _ := e.svc.AdminListKeys(e.ctx, u); len(list) != 2 {
		t.Fatalf("admin list = %+v", list)
	}
	if list, _ := e.svc.ListKeys(e.ctx, other); list == nil || len(list) != 0 {
		t.Fatalf("empty list must be non-nil: %#v", list)
	}

	// 列表 JSON 不含哈希
	b, _ := json.Marshal(list)
	if strings.Contains(string(b), "hash") {
		t.Fatalf("json leaks hash: %s", b)
	}
}

func TestAdminDisableKey(t *testing.T) {
	e := newEnv(t)
	u := e.verifiedUser()
	plain, k := e.createKey(u)
	if code, _ := e.authCode(e.svc, plain); code != 0 {
		t.Fatal("precondition")
	}
	if err := e.svc.AdminDisableKey(e.ctx, e.admin(), k.ID, " "); errCode(err) != "llm_key_invalid" {
		t.Fatalf("empty reason: %v", err)
	}
	if err := e.svc.AdminDisableKey(e.ctx, e.admin(), 999999, "滥用"); !errors.Is(err, apperr.ErrLLMKeyNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if err := e.svc.AdminDisableKey(e.ctx, e.admin(), k.ID, "滥用"); err != nil {
		t.Fatal(err)
	}
	if got := e.keyByID(u, k.ID); got.Status != llm.KeyDisabled {
		t.Fatalf("status = %s", got.Status)
	}
	// 本进程立即失效
	if code, c := e.authCode(e.svc, plain); code != 401 || c != "invalid_api_key" {
		t.Fatalf("after admin disable: %d %s", code, c)
	}
	diff := e.lastAuditDiff("llm.key.admin_disable", strconv.FormatInt(k.ID, 10))
	if diff["reason"] != "滥用" || diff["key_hint"] != k.KeyHint {
		t.Fatalf("diff = %v", diff)
	}
	// 已停用：状态不变，但照记审计（留下管理员的原因）；已撤销：invalid_state
	if err := e.svc.AdminDisableKey(e.ctx, e.admin(), k.ID, "again"); err != nil {
		t.Fatal(err)
	}
	if e.auditCount("llm.key.admin_disable", strconv.FormatInt(k.ID, 10)) != 2 {
		t.Fatal("repeated admin disable must still audit")
	}
	if diff := e.lastAuditDiff("llm.key.admin_disable", strconv.FormatInt(k.ID, 10)); diff["reason"] != "again" || diff["from"] != llm.KeyDisabled {
		t.Fatalf("second diff = %v", diff)
	}
	if err := e.svc.RevokeKey(e.ctx, u, k.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.AdminDisableKey(e.ctx, e.admin(), k.ID, "x"); !errors.Is(err, apperr.ErrInvalidState) {
		t.Fatalf("revoked: %v", err)
	}
}
