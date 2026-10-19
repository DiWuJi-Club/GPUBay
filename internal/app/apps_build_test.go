package app_test

import (
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/admin"
	"spark-cloud/internal/app"
	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/config"
	"spark-cloud/internal/platform/dbtest"
)

// TestBuildWiresApps：Build 组装应用部署（计划 7）——控制台 /apps 路由已注册、rental 钩子已接（带已上架应用的下单生成 queued 部署，
// 而不是 noop 钩子的 app_not_found）、后台仪表盘与实例详情不因 Apps 为 nil 而 500、Runner 能拿到咨询锁跑一轮。
func TestBuildWiresApps(t *testing.T) {
	e := newBookingEnv(t, 1)
	a := e.a
	if a.Apps == nil || a.AppCatalog == nil || a.AppRunner == nil {
		t.Fatalf("Apps / AppCatalog / AppRunner = %v / %v / %v", a.Apps, a.AppCatalog, a.AppRunner)
	}
	actor := audit.Actor{AdminID: 1}
	img := dbtest.ImageID(t, e.pool)
	jl, err := a.AppCatalog.CreateApp(e.ctx, actor, appdeploy.AppInput{Slug: "jupyterlab", Name: "JupyterLab", Summary: "笔记本",
		Category: "notebook", Icon: "notebook", Status: appdeploy.StatusPublished})
	if err != nil {
		t.Fatal(err)
	}
	v, err := a.AppCatalog.CreateVersion(e.ctx, actor, jl.ID, appdeploy.VersionInput{Version: "1.0.0", ImageIDs: []int64{img},
		Spec:   appdeploy.Spec{Ports: []appdeploy.PortSpec{{Port: 8888}}},
		Script: "#!/bin/sh\ncase \"$1\" in install) ;; start) sleep 1 & echo $! > \"$SPARK_APP_DIR/service.pid\" ;; esac\n"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.AppCatalog.PublishVersion(e.ctx, actor, jl.ID, v.ID); err != nil {
		t.Fatal(err)
	}

	// 控制台目录路由（srv.Apps 非 nil 才注册）
	items := e.c.do("GET", "/apps", nil)["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["slug"] != "jupyterlab" {
		t.Fatalf("GET /apps = %v", items)
	}
	// 下单预装：钩子已接 → 实例带一个 queued 部署
	order := e.c.do("POST", "/orders", map[string]any{"product_id": dbtest.ProductID(t, e.pool), "duration": "1d", "image_id": img, "app_id": jl.ID})
	inst := order["instance"].(map[string]any)
	instID := inst["id"].(string)
	var state string
	if err := e.pool.QueryRow(e.ctx, `SELECT state FROM app_deployments WHERE instance_id = $1 AND app_id = $2`, instID, jl.ID).Scan(&state); err != nil {
		t.Fatalf("deployment of the order: %v", err)
	}
	if state != "queued" {
		t.Fatalf("deployment state = %q, want queued", state)
	}
	if sum := inst["apps_summary"].([]any); len(sum) != 1 || sum[0].(map[string]any)["state"] != "queued" {
		t.Fatalf("apps_summary = %v", inst["apps_summary"])
	}
	if got := e.c.do("GET", "/instances/"+instID+"/apps", nil)["items"].([]any); len(got) != 1 {
		t.Fatalf("GET /instances/%s/apps = %v", instID, got)
	}

	// Runner 一轮：拿到咨询锁、不报错（实例还在开通，部署只是被推后）
	if _, locked, err := a.AppRunner.Tick(e.ctx); err != nil || !locked {
		t.Fatalf("runner tick: locked %v err %v", locked, err)
	}

	// 后台：仪表盘与实例详情（Apps 未接时 nil 指针 → 500）、应用目录路由（AppCatalog）
	adminTS := httptest.NewServer(a.AdminHandler)
	t.Cleanup(adminTS.Close)
	if _, err := a.Admins.Create(e.ctx, audit.Actor{}, "root", "correct horse battery", admin.RoleSuper); err != nil {
		t.Fatal(err)
	}
	jar, _ := cookiejar.New(nil)
	ac := &client{t: t, base: adminTS.URL + "/admin/api/v1", http: &http.Client{Jar: jar}}
	login := ac.do("POST", "/auth/login", map[string]string{"username": "root", "password": "correct horse battery"})
	u, _ := url.Parse(login["otpauth_uri"].(string))
	e.clk.Advance(30 * time.Second)
	code, _ := admin.TOTPCode(u.Query().Get("secret"), admin.TOTPStep(e.clk.Now()))
	ac.do("POST", "/auth/totp/setup", map[string]string{"code": code})
	if dash := ac.do("GET", "/dashboard", nil); dash["apps_installing"] != float64(1) {
		t.Fatalf("dashboard apps_installing = %v", dash["apps_installing"])
	}
	if apps := ac.do("GET", "/instances/"+instID, nil)["apps"].([]any); len(apps) != 1 {
		t.Fatalf("admin instance apps = %v", apps)
	}
	if got := ac.do("GET", "/apps", nil)["items"].([]any); len(got) != 1 {
		t.Fatalf("admin GET /apps = %v", got)
	}
}

// 测试注入的应用健康检查地址（把 run.sh 的探测指向假应用）绝不能用于生产。
func TestBuildRejectsAppHealthAddrInProd(t *testing.T) {
	cfg := config.Config{Env: "prod", Domain: "spark.test", SecretKey: dbtest.Key}
	_, err := app.Build(cfg, nil, discardLog(), app.Options{AppHealthAddr: func(int) string { return "127.0.0.1:1" }})
	if err == nil || !strings.Contains(err.Error(), "健康检查") {
		t.Fatalf("err = %v, want app health addr rejection", err)
	}
}

// 测试注入的应用根目录（假容器里按实例分目录）同样不能用于生产。
func TestBuildRejectsAppRootInProd(t *testing.T) {
	cfg := config.Config{Env: "prod", Domain: "spark.test", SecretKey: dbtest.Key}
	for name, opt := range map[string]app.Options{
		"AppRoot":    {AppRoot: "/tmp/x"},
		"AppRootFor": {AppRootFor: func(string) string { return "/tmp/x" }},
	} {
		if _, err := app.Build(cfg, nil, discardLog(), opt); err == nil || !strings.Contains(err.Error(), "应用根目录") {
			t.Fatalf("%s: err = %v, want app root rejection", name, err)
		}
	}
}
