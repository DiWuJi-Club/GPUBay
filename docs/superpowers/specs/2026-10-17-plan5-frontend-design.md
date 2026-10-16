# Spark Cloud — 子项目：前端（用户控制台与管理后台，计划 5）设计文档

- 日期：2026-10-17
- 状态：已评审，修改后通过（裁定见 §12）
- 依赖：计划 1（后端核心闭环）、计划 4（管理后台 API 与外部插件）；分支 `feat/plan5-frontend` 基于计划 4 分支，所有后端 API 已就绪
- 总设计：`docs/superpowers/specs/2026-10-14-spark-cloud-mvp-design.md`
- 架构验证：一次性 spike（scratchpad `spike5/`，不进仓库），结论已并入本文 §1.4

## 0. 目标与范围

让用户和运营团队不再靠 curl 使用平台：
- **用户控制台**（`web/console`）：登录、算力市场下单、实例管理（SSH 信息、续费、释放）、钱包与充值、实名认证、账户与帮助。手机端与电脑端同等精心。
- **管理后台**（`web/admin`）：计划 4 的全部后台能力（登录与动态码、仪表盘、机器 / PDU、告警、实例售后、用户与企业认证、订单 / 流水 / 充值与导出、价格与镜像、管理员、审计、个人设置）。只做电脑端。
- **共用层**（`web/shared`）：请求封装、错误处理、类型定义、金额 / 时间格式化、品牌配置、响应式工具。
- **后端配套改动**（§6）：静态托管与单页应用回退、`make build` 先构建前端、少量接口补充、端到端测试专用服务。

**完成标准**（用户的硬要求，原话「你不能假设，要把功能测试完才算」）：§7.5 覆盖表里的每一行都有对应的 Playwright 端到端测试，跑在真实后端（embedded-postgres + mock 插件，Go 程序真实启动）和本机 Chrome 上；用户控制台每条流程在电脑和手机两种尺寸都通过；管理后台在电脑尺寸通过；共用逻辑有 Vitest 单测；`make check` 一条命令全部通过。没有测过的功能不算完成。

### 0.1 已确认的决策

1. 品牌先用 `Spark Cloud` 占位；名称、logo、主色集中在 `shared/src/brand.ts` 一处配置。
2. 用户控制台手机端与电脑端同等精心；管理后台只做电脑端（最小宽度 1200px）。
3. 手机充值只给支付宝（拉起 App，回到页面时查询到账）；微信只在电脑端。是否手机按 User-Agent 判断，不看屏幕宽度。
4. `web/` 用 npm workspaces：`shared`、`console`、`admin`。Element Plus + 自写响应式，不引入 Vant。
5. 构建产物由 Go embed 进 platform 二进制：控制台挂在 `:8080` 的 `/`，后台挂在 `:8081` 的 `/admin/`，与各自 API 同源，复用现有 Cookie（`sc_session` Path=/，`sc_admin` Path=/admin/）。
6. 接口类型在 `shared` 里手写（后端没有 OpenAPI 描述文件），字段与 Go 的 JSON 标签逐个对应；接口稳定后再改为自动生成。
7. 端到端测试的后端是独立的测试专用入口 `cmd/e2e-server`（§7.2）：复用 `app.Build` 与同一套路由 / 静态托管，额外提供测试钩子。生产二进制 `cmd/platform` 不包含任何钩子代码。

### 0.2 不包含

- 实例端口管理、Web 访问、GPU / 内存小图、SSH 公钥管理（依赖计划 2 / 3 的 Agent 与接入层）。
- 优惠券、发票、工单、站内信、档期预订、自动续费。
- 国际化（界面只有简体中文）、深色模式、PWA。
- ESLint / Prettier 等代码风格工具链（类型检查 `vue-tsc` 与测试是质量门槛；风格由编辑器格式化）。
- 后台的手机适配。
- 到期提醒「剩余不足 1 小时时两档提醒都视为已发送」（计划 1 遗留，属于后端提醒逻辑，不在本计划）。

## 1. 工程结构与技术选型

### 1.1 目录

```
web/
├── package.json             # workspaces: shared, console, admin；overrides.typescript = ~5.9.3
├── package-lock.json
├── tsconfig.base.json       # strict、moduleResolution bundler、paths @spark/shared
├── playwright.config.ts     # 三个 project：console-desktop、console-mobile、admin
├── e2e/                     # 端到端测试（跨 console / admin，共用 helpers）
│   ├── helpers/             # hooks.ts、totp.ts、console.ts、admin.ts、phone.ts、money.ts
│   ├── fixtures/            # license.png、license.pdf、bad.gif、machines-ok.csv、machines-bad.csv
│   ├── console/*.spec.ts
│   └── admin/*.spec.ts
├── shared/                  # @spark/shared
│   ├── package.json
│   ├── vitest.config.ts
│   └── src/
│       ├── brand.ts         # 品牌：name、logoUrl、primaryColor
│       ├── http.ts          # 请求封装（§2.1）
│       ├── errors.ts        # ApiError、错误码 → 文案
│       ├── format.ts        # 金额、时间、时长（§2.4）
│       ├── device.ts        # isMobileUA()、useMobile()（§2.5）
│       ├── idempotency.ts   # Idempotency-Key 生成与复用
│       ├── labels.ts        # 枚举值 → 中文（实例 / 订单 / 流水 / 充值 / 机器 / 告警 / 角色 / 审计动作）
│       ├── types/           # user.ts、catalog.ts、instance.ts、wallet.ts、recharge.ts、kyc.ts、admin.ts
│       └── __tests__/
├── console/                 # 用户控制台
│   ├── index.html
│   ├── vite.config.ts       # base '/'，proxy /api → 127.0.0.1:8080，outDir ../../internal/web/dist/console
│   └── src/{main.ts, App.vue, router.ts, store/session.ts, layouts/, views/, components/, composables/}
└── admin/                   # 管理后台
    ├── index.html
    ├── vite.config.ts       # base '/admin/'，proxy /admin/api → 127.0.0.1:8081，outDir ../../internal/web/dist/admin
    └── src/{main.ts, App.vue, router.ts, store/session.ts, layouts/, views/, components/, composables/}
```

Go 侧新增 `internal/web`（embed 与单页应用处理器，§3.1）和 `cmd/e2e-server`（§7.2）。

### 1.2 依赖版本（固定，`package-lock.json` 入库）

| 包 | 版本 | 用途 |
|---|---|---|
| vue | 3.5.44 | |
| vue-router | 5.4.0 | |
| element-plus | 2.14.7 | UI 组件；按需引入 |
| unplugin-vue-components / unplugin-auto-import | 32.1.0 / 21.1.0 | Element Plus 按需引入 |
| qrcode / @types/qrcode | 1.5.4 / 1.5.5 | 浏览器本地生成二维码（充值码、动态码绑定码） |
| vite | 8.3.4 | |
| @vitejs/plugin-vue | 6.0.10 | |
| typescript | ~5.9.3（根 `overrides` 固定） | TS 7 与 vue-tsc 不兼容 |
| vue-tsc | 3.3.13 | 类型检查 |
| vitest / @vue/test-utils / jsdom | 5.0.5 / 2.5.1 / 30.1.3 | 单测 |
| @playwright/test | 1.64.1 | 端到端；用本机 Chrome（`channel: 'chrome'`），不下载浏览器 |

不引入：Pinia（两个应用各只有一个会话状态，用模块级 `reactive` 即可）、ECharts（仪表盘柱状图自绘 SVG）、axios（用 `fetch`）、dayjs 时区插件（用 `Intl.DateTimeFormat`）。

### 1.3 构建与脚本

根 `package.json` scripts：
- `dev:console` / `dev:admin`：Vite 开发服务器（5173 / 5174）。
- `build`：`npm run -ws --if-present build`（shared 无构建；console 与 admin 各执行 `vue-tsc --noEmit && vite build`）。
- `test`：`vitest run` 跑 shared 与两个应用的单测。
- `e2e`：`playwright test`（见 §7）。

Vite 配置要点（来自 spike）：
- `Components({ resolvers: [ElementPlusResolver()] })` + `AutoImport(...)` 按需引入；路由全部懒加载（`() => import(...)`）。首屏约 90KB gzip。
- `optimizeDeps.include` 预列用到的 `element-plus/es/components/*/style/css`，避免开发时首次打开多刷新一次。
- `build.outDir` 指向 `internal/web/dist/{console,admin}`，`emptyOutDir: true`；资源文件名带内容哈希（Vite 默认）。

### 1.4 架构验证结论（已并入设计）

| 项目 | 结论 |
|---|---|
| TypeScript | 根 `package.json` `overrides.typescript = "~5.9.3"`，TS 7 与 vue-tsc 不兼容 |
| 包体积 | Element Plus 整包 1MB → 按需引入 + 路由懒加载，首屏约 90KB gzip |
| 手机弹窗 | 一句话确认框用底部弹出样式；只有表单类弹窗（续费、实名、充值、实例操作表单）全屏 |
| 开发体验 | `optimizeDeps.include` 预列 Element Plus 样式，首次打开不再多刷新一次 |
| 测试选择器 | `el-input` 的 `data-testid` 会落到内部的 `<input>` 上，`getByTestId('phone').fill()` 可直接用；自定义组件要把 `data-testid` 显式透传到真正可交互的元素 |
| 截图 | 截图前等弹窗动画结束（`animations: 'disabled'` 或等待 `.el-dialog` 的 `transitionend`） |
| Go embed | 深链接回退到 index.html；index.html `Cache-Control: no-cache`，`assets/` 长缓存；未知 `/api/*` 返回 JSON 404；同源 Cookie 登录有效 |
| 开发代理 | 同一套端到端测试改走 Vite 代理端口也能通过，Cookie 正常 |

## 2. 共用层（`@spark/shared`）

### 2.1 请求封装 `http.ts`

```ts
export interface ApiError { status: number; code: string; message: string; requestId: string; details?: unknown /* 完整错误响应体，例如机器 CSV 导入的逐行 errors */ }
export interface HttpOptions { idempotencyKey?: string; signal?: AbortSignal; quiet?: boolean /* 401 时不触发 onUnauthorized，仅用于会话探测 */ }
export function createHttp(cfg: {
  base: string                                  // '/api/v1' 或 '/admin/api/v1'
  onUnauthorized: () => void                    // 401：清会话并跳登录
  promptTotp?: () => Promise<string | null>     // 后台：弹出动态码输入框，取消返回 null
}): {
  get<T>(path: string, query?: Record<string, unknown>, opt?: HttpOptions): Promise<T>
  post<T>(path: string, body?: unknown, opt?: HttpOptions): Promise<T>
  put<T>(path: string, body?: unknown, opt?: HttpOptions): Promise<T>
  upload<T>(path: string, file: File): Promise<T>   // multipart，字段名 file
  download(path: string, query?: Record<string, unknown>): Promise<{ blob: Blob; filename: string }>
}
```

规则：
- 所有请求 `credentials: 'same-origin'`；写请求 `Content-Type: application/json`（后端对非 JSON 写请求返回 400，兼作 CSRF 防护）；`upload` 不手动设 Content-Type，由浏览器生成 multipart 边界。
- 查询参数值为 `undefined`、`null`、`''` 时不拼入 URL（后台列表筛选留空即「不筛选」）。
- 响应非 2xx：解析 `{code, message, request_id}` 抛出 `ApiError`；解析失败（网关 HTML、网络中断）抛 `ApiError{status, code: 'network', message: '网络异常，请稍后重试', requestId: ''}`。`download` 用于 CSV 导出：先 `fetch`，若响应 `Content-Type` 是 `application/json` 则按错误处理（例如超过 10 万行时后端返回 400 `export_too_large`，见 `internal/platform/csvx`），否则从 `Content-Disposition` 取文件名并返回 Blob，由调用方用 `URL.createObjectURL` 触发下载。
- **401** `unauthorized`：调用 `onUnauthorized()` 后仍把错误抛给调用方（调用方通常不再处理）。`quiet: true` 的请求不调用 `onUnauthorized`，只用于启动时的会话探测 `GET /me`（市场、帮助等公开页面的匿名访客会收到 401，不能因此跳登录页）。
- **403 `totp_required`**（仅后台）：调用 `promptTotp()`；用户输入后把 `totp` 字段合并进原请求体重发一次（后端在请求体 `totp` 字段里接收动态码）；再次 `invalid_totp` 则再弹一次（最多 3 次）；用户取消则抛出 `ApiError{code: 'cancelled'}`，调用方静默处理。多个并发请求同时收到 `totp_required` 时只弹一个框，共用一次输入。后端在 5 分钟窗口内不再返回 `totp_required`，所以前端不维护窗口状态。
- **403 `totp_setup_required`**（仅后台）：跳到登录页的绑定步骤。
- 其他错误原样抛出，由页面按错误码处理（§2.2）。
- 不做自动重试（写操作重试由用户点击触发，靠 Idempotency-Key 保证安全）。
- 请求超时 30 秒（`AbortSignal.timeout`），上传与下载 120 秒。

