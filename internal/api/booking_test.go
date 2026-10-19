package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/api"
	"spark-cloud/internal/audit"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/dbtest"
)

// fundedClient 登录、实名并充值 ¥10,000。
func (e *testEnv) fundedClient(phone string) (*client, int64) {
	e.t.Helper()
	c, uid := e.verifiedClient(phone)
	if _, err := e.wallet.AdminAdjust(context.Background(), 1, uid, 1_000_000, "测试充值", ""); err != nil {
		e.t.Fatal(err)
	}
	return c, uid
}

// machineOf 从数据库读实例所在机器（实例视图不透出机器 ID）。
func (e *testEnv) machineOf(instanceID string) int64 {
	e.t.Helper()
	var id int64
	if err := e.pool.QueryRow(context.Background(), `SELECT machine_id FROM instances WHERE id = $1`, instanceID).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func TestBookingEndpoints(t *testing.T) {
	e := newTestEnv(t)
	m1 := dbtest.CreateMachine(t, e.pool, "SN001")
	m2 := dbtest.CreateMachine(t, e.pool, "SN002")
	g, err := e.fleet.CreateGroup(context.Background(), audit.Actor{AdminID: 1}, fleet.GroupInput{Name: "A 区", Region: "一楼", Visible: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.fleet.AssignMachines(context.Background(), audit.Actor{AdminID: 1}, g.ID, []int64{m1, m2}); err != nil {
		t.Fatal(err)
	}
	anon := e.client()
	code, out := anon.do("GET", "/groups", nil)
	if code != 200 || len(out["items"].([]any)) != 1 || out["items"].([]any)[0].(map[string]any)["idle_now"].(float64) != 2 || out["now"] == nil {
		t.Fatalf("anonymous groups = %d %v", code, out)
	}
	code, out = anon.do("GET", "/groups/"+itoa(g.ID)+"/calendar?days=3", nil)
	if code != 200 || len(out["days"].([]any)) != 3 {
		t.Fatalf("calendar = %d %v", code, out)
	}
	start := dbtest.T0.Add(3 * time.Hour).Format(time.RFC3339)
	code, out = anon.do("GET", "/availability?product_id="+itoa(dbtest.ProductID(t, e.pool))+"&group_id="+itoa(g.ID)+"&start_at="+start+"&duration=1d&quantity=1", nil)
	if code != 200 || out["available"].(float64) != 2 {
		t.Fatalf("availability = %d %v", code, out)
	}
	if code, _ := anon.do("POST", "/bookings", map[string]any{}); code != 401 {
		t.Fatalf("anonymous booking = %d", code)
	}

	c, _ := e.fundedClient("13800000010")
	body := map[string]any{"product_id": dbtest.ProductID(t, e.pool), "image_id": dbtest.ImageID(t, e.pool), "group_id": g.ID,
		"count": 2, "duration": "1d", "quantity": 1, "start_at": start, "auto_renew": true, "auto_renew_duration": "1w"}
	code, out = c.doH("POST", "/bookings", body, map[string]string{"Idempotency-Key": "b-1"})
	if code != 201 {
		t.Fatalf("booking = %d %v", code, out)
	}
	batch := out["batch"].(map[string]any)
	insts := out["instances"].([]any)
	if batch["count"].(float64) != 2 || len(insts) != 2 || len(out["orders"].([]any)) != 2 {
		t.Fatalf("booking result = %v", out)
	}
	first := insts[0].(map[string]any)
	id := first["id"].(string)
	if first["state"] != "reserved" || first["scheduled_start_at"] == nil || first["group_name"] != "A 区" || first["machine_label"] == "" ||
		first["auto_renew"] != true || first["auto_renew_duration"] != "1w" || first["batch_id"] != batch["id"] || first["cancellable_until"] == nil {
		t.Fatalf("instance view = %v", first)
	}
	if code, again := c.doH("POST", "/bookings", body, map[string]string{"Idempotency-Key": "b-1"}); code != 201 || again["batch"].(map[string]any)["id"] != batch["id"] {
		t.Fatalf("replay = %d %v", code, again)
	}
	if code, out := c.do("GET", "/bookings/"+batch["id"].(string), nil); code != 200 || len(out["instances"].([]any)) != 2 {
		t.Fatalf("get batch = %d %v", code, out)
	}
	if code, out := c.do("GET", "/instances/"+id, nil); code != 200 || len(out["renew_options"].([]any)) != 3 || out["scheduled_end_at"] == nil {
		t.Fatalf("instance = %d %v", code, out)
	}
	if code, out := c.do("GET", "/instances/"+id+"/events", nil); code != 200 || len(out["items"].([]any)) != 1 {
		t.Fatalf("events = %d %v", code, out)
	}
	if code, out := c.do("PUT", "/instances/"+id+"/auto-renew", map[string]any{"enabled": false, "duration": "1d"}); code != 200 || out["auto_renew"] != false {
		t.Fatalf("auto-renew = %d %v", code, out)
	}
	// 关闭时档位可留空，保留原档位
	if code, out := c.do("PUT", "/instances/"+id+"/auto-renew", map[string]any{"enabled": false}); code != 200 || out["auto_renew_duration"] != "1d" {
		t.Fatalf("auto-renew off without duration = %d %v", code, out)
	}
	if code, out := c.do("POST", "/instances/"+id+"/renew", map[string]any{"duration": "1d", "quantity": 2}); code != 200 || out["order"].(map[string]any)["quantity"].(float64) != 2 {
		t.Fatalf("renew x2 = %d %v", code, out)
	}
	code, out = c.do("GET", "/instances/"+id+"/cancel-quote", nil)
	if code != 200 || out["percent"].(float64) != 100 || out["refund"].(float64) != 3*6900 || out["max"].(float64) != 3*6900 || out["deadline"] == nil {
		t.Fatalf("cancel-quote = %d %v", code, out)
	}
	// 别人的实例：与不存在同样 404
	other, _ := e.fundedClient("13800000019")
	for _, p := range []struct{ method, path string }{{"GET", "/instances/" + id + "/cancel-quote"}, {"POST", "/instances/" + id + "/cancel"},
		{"GET", "/instances/" + id + "/events"}, {"PUT", "/instances/" + id + "/auto-renew"}, {"GET", "/bookings/" + batch["id"].(string)}} {
		var b any
		if p.method != "GET" {
			b = map[string]any{"enabled": false}
		}
		if code, out := other.do(p.method, p.path, b); code != 404 {
			t.Fatalf("other user %s %s = %d %v", p.method, p.path, code, out)
		}
	}
	if code, out := c.do("POST", "/instances/"+id+"/cancel", map[string]any{}); code != 200 || out["state"] != "cancelled" || len(out["renew_options"].([]any)) != 0 {
		t.Fatalf("cancel = %d %v", code, out)
	}
	if code, out := c.do("POST", "/instances/"+id+"/cancel", map[string]any{}); code != 409 || out["code"] != "invalid_state" {
		t.Fatalf("cancel twice = %d %v", code, out)
	}
	// 参数错误码透传
	bad := map[string]any{"product_id": dbtest.ProductID(t, e.pool), "image_id": dbtest.ImageID(t, e.pool), "count": 1, "duration": "1d", "quantity": 1,
		"start_at": dbtest.T0.Add(3*time.Hour + 30*time.Minute).Format(time.RFC3339)}
	if code, out := c.do("POST", "/bookings", bad); code != 400 || out["code"] != "booking_not_hour" {
		t.Fatalf("not hour = %d %v", code, out)
	}
	bad["start_at"] = "yesterday"
	if code, out := c.do("POST", "/bookings", bad); code != 400 || out["code"] != "bad_request" {
		t.Fatalf("bad time = %d %v", code, out)
	}
	// 市场 idle_count 现在是「此刻可即时租用」：两台机器都有 3 小时后的预订（一台已取消）→ 1 台可即时租 1 天
	code, out = anon.do("GET", "/products", nil)
	if code != 200 || out["items"].([]any)[0].(map[string]any)["idle_count"].(float64) != 1 {
		t.Fatalf("idle_count = %d %v", code, out)
	}
	// 机器日历：匿名也能看，但不带实例 ID 与用户 ID（看仍有预订的那台：insts[1] 所在机器）
	booked := e.machineOf(insts[1].(map[string]any)["id"].(string))
	code, out = anon.do("GET", "/machines/"+itoa(booked)+"/calendar?days=5", nil)
	periods := out["periods"].([]any)
	if code != 200 || len(periods) != 1 || periods[0].(map[string]any)["instance_id"] != nil || periods[0].(map[string]any)["user_id"] != nil ||
		periods[0].(map[string]any)["mine"] != false {
		t.Fatalf("machine calendar = %d %v", code, out)
	}
	// 本人带会话看：标 mine
	if code, out := c.do("GET", "/machines/"+itoa(booked)+"/calendar?days=5", nil); code != 200 || out["periods"].([]any)[0].(map[string]any)["mine"] != true {
		t.Fatalf("own machine calendar = %d %v", code, out)
	}
}

func TestAnonymousEndpointsHideInternals(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	m1 := dbtest.CreateMachine(t, e.pool, "SN-SECRET-1")
	m2 := dbtest.CreateMachine(t, e.pool, "SN-SECRET-2")
	if _, err := e.pool.Exec(ctx, `UPDATE machines SET lan_ip = '10.9.8.7' WHERE id = $1`, m1); err != nil {
		t.Fatal(err)
	}
	vis, err := e.fleet.CreateGroup(ctx, audit.Actor{AdminID: 1}, fleet.GroupInput{Name: "公开", Visible: true})
	if err != nil {
		t.Fatal(err)
	}
	hid, err := e.fleet.CreateGroup(ctx, audit.Actor{AdminID: 1}, fleet.GroupInput{Name: "隐藏区", Visible: false})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.fleet.AssignMachines(ctx, audit.Actor{AdminID: 1}, vis.ID, []int64{m1}); err != nil {
		t.Fatal(err)
	}
	if err := e.fleet.AssignMachines(ctx, audit.Actor{AdminID: 1}, hid.ID, []int64{m2}); err != nil {
		t.Fatal(err)
	}
	anon := e.client()
	for _, p := range []string{"/groups", "/groups/" + itoa(vis.ID) + "/machines", "/groups/" + itoa(vis.ID) + "/calendar?days=2",
		"/machines/" + itoa(m1) + "/calendar?days=2"} {
		resp, err := http.Get(e.ts.URL + "/api/v1" + p)
		if err != nil {
			t.Fatal(err)
		}
		var raw json.RawMessage
		_ = json.NewDecoder(resp.Body).Decode(&raw)
		resp.Body.Close()
		s := string(raw)
		if resp.StatusCode != 200 || strings.Contains(s, "SN-SECRET") || strings.Contains(s, "10.9.8.7") || strings.Contains(s, "隐藏区") ||
			strings.Contains(s, "user_id") || strings.Contains(s, "serial") {
			t.Fatalf("%s = %d %s", p, resp.StatusCode, s)
		}
		if resp.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("%s cache-control = %q", p, resp.Header.Get("Cache-Control"))
		}
	}
	// 隐藏机组与其中的机器按不存在处理
	for _, p := range []string{"/groups/" + itoa(hid.ID) + "/machines", "/groups/" + itoa(hid.ID) + "/calendar", "/machines/" + itoa(m2) + "/calendar",
		"/machines/999999/calendar"} {
		if code, out := anon.do("GET", p, nil); code != 404 {
			t.Fatalf("%s = %d %v", p, code, out)
		}
	}
	if code, out := anon.do("GET", "/availability?product_id="+itoa(dbtest.ProductID(t, e.pool))+"&group_id="+itoa(hid.ID)+"&duration=1d", nil); code != 404 {
		t.Fatalf("availability hidden group = %d %v", code, out)
	}
	if code, out := anon.do("GET", "/availability?product_id="+itoa(dbtest.ProductID(t, e.pool))+"&group_id="+itoa(vis.ID)+"&duration=1d&quantity=x", nil); code != 400 || out["code"] != "bad_request" {
		t.Fatalf("availability bad quantity = %d %v", code, out)
	}
	// 指定机器下单：机组不对、机组隐藏、机器不存在一律同一个 404，探测不出隐藏机器属于哪个机组
	c, _ := e.fundedClient("13800000077")
	start := dbtest.T0.Add(3 * time.Hour).Format(time.RFC3339)
	body := func(machine int64, group *int64) map[string]any {
		b := map[string]any{"product_id": dbtest.ProductID(t, e.pool), "image_id": dbtest.ImageID(t, e.pool), "count": 1, "duration": "1d",
			"quantity": 1, "start_at": start, "machine_id": machine}
		if group != nil {
			b["group_id"] = *group
		}
		return b
	}
	for name, b := range map[string]map[string]any{
		"hidden machine, hidden group":  body(m2, &hid.ID),
		"hidden machine, visible group": body(m2, &vis.ID),
		"visible machine, hidden group": body(m1, &hid.ID),
		"visible machine, no group":     body(m1, nil),
		"missing machine":               body(999999, &vis.ID),
	} {
		if code, out := c.do("POST", "/bookings", b); code != 404 || out["code"] != "not_found" {
			t.Fatalf("%s = %d %v", name, code, out)
		}
	}
	if code, out := c.do("POST", "/bookings", body(m1, &vis.ID)); code != 201 {
		t.Fatalf("visible machine in its group = %d %v", code, out)
	}
}

func TestCalendarEndpointsRateLimitedPerIP(t *testing.T) {
	e := newTestEnvWith(t, func(s *api.Server) { s.CalendarLimit = 2 })
	anon := e.client()
	for i := 0; i < 2; i++ {
		if code, _ := anon.do("GET", "/groups", nil); code != 200 {
			t.Fatalf("call %d = %d", i, code)
		}
	}
	if code, out := anon.do("GET", "/groups", nil); code != http.StatusTooManyRequests || out["code"] != "rate_limited" {
		t.Fatalf("third call = %d %v", code, out)
	}
}

// 终审 M1：机组列表 / 机组机器 / 机组日历按 product_id 只算该商品的机器（机组可跨商品，规格 §3.1）；不带 product_id 时不过滤。
func TestGroupEndpointsFilterByProduct(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	p1 := dbtest.ProductID(t, e.pool)
	var p2 int64
	if err := e.pool.QueryRow(ctx, `INSERT INTO products (code, name, created_at, updated_at) VALUES ('spark-2', '另一商品', now(), now()) RETURNING id`).Scan(&p2); err != nil {
		t.Fatal(err)
	}
	m1 := dbtest.CreateMachine(t, e.pool, "SN-P1")
	m2 := dbtest.CreateMachine(t, e.pool, "SN-P2")
	if _, err := e.pool.Exec(ctx, `UPDATE machines SET product_id = $1 WHERE id = $2`, p2, m2); err != nil {
		t.Fatal(err)
	}
	g, err := e.fleet.CreateGroup(ctx, audit.Actor{AdminID: 1}, fleet.GroupInput{Name: "混合区", Visible: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.fleet.AssignMachines(ctx, audit.Actor{AdminID: 1}, g.ID, []int64{m1, m2}); err != nil {
		t.Fatal(err)
	}
	anon := e.client()
	for _, tc := range []struct {
		q    string
		want float64
	}{{"?product_id=" + itoa(p1), 1}, {"?product_id=" + itoa(p2), 1}, {"", 2}} {
		code, out := anon.do("GET", "/groups"+tc.q, nil)
		row := out["items"].([]any)[0].(map[string]any)
		if code != 200 || row["machine_count"].(float64) != tc.want || row["idle_now"].(float64) != tc.want {
			t.Fatalf("groups%s = %d %v", tc.q, code, out)
		}
		code, out = anon.do("GET", "/groups/"+itoa(g.ID)+"/machines"+tc.q, nil)
		if items := out["items"].([]any); code != 200 || float64(len(items)) != tc.want {
			t.Fatalf("machines%s = %d %v", tc.q, code, out)
		}
		sep := "?"
		if tc.q != "" {
			sep = "&"
		}
		code, out = anon.do("GET", "/groups/"+itoa(g.ID)+"/calendar"+tc.q+sep+"days=2", nil)
		day := out["days"].([]any)[0].(map[string]any)
		if code != 200 || day["total"].(float64) != tc.want || day["free"].(float64) != tc.want {
			t.Fatalf("calendar%s = %d %v", tc.q, code, out)
		}
	}
	if items := func() []any {
		_, out := anon.do("GET", "/groups/"+itoa(g.ID)+"/machines?product_id="+itoa(p1), nil)
		return out["items"].([]any)
	}(); items[0].(map[string]any)["id"].(float64) != float64(m1) {
		t.Fatalf("machines of p1 = %v", items)
	}
	for _, p := range []string{"/groups?product_id=x", "/groups/" + itoa(g.ID) + "/machines?product_id=0", "/groups/" + itoa(g.ID) + "/calendar?product_id=-1"} {
		if code, out := anon.do("GET", p, nil); code != 400 || out["code"] != "bad_request" {
			t.Fatalf("%s = %d %v", p, code, out)
		}
	}
}

// 终审 M2：匿名可用性查询带 machine_id 时与指定机器下单同一校验（机器在可见机组、group_id 与之一致），
// 未分组 / 隐藏机组 / 机组不对 / 不存在一律 404，探测不出未上架机器的空闲情况。
func TestAvailabilityMachineVisibility(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	vm := dbtest.CreateMachine(t, e.pool, "SN-VIS")
	hm := dbtest.CreateMachine(t, e.pool, "SN-HID")
	loose := dbtest.CreateMachine(t, e.pool, "SN-LOOSE")
	vis, _ := e.fleet.CreateGroup(ctx, audit.Actor{AdminID: 1}, fleet.GroupInput{Name: "公开", Visible: true})
	other, _ := e.fleet.CreateGroup(ctx, audit.Actor{AdminID: 1}, fleet.GroupInput{Name: "另一公开", Visible: true})
	hid, _ := e.fleet.CreateGroup(ctx, audit.Actor{AdminID: 1}, fleet.GroupInput{Name: "隐藏", Visible: false})
	if err := e.fleet.AssignMachines(ctx, audit.Actor{AdminID: 1}, vis.ID, []int64{vm}); err != nil {
		t.Fatal(err)
	}
	if err := e.fleet.AssignMachines(ctx, audit.Actor{AdminID: 1}, hid.ID, []int64{hm}); err != nil {
		t.Fatal(err)
	}
	anon := e.client()
	base := "/availability?product_id=" + itoa(dbtest.ProductID(t, e.pool)) + "&duration=1d&start_at=" + dbtest.T0.Add(3*time.Hour).Format(time.RFC3339)
	q := func(machine int64, group int64) string {
		p := base + "&machine_id=" + itoa(machine)
		if group != 0 {
			p += "&group_id=" + itoa(group)
		}
		return p
	}
	for name, p := range map[string]string{
		"ungrouped machine, no group":      q(loose, 0),
		"ungrouped machine, visible group": q(loose, vis.ID),
		"hidden machine, visible group":    q(hm, vis.ID),
		"visible machine, no group":        q(vm, 0),
		"visible machine, other group":     q(vm, other.ID),
		"missing machine":                  q(999999, vis.ID),
	} {
		if code, out := anon.do("GET", p, nil); code != 404 || out["code"] != "not_found" {
			t.Fatalf("%s = %d %v", name, code, out)
		}
	}
	if code, out := anon.do("GET", q(vm, vis.ID), nil); code != 200 || out["available"].(float64) != 1 {
		t.Fatalf("visible machine in its group = %d %v", code, out)
	}
}
