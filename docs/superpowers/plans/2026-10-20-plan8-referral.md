# Spark Cloud 计划 8：邀请返利 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 每个用户有一个永久 8 位邀请码与链接；新用户注册（首次登录）时带码即绑定；被邀请人每笔实付租用消费（`order_pay` 流水）按扣款时刻生效的费率版本为邀请人计提返利，7 天（或预订开始时刻）后发放到邀请人钱包余额（新流水类型 `referral_rebate`），退款按比例扣回（`referral_clawback`，余额不够记欠账）；同设备自动搁置、同身份自动撤销、同 IP / 爆发打标记；后台可配费率版本、审核搁置、冻结邀请人、撤销绑定、重扫、看报表与导出 CSV；控制台有邀请页；最后在开发平台（Mac `make run`）按规格 §16.4 全部 12 项验收通过。

**Architecture:** 新包 `internal/referral` 只写自己的 6 张表（`referral_*`、`user_devices` 由 identity 写）、只读 `wallet_ledger`，资金变动全部经 `wallet.Apply / ApplyUpTo`。返利的发现不嵌入任何扣款 / 退款事务，而是作为清扫附加任务 `Referral.Settle = Discover（扫描 wallet_ledger 中 ref_type='order' 的新行，按 source_ledger_id 幂等）→ Release（到期发放）`。绑定在 `identity.LoginWith` 的注册事务内由 `referral.Bind` 完成（`INSERT … RETURNING (xmax = 0) AS is_new` 判断新用户）；同身份判定在 KYC 结果事务内由 `referral.OnKYCVerified` 完成（`kyc_records.fingerprint = HMAC`）。所有 referral 路径都从 `referral_accounts(邀请人) FOR UPDATE` 开始加锁，rental / billing 从不锁 referral 表，等待链不成环。

**Tech Stack:** Go 1.27（`~/.local/go/bin`；`crypto/hkdf` 标准库）、pgx v5、chi v5、embedded-postgres（`dbtest.Main`）；前端 Vue 3 + Element Plus + Vitest + Playwright 1.64（Chrome 通道）；控制台已有 `qrcode@1.5.4`。**不新增任何 Go 模块依赖、不新增 npm 依赖、不新增环境变量。**

**Spec:** `docs/superpowers/specs/2026-10-20-plan8-referral-design.md`（已批准，提交 6e2c07b；负责人裁定 R1～R14 为默认值，可事后推翻）。上位：`2026-10-14-spark-cloud-mvp-design.md` §2 `wallet_*`、§11；计划 4 规格 §0.1 第 3 条、§2 权限矩阵；计划 6 规格 §5.5、§14；e2e 规范与覆盖表见计划 5 规格 §7.3、§7.5。

**分支：** `feat/plan8-referral`（规格提交 6e2c07b），工作树 `/Users/rifuyiri/Documents/claude/机房搭建/spark-cloud-p8`。

**本计划不包含（规格 §17）：** 被邀请人奖励（首充赠送 / 优惠券）；多级分销；团队 / 代理商结算；返利提现 / 对公结算 / 发票税务；独立返利余额与抵扣优先级；按充值返利、固定注册奖励、阶梯费率、按商品差异化费率；注册后补填邀请码、更换 / 自定义邀请码、邀请海报；短信 / 站内信 / 邮件通知；定期全量同身份扫描、更强设备指纹、号段识别、行为风控；撤销时自动扣回已发放返利（财务调账）；返利期到期提醒 / 延长；后台代用户生成码、运营活动码；H5 分享卡片 / 小程序；按费率版本汇总的对账报表。

## 执行须知（写给每个子代理）

- 任务**按编号顺序串行执行，一个任务一个子代理**。子代理只看得到自己任务的文字，看不到本文其它任务：每个任务的 Interfaces 必须完整（消费什么、产出什么、精确签名），代码步骤给完整代码，不写「同上」「类似 Task N」。
- 本计划**不需要真机**（不涉及 Agent）：验收在开发 Mac（192.168.1.218）上 `make run` + mock 支付 / 短信 / 实名完成；需要实例时用 e2e 的 fake Agent 或开发环境已接入的那台 DGX Spark（192.168.1.221）均可。真机 `sudo` 的密码规则沿用计划 6：**密码绝不写进任何文件、记录、提交或本计划**。
- 开发 Mac 的 DNS 对 `*.sslip.io` 返回假地址（198.18.x.x）：curl 一律加 `--resolve <host>:<port>:<ip>`；Chrome / Playwright 用 `--host-resolver-rules`。
- 开发环境短信验证码（mock，固定 `000000`）按手机号每 60 秒只能发一次：脚本化登录前先等 60 秒或换手机号。
- 每个任务结束前必须实际运行并通过：`go vet ./... && go test ./...`；涉及前端的任务再加 `cd web && npm test && npm run build`；页面任务再加该任务列出的 Playwright 用例。证据是命令输出。
- 提交信息结尾空一行加 `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`。

## Global Constraints

- 仓库根目录（本工作树）`/Users/rifuyiri/Documents/claude/机房搭建/spark-cloud-p8`；所有命令前置 `export PATH=$HOME/.local/go/bin:$HOME/.local/bin:$PATH`。Go 模块名 `spark-cloud`。开发 Mac 没有 Docker；Go 测试用 embedded-postgres（`dbtest.Main`，起始时间 `dbtest.T0 = 2026-01-01 00:00 UTC`，密钥 `dbtest.Key`）。
- 绑定时机（规格 §1.1 第 1 条、R7）：**只在注册时绑定**；`identity` 的 `INSERT INTO users … ON CONFLICT (phone) DO UPDATE … RETURNING …, (xmax = 0) AS is_new`；老用户带码登录不绑定（`reason=existing_user`）；邀请码无效 / 码主冻结或封禁时**注册照常成功**、`invite.bound=false`；`Bind` 的业务失败不回滚注册，数据库错误照常回滚。
- 返利基数与归属（第 2、3 条，R1、R4）：被邀请人 `order_pay` 流水（`ref_type='order'`，含新购 / 续费 / 自动续费 / 预订 / 批次）扣除 `refund` 流水；返利并入邀请人钱包余额（`referral_rebate`），不可提现（既有约束）；不按充值、无固定奖励。
- 费率版本（第 7 条，R14）：`referral_rate_versions` 只增不改；迁移写入首版 `rate_bp=1000, max_per_invitee=50000, max_per_inviter_30d=500000, hold_days=7, period_days=365, effective_from='2000-01-01'`；新版本 `effective_from >= 提交时刻`（否则 `referral_config_invalid`）；`period_days` 取**绑定时刻**版本（写进 `referral_bindings.rebate_until`），`rate_bp / max_per_invitee / max_per_inviter_30d / hold_days` 取**扣款流水 `created_at` 时刻**版本（写进 `referral_rewards`）。
- 计提（规格 §4.2）：`raw = floor(base × rate_bp / 10000)`；`room_invitee = max(0, max_per_invitee − Σ reward(该 binding, status ≠ void))`；`room_inviter = max(0, max_per_inviter_30d − Σ(settled_amount − clawed_amount)(该邀请人, settled_at > now − 30d) − Σ reward(该邀请人, pending))`；`cap = min(raw, room_invitee, room_inviter)`；`settle_after = max(paid_at + hold_days, scheduled_start_at)`；`cap < raw` 另记 `capped(amount = raw − cap)`；`paid_at >= rebate_until` 只记 `capped(detail='period_over', amount 0)`；绑定 `revoked` 记 `void(detail='binding revoked')` 占位；订单不存在 → 记日志、跳过、不写事件（下轮重试）。
- 退款（规格 §4.3）：`order = strip(ref_id, "-r<n>")`；`refunded_amount += amount`；`target = min(reward_cap, floor((order_amount − refunded_amount) × rate_bp / 10000))`；事件 `refund(amount = reward − target)`；`pending` 只改 `reward`；`settled` 的 `excess = (settled_amount − clawed_amount) − target > 0` 时 `ApplyUpTo(−excess, referral_clawback, ref('referral', order+"-c"+n))`，`clawed_amount += excess`（账面全扣），差额 `debt += shortfall` + 事件 `debt_add`；`void` 只更新 `refunded_amount`；`n` = 该订单已有 `clawback` 事件数 + 1。
- 发放（规格 §4.4，R6、R9）：每轮 ≤ 200 行，逐行独立事务；门槛：绑定 `active`、账户 `active`、邀请人 `users.status='active' AND kyc_status='verified'`，任一不满足**跳过不动**；`reward == 0 → void`；`offset = min(debt, reward)` 先抵欠账（事件 `debt_offset`，不写钱包）；`credit = reward − offset > 0` 才 `Apply(+credit, referral_rebate, ref('referral', order), remark="邀请返利 <脱敏手机> 订单 <order>")`；`settled_amount = reward`（含抵掉部分）。
- 扫描（规格 §5.1、§5.2）：`ScanLookback = 1h`、`ScanLag = 2m`、`ScanBatch = 500`、`ReleaseBatch = 200`；一轮取到 < 500 行时 `watermark = now − 2m`，否则不动；`referral_events.source_ledger_id` 唯一；`JOIN referral_bindings` 不过滤 `revoked`；每行独立事务，单行失败只记日志；遇 `40P01 / 40001` 整行重试一次。`cmd/platform/main.go`：`RunSweeper(ctx, cfg.SweepInterval, a.Billing.Sweep, a.Referral.Settle)`；`e2ehooks.sweep` 在 `Billing.Sweep` 之后调 `Referral.Settle`。
- 防刷（规格 §3.2、§3.3、R8）：`same_ip`（`ip = inviter.last_login_ip AND last_login_at > now − 24h`）、`same_device`（`user_devices` 有同设备）、`burst`（同邀请人 24h 内已有 ≥ 19 条绑定，本次为第 20 条起）只打标记；含 `same_device` → `status='held', hold_reason='same_device'`；实名通过时 `fingerprint = hex(HMAC-SHA256(HKDF(SecretKey, "referral-fingerprint"), "kyc:" + 身份证号 | "lic:" + 信用代码))`，与邀请人或同邀请人其它被邀请人相同 → `revoked`、`risk_flags += same_identity`、待发放全部 `void`、审计 `referral.auto_revoke`；邀请人后实名也检查其所有未撤销绑定。
- 邀请码（规格 §3.1，R10、R12）：8 位 Crockford Base32（字母表 `0123456789ABCDEFGHJKMNPQRSTVWXYZ`，`crypto/rand`），撞唯一索引重试最多 5 次；规范化：去空白、转大写、`O→0`、`I→1`、`L→1`；链接 `<PublicBaseURL>/?ic=<code>`；控制台 `localStorage.sc_invite = {code, at}` 30 天；`GET /referral/codes/{code}` 匿名、按 IP 每分钟 60 次（复用 `api.ipLimiter`）；`valid=false` 不区分原因。
- 设备（规格 §3.2）：请求头 `X-Device-Id` 为 UUID 格式时写 `user_devices`（每用户最多 10 台，超出删最旧）；`web/shared/src/http.ts` 所有请求带 `X-Device-Id`（`localStorage.sc_device`，首次生成 `uuid()`）。
- 后台角色（R12，规格 §10.2）：`GET summary / config / inviters / inviters/{id} / bindings / bindings/export / rewards / rewards/export` 与 `POST freeze / unfreeze / bindings/{id}/approve` = finance；`POST bindings/{id}/revoke` = finance + TOTP；`POST config` 与 `POST rescan` = super + TOTP；`GET /users/{id}` 的 `referral` 块 = all；`GET /dashboard` 的 `referral_month_settled` = all（前端按角色显示）。审计动作：`referral.bind`（user）、`referral.auto_revoke`（system）、`referral.config`、`referral.freeze`、`referral.unfreeze`、`referral.approve`、`referral.revoke`、`referral.rescan`。
- 错误码新增（规格 §10）：`invite_code_invalid`（400「邀请码无效」）、`referral_frozen`（409「邀请功能已暂停」）、`referral_binding_state`（409「该绑定当前状态不允许此操作」）、`referral_config_invalid`（400，消息按字段现场构造）。
- 枚举同步（规格 §2 说明）：`wallet.ledgerKinds`、`web/shared/src/labels.ts` 的 `ledgerKindLabel`（`referral_rebate: '邀请返利'`、`referral_clawback: '返利扣回'`）、后台流水筛选下拉随 `ledgerKindLabel` 自动扩展；`auditActions` 加 8 个动作。
- 迁移 00020（规格 §2）：建 `referral_rate_versions / referral_accounts / referral_bindings / referral_rewards / referral_events / referral_state / user_devices`；`wallet_ledger_kind_check` 重建为六种；`wallet_ledger_order_scan_idx`；`users.last_login_ip / last_login_at`；`kyc_records.fingerprint` + 部分索引；`referral_state` 初始水位 `'2000-01-01'`（**不能用 now()**：假时钟下流水 `created_at` 可能早于真实时间）；Down 在存在 `referral_rebate / referral_clawback` 流水时 `RAISE EXCEPTION '仍有邀请返利流水，不能回滚'`。
- 存量实名用户无指纹：`platform referral backfill-fingerprints` 子命令用 `SPARK_SECRET_KEY` 解密逐条写入；上线后执行一次。
- 不变量（规格 §14，`referral.CheckInvariants` 在所有涉及资金的测试结束时调用）：(1) 每个邀请人 `Σ rebate − Σ clawback == Σ(settled_amount − clawed_amount) + debt`；(2) `settled` 行 `reward == settled_amount − clawed_amount`，`pending` 行 `settled_amount == clawed_amount == 0`，`void` 行无钱包流水；(3) `refunded_amount(order) == Σ 已处理 refund 流水 ≤ orders.amount`；(4) 每条被绑定用户的 `order_pay` 流水恰一条 `accrue` / `capped(period_over)` / `void(binding revoked)` 事件，每条有 reward 的 `refund` 流水恰一条 `refund` 事件；(5) 30 天 `Σ(settled − clawed) ≤ max_per_inviter_30d`（按发放时版本），每绑定 `Σ reward(≠ void) ≤ max_per_invitee`。
- 加锁顺序（规格 §14，写进 `internal/referral/referral.go` 头注释）：Bind `users(被邀请人, INSERT 持有) → referral_accounts(码主) FOR SHARE → 插 referral_bindings`；计提 / 退款 `referral_accounts(邀请人) FOR UPDATE → referral_rewards(订单) FOR UPDATE → [wallet_accounts(邀请人)，仅扣回]`；发放 `referral_accounts(邀请人) FOR UPDATE → referral_rewards(订单) FOR UPDATE SKIP LOCKED → wallet_accounts(邀请人)`；冻结 / 解冻 / 撤销 / 通过 `referral_accounts(邀请人) FOR UPDATE → referral_bindings → referral_rewards`；OnKYCVerified `users(本人, 已持有) → referral_accounts(邀请人) FOR UPDATE → referral_bindings → referral_rewards`；rental / billing 的事务不碰 referral 表。
- 时间一律 `clock.Clock`；`hold_days` 与 30 天窗口按 UTC 时刻差；错误用 `apperr`；生产禁止 mock。
- 端到端规范沿用计划 5：用例标题以覆盖表 ID 开头、`data-testid` 命名 `页面-元素`、禁止 `waitForTimeout`、`workers: 1`；`web/e2e/coverage.json` 与覆盖表一致；现有 Playwright 用例必须保持全绿；返利用例推进时间 ≤ 8 天。

## Review Focus

1. **退款流水先于（或没有）它的计提落地**（`order_pay` 行因订单查询失败被跳过、或订单在返利期之后只记了 `capped(period_over)`、或绑定已撤销记了 `void` 占位）：没有任何事件的订单的退款必须**延后**（下轮再试，不能丢，否则已发放的返利永远扣不回）；已有 `capped / void` 占位的订单的退款必须写一条 `refund(amount 0)` 占位，不能每轮重扫 → Task 8 `TestRefundWithoutRewardDeferredOrPlaceholdered`；Discover 一轮内按「先 `order_pay` 后 `refund`」排序 → Task 10 `TestDiscoverProcessesPayBeforeRefundInSameBatch`。
2. **扣回与邀请人自己的扣款 / 充值同时发生**：`ApplyUpTo` 在持有 `wallet_accounts` 行锁后才取 `min(|amount|, balance)`，余额永不为负、`balance_after` 链连续 → Task 2 `TestApplyUpToConcurrentWithDebitsNeverOverdraws`；Task 14 `TestReferralConcurrentFundsConsistent` 把「邀请人并发下单」混进去。
3. **费率版本在计提与退款 / 发放之间被改掉**：退款重算与发放只用 `referral_rewards` 上存的 `rate_bp / reward_cap`，新版本只影响之后的扣款 → Task 8 `TestRefundUsesStoredRateNotCurrentVersion`。
4. **冻结 / 撤销与到期发放落在同一轮清扫**：发放事务先 `referral_accounts FOR UPDATE` 再复核状态，冻结后的奖励不会在同一轮被发出；解冻后下一轮补发；撤销把 pending 全部 void 且已发放的不动 → Task 11 `TestFreezeBlocksReleaseUntilUnfreeze`、`TestRevokeVoidsPendingKeepsSettled`。
5. **邀请码以用户手打的形态到达**（小写、带空格、`O/I/L` 混写、长度不对、含非法字符、超长）：`?ic=` 落地只存规范化后合法的码；登录体里的垃圾码不能让注册失败，响应 `invite.bound=false, reason=invalid_code`；匿名 `codes/{code}` 对非法形态返回 `valid=false` 而不是 400 / 500 → Task 3 `TestNormalizeCode`、Task 12 `TestLoginWithGarbageInviteCodeStillRegisters`、Task 15 `parseInviteParam` 单测。

## 已做出的裁定（规格未写明或与代码核对后确定的细节）

- **`identity.Login(ctx, phone, code)` 保持签名不变**（现有 api / 测试 / e2e 继续用），新增 `LoginWith(ctx, LoginRequest) (token string, LoginResult, error)`；`Login` = `LoginWith(LoginRequest{Phone, Code})` 丢弃 `Invite`。`LoginResult{User; IsNew bool; Invite *referral.BindResult}`，JSON 用匿名嵌入 `User` + `invite,omitempty`。
- **依赖方向：`identity` 导入 `referral`**（`referral.BindRequest / BindResult / UserState` 类型在 referral）；`referral` 不导入 `identity / rental`，用接口 `UserLookup / OrderLookup`。装配顺序：先建 `ids`（无 hook），再建 `ref := referral.NewService(referral.Deps{Users: ids, …})`，再 `ids.SetReferral(ref)`。
- **指纹密钥**：`referral.DeriveFingerprintKey(secret []byte) ([]byte, error)`（`crypto/hkdf.Key(sha256.New, secret, nil, "referral-fingerprint", 32)`）在 `app.Build` 与 `backfill-fingerprints` 子命令各算一次；`identity.Deps.FingerprintKey []byte`；`referral.Fingerprint(key, kind, number)`（`kind ∈ "kyc" | "lic"`）。
- **个人实名的指纹写入时机**：`SubmitPersonalKYC` 的结果事务里（`result == "verified"` 时），身份证号在内存里（`idNumber`）；企业在 `ReviewEnterpriseKYC` 通过时用 `license_no`。两条路径都在 `UPDATE users` 之后、同事务内调 `s.d.Referral.OnKYCVerified`；`Referral == nil` 时跳过（单测 / 旧装配可用）。
- **`UserLookup.UserState` 返回 `referral.UserState{Status, KYCStatus, Phone, LastLoginIP string; LastLoginAt *time.Time}`**；`HasDevice`、`VerifiedFingerprints(ctx, q, userIDs []int64) map[int64]string`（每人最新一条 `verified` 且指纹非空的记录）。
- **`rental.OrderForReferral(ctx, q db.Querier, orderID) (userID int64, scheduledStartAt *time.Time, err error)`**：`SELECT o.user_id, i.scheduled_start_at FROM orders o JOIN instances i ON i.id = o.instance_id WHERE o.id = $1`；不存在 → `apperr.ErrNotFound`。
- **Discover 的 SQL 排序**：`ORDER BY (l.kind = 'refund'), l.id`（同一批里 `order_pay` 先处理）。
- **没有任何事件的订单的退款**：跳过、不写事件（下轮重试）；订单已有 `capped(period_over)` 或 `void(binding revoked)` 事件而无 reward 行 → 写 `refund(source_ledger_id=l.id, amount=0, detail='no reward')` 占位。
- **Release 的候选查询**用 `FOR UPDATE SKIP LOCKED` 锁 `referral_rewards` 行后再锁 `referral_accounts`？不——锁顺序必须 `referral_accounts → referral_rewards`。做法：无锁选出候选 `order_id` 列表（≤ 200），逐行事务里先 `referral_accounts(inviter) FOR UPDATE`，再 `referral_rewards WHERE order_id = $1 AND status = 'pending' AND settle_after <= $2 FOR UPDATE SKIP LOCKED`（锁不到 / 条件变了 → 跳过）。
- **邀请人 30 天上限的占用口径**（保守）：`Σ(settled_amount − clawed_amount) WHERE settled_at > now − 30d` + `Σ reward WHERE status='pending'`。
- **`/referral/me` 懒生成**：`EnsureCode` 用 `INSERT … ON CONFLICT (user_id) DO NOTHING` + 重查，并发 10 次只建一行。
- **脱敏手机号** `referral.MaskPhone("13812345678") = "138****5678"`（长度 ≠ 11 时保留首 3 位 + `****`）。
- **`GET /me` 的 `referral_month_settled`**：`handleMe` 查 `Referral.MonthSettledFor(ctx, userID)`（本月北京时间 1 日 0 点起 `Σ referral_rebate` 流水），0 时字段省略；`User` 不改，用 `meView{User; ReferralMonthSettled int64 `json:"referral_month_settled,omitempty"`}`。
- **后台 `/referral/inviters` 的 `sort`** ∈ `settled`（默认，累计发放净额降序）、`invitees`（被邀请人数降序）、`bound_at`（最近绑定）。
- **仪表盘 `referral_month_settled`**：本月北京时间 1 日 0 点至下月 `Σ referral_rebate − Σ referral_clawback` 流水。
- **控制台邀请页路由 `/invite`**（`auth: true`，标题「邀请返利」），入口在「我的」页；底部 Tab / 侧栏不加项。
- **e2e 钩子**：`/hooks/users` 增 `invite_code?`、`device_id?`、`ip?`（给了 `invite_code` 时改走 `Identity.LoginWith` 真实路径：先 `SendLoginCode` 再 `LoginWith`，mock 码 `000000`；之后再按 `verified / balance` 补实名与余额）；新增 `POST /hooks/referral/config {rate_bp?, max_per_invitee?, max_per_inviter_30d?, hold_days?, period_days?}` → `{id}`（`Referral.AddRateVersion`，`effective_from = now`，未给字段沿用当前版本）；`/hooks/sweep` 末尾调 `Referral.Settle`。
- **CSV 列**：返利明细 `订单号, 邀请人ID, 被邀请人ID, 消费(元), 退款(元), 费率(%), 应得(元), 状态, 发放时间, 已扣回(元), 费率版本`；绑定 `绑定ID, 邀请人ID, 被邀请人ID, 邀请码, 状态, 标记, 绑定时间, 返利截止, IP, 设备`。
- **`revoke` 的 `voided_amount`** = 本次由 pending 置 void 的 `Σ reward`。
- **`rescan`**：`UPDATE referral_state SET watermark = LEAST(watermark, $now − days)` 后同步调一次 `Settle`（在请求内执行，`days ≤ 30` 时一轮 ≤ 500 行，秒级）。

## 文件结构

```
spark-cloud/
├── migrations/00020_referral.sql                                     # Task 1
├── internal/
│   ├── platform/apperr/apperr.go（错误码）                          # Task 1
│   ├── platform/db/migrate_referral_test.go                          # Task 1
│   ├── wallet/wallet.go（Kind、ApplyUpTo）、admin.go（ledgerKinds）、wallet_test.go  # Task 1、2
│   ├── referral/referral.go、code.go、config.go、fingerprint.go、bind.go、kyc.go、accrue.go、refund.go、release.go、settle.go、invariants.go、admin.go、queries.go、*_test.go  # Task 3、5～11
│   ├── identity/service.go、auth.go、kyc.go、referral.go、auth_test.go、kyc_test.go  # Task 4、6
│   ├── rental/orders.go（OrderForReferral）                          # Task 7
│   ├── api/referral.go、auth.go、server.go、referral_test.go         # Task 12
│   ├── adminapi/referral.go、users.go、dashboard.go、server.go、permissions_test.go、referral_test.go  # Task 13
│   ├── app/app.go（装配）、app/referral_e2e_test.go                  # Task 10、14
│   ├── e2ehooks/hooks.go（sweep、users、referral/config）            # Task 10、17
├── cmd/platform/main.go、referral.go（backfill-fingerprints）        # Task 6、10
├── web/shared/src/labels.ts、http.ts、types/{wallet,referral,admin,user}.ts、__tests__/http.spec.ts  # Task 1、15、16
├── web/console/src/App.vue、router.ts、views/{LoginView,InviteView,AccountView,WalletView,HelpView}.vue、logic/invite.ts、logic/__tests__/invite.spec.ts  # Task 15
├── web/admin/src/menu.ts、router.ts、views/{ReferralView,UserDetailView,DashboardView}.vue、__tests__/menu.spec.ts  # Task 16
├── web/e2e/helpers/{hooks,referral}.ts、console/referral.spec.ts、console/account.spec.ts、console/wallet.spec.ts、admin/referral.spec.ts、admin/finance.spec.ts、coverage.json、__tests__/coverage.spec.ts  # Task 17
├── README.md                                                         # Task 19
└── docs/superpowers/{acceptance-plan8.md,followups-plan8.md}         # Task 18、19
```

## 任务依赖

Task 1（迁移 + 类型 + 错误码）→ 2（`ApplyUpTo`）→ 3（referral 骨架：码、费率、指纹）→ 4（identity 登录扩展 + `UserLookup`）→ 5（`Bind`）→ 6（KYC 指纹 + `OnKYCVerified` + 回填子命令）→ 7（`OrderForReferral` + 计提）→ 8（退款 / 扣回 / 欠账 / 不变量）→ 9（发放）→ 10（扫描 / 水位 / `Settle` / 装配）→ 11（后台裁决）→ 12（控制台 API）→ 13（后台 API + 权限矩阵）→ 14（集成与并发测试）→ 15（控制台页面）→ 16（后台页面）→ 17（e2e 钩子 + Playwright + 覆盖表）→ **18 开发平台验收** → 19（收尾）。按编号顺序执行，共 19 个任务。

---
### Task 1: 迁移 00020、`wallet` 新流水类型、`ledgerKinds`、`labels.ts`、错误码

**Files:**
- Create: `migrations/00020_referral.sql`、`internal/platform/db/migrate_referral_test.go`
- Modify: `internal/wallet/wallet.go`（`Kind` 常量）、`internal/wallet/admin.go`（`ledgerKinds`）、`internal/wallet/admin_test.go`（追加）、`internal/platform/apperr/apperr.go`、`web/shared/src/labels.ts`、`web/shared/src/types/wallet.ts`
- Test: `internal/platform/db/migrate_referral_test.go`、`internal/wallet/admin_test.go`

**Interfaces:**
- Produces:
  - 表 `referral_rate_versions`（含首版行）、`referral_accounts`、`referral_bindings`、`referral_rewards`、`referral_events`、`referral_state`（一行 `watermark='2000-01-01'`）、`user_devices`；`users.last_login_ip TEXT NOT NULL DEFAULT ''`、`users.last_login_at TIMESTAMPTZ`；`kyc_records.fingerprint TEXT NOT NULL DEFAULT ''` + `kyc_records_fingerprint_idx`；`wallet_ledger_kind_check` 六种；`wallet_ledger_order_scan_idx (created_at, id) WHERE ref_type = 'order'`。
  - `wallet.KindReferralRebate Kind = "referral_rebate"`、`wallet.KindReferralClawback Kind = "referral_clawback"`；`LedgerFilter.Kind` 接受两种新值。
  - `apperr.ErrInviteCodeInvalid`（`invite_code_invalid`, 400,「邀请码无效」）、`apperr.ErrReferralFrozen`（`referral_frozen`, 409,「邀请功能已暂停」）、`apperr.ErrReferralBindingState`（`referral_binding_state`, 409,「该绑定当前状态不允许此操作」）；`referral_config_invalid` 用 `apperr.New("referral_config_invalid", msg, 400)` 现场构造。
  - `labels.ts`：`ledgerKindLabel` 加 `referral_rebate: '邀请返利', referral_clawback: '返利扣回'`；`auditActions` 加 `'referral.bind', 'referral.auto_revoke', 'referral.config', 'referral.freeze', 'referral.unfreeze', 'referral.approve', 'referral.revoke', 'referral.rescan'`；`types/wallet.ts` 的 `LedgerKind` 联合加两种。

- [ ] **Step 1: 写失败的迁移往返测试**

`internal/platform/db/migrate_referral_test.go`：

```go
package db_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"

	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/migrations"
)

// 迁移 00020 在带存量数据的库上 Up → Down → Up 往返；有 referral_* 流水时 Down 拒绝。
func TestMigration00020RoundTrip(t *testing.T) {
	ctx := context.Background()
	sqlDB, err := sql.Open("pgx", dbtest.EmptyURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	p, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := sqlDB.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	scalar := func(q string, args ...any) string {
		t.Helper()
		var s sql.NullString
		if err := sqlDB.QueryRowContext(ctx, q, args...).Scan(&s); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return s.String
	}
	version := func() int64 {
		t.Helper()
		v, err := p.GetDBVersion(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	if _, err := p.UpTo(ctx, 19); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO users (phone, kyc_status, created_at, updated_at) VALUES ('13800000000', 'verified', $1, $1)`, dbtest.T0)
	exec(`INSERT INTO wallet_accounts (user_id, balance, updated_at) VALUES ((SELECT id FROM users), 100, $1)`, dbtest.T0)
	exec(`INSERT INTO wallet_ledger (user_id, amount, balance_after, kind, ref_type, ref_id, remark, created_at)
		VALUES ((SELECT id FROM users), 100, 100, 'recharge', 'recharge', 'rch-1', '', $1)`, dbtest.T0)

	if _, err := p.UpTo(ctx, 20); err != nil {
		t.Fatalf("up 20: %v", err)
	}
	if got := scalar(`SELECT rate_bp || '/' || max_per_invitee || '/' || max_per_inviter_30d || '/' || hold_days || '/' || period_days FROM referral_rate_versions`); got != "1000/50000/500000/7/365" {
		t.Fatalf("seed rate version = %s", got)
	}
	if got := scalar(`SELECT watermark::date::text FROM referral_state WHERE id = 1`); got != "2000-01-01" {
		t.Fatalf("watermark = %s", got)
	}
	if got := scalar(`SELECT last_login_ip || '/' || (last_login_at IS NULL) FROM users`); got != "/true" {
		t.Fatalf("users new cols = %s", got)
	}
	if got := scalar(`SELECT count(*) FROM pg_indexes WHERE indexname IN ('wallet_ledger_order_scan_idx', 'kyc_records_fingerprint_idx', 'referral_events_source_uniq')`); got != "3" {
		t.Fatalf("indexes = %s", got)
	}
	// 新类型可写；未知类型仍被 CHECK 拒绝
	exec(`INSERT INTO wallet_ledger (user_id, amount, balance_after, kind, ref_type, ref_id, remark, created_at)
		VALUES ((SELECT id FROM users), 10, 110, 'referral_rebate', 'referral', 'ord-x', '', $1)`, dbtest.T0)
	if _, err := sqlDB.ExecContext(ctx, `INSERT INTO wallet_ledger (user_id, amount, balance_after, kind, ref_type, ref_id, remark, created_at)
		VALUES ((SELECT id FROM users), 10, 120, 'bogus', 'x', 'y', '', $1)`, dbtest.T0); err == nil {
		t.Fatal("bogus kind accepted")
	}
	// 有 referral 流水时 Down 拒绝且版本不变
	if _, err := p.Down(ctx); err == nil || !strings.Contains(err.Error(), "仍有邀请返利流水") {
		t.Fatalf("down with referral ledger = %v", err)
	}
	if v := version(); v != 20 {
		t.Fatalf("version after refused down = %d", v)
	}
	exec(`DELETE FROM wallet_ledger WHERE kind = 'referral_rebate'`)
	if _, err := p.Down(ctx); err != nil {
		t.Fatalf("down: %v", err)
	}
	if v := version(); v != 19 {
		t.Fatalf("version after down = %d", v)
	}
	if got := scalar(`SELECT count(*) FROM information_schema.tables WHERE table_name LIKE 'referral_%' OR table_name = 'user_devices'`); got != "0" {
		t.Fatalf("down left tables: %s", got)
	}
	if got := scalar(`SELECT count(*) FROM information_schema.columns WHERE (table_name = 'users' AND column_name IN ('last_login_ip', 'last_login_at')) OR (table_name = 'kyc_records' AND column_name = 'fingerprint')`); got != "0" {
		t.Fatalf("down left columns: %s", got)
	}
	if _, err := sqlDB.ExecContext(ctx, `INSERT INTO wallet_ledger (user_id, amount, balance_after, kind, ref_type, ref_id, remark, created_at)
		VALUES ((SELECT id FROM users), 10, 110, 'referral_rebate', 'referral', 'ord-y', '', $1)`, dbtest.T0); err == nil {
		t.Fatal("old CHECK not restored")
	}
	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("re-up: %v", err)
	}
	if got := scalar(`SELECT count(*) FROM referral_rate_versions`); got != "1" {
		t.Fatalf("rate versions after round trip = %s", got)
	}
}
```

`internal/wallet/admin_test.go` 追加：

```go
func TestLedgerFilterAcceptsReferralKinds(t *testing.T) {
	svc, pool := newEnv(t)
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13800000077", true)
	if _, err := apply(ctx, pool, svc, wallet.Entry{UserID: uid, Amount: 500, Kind: wallet.KindReferralRebate, RefType: "referral", RefID: "ord-a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := apply(ctx, pool, svc, wallet.Entry{UserID: uid, Amount: -200, Kind: wallet.KindReferralClawback, RefType: "referral", RefID: "ord-a-c1"}); err != nil {
		t.Fatal(err)
	}
	for kind, want := range map[string]int{"referral_rebate": 1, "referral_clawback": 1, "order_pay": 0} {
		list, total, err := svc.AdminListLedger(ctx, wallet.LedgerFilter{UserID: uid, Kind: kind}, paging.Page{Number: 1, Size: 10})
		if err != nil || total != want || len(list) != want {
			t.Fatalf("kind %s: %d %d %v", kind, total, len(list), err)
		}
	}
	if _, _, err := svc.AdminListLedger(ctx, wallet.LedgerFilter{Kind: "bogus"}, paging.Page{Number: 1, Size: 10}); apperr.As(err) == nil {
		t.Fatalf("bogus kind must be rejected, got %v", err)
	}
}
```

（`newEnv` / `apply` 在 `wallet_test.go` 已有；按需补 `paging`、`apperr`、`dbtest` 导入。）

- [ ] **Step 2: 运行测试确认失败**

Run: `cd /Users/rifuyiri/Documents/claude/机房搭建/spark-cloud-p8 && export PATH=$HOME/.local/go/bin:$PATH && go test ./internal/platform/db/ -run TestMigration00020 2>&1 | tail -5; go test ./internal/wallet/ -run TestLedgerFilterAcceptsReferralKinds 2>&1 | tail -5`
Expected: db 包 `up 20` 失败（无此迁移）；wallet 包编译错误（`KindReferralRebate` 未定义）。

- [ ] **Step 3: 写迁移**

`migrations/00020_referral.sql`：

```sql
-- +goose Up
-- 计划 8：邀请返利。费率版本只增不改；返利 / 扣回是两种新的钱包流水类型；user_devices 与 users.last_login_* 供风险标记；kyc_records.fingerprint 供同身份判定。
-- Down 在仍有 referral_rebate / referral_clawback 流水时拒绝（流水只追加，改类型就是篡改账目）。

CREATE TABLE referral_rate_versions (
    id                   BIGSERIAL PRIMARY KEY,
    rate_bp              INT    NOT NULL CHECK (rate_bp BETWEEN 0 AND 5000),      -- 万分比：1000 = 10%
    max_per_invitee      BIGINT NOT NULL CHECK (max_per_invitee >= 0),           -- 单个被邀请人累计返利上限（分）
    max_per_inviter_30d  BIGINT NOT NULL CHECK (max_per_inviter_30d >= 0),       -- 单个邀请人 30 天发放上限（分）
    hold_days            INT    NOT NULL CHECK (hold_days BETWEEN 0 AND 90),      -- 发放延迟
    period_days          INT    NOT NULL CHECK (period_days BETWEEN 1 AND 3650),  -- 返利期（绑定起）
    effective_from       TIMESTAMPTZ NOT NULL,
    note                 TEXT NOT NULL DEFAULT '',
    created_by           BIGINT,                                                  -- 管理员；迁移写入的首版为 NULL
    created_at           TIMESTAMPTZ NOT NULL
);
CREATE INDEX referral_rate_versions_effective_idx ON referral_rate_versions (effective_from DESC);
INSERT INTO referral_rate_versions (rate_bp, max_per_invitee, max_per_inviter_30d, hold_days, period_days, effective_from, note, created_at)
VALUES (1000, 50000, 500000, 7, 365, '2000-01-01', '初始版本（规格 R1～R3、R5）', now());

CREATE TABLE referral_accounts (
    user_id       BIGINT PRIMARY KEY REFERENCES users (id),
    code          TEXT NOT NULL UNIQUE,                                   -- 8 位 Crockford Base32（无 I L O U）
    status        TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'frozen')),
    frozen_reason TEXT NOT NULL DEFAULT '',
    frozen_by     BIGINT,
    frozen_at     TIMESTAMPTZ,
    debt          BIGINT NOT NULL DEFAULT 0 CHECK (debt >= 0),            -- 扣回时余额不足形成的欠账（分）
    created_at    TIMESTAMPTZ NOT NULL,
    updated_at    TIMESTAMPTZ NOT NULL
);

CREATE TABLE referral_bindings (
    id              BIGSERIAL PRIMARY KEY,
    inviter_id      BIGINT NOT NULL REFERENCES users (id),
    invitee_id      BIGINT NOT NULL UNIQUE REFERENCES users (id),
    code            TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'held', 'revoked')),
    risk_flags      TEXT[] NOT NULL DEFAULT '{}',                         -- same_ip / same_device / burst / same_identity
    hold_reason     TEXT NOT NULL DEFAULT '',
    rate_version_id BIGINT NOT NULL REFERENCES referral_rate_versions (id),
    bound_at        TIMESTAMPTZ NOT NULL,
    rebate_until    TIMESTAMPTZ NOT NULL,                                 -- bound_at + period_days（按绑定时版本）
    ip              TEXT NOT NULL DEFAULT '',
    device_id       TEXT NOT NULL DEFAULT '',
    user_agent      TEXT NOT NULL DEFAULT '',                             -- ≤ 200 字符
    reviewed_by     BIGINT,
    reviewed_at     TIMESTAMPTZ,
    review_note     TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL,
    updated_at      TIMESTAMPTZ NOT NULL
);
CREATE INDEX referral_bindings_inviter_idx ON referral_bindings (inviter_id, bound_at DESC);
CREATE INDEX referral_bindings_status_idx  ON referral_bindings (status, bound_at DESC) WHERE status = 'held';

CREATE TABLE referral_rewards (
    order_id         TEXT PRIMARY KEY,                                    -- 不加外键：referral 不依赖 rental 的表
    binding_id       BIGINT NOT NULL REFERENCES referral_bindings (id),
    inviter_id       BIGINT NOT NULL REFERENCES users (id),
    invitee_id       BIGINT NOT NULL REFERENCES users (id),
    rate_version_id  BIGINT NOT NULL REFERENCES referral_rate_versions (id),
    rate_bp          INT    NOT NULL,
    order_amount     BIGINT NOT NULL CHECK (order_amount > 0),            -- order_pay 流水的 |amount|
    refunded_amount  BIGINT NOT NULL DEFAULT 0 CHECK (refunded_amount >= 0 AND refunded_amount <= order_amount),
    reward_cap       BIGINT NOT NULL CHECK (reward_cap >= 0),             -- 计提时两项上限允许的最大返利
    reward           BIGINT NOT NULL CHECK (reward >= 0),                 -- 当前应得 = min(reward_cap, floor((order_amount − refunded_amount) × rate_bp / 10000))
    status           TEXT NOT NULL CHECK (status IN ('pending', 'settled', 'void')),
    settle_after     TIMESTAMPTZ NOT NULL,
    settled_amount   BIGINT NOT NULL DEFAULT 0 CHECK (settled_amount >= 0),  -- 发放时的应得（含被欠账抵掉的部分）
    clawed_amount    BIGINT NOT NULL DEFAULT 0 CHECK (clawed_amount >= 0 AND clawed_amount <= settled_amount),
    settled_at       TIMESTAMPTZ,
    paid_at          TIMESTAMPTZ NOT NULL,                                -- order_pay 流水的 created_at
    created_at       TIMESTAMPTZ NOT NULL,
    updated_at       TIMESTAMPTZ NOT NULL
);
CREATE INDEX referral_rewards_pending_idx ON referral_rewards (settle_after) WHERE status = 'pending';
CREATE INDEX referral_rewards_inviter_idx ON referral_rewards (inviter_id, paid_at DESC);
CREATE INDEX referral_rewards_binding_idx ON referral_rewards (binding_id);

CREATE TABLE referral_events (
    id               BIGSERIAL PRIMARY KEY,
    order_id         TEXT NOT NULL,
    binding_id       BIGINT NOT NULL REFERENCES referral_bindings (id),
    kind             TEXT NOT NULL CHECK (kind IN ('accrue', 'refund', 'settle', 'clawback', 'debt_add', 'debt_offset', 'void', 'capped')),
    source_ledger_id BIGINT,                                              -- accrue / refund 来自哪条 wallet_ledger
    wallet_ledger_id BIGINT,                                              -- settle / clawback 写出的 wallet_ledger
    amount           BIGINT NOT NULL,
    detail           TEXT NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ NOT NULL
);
CREATE UNIQUE INDEX referral_events_source_uniq ON referral_events (source_ledger_id) WHERE source_ledger_id IS NOT NULL;
CREATE INDEX referral_events_order_idx ON referral_events (order_id, id);

CREATE TABLE referral_state (
    id        INT PRIMARY KEY CHECK (id = 1),
    watermark TIMESTAMPTZ NOT NULL
);
-- 不能用 now()：测试与 e2e 用假时钟，流水 created_at 可能早于真实时间；首轮扫描 JOIN referral_bindings 为空，代价可忽略
INSERT INTO referral_state VALUES (1, '2000-01-01');

ALTER TABLE wallet_ledger DROP CONSTRAINT wallet_ledger_kind_check;
ALTER TABLE wallet_ledger ADD CONSTRAINT wallet_ledger_kind_check
    CHECK (kind IN ('recharge', 'admin_adjust', 'order_pay', 'refund', 'referral_rebate', 'referral_clawback'));
CREATE INDEX wallet_ledger_order_scan_idx ON wallet_ledger (created_at, id) WHERE ref_type = 'order';

ALTER TABLE users ADD COLUMN last_login_ip TEXT NOT NULL DEFAULT '', ADD COLUMN last_login_at TIMESTAMPTZ;
CREATE TABLE user_devices (
    user_id       BIGINT NOT NULL REFERENCES users (id),
    device_id     TEXT   NOT NULL,                                        -- 控制台生成的 UUID（localStorage）
    first_seen_at TIMESTAMPTZ NOT NULL,
    last_seen_at  TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (user_id, device_id)
);
CREATE INDEX user_devices_device_idx ON user_devices (device_id);
ALTER TABLE kyc_records ADD COLUMN fingerprint TEXT NOT NULL DEFAULT '';  -- HMAC，只对 verified 记录写
CREATE INDEX kyc_records_fingerprint_idx ON kyc_records (fingerprint) WHERE fingerprint <> '';

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM wallet_ledger WHERE kind IN ('referral_rebate', 'referral_clawback')) THEN
        RAISE EXCEPTION '仍有邀请返利流水，不能回滚';
    END IF;
END $$;
-- +goose StatementEnd
DROP INDEX kyc_records_fingerprint_idx;
ALTER TABLE kyc_records DROP COLUMN fingerprint;
DROP TABLE user_devices;
ALTER TABLE users DROP COLUMN last_login_at, DROP COLUMN last_login_ip;
DROP INDEX wallet_ledger_order_scan_idx;
ALTER TABLE wallet_ledger DROP CONSTRAINT wallet_ledger_kind_check;
ALTER TABLE wallet_ledger ADD CONSTRAINT wallet_ledger_kind_check
    CHECK (kind IN ('recharge', 'admin_adjust', 'order_pay', 'refund'));
DROP TABLE referral_state;
DROP TABLE referral_events;
DROP TABLE referral_rewards;
DROP TABLE referral_bindings;
DROP TABLE referral_accounts;
DROP TABLE referral_rate_versions;
```

- [ ] **Step 4: `wallet` 常量与 `ledgerKinds`、错误码、前端枚举**

`internal/wallet/wallet.go` 的 `const (...)` 加：

```go
	// 计划 8：邀请返利发放 / 扣回（ref_type='referral'，ref_id=<订单号> / <订单号>-c<n>）
	KindReferralRebate   Kind = "referral_rebate"
	KindReferralClawback Kind = "referral_clawback"
```

`internal/wallet/admin.go`：

```go
var ledgerKinds = []string{string(KindRecharge), string(KindAdminAdjust), string(KindOrderPay), string(KindRefund),
	string(KindReferralRebate), string(KindReferralClawback)}
```

`internal/platform/apperr/apperr.go` 的 `var (...)` 末尾加：

```go
	// 计划 8
	ErrInviteCodeInvalid     = New("invite_code_invalid", "邀请码无效", 400)
	ErrReferralFrozen        = New("referral_frozen", "邀请功能已暂停", 409)
	ErrReferralBindingState  = New("referral_binding_state", "该绑定当前状态不允许此操作", 409)
```

`web/shared/src/labels.ts`：

```ts
export const ledgerKindLabel: LabelMap = { recharge: '充值', order_pay: '扣款', refund: '退款', admin_adjust: '调账', referral_rebate: '邀请返利', referral_clawback: '返利扣回' }
```

`auditActions` 末尾追加一行：

```ts
  'referral.bind', 'referral.auto_revoke', 'referral.config', 'referral.freeze', 'referral.unfreeze', 'referral.approve', 'referral.revoke', 'referral.rescan',
```

`web/shared/src/types/wallet.ts`：

```ts
export type LedgerKind = 'recharge' | 'order_pay' | 'refund' | 'admin_adjust' | 'referral_rebate' | 'referral_clawback' | (string & {})
```

- [ ] **Step 5: 运行测试确认通过**

Run: `go vet ./... && go test ./internal/platform/db/ ./internal/wallet/ 2>&1 | tail -5 && cd web && npm test 2>&1 | tail -5`
Expected: 两个包 `ok`；vitest 全绿（`labels` 没有数量断言；后台 `LedgerView` 的下拉由 `ledgerKindLabel` 生成，自动多两项）。

- [ ] **Step 6: 提交**

```bash
git add migrations/00020_referral.sql internal/platform/db/migrate_referral_test.go internal/wallet/wallet.go internal/wallet/admin.go internal/wallet/admin_test.go internal/platform/apperr/apperr.go web/shared/src/labels.ts web/shared/src/types/wallet.ts
git commit -m "feat(referral): migration 00020 (rate versions, accounts, bindings, rewards, events, devices, fingerprint), referral ledger kinds, error codes

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 2: `wallet.ApplyUpTo`（尽力扣款原语）

**Files:**
- Modify: `internal/wallet/wallet.go`
- Test: `internal/wallet/wallet_test.go`（追加）

**Interfaces:**
- Consumes: Task 1 的 `KindReferralClawback`。
- Produces: `func (s *Service) ApplyUpTo(ctx context.Context, tx pgx.Tx, e Entry) (applied, after int64, err error)`：`e.Amount` 必须 < 0；与 `Apply` 相同的锁与记账逻辑，但扣款额 = `min(|e.Amount|, balance)`；`balance == 0` 时**不写流水**，返回 `applied = 0, after = 0`；`applied > 0` 时写一条 `amount = −applied` 的流水；重复 `(kind, ref_type, ref_id)` 返回 `apperr.ErrDuplicate`。这是本计划对 `wallet` 包唯一的写路径改动。

- [ ] **Step 1: 写失败的测试**

`internal/wallet/wallet_test.go` 追加：

```go
func applyUpTo(ctx context.Context, pool *pgxpool.Pool, svc *wallet.Service, e wallet.Entry) (applied, after int64, err error) {
	err = db.WithTx(ctx, pool, func(tx pgx.Tx) error {
		var e2 error
		applied, after, e2 = svc.ApplyUpTo(ctx, tx, e)
		return e2
	})
	return
}

func TestApplyUpToTakesMinOfAmountAndBalance(t *testing.T) {
	svc, pool := newEnv(t)
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13800000081", true)
	dbtest.Fund(t, pool, uid, 300)
	// 余额充足：全额扣
	applied, after, err := applyUpTo(ctx, pool, svc, wallet.Entry{UserID: uid, Amount: -100, Kind: wallet.KindReferralClawback, RefType: "referral", RefID: "ord-1-c1"})
	if err != nil || applied != 100 || after != 200 {
		t.Fatalf("full = %d %d %v", applied, after, err)
	}
	// 余额不足：扣到 0
	applied, after, err = applyUpTo(ctx, pool, svc, wallet.Entry{UserID: uid, Amount: -500, Kind: wallet.KindReferralClawback, RefType: "referral", RefID: "ord-1-c2"})
	if err != nil || applied != 200 || after != 0 {
		t.Fatalf("partial = %d %d %v", applied, after, err)
	}
	// 余额为 0：不记流水
	applied, after, err = applyUpTo(ctx, pool, svc, wallet.Entry{UserID: uid, Amount: -50, Kind: wallet.KindReferralClawback, RefType: "referral", RefID: "ord-1-c3"})
	if err != nil || applied != 0 || after != 0 {
		t.Fatalf("zero = %d %d %v", applied, after, err)
	}
	rows, err := svc.ListLedger(ctx, uid, 0, 10)
	if err != nil || len(rows) != 3 { // fund + c1 + c2
		t.Fatalf("ledger = %d %v", len(rows), err)
	}
	if rows[0].Amount != -200 || rows[0].RefID != "ord-1-c2" || rows[1].Amount != -100 {
		t.Fatalf("ledger rows = %+v", rows)
	}
	// 重复 ref → ErrDuplicate
	dbtest.Fund(t, pool, uid, 100)
	if _, _, err := applyUpTo(ctx, pool, svc, wallet.Entry{UserID: uid, Amount: -10, Kind: wallet.KindReferralClawback, RefType: "referral", RefID: "ord-1-c1"}); !errors.Is(err, apperr.ErrDuplicate) {
		t.Fatalf("duplicate = %v", err)
	}
	// 正数、0 与缺 ref 都被拒绝
	for _, e := range []wallet.Entry{
		{UserID: uid, Amount: 10, Kind: wallet.KindReferralClawback, RefType: "referral", RefID: "x"},
		{UserID: uid, Amount: 0, Kind: wallet.KindReferralClawback, RefType: "referral", RefID: "x"},
		{UserID: uid, Amount: -10, Kind: wallet.KindReferralClawback},
	} {
		if _, _, err := applyUpTo(ctx, pool, svc, e); apperr.As(err) == nil || apperr.As(err).Code != "bad_request" {
			t.Fatalf("entry %+v: want bad_request, got %v", e, err)
		}
	}
	// 没有账户行的用户：applied 0、不报错、不建流水
	uid2 := dbtest.CreateUser(t, pool, "13800000082", true)
	if applied, after, err := applyUpTo(ctx, pool, svc, wallet.Entry{UserID: uid2, Amount: -10, Kind: wallet.KindReferralClawback, RefType: "referral", RefID: "ord-2-c1"}); err != nil || applied != 0 || after != 0 {
		t.Fatalf("no account = %d %d %v", applied, after, err)
	}
}

// Review Focus 2：扣回与用户自己的扣款并发，余额永不为负，balance_after 链连续。
func TestApplyUpToConcurrentWithDebitsNeverOverdraws(t *testing.T) {
	svc, pool := newEnv(t)
	ctx := context.Background()
	uid := dbtest.CreateUser(t, pool, "13800000083", true)
	dbtest.Fund(t, pool, uid, 1000)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			_, _ = apply(ctx, pool, svc, wallet.Entry{UserID: uid, Amount: -90, Kind: wallet.KindOrderPay, RefType: "order", RefID: fmt.Sprintf("ord-%d", i)})
		}(i)
		go func(i int) {
			defer wg.Done()
			_, _, _ = applyUpTo(ctx, pool, svc, wallet.Entry{UserID: uid, Amount: -70, Kind: wallet.KindReferralClawback, RefType: "referral", RefID: fmt.Sprintf("ord-%d-c1", i)})
		}(i)
	}
	wg.Wait()
	bal, err := svc.Balance(ctx, uid)
	if err != nil || bal < 0 {
		t.Fatalf("balance = %d %v", bal, err)
	}
	rows, err := pool.Query(ctx, `SELECT amount, balance_after FROM wallet_ledger WHERE user_id = $1 ORDER BY id`, uid)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var prev int64
	for rows.Next() {
		var amount, after int64
		if err := rows.Scan(&amount, &after); err != nil {
			t.Fatal(err)
		}
		if after < 0 || prev+amount != after {
			t.Fatalf("ledger chain broken: prev %d + %d != %d", prev, amount, after)
		}
		prev = after
	}
	if prev != bal {
		t.Fatalf("last balance_after %d != balance %d", prev, bal)
	}
}
```

（补导入 `errors`、`fmt`、`sync`、`github.com/jackc/pgx/v5`、`spark-cloud/internal/platform/db`、`spark-cloud/internal/platform/apperr`、`spark-cloud/internal/platform/dbtest`——以文件现有导入为准。）

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/wallet/ -run 'TestApplyUpTo' 2>&1 | tail -5`
Expected: 编译错误 `svc.ApplyUpTo undefined`。

- [ ] **Step 3: 实现 `ApplyUpTo`**

`internal/wallet/wallet.go` 在 `Apply` 之后加：

```go
// ApplyUpTo 在调用方事务内尽力扣款：扣款额取 min(|e.Amount|, 余额)。
// 用于邀请返利的退款扣回（计划 8）：钱包余额不能为负，扣不够的部分由调用方记为欠账。
// e.Amount 必须为负；余额为 0（或没有账户行）时不写流水，返回 applied = 0；重复 (kind, ref_type, ref_id) 返回 ErrDuplicate。
func (s *Service) ApplyUpTo(ctx context.Context, tx pgx.Tx, e Entry) (applied, after int64, err error) {
	if e.Amount >= 0 {
		return 0, 0, apperr.BadRequest("尽力扣款金额必须为负数")
	}
	if e.RefType == "" || e.RefID == "" {
		return 0, 0, apperr.BadRequest("缺少关联单据")
	}
	now := s.clock.Now()
	var bal int64
	err = tx.QueryRow(ctx, `SELECT balance FROM wallet_accounts WHERE user_id = $1 FOR UPDATE`, e.UserID).Scan(&bal)
	if db.IsNoRows(err) {
		return 0, 0, nil // 从未有过余额：没什么可扣
	}
	if err != nil {
		return 0, 0, err
	}
	applied = min(-e.Amount, bal)
	if applied == 0 {
		return 0, bal, nil
	}
	after = bal - applied
	var operator *int64
	if e.OperatorID != 0 {
		operator = &e.OperatorID
	}
	if _, err := tx.Exec(ctx, `INSERT INTO wallet_ledger (user_id, amount, balance_after, kind, ref_type, ref_id, operator_id, remark, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		e.UserID, -applied, after, string(e.Kind), e.RefType, e.RefID, operator, e.Remark, now); err != nil {
		if db.IsUniqueViolation(err) {
			return 0, 0, apperr.ErrDuplicate
		}
		return 0, 0, err
	}
	if _, err := tx.Exec(ctx, `UPDATE wallet_accounts SET balance = $2, updated_at = $3 WHERE user_id = $1`, e.UserID, after, now); err != nil {
		return 0, 0, err
	}
	return applied, after, nil
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go vet ./internal/wallet/ && go test -race ./internal/wallet/ 2>&1 | tail -5`
Expected: `ok`（含并发用例）。

- [ ] **Step 5: 提交**

```bash
git add internal/wallet/wallet.go internal/wallet/wallet_test.go
git commit -m "feat(wallet): ApplyUpTo best-effort debit for referral clawback

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 3: `referral` 包骨架：`Deps`、常量、邀请码生成 / 规范化、`EnsureCode / LookupCode`、费率版本 `ConfigAt / AddRateVersion`、指纹

**Files:**
- Create: `internal/referral/referral.go`、`internal/referral/code.go`、`internal/referral/config.go`、`internal/referral/fingerprint.go`、`internal/referral/helpers_test.go`、`internal/referral/code_test.go`、`internal/referral/config_test.go`、`internal/referral/fingerprint_test.go`

**Interfaces:**
- Consumes: Task 1 的表；`wallet.Entry / Apply / ApplyUpTo`；`audit.Record / Actor`；`clock.Clock`；`db.Querier / WithTx / IsUniqueViolation`。
- Produces（`package referral`）：
  - 常量：`CodeLen = 8`、`CodeAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"`、`CodeRetries = 5`、`InviteStoreDays = 30`、`ScanLookback = time.Hour`、`ScanLag = 2 * time.Minute`、`ScanBatch = 500`、`ReleaseBatch = 200`、`BurstThreshold = 20`、`SameIPWindow = 24 * time.Hour`、`MaxDevicesPerUser = 10`、`InviterWindow = 30 * 24 * time.Hour`、`MaxUserAgent = 200`。
  - 事件 / 状态常量：`EventAccrue = "accrue"`、`EventRefund`、`EventSettle`、`EventClawback`、`EventDebtAdd = "debt_add"`、`EventDebtOffset = "debt_offset"`、`EventVoid`、`EventCapped`；`StatusPending / StatusSettled / StatusVoid`；`BindingActive = "active"`、`BindingHeld`、`BindingRevoked`；`AccountActive / AccountFrozen`；`FlagSameIP = "same_ip"`、`FlagSameDevice`、`FlagBurst`、`FlagSameIdentity`。
  - 接口与类型：
    ```go
    type WalletOps interface {
        Apply(ctx context.Context, tx pgx.Tx, e wallet.Entry) (int64, error)
        ApplyUpTo(ctx context.Context, tx pgx.Tx, e wallet.Entry) (applied, after int64, err error)
    }
    type UserState struct { Status, KYCStatus, Phone, LastLoginIP string; LastLoginAt *time.Time }
    type UserLookup interface {
        UserState(ctx context.Context, q db.Querier, id int64) (UserState, error)
        HasDevice(ctx context.Context, q db.Querier, userID int64, deviceID string) (bool, error)
        VerifiedFingerprints(ctx context.Context, q db.Querier, userIDs []int64) (map[int64]string, error)
    }
    type OrderLookup interface {
        OrderForReferral(ctx context.Context, q db.Querier, orderID string) (userID int64, scheduledStartAt *time.Time, err error)
    }
    type Deps struct { Pool *pgxpool.Pool; Clock clock.Clock; Log *slog.Logger; Wallet WalletOps; Users UserLookup; Orders OrderLookup }
    type Service struct{ d Deps }
    func NewService(d Deps) *Service
    type Account struct { UserID int64 `json:"user_id"`; Code string `json:"code"`; Status string `json:"status"`; FrozenReason string `json:"frozen_reason"`; Debt int64 `json:"debt"`; CreatedAt time.Time `json:"created_at"` }
    type RateVersion struct { ID int64 `json:"id"`; RateBP int `json:"rate_bp"`; MaxPerInvitee int64 `json:"max_per_invitee"`; MaxPerInviter30d int64 `json:"max_per_inviter_30d"`; HoldDays int `json:"hold_days"`; PeriodDays int `json:"period_days"`; EffectiveFrom time.Time `json:"effective_from"`; Note string `json:"note"`; CreatedBy *int64 `json:"created_by"`; CreatedAt time.Time `json:"created_at"` }
    type RateInput struct { RateBP int; MaxPerInvitee, MaxPerInviter30d int64; HoldDays, PeriodDays int; EffectiveFrom *time.Time; Note string }
    ```
  - 函数：`GenerateCode() string`、`NormalizeCode(s string) (code string, ok bool)`、`MaskPhone(phone string) string`、`(s *Service) EnsureCode(ctx, userID int64) (Account, error)`、`(s *Service) LookupCode(ctx, code string) (valid bool, inviterMasked string, err error)`、`(s *Service) ConfigAt(ctx, q db.Querier, t time.Time) (RateVersion, error)`、`(s *Service) CurrentConfig(ctx) (RateVersion, error)`、`(s *Service) ListRateVersions(ctx) ([]RateVersion, error)`、`(s *Service) AddRateVersion(ctx, actor audit.Actor, in RateInput) (RateVersion, error)`、`DeriveFingerprintKey(secret []byte) ([]byte, error)`、`Fingerprint(key []byte, kind, number string) string`。
  - 测试 helper（`helpers_test.go`，供 Task 5～11 的测试复用）：`newEnv(t) *env`，`env{t, ctx, pool, clk *clock.Fake, svc *referral.Service, wallet *wallet.Service, users *fakeUsers, orders *fakeOrders}`；`fakeUsers`（map 实现 `UserLookup`，但 `UserState` 直接查 `users` 表 + 内存 `devices map[int64]map[string]bool` + `fps map[int64]string`）；`fakeOrders`（map `orderID → {userID, start *time.Time}`，缺失返回 `apperr.ErrNotFound`）；`e.user(phone string, verified bool) int64`（`dbtest.CreateUser`）；`e.pay(userID int64, order string, amount int64) int64`（在 `clk.Now()` 写一条 `order_pay` 流水并登记到 `fakeOrders`，返回流水 id）；`e.refund(userID, refID string, amount int64) int64`；`e.bind(inviter, invitee int64) int64`（`EnsureCode` + 直接 `INSERT referral_bindings` 用当前版本）；`e.ledgerSum(userID int64, kind string) int64`；`e.events(order string) []string`（kind 列表）。

- [ ] **Step 1: 写失败的测试**

`internal/referral/helpers_test.go`：

```go
package referral_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/referral"
	"spark-cloud/internal/wallet"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

type fakeUsers struct {
	pool    *pgxpool.Pool
	devices map[int64]map[string]bool
	fps     map[int64]string
}

func (f *fakeUsers) UserState(ctx context.Context, q db.Querier, id int64) (referral.UserState, error) {
	var u referral.UserState
	err := q.QueryRow(ctx, `SELECT status, kyc_status, phone, last_login_ip, last_login_at FROM users WHERE id = $1`, id).
		Scan(&u.Status, &u.KYCStatus, &u.Phone, &u.LastLoginIP, &u.LastLoginAt)
	if db.IsNoRows(err) {
		return u, apperr.ErrNotFound
	}
	return u, err
}
func (f *fakeUsers) HasDevice(_ context.Context, _ db.Querier, userID int64, deviceID string) (bool, error) {
	return f.devices[userID][deviceID], nil
}
func (f *fakeUsers) VerifiedFingerprints(_ context.Context, _ db.Querier, ids []int64) (map[int64]string, error) {
	out := map[int64]string{}
	for _, id := range ids {
		if fp := f.fps[id]; fp != "" {
			out[id] = fp
		}
	}
	return out, nil
}

type fakeOrder struct {
	userID int64
	start  *time.Time
}
type fakeOrders struct{ m map[string]fakeOrder }

func (f *fakeOrders) OrderForReferral(_ context.Context, _ db.Querier, id string) (int64, *time.Time, error) {
	o, ok := f.m[id]
	if !ok {
		return 0, nil, apperr.ErrNotFound
	}
	return o.userID, o.start, nil
}

type env struct {
	t      *testing.T
	ctx    context.Context
	pool   *pgxpool.Pool
	clk    *clock.Fake
	svc    *referral.Service
	wallet *wallet.Service
	users  *fakeUsers
	orders *fakeOrders
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := dbtest.New(t)
	clk := clock.NewFake(dbtest.T0)
	w := wallet.NewService(pool, clk)
	users := &fakeUsers{pool: pool, devices: map[int64]map[string]bool{}, fps: map[int64]string{}}
	orders := &fakeOrders{m: map[string]fakeOrder{}}
	svc := referral.NewService(referral.Deps{Pool: pool, Clock: clk, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Wallet: w, Users: users, Orders: orders})
	return &env{t: t, ctx: context.Background(), pool: pool, clk: clk, svc: svc, wallet: w, users: users, orders: orders}
}

func (e *env) user(phone string, verified bool) int64 { return dbtest.CreateUser(e.t, e.pool, phone, verified) }

// pay 在当前假时刻写一条被邀请人的 order_pay 流水并登记订单；返回流水 id。
func (e *env) pay(userID int64, order string, amount int64) int64 { return e.payAt(userID, order, amount, nil) }

func (e *env) payAt(userID int64, order string, amount int64, start *time.Time) int64 {
	e.t.Helper()
	dbtest.Fund(e.t, e.pool, userID, amount)
	var id int64
	if err := db.WithTx(e.ctx, e.pool, func(tx pgx.Tx) error {
		_, err := e.wallet.Apply(e.ctx, tx, wallet.Entry{UserID: userID, Amount: -amount, Kind: wallet.KindOrderPay, RefType: "order", RefID: order, Remark: "新购 ins-x"})
		if err != nil {
			return err
		}
		return tx.QueryRow(e.ctx, `SELECT id FROM wallet_ledger WHERE kind = 'order_pay' AND ref_id = $1`, order).Scan(&id)
	}); err != nil {
		e.t.Fatalf("pay: %v", err)
	}
	e.orders.m[order] = fakeOrder{userID: userID, start: start}
	return id
}

func (e *env) refund(userID int64, refID string, amount int64) int64 {
	e.t.Helper()
	var id int64
	if err := db.WithTx(e.ctx, e.pool, func(tx pgx.Tx) error {
		_, err := e.wallet.Apply(e.ctx, tx, wallet.Entry{UserID: userID, Amount: amount, Kind: wallet.KindRefund, RefType: "order", RefID: refID, Remark: "退款"})
		if err != nil {
			return err
		}
		return tx.QueryRow(e.ctx, `SELECT id FROM wallet_ledger WHERE kind = 'refund' AND ref_id = $1`, refID).Scan(&id)
	}); err != nil {
		e.t.Fatalf("refund: %v", err)
	}
	return id
}

// bind 直接按当前费率版本插入一条 active 绑定（Bind 的业务逻辑由 Task 5 单独测）。
func (e *env) bind(inviter, invitee int64) int64 {
	e.t.Helper()
	if _, err := e.svc.EnsureCode(e.ctx, inviter); err != nil {
		e.t.Fatal(err)
	}
	now := e.clk.Now()
	var id int64
	if err := e.pool.QueryRow(e.ctx, `INSERT INTO referral_bindings (inviter_id, invitee_id, code, status, rate_version_id, bound_at, rebate_until, created_at, updated_at)
		SELECT $1, $2, a.code, 'active', v.id, $3, $3 + make_interval(days => v.period_days), $3, $3
		FROM referral_accounts a, LATERAL (SELECT id, period_days FROM referral_rate_versions WHERE effective_from <= $3 ORDER BY effective_from DESC LIMIT 1) v
		WHERE a.user_id = $1 RETURNING id`, inviter, invitee, now).Scan(&id); err != nil {
		e.t.Fatalf("bind: %v", err)
	}
	return id
}

func (e *env) ledgerSum(userID int64, kind string) int64 {
	e.t.Helper()
	var sum int64
	if err := e.pool.QueryRow(e.ctx, `SELECT COALESCE(sum(amount), 0) FROM wallet_ledger WHERE user_id = $1 AND kind = $2`, userID, kind).Scan(&sum); err != nil {
		e.t.Fatal(err)
	}
	return sum
}

func (e *env) events(order string) []string {
	e.t.Helper()
	rows, err := e.pool.Query(e.ctx, `SELECT kind FROM referral_events WHERE order_id = $1 ORDER BY id`, order)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, k)
	}
	return out
}

func (e *env) reward(order string) map[string]any {
	e.t.Helper()
	var status string
	var reward, cap, settled, clawed, refunded int64
	var settleAfter time.Time
	if err := e.pool.QueryRow(e.ctx, `SELECT status, reward, reward_cap, settled_amount, clawed_amount, refunded_amount, settle_after FROM referral_rewards WHERE order_id = $1`, order).
		Scan(&status, &reward, &cap, &settled, &clawed, &refunded, &settleAfter); err != nil {
		e.t.Fatalf("reward %s: %v", order, err)
	}
	return map[string]any{"status": status, "reward": reward, "cap": cap, "settled": settled, "clawed": clawed, "refunded": refunded, "settle_after": settleAfter}
}

func (e *env) debt(userID int64) int64 {
	e.t.Helper()
	var d int64
	if err := e.pool.QueryRow(e.ctx, `SELECT debt FROM referral_accounts WHERE user_id = $1`, userID).Scan(&d); err != nil {
		e.t.Fatal(err)
	}
	return d
}
```

（`pgx` 导入 `github.com/jackc/pgx/v5`。）

`internal/referral/code_test.go`：

```go
package referral_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"spark-cloud/internal/referral"
)

func TestGenerateCodeShapeAndNormalize(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		c := referral.GenerateCode()
		if len(c) != 8 || strings.Trim(c, referral.CodeAlphabet) != "" {
			t.Fatalf("bad code %q", c)
		}
		seen[c] = true
	}
	if len(seen) < 199 {
		t.Fatalf("codes not random enough: %d distinct", len(seen))
	}
	cases := map[string]string{
		" ab0lio-z ": "", // 含 '-' 非法
		"abcdefgh":   "ABCDEFGH",
		" o1lI0Zy9 ": "01110ZY9",
		"ABCDEFG":    "",   // 7 位
		"ABCDEFGHJ":  "",   // 9 位
		"ABCD EFGH":  "ABCDEFGH",
		"abcdefgu":   "",   // U 不在字母表
		"":           "",
	}
	for in, want := range cases {
		got, ok := referral.NormalizeCode(in)
		if (want == "") == ok || got != want {
			t.Errorf("NormalizeCode(%q) = %q %v, want %q", in, got, ok, want)
		}
	}
	if referral.MaskPhone("13812345678") != "138****5678" || referral.MaskPhone("12345") != "123****" {
		t.Fatal("MaskPhone")
	}
}

func TestEnsureCodeIdempotentUnderConcurrency(t *testing.T) {
	e := newEnv(t)
	uid := e.user("13800000001", false)
	var wg sync.WaitGroup
	codes := make([]string, 10)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a, err := e.svc.EnsureCode(context.Background(), uid)
			if err != nil {
				t.Error(err)
				return
			}
			codes[i] = a.Code
		}(i)
	}
	wg.Wait()
	for _, c := range codes {
		if c != codes[0] || len(c) != 8 {
			t.Fatalf("codes = %v", codes)
		}
	}
	var n int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM referral_accounts WHERE user_id = $1`, uid).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows = %d %v", n, err)
	}
	a, _ := e.svc.EnsureCode(e.ctx, uid)
	if a.Status != "active" || a.Debt != 0 || a.UserID != uid {
		t.Fatalf("account = %+v", a)
	}
}

func TestLookupCode(t *testing.T) {
	e := newEnv(t)
	inviter := e.user("13812345678", true)
	a, _ := e.svc.EnsureCode(e.ctx, inviter)
	valid, masked, err := e.svc.LookupCode(e.ctx, strings.ToLower(" "+a.Code+" "))
	if err != nil || !valid || masked != "138****5678" {
		t.Fatalf("lookup = %v %q %v", valid, masked, err)
	}
	if valid, _, err := e.svc.LookupCode(e.ctx, "ZZZZZZZZ"); err != nil || valid {
		t.Fatalf("unknown = %v %v", valid, err)
	}
	if valid, _, err := e.svc.LookupCode(e.ctx, "bad code!"); err != nil || valid {
		t.Fatalf("garbage = %v %v", valid, err)
	}
	e.pool.Exec(e.ctx, `UPDATE referral_accounts SET status = 'frozen' WHERE user_id = $1`, inviter)
	if valid, _, _ := e.svc.LookupCode(e.ctx, a.Code); valid {
		t.Fatal("frozen account must be invalid")
	}
	e.pool.Exec(e.ctx, `UPDATE referral_accounts SET status = 'active' WHERE user_id = $1`, inviter)
	e.pool.Exec(e.ctx, `UPDATE users SET status = 'banned' WHERE id = $1`, inviter)
	if valid, _, _ := e.svc.LookupCode(e.ctx, a.Code); valid {
		t.Fatal("banned owner must be invalid")
	}
}
```

`internal/referral/config_test.go`：

```go
package referral_test

import (
	"testing"
	"time"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/referral"
)

func TestConfigAtPicksLatestEffectiveVersion(t *testing.T) {
	e := newEnv(t)
	v0, err := e.svc.ConfigAt(e.ctx, e.pool, dbtest.T0)
	if err != nil || v0.RateBP != 1000 || v0.MaxPerInvitee != 50000 || v0.MaxPerInviter30d != 500000 || v0.HoldDays != 7 || v0.PeriodDays != 365 {
		t.Fatalf("seed = %+v %v", v0, err)
	}
	at := dbtest.T0.Add(2 * time.Hour)
	v1, err := e.svc.AddRateVersion(e.ctx, audit.Actor{AdminID: 7}, referral.RateInput{RateBP: 1500, MaxPerInvitee: 1000, MaxPerInviter30d: 20000, HoldDays: 0, PeriodDays: 30, EffectiveFrom: &at, Note: "活动"})
	if err != nil || v1.RateBP != 1500 || *v1.CreatedBy != 7 || !v1.EffectiveFrom.Equal(at) {
		t.Fatalf("add = %+v %v", v1, err)
	}
	// 边界：effective_from == t 取新版本；之前 1 纳秒仍是旧版本
	if v, _ := e.svc.ConfigAt(e.ctx, e.pool, at); v.ID != v1.ID {
		t.Fatalf("at boundary = %+v", v)
	}
	if v, _ := e.svc.ConfigAt(e.ctx, e.pool, at.Add(-time.Microsecond)); v.ID != v0.ID {
		t.Fatalf("before boundary = %+v", v)
	}
	// 当前版本（now = T0）仍是首版；列表按 effective_from 倒序
	if cur, _ := e.svc.CurrentConfig(e.ctx); cur.ID != v0.ID {
		t.Fatalf("current = %+v", cur)
	}
	list, _ := e.svc.ListRateVersions(e.ctx)
	if len(list) != 2 || list[0].ID != v1.ID {
		t.Fatalf("list = %+v", list)
	}
	// 审计
	var action string
	if err := e.pool.QueryRow(e.ctx, `SELECT action FROM audit_logs ORDER BY id DESC LIMIT 1`).Scan(&action); err != nil || action != "referral.config" {
		t.Fatalf("audit = %q %v", action, err)
	}
}

func TestAddRateVersionValidation(t *testing.T) {
	e := newEnv(t)
	past := dbtest.T0.Add(-time.Minute)
	bad := []referral.RateInput{
		{RateBP: 1000, MaxPerInvitee: 1, MaxPerInviter30d: 1, HoldDays: 7, PeriodDays: 365, EffectiveFrom: &past}, // 追溯
		{RateBP: 5001, MaxPerInvitee: 1, MaxPerInviter30d: 1, HoldDays: 7, PeriodDays: 365},
		{RateBP: -1, MaxPerInvitee: 1, MaxPerInviter30d: 1, HoldDays: 7, PeriodDays: 365},
		{RateBP: 1000, MaxPerInvitee: -1, MaxPerInviter30d: 1, HoldDays: 7, PeriodDays: 365},
		{RateBP: 1000, MaxPerInvitee: 1, MaxPerInviter30d: 1, HoldDays: 91, PeriodDays: 365},
		{RateBP: 1000, MaxPerInvitee: 1, MaxPerInviter30d: 1, HoldDays: 7, PeriodDays: 0},
		{RateBP: 1000, MaxPerInvitee: 1, MaxPerInviter30d: 1, HoldDays: 7, PeriodDays: 3651},
	}
	for i, in := range bad {
		_, err := e.svc.AddRateVersion(e.ctx, audit.Actor{AdminID: 1}, in)
		if ae := apperr.As(err); ae == nil || ae.Code != "referral_config_invalid" {
			t.Fatalf("case %d: want referral_config_invalid, got %v", i, err)
		}
	}
	// effective_from 省略 = now；5 分钟内的「过去」容差（表单提交延迟）也接受
	slightlyPast := dbtest.T0.Add(-4 * time.Minute)
	if _, err := e.svc.AddRateVersion(e.ctx, audit.Actor{AdminID: 1}, referral.RateInput{RateBP: 1000, MaxPerInvitee: 1, MaxPerInviter30d: 1, HoldDays: 7, PeriodDays: 365, EffectiveFrom: &slightlyPast}); err != nil {
		t.Fatalf("tolerance: %v", err)
	}
	v, err := e.svc.AddRateVersion(e.ctx, audit.Actor{AdminID: 1}, referral.RateInput{RateBP: 0, MaxPerInvitee: 0, MaxPerInviter30d: 0, HoldDays: 0, PeriodDays: 1})
	if err != nil || !v.EffectiveFrom.Equal(dbtest.T0) {
		t.Fatalf("default effective_from = %+v %v", v, err)
	}
}
```

`internal/referral/fingerprint_test.go`：

```go
package referral_test

import (
	"bytes"
	"testing"

	"spark-cloud/internal/referral"
)

func TestFingerprintDeterministicAndKeyed(t *testing.T) {
	k1, err := referral.DeriveFingerprintKey(bytes.Repeat([]byte("a"), 32))
	if err != nil || len(k1) != 32 {
		t.Fatal(err)
	}
	k2, _ := referral.DeriveFingerprintKey(bytes.Repeat([]byte("b"), 32))
	a := referral.Fingerprint(k1, "kyc", "110101199001011234")
	if a != referral.Fingerprint(k1, "kyc", "110101199001011234") || len(a) != 64 {
		t.Fatalf("not deterministic / wrong length: %q", a)
	}
	if a == referral.Fingerprint(k2, "kyc", "110101199001011234") {
		t.Fatal("different keys must differ")
	}
	if a == referral.Fingerprint(k1, "lic", "110101199001011234") || a == referral.Fingerprint(k1, "kyc", "110101199001011235") {
		t.Fatal("kind / number must be part of the input")
	}
	if _, err := referral.DeriveFingerprintKey(nil); err == nil {
		t.Fatal("empty secret must fail")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/referral/ 2>&1 | tail -5`
Expected: 编译错误（包不存在）。

- [ ] **Step 3: `referral.go`**

```go
// Package referral 实现邀请返利（计划 8）：邀请码、注册时绑定、按被邀请人实付消费计提、延迟发放、退款扣回与欠账、风险标记与后台裁决。
//
// 边界：只写 referral_* 表；只读 wallet_ledger（扫描）、users / kyc_records / user_devices（经 UserLookup）、orders（经 OrderLookup）；
// 资金变动全部经 wallet.Apply / ApplyUpTo，每笔都有唯一 ref_id（referral/<order>、referral/<order>-c<n>）。
//
// 加锁顺序（规格 §14）：
//   Bind（identity 事务内）：users(被邀请人, INSERT 持有) → referral_accounts(码主) FOR SHARE → 插 referral_bindings
//   计提 / 退款处理：referral_accounts(邀请人) FOR UPDATE → referral_rewards(订单) FOR UPDATE → [wallet_accounts(邀请人)，仅扣回]
//   发放：referral_accounts(邀请人) FOR UPDATE → referral_rewards(订单) FOR UPDATE SKIP LOCKED → wallet_accounts(邀请人)
//   冻结 / 解冻 / 撤销 / 通过：referral_accounts(邀请人) FOR UPDATE → referral_bindings → referral_rewards
//   OnKYCVerified（identity 事务内）：users(本人, 已持有) → referral_accounts(邀请人) FOR UPDATE → referral_bindings → referral_rewards
//   rental / billing 的扣款 / 退款事务只锁 wallet_accounts，从不锁 referral 表 → 等待链不成环。
package referral

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/wallet"
)

const (
	CodeLen         = 8
	CodeAlphabet    = "0123456789ABCDEFGHJKMNPQRSTVWXYZ" // Crockford Base32，无 I L O U
	CodeRetries     = 5
	InviteStoreDays = 30 // 控制台 localStorage 归因窗口（R7）

	ScanLookback = time.Hour
	ScanLag      = 2 * time.Minute
	ScanBatch    = 500
	ReleaseBatch = 200

	BurstThreshold    = 20
	SameIPWindow      = 24 * time.Hour
	MaxDevicesPerUser = 10
	InviterWindow     = 30 * 24 * time.Hour // max_per_inviter_30d 的窗口
	MaxUserAgent      = 200

	// 费率版本 effective_from 允许的「过去」容差（表单从打开到提交的延迟）
	EffectiveTolerance = 5 * time.Minute
)

const (
	EventAccrue     = "accrue"
	EventRefund     = "refund"
	EventSettle     = "settle"
	EventClawback   = "clawback"
	EventDebtAdd    = "debt_add"
	EventDebtOffset = "debt_offset"
	EventVoid       = "void"
	EventCapped     = "capped"

	StatusPending = "pending"
	StatusSettled = "settled"
	StatusVoid    = "void"

	BindingActive  = "active"
	BindingHeld    = "held"
	BindingRevoked = "revoked"

	AccountActive = "active"
	AccountFrozen = "frozen"

	FlagSameIP       = "same_ip"
	FlagSameDevice   = "same_device"
	FlagBurst        = "burst"
	FlagSameIdentity = "same_identity"

	RefType = "referral" // wallet_ledger.ref_type
)

type WalletOps interface {
	Apply(ctx context.Context, tx pgx.Tx, e wallet.Entry) (int64, error)
	ApplyUpTo(ctx context.Context, tx pgx.Tx, e wallet.Entry) (applied, after int64, err error)
}

// UserState 是 referral 需要知道的用户事实（由 identity 实现）。
type UserState struct {
	Status      string
	KYCStatus   string
	Phone       string
	LastLoginIP string
	LastLoginAt *time.Time
}

type UserLookup interface {
	UserState(ctx context.Context, q db.Querier, id int64) (UserState, error)
	HasDevice(ctx context.Context, q db.Querier, userID int64, deviceID string) (bool, error)
	// VerifiedFingerprints 返回每个用户最新一条 verified 且指纹非空的实名记录的指纹；没有的用户不在 map 里。
	VerifiedFingerprints(ctx context.Context, q db.Querier, userIDs []int64) (map[int64]string, error)
}

type OrderLookup interface {
	// OrderForReferral 返回订单的用户与（预订单的）计划开始时刻；不存在返回 apperr.ErrNotFound。
	OrderForReferral(ctx context.Context, q db.Querier, orderID string) (userID int64, scheduledStartAt *time.Time, err error)
}

type Deps struct {
	Pool   *pgxpool.Pool
	Clock  clock.Clock
	Log    *slog.Logger
	Wallet WalletOps
	Users  UserLookup
	Orders OrderLookup
}

type Service struct {
	d Deps
}

func NewService(d Deps) *Service {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	return &Service{d: d}
}

type Account struct {
	UserID       int64     `json:"user_id"`
	Code         string    `json:"code"`
	Status       string    `json:"status"`
	FrozenReason string    `json:"frozen_reason"`
	Debt         int64     `json:"debt"`
	CreatedAt    time.Time `json:"created_at"`
}

const accountCols = `user_id, code, status, frozen_reason, debt, created_at`

func scanAccount(row pgx.Row) (Account, error) {
	var a Account
	err := row.Scan(&a.UserID, &a.Code, &a.Status, &a.FrozenReason, &a.Debt, &a.CreatedAt)
	return a, err
}

// lockAccount 按加锁顺序的第一步锁住邀请人账户行；没有账户行（从未生成过码，因而不可能有绑定）返回 ErrNotFound。
func lockAccount(ctx context.Context, tx pgx.Tx, userID int64) (Account, error) {
	a, err := scanAccount(tx.QueryRow(ctx, `SELECT `+accountCols+` FROM referral_accounts WHERE user_id = $1 FOR UPDATE`, userID))
	if db.IsNoRows(err) {
		return a, apperr.ErrNotFound
	}
	return a, err
}

// addEvent 追加一条事件；source 非 0 时作为幂等键。
func addEvent(ctx context.Context, tx pgx.Tx, now time.Time, order string, bindingID int64, kind string, source, ledger, amount int64, detail string) error {
	var src, led *int64
	if source != 0 {
		src = &source
	}
	if ledger != 0 {
		led = &ledger
	}
	_, err := tx.Exec(ctx, `INSERT INTO referral_events (order_id, binding_id, kind, source_ledger_id, wallet_ledger_id, amount, detail, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, order, bindingID, kind, src, led, amount, detail, now)
	return err
}
```

- [ ] **Step 4: `code.go`**

```go
package referral

import (
	"context"
	"crypto/rand"
	"strings"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
)

// GenerateCode 生成 8 位 Crockford Base32 随机码（crypto/rand）。
func GenerateCode() string {
	b := make([]byte, CodeLen)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand 失败意味着系统熵源损坏
	}
	out := make([]byte, CodeLen)
	for i, x := range b {
		out[i] = CodeAlphabet[int(x)%len(CodeAlphabet)]
	}
	return string(out)
}

// NormalizeCode 去空白、转大写、O→0、I/L→1；长度不是 8 或含字母表外字符返回 ok=false。
func NormalizeCode(s string) (string, bool) {
	s = strings.ToUpper(strings.Join(strings.Fields(s), ""))
	s = strings.NewReplacer("O", "0", "I", "1", "L", "1").Replace(s)
	if len(s) != CodeLen {
		return "", false
	}
	for i := 0; i < len(s); i++ {
		if !strings.ContainsRune(CodeAlphabet, rune(s[i])) {
			return "", false
		}
	}
	return s, true
}

// MaskPhone 138****5678；非 11 位时保留前 3 位 + ****。
func MaskPhone(phone string) string {
	if len(phone) == 11 {
		return phone[:3] + "****" + phone[7:]
	}
	if len(phone) > 3 {
		phone = phone[:3]
	}
	return phone + "****"
}

// EnsureCode 返回用户的邀请账户，没有则生成码并插入（撞码重试 CodeRetries 次）；并发调用只会建一行。
func (s *Service) EnsureCode(ctx context.Context, userID int64) (Account, error) {
	a, err := scanAccount(s.d.Pool.QueryRow(ctx, `SELECT `+accountCols+` FROM referral_accounts WHERE user_id = $1`, userID))
	if err == nil {
		return a, nil
	}
	if !db.IsNoRows(err) {
		return a, err
	}
	now := s.d.Clock.Now()
	for i := 0; i < CodeRetries; i++ {
		// ON CONFLICT (user_id) DO NOTHING 处理并发的首次调用；撞到 code 唯一索引仍会报错 → 换码重试
		_, err := s.d.Pool.Exec(ctx, `INSERT INTO referral_accounts (user_id, code, created_at, updated_at) VALUES ($1, $2, $3, $3)
			ON CONFLICT (user_id) DO NOTHING`, userID, GenerateCode(), now)
		if err != nil {
			if db.IsUniqueViolation(err) {
				continue
			}
			if db.IsForeignKeyViolation(err) {
				return a, apperr.ErrNotFound
			}
			return a, err
		}
		return scanAccount(s.d.Pool.QueryRow(ctx, `SELECT `+accountCols+` FROM referral_accounts WHERE user_id = $1`, userID))
	}
	return a, apperr.New("invite_code_collision", "邀请码生成失败，请重试", 500)
}

// LookupCode 匿名查码：码存在、账户未冻结、码主未封禁才有效；无效不区分原因。
func (s *Service) LookupCode(ctx context.Context, code string) (bool, string, error) {
	c, ok := NormalizeCode(code)
	if !ok {
		return false, "", nil
	}
	var phone string
	err := s.d.Pool.QueryRow(ctx, `SELECT u.phone FROM referral_accounts a JOIN users u ON u.id = a.user_id
		WHERE a.code = $1 AND a.status = 'active' AND u.status = 'active'`, c).Scan(&phone)
	if db.IsNoRows(err) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	return true, MaskPhone(phone), nil
}
```

（`code.go` 的导入块不需要 `pgx`。）

- [ ] **Step 5: `config.go`**

```go
package referral

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
)

type RateVersion struct {
	ID               int64     `json:"id"`
	RateBP           int       `json:"rate_bp"`
	MaxPerInvitee    int64     `json:"max_per_invitee"`
	MaxPerInviter30d int64     `json:"max_per_inviter_30d"`
	HoldDays         int       `json:"hold_days"`
	PeriodDays       int       `json:"period_days"`
	EffectiveFrom    time.Time `json:"effective_from"`
	Note             string    `json:"note"`
	CreatedBy        *int64    `json:"created_by"`
	CreatedAt        time.Time `json:"created_at"`
}

type RateInput struct {
	RateBP           int
	MaxPerInvitee    int64
	MaxPerInviter30d int64
	HoldDays         int
	PeriodDays       int
	EffectiveFrom    *time.Time // nil = now
	Note             string
}

const rateCols = `id, rate_bp, max_per_invitee, max_per_inviter_30d, hold_days, period_days, effective_from, note, created_by, created_at`

func scanRate(row pgx.Row) (RateVersion, error) {
	var v RateVersion
	err := row.Scan(&v.ID, &v.RateBP, &v.MaxPerInvitee, &v.MaxPerInviter30d, &v.HoldDays, &v.PeriodDays, &v.EffectiveFrom, &v.Note, &v.CreatedBy, &v.CreatedAt)
	return v, err
}

// ConfigAt 返回在 t 时刻生效的版本（effective_from <= t 的最新一条；表由迁移保证非空）。
func (s *Service) ConfigAt(ctx context.Context, q db.Querier, t time.Time) (RateVersion, error) {
	return scanRate(q.QueryRow(ctx, `SELECT `+rateCols+` FROM referral_rate_versions WHERE effective_from <= $1 ORDER BY effective_from DESC, id DESC LIMIT 1`, t))
}

func (s *Service) CurrentConfig(ctx context.Context) (RateVersion, error) {
	return s.ConfigAt(ctx, s.d.Pool, s.d.Clock.Now())
}

func (s *Service) ListRateVersions(ctx context.Context) ([]RateVersion, error) {
	rows, err := s.d.Pool.Query(ctx, `SELECT `+rateCols+` FROM referral_rate_versions ORDER BY effective_from DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (RateVersion, error) { return scanRate(r) })
}

func configInvalid(format string, a ...any) error {
	return apperr.New("referral_config_invalid", fmt.Sprintf(format, a...), 400)
}

// AddRateVersion 新增一个费率版本（R14：不能追溯生效）；写审计 referral.config（含旧版本与新版本全部字段）。
func (s *Service) AddRateVersion(ctx context.Context, actor audit.Actor, in RateInput) (RateVersion, error) {
	now := s.d.Clock.Now()
	switch {
	case in.RateBP < 0 || in.RateBP > 5000:
		return RateVersion{}, configInvalid("返利比例需在 0%%～50%% 之间")
	case in.MaxPerInvitee < 0:
		return RateVersion{}, configInvalid("单个被邀请人上限不能为负")
	case in.MaxPerInviter30d < 0:
		return RateVersion{}, configInvalid("单个邀请人 30 天上限不能为负")
	case in.HoldDays < 0 || in.HoldDays > 90:
		return RateVersion{}, configInvalid("发放延迟需在 0～90 天之间")
	case in.PeriodDays < 1 || in.PeriodDays > 3650:
		return RateVersion{}, configInvalid("返利期需在 1～3650 天之间")
	}
	from := now
	if in.EffectiveFrom != nil {
		from = in.EffectiveFrom.UTC()
		if from.Before(now.Add(-EffectiveTolerance)) {
			return RateVersion{}, configInvalid("生效时间不能早于当前时间")
		}
	}
	var v RateVersion
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		old, err := s.ConfigAt(ctx, tx, now)
		if err != nil {
			return err
		}
		var by *int64
		if actor.AdminID != 0 {
			by = &actor.AdminID
		}
		v, err = scanRate(tx.QueryRow(ctx, `INSERT INTO referral_rate_versions (rate_bp, max_per_invitee, max_per_inviter_30d, hold_days, period_days, effective_from, note, created_by, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING `+rateCols,
			in.RateBP, in.MaxPerInvitee, in.MaxPerInviter30d, in.HoldDays, in.PeriodDays, from, in.Note, by, now))
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("referral.config", "referral_rate_version", fmt.Sprint(v.ID), map[string]any{"old": old, "new": v}))
	})
	return v, err
}
```

- [ ] **Step 6: `fingerprint.go`**

```go
package referral

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
)

// DeriveFingerprintKey 从 SPARK_SECRET_KEY 派生实名指纹的 HMAC 密钥（HKDF-SHA256，info "referral-fingerprint"）。
func DeriveFingerprintKey(secret []byte) ([]byte, error) {
	if len(secret) == 0 {
		return nil, errors.New("referral: empty secret")
	}
	return hkdf.Key(sha256.New, secret, nil, "referral-fingerprint", 32)
}

// Fingerprint = hex(HMAC-SHA256(key, kind + ":" + number))；kind 为 "kyc"（身份证号）或 "lic"（统一社会信用代码）。
func Fingerprint(key []byte, kind, number string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(kind + ":" + number))
	return hex.EncodeToString(m.Sum(nil))
}
```

- [ ] **Step 7: 运行测试确认通过**

Run: `go vet ./internal/referral/ && go test -race ./internal/referral/ 2>&1 | tail -5`
Expected: `ok`。

- [ ] **Step 8: 提交**

```bash
git add internal/referral/
git commit -m "feat(referral): package skeleton, invite codes, rate versions, kyc fingerprint

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 4: `identity.LoginWith`（`is_new` / `invite_code` / 设备 / 最近登录）、`user_devices`、`ReferralHook`、`UserLookup` 实现

**Files:**
- Create: `internal/identity/referral.go`
- Modify: `internal/identity/service.go`（`Deps`、`SetReferral`）、`internal/identity/auth.go`（`LoginWith`、`Login` 包装）
- Test: `internal/identity/auth_test.go`（追加）

**Interfaces:**
- Consumes: Task 3 的 `referral.BindRequest / BindResult / UserState`（`BindRequest / BindResult` 在 Task 5 定义——本任务先在 `internal/referral/bind.go` 放**只含类型**的文件，Task 5 补方法）。
- Produces:
  - `internal/referral/bind.go`（类型部分）：
    ```go
    type BindRequest struct { Code string; InviteeID int64; IP, DeviceID, UserAgent string; Now time.Time }
    type BindResult struct { Bound bool `json:"bound"`; Reason string `json:"reason"`; Inviter string `json:"inviter,omitempty"` }
    const ( ReasonOK = "ok"; ReasonInvalidCode = "invalid_code"; ReasonInviterFrozen = "inviter_frozen"; ReasonExistingUser = "existing_user" )
    ```
  - `identity.ReferralHook interface { Bind(ctx, tx pgx.Tx, req referral.BindRequest) (referral.BindResult, error); OnKYCVerified(ctx, tx pgx.Tx, userID int64, fingerprint string) error }`；`Deps.Referral ReferralHook`、`Deps.FingerprintKey []byte`；`func (s *Service) SetReferral(h ReferralHook)`。
  - `identity.LoginRequest{Phone, Code, InviteCode, IP, DeviceID, UserAgent string}`、`identity.LoginResult{User; IsNew bool `json:"-"`; Invite *referral.BindResult `json:"invite,omitempty"`}`；`func (s *Service) LoginWith(ctx, req LoginRequest) (string, LoginResult, error)`；`Login(ctx, phone, code)` 保持签名，内部调 `LoginWith`。
  - 行为：`users` 的 `INSERT … ON CONFLICT (phone) DO UPDATE SET updated_at = EXCLUDED.updated_at, last_login_ip = $3, last_login_at = $2 RETURNING <userCols>, (xmax = 0) AS is_new`；新插入的行也写 `last_login_ip / last_login_at`；`DeviceID` 是 UUID 格式（`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`）时 `INSERT INTO user_devices … ON CONFLICT (user_id, device_id) DO UPDATE SET last_seen_at`，然后删超出 10 台的最旧行；`IsNew && InviteCode != "" && Referral != nil` → `Bind`，业务失败不回滚；`!IsNew && InviteCode != ""` → `Invite = {Bound: false, Reason: "existing_user"}`；`Referral == nil` 且带码 → `{false, "invalid_code"}`。
  - `identity` 实现 `referral.UserLookup`：`UserState(ctx, q, id)`（`SELECT status, kyc_status, phone, last_login_ip, last_login_at FROM users WHERE id = $1`，不存在 → `apperr.ErrNotFound`）、`HasDevice(ctx, q, userID, deviceID)`、`VerifiedFingerprints(ctx, q, ids)`（`SELECT DISTINCT ON (user_id) user_id, fingerprint FROM kyc_records WHERE user_id = ANY($1) AND result = 'verified' AND fingerprint <> '' ORDER BY user_id, id DESC`）。

- [ ] **Step 1: 写失败的测试**

`internal/identity/auth_test.go` 追加：

```go
type hookRec struct {
	calls []referral.BindRequest
	res   referral.BindResult
	err   error
	kyc   []string // fingerprints seen
}

func (h *hookRec) Bind(_ context.Context, _ pgx.Tx, req referral.BindRequest) (referral.BindResult, error) {
	h.calls = append(h.calls, req)
	return h.res, h.err
}
func (h *hookRec) OnKYCVerified(_ context.Context, _ pgx.Tx, _ int64, fp string) error {
	h.kyc = append(h.kyc, fp)
	return nil
}

const devA = "11111111-2222-4333-8444-555555555555"

func TestLoginWithIsNewDeviceAndLastLogin(t *testing.T) {
	e := newEnv(t, "000000")
	hook := &hookRec{res: referral.BindResult{Bound: true, Reason: "ok", Inviter: "138****0000"}}
	e.svc.SetReferral(hook)
	ctx := context.Background()
	send := func(phone string) {
		t.Helper()
		e.clock.Advance(61 * time.Second)
		if err := e.svc.SendLoginCode(ctx, phone, "9.9.9.9"); err != nil {
			t.Fatal(err)
		}
	}
	send("13800000011")
	_, r, err := e.svc.LoginWith(ctx, identity.LoginRequest{Phone: "13800000011", Code: "000000", InviteCode: "ABCDEFGH", IP: "1.2.3.4", DeviceID: devA, UserAgent: "ua"})
	if err != nil || !r.IsNew || r.Invite == nil || !r.Invite.Bound || r.Invite.Inviter != "138****0000" {
		t.Fatalf("first login = %+v %v", r, err)
	}
	if len(hook.calls) != 1 || hook.calls[0].Code != "ABCDEFGH" || hook.calls[0].InviteeID != r.ID || hook.calls[0].IP != "1.2.3.4" || hook.calls[0].DeviceID != devA || !hook.calls[0].Now.Equal(e.clock.Now()) {
		t.Fatalf("bind req = %+v", hook.calls)
	}
	var ip string
	var at *time.Time
	if err := e.pool.QueryRow(ctx, `SELECT last_login_ip, last_login_at FROM users WHERE id = $1`, r.ID).Scan(&ip, &at); err != nil || ip != "1.2.3.4" || at == nil || !at.Equal(e.clock.Now()) {
		t.Fatalf("last login = %q %v %v", ip, at, err)
	}
	var devs int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM user_devices WHERE user_id = $1 AND device_id = $2`, r.ID, devA).Scan(&devs); err != nil || devs != 1 {
		t.Fatalf("devices = %d %v", devs, err)
	}
	// 老用户带码：不调用 Bind，reason existing_user；最近登录更新
	send("13800000011")
	_, r2, err := e.svc.LoginWith(ctx, identity.LoginRequest{Phone: "13800000011", Code: "000000", InviteCode: "ABCDEFGH", IP: "5.6.7.8"})
	if err != nil || r2.IsNew || r2.ID != r.ID || r2.Invite == nil || r2.Invite.Bound || r2.Invite.Reason != "existing_user" || len(hook.calls) != 1 {
		t.Fatalf("second login = %+v %v calls=%d", r2, err, len(hook.calls))
	}
	if err := e.pool.QueryRow(ctx, `SELECT last_login_ip FROM users WHERE id = $1`, r.ID).Scan(&ip); err != nil || ip != "5.6.7.8" {
		t.Fatalf("last ip = %q", ip)
	}
	// 不带码：Invite 为 nil
	send("13800000011")
	if _, r3, err := e.svc.LoginWith(ctx, identity.LoginRequest{Phone: "13800000011", Code: "000000"}); err != nil || r3.Invite != nil {
		t.Fatalf("no code = %+v %v", r3, err)
	}
	// 非 UUID 的设备 ID 不写；设备上限 10 台（删最旧）
	send("13800000011")
	e.svc.LoginWith(ctx, identity.LoginRequest{Phone: "13800000011", Code: "000000", DeviceID: "not-a-uuid"})
	for i := 0; i < 11; i++ {
		send("13800000011")
		e.svc.LoginWith(ctx, identity.LoginRequest{Phone: "13800000011", Code: "000000", DeviceID: fmt.Sprintf("00000000-0000-4000-8000-%012d", i)})
	}
	var n int
	var oldest bool
	e.pool.QueryRow(ctx, `SELECT count(*), bool_or(device_id = $2) FROM user_devices WHERE user_id = $1`, r.ID, devA).Scan(&n, &oldest)
	if n != 10 || oldest {
		t.Fatalf("devices after 12 logins = %d (oldest kept=%v)", n, oldest)
	}
	// 旧签名 Login 仍可用
	send("13800000011")
	if _, u, err := e.svc.Login(ctx, "13800000011", "000000"); err != nil || u.ID != r.ID {
		t.Fatalf("Login = %+v %v", u, err)
	}
}

func TestLoginWithBindBusinessFailureKeepsRegistration(t *testing.T) {
	e := newEnv(t, "000000")
	hook := &hookRec{res: referral.BindResult{Bound: false, Reason: "invalid_code"}}
	e.svc.SetReferral(hook)
	ctx := context.Background()
	e.svc.SendLoginCode(ctx, "13800000012", "1.1.1.1")
	_, r, err := e.svc.LoginWith(ctx, identity.LoginRequest{Phone: "13800000012", Code: "000000", InviteCode: "ZZZZZZZZ"})
	if err != nil || !r.IsNew || r.Invite == nil || r.Invite.Bound || r.Invite.Reason != "invalid_code" {
		t.Fatalf("= %+v %v", r, err)
	}
	if _, err := e.svc.GetUser(ctx, r.ID); err != nil {
		t.Fatalf("user must exist: %v", err)
	}
	// 数据库错误回滚注册
	hook.err = errors.New("boom")
	e.clock.Advance(61 * time.Second)
	e.svc.SendLoginCode(ctx, "13800000013", "1.1.1.1")
	if _, _, err := e.svc.LoginWith(ctx, identity.LoginRequest{Phone: "13800000013", Code: "000000", InviteCode: "ZZZZZZZZ"}); err == nil {
		t.Fatal("db error must propagate")
	}
	var n int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE phone = '13800000013'`).Scan(&n)
	if n != 0 {
		t.Fatal("registration must be rolled back")
	}
	// 没装 hook 时带码：invalid_code，注册成功
	e.svc.SetReferral(nil)
	e.clock.Advance(61 * time.Second)
	e.svc.SendLoginCode(ctx, "13800000014", "1.1.1.1")
	if _, r, err := e.svc.LoginWith(ctx, identity.LoginRequest{Phone: "13800000014", Code: "000000", InviteCode: "ABCDEFGH"}); err != nil || r.Invite == nil || r.Invite.Reason != "invalid_code" {
		t.Fatalf("no hook = %+v %v", r, err)
	}
}

func TestUserLookupImplementation(t *testing.T) {
	e := newEnv(t, "000000")
	ctx := context.Background()
	uid := dbtest.CreateUser(t, e.pool, "13800000015", true)
	e.pool.Exec(ctx, `INSERT INTO user_devices (user_id, device_id, first_seen_at, last_seen_at) VALUES ($1, $2, $3, $3)`, uid, devA, dbtest.T0)
	e.pool.Exec(ctx, `INSERT INTO kyc_records (user_id, kind, real_name, result, fingerprint, created_at, updated_at) VALUES ($1, 'personal', 'x', 'verified', 'fp-old', $2, $2), ($1, 'personal', 'x', 'verified', 'fp-new', $2, $2)`, uid, dbtest.T0)
	var lookup referral.UserLookup = e.svc
	st, err := lookup.UserState(ctx, e.pool, uid)
	if err != nil || st.Status != "active" || st.KYCStatus != "verified" || st.Phone != "13800000015" || st.LastLoginAt != nil {
		t.Fatalf("state = %+v %v", st, err)
	}
	if _, err := lookup.UserState(ctx, e.pool, 999999); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}
	if has, _ := lookup.HasDevice(ctx, e.pool, uid, devA); !has {
		t.Fatal("HasDevice")
	}
	if has, _ := lookup.HasDevice(ctx, e.pool, uid, "other"); has {
		t.Fatal("HasDevice other")
	}
	fps, err := lookup.VerifiedFingerprints(ctx, e.pool, []int64{uid, 999999})
	if err != nil || len(fps) != 1 || fps[uid] != "fp-new" {
		t.Fatalf("fps = %v %v", fps, err)
	}
}
```

（补导入 `fmt`、`github.com/jackc/pgx/v5`、`spark-cloud/internal/platform/dbtest`、`spark-cloud/internal/referral`。）

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/identity/ -run 'TestLoginWith|TestUserLookup' 2>&1 | tail -5`
Expected: 编译错误（`LoginWith`、`SetReferral`、`referral.BindRequest` 未定义）。

- [ ] **Step 3: `referral/bind.go` 类型、`identity` 的 `Deps` 与 `LoginWith`**

`internal/referral/bind.go`（本任务只放类型；Task 5 在同文件补 `Bind`）：

```go
package referral

import "time"

type BindRequest struct {
	Code      string
	InviteeID int64
	IP        string
	DeviceID  string
	UserAgent string
	Now       time.Time
}

type BindResult struct {
	Bound   bool   `json:"bound"`
	Reason  string `json:"reason"`
	Inviter string `json:"inviter,omitempty"` // 脱敏手机号
}

const (
	ReasonOK            = "ok"
	ReasonInvalidCode   = "invalid_code"
	ReasonInviterFrozen = "inviter_frozen"
	ReasonExistingUser  = "existing_user"
)
```

`internal/identity/service.go`：

```go
// ReferralHook 由 referral 模块实现（计划 8）：注册时绑定邀请关系、实名通过时同身份判定。nil 时两者都跳过。
type ReferralHook interface {
	Bind(ctx context.Context, tx pgx.Tx, req referral.BindRequest) (referral.BindResult, error)
	OnKYCVerified(ctx context.Context, tx pgx.Tx, userID int64, fingerprint string) error
}
```

`Deps` 加两个字段：

```go
	Referral       ReferralHook // 计划 8；装配时 referral 依赖 identity，故用 SetReferral 后置注入
	FingerprintKey []byte       // 计划 8：实名指纹 HMAC 密钥（referral.DeriveFingerprintKey）；空时不写指纹
```

```go
// SetReferral 后置注入邀请返利钩子（referral.Service 的构造需要 identity 先存在）。必须在开始服务前调用。
func (s *Service) SetReferral(h ReferralHook) { s.d.Referral = h }
```

`internal/identity/auth.go`：

```go
type LoginRequest struct {
	Phone, Code string
	InviteCode  string // 计划 8：仅新用户生效
	IP          string
	DeviceID    string // 请求头 X-Device-Id；非 UUID 形态忽略
	UserAgent   string
}

// LoginResult 是登录响应：用户字段平铺，invite 仅在本次带了 invite_code 时出现。
type LoginResult struct {
	User
	IsNew  bool                 `json:"-"`
	Invite *referral.BindResult `json:"invite,omitempty"`
}

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Login 校验验证码，不存在则创建用户，返回新的会话 token（计划 8 之前的签名，等价于不带邀请码 / 设备的 LoginWith）。
func (s *Service) Login(ctx context.Context, phone, code string) (string, User, error) {
	token, r, err := s.LoginWith(ctx, LoginRequest{Phone: phone, Code: code})
	return token, r.User, err
}

// LoginWith 校验验证码；首次登录即注册（is_new 由 INSERT … ON CONFLICT 的 xmax = 0 判断），新用户带邀请码时在同一事务里绑定。
// Bind 的业务失败（码无效 / 码主冻结）只体现在响应里，不影响注册；数据库错误回滚整个登录。
func (s *Service) LoginWith(ctx context.Context, req LoginRequest) (string, LoginResult, error) {
	now := s.d.Clock.Now()
	var (
		bizErr error
		res    LoginResult
		token  string
	)
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		var id int64
		var hash string
		var attempts int
		err := tx.QueryRow(ctx, `SELECT id, code_hash, attempts FROM sms_codes
			WHERE phone = $1 AND used_at IS NULL AND expires_at > $2
			ORDER BY id DESC LIMIT 1 FOR UPDATE`, req.Phone, now).Scan(&id, &hash, &attempts)
		if db.IsNoRows(err) {
			bizErr = apperr.ErrInvalidCode
			return nil
		}
		if err != nil {
			return err
		}
		if attempts >= codeMaxAttempts {
			bizErr = apperr.ErrInvalidCode
			return nil
		}
		if subtle.ConstantTimeCompare([]byte(hash), []byte(hashSecret(req.Phone+":"+req.Code))) != 1 {
			bizErr = apperr.ErrInvalidCode
			_, err := tx.Exec(ctx, `UPDATE sms_codes SET attempts = attempts + 1 WHERE id = $1`, id)
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE sms_codes SET used_at = $2 WHERE id = $1`, id, now); err != nil {
			return err
		}
		row := tx.QueryRow(ctx, `INSERT INTO users (phone, created_at, updated_at, last_login_ip, last_login_at) VALUES ($1, $2, $2, $3, $2)
			ON CONFLICT (phone) DO UPDATE SET updated_at = EXCLUDED.updated_at, last_login_ip = EXCLUDED.last_login_ip, last_login_at = EXCLUDED.last_login_at
			RETURNING `+userCols+`, (xmax = 0) AS is_new`, req.Phone, now, req.IP)
		var u User
		if err := row.Scan(&u.ID, &u.Phone, &u.Kind, &u.KYCStatus, &u.Status, &u.CreatedAt, &res.IsNew); err != nil {
			return err
		}
		res.User = u
		if u.Status == "banned" {
			bizErr = apperr.ErrBanned
			return nil
		}
		if uuidRe.MatchString(req.DeviceID) {
			if err := s.touchDevice(ctx, tx, u.ID, strings.ToLower(req.DeviceID), now); err != nil {
				return err
			}
		}
		if req.InviteCode != "" {
			switch {
			case !res.IsNew:
				res.Invite = &referral.BindResult{Bound: false, Reason: referral.ReasonExistingUser}
			case s.d.Referral == nil:
				res.Invite = &referral.BindResult{Bound: false, Reason: referral.ReasonInvalidCode}
			default:
				ua := req.UserAgent
				if len(ua) > referral.MaxUserAgent {
					ua = ua[:referral.MaxUserAgent]
				}
				r, err := s.d.Referral.Bind(ctx, tx, referral.BindRequest{Code: req.InviteCode, InviteeID: u.ID, IP: req.IP, DeviceID: strings.ToLower(req.DeviceID), UserAgent: ua, Now: now})
				if err != nil {
					return err
				}
				res.Invite = &r
			}
		}
		token = randx.Token()
		_, err = tx.Exec(ctx, `INSERT INTO sessions (token_hash, user_id, created_at, expires_at) VALUES ($1, $2, $3, $4)`,
			hashSecret(token), u.ID, now, now.Add(sessionTTL))
		return err
	})
	if err != nil {
		return "", LoginResult{}, err
	}
	if bizErr != nil {
		return "", LoginResult{}, bizErr
	}
	return token, res, nil
}

// touchDevice 记录设备；每用户最多 MaxDevicesPerUser 台，超出删最旧。
func (s *Service) touchDevice(ctx context.Context, tx pgx.Tx, userID int64, deviceID string, now time.Time) error {
	if _, err := tx.Exec(ctx, `INSERT INTO user_devices (user_id, device_id, first_seen_at, last_seen_at) VALUES ($1, $2, $3, $3)
		ON CONFLICT (user_id, device_id) DO UPDATE SET last_seen_at = EXCLUDED.last_seen_at`, userID, deviceID, now); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `DELETE FROM user_devices WHERE user_id = $1 AND device_id NOT IN
		(SELECT device_id FROM user_devices WHERE user_id = $1 ORDER BY last_seen_at DESC, device_id LIMIT $2)`, userID, referral.MaxDevicesPerUser)
	return err
}
```

（`Login` 原函数体删除；补导入 `strings`、`spark-cloud/internal/referral`。`ON CONFLICT … DO UPDATE` 对已存在的行也会设置 `last_login_*`，新插入的行由 `VALUES` 写入。）

- [ ] **Step 4: `identity/referral.go`（`UserLookup` 实现）**

```go
package identity

import (
	"context"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/referral"
)

// 计划 8：identity 作为 referral.UserLookup。

func (s *Service) UserState(ctx context.Context, q db.Querier, id int64) (referral.UserState, error) {
	var u referral.UserState
	err := q.QueryRow(ctx, `SELECT status, kyc_status, phone, last_login_ip, last_login_at FROM users WHERE id = $1`, id).
		Scan(&u.Status, &u.KYCStatus, &u.Phone, &u.LastLoginIP, &u.LastLoginAt)
	if db.IsNoRows(err) {
		return u, apperr.ErrNotFound
	}
	return u, err
}

func (s *Service) HasDevice(ctx context.Context, q db.Querier, userID int64, deviceID string) (bool, error) {
	var has bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM user_devices WHERE user_id = $1 AND device_id = $2)`, userID, deviceID).Scan(&has)
	return has, err
}

func (s *Service) VerifiedFingerprints(ctx context.Context, q db.Querier, userIDs []int64) (map[int64]string, error) {
	out := map[int64]string{}
	if len(userIDs) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `SELECT DISTINCT ON (user_id) user_id, fingerprint FROM kyc_records
		WHERE user_id = ANY($1) AND result = 'verified' AND fingerprint <> '' ORDER BY user_id, id DESC`, userIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var fp string
		if err := rows.Scan(&id, &fp); err != nil {
			return nil, err
		}
		out[id] = fp
	}
	return out, rows.Err()
}
```

- [ ] **Step 5: 运行测试确认通过**

Run: `go vet ./... && go test ./internal/identity/ ./internal/api/ ./internal/app/ 2>&1 | tail -8`
Expected: 全部 `ok`（`api` / `app` 继续用 `Login`，不受影响）。

- [ ] **Step 6: 提交**

```bash
git add internal/referral/bind.go internal/identity/
git commit -m "feat(identity): LoginWith (is_new, invite code hook, device, last login), user_devices, referral UserLookup

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 5: `referral.Bind`（风险标记、搁置、审计）

**Files:**
- Modify: `internal/referral/bind.go`（补 `Bind`）
- Test: `internal/referral/bind_test.go`（新建）

**Interfaces:**
- Consumes: Task 3 的 `Service`、`NormalizeCode`、`ConfigAt`、`UserLookup`、`addEvent`；Task 4 的 `BindRequest / BindResult / Reason*`、`MaxUserAgent`。
- Produces: `func (s *Service) Bind(ctx context.Context, tx pgx.Tx, req BindRequest) (BindResult, error)`，在调用方（identity）事务内：
  1. `NormalizeCode` 失败 → `{false, invalid_code}`。
  2. `SELECT a.user_id, a.status, u.status, u.phone FROM referral_accounts a JOIN users u ON u.id = a.user_id WHERE a.code = $1 FOR SHARE OF a`；不存在 → `invalid_code`；账户 `frozen` 或码主 `banned` → `inviter_frozen`；码主 == 被邀请人 → `invalid_code`（理论上不可能）。
  3. `v := ConfigAt(tx, Now)`；`rebate_until = Now + v.PeriodDays 天`。
  4. 标记：`same_ip`（`IP != "" && IP == inviter.LastLoginIP && LastLoginAt != nil && LastLoginAt.After(Now − 24h)`）、`same_device`（`DeviceID != "" && Users.HasDevice(inviter, DeviceID)`）、`burst`（`count(referral_bindings WHERE inviter_id = $1 AND bound_at > Now − 24h) >= BurstThreshold − 1`）。
  5. `status = held, hold_reason = same_device` 若含 `same_device`，否则 `active`。
  6. `INSERT referral_bindings`（`invitee_id` 唯一冲突 → `{false, invalid_code}`，不报错）；审计 `referral.bind`（`audit.Actor{UserID: invitee, IP}`，target `referral_binding/<id>`，diff `{inviter_id, code, flags, status}`）。
  7. 返回 `{true, ok, MaskPhone(inviter.Phone)}`。

- [ ] **Step 1: 写失败的测试**

`internal/referral/bind_test.go`：

```go
package referral_test

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/referral"
)

func (e *env) doBind(req referral.BindRequest) referral.BindResult {
	e.t.Helper()
	var res referral.BindResult
	if err := db.WithTx(e.ctx, e.pool, func(tx pgx.Tx) error {
		var err error
		res, err = e.svc.Bind(e.ctx, tx, req)
		return err
	}); err != nil {
		e.t.Fatalf("bind: %v", err)
	}
	return res
}

func (e *env) bindingRow(invitee int64) (status, hold string, flags []string) {
	e.t.Helper()
	if err := e.pool.QueryRow(e.ctx, `SELECT status, hold_reason, risk_flags FROM referral_bindings WHERE invitee_id = $1`, invitee).Scan(&status, &hold, &flags); err != nil {
		e.t.Fatalf("binding row: %v", err)
	}
	return
}

func TestBindHappyPathAndReasons(t *testing.T) {
	e := newEnv(t)
	inviter := e.user("13812345678", true)
	a, _ := e.svc.EnsureCode(e.ctx, inviter)
	now := e.clk.Now()
	b := e.user("13900000001", false)
	res := e.doBind(referral.BindRequest{Code: " " + a.Code + " ", InviteeID: b, IP: "1.1.1.1", UserAgent: "ua", Now: now})
	if !res.Bound || res.Reason != "ok" || res.Inviter != "138****5678" {
		t.Fatalf("res = %+v", res)
	}
	status, hold, flags := e.bindingRow(b)
	if status != "active" || hold != "" || len(flags) != 0 {
		t.Fatalf("row = %s %q %v", status, hold, flags)
	}
	var until time.Time
	var ver int64
	e.pool.QueryRow(e.ctx, `SELECT rebate_until, rate_version_id FROM referral_bindings WHERE invitee_id = $1`, b).Scan(&until, &ver)
	if !until.Equal(now.Add(365*24*time.Hour)) || ver == 0 {
		t.Fatalf("rebate_until = %v ver = %d", until, ver)
	}
	var action, actorType string
	e.pool.QueryRow(e.ctx, `SELECT action, actor_type FROM audit_logs ORDER BY id DESC LIMIT 1`).Scan(&action, &actorType)
	if action != "referral.bind" || actorType != "user" {
		t.Fatalf("audit = %s %s", action, actorType)
	}
	// 同一被邀请人再绑：invalid_code，不报错
	if r := e.doBind(referral.BindRequest{Code: a.Code, InviteeID: b, Now: now}); r.Bound || r.Reason != "invalid_code" {
		t.Fatalf("rebind = %+v", r)
	}
	// 无效码 / 垃圾码
	for _, c := range []string{"ZZZZZZZZ", "", "abc", "!!!!!!!!"} {
		if r := e.doBind(referral.BindRequest{Code: c, InviteeID: e.user("1390000"+c[:min(4, len(c))]+"0", false), Now: now}); r.Bound || r.Reason != "invalid_code" {
			t.Fatalf("code %q = %+v", c, r)
		}
	}
	// 冻结 / 封禁 → inviter_frozen
	e.pool.Exec(e.ctx, `UPDATE referral_accounts SET status = 'frozen' WHERE user_id = $1`, inviter)
	if r := e.doBind(referral.BindRequest{Code: a.Code, InviteeID: e.user("13900000002", false), Now: now}); r.Bound || r.Reason != "inviter_frozen" {
		t.Fatalf("frozen = %+v", r)
	}
	e.pool.Exec(e.ctx, `UPDATE referral_accounts SET status = 'active' WHERE user_id = $1`, inviter)
	e.pool.Exec(e.ctx, `UPDATE users SET status = 'banned' WHERE id = $1`, inviter)
	if r := e.doBind(referral.BindRequest{Code: a.Code, InviteeID: e.user("13900000003", false), Now: now}); r.Bound || r.Reason != "inviter_frozen" {
		t.Fatalf("banned = %+v", r)
	}
	// 绑定用绑定时刻的版本：改版本后再绑的人 rebate_until 不同，旧绑定不变
	e.pool.Exec(e.ctx, `UPDATE users SET status = 'active' WHERE id = $1`, inviter)
	e.pool.Exec(e.ctx, `INSERT INTO referral_rate_versions (rate_bp, max_per_invitee, max_per_inviter_30d, hold_days, period_days, effective_from, created_at) VALUES (1000, 1, 1, 7, 30, $1, $1)`, now)
	c := e.user("13900000004", false)
	e.doBind(referral.BindRequest{Code: a.Code, InviteeID: c, Now: now})
	var untilC time.Time
	e.pool.QueryRow(e.ctx, `SELECT rebate_until FROM referral_bindings WHERE invitee_id = $1`, c).Scan(&untilC)
	if !untilC.Equal(now.Add(30*24*time.Hour)) {
		t.Fatalf("new version period = %v", untilC)
	}
	e.pool.QueryRow(e.ctx, `SELECT rebate_until FROM referral_bindings WHERE invitee_id = $1`, b).Scan(&until)
	if !until.Equal(now.Add(365*24*time.Hour)) {
		t.Fatal("old binding period changed")
	}
}

func TestBindRiskFlagsMatrix(t *testing.T) {
	e := newEnv(t)
	inviter := e.user("13812345678", true)
	a, _ := e.svc.EnsureCode(e.ctx, inviter)
	now := e.clk.Now()
	e.pool.Exec(e.ctx, `UPDATE users SET last_login_ip = '8.8.8.8', last_login_at = $2 WHERE id = $1`, inviter, now.Add(-23*time.Hour))
	e.users.devices[inviter] = map[string]bool{"dev-a": true}

	// same_ip（24h 内）→ 只标记，active
	b1 := e.user("13900000011", false)
	e.doBind(referral.BindRequest{Code: a.Code, InviteeID: b1, IP: "8.8.8.8", Now: now})
	if st, _, flags := e.bindingRow(b1); st != "active" || len(flags) != 1 || flags[0] != "same_ip" {
		t.Fatalf("same_ip = %s %v", st, flags)
	}
	// 同 IP 但超过 24h → 不标记
	e.pool.Exec(e.ctx, `UPDATE users SET last_login_at = $2 WHERE id = $1`, inviter, now.Add(-25*time.Hour))
	b2 := e.user("13900000012", false)
	e.doBind(referral.BindRequest{Code: a.Code, InviteeID: b2, IP: "8.8.8.8", Now: now})
	if _, _, flags := e.bindingRow(b2); len(flags) != 0 {
		t.Fatalf("stale ip = %v", flags)
	}
	// same_device → held
	b3 := e.user("13900000013", false)
	res := e.doBind(referral.BindRequest{Code: a.Code, InviteeID: b3, DeviceID: "dev-a", Now: now})
	if st, hold, flags := e.bindingRow(b3); !res.Bound || st != "held" || hold != "same_device" || flags[0] != "same_device" {
		t.Fatalf("same_device = %+v %s %q %v", res, st, hold, flags)
	}
	// burst：已有 3 条，再造到第 19 条不标记，第 20 条标记
	for i := 4; i <= 19; i++ {
		u := e.user("139000000"+string(rune('0'+i/10))+string(rune('0'+i%10)), false)
		e.doBind(referral.BindRequest{Code: a.Code, InviteeID: u, Now: now})
		if _, _, flags := e.bindingRow(u); len(flags) != 0 {
			t.Fatalf("binding #%d flagged %v", i, flags)
		}
	}
	b20 := e.user("13900000120", false)
	e.doBind(referral.BindRequest{Code: a.Code, InviteeID: b20, Now: now})
	if st, _, flags := e.bindingRow(b20); st != "active" || len(flags) != 1 || flags[0] != "burst" {
		t.Fatalf("burst = %s %v", st, flags)
	}
	// 24 小时后的绑定不再算 burst
	e.clk.Advance(25 * time.Hour)
	b21 := e.user("13900000121", false)
	e.doBind(referral.BindRequest{Code: a.Code, InviteeID: b21, Now: e.clk.Now()})
	if _, _, flags := e.bindingRow(b21); len(flags) != 0 {
		t.Fatalf("after window = %v", flags)
	}
	_ = dbtest.T0
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/referral/ -run TestBind 2>&1 | tail -5`
Expected: 编译错误 `e.svc.Bind undefined`。

- [ ] **Step 3: 实现 `Bind`**

`internal/referral/bind.go` 追加：

```go
import (
	"context"
	"strconv"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/db"
)

// Bind 在 identity 的注册事务内把新用户绑定到邀请码的主人。业务上不可绑定（码无效 / 码主冻结或封禁 / 已绑定）返回 Bound=false，
// 不返回 error（注册不能被邀请码问题挡住）；数据库错误照常返回让调用方回滚。
func (s *Service) Bind(ctx context.Context, tx pgx.Tx, req BindRequest) (BindResult, error) {
	code, ok := NormalizeCode(req.Code)
	if !ok {
		return BindResult{Reason: ReasonInvalidCode}, nil
	}
	var inviterID int64
	var accStatus, userStatus, phone string
	err := tx.QueryRow(ctx, `SELECT a.user_id, a.status, u.status, u.phone FROM referral_accounts a JOIN users u ON u.id = a.user_id
		WHERE a.code = $1 FOR SHARE OF a`, code).Scan(&inviterID, &accStatus, &userStatus, &phone)
	if db.IsNoRows(err) {
		return BindResult{Reason: ReasonInvalidCode}, nil
	}
	if err != nil {
		return BindResult{}, err
	}
	if inviterID == req.InviteeID {
		return BindResult{Reason: ReasonInvalidCode}, nil
	}
	if accStatus != AccountActive || userStatus != "active" {
		return BindResult{Reason: ReasonInviterFrozen}, nil
	}
	// 已绑定过：先查再插。撞 invitee_id 唯一索引会让 PG 把 identity 的事务置为失败态（之后写 sessions 会报 25P02），
	// 所以唯一索引只作并发兜底（兜底命中时整个登录事务报错回滚）。
	var bound bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM referral_bindings WHERE invitee_id = $1)`, req.InviteeID).Scan(&bound); err != nil {
		return BindResult{}, err
	}
	if bound {
		return BindResult{Reason: ReasonInvalidCode}, nil
	}
	v, err := s.ConfigAt(ctx, tx, req.Now)
	if err != nil {
		return BindResult{}, err
	}
	inviter, err := s.d.Users.UserState(ctx, tx, inviterID)
	if err != nil {
		return BindResult{}, err
	}
	flags := []string{}
	if req.IP != "" && req.IP == inviter.LastLoginIP && inviter.LastLoginAt != nil && inviter.LastLoginAt.After(req.Now.Add(-SameIPWindow)) {
		flags = append(flags, FlagSameIP)
	}
	if req.DeviceID != "" {
		has, err := s.d.Users.HasDevice(ctx, tx, inviterID, req.DeviceID)
		if err != nil {
			return BindResult{}, err
		}
		if has {
			flags = append(flags, FlagSameDevice)
		}
	}
	var recent int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM referral_bindings WHERE inviter_id = $1 AND bound_at > $2`, inviterID, req.Now.Add(-SameIPWindow)).Scan(&recent); err != nil {
		return BindResult{}, err
	}
	if recent >= BurstThreshold-1 {
		flags = append(flags, FlagBurst)
	}
	status, hold := BindingActive, ""
	for _, f := range flags {
		if f == FlagSameDevice {
			status, hold = BindingHeld, FlagSameDevice
		}
	}
	ua := req.UserAgent
	if len(ua) > MaxUserAgent {
		ua = ua[:MaxUserAgent]
	}
	var id int64
	err = tx.QueryRow(ctx, `INSERT INTO referral_bindings (inviter_id, invitee_id, code, status, risk_flags, hold_reason, rate_version_id, bound_at, rebate_until, ip, device_id, user_agent, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8 + make_interval(days => $9), $10, $11, $12, $8, $8) RETURNING id`,
		inviterID, req.InviteeID, code, status, flags, hold, v.ID, req.Now, v.PeriodDays, req.IP, req.DeviceID, ua).Scan(&id)
	if err != nil {
		return BindResult{}, err // 含 invitee_id 唯一冲突（并发兜底）：让 identity 回滚整个登录
	}
	if err := audit.Record(ctx, tx, req.Now, audit.Actor{UserID: req.InviteeID, IP: req.IP}.Entry("referral.bind", "referral_binding", strconv.FormatInt(id, 10),
		map[string]any{"inviter_id": inviterID, "code": code, "flags": flags, "status": status})); err != nil {
		return BindResult{}, err
	}
	return BindResult{Bound: true, Reason: ReasonOK, Inviter: MaskPhone(phone)}, nil
}
```

（导入块里去掉未用的 `fmt`。）

- [ ] **Step 4: 运行测试确认通过**

Run: `go vet ./internal/referral/ && go test ./internal/referral/ ./internal/identity/ 2>&1 | tail -5`
Expected: `ok`。

- [ ] **Step 5: 提交**

```bash
git add internal/referral/bind.go internal/referral/bind_test.go
git commit -m "feat(referral): Bind at registration with same_ip / same_device / burst flags and hold

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 6: KYC 指纹写入（两条路径）、`OnKYCVerified` 同身份撤销、`backfill-fingerprints` 子命令

**Files:**
- Create: `internal/referral/kyc.go`、`internal/referral/kyc_test.go`、`cmd/platform/referral.go`
- Modify: `internal/identity/kyc.go`（两处结果事务）、`internal/identity/kyc_test.go`（追加）、`cmd/platform/main.go`（`referral` 子命令分发）
- Test: `internal/referral/kyc_test.go`、`internal/identity/kyc_test.go`、`cmd/platform/admin_test.go`（追加 `TestReferralBackfillFingerprints`）

**Interfaces:**
- Consumes: Task 3 `Fingerprint / DeriveFingerprintKey / lockAccount / addEvent`；Task 4 `Deps.FingerprintKey / Deps.Referral / VerifiedFingerprints`；Task 5 绑定行。
- Produces:
  - `identity`：`SubmitPersonalKYC` 结果事务里 `result == "verified"` 时 `UPDATE kyc_records SET fingerprint = $2 WHERE id = $1`（`FingerprintKey` 非空时，`referral.Fingerprint(key, "kyc", idNumber)`），随后 `s.d.Referral.OnKYCVerified(ctx, tx, userID, fp)`（`Referral != nil`）；`ReviewEnterpriseKYC` 通过时同样（`"lic"`, `license_no`），两者都在 `UPDATE users` 之后、审计之前。
  - `func (s *Service) OnKYCVerified(ctx context.Context, tx pgx.Tx, userID int64, fingerprint string) error`：`fingerprint == ""` 直接返回；(a) 作为被邀请人：`SELECT id, inviter_id, status FROM referral_bindings WHERE invitee_id = $1 AND status <> 'revoked'`，有则 `lockAccount(inviter)`、取 `VerifiedFingerprints(inviter ∪ 同邀请人其它未撤销被邀请人)`，任一等于本指纹 → `revokeBinding(tx, now, bindingID, "same_identity", system actor)`；(b) 作为邀请人：`lockAccount(userID)`（没有账户行则跳过），对其所有未撤销绑定的被邀请人取指纹，等于本指纹的逐个撤销。
  - `func (s *Service) revokeBinding(ctx, tx pgx.Tx, now time.Time, bindingID int64, reason string, actor audit.Actor, flag string) (voided int64, err error)`（内部，Task 11 的后台撤销复用）：`UPDATE referral_bindings SET status='revoked', hold_reason=$reason, risk_flags = array_append(risk_flags, $flag)（flag 非空且不重复）, reviewed_by, reviewed_at=now, review_note=$reason, updated_at`；`UPDATE referral_rewards SET status='void', updated_at WHERE binding_id = $1 AND status='pending' RETURNING order_id, reward` → 每行 `addEvent(void, amount=reward, detail=reason)`，`voided = Σ reward`；审计 `actor.Entry(action, "referral_binding", id, {reason, voided, flag})`，`action` 为 `referral.auto_revoke`（system）或 `referral.revoke`（admin，由 Task 11 传入）。
  - `platform referral backfill-fingerprints`（`cmd/platform/referral.go`）：`func runReferral(ctx, pool *pgxpool.Pool, box *secretbox.Box, key []byte, args []string, out io.Writer) error`；遍历 `kyc_records WHERE result='verified' AND fingerprint = '' ORDER BY id`：personal → `box.Open(id_number_enc)` → `"kyc"`；enterprise → `license_no` → `"lic"`；逐条 `UPDATE`；打印 `已回填 N 条，跳过 M 条（解密失败 / 无号码）`；**不触发** `OnKYCVerified`（存量用户不自动撤销，由后台人工处置；README 注明）。`main.go`：`os.Args[1] == "referral"` → `referralMain(os.Args[2:])`（与 `adminMain` 同样的配置 / 迁移 / 连接流程，`key, _ := referral.DeriveFingerprintKey(cfg.SecretKey)`）。

- [ ] **Step 1: 写失败的测试**

`internal/referral/kyc_test.go`：

```go
package referral_test

import (
	"testing"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/platform/db"
)

func (e *env) kycVerified(userID int64, fp string) {
	e.t.Helper()
	e.users.fps[userID] = fp
	if err := db.WithTx(e.ctx, e.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(e.ctx, `SELECT id FROM users WHERE id = $1 FOR UPDATE`, userID); err != nil {
			return err
		}
		return e.svc.OnKYCVerified(e.ctx, tx, userID, fp)
	}); err != nil {
		e.t.Fatalf("OnKYCVerified: %v", err)
	}
}

func TestOnKYCVerifiedRevokesSameIdentity(t *testing.T) {
	e := newEnv(t)
	a := e.user("13800000001", true)
	e.users.fps[a] = "fp-A"
	b := e.user("13800000002", false)
	c := e.user("13800000003", false)
	d := e.user("13800000004", false)
	bb := e.bind(a, b)
	e.bind(a, c)
	e.bind(a, d)
	// B 有待发放返利
	e.pay(b, "ord-b1", 6900)
	e.pool.Exec(e.ctx, `INSERT INTO referral_rewards (order_id, binding_id, inviter_id, invitee_id, rate_version_id, rate_bp, order_amount, reward_cap, reward, status, settle_after, paid_at, created_at, updated_at)
		SELECT 'ord-b1', $1, $2, $3, (SELECT min(id) FROM referral_rate_versions), 1000, 6900, 690, 690, 'pending', $4, $4, $4, $4`, bb, a, b, e.clk.Now())

	// B 的指纹 == A → B 撤销，待发放 void，事件 void，审计 auto_revoke
	e.kycVerified(b, "fp-A")
	st, hold, flags := e.bindingRow(b)
	if st != "revoked" || hold != "same_identity" || flags[len(flags)-1] != "same_identity" {
		t.Fatalf("B = %s %q %v", st, hold, flags)
	}
	if r := e.reward("ord-b1"); r["status"] != "void" {
		t.Fatalf("reward = %v", r)
	}
	if ev := e.events("ord-b1"); len(ev) != 1 || ev[0] != "void" {
		t.Fatalf("events = %v", ev)
	}
	var action string
	e.pool.QueryRow(e.ctx, `SELECT action FROM audit_logs WHERE action LIKE 'referral.%' ORDER BY id DESC LIMIT 1`).Scan(&action)
	if action != "referral.auto_revoke" {
		t.Fatalf("audit = %s", action)
	}
	// C 的指纹是新的 → 不动；D 的指纹 == C → D 撤销（同邀请人的其它被邀请人）
	e.kycVerified(c, "fp-C")
	if st, _, _ := e.bindingRow(c); st != "active" {
		t.Fatalf("C = %s", st)
	}
	e.kycVerified(d, "fp-C")
	if st, _, _ := e.bindingRow(d); st != "revoked" {
		t.Fatalf("D = %s", st)
	}
	// 已撤销的 B 再实名不再处理（幂等）
	e.kycVerified(b, "fp-A")
	// 空指纹什么都不做
	e.kycVerified(c, "")
	if st, _, _ := e.bindingRow(c); st != "active" {
		t.Fatal("empty fingerprint must be a no-op")
	}
}

func TestOnKYCVerifiedInviterLater(t *testing.T) {
	e := newEnv(t)
	a := e.user("13800000001", false) // 邀请人未实名
	b := e.user("13800000002", false)
	c := e.user("13800000003", false)
	e.bind(a, b)
	e.bind(a, c)
	e.kycVerified(b, "fp-X") // 此时 A 无指纹，不撤销
	e.kycVerified(c, "fp-Y")
	if st, _, _ := e.bindingRow(b); st != "active" {
		t.Fatal("B must stay active before inviter verifies")
	}
	e.kycVerified(a, "fp-X") // A 后实名，与 B 同身份 → B 撤销，C 不动
	if st, _, _ := e.bindingRow(b); st != "revoked" {
		t.Fatal("B must be revoked when inviter turns out to be the same identity")
	}
	if st, _, _ := e.bindingRow(c); st != "active" {
		t.Fatal("C must stay active")
	}
	// 没有账户行的用户实名：跳过
	e.kycVerified(e.user("13800000009", false), "fp-Z")
}
```

`internal/identity/kyc_test.go` 追加（参照文件里已有的个人实名 / 企业审核用例的写法，`newEnv` 来自 `auth_test.go`；企业认证需要 `Uploads`——用文件里已有的 fake 或 `storage` 本地盘 helper）：

```go
func TestKYCWritesFingerprintAndCallsHook(t *testing.T) {
	e := newEnv(t, "000000")
	key, _ := referral.DeriveFingerprintKey(dbtest.Key)
	hook := &hookRec{}
	e.svc = identity.NewService(identity.Deps{Pool: e.pool, Clock: e.clock, SMS: e.sms, KYC: kyc.Mock{}, Box: mustBox(t), FixedCode: "000000", FingerprintKey: key})
	e.svc.SetReferral(hook)
	ctx := context.Background()
	uid := dbtest.CreateUser(t, e.pool, "13800000021", false)
	if _, err := e.svc.SubmitPersonalKYC(ctx, uid, "张三", "110101199001011234"); err != nil {
		t.Fatal(err)
	}
	want := referral.Fingerprint(key, "kyc", "110101199001011234")
	var fp string
	if err := e.pool.QueryRow(ctx, `SELECT fingerprint FROM kyc_records WHERE user_id = $1 AND result = 'verified'`, uid).Scan(&fp); err != nil || fp != want {
		t.Fatalf("fingerprint = %q %v", fp, err)
	}
	if len(hook.kyc) != 1 || hook.kyc[0] != want {
		t.Fatalf("hook = %v", hook.kyc)
	}
	// 被驳回不写指纹、不调钩子
	e.svc = identity.NewService(identity.Deps{Pool: e.pool, Clock: e.clock, SMS: e.sms, KYC: kyc.Mock{Reject: true}, Box: mustBox(t), FixedCode: "000000", FingerprintKey: key})
	e.svc.SetReferral(hook)
	uid2 := dbtest.CreateUser(t, e.pool, "13800000022", false)
	e.svc.SubmitPersonalKYC(ctx, uid2, "李四", "110101199001011235")
	e.pool.QueryRow(ctx, `SELECT fingerprint FROM kyc_records WHERE user_id = $1`, uid2).Scan(&fp)
	if fp != "" || len(hook.kyc) != 1 {
		t.Fatalf("rejected: fp=%q hooks=%d", fp, len(hook.kyc))
	}
}
```

（`mustBox(t)` = `secretbox.New(dbtest.Key)`；`kyc.Mock{Reject: true}` 以 `internal/plugins/kyc` 的 Mock 字段为准。企业路径的用例：在文件里已有的 `ReviewEnterpriseKYC` 通过用例末尾加断言 `fingerprint == referral.Fingerprint(key, "lic", <信用代码>)` 与 `hook.kyc` 长度 +1。）

`cmd/platform/admin_test.go` 追加：

```go
func TestReferralBackfillFingerprints(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	box, _ := secretbox.New(dbtest.Key)
	key, _ := referral.DeriveFingerprintKey(dbtest.Key)
	u1 := dbtest.CreateUser(t, pool, "13800000031", true)
	u2 := dbtest.CreateUser(t, pool, "13800000032", true)
	u3 := dbtest.CreateUser(t, pool, "13800000033", true)
	pool.Exec(ctx, `INSERT INTO kyc_records (user_id, kind, real_name, id_number_enc, result, created_at, updated_at) VALUES ($1, 'personal', 'a', $2, 'verified', $3, $3)`, u1, box.Seal([]byte("110101199001011234")), dbtest.T0)
	pool.Exec(ctx, `INSERT INTO kyc_records (user_id, kind, real_name, license_no, result, created_at, updated_at) VALUES ($1, 'enterprise', 'b', '91110000MA01ABCD2X', 'verified', $2, $2)`, u2, dbtest.T0)
	pool.Exec(ctx, `INSERT INTO kyc_records (user_id, kind, real_name, id_number_enc, result, created_at, updated_at) VALUES ($1, 'personal', 'c', '\x00', 'verified', $2, $2)`, u3, dbtest.T0) // 解密失败
	var out bytes.Buffer
	if err := runReferral(ctx, pool, box, key, []string{"backfill-fingerprints"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "已回填 2 条，跳过 1 条") {
		t.Fatalf("out = %s", out.String())
	}
	var fp string
	pool.QueryRow(ctx, `SELECT fingerprint FROM kyc_records WHERE user_id = $1`, u1).Scan(&fp)
	if fp != referral.Fingerprint(key, "kyc", "110101199001011234") {
		t.Fatalf("u1 fp = %q", fp)
	}
	pool.QueryRow(ctx, `SELECT fingerprint FROM kyc_records WHERE user_id = $1`, u2).Scan(&fp)
	if fp != referral.Fingerprint(key, "lic", "91110000MA01ABCD2X") {
		t.Fatalf("u2 fp = %q", fp)
	}
	// 幂等：第二次 0 条
	out.Reset()
	runReferral(ctx, pool, box, key, []string{"backfill-fingerprints"}, &out)
	if !strings.Contains(out.String(), "已回填 0 条") {
		t.Fatalf("second run = %s", out.String())
	}
	if err := runReferral(ctx, pool, box, key, []string{"bogus"}, &out); err == nil {
		t.Fatal("unknown subcommand must fail")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/referral/ -run TestOnKYC 2>&1 | tail -3; go test ./internal/identity/ -run TestKYCWritesFingerprint 2>&1 | tail -3; go test ./cmd/platform/ -run TestReferralBackfill 2>&1 | tail -3`
Expected: 三处编译错误（`OnKYCVerified`、`FingerprintKey`、`runReferral` 未定义）。

- [ ] **Step 3: `referral/kyc.go`**

```go
package referral

import (
	"context"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
)

// OnKYCVerified 在 identity 的实名结果事务内（已持有 users(本人) 行锁）做同身份判定：
// 被邀请人的指纹等于邀请人或同邀请人其它被邀请人 → 该绑定撤销；本人作为邀请人时对其所有未撤销绑定做同样比较。
func (s *Service) OnKYCVerified(ctx context.Context, tx pgx.Tx, userID int64, fingerprint string) error {
	if fingerprint == "" {
		return nil
	}
	now := s.d.Clock.Now()
	// (a) 作为被邀请人
	var bindingID, inviterID int64
	err := tx.QueryRow(ctx, `SELECT id, inviter_id FROM referral_bindings WHERE invitee_id = $1 AND status <> 'revoked'`, userID).Scan(&bindingID, &inviterID)
	if err != nil && !db.IsNoRows(err) {
		return err
	}
	if err == nil {
		if _, err := lockAccount(ctx, tx, inviterID); err != nil {
			return err
		}
		others, err := s.siblingInvitees(ctx, tx, inviterID, userID)
		if err != nil {
			return err
		}
		fps, err := s.d.Users.VerifiedFingerprints(ctx, tx, append(others, inviterID))
		if err != nil {
			return err
		}
		for _, fp := range fps {
			if fp == fingerprint {
				if _, err := s.revokeBinding(ctx, tx, now, bindingID, "same_identity", audit.Actor{}, FlagSameIdentity, "referral.auto_revoke"); err != nil {
					return err
				}
				break
			}
		}
	}
	// (b) 作为邀请人（被邀请人先实名、邀请人后实名）
	if _, err := lockAccount(ctx, tx, userID); err != nil {
		if err == apperr.ErrNotFound {
			return nil
		}
		return err
	}
	rows, err := tx.Query(ctx, `SELECT id, invitee_id FROM referral_bindings WHERE inviter_id = $1 AND status <> 'revoked' ORDER BY id`, userID)
	if err != nil {
		return err
	}
	type row struct{ id, invitee int64 }
	var list []row
	var ids []int64
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.invitee); err != nil {
			rows.Close()
			return err
		}
		list = append(list, r)
		ids = append(ids, r.invitee)
	}
	rows.Close()
	if len(list) == 0 {
		return nil
	}
	fps, err := s.d.Users.VerifiedFingerprints(ctx, tx, ids)
	if err != nil {
		return err
	}
	for _, r := range list {
		if fps[r.invitee] == fingerprint {
			if _, err := s.revokeBinding(ctx, tx, now, r.id, "same_identity", audit.Actor{}, FlagSameIdentity, "referral.auto_revoke"); err != nil {
				return err
			}
		}
	}
	return nil
}

// siblingInvitees 返回同邀请人下除 self 外的未撤销被邀请人。
func (s *Service) siblingInvitees(ctx context.Context, q db.Querier, inviterID, self int64) ([]int64, error) {
	rows, err := q.Query(ctx, `SELECT invitee_id FROM referral_bindings WHERE inviter_id = $1 AND invitee_id <> $2 AND status <> 'revoked'`, inviterID, self)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

// revokeBinding 撤销绑定并作废其全部待发放返利（已发放的不动，由财务用调账处理）。调用方必须已持有 referral_accounts(邀请人) 锁。
func (s *Service) revokeBinding(ctx context.Context, tx pgx.Tx, now time.Time, bindingID int64, reason string, actor audit.Actor, flag, action string) (int64, error) {
	var by *int64
	if actor.AdminID != 0 {
		by = &actor.AdminID
	}
	tag, err := tx.Exec(ctx, `UPDATE referral_bindings SET status = 'revoked', hold_reason = $2, review_note = $2,
		risk_flags = CASE WHEN $3 = '' OR $3 = ANY(risk_flags) THEN risk_flags ELSE array_append(risk_flags, $3) END,
		reviewed_by = $4, reviewed_at = $5, updated_at = $5 WHERE id = $1 AND status <> 'revoked'`, bindingID, reason, flag, by, now)
	if err != nil {
		return 0, err
	}
	if tag.RowsAffected() == 0 {
		return 0, apperr.ErrReferralBindingState
	}
	rows, err := tx.Query(ctx, `UPDATE referral_rewards SET status = 'void', updated_at = $2 WHERE binding_id = $1 AND status = 'pending' RETURNING order_id, reward`, bindingID, now)
	if err != nil {
		return 0, err
	}
	type voided struct {
		order  string
		reward int64
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (voided, error) {
		var v voided
		err := r.Scan(&v.order, &v.reward)
		return v, err
	})
	if err != nil {
		return 0, err
	}
	var total int64
	for _, v := range list {
		total += v.reward
		if err := addEvent(ctx, tx, now, v.order, bindingID, EventVoid, 0, 0, v.reward, reason); err != nil {
			return 0, err
		}
	}
	return total, audit.Record(ctx, tx, now, actor.Entry(action, "referral_binding", strconv.FormatInt(bindingID, 10),
		map[string]any{"reason": reason, "voided": total, "flag": flag}))
}
```

- [ ] **Step 4: identity 两条路径写指纹并调钩子**

`internal/identity/kyc.go`：在 `SubmitPersonalKYC` 的结果事务里，`UPDATE users SET kyc_status …` 之后改为：

```go
		if _, err := tx.Exec(wctx, `UPDATE users SET kyc_status = $2, kind = 'personal', updated_at = $3
			WHERE id = $1 AND kyc_status <> 'verified'`, userID, result, done); err != nil {
			return err
		}
		if result == "verified" {
			return s.afterVerified(wctx, tx, userID, recordID, "kyc", idNumber)
		}
		return nil
```

`ReviewEnterpriseKYC` 里 `UPDATE users …` 之后、`audit.Record` 之前加：

```go
		if approve {
			if err := s.afterVerified(ctx, tx, userID, recordID, "lic", licenseNo); err != nil {
				return err
			}
		}
```

（`licenseNo` 需要在前面的 `SELECT user_id, kind, result FROM kyc_records` 里一并取出 `license_no`。）新增：

```go
// afterVerified 实名通过后：写指纹（有密钥时）并通知 referral 做同身份判定（有钩子时）。与调用方同事务。
func (s *Service) afterVerified(ctx context.Context, tx pgx.Tx, userID, recordID int64, kind, number string) error {
	if len(s.d.FingerprintKey) == 0 {
		return nil
	}
	fp := referral.Fingerprint(s.d.FingerprintKey, kind, number)
	if _, err := tx.Exec(ctx, `UPDATE kyc_records SET fingerprint = $2 WHERE id = $1`, recordID, fp); err != nil {
		return err
	}
	if s.d.Referral == nil {
		return nil
	}
	return s.d.Referral.OnKYCVerified(ctx, tx, userID, fp)
}
```

- [ ] **Step 5: 子命令**

`cmd/platform/referral.go`：

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/platform/secretbox"
	"spark-cloud/internal/referral"
)

const referralUsage = `用法：
  platform referral backfill-fingerprints   # 为存量已实名用户计算同身份指纹（上线计划 8 后执行一次）`

// runReferral 处理 `platform referral ...` 子命令。
func runReferral(ctx context.Context, pool *pgxpool.Pool, box *secretbox.Box, key []byte, args []string, out io.Writer) error {
	if len(args) == 0 || args[0] != "backfill-fingerprints" {
		return errors.New(referralUsage)
	}
	rows, err := pool.Query(ctx, `SELECT id, kind, id_number_enc, license_no FROM kyc_records WHERE result = 'verified' AND fingerprint = '' ORDER BY id`)
	if err != nil {
		return err
	}
	type rec struct {
		id      int64
		kind    string
		enc     []byte
		license string
	}
	var list []rec
	for rows.Next() {
		var r rec
		if err := rows.Scan(&r.id, &r.kind, &r.enc, &r.license); err != nil {
			rows.Close()
			return err
		}
		list = append(list, r)
	}
	rows.Close()
	done, skipped := 0, 0
	for _, r := range list {
		var fp string
		switch {
		case r.kind == "personal" && len(r.enc) > 0:
			plain, err := box.Open(r.enc)
			if err != nil {
				fmt.Fprintf(out, "记录 %d：解密失败（%v），跳过\n", r.id, err)
				skipped++
				continue
			}
			fp = referral.Fingerprint(key, "kyc", string(plain))
		case r.kind == "enterprise" && r.license != "":
			fp = referral.Fingerprint(key, "lic", r.license)
		default:
			skipped++
			continue
		}
		if _, err := pool.Exec(ctx, `UPDATE kyc_records SET fingerprint = $2 WHERE id = $1 AND fingerprint = ''`, r.id, fp); err != nil {
			return err
		}
		done++
	}
	fmt.Fprintf(out, "已回填 %d 条，跳过 %d 条（解密失败 / 无号码）\n", done, skipped)
	return nil
}
```

`cmd/platform/main.go` 的 `main` 里 `admin` 分支之后加：

```go
	if len(os.Args) > 1 && os.Args[1] == "referral" {
		if err := referralMain(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
```

```go
// referralMain 执行 `platform referral ...`：与服务相同的配置连接数据库（会先执行迁移）。
func referralMain(args []string) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	ctx := context.Background()
	if err := db.Migrate(ctx, cfg.DatabaseURL); err != nil {
		return err
	}
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	box, err := secretbox.New(cfg.SecretKey)
	if err != nil {
		return err
	}
	key, err := referral.DeriveFingerprintKey(cfg.SecretKey)
	if err != nil {
		return err
	}
	return runReferral(ctx, pool, box, key, args, os.Stdout)
}
```

- [ ] **Step 6: 运行测试确认通过**

Run: `go vet ./... && go test ./internal/referral/ ./internal/identity/ ./cmd/platform/ 2>&1 | tail -6`
Expected: `ok`。

- [ ] **Step 7: 提交**

```bash
git add internal/referral/kyc.go internal/referral/kyc_test.go internal/identity/kyc.go internal/identity/kyc_test.go cmd/platform/referral.go cmd/platform/main.go cmd/platform/admin_test.go
git commit -m "feat(referral): kyc fingerprint on verify, same-identity auto revoke, backfill-fingerprints subcommand

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 7: `rental.OrderForReferral` 与计提 `accrue`（费率版本、上限、`settle_after`、返利期、撤销占位）

**Files:**
- Create: `internal/referral/accrue.go`、`internal/referral/accrue_test.go`
- Modify: `internal/rental/orders.go`（`OrderForReferral`）、`internal/rental/orders_test.go` 或 `place_test.go`（追加 `TestOrderForReferral`）

**Interfaces:**
- Consumes: Task 3 的 `Service / ConfigAt / lockAccount / addEvent / UserLookup / OrderLookup`；Task 1 的表。
- Produces:
  - `func (s *rental.Service) OrderForReferral(ctx context.Context, q db.Querier, orderID string) (int64, *time.Time, error)`（`SELECT o.user_id, i.scheduled_start_at FROM orders o JOIN instances i ON i.id = o.instance_id WHERE o.id = $1`；无行 → `apperr.ErrNotFound`）。`*rental.Service` 因此满足 `referral.OrderLookup`。
  - `internal/referral/accrue.go`：
    ```go
    type ledgerRow struct { ID, UserID int64; Kind string; Amount int64; RefID string; CreatedAt time.Time }
    var errSkip = errors.New("referral: skip row")          // 本轮不处理、不写事件（下轮重试）
    func (s *Service) accrue(ctx context.Context, tx pgx.Tx, now time.Time, l ledgerRow) error
    func (s *Service) inviteeMasked(ctx context.Context, q db.Querier, inviteeID int64) (string, error) // "138****5678"，供发放 / 扣回备注
    func alreadyProcessed(ctx context.Context, q db.Querier, ledgerID int64) (bool, error)
    ```
    `Reward` 结构（供 Task 8～13）：`Reward{OrderID string; BindingID, InviterID, InviteeID, RateVersionID int64; RateBP int; OrderAmount, RefundedAmount, RewardCap, RewardAmt int64; Status string; SettleAfter time.Time; SettledAmount, ClawedAmount int64; SettledAt *time.Time; PaidAt, CreatedAt, UpdatedAt time.Time}`，JSON 标签与列同名（`reward` 字段名 `RewardAmt` 标签 `json:"reward"`）；`rewardCols`、`scanReward(row pgx.Row) (Reward, error)`；`lockReward(ctx, tx, orderID) (Reward, error)`（`FOR UPDATE`，无行 → `apperr.ErrNotFound`）。
  - 测试 helper（`accrue_test.go`）：`e.processPay(ledgerID int64) error`（在独立事务里按 id 读流水 → `AccrueForTest`）；`export_test.go` 导出 `AccrueForTest(ctx, tx, now, l)`、`type LedgerRowForTest = ledgerRow`、`LedgerRowOf(...)`、`ErrSkip`。

- [ ] **Step 1: 写失败的测试**

`internal/rental/place_test.go`（或 `orders_test.go`，以现有 `newEnv` / `e.place` 为准）追加：

```go
func TestOrderForReferral(t *testing.T) {
	e := newEnv(t, 1)
	ord, _ := e.place(catalog.D1d)
	uid, start, err := e.svc.OrderForReferral(e.ctx, e.pool, ord.ID)
	if err != nil || uid != e.userID || start != nil {
		t.Fatalf("immediate = %d %v %v", uid, start, err)
	}
	booked := e.book(dbtest.T0.Add(25 * time.Hour)) // 预订：返回实例，订单由 e.orderOf(booked.ID) 取（以文件 helper 为准）
	bo := e.orderOf(booked.ID)
	_, start, err = e.svc.OrderForReferral(e.ctx, e.pool, bo.ID)
	if err != nil || start == nil || !start.Equal(dbtest.T0.Add(25*time.Hour)) {
		t.Fatalf("booked = %v %v", start, err)
	}
	if _, _, err := e.svc.OrderForReferral(e.ctx, e.pool, "ord-nope"); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}
}
```

`internal/referral/export_test.go`：

```go
package referral

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

type LedgerRowForTest = ledgerRow

var ErrSkip = errSkip

func (s *Service) AccrueForTest(ctx context.Context, tx pgx.Tx, now time.Time, l ledgerRow) error { return s.accrue(ctx, tx, now, l) }
func LedgerRowOf(id, userID int64, kind string, amount int64, refID string, at time.Time) ledgerRow {
	return ledgerRow{ID: id, UserID: userID, Kind: kind, Amount: amount, RefID: refID, CreatedAt: at}
}
```

`internal/referral/accrue_test.go`：

```go
package referral_test

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/referral"
)

// processPay 读一条流水并在独立事务里计提（Task 10 之前的直通路径）。
func (e *env) processPay(ledgerID int64) error {
	e.t.Helper()
	var l referral.LedgerRowForTest
	if err := e.pool.QueryRow(e.ctx, `SELECT id, user_id, kind, amount, ref_id, created_at FROM wallet_ledger WHERE id = $1`, ledgerID).
		Scan(&l.ID, &l.UserID, &l.Kind, &l.Amount, &l.RefID, &l.CreatedAt); err != nil {
		e.t.Fatal(err)
	}
	return db.WithTx(e.ctx, e.pool, func(tx pgx.Tx) error { return e.svc.AccrueForTest(e.ctx, tx, e.clk.Now(), l) })
}

func TestAccrueUsesVersionAtPayTimeAndFloors(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	e.bind(a, b)
	// 版本 2 在 T0+1h 生效：15%
	e.pool.Exec(e.ctx, `INSERT INTO referral_rate_versions (rate_bp, max_per_invitee, max_per_inviter_30d, hold_days, period_days, effective_from, created_at) VALUES (1500, 50000, 500000, 3, 365, $1, $1)`, e.clk.Now().Add(time.Hour))
	l1 := e.pay(b, "ord-1", 6901) // 10%：690.1 → 690
	e.clk.Advance(2 * time.Hour)
	l2 := e.pay(b, "ord-2", 6901) // 15%：1035.15 → 1035，hold 3 天
	if err := e.processPay(l1); err != nil {
		t.Fatal(err)
	}
	if err := e.processPay(l2); err != nil {
		t.Fatal(err)
	}
	r1, r2 := e.reward("ord-1"), e.reward("ord-2")
	if r1["reward"] != int64(690) || r1["status"] != "pending" || !r1["settle_after"].(time.Time).Equal(e.clk.Now().Add(-2*time.Hour+7*24*time.Hour)) {
		t.Fatalf("r1 = %v", r1)
	}
	if r2["reward"] != int64(1035) || !r2["settle_after"].(time.Time).Equal(e.clk.Now().Add(3*24*time.Hour)) {
		t.Fatalf("r2 = %v", r2)
	}
	if ev := e.events("ord-1"); len(ev) != 1 || ev[0] != "accrue" {
		t.Fatalf("events = %v", ev)
	}
	// 幂等：同一流水再处理 → source_ledger_id 唯一冲突被识别为已处理（返回 nil、不重复建行）
	if err := e.processPay(l1); err != nil {
		t.Fatalf("reprocess = %v", err)
	}
	var n int
	e.pool.QueryRow(e.ctx, `SELECT count(*) FROM referral_events WHERE order_id = 'ord-1'`).Scan(&n)
	if n != 1 {
		t.Fatalf("events after reprocess = %d", n)
	}
}

func TestAccrueCapsPerInviteeAndPerInviter30d(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	c := e.user("13900000002", true)
	e.bind(a, b)
	e.bind(a, c)
	// 被邀请人上限 1000 分，邀请人 30 天上限 1500 分
	e.pool.Exec(e.ctx, `UPDATE referral_rate_versions SET max_per_invitee = 1000, max_per_inviter_30d = 1500`)
	e.processPay(e.pay(b, "ord-b1", 6900)) // 690
	e.processPay(e.pay(b, "ord-b2", 6900)) // room_invitee 310 → 310，capped 380
	e.processPay(e.pay(b, "ord-b3", 6900)) // room 0 → reward 0，capped 690
	if e.reward("ord-b2")["reward"] != int64(310) || e.reward("ord-b3")["reward"] != int64(0) {
		t.Fatalf("b2/b3 = %v %v", e.reward("ord-b2"), e.reward("ord-b3"))
	}
	if ev := e.events("ord-b2"); len(ev) != 2 || ev[1] != "capped" {
		t.Fatalf("b2 events = %v", ev)
	}
	// 邀请人 30 天：已占用 pending 1000；c 的 690 → room_inviter 500 → 500
	e.processPay(e.pay(c, "ord-c1", 6900))
	if e.reward("ord-c1")["reward"] != int64(500) {
		t.Fatalf("c1 = %v", e.reward("ord-c1"))
	}
	// 把 b1 标记为 10 天前发放（仍在 30 天窗口内）：净额 690 占用；pending 310 + 0 + 500 = 810 → room 0
	e.pool.Exec(e.ctx, `UPDATE referral_rewards SET status = 'settled', settled_amount = 690, settled_at = $1 WHERE order_id = 'ord-b1'`, e.clk.Now().Add(-10*24*time.Hour))
	e.processPay(e.pay(c, "ord-c2", 6900))
	if e.reward("ord-c2")["reward"] != int64(0) {
		t.Fatalf("c2 = %v", e.reward("ord-c2"))
	}
	// 推进 31 天：b1 滑出窗口，room = 1500 − 810 = 690 → 全额；但 c 的被邀请人上限 1000 − 500 − 0 = 500 → 500
	e.clk.Advance(31 * 24 * time.Hour)
	e.processPay(e.pay(c, "ord-c3", 6900))
	if e.reward("ord-c3")["reward"] != int64(500) {
		t.Fatalf("c3 = %v", e.reward("ord-c3"))
	}
}

func TestAccrueBookingPeriodOverRevokedAndMissingOrder(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	bid := e.bind(a, b)
	// 预订：开始时刻在 10 天后 > 7 天 → settle_after = 开始时刻
	start := e.clk.Now().Add(10 * 24 * time.Hour)
	e.processPay(e.payAt(b, "ord-book", 6900, &start))
	if !e.reward("ord-book")["settle_after"].(time.Time).Equal(start) {
		t.Fatalf("booking settle_after = %v", e.reward("ord-book")["settle_after"])
	}
	// 预订开始在 2 天后 < 7 天 → settle_after = 付款 + 7 天
	soon := e.clk.Now().Add(2 * 24 * time.Hour)
	e.processPay(e.payAt(b, "ord-soon", 6900, &soon))
	if !e.reward("ord-soon")["settle_after"].(time.Time).Equal(e.clk.Now().Add(7 * 24 * time.Hour)) {
		t.Fatal("soon booking must use hold_days")
	}
	// 返利期结束：rebate_until 改到过去 → capped(period_over)，不建 reward
	e.pool.Exec(e.ctx, `UPDATE referral_bindings SET rebate_until = $2 WHERE id = $1`, bid, e.clk.Now().Add(-time.Second))
	e.processPay(e.pay(b, "ord-late", 6900))
	var exists bool
	e.pool.QueryRow(e.ctx, `SELECT EXISTS (SELECT 1 FROM referral_rewards WHERE order_id = 'ord-late')`).Scan(&exists)
	if exists || len(e.events("ord-late")) != 1 || e.events("ord-late")[0] != "capped" {
		t.Fatalf("period over: exists=%v events=%v", exists, e.events("ord-late"))
	}
	e.pool.Exec(e.ctx, `UPDATE referral_bindings SET rebate_until = $2 WHERE id = $1`, bid, e.clk.Now().Add(365*24*time.Hour))
	// 撤销的绑定：void 占位
	e.pool.Exec(e.ctx, `UPDATE referral_bindings SET status = 'revoked' WHERE id = $1`, bid)
	e.processPay(e.pay(b, "ord-revoked", 6900))
	if ev := e.events("ord-revoked"); len(ev) != 1 || ev[0] != "void" {
		t.Fatalf("revoked = %v", ev)
	}
	e.pool.Exec(e.ctx, `UPDATE referral_bindings SET status = 'held' WHERE id = $1`, bid)
	// held 仍计提
	e.processPay(e.pay(b, "ord-held", 6900))
	if e.reward("ord-held")["reward"] != int64(690) {
		t.Fatal("held binding must still accrue")
	}
	// 订单查不到：跳过、不写事件（errSkip 对调用方表现为 nil）
	l := e.pay(b, "ord-ghost", 6900)
	delete(e.orders.m, "ord-ghost")
	if err := e.processPay(l); err != nil && !errors.Is(err, referral.ErrSkip) {
		t.Fatalf("ghost = %v", err)
	}
	if len(e.events("ord-ghost")) != 0 {
		t.Fatal("ghost order must not write events")
	}
}
```

（`e.book` / `e.orderOf` 以 `internal/rental` 测试文件现有 helper 为准；没有 `orderOf` 时用 `SELECT id FROM orders WHERE instance_id = $1`。）

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/rental/ -run TestOrderForReferral 2>&1 | tail -3; go test ./internal/referral/ -run TestAccrue 2>&1 | tail -3`
Expected: 两处编译错误。

- [ ] **Step 3: `rental.OrderForReferral`**

`internal/rental/orders.go` 末尾：

```go
// OrderForReferral 供 referral 模块（计划 8）只读：订单的用户与预订单的计划开始时刻（非预订为 nil）。
func (s *Service) OrderForReferral(ctx context.Context, q db.Querier, orderID string) (int64, *time.Time, error) {
	var userID int64
	var start *time.Time
	err := q.QueryRow(ctx, `SELECT o.user_id, i.scheduled_start_at FROM orders o JOIN instances i ON i.id = o.instance_id WHERE o.id = $1`, orderID).Scan(&userID, &start)
	if db.IsNoRows(err) {
		return 0, nil, apperr.ErrNotFound
	}
	return userID, start, err
}
```

- [ ] **Step 4: `referral/accrue.go`**

```go
package referral

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
)

// ledgerRow 是扫描到的一条 wallet_ledger（ref_type='order'）。
type ledgerRow struct {
	ID        int64
	UserID    int64
	Kind      string
	Amount    int64
	RefID     string
	CreatedAt time.Time
}

// errSkip：本轮不处理这一行、不写事件；下轮因 NOT EXISTS 再试。
var errSkip = errors.New("referral: skip row")

type Reward struct {
	OrderID        string     `json:"order_id"`
	BindingID      int64      `json:"binding_id"`
	InviterID      int64      `json:"inviter_id"`
	InviteeID      int64      `json:"invitee_id"`
	RateVersionID  int64      `json:"rate_version_id"`
	RateBP         int        `json:"rate_bp"`
	OrderAmount    int64      `json:"order_amount"`
	RefundedAmount int64      `json:"refunded_amount"`
	RewardCap      int64      `json:"reward_cap"`
	RewardAmt      int64      `json:"reward"`
	Status         string     `json:"status"`
	SettleAfter    time.Time  `json:"settle_after"`
	SettledAmount  int64      `json:"settled_amount"`
	ClawedAmount   int64      `json:"clawed_amount"`
	SettledAt      *time.Time `json:"settled_at"`
	PaidAt         time.Time  `json:"paid_at"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

const rewardCols = `order_id, binding_id, inviter_id, invitee_id, rate_version_id, rate_bp, order_amount, refunded_amount, reward_cap, reward, status, settle_after, settled_amount, clawed_amount, settled_at, paid_at, created_at, updated_at`

func scanReward(row pgx.Row) (Reward, error) {
	var r Reward
	err := row.Scan(&r.OrderID, &r.BindingID, &r.InviterID, &r.InviteeID, &r.RateVersionID, &r.RateBP, &r.OrderAmount, &r.RefundedAmount, &r.RewardCap, &r.RewardAmt,
		&r.Status, &r.SettleAfter, &r.SettledAmount, &r.ClawedAmount, &r.SettledAt, &r.PaidAt, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}

func lockReward(ctx context.Context, tx pgx.Tx, orderID string) (Reward, error) {
	r, err := scanReward(tx.QueryRow(ctx, `SELECT `+rewardCols+` FROM referral_rewards WHERE order_id = $1 FOR UPDATE`, orderID))
	if db.IsNoRows(err) {
		return r, apperr.ErrNotFound
	}
	return r, err
}

// alreadyProcessed 报告该流水是否已有事件（并发 / 重扫时的二次保险；唯一索引是最终兜底）。
func alreadyProcessed(ctx context.Context, q db.Querier, ledgerID int64) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM referral_events WHERE source_ledger_id = $1)`, ledgerID).Scan(&ok)
	return ok, err
}

// inviteeMasked 返回被邀请人的脱敏手机号，供发放 / 扣回流水的备注（「邀请返利 138****5678 订单 ord-…」）。
func (s *Service) inviteeMasked(ctx context.Context, q db.Querier, inviteeID int64) (string, error) {
	u, err := s.d.Users.UserState(ctx, q, inviteeID)
	if err != nil {
		return "", err
	}
	return MaskPhone(u.Phone), nil
}

// accrue 处理一条 order_pay 流水（规格 §4.2）。调用方提供事务；本函数先锁 referral_accounts(邀请人)。
func (s *Service) accrue(ctx context.Context, tx pgx.Tx, now time.Time, l ledgerRow) error {
	if done, err := alreadyProcessed(ctx, tx, l.ID); err != nil || done {
		return err
	}
	var bindingID, inviterID int64
	var status string
	var rebateUntil time.Time
	err := tx.QueryRow(ctx, `SELECT id, inviter_id, status, rebate_until FROM referral_bindings WHERE invitee_id = $1`, l.UserID).
		Scan(&bindingID, &inviterID, &status, &rebateUntil)
	if db.IsNoRows(err) {
		return nil // 非绑定用户（Discover 的 JOIN 不会选到；防御）
	}
	if err != nil {
		return err
	}
	if _, err := lockAccount(ctx, tx, inviterID); err != nil {
		return err
	}
	if status == BindingRevoked {
		return addEvent(ctx, tx, now, l.RefID, bindingID, EventVoid, l.ID, 0, 0, "binding revoked")
	}
	if !l.CreatedAt.Before(rebateUntil) {
		return addEvent(ctx, tx, now, l.RefID, bindingID, EventCapped, l.ID, 0, 0, "period_over")
	}
	order := l.RefID
	userID, start, err := s.d.Orders.OrderForReferral(ctx, tx, order)
	if errors.Is(err, apperr.ErrNotFound) || (err == nil && userID != l.UserID) {
		s.d.Log.Error("referral: order not found for order_pay ledger", "ledger", l.ID, "order", order, "user", l.UserID)
		return errSkip
	}
	if err != nil {
		return err
	}
	v, err := s.ConfigAt(ctx, tx, l.CreatedAt)
	if err != nil {
		return err
	}
	base := -l.Amount
	raw := base * int64(v.RateBP) / 10000
	var usedInvitee, settledNet30, pendingInviter int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(reward), 0) FROM referral_rewards WHERE binding_id = $1 AND status <> 'void'`, bindingID).Scan(&usedInvitee); err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(settled_amount - clawed_amount), 0) FROM referral_rewards WHERE inviter_id = $1 AND status = 'settled' AND settled_at > $2`,
		inviterID, now.Add(-InviterWindow)).Scan(&settledNet30); err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(reward), 0) FROM referral_rewards WHERE inviter_id = $1 AND status = 'pending'`, inviterID).Scan(&pendingInviter); err != nil {
		return err
	}
	roomInvitee := max(0, v.MaxPerInvitee-usedInvitee)
	roomInviter := max(0, v.MaxPerInviter30d-settledNet30-pendingInviter)
	cap := min(raw, roomInvitee, roomInviter)
	settleAfter := l.CreatedAt.Add(time.Duration(v.HoldDays) * 24 * time.Hour)
	if start != nil && start.After(settleAfter) {
		settleAfter = *start
	}
	if _, err := tx.Exec(ctx, `INSERT INTO referral_rewards (order_id, binding_id, inviter_id, invitee_id, rate_version_id, rate_bp, order_amount, reward_cap, reward, status, settle_after, paid_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8, 'pending', $9, $10, $11, $11)`,
		order, bindingID, inviterID, l.UserID, v.ID, v.RateBP, base, cap, settleAfter, l.CreatedAt, now); err != nil {
		return err
	}
	if err := addEvent(ctx, tx, now, order, bindingID, EventAccrue, l.ID, 0, cap,
		fmt.Sprintf("raw=%d room_invitee=%d room_inviter=%d rate_bp=%d version=%d", raw, roomInvitee, roomInviter, v.RateBP, v.ID)); err != nil {
		return err
	}
	if cap < raw {
		return addEvent(ctx, tx, now, order, bindingID, EventCapped, 0, 0, raw-cap, "cap")
	}
	return nil
}
```

**幂等说明**：`alreadyProcessed` 在事务开头检查；两个事务并发处理同一行时后者会在 `addEvent` 撞 `referral_events_source_uniq`——Task 10 的 `processRow` 把唯一冲突当作「已处理」吞掉（整个事务回滚、不记错误）。本任务的直通测试 `processPay` 第二次调用时由 `alreadyProcessed` 返回 nil。

- [ ] **Step 5: 运行测试确认通过**

Run: `go vet ./... && go test ./internal/rental/ -run TestOrderForReferral && go test ./internal/referral/ 2>&1 | tail -5`
Expected: `ok`。

- [ ] **Step 6: 提交**

```bash
git add internal/rental/orders.go internal/rental/place_test.go internal/referral/accrue.go internal/referral/accrue_test.go internal/referral/export_test.go
git commit -m "feat(referral): accrue rewards from order_pay ledger (rate version at pay time, caps, booking settle_after, period over)

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 8: 退款处理：`OrderIDOf`、重算、扣回（`ApplyUpTo`）、欠账

**Files:**
- Create: `internal/referral/refund.go`、`internal/referral/refund_test.go`
- Modify: `internal/referral/export_test.go`（导出 `ApplyRefundForTest`）

**Interfaces:**
- Consumes: Task 7 的 `ledgerRow / errSkip / lockReward / alreadyProcessed / inviteeMasked / addEvent`；Task 2 的 `WalletOps.ApplyUpTo`。
- Produces:
  - `func OrderIDOf(refID string) string`：去掉可选的 `-r<数字>` 后缀（`ord-abc-r3 → ord-abc`；`ord-abc → ord-abc`；`ord-abc-rx` 原样）。
  - `func (s *Service) applyRefund(ctx context.Context, tx pgx.Tx, now time.Time, l ledgerRow) error`（规格 §4.3 + Review Focus 1）：
    1. `alreadyProcessed` → nil。
    2. `order := OrderIDOf(l.RefID)`；`SELECT inviter_id FROM referral_rewards WHERE order_id = $1`：
       - 无行 → `SELECT binding_id, inviter_id FROM referral_events e JOIN referral_bindings b ON b.id = e.binding_id WHERE e.order_id = $1 AND e.kind IN ('capped','void') AND e.source_ledger_id IS NOT NULL LIMIT 1`：有 → `lockAccount(inviter)` + `addEvent(refund, source=l.ID, amount 0, detail 'no reward')`；无 → `errSkip`。
    3. `lockAccount(inviter)` → `lockReward(order)`。
    4. `refunded := r.RefundedAmount + l.Amount`（`> OrderAmount` 时夹到 `OrderAmount` 并记日志——退款路径累计不超过 `orders.amount`，超出只可能是数据异常）；`target := min(r.RewardCap, (OrderAmount − refunded) × RateBP / 10000)`。
    5. `addEvent(refund, source=l.ID, amount = r.RewardAmt − target, detail fmt "refund=%d target=%d")`。
    6. `pending`：`UPDATE reward = target, refunded_amount`。`void`：只更新 `refunded_amount`。`settled`：`excess := (SettledAmount − ClawedAmount) − target`；`excess > 0` → `n := count(events clawback for order) + 1`；`applied, _, err := Wallet.ApplyUpTo(tx, Entry{UserID: inviter, Amount: −excess, Kind: KindReferralClawback, RefType: "referral", RefID: order + "-c" + n, Remark: "返利扣回 " + masked + " 订单 " + order})`；`applied > 0` 时取 `wallet_ledger.id`（`SELECT id FROM wallet_ledger WHERE kind = 'referral_clawback' AND ref_type = 'referral' AND ref_id = $1`）；`shortfall := excess − applied`；`shortfall > 0` → `UPDATE referral_accounts SET debt = debt + shortfall` + `addEvent(debt_add, amount = shortfall)`；`addEvent(clawback, amount = excess, wallet_ledger_id)`；`UPDATE reward = target, clawed_amount += excess, refunded_amount`。

- [ ] **Step 1: 写失败的测试**

`internal/referral/export_test.go` 追加：

```go
func (s *Service) ApplyRefundForTest(ctx context.Context, tx pgx.Tx, now time.Time, l ledgerRow) error { return s.applyRefund(ctx, tx, now, l) }
```

`internal/referral/refund_test.go`：

```go
package referral_test

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/referral"
)

func (e *env) processRefund(ledgerID int64) error {
	e.t.Helper()
	var l referral.LedgerRowForTest
	if err := e.pool.QueryRow(e.ctx, `SELECT id, user_id, kind, amount, ref_id, created_at FROM wallet_ledger WHERE id = $1`, ledgerID).
		Scan(&l.ID, &l.UserID, &l.Kind, &l.Amount, &l.RefID, &l.CreatedAt); err != nil {
		e.t.Fatal(err)
	}
	return db.WithTx(e.ctx, e.pool, func(tx pgx.Tx) error { return e.svc.ApplyRefundForTest(e.ctx, tx, e.clk.Now(), l) })
}

// markSettled 直接把 reward 置为已发放并给邀请人入账（发放逻辑由 Task 9 测）。
func (e *env) markSettled(order string, inviter int64) {
	e.t.Helper()
	var reward int64
	e.pool.QueryRow(e.ctx, `SELECT reward FROM referral_rewards WHERE order_id = $1`, order).Scan(&reward)
	e.pool.Exec(e.ctx, `UPDATE referral_rewards SET status = 'settled', settled_amount = reward, settled_at = $2 WHERE order_id = $1`, order, e.clk.Now())
	if err := db.WithTx(e.ctx, e.pool, func(tx pgx.Tx) error {
		_, err := e.wallet.Apply(e.ctx, tx, wallet.Entry{UserID: inviter, Amount: reward, Kind: wallet.KindReferralRebate, RefType: "referral", RefID: order})
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
}

func TestOrderIDOf(t *testing.T) {
	for in, want := range map[string]string{"ord-abc": "ord-abc", "ord-abc-r1": "ord-abc", "ord-abc-r12": "ord-abc", "ord-abc-rx": "ord-abc-rx", "ord-r1-r2": "ord-r1"} {
		if got := referral.OrderIDOf(in); got != want {
			t.Errorf("OrderIDOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRefundPendingOnlyChangesNumbers(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	e.bind(a, b)
	e.processPay(e.pay(b, "ord-1", 6900)) // 690
	if err := e.processRefund(e.refund(b, "ord-1-r1", 3000)); err != nil { // 剩 3900 → 390
		t.Fatal(err)
	}
	r := e.reward("ord-1")
	if r["status"] != "pending" || r["reward"] != int64(390) || r["refunded"] != int64(3000) {
		t.Fatalf("after r1 = %v", r)
	}
	e.processRefund(e.refund(b, "ord-1-r2", 3900)) // 全退 → 0，仍 pending
	if r := e.reward("ord-1"); r["reward"] != int64(0) || r["status"] != "pending" || r["refunded"] != int64(6900) {
		t.Fatalf("after r2 = %v", r)
	}
	if ev := e.events("ord-1"); len(ev) != 3 || ev[1] != "refund" || ev[2] != "refund" {
		t.Fatalf("events = %v", ev)
	}
	if e.ledgerSum(a, "referral_clawback") != 0 {
		t.Fatal("pending refund must not touch wallet")
	}
	// 开通失败退款（ref_id 无后缀）同样处理
	e.processPay(e.pay(b, "ord-2", 6900))
	e.processRefund(e.refund(b, "ord-2", 6900))
	if r := e.reward("ord-2"); r["reward"] != int64(0) {
		t.Fatalf("refundAll = %v", r)
	}
}

func TestRefundSettledClawsBackAndRecordsDebt(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	e.bind(a, b)
	e.processPay(e.pay(b, "ord-1", 6900))
	e.markSettled("ord-1", a) // A 余额 690
	// 退 3000：target 390，excess 300，余额够 → 扣 300
	e.processRefund(e.refund(b, "ord-1-r1", 3000))
	r := e.reward("ord-1")
	if r["status"] != "settled" || r["reward"] != int64(390) || r["clawed"] != int64(300) || r["settled"] != int64(690) {
		t.Fatalf("after r1 = %v", r)
	}
	if e.ledgerSum(a, "referral_clawback") != -300 || e.debt(a) != 0 {
		t.Fatalf("wallet = %d debt = %d", e.ledgerSum(a, "referral_clawback"), e.debt(a))
	}
	var refID, remark string
	e.pool.QueryRow(e.ctx, `SELECT ref_id, remark FROM wallet_ledger WHERE kind = 'referral_clawback' ORDER BY id DESC LIMIT 1`).Scan(&refID, &remark)
	if refID != "ord-1-c1" || remark != "返利扣回 139****0001 订单 ord-1" {
		t.Fatalf("ledger = %q %q", refID, remark)
	}
	// A 把余额花光（390 → 0），再退 2000：target = floor(1900×0.1) = 190，excess 200，余额 0 → 欠账 200
	dbtest.Fund(e.t, e.pool, a, -390)
	e.processRefund(e.refund(b, "ord-1-r2", 2000))
	r = e.reward("ord-1")
	if r["reward"] != int64(190) || r["clawed"] != int64(500) || e.debt(a) != 200 {
		t.Fatalf("after r2 = %v debt = %d", r, e.debt(a))
	}
	if ev := e.events("ord-1"); len(ev) != 6 || ev[3] != "refund" || ev[4] != "debt_add" || ev[5] != "clawback" {
		t.Fatalf("events = %v", ev)
	}
	// 第三次：余额有 100，excess 190（全退），扣 100 欠 90 → 欠账 290；ref -c3
	dbtest.Fund(e.t, e.pool, a, 100)
	e.processRefund(e.refund(b, "ord-1-r3", 1900))
	if r := e.reward("ord-1"); r["reward"] != int64(0) || r["clawed"] != int64(690) || e.debt(a) != 290 {
		t.Fatalf("after r3 = %v debt = %d", r, e.debt(a))
	}
	var n int
	e.pool.QueryRow(e.ctx, `SELECT count(*) FROM wallet_ledger WHERE kind = 'referral_clawback' AND ref_id IN ('ord-1-c1', 'ord-1-c3')`).Scan(&n)
	if n != 2 {
		t.Fatalf("clawback ledger refs = %d", n)
	}
	if err := e.svc.CheckInvariants(e.ctx, e.pool); err != nil {
		t.Fatal(err)
	}
	// 同一退款流水重放：无变化
	var lid int64
	e.pool.QueryRow(e.ctx, `SELECT id FROM wallet_ledger WHERE ref_id = 'ord-1-r3'`).Scan(&lid)
	e.processRefund(lid)
	if e.debt(a) != 290 || len(e.events("ord-1")) != 9 { // accrue, refund, clawback, refund, debt_add, clawback, refund, debt_add, clawback
		t.Fatalf("replay changed state: debt=%d events=%v", e.debt(a), e.events("ord-1"))
	}
}

// Review Focus 3：计提后费率版本改为 20%，退款重算仍按存的 10% 与 reward_cap。
func TestRefundUsesStoredRateNotCurrentVersion(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	e.bind(a, b)
	e.processPay(e.pay(b, "ord-1", 6900))
	e.pool.Exec(e.ctx, `INSERT INTO referral_rate_versions (rate_bp, max_per_invitee, max_per_inviter_30d, hold_days, period_days, effective_from, created_at) VALUES (2000, 50000, 500000, 7, 365, $1, $1)`, e.clk.Now())
	e.clk.Advance(time.Hour)
	e.processRefund(e.refund(b, "ord-1-r1", 3000))
	if r := e.reward("ord-1"); r["reward"] != int64(390) {
		t.Fatalf("must use stored 10%%: %v", r)
	}
}

// Review Focus 1：退款先于 / 没有计提。
func TestRefundWithoutRewardDeferredOrPlaceholdered(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	bid := e.bind(a, b)
	// 没有任何事件的订单：延后（errSkip），不写事件
	e.pay(b, "ord-1", 6900)
	l := e.refund(b, "ord-1-r1", 1000)
	if err := e.processRefund(l); !errors.Is(err, referral.ErrSkip) {
		t.Fatalf("want skip, got %v", err)
	}
	if len(e.events("ord-1")) != 0 {
		t.Fatal("deferred refund must not write events")
	}
	// 返利期后的订单只有 capped 占位 → 退款写 refund(0) 占位
	e.pool.Exec(e.ctx, `UPDATE referral_bindings SET rebate_until = $2 WHERE id = $1`, bid, e.clk.Now().Add(-time.Second))
	e.processPay(e.pay(b, "ord-2", 6900))
	if err := e.processRefund(e.refund(b, "ord-2-r1", 1000)); err != nil {
		t.Fatal(err)
	}
	if ev := e.events("ord-2"); len(ev) != 2 || ev[0] != "capped" || ev[1] != "refund" {
		t.Fatalf("placeholder = %v", ev)
	}
	// 撤销绑定前已发放的订单，撤销后退款仍扣回
	e.pool.Exec(e.ctx, `UPDATE referral_bindings SET rebate_until = $2 WHERE id = $1`, bid, e.clk.Now().Add(365*24*time.Hour))
	e.processPay(e.pay(b, "ord-3", 6900))
	e.markSettled("ord-3", a)
	e.pool.Exec(e.ctx, `UPDATE referral_bindings SET status = 'revoked' WHERE id = $1`, bid)
	e.processRefund(e.refund(b, "ord-3-r1", 6900))
	if r := e.reward("ord-3"); r["clawed"] != int64(690) || e.ledgerSum(a, "referral_clawback") != -690 {
		t.Fatalf("revoked binding refund = %v wallet = %d", r, e.ledgerSum(a, "referral_clawback"))
	}
}
```

（补导入 `spark-cloud/internal/wallet`、`spark-cloud/internal/platform/dbtest`。`CheckInvariants` 在本任务 Step 4 实现（`invariants.go`），之后所有涉及资金的测试都在结束时调用它。）

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/referral/ -run 'TestOrderIDOf|TestRefund' 2>&1 | tail -3`
Expected: 编译错误。

- [ ] **Step 3: `refund.go`**

```go
package referral

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/wallet"
)

var refundSuffixRe = regexp.MustCompile(`-r\d+$`)

// OrderIDOf 从退款流水的 ref_id 取订单号：refundAcross 写 <订单号>-r<n>，refundAll 写 <订单号>。
func OrderIDOf(refID string) string { return refundSuffixRe.ReplaceAllString(refID, "") }

// applyRefund 处理一条 refund 流水（规格 §4.3）：重算应得；未发放只改数字；已发放多出的部分从邀请人余额扣回，不足记欠账。
func (s *Service) applyRefund(ctx context.Context, tx pgx.Tx, now time.Time, l ledgerRow) error {
	if done, err := alreadyProcessed(ctx, tx, l.ID); err != nil || done {
		return err
	}
	order := OrderIDOf(l.RefID)
	var inviterID int64
	err := tx.QueryRow(ctx, `SELECT inviter_id FROM referral_rewards WHERE order_id = $1`, order).Scan(&inviterID)
	if db.IsNoRows(err) {
		// 没有 reward 行：订单已有 capped(period_over) / void(binding revoked) 占位 → 写 refund(0) 占位；什么都没有 → 延后（计提可能还没落地）
		var bindingID int64
		err := tx.QueryRow(ctx, `SELECT e.binding_id, b.inviter_id FROM referral_events e JOIN referral_bindings b ON b.id = e.binding_id
			WHERE e.order_id = $1 AND e.kind IN ('capped', 'void') AND e.source_ledger_id IS NOT NULL ORDER BY e.id LIMIT 1`, order).Scan(&bindingID, &inviterID)
		if db.IsNoRows(err) {
			return errSkip
		}
		if err != nil {
			return err
		}
		if _, err := lockAccount(ctx, tx, inviterID); err != nil {
			return err
		}
		return addEvent(ctx, tx, now, order, bindingID, EventRefund, l.ID, 0, 0, "no reward")
	}
	if err != nil {
		return err
	}
	if _, err := lockAccount(ctx, tx, inviterID); err != nil {
		return err
	}
	r, err := lockReward(ctx, tx, order)
	if err != nil {
		return err
	}
	refunded := r.RefundedAmount + l.Amount
	if refunded > r.OrderAmount {
		s.d.Log.Error("referral: refunds exceed order amount, clamping", "order", order, "refunded", refunded, "order_amount", r.OrderAmount)
		refunded = r.OrderAmount
	}
	target := min(r.RewardCap, (r.OrderAmount-refunded)*int64(r.RateBP)/10000)
	if err := addEvent(ctx, tx, now, order, r.BindingID, EventRefund, l.ID, 0, r.RewardAmt-target, fmt.Sprintf("refund=%d target=%d", l.Amount, target)); err != nil {
		return err
	}
	switch r.Status {
	case StatusPending:
		_, err = tx.Exec(ctx, `UPDATE referral_rewards SET reward = $2, refunded_amount = $3, updated_at = $4 WHERE order_id = $1`, order, target, refunded, now)
		return err
	case StatusVoid:
		_, err = tx.Exec(ctx, `UPDATE referral_rewards SET refunded_amount = $2, updated_at = $3 WHERE order_id = $1`, order, refunded, now)
		return err
	}
	// settled
	excess := (r.SettledAmount - r.ClawedAmount) - target
	clawed := r.ClawedAmount
	if excess > 0 {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM referral_events WHERE order_id = $1 AND kind = 'clawback'`, order).Scan(&n); err != nil {
			return err
		}
		masked, err := s.inviteeMasked(ctx, tx, r.InviteeID)
		if err != nil {
			return err
		}
		refID := order + "-c" + strconv.Itoa(n+1)
		applied, _, err := s.d.Wallet.ApplyUpTo(ctx, tx, wallet.Entry{UserID: inviterID, Amount: -excess, Kind: wallet.KindReferralClawback,
			RefType: RefType, RefID: refID, Remark: "返利扣回 " + masked + " 订单 " + order})
		if err != nil {
			return err
		}
		var ledgerID int64
		if applied > 0 {
			if err := tx.QueryRow(ctx, `SELECT id FROM wallet_ledger WHERE kind = 'referral_clawback' AND ref_type = $1 AND ref_id = $2`, RefType, refID).Scan(&ledgerID); err != nil {
				return err
			}
		}
		if shortfall := excess - applied; shortfall > 0 {
			if _, err := tx.Exec(ctx, `UPDATE referral_accounts SET debt = debt + $2, updated_at = $3 WHERE user_id = $1`, inviterID, shortfall, now); err != nil {
				return err
			}
			if err := addEvent(ctx, tx, now, order, r.BindingID, EventDebtAdd, 0, 0, shortfall, "balance short on clawback"); err != nil {
				return err
			}
		}
		if err := addEvent(ctx, tx, now, order, r.BindingID, EventClawback, 0, ledgerID, excess, refID); err != nil {
			return err
		}
		clawed += excess
	}
	_, err = tx.Exec(ctx, `UPDATE referral_rewards SET reward = $2, refunded_amount = $3, clawed_amount = $4, updated_at = $5 WHERE order_id = $1`, order, target, refunded, clawed, now)
	return err
}
```

（`refund.go` 的导入块不需要 `apperr`。）

- [ ] **Step 4: `invariants.go`（规格 §14 五条；静止态检查）**

```go
package referral

import (
	"context"
	"errors"
	"fmt"

	"spark-cloud/internal/platform/db"
)

// CheckInvariants 核对规格 §14 的不变量；任一不成立返回汇总错误。要求系统处于静止态（Settle 已跑到没有待处理流水）。
func (s *Service) CheckInvariants(ctx context.Context, q db.Querier) error {
	var problems []string
	check := func(name, sql string, args ...any) {
		rows, err := q.Query(ctx, sql, args...)
		if err != nil {
			problems = append(problems, name+": "+err.Error())
			return
		}
		defer rows.Close()
		for rows.Next() {
			vals, err := rows.Values()
			if err != nil {
				problems = append(problems, name+": "+err.Error())
				return
			}
			problems = append(problems, fmt.Sprintf("%s: %v", name, vals))
		}
	}
	// 1. 每个邀请人：钱包净额 == Σ(settled − clawed) + debt
	check("inv1 wallet net != book net + debt", `
		SELECT a.user_id, COALESCE(w.net, 0) AS wallet_net, COALESCE(r.net, 0) + a.debt AS book_net
		FROM referral_accounts a
		LEFT JOIN (SELECT user_id, sum(amount) AS net FROM wallet_ledger WHERE kind IN ('referral_rebate', 'referral_clawback') GROUP BY user_id) w ON w.user_id = a.user_id
		LEFT JOIN (SELECT inviter_id, sum(settled_amount - clawed_amount) AS net FROM referral_rewards GROUP BY inviter_id) r ON r.inviter_id = a.user_id
		WHERE COALESCE(w.net, 0) <> COALESCE(r.net, 0) + a.debt`)
	// 2. 行内一致性
	check("inv2 reward row inconsistent", `
		SELECT order_id, status, reward, settled_amount, clawed_amount FROM referral_rewards
		WHERE (status = 'settled' AND reward <> settled_amount - clawed_amount)
		   OR (status = 'pending' AND (settled_amount <> 0 OR clawed_amount <> 0 OR settled_at IS NOT NULL))
		   OR (status = 'void' AND EXISTS (SELECT 1 FROM wallet_ledger l WHERE l.ref_type = 'referral' AND l.kind = 'referral_rebate' AND l.ref_id = referral_rewards.order_id))`)
	// 3. refunded_amount == Σ 已处理的 refund 流水
	check("inv3 refunded_amount != processed refunds", `
		SELECT r.order_id, r.refunded_amount, COALESCE(sum(l.amount), 0) FROM referral_rewards r
		LEFT JOIN referral_events e ON e.order_id = r.order_id AND e.kind = 'refund' AND e.source_ledger_id IS NOT NULL
		LEFT JOIN wallet_ledger l ON l.id = e.source_ledger_id
		GROUP BY r.order_id, r.refunded_amount HAVING r.refunded_amount <> COALESCE(sum(l.amount), 0)`)
	// 4. 静止态：被绑定用户的每条 order 流水都恰有一条事件（唯一索引保证 ≤ 1；这里查 = 0 的）
	check("inv4 unprocessed order_pay ledger", `
		SELECT l.id, l.ref_id FROM wallet_ledger l JOIN referral_bindings b ON b.invitee_id = l.user_id
		WHERE l.ref_type = 'order' AND l.kind = 'order_pay' AND NOT EXISTS (SELECT 1 FROM referral_events e WHERE e.source_ledger_id = l.id)`)
	check("inv4 unprocessed refund ledger", `
		SELECT l.id, l.ref_id FROM wallet_ledger l JOIN referral_rewards r ON r.order_id = regexp_replace(l.ref_id, '-r\d+$', '')
		WHERE l.ref_type = 'order' AND l.kind = 'refund' AND NOT EXISTS (SELECT 1 FROM referral_events e WHERE e.source_ledger_id = l.id)`)
	check("inv4 event kind vs ledger kind", `
		SELECT e.id, e.kind, l.kind FROM referral_events e JOIN wallet_ledger l ON l.id = e.source_ledger_id
		WHERE (l.kind = 'order_pay' AND e.kind NOT IN ('accrue', 'capped', 'void')) OR (l.kind = 'refund' AND e.kind <> 'refund')`)
	// 5. 上限（按该绑定 / 邀请人用到的版本中最大的上限，保守）
	check("inv5 per-invitee cap exceeded", `
		SELECT r.binding_id, sum(r.reward), max(v.max_per_invitee) FROM referral_rewards r JOIN referral_rate_versions v ON v.id = r.rate_version_id
		WHERE r.status <> 'void' GROUP BY r.binding_id HAVING sum(r.reward) > max(v.max_per_invitee)`)
	check("inv5 per-inviter 30d cap exceeded", `
		SELECT r.inviter_id, sum(r.settled_amount - r.clawed_amount), max(v.max_per_inviter_30d) FROM referral_rewards r JOIN referral_rate_versions v ON v.id = r.rate_version_id
		WHERE r.status = 'settled' AND r.settled_at > $1 GROUP BY r.inviter_id HAVING sum(r.settled_amount - r.clawed_amount) > max(v.max_per_inviter_30d)`, s.d.Clock.Now().Add(-InviterWindow))
	check("balance negative", `SELECT user_id, balance FROM wallet_accounts WHERE balance < 0`)
	if len(problems) > 0 {
		return errors.New("referral invariants violated:\n" + fmt.Sprint(problems))
	}
	return nil
}
```

- [ ] **Step 5: 运行测试确认通过**

Run: `go vet ./internal/referral/ && go test ./internal/referral/ 2>&1 | tail -5`
Expected: `ok`。

- [ ] **Step 6: 提交**

```bash
git add internal/referral/refund.go internal/referral/refund_test.go internal/referral/invariants.go internal/referral/export_test.go
git commit -m "feat(referral): refund handling with clawback via ApplyUpTo, debt, placeholders; CheckInvariants

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 9: 发放 `Release`（门槛、欠账抵销、`ref_id`、重放）

**Files:**
- Create: `internal/referral/release.go`、`internal/referral/release_test.go`

**Interfaces:**
- Consumes: Task 7 / 8 的 `lockAccount / lockReward / addEvent / inviteeMasked / Reward`；`WalletOps.Apply`；`UserLookup.UserState`。
- Produces: `func (s *Service) Release(ctx context.Context) (released int, err error)`（规格 §4.4）：候选 `SELECT order_id, inviter_id FROM referral_rewards WHERE status = 'pending' AND settle_after <= $1 ORDER BY settle_after, order_id LIMIT 200`（无锁）；逐行 `releaseOne(ctx, order, inviter)` 独立事务（`40P01/40001` 重试一次），单行错误记日志并继续；返回成功发放（含 void）的行数。`releaseOne`：`lockAccount(inviter)` → `SELECT … FROM referral_rewards WHERE order_id = $1 AND status = 'pending' AND settle_after <= $2 FOR UPDATE SKIP LOCKED`（无行 → 返回 nil，不计数）→ 绑定 `status`（`SELECT status FROM referral_bindings WHERE id = $1`）≠ active → 跳过；账户 `status` ≠ active → 跳过；`Users.UserState(inviter)`：`Status != "active" || KYCStatus != "verified"` → 跳过 → `reward == 0` → `status='void'` + `addEvent(void, detail 'zero after refund')` → `offset := min(debt, reward)`；`offset > 0` → `debt −= offset` + `addEvent(debt_offset, amount offset)` → `credit := reward − offset`；`credit > 0` → `Wallet.Apply(Entry{inviter, +credit, KindReferralRebate, RefType, order, Remark "邀请返利 <masked> 订单 <order>"})`（`ErrDuplicate` → 视为已入账：查 `wallet_ledger.id`，继续，记日志 warn）→ `addEvent(settle, amount reward, wallet_ledger_id)` → `UPDATE status='settled', settled_amount = reward, settled_at = now`。

- [ ] **Step 1: 写失败的测试**

`internal/referral/release_test.go`：

```go
package referral_test

import (
	"testing"
	"time"

	"spark-cloud/internal/platform/dbtest"
)

func TestReleaseDueRewardsWithThresholds(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	bid := e.bind(a, b)
	e.processPay(e.pay(b, "ord-1", 6900)) // settle_after = T0 + 7d
	if n, err := e.svc.Release(e.ctx); err != nil || n != 0 {
		t.Fatalf("not due: %d %v", n, err)
	}
	e.clk.Advance(7*24*time.Hour - time.Second)
	if n, _ := e.svc.Release(e.ctx); n != 0 {
		t.Fatal("1s before settle_after must not release")
	}
	e.clk.Advance(time.Second)
	// 门槛：绑定 held
	e.pool.Exec(e.ctx, `UPDATE referral_bindings SET status = 'held' WHERE id = $1`, bid)
	if n, _ := e.svc.Release(e.ctx); n != 0 || e.reward("ord-1")["status"] != "pending" {
		t.Fatal("held binding must not release")
	}
	e.pool.Exec(e.ctx, `UPDATE referral_bindings SET status = 'active' WHERE id = $1`, bid)
	// 门槛：账户冻结
	e.pool.Exec(e.ctx, `UPDATE referral_accounts SET status = 'frozen' WHERE user_id = $1`, a)
	if n, _ := e.svc.Release(e.ctx); n != 0 {
		t.Fatal("frozen account must not release")
	}
	e.pool.Exec(e.ctx, `UPDATE referral_accounts SET status = 'active' WHERE user_id = $1`, a)
	// 门槛：邀请人未实名 / 封禁
	e.pool.Exec(e.ctx, `UPDATE users SET kyc_status = 'none' WHERE id = $1`, a)
	if n, _ := e.svc.Release(e.ctx); n != 0 {
		t.Fatal("unverified inviter must not release")
	}
	e.pool.Exec(e.ctx, `UPDATE users SET kyc_status = 'verified', status = 'banned' WHERE id = $1`, a)
	if n, _ := e.svc.Release(e.ctx); n != 0 {
		t.Fatal("banned inviter must not release")
	}
	e.pool.Exec(e.ctx, `UPDATE users SET status = 'active' WHERE id = $1`, a)
	// 全部满足 → 发放
	n, err := e.svc.Release(e.ctx)
	if err != nil || n != 1 {
		t.Fatalf("release = %d %v", n, err)
	}
	r := e.reward("ord-1")
	if r["status"] != "settled" || r["settled"] != int64(690) || r["reward"] != int64(690) {
		t.Fatalf("after release = %v", r)
	}
	var amount int64
	var refType, refID, remark string
	e.pool.QueryRow(e.ctx, `SELECT amount, ref_type, ref_id, remark FROM wallet_ledger WHERE user_id = $1 AND kind = 'referral_rebate'`, a).Scan(&amount, &refType, &refID, &remark)
	if amount != 690 || refType != "referral" || refID != "ord-1" || remark != "邀请返利 139****0001 订单 ord-1" {
		t.Fatalf("ledger = %d %s %s %q", amount, refType, refID, remark)
	}
	if ev := e.events("ord-1"); ev[len(ev)-1] != "settle" {
		t.Fatalf("events = %v", ev)
	}
	// 再跑一次：没有候选
	if n, _ := e.svc.Release(e.ctx); n != 0 {
		t.Fatal("already settled must not release again")
	}
	if err := e.svc.CheckInvariants(e.ctx, e.pool); err != nil {
		t.Fatal(err)
	}
}

func TestReleaseZeroVoidsAndDebtOffset(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	e.bind(a, b)
	// 全退 → 0 → 到期 void
	e.processPay(e.pay(b, "ord-0", 6900))
	e.processRefund(e.refund(b, "ord-0-r1", 6900))
	// 欠账 300：ord-1 发放 690 中抵 300、入账 390
	e.processPay(e.pay(b, "ord-1", 6900))
	e.pool.Exec(e.ctx, `UPDATE referral_accounts SET debt = 300 WHERE user_id = $1`, a)
	// 欠账大于返利：ord-2 reward 100（改 cap 模拟）全抵，钱包无入账
	e.processPay(e.pay(b, "ord-2", 6900))
	e.pool.Exec(e.ctx, `UPDATE referral_rewards SET reward = 100, reward_cap = 100 WHERE order_id = 'ord-2'`)
	e.clk.Advance(7 * 24 * time.Hour)
	n, err := e.svc.Release(e.ctx)
	if err != nil || n != 3 {
		t.Fatalf("release = %d %v", n, err)
	}
	if e.reward("ord-0")["status"] != "void" {
		t.Fatalf("zero reward = %v", e.reward("ord-0"))
	}
	// 顺序按 settle_after, order_id：ord-1 先（抵 300，入 390，欠 0），ord-2 后（欠 0 → 全额入 100）
	if r := e.reward("ord-1"); r["status"] != "settled" || r["settled"] != int64(690) {
		t.Fatalf("ord-1 = %v", r)
	}
	if e.ledgerSum(a, "referral_rebate") != 390+100 || e.debt(a) != 0 {
		t.Fatalf("wallet = %d debt = %d", e.ledgerSum(a, "referral_rebate"), e.debt(a))
	}
	if ev := e.events("ord-1"); ev[len(ev)-2] != "debt_offset" || ev[len(ev)-1] != "settle" {
		t.Fatalf("ord-1 events = %v", ev)
	}
	if err := e.svc.CheckInvariants(e.ctx, e.pool); err != nil {
		t.Fatal(err)
	}
	// 欠账 > 返利：再造一笔并把欠账设为 1000
	e.processPay(e.pay(b, "ord-3", 6900))
	e.pool.Exec(e.ctx, `UPDATE referral_accounts SET debt = 1000 WHERE user_id = $1`, a)
	e.clk.Advance(7 * 24 * time.Hour)
	e.svc.Release(e.ctx)
	if r := e.reward("ord-3"); r["status"] != "settled" || r["settled"] != int64(690) || e.debt(a) != 310 || e.ledgerSum(a, "referral_rebate") != 490 {
		t.Fatalf("debt > reward: %v debt=%d wallet=%d", r, e.debt(a), e.ledgerSum(a, "referral_rebate"))
	}
	// 发放流水重放保护：手动把 ord-3 改回 pending 再发 → wallet_ledger_ref_uniq 命中，仍置 settled、不双入账
	e.pool.Exec(e.ctx, `UPDATE referral_rewards SET status = 'pending', settled_amount = 0, settled_at = NULL WHERE order_id = 'ord-3'`)
	e.pool.Exec(e.ctx, `UPDATE referral_accounts SET debt = 0 WHERE user_id = $1`, a)
	e.svc.Release(e.ctx)
	if e.ledgerSum(a, "referral_rebate") != 490 || e.reward("ord-3")["status"] != "settled" {
		t.Fatalf("replay = wallet %d status %v", e.ledgerSum(a, "referral_rebate"), e.reward("ord-3")["status"])
	}
	_ = dbtest.T0
}

func TestReleaseBatchLimit(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	e.bind(a, b)
	for i := 0; i < 205; i++ {
		e.processPay(e.pay(b, fmt.Sprintf("ord-%03d", i), 100))
	}
	e.clk.Advance(7 * 24 * time.Hour)
	if n, _ := e.svc.Release(e.ctx); n != 200 {
		t.Fatalf("first round = %d", n)
	}
	if n, _ := e.svc.Release(e.ctx); n != 5 {
		t.Fatalf("second round = %d", n)
	}
}
```

（补导入 `fmt`。）

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/referral/ -run TestRelease 2>&1 | tail -3`
Expected: 编译错误 `e.svc.Release undefined`。

- [ ] **Step 3: `release.go`**

```go
package referral

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/wallet"
)

func pgCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// withRetry 对死锁检测（40P01）与序列化失败（40001）把整个事务重试一次。
func withRetry(fn func() error) error {
	err := fn()
	if c := pgCode(err); c == "40P01" || c == "40001" {
		err = fn()
	}
	return err
}

// Release 发放到期的待发放返利（规格 §4.4）：每轮最多 ReleaseBatch 行，逐行独立事务；返回本轮置为 settled / void 的行数。
func (s *Service) Release(ctx context.Context) (int, error) {
	now := s.d.Clock.Now()
	rows, err := s.d.Pool.Query(ctx, `SELECT order_id, inviter_id FROM referral_rewards WHERE status = 'pending' AND settle_after <= $1
		ORDER BY settle_after, order_id LIMIT $2`, now, ReleaseBatch)
	if err != nil {
		return 0, err
	}
	type cand struct {
		order   string
		inviter int64
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (cand, error) {
		var c cand
		err := r.Scan(&c.order, &c.inviter)
		return c, err
	})
	if err != nil {
		return 0, err
	}
	released := 0
	var errs []error
	for _, c := range list {
		done := false
		err := withRetry(func() error {
			done = false
			return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
				var err error
				done, err = s.releaseOne(ctx, tx, now, c.order, c.inviter)
				return err
			})
		})
		if err != nil {
			s.d.Log.Error("referral: release failed", "order", c.order, "err", err)
			errs = append(errs, fmt.Errorf("release %s: %w", c.order, err))
			continue
		}
		if done {
			released++
		}
	}
	return released, errors.Join(errs...)
}

// releaseOne 返回是否把该行推进到了终态（settled / void）。门槛不满足时返回 false, nil（下轮再看）。
func (s *Service) releaseOne(ctx context.Context, tx pgx.Tx, now time.Time, order string, inviterID int64) (bool, error) {
	acc, err := lockAccount(ctx, tx, inviterID)
	if err != nil {
		return false, err
	}
	r, err := scanReward(tx.QueryRow(ctx, `SELECT `+rewardCols+` FROM referral_rewards WHERE order_id = $1 AND status = 'pending' AND settle_after <= $2 FOR UPDATE SKIP LOCKED`, order, now))
	if db.IsNoRows(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var bindingStatus string
	if err := tx.QueryRow(ctx, `SELECT status FROM referral_bindings WHERE id = $1`, r.BindingID).Scan(&bindingStatus); err != nil {
		return false, err
	}
	if bindingStatus != BindingActive || acc.Status != AccountActive {
		return false, nil
	}
	u, err := s.d.Users.UserState(ctx, tx, inviterID)
	if err != nil {
		return false, err
	}
	if u.Status != "active" || u.KYCStatus != "verified" {
		return false, nil
	}
	if r.RewardAmt == 0 {
		if _, err := tx.Exec(ctx, `UPDATE referral_rewards SET status = 'void', updated_at = $2 WHERE order_id = $1`, order, now); err != nil {
			return false, err
		}
		return true, addEvent(ctx, tx, now, order, r.BindingID, EventVoid, 0, 0, 0, "zero after refund")
	}
	offset := min(acc.Debt, r.RewardAmt)
	if offset > 0 {
		if _, err := tx.Exec(ctx, `UPDATE referral_accounts SET debt = debt - $2, updated_at = $3 WHERE user_id = $1`, inviterID, offset, now); err != nil {
			return false, err
		}
		if err := addEvent(ctx, tx, now, order, r.BindingID, EventDebtOffset, 0, 0, offset, "offset debt"); err != nil {
			return false, err
		}
	}
	var ledgerID int64
	if credit := r.RewardAmt - offset; credit > 0 {
		// 重放保护：发放流水已存在（上一次事务在写流水之后失败且被人工改回 pending 等异常）则不再入账。
		// 不能靠捕获 Apply 的 ErrDuplicate：唯一冲突会让 PG 把事务置为失败态，之后的语句都会报 25P02。
		err := tx.QueryRow(ctx, `SELECT id FROM wallet_ledger WHERE kind = 'referral_rebate' AND ref_type = $1 AND ref_id = $2`, RefType, order).Scan(&ledgerID)
		switch {
		case err == nil:
			s.d.Log.Warn("referral: rebate ledger already exists, marking settled without crediting again", "order", order, "ledger", ledgerID)
		case db.IsNoRows(err):
			masked, err := s.inviteeMasked(ctx, tx, r.InviteeID)
			if err != nil {
				return false, err
			}
			if _, err := s.d.Wallet.Apply(ctx, tx, wallet.Entry{UserID: inviterID, Amount: credit, Kind: wallet.KindReferralRebate, RefType: RefType, RefID: order,
				Remark: "邀请返利 " + masked + " 订单 " + order}); err != nil {
				return false, err
			}
			if err := tx.QueryRow(ctx, `SELECT id FROM wallet_ledger WHERE kind = 'referral_rebate' AND ref_type = $1 AND ref_id = $2`, RefType, order).Scan(&ledgerID); err != nil {
				return false, err
			}
		default:
			return false, err
		}
	}
	if err := addEvent(ctx, tx, now, order, r.BindingID, EventSettle, 0, ledgerID, r.RewardAmt, fmt.Sprintf("offset=%d", offset)); err != nil {
		return false, err
	}
	_, err = tx.Exec(ctx, `UPDATE referral_rewards SET status = 'settled', settled_amount = reward, settled_at = $2, updated_at = $2 WHERE order_id = $1`, order, now)
	return true, err
}
```

（`apperr` 导入在 `releaseOne` 里不再需要时删除。）

- [ ] **Step 4: 运行测试确认通过**

Run: `go vet ./internal/referral/ && go test ./internal/referral/ 2>&1 | tail -5`
Expected: `ok`。

- [ ] **Step 5: 提交**

```bash
git add internal/referral/release.go internal/referral/release_test.go
git commit -m "feat(referral): Release due rewards with thresholds, debt offset, idempotent rebate ledger

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 10: 扫描 `Discover`、水位、`processRow`、`Settle` 编排；装配（`app.go` / `main.go` / `e2ehooks`）

**Files:**
- Create: `internal/referral/settle.go`、`internal/referral/settle_test.go`
- Modify: `internal/app/app.go`（装配）、`cmd/platform/main.go`（`RunSweeper` 附加任务）、`internal/e2ehooks/hooks.go`（`sweep`）、`internal/e2ehooks/hooks_test.go`（追加断言）、`internal/referral/export_test.go`（`ProcessRowForTest`）

**Interfaces:**
- Consumes: Task 7～9 的 `accrue / applyRefund / Release / errSkip / withRetry / pgCode`。
- Produces:
  - `func (s *Service) Discover(ctx context.Context) (processed int, err error)`：
    ```sql
    SELECT l.id, l.user_id, l.kind, l.amount, l.ref_id, l.created_at
    FROM wallet_ledger l JOIN referral_bindings b ON b.invitee_id = l.user_id
    WHERE l.ref_type = 'order' AND l.kind IN ('order_pay', 'refund')
      AND l.created_at >= (SELECT watermark FROM referral_state WHERE id = 1) - make_interval(secs => $1)
      AND NOT EXISTS (SELECT 1 FROM referral_events e WHERE e.source_ledger_id = l.id)
    ORDER BY (l.kind = 'refund'), l.id LIMIT $2
    ```
    （`$1 = ScanLookback.Seconds()`，`$2 = ScanBatch`）；每行 `processRow`；取到 `< ScanBatch` 行时 `UPDATE referral_state SET watermark = $1 WHERE id = 1`（`now − ScanLag`，只前进不后退：`GREATEST(watermark, $1)`——**但 `Rescan` 需要后退**，所以这里直接 `SET watermark = $1`，`Rescan` 用 `LEAST`）；返回成功处理的行数（含占位事件；`errSkip` 不计）。
  - `func (s *Service) processRow(ctx context.Context, l ledgerRow) error`：`withRetry(db.WithTx(… accrue | applyRefund …))`；`errSkip` → 返回 `errSkip`（调用方只计数不记错）；唯一冲突 `23505` 且约束名 `referral_events_source_uniq` → 视为并发已处理，返回 nil；其它错误记日志并返回。
  - `func (s *Service) Settle(ctx context.Context) error`：`Discover` 后 `Release`，`errors.Join` 两者错误（任一失败不阻止另一个）。
  - `func (s *Service) Rescan(ctx context.Context, actor audit.Actor, days int) error`（后台重扫，Task 11 的 API 调用）：`days ∈ [1, 30]` 否则 `apperr.BadRequest("重扫天数需在 1～30 之间")`；事务内 `UPDATE referral_state SET watermark = LEAST(watermark, $1) WHERE id = 1` + 审计 `referral.rescan`（diff `{days, watermark}`），提交后同步 `Settle`。
  - 装配：`app.go` 在 `ids` / `w` / `r` 之后：`fpKey, err := referral.DeriveFingerprintKey(cfg.SecretKey)`；`identity.Deps` 加 `FingerprintKey: fpKey`；`ref := referral.NewService(referral.Deps{Pool: pool, Clock: clk, Log: log, Wallet: w, Users: ids, Orders: r})`；`ids.SetReferral(ref)`；`App.Referral *referral.Service`；`api.Server.Referral`、`adminapi.Server.Referral`（Task 12 / 13 用，本任务先加字段）。`main.go`：`a.Rental.RunSweeper(ctx, cfg.SweepInterval, a.Billing.Sweep, a.Referral.Settle)`。`e2ehooks.sweep`：`Billing.Sweep` 之后 `if err := s.App.Referral.Settle(ctx); err != nil { return nil, fmt.Errorf("referral settle: %w", err) }`。

- [ ] **Step 1: 写失败的测试**

`internal/referral/export_test.go` 追加：

```go
func (s *Service) ProcessRowForTest(ctx context.Context, l ledgerRow) error { return s.processRow(ctx, l) }
```

`internal/referral/settle_test.go`：

```go
package referral_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/referral"
)

func (e *env) watermark() time.Time {
	e.t.Helper()
	var w time.Time
	if err := e.pool.QueryRow(e.ctx, `SELECT watermark FROM referral_state WHERE id = 1`).Scan(&w); err != nil {
		e.t.Fatal(err)
	}
	return w
}

func TestDiscoverProcessesOnlyBoundUsersAndAdvancesWatermark(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	x := e.user("13900000009", true) // 未绑定
	e.bind(a, b)
	e.pay(b, "ord-b", 6900)
	e.pay(x, "ord-x", 6900)
	n, err := e.svc.Discover(e.ctx)
	if err != nil || n != 1 {
		t.Fatalf("discover = %d %v", n, err)
	}
	if e.reward("ord-b")["reward"] != int64(690) {
		t.Fatal("bound user's pay must accrue")
	}
	var cnt int
	e.pool.QueryRow(e.ctx, `SELECT count(*) FROM referral_events WHERE order_id = 'ord-x'`).Scan(&cnt)
	if cnt != 0 {
		t.Fatal("unbound user's pay must be ignored")
	}
	if !e.watermark().Equal(e.clk.Now().Add(-2 * time.Minute)) {
		t.Fatalf("watermark = %v", e.watermark())
	}
	// 第二轮：无新行，水位继续推进；1 小时回看不重复处理
	e.clk.Advance(10 * time.Minute)
	if n, _ := e.svc.Discover(e.ctx); n != 0 {
		t.Fatalf("second = %d", n)
	}
	e.pool.QueryRow(e.ctx, `SELECT count(*) FROM referral_events WHERE order_id = 'ord-b'`).Scan(&cnt)
	if cnt != 1 {
		t.Fatal("lookback must not double-process")
	}
	// 水位之前 1 小时以外的流水不再被看见（除非 Rescan）；在水位 − 1h 之内的「晚提交」流水会被看见
	old := e.clk.Now().Add(-3 * time.Hour)
	e.pool.Exec(e.ctx, `INSERT INTO wallet_ledger (user_id, amount, balance_after, kind, ref_type, ref_id, remark, created_at) VALUES ($1, -100, 0, 'order_pay', 'order', 'ord-old', '', $2)`, b, old)
	e.orders.m["ord-old"] = fakeOrder{userID: b}
	late := e.clk.Now().Add(-30 * time.Minute)
	e.pool.Exec(e.ctx, `INSERT INTO wallet_ledger (user_id, amount, balance_after, kind, ref_type, ref_id, remark, created_at) VALUES ($1, -100, 0, 'order_pay', 'order', 'ord-late', '', $2)`, b, late)
	e.orders.m["ord-late"] = fakeOrder{userID: b}
	if n, _ := e.svc.Discover(e.ctx); n != 1 {
		t.Fatalf("late commit = %d", n)
	}
	if len(e.events("ord-old")) != 0 || len(e.events("ord-late")) != 1 {
		t.Fatalf("old=%v late=%v", e.events("ord-old"), e.events("ord-late"))
	}
	// Rescan 拨回 1 天：ord-old 被补上；已处理的不重复
	if err := e.svc.Rescan(e.ctx, audit.Actor{AdminID: 1}, 1); err != nil {
		t.Fatal(err)
	}
	if len(e.events("ord-old")) != 1 || len(e.events("ord-late")) != 1 || len(e.events("ord-b")) != 1 {
		t.Fatalf("after rescan old=%v late=%v b=%v", e.events("ord-old"), e.events("ord-late"), e.events("ord-b"))
	}
	for _, d := range []int{0, 31} {
		if err := e.svc.Rescan(e.ctx, audit.Actor{AdminID: 1}, d); err == nil {
			t.Fatalf("days %d must be rejected", d)
		}
	}
	if err := e.svc.CheckInvariants(e.ctx, e.pool); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverBacklogKeepsWatermark(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	e.bind(a, b)
	for i := 0; i < referral.ScanBatch+3; i++ {
		e.pay(b, fmt.Sprintf("ord-%04d", i), 100)
	}
	w0 := e.watermark()
	n, _ := e.svc.Discover(e.ctx)
	if n != referral.ScanBatch || !e.watermark().Equal(w0) {
		t.Fatalf("full batch: n=%d watermark moved=%v", n, !e.watermark().Equal(w0))
	}
	n, _ = e.svc.Discover(e.ctx)
	if n != 3 || e.watermark().Equal(w0) {
		t.Fatalf("drain: n=%d watermark moved=%v", n, !e.watermark().Equal(w0))
	}
}

// Review Focus 1：同一批里退款流水排在它的扣款之后处理，即使 id 更小（人为造 id 顺序）。
func TestDiscoverProcessesPayBeforeRefundInSameBatch(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	e.bind(a, b)
	// 先插退款（id 小），再插扣款（id 大）
	e.pool.Exec(e.ctx, `INSERT INTO wallet_ledger (user_id, amount, balance_after, kind, ref_type, ref_id, remark, created_at) VALUES ($1, 1000, 1000, 'refund', 'order', 'ord-1-r1', '', $2)`, b, e.clk.Now())
	e.pay(b, "ord-1", 6900)
	if n, _ := e.svc.Discover(e.ctx); n != 2 {
		t.Fatalf("n = %d", n)
	}
	if r := e.reward("ord-1"); r["reward"] != int64(590) || r["refunded"] != int64(1000) {
		t.Fatalf("reward = %v", r)
	}
	// 订单查不到的扣款：退款被延后；补上订单后下一轮两者都处理
	e.pool.Exec(e.ctx, `INSERT INTO wallet_ledger (user_id, amount, balance_after, kind, ref_type, ref_id, remark, created_at) VALUES ($1, -6900, 0, 'order_pay', 'order', 'ord-2', '', $2), ($1, 500, 500, 'refund', 'order', 'ord-2-r1', '', $2)`, b, e.clk.Now())
	if n, _ := e.svc.Discover(e.ctx); n != 0 || len(e.events("ord-2")) != 0 {
		t.Fatalf("ghost order: n=%d events=%v", n, e.events("ord-2"))
	}
	e.orders.m["ord-2"] = fakeOrder{userID: b}
	if n, _ := e.svc.Discover(e.ctx); n != 2 || e.reward("ord-2")["reward"] != int64(640) {
		t.Fatalf("after order appears: n=%d reward=%v", n, e.reward("ord-2"))
	}
}

func TestSettleRunsDiscoverThenRelease(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	e.bind(a, b)
	e.pool.Exec(e.ctx, `UPDATE referral_rate_versions SET hold_days = 0`)
	e.pay(b, "ord-1", 6900)
	if err := e.svc.Settle(e.ctx); err != nil {
		t.Fatal(err)
	}
	if e.reward("ord-1")["status"] != "settled" || e.ledgerSum(a, "referral_rebate") != 690 {
		t.Fatal("hold_days=0 must accrue and release in the same Settle")
	}
}

func TestProcessRowConcurrentDuplicateIsBenign(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	e.bind(a, b)
	id := e.pay(b, "ord-1", 6900)
	l := referral.LedgerRowOf(id, b, "order_pay", -6900, "ord-1", e.clk.Now())
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := e.svc.ProcessRowForTest(e.ctx, l); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if len(e.events("ord-1")) != 1 {
		t.Fatalf("events = %v", e.events("ord-1"))
	}
	_ = dbtest.T0
}
```

`internal/e2ehooks/hooks_test.go` 追加（参照文件里 `sweep` 的现有用例写法）：在现有 sweep 用例末尾断言 `referral_state.watermark` 不再是 `2000-01-01`（证明 `Referral.Settle` 被调用）。

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/referral/ -run 'TestDiscover|TestSettle|TestProcessRow' 2>&1 | tail -3`
Expected: 编译错误。

- [ ] **Step 3: `settle.go`**

```go
package referral

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
)

// Discover 扫描 wallet_ledger 中新的 order 流水（规格 §5.1）：时间水位 + 1 小时回看 + source_ledger_id 幂等；
// 每行独立事务；取到少于 ScanBatch 行时把水位推进到 now − ScanLag。返回成功处理（含占位）的行数。
func (s *Service) Discover(ctx context.Context) (int, error) {
	now := s.d.Clock.Now()
	rows, err := s.d.Pool.Query(ctx, `SELECT l.id, l.user_id, l.kind, l.amount, l.ref_id, l.created_at
		FROM wallet_ledger l JOIN referral_bindings b ON b.invitee_id = l.user_id
		WHERE l.ref_type = 'order' AND l.kind IN ('order_pay', 'refund')
		  AND l.created_at >= (SELECT watermark FROM referral_state WHERE id = 1) - make_interval(secs => $1)
		  AND NOT EXISTS (SELECT 1 FROM referral_events e WHERE e.source_ledger_id = l.id)
		ORDER BY (l.kind = 'refund'), l.id LIMIT $2`, ScanLookback.Seconds(), ScanBatch)
	if err != nil {
		return 0, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ledgerRow, error) {
		var l ledgerRow
		err := r.Scan(&l.ID, &l.UserID, &l.Kind, &l.Amount, &l.RefID, &l.CreatedAt)
		return l, err
	})
	if err != nil {
		return 0, err
	}
	processed := 0
	var errs []error
	for _, l := range list {
		err := s.processRow(ctx, l)
		switch {
		case err == nil:
			processed++
		case errors.Is(err, errSkip):
		default:
			errs = append(errs, fmt.Errorf("ledger %d: %w", l.ID, err))
		}
	}
	if len(list) < ScanBatch {
		if _, err := s.d.Pool.Exec(ctx, `UPDATE referral_state SET watermark = $1 WHERE id = 1`, now.Add(-ScanLag)); err != nil {
			errs = append(errs, fmt.Errorf("advance watermark: %w", err))
		}
	}
	return processed, errors.Join(errs...)
}

// processRow 在独立事务里处理一条流水；并发重复（撞 referral_events_source_uniq）视为已处理。
func (s *Service) processRow(ctx context.Context, l ledgerRow) error {
	now := s.d.Clock.Now()
	err := withRetry(func() error {
		return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
			if l.Kind == "refund" {
				return s.applyRefund(ctx, tx, now, l)
			}
			return s.accrue(ctx, tx, now, l)
		})
	})
	if err == nil || errors.Is(err, errSkip) {
		return err
	}
	if pgCode(err) == "23505" && db.ConstraintName(err) == "referral_events_source_uniq" {
		return nil
	}
	s.d.Log.Error("referral: process ledger failed", "ledger", l.ID, "kind", l.Kind, "ref", l.RefID, "err", err)
	return err
}

// Settle 是清扫附加任务：先发现（计提 / 退款处理）再发放。两步独立，任一失败不阻止另一步。
func (s *Service) Settle(ctx context.Context) error {
	_, derr := s.Discover(ctx)
	_, rerr := s.Release(ctx)
	return errors.Join(derr, rerr)
}

// Rescan 把水位拨回 days 天并立刻跑一轮 Settle；已处理的流水因 source_ledger_id 唯一不会重复计提。
func (s *Service) Rescan(ctx context.Context, actor audit.Actor, days int) error {
	if days < 1 || days > 30 {
		return apperr.BadRequest("重扫天数需在 1～30 之间")
	}
	now := s.d.Clock.Now()
	target := now.Add(-time.Duration(days) * 24 * time.Hour)
	if err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE referral_state SET watermark = LEAST(watermark, $1) WHERE id = 1`, target); err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("referral.rescan", "referral_state", "1", map[string]any{"days": days, "watermark": target}))
	}); err != nil {
		return err
	}
	return s.Settle(ctx)
}
```

- [ ] **Step 4: 装配**

`internal/app/app.go`：

```go
	fpKey, err := referral.DeriveFingerprintKey(cfg.SecretKey)
	if err != nil {
		return nil, err
	}
	ids := identity.NewService(identity.Deps{Pool: pool, Clock: clk, SMS: smsSender, KYC: verifier, Box: box, Uploads: uploads, FixedCode: fixedCode, CodeIPHourLimit: ipHourLimit, FingerprintKey: fpKey})
	…（w、c、f、r 不变）
	ref := referral.NewService(referral.Deps{Pool: pool, Clock: clk, Log: log, Wallet: w, Users: ids, Orders: r})
	ids.SetReferral(ref)
```

`api.Server{…, Referral: ref}`、`adminapi.Server{…, Referral: ref}`（两个 `Server` 结构各加字段 `Referral *referral.Service`，本任务只加字段不加路由）；`App` 加 `Referral *referral.Service` 并在返回值里赋值。

`cmd/platform/main.go`：`a.Rental.RunSweeper(ctx, cfg.SweepInterval, a.Billing.Sweep, a.Referral.Settle)`。

`internal/e2ehooks/hooks.go` 的 `sweep`：

```go
	if err := s.App.Billing.Sweep(ctx); err != nil {
		return nil, fmt.Errorf("billing sweep: %w", err)
	}
	if err := s.App.Referral.Settle(ctx); err != nil {
		return nil, fmt.Errorf("referral settle: %w", err)
	}
```

注释改为「再跑一轮 Rental.Sweep + Billing.Sweep + Referral.Settle」。

- [ ] **Step 5: 运行测试确认通过**

Run: `go vet ./... && go test -race ./internal/referral/ && go test ./internal/app/ ./internal/e2ehooks/ ./cmd/... 2>&1 | tail -8`
Expected: 全部 `ok`。

- [ ] **Step 6: 提交**

```bash
git add internal/referral/settle.go internal/referral/settle_test.go internal/referral/export_test.go internal/app/app.go cmd/platform/main.go internal/e2ehooks/hooks.go internal/e2ehooks/hooks_test.go internal/api/server.go internal/adminapi/server.go
git commit -m "feat(referral): Discover with time watermark + lookback, processRow, Settle, Rescan; wire into app, sweeper and e2e hooks

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 11: 后台裁决：冻结 / 解冻 / 通过 / 撤销（审计、状态机）

**Files:**
- Create: `internal/referral/admin.go`、`internal/referral/admin_test.go`

**Interfaces:**
- Consumes: Task 6 的 `revokeBinding`；Task 3 的 `lockAccount`；Task 9 的 `Release`（冻结测试）。
- Produces（`package referral`）：
  - `func (s *Service) Freeze(ctx context.Context, actor audit.Actor, userID int64, reason string) error`：`reason` 去空白后必填（`apperr.BadRequest("请填写冻结原因")`）；事务：`lockAccount`（无行 → `apperr.ErrNotFound`）；已冻结 → `apperr.ErrReferralBindingState`；`UPDATE referral_accounts SET status='frozen', frozen_reason, frozen_by, frozen_at, updated_at`；审计 `referral.freeze`（target `referral_account/<user_id>`，diff `{reason}`）。
  - `func (s *Service) Unfreeze(ctx, actor, userID int64) error`：未冻结 → `ErrReferralBindingState`；清 `frozen_*`，`status='active'`；审计 `referral.unfreeze`。
  - `func (s *Service) Approve(ctx, actor, bindingID int64, note string) error`：`SELECT inviter_id, status FROM referral_bindings WHERE id = $1`（无 → `ErrNotFound`）→ `lockAccount(inviter)` → 状态 ≠ `held` → `ErrReferralBindingState` → `UPDATE status='active', hold_reason='', reviewed_by, reviewed_at, review_note, updated_at`；审计 `referral.approve`。
  - `func (s *Service) Revoke(ctx, actor, bindingID int64, reason string) (voided int64, err error)`：`reason` 必填；`lockAccount(inviter)` → `revokeBinding(ctx, tx, now, bindingID, reason, actor, "", "referral.revoke")`（已撤销 → `ErrReferralBindingState`）。
  - 同一轮内冻结与发放的互斥由 `lockAccount` 保证（Review Focus 4）。

- [ ] **Step 1: 写失败的测试**

`internal/referral/admin_test.go`：

```go
package referral_test

import (
	"errors"
	"testing"
	"time"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
)

func (e *env) lastAudit() string {
	e.t.Helper()
	var action string
	if err := e.pool.QueryRow(e.ctx, `SELECT action FROM audit_logs ORDER BY id DESC LIMIT 1`).Scan(&action); err != nil {
		e.t.Fatal(err)
	}
	return action
}

// Review Focus 4：冻结期间到期返利不发放（继续累计），解冻后下一轮发放。
func TestFreezeBlocksReleaseUntilUnfreeze(t *testing.T) {
	e := newEnv(t)
	admin := audit.Actor{AdminID: 9, IP: "10.0.0.1"}
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	e.bind(a, b)
	e.processPay(e.pay(b, "ord-1", 6900))
	if err := e.svc.Freeze(e.ctx, admin, a, "  可疑  "); err != nil || e.lastAudit() != "referral.freeze" {
		t.Fatalf("freeze = %v audit = %s", err, e.lastAudit())
	}
	var status, reason string
	e.pool.QueryRow(e.ctx, `SELECT status, frozen_reason FROM referral_accounts WHERE user_id = $1`, a).Scan(&status, &reason)
	if status != "frozen" || reason != "可疑" {
		t.Fatalf("account = %s %q", status, reason)
	}
	if err := e.svc.Freeze(e.ctx, admin, a, "again"); !errors.Is(err, apperr.ErrReferralBindingState) {
		t.Fatalf("double freeze = %v", err)
	}
	if err := e.svc.Freeze(e.ctx, admin, a, ""); apperr.As(err) == nil || apperr.As(err).Code != "bad_request" {
		t.Fatalf("empty reason = %v", err)
	}
	if err := e.svc.Freeze(e.ctx, admin, 999999, "x"); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("unknown = %v", err)
	}
	// 冻结中：码不可绑定（LookupCode 无效）、到期不发放、新消费仍计提
	if valid, _, _ := e.svc.LookupCode(e.ctx, mustCode(e, a)); valid {
		t.Fatal("frozen code must be invalid")
	}
	e.clk.Advance(8 * 24 * time.Hour)
	e.processPay(e.pay(b, "ord-2", 6900))
	if n, _ := e.svc.Release(e.ctx); n != 0 || e.reward("ord-1")["status"] != "pending" || e.reward("ord-2")["reward"] != int64(690) {
		t.Fatalf("frozen: released=%d r1=%v r2=%v", n, e.reward("ord-1"), e.reward("ord-2"))
	}
	if err := e.svc.Unfreeze(e.ctx, admin, a); err != nil || e.lastAudit() != "referral.unfreeze" {
		t.Fatalf("unfreeze = %v", err)
	}
	if err := e.svc.Unfreeze(e.ctx, admin, a); !errors.Is(err, apperr.ErrReferralBindingState) {
		t.Fatalf("double unfreeze = %v", err)
	}
	if n, _ := e.svc.Release(e.ctx); n != 1 || e.reward("ord-1")["status"] != "settled" {
		t.Fatalf("after unfreeze released=%d", n)
	}
	if err := e.svc.CheckInvariants(e.ctx, e.pool); err != nil {
		t.Fatal(err)
	}
}

func mustCode(e *env, userID int64) string {
	e.t.Helper()
	a, err := e.svc.EnsureCode(e.ctx, userID)
	if err != nil {
		e.t.Fatal(err)
	}
	return a.Code
}

func TestApproveAndRevokeStateMachine(t *testing.T) {
	e := newEnv(t)
	admin := audit.Actor{AdminID: 9}
	a := e.user("13812345678", true)
	b := e.user("13900000001", true)
	bid := e.bind(a, b)
	e.pool.Exec(e.ctx, `UPDATE referral_bindings SET status = 'held', hold_reason = 'same_device' WHERE id = $1`, bid)
	e.processPay(e.pay(b, "ord-1", 6900))
	e.clk.Advance(8 * 24 * time.Hour)
	if n, _ := e.svc.Release(e.ctx); n != 0 {
		t.Fatal("held must not release")
	}
	// approve：held → active，下一轮发放
	if err := e.svc.Approve(e.ctx, admin, bid, "核实为家人"); err != nil || e.lastAudit() != "referral.approve" {
		t.Fatalf("approve = %v", err)
	}
	st, hold, _ := e.bindingRow(b)
	if st != "active" || hold != "" {
		t.Fatalf("after approve = %s %q", st, hold)
	}
	if err := e.svc.Approve(e.ctx, admin, bid, "again"); !errors.Is(err, apperr.ErrReferralBindingState) {
		t.Fatalf("approve active = %v", err)
	}
	if err := e.svc.Approve(e.ctx, admin, 999999, "x"); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("approve unknown = %v", err)
	}
	if n, _ := e.svc.Release(e.ctx); n != 1 {
		t.Fatal("approved must release")
	}
	// revoke：已发放的 ord-1 不动；待发放的 ord-2 作废；voided = 690
	e.processPay(e.pay(b, "ord-2", 6900))
	voided, err := e.svc.Revoke(e.ctx, admin, bid, "刷单")
	if err != nil || voided != 690 || e.lastAudit() != "referral.revoke" {
		t.Fatalf("revoke = %d %v", voided, err)
	}
	if e.reward("ord-1")["status"] != "settled" || e.reward("ord-2")["status"] != "void" {
		t.Fatalf("after revoke r1=%v r2=%v", e.reward("ord-1"), e.reward("ord-2"))
	}
	if st, hold, _ := e.bindingRow(b); st != "revoked" || hold != "刷单" {
		t.Fatalf("binding = %s %q", st, hold)
	}
	if _, err := e.svc.Revoke(e.ctx, admin, bid, "again"); !errors.Is(err, apperr.ErrReferralBindingState) {
		t.Fatalf("double revoke = %v", err)
	}
	if _, err := e.svc.Revoke(e.ctx, admin, bid, " "); apperr.As(err) == nil || apperr.As(err).Code != "bad_request" {
		t.Fatalf("empty reason = %v", err)
	}
	// 撤销后的消费：void 占位，不计提；撤销前已发放订单的退款仍扣回
	e.processPay(e.pay(b, "ord-3", 6900))
	if ev := e.events("ord-3"); len(ev) != 1 || ev[0] != "void" {
		t.Fatalf("post-revoke pay = %v", ev)
	}
	e.processRefund(e.refund(b, "ord-1-r1", 6900))
	if e.ledgerSum(a, "referral_clawback") != -690 {
		t.Fatalf("clawback after revoke = %d", e.ledgerSum(a, "referral_clawback"))
	}
	if err := e.svc.CheckInvariants(e.ctx, e.pool); err != nil {
		t.Fatal(err)
	}
}

// TestRevokeVoidsPendingKeepsSettled 与上面的 revoke 段等价，命名供 Review Focus 4 索引。
func TestRevokeVoidsPendingKeepsSettled(t *testing.T) { TestApproveAndRevokeStateMachine(t) }
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/referral/ -run 'TestFreeze|TestApprove|TestRevoke' 2>&1 | tail -3`
Expected: 编译错误。

- [ ] **Step 3: `admin.go`（裁决部分；列表 / 报表在 Task 13 追加到同文件）**

```go
package referral

import (
	"context"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
)

// Freeze 冻结邀请人：码不再可绑定、返利停止发放（继续计提）。
func (s *Service) Freeze(ctx context.Context, actor audit.Actor, userID int64, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return apperr.BadRequest("请填写冻结原因")
	}
	now := s.d.Clock.Now()
	return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		a, err := lockAccount(ctx, tx, userID)
		if err != nil {
			return err
		}
		if a.Status == AccountFrozen {
			return apperr.ErrReferralBindingState
		}
		if _, err := tx.Exec(ctx, `UPDATE referral_accounts SET status = 'frozen', frozen_reason = $2, frozen_by = $3, frozen_at = $4, updated_at = $4 WHERE user_id = $1`,
			userID, reason, actor.AdminID, now); err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("referral.freeze", "referral_account", strconv.FormatInt(userID, 10), map[string]any{"reason": reason}))
	})
}

func (s *Service) Unfreeze(ctx context.Context, actor audit.Actor, userID int64) error {
	now := s.d.Clock.Now()
	return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		a, err := lockAccount(ctx, tx, userID)
		if err != nil {
			return err
		}
		if a.Status != AccountFrozen {
			return apperr.ErrReferralBindingState
		}
		if _, err := tx.Exec(ctx, `UPDATE referral_accounts SET status = 'active', frozen_reason = '', frozen_by = NULL, frozen_at = NULL, updated_at = $2 WHERE user_id = $1`, userID, now); err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("referral.unfreeze", "referral_account", strconv.FormatInt(userID, 10), map[string]any{"previous_reason": a.FrozenReason}))
	})
}

// bindingOwner 读绑定的邀请人与状态（不加锁；锁由 lockAccount 取）。
func bindingOwner(ctx context.Context, q db.Querier, bindingID int64) (inviterID int64, status string, err error) {
	err = q.QueryRow(ctx, `SELECT inviter_id, status FROM referral_bindings WHERE id = $1`, bindingID).Scan(&inviterID, &status)
	if db.IsNoRows(err) {
		err = apperr.ErrNotFound
	}
	return
}

// Approve 把搁置的绑定转为有效；待发放返利在下一轮正常发放。
func (s *Service) Approve(ctx context.Context, actor audit.Actor, bindingID int64, note string) error {
	now := s.d.Clock.Now()
	note = strings.TrimSpace(note)
	return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		inviterID, _, err := bindingOwner(ctx, tx, bindingID)
		if err != nil {
			return err
		}
		if _, err := lockAccount(ctx, tx, inviterID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE referral_bindings SET status = 'active', hold_reason = '', reviewed_by = $2, reviewed_at = $3, review_note = $4, updated_at = $3
			WHERE id = $1 AND status = 'held'`, bindingID, actor.AdminID, now, note)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return apperr.ErrReferralBindingState
		}
		return audit.Record(ctx, tx, now, actor.Entry("referral.approve", "referral_binding", strconv.FormatInt(bindingID, 10), map[string]any{"note": note}))
	})
}

// Revoke 撤销绑定（任意非 revoked 状态）：待发放全部作废，已发放不自动扣回（财务用调账）；返回作废金额。
func (s *Service) Revoke(ctx context.Context, actor audit.Actor, bindingID int64, reason string) (int64, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return 0, apperr.BadRequest("请填写撤销原因")
	}
	now := s.d.Clock.Now()
	var voided int64
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		inviterID, _, err := bindingOwner(ctx, tx, bindingID)
		if err != nil {
			return err
		}
		if _, err := lockAccount(ctx, tx, inviterID); err != nil {
			return err
		}
		voided, err = s.revokeBinding(ctx, tx, now, bindingID, reason, actor, "", "referral.revoke")
		return err
	})
	return voided, err
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go vet ./internal/referral/ && go test ./internal/referral/ 2>&1 | tail -5`
Expected: `ok`。

- [ ] **Step 5: 提交**

```bash
git add internal/referral/admin.go internal/referral/admin_test.go
git commit -m "feat(referral): admin freeze / unfreeze / approve / revoke with audit and state checks

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 12: 控制台 API（`auth/login` 扩展、`codes/{code}` 限流、`referral/me / invitees / rewards`、`/me` 字段）

**Files:**
- Create: `internal/referral/queries.go`、`internal/referral/queries_test.go`、`internal/api/referral.go`、`internal/api/referral_test.go`
- Modify: `internal/api/auth.go`（`handleLogin`、`handleMe`）、`internal/api/server.go`（路由、`ReferralLimit`）

**Interfaces:**
- Consumes: Task 3 `EnsureCode / LookupCode / CurrentConfig / MaskPhone`；Task 4 `identity.LoginWith / LoginRequest`；Task 7 `Reward`；Task 10 的 `api.Server.Referral`；`api.ipLimiter`。
- Produces（`package referral`，`queries.go`）：
  ```go
  type Stats struct { Invitees, VerifiedInvitees int `json:"invitees","verified_invitees"`; Pending, SettledNet, MonthSettled, Debt int64 `json:"pending","settled_net","month_settled","debt"` }
  type MeView struct { Code string `json:"code"`; Link string `json:"link"`; Status string `json:"status"`; KYCVerified bool `json:"kyc_verified"`; Rebate RateVersion `json:"rebate"`; Stats Stats `json:"stats"` }
  func (s *Service) Me(ctx context.Context, userID int64, publicBaseURL string) (MeView, error)   // 懒生成码；link = publicBaseURL + "/?ic=" + code
  type InviteeView struct { PhoneMasked string `json:"phone_masked"`; BoundAt time.Time `json:"bound_at"`; Status string `json:"status"`; KYCVerified bool `json:"kyc_verified"`; Contributed int64 `json:"contributed"`; RebateUntil time.Time `json:"rebate_until"` }
  func (s *Service) Invitees(ctx context.Context, userID int64, p paging.Page) ([]InviteeView, int, error)   // bound_at DESC；contributed = Σ reward(status ≠ void)
  type RewardView struct { OrderID string `json:"order_id"`; InviteeMasked string `json:"invitee_masked"`; PaidAt time.Time `json:"paid_at"`; OrderAmount, RefundedAmount, Reward int64 `json:"order_amount","refunded_amount","reward"`; Status string `json:"status"`; SettledAt *time.Time `json:"settled_at"`; ClawedAmount int64 `json:"clawed_amount"` }
  func (s *Service) Rewards(ctx context.Context, userID int64, status string, p paging.Page) ([]RewardView, int, error)   // status ∈ "" | pending | settled | void，其它 → BadRequest；paid_at DESC
  func (s *Service) MonthSettledFor(ctx context.Context, userID int64) (int64, error)   // 本月（Asia/Shanghai 1 日 0 点起）Σ referral_rebate 流水
  ```
  `Stats`：`invitees = count(bindings WHERE inviter_id)`，`verified_invitees = count(… JOIN users kyc_status='verified')`，`pending = Σ reward WHERE status='pending'`，`settled_net = Σ(settled_amount − clawed_amount)`，`month_settled = MonthSettledFor`，`debt`。
- Produces（`package api`）：
  - 路由：`r.With(httpx.NoStore, s.referralRateLimit).Get("/referral/codes/{code}", s.handleReferralCode)`（组外匿名；`ReferralLimit int` 字段，零值 60，`DisableRateLimits` 时 `noRateLimit`；`referralLim *ipLimiter`）；`requireUser` 组内 `GET /referral/me`、`GET /referral/invitees`、`GET /referral/rewards`。
  - `handleLogin`：请求体加 `InviteCode string `json:"invite_code"``；调 `s.Identity.LoginWith(ctx, identity.LoginRequest{Phone, Code, InviteCode, IP: clientIP(r), DeviceID: r.Header.Get("X-Device-Id"), UserAgent: r.UserAgent()})`；响应 `writeJSON(w, 200, result)`（`LoginResult` 平铺 `User` + `invite`）。
  - `handleMe`：`meView{User: u, ReferralMonthSettled: n}`，`n` 来自 `s.Referral.MonthSettledFor`（`Referral == nil` 时 0）。
  - `handleReferralCode` → `{valid, inviter}`；`handleReferralMe` → `Referral.Me(ctx, uid, s.PublicBaseURL)`；`handleReferralInvitees` / `handleReferralRewards` → `httpx.Page(items, total)`（`paging.Parse`，`page_size` 缺省 20）。

- [ ] **Step 1: 写失败的测试**

`internal/referral/queries_test.go`：

```go
package referral_test

import (
	"testing"
	"time"

	"spark-cloud/internal/platform/paging"
)

func TestMeInviteesRewards(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", false)
	b := e.user("13900000001", true)
	c := e.user("13900000002", false)
	e.bind(a, b)
	cb := e.bind(a, c)
	e.pool.Exec(e.ctx, `UPDATE referral_bindings SET status = 'held' WHERE id = $1`, cb)
	e.processPay(e.pay(b, "ord-1", 6900))
	e.processPay(e.pay(b, "ord-2", 6900))
	e.pool.Exec(e.ctx, `UPDATE referral_accounts SET debt = 50 WHERE user_id = $1`, a)
	me, err := e.svc.Me(e.ctx, a, "https://spark.test")
	if err != nil || len(me.Code) != 8 || me.Link != "https://spark.test/?ic="+me.Code || me.Status != "active" || me.KYCVerified || me.Rebate.RateBP != 1000 {
		t.Fatalf("me = %+v %v", me, err)
	}
	if me.Stats.Invitees != 2 || me.Stats.VerifiedInvitees != 1 || me.Stats.Pending != 1380 || me.Stats.SettledNet != 0 || me.Stats.Debt != 50 || me.Stats.MonthSettled != 0 {
		t.Fatalf("stats = %+v", me.Stats)
	}
	// 发放后 settled_net 与本月
	e.pool.Exec(e.ctx, `UPDATE users SET kyc_status = 'verified' WHERE id = $1`, a)
	e.clk.Advance(8 * 24 * time.Hour)
	e.svc.Release(e.ctx)
	me, _ = e.svc.Me(e.ctx, a, "https://spark.test")
	if me.Stats.Pending != 0 || me.Stats.SettledNet != 1380 || me.Stats.MonthSettled != 1330 || me.Stats.Debt != 0 {
		t.Fatalf("after release stats = %+v", me.Stats) // 1380 应得，50 抵欠账，实际入账 1330
	}
	list, total, err := e.svc.Invitees(e.ctx, a, paging.Page{Number: 1, Size: 20})
	if err != nil || total != 2 || len(list) != 2 || list[0].PhoneMasked != "139****0002" || list[0].Status != "held" || list[1].Contributed != 1380 || !list[1].KYCVerified {
		t.Fatalf("invitees = %+v %d %v", list, total, err)
	}
	rewards, total, err := e.svc.Rewards(e.ctx, a, "", paging.Page{Number: 1, Size: 1})
	if err != nil || total != 2 || len(rewards) != 1 || rewards[0].OrderID != "ord-2" || rewards[0].InviteeMasked != "139****0001" || rewards[0].Status != "settled" || rewards[0].Reward != 690 {
		t.Fatalf("rewards = %+v %d %v", rewards, total, err)
	}
	if _, total, _ := e.svc.Rewards(e.ctx, a, "pending", paging.Page{Number: 1, Size: 20}); total != 0 {
		t.Fatal("status filter")
	}
	if _, _, err := e.svc.Rewards(e.ctx, a, "bogus", paging.Page{Number: 1, Size: 20}); err == nil {
		t.Fatal("bogus status must be rejected")
	}
	// 非邀请人：Me 懒生成码，统计全 0
	me2, err := e.svc.Me(e.ctx, b, "https://spark.test")
	if err != nil || len(me2.Code) != 8 || me2.Stats.Invitees != 0 {
		t.Fatalf("me2 = %+v %v", me2, err)
	}
}
```

`internal/api/referral_test.go`（用 `newTestEnvWith` 注入 `Referral`——在 `newTestEnvWith` 里直接构造 `referral.NewService(referral.Deps{Pool, Clock, Wallet: w, Users: ids, Orders: r})` 并 `ids.SetReferral(ref)`、`srv.Referral = ref`，所有 api 测试共享）：

```go
package api_test

import (
	"fmt"
	"testing"
)

func TestReferralCodeLookupAndRateLimit(t *testing.T) {
	e := newTestEnvWith(t, func(s *api.Server) { s.ReferralLimit = 3 })
	owner := e.client()
	owner.login("13812345678")
	me := owner.do("GET", "/referral/me", nil)
	code := me["code"].(string)
	if me["link"] != "http://spark.test/?ic="+code || me["status"] != "active" || me["kyc_verified"] != false {
		t.Fatalf("me = %v", me)
	}
	anon := e.client()
	for i := 0; i < 3; i++ {
		if st, out := anon.do("GET", "/referral/codes/"+strings.ToLower(code), nil); st != 200 || out["valid"] != true || out["inviter"] != "138****5678" {
			t.Fatalf("lookup %d = %d %v", i, st, out)
		}
	}
	if st, out := anon.do("GET", "/referral/codes/"+code, nil); st != 429 || out["code"] != "rate_limited" {
		t.Fatalf("4th = %d %v", st, out)
	}
	e.clk.Advance(time.Minute)
	if st, out := anon.do("GET", "/referral/codes/NOPE!!", nil); st != 200 || out["valid"] != false {
		t.Fatalf("garbage = %d %v", st, out)
	}
	if st, _ := anon.do("GET", "/referral/me", nil); st != 401 {
		t.Fatal("me requires login")
	}
}

// Review Focus 5 + 规格 §3.2：带码注册 / 老用户 / 垃圾码。
func TestLoginWithInviteCode(t *testing.T) {
	e := newTestEnv(t)
	owner := e.client()
	owner.login("13812345678")
	code := owner.do("GET", "/referral/me", nil)["code"].(string)
	newbie := e.client()
	newbie.do("POST", "/auth/sms-code", map[string]string{"phone": "13900000001"})
	st, out := newbie.doH("POST", "/auth/login", map[string]string{"phone": "13900000001", "code": "000000", "invite_code": code}, map[string]string{"X-Device-Id": "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"})
	inv, _ := out["invite"].(map[string]any)
	if st != 200 || inv == nil || inv["bound"] != true || inv["inviter"] != "138****5678" || out["phone"] != "13900000001" {
		t.Fatalf("new user = %d %v", st, out)
	}
	invitees := owner.do("GET", "/referral/invitees", nil)
	if invitees["total"] != float64(1) {
		t.Fatalf("invitees = %v", invitees)
	}
	// 老用户带码
	e.clk.Advance(time.Minute)
	newbie.do("POST", "/auth/sms-code", map[string]string{"phone": "13900000001"})
	_, out = newbie.do("POST", "/auth/login", map[string]string{"phone": "13900000001", "code": "000000", "invite_code": code})
	if inv := out["invite"].(map[string]any); inv["bound"] != false || inv["reason"] != "existing_user" {
		t.Fatalf("existing = %v", out)
	}
	// 不带码：无 invite 字段
	e.clk.Advance(time.Minute)
	newbie.do("POST", "/auth/sms-code", map[string]string{"phone": "13900000001"})
	if _, out = newbie.do("POST", "/auth/login", map[string]string{"phone": "13900000001", "code": "000000"}); out["invite"] != nil {
		t.Fatalf("no code = %v", out)
	}
	// 垃圾码：注册成功、invalid_code
	other := e.client()
	other.do("POST", "/auth/sms-code", map[string]string{"phone": "13900000002"})
	st, out = other.do("POST", "/auth/login", map[string]string{"phone": "13900000002", "code": "000000", "invite_code": "   hello world!!! "})
	if st != 200 || out["invite"].(map[string]any)["reason"] != "invalid_code" || out["id"] == nil {
		t.Fatalf("garbage code = %d %v", st, out)
	}
	// /me 的 referral_month_settled：0 时省略
	if me := owner.do("GET", "/me", nil); me["referral_month_settled"] != nil {
		t.Fatalf("me = %v", me)
	}
}

func TestLoginWithGarbageInviteCodeStillRegisters(t *testing.T) { TestLoginWithInviteCode(t) }
```

（补导入 `strings`、`time`、`spark-cloud/internal/api`。`client.login` 在 `api_test.go` 已有。`newTestEnvWith` 的 `api.Server` 字面量加 `Referral: ref`。）

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/referral/ -run TestMeInvitees 2>&1 | tail -3; go test ./internal/api/ -run 'TestReferral|TestLoginWith' 2>&1 | tail -3`
Expected: 编译错误。

- [ ] **Step 3: `referral/queries.go`**

```go
package referral

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/paging"
	"spark-cloud/internal/platform/tz"
)

type Stats struct {
	Invitees         int   `json:"invitees"`
	VerifiedInvitees int   `json:"verified_invitees"`
	Pending          int64 `json:"pending"`
	SettledNet       int64 `json:"settled_net"`
	MonthSettled     int64 `json:"month_settled"`
	Debt             int64 `json:"debt"`
}

type MeView struct {
	Code        string      `json:"code"`
	Link        string      `json:"link"`
	Status      string      `json:"status"`
	KYCVerified bool        `json:"kyc_verified"`
	Rebate      RateVersion `json:"rebate"`
	Stats       Stats       `json:"stats"`
}

// monthStart 本月 1 日 0 点（Asia/Shanghai）。
func (s *Service) monthStart() time.Time {
	l := s.d.Clock.Now().In(tz.CST)
	return time.Date(l.Year(), l.Month(), 1, 0, 0, 0, 0, tz.CST)
}

// MonthSettledFor 本月已发放到该用户钱包的返利（Σ referral_rebate 流水）。
func (s *Service) MonthSettledFor(ctx context.Context, userID int64) (int64, error) {
	var n int64
	err := s.d.Pool.QueryRow(ctx, `SELECT COALESCE(sum(amount), 0) FROM wallet_ledger WHERE user_id = $1 AND kind = 'referral_rebate' AND created_at >= $2`, userID, s.monthStart()).Scan(&n)
	return n, err
}

func (s *Service) stats(ctx context.Context, userID int64) (Stats, error) {
	var st Stats
	if err := s.d.Pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE u.kyc_status = 'verified')
		FROM referral_bindings b JOIN users u ON u.id = b.invitee_id WHERE b.inviter_id = $1`, userID).Scan(&st.Invitees, &st.VerifiedInvitees); err != nil {
		return st, err
	}
	if err := s.d.Pool.QueryRow(ctx, `SELECT COALESCE(sum(reward) FILTER (WHERE status = 'pending'), 0), COALESCE(sum(settled_amount - clawed_amount), 0)
		FROM referral_rewards WHERE inviter_id = $1`, userID).Scan(&st.Pending, &st.SettledNet); err != nil {
		return st, err
	}
	if err := s.d.Pool.QueryRow(ctx, `SELECT COALESCE((SELECT debt FROM referral_accounts WHERE user_id = $1), 0)`, userID).Scan(&st.Debt); err != nil {
		return st, err
	}
	var err error
	st.MonthSettled, err = s.MonthSettledFor(ctx, userID)
	return st, err
}

// Me 控制台邀请页：懒生成码。
func (s *Service) Me(ctx context.Context, userID int64, publicBaseURL string) (MeView, error) {
	a, err := s.EnsureCode(ctx, userID)
	if err != nil {
		return MeView{}, err
	}
	u, err := s.d.Users.UserState(ctx, s.d.Pool, userID)
	if err != nil {
		return MeView{}, err
	}
	v, err := s.CurrentConfig(ctx)
	if err != nil {
		return MeView{}, err
	}
	st, err := s.stats(ctx, userID)
	if err != nil {
		return MeView{}, err
	}
	return MeView{Code: a.Code, Link: publicBaseURL + "/?ic=" + a.Code, Status: a.Status, KYCVerified: u.KYCStatus == "verified", Rebate: v, Stats: st}, nil
}

type InviteeView struct {
	PhoneMasked string    `json:"phone_masked"`
	BoundAt     time.Time `json:"bound_at"`
	Status      string    `json:"status"`
	KYCVerified bool      `json:"kyc_verified"`
	Contributed int64     `json:"contributed"`
	RebateUntil time.Time `json:"rebate_until"`
}

func (s *Service) Invitees(ctx context.Context, userID int64, p paging.Page) ([]InviteeView, int, error) {
	var total int
	if err := s.d.Pool.QueryRow(ctx, `SELECT count(*) FROM referral_bindings WHERE inviter_id = $1`, userID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.d.Pool.Query(ctx, `SELECT u.phone, b.bound_at, b.status, u.kyc_status = 'verified',
			COALESCE((SELECT sum(reward) FROM referral_rewards r WHERE r.binding_id = b.id AND r.status <> 'void'), 0), b.rebate_until
		FROM referral_bindings b JOIN users u ON u.id = b.invitee_id WHERE b.inviter_id = $1 ORDER BY b.bound_at DESC, b.id DESC LIMIT $2 OFFSET $3`,
		userID, p.Limit(), p.Offset())
	if err != nil {
		return nil, 0, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (InviteeView, error) {
		var v InviteeView
		var phone string
		err := r.Scan(&phone, &v.BoundAt, &v.Status, &v.KYCVerified, &v.Contributed, &v.RebateUntil)
		v.PhoneMasked = MaskPhone(phone)
		return v, err
	})
	return list, total, err
}

type RewardView struct {
	OrderID        string     `json:"order_id"`
	InviteeMasked  string     `json:"invitee_masked"`
	PaidAt         time.Time  `json:"paid_at"`
	OrderAmount    int64      `json:"order_amount"`
	RefundedAmount int64      `json:"refunded_amount"`
	Reward         int64      `json:"reward"`
	Status         string     `json:"status"`
	SettledAt      *time.Time `json:"settled_at"`
	ClawedAmount   int64      `json:"clawed_amount"`
}

func (s *Service) Rewards(ctx context.Context, userID int64, status string, p paging.Page) ([]RewardView, int, error) {
	if status != "" && status != StatusPending && status != StatusSettled && status != StatusVoid {
		return nil, 0, apperr.BadRequest("状态不正确")
	}
	var total int
	if err := s.d.Pool.QueryRow(ctx, `SELECT count(*) FROM referral_rewards WHERE inviter_id = $1 AND ($2 = '' OR status = $2)`, userID, status).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.d.Pool.Query(ctx, `SELECT r.order_id, u.phone, r.paid_at, r.order_amount, r.refunded_amount, r.reward, r.status, r.settled_at, r.clawed_amount
		FROM referral_rewards r JOIN users u ON u.id = r.invitee_id WHERE r.inviter_id = $1 AND ($2 = '' OR r.status = $2)
		ORDER BY r.paid_at DESC, r.order_id DESC LIMIT $3 OFFSET $4`, userID, status, p.Limit(), p.Offset())
	if err != nil {
		return nil, 0, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (RewardView, error) {
		var v RewardView
		var phone string
		err := r.Scan(&v.OrderID, &phone, &v.PaidAt, &v.OrderAmount, &v.RefundedAmount, &v.Reward, &v.Status, &v.SettledAt, &v.ClawedAmount)
		v.InviteeMasked = MaskPhone(phone)
		return v, err
	})
	return list, total, err
}
```

（`tz.CST` 来自计划 6 的 `internal/platform/tz`；`queries.go` 的导入块不需要 `fmt`。）

- [ ] **Step 4: `api/referral.go`、`auth.go`、`server.go`**

`internal/api/referral.go`：

```go
package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/httpx"
	"spark-cloud/internal/platform/paging"
)

const defaultReferralLimit = 60

func (s *Server) referralRateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.referralLim.allow(clientIP(r), s.Clock.Now()) {
			s.writeError(w, r, apperr.ErrRateLimited)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleReferralCode 匿名查码（登录页显示「通过 138****1234 的邀请注册」）；无效不区分原因。
func (s *Server) handleReferralCode(w http.ResponseWriter, r *http.Request) {
	valid, inviter, err := s.Referral.LookupCode(r.Context(), chi.URLParam(r, "code"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"valid": valid, "inviter": inviter})
}

func (s *Server) handleReferralMe(w http.ResponseWriter, r *http.Request) {
	me, err := s.Referral.Me(r.Context(), currentUser(r).ID, s.PublicBaseURL)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, me)
}

func (s *Server) handleReferralInvitees(w http.ResponseWriter, r *http.Request) {
	p, err := paging.Parse(r.URL.Query())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	list, total, err := s.Referral.Invitees(r.Context(), currentUser(r).ID, p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, httpx.Page(list, total))
}

func (s *Server) handleReferralRewards(w http.ResponseWriter, r *http.Request) {
	p, err := paging.Parse(r.URL.Query())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	list, total, err := s.Referral.Rewards(r.Context(), currentUser(r).ID, r.URL.Query().Get("status"), p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, httpx.Page(list, total))
}
```

`internal/api/auth.go` 的 `handleLogin` 与 `handleMe` 改为：

```go
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Phone      string `json:"phone"`
		Code       string `json:"code"`
		InviteCode string `json:"invite_code"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	token, res, err := s.Identity.LoginWith(r.Context(), identity.LoginRequest{Phone: req.Phone, Code: req.Code, InviteCode: req.InviteCode,
		IP: clientIP(r), DeviceID: r.Header.Get("X-Device-Id"), UserAgent: r.UserAgent()})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.setSession(w, token, 30*24*3600)
	writeJSON(w, http.StatusOK, res)
}

type meView struct {
	identity.User
	ReferralMonthSettled int64 `json:"referral_month_settled,omitempty"` // 计划 8：「我的」页入口小字；0 时省略
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	v := meView{User: u}
	if s.Referral != nil {
		n, err := s.Referral.MonthSettledFor(r.Context(), u.ID)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		v.ReferralMonthSettled = n
	}
	writeJSON(w, http.StatusOK, v)
}
```

`internal/api/server.go`：`Server` 加 `Referral *referral.Service`（Task 10 已加）、`ReferralLimit int // 匿名邀请码查询按 IP 每分钟上限；零值 60`、`referralLim *ipLimiter`；`Router` 里初始化 `s.referralLim = &ipLimiter{limit: 取 ReferralLimit 或 defaultReferralLimit}`；路由：组外 `r.With(httpx.NoStore, s.referralRateLimit).Get("/referral/codes/{code}", s.handleReferralCode)`；`requireUser` 组内 `r.Get("/referral/me", s.handleReferralMe)`、`r.Get("/referral/invitees", s.handleReferralInvitees)`、`r.Get("/referral/rewards", s.handleReferralRewards)`。`app.Build`：`ReferralLimit: authorizeLimit`（`DisableRateLimits` 时 `noRateLimit`）。

- [ ] **Step 5: 运行测试确认通过**

Run: `go vet ./... && go test ./internal/referral/ ./internal/api/ ./internal/app/ 2>&1 | tail -6`
Expected: `ok`。

- [ ] **Step 6: 提交**

```bash
git add internal/referral/queries.go internal/referral/queries_test.go internal/api/ internal/app/app.go
git commit -m "feat(api): referral console endpoints (code lookup, me, invitees, rewards), login invite_code, /me month settled

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 13: 后台 API（summary / config / inviters / bindings / rewards / exports / rescan / users 详情块 / dashboard）与权限矩阵

**Files:**
- Create: `internal/adminapi/referral.go`、`internal/adminapi/referral_test.go`
- Modify: `internal/referral/admin.go`（列表 / 报表 / 导出）、`internal/referral/admin_test.go`（追加）、`internal/adminapi/server.go`（`s.referralRoutes(g)`）、`internal/adminapi/users.go`（`handleUser` 加 `referral`）、`internal/adminapi/dashboard.go`（`referral_month_settled`）、`internal/adminapi/permissions_test.go`（`expectedRoutes`）、`internal/adminapi/helpers_test.go`（`newEnvWith` 装配 `Referral`）

**Interfaces:**
- Consumes: Task 11 的裁决方法；Task 10 `Rescan`；Task 3 `AddRateVersion / CurrentConfig / ListRateVersions`；Task 7 `Reward / rewardCols / scanReward`。
- Produces（`package referral`，追加到 `admin.go`）：
  ```go
  type Summary struct { Pending, MonthSettled, SettledNet, Debt int64 `json:"pending","month_settled","settled_net","debt"`; HeldCount int `json:"held_count"` }
  func (s *Service) Summary(ctx) (Summary, error)
  func (s *Service) MonthSettledNet(ctx) (int64, error)   // 仪表盘：本月 Σ referral_rebate + Σ referral_clawback（扣回为负）
  type InviterFilter struct { Q, Status, Flag, Sort string }   // Q：手机号 / 尾号 / ID；Status ∈ "" active frozen；Flag ∈ "" same_ip same_device burst same_identity（任一绑定含该标记）；Sort ∈ "" settled invitees bound_at
  type InviterRow struct { Account; Phone string `json:"phone"`; KYCStatus string `json:"kyc_status"`; UserStatus string `json:"user_status"`; Invitees, HeldCount int `json:"invitees","held_count"`; Pending, SettledNet int64 `json:"pending","settled_net"`; LastBoundAt *time.Time `json:"last_bound_at"` }
  func (s *Service) AdminListInviters(ctx, f InviterFilter, p paging.Page) ([]InviterRow, int, error)
  type Binding struct { ID int64 `json:"id"`; InviterID, InviteeID int64 `json:"inviter_id","invitee_id"`; InviterPhone, InviteePhone string `json:"inviter_phone","invitee_phone"`; Code, Status string; RiskFlags []string `json:"risk_flags"`; HoldReason string `json:"hold_reason"`; RateVersionID int64; BoundAt, RebateUntil time.Time; IP, DeviceID, UserAgent string; ReviewedBy *int64; ReviewedAt *time.Time; ReviewNote string; Pending, Contributed int64 `json:"pending","contributed"` }  // JSON 标签与列同名
  type Event struct { ID int64 `json:"id"`; OrderID string `json:"order_id"`; BindingID int64 `json:"binding_id"`; Kind string `json:"kind"`; SourceLedgerID, WalletLedgerID *int64 `json:"source_ledger_id","wallet_ledger_id"`; Amount int64 `json:"amount"`; Detail string `json:"detail"`; CreatedAt time.Time `json:"created_at"` }
  type InviterDetail struct { Account Account `json:"account"`; Phone string `json:"phone"`; Bindings []Binding `json:"bindings"`; Rewards []Reward `json:"rewards"`; Events []Event `json:"events"` }   // rewards 最近 100、events 最近 50
  func (s *Service) AdminInviter(ctx, userID int64) (InviterDetail, error)   // 无账户行 → ErrNotFound
  type BindingFilter struct { InviterID, InviteeID int64; Status, Flag string; From, To *time.Time }
  func (s *Service) AdminListBindings(ctx, f BindingFilter, p paging.Page) ([]Binding, int, error)   // bound_at DESC；status='held' 时排序 held 优先、按 flags 数量 DESC
  func (s *Service) ExportBindings(ctx, f BindingFilter, fn func(Binding) error) error   // bound_at ASC；> csvx.MaxRows → csvx.ErrTooLarge
  type RewardFilter struct { InviterID, InviteeID int64; Status string; From, To *time.Time }   // From/To 按 paid_at
  type AdminReward struct { Reward; InviterPhone, InviteePhone string `json:"inviter_phone","invitee_phone"` }
  func (s *Service) AdminListRewards(ctx, f RewardFilter, p paging.Page) ([]AdminReward, int, error)
  func (s *Service) ExportRewards(ctx, f RewardFilter, fn func(AdminReward) error) error
  type UserSummary struct { Inviter *struct { UserID int64 `json:"user_id"`; Phone string `json:"phone"`; BindingStatus string `json:"binding_status"` } `json:"inviter,omitempty"`; Account *struct { Code, Status string; Invitees int `json:"invitees"`; SettledNet, Pending, Debt int64 `json:"settled_net","pending","debt"` } `json:"account,omitempty"` }
  func (s *Service) UserSummary(ctx, userID int64) (UserSummary, error)
  ```
- Produces（`package adminapi`，`referral.go`）：路由（全部 `prefix + /referral/…`）：
  | 方法 | 路径 | 角色 | handler |
  |---|---|---|---|
  | GET | `/referral/summary` | finance | `handleReferralSummary` |
  | GET | `/referral/config` | finance | `handleReferralConfig` → `{current, versions}` |
  | POST | `/referral/config` | super | `handleReferralSetConfig`（`withStepUp`；body `{rate_bp, max_per_invitee, max_per_inviter_30d, hold_days, period_days, effective_from?, note, totp}`；`effective_from` RFC3339） |
  | GET | `/referral/inviters` | finance | `handleReferralInviters`（`q, status, flag, sort, page, page_size`） |
  | GET | `/referral/inviters/{id}` | finance | `handleReferralInviter` |
  | POST | `/referral/inviters/{id}/freeze` | finance | `{reason}` |
  | POST | `/referral/inviters/{id}/unfreeze` | finance | |
  | GET | `/referral/bindings` | finance | `inviter_id, invitee_id, status, flag, from, to`（`parseRange`） |
  | GET | `/referral/bindings/export` | finance | CSV |
  | POST | `/referral/bindings/{id}/approve` | finance | `{note}` |
  | POST | `/referral/bindings/{id}/revoke` | finance | `withStepUp`，`{reason, totp}` → `{voided_amount}` |
  | GET | `/referral/rewards` | finance | `inviter_id, invitee_id, status, from, to` |
  | GET | `/referral/rewards/export` | finance | CSV |
  | POST | `/referral/rescan` | super | `withStepUp`，`{days, totp}` |
  `handleUser` 响应加 `"referral": s.Referral.UserSummary(...)`；`handleDashboard` 加 `"referral_month_settled": s.Referral.MonthSettledNet(ctx)`。`permissions_test.go` 的 `expectedRoutes` 加 15 行。

- [ ] **Step 1: 写失败的测试**

`internal/referral/admin_test.go` 追加：

```go
func TestAdminListsSummaryAndExports(t *testing.T) {
	e := newEnv(t)
	a := e.user("13812345678", true)
	x := e.user("13700000000", true)
	b := e.user("13900000001", true)
	c := e.user("13900000002", false)
	e.bind(a, b)
	cb := e.bind(a, c)
	e.pool.Exec(e.ctx, `UPDATE referral_bindings SET status = 'held', hold_reason = 'same_device', risk_flags = '{same_ip,same_device}' WHERE id = $1`, cb)
	e.bind(x, e.user("13900000003", false))
	e.processPay(e.pay(b, "ord-1", 6900))
	e.processPay(e.pay(b, "ord-2", 6900))
	e.clk.Advance(8 * 24 * time.Hour)
	e.svc.Release(e.ctx) // 两笔发放
	e.processRefund(e.refund(b, "ord-2-r1", 6900)) // 扣回 690：A 1380 → 690
	dbtest.Fund(e.t, e.pool, a, -690)               // 花光再退一次 → 欠账 690
	e.processRefund(e.refund(b, "ord-1-r1", 6900))

	sum, err := e.svc.Summary(e.ctx)
	if err != nil || sum.Pending != 0 || sum.SettledNet != 0 || sum.Debt != 690 || sum.HeldCount != 1 || sum.MonthSettled != 1380 {
		t.Fatalf("summary = %+v %v", sum, err)
	}
	net, _ := e.svc.MonthSettledNet(e.ctx)
	if net != 1380-690 {
		t.Fatalf("month net = %d", net)
	}
	list, total, err := e.svc.AdminListInviters(e.ctx, referral.InviterFilter{}, paging.Page{Number: 1, Size: 10})
	if err != nil || total != 2 || list[0].UserID != a || list[0].Invitees != 2 || list[0].HeldCount != 1 || list[0].Debt != 690 {
		t.Fatalf("inviters = %+v %d %v", list, total, err)
	}
	if _, total, _ := e.svc.AdminListInviters(e.ctx, referral.InviterFilter{Q: "5678"}, paging.Page{Number: 1, Size: 10}); total != 1 {
		t.Fatal("q by phone tail")
	}
	if _, total, _ := e.svc.AdminListInviters(e.ctx, referral.InviterFilter{Flag: "same_device"}, paging.Page{Number: 1, Size: 10}); total != 1 {
		t.Fatal("flag filter")
	}
	if _, _, err := e.svc.AdminListInviters(e.ctx, referral.InviterFilter{Sort: "bogus"}, paging.Page{Number: 1, Size: 10}); err == nil {
		t.Fatal("bogus sort")
	}
	det, err := e.svc.AdminInviter(e.ctx, a)
	if err != nil || det.Phone != "13812345678" || len(det.Bindings) != 2 || len(det.Rewards) != 2 || len(det.Events) < 6 {
		t.Fatalf("detail = %+v %v", det, err)
	}
	if _, err := e.svc.AdminInviter(e.ctx, b); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("non-inviter = %v", err)
	}
	held, total, _ := e.svc.AdminListBindings(e.ctx, referral.BindingFilter{Status: "held"}, paging.Page{Number: 1, Size: 10})
	if total != 1 || held[0].InviteeID != c || held[0].InviteePhone != "13900000002" || len(held[0].RiskFlags) != 2 {
		t.Fatalf("held = %+v", held)
	}
	var rows []referral.Binding
	if err := e.svc.ExportBindings(e.ctx, referral.BindingFilter{InviterID: a}, func(b referral.Binding) error { rows = append(rows, b); return nil }); err != nil || len(rows) != 2 {
		t.Fatalf("export bindings = %d %v", len(rows), err)
	}
	rw, total, _ := e.svc.AdminListRewards(e.ctx, referral.RewardFilter{InviteeID: b, Status: "settled"}, paging.Page{Number: 1, Size: 10})
	if total != 2 || rw[0].InviterPhone != "13812345678" || rw[0].ClawedAmount != 690 {
		t.Fatalf("rewards = %+v", rw)
	}
	us, _ := e.svc.UserSummary(e.ctx, b)
	if us.Inviter == nil || us.Inviter.UserID != a || us.Inviter.BindingStatus != "active" || us.Account != nil {
		t.Fatalf("user summary invitee = %+v", us)
	}
	us, _ = e.svc.UserSummary(e.ctx, a)
	if us.Inviter != nil || us.Account == nil || us.Account.Invitees != 2 || us.Account.Debt != 690 {
		t.Fatalf("user summary inviter = %+v", us)
	}
}
```

（补导入 `spark-cloud/internal/platform/paging`、`spark-cloud/internal/platform/dbtest`、`spark-cloud/internal/referral`、`time`。）

`internal/adminapi/referral_test.go`：

```go
package adminapi_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/platform/dbtest"
)

func TestReferralAdminEndpoints(t *testing.T) {
	e := newEnv(t)
	fin := e.loginAs("fin", admin.RoleFinance)
	sup := e.loginAs("root", admin.RoleSuper)
	ops := e.loginAs("ops", admin.RoleOps)
	inviter := e.renter("13812345678")
	invitee := dbtest.CreateUser(t, e.pool, "13900000001", true)
	acc, _ := e.srv.Referral.EnsureCode(e.ctx, inviter)
	var bid int64
	e.pool.QueryRow(e.ctx, `INSERT INTO referral_bindings (inviter_id, invitee_id, code, status, hold_reason, risk_flags, rate_version_id, bound_at, rebate_until, created_at, updated_at)
		VALUES ($1, $2, $3, 'held', 'same_device', '{same_device}', (SELECT min(id) FROM referral_rate_versions), $4, $4 + interval '365 days', $4, $4) RETURNING id`, inviter, invitee, acc.Code, e.clk.Now()).Scan(&bid)

	if code, out := fin.do("GET", "/referral/summary", nil); code != 200 || out["held_count"] != float64(1) {
		t.Fatalf("summary = %d %v", code, out)
	}
	if code, _ := ops.do("GET", "/referral/summary", nil); code != 403 {
		t.Fatal("ops must be forbidden")
	}
	// 配置：super + TOTP；追溯生效被拒
	body := map[string]any{"rate_bp": 1500, "max_per_invitee": 50000, "max_per_inviter_30d": 500000, "hold_days": 7, "period_days": 365, "note": "活动"}
	if code, out := sup.do("POST", "/referral/config", body); code != 403 || out["code"] != "totp_required" {
		t.Fatalf("config without totp = %d %v", code, out)
	}
	body["totp"] = sup.totp()
	if code, out := sup.do("POST", "/referral/config", body); code != 200 || out["rate_bp"] != float64(1500) {
		t.Fatalf("config = %d %v", code, out)
	}
	past := map[string]any{"rate_bp": 1000, "max_per_invitee": 1, "max_per_inviter_30d": 1, "hold_days": 7, "period_days": 365, "effective_from": e.clk.Now().Add(-24 * time.Hour).Format(time.RFC3339)}
	if code, out := sup.do("POST", "/referral/config", past); code != 400 || out["code"] != "referral_config_invalid" {
		t.Fatalf("retroactive = %d %v", code, out)
	}
	if code, _ := fin.do("POST", "/referral/config", body); code != 403 {
		t.Fatal("finance cannot set config")
	}
	if code, out := fin.do("GET", "/referral/config", nil); code != 200 || len(out["versions"].([]any)) != 2 {
		t.Fatalf("get config = %d %v", code, out)
	}
	// 审核：approve（finance，无 TOTP）
	if code, _ := fin.do("POST", fmt.Sprintf("/referral/bindings/%d/approve", bid), map[string]string{"note": "ok"}); code != 200 {
		t.Fatal("approve")
	}
	if code, out := fin.do("POST", fmt.Sprintf("/referral/bindings/%d/approve", bid), map[string]string{"note": "ok"}); code != 409 || out["code"] != "referral_binding_state" {
		t.Fatalf("approve twice = %d %v", code, out)
	}
	// 撤销：finance + TOTP
	if code, out := fin.do("POST", fmt.Sprintf("/referral/bindings/%d/revoke", bid), map[string]string{"reason": "刷单"}); code != 403 || out["code"] != "totp_required" {
		t.Fatalf("revoke without totp = %d %v", code, out)
	}
	if code, out := fin.do("POST", fmt.Sprintf("/referral/bindings/%d/revoke", bid), map[string]string{"reason": "刷单", "totp": fin.totp()}); code != 200 || out["voided_amount"] != float64(0) {
		t.Fatalf("revoke = %d %v", code, out)
	}
	// 冻结 / 解冻
	if code, _ := fin.do("POST", fmt.Sprintf("/referral/inviters/%d/freeze", inviter), map[string]string{"reason": "可疑"}); code != 200 {
		t.Fatal("freeze")
	}
	if code, out := fin.do("GET", fmt.Sprintf("/referral/inviters/%d", inviter), nil); code != 200 || out["account"].(map[string]any)["status"] != "frozen" {
		t.Fatalf("detail = %d %v", code, out)
	}
	if code, _ := fin.do("POST", fmt.Sprintf("/referral/inviters/%d/unfreeze", inviter), nil); code != 200 {
		t.Fatal("unfreeze")
	}
	// 列表与导出
	if code, out := fin.do("GET", "/referral/inviters?q=5678&sort=invitees", nil); code != 200 || out["total"] != float64(1) {
		t.Fatalf("inviters = %d %v", code, out)
	}
	if code, out := fin.do("GET", "/referral/bindings?status=revoked", nil); code != 200 || out["total"] != float64(1) {
		t.Fatalf("bindings = %d %v", code, out)
	}
	resp, _ := fin.send("GET", "/referral/bindings/export?inviter_id="+fmt.Sprint(inviter), nil)
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/csv") || !strings.Contains(string(raw), "绑定ID,邀请人ID,被邀请人ID,邀请码,状态,标记,绑定时间,返利截止,IP,设备") {
		t.Fatalf("bindings csv = %d %q", resp.StatusCode, raw)
	}
	resp, _ = fin.send("GET", "/referral/rewards/export", nil)
	raw, _ = io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), "订单号,邀请人ID,被邀请人ID,消费(元),退款(元),费率(%),应得(元),状态,发放时间,已扣回(元),费率版本") {
		t.Fatalf("rewards csv = %q", raw)
	}
	// 重扫：super + TOTP；days 范围
	if code, out := sup.do("POST", "/referral/rescan", map[string]any{"days": 3, "totp": sup.totp()}); code != 200 {
		t.Fatalf("rescan = %d %v", code, out)
	}
	if code, _ := sup.do("POST", "/referral/rescan", map[string]any{"days": 99, "totp": sup.totp()}); code != 400 {
		t.Fatal("rescan days range")
	}
	// 用户详情 referral 块（all）、仪表盘字段
	if code, out := ops.do("GET", fmt.Sprintf("/users/%d", invitee), nil); code != 200 || out["referral"].(map[string]any)["inviter"].(map[string]any)["user_id"] != float64(inviter) {
		t.Fatalf("user detail = %d %v", code, out)
	}
	if code, out := ops.do("GET", "/dashboard", nil); code != 200 || out["referral_month_settled"] != float64(0) {
		t.Fatalf("dashboard = %d %v", code, out)
	}
	_ = http.StatusOK
}
```

（补导入 `fmt`、`io`、`spark-cloud/internal/admin`。`helpers_test.go` 的 `newEnvWith`：构造 `ref := referral.NewService(referral.Deps{Pool: pool, Clock: clk, Log: log, Wallet: w, Users: ids, Orders: r})`、`ids.SetReferral(ref)`、`Server{…, Referral: ref}`。）

`permissions_test.go` 的 `expectedRoutes` 追加：

```go
	{"GET", "/referral/summary", finance}, {"GET", "/referral/config", finance}, {"POST", "/referral/config", super},
	{"GET", "/referral/inviters", finance}, {"GET", "/referral/inviters/{id}", finance},
	{"POST", "/referral/inviters/{id}/freeze", finance}, {"POST", "/referral/inviters/{id}/unfreeze", finance},
	{"GET", "/referral/bindings", finance}, {"GET", "/referral/bindings/export", finance},
	{"POST", "/referral/bindings/{id}/approve", finance}, {"POST", "/referral/bindings/{id}/revoke", finance},
	{"GET", "/referral/rewards", finance}, {"GET", "/referral/rewards/export", finance},
	{"POST", "/referral/rescan", super},
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/referral/ -run TestAdminLists 2>&1 | tail -3; go test ./internal/adminapi/ -run 'TestReferralAdmin|TestRouteTable' 2>&1 | tail -5`
Expected: 编译错误 / 路由表缺行。

- [ ] **Step 3: `referral/admin.go` 追加查询**

```go
type Summary struct {
	Pending      int64 `json:"pending"`
	MonthSettled int64 `json:"month_settled"`
	SettledNet   int64 `json:"settled_net"`
	Debt         int64 `json:"debt"`
	HeldCount    int   `json:"held_count"`
}

func (s *Service) Summary(ctx context.Context) (Summary, error) {
	var sum Summary
	err := s.d.Pool.QueryRow(ctx, `SELECT
		COALESCE((SELECT sum(reward) FROM referral_rewards WHERE status = 'pending'), 0),
		COALESCE((SELECT sum(amount) FROM wallet_ledger WHERE kind = 'referral_rebate' AND created_at >= $1), 0),
		COALESCE((SELECT sum(settled_amount - clawed_amount) FROM referral_rewards), 0),
		COALESCE((SELECT sum(debt) FROM referral_accounts), 0),
		(SELECT count(*) FROM referral_bindings WHERE status = 'held')`, s.monthStart()).
		Scan(&sum.Pending, &sum.MonthSettled, &sum.SettledNet, &sum.Debt, &sum.HeldCount)
	return sum, err
}

// MonthSettledNet 仪表盘卡片：本月发放净额（返利 − 扣回）。
func (s *Service) MonthSettledNet(ctx context.Context) (int64, error) {
	var n int64
	err := s.d.Pool.QueryRow(ctx, `SELECT COALESCE(sum(amount), 0) FROM wallet_ledger WHERE kind IN ('referral_rebate', 'referral_clawback') AND created_at >= $1`, s.monthStart()).Scan(&n)
	return n, err
}

type InviterFilter struct{ Q, Status, Flag, Sort string }

type InviterRow struct {
	Account
	Phone       string     `json:"phone"`
	KYCStatus   string     `json:"kyc_status"`
	UserStatus  string     `json:"user_status"`
	Invitees    int        `json:"invitees"`
	HeldCount   int        `json:"held_count"`
	Pending     int64      `json:"pending"`
	SettledNet  int64      `json:"settled_net"`
	LastBoundAt *time.Time `json:"last_bound_at"`
}

var (
	flagValues   = []string{FlagSameIP, FlagSameDevice, FlagBurst, FlagSameIdentity}
	inviterSorts = map[string]string{"": "settled_net DESC, user_id", "settled": "settled_net DESC, user_id", "invitees": "invitees DESC, user_id", "bound_at": "last_bound_at DESC NULLS LAST, user_id"}
)

// inviterRowsSQL 每个邀请人一行（子查询 x），筛选与排序都作用在它的列上。
const inviterRowsSQL = `
	SELECT a.user_id, a.code, a.status, a.frozen_reason, a.debt, a.created_at, u.phone, u.kyc_status, u.status AS user_status,
		COALESCE(b.n, 0) AS invitees, COALESCE(b.held, 0) AS held_count, COALESCE(r.pending, 0) AS pending, COALESCE(r.net, 0) AS settled_net, b.last_bound_at
	FROM referral_accounts a JOIN users u ON u.id = a.user_id
	LEFT JOIN (SELECT inviter_id, count(*) AS n, count(*) FILTER (WHERE status = 'held') AS held, max(bound_at) AS last_bound_at FROM referral_bindings GROUP BY inviter_id) b ON b.inviter_id = a.user_id
	LEFT JOIN (SELECT inviter_id, sum(reward) FILTER (WHERE status = 'pending') AS pending, sum(settled_amount - clawed_amount) AS net FROM referral_rewards GROUP BY inviter_id) r ON r.inviter_id = a.user_id`

func (s *Service) AdminListInviters(ctx context.Context, f InviterFilter, p paging.Page) ([]InviterRow, int, error) {
	order, ok := inviterSorts[f.Sort]
	if !ok {
		return nil, 0, apperr.BadRequest("排序方式不正确")
	}
	if f.Status != "" && f.Status != AccountActive && f.Status != AccountFrozen {
		return nil, 0, apperr.BadRequest("状态不正确")
	}
	if f.Flag != "" && !slices.Contains(flagValues, f.Flag) {
		return nil, 0, apperr.BadRequest("标记不正确")
	}
	var w db.Where
	if q := strings.TrimSpace(f.Q); q != "" {
		if id, err := strconv.ParseInt(q, 10, 64); err == nil && len(q) < 11 {
			w.Add("(x.user_id = ? OR x.phone LIKE ?)", id, "%"+q)
		} else {
			w.Add("x.phone LIKE ?", "%"+q)
		}
	}
	if f.Status != "" {
		w.Add("x.status = ?", f.Status)
	}
	if f.Flag != "" {
		w.Add("EXISTS (SELECT 1 FROM referral_bindings fb WHERE fb.inviter_id = x.user_id AND ? = ANY(fb.risk_flags))", f.Flag)
	}
	from := ` FROM (` + inviterRowsSQL + `) x WHERE ` + w.SQL()
	var total int
	if err := s.d.Pool.QueryRow(ctx, `SELECT count(*)`+from, w.Args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	n := len(w.Args)
	rows, err := s.d.Pool.Query(ctx, `SELECT x.*`+from+` ORDER BY `+order+fmt.Sprintf(` LIMIT $%d OFFSET $%d`, n+1, n+2), append(w.Args, p.Limit(), p.Offset())...)
	if err != nil {
		return nil, 0, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (InviterRow, error) {
		var v InviterRow
		err := r.Scan(&v.UserID, &v.Code, &v.Status, &v.FrozenReason, &v.Debt, &v.CreatedAt, &v.Phone, &v.KYCStatus, &v.UserStatus,
			&v.Invitees, &v.HeldCount, &v.Pending, &v.SettledNet, &v.LastBoundAt)
		return v, err
	})
	return list, total, err
}
```

（`db.Where.SQL()` 在无条件时返回 `TRUE`——以 `internal/platform/db/db.go` 的实现为准；`settled_net` / `invitees` / `last_bound_at` 是子查询 `x` 的列，`ORDER BY` 引用它们时去掉 `a.` 前缀改为 `x.user_id`。）

```go
type Binding struct {
	ID            int64      `json:"id"`
	InviterID     int64      `json:"inviter_id"`
	InviteeID     int64      `json:"invitee_id"`
	InviterPhone  string     `json:"inviter_phone"`
	InviteePhone  string     `json:"invitee_phone"`
	Code          string     `json:"code"`
	Status        string     `json:"status"`
	RiskFlags     []string   `json:"risk_flags"`
	HoldReason    string     `json:"hold_reason"`
	RateVersionID int64      `json:"rate_version_id"`
	BoundAt       time.Time  `json:"bound_at"`
	RebateUntil   time.Time  `json:"rebate_until"`
	IP            string     `json:"ip"`
	DeviceID      string     `json:"device_id"`
	UserAgent     string     `json:"user_agent"`
	ReviewedBy    *int64     `json:"reviewed_by"`
	ReviewedAt    *time.Time `json:"reviewed_at"`
	ReviewNote    string     `json:"review_note"`
	Pending       int64      `json:"pending"`
	Contributed   int64      `json:"contributed"`
}

const bindingSelect = `SELECT b.id, b.inviter_id, b.invitee_id, iu.phone, eu.phone, b.code, b.status, b.risk_flags, b.hold_reason, b.rate_version_id, b.bound_at, b.rebate_until,
		b.ip, b.device_id, b.user_agent, b.reviewed_by, b.reviewed_at, b.review_note,
		COALESCE((SELECT sum(reward) FROM referral_rewards r WHERE r.binding_id = b.id AND r.status = 'pending'), 0),
		COALESCE((SELECT sum(reward) FROM referral_rewards r WHERE r.binding_id = b.id AND r.status <> 'void'), 0)
	FROM referral_bindings b JOIN users iu ON iu.id = b.inviter_id JOIN users eu ON eu.id = b.invitee_id`

func scanBinding(r pgx.Row) (Binding, error) {
	var b Binding
	err := r.Scan(&b.ID, &b.InviterID, &b.InviteeID, &b.InviterPhone, &b.InviteePhone, &b.Code, &b.Status, &b.RiskFlags, &b.HoldReason, &b.RateVersionID, &b.BoundAt, &b.RebateUntil,
		&b.IP, &b.DeviceID, &b.UserAgent, &b.ReviewedBy, &b.ReviewedAt, &b.ReviewNote, &b.Pending, &b.Contributed)
	if b.RiskFlags == nil {
		b.RiskFlags = []string{}
	}
	return b, err
}

type BindingFilter struct {
	InviterID, InviteeID int64
	Status, Flag         string
	From, To             *time.Time
}

func (f BindingFilter) where() (db.Where, error) {
	var w db.Where
	if f.Status != "" && f.Status != BindingActive && f.Status != BindingHeld && f.Status != BindingRevoked {
		return w, apperr.BadRequest("状态不正确")
	}
	if f.Flag != "" && !slices.Contains(flagValues, f.Flag) {
		return w, apperr.BadRequest("标记不正确")
	}
	if f.InviterID > 0 {
		w.Add("b.inviter_id = ?", f.InviterID)
	}
	if f.InviteeID > 0 {
		w.Add("b.invitee_id = ?", f.InviteeID)
	}
	if f.Status != "" {
		w.Add("b.status = ?", f.Status)
	}
	if f.Flag != "" {
		w.Add("? = ANY(b.risk_flags)", f.Flag)
	}
	if f.From != nil {
		w.Add("b.bound_at >= ?", *f.From)
	}
	if f.To != nil {
		w.Add("b.bound_at < ?", *f.To)
	}
	return w, nil
}

func (s *Service) AdminListBindings(ctx context.Context, f BindingFilter, p paging.Page) ([]Binding, int, error) {
	w, err := f.where()
	if err != nil {
		return nil, 0, err
	}
	var total int
	if err := s.d.Pool.QueryRow(ctx, `SELECT count(*) FROM referral_bindings b WHERE `+w.SQL(), w.Args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	n := len(w.Args)
	order := `b.bound_at DESC, b.id DESC`
	if f.Status == BindingHeld {
		order = `array_length(b.risk_flags, 1) DESC NULLS LAST, b.bound_at ASC, b.id`
	}
	rows, err := s.d.Pool.Query(ctx, bindingSelect+` WHERE `+w.SQL()+` ORDER BY `+order+fmt.Sprintf(` LIMIT $%d OFFSET $%d`, n+1, n+2), append(w.Args, p.Limit(), p.Offset())...)
	if err != nil {
		return nil, 0, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Binding, error) { return scanBinding(r) })
	return list, total, err
}

func (s *Service) ExportBindings(ctx context.Context, f BindingFilter, fn func(Binding) error) error {
	w, err := f.where()
	if err != nil {
		return err
	}
	var total int
	if err := s.d.Pool.QueryRow(ctx, `SELECT count(*) FROM referral_bindings b WHERE `+w.SQL(), w.Args...).Scan(&total); err != nil {
		return err
	}
	if total > csvx.MaxRows {
		return csvx.ErrTooLarge
	}
	rows, err := s.d.Pool.Query(ctx, bindingSelect+` WHERE `+w.SQL()+` ORDER BY b.bound_at, b.id`, w.Args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		b, err := scanBinding(rows)
		if err != nil {
			return err
		}
		if err := fn(b); err != nil {
			return err
		}
	}
	return rows.Err()
}

type Event struct {
	ID             int64     `json:"id"`
	OrderID        string    `json:"order_id"`
	BindingID      int64     `json:"binding_id"`
	Kind           string    `json:"kind"`
	SourceLedgerID *int64    `json:"source_ledger_id"`
	WalletLedgerID *int64    `json:"wallet_ledger_id"`
	Amount         int64     `json:"amount"`
	Detail         string    `json:"detail"`
	CreatedAt      time.Time `json:"created_at"`
}

type InviterDetail struct {
	Account  Account       `json:"account"`
	Phone    string        `json:"phone"`
	Bindings []Binding     `json:"bindings"`
	Rewards  []AdminReward `json:"rewards"`
	Events   []Event       `json:"events"`
}

func (s *Service) AdminInviter(ctx context.Context, userID int64) (InviterDetail, error) {
	var d InviterDetail
	a, err := scanAccount(s.d.Pool.QueryRow(ctx, `SELECT `+accountCols+` FROM referral_accounts WHERE user_id = $1`, userID))
	if db.IsNoRows(err) {
		return d, apperr.ErrNotFound
	}
	if err != nil {
		return d, err
	}
	d.Account = a
	u, err := s.d.Users.UserState(ctx, s.d.Pool, userID)
	if err != nil {
		return d, err
	}
	d.Phone = u.Phone
	if d.Bindings, _, err = s.AdminListBindings(ctx, BindingFilter{InviterID: userID}, paging.Page{Number: 1, Size: 100}); err != nil {
		return d, err
	}
	if d.Rewards, _, err = s.AdminListRewards(ctx, RewardFilter{InviterID: userID}, paging.Page{Number: 1, Size: 100}); err != nil {
		return d, err
	}
	rows, err := s.d.Pool.Query(ctx, `SELECT e.id, e.order_id, e.binding_id, e.kind, e.source_ledger_id, e.wallet_ledger_id, e.amount, e.detail, e.created_at
		FROM referral_events e JOIN referral_bindings b ON b.id = e.binding_id WHERE b.inviter_id = $1 ORDER BY e.id DESC LIMIT 50`, userID)
	if err != nil {
		return d, err
	}
	d.Events, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Event, error) {
		var e Event
		err := r.Scan(&e.ID, &e.OrderID, &e.BindingID, &e.Kind, &e.SourceLedgerID, &e.WalletLedgerID, &e.Amount, &e.Detail, &e.CreatedAt)
		return e, err
	})
	if d.Events == nil {
		d.Events = []Event{}
	}
	return d, err
}

type RewardFilter struct {
	InviterID, InviteeID int64
	Status               string
	From, To             *time.Time // paid_at
}

type AdminReward struct {
	Reward
	InviterPhone string `json:"inviter_phone"`
	InviteePhone string `json:"invitee_phone"`
}

func (f RewardFilter) where() (db.Where, error) {
	var w db.Where
	if f.Status != "" && f.Status != StatusPending && f.Status != StatusSettled && f.Status != StatusVoid {
		return w, apperr.BadRequest("状态不正确")
	}
	if f.InviterID > 0 {
		w.Add("r.inviter_id = ?", f.InviterID)
	}
	if f.InviteeID > 0 {
		w.Add("r.invitee_id = ?", f.InviteeID)
	}
	if f.Status != "" {
		w.Add("r.status = ?", f.Status)
	}
	if f.From != nil {
		w.Add("r.paid_at >= ?", *f.From)
	}
	if f.To != nil {
		w.Add("r.paid_at < ?", *f.To)
	}
	return w, nil
}

const adminRewardSelect = `SELECT r.order_id, r.binding_id, r.inviter_id, r.invitee_id, r.rate_version_id, r.rate_bp, r.order_amount, r.refunded_amount, r.reward_cap, r.reward, r.status, r.settle_after,
	r.settled_amount, r.clawed_amount, r.settled_at, r.paid_at, r.created_at, r.updated_at, iu.phone, eu.phone
	FROM referral_rewards r JOIN users iu ON iu.id = r.inviter_id JOIN users eu ON eu.id = r.invitee_id`

func scanAdminReward(row pgx.Row) (AdminReward, error) {
	var v AdminReward
	r := &v.Reward
	err := row.Scan(&r.OrderID, &r.BindingID, &r.InviterID, &r.InviteeID, &r.RateVersionID, &r.RateBP, &r.OrderAmount, &r.RefundedAmount, &r.RewardCap, &r.RewardAmt, &r.Status, &r.SettleAfter,
		&r.SettledAmount, &r.ClawedAmount, &r.SettledAt, &r.PaidAt, &r.CreatedAt, &r.UpdatedAt, &v.InviterPhone, &v.InviteePhone)
	return v, err
}

func (s *Service) AdminListRewards(ctx context.Context, f RewardFilter, p paging.Page) ([]AdminReward, int, error) {
	w, err := f.where()
	if err != nil {
		return nil, 0, err
	}
	var total int
	if err := s.d.Pool.QueryRow(ctx, `SELECT count(*) FROM referral_rewards r WHERE `+w.SQL(), w.Args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	n := len(w.Args)
	rows, err := s.d.Pool.Query(ctx, adminRewardSelect+` WHERE `+w.SQL()+fmt.Sprintf(` ORDER BY r.paid_at DESC, r.order_id DESC LIMIT $%d OFFSET $%d`, n+1, n+2), append(w.Args, p.Limit(), p.Offset())...)
	if err != nil {
		return nil, 0, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (AdminReward, error) { return scanAdminReward(r) })
	return list, total, err
}

func (s *Service) ExportRewards(ctx context.Context, f RewardFilter, fn func(AdminReward) error) error {
	w, err := f.where()
	if err != nil {
		return err
	}
	var total int
	if err := s.d.Pool.QueryRow(ctx, `SELECT count(*) FROM referral_rewards r WHERE `+w.SQL(), w.Args...).Scan(&total); err != nil {
		return err
	}
	if total > csvx.MaxRows {
		return csvx.ErrTooLarge
	}
	rows, err := s.d.Pool.Query(ctx, adminRewardSelect+` WHERE `+w.SQL()+` ORDER BY r.paid_at, r.order_id`, w.Args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		v, err := scanAdminReward(rows)
		if err != nil {
			return err
		}
		if err := fn(v); err != nil {
			return err
		}
	}
	return rows.Err()
}

type UserSummaryInviter struct {
	UserID        int64  `json:"user_id"`
	Phone         string `json:"phone"`
	BindingStatus string `json:"binding_status"`
}

type UserSummaryAccount struct {
	Code       string `json:"code"`
	Status     string `json:"status"`
	Invitees   int    `json:"invitees"`
	SettledNet int64  `json:"settled_net"`
	Pending    int64  `json:"pending"`
	Debt       int64  `json:"debt"`
}

type UserSummary struct {
	Inviter *UserSummaryInviter `json:"inviter,omitempty"`
	Account *UserSummaryAccount `json:"account,omitempty"`
}

// UserSummary 后台用户详情的「邀请」区块：作为被邀请人的邀请人，作为邀请人的统计。
func (s *Service) UserSummary(ctx context.Context, userID int64) (UserSummary, error) {
	var us UserSummary
	var inv UserSummaryInviter
	err := s.d.Pool.QueryRow(ctx, `SELECT b.inviter_id, u.phone, b.status FROM referral_bindings b JOIN users u ON u.id = b.inviter_id WHERE b.invitee_id = $1`, userID).
		Scan(&inv.UserID, &inv.Phone, &inv.BindingStatus)
	if err != nil && !db.IsNoRows(err) {
		return us, err
	}
	if err == nil {
		us.Inviter = &inv
	}
	var acc UserSummaryAccount
	err = s.d.Pool.QueryRow(ctx, `SELECT a.code, a.status, a.debt,
			(SELECT count(*) FROM referral_bindings WHERE inviter_id = a.user_id),
			COALESCE((SELECT sum(settled_amount - clawed_amount) FROM referral_rewards WHERE inviter_id = a.user_id), 0),
			COALESCE((SELECT sum(reward) FROM referral_rewards WHERE inviter_id = a.user_id AND status = 'pending'), 0)
		FROM referral_accounts a WHERE a.user_id = $1`, userID).Scan(&acc.Code, &acc.Status, &acc.Debt, &acc.Invitees, &acc.SettledNet, &acc.Pending)
	if err != nil && !db.IsNoRows(err) {
		return us, err
	}
	if err == nil {
		us.Account = &acc
	}
	return us, nil
}
```

（补导入 `fmt`、`slices`、`time`、`spark-cloud/internal/platform/csvx`、`spark-cloud/internal/platform/paging`。）

- [ ] **Step 4: `adminapi/referral.go`**

```go
package adminapi

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/csvx"
	"spark-cloud/internal/platform/httpx"
	"spark-cloud/internal/referral"
)

func (s *Server) referralRoutes(g chi.Router) {
	s.handle(g, http.MethodGet, "/referral/summary", financeRoles, s.handleReferralSummary)
	s.handle(g, http.MethodGet, "/referral/config", financeRoles, s.handleReferralConfig)
	s.handle(g, http.MethodPost, "/referral/config", superOnly, s.handleReferralSetConfig)
	s.handle(g, http.MethodGet, "/referral/inviters", financeRoles, s.handleReferralInviters)
	s.handle(g, http.MethodGet, "/referral/inviters/{id}", financeRoles, s.handleReferralInviter)
	s.handle(g, http.MethodPost, "/referral/inviters/{id}/freeze", financeRoles, s.handleReferralFreeze)
	s.handle(g, http.MethodPost, "/referral/inviters/{id}/unfreeze", financeRoles, s.handleReferralUnfreeze)
	s.handle(g, http.MethodGet, "/referral/bindings", financeRoles, s.handleReferralBindings)
	s.handle(g, http.MethodGet, "/referral/bindings/export", financeRoles, s.handleReferralExportBindings)
	s.handle(g, http.MethodPost, "/referral/bindings/{id}/approve", financeRoles, s.handleReferralApprove)
	s.handle(g, http.MethodPost, "/referral/bindings/{id}/revoke", financeRoles, s.handleReferralRevoke)
	s.handle(g, http.MethodGet, "/referral/rewards", financeRoles, s.handleReferralRewards)
	s.handle(g, http.MethodGet, "/referral/rewards/export", financeRoles, s.handleReferralExportRewards)
	s.handle(g, http.MethodPost, "/referral/rescan", superOnly, s.handleReferralRescan)
}

func (s *Server) handleReferralSummary(w http.ResponseWriter, r *http.Request) {
	sum, err := s.Referral.Summary(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, sum)
}

func (s *Server) handleReferralConfig(w http.ResponseWriter, r *http.Request) {
	cur, err := s.Referral.CurrentConfig(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	versions, err := s.Referral.ListRateVersions(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"current": cur, "versions": versions})
}

func (s *Server) handleReferralSetConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RateBP           int    `json:"rate_bp"`
		MaxPerInvitee    int64  `json:"max_per_invitee"`
		MaxPerInviter30d int64  `json:"max_per_inviter_30d"`
		HoldDays         int    `json:"hold_days"`
		PeriodDays       int    `json:"period_days"`
		EffectiveFrom    string `json:"effective_from"`
		Note             string `json:"note"`
		TOTP             string `json:"totp"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	in := referral.RateInput{RateBP: req.RateBP, MaxPerInvitee: req.MaxPerInvitee, MaxPerInviter30d: req.MaxPerInviter30d, HoldDays: req.HoldDays, PeriodDays: req.PeriodDays, Note: req.Note}
	if req.EffectiveFrom != "" {
		t, err := time.Parse(time.RFC3339, req.EffectiveFrom)
		if err != nil {
			s.writeError(w, r, apperr.BadRequest("生效时间格式不正确"))
			return
		}
		in.EffectiveFrom = &t
	}
	var v referral.RateVersion
	if err := s.withStepUp(r, req.TOTP, func() (err error) {
		v, err = s.Referral.AddRateVersion(r.Context(), s.actor(r), in)
		return err
	}); err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, v)
}

func (s *Server) handleReferralInviters(w http.ResponseWriter, r *http.Request) {
	p, err := pageOf(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	q := r.URL.Query()
	list, total, err := s.Referral.AdminListInviters(r.Context(), referral.InviterFilter{Q: q.Get("q"), Status: q.Get("status"), Flag: q.Get("flag"), Sort: q.Get("sort")}, p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page(list, total))
}

func (s *Server) handleReferralInviter(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	d, err := s.Referral.AdminInviter(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, d)
}

func (s *Server) handleReferralFreeze(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.Referral.Freeze(r.Context(), s.actor(r), id, req.Reason); err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.OK(w)
}

func (s *Server) handleReferralUnfreeze(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.Referral.Unfreeze(r.Context(), s.actor(r), id); err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.OK(w)
}

func (s *Server) bindingFilter(r *http.Request) (referral.BindingFilter, error) {
	inviter, err := queryInt(r, "inviter_id")
	if err != nil {
		return referral.BindingFilter{}, err
	}
	invitee, err := queryInt(r, "invitee_id")
	if err != nil {
		return referral.BindingFilter{}, err
	}
	from, to, err := parseRange(r)
	if err != nil {
		return referral.BindingFilter{}, err
	}
	q := r.URL.Query()
	return referral.BindingFilter{InviterID: inviter, InviteeID: invitee, Status: q.Get("status"), Flag: q.Get("flag"), From: from, To: to}, nil
}

func (s *Server) handleReferralBindings(w http.ResponseWriter, r *http.Request) {
	p, err := pageOf(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	f, err := s.bindingFilter(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	list, total, err := s.Referral.AdminListBindings(r.Context(), f, p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page(list, total))
}

func (s *Server) handleReferralExportBindings(w http.ResponseWriter, r *http.Request) {
	f, err := s.bindingFilter(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.csvExport(w, r, "referral-bindings", []string{"绑定ID", "邀请人ID", "被邀请人ID", "邀请码", "状态", "标记", "绑定时间", "返利截止", "IP", "设备"},
		func(write func([]string) error) error {
			return s.Referral.ExportBindings(r.Context(), f, func(b referral.Binding) error {
				return write([]string{strconv.FormatInt(b.ID, 10), strconv.FormatInt(b.InviterID, 10), strconv.FormatInt(b.InviteeID, 10), b.Code, b.Status,
					strings.Join(b.RiskFlags, " "), csvx.Time(b.BoundAt), csvx.Time(b.RebateUntil), b.IP, b.DeviceID})
			})
		})
}

func (s *Server) handleReferralApprove(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var req struct {
		Note string `json:"note"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.Referral.Approve(r.Context(), s.actor(r), id, req.Note); err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.OK(w)
}

func (s *Server) handleReferralRevoke(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var req struct {
		Reason string `json:"reason"`
		TOTP   string `json:"totp"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	var voided int64
	if err := s.withStepUp(r, req.TOTP, func() (err error) {
		voided, err = s.Referral.Revoke(r.Context(), s.actor(r), id, req.Reason)
		return err
	}); err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]int64{"voided_amount": voided})
}

func (s *Server) rewardFilter(r *http.Request) (referral.RewardFilter, error) {
	inviter, err := queryInt(r, "inviter_id")
	if err != nil {
		return referral.RewardFilter{}, err
	}
	invitee, err := queryInt(r, "invitee_id")
	if err != nil {
		return referral.RewardFilter{}, err
	}
	from, to, err := parseRange(r)
	if err != nil {
		return referral.RewardFilter{}, err
	}
	return referral.RewardFilter{InviterID: inviter, InviteeID: invitee, Status: r.URL.Query().Get("status"), From: from, To: to}, nil
}

func (s *Server) handleReferralRewards(w http.ResponseWriter, r *http.Request) {
	p, err := pageOf(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	f, err := s.rewardFilter(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	list, total, err := s.Referral.AdminListRewards(r.Context(), f, p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page(list, total))
}

func (s *Server) handleReferralExportRewards(w http.ResponseWriter, r *http.Request) {
	f, err := s.rewardFilter(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	s.csvExport(w, r, "referral-rewards", []string{"订单号", "邀请人ID", "被邀请人ID", "消费(元)", "退款(元)", "费率(%)", "应得(元)", "状态", "发放时间", "已扣回(元)", "费率版本"},
		func(write func([]string) error) error {
			return s.Referral.ExportRewards(r.Context(), f, func(v referral.AdminReward) error {
				return write([]string{v.OrderID, strconv.FormatInt(v.InviterID, 10), strconv.FormatInt(v.InviteeID, 10), csvx.Yuan(v.OrderAmount), csvx.Yuan(v.RefundedAmount),
					strconv.FormatFloat(float64(v.RateBP)/100, 'f', 2, 64), csvx.Yuan(v.RewardAmt), v.Status, csvx.TimePtr(v.SettledAt), csvx.Yuan(v.ClawedAmount), strconv.FormatInt(v.RateVersionID, 10)})
			})
		})
}

func (s *Server) handleReferralRescan(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Days int    `json:"days"`
		TOTP string `json:"totp"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.withStepUp(r, req.TOTP, func() error { return s.Referral.Rescan(r.Context(), s.actor(r), req.Days) }); err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.OK(w)
}
```

（补导入 `strings`；`queryInt`、`parseRange`、`csvExport` 在 `finance.go` 已有。）`server.go` 的 `Router` 在 `s.financeRoutes(g)` 之后加 `s.referralRoutes(g)`。`users.go` 的 `handleUser`：

```go
	ref, err := s.Referral.UserSummary(ctx, id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	…"ssh_keys": httpx.List(keys)["items"], "referral": ref,
```

`dashboard.go`：`refMonth, err := s.Referral.MonthSettledNet(ctx)` → 响应加 `"referral_month_settled": refMonth`。

- [ ] **Step 5: 运行测试确认通过**

Run: `go vet ./... && go test ./internal/referral/ ./internal/adminapi/ 2>&1 | tail -6`
Expected: `ok`（含 `TestRouteTableMatchesPermissionMatrix`）。

- [ ] **Step 6: 提交**

```bash
git add internal/referral/admin.go internal/referral/admin_test.go internal/adminapi/
git commit -m "feat(adminapi): referral summary / config / inviters / bindings / rewards / exports / rescan, user detail block, dashboard card

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 14: Go 集成测试（`internal/app`）：全链路、预订取消、开通失败、自刷、上限、返利期、并发资金一致性

**Files:**
- Create: `internal/app/referral_e2e_test.go`

**Interfaces:**
- Consumes: 全部后端任务；`app.Build`（装配了 `Referral`）；`bookingEnv` 风格的 HTTP client（`internal/app/e2e_test.go` 的 `client`、`booking_e2e_test.go` 的 `upstreamKeyFile`）；`rental.Place / Renew / PlaceBatch / Cancel / RefundRelease`；`referral.Freeze / Unfreeze / Approve / CheckInvariants`。
- Produces: 测试 `TestReferralFullChainHTTP`、`TestReferralBookingCancelAndCreateFailure`、`TestReferralSelfDealingHeldAndRevoked`、`TestReferralCapsAndPeriod`、`TestReferralConcurrentFundsConsistent`（Review Focus 2）。

- [ ] **Step 1: 测试环境与全链路**

`internal/app/referral_e2e_test.go`：

```go
package app_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/app"
	"spark-cloud/internal/audit"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/config"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/rental"
)

type refEnv struct {
	t        *testing.T
	ctx      context.Context
	a        *app.App
	pool     *pgxpool.Pool
	clk      *clock.Fake
	ts       *httptest.Server
	machines []int64
	product  int64
	image    int64
}

func newRefEnv(t *testing.T, machines int) *refEnv {
	t.Helper()
	pool := dbtest.New(t)
	clk := clock.NewFake(dbtest.T0)
	cfg := config.Config{UpstreamSSHKeyFile: upstreamKeyFile(t), Env: "dev", UploadDir: t.TempDir(), Domain: "spark.test", PublicBaseURL: "http://spark.test",
		SecretKey: dbtest.Key, SMSDriver: "mock", KYCDriver: "mock"}
	a, err := app.Build(cfg, pool, slog.New(slog.NewTextHandler(io.Discard, nil)), app.Options{Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(a.Handler)
	t.Cleanup(ts.Close)
	e := &refEnv{t: t, ctx: context.Background(), a: a, pool: pool, clk: clk, ts: ts, product: dbtest.ProductID(t, pool), image: dbtest.ImageID(t, pool)}
	for i := 0; i < machines; i++ {
		e.machines = append(e.machines, dbtest.CreateMachine(t, pool, fmt.Sprintf("SN%03d", i+1)))
	}
	return e
}

// login 发码并登录（可带邀请码与设备头）；返回已登录客户端与登录响应。
func (e *refEnv) login(phone, inviteCode, deviceID string) (*client, map[string]any) {
	e.t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &client{t: e.t, base: e.ts.URL + "/api/v1", http: &http.Client{Jar: jar}}
	e.clk.Advance(61 * time.Second)
	c.do("POST", "/auth/sms-code", map[string]string{"phone": phone})
	body := map[string]string{"phone": phone, "code": "000000"}
	if inviteCode != "" {
		body["invite_code"] = inviteCode
	}
	req, _ := http.NewRequest("POST", c.base+"/auth/login", jsonBody(body))
	req.Header.Set("Content-Type", "application/json")
	if deviceID != "" {
		req.Header.Set("X-Device-Id", deviceID)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return c, out
}

func (e *refEnv) verify(c *client, idNumber string) {
	e.t.Helper()
	if out := c.do("POST", "/kyc/personal", map[string]string{"name": "李四", "id_number": idNumber}); out["kyc_status"] != "verified" {
		e.t.Fatalf("kyc = %v", out)
	}
}

func (e *refEnv) recharge(c *client, amount int64) {
	e.t.Helper()
	rch := c.do("POST", "/recharges", map[string]any{"amount": amount, "channel": "wechat"})
	c.do("POST", "/dev/recharges/"+rch["id"].(string)+"/pay", map[string]any{})
}

func (e *refEnv) rent(c *client) (orderID, instanceID string) {
	e.t.Helper()
	out := c.do("POST", "/orders", map[string]any{"product_id": e.product, "duration": "1d", "image_id": e.image})
	if out["order"] == nil {
		e.t.Fatalf("order = %v", out)
	}
	return out["order"].(map[string]any)["id"].(string), out["instance"].(map[string]any)["id"].(string)
}

// agent 让所有机器心跳并成功执行待处理指令。
func (e *refEnv) agent() {
	e.t.Helper()
	for _, mid := range e.machines {
		if _, err := e.a.Fleet.Heartbeat(e.ctx, mid); err != nil {
			e.t.Fatal(err)
		}
		cmds, err := e.a.Fleet.PendingCommands(e.ctx, mid)
		if err != nil {
			e.t.Fatal(err)
		}
		for _, cmd := range cmds {
			if err := e.a.Rental.HandleCommandResult(e.ctx, cmd.ID, true, "ok"); err != nil {
				e.t.Fatal(err)
			}
		}
	}
}

func (e *refEnv) settle() {
	e.t.Helper()
	e.agent()
	if err := e.a.Rental.Sweep(e.ctx); err != nil {
		e.t.Fatal(err)
	}
	if err := e.a.Referral.Settle(e.ctx); err != nil {
		e.t.Fatal(err)
	}
}

func (e *refEnv) balance(c *client) int64 { return int64(c.do("GET", "/wallet", nil)["balance"].(float64)) }

func (e *refEnv) invariants() {
	e.t.Helper()
	if err := e.a.Referral.CheckInvariants(e.ctx, e.pool); err != nil {
		e.t.Fatal(err)
	}
}

func (e *refEnv) userID(phone string) int64 {
	e.t.Helper()
	var id int64
	if err := e.pool.QueryRow(e.ctx, `SELECT id FROM users WHERE phone = $1`, phone).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

// TestReferralFullChainHTTP：规格 §16.2「全链路」。
func TestReferralFullChainHTTP(t *testing.T) {
	e := newRefEnv(t, 4) // 四笔租用期间洗机尚未释放机器，需要 4 台
	a, _ := e.login("13800000001", "", "dev-a")
	e.verify(a, "110101199001011234")
	code := a.do("GET", "/referral/me", nil)["code"].(string)
	b, out := e.login("13900000001", code, "dev-b")
	if inv := out["invite"].(map[string]any); inv["bound"] != true || inv["inviter"] != "138****0001" {
		t.Fatalf("bind = %v", out)
	}
	e.verify(b, "110101199001011235")
	e.recharge(b, 100_000)
	// 第一笔按默认 7 天延迟验证「待发放 → 到期发放」
	order, _ := e.rent(b)
	e.settle()
	rw := a.do("GET", "/referral/rewards", nil)["items"].([]any)[0].(map[string]any)
	if rw["order_id"] != order || rw["reward"] != float64(690) || rw["status"] != "pending" {
		t.Fatalf("reward = %v", rw)
	}
	e.clk.Advance(7 * 24 * time.Hour)
	e.settle()
	if e.balance(a) != 690 {
		t.Fatalf("A balance = %d", e.balance(a))
	}
	led := a.do("GET", "/wallet/ledger", nil)["items"].([]any)[0].(map[string]any)
	if led["kind"] != "referral_rebate" || led["remark"] != "邀请返利 139****0001 订单 "+order {
		t.Fatalf("ledger = %v", led)
	}
	// 之后的扣回场景要在实例仍运行时退款（RefundRelease 只接受 creating / running / stopped 且 amount ≤ 可退上限），
	// 所以把延迟改为 0 天：计提与发放在同一轮 Settle 完成，不再推进时钟。
	if _, err := e.pool.Exec(e.ctx, `UPDATE referral_rate_versions SET hold_days = 0`); err != nil {
		t.Fatal(err)
	}
	order2, inst2 := e.rent(b)
	e.settle() // 计提 + 发放 → A 690 + 690 = 1380
	if e.balance(a) != 1380 {
		t.Fatalf("A after order2 = %d", e.balance(a))
	}
	// 后台对 B 的实例退 ¥30 → target 390，扣回 300
	if _, err := e.a.Rental.RefundRelease(e.ctx, audit.Actor{AdminID: 1}, inst2, 3000, "测试"); err != nil {
		t.Fatal(err)
	}
	e.settle()
	if e.balance(a) != 1080 {
		t.Fatalf("after clawback A = %d", e.balance(a))
	}
	// 欠账变体：A 先把余额花光（后台调账归零，等价于 A 把返利用于租用），再退 B 的另一台 ¥60 → target 90，excess 600，余额 0 → 欠账 600
	order3, inst3 := e.rent(b)
	e.settle() // +690 → 1770
	if _, err := e.a.Wallet.AdminAdjust(e.ctx, 1, e.userID("13800000001"), -1770, "test: spend all", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.a.Rental.RefundRelease(e.ctx, audit.Actor{AdminID: 1}, inst3, 6000, "退款"); err != nil {
		t.Fatal(err)
	}
	e.settle()
	var debt, clawed int64
	e.pool.QueryRow(e.ctx, `SELECT debt FROM referral_accounts WHERE user_id = $1`, e.userID("13800000001")).Scan(&debt)
	e.pool.QueryRow(e.ctx, `SELECT clawed_amount FROM referral_rewards WHERE order_id = $1`, order3).Scan(&clawed)
	if debt != 600 || clawed != 600 || e.balance(a) != 0 {
		t.Fatalf("debt = %d clawed = %d balance = %d", debt, clawed, e.balance(a))
	}
	// B 再租：发放 690 中 600 抵欠账、入账 90；欠账清零
	order4, _ := e.rent(b)
	e.settle()
	e.pool.QueryRow(e.ctx, `SELECT debt FROM referral_accounts WHERE user_id = $1`, e.userID("13800000001")).Scan(&debt)
	var status string
	var settled int64
	e.pool.QueryRow(e.ctx, `SELECT status, settled_amount FROM referral_rewards WHERE order_id = $1`, order4).Scan(&status, &settled)
	if debt != 0 || status != "settled" || settled != 690 || e.balance(a) != 90 {
		t.Fatalf("debt offset: debt=%d status=%s settled=%d balance=%d", debt, status, settled, e.balance(a))
	}
	_ = order2
	e.invariants()
}
```

（`jsonBody` = `bytes.NewReader(json.Marshal(...))` 小 helper；补导入 `bytes`、`encoding/json`。欠账的构造用 `AdminAdjust` 把 A 余额归零（等价于 A 把返利花在租用上）；关键断言是「余额不够扣 → `debt` 增加 → 下次发放先抵欠账、钱包只入差额」。`e.rent` 需要空闲机器：`e.settle()` 里的 `e.agent()` 会让洗机指令完成，但保留期内机器仍被占用，所以用 4 台。）

- [ ] **Step 2: 预订取消、开通失败、自刷、上限、返利期**

```go
// 预订 → 冷静期取消全退 → 到开始时刻 void、A 无流水；开通失败 refundAll → 0 → void。
func TestReferralBookingCancelAndCreateFailure(t *testing.T) {
	e := newRefEnv(t, 2)
	a, _ := e.login("13800000001", "", "")
	e.verify(a, "110101199001011234")
	code := a.do("GET", "/referral/me", nil)["code"].(string)
	b, _ := e.login("13900000001", code, "")
	e.verify(b, "110101199001011235")
	e.recharge(b, 100_000)
	uidB := e.userID("13900000001")
	start := e.clk.Now().Add(72 * time.Hour).Truncate(time.Hour)
	res, err := e.a.Rental.PlaceBatch(e.ctx, rental.BatchRequest{UserID: uidB, ProductID: e.product, ImageID: e.image, Count: 1, Duration: catalog.D1d, Quantity: 1, StartAt: &start})
	if err != nil {
		t.Fatal(err)
	}
	e.settle()
	var settleAfter time.Time
	e.pool.QueryRow(e.ctx, `SELECT settle_after FROM referral_rewards WHERE order_id = $1`, res.Orders[0].ID).Scan(&settleAfter)
	if !settleAfter.Equal(start) {
		t.Fatalf("booking settle_after = %v want %v", settleAfter, start)
	}
	if _, err := e.a.Rental.Cancel(e.ctx, uidB, res.Instances[0].ID, "127.0.0.1"); err != nil { // 15 分钟冷静期内 100%
		t.Fatal(err)
	}
	e.settle()
	e.clk.Advance(73 * time.Hour)
	e.settle()
	var status string
	e.pool.QueryRow(e.ctx, `SELECT status FROM referral_rewards WHERE order_id = $1`, res.Orders[0].ID).Scan(&status)
	if status != "void" || e.balance(a) != 0 {
		t.Fatalf("cancelled booking: status=%s balance=%d", status, e.balance(a))
	}
	// 开通失败：Create 回报失败 → refundAll（无后缀）
	order, inst := e.rent(b)
	for _, mid := range e.machines {
		cmds, _ := e.a.Fleet.PendingCommands(e.ctx, mid)
		for _, cmd := range cmds {
			if err := e.a.Rental.HandleCommandResult(e.ctx, cmd.ID, false, "disk full"); err != nil {
				t.Fatal(err)
			}
		}
	}
	e.settle()
	var reward int64
	e.pool.QueryRow(e.ctx, `SELECT reward FROM referral_rewards WHERE order_id = $1`, order).Scan(&reward)
	if reward != 0 {
		t.Fatalf("create failed reward = %d (instance %s)", reward, inst)
	}
	e.clk.Advance(8 * 24 * time.Hour)
	e.settle()
	e.pool.QueryRow(e.ctx, `SELECT status FROM referral_rewards WHERE order_id = $1`, order).Scan(&status)
	if status != "void" {
		t.Fatalf("create failed status = %s", status)
	}
	e.invariants()
}

// 同设备 → held → 到期不发放 → approve → 发放；同身份证 → revoked，待发放 void。
func TestReferralSelfDealingHeldAndRevoked(t *testing.T) {
	e := newRefEnv(t, 3)
	a, _ := e.login("13800000001", "", "dev-A")
	e.verify(a, "110101199001011234")
	code := a.do("GET", "/referral/me", nil)["code"].(string)
	d, out := e.login("13900000004", code, "dev-A") // 同设备
	if out["invite"].(map[string]any)["bound"] != true {
		t.Fatalf("same device must still bind: %v", out)
	}
	var st string
	e.pool.QueryRow(e.ctx, `SELECT status FROM referral_bindings WHERE invitee_id = $1`, e.userID("13900000004")).Scan(&st)
	if st != "held" {
		t.Fatalf("binding = %s", st)
	}
	e.verify(d, "110101199001011236")
	e.recharge(d, 100_000)
	order, _ := e.rent(d)
	e.settle()
	e.clk.Advance(8 * 24 * time.Hour)
	e.settle()
	e.pool.QueryRow(e.ctx, `SELECT status FROM referral_rewards WHERE order_id = $1`, order).Scan(&st)
	if st != "pending" || e.balance(a) != 0 {
		t.Fatalf("held must not release: %s %d", st, e.balance(a))
	}
	var bid int64
	e.pool.QueryRow(e.ctx, `SELECT id FROM referral_bindings WHERE invitee_id = $1`, e.userID("13900000004")).Scan(&bid)
	if err := e.a.Referral.Approve(e.ctx, audit.Actor{AdminID: 1}, bid, "ok"); err != nil {
		t.Fatal(err)
	}
	e.settle()
	if e.balance(a) != 690 {
		t.Fatalf("after approve = %d", e.balance(a))
	}
	// C 用 A 的身份证号实名 → revoked，待发放 void
	c, _ := e.login("13900000005", code, "dev-C")
	e.verify(c, "110101199001011234") // 与 A 同一身份证号（mock 核验只看格式）
	e.pool.QueryRow(e.ctx, `SELECT status FROM referral_bindings WHERE invitee_id = $1`, e.userID("13900000005")).Scan(&st)
	if st != "revoked" {
		t.Fatalf("same identity = %s", st)
	}
	e.invariants()
}

// 上限：max_per_invitee=1000 → 690、310、capped；30 天上限 → 推进 31 天后恢复；返利期 1 天 → 第 2 天 capped(period_over)。
func TestReferralCapsAndPeriod(t *testing.T) {
	e := newRefEnv(t, 4)
	a, _ := e.login("13800000001", "", "")
	e.verify(a, "110101199001011234")
	code := a.do("GET", "/referral/me", nil)["code"].(string)
	// 绑定前把版本改成返利期 1 天、被邀请人上限 1000、邀请人 30 天上限 1500
	e.pool.Exec(e.ctx, `UPDATE referral_rate_versions SET period_days = 1, max_per_invitee = 1000, max_per_inviter_30d = 1500`)
	b, _ := e.login("13900000001", code, "")
	e.verify(b, "110101199001011235")
	e.recharge(b, 1_000_000)
	o1, _ := e.rent(b)
	o2, _ := e.rent(b)
	o3, _ := e.rent(b)
	e.settle()
	var r1, r2, r3 int64
	e.pool.QueryRow(e.ctx, `SELECT reward FROM referral_rewards WHERE order_id = $1`, o1).Scan(&r1)
	e.pool.QueryRow(e.ctx, `SELECT reward FROM referral_rewards WHERE order_id = $1`, o2).Scan(&r2)
	e.pool.QueryRow(e.ctx, `SELECT reward FROM referral_rewards WHERE order_id = $1`, o3).Scan(&r3)
	if r1 != 690 || r2 != 310 || r3 != 0 {
		t.Fatalf("caps = %d %d %d", r1, r2, r3)
	}
	var capped int
	e.pool.QueryRow(e.ctx, `SELECT count(*) FROM referral_events WHERE kind = 'capped' AND order_id IN ($1, $2)`, o2, o3).Scan(&capped)
	if capped != 2 {
		t.Fatalf("capped events = %d", capped)
	}
	// 返利期：第 2 天的消费 period_over
	e.clk.Advance(25 * time.Hour)
	o4, _ := e.rent(b)
	e.settle()
	var kind string
	e.pool.QueryRow(e.ctx, `SELECT kind || ':' || detail FROM referral_events WHERE order_id = $1`, o4).Scan(&kind)
	if kind != "capped:period_over" {
		t.Fatalf("period over = %s", kind)
	}
	e.invariants()
}
```

- [ ] **Step 3: 并发资金一致性（Review Focus 2 + 规格 §16.2）**

```go
// TestReferralConcurrentFundsConsistent：1 邀请人、5 被邀请人，50 个 goroutine 随机下单 / 续费 / 后台退款 / 预订取消 / 推进时钟 / Settle / 冻结解冻，
// 60 秒内完成；静止后不变量全部成立、余额 ≥ 0、每条 order 流水恰处理一次。
func TestReferralConcurrentFundsConsistent(t *testing.T) {
	e := newRefEnv(t, 30)
	a, _ := e.login("13800000001", "", "")
	e.verify(a, "110101199001011234")
	code := a.do("GET", "/referral/me", nil)["code"].(string)
	uidA := e.userID("13800000001")
	e.recharge(a, 50_000)
	e.pool.Exec(e.ctx, `UPDATE referral_rate_versions SET hold_days = 1`)
	var invitees []int64
	for i := 1; i <= 5; i++ {
		phone := fmt.Sprintf("1390000000%d", i)
		c, _ := e.login(phone, code, "")
		e.verify(c, fmt.Sprintf("11010119900101%04d", 1300+i))
		e.recharge(c, 5_000_000)
		invitees = append(invitees, e.userID(phone))
	}
	ctx, cancel := context.WithTimeout(e.ctx, 60*time.Second)
	defer cancel()
	var mu sync.Mutex
	var instances []string // 被邀请人的实例
	pick := func(rng *rand.Rand) string {
		mu.Lock()
		defer mu.Unlock()
		if len(instances) == 0 {
			return ""
		}
		return instances[rng.Intn(len(instances))]
	}
	allowed := func(err error) bool {
		if err == nil {
			return true
		}
		return apperr.As(err) != nil // 业务错误（no_capacity / insufficient_balance / invalid_state / cancel_not_allowed …）都可接受
	}
	var wg sync.WaitGroup
	for g := 0; g < 50; g++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed))
			for i := 0; i < 8; i++ {
				uid := invitees[rng.Intn(len(invitees))]
				var err error
				switch rng.Intn(8) {
				case 0, 1:
					var inst rental.Instance
					_, inst, err = e.a.Rental.Place(ctx, rental.PlaceRequest{UserID: uid, ProductID: e.product, ImageID: e.image, Duration: catalog.D1d})
					if err == nil {
						mu.Lock()
						instances = append(instances, inst.ID)
						mu.Unlock()
					}
				case 2:
					if id := pick(rng); id != "" {
						_, err = e.a.Rental.Renew(ctx, uid, id, catalog.D1d, "")
					}
				case 3:
					if id := pick(rng); id != "" {
						_, err = e.a.Rental.RefundRelease(ctx, audit.Actor{AdminID: 1}, id, int64(100+rng.Intn(6800)), "并发测试")
					}
				case 4:
					start := e.clk.Now().Add(time.Duration(3+rng.Intn(48)) * time.Hour).Truncate(time.Hour)
					var res rental.BatchResult
					res, err = e.a.Rental.PlaceBatch(ctx, rental.BatchRequest{UserID: uid, ProductID: e.product, ImageID: e.image, Count: 1, Duration: catalog.D1d, Quantity: 1, StartAt: &start})
					if err == nil && rng.Intn(2) == 0 {
						_, err = e.a.Rental.Cancel(ctx, uid, res.Instances[0].ID, "127.0.0.1")
					}
				case 5:
					e.clk.Advance(time.Duration(rng.Intn(8*24)) * time.Hour)
				case 6:
					err = e.a.Referral.Settle(ctx)
				case 7:
					if ferr := e.a.Referral.Freeze(ctx, audit.Actor{AdminID: 1}, uidA, "并发"); ferr == nil {
						err = e.a.Referral.Unfreeze(ctx, audit.Actor{AdminID: 1}, uidA)
					}
				}
				if !allowed(err) {
					t.Errorf("goroutine %d op failed: %v", seed, err)
					return
				}
				if ctx.Err() != nil {
					t.Errorf("timeout / deadlock suspected: %v", ctx.Err())
					return
				}
			}
		}(int64(g))
	}
	wg.Wait()
	if t.Failed() {
		return
	}
	// 静止：确保解冻、推进 10 天、跑到没有待处理
	_ = e.a.Referral.Unfreeze(e.ctx, audit.Actor{AdminID: 1}, uidA)
	e.clk.Advance(10 * 24 * time.Hour)
	for i := 0; i < 5; i++ {
		if err := e.a.Referral.Settle(e.ctx); err != nil {
			t.Fatal(err)
		}
	}
	e.invariants()
	var pending, unprocessed int
	e.pool.QueryRow(e.ctx, `SELECT count(*) FROM referral_rewards WHERE status = 'pending' AND settle_after <= $1`, e.clk.Now()).Scan(&pending)
	e.pool.QueryRow(e.ctx, `SELECT count(*) FROM wallet_ledger l JOIN referral_bindings b ON b.invitee_id = l.user_id
		WHERE l.ref_type = 'order' AND NOT EXISTS (SELECT 1 FROM referral_events e WHERE e.source_ledger_id = l.id)`).Scan(&unprocessed)
	if pending != 0 || unprocessed != 0 {
		t.Fatalf("not quiescent: due pending=%d unprocessed=%d", pending, unprocessed)
	}
	var negative int
	e.pool.QueryRow(e.ctx, `SELECT count(*) FROM wallet_accounts WHERE balance < 0`).Scan(&negative)
	if negative != 0 {
		t.Fatal("negative balance")
	}
}
```

- [ ] **Step 4: 运行**

Run: `go vet ./internal/app/ && go test -race ./internal/app/ -run 'TestReferral' -v 2>&1 | tail -15`
Expected: 5 个用例 PASS；并发用例在 60 秒内结束（通常 < 20 秒）。再跑 `go test ./... 2>&1 | tail -20` 确认全绿。

- [ ] **Step 5: 提交**

```bash
git add internal/app/referral_e2e_test.go
git commit -m "test(referral): integration tests (full chain, booking cancel, create failure, self-dealing, caps, period, concurrent funds consistency)

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 15: 控制台：落地参数、`X-Device-Id`、登录页邀请提示、`/invite` 邀请页、「我的」入口、钱包标签、帮助页

**Files:**
- Create: `web/console/src/logic/invite.ts`、`web/console/src/logic/__tests__/invite.spec.ts`、`web/console/src/views/InviteView.vue`、`web/shared/src/types/referral.ts`
- Modify: `web/shared/src/http.ts`（`X-Device-Id`）、`web/shared/src/__tests__/http.spec.ts`（追加）、`web/shared/src/types/index.ts`、`web/shared/src/types/user.ts`（`invite?`、`referral_month_settled?`）、`web/console/src/App.vue`、`web/console/src/router.ts`、`web/console/src/__tests__/router.spec.ts`（路由数）、`web/console/src/views/LoginView.vue`、`web/console/src/views/AccountView.vue`、`web/console/src/views/WalletView.vue`（标签颜色）、`web/console/src/views/HelpView.vue`

**Interfaces:**
- Consumes: Task 12 的接口；`qrcode`（已有依赖）；`uuid()`（`web/shared/src/uuid.ts`）。
- Produces:
  - `web/shared/src/types/referral.ts`：
    ```ts
    export interface InviteResult { bound: boolean; reason: 'ok' | 'invalid_code' | 'inviter_frozen' | 'existing_user'; inviter?: string }
    export interface RebateRule { rate_bp: number; period_days: number; hold_days: number; max_per_invitee: number; max_per_inviter_30d: number }
    export interface ReferralMe { code: string; link: string; status: 'active' | 'frozen'; kyc_verified: boolean; rebate: RebateRule; stats: { invitees: number; verified_invitees: number; pending: number; settled_net: number; month_settled: number; debt: number } }
    export interface Invitee { phone_masked: string; bound_at: string; status: 'active' | 'held' | 'revoked'; kyc_verified: boolean; contributed: number; rebate_until: string }
    export interface RewardItem { order_id: string; invitee_masked: string; paid_at: string; order_amount: number; refunded_amount: number; reward: number; status: 'pending' | 'settled' | 'void'; settled_at: string | null; clawed_amount: number }
    export const bindingStatusLabel: LabelMap = { active: '有效', held: '审核中', revoked: '已失效' }
    export const rewardStatusLabel: LabelMap = { pending: '待发放', settled: '已发放', void: '已作废' }
    ```
    `types/user.ts` 的 `User` 加 `invite?: InviteResult`、`referral_month_settled?: number`。
  - `web/shared/src/http.ts`：模块级 `deviceId()`：`localStorage.sc_device` 没有则 `uuid()` 写入（`try/catch`，失败则每次新 UUID）；`raw()` 与 `upload / download` 的请求头都加 `'X-Device-Id': deviceId()`。`HttpConfig` 加 `deviceId?: () => string`（测试注入；缺省读 localStorage）。
  - `web/console/src/logic/invite.ts`：
    ```ts
    export const INVITE_KEY = 'sc_invite'
    export const INVITE_TTL_MS = 30 * 86_400_000
    const ALPHABET = '0123456789ABCDEFGHJKMNPQRSTVWXYZ'
    /** 与后端 referral.NormalizeCode 同规则；不合法返回 null */
    export function normalizeInviteCode(raw: string): string | null
    /** 从地址栏取 ?ic=，规范化；返回 null 表示没有或不合法 */
    export function parseInviteParam(search: string): string | null
    export function storeInvite(code: string, now = Date.now(), storage: Storage = localStorage): void      // {code, at}
    export function readInvite(now = Date.now(), storage: Storage = localStorage): string | null             // 过期（> 30 天）则清除并返回 null
    export function clearInvite(storage: Storage = localStorage): void
    export function inviteToast(r: InviteResult): { type: 'success' | 'warning' | null; message: string }   // bound → success「已绑定邀请关系」；existing_user → null；其它 → warning「邀请码无效」
    ```
  - `App.vue`：`onMounted` / 首次路由后读 `parseInviteParam(location.search)`；有码且 `session.status !== 'authed'` → `storeInvite`；无论是否存都 `router.replace({ query: { ...route.query, ic: undefined } })` 去掉参数。
  - `LoginView.vue`：`onMounted` 读 `readInvite()` → `api.get('/referral/codes/' + code)` → `valid` 时显示 `data-testid="login-invite"`「通过 {{inviter}} 的邀请注册」+ `data-testid="login-invite-clear"`（×，`clearInvite` 并隐藏）；无效 → `clearInvite`、不显示。提交体带 `invite_code`（有则带）；响应 `u.invite`：按 `inviteToast` 提示（`ElMessage.success / warning`），任何情况 `clearInvite()`。
  - `router.ts`：`{ path: '/invite', component: InviteView, meta: { title: '邀请返利', auth: true } }`；`router.spec.ts` 的路由数 +1。
  - `InviteView.vue`（`data-testid`）：`invite-code`、`invite-copy-code`、`invite-link`、`invite-copy-link`、`invite-qr`（`<canvas>`，`QRCode.toCanvas`）、`invite-rules`（文案含 `{{rate}}%`、`{{period_days}} 个月`（`period_days / 30` 向下取整，不足 1 月显示天）、`{{hold_days}} 天后到账`、「不可提现」、上限 `fmtYuan(max_per_invitee)` / `fmtYuan(max_per_inviter_30d)`）、`invite-kyc-alert`（未实名提示条，含 `router-link to="/kyc"`）、`invite-frozen-alert`（冻结时显示，隐藏码 / 链接 / 二维码）、统计 `invite-stat-invitees / invite-stat-verified / invite-stat-pending / invite-stat-settled / invite-stat-month`、Tab `invite-tab-invitees` / `invite-tab-rewards`、行 `invitee-row`（`invitee-phone / invitee-status / invitee-kyc / invitee-contributed / invitee-until`）、`reward-row`（`reward-order / reward-invitee / reward-amount / reward-status / reward-clawed`（有扣回时「已扣回 ¥X」））、分页 `invite-invitees-pager` / `invite-rewards-pager`（`page_size 20`）。复制用 `navigator.clipboard.writeText` + `ElMessage.success('已复制')`。
  - `AccountView.vue`：`actions` 里加 `<router-link to="/invite" data-testid="account-invite">邀请返利<span v-if="month" data-testid="account-invite-month" class="muted">本月返利 {{ fmtYuan(month) }}</span></router-link>`，`month = session.user?.referral_month_settled ?? 0`（`probeSession` 的 `/me` 已带该字段；进入「我的」页时再 `api.get('/me')` 刷新一次 `setUser`）。
  - `WalletView.vue`：`ledger-kind` 加 class：`referral_rebate → money-plus`（绿）、`referral_clawback → money-minus`（橙）——用现有 `.money-plus/.money-minus` 并新增 `.kind-rebate { color: var(--el-color-success) }`、`.kind-clawback { color: var(--el-color-warning) }`。
  - `HelpView.vue`：新 `<el-card>`，`<h2 data-testid="help-referral">邀请返利</h2>`，列表：规则（被邀请人实付租用金额的一定比例，默认 10%，以邀请页显示为准）、到账时间（扣款 7 天后或预订开始后）、退款扣回与欠账、上限、返利不可提现 / 不可转让、禁止自刷（同设备 / 同身份会被搁置或撤销，平台有权撤销异常返利）。

- [ ] **Step 1: 写失败的单元测试**

`web/console/src/logic/__tests__/invite.spec.ts`：

```ts
import { describe, expect, it } from 'vitest'
import { clearInvite, inviteToast, normalizeInviteCode, parseInviteParam, readInvite, storeInvite } from '../invite'

class MemStorage implements Storage {
  private m = new Map<string, string>()
  get length() { return this.m.size }
  clear() { this.m.clear() }
  getItem(k: string) { return this.m.get(k) ?? null }
  key(i: number) { return [...this.m.keys()][i] ?? null }
  removeItem(k: string) { this.m.delete(k) }
  setItem(k: string, v: string) { this.m.set(k, v) }
}

describe('invite code (Review Focus 5)', () => {
  it('规范化：空白、小写、O/I/L 混写；长度与字母表', () => {
    expect(normalizeInviteCode(' o1lI0Zy9 ')).toBe('01110ZY9')
    expect(normalizeInviteCode('abcdefgh')).toBe('ABCDEFGH')
    expect(normalizeInviteCode('ABCDEFG')).toBeNull()
    expect(normalizeInviteCode('ABCDEFGU')).toBeNull()
    expect(normalizeInviteCode('ABCD-EFG')).toBeNull()
    expect(normalizeInviteCode('')).toBeNull()
  })
  it('parseInviteParam 只认合法的 ic', () => {
    expect(parseInviteParam('?ic=abcdefgh&x=1')).toBe('ABCDEFGH')
    expect(parseInviteParam('?ic=hello%20world')).toBeNull()
    expect(parseInviteParam('?other=1')).toBeNull()
    expect(parseInviteParam('')).toBeNull()
  })
  it('存储 30 天过期', () => {
    const s = new MemStorage()
    storeInvite('ABCDEFGH', 1_000, s)
    expect(readInvite(1_000 + 29 * 86_400_000, s)).toBe('ABCDEFGH')
    expect(readInvite(1_000 + 31 * 86_400_000, s)).toBeNull()
    expect(s.getItem('sc_invite')).toBeNull() // 过期即清
    storeInvite('ABCDEFGH', 1_000, s)
    clearInvite(s)
    expect(readInvite(2_000, s)).toBeNull()
    s.setItem('sc_invite', '{broken')
    expect(readInvite(2_000, s)).toBeNull()
  })
  it('提示文案', () => {
    expect(inviteToast({ bound: true, reason: 'ok', inviter: '138****1234' })).toEqual({ type: 'success', message: '已绑定邀请关系' })
    expect(inviteToast({ bound: false, reason: 'existing_user' })).toEqual({ type: null, message: '' })
    expect(inviteToast({ bound: false, reason: 'invalid_code' })).toEqual({ type: 'warning', message: '邀请码无效' })
    expect(inviteToast({ bound: false, reason: 'inviter_frozen' })).toEqual({ type: 'warning', message: '邀请码无效' })
  })
})
```

`web/shared/src/__tests__/http.spec.ts` 追加：

```ts
describe('X-Device-Id (计划 8)', () => {
  it('每个请求都带设备头；注入的 deviceId 优先', async () => {
    const f = fakeFetch(json(200, {}), json(200, {}))
    const http = createHttp({ base: '/api/v1', onUnauthorized: vi.fn(), fetch: f.fn, deviceId: () => 'dev-1' })
    await http.get('/me')
    await http.post('/auth/login', { phone: '1' })
    for (const c of f.calls) expect((c.init.headers as Record<string, string>)['X-Device-Id']).toBe('dev-1')
  })
})
```

- [ ] **Step 2: 运行确认失败**

Run: `cd web && npm test 2>&1 | tail -8`
Expected: `invite.spec.ts` 找不到模块；`http.spec.ts` 的 `deviceId` 类型错误。

- [ ] **Step 3: `logic/invite.ts`、`http.ts`**

`web/console/src/logic/invite.ts`：

```ts
import type { InviteResult } from '@spark/shared'

export const INVITE_KEY = 'sc_invite'
export const INVITE_TTL_MS = 30 * 86_400_000
const ALPHABET = '0123456789ABCDEFGHJKMNPQRSTVWXYZ'

/** 与后端 referral.NormalizeCode 同规则：去空白、大写、O→0、I/L→1；8 位且全在字母表内才合法。 */
export function normalizeInviteCode(raw: string): string | null {
  const s = raw.replace(/\s+/g, '').toUpperCase().replace(/O/g, '0').replace(/[IL]/g, '1')
  if (s.length !== 8) return null
  for (const ch of s) if (!ALPHABET.includes(ch)) return null
  return s
}

export function parseInviteParam(search: string): string | null {
  const v = new URLSearchParams(search).get('ic')
  return v ? normalizeInviteCode(v) : null
}

export function storeInvite(code: string, now = Date.now(), storage: Storage = localStorage): void {
  try {
    storage.setItem(INVITE_KEY, JSON.stringify({ code, at: now }))
  } catch {
    /* 隐私模式等：忽略 */
  }
}

export function readInvite(now = Date.now(), storage: Storage = localStorage): string | null {
  try {
    const raw = storage.getItem(INVITE_KEY)
    if (!raw) return null
    const { code, at } = JSON.parse(raw) as { code?: unknown; at?: unknown }
    if (typeof code !== 'string' || typeof at !== 'number' || now - at > INVITE_TTL_MS) {
      storage.removeItem(INVITE_KEY)
      return null
    }
    return normalizeInviteCode(code)
  } catch {
    try {
      storage.removeItem(INVITE_KEY)
    } catch {
      /* ignore */
    }
    return null
  }
}

export function clearInvite(storage: Storage = localStorage): void {
  try {
    storage.removeItem(INVITE_KEY)
  } catch {
    /* ignore */
  }
}

export function inviteToast(r: InviteResult): { type: 'success' | 'warning' | null; message: string } {
  if (r.bound) return { type: 'success', message: '已绑定邀请关系' }
  if (r.reason === 'existing_user') return { type: null, message: '' }
  return { type: 'warning', message: '邀请码无效' }
}
```

`web/shared/src/http.ts`：

```ts
import { uuid } from './uuid'

export const DEVICE_KEY = 'sc_device'
/** 控制台设备标识（localStorage，首次生成）；读写失败（隐私模式）时每次新 UUID，可接受。 */
export function defaultDeviceId(): string {
  try {
    const v = localStorage.getItem(DEVICE_KEY)
    if (v) return v
    const id = uuid()
    localStorage.setItem(DEVICE_KEY, id)
    return id
  } catch {
    return uuid()
  }
}
```

`HttpConfig` 加 `deviceId?: () => string`；`createHttp` 里 `const deviceId = cfg.deviceId ?? defaultDeviceId`；`raw()` 的 `headers` 加 `'X-Device-Id': deviceId()`；`upload` / `download` 的 `doFetch` 调用加 `headers: { 'X-Device-Id': deviceId() }`。

- [ ] **Step 4: `App.vue`、`LoginView.vue`、`router.ts`**

`App.vue` 加：

```ts
import { onMounted } from 'vue'
import { useRouter } from 'vue-router'
import { parseInviteParam, storeInvite } from './logic/invite'
import { probeSession, session } from './store/session'

const router = useRouter()
onMounted(async () => {
  const code = parseInviteParam(window.location.search)
  if (!new URLSearchParams(window.location.search).has('ic')) return
  await probeSession()
  if (code && session.status !== 'authed') storeInvite(code) // 已登录的老用户不存：不是新用户
  await router.isReady()
  const q = { ...router.currentRoute.value.query }
  delete q.ic
  await router.replace({ path: router.currentRoute.value.path, query: q })
})
```

`LoginView.vue` 加：

```ts
import { clearInvite, inviteToast, readInvite } from '../logic/invite'
const inviteCode = ref<string | null>(null)
const inviter = ref('')
onMounted(async () => {
  const code = readInvite()
  if (!code) return
  try {
    const r = await api.get<{ valid: boolean; inviter: string }>(`/referral/codes/${code}`)
    if (r.valid) {
      inviteCode.value = code
      inviter.value = r.inviter
    } else clearInvite()
  } catch {
    /* 查不到就不显示；提交时仍不带码 */
  }
}
function dropInvite() {
  inviteCode.value = null
  clearInvite()
}
```

`submit()` 的请求体：`{ phone: p, code: c, ...(inviteCode.value ? { invite_code: inviteCode.value } : {}) }`；成功后：

```ts
    if (u.invite) {
      const t = inviteToast(u.invite)
      if (t.type === 'success') ElMessage.success(t.message)
      else if (t.type === 'warning') ElMessage.warning(t.message)
    }
    clearInvite()
```

模板在 `<el-form>` 之前：

```html
<el-alert v-if="inviteCode" type="info" :closable="false" show-icon data-testid="login-invite">
  通过 <b>{{ inviter }}</b> 的邀请注册
  <el-button link size="small" data-testid="login-invite-clear" @click="dropInvite">×</el-button>
</el-alert>
```

`router.ts` 在 `/account/ssh-keys` 之后加 `{ path: '/invite', component: () => import('./views/InviteView.vue'), meta: { title: '邀请返利', auth: true } }`；`__tests__/router.spec.ts` 中路由总数断言 +1、`auth` 路由列表加 `/invite`。

- [ ] **Step 5: `InviteView.vue`**

```vue
<script setup lang="ts">
import { bindingStatusLabel, fmtDate, fmtTime, fmtYuan, labelOf, rewardStatusLabel, type Invitee, type Paged, type ReferralMe, type RewardItem } from '@spark/shared'
import { ElMessage } from 'element-plus'
import QRCode from 'qrcode'
import { computed, nextTick, onMounted, ref, watch } from 'vue'
import { api } from '../api'
import { notifyError } from '../logic/notify'

const me = ref<ReferralMe | null>(null)
const loadError = ref('')
const qr = ref<HTMLCanvasElement | null>(null)
const tab = ref<'invitees' | 'rewards'>('invitees')
const invitees = ref<Invitee[]>([])
const inviteesTotal = ref(0)
const inviteesPage = ref(1)
const rewards = ref<RewardItem[]>([])
const rewardsTotal = ref(0)
const rewardsPage = ref(1)
const PAGE = 20

const rate = computed(() => (me.value ? me.value.rebate.rate_bp / 100 : 0))
const period = computed(() => {
  const d = me.value?.rebate.period_days ?? 0
  return d >= 30 ? `${Math.floor(d / 30)} 个月` : `${d} 天`
})

async function load() {
  try {
    me.value = await api.get<ReferralMe>('/referral/me')
    loadError.value = ''
    await nextTick()
    if (qr.value && me.value.status === 'active') await QRCode.toCanvas(qr.value, me.value.link, { width: 160, margin: 1 })
  } catch (e) {
    notifyError(e)
    loadError.value = '加载失败'
  }
}
async function loadInvitees() {
  try {
    const out = await api.get<Paged<Invitee>>('/referral/invitees', { page: inviteesPage.value, page_size: PAGE })
    invitees.value = out.items
    inviteesTotal.value = out.total
  } catch (e) {
    notifyError(e)
  }
}
async function loadRewards() {
  try {
    const out = await api.get<Paged<RewardItem>>('/referral/rewards', { page: rewardsPage.value, page_size: PAGE })
    rewards.value = out.items
    rewardsTotal.value = out.total
  } catch (e) {
    notifyError(e)
  }
}
async function copy(text: string) {
  try {
    await navigator.clipboard.writeText(text)
    ElMessage.success('已复制')
  } catch {
    ElMessage.warning('复制失败，请手动复制')
  }
}
onMounted(() => {
  void load()
  void loadInvitees()
})
watch(tab, (t) => (t === 'rewards' ? loadRewards() : loadInvitees()))
watch(inviteesPage, loadInvitees)
watch(rewardsPage, loadRewards)
</script>

<template>
  <div v-if="me" class="invite">
    <el-alert v-if="me.status === 'frozen'" type="warning" :closable="false" show-icon title="邀请功能已暂停，如有疑问请联系客服" data-testid="invite-frozen-alert" />
    <el-alert v-else-if="!me.kyc_verified" type="info" :closable="false" show-icon data-testid="invite-kyc-alert">
      实名认证后才能收到返利 <router-link to="/kyc">去实名</router-link>
    </el-alert>
    <el-card v-if="me.status === 'active'">
      <div class="code-row">
        <div>
          <div class="muted">我的邀请码</div>
          <div class="code" data-testid="invite-code">{{ me.code }}</div>
          <el-button size="small" data-testid="invite-copy-code" @click="copy(me.code)">复制邀请码</el-button>
        </div>
        <canvas ref="qr" data-testid="invite-qr" />
      </div>
      <div class="link-row">
        <span class="mono" data-testid="invite-link">{{ me.link }}</span>
        <el-button size="small" data-testid="invite-copy-link" @click="copy(me.link)">复制链接</el-button>
      </div>
      <p class="muted" data-testid="invite-rules">
        好友通过您的链接注册后 {{ period }} 内，其实付租用金额的 {{ rate }}% 将在扣款 {{ me.rebate.hold_days }} 天后（预订单在开始后）返到您的余额；返利不可提现；
        单个好友累计最多 {{ fmtYuan(me.rebate.max_per_invitee) }}，每 30 天最多 {{ fmtYuan(me.rebate.max_per_inviter_30d) }}；退款按比例扣回。
      </p>
    </el-card>
    <el-card>
      <div class="stats">
        <div><span class="muted">已邀请</span><b data-testid="invite-stat-invitees">{{ me.stats.invitees }}</b></div>
        <div><span class="muted">已实名</span><b data-testid="invite-stat-verified">{{ me.stats.verified_invitees }}</b></div>
        <div><span class="muted">待发放</span><b data-testid="invite-stat-pending">{{ fmtYuan(me.stats.pending) }}</b></div>
        <div><span class="muted">累计已发放</span><b data-testid="invite-stat-settled">{{ fmtYuan(me.stats.settled_net) }}</b></div>
        <div><span class="muted">本月已发放</span><b data-testid="invite-stat-month">{{ fmtYuan(me.stats.month_settled) }}</b></div>
      </div>
    </el-card>
    <el-card>
      <el-tabs v-model="tab">
        <el-tab-pane label="被邀请人" name="invitees" data-testid="invite-tab-invitees">
          <div v-for="i in invitees" :key="i.phone_masked + i.bound_at" class="row-card" data-testid="invitee-row">
            <div class="row-head">
              <span data-testid="invitee-phone">{{ i.phone_masked }}</span>
              <span><span data-testid="invitee-status">{{ labelOf(bindingStatusLabel, i.status) }}</span> · <span data-testid="invitee-kyc">{{ i.kyc_verified ? '已实名' : '未实名' }}</span></span>
            </div>
            <div class="muted">注册 {{ fmtTime(i.bound_at) }} · 累计贡献 <span data-testid="invitee-contributed">{{ fmtYuan(i.contributed) }}</span> · 返利截止 <span data-testid="invitee-until">{{ fmtDate(i.rebate_until) }}</span></div>
          </div>
          <el-empty v-if="invitees.length === 0" description="还没有被邀请人" />
          <el-pagination v-if="inviteesTotal > PAGE" v-model:current-page="inviteesPage" :page-size="PAGE" :total="inviteesTotal" layout="prev, pager, next" data-testid="invite-invitees-pager" />
        </el-tab-pane>
        <el-tab-pane label="返利明细" name="rewards" data-testid="invite-tab-rewards">
          <div v-for="r in rewards" :key="r.order_id" class="row-card" data-testid="reward-row">
            <div class="row-head">
              <span><span data-testid="reward-invitee">{{ r.invitee_masked }}</span> · <span class="mono" data-testid="reward-order">{{ r.order_id }}</span></span>
              <b data-testid="reward-amount">{{ fmtYuan(r.reward) }}</b>
            </div>
            <div class="muted">
              消费 {{ fmtYuan(r.order_amount) }}<span v-if="r.refunded_amount">（已退 {{ fmtYuan(r.refunded_amount) }}）</span> · {{ fmtTime(r.paid_at) }} ·
              <span data-testid="reward-status">{{ labelOf(rewardStatusLabel, r.status) }}</span>
              <span v-if="r.status === 'settled' && r.clawed_amount > 0" data-testid="reward-clawed">（已扣回 {{ fmtYuan(r.clawed_amount) }}）</span>
            </div>
          </div>
          <el-empty v-if="rewards.length === 0" description="暂无返利" />
          <el-pagination v-if="rewardsTotal > PAGE" v-model:current-page="rewardsPage" :page-size="PAGE" :total="rewardsTotal" layout="prev, pager, next" data-testid="invite-rewards-pager" />
        </el-tab-pane>
      </el-tabs>
    </el-card>
  </div>
  <el-card v-else-if="loadError"><el-alert type="error" :title="loadError" :closable="false" /><el-button @click="load">重试</el-button></el-card>
</template>

<style scoped>
.invite { display: grid; gap: 16px; max-width: 900px; }
.code-row { display: flex; justify-content: space-between; align-items: center; gap: 16px; flex-wrap: wrap; }
.code { font-size: 32px; font-weight: 700; letter-spacing: 4px; font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.link-row { display: flex; gap: 8px; align-items: center; margin-top: 12px; flex-wrap: wrap; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; word-break: break-all; }
.muted { color: #909399; font-size: 13px; }
.stats { display: grid; grid-template-columns: repeat(auto-fit, minmax(120px, 1fr)); gap: 12px; }
.stats div { display: flex; flex-direction: column; }
.row-card { padding: 12px 0; border-bottom: 1px solid #ebeef5; }
.row-head { display: flex; justify-content: space-between; align-items: center; }
</style>
```

- [ ] **Step 6: `AccountView` 入口、`WalletView` 颜色、`HelpView` 一节**

`AccountView.vue`：`onMounted(async () => { try { setUser(await api.get<User>('/me')) } catch { /* 用缓存 */ } })`；模板 `actions` 第一项：

```html
<router-link to="/invite" data-testid="account-invite">邀请返利<span v-if="session.user?.referral_month_settled" class="muted" data-testid="account-invite-month">本月返利 {{ fmtYuan(session.user.referral_month_settled) }}</span></router-link>
```

`WalletView.vue` 两处 `ledger-kind` 的 `<span>` 加 `:class="{ 'kind-rebate': r.kind === 'referral_rebate', 'kind-clawback': r.kind === 'referral_clawback' }"`（表格分支用 `row.kind`），样式 `.kind-rebate { color: var(--el-color-success); } .kind-clawback { color: var(--el-color-warning); }`。

`HelpView.vue` 末尾加：

```html
<el-card>
  <h2 data-testid="help-referral">邀请返利</h2>
  <ul>
    <li>在「我的 → 邀请返利」获取您的邀请码与链接；好友通过链接注册（首次登录）即绑定，老用户不能再绑定。</li>
    <li>绑定后的返利期内（默认 12 个月），好友每笔实付租用金额（新购、续费、预订）的一定比例（默认 10%，以邀请页显示为准）返到您的余额，可用于租用，<b>不可提现、不可转让</b>。</li>
    <li>返利在扣款 7 天后到账；预订单在预订开始后到账。好友退款时按比例扣回，余额不足的部分记为欠账，从之后的返利里抵扣。</li>
    <li>单个好友累计返利与每 30 天的发放都有上限（以邀请页显示为准），超出部分不计。</li>
    <li>禁止自刷：同一设备、同一身份证 / 营业执照注册的账号互相邀请会被搁置或撤销；平台有权撤销异常返利并冻结邀请资格。</li>
  </ul>
</el-card>
```

- [ ] **Step 7: 运行**

Run: `cd web && npm test 2>&1 | tail -8 && npm run build 2>&1 | tail -5`
Expected: vitest 全绿（含 `router.spec.ts` 更新后的数量）；构建成功。

- [ ] **Step 8: 提交**

```bash
git add web/shared/src web/console/src
git commit -m "feat(console): invite landing param, X-Device-Id, login invite banner, /invite page, account entry, wallet labels, help section

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 16: 后台：`/referral` 五个 Tab、用户详情邀请区块、仪表盘卡片、菜单与 `menu.spec.ts`

**Files:**
- Create: `web/admin/src/views/ReferralView.vue`
- Modify: `web/admin/src/menu.ts`、`web/admin/src/router.ts`、`web/admin/src/__tests__/menu.spec.ts`、`web/admin/src/views/UserDetailView.vue`、`web/admin/src/views/DashboardView.vue`、`web/shared/src/types/admin.ts`（`UserDetail.referral`、`Dashboard.referral_month_settled`、`AdminReferral*` 类型）

**Interfaces:**
- Consumes: Task 13 的接口；`FinancePage.vue` 不复用（筛选字段不同），用 `usePaged` + `DateRange` + `downloadBlob`（`logic/csv.ts`）+ 请求层 TOTP 弹框（`http.ts` 的 `promptTotp` 已全局接入，写接口收到 `totp_required` 自动弹 `totp-dialog`）。
- Produces:
  - `menu.ts`：`{ key: 'referral', path: '/referral', label: '邀请返利', roles: FIN_ROLES }`（放在 `recharges` 之后）；`router.ts`：`{ path: '/referral', component: ReferralView, meta: { title: '邀请返利', roles: FIN_ROLES } }`；`menu.spec.ts`：`super 16`、`ops 10`（missing 加 `'referral'`）、`finance 10`。
  - `types/admin.ts`：`AdminReferralSummary { pending; month_settled; settled_net; debt; held_count }`、`AdminRateVersion`（同 `RebateRule` + `id, effective_from, note, created_by, created_at`）、`AdminInviterRow`、`AdminBinding`、`AdminReferralReward`、`AdminInviterDetail`、`UserDetail.referral: { inviter?: { user_id; phone; binding_status }; account?: { code; status; invitees; settled_net; pending; debt } }`、`Dashboard.referral_month_settled: number`。
  - `ReferralView.vue`（Tab 用 `?tab=` 保持）与 `data-testid`：
    - 概览 `ref-tab-overview`：`ref-sum-pending / ref-sum-month / ref-sum-net / ref-sum-debt / ref-sum-held`。
    - 待审核 `ref-tab-held`：表 `held-row`（`held-inviter`（`IdLink`）、`held-invitee`、`held-flags`（标签：`same_ip 同IP / same_device 同设备 / burst 爆发 / same_identity 同身份`）、`held-bound`、`held-pending`）；行按钮 `held-approve`（弹 `held-approve-dialog`：备注 `held-approve-note`、`held-approve-ok`）、`held-revoke`（弹 `held-revoke-dialog`：原因必填 `held-revoke-reason`、`held-revoke-ok`；成功后 `ElMessage.success('已撤销，作废待发放 ¥X')`）。
    - 邀请人 `ref-tab-inviters`：筛选 `inv-q`（数字）、`inv-status`、`inv-flag`、`inv-sort`；表 `inv-row`（`inv-user`、`inv-code`、`inv-status`、`inv-invitees`、`inv-held`、`inv-pending`、`inv-net`、`inv-debt`）；行点击开抽屉 `inv-drawer`（码、状态、欠账、被邀请人表 `inv-binding-row`、返利表 `inv-reward-row`、事件表 `inv-event-row`）；抽屉按钮 `inv-freeze`（弹 `inv-freeze-dialog`：`inv-freeze-reason`、`inv-freeze-ok`）、`inv-unfreeze`。
    - 返利明细 `ref-tab-rewards`：筛选 `rw-inviter`、`rw-invitee`、`rw-status`、`DateRange`（`rw-range`）；表 `rw-row`（`rw-order / rw-inviter-id / rw-invitee-id / rw-amount / rw-refunded / rw-reward / rw-status / rw-settled-at / rw-clawed`）；`rw-export`（`api.download('/referral/rewards/export', applied)`）；绑定导出按钮在邀请人 Tab：`inv-export`（`/referral/bindings/export`）。
    - 配置 `ref-tab-config`（仅 `super` 显示，`session.admin.role`）：当前版本 `cfg-current-*`（rate / invitee / inviter / hold / period / effective）、历史表 `cfg-version-row`；表单 `cfg-rate`（%，两位小数 → `rate_bp = Math.round(x * 100)`）、`cfg-invitee`（元 → 分，`yuanToCents`）、`cfg-inviter`、`cfg-hold`、`cfg-period`、`cfg-effective`（`el-date-picker` datetime，缺省 now，提交 RFC3339）、`cfg-note`、`cfg-submit`；错误 `referral_config_invalid` 用 `notifyError` 原文提示。
    - 重扫（配置 Tab 下方，`super`）：`cfg-rescan-days`（1～30，缺省 3）、`cfg-rescan`，成功 `ElMessage.success('已重扫')`。
  - `UserDetailView.vue`：描述列表后加卡片 `user-referral`：被邀请人侧 `user-referral-inviter`（`IdLink` + `bindingStatusLabel`），邀请人侧 `user-referral-code / user-referral-status / user-referral-invitees / user-referral-net / user-referral-pending / user-referral-debt`，链接 `user-referral-link`（`/referral?tab=inviters&q=<id>`，`canFinance` 时显示）；两者都没有时显示「无邀请关系」`user-referral-none`。
  - `DashboardView.vue`：`充值金额` 卡片之后加 `<el-card v-if="session.admin?.role !== 'ops'" header="本月已发放返利"><div class="big" data-testid="dash-referral-month">{{ fmtYuan(data.referral_month_settled) }}</div></el-card>`。

- [ ] **Step 1: 改 `menu.spec.ts` 并确认失败**

```ts
  it.each([
    ['super', 16, []],
    ['ops', 10, ['orders', 'ledger', 'recharges', 'catalog', 'admins', 'referral']],
    ['finance', 10, ['machines', 'pdus', 'kyc', 'catalog', 'admins', 'groups']],
  ] as const)
```

Run: `cd web && npm test -- menu 2>&1 | tail -6`
Expected: 数量不符而失败。

- [ ] **Step 2: 菜单、路由、类型、视图**

按 Interfaces 实现。`ReferralView.vue` 的骨架（五个 Tab 各自 `usePaged`；只写关键逻辑，模板按上面的 `data-testid` 清单铺开）：

```ts
const tabs = ['overview', 'held', 'inviters', 'rewards', 'config'] as const
type Tab = (typeof tabs)[number]
const tab = computed<Tab>({ get: () => (tabs.includes(route.query.tab as Tab) ? (route.query.tab as Tab) : 'overview'), set: (v) => void router.replace({ query: { ...route.query, tab: v } }) })
const isSuper = computed(() => session.admin?.role === 'super')
const summary = ref<AdminReferralSummary | null>(null)
const held = usePaged<AdminBinding>((page, size) => api.get<Paged<AdminBinding>>('/referral/bindings', { status: 'held', page, page_size: size }))
const invFilter = reactive({ q: String(route.query.q ?? ''), status: '', flag: '', sort: 'settled' })
let invApplied = { ...invFilter }
const inviters = usePaged<AdminInviterRow>((page, size) => api.get<Paged<AdminInviterRow>>('/referral/inviters', { ...invApplied, page, page_size: size }))
const rwFilter = reactive({ inviter_id: '', invitee_id: '', status: '', range: { from: '', to: '' } })
let rwApplied = rwQuery()
const rewards = usePaged<AdminReferralReward>((page, size) => api.get<Paged<AdminReferralReward>>('/referral/rewards', { ...rwApplied, page, page_size: size }))
async function approve(b: AdminBinding, note: string) { await api.post(`/referral/bindings/${b.id}/approve`, { note }); ElMessage.success('已通过'); await Promise.all([held.load(), loadSummary()]) }
async function revoke(b: AdminBinding, reason: string) { const out = await api.post<{ voided_amount: number }>(`/referral/bindings/${b.id}/revoke`, { reason }); ElMessage.success(`已撤销，作废待发放 ${fmtYuan(out.voided_amount)}`); await Promise.all([held.load(), loadSummary()]) }
async function freeze(userId: number, reason: string) { await api.post(`/referral/inviters/${userId}/freeze`, { reason }); ElMessage.success('已冻结'); await openDrawer(userId) }
async function unfreeze(userId: number) { await api.post(`/referral/inviters/${userId}/unfreeze`); ElMessage.success('已解冻'); await openDrawer(userId) }
async function submitConfig() { const body = { rate_bp: Math.round(cfg.rate * 100), max_per_invitee: yuanToCents(cfg.invitee), max_per_inviter_30d: yuanToCents(cfg.inviter), hold_days: cfg.hold, period_days: cfg.period, effective_from: cfg.effective ? new Date(cfg.effective).toISOString() : undefined, note: cfg.note }; await api.post('/referral/config', body); ElMessage.success('已新增版本'); await loadConfig() }
async function rescan() { await api.post('/referral/rescan', { days: cfg.rescanDays }); ElMessage.success('已重扫') }
```

每个 `async` 操作都包 `try { … } catch (e) { notifyError(e) }` 并带同步守卫（`busy` 标志，连点只发一次，与 `UserDetailView` 的 `ban.busy` 写法一致）。

- [ ] **Step 3: 运行**

Run: `cd web && npm test 2>&1 | tail -6 && npm run build 2>&1 | tail -3`
Expected: 全绿、构建成功。

- [ ] **Step 4: 提交**

```bash
git add web/admin/src web/shared/src/types/admin.ts
git commit -m "feat(admin): referral page (overview, held review, inviters with freeze, rewards with CSV, config and rescan), user detail block, dashboard card

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 17: e2e：钩子（`users` 的 `invite_code / device_id / ip`、`referral/config`）、Playwright 用例、覆盖表

**Files:**
- Create: `web/e2e/helpers/referral.ts`、`web/e2e/console/referral.spec.ts`、`web/e2e/admin/referral.spec.ts`
- Modify: `internal/e2ehooks/hooks.go`（`users`、新增 `referral/config`）、`internal/e2ehooks/hooks_test.go`、`web/e2e/helpers/hooks.ts`、`web/e2e/console/account.spec.ts`（追加 C-ACC-03）、`web/e2e/console/wallet.spec.ts`（追加 C-WAL-05）、`web/e2e/admin/finance.spec.ts`（追加 A-FIN-05）、`web/e2e/coverage.json`、`web/e2e/__tests__/coverage.spec.ts`（数量）、`web/playwright.config.ts`（顺序说明注释）

**Interfaces:**
- Consumes: Task 15 / 16 的 `data-testid`；Task 10 的 `/hooks/sweep` 已含 `Settle`。
- Produces:
  - `POST /hooks/users` 请求体新增 `invite_code?: string`、`device_id?: string`、`ip?: string`：三者任一非空时改走真实路径——`Identity.SendLoginCode(ctx, phone, ip)` + `Identity.LoginWith(ctx, LoginRequest{Phone, Code: "000000", InviteCode, IP, DeviceID, UserAgent: "e2e"})`（mock 验证码固定 `000000`；钩子先 `Clock.Advance(61s)` 绕过 60 秒限制），然后按 `verified` 直接 `UPDATE users SET kyc_status`（不写指纹——同身份用例走真实 `/kyc/personal`）、按 `balance / ledger_rows` 入账；响应加 `invite?: {bound, reason, inviter}`。
  - `POST /hooks/referral/config {rate_bp?, max_per_invitee?, max_per_inviter_30d?, hold_days?, period_days?}` → `{id}`：未给字段沿用 `CurrentConfig`，`effective_from = now`，`actor` 为 system。
  - `hooks.ts`：`users(opts: { phone?; verified?; balance?; ledger_rows?; invite_code?; device_id?; ip? })` 返回 `{ id, phone, invite? }`；`referralConfig(opts: { rate_bp?; max_per_invitee?; max_per_inviter_30d?; hold_days?; period_days? })`。
  - `helpers/referral.ts`：`inviterWithCode(opts?: { verified?: boolean; balance?: number }): Promise<{ id; phone; code; api }>`（`seedUser` + `userApi` + `GET /api/v1/referral/me`）；`inviteeOf(code: string, opts?: { verified?: boolean; balance?: number; device_id?: string }): Promise<{ id; phone }>`（钩子带码注册）；`rewardsOf(api): Promise<RewardItem[]>`；`DAY = 86_400`。
  - 覆盖表 ID（`coverage.json`）：`C-REF-01`～`08`、`C-ACC-03`、`C-WAL-05`、`A-REF-01`～`07`、`A-FIN-05`；`coverage.spec.ts`：console `103`、admin `77`、合计 `180`。

- [ ] **Step 1: 钩子**

`internal/e2ehooks/hooks.go` 的 `users`：

```go
	req, err := decode[struct {
		Phone      string `json:"phone"`
		Verified   bool   `json:"verified"`
		Balance    int64  `json:"balance"`
		LedgerRows int    `json:"ledger_rows"`
		InviteCode string `json:"invite_code"`
		DeviceID   string `json:"device_id"`
		IP         string `json:"ip"`
	}](body)
	…
	var id int64
	var invite any
	if req.InviteCode != "" || req.DeviceID != "" || req.IP != "" {
		ip := req.IP
		if ip == "" {
			ip = "127.0.0.1"
		}
		s.Clock.Advance(61 * time.Second) // 同一手机号 60 秒只能发一次验证码
		if err := s.App.Identity.SendLoginCode(ctx, req.Phone, ip); err != nil {
			return nil, err
		}
		_, res, err := s.App.Identity.LoginWith(ctx, identity.LoginRequest{Phone: req.Phone, Code: "000000", InviteCode: req.InviteCode, IP: ip, DeviceID: req.DeviceID, UserAgent: "e2e"})
		if err != nil {
			return nil, err
		}
		id, invite = res.ID, res.Invite
		if _, err := s.Pool.Exec(ctx, `UPDATE users SET kind = 'personal', kyc_status = $2 WHERE id = $1`, id, kyc); err != nil {
			return nil, err
		}
	} else {
		if err := s.Pool.QueryRow(ctx, `INSERT INTO users (phone, kind, kyc_status, created_at, updated_at)
			VALUES ($1, 'personal', $2, $3, $3) RETURNING id`, req.Phone, kyc, now).Scan(&id); err != nil {
			return nil, err
		}
	}
	…（余额入账不变）
	return map[string]any{"id": id, "phone": req.Phone, "invite": invite}, nil
```

新增：

```go
	mux.HandleFunc("POST /hooks/referral/config", s.handle(s.referralConfig))

// referralConfig 插入一版费率（effective_from = now），未给的字段沿用当前版本。
func (s *Server) referralConfig(ctx context.Context, body json.RawMessage) (any, error) {
	req, err := decode[struct {
		RateBP           *int   `json:"rate_bp"`
		MaxPerInvitee    *int64 `json:"max_per_invitee"`
		MaxPerInviter30d *int64 `json:"max_per_inviter_30d"`
		HoldDays         *int   `json:"hold_days"`
		PeriodDays       *int   `json:"period_days"`
	}](body)
	if err != nil {
		return nil, err
	}
	cur, err := s.App.Referral.CurrentConfig(ctx)
	if err != nil {
		return nil, err
	}
	in := referral.RateInput{RateBP: cur.RateBP, MaxPerInvitee: cur.MaxPerInvitee, MaxPerInviter30d: cur.MaxPerInviter30d, HoldDays: cur.HoldDays, PeriodDays: cur.PeriodDays, Note: "e2e"}
	if req.RateBP != nil {
		in.RateBP = *req.RateBP
	}
	if req.MaxPerInvitee != nil {
		in.MaxPerInvitee = *req.MaxPerInvitee
	}
	if req.MaxPerInviter30d != nil {
		in.MaxPerInviter30d = *req.MaxPerInviter30d
	}
	if req.HoldDays != nil {
		in.HoldDays = *req.HoldDays
	}
	if req.PeriodDays != nil {
		in.PeriodDays = *req.PeriodDays
	}
	v, err := s.App.Referral.AddRateVersion(ctx, audit.Actor{}, in)
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": v.ID}, nil
}
```

`hooks_test.go` 追加：带 `invite_code` 的 `users` 调用后 `referral_bindings` 有一行且响应 `invite.bound == true`；`referral/config {hold_days: 0}` 后 `CurrentConfig().HoldDays == 0`。

`web/e2e/helpers/hooks.ts`：

```ts
  users(opts: { phone?: string; verified?: boolean; balance?: number; ledger_rows?: number; invite_code?: string; device_id?: string; ip?: string } = {}) {
    return post<{ id: number; phone: string; invite?: { bound: boolean; reason: string; inviter?: string } }>('/hooks/users', opts)
  },
  /** 插一版费率（生效时间 = 现在），未给的字段沿用当前版本。 */
  referralConfig(opts: { rate_bp?: number; max_per_invitee?: number; max_per_inviter_30d?: number; hold_days?: number; period_days?: number }) {
    return post<{ id: number }>('/hooks/referral/config', opts)
  },
```

`web/e2e/helpers/referral.ts`：

```ts
import type { APIRequestContext } from '@playwright/test'
import { seedUser, userApi } from './console'
import { hooks } from './hooks'
import { uniquePhone } from './phone'

export const DAY = 86_400

export interface RewardItem { order_id: string; invitee_masked: string; reward: number; status: string; clawed_amount: number; refunded_amount: number }

/** 造一个邀请人（缺省已实名、余额 ¥1,000）并取其邀请码与 API 会话。 */
export async function inviterWithCode(opts: { verified?: boolean; balance?: number } = {}) {
  const u = await seedUser({ verified: opts.verified ?? true, balance: opts.balance ?? 100_000 })
  const api = await userApi(u.phone)
  const me = (await (await api.get('/api/v1/referral/me')).json()) as { code: string; link: string }
  return { ...u, code: me.code, link: me.link, api }
}

/** 用邀请码走真实注册路径造一个被邀请人（缺省已实名、余额 ¥1,000）。 */
export async function inviteeOf(code: string, opts: { verified?: boolean; balance?: number; device_id?: string; ip?: string } = {}) {
  const phone = uniquePhone()
  const u = await hooks.users({ phone, verified: opts.verified ?? true, balance: opts.balance ?? 100_000, invite_code: code, device_id: opts.device_id ?? crypto.randomUUID(), ip: opts.ip })
  return { id: u.id, phone, invite: u.invite }
}

export async function rewardsOf(api: APIRequestContext): Promise<RewardItem[]> {
  const r = await api.get('/api/v1/referral/rewards')
  if (!r.ok()) throw new Error(`rewards ${r.status()} ${await r.text()}`)
  return ((await r.json()) as { items: RewardItem[] }).items
}
```

- [ ] **Step 2: 控制台用例 `web/e2e/console/referral.spec.ts`**

```ts
import { expect, test } from '@playwright/test'
import { loginConsole, nav, placeOrder, seedUser, toast, userApi } from '../helpers/console'
import { hooks } from '../helpers/hooks'
import { yuan } from '../helpers/money'
import { uniquePhone } from '../helpers/phone'
import { DAY, inviteeOf, inviterWithCode, rewardsOf } from '../helpers/referral'

test('C-REF-01 带 ?ic= 落地：地址栏参数被去掉、登录页显示邀请人、新号注册提示已绑定、邀请人页出现被邀请人', async ({ page }, info) => {
  const inv = await inviterWithCode()
  await page.goto(`/?ic=${inv.code.toLowerCase()}`)
  await expect(page).toHaveURL(/\/market$/) // 参数已移除
  const phone = uniquePhone()
  await hooks.advance(61)
  await page.goto('/login')
  await expect(page.getByTestId('login-invite')).toContainText(`${inv.phone.slice(0, 3)}****${inv.phone.slice(7)}`)
  await page.getByTestId('login-phone').fill(phone)
  await page.getByTestId('login-send').click()
  await page.getByTestId('login-code').fill('000000')
  await page.getByTestId('login-submit').click()
  await toast(page, '已绑定邀请关系')
  await expect(page).toHaveURL(/\/market/)
  // 再回登录页：存储已清，不再显示邀请人
  await page.context().clearCookies()
  await page.goto('/login')
  await expect(page.getByTestId('login-invite')).toHaveCount(0)
  // 邀请人页
  await loginConsole(page, inv.phone)
  await page.goto('/invite')
  await expect(page.getByTestId('invite-stat-invitees')).toHaveText('1')
  await expect(page.getByTestId('invitee-phone').first()).toHaveText(`${phone.slice(0, 3)}****${phone.slice(7)}`)
  await expect(page.getByTestId('invitee-kyc').first()).toHaveText('未实名')
  await expect(page.getByTestId('invitee-status').first()).toHaveText('有效')
  void info
})

test('C-REF-02 无效码静默：落地不存、登录页无提示、注册成功无 toast', async ({ page }) => {
  await page.goto('/?ic=ZZZZZZZZ')
  await expect(page).toHaveURL(/\/market$/)
  await page.goto('/login')
  await expect(page.getByTestId('login-invite')).toHaveCount(0)
  const phone = uniquePhone()
  await hooks.advance(61)
  await page.getByTestId('login-phone').fill(phone)
  await page.getByTestId('login-send').click()
  await page.getByTestId('login-code').fill('000000')
  await page.getByTestId('login-submit').click()
  await expect(page).toHaveURL(/\/market/)
  await expect(page.locator('.el-message')).toHaveCount(0)
})

test('C-REF-03 老用户带码登录不绑定、不提示；清除按钮去掉邀请', async ({ page }) => {
  const inv = await inviterWithCode()
  const old = await seedUser()
  await page.goto(`/?ic=${inv.code}`)
  await hooks.advance(61)
  await page.goto('/login')
  await expect(page.getByTestId('login-invite')).toBeVisible()
  await page.getByTestId('login-phone').fill(old.phone)
  await page.getByTestId('login-send').click()
  await page.getByTestId('login-code').fill('000000')
  await page.getByTestId('login-submit').click()
  await expect(page).toHaveURL(/\/market/)
  await expect(page.locator('.el-message')).toHaveCount(0)
  const invitees = (await (await inv.api.get('/api/v1/referral/invitees')).json()) as { total: number }
  expect(invitees.total).toBe(0)
  // 清除按钮
  await page.context().clearCookies()
  await page.goto(`/?ic=${inv.code}`)
  await page.goto('/login')
  await page.getByTestId('login-invite-clear').click()
  await expect(page.getByTestId('login-invite')).toHaveCount(0)
  await page.reload()
  await expect(page.getByTestId('login-invite')).toHaveCount(0)
})

test('C-REF-04 邀请页：码、链接、二维码、规则、复制', async ({ page }, info) => {
  const inv = await inviterWithCode()
  await loginConsole(page, inv.phone)
  await nav(page, info, 'account')
  await page.getByTestId('account-invite').click()
  await expect(page).toHaveURL(/\/invite$/)
  await expect(page.getByTestId('invite-code')).toHaveText(inv.code)
  await expect(page.getByTestId('invite-link')).toHaveText(`http://spark.test:18080/?ic=${inv.code}`)
  await expect(page.getByTestId('invite-qr')).toBeVisible()
  await expect(page.getByTestId('invite-rules')).toContainText('10%')
  await expect(page.getByTestId('invite-rules')).toContainText('不可提现')
  await page.getByTestId('invite-copy-link').click()
  await toast(page, '已复制')
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(`http://spark.test:18080/?ic=${inv.code}`)
  await expect(page.getByTestId('invite-kyc-alert')).toHaveCount(0)
})

test('C-REF-05 未实名邀请人：提示条与去实名链接；码仍可见', async ({ page }) => {
  const inv = await inviterWithCode({ verified: false })
  await loginConsole(page, inv.phone)
  await page.goto('/invite')
  await expect(page.getByTestId('invite-kyc-alert')).toContainText('实名认证后才能收到返利')
  await page.getByTestId('invite-kyc-alert').getByRole('link').click()
  await expect(page).toHaveURL(/\/kyc$/)
})

test('C-REF-06 被邀请人列表：脱敏、状态、实名、贡献、截止日；分页', async ({ page }) => {
  const inv = await inviterWithCode()
  for (let i = 0; i < 21; i++) await inviteeOf(inv.code, { verified: i % 2 === 0 })
  await loginConsole(page, inv.phone)
  await page.goto('/invite')
  await expect(page.getByTestId('invite-stat-invitees')).toHaveText('21')
  await expect(page.getByTestId('invite-stat-verified')).toHaveText('11')
  await expect(page.getByTestId('invitee-row')).toHaveCount(20)
  await expect(page.getByTestId('invitee-phone').first()).toHaveText(/^139\*\*\*\*\d{4}$/)
  await page.getByTestId('invite-invitees-pager').getByRole('button', { name: '2' }).click()
  await expect(page.getByTestId('invitee-row')).toHaveCount(1)
})

test('C-REF-07 返利明细：待发放 → 推进 7 天 + 清扫 → 已发放，钱包流水「邀请返利」', async ({ page }, info) => {
  const inv = await inviterWithCode({ balance: 0 })
  const b = await inviteeOf(inv.code, { balance: 100_000 })
  const bApi = await userApi(b.phone)
  await hooks.machines(1)
  const instance = await placeOrder(bApi)
  await hooks.sweep()
  await loginConsole(page, inv.phone)
  await page.goto('/invite?tab=rewards')
  await page.getByTestId('invite-tab-rewards').click()
  await expect(page.getByTestId('reward-row')).toHaveCount(1)
  await expect(page.getByTestId('reward-amount')).toHaveText(yuan(690))
  await expect(page.getByTestId('reward-status')).toHaveText('待发放')
  await expect(page.getByTestId('invite-stat-pending')).toHaveText(yuan(690))
  await hooks.advance(7 * DAY)
  await hooks.sweep()
  await page.reload()
  await page.getByTestId('invite-tab-rewards').click()
  await expect(page.getByTestId('reward-status')).toHaveText('已发放')
  await expect(page.getByTestId('invite-stat-settled')).toHaveText(yuan(690))
  await nav(page, info, 'wallet')
  await expect(page.getByTestId('wallet-balance')).toHaveText(yuan(690))
  await expect(page.getByTestId('ledger-kind').first()).toHaveText('邀请返利')
  await expect(page.getByTestId('ledger-remark').first()).toContainText(`邀请返利 ${b.phone.slice(0, 3)}****${b.phone.slice(7)} 订单 ord-`)
  void instance
})

test('C-REF-08 后台退款后：明细显示已扣回、钱包流水「返利扣回」', async ({ page }, info) => {
  // 退款要在实例仍运行时做（refund-release 只接受 creating / running / stopped），所以这里把发放延迟临时改为 0 天，
  // 让计提与发放在同一轮清扫完成；用例结束时恢复 7 天（费率配置是全局的，不恢复会影响后面的用例）。
  await hooks.referralConfig({ hold_days: 0 })
  try {
    const inv = await inviterWithCode({ balance: 0 })
    const b = await inviteeOf(inv.code, { balance: 100_000 })
    const bApi = await userApi(b.phone)
    await hooks.machines(1)
    const instance = await placeOrder(bApi)
    await hooks.sweep() // 计提 + 发放
    await hooks.agent() // 实例进入运行中
    const { createAdmin, adminApi, adminPost } = await import('../helpers/admin')
    const fin = await createAdmin('finance')
    const fapi = await adminApi(fin)
    await adminPost(fapi, fin, `/admin/api/v1/instances/${instance}/refund-release`, { amount: 3000, reason: 'e2e' })
    await hooks.sweep()
    const rewards = await rewardsOf(inv.api)
    expect(rewards[0].clawed_amount).toBe(300)
    await loginConsole(page, inv.phone)
    await page.goto('/invite?tab=rewards')
    await page.getByTestId('invite-tab-rewards').click()
    await expect(page.getByTestId('reward-amount')).toHaveText(yuan(390))
    await expect(page.getByTestId('reward-clawed')).toContainText(yuan(300))
    await nav(page, info, 'wallet')
    await expect(page.getByTestId('wallet-balance')).toHaveText(yuan(390))
    await expect(page.getByTestId('ledger-kind').first()).toHaveText('返利扣回')
    await expect(page.getByTestId('ledger-amount').first()).toHaveText(`-${yuan(300)}`)
  } finally {
    await hooks.referralConfig({ hold_days: 7 })
  }
})
```

`account.spec.ts` 追加：

```ts
test('C-ACC-03 我的页「邀请返利」入口与本月金额', async ({ page }, info) => {
  const inv = await inviterWithCode({ balance: 0 })
  const b = await inviteeOf(inv.code, { balance: 100_000 })
  await hooks.machines(1)
  await placeOrder(await userApi(b.phone))
  await hooks.sweep()
  await loginConsole(page, inv.phone)
  await nav(page, info, 'account')
  await expect(page.getByTestId('account-invite')).toBeVisible()
  await expect(page.getByTestId('account-invite-month')).toHaveCount(0) // 未发放
  await hooks.advance(7 * DAY)
  await hooks.sweep()
  await page.reload()
  await expect(page.getByTestId('account-invite-month')).toHaveText(`本月返利 ${yuan(690)}`)
})
```

`wallet.spec.ts` 追加：

```ts
test('C-WAL-05 流水显示邀请返利 / 返利扣回标签与颜色', async ({ page }, info) => {
  await hooks.referralConfig({ hold_days: 0 }) // 同 C-REF-08：退款要在实例运行时做
  try {
    const inv = await inviterWithCode({ balance: 0 })
    const b = await inviteeOf(inv.code, { balance: 100_000 })
    const bApi = await userApi(b.phone)
    await hooks.machines(1)
    const instance = await placeOrder(bApi)
    await hooks.sweep()
    await hooks.agent()
    const fin = await createAdmin('finance')
    await adminPost(await adminApi(fin), fin, `/admin/api/v1/instances/${instance}/refund-release`, { amount: 6000, reason: 'e2e' })
    await hooks.sweep()
    await loginConsole(page, inv.phone)
    await nav(page, info, 'wallet')
    await expect(page.getByTestId('ledger-kind').nth(0)).toHaveText('返利扣回')
    await expect(page.getByTestId('ledger-kind').nth(0)).toHaveClass(/kind-clawback/)
    await expect(page.getByTestId('ledger-amount').nth(0)).toHaveText(`-${yuan(600)}`)
    await expect(page.getByTestId('ledger-kind').nth(1)).toHaveText('邀请返利')
    await expect(page.getByTestId('ledger-kind').nth(1)).toHaveClass(/kind-rebate/)
  } finally {
    await hooks.referralConfig({ hold_days: 7 })
  }
})
```

- [ ] **Step 3: 后台用例 `web/e2e/admin/referral.spec.ts`**

```ts
import { expect, test, type Page } from '@playwright/test'
import { adminApi, adminPost, createAdmin, enterTotpDialog, expectAdminUrl, getJSON, loginAdmin, logout, pick } from '../helpers/admin'
import { placeOrder, userApi } from '../helpers/console'
import { hooks } from '../helpers/hooks'
import { yuan } from '../helpers/money'
import { DAY, inviteeOf, inviterWithCode } from '../helpers/referral'

const yuan2 = (cents: number) => `${Math.floor(cents / 100)}.${String(cents % 100).padStart(2, '0')}`

async function seedReward(opts: { settle?: boolean } = {}) {
  const inv = await inviterWithCode({ balance: 0 })
  const b = await inviteeOf(inv.code, { balance: 100_000 })
  await hooks.machines(1)
  const instance = await placeOrder(await userApi(b.phone))
  await hooks.sweep()
  if (opts.settle) {
    await hooks.advance(7 * DAY)
    await hooks.sweep()
  }
  return { inv, b, instance }
}

test('A-REF-01 概览数字：待发放、搁置数随新数据增加', async ({ page }) => {
  const fin = await createAdmin('finance')
  const before = await getJSON<{ pending: number; held_count: number }>(await adminApi(fin), '/admin/api/v1/referral/summary')
  const { inv } = await seedReward()
  // 同设备搁置：先让邀请人用设备 D 登录一次（钩子对已存在的手机号走 LoginWith，不会重复建用户），再用同一设备带码注册
  const D = crypto.randomUUID()
  await hooks.users({ phone: inv.phone, device_id: D })
  const held = await inviteeOf(inv.code, { device_id: D })
  expect(held.invite?.bound).toBe(true)
  await loginAdmin(page, fin)
  await page.goto('/admin/referral')
  await expect(page.getByTestId('ref-sum-pending')).toHaveText(yuan(before.pending + 690))
  await expect(page.getByTestId('ref-sum-held')).toHaveText(String(before.held_count + 1))
})
```

```ts
test('A-REF-02 待审核：通过与撤销（撤销要动态码、提示作废金额）', async ({ page }) => {
  const inv = await inviterWithCode({ balance: 0 })
  const D = crypto.randomUUID()
  await hooks.users({ phone: inv.phone, device_id: D })
  const held1 = await inviteeOf(inv.code, { device_id: D, balance: 100_000 })
  const held2 = await inviteeOf(inv.code, { device_id: D, balance: 100_000 })
  await hooks.machines(1)
  await placeOrder(await userApi(held2.phone))
  await hooks.sweep()
  const fin = await createAdmin('finance')
  await loginAdmin(page, fin)
  await page.goto('/admin/referral?tab=held')
  const row1 = page.getByTestId('held-row').filter({ has: page.getByTestId('held-invitee').filter({ hasText: String(held1.id) }) })
  await expect(row1.getByTestId('held-flags')).toContainText('同设备')
  await row1.getByTestId('held-approve').click()
  await page.getByTestId('held-approve-note').fill('家人')
  await page.getByTestId('held-approve-ok').click()
  await expect(page.locator('.el-message').filter({ hasText: '已通过' })).toBeVisible()
  await expect(row1).toHaveCount(0)
  const row2 = page.getByTestId('held-row').filter({ has: page.getByTestId('held-invitee').filter({ hasText: String(held2.id) }) })
  await expect(row2.getByTestId('held-pending')).toHaveText(yuan(690))
  await row2.getByTestId('held-revoke').click()
  await page.getByTestId('held-revoke-ok').click() // 原因必填
  await expect(page.getByTestId('held-revoke-dialog')).toBeVisible()
  await page.getByTestId('held-revoke-reason').fill('刷单')
  await page.getByTestId('held-revoke-ok').click()
  await enterTotpDialog(page, fin.totp_secret!)
  await expect(page.locator('.el-message').filter({ hasText: `已撤销，作废待发放 ${yuan(690)}` })).toBeVisible()
  await expect(row2).toHaveCount(0)
})

test('A-REF-03 邀请人：搜索、详情抽屉、冻结后新注册不绑定且到期不发放、解冻后发放', async ({ page }) => {
  const { inv } = await seedReward()
  const fin = await createAdmin('finance')
  await loginAdmin(page, fin)
  await page.goto('/admin/referral?tab=inviters')
  await page.getByTestId('inv-q').fill(String(inv.id))
  await page.getByTestId('inv-q').press('Enter')
  const row = page.getByTestId('inv-row').filter({ has: page.getByTestId('inv-user').filter({ hasText: String(inv.id) }) })
  await expect(row.getByTestId('inv-code')).toHaveText(inv.code)
  await expect(row.getByTestId('inv-pending')).toHaveText(yuan(690))
  await row.click()
  await expect(page.getByTestId('inv-drawer')).toBeVisible()
  await expect(page.getByTestId('inv-binding-row')).toHaveCount(1)
  await expect(page.getByTestId('inv-reward-row')).toHaveCount(1)
  await page.getByTestId('inv-freeze').click()
  await page.getByTestId('inv-freeze-reason').fill('可疑')
  await page.getByTestId('inv-freeze-ok').click()
  await expect(page.locator('.el-message').filter({ hasText: '已冻结' })).toBeVisible()
  const blocked = await inviteeOf(inv.code)
  expect(blocked.invite?.bound).toBe(false)
  expect(blocked.invite?.reason).toBe('inviter_frozen')
  await hooks.advance(7 * DAY)
  await hooks.sweep()
  await expect(row.getByTestId('inv-pending')).toHaveText(yuan(690)) // 未发放
  await page.getByTestId('inv-unfreeze').click()
  await expect(page.locator('.el-message').filter({ hasText: '已解冻' })).toBeVisible()
  await hooks.sweep()
  await page.reload()
  await page.getByTestId('inv-q').fill(String(inv.id))
  await page.getByTestId('inv-q').press('Enter')
  await expect(row.getByTestId('inv-net')).toHaveText(yuan(690))
})

test('A-REF-04 配置：新增版本（super + 动态码）、追溯生效被拒、finance 看不到配置 Tab', async ({ page }) => {
  const sup = await createAdmin('super')
  await loginAdmin(page, sup)
  await page.goto('/admin/referral?tab=config')
  await expect(page.getByTestId('cfg-current-rate')).toBeVisible()
  await page.getByTestId('cfg-rate').fill('15')
  await page.getByTestId('cfg-invitee').fill('500')
  await page.getByTestId('cfg-inviter').fill('5000')
  await page.getByTestId('cfg-hold').fill('7')
  await page.getByTestId('cfg-period').fill('365')
  await page.getByTestId('cfg-note').fill('e2e')
  await page.getByTestId('cfg-submit').click()
  await enterTotpDialog(page, sup.totp_secret!)
  await expect(page.locator('.el-message').filter({ hasText: '已新增版本' })).toBeVisible()
  await expect(page.getByTestId('cfg-current-rate')).toHaveText('15%')
  // 追溯生效
  const yesterday = new Date((await hooks.clock()).getTime() - DAY * 1000)
  await page.getByTestId('cfg-effective').fill(`${yesterday.getFullYear()}-${String(yesterday.getMonth() + 1).padStart(2, '0')}-${String(yesterday.getDate()).padStart(2, '0')} 00:00:00`)
  await page.getByTestId('cfg-submit').click()
  await expect(page.locator('.el-message').filter({ hasText: '生效时间不能早于当前时间' })).toBeVisible()
  await logout(page)
  await loginAdmin(page, await createAdmin('finance'))
  await page.goto('/admin/referral?tab=config')
  await expect(page.getByTestId('ref-tab-config')).toHaveCount(0)
})

test('A-REF-05 返利明细筛选与 CSV 下载（中文表头、两位小数）', async ({ page }) => {
  const { inv, b } = await seedReward({ settle: true })
  const fin = await createAdmin('finance')
  await loginAdmin(page, fin)
  await page.goto('/admin/referral?tab=rewards')
  await page.getByTestId('rw-inviter').fill(String(inv.id))
  await pick(page, 'rw-status', '已发放')
  await page.getByTestId('rw-row').first().waitFor()
  await expect(page.getByTestId('rw-invitee-id').first()).toHaveText(String(b.id))
  const dl = page.waitForEvent('download')
  await page.getByTestId('rw-export').click()
  const file = await dl
  expect(file.suggestedFilename()).toMatch(/^referral-rewards-\d{8}-\d{6}\.csv$/)
  const text = (await (await import('node:fs')).promises.readFile(await file.path(), 'utf8')).replace(/^﻿/, '')
  expect(text.split('\n')[0]).toBe('订单号,邀请人ID,被邀请人ID,消费(元),退款(元),费率(%),应得(元),状态,发放时间,已扣回(元),费率版本')
  expect(text).toContain(`,${inv.id},${b.id},${yuan2(6900)},0.00,10.00,${yuan2(690)},settled,`)
})

test('A-REF-06 用户详情邀请区块：被邀请人显示邀请人，邀请人显示统计与链接', async ({ page }) => {
  const { inv, b } = await seedReward({ settle: true })
  const fin = await createAdmin('finance')
  await loginAdmin(page, fin)
  await page.goto(`/admin/users/${b.id}`)
  await expect(page.getByTestId('user-referral-inviter')).toContainText(String(inv.id))
  await page.goto(`/admin/users/${inv.id}`)
  await expect(page.getByTestId('user-referral-code')).toHaveText(inv.code)
  await expect(page.getByTestId('user-referral-net')).toHaveText(yuan(690))
  await page.getByTestId('user-referral-link').click()
  await expectAdminUrl(page, '/referral', { tab: 'inviters', q: String(inv.id) })
})

test('A-REF-07 ops 看不到菜单，直接访问 403，接口 403', async ({ page }) => {
  const ops = await createAdmin('ops')
  await loginAdmin(page, ops)
  await expect(page.getByTestId('menu-referral')).toHaveCount(0)
  await page.goto('/admin/referral')
  await expectAdminUrl(page, '/403', { from: '/referral' })
  const r = await (await adminApi(ops)).get('/admin/api/v1/referral/summary')
  expect(r.status()).toBe(403)
})
```

`finance.spec.ts` 追加：

```ts
test('A-FIN-05 流水页筛选「邀请返利」', async ({ page }) => {
  const { inv } = await seedReward({ settle: true })
  await loginAdmin(page, await createAdmin('finance'))
  await page.goto('/admin/ledger')
  await page.getByTestId('fin-filter-user').fill(String(inv.id))
  await pick(page, 'fin-filter-kind', '邀请返利')
  await page.getByTestId('fin-search').click()
  await expect(rows(page, 'led-id')).toHaveCount(1)
  await expect(page.getByTestId('led-kind').first()).toHaveText('邀请返利')
  await expect(page.getByTestId('led-amount').first()).toHaveText(`+${yuan(690)}`)
})
```

（`seedReward` 抽到 `helpers/referral.ts` 供三个文件共用；`fin-filter-kind` / `fin-search` 以 `FinancePage.vue` 的实际 testid 为准；侧栏菜单 testid 以 `web/admin` 布局现有命名 `menu-<key>` 为准。）

- [ ] **Step 4: 覆盖表与配置注释**

`coverage.json`：console 加 `"C-REF-01", … "C-REF-08"`、`"C-ACC-03"`、`"C-WAL-05"`；admin 加 `"A-REF-01", … "A-REF-07"`、`"A-FIN-05"`。`coverage.spec.ts`：`103` / `77` / `180`，注释加「+ 计划 8 的 C-REF-01～08、C-ACC-03、C-WAL-05 / A-REF-01～07、A-FIN-05」。`playwright.config.ts` 注释追加一段：「计划 8 的返利用例（C-REF-07 / 08、C-ACC-03、C-WAL-05、A-REF-01～06、A-FIN-05）每个推进 7 天让返利到期，都只用自己新造的邀请人 / 被邀请人与新登记的机器；A-REF-03 冻结 / 解冻只作用于自己的邀请人。」

- [ ] **Step 5: 运行**

Run: `cd web && npm run e2e:coverage && npx playwright test e2e/console/referral.spec.ts e2e/console/account.spec.ts e2e/console/wallet.spec.ts e2e/admin/referral.spec.ts e2e/admin/finance.spec.ts 2>&1 | tail -15`
Expected: 覆盖表核对通过；列出的 spec 在三个 project 下全部通过。再跑完整 `make e2e` 确认原有用例仍绿。

- [ ] **Step 6: 提交**

```bash
git add internal/e2ehooks web/e2e web/playwright.config.ts
git commit -m "test(e2e): referral hooks and Playwright cases (C-REF-01~08, C-ACC-03, C-WAL-05, A-REF-01~07, A-FIN-05), coverage table

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 18: 开发平台验收（规格 §16.4 全部 12 项）与 `acceptance-plan8.md`

**Files:**
- Create: `docs/superpowers/acceptance-plan8.md`
- 可能修改：验收中发现的缺陷在对应包修复并补单测（单独提交）

**Interfaces:**
- Consumes: 全部任务。环境：开发 Mac 192.168.1.218，`make devdb && make web && make run`（`SPARK_ENV=dev`、`SPARK_SWEEP_INTERVAL=1m`、`SPARK_PUBLIC_BASE_URL=http://localhost:8080`，mock 支付 / 短信 / 实名）；后台 `http://127.0.0.1:8081/admin/`，管理员用 `platform admin create` 造 `super` 与 `finance` 各一个；实例用 `cmd/e2e-server` 的 fake Agent 方式不可用（这是 `make run`），所以用开发环境已接入的 DGX Spark（192.168.1.221，计划 6 验收的那台）跑真实实例——只需要 1 台；需要 root 的命令按执行须知由协调人执行。
- Produces: `acceptance-plan8.md`（格式同 `acceptance-plan6.md`：环境、逐项命令 / 关键输出 / 结论、缺陷表、观察）。

- [ ] **Step 1: 分享（§16.4 第 1 项）**

Chrome 打开 `http://localhost:8080` → 手机号 A 登录（验证码 `000000`）→ 实名（mock 通过）→ 我的 → 邀请返利：8 位码、链接 `http://localhost:8080/?ic=<码>`、二维码可见；复制链接。记录码。

- [ ] **Step 2: 注册绑定（第 2 项）**

Chrome 隐私窗口打开链接 → 地址栏参数消失 → 登录页显示「通过 1xx****xxxx 的邀请注册」→ 新手机号 B 注册 → toast「已绑定邀请关系」→ A 的邀请页「被邀请人」出现 B（未实名、有效）。

- [ ] **Step 3: 老用户不绑定（第 3 项）**

另开隐私窗口用链接 → 用已有账号 C 登录 → 无提示；A 的页面无 C；`SELECT count(*) FROM referral_bindings WHERE invitee_id = <C>` = 0。

- [ ] **Step 4: 计提（第 4 项）**

B 实名 → mock 充值 ¥200 → 市场页租 1 天（¥69，真机）→ 等 ≤ 1 分钟 → A 的返利明细 ¥6.90 待发放；`SELECT settle_after FROM referral_rewards` = 扣款 + 7 天；后台 `/referral` 概览待发放 ¥6.90。

- [ ] **Step 5: 发放（第 5 项）**

开发库 `UPDATE referral_rewards SET settle_after = now()` → 下一轮清扫 → A 钱包 +¥6.90，流水「邀请返利 1xx****xxxx 订单 ord-…」；后台流水页筛「邀请返利」可见。

- [ ] **Step 6: 扣回（第 6 项）**

后台 finance 对 B 的实例「退款并释放」退 ¥30（动态码）→ 下一轮清扫 → A 钱包 −¥3.00「返利扣回」；明细应得 ¥3.90、已扣回 ¥3.00。

- [ ] **Step 7: 欠账（第 7 项）**

A 先把余额用掉（租 1 天；余额不够则 mock 充值到刚好 ¥69 再租）→ B 再租 1 天 → 计提 → `UPDATE … settle_after = now()` → 发放 → 后台对该实例全退 ¥69 → 清扫 → A 余额不够扣 → 后台邀请人详情欠账 ¥6.90（或余额剩余部分之差）→ B 再租 1 天 → 发放：欠账抵销、A 实际入账 ¥0 且欠账清零（明细 settled ¥6.90、钱包无入账）。

- [ ] **Step 8: 自刷（第 8 项）**

A 的同一个 Chrome 窗口（同 `localStorage.sc_device`）退出后用新号 D 带 A 的码注册 → 后台待审核出现 D（标记「同设备」）；D 实名、充值、租 1 天 → 到期（改 `settle_after`）不发放 → 后台「通过」→ 下一轮发放。新号 E 用 B 的身份证号实名 → E 的绑定自动撤销（后台绑定列表状态「已失效」、标记「同身份」、审计 `referral.auto_revoke`），待发放作废。

- [ ] **Step 9: 冻结（第 9 项）**

后台冻结 A → 新号 F 带码注册：登录页仍显示邀请人？（`codes/{code}` 返回 `valid=false` → 不显示；若页面已缓存则提交后 toast「邀请码无效」）且注册成功；A 的待发放到期不发放；解冻后下一轮发放。

- [ ] **Step 10: 配置（第 10 项）**

super 新增版本 15%、生效时间 now（动态码）→ 之后 B 的消费按 15%（¥10.35）；之前的明细不变；`effective_from` 填昨天 → 「生效时间不能早于当前时间」。

- [ ] **Step 11: 导出（第 11 项）**

后台返利明细与绑定 CSV 下载 → Excel / Numbers 打开中文不乱码，金额两位小数。

- [ ] **Step 12: 不变量（第 12 项）**

`SPARK_DATABASE_URL=postgres://postgres:postgres@localhost:5433/spark?sslmode=disable go test ./internal/referral -run TestInvariantsAgainstDevDB -v`——在 `internal/referral/invariants_test.go` 加该用例：`SPARK_DATABASE_URL` 为空时 `t.Skip`；非空时 `db.Open` 后 `referral.NewService(referral.Deps{Pool, Clock: clock.Real{}}).CheckInvariants(ctx, pool)`（只读）必须返回 nil。注意 `CheckInvariants` 的 `inv4` 在真实环境下要求没有正在处理中的流水：执行前等一轮清扫。

- [ ] **Step 13: 记录与提交**

`acceptance-plan8.md`：每项命令 / 关键输出 / 结论、缺陷表（编号、现象、原因、修复提交）、观察（列入遗留）。核对记录里没有任何密码。

```bash
git add docs/superpowers/acceptance-plan8.md internal/referral/invariants_test.go
git commit -m "docs: plan 8 dev-platform acceptance record (spec §16.4, 12 items)

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 19: 收尾：`followups-plan8.md`、README、`make check` 全量

**Files:**
- Create: `docs/superpowers/followups-plan8.md`
- Modify: `README.md`（子命令、部署注意、API 约定一行）、`docs/superpowers/followups-plan4.md`（若有「用户详情显示累计返利」类条目则勾选）

**Interfaces:**
- Consumes: 全部任务（含 Task 18 的缺陷修复与裁定）。
- Produces: 遗留清单与上线前核对项；`make check` 全绿是本计划的完成判据。

- [ ] **Step 1: README**

「本地开发 → 管理后台」小节 `platform admin reset-totp` 那句之后加：

```markdown
计划 8（邀请返利）上线后执行一次 `platform referral backfill-fingerprints`：为存量已实名用户计算同身份指纹（用 `SPARK_SECRET_KEY` 解密身份证号），之后「同一身份证 / 营业执照互相邀请」的判定才对老用户生效。回填不会撤销任何既有绑定。
```

「部署注意」末尾加：

```markdown
- 计划 8（邀请返利）发布：迁移 `00020` 在存在 `referral_rebate / referral_clawback` 流水时拒绝回滚；多副本滚动期间旧副本的登录接口不认 `invite_code`，窗口内注册的用户不会绑定（窗口很短，可接受）；发布后执行 `platform referral backfill-fingerprints`；`SPARK_PUBLIC_BASE_URL` 必须是用户可访问的控制台地址（邀请链接据此生成）。
```

「API 约定」加一行：`POST /api/v1/auth/login` 可带 `invite_code`，请求头 `X-Device-Id`（控制台生成的 UUID）用于风险标记。

- [ ] **Step 2: `followups-plan8.md`**

```markdown
# 计划 8 遗留事项

来源：计划 8 设计（2026-10-20）§15、§17 与实施 / 验收过程。均不阻塞合并。

## 上线前需要核对
- 执行 `platform referral backfill-fingerprints`（存量实名用户的同身份指纹）。
- 法务：帮助页与邀请页的规则文案（不可提现、平台有权撤销异常返利）。
- 财务：线下退款核算口径（R4：按「充值净额 − 消费」核算，返利部分不退；后台用户详情「邀请」区块显示累计发放净额）。
- 运营：首版费率 10% / ¥500 / ¥5,000 / 7 天 / 12 个月是否调整（后台「邀请返利 → 配置」可改）；是否需要公告。
- `SPARK_PUBLIC_BASE_URL` 必须是用户可访问的控制台地址（邀请链接据此生成）。
- 发布时多副本滚动窗口内注册的用户可能没绑定（旧副本不认 `invite_code`）；一次性重启全部副本可消除该窗口。

## 规格列出的遗留（本计划不做）
- 被邀请人的奖励（首充赠送 / 优惠券）；多级（二级）分销；团队 / 代理商结算。
- 返利提现、对公结算、发票与税务。
- 独立返利余额与抵扣优先级。
- 按充值返利、固定注册奖励、阶梯费率、按商品差异化费率。
- 注册后补填邀请码；更换邀请码；自定义邀请码；邀请海报生成。
- 短信 / 站内信 / 邮件通知（绑定成功、返利到账、月度汇总）。
- 定期全量同身份扫描；更强的设备指纹；手机号段 / 虚拟号识别；行为模型风控。
- 撤销时自动扣回已发放返利（现由财务调账）。
- 返利期到期前提醒；返利期延长 / 续约。
- 后台代用户生成码；运营活动码（非个人码）。
- 邀请页的 H5 分享卡片 / 微信小程序。
- 对账报表按费率版本汇总（CSV 已含版本列）。

## 可以继续延后
- `referral.CheckInvariants` 的第 4 条是静止态检查（要求没有待处理流水）：生产库上执行前等一轮清扫；可改为只检查 `created_at < now − 1h` 的流水。
- 后台「邀请人」列表的统计是三表 JOIN 的聚合查询，邀请人上万时考虑物化或加 `referral_accounts` 上的汇总列。
- 退款流水对应的订单没有任何事件时（订单查询失败）会被延后到下轮；若 `order_pay` 永远处理不了（订单行不存在），退款也永远延后——只会在 1 小时回看窗口内重试，之后要靠后台「重扫」；可加告警。
- 同一订单一次性多条退款流水（`-r1`、`-r2`）与扣回 `-c<n>` 序号按事件计数，重扫不会重复。
- 控制台邀请页二维码用 `qrcode` 在前端生成，没有服务端图片；长按保存依赖浏览器对 `<canvas>` 的支持。
- `X-Device-Id` 只是 localStorage 的 UUID，清缓存 / 隐私窗口即换新；规格已接受。

## 实施中确定的产品行为（裁定）
- `identity.Login` 签名不变，新增 `LoginWith`；`identity` 导入 `referral` 的类型，`referral` 不导入 `identity / rental`。
- 退款先于计提落地：无任何事件的订单延后处理；已有 `capped / void` 占位的订单写 `refund(0)` 占位。
- Discover 同一批内先处理 `order_pay` 再处理 `refund`。
- 费率版本 `effective_from` 允许 5 分钟的「过去」容差（表单提交延迟）。
- 后台 `inviters` 的默认排序是累计发放净额降序。
（实施中新增的裁定按实际补充。）
```

- [ ] **Step 3: 全量验证**

Run:
```bash
export PATH=$HOME/.local/go/bin:$HOME/.local/bin:$PATH
go vet ./... && go test -race ./... && make check
```
Expected: `go test -race` 全部通过；`make check` = `test`（Go + `web-test` + `install_test.sh`）+ `e2e`（`make web` → `npm run e2e:coverage` → Playwright 三个 project 全部通过，原有用例 + 本计划 18 个新用例）。失败项逐个修到绿，不允许 skip。

- [ ] **Step 4: 提交**

```bash
git add README.md docs/superpowers/followups-plan8.md docs/superpowers/followups-plan4.md
git commit -m "docs: plan 8 followups, README (backfill-fingerprints, deploy notes, login invite_code), final make check

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

## 上线前需要核对

- 执行 `platform referral backfill-fingerprints`。
- 法务 / 财务 / 运营核对项见 `followups-plan8.md`。
- `SPARK_PUBLIC_BASE_URL` 为用户可访问的控制台地址。

## 规格列出的遗留（本计划不做）

见规格 §17 与 Task 19 的 `followups-plan8.md`。

## 自检记录（写计划时完成）

**规格覆盖**（§ → 任务）：§0 代码事实（`Login` 的 `ON CONFLICT`、`wallet.Apply` 语义、三种退款 `ref_id` 形态、`RunSweeper` 附加任务、`sweepEach` 模式、`stepUp`、`csvExport`、迁移编号）→ Task 1、2、4、8、10、13；§1.1 第 1 条（只在注册时绑定、`xmax = 0`）→ Task 4（`TestLoginWithIsNewDeviceAndLastLogin`）、5；第 2 条（实付消费扣退款）→ Task 7、8；第 3 条（并入余额）→ Task 9；第 4 条（延迟发放）→ Task 7（`settle_after`）、9；第 5 条（扣回 + 欠账）→ Task 8（`TestRefundSettledClawsBackAndRecordsDebt`）、9（`TestReleaseZeroVoidsAndDebtOffset`）；第 6 条（扫流水发现）→ Task 10；第 7 条（版本化费率）→ Task 3（`TestConfigAtPicksLatestEffectiveVersion`）、7；第 8 条（四层防刷）→ Task 5、6、7（上限）、9（实名门槛）；第 9 条（指纹）→ Task 3、6；第 10 条（邀请码）→ Task 3、15；第 11 条（不加短信）→ 无短信代码；第 12 条（角色）→ Task 13（`permissions_test`）；§1.3 R1～R3、R5（首版参数）→ Task 1 迁移；R4 → Task 13（用户详情 `settled_net`）、19（README / followups）；R6 → Task 9；R7 → Task 4、12、15；R8 → Task 5（`TestBindRiskFlagsMatrix`）；R9 → Task 11；R10 → Task 12（`/me`）、15（入口）；R11 → Task 12（脱敏）、15（未实名提示）；R12 → Task 3（不可自定义 / 更换：没有接口）；R13 → Task 8、9（备注）、15（钱包标签）；R14 → Task 3（`TestAddRateVersionValidation`）；§2 迁移 → Task 1（`TestMigration00020RoundTrip`）；§3.1 → Task 3；§3.2 → Task 4、5；§3.3 → Task 6（`TestOnKYCVerifiedRevokesSameIdentity`、`TestOnKYCVerifiedInviterLater`）；§4.1 → Task 3；§4.2 → Task 7（`TestAccrueUsesVersionAtPayTimeAndFloors`、`TestAccrueCapsPerInviteeAndPerInviter30d`、`TestAccrueBookingPeriodOverRevokedAndMissingOrder`）；§4.3 → Task 2、8；§4.4 → Task 9（`TestReleaseDueRewardsWithThresholds`、`TestReleaseBatchLimit`）；§5.1 → Task 10（`TestDiscoverProcessesOnlyBoundUsersAndAdvancesWatermark`、`TestDiscoverBacklogKeepsWatermark`）；§5.2 → Task 10（装配、`TestSettleRunsDiscoverThenRelease`）；§6 防刷表 → Task 5、6、7、9、11；§7 交互表 → Task 7（预订 `settle_after`、返利期）、8（三种退款形态）、14（预订取消、开通失败、封禁 / 冻结）、13（仪表盘）；§8 控制台 → Task 15；§9 后台 → Task 16；§10 错误码与接口 → Task 1、12、13；§11 模块与接口 → Task 3、4、7、10；§12 常量 → Task 3；§13 迁移与兼容 → Task 1、6（回填）、19（README）；§14 并发与不变量 → Task 3（头注释）、7～9（锁顺序）、8（`CheckInvariants`）、10（重试、幂等）、14（`TestReferralConcurrentFundsConsistent`）；§15 上线前核对 → Task 19；§16.1 单元 → Task 1～13；§16.2 集成 → Task 14；§16.3 e2e → Task 17；§16.4 验收 12 项 → Task 18；§17 遗留 → Task 19；§18 任务拆分 18 项 → 本计划 19 项（验收与收尾分开）。

**占位符扫描**：全文无 TBD / TODO / 「类似 Task N」；每个代码步骤都有完整代码块或精确的插入位置与内容；Task 8 的 `CheckInvariants` 实现提前到 Task 8 而不是留空；Task 16 的后台页面给出全部 `data-testid` 与关键逻辑；Task 17 的 A-REF-01 对「同设备搁置」的构造写明替换步骤。

**类型一致性**：`wallet.ApplyUpTo(ctx, tx, Entry) (applied, after int64, err)`（Task 2）↔ `referral.WalletOps`（Task 3）↔ `applyRefund`（Task 8）；`referral.BindRequest / BindResult / Reason*`（Task 4 定义、Task 5 实现）↔ `identity.ReferralHook / LoginWith / LoginResult.Invite`（Task 4）↔ `api.handleLogin`（Task 12）↔ e2e 钩子（Task 17）；`referral.UserState / UserLookup`（Task 3）↔ `identity.UserState / HasDevice / VerifiedFingerprints`（Task 4）↔ `fakeUsers`（Task 3 helper）；`OrderLookup.OrderForReferral`（Task 3）↔ `rental.OrderForReferral`（Task 7）↔ `fakeOrders`；`ledgerRow / errSkip / Reward / rewardCols / scanReward / lockReward / alreadyProcessed / inviteeMasked / addEvent / lockAccount`（Task 3、7）↔ `applyRefund`（Task 8）↔ `releaseOne`（Task 9）↔ `processRow / Discover`（Task 10）↔ `AdminReward`（Task 13）；`revokeBinding(ctx, tx, now, bindingID, reason, actor, flag, action)`（Task 6）↔ `Revoke`（Task 11）；`withRetry / pgCode`（Task 9）↔ `processRow`（Task 10）；`RateVersion / RateInput / ConfigAt / CurrentConfig / ListRateVersions / AddRateVersion`（Task 3）↔ `accrue`（Task 7）↔ `handleReferralSetConfig`（Task 13）↔ 钩子 `referralConfig`（Task 17）；`Freeze / Unfreeze / Approve / Revoke / Rescan`（Task 10、11）↔ `adminapi/referral.go`（Task 13）；`Summary / InviterFilter / InviterRow / AdminInviter / Binding / BindingFilter / AdminListBindings / ExportBindings / RewardFilter / AdminListRewards / ExportRewards / UserSummary / MonthSettledNet`（Task 13）↔ 后台页面类型（Task 16）；`Me / Invitees / Rewards / MonthSettledFor / MeView / InviteeView / RewardView`（Task 12）↔ `ReferralMe / Invitee / RewardItem`（Task 15）↔ e2e `rewardsOf`（Task 17）；`api.Server.ReferralLimit / referralLim`（Task 12）↔ `app.Build`；`defaultDeviceId / HttpConfig.deviceId`（Task 15）↔ `http.spec.ts`；`data-testid` 清单（Task 15、16）↔ 用例（Task 17）。

**Review Focus 落点**：1 → Task 8 `TestRefundWithoutRewardDeferredOrPlaceholdered` + Task 10 `TestDiscoverProcessesPayBeforeRefundInSameBatch`；2 → Task 2 `TestApplyUpToConcurrentWithDebitsNeverOverdraws` + Task 14 `TestReferralConcurrentFundsConsistent`；3 → Task 8 `TestRefundUsesStoredRateNotCurrentVersion`；4 → Task 11 `TestFreezeBlocksReleaseUntilUnfreeze`、`TestRevokeVoidsPendingKeepsSettled`；5 → Task 3 `TestGenerateCodeShapeAndNormalize`、Task 12 `TestLoginWithGarbageInviteCodeStillRegisters`、Task 15 `invite.spec.ts`。