### 2.2 错误处理 `errors.ts`

- 控制台页面统一经 `notifyError(err)` 提示，`unauthorized` / `cancelled` 静默不提示。
- `messageOf(err)`：`ApiError` 取 `message`；`status >= 500` 时追加「（请求号 xxx）」便于排查；非 `ApiError` 显示「操作失败，请稍后重试」。
- 页面级约定：
  - 表单字段校验错误在字段下方显示（Element Plus 表单规则），不发请求。
  - 请求失败用 `ElMessage.error(messageOf(err))`；成功用 `ElMessage.success`。
  - 需要引导的错误码在控制台统一处理：`kyc_required` → 对话「请先完成实名认证」+「去实名」按钮（跳 `/kyc?redirect=当前路径`）；`insufficient_balance` → 「余额不足」+「去充值」（跳 `/wallet/recharge?redirect=当前路径`）；`banned` → 清会话、跳登录页并显示「账号已被封禁」；`no_capacity` → 「暂无空闲机器」并刷新市场数据；`rate_limited` → 原文提示。
  - 后台 `forbidden` → 「无权执行该操作」；`duplicate`（60 秒内重复资金操作）、`export_too_large`（导出超过 10 万行）→ 原文提示。

### 2.3 幂等键 `idempotency.ts`

- 下单与续费必须带 `Idempotency-Key` 请求头（认领计划 1 遗留）。
- `useIdempotencyKey()`：打开确认框时生成 `crypto.randomUUID()`；同一次确认的重复点击、网络错误后的再次点击都复用该键；请求成功或用户修改了参数（租期、镜像、实例）后重新生成。键长 36 ≤ 后端上限 64。
- 后端对同用户同键的重放返回首次结果（HTTP 201 / 200 同样处理；并发的两份请求由后端咨询锁串行化，后到的一份同样拿到首次结果），所以双击只扣一次款。确认按钮在请求进行中仍置为 loading 禁用，幂等键是对网络错误后重试的兜底，不是替代。

### 2.4 金额与时间 `format.ts`

- 后端金额一律是**分**（整数）。`fmtYuan(cents)` → `¥69.00`、`¥1,599.00`、`-¥10.00`（千分位分隔，两位小数，负数在 ¥ 前加负号）。`yuanToCents(input)`：接受字符串，校验为非负、最多两位小数的数字，返回分；否则返回 `null`，表单显示「金额格式不正确」。
- 时间一律按**北京时间**显示：`fmtTime(iso)` → `2026-10-17 14:05`，`fmtTimeSec` 到秒，`fmtDate` → `2026-10-17`，实现用 `Intl.DateTimeFormat('zh-CN', { timeZone: 'Asia/Shanghai', hour12: false, ... })`；后台日期筛选把用户选的日期按 `YYYY-MM-DD` 原样发给后端（后端按北京时间解释，`to` 含当天）。
- 时长 `fmtDuration(seconds)` → `6 天 23 小时 59 分`（不足 1 分显示「不足 1 分钟」；0 显示「0 分钟」）。
- 倒计时 `useCountdown(remainingSeconds, fetchedAt)`：以拿到数据的本地时刻为基准每秒递减，不比较设备时钟与服务器时钟；归零后触发一次刷新。

### 2.5 响应式与设备 `device.ts`

- `isMobileUA()`：`/Android|iPhone|iPad|iPod|Mobile/i.test(navigator.userAgent)`，只用于**充值渠道**的选择（决策 3）。
- `useMobile()`：`matchMedia('(max-width: 767px)')`，用于布局。断点 768px。
- 控制台布局规则：
  - ≥ 768px：左侧菜单（可折叠）+ 内容区；列表用 `el-table`。
  - < 768px：顶部标题栏 + 底部四个标签（算力市场 / 我的实例 / 钱包 / 我的，`padding-bottom: env(safe-area-inset-bottom)`）；列表用卡片；可点元素高度 ≥ 44px；输入框字号 16px（避免 iOS 聚焦放大）。
  - 弹窗：`confirm()` 封装在手机上渲染为底部弹出（`el-dialog` 自定义类 `sheet`：固定在底部、宽度 100%、圆角顶边）；表单弹窗在手机上 `fullscreen`。电脑上都是居中对话框（宽度 420 / 560）。
- 后台只按 ≥ 1200px 设计，不做窄屏适配；`<meta viewport>` 仍设置。

### 2.6 品牌 `brand.ts`

```ts
export const brand = { name: 'Spark Cloud', shortName: 'Spark', logoUrl: '/brand/logo.svg', primaryColor: '#2563eb', supportText: '如需帮助请联系客服' }
```
两个应用在 `main.ts` 里把 `primaryColor` 写入 `--el-color-primary`，`index.html` 的 `<title>` 与 logo 从此处取。替换品牌只改这一个文件和 `public/brand/` 下的图片。

### 2.7 类型 `types/`

手写接口类型，字段名与 Go JSON 标签一致（snake_case）。关键类型：`User{id, phone, kind, kyc_status, status, created_at}`、`Product{id, code, name, spec: {gpu, memory_gb, disk_tb, cpu}, prices: {duration, amount}[], idle_count}`、`Image{id, name, description}`、`Instance`（见 §4.4）、`Order{id, kind, instance_id, product_id, duration, amount, status, refunded_amount, created_at}`、`LedgerRow{id, amount, balance_after, kind, ref_type, ref_id, remark, created_at}`、`Recharge{id, channel, amount, status, qr_code?, remaining_seconds, expires_at, paid_at, closed_at, created_at}`、`KYCRecord`、后台的 `Admin`、`Machine`、`PDU`、`Alert`、`AdminInstance`、`AdminOrder`、`AdminLedgerRow`、`AdminRecharge`、`AdminProduct`、`AdminImage`、`AuditEntry`、`Dashboard`、`RefundQuote`。枚举值用字符串字面量联合类型，中文映射放在 `labels.ts`（实例状态、订单类型 / 状态、流水类型、充值状态、机器状态、告警级别、管理员角色、审计动作）。

## 3. 构建、托管与部署

### 3.1 Go 静态托管 `internal/web`

```go
package web
//go:embed dist
var dist embed.FS           // dist/README.md 入库保证 embed 始终有文件；dist/console、dist/admin 由前端构建生成，gitignore

func Console() http.Handler  // fs.Sub(dist, "dist/console")
func Admin() http.Handler    // fs.Sub(dist, "dist/admin")，内部路径前缀 /admin/
```

处理器行为（spike 的 `spaHandler` 加安全头）：
- 请求路径对应的文件存在 → 直接返回；`assets/` 下的文件 `Cache-Control: public, max-age=31536000, immutable`，其他文件（`favicon`、`brand/`）`max-age=3600`。
- 其他路径 → 返回 `index.html`，`Cache-Control: no-cache`。
- 构建产物不存在（未执行 `make web`）→ 503 文本「前端尚未构建，请执行 make web」，不影响 API。
- 所有响应加安全头：`Content-Security-Policy`（§8）、`X-Content-Type-Options: nosniff`、`X-Frame-Options: DENY`、`Referrer-Policy: same-origin`。

挂载方式：
- 用户端 `api.Server.Router()`：`/api/*` 走现有 API 子路由（未知 `/api/*` 由 chi 的 `NotFound` 返回 JSON 404 `{code: "not_found"}`）；其余路径交给 `web.Console()`。路由级中间件 `requireJSONForWrites` 对 GET / HEAD 直接放行，静态文件不受影响；静态文件不加 `NoStore`（`NoStore` 只在需登录的 API 分组上）。
- 后台 `adminapi.Server.Router()`：`/admin/api/*` 走现有子路由（保留 `NoStore`、`requireJSONForWrites`）；`/admin/` 与 `/admin/*` 非 API 路径交给 `web.Admin()`；`/` 重定向到 `/admin/`；其他路径 JSON 404。
- 开发时 `make run` 的后端也会托管已构建的产物；前端开发用 Vite 开发服务器 + 代理（§9）。

### 3.2 Makefile

```
web-install:  cd web && npm ci
web:          cd web && npm run build            # 产物到 internal/web/dist/{console,admin}
web-test:     cd web && npm test
e2e:          $(MAKE) web && cd web && npm run e2e:coverage && npx playwright test   # 自动拉起 cmd/e2e-server（§7.2）
test:         go vet ./... && go test ./... && $(MAKE) web-test
build:        $(MAKE) web && GOOS=linux GOARCH=amd64 go build ... && GOOS=linux GOARCH=arm64 go build ...
check:        $(MAKE) test && $(MAKE) e2e
```
`build` 先构建前端再编译后端，保证二进制里是最新产物。`e2e` 同样先 `web`：端到端测试访问的是 `cmd/e2e-server` embed 进去的产物，不先构建就会测到上一次的前端（或 503）。`.gitignore` 增加 `internal/web/dist/console/`、`internal/web/dist/admin/`、`web/node_modules/`、`web/test-results/`、`web/playwright-report/`。

### 3.3 部署说明

- 用户控制台与 API 同一个 `:8080`，前面的反向代理（HTTPS 终止）不需要额外路由规则。
- 管理后台与后台 API 同一个 `:8081`，仍只监听内网地址；运营人员通过内网或 VPN 访问 `http(s)://<内网地址>:8081/admin/`。
- `index.html` 不缓存，发布新版本后刷新即生效；哈希资源长缓存。

## 4. 用户控制台

### 4.1 导航与路由

| 路由 | 页面 | 登录 |
|---|---|---|
| `/login` | 登录 | 否（已登录访问则跳 `redirect` 或 `/market`） |
| `/market` | 算力市场 | 否 |
| `/instances` | 我的实例 | 是 |
| `/instances/:id` | 实例详情 | 是 |
| `/wallet` | 钱包（流水 / 订单 / 充值记录） | 是 |
| `/wallet/recharge` | 发起充值 | 是 |
| `/wallet/recharge/:id` | 充值单（二维码 / 拉起支付宝 / 结果） | 是 |
| `/kyc` | 实名认证 | 是 |
| `/account` | 我的 | 是 |
| `/help` | 使用帮助 | 否 |
| `/` | 重定向到 `/market` | |
| 其他 | 404 页 | |

- `router.beforeEach`：会话未知时先 `GET /api/v1/me`（`quiet: true`，应用启动只请求一次，结果缓存在 `store/session.ts`；公开页面也探测，用于顶栏手机号与「立即租用」的分支）；未登录跳 `/login?redirect=<原路径>`；`redirect` 只接受站内相对路径（以 `/` 开头且不以 `//` 开头），否则忽略。
- 电脑端左侧菜单：算力市场、我的实例、钱包、我的；顶部右侧显示手机号（中间四位打码）与「使用帮助」。手机端底部四个标签，帮助入口在「我的」页内。
- 页面标题 `document.title = 页面名 + ' - ' + brand.name`。

### 4.2 登录 `/login`

接口：
- `POST /api/v1/auth/sms-code {phone}` → `{ok: true}`。错误：`bad_request`（手机号格式）、`rate_limited`（同号 60 秒 1 次、每日 10 次、同 IP 每小时 30 次）。
- `POST /api/v1/auth/login {phone, code}` → `User`，并下发 `sc_session` Cookie（30 天）。错误：`invalid_code`（400，含 5 次试错超限）、`banned`（403）。

行为：
- 手机号正则 `^1[3-9]\d{9}$` 前端预校验；「发送验证码」点击后禁用并显示 60 秒倒计时（倒计时状态只在内存，刷新页面即重置，服务端仍有 60 秒限制会返回 `rate_limited`）。
- 验证码输入框 `inputmode="numeric" autocomplete="one-time-code"`，6 位。开发 / 测试环境的 mock 短信固定验证码 `000000`，页面不显示任何提示（避免误上生产）。
- 登录成功写入 session store，跳转 `redirect` 或 `/market`。未注册过的手机号由后端自动注册，前端无差别。

### 4.3 算力市场 `/market`

接口：
- `GET /api/v1/products` → `{items: Product[]}`，`prices` 只含已启用档位，`idle_count` 为该商品空闲且在线的机器数。
- `GET /api/v1/images` → `{items: Image[]}`（只有启用的镜像，按 sort 排序）。
- 已登录时 `GET /api/v1/wallet` → `{balance}` 用于确认框。
- `POST /api/v1/orders {product_id, duration, image_id}` + `Idempotency-Key` → 201 `{order: Order, instance: Instance}`。错误：`unauthorized`、`kyc_required`、`banned`、`insufficient_balance`（402）、`no_capacity`（409）、`bad_request`（租期 / 镜像无效）、`idempotency_conflict`。

