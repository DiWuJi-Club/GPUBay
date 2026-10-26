package adminapi_test

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	"spark-cloud/internal/admin"
)

var (
	all     = []admin.Role{admin.RoleSuper, admin.RoleOps, admin.RoleFinance}
	ops     = []admin.Role{admin.RoleSuper, admin.RoleOps}
	finance = []admin.Role{admin.RoleSuper, admin.RoleFinance}
	super   = []admin.Role{admin.RoleSuper}
)

// expectedRoutes 是规格 §2 权限矩阵展开到每个接口后的期望（不从实现读取，防止实现改错而测试跟着错）。
var expectedRoutes = []struct {
	method, path string
	roles        []admin.Role
}{
	{"GET", "/me", all}, {"POST", "/me/password", all},
	{"GET", "/admins", super}, {"POST", "/admins", super}, {"PUT", "/admins/{id}", super}, {"POST", "/admins/{id}/reset-totp", super},

	{"GET", "/machines", ops}, {"POST", "/machines", ops}, {"POST", "/machines/import", ops}, {"GET", "/machines/{id}", ops},
	{"POST", "/machines/{id}/maintenance", ops}, {"POST", "/machines/{id}/power-cycle", ops},
	{"POST", "/machines/{id}/enroll-command", ops}, {"POST", "/machines/{id}/revoke-enrollment", ops},
	{"GET", "/pdus", ops}, {"POST", "/pdus", ops}, {"PUT", "/pdus/{id}", ops},
	{"GET", "/alerts", all}, {"POST", "/alerts/{id}/resolve", all},
	{"GET", "/groups", all}, {"POST", "/groups", ops}, {"PUT", "/groups/{id}", ops}, {"DELETE", "/groups/{id}", ops},
	{"POST", "/groups/{id}/machines", ops}, {"DELETE", "/groups/{id}/machines", ops}, {"PUT", "/machines/{id}", ops},
	{"GET", "/bookings", all}, {"GET", "/bookings/upcoming", all}, {"POST", "/instances/{id}/cancel-booking", ops},
	{"POST", "/instances/{id}/reassign", ops}, {"GET", "/machines/{id}/calendar", ops},

	{"GET", "/instances", all}, {"GET", "/instances/{id}", all}, {"GET", "/instances/{id}/refund-quote", all},
	{"POST", "/instances/{id}/extend", ops}, {"POST", "/instances/{id}/migrate", ops}, {"POST", "/instances/{id}/rewipe", ops},
	{"POST", "/instances/{id}/refund-release", finance},
	{"GET", "/instances/{id}/ssh-sessions", all}, {"POST", "/instances/{id}/ports/{port}/force-private", ops},
	{"DELETE", "/instances/{id}/ports/{port}", ops},

	{"GET", "/users", all}, {"GET", "/users/{id}", all},
	{"POST", "/users/{id}/ban", ops}, {"POST", "/users/{id}/unban", ops},
	{"GET", "/kyc/pending", ops}, {"GET", "/kyc/{id}", ops}, {"POST", "/kyc/{id}/review", ops}, {"GET", "/files/*", ops},
	{"POST", "/users/{id}/adjust", finance}, {"POST", "/users/{id}/manual-recharge", finance},

	{"GET", "/orders", finance}, {"GET", "/orders/export", finance}, {"GET", "/ledger", finance}, {"GET", "/ledger/export", finance},
	{"GET", "/recharges", finance}, {"GET", "/recharges/export", finance},

	{"GET", "/referral/summary", finance}, {"GET", "/referral/config", finance}, {"POST", "/referral/config", super},
	{"GET", "/referral/inviters", finance}, {"GET", "/referral/inviters/{id}", finance},
	{"POST", "/referral/inviters/{id}/freeze", finance}, {"POST", "/referral/inviters/{id}/unfreeze", finance},
	{"GET", "/referral/bindings", finance}, {"GET", "/referral/bindings/export", finance},
	{"POST", "/referral/bindings/{id}/approve", finance}, {"POST", "/referral/bindings/{id}/revoke", finance},
	{"GET", "/referral/rewards", finance}, {"GET", "/referral/rewards/export", finance},
	{"POST", "/referral/rescan", super},

	{"GET", "/products", all}, {"PUT", "/products/{id}/prices/{duration}", super},
	{"GET", "/images", all}, {"POST", "/images", super}, {"PUT", "/images/{id}", super},
	{"GET", "/audit", all}, {"GET", "/dashboard", all},

	// 计划 7 D8：super 维护目录（建 / 改 / 发布 / 下线）；ops 预拉取与强制卸载；finance 只读（含脚本原文——只在后台可见）
	{"GET", "/apps", all}, {"POST", "/apps", super}, {"PUT", "/apps/{id}", super}, {"DELETE", "/apps/{id}", super},
	{"GET", "/apps/{id}/versions", all}, {"POST", "/apps/{id}/versions", super}, {"PUT", "/apps/{id}/versions/{vid}", super},
	{"POST", "/apps/{id}/versions/{vid}/publish", super}, {"POST", "/apps/{id}/versions/{vid}/prepull", ops},
	{"GET", "/apps/{id}/versions/{vid}/script", all},
	{"GET", "/deployments", all}, {"GET", "/deployments/{dep}", all}, {"POST", "/deployments/{dep}/force-remove", ops},

	// 计划 9 Token 商城（规格 §10.2，按负责人裁定修订）：模型只读全员、改动 super（上下线另要 TOTP）；机器池 / 节点 ops（移除另要 TOTP）；
	// 账户与 Key 列表 ops + finance，改账户 / 停用 Key ops；用量、汇总、结算 finance；仪表盘卡片全员
	{"GET", "/llm/models", all}, {"POST", "/llm/models", super}, {"PUT", "/llm/models/{id}", super},
	{"POST", "/llm/models/{id}/status", super}, {"DELETE", "/llm/models/{id}", super},
	{"GET", "/llm/pool", ops}, {"GET", "/llm/nodes", ops}, {"GET", "/llm/nodes/{id}", ops},
	{"POST", "/llm/nodes/{id}/drain", ops}, {"POST", "/llm/nodes/{id}/rebuild", ops}, {"POST", "/llm/nodes/{id}/remove", ops},
	{"GET", "/llm/accounts", all}, {"PUT", "/llm/accounts/{user_id}", ops},
	{"GET", "/llm/keys", all}, {"POST", "/llm/keys/{id}/disable", ops},
	{"GET", "/llm/usage", finance}, {"GET", "/llm/usage/export", finance}, {"GET", "/llm/usage/daily", finance},
	{"GET", "/llm/settlements", finance}, {"GET", "/llm/summary", all},
	// 计划 11 D6：全部角色可读工作流目录 / 版本详情 / 安装列表；super 维护目录与版本、上传封面；发布 = super + 动态码
	{"GET", "/workflows", all}, {"POST", "/workflows", super}, {"GET", "/workflows/{id}", all}, {"PUT", "/workflows/{id}", super},
	{"DELETE", "/workflows/{id}", super}, {"GET", "/workflows/{id}/cover", all}, {"POST", "/workflows/{id}/cover", super},
	{"GET", "/workflows/{id}/installs", all},
	{"GET", "/workflows/{id}/versions", all}, {"POST", "/workflows/{id}/versions", super},
	{"GET", "/workflows/{id}/versions/{vid}", all}, {"PUT", "/workflows/{id}/versions/{vid}", super},
	{"POST", "/workflows/{id}/versions/{vid}/publish", super},
	// 计划 12 D8：AI 管家设置与用量全员可看、只有 super 改设置（不需动态码）；对话内容只给 ops
	{"GET", "/butler/settings", all}, {"PUT", "/butler/settings", super},
	{"GET", "/butler/usage", all}, {"GET", "/butler/usage/users", all},
	{"GET", "/butler/conversations", ops}, {"GET", "/butler/conversations/{id}", ops},
	// 计划 13 §10.2：集群网络（fabric）与集群全员可看；改 fabric、延长 / 重新下发集群 ops（不要求动态码，与实例 extend 一致）
	{"GET", "/fabrics", all}, {"POST", "/fabrics", ops}, {"PUT", "/fabrics/{id}", ops}, {"DELETE", "/fabrics/{id}", ops},
	{"GET", "/clusters", all}, {"GET", "/clusters/{id}", all},
	{"POST", "/clusters/{id}/extend", ops}, {"POST", "/clusters/{id}/resync", ops}, {"POST", "/clusters/{id}/app/force-remove", ops},
}

