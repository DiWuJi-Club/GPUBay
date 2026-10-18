package adminapi_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/admin"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/dbtest"
)

func TestPricesAndImagesEndpoints(t *testing.T) {
	e := newEnv(t)
	root := e.loginAs("root", admin.RoleSuper)
	pid := itoa(dbtest.ProductID(t, e.pool))
	if code, out := root.do("GET", "/products", nil); code != 200 || len(out["items"].([]any)) != 1 {
		t.Fatalf("products = %d %v", code, out)
	}
	if code, out := root.do("PUT", "/products/"+pid+"/prices/1d", map[string]any{"amount": 7900, "enabled": true}); code != 403 || codeOf(out) != "totp_required" {
		t.Fatalf("price without totp = %d %v", code, out)
	}
	if code, out := root.do("PUT", "/products/"+pid+"/prices/1d", map[string]any{"amount": 7900, "enabled": true, "totp": root.totp()}); code != 200 || out["amount"].(float64) != 7900 {
		t.Fatalf("price = %d %v", code, out)
	}
	if code, out := root.do("PUT", "/products/"+pid+"/prices/2h", map[string]any{"amount": 100, "enabled": true}); code != 400 {
		t.Fatalf("bad duration = %d %v", code, out)
	}
	code, out := root.do("POST", "/images", map[string]any{"name": "SGLang", "ref": "registry.spark.internal/base/sglang:0.5", "enabled": true, "sort": 40})
	if code != 201 {
		t.Fatalf("create image = %d %v", code, out)
	}
	id := itoa(int64(out["id"].(float64)))
	if code, out := root.do("PUT", "/images/"+id, map[string]any{"name": "SGLang", "ref": "registry.spark.internal/base/sglang:0.6", "enabled": false}); code != 200 || out["enabled"] != false {
		t.Fatalf("update image = %d %v", code, out)
	}
	if code, out := root.do("GET", "/images", nil); code != 200 || len(out["items"].([]any)) != 4 {
		t.Fatalf("images = %d %v", code, out)
	}
	ops := e.loginAs("ops", admin.RoleOps)
	// 读权限已对全部角色开放（B6）；写价格仍仅 super
	if code, out := ops.do("PUT", "/products/1/prices/1d", map[string]any{"amount": 100, "enabled": true}); code != 403 || codeOf(out) != "forbidden" {
		t.Fatalf("ops set price = %d %v", code, out)
	}
}

func TestAuditEndpoint(t *testing.T) {
	e := newEnv(t)
	root := e.loginAs("root", admin.RoleSuper)
	fin := e.loginAs("fin", admin.RoleFinance)
	if code, out := fin.do("GET", "/audit?action=admin.totp_bind", nil); code != 200 || out["total"].(float64) != 2 {
		t.Fatalf("audit = %d %v", code, out)
	}
	if code, out := fin.do("GET", "/audit?actor_id="+itoa(root.admin.ID)+"&action=admin.totp_bind", nil); code != 200 || out["total"].(float64) != 1 {
		t.Fatalf("audit by actor = %d %v", code, out)
	}
	if code, _ := fin.do("GET", "/audit?from=bad", nil); code != 400 {
		t.Fatalf("bad from = %d", code)
	}
	// 操作人名称：root 的操作带用户名；命令行 / 系统写入的记录为空串
	if code, out := root.do("POST", "/images", map[string]any{"name": "审计镜像", "ref": "registry.spark.internal/base/audit:1", "enabled": true, "sort": 50}); code != 201 {
		t.Fatalf("create image = %d %v", code, out)
	}
	code, out := fin.do("GET", "/audit?action=image.create", nil)
	items := out["items"].([]any)
	if code != 200 || len(items) != 1 || items[0].(map[string]any)["actor_name"] != "root" {
		t.Fatalf("audit actor_name = %d %v", code, out)
	}
	code, out = fin.do("GET", "/audit?action=admin.create", nil)
	if code != 200 || out["items"].([]any)[0].(map[string]any)["actor_name"] != "" {
		t.Fatalf("system entry actor_name = %d %v, want empty", code, out)
	}
	// B6：ops / finance 也能读商品与镜像
	ops := e.loginAs("ops2", admin.RoleOps)
	if code, _ := ops.do("GET", "/products", nil); code != 200 {
		t.Fatalf("ops products = %d", code)
	}
	if code, _ := fin.do("GET", "/images", nil); code != 200 {
		t.Fatalf("finance images = %d", code)
	}
	if code, out := fin.do("POST", "/images", map[string]any{"name": "x", "ref": "r", "enabled": true, "sort": 1}); code != 403 || codeOf(out) != "forbidden" {
		t.Fatalf("finance create image = %d %v, want forbidden", code, out)
	}
}

func TestImageEnableBroadcastsPull(t *testing.T) {
	e := newEnv(t)
	root := e.loginAs("root", admin.RoleSuper)
	online := e.machine("SN-ON")
	off := e.machine("SN-OFF")
	_, _ = e.pool.Exec(e.ctx, `UPDATE machines SET agent_online = false WHERE id = $1`, off)
	code, out := root.do("POST", "/images", map[string]any{"name": "cuda", "ref": "nvcr.io/x/cuda:13", "enabled": true, "sort": 1})
	if code != 201 {
		t.Fatalf("create = %d %v", code, out)
	}
	cmds, _ := e.fleet.PendingCommands(e.ctx, online)
	if len(cmds) != 1 || cmds[0].Type != fleet.CmdPullImage || !strings.Contains(string(cmds[0].Payload), "nvcr.io/x/cuda:13") {
		t.Fatalf("pull = %+v", cmds)
	}
	if cmds, _ := e.fleet.PendingCommands(e.ctx, off); len(cmds) != 0 {
		t.Fatal("offline machine got pull")
	}
	id := int64(out["id"].(float64))
	// 停用不广播；再启用广播
	if code, _ := root.do("PUT", fmt.Sprintf("/images/%d", id), map[string]any{"name": "cuda", "ref": "nvcr.io/x/cuda:13", "enabled": false, "sort": 1}); code != 200 {
		t.Fatal(code)
	}
	if cmds, _ := e.fleet.PendingCommands(e.ctx, online); len(cmds) != 1 {
		t.Fatalf("disable broadcast: %+v", cmds)
	}
	e.clk.Advance(time.Second) // 命令按 created_at, uuid 排序；固定时钟下同刻并列会乱序
	if code, _ := root.do("PUT", fmt.Sprintf("/images/%d", id), map[string]any{"name": "cuda", "ref": "nvcr.io/x/cuda:14", "enabled": true, "sort": 1}); code != 200 {
		t.Fatal(code)
	}
	if cmds, _ := e.fleet.PendingCommands(e.ctx, online); len(cmds) != 2 || !strings.Contains(string(cmds[1].Payload), "cuda:14") {
		t.Fatalf("enable broadcast: %+v", cmds)
	}
}