行为：
- 每个商品一张卡片：名称、`spec` 四项（GPU、内存 GB、硬盘 TB、CPU）、空闲台数、三档价格（按 `1d / 1w / 1m` 顺序，未启用的档位不显示）。
- 卡片内选择租期（单选，默认第一档）与镜像（下拉，默认第一个）；「立即租用」按钮在 `idle_count === 0` 时禁用并显示「暂无空闲」。
- 点击「立即租用」：未登录 → 跳登录；未实名（`me.kyc_status !== 'verified'`）→ 引导去实名（不发下单请求）；否则弹确认框：商品、租期、镜像、价格、当前余额、扣款后余额；余额不足时确认按钮变为「去充值」。
- 确认后 `POST /orders`；成功跳 `/instances/:id`，并提示「下单成功，正在开通」。
- 30 秒自动刷新一次空闲台数（页面可见时）。

### 4.4 我的实例 `/instances` 与实例详情 `/instances/:id`

实例视图（`GET /api/v1/instances`、`GET /api/v1/instances/{id}`）字段：`id（形如 ins-xxxxxxxx）, state, product_id, image_id, product_name, image_name（§6 B5 新增）, suspended, expires_at, remaining_seconds, pending_seconds, retain_until, ssh_command, created_at`。状态值与中文：`creating` 开通中、`running` 运行中、`stopped` 已到期停机、`wiping` 洗机中、`released` 已释放、`failed` 开通失败。`retain_until` 只在 `stopped && pending_seconds == 0` 时非空（停机后有待开始的续费时数据不会被清除）。列表最多返回最近 100 条（后端上限，不分页）。

列表行为：
- 每个实例一张卡片（电脑端为表格行）：ID、镜像名、状态标签（颜色：开通中蓝、运行中绿、停机橙、洗机中灰、已释放灰、失败红）、剩余时长倒计时（running）或「数据保留至 时间」（stopped 且 `retain_until` 非空）或「已付 X 待开始」（`pending_seconds > 0`）。
- `suspended === true` 时额外显示「计时已暂停（机器维护中）」。
- 有 `creating` 或 `wiping` 实例时每 10 秒刷新（`setInterval`，页面不可见时暂停）。
- 按创建时间倒序显示全部实例（含已释放 / 失败），不提供筛选开关（见评审裁定 1）。

详情页接口：
- `GET /api/v1/instances/{id}/password` → `{password}`。
- `POST /api/v1/instances/{id}/renew {duration}` + `Idempotency-Key` → `{order}`。后端允许 `creating / running / stopped`；其他状态 `invalid_state`。
- `POST /api/v1/instances/{id}/release {}` → `{ok}`。后端允许 `running / stopped`。

详情行为：
- 基本信息：ID、商品名、镜像名、状态、创建时间、到期时间（`expires_at`）、剩余时长倒计时、`pending_seconds` 换算的「已付待开始时长」。
- SSH：`ssh_command` 非空（running）时显示命令和「复制」按钮；密码默认隐藏为「••••••••」，点「显示」才请求 `/password`（每次点击都重新请求，不缓存到任何存储），可复制；离开页面即丢弃。非 running 状态显示「实例运行中才能连接」。
- 续费：按钮在 `creating / running / stopped` 显示。弹出表单（手机全屏）：选择租期，显示价格（来自 `/products` 中该 `product_id` 的当前价格）、余额、扣后余额；余额不足改为「去充值」。成功后提示「续费成功」并刷新详情；停机实例提示「正在重新启动」。
- 释放：按钮在 `running / stopped` 显示。确认框固定文案「释放后数据将被清除，已付费用不退，确定释放吗？」；当 `state === 'stopped' && pending_seconds > 0` 时追加一行红字「该实例有已付费但尚未开始计时的 X（时长），释放后这部分时长也会作废」（认领计划 1 遗留）。确认框的确认按钮文案「确定释放」（红色），点击即执行，不再叠加第二层点击确认（评审裁定 2）。成功后状态变为洗机中。
- 开通失败：显示「开通失败，费用已原路退回余额」。
- 换机：用户端接口不暴露换机来源，换机后的新实例按普通实例展示；「SSH 密码已变化」由后台换机成功提示运营通知用户（§5.6），帮助页说明「如遇换机，SSH 密码会变化，请在详情页重新查看」。

### 4.5 钱包 `/wallet`

接口：
- `GET /api/v1/wallet` → `{balance}`。
- `GET /api/v1/wallet/ledger?before_id=&limit=` → `{items: LedgerRow[]}`，按 id 倒序，游标 `before_id` 取最后一条的 id，`limit` 默认 20、最大 100。`kind`：`recharge` 充值、`order_pay` 扣款、`refund` 退款、`admin_adjust` 调账；未知值原样显示。
- `GET /api/v1/orders` → `{items: Order[]}`（最近 50 条，后端上限）。
- `GET /api/v1/recharges?page=&page_size=` → `{items: Recharge[], total}`。

行为：
- 顶部余额卡片 + 「充值」按钮；未实名用户点充值先引导实名（后端创建充值单要求已实名）。
- 三个标签页：流水（滚动到底自动加载更多，使用 `IntersectionObserver` 哨兵；正数绿色 `+¥100.00`，负数 `-¥69.00`，显示变动后余额与备注）、订单（类型 新购 / 续费、实例 ID 可点击、租期、金额、状态 已支付 / 已退款 / 部分退款）、充值记录（分页，状态 待支付 / 已支付 / 已关闭；待支付的可点进 `/wallet/recharge/:id` 继续支付）。
- 标签选择写在 query（`?tab=orders`），刷新保持。

### 4.6 充值 `/wallet/recharge` 与 `/wallet/recharge/:id`

接口：
- `GET /api/v1/pay/channels` → `{items: string[]}`（`alipay`、`wechat` 的子集，按名称排序；mock 支付两者都有）。
- `POST /api/v1/recharges {amount, channel}` → 201 `Recharge`（`qr_code` 仅待支付时返回，有效 15 分钟；`remaining_seconds` 为服务器计算的剩余秒数，§6 B5）。错误：`kyc_required`、`banned`、`bad_request`（金额不在 1 元～100 万元）、`too_many_pending`（同时 5 张待支付）、`pay_unavailable`（502）。
- `GET /api/v1/recharges/{id}` → `Recharge`（轮询）。
- `POST /api/v1/dev/recharges/{id}/pay {}` → `Recharge`（仅 mock 支付且非生产环境注册；生产环境不存在该路由）。

发起页行为：
- 金额档位 50 / 100 / 500 / 1000 元 + 自定义输入（1～1,000,000 元，最多两位小数，`yuanToCents`）。
- 渠道：`isMobileUA()` 为真时只显示支付宝（若 `channels` 不含 `alipay` 显示「当前暂不支持手机端充值」）；电脑端显示 `channels` 里有的渠道（微信 / 支付宝），默认选第一个。
- 提交后跳 `/wallet/recharge/:id`。

充值单页行为：
- 加载 `GET /recharges/{id}`：`paid` 显示成功页（金额、到账时间、「返回钱包」）；`closed` 显示「充值单已关闭，请重新发起」；`pending` 进入支付态。
- 支付态（电脑）：用 `qrcode` 在浏览器把 `qr_code` 画成 240px 二维码；显示渠道、金额、由 `remaining_seconds` 驱动的倒计时（`useCountdown`，每次轮询结果都重新校准）；每 3 秒 `GET /recharges/{id}`，页面不可见时暂停、恢复可见时立即查一次；变为 `paid` 提示「充值成功」并刷新 session 余额；倒计时归零后再查一次，仍 pending 则显示「二维码已过期，请重新发起」。
- 支付态（手机，支付宝）：按钮「打开支付宝付款」，`href` 为 `alipays://platformapi/startapp?saId=10000007&qrcode=<encodeURIComponent(qr_code)>`；下方小字链接「没有自动打开？点此在浏览器中打开」指向 `qr_code` 原始地址。监听 `visibilitychange`，回到页面时立即查询并恢复 3 秒轮询；其余与电脑端相同。
- `qr_code` 以 `mock://` 开头（开发 / 测试环境的模拟支付）时，二维码区域改为显示「开发环境模拟支付」与按钮「模拟支付成功」，点击调 `POST /dev/recharges/{id}/pay`，随后按正常轮询逻辑显示成功。手机端同样显示该按钮。
- 离开页面时清理定时器。

### 4.7 实名认证 `/kyc`

接口：
- `GET /api/v1/kyc` → `{record: KYCRecord | null}`（§6 B4 新增；最近一条记录，含 `kind, result, reason, real_name, license_no, created_at`，个人的 `real_name` 只显示姓氏加「*」）。
- `POST /api/v1/kyc/personal {name, id_number}` → `User`（成功即 `kyc_status = 'verified'`）。错误：`bad_request`（姓名为空 / 身份证格式）、`kyc_rejected`（400，message 为「实名核验未通过：原因」）、`kyc_in_progress`（409）、`kyc_unavailable`（503）、`invalid_state`（已认证或审核中）、`rate_limited`（每日 5 次）。
- `POST /api/v1/uploads`（multipart `file`）→ 201 `{key, size, content_type, purpose, created_at}`。错误：`file_too_large`（413，10MB）、`unsupported_file_type`（415，仅 JPG / PNG / PDF，按内容判断）。
- `POST /api/v1/kyc/enterprise {company_name, license_no, license_file}` → `{ok}`，之后 `kyc_status = 'pending'`。错误：`bad_request`（企业名称 / 信用代码格式 `^[0-9A-HJ-NPQRTUWXY]{2}\d{6}[0-9A-HJ-NPQRTUWXY]{10}$` / 未上传）、`invalid_state`。

行为：
- 顶部状态条：`none` 未认证、`pending` 审核中（企业）、`verified` 已认证（显示类型与名称）、`rejected` 未通过（显示 `record.reason`，可重新提交）。
- `verified` / `pending` 时隐藏表单。
- 个人 / 企业两个标签。个人：姓名、身份证号（18 位，末位可为 X，前端预校验），提交后按结果提示；`kyc_rejected` 的原因显示在表单上方红框，并刷新状态条。企业：企业名称、统一社会信用代码、营业执照（`el-upload` 手动上传，选择后立即调 `/uploads`，成功显示文件名与预览缩略图（图片）或 PDF 图标；失败显示错误并允许重选），提交后状态条变「审核中，通常 1 个工作日内完成」。
- `redirect` 参数：认证成功后跳回（例如从市场跳来的下单流程）。

### 4.8 我的 `/account` 与帮助 `/help`

- `/account`：手机号、实名状态（链接到 `/kyc`）、注册时间、「使用帮助」入口、「退出登录」（`POST /api/v1/auth/logout` → 清 session → 跳 `/login`）。
- `/help`：静态内容三节：怎么用 SSH 连接（命令、密码、换机后密码会变化）、数据保留规则（到期停机保留 6 小时，续费可恢复；主动释放立即清除且不退款）、计费规则（预付包时长 1 天 / 1 周 / 1 月，从开通成功开始计时，机器故障暂停计时并补回）。内容为 Markdown 风格的静态 Vue 组件，不请求接口。

## 5. 管理后台

### 5.1 导航、路由与权限

所有后台接口前缀 `/admin/api/v1`。路由（`createWebHistory('/admin/')`）：

| 路由 | 页面 | 角色（菜单与路由守卫一致） |
|---|---|---|
| `/login` | 登录（三步：密码 / 绑定动态码 / 动态码） | — |
| `/` | 仪表盘 | 全部 |
| `/machines`、`/machines/:id` | 机器 | super、ops |
| `/pdus` | PDU | super、ops |
| `/alerts` | 告警 | 全部 |
| `/instances`、`/instances/:id` | 实例 | 全部（操作按角色） |
| `/users`、`/users/:id` | 用户 | 全部（操作按角色） |
| `/kyc` | 企业认证审核 | super、ops |
| `/orders`、`/ledger`、`/recharges` | 订单 / 流水 / 充值单 | super、finance |
| `/catalog` | 价格与镜像 | super |
| `/admins` | 管理员 | super |
| `/audit` | 审计日志 | 全部 |
| `/settings` | 个人设置 | 全部 |

- 启动时 `GET /me` → `Admin{id, username, role, status, totp_confirmed, last_login_at, last_login_ip, created_at}`；401 跳 `/login`。
- 菜单按 `role` 过滤；守卫对无权限路由渲染 403 页「无权访问该页面」（后端对接口同样拦截，前端隐藏只是体验）。
- 顶栏：品牌名、当前用户名与角色、「个人设置」、「退出」。

### 5.2 登录 `/login`

接口：
- `POST /auth/login {username, password}` → `{next: 'totp' | 'totp_setup', otpauth_uri}` + `sc_admin` Cookie（阶段 password_ok，5 分钟）。错误：`invalid_credentials`（401）、`account_locked`（423，15 分钟）、`rate_limited`（同 IP 每分钟 20 次）。
- `POST /auth/totp/setup {code}` → `{admin, recovery_codes: string[8]}`。错误：`invalid_totp`、`unauthorized`（临时会话过期）、`invalid_state`。
- `POST /auth/totp {code}` 或 `{recovery_code}`（二选一）→ `{admin}`。错误同上。
- `POST /auth/logout {}` → `{ok}`。

