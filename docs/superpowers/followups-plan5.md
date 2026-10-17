# 计划 5 遗留事项

来源：计划 5 实施过程中的裁定（2026-10-17 起）。均不阻塞合并。

## 上线前需要核对
- 手机端 `alipays://` 拉起只能真机验证；上线前用真机走一次支付宝沙箱（自动化只断言链接格式与回页查询）。
- 品牌占位 `Spark Cloud`：替换 `web/shared/src/brand.ts` 与两个应用 `public/brand/logo.svg`。
- CI 机器需预装 Chrome，或设 `E2E_BROWSER_CHANNEL=chromium`。
- 静态处理器拒绝任何以 `.` 开头的路径段（连 `/.well-known/` 也回退 index.html）：ACME HTTP-01 等需要 `/.well-known/` 时在反向代理处理（当前 TLS 在代理终止，无影响）。
- 待负责人决定：模拟支付成功接口 `POST /api/v1/dev/recharges/{id}/pay` 在 `SPARK_PAY_DRIVER=mock` 且非 prod 的所有环境都会注册（`internal/app/app.go:143` `devPay = cfg.Env != "prod"`，路由见 `internal/api/server.go:94`），因此 staging 用 mock 支付时同样对外暴露；是否收紧为仅 dev 尚未决定。

## 可以继续延后
- 后台收到 `403 totp_setup_required` 时回到登录页重新输入密码，而不是直接进入绑定步骤（临时会话由密码步骤签发）。
- 用户实例列表上限 100 条、订单 50 条不分页；企业大客户前需要后端分页。
- 后台用户详情的流水不含操作人；需要看操作人时到流水页按用户 ID 筛选。
- 控制台与后台的 `safeRedirect` 各一份实现（避免 shared 依赖路由）；如再增加应用可上移到 shared。

## 实施中确定的产品行为（裁定）
- 后台镜像的「删除」实际是停用（带确认）：后端没有 DELETE 接口，且已有实例引用镜像；确需硬删除时再加接口。
- 商品「下架」按时长档进行（停用该档价格），没有商品级的上下架状态，与后端模型一致。
- 「退款并释放」金额填 0 表示只释放、不退款（界面有提示与确认文案）；可退上限为 0 时这是唯一的释放方式。
- 后台登出请求失败时保持登录并给出警告，不假装已经退出。
- 后台告警角标刷新失败时静默，不打扰用户（401 仍按会话过期统一处理）。
- 调账：扣减后余额会变负时前端直接拦截、不进入确认步骤；若后端因余额已变化仍拒绝，提示并刷新余额。
- 手动入账提交前多一步确认（用户、金额、入账前后余额），再由请求层要求动态码。
- A-AUTH-08（后台会话过期）改为覆盖真实的 401 路径：设置页提交与告警角标定时刷新；原「点菜单触发 401」因骨架页不发请求而删去。
- 前端产物用 `//go:embed all:dist` 嵌入（否则 Vite 以 `_` 开头的分包会被丢掉），静态处理器拒绝任何以 `.` 开头的路径段（回退到 index.html）。
- 控制台登录验证码输入框不设 `maxlength`，粘贴「000 000」这类带空格或全角的验证码也能用；控制台请求失败提示统一经 `notifyError`，`unauthorized`（401）与 `cancelled` 静默。
- Playwright `webServer` 配置了 `gracefulShutdown`（SIGTERM，20 秒），让 e2e-server 有时间停掉嵌入式 PostgreSQL；改回默认（SIGKILL）会留下孤儿 postgres 占用 SysV 共享内存（macOS 只有 32 段），积累后新的嵌入式数据库无法启动。

## 评审中延后的小问题

### 共用层（web/shared）
- 二次验证完成、`stepUp` 清空后，并发请求里迟到的 `totp_required` 会再弹一次动态码框（输入后也能成功）。
- 首个二次验证失败（非取消）后，等待中的请求拿到的是原始 `totp_required` 而不是首个请求的错误。
- `upload()` 对非 JSON 的 2xx 响应抛 `SyntaxError`（不是 `ApiError`，提示为「操作失败，请稍后重试」）；后端永远返回 JSON。
- 调用方主动 abort 的请求被映射为「网络异常」。
- 缺少的单测：首个被取消时等待者一起取消、`upload()` 401 经 `handleAuth`。
- `useCountdown` 在系统时钟回拨时不做钳制；`fmtYuan(-0.5)` 显示为 `-¥0.00`。
- 单测卫生：`vi.useRealTimers` 没放进 `afterEach`；`useMobile` 无单测；`idempotency.spec` 末行是同义反复（计划原文）。