func roleKey(roles []admin.Role) string {
	s := make([]string, len(roles))
	for i, r := range roles {
		s[i] = string(r)
	}
	sort.Strings(s)
	return strings.Join(s, "+")
}

func TestRouteTableMatchesPermissionMatrix(t *testing.T) {
	e := newEnv(t)
	got := map[string]string{}
	for _, r := range e.srv.Routes() {
		got[r.Method+" "+r.Pattern] = roleKey(r.Roles)
	}
	want := map[string]string{}
	for _, r := range expectedRoutes {
		want[r.method+" /admin/api/v1"+r.path] = roleKey(r.roles)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: roles = %q, want %q", k, got[k], v)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("unexpected protected route %s (add it to the permission matrix)", k)
		}
	}
}

// TestPermissionMatrix 用三个角色逐个请求每个接口：无权限必须是 403 forbidden，有权限则不能是 forbidden
// （参数不全导致的 400 / 404、需要二次验证的 403 totp_required 都算放行）。
func TestPermissionMatrix(t *testing.T) {
	e := newEnv(t)
	clients := map[admin.Role]*client{
		admin.RoleSuper:   e.loginAs("root", admin.RoleSuper),
		admin.RoleOps:     e.loginAs("ops", admin.RoleOps),
		admin.RoleFinance: e.loginAs("fin", admin.RoleFinance),
	}
	fill := strings.NewReplacer("{id}", "999999", "{user_id}", "999999", "{duration}", "1d", "*", "2026/01/abcdefghijklmnopqrstuvwxyz012345.png")
	for _, r := range expectedRoutes {
		path := fill.Replace(r.path)
		for role, c := range clients {
			var body any
			if r.method != "GET" {
				body = map[string]any{}
			}
			var code int
			var out map[string]any
			if r.method == "POST" && r.path == "/workflows/{id}/cover" { // 封面上传只收 multipart（字段名不对 → 有权角色 400）
				resp, o := c.upload(path, "x", "x.png", []byte("x"))
				code, out = resp.StatusCode, o
			} else {
				code, out = c.do(r.method, path, body)
			}
			allowed := slices.Contains(r.roles, role)
			name := fmt.Sprintf("%s %s as %s", r.method, path, role)
			switch {
			case allowed && codeOf(out) == "forbidden":
				t.Errorf("%s: allowed role got forbidden", name)
			case !allowed && (code != 403 || codeOf(out) != "forbidden"):
				t.Errorf("%s: want 403 forbidden, got %d %v", name, code, out)
			case code == 401 || code >= 500:
				t.Errorf("%s: unexpected %d %v", name, code, out)
			}
		}
	}
	if code, _ := e.anon().do("GET", "/dashboard", nil); code != 401 {
		t.Fatalf("anonymous dashboard = %d", code)
	}
}