行为：
- 步骤一：账号、密码。`next === 'totp_setup'` 进入步骤二，否则步骤三。
- 步骤二（首次绑定）：用 `qrcode` 把 `otpauth_uri` 画成二维码，并显示可复制的密钥（URI 中 `secret` 参数）；输入 6 位动态码；成功后展示 8 个恢复码（格式 `xxxxx-xxxxx`，等宽字体、可一键复制全部），复选框「我已妥善保存这些恢复码」勾选后「进入后台」按钮才可用；进入后台后不再显示。
- 步骤三：动态码输入（自动去空格）；「改用恢复码」切换为恢复码输入（格式 `xxxxx-xxxxx`）。
- 任一步骤收到 401 显示「登录已超时，请重新输入密码」并回到步骤一。
- 注意：同一动态码 30 秒内不能重复使用（后端防重放），文案提示「如刚用过该动态码，请等待下一个」。

### 5.3 仪表盘 `/`

`GET /dashboard` → `{machines: {by_state: {idle, allocated, maintenance, faulty}, online, total}, instances: {creating, running, stopped, wiping, released, failed}, recharge_today, recharge_month, order_today, order_month, utilization: {date, rate}[30]}`。`by_state` 与 `instances` 都是「状态 → 数量」的 map，后端只输出数据库里出现过的状态，缺省键前端按 0 显示。

- 四组卡片：机器（总数、在线、各状态台数）、实例（各状态数量）、今日 / 本月充值金额、今日 / 本月下单金额（`fmtYuan`）。
- 近 30 天出租率柱状图：自绘 SVG，30 根柱，高度 = `rate × 100%`，悬停显示日期与百分比（保留 1 位小数），最后一根为今天（截至当前）。
- 60 秒自动刷新。

### 5.4 机器 `/machines`、`/machines/:id` 与 PDU `/pdus`（super / ops）

接口：
- `GET /machines?state=&online=&rack=&page=&page_size=` → `{items: Machine[], total}`；`Machine{id, serial, rack, slot, lan_ip, product_id, state, agent_online, last_seen_at, pdu_id, pdu_outlet, created_at}`；`state`：`idle` 空闲、`allocated` 已分配、`maintenance` 维护中、`faulty` 故障。
- `POST /machines {serial, rack, slot, lan_ip, product_id, pdu_id?, pdu_outlet?}` → 201 `Machine`。错误：`duplicate_serial`、`pdu_outlet_taken`、`bad_request`（IP 格式、PDU 与插座需同填、插座 1～256、商品 / PDU 不存在）。
- `POST /machines/import {csv}` → 201 `{items: Machine[]}`；失败 400 `{code: 'import_invalid', message, request_id, errors: {line, message}[]}`（整批不导入），或 `bad_request`（表头不对、超过单次上限）。CSV 表头固定 `serial,rack,slot,lan_ip,product_code,pdu_name,pdu_outlet`。
- `GET /machines/{id}` → `{machine, current_instance: AdminInstance | null, history: AdminInstance[]}`。
- `POST /machines/{id}/maintenance {on}` → `Machine`。错误：`invalid_state`（结束维护时机器上还有未结束实例，message 说明）。
- `POST /machines/{id}/power-cycle {}` → `{ok}`。错误：`bad_request`（未配置 PDU）。
- `GET /products`（§6 B6 放开为全部角色可读）用于登记表单的商品下拉与列表中的商品名。
- `GET /pdus` → `{items: PDU[]}`；`PDU{id, name, driver, address, has_community, oid_prefix, cycle_value, on_value, created_at}`。`POST /pdus` / `PUT /pdus/{id}` 请求体 `{name, driver: 'snmp' | 'mock', address, community, oid_prefix, cycle_value, on_value}`。错误：`duplicate_name`、`bad_request`（snmp 必填 community；修改地址必须重新填写 community；地址 / OID 格式）。

行为：
- 机器列表：筛选栏（状态下拉、在线 是 / 否、机柜文本）、分页（每页 20，可选 50 / 100）；列：ID、序列号、机柜 / 槽位、内网 IP、商品、状态标签、在线（绿点 / 灰点 + 最后心跳时间）、PDU（名称 + 插座）；行点击进详情。
- 「登记机器」表单弹窗：序列号、机柜、槽位、内网 IP、商品（下拉）、PDU（下拉，可空）+ 插座号。
- 「CSV 导入」：选择文件后前端读取文本（限制 1MB）直接提交；成功提示导入台数并刷新；`import_invalid` 时弹窗按行列出错误（行号、原因），并提供下载模板按钮（生成表头 CSV）。
- 详情：机器信息、当前实例（链接到实例详情）、历史实例表格；操作：「进入维护」/「结束维护」（确认框）、「断电重启」（确认框文案「将通过 PDU 对该机器断电重启，机器上的实例会中断，确定执行？」，确认即执行）。
- PDU 页：表格 + 新增 / 编辑弹窗。`community` 字段用密码框；编辑时占位文字「留空表示不修改；修改了地址必须重新填写」，`has_community` 为真时显示「已设置」标记。驱动为 mock 时隐藏 SNMP 相关字段。

### 5.5 告警 `/alerts`（全部角色）

- `GET /alerts?resolved=&page=` → `{items: Alert[], total}`；`Alert{id, level: 'warning' | 'critical', kind, machine_id, instance_id, message, created_at, resolved_at, resolved_by}`；`kind` 中文映射：`machine_offline` 机器失联、`create_failed` 开通失败、`start_failed` 启动失败、`stop_failed` 停机失败、`wipe_failed` 洗机失败、`recharge_amount_mismatch` 支付金额不符、`recharge_paid_after_close` 关单后到账；未知原样显示。
- `POST /alerts/{id}/resolve {}` → `{ok}`。错误：`invalid_state`（已处理）。
- 默认 `resolved=false`，切换「全部」显示已处理；级别着色；机器 / 实例 ID 链接到对应详情；「标记已处理」按钮，成功后从未处理列表移除。侧边菜单显示未处理数量角标（60 秒刷新一次 `page_size=1` 取 `total`）。

### 5.6 实例 `/instances`、`/instances/:id`（全部角色查看，操作按角色）

接口：
- `GET /instances?state=&user_id=&machine_id=&page=` → `{items: AdminInstance[], total}`；`AdminInstance{id, user_id, machine_id, product_id, image_id, state, suspended, pending_seconds, remaining_seconds, expires_at, stopped_at, created_at}`。
- `GET /instances/{id}` → `{instance, events: {id, from_state, to_state, reason, at}[], orders: AdminOrder[], commands: {id, machine_id, type, status, result, attempts, created_at}[]}`。
- `GET /instances/{id}/refund-quote` → `{instance_id, remaining_seconds, unit_order_id, suggested, max}`（分；后端对全部角色开放，前端只在退款表单里用）。
- `POST /instances/{id}/extend {seconds, reason}` → `AdminInstance`（ops；60 秒～30 天；`creating / running / stopped`）。
- `POST /instances/{id}/migrate {reason}` → `{instance: AdminInstance}`（ops；仅 running；错误 `no_capacity`、`invalid_state`（无剩余时长））。
- `POST /instances/{id}/rewipe {}` → `{ok}`（ops；仅 wiping）。
- `POST /instances/{id}/refund-release {amount, reason, totp}` → `{refunded}`（finance；`creating / running / stopped`；`bad_request` 金额超上限或为负）。
- `GET /images`、`GET /products`（§6 B6）用于显示镜像名与商品名。

行为：
- 列表筛选：状态下拉、用户 ID、机器 ID；列：ID、用户（链接）、机器（链接，finance 只显示 ID 不可点）、镜像、状态（含「计时已暂停」标记）、剩余 / 待开始时长、到期时间、创建时间。
- 详情：基本信息卡；状态时间线（`events` 竖向时间轴，显示 from → to、原因、时间）；订单表（订单号、类型、租期、金额、已退、状态、时间）；指令表（类型、状态、尝试次数、结果、时间）。
- 操作区按 `me.role`：
  - ops / super：「免费延长」表单（数值 + 单位 小时 / 天 → 换算秒，范围提示 1 分钟～30 天；原因必填）；「换机」确认框（原因必填；成功后提示「已在新机器上创建实例 <新 ID>，SSH 密码已变化，请通知用户」并链接到新实例，认领计划 4 遗留）；「重新洗机」仅 wiping 状态显示。
  - finance / super：「退款并释放」表单：打开时请求 `refund-quote`，金额框预填 `suggested`（元），下方显示「建议 ¥x（按剩余时长 … 折算），最多可退 ¥max」；金额可改但前端限制 0 ≤ 金额 ≤ max；原因必填；提交后由请求层弹动态码；成功提示「已退款 ¥refunded，实例进入洗机」。
  - 按钮只在后端允许的状态显示（延长：creating / running / stopped；换机：running；重新洗机：wiping；退款并释放：creating / running / stopped）。

### 5.7 用户 `/users`、`/users/:id`（全部角色查看，操作按角色）

接口：
- `GET /users?q=&kyc_status=&status=&page=` → `{items: User[], total}`；`q` 为 11 位数字按完整手机号匹配，其他纯数字按用户 ID 或手机尾号匹配，非数字后端返回 `bad_request`（前端只允许数字输入）。
- `GET /users/{id}` → `{user, balance, instances: AdminInstance[]（最近 20）, orders: AdminOrder[]（最近 20）, ledger: LedgerRow[]（最近 20，与用户端同构，**不含** `operator_id`）, kyc: KYCRecord[]}`；`KYCRecord{id, user_id, kind, real_name, id_number_masked（前 3 后 4，仅个人）, license_no, license_file, result, reason, reviewer_id, created_at, updated_at}`，企业记录的 `real_name` 存的是企业名称。
- `POST /users/{id}/ban {reason}`（ops，原因必填）、`POST /users/{id}/unban {}`。错误：`invalid_state`。
- `POST /users/{id}/adjust {amount, remark, totp}` → `{balance}`（finance；`amount` 正负皆可，分）；`POST /users/{id}/manual-recharge {amount, remark, totp}` → `Recharge`（finance；1 元～100 万元；备注必填，例如转账流水号）。错误：`duplicate`（60 秒内相同操作）、`bad_request`。

行为：
- 列表：搜索框（手机号 / 尾号 / ID）、实名状态、账号状态筛选；列：ID、手机号、类型、实名状态、账号状态、注册时间。
- 详情：用户卡（含余额）、四个标签（实例、订单、流水、实名记录；流水表显示类型、金额、变动后余额、备注，操作人只在 §5.9 的流水页显示；身份证号用 `id_number_masked`）。更多数据链接到对应列表页并带 `user_id` 筛选（实例对全部角色；订单 / 流水 / 充值仅 finance / super 可见该链接）。
- 操作：ops / super「封禁」（原因必填）/「解封」；finance / super「调账」（金额元、可负、备注必填、提交前确认框显示「将把余额从 ¥a 调整为 ¥b」）与「手动入账」（金额元、备注必填）。两者提交后由请求层弹动态码。

### 5.8 企业认证审核 `/kyc`（super / ops）

- `GET /kyc/pending?page=` → `{items: KYCRecord[], total}`；`GET /kyc/{id}` → `KYCRecord`；`POST /kyc/{id}/review {approve, reason}` → `{ok}`（驳回时 `reason` 必填，后端 `bad_request`）；`GET /files/{key}` 返回文件（需后台会话，`Content-Disposition: inline`）。
- 列表：企业名称（`real_name`）、信用代码（`license_no`）、用户 ID（链接）、提交时间、「审核」按钮。
- 审核弹窗：左侧资料，右侧执照预览：`content_type` 未知时先用 `<img src="/admin/api/v1/files/<key>">`，`onerror` 时改为按钮「在新窗口打开」；`key` 以 `.pdf` 结尾时直接显示「在新窗口打开 PDF」按钮（`window.open`）。按钮「通过」（确认框）、「驳回」（原因必填）。处理后从列表移除。

### 5.9 订单 / 流水 / 充值单 `/orders`、`/ledger`、`/recharges`（super / finance）

