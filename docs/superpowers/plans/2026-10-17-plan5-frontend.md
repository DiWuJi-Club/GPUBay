# Spark Cloud 计划 5：前端（用户控制台与管理后台）Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让用户和运营团队不再靠 curl 使用平台：交付用户控制台（`web/console`，手机与电脑同等精心）、管理后台（`web/admin`，电脑端）、共用层（`web/shared`），由 Go 二进制 embed 托管；配套后端改动 B1～B11；端到端测试跑在真实后端（`cmd/e2e-server`：embedded-postgres + mock 插件 + 假时钟）与本机 Chrome 上，设计 §7.5 覆盖表每一行都有通过的 Playwright 用例，`make check` 一条命令全绿。

**Architecture:** npm workspaces（`shared` / `console` / `admin`），Vue 3 + Element Plus 按需引入 + 自写响应式（断点 768px），`fetch` 封装统一处理错误码、401、后台 `totp_required` 自动重发。构建产物输出到 `internal/web/dist/{console,admin}`，由新包 `internal/web` embed 并做单页应用回退，分别挂在 `:8080` 的 `/` 与 `:8081` 的 `/admin/`，与各自 API 同源复用 Cookie。端到端环境是独立 `main` 包 `cmd/e2e-server`：复用 `app.Build` 与 `App.Servers`，第三个监听 `127.0.0.1:18090` 提供测试钩子（推进假时钟、模拟 Agent、造数据）；生产二进制 `cmd/platform` 不含任何钩子代码。

**Tech Stack:** Go 1.27（`~/.local/go/bin`）、chi v5、embedded-postgres；Node 22 / npm 10（`~/.local/bin`）；vue 3.5.44、vue-router 5.4.0、element-plus 2.14.7、unplugin-vue-components 32.1.0、unplugin-auto-import 21.1.0、qrcode 1.5.4、vite 8.3.4、@vitejs/plugin-vue 6.0.10、typescript ~5.9.3、vue-tsc 3.3.13、vitest 5.0.5、@vue/test-utils 2.5.1、jsdom 30.1.3、@playwright/test 1.64.1（本机 Chrome，`channel: 'chrome'`）。

**Spec:** `docs/superpowers/specs/2026-10-17-plan5-frontend-design.md`（总设计 `docs/superpowers/specs/2026-10-14-spark-cloud-mvp-design.md`；计划 4 设计 `docs/superpowers/specs/2026-10-15-plan4-admin-payments-design.md`）

**分支：** `feat/plan5-frontend`（基于计划 4 分支，所有后端 API 已就绪；设计文档已提交 e45ba40）。

**本计划不包含：** 实例端口管理 / Web 访问 / GPU 小图 / SSH 公钥（计划 2、3）；优惠券、发票、工单、站内信、档期预订、自动续费；国际化、深色模式、PWA；ESLint / Prettier；后台手机适配。

## Global Constraints

- 仓库根目录 `/Users/rifuyiri/Documents/claude/机房搭建/spark-cloud`；所有命令前置 `export PATH=$HOME/.local/go/bin:$HOME/.local/bin:$PATH`。Go 模块名 `spark-cloud`。
- 前端依赖版本固定（设计 §1.2），`package-lock.json` 入库；根 `package.json` 的 `overrides.typescript = "~5.9.3"`（TS 7 与 vue-tsc 不兼容）。不引入 Pinia、ECharts、axios、dayjs 时区插件，不引入任何第三方 CDN / 字体 / 统计脚本。
- Element Plus 按需引入（`unplugin-vue-components` + `unplugin-auto-import`），路由全部懒加载；首屏目标约 90KB gzip。
- 金额一律是**分**（整数）；显示用 `fmtYuan`：`¥69.00`、`¥1,599.00`、`-¥10.00`。时间一律按北京时间显示 `yyyy-MM-dd HH:mm`（`Intl.DateTimeFormat('zh-CN', { timeZone: 'Asia/Shanghai', hour12: false })`）。
- 控制台断点 768px：≥ 768 左侧菜单 + 表格；< 768 顶栏 + 底部四个标签（算力市场 / 我的实例 / 钱包 / 我的）+ 卡片，可点元素 ≥ 44px，输入框字号 16px。手机确认框为底部弹出（`el-dialog` 自定义类 `sheet`），表单弹窗 `fullscreen`；电脑端居中宽 420 / 560。后台只按 ≥ 1200px 设计。
- 充值渠道按 User-Agent（`isMobileUA()`）而不是屏幕宽度：手机只给支付宝；电脑显示 `channels` 里有的渠道。
- 接口类型手写在 `shared/src/types/`，字段名与 Go JSON 标签一致（snake_case）。
- 会话只用后端 HttpOnly Cookie，前端不保存 token；`localStorage` 只存界面偏好（后台侧边栏折叠）。不用 `v-html`；`redirect` 参数只接受以 `/` 开头且不以 `//` 开头的站内路径。身份证输入框 `autocomplete="off"`。SSH 密码只在点击时请求、只放内存。
- 静态托管响应头（B1）：`Content-Security-Policy: default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; font-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; object-src 'none'; base-uri 'self'; form-action 'self'`、`X-Content-Type-Options: nosniff`、`X-Frame-Options: DENY`、`Referrer-Policy: same-origin`。`index.html` 不加任何内联 `<script>`。
- 下单与续费请求必须带 `Idempotency-Key`（`crypto.randomUUID()`，36 字符 ≤ 后端上限 64）。
- `app.Options.DisableRateLimits` 只有 `cmd/e2e-server` 设置，没有任何环境变量能打开；`cmd/platform` 不包含钩子代码；钩子只监听 `127.0.0.1`。
- 端到端测试规范（设计 §7.3）：用例标题以覆盖表 ID 开头；`data-testid` 命名 `页面-元素`；不用 CSS 类名定位业务元素（`.el-dialog`、`.is-fullscreen`、`.sheet` 只用于响应式断言）；禁止 `waitForTimeout`（截图前除外）；轮询逻辑用 `page.clock`；`workers: 1`、`fullyParallel: false`；每个测试自行登录、自行造数据；`web/e2e/coverage.json` 与设计 §7.5 一一对应，`npm run e2e:coverage` 核对无缺失、无多余、无 `skip / fixme / only`。
- 控制台每个 C- 用例在 `console-desktop` 与 `console-mobile` 两个 project 都执行并通过；后台 A- 用例在 `admin` project 通过。
- 控制台视图里请求失败的提示一律走 `notifyError(e)`（Task 8 `src/logic/notify.ts`，对 `unauthorized` / `cancelled` 静默），不直接 `ElMessage.error(messageOf(e))`；后台视图同理用各自的 `report(e)`（对 `cancelled` 静默）或 `notifyError`。
- 端到端断言不得落在 Element Plus 的包装元素上：`getByRole('dialog')` 命中的是 `.el-overlay-dialog` 包装层（只用于可见 / 隐藏判断），`is-fullscreen`、`sheet` 等类与 `data-testid` 都在内层 `.el-dialog` 上——响应式与类名断言必须用传给 `AppDialog` 的 `data-testid` 定位。
- 每个任务结束前必须实际运行并通过：`go vet ./... && go test ./...`；从 Task 5 起再加 `cd web && npm test`（Vitest）与 `npm run build`（`vue-tsc --noEmit && vite build`）；页面任务再加该任务列出的 Playwright 用例。任务完成的证据是命令输出，不是「看起来能用」。
- 提交信息结尾空一行加 `Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>`。

## Review Focus

1. **粘贴进来的手机号 / 验证码带空格或全角数字**（从微信复制「138 0000 1234」、输入法打出「０００００0」）：去掉空白、全角转半角后再校验与提交，而不是直接报「手机号格式不正确」→ Task 8 `console/src/logic/__tests__/login.spec.ts` 的 `normalizePhone` / `normalizeCode` 用例。
2. **多个请求同时收到 401**（钱包页同时请求余额、流水、订单，会话过期时三个都 401）：只提示一次「请先登录」、只跳转一次，而不是三条提示叠在一起 → Task 8 `console/src/store/__tests__/session.spec.ts` 的 `handleUnauthorized 连续调用只跳转一次`。
3. **自定义充值金额的边缘写法**（`" 88.50 "`、`"100."`、`".5"`、`"1e3"`、`"１００"`）：首尾空白可接受，其余一律显示「金额格式不正确」，绝不被 `Number()` 静默解析成别的数 → Task 5 `shared/src/__tests__/format.spec.ts` 的 `yuanToCents 边缘写法`。
4. **浏览器把后台标签页的定时器节流**（用户切走 10 分钟再切回来）：倒计时按真实流逝时间一次跳到正确值，而不是按「每 tick 减 1」慢了 10 分钟 → Task 5 `useCountdown 按时间源而不是 tick 数递减`。
5. **双击「确定释放」/「确认」等非幂等按钮**：第二次点击不发第二个请求（按钮在请求进行中 loading 禁用），用户不会看到一个成功一个「当前状态不允许该操作」→ Task 10 C-DET-06 内「双击确定释放只发出一个 `/release` 请求」断言。

## 已做出的裁定（规格未写明或与代码核对后确定的细节）

- **被封禁的已登录用户**：后端 `identity.Ban` 会删除该用户全部会话，所以「已登录用户被封禁后下一次请求」收到的是 401 `unauthorized`（不是 403 `banned`），前端走统一的 401 流程：清会话、跳登录页、提示「请先登录」。登录时才会收到 403 `banned` → 提示「账号已被封禁」。C-AUTH-07 按此断言。
- **`/me` 探测三态**：`session.status` 为 `'unknown' | 'anon' | 'authed'`；应用启动只探测一次（`quiet: true`），匿名访客 401 不提示、不跳转。
- **静态处理器可注入**：`api.Server` 与 `adminapi.Server` 增加字段 `Static http.Handler`（为 nil 时分别用 `web.Console()` / `web.Admin()`），Go 单测用 `fstest.MapFS` 构造的处理器验证路由级行为，不依赖已构建的产物。
- **后台路由器的 `NoStore` 下沉**：`httpx.NoStore` 从全局中间件移到 `/admin/api/v1` 分组，静态文件才能带自己的 `Cache-Control`（设计 §3.1 要求 API 保留 `NoStore`，静态文件不加）。
- **用户端未知 `/api/*` 的 JSON 404**：用 chi `r.NotFound`：路径以 `/api/` 开头返回 `{code: "not_found"}`，其余交给 SPA 处理器；`/api/v1` 子路由继承父路由的 NotFound。
- **`web/` 放空 `go.mod`**（`module spark-cloud/web`）：预先把 `node_modules` 从主模块排除，避免 `go vet ./...` 遍历数十万文件并可能撞上第三方包里的 `.go` 文件（设计 §11 允许）。
- **Vitest 用根配置的多项目**：`web/vitest.config.ts` 的 `test.projects` 列出 `shared`、`console`、`admin`（jsdom）与 `e2e-helpers`（node，测 `totp.ts` 与覆盖表核对脚本）；`npm test` 一次跑完。
- **同一手机号 60 秒只能发一次验证码，而假时钟永不自然流逝**：端到端 helper `loginConsole()` 在点击「发送验证码」前先 `hooks.advance(61)`；一个测试里如果既用 `userApi()`（API 登录，也要发码）又用界面登录，靠这一步避免 `rate_limited`。
- **测试造数的唯一名字带启动戳**：手机号 `139 + 启动时间戳后 4 位 + 4 位自增`（设计原文）；管理员用户名 `e2e_<戳>_<自增>`、机器序列号 `E2E-<戳>-<自增>`——数据库在整轮测试里共享，worker 重启后自增归零，不带戳会撞 `duplicate_username` / `duplicate_serial`。
- **`hooks/users` 的实现**：用户行直接 `INSERT INTO users`（与 `dbtest.CreateUser` 同一条 SQL，`verified` 时 `kyc_status='verified', kind='personal'`）；余额与分页流水都走 `Wallet.AdminAdjust(operatorID=0, remark='e2e-seed-balance' / 'e2e-seed-<i>', ip='127.0.0.1')`，因此 C-WAL-01 看到的第一条流水是「调账 +¥1,000.00」。
- **`useCountdown` 的形态**：`useCountdown({ now?, onZero? })` 返回 `{ remaining, start(seconds), stop() }`，剩余值 = `start 时的秒数 − floor((now() − start 时刻) / 1000)`，每秒重算，不做「每 tick 减 1」。
- **`createHttp` 多两个可选回调**：`onTotpSetupRequired`（后台收到 403 `totp_setup_required` 时跳登录页绑定步骤）与 `promptTotp(message?)` 的可选参数（`invalid_totp` 重弹时把错误文案带给弹框）；其余签名与设计 §2.1 一致。
- **`ApiError` 是 `class`**（`extends Error`，字段与设计接口一致，另加 `details: Record<string, unknown>` 保存完整错误响应体），方便 `instanceof` 与 Vitest 断言；带明细的错误（机器 CSV 导入的 `import_invalid.errors`）从 `details` 读取，不重放请求。
- **后台 TOTP 弹框是单例组件**：`admin/src/store/totp.ts` 导出 `promptTotp(message?)`，`App.vue` 挂一个 `TotpDialog`，并发请求共用同一个 Promise。
- **仪表盘柱状图的悬停提示**用 SVG `<title>`（原生提示，无额外组件），测试断言 `title` 文本。
- **企业执照预览**：`<img src="/admin/api/v1/files/<key>">`，`key` 以 `.pdf` 结尾直接显示「在新窗口打开 PDF」按钮；`onerror` 回退到「在新窗口打开」按钮（设计 §5.8）。
- **A-ERR-01 用真实角色变更触发 403，不用 `page.route` 改写请求**：super 打开机器页后，另一个 super 通过 API 把它降为 finance（`PUT /admins/{id}`；后端每次请求都从 `admins` 表重新读角色，降级不删会话），再点「查询」→ 真实的 403 `forbidden` → 页面提示「无权执行该操作」且不白屏。这是生产里会发生的路径（角色被改时会话仍在），比篡改请求 URL 可信。

## 文件结构

```
spark-cloud/
├── Makefile                              # 修改：web-install / web / web-test / e2e / check；build 依赖 web；test 含 web-test（Task 5）
├── .gitignore                            # 修改：internal/web/dist/{console,admin}/、web/node_modules/、web/test-results/、web/playwright-report/（Task 3）
├── cmd/platform/main.go                  # 修改：用 app.Servers（Task 1）
├── cmd/e2e-server/main.go                # 新增：端到端测试服务（Task 4）
├── internal/
│   ├── app/app.go                        # 修改：Options.DisableRateLimits、Servers()、SecureCookie（Task 1）
│   ├── api/{server,instances,recharges,auth}.go  # 修改：Static 挂载、product_name/image_name、remaining_seconds、GET /kyc（Task 2、3）
│   ├── adminapi/{server,catalog,ratelimit}.go    # 修改：LoginLimit、products/images 读权限、audit actor_name、Static 挂载（Task 1、2、3）
│   ├── identity/{service,auth}.go        # 修改：Deps.CodeIPHourLimit（Task 1）
│   ├── catalog/catalog.go                # 修改：ProductName / ImageName（Task 2）
│   ├── web/{web.go,web_test.go}          # 新增：embed + spaHandler + 安全头（Task 3）
│   ├── web/dist/README.md                # 新增：保证 embed 目录非空（Task 3）
│   └── e2ehooks/{hooks.go,kyc.go,hooks_test.go}  # 新增：钩子处理器（Task 4）
├── docs/superpowers/specs/2026-10-15-plan4-admin-payments-design.md  # 修改：B11 脚注（Task 2）
└── web/
    ├── go.mod                            # 空模块，把 node_modules 排除出 go vet ./...
    ├── package.json / package-lock.json / tsconfig.base.json / vitest.config.ts
    ├── playwright.config.ts
    ├── e2e/
    │   ├── coverage.json、coverage.ts     # 覆盖表 ID 与核对脚本
    │   ├── helpers/{hooks,totp,phone,money,console,admin}.ts
    │   ├── fixtures/{license.png,license.pdf,bad.gif,machines-ok.csv,machines-bad.csv}
    │   ├── console/{nav,auth,market,instances,instance-detail,wallet,recharge,kyc,account}.spec.ts
    │   └── admin/{auth,nav,dashboard,machines,pdus,alerts,instances,users,kyc,finance,catalog,admins,settings,audit}.spec.ts
    ├── shared/                            # @spark/shared
    │   ├── package.json、vitest.config.ts
    │   └── src/{brand,http,errors,format,device,idempotency,labels}.ts、types/*.ts、__tests__/*.spec.ts
    ├── console/
    │   ├── index.html、vite.config.ts、tsconfig.json、vitest.config.ts、public/brand/logo.svg
    │   └── src/{main.ts,App.vue,router.ts,api.ts,store/session.ts,layouts/ConsoleLayout.vue,
    │        components/AppDialog.vue,composables/{useGuide,usePolling}.ts,logic/{login,instance,recharge}.ts,
    │        views/{LoginView,MarketView,InstancesView,InstanceDetailView,WalletView,RechargeView,RechargeOrderView,KycView,AccountView,HelpView,NotFoundView}.vue}
    └── admin/
        ├── index.html、vite.config.ts、tsconfig.json、vitest.config.ts、public/brand/logo.svg
        └── src/{main.ts,App.vue,router.ts,api.ts,menu.ts,store/{session,totp}.ts,layouts/AdminLayout.vue,
             components/{TotpDialog,PageTable,DateRange,IdLink}.vue,
             views/{LoginView,DashboardView,MachinesView,MachineDetailView,PdusView,AlertsView,InstancesView,InstanceDetailView,
                    UsersView,UserDetailView,KycView,OrdersView,LedgerView,RechargesView,CatalogView,AdminsView,AuditView,SettingsView,ForbiddenView}.vue}
```

## 任务依赖

Task 1 → 2 → 3 → 4（后端配套与端到端服务，全部有 Go 测试）；5 → 6（共用层）；7（Playwright 环境与 helpers，依赖 4、6）；8 → 9 → 10 → 11 → 12 → 13 → 14（控制台，每个任务自带覆盖表用例）；15 → 16 → 17 → 18 → 19 → 20 → 21（后台）；22（收尾、文档、`make check`）。按编号顺序执行，共 22 个任务。

---
### Task 1: B2 / B8 / B10：服务器组装下沉、速率限制开关、staging Secure Cookie

**Files:**
- Modify: `internal/app/app.go`（`Options`、`Build`、新增 `Servers`）
- Modify: `cmd/platform/main.go`（改用 `a.Servers(cfg)`）
- Modify: `internal/identity/service.go`（`Deps.CodeIPHourLimit`）、`internal/identity/auth.go`（`SendLoginCode` 使用该字段）
- Modify: `internal/adminapi/server.go`（`LoginLimit` 字段）
- Modify: `internal/adminapi/helpers_test.go`（`newEnvWith`）
- Test: `internal/identity/auth_test.go`、`internal/adminapi/auth_test.go`、`internal/app/e2e_test.go`（各追加）

**Interfaces:**
- Consumes: 计划 4 的 `app.Build`、`identity.Deps`、`adminapi.Server`、`config.Config`。
- Produces:
  - `app.Options.DisableRateLimits bool`（为真且 `cfg.Env == "prod"` 时 `Build` 返回错误「生产环境禁止关闭速率限制」）。
  - `func (a *App) Servers(cfg config.Config) (public, admin *http.Server)`：地址取 `cfg.HTTPAddr` / `cfg.AdminAddr`；超时与原 `cmd/platform` 相同（ReadHeader 10s、Read 30s、Write 60s / 120s、Idle 120s）。
  - `identity.Deps.CodeIPHourLimit int`（零值 = 默认 30）。
  - `adminapi.Server.LoginLimit int`（零值 = 默认 20；覆盖 `/auth/login`、`/auth/totp`、`/auth/totp/setup` 共用的限流器）。
  - `api.Server.SecureCookie` 与 `adminapi.Server.SecureCookie` 由 `Build` 设为 `cfg.Env != "dev"`。
  - 测试辅助 `newEnvWith(t, func(*adminapi.Server))`（`internal/adminapi/helpers_test.go`）。

- [ ] **Step 1: 写失败的测试**

`internal/identity/auth_test.go` 末尾追加：

```go
func TestSendCodeIPHourLimitConfigurable(t *testing.T) {
	ctx := context.Background()
	svc := identity.NewService(identity.Deps{Pool: dbtest.New(t), Clock: clock.NewFake(dbtest.T0), SMS: &sms.Mock{}, CodeIPHourLimit: 2})
	for i, phone := range []string{"13800000001", "13800000002"} {
		if err := svc.SendLoginCode(ctx, phone, "10.0.0.9"); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	if err := svc.SendLoginCode(ctx, "13800000003", "10.0.0.9"); !errors.Is(err, apperr.ErrRateLimited) {
		t.Fatalf("third send from same ip = %v, want rate_limited", err)
	}
	// 零值保持默认 30 次
	def := identity.NewService(identity.Deps{Pool: dbtest.New(t), Clock: clock.NewFake(dbtest.T0), SMS: &sms.Mock{}})
	for i := 0; i < 30; i++ {
		if err := def.SendLoginCode(ctx, fmt.Sprintf("138000%05d", i), "10.0.0.8"); err != nil {
			t.Fatalf("default send %d: %v", i, err)
		}
	}
	if err := def.SendLoginCode(ctx, "13999999999", "10.0.0.8"); !errors.Is(err, apperr.ErrRateLimited) {
		t.Fatalf("31st send = %v, want rate_limited", err)
	}
}
```

`internal/adminapi/auth_test.go` 末尾追加：

```go
func TestLoginLimitConfigurable(t *testing.T) {
	e := newEnvWith(t, func(s *adminapi.Server) { s.LoginLimit = 2 })
	c := e.anon()
	for i := 0; i < 2; i++ {
		if code, _ := c.do("POST", "/auth/login", map[string]string{"username": "nobody", "password": "wrong-password-123"}); code != 401 {
			t.Fatalf("attempt %d = %d, want 401", i, code)
		}
	}
	if code, out := c.do("POST", "/auth/login", map[string]string{"username": "nobody", "password": "wrong-password-123"}); code != 429 || codeOf(out) != "rate_limited" {
		t.Fatalf("third attempt = %d %v, want 429 rate_limited", code, out)
	}
}
```

（`auth_test.go` 的 import 需包含 `"spark-cloud/internal/adminapi"`。）

`internal/app/e2e_test.go` 末尾追加：

```go
func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestBuildRejectsDisabledRateLimitsInProd(t *testing.T) {
	cfg := config.Config{Env: "prod", Domain: "spark.test", SecretKey: dbtest.Key, SMSDriver: "aliyun", KYCDriver: "aliyun",
		PayDriver: "real", PDUDriver: "snmp", UploadDir: t.TempDir()}
	if _, err := app.Build(cfg, nil, discardLog(), app.Options{DisableRateLimits: true}); err == nil || !strings.Contains(err.Error(), "速率限制") {
		t.Fatalf("err = %v, want 生产环境禁止关闭速率限制", err)
	}
}

func TestServersUseConfiguredAddressesAndTimeouts(t *testing.T) {
	cfg := config.Config{Env: "dev", Domain: "spark.test", SecretKey: dbtest.Key, SMSDriver: "mock", KYCDriver: "mock", UploadDir: t.TempDir()}
	a, err := app.Build(cfg, dbtest.New(t), discardLog(), app.Options{})
	if err != nil {
		t.Fatal(err)
	}
	cfg.HTTPAddr, cfg.AdminAddr = "127.0.0.1:18080", "127.0.0.1:18081"
	pub, adm := a.Servers(cfg)
	if pub.Addr != "127.0.0.1:18080" || adm.Addr != "127.0.0.1:18081" || pub.Handler == nil || adm.Handler == nil ||
		pub.ReadHeaderTimeout != 10*time.Second || pub.WriteTimeout != 60*time.Second || adm.WriteTimeout != 120*time.Second ||
		pub.IdleTimeout != 120*time.Second {
		t.Fatalf("servers = %+v / %+v", pub, adm)
	}
}

func TestSecureCookieOutsideDev(t *testing.T) {
	for env, wantSecure := range map[string]bool{"dev": false, "staging": true} {
		cfg := config.Config{Env: env, Domain: "spark.test", SecretKey: dbtest.Key, SMSDriver: "mock", KYCDriver: "mock", UploadDir: t.TempDir()}
		a, err := app.Build(cfg, dbtest.New(t), discardLog(), app.Options{Clock: clock.NewFake(dbtest.T0)})
		if err != nil {
			t.Fatal(err)
		}
		ts := httptest.NewServer(a.Handler)
		post := func(path, body string) *http.Response {
			resp, err := http.Post(ts.URL+"/api/v1"+path, "application/json", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			return resp
		}
		post("/auth/sms-code", `{"phone":"13800007777"}`).Body.Close()
		resp := post("/auth/login", `{"phone":"13800007777","code":"000000"}`)
		resp.Body.Close()
		secure := false
		for _, c := range resp.Cookies() {
			if c.Name == "sc_session" {
				secure = c.Secure
			}
		}
		if resp.StatusCode != 200 || secure != wantSecure {
			t.Fatalf("%s: status %d secure %v, want %v", env, resp.StatusCode, secure, wantSecure)
		}
		ts.Close()
	}
}
```

（`e2e_test.go` 的 import 增加 `"strings"`。）

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/identity/ -run TestSendCodeIPHourLimitConfigurable; go test ./internal/adminapi/ -run TestLoginLimitConfigurable; go test ./internal/app/ -run 'TestBuildRejectsDisabledRateLimitsInProd|TestServersUseConfiguredAddressesAndTimeouts|TestSecureCookieOutsideDev'`
Expected: 编译错误（`CodeIPHourLimit`、`newEnvWith`、`LoginLimit`、`DisableRateLimits`、`Servers` 未定义）。

- [ ] **Step 3: 实现**

`internal/identity/service.go` 的 `Deps` 追加字段：

```go
	// CodeIPHourLimit 同一 IP 每小时验证码上限；零值表示默认 30。只有端到端测试服务会调大它。
	CodeIPHourLimit int
```

`internal/identity/auth.go` 的 `SendLoginCode` 中，把 `ipHour >= codeIPHourLimit` 改为使用可配置上限：在 `now := s.d.Clock.Now()` 之后加

```go
	ipLimit := s.d.CodeIPHourLimit
	if ipLimit <= 0 {
		ipLimit = codeIPHourLimit
	}
```

并把判断改为 `if recent > 0 || daily >= codeDailyLimit || ipHour >= ipLimit {`。

`internal/adminapi/server.go`：`Server` 结构体追加

```go
	LoginLimit   int  // 同一 IP 每分钟登录相关请求上限；零值表示默认 20。只有端到端测试服务会调大它
```

`Router()` 开头改为：

```go
	limit := s.LoginLimit
	if limit <= 0 {
		limit = loginLimit
	}
	s.limiter = newIPLimiter(limit, time.Minute, s.Clock)
```

`internal/adminapi/helpers_test.go`：把 `func newEnv(t *testing.T) *env {` 改为

```go
func newEnv(t *testing.T) *env { return newEnvWith(t, nil) }

// newEnvWith 在构建路由前允许调整 Server 字段（例如 LoginLimit）。
func newEnvWith(t *testing.T, tweak func(*adminapi.Server)) *env {
```

并在 `srv := &adminapi.Server{...}` 之后、`ts := httptest.NewServer(srv.Router())` 之前插入

```go
	if tweak != nil {
		tweak(srv)
	}
```

`internal/app/app.go`：

```go
// Options 用于测试时替换时钟与插件；零值表示按配置创建。
type Options struct {
	Clock clock.Clock
	SMS   sms.Sender
	KYC   kyc.Verifier
	Pay   map[string]pay.Provider // 测试注入的支付渠道
	PDU   pdu.Driver              // 测试注入：所有 PDU 都使用该驱动
	// DisableRateLimits 把验证码按 IP 限流与后台登录限流调到 100 万次。只供 cmd/e2e-server 使用：
	// 两个限流器都按注入的时钟计窗口，假时钟下窗口永不滑过。没有任何配置项或环境变量能打开它，
	// 并且生产环境设置为 true 时 Build 直接报错。
	DisableRateLimits bool
}

const noRateLimit = 1_000_000
```

`Build` 开头（现有「生产环境禁止使用 mock 插件」检查之前）加：

```go
	if opt.DisableRateLimits && cfg.Env == "prod" {
		return nil, errors.New("生产环境禁止关闭速率限制")
	}
	ipHourLimit, loginLimit := 0, 0
	if opt.DisableRateLimits {
		ipHourLimit, loginLimit = noRateLimit, noRateLimit
	}
```

`identity.NewService(...)` 的 `Deps` 加 `CodeIPHourLimit: ipHourLimit`；`api.Server` 的 `SecureCookie: cfg.Env != "dev"`；`adminapi.Server` 的 `SecureCookie: cfg.Env != "dev", LoginLimit: loginLimit`。文件末尾追加：

```go
// Servers 按配置创建用户端与后台两个 HTTP 服务器（地址与超时），cmd/platform 与 cmd/e2e-server 共用，
// 保证端到端测试跑的是与生产相同的监听与路由。
func (a *App) Servers(cfg config.Config) (public, admin *http.Server) {
	public = &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           a.Handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	admin = &http.Server{
		Addr:              cfg.AdminAddr,
		Handler:           a.AdminHandler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      120 * time.Second, // CSV 导出最多 10 万行
		IdleTimeout:       120 * time.Second,
	}
	return public, admin
}
```

`cmd/platform/main.go` 的 `run` 中，把从 `srv := &http.Server{` 到 `adminSrv := &http.Server{...}` 结束的两段替换为一行：

```go
	srv, adminSrv := a.Servers(cfg)
```

并删除 `main.go` 中不再使用的 `"net/http"` 以外的 import（`http.Server` 与 `http.ErrServerClosed` 仍用到 `net/http`，`time` 不再使用则删除）。

- [ ] **Step 4: 运行测试确认通过**

Run: `go vet ./... && go test ./...`
Expected: 全部 PASS（含 `TestSendCodeIPHourLimitConfigurable`、`TestLoginLimitConfigurable`、`TestBuildRejectsDisabledRateLimitsInProd`、`TestServersUseConfiguredAddressesAndTimeouts`、`TestSecureCookieOutsideDev`）。

- [ ] **Step 5: 提交**

```bash
git add internal/app internal/identity internal/adminapi cmd/platform
git commit -m "feat(app): share Servers() with e2e-server; DisableRateLimits option; secure cookies outside dev

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---
### Task 2: B4 / B5 / B6 / B7 / B11：用户实名记录接口、视图补字段、后台读权限放开、审计操作人名称、文档脚注

**Files:**
- Modify: `internal/catalog/catalog.go`（`ProductName`、`ImageName`）
- Modify: `internal/api/instances.go`（`product_name`、`image_name`）、`internal/api/recharges.go`（`remaining_seconds`）、`internal/api/auth.go`（`handleMyKYC`）、`internal/api/server.go`（路由 `GET /kyc`）
- Modify: `internal/adminapi/catalog.go`（products / images 读权限、`handleAudit` 的 `actor_name`）
- Modify: `internal/adminapi/permissions_test.go`（矩阵）
- Modify: `docs/superpowers/specs/2026-10-15-plan4-admin-payments-design.md`（权限矩阵脚注）
- Test: `internal/catalog/catalog_test.go`、`internal/api/api_test.go`、`internal/api/recharges_test.go`、`internal/adminapi/catalog_test.go`（各追加）

**Interfaces:**
- Consumes: Task 1 无直接依赖；计划 4 的 `identity.UserKYCRecords`、`audit.Log`、`admin.Service.List`。
- Produces:
  - `catalog.(*Service).ProductName(ctx, id int64) (string, error)`、`ImageName(ctx, id int64) (string, error)`：不受 `status` / `enabled` 过滤；不存在返回 `apperr.ErrNotFound`。
  - 用户端实例视图新增 `product_name`、`image_name`（字符串）；充值视图新增 `remaining_seconds`（int，待支付时 `expires_at − 服务器当前时间` 向下取整且 ≥ 0，其他状态 0）。
  - `GET /api/v1/kyc` → `{record: null | {id, kind, real_name, license_no?, result, reason, created_at, updated_at}}`；个人记录 `real_name` 只保留首字（`李四` → `李*`）。
  - 后台 `GET /admin/api/v1/products`、`GET /admin/api/v1/images` 对 `super / ops / finance` 全部开放。
  - 后台 `GET /admin/api/v1/audit` 每条增加 `actor_name`（`actor_type == "admin"` 且能找到时为用户名，否则空串）。

- [ ] **Step 1: 写失败的测试**

`internal/catalog/catalog_test.go` 末尾追加：

```go
func TestNamesIgnoreStatusAndEnabled(t *testing.T) {
	pool := dbtest.New(t)
	svc := catalog.NewService(pool)
	ctx := context.Background()
	pid, iid := dbtest.ProductID(t, pool), dbtest.ImageID(t, pool)
	if _, err := pool.Exec(ctx, `UPDATE images SET enabled = false WHERE id = $1`, iid); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE products SET status = 'inactive' WHERE id = $1`, pid); err != nil {
		t.Fatal(err)
	}
	if name, err := svc.ProductName(ctx, pid); err != nil || name != "DGX Spark 整机" {
		t.Fatalf("ProductName = %q, %v", name, err)
	}
	if name, err := svc.ImageName(ctx, iid); err != nil || name != "Ubuntu 24.04 + CUDA" {
		t.Fatalf("ImageName = %q, %v", name, err)
	}
	if _, err := svc.ImageName(ctx, 999999); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("missing image = %v, want not_found", err)
	}
}
```

（import 增加 `"errors"`、`"spark-cloud/internal/platform/apperr"`，若尚未引入。）

`internal/api/api_test.go` 末尾追加：

```go
func TestInstanceViewIncludesProductAndImageNames(t *testing.T) {
	e := newTestEnv(t)
	dbtest.CreateMachine(t, e.pool, "SN-NAMES")
	c, uid := e.verifiedClient("13800000021")
	if _, err := e.wallet.AdminAdjust(context.Background(), 1, uid, 100000, "测试充值", ""); err != nil {
		t.Fatal(err)
	}
	iid := dbtest.ImageID(t, e.pool)
	code, out := c.do("POST", "/orders", map[string]any{"product_id": dbtest.ProductID(t, e.pool), "duration": "1d", "image_id": iid})
	if code != 201 {
		t.Fatalf("order = %d %v", code, out)
	}
	inst := out["instance"].(map[string]any)
	if inst["product_name"] != "DGX Spark 整机" || inst["image_name"] != "Ubuntu 24.04 + CUDA" {
		t.Fatalf("names on create = %v", inst)
	}
	// 镜像停用后仍能显示名称
	if _, err := e.pool.Exec(context.Background(), `UPDATE images SET enabled = false WHERE id = $1`, iid); err != nil {
		t.Fatal(err)
	}
	id := inst["id"].(string)
	if code, out := c.do("GET", "/instances/"+id, nil); code != 200 || out["image_name"] != "Ubuntu 24.04 + CUDA" {
		t.Fatalf("detail = %d %v", code, out)
	}
	if code, out := c.do("GET", "/instances", nil); code != 200 || out["items"].([]any)[0].(map[string]any)["product_name"] != "DGX Spark 整机" {
		t.Fatalf("list = %d %v", code, out)
	}
}

func TestMyKYCRecord(t *testing.T) {
	e := newTestEnv(t)
	c := e.client()
	c.login("13800000022")
	if code, out := c.do("GET", "/kyc", nil); code != 200 || out["record"] != nil {
		t.Fatalf("empty kyc = %d %v", code, out)
	}
	if code, out := c.do("POST", "/kyc/personal", map[string]string{"name": "欧阳娜娜", "id_number": "11010119900101123X"}); code != 200 {
		t.Fatalf("kyc = %d %v", code, out)
	}
	code, out := c.do("GET", "/kyc", nil)
	rec, _ := out["record"].(map[string]any)
	if code != 200 || rec == nil || rec["kind"] != "personal" || rec["result"] != "verified" || rec["real_name"] != "欧***" {
		t.Fatalf("kyc record = %d %v", code, out)
	}
	for _, k := range []string{"id_number_masked", "license_file", "user_id"} {
		if _, ok := rec[k]; ok {
			t.Fatalf("%s must not be exposed: %v", k, rec)
		}
	}
	if code, _ := e.client().do("GET", "/kyc", nil); code != 401 {
		t.Fatalf("anonymous kyc = %d, want 401", code)
	}
}
```

`internal/api/recharges_test.go` 的 `TestRechargeFlowWithDevPay` 中，`id := out["id"].(string)` 之后插入：

```go
	if out["remaining_seconds"].(float64) != 900 {
		t.Fatalf("remaining_seconds on create = %v, want 900", out["remaining_seconds"])
	}
	e.clk.Advance(60 * time.Second)
	if code, out := c.do("GET", "/recharges/"+id, nil); code != 200 || out["remaining_seconds"].(float64) != 840 {
		t.Fatalf("remaining after 60s = %d %v, want 840", code, out)
	}
```

并在 dev pay 之后的 `GET /recharges/{id}` 断言里追加条件 `|| out["remaining_seconds"].(float64) != 0`。这需要 `testEnv` 暴露时钟：`internal/api/api_test.go` 的 `testEnv` 结构体加字段 `clk *clock.Fake`，`newTestEnv` 返回时填 `clk: clk`；`recharges_test.go` import 增加 `"time"`。

`internal/adminapi/permissions_test.go` 的 `expectedRoutes` 中，把

```go
	{"GET", "/products", super}, {"PUT", "/products/{id}/prices/{duration}", super},
	{"GET", "/images", super}, {"POST", "/images", super}, {"PUT", "/images/{id}", super},
```

改为

```go
	{"GET", "/products", all}, {"PUT", "/products/{id}/prices/{duration}", super},
	{"GET", "/images", all}, {"POST", "/images", super}, {"PUT", "/images/{id}", super},
```

`internal/adminapi/catalog_test.go` 的 `TestAuditEndpoint` 末尾追加：

```go
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
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/catalog/ ./internal/api/ ./internal/adminapi/`
Expected: 编译错误（`ProductName` 未定义、`e.clk` 不存在）；`TestRouteTableMatchesPermissionMatrix` 失败。

- [ ] **Step 3: 实现**

`internal/catalog/catalog.go` 末尾追加：

```go
// ProductName 返回商品名称，不受上下架状态影响（用户端展示历史实例用）。
func (s *Service) ProductName(ctx context.Context, id int64) (string, error) {
	var name string
	err := s.pool.QueryRow(ctx, `SELECT name FROM products WHERE id = $1`, id).Scan(&name)
	if db.IsNoRows(err) {
		return "", apperr.ErrNotFound
	}
	return name, err
}

// ImageName 返回镜像名称，不受启用状态影响（镜像停用后用户仍能看到自己实例的镜像名）。
func (s *Service) ImageName(ctx context.Context, id int64) (string, error) {
	var name string
	err := s.pool.QueryRow(ctx, `SELECT name FROM images WHERE id = $1`, id).Scan(&name)
	if db.IsNoRows(err) {
		return "", apperr.ErrNotFound
	}
	return name, err
}
```

`internal/api/instances.go`：`instanceView` 在 `ImageID` 之后加

```go
	ProductName      string     `json:"product_name"`
	ImageName        string     `json:"image_name"`
```

并追加：

```go
// nameCache 在一次请求内去重商品 / 镜像名称查询（列表最多 100 条，通常只有 1 个商品、几个镜像）。
type nameCache struct {
	products, images map[int64]string
}

func newNameCache() *nameCache { return &nameCache{products: map[int64]string{}, images: map[int64]string{}} }

func (c *nameCache) lookup(cache map[int64]string, id int64, fetch func() (string, error)) (string, error) {
	if n, ok := cache[id]; ok {
		return n, nil
	}
	n, err := fetch()
	if errors.Is(err, apperr.ErrNotFound) { // 商品与镜像没有删除接口；兜底显示空名称而不是报错
		n, err = "", nil
	}
	if err != nil {
		return "", err
	}
	cache[id] = n
	return n, nil
}

// viewNamed 在基础视图上补上商品名与镜像名。
func (s *Server) viewNamed(ctx context.Context, i rental.Instance, nc *nameCache) (instanceView, error) {
	v := s.view(i)
	var err error
	if v.ProductName, err = nc.lookup(nc.products, i.ProductID, func() (string, error) { return s.Catalog.ProductName(ctx, i.ProductID) }); err != nil {
		return v, err
	}
	if v.ImageName, err = nc.lookup(nc.images, i.ImageID, func() (string, error) { return s.Catalog.ImageName(ctx, i.ImageID) }); err != nil {
		return v, err
	}
	return v, nil
}

func (s *Server) viewOne(ctx context.Context, i rental.Instance) (instanceView, error) {
	return s.viewNamed(ctx, i, newNameCache())
}

func (s *Server) views(ctx context.Context, insts []rental.Instance) ([]instanceView, error) {
	nc := newNameCache()
	out := make([]instanceView, 0, len(insts))
	for _, i := range insts {
		v, err := s.viewNamed(ctx, i, nc)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}
```

（import 增加 `"context"`、`"errors"`、`"spark-cloud/internal/platform/apperr"`。）三个处理器改为：

```go
	// handlePlaceOrder 成功分支
	v, err := s.viewOne(r.Context(), inst)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"order": ord, "instance": v})

	// handleInstances
	views, err := s.views(r.Context(), insts)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list(views))

	// handleInstance
	v, err := s.viewOne(r.Context(), inst)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
```

`internal/api/recharges.go`：`rechargeView` 在 `QRCode` 之后加 `RemainingSeconds int64 \`json:"remaining_seconds"\``；`toRechargeView` 改为方法：

```go
func (s *Server) toRechargeView(r billing.Recharge) rechargeView {
	v := rechargeView{ID: r.ID, Channel: r.Channel, Amount: r.Amount, Status: r.Status, ExpiresAt: r.ExpiresAt,
		PaidAt: r.PaidAt, ClosedAt: r.ClosedAt, CreatedAt: r.CreatedAt}
	if r.Status == billing.StatusPending {
		v.QRCode = r.QRCode
		if rem := r.ExpiresAt.Sub(s.Clock.Now()); rem > 0 {
			v.RemainingSeconds = int64(rem / time.Second) // 向下取整；前端用它驱动倒计时，不拿设备时钟减 expires_at
		}
	}
	return v
}
```

文件内四处 `toRechargeView(` 调用改为 `s.toRechargeView(`。

`internal/api/auth.go` 末尾追加：

```go
// kycRecordView 是用户查看自己最近一条实名记录的视图：不含身份证号（含脱敏）与执照文件 key。
type kycRecordView struct {
	ID        int64     `json:"id"`
	Kind      string    `json:"kind"`
	RealName  string    `json:"real_name"`
	LicenseNo string    `json:"license_no,omitempty"`
	Result    string    `json:"result"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// maskName 个人姓名只保留首字，其余以 * 代替。
func maskName(name string) string {
	r := []rune(name)
	if len(r) <= 1 {
		return name
	}
	return string(r[0]) + strings.Repeat("*", len(r)-1)
}

// handleMyKYC 返回用户最近一条实名记录（含驳回原因），没有记录时 record 为 null。
func (s *Server) handleMyKYC(w http.ResponseWriter, r *http.Request) {
	records, err := s.Identity.UserKYCRecords(r.Context(), currentUser(r).ID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if len(records) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"record": nil})
		return
	}
	k := records[0]
	v := kycRecordView{ID: k.ID, Kind: k.Kind, RealName: k.RealName, LicenseNo: k.LicenseNo, Result: k.Result, Reason: k.Reason,
		CreatedAt: k.CreatedAt, UpdatedAt: k.UpdatedAt}
	if k.Kind == "personal" {
		v.RealName = maskName(k.RealName)
	}
	writeJSON(w, http.StatusOK, map[string]any{"record": v})
}
```

（import 增加 `"strings"`、`"time"`。）`internal/api/server.go` 的登录分组里 `r.Post("/kyc/personal", ...)` 之前加 `r.Get("/kyc", s.handleMyKYC)`。

`internal/adminapi/catalog.go`：路由改为

```go
	s.handle(g, http.MethodGet, "/products", allRoles, s.handleAdminProducts)
	s.handle(g, http.MethodPut, "/products/{id}/prices/{duration}", superOnly, s.handleSetPrice)
	s.handle(g, http.MethodGet, "/images", allRoles, s.handleAdminImages)
```

`handleAudit` 的末尾（取得 `list, total` 之后）替换为：

```go
	admins, err := s.Admins.List(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	names := make(map[int64]string, len(admins))
	for _, a := range admins {
		names[a.ID] = a.Username
	}
	views := make([]auditView, 0, len(list))
	for _, l := range list {
		v := auditView{Log: l}
		if l.ActorType == "admin" {
			v.ActorName = names[l.ActorID]
		}
		views = append(views, v)
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page(views, total))
```

并在文件内定义：

```go
// auditView 在审计记录上补操作人用户名（admin 之外的 actor 或已不存在的管理员为空串）。
type auditView struct {
	audit.Log
	ActorName string `json:"actor_name"`
}
```

`docs/superpowers/specs/2026-10-15-plan4-admin-payments-design.md`：在「权限矩阵」表格之后、「**审计**」段之前插入一行脚注：

```markdown
> 计划 5 调整（2026-10-17，见 `2026-10-17-plan5-frontend-design.md` §6 B6）：`GET /products` 与 `GET /images` 的读权限放开为全部角色（ops 登记机器需选商品、所有角色的实例页需显示镜像 / 商品名）；写接口（价格、镜像新增 / 修改）仍仅 super。
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go vet ./... && go test ./...`
Expected: 全部 PASS（含 `TestNamesIgnoreStatusAndEnabled`、`TestInstanceViewIncludesProductAndImageNames`、`TestMyKYCRecord`、`TestRechargeFlowWithDevPay`、`TestAuditEndpoint`、`TestRouteTableMatchesPermissionMatrix`、`TestPermissionMatrix`）。

- [ ] **Step 5: 提交**

```bash
git add internal/catalog internal/api internal/adminapi docs/superpowers/specs/2026-10-15-plan4-admin-payments-design.md
git commit -m "feat(api): GET /kyc, product/image names on instances, recharge remaining_seconds; admin products/images readable by all roles; audit actor_name

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---
### Task 3: B1：静态托管 `internal/web` 与两个路由器的挂载

**Files:**
- Create: `internal/web/web.go`、`internal/web/web_test.go`、`internal/web/dist/README.md`
- Modify: `internal/api/server.go`（`Static` 字段、JSON 404、SPA 回退）
- Modify: `internal/adminapi/server.go`（`Static` 字段、`NoStore` 下沉、`/admin/` 挂载、`/` 重定向、JSON 404）
- Modify: `internal/api/api_test.go`（`newTestEnvWith`）、`internal/adminapi/helpers_test.go`（无改动，复用 `newEnvWith`）
- Modify: `.gitignore`
- Test: `internal/web/web_test.go`、`internal/api/api_test.go`、`internal/adminapi/auth_test.go`（追加）

**Interfaces:**
- Consumes: Task 1 的 `newEnvWith`。
- Produces:
  - `web.NewHandler(fsys fs.FS, prefix string) http.Handler`：`prefix` 为 `"/"` 或 `"/admin/"`；文件存在直接返回（`assets/` 下 `Cache-Control: public, max-age=31536000, immutable`，其余 `public, max-age=3600`）；其他路径返回 `index.html`（`Cache-Control: no-cache`）；`index.html` 不存在返回 503 文本「前端尚未构建，请执行 make web」；所有响应带 CSP、`X-Content-Type-Options: nosniff`、`X-Frame-Options: DENY`、`Referrer-Policy: same-origin`。
  - `web.Console() http.Handler`（`fs.Sub(dist, "dist/console")`，前缀 `/`）、`web.Admin() http.Handler`（`dist/admin`，前缀 `/admin/`）。
  - `api.Server.Static http.Handler`、`adminapi.Server.Static http.Handler`（nil 时分别取 `web.Console()` / `web.Admin()`）。
  - 用户端：未知 `/api/*` → `404 {code: "not_found"}`；其余路径 → 控制台 SPA。后台：`/` 与 `/admin` → `302 /admin/`；`/admin/`、`/admin/*`（非 `/admin/api/`）→ 后台 SPA；`/admin/api/*` 未知路径与其他路径 → JSON 404。API 响应仍 `Cache-Control: no-store`，静态文件不带 `no-store`。
  - 测试辅助 `newTestEnvWith(t, func(*api.Server))`（`internal/api/api_test.go`）。

- [ ] **Step 1: 写失败的测试**

`internal/web/web_test.go`：

```go
package web_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"spark-cloud/internal/web"
)

func serve(h http.Handler, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestSPAHandler(t *testing.T) {
	fsys := fstest.MapFS{
		"index.html":           {Data: []byte("<!doctype html><title>admin</title>")},
		"assets/app.abc123.js": {Data: []byte("console.log(1)")},
		"brand/logo.svg":       {Data: []byte("<svg xmlns=\"http://www.w3.org/2000/svg\"/>")},
	}
	h := web.NewHandler(fsys, "/admin/")
	cases := []struct{ path, cache, ctype, body string }{
		{"/admin/", "no-cache", "text/html", "<!doctype"},
		{"/admin/index.html", "no-cache", "text/html", "<!doctype"},
		{"/admin/instances/ins-abc12345", "no-cache", "text/html", "<!doctype"},
		{"/admin/assets/", "no-cache", "text/html", "<!doctype"}, // 目录不直接列出，回退 index
		{"/admin/../etc/passwd", "no-cache", "text/html", "<!doctype"},
		{"/admin/assets/app.abc123.js", "public, max-age=31536000, immutable", "javascript", "console.log"},
		{"/admin/brand/logo.svg", "public, max-age=3600", "image/svg+xml", "<svg"},
	}
	for _, c := range cases {
		rec := serve(h, c.path)
		if rec.Code != 200 || rec.Header().Get("Cache-Control") != c.cache ||
			!strings.Contains(rec.Header().Get("Content-Type"), c.ctype) || !strings.Contains(rec.Body.String(), c.body) {
			t.Errorf("%s: %d cache=%q type=%q body=%q", c.path, rec.Code, rec.Header().Get("Cache-Control"), rec.Header().Get("Content-Type"), rec.Body.String())
		}
		for k, want := range map[string]string{"X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY", "Referrer-Policy": "same-origin"} {
			if rec.Header().Get(k) != want {
				t.Errorf("%s: %s = %q, want %q", c.path, k, rec.Header().Get(k), want)
			}
		}
		if csp := rec.Header().Get("Content-Security-Policy"); !strings.HasPrefix(csp, "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline';") ||
			!strings.Contains(csp, "frame-ancestors 'none'") || !strings.Contains(csp, "form-action 'self'") {
			t.Errorf("%s: csp = %q", c.path, csp)
		}
	}
	if rec := httptest.NewRecorder(); true {
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("POST to spa = %d, want 405", rec.Code)
		}
	}
}

func TestSPAHandlerWithoutBuild(t *testing.T) {
	rec := serve(web.NewHandler(fstest.MapFS{}, "/"), "/market")
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "前端尚未构建，请执行 make web") ||
		rec.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatalf("missing build = %d %q", rec.Code, rec.Body.String())
	}
}

// 无论产物是否已构建，Console / Admin 都能构造并响应（200 或 503），不会 panic。
func TestConsoleAndAdminHandlers(t *testing.T) {
	for name, h := range map[string]http.Handler{"console": web.Console(), "admin": web.Admin()} {
		rec := serve(h, "/admin/x")
		if rec.Code != 200 && rec.Code != 503 {
			t.Fatalf("%s: %d", name, rec.Code)
		}
	}
}
```

`internal/api/api_test.go`：把 `func newTestEnv(t *testing.T) *testEnv {` 改为

```go
func newTestEnv(t *testing.T) *testEnv { return newTestEnvWith(t, nil) }

// newTestEnvWith 在构建路由前允许调整 Server 字段（例如 Static）。
func newTestEnvWith(t *testing.T, tweak func(*api.Server)) *testEnv {
```

并在 `srv := &api.Server{...}` 之后、`ts := httptest.NewServer(srv.Router())` 之前加 `if tweak != nil { tweak(srv) }`。末尾追加：

```go
func TestRouterServesConsoleAndJSON404(t *testing.T) {
	index := fstest.MapFS{"index.html": {Data: []byte("<!doctype html><title>console</title>")}}
	e := newTestEnvWith(t, func(s *api.Server) { s.Static = web.NewHandler(index, "/") })
	get := func(path string) *http.Response {
		resp, err := http.Get(e.ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}
	for _, p := range []string{"/", "/market", "/instances/ins-abc12345", "/wallet/recharge/rch-x"} {
		resp := get(p)
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 || !strings.Contains(string(body), "console") || resp.Header.Get("Cache-Control") != "no-cache" ||
			resp.Header.Get("Content-Security-Policy") == "" {
			t.Fatalf("%s: %d %q cache=%q", p, resp.StatusCode, body, resp.Header.Get("Cache-Control"))
		}
	}
	resp := get("/api/v1/nope")
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != 404 || out["code"] != "not_found" {
		t.Fatalf("unknown api = %d %v", resp.StatusCode, out)
	}
	if resp := get("/api/nope"); resp.StatusCode != 404 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("/api/nope = %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if resp := get("/api/v1/wallet"); resp.StatusCode != 401 || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("anonymous wallet = %d cache=%q", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
}
```

（import 增加 `"strings"`、`"testing/fstest"`、`"spark-cloud/internal/web"`。）

`internal/adminapi/auth_test.go` 末尾追加：

```go
func TestAdminRouterServesSPAAndJSON404(t *testing.T) {
	index := fstest.MapFS{"index.html": {Data: []byte("<!doctype html><title>admin</title>")}}
	e := newEnvWith(t, func(s *adminapi.Server) { s.Static = web.NewHandler(index, "/admin/") })
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(path string) (*http.Response, string) {
		resp, err := noRedirect.Get(e.ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp, string(body)
	}
	for _, p := range []string{"/", "/admin"} {
		if resp, _ := get(p); resp.StatusCode != 302 || resp.Header.Get("Location") != "/admin/" {
			t.Fatalf("%s: %d -> %q, want 302 /admin/", p, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	for _, p := range []string{"/admin/", "/admin/machines/12", "/admin/login"} {
		resp, body := get(p)
		if resp.StatusCode != 200 || !strings.Contains(body, "admin") || resp.Header.Get("Cache-Control") != "no-cache" ||
			resp.Header.Get("Content-Security-Policy") == "" {
			t.Fatalf("%s: %d %q cache=%q", p, resp.StatusCode, body, resp.Header.Get("Cache-Control"))
		}
	}
	for _, p := range []string{"/admin/api/v1/nope", "/admin/api/", "/nope", "/api/v1/products"} {
		resp, body := get(p)
		if resp.StatusCode != 404 || !strings.Contains(body, `"not_found"`) {
			t.Fatalf("%s: %d %q, want JSON 404", p, resp.StatusCode, body)
		}
	}
	if resp, _ := get("/admin/api/v1/dashboard"); resp.StatusCode != 401 || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("anonymous dashboard = %d cache=%q", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
}
```

（`auth_test.go` 的 import 增加 `"io"`、`"testing/fstest"`、`"spark-cloud/internal/web"`；`"net/http"`、`"strings"` 该文件已引入。）

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/web/ ./internal/api/ ./internal/adminapi/`
Expected: 编译错误（包 `web` 不存在、`Static` 未定义、`newTestEnvWith` 未定义）。

- [ ] **Step 3: 实现 `internal/web`**

`internal/web/dist/README.md`：

```markdown
# 前端构建产物目录

`console/` 与 `admin/` 由 `make web`（`cd web && npm run build`）生成并被 `internal/web` embed 进二进制，已 gitignore。
本文件入库是为了保证 `//go:embed dist` 在未构建时也有文件可嵌入；未构建时访问页面会返回 503「前端尚未构建，请执行 make web」。
```

`internal/web/web.go`：

```go
// Package web 把前端构建产物 embed 进 platform 二进制，并以单页应用方式托管：
// 存在的文件直接返回（带哈希的 assets 长缓存），其余路径回退到 index.html（不缓存）。
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// dist/console 与 dist/admin 由前端构建生成（gitignore）；dist/README.md 入库保证 embed 始终有文件。
//
//go:embed dist
var dist embed.FS

// CSP 是静态页面的内容安全策略：只允许同源脚本与请求；style-src 'unsafe-inline' 是 Element Plus 行内样式所需。
const CSP = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; " +
	"font-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; object-src 'none'; base-uri 'self'; form-action 'self'"

// Console 托管用户控制台（挂在 :8080 的 /）。
func Console() http.Handler { return NewHandler(sub("dist/console"), "/") }

// Admin 托管管理后台（挂在 :8081 的 /admin/）。
func Admin() http.Handler { return NewHandler(sub("dist/admin"), "/admin/") }

func sub(dir string) fs.FS {
	s, err := fs.Sub(dist, dir) // 只校验路径格式；目录不存在时 Open 会失败，处理器返回 503
	if err != nil {
		panic(err)
	}
	return s
}

// NewHandler 返回单页应用处理器。prefix 是挂载前缀（"/" 或 "/admin/"），请求路径去掉前缀后在 fsys 中查找。
func NewHandler(fsys fs.FS, prefix string) http.Handler {
	files := http.FileServerFS(fsys)
	trim := strings.TrimSuffix(prefix, "/")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", CSP)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		rel := strings.TrimPrefix(path.Clean("/"+strings.TrimPrefix(r.URL.Path, trim)), "/")
		if rel != "" && rel != "index.html" {
			if st, err := fs.Stat(fsys, rel); err == nil && !st.IsDir() {
				if strings.HasPrefix(rel, "assets/") {
					h.Set("Cache-Control", "public, max-age=31536000, immutable") // 文件名带内容哈希
				} else {
					h.Set("Cache-Control", "public, max-age=3600") // favicon、brand/ 等
				}
				r2 := r.Clone(r.Context())
				r2.URL.Path = "/" + rel
				files.ServeHTTP(w, r2)
				return
			}
		}
		index, err := fs.ReadFile(fsys, "index.html")
		if err != nil {
			h.Set("Content-Type", "text/plain; charset=utf-8")
			h.Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("前端尚未构建，请执行 make web\n"))
			return
		}
		h.Set("Cache-Control", "no-cache") // 发布新版本后刷新即生效
		h.Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(index)
	})
}
```

- [ ] **Step 4: 挂载到两个路由器**

`internal/api/server.go`：`Server` 追加字段

```go
	Static       http.Handler // 控制台静态文件；nil 时使用 web.Console()
```

`Router()` 中，在 `r.Use(...)` 之后、`r.Route("/api/v1", ...)` **之前**（chi 在 Mount 时把父路由的 NotFound 复制给子路由，顺序不能反）加：

```go
	static := s.Static
	if static == nil {
		static = web.Console()
	}
	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasPrefix(req.URL.Path, "/api/") {
			s.writeError(w, req, apperr.ErrNotFound) // 未知 API 路径返回 JSON 404，不回退页面
			return
		}
		static.ServeHTTP(w, req)
	})
```

（import 增加 `"spark-cloud/internal/web"`。）

`internal/adminapi/server.go`：`Server` 追加字段 `Static http.Handler // 后台静态文件；nil 时使用 web.Admin()`。`Router()` 改为：

```go
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	// 后台只在内网监听，不使用 RealIP：任何内网客户端都能伪造 X-Forwarded-For，
	// 会绕过按 IP 的登录限流并伪造审计日志里的 IP。始终以连接的 RemoteAddr 为准。
	r.Use(httpx.Recoverer(s.Log), s.requireJSONForWrites)
	jsonNotFound := func(w http.ResponseWriter, req *http.Request) { s.writeError(w, req, apperr.ErrNotFound) }
	r.NotFound(jsonNotFound)
	r.Route(prefix, func(r chi.Router) {
		r.Use(httpx.NoStore) // API 响应一律不缓存；静态文件由 web 处理器自行设置 Cache-Control
		r.With(s.rateLimit).Post("/auth/login", s.handleLogin)
		r.With(s.rateLimit).Post("/auth/totp/setup", s.handleTOTPSetup)
		r.With(s.rateLimit).Post("/auth/totp", s.handleTOTP)
		r.Post("/auth/logout", s.handleLogout)
		r.Group(func(g chi.Router) {
			g.Use(s.requireAdmin)
			s.accountRoutes(g)
			s.machineRoutes(g)
			s.instanceRoutes(g)
			s.userRoutes(g)
			s.financeRoutes(g)
			s.catalogRoutes(g)
		})
	})
	static := s.Static
	if static == nil {
		static = web.Admin()
	}
	redirectAdmin := func(w http.ResponseWriter, req *http.Request) { http.Redirect(w, req, "/admin/", http.StatusFound) }
	r.Get("/", redirectAdmin)
	r.Get("/admin", redirectAdmin)
	r.HandleFunc("/admin/api/*", jsonNotFound) // /admin/api/ 下非 v1 的路径不回退页面
	r.Get("/admin/", static.ServeHTTP)
	r.Get("/admin/*", static.ServeHTTP)
	return r
```

（import 增加 `"spark-cloud/internal/web"`。`requireJSONForWrites` 对 GET 放行，静态文件不受影响。）

`.gitignore` 追加：

```
/internal/web/dist/console/
/internal/web/dist/admin/
/web/node_modules/
/web/test-results/
/web/playwright-report/
/web/*/dist/
```

- [ ] **Step 5: 运行测试确认通过**

Run: `go vet ./... && go test ./...`
Expected: 全部 PASS（含 `TestSPAHandler`、`TestSPAHandlerWithoutBuild`、`TestConsoleAndAdminHandlers`、`TestRouterServesConsoleAndJSON404`、`TestAdminRouterServesSPAAndJSON404`、`TestLoginAndTOTPSetupOverHTTP`（其中有后台 API 响应 `no-store` 的断言，`NoStore` 下沉后必须仍然通过）、`TestPermissionMatrix`）。再手工确认一次：

```bash
SPARK_DATABASE_URL=postgres://postgres:postgres@localhost:5433/spark?sslmode=disable SPARK_SECRET_KEY=MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE= go run ./cmd/platform &
sleep 3; curl -si http://127.0.0.1:8080/market | head -5; curl -si http://127.0.0.1:8081/ | head -3; kill %1
```

Expected: `/market` 返回 `503` 与「前端尚未构建，请执行 make web」（`make devdb` 需已在另一终端运行）；`:8081/` 返回 `302` `Location: /admin/`。

- [ ] **Step 6: 提交**

```bash
git add internal/web internal/api internal/adminapi .gitignore
git commit -m "feat(web): embed and serve SPA builds with security headers; JSON 404 for unknown API paths

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---
### Task 4: B9：`internal/e2ehooks` 与 `cmd/e2e-server`

**Files:**
- Create: `internal/e2ehooks/kyc.go`、`internal/e2ehooks/hooks.go`、`internal/e2ehooks/hooks_test.go`
- Create: `cmd/e2e-server/main.go`

**Interfaces:**
- Consumes: Task 1 的 `app.Options.DisableRateLimits`、`App.Servers`；计划 4 的 `fleet.Register/Heartbeat/PendingCommands`、`rental.HandleCommandResult/Sweep`、`billing.Sweep`、`wallet.AdminAdjust`、`admin.Create/EnrollTOTP/ConfirmEnrollment`、`catalog.ProductIDByCode`。
- Produces:
  - `e2ehooks.SwitchableKYC`（实现 `kyc.Verifier`）：`SetReject(bool)`。
  - `e2ehooks.Server{App *app.App; Pool *pgxpool.Pool; Clock *clock.Fake; KYC *SwitchableKYC; Log *slog.Logger}`，`Handler() http.Handler`，只接受来自回环地址的连接。
  - 钩子（JSON）：`GET /healthz` → `{ok}`；`GET /hooks/clock` → `{now}`；`POST /hooks/clock/advance {seconds}` → `{now}`（`seconds ≤ 0` 返回 400）；`POST /hooks/sweep {heartbeat?}`（缺省 true：先对全部机器心跳）→ `{ok}`；`POST /hooks/agent {machine_id?, fail?, result?}` → `{completed}`；`POST /hooks/machines {count, serial_prefix?}` → `{ids}`；`POST /hooks/users {phone?, verified?, balance?, ledger_rows?}` → `{id, phone}`（先写 `ledger_rows` 条 +1 分的调账流水 `e2e-seed-<i>`，再调一笔 `balance − ledger_rows` 的 `e2e-seed-balance`，最终余额恰等于 `balance`，流水共 `ledger_rows + 1` 条）；`POST /hooks/admins {username, role, bind_totp}` → `{id, password, totp_secret?, recovery_codes?}`，密码固定 `e2e-password-123456`；`POST /hooks/kyc {reject}` → `{ok}`。
  - `cmd/e2e-server`：监听 `127.0.0.1:18080`（控制台 + 用户 API）、`127.0.0.1:18081`（后台）、`127.0.0.1:18090`（钩子）；就绪后向 stdout 打印 `E2E READY`；`Env=dev`、`Domain=spark.test`、mock 短信 / 支付 / PDU、`SwitchableKYC`、`clock.NewFake(time.Now())`、`DisableRateLimits: true`；不启动定时清扫器；SIGTERM 时停库并删除临时目录。

- [ ] **Step 1: 写失败的测试**

`internal/e2ehooks/hooks_test.go`：

```go
package e2ehooks_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"spark-cloud/internal/app"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/e2ehooks"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/config"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/platform/paging"
	"spark-cloud/internal/rental"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

type env struct {
	t   *testing.T
	a   *app.App
	clk *clock.Fake
	ts  *httptest.Server
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := dbtest.New(t)
	clk := clock.NewFake(dbtest.T0)
	k := &e2ehooks.SwitchableKYC{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := config.Config{Env: "dev", Domain: "spark.test", SecretKey: dbtest.Key, SMSDriver: "mock", KYCDriver: "mock", UploadDir: t.TempDir()}
	a, err := app.Build(cfg, pool, log, app.Options{Clock: clk, KYC: k, DisableRateLimits: true})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer((&e2ehooks.Server{App: a, Pool: pool, Clock: clk, KYC: k, Log: log}).Handler())
	t.Cleanup(ts.Close)
	return &env{t: t, a: a, clk: clk, ts: ts}
}

func (e *env) call(method, path string, body any) (int, map[string]any) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.ts.URL+path, rd)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (e *env) must(method, path string, body any) map[string]any {
	e.t.Helper()
	code, out := e.call(method, path, body)
	if code != 200 {
		e.t.Fatalf("%s %s = %d %v", method, path, code, out)
	}
	return out
}

func TestClockHooks(t *testing.T) {
	e := newEnv(t)
	if code, out := e.call("GET", "/healthz", nil); code != 200 || out["ok"] != true {
		t.Fatalf("healthz = %d %v", code, out)
	}
	now, _ := time.Parse(time.RFC3339Nano, e.must("GET", "/hooks/clock", nil)["now"].(string))
	if !now.Equal(dbtest.T0) {
		t.Fatalf("clock = %v, want %v", now, dbtest.T0)
	}
	after, _ := time.Parse(time.RFC3339Nano, e.must("POST", "/hooks/clock/advance", map[string]int{"seconds": 90})["now"].(string))
	if !after.Equal(dbtest.T0.Add(90 * time.Second)) {
		t.Fatalf("advanced = %v", after)
	}
	if code, _ := e.call("POST", "/hooks/clock/advance", map[string]int{"seconds": 0}); code != 400 {
		t.Fatalf("advance 0 = %d, want 400", code)
	}
	if code, _ := e.call("POST", "/hooks/clock/advance", map[string]int{"seconds": -5}); code != 400 {
		t.Fatalf("advance -5 = %d, want 400", code)
	}
}

func TestMachinesUsersAgentAndSweep(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	ids := e.must("POST", "/hooks/machines", map[string]any{"count": 2, "serial_prefix": "T-"})["ids"].([]any)
	if len(ids) != 2 {
		t.Fatalf("machines = %v", ids)
	}
	mid := int64(ids[0].(float64))
	if m, err := e.a.Fleet.Get(ctx, e.a.Fleet.Pool(), mid); err != nil || !m.AgentOnline || m.State != "idle" || m.Serial != "T-1" {
		t.Fatalf("machine = %+v, %v", m, err)
	}
	u := e.must("POST", "/hooks/users", map[string]any{"phone": "13900000001", "verified": true, "balance": 100000, "ledger_rows": 3})
	uid := int64(u["id"].(float64))
	if user, err := e.a.Identity.GetUser(ctx, uid); err != nil || user.KYCStatus != "verified" || user.Phone != "13900000001" {
		t.Fatalf("user = %+v, %v", user, err)
	}
	if bal, _ := e.a.Wallet.Balance(ctx, uid); bal != 100000 {
		t.Fatalf("balance = %d, want 100000", bal)
	}
	if rows, _ := e.a.Wallet.ListLedger(ctx, uid, 0, 100); len(rows) != 4 {
		t.Fatalf("ledger rows = %d, want 4 (3 seed + 1 balance)", len(rows))
	}
	if code, out := e.call("POST", "/hooks/users", map[string]any{}); code != 200 || len(out["phone"].(string)) != 11 {
		t.Fatalf("default user = %d %v", code, out)
	}

	_, inst, err := e.a.Rental.Place(ctx, rental.PlaceRequest{UserID: uid, ProductID: dbtest.ProductID(t, e.a.Fleet.Pool()),
		ImageID: dbtest.ImageID(t, e.a.Fleet.Pool()), Duration: catalog.D1d})
	if err != nil {
		t.Fatal(err)
	}
	if out := e.must("POST", "/hooks/agent", map[string]any{}); out["completed"].(float64) != 1 {
		t.Fatalf("agent = %v", out)
	}
	if got, _ := e.a.Rental.Get(ctx, inst.ID); got.State != rental.StateRunning {
		t.Fatalf("after agent = %s, want running", got.State)
	}
	e.must("POST", "/hooks/clock/advance", map[string]int{"seconds": 86400})
	e.must("POST", "/hooks/sweep", map[string]any{})
	if got, _ := e.a.Rental.Get(ctx, inst.ID); got.State != rental.StateStopped {
		t.Fatalf("after sweep = %s, want stopped (sweep must heartbeat first)", got.State)
	}

	// 开通失败：第二台机器上下单后 Agent 回报失败 → failed + create_failed 告警
	u2 := e.must("POST", "/hooks/users", map[string]any{"verified": true, "balance": 100000})
	_, inst2, err := e.a.Rental.Place(ctx, rental.PlaceRequest{UserID: int64(u2["id"].(float64)), ProductID: dbtest.ProductID(t, e.a.Fleet.Pool()),
		ImageID: dbtest.ImageID(t, e.a.Fleet.Pool()), Duration: catalog.D1d})
	if err != nil {
		t.Fatal(err)
	}
	e.must("POST", "/hooks/agent", map[string]any{"fail": true})
	if got, _ := e.a.Rental.Get(ctx, inst2.ID); got.State != rental.StateFailed {
		t.Fatalf("after failed agent = %s, want failed", got.State)
	}
	unresolved := false
	alerts, _, err := e.a.Fleet.ListAlerts(ctx, &unresolved, paging.Page{Number: 1, Size: 20})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, al := range alerts {
		if al.Kind == "create_failed" && al.InstanceID == inst2.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("create_failed alert missing: %+v", alerts)
	}
}

func TestSweepWithoutHeartbeatSuspends(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.must("POST", "/hooks/machines", map[string]any{"count": 1})
	u := e.must("POST", "/hooks/users", map[string]any{"verified": true, "balance": 100000})
	_, inst, err := e.a.Rental.Place(ctx, rental.PlaceRequest{UserID: int64(u["id"].(float64)), ProductID: dbtest.ProductID(t, e.a.Fleet.Pool()),
		ImageID: dbtest.ImageID(t, e.a.Fleet.Pool()), Duration: catalog.D1d})
	if err != nil {
		t.Fatal(err)
	}
	e.must("POST", "/hooks/agent", map[string]any{})
	e.must("POST", "/hooks/clock/advance", map[string]int{"seconds": 360})
	e.must("POST", "/hooks/sweep", map[string]any{"heartbeat": false})
	if got, _ := e.a.Rental.Get(ctx, inst.ID); !got.Suspended() || got.State != rental.StateRunning {
		t.Fatalf("after offline sweep = %+v, want suspended running", got)
	}
}

func TestAdminsHook(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	out := e.must("POST", "/hooks/admins", map[string]any{"username": "e2e_root", "role": "super", "bind_totp": true})
	if out["password"] != "e2e-password-123456" || out["totp_secret"] == "" || len(out["recovery_codes"].([]any)) != 8 {
		t.Fatalf("admins = %v", out)
	}
	a, err := e.a.Admins.FindByUsername(ctx, "e2e_root")
	if err != nil || !a.TOTPConfirmed || string(a.Role) != "super" {
		t.Fatalf("admin = %+v, %v", a, err)
	}
	res, err := e.a.Admins.Login(ctx, "e2e_root", "e2e-password-123456", "127.0.0.1")
	if err != nil || res.NeedTOTPSetup {
		t.Fatalf("login = %+v, %v", res, err)
	}
	out = e.must("POST", "/hooks/admins", map[string]any{"username": "e2e_ops", "role": "ops", "bind_totp": false})
	if _, ok := out["totp_secret"]; ok {
		t.Fatalf("unbound admin must not return secret: %v", out)
	}
	if a, _ := e.a.Admins.FindByUsername(ctx, "e2e_ops"); a.TOTPConfirmed {
		t.Fatal("ops must not be bound")
	}
	if code, _ := e.call("POST", "/hooks/admins", map[string]any{"username": "e2e_root", "role": "super", "bind_totp": false}); code != 500 {
		t.Fatalf("duplicate username = %d, want 500", code)
	}
}

func TestKYCHook(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	u := e.must("POST", "/hooks/users", map[string]any{"verified": false})
	uid := int64(u["id"].(float64))
	e.must("POST", "/hooks/kyc", map[string]bool{"reject": true})
	if _, err := e.a.Identity.SubmitPersonalKYC(ctx, uid, "张三", "11010119900101123X"); apperr.As(err) == nil || apperr.As(err).Code != "kyc_rejected" {
		t.Fatalf("rejected kyc = %v", err)
	}
	e.must("POST", "/hooks/kyc", map[string]bool{"reject": false})
	if user, err := e.a.Identity.SubmitPersonalKYC(ctx, uid, "张三", "11010119900101123X"); err != nil || user.KYCStatus != "verified" {
		t.Fatalf("passed kyc = %+v, %v", user, err)
	}
}

func TestLoopbackOnly(t *testing.T) {
	e := newEnv(t)
	h := (&e2ehooks.Server{App: e.a, Clock: e.clk, KYC: &e2ehooks.SwitchableKYC{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Handler()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.RemoteAddr = "10.0.0.5:1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-loopback = %d, want 403", rec.Code)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/e2ehooks/`
Expected: 编译错误（包不存在）。

- [ ] **Step 3: 实现 `internal/e2ehooks`**

`internal/e2ehooks/kyc.go`：

```go
package e2ehooks

import (
	"context"
	"sync/atomic"

	"spark-cloud/internal/plugins/kyc"
)

// SwitchableKYC 是可在运行中切换「全部通过 / 全部不通过」的实名核验 mock。
// kyc.Mock 是值类型、Reject 是普通字段，不能在服务运行时修改（数据竞争），所以端到端服务用它代替。
type SwitchableKYC struct {
	reject atomic.Bool
}

func (k *SwitchableKYC) SetReject(v bool) { k.reject.Store(v) }

func (k *SwitchableKYC) VerifyPerson(context.Context, string, string, string) (kyc.Result, error) {
	if k.reject.Load() {
		return kyc.Result{Passed: false, Reason: "姓名、身份证号与手机号不一致"}, nil
	}
	return kyc.Result{Passed: true}, nil
}
```

`internal/e2ehooks/hooks.go`：

```go
// Package e2ehooks 为 cmd/e2e-server 提供端到端测试钩子：推进假时钟、模拟 Agent、造数据、切换实名 mock。
// 只被 cmd/e2e-server 引用，生产二进制不包含它。所有钩子用 app.App 暴露的服务实现，不复制业务逻辑。
package e2ehooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/admin"
	"spark-cloud/internal/app"
	"spark-cloud/internal/audit"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/randx"
)

// SeedPassword 是钩子创建的管理员的固定密码。
const SeedPassword = "e2e-password-123456"

type Server struct {
	App   *app.App
	Pool  *pgxpool.Pool
	Clock *clock.Fake
	KYC   *SwitchableKYC
	Log   *slog.Logger
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]bool{"ok": true}) })
	mux.HandleFunc("GET /hooks/clock", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]any{"now": s.Clock.Now()}) })
	mux.HandleFunc("POST /hooks/clock/advance", s.handle(s.advance))
	mux.HandleFunc("POST /hooks/sweep", s.handle(s.sweep))
	mux.HandleFunc("POST /hooks/agent", s.handle(s.agent))
	mux.HandleFunc("POST /hooks/machines", s.handle(s.machines))
	mux.HandleFunc("POST /hooks/users", s.handle(s.users))
	mux.HandleFunc("POST /hooks/admins", s.handle(s.admins))
	mux.HandleFunc("POST /hooks/kyc", s.handle(s.kyc))
	return loopbackOnly(mux)
}

// loopbackOnly 只接受来自 127.0.0.1 / ::1 的连接（监听地址也是 127.0.0.1，这里是第二道保险）。
func loopbackOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type badRequest string

func (b badRequest) Error() string { return string(b) }

// handle 统一解析 JSON 请求体、输出结果或错误（400 参数错误，其余 500 并记日志）。
func (s *Server) handle(fn func(ctx context.Context, body json.RawMessage) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body json.RawMessage
		if r.ContentLength != 0 {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				writeJSON(w, 400, map[string]string{"error": "请求体必须是 JSON"})
				return
			}
		}
		if len(body) == 0 {
			body = json.RawMessage("{}")
		}
		out, err := fn(r.Context(), body)
		var br badRequest
		switch {
		case errors.As(err, &br):
			writeJSON(w, 400, map[string]string{"error": err.Error()})
		case err != nil:
			s.Log.Error("hook failed", "path", r.URL.Path, "err", err)
			writeJSON(w, 500, map[string]string{"error": err.Error()})
		default:
			writeJSON(w, 200, out)
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func decode[T any](body json.RawMessage) (T, error) {
	var v T
	if err := json.Unmarshal(body, &v); err != nil {
		return v, badRequest("请求参数格式不正确: " + err.Error())
	}
	return v, nil
}

func (s *Server) advance(_ context.Context, body json.RawMessage) (any, error) {
	req, err := decode[struct {
		Seconds int64 `json:"seconds"`
	}](body)
	if err != nil {
		return nil, err
	}
	if req.Seconds <= 0 {
		return nil, badRequest("seconds 必须为正数（假时钟只能向前）")
	}
	s.Clock.Advance(time.Duration(req.Seconds) * time.Second)
	return map[string]any{"now": s.Clock.Now()}, nil
}

func (s *Server) machineIDs(ctx context.Context) ([]int64, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id FROM machines ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Server) heartbeatAll(ctx context.Context) error {
	ids, err := s.machineIDs(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := s.App.Fleet.Heartbeat(ctx, id); err != nil {
			return fmt.Errorf("heartbeat %d: %w", id, err)
		}
	}
	return nil
}

// sweep 先对全部机器心跳（除非 heartbeat=false），再跑一轮 Rental.Sweep + Billing.Sweep。
// Rental.Sweep 先做离线检测与暂停、再做到期停机，而到期停机只处理未暂停的实例：推进 24 小时后不心跳直接清扫，
// 机器会被判失联、实例被暂停而永远不会停机。
func (s *Server) sweep(ctx context.Context, body json.RawMessage) (any, error) {
	req, err := decode[struct {
		Heartbeat *bool `json:"heartbeat"`
	}](body)
	if err != nil {
		return nil, err
	}
	if req.Heartbeat == nil || *req.Heartbeat {
		if err := s.heartbeatAll(ctx); err != nil {
			return nil, err
		}
	}
	if err := s.App.Rental.Sweep(ctx); err != nil {
		return nil, fmt.Errorf("rental sweep: %w", err)
	}
	if err := s.App.Billing.Sweep(ctx); err != nil {
		return nil, fmt.Errorf("billing sweep: %w", err)
	}
	return map[string]bool{"ok": true}, nil
}

// agent 模拟 Agent：对指定机器（缺省全部）心跳，并把全部待处理指令按成功 / 失败回报。
func (s *Server) agent(ctx context.Context, body json.RawMessage) (any, error) {
	req, err := decode[struct {
		MachineID *int64 `json:"machine_id"`
		Fail      bool   `json:"fail"`
		Result    string `json:"result"`
	}](body)
	if err != nil {
		return nil, err
	}
	ids := []int64{}
	if req.MachineID != nil {
		ids = append(ids, *req.MachineID)
	} else if ids, err = s.machineIDs(ctx); err != nil {
		return nil, err
	}
	result := req.Result
	if result == "" {
		if req.Fail {
			result = "e2e: simulated failure"
		} else {
			result = "ok"
		}
	}
	completed := 0
	for _, mid := range ids {
		if _, err := s.App.Fleet.Heartbeat(ctx, mid); err != nil {
			return nil, fmt.Errorf("heartbeat %d: %w", mid, err)
		}
		cmds, err := s.App.Fleet.PendingCommands(ctx, mid)
		if err != nil {
			return nil, err
		}
		for _, c := range cmds {
			if err := s.App.Rental.HandleCommandResult(ctx, c.ID, !req.Fail, result); err != nil {
				return nil, fmt.Errorf("command %s (%s): %w", c.ID, c.Type, err)
			}
			completed++
		}
	}
	return map[string]int{"completed": completed}, nil
}

func (s *Server) machines(ctx context.Context, body json.RawMessage) (any, error) {
	req, err := decode[struct {
		Count        int    `json:"count"`
		SerialPrefix string `json:"serial_prefix"`
	}](body)
	if err != nil {
		return nil, err
	}
	if req.Count <= 0 || req.Count > 50 {
		return nil, badRequest("count 必须在 1～50 之间")
	}
	prefix := req.SerialPrefix
	if prefix == "" {
		prefix = "E2E-" + randx.Digits(6) + "-"
	}
	pid, err := s.App.Catalog.ProductIDByCode(ctx, "spark-1")
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, req.Count)
	for i := 1; i <= req.Count; i++ {
		m, err := s.App.Fleet.Register(ctx, fleet.NewMachine{Serial: fmt.Sprintf("%s%d", prefix, i), Rack: "E2E", Slot: fmt.Sprint(i),
			LanIP: fmt.Sprintf("10.99.%d.%d", (len(ids)/250)%250, len(ids)%250+1), ProductID: pid})
		if err != nil {
			return nil, err
		}
		if _, err := s.App.Fleet.Heartbeat(ctx, m.ID); err != nil {
			return nil, err
		}
		ids = append(ids, m.ID)
	}
	return map[string]any{"ids": ids}, nil
}

// users 创建用户：用户行直接插入（与 dbtest.CreateUser 相同），余额与分页流水通过 Wallet.AdminAdjust 写入。
// AdminAdjust 拒绝 60 秒内完全相同的调账，而假时钟下这 60 秒永远不会过去，所以每条流水的备注都不同。
func (s *Server) users(ctx context.Context, body json.RawMessage) (any, error) {
	req, err := decode[struct {
		Phone      string `json:"phone"`
		Verified   bool   `json:"verified"`
		Balance    int64  `json:"balance"`
		LedgerRows int    `json:"ledger_rows"`
	}](body)
	if err != nil {
		return nil, err
	}
	if req.Phone == "" {
		req.Phone = "139" + randx.Digits(8)
	}
	if req.LedgerRows < 0 || req.LedgerRows > 200 || req.Balance < 0 || int64(req.LedgerRows) > req.Balance && req.Balance > 0 {
		return nil, badRequest("ledger_rows 需在 0～200 之间且不大于 balance（分）")
	}
	kyc := "none"
	if req.Verified {
		kyc = "verified"
	}
	now := s.Clock.Now()
	var id int64
	if err := s.Pool.QueryRow(ctx, `INSERT INTO users (phone, kind, kyc_status, created_at, updated_at)
		VALUES ($1, 'personal', $2, $3, $3) RETURNING id`, req.Phone, kyc, now).Scan(&id); err != nil {
		return nil, err
	}
	for i := 1; i <= req.LedgerRows; i++ {
		if _, err := s.App.Wallet.AdminAdjust(ctx, 0, id, 1, fmt.Sprintf("e2e-seed-%d", i), "127.0.0.1"); err != nil {
			return nil, err
		}
	}
	if rest := req.Balance - int64(req.LedgerRows); rest > 0 {
		if _, err := s.App.Wallet.AdminAdjust(ctx, 0, id, rest, "e2e-seed-balance", "127.0.0.1"); err != nil {
			return nil, err
		}
	}
	return map[string]any{"id": id, "phone": req.Phone}, nil
}

// admins 创建管理员；bind_totp 为真时走命令行同款路径 EnrollTOTP → 按假时钟算码 → ConfirmEnrollment。
func (s *Server) admins(ctx context.Context, body json.RawMessage) (any, error) {
	req, err := decode[struct {
		Username string `json:"username"`
		Role     string `json:"role"`
		BindTOTP bool   `json:"bind_totp"`
	}](body)
	if err != nil {
		return nil, err
	}
	a, err := s.App.Admins.Create(ctx, audit.Actor{}, req.Username, SeedPassword, admin.Role(req.Role))
	if err != nil {
		return nil, err
	}
	out := map[string]any{"id": a.ID, "password": SeedPassword}
	if !req.BindTOTP {
		return out, nil
	}
	uri, err := s.App.Admins.EnrollTOTP(ctx, a.ID)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(uri)
	if err != nil {
		return nil, err
	}
	secret := u.Query().Get("secret")
	code, err := admin.TOTPCode(secret, admin.TOTPStep(s.Clock.Now()))
	if err != nil {
		return nil, err
	}
	codes, err := s.App.Admins.ConfirmEnrollment(ctx, a.ID, code)
	if err != nil {
		return nil, err
	}
	out["totp_secret"], out["recovery_codes"] = secret, codes
	return out, nil
}

func (s *Server) kyc(_ context.Context, body json.RawMessage) (any, error) {
	req, err := decode[struct {
		Reject bool `json:"reject"`
	}](body)
	if err != nil {
		return nil, err
	}
	s.KYC.SetReject(req.Reject)
	return map[string]bool{"ok": true}, nil
}
```

- [ ] **Step 4: 运行钩子测试**

Run: `go test ./internal/e2ehooks/ -v`
Expected: `TestClockHooks`、`TestMachinesUsersAgentAndSweep`、`TestSweepWithoutHeartbeatSuspends`、`TestAdminsHook`、`TestKYCHook`、`TestLoopbackOnly` 全部 PASS。

- [ ] **Step 5: 实现 `cmd/e2e-server/main.go`**

```go
// e2e-server 是端到端测试专用后端：嵌入式 PostgreSQL + mock 插件 + 假时钟，复用与生产相同的 app.Build 与 App.Servers，
// 另在 127.0.0.1:18090 提供测试钩子（internal/e2ehooks）。不随 make build 发布；cmd/platform 不包含任何钩子代码。
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"

	"spark-cloud/internal/app"
	"spark-cloud/internal/e2ehooks"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/config"
	"spark-cloud/internal/platform/db"
)

const (
	publicAddr = "127.0.0.1:18080"
	adminAddr  = "127.0.0.1:18081"
	hooksAddr  = "127.0.0.1:18090"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "e2e-server:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	dir, err := os.MkdirTemp("", "spark-e2e-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	port := freePort()
	pg := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Port(uint32(port)).
		RuntimePath(filepath.Join(dir, "runtime")).
		DataPath(filepath.Join(dir, "data")).
		Logger(io.Discard))
	if err := pg.Start(); err != nil {
		return fmt.Errorf("start embedded postgres: %w", err)
	}
	defer pg.Stop() //nolint:errcheck
	dbURL := fmt.Sprintf("postgres://postgres:postgres@localhost:%d/postgres?sslmode=disable", port)
	if err := db.Migrate(ctx, dbURL); err != nil {
		return err
	}
	pool, err := db.Open(ctx, dbURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	cfg := config.Config{
		Env: "dev", HTTPAddr: publicAddr, AdminAddr: adminAddr, DatabaseURL: dbURL, Domain: "spark.test",
		SecretKey: bytes.Repeat([]byte("e"), 32), SMSDriver: "mock", KYCDriver: "mock", PayDriver: "mock", PDUDriver: "mock",
		UploadDir: filepath.Join(dir, "uploads"), SweepInterval: time.Minute,
	}
	clk := clock.NewFake(time.Now())
	kycSwitch := &e2ehooks.SwitchableKYC{}
	a, err := app.Build(cfg, pool, log, app.Options{Clock: clk, KYC: kycSwitch, DisableRateLimits: true})
	if err != nil {
		return err
	}
	public, adminSrv := a.Servers(cfg) // 与 cmd/platform 完全相同的监听与路由；不启动定时清扫器，清扫由钩子触发
	hooks := &http.Server{
		Addr:              hooksAddr,
		Handler:           (&e2ehooks.Server{App: a, Pool: pool, Clock: clk, KYC: kycSwitch, Log: log}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	servers := []*http.Server{public, adminSrv, hooks}
	errc := make(chan error, len(servers))
	for _, s := range servers {
		go func(s *http.Server) { errc <- s.ListenAndServe() }(s)
	}
	for _, s := range servers {
		if err := waitListening(ctx, s.Addr); err != nil {
			return err
		}
	}
	fmt.Println("E2E READY") // Playwright 以 GET /healthz 为就绪探测；这一行便于人工查看
	log.Info("e2e-server ready", "console", "http://"+publicAddr, "admin", "http://"+adminAddr+"/admin/", "hooks", "http://"+hooksAddr)

	var runErr error
	select {
	case <-ctx.Done():
	case err := <-errc:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			runErr = err
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, s := range servers {
		if err := s.Shutdown(shutdownCtx); err != nil && runErr == nil {
			runErr = err
		}
	}
	return runErr
}

func freePort() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// waitListening 等待端口可连接（最多 10 秒）。
func waitListening(ctx context.Context, addr string) error {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
			c.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	return fmt.Errorf("%s 未在 10 秒内开始监听", addr)
}
```

- [ ] **Step 6: 手工冒烟**

Run（后台启动、探测、再停止）：

```bash
go run ./cmd/e2e-server & E2E_PID=$!
for i in $(seq 1 60); do curl -sf http://127.0.0.1:18090/healthz && break; sleep 2; done; echo
curl -s http://127.0.0.1:18090/hooks/clock; echo
curl -s -X POST http://127.0.0.1:18090/hooks/machines -d '{"count":1}'; echo
curl -s -X POST http://127.0.0.1:18090/hooks/users -d '{"verified":true,"balance":100000}'; echo
curl -si http://127.0.0.1:18080/api/v1/products | head -1
curl -si http://127.0.0.1:18080/market | head -1
curl -si http://127.0.0.1:18081/admin/ | head -1
kill -TERM $E2E_PID; wait $E2E_PID; echo "exit=$?"
```

Expected：`{"ok":true}`；`{"now":"..."}`；`{"ids":[1]}`；`{"id":1,"phone":"139..."}`；`HTTP/1.1 200 OK`；`HTTP/1.1 503 Service Unavailable`（尚未构建前端）；`HTTP/1.1 503 Service Unavailable`；进程收到 SIGTERM 后退出码 0，`/tmp/spark-e2e-*` 目录被删除（`ls -d /tmp/spark-e2e-* 2>/dev/null` 无输出；macOS 上 `os.TempDir()` 为 `$TMPDIR`）。

- [ ] **Step 7: 全量验证并提交**

Run: `go vet ./... && go test ./...`
Expected: 全部 PASS。

```bash
git add internal/e2ehooks cmd/e2e-server
git commit -m "feat(e2e): e2e-server with embedded postgres, fake clock and test hooks

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---
### Task 5: web 工作区脚手架、`@spark/shared` 纯函数、Makefile（B3）

**Files:**
- Create: `web/go.mod`、`web/package.json`、`web/tsconfig.base.json`、`web/vitest.config.ts`
- Create: `web/shared/package.json`、`web/shared/tsconfig.json`、`web/shared/vitest.config.ts`
- Create: `web/shared/src/{index,brand,errors,format,device,idempotency,labels}.ts`
- Create: `web/shared/src/__tests__/{format,device,idempotency,errors,labels}.spec.ts`
- Modify: `Makefile`

**Interfaces:**
- Consumes: 无。
- Produces（后续所有前端任务依赖，从 `@spark/shared` 导入）:
  - `brand = { name: 'Spark Cloud', shortName: 'Spark', logoUrl: 'brand/logo.svg', primaryColor: '#2563eb', supportText: '如需帮助请联系客服' }`（`logoUrl` 不带前导 `/`，各应用用 `import.meta.env.BASE_URL + brand.logoUrl`，后台挂在 `/admin/` 才能找到图片）。
  - `class ApiError extends Error { status: number; code: string; message: string; requestId: string; details: Record<string, unknown> }`（`details` 是后端错误响应体的完整对象，供 `import_invalid` 这类带 `errors` 明细的错误使用；网络错误为 `{}`）、`isApiError(e): e is ApiError`、`messageOf(err: unknown): string`。
  - `fmtYuan(cents: number): string`、`yuanToCents(input: string): number | null`、`fmtTime(iso?: string | Date | null): string`（`yyyy-MM-dd HH:mm`）、`fmtTimeSec`（到秒）、`fmtDate`（`yyyy-MM-dd`）、`fmtDuration(seconds: number): string`、`useCountdown(opts?: { now?: () => number; onZero?: () => void }): { remaining: Ref<number>; start(seconds: number): void; stop(): void }`。
  - `isMobileUA(ua?: string): boolean`、`useMobile(): Ref<boolean>`（断点 767px）。
  - `createIdempotencyKey(): { current(): string; rotate(): void }`、`useIdempotencyKey(deps: () => unknown)`（`deps` 变化自动 `rotate`）。
  - `labels.ts`：`instanceStateLabel`、`instanceStateType`（`el-tag` 的 type：creating→`primary`、running→`success`、stopped→`warning`、wiping→`info`、released→`info`、failed→`danger`）、`orderKindLabel`、`orderStatusLabel`、`ledgerKindLabel`、`rechargeStatusLabel`、`rechargeChannelLabel`、`machineStateLabel`、`alertLevelLabel`、`alertKindLabel`、`roleLabel`、`kycStatusLabel`、`durationLabel`、`auditActions: string[]`，以及 `labelOf(map, value)`（未知值原样返回）。
  - 根脚本：`npm test`（Vitest 全部项目）、`npm run build`（shared 类型检查 + 各应用构建）、`npm run e2e`、`npm run e2e:coverage`（Task 7 提供脚本文件）。
  - Makefile 目标：`web-install`、`web`、`web-test`、`e2e`（先 `web`，再 `e2e:coverage`，再 `playwright test`）、`check`（`test` + `e2e`）；`build` 依赖 `web`；`test` 末尾追加 `web-test`。

- [ ] **Step 1: 建立工作区骨架**

`web/go.mod`（把 `node_modules` 排除出主模块的 `go vet ./...`）：

```
module spark-cloud/web

go 1.27.2
```

`web/package.json`：

```json
{
  "name": "spark-cloud-web",
  "private": true,
  "workspaces": ["shared"],
  "overrides": { "typescript": "~5.9.3" },
  "scripts": {
    "dev:console": "npm run dev -w console",
    "dev:admin": "npm run dev -w admin",
    "build": "npm run typecheck -w shared && npm run -ws --if-present build",
    "test": "vitest run",
    "e2e": "playwright test",
    "e2e:coverage": "node e2e/coverage.mjs"
  },
  "devDependencies": {
    "@playwright/test": "1.64.1",
    "@vue/test-utils": "2.5.1",
    "jsdom": "30.1.3",
    "typescript": "~5.9.3",
    "vitest": "5.0.5"
  }
}
```

（`console`、`admin` 工作区分别在 Task 8、Task 15 创建时加入 `workspaces`；npm 对不存在的工作区目录会报错。）

`web/tsconfig.base.json`：

```json
{
  "compilerOptions": {
    "target": "ES2022",
    "module": "ESNext",
    "moduleResolution": "Bundler",
    "strict": true,
    "noUncheckedIndexedAccess": false,
    "jsx": "preserve",
    "skipLibCheck": true,
    "noEmit": true,
    "allowImportingTsExtensions": true,
    "esModuleInterop": true,
    "resolveJsonModule": true,
    "isolatedModules": true,
    "lib": ["ES2022", "DOM", "DOM.Iterable"],
    "baseUrl": ".",
    "paths": { "@spark/shared": ["shared/src/index.ts"] }
  }
}
```

`web/vitest.config.ts`：

```ts
import { defineConfig } from 'vitest/config'

// 每个工作区一个 Vitest 项目；console / admin / e2e 的配置在各自任务中加入。
export default defineConfig({
  test: {
    projects: ['shared/vitest.config.ts'],
  },
})
```

`web/shared/package.json`：

```json
{
  "name": "@spark/shared",
  "version": "0.0.0",
  "private": true,
  "type": "module",
  "main": "src/index.ts",
  "exports": { ".": "./src/index.ts" },
  "scripts": { "typecheck": "tsc --noEmit -p tsconfig.json" },
  "dependencies": { "vue": "3.5.44" }
}
```

`web/shared/tsconfig.json`：

```json
{ "extends": "../tsconfig.base.json", "include": ["src/**/*.ts"] }
```

`web/shared/vitest.config.ts`：

```ts
import { defineConfig } from 'vitest/config'

export default defineConfig({
  test: { name: 'shared', environment: 'jsdom', include: ['src/**/*.spec.ts'] },
})
```

Run: `cd web && npm install`
Expected: 生成 `web/package-lock.json`，`node_modules/.package-lock.json` 中 `vitest` 为 `5.0.5`、`typescript` 为 `5.9.x`（`npm ls typescript` 全部 `5.9.x`，无 7.x）。

- [ ] **Step 2: 写失败的单测（U-01～U-05、U-08～U-10 与 Review Focus 3、4）**

`web/shared/src/__tests__/format.spec.ts`：

```ts
import { describe, expect, it, vi } from 'vitest'
import { fmtDate, fmtDuration, fmtTime, fmtTimeSec, fmtYuan, useCountdown, yuanToCents } from '../format'

describe('fmtYuan (U-01)', () => {
  it.each([
    [0, '¥0.00'],
    [6900, '¥69.00'],
    [159900, '¥1,599.00'],
    [-1000, '-¥10.00'],
    [1, '¥0.01'],
    [123456789, '¥1,234,567.89'],
  ])('%d -> %s', (cents, want) => {
    expect(fmtYuan(cents)).toBe(want)
  })
})

describe('yuanToCents (U-02)', () => {
  it.each([
    ['88.5', 8850],
    ['1.234', null],
    ['-1', null],
    ['abc', null],
    ['1000000', 100000000],
    ['0', 0],
    ['69', 6900],
    ['0.01', 1],
  ])('%s -> %s', (input, want) => {
    expect(yuanToCents(input)).toBe(want)
  })

  it('边缘写法：首尾空白可接受，其余一律拒绝（Review Focus 3）', () => {
    expect(yuanToCents(' 88.50 ')).toBe(8850)
    expect(yuanToCents('100.')).toBeNull()
    expect(yuanToCents('.5')).toBeNull()
    expect(yuanToCents('1e3')).toBeNull()
    expect(yuanToCents('１００')).toBeNull()
    expect(yuanToCents('')).toBeNull()
    expect(yuanToCents('1,000')).toBeNull()
  })
})

describe('fmtTime / fmtDate 北京时间 (U-03)', () => {
  it('跨日边界：UTC 17:30 是北京时间次日 01:30', () => {
    expect(fmtTime('2026-10-17T17:30:00Z')).toBe('2026-10-18 01:30')
    expect(fmtTimeSec('2026-10-17T17:30:05Z')).toBe('2026-10-18 01:30:05')
    expect(fmtDate('2026-10-17T17:30:00Z')).toBe('2026-10-18')
  })
  it('午夜显示 00 而不是 24', () => {
    expect(fmtTime('2026-10-17T16:00:00Z')).toBe('2026-10-18 00:00')
  })
  it('空值与非法值返回空串', () => {
    expect(fmtTime(null)).toBe('')
    expect(fmtTime(undefined)).toBe('')
    expect(fmtTime('not-a-date')).toBe('')
  })
})

describe('fmtDuration (U-04)', () => {
  it.each([
    [0, '0 分钟'],
    [59, '不足 1 分钟'],
    [3600, '1 小时'],
    [86400 + 3661, '1 天 1 小时 1 分'],
    [6 * 86400 + 23 * 3600 + 59 * 60 + 30, '6 天 23 小时 59 分'],
  ])('%d -> %s', (seconds, want) => {
    expect(fmtDuration(seconds)).toBe(want)
  })
})

describe('useCountdown (U-05)', () => {
  it('以注入时间源递减并在归零触发一次回调', () => {
    vi.useFakeTimers()
    let t = 0
    const onZero = vi.fn()
    const cd = useCountdown({ now: () => t, onZero })
    cd.start(3)
    expect(cd.remaining.value).toBe(3)
    t = 1000
    vi.advanceTimersByTime(1000)
    expect(cd.remaining.value).toBe(2)
    t = 3000
    vi.advanceTimersByTime(1000)
    expect(cd.remaining.value).toBe(0)
    expect(onZero).toHaveBeenCalledTimes(1)
    t = 10000
    vi.advanceTimersByTime(5000)
    expect(onZero).toHaveBeenCalledTimes(1)
    vi.useRealTimers()
  })

  it('按时间源而不是 tick 数递减：标签页被节流后一次跳到正确值（Review Focus 4）', () => {
    vi.useFakeTimers()
    let t = 0
    const cd = useCountdown({ now: () => t })
    cd.start(600)
    t = 120_000 // 两分钟内浏览器只给了一次 tick
    vi.advanceTimersByTime(1000)
    expect(cd.remaining.value).toBe(480)
    cd.stop()
    vi.useRealTimers()
  })

  it('重新 start 会重置基准', () => {
    vi.useFakeTimers()
    let t = 0
    const cd = useCountdown({ now: () => t })
    cd.start(10)
    t = 5000
    vi.advanceTimersByTime(1000)
    expect(cd.remaining.value).toBe(5)
    cd.start(100)
    expect(cd.remaining.value).toBe(100)
    t = 6000
    vi.advanceTimersByTime(1000)
    expect(cd.remaining.value).toBe(99)
    cd.stop()
    vi.useRealTimers()
  })
})
```

`web/shared/src/__tests__/device.spec.ts`：

```ts
import { describe, expect, it } from 'vitest'
import { isMobileUA } from '../device'

describe('isMobileUA (U-09)', () => {
  it.each([
    ['Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1', true],
    ['Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Mobile Safari/537.36', true],
    ['Mozilla/5.0 (iPad; CPU OS 17_0 like Mac OS X) AppleWebKit/605.1.15', true],
    ['Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36', false],
    ['Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/120.0 Safari/537.36', false],
  ])('%s', (ua, want) => {
    expect(isMobileUA(ua)).toBe(want)
  })
})
```

`web/shared/src/__tests__/idempotency.spec.ts`：

```ts
import { nextTick, ref } from 'vue'
import { describe, expect, it } from 'vitest'
import { createIdempotencyKey, useIdempotencyKey } from '../idempotency'

describe('idempotency key (U-08)', () => {
  it('重试复用同一个键，成功后 rotate 生成新键', () => {
    const k = createIdempotencyKey()
    const first = k.current()
    expect(first).toMatch(/^[0-9a-f-]{36}$/)
    expect(k.current()).toBe(first)
    k.rotate()
    expect(k.current()).not.toBe(first)
  })

  it('参数变化时自动换键', async () => {
    const duration = ref('1d')
    const imageId = ref(1)
    const k = useIdempotencyKey(() => [duration.value, imageId.value])
    const first = k.current()
    duration.value = '1w'
    await nextTick()
    const second = k.current()
    expect(second).not.toBe(first)
    imageId.value = 2
    await nextTick()
    expect(k.current()).not.toBe(second)
    await nextTick()
    expect(k.current()).toBe(k.current())
  })
})
```

`web/shared/src/__tests__/errors.spec.ts`：

```ts
import { describe, expect, it } from 'vitest'
import { ApiError, isApiError, messageOf } from '../errors'

describe('messageOf (U-10)', () => {
  it('业务错误显示 message；5xx 追加请求号', () => {
    expect(messageOf(new ApiError(402, 'insufficient_balance', '余额不足，请先充值', 'req-1'))).toBe('余额不足，请先充值')
    expect(messageOf(new ApiError(500, 'internal', '服务器内部错误，请稍后重试', 'abc123'))).toBe('服务器内部错误，请稍后重试（请求号 abc123）')
    expect(messageOf(new ApiError(503, 'kyc_unavailable', '实名核验服务暂时不可用，请稍后再试', ''))).toBe('实名核验服务暂时不可用，请稍后再试')
  })
  it('网络错误与未知错误', () => {
    expect(messageOf(new ApiError(0, 'network', '网络异常，请稍后重试', ''))).toBe('网络异常，请稍后重试')
    expect(messageOf(new TypeError('boom'))).toBe('操作失败，请稍后重试')
    expect(messageOf(undefined)).toBe('操作失败，请稍后重试')
  })
  it('isApiError', () => {
    expect(isApiError(new ApiError(404, 'not_found', '资源不存在'))).toBe(true)
    expect(isApiError(new Error('x'))).toBe(false)
  })
})
```

`web/shared/src/__tests__/labels.spec.ts`：

```ts
import { describe, expect, it } from 'vitest'
import { alertKindLabel, auditActions, instanceStateLabel, instanceStateType, labelOf, ledgerKindLabel } from '../labels'

describe('labels', () => {
  it('实例状态中文与颜色', () => {
    expect(labelOf(instanceStateLabel, 'creating')).toBe('开通中')
    expect(labelOf(instanceStateLabel, 'stopped')).toBe('已到期停机')
    expect(labelOf(instanceStateType, 'running')).toBe('success')
    expect(labelOf(instanceStateType, 'failed')).toBe('danger')
  })
  it('未知值原样显示', () => {
    expect(labelOf(ledgerKindLabel, 'mystery')).toBe('mystery')
    expect(labelOf(alertKindLabel, 'new_kind')).toBe('new_kind')
  })
  it('审计动作列表与后端写入值一致', () => {
    expect(auditActions).toHaveLength(26)
    expect(auditActions).toContain('instance.refund_release')
    expect(auditActions).toContain('wallet.manual_recharge')
  })
})
```

Run: `cd web && npm test`
Expected: FAIL（模块不存在）。

- [ ] **Step 3: 实现共用纯函数**

`web/shared/src/brand.ts`：

```ts
// 品牌占位：替换品牌只改这一个文件与各应用 public/brand/ 下的图片。
export const brand = {
  name: 'Spark Cloud',
  shortName: 'Spark',
  logoUrl: 'brand/logo.svg', // 相对应用 base：console 为 /brand/logo.svg，admin 为 /admin/brand/logo.svg
  primaryColor: '#2563eb',
  supportText: '如需帮助请联系客服',
} as const
```

`web/shared/src/errors.ts`：

```ts
export class ApiError extends Error {
  readonly status: number
  readonly code: string
  readonly requestId: string
  /** 后端错误响应体的完整对象（例如 import_invalid 的 errors 明细）；网络错误为 {} */
  readonly details: Record<string, unknown>

  constructor(status: number, code: string, message: string, requestId = '', details: Record<string, unknown> = {}) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
    this.requestId = requestId
    this.details = details
  }
}

export function isApiError(e: unknown): e is ApiError {
  return e instanceof ApiError
}

/** 把任意错误变成可直接展示的文案；5xx 追加请求号便于排查。 */
export function messageOf(err: unknown): string {
  if (isApiError(err)) {
    if (err.status >= 500 && err.requestId) return `${err.message}（请求号 ${err.requestId}）`
    return err.message
  }
  return '操作失败，请稍后重试'
}
```

`web/shared/src/format.ts`：

```ts
import { getCurrentScope, onScopeDispose, ref, type Ref } from 'vue'

/** 分 → ¥x,xxx.xx；负数在 ¥ 前加负号。 */
export function fmtYuan(cents: number): string {
  const abs = Math.abs(Math.trunc(cents))
  const yuan = String(Math.floor(abs / 100)).replace(/\B(?=(\d{3})+(?!\d))/g, ',')
  const fen = String(abs % 100).padStart(2, '0')
  return `${cents < 0 ? '-' : ''}¥${yuan}.${fen}`
}

const yuanRe = /^(\d+)(?:\.(\d{1,2}))?$/

/** 元（字符串）→ 分；非法（负数、超过两位小数、科学计数、全角、空）返回 null。 */
export function yuanToCents(input: string): number | null {
  const m = yuanRe.exec(input.trim())
  if (!m) return null
  const yuan = Number(m[1])
  if (!Number.isSafeInteger(yuan * 100)) return null
  return yuan * 100 + Number((m[2] ?? '').padEnd(2, '0'))
}

const cst = new Intl.DateTimeFormat('en-US', {
  timeZone: 'Asia/Shanghai',
  hourCycle: 'h23',
  year: 'numeric',
  month: '2-digit',
  day: '2-digit',
  hour: '2-digit',
  minute: '2-digit',
  second: '2-digit',
})

function cstParts(iso: string | Date | null | undefined): Record<string, string> | null {
  if (iso == null || iso === '') return null
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return null
  const out: Record<string, string> = {}
  for (const p of cst.formatToParts(d)) out[p.type] = p.value
  return out
}

export function fmtDate(iso?: string | Date | null): string {
  const p = cstParts(iso)
  return p ? `${p.year}-${p.month}-${p.day}` : ''
}

export function fmtTime(iso?: string | Date | null): string {
  const p = cstParts(iso)
  return p ? `${p.year}-${p.month}-${p.day} ${p.hour}:${p.minute}` : ''
}

export function fmtTimeSec(iso?: string | Date | null): string {
  const p = cstParts(iso)
  return p ? `${p.year}-${p.month}-${p.day} ${p.hour}:${p.minute}:${p.second}` : ''
}

/** 秒 → 「6 天 23 小时 59 分」；不足 1 分钟 / 0 分钟单独处理。 */
export function fmtDuration(seconds: number): string {
  const s = Math.max(0, Math.floor(seconds))
  if (s === 0) return '0 分钟'
  if (s < 60) return '不足 1 分钟'
  const d = Math.floor(s / 86400)
  const h = Math.floor((s % 86400) / 3600)
  const m = Math.floor((s % 3600) / 60)
  const parts: string[] = []
  if (d) parts.push(`${d} 天`)
  if (h) parts.push(`${h} 小时`)
  if (m) parts.push(`${m} 分`)
  return parts.join(' ')
}

export interface CountdownOptions {
  now?: () => number
  onZero?: () => void
}

/**
 * 倒计时：以 start 时刻为基准，每秒按真实流逝时间重算剩余秒数（不是每 tick 减 1），
 * 浏览器节流后台标签页的定时器时也不会漂移；归零时触发一次 onZero 并停止。
 */
export function useCountdown(opts: CountdownOptions = {}): { remaining: Ref<number>; start: (seconds: number) => void; stop: () => void } {
  const now = opts.now ?? (() => Date.now())
  const remaining = ref(0)
  let timer: ReturnType<typeof setInterval> | undefined
  let base = 0
  let initial = 0

  function stop() {
    if (timer !== undefined) {
      clearInterval(timer)
      timer = undefined
    }
  }
  function tick() {
    const next = Math.max(0, initial - Math.floor((now() - base) / 1000))
    remaining.value = next
    if (next === 0) {
      stop()
      opts.onZero?.()
    }
  }
  function start(seconds: number) {
    stop()
    initial = Math.max(0, Math.floor(seconds))
    base = now()
    remaining.value = initial
    if (initial > 0) timer = setInterval(tick, 1000)
  }
  if (getCurrentScope()) onScopeDispose(stop)
  return { remaining, start, stop }
}
```

`web/shared/src/device.ts`：

```ts
import { getCurrentScope, onScopeDispose, ref, type Ref } from 'vue'

/** 只用于充值渠道的选择（手机只给支付宝）；布局用 useMobile。 */
export function isMobileUA(ua: string = navigator.userAgent): boolean {
  return /Android|iPhone|iPad|iPod|Mobile/i.test(ua)
}

export const MOBILE_QUERY = '(max-width: 767px)'

/** 布局断点 768px：< 768 为手机布局。 */
export function useMobile(): Ref<boolean> {
  const q = window.matchMedia(MOBILE_QUERY)
  const mobile = ref(q.matches)
  const onChange = (e: MediaQueryListEvent) => {
    mobile.value = e.matches
  }
  q.addEventListener('change', onChange)
  if (getCurrentScope()) onScopeDispose(() => q.removeEventListener('change', onChange))
  return mobile
}
```

`web/shared/src/idempotency.ts`：

```ts
import { watch } from 'vue'

export interface IdempotencyKey {
  current(): string
  rotate(): void
}

/** 打开确认框时生成一个键；同一次确认的重复点击、网络错误后的再次点击都复用它；成功后调用 rotate()。 */
export function createIdempotencyKey(gen: () => string = () => crypto.randomUUID()): IdempotencyKey {
  let key = gen()
  return {
    current: () => key,
    rotate() {
      key = gen()
    },
  }
}

/** deps 返回的参数（租期、镜像、实例）变化时自动换键。 */
export function useIdempotencyKey(deps: () => unknown): IdempotencyKey {
  const k = createIdempotencyKey()
  watch(deps, () => k.rotate(), { deep: true })
  return k
}
```

`web/shared/src/labels.ts`：

```ts
export type LabelMap = Readonly<Record<string, string>>

/** 枚举值 → 中文；未知值原样返回（后端新增枚举时不至于显示空白）。 */
export function labelOf(map: LabelMap, value: string | null | undefined): string {
  if (value == null) return ''
  return map[value] ?? value
}

export const instanceStateLabel: LabelMap = {
  creating: '开通中', running: '运行中', stopped: '已到期停机', wiping: '洗机中', released: '已释放', failed: '开通失败',
}
/** el-tag 的 type：开通中蓝、运行中绿、停机橙、洗机中灰、已释放灰、失败红。 */
export const instanceStateType: LabelMap = {
  creating: 'primary', running: 'success', stopped: 'warning', wiping: 'info', released: 'info', failed: 'danger',
}
export const orderKindLabel: LabelMap = { new: '新购', renew: '续费' }
export const orderStatusLabel: LabelMap = { paid: '已支付', refunded: '已退款', partially_refunded: '部分退款' }
export const ledgerKindLabel: LabelMap = { recharge: '充值', order_pay: '扣款', refund: '退款', admin_adjust: '调账' }
export const rechargeStatusLabel: LabelMap = { pending: '待支付', paid: '已支付', closed: '已关闭' }
export const rechargeChannelLabel: LabelMap = { wechat: '微信支付', alipay: '支付宝', manual: '手动' }
export const machineStateLabel: LabelMap = { idle: '空闲', allocated: '已分配', maintenance: '维护中', faulty: '故障' }
export const alertLevelLabel: LabelMap = { warning: '警告', critical: '严重' }
export const alertKindLabel: LabelMap = {
  machine_offline: '机器失联', create_failed: '开通失败', start_failed: '启动失败', stop_failed: '停机失败',
  wipe_failed: '洗机失败', recharge_amount_mismatch: '支付金额不符', recharge_paid_after_close: '关单后到账',
}
export const roleLabel: LabelMap = { super: '超级管理员', ops: '运维', finance: '财务' }
export const adminStatusLabel: LabelMap = { active: '正常', disabled: '已停用' }
export const userStatusLabel: LabelMap = { active: '正常', banned: '已封禁' }
export const userKindLabel: LabelMap = { personal: '个人', enterprise: '企业' }
export const kycStatusLabel: LabelMap = { none: '未认证', pending: '审核中', verified: '已认证', rejected: '未通过' }
export const kycResultLabel: LabelMap = { pending: '审核中', verified: '已通过', rejected: '未通过', error: '核验出错' }
export const durationLabel: LabelMap = { '1d': '1 天', '1w': '1 周', '1m': '1 月' }
export const commandTypeLabel: LabelMap = {
  create_instance: '开通实例', start_instance: '启动实例', stop_instance: '停止实例', wipe_instance: '洗机',
}

/** 后台审计动作（与后端写入值一致，顺序按模块）。 */
export const auditActions: readonly string[] = [
  'admin.create', 'admin.update', 'admin.reset_totp', 'admin.login', 'admin.login_recovery_code', 'admin.totp_bind', 'admin.password_change',
  'machine.register', 'machine.import', 'machine.maintenance', 'machine.power_cycle',
  'pdu.create', 'pdu.update',
  'alert.resolve',
  'instance.extend', 'instance.migrate', 'instance.rewipe', 'instance.refund_release',
  'user.ban', 'user.unban',
  'kyc.review',
  'wallet.adjust', 'wallet.manual_recharge',
  'price.update',
  'image.create', 'image.update',
]
```

`web/shared/src/index.ts`：

```ts
export * from './brand'
export * from './errors'
export * from './format'
export * from './device'
export * from './idempotency'
export * from './labels'
```

- [ ] **Step 4: 运行单测确认通过**

Run: `cd web && npm test && npm run typecheck -w shared`
Expected: shared 项目全部 PASS（format 15+、device 5、idempotency 2、errors 3、labels 3）；`tsc` 无错误。

- [ ] **Step 5: Makefile（B3）**

`Makefile` 整体替换为：

```make
export PATH := $(HOME)/.local/go/bin:$(HOME)/.local/bin:$(PATH)

DEV_DB  := postgres://postgres:postgres@localhost:5433/spark?sslmode=disable
# 仅本地开发使用的固定密钥，生产环境必须用 openssl rand -base64 32 生成
DEV_KEY := MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE=

.PHONY: test devdb run build web-install web web-test e2e check

test:
	go vet ./... && go test ./... && $(MAKE) web-test

devdb:
	go run ./cmd/devdb

run:
	SPARK_DATABASE_URL=$(DEV_DB) SPARK_SECRET_KEY=$(DEV_KEY) go run ./cmd/platform

# 前端：首次 make web-install；产物输出到 internal/web/dist/{console,admin}，由 Go embed
web-install:
	cd web && npm ci

web:
	cd web && npm run build

web-test:
	cd web && npm test

# 端到端：先构建前端（e2e-server 与生产一样从 embed 读产物），核对覆盖表，再跑 Playwright（自动拉起 cmd/e2e-server）
e2e: web
	cd web && npm run e2e:coverage && npx playwright test

# 发布产物先构建前端再编译后端，保证二进制里是最新页面
build: web
	GOOS=linux GOARCH=amd64 go build -o bin/platform-linux-amd64 ./cmd/platform
	GOOS=linux GOARCH=arm64 go build -o bin/platform-linux-arm64 ./cmd/platform

check: test e2e
```

Run: `make test`
Expected: Go 测试全部 PASS 后执行 `cd web && npm test`，Vitest PASS。

- [ ] **Step 6: 提交**

```bash
git add Makefile web/go.mod web/package.json web/package-lock.json web/tsconfig.base.json web/vitest.config.ts web/shared
git commit -m "feat(web): npm workspace, @spark/shared formatting/device/labels/idempotency with unit tests; make web targets

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---
### Task 6: `@spark/shared` 请求封装 `http.ts` 与接口类型 `types/`

**Files:**
- Create: `web/shared/src/http.ts`、`web/shared/src/types/{index,user,catalog,instance,wallet,recharge,kyc,admin}.ts`
- Modify: `web/shared/src/index.ts`（导出 `http` 与 `types`）
- Create: `web/shared/src/__tests__/http.spec.ts`

**Interfaces:**
- Consumes: Task 5 的 `ApiError`。
- Produces:
  - `createHttp(cfg: { base: string; onUnauthorized: () => void; promptTotp?: (message?: string) => Promise<string | null>; onTotpSetupRequired?: () => void; fetch?: typeof fetch }): Http`，`Http = { get<T>(path, query?, opt?), post<T>(path, body?, opt?), put<T>(path, body?, opt?), upload<T>(path, file: File), download(path, query?): Promise<{ blob: Blob; filename: string }> }`，`HttpOptions = { idempotencyKey?: string; signal?: AbortSignal; quiet?: boolean }`。
  - 行为：`credentials: 'same-origin'`；写请求 `Content-Type: application/json`；查询参数 `undefined / null / ''` 不拼入；非 2xx 解析 `{code, message, request_id}` 抛 `ApiError`（整个响应体对象放进 `details`，`import_invalid` 的逐行 `errors` 从 `err.details.errors` 取），解析失败抛 `ApiError{status, code: 'network', message: '网络异常，请稍后重试'}`，`fetch` 抛错（断网、超时）同样抛 `network`（`status 0`）；401 `unauthorized` 且非 `quiet` → `onUnauthorized()` 后仍抛出；403 `totp_setup_required` → `onTotpSetupRequired?.()` 后抛出；403 `totp_required` 且配置了 `promptTotp` → 弹框、把 `totp` 合并进原请求体重发，`invalid_totp` 最多 3 次，取消抛 `ApiError{code: 'cancelled'}`；并发收到 `totp_required` 只弹一次：后到的请求等首个请求完成二次验证后直接重发原请求（后端 5 分钟窗口内不再要求）；请求超时 30 秒，上传 / 下载 120 秒；不自动重试。
  - `buildQuery(query?): string`、`filenameOf(contentDisposition: string | null, fallback = 'export.csv'): string`（支持 `filename="x.csv"` 与 `filename*=UTF-8''...`）。
  - 类型（`@spark/shared` 导出）：`User`、`KycStatus`、`Duration`、`Price`、`Product`、`Image`、`InstanceState`、`Instance`、`Order`、`LedgerRow`、`Recharge`、`RechargeStatus`、`KYCRecord`、`Upload`、`Role`、`Admin`、`AdminLoginResult`、`Machine`、`MachineDetail`、`PDU`、`PDUInput`、`Alert`、`AdminInstance`、`InstanceEvent`、`CommandRow`、`AdminInstanceDetail`、`AdminOrder`、`AdminLedgerRow`、`AdminRecharge`、`AdminKYCRecord`、`UserDetail`、`AdminProduct`、`AdminPrice`、`AdminImage`、`ImageInput`、`AuditEntry`、`Dashboard`、`RefundQuote`、`Paged<T> = { items: T[]; total: number }`、`Listed<T> = { items: T[] }`、`ImportError = { line: number; message: string }`。

- [ ] **Step 1: 写失败的单测（U-06、U-07、U-14）**

`web/shared/src/__tests__/http.spec.ts`（在 node 环境运行，使用 Node 自带的 `Response` / `File`）：

```ts
// @vitest-environment node
import { describe, expect, it, vi } from 'vitest'
import { ApiError } from '../errors'
import { buildQuery, createHttp, filenameOf } from '../http'

type Call = { url: string; init: RequestInit; body: unknown }

function json(status: number, body: unknown, headers: Record<string, string> = {}) {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json', ...headers } })
}

/** 记录每次调用，按顺序返回预置响应（函数形式可根据请求决定响应）。 */
function fakeFetch(...responses: Array<Response | ((c: Call) => Response)>) {
  const calls: Call[] = []
  const fn = vi.fn(async (input: RequestInfo | URL, init: RequestInit = {}) => {
    const body = typeof init.body === 'string' ? JSON.parse(init.body) : init.body
    const call = { url: String(input), init, body }
    calls.push(call)
    const next = responses.shift()
    if (!next) throw new Error(`unexpected fetch #${calls.length} ${call.url}`)
    return typeof next === 'function' ? next(call) : next
  })
  return { fn: fn as unknown as typeof fetch, calls }
}

describe('buildQuery', () => {
  it('空值剔除、其余编码', () => {
    expect(buildQuery({ state: '', user_id: undefined, machine_id: null, page: 2, q: '张 三' })).toBe('?page=2&q=%E5%BC%A0+%E4%B8%89')
    expect(buildQuery({})).toBe('')
    expect(buildQuery()).toBe('')
  })
})

describe('http 基本行为 (U-06)', () => {
  it('GET 带查询参数、同源 Cookie、JSON 响应', async () => {
    const f = fakeFetch(json(200, { items: [1] }))
    const http = createHttp({ base: '/api/v1', onUnauthorized: vi.fn(), fetch: f.fn })
    const out = await http.get<{ items: number[] }>('/orders', { status: '', page: 1 })
    expect(out.items).toEqual([1])
    expect(f.calls[0].url).toBe('/api/v1/orders?page=1')
    expect(f.calls[0].init.credentials).toBe('same-origin')
    expect(f.calls[0].init.method).toBe('GET')
  })

  it('POST 带 JSON 头与 Idempotency-Key', async () => {
    const f = fakeFetch(json(201, { ok: true }))
    const http = createHttp({ base: '/api/v1', onUnauthorized: vi.fn(), fetch: f.fn })
    await http.post('/orders', { product_id: 1 }, { idempotencyKey: 'k-1' })
    const h = f.calls[0].init.headers as Record<string, string>
    expect(h['Content-Type']).toBe('application/json')
    expect(h['Idempotency-Key']).toBe('k-1')
    expect(f.calls[0].body).toEqual({ product_id: 1 })
  })

  it('错误体解析为 ApiError，并保留完整响应体 details', async () => {
    const f = fakeFetch(
      json(402, { code: 'insufficient_balance', message: '余额不足，请先充值', request_id: 'r1' }),
      json(400, { code: 'import_invalid', message: '导入数据有误，未导入任何机器', request_id: 'r2', errors: [{ line: 2, message: '内网 IP 格式不正确' }] }),
    )
    const http = createHttp({ base: '/api/v1', onUnauthorized: vi.fn(), fetch: f.fn })
    const err = await http.post('/orders', {}).catch((e) => e)
    expect(err).toBeInstanceOf(ApiError)
    expect(err).toMatchObject({ status: 402, code: 'insufficient_balance', message: '余额不足，请先充值', requestId: 'r1' })
    expect(err.details).toEqual({ code: 'insufficient_balance', message: '余额不足，请先充值', request_id: 'r1' })
    const imp = await http.post('/machines/import', { csv: 'x' }).catch((e) => e)
    expect(imp).toMatchObject({ status: 400, code: 'import_invalid' })
    expect(imp.details.errors).toEqual([{ line: 2, message: '内网 IP 格式不正确' }])
  })

  it('非 JSON 响应与网络异常映射为 network', async () => {
    const html = new Response('<html>502 Bad Gateway</html>', { status: 502, headers: { 'Content-Type': 'text/html' } })
    const f = fakeFetch(html, () => {
      throw new TypeError('Failed to fetch')
    })
    const http = createHttp({ base: '/api/v1', onUnauthorized: vi.fn(), fetch: f.fn })
    const e1 = await http.get('/wallet').catch((e) => e)
    expect(e1).toMatchObject({ code: 'network', status: 502, message: '网络异常，请稍后重试' })
    const e2 = await http.get('/wallet').catch((e) => e)
    expect(e2).toMatchObject({ code: 'network', status: 0 })
  })

  it('401 调 onUnauthorized 并抛出；quiet 不调', async () => {
    const onUnauthorized = vi.fn()
    const f = fakeFetch(json(401, { code: 'unauthorized', message: '请先登录' }), json(401, { code: 'unauthorized', message: '请先登录' }))
    const http = createHttp({ base: '/api/v1', onUnauthorized, fetch: f.fn })
    await expect(http.get('/me')).rejects.toMatchObject({ code: 'unauthorized' })
    expect(onUnauthorized).toHaveBeenCalledTimes(1)
    await expect(http.get('/me', undefined, { quiet: true })).rejects.toMatchObject({ code: 'unauthorized' })
    expect(onUnauthorized).toHaveBeenCalledTimes(1)
  })

  it('403 totp_setup_required 调 onTotpSetupRequired', async () => {
    const onTotpSetupRequired = vi.fn()
    const f = fakeFetch(json(403, { code: 'totp_setup_required', message: '请先绑定动态码' }))
    const http = createHttp({ base: '/admin/api/v1', onUnauthorized: vi.fn(), onTotpSetupRequired, fetch: f.fn })
    await expect(http.get('/me')).rejects.toMatchObject({ code: 'totp_setup_required' })
    expect(onTotpSetupRequired).toHaveBeenCalledTimes(1)
  })

  it('upload 用 multipart 字段 file，不手动设 Content-Type', async () => {
    const f = fakeFetch(json(201, { key: 'k' }))
    const http = createHttp({ base: '/api/v1', onUnauthorized: vi.fn(), fetch: f.fn })
    const file = new File([new Uint8Array([1, 2, 3])], 'license.png', { type: 'image/png' })
    await http.upload('/uploads', file)
    const init = f.calls[0].init
    expect(init.body).toBeInstanceOf(FormData)
    expect((init.body as FormData).get('file')).toBeInstanceOf(File)
    expect((init.headers as Record<string, string> | undefined)?.['Content-Type']).toBeUndefined()
  })
})

const totpRequired = () => json(403, { code: 'totp_required', message: '该操作需要输入动态码' })
const invalidTotp = () => json(400, { code: 'invalid_totp', message: '动态码错误或已使用过' })

describe('http 动态码重发 (U-07)', () => {
  it('totp_required → promptTotp → 重发时请求体含 totp', async () => {
    const promptTotp = vi.fn(async () => '123456')
    const f = fakeFetch(totpRequired(), json(200, { balance: 1 }))
    const http = createHttp({ base: '/admin/api/v1', onUnauthorized: vi.fn(), promptTotp, fetch: f.fn })
    const out = await http.post('/users/1/adjust', { amount: 100, remark: 'x' })
    expect(out).toEqual({ balance: 1 })
    expect(promptTotp).toHaveBeenCalledTimes(1)
    expect(f.calls[1].body).toEqual({ amount: 100, remark: 'x', totp: '123456' })
  })

  it('并发两个请求只弹一次；后到的请求在首个验证完成后直接重发', async () => {
    let stepped = false
    const serve = (c: Call) => {
      const body = c.body as { totp?: string } | undefined
      if (body?.totp === '123456') stepped = true
      if (!stepped && !body?.totp) return totpRequired()
      return json(200, { ok: c.url })
    }
    const f = fakeFetch(serve, serve, serve, serve)
    const promptTotp = vi.fn(() => new Promise<string | null>((resolve) => setTimeout(() => resolve('123456'), 10)))
    const http = createHttp({ base: '/admin/api/v1', onUnauthorized: vi.fn(), promptTotp, fetch: f.fn })
    const [a, b] = await Promise.all([http.post('/users/1/adjust', { amount: 1 }), http.post('/users/2/adjust', { amount: 2 })])
    expect(a).toEqual({ ok: '/admin/api/v1/users/1/adjust' })
    expect(b).toEqual({ ok: '/admin/api/v1/users/2/adjust' })
    expect(promptTotp).toHaveBeenCalledTimes(1)
    expect(f.calls).toHaveLength(4)
  })

  it('取消抛 cancelled，不再发请求', async () => {
    const f = fakeFetch(totpRequired())
    const http = createHttp({ base: '/admin/api/v1', onUnauthorized: vi.fn(), promptTotp: async () => null, fetch: f.fn })
    await expect(http.post('/admins', { username: 'x' })).rejects.toMatchObject({ code: 'cancelled' })
    expect(f.calls).toHaveLength(1)
  })

  it('invalid_totp 最多重试 3 次，之后抛出 invalid_totp', async () => {
    const promptTotp = vi.fn(async (message?: string) => (message ? 'again' : 'first'))
    const f = fakeFetch(totpRequired(), invalidTotp(), invalidTotp(), invalidTotp())
    const http = createHttp({ base: '/admin/api/v1', onUnauthorized: vi.fn(), promptTotp, fetch: f.fn })
    await expect(http.put('/products/1/prices/1d', { amount: 1 })).rejects.toMatchObject({ code: 'invalid_totp' })
    expect(promptTotp).toHaveBeenCalledTimes(3)
    expect(promptTotp.mock.calls[1][0]).toBe('动态码错误或已使用过')
    expect(f.calls).toHaveLength(4)
  })

  it('没有配置 promptTotp（控制台）时 totp_required 原样抛出', async () => {
    const f = fakeFetch(totpRequired())
    const http = createHttp({ base: '/api/v1', onUnauthorized: vi.fn(), fetch: f.fn })
    await expect(http.post('/x', {})).rejects.toMatchObject({ code: 'totp_required' })
  })
})

describe('download (U-14)', () => {
  it('CSV 响应取文件名并返回 Blob', async () => {
    const csv = new Response('﻿订单号,用户ID\n', { status: 200, headers: { 'Content-Type': 'text/csv; charset=utf-8', 'Content-Disposition': 'attachment; filename="orders-20261017-140501.csv"' } })
    const f = fakeFetch(csv)
    const http = createHttp({ base: '/admin/api/v1', onUnauthorized: vi.fn(), fetch: f.fn })
    const out = await http.download('/orders/export', { kind: 'new', status: '' })
    expect(f.calls[0].url).toBe('/admin/api/v1/orders/export?kind=new')
    expect(out.filename).toBe('orders-20261017-140501.csv')
    expect(await out.blob.text()).toContain('订单号')
  })

  it('JSON 响应按错误抛出（例如 export_too_large）', async () => {
    const f = fakeFetch(json(400, { code: 'export_too_large', message: '导出数据超过 10 万行，请缩小筛选范围' }))
    const http = createHttp({ base: '/admin/api/v1', onUnauthorized: vi.fn(), fetch: f.fn })
    await expect(http.download('/ledger/export')).rejects.toMatchObject({ code: 'export_too_large' })
  })

  it('filenameOf 支持两种写法并有兜底', () => {
    expect(filenameOf('attachment; filename="a.csv"')).toBe('a.csv')
    expect(filenameOf('attachment; filename=b.csv')).toBe('b.csv')
    expect(filenameOf("attachment; filename*=UTF-8''%E6%B5%81%E6%B0%B4.csv")).toBe('流水.csv')
    expect(filenameOf(null)).toBe('export.csv')
    expect(filenameOf('inline')).toBe('export.csv')
  })
})
```

Run: `cd web && npm test`
Expected: `http.spec.ts` FAIL（模块不存在）。

- [ ] **Step 2: 实现 `http.ts`**

`web/shared/src/http.ts`：

```ts
import { ApiError } from './errors'

export interface HttpOptions {
  idempotencyKey?: string
  signal?: AbortSignal
  /** 401 时不触发 onUnauthorized，仅用于启动时的会话探测 GET /me。 */
  quiet?: boolean
}

export interface HttpConfig {
  /** '/api/v1' 或 '/admin/api/v1' */
  base: string
  /** 401：清会话并跳登录 */
  onUnauthorized: () => void
  /** 后台：弹出动态码输入框；取消返回 null。message 为上一次 invalid_totp 的文案。 */
  promptTotp?: (message?: string) => Promise<string | null>
  /** 后台：403 totp_setup_required → 跳登录页的绑定步骤 */
  onTotpSetupRequired?: () => void
  /** 测试注入 */
  fetch?: typeof fetch
}

export interface Http {
  get<T>(path: string, query?: Record<string, unknown>, opt?: HttpOptions): Promise<T>
  post<T>(path: string, body?: unknown, opt?: HttpOptions): Promise<T>
  put<T>(path: string, body?: unknown, opt?: HttpOptions): Promise<T>
  /** multipart，字段名 file；不手动设 Content-Type，由浏览器生成边界 */
  upload<T>(path: string, file: File): Promise<T>
  /** CSV 导出：JSON 响应按错误处理，否则从 Content-Disposition 取文件名 */
  download(path: string, query?: Record<string, unknown>): Promise<{ blob: Blob; filename: string }>
}

export const REQUEST_TIMEOUT_MS = 30_000
export const TRANSFER_TIMEOUT_MS = 120_000
const TOTP_MAX_ATTEMPTS = 3

/** 查询参数值为 undefined / null / '' 时不拼入（后台列表筛选留空即「不筛选」）。 */
export function buildQuery(query?: Record<string, unknown>): string {
  if (!query) return ''
  const p = new URLSearchParams()
  for (const [k, v] of Object.entries(query)) {
    if (v === undefined || v === null || v === '') continue
    p.set(k, String(v))
  }
  const s = p.toString()
  return s ? `?${s}` : ''
}

export function filenameOf(disposition: string | null, fallback = 'export.csv'): string {
  if (!disposition) return fallback
  const star = /filename\*=(?:UTF-8'')?([^;]+)/i.exec(disposition)
  if (star) {
    try {
      return decodeURIComponent(star[1].trim().replace(/^"|"$/g, ''))
    } catch {
      /* 非法编码，退回普通写法 */
    }
  }
  const plain = /filename="?([^";]+)"?/i.exec(disposition)
  return plain ? plain[1].trim() : fallback
}

const networkError = (status = 0) => new ApiError(status, 'network', '网络异常，请稍后重试', '')
const cancelledError = () => new ApiError(0, 'cancelled', '已取消', '')

function withTimeout(ms: number, extra?: AbortSignal): AbortSignal {
  const t = AbortSignal.timeout(ms)
  return extra ? AbortSignal.any([t, extra]) : t
}

async function errorOf(resp: Response): Promise<ApiError> {
  try {
    const body = (await resp.json()) as { code?: unknown; message?: unknown; request_id?: unknown }
    if (typeof body?.code === 'string' && typeof body.message === 'string') {
      // 整个响应体放进 details：import_invalid 的逐行 errors 等明细由页面从 err.details 读取，不必重放请求
      return new ApiError(resp.status, body.code, body.message, typeof body.request_id === 'string' ? body.request_id : '', body as Record<string, unknown>)
    }
  } catch {
    /* 网关 HTML、空响应 */
  }
  return networkError(resp.status)
}

export function createHttp(cfg: HttpConfig): Http {
  const doFetch: typeof fetch = cfg.fetch ?? ((input, init) => fetch(input, init))
  // 正在进行的二次验证（含首个请求的重发）；并发收到 totp_required 的其他请求等它完成后直接重发
  let stepUp: Promise<unknown> | null = null

  async function raw<T>(method: string, path: string, body: unknown, opt: HttpOptions): Promise<T> {
    const headers: Record<string, string> = { Accept: 'application/json' }
    if (body !== undefined) headers['Content-Type'] = 'application/json'
    if (opt.idempotencyKey) headers['Idempotency-Key'] = opt.idempotencyKey
    let resp: Response
    try {
      resp = await doFetch(cfg.base + path, {
        method,
        headers,
        body: body === undefined ? undefined : JSON.stringify(body),
        credentials: 'same-origin',
        signal: withTimeout(REQUEST_TIMEOUT_MS, opt.signal),
      })
    } catch {
      throw networkError()
    }
    if (resp.ok) {
      try {
        return (await resp.json()) as T
      } catch {
        throw networkError(resp.status)
      }
    }
    throw await errorOf(resp)
  }

  function handleAuth(err: ApiError, opt: HttpOptions) {
    if (err.status === 401 && err.code === 'unauthorized' && !opt.quiet) cfg.onUnauthorized()
    if (err.status === 403 && err.code === 'totp_setup_required') cfg.onTotpSetupRequired?.()
  }

  async function retryWithTotp<T>(method: string, path: string, body: unknown, opt: HttpOptions): Promise<T> {
    if (stepUp) {
      try {
        await stepUp
      } catch (e) {
        if (e instanceof ApiError && e.code === 'cancelled') throw e // 首个被取消则一起取消
      }
      return raw<T>(method, path, body, opt) // 后端 5 分钟窗口已打开，不再需要 totp
    }
    const run = (async () => {
      let message: string | undefined
      for (let attempt = 1; ; attempt++) {
        const code = await cfg.promptTotp!(message)
        if (code === null) throw cancelledError()
        const base = body && typeof body === 'object' ? (body as Record<string, unknown>) : {}
        try {
          return await raw<T>(method, path, { ...base, totp: code }, opt)
        } catch (e) {
          if (e instanceof ApiError && e.code === 'invalid_totp' && attempt < TOTP_MAX_ATTEMPTS) {
            message = e.message
            continue
          }
          throw e
        }
      }
    })()
    stepUp = run.finally(() => {
      stepUp = null
    })
    stepUp.catch(() => {}) // 等待者各自处理错误，这里只防止未处理的 rejection
    return run
  }

  async function request<T>(method: string, path: string, body: unknown, opt: HttpOptions = {}): Promise<T> {
    try {
      return await raw<T>(method, path, body, opt)
    } catch (e) {
      if (!(e instanceof ApiError)) throw e
      handleAuth(e, opt)
      if (e.status === 403 && e.code === 'totp_required' && cfg.promptTotp) return retryWithTotp<T>(method, path, body, opt)
      throw e
    }
  }

  return {
    get: (path, query, opt) => request(`GET`, path + buildQuery(query), undefined, opt),
    post: (path, body, opt) => request('POST', path, body ?? {}, opt),
    put: (path, body, opt) => request('PUT', path, body ?? {}, opt),
    async upload<T>(path: string, file: File): Promise<T> {
      const fd = new FormData()
      fd.append('file', file, file.name)
      let resp: Response
      try {
        resp = await doFetch(cfg.base + path, { method: 'POST', body: fd, credentials: 'same-origin', signal: AbortSignal.timeout(TRANSFER_TIMEOUT_MS) })
      } catch {
        throw networkError()
      }
      if (resp.ok) return (await resp.json()) as T
      const err = await errorOf(resp)
      handleAuth(err, {})
      throw err
    },
    async download(path, query) {
      let resp: Response
      try {
        resp = await doFetch(cfg.base + path + buildQuery(query), { credentials: 'same-origin', signal: AbortSignal.timeout(TRANSFER_TIMEOUT_MS) })
      } catch {
        throw networkError()
      }
      const ct = resp.headers.get('Content-Type') ?? ''
      if (!resp.ok || ct.includes('application/json')) {
        const err = await errorOf(resp)
        handleAuth(err, {})
        throw err
      }
      return { blob: await resp.blob(), filename: filenameOf(resp.headers.get('Content-Disposition')) }
    },
  }
}
```

- [ ] **Step 3: 接口类型**

`web/shared/src/types/user.ts`：

```ts
export type KycStatus = 'none' | 'pending' | 'verified' | 'rejected'
export type UserKind = 'personal' | 'enterprise'
export type UserStatus = 'active' | 'banned'

export interface User {
  id: number
  phone: string
  kind: UserKind
  kyc_status: KycStatus
  status: UserStatus
  created_at: string
}
```

`web/shared/src/types/catalog.ts`：

```ts
export type Duration = '1d' | '1w' | '1m'
export const DURATIONS: readonly Duration[] = ['1d', '1w', '1m']

export interface Price {
  duration: Duration
  amount: number
}

export interface ProductSpec {
  gpu: string
  memory_gb: number
  disk_tb: number
  cpu: string
}

export interface Product {
  id: number
  code: string
  name: string
  spec: ProductSpec
  prices: Price[]
  idle_count: number
}

export interface Image {
  id: number
  name: string
  description: string
}
```

`web/shared/src/types/instance.ts`：

```ts
import type { Duration } from './catalog'

export type InstanceState = 'creating' | 'running' | 'stopped' | 'wiping' | 'released' | 'failed'

export interface Instance {
  id: string
  state: InstanceState
  product_id: number
  image_id: number
  product_name: string
  image_name: string
  suspended: boolean
  expires_at: string | null
  remaining_seconds: number
  pending_seconds: number
  retain_until: string | null
  ssh_command: string
  created_at: string
}

export type OrderKind = 'new' | 'renew'
export type OrderStatus = 'paid' | 'refunded' | 'partially_refunded'

export interface Order {
  id: string
  kind: OrderKind
  instance_id: string
  product_id: number
  duration: Duration
  amount: number
  status: OrderStatus
  refunded_amount: number
  created_at: string
}
```

`web/shared/src/types/wallet.ts`：

```ts
export type LedgerKind = 'recharge' | 'order_pay' | 'refund' | 'admin_adjust' | (string & {})

export interface LedgerRow {
  id: number
  amount: number
  balance_after: number
  kind: LedgerKind
  ref_type: string
  ref_id: string
  remark: string
  created_at: string
}
```

`web/shared/src/types/recharge.ts`：

```ts
export type RechargeStatus = 'pending' | 'paid' | 'closed'
export type RechargeChannel = 'wechat' | 'alipay' | 'manual' | (string & {})

export interface Recharge {
  id: string
  channel: RechargeChannel
  amount: number
  status: RechargeStatus
  qr_code?: string
  remaining_seconds: number
  expires_at: string
  paid_at: string | null
  closed_at: string | null
  created_at: string
}
```

`web/shared/src/types/kyc.ts`：

```ts
export type KycResult = 'pending' | 'verified' | 'rejected' | 'error'

/** 用户端 GET /kyc 返回的记录（个人 real_name 已脱敏）。 */
export interface KYCRecord {
  id: number
  kind: 'personal' | 'enterprise'
  real_name: string
  license_no?: string
  result: KycResult
  reason: string
  created_at: string
  updated_at: string
}

export interface Upload {
  key: string
  size: number
  content_type: string
  purpose: string
  created_at: string
}
```

`web/shared/src/types/admin.ts`：

```ts
import type { Duration } from './catalog'
import type { InstanceState, OrderKind, OrderStatus } from './instance'
import type { KycResult } from './kyc'
import type { LedgerKind, LedgerRow } from './wallet'
import type { RechargeChannel, RechargeStatus } from './recharge'
import type { User } from './user'

export interface Paged<T> {
  items: T[]
  total: number
}
export interface Listed<T> {
  items: T[]
}

export type Role = 'super' | 'ops' | 'finance'

export interface Admin {
  id: number
  username: string
  role: Role
  status: 'active' | 'disabled'
  totp_confirmed: boolean
  last_login_at: string | null
  last_login_ip: string
  created_at: string
}

export interface AdminLoginResult {
  next: 'totp' | 'totp_setup'
  otpauth_uri: string
}

export type MachineState = 'idle' | 'allocated' | 'maintenance' | 'faulty'

export interface Machine {
  id: number
  serial: string
  rack: string
  slot: string
  lan_ip: string
  product_id: number
  state: MachineState
  agent_online: boolean
  last_seen_at: string | null
  pdu_id: number | null
  pdu_outlet: number | null
  created_at: string
}

export interface AdminInstance {
  id: string
  user_id: number
  machine_id: number
  product_id: number
  image_id: number
  state: InstanceState
  suspended: boolean
  pending_seconds: number
  remaining_seconds: number
  expires_at: string | null
  stopped_at: string | null
  created_at: string
}

export interface MachineDetail {
  machine: Machine
  current_instance: AdminInstance | null
  history: AdminInstance[]
}

export interface ImportError {
  line: number
  message: string
}

export interface PDU {
  id: number
  name: string
  driver: 'snmp' | 'mock'
  address: string
  has_community: boolean
  oid_prefix: string
  cycle_value: number
  on_value: number
  created_at: string
}

export interface PDUInput {
  name: string
  driver: 'snmp' | 'mock'
  address: string
  community: string
  oid_prefix: string
  cycle_value: number
  on_value: number
}

export interface Alert {
  id: number
  level: 'warning' | 'critical'
  kind: string
  machine_id: number | null
  instance_id: string
  message: string
  created_at: string
  resolved_at: string | null
  resolved_by: number | null
}

export interface InstanceEvent {
  id: number
  from_state: string
  to_state: string
  reason: string
  at: string
}

export interface CommandRow {
  id: string
  machine_id: number
  type: string
  status: string
  result: string
  attempts: number
  created_at: string
}

export interface AdminOrder {
  id: string
  user_id: number
  kind: OrderKind
  instance_id: string
  product_id: number
  duration: Duration
  amount: number
  refunded_amount: number
  status: OrderStatus
  created_at: string
}

export interface AdminInstanceDetail {
  instance: AdminInstance
  events: InstanceEvent[]
  orders: AdminOrder[]
  commands: CommandRow[]
}

export interface RefundQuote {
  instance_id: string
  remaining_seconds: number
  unit_order_id: string
  suggested: number
  max: number
}

export interface AdminLedgerRow extends LedgerRow {
  user_id: number
  operator_id: number | null
  kind: LedgerKind
}

export interface AdminRecharge {
  id: string
  user_id: number
  channel: RechargeChannel
  amount: number
  status: RechargeStatus
  channel_trade_no?: string
  operator_id?: number | null
  remark?: string
  expires_at: string
  paid_at: string | null
  closed_at: string | null
  created_at: string
}

export interface AdminKYCRecord {
  id: number
  user_id: number
  kind: 'personal' | 'enterprise'
  real_name: string
  id_number_masked?: string
  license_no?: string
  license_file?: string
  result: KycResult
  reason: string
  reviewer_id: number | null
  created_at: string
  updated_at: string
}

export interface UserDetail {
  user: User
  balance: number
  instances: AdminInstance[]
  orders: AdminOrder[]
  ledger: LedgerRow[]
  kyc: AdminKYCRecord[]
}

export interface AdminPrice {
  duration: Duration
  amount: number
  enabled: boolean
  updated_at: string
}

export interface AdminProduct {
  id: number
  code: string
  name: string
  spec: Record<string, unknown>
  status: 'active' | 'inactive'
  prices: AdminPrice[]
}

export interface AdminImage {
  id: number
  name: string
  ref: string
  description: string
  enabled: boolean
  sort: number
  created_at: string
  updated_at: string
}

export interface ImageInput {
  name: string
  ref: string
  description: string
  enabled: boolean
  sort: number
}

export interface AuditEntry {
  id: number
  actor_type: 'admin' | 'user' | 'system'
  actor_id: number
  actor_name: string
  action: string
  target_type: string
  target_id: string
  diff: unknown
  ip: string
  created_at: string
}

export interface Dashboard {
  machines: { by_state: Partial<Record<MachineState, number>>; online: number; total: number }
  instances: Partial<Record<InstanceState, number>>
  recharge_today: number
  recharge_month: number
  order_today: number
  order_month: number
  utilization: { date: string; rate: number }[]
}
```

`web/shared/src/types/index.ts`：

```ts
export * from './user'
export * from './catalog'
export * from './instance'
export * from './wallet'
export * from './recharge'
export * from './kyc'
export * from './admin'
```

`web/shared/src/index.ts` 追加：

```ts
export * from './http'
export * from './types'
```

- [ ] **Step 4: 运行测试确认通过**

Run: `cd web && npm test && npm run typecheck -w shared`
Expected: `http.spec.ts` 全部 PASS（buildQuery 1、基本行为 7、动态码 5、download 3）；`tsc` 无错误。

- [ ] **Step 5: 提交**

```bash
git add web/shared
git commit -m "feat(shared): fetch wrapper with error mapping, 401 handling, TOTP re-send and CSV download; API types

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---
### Task 7: Playwright 环境：配置、helpers、fixtures、覆盖表核对脚本

**Files:**
- Create: `web/playwright.config.ts`、`web/e2e/tsconfig.json`、`web/e2e/vitest.config.ts`
- Create: `web/e2e/helpers/{hooks,phone,money,totp,console,admin}.ts`
- Create: `web/e2e/coverage.json`、`web/e2e/coverage.mjs`
- Create: `web/e2e/fixtures/{license.png,license.pdf,bad.gif,machines-ok.csv,machines-bad.csv}`
- Create: `web/e2e/__tests__/{totp,coverage}.spec.ts`
- Modify: `web/vitest.config.ts`（加入 `e2e/vitest.config.ts`）

**Interfaces:**
- Consumes: Task 4 的钩子；Task 5 的 `fmtYuan` 规则（`money.ts` 独立实现同一格式，避免 Playwright 进程加载 vue）。
- Produces（所有 spec 使用）:
  - 常量：`CONSOLE_URL = 'http://127.0.0.1:18080'`、`ADMIN_URL = 'http://127.0.0.1:18081'`、`HOOKS_URL = 'http://127.0.0.1:18090'`。
  - `hooks`（`helpers/hooks.ts`）：`clock(): Promise<Date>`、`advance(seconds): Promise<Date>`、`sweep({ heartbeat? })`、`agent({ machine_id?, fail?, result? }): Promise<{ completed }>`、`machines(count = 1, serial_prefix?): Promise<number[]>`、`users({ phone?, verified?, balance?, ledger_rows? }): Promise<{ id; phone }>`、`admins({ username, role, bind_totp })`、`kyc(reject: boolean)`。
  - `uniquePhone()`（`helpers/phone.ts`）、`nextUsername()`、`nextSerial()`（`helpers/admin.ts`）、`yuan(cents): string`（`helpers/money.ts`）。
  - `totp(secretBase32, unixSeconds): string`、`base32Decode`、`hotp`（`helpers/totp.ts`）。
  - 控制台 helpers（`helpers/console.ts`）：`isMobile(info: TestInfo)`、`seedUser({ verified = true, balance = 100000, ledger_rows? })`、`loginConsole(page, phone, expectPath = '/market')`（先 `hooks.advance(61)` 再发码、填 `000000`）、`nav(page, info, 'market' | 'instances' | 'wallet' | 'account')`、`userApi(phone): Promise<APIRequestContext>`（API 登录到 `:18080`）、`placeOrder(api, { duration?, imageIndex? }): Promise<string>`（返回实例 ID）、`toast(page, text)`（`getByRole('alert')` 含文案）、`dialog(page)`（`getByRole('dialog')`）。
  - 后台 helpers（`helpers/admin.ts`）：`createAdmin(role, bindTotp = true): Promise<SeededAdmin>`（`{ id, username, password, role, totp_secret?, recovery_codes? }`）、`totpNow(secret)`（先 `advance(30)`）、`loginAdmin(page, admin)`、`enterTotpDialog(page, secret)`（全局动态码弹框 `totp-dialog` / `totp-input` / `totp-ok`）、`adminApi(admin): Promise<APIRequestContext>`、`adminSend(ctx, admin, 'post' | 'put', path, data)` 及其包装 `adminPost` / `adminPut`（收到 `totp_required` 自动补 `totp` 重发）。
  - 登录页 testid 约定（Task 8 / 15 实现）：控制台 `login-phone`、`login-send`、`login-code`、`login-submit`；后台 `login-username`、`login-password`、`login-submit`、`login-totp-code`、`login-totp-submit`；后台布局 `layout-username`。
  - `npm run e2e:coverage`：读取 `e2e/coverage.json`（`{ console: string[], admin: string[] }`）与 `e2e/console/*.spec.ts`、`e2e/admin/*.spec.ts`，核对每个 ID 恰好出现一次、无未登记 ID、无 `test.skip / fixme / only`、无 `describe.skip / only`、每个 `test(` 标题以 ID 开头；失败非零退出并逐条打印。
  - Playwright project：`console-desktop`（Desktop Chrome 1280×720，`baseURL` 控制台）、`console-mobile`（`devices['iPhone 13']` + chromium/chrome）、`admin`（1440×900，`baseURL` 后台）；`workers: 1`、`fullyParallel: false`、`forbidOnly: true`、`trace: 'retain-on-failure'`、剪贴板权限、`webServer` 自动 `go run ./cmd/e2e-server`、`E2E_BROWSER_CHANNEL`（默认 `chrome`）。

- [ ] **Step 1: Playwright 配置**

`web/playwright.config.ts`：

```ts
import { defineConfig, devices } from '@playwright/test'

const channel = process.env.E2E_BROWSER_CHANNEL ?? 'chrome' // CI 可设为 chromium 使用 Playwright 自带浏览器
const browser = channel === 'chromium' ? {} : { channel }

export const CONSOLE_URL = 'http://127.0.0.1:18080'
export const ADMIN_URL = 'http://127.0.0.1:18081'

export default defineConfig({
  testDir: 'e2e',
  testMatch: /(console|admin)\/.*\.spec\.ts$/,
  outputDir: 'test-results',
  // 假时钟全局共享：串行执行，每个测试自行登录、自行造数据
  workers: 1,
  fullyParallel: false,
  retries: 0,
  forbidOnly: true,
  timeout: 90_000,
  expect: { timeout: 10_000 },
  reporter: [['list'], ['html', { open: 'never', outputFolder: 'playwright-report' }]],
  use: {
    headless: true,
    locale: 'zh-CN',
    timezoneId: 'Asia/Shanghai',
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
    permissions: ['clipboard-read', 'clipboard-write'],
    ...browser,
  },
  webServer: {
    command: 'go run ./cmd/e2e-server',
    cwd: '..',
    url: 'http://127.0.0.1:18090/healthz',
    timeout: 180_000,
    // 永远由 Playwright 自己拉起：e2e-server 里 embed 的是启动那一刻的前端产物，复用手工起的旧进程会悄悄测到上一次的页面；
    // 端口被占用时直接报错，比「看似通过」好
    reuseExistingServer: false,
    stdout: 'pipe',
    stderr: 'pipe',
  },
  projects: [
    {
      name: 'console-desktop',
      testMatch: /console\/.*\.spec\.ts$/,
      use: { ...devices['Desktop Chrome'], ...browser, baseURL: CONSOLE_URL, viewport: { width: 1280, height: 720 } },
    },
    {
      name: 'console-mobile',
      testMatch: /console\/.*\.spec\.ts$/,
      use: { ...devices['iPhone 13'], defaultBrowserType: 'chromium', ...browser, baseURL: CONSOLE_URL },
    },
    {
      name: 'admin',
      testMatch: /admin\/.*\.spec\.ts$/,
      use: { ...devices['Desktop Chrome'], ...browser, baseURL: ADMIN_URL, viewport: { width: 1440, height: 900 } },
    },
  ],
})
```

`web/e2e/tsconfig.json`（给编辑器与 Vitest 用；Playwright 自带转译）：

```json
{ "extends": "../tsconfig.base.json", "compilerOptions": { "types": ["node"] }, "include": ["**/*.ts", "coverage.mjs"] }
```

Run: `cd web && npm install -D @types/node@^22 && npx playwright test --list`
Expected: 打印 `Total: 0 tests in 0 files`（配置可加载、Chrome channel 可用，无报错）。

- [ ] **Step 2: helpers**

`web/e2e/helpers/hooks.ts`：

```ts
import { request, type APIRequestContext } from '@playwright/test'

export const HOOKS_URL = 'http://127.0.0.1:18090'
export const CONSOLE_URL = 'http://127.0.0.1:18080'
export const ADMIN_URL = 'http://127.0.0.1:18081'

let ctx: APIRequestContext | undefined
async function api(): Promise<APIRequestContext> {
  ctx ??= await request.newContext({ baseURL: HOOKS_URL })
  return ctx
}

async function post<T>(path: string, body: unknown): Promise<T> {
  const r = await (await api()).post(path, { data: body })
  if (!r.ok()) throw new Error(`hook ${path} -> ${r.status()} ${await r.text()}`)
  return (await r.json()) as T
}

export type Role = 'super' | 'ops' | 'finance'

export const hooks = {
  async clock(): Promise<Date> {
    const r = await (await api()).get('/hooks/clock')
    return new Date(((await r.json()) as { now: string }).now)
  },
  async advance(seconds: number): Promise<Date> {
    return new Date((await post<{ now: string }>('/hooks/clock/advance', { seconds })).now)
  },
  async sweep(opts: { heartbeat?: boolean } = {}): Promise<void> {
    await post('/hooks/sweep', opts)
  },
  agent(opts: { machine_id?: number; fail?: boolean; result?: string } = {}) {
    return post<{ completed: number }>('/hooks/agent', opts)
  },
  async machines(count = 1, serial_prefix?: string): Promise<number[]> {
    return (await post<{ ids: number[] }>('/hooks/machines', { count, serial_prefix })).ids
  },
  users(opts: { phone?: string; verified?: boolean; balance?: number; ledger_rows?: number } = {}) {
    return post<{ id: number; phone: string }>('/hooks/users', opts)
  },
  admins(opts: { username: string; role: Role; bind_totp: boolean }) {
    return post<{ id: number; password: string; totp_secret?: string; recovery_codes?: string[] }>('/hooks/admins', opts)
  },
  async kyc(reject: boolean): Promise<void> {
    await post('/hooks/kyc', { reject })
  },
}
```

`web/e2e/helpers/phone.ts`：

```ts
// 139 + 启动时间戳后 4 位 + 4 位自增：同一轮测试内唯一，worker 重启后戳变化也不会与之前的号码重复
const stamp = String(Date.now()).slice(-4)
let seq = 0

export function uniquePhone(): string {
  seq += 1
  return `139${stamp}${String(seq).padStart(4, '0')}`
}

export const runStamp = stamp
```

`web/e2e/helpers/money.ts`：

```ts
/** 与 @spark/shared 的 fmtYuan 同一格式（独立实现，避免测试进程加载 vue）。 */
export function yuan(cents: number): string {
  const abs = Math.abs(Math.trunc(cents))
  const whole = String(Math.floor(abs / 100)).replace(/\B(?=(\d{3})+(?!\d))/g, ',')
  return `${cents < 0 ? '-' : ''}¥${whole}.${String(abs % 100).padStart(2, '0')}`
}
```

`web/e2e/helpers/totp.ts`：

```ts
import { createHmac } from 'node:crypto'

const ALPHABET = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567'

export function base32Decode(s: string): Buffer {
  const clean = s.toUpperCase().replace(/\s+/g, '').replace(/=+$/, '')
  let bits = 0
  let value = 0
  const out: number[] = []
  for (const ch of clean) {
    const idx = ALPHABET.indexOf(ch)
    if (idx < 0) throw new Error(`invalid base32 character: ${ch}`)
    value = (value << 5) | idx
    bits += 5
    if (bits >= 8) {
      out.push((value >>> (bits - 8)) & 0xff)
      bits -= 8
    }
  }
  return Buffer.from(out)
}

/** RFC 4226 HOTP（HMAC-SHA1）。 */
export function hotp(key: Buffer, counter: bigint, digits = 6): string {
  const msg = Buffer.alloc(8)
  msg.writeBigUInt64BE(counter)
  const h = createHmac('sha1', key).update(msg).digest()
  const off = h[h.length - 1] & 0x0f
  const code = ((h[off] & 0x7f) << 24) | (h[off + 1] << 16) | (h[off + 2] << 8) | h[off + 3]
  return String(code % 10 ** digits).padStart(digits, '0')
}

/** RFC 6238 TOTP：30 秒一步、6 位；时间取服务器假时钟（GET /hooks/clock）。 */
export function totp(secretBase32: string, unixSeconds: number, period = 30, digits = 6): string {
  return hotp(base32Decode(secretBase32), BigInt(Math.floor(unixSeconds / period)), digits)
}
```

`web/e2e/helpers/console.ts`：

```ts
import { expect, request, type APIRequestContext, type Locator, type Page, type TestInfo } from '@playwright/test'
import { CONSOLE_URL, hooks } from './hooks'
import { uniquePhone } from './phone'

export const isMobile = (info: TestInfo) => info.project.name === 'console-mobile'

export async function seedUser(opts: { verified?: boolean; balance?: number; ledger_rows?: number } = {}) {
  const phone = uniquePhone()
  const u = await hooks.users({ phone, verified: opts.verified ?? true, balance: opts.balance ?? 100000, ledger_rows: opts.ledger_rows })
  return { id: u.id, phone }
}

/**
 * 界面登录。同一手机号 60 秒只能发一次验证码，而假时钟不会自己流逝：
 * 先推进 61 秒，保证本测试里之前（例如 userApi）发过的码不会让这次 rate_limited。
 */
export async function loginConsole(page: Page, phone: string, expectPath = '/market') {
  await hooks.advance(61)
  await page.goto('/login')
  await page.getByTestId('login-phone').fill(phone)
  await page.getByTestId('login-send').click()
  await page.getByTestId('login-code').fill('000000')
  await page.getByTestId('login-submit').click()
  await expect(page).toHaveURL(new RegExp(`${expectPath.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}(\\?|$)`))
}

export type ConsoleTab = 'market' | 'instances' | 'wallet' | 'account'

/** 电脑点侧边菜单，手机点底部标签。 */
export async function nav(page: Page, info: TestInfo, tab: ConsoleTab) {
  await page.getByTestId(isMobile(info) ? `tabbar-${tab}` : `menu-${tab}`).click()
}

/** API 登录（发码 + 登录），用于造数据；之后若要界面登录同一手机号，loginConsole 会先推进 61 秒。 */
export async function userApi(phone: string): Promise<APIRequestContext> {
  const ctx = await request.newContext({ baseURL: CONSOLE_URL })
  await hooks.advance(61)
  const sms = await ctx.post('/api/v1/auth/sms-code', { data: { phone } })
  if (!sms.ok()) throw new Error(`sms-code ${sms.status()} ${await sms.text()}`)
  const login = await ctx.post('/api/v1/auth/login', { data: { phone, code: '000000' } })
  if (!login.ok()) throw new Error(`login ${login.status()} ${await login.text()}`)
  return ctx
}

/** 通过 API 下单（商品固定 spark-1），返回实例 ID；默认 1 天、第一个镜像。 */
export async function placeOrder(api: APIRequestContext, opts: { duration?: '1d' | '1w' | '1m'; imageIndex?: number } = {}): Promise<string> {
  const products = ((await (await api.get('/api/v1/products')).json()) as { items: { id: number }[] }).items
  const images = ((await (await api.get('/api/v1/images')).json()) as { items: { id: number }[] }).items
  const r = await api.post('/api/v1/orders', {
    data: { product_id: products[0].id, duration: opts.duration ?? '1d', image_id: images[opts.imageIndex ?? 0].id },
    headers: { 'Idempotency-Key': crypto.randomUUID() },
  })
  if (r.status() !== 201) throw new Error(`order ${r.status()} ${await r.text()}`)
  return ((await r.json()) as { instance: { id: string } }).instance.id
}

/** ElMessage 渲染为 role="alert"。 */
export async function toast(page: Page, text: string | RegExp) {
  await expect(page.getByRole('alert').filter({ hasText: text }).first()).toBeVisible()
}

export function dialog(page: Page): Locator {
  return page.getByRole('dialog')
}
```

`web/e2e/helpers/admin.ts`：

```ts
import { expect, request, type APIRequestContext, type Page } from '@playwright/test'
import { ADMIN_URL, hooks, type Role } from './hooks'
import { runStamp } from './phone'
import { totp } from './totp'

let adminSeq = 0
let serialSeq = 0

export function nextUsername(): string {
  adminSeq += 1
  return `e2e_${runStamp}_${adminSeq}`
}

export function nextSerial(): string {
  serialSeq += 1
  return `E2E-${runStamp}-${serialSeq}`
}

export interface SeededAdmin {
  id: number
  username: string
  password: string
  role: Role
  totp_secret?: string
  recovery_codes?: string[]
}

export async function createAdmin(role: Role, bindTotp = true): Promise<SeededAdmin> {
  const username = nextUsername()
  const r = await hooks.admins({ username, role, bind_totp: bindTotp })
  return { ...r, username, role }
}

/** 先推进 30 秒（同一时间片的动态码只能用一次），再按服务器时间算码。 */
export async function totpNow(secret: string): Promise<string> {
  const now = await hooks.advance(30)
  return totp(secret, Math.floor(now.getTime() / 1000))
}

export async function loginAdmin(page: Page, admin: SeededAdmin) {
  await page.goto('/admin/login')
  await page.getByTestId('login-username').fill(admin.username)
  await page.getByTestId('login-password').fill(admin.password)
  await page.getByTestId('login-submit').click()
  await expect(page.getByTestId('login-totp-code')).toBeVisible()
  await page.getByTestId('login-totp-code').fill(await totpNow(admin.totp_secret!))
  await page.getByTestId('login-totp-submit').click()
  await expect(page.getByTestId('layout-username')).toHaveText(admin.username)
}

/** 全局动态码弹框（请求层 totp_required 触发）。 */
export async function enterTotpDialog(page: Page, secret: string) {
  await expect(page.getByTestId('totp-dialog')).toBeVisible()
  await page.getByTestId('totp-input').fill(await totpNow(secret))
  await page.getByTestId('totp-ok').click()
}

export async function adminApi(admin: SeededAdmin): Promise<APIRequestContext> {
  const ctx = await request.newContext({ baseURL: ADMIN_URL })
  const login = await ctx.post('/admin/api/v1/auth/login', { data: { username: admin.username, password: admin.password } })
  if (!login.ok()) throw new Error(`admin login ${login.status()} ${await login.text()}`)
  const t = await ctx.post('/admin/api/v1/auth/totp', { data: { code: await totpNow(admin.totp_secret!) } })
  if (!t.ok()) throw new Error(`admin totp ${t.status()} ${await t.text()}`)
  return ctx
}

/** 后台写接口；收到 totp_required 时自动补 totp 重发一次。 */
export async function adminSend<T = Record<string, unknown>>(ctx: APIRequestContext, admin: SeededAdmin, method: 'post' | 'put', path: string, data: Record<string, unknown> = {}): Promise<T> {
  let r = await ctx[method](path, { data })
  if (r.status() === 403 && ((await r.json()) as { code: string }).code === 'totp_required') {
    r = await ctx[method](path, { data: { ...data, totp: await totpNow(admin.totp_secret!) } })
  }
  if (!r.ok()) throw new Error(`${method.toUpperCase()} ${path} ${r.status()} ${await r.text()}`)
  return (await r.json()) as T
}
export const adminPost = <T = Record<string, unknown>>(ctx: APIRequestContext, admin: SeededAdmin, path: string, data: Record<string, unknown> = {}) => adminSend<T>(ctx, admin, 'post', path, data)
export const adminPut = <T = Record<string, unknown>>(ctx: APIRequestContext, admin: SeededAdmin, path: string, data: Record<string, unknown> = {}) => adminSend<T>(ctx, admin, 'put', path, data)
```

- [ ] **Step 3: fixtures**

Run（在 `web/e2e/fixtures/` 下生成二进制文件；PNG / GIF 为 1×1 像素，PDF 为最小合法文档，后端按内容嗅探类型）：

```bash
mkdir -p web/e2e/fixtures && cd web/e2e/fixtures
echo 'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==' | base64 -d > license.png
echo 'R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBRAA7' | base64 -d > bad.gif
printf '%%PDF-1.4\n1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj\n2 0 obj<</Type/Pages/Kids[3 0 R]/Count 1>>endobj\n3 0 obj<</Type/Page/Parent 2 0 R/MediaBox[0 0 200 200]>>endobj\nxref\n0 4\n0000000000 65535 f \ntrailer<</Size 4/Root 1 0 R>>\nstartxref\n0\n%%%%EOF\n' > license.pdf
file license.png bad.gif license.pdf
```

Expected: `license.png: PNG image data, 1 x 1`、`bad.gif: GIF image data, version 89a, 1 x 1`、`license.pdf: PDF document, version 1.4`。

`web/e2e/fixtures/machines-ok.csv`（`{{N}}` 由测试替换为运行戳，避免重复序列号）：

```csv
serial,rack,slot,lan_ip,product_code,pdu_name,pdu_outlet
E2E-CSV-{{N}}-1,B01,01,10.98.0.1,spark-1,,
E2E-CSV-{{N}}-2,B01,02,10.98.0.2,spark-1,,
E2E-CSV-{{N}}-3,B01,03,10.98.0.3,spark-1,,
```

`web/e2e/fixtures/machines-bad.csv`：

```csv
serial,rack,slot,lan_ip,product_code,pdu_name,pdu_outlet
E2E-BAD-{{N}}-1,B02,01,10.98.1,spark-1,,
E2E-BAD-{{N}}-2,B02,02,10.98.1.2,no-such-product,,
E2E-BAD-{{N}}-3,B02,03,10.98.1.3,spark-1,ghost-pdu,1
```

- [ ] **Step 4: 覆盖表与核对脚本**

`web/e2e/coverage.json`（与设计 §7.5 一一对应，改表必须同步改它）：

```json
{
  "console": [
    "C-NAV-01", "C-NAV-02", "C-NAV-03",
    "C-AUTH-01", "C-AUTH-02", "C-AUTH-03", "C-AUTH-04", "C-AUTH-05", "C-AUTH-06", "C-AUTH-07",
    "C-MKT-01", "C-MKT-02", "C-MKT-03", "C-MKT-04", "C-MKT-05", "C-MKT-06", "C-MKT-07", "C-MKT-08",
    "C-INS-01", "C-INS-02", "C-INS-03", "C-INS-04",
    "C-DET-01", "C-DET-02", "C-DET-03", "C-DET-04", "C-DET-05", "C-DET-06", "C-DET-07", "C-DET-08", "C-DET-09",
    "C-WAL-01", "C-WAL-02", "C-WAL-03", "C-WAL-04",
    "C-RCH-01", "C-RCH-02", "C-RCH-03", "C-RCH-04", "C-RCH-05", "C-RCH-06", "C-RCH-07",
    "C-KYC-01", "C-KYC-02", "C-KYC-03", "C-KYC-04", "C-KYC-05", "C-KYC-06",
    "C-ACC-01", "C-ACC-02",
    "C-ERR-01", "C-ERR-02"
  ],
  "admin": [
    "A-AUTH-01", "A-AUTH-02", "A-AUTH-03", "A-AUTH-04", "A-AUTH-05", "A-AUTH-06", "A-AUTH-07", "A-AUTH-08",
    "A-NAV-01", "A-NAV-02",
    "A-DASH-01",
    "A-MCH-01", "A-MCH-02", "A-MCH-03", "A-MCH-04", "A-MCH-05", "A-MCH-06",
    "A-PDU-01", "A-PDU-02",
    "A-ALT-01",
    "A-INS-01", "A-INS-02", "A-INS-03", "A-INS-04", "A-INS-05", "A-INS-06", "A-INS-07", "A-INS-08", "A-INS-09",
    "A-USR-01", "A-USR-02", "A-USR-03", "A-USR-04", "A-USR-05", "A-USR-06",
    "A-KYC-01", "A-KYC-02",
    "A-FIN-01", "A-FIN-02", "A-FIN-03", "A-FIN-04",
    "A-CAT-01", "A-CAT-02", "A-CAT-03",
    "A-ADM-01", "A-ADM-02", "A-ADM-03",
    "A-SET-01",
    "A-AUD-01",
    "A-ERR-01"
  ]
}
```

`web/e2e/coverage.mjs`：

```js
#!/usr/bin/env node
// 核对覆盖表：coverage.json 里的每个 ID 在 e2e/console、e2e/admin 的 spec 中恰好出现一次；
// 没有未登记的 ID、没有不带 ID 的用例、没有 test.skip / fixme / only、describe.skip / only。
import { readFileSync, readdirSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

const ID_PREFIX = /^([CA]-[A-Z]+-\d{2})\b/

/** @param {{file: string, text: string}[]} specs @param {string[]} expected @returns {string[]} */
export function check(specs, expected) {
  const errors = []
  const seen = new Map()
  for (const { file, text } of specs) {
    for (const m of text.matchAll(/\b(?:test|it)\.(skip|fixme|only)\s*\(/g)) errors.push(`${file}: 禁止 test.${m[1]}`)
    for (const m of text.matchAll(/\bdescribe\.(skip|only)\s*\(/g)) errors.push(`${file}: 禁止 describe.${m[1]}`)
    for (const m of text.matchAll(/\btest\s*\(\s*(['"`])([^'"`\n]*)\1/g)) {
      const title = m[2]
      const id = ID_PREFIX.exec(title)?.[1]
      if (!id) {
        errors.push(`${file}: 用例标题未以覆盖表 ID 开头：「${title}」`)
        continue
      }
      if (!expected.includes(id)) errors.push(`${file}: 未登记的 ID ${id}`)
      seen.set(id, (seen.get(id) ?? 0) + 1)
    }
  }
  for (const id of expected) if (!seen.has(id)) errors.push(`缺少用例：${id}`)
  for (const [id, n] of seen) if (n > 1) errors.push(`ID ${id} 出现了 ${n} 次（每行覆盖表只对应一个用例）`)
  return errors
}

function main() {
  const root = dirname(fileURLToPath(import.meta.url))
  const table = JSON.parse(readFileSync(join(root, 'coverage.json'), 'utf8'))
  const expected = [...table.console, ...table.admin]
  if (new Set(expected).size !== expected.length) {
    console.error('coverage.json 含重复 ID')
    process.exit(1)
  }
  const specs = []
  for (const dir of ['console', 'admin']) {
    let names = []
    try {
      names = readdirSync(join(root, dir)).filter((n) => n.endsWith('.spec.ts'))
    } catch {
      /* 目录尚未创建 */
    }
    for (const n of names) specs.push({ file: `${dir}/${n}`, text: readFileSync(join(root, dir, n), 'utf8') })
  }
  const errors = check(specs, expected)
  if (errors.length) {
    for (const e of errors) console.error('  ' + e)
    console.error(`覆盖表核对失败：${errors.length} 个问题`)
    process.exit(1)
  }
  console.log(`覆盖表核对通过：${expected.length} 个用例 ID（控制台 ${table.console.length}，后台 ${table.admin.length}）`)
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) main()
```

- [ ] **Step 5: helper 单测（U-13 与核对脚本）**

`web/e2e/vitest.config.ts`：

```ts
import { defineConfig } from 'vitest/config'

export default defineConfig({
  test: { name: 'e2e-helpers', environment: 'node', include: ['__tests__/*.spec.ts'] },
})
```

`web/vitest.config.ts` 的 `projects` 改为 `['shared/vitest.config.ts', 'e2e/vitest.config.ts']`。

`web/e2e/__tests__/totp.spec.ts`（RFC 6238 附录 B 向量，密钥 ASCII `12345678901234567890`，SHA1，8 位）：

```ts
import { describe, expect, it } from 'vitest'
import { base32Decode, hotp, totp } from '../helpers/totp'

const SECRET_B32 = 'GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ' // "12345678901234567890"

describe('totp helper (U-13)', () => {
  it('base32 解码', () => {
    expect(base32Decode(SECRET_B32).toString('ascii')).toBe('12345678901234567890')
    expect(base32Decode('gezd gnbv').toString('ascii')).toBe('12345')
  })
  it.each([
    [59, '94287082'],
    [1111111109, '07081804'],
    [1111111111, '14050471'],
    [1234567890, '89005924'],
    [2000000000, '69279037'],
    [20000000000, '65353130'],
  ])('T=%d -> %s', (t, want) => {
    expect(totp(SECRET_B32, t, 30, 8)).toBe(want)
  })
  it('默认 6 位、30 秒', () => {
    expect(totp(SECRET_B32, 59)).toBe('287082')
    expect(hotp(base32Decode(SECRET_B32), 1n)).toBe('287082')
  })
})
```

`web/e2e/__tests__/coverage.spec.ts`：

```ts
import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { describe, expect, it } from 'vitest'
import { check } from '../coverage.mjs'

const table = JSON.parse(readFileSync(join(__dirname, '..', 'coverage.json'), 'utf8')) as { console: string[]; admin: string[] }

describe('coverage.json', () => {
  it('与设计 §7.5 的数量一致且无重复', () => {
    expect(table.console).toHaveLength(52)
    expect(table.admin).toHaveLength(50)
    expect(new Set([...table.console, ...table.admin]).size).toBe(102)
  })
})

describe('check()', () => {
  const ok = { file: 'console/a.spec.ts', text: "test('C-NAV-01 响应式布局', async () => {})\ntest('C-NAV-02 弹窗', async () => {})" }
  it('全部出现且无违规', () => {
    expect(check([ok], ['C-NAV-01', 'C-NAV-02'])).toEqual([])
  })
  it('缺少、未登记、重复', () => {
    const errors = check([ok, { file: 'console/b.spec.ts', text: "test('C-NAV-02 again', () => {})\ntest('C-XXX-99 x', () => {})" }], ['C-NAV-01', 'C-NAV-02', 'C-NAV-03'])
    expect(errors).toContain('缺少用例：C-NAV-03')
    expect(errors).toContain('console/b.spec.ts: 未登记的 ID C-XXX-99')
    expect(errors.some((e) => e.includes('C-NAV-02 出现了 2 次'))).toBe(true)
  })
  it('skip / fixme / only 与无 ID 标题', () => {
    const errors = check([{ file: 'admin/x.spec.ts', text: "test.skip('A-AUTH-01 a', () => {})\ntest.only('A-AUTH-02 b', () => {})\ntest.describe.only('g', () => {})\ntest('no id here', () => {})" }], ['A-AUTH-01', 'A-AUTH-02'])
    expect(errors).toContain('admin/x.spec.ts: 禁止 test.skip')
    expect(errors).toContain('admin/x.spec.ts: 禁止 test.only')
    expect(errors).toContain('admin/x.spec.ts: 禁止 describe.only')
    expect(errors.some((e) => e.includes('未以覆盖表 ID 开头'))).toBe(true)
  })
})
```

Run: `cd web && npm test && npm run e2e:coverage; echo "exit=$?"`
Expected: Vitest 的 `e2e-helpers` 项目 PASS（totp 9、coverage 4）；`e2e:coverage` 此时打印 102 条「缺少用例」并 `exit=1`（spec 尚未编写，这是预期的失败，Task 22 收尾时必须变为通过）。

- [ ] **Step 6: 用钩子跑一次 webServer 链路**

Run: `cd web && npx playwright test --project=admin --list`
Expected: `Total: 0 tests`。再 `cd web && npx playwright test --project=admin` 
Expected: Playwright 启动 `go run ./cmd/e2e-server`（stdout 出现 `E2E READY`），因无测试而报 `Error: No tests found` 退出；进程被 Playwright 终止，无残留（`lsof -i :18090` 无输出）。

- [ ] **Step 7: 提交**

```bash
git add web/playwright.config.ts web/e2e web/vitest.config.ts web/package.json web/package-lock.json
git commit -m "test(e2e): playwright projects, hooks/totp/console/admin helpers, fixtures, coverage table check

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---
### Task 8: 控制台骨架：入口、路由与守卫、会话、布局、AppDialog、登录、我的、帮助、404

**Files:**
- Create: `web/console/{package.json,index.html,vite.config.ts,tsconfig.json,vitest.config.ts}`、`web/console/public/brand/logo.svg`
- Create: `web/console/src/{main.ts,App.vue,router.ts,api.ts,styles.css,env.d.ts}`、`web/console/src/store/session.ts`、`web/console/src/layouts/ConsoleLayout.vue`、`web/console/src/components/AppDialog.vue`、`web/console/src/logic/login.ts`
- Create: `web/console/src/views/{LoginView,AccountView,HelpView,NotFoundView}.vue`
- Create（页面骨架，只有标题，后续任务整体替换）: `web/console/src/views/{MarketView,InstancesView,InstanceDetailView,WalletView,RechargeView,RechargeOrderView,KycView}.vue`
- Create: `web/console/src/logic/__tests__/login.spec.ts`、`web/console/src/store/__tests__/session.spec.ts`、`web/console/src/__tests__/router.spec.ts`
- Create: `web/e2e/console/{nav,auth,account}.spec.ts`
- Modify: `web/package.json`（`workspaces` 加 `console`）、`web/vitest.config.ts`（加 `console/vitest.config.ts`）

**Interfaces:**
- Consumes: Task 5、6 的 `@spark/shared`；Task 7 的 helpers 与 testid 约定。
- Produces（后续控制台任务依赖）:
  - `api`（`src/api.ts`）：`createHttp({ base: '/api/v1' })` 的实例；`onUnauthorized(fn)` 注册 401 处理。
  - `src/store/session.ts`：`session: { status: 'unknown' | 'anon' | 'authed'; user: User | null; balance: number | null }`、`probeSession()`（只请求一次 `GET /me`，`quiet`）、`setUser(u)`、`clearSession()`、`refreshBalance()`（`GET /wallet` → `session.balance`）、`handleUnauthorized(message = '请先登录')`（清会话、提示一次、`router.replace('/login?redirect=<当前>')`，并发只执行一次）、`bindRouter(router)`。
  - `router`（`src/router.ts`）：路由表（设计 §4.1，`meta: { title, auth? }`）、`safeRedirect(v): string | null`；`beforeEach` 先 `probeSession()`；`afterEach` 设 `document.title = '<页面名> - Spark Cloud'`。
  - `AppDialog`（`src/components/AppDialog.vue`）：`v-model` 显隐，props `kind: 'confirm' | 'form'`（默认 `confirm`）、`title`、`width?`；手机：confirm 加类 `sheet`（底部弹出，不全屏），form `fullscreen`；电脑：居中，宽 420 / 560；插槽 `default`、`footer`。
  - `src/logic/login.ts`：`normalizePhone(s)`（去空白 / 连字符、全角转半角）、`normalizeCode(s)`（只留数字、最多 6 位）、`isValidPhone(s)`、`maskPhone(p)`（`138****1234`）。
  - `src/logic/notify.ts`：`notifyError(e: unknown)`——页面里「请求失败就弹错误」的统一出口：`unauthorized`（已由 `handleUnauthorized` 提示并跳转）与 `cancelled` 静默，其余 `ElMessage.error(messageOf(e))`。控制台所有视图的 catch 分支一律用它，不直接写 `ElMessage.error(messageOf(e))`；否则会话失效时同一页面的多个请求会叠出多条「请先登录」（C-AUTH-06 断言只有一条）。
  - 布局 testid：`layout-side`、`menu-market|instances|wallet|account`、`layout-tabbar`、`tabbar-market|instances|wallet|account`、`layout-phone`（中间四位打码）、`layout-help`（电脑）、`layout-login`（电脑未登录）。
  - 登录页 testid：`login-phone`、`login-send`、`login-code`、`login-submit`；我的页：`account-phone`、`account-kyc`（链接到 `/kyc`）、`account-created`、`account-help`、`account-logout`；帮助页：`help-page`、`help-ssh`、`help-retention`、`help-billing`；404：`notfound-page`、`notfound-home`。
  - 覆盖：C-NAV-01、C-NAV-03、C-AUTH-01～05、C-ACC-01、C-ACC-02。

- [ ] **Step 1: 工作区与构建配置**

`web/package.json` 的 `workspaces` 改为 `["shared", "console"]`；`web/vitest.config.ts` 的 `projects` 加 `'console/vitest.config.ts'`。

`web/console/package.json`：

```json
{
  "name": "console",
  "private": true,
  "type": "module",
  "scripts": {
    "dev": "vite --port 5173",
    "build": "vue-tsc --noEmit && vite build",
    "test": "vitest run"
  },
  "dependencies": {
    "@spark/shared": "*",
    "element-plus": "2.14.7",
    "qrcode": "1.5.4",
    "vue": "3.5.44",
    "vue-router": "5.4.0"
  },
  "devDependencies": {
    "@types/qrcode": "1.5.5",
    "@vitejs/plugin-vue": "6.0.10",
    "unplugin-auto-import": "21.1.0",
    "unplugin-vue-components": "32.1.0",
    "vite": "8.3.4",
    "vue-tsc": "3.3.13"
  }
}
```

`web/console/vite.config.ts`：

```ts
import { fileURLToPath, URL } from 'node:url'
import vue from '@vitejs/plugin-vue'
import AutoImport from 'unplugin-auto-import/vite'
import Components from 'unplugin-vue-components/vite'
import { ElementPlusResolver } from 'unplugin-vue-components/resolvers'
import { defineConfig } from 'vite'

// 开发时预列用到的 Element Plus 样式，避免首次打开多刷新一次
const epComponents = [
  'button', 'input', 'input-number', 'form', 'form-item', 'dialog', 'message', 'table', 'table-column', 'tag', 'card', 'select', 'option',
  'radio-group', 'radio-button', 'tabs', 'tab-pane', 'pagination', 'upload', 'alert', 'skeleton', 'empty', 'descriptions', 'descriptions-item',
  'divider', 'link', 'result', 'checkbox', 'icon',
]

export default defineConfig({
  base: '/',
  plugins: [
    vue(),
    // 生成的 d.ts 入库，vue-tsc --noEmit 在 vite build 之前运行时也能识别自动引入的组件与 ElMessage
    AutoImport({ resolvers: [ElementPlusResolver()], dts: 'src/auto-imports.d.ts' }),
    Components({ resolvers: [ElementPlusResolver()], dts: 'src/components.d.ts' }),
  ],
  resolve: {
    alias: {
      '@spark/shared': fileURLToPath(new URL('../shared/src/index.ts', import.meta.url)),
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  server: { port: 5173, proxy: { '/api': 'http://127.0.0.1:8080' } },
  optimizeDeps: { include: ['element-plus/es', ...epComponents.map((c) => `element-plus/es/components/${c}/style/css`)] },
  build: { outDir: fileURLToPath(new URL('../../internal/web/dist/console', import.meta.url)), emptyOutDir: true },
})
```

`web/console/tsconfig.json`：

```json
{
  "extends": "../tsconfig.base.json",
  "compilerOptions": {
    "types": ["vite/client"],
    "baseUrl": ".",
    "paths": { "@spark/shared": ["../shared/src/index.ts"], "@/*": ["./src/*"] }
  },
  "include": ["src/**/*.ts", "src/**/*.d.ts", "src/**/*.vue", "../shared/src/**/*.ts"]
}
```

`web/console/vitest.config.ts`：

```ts
import { fileURLToPath, URL } from 'node:url'
import vue from '@vitejs/plugin-vue'
import { defineConfig } from 'vitest/config'

export default defineConfig({
  plugins: [vue()],
  resolve: {
    alias: {
      '@spark/shared': fileURLToPath(new URL('../shared/src/index.ts', import.meta.url)),
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  test: { name: 'console', environment: 'jsdom', include: ['src/**/*.spec.ts'] },
})
```

`web/console/index.html`（没有任何内联脚本，满足 CSP）：

```html
<!doctype html>
<html lang="zh-CN">
  <head>
    <meta charset="UTF-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover" />
    <link rel="icon" href="/brand/logo.svg" />
    <title>Spark Cloud</title>
  </head>
  <body>
    <div id="app"></div>
    <script type="module" src="/src/main.ts"></script>
  </body>
</html>
```

`web/console/public/brand/logo.svg`：

```svg
<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32" width="32" height="32"><rect width="32" height="32" rx="8" fill="#2563eb"/><path d="M18 5 8 18h7l-1 9 10-13h-7z" fill="#fff"/></svg>
```

`web/console/src/env.d.ts`：

```ts
/// <reference types="vite/client" />
declare module '*.vue' {
  import type { DefineComponent } from 'vue'
  const component: DefineComponent<Record<string, never>, Record<string, never>, unknown>
  export default component
}
```

Run: `cd web && npm install && npx vite build -c console/vite.config.ts 2>&1 | tail -3`
Expected: 安装成功；此时 `src/main.ts` 还不存在，构建报错属预期；但 `src/auto-imports.d.ts`、`src/components.d.ts` 已生成（插件在配置阶段写出）。若未生成，等 Step 3 写完源码后 `npm run build -w console` 会生成，再一起提交。

- [ ] **Step 2: 写失败的单测（Review Focus 1、2 与 safeRedirect）**

`web/console/src/logic/__tests__/login.spec.ts`：

```ts
import { describe, expect, it } from 'vitest'
import { isValidPhone, maskPhone, normalizeCode, normalizePhone } from '../login'

describe('login logic (Review Focus 1)', () => {
  it('手机号去空白、连字符、全角', () => {
    expect(normalizePhone('138 0000 1234')).toBe('13800001234')
    expect(normalizePhone('138-0000-1234')).toBe('13800001234')
    expect(normalizePhone('１３８００００１２３４')).toBe('13800001234')
    expect(isValidPhone(' 139 0000 0000 ')).toBe(true)
    expect(isValidPhone('12800000000')).toBe(false)
    expect(isValidPhone('1380000123')).toBe(false)
  })
  it('验证码只留数字、最多 6 位', () => {
    expect(normalizeCode('000 000')).toBe('000000')
    expect(normalizeCode('０００００0')).toBe('000000')
    expect(normalizeCode('12-34-56-78')).toBe('123456')
    expect(normalizeCode('abc')).toBe('')
  })
  it('打码', () => {
    expect(maskPhone('13800001234')).toBe('138****1234')
    expect(maskPhone('')).toBe('')
  })
})
```

`web/console/src/store/__tests__/session.spec.ts`：

```ts
import { ref } from 'vue'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const { warning } = vi.hoisted(() => ({ warning: vi.fn() })) // vi.mock 会被提升到文件顶部，工厂里引用的变量必须用 vi.hoisted 声明
vi.mock('element-plus', () => ({ ElMessage: { warning, error: vi.fn(), success: vi.fn() } }))
vi.mock('../../api', () => ({ api: { get: vi.fn() }, onUnauthorized: vi.fn() }))

import { bindRouter, clearSession, handleUnauthorized, session, setUser } from '../session'

function fakeRouter(path: string, fullPath = path, auth = true) {
  let after: () => void = () => {}
  const router = {
    currentRoute: ref({ path, fullPath, meta: { auth } }),
    replace: vi.fn(async () => {}),
    afterEach: (fn: () => void) => {
      after = fn
    },
  }
  return { router, fireAfterEach: () => after() }
}

describe('handleUnauthorized (Review Focus 2)', () => {
  beforeEach(() => {
    warning.mockClear()
    setUser({ id: 1, phone: '13800001234', kind: 'personal', kyc_status: 'verified', status: 'active', created_at: '2026-01-01T00:00:00Z' })
  })

  it('连续三次 401 只提示一次、只跳转一次，并带 redirect', () => {
    const { router, fireAfterEach } = fakeRouter('/wallet', '/wallet?tab=orders')
    bindRouter(router as never)
    handleUnauthorized()
    handleUnauthorized()
    handleUnauthorized()
    expect(session.status).toBe('anon')
    expect(session.user).toBeNull()
    expect(warning).toHaveBeenCalledTimes(1)
    expect(warning).toHaveBeenCalledWith('请先登录')
    expect(router.replace).toHaveBeenCalledTimes(1)
    expect(router.replace).toHaveBeenCalledWith({ path: '/login', query: { redirect: '/wallet?tab=orders' } })
    fireAfterEach()
    router.currentRoute.value = { path: '/login', fullPath: '/login', meta: { auth: false } }
    handleUnauthorized()
    expect(router.replace).toHaveBeenCalledTimes(1)
  })

  it('匿名访客在公开页收到 401 不提示不跳转', () => {
    clearSession()
    const { router } = fakeRouter('/market', '/market', false)
    bindRouter(router as never)
    handleUnauthorized()
    expect(warning).not.toHaveBeenCalled()
    expect(router.replace).not.toHaveBeenCalled()
  })
})
```

`web/console/src/__tests__/router.spec.ts`：

```ts
import { describe, expect, it } from 'vitest'
import { safeRedirect } from '../router'

describe('safeRedirect', () => {
  it('只接受站内相对路径', () => {
    expect(safeRedirect('/instances')).toBe('/instances')
    expect(safeRedirect('/kyc?redirect=/market')).toBe('/kyc?redirect=/market')
    expect(safeRedirect('//evil.com')).toBeNull()
    expect(safeRedirect('https://evil.com')).toBeNull()
    expect(safeRedirect(['/a'])).toBeNull()
    expect(safeRedirect(undefined)).toBeNull()
  })
})
```

Run: `cd web && npm test`
Expected: console 项目 FAIL（模块不存在）。

- [ ] **Step 3: 入口、会话、路由、布局**

`web/console/src/api.ts`：

```ts
import { createHttp } from '@spark/shared'

const handlers = { unauthorized: () => {} }

/** 控制台 API 客户端。401 的处理由 store/session 注册，避免循环引用。 */
export const api = createHttp({ base: '/api/v1', onUnauthorized: () => handlers.unauthorized() })

export function onUnauthorized(fn: () => void) {
  handlers.unauthorized = fn
}
```

`web/console/src/store/session.ts`：

```ts
import { ElMessage } from 'element-plus'
import { reactive } from 'vue'
import type { Router } from 'vue-router'
import type { User } from '@spark/shared'
import { api, onUnauthorized } from '../api'

export type SessionStatus = 'unknown' | 'anon' | 'authed'

export const session = reactive<{ status: SessionStatus; user: User | null; balance: number | null }>({
  status: 'unknown',
  user: null,
  balance: null,
})

let probing: Promise<void> | null = null

/** 应用启动只探测一次 GET /me（quiet：匿名访客的 401 不提示、不跳转）。 */
export function probeSession(): Promise<void> {
  if (session.status !== 'unknown') return Promise.resolve()
  probing ??= api
    .get<User>('/me', undefined, { quiet: true })
    .then(
      (u) => setUser(u),
      () => {
        session.status = 'anon'
        session.user = null
      },
    )
    .finally(() => {
      probing = null
    })
  return probing
}

export function setUser(u: User) {
  session.user = u
  session.status = 'authed'
}

export function clearSession() {
  session.user = null
  session.balance = null
  session.status = 'anon'
}

export async function refreshBalance() {
  if (session.status !== 'authed') return
  const { balance } = await api.get<{ balance: number }>('/wallet')
  session.balance = balance
}

let router: Router | null = null
let redirecting = false

export function bindRouter(r: Router) {
  router = r
  r.afterEach(() => {
    redirecting = false
  })
}

/**
 * 401：清会话、提示一次、跳登录页并带 redirect。
 * 并发多个请求同时 401（钱包页三个请求）只提示一次、只跳一次；匿名访客在公开页不提示不跳转。
 */
export function handleUnauthorized(message = '请先登录') {
  const wasAuthed = session.status === 'authed'
  clearSession()
  if (!router || redirecting) return
  const current = router.currentRoute.value
  if (current.path === '/login') return
  if (!wasAuthed && !current.meta.auth) return
  redirecting = true
  ElMessage.warning(message)
  void router.replace({ path: '/login', query: { redirect: current.fullPath } })
}

onUnauthorized(() => handleUnauthorized())
```

`web/console/src/router.ts`：

```ts
import { brand } from '@spark/shared'
import { createRouter, createWebHistory } from 'vue-router'
import { bindRouter, probeSession, session } from './store/session'

declare module 'vue-router' {
  interface RouteMeta {
    title: string
    auth?: boolean
  }
}

/** redirect 参数只接受站内相对路径（以 / 开头且不以 // 开头）。 */
export function safeRedirect(v: unknown): string | null {
  return typeof v === 'string' && v.startsWith('/') && !v.startsWith('//') ? v : null
}

export const router = createRouter({
  history: createWebHistory('/'),
  routes: [
    { path: '/', redirect: '/market' },
    { path: '/login', component: () => import('./views/LoginView.vue'), meta: { title: '登录' } },
    { path: '/market', component: () => import('./views/MarketView.vue'), meta: { title: '算力市场' } },
    { path: '/instances', component: () => import('./views/InstancesView.vue'), meta: { title: '我的实例', auth: true } },
    { path: '/instances/:id', component: () => import('./views/InstanceDetailView.vue'), meta: { title: '实例详情', auth: true } },
    { path: '/wallet', component: () => import('./views/WalletView.vue'), meta: { title: '钱包', auth: true } },
    { path: '/wallet/recharge', component: () => import('./views/RechargeView.vue'), meta: { title: '充值', auth: true } },
    { path: '/wallet/recharge/:id', component: () => import('./views/RechargeOrderView.vue'), meta: { title: '充值单', auth: true } },
    { path: '/kyc', component: () => import('./views/KycView.vue'), meta: { title: '实名认证', auth: true } },
    { path: '/account', component: () => import('./views/AccountView.vue'), meta: { title: '我的', auth: true } },
    { path: '/help', component: () => import('./views/HelpView.vue'), meta: { title: '使用帮助' } },
    { path: '/:pathMatch(.*)*', component: () => import('./views/NotFoundView.vue'), meta: { title: '页面不存在' } },
  ],
})

router.beforeEach(async (to) => {
  await probeSession()
  if (to.meta.auth && session.status !== 'authed') return { path: '/login', query: { redirect: to.fullPath } }
  if (to.path === '/login' && session.status === 'authed') return safeRedirect(to.query.redirect) ?? '/market'
  return true
})

router.afterEach((to) => {
  document.title = `${to.meta.title} - ${brand.name}`
})

bindRouter(router)
```

`web/console/src/main.ts`：

```ts
import { brand } from '@spark/shared'
import { createApp } from 'vue'
import App from './App.vue'
import { router } from './router'
import './styles.css'

document.documentElement.style.setProperty('--el-color-primary', brand.primaryColor)
createApp(App).use(router).mount('#app')
```

`web/console/src/styles.css`：

```css
:root {
  --sc-tabbar-height: 56px;
}
body {
  margin: 0;
  font-family: system-ui, -apple-system, 'PingFang SC', 'Microsoft YaHei', sans-serif;
  background: #f5f7fa;
  color: #303133;
}
a {
  color: var(--el-color-primary);
}
.money-plus {
  color: var(--el-color-success);
}
.money-minus {
  color: #303133;
}
@media (max-width: 767px) {
  /* 输入框 16px 避免 iOS 聚焦放大；可点元素 ≥ 44px */
  .el-input__inner,
  .el-textarea__inner {
    font-size: 16px;
  }
  .el-button {
    min-height: 44px;
  }
  /* 一句话确认框：底部弹出 */
  .el-dialog.sheet {
    position: fixed;
    left: 0;
    right: 0;
    bottom: 0;
    margin: 0 !important;
    width: 100% !important;
    max-width: 100%;
    border-radius: 16px 16px 0 0;
  }
  .el-dialog.sheet .el-dialog__body {
    padding-bottom: calc(16px + env(safe-area-inset-bottom));
  }
}
```

`web/console/src/App.vue`：

```vue
<script setup lang="ts">
import { useRoute } from 'vue-router'
import ConsoleLayout from './layouts/ConsoleLayout.vue'

const route = useRoute()
</script>

<template>
  <router-view v-if="route.path === '/login'" />
  <ConsoleLayout v-else>
    <router-view />
  </ConsoleLayout>
</template>
```

`web/console/src/layouts/ConsoleLayout.vue`：

```vue
<script setup lang="ts">
import { brand, useMobile } from '@spark/shared'
import { computed } from 'vue'
import { useRoute } from 'vue-router'
import { maskPhone } from '../logic/login'
import { session } from '../store/session'

const mobile = useMobile()
const route = useRoute()
const tabs = [
  { key: 'market', path: '/market', label: '算力市场' },
  { key: 'instances', path: '/instances', label: '我的实例' },
  { key: 'wallet', path: '/wallet', label: '钱包' },
  { key: 'account', path: '/account', label: '我的' },
] as const
const active = computed(() => tabs.find((t) => route.path === t.path || route.path.startsWith(t.path + '/'))?.key ?? '')
const logo = import.meta.env.BASE_URL + brand.logoUrl
</script>

<template>
  <div class="shell" :class="{ mobile }" data-testid="layout">
    <aside v-if="!mobile" class="side" data-testid="layout-side">
      <div class="logo"><img :src="logo" alt="" width="28" height="28" /><span>{{ brand.name }}</span></div>
      <nav>
        <router-link v-for="t in tabs" :key="t.key" :to="t.path" class="menu-item" :class="{ active: active === t.key }" :data-testid="`menu-${t.key}`">
          {{ t.label }}
        </router-link>
      </nav>
    </aside>
    <div class="main">
      <header class="topbar" data-testid="layout-topbar">
        <span class="title">{{ route.meta.title }}</span>
        <span class="right">
          <router-link v-if="!mobile" to="/help" data-testid="layout-help">使用帮助</router-link>
          <span v-if="session.user" data-testid="layout-phone">{{ maskPhone(session.user.phone) }}</span>
          <router-link v-else-if="!mobile" to="/login" data-testid="layout-login">登录</router-link>
        </span>
      </header>
      <main class="content"><slot /></main>
    </div>
    <nav v-if="mobile" class="tabbar" data-testid="layout-tabbar">
      <router-link v-for="t in tabs" :key="t.key" :to="t.path" :class="{ active: active === t.key }" :data-testid="`tabbar-${t.key}`">
        {{ t.label }}
      </router-link>
    </nav>
  </div>
</template>

<style scoped>
.shell { display: flex; min-height: 100vh; }
.side { width: 200px; background: #fff; border-right: 1px solid #ebeef5; flex-shrink: 0; }
.logo { display: flex; align-items: center; gap: 8px; padding: 16px; font-weight: 600; }
.menu-item { display: block; padding: 12px 16px; color: #303133; text-decoration: none; }
.menu-item.active { color: var(--el-color-primary); background: #ecf5ff; }
.main { flex: 1; display: flex; flex-direction: column; min-width: 0; }
.topbar { height: 56px; display: flex; align-items: center; justify-content: space-between; padding: 0 16px; background: #fff; border-bottom: 1px solid #ebeef5; }
.title { font-weight: 600; }
.right { display: flex; gap: 16px; align-items: center; }
.content { flex: 1; padding: 16px; }
.shell.mobile .content { padding: 12px 16px calc(var(--sc-tabbar-height) + 16px + env(safe-area-inset-bottom)); }
.tabbar {
  position: fixed; left: 0; right: 0; bottom: 0; display: flex; background: #fff; border-top: 1px solid #ebeef5;
  height: calc(var(--sc-tabbar-height) + env(safe-area-inset-bottom)); padding-bottom: env(safe-area-inset-bottom); box-sizing: border-box;
}
.tabbar a { flex: 1; display: flex; align-items: center; justify-content: center; min-height: 44px; color: #606266; text-decoration: none; font-size: 13px; }
.tabbar a.active { color: var(--el-color-primary); }
</style>
```

`web/console/src/components/AppDialog.vue`：

```vue
<script setup lang="ts">
import { useMobile } from '@spark/shared'

const props = withDefaults(defineProps<{ kind?: 'confirm' | 'form'; title?: string; width?: number }>(), { kind: 'confirm', title: '', width: undefined })
const visible = defineModel<boolean>({ required: true })
const mobile = useMobile()
</script>

<template>
  <!-- 手机：一句话确认框底部弹出（sheet，不全屏），表单弹窗全屏；电脑：居中 420 / 560 -->
  <el-dialog
    v-model="visible"
    :title="props.title"
    :fullscreen="mobile && props.kind === 'form'"
    :class="{ sheet: mobile && props.kind === 'confirm' }"
    :width="props.width ?? (props.kind === 'confirm' ? 420 : 560)"
    :align-center="!mobile"
    append-to-body
    destroy-on-close
  >
    <slot />
    <template #footer><slot name="footer" /></template>
  </el-dialog>
</template>
```

`web/console/src/logic/login.ts`：

```ts
export const PHONE_RE = /^1[3-9]\d{9}$/

const toHalfWidth = (s: string) => s.replace(/[０-９]/g, (c) => String.fromCharCode(c.charCodeAt(0) - 0xfee0))

/** 去空白与连字符、全角数字转半角（从微信复制的「138 0000 1234」）。 */
export function normalizePhone(s: string): string {
  return toHalfWidth(s).replace(/[\s-]/g, '')
}

/** 只保留数字，最多 6 位（短信里的「000 000」）。 */
export function normalizeCode(s: string): string {
  return toHalfWidth(s).replace(/\D/g, '').slice(0, 6)
}

export function isValidPhone(s: string): boolean {
  return PHONE_RE.test(normalizePhone(s))
}

export function maskPhone(p: string): string {
  return p.length === 11 ? `${p.slice(0, 3)}****${p.slice(7)}` : p
}
```

`web/console/src/logic/notify.ts`：

```ts
import { isApiError, messageOf } from '@spark/shared'
import { ElMessage } from 'element-plus'

/**
 * 页面请求失败的统一提示：unauthorized 已由 handleUnauthorized 提示一次并跳登录，cancelled 是用户主动取消，两者静默；
 * 其余按 messageOf 显示（5xx 带请求号）。控制台视图的 catch 分支一律用它。
 */
export function notifyError(e: unknown): void {
  if (isApiError(e) && (e.code === 'unauthorized' || e.code === 'cancelled')) return
  ElMessage.error(messageOf(e))
}
```

- [ ] **Step 4: 登录、我的、帮助、404 与页面骨架**

`web/console/src/views/LoginView.vue`：

```vue
<script setup lang="ts">
import { brand, isApiError, messageOf, type User } from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, onUnmounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api } from '../api'
import { isValidPhone, normalizeCode, normalizePhone } from '../logic/login'
import { safeRedirect } from '../router'
import { setUser } from '../store/session'

const route = useRoute()
const router = useRouter()
const phone = ref('')
const code = ref('')
const phoneError = ref('')
const codeError = ref('')
const sending = ref(false)
const submitting = ref(false)
const countdown = ref(0) // 只在内存，刷新即重置；服务端仍有 60 秒限制
let timer: ReturnType<typeof setInterval> | undefined
const sendLabel = computed(() => (countdown.value > 0 ? `${countdown.value} 秒后重发` : '发送验证码'))
const logo = import.meta.env.BASE_URL + brand.logoUrl

function startCountdown() {
  countdown.value = 60
  timer = setInterval(() => {
    countdown.value -= 1
    if (countdown.value <= 0) clearInterval(timer)
  }, 1000)
}

async function send() {
  const p = normalizePhone(phone.value)
  if (!isValidPhone(p)) {
    phoneError.value = '手机号格式不正确'
    return
  }
  phoneError.value = ''
  sending.value = true
  try {
    await api.post('/auth/sms-code', { phone: p })
    startCountdown()
  } catch (e) {
    ElMessage.error(messageOf(e)) // rate_limited 等原文提示
  } finally {
    sending.value = false
  }
}

async function submit() {
  const p = normalizePhone(phone.value)
  const c = normalizeCode(code.value)
  phoneError.value = isValidPhone(p) ? '' : '手机号格式不正确'
  codeError.value = c.length === 6 ? '' : '请输入 6 位验证码'
  if (phoneError.value || codeError.value) return
  submitting.value = true
  try {
    const u = await api.post<User>('/auth/login', { phone: p, code: c })
    setUser(u)
    await router.replace(safeRedirect(route.query.redirect) ?? '/market')
  } catch (e) {
    if (isApiError(e) && e.code === 'invalid_code') codeError.value = e.message
    else ElMessage.error(messageOf(e)) // banned → 「账号已被封禁」
  } finally {
    submitting.value = false
  }
}

onUnmounted(() => clearInterval(timer))
</script>

<template>
  <div class="login-page">
    <el-card class="login-card" data-testid="login-card">
      <h1 class="brand"><img :src="logo" alt="" width="32" height="32" /> {{ brand.name }}</h1>
      <el-form label-position="top" @submit.prevent="submit">
        <el-form-item label="手机号" :error="phoneError">
          <el-input v-model="phone" data-testid="login-phone" inputmode="tel" autocomplete="tel" maxlength="13" placeholder="请输入手机号" />
        </el-form-item>
        <el-form-item label="验证码" :error="codeError">
          <div class="code-row">
            <el-input v-model="code" data-testid="login-code" inputmode="numeric" autocomplete="one-time-code" maxlength="6" placeholder="6 位验证码" />
            <el-button data-testid="login-send" :disabled="countdown > 0 || sending" :loading="sending" @click="send">{{ sendLabel }}</el-button>
          </div>
        </el-form-item>
        <el-button type="primary" native-type="submit" class="submit" data-testid="login-submit" :loading="submitting">登录 / 注册</el-button>
      </el-form>
      <p class="hint">未注册的手机号将自动注册；登录即表示同意服务条款</p>
    </el-card>
  </div>
</template>

<style scoped>
.login-page { min-height: 100vh; display: flex; align-items: center; justify-content: center; padding: 16px; box-sizing: border-box; }
.login-card { width: 100%; max-width: 420px; }
.brand { display: flex; align-items: center; gap: 8px; font-size: 20px; margin: 0 0 16px; }
.code-row { display: flex; gap: 8px; width: 100%; }
.code-row .el-input { flex: 1; }
.submit { width: 100%; }
.hint { color: #909399; font-size: 12px; text-align: center; margin: 12px 0 0; }
</style>
```

`web/console/src/views/AccountView.vue`：

```vue
<script setup lang="ts">
import { fmtTime, kycStatusLabel, labelOf } from '@spark/shared'
import { ref } from 'vue'
import { useRouter } from 'vue-router'
import { api } from '../api'
import { clearSession, session } from '../store/session'

const router = useRouter()
const loggingOut = ref(false)

async function logout() {
  loggingOut.value = true
  try {
    await api.post('/auth/logout')
  } finally {
    clearSession()
    loggingOut.value = false
    await router.replace('/login')
  }
}
</script>

<template>
  <el-card data-testid="account-card">
    <el-descriptions :column="1" border>
      <el-descriptions-item label="手机号"><span data-testid="account-phone">{{ session.user?.phone }}</span></el-descriptions-item>
      <el-descriptions-item label="实名状态">
        <router-link to="/kyc" data-testid="account-kyc">{{ labelOf(kycStatusLabel, session.user?.kyc_status) }}</router-link>
      </el-descriptions-item>
      <el-descriptions-item label="注册时间"><span data-testid="account-created">{{ fmtTime(session.user?.created_at) }}</span></el-descriptions-item>
    </el-descriptions>
    <div class="actions">
      <router-link to="/help" data-testid="account-help">使用帮助</router-link>
      <el-button data-testid="account-logout" :loading="loggingOut" @click="logout">退出登录</el-button>
    </div>
  </el-card>
</template>

<style scoped>
.actions { display: flex; justify-content: space-between; align-items: center; margin-top: 16px; }
</style>
```

`web/console/src/views/HelpView.vue`（静态内容，不请求接口）：

```vue
<script setup lang="ts">
import { brand } from '@spark/shared'
</script>

<template>
  <div class="help" data-testid="help-page">
    <el-card>
      <h2 data-testid="help-ssh">怎么用 SSH 连接</h2>
      <ol>
        <li>实例状态变为「运行中」后，在实例详情页复制 SSH 命令（形如 <code>ssh ins-xxxxxxxx@ssh.您的域名</code>）。</li>
        <li>点击「显示」查看 SSH 密码并复制；密码只在您点击时才会显示，离开页面后需要重新查看。</li>
        <li>如遇换机，SSH 密码会变化，请在详情页重新查看。</li>
      </ol>
    </el-card>
    <el-card>
      <h2 data-testid="help-retention">数据保留规则</h2>
      <ul>
        <li>到期停机后数据保留 6 小时，期间续费可恢复运行并保留数据。</li>
        <li>超过保留期或主动释放，数据立即清除且已付费用不退。</li>
      </ul>
    </el-card>
    <el-card>
      <h2 data-testid="help-billing">计费规则</h2>
      <ul>
        <li>预付包时长：1 天 / 1 周 / 1 月（按 30 天计），从开通成功开始计时。</li>
        <li>机器故障期间暂停计时，恢复后自动补回暂停的时长。</li>
        <li>停机期间续费，时长在重新启动成功后开始计算。</li>
      </ul>
      <p class="support">{{ brand.supportText }}</p>
    </el-card>
  </div>
</template>

<style scoped>
.help { display: grid; gap: 16px; max-width: 760px; }
h2 { font-size: 16px; margin: 0 0 8px; }
.support { color: #909399; font-size: 12px; }
</style>
```

`web/console/src/views/NotFoundView.vue`：

```vue
<script setup lang="ts">
import { useRouter } from 'vue-router'

const router = useRouter()
</script>

<template>
  <el-result icon="warning" title="页面不存在" sub-title="您访问的地址不存在或已失效" data-testid="notfound-page">
    <template #extra>
      <el-button type="primary" data-testid="notfound-home" @click="router.replace('/market')">返回算力市场</el-button>
    </template>
  </el-result>
</template>
```

页面骨架（每个文件一样的结构，只换标题；后续任务整体替换）：`MarketView.vue`（`算力市场`）、`InstancesView.vue`（`我的实例`）、`InstanceDetailView.vue`（`实例详情`）、`WalletView.vue`（`钱包`）、`RechargeView.vue`（`充值`）、`RechargeOrderView.vue`（`充值单`）、`KycView.vue`（`实名认证`）：

```vue
<template>
  <el-card><h2>算力市场</h2></el-card>
</template>
```

- [ ] **Step 5: 单测、类型检查与构建**

Run: `cd web && npm test && npm run build`
Expected: console 项目 Vitest PASS（login 3、session 2、router 1）；`vue-tsc --noEmit` 无错误；`vite build` 输出到 `internal/web/dist/console/`（`ls ../internal/web/dist/console/assets | head` 有带哈希的 js / css）。`git status` 应看到新生成的 `web/console/src/auto-imports.d.ts` 与 `web/console/src/components.d.ts`——一并提交。

- [ ] **Step 6: 端到端用例**

`web/e2e/console/nav.spec.ts`（C-NAV-02 在 Task 10 加入本文件）：

```ts
import { expect, test } from '@playwright/test'
import { isMobile, loginConsole, seedUser } from '../helpers/console'

test('C-NAV-01 响应式布局：电脑侧边菜单、手机底部四个标签', async ({ page }, info) => {
  const u = await seedUser()
  await loginConsole(page, u.phone)
  const tabs = ['market', 'instances', 'wallet', 'account']
  if (isMobile(info)) {
    await expect(page.getByTestId('layout-tabbar')).toBeVisible()
    for (const t of tabs) await expect(page.getByTestId(`tabbar-${t}`)).toBeVisible()
    await expect(page.getByTestId('layout-side')).toHaveCount(0)
    const box = await page.getByTestId('tabbar-wallet').boundingBox()
    expect(box?.height ?? 0).toBeGreaterThanOrEqual(44)
  } else {
    await expect(page.getByTestId('layout-side')).toBeVisible()
    for (const t of tabs) await expect(page.getByTestId(`menu-${t}`)).toBeVisible()
    await expect(page.getByTestId('layout-tabbar')).toHaveCount(0)
    await expect(page.getByTestId('layout-help')).toBeVisible()
  }
  await expect(page.getByTestId('layout-phone')).toHaveText(`${u.phone.slice(0, 3)}****${u.phone.slice(7)}`)
})

test('C-NAV-03 404 页并可返回市场', async ({ page }) => {
  await page.goto('/nope')
  await expect(page.getByTestId('notfound-page')).toBeVisible()
  await expect(page).toHaveTitle('页面不存在 - Spark Cloud')
  await page.getByTestId('notfound-home').click()
  await expect(page).toHaveURL(/\/market$/)
  await expect(page).toHaveTitle('算力市场 - Spark Cloud')
})
```

`web/e2e/console/auth.spec.ts`（C-AUTH-06 在 Task 12、C-AUTH-07 在 Task 11 加入本文件）：

```ts
import { expect, test } from '@playwright/test'
import { loginConsole, nav, seedUser } from '../helpers/console'
import { hooks } from '../helpers/hooks'
import { uniquePhone } from '../helpers/phone'

test('C-AUTH-01 未登录访问受保护页重定向到登录，登录后回到原页', async ({ page }) => {
  const u = await seedUser()
  await page.goto('/instances')
  await expect(page).toHaveURL(/\/login\?redirect=(%2F|\/)instances$/)
  await hooks.advance(61)
  await page.getByTestId('login-phone').fill(u.phone)
  await page.getByTestId('login-send').click()
  await page.getByTestId('login-code').fill('000000')
  await page.getByTestId('login-submit').click()
  await expect(page).toHaveURL(/\/instances$/)
})

test('C-AUTH-02 发送验证码：格式错误不发请求，合法号倒计时 60 秒', async ({ page }) => {
  await page.clock.install()
  let smsRequests = 0
  page.on('request', (r) => {
    if (r.url().includes('/api/v1/auth/sms-code')) smsRequests += 1
  })
  await page.goto('/login')
  await page.getByTestId('login-phone').fill('123')
  await page.getByTestId('login-send').click()
  await expect(page.getByText('手机号格式不正确')).toBeVisible()
  expect(smsRequests).toBe(0)

  await hooks.advance(61)
  await page.getByTestId('login-phone').fill(uniquePhone())
  await page.getByTestId('login-send').click()
  await expect(page.getByTestId('login-send')).toBeDisabled()
  await expect(page.getByTestId('login-send')).toHaveText(/\d+ 秒后重发/)
  expect(smsRequests).toBe(1)
  await page.clock.runFor(60_000)
  await expect(page.getByTestId('login-send')).toBeEnabled()
  await expect(page.getByTestId('login-send')).toHaveText('发送验证码')
})

test('C-AUTH-03 验证码错误停留在登录页', async ({ page }) => {
  await hooks.advance(61)
  await page.goto('/login')
  await page.getByTestId('login-phone').fill(uniquePhone())
  await page.getByTestId('login-send').click()
  await page.getByTestId('login-code').fill('111111')
  await page.getByTestId('login-submit').click()
  await expect(page.getByText('验证码错误或已过期')).toBeVisible()
  await expect(page).toHaveURL(/\/login$/)
})

test('C-AUTH-04 新手机号登录自动注册；刷新后会话保持', async ({ page }) => {
  const phone = uniquePhone()
  await loginConsole(page, phone)
  await expect(page).toHaveURL(/\/market$/)
  await expect(page.getByTestId('layout-phone')).toHaveText(`${phone.slice(0, 3)}****${phone.slice(7)}`)
  await page.reload()
  await expect(page.getByTestId('layout-phone')).toHaveText(`${phone.slice(0, 3)}****${phone.slice(7)}`)
})

test('C-AUTH-05 退出登录后回到登录页，再访问受保护页被重定向', async ({ page }, info) => {
  const u = await seedUser()
  await loginConsole(page, u.phone)
  await nav(page, info, 'account')
  await page.getByTestId('account-logout').click()
  await expect(page).toHaveURL(/\/login$/)
  await page.goto('/wallet')
  await expect(page).toHaveURL(/\/login\?redirect=(%2F|\/)wallet$/)
})
```

`web/e2e/console/account.spec.ts`：

```ts
import { expect, test } from '@playwright/test'
import { loginConsole, nav, seedUser } from '../helpers/console'

test('C-ACC-01 我的页显示手机号、实名状态链接与退出', async ({ page }, info) => {
  const u = await seedUser({ verified: true })
  await loginConsole(page, u.phone)
  await nav(page, info, 'account')
  await expect(page).toHaveURL(/\/account$/)
  await expect(page.getByTestId('account-phone')).toHaveText(u.phone)
  await expect(page.getByTestId('account-kyc')).toHaveText('已认证')
  await expect(page.getByTestId('account-kyc')).toHaveAttribute('href', '/kyc')
  await expect(page.getByTestId('account-created')).toHaveText(/^\d{4}-\d{2}-\d{2} \d{2}:\d{2}$/)
  await expect(page.getByTestId('account-logout')).toBeVisible()
  await expect(page.getByTestId('account-help')).toHaveAttribute('href', '/help')
})

test('C-ACC-02 帮助页未登录可访问，含三节标题', async ({ page }) => {
  await page.goto('/help')
  await expect(page).toHaveURL(/\/help$/)
  await expect(page.getByTestId('help-ssh')).toHaveText('怎么用 SSH 连接')
  await expect(page.getByTestId('help-retention')).toHaveText('数据保留规则')
  await expect(page.getByTestId('help-billing')).toHaveText('计费规则')
  await expect(page.getByTestId('help-page')).toContainText('如遇换机，SSH 密码会变化')
  await expect(page.getByTestId('help-page')).toContainText('保留 6 小时')
})
```

Run: `make web && cd web && npx playwright test console/nav.spec.ts console/auth.spec.ts console/account.spec.ts`
Expected: 9 个用例 × 2 个 project = 18 passed。失败时看 `web/test-results/` 的 trace（`npx playwright show-trace <zip>`）。

- [ ] **Step 7: 全量验证并提交**

Run: `go vet ./... && go test ./... && cd web && npm test && npm run build`
Expected: 全部 PASS。

```bash
git add web/package.json web/package-lock.json web/vitest.config.ts web/console web/e2e/console
git commit -m "feat(console): app shell, router guard, session store, responsive layout, login/account/help/404 with e2e

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---
### Task 9: 控制台「我的实例」列表

**Files:**
- Create: `web/console/src/composables/usePolling.ts`、`web/console/src/composables/useTicker.ts`、`web/console/src/logic/instance.ts`
- Replace: `web/console/src/views/InstancesView.vue`
- Create: `web/console/src/logic/__tests__/instance.spec.ts`
- Create: `web/e2e/console/instances.spec.ts`、`web/e2e/helpers/instances.ts`

**Interfaces:**
- Consumes: Task 8 的 `api`、`session`、布局；Task 7 helpers。
- Produces:
  - `usePolling(fn, intervalMs, opts?: { enabled?: () => boolean; immediateOnVisible?: boolean }): { start(); stop() }`：`setInterval` 定时执行；`document.visibilityState !== 'visible'` 或 `enabled()` 为假时跳过；恢复可见时立即执行一次（默认开）；组件卸载自动停止。
  - `useTicker(): Ref<number>`：每秒更新的 `Date.now()`（整页共用一个定时器驱动所有倒计时）。
  - `src/logic/instance.ts`：`remainingNow(i, fetchedAt, now)`、`instanceTimeText(i, remaining)`（运行中 `剩余 X` / 停机 `数据保留至 yyyy-MM-dd HH:mm` / `已付 X 待开始` / 失败 `费用已原路退回余额`）、`releaseWarning(i): string | null`（仅 `stopped && pending_seconds > 0`）、`canRenew(state)`（creating / running / stopped）、`canRelease(state)`（running / stopped）。
  - 列表 testid：`instances-table`（电脑 `el-table`）、`instances-cards` / `instance-card`（手机）、`instances-empty`；每行 / 卡片内 `instance-id`（链接到详情）、`instance-image`、`instance-state`（`el-tag`，class 含 `el-tag--<type>`）、`instance-time`（属性 `data-remaining` 为当前剩余秒）、`instance-suspended`。
  - e2e helper `helpers/instances.ts`：`instanceRow(page, info, id): Locator`（电脑 `getByRole('row')`、手机 `getByTestId('instance-card')`，按 ID 过滤）、`runningInstance(phone)`（API 下单 + `hooks.agent()`，返回 `{ api, id }`）、`stateLabels`。
  - 覆盖：C-INS-01、C-INS-02。

- [ ] **Step 1: 写失败的单测（U-11 与状态 / 文案逻辑）**

`web/console/src/logic/__tests__/instance.spec.ts`：

```ts
import type { Instance } from '@spark/shared'
import { describe, expect, it } from 'vitest'
import { canRelease, canRenew, instanceTimeText, releaseWarning, remainingNow } from '../instance'

const base: Instance = {
  id: 'ins-abc12345', state: 'running', product_id: 1, image_id: 1, product_name: 'DGX Spark 整机', image_name: 'PyTorch', suspended: false,
  expires_at: '2026-10-18T06:00:00Z', remaining_seconds: 86399, pending_seconds: 0, retain_until: null, ssh_command: 'ssh ins-abc12345@ssh.spark.test',
  created_at: '2026-10-17T06:00:00Z',
}

describe('releaseWarning (U-11)', () => {
  it('仅 stopped 且 pending_seconds > 0 时返回追加文案', () => {
    expect(releaseWarning({ ...base, state: 'stopped', pending_seconds: 86400 })).toBe('该实例有已付费但尚未开始计时的 1 天，释放后这部分时长也会作废')
    expect(releaseWarning({ ...base, state: 'stopped', pending_seconds: 0 })).toBeNull()
    expect(releaseWarning({ ...base, state: 'running', pending_seconds: 86400 })).toBeNull()
  })
})

describe('instance time text', () => {
  it('运行中按剩余秒递减', () => {
    expect(remainingNow(base, 1000, 4000)).toBe(86396)
    expect(instanceTimeText(base, 86396)).toBe('剩余 23 小时 59 分')
  })
  it('暂停中不递减', () => {
    expect(remainingNow({ ...base, suspended: true }, 1000, 99_000)).toBe(86399)
  })
  it('停机显示保留时间，待开始显示已付时长，失败显示退款', () => {
    expect(instanceTimeText({ ...base, state: 'stopped', retain_until: '2026-10-18T12:00:00Z' }, 0)).toBe('数据保留至 2026-10-18 20:00')
    expect(instanceTimeText({ ...base, state: 'stopped', retain_until: null, pending_seconds: 604800 }, 0)).toBe('已付 7 天 待开始')
    expect(instanceTimeText({ ...base, state: 'creating', pending_seconds: 86400 }, 0)).toBe('已付 1 天 待开始')
    expect(instanceTimeText({ ...base, state: 'failed', pending_seconds: 0 }, 0)).toBe('费用已原路退回余额')
    expect(instanceTimeText({ ...base, state: 'released', pending_seconds: 0 }, 0)).toBe('')
  })
})

describe('按钮与状态匹配 (C-DET-09 的逻辑)', () => {
  it.each([
    ['creating', true, false],
    ['running', true, true],
    ['stopped', true, true],
    ['wiping', false, false],
    ['released', false, false],
    ['failed', false, false],
  ] as const)('%s renew=%s release=%s', (state, renew, release) => {
    expect(canRenew(state)).toBe(renew)
    expect(canRelease(state)).toBe(release)
  })
})
```

Run: `cd web && npm test`
Expected: FAIL（模块不存在）。

- [ ] **Step 2: 实现 composables 与逻辑**

`web/console/src/composables/usePolling.ts`：

```ts
import { getCurrentScope, onMounted, onScopeDispose } from 'vue'

export interface PollingOptions {
  /** 返回 false 时本轮跳过（例如没有开通中 / 洗机中的实例） */
  enabled?: () => boolean
  /** 页面从不可见恢复为可见时立即执行一次（默认 true） */
  immediateOnVisible?: boolean
}

/** 可见时按固定间隔轮询；不可见时暂停；组件卸载自动停止。 */
export function usePolling(fn: () => void | Promise<void>, intervalMs: number, opts: PollingOptions = {}) {
  let timer: ReturnType<typeof setInterval> | undefined
  const visible = () => document.visibilityState === 'visible'
  const tick = () => {
    if (visible() && (opts.enabled?.() ?? true)) void fn()
  }
  const onVisibility = () => {
    if (visible() && (opts.immediateOnVisible ?? true) && (opts.enabled?.() ?? true)) void fn()
  }
  function start() {
    stop()
    timer = setInterval(tick, intervalMs)
  }
  function stop() {
    if (timer !== undefined) {
      clearInterval(timer)
      timer = undefined
    }
  }
  onMounted(() => {
    document.addEventListener('visibilitychange', onVisibility)
    start()
  })
  if (getCurrentScope()) {
    onScopeDispose(() => {
      document.removeEventListener('visibilitychange', onVisibility)
      stop()
    })
  }
  return { start, stop }
}
```

`web/console/src/composables/useTicker.ts`：

```ts
import { getCurrentScope, onScopeDispose, ref, type Ref } from 'vue'

/** 每秒更新的当前时间戳，驱动页面上所有倒计时（按真实流逝时间计算，不累计 tick）。 */
export function useTicker(): Ref<number> {
  const now = ref(Date.now())
  const timer = setInterval(() => {
    now.value = Date.now()
  }, 1000)
  if (getCurrentScope()) onScopeDispose(() => clearInterval(timer))
  return now
}
```

`web/console/src/logic/instance.ts`：

```ts
import { fmtDuration, fmtTime, type Instance, type InstanceState } from '@spark/shared'

/** 以拿到数据的本地时刻为基准递减；暂停中与非运行状态不递减。 */
export function remainingNow(i: Instance, fetchedAt: number, now: number): number {
  if (i.state !== 'running' || i.suspended) return i.remaining_seconds
  return Math.max(0, i.remaining_seconds - Math.floor((now - fetchedAt) / 1000))
}

export function instanceTimeText(i: Instance, remaining: number): string {
  if (i.state === 'running') return `剩余 ${fmtDuration(remaining)}`
  if (i.state === 'stopped' && i.retain_until) return `数据保留至 ${fmtTime(i.retain_until)}`
  if (i.pending_seconds > 0) return `已付 ${fmtDuration(i.pending_seconds)} 待开始`
  if (i.state === 'failed') return '费用已原路退回余额'
  return ''
}

/** 停机且有待开始的续费：释放会作废这部分时长（计划 1 遗留）。 */
export function releaseWarning(i: Instance): string | null {
  if (i.state === 'stopped' && i.pending_seconds > 0) {
    return `该实例有已付费但尚未开始计时的 ${fmtDuration(i.pending_seconds)}，释放后这部分时长也会作废`
  }
  return null
}

export const canRenew = (s: InstanceState) => s === 'creating' || s === 'running' || s === 'stopped'
export const canRelease = (s: InstanceState) => s === 'running' || s === 'stopped'
```

- [ ] **Step 3: 列表页**

`web/console/src/views/InstancesView.vue`：

```vue
<script setup lang="ts">
import { instanceStateLabel, instanceStateType, labelOf, messageOf, useMobile, type Instance, type Listed } from '@spark/shared'
import { computed, onMounted, ref } from 'vue'
import { api } from '../api'
import { usePolling } from '../composables/usePolling'
import { useTicker } from '../composables/useTicker'
import { instanceTimeText, remainingNow } from '../logic/instance'

const mobile = useMobile()
const items = ref<Instance[]>([])
const fetchedAt = ref(0)
const loading = ref(true)
const loadError = ref('')
const now = useTicker()

async function load() {
  try {
    items.value = (await api.get<Listed<Instance>>('/instances')).items // 后端已按创建时间倒序、最多 100 条
    fetchedAt.value = Date.now()
    loadError.value = ''
  } catch (e) {
    loadError.value = messageOf(e)
  } finally {
    loading.value = false
  }
}

const busy = computed(() => items.value.some((i) => i.state === 'creating' || i.state === 'wiping'))
usePolling(load, 10_000, { enabled: () => busy.value })
onMounted(load)

const rows = computed(() =>
  items.value.map((i) => {
    const remaining = remainingNow(i, fetchedAt.value, now.value)
    return { ...i, remaining, timeText: instanceTimeText(i, remaining) }
  }),
)
</script>

<template>
  <div class="instances">
    <el-alert v-if="loadError" type="error" :title="loadError" show-icon :closable="false" />
    <el-skeleton v-else-if="loading" :rows="4" animated />
    <el-empty v-else-if="rows.length === 0" description="还没有实例，去算力市场租一台吧" data-testid="instances-empty">
      <router-link to="/market"><el-button type="primary">去算力市场</el-button></router-link>
    </el-empty>

    <div v-else-if="mobile" class="cards" data-testid="instances-cards">
      <el-card v-for="i in rows" :key="i.id" class="card" data-testid="instance-card" :data-id="i.id" :data-state="i.state">
        <div class="card-head">
          <router-link :to="`/instances/${i.id}`" class="mono" data-testid="instance-id">{{ i.id }}</router-link>
          <el-tag :type="labelOf(instanceStateType, i.state) as 'primary'" data-testid="instance-state">{{ labelOf(instanceStateLabel, i.state) }}</el-tag>
        </div>
        <div class="muted" data-testid="instance-image">{{ i.image_name }}</div>
        <div data-testid="instance-time" :data-remaining="i.remaining">{{ i.timeText }}</div>
        <div v-if="i.suspended" class="warn" data-testid="instance-suspended">计时已暂停（机器维护中）</div>
      </el-card>
    </div>

    <el-table v-else :data="rows" data-testid="instances-table" row-key="id">
      <el-table-column label="实例 ID" min-width="170">
        <template #default="{ row }"><router-link :to="`/instances/${row.id}`" class="mono" data-testid="instance-id">{{ row.id }}</router-link></template>
      </el-table-column>
      <el-table-column label="镜像" min-width="160">
        <template #default="{ row }"><span data-testid="instance-image">{{ row.image_name }}</span></template>
      </el-table-column>
      <el-table-column label="状态" width="130">
        <template #default="{ row }">
          <el-tag :type="labelOf(instanceStateType, row.state) as 'primary'" data-testid="instance-state">{{ labelOf(instanceStateLabel, row.state) }}</el-tag>
          <div v-if="row.suspended" class="warn" data-testid="instance-suspended">计时已暂停（机器维护中）</div>
        </template>
      </el-table-column>
      <el-table-column label="时长" min-width="220">
        <template #default="{ row }"><span data-testid="instance-time" :data-remaining="row.remaining">{{ row.timeText }}</span></template>
      </el-table-column>
    </el-table>
  </div>
</template>

<style scoped>
.cards { display: grid; gap: 12px; }
.card-head { display: flex; justify-content: space-between; align-items: center; margin-bottom: 6px; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.muted { color: #909399; font-size: 13px; margin-bottom: 4px; }
.warn { color: var(--el-color-warning); font-size: 12px; }
</style>
```

Run: `cd web && npm test && npm run build`
Expected: Vitest PASS（instance 4 个 describe）；构建通过。

- [ ] **Step 4: 端到端用例**

`web/e2e/helpers/instances.ts`：

```ts
import { type APIRequestContext, type Locator, type Page, type TestInfo } from '@playwright/test'
import { isMobile, placeOrder, userApi } from './console'
import { hooks } from './hooks'

export const stateLabels: Record<string, { text: string; type: string }> = {
  creating: { text: '开通中', type: 'primary' },
  running: { text: '运行中', type: 'success' },
  stopped: { text: '已到期停机', type: 'warning' },
  wiping: { text: '洗机中', type: 'info' },
  released: { text: '已释放', type: 'info' },
  failed: { text: '开通失败', type: 'danger' },
}

/** 列表里某个实例所在的行（电脑 el-table 的 row）或卡片（手机）。 */
export function instanceRow(page: Page, info: TestInfo, id: string): Locator {
  const idCell = page.getByTestId('instance-id').filter({ hasText: id })
  return isMobile(info) ? page.getByTestId('instance-card').filter({ has: idCell }) : page.getByRole('row').filter({ has: idCell })
}

/** API 下单并让 Agent 开通成功，返回运行中的实例。 */
export async function runningInstance(phone: string, opts: { duration?: '1d' | '1w' | '1m' } = {}): Promise<{ api: APIRequestContext; id: string }> {
  const api = await userApi(phone)
  await hooks.machines(1)
  const id = await placeOrder(api, opts)
  await hooks.agent()
  return { api, id }
}

export async function instanceOf(api: APIRequestContext, id: string) {
  return (await (await api.get(`/api/v1/instances/${id}`)).json()) as { state: string; remaining_seconds: number; retain_until: string | null; pending_seconds: number; suspended: boolean }
}
```

`web/e2e/console/instances.spec.ts`：

```ts
import { expect, test } from '@playwright/test'
import { loginConsole, nav, placeOrder, seedUser, userApi } from '../helpers/console'
import { hooks } from '../helpers/hooks'
import { instanceRow, stateLabels } from '../helpers/instances'

test('C-INS-01 六种状态同时展示，标签文案与颜色正确，按创建时间倒序', async ({ page }, info) => {
  const u = await seedUser({ balance: 1_000_000 })
  const api = await userApi(u.phone)
  await hooks.machines(6)
  // 每一步之间推进 1 秒，让 created_at 严格递增；agent() 作用于全部机器，所以按「不会被后续 agent 打扰」的顺序构造
  const next = async () => {
    await hooks.advance(1)
    return placeOrder(api)
  }
  const stopped = await next()
  await hooks.agent()
  await hooks.advance(86400)
  await hooks.sweep()
  await hooks.agent() // 完成停机指令
  const released = await next()
  await hooks.agent()
  await api.post(`/api/v1/instances/${released}/release`, { data: {} })
  await hooks.agent()
  const failed = await next()
  await hooks.agent({ fail: true })
  const running = await next()
  await hooks.agent()
  const wiping = await next()
  await hooks.agent()
  await api.post(`/api/v1/instances/${wiping}/release`, { data: {} })
  const creating = await next()

  await loginConsole(page, u.phone)
  await nav(page, info, 'instances')
  const expectedOrder = [creating, wiping, running, failed, released, stopped]
  await expect(page.getByTestId('instance-id')).toHaveText(expectedOrder)
  const states = { creating, wiping, running, failed, released, stopped }
  for (const [state, id] of Object.entries(states)) {
    const row = instanceRow(page, info, id)
    await expect(row.getByTestId('instance-state')).toHaveText(stateLabels[state].text)
    await expect(row.getByTestId('instance-state')).toHaveClass(new RegExp(`el-tag--${stateLabels[state].type}`))
  }
  await expect(instanceRow(page, info, creating).getByTestId('instance-time')).toHaveText('已付 1 天 待开始')
  await expect(instanceRow(page, info, failed).getByTestId('instance-time')).toHaveText('费用已原路退回余额')
  await expect(instanceRow(page, info, stopped).getByTestId('instance-time')).toHaveText(/^数据保留至 \d{4}-\d{2}-\d{2} \d{2}:\d{2}$/)
  if (info.project.name === 'console-mobile') await expect(page.getByTestId('instances-cards')).toBeVisible()
  else await expect(page.getByTestId('instances-table')).toBeVisible()
})

test('C-INS-02 开通中实例每 10 秒自动刷新为运行中', async ({ page }, info) => {
  const u = await seedUser()
  const api = await userApi(u.phone)
  await hooks.machines(1)
  const id = await placeOrder(api)
  await page.clock.install()
  await loginConsole(page, u.phone)
  await nav(page, info, 'instances')
  const row = instanceRow(page, info, id)
  await expect(row.getByTestId('instance-state')).toHaveText('开通中')
  await hooks.agent()
  await page.clock.runFor(10_000)
  await expect(row.getByTestId('instance-state')).toHaveText('运行中')
  await expect(row.getByTestId('instance-time')).toHaveText(/^剩余 /)
})
```

Run: `make web && cd web && npx playwright test console/instances.spec.ts`
Expected: 2 × 2 = 4 passed。

- [ ] **Step 5: 全量验证并提交**

Run: `go vet ./... && go test ./... && cd web && npm test && npm run build && npx playwright test console/`
Expected: 全部 PASS（此时控制台 e2e 共 11 个用例 × 2）。

```bash
git add web/console web/e2e
git commit -m "feat(console): instance list with state tags, countdown ticker and 10s polling; e2e C-INS-01/02

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---
### Task 10: 控制台实例详情：SSH、密码、续费、释放

**Files:**
- Replace: `web/console/src/views/InstanceDetailView.vue`
- Create: `web/console/src/composables/useGuide.ts`、`web/console/src/components/GuideDialog.vue`
- Create: `web/e2e/helpers/time.ts`
- Create: `web/e2e/console/instance-detail.spec.ts`
- Modify: `web/e2e/console/nav.spec.ts`（C-NAV-02）、`web/e2e/console/instances.spec.ts`（C-INS-03、C-INS-04）

**Interfaces:**
- Consumes: Task 9 的 `logic/instance.ts`、`useTicker`；Task 8 的 `AppDialog`、`session`、`refreshBalance`；`useIdempotencyKey`、`useCountdown`。
- Produces:
  - `useGuide()`：`{ state, kycRequired(), insufficientBalance(), handle(err, opts?: { onNoCapacity?: () => void }) }`——控制台统一错误引导：`kyc_required` → 对话「请先完成实名认证」+「去实名」（`/kyc?redirect=<当前>`）；`insufficient_balance` → 「余额不足」+「去充值」（`/wallet/recharge?redirect=<当前>`）；`banned` → `handleUnauthorized('账号已被封禁')`；`no_capacity` → 提示「暂无空闲机器」并回调；`cancelled` / `unauthorized` 静默；其他 `ElMessage.error(messageOf)`。`GuideDialog`（prop `guide`）testid：`guide-dialog`、`guide-action`、`guide-cancel`。
  - 详情 testid：`detail-id`、`detail-product`、`detail-image`、`detail-state`、`detail-created`、`detail-expires`、`detail-remaining`（`data-remaining`）、`detail-pending`、`detail-suspended`、`detail-failed-hint`；SSH：`detail-ssh-command`、`detail-ssh-copy`、`detail-ssh-hint`、`detail-password`（默认 `••••••••`）、`detail-password-show`、`detail-password-copy`；续费：`detail-renew-btn`、`detail-renew-dialog`、`detail-renew-duration-<1d|1w|1m>`、`detail-renew-price`、`detail-renew-balance`、`detail-renew-after`、`detail-renew-ok`（余额不足时文案「去充值」）、`detail-renew-cancel`；释放：`detail-release-btn`、`detail-release-dialog`、`detail-release-text`、`detail-release-warning`、`detail-release-ok`（文案「确定释放」、`type="danger"`）、`detail-release-cancel`。
  - e2e helper `helpers/time.ts`：`cstTime(iso): string`（与 `fmtTime` 同格式）。
  - 覆盖：C-DET-01～09、C-INS-03、C-INS-04、C-NAV-02（含 Review Focus 5 的双击断言）。

- [ ] **Step 1: 引导对话与详情页**

`web/console/src/composables/useGuide.ts`：

```ts
import { isApiError, messageOf } from '@spark/shared'
import { ElMessage } from 'element-plus'
import { reactive } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { handleUnauthorized } from '../store/session'

export interface GuideState {
  visible: boolean
  title: string
  message: string
  action: string
  to: { path: string; query: Record<string, string> } | null
}

/** 控制台统一的「需要引导」错误处理：实名、充值、封禁、无空闲机器。 */
export function useGuide() {
  const route = useRoute()
  const router = useRouter()
  const state = reactive<GuideState>({ visible: false, title: '', message: '', action: '', to: null })

  function show(title: string, message: string, action: string, path: string) {
    Object.assign(state, { visible: true, title, message, action, to: { path, query: { redirect: route.fullPath } } })
  }
  function kycRequired() {
    show('请先完成实名认证', '按监管要求，下单与充值前需要完成实名认证。', '去实名', '/kyc')
  }
  function insufficientBalance() {
    show('余额不足', '当前余额不足以支付本次费用，请先充值。', '去充值', '/wallet/recharge')
  }
  async function go() {
    state.visible = false
    if (state.to) await router.push(state.to)
  }
  function handle(err: unknown, opts: { onNoCapacity?: () => void } = {}) {
    if (!isApiError(err)) {
      ElMessage.error(messageOf(err))
      return
    }
    switch (err.code) {
      case 'kyc_required':
        return kycRequired()
      case 'insufficient_balance':
        return insufficientBalance()
      case 'banned':
        return handleUnauthorized('账号已被封禁')
      case 'no_capacity':
        ElMessage.warning('暂无空闲机器')
        return opts.onNoCapacity?.()
      case 'cancelled':
      case 'unauthorized':
        return
      default:
        ElMessage.error(messageOf(err))
    }
  }
  return { state, kycRequired, insufficientBalance, handle, go }
}

export type Guide = ReturnType<typeof useGuide>
```

`web/console/src/components/GuideDialog.vue`：

```vue
<script setup lang="ts">
import type { Guide } from '../composables/useGuide'
import AppDialog from './AppDialog.vue'

const props = defineProps<{ guide: Guide }>()
</script>

<template>
  <AppDialog v-model="props.guide.state.visible" kind="confirm" :title="props.guide.state.title">
    <p data-testid="guide-dialog">{{ props.guide.state.message }}</p>
    <template #footer>
      <el-button data-testid="guide-cancel" @click="props.guide.state.visible = false">取消</el-button>
      <el-button type="primary" data-testid="guide-action" @click="props.guide.go()">{{ props.guide.state.action }}</el-button>
    </template>
  </AppDialog>
</template>
```

`web/console/src/views/InstanceDetailView.vue`：

```vue
<script setup lang="ts">
import {
  durationLabel, fmtDuration, fmtTime, fmtYuan, instanceStateLabel, instanceStateType, labelOf, messageOf, useCountdown, useIdempotencyKey,
  type Duration, type Instance, type Listed, type Order, type Product,
} from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api } from '../api'
import AppDialog from '../components/AppDialog.vue'
import GuideDialog from '../components/GuideDialog.vue'
import { useGuide } from '../composables/useGuide'
import { canRelease, canRenew, releaseWarning } from '../logic/instance'
import { refreshBalance, session } from '../store/session'

const route = useRoute()
const router = useRouter()
const guide = useGuide()
const id = computed(() => String(route.params.id))
const inst = ref<Instance | null>(null)
const loadError = ref('')
const countdown = useCountdown()

async function load() {
  try {
    inst.value = await api.get<Instance>(`/instances/${id.value}`)
    loadError.value = ''
    if (inst.value.state === 'running' && !inst.value.suspended) countdown.start(inst.value.remaining_seconds)
    else {
      countdown.stop()
      countdown.remaining.value = inst.value.remaining_seconds
    }
  } catch (e) {
    loadError.value = messageOf(e)
  }
}
onMounted(load)

// ---- SSH 密码：只在点击时请求，只放内存，离开页面即丢弃 ----
const password = ref('')
const loadingPassword = ref(false)
async function showPassword() {
  loadingPassword.value = true
  try {
    password.value = (await api.get<{ password: string }>(`/instances/${id.value}/password`)).password
  } catch (e) {
    ElMessage.error(messageOf(e))
  } finally {
    loadingPassword.value = false
  }
}
async function copy(text: string) {
  try {
    await navigator.clipboard.writeText(text)
    ElMessage.success('已复制')
  } catch {
    ElMessage.error('复制失败，请手动选择复制')
  }
}

// ---- 续费 ----
const renewVisible = ref(false)
const renewDuration = ref<Duration>('1d')
const product = ref<Product | null>(null)
const renewing = ref(false)
const renewKey = useIdempotencyKey(() => [id.value, renewDuration.value])
const renewPrice = computed(() => product.value?.prices.find((p) => p.duration === renewDuration.value)?.amount ?? null)
const balance = computed(() => session.balance ?? 0)
const renewAfter = computed(() => (renewPrice.value == null ? null : balance.value - renewPrice.value))
const renewInsufficient = computed(() => renewPrice.value != null && balance.value < renewPrice.value)

async function openRenew() {
  const [{ items }] = await Promise.all([api.get<Listed<Product>>('/products'), refreshBalance()])
  product.value = items.find((p) => p.id === inst.value?.product_id) ?? null
  renewDuration.value = product.value?.prices[0]?.duration ?? '1d'
  renewVisible.value = true
}
async function confirmRenew() {
  if (!inst.value) return
  if (renewInsufficient.value) {
    renewVisible.value = false
    await router.push({ path: '/wallet/recharge', query: { redirect: route.fullPath } }) // 「去充值」直接跳转，带 redirect 回来
    return
  }
  renewing.value = true
  const wasStopped = inst.value.state === 'stopped'
  try {
    await api.post<{ order: Order }>(`/instances/${id.value}/renew`, { duration: renewDuration.value }, { idempotencyKey: renewKey.current() })
    renewKey.rotate()
    renewVisible.value = false
    ElMessage.success(wasStopped ? '续费成功，正在重新启动' : '续费成功')
    await Promise.all([load(), refreshBalance()])
  } catch (e) {
    renewVisible.value = false
    guide.handle(e)
  } finally {
    renewing.value = false
  }
}

// ---- 释放：一层确认框，确认按钮点击即执行（评审裁定 2） ----
const releaseVisible = ref(false)
const releasing = ref(false)
const warning = computed(() => (inst.value ? releaseWarning(inst.value) : null))
async function confirmRelease() {
  if (releasing.value) return // 双击保护：请求进行中第二次点击不发请求
  releasing.value = true
  try {
    await api.post(`/instances/${id.value}/release`, {})
    releaseVisible.value = false
    ElMessage.success('已提交释放，正在清除数据')
    await load()
  } catch (e) {
    releaseVisible.value = false
    guide.handle(e)
  } finally {
    releasing.value = false
  }
}

watch(id, load)
</script>

<template>
  <div class="detail">
    <el-alert v-if="loadError" type="error" :title="loadError" show-icon :closable="false" />
    <template v-else-if="inst">
      <el-card>
        <template #header>
          <div class="head">
            <span class="mono" data-testid="detail-id">{{ inst.id }}</span>
            <el-tag :type="labelOf(instanceStateType, inst.state) as 'primary'" data-testid="detail-state">{{ labelOf(instanceStateLabel, inst.state) }}</el-tag>
          </div>
        </template>
        <el-alert v-if="inst.state === 'failed'" type="error" title="开通失败，费用已原路退回余额" show-icon :closable="false" data-testid="detail-failed-hint" />
        <el-alert v-if="inst.suspended" type="warning" title="计时已暂停（机器维护中）" show-icon :closable="false" data-testid="detail-suspended" />
        <el-descriptions :column="1" border>
          <el-descriptions-item label="商品"><span data-testid="detail-product">{{ inst.product_name }}</span></el-descriptions-item>
          <el-descriptions-item label="镜像"><span data-testid="detail-image">{{ inst.image_name }}</span></el-descriptions-item>
          <el-descriptions-item label="创建时间"><span data-testid="detail-created">{{ fmtTime(inst.created_at) }}</span></el-descriptions-item>
          <el-descriptions-item label="到期时间"><span data-testid="detail-expires">{{ inst.expires_at ? fmtTime(inst.expires_at) : '—' }}</span></el-descriptions-item>
          <el-descriptions-item label="剩余时长">
            <span data-testid="detail-remaining" :data-remaining="countdown.remaining.value">{{ inst.state === 'running' ? fmtDuration(countdown.remaining.value) : '—' }}</span>
          </el-descriptions-item>
          <el-descriptions-item v-if="inst.pending_seconds > 0" label="已付待开始">
            <span data-testid="detail-pending">{{ fmtDuration(inst.pending_seconds) }}</span>
          </el-descriptions-item>
          <el-descriptions-item v-if="inst.state === 'stopped' && inst.retain_until" label="数据保留至">
            <span data-testid="detail-retain">{{ fmtTime(inst.retain_until) }}</span>
          </el-descriptions-item>
        </el-descriptions>
        <div class="actions">
          <el-button v-if="canRenew(inst.state)" type="primary" data-testid="detail-renew-btn" @click="openRenew">续费</el-button>
          <el-button v-if="canRelease(inst.state)" type="danger" plain data-testid="detail-release-btn" @click="releaseVisible = true">释放</el-button>
        </div>
      </el-card>

      <el-card header="SSH 连接">
        <template v-if="inst.state === 'running' && inst.ssh_command">
          <div class="row">
            <code class="mono" data-testid="detail-ssh-command">{{ inst.ssh_command }}</code>
            <el-button size="small" data-testid="detail-ssh-copy" @click="copy(inst.ssh_command)">复制</el-button>
          </div>
          <div class="row">
            <span>密码：</span>
            <code class="mono" data-testid="detail-password">{{ password || '••••••••' }}</code>
            <el-button v-if="!password" size="small" :loading="loadingPassword" data-testid="detail-password-show" @click="showPassword">显示</el-button>
            <el-button v-else size="small" data-testid="detail-password-copy" @click="copy(password)">复制</el-button>
          </div>
        </template>
        <p v-else class="muted" data-testid="detail-ssh-hint">实例运行中才能连接</p>
      </el-card>
    </template>

    <AppDialog v-model="renewVisible" kind="form" title="续费" data-testid="detail-renew-dialog">
      <el-form label-position="top">
        <el-form-item label="租期">
          <el-radio-group v-model="renewDuration">
            <el-radio-button v-for="p in product?.prices ?? []" :key="p.duration" :value="p.duration" :data-testid="`detail-renew-duration-${p.duration}`">
              {{ labelOf(durationLabel, p.duration) }} {{ fmtYuan(p.amount) }}
            </el-radio-button>
          </el-radio-group>
        </el-form-item>
        <el-descriptions :column="1" border>
          <el-descriptions-item label="价格"><span data-testid="detail-renew-price">{{ renewPrice == null ? '—' : fmtYuan(renewPrice) }}</span></el-descriptions-item>
          <el-descriptions-item label="当前余额"><span data-testid="detail-renew-balance">{{ fmtYuan(balance) }}</span></el-descriptions-item>
          <el-descriptions-item label="扣款后余额">
            <span data-testid="detail-renew-after" :class="{ warn: renewInsufficient }">{{ renewAfter == null ? '—' : fmtYuan(renewAfter) }}</span>
            <span v-if="renewInsufficient" class="warn">（余额不足）</span>
          </el-descriptions-item>
        </el-descriptions>
      </el-form>
      <template #footer>
        <el-button data-testid="detail-renew-cancel" @click="renewVisible = false">取消</el-button>
        <el-button type="primary" :loading="renewing" data-testid="detail-renew-ok" @click="confirmRenew">{{ renewInsufficient ? '去充值' : '确认续费' }}</el-button>
      </template>
    </AppDialog>

    <AppDialog v-model="releaseVisible" kind="confirm" title="释放实例" data-testid="detail-release-dialog">
      <p data-testid="detail-release-text">释放后数据将被清除，已付费用不退，确定释放吗？</p>
      <p v-if="warning" class="danger" data-testid="detail-release-warning">{{ warning }}</p>
      <template #footer>
        <el-button data-testid="detail-release-cancel" @click="releaseVisible = false">取消</el-button>
        <el-button type="danger" :loading="releasing" :disabled="releasing" data-testid="detail-release-ok" @click="confirmRelease">确定释放</el-button>
      </template>
    </AppDialog>
    <GuideDialog :guide="guide" />
  </div>
</template>

<style scoped>
.detail { display: grid; gap: 16px; max-width: 760px; }
.head { display: flex; justify-content: space-between; align-items: center; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; word-break: break-all; }
.row { display: flex; align-items: center; gap: 8px; margin: 8px 0; flex-wrap: wrap; }
.actions { display: flex; gap: 12px; margin-top: 16px; }
.muted { color: #909399; }
.warn { color: var(--el-color-warning); }
.danger { color: var(--el-color-danger); }
</style>
```

Run: `cd web && npm run build`
Expected: `vue-tsc` 与构建通过。

- [ ] **Step 2: 端到端用例**

`web/e2e/helpers/time.ts`：

```ts
const fmt = new Intl.DateTimeFormat('en-US', { timeZone: 'Asia/Shanghai', hourCycle: 'h23', year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' })

/** 与 @spark/shared 的 fmtTime 同格式：北京时间 yyyy-MM-dd HH:mm。 */
export function cstTime(iso: string): string {
  const p: Record<string, string> = {}
  for (const x of fmt.formatToParts(new Date(iso))) p[x.type] = x.value
  return `${p.year}-${p.month}-${p.day} ${p.hour}:${p.minute}`
}
```

`web/e2e/console/instance-detail.spec.ts`：

```ts
import { expect, test, type Page } from '@playwright/test'
import { dialog, loginConsole, placeOrder, seedUser, toast, userApi } from '../helpers/console'
import { hooks } from '../helpers/hooks'
import { instanceOf, runningInstance } from '../helpers/instances'

const clipboard = (page: Page) => page.evaluate(() => navigator.clipboard.readText())

test('C-DET-01 SSH 命令显示与复制；非运行中显示提示', async ({ page }) => {
  const u = await seedUser()
  const { api, id } = await runningInstance(u.phone)
  await loginConsole(page, u.phone)
  await page.goto(`/instances/${id}`)
  await expect(page.getByTestId('detail-ssh-command')).toHaveText(`ssh ${id}@ssh.spark.test`)
  await page.getByTestId('detail-ssh-copy').click()
  await toast(page, '已复制')
  expect(await clipboard(page)).toBe(`ssh ${id}@ssh.spark.test`)
  await hooks.machines(1)
  const creating = await placeOrder(api)
  await page.goto(`/instances/${creating}`)
  await expect(page.getByTestId('detail-ssh-hint')).toHaveText('实例运行中才能连接')
  await expect(page.getByTestId('detail-ssh-command')).toHaveCount(0)
})

test('C-DET-02 密码默认隐藏，点显示才请求，再次进入仍隐藏', async ({ page }) => {
  const u = await seedUser()
  const { id } = await runningInstance(u.phone)
  let passwordRequests = 0
  page.on('request', (r) => {
    if (r.url().endsWith(`/api/v1/instances/${id}/password`)) passwordRequests += 1
  })
  await loginConsole(page, u.phone)
  await page.goto(`/instances/${id}`)
  await expect(page.getByTestId('detail-password')).toHaveText('••••••••')
  expect(passwordRequests).toBe(0)
  await page.getByTestId('detail-password-show').click()
  await expect(page.getByTestId('detail-password')).toHaveText(/^[A-Za-z0-9!@#$%^&*_+\-=?]{16}$/)
  expect(passwordRequests).toBe(1)
  const pw = await page.getByTestId('detail-password').textContent()
  await page.getByTestId('detail-password-copy').click()
  expect(await clipboard(page)).toBe(pw)
  await page.reload()
  await expect(page.getByTestId('detail-password')).toHaveText('••••••••')
  expect(passwordRequests).toBe(1)
})

test('C-DET-03 运行中续费 1 周：价格、剩余时长、余额与续费订单', async ({ page }) => {
  const u = await seedUser({ balance: 100000 })
  const { api, id } = await runningInstance(u.phone)
  await loginConsole(page, u.phone)
  await page.goto(`/instances/${id}`)
  await page.getByTestId('detail-renew-btn').click()
  await expect(dialog(page)).toBeVisible()
  await page.getByTestId('detail-renew-duration-1w').click()
  await expect(page.getByTestId('detail-renew-price')).toHaveText('¥414.00')
  await expect(page.getByTestId('detail-renew-balance')).toHaveText('¥931.00')
  await expect(page.getByTestId('detail-renew-after')).toHaveText('¥517.00')
  await page.getByTestId('detail-renew-ok').click()
  await toast(page, '续费成功')
  const remaining = Number(await page.getByTestId('detail-remaining').getAttribute('data-remaining'))
  expect(remaining).toBeGreaterThan(7 * 86400 + 23 * 3600)
  expect(remaining).toBeLessThanOrEqual(8 * 86400)
  expect((await (await api.get('/api/v1/wallet')).json()).balance).toBe(100000 - 6900 - 41400)
  const orders = (await (await api.get('/api/v1/orders')).json()).items as { kind: string; instance_id: string; amount: number }[]
  expect(orders.some((o) => o.kind === 'renew' && o.instance_id === id && o.amount === 41400)).toBe(true)
})

test('C-DET-04 停机实例续费后提示重新启动，Agent 完成后运行中', async ({ page }) => {
  const u = await seedUser()
  const { api, id } = await runningInstance(u.phone)
  await hooks.advance(86400)
  await hooks.sweep()
  await hooks.agent()
  expect((await instanceOf(api, id)).state).toBe('stopped')
  await loginConsole(page, u.phone)
  await page.goto(`/instances/${id}`)
  await expect(page.getByTestId('detail-state')).toHaveText('已到期停机')
  await page.getByTestId('detail-renew-btn').click()
  await page.getByTestId('detail-renew-duration-1d').click()
  await page.getByTestId('detail-renew-ok').click()
  await toast(page, '正在重新启动')
  await expect(page.getByTestId('detail-pending')).toHaveText('1 天')
  await hooks.agent()
  await page.reload()
  await expect(page.getByTestId('detail-state')).toHaveText('运行中')
})

test('C-DET-05 续费余额不足时按钮变为去充值', async ({ page }) => {
  const u = await seedUser({ balance: 10000 })
  const { id } = await runningInstance(u.phone) // 扣 ¥69 后余额 ¥31
  await loginConsole(page, u.phone)
  await page.goto(`/instances/${id}`)
  await page.getByTestId('detail-renew-btn').click()
  await page.getByTestId('detail-renew-duration-1d').click()
  await expect(page.getByTestId('detail-renew-after')).toContainText('余额不足')
  await expect(page.getByTestId('detail-renew-ok')).toHaveText('去充值')
  await page.getByTestId('detail-renew-ok').click()
  await expect(page).toHaveURL(/\/wallet\/recharge\?redirect=(%2F|\/)instances(%2F|\/)ins-/)
})

test('C-DET-06 释放确认：取消不发请求，确认后洗机中；双击只发一个请求', async ({ page }) => {
  const u = await seedUser()
  const { id } = await runningInstance(u.phone)
  let releaseRequests = 0
  page.on('request', (r) => {
    if (r.method() === 'POST' && r.url().endsWith(`/api/v1/instances/${id}/release`)) releaseRequests += 1
  })
  await loginConsole(page, u.phone)
  await page.goto(`/instances/${id}`)
  await page.getByTestId('detail-release-btn').click()
  await expect(page.getByTestId('detail-release-text')).toHaveText('释放后数据将被清除，已付费用不退，确定释放吗？')
  await expect(page.getByTestId('detail-release-warning')).toHaveCount(0)
  await expect(page.getByTestId('detail-release-ok')).toHaveText('确定释放')
  await page.getByTestId('detail-release-cancel').click()
  await expect(dialog(page)).toBeHidden()
  expect(releaseRequests).toBe(0)
  await expect(page.getByTestId('detail-state')).toHaveText('运行中')

  await page.getByTestId('detail-release-btn').click()
  await page.getByTestId('detail-release-ok').dblclick() // Review Focus 5：请求进行中按钮 loading 禁用
  await toast(page, '已提交释放')
  await expect(page.getByTestId('detail-state')).toHaveText('洗机中')
  expect(releaseRequests).toBe(1)
  await expect(page.getByTestId('detail-renew-btn')).toHaveCount(0)
  await expect(page.getByTestId('detail-release-btn')).toHaveCount(0)
})

test('C-DET-07 停机且有待开始续费时，释放确认框追加作废提示', async ({ page }) => {
  const u = await seedUser()
  const { api, id } = await runningInstance(u.phone)
  await hooks.advance(86400)
  await hooks.sweep()
  await hooks.agent()
  const renew = await api.post(`/api/v1/instances/${id}/renew`, { data: { duration: '1w' }, headers: { 'Idempotency-Key': crypto.randomUUID() } })
  expect(renew.ok()).toBe(true)
  expect((await instanceOf(api, id)).pending_seconds).toBe(7 * 86400)
  await loginConsole(page, u.phone)
  await page.goto(`/instances/${id}`)
  await page.getByTestId('detail-release-btn').click()
  await expect(page.getByTestId('detail-release-warning')).toHaveText('该实例有已付费但尚未开始计时的 7 天，释放后这部分时长也会作废')
})

test('C-DET-08 开通失败：提示退款、余额恢复、无操作按钮', async ({ page }) => {
  const u = await seedUser({ balance: 100000 })
  const api = await userApi(u.phone)
  await hooks.machines(1)
  const id = await placeOrder(api)
  await hooks.agent({ fail: true })
  await loginConsole(page, u.phone)
  await page.goto(`/instances/${id}`)
  await expect(page.getByTestId('detail-state')).toHaveText('开通失败')
  await expect(page.getByTestId('detail-failed-hint')).toContainText('费用已原路退回余额')
  await expect(page.getByTestId('detail-renew-btn')).toHaveCount(0)
  await expect(page.getByTestId('detail-release-btn')).toHaveCount(0)
  expect((await (await api.get('/api/v1/wallet')).json()).balance).toBe(100000)
})

test('C-DET-09 按钮与状态匹配', async ({ page }) => {
  const u = await seedUser({ balance: 1_000_000 })
  const api = await userApi(u.phone)
  await hooks.machines(6)
  const stopped = await placeOrder(api)
  await hooks.agent()
  await hooks.advance(86400)
  await hooks.sweep()
  await hooks.agent()
  const released = await placeOrder(api)
  await hooks.agent()
  await api.post(`/api/v1/instances/${released}/release`, { data: {} })
  await hooks.agent()
  const failed = await placeOrder(api)
  await hooks.agent({ fail: true })
  const running = await placeOrder(api)
  await hooks.agent()
  const wiping = await placeOrder(api)
  await hooks.agent()
  await api.post(`/api/v1/instances/${wiping}/release`, { data: {} })
  const creating = await placeOrder(api)

  await loginConsole(page, u.phone)
  const matrix: [string, boolean, boolean][] = [
    [creating, true, false], [running, true, true], [stopped, true, true], [wiping, false, false], [released, false, false], [failed, false, false],
  ]
  for (const [id, renew, release] of matrix) {
    await page.goto(`/instances/${id}`)
    await expect(page.getByTestId('detail-id')).toHaveText(id)
    await expect(page.getByTestId('detail-renew-btn')).toHaveCount(renew ? 1 : 0)
    await expect(page.getByTestId('detail-release-btn')).toHaveCount(release ? 1 : 0)
  }
})
```

`web/e2e/console/instances.spec.ts` 末尾追加：

```ts
test('C-INS-03 机器失联后列表与详情显示计时已暂停', async ({ page }, info) => {
  const u = await seedUser()
  const { id } = await runningInstance(u.phone)
  await hooks.advance(360)
  await hooks.sweep({ heartbeat: false })
  await loginConsole(page, u.phone)
  await nav(page, info, 'instances')
  await expect(instanceRow(page, info, id).getByTestId('instance-suspended')).toHaveText('计时已暂停（机器维护中）')
  await page.goto(`/instances/${id}`)
  await expect(page.getByTestId('detail-suspended')).toBeVisible()
  await hooks.sweep() // 心跳恢复，避免影响后续用例
})

test('C-INS-04 运行中倒计时每秒递减；停机显示北京时间的数据保留截止', async ({ page }, info) => {
  const u = await seedUser({ balance: 200000 })
  const { api, id: stoppedId } = await runningInstance(u.phone)
  await hooks.advance(86400)
  await hooks.sweep()
  await hooks.agent()
  await hooks.machines(1)
  const runningId = await placeOrder(api)
  await hooks.agent()
  const stopped = await instanceOf(api, stoppedId)
  expect(stopped.retain_until).not.toBeNull()

  await loginConsole(page, u.phone)
  await nav(page, info, 'instances')
  const runningRow = instanceRow(page, info, runningId)
  // 下单后 loginConsole 已把假时钟推进了 61 秒（发码限流），所以此时是「23 小时 58 分」量级，不是整整 1 天
  await expect(runningRow.getByTestId('instance-time')).toHaveText(/^剩余 23 小时 5\d 分$/)
  const first = Number(await runningRow.getByTestId('instance-time').getAttribute('data-remaining'))
  await expect.poll(async () => Number(await runningRow.getByTestId('instance-time').getAttribute('data-remaining')), { timeout: 5000 }).toBeLessThan(first)
  await expect(instanceRow(page, info, stoppedId).getByTestId('instance-time')).toHaveText(`数据保留至 ${cstTime(stopped.retain_until!)}`)

  await page.goto(`/instances/${runningId}`)
  const d1 = Number(await page.getByTestId('detail-remaining').getAttribute('data-remaining'))
  await expect.poll(async () => Number(await page.getByTestId('detail-remaining').getAttribute('data-remaining')), { timeout: 5000 }).toBeLessThan(d1)
  await page.goto(`/instances/${stoppedId}`)
  await expect(page.getByTestId('detail-retain')).toHaveText(cstTime(stopped.retain_until!))
})
```

（该文件 import 增加 `import { instanceOf, runningInstance } from '../helpers/instances'` 与 `import { cstTime } from '../helpers/time'`。）

`web/e2e/console/nav.spec.ts` 末尾追加：

```ts
test('C-NAV-02 手机：确认框底部弹出不全屏、表单弹窗全屏；电脑：都居中不全屏', async ({ page }, info) => {
  const u = await seedUser()
  const { id } = await runningInstance(u.phone)
  await loginConsole(page, u.phone)
  await page.goto(`/instances/${id}`)
  await page.getByTestId('detail-release-btn').click()
  // Element Plus 的 role="dialog" 在 .el-overlay-dialog 包装层上；sheet / is-fullscreen 类与 data-testid 都在内层 .el-dialog 上，
  // 所以类名与位置断言必须用 AppDialog 的 testid 定位，用 getByRole('dialog') 断言类名会在错误的元素上「通过」
  const dlg = page.getByTestId('detail-release-dialog')
  await expect(dlg).toBeVisible()
  await expect(dlg).toHaveClass(/\bel-dialog\b/) // 确认定位到的确是 .el-dialog 本体
  if (isMobile(info)) {
    await expect(dlg).toHaveClass(/\bsheet\b/)
    await expect(dlg).not.toHaveClass(/is-fullscreen/)
    const box = await dlg.boundingBox()
    const viewport = page.viewportSize()!
    expect(Math.round(box!.y + box!.height)).toBeGreaterThanOrEqual(viewport.height - 1) // 贴底
    expect(Math.round(box!.width)).toBe(viewport.width) // 通栏
    expect(box!.height).toBeLessThan(viewport.height * 0.8) // 不是全屏
  } else {
    await expect(dlg).not.toHaveClass(/\bsheet\b/)
    await expect(dlg).not.toHaveClass(/is-fullscreen/)
    const box = await dlg.boundingBox()
    const viewport = page.viewportSize()!
    expect(Math.abs(box!.x + box!.width / 2 - viewport.width / 2)).toBeLessThan(4) // 水平居中
    expect(Math.round(box!.width)).toBe(420)
  }
  await page.getByTestId('detail-release-cancel').click()
  await expect(dlg).toBeHidden()
  await page.getByTestId('detail-renew-btn').click()
  const form = page.getByTestId('detail-renew-dialog')
  await expect(form).toBeVisible()
  if (isMobile(info)) {
    await expect(form).toHaveClass(/is-fullscreen/)
    const box = await form.boundingBox()
    const viewport = page.viewportSize()!
    expect(Math.round(box!.width)).toBe(viewport.width)
    expect(Math.round(box!.height)).toBe(viewport.height)
  } else {
    await expect(form).not.toHaveClass(/is-fullscreen/)
    expect(Math.round((await form.boundingBox())!.width)).toBe(560)
  }
})
```

（该文件 import 增加 `import { runningInstance } from '../helpers/instances'`。）

Run: `make web && cd web && npx playwright test console/instance-detail.spec.ts console/instances.spec.ts console/nav.spec.ts`
Expected: （9 + 4 + 3）× 2 = 32 passed。

- [ ] **Step 3: 全量验证并提交**

Run: `go vet ./... && go test ./... && cd web && npm test && npm run build && npx playwright test console/`
Expected: 全部 PASS。

```bash
git add web/console web/e2e
git commit -m "feat(console): instance detail with SSH info, on-demand password, renew and release dialogs; e2e C-DET/C-INS/C-NAV-02

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---
### Task 11: 控制台算力市场与下单

**Files:**
- Replace: `web/console/src/views/MarketView.vue`
- Modify: `web/e2e/helpers/admin.ts`（追加 `drainIdleMachines`）
- Create: `web/e2e/console/market.spec.ts`
- Modify: `web/e2e/console/auth.spec.ts`（C-AUTH-07）

**Interfaces:**
- Consumes: Task 8～10 的 `api`、`session`、`refreshBalance`、`AppDialog`、`GuideDialog`、`useGuide`、`usePolling`、`useIdempotencyKey`；Task 7 的 `adminApi` / `adminPost`。
- Produces:
  - 市场 testid：`market-card`（属性 `data-product-id`）、`market-name`、`market-spec-gpu|memory|disk|cpu`、`market-idle`（空闲台数数字）、`market-price-<1d|1w|1m>`（未启用档位不渲染）、`market-duration-<1d|1w|1m>`（单选）、`market-image`（下拉）、`market-rent-btn`（`idle_count === 0` 时禁用且文案「暂无空闲」，否则「立即租用」）；确认框 `market-confirm-dialog`、`market-confirm-product`、`market-confirm-duration`、`market-confirm-image`、`market-confirm-price`、`market-confirm-balance`、`market-confirm-after`、`market-confirm-ok`（余额不足时文案「去充值」，点击直接跳 `/wallet/recharge?redirect=/market`）、`market-confirm-cancel`。
  - 下单请求带 `Idempotency-Key`；网络错误后确认框保持打开，再次点击复用同一个键；成功后 `ElMessage.success('下单成功，正在开通')` 并跳 `/instances/:id`。
  - e2e helper `drainIdleMachines(ctx, admin)`：把当前全部 `idle` 机器置为维护中，使 `idle_count` 归零（数据库在整轮测试里共享，「不登记机器」不等于没有空闲机器）。
  - 覆盖：C-MKT-01～08、C-AUTH-07。

- [ ] **Step 1: 市场页**

`web/console/src/views/MarketView.vue`：

```vue
<script setup lang="ts">
import {
  durationLabel, fmtYuan, isApiError, labelOf, messageOf, useIdempotencyKey, type Duration, type Image, type Instance, type Listed, type Order, type Product,
} from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, onMounted, reactive, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api } from '../api'
import AppDialog from '../components/AppDialog.vue'
import GuideDialog from '../components/GuideDialog.vue'
import { useGuide } from '../composables/useGuide'
import { usePolling } from '../composables/usePolling'
import { refreshBalance, session } from '../store/session'

const route = useRoute()
const router = useRouter()
const guide = useGuide()
const products = ref<Product[]>([])
const images = ref<Image[]>([])
const loading = ref(true)
const loadError = ref('')
// 每个商品卡片内的选择：租期默认第一档、镜像默认第一个
const sel = reactive<Record<number, { duration: Duration; imageId: number | null }>>({})

async function load() {
  try {
    const [p, i] = await Promise.all([api.get<Listed<Product>>('/products'), api.get<Listed<Image>>('/images')])
    products.value = p.items
    images.value = i.items
    for (const prod of p.items) {
      const cur = sel[prod.id]
      if (!cur) sel[prod.id] = { duration: prod.prices[0]?.duration ?? '1d', imageId: i.items[0]?.id ?? null }
      else if (!prod.prices.some((x) => x.duration === cur.duration)) cur.duration = prod.prices[0]?.duration ?? '1d' // 档位被下架
    }
    loadError.value = ''
  } catch (e) {
    loadError.value = messageOf(e)
  } finally {
    loading.value = false
  }
}
onMounted(load)
usePolling(load, 30_000) // 30 秒刷新一次空闲台数（页面可见时）

const specItems = (p: Product) => [
  { key: 'gpu', label: 'GPU', value: p.spec.gpu },
  { key: 'memory', label: '内存', value: `${p.spec.memory_gb} GB` },
  { key: 'disk', label: '硬盘', value: `${p.spec.disk_tb} TB` },
  { key: 'cpu', label: 'CPU', value: p.spec.cpu },
]

// ---- 确认框 ----
const confirm = reactive<{ visible: boolean; product: Product | null }>({ visible: false, product: null })
const confirmSel = computed(() => (confirm.product ? sel[confirm.product.id] : null))
const price = computed(() => confirm.product?.prices.find((p) => p.duration === confirmSel.value?.duration)?.amount ?? null)
const imageName = computed(() => images.value.find((i) => i.id === confirmSel.value?.imageId)?.name ?? '')
const balance = computed(() => session.balance ?? 0)
const after = computed(() => (price.value == null ? null : balance.value - price.value))
const insufficient = computed(() => price.value != null && balance.value < price.value)
const submitting = ref(false)
const key = useIdempotencyKey(() => [confirm.product?.id, confirmSel.value?.duration, confirmSel.value?.imageId])

async function rent(p: Product) {
  if (session.status !== 'authed') {
    await router.push({ path: '/login', query: { redirect: route.fullPath } })
    return
  }
  if (session.user?.kyc_status !== 'verified') {
    guide.kycRequired() // 不发下单请求
    return
  }
  try {
    await refreshBalance()
  } catch (e) {
    guide.handle(e)
    return
  }
  confirm.product = p
  confirm.visible = true
}

async function submit() {
  if (!confirm.product || !confirmSel.value) return
  if (insufficient.value) {
    confirm.visible = false
    await router.push({ path: '/wallet/recharge', query: { redirect: route.fullPath } })
    return
  }
  submitting.value = true
  try {
    const out = await api.post<{ order: Order; instance: Instance }>(
      '/orders',
      { product_id: confirm.product.id, duration: confirmSel.value.duration, image_id: confirmSel.value.imageId },
      { idempotencyKey: key.current() },
    )
    key.rotate()
    confirm.visible = false
    ElMessage.success('下单成功，正在开通')
    await router.push(`/instances/${out.instance.id}`)
  } catch (e) {
    // 需要引导的错误关闭确认框；网络错误等保持打开，用户再次点击时复用同一个幂等键
    if (isApiError(e) && ['kyc_required', 'insufficient_balance', 'no_capacity', 'banned'].includes(e.code)) confirm.visible = false
    guide.handle(e, { onNoCapacity: load })
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <div class="market">
    <el-alert v-if="loadError" type="error" :title="loadError" show-icon :closable="false" />
    <el-skeleton v-else-if="loading" :rows="5" animated />
    <div v-else class="cards">
      <el-card v-for="p in products" :key="p.id" class="card" data-testid="market-card" :data-product-id="p.id">
        <template #header>
          <div class="head">
            <span class="name" data-testid="market-name">{{ p.name }}</span>
            <span class="idle">空闲 <b data-testid="market-idle">{{ p.idle_count }}</b> 台</span>
          </div>
        </template>
        <ul class="spec">
          <li v-for="s in specItems(p)" :key="s.key"><span class="label">{{ s.label }}</span><span :data-testid="`market-spec-${s.key}`">{{ s.value }}</span></li>
        </ul>
        <div class="prices">
          <span v-for="pr in p.prices" :key="pr.duration" :data-testid="`market-price-${pr.duration}`">{{ labelOf(durationLabel, pr.duration) }} {{ fmtYuan(pr.amount) }}</span>
        </div>
        <el-form label-position="top" class="form">
          <el-form-item label="租期">
            <el-radio-group v-model="sel[p.id].duration">
              <el-radio-button v-for="pr in p.prices" :key="pr.duration" :value="pr.duration" :data-testid="`market-duration-${pr.duration}`">{{ labelOf(durationLabel, pr.duration) }}</el-radio-button>
            </el-radio-group>
          </el-form-item>
          <el-form-item label="镜像">
            <el-select v-model="sel[p.id].imageId" data-testid="market-image" placeholder="选择镜像">
              <el-option v-for="img in images" :key="img.id" :label="img.name" :value="img.id" />
            </el-select>
          </el-form-item>
          <el-button type="primary" class="rent" data-testid="market-rent-btn" :disabled="p.idle_count === 0" @click="rent(p)">
            {{ p.idle_count === 0 ? '暂无空闲' : '立即租用' }}
          </el-button>
        </el-form>
      </el-card>
    </div>

    <AppDialog v-model="confirm.visible" kind="confirm" title="确认下单" data-testid="market-confirm-dialog">
      <el-descriptions v-if="confirm.product && confirmSel" :column="1" border>
        <el-descriptions-item label="商品"><span data-testid="market-confirm-product">{{ confirm.product.name }}</span></el-descriptions-item>
        <el-descriptions-item label="租期"><span data-testid="market-confirm-duration">{{ labelOf(durationLabel, confirmSel.duration) }}</span></el-descriptions-item>
        <el-descriptions-item label="镜像"><span data-testid="market-confirm-image">{{ imageName }}</span></el-descriptions-item>
        <el-descriptions-item label="价格"><span data-testid="market-confirm-price">{{ price == null ? '—' : fmtYuan(price) }}</span></el-descriptions-item>
        <el-descriptions-item label="当前余额"><span data-testid="market-confirm-balance">{{ fmtYuan(balance) }}</span></el-descriptions-item>
        <el-descriptions-item label="扣款后余额">
          <span data-testid="market-confirm-after" :class="{ warn: insufficient }">{{ after == null ? '—' : fmtYuan(after) }}</span>
          <span v-if="insufficient" class="warn">（余额不足）</span>
        </el-descriptions-item>
      </el-descriptions>
      <template #footer>
        <el-button data-testid="market-confirm-cancel" @click="confirm.visible = false">取消</el-button>
        <el-button type="primary" :loading="submitting" data-testid="market-confirm-ok" @click="submit">{{ insufficient ? '去充值' : '确认下单' }}</el-button>
      </template>
    </AppDialog>
    <GuideDialog :guide="guide" />
  </div>
</template>

<style scoped>
.cards { display: grid; gap: 16px; grid-template-columns: repeat(auto-fill, minmax(320px, 1fr)); }
.head { display: flex; justify-content: space-between; align-items: center; }
.name { font-weight: 600; }
.idle { color: #909399; font-size: 13px; }
.spec { list-style: none; padding: 0; margin: 0 0 12px; display: grid; gap: 4px; }
.spec .label { display: inline-block; width: 48px; color: #909399; }
.prices { display: flex; flex-wrap: wrap; gap: 12px; margin-bottom: 12px; font-weight: 600; }
.rent { width: 100%; }
.warn { color: var(--el-color-warning); }
</style>
```

Run: `cd web && npm run build`
Expected: 通过。

- [ ] **Step 2: helper 与端到端用例**

`web/e2e/helpers/admin.ts` 末尾追加：

```ts
/** 把当前全部空闲机器置为维护中：数据库整轮共享，「没有空闲机器」只能这样构造。 */
export async function drainIdleMachines(ctx: APIRequestContext, admin: SeededAdmin) {
  for (let round = 0; round < 10; round++) {
    const r = await ctx.get('/admin/api/v1/machines?state=idle&page_size=100')
    const { items } = (await r.json()) as { items: { id: number }[] }
    if (items.length === 0) return
    for (const m of items) await adminPost(ctx, admin, `/admin/api/v1/machines/${m.id}/maintenance`, { on: true })
  }
}
```

`web/e2e/console/market.spec.ts`：

```ts
import { expect, request, test } from '@playwright/test'
import { adminApi, createAdmin, drainIdleMachines } from '../helpers/admin'
import { dialog, loginConsole, nav, seedUser, toast, userApi } from '../helpers/console'
import { CONSOLE_URL, hooks } from '../helpers/hooks'

const card = (page: import('@playwright/test').Page) => page.getByTestId('market-card').first()

test('C-MKT-01 市场未登录可见：规格、空闲台数、三档价格', async ({ page }) => {
  await hooks.machines(1)
  const api = await request.newContext({ baseURL: CONSOLE_URL })
  const product = ((await (await api.get('/api/v1/products')).json()) as { items: { idle_count: number }[] }).items[0]
  await page.goto('/market')
  await expect(card(page).getByTestId('market-name')).toHaveText('DGX Spark 整机')
  await expect(card(page).getByTestId('market-spec-gpu')).toHaveText('NVIDIA GB10 Grace Blackwell')
  await expect(card(page).getByTestId('market-spec-memory')).toHaveText('128 GB')
  await expect(card(page).getByTestId('market-spec-disk')).toHaveText('4 TB')
  await expect(card(page).getByTestId('market-spec-cpu')).toHaveText('20 核 Arm')
  await expect(card(page).getByTestId('market-idle')).toHaveText(String(product.idle_count))
  await expect(card(page).getByTestId('market-price-1d')).toHaveText('1 天 ¥69.00')
  await expect(card(page).getByTestId('market-price-1w')).toHaveText('1 周 ¥414.00')
  await expect(card(page).getByTestId('market-price-1m')).toHaveText('1 月 ¥1,599.00')
  await expect(card(page).getByTestId('market-rent-btn')).toHaveText('立即租用')
})

test('C-MKT-02 未登录点立即租用跳登录，登录后回到市场', async ({ page }) => {
  await hooks.machines(1)
  const u = await seedUser()
  await page.goto('/market')
  await card(page).getByTestId('market-rent-btn').click()
  await expect(page).toHaveURL(/\/login\?redirect=(%2F|\/)market$/)
  await hooks.advance(61)
  await page.getByTestId('login-phone').fill(u.phone)
  await page.getByTestId('login-send').click()
  await page.getByTestId('login-code').fill('000000')
  await page.getByTestId('login-submit').click()
  await expect(page).toHaveURL(/\/market$/)
})

test('C-MKT-03 未实名用户点租用引导去实名', async ({ page }, info) => {
  await hooks.machines(1)
  const u = await seedUser({ verified: false })
  let orderRequests = 0
  page.on('request', (r) => {
    if (r.method() === 'POST' && r.url().endsWith('/api/v1/orders')) orderRequests += 1
  })
  await loginConsole(page, u.phone)
  await nav(page, info, 'market')
  await card(page).getByTestId('market-rent-btn').click()
  await expect(page.getByRole('dialog')).toContainText('请先完成实名认证')
  await page.getByTestId('guide-action').click()
  await expect(page).toHaveURL(/\/kyc\?redirect=(%2F|\/)market$/)
  expect(orderRequests).toBe(0)
})

test('C-MKT-04 余额不足：确认框提示并改为去充值', async ({ page }, info) => {
  await hooks.machines(1)
  const u = await seedUser({ balance: 1000 })
  await loginConsole(page, u.phone)
  await nav(page, info, 'market')
  await card(page).getByTestId('market-rent-btn').click()
  await expect(page.getByTestId('market-confirm-balance')).toHaveText('¥10.00')
  await expect(page.getByTestId('market-confirm-after')).toContainText('余额不足')
  await expect(page.getByTestId('market-confirm-ok')).toHaveText('去充值')
  await page.getByTestId('market-confirm-ok').click()
  await expect(page).toHaveURL(/\/wallet\/recharge\?redirect=(%2F|\/)market$/)
})

test('C-MKT-05 下单成功跳转实例详情，余额减少', async ({ page }, info) => {
  await hooks.machines(1)
  const u = await seedUser({ balance: 100000 })
  await loginConsole(page, u.phone)
  await nav(page, info, 'market')
  await card(page).getByTestId('market-rent-btn').click()
  await expect(page.getByTestId('market-confirm-price')).toHaveText('¥69.00')
  await expect(page.getByTestId('market-confirm-balance')).toHaveText('¥1,000.00')
  await expect(page.getByTestId('market-confirm-after')).toHaveText('¥931.00')
  await page.getByTestId('market-confirm-ok').click()
  await toast(page, '下单成功')
  await expect(page).toHaveURL(/\/instances\/ins-[a-z0-9]+$/)
  await expect(page.getByTestId('detail-state')).toHaveText('开通中')
  const api = await userApi(u.phone)
  expect((await (await api.get('/api/v1/wallet')).json()).balance).toBe(93100)
})

test('C-MKT-06 无空闲机器时按钮禁用，30 秒刷新后恢复', async ({ page }) => {
  const admin = await createAdmin('ops')
  await drainIdleMachines(await adminApi(admin), admin)
  await page.clock.install()
  await page.goto('/market')
  await expect(card(page).getByTestId('market-rent-btn')).toBeDisabled()
  await expect(card(page).getByTestId('market-rent-btn')).toHaveText('暂无空闲')
  await expect(card(page).getByTestId('market-idle')).toHaveText('0')
  await hooks.machines(1)
  await page.clock.runFor(30_000)
  await expect(card(page).getByTestId('market-idle')).toHaveText('1')
  await expect(card(page).getByTestId('market-rent-btn')).toBeEnabled()
  await expect(card(page).getByTestId('market-rent-btn')).toHaveText('立即租用')
})

test('C-MKT-07 幂等：响应丢失后重试复用同一个键，只扣一次款', async ({ page }, info) => {
  await hooks.machines(2)
  const u = await seedUser({ balance: 100000 })
  const keys: string[] = []
  let dropped = false
  await page.route('**/api/v1/orders', async (route) => {
    if (route.request().method() !== 'POST') return route.continue()
    keys.push(route.request().headers()['idempotency-key'])
    if (!dropped) {
      dropped = true
      await route.fetch() // 请求真实到达后端（订单已创建），再让浏览器看到网络错误
      return route.abort('failed')
    }
    return route.continue()
  })
  await loginConsole(page, u.phone)
  await nav(page, info, 'market')
  await card(page).getByTestId('market-rent-btn').click()
  await page.getByTestId('market-confirm-ok').click()
  await toast(page, '网络异常')
  await expect(dialog(page)).toBeVisible()
  await page.getByTestId('market-confirm-ok').click()
  await toast(page, '下单成功')
  await expect(page).toHaveURL(/\/instances\/ins-/)
  expect(keys).toHaveLength(2)
  expect(keys[0]).toBe(keys[1])
  expect(keys[0]).toMatch(/^[0-9a-f-]{36}$/)
  const api = await userApi(u.phone)
  expect(((await (await api.get('/api/v1/instances')).json()) as { items: unknown[] }).items).toHaveLength(1)
  expect((await (await api.get('/api/v1/wallet')).json()).balance).toBe(93100)
})

test('C-MKT-08 选择 1 周与第二个镜像，确认框与详情一致', async ({ page }, info) => {
  await hooks.machines(1)
  const u = await seedUser({ balance: 100000 })
  await loginConsole(page, u.phone)
  await nav(page, info, 'market')
  await card(page).getByTestId('market-duration-1w').click()
  await card(page).getByTestId('market-image').click()
  await page.getByRole('option', { name: 'PyTorch' }).click()
  await card(page).getByTestId('market-rent-btn').click()
  await expect(page.getByTestId('market-confirm-duration')).toHaveText('1 周')
  await expect(page.getByTestId('market-confirm-price')).toHaveText('¥414.00')
  await expect(page.getByTestId('market-confirm-image')).toHaveText('PyTorch')
  await page.getByTestId('market-confirm-ok').click()
  await expect(page).toHaveURL(/\/instances\/ins-/)
  await expect(page.getByTestId('detail-image')).toHaveText('PyTorch')
  await expect(page.getByTestId('detail-pending')).toHaveText('7 天')
})
```

`web/e2e/console/auth.spec.ts` 末尾追加：

```ts
test('C-AUTH-07 封禁：已登录用户下一次请求跳登录；封禁用户登录提示账号已被封禁', async ({ page }, info) => {
  const u = await seedUser()
  await loginConsole(page, u.phone)
  const admin = await createAdmin('ops')
  await adminPost(await adminApi(admin), admin, `/admin/api/v1/users/${u.id}/ban`, { reason: 'e2e 测试封禁' })
  await nav(page, info, 'instances') // 后端已删除会话 → 401 → 跳登录并提示
  await expect(page).toHaveURL(/\/login\?redirect=(%2F|\/)instances$/)
  await toast(page, '请先登录')
  await hooks.advance(61)
  await page.getByTestId('login-phone').fill(u.phone)
  await page.getByTestId('login-send').click()
  await page.getByTestId('login-code').fill('000000')
  await page.getByTestId('login-submit').click()
  await toast(page, '账号已被封禁')
  await expect(page).toHaveURL(/\/login/)
})
```

（该文件 import 增加 `import { adminApi, adminPost, createAdmin } from '../helpers/admin'` 与 `toast`。）

Run: `make web && cd web && npx playwright test console/market.spec.ts console/auth.spec.ts`
Expected: （8 + 6）× 2 = 28 passed。

- [ ] **Step 3: 全量验证并提交**

Run: `go vet ./... && go test ./... && cd web && npm test && npm run build && npx playwright test console/`
Expected: 全部 PASS。

```bash
git add web/console web/e2e
git commit -m "feat(console): market cards with duration/image selection, order confirm with idempotency key; e2e C-MKT, C-AUTH-07

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---
### Task 12: 控制台钱包（余额、流水、订单、充值记录）

**Files:**
- Replace: `web/console/src/views/WalletView.vue`
- Create: `web/e2e/console/wallet.spec.ts`
- Modify: `web/e2e/console/auth.spec.ts`（C-AUTH-06）

**Interfaces:**
- Consumes: Task 8～10 的 `api`、`session`、`useGuide` / `GuideDialog`、`useMobile`；shared 的 `fmtYuan`、`fmtTime`、`labels`。
- Produces:
  - 钱包 testid：`wallet-balance`、`wallet-recharge-btn`（未实名 → 引导实名）、`wallet-refresh`、`wallet-error`；标签页用 `el-tabs`（`role="tab"`，名称「流水」「订单」「充值记录」），选择写在 `?tab=ledger|orders|recharges`；流水：`ledger-row`、`ledger-kind`、`ledger-amount`（正数 `+¥100.00` 带类 `money-plus`，负数 `-¥69.00` 带类 `money-minus`）、`ledger-after`、`ledger-remark`、`ledger-time`、`ledger-sentinel`（`IntersectionObserver` 哨兵，每页 20，游标 `before_id`）、`ledger-end`；订单：`order-row`、`order-kind`、`order-instance`（链接 `/instances/<id>`）、`order-duration`、`order-amount`、`order-status`；充值记录：`recharge-row`、`recharge-channel`、`recharge-amount`、`recharge-status`、`recharge-time`、`recharge-continue`（待支付时链接到 `/wallet/recharge/<id>`）、`wallet-recharges-pager`（每页 10）。
  - 余额请求失败：`notifyError(err)`（Task 8；401 静默，避免与 `handleUnauthorized` 的「请先登录」叠成两条）+ 页内 `wallet-error`。
  - 覆盖：C-WAL-01～04、C-AUTH-06、C-ERR-01、C-ERR-02。

- [ ] **Step 1: 钱包页**

`web/console/src/views/WalletView.vue`：

```vue
<script setup lang="ts">
import {
  durationLabel, fmtTime, fmtYuan, labelOf, ledgerKindLabel, messageOf, orderKindLabel, orderStatusLabel, rechargeChannelLabel, rechargeStatusLabel, useMobile,
  type LedgerRow, type Listed, type Order, type Paged, type Recharge,
} from '@spark/shared'
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api } from '../api'
import GuideDialog from '../components/GuideDialog.vue'
import { useGuide } from '../composables/useGuide'
import { notifyError } from '../logic/notify'
import { session } from '../store/session'

const route = useRoute()
const router = useRouter()
const guide = useGuide()
const mobile = useMobile()
const tabs = ['ledger', 'orders', 'recharges'] as const
type Tab = (typeof tabs)[number]
const tab = computed<Tab>({
  get: () => (tabs.includes(route.query.tab as Tab) ? (route.query.tab as Tab) : 'ledger'),
  set: (v) => void router.replace({ query: { ...route.query, tab: v } }),
})

// ---- 余额 ----
const balance = ref<number | null>(null)
const balanceError = ref('')
async function loadBalance() {
  try {
    balance.value = (await api.get<{ balance: number }>('/wallet')).balance
    session.balance = balance.value
    balanceError.value = ''
  } catch (e) {
    balanceError.value = messageOf(e)
    notifyError(e) // 401 时静默：handleUnauthorized 已提示「请先登录」并跳转（C-AUTH-06 要求只有一条提示）
  }
}
function goRecharge() {
  if (session.user?.kyc_status !== 'verified') return guide.kycRequired() // 后端创建充值单要求已实名
  void router.push('/wallet/recharge')
}

// ---- 流水：游标分页 + 滚动到底自动加载 ----
const PAGE = 20
const ledger = ref<LedgerRow[]>([])
const ledgerHasMore = ref(true)
const ledgerLoading = ref(false)
const sentinel = ref<HTMLElement | null>(null)
async function loadLedger(reset = false) {
  if (ledgerLoading.value) return
  if (reset) {
    ledger.value = []
    ledgerHasMore.value = true
  }
  if (!ledgerHasMore.value) return
  ledgerLoading.value = true
  try {
    const before = ledger.value.length ? ledger.value[ledger.value.length - 1].id : undefined
    const { items } = await api.get<Listed<LedgerRow>>('/wallet/ledger', { before_id: before, limit: PAGE })
    ledger.value.push(...items)
    ledgerHasMore.value = items.length === PAGE
  } catch (e) {
    notifyError(e)
  } finally {
    ledgerLoading.value = false
  }
}
let io: IntersectionObserver | undefined
onMounted(() => {
  io = new IntersectionObserver((entries) => {
    if (entries.some((en) => en.isIntersecting)) void loadLedger()
  })
})
watch(sentinel, (el, old) => {
  if (old) io?.unobserve(old)
  if (el) io?.observe(el)
})
onUnmounted(() => io?.disconnect())

// ---- 订单 ----
const orders = ref<Order[]>([])
async function loadOrders() {
  try {
    orders.value = (await api.get<Listed<Order>>('/orders')).items
  } catch (e) {
    notifyError(e)
  }
}

// ---- 充值记录 ----
const recharges = ref<Recharge[]>([])
const rechargeTotal = ref(0)
const rechargePage = ref(1)
async function loadRecharges() {
  try {
    const out = await api.get<Paged<Recharge>>('/recharges', { page: rechargePage.value, page_size: 10 })
    recharges.value = out.items
    rechargeTotal.value = out.total
  } catch (e) {
    notifyError(e)
  }
}
watch(rechargePage, loadRecharges)

const loaded = new Set<Tab>()
watch(
  tab,
  (t) => {
    if (loaded.has(t)) return
    loaded.add(t)
    if (t === 'ledger') void loadLedger(true)
    if (t === 'orders') void loadOrders()
    if (t === 'recharges') void loadRecharges()
  },
  { immediate: true },
)

async function refresh() {
  loaded.clear()
  await loadBalance()
  loaded.add(tab.value)
  if (tab.value === 'ledger') await loadLedger(true)
  if (tab.value === 'orders') await loadOrders()
  if (tab.value === 'recharges') await loadRecharges()
}
onMounted(loadBalance)

const signed = (n: number) => (n >= 0 ? `+${fmtYuan(n)}` : fmtYuan(n))
</script>

<template>
  <div class="wallet">
    <el-card class="balance-card">
      <div class="balance-row">
        <div>
          <div class="muted">账户余额</div>
          <div class="balance" data-testid="wallet-balance">{{ balance == null ? '—' : fmtYuan(balance) }}</div>
          <el-alert v-if="balanceError" type="error" :title="balanceError" :closable="false" show-icon data-testid="wallet-error" />
        </div>
        <div class="balance-actions">
          <el-button data-testid="wallet-refresh" @click="refresh">刷新</el-button>
          <el-button type="primary" data-testid="wallet-recharge-btn" @click="goRecharge">充值</el-button>
        </div>
      </div>
    </el-card>

    <el-card>
      <el-tabs v-model="tab">
        <el-tab-pane label="流水" name="ledger">
          <template v-if="mobile">
            <div v-for="r in ledger" :key="r.id" class="row-card" data-testid="ledger-row">
              <div class="row-head">
                <span data-testid="ledger-kind">{{ labelOf(ledgerKindLabel, r.kind) }}</span>
                <b :class="r.amount >= 0 ? 'money-plus' : 'money-minus'" data-testid="ledger-amount">{{ signed(r.amount) }}</b>
              </div>
              <div class="muted">余额 <span data-testid="ledger-after">{{ fmtYuan(r.balance_after) }}</span> · <span data-testid="ledger-time">{{ fmtTime(r.created_at) }}</span></div>
              <div v-if="r.remark" class="muted" data-testid="ledger-remark">{{ r.remark }}</div>
            </div>
          </template>
          <el-table v-else :data="ledger" row-key="id">
            <el-table-column label="类型" width="100"><template #default="{ row }"><span data-testid="ledger-kind">{{ labelOf(ledgerKindLabel, row.kind) }}</span></template></el-table-column>
            <el-table-column label="金额" width="140"><template #default="{ row }"><b :class="row.amount >= 0 ? 'money-plus' : 'money-minus'" data-testid="ledger-amount">{{ signed(row.amount) }}</b></template></el-table-column>
            <el-table-column label="变动后余额" width="140"><template #default="{ row }"><span data-testid="ledger-after">{{ fmtYuan(row.balance_after) }}</span></template></el-table-column>
            <el-table-column label="备注" min-width="160"><template #default="{ row }"><span data-testid="ledger-remark">{{ row.remark }}</span></template></el-table-column>
            <el-table-column label="时间" width="160"><template #default="{ row }"><span data-testid="ledger-time">{{ fmtTime(row.created_at) }}</span></template></el-table-column>
          </el-table>
          <div v-if="ledgerHasMore" ref="sentinel" class="sentinel" data-testid="ledger-sentinel">{{ ledgerLoading ? '加载中…' : '' }}</div>
          <div v-else-if="ledger.length" class="muted end" data-testid="ledger-end">没有更多了</div>
          <el-empty v-else description="暂无流水" />
        </el-tab-pane>

        <el-tab-pane label="订单" name="orders">
          <div v-for="o in orders" :key="o.id" class="row-card" data-testid="order-row">
            <div class="row-head">
              <span><span data-testid="order-kind">{{ labelOf(orderKindLabel, o.kind) }}</span> · <span data-testid="order-duration">{{ labelOf(durationLabel, o.duration) }}</span></span>
              <b data-testid="order-amount">{{ fmtYuan(o.amount) }}</b>
            </div>
            <div class="muted">
              <router-link :to="`/instances/${o.instance_id}`" class="mono" data-testid="order-instance">{{ o.instance_id }}</router-link>
              · <span data-testid="order-status">{{ labelOf(orderStatusLabel, o.status) }}</span> · {{ fmtTime(o.created_at) }}
            </div>
          </div>
          <el-empty v-if="orders.length === 0" description="暂无订单" />
        </el-tab-pane>

        <el-tab-pane label="充值记录" name="recharges">
          <div v-for="r in recharges" :key="r.id" class="row-card" data-testid="recharge-row">
            <div class="row-head">
              <span><span data-testid="recharge-channel">{{ labelOf(rechargeChannelLabel, r.channel) }}</span> · <span data-testid="recharge-status">{{ labelOf(rechargeStatusLabel, r.status) }}</span></span>
              <b data-testid="recharge-amount">{{ fmtYuan(r.amount) }}</b>
            </div>
            <div class="muted">
              <span data-testid="recharge-time">{{ fmtTime(r.created_at) }}</span>
              <router-link v-if="r.status === 'pending'" :to="`/wallet/recharge/${r.id}`" data-testid="recharge-continue">继续支付</router-link>
            </div>
          </div>
          <el-empty v-if="recharges.length === 0" description="暂无充值记录" />
          <el-pagination v-if="rechargeTotal > 10" v-model:current-page="rechargePage" :page-size="10" :total="rechargeTotal" layout="prev, pager, next" data-testid="wallet-recharges-pager" />
        </el-tab-pane>
      </el-tabs>
    </el-card>
    <GuideDialog :guide="guide" />
  </div>
</template>

<style scoped>
.wallet { display: grid; gap: 16px; max-width: 900px; }
.balance-row { display: flex; justify-content: space-between; align-items: center; gap: 12px; flex-wrap: wrap; }
.balance { font-size: 28px; font-weight: 600; }
.balance-actions { display: flex; gap: 8px; }
.muted { color: #909399; font-size: 13px; }
.row-card { padding: 12px 0; border-bottom: 1px solid #ebeef5; }
.row-head { display: flex; justify-content: space-between; align-items: center; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.sentinel { height: 24px; text-align: center; color: #909399; font-size: 12px; }
.end { text-align: center; padding: 12px; }
</style>
```

Run: `cd web && npm run build`
Expected: 通过。

- [ ] **Step 2: 端到端用例**

`web/e2e/console/wallet.spec.ts`：

```ts
import { expect, test } from '@playwright/test'
import { loginConsole, nav, placeOrder, seedUser, toast, userApi } from '../helpers/console'
import { hooks } from '../helpers/hooks'

test('C-WAL-01 余额与流水：类型中文、正负着色、变动后余额、滚动加载第二页', async ({ page }, info) => {
  const u = await seedUser({ balance: 106900, ledger_rows: 25 }) // 25 条 +¥0.01、1 条 +¥1,068.75，再下单 -¥69.00 → 27 条、余额 ¥1,000.00
  const api = await userApi(u.phone)
  await hooks.machines(1)
  await placeOrder(api)
  await loginConsole(page, u.phone)
  await nav(page, info, 'wallet')
  await expect(page.getByTestId('wallet-balance')).toHaveText('¥1,000.00')
  await expect(page.getByTestId('ledger-amount')).toHaveCount(20)
  const first = page.getByTestId('ledger-amount').first()
  await expect(first).toHaveText('-¥69.00')
  await expect(first).toHaveClass(/money-minus/)
  await expect(page.getByTestId('ledger-kind').first()).toHaveText('扣款')
  await expect(page.getByTestId('ledger-after').first()).toHaveText('¥1,000.00')
  await expect(page.getByTestId('ledger-amount').nth(1)).toHaveText('+¥1,068.75')
  await expect(page.getByTestId('ledger-amount').nth(1)).toHaveClass(/money-plus/)
  await expect(page.getByTestId('ledger-kind').nth(1)).toHaveText('调账')
  await page.getByTestId('ledger-sentinel').scrollIntoViewIfNeeded()
  await expect(page.getByTestId('ledger-amount')).toHaveCount(27)
  await expect(page.getByTestId('ledger-end')).toBeVisible()
})

test('C-WAL-02 订单 tab：新购 / 续费、租期、金额、状态，实例 ID 可点击', async ({ page }, info) => {
  const u = await seedUser({ balance: 100000 })
  const api = await userApi(u.phone)
  await hooks.machines(1)
  const id = await placeOrder(api)
  await hooks.agent()
  await api.post(`/api/v1/instances/${id}/renew`, { data: { duration: '1w' }, headers: { 'Idempotency-Key': crypto.randomUUID() } })
  await loginConsole(page, u.phone)
  await nav(page, info, 'wallet')
  await page.getByRole('tab', { name: '订单' }).click()
  await expect(page).toHaveURL(/tab=orders/)
  const rows = page.getByTestId('order-row')
  await expect(rows).toHaveCount(2)
  await expect(rows.nth(0).getByTestId('order-kind')).toHaveText('续费')
  await expect(rows.nth(0).getByTestId('order-duration')).toHaveText('1 周')
  await expect(rows.nth(0).getByTestId('order-amount')).toHaveText('¥414.00')
  await expect(rows.nth(0).getByTestId('order-status')).toHaveText('已支付')
  await expect(rows.nth(1).getByTestId('order-kind')).toHaveText('新购')
  await expect(rows.nth(1).getByTestId('order-amount')).toHaveText('¥69.00')
  await rows.nth(1).getByTestId('order-instance').click()
  await expect(page).toHaveURL(new RegExp(`/instances/${id}$`))
})

test('C-WAL-03 充值记录：三种状态、待支付可继续、?tab=recharges 刷新保持', async ({ page }, info) => {
  const u = await seedUser()
  const api = await userApi(u.phone)
  const create = async () => ((await (await api.post('/api/v1/recharges', { data: { amount: 5000, channel: 'alipay' } })).json()) as { id: string }).id
  const paid = await create()
  await api.post(`/api/v1/dev/recharges/${paid}/pay`, { data: {} })
  const closed = await create()
  await hooks.advance(7200)
  await hooks.sweep()
  const pending = await create()
  await loginConsole(page, u.phone)
  await nav(page, info, 'wallet')
  await page.getByRole('tab', { name: '充值记录' }).click()
  await expect(page).toHaveURL(/tab=recharges/)
  const rows = page.getByTestId('recharge-row')
  await expect(rows).toHaveCount(3)
  await expect(rows.nth(0).getByTestId('recharge-status')).toHaveText('待支付')
  await expect(rows.nth(0).getByTestId('recharge-channel')).toHaveText('支付宝')
  await expect(rows.nth(0).getByTestId('recharge-amount')).toHaveText('¥50.00')
  await expect(rows.nth(1).getByTestId('recharge-status')).toHaveText('已关闭')
  await expect(rows.nth(2).getByTestId('recharge-status')).toHaveText('已支付')
  await expect(rows.nth(1).getByTestId('recharge-continue')).toHaveCount(0)
  await page.reload()
  await expect(page.getByRole('tab', { name: '充值记录' })).toHaveAttribute('aria-selected', 'true')
  await page.getByTestId('recharge-row').first().getByTestId('recharge-continue').click()
  await expect(page).toHaveURL(new RegExp(`/wallet/recharge/${pending}$`))
  void closed
})

test('C-WAL-04 未实名用户点充值引导实名', async ({ page }, info) => {
  const u = await seedUser({ verified: false, balance: 0 })
  await loginConsole(page, u.phone)
  await nav(page, info, 'wallet')
  await page.getByTestId('wallet-recharge-btn').click()
  await expect(page.getByRole('dialog')).toContainText('请先完成实名认证')
  await page.getByTestId('guide-action').click()
  await expect(page).toHaveURL(/\/kyc\?redirect=(%2F|\/)wallet/)
})

test('C-ERR-01 服务器错误提示含请求号', async ({ page }, info) => {
  const u = await seedUser()
  await loginConsole(page, u.phone)
  await page.route('**/api/v1/wallet', (route) =>
    route.fulfill({ status: 500, contentType: 'application/json', body: JSON.stringify({ code: 'internal', message: '服务器内部错误，请稍后重试', request_id: 'req-e2e-500' }) }),
  )
  await nav(page, info, 'wallet')
  await toast(page, '服务器内部错误，请稍后重试（请求号 req-e2e-500）')
  await expect(page.getByTestId('wallet-error')).toContainText('请求号 req-e2e-500')
})

test('C-ERR-02 网络错误提示', async ({ page }, info) => {
  const u = await seedUser()
  await loginConsole(page, u.phone)
  await page.route('**/api/v1/wallet', (route) => route.abort('failed'))
  await nav(page, info, 'wallet')
  await toast(page, '网络异常，请稍后重试')
})
```

`web/e2e/console/auth.spec.ts` 末尾追加：

```ts
test('C-AUTH-06 会话失效：清 Cookie 后在钱包页刷新 → 跳登录并提示', async ({ page, context }, info) => {
  const u = await seedUser()
  await loginConsole(page, u.phone)
  await nav(page, info, 'wallet')
  await expect(page.getByTestId('wallet-balance')).toHaveText('¥1,000.00')
  await context.clearCookies()
  await page.getByTestId('wallet-refresh').click()
  await expect(page).toHaveURL(/\/login\?redirect=(%2F|\/)wallet/)
  await toast(page, '请先登录')
  await expect(page.getByRole('alert').filter({ hasText: '请先登录' })).toHaveCount(1) // 多个 401 只提示一次
})
```

Run: `make web && cd web && npx playwright test console/wallet.spec.ts console/auth.spec.ts`
Expected: （6 + 7）× 2 = 26 passed。

- [ ] **Step 3: 全量验证并提交**

Run: `go vet ./... && go test ./... && cd web && npm test && npm run build && npx playwright test console/`
Expected: 全部 PASS。

```bash
git add web/console web/e2e
git commit -m "feat(console): wallet with balance, cursor-paged ledger, orders and recharges tabs; e2e C-WAL, C-ERR, C-AUTH-06

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---
### Task 13: 控制台充值：发起页与充值单页

**Files:**
- Create: `web/console/src/logic/recharge.ts`、`web/console/src/logic/__tests__/recharge.spec.ts`
- Replace: `web/console/src/views/RechargeView.vue`、`web/console/src/views/RechargeOrderView.vue`
- Create: `web/e2e/console/recharge.spec.ts`

**Interfaces:**
- Consumes: Task 8～10 的 `api`、`session` / `refreshBalance`、`useGuide` / `GuideDialog`、`usePolling`；shared 的 `yuanToCents`、`fmtYuan`、`useCountdown`、`isMobileUA`；`qrcode`。
- Produces:
  - `src/logic/recharge.ts`：`alipayLink(qr): string`（`alipays://platformapi/startapp?saId=10000007&qrcode=<encodeURIComponent(qr)>`）、`isMockQR(qr)`（`mock://` 前缀）、`mmss(seconds)`（`15:00`）、`validateAmount(input): { cents: number } | { error: string }`（格式错「金额格式不正确」；范围错「充值金额需在 1 元到 100 万元之间」）、`PRESETS = [50, 100, 500, 1000]`。
  - 发起页 testid：`recharge-preset-<50|100|500|1000>`、`recharge-custom`、`recharge-amount-error`、`recharge-channel-<alipay|wechat>`、`recharge-unavailable`（手机且无支付宝）、`recharge-submit`；成功跳 `/wallet/recharge/:id`（保留 `redirect` query）。
  - 充值单页 testid：`rchorder-pending`、`rchorder-amount`、`rchorder-channel`、`rchorder-countdown`（`mm:ss`）、`rchorder-qr`（电脑 `canvas`）、`rchorder-alipay-link`（手机 `<a>`）、`rchorder-open-browser`（手机，`href` 为 `qr_code` 原文）、`rchorder-mock`（`mock://` 时的「开发环境模拟支付」块）、`rchorder-mock-pay`、`rchorder-expired`、`rchorder-paid`、`rchorder-paid-at`、`rchorder-closed`、`rchorder-back`、`rchorder-again`。
  - 轮询：每 3 秒 `GET /recharges/:id`，页面不可见时暂停、恢复可见时立即查一次；每次结果用 `remaining_seconds` 重新校准倒计时；变为 `paid` 时 `ElMessage.success('充值成功')` 并 `refreshBalance()`；倒计时归零再查一次，仍 pending 显示 `rchorder-expired`；离开页面清理定时器。
  - 覆盖：C-RCH-01～07。

- [ ] **Step 1: 写失败的单测**

`web/console/src/logic/__tests__/recharge.spec.ts`：

```ts
import { describe, expect, it } from 'vitest'
import { alipayLink, isMockQR, mmss, validateAmount } from '../recharge'

describe('recharge logic', () => {
  it('支付宝拉起链接按 encodeURIComponent 编码二维码内容', () => {
    expect(alipayLink('mock://pay/rch-1')).toBe('alipays://platformapi/startapp?saId=10000007&qrcode=mock%3A%2F%2Fpay%2Frch-1')
    expect(alipayLink('https://qr.alipay.com/bax?x=1&y=2')).toContain('qrcode=https%3A%2F%2Fqr.alipay.com%2Fbax%3Fx%3D1%26y%3D2')
  })
  it('mock 判断与倒计时格式', () => {
    expect(isMockQR('mock://pay/rch-1')).toBe(true)
    expect(isMockQR('https://qr.alipay.com/x')).toBe(false)
    expect(mmss(900)).toBe('15:00')
    expect(mmss(59)).toBe('00:59')
    expect(mmss(0)).toBe('00:00')
  })
  it('金额校验：格式与范围', () => {
    expect(validateAmount('88.5')).toEqual({ cents: 8850 })
    expect(validateAmount('1.234')).toEqual({ error: '金额格式不正确' })
    expect(validateAmount('abc')).toEqual({ error: '金额格式不正确' })
    expect(validateAmount('0.5')).toEqual({ error: '充值金额需在 1 元到 100 万元之间' })
    expect(validateAmount('1000001')).toEqual({ error: '充值金额需在 1 元到 100 万元之间' })
    expect(validateAmount('1000000')).toEqual({ cents: 100000000 })
    expect(validateAmount('1')).toEqual({ cents: 100 })
  })
})
```

Run: `cd web && npm test`
Expected: FAIL（模块不存在）。

- [ ] **Step 2: 逻辑与两个页面**

`web/console/src/logic/recharge.ts`：

```ts
import { yuanToCents } from '@spark/shared'

export const PRESETS = [50, 100, 500, 1000] as const
export const MIN_CENTS = 100
export const MAX_CENTS = 100_000_000

export function alipayLink(qr: string): string {
  return `alipays://platformapi/startapp?saId=10000007&qrcode=${encodeURIComponent(qr)}`
}

export const isMockQR = (qr: string) => qr.startsWith('mock://')

export function mmss(seconds: number): string {
  const s = Math.max(0, Math.floor(seconds))
  return `${String(Math.floor(s / 60)).padStart(2, '0')}:${String(s % 60).padStart(2, '0')}`
}

export function validateAmount(input: string): { cents: number } | { error: string } {
  const cents = yuanToCents(input)
  if (cents == null) return { error: '金额格式不正确' }
  if (cents < MIN_CENTS || cents > MAX_CENTS) return { error: '充值金额需在 1 元到 100 万元之间' }
  return { cents }
}
```

`web/console/src/views/RechargeView.vue`：

```vue
<script setup lang="ts">
import { isMobileUA, labelOf, messageOf, rechargeChannelLabel, type Listed, type Recharge } from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api } from '../api'
import GuideDialog from '../components/GuideDialog.vue'
import { useGuide } from '../composables/useGuide'
import { PRESETS, validateAmount } from '../logic/recharge'

const route = useRoute()
const router = useRouter()
const guide = useGuide()
const mobileUA = isMobileUA() // 渠道按设备类型（User-Agent），不看屏幕宽度
const channels = ref<string[]>([])
const channel = ref('')
const amount = ref('100')
const amountError = ref('')
const submitting = ref(false)
const loading = ref(true)

const visibleChannels = computed(() => (mobileUA ? channels.value.filter((c) => c === 'alipay') : channels.value))
const unavailable = computed(() => !loading.value && visibleChannels.value.length === 0)

onMounted(async () => {
  try {
    channels.value = (await api.get<Listed<string>>('/pay/channels')).items
    channel.value = visibleChannels.value[0] ?? ''
  } catch (e) {
    ElMessage.error(messageOf(e))
  } finally {
    loading.value = false
  }
})

function pick(yuan: number) {
  amount.value = String(yuan)
  amountError.value = ''
}

async function submit() {
  const v = validateAmount(amount.value)
  if ('error' in v) {
    amountError.value = v.error
    return
  }
  amountError.value = ''
  if (!channel.value) return
  submitting.value = true
  try {
    const r = await api.post<Recharge>('/recharges', { amount: v.cents, channel: channel.value })
    await router.push({ path: `/wallet/recharge/${r.id}`, query: route.query.redirect ? { redirect: String(route.query.redirect) } : {} })
  } catch (e) {
    guide.handle(e) // kyc_required 引导；too_many_pending / pay_unavailable / bad_request 原文提示
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <div class="recharge">
    <el-card>
      <el-form label-position="top" @submit.prevent="submit">
        <el-form-item label="充值金额（元）" :error="amountError">
          <div class="presets">
            <el-button v-for="p in PRESETS" :key="p" :type="amount === String(p) ? 'primary' : 'default'" :data-testid="`recharge-preset-${p}`" @click="pick(p)">{{ p }} 元</el-button>
          </div>
          <el-input v-model="amount" data-testid="recharge-custom" inputmode="decimal" placeholder="自定义金额，1～1,000,000 元，最多两位小数" @input="amountError = ''" />
          <span v-if="amountError" class="field-error" data-testid="recharge-amount-error">{{ amountError }}</span>
        </el-form-item>
        <el-form-item label="支付方式">
          <el-alert v-if="unavailable" type="warning" title="当前暂不支持手机端充值" :closable="false" show-icon data-testid="recharge-unavailable" />
          <el-radio-group v-else v-model="channel">
            <el-radio-button v-for="c in visibleChannels" :key="c" :value="c" :data-testid="`recharge-channel-${c}`">{{ labelOf(rechargeChannelLabel, c) }}</el-radio-button>
          </el-radio-group>
        </el-form-item>
        <el-button type="primary" native-type="submit" class="submit" :disabled="unavailable || !channel" :loading="submitting" data-testid="recharge-submit">下一步</el-button>
      </el-form>
    </el-card>
    <GuideDialog :guide="guide" />
  </div>
</template>

<style scoped>
.recharge { max-width: 560px; }
.presets { display: flex; flex-wrap: wrap; gap: 8px; margin-bottom: 8px; width: 100%; }
.submit { width: 100%; }
.field-error { color: var(--el-color-danger); font-size: 12px; line-height: 1; padding-top: 4px; }
</style>
```

（`el-form-item` 的 `:error` 已显示错误文案；额外的 `recharge-amount-error` 让测试有稳定定位，样式上把 `el-form-item` 的内置错误用 `:show-message="false"` 关掉——即 `<el-form-item label="充值金额（元）" :error="amountError" :show-message="false">`。）

`web/console/src/views/RechargeOrderView.vue`：

```vue
<script setup lang="ts">
import { fmtTime, fmtYuan, isMobileUA, labelOf, messageOf, rechargeChannelLabel, useCountdown, type Recharge } from '@spark/shared'
import { ElMessage } from 'element-plus'
import QRCode from 'qrcode'
import { computed, nextTick, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api } from '../api'
import { usePolling } from '../composables/usePolling'
import { alipayLink, isMockQR, mmss } from '../logic/recharge'
import { refreshBalance } from '../store/session'

const route = useRoute()
const router = useRouter()
const id = computed(() => String(route.params.id))
const mobileUA = isMobileUA()
const rch = ref<Recharge | null>(null)
const loadError = ref('')
const expired = ref(false)
const paying = ref(false)
const canvas = ref<HTMLCanvasElement | null>(null)
let announcedPaid = false

const countdown = useCountdown({
  onZero: async () => {
    await load() // 归零后再查一次，仍待支付则提示过期
    if (rch.value?.status === 'pending') expired.value = true
  },
})

async function load() {
  try {
    const r = await api.get<Recharge>(`/recharges/${id.value}`)
    rch.value = r
    loadError.value = ''
    if (r.status === 'pending') {
      countdown.start(r.remaining_seconds) // 每次轮询都按服务器剩余秒重新校准
      if (r.remaining_seconds === 0) expired.value = true
    } else {
      countdown.stop()
    }
    if (r.status === 'paid' && !announcedPaid) {
      announcedPaid = true
      ElMessage.success('充值成功')
      void refreshBalance()
    }
  } catch (e) {
    loadError.value = messageOf(e)
  }
}

onMounted(load)
usePolling(load, 3_000, { enabled: () => rch.value?.status === 'pending' && !expired.value })

const qr = computed(() => rch.value?.qr_code ?? '')
const mock = computed(() => isMockQR(qr.value))
const showCanvas = computed(() => !mobileUA && rch.value?.status === 'pending' && !!qr.value)
watch([showCanvas, qr], async ([show, text]) => {
  if (!show || !text) return
  await nextTick()
  if (canvas.value) await QRCode.toCanvas(canvas.value, text, { width: 240, margin: 1 })
}, { immediate: true })

async function mockPay() {
  paying.value = true
  try {
    await api.post(`/dev/recharges/${id.value}/pay`, {})
    await load()
  } catch (e) {
    ElMessage.error(messageOf(e))
  } finally {
    paying.value = false
  }
}

function back() {
  const redirect = typeof route.query.redirect === 'string' && route.query.redirect.startsWith('/') && !route.query.redirect.startsWith('//') ? route.query.redirect : '/wallet'
  void router.replace(redirect)
}
</script>

<template>
  <div class="order">
    <el-alert v-if="loadError" type="error" :title="loadError" :closable="false" show-icon />
    <el-card v-else-if="rch">
      <template v-if="rch.status === 'paid'">
        <el-result icon="success" title="充值成功" data-testid="rchorder-paid">
          <template #sub-title>
            <p>到账 {{ fmtYuan(rch.amount) }}</p>
            <p data-testid="rchorder-paid-at">到账时间 {{ fmtTime(rch.paid_at) }}</p>
          </template>
          <template #extra><el-button type="primary" data-testid="rchorder-back" @click="back">返回钱包</el-button></template>
        </el-result>
      </template>

      <template v-else-if="rch.status === 'closed'">
        <el-result icon="warning" title="充值单已关闭，请重新发起" data-testid="rchorder-closed">
          <template #extra><el-button type="primary" data-testid="rchorder-again" @click="router.replace('/wallet/recharge')">重新发起</el-button></template>
        </el-result>
      </template>

      <template v-else>
        <div class="pending" data-testid="rchorder-pending">
          <div class="amount" data-testid="rchorder-amount">{{ fmtYuan(rch.amount) }}</div>
          <div class="muted"><span data-testid="rchorder-channel">{{ labelOf(rechargeChannelLabel, rch.channel) }}</span> · 剩余 <span data-testid="rchorder-countdown">{{ mmss(countdown.remaining.value) }}</span></div>

          <el-alert v-if="expired" type="warning" title="二维码已过期，请重新发起" :closable="false" show-icon data-testid="rchorder-expired">
            <el-button size="small" data-testid="rchorder-again" @click="router.replace('/wallet/recharge')">重新发起</el-button>
          </el-alert>

          <template v-else>
            <canvas v-if="showCanvas" ref="canvas" class="qr" data-testid="rchorder-qr" width="240" height="240" />
            <template v-if="mobileUA">
              <a :href="alipayLink(qr)" class="el-button el-button--primary el-button--large alipay" data-testid="rchorder-alipay-link">打开支付宝付款</a>
              <a :href="qr" class="muted small" target="_blank" rel="noopener" data-testid="rchorder-open-browser">没有自动打开？点此在浏览器中打开</a>
            </template>
            <div v-if="mock" class="mock" data-testid="rchorder-mock">
              <p>开发环境模拟支付</p>
              <el-button type="success" :loading="paying" data-testid="rchorder-mock-pay" @click="mockPay">模拟支付成功</el-button>
            </div>
            <p class="muted small">支付完成后页面会自动刷新</p>
          </template>
        </div>
      </template>
    </el-card>
  </div>
</template>

<style scoped>
.order { max-width: 560px; }
.pending { display: flex; flex-direction: column; align-items: center; gap: 12px; text-align: center; }
.amount { font-size: 28px; font-weight: 600; }
.muted { color: #909399; }
.small { font-size: 12px; }
.qr { width: 240px; height: 240px; }
.alipay { width: 100%; max-width: 320px; text-decoration: none; }
.mock { border: 1px dashed var(--el-color-warning); padding: 12px; border-radius: 8px; }
</style>
```

Run: `cd web && npm test && npm run build`
Expected: Vitest PASS（recharge 3）；构建通过。

- [ ] **Step 3: 端到端用例**

`web/e2e/console/recharge.spec.ts`：

```ts
import { expect, test, type Page } from '@playwright/test'
import { isMobile, loginConsole, nav, seedUser, toast, userApi } from '../helpers/console'
import { hooks } from '../helpers/hooks'

async function createRecharge(phone: string, amount = 5000) {
  const api = await userApi(phone)
  const r = await api.post('/api/v1/recharges', { data: { amount, channel: 'alipay' } })
  if (r.status() !== 201) throw new Error(await r.text())
  const body = (await r.json()) as { id: string; qr_code: string }
  return { api, id: body.id, qr: body.qr_code }
}

function setVisibility(page: Page, state: 'visible' | 'hidden') {
  return page.evaluate((s) => {
    Object.defineProperty(document, 'visibilityState', { value: s, configurable: true })
    document.dispatchEvent(new Event('visibilitychange'))
  }, state)
}

test('C-RCH-01 发起页金额校验', async ({ page }) => {
  const u = await seedUser()
  let posts = 0
  page.on('request', (r) => {
    if (r.method() === 'POST' && r.url().endsWith('/api/v1/recharges')) posts += 1
  })
  await loginConsole(page, u.phone)
  await page.goto('/wallet/recharge')
  await page.getByTestId('recharge-preset-100').click()
  await expect(page.getByTestId('recharge-custom')).toHaveValue('100')
  for (const [value, error] of [
    ['0.5', '充值金额需在 1 元到 100 万元之间'],
    ['1000001', '充值金额需在 1 元到 100 万元之间'],
    ['1.234', '金额格式不正确'],
  ]) {
    await page.getByTestId('recharge-custom').fill(value)
    await page.getByTestId('recharge-submit').click()
    await expect(page.getByTestId('recharge-amount-error')).toHaveText(error)
  }
  expect(posts).toBe(0)
  await page.getByTestId('recharge-custom').fill('88.5')
  await page.getByTestId('recharge-submit').click()
  await expect(page).toHaveURL(/\/wallet\/recharge\/rch-/)
  await expect(page.getByTestId('rchorder-amount')).toHaveText('¥88.50')
  expect(posts).toBe(1)
})

test('C-RCH-02 渠道按设备类型：电脑两个渠道且缩窄视口不变；手机只有支付宝', async ({ page }, info) => {
  const u = await seedUser()
  await loginConsole(page, u.phone)
  await page.goto('/wallet/recharge')
  if (isMobile(info)) {
    await expect(page.getByTestId('recharge-channel-alipay')).toBeVisible()
    await expect(page.getByTestId('recharge-channel-wechat')).toHaveCount(0)
  } else {
    await expect(page.getByTestId('recharge-channel-wechat')).toBeVisible()
    await expect(page.getByTestId('recharge-channel-alipay')).toBeVisible()
    await page.setViewportSize({ width: 390, height: 844 })
    await expect(page.getByTestId('recharge-channel-wechat')).toBeVisible()
    await expect(page.getByTestId('recharge-channel-alipay')).toBeVisible()
  }
})

test('C-RCH-03 支付态：倒计时、3 秒轮询、模拟支付成功、余额更新、离开后停止轮询', async ({ page }, info) => {
  const u = await seedUser({ balance: 100000 })
  await page.clock.install()
  await loginConsole(page, u.phone)
  await page.goto('/wallet/recharge')
  await page.getByTestId('recharge-preset-50').click()
  await page.getByTestId('recharge-submit').click()
  await expect(page).toHaveURL(/\/wallet\/recharge\/rch-/)
  const rid = page.url().split('/').pop()!
  let polls = 0
  page.on('request', (r) => {
    if (r.method() === 'GET' && r.url().endsWith(`/api/v1/recharges/${rid}`)) polls += 1
  })
  await expect(page.getByTestId('rchorder-countdown')).toHaveText('15:00')
  await page.clock.runFor(1000)
  await expect(page.getByTestId('rchorder-countdown')).toHaveText('14:59')
  const before = polls
  await page.clock.runFor(3000)
  await expect.poll(() => polls).toBe(before + 1)
  await expect(page.getByTestId('rchorder-mock')).toContainText('开发环境模拟支付')
  if (isMobile(info)) {
    await expect(page.getByTestId('rchorder-qr')).toHaveCount(0)
    await expect(page.getByTestId('rchorder-alipay-link')).toBeVisible()
  } else {
    await expect(page.getByTestId('rchorder-qr')).toBeVisible()
    await expect(page.getByTestId('rchorder-alipay-link')).toHaveCount(0)
  }
  await page.getByTestId('rchorder-mock-pay').click()
  await toast(page, '充值成功')
  await expect(page.getByTestId('rchorder-paid')).toBeVisible()
  await page.getByTestId('rchorder-back').click()
  await expect(page).toHaveURL(/\/wallet$/)
  await expect(page.getByTestId('wallet-balance')).toHaveText('¥1,050.00')
  const afterLeave = polls
  await page.clock.runFor(9000)
  expect(polls).toBe(afterLeave) // 离开页面后定时器已清理
})

test('C-RCH-04 手机拉起支付宝链接与回页立即查询；电脑不可见时暂停轮询、恢复时立即查一次', async ({ page }, info) => {
  const u = await seedUser({ balance: 100000 })
  const { api, id, qr } = await createRecharge(u.phone)
  // 两个 project 都接管浏览器定时器：否则 3 秒轮询自己就能查到已支付，「回到页面立即查询」这条逻辑有没有都会通过
  await page.clock.install()
  await loginConsole(page, u.phone)
  let polls = 0
  page.on('request', (r) => {
    if (r.method() === 'GET' && r.url().endsWith(`/api/v1/recharges/${id}`)) polls += 1
  })
  await page.goto(`/wallet/recharge/${id}`)
  await expect(page.getByTestId('rchorder-pending')).toBeVisible()
  if (isMobile(info)) {
    const href = await page.getByTestId('rchorder-alipay-link').getAttribute('href')
    expect(href).toBe(`alipays://platformapi/startapp?saId=10000007&qrcode=${encodeURIComponent(qr)}`)
    await expect(page.getByTestId('rchorder-open-browser')).toHaveAttribute('href', qr)
  }
  // 用户切到支付宝（页面不可见）：轮询暂停
  await setVisibility(page, 'hidden')
  const paused = polls
  await page.clock.runFor(9000)
  expect(polls).toBe(paused)
  // 支付宝侧完成支付
  await api.post(`/api/v1/dev/recharges/${id}/pay`, { data: {} })
  await expect(page.getByTestId('rchorder-pending')).toBeVisible() // 页面不可见期间没有自己查到
  // 回到页面：不等下一个 3 秒，立即查询一次并显示成功
  await setVisibility(page, 'visible')
  await expect.poll(() => polls).toBe(paused + 1)
  await expect(page.getByTestId('rchorder-paid')).toBeVisible()
  await toast(page, '充值成功')
  // 已支付后不再轮询
  await page.clock.runFor(9000)
  expect(polls).toBe(paused + 1)
})

test('C-RCH-05 关单后显示已关闭', async ({ page }) => {
  const u = await seedUser()
  const { id } = await createRecharge(u.phone)
  await hooks.advance(7200)
  await hooks.sweep()
  await loginConsole(page, u.phone)
  await page.goto(`/wallet/recharge/${id}`)
  await expect(page.getByTestId('rchorder-closed')).toContainText('已关闭，请重新发起')
  await page.getByTestId('rchorder-again').click()
  await expect(page).toHaveURL(/\/wallet\/recharge$/)
})

test('C-RCH-06 待支付过多', async ({ page }) => {
  const u = await seedUser()
  const api = await userApi(u.phone)
  for (let i = 0; i < 5; i++) {
    const r = await api.post('/api/v1/recharges', { data: { amount: 1000, channel: 'alipay' } })
    expect(r.status()).toBe(201)
  }
  await loginConsole(page, u.phone)
  await page.goto('/wallet/recharge')
  await page.getByTestId('recharge-preset-50').click()
  await page.getByTestId('recharge-submit').click()
  await toast(page, '待支付的充值单过多')
  await expect(page).toHaveURL(/\/wallet\/recharge$/)
})

test('C-RCH-07 已支付单重进直接显示成功页', async ({ page }) => {
  const u = await seedUser()
  const { api, id } = await createRecharge(u.phone, 8850)
  await api.post(`/api/v1/dev/recharges/${id}/pay`, { data: {} })
  await loginConsole(page, u.phone)
  await page.goto(`/wallet/recharge/${id}`)
  await expect(page.getByTestId('rchorder-paid')).toContainText('¥88.50')
  await expect(page.getByTestId('rchorder-paid-at')).toHaveText(/到账时间 \d{4}-\d{2}-\d{2} \d{2}:\d{2}/)
  await expect(page.getByTestId('rchorder-pending')).toHaveCount(0)
})
```

Run: `make web && cd web && npx playwright test console/recharge.spec.ts`
Expected: 7 × 2 = 14 passed。

- [ ] **Step 4: 全量验证并提交**

Run: `go vet ./... && go test ./... && cd web && npm test && npm run build && npx playwright test console/`
Expected: 全部 PASS。

```bash
git add web/console web/e2e
git commit -m "feat(console): recharge flow with channel-by-UA, QR/alipay deep link, 3s polling and mock pay; e2e C-RCH

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---
### Task 14: 控制台实名认证

**Files:**
- Replace: `web/console/src/views/KycView.vue`
- Create: `web/console/src/logic/kyc.ts`、`web/console/src/logic/__tests__/kyc.spec.ts`
- Create: `web/e2e/console/kyc.spec.ts`

**Interfaces:**
- Consumes: Task 8 的 `api`、`session` / `setUser`、`safeRedirect`；Task 2 的 `GET /api/v1/kyc`；Task 7 的 `adminApi` / `adminPost`、fixtures。
- Produces:
  - `src/logic/kyc.ts`：`ID_NUMBER_RE = /^\d{17}[\dX]$/`、`LICENSE_RE = /^[0-9A-HJ-NPQRTUWXY]{2}\d{6}[0-9A-HJ-NPQRTUWXY]{10}$/`、`validatePersonal({ name, idNumber })` / `validateEnterprise({ company, licenseNo, fileKey })` → `Record<field, string>` 错误表。
  - testid：状态条 `kyc-status`（文案 未认证 / 审核中，通常 1 个工作日内完成 / 已认证 / 未通过）、`kyc-verified-name`、`kyc-reason`；个人：`kyc-name`、`kyc-idno`（`autocomplete="off"`）、`kyc-personal-submit`、`kyc-rejected-box`、`kyc-name-error`、`kyc-idno-error`；企业：`kyc-company`、`kyc-license-no`、`kyc-upload`（`el-upload`，内含 `input[type=file]`）、`kyc-file-name`、`kyc-file-preview`（图片缩略图 `<img>`）、`kyc-file-pdf`（PDF 图标）、`kyc-upload-error`、`kyc-company-error`、`kyc-license-error`、`kyc-enterprise-submit`（未上传时禁用）；标签 `role="tab"` 名称「个人」「企业」。
  - 认证成功：`setUser(u)`，`redirect` 存在则跳回。
  - 覆盖：C-KYC-01～06。

- [ ] **Step 1: 写失败的单测**

`web/console/src/logic/__tests__/kyc.spec.ts`：

```ts
import { describe, expect, it } from 'vitest'
import { validateEnterprise, validatePersonal } from '../kyc'

describe('kyc validation', () => {
  it('个人：姓名必填、身份证 18 位末位可 X', () => {
    expect(validatePersonal({ name: '张三', idNumber: '11010119900101123X' })).toEqual({})
    expect(validatePersonal({ name: '张三', idNumber: '11010119900101123x' })).toEqual({})
    expect(validatePersonal({ name: '', idNumber: '110101199001011234' })).toEqual({ name: '请填写真实姓名' })
    expect(validatePersonal({ name: '张三', idNumber: '11010119900101123' })).toEqual({ idNumber: '身份证号格式不正确' })
  })
  it('企业：名称、信用代码、执照', () => {
    expect(validateEnterprise({ company: '测试科技有限公司', licenseNo: '91110000MA01ABCD2X', fileKey: 'k' })).toEqual({})
    expect(validateEnterprise({ company: '测试科技有限公司', licenseNo: '123', fileKey: 'k' })).toEqual({ licenseNo: '统一社会信用代码格式不正确' })
    expect(validateEnterprise({ company: '', licenseNo: '91110000MA01ABCD2X', fileKey: '' })).toEqual({ company: '请填写企业名称', fileKey: '请上传营业执照' })
    expect(validateEnterprise({ company: 'x', licenseNo: '91110000ma01abcd2x', fileKey: 'k' })).toEqual({})
  })
})
```

Run: `cd web && npm test`
Expected: FAIL（模块不存在）。

- [ ] **Step 2: 逻辑与页面**

`web/console/src/logic/kyc.ts`：

```ts
export const ID_NUMBER_RE = /^\d{17}[\dX]$/
export const LICENSE_RE = /^[0-9A-HJ-NPQRTUWXY]{2}\d{6}[0-9A-HJ-NPQRTUWXY]{10}$/

export function validatePersonal(v: { name: string; idNumber: string }): Partial<Record<'name' | 'idNumber', string>> {
  const errors: Partial<Record<'name' | 'idNumber', string>> = {}
  if (!v.name.trim()) errors.name = '请填写真实姓名'
  if (!ID_NUMBER_RE.test(v.idNumber.trim().toUpperCase())) errors.idNumber = '身份证号格式不正确'
  return errors
}

export function validateEnterprise(v: { company: string; licenseNo: string; fileKey: string }): Partial<Record<'company' | 'licenseNo' | 'fileKey', string>> {
  const errors: Partial<Record<'company' | 'licenseNo' | 'fileKey', string>> = {}
  if (!v.company.trim()) errors.company = '请填写企业名称'
  if (!LICENSE_RE.test(v.licenseNo.trim().toUpperCase())) errors.licenseNo = '统一社会信用代码格式不正确'
  if (!v.fileKey) errors.fileKey = '请上传营业执照'
  return errors
}
```

`web/console/src/views/KycView.vue`：

```vue
<script setup lang="ts">
import { isApiError, messageOf, type KYCRecord, type Upload, type User } from '@spark/shared'
import { ElMessage, type UploadRequestOptions } from 'element-plus'
import { computed, onMounted, reactive, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api } from '../api'
import { validateEnterprise, validatePersonal } from '../logic/kyc'
import { safeRedirect } from '../router'
import { session, setUser } from '../store/session'

const route = useRoute()
const router = useRouter()
const record = ref<KYCRecord | null>(null)
const tab = ref<'personal' | 'enterprise'>('personal')
const status = computed(() => session.user?.kyc_status ?? 'none')
const showForm = computed(() => status.value === 'none' || status.value === 'rejected')

async function loadRecord() {
  try {
    record.value = (await api.get<{ record: KYCRecord | null }>('/kyc')).record
  } catch (e) {
    ElMessage.error(messageOf(e))
  }
}
async function refreshUser() {
  setUser(await api.get<User>('/me'))
}
onMounted(loadRecord)

const statusText = computed(() => {
  switch (status.value) {
    case 'verified':
      return '已认证'
    case 'pending':
      return '审核中，通常 1 个工作日内完成'
    case 'rejected':
      return '未通过'
    default:
      return '未认证'
  }
})
const statusType = computed(() => ({ verified: 'success', pending: 'warning', rejected: 'error', none: 'info' })[status.value] as 'success')

// ---- 个人 ----
const personal = reactive({ name: '', idNumber: '' })
const personalErrors = ref<Partial<Record<'name' | 'idNumber', string>>>({})
const rejectedMessage = ref('')
const submittingPersonal = ref(false)
async function submitPersonal() {
  personalErrors.value = validatePersonal(personal)
  if (Object.keys(personalErrors.value).length) return
  submittingPersonal.value = true
  rejectedMessage.value = ''
  try {
    const u = await api.post<User>('/kyc/personal', { name: personal.name.trim(), id_number: personal.idNumber.trim().toUpperCase() })
    setUser(u)
    ElMessage.success('实名认证成功')
    await loadRecord()
    const back = safeRedirect(route.query.redirect)
    if (back) await router.replace(back)
  } catch (e) {
    if (isApiError(e) && e.code === 'kyc_rejected') {
      rejectedMessage.value = e.message
      await Promise.all([refreshUser(), loadRecord()])
    } else ElMessage.error(messageOf(e))
  } finally {
    submittingPersonal.value = false
  }
}

// ---- 企业 ----
const enterprise = reactive({ company: '', licenseNo: '', fileKey: '', fileName: '', previewUrl: '', isPdf: false })
const enterpriseErrors = ref<Partial<Record<'company' | 'licenseNo' | 'fileKey', string>>>({})
const uploadError = ref('')
const uploading = ref(false)
const submittingEnterprise = ref(false)

async function doUpload(opt: UploadRequestOptions) {
  uploading.value = true
  uploadError.value = ''
  try {
    const up = await api.upload<Upload>('/uploads', opt.file)
    enterprise.fileKey = up.key
    enterprise.fileName = opt.file.name
    enterprise.isPdf = up.content_type === 'application/pdf'
    if (enterprise.previewUrl) URL.revokeObjectURL(enterprise.previewUrl)
    enterprise.previewUrl = enterprise.isPdf ? '' : URL.createObjectURL(opt.file)
    enterpriseErrors.value.fileKey = undefined
  } catch (e) {
    enterprise.fileKey = ''
    enterprise.fileName = ''
    enterprise.previewUrl = ''
    uploadError.value = messageOf(e) // file_too_large / unsupported_file_type 原文
    ElMessage.error(uploadError.value)
  } finally {
    uploading.value = false
  }
}

async function submitEnterprise() {
  enterpriseErrors.value = validateEnterprise(enterprise)
  if (Object.keys(enterpriseErrors.value).length) return
  submittingEnterprise.value = true
  try {
    await api.post('/kyc/enterprise', { company_name: enterprise.company.trim(), license_no: enterprise.licenseNo.trim().toUpperCase(), license_file: enterprise.fileKey })
    ElMessage.success('已提交，等待审核')
    await Promise.all([refreshUser(), loadRecord()])
  } catch (e) {
    ElMessage.error(messageOf(e))
  } finally {
    submittingEnterprise.value = false
  }
}
</script>

<template>
  <div class="kyc">
    <el-alert :type="statusType" :closable="false" show-icon data-testid="kyc-status-bar">
      <template #title>
        <span data-testid="kyc-status">{{ statusText }}</span>
        <span v-if="status === 'verified' && record" data-testid="kyc-verified-name">（{{ record.kind === 'enterprise' ? '企业' : '个人' }}：{{ record.real_name }}）</span>
      </template>
      <p v-if="status === 'rejected' && record?.reason" data-testid="kyc-reason">原因：{{ record.reason }}</p>
    </el-alert>

    <el-card v-if="showForm">
      <el-tabs v-model="tab">
        <el-tab-pane label="个人" name="personal">
          <el-alert v-if="rejectedMessage" type="error" :title="rejectedMessage" :closable="false" show-icon data-testid="kyc-rejected-box" />
          <el-form label-position="top" @submit.prevent="submitPersonal">
            <el-form-item label="真实姓名" :error="personalErrors.name" :show-message="false">
              <el-input v-model="personal.name" data-testid="kyc-name" maxlength="50" />
              <span v-if="personalErrors.name" class="field-error" data-testid="kyc-name-error">{{ personalErrors.name }}</span>
            </el-form-item>
            <el-form-item label="身份证号" :error="personalErrors.idNumber" :show-message="false">
              <el-input v-model="personal.idNumber" data-testid="kyc-idno" maxlength="18" autocomplete="off" />
              <span v-if="personalErrors.idNumber" class="field-error" data-testid="kyc-idno-error">{{ personalErrors.idNumber }}</span>
            </el-form-item>
            <el-button type="primary" native-type="submit" :loading="submittingPersonal" data-testid="kyc-personal-submit">提交认证</el-button>
          </el-form>
        </el-tab-pane>

        <el-tab-pane label="企业" name="enterprise">
          <el-form label-position="top" @submit.prevent="submitEnterprise">
            <el-form-item label="企业名称" :error="enterpriseErrors.company" :show-message="false">
              <el-input v-model="enterprise.company" data-testid="kyc-company" maxlength="100" />
              <span v-if="enterpriseErrors.company" class="field-error" data-testid="kyc-company-error">{{ enterpriseErrors.company }}</span>
            </el-form-item>
            <el-form-item label="统一社会信用代码" :error="enterpriseErrors.licenseNo" :show-message="false">
              <el-input v-model="enterprise.licenseNo" data-testid="kyc-license-no" maxlength="18" />
              <span v-if="enterpriseErrors.licenseNo" class="field-error" data-testid="kyc-license-error">{{ enterpriseErrors.licenseNo }}</span>
            </el-form-item>
            <el-form-item label="营业执照（JPG / PNG / PDF，10MB 以内）">
              <el-upload data-testid="kyc-upload" :http-request="doUpload" :show-file-list="false" accept=".jpg,.jpeg,.png,.pdf,image/jpeg,image/png,application/pdf">
                <el-button :loading="uploading">{{ enterprise.fileKey ? '重新选择' : '选择文件' }}</el-button>
              </el-upload>
              <div v-if="enterprise.fileKey" class="file">
                <img v-if="enterprise.previewUrl" :src="enterprise.previewUrl" alt="营业执照预览" class="thumb" data-testid="kyc-file-preview" />
                <span v-else class="pdf" data-testid="kyc-file-pdf">PDF</span>
                <span data-testid="kyc-file-name">{{ enterprise.fileName }}</span>
              </div>
              <span v-if="uploadError" class="field-error" data-testid="kyc-upload-error">{{ uploadError }}</span>
              <span v-else-if="enterpriseErrors.fileKey" class="field-error">{{ enterpriseErrors.fileKey }}</span>
            </el-form-item>
            <el-button type="primary" native-type="submit" :disabled="!enterprise.fileKey" :loading="submittingEnterprise" data-testid="kyc-enterprise-submit">提交审核</el-button>
          </el-form>
        </el-tab-pane>
      </el-tabs>
    </el-card>
  </div>
</template>

<style scoped>
.kyc { display: grid; gap: 16px; max-width: 640px; }
.field-error { color: var(--el-color-danger); font-size: 12px; padding-top: 4px; }
.file { display: flex; align-items: center; gap: 8px; margin-top: 8px; }
.thumb { width: 64px; height: 64px; object-fit: cover; border: 1px solid #ebeef5; border-radius: 4px; }
.pdf { display: inline-block; padding: 2px 6px; border: 1px solid var(--el-color-danger); color: var(--el-color-danger); border-radius: 4px; font-size: 12px; }
</style>
```

Run: `cd web && npm test && npm run build`
Expected: Vitest PASS；构建通过。

- [ ] **Step 3: 端到端用例**

`web/e2e/console/kyc.spec.ts`：

```ts
import { expect, test, type Page } from '@playwright/test'
import { fileURLToPath } from 'node:url'
import { adminApi, adminPost, createAdmin } from '../helpers/admin'
import { loginConsole, seedUser, toast } from '../helpers/console'
import { hooks } from '../helpers/hooks'

const fixture = (name: string) => fileURLToPath(new URL(`../fixtures/${name}`, import.meta.url))
const LICENSE_NO = '91110000MA01ABCD2X'

async function submitEnterprise(page: Page, file = 'license.png') {
  await page.getByRole('tab', { name: '企业' }).click()
  await page.getByTestId('kyc-company').fill('测试科技有限公司')
  await page.getByTestId('kyc-license-no').fill(LICENSE_NO)
  await page.getByTestId('kyc-upload').locator('input[type=file]').setInputFiles(fixture(file))
  await expect(page.getByTestId('kyc-file-name')).toHaveText(file)
  await page.getByTestId('kyc-enterprise-submit').click()
  await expect(page.getByTestId('kyc-status')).toHaveText('审核中，通常 1 个工作日内完成')
}

async function reviewLatest(userId: number, approve: boolean, reason = '') {
  const admin = await createAdmin('ops')
  const ctx = await adminApi(admin)
  const pending = ((await (await ctx.get('/admin/api/v1/kyc/pending?page_size=100')).json()) as { items: { id: number; user_id: number }[] }).items
  const rec = pending.find((r) => r.user_id === userId)
  if (!rec) throw new Error('pending kyc record not found')
  await adminPost(ctx, admin, `/admin/api/v1/kyc/${rec.id}/review`, { approve, reason })
}

test('C-KYC-01 个人实名成功并按 redirect 回到市场', async ({ page }) => {
  const u = await seedUser({ verified: false })
  await loginConsole(page, u.phone)
  await page.goto('/kyc?redirect=/market')
  await expect(page.getByTestId('kyc-status')).toHaveText('未认证')
  await page.getByTestId('kyc-name').fill('张三')
  await page.getByTestId('kyc-idno').fill('11010119900101123X')
  await page.getByTestId('kyc-personal-submit').click()
  await toast(page, '实名认证成功')
  await expect(page).toHaveURL(/\/market$/)
  await page.goto('/kyc')
  await expect(page.getByTestId('kyc-status')).toHaveText('已认证')
  await expect(page.getByTestId('kyc-verified-name')).toContainText('个人：张*')
  await expect(page.getByTestId('kyc-name')).toHaveCount(0)
})

test('C-KYC-02 个人实名失败：红框原因与未通过状态', async ({ page }) => {
  const u = await seedUser({ verified: false })
  await loginConsole(page, u.phone)
  await page.goto('/kyc')
  await hooks.kyc(true)
  try {
    await page.getByTestId('kyc-name').fill('李四')
    await page.getByTestId('kyc-idno').fill('110101199001011234')
    await page.getByTestId('kyc-personal-submit').click()
    await expect(page.getByTestId('kyc-rejected-box')).toContainText('实名核验未通过：')
    await expect(page.getByTestId('kyc-status')).toHaveText('未通过')
    await expect(page.getByTestId('kyc-reason')).toContainText('姓名、身份证号与手机号不一致')
    await expect(page.getByTestId('kyc-personal-submit')).toBeVisible() // 可重新提交
  } finally {
    await hooks.kyc(false)
  }
})

test('C-KYC-03 字段校验：身份证 17 位、信用代码格式、未上传不能提交', async ({ page }) => {
  const u = await seedUser({ verified: false })
  let posts = 0
  page.on('request', (r) => {
    if (r.method() === 'POST' && r.url().includes('/api/v1/kyc/')) posts += 1
  })
  await loginConsole(page, u.phone)
  await page.goto('/kyc')
  await page.getByTestId('kyc-name').fill('王五')
  await page.getByTestId('kyc-idno').fill('11010119900101123')
  await page.getByTestId('kyc-personal-submit').click()
  await expect(page.getByTestId('kyc-idno-error')).toHaveText('身份证号格式不正确')
  await page.getByRole('tab', { name: '企业' }).click()
  await expect(page.getByTestId('kyc-enterprise-submit')).toBeDisabled()
  await page.getByTestId('kyc-company').fill('测试科技有限公司')
  await page.getByTestId('kyc-license-no').fill('123')
  await page.getByTestId('kyc-upload').locator('input[type=file]').setInputFiles(fixture('license.png'))
  await expect(page.getByTestId('kyc-enterprise-submit')).toBeEnabled()
  await page.getByTestId('kyc-enterprise-submit').click()
  await expect(page.getByTestId('kyc-license-error')).toHaveText('统一社会信用代码格式不正确')
  expect(posts).toBe(0)
})

test('C-KYC-04 企业提交显示缩略图与审核中，后台通过后已认证', async ({ page }) => {
  const u = await seedUser({ verified: false })
  await loginConsole(page, u.phone)
  await page.goto('/kyc')
  await page.getByRole('tab', { name: '企业' }).click()
  await page.getByTestId('kyc-upload').locator('input[type=file]').setInputFiles(fixture('license.png'))
  await expect(page.getByTestId('kyc-file-preview')).toBeVisible()
  await page.getByTestId('kyc-company').fill('测试科技有限公司')
  await page.getByTestId('kyc-license-no').fill(LICENSE_NO)
  await page.getByTestId('kyc-enterprise-submit').click()
  await expect(page.getByTestId('kyc-status')).toHaveText('审核中，通常 1 个工作日内完成')
  await expect(page.getByTestId('kyc-company')).toHaveCount(0)
  await reviewLatest(u.id, true)
  await page.reload()
  await expect(page.getByTestId('kyc-status')).toHaveText('已认证')
  await expect(page.getByTestId('kyc-verified-name')).toContainText('企业：测试科技有限公司')
})

test('C-KYC-05 企业驳回后显示原因并可重新提交', async ({ page }) => {
  const u = await seedUser({ verified: false })
  await loginConsole(page, u.phone)
  await page.goto('/kyc')
  await submitEnterprise(page)
  await reviewLatest(u.id, false, '执照不清晰')
  await page.reload()
  await expect(page.getByTestId('kyc-status')).toHaveText('未通过')
  await expect(page.getByTestId('kyc-reason')).toContainText('执照不清晰')
  await page.getByRole('tab', { name: '企业' }).click()
  await expect(page.getByTestId('kyc-company')).toBeVisible()
})

test('C-KYC-06 上传限制：GIF 被拒绝，PDF 显示图标', async ({ page }) => {
  const u = await seedUser({ verified: false })
  await loginConsole(page, u.phone)
  await page.goto('/kyc')
  await page.getByRole('tab', { name: '企业' }).click()
  const input = page.getByTestId('kyc-upload').locator('input[type=file]')
  await input.setInputFiles(fixture('bad.gif'))
  await expect(page.getByTestId('kyc-upload-error')).toHaveText('仅支持 JPG、PNG、PDF 文件')
  await expect(page.getByTestId('kyc-enterprise-submit')).toBeDisabled()
  await input.setInputFiles(fixture('license.pdf'))
  await expect(page.getByTestId('kyc-file-pdf')).toBeVisible()
  await expect(page.getByTestId('kyc-file-name')).toHaveText('license.pdf')
  await expect(page.getByTestId('kyc-upload-error')).toHaveCount(0)
})
```

Run: `make web && cd web && npx playwright test console/kyc.spec.ts`
Expected: 6 × 2 = 12 passed。

- [ ] **Step 4: 全量验证并提交（控制台 52 个用例齐）**

Run: `go vet ./... && go test ./... && cd web && npm test && npm run build && npx playwright test --project=console-desktop --project=console-mobile`
Expected: 全部 PASS，控制台 52 × 2 = 104 passed。再 `node e2e/coverage.mjs | grep -c '缺少用例：C-'` 应为 0（只剩 A- 缺少）。

```bash
git add web/console web/e2e
git commit -m "feat(console): KYC page with personal verification and enterprise upload/review flow; e2e C-KYC

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---
### Task 15: 后台骨架：入口、路由与角色守卫、会话、布局与菜单、动态码弹框、登录三步、个人设置、403

**Files:**
- Create: `web/admin/{package.json,index.html,vite.config.ts,tsconfig.json,vitest.config.ts}`、`web/admin/public/brand/logo.svg`（与 console 相同）
- Create: `web/admin/src/{main.ts,App.vue,router.ts,api.ts,menu.ts,styles.css,env.d.ts}`、`web/admin/src/store/{session,totp}.ts`、`web/admin/src/layouts/AdminLayout.vue`、`web/admin/src/components/TotpDialog.vue`
- Create: `web/admin/src/views/{LoginView,SettingsView,ForbiddenView,NotFoundView}.vue`
- Create（骨架，只有标题，后续任务整体替换）: `web/admin/src/views/{DashboardView,MachinesView,MachineDetailView,PdusView,AlertsView,InstancesView,InstanceDetailView,UsersView,UserDetailView,KycView,OrdersView,LedgerView,RechargesView,CatalogView,AdminsView,AuditView}.vue`
- Create: `web/admin/src/__tests__/menu.spec.ts`、`web/admin/src/store/__tests__/totp.spec.ts`
- Create: `web/e2e/admin/{auth,nav,settings}.spec.ts`
- Modify: `web/package.json`（`workspaces` 加 `admin`）、`web/vitest.config.ts`（加 `admin/vitest.config.ts`）

**Interfaces:**
- Consumes: Task 5～7 的 shared 与 helpers；Task 3 的 `/admin/` 托管。
- Produces（后续后台任务依赖）:
  - `api`（`src/api.ts`）：`createHttp({ base: '/admin/api/v1', onUnauthorized, promptTotp, onTotpSetupRequired })`。
  - `src/store/session.ts`：`session: { status: 'unknown' | 'anon' | 'authed'; admin: Admin | null }`、`probeSession()`、`setAdmin(a)`、`clearSession()`、`handleUnauthorized()`（在 `/login` 以外：清会话、提示一次「登录已失效，请重新登录」、`router.replace('/login?redirect=<当前>')`）、`bindRouter(router)`、`hasRole(roles)`。
  - `src/store/totp.ts`：`totpState`、`promptTotp(message?): Promise<string | null>`（单例）、`submitTotp()`、`cancelTotp()`；`TotpDialog` testid：`totp-dialog`、`totp-message`、`totp-input`、`totp-ok`、`totp-cancel`。
  - `src/menu.ts`：`menu: MenuItem[]`（13 项，`{ key, path, label, roles }`）、`menuFor(role)`、角色常量 `ALL_ROLES`、`OPS_ROLES`、`FIN_ROLES`、`SUPER_ONLY`。
  - `router`：`createWebHistory('/admin/')`；路由 `meta: { title, roles?: Role[] }`；无权限 → `/403`；未登录 → `/login?redirect=`；`afterEach` 标题 `<页面> - Spark Cloud 管理后台`。
  - 布局 testid：`menu-<key>`（key 同 `menu.ts`）、`menu-alerts-badge`（未处理告警数，60 秒刷新）、`layout-username`、`layout-role`、`layout-settings`、`layout-logout`、`layout-collapse`（折叠状态存 `localStorage` 键 `sc_admin_sidebar_collapsed`）。
  - 登录 testid：`login-username`、`login-password`、`login-submit`、`login-timeout`（「登录已超时，请重新输入密码」）；绑定步骤 `login-setup-qr`（canvas）、`login-setup-secret`、`login-setup-copy-secret`、`login-setup-code`、`login-setup-submit`、`login-recovery-codes` / `login-recovery-code`（8 个）、`login-recovery-copy`、`login-recovery-saved`（复选框）、`login-enter`；动态码步骤 `login-totp-code`、`login-totp-submit`、`login-totp-hint`、`login-use-recovery`、`login-recovery-input`、`login-recovery-submit`、`login-use-totp`。
  - 设置页 testid：`settings-old`、`settings-new`、`settings-confirm`、`settings-submit`、`settings-confirm-error`、`settings-new-error`；403 页 `forbidden-page`、`forbidden-home`。
  - 覆盖：A-AUTH-01～08、A-NAV-01、A-NAV-02、A-SET-01、U-12。

- [ ] **Step 1: 工作区与配置**

`web/package.json` 的 `workspaces` 改为 `["shared", "console", "admin"]`；`web/vitest.config.ts` 的 `projects` 加 `'admin/vitest.config.ts'`。

`web/admin/package.json`（与 console 相同的依赖，名称 `admin`，`dev` 为 `vite --port 5174`）。`web/admin/tsconfig.json`、`web/admin/vitest.config.ts`、`web/admin/src/env.d.ts`、`web/admin/public/brand/logo.svg` 与 console 的同名文件内容相同（`vitest.config.ts` 的 `test.name` 为 `admin`）。

`web/admin/vite.config.ts`（与 console 的差异：`base`、代理、`outDir`、预列组件多了表格相关）：

```ts
import { fileURLToPath, URL } from 'node:url'
import vue from '@vitejs/plugin-vue'
import AutoImport from 'unplugin-auto-import/vite'
import Components from 'unplugin-vue-components/vite'
import { ElementPlusResolver } from 'unplugin-vue-components/resolvers'
import { defineConfig } from 'vite'

const epComponents = [
  'button', 'input', 'input-number', 'form', 'form-item', 'dialog', 'message', 'table', 'table-column', 'tag', 'card', 'select', 'option',
  'radio-group', 'radio-button', 'tabs', 'tab-pane', 'pagination', 'alert', 'skeleton', 'empty', 'descriptions', 'descriptions-item',
  'divider', 'link', 'result', 'checkbox', 'switch', 'date-picker', 'timeline', 'timeline-item', 'badge', 'tooltip', 'icon',
]

export default defineConfig({
  base: '/admin/',
  plugins: [
    vue(),
    AutoImport({ resolvers: [ElementPlusResolver()], dts: 'src/auto-imports.d.ts' }),
    Components({ resolvers: [ElementPlusResolver()], dts: 'src/components.d.ts' }),
  ],
  resolve: {
    alias: {
      '@spark/shared': fileURLToPath(new URL('../shared/src/index.ts', import.meta.url)),
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  server: { port: 5174, proxy: { '/admin/api': 'http://127.0.0.1:8081' } },
  optimizeDeps: { include: ['element-plus/es', ...epComponents.map((c) => `element-plus/es/components/${c}/style/css`)] },
  build: { outDir: fileURLToPath(new URL('../../internal/web/dist/admin', import.meta.url)), emptyOutDir: true },
})
```

`web/admin/index.html`：与 console 相同，`<title>Spark Cloud 管理后台</title>`，`<link rel="icon" href="/brand/logo.svg" />`（Vite 构建时按 `base` 改写为 `/admin/brand/logo.svg`）。

Run: `cd web && npm install`
Expected: 成功。

- [ ] **Step 2: 写失败的单测（U-12 与动态码单例）**

`web/admin/src/__tests__/menu.spec.ts`：

```ts
import { describe, expect, it } from 'vitest'
import { menu, menuFor } from '../menu'
import { routes } from '../router'

describe('menuFor (U-12)', () => {
  it.each([
    ['super', 13, []],
    ['ops', 8, ['orders', 'ledger', 'recharges', 'catalog', 'admins']],
    ['finance', 8, ['machines', 'pdus', 'kyc', 'catalog', 'admins']],
  ] as const)('%s 有 %d 项', (role, count, missing) => {
    const items = menuFor(role)
    expect(items).toHaveLength(count)
    for (const key of missing) expect(items.find((m) => m.key === key)).toBeUndefined()
  })

  it('菜单与路由 roles 一致（表驱动）', () => {
    for (const item of menu) {
      const route = routes.find((r) => r.path === item.path)
      expect(route, item.path).toBeDefined()
      expect([...(route!.meta?.roles ?? [])].sort()).toEqual([...item.roles].sort())
    }
  })
})
```

`web/admin/src/store/__tests__/totp.spec.ts`：

```ts
import { describe, expect, it } from 'vitest'
import { cancelTotp, promptTotp, submitTotp, totpState } from '../totp'

describe('promptTotp 单例', () => {
  it('提交返回去空白的动态码；取消返回 null', async () => {
    const p = promptTotp()
    expect(totpState.visible).toBe(true)
    totpState.code = '123 456'
    submitTotp()
    expect(await p).toBe('123456')
    expect(totpState.visible).toBe(false)
    const q = promptTotp('动态码错误或已使用过')
    expect(totpState.message).toBe('动态码错误或已使用过')
    cancelTotp()
    expect(await q).toBeNull()
  })
})
```

Run: `cd web && npm test`
Expected: admin 项目 FAIL（模块不存在）。

- [ ] **Step 3: 入口、会话、动态码、菜单、路由、布局**

`web/admin/src/api.ts`：

```ts
import { createHttp } from '@spark/shared'
import { promptTotp } from './store/totp'

const handlers = { unauthorized: () => {}, totpSetupRequired: () => {} }

export const api = createHttp({
  base: '/admin/api/v1',
  onUnauthorized: () => handlers.unauthorized(),
  promptTotp,
  onTotpSetupRequired: () => handlers.totpSetupRequired(),
})

export function onUnauthorized(fn: () => void) {
  handlers.unauthorized = fn
}
export function onTotpSetupRequired(fn: () => void) {
  handlers.totpSetupRequired = fn
}
```

`web/admin/src/store/totp.ts`：

```ts
import { reactive } from 'vue'

export const totpState = reactive({ visible: false, message: '', code: '' })
let resolver: ((v: string | null) => void) | null = null

/** 全局动态码弹框（单例）：请求层收到 totp_required 时调用；并发请求共用同一次输入。 */
export function promptTotp(message?: string): Promise<string | null> {
  totpState.message = message ?? ''
  totpState.code = ''
  totpState.visible = true
  return new Promise((resolve) => {
    resolver = resolve
  })
}

export function submitTotp() {
  const r = resolver
  resolver = null
  totpState.visible = false
  r?.(totpState.code.replace(/\s+/g, ''))
}

export function cancelTotp() {
  const r = resolver
  resolver = null
  totpState.visible = false
  r?.(null)
}
```

`web/admin/src/components/TotpDialog.vue`：

```vue
<script setup lang="ts">
import { cancelTotp, submitTotp, totpState } from '../store/totp'
</script>

<template>
  <el-dialog v-model="totpState.visible" title="安全验证" width="420" align-center :close-on-click-modal="false" :show-close="false" append-to-body data-testid="totp-dialog">
    <p>该操作需要输入验证器 App 中的 6 位动态码。</p>
    <el-alert v-if="totpState.message" type="error" :title="totpState.message" :closable="false" show-icon data-testid="totp-message" />
    <el-input v-model="totpState.code" data-testid="totp-input" inputmode="numeric" autocomplete="one-time-code" maxlength="7" placeholder="动态码" @keyup.enter="submitTotp" />
    <template #footer>
      <el-button data-testid="totp-cancel" @click="cancelTotp">取消</el-button>
      <el-button type="primary" data-testid="totp-ok" :disabled="totpState.code.replace(/\s+/g, '').length !== 6" @click="submitTotp">确定</el-button>
    </template>
  </el-dialog>
</template>
```

`web/admin/src/store/session.ts`：

```ts
import type { Admin, Role } from '@spark/shared'
import { ElMessage } from 'element-plus'
import { reactive } from 'vue'
import type { Router } from 'vue-router'
import { api, onTotpSetupRequired, onUnauthorized } from '../api'

export const session = reactive<{ status: 'unknown' | 'anon' | 'authed'; admin: Admin | null }>({ status: 'unknown', admin: null })

let probing: Promise<void> | null = null
export function probeSession(): Promise<void> {
  if (session.status !== 'unknown') return Promise.resolve()
  probing ??= api
    .get<Admin>('/me', undefined, { quiet: true })
    .then(
      (a) => setAdmin(a),
      () => {
        session.status = 'anon'
        session.admin = null
      },
    )
    .finally(() => {
      probing = null
    })
  return probing
}

export function setAdmin(a: Admin) {
  session.admin = a
  session.status = 'authed'
}

export function clearSession() {
  session.admin = null
  session.status = 'anon'
}

export const hasRole = (roles?: readonly Role[]) => !roles || (session.admin != null && roles.includes(session.admin.role))

let router: Router | null = null
let redirecting = false
export function bindRouter(r: Router) {
  router = r
  r.afterEach(() => {
    redirecting = false
  })
}

/** 401：清会话、提示一次、回登录页（登录页自身的 401 由页面处理：临时会话超时回到步骤一）。 */
export function handleUnauthorized() {
  const wasAuthed = session.status === 'authed'
  clearSession()
  if (!router || redirecting) return
  const current = router.currentRoute.value
  if (current.path === '/login') return
  redirecting = true
  if (wasAuthed) ElMessage.warning('登录已失效，请重新登录')
  void router.replace({ path: '/login', query: { redirect: current.fullPath } })
}

onUnauthorized(handleUnauthorized)
onTotpSetupRequired(() => {
  clearSession()
  ElMessage.warning('请先绑定动态码：重新输入密码后按提示绑定')
  void router?.replace('/login')
})
```

`web/admin/src/menu.ts`：

```ts
import type { Role } from '@spark/shared'

export const ALL_ROLES: readonly Role[] = ['super', 'ops', 'finance']
export const OPS_ROLES: readonly Role[] = ['super', 'ops']
export const FIN_ROLES: readonly Role[] = ['super', 'finance']
export const SUPER_ONLY: readonly Role[] = ['super']

export interface MenuItem {
  key: string
  path: string
  label: string
  roles: readonly Role[]
}

/** 侧边菜单（与路由 meta.roles 一致，由 menu.spec.ts 表驱动核对）。 */
export const menu: MenuItem[] = [
  { key: 'dashboard', path: '/', label: '仪表盘', roles: ALL_ROLES },
  { key: 'machines', path: '/machines', label: '机器', roles: OPS_ROLES },
  { key: 'pdus', path: '/pdus', label: 'PDU', roles: OPS_ROLES },
  { key: 'alerts', path: '/alerts', label: '告警', roles: ALL_ROLES },
  { key: 'instances', path: '/instances', label: '实例', roles: ALL_ROLES },
  { key: 'users', path: '/users', label: '用户', roles: ALL_ROLES },
  { key: 'kyc', path: '/kyc', label: '企业认证', roles: OPS_ROLES },
  { key: 'orders', path: '/orders', label: '订单', roles: FIN_ROLES },
  { key: 'ledger', path: '/ledger', label: '流水', roles: FIN_ROLES },
  { key: 'recharges', path: '/recharges', label: '充值单', roles: FIN_ROLES },
  { key: 'catalog', path: '/catalog', label: '价格与镜像', roles: SUPER_ONLY },
  { key: 'admins', path: '/admins', label: '管理员', roles: SUPER_ONLY },
  { key: 'audit', path: '/audit', label: '审计日志', roles: ALL_ROLES },
]

export function menuFor(role: Role): MenuItem[] {
  return menu.filter((m) => m.roles.includes(role))
}
```

`web/admin/src/router.ts`：

```ts
import { brand, type Role } from '@spark/shared'
import { createRouter, createWebHistory, type RouteRecordRaw } from 'vue-router'
import { ALL_ROLES, FIN_ROLES, OPS_ROLES, SUPER_ONLY } from './menu'
import { bindRouter, hasRole, probeSession, session } from './store/session'

declare module 'vue-router' {
  interface RouteMeta {
    title: string
    roles?: readonly Role[]
  }
}

export function safeRedirect(v: unknown): string | null {
  return typeof v === 'string' && v.startsWith('/') && !v.startsWith('//') ? v : null
}

export const routes: RouteRecordRaw[] = [
  { path: '/login', component: () => import('./views/LoginView.vue'), meta: { title: '登录' } },
  { path: '/', component: () => import('./views/DashboardView.vue'), meta: { title: '仪表盘', roles: ALL_ROLES } },
  { path: '/machines', component: () => import('./views/MachinesView.vue'), meta: { title: '机器', roles: OPS_ROLES } },
  { path: '/machines/:id', component: () => import('./views/MachineDetailView.vue'), meta: { title: '机器详情', roles: OPS_ROLES } },
  { path: '/pdus', component: () => import('./views/PdusView.vue'), meta: { title: 'PDU', roles: OPS_ROLES } },
  { path: '/alerts', component: () => import('./views/AlertsView.vue'), meta: { title: '告警', roles: ALL_ROLES } },
  { path: '/instances', component: () => import('./views/InstancesView.vue'), meta: { title: '实例', roles: ALL_ROLES } },
  { path: '/instances/:id', component: () => import('./views/InstanceDetailView.vue'), meta: { title: '实例详情', roles: ALL_ROLES } },
  { path: '/users', component: () => import('./views/UsersView.vue'), meta: { title: '用户', roles: ALL_ROLES } },
  { path: '/users/:id', component: () => import('./views/UserDetailView.vue'), meta: { title: '用户详情', roles: ALL_ROLES } },
  { path: '/kyc', component: () => import('./views/KycView.vue'), meta: { title: '企业认证审核', roles: OPS_ROLES } },
  { path: '/orders', component: () => import('./views/OrdersView.vue'), meta: { title: '订单', roles: FIN_ROLES } },
  { path: '/ledger', component: () => import('./views/LedgerView.vue'), meta: { title: '流水', roles: FIN_ROLES } },
  { path: '/recharges', component: () => import('./views/RechargesView.vue'), meta: { title: '充值单', roles: FIN_ROLES } },
  { path: '/catalog', component: () => import('./views/CatalogView.vue'), meta: { title: '价格与镜像', roles: SUPER_ONLY } },
  { path: '/admins', component: () => import('./views/AdminsView.vue'), meta: { title: '管理员', roles: SUPER_ONLY } },
  { path: '/audit', component: () => import('./views/AuditView.vue'), meta: { title: '审计日志', roles: ALL_ROLES } },
  { path: '/settings', component: () => import('./views/SettingsView.vue'), meta: { title: '个人设置', roles: ALL_ROLES } },
  { path: '/403', component: () => import('./views/ForbiddenView.vue'), meta: { title: '无权访问' } },
  { path: '/:pathMatch(.*)*', component: () => import('./views/NotFoundView.vue'), meta: { title: '页面不存在' } },
]

export const router = createRouter({ history: createWebHistory('/admin/'), routes })

router.beforeEach(async (to) => {
  await probeSession()
  if (to.path === '/login') return session.status === 'authed' ? safeRedirect(to.query.redirect) ?? '/' : true
  if (session.status !== 'authed') return { path: '/login', query: { redirect: to.fullPath } }
  if (to.meta.roles && !hasRole(to.meta.roles)) return { path: '/403', query: { from: to.fullPath } }
  return true
})

router.afterEach((to) => {
  document.title = `${to.meta.title} - ${brand.name} 管理后台`
})

bindRouter(router)
```

`web/admin/src/main.ts`、`web/admin/src/styles.css`：与 console 相同（样式不需要手机断点部分，保留 body 与 `.money-plus/.money-minus`，并加 `html { min-width: 1200px; }`）。

`web/admin/src/App.vue`：

```vue
<script setup lang="ts">
import { useRoute } from 'vue-router'
import TotpDialog from './components/TotpDialog.vue'
import AdminLayout from './layouts/AdminLayout.vue'

const route = useRoute()
const bare = ['/login', '/403']
</script>

<template>
  <router-view v-if="bare.includes(route.path)" />
  <AdminLayout v-else><router-view /></AdminLayout>
  <TotpDialog />
</template>
```

`web/admin/src/layouts/AdminLayout.vue`：

```vue
<script setup lang="ts">
import { brand, labelOf, roleLabel, type Paged } from '@spark/shared'
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api } from '../api'
import { menuFor } from '../menu'
import { clearSession, session } from '../store/session'

const route = useRoute()
const router = useRouter()
const COLLAPSE_KEY = 'sc_admin_sidebar_collapsed'
const collapsed = ref(localStorage.getItem(COLLAPSE_KEY) === '1')
function toggle() {
  collapsed.value = !collapsed.value
  localStorage.setItem(COLLAPSE_KEY, collapsed.value ? '1' : '0')
}
const items = computed(() => (session.admin ? menuFor(session.admin.role) : []))
const active = computed(() => items.value.find((m) => (m.path === '/' ? route.path === '/' : route.path === m.path || route.path.startsWith(m.path + '/')))?.key ?? '')
const logo = import.meta.env.BASE_URL + brand.logoUrl

// 未处理告警角标：60 秒刷新一次 total
const unresolved = ref(0)
let timer: ReturnType<typeof setInterval> | undefined
async function loadBadge() {
  try {
    unresolved.value = (await api.get<Paged<unknown>>('/alerts', { resolved: 'false', page_size: 1 })).total
  } catch {
    /* 角标失败不打扰用户 */
  }
}
onMounted(() => {
  void loadBadge()
  timer = setInterval(loadBadge, 60_000)
})
onUnmounted(() => clearInterval(timer))

async function logout() {
  try {
    await api.post('/auth/logout')
  } finally {
    clearSession()
    await router.replace('/login')
  }
}
</script>

<template>
  <div class="shell">
    <aside class="side" :class="{ collapsed }" data-testid="layout-side">
      <div class="logo"><img :src="logo" alt="" width="28" height="28" /><span v-if="!collapsed">{{ brand.name }}</span></div>
      <nav>
        <router-link v-for="m in items" :key="m.key" :to="m.path" class="menu-item" :class="{ active: active === m.key }" :data-testid="`menu-${m.key}`">
          <span class="label">{{ collapsed ? m.label.slice(0, 2) : m.label }}</span>
          <el-badge v-if="m.key === 'alerts' && unresolved > 0" :value="unresolved" :max="99" class="badge" data-testid="menu-alerts-badge" />
        </router-link>
      </nav>
      <button class="collapse" data-testid="layout-collapse" @click="toggle">{{ collapsed ? '»' : '«' }}</button>
    </aside>
    <div class="main">
      <header class="topbar">
        <span class="title">{{ route.meta.title }}</span>
        <span class="right">
          <span data-testid="layout-username">{{ session.admin?.username }}</span>
          <el-tag size="small" data-testid="layout-role">{{ labelOf(roleLabel, session.admin?.role) }}</el-tag>
          <router-link to="/settings" data-testid="layout-settings">个人设置</router-link>
          <el-button link data-testid="layout-logout" @click="logout">退出</el-button>
        </span>
      </header>
      <main class="content"><slot /></main>
    </div>
  </div>
</template>

<style scoped>
.shell { display: flex; min-height: 100vh; }
.side { width: 200px; background: #1f2d3d; color: #cfd8e3; display: flex; flex-direction: column; flex-shrink: 0; }
.side.collapsed { width: 64px; }
.logo { display: flex; align-items: center; gap: 8px; padding: 16px; color: #fff; font-weight: 600; }
.menu-item { display: flex; align-items: center; justify-content: space-between; padding: 10px 16px; color: #cfd8e3; text-decoration: none; }
.menu-item.active { background: var(--el-color-primary); color: #fff; }
.collapse { margin-top: auto; background: none; border: 0; color: #cfd8e3; padding: 12px; cursor: pointer; }
.main { flex: 1; display: flex; flex-direction: column; min-width: 0; }
.topbar { height: 56px; display: flex; align-items: center; justify-content: space-between; padding: 0 24px; background: #fff; border-bottom: 1px solid #ebeef5; }
.title { font-weight: 600; }
.right { display: flex; align-items: center; gap: 12px; }
.content { flex: 1; padding: 24px; }
</style>
```

- [ ] **Step 4: 登录三步、个人设置、403、404 与页面骨架**

`web/admin/src/views/LoginView.vue`：

```vue
<script setup lang="ts">
import { brand, isApiError, messageOf, type Admin, type AdminLoginResult } from '@spark/shared'
import { ElMessage } from 'element-plus'
import QRCode from 'qrcode'
import { nextTick, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api } from '../api'
import { safeRedirect } from '../router'
import { setAdmin } from '../store/session'

type Step = 'password' | 'setup' | 'totp' | 'recovery-codes'
const route = useRoute()
const router = useRouter()
const step = ref<Step>('password')
const busy = ref(false)
const timeout = ref(false)
const username = ref('')
const password = ref('')
const otpauthUri = ref('')
const secret = ref('')
const setupCode = ref('')
const qrCanvas = ref<HTMLCanvasElement | null>(null)
const recoveryCodes = ref<string[]>([])
const saved = ref(false)
const totpCode = ref('')
const useRecovery = ref(false)
const recoveryInput = ref('')
const logo = import.meta.env.BASE_URL + brand.logoUrl

function fail(e: unknown) {
  // 临时会话（密码通过后 5 分钟）过期：回到步骤一
  if (isApiError(e) && e.status === 401) {
    step.value = 'password'
    timeout.value = true
    password.value = ''
    return
  }
  ElMessage.error(messageOf(e))
}

async function enter(a: Admin) {
  setAdmin(a)
  await router.replace(safeRedirect(route.query.redirect) ?? '/')
}

async function submitPassword() {
  busy.value = true
  timeout.value = false
  try {
    const out = await api.post<AdminLoginResult>('/auth/login', { username: username.value.trim(), password: password.value })
    if (out.next === 'totp_setup') {
      otpauthUri.value = out.otpauth_uri
      secret.value = new URL(out.otpauth_uri).searchParams.get('secret') ?? ''
      step.value = 'setup'
    } else step.value = 'totp'
  } catch (e) {
    ElMessage.error(messageOf(e)) // invalid_credentials / account_locked / rate_limited 原文
  } finally {
    busy.value = false
  }
}

watch([step, otpauthUri], async ([s, uri]) => {
  if (s !== 'setup' || !uri) return
  await nextTick()
  if (qrCanvas.value) await QRCode.toCanvas(qrCanvas.value, uri, { width: 200, margin: 1 })
})

async function submitSetup() {
  busy.value = true
  try {
    const out = await api.post<{ admin: Admin; recovery_codes: string[] }>('/auth/totp/setup', { code: setupCode.value.replace(/\s+/g, '') })
    recoveryCodes.value = out.recovery_codes
    setAdmin(out.admin)
    step.value = 'recovery-codes' // 恢复码只展示这一次
  } catch (e) {
    fail(e)
  } finally {
    busy.value = false
  }
}

async function submitTotp() {
  busy.value = true
  try {
    const body = useRecovery.value ? { recovery_code: recoveryInput.value.trim() } : { code: totpCode.value.replace(/\s+/g, '') }
    const out = await api.post<{ admin: Admin }>('/auth/totp', body)
    await enter(out.admin)
  } catch (e) {
    fail(e)
  } finally {
    busy.value = false
  }
}

async function copy(text: string, tip: string) {
  await navigator.clipboard.writeText(text)
  ElMessage.success(tip)
}
</script>

<template>
  <div class="login-page">
    <el-card class="card">
      <h1 class="brand"><img :src="logo" alt="" width="28" height="28" /> {{ brand.name }} 管理后台</h1>
      <el-alert v-if="timeout" type="warning" title="登录已超时，请重新输入密码" :closable="false" show-icon data-testid="login-timeout" />

      <el-form v-if="step === 'password'" label-position="top" @submit.prevent="submitPassword">
        <el-form-item label="账号"><el-input v-model="username" data-testid="login-username" autocomplete="username" /></el-form-item>
        <el-form-item label="密码"><el-input v-model="password" type="password" show-password data-testid="login-password" autocomplete="current-password" /></el-form-item>
        <el-button type="primary" native-type="submit" class="full" :loading="busy" data-testid="login-submit">下一步</el-button>
      </el-form>

      <div v-else-if="step === 'setup'" data-testid="login-setup">
        <p>首次登录需要绑定动态码：用验证器 App（如 Google Authenticator）扫描二维码，或手动输入密钥。</p>
        <canvas ref="qrCanvas" width="200" height="200" data-testid="login-setup-qr" />
        <div class="secret-row">
          <code data-testid="login-setup-secret">{{ secret }}</code>
          <el-button size="small" data-testid="login-setup-copy-secret" @click="copy(secret, '密钥已复制')">复制</el-button>
        </div>
        <el-form label-position="top" @submit.prevent="submitSetup">
          <el-form-item label="输入 App 显示的 6 位动态码"><el-input v-model="setupCode" data-testid="login-setup-code" inputmode="numeric" maxlength="7" /></el-form-item>
          <el-button type="primary" native-type="submit" class="full" :loading="busy" data-testid="login-setup-submit">绑定</el-button>
        </el-form>
      </div>

      <div v-else-if="step === 'recovery-codes'">
        <el-alert type="warning" title="请妥善保存以下 8 个恢复码，每个只能使用一次；此页面关闭后不再显示。" :closable="false" show-icon />
        <ul class="codes" data-testid="login-recovery-codes">
          <li v-for="c in recoveryCodes" :key="c" data-testid="login-recovery-code">{{ c }}</li>
        </ul>
        <el-button data-testid="login-recovery-copy" @click="copy(recoveryCodes.join('\n'), '恢复码已复制')">复制全部</el-button>
        <el-checkbox v-model="saved" data-testid="login-recovery-saved">我已妥善保存这些恢复码</el-checkbox>
        <el-button type="primary" class="full" :disabled="!saved" data-testid="login-enter" @click="router.replace(safeRedirect(route.query.redirect) ?? '/')">进入后台</el-button>
      </div>

      <el-form v-else label-position="top" @submit.prevent="submitTotp">
        <template v-if="!useRecovery">
          <el-form-item label="动态码"><el-input v-model="totpCode" data-testid="login-totp-code" inputmode="numeric" autocomplete="one-time-code" maxlength="7" /></el-form-item>
          <p class="hint" data-testid="login-totp-hint">如刚用过该动态码，请等待下一个（同一动态码 30 秒内不能重复使用）</p>
          <el-button type="primary" native-type="submit" class="full" :loading="busy" data-testid="login-totp-submit">登录</el-button>
          <el-button link data-testid="login-use-recovery" @click="useRecovery = true">改用恢复码</el-button>
        </template>
        <template v-else>
          <el-form-item label="恢复码（格式 xxxxx-xxxxx）"><el-input v-model="recoveryInput" data-testid="login-recovery-input" /></el-form-item>
          <el-button type="primary" native-type="submit" class="full" :loading="busy" data-testid="login-recovery-submit">登录</el-button>
          <el-button link data-testid="login-use-totp" @click="useRecovery = false">改用动态码</el-button>
        </template>
      </el-form>
    </el-card>
  </div>
</template>

<style scoped>
.login-page { min-height: 100vh; display: flex; align-items: center; justify-content: center; background: #f5f7fa; }
.card { width: 440px; }
.brand { display: flex; align-items: center; gap: 8px; font-size: 18px; margin: 0 0 16px; }
.full { width: 100%; margin-top: 8px; }
.secret-row { display: flex; align-items: center; gap: 8px; margin: 8px 0 16px; }
.codes { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; columns: 2; margin: 16px 0; padding-left: 20px; }
.hint { color: #909399; font-size: 12px; }
</style>
```

（`setAdmin` 已在 `submitSetup` 成功时调用，「进入后台」只需跳转。）

`web/admin/src/views/SettingsView.vue`：

```vue
<script setup lang="ts">
import { messageOf } from '@spark/shared'
import { ElMessage } from 'element-plus'
import { reactive, ref } from 'vue'
import { api } from '../api'

const form = reactive({ old: '', next: '', confirm: '' })
const errors = reactive({ next: '', confirm: '' })
const busy = ref(false)

async function submit() {
  errors.next = form.next.length >= 12 ? '' : '新密码至少 12 位'
  errors.confirm = form.next === form.confirm ? '' : '两次输入的新密码不一致'
  if (errors.next || errors.confirm) return
  busy.value = true
  try {
    await api.post('/me/password', { old: form.old, new: form.next })
    ElMessage.success('密码已修改')
    form.old = form.next = form.confirm = ''
  } catch (e) {
    ElMessage.error(messageOf(e)) // wrong_password → 「原密码不正确」
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <el-card class="settings">
    <h2>修改密码</h2>
    <el-form label-position="top" @submit.prevent="submit">
      <el-form-item label="原密码"><el-input v-model="form.old" type="password" show-password data-testid="settings-old" autocomplete="current-password" /></el-form-item>
      <el-form-item label="新密码（至少 12 位）" :error="errors.next" :show-message="false">
        <el-input v-model="form.next" type="password" show-password data-testid="settings-new" autocomplete="new-password" />
        <span v-if="errors.next" class="field-error" data-testid="settings-new-error">{{ errors.next }}</span>
      </el-form-item>
      <el-form-item label="确认新密码" :error="errors.confirm" :show-message="false">
        <el-input v-model="form.confirm" type="password" show-password data-testid="settings-confirm" autocomplete="new-password" />
        <span v-if="errors.confirm" class="field-error" data-testid="settings-confirm-error">{{ errors.confirm }}</span>
      </el-form-item>
      <el-button type="primary" native-type="submit" :loading="busy" data-testid="settings-submit">保存</el-button>
    </el-form>
  </el-card>
</template>

<style scoped>
.settings { max-width: 480px; }
.field-error { color: var(--el-color-danger); font-size: 12px; }
</style>
```

`web/admin/src/views/ForbiddenView.vue`：

```vue
<template>
  <el-result icon="error" title="无权访问该页面" sub-title="当前角色没有该功能的权限，如有需要请联系超级管理员" data-testid="forbidden-page">
    <template #extra><router-link to="/"><el-button type="primary" data-testid="forbidden-home">回到仪表盘</el-button></router-link></template>
  </el-result>
</template>
```

`web/admin/src/views/NotFoundView.vue`：`el-result` 标题「页面不存在」，`data-testid="admin-notfound"`，按钮回 `/`。

页面骨架（Dashboard / Machines / MachineDetail / Pdus / Alerts / Instances / InstanceDetail / Users / UserDetail / Kyc / Orders / Ledger / Recharges / Catalog / Admins / Audit 共 16 个，每个 `<template><el-card><h2>页面名</h2></el-card></template>`，页面名取路由 `title`）。

Run: `cd web && npm test && npm run build`
Expected: Vitest PASS（menu 2、totp 1）；`vue-tsc` 与构建通过，产物在 `internal/web/dist/admin/`；提交生成的 `web/admin/src/auto-imports.d.ts`、`components.d.ts`。

- [ ] **Step 5: 端到端用例**

`web/e2e/admin/auth.spec.ts`：

```ts
import { expect, test } from '@playwright/test'
import { createAdmin, loginAdmin, totpNow } from '../helpers/admin'
import { toast } from '../helpers/console'
import { hooks } from '../helpers/hooks'
import { totp } from '../helpers/totp'

test('A-AUTH-01 首次登录绑定动态码：二维码、密钥、恢复码、勾选后进入', async ({ page }) => {
  const admin = await createAdmin('super', false)
  await page.goto('/admin/login')
  await page.getByTestId('login-username').fill(admin.username)
  await page.getByTestId('login-password').fill(admin.password)
  await page.getByTestId('login-submit').click()
  await expect(page.getByTestId('login-setup-qr')).toBeVisible()
  const secret = (await page.getByTestId('login-setup-secret').textContent())!.trim()
  expect(secret).toMatch(/^[A-Z2-7]{16,}$/)
  await page.getByTestId('login-setup-copy-secret').click()
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(secret)
  await page.getByTestId('login-setup-code').fill(await totpNow(secret))
  await page.getByTestId('login-setup-submit').click()
  const codes = page.getByTestId('login-recovery-code')
  await expect(codes).toHaveCount(8)
  const texts = await codes.allTextContents()
  for (const c of texts) expect(c.trim()).toMatch(/^[a-z0-9]{5}-[a-z0-9]{5}$/)
  await page.getByTestId('login-recovery-copy').click()
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(texts.map((t) => t.trim()).join('\n'))
  await expect(page.getByTestId('login-enter')).toBeDisabled()
  await page.getByTestId('login-recovery-saved').click()
  await expect(page.getByTestId('login-enter')).toBeEnabled()
  await page.getByTestId('login-enter').click()
  await expect(page).toHaveURL(/\/admin\/$/)
  await expect(page.getByTestId('layout-username')).toHaveText(admin.username)
  await page.reload()
  await expect(page.getByTestId('layout-username')).toHaveText(admin.username)
  await expect(page.getByTestId('login-recovery-codes')).toHaveCount(0)
})

test('A-AUTH-02 动态码登录，刷新仍登录', async ({ page }) => {
  const admin = await createAdmin('ops')
  await loginAdmin(page, admin)
  await expect(page).toHaveURL(/\/admin\/$/)
  await page.reload()
  await expect(page.getByTestId('layout-username')).toHaveText(admin.username)
})

test('A-AUTH-03 恢复码登录成功；同一恢复码再次使用失败', async ({ page }) => {
  const admin = await createAdmin('ops')
  const code = admin.recovery_codes![0]
  const loginWithRecovery = async () => {
    await page.goto('/admin/login')
    await page.getByTestId('login-username').fill(admin.username)
    await page.getByTestId('login-password').fill(admin.password)
    await page.getByTestId('login-submit').click()
    await page.getByTestId('login-use-recovery').click()
    await page.getByTestId('login-recovery-input').fill(code)
    await page.getByTestId('login-recovery-submit').click()
  }
  await loginWithRecovery()
  await expect(page.getByTestId('layout-username')).toHaveText(admin.username)
  await page.getByTestId('layout-logout').click()
  await expect(page).toHaveURL(/\/admin\/login$/)
  await loginWithRecovery()
  await toast(page, '动态码错误或已使用过')
  await expect(page).toHaveURL(/\/admin\/login/)
})

test('A-AUTH-04 密码错误与动态码错误提示', async ({ page }) => {
  const admin = await createAdmin('finance')
  await page.goto('/admin/login')
  await page.getByTestId('login-username').fill(admin.username)
  await page.getByTestId('login-password').fill('definitely-wrong-password')
  await page.getByTestId('login-submit').click()
  await toast(page, '用户名或密码错误')
  await page.getByTestId('login-password').fill(admin.password)
  await page.getByTestId('login-submit').click()
  await page.getByTestId('login-totp-code').fill('000000')
  await page.getByTestId('login-totp-submit').click()
  await toast(page, '动态码错误或已使用过')
  await expect(page.getByTestId('login-totp-hint')).toContainText('如刚用过该动态码，请等待下一个')
})

test('A-AUTH-05 连续 5 次失败后锁定', async ({ page }) => {
  const admin = await createAdmin('ops')
  await page.goto('/admin/login')
  for (let i = 0; i < 5; i++) {
    await page.getByTestId('login-username').fill(admin.username)
    await page.getByTestId('login-password').fill(`wrong-password-${i}`)
    await page.getByTestId('login-submit').click()
    await toast(page, '用户名或密码错误')
  }
  await page.getByTestId('login-password').fill(admin.password)
  await page.getByTestId('login-submit').click()
  await toast(page, '登录失败次数过多')
})

test('A-AUTH-06 未登录访问受保护页与退出后再访问都回登录页', async ({ page }) => {
  const admin = await createAdmin('super')
  await page.goto('/admin/machines')
  await expect(page).toHaveURL(/\/admin\/login\?redirect=(%2F|\/)machines$/)
  await page.getByTestId('login-username').fill(admin.username)
  await page.getByTestId('login-password').fill(admin.password)
  await page.getByTestId('login-submit').click()
  await page.getByTestId('login-totp-code').fill(await totpNow(admin.totp_secret!))
  await page.getByTestId('login-totp-submit').click()
  await expect(page).toHaveURL(/\/admin\/machines$/)
  await page.getByTestId('layout-logout').click()
  await expect(page).toHaveURL(/\/admin\/login$/)
  await page.goto('/admin/machines')
  await expect(page).toHaveURL(/\/admin\/login\?redirect=/)
})

test('A-AUTH-07 临时会话超时：6 分钟后提交动态码回到步骤一', async ({ page }) => {
  const admin = await createAdmin('ops')
  await page.goto('/admin/login')
  await page.getByTestId('login-username').fill(admin.username)
  await page.getByTestId('login-password').fill(admin.password)
  await page.getByTestId('login-submit').click()
  await expect(page.getByTestId('login-totp-code')).toBeVisible()
  const now = await hooks.advance(360)
  await page.getByTestId('login-totp-code').fill(totp(admin.totp_secret!, Math.floor(now.getTime() / 1000)))
  await page.getByTestId('login-totp-submit').click()
  await expect(page.getByTestId('login-timeout')).toHaveText('登录已超时，请重新输入密码')
  await expect(page.getByTestId('login-password')).toBeVisible()
})

test('A-AUTH-08 会话过期：推进 13 小时后任意请求跳登录', async ({ page }) => {
  const admin = await createAdmin('ops')
  await loginAdmin(page, admin)
  await hooks.advance(46800)
  await page.getByTestId('menu-instances').click()
  await expect(page).toHaveURL(/\/admin\/login\?redirect=/)
  await toast(page, '登录已失效，请重新登录')
})
```

`web/e2e/admin/nav.spec.ts`：

```ts
import { expect, test } from '@playwright/test'
import { createAdmin, loginAdmin } from '../helpers/admin'

test('A-NAV-01 菜单按角色过滤', async ({ page }) => {
  const cases: [('super' | 'ops' | 'finance'), number, string[]][] = [
    ['super', 13, []],
    ['ops', 8, ['orders', 'ledger', 'recharges', 'catalog', 'admins']],
    ['finance', 8, ['machines', 'pdus', 'kyc', 'catalog', 'admins']],
  ]
  for (const [role, count, missing] of cases) {
    const admin = await createAdmin(role)
    await loginAdmin(page, admin)
    await expect(page.getByTestId('layout-role')).toHaveText({ super: '超级管理员', ops: '运维', finance: '财务' }[role])
    await expect(page.getByTestId(/^menu-(?!alerts-badge)/)).toHaveCount(count)
    for (const key of missing) await expect(page.getByTestId(`menu-${key}`)).toHaveCount(0)
    await page.getByTestId('layout-logout').click()
    await expect(page).toHaveURL(/\/admin\/login$/)
  }
})

test('A-NAV-02 无权限路由显示 403 页', async ({ page }) => {
  const admin = await createAdmin('finance')
  await loginAdmin(page, admin)
  await page.goto('/admin/machines')
  await expect(page.getByTestId('forbidden-page')).toContainText('无权访问该页面')
  await page.getByTestId('forbidden-home').click()
  await expect(page).toHaveURL(/\/admin\/$/)
})
```

`web/e2e/admin/settings.spec.ts`：

```ts
import { expect, test } from '@playwright/test'
import { createAdmin, loginAdmin } from '../helpers/admin'
import { toast } from '../helpers/console'

test('A-SET-01 修改密码：原密码错误提示；成功后用新密码登录', async ({ page }) => {
  const admin = await createAdmin('ops')
  await loginAdmin(page, admin)
  await page.getByTestId('layout-settings').click()
  await expect(page).toHaveURL(/\/admin\/settings$/)
  const newPassword = 'new-e2e-password-7890'
  await page.getByTestId('settings-old').fill('not-the-old-password')
  await page.getByTestId('settings-new').fill(newPassword)
  await page.getByTestId('settings-confirm').fill(newPassword + 'x')
  await page.getByTestId('settings-submit').click()
  await expect(page.getByTestId('settings-confirm-error')).toHaveText('两次输入的新密码不一致')
  await page.getByTestId('settings-confirm').fill(newPassword)
  await page.getByTestId('settings-submit').click()
  await toast(page, '原密码不正确')
  await page.getByTestId('settings-old').fill(admin.password)
  await page.getByTestId('settings-submit').click()
  await toast(page, '密码已修改')
  await page.getByTestId('layout-logout').click()
  await loginAdmin(page, { ...admin, password: newPassword })
})
```

Run: `make web && cd web && npx playwright test --project=admin admin/auth.spec.ts admin/nav.spec.ts admin/settings.spec.ts`
Expected: 11 passed。

- [ ] **Step 6: 全量验证并提交**

Run: `go vet ./... && go test ./... && cd web && npm test && npm run build`
Expected: 全部 PASS。

```bash
git add web/package.json web/package-lock.json web/vitest.config.ts web/admin web/e2e/admin
git commit -m "feat(admin): app shell with role-aware menu, three-step login with TOTP setup, global TOTP prompt, settings; e2e A-AUTH/A-NAV/A-SET

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---
### Task 16: 后台仪表盘、告警、审计日志

**Files:**
- Create: `web/admin/src/composables/usePaged.ts`、`web/admin/src/store/alerts.ts`、`web/admin/src/components/{DateRange,IdLink}.vue`
- Modify: `web/admin/src/layouts/AdminLayout.vue`（角标改用 `store/alerts.ts`）
- Replace: `web/admin/src/views/{DashboardView,AlertsView,AuditView}.vue`
- Create: `web/admin/src/composables/__tests__/usePaged.spec.ts`
- Modify: `web/e2e/helpers/time.ts`（追加 `cstDate`）
- Create: `web/e2e/admin/{dashboard,alerts,audit}.spec.ts`

**Interfaces:**
- Consumes: Task 15 的 `api`、`session`、布局；Task 2 的 `actor_name`；shared 的 `labels`、`fmtYuan`、`fmtTime`。
- Produces（后续后台任务依赖）:
  - `usePaged<T>(fetcher: (page, pageSize) => Promise<Paged<T>>, pageSize = 20)` → `{ items, total, page, size, loading, load(), reset() }`；`page / size` 变化自动加载；请求失败 `ElMessage.error(messageOf(e))`（`forbidden` → 「无权执行该操作」）。
  - `store/alerts.ts`：`unresolvedAlerts: Ref<number>`、`refreshUnresolved()`（`GET /alerts?resolved=false&page_size=1` 取 `total`）。
  - `DateRange.vue`：props `modelValue: { from: string; to: string }`（`YYYY-MM-DD`，原样发给后端）、`prefix`（testid 前缀 → `<prefix>-from`、`<prefix>-to`，两个原生 `input[type=date]`，`to` 的 `min` 为 `from`）。
  - `IdLink.vue`：props `kind: 'machine' | 'instance' | 'user'`、`id`、`disabled?`（finance 看机器只显示 ID 不可点）→ 渲染 `router-link` 到 `/machines/:id` / `/instances/:id` / `/users/:id`，testid 由调用方传 `data-testid`。
  - 仪表盘 testid：`dash-machines-total`、`dash-machines-online`、`dash-machine-<idle|allocated|maintenance|faulty>`、`dash-instance-<creating|running|stopped|wiping|released|failed>`、`dash-recharge-today`、`dash-recharge-month`、`dash-order-today`、`dash-order-month`、`dash-chart`（SVG）、`dash-bar`（30 个 `rect`，各含 `<title>yyyy-MM-dd 出租率 x.x%</title>`）；60 秒刷新。
  - 告警 testid：`alerts-filter-unresolved`、`alerts-filter-all`、`alert-row`、`alert-level`、`alert-kind`、`alert-message`、`alert-machine`、`alert-instance`、`alert-time`、`alert-resolved-at`、`alert-resolve`；`alerts-pager`。
  - 审计 testid：`audit-actor`、`audit-action`（`el-select` filterable + allow-create，选项 `auditActions`）、`audit-target-type`、`audit-target-id`、`audit-from` / `audit-to`、`audit-search`、`audit-reset`、`audit-row`、`audit-time`、`audit-actor-cell`（`actor_name`，空则 `#actor_id`，`system` 显示「系统」）、`audit-action-cell`、`audit-target`（可点击跳机器 / 实例 / 用户）、`audit-ip`、展开行 `audit-diff`（`<pre>` 两空格缩进 JSON）、`audit-pager`。
  - e2e helper `cstDate(iso): string`（`yyyy-MM-dd`）。
  - 覆盖：A-DASH-01、A-ALT-01、A-AUD-01。

- [ ] **Step 1: 写失败的单测**

`web/admin/src/composables/__tests__/usePaged.spec.ts`：

```ts
import { nextTick } from 'vue'
import { describe, expect, it, vi } from 'vitest'

const { error } = vi.hoisted(() => ({ error: vi.fn() }))
vi.mock('element-plus', () => ({ ElMessage: { error, success: vi.fn(), warning: vi.fn() } }))

import { ApiError } from '@spark/shared'
import { usePaged } from '../usePaged'

describe('usePaged', () => {
  it('翻页自动加载，reset 回到第一页', async () => {
    const fetcher = vi.fn(async (page: number, size: number) => ({ items: [`p${page}s${size}`], total: 45 }))
    const list = usePaged(fetcher, 20)
    await list.load()
    expect(list.items.value).toEqual(['p1s20'])
    expect(list.total.value).toBe(45)
    list.page.value = 3
    await nextTick()
    await vi.waitFor(() => expect(fetcher).toHaveBeenLastCalledWith(3, 20))
    list.reset()
    await vi.waitFor(() => expect(list.page.value).toBe(1))
    expect(fetcher).toHaveBeenLastCalledWith(1, 20)
  })
  it('请求失败提示原文', async () => {
    const list = usePaged(async () => {
      throw new ApiError(403, 'forbidden', '无权执行该操作')
    })
    await list.load()
    expect(error).toHaveBeenCalledWith('无权执行该操作')
    expect(list.items.value).toEqual([])
  })
})
```

Run: `cd web && npm test`
Expected: FAIL（模块不存在）。

- [ ] **Step 2: 通用件**

`web/admin/src/composables/usePaged.ts`：

```ts
import { messageOf, type Paged } from '@spark/shared'
import { ElMessage } from 'element-plus'
import { ref, watch, type Ref } from 'vue'

/** 后台列表通用分页：page / page_size 变化自动加载；错误用原文提示（forbidden → 无权执行该操作）。 */
export function usePaged<T>(fetcher: (page: number, pageSize: number) => Promise<Paged<T>>, pageSize = 20) {
  const items = ref<T[]>([]) as Ref<T[]>
  const total = ref(0)
  const page = ref(1)
  const size = ref(pageSize)
  const loading = ref(false)

  async function load() {
    loading.value = true
    try {
      const out = await fetcher(page.value, size.value)
      items.value = out.items
      total.value = out.total
    } catch (e) {
      ElMessage.error(messageOf(e))
    } finally {
      loading.value = false
    }
  }
  watch([page, size], () => void load())
  /** 筛选条件变化：回到第一页并重新加载 */
  function reset() {
    if (page.value === 1) void load()
    else page.value = 1
  }
  return { items, total, page, size, loading, load, reset }
}
```

`web/admin/src/store/alerts.ts`：

```ts
import type { Paged } from '@spark/shared'
import { ref } from 'vue'
import { api } from '../api'

export const unresolvedAlerts = ref(0)

export async function refreshUnresolved() {
  try {
    unresolvedAlerts.value = (await api.get<Paged<unknown>>('/alerts', { resolved: 'false', page_size: 1 })).total
  } catch {
    /* 角标失败不打扰用户 */
  }
}
```

`AdminLayout.vue`：删除本地的 `unresolved` / `loadBadge`，改为 `import { refreshUnresolved, unresolvedAlerts } from '../store/alerts'`，`onMounted(() => { void refreshUnresolved(); timer = setInterval(refreshUnresolved, 60_000) })`，模板里 `unresolved` 改为 `unresolvedAlerts`。

`web/admin/src/components/DateRange.vue`：

```vue
<script setup lang="ts">
const model = defineModel<{ from: string; to: string }>({ required: true })
defineProps<{ prefix: string }>()
</script>

<template>
  <!-- 原生日期输入：用户选的日期按 YYYY-MM-DD 原样发给后端（后端按北京时间解释，to 含当天） -->
  <span class="range">
    <input v-model="model.from" type="date" class="el-input__inner native-date" :data-testid="`${prefix}-from`" :max="model.to || undefined" />
    <span class="sep">至</span>
    <input v-model="model.to" type="date" class="el-input__inner native-date" :data-testid="`${prefix}-to`" :min="model.from || undefined" />
  </span>
</template>

<style scoped>
.range { display: inline-flex; align-items: center; gap: 6px; }
.native-date { height: 32px; border: 1px solid #dcdfe6; border-radius: 4px; padding: 0 8px; font-size: 14px; background: #fff; }
.sep { color: #909399; }
</style>
```

`web/admin/src/components/IdLink.vue`：

```vue
<script setup lang="ts">
import { computed } from 'vue'

const props = defineProps<{ kind: 'machine' | 'instance' | 'user'; id: string | number | null | undefined; disabled?: boolean }>()
const to = computed(() => ({ machine: '/machines/', instance: '/instances/', user: '/users/' })[props.kind] + String(props.id))
</script>

<template>
  <span v-if="props.id == null || props.id === ''">—</span>
  <span v-else-if="props.disabled" class="mono">{{ props.id }}</span>
  <router-link v-else :to="to" class="mono">{{ props.id }}</router-link>
</template>

<style scoped>
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
</style>
```

- [ ] **Step 3: 三个页面**

`web/admin/src/views/DashboardView.vue`：

```vue
<script setup lang="ts">
import { fmtYuan, instanceStateLabel, machineStateLabel, messageOf, type Dashboard } from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { api } from '../api'

const data = ref<Dashboard | null>(null)
const machineStates = ['idle', 'allocated', 'maintenance', 'faulty'] as const
const instanceStates = ['creating', 'running', 'stopped', 'wiping', 'released', 'failed'] as const

async function load() {
  try {
    data.value = await api.get<Dashboard>('/dashboard')
  } catch (e) {
    ElMessage.error(messageOf(e))
  }
}
let timer: ReturnType<typeof setInterval> | undefined
onMounted(() => {
  void load()
  timer = setInterval(load, 60_000)
})
onUnmounted(() => clearInterval(timer))

// 近 30 天出租率柱状图：自绘 SVG，宽 600 高 160，每根柱宽 14 间距 6
const W = 600
const H = 160
const bars = computed(() =>
  (data.value?.utilization ?? []).map((u, i) => {
    const h = Math.round(u.rate * (H - 20))
    return { x: i * 20 + 4, y: H - h, h, date: u.date, pct: (u.rate * 100).toFixed(1) }
  }),
)
</script>

<template>
  <div v-if="data" class="dash">
    <div class="grid">
      <el-card header="机器">
        <div class="big"><span data-testid="dash-machines-total">{{ data.machines.total }}</span> 台 · 在线 <span data-testid="dash-machines-online">{{ data.machines.online }}</span></div>
        <ul class="kv">
          <li v-for="s in machineStates" :key="s"><span>{{ machineStateLabel[s] }}</span><b :data-testid="`dash-machine-${s}`">{{ data.machines.by_state[s] ?? 0 }}</b></li>
        </ul>
      </el-card>
      <el-card header="实例">
        <ul class="kv">
          <li v-for="s in instanceStates" :key="s"><span>{{ instanceStateLabel[s] }}</span><b :data-testid="`dash-instance-${s}`">{{ data.instances[s] ?? 0 }}</b></li>
        </ul>
      </el-card>
      <el-card header="充值金额">
        <ul class="kv">
          <li><span>今日</span><b data-testid="dash-recharge-today">{{ fmtYuan(data.recharge_today) }}</b></li>
          <li><span>本月</span><b data-testid="dash-recharge-month">{{ fmtYuan(data.recharge_month) }}</b></li>
        </ul>
      </el-card>
      <el-card header="下单金额">
        <ul class="kv">
          <li><span>今日</span><b data-testid="dash-order-today">{{ fmtYuan(data.order_today) }}</b></li>
          <li><span>本月</span><b data-testid="dash-order-month">{{ fmtYuan(data.order_month) }}</b></li>
        </ul>
      </el-card>
    </div>
    <el-card header="近 30 天出租率（最后一根为今天，截至当前）">
      <svg :viewBox="`0 0 ${W} ${H}`" class="chart" data-testid="dash-chart" role="img" aria-label="近 30 天出租率">
        <line x1="0" :y1="H - 0.5" :x2="W" :y2="H - 0.5" stroke="#dcdfe6" />
        <rect v-for="b in bars" :key="b.date" :x="b.x" :y="b.y" width="14" :height="b.h" fill="var(--el-color-primary)" data-testid="dash-bar" :data-date="b.date" :data-pct="b.pct">
          <title>{{ b.date }} 出租率 {{ b.pct }}%</title>
        </rect>
      </svg>
    </el-card>
  </div>
  <el-skeleton v-else :rows="6" animated />
</template>

<style scoped>
.dash { display: grid; gap: 16px; }
.grid { display: grid; grid-template-columns: repeat(4, 1fr); gap: 16px; }
.big { font-size: 20px; font-weight: 600; margin-bottom: 8px; }
.kv { list-style: none; margin: 0; padding: 0; display: grid; gap: 6px; }
.kv li { display: flex; justify-content: space-between; }
.chart { width: 100%; height: 200px; }
</style>
```

`web/admin/src/views/AlertsView.vue`：

```vue
<script setup lang="ts">
import { alertKindLabel, alertLevelLabel, fmtTime, labelOf, messageOf, type Alert, type Paged } from '@spark/shared'
import { ElMessage } from 'element-plus'
import { onMounted, ref, watch } from 'vue'
import { api } from '../api'
import IdLink from '../components/IdLink.vue'
import { usePaged } from '../composables/usePaged'
import { refreshUnresolved } from '../store/alerts'

const scope = ref<'unresolved' | 'all'>('unresolved')
const list = usePaged<Alert>((page, size) => api.get<Paged<Alert>>('/alerts', { resolved: scope.value === 'unresolved' ? 'false' : undefined, page, page_size: size }))
watch(scope, () => list.reset())
onMounted(list.load)

async function resolve(a: Alert) {
  try {
    await api.post(`/alerts/${a.id}/resolve`, {})
    ElMessage.success('已标记为已处理')
    await Promise.all([list.load(), refreshUnresolved()])
  } catch (e) {
    ElMessage.error(messageOf(e))
  }
}
</script>

<template>
  <el-card>
    <el-radio-group v-model="scope" class="toolbar">
      <el-radio-button value="unresolved" data-testid="alerts-filter-unresolved">未处理</el-radio-button>
      <el-radio-button value="all" data-testid="alerts-filter-all">全部</el-radio-button>
    </el-radio-group>
    <el-table :data="list.items.value" v-loading="list.loading.value" row-key="id">
      <el-table-column label="级别" width="90">
        <template #default="{ row }"><el-tag :type="row.level === 'critical' ? 'danger' : 'warning'" data-testid="alert-level">{{ labelOf(alertLevelLabel, row.level) }}</el-tag></template>
      </el-table-column>
      <el-table-column label="类型" width="130"><template #default="{ row }"><span data-testid="alert-kind">{{ labelOf(alertKindLabel, row.kind) }}</span></template></el-table-column>
      <el-table-column label="内容" min-width="240"><template #default="{ row }"><span data-testid="alert-message">{{ row.message }}</span></template></el-table-column>
      <el-table-column label="机器" width="100"><template #default="{ row }"><IdLink kind="machine" :id="row.machine_id" data-testid="alert-machine" /></template></el-table-column>
      <el-table-column label="实例" width="160"><template #default="{ row }"><IdLink kind="instance" :id="row.instance_id" data-testid="alert-instance" /></template></el-table-column>
      <el-table-column label="时间" width="160"><template #default="{ row }"><span data-testid="alert-time">{{ fmtTime(row.created_at) }}</span></template></el-table-column>
      <el-table-column label="操作" width="140">
        <template #default="{ row }">
          <el-button v-if="!row.resolved_at" size="small" type="primary" data-testid="alert-resolve" @click="resolve(row)">标记已处理</el-button>
          <span v-else class="muted" data-testid="alert-resolved-at">{{ fmtTime(row.resolved_at) }} 已处理</span>
        </template>
      </el-table-column>
    </el-table>
    <el-pagination v-model:current-page="list.page.value" v-model:page-size="list.size.value" :total="list.total.value" layout="total, prev, pager, next" class="pager" data-testid="alerts-pager" />
  </el-card>
</template>

<style scoped>
.toolbar { margin-bottom: 12px; }
.pager { margin-top: 12px; justify-content: flex-end; }
.muted { color: #909399; }
</style>
```

（表格每行需要一个可定位的容器：测试用 `page.getByRole('row').filter({ has: ... })`；`alert-row` 不需要单独的 testid，后续任务同理。）

`web/admin/src/views/AuditView.vue`：

```vue
<script setup lang="ts">
import { auditActions, fmtTime, type AuditEntry, type Paged } from '@spark/shared'
import { onMounted, reactive } from 'vue'
import { api } from '../api'
import DateRange from '../components/DateRange.vue'
import IdLink from '../components/IdLink.vue'
import { usePaged } from '../composables/usePaged'

const filter = reactive({ actor_id: '', action: '', target_type: '', target_id: '', range: { from: '', to: '' } })
const list = usePaged<AuditEntry>((page, size) =>
  api.get<Paged<AuditEntry>>('/audit', {
    actor_id: filter.actor_id, action: filter.action, target_type: filter.target_type, target_id: filter.target_id,
    from: filter.range.from, to: filter.range.to, page, page_size: size,
  }),
)
onMounted(list.load)
function resetFilter() {
  Object.assign(filter, { actor_id: '', action: '', target_type: '', target_id: '', range: { from: '', to: '' } })
  list.reset()
}
const linkKind = (t: string) => (t === 'machine' || t === 'instance' || t === 'user' ? t : null)
const actorText = (e: AuditEntry) => (e.actor_type === 'system' ? '系统' : e.actor_name || `#${e.actor_id}`)
const pretty = (d: unknown) => JSON.stringify(d ?? {}, null, 2)
</script>

<template>
  <el-card>
    <el-form inline class="filters" @submit.prevent="list.reset()">
      <el-form-item label="操作人 ID"><el-input v-model="filter.actor_id" data-testid="audit-actor" clearable style="width: 120px" /></el-form-item>
      <el-form-item label="动作">
        <el-select v-model="filter.action" filterable allow-create default-first-option clearable placeholder="全部" data-testid="audit-action" style="width: 220px">
          <el-option v-for="a in auditActions" :key="a" :label="a" :value="a" />
        </el-select>
      </el-form-item>
      <el-form-item label="对象类型"><el-input v-model="filter.target_type" data-testid="audit-target-type" clearable style="width: 120px" /></el-form-item>
      <el-form-item label="对象 ID"><el-input v-model="filter.target_id" data-testid="audit-target-id" clearable style="width: 160px" /></el-form-item>
      <el-form-item label="时间"><DateRange v-model="filter.range" prefix="audit" /></el-form-item>
      <el-button type="primary" native-type="submit" data-testid="audit-search">查询</el-button>
      <el-button data-testid="audit-reset" @click="resetFilter">重置</el-button>
    </el-form>
    <el-table :data="list.items.value" v-loading="list.loading.value" row-key="id">
      <el-table-column type="expand">
        <template #default="{ row }"><pre class="diff" data-testid="audit-diff">{{ pretty(row.diff) }}</pre></template>
      </el-table-column>
      <el-table-column label="时间" width="160"><template #default="{ row }"><span data-testid="audit-time">{{ fmtTime(row.created_at) }}</span></template></el-table-column>
      <el-table-column label="操作人" width="140"><template #default="{ row }"><span data-testid="audit-actor-cell">{{ actorText(row) }}</span></template></el-table-column>
      <el-table-column label="动作" width="200"><template #default="{ row }"><code data-testid="audit-action-cell">{{ row.action }}</code></template></el-table-column>
      <el-table-column label="对象" min-width="220">
        <template #default="{ row }">
          <span data-testid="audit-target">{{ row.target_type }} <IdLink v-if="linkKind(row.target_type)" :kind="linkKind(row.target_type)!" :id="row.target_id" /><span v-else class="mono">{{ row.target_id }}</span></span>
        </template>
      </el-table-column>
      <el-table-column label="IP" width="140"><template #default="{ row }"><span data-testid="audit-ip">{{ row.ip }}</span></template></el-table-column>
    </el-table>
    <el-pagination v-model:current-page="list.page.value" v-model:page-size="list.size.value" :total="list.total.value" layout="total, prev, pager, next" class="pager" data-testid="audit-pager" />
  </el-card>
</template>

<style scoped>
.filters { display: flex; flex-wrap: wrap; gap: 8px 16px; align-items: center; margin-bottom: 12px; }
.diff { margin: 0; padding: 8px 16px; background: #f5f7fa; font-size: 12px; white-space: pre-wrap; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.pager { margin-top: 12px; justify-content: flex-end; }
</style>
```

Run: `cd web && npm test && npm run build`
Expected: Vitest PASS（usePaged 2）；构建通过。

- [ ] **Step 4: 端到端用例**

`web/e2e/helpers/time.ts` 末尾追加：

```ts
const dateFmt = new Intl.DateTimeFormat('en-US', { timeZone: 'Asia/Shanghai', year: 'numeric', month: '2-digit', day: '2-digit' })

/** 北京时间 yyyy-MM-dd（用于后台日期筛选）。 */
export function cstDate(iso: string | Date): string {
  const p: Record<string, string> = {}
  for (const x of dateFmt.formatToParts(new Date(iso))) p[x.type] = x.value
  return `${p.year}-${p.month}-${p.day}`
}
```

`web/e2e/admin/dashboard.spec.ts`：

```ts
import { expect, test } from '@playwright/test'
import { adminApi, createAdmin, loginAdmin } from '../helpers/admin'
import { yuan } from '../helpers/money'

type Dash = {
  machines: { by_state: Record<string, number>; online: number; total: number }
  instances: Record<string, number>
  recharge_today: number
  recharge_month: number
  order_today: number
  order_month: number
  utilization: { date: string; rate: number }[]
}

test('A-DASH-01 仪表盘数字与接口一致、金额格式、30 根柱', async ({ page }) => {
  const admin = await createAdmin('finance')
  const d = (await (await (await adminApi(admin)).get('/admin/api/v1/dashboard')).json()) as Dash
  await loginAdmin(page, admin)
  await expect(page.getByTestId('dash-machines-total')).toHaveText(String(d.machines.total))
  await expect(page.getByTestId('dash-machines-online')).toHaveText(String(d.machines.online))
  for (const s of ['idle', 'allocated', 'maintenance', 'faulty']) await expect(page.getByTestId(`dash-machine-${s}`)).toHaveText(String(d.machines.by_state[s] ?? 0))
  for (const s of ['creating', 'running', 'stopped', 'wiping', 'released', 'failed']) await expect(page.getByTestId(`dash-instance-${s}`)).toHaveText(String(d.instances[s] ?? 0))
  await expect(page.getByTestId('dash-recharge-today')).toHaveText(yuan(d.recharge_today))
  await expect(page.getByTestId('dash-recharge-month')).toHaveText(yuan(d.recharge_month))
  await expect(page.getByTestId('dash-order-today')).toHaveText(yuan(d.order_today))
  await expect(page.getByTestId('dash-order-month')).toHaveText(yuan(d.order_month))
  const bars = page.getByTestId('dash-bar')
  await expect(bars).toHaveCount(30)
  const last = d.utilization[29]
  await expect(bars.nth(29)).toHaveAttribute('data-date', last.date)
  await expect(bars.nth(29).locator('title')).toHaveText(`${last.date} 出租率 ${(last.rate * 100).toFixed(1)}%`)
})
```

`web/e2e/admin/alerts.spec.ts`：

```ts
import { expect, test } from '@playwright/test'
import { adminApi, createAdmin, loginAdmin } from '../helpers/admin'
import { placeOrder, seedUser, toast, userApi } from '../helpers/console'
import { hooks } from '../helpers/hooks'

test('A-ALT-01 开通失败告警：未处理列表、角标、标记已处理、全部可见', async ({ page }) => {
  const u = await seedUser()
  const api = await userApi(u.phone)
  await hooks.machines(1)
  const instanceId = await placeOrder(api)
  await hooks.agent({ fail: true })
  const admin = await createAdmin('ops')
  const ctx = await adminApi(admin)
  const total = ((await (await ctx.get('/admin/api/v1/alerts?resolved=false&page_size=1')).json()) as { total: number }).total
  await loginAdmin(page, admin)
  await expect(page.getByTestId('menu-alerts-badge')).toContainText(String(total))
  await page.getByTestId('menu-alerts').click()
  const row = page.getByRole('row').filter({ has: page.getByTestId('alert-instance').filter({ hasText: instanceId }) })
  await expect(row.getByTestId('alert-kind')).toHaveText('开通失败')
  await expect(row.getByTestId('alert-level')).toHaveText('严重')
  await row.getByTestId('alert-resolve').click()
  await toast(page, '已标记为已处理')
  await expect(row).toHaveCount(0)
  await expect(page.getByTestId('menu-alerts-badge')).toHaveCount(total - 1 > 0 ? 1 : 0)
  if (total - 1 > 0) await expect(page.getByTestId('menu-alerts-badge')).toContainText(String(total - 1))
  await page.getByTestId('alerts-filter-all').click()
  await expect(row.getByTestId('alert-resolved-at')).toContainText('已处理')
})
```

`web/e2e/admin/audit.spec.ts`：

```ts
import { expect, test } from '@playwright/test'
import { adminApi, adminPost, createAdmin, loginAdmin } from '../helpers/admin'
import { seedUser } from '../helpers/console'
import { hooks } from '../helpers/hooks'
import { cstDate } from '../helpers/time'

test('A-AUD-01 审计日志按操作人 / 动作 / 对象 / 时间筛选，操作人显示用户名，对象可点击，展开 diff', async ({ page }) => {
  // 用 user.ban 造记录：它的 diff 非空（{reason}），对象类型 user 可跳转；alert.resolve 的 diff 是 null，格式化后只有「{}」，测不出两空格缩进
  const u = await seedUser()
  const admin = await createAdmin('super')
  const ctx = await adminApi(admin)
  await adminPost(ctx, admin, `/admin/api/v1/users/${u.id}/ban`, { reason: 'e2e 审计用例' })
  const today = cstDate(await hooks.clock())

  await loginAdmin(page, admin)
  await page.getByTestId('menu-audit').click()
  await page.getByTestId('audit-actor').fill(String(admin.id))
  await page.getByTestId('audit-action').click()
  await page.getByRole('option', { name: 'user.ban' }).click()
  await page.getByTestId('audit-target-type').fill('user')
  await page.getByTestId('audit-target-id').fill(String(u.id))
  await page.getByTestId('audit-from').fill(today)
  await page.getByTestId('audit-to').fill(today)
  await page.getByTestId('audit-search').click()
  const rows = page.getByRole('row').filter({ has: page.getByTestId('audit-action-cell') })
  await expect(rows).toHaveCount(1)
  await expect(rows.first().getByTestId('audit-actor-cell')).toHaveText(admin.username)
  await expect(rows.first().getByTestId('audit-action-cell')).toHaveText('user.ban')
  await expect(rows.first().getByTestId('audit-target')).toContainText(`user ${u.id}`)
  await expect(rows.first().getByTestId('audit-target').locator('a')).toHaveAttribute('href', `/admin/users/${u.id}`)
  await rows.first().locator('.el-table__expand-icon').click() // Element Plus 的展开控件（UI 控件，不是业务元素）
  const diff = page.getByTestId('audit-diff')
  await expect(diff).toBeVisible()
  const text = (await diff.textContent())!
  expect(JSON.parse(text)).toEqual({ reason: 'e2e 审计用例' })
  expect(text).toBe('{\n  "reason": "e2e 审计用例"\n}') // 两空格缩进的格式化 JSON

  // 操作人筛选换成另一个管理员 → 空；日期范围换成 2000 年 → 空
  const other = await createAdmin('ops')
  await page.getByTestId('audit-actor').fill(String(other.id))
  await page.getByTestId('audit-search').click()
  await expect(rows).toHaveCount(0)
  await page.getByTestId('audit-actor').fill(String(admin.id))
  await page.getByTestId('audit-from').fill('2000-01-01')
  await page.getByTestId('audit-to').fill('2000-01-02')
  await page.getByTestId('audit-search').click()
  await expect(rows).toHaveCount(0)
  // 对象链接真的能跳到用户详情
  await page.getByTestId('audit-reset').click()
  await page.getByTestId('audit-target-id').fill(String(u.id))
  await page.getByTestId('audit-search').click()
  await rows.first().getByTestId('audit-target').locator('a').click()
  await expect(page).toHaveURL(new RegExp(`/admin/users/${u.id}$`))
})
```

Run: `make web && cd web && npx playwright test --project=admin admin/dashboard.spec.ts admin/alerts.spec.ts admin/audit.spec.ts`
Expected: 3 passed。

- [ ] **Step 5: 全量验证并提交**

Run: `go vet ./... && go test ./... && cd web && npm test && npm run build && npx playwright test --project=admin`
Expected: 全部 PASS。

```bash
git add web/admin web/e2e
git commit -m "feat(admin): dashboard with SVG utilization chart, alerts with badge, audit log with filters and diff; e2e A-DASH/A-ALT/A-AUD

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---
### Task 17: 后台机器与 PDU

**Files:**
- Replace: `web/admin/src/views/{MachinesView,MachineDetailView,PdusView}.vue`
- Create: `web/admin/src/logic/csv.ts`、`web/admin/src/logic/__tests__/csv.spec.ts`
- Create: `web/e2e/admin/{machines,pdus}.spec.ts`
- Modify: `web/e2e/helpers/admin.ts`（追加 `createPDU`、`registerMachine`）

**Interfaces:**
- Consumes: Task 16 的 `usePaged`、`IdLink`；Task 2 的 B6（ops 可读 `/products`）；Task 11 helper `drainIdleMachines`。
- Produces:
  - `src/logic/csv.ts`：`IMPORT_HEADER = 'serial,rack,slot,lan_ip,product_code,pdu_name,pdu_outlet'`、`templateCSV()`（表头 + BOM）、`MAX_IMPORT_BYTES = 1 << 20`、`readCSVFile(file): Promise<string>`（超过 1MB 抛错「文件不能超过 1MB」）。
  - 机器列表 testid：`machines-filter-state`、`machines-filter-online`、`machines-filter-rack`、`machines-search`、`machines-reset`、行内 `machine-id`、`machine-serial`、`machine-rack`、`machine-ip`、`machine-product`、`machine-state`、`machine-online`（在线绿点 / 离线灰点 + 最后心跳）、`machine-pdu`、`machines-pager`（每页 20 / 50 / 100）、`machines-total`；登记弹窗 `machines-register-btn`、`register-serial`、`register-rack`、`register-slot`、`register-ip`、`register-product`、`register-pdu`、`register-outlet`、`register-submit`；导入弹窗 `machines-import-btn`、`import-file`（`input[type=file]`）、`import-submit`、`import-template`、`import-errors` / `import-error-row`（含 `import-error-line`、`import-error-message`）。
  - 机器详情 testid：`mdetail-serial`、`mdetail-state`、`mdetail-online`、`mdetail-current`（当前实例 `IdLink`）、`mdetail-none`、`mdetail-history-row` / `mdetail-history-id`、`mdetail-maintenance-btn`（文案「进入维护」/「结束维护」）、`mdetail-maintenance-ok`、`mdetail-power-btn`、`mdetail-power-text`（固定文案「将通过 PDU 对该机器断电重启，机器上的实例会中断，确定执行？」）、`mdetail-power-ok`、`mdetail-power-cancel`。
  - PDU testid：`pdus-create-btn`、行内 `pdu-name`、`pdu-driver`、`pdu-address`、`pdu-has-community`（「已设置」标记）、`pdu-edit`；表单 `pdu-form-name`、`pdu-form-driver-<snmp|mock>`、`pdu-form-address`、`pdu-form-community`（`type="password"`；编辑时 placeholder「留空表示不修改；修改了地址必须重新填写」）、`pdu-form-oid`、`pdu-form-cycle`、`pdu-form-on`、`pdu-form-submit`；驱动为 mock 时隐藏 SNMP 字段。
  - e2e helpers：`createPDU(ctx, admin, { name?, driver? })` → `{ id, name }`；`registerMachine(ctx, admin, { rack?, pdu_id?, pdu_outlet? })` → `{ id, serial }`（API 登记，不心跳 → 离线）。
  - 覆盖：A-MCH-01～06、A-PDU-01、A-PDU-02。

- [ ] **Step 1: 写失败的单测**

`web/admin/src/logic/__tests__/csv.spec.ts`：

```ts
import { describe, expect, it } from 'vitest'
import { IMPORT_HEADER, MAX_IMPORT_BYTES, readCSVFile, templateCSV } from '../csv'

describe('csv', () => {
  it('模板带 BOM 与固定表头', () => {
    expect(templateCSV()).toBe(`﻿${IMPORT_HEADER}\n`)
  })
  it('读取文本并限制 1MB', async () => {
    const small = new File(['serial,rack\nA,B\n'], 'm.csv', { type: 'text/csv' })
    expect(await readCSVFile(small)).toBe('serial,rack\nA,B\n')
    const big = new File([new Uint8Array(MAX_IMPORT_BYTES + 1)], 'big.csv')
    await expect(readCSVFile(big)).rejects.toThrow('文件不能超过 1MB')
  })
})
```

Run: `cd web && npm test`
Expected: FAIL（模块不存在）。

- [ ] **Step 2: 逻辑与三个页面**

`web/admin/src/logic/csv.ts`：

```ts
export const IMPORT_HEADER = 'serial,rack,slot,lan_ip,product_code,pdu_name,pdu_outlet'
export const MAX_IMPORT_BYTES = 1 << 20

export function templateCSV(): string {
  return `﻿${IMPORT_HEADER}\n`
}

/** 前端读取 CSV 文本直接提交（后端接收 {csv}），限制 1MB。 */
export async function readCSVFile(file: File): Promise<string> {
  if (file.size > MAX_IMPORT_BYTES) throw new Error('文件不能超过 1MB')
  return file.text()
}

export function downloadText(filename: string, text: string, type = 'text/csv;charset=utf-8') {
  const url = URL.createObjectURL(new Blob([text], { type }))
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  a.click()
  URL.revokeObjectURL(url)
}
```

`web/admin/src/views/MachinesView.vue`：

```vue
<script setup lang="ts">
import { fmtTime, isApiError, labelOf, machineStateLabel, messageOf, type AdminProduct, type ImportError, type Listed, type Machine, type Paged, type PDU } from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, onMounted, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { api } from '../api'
import { usePaged } from '../composables/usePaged'
import { downloadText, readCSVFile, templateCSV } from '../logic/csv'

const router = useRouter()
const filter = reactive({ state: '', online: '', rack: '' })
const list = usePaged<Machine>((page, size) => api.get<Paged<Machine>>('/machines', { state: filter.state, online: filter.online, rack: filter.rack.trim(), page, page_size: size }))
const products = ref<AdminProduct[]>([])
const pdus = ref<PDU[]>([])
const productName = computed(() => new Map(products.value.map((p) => [p.id, p.name])))
const pduName = computed(() => new Map(pdus.value.map((p) => [p.id, p.name])))
const stateType = (s: string) => ({ idle: 'success', allocated: 'primary', maintenance: 'warning', faulty: 'danger' })[s] ?? 'info'

onMounted(async () => {
  await list.load()
  const [p, d] = await Promise.all([api.get<Listed<AdminProduct>>('/products'), api.get<Listed<PDU>>('/pdus')])
  products.value = p.items
  pdus.value = d.items
})
function resetFilter() {
  Object.assign(filter, { state: '', online: '', rack: '' })
  list.reset()
}

// ---- 登记 ----
const registerVisible = ref(false)
const form = reactive<{ serial: string; rack: string; slot: string; lan_ip: string; product_id: number | null; pdu_id: number | null; pdu_outlet: number | null }>({
  serial: '', rack: '', slot: '', lan_ip: '', product_id: null, pdu_id: null, pdu_outlet: null,
})
const registering = ref(false)
function openRegister() {
  Object.assign(form, { serial: '', rack: '', slot: '', lan_ip: '', product_id: products.value[0]?.id ?? null, pdu_id: null, pdu_outlet: null })
  registerVisible.value = true
}
async function register() {
  registering.value = true
  try {
    const body: Record<string, unknown> = { serial: form.serial.trim(), rack: form.rack.trim(), slot: form.slot.trim(), lan_ip: form.lan_ip.trim(), product_id: form.product_id }
    if (form.pdu_id != null) body.pdu_id = form.pdu_id
    if (form.pdu_outlet != null) body.pdu_outlet = form.pdu_outlet
    await api.post('/machines', body)
    ElMessage.success('机器已登记')
    registerVisible.value = false
    list.reset()
  } catch (e) {
    ElMessage.error(messageOf(e)) // duplicate_serial / pdu_outlet_taken / bad_request 原文
  } finally {
    registering.value = false
  }
}

// ---- CSV 导入 ----
const importVisible = ref(false)
const importText = ref('')
const importName = ref('')
const importErrors = ref<ImportError[]>([])
const importing = ref(false)
async function pickFile(ev: Event) {
  const file = (ev.target as HTMLInputElement).files?.[0]
  if (!file) return
  try {
    importText.value = await readCSVFile(file)
    importName.value = file.name
    importErrors.value = []
  } catch (e) {
    ElMessage.error(e instanceof Error ? e.message : '读取文件失败')
  }
}
async function doImport() {
  importing.value = true
  try {
    const out = await api.post<Listed<Machine>>('/machines/import', { csv: importText.value })
    ElMessage.success(`成功导入 ${out.items.length} 台机器`)
    importVisible.value = false
    importText.value = ''
    list.reset()
  } catch (e) {
    if (isApiError(e) && e.code === 'import_invalid') {
      // 逐行错误在错误响应体的 errors 字段里，ApiError.details 保留了完整响应体
      importErrors.value = Array.isArray(e.details.errors) ? (e.details.errors as ImportError[]) : []
      ElMessage.error(e.message)
    } else ElMessage.error(messageOf(e))
  } finally {
    importing.value = false
  }
}
</script>

<template>
  <el-card>
    <el-form inline class="filters" @submit.prevent="list.reset()">
      <el-form-item label="状态">
        <el-select v-model="filter.state" clearable placeholder="全部" data-testid="machines-filter-state" style="width: 130px">
          <el-option v-for="(label, v) in machineStateLabel" :key="v" :label="label" :value="v" />
        </el-select>
      </el-form-item>
      <el-form-item label="在线">
        <el-select v-model="filter.online" clearable placeholder="全部" data-testid="machines-filter-online" style="width: 100px">
          <el-option label="是" value="true" /><el-option label="否" value="false" />
        </el-select>
      </el-form-item>
      <el-form-item label="机柜"><el-input v-model="filter.rack" clearable data-testid="machines-filter-rack" style="width: 140px" /></el-form-item>
      <el-button type="primary" native-type="submit" data-testid="machines-search">查询</el-button>
      <el-button data-testid="machines-reset" @click="resetFilter">重置</el-button>
      <span class="spacer" />
      <el-button data-testid="machines-import-btn" @click="importVisible = true">CSV 导入</el-button>
      <el-button type="primary" data-testid="machines-register-btn" @click="openRegister">登记机器</el-button>
    </el-form>

    <el-table :data="list.items.value" v-loading="list.loading.value" row-key="id" class="clickable" @row-click="(row: Machine) => router.push(`/machines/${row.id}`)">
      <el-table-column label="ID" width="80"><template #default="{ row }"><span data-testid="machine-id">{{ row.id }}</span></template></el-table-column>
      <el-table-column label="序列号" min-width="160"><template #default="{ row }"><span class="mono" data-testid="machine-serial">{{ row.serial }}</span></template></el-table-column>
      <el-table-column label="机柜 / 槽位" width="120"><template #default="{ row }"><span data-testid="machine-rack">{{ row.rack }} / {{ row.slot }}</span></template></el-table-column>
      <el-table-column label="内网 IP" width="140"><template #default="{ row }"><span class="mono" data-testid="machine-ip">{{ row.lan_ip }}</span></template></el-table-column>
      <el-table-column label="商品" min-width="140"><template #default="{ row }"><span data-testid="machine-product">{{ productName.get(row.product_id) ?? row.product_id }}</span></template></el-table-column>
      <el-table-column label="状态" width="100">
        <template #default="{ row }"><el-tag :type="stateType(row.state) as 'success'" data-testid="machine-state">{{ labelOf(machineStateLabel, row.state) }}</el-tag></template>
      </el-table-column>
      <el-table-column label="在线" width="200">
        <template #default="{ row }">
          <span data-testid="machine-online" :data-online="row.agent_online"><i class="dot" :class="row.agent_online ? 'on' : 'off'" />{{ row.agent_online ? '在线' : '离线' }} <small class="muted">{{ fmtTime(row.last_seen_at) || '从未心跳' }}</small></span>
        </template>
      </el-table-column>
      <el-table-column label="PDU" width="160"><template #default="{ row }"><span data-testid="machine-pdu">{{ row.pdu_id ? `${pduName.get(row.pdu_id) ?? row.pdu_id} #${row.pdu_outlet}` : '—' }}</span></template></el-table-column>
    </el-table>
    <div class="pager-row">
      <span class="muted" data-testid="machines-total">共 {{ list.total.value }} 台</span>
      <el-pagination v-model:current-page="list.page.value" v-model:page-size="list.size.value" :page-sizes="[20, 50, 100]" :total="list.total.value" layout="sizes, prev, pager, next" data-testid="machines-pager" />
    </div>

    <el-dialog v-model="registerVisible" title="登记机器" width="520" align-center destroy-on-close>
      <el-form label-position="top" @submit.prevent="register">
        <el-form-item label="序列号"><el-input v-model="form.serial" data-testid="register-serial" /></el-form-item>
        <div class="two">
          <el-form-item label="机柜"><el-input v-model="form.rack" data-testid="register-rack" /></el-form-item>
          <el-form-item label="槽位"><el-input v-model="form.slot" data-testid="register-slot" /></el-form-item>
        </div>
        <el-form-item label="内网 IP"><el-input v-model="form.lan_ip" data-testid="register-ip" /></el-form-item>
        <el-form-item label="商品">
          <el-select v-model="form.product_id" data-testid="register-product"><el-option v-for="p in products" :key="p.id" :label="p.name" :value="p.id" /></el-select>
        </el-form-item>
        <div class="two">
          <el-form-item label="PDU（可空）">
            <el-select v-model="form.pdu_id" clearable placeholder="不配置" data-testid="register-pdu"><el-option v-for="p in pdus" :key="p.id" :label="p.name" :value="p.id" /></el-select>
          </el-form-item>
          <el-form-item label="插座号（1～256）"><el-input-number v-model="form.pdu_outlet" :min="1" :max="256" :controls="false" data-testid="register-outlet" /></el-form-item>
        </div>
        <el-button type="primary" native-type="submit" :loading="registering" data-testid="register-submit">登记</el-button>
      </el-form>
    </el-dialog>

    <el-dialog v-model="importVisible" title="CSV 导入机器" width="640" align-center destroy-on-close>
      <p class="muted">表头固定：<code>serial,rack,slot,lan_ip,product_code,pdu_name,pdu_outlet</code>；任一行有错整批不导入。</p>
      <div class="import-row">
        <input type="file" accept=".csv,text/csv" data-testid="import-file" @change="pickFile" />
        <el-button link type="primary" data-testid="import-template" @click="downloadText('machines-template.csv', templateCSV())">下载模板</el-button>
      </div>
      <p v-if="importName" class="muted">已选择：{{ importName }}</p>
      <el-table v-if="importErrors.length" :data="importErrors" size="small" data-testid="import-errors">
        <el-table-column label="行号" width="80"><template #default="{ row }"><span data-testid="import-error-line">{{ row.line }}</span></template></el-table-column>
        <el-table-column label="原因"><template #default="{ row }"><span data-testid="import-error-message">{{ row.message }}</span></template></el-table-column>
      </el-table>
      <template #footer>
        <el-button @click="importVisible = false">取消</el-button>
        <el-button type="primary" :disabled="!importText" :loading="importing" data-testid="import-submit" @click="doImport">导入</el-button>
      </template>
    </el-dialog>
  </el-card>
</template>

<style scoped>
.filters { display: flex; flex-wrap: wrap; gap: 8px 16px; align-items: center; margin-bottom: 12px; }
.spacer { flex: 1; }
.clickable :deep(tbody tr) { cursor: pointer; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.muted { color: #909399; }
.dot { display: inline-block; width: 8px; height: 8px; border-radius: 50%; margin-right: 6px; }
.dot.on { background: var(--el-color-success); }
.dot.off { background: #c0c4cc; }
.pager-row { display: flex; justify-content: space-between; align-items: center; margin-top: 12px; }
.two { display: grid; grid-template-columns: 1fr 1fr; gap: 12px; }
.import-row { display: flex; align-items: center; gap: 12px; margin: 8px 0; }
</style>
```

（`import_invalid` 的逐行错误在响应体 `errors` 字段里；`ApiError.details`（Task 5 / 6）保留了完整响应体，页面直接读取，不重放请求——重放会绕过请求层的 401 处理、把同一份 CSV 再发一次，而且一旦后端把「整批校验」改成「部分成功」就会产生副作用。）

`web/admin/src/views/MachineDetailView.vue`：

```vue
<script setup lang="ts">
import { fmtTime, instanceStateLabel, labelOf, machineStateLabel, messageOf, type MachineDetail } from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, onMounted, ref } from 'vue'
import { useRoute } from 'vue-router'
import { api } from '../api'
import IdLink from '../components/IdLink.vue'

const route = useRoute()
const id = computed(() => String(route.params.id))
const data = ref<MachineDetail | null>(null)
const maintenanceVisible = ref(false)
const powerVisible = ref(false)
const busy = ref(false)
const inMaintenance = computed(() => data.value?.machine.state === 'maintenance' || data.value?.machine.state === 'faulty')

async function load() {
  try {
    data.value = await api.get<MachineDetail>(`/machines/${id.value}`)
  } catch (e) {
    ElMessage.error(messageOf(e))
  }
}
onMounted(load)

async function toggleMaintenance() {
  busy.value = true
  try {
    await api.post(`/machines/${id.value}/maintenance`, { on: !inMaintenance.value })
    ElMessage.success(inMaintenance.value ? '已结束维护' : '已进入维护')
    maintenanceVisible.value = false
    await load()
  } catch (e) {
    maintenanceVisible.value = false
    ElMessage.error(messageOf(e)) // 「该机器上还有未结束的实例，不能结束维护」等原文
  } finally {
    busy.value = false
  }
}
async function powerCycle() {
  busy.value = true
  try {
    await api.post(`/machines/${id.value}/power-cycle`, {})
    ElMessage.success('已通过 PDU 断电重启')
    powerVisible.value = false
  } catch (e) {
    powerVisible.value = false
    ElMessage.error(messageOf(e)) // 「该机器未配置 PDU」等原文
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <div v-if="data" class="detail">
    <el-card>
      <template #header><div class="head"><span class="mono" data-testid="mdetail-serial">{{ data.machine.serial }}</span><el-tag data-testid="mdetail-state">{{ labelOf(machineStateLabel, data.machine.state) }}</el-tag></div></template>
      <el-descriptions :column="3" border>
        <el-descriptions-item label="机柜 / 槽位">{{ data.machine.rack }} / {{ data.machine.slot }}</el-descriptions-item>
        <el-descriptions-item label="内网 IP"><span class="mono">{{ data.machine.lan_ip }}</span></el-descriptions-item>
        <el-descriptions-item label="在线"><span data-testid="mdetail-online">{{ data.machine.agent_online ? '在线' : '离线' }}</span>（{{ fmtTime(data.machine.last_seen_at) || '从未心跳' }}）</el-descriptions-item>
        <el-descriptions-item label="PDU">{{ data.machine.pdu_id ? `#${data.machine.pdu_id} 插座 ${data.machine.pdu_outlet}` : '未配置' }}</el-descriptions-item>
        <el-descriptions-item label="登记时间">{{ fmtTime(data.machine.created_at) }}</el-descriptions-item>
        <el-descriptions-item label="当前实例">
          <IdLink v-if="data.current_instance" kind="instance" :id="data.current_instance.id" data-testid="mdetail-current" />
          <span v-else class="muted" data-testid="mdetail-none">无</span>
        </el-descriptions-item>
      </el-descriptions>
      <div class="actions">
        <el-button data-testid="mdetail-maintenance-btn" @click="maintenanceVisible = true">{{ inMaintenance ? '结束维护' : '进入维护' }}</el-button>
        <el-button type="danger" plain data-testid="mdetail-power-btn" @click="powerVisible = true">断电重启</el-button>
      </div>
    </el-card>

    <el-card header="历史实例">
      <el-table :data="data.history" row-key="id" :row-class-name="() => 'history-row'">
        <el-table-column label="实例"><template #default="{ row }"><span data-testid="mdetail-history-row"><IdLink kind="instance" :id="row.id" data-testid="mdetail-history-id" /></span></template></el-table-column>
        <el-table-column label="用户" width="100"><template #default="{ row }"><IdLink kind="user" :id="row.user_id" /></template></el-table-column>
        <el-table-column label="状态" width="120"><template #default="{ row }">{{ labelOf(instanceStateLabel, row.state) }}</template></el-table-column>
        <el-table-column label="创建时间" width="160"><template #default="{ row }">{{ fmtTime(row.created_at) }}</template></el-table-column>
      </el-table>
    </el-card>

    <el-dialog v-model="maintenanceVisible" :title="inMaintenance ? '结束维护' : '进入维护'" width="420" align-center>
      <p>{{ inMaintenance ? '结束维护后机器重新进入可售库存。' : '进入维护后该机器不再分配给新订单。' }}</p>
      <template #footer>
        <el-button @click="maintenanceVisible = false">取消</el-button>
        <el-button type="primary" :loading="busy" data-testid="mdetail-maintenance-ok" @click="toggleMaintenance">确定</el-button>
      </template>
    </el-dialog>
    <el-dialog v-model="powerVisible" title="断电重启" width="420" align-center>
      <p data-testid="mdetail-power-text">将通过 PDU 对该机器断电重启，机器上的实例会中断，确定执行？</p>
      <template #footer>
        <el-button data-testid="mdetail-power-cancel" @click="powerVisible = false">取消</el-button>
        <el-button type="danger" :loading="busy" data-testid="mdetail-power-ok" @click="powerCycle">确定执行</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
.detail { display: grid; gap: 16px; }
.head { display: flex; justify-content: space-between; align-items: center; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.muted { color: #909399; }
.actions { display: flex; gap: 12px; margin-top: 16px; }
</style>
```

`web/admin/src/views/PdusView.vue`：

```vue
<script setup lang="ts">
import { fmtTime, messageOf, type Listed, type PDU, type PDUInput } from '@spark/shared'
import { ElMessage } from 'element-plus'
import { onMounted, reactive, ref } from 'vue'
import { api } from '../api'

const items = ref<PDU[]>([])
const visible = ref(false)
const editing = ref<PDU | null>(null)
const busy = ref(false)
const form = reactive<PDUInput>({ name: '', driver: 'snmp', address: '', community: '', oid_prefix: '', cycle_value: 3, on_value: 1 })

async function load() {
  try {
    items.value = (await api.get<Listed<PDU>>('/pdus')).items
  } catch (e) {
    ElMessage.error(messageOf(e))
  }
}
onMounted(load)

function openCreate() {
  editing.value = null
  Object.assign(form, { name: '', driver: 'snmp', address: '', community: '', oid_prefix: '', cycle_value: 3, on_value: 1 })
  visible.value = true
}
function openEdit(p: PDU) {
  editing.value = p
  Object.assign(form, { name: p.name, driver: p.driver, address: p.address, community: '', oid_prefix: p.oid_prefix, cycle_value: p.cycle_value, on_value: p.on_value })
  visible.value = true
}
async function submit() {
  busy.value = true
  try {
    if (editing.value) await api.put(`/pdus/${editing.value.id}`, { ...form })
    else await api.post('/pdus', { ...form })
    ElMessage.success(editing.value ? 'PDU 已更新' : 'PDU 已添加')
    visible.value = false
    await load()
  } catch (e) {
    ElMessage.error(messageOf(e)) // duplicate_name / 「请填写 SNMP community」/「修改 PDU 地址时请重新填写 SNMP community」原文
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <el-card>
    <div class="toolbar"><el-button type="primary" data-testid="pdus-create-btn" @click="openCreate">新增 PDU</el-button></div>
    <el-table :data="items" row-key="id">
      <el-table-column label="名称" min-width="140"><template #default="{ row }"><span data-testid="pdu-name">{{ row.name }}</span></template></el-table-column>
      <el-table-column label="驱动" width="90"><template #default="{ row }"><span data-testid="pdu-driver">{{ row.driver }}</span></template></el-table-column>
      <el-table-column label="地址" min-width="160"><template #default="{ row }"><span class="mono" data-testid="pdu-address">{{ row.address }}</span></template></el-table-column>
      <el-table-column label="community" width="110"><template #default="{ row }"><el-tag v-if="row.has_community" size="small" data-testid="pdu-has-community">已设置</el-tag><span v-else class="muted">—</span></template></el-table-column>
      <el-table-column label="OID 前缀" min-width="200"><template #default="{ row }"><span class="mono">{{ row.oid_prefix }}</span></template></el-table-column>
      <el-table-column label="创建时间" width="160"><template #default="{ row }">{{ fmtTime(row.created_at) }}</template></el-table-column>
      <el-table-column label="操作" width="90"><template #default="{ row }"><el-button size="small" data-testid="pdu-edit" @click="openEdit(row)">编辑</el-button></template></el-table-column>
    </el-table>

    <el-dialog v-model="visible" :title="editing ? '编辑 PDU' : '新增 PDU'" width="520" align-center destroy-on-close>
      <el-form label-position="top" @submit.prevent="submit">
        <el-form-item label="名称"><el-input v-model="form.name" data-testid="pdu-form-name" /></el-form-item>
        <el-form-item label="驱动">
          <el-radio-group v-model="form.driver">
            <el-radio-button value="snmp" data-testid="pdu-form-driver-snmp">snmp</el-radio-button>
            <el-radio-button value="mock" data-testid="pdu-form-driver-mock">mock（开发）</el-radio-button>
          </el-radio-group>
        </el-form-item>
        <el-form-item label="地址（host 或 host:port）"><el-input v-model="form.address" data-testid="pdu-form-address" /></el-form-item>
        <template v-if="form.driver === 'snmp'">
          <el-form-item label="SNMP community">
            <el-input v-model="form.community" type="password" show-password autocomplete="new-password" data-testid="pdu-form-community" :placeholder="editing ? '留空表示不修改；修改了地址必须重新填写' : ''" />
            <el-tag v-if="editing?.has_community" size="small" class="set-tag" data-testid="pdu-has-community">已设置</el-tag>
          </el-form-item>
          <el-form-item label="OID 前缀"><el-input v-model="form.oid_prefix" data-testid="pdu-form-oid" placeholder="例如 1.3.6.1.4.1.318.1.1.4.4.2.1.3" /></el-form-item>
          <div class="two">
            <el-form-item label="断电重启取值"><el-input-number v-model="form.cycle_value" :controls="false" data-testid="pdu-form-cycle" /></el-form-item>
            <el-form-item label="上电取值"><el-input-number v-model="form.on_value" :controls="false" data-testid="pdu-form-on" /></el-form-item>
          </div>
        </template>
        <el-button type="primary" native-type="submit" :loading="busy" data-testid="pdu-form-submit">保存</el-button>
      </el-form>
    </el-dialog>
  </el-card>
</template>

<style scoped>
.toolbar { margin-bottom: 12px; display: flex; justify-content: flex-end; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.muted { color: #909399; }
.set-tag { margin-top: 4px; }
.two { display: grid; grid-template-columns: 1fr 1fr; gap: 12px; }
</style>
```

Run: `cd web && npm test && npm run build`
Expected: Vitest PASS（csv 2）；构建通过。

- [ ] **Step 3: 端到端用例**

`web/e2e/helpers/admin.ts` 末尾追加：

```ts
let pduSeq = 0
export async function createPDU(ctx: APIRequestContext, admin: SeededAdmin, opts: { name?: string; driver?: 'mock' | 'snmp' } = {}) {
  pduSeq += 1
  const name = opts.name ?? `pdu-${runStamp}-${pduSeq}`
  const body = { name, driver: opts.driver ?? 'mock', address: `10.97.0.${pduSeq % 250}`, community: opts.driver === 'snmp' ? 'public' : '', oid_prefix: '1.3.6.1.4.1.318.1.1.4.4.2.1.3', cycle_value: 3, on_value: 1 }
  const out = await adminPost<{ id: number; name: string }>(ctx, admin, '/admin/api/v1/pdus', body)
  return { id: out.id, name }
}

let ipSeq = 0
export async function registerMachine(ctx: APIRequestContext, admin: SeededAdmin, opts: { rack?: string; pdu_id?: number; pdu_outlet?: number } = {}) {
  ipSeq += 1
  const products = ((await (await ctx.get('/admin/api/v1/products')).json()) as { items: { id: number }[] }).items
  const serial = nextSerial()
  const body: Record<string, unknown> = { serial, rack: opts.rack ?? 'E2E', slot: String(ipSeq), lan_ip: `10.96.${Math.floor(ipSeq / 250)}.${ipSeq % 250}`, product_id: products[0].id }
  if (opts.pdu_id) {
    body.pdu_id = opts.pdu_id
    body.pdu_outlet = opts.pdu_outlet
  }
  const out = await adminPost<{ id: number }>(ctx, admin, '/admin/api/v1/machines', body)
  return { id: out.id, serial }
}
```

`web/e2e/admin/machines.spec.ts`：

```ts
import { expect, request, test } from '@playwright/test'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { adminApi, adminPost, createAdmin, createPDU, drainIdleMachines, loginAdmin, nextSerial, registerMachine } from '../helpers/admin'
import { placeOrder, seedUser, toast, userApi } from '../helpers/console'
import { CONSOLE_URL, hooks } from '../helpers/hooks'
import { runStamp } from '../helpers/phone'

const fixtureText = (name: string) => readFileSync(fileURLToPath(new URL(`../fixtures/${name}`, import.meta.url)), 'utf8').replaceAll('{{N}}', runStamp)
const rowBySerial = (page: import('@playwright/test').Page, serial: string) => page.getByRole('row').filter({ has: page.getByTestId('machine-serial').filter({ hasText: serial }) })

async function idleCount(): Promise<number> {
  const ctx = await request.newContext({ baseURL: CONSOLE_URL })
  return ((await (await ctx.get('/api/v1/products')).json()) as { items: { idle_count: number }[] }).items[0].idle_count
}

test('A-MCH-01 机器列表筛选与分页', async ({ page }) => {
  // 列表按 rack, slot, id 排序且整轮测试共享数据库：不带机柜筛选去找某一台会落到第 N 页。
  // 所以本用例的两台机器都登记在唯一机柜 R<戳> 下，每次都叠加机柜筛选，再分别验证状态 / 在线两个筛选。
  const admin = await createAdmin('ops')
  const ctx = await adminApi(admin)
  const rack = `R${runStamp}`
  const offline = await registerMachine(ctx, admin, { rack }) // API 登记，不心跳 → 离线
  await adminPost(ctx, admin, `/admin/api/v1/machines/${offline.id}/maintenance`, { on: true })
  const online = await registerMachine(ctx, admin, { rack })
  await hooks.agent({ machine_id: online.id }) // 只对这台心跳 → 在线、空闲
  const search = async (opts: { state?: string; online?: string }) => {
    await page.getByTestId('machines-reset').click()
    await page.getByTestId('machines-filter-rack').fill(rack)
    if (opts.state) {
      await page.getByTestId('machines-filter-state').click()
      await page.getByRole('option', { name: opts.state, exact: true }).click()
    }
    if (opts.online) {
      await page.getByTestId('machines-filter-online').click()
      await page.getByRole('option', { name: opts.online, exact: true }).click()
    }
    await page.getByTestId('machines-search').click()
  }
  await loginAdmin(page, admin)
  await page.getByTestId('menu-machines').click()

  await search({})
  await expect(page.getByTestId('machines-total')).toHaveText('共 2 台')
  await expect(rowBySerial(page, offline.serial).getByTestId('machine-state')).toHaveText('维护中')
  await expect(rowBySerial(page, offline.serial).getByTestId('machine-online')).toContainText('离线')
  await expect(rowBySerial(page, online.serial).getByTestId('machine-state')).toHaveText('空闲')
  await expect(rowBySerial(page, online.serial).getByTestId('machine-online')).toContainText('在线')
  await expect(rowBySerial(page, online.serial).getByTestId('machine-product')).toHaveText('DGX Spark 整机')

  await search({ state: '空闲' })
  await expect(page.getByTestId('machines-total')).toHaveText('共 1 台')
  await expect(rowBySerial(page, online.serial)).toHaveCount(1)
  await expect(rowBySerial(page, offline.serial)).toHaveCount(0)

  await search({ online: '否' })
  await expect(page.getByTestId('machines-total')).toHaveText('共 1 台')
  await expect(rowBySerial(page, offline.serial)).toHaveCount(1)

  await search({ state: '维护中', online: '是' })
  await expect(page.getByTestId('machines-total')).toHaveText('共 0 台')

  // 分页 total 与接口一致（不带机柜筛选）；先取接口值再断言，避免读到重置前的旧文案
  const apiTotal = ((await (await ctx.get('/admin/api/v1/machines?page_size=1')).json()) as { total: number }).total
  expect(apiTotal).toBeGreaterThanOrEqual(2)
  await page.getByTestId('machines-reset').click()
  await expect(page.getByTestId('machines-total')).toHaveText(`共 ${apiTotal} 台`)
  await expect(page.getByRole('row').filter({ has: page.getByTestId('machine-id') })).toHaveCount(Math.min(20, apiTotal))

  // 行点击进详情
  await search({})
  await rowBySerial(page, online.serial).getByTestId('machine-id').click()
  await expect(page).toHaveURL(new RegExp(`/admin/machines/${online.id}$`))
})

test('A-MCH-02 单台登记：成功、重复序列号、PDU 与插座只填一个', async ({ page }) => {
  const admin = await createAdmin('ops')
  const ctx = await adminApi(admin)
  const pdu = await createPDU(ctx, admin)
  await loginAdmin(page, admin)
  await page.getByTestId('menu-machines').click()
  const serial = nextSerial()
  const fill = async () => {
    await page.getByTestId('machines-register-btn').click()
    await page.getByTestId('register-serial').fill(serial)
    await page.getByTestId('register-rack').fill(`R${runStamp}`)
    await page.getByTestId('register-slot').fill('07')
    await page.getByTestId('register-ip').fill('10.95.0.7')
  }
  await fill()
  await page.getByTestId('register-submit').click()
  await toast(page, '机器已登记')
  await page.getByTestId('machines-filter-rack').fill(`R${runStamp}`)
  await page.getByTestId('machines-search').click()
  await expect(rowBySerial(page, serial)).toHaveCount(1)
  await fill()
  await page.getByTestId('register-submit').click()
  await toast(page, '序列号已存在')
  await page.getByTestId('register-serial').fill(nextSerial())
  await page.getByTestId('register-pdu').click()
  await page.getByRole('option', { name: pdu.name }).click()
  await page.getByTestId('register-submit').click()
  await toast(page, 'PDU 与插座号需同时填写')
})

test('A-MCH-03 CSV 导入：成功 3 台、逐行错误整批不导入、模板下载', async ({ page }) => {
  const admin = await createAdmin('ops')
  const ctx = await adminApi(admin)
  const countB02 = async () => ((await (await ctx.get('/admin/api/v1/machines?rack=B02&page_size=1')).json()) as { total: number }).total
  await loginAdmin(page, admin)
  await page.getByTestId('menu-machines').click()
  await page.getByTestId('machines-import-btn').click()
  await page.getByTestId('import-file').setInputFiles({ name: 'machines-ok.csv', mimeType: 'text/csv', buffer: Buffer.from(fixtureText('machines-ok.csv')) })
  await page.getByTestId('import-submit').click()
  await toast(page, '成功导入 3 台机器')
  for (let i = 1; i <= 3; i++) {
    const r = await ctx.get(`/admin/api/v1/machines?rack=B01&page_size=100`)
    const items = ((await r.json()) as { items: { serial: string }[] }).items
    expect(items.some((m) => m.serial === `E2E-CSV-${runStamp}-${i}`)).toBe(true)
  }
  const before = await countB02()
  await page.getByTestId('machines-import-btn').click()
  await page.getByTestId('import-file').setInputFiles({ name: 'machines-bad.csv', mimeType: 'text/csv', buffer: Buffer.from(fixtureText('machines-bad.csv')) })
  await page.getByTestId('import-submit').click()
  await expect(page.getByTestId('import-error-line')).toHaveText(['2', '3', '4'])
  await expect(page.getByTestId('import-error-message').nth(0)).toContainText('IP')
  expect(await countB02()).toBe(before)
  const download = page.waitForEvent('download')
  await page.getByTestId('import-template').click()
  const file = await download
  const text = await new Response(await file.createReadStream()).text()
  expect(text.replace(/^﻿/, '').split('\n')[0]).toBe('serial,rack,slot,lan_ip,product_code,pdu_name,pdu_outlet')
})

test('A-MCH-04 维护：进入维护减少市场空闲数；有实例不能结束维护；无实例可结束', async ({ page }) => {
  const admin = await createAdmin('ops')
  const [mid] = await hooks.machines(1)
  const before = await idleCount()
  await loginAdmin(page, admin)
  await page.goto(`/admin/machines/${mid}`)
  await page.getByTestId('mdetail-maintenance-btn').click()
  await page.getByTestId('mdetail-maintenance-ok').click()
  await toast(page, '已进入维护')
  await expect(page.getByTestId('mdetail-state')).toHaveText('维护中')
  expect(await idleCount()).toBe(before - 1)
  await page.getByTestId('mdetail-maintenance-btn').click()
  await page.getByTestId('mdetail-maintenance-ok').click()
  await toast(page, '已结束维护')
  await expect(page.getByTestId('mdetail-state')).toHaveText('空闲')

  // 洗机失败 → 机器故障、实例停在洗机中 → 结束维护被拒绝
  await drainIdleMachines(await adminApi(admin), admin)
  const [faultyId] = await hooks.machines(1)
  const u = await seedUser()
  const api = await userApi(u.phone)
  const instanceId = await placeOrder(api)
  await hooks.agent()
  await api.post(`/api/v1/instances/${instanceId}/release`, { data: {} })
  await hooks.agent({ fail: true })
  await page.goto(`/admin/machines/${faultyId}`)
  await expect(page.getByTestId('mdetail-state')).toHaveText('故障')
  await page.getByTestId('mdetail-maintenance-btn').click()
  await page.getByTestId('mdetail-maintenance-ok').click()
  await toast(page, '该机器上还有未结束的实例，不能结束维护')
})

test('A-MCH-05 断电重启：确认文案、取消不发请求、无 PDU 报错、mock PDU 成功并写审计', async ({ page }) => {
  const admin = await createAdmin('ops')
  const ctx = await adminApi(admin)
  const noPdu = await registerMachine(ctx, admin)
  const pdu = await createPDU(ctx, admin)
  const withPdu = await registerMachine(ctx, admin, { pdu_id: pdu.id, pdu_outlet: 3 })
  let posts = 0
  page.on('request', (r) => {
    if (r.method() === 'POST' && r.url().includes('/power-cycle')) posts += 1
  })
  await loginAdmin(page, admin)
  await page.goto(`/admin/machines/${noPdu.id}`)
  await page.getByTestId('mdetail-power-btn').click()
  await expect(page.getByTestId('mdetail-power-text')).toHaveText('将通过 PDU 对该机器断电重启，机器上的实例会中断，确定执行？')
  await page.getByTestId('mdetail-power-cancel').click()
  expect(posts).toBe(0)
  await page.getByTestId('mdetail-power-btn').click()
  await page.getByTestId('mdetail-power-ok').click()
  await toast(page, '该机器未配置 PDU')
  await page.goto(`/admin/machines/${withPdu.id}`)
  await page.getByTestId('mdetail-power-btn').click()
  await page.getByTestId('mdetail-power-ok').click()
  await toast(page, '已通过 PDU 断电重启')
  const audit = (await (await ctx.get(`/admin/api/v1/audit?action=machine.power_cycle&target_id=${withPdu.id}`)).json()) as { total: number }
  expect(audit.total).toBe(1)
})

test('A-MCH-06 机器详情显示当前实例与历史实例', async ({ page }) => {
  const admin = await createAdmin('ops')
  await drainIdleMachines(await adminApi(admin), admin)
  const [mid] = await hooks.machines(1)
  const u = await seedUser({ balance: 100000 })
  const api = await userApi(u.phone)
  const released = await placeOrder(api)
  await hooks.agent()
  await api.post(`/api/v1/instances/${released}/release`, { data: {} })
  await hooks.agent()
  const running = await placeOrder(api)
  await hooks.agent()
  await loginAdmin(page, admin)
  await page.goto(`/admin/machines/${mid}`)
  await expect(page.getByTestId('mdetail-current')).toHaveText(running)
  await expect(page.getByTestId('mdetail-history-id').filter({ hasText: released })).toHaveCount(1)
})
```

`web/e2e/admin/pdus.spec.ts`：

```ts
import { expect, test } from '@playwright/test'
import { adminApi, createAdmin, createPDU, loginAdmin } from '../helpers/admin'
import { toast } from '../helpers/console'
import { runStamp } from '../helpers/phone'

test('A-PDU-01 新增 PDU：mock 成功；snmp 缺 community 报错', async ({ page }) => {
  const admin = await createAdmin('ops')
  await loginAdmin(page, admin)
  await page.getByTestId('menu-pdus').click()
  await page.getByTestId('pdus-create-btn').click()
  await page.getByTestId('pdu-form-name').fill(`ui-mock-${runStamp}`)
  await page.getByTestId('pdu-form-driver-mock').click()
  await expect(page.getByTestId('pdu-form-community')).toHaveCount(0)
  await page.getByTestId('pdu-form-address').fill('10.94.0.1')
  await page.getByTestId('pdu-form-submit').click()
  await toast(page, 'PDU 已添加')
  await expect(page.getByTestId('pdu-name').filter({ hasText: `ui-mock-${runStamp}` })).toHaveCount(1)
  await page.getByTestId('pdus-create-btn').click()
  await page.getByTestId('pdu-form-name').fill(`ui-snmp-${runStamp}`)
  await page.getByTestId('pdu-form-driver-snmp').click()
  await page.getByTestId('pdu-form-address').fill('10.94.0.2')
  await page.getByTestId('pdu-form-oid').fill('1.3.6.1.4.1.318.1.1.4.4.2.1.3')
  await page.getByTestId('pdu-form-submit').click()
  await toast(page, '请填写 SNMP community')
})

test('A-PDU-02 编辑 PDU：community 密码框与提示；留空保存；改地址不填 community 报错', async ({ page }) => {
  const admin = await createAdmin('ops')
  const ctx = await adminApi(admin)
  const pdu = await createPDU(ctx, admin, { driver: 'snmp' })
  await loginAdmin(page, admin)
  await page.getByTestId('menu-pdus').click()
  const row = page.getByRole('row').filter({ has: page.getByTestId('pdu-name').filter({ hasText: pdu.name }) })
  await expect(row.getByTestId('pdu-has-community')).toHaveText('已设置')
  await row.getByTestId('pdu-edit').click()
  const community = page.getByTestId('pdu-form-community')
  await expect(community).toHaveAttribute('type', 'password')
  await expect(community).toHaveAttribute('placeholder', '留空表示不修改；修改了地址必须重新填写')
  await expect(page.getByRole('dialog').getByTestId('pdu-has-community')).toHaveText('已设置')
  await page.getByTestId('pdu-form-submit').click()
  await toast(page, 'PDU 已更新')
  await expect(row.getByTestId('pdu-has-community')).toHaveText('已设置')
  await row.getByTestId('pdu-edit').click()
  await page.getByTestId('pdu-form-address').fill('10.94.9.9')
  await page.getByTestId('pdu-form-submit').click()
  await toast(page, '修改 PDU 地址时请重新填写 SNMP community')
})
```

Run: `make web && cd web && npx playwright test --project=admin admin/machines.spec.ts admin/pdus.spec.ts`
Expected: 8 passed。

- [ ] **Step 4: 全量验证并提交**

Run: `go vet ./... && go test ./... && cd web && npm test && npm run build && npx playwright test --project=admin`
Expected: 全部 PASS。

```bash
git add web/admin web/e2e
git commit -m "feat(admin): machines list/register/import/detail with maintenance and power cycle; PDU management; e2e A-MCH, A-PDU

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---
### Task 18: 后台实例列表、详情与售后操作

**Files:**
- Replace: `web/admin/src/views/{InstancesView,InstanceDetailView}.vue`
- Create: `web/admin/src/logic/instanceOps.ts`、`web/admin/src/logic/__tests__/instanceOps.spec.ts`
- Modify: `web/shared/src/labels.ts`（追加 `commandStatusLabel`）
- Create: `web/e2e/admin/instances.spec.ts`、`web/e2e/admin/errors.spec.ts`

**Interfaces:**
- Consumes: Task 15～17 的 `api`、`session.admin.role`、`usePaged`、`IdLink`、`enterTotpDialog`、`drainIdleMachines`；Task 6 的 `http` 动态码重发；shared `labels`、`fmtDuration`。
- Produces:
  - `src/logic/instanceOps.ts`：`extendSeconds(amount: number, unit: 'hour' | 'day'): number`、`validateExtend(seconds): string | null`（`60 ≤ s ≤ 30 × 86400`，否则「补偿时长需在 1 分钟到 30 天之间」）、`validateRefund(yuan: string, maxCents: number): { cents: number } | { error: string }`（格式 / 负数 / 超上限「退款金额不能超过可退上限」）、`showExtend / showMigrate / showRewipe / showRefund(role, state): boolean`（按角色与后端允许的状态）。
  - `commandStatusLabel`：`pending` 待执行、`sent` 已下发、`done` 已完成、`failed` 失败。
  - 列表 testid：`ainst-filter-state`、`ainst-filter-user`、`ainst-filter-machine`、`ainst-search`、`ainst-reset`、`ainst-id`、`ainst-user`、`ainst-machine`、`ainst-image`、`ainst-state`、`ainst-suspended`、`ainst-time`、`ainst-expires`、`ainst-created`、`ainst-pager`、`ainst-total`；支持 `?user_id=&machine_id=&state=` 预填。
  - 详情 testid：`adetail-id`、`adetail-state`、`adetail-user`、`adetail-machine`、`adetail-product`、`adetail-image`、`adetail-remaining`（`data-remaining`）、`adetail-pending`、`adetail-expires`、`adetail-suspended`、`adetail-event`（文本 `from → to · reason · 时间`）、`adetail-order-row`（含 `adetail-order-id`、`adetail-order-kind`、`adetail-order-amount`、`adetail-order-refunded`、`adetail-order-status`）、`adetail-cmd-row`（含 `adetail-cmd-type`、`adetail-cmd-status`、`adetail-cmd-attempts`、`adetail-cmd-result`）；操作：`adetail-extend-btn`、`extend-amount`、`extend-unit`、`extend-reason`、`extend-error`、`extend-submit`；`adetail-migrate-btn`、`migrate-reason`、`migrate-submit`、`migrate-result`、`adetail-migrate-new`；`adetail-rewipe-btn`、`rewipe-ok`；`adetail-refund-btn`、`refund-amount`、`refund-hint`、`refund-reason`、`refund-error`、`refund-submit`。
  - 覆盖：A-INS-01～09、A-ERR-01。

- [ ] **Step 1: 写失败的单测**

`web/admin/src/logic/__tests__/instanceOps.spec.ts`：

```ts
import { describe, expect, it } from 'vitest'
import { extendSeconds, showExtend, showMigrate, showRefund, showRewipe, validateExtend, validateRefund } from '../instanceOps'

describe('instance ops', () => {
  it('延长时长换算与范围', () => {
    expect(extendSeconds(2, 'day')).toBe(172800)
    expect(extendSeconds(1.5, 'hour')).toBe(5400)
    expect(validateExtend(extendSeconds(31, 'day'))).toBe('补偿时长需在 1 分钟到 30 天之间')
    expect(validateExtend(30)).toBe('补偿时长需在 1 分钟到 30 天之间')
    expect(validateExtend(60)).toBeNull()
    expect(validateExtend(30 * 86400)).toBeNull()
  })
  it('退款金额：0 ≤ 金额 ≤ 上限', () => {
    expect(validateRefund('69', 6900)).toEqual({ cents: 6900 })
    expect(validateRefund('0', 6900)).toEqual({ cents: 0 })
    expect(validateRefund('69.01', 6900)).toEqual({ error: '退款金额不能超过可退上限' })
    expect(validateRefund('-1', 6900)).toEqual({ error: '金额格式不正确' })
    expect(validateRefund('abc', 6900)).toEqual({ error: '金额格式不正确' })
  })
  it('按钮按角色与状态', () => {
    expect(showExtend('ops', 'running')).toBe(true)
    expect(showExtend('finance', 'running')).toBe(false)
    expect(showExtend('super', 'wiping')).toBe(false)
    expect(showMigrate('ops', 'running')).toBe(true)
    expect(showMigrate('ops', 'stopped')).toBe(false)
    expect(showRewipe('ops', 'wiping')).toBe(true)
    expect(showRewipe('finance', 'wiping')).toBe(false)
    expect(showRefund('finance', 'stopped')).toBe(true)
    expect(showRefund('ops', 'running')).toBe(false)
    expect(showRefund('super', 'released')).toBe(false)
  })
})
```

Run: `cd web && npm test`
Expected: FAIL（模块不存在）。

- [ ] **Step 2: 逻辑、标签与两个页面**

`web/shared/src/labels.ts` 追加：

```ts
export const commandStatusLabel: LabelMap = { pending: '待执行', sent: '已下发', done: '已完成', failed: '失败' }
```

`web/admin/src/logic/instanceOps.ts`：

```ts
import { yuanToCents, type InstanceState, type Role } from '@spark/shared'

export const MIN_EXTEND = 60
export const MAX_EXTEND = 30 * 86400

export function extendSeconds(amount: number, unit: 'hour' | 'day'): number {
  return Math.round(amount * (unit === 'day' ? 86400 : 3600))
}

export function validateExtend(seconds: number): string | null {
  return Number.isFinite(seconds) && seconds >= MIN_EXTEND && seconds <= MAX_EXTEND ? null : '补偿时长需在 1 分钟到 30 天之间'
}

export function validateRefund(yuan: string, maxCents: number): { cents: number } | { error: string } {
  const cents = yuanToCents(yuan)
  if (cents == null) return { error: '金额格式不正确' }
  if (cents > maxCents) return { error: '退款金额不能超过可退上限' }
  return { cents }
}

const opsRole = (r: Role) => r === 'ops' || r === 'super'
const finRole = (r: Role) => r === 'finance' || r === 'super'
const extendable = (s: InstanceState) => s === 'creating' || s === 'running' || s === 'stopped'

export const showExtend = (r: Role, s: InstanceState) => opsRole(r) && extendable(s)
export const showMigrate = (r: Role, s: InstanceState) => opsRole(r) && s === 'running'
export const showRewipe = (r: Role, s: InstanceState) => opsRole(r) && s === 'wiping'
export const showRefund = (r: Role, s: InstanceState) => finRole(r) && extendable(s)
```

`web/admin/src/views/InstancesView.vue`：

```vue
<script setup lang="ts">
import { fmtDuration, fmtTime, instanceStateLabel, instanceStateType, labelOf, type AdminImage, type AdminInstance, type Listed, type Paged } from '@spark/shared'
import { computed, onMounted, reactive, ref } from 'vue'
import { useRoute } from 'vue-router'
import { api } from '../api'
import IdLink from '../components/IdLink.vue'
import { usePaged } from '../composables/usePaged'
import { session } from '../store/session'

const route = useRoute()
const filter = reactive({ state: String(route.query.state ?? ''), user_id: String(route.query.user_id ?? ''), machine_id: String(route.query.machine_id ?? '') })
const list = usePaged<AdminInstance>((page, size) => api.get<Paged<AdminInstance>>('/instances', { state: filter.state, user_id: filter.user_id.trim(), machine_id: filter.machine_id.trim(), page, page_size: size }))
const images = ref<AdminImage[]>([])
const imageName = computed(() => new Map(images.value.map((i) => [i.id, i.name])))
const isFinance = computed(() => session.admin?.role === 'finance')
const digits = (v: string) => v.replace(/\D/g, '')

onMounted(async () => {
  await list.load()
  images.value = (await api.get<Listed<AdminImage>>('/images')).items
})
function resetFilter() {
  Object.assign(filter, { state: '', user_id: '', machine_id: '' })
  list.reset()
}
const timeText = (i: AdminInstance) => (i.state === 'running' ? `剩余 ${fmtDuration(i.remaining_seconds)}` : i.pending_seconds > 0 ? `待开始 ${fmtDuration(i.pending_seconds)}` : '—')
</script>

<template>
  <el-card>
    <el-form inline class="filters" @submit.prevent="list.reset()">
      <el-form-item label="状态">
        <el-select v-model="filter.state" clearable placeholder="全部" data-testid="ainst-filter-state" style="width: 140px">
          <el-option v-for="(label, v) in instanceStateLabel" :key="v" :label="label" :value="v" />
        </el-select>
      </el-form-item>
      <el-form-item label="用户 ID"><el-input :model-value="filter.user_id" clearable data-testid="ainst-filter-user" style="width: 120px" @update:model-value="(v: string) => (filter.user_id = digits(v))" /></el-form-item>
      <el-form-item label="机器 ID"><el-input :model-value="filter.machine_id" clearable data-testid="ainst-filter-machine" style="width: 120px" @update:model-value="(v: string) => (filter.machine_id = digits(v))" /></el-form-item>
      <el-button type="primary" native-type="submit" data-testid="ainst-search">查询</el-button>
      <el-button data-testid="ainst-reset" @click="resetFilter">重置</el-button>
    </el-form>
    <el-table :data="list.items.value" v-loading="list.loading.value" row-key="id">
      <el-table-column label="实例" min-width="160"><template #default="{ row }"><IdLink kind="instance" :id="row.id" data-testid="ainst-id" /></template></el-table-column>
      <el-table-column label="用户" width="90"><template #default="{ row }"><IdLink kind="user" :id="row.user_id" data-testid="ainst-user" /></template></el-table-column>
      <el-table-column label="机器" width="90"><template #default="{ row }"><IdLink kind="machine" :id="row.machine_id" :disabled="isFinance" data-testid="ainst-machine" /></template></el-table-column>
      <el-table-column label="镜像" min-width="140"><template #default="{ row }"><span data-testid="ainst-image">{{ imageName.get(row.image_id) ?? row.image_id }}</span></template></el-table-column>
      <el-table-column label="状态" width="150">
        <template #default="{ row }">
          <el-tag :type="labelOf(instanceStateType, row.state) as 'primary'" data-testid="ainst-state">{{ labelOf(instanceStateLabel, row.state) }}</el-tag>
          <el-tag v-if="row.suspended" type="warning" size="small" data-testid="ainst-suspended">计时已暂停</el-tag>
        </template>
      </el-table-column>
      <el-table-column label="剩余 / 待开始" min-width="160"><template #default="{ row }"><span data-testid="ainst-time">{{ timeText(row) }}</span></template></el-table-column>
      <el-table-column label="到期时间" width="160"><template #default="{ row }"><span data-testid="ainst-expires">{{ fmtTime(row.expires_at) || '—' }}</span></template></el-table-column>
      <el-table-column label="创建时间" width="160"><template #default="{ row }"><span data-testid="ainst-created">{{ fmtTime(row.created_at) }}</span></template></el-table-column>
    </el-table>
    <div class="pager-row">
      <span class="muted" data-testid="ainst-total">共 {{ list.total.value }} 个</span>
      <el-pagination v-model:current-page="list.page.value" v-model:page-size="list.size.value" :total="list.total.value" layout="prev, pager, next" data-testid="ainst-pager" />
    </div>
  </el-card>
</template>

<style scoped>
.filters { display: flex; flex-wrap: wrap; gap: 8px 16px; align-items: center; margin-bottom: 12px; }
.pager-row { display: flex; justify-content: space-between; align-items: center; margin-top: 12px; }
.muted { color: #909399; }
</style>
```

`web/admin/src/views/InstanceDetailView.vue`：

```vue
<script setup lang="ts">
import {
  commandStatusLabel, commandTypeLabel, durationLabel, fmtDuration, fmtTime, fmtYuan, instanceStateLabel, instanceStateType, isApiError, labelOf, messageOf,
  orderKindLabel, orderStatusLabel, type AdminImage, type AdminInstance, type AdminInstanceDetail, type AdminProduct, type Listed, type RefundQuote,
} from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, onMounted, reactive, ref } from 'vue'
import { useRoute } from 'vue-router'
import { api } from '../api'
import IdLink from '../components/IdLink.vue'
import { extendSeconds, showExtend, showMigrate, showRefund, showRewipe, validateExtend, validateRefund } from '../logic/instanceOps'
import { session } from '../store/session'

const route = useRoute()
const id = computed(() => String(route.params.id))
const data = ref<AdminInstanceDetail | null>(null)
const images = ref<AdminImage[]>([])
const products = ref<AdminProduct[]>([])
const role = computed(() => session.admin?.role ?? 'finance')
const inst = computed(() => data.value?.instance)
const imageName = computed(() => images.value.find((i) => i.id === inst.value?.image_id)?.name ?? inst.value?.image_id)
const productName = computed(() => products.value.find((p) => p.id === inst.value?.product_id)?.name ?? inst.value?.product_id)
const isFinance = computed(() => role.value === 'finance')

async function load() {
  try {
    data.value = await api.get<AdminInstanceDetail>(`/instances/${id.value}`)
  } catch (e) {
    ElMessage.error(messageOf(e))
  }
}
onMounted(async () => {
  await load()
  const [i, p] = await Promise.all([api.get<Listed<AdminImage>>('/images'), api.get<Listed<AdminProduct>>('/products')])
  images.value = i.items
  products.value = p.items
})

// ---- 免费延长（ops）----
const extend = reactive({ visible: false, amount: 1, unit: 'day' as 'hour' | 'day', reason: '', error: '', busy: false })
async function submitExtend() {
  const seconds = extendSeconds(extend.amount, extend.unit)
  extend.error = validateExtend(seconds) ?? (extend.reason.trim() ? '' : '请填写补偿原因')
  if (extend.error) return
  extend.busy = true
  try {
    await api.post<AdminInstance>(`/instances/${id.value}/extend`, { seconds, reason: extend.reason.trim() })
    ElMessage.success(`已延长 ${fmtDuration(seconds)}`)
    extend.visible = false
    await load()
  } catch (e) {
    ElMessage.error(messageOf(e))
  } finally {
    extend.busy = false
  }
}

// ---- 换机（ops）----
const migrate = reactive({ visible: false, reason: '', busy: false, newId: '' })
async function submitMigrate() {
  if (!migrate.reason.trim()) return ElMessage.warning('请填写换机原因')
  migrate.busy = true
  try {
    const out = await api.post<{ instance: AdminInstance }>(`/instances/${id.value}/migrate`, { reason: migrate.reason.trim() })
    migrate.visible = false
    migrate.newId = out.instance.id // 认领计划 4 遗留：换机后 SSH 密码变化，提示运营通知用户
    await load()
  } catch (e) {
    ElMessage.error(messageOf(e)) // no_capacity → 「暂无空闲机器，请稍后再试」
  } finally {
    migrate.busy = false
  }
}

// ---- 重新洗机（ops）----
const rewipe = reactive({ visible: false, busy: false })
async function submitRewipe() {
  rewipe.busy = true
  try {
    await api.post(`/instances/${id.value}/rewipe`, {})
    ElMessage.success('已重新下发洗机指令')
    rewipe.visible = false
    await load()
  } catch (e) {
    ElMessage.error(messageOf(e))
  } finally {
    rewipe.busy = false
  }
}

// ---- 退款并释放（finance；动态码由请求层弹出）----
const refund = reactive({ visible: false, quote: null as RefundQuote | null, amount: '', reason: '', error: '', busy: false })
async function openRefund() {
  try {
    refund.quote = await api.get<RefundQuote>(`/instances/${id.value}/refund-quote`)
    refund.amount = (refund.quote.suggested / 100).toFixed(2)
    refund.reason = ''
    refund.error = ''
    refund.visible = true
  } catch (e) {
    ElMessage.error(messageOf(e))
  }
}
async function submitRefund() {
  if (!refund.quote) return
  const v = validateRefund(refund.amount, refund.quote.max)
  if ('error' in v) {
    refund.error = v.error
    return
  }
  if (!refund.reason.trim()) {
    refund.error = '请填写退款原因'
    return
  }
  refund.error = ''
  refund.busy = true
  try {
    const out = await api.post<{ refunded: number }>(`/instances/${id.value}/refund-release`, { amount: v.cents, reason: refund.reason.trim() })
    ElMessage.success(`已退款 ${fmtYuan(out.refunded)}，实例进入洗机`)
    refund.visible = false
    await load()
  } catch (e) {
    if (!(isApiError(e) && e.code === 'cancelled')) ElMessage.error(messageOf(e)) // 用户取消动态码：静默
  } finally {
    refund.busy = false
  }
}
</script>

<template>
  <div v-if="inst" class="detail">
    <el-card>
      <template #header>
        <div class="head"><span class="mono" data-testid="adetail-id">{{ inst.id }}</span>
          <span><el-tag :type="labelOf(instanceStateType, inst.state) as 'primary'" data-testid="adetail-state">{{ labelOf(instanceStateLabel, inst.state) }}</el-tag>
            <el-tag v-if="inst.suspended" type="warning" data-testid="adetail-suspended">计时已暂停</el-tag></span>
        </div>
      </template>
      <el-descriptions :column="3" border>
        <el-descriptions-item label="用户"><IdLink kind="user" :id="inst.user_id" data-testid="adetail-user" /></el-descriptions-item>
        <el-descriptions-item label="机器"><IdLink kind="machine" :id="inst.machine_id" :disabled="isFinance" data-testid="adetail-machine" /></el-descriptions-item>
        <el-descriptions-item label="商品"><span data-testid="adetail-product">{{ productName }}</span></el-descriptions-item>
        <el-descriptions-item label="镜像"><span data-testid="adetail-image">{{ imageName }}</span></el-descriptions-item>
        <el-descriptions-item label="剩余时长"><span data-testid="adetail-remaining" :data-remaining="inst.remaining_seconds">{{ inst.state === 'running' ? fmtDuration(inst.remaining_seconds) : '—' }}</span></el-descriptions-item>
        <el-descriptions-item label="待开始"><span data-testid="adetail-pending">{{ inst.pending_seconds > 0 ? fmtDuration(inst.pending_seconds) : '—' }}</span></el-descriptions-item>
        <el-descriptions-item label="到期时间"><span data-testid="adetail-expires">{{ fmtTime(inst.expires_at) || '—' }}</span></el-descriptions-item>
        <el-descriptions-item label="停机时间">{{ fmtTime(inst.stopped_at) || '—' }}</el-descriptions-item>
        <el-descriptions-item label="创建时间">{{ fmtTime(inst.created_at) }}</el-descriptions-item>
      </el-descriptions>
      <div class="actions">
        <el-button v-if="showExtend(role, inst.state)" data-testid="adetail-extend-btn" @click="extend.visible = true">免费延长</el-button>
        <el-button v-if="showMigrate(role, inst.state)" data-testid="adetail-migrate-btn" @click="migrate.visible = true">换机</el-button>
        <el-button v-if="showRewipe(role, inst.state)" data-testid="adetail-rewipe-btn" @click="rewipe.visible = true">重新洗机</el-button>
        <el-button v-if="showRefund(role, inst.state)" type="danger" plain data-testid="adetail-refund-btn" @click="openRefund">退款并释放</el-button>
      </div>
    </el-card>

    <div class="two">
      <el-card header="状态时间线">
        <el-timeline>
          <el-timeline-item v-for="e in data!.events" :key="e.id" :timestamp="fmtTime(e.at)">
            <span data-testid="adetail-event">{{ e.from_state || '—' }} → {{ e.to_state }} · {{ e.reason }} · {{ fmtTime(e.at) }}</span>
          </el-timeline-item>
        </el-timeline>
      </el-card>
      <el-card header="订单">
        <el-table :data="data!.orders" row-key="id" size="small">
          <el-table-column label="订单号" min-width="150"><template #default="{ row }"><span class="mono" data-testid="adetail-order-id">{{ row.id }}</span></template></el-table-column>
          <el-table-column label="类型" width="70"><template #default="{ row }"><span data-testid="adetail-order-kind">{{ labelOf(orderKindLabel, row.kind) }}</span></template></el-table-column>
          <el-table-column label="租期" width="70"><template #default="{ row }">{{ labelOf(durationLabel, row.duration) }}</template></el-table-column>
          <el-table-column label="金额" width="100"><template #default="{ row }"><span data-testid="adetail-order-amount">{{ fmtYuan(row.amount) }}</span></template></el-table-column>
          <el-table-column label="已退" width="100"><template #default="{ row }"><span data-testid="adetail-order-refunded">{{ fmtYuan(row.refunded_amount) }}</span></template></el-table-column>
          <el-table-column label="状态" width="90"><template #default="{ row }"><span data-testid="adetail-order-status">{{ labelOf(orderStatusLabel, row.status) }}</span></template></el-table-column>
          <el-table-column label="时间" width="150"><template #default="{ row }">{{ fmtTime(row.created_at) }}</template></el-table-column>
        </el-table>
      </el-card>
    </div>
    <el-card header="指令">
      <el-table :data="data!.commands" row-key="id" size="small">
        <el-table-column label="类型" width="120"><template #default="{ row }"><span data-testid="adetail-cmd-type">{{ labelOf(commandTypeLabel, row.type) }}</span></template></el-table-column>
        <el-table-column label="状态" width="90"><template #default="{ row }"><span data-testid="adetail-cmd-status">{{ labelOf(commandStatusLabel, row.status) }}</span></template></el-table-column>
        <el-table-column label="尝试" width="70"><template #default="{ row }"><span data-testid="adetail-cmd-attempts">{{ row.attempts }}</span></template></el-table-column>
        <el-table-column label="结果" min-width="200"><template #default="{ row }"><span data-testid="adetail-cmd-result">{{ row.result }}</span></template></el-table-column>
        <el-table-column label="时间" width="150"><template #default="{ row }">{{ fmtTime(row.created_at) }}</template></el-table-column>
      </el-table>
    </el-card>

    <el-dialog v-model="extend.visible" title="免费延长" width="460" align-center destroy-on-close>
      <el-form label-position="top" @submit.prevent="submitExtend">
        <div class="two">
          <el-form-item label="时长"><el-input-number v-model="extend.amount" :min="0" :controls="false" data-testid="extend-amount" /></el-form-item>
          <el-form-item label="单位">
            <el-select v-model="extend.unit" data-testid="extend-unit"><el-option label="小时" value="hour" /><el-option label="天" value="day" /></el-select>
          </el-form-item>
        </div>
        <p class="muted">范围 1 分钟～30 天</p>
        <el-form-item label="原因（必填）"><el-input v-model="extend.reason" data-testid="extend-reason" /></el-form-item>
        <p v-if="extend.error" class="error" data-testid="extend-error">{{ extend.error }}</p>
        <el-button type="primary" native-type="submit" :loading="extend.busy" data-testid="extend-submit">确定延长</el-button>
      </el-form>
    </el-dialog>

    <el-dialog v-model="migrate.visible" title="换机" width="460" align-center destroy-on-close>
      <p>将在另一台空闲机器上用相同镜像重建实例，剩余时长转移；原实例进入洗机。</p>
      <el-form label-position="top" @submit.prevent="submitMigrate">
        <el-form-item label="原因（必填）"><el-input v-model="migrate.reason" data-testid="migrate-reason" /></el-form-item>
        <el-button type="primary" native-type="submit" :loading="migrate.busy" data-testid="migrate-submit">确定换机</el-button>
      </el-form>
    </el-dialog>
    <el-dialog :model-value="!!migrate.newId" title="换机成功" width="460" align-center @close="migrate.newId = ''">
      <p data-testid="migrate-result">已在新机器上创建实例 <IdLink kind="instance" :id="migrate.newId" data-testid="adetail-migrate-new" />，SSH 密码已变化，请通知用户。</p>
    </el-dialog>

    <el-dialog v-model="rewipe.visible" title="重新洗机" width="420" align-center>
      <p>将重新向机器下发洗机指令。</p>
      <template #footer>
        <el-button @click="rewipe.visible = false">取消</el-button>
        <el-button type="primary" :loading="rewipe.busy" data-testid="rewipe-ok" @click="submitRewipe">确定</el-button>
      </template>
    </el-dialog>

    <el-dialog v-model="refund.visible" title="退款并释放" width="480" align-center destroy-on-close>
      <el-form v-if="refund.quote" label-position="top" @submit.prevent="submitRefund">
        <el-form-item label="退款金额（元）"><el-input v-model="refund.amount" inputmode="decimal" data-testid="refund-amount" /></el-form-item>
        <p class="muted" data-testid="refund-hint">建议 {{ fmtYuan(refund.quote.suggested) }}（按剩余时长 {{ fmtDuration(refund.quote.remaining_seconds) }} 折算），最多可退 {{ fmtYuan(refund.quote.max) }}</p>
        <el-form-item label="原因（必填）"><el-input v-model="refund.reason" data-testid="refund-reason" /></el-form-item>
        <p v-if="refund.error" class="error" data-testid="refund-error">{{ refund.error }}</p>
        <el-button type="danger" native-type="submit" :loading="refund.busy" data-testid="refund-submit">退款并释放</el-button>
      </el-form>
    </el-dialog>
  </div>
</template>

<style scoped>
.detail { display: grid; gap: 16px; }
.head { display: flex; justify-content: space-between; align-items: center; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.muted { color: #909399; font-size: 13px; }
.error { color: var(--el-color-danger); font-size: 13px; }
.actions { display: flex; gap: 12px; margin-top: 16px; }
.two { display: grid; grid-template-columns: 1fr 1fr; gap: 16px; }
</style>
```

Run: `cd web && npm test && npm run build`
Expected: Vitest PASS（instanceOps 3）；构建通过。

- [ ] **Step 3: 端到端用例**

`web/e2e/admin/instances.spec.ts`：

```ts
import { expect, test, type Page } from '@playwright/test'
import { adminApi, createAdmin, drainIdleMachines, enterTotpDialog, loginAdmin, totpNow } from '../helpers/admin'
import { placeOrder, seedUser, toast, userApi } from '../helpers/console'
import { hooks } from '../helpers/hooks'
import { runningInstance } from '../helpers/instances'

type AdminInst = { id: string; user_id: number; machine_id: number; state: string; remaining_seconds: number }

async function adminInstance(admin: Awaited<ReturnType<typeof createAdmin>>, id: string): Promise<AdminInst> {
  const ctx = await adminApi(admin)
  return ((await (await ctx.get(`/admin/api/v1/instances/${id}`)).json()) as { instance: AdminInst }).instance
}

const rows = (page: Page) => page.getByRole('row').filter({ has: page.getByTestId('ainst-id') })

test('A-INS-01 实例列表按状态 / 用户 / 机器筛选', async ({ page }) => {
  const a = await seedUser()
  const b = await seedUser()
  const ia = await runningInstance(a.phone)
  const ib = await runningInstance(b.phone)
  const admin = await createAdmin('finance')
  const detail = await adminInstance(admin, ia.id)
  await loginAdmin(page, admin)
  await page.getByTestId('menu-instances').click()
  await page.getByTestId('ainst-filter-user').fill(String(a.id))
  await page.getByTestId('ainst-search').click()
  await expect(rows(page)).toHaveCount(1)
  await expect(rows(page).first().getByTestId('ainst-id')).toHaveText(ia.id)
  await expect(rows(page).first().getByTestId('ainst-machine')).toHaveText(String(detail.machine_id)) // finance 只显示 ID
  await expect(rows(page).first().getByTestId('ainst-machine').locator('a')).toHaveCount(0)
  await page.getByTestId('ainst-filter-user').fill('')
  await page.getByTestId('ainst-filter-machine').fill(String(detail.machine_id))
  await page.getByTestId('ainst-search').click()
  await expect(rows(page).first().getByTestId('ainst-id')).toHaveText(ia.id)
  await page.getByTestId('ainst-reset').click()
  await page.getByTestId('ainst-filter-user').fill(String(b.id))
  await page.getByTestId('ainst-filter-state').click()
  await page.getByRole('option', { name: '开通失败' }).click()
  await page.getByTestId('ainst-search').click()
  await expect(page.getByTestId('ainst-total')).toHaveText('共 0 个')
  await page.getByTestId('ainst-filter-state').click()
  await page.getByRole('option', { name: '运行中' }).click()
  await page.getByTestId('ainst-search').click()
  await expect(rows(page).first().getByTestId('ainst-id')).toHaveText(ib.id)
  await expect(rows(page).first().getByTestId('ainst-state')).toHaveText('运行中')
})

test('A-INS-02 实例详情：基本信息、时间线、订单、指令', async ({ page }) => {
  const u = await seedUser()
  const { id } = await runningInstance(u.phone)
  const admin = await createAdmin('ops')
  await loginAdmin(page, admin)
  await page.goto(`/admin/instances/${id}`)
  await expect(page.getByTestId('adetail-id')).toHaveText(id)
  await expect(page.getByTestId('adetail-user')).toHaveText(String(u.id))
  await expect(page.getByTestId('adetail-image')).toHaveText('Ubuntu 24.04 + CUDA')
  await expect(page.getByTestId('adetail-event').filter({ hasText: 'creating → running' })).toHaveCount(1)
  await expect(page.getByTestId('adetail-order-kind')).toHaveText(['新购'])
  await expect(page.getByTestId('adetail-order-amount')).toHaveText(['¥69.00'])
  await expect(page.getByTestId('adetail-cmd-type')).toHaveText(['开通实例'])
  await expect(page.getByTestId('adetail-cmd-status')).toHaveText(['已完成'])
})

test('A-INS-03 免费延长 2 天；31 天被前端拦截', async ({ page }) => {
  const u = await seedUser()
  const { id } = await runningInstance(u.phone)
  const admin = await createAdmin('ops')
  let posts = 0
  page.on('request', (r) => {
    if (r.method() === 'POST' && r.url().endsWith('/extend')) posts += 1
  })
  await loginAdmin(page, admin)
  await page.goto(`/admin/instances/${id}`)
  const before = Number(await page.getByTestId('adetail-remaining').getAttribute('data-remaining'))
  await page.getByTestId('adetail-extend-btn').click()
  await page.getByTestId('extend-amount').locator('input').fill('31')
  await page.getByTestId('extend-reason').fill('机器故障补偿')
  await page.getByTestId('extend-submit').click()
  await expect(page.getByTestId('extend-error')).toHaveText('补偿时长需在 1 分钟到 30 天之间')
  expect(posts).toBe(0)
  await page.getByTestId('extend-amount').locator('input').fill('2')
  await page.getByTestId('extend-submit').click()
  await toast(page, '已延长 2 天')
  const after = Number(await page.getByTestId('adetail-remaining').getAttribute('data-remaining'))
  expect(after - before).toBeGreaterThanOrEqual(172800 - 5)
  expect(after - before).toBeLessThanOrEqual(172800)
  const ctx = await adminApi(admin)
  expect(((await (await ctx.get(`/admin/api/v1/audit?action=instance.extend&target_id=${id}`)).json()) as { total: number }).total).toBe(1)
})

test('A-INS-04 换机：无空闲机器提示；有空闲机器成功并提示密码变化', async ({ page }) => {
  const admin = await createAdmin('ops')
  await drainIdleMachines(await adminApi(admin), admin)
  const u = await seedUser()
  const { id } = await runningInstance(u.phone) // 唯一空闲机器被占用
  await loginAdmin(page, admin)
  await page.goto(`/admin/instances/${id}`)
  await page.getByTestId('adetail-migrate-btn').click()
  await page.getByTestId('migrate-reason').fill('硬件告警')
  await page.getByTestId('migrate-submit').click()
  await toast(page, '暂无空闲机器')
  await hooks.machines(1)
  await page.getByTestId('migrate-submit').click()
  const result = page.getByTestId('migrate-result')
  await expect(result).toContainText('SSH 密码已变化，请通知用户')
  const newId = (await page.getByTestId('adetail-migrate-new').textContent())!.trim()
  expect(newId).toMatch(/^ins-/)
  expect(newId).not.toBe(id)
  await expect(page.getByTestId('adetail-state')).toHaveText('洗机中')
})

test('A-INS-05 洗机失败后重新洗机', async ({ page }) => {
  const u = await seedUser()
  const { api, id } = await runningInstance(u.phone)
  await api.post(`/api/v1/instances/${id}/release`, { data: {} })
  await hooks.agent({ fail: true })
  const admin = await createAdmin('ops')
  await loginAdmin(page, admin)
  await page.goto(`/admin/instances/${id}`)
  await expect(page.getByTestId('adetail-state')).toHaveText('洗机中')
  await expect(page.getByTestId('adetail-cmd-type').filter({ hasText: '洗机' })).toHaveCount(1)
  await page.getByTestId('adetail-rewipe-btn').click()
  await page.getByTestId('rewipe-ok').click()
  await toast(page, '已重新下发洗机指令')
  await expect(page.getByTestId('adetail-cmd-type').filter({ hasText: '洗机' })).toHaveCount(2)
})

test('A-INS-06 退款并释放：建议金额、上限校验、动态码、余额与状态', async ({ page }) => {
  const u = await seedUser({ balance: 100000 })
  const { api, id } = await runningInstance(u.phone)
  const admin = await createAdmin('finance')
  await loginAdmin(page, admin)
  await page.goto(`/admin/instances/${id}`)
  await page.getByTestId('adetail-refund-btn').click()
  const prefilled = Number(await page.getByTestId('refund-amount').inputValue())
  expect(prefilled).toBeGreaterThan(0)
  expect(prefilled).toBeLessThanOrEqual(69)
  await expect(page.getByTestId('refund-hint')).toContainText('最多可退 ¥')
  await page.getByTestId('refund-amount').fill('69.01')
  await page.getByTestId('refund-reason').fill('机器故障')
  await page.getByTestId('refund-submit').click()
  await expect(page.getByTestId('refund-error')).toHaveText('退款金额不能超过可退上限')
  await page.getByTestId('refund-amount').fill(prefilled.toFixed(2))
  await page.getByTestId('refund-submit').click()
  await enterTotpDialog(page, admin.totp_secret!)
  await toast(page, '已退款 ¥')
  await expect(page.getByTestId('adetail-state')).toHaveText('洗机中')
  const balance = (await (await api.get('/api/v1/wallet')).json()).balance as number
  expect(balance).toBe(100000 - 6900 + Math.round(prefilled * 100))
})

test('A-INS-07 五分钟内连续两次高风险操作只弹一次动态码', async ({ page }) => {
  const u = await seedUser({ balance: 100000 })
  const first = await runningInstance(u.phone)
  await hooks.machines(1)
  const secondId = await placeOrder(first.api)
  await hooks.agent()
  const admin = await createAdmin('finance')
  await loginAdmin(page, admin)
  for (const [i, id] of [first.id, secondId].entries()) {
    await page.goto(`/admin/instances/${id}`)
    await page.getByTestId('adetail-refund-btn').click()
    await page.getByTestId('refund-reason').fill('批量退款')
    await page.getByTestId('refund-submit').click()
    if (i === 0) await enterTotpDialog(page, admin.totp_secret!)
    else await expect(page.getByTestId('totp-dialog')).toBeHidden()
    await toast(page, '已退款 ¥')
    await expect(page.getByTestId('adetail-state')).toHaveText('洗机中')
  }
})

test('A-INS-08 动态码取消不执行；错误码提示后可重试', async ({ page }) => {
  const u = await seedUser({ balance: 100000 })
  const { api, id } = await runningInstance(u.phone)
  const admin = await createAdmin('finance')
  await loginAdmin(page, admin)
  await page.goto(`/admin/instances/${id}`)
  await page.getByTestId('adetail-refund-btn').click()
  await page.getByTestId('refund-reason').fill('测试取消')
  await page.getByTestId('refund-submit').click()
  await expect(page.getByTestId('totp-dialog')).toBeVisible()
  await page.getByTestId('totp-cancel').click()
  await expect(page.getByTestId('totp-dialog')).toBeHidden()
  await page.reload()
  await expect(page.getByTestId('adetail-state')).toHaveText('运行中')
  expect((await (await api.get('/api/v1/wallet')).json()).balance).toBe(93100)

  await page.getByTestId('adetail-refund-btn').click()
  await page.getByTestId('refund-reason').fill('测试重试')
  await page.getByTestId('refund-submit').click()
  await page.getByTestId('totp-input').fill('000000')
  await page.getByTestId('totp-ok').click()
  await expect(page.getByTestId('totp-message')).toContainText('动态码错误或已使用过')
  await page.getByTestId('totp-input').fill(await totpNow(admin.totp_secret!))
  await page.getByTestId('totp-ok').click()
  await toast(page, '已退款 ¥')
})

test('A-INS-09 操作按钮按角色', async ({ page }) => {
  const u = await seedUser()
  const { id } = await runningInstance(u.phone)
  const fin = await createAdmin('finance')
  await loginAdmin(page, fin)
  await page.goto(`/admin/instances/${id}`)
  await expect(page.getByTestId('adetail-refund-btn')).toBeVisible()
  for (const t of ['adetail-extend-btn', 'adetail-migrate-btn', 'adetail-rewipe-btn']) await expect(page.getByTestId(t)).toHaveCount(0)
  await page.getByTestId('layout-logout').click()
  const ops = await createAdmin('ops')
  await loginAdmin(page, ops)
  await page.goto(`/admin/instances/${id}`)
  await expect(page.getByTestId('adetail-extend-btn')).toBeVisible()
  await expect(page.getByTestId('adetail-migrate-btn')).toBeVisible()
  await expect(page.getByTestId('adetail-refund-btn')).toHaveCount(0)
})
```

`web/e2e/admin/errors.spec.ts`：

```ts
import { expect, test } from '@playwright/test'
import { adminApi, adminPut, createAdmin, loginAdmin } from '../helpers/admin'
import { toast } from '../helpers/console'

test('A-ERR-01 会话中途被降权：接口 403 以提示显示而不是白屏', async ({ page }) => {
  // 真实路径：super 正在看机器页，另一位 super 把他降为 finance。后端每次请求都从 admins 表重读角色（降级不删会话），
  // 页面里缓存的角色仍是 super，所以下一次 GET /machines 得到真实的 403 forbidden。不用 page.route 篡改请求。
  const root = await createAdmin('super')
  const victim = await createAdmin('super')
  await loginAdmin(page, victim)
  await page.getByTestId('menu-machines').click()
  await expect(page.getByTestId('machines-total')).toHaveText(/共 \d+ 台/)
  await adminPut(await adminApi(root), root, `/admin/api/v1/admins/${victim.id}`, { role: 'finance' })
  await page.getByTestId('machines-search').click()
  await toast(page, '无权执行该操作')
  // 不白屏：筛选栏、布局、用户名都还在，没有被踢到登录页
  await expect(page.getByTestId('machines-search')).toBeVisible()
  await expect(page.getByTestId('layout-username')).toHaveText(victim.username)
  await expect(page).toHaveURL(/\/admin\/machines$/)
  // 刷新后前端拿到新角色：机器页变成 403 页，菜单里不再有「机器」
  await page.reload()
  await expect(page.getByTestId('forbidden-page')).toBeVisible()
  await page.getByTestId('forbidden-home').click()
  await expect(page.getByTestId('menu-machines')).toHaveCount(0)
  await expect(page.getByTestId('layout-role')).toHaveText('财务')
})
```

Run: `make web && cd web && npx playwright test --project=admin admin/instances.spec.ts admin/errors.spec.ts`
Expected: 10 passed。

- [ ] **Step 4: 全量验证并提交**

Run: `go vet ./... && go test ./... && cd web && npm test && npm run build && npx playwright test --project=admin`
Expected: 全部 PASS。

```bash
git add web/shared/src/labels.ts web/admin web/e2e
git commit -m "feat(admin): instance list/detail with extend, migrate, rewipe and refund-release (TOTP step-up); e2e A-INS, A-ERR-01

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---
### Task 19: 后台用户、封禁、调账、手动入账与企业认证审核

**Files:**
- Replace: `web/admin/src/views/{UsersView,UserDetailView,KycView}.vue`
- Create: `web/admin/src/logic/money.ts`、`web/admin/src/logic/__tests__/money.spec.ts`
- Create: `web/e2e/admin/{users,kyc}.spec.ts`
- Modify: `web/e2e/helpers/console.ts`（追加 `uploadLicense`、`submitEnterpriseKyc`）

**Interfaces:**
- Consumes: Task 15～18 的 `api`、`session`、`usePaged`、`IdLink`、`enterTotpDialog`、`totpNow`；shared `labels`、`fmtYuan`、`yuanToCents`。
- Produces:
  - `src/logic/money.ts`：`parseSignedYuan(input): number | null`（允许前导 `-`，非零，复用 `yuanToCents`）。
  - 用户列表 testid：`users-q`（只允许数字）、`users-filter-kyc`、`users-filter-status`、`users-search`、`users-reset`、`user-id`、`user-phone`、`user-kind`、`user-kyc`、`user-status`、`user-created`、`users-total`、`users-pager`。
  - 用户详情 testid：`udetail-id`、`udetail-phone`、`udetail-kind`、`udetail-kyc`、`udetail-status`、`udetail-balance`；标签「实例」「订单」「流水」「实名记录」；`udetail-instance-row`、`udetail-order-row`、`udetail-ledger-row`（`udetail-ledger-kind`、`udetail-ledger-amount`、`udetail-ledger-after`、`udetail-ledger-remark`）、`udetail-kyc-row`（`udetail-kyc-kind`、`udetail-kyc-name`、`udetail-kyc-idno`、`udetail-kyc-result`、`udetail-kyc-reason`）；`udetail-more-instances`、`udetail-more-orders`、`udetail-more-ledger`、`udetail-more-recharges`（后三个仅 finance / super）；操作：`udetail-ban-btn`、`ban-reason`、`ban-error`、`ban-submit`、`udetail-unban-btn`、`unban-ok`、`udetail-adjust-btn`、`adjust-amount`、`adjust-remark`、`adjust-error`、`adjust-submit`、`adjust-confirm-text`（「将把余额从 ¥a 调整为 ¥b」）、`adjust-confirm-ok`、`udetail-recharge-btn`、`mrecharge-amount`、`mrecharge-remark`、`mrecharge-error`、`mrecharge-submit`。
  - 企业审核 testid：`kyc-row`、`kyc-company`、`kyc-license-no`、`kyc-user`、`kyc-submitted`、`kyc-review-btn`；弹窗 `kyc-review-dialog`、`kyc-preview-img`、`kyc-open-file`、`kyc-open-pdf`、`kyc-approve`、`kyc-approve-ok`、`kyc-reject`、`kyc-reject-reason`、`kyc-reject-error`、`kyc-reject-submit`。
  - e2e helpers：`uploadLicense(api, name: 'license.png' | 'license.pdf'): Promise<string>`（返回 key）、`submitEnterpriseKyc(api, { company, licenseNo, file })`。
  - 覆盖：A-USR-01～06、A-KYC-01、A-KYC-02。

- [ ] **Step 1: 写失败的单测**

`web/admin/src/logic/__tests__/money.spec.ts`：

```ts
import { describe, expect, it } from 'vitest'
import { parseSignedYuan } from '../money'

describe('parseSignedYuan', () => {
  it.each([
    ['10', 1000],
    ['-5', -500],
    ['-0.5', -50],
    [' 12.34 ', 1234],
    ['0', null],
    ['abc', null],
    ['1.234', null],
    ['--1', null],
  ])('%s -> %s', (input, want) => {
    expect(parseSignedYuan(input)).toBe(want)
  })
})
```

Run: `cd web && npm test`
Expected: FAIL（模块不存在）。

- [ ] **Step 2: 逻辑与三个页面**

`web/admin/src/logic/money.ts`：

```ts
import { yuanToCents } from '@spark/shared'

/** 调账金额：元，允许负数，不允许 0；返回分。 */
export function parseSignedYuan(input: string): number | null {
  const s = input.trim()
  const negative = s.startsWith('-')
  const cents = yuanToCents(negative ? s.slice(1) : s)
  if (cents == null || cents === 0) return null
  return negative ? -cents : cents
}
```

`web/admin/src/views/UsersView.vue`：

```vue
<script setup lang="ts">
import { fmtTime, kycStatusLabel, labelOf, userKindLabel, userStatusLabel, type Paged, type User } from '@spark/shared'
import { onMounted, reactive } from 'vue'
import { api } from '../api'
import IdLink from '../components/IdLink.vue'
import { usePaged } from '../composables/usePaged'

const filter = reactive({ q: '', kyc_status: '', status: '' })
const list = usePaged<User>((page, size) => api.get<Paged<User>>('/users', { q: filter.q, kyc_status: filter.kyc_status, status: filter.status, page, page_size: size }))
onMounted(list.load)
function resetFilter() {
  Object.assign(filter, { q: '', kyc_status: '', status: '' })
  list.reset()
}
</script>

<template>
  <el-card>
    <el-form inline class="filters" @submit.prevent="list.reset()">
      <el-form-item label="手机号 / 尾号 / ID">
        <el-input :model-value="filter.q" clearable placeholder="只能输入数字" data-testid="users-q" style="width: 200px" @update:model-value="(v: string) => (filter.q = v.replace(/\D/g, ''))" />
      </el-form-item>
      <el-form-item label="实名">
        <el-select v-model="filter.kyc_status" clearable placeholder="全部" data-testid="users-filter-kyc" style="width: 120px">
          <el-option v-for="(label, v) in kycStatusLabel" :key="v" :label="label" :value="v" />
        </el-select>
      </el-form-item>
      <el-form-item label="账号">
        <el-select v-model="filter.status" clearable placeholder="全部" data-testid="users-filter-status" style="width: 120px">
          <el-option v-for="(label, v) in userStatusLabel" :key="v" :label="label" :value="v" />
        </el-select>
      </el-form-item>
      <el-button type="primary" native-type="submit" data-testid="users-search">查询</el-button>
      <el-button data-testid="users-reset" @click="resetFilter">重置</el-button>
    </el-form>
    <el-table :data="list.items.value" v-loading="list.loading.value" row-key="id">
      <el-table-column label="ID" width="90"><template #default="{ row }"><IdLink kind="user" :id="row.id" data-testid="user-id" /></template></el-table-column>
      <el-table-column label="手机号" width="140"><template #default="{ row }"><span class="mono" data-testid="user-phone">{{ row.phone }}</span></template></el-table-column>
      <el-table-column label="类型" width="80"><template #default="{ row }"><span data-testid="user-kind">{{ labelOf(userKindLabel, row.kind) }}</span></template></el-table-column>
      <el-table-column label="实名" width="100"><template #default="{ row }"><span data-testid="user-kyc">{{ labelOf(kycStatusLabel, row.kyc_status) }}</span></template></el-table-column>
      <el-table-column label="账号" width="100"><template #default="{ row }"><el-tag :type="row.status === 'banned' ? 'danger' : 'success'" data-testid="user-status">{{ labelOf(userStatusLabel, row.status) }}</el-tag></template></el-table-column>
      <el-table-column label="注册时间" width="160"><template #default="{ row }"><span data-testid="user-created">{{ fmtTime(row.created_at) }}</span></template></el-table-column>
    </el-table>
    <div class="pager-row">
      <span class="muted" data-testid="users-total">共 {{ list.total.value }} 人</span>
      <el-pagination v-model:current-page="list.page.value" v-model:page-size="list.size.value" :total="list.total.value" layout="prev, pager, next" data-testid="users-pager" />
    </div>
  </el-card>
</template>

<style scoped>
.filters { display: flex; flex-wrap: wrap; gap: 8px 16px; align-items: center; margin-bottom: 12px; }
.pager-row { display: flex; justify-content: space-between; align-items: center; margin-top: 12px; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.muted { color: #909399; }
</style>
```

`web/admin/src/views/UserDetailView.vue`：

```vue
<script setup lang="ts">
import {
  durationLabel, fmtTime, fmtYuan, instanceStateLabel, isApiError, kycResultLabel, kycStatusLabel, labelOf, ledgerKindLabel, messageOf, orderKindLabel, orderStatusLabel,
  userKindLabel, userStatusLabel, yuanToCents, type UserDetail,
} from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, onMounted, reactive, ref } from 'vue'
import { useRoute } from 'vue-router'
import { api } from '../api'
import IdLink from '../components/IdLink.vue'
import { parseSignedYuan } from '../logic/money'
import { session } from '../store/session'

const route = useRoute()
const id = computed(() => String(route.params.id))
const data = ref<UserDetail | null>(null)
const tab = ref('instances')
const role = computed(() => session.admin?.role)
const canOps = computed(() => role.value === 'ops' || role.value === 'super')
const canFinance = computed(() => role.value === 'finance' || role.value === 'super')

async function load() {
  try {
    data.value = await api.get<UserDetail>(`/users/${id.value}`)
  } catch (e) {
    ElMessage.error(messageOf(e))
  }
}
onMounted(load)

function report(e: unknown) {
  if (!(isApiError(e) && e.code === 'cancelled')) ElMessage.error(messageOf(e))
}

// ---- 封禁 / 解封（ops）----
const ban = reactive({ visible: false, reason: '', error: '', busy: false })
const unban = reactive({ visible: false, busy: false })
async function submitBan() {
  ban.error = ban.reason.trim() ? '' : '请填写封禁原因'
  if (ban.error) return
  ban.busy = true
  try {
    await api.post(`/users/${id.value}/ban`, { reason: ban.reason.trim() })
    ElMessage.success('已封禁')
    ban.visible = false
    await load()
  } catch (e) {
    report(e)
  } finally {
    ban.busy = false
  }
}
async function submitUnban() {
  unban.busy = true
  try {
    await api.post(`/users/${id.value}/unban`, {})
    ElMessage.success('已解封')
    unban.visible = false
    await load()
  } catch (e) {
    report(e)
  } finally {
    unban.busy = false
  }
}

// ---- 调账（finance）：提交前确认「将把余额从 ¥a 调整为 ¥b」，动态码由请求层弹出 ----
const adjust = reactive({ visible: false, confirm: false, amount: '', remark: '', error: '', busy: false, cents: 0 })
const adjustAfter = computed(() => (data.value?.balance ?? 0) + adjust.cents)
function openAdjustConfirm() {
  const cents = parseSignedYuan(adjust.amount)
  adjust.error = cents == null ? '金额格式不正确（可为负数，不能为 0）' : adjust.remark.trim() ? '' : '请填写备注'
  if (adjust.error) return
  adjust.cents = cents!
  adjust.confirm = true
}
async function submitAdjust() {
  adjust.busy = true
  try {
    const out = await api.post<{ balance: number }>(`/users/${id.value}/adjust`, { amount: adjust.cents, remark: adjust.remark.trim() })
    ElMessage.success(`余额已调整为 ${fmtYuan(out.balance)}`)
    adjust.confirm = false
    adjust.visible = false
    await load()
  } catch (e) {
    adjust.confirm = false
    report(e) // duplicate → 「60 秒内已提交过相同的操作，请勿重复提交」
  } finally {
    adjust.busy = false
  }
}

// ---- 手动入账（finance）----
const mrecharge = reactive({ visible: false, amount: '', remark: '', error: '', busy: false })
async function submitManualRecharge() {
  const cents = yuanToCents(mrecharge.amount)
  mrecharge.error = cents == null || cents < 100 || cents > 100_000_000 ? '入账金额需在 1 元到 100 万元之间' : mrecharge.remark.trim() ? '' : '请填写备注（例如转账流水号）'
  if (mrecharge.error) return
  mrecharge.busy = true
  try {
    await api.post(`/users/${id.value}/manual-recharge`, { amount: cents, remark: mrecharge.remark.trim() })
    ElMessage.success(`已入账 ${fmtYuan(cents!)}`)
    mrecharge.visible = false
    await load()
  } catch (e) {
    report(e)
  } finally {
    mrecharge.busy = false
  }
}
const signed = (n: number) => (n >= 0 ? `+${fmtYuan(n)}` : fmtYuan(n))
</script>

<template>
  <div v-if="data" class="detail">
    <el-card>
      <el-descriptions :column="3" border>
        <el-descriptions-item label="用户 ID"><span data-testid="udetail-id">{{ data.user.id }}</span></el-descriptions-item>
        <el-descriptions-item label="手机号"><span class="mono" data-testid="udetail-phone">{{ data.user.phone }}</span></el-descriptions-item>
        <el-descriptions-item label="类型"><span data-testid="udetail-kind">{{ labelOf(userKindLabel, data.user.kind) }}</span></el-descriptions-item>
        <el-descriptions-item label="实名"><span data-testid="udetail-kyc">{{ labelOf(kycStatusLabel, data.user.kyc_status) }}</span></el-descriptions-item>
        <el-descriptions-item label="账号"><el-tag :type="data.user.status === 'banned' ? 'danger' : 'success'" data-testid="udetail-status">{{ labelOf(userStatusLabel, data.user.status) }}</el-tag></el-descriptions-item>
        <el-descriptions-item label="余额"><b data-testid="udetail-balance">{{ fmtYuan(data.balance) }}</b></el-descriptions-item>
      </el-descriptions>
      <div class="actions">
        <template v-if="canOps">
          <el-button v-if="data.user.status === 'active'" type="danger" plain data-testid="udetail-ban-btn" @click="ban.visible = true">封禁</el-button>
          <el-button v-else data-testid="udetail-unban-btn" @click="unban.visible = true">解封</el-button>
        </template>
        <template v-if="canFinance">
          <el-button data-testid="udetail-adjust-btn" @click="Object.assign(adjust, { visible: true, amount: '', remark: '', error: '' })">调账</el-button>
          <el-button data-testid="udetail-recharge-btn" @click="Object.assign(mrecharge, { visible: true, amount: '', remark: '', error: '' })">手动入账</el-button>
        </template>
      </div>
    </el-card>

    <el-card>
      <el-tabs v-model="tab">
        <el-tab-pane label="实例" name="instances">
          <el-table :data="data.instances" row-key="id" size="small">
            <el-table-column label="实例"><template #default="{ row }"><span data-testid="udetail-instance-row"><IdLink kind="instance" :id="row.id" /></span></template></el-table-column>
            <el-table-column label="状态" width="120"><template #default="{ row }">{{ labelOf(instanceStateLabel, row.state) }}</template></el-table-column>
            <el-table-column label="创建时间" width="160"><template #default="{ row }">{{ fmtTime(row.created_at) }}</template></el-table-column>
          </el-table>
          <router-link :to="{ path: '/instances', query: { user_id: data.user.id } }" data-testid="udetail-more-instances">更多实例</router-link>
        </el-tab-pane>
        <el-tab-pane label="订单" name="orders">
          <el-table :data="data.orders" row-key="id" size="small">
            <el-table-column label="订单号"><template #default="{ row }"><span class="mono" data-testid="udetail-order-row">{{ row.id }}</span></template></el-table-column>
            <el-table-column label="类型" width="80"><template #default="{ row }">{{ labelOf(orderKindLabel, row.kind) }}</template></el-table-column>
            <el-table-column label="租期" width="80"><template #default="{ row }">{{ labelOf(durationLabel, row.duration) }}</template></el-table-column>
            <el-table-column label="金额" width="110"><template #default="{ row }">{{ fmtYuan(row.amount) }}</template></el-table-column>
            <el-table-column label="状态" width="100"><template #default="{ row }">{{ labelOf(orderStatusLabel, row.status) }}</template></el-table-column>
          </el-table>
          <router-link v-if="canFinance" :to="{ path: '/orders', query: { user_id: data.user.id } }" data-testid="udetail-more-orders">更多订单</router-link>
        </el-tab-pane>
        <el-tab-pane label="流水" name="ledger">
          <el-table :data="data.ledger" row-key="id" size="small">
            <el-table-column label="类型" width="90"><template #default="{ row }"><span data-testid="udetail-ledger-kind">{{ labelOf(ledgerKindLabel, row.kind) }}</span></template></el-table-column>
            <el-table-column label="金额" width="120"><template #default="{ row }"><span data-testid="udetail-ledger-amount" :class="row.amount >= 0 ? 'money-plus' : 'money-minus'">{{ signed(row.amount) }}</span></template></el-table-column>
            <el-table-column label="变动后余额" width="130"><template #default="{ row }"><span data-testid="udetail-ledger-after">{{ fmtYuan(row.balance_after) }}</span></template></el-table-column>
            <el-table-column label="备注"><template #default="{ row }"><span data-testid="udetail-ledger-remark">{{ row.remark }}</span></template></el-table-column>
            <el-table-column label="时间" width="160"><template #default="{ row }">{{ fmtTime(row.created_at) }}</template></el-table-column>
          </el-table>
          <template v-if="canFinance">
            <router-link :to="{ path: '/ledger', query: { user_id: data.user.id } }" data-testid="udetail-more-ledger">更多流水</router-link> ·
            <router-link :to="{ path: '/recharges', query: { user_id: data.user.id } }" data-testid="udetail-more-recharges">充值单</router-link>
          </template>
        </el-tab-pane>
        <el-tab-pane label="实名记录" name="kyc">
          <el-table :data="data.kyc" row-key="id" size="small">
            <el-table-column label="类型" width="80"><template #default="{ row }"><span data-testid="udetail-kyc-kind">{{ row.kind === 'personal' ? '个人' : '企业' }}</span></template></el-table-column>
            <el-table-column label="姓名 / 企业"><template #default="{ row }"><span data-testid="udetail-kyc-name">{{ row.real_name }}</span></template></el-table-column>
            <el-table-column label="身份证 / 信用代码" width="200"><template #default="{ row }"><span class="mono" data-testid="udetail-kyc-idno">{{ row.id_number_masked || row.license_no }}</span></template></el-table-column>
            <el-table-column label="结果" width="90"><template #default="{ row }"><span data-testid="udetail-kyc-result">{{ labelOf(kycResultLabel, row.result) }}</span></template></el-table-column>
            <el-table-column label="原因"><template #default="{ row }"><span data-testid="udetail-kyc-reason">{{ row.reason }}</span></template></el-table-column>
            <el-table-column label="时间" width="160"><template #default="{ row }">{{ fmtTime(row.created_at) }}</template></el-table-column>
          </el-table>
        </el-tab-pane>
      </el-tabs>
    </el-card>

    <el-dialog v-model="ban.visible" title="封禁用户" width="420" align-center destroy-on-close>
      <el-form label-position="top" @submit.prevent="submitBan">
        <el-form-item label="原因（必填）"><el-input v-model="ban.reason" data-testid="ban-reason" /></el-form-item>
        <p v-if="ban.error" class="error" data-testid="ban-error">{{ ban.error }}</p>
        <el-button type="danger" native-type="submit" :loading="ban.busy" data-testid="ban-submit">确定封禁</el-button>
      </el-form>
    </el-dialog>
    <el-dialog v-model="unban.visible" title="解封用户" width="420" align-center>
      <p>解封后用户可重新登录。</p>
      <template #footer><el-button @click="unban.visible = false">取消</el-button><el-button type="primary" :loading="unban.busy" data-testid="unban-ok" @click="submitUnban">确定</el-button></template>
    </el-dialog>

    <el-dialog v-model="adjust.visible" title="调账" width="460" align-center destroy-on-close>
      <el-form label-position="top" @submit.prevent="openAdjustConfirm">
        <el-form-item label="金额（元，可为负数）"><el-input v-model="adjust.amount" inputmode="decimal" data-testid="adjust-amount" /></el-form-item>
        <el-form-item label="备注（必填）"><el-input v-model="adjust.remark" data-testid="adjust-remark" /></el-form-item>
        <p v-if="adjust.error" class="error" data-testid="adjust-error">{{ adjust.error }}</p>
        <el-button type="primary" native-type="submit" data-testid="adjust-submit">下一步</el-button>
      </el-form>
    </el-dialog>
    <el-dialog v-model="adjust.confirm" title="确认调账" width="420" align-center>
      <p data-testid="adjust-confirm-text">将把余额从 {{ fmtYuan(data.balance) }} 调整为 {{ fmtYuan(adjustAfter) }}</p>
      <template #footer><el-button @click="adjust.confirm = false">取消</el-button><el-button type="primary" :loading="adjust.busy" data-testid="adjust-confirm-ok" @click="submitAdjust">确定</el-button></template>
    </el-dialog>

    <el-dialog v-model="mrecharge.visible" title="手动入账" width="460" align-center destroy-on-close>
      <el-form label-position="top" @submit.prevent="submitManualRecharge">
        <el-form-item label="金额（元，1～1,000,000）"><el-input v-model="mrecharge.amount" inputmode="decimal" data-testid="mrecharge-amount" /></el-form-item>
        <el-form-item label="备注（必填，例如转账流水号）"><el-input v-model="mrecharge.remark" data-testid="mrecharge-remark" /></el-form-item>
        <p v-if="mrecharge.error" class="error" data-testid="mrecharge-error">{{ mrecharge.error }}</p>
        <el-button type="primary" native-type="submit" :loading="mrecharge.busy" data-testid="mrecharge-submit">入账</el-button>
      </el-form>
    </el-dialog>
  </div>
</template>

<style scoped>
.detail { display: grid; gap: 16px; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.actions { display: flex; gap: 12px; margin-top: 16px; }
.error { color: var(--el-color-danger); font-size: 13px; }
</style>
```

`web/admin/src/views/KycView.vue`：

```vue
<script setup lang="ts">
import { fmtTime, isApiError, messageOf, type AdminKYCRecord, type Paged } from '@spark/shared'
import { ElMessage } from 'element-plus'
import { onMounted, reactive } from 'vue'
import { api } from '../api'
import IdLink from '../components/IdLink.vue'
import { usePaged } from '../composables/usePaged'

const list = usePaged<AdminKYCRecord>((page, size) => api.get<Paged<AdminKYCRecord>>('/kyc/pending', { page, page_size: size }))
onMounted(list.load)

const review = reactive({ visible: false, record: null as AdminKYCRecord | null, imgFailed: false, approveConfirm: false, rejectMode: false, reason: '', error: '', busy: false })
const fileUrl = (key: string) => `/admin/api/v1/files/${key}`
function open(r: AdminKYCRecord) {
  Object.assign(review, { visible: true, record: r, imgFailed: false, approveConfirm: false, rejectMode: false, reason: '', error: '' })
}
function openInNewWindow() {
  if (review.record?.license_file) window.open(fileUrl(review.record.license_file), '_blank', 'noopener')
}
async function submit(approve: boolean) {
  if (!review.record) return
  if (!approve && !review.reason.trim()) {
    review.error = '请填写驳回原因'
    return
  }
  review.busy = true
  try {
    await api.post(`/kyc/${review.record.id}/review`, { approve, reason: approve ? '' : review.reason.trim() })
    ElMessage.success(approve ? '已通过' : '已驳回')
    review.visible = false
    await list.load()
  } catch (e) {
    if (!(isApiError(e) && e.code === 'cancelled')) ElMessage.error(messageOf(e))
  } finally {
    review.busy = false
  }
}
</script>

<template>
  <el-card>
    <el-table :data="list.items.value" v-loading="list.loading.value" row-key="id">
      <el-table-column label="企业名称" min-width="200"><template #default="{ row }"><span data-testid="kyc-company">{{ row.real_name }}</span></template></el-table-column>
      <el-table-column label="统一社会信用代码" width="200"><template #default="{ row }"><span class="mono" data-testid="kyc-license-no">{{ row.license_no }}</span></template></el-table-column>
      <el-table-column label="用户" width="100"><template #default="{ row }"><IdLink kind="user" :id="row.user_id" data-testid="kyc-user" /></template></el-table-column>
      <el-table-column label="提交时间" width="160"><template #default="{ row }"><span data-testid="kyc-submitted">{{ fmtTime(row.created_at) }}</span></template></el-table-column>
      <el-table-column label="操作" width="100"><template #default="{ row }"><el-button size="small" type="primary" data-testid="kyc-review-btn" @click="open(row)">审核</el-button></template></el-table-column>
    </el-table>
    <el-pagination v-model:current-page="list.page.value" v-model:page-size="list.size.value" :total="list.total.value" layout="total, prev, pager, next" class="pager" />

    <el-dialog v-model="review.visible" title="企业认证审核" width="860" align-center destroy-on-close data-testid="kyc-review-dialog">
      <div v-if="review.record" class="review">
        <el-descriptions :column="1" border class="info">
          <el-descriptions-item label="企业名称">{{ review.record.real_name }}</el-descriptions-item>
          <el-descriptions-item label="信用代码"><span class="mono">{{ review.record.license_no }}</span></el-descriptions-item>
          <el-descriptions-item label="用户"><IdLink kind="user" :id="review.record.user_id" /></el-descriptions-item>
          <el-descriptions-item label="提交时间">{{ fmtTime(review.record.created_at) }}</el-descriptions-item>
        </el-descriptions>
        <div class="preview">
          <template v-if="review.record.license_file?.endsWith('.pdf')">
            <el-button data-testid="kyc-open-pdf" @click="openInNewWindow">在新窗口打开 PDF</el-button>
          </template>
          <template v-else-if="!review.imgFailed">
            <img :src="fileUrl(review.record.license_file ?? '')" alt="营业执照" class="license" data-testid="kyc-preview-img" @error="review.imgFailed = true" />
          </template>
          <el-button v-else data-testid="kyc-open-file" @click="openInNewWindow">在新窗口打开</el-button>
        </div>
      </div>
      <div v-if="review.rejectMode" class="reject">
        <el-input v-model="review.reason" placeholder="驳回原因（必填，会展示给用户）" data-testid="kyc-reject-reason" />
        <p v-if="review.error" class="error" data-testid="kyc-reject-error">{{ review.error }}</p>
      </div>
      <template #footer>
        <template v-if="review.approveConfirm">
          <span>确认通过该企业认证？</span>
          <el-button @click="review.approveConfirm = false">取消</el-button>
          <el-button type="primary" :loading="review.busy" data-testid="kyc-approve-ok" @click="submit(true)">确认通过</el-button>
        </template>
        <template v-else-if="review.rejectMode">
          <el-button @click="review.rejectMode = false">取消</el-button>
          <el-button type="danger" :loading="review.busy" data-testid="kyc-reject-submit" @click="submit(false)">确认驳回</el-button>
        </template>
        <template v-else>
          <el-button type="danger" plain data-testid="kyc-reject" @click="review.rejectMode = true">驳回</el-button>
          <el-button type="primary" data-testid="kyc-approve" @click="review.approveConfirm = true">通过</el-button>
        </template>
      </template>
    </el-dialog>
  </el-card>
</template>

<style scoped>
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.pager { margin-top: 12px; justify-content: flex-end; }
.review { display: grid; grid-template-columns: 1fr 1fr; gap: 16px; }
.preview { display: flex; align-items: center; justify-content: center; min-height: 240px; background: #f5f7fa; border-radius: 8px; }
.license { max-width: 100%; max-height: 420px; }
.reject { margin-top: 12px; }
.error { color: var(--el-color-danger); font-size: 13px; }
</style>
```

Run: `cd web && npm test && npm run build`
Expected: Vitest PASS（money 8）；构建通过。

- [ ] **Step 3: 端到端用例**

`web/e2e/helpers/console.ts` 末尾追加：

```ts
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

/** 用户上传营业执照（multipart 字段 file），返回文件 key。 */
export async function uploadLicense(api: APIRequestContext, name: 'license.png' | 'license.pdf'): Promise<string> {
  const buffer = readFileSync(fileURLToPath(new URL(`../fixtures/${name}`, import.meta.url)))
  const r = await api.post('/api/v1/uploads', { multipart: { file: { name, mimeType: name.endsWith('.pdf') ? 'application/pdf' : 'image/png', buffer } } })
  if (r.status() !== 201) throw new Error(`upload ${r.status()} ${await r.text()}`)
  return ((await r.json()) as { key: string }).key
}

export async function submitEnterpriseKyc(api: APIRequestContext, opts: { company: string; licenseNo: string; file: 'license.png' | 'license.pdf' }) {
  const key = await uploadLicense(api, opts.file)
  const r = await api.post('/api/v1/kyc/enterprise', { data: { company_name: opts.company, license_no: opts.licenseNo, license_file: key } })
  if (!r.ok()) throw new Error(`kyc/enterprise ${r.status()} ${await r.text()}`)
  return key
}
```

（把这两个 `import` 挪到文件顶部的 import 区。）

`web/e2e/admin/users.spec.ts`：

```ts
import { expect, test } from '@playwright/test'
import { adminApi, createAdmin, enterTotpDialog, loginAdmin } from '../helpers/admin'
import { seedUser, toast, userApi } from '../helpers/console'
import { runningInstance } from '../helpers/instances'

const userRows = (page: import('@playwright/test').Page) => page.getByRole('row').filter({ has: page.getByTestId('user-id') })

test('A-USR-01 用户搜索：手机号、尾号、ID 都命中；字母被拦截', async ({ page }) => {
  const u = await seedUser()
  const admin = await createAdmin('ops')
  await loginAdmin(page, admin)
  await page.getByTestId('menu-users').click()
  await page.getByTestId('users-q').fill(u.phone)
  await page.getByTestId('users-search').click()
  await expect(userRows(page)).toHaveCount(1)
  await expect(userRows(page).first().getByTestId('user-phone')).toHaveText(u.phone)
  await page.getByTestId('users-q').fill(u.phone.slice(-4))
  await page.getByTestId('users-search').click()
  await expect(userRows(page).filter({ has: page.getByTestId('user-phone').filter({ hasText: u.phone }) })).toHaveCount(1)
  await page.getByTestId('users-q').fill(String(u.id))
  await page.getByTestId('users-search').click()
  await expect(userRows(page).first().getByTestId('user-id')).toHaveText(String(u.id))
  await page.getByTestId('users-q').fill('abc')
  await expect(page.getByTestId('users-q')).toHaveValue('')
  await page.getByTestId('users-q').fill('12ab34')
  await expect(page.getByTestId('users-q')).toHaveValue('1234')
})

test('A-USR-02 用户详情：余额、实例、订单、流水、实名记录脱敏', async ({ page }) => {
  const u = await seedUser({ verified: false, balance: 100000 })
  const api = await userApi(u.phone)
  await api.post('/api/v1/kyc/personal', { data: { name: '张三', id_number: '110101199001011234' } })
  const { id } = await runningInstance(u.phone)
  const admin = await createAdmin('finance')
  await loginAdmin(page, admin)
  await page.goto(`/admin/users/${u.id}`)
  await expect(page.getByTestId('udetail-phone')).toHaveText(u.phone)
  await expect(page.getByTestId('udetail-kyc')).toHaveText('已认证')
  await expect(page.getByTestId('udetail-balance')).toHaveText('¥931.00')
  await expect(page.getByTestId('udetail-instance-row').filter({ hasText: id })).toHaveCount(1)
  await page.getByRole('tab', { name: '订单' }).click()
  await expect(page.getByTestId('udetail-order-row')).toHaveCount(1)
  await page.getByRole('tab', { name: '流水' }).click()
  await expect(page.getByTestId('udetail-ledger-kind').first()).toHaveText('扣款')
  await expect(page.getByTestId('udetail-ledger-amount').first()).toHaveText('-¥69.00')
  await expect(page.getByTestId('udetail-ledger-after').first()).toHaveText('¥931.00')
  await expect(page.getByTestId('udetail-more-ledger')).toBeVisible()
  await page.getByRole('tab', { name: '实名记录' }).click()
  await expect(page.getByTestId('udetail-kyc-kind')).toHaveText(['个人'])
  await expect(page.getByTestId('udetail-kyc-idno')).toHaveText(['110***********1234'])
  await expect(page.getByTestId('udetail-kyc-result')).toHaveText(['已通过'])
})

test('A-USR-03 封禁需原因；封禁后状态已封禁；解封恢复', async ({ page }) => {
  const u = await seedUser()
  const admin = await createAdmin('ops')
  await loginAdmin(page, admin)
  await page.goto(`/admin/users/${u.id}`)
  await page.getByTestId('udetail-ban-btn').click()
  await page.getByTestId('ban-submit').click()
  await expect(page.getByTestId('ban-error')).toHaveText('请填写封禁原因')
  await page.getByTestId('ban-reason').fill('违规使用')
  await page.getByTestId('ban-submit').click()
  await toast(page, '已封禁')
  await expect(page.getByTestId('udetail-status')).toHaveText('已封禁')
  await page.getByTestId('udetail-unban-btn').click()
  await page.getByTestId('unban-ok').click()
  await toast(page, '已解封')
  await expect(page.getByTestId('udetail-status')).toHaveText('正常')
})

test('A-USR-04 调账 +¥10 与 −¥5（动态码）→ 余额与流水', async ({ page }) => {
  const u = await seedUser({ balance: 100000 })
  const admin = await createAdmin('finance')
  await loginAdmin(page, admin)
  await page.goto(`/admin/users/${u.id}`)
  await page.getByTestId('udetail-adjust-btn').click()
  await page.getByTestId('adjust-amount').fill('10')
  await page.getByTestId('adjust-remark').fill('活动补偿')
  await page.getByTestId('adjust-submit').click()
  await expect(page.getByTestId('adjust-confirm-text')).toHaveText('将把余额从 ¥1,000.00 调整为 ¥1,010.00')
  await page.getByTestId('adjust-confirm-ok').click()
  await enterTotpDialog(page, admin.totp_secret!)
  await toast(page, '余额已调整为 ¥1,010.00')
  await expect(page.getByTestId('udetail-balance')).toHaveText('¥1,010.00')
  await page.getByTestId('udetail-adjust-btn').click()
  await page.getByTestId('adjust-amount').fill('-5')
  await page.getByTestId('adjust-remark').fill('纠错')
  await page.getByTestId('adjust-submit').click()
  await expect(page.getByTestId('adjust-confirm-text')).toHaveText('将把余额从 ¥1,010.00 调整为 ¥1,005.00')
  await page.getByTestId('adjust-confirm-ok').click()
  await expect(page.getByTestId('totp-dialog')).toBeHidden() // 5 分钟窗口内不再要求
  await toast(page, '余额已调整为 ¥1,005.00')
  await page.getByRole('tab', { name: '流水' }).click()
  await expect(page.getByTestId('udetail-ledger-amount').nth(0)).toHaveText('-¥5.00')
  await expect(page.getByTestId('udetail-ledger-amount').nth(1)).toHaveText('+¥10.00')
  await expect(page.getByTestId('udetail-ledger-kind').nth(0)).toHaveText('调账')
})

test('A-USR-05 手动入账 ¥100：充值单渠道「手动」、余额增加', async ({ page }) => {
  const u = await seedUser({ balance: 100000 })
  const admin = await createAdmin('finance')
  await loginAdmin(page, admin)
  await page.goto(`/admin/users/${u.id}`)
  await page.getByTestId('udetail-recharge-btn').click()
  await page.getByTestId('mrecharge-amount').fill('100')
  await page.getByTestId('mrecharge-remark').fill('对公转账 20261017-001')
  await page.getByTestId('mrecharge-submit').click()
  await enterTotpDialog(page, admin.totp_secret!)
  await toast(page, '已入账 ¥100.00')
  await expect(page.getByTestId('udetail-balance')).toHaveText('¥1,100.00')
  const ctx = await adminApi(admin)
  const recharges = ((await (await ctx.get(`/admin/api/v1/recharges?user_id=${u.id}`)).json()) as { items: { channel: string; status: string; amount: number }[] }).items
  expect(recharges.some((r) => r.channel === 'manual' && r.status === 'paid' && r.amount === 10000)).toBe(true)
  await page.getByRole('tab', { name: '流水' }).click()
  await expect(page.getByTestId('udetail-ledger-kind').first()).toHaveText('充值')
  await expect(page.getByTestId('udetail-ledger-amount').first()).toHaveText('+¥100.00')
})

test('A-USR-06 60 秒内相同调账被拦截', async ({ page }) => {
  const u = await seedUser({ balance: 100000 })
  const admin = await createAdmin('finance')
  await loginAdmin(page, admin)
  await page.goto(`/admin/users/${u.id}`)
  const adjustOnce = async () => {
    await page.getByTestId('udetail-adjust-btn').click()
    await page.getByTestId('adjust-amount').fill('10')
    await page.getByTestId('adjust-remark').fill('重复提交测试')
    await page.getByTestId('adjust-submit').click()
    await page.getByTestId('adjust-confirm-ok').click()
  }
  await adjustOnce()
  await enterTotpDialog(page, admin.totp_secret!)
  await toast(page, '余额已调整为 ¥1,010.00')
  await adjustOnce()
  await toast(page, '60 秒内已提交过相同的操作')
  await expect(page.getByTestId('udetail-balance')).toHaveText('¥1,010.00')
})
```

`web/e2e/admin/kyc.spec.ts`：

```ts
import { expect, test } from '@playwright/test'
import { adminApi, createAdmin, loginAdmin } from '../helpers/admin'
import { seedUser, submitEnterpriseKyc, toast, userApi } from '../helpers/console'
import { runStamp } from '../helpers/phone'

const LICENSE_NO = '91110000MA01ABCD2X'

test('A-KYC-01 待审核列表、PNG 预览与 PDF 新窗口', async ({ page, context }) => {
  const png = await seedUser({ verified: false })
  await submitEnterpriseKyc(await userApi(png.phone), { company: `图片企业-${runStamp}`, licenseNo: LICENSE_NO, file: 'license.png' })
  const pdf = await seedUser({ verified: false })
  await submitEnterpriseKyc(await userApi(pdf.phone), { company: `文档企业-${runStamp}`, licenseNo: LICENSE_NO, file: 'license.pdf' })
  const admin = await createAdmin('ops')
  await loginAdmin(page, admin)
  await page.getByTestId('menu-kyc').click()
  const pngRow = page.getByRole('row').filter({ has: page.getByTestId('kyc-company').filter({ hasText: `图片企业-${runStamp}` }) })
  await expect(pngRow.getByTestId('kyc-license-no')).toHaveText(LICENSE_NO)
  await expect(pngRow.getByTestId('kyc-user')).toHaveText(String(png.id))
  await pngRow.getByTestId('kyc-review-btn').click()
  const img = page.getByTestId('kyc-preview-img')
  await expect(img).toBeVisible()
  await expect.poll(() => img.evaluate((el) => (el as HTMLImageElement).complete && (el as HTMLImageElement).naturalWidth)).toBe(1)
  await page.keyboard.press('Escape')
  const pdfRow = page.getByRole('row').filter({ has: page.getByTestId('kyc-company').filter({ hasText: `文档企业-${runStamp}` }) })
  await pdfRow.getByTestId('kyc-review-btn').click()
  const popup = context.waitForEvent('page')
  await page.getByTestId('kyc-open-pdf').click()
  const opened = await popup
  expect(opened.url()).toMatch(/\/admin\/api\/v1\/files\/.+\.pdf$/)
  await opened.close()
})

test('A-KYC-02 通过与驳回', async ({ page }) => {
  const ok = await seedUser({ verified: false })
  await submitEnterpriseKyc(await userApi(ok.phone), { company: `通过企业-${runStamp}`, licenseNo: LICENSE_NO, file: 'license.png' })
  const bad = await seedUser({ verified: false })
  await submitEnterpriseKyc(await userApi(bad.phone), { company: `驳回企业-${runStamp}`, licenseNo: LICENSE_NO, file: 'license.png' })
  const admin = await createAdmin('ops')
  const ctx = await adminApi(admin)
  let reviews = 0
  page.on('request', (r) => {
    if (r.method() === 'POST' && r.url().includes('/kyc/') && r.url().endsWith('/review')) reviews += 1
  })
  await loginAdmin(page, admin)
  await page.getByTestId('menu-kyc').click()
  const row = (name: string) => page.getByRole('row').filter({ has: page.getByTestId('kyc-company').filter({ hasText: name }) })
  await row(`通过企业-${runStamp}`).getByTestId('kyc-review-btn').click()
  await page.getByTestId('kyc-approve').click()
  await page.getByTestId('kyc-approve-ok').click()
  await toast(page, '已通过')
  await expect(row(`通过企业-${runStamp}`)).toHaveCount(0)
  expect(((await (await ctx.get(`/admin/api/v1/users/${ok.id}`)).json()) as { user: { kyc_status: string } }).user.kyc_status).toBe('verified')

  await row(`驳回企业-${runStamp}`).getByTestId('kyc-review-btn').click()
  await page.getByTestId('kyc-reject').click()
  await page.getByTestId('kyc-reject-submit').click()
  await expect(page.getByTestId('kyc-reject-error')).toHaveText('请填写驳回原因')
  expect(reviews).toBe(1)
  await page.getByTestId('kyc-reject-reason').fill('执照不清晰')
  await page.getByTestId('kyc-reject-submit').click()
  await toast(page, '已驳回')
  await expect(row(`驳回企业-${runStamp}`)).toHaveCount(0)
  const detail = (await (await ctx.get(`/admin/api/v1/users/${bad.id}`)).json()) as { user: { kyc_status: string }; kyc: { result: string; reason: string }[] }
  expect(detail.user.kyc_status).toBe('rejected')
  expect(detail.kyc[0]).toMatchObject({ result: 'rejected', reason: '执照不清晰' })
})
```

Run: `make web && cd web && npx playwright test --project=admin admin/users.spec.ts admin/kyc.spec.ts`
Expected: 8 passed。

- [ ] **Step 4: 全量验证并提交**

Run: `go vet ./... && go test ./... && cd web && npm test && npm run build && npx playwright test --project=admin`
Expected: 全部 PASS。

```bash
git add web/admin web/e2e
git commit -m "feat(admin): users list/detail with ban, adjust and manual recharge (TOTP); enterprise KYC review with file preview; e2e A-USR, A-KYC

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---
### Task 20: 后台订单、流水、充值单与 CSV 导出

**Files:**
- Create: `web/admin/src/components/FinancePage.vue`
- Replace: `web/admin/src/views/{OrdersView,LedgerView,RechargesView}.vue`
- Create: `web/e2e/admin/finance.spec.ts`

**Interfaces:**
- Consumes: Task 16 的 `usePaged`、`DateRange`、`IdLink`；Task 6 的 `http.download`；shared `labels`、`fmtYuan`、`fmtTime`。
- Produces:
  - `FinancePage.vue`：props `endpoint: 'orders' | 'ledger' | 'recharges'`、`kindParam: 'kind' | 'channel'`、`kindOptions: { label; value }[]`、`statusOptions: { label; value }[]`；内部维护筛选 `{ user_id, kind, status, range: { from, to } }`（`?user_id=` 预填）、分页（`usePaged`）、「导出 CSV」（`api.download('/<endpoint>/export', 同筛选参数)` → `URL.createObjectURL` 触发下载，错误 `ElMessage.error(messageOf)`）；默认插槽 `#table="{ items }"`。
  - 共用 testid：`fin-filter-user`、`fin-filter-kind`、`fin-filter-status`、`fin-from` / `fin-to`、`fin-search`、`fin-reset`、`fin-export`、`fin-total`、`fin-pager`。
  - 订单行：`ord-id`、`ord-user`、`ord-kind`、`ord-instance`、`ord-duration`、`ord-amount`、`ord-refunded`、`ord-status`、`ord-time`；流水行：`led-id`、`led-user`、`led-kind`、`led-amount`、`led-after`、`led-ref`、`led-operator`、`led-remark`、`led-time`；充值单行：`rch-id`、`rch-user`、`rch-channel`、`rch-amount`、`rch-status`、`rch-trade-no`、`rch-operator`、`rch-remark`、`rch-created`、`rch-paid`。
  - 覆盖：A-FIN-01～04。

- [ ] **Step 1: 共用组件与三个页面**

`web/admin/src/components/FinancePage.vue`：

```vue
<script setup lang="ts">
import { messageOf, type Paged } from '@spark/shared'
import { ElMessage } from 'element-plus'
import { onMounted, reactive, ref } from 'vue'
import { useRoute } from 'vue-router'
import { api } from '../api'
import { usePaged } from '../composables/usePaged'
import DateRange from './DateRange.vue'

const props = defineProps<{
  endpoint: 'orders' | 'ledger' | 'recharges'
  kindParam: 'kind' | 'channel'
  kindLabel: string
  kindOptions: { label: string; value: string }[]
  statusOptions: { label: string; value: string }[]
}>()

const route = useRoute()
const filter = reactive({ user_id: String(route.query.user_id ?? ''), kind: '', status: '', range: { from: '', to: '' } })
const query = () => ({ user_id: filter.user_id.trim(), [props.kindParam]: filter.kind, status: filter.status, from: filter.range.from, to: filter.range.to })
const list = usePaged<Record<string, unknown>>((page, size) => api.get<Paged<Record<string, unknown>>>(`/${props.endpoint}`, { ...query(), page, page_size: size }))
onMounted(list.load)
function resetFilter() {
  Object.assign(filter, { user_id: '', kind: '', status: '', range: { from: '', to: '' } })
  list.reset()
}

const exporting = ref(false)
async function exportCSV() {
  exporting.value = true
  try {
    const { blob, filename } = await api.download(`/${props.endpoint}/export`, query())
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = filename
    a.click()
    URL.revokeObjectURL(url)
  } catch (e) {
    ElMessage.error(messageOf(e)) // export_too_large 等原文
  } finally {
    exporting.value = false
  }
}
</script>

<template>
  <el-card>
    <el-form inline class="filters" @submit.prevent="list.reset()">
      <el-form-item label="用户 ID"><el-input :model-value="filter.user_id" clearable data-testid="fin-filter-user" style="width: 120px" @update:model-value="(v: string) => (filter.user_id = v.replace(/\D/g, ''))" /></el-form-item>
      <el-form-item :label="kindLabel">
        <el-select v-model="filter.kind" clearable placeholder="全部" data-testid="fin-filter-kind" style="width: 130px">
          <el-option v-for="o in kindOptions" :key="o.value" :label="o.label" :value="o.value" />
        </el-select>
      </el-form-item>
      <el-form-item v-if="statusOptions.length" label="状态">
        <el-select v-model="filter.status" clearable placeholder="全部" data-testid="fin-filter-status" style="width: 130px">
          <el-option v-for="o in statusOptions" :key="o.value" :label="o.label" :value="o.value" />
        </el-select>
      </el-form-item>
      <el-form-item label="日期"><DateRange v-model="filter.range" prefix="fin" /></el-form-item>
      <el-button type="primary" native-type="submit" data-testid="fin-search">查询</el-button>
      <el-button data-testid="fin-reset" @click="resetFilter">重置</el-button>
      <span class="spacer" />
      <el-button :loading="exporting" data-testid="fin-export" @click="exportCSV">导出 CSV</el-button>
    </el-form>
    <div v-loading="list.loading.value">
      <slot name="table" :items="list.items.value" />
    </div>
    <div class="pager-row">
      <span class="muted" data-testid="fin-total">共 {{ list.total.value }} 条</span>
      <el-pagination v-model:current-page="list.page.value" v-model:page-size="list.size.value" :total="list.total.value" layout="prev, pager, next" data-testid="fin-pager" />
    </div>
  </el-card>
</template>

<style scoped>
.filters { display: flex; flex-wrap: wrap; gap: 8px 16px; align-items: center; margin-bottom: 12px; }
.spacer { flex: 1; }
.pager-row { display: flex; justify-content: space-between; align-items: center; margin-top: 12px; }
.muted { color: #909399; }
</style>
```

`web/admin/src/views/OrdersView.vue`：

```vue
<script setup lang="ts">
import { durationLabel, fmtTime, fmtYuan, labelOf, orderKindLabel, orderStatusLabel, type AdminOrder } from '@spark/shared'
import FinancePage from '../components/FinancePage.vue'
import IdLink from '../components/IdLink.vue'

const kinds = Object.entries(orderKindLabel).map(([value, label]) => ({ value, label }))
const statuses = Object.entries(orderStatusLabel).map(([value, label]) => ({ value, label }))
</script>

<template>
  <FinancePage endpoint="orders" kind-param="kind" kind-label="类型" :kind-options="kinds" :status-options="statuses">
    <template #table="{ items }">
      <el-table :data="items as AdminOrder[]" row-key="id">
        <el-table-column label="订单号" min-width="160"><template #default="{ row }"><span class="mono" data-testid="ord-id">{{ row.id }}</span></template></el-table-column>
        <el-table-column label="用户" width="90"><template #default="{ row }"><IdLink kind="user" :id="row.user_id" data-testid="ord-user" /></template></el-table-column>
        <el-table-column label="类型" width="80"><template #default="{ row }"><span data-testid="ord-kind">{{ labelOf(orderKindLabel, row.kind) }}</span></template></el-table-column>
        <el-table-column label="实例" min-width="160"><template #default="{ row }"><IdLink kind="instance" :id="row.instance_id" data-testid="ord-instance" /></template></el-table-column>
        <el-table-column label="租期" width="80"><template #default="{ row }"><span data-testid="ord-duration">{{ labelOf(durationLabel, row.duration) }}</span></template></el-table-column>
        <el-table-column label="金额" width="110"><template #default="{ row }"><span data-testid="ord-amount">{{ fmtYuan(row.amount) }}</span></template></el-table-column>
        <el-table-column label="已退" width="110"><template #default="{ row }"><span data-testid="ord-refunded">{{ fmtYuan(row.refunded_amount) }}</span></template></el-table-column>
        <el-table-column label="状态" width="100"><template #default="{ row }"><span data-testid="ord-status">{{ labelOf(orderStatusLabel, row.status) }}</span></template></el-table-column>
        <el-table-column label="下单时间" width="160"><template #default="{ row }"><span data-testid="ord-time">{{ fmtTime(row.created_at) }}</span></template></el-table-column>
      </el-table>
    </template>
  </FinancePage>
</template>

<style scoped>
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
</style>
```

`web/admin/src/views/LedgerView.vue`（`kindParam="kind"`，选项 `ledgerKindLabel`，无状态筛选 `:status-options="[]"`；列：`led-id`、`led-user`、`led-kind`、`led-amount`（正负着色 `money-plus / money-minus`，`+¥100.00`）、`led-after`、`led-ref`（`ref_type ref_id`）、`led-operator`（`operator_id ?? '—'`）、`led-remark`、`led-time`）。

`web/admin/src/views/RechargesView.vue`（`kindParam="channel"`，`kind-label="渠道"`，选项 `rechargeChannelLabel`，状态 `rechargeStatusLabel`；列：`rch-id`、`rch-user`、`rch-channel`、`rch-amount`、`rch-status`、`rch-trade-no`、`rch-operator`、`rch-remark`、`rch-created`、`rch-paid`（`fmtTime(paid_at) || '—'`））。两个文件的结构与 `OrdersView.vue` 完全相同，只换列。

Run: `cd web && npm run build`
Expected: 通过。

- [ ] **Step 2: 端到端用例**

`web/e2e/admin/finance.spec.ts`：

```ts
import { expect, test, type Page } from '@playwright/test'
import { createAdmin, loginAdmin } from '../helpers/admin'
import { placeOrder, seedUser, userApi } from '../helpers/console'
import { hooks } from '../helpers/hooks'
import { cstDate } from '../helpers/time'

const rows = (page: Page, testid: string) => page.getByRole('row').filter({ has: page.getByTestId(testid) })

async function seedFinanceUser() {
  const u = await seedUser({ balance: 200000 })
  const api = await userApi(u.phone)
  await hooks.machines(1)
  const id = await placeOrder(api)
  await hooks.agent()
  await api.post(`/api/v1/instances/${id}/renew`, { data: { duration: '1w' }, headers: { 'Idempotency-Key': crypto.randomUUID() } })
  return { u, api, id }
}

async function selectOption(page: Page, testid: string, name: string) {
  await page.getByTestId(testid).click()
  await page.getByRole('option', { name, exact: true }).click()
}

test('A-FIN-01 订单表筛选与分页', async ({ page }) => {
  const { u, id } = await seedFinanceUser()
  const today = cstDate(await hooks.clock())
  const admin = await createAdmin('finance')
  await loginAdmin(page, admin)
  await page.getByTestId('menu-orders').click()
  await page.getByTestId('fin-filter-user').fill(String(u.id))
  await page.getByTestId('fin-search').click()
  await expect(rows(page, 'ord-id')).toHaveCount(2)
  await expect(page.getByTestId('fin-total')).toHaveText('共 2 条')
  await expect(page.getByTestId('ord-instance').first()).toHaveText(id)
  await selectOption(page, 'fin-filter-kind', '续费')
  await page.getByTestId('fin-search').click()
  await expect(rows(page, 'ord-id')).toHaveCount(1)
  await expect(page.getByTestId('ord-kind')).toHaveText(['续费'])
  await expect(page.getByTestId('ord-amount')).toHaveText(['¥414.00'])
  await selectOption(page, 'fin-filter-status', '已支付')
  await page.getByTestId('fin-from').fill(today)
  await page.getByTestId('fin-to').fill(today)
  await page.getByTestId('fin-search').click()
  await expect(rows(page, 'ord-id')).toHaveCount(1)
  await page.getByTestId('fin-from').fill('2000-01-01')
  await page.getByTestId('fin-to').fill('2000-01-02')
  await page.getByTestId('fin-search').click()
  await expect(page.getByTestId('fin-total')).toHaveText('共 0 条')
  await page.getByTestId('fin-reset').click()
  await expect(page.getByTestId('fin-pager')).toBeVisible()
})

test('A-FIN-02 流水表筛选', async ({ page }) => {
  const { u } = await seedFinanceUser()
  const today = cstDate(await hooks.clock())
  const admin = await createAdmin('finance')
  await loginAdmin(page, admin)
  await page.getByTestId('menu-ledger').click()
  await page.getByTestId('fin-filter-user').fill(String(u.id))
  await page.getByTestId('fin-search').click()
  await expect(rows(page, 'led-id')).toHaveCount(3) // 调账(种子) + 两笔扣款
  await selectOption(page, 'fin-filter-kind', '扣款')
  await page.getByTestId('fin-search').click()
  await expect(rows(page, 'led-id')).toHaveCount(2)
  await expect(page.getByTestId('led-amount').first()).toHaveText('-¥414.00')
  await expect(page.getByTestId('led-amount').first()).toHaveClass(/money-minus/)
  await page.getByTestId('fin-from').fill(today)
  await page.getByTestId('fin-to').fill(today)
  await page.getByTestId('fin-search').click()
  await expect(rows(page, 'led-id')).toHaveCount(2)
})

test('A-FIN-03 充值表筛选', async ({ page }) => {
  const u = await seedUser()
  const api = await userApi(u.phone)
  const create = async (channel: string) => ((await (await api.post('/api/v1/recharges', { data: { amount: 5000, channel } })).json()) as { id: string }).id
  const paid = await create('wechat')
  await api.post(`/api/v1/dev/recharges/${paid}/pay`, { data: {} })
  await create('alipay')
  await hooks.advance(7200)
  await hooks.sweep()
  await create('alipay')
  const admin = await createAdmin('finance')
  await loginAdmin(page, admin)
  await page.getByTestId('menu-recharges').click()
  await page.getByTestId('fin-filter-user').fill(String(u.id))
  await page.getByTestId('fin-search').click()
  await expect(rows(page, 'rch-id')).toHaveCount(3)
  await selectOption(page, 'fin-filter-kind', '支付宝')
  await page.getByTestId('fin-search').click()
  await expect(rows(page, 'rch-id')).toHaveCount(2)
  await selectOption(page, 'fin-filter-status', '已关闭')
  await page.getByTestId('fin-search').click()
  await expect(rows(page, 'rch-id')).toHaveCount(1)
  await page.getByTestId('fin-reset').click()
  await page.getByTestId('fin-filter-user').fill(String(u.id))
  await selectOption(page, 'fin-filter-status', '已支付')
  await page.getByTestId('fin-search').click()
  await expect(page.getByTestId('rch-channel')).toHaveText(['微信支付'])
  await expect(page.getByTestId('rch-paid').first()).toHaveText(/\d{4}-\d{2}-\d{2} \d{2}:\d{2}/)
})

test('A-FIN-04 三张表导出 CSV：文件名、BOM、中文表头、行数', async ({ page }) => {
  const { u } = await seedFinanceUser()
  const api = await userApi(u.phone)
  const rid = ((await (await api.post('/api/v1/recharges', { data: { amount: 1000, channel: 'alipay' } })).json()) as { id: string }).id
  await api.post(`/api/v1/dev/recharges/${rid}/pay`, { data: {} })
  const admin = await createAdmin('finance')
  await loginAdmin(page, admin)
  const cases: [string, string, string, number][] = [
    ['orders', 'orders', '订单号,用户ID,类型,实例,租期,金额(元),已退(元),状态,下单时间', 2],
    ['ledger', 'ledger', '流水ID,用户ID,类型,金额(元),变动后余额(元),关联类型,关联单号,操作人,备注,时间', 4],
    ['recharges', 'recharges', '充值单号,用户ID,渠道,金额(元),状态,渠道单号,操作人,备注,创建时间,支付时间,关闭时间', 1],
  ]
  for (const [menu, prefix, header, dataRows] of cases) {
    await page.getByTestId(`menu-${menu}`).click()
    await page.getByTestId('fin-filter-user').fill(String(u.id))
    await page.getByTestId('fin-search').click()
    await expect(page.getByTestId('fin-total')).toHaveText(`共 ${dataRows} 条`)
    const download = page.waitForEvent('download')
    await page.getByTestId('fin-export').click()
    const file = await download
    expect(file.suggestedFilename()).toMatch(new RegExp(`^${prefix}-\\d{8}-\\d{6}\\.csv$`))
    const text = await new Response(await file.createReadStream()).text()
    expect(text.charCodeAt(0)).toBe(0xfeff)
    const lines = text.replace(/^﻿/, '').trimEnd().split(/\r?\n/)
    expect(lines[0]).toBe(header)
    expect(lines.length - 1).toBe(dataRows)
  }
})
```

Run: `make web && cd web && npx playwright test --project=admin admin/finance.spec.ts`
Expected: 4 passed。

- [ ] **Step 3: 全量验证并提交**

Run: `go vet ./... && go test ./... && cd web && npm test && npm run build && npx playwright test --project=admin`
Expected: 全部 PASS。

```bash
git add web/admin web/e2e
git commit -m "feat(admin): orders/ledger/recharges with shared filter+table+export page; e2e A-FIN

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---
### Task 21: 后台价格与镜像、管理员管理

**Files:**
- Replace: `web/admin/src/views/{CatalogView,AdminsView}.vue`
- Create: `web/e2e/admin/{catalog,admins}.spec.ts`

**Interfaces:**
- Consumes: Task 15 的 `api`、`enterTotpDialog`；Task 6 的动态码重发；shared `labels`、`fmtYuan`、`yuanToCents`。
- Produces:
  - 价格 testid：`cat-product`（`data-product-id`）、每档 `cat-price-amount-<1d|1w|1m>`、`cat-price-enabled-<d>`（`el-switch`，切换即 `PUT`，动态码由请求层弹出）、`cat-price-edit-<d>`；弹窗 `price-amount`（元）、`price-enabled`、`price-error`、`price-submit`。
  - 镜像 testid：`images-create-btn`、行 `img-name`、`img-ref`、`img-desc`、`img-enabled`（`el-switch`）、`img-sort`、`img-up`、`img-down`、`img-edit`；表单 `img-form-name`、`img-form-ref`、`img-form-desc`、`img-form-enabled`、`img-form-sort`、`img-form-submit`。上移 / 下移交换相邻两项的 `sort`（各发一次 `PUT`）。
  - 管理员 testid：`admins-create-btn`、`adm-create-username`、`adm-create-password`、`adm-create-role`、`adm-create-submit`；行 `adm-username`、`adm-role`、`adm-status`、`adm-totp`（已绑定 / 未绑定）、`adm-last-login`、`adm-last-ip`、`adm-edit`、`adm-reset-totp`；编辑弹窗 `adm-form-role`、`adm-form-status`、`adm-form-submit`；重置确认 `adm-reset-text`（「该管理员将被强制下线并在下次登录时重新绑定」）、`adm-reset-ok`。
  - 覆盖：A-CAT-01～03、A-ADM-01～03。测试结束把价格 / 档位改回初始值（¥69.00、1 周启用），避免影响同一轮里之后的用例。

- [ ] **Step 1: 两个页面**

`web/admin/src/views/CatalogView.vue`：

```vue
<script setup lang="ts">
import { durationLabel, fmtYuan, isApiError, labelOf, messageOf, yuanToCents, type AdminImage, type AdminProduct, type Duration, type ImageInput, type Listed } from '@spark/shared'
import { ElMessage } from 'element-plus'
import { onMounted, reactive, ref } from 'vue'
import { api } from '../api'

const products = ref<AdminProduct[]>([])
const images = ref<AdminImage[]>([])
async function load() {
  try {
    const [p, i] = await Promise.all([api.get<Listed<AdminProduct>>('/products'), api.get<Listed<AdminImage>>('/images')])
    products.value = p.items
    images.value = [...i.items].sort((a, b) => a.sort - b.sort || a.id - b.id)
  } catch (e) {
    ElMessage.error(messageOf(e))
  }
}
onMounted(load)
function report(e: unknown) {
  if (!(isApiError(e) && e.code === 'cancelled')) ElMessage.error(messageOf(e))
}

// ---- 价格 ----
const price = reactive({ visible: false, productId: 0, duration: '1d' as Duration, amount: '', enabled: true, error: '', busy: false })
function openPrice(p: AdminProduct, d: Duration) {
  const cur = p.prices.find((x) => x.duration === d)
  Object.assign(price, { visible: true, productId: p.id, duration: d, amount: cur ? (cur.amount / 100).toFixed(2) : '', enabled: cur?.enabled ?? true, error: '' })
}
async function savePrice(productId: number, d: Duration, amount: number, enabled: boolean) {
  await api.put(`/products/${productId}/prices/${d}`, { amount, enabled })
  ElMessage.success('价格已保存')
  await load()
}
async function submitPrice() {
  const cents = yuanToCents(price.amount)
  price.error = cents == null || cents < 1 || cents > 100_000_000 ? '价格需在 0.01 元到 100 万元之间' : ''
  if (price.error) return
  price.busy = true
  try {
    await savePrice(price.productId, price.duration, cents!, price.enabled)
    price.visible = false
  } catch (e) {
    report(e)
  } finally {
    price.busy = false
  }
}
async function togglePrice(p: AdminProduct, d: Duration, enabled: boolean) {
  const cur = p.prices.find((x) => x.duration === d)
  if (!cur) return
  try {
    await savePrice(p.id, d, cur.amount, enabled)
  } catch (e) {
    report(e)
    await load()
  }
}

// ---- 镜像（没有删除接口，用停用代替）----
const img = reactive({ visible: false, editing: null as AdminImage | null, form: { name: '', ref: '', description: '', enabled: true, sort: 0 } as ImageInput, busy: false })
function openImage(i: AdminImage | null) {
  img.editing = i
  img.form = i ? { name: i.name, ref: i.ref, description: i.description, enabled: i.enabled, sort: i.sort } : { name: '', ref: '', description: '', enabled: true, sort: (images.value.at(-1)?.sort ?? 0) + 10 }
  img.visible = true
}
async function submitImage() {
  img.busy = true
  try {
    if (img.editing) await api.put(`/images/${img.editing.id}`, img.form)
    else await api.post('/images', img.form)
    ElMessage.success(img.editing ? '镜像已更新' : '镜像已新增')
    img.visible = false
    await load()
  } catch (e) {
    report(e) // 「镜像地址必须以小写字母或数字开头」等原文
  } finally {
    img.busy = false
  }
}
const put = (i: AdminImage, patch: Partial<ImageInput>) => api.put(`/images/${i.id}`, { name: i.name, ref: i.ref, description: i.description, enabled: i.enabled, sort: i.sort, ...patch })
async function toggleImage(i: AdminImage, enabled: boolean) {
  try {
    await put(i, { enabled })
    ElMessage.success(enabled ? '镜像已启用' : '镜像已停用')
  } catch (e) {
    report(e)
  }
  await load()
}
/** 与相邻项交换 sort 值（各发一次 PUT）；两项 sort 相同时先把本项 ±1 保证顺序变化。 */
async function move(i: AdminImage, dir: -1 | 1) {
  const idx = images.value.findIndex((x) => x.id === i.id)
  const other = images.value[idx + dir]
  if (!other) return
  let a = other.sort
  let b = i.sort
  if (a === b) a += dir
  try {
    await put(i, { sort: a })
    await put(other, { sort: b })
    ElMessage.success('顺序已调整')
  } catch (e) {
    report(e)
  }
  await load()
}
</script>

<template>
  <div class="catalog">
    <el-card v-for="p in products" :key="p.id" :header="`${p.name}（${p.code}）`" data-testid="cat-product" :data-product-id="p.id">
      <el-table :data="p.prices" row-key="duration">
        <el-table-column label="租期" width="100"><template #default="{ row }">{{ labelOf(durationLabel, row.duration) }}</template></el-table-column>
        <el-table-column label="当前价格" width="140"><template #default="{ row }"><span :data-testid="`cat-price-amount-${row.duration}`">{{ fmtYuan(row.amount) }}</span></template></el-table-column>
        <el-table-column label="启用" width="100">
          <template #default="{ row }"><el-switch :model-value="row.enabled" :data-testid="`cat-price-enabled-${row.duration}`" @change="(v: boolean) => togglePrice(p, row.duration, v)" /></template>
        </el-table-column>
        <el-table-column label="操作"><template #default="{ row }"><el-button size="small" :data-testid="`cat-price-edit-${row.duration}`" @click="openPrice(p, row.duration)">修改</el-button></template></el-table-column>
      </el-table>
    </el-card>

    <el-card>
      <template #header><div class="head"><span>镜像</span><el-button type="primary" data-testid="images-create-btn" @click="openImage(null)">新增镜像</el-button></div></template>
      <el-table :data="images" row-key="id">
        <el-table-column label="名称" min-width="160"><template #default="{ row }"><span data-testid="img-name">{{ row.name }}</span></template></el-table-column>
        <el-table-column label="仓库地址" min-width="260"><template #default="{ row }"><code data-testid="img-ref">{{ row.ref }}</code></template></el-table-column>
        <el-table-column label="描述" min-width="160"><template #default="{ row }"><span data-testid="img-desc">{{ row.description }}</span></template></el-table-column>
        <el-table-column label="启用" width="90"><template #default="{ row }"><el-switch :model-value="row.enabled" data-testid="img-enabled" @change="(v: boolean) => toggleImage(row, v)" /></template></el-table-column>
        <el-table-column label="排序" width="80"><template #default="{ row }"><span data-testid="img-sort">{{ row.sort }}</span></template></el-table-column>
        <el-table-column label="操作" width="220">
          <template #default="{ row, $index }">
            <el-button size="small" :disabled="$index === 0" data-testid="img-up" @click="move(row, -1)">上移</el-button>
            <el-button size="small" :disabled="$index === images.length - 1" data-testid="img-down" @click="move(row, 1)">下移</el-button>
            <el-button size="small" data-testid="img-edit" @click="openImage(row)">编辑</el-button>
          </template>
        </el-table-column>
      </el-table>
    </el-card>

    <el-dialog v-model="price.visible" :title="`修改价格：${labelOf(durationLabel, price.duration)}`" width="420" align-center destroy-on-close>
      <el-form label-position="top" @submit.prevent="submitPrice">
        <el-form-item label="价格（元）"><el-input v-model="price.amount" inputmode="decimal" data-testid="price-amount" /></el-form-item>
        <el-form-item label="启用"><el-switch v-model="price.enabled" data-testid="price-enabled" /></el-form-item>
        <p v-if="price.error" class="error" data-testid="price-error">{{ price.error }}</p>
        <el-button type="primary" native-type="submit" :loading="price.busy" data-testid="price-submit">保存</el-button>
      </el-form>
    </el-dialog>

    <el-dialog v-model="img.visible" :title="img.editing ? '编辑镜像' : '新增镜像'" width="520" align-center destroy-on-close>
      <el-form label-position="top" @submit.prevent="submitImage">
        <el-form-item label="名称"><el-input v-model="img.form.name" data-testid="img-form-name" /></el-form-item>
        <el-form-item label="仓库地址（以小写字母或数字开头）"><el-input v-model="img.form.ref" data-testid="img-form-ref" /></el-form-item>
        <el-form-item label="描述"><el-input v-model="img.form.description" data-testid="img-form-desc" /></el-form-item>
        <div class="two">
          <el-form-item label="启用"><el-switch v-model="img.form.enabled" data-testid="img-form-enabled" /></el-form-item>
          <el-form-item label="排序值"><el-input-number v-model="img.form.sort" :controls="false" data-testid="img-form-sort" /></el-form-item>
        </div>
        <el-button type="primary" native-type="submit" :loading="img.busy" data-testid="img-form-submit">保存</el-button>
      </el-form>
    </el-dialog>
  </div>
</template>

<style scoped>
.catalog { display: grid; gap: 16px; }
.head { display: flex; justify-content: space-between; align-items: center; }
.error { color: var(--el-color-danger); font-size: 13px; }
.two { display: grid; grid-template-columns: 1fr 1fr; gap: 12px; }
</style>
```

`web/admin/src/views/AdminsView.vue`：

```vue
<script setup lang="ts">
import { adminStatusLabel, fmtTime, isApiError, labelOf, messageOf, roleLabel, type Admin, type Listed, type Role } from '@spark/shared'
import { ElMessage } from 'element-plus'
import { onMounted, reactive, ref } from 'vue'
import { api } from '../api'

const items = ref<Admin[]>([])
async function load() {
  try {
    items.value = (await api.get<Listed<Admin>>('/admins')).items
  } catch (e) {
    ElMessage.error(messageOf(e))
  }
}
onMounted(load)
function report(e: unknown) {
  if (!(isApiError(e) && e.code === 'cancelled')) ElMessage.error(messageOf(e))
}
const roles: Role[] = ['super', 'ops', 'finance']

const create = reactive({ visible: false, username: '', password: '', role: 'ops' as Role, error: '', busy: false })
async function submitCreate() {
  create.error = /^[a-z][a-z0-9_.-]{2,31}$/.test(create.username) ? (create.password.length >= 12 ? '' : '密码至少 12 位') : '用户名需为 3～32 位小写字母、数字或 _ . -，并以字母开头'
  if (create.error) return
  create.busy = true
  try {
    await api.post('/admins', { username: create.username, password: create.password, role: create.role })
    ElMessage.success('管理员已创建，首次登录时绑定动态码')
    create.visible = false
    await load()
  } catch (e) {
    report(e) // duplicate_username → 「用户名已存在」
  } finally {
    create.busy = false
  }
}

const edit = reactive({ visible: false, target: null as Admin | null, role: 'ops' as Role, status: 'active' as 'active' | 'disabled', busy: false })
function openEdit(a: Admin) {
  Object.assign(edit, { visible: true, target: a, role: a.role, status: a.status })
}
async function submitEdit() {
  if (!edit.target) return
  edit.busy = true
  try {
    await api.put(`/admins/${edit.target.id}`, { role: edit.role, status: edit.status })
    ElMessage.success('已保存')
    edit.visible = false
    await load()
  } catch (e) {
    report(e) // last_super 等原文
  } finally {
    edit.busy = false
  }
}

const reset = reactive({ visible: false, target: null as Admin | null, busy: false })
async function submitReset() {
  if (!reset.target) return
  reset.busy = true
  try {
    await api.post(`/admins/${reset.target.id}/reset-totp`, {})
    ElMessage.success('已重置动态码')
    reset.visible = false
    await load()
  } catch (e) {
    report(e)
  } finally {
    reset.busy = false
  }
}
</script>

<template>
  <el-card>
    <div class="toolbar"><el-button type="primary" data-testid="admins-create-btn" @click="Object.assign(create, { visible: true, username: '', password: '', role: 'ops', error: '' })">新增管理员</el-button></div>
    <el-table :data="items" row-key="id">
      <el-table-column label="用户名" min-width="140"><template #default="{ row }"><span data-testid="adm-username">{{ row.username }}</span></template></el-table-column>
      <el-table-column label="角色" width="120"><template #default="{ row }"><span data-testid="adm-role">{{ labelOf(roleLabel, row.role) }}</span></template></el-table-column>
      <el-table-column label="状态" width="100"><template #default="{ row }"><el-tag :type="row.status === 'active' ? 'success' : 'info'" data-testid="adm-status">{{ labelOf(adminStatusLabel, row.status) }}</el-tag></template></el-table-column>
      <el-table-column label="动态码" width="100"><template #default="{ row }"><span data-testid="adm-totp">{{ row.totp_confirmed ? '已绑定' : '未绑定' }}</span></template></el-table-column>
      <el-table-column label="最后登录" width="160"><template #default="{ row }"><span data-testid="adm-last-login">{{ fmtTime(row.last_login_at) || '—' }}</span></template></el-table-column>
      <el-table-column label="IP" width="140"><template #default="{ row }"><span data-testid="adm-last-ip">{{ row.last_login_ip || '—' }}</span></template></el-table-column>
      <el-table-column label="操作" width="220">
        <template #default="{ row }">
          <el-button size="small" data-testid="adm-edit" @click="openEdit(row)">修改角色 / 状态</el-button>
          <el-button size="small" type="danger" plain data-testid="adm-reset-totp" @click="Object.assign(reset, { visible: true, target: row })">重置动态码</el-button>
        </template>
      </el-table-column>
    </el-table>

    <el-dialog v-model="create.visible" title="新增管理员" width="460" align-center destroy-on-close>
      <el-form label-position="top" @submit.prevent="submitCreate">
        <el-form-item label="用户名"><el-input v-model="create.username" data-testid="adm-create-username" autocomplete="off" /></el-form-item>
        <el-form-item label="初始密码（至少 12 位）"><el-input v-model="create.password" type="password" show-password data-testid="adm-create-password" autocomplete="new-password" /></el-form-item>
        <el-form-item label="角色"><el-select v-model="create.role" data-testid="adm-create-role"><el-option v-for="r in roles" :key="r" :label="labelOf(roleLabel, r)" :value="r" /></el-select></el-form-item>
        <p v-if="create.error" class="error">{{ create.error }}</p>
        <el-button type="primary" native-type="submit" :loading="create.busy" data-testid="adm-create-submit">创建</el-button>
      </el-form>
    </el-dialog>

    <el-dialog v-model="edit.visible" :title="`修改 ${edit.target?.username}`" width="420" align-center destroy-on-close>
      <el-form label-position="top" @submit.prevent="submitEdit">
        <el-form-item label="角色"><el-select v-model="edit.role" data-testid="adm-form-role"><el-option v-for="r in roles" :key="r" :label="labelOf(roleLabel, r)" :value="r" /></el-select></el-form-item>
        <el-form-item label="状态"><el-select v-model="edit.status" data-testid="adm-form-status"><el-option label="正常" value="active" /><el-option label="已停用" value="disabled" /></el-select></el-form-item>
        <el-button type="primary" native-type="submit" :loading="edit.busy" data-testid="adm-form-submit">保存</el-button>
      </el-form>
    </el-dialog>

    <el-dialog v-model="reset.visible" title="重置动态码" width="420" align-center>
      <p data-testid="adm-reset-text">该管理员将被强制下线并在下次登录时重新绑定。</p>
      <template #footer><el-button @click="reset.visible = false">取消</el-button><el-button type="danger" :loading="reset.busy" data-testid="adm-reset-ok" @click="submitReset">确定重置</el-button></template>
    </el-dialog>
  </el-card>
</template>

<style scoped>
.toolbar { display: flex; justify-content: flex-end; margin-bottom: 12px; }
.error { color: var(--el-color-danger); font-size: 13px; }
</style>
```

Run: `cd web && npm run build`
Expected: 通过。

- [ ] **Step 2: 端到端用例**

`web/e2e/admin/catalog.spec.ts`：

```ts
import { expect, request, test, type Page } from '@playwright/test'
import { createAdmin, enterTotpDialog, loginAdmin } from '../helpers/admin'
import { toast } from '../helpers/console'
import { CONSOLE_URL } from '../helpers/hooks'
import { runStamp } from '../helpers/phone'

type PublicProduct = { prices: { duration: string; amount: number }[] }
type PublicImage = { id: number; name: string }
async function publicCatalog() {
  const ctx = await request.newContext({ baseURL: CONSOLE_URL })
  const products = ((await (await ctx.get('/api/v1/products')).json()) as { items: PublicProduct[] }).items
  const images = ((await (await ctx.get('/api/v1/images')).json()) as { items: PublicImage[] }).items
  return { product: products[0], images }
}
const imageRow = (page: Page, name: string) => page.getByRole('row').filter({ has: page.getByTestId('img-name').filter({ hasText: name }) })

test('A-CAT-01 修改 1 天价格为 ¥79 需动态码；控制台返回 7900；再改回', async ({ page }) => {
  const admin = await createAdmin('super')
  await loginAdmin(page, admin)
  await page.getByTestId('menu-catalog').click()
  await expect(page.getByTestId('cat-price-amount-1d')).toHaveText('¥69.00')
  await page.getByTestId('cat-price-edit-1d').click()
  await page.getByTestId('price-amount').fill('79')
  await page.getByTestId('price-submit').click()
  await enterTotpDialog(page, admin.totp_secret!)
  await toast(page, '价格已保存')
  await expect(page.getByTestId('cat-price-amount-1d')).toHaveText('¥79.00')
  expect((await publicCatalog()).product.prices.find((p) => p.duration === '1d')?.amount).toBe(7900)
  await page.getByTestId('cat-price-edit-1d').click()
  await page.getByTestId('price-amount').fill('69')
  await page.getByTestId('price-submit').click()
  await expect(page.getByTestId('totp-dialog')).toBeHidden()
  await toast(page, '价格已保存')
  await expect(page.getByTestId('cat-price-amount-1d')).toHaveText('¥69.00')
})

test('A-CAT-02 下架 1 周档位后市场不再显示；再启用', async ({ page }) => {
  const admin = await createAdmin('super')
  await loginAdmin(page, admin)
  await page.getByTestId('menu-catalog').click()
  await page.getByTestId('cat-price-enabled-1w').click()
  await enterTotpDialog(page, admin.totp_secret!)
  await toast(page, '价格已保存')
  await expect(page.getByTestId('cat-price-enabled-1w')).toHaveAttribute('aria-checked', 'false')
  expect((await publicCatalog()).product.prices.map((p) => p.duration)).toEqual(['1d', '1m'])
  await page.getByTestId('cat-price-enabled-1w').click()
  await toast(page, '价格已保存')
  await expect(page.getByTestId('cat-price-enabled-1w')).toHaveAttribute('aria-checked', 'true')
  expect((await publicCatalog()).product.prices.map((p) => p.duration)).toEqual(['1d', '1w', '1m'])
})

test('A-CAT-03 镜像新增、编辑、上移 / 下移、停用，与市场一致；非法 ref 报错', async ({ page }) => {
  const admin = await createAdmin('super')
  const name = `E2E 镜像 ${runStamp}`
  await loginAdmin(page, admin)
  await page.getByTestId('menu-catalog').click()
  await page.getByTestId('images-create-btn').click()
  await page.getByTestId('img-form-name').fill(name)
  await page.getByTestId('img-form-ref').fill('-bad/ref:1')
  await page.getByTestId('img-form-submit').click()
  await toast(page, '镜像地址必须以小写字母或数字开头')
  await page.getByTestId('img-form-ref').fill(`registry.spark.internal/e2e/${runStamp}:1`)
  await page.getByTestId('img-form-desc').fill('端到端测试镜像')
  await page.getByTestId('img-form-submit').click()
  await toast(page, '镜像已新增')
  let { images } = await publicCatalog()
  expect(images.at(-1)?.name).toBe(name)

  await imageRow(page, name).getByTestId('img-edit').click()
  await page.getByTestId('img-form-name').fill(`${name} v2`)
  await page.getByTestId('img-form-submit').click()
  await toast(page, '镜像已更新')
  const name2 = `${name} v2`
  await expect(imageRow(page, name2)).toHaveCount(1)

  await imageRow(page, name2).getByTestId('img-up').click()
  await toast(page, '顺序已调整')
  // 市场顺序以 /api/v1/images（只含启用镜像）为准：上移后排在倒数第二，下移后回到最后
  await expect.poll(async () => (await publicCatalog()).images.at(-2)?.name).toBe(name2)
  await imageRow(page, name2).getByTestId('img-down').click()
  await toast(page, '顺序已调整')
  await expect.poll(async () => (await publicCatalog()).images.at(-1)?.name).toBe(name2)

  await imageRow(page, name2).getByTestId('img-enabled').click()
  await toast(page, '镜像已停用')
  images = (await publicCatalog()).images
  expect(images.some((i) => i.name === name2)).toBe(false)
})
```

`web/e2e/admin/admins.spec.ts`：

```ts
import { expect, test, type Page } from '@playwright/test'
import { createAdmin, enterTotpDialog, loginAdmin, nextUsername, totpNow } from '../helpers/admin'
import { toast } from '../helpers/console'

const adminRow = (page: Page, username: string) => page.getByRole('row').filter({ has: page.getByTestId('adm-username').filter({ hasText: username }) })

async function loginUntilSetup(page: Page, username: string, password: string) {
  await page.goto('/admin/login')
  await page.getByTestId('login-username').fill(username)
  await page.getByTestId('login-password').fill(password)
  await page.getByTestId('login-submit').click()
}

test('A-ADM-01 新增管理员需动态码；重复用户名提示；新管理员登录进入绑定流程', async ({ page }) => {
  const root = await createAdmin('super')
  await loginAdmin(page, root)
  await page.getByTestId('menu-admins').click()
  const username = nextUsername()
  const password = 'initial-password-123456'
  await page.getByTestId('admins-create-btn').click()
  await page.getByTestId('adm-create-username').fill(username)
  await page.getByTestId('adm-create-password').fill(password)
  await page.getByTestId('adm-create-role').click()
  await page.getByRole('option', { name: '运维' }).click()
  await page.getByTestId('adm-create-submit').click()
  await enterTotpDialog(page, root.totp_secret!)
  await toast(page, '管理员已创建')
  await expect(adminRow(page, username).getByTestId('adm-role')).toHaveText('运维')
  await expect(adminRow(page, username).getByTestId('adm-totp')).toHaveText('未绑定')
  await page.getByTestId('admins-create-btn').click()
  await page.getByTestId('adm-create-username').fill(username)
  await page.getByTestId('adm-create-password').fill(password)
  await page.getByTestId('adm-create-submit').click()
  await toast(page, '用户名已存在')
  await page.keyboard.press('Escape')
  await page.getByTestId('layout-logout').click()
  await loginUntilSetup(page, username, password)
  await expect(page.getByTestId('login-setup-qr')).toBeVisible()
})

test('A-ADM-02 修改角色后菜单变化；停用后登录失败', async ({ page }) => {
  const root = await createAdmin('super')
  const target = await createAdmin('ops')
  await loginAdmin(page, root)
  await page.getByTestId('menu-admins').click()
  await adminRow(page, target.username).getByTestId('adm-edit').click()
  await page.getByTestId('adm-form-role').click()
  await page.getByRole('option', { name: '财务' }).click()
  await page.getByTestId('adm-form-submit').click()
  await enterTotpDialog(page, root.totp_secret!)
  await toast(page, '已保存')
  await expect(adminRow(page, target.username).getByTestId('adm-role')).toHaveText('财务')
  await page.getByTestId('layout-logout').click()
  await loginAdmin(page, { ...target, role: 'finance' })
  await expect(page.getByTestId('menu-orders')).toBeVisible()
  await expect(page.getByTestId('menu-machines')).toHaveCount(0)
  await page.getByTestId('layout-logout').click()

  await loginAdmin(page, root)
  await page.getByTestId('menu-admins').click()
  await adminRow(page, target.username).getByTestId('adm-edit').click()
  await page.getByTestId('adm-form-status').click()
  await page.getByRole('option', { name: '已停用' }).click()
  await page.getByTestId('adm-form-submit').click()
  await toast(page, '已保存')
  await expect(adminRow(page, target.username).getByTestId('adm-status')).toHaveText('已停用')
  await page.getByTestId('layout-logout').click()
  await loginUntilSetup(page, target.username, target.password)
  await toast(page, '用户名或密码错误')
})

test('A-ADM-03 重置他人动态码后其下次登录进入绑定步骤', async ({ page }) => {
  const root = await createAdmin('super')
  const target = await createAdmin('ops')
  await loginAdmin(page, root)
  await page.getByTestId('menu-admins').click()
  await adminRow(page, target.username).getByTestId('adm-reset-totp').click()
  await expect(page.getByTestId('adm-reset-text')).toHaveText('该管理员将被强制下线并在下次登录时重新绑定。')
  await page.getByTestId('adm-reset-ok').click()
  await enterTotpDialog(page, root.totp_secret!)
  await toast(page, '已重置动态码')
  await expect(adminRow(page, target.username).getByTestId('adm-totp')).toHaveText('未绑定')
  await page.getByTestId('layout-logout').click()
  await loginUntilSetup(page, target.username, target.password)
  await expect(page.getByTestId('login-setup-qr')).toBeVisible()
  const secret = (await page.getByTestId('login-setup-secret').textContent())!.trim()
  await page.getByTestId('login-setup-code').fill(await totpNow(secret))
  await page.getByTestId('login-setup-submit').click()
  await expect(page.getByTestId('login-recovery-code')).toHaveCount(8)
})
```

Run: `make web && cd web && npx playwright test --project=admin admin/catalog.spec.ts admin/admins.spec.ts`
Expected: 6 passed。

- [ ] **Step 3: 全量验证并提交（后台 50 个用例齐）**

Run: `go vet ./... && go test ./... && cd web && npm test && npm run build && npx playwright test --project=admin && npm run e2e:coverage`
Expected: 全部 PASS；后台 50 passed；`e2e:coverage` 打印「覆盖表核对通过：102 个用例 ID（控制台 52，后台 50）」。

```bash
git add web/admin web/e2e
git commit -m "feat(admin): price and image management, admin accounts with TOTP step-up; e2e A-CAT, A-ADM

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

---
### Task 22: 收尾：文档、遗留清单、覆盖表核对与 `make check` 全量

**Files:**
- Modify: `README.md`（本地开发、部署注意、API 约定）
- Create: `docs/superpowers/followups-plan5.md`
- Modify: `docs/superpowers/followups-plan1.md`、`docs/superpowers/followups-plan4.md`（标记本计划认领的条目）

**Interfaces:**
- Consumes: 前面全部任务。
- Produces: 可交付的文档；`make check` 一条命令全绿的证据。

- [ ] **Step 1: README**

`README.md`「本地开发」一节的命令块替换为：

```bash
make test         # Go 测试 + 前端单测（首次会下载 PostgreSQL 二进制）
make devdb        # 终端 1：启动本地数据库（端口 5433）
make run          # 终端 2：启动 platform（控制台 http://localhost:8080 ，后台 http://127.0.0.1:8081/admin/ ，托管上次 make web 的产物）
make web-install  # 首次：安装前端依赖（Node 22 / npm 10，本机在 ~/.local/bin）
make web          # 构建前端到 internal/web/dist/{console,admin}（Go embed）
make e2e          # 端到端：构建前端 → 核对覆盖表 → Playwright（自动拉起 cmd/e2e-server，需要本机 Chrome）
make check        # test + e2e
```

并在该节末尾追加：

````markdown
### 前端开发

```bash
cd web && npm run dev:console   # http://127.0.0.1:5173 ，/api 代理到 8080
cd web && npm run dev:admin     # http://127.0.0.1:5174/admin/ ，/admin/api 代理到 8081
```

控制台登录：任意手机号 + 验证码 `000000`；后台管理员用 `platform admin create` 创建。
端到端测试全量约 10～15 分钟；只跑子集：`cd web && npx playwright test --project=admin` 或 `-g 'C-RCH'`；调试：`npx playwright test --headed --project=console-mobile -g 'C-RCH-04'`，失败的 trace 用 `npx playwright show-trace web/test-results/<目录>/trace.zip`。
CI 机器没有 Chrome 时设 `E2E_BROWSER_CHANNEL=chromium` 并 `npx playwright install chromium`。
````

「部署注意」末尾追加：

```markdown
- 用户控制台与用户 API 同在 `:8080`（`/` 为页面，`/api/*` 为接口），前面的反向代理不需要额外路由规则；管理后台与后台 API 同在 `:8081`（`/admin/`），仍只监听内网地址。
- `make build` 会先构建前端再编译后端，二进制内含页面；`index.html` 不缓存，发布后刷新即生效，`assets/` 带内容哈希长缓存。
- `SPARK_ENV=staging` 与 `prod` 都会给 Cookie 加 `Secure`，所以 staging 必须走 HTTPS。
```

「API 约定」末尾追加：

```markdown
- `GET /api/v1/kyc` 返回用户最近一条实名记录（含驳回原因），没有记录时 `record` 为 `null`；个人姓名只保留首字。
- 实例视图含 `product_name`、`image_name`（镜像停用后仍返回名称）；充值单视图含 `remaining_seconds`（服务器计算的剩余支付秒数）。
- 后台 `GET /admin/api/v1/products`、`GET /admin/api/v1/images` 对全部角色可读；`GET /admin/api/v1/audit` 每条含 `actor_name`。
```

- [ ] **Step 2: 遗留清单**

`docs/superpowers/followups-plan5.md`：

```markdown
# 计划 5 遗留事项

来源：计划 5 实施过程中的裁定（2026-10-17 起）。均不阻塞合并。

## 上线前需要核对
- 手机端 `alipays://` 拉起只能真机验证；上线前用真机走一次支付宝沙箱（自动化只断言链接格式与回页查询）。
- 品牌占位 `Spark Cloud`：替换 `web/shared/src/brand.ts` 与两个应用 `public/brand/logo.svg`。
- CI 机器需预装 Chrome，或设 `E2E_BROWSER_CHANNEL=chromium`。

## 可以继续延后
- 后台收到 `403 totp_setup_required` 时回到登录页重新输入密码，而不是直接进入绑定步骤（临时会话由密码步骤签发）。
- 用户实例列表上限 100 条、订单 50 条不分页；企业大客户前需要后端分页。
- 后台用户详情的流水不含操作人；需要看操作人时到流水页按用户 ID 筛选。
- 控制台与后台的 `safeRedirect` 各一份实现（避免 shared 依赖路由）；如再增加应用可上移到 shared。
```

`docs/superpowers/followups-plan1.md`「前端 / 上线前」一节改为：

```markdown
## 前端 / 上线前
- ✅ 计划 5 已处理：释放「停机且有待开始续费」的实例会放弃已付时长：需要界面二次确认，或服务端加确认参数。
- 剩余不足 1 小时时续费或恢复，两档提醒都会被视为已发送。
- ✅ 计划 5 已处理：staging 走 HTTPS 时也应开启 Secure Cookie。
- ✅ 计划 5 已处理：前端下单与续费应发送 `Idempotency-Key` 请求头。
```

`docs/superpowers/followups-plan4.md`「可以继续延后」里的「换机后新实例的 SSH 密码与原实例不同，需要在前端提示用户（计划 5）。」前面加「✅ 计划 5 已处理：」。

- [ ] **Step 3: 全量核对**

Run: `cd web && npm run e2e:coverage`
Expected: `覆盖表核对通过：102 个用例 ID（控制台 52，后台 50）`。再核对没有被禁止的写法：

```bash
grep -rnE "test\.(skip|fixme|only)|describe\.(skip|only)" web/e2e/console web/e2e/admin ; echo "grep exit=$? (1 表示没有匹配，正确)"
grep -rn "waitForTimeout" web/e2e ; echo "grep exit=$? (1 表示没有匹配，正确)"
grep -rn "v-html" web/console/src web/admin/src ; echo "grep exit=$? (1 表示没有匹配，正确)"
grep -rn "localStorage" web/console/src web/admin/src ; echo "只应出现在 admin/src/layouts/AdminLayout.vue（侧边栏折叠）"
```

Run: `make check 2>&1 | tee /tmp/plan5-check.log | tail -40`
Expected（按顺序）：`go vet` 无输出；`go test ./...` 全部 `ok`；Vitest 全部项目通过；`vue-tsc` 无错误；两个 `vite build` 成功；`覆盖表核对通过`；Playwright 结束行 `154 passed`（52 × 2 + 50），没有 `skipped` / `flaky`。全程约 10～15 分钟。若有失败：用 `npx playwright show-trace` 看 trace 修复，然后重新跑完整 `make check`，不接受只跑子集的结果。

Run: `make build && ls -la bin/ && unzip -l /dev/null 2>/dev/null; go tool nm bin/platform-linux-amd64 | grep -c 'spark-cloud/internal/web' `
Expected: `bin/platform-linux-amd64`、`bin/platform-linux-arm64` 生成；`nm` 计数 > 0（`internal/web` 已链接进二进制）；`internal/web/dist/console/index.html` 与 `internal/web/dist/admin/index.html` 存在（`ls internal/web/dist/*/index.html`）。

- [ ] **Step 4: 提交**

```bash
git add README.md docs/superpowers/followups-plan5.md docs/superpowers/followups-plan1.md docs/superpowers/followups-plan4.md
git commit -m "docs: frontend dev/deploy notes, plan 5 followups, mark claimed plan 1/4 items

Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>"
```

然后 `git log --oneline feat/plan4-admin-payments..HEAD | wc -l` 应为 22（每个任务一个提交），`git status` 干净。

---

## 自检记录（写计划时完成）

**规格覆盖：** §0.1 决策 1～7 → brand（Task 5）、响应式与后台 1200px（Task 8、15）、渠道按 UA（Task 13）、npm workspaces（Task 5）、Go embed（Task 3）、手写类型（Task 6）、`cmd/e2e-server`（Task 4）；§1 工程结构与版本 → Task 5、7、8、15；§2 共用层（http / errors / idempotency / format / device / brand / types / labels）→ Task 5、6；§3 托管、Makefile、部署 → Task 3、5、22；§4 控制台各页 → Task 8（导航 / 登录 / 我的 / 帮助 / 404）、9（实例列表）、10（详情）、11（市场）、12（钱包）、13（充值）、14（实名）；§5 后台各页 → Task 15（导航 / 登录 / 设置 / 403 / 动态码弹框）、16（仪表盘 / 告警 / 审计）、17（机器 / PDU）、18（实例）、19（用户 / 企业认证）、20（订单 / 流水 / 充值单 / 导出）、21（价格镜像 / 管理员）；§6 B1～B11 → Task 3（B1）、1（B2、B8、B10）、5（B3）、2（B4～B7、B11）、4（B9）；§7 测试分层、e2e-server、规范、造数据手册、覆盖表 102 行、Vitest U-01～U-14 → Task 4、5、6、7 与各页面任务（每行 ID 在对应任务的 spec 中各出现一次；U-01～U-05、U-08～U-10 在 Task 5，U-06、U-07、U-14 在 Task 6，U-13 在 Task 7，U-11 在 Task 9，U-12 在 Task 15）；§8 安全（CSP、无 v-html、Cookie、redirect、密码只在内存、钩子回环）→ Task 3、4、8、10、22 核对命令；§9 开发流程 → Task 22 README；§10 认领遗留 → Task 10（C-DET-07）、11（C-MKT-07）、1（B10）、18（A-INS-04）、8（C-ACC-02）、22（标记）；§11 风险 → `web/go.mod`（Task 5）、`E2E_BROWSER_CHANNEL`（Task 7）；§12 裁定 1～13 → 实例列表无过滤开关（Task 9）、释放一层确认（Task 10）、sweep 默认心跳（Task 4）、A-ADM-02 不测 last_super（Task 21）、B8 生产保护（Task 1）、B6（Task 2）、钩子实现约束（Task 4）、C-RCH-02 缩窄视口（Task 13）、C-RCH-03/04 双 project（Task 13）、`make e2e` 先 `web`（Task 5）、无恢复码下载（Task 15）。

**占位扫描：** 全文无 TBD / TODO / 「类似 Task N」/ 「补充错误处理」；每个代码步骤给出代码；`LedgerView.vue` 与 `RechargesView.vue` 以 `OrdersView.vue` 为模板并列出全部列名与 testid；admin 的 `package.json` / `tsconfig.json` / `vitest.config.ts` / `env.d.ts` 与 console 同内容并写明差异。

**类型一致性：** `createHttp` / `HttpOptions` / `ApiError`（Task 6）在 Task 8、15 的 `api.ts` 与各页面中按同一签名使用；`useCountdown({ now?, onZero? })` 返回 `{ remaining, start, stop }`（Task 5）在 Task 10、13 中一致；`useIdempotencyKey(deps)` 返回 `{ current(), rotate() }`（Task 5）在 Task 10、11 一致；`usePolling(fn, ms, { enabled, immediateOnVisible })`（Task 9）在 Task 11、13 一致；`usePaged` 返回 `{ items, total, page, size, loading, load, reset }`（Task 16）在 Task 17～20 一致；helpers：`hooks.*`、`seedUser`、`loginConsole`、`nav`、`userApi`、`placeOrder`、`toast`、`dialog`（Task 7）、`runningInstance` / `instanceRow` / `instanceOf`（Task 9）、`cstTime` / `cstDate`（Task 10 / 16）、`createAdmin` / `loginAdmin` / `enterTotpDialog` / `totpNow` / `adminApi` / `adminPost`（Task 7）、`drainIdleMachines`（Task 11）、`createPDU` / `registerMachine`（Task 17）、`uploadLicense` / `submitEnterpriseKyc`（Task 19）在后续任务中按定义签名调用；testid 以各任务 Interfaces 为准，后续任务引用的名称均在定义任务中出现。

**Review Focus：** 5 条各有测试归属：1 → Task 8 `login.spec.ts`；2 → Task 8 `session.spec.ts`；3 → Task 5 `format.spec.ts`「边缘写法」；4 → Task 5 `useCountdown 按时间源`；5 → Task 10 C-DET-06 双击断言。

---

## 计划评审裁定（2026-10-17，独立评审）

评审方式：逐任务对照设计 `2026-10-17-plan5-frontend-design.md` 与真实代码（`internal/app`、`api`、`adminapi`、`admin`、`identity`、`fleet`、`rental`、`billing`、`wallet`、`catalog`、`storage`、`audit`、`platform/*`、`migrations/`、chi v5.4.0 源码、spike 里的 element-plus 2.14.7 产物）核对；以下「裁定 — 理由」已直接改入正文。

1. **`ApiError` 增加 `details`（完整错误响应体），机器 CSV 导入从 `err.details.errors` 读逐行错误，删掉 `fetch` 重放** — 重放绕过请求层的 401 / 超时处理、把同一份 CSV 再发一次，而且依赖「后端整批不导入」这个可变前提；`errorOf` 本来就解析了整个响应体，保留它是零成本。Task 5 / 6 / 17 与 Task 22 遗留清单同步，`http.spec.ts` 增加 `details` 断言。
2. **A-ERR-01 改为真实降权触发 403，不再用 `page.route` 改写请求路径** — 原写法证明的是「页面能处理一个被篡改的请求返回的 403」，不是任何真实路径。后端 `admin.Authenticate` 每次请求都 `JOIN admins` 重读角色，`Update` 只在停用时删会话，所以「super 看机器页时被另一位 super 降为 finance，再点查询」会得到真实的 403 `forbidden`；用例顺带验证刷新后进入 403 页、菜单按新角色过滤。为此 Task 7 helper 增加 `adminSend` / `adminPut`。
3. **C-NAV-02 的类名与位置断言改用 `AppDialog` 的 `data-testid` 定位** — 实测 element-plus 2.14.7：`role="dialog"` 在 `.el-overlay-dialog` 包装层，`is-fullscreen`、自定义 `class="sheet"` 与 `$attrs` 都落在内层 `.el-dialog`。原用例的 `not.toHaveClass(/is-fullscreen/)` 与贴底 `boundingBox` 断言都会在包装层上「通过」，是典型的看似通过实则没测到；现在还追加了宽度 420 / 560、居中、通栏、全屏尺寸等几何断言。Global Constraints 补充了这条规则。
4. **C-RCH-04 两个 project 都接管浏览器定时器，手机分支改为「不可见 → 支付 → 推进 9 秒无轮询 → 可见 → 恰好多一次查询并显示成功」** — 原手机分支没有 `page.clock`，3 秒真实轮询自己就会查到已支付，`visibilitychange` 的逻辑有没有都通过。
5. **新增 `notifyError`（Task 8），控制台视图的请求失败提示一律经它，`unauthorized` / `cancelled` 静默；Task 12 `WalletView` 已改** — 原 `WalletView` 在 401 时既由 `handleUnauthorized` 弹「请先登录」，又在 catch 里 `ElMessage.error('请先登录')`，C-AUTH-06「只提示一次」必然失败（并发三个 401 会叠出四条）。
6. **C-INS-04 的剩余时长正则由 `(1 天|23 小时 59 分)` 改为 `23 小时 5\d 分`** — `loginConsole` 为绕过发码限流每次推进假时钟 61 秒，下单后再登录看到的是 23 小时 58 分；原正则必失败。
7. **A-AUD-01 改用 `user.ban` 造审计记录，并断言对象链接可点击跳转** — `alert.resolve` 的 `Entry(..., nil)` diff 为 `null`，格式化后只有 `{}`，原「两空格缩进」正则必失败；`user.ban` 的 diff 是 `{reason}`，可以精确断言格式化结果，且 `target_type=user` 能验证「对象可点击」。
8. **A-MCH-01 两台机器都登记在唯一机柜下，所有筛选叠加机柜条件；分页 total 先取接口值再断言** — `ListMachines` 按 `rack, slot, id` 排序，整轮测试共享数据库，不带机柜筛选去找 `hooks/machines` 登记的那台（rack 固定 `E2E`、slot 1）会落到后面几页；`hooks/agent {machine_id}` 可以只让目标机器上线，无需再用 `serial_prefix`。
9. **`playwright.config.ts` 的 `reuseExistingServer` 固定为 `false`** — `cmd/e2e-server` embed 的是进程启动那一刻的前端产物，复用手工起的旧进程等于测上一次的页面，恰恰违反「不能假设」；端口被占用时报错更好。副作用是每轮都是全新数据库，A-CAT 改价 / 下架即使中途失败也不会污染下一轮。设计 §7.2 原文写的是 `!process.env.CI`，需同步（见下）。
10. **Task 3 预期通过的测试名修正** — 仓库里没有 `TestAuthenticatedResponsesAreNoStore`，后台 API `no-store` 的断言在 `TestLoginAndTOTPSetupOverHTTP` 里；`auth_test.go` 已引入 `net/http`、`strings`，import 说明相应改正。

**核查结论（未改动，已逐项确认）：**
- 设计覆盖：§6 B1～B11 → Task 3 / 1 / 5 / 2 / 2 / 2 / 2 / 1 / 4 / 1 / 2；§7.2 钩子 9 条全部在 Task 4 实现且有 Go 单测；§7.5 控制台 52 + 后台 50 个 ID 在 Task 8～21 各出现一次且每条都有具体断言，`coverage.json` 与 `coverage.mjs` 校验数量；U-01～U-14 分布在 Task 5 / 6 / 7 / 9 / 15；§12 评审裁定 1～13 均已落实。没有「只能假设通过」的功能。
- 代码一致性：Task 1～4 引用的 `identity.Deps`、`adminapi.Server` / `newIPLimiter` / `loginLimit`、`config.Config` 字段、`dbtest.Key / T0 / CreateUser`、`fleet.NewMachine / Register / Heartbeat / PendingCommands / ListAlerts / Get / Pool`、`rental.Place / PlaceRequest / HandleCommandResult / Sweep / State*`、`billing.Sweep`、`wallet.AdminAdjust / Balance / ListLedger`、`admin.Create / EnrollTOTP / ConfirmEnrollment / FindByUsername / Login / TOTPCode / TOTPStep`、`identity.UserKYCRecords / SubmitPersonalKYC / GetUser`、`catalog.ProductIDByCode`、`randx.Digits`、`apperr.As`、`audit.Log`、`paging.Page`、`db.IsNoRows` 全部存在且签名一致；chi v5.4.0 的 `NotFound` 会向已挂载 / 后挂载的子路由传播；`http.FileServerFS` 可用（Go 1.27）。Task 6 全部 TypeScript 类型字段与 Go JSON 标签逐个对照一致（含 `AdminLedgerRow.operator_id`、`Recharge.qr_code` omitempty、`Image` 不含 `ref`、`Order` 不含 `user_id`、`Dashboard.machines.by_state` 为 map）。前端用到的全部错误文案（`验证码错误或已过期`、`用户名或密码错误`、`登录失败次数过多`、`动态码错误或已使用过`、`序列号已存在`、`PDU 与插座号需同时填写`、`该机器未配置 PDU`、`请填写 SNMP community`、`修改 PDU 地址时请重新填写 SNMP community`、`该机器上还有未结束的实例，不能结束维护`、`暂无空闲机器`、`待支付的充值单过多`、`60 秒内已提交过相同的操作`、`用户名已存在`、`原密码不正确`、`无权执行该操作`、`仅支持 JPG、PNG、PDF 文件`、`实名核验未通过：`）与 26 个审计动作名、CSV 中文表头与 `name-20060102-150405.csv` 文件名、恢复码 `xxxxx-xxxxx`、身份证 `前 3 后 4` 脱敏、`CloseAfter = 2h`、`QRTTL = 15min`、`retain_until` 仅 `pending_seconds == 0`、种子商品 / 价格 / 三个镜像名均与代码一致。任务顺序可执行：Task 1～4 每步只依赖前一任务产出；Task 5 起的 `web/go.mod` 保证 `go vet ./...` 不进 `node_modules`。
- 安全：`DisableRateLimits` 只是 `app.Options` 字段，`cmd/platform` 传零值、无配置或环境变量入口，`Build` 在 `Env == "prod"` 时拒绝启动并有单测；`internal/e2ehooks` 只被 `cmd/e2e-server` 引用，`make build` 只编译 `cmd/platform`，钩子监听 `127.0.0.1` 且处理器再校验回环地址（有单测）。CSP：Vite 产物只有外链 `<script type="module">` 与 `<link rel="stylesheet">`，无内联脚本；Element Plus 的行内 `style` 属性由 `style-src 'unsafe-inline'` 覆盖；二维码走 `canvas`、执照缩略图走 `blob:`、后台执照预览走同源 `/admin/api/v1/files/`，均在 `img-src 'self' data: blob:` 内；无字体文件、无第三方源；`alipays://` 是导航不受 CSP 约束；`form-action` 不影响 `@submit.prevent` 的表单。
- 占位 / YAGNI：全文无 TBD；`downloadText`、`IdLink.disabled`、`commandTypeLabel`、`kycResultLabel` 等都有调用方；未发现多余抽象。

**本轮未改、留给主控的事项：**
- 设计文档同步（评审只改计划）：§7.2 `reuseExistingServer: !process.env.CI` → `false`；§2.1 `ApiError` 增 `details`；§2.2 增「控制台页面经 `notifyError` 提示，`unauthorized` / `cancelled` 静默」一句；§7.5 A-ERR-01 的断言要点改为「会话中途被降权 → 真实 403 → 提示而不是白屏」。