### 控制台
- 启动时探测会话把 5xx / 网络错误当作未登录，已登录用户会被送到 `/login`，且本页会话状态固定为未登录直到整页刷新。
- 实例详情首次加载遇到 401 时，页内错误条会一闪显示「请先登录」（随即跳转登录页）。
- 实例列表加载没有在途请求保护，慢的旧轮询结果可能覆盖新结果；`usePolling` / `useTicker` 没有单测；已释放实例的时间列为空未断言。
- 市场页与实例页首次加载失败时 toast 与页内提示条同时出现；刷新后已失效的镜像选择不会被清除。
- 钱包流水在静默错误（401 / 取消）时也会显示页内重试提示条。
- 手机端回到前台后恢复 3 秒轮询、充值起始页防重复提交只由代码结构保证，没有自动化断言。
- 实名提交的 `kyc_in_progress` / `kyc_unavailable` / `rate_limited` / `invalid_state` 专属文案未单独测试（走通用 `notifyError`）；上传途中离开页面只做了代码审查。
- 手机底部弹出确认框没有高度上限。

### 后台
- 审计接口每次请求全量读取管理员列表来填 `actor_name`；若管理员列表改为分页，名字会变空。
- `notify.ts` 控制台与后台各一份（shared 不依赖 element-plus）。
- 登出时遇到 401 会丢掉 `redirect` 参数；菜单高亮状态没有断言。
- 机器详情显示 PDU 的 ID 而不是名称。
- `usePaged` 在第 1 页同时改页码和每页条数时会多发一次请求（结果正确）；机器页 `onBeforeRouteUpdate` 与财务页 `search()` 在导航确认前就执行；操作中的 busy 状态会跨路由保留。
- 调账「余额不足」的文案映射写在视图里，没有收进统一错误映射。
- 商品价格排序失败时的回滚是尽力而为。
- 实例 `suspended` 状态只用模拟响应测试（真实路径需要全局离线清扫）；A-INS-09 端到端只覆盖 running / wiping，其余状态由单测矩阵覆盖。

### 后端（Go）
- `internal/api/instances.go` 的 `nameCache.lookup` 接收者未使用。
- 静态处理器对 `/admin/api/v1` 以外的 404 / 415 没有 `no-store`；嵌套目录的 `index.html` 会 301；`internal/web` 测试里对控制台处理器请求了 `/admin/x`。

### 测试基础设施
- e2e project 顺序依赖：全量用例共享一个嵌入式数据库和一个假时钟，只在 console-desktop → console-mobile → admin 的顺序下验证过。会改全局状态且不恢复的用例：A-INS-07 推进 301 秒、A-FIN-04 推进接近 24 小时、A-AUTH / A-DASH 会话过期用例推进 13 小时、A-ALT-01 让全部机器失联一轮、C-MKT-06 / A-INS-04 把当时所有空闲机器置为维护。说明写在 `web/playwright.config.ts` 的 `projects` 上方；调整顺序或新增此类用例时要跑完整 `make check`。
- e2e-server 的 `waitListening` 可能把占着端口的旧进程当成已就绪（应先 `net.Listen`）。
- e2e 钩子：`/hooks/users` 非原子；钩子服务错误一律 500；重复 `serial_prefix` 会部分插入；分块传输的空请求体返回 400；钩子单测对 `totp_secret` 只做了弱断言。
- 覆盖表核对器识别不了 `test['skip']` 或 `const t = test` 这类别名写法。
- 时钟冻结后的用例依赖 `MessageChannel` 不被 `page.clock` 接管（`drainTasks`）；Playwright 若改变这一点需要换实现。
- C-INS-04 用真实时间的 `expect.poll`（计划原文）而不是 `page.clock`；C-INS-03 的清扫恢复不在 `finally` 里、详情页只断言可见。
- C-MKT-07 区分不出第二次点击是被哪一道防重复守卫拦下的。
- A-MCH-04 按全站空闲台数的差值断言，依赖同一时刻没有其他用例改动空闲机器（串行执行下成立）。
- 后台财务日期筛选的「from > to」端到端用例实际是同一时刻（真正的 from > to 由 Go 单测 `TestParseRange` 覆盖）；每页条数切换只有单测。