接口：
- `GET /orders?user_id=&kind=&status=&from=&to=&page=`；`AdminOrder{id, user_id, kind, instance_id, product_id, duration, amount, refunded_amount, status, created_at}`；`kind`：`new` / `renew`；`status`：`paid` / `refunded` / `partially_refunded`。
- `GET /ledger?user_id=&kind=&from=&to=&page=`；`AdminLedgerRow{id, user_id, amount, balance_after, kind, ref_type, ref_id, operator_id, remark, created_at}`。
- `GET /recharges?user_id=&channel=&status=&from=&to=&page=`；`AdminRecharge{id, user_id, channel, amount, status, channel_trade_no, operator_id, remark, expires_at, paid_at, closed_at, created_at}`。
- `GET /orders/export`、`/ledger/export`、`/recharges/export`：同筛选参数，返回 `text/csv`（UTF-8 BOM）附件，文件名 `orders-20261017-140501.csv` 形式；超过 10 万行返回 JSON 400 `export_too_large`。
- 日期 `from` / `to` 为 `YYYY-MM-DD`（北京时间，`to` 含当天）；`from` 必须早于 `to`（前端日期范围控件保证）。

行为：三页共用一个「筛选 + 表格 + 分页 + 导出」组件；筛选：用户 ID、类型 / 渠道、状态、日期范围；「导出 CSV」用 `http.download` 按当前筛选条件下载，错误用消息提示；订单 / 充值金额与已退金额 `fmtYuan`；实例 ID、用户 ID 可点击跳转。

### 5.10 价格与镜像 `/catalog`（super）

- `GET /products` → `{items: AdminProduct[]}`；`AdminProduct{id, code, name, spec, status, prices: {duration, amount, enabled, updated_at}[]}`。
- `PUT /products/{id}/prices/{duration} {amount, enabled, totp}` → `AdminPrice`（`duration` ∈ `1d / 1w / 1m`；`amount` 分）。
- `GET /images` → `{items: AdminImage[]}`；`AdminImage{id, name, ref, description, enabled, sort, created_at, updated_at}`；`POST /images {name, ref, description, enabled, sort}` → 201；`PUT /images/{id}` 同体。后端没有删除镜像的接口，界面提供「停用」（`enabled=false`）代替删除。
- 价格区：每个商品一张卡，三档各一行：当前价格（元）、启用开关、「修改」弹窗（金额元 → 分，启用状态）；提交后由请求层弹动态码；保存后刷新。
- 镜像区：表格（名称、仓库地址、描述、启用、排序值）、新增 / 编辑弹窗、排序用「上移 / 下移」按钮交换相邻两项的 `sort` 值（各发一次 `PUT`）；`ref` 必须以 `[a-z0-9]` 开头（后端校验，失败显示 `bad_request` 原文）。

### 5.11 管理员 `/admins`（super）与个人设置 `/settings`

- `GET /admins` → `{items: Admin[]}`；`POST /admins {username, password, role, totp}` → 201（用户名 3～32 位小写字母 / 数字 / `_ . -` 且字母开头；密码 ≥ 12 位；错误 `duplicate_username`）；`PUT /admins/{id} {role?: 'super' | 'ops' | 'finance', status?: 'active' | 'disabled', totp}`（错误 `last_super`，前端只显示后端原文，不做专门处理）；`POST /admins/{id}/reset-totp {totp}` → `{ok}`。
- `POST /me/password {old, new}` → `{ok}`（错误 `wrong_password`、`bad_request` 新密码 < 12 位）。
- 列表：用户名、角色、状态、动态码（已绑定 / 未绑定）、最后登录时间与 IP；操作「修改角色 / 状态」「重置动态码」（确认框说明「该管理员将被强制下线并在下次登录时重新绑定」）；「新增管理员」弹窗（用户名、密码、角色）。所有写操作由请求层弹动态码。
- 个人设置：修改密码（原密码、新密码、确认）、退出登录。

### 5.12 审计日志 `/audit`（全部角色）

- `GET /audit?actor_id=&action=&target_type=&target_id=&from=&to=&page=` → `{items: AuditEntry[], total}`；`AuditEntry{id, actor_type, actor_id, actor_name（§6 B7 新增）, action, target_type, target_id, diff, ip, created_at}`。
- 动作下拉固定列表（与后端写入值一致）：`admin.create`、`admin.update`、`admin.reset_totp`、`admin.login`、`admin.login_recovery_code`、`admin.totp_bind`、`admin.password_change`、`machine.register`、`machine.import`、`machine.maintenance`、`machine.power_cycle`、`pdu.create`、`pdu.update`、`alert.resolve`、`instance.extend`、`instance.migrate`、`instance.rewipe`、`instance.refund_release`、`user.ban`、`user.unban`、`kyc.review`、`wallet.adjust`、`wallet.manual_recharge`、`price.update`、`image.create`、`image.update`；也允许手填。
- 表格：时间、操作人（`actor_name`，空则 `#actor_id`）、动作、对象（类型 + ID，可点击跳转到机器 / 实例 / 用户）、IP；行展开显示 `diff` 的格式化 JSON（`<pre>`，2 空格缩进）。

### 5.13 动态码二次验证（统一）

- 后端返回 `403 totp_required` 时，`http.ts` 弹出全局动态码输入框（单例，Promise），填写后把 `totp` 合并进原请求体自动重发；`invalid_totp` 提示并允许重试；取消则放弃本次操作。5 分钟内再次执行高风险操作后端不会再要求，前端不做预判。
- 涉及接口：`users/{id}/adjust`、`users/{id}/manual-recharge`、`instances/{id}/refund-release`、`products/{id}/prices/{duration}`、`admins`（POST / PUT / reset-totp）。表单本身不含动态码字段。

## 6. 后端配套改动（属于本计划）

逐页对照后，前端需要但后端没有或不合适的点如下。编号用于实施计划引用。

- **B1 静态托管与单页应用回退**：新增 `internal/web`（§3.1），在 `api.Server.Router()` 与 `adminapi.Server.Router()` 中挂载；安全响应头（§8）。测试：Go 单测验证深链接回退、`index.html` no-cache、`assets/` immutable、未知 `/api/*` 仍是 JSON 404、`/admin/` 下同样行为、产物缺失时 503。
- **B2 服务器组装下沉**：把 `cmd/platform/main.go` 里两个 `http.Server` 的创建（地址、超时）移到 `internal/app`：`func (a *App) Servers(cfg config.Config) (public, admin *http.Server)`；`cmd/platform` 与 `cmd/e2e-server` 共用，保证端到端测试跑的是同一套监听与路由。
- **B3 Makefile**：`web-install`、`web`、`web-test`、`e2e`、`check` 目标；`build` 依赖 `web`；`test` 包含 `web-test`（§3.2）。`.gitignore` 增补。
- **B4 用户查询自己的实名记录**：新增 `GET /api/v1/kyc`（需登录）→ `{record: KYCRecord | null}`，取该用户最近一条 `kyc_records`（按 `created_at desc, id desc`）；字段 `id, kind, real_name, license_no, result, reason, created_at, updated_at`，不返回 `id_number_masked` 与 `license_file`；个人记录 `real_name` 只保留首字其余以 `*` 代替。实现复用 `identity.UserKYCRecords` 取第一条并裁剪。现状：`User` 只有 `kyc_status`，没有驳回原因，页面无法展示「驳回原因」。
- **B5 用户端视图补字段**：(a) `api.instanceView` 增加 `product_name`、`image_name`（镜像停用后仍能显示名称）。实现：`catalog` 新增 `ProductName(ctx, id)` 与 `ImageName(ctx, id)`（不受 `enabled` 过滤），处理器内用一个小 map 去重查询。现状：只有 `product_id` / `image_id`，而 `GET /images` 不返回已停用镜像。(b) `api.rechargeView` 增加 `remaining_seconds`（待支付时为 `expires_at − 服务器当前时间`，向下取整且不小于 0；其他状态为 0），由 `s.Clock` 计算。原因：§2.4 规定前端不拿设备时钟与服务器时间戳相减，充值倒计时需要服务器给出的剩余秒数（实例视图已有同类字段）。
- **B6 后台商品 / 镜像读接口放开角色**：`GET /admin/api/v1/products` 与 `GET /admin/api/v1/images` 由 `superOnly` 改为 `allRoles`；写接口（`PUT prices`、`POST/PUT images`）仍 `superOnly`。原因：ops 登记机器需要选商品，所有角色的实例页需要显示镜像 / 商品名。同步更新 `permissions_test.go` 的矩阵。
- **B7 审计日志操作人名称**：`adminapi.handleAudit` 在返回前用 `Admins.List()` 建立 `id → username` 映射，为每条记录补 `actor_name`（`actor_type != 'admin'` 或找不到时为空串）。响应类型为 `adminapi` 内的视图结构，不改 `audit` 包。
- **B8 速率限制可关闭（仅测试）**：`app.Options.DisableRateLimits bool`；为真时 `identity.Deps.CodeIPHourLimit = 1_000_000`（新增字段，零值表示默认 30）、`adminapi.Server.LoginLimit = 1_000_000`（新增字段，零值表示默认 20，覆盖 `/auth/login`、`/auth/totp`、`/auth/totp/setup` 三个接口共用的限流器）。`Options` 是 Go 结构体字段而不是配置项：只有 `cmd/e2e-server` 设置它，`cmd/platform` 传 `app.Options{}`，没有任何环境变量能打开；再加一道保险：`app.Build` 在 `DisableRateLimits && cfg.Env == "prod"` 时返回错误（与「生产环境禁止 mock 插件」同一处检查，Go 单测覆盖）。原因：两个限流器的时间窗都按注入的时钟计算，端到端环境用的是假时钟，窗口永远不会自然滑过——不是「几分钟内触发」，而是累计 30 次验证码 / 20 次后台登录请求后必然全部 429。
- **B9 端到端测试服务 `cmd/e2e-server`**：见 §7.2。独立 `main` 包，不在 `make build` 产物中，钩子监听 127.0.0.1。
- **B10 staging 开启 Secure Cookie**（认领计划 1 遗留）：`api.Server.SecureCookie` 与 `adminapi.Server.SecureCookie` 由 `cfg.Env == "prod"` 改为 `cfg.Env != "dev"`；staging 必须走 HTTPS（部署说明里写明）。
- **B11 计划 4 文档脚注**：`/products`、`/images` 读权限变化记入 `docs/superpowers/specs/2026-10-15-plan4-admin-payments-design.md` 的权限矩阵（一行脚注，不改其他内容）。

明确**不改**的后端行为（前端适配）：用户订单列表固定最近 50 条、实例列表最多 100 条（计划 1 遗留「可以继续延后」保持）；镜像没有删除接口（用停用代替）；充值需要先实名（界面引导）；mock 支付的模拟成功入口 `POST /api/v1/dev/recharges/{id}/pay` 已存在且只在非生产环境注册。

## 7. 测试与完成标准

### 7.1 分层

| 层 | 工具 | 范围 |
|---|---|---|
| 单元 | Vitest + jsdom + @vue/test-utils | `shared` 全部纯函数与 `http.ts`（用 `fetch` 桩）；两个应用里的纯逻辑组件（倒计时、释放确认文案、菜单过滤、金额输入） |
| Go 单测 | `go test` | B1 静态托管、B4 / B5 / B6 / B7 接口、B8 开关、e2e-server 钩子处理器 |
| 端到端 | Playwright 1.64.1 + 本机 Chrome（`channel: 'chrome'`，无头） | §7.5 覆盖表全部行；控制台每条用例在 `console-desktop`（Desktop Chrome，1280×720）与 `console-mobile`（`devices['iPhone 13']`，390×844，触摸、移动 UA）两个 project 各跑一遍；后台在 `admin`（1440×900）跑 |

### 7.2 端到端环境：`cmd/e2e-server`

**启动**（Playwright `webServer` 自动执行 `go run ./cmd/e2e-server`，就绪探测 `GET http://127.0.0.1:18090/healthz`，超时 180 秒，`reuseExistingServer: false`，因为 e2e-server embed 的是启动时的前端产物）：
1. 用 `embedded-postgres` 在临时目录启动 PostgreSQL（随机端口，二进制缓存在 `~/.embedded-postgres-go`，与 `dbtest` 相同），执行迁移。
2. `app.Build` 配置：`Env=dev`、`Domain=spark.test`、`SecretKey` 固定测试密钥、`UploadDir` 临时目录、`SMSDriver=mock`（固定验证码 `000000`）、`PayDriver=mock`（`DevPay` 为真，`qr_code` 为 `mock://pay/<单号>`）、`PDUDriver=mock`；`Options{Clock: clock.NewFake(time.Now()), KYC: &kycMock, DisableRateLimits: true}`。
3. `a.Servers(cfg)` 得到与生产相同的两个服务器，地址改为 `127.0.0.1:18080`（控制台 + 用户 API）与 `127.0.0.1:18081`（后台 + 后台 API）；**不启动**定时清扫器（清扫由钩子触发，避免与假时钟赛跑）。
4. 第三个监听 `127.0.0.1:18090` 提供钩子；全部就绪后打印 `E2E READY`。
5. 收到 SIGTERM 时停止服务器与数据库并删除临时目录。

**钩子**（JSON，只接受来自 127.0.0.1 的连接；路径前缀 `/hooks`）：

