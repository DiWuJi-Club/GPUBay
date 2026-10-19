package referral_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/platform/paging"
	"spark-cloud/internal/platform/tz"
	"spark-cloud/internal/referral"
)

func TestMeInviteesRewards(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", false)
	b := e.user("13900000001", true)
	c := e.user("13900000002", false)
	e.bind(a, b)
	cb := e.bind(a, c)
	e.bindExec(`UPDATE referral_bindings SET status = 'held' WHERE id = $1`, cb)
	e.accrueOK(e.pay(b, "ord-1", 6900))
	e.accrueOK(e.pay(b, "ord-2", 6900))
	e.bindExec(`UPDATE referral_accounts SET debt = 50 WHERE user_id = $1`, a)
	me, err := e.svc.Me(e.ctx, a, "https://spark.test")
	if err != nil || len(me.Code) != 8 || me.Link != "https://spark.test/?ic="+me.Code || me.Status != "active" || me.KYCVerified || me.Rebate.RateBP != 1000 {
		t.Fatalf("me = %+v %v", me, err)
	}
	if me.Stats.Invitees != 2 || me.Stats.VerifiedInvitees != 1 || me.Stats.Pending != 1380 || me.Stats.SettledNet != 0 || me.Stats.Debt != 50 || me.Stats.MonthSettled != 0 {
		t.Fatalf("stats = %+v", me.Stats)
	}
	// 待发放明细带预计到账时间（settle_after = 扣款 + 持有期 7 天；验收缺陷 P8-A1）
	pend, _, err := e.svc.Rewards(e.ctx, a, "pending", paging.Page{Number: 1, Size: 20})
	if err != nil || len(pend) != 2 || pend[0].PaidAt.IsZero() || !pend[0].SettleAfter.Equal(pend[0].PaidAt.Add(7*24*time.Hour)) {
		t.Fatalf("pending rewards = %+v %v", pend, err)
	}
	if raw, _ := json.Marshal(pend[0]); !strings.Contains(string(raw), `"settle_after":"`) {
		t.Fatalf("reward json lacks settle_after: %s", raw)
	}
	// rebate 只给规则摘要：不带费率版本的内部字段（创建人、备注、id）
	raw, _ := json.Marshal(me)
	for _, leak := range []string{`"created_by"`, `"note"`, `"id"`} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("me json leaks %s: %s", leak, raw)
		}
	}
	// 发放后 settled_net 与本月
	e.bindExec(`UPDATE users SET kyc_status = 'verified' WHERE id = $1`, a)
	e.clk.Advance(8 * 24 * time.Hour)
	if _, err := e.svc.Release(e.ctx); err != nil {
		t.Fatal(err)
	}
	me, _ = e.svc.Me(e.ctx, a, "https://spark.test")
	if me.Stats.Pending != 0 || me.Stats.SettledNet != 1380 || me.Stats.MonthSettled != 1330 || me.Stats.Debt != 0 {
		t.Fatalf("after release stats = %+v", me.Stats) // 1380 应得，50 抵欠账，实际入账 1330
	}
	if n, err := e.svc.MonthSettledFor(e.ctx, a); err != nil || n != 1330 {
		t.Fatalf("month settled = %d %v", n, err)
	}
	// 下个月 1 日（北京时间）起本月清零；累计不变
	l := e.clk.Now().In(tz.CST)
	e.clk.Advance(time.Date(l.Year(), l.Month()+1, 1, 0, 0, 0, 0, tz.CST).Sub(e.clk.Now()))
	if n, _ := e.svc.MonthSettledFor(e.ctx, a); n != 0 {
		t.Fatalf("next month settled = %d", n)
	}
	list, total, err := e.svc.Invitees(e.ctx, a, paging.Page{Number: 1, Size: 20})
	if err != nil || total != 2 || len(list) != 2 || list[0].PhoneMasked != "139****0002" || list[0].Status != "held" || list[1].Contributed != 1380 || !list[1].KYCVerified {
		t.Fatalf("invitees = %+v %d %v", list, total, err)
	}
	raw, _ = json.Marshal(list)
	for _, leak := range []string{"13900000001", "13900000002", `"invitee_id"`, `"user_id"`, `"id"`, `"flags"`, `"hold_reason"`} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("invitees json leaks %s: %s", leak, raw)
		}
	}
	rewards, total, err := e.svc.Rewards(e.ctx, a, "", paging.Page{Number: 1, Size: 1})
	if err != nil || total != 2 || len(rewards) != 1 || rewards[0].OrderID != "ord-2" || rewards[0].InviteeMasked != "139****0001" || rewards[0].Status != "settled" || rewards[0].Reward != 690 || rewards[0].SettledAt == nil {
		t.Fatalf("rewards = %+v %d %v", rewards, total, err)
	}
	if rewards, _, _ := e.svc.Rewards(e.ctx, a, "", paging.Page{Number: 2, Size: 1}); len(rewards) != 1 || rewards[0].OrderID != "ord-1" {
		t.Fatalf("rewards page 2 = %+v", rewards)
	}
	if _, total, _ := e.svc.Rewards(e.ctx, a, "pending", paging.Page{Number: 1, Size: 20}); total != 0 {
		t.Fatal("status filter")
	}
	if _, total, _ := e.svc.Rewards(e.ctx, a, "settled", paging.Page{Number: 1, Size: 20}); total != 2 {
		t.Fatal("status filter settled")
	}
	if _, _, err := e.svc.Rewards(e.ctx, a, "bogus", paging.Page{Number: 1, Size: 20}); err == nil {
		t.Fatal("bogus status must be rejected")
	}
	// 非邀请人：Me 懒生成码，统计全 0；再次调用码不变
	me2, err := e.svc.Me(e.ctx, b, "https://spark.test")
	if err != nil || len(me2.Code) != 8 || me2.Stats != (referral.Stats{}) {
		t.Fatalf("me2 = %+v %v", me2, err)
	}
	if again, _ := e.svc.Me(e.ctx, b, "https://spark.test"); again.Code != me2.Code {
		t.Fatalf("code changed: %s → %s", me2.Code, again.Code)
	}
	// 别人的数据看不到
	if _, total, _ := e.svc.Invitees(e.ctx, b, paging.Page{Number: 1, Size: 20}); total != 0 {
		t.Fatal("b has no invitees")
	}
	if _, total, _ := e.svc.Rewards(e.ctx, b, "", paging.Page{Number: 1, Size: 20}); total != 0 {
		t.Fatal("b has no rewards")
	}
}