| 方法 路径 | 请求 | 响应 | 用途 |
|---|---|---|---|
| `GET /healthz` | | `{ok}` | 就绪探测 |
| `GET /hooks/clock` | | `{now}` | 测试计算动态码用的服务器时间 |
| `POST /hooks/clock/advance` | `{seconds}` | `{now}` | 推进假时钟（只能向前） |
| `POST /hooks/sweep` | `{heartbeat?: bool}`（缺省 `true`） | `{ok}` | 先对全部机器 `Fleet.Heartbeat`（除非 `heartbeat: false`），再执行一次 `Rental.Sweep` + `Billing.Sweep`（到期停机、超时洗机、失联暂停、开通超时、充值关单…）。必须先心跳：`Rental.Sweep` 先做离线检测与暂停、再做到期停机，而到期停机只处理未暂停的实例；推进 24 小时后不心跳直接清扫，机器会被判失联、实例被暂停而不会停机（`internal/app/e2e_test.go` 的 `sweep` helper 就是这样写的）。只有「计时已暂停 / 机器失联」场景传 `heartbeat: false` |
| `POST /hooks/agent` | `{machine_id?, fail?: bool, result?: string}` | `{completed: n}` | 模拟 Agent：对指定机器（缺省全部）心跳，并把全部待处理指令按成功 / 失败回报（计划 2 未做，这是让实例进入 running / stopped / released / failed 的唯一手段） |
| `POST /hooks/machines` | `{count, serial_prefix?}` | `{ids: number[]}` | 通过 `Fleet.Register` 登记 `count` 台 `spark-1` 机器并心跳使其在线 |
| `POST /hooks/users` | `{phone?, verified?: bool, balance?: cents, ledger_rows?: n}` | `{id, phone}` | 创建用户（可直接实名、预置余额、追加 n 条小额调账流水用于分页测试）；登录仍走界面。`ledger_rows` 通过 `Wallet.AdminAdjust` 写入，每条用不同备注 `e2e-seed-<i>`——该方法拒绝 60 秒内完全相同的调账，而假时钟下这 60 秒永远不会过去 |
| `POST /hooks/admins` | `{username, role, bind_totp: bool}` | `{id, password, totp_secret?, recovery_codes?}` | 创建管理员（`Admins.Create`），密码固定 `e2e-password-123456`；`bind_totp` 为真时走命令行同款路径 `Admins.EnrollTOTP` → 按假时钟算码 → `Admins.ConfirmEnrollment`，返回密钥与恢复码 |
| `POST /hooks/kyc` | `{reject: bool}` | `{ok}` | 切换 mock 实名核验为全部不通过 / 通过。`kyc.Mock` 是值类型、`Reject` 是普通字段，不能在运行中改字段（数据竞争）；`e2ehooks` 自带一个 `atomic.Bool` 驱动的 `switchableKYC` 实现 `kyc.Verifier`，通过 `Options.KYC` 注入 |

钩子处理器放在 `internal/e2ehooks`（只被 `cmd/e2e-server` 引用），用 `app.App` 暴露的服务与 `pgxpool` 实现；不复制业务逻辑。`hooks/agent` 用 `Fleet.Heartbeat` + `Fleet.PendingCommands` + `Rental.HandleCommandResult`，与 `internal/app/e2e_test.go` 的 `agent` helper 相同。

**假时钟的影响与规则**：
- 服务端所有「现在」来自假时钟（会话过期、TOTP 时间片、充值单过期、到期判断）；浏览器时间是真实时间，Cookie `Max-Age` 用的是浏览器时间，不受影响。
- TOTP 防重放：同一管理员 30 秒时间片内同一动态码只能用一次。测试里**任何**需要输入动态码的地方（登录、绑定、高风险操作的二次验证）都由 helper `enterTotp()` 先 `clock/advance 30` 再按 `GET /hooks/clock` 的时间计算。
- 时钟全局共享，因此 **Playwright `workers: 1`，`fullyParallel: false`**；每个测试自行登录、自行造数据，不依赖前一个测试留下的状态；测试只假设时间单调向前，不假设绝对时间。
- 需要大幅推进时间的用例（到期 24 小时、关单 2 小时、会话过期 13 小时）之后，其他测试重新登录即可（helper 每个测试都登录）。同一个测试内也要注意：后台会话空闲 2 小时失效（`admin.IdleTimeout`），推进 ≥ 2 小时后后台界面或 `adminApi()` 必须重新登录；用户会话是 30 天绝对有效期，不受这些推进影响。
- 推进 ≥ 5 分钟后，所有没再心跳的机器在下一次清扫时都会被判失联（`OfflineTimeout`）。每个测试只用自己新登记的机器，且 `hooks/sweep` 默认先心跳，所以互不影响；只有失联场景显式传 `heartbeat: false`。

### 7.3 测试规范

- 每个用例标题以覆盖表 ID 开头，例如 `test('C-MKT-05 下单成功跳转实例详情', ...)`。覆盖表的 ID 列表维护在 `web/e2e/coverage.json`（与 §7.5 一一对应，改表必须同步改它）；脚本 `npm run e2e:coverage` 扫描 spec 文件，核对该列表里的 ID 全部出现、没有多余的未登记 ID、没有 `test.skip` / `test.fixme` / `test.only`，否则非零退出（`make e2e` 先跑它）。
- 选择器优先 `getByTestId`（`data-testid` 命名 `页面-元素`，如 `market-rent-btn`），其次 `getByRole` / `getByText`；不使用 CSS 类名定位业务元素（Element Plus 的 `.el-dialog`、`.is-fullscreen` 仅用于响应式断言）。
- 断言用 Playwright 自动等待（`expect(locator).toHaveText` 等），禁止固定 `waitForTimeout`，唯一例外是截图前等动画。
- 轮询类逻辑（实例列表 10 秒、充值 3 秒、空闲台数 30 秒）用 `page.clock.install()` + `page.clock.runFor()` 快进浏览器定时器，不真等。
- 手机 project 的用例与电脑共用同一份 spec，通过 `test.info().project.name` 分支做布局断言；导航 helper 封装「电脑点侧边菜单 / 手机点底部标签」。
- 剪贴板：浏览器上下文授予 `clipboard-read`、`clipboard-write`，用 `page.evaluate(() => navigator.clipboard.readText())` 断言复制结果。
- 下载：`page.waitForEvent('download')`，读取文件内容断言 BOM 与表头。
- 新窗口：`context.waitForEvent('page')` 断言 PDF 预览地址。
- 控制台 spec 里需要后台动作（审核企业认证、封禁用户）时，用 helper `adminApi()`（Playwright `request` 上下文直连 `:18081`，完成登录 + 动态码）调用接口，不通过后台界面。
- 每个测试的手机号由 `uniquePhone()` 生成（`139` + 启动时间戳后 4 位 + 4 位自增），管理员用户名 `e2e_<自增>`，机器序列号 `E2E-<自增>`。
- 失败时自动保存截图、trace（`trace: 'retain-on-failure'`）到 `web/test-results/`。
- 动态码在测试里用 `e2e/helpers/totp.ts` 计算（RFC 6238，HMAC-SHA1，6 位，30 秒；base32 密钥；时间取 `GET /hooks/clock`），该 helper 有 Vitest 用例对照 RFC 6238 附录向量。

### 7.4 造数据手册（供实施照抄）

| 需要 | 做法 |
|---|---|
| 已登录普通用户 | `hooks/users {verified: true, balance: 100000}` → 界面登录（发送验证码 → 填 `000000`） |
| 未实名用户 | `hooks/users {verified: false}` |
| 空闲机器 | `hooks/machines {count: 1}` |
| 运行中实例 | 界面下单（或 API `POST /orders`）→ `hooks/agent` |
| 已到期停机 | 运行中 → `clock/advance 86400` → `hooks/sweep`（默认先心跳；清扫即置为 stopped 并下发停机指令；再 `hooks/agent` 可把停机指令标为完成，非必需） |
| 停机且有待开始续费 | 停机 → 界面续费（不执行 `hooks/agent`，启动指令待处理，`pending_seconds > 0`） |
| 洗机中 | 运行中 → 释放（不执行 `hooks/agent`） |
| 已释放 | 洗机中 → `hooks/agent` |
| 开通失败（并产生 `create_failed` 告警） | 下单 → `hooks/agent {fail: true}` |
| 洗机失败（机器 faulty、实例停在 wiping，产生 `wipe_failed` 告警） | 洗机中 → `hooks/agent {fail: true}` |
| 计时已暂停 | 运行中 → `clock/advance 360`（> 5 分钟且不心跳）→ `hooks/sweep {heartbeat: false}` |
| 机器失联告警 | 同上 |
| 充值到账 | 界面发起 → 点「模拟支付成功」（或 API `POST /dev/recharges/{id}/pay`） |
| 充值关单 | 发起 → `clock/advance 7200` → `hooks/sweep` |
| 待支付单过多 | API 连续创建 5 张 → 界面再发起 |
| 企业认证待审核 | 界面上传 `fixtures/license.png` 并提交 |
| 实名未通过 | `hooks/kyc {reject: true}` → 提交个人实名 → 用后恢复 `{reject: false}` |
| 管理员（已绑定动态码） | `hooks/admins {username, role, bind_totp: true}` → 界面登录：密码 → `enterTotp()`（先推进 30 秒再用 `totp_secret` 算码） |
| 管理员首次绑定 | `hooks/admins {bind_totp: false}` |
| 后台会话过期 | 登录后 `clock/advance 46800`（13 小时） |
| 登录锁定 | 错误密码 5 次 |
| 退款可退额度 | 运行中实例（订单 ¥69.00）→ `refund-quote` |
| 换机无空闲机器 | 只登记 1 台机器并下单 |
| 流水分页 | `hooks/users {ledger_rows: 25}` |

### 7.5 覆盖表（完成判定依据）

控制台用例（C-）在 `console-desktop` 与 `console-mobile` 都执行；后台用例（A-）在 `admin` 执行；U- 为 Vitest。

**用户控制台**

| ID | 功能 | 断言要点 |
|---|---|---|
| C-NAV-01 | 响应式布局 | 电脑：侧边菜单可见、底部标签不存在；手机：底部四个标签可见、侧边菜单不存在 |
| C-NAV-02 | 手机弹窗样式 | 手机：释放确认框带 `sheet` 类且不全屏；续费表单弹窗 `is-fullscreen`。电脑：两者都居中、不全屏 |
| C-NAV-03 | 404 页 | 访问 `/nope` 显示 404 页并可返回市场 |
| C-AUTH-01 | 未登录重定向 | 访问 `/instances` → `/login?redirect=/instances`；登录后回到 `/instances` |
| C-AUTH-02 | 发送验证码 | 手机号格式错误显示字段错误且不发请求；合法号点击后按钮禁用并显示 60 秒倒计时（`page.clock` 快进后恢复） |
| C-AUTH-03 | 验证码错误 | 填 `111111` → 提示「验证码错误或已过期」，仍在登录页 |
| C-AUTH-04 | 登录与自动注册、会话保持 | 新手机号登录成功进入市场；刷新页面仍登录（顶栏显示打码手机号） |
| C-AUTH-05 | 退出登录 | 「我的」→ 退出 → 回到登录页；再访问 `/wallet` 被重定向 |
| C-AUTH-06 | 会话失效处理 | `context.clearCookies()` 后在钱包页点刷新 → 跳转登录页并提示「请先登录」 |
| C-AUTH-07 | 封禁用户 | 被后台封禁的用户登录 → 提示「账号已被封禁」；已登录用户被封禁后下一次请求跳登录并提示 |
| C-MKT-01 | 市场展示 | 未登录可见；卡片含 GPU / 128 GB / 4 TB / CPU、空闲台数、`¥69.00` `¥414.00` `¥1,599.00` |
| C-MKT-02 | 未登录租用 | 点「立即租用」→ 登录页；登录后回到市场 |
| C-MKT-03 | 未实名租用 | 弹「请先完成实名认证」→ 「去实名」到 `/kyc?redirect=/market` |
| C-MKT-04 | 余额不足 | 余额 ¥10 → 确认框显示余额不足，按钮为「去充值」→ `/wallet/recharge` |
| C-MKT-05 | 下单成功 | 确认框显示价格 / 余额 / 扣后余额 → 确认 → 跳 `/instances/ins-…`，状态「开通中」；钱包余额减少 ¥69.00 |
| C-MKT-06 | 无空闲机器 | 不登记机器 → 按钮禁用「暂无空闲」；登记后 30 秒刷新（`page.clock`）按钮可用 |
| C-MKT-07 | 幂等 | 确认按钮连点两次 → 只有 1 个实例、余额只扣一次 |
| C-MKT-08 | 租期 / 镜像选择 | 选 1 周 + 第二个镜像 → 确认框价格 ¥414.00、镜像名正确；下单后详情页镜像名一致 |
| C-INS-01 | 六种状态展示 | 构造 creating / running / stopped / wiping / released / failed 各一个 → 六个都显示，标签文案与颜色类正确，按创建时间倒序 |
| C-INS-02 | 自动刷新 | 开通中实例 → `hooks/agent` → `page.clock.runFor(10000)` → 无需刷新变为「运行中」 |
| C-INS-03 | 计时已暂停 | 失联暂停后列表与详情显示「计时已暂停」 |
| C-INS-04 | 倒计时与保留时间 | 列表与详情：运行中显示「剩余 23 小时 59 分」量级并每秒递减；停机显示「数据保留至 yyyy-MM-dd HH:mm」且与 `retain_until` 换算成北京时间一致 |
| C-DET-01 | SSH 命令复制 | 显示 `ssh ins-…@ssh.spark.test`，点复制后剪贴板一致；非运行中显示提示 |
| C-DET-02 | 密码查看 | 默认 `••••••••`；点显示后发出 `/password` 请求并显示 16 位密码；复制一致；再次进入页面仍默认隐藏 |
| C-DET-03 | 续费成功 | 运行中选 1 周 → 价格 ¥414.00 → 确认 → 剩余时长约 8 天、余额减少、订单 tab 出现续费订单 |
| C-DET-04 | 停机续费 | 停机实例续费 → 提示「正在重新启动」→ `hooks/agent` 后变运行中 |
| C-DET-05 | 续费余额不足 | 按钮变「去充值」 |
| C-DET-06 | 释放确认 | 文案含「数据将被清除，已付费用不退」；取消后状态不变且无请求发出；确认后状态洗机中；洗机中无续费 / 释放按钮 |
| C-DET-07 | 释放作废待开始时长 | 停机 + 待开始续费 → 确认框额外显示「这部分时长也会作废」及时长 |
| C-DET-08 | 开通失败展示 | 失败实例显示「费用已原路退回余额」，余额恢复，无操作按钮 |
| C-DET-09 | 按钮与状态匹配 | creating 有续费无释放；running / stopped 两者都有；wiping / released / failed 都无 |
| C-WAL-01 | 余额与流水 | 余额 `¥1,000.00`；流水显示类型中文、正负着色、变动后余额；25 条时滚动到底自动加载第二页 |
| C-WAL-02 | 订单 tab | 新购 / 续费、租期、金额、状态；实例 ID 可点击 |
| C-WAL-03 | 充值记录 tab | 待支付 / 已支付 / 已关闭状态；待支付可点进继续支付；`?tab=recharges` 刷新保持 |
| C-WAL-04 | 未实名充值 | 点充值 → 引导实名 |
| C-RCH-01 | 发起页校验 | 档位选择；自定义 `0.5`、`1000001`、`1.234` 报错；`88.5` 通过 |
| C-RCH-02 | 渠道按设备 | 电脑 project：微信 + 支付宝，且把视口缩到 390 宽后仍是两个渠道（证明按 UA 不按宽度）；手机 project：只有支付宝 |
| C-RCH-03 | 支付态与模拟到账 | 两个 project 都执行：倒计时从 15:00 递减、3 秒轮询（`page.clock`）、「开发环境模拟支付」与「模拟支付成功」按钮可见；点按钮 → 「充值成功」→ 余额更新。电脑 project 另断言二维码画布可见、无 `alipays://` 按钮；手机 project 断言无二维码画布 |
| C-RCH-04 | 手机拉起支付宝 | 手机 project：按钮 `href` 以 `alipays://platformapi/startapp?saId=10000007&qrcode=` 开头并含编码后的 `qr_code`，「在浏览器中打开」链接指向 `qr_code` 原文；用 API 模拟支付后触发 `visibilitychange` → 立即查询并显示成功。电脑 project：页面不可见时轮询暂停、恢复可见时立即查一次（同一 `visibilitychange` 逻辑） |
| C-RCH-05 | 关单 | 推进 2 小时 + 清扫 → 页面显示「已关闭，请重新发起」 |
| C-RCH-06 | 待支付过多 | 第 6 张提示「待支付的充值单过多…」 |
| C-RCH-07 | 已支付单重进 | 访问已支付的 `/wallet/recharge/:id` 直接显示成功页 |
| C-KYC-01 | 个人实名成功 | 提交 → 状态条「已认证」；`redirect` 回市场 |
| C-KYC-02 | 个人实名失败 | mock 拒绝 → 红框显示「实名核验未通过：…」；状态「未通过」 |
| C-KYC-03 | 字段校验 | 身份证 17 位报错；信用代码格式错误报错；未上传执照不能提交 |
| C-KYC-04 | 企业提交与通过 | 上传 PNG 显示缩略图 → 提交 → 「审核中」；后台（API）通过 → 刷新显示「已认证」 |
| C-KYC-05 | 企业驳回 | 后台驳回原因「执照不清晰」→ 状态条显示原因，可重新提交 |
| C-KYC-06 | 上传限制 | `bad.gif` → 「仅支持 JPG、PNG、PDF 文件」；PDF 上传成功显示 PDF 图标 |
| C-ACC-01 | 我的页 | 显示手机号、实名状态链接、退出 |
| C-ACC-02 | 帮助页 | 未登录可访问；含 SSH / 数据保留 / 计费三节标题 |
| C-ERR-01 | 服务器错误提示 | `page.route` 把 `/api/v1/wallet` 改为 500 JSON → 提示含「请求号」 |
| C-ERR-02 | 网络错误提示 | `page.route` abort → 提示「网络异常」 |

**管理后台**

| ID | 功能 | 断言要点 |
|---|---|---|
| A-AUTH-01 | 首次登录绑定动态码 | 密码 → 二维码与密钥 → 用密钥算码 → 8 个 `xxxxx-xxxxx` 恢复码、「复制全部」后剪贴板一致 → 未勾选时按钮禁用 → 勾选进入仪表盘；刷新后不再显示恢复码 |
| A-AUTH-02 | 动态码登录 | 密码 → 动态码 → 仪表盘；刷新仍登录 |
| A-AUTH-03 | 恢复码登录 | 切换恢复码 → 成功；同一恢复码再次使用失败 |
| A-AUTH-04 | 错误提示 | 密码错误「用户名或密码错误」；动态码错误「动态码错误或已使用过」 |
| A-AUTH-05 | 锁定 | 5 次错误 → 「登录失败次数过多…」 |
| A-AUTH-06 | 未登录与退出 | 未登录访问 `/admin/machines` → 登录页；退出后同样 |
| A-AUTH-07 | 临时会话超时 | 密码通过后推进 6 分钟再提交动态码 → 回到步骤一并提示 |
| A-AUTH-08 | 会话过期 | 推进 13 小时后任意请求 → 跳登录 |
| A-NAV-01 | 菜单按角色 | super 13 项；ops 无订单 / 流水 / 充值 / 价格镜像 / 管理员；finance 无机器 / PDU / 企业认证 / 价格镜像 / 管理员 |
| A-NAV-02 | 无权限路由 | finance 直接访问 `/admin/machines` → 403 页 |
| A-DASH-01 | 仪表盘 | 数字与 `GET /dashboard` 一致；金额 `fmtYuan`；30 根柱 |
| A-MCH-01 | 机器列表与筛选 | 状态 / 在线 / 机柜筛选结果正确；分页 total |
| A-MCH-02 | 单台登记 | 成功出现在列表；重复序列号提示「序列号已存在」；PDU 与插座只填一个提示 |
| A-MCH-03 | CSV 导入 | `machines-ok.csv` 导入 3 台；`machines-bad.csv` 弹出逐行错误且列表无新增；模板下载表头正确 |
| A-MCH-04 | 维护 | 进入维护 → 状态「维护中」，市场空闲数减 1；有实例时结束维护提示错误；无实例时结束成功 |
| A-MCH-05 | 断电重启 | 确认框文案正确，取消不发请求；无 PDU 机器提示错误；有 mock PDU 机器成功并出现审计 `machine.power_cycle` |
| A-MCH-06 | 机器详情 | 当前实例与历史实例显示 |
| A-PDU-01 | 新增 PDU | mock 成功；snmp 缺 community 报错 |
| A-PDU-02 | 编辑 PDU | community 为密码框且显示提示「留空表示不修改；修改了地址必须重新填写」与「已设置」标记；留空保存成功（`has_community` 仍为真）；改地址不填 community 报错 |
| A-ALT-01 | 告警 | 开通失败产生告警显示在未处理；标记已处理后消失；「全部」可见；菜单角标数字 |
| A-INS-01 | 实例列表筛选 | 状态 / 用户 ID / 机器 ID |
| A-INS-02 | 实例详情 | 基本信息、时间线含 `creating → running`、订单、指令 |
| A-INS-03 | 免费延长 | 2 天 + 原因 → 剩余时长 +48 小时；审计出现 `instance.extend`；范围外（31 天）前端拦截 |
| A-INS-04 | 换机 | 有空闲机器：提示新实例 ID 与密码变化，原实例洗机中；无空闲机器：「暂无空闲机器」 |
| A-INS-05 | 重新洗机 | 洗机失败的实例显示按钮 → 执行后指令表新增 `wipe_instance` |
| A-INS-06 | 退款并释放 | 建议金额预填（≤ ¥69.00）、上限提示；超上限报错；成功弹动态码 → 「已退款」；用户余额增加；实例洗机中 |
| A-INS-07 | 动态码窗口 | 连续两次高风险操作只弹一次动态码框 |
| A-INS-08 | 动态码取消与错误 | 取消 → 操作未执行；错误码 → 提示并可重试 |
| A-INS-09 | 操作按角色 | finance 看不到延长 / 换机 / 重新洗机；ops 看不到退款 |
| A-USR-01 | 用户搜索 | 完整手机号、尾号 4 位、用户 ID 都命中；字母输入被拦截 |
| A-USR-02 | 用户详情 | 余额、实例、订单、流水、实名记录（身份证 `110***…1234` 形式脱敏） |
| A-USR-03 | 封禁 / 解封 | 原因必填；封禁后状态「已封禁」；解封恢复 |
| A-USR-04 | 调账 | +¥10 与 −¥5（动态码）→ 余额变化、流水 `admin_adjust` |
| A-USR-05 | 手动入账 | ¥100 + 备注 → 充值单渠道「手动」、余额增加 |
| A-USR-06 | 重复提交拦截 | 60 秒内相同调账 → 「60 秒内已提交过相同的操作」 |
| A-KYC-01 | 待审核与预览 | 列表显示；PNG 预览 `<img>` 加载成功；PDF 记录点击在新窗口打开 `/admin/api/v1/files/…` |
| A-KYC-02 | 通过 / 驳回 | 通过 → 用户 `verified`；驳回不填原因不能提交；填原因 → `rejected` |
| A-FIN-01 | 订单表 | 筛选用户 / 类型 / 状态 / 日期；分页 |
| A-FIN-02 | 流水表 | 筛选用户 / 类型 / 日期 |
| A-FIN-03 | 充值表 | 筛选用户 / 渠道 / 状态 / 日期 |
| A-FIN-04 | 导出 CSV | 三张表各下载一次：文件名前缀正确、内容以 BOM 开头、表头为中文列名、行数与筛选一致 |
| A-CAT-01 | 修改价格 | 1 天改为 ¥79 需动态码 → 列表显示 ¥79.00；控制台 `/products` 返回 7900 |
| A-CAT-02 | 下架档位 | 关闭 1 周 → 市场不再显示周价 |
| A-CAT-03 | 镜像管理 | 新增、编辑、停用、上移 / 下移 → 市场镜像列表与顺序一致；非法 `ref` 报错 |
| A-ADM-01 | 新增管理员 | 需动态码；重复用户名提示；新管理员可登录并进入绑定流程 |
| A-ADM-02 | 修改角色 / 停用 | 改为 finance 后其菜单变化；停用后其登录失败（`last_super` 由后端单测覆盖，界面只显示原文，见评审裁定 4） |
| A-ADM-03 | 重置他人动态码 | 目标管理员下次登录进入绑定步骤 |
| A-SET-01 | 修改密码 | 原密码错误提示；成功后用新密码登录 |
| A-AUD-01 | 审计日志 | 按操作人 / 动作 / 对象 / 时间筛选；操作人显示用户名；展开 diff 为格式化 JSON |
| A-ERR-01 | 权限错误提示 | super 会话打开机器页时被另一位 super 降为 finance → 点查询得到真实 403 → 页面提示「无权执行该操作」而不是白屏；刷新后进入 403 页，菜单按新角色过滤 |

**单元测试（Vitest）**

| ID | 内容 |
|---|---|
| U-01 | `fmtYuan`：0、6900、159900、-1000、1 → `¥0.00`、`¥69.00`、`¥1,599.00`、`-¥10.00`、`¥0.01` |
| U-02 | `yuanToCents`：`"88.5"`→8850、`"1.234"`→null、`"-1"`→null、`"abc"`→null、`"1000000"`→100000000 |
| U-03 | `fmtTime` / `fmtDate` 固定 ISO 时间转北京时间（跨日边界） |
| U-04 | `fmtDuration`：0、59、3600、86400+3661 |
| U-05 | `useCountdown` 以注入时间源递减并在归零触发回调 |
| U-06 | `http`：401 调 `onUnauthorized`；错误体解析；非 JSON 响应映射为 `network`；查询参数空值剔除 |
| U-07 | `http`：`totp_required` → `promptTotp` → 重发时请求体含 `totp`；并发两个请求只弹一次；取消抛 `cancelled`；`invalid_totp` 重试上限 |
| U-08 | `useIdempotencyKey`：重试复用、成功后重新生成、参数变化重新生成 |
| U-09 | `isMobileUA` 对 iPhone / Android / 桌面 UA 的判断 |
| U-10 | `messageOf`：5xx 追加请求号；未知错误默认文案 |
| U-11 | `releaseWarning(instance)`：仅 stopped 且 pending_seconds > 0 时返回追加文案 |
| U-12 | 后台 `menuFor(role)` 与路由 `roles` 一致（表驱动，三种角色） |
| U-13 | `totp.ts`（e2e helper）对照 RFC 6238 向量 |
| U-14 | `download` 处理：JSON 响应按错误抛出；CSV 响应取文件名 |

### 7.6 完成判定

1. `make check` 通过：`go vet`、`go test ./...`（含 B1～B9 的 Go 单测）、`vue-tsc`、Vitest、`e2e:coverage` 核对、Playwright 三个 project 全绿。
2. 覆盖表每一行在 spec 中出现且无跳过；新增功能必须先在覆盖表加行再实现。
3. 控制台每个用例在电脑与手机两个 project 都通过，不允许只在其中一个 project 运行（除 C-RCH-02 / C-RCH-04 这类按设备分支的用例，也仍在两个 project 执行并各自断言）。
4. 实施计划中每个任务的验收都引用覆盖表 ID；任务完成的证据是对应用例通过的输出，而不是「看起来能用」。

## 8. 安全

- **会话**：只用后端下发的 HttpOnly Cookie；前端不保存任何 token。`localStorage` 只存界面偏好（后台侧边栏折叠），不存手机号、余额、密码、动态码、恢复码。
- **敏感数据**：SSH 密码只在用户点击时请求并显示在内存中，离开页面即丢弃；恢复码只展示一次；身份证号输入框 `autocomplete="off"`；后台所有 API 响应本身就是 `no-store`。
- **CSP**（静态托管响应头，B1）：`default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; font-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; object-src 'none'; base-uri 'self'; form-action 'self'`。`style-src 'unsafe-inline'` 是 Element Plus 行内样式所需；不引入任何第三方 CDN、字体或统计脚本；二维码本地生成。
- **CSRF**：后端已要求写请求 `Content-Type: application/json` 且 Cookie `SameSite=Lax`（用户）/ `Strict`（后台）；前端不使用表单提交。
- **XSS**：不用 `v-html`；审计 `diff`、告警消息、备注都以文本渲染。
- **重定向**：`redirect` 参数只接受站内相对路径。
- **后台暴露面**：后台 SPA 只在 `:8081` 托管，与 API 同样只监听内网；控制台构建产物不包含后台代码（独立应用）。
- **开发钩子**：模拟支付按钮只在 `qr_code` 为 `mock://` 时出现，生产的真实渠道不会返回该前缀，且生产不注册 `/dev/` 路由；`cmd/e2e-server` 不随 `make build` 发布，钩子只监听 127.0.0.1。
- **staging** Secure Cookie（B10）。

## 9. 开发流程

1. 首次：`make web-install`（Node 22 / npm 10：`export PATH=$HOME/.local/bin:$PATH`；Go 1.27：`export PATH=$HOME/.local/go/bin:$PATH`）。
2. `make devdb`（终端 1）、`make run`（终端 2，后端 :8080 / :8081，同时托管上次 `make web` 的产物）。
3. 前端开发：`cd web && npm run dev:console`（http://127.0.0.1:5173，`/api` 代理到 8080）或 `npm run dev:admin`（http://127.0.0.1:5174/admin/，`/admin/api` 代理到 8081）。热更新保留表单状态。
4. 本地管理员：`SPARK_DATABASE_URL=… SPARK_SECRET_KEY=… go run ./cmd/platform admin create --username root`（终端二维码绑定动态码）。本地用户：登录页任意手机号 + `000000`。
5. 提交前：`make test`；改动涉及界面流程时 `make e2e`（首次会下载 embedded-postgres 二进制并缓存；全量约 10～15 分钟，可用 `npx playwright test --project=admin` 或 `-g 'C-RCH'` 只跑子集）。
6. 调试端到端：`npx playwright test --headed --project=console-mobile -g 'C-RCH-04'`；失败的 trace 用 `npx playwright show-trace`。
7. 发布：`make build` → 产物 `bin/platform-linux-{amd64,arm64}` 内含前端。

## 10. 本计划认领的遗留事项

- 计划 1「释放停机且有待开始续费的实例需二次确认」：§4.4 释放确认框追加提示（C-DET-07）。
- 计划 1「前端下单与续费应发送 `Idempotency-Key`」：§2.3（C-MKT-07）。
- 计划 1「staging 走 HTTPS 时也应开启 Secure Cookie」：B10。
- 计划 4「换机后新实例的 SSH 密码与原实例不同，需要在前端提示用户」：后台换机成功提示运营通知用户（A-INS-04），帮助页说明（C-ACC-02）。
- 计划 1「各接口 401 表格测试」：A-AUTH-06 / C-AUTH-06 覆盖界面侧；后端表格测试仍留在遗留清单。

## 11. 风险与已知限制

- `go vet ./...` 会遍历 `web/` 目录；`node_modules` 中极少数包可能带 `.go` 文件导致编译错误。若发生，在 `web/` 放一个空的 `go.mod`（`module spark-cloud/web`）把它从主模块排除。
- 端到端串行执行（`workers: 1`）是假时钟共享的代价；用例总数约 52 × 2（控制台两种尺寸）+ 50（后台）≈ 155，目标 15 分钟内。若超时，优先把「造数据」从界面改为 API 调用，而不是并行化。
- 手机端 `alipays://` 链接的实际拉起只能在真机验证；自动化只断言链接格式与回页查询逻辑。上线前用真机走一次支付宝沙箱。
- 后台 `GET /files/{key}` 不返回 `content_type` 给列表，PNG / JPG 用 `<img>` 预览、失败回退到新窗口打开；PDF 按 key 后缀判断（上传时后端按文件内容生成 `.jpg / .png / .pdf` 后缀，所以后缀可信）。
- 后台 `GET /users/{id}` 的流水不含操作人；需要看操作人时去流水页按用户 ID 筛选（finance / super）。
- 用户实例列表上限 100 条、订单上限 50 条不分页，个人用户在 MVP 阶段足够；企业大客户前需要后端加分页。
- `make e2e` 依赖本机已安装 Chrome；CI 机器需预装 Chrome 或改 `channel` 为 Playwright 自带 chromium（配置项 `E2E_BROWSER_CHANNEL`，默认 `chrome`）。

## 12. 评审裁定

独立评审逐条对照 `internal/api`、`internal/adminapi`、`internal/app`、`internal/admin`、`internal/rental`、`internal/billing`、`internal/wallet`、`internal/fleet`、`internal/identity`、`internal/storage` 与 `migrations/` 核对后的结论。小修（错误码、字段、钩子语义）已直接改入正文；以下是涉及取舍的裁定。

1. **去掉实例列表的「隐藏已释放 / 失败」开关** — 用户确认的设计是「每个实例一张卡片，显示状态标签（…已释放 / 开通失败）」，默认隐藏会让刚释放的用户找不到实例；列表上限 100 条，MVP 不需要筛选。少一个 `localStorage` 偏好、少一条断言分支。
2. **释放与断电重启只保留一层确认框，去掉「按钮二次点击、3 秒内有效」机制** — 用户确认的是「要二次确认」（即确认框），原文已叠加成按钮 → 确认框 → 再点一次的三层确认，属于过度设计且 3 秒窗口会让端到端用例变脆。确认框文案和「停机且有待开始续费」的额外警告原样保留（C-DET-06 / C-DET-07）。
3. **`hooks/sweep` 改为默认先对全部机器心跳** — 对照 `rental.Sweep` 的顺序（离线检测 → 暂停 → 到期停机，且到期停机只处理未暂停实例）与 `internal/app/e2e_test.go` 的 `sweep` helper：原文「推进 24 小时 → 清扫」在不心跳的情况下根本到不了 stopped 状态，C-DET-04 / C-DET-07 / C-INS-04 等用例会全部失败。失联场景显式传 `heartbeat: false`。
4. **A-ADM-02 不再在界面上验证 `last_super`** — 端到端数据库在整轮测试里共享，之前用例创建的 super 都还在，「最后一个 super」条件根本不成立；要成立得先停用其他所有 super，副作用波及后续用例。该规则已有 `internal/admin` 单测，界面只显示后端原文。
5. **B8 保留，并加 `app.Build` 的生产保护** — 评审确认两个限流器（`identity` 的验证码按 IP 每小时 30 次、`adminapi` 的登录每分钟 20 次）都按注入的时钟计窗口，假时钟下窗口永不滑过，所以不是「可有可无的加速」而是端到端跑通的前提。`Options` 字段无法由配置或环境变量打开，再加 `DisableRateLimits && Env == "prod"` 直接拒绝启动，与现有「生产禁止 mock 插件」检查同一处。
6. **B6（`GET /products`、`GET /images` 对全部后台角色开放）合理** — 后台 SPA 在 `:8081`，无法同源访问 `:8080` 的公开商品接口；两个接口只读、不含敏感数据（`ref` 是内网仓库地址）；写接口仍 `superOnly`，`permissions_test.go` 矩阵同步。
7. **`hooks/users.ledger_rows`、`hooks/kyc`、`hooks/admins` 的实现约束写死在钩子表里** — 分别因为 `AdminAdjust` 的 60 秒重复检查在假时钟下永不过期、`kyc.Mock` 是值类型无法运行中切换、服务端绑定动态码必须走 `EnrollTOTP` / `ConfirmEnrollment`。不写清楚实施时一定会踩到。
8. **C-RCH-02 在电脑 project 增加「缩窄视口仍显示两个渠道」断言** — 用户明确要求「是不是手机按设备类型判断，不看屏幕宽度」，而两个 project 同时改变了 UA 和宽度，原覆盖表区分不出来是哪个在起作用；加一行断言即可直接验证这条要求。
9. **C-RCH-03 / C-RCH-04 重写为两个 project 都有明确断言** — 原文标题带「电脑」「手机」，与 §7.6「每个用例在两种尺寸都通过」自相矛盾；现在手机 project 也验证「模拟支付成功」按钮（用户确认稿第二段第 6 条），电脑 project 也验证页面可见性暂停 / 恢复轮询。
10. **`Makefile` 的 `e2e` 目标先执行 `web`** — `cmd/e2e-server` 与生产一样从 embed 读前端产物，不先构建就是在测上一次的前端；同时把 `e2e:coverage` 核对放进该目标。
11. **去掉恢复码「下载为 txt」** — 用户确认稿只要求展示并勾选「已保存」；复制全部已够用，少一个文件下载路径和一条断言。
12. **纠正与代码不符的细节（已改正文）**：CSV 导出超限错误码是 `export_too_large` 不是 `bad_request`；后台用户详情的 `ledger` 是 `wallet.LedgerRow`（无 `operator_id`）；`retain_until` 仅在 `pending_seconds == 0` 时返回；`by_state` / `instances` 是只含出现过状态的 map；企业实名记录的企业名称在 `real_name`；`PUT /admins/{id}` 的 `status` 取值 `active | disabled`；`/me` 探测必须 `quiet` 否则匿名访客打开市场页会被弹到登录页；后台会话有 2 小时空闲失效，推进时钟超过 2 小时的用例要重新登录。
13. **未改动但确认过的点**：`vue-router 5.4.0`、`vite 8.3.4`、`element-plus 2.14.7`、`@playwright/test 1.64.1` 等版本与 spike 实际安装的 `package.json` 一致；本机已装 Chrome（`/Applications/Google Chrome.app`）、Node 22、embedded-postgres 二进制已缓存，`make e2e` 不需要 Docker；用户端订单 50 条 / 实例 100 条上限、`invalid_code` 400、`banned` 403、`insufficient_balance` 402、`no_capacity` 409、`too_many_pending` 409、`pay_unavailable` 502、`file_too_large` 413、`unsupported_file_type` 415、`kyc_rejected` 400、`account_locked` 423、全部审计动作名与告警 `kind` 均与代码一致。
