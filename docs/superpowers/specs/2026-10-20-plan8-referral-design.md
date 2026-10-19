# Spark Cloud — 计划 8：邀请返利设计文档

- 日期：2026-10-20
- 状态：待评审（负责人已授权由设计方裁定，不再逐项询问；代负责人作出的产品裁定集中列在 §1.3，可事后推翻）
- 上位文档：`2026-10-14-spark-cloud-mvp-design.md`（下称「总设计」）§2 `wallet_*`、§11；`2026-10-15-plan4-admin-payments-design.md`（下称「计划 4」）§0.1 第 3 条、§2 权限矩阵、§3 用户 / 财务；`2026-10-19-plan6-booking-design.md`（下称「计划 6」）§5.5 取消退款、§14 并发；`followups-plan4.md` / `followups-plan6.md`
- 范围：邀请码与邀请链接、注册时绑定邀请关系、按被邀请人**实付租用消费**的比例给邀请人返利、退款扣回、防刷、后台配置 / 审核 / 冻结 / 报表 / CSV、控制台邀请页、测试与开发平台验收。
- 不在范围：见 §17。

## 0. 现状与约束（实现时不必再查）

**代码事实（`main` 上计划 1～6 已合并，分支 `feat/plan8-referral`）**
- 账号：`users(id, phone UNIQUE, kind, kyc_status ∈ {none,pending,verified,rejected}, status ∈ {active,banned}, created_at, updated_at)`。**没有独立的注册接口**：`identity.Login` 校验验证码后 `INSERT INTO users … ON CONFLICT (phone) DO UPDATE … RETURNING`，首次登录即注册；`users` 表没有最近登录 IP、没有设备信息；`sessions` 只有 `token_hash / user_id / 时间`。控制台登录页 `LoginView.vue` 提交 `POST /api/v1/auth/login {phone, code}`，按钮文案「登录 / 注册」，登录后跳 `route.query.redirect`。
- 实名：`kyc_records(user_id, kind, real_name, id_number_enc BYTEA(AES-GCM), license_no, result ∈ {pending,verified,rejected,error})`；身份证号只有密文，**无法按身份证号做相等比较**；企业用 `license_no` 明文。`identity.CanRent` = 账号未封禁且 `kyc_status='verified'`，所有下单 / 续费路径都先过它——**产生 `order_pay` 流水的用户一定已实名**。
- 钱包：`wallet_accounts(user_id PK, balance ≥ 0 CHECK)`；`wallet_ledger(id BIGSERIAL, user_id, amount ≠ 0, balance_after, kind CHECK IN ('recharge','admin_adjust','order_pay','refund'), ref_type, ref_id, operator_id, remark, created_at)` 只追加；`wallet_ledger_ref_uniq (kind, ref_type, ref_id) WHERE ref_id <> ''` 防重复记账。`wallet.Apply(ctx, tx, Entry)` 在调用方事务内 `FOR UPDATE` 锁账户行、余额不足返回 `ErrInsufficientBalance`、重复返回 `ErrDuplicate`；`AdminAdjust` 用咨询锁 `wallet-admin:<user>` 串行化并写审计 `wallet.adjust`。后台 `LedgerFilter.Kind` 只接受 `ledgerKinds` 四种；前端 `web/shared/src/labels.ts` 的 `ledgerKindLabel` 同四种。**余额不可提现**（计划 4 §0.1 第 3 条），线下退款由财务后台「调账」扣减。
- 订单与扣款：`rental.charge` 一单一笔 `order_pay` 流水，`ref_type='order'`、`ref_id=<订单号>`，`remark` 为「新购 / 续费 / 自动续费 <实例>」；`orders.amount` = 单价 × `quantity`，`kind ∈ {new, renew}`，`refunded_amount / refund_count / status ∈ {paid, partially_refunded, refunded}`。
- 退款路径（**全部**经 `wallet.Apply(kind=refund, ref_type='order')`，共三种 `ref_id` 形态）：
  - `refundAcross`（`refund.go`）：`ref_id = <订单号>-r<序号>`。调用方：后台「退款并释放」`RefundRelease`、预订取消 `cancelInstance / cancelPlatform`（用户 100% / 50%、后台、平台）、迟开通补偿 `compensateShortfall`、机器恢复在线时的封顶补偿。
  - `refundAll`（`results.go`，开通失败）：`ref_id = <订单号>`（无后缀）。
  - `refundTransferred`（换机后新实例开通失败，按 `instance_transfers.seconds` 折算）：走 `refundAcross` 对原实例链路的订单退款（同 `-r<n>` 形态）。
  - 因此「订单号」= `ref_id` 去掉可选的 `-r<数字>` 后缀；一笔订单可能有多条退款流水，累计不超过 `orders.amount`（`orders_refunded_amount_chk`）。
- 充值：`recharges` 经 `billing.Settle` 入账，`kind=recharge, ref_type='recharge'`；手动入账 `channel=manual`。
- 清扫：`rental.RunSweeper(ctx, interval, extra...)` 每分钟在咨询锁下跑 `Rental.Sweep` 再跑附加任务（现在只有 `Billing.Sweep`，`cmd/platform/main.go:96`）；e2e-server 不启动清扫器，`POST /hooks/sweep` 顺序调用 `Rental.Sweep` 与 `Billing.Sweep`。`sweepEach` 模式：无锁选候选、逐个独立事务 `FOR UPDATE SKIP LOCKED` 复核。
- 后台：角色 `super / ops / finance`，路由表 `allRoles / opsRoles / financeRoles`，`permissions_test.go` 表驱动核对；资金操作走 `stepUp` TOTP（`totp` 字段，5 分钟窗口，成功后才开窗）；审计 `audit.Record(ctx, tx, now, actor.Entry(action, targetType, targetID, diff))`；CSV 导出 `csvExport`（UTF-8 BOM、最多 `csvx.MaxRows` 10 万行）；用户详情 `handleUser` 返回 `user / balance / instances / orders / ledger / kyc / ssh_keys`。
- 前端：控制台路由 `/login`、`/market`、`/instances*`、`/wallet*`、`/kyc`、`/account`、`/account/ssh-keys`、`/help`；`qrcode@1.5.4` 已是控制台依赖（充值二维码）；`web/shared/src/device.ts` 存在（设备判定，非设备 ID）。后台菜单 `web/admin/src/menu.ts`（`menu.spec.ts` 核对 `meta.roles`），已有 `用户 / 订单 / 流水 / 充值单` 页。
- 通知：只有短信（`sms.Sender`，模板 `login_code / expiry_reminder / auto_renew_failed / booking_cancelled`，aliyun 驱动要求全部模板编号配置齐全）。没有站内信。
- e2e：Playwright 单 worker、假时钟；钩子 `/hooks/users {phone?, verified, balance, ledger_rows}`、`/hooks/kyc {reject}`、`/hooks/sweep`、`/hooks/admins`、`/hooks/clock/advance`。覆盖表前缀已有 `C-AUTH / C-ACC / C-WAL / A-USR / A-FIN` 等。
- 迁移编号到 `00019_booking.sql`；本计划是 `00020_referral.sql`（带 `+goose Down`）。
- 配置：`SPARK_PUBLIC_BASE_URL`（控制台对外地址，形如 `https://域名`）、`SPARK_SECRET_KEY`（`secretbox`，可派生 HMAC 密钥）。

**竞品做法（国内 GPU 云常见）**：邀请链接 / 邀请码；被邀请人注册即绑定、终身或 6～12 个月有效；返利 = 被邀请人实际消费的 5%～15%，退款扣回；返利进不可提现余额，用于抵扣租用；绑定后的被邀请人在邀请人页面显示脱敏手机号；后台有冻结与黑名单。

## 1. 架构与核心决策

```
 注册（登录首单） ──invite_code──▶ identity.Login ──(新用户)──▶ referral.Bind（同事务）
                                                                  │ 风险标记（同 IP / 同设备 / 爆发）
 实名通过 ─────────────────────▶ identity.KYC ──▶ referral.OnKYCVerified（同身份 → 撤销绑定）
                                                                  ▼
 ┌─────────────────── referral（只读 wallet_ledger，只写自己的表；资金变动全部经 wallet.Apply）──────────────────┐
 │ Settle（清扫附加任务，每分钟）：                                                                          │
 │   1. Discover：扫描 wallet_ledger 中 kind ∈ {order_pay, refund} AND ref_type='order' 且用户有绑定的新行      │
 │      order_pay → 建 / 更新 referral_rewards（按下单时刻生效的费率版本计算、按上限封顶）→ pending         │
 │      refund    → 重算该订单的应得返利；已发放部分多出来的 → 扣回（余额不够的记为欠账）                       │
 │   2. Release：settle_after 到期且绑定 active、邀请人未冻结且已实名 → 先抵欠账，再 wallet.Apply(+referral_rebate) │
 │ 幂等：referral_events.source_ledger_id 唯一；wallet_ledger_ref_uniq (kind, 'referral', ref_id)           │
 └───────────────────────────────────────────────────────────────────────────────────────────────────────┘
                                   │                                          │
                          控制台 /invite（码、链接、被邀请人、收益）        后台 /referral（配置、审核、冻结、报表、CSV）
```

### 1.1 决策一览

| # | 决策 | 结论 | 理由 |
|---|---|---|---|
| 1 | 绑定时机 | **只在注册时绑定**：`POST /auth/login` 带 `invite_code`，`identity.Login` 的 `INSERT … ON CONFLICT` 用 `(xmax = 0) AS is_new` 判断本次是否创建了用户，只有新用户才在同一事务里 `referral.Bind`；老用户带码登录不绑定，响应说明原因 | 「先注册再补填邀请码」是最常见的刷返利方式（把存量用户改挂到自己名下）；注册即绑定也让「谁带来了这个用户」只有一个答案 |
| 2 | 返利基数 | **被邀请人的实付租用消费**（`order_pay` 流水，含新购、手动续费、自动续费、预订、批次），**扣除退款**；不按充值、不给固定奖励 | 充值可以线下退、可以调账，按充值返利等于让平台垫钱；固定奖励会招来批量注册；按实付消费返利的成本与收入严格成比例 |
| 3 | 返利归属 | 返利**并入邀请人的钱包余额**（新流水类型 `referral_rebate`），不另设「返利余额」；余额本就不可提现，不新增提现 | 另设账户要改 `charge`（两账户扣款顺序）、所有退款路径（退回哪个账户）与前端，资金正确性风险最大；钱包余额不可提现的既有约束已经满足「不能变现」 |
| 4 | 发放时机 | 先记为 **待发放（pending）**，`settle_after = max(扣款时刻 + 7 天, 预订开始时刻)` 到期后发放到余额 | 用户可自行全额退款的窗口（预订取消：15 分钟冷静期、开始前 24h 全退）全部落在发放之前，避免「付款 → 拿返利 → 退款 → 返利已花掉」；7 天也覆盖开通失败自动退款与绝大多数售后 |
| 5 | 扣回 | 退款流水出现时**重算该订单的应得返利** `floor((amount − refunded) × 费率)`；未发放的只改数字；已发放的多出部分从邀请人余额扣回（`referral_clawback`），余额不够的差额记为**欠账**，后续返利先抵欠账 | 钱包余额不能为负（CHECK），强扣会失败；欠账保证平台最终不多付；所有退款路径都写 `refund` 流水，按流水扣回不需要改任何一条退款路径 |
| 6 | 发现机制 | referral 不嵌入 `charge` / 退款事务，而是作为清扫附加任务**扫描 `wallet_ledger`**（`wallet.ScanOrderLedger` 接口，只读），按 `source_ledger_id` 幂等落库 | 不碰 rental 的 7 条资金路径；钱包流水是唯一的资金真相，扫流水天然覆盖未来新增的退款路径；延迟一分钟对返利无影响 |
| 7 | 费率与参数 | 后台可配，**版本化**（`referral_rate_versions`，只增不改），订单按其扣款时刻生效的版本计算；绑定的有效期按绑定时刻生效的版本固定 | 改费率不影响已产生的返利；审计与对账都能回溯 |
| 8 | 防刷 | 四层：① 不能自邀（新用户不可能是码主；同身份在实名时撤销）② 实名门槛（被邀请人必须实名才能下单，邀请人必须实名才能收款）③ 同 IP / 同设备 / 爆发式绑定标记，同设备或同身份自动**搁置**待审 ④ 单被邀请人终身上限、单邀请人 30 天上限 | 没有一种手段单独够用；标记 + 搁置 + 上限把损失封顶，后台再人工裁决 |
| 9 | 同身份判定 | 实名通过时计算 `fingerprint = HMAC-SHA256(SecretKey, 'kyc:' + 身份证号 或 'lic:' + 信用代码)` 存 `kyc_records.fingerprint`；被邀请人的指纹等于邀请人或邀请人任一其他被邀请人 → 该绑定撤销（返利全部作废） | 身份证号只有密文无法比较；HMAC 不可逆、可相等比较；一个人多个手机号注册互相邀请是最常见的自刷 |
| 10 | 邀请码 | 每人一个、永久、8 位 Crockford Base32（去掉 `I L O U`），首次打开邀请页时生成；链接 `https://<域名>/?ic=<码>`；控制台把码存 localStorage 30 天，注册时自动带上 | 永久码便于线下传播；8 位随机不可枚举；30 天归因窗口与竞品一致 |
| 11 | 通知 | **不加短信**；控制台「我的」与邀请页展示新增返利；绑定成功在登录响应里告知被邀请人 | 返利是低频小额，短信有成本且易被当作骚扰；本计划不引入站内信 |
| 12 | 后台角色 | 配置费率 super + TOTP；审核搁置 / 冻结 / 解冻 / 撤销 finance（撤销会作废待发放返利，走 TOTP）；报表与导出 finance；用户详情的邀请信息 all | 与计划 4「涉及资金的走 finance + TOTP」一致 |

### 1.2 不做的替代方案（评审时不必再提）
- 「独立返利余额 + 租用优先扣返利」：改 `charge` 双账户、改 7 条退款路径、前端两处余额；放弃（第 3 条）。
- 「在 `charge` / `refundAcross` 里同步调用 referral」：耦合进每条资金路径，漏一条就漏返利或漏扣回；放弃（第 6 条）。
- 「按充值金额返利」：放弃（第 2 条）。
- 「注册后 N 天内可补填邀请码」：放弃（第 1 条）。
- 「返利可提现 / 对公结算」：涉及税务与打款，不在 v1。
- 「被邀请人也得奖励（优惠券 / 首充赠送）」：没有优惠券体系（计划 6 遗留）；放到遗留清单。
- 「用 `wallet_ledger.id` 做游标」：BIGSERIAL 的分配顺序 ≠ 提交顺序，游标会跳过晚提交的小 id；改用「时间水位 + 1 小时回看 + 唯一键幂等」（§5.1）。

### 1.3 负责人裁定（默认，可推翻）

| # | 裁定 | 理由 |
|---|---|---|
| R1 | 返利比例 **10%**（`rate_bp = 1000`），按被邀请人实付租用金额（扣退款）计算，向下取整到分 | 竞品 5%～15%；10% 在 ¥69/天的定价下每台每天 ¥6.9，运营可在后台改 |
| R2 | 返利期 **12 个月**（自绑定时刻起），到期后被邀请人的消费不再产生返利；绑定本身永久保留（用于展示与统计） | 「终身返利」让获客成本不封顶；12 个月足够覆盖一个客户的主要生命周期 |
| R3 | 上限：**单个被邀请人累计返利 ≤ ¥500**；**单个邀请人 30 天内发放 ≤ ¥5,000**；超出部分不计（记录为 `capped`，不补发） | 把最坏情况的损失封顶；¥500 ≈ 被邀请人消费 ¥5,000；大客户返利另行线下谈 |
| R4 | 返利进钱包余额，**不可提现、不可转让**；财务线下退款时按「充值净额 − 消费」核算，返利部分不退（后台用户详情显示「累计返利」供财务核对） | 见 §1.1 第 3 条；防止「邀请 → 返利 → 线下退款」变现 |
| R5 | 发放延迟：`settle_after = max(扣款 + 7 天, 预订开始时刻)`；发放前退款只改数字，发放后退款扣回、不足记欠账 | 见 §1.1 第 4、5 条 |
| R6 | 邀请人**必须已实名**才发放（未实名时返利保持待发放，实名后下一轮发放）；被邀请人下单本身已要求实名 | 返利是收益，收益人要可追溯 |
| R7 | 绑定只在注册时；邀请码存 localStorage **30 天**；老用户带码登录不绑定；邀请码无效 / 码主被冻结时**注册照常成功、不绑定**，登录响应 `invite.bound=false` 并给原因 | 邀请码问题不能挡注册 |
| R8 | 自动搁置（`held`，返利待发放但不发放，等后台裁决）的条件：被邀请人与邀请人**同设备**，或同身份（撤销而非搁置）；**同 IP（24h 内）**与**爆发（同一邀请人 24h 内 ≥ 20 个绑定）**只打标记不搁置 | 家庭 / 公司 NAT 下同 IP 很常见，搁置会误伤；同设备几乎只有自刷 |
| R9 | 后台裁决：搁置 → `approve`（转 active，待发放照常发放）或 `revoke`（撤销：待发放全部作废，已发放不自动扣回、由财务用现有调账处理）；冻结邀请人：码不再可绑定、返利停止发放（继续累计），解冻后恢复 | 撤销与冻结是两个粒度（单个绑定 / 整个邀请人） |
| R10 | 不发短信、不做站内信；控制台「我的」页显示「本月返利 ¥X」入口 | 见 §1.1 第 11 条 |
| R11 | 邀请页需登录；未实名也能看到码与链接（页面提示「实名后才能收款」）；被邀请人在邀请人页面以脱敏手机号 `138****1234` 显示，不显示姓名 | 先让用户能分享；隐私最小化 |
| R12 | 邀请码不可自定义、不可更换；一人一码永久有效 | 自定义码会被用来冒充运营活动，更换码会让线下传播的旧码失效 |
| R13 | 返利流水在用户钱包页按「邀请返利 / 返利扣回」展示，备注写被邀请人脱敏手机号与订单号 | 用户要能对账 |
| R14 | 费率版本的 `effective_from` 必须 ≥ 提交时刻（不能追溯生效）；首个版本由迁移写入（R1～R3、R5 的默认值） | 追溯生效会改变已发放返利的口径 |

## 2. 数据模型（迁移 `00020_referral.sql`，带 `+goose Down`）

```sql
-- 费率版本（只增不改）
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

-- 邀请人账户（有码才有行）
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

-- 绑定：一个被邀请人只能被绑定一次
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

-- 返利：一笔订单一行（订单是扣款与退款的共同主键）
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

-- 事件：只追加；source_ledger_id 是「一条钱包流水只处理一次」的幂等键
CREATE TABLE referral_events (
    id               BIGSERIAL PRIMARY KEY,
    order_id         TEXT NOT NULL,
    binding_id       BIGINT NOT NULL REFERENCES referral_bindings (id),
    kind             TEXT NOT NULL CHECK (kind IN ('accrue', 'refund', 'settle', 'clawback', 'debt_add', 'debt_offset', 'void', 'capped')),
    source_ledger_id BIGINT,                                              -- accrue / refund 来自哪条 wallet_ledger
    wallet_ledger_id BIGINT,                                              -- settle / clawback / debt_offset 写出的 wallet_ledger
    amount           BIGINT NOT NULL,
    detail           TEXT NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ NOT NULL
);
CREATE UNIQUE INDEX referral_events_source_uniq ON referral_events (source_ledger_id) WHERE source_ledger_id IS NOT NULL;
CREATE INDEX referral_events_order_idx ON referral_events (order_id, id);

-- 扫描水位（单行）
CREATE TABLE referral_state (
    id        INT PRIMARY KEY CHECK (id = 1),
    watermark TIMESTAMPTZ NOT NULL
);
INSERT INTO referral_state VALUES (1, '2000-01-01');   -- 不能用 now()：测试与 e2e 用假时钟，流水 created_at 可能早于真实时间；
                                                       -- 首轮扫描会看全部历史流水，但 JOIN referral_bindings 为空，代价可忽略，之后水位推进到 now − 2m

-- 钱包：两种新流水类型 + 扫描索引
ALTER TABLE wallet_ledger DROP CONSTRAINT wallet_ledger_kind_check;
ALTER TABLE wallet_ledger ADD CONSTRAINT wallet_ledger_kind_check
    CHECK (kind IN ('recharge', 'admin_adjust', 'order_pay', 'refund', 'referral_rebate', 'referral_clawback'));
CREATE INDEX wallet_ledger_order_scan_idx ON wallet_ledger (created_at, id) WHERE ref_type = 'order';

-- 身份：最近登录 IP、设备、实名指纹
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
```

说明：
- `referral_rewards.order_id` 不加外键（模块边界：referral 不引用 rental 的表），由测试保证每行都对应真实订单。
- `wallet_ledger_kind_check` 的约束名以实际库为准（`00004` 用的是列内 CHECK，PG 自动命名为 `wallet_ledger_kind_check`；迁移前用 `\d` 核对，Down 恢复四种）。
- `wallet.ledgerKinds`、`web/shared/src/labels.ts`、后台流水筛选下拉同步加两种类型（中文：`邀请返利`、`返利扣回`）。
- **存量实名用户没有指纹**：迁移不解密回填（迁移脚本拿不到密钥）；提供 `platform referral backfill-fingerprints` 子命令，用 `SPARK_SECRET_KEY` 解密 `id_number_enc` 逐条写 `fingerprint`，上线后执行一次（§15）。未回填前同身份判定只对新实名生效。
- **Down**：`DROP TABLE referral_events, referral_rewards, referral_bindings, referral_accounts, referral_rate_versions, referral_state, user_devices`；`kyc_records` / `users` 删新列；`wallet_ledger_kind_check` 恢复四种——**若已存在 `referral_*` 流水则 Down 拒绝**（`DO $$ … RAISE EXCEPTION '仍有邀请返利流水，不能回滚' $$`）：流水只追加，改类型就是篡改账目。

## 3. 邀请码与绑定

### 3.1 邀请码
- `referral.EnsureCode(ctx, userID)`：有行则返回，没有则生成 8 位 Crockford Base32（`crypto/rand`，字母表 `0123456789ABCDEFGHJKMNPQRSTVWXYZ`）插入；撞唯一索引重试（最多 5 次）。被封禁用户也能有码（但冻结 / 封禁状态下的码不可绑定，见 §3.2）。
- 输入规范化：去空白、转大写、`O→0`、`I/L→1`（用户手打常混）。
- 链接：`<PublicBaseURL>/?ic=<code>`（市场页）；控制台 `App.vue` 在任意路由读到 `ic` 参数时存 `localStorage.sc_invite = {code, at}`，并从地址栏移除参数；30 天后过期（R7）；已登录用户访问带码链接不存储（已经是老用户）。
- `GET /api/v1/referral/codes/{code}`（匿名，按 IP 每分钟 60 次）：`{valid, inviter: "138****1234"}`；`valid=false` 的情况（不存在 / 冻结 / 码主被封禁）不区分原因。登录页据此显示「通过 138****1234 的邀请注册」，可点 × 清除。

### 3.2 注册时绑定（`identity.Login` 改动）
- 请求：`POST /auth/login {phone, code, invite_code?}`，请求头 `X-Device-Id`（控制台生成的 UUID，`localStorage.sc_device`，无则不带）。
- `Login` 事务内：
  1. `INSERT INTO users … ON CONFLICT (phone) DO UPDATE SET updated_at = EXCLUDED.updated_at, last_login_ip = $ip, last_login_at = $now RETURNING <userCols>, (xmax = 0) AS is_new`（新插入的行 `xmax = 0`；冲突更新的行 `xmax ≠ 0`）。
  2. `INSERT INTO user_devices … ON CONFLICT DO UPDATE SET last_seen_at`（有 `X-Device-Id` 且格式为 UUID 时；每用户最多 10 台，超出时删最旧——`DELETE … WHERE user_id = $1 AND device_id NOT IN (SELECT … ORDER BY last_seen_at DESC LIMIT 10)`）。
  3. `is_new AND invite_code != ""` → `referral.Bind(ctx, tx, BindRequest{Code, InviteeID, IP, DeviceID, UserAgent, Now})` → 返回 `BindResult{Bound bool, Reason string, InviterMasked string}`。`Bind` 的任何**业务**失败（码不存在、码主冻结 / 封禁、码主 = 被邀请人——理论上不可能）都返回 `Bound=false` 且**不回滚注册**；数据库错误照常回滚。
  4. 响应：`User` 增加 `invite: {bound, reason, inviter}`（仅本次请求带了 `invite_code` 时出现；`reason ∈ invalid_code / inviter_frozen / existing_user / ok`）。
- `referral.Bind`（在 identity 事务内，锁顺序 `users(被邀请人, 已由 INSERT 持有) → referral_accounts(码主) FOR SHARE → referral_bindings 插入`）：
  1. 规范化码、查 `referral_accounts` 并 `FOR SHARE`（防止同时被冻结）；`status='frozen'` 或码主 `users.status='banned'` → `inviter_frozen`。
  2. 取当前费率版本 `v`（`effective_from <= now ORDER BY effective_from DESC LIMIT 1`）；`rebate_until = now + v.period_days`。
  3. 风险标记（只读查询，都是 O(1) 索引）：
     - `same_ip`：`ip != '' AND ip = inviter.last_login_ip AND inviter.last_login_at > now − 24h`；
     - `same_device`：`device_id != '' AND EXISTS (user_devices WHERE user_id = inviter AND device_id = $device)`；
     - `burst`：`count(referral_bindings WHERE inviter_id = inviter AND bound_at > now − 24h) >= 20`（含本次前的 19 条）。
  4. `status = 'held'`（`hold_reason='same_device'`）若含 `same_device`，否则 `active`。
  5. 插入绑定（`invitee_id UNIQUE` 兜底）；写审计 `referral.bind`（actor user，diff 含 flags）。
- **不做**：同一手机号注销重注册（系统没有注销）；绑定后改邀请人（没有接口）。

### 3.3 实名通过时的同身份判定（`identity.SubmitPersonalKYC` / `ReviewEnterpriseKYC` 的结果事务）
- 结果为 `verified` 时，在同一事务里：`fingerprint = hex(HMAC-SHA256(key, "kyc:" + 身份证号))`（企业：`"lic:" + 信用代码`），`key = HKDF(SecretKey, "referral-fingerprint")`；写入本条 `kyc_records.fingerprint`。
- 然后调用 `referral.OnKYCVerified(ctx, tx, userID, fingerprint)`：
  1. 若该用户是**被邀请人**（有 `referral_bindings.invitee_id = userID` 且状态 ≠ revoked）：查「邀请人的 verified 指纹」与「同邀请人其他被邀请人的 verified 指纹」是否等于本指纹（`kyc_records_fingerprint_idx`）；命中 → 绑定 `revoked`，`risk_flags += same_identity`，`hold_reason='same_identity'`，该绑定全部 `pending` 返利 → `void`（事件 `void`），审计 `referral.auto_revoke`。
  2. 若该用户是**邀请人**（有被邀请人）：对其所有未撤销绑定做同样比较（被邀请人先实名、邀请人后实名的情况）。
- 指纹比较只在实名通过时做一次，不做定期全量扫描（遗留）。

## 4. 返利计算

### 4.1 费率版本
- `referral.ConfigAt(ctx, q, t)`：`effective_from <= t` 的最新版本；表非空（迁移写入首版，`effective_from='2000-01-01'`）。
- 后台新增版本：`effective_from >= now`（R14），字段范围见 CHECK；写审计 `referral.config`（含新旧版本全部字段）。
- 哪个版本决定什么：

| 参数 | 取哪个版本 |
|---|---|
| `period_days`（返利期） | **绑定时刻**的版本（写入 `referral_bindings.rebate_until`，不随后续版本变） |
| `rate_bp`、`max_per_invitee`、`max_per_inviter_30d`、`hold_days` | **扣款流水 `created_at` 时刻**的版本（写入 `referral_rewards.rate_version_id / rate_bp / reward_cap / settle_after`） |

### 4.2 计提（处理一条 `order_pay` 流水 `l`，用户 = 被邀请人）
```
b := 该用户的绑定（status ∈ active/held；revoked 不计提）
if l.created_at >= b.rebate_until → 事件 capped(detail='period_over') 仅记录（amount 0），不建 reward
v := ConfigAt(l.created_at)
base := -l.amount
raw  := floor(base × v.rate_bp / 10000)
room_invitee := max(0, v.max_per_invitee − Σ reward(该 binding 的 rewards, status ≠ void))
room_inviter := max(0, v.max_per_inviter_30d − Σ settled_amount − clawed_amount(该邀请人, settled_at > now − 30d) − Σ reward(该邀请人, pending))
cap  := min(raw, room_invitee, room_inviter)
settle_after := max(l.created_at + v.hold_days, OrderLookup(order).scheduled_start_at (NULL 视为 0))
INSERT referral_rewards(order_id, …, order_amount=base, rate_bp, reward_cap=cap, reward=cap, status='pending', settle_after, paid_at=l.created_at)
INSERT referral_events(accrue, source_ledger_id=l.id, amount=cap, detail=raw/room 明细)
cap < raw → 另记事件 capped(amount = raw − cap)
```
- `OrderLookup` 是 rental 提供的只读接口 `OrderForReferral(ctx, q, orderID) (userID int64, scheduledStartAt *time.Time, err error)`（读 `orders JOIN instances`）；订单不存在（理论上不可能）→ 本行记错误日志、跳过，不写事件（下轮重试）。
- 并发：同一邀请人的两笔计提同时算 `room_inviter` 会双花上限 → 计提事务先 `SELECT … FROM referral_accounts WHERE user_id = inviter FOR UPDATE`（邀请人必有账户行，因为有码才有绑定）。锁顺序统一为 `referral_accounts(邀请人) → referral_rewards(订单) → wallet_accounts(邀请人)`（§14）。
- 30 天上限按「发放净额 + 待发放」占用，是保守口径：待发放最终可能因退款变小，空出的额度下次计提时自然可用。

### 4.3 退款（处理一条 `refund` 流水 `l`，`ref_type='order'`）
```
order := strip(l.ref_id, "-r<n>")
r := referral_rewards[order]（FOR UPDATE，先锁 referral_accounts(邀请人)）；不存在 → 事件不写、返回（此订单无返利）
r.refunded_amount += l.amount（受 CHECK ≤ order_amount 兜底）
target := min(r.reward_cap, floor((order_amount − refunded_amount) × rate_bp / 10000))
事件 refund(source_ledger_id=l.id, amount = r.reward − target)
switch r.status:
  pending: r.reward = target；target == 0 → 保持 pending（到期时按 0 处理为 void）
  settled: excess := (settled_amount − clawed_amount) − target
           if excess > 0:
             applied := Wallet.ApplyUpTo(tx, Entry{inviter, −excess, referral_clawback, ref('referral', order+"-c"+n), remark})
             shortfall := excess − applied
             r.clawed_amount += excess                     -- 账面上全部扣回
             shortfall > 0 → referral_accounts.debt += shortfall，事件 debt_add
             事件 clawback(amount=excess, wallet_ledger_id)
           r.reward = target
  void: 只更新 refunded_amount
```
- `n` = 该订单已有 `clawback` 事件数 + 1（`-c1`、`-c2` …），与 `wallet_ledger_ref_uniq` 一起保证一次退款只扣一次。
- **`wallet.ApplyUpTo`（新钱包原语）**：与 `Apply` 相同的锁与记账逻辑，但扣款额取 `min(|amount|, balance)`；`balance = 0` 时不记流水返回 `applied = 0`。单测覆盖（§16.1）。这是本计划对 `wallet` 包唯一的写路径改动。

### 4.4 发放（`Release`）
候选：`referral_rewards WHERE status='pending' AND settle_after <= now` 按 `settle_after` 每轮最多 200 行，逐行独立事务：
1. `referral_accounts(邀请人) FOR UPDATE` → `referral_rewards(order) FOR UPDATE` 复核 `status='pending' AND settle_after <= now`。
2. 门槛（任一不满足 → 跳过，本轮不动，下轮再看）：绑定 `status='active'`；邀请人账户 `status='active'`；邀请人 `users.status='active' AND kyc_status='verified'`（`identity.UserState` 接口）。
3. `reward == 0` → `status='void'`，事件 `void(detail='zero after refund')`。
4. 抵欠账：`offset := min(debt, reward)`；`debt −= offset`；事件 `debt_offset(amount=offset)`（不写钱包流水）。
5. `credit := reward − offset`；`credit > 0` → `Wallet.Apply(Entry{inviter, +credit, referral_rebate, ref('referral', order), remark="邀请返利 138****1234 订单 ord-…"})`；事件 `settle(amount=reward, wallet_ledger_id)`。
6. `status='settled'`、`settled_amount = reward`、`settled_at = now`。

`settled_amount` 记应得（含被欠账抵掉的部分）而不是实际入账额，这样 `settled_amount − clawed_amount` 始终等于「该订单对邀请人的净贡献」，与 §14 不变量一致。

## 5. 发现与清扫

### 5.1 扫描（`referral.Settle` = Discover + Release，作为清扫附加任务）
```sql
-- 一轮 Discover
SELECT l.id, l.user_id, l.kind, l.amount, l.ref_id, l.created_at
FROM wallet_ledger l
JOIN referral_bindings b ON b.invitee_id = l.user_id          -- 不过滤 status：revoked 绑定的退款也要扣回（见下）
WHERE l.ref_type = 'order' AND l.kind IN ('order_pay', 'refund')
  AND l.created_at >= $watermark - interval '1 hour'
  AND NOT EXISTS (SELECT 1 FROM referral_events e WHERE e.source_ledger_id = l.id)
ORDER BY l.id LIMIT 500
```
- 每行独立事务处理（§4.2 / §4.3）；单行失败只记日志，不影响其他行（下轮因 `NOT EXISTS` 再试）。
- 水位：本轮取到 **少于 500 行** 时 `watermark = now − 2 分钟`；否则不动（有积压，下轮继续）。1 小时回看 + 2 分钟滞后覆盖「id 小但提交晚」的流水（事务不会活过几秒）；即便漏过，`source_ledger_id` 唯一 + 后台「重扫」（§10.2，把水位拨回 N 天）也能补上，且重扫绝不重复计提。
- `revoked` 绑定：计提不做；**退款仍要处理**（撤销前已发放的返利，退款时要扣回）——所以 JOIN 不过滤 `revoked`，在 §4.2 里对 `order_pay` 按状态跳过并写事件 `void(detail='binding revoked', source_ledger_id=l.id)` 占位（否则每轮重扫）。
- 非绑定用户的流水不会被选中（JOIN），不需要占位。
- 性能：`wallet_ledger_order_scan_idx` + `referral_bindings.invitee_id` 唯一索引；200 台规模下每小时 `order` 流水不过几百条。

### 5.2 接入
- `cmd/platform/main.go`：`a.Rental.RunSweeper(ctx, cfg.SweepInterval, a.Billing.Sweep, a.Referral.Settle)`。
- `e2ehooks` 的 `sweep`：在 `Billing.Sweep` 之后调用 `Referral.Settle`。
- `Settle` 内部顺序：Discover → Release（同一轮里新计提的 `hold_days=0` 配置可以立刻发放；默认 7 天不会）。
- 清扫咨询锁已由 `RunSweeper` 持有，多副本不会并发跑 `Settle`；行级 `FOR UPDATE SKIP LOCKED` 作为第二道保险（后台「重扫」与清扫可能同时跑）。

## 6. 防刷汇总

| 手段 | 位置 | 效果 |
|---|---|---|
| 只在注册时绑定 | §3.2 | 存量用户不能被收编 |
| 码主冻结 / 封禁时不可绑定 | §3.2 | 冻结立即止血 |
| 同设备 → 搁置；同身份 → 撤销 | §3.2、§3.3 | 自刷的两种主要形态 |
| 同 IP / 爆发 → 标记 | §3.2 | 后台审核队列排序依据 |
| 被邀请人必须实名才能下单（既有） | `CanRent` | 计提前提 |
| 邀请人必须实名才发放 | §4.4 | 收益可追溯 |
| 发放延迟 7 天 / 预订开始后 | §4.2 | 用户自助退款窗口内不发放 |
| 单被邀请人 ¥500、单邀请人 30 天 ¥5,000 | §4.2 | 损失封顶 |
| 退款扣回 + 欠账 | §4.3 | 退款不能套利 |
| 返利不可提现（既有） | 计划 4 | 不能变现 |
| 后台冻结 / 撤销 / 重扫 | §10.2 | 人工处置 |

## 7. 与现有行为的交互

| 场景 | 行为 |
|---|---|
| 新购 / 续费 / 自动续费 / 预订 / 批次 | 每笔 `order_pay` 一条 reward；预订单 `settle_after ≥ 开始时刻`；批次 N 单 N 行 |
| 预订取消（用户 / 后台 / 平台） | `refund` 流水 → reward 未发放时只改数字（通常减到 0 → 到期 void）；若已发放（不可能：发放不早于开始时刻，开始后不能取消）→ 扣回路径仍正确 |
| 开通失败退款（`refundAll`，`ref_id` 无后缀） | 同上；`strip` 对无后缀的 `ref_id` 原样返回 |
| 后台退款并释放（部分退款） | 已发放 → 按比例扣回多出部分；未发放 → 改数字 |
| 迟开通补偿、机器恢复封顶补偿 | 都是 `refund` 流水，同上（返利随之略减，用户无感） |
| 换机（`instance_transfers`） | 不产生订单，不影响；新实例开通失败走 `refundAcross` 对原订单退款，照常处理 |
| 后台调账 `admin_adjust` / 充值 | 不参与（`ref_type ≠ 'order'`） |
| 被邀请人被封禁 | 已有 reward 照常发放（消费真实发生）；封禁后不能下单，自然没有新返利 |
| 邀请人被封禁 | 发放门槛不满足 → 待发放累积；解封后发放；封禁期间码不可绑定 |
| 邀请人实名被驳回 / 从未实名 | 同上，累积不发放 |
| 邀请人余额被财务线下退款（调账扣减） | 财务按 R4 核算；后台用户详情展示「累计返利净额」 |
| 费率版本变更 | 只影响变更后扣款的订单；绑定的返利期不变 |
| 返利期结束 | 结束后的扣款记 `capped(period_over)`，不建 reward；结束前的 reward 照常发放与扣回 |
| 钱包流水页 / 后台流水页 | 两种新类型可筛选、可导出（`ledgerKinds` 扩展） |
| 仪表盘 | 新增「本月已发放返利」卡片（finance 可见） |

## 8. 控制台页面

### 8.1 落地与登录
- `App.vue`：读 `?ic=`，校验格式（规范化后 8 位字母表内）→ 存 `localStorage.sc_invite={code, at}` → `router.replace` 去掉参数；已登录不存。
- `LoginView.vue`：读 `sc_invite`（30 天内）→ 调 `GET /referral/codes/{code}` → 有效时在表单上方显示「通过 138****1234 的邀请注册」+ ×；无效时静默丢弃并清存储。提交 `POST /auth/login` 带 `invite_code`；响应 `invite.bound=true` → 清存储并 toast「已绑定邀请关系」；`reason=existing_user` → 清存储、不提示；其他原因 → 清存储、toast 后端文案（「邀请码无效」）。
- `web/shared/src/http.ts`：所有请求带 `X-Device-Id`（`localStorage.sc_device`，首次生成 UUID；私密窗口下每次新 UUID，可接受）。

### 8.2 邀请页 `/invite`（auth）
- 顶部卡片：邀请码（大字 + 复制）、链接（复制）、二维码（`qrcode` 生成，可长按保存）、规则摘要（10%、12 个月、7 天后到账、不可提现、上限）。未实名时顶部提示条「实名认证后才能收到返利 → 去实名」。账户冻结时提示条「邀请功能已暂停，如有疑问请联系客服」并隐藏码与链接。
- 统计条：已邀请人数、已实名人数、待发放、累计已发放（净额）、本月已发放。
- Tab「被邀请人」：脱敏手机号、注册时间、状态（未实名 / 已实名 / 审核中 / 已失效）、累计贡献返利（分→元）、返利截止日。
- Tab「返利明细」：时间、被邀请人（脱敏）、订单号、消费金额、返利、状态（待发放 / 已发放 / 已作废；已发放且有扣回时显示「已扣回 ¥X」）。
- 手机端两 Tab 各自分页（`page_size 20`）。

### 8.3 其他页面
- `/account`：新增入口「邀请返利」（显示「本月返利 ¥X」小字，X > 0 时）；
- `/wallet`：流水类型新增「邀请返利」「返利扣回」标签与颜色（返利绿色、扣回橙色）；
- `/help`：新增一节「邀请返利」（规则、到账时间、扣回、上限、不可提现、禁止自刷）。
- 菜单（底部 / 侧栏）不加项，入口在「我的」。

## 9. 后台页面
- 菜单新增 `邀请返利 /referral`（`FIN_ROLES`）；`menu.spec.ts` 表同步。
- `/referral`（Tab）：
  - **概览**：待发放总额、本月发放、累计发放净额、欠账总额、搁置待审数；近 30 天每日发放折线（可省，表格也行）。
  - **待审核**：`status='held'` 的绑定（邀请人、被邀请人、标记、绑定时间、累计待发放）；行操作「通过」「撤销（原因必填 + TOTP）」。
  - **邀请人**：搜索（手机号 / ID）、筛选（状态、标记）、按累计发放 / 被邀请人数排序；行点击进详情抽屉：码、状态、欠账、被邀请人列表（含标记与状态）、返利列表、事件；操作「冻结（原因）」「解冻」。
  - **返利明细**：筛选（邀请人、被邀请人、状态、时间）、导出 CSV。
  - **配置**（super）：当前版本参数 + 历史版本表；「新增版本」表单（生效时间默认 now，TOTP）。
- `/users/:id`：新增「邀请」区块：邀请人（若有）、绑定状态、作为邀请人的统计（码、被邀请人数、累计发放净额、欠账）、链接到 `/referral` 详情。
- `/ledger`：类型筛选加两种。
- 仪表盘：finance 可见「本月已发放返利」卡片。

## 10. API

错误码（`apperr`）新增：`invite_code_invalid`（400「邀请码无效」）、`referral_frozen`（409「邀请功能已暂停」）、`referral_binding_state`（409「该绑定当前状态不允许此操作」）、`referral_config_invalid`（400，消息按字段）。

### 10.1 控制台（`/api/v1`）

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/auth/login` | body 增 `invite_code?`；头 `X-Device-Id?`；响应 `User` 增 `invite?: {bound, reason, inviter}` |
| GET | `/referral/codes/{code}` | 匿名，限流 60/min/IP → `{valid, inviter}` |
| GET | `/referral/me` | 需登录；**懒生成**码；→ `{code, link, status, kyc_verified, rebate: {rate_bp, period_days, hold_days, max_per_invitee, max_per_inviter_30d}, stats: {invitees, verified_invitees, pending, settled_net, month_settled, debt}}` |
| GET | `/referral/invitees?page=` | `{items:[{phone_masked, bound_at, status, kyc_verified, contributed, rebate_until}], total}`；`status` 映射：active→`有效`、held→`审核中`、revoked→`已失效` |
| GET | `/referral/rewards?page=&status=` | `{items:[{order_id, invitee_masked, paid_at, order_amount, refunded_amount, reward, status, settled_at, clawed_amount}], total}` |
| GET | `/me` | 增 `referral_month_settled`（分；0 时省略）——给「我的」页入口用，避免多一次请求 |

### 10.2 后台（`/admin/api/v1`）

| 方法 | 路径 | 角色 | 说明 |
|---|---|---|---|
| GET | `/referral/summary` | finance | 概览数字 |
| GET | `/referral/config` | finance | `{current, versions[]}` |
| POST | `/referral/config` | super + TOTP | `{rate_bp, max_per_invitee, max_per_inviter_30d, hold_days, period_days, effective_from?, note, totp}`；审计 `referral.config` |
| GET | `/referral/inviters?q=&status=&flag=&sort=&page=` | finance | 列表（每行带统计） |
| GET | `/referral/inviters/{user_id}` | finance | 详情（账户、绑定、返利、最近 50 条事件） |
| POST | `/referral/inviters/{user_id}/freeze` | finance | `{reason}`；审计 `referral.freeze` |
| POST | `/referral/inviters/{user_id}/unfreeze` | finance | 审计 `referral.unfreeze` |
| GET | `/referral/bindings?inviter_id=&invitee_id=&status=&flag=&from=&to=&page=` | finance | |
| POST | `/referral/bindings/{id}/approve` | finance | `held → active`，`{note}`；审计 `referral.approve` |
| POST | `/referral/bindings/{id}/revoke` | finance + TOTP | 任意非 revoked 状态 → `revoked`，待发放全部 `void`，`{reason, totp}` → `{voided_amount}`；审计 `referral.revoke` |
| GET | `/referral/rewards?inviter_id=&invitee_id=&status=&from=&to=&page=` | finance | |
| GET | `/referral/rewards/export?…` | finance | CSV：订单、邀请人、被邀请人、消费、退款、费率、应得、状态、发放时间、扣回、费率版本 |
| GET | `/referral/bindings/export?…` | finance | CSV：绑定 ID、邀请人、被邀请人、码、状态、标记、绑定时间、截止、IP、设备 |
| POST | `/referral/rescan` | super + TOTP | `{days}`（1～30）：`watermark = now − days`，立刻跑一轮 `Settle`；审计 `referral.rescan` |
| GET | `/users/{id}` | all | 响应增 `referral: {inviter?: {user_id, phone, binding_status}, account?: {code, status, invitees, settled_net, pending, debt}}` |
| GET | `/ledger` | finance | `kind` 筛选接受两种新类型 |
| GET | `/dashboard` | all | 增 `referral_month_settled`（finance 以外角色也返回数字，前端按角色显示） |

## 11. 模块与接口

新包 `internal/referral`：
```go
type Deps struct {
    Pool   *pgxpool.Pool
    Clock  clock.Clock
    Log    *slog.Logger
    Wallet WalletOps      // Apply / ApplyUpTo
    Users  UserLookup     // UserState(ctx, q, id) (status, kycStatus, phone, lastLoginIP, lastLoginAt)、HasDevice(ctx, q, userID, deviceID)、VerifiedFingerprints(ctx, q, userIDs) map[int64]string
    Orders OrderLookup    // OrderForReferral(ctx, q, orderID) (userID, scheduledStartAt *time.Time, err)
}
// 对外
func (s *Service) EnsureCode(ctx, userID) (Account, error)
func (s *Service) LookupCode(ctx, code) (valid bool, inviterMasked string, err error)
func (s *Service) Bind(ctx, tx, BindRequest) (BindResult, error)              // identity.Login 调用
func (s *Service) OnKYCVerified(ctx, tx, userID int64, fingerprint string) error  // identity KYC 调用
func (s *Service) Settle(ctx) error                                          // 清扫附加任务
func (s *Service) Me / Invitees / Rewards(...)                                // 控制台
func (s *Service) Admin*(...)                                                // 后台
```
- `wallet` 新增：`Kind` 常量 `KindReferralRebate / KindReferralClawback`、`ApplyUpTo`（§4.3）。**扫描查询（§5.1）由 referral 直接对 `wallet_ledger` 只读**——这是本计划唯一一处跨模块读表，规格明确允许：查询要同时 JOIN `referral_bindings` 并 anti-join `referral_events`，放在 `wallet` 里会让 wallet 反向依赖 referral 的表；而流水是只追加的全局事实表，读它不会绕过任何不变量。写路径仍全部经 `wallet.Apply / ApplyUpTo`。
- `identity`：`Login` 增 `invite_code / deviceID / userAgent` 参数与 `is_new`；`last_login_*`、`user_devices` 维护；KYC 两条路径写指纹并调 `OnKYCVerified`；`Deps` 增 `Referral ReferralHook`（接口，e2e / 单测可用 noop）。
- `rental`：实现 `OrderForReferral`（只读）。
- `adminapi` / `api`：新 handler 文件 `referral.go`；`permissions_test.go` 加路由。
- `app.go`：装配；`main.go` 把 `Referral.Settle` 加入附加任务。

## 12. 配置与常量

| 项 | 值 | 说明 |
|---|---|---|
| 首版费率（迁移写入） | 10% / ¥500 / ¥5,000 / 7 天 / 365 天 | R1～R3、R5；之后后台改 |
| `InviteStoreDays` | 30 | 控制台 localStorage（R7） |
| `ScanLookback` / `ScanLag` / `ScanBatch` | 1h / 2m / 500 | §5.1 |
| `ReleaseBatch` | 200 | §4.4 |
| `BurstThreshold` / `SameIPWindow` | 20 / 24h | §3.2 |
| `MaxDevicesPerUser` | 10 | §3.2 |
| 码长度 / 字母表 | 8 / Crockford（无 I L O U） | §3.1 |
| `codes/{code}` 限流 | 60/min/IP | 复用计划 6 的匿名限流器 |

全部常量在 `internal/referral/referral.go`；没有新环境变量（链接用 `SPARK_PUBLIC_BASE_URL`）。

## 13. 迁移与兼容
1. 迁移 00020 对存量数据：无回填；历史流水不会计返利（上线前没有任何绑定，扫描 JOIN 不命中）。
2. 发布顺序：platform 单独发布；多副本滚动期间旧副本的 `Login` 不认 `invite_code`（请求体多余字段被忽略）→ 滚动窗口内注册的用户可能没绑定；窗口很短，可接受（README 注明）。
3. 上线后执行一次 `platform referral backfill-fingerprints`（§2），之后同身份判定对存量实名用户生效。
4. 前端：老版本控制台没有 `X-Device-Id`，后端视为空（不打 `same_device`）。
5. 回滚：Down 在存在 `referral_*` 流水时拒绝（§2）；若必须回滚，先在后台冻结全部账户让发放停止，再评估。

## 14. 并发与正确性

- **资金只经 `wallet.Apply / ApplyUpTo`**，referral 不直接写 `wallet_accounts / wallet_ledger`；每笔发放 / 扣回都有唯一 `ref_id`（`referral/<order>`、`referral/<order>-c<n>`），`wallet_ledger_ref_uniq` 兜底重放。
- **发现幂等**：`referral_events.source_ledger_id` 唯一；同一条钱包流水在任何重扫 / 并发下只会产生一次计提或一次退款处理。处理事务失败时事件与状态一起回滚。
- **加锁顺序**（写进 `referral.go` 头注释）：

  | 路径 | 顺序 |
  |---|---|
  | Bind（在 identity 事务内） | users(被邀请人, INSERT 持有) → users(码主) FOR KEY SHARE → referral_accounts(码主) FOR SHARE → 插 referral_bindings |
  | 计提 / 退款处理 | referral_accounts(邀请人) FOR UPDATE → referral_rewards(订单) FOR UPDATE → [wallet_accounts(邀请人)，仅扣回] |
  | 发放 | referral_accounts(邀请人) FOR UPDATE → referral_rewards(订单) FOR UPDATE SKIP LOCKED → wallet_accounts(邀请人) |
  | 冻结 / 解冻 / 撤销 / 通过 | referral_accounts(邀请人) FOR UPDATE → referral_bindings → referral_rewards（撤销时批量 void） |
  | OnKYCVerified（在 identity 事务内） | users(本人, 已持有) → referral_accounts(邀请人) FOR UPDATE → referral_bindings → referral_rewards |
  | rental 的扣款 / 退款事务 | 不碰 referral 任何表（只写 wallet），与上面互不等待 |

  所有 referral 路径都从 `referral_accounts(邀请人)` 开始，之后只锁更细粒度的行与 `wallet_accounts`，而 rental / billing 的事务只锁 `wallet_accounts`（从不反过来锁 referral 表）→ 等待链不成环。`OnKYCVerified` 先持有 `users` 行锁再锁 `referral_accounts`，而 `Bind` 持有新用户行（别人不会等它），再显式 `FOR KEY SHARE` 码主的 `users` 行、最后 `FOR SHARE` 码主账户——同样是 users → accounts：插 `referral_bindings` 时外键检查本来也要对码主 `users` 行取 `KEY SHARE`，若放到账户锁之后就与码主本人的 `OnKYCVerified`（users `FOR UPDATE` → accounts `FOR UPDATE`）反向而死锁，所以提前显式加。与「冻结」（`FOR UPDATE`）互斥但不成环。
- **上限并发**：30 天 / 被邀请人上限在持有 `referral_accounts(邀请人)` 行锁时计算（§4.2），同一邀请人的计提串行。
- **不变量**（集成测试与 `make check` 断言，对每个邀请人 `v`）：
  1. `Σ wallet_ledger(v, referral_rebate) − Σ wallet_ledger(v, referral_clawback) == Σ(settled_amount − clawed_amount)(v) + debt(v)`；推导：入账 = Σ settled − Σ 抵销，扣款 = Σ clawed − Σ 欠账新增，而 `debt = Σ 欠账新增 − Σ 抵销`。等价表述：钱包净额 = 账面净应得 + 未抵销欠账。
  2. 每个 `settled` reward：`reward == settled_amount − clawed_amount`；每个 `pending` reward：`settled_amount == clawed_amount == 0`；每个 `void`：`reward` 可非零但无钱包流水。
  3. `refunded_amount(order) == Σ refund 流水(order)`（已处理部分）且 `≤ orders.amount`。
  4. 每条 `order_pay` 流水（被邀请人、绑定非 revoked、在返利期内）恰有一条 `accrue` 或 `capped(period_over)` 事件；每条 `refund` 流水（订单有 reward）恰有一条 `refund` 事件。
  5. 任一邀请人 30 天 `Σ settled_amount − clawed_amount ≤ max_per_inviter_30d`（按发放时版本）；任一绑定 `Σ reward ≤ max_per_invitee`。
- **时钟**：全部用注入的 `clock.Clock`；`hold_days` 与 30 天窗口按 UTC 时刻差，不按自然日。
- **重试**：处理单行遇 `40P01 / 40001` 整行重试一次；再失败记日志，下轮 `NOT EXISTS` 兜底。

## 15. 上线前核对（写入 `followups-plan8.md`）
- 执行 `platform referral backfill-fingerprints`。
- 法务：帮助页与邀请页的规则文案（不可提现、平台有权撤销异常返利）。
- 财务：线下退款核算口径（R4）。
- 运营：首版费率是否调整（后台可改）；是否需要公告。
- `SPARK_PUBLIC_BASE_URL` 必须是用户可访问的控制台地址（链接据此生成）。

## 16. 测试与验收

### 16.1 单元（Go，嵌入式 PG，假时钟）
- `wallet`：`ApplyUpTo` 余额充足 / 不足 / 为 0（不记流水）/ 重复 ref（`ErrDuplicate`）；新类型通过 `LedgerFilter`；Down 拒绝已有 referral 流水。
- `referral/code`：生成字母表与长度、撞码重试、规范化（`o→0`、`l→1`、空白、大小写）、`LookupCode` 对冻结 / 封禁码主返回无效。
- `identity.Login`：`is_new` 判定（首次 true、再次 false）；`last_login_*`、`user_devices` 维护与 10 台上限；带 `invite_code` 的新用户绑定、老用户不绑定（`existing_user`）、无效码不影响注册；`Bind` 的数据库错误回滚注册。
- `referral.Bind`：风险标记矩阵（同 IP 24h 内 / 超过 24h、同设备、第 20 个绑定触发 burst）；`same_device` → held；码主冻结 / 封禁 → `inviter_frozen`；`invitee_id` 唯一兜底；审计。
- `OnKYCVerified`：指纹写入（HMAC 可复现、不同密钥不同）；被邀请人与邀请人同指纹 → revoked + 待发放 void；两被邀请人同指纹 → 后者 revoked；邀请人后实名触发对其被邀请人的检查；企业信用代码同理；存量无指纹不误判。
- `ConfigAt`：版本选择（边界 `effective_from == t`）、`effective_from < now` 被拒、CHECK 范围、审计。
- 计提：费率取扣款时刻版本；`floor`；`reward_cap` 取三者最小；被邀请人上限累计（含 pending、不含 void）；邀请人 30 天上限含 pending；返利期后 `capped(period_over)`；预订单 `settle_after = 开始时刻`（> 7 天时）；`OrderLookup` 失败不写事件。
- 退款处理：`ref_id` 三种形态解析；pending 改数字；settled 扣回（余额充足 / 部分 / 为 0 → 欠账）；`-c1 / -c2` 序号；多次退款累计；订单无 reward 的退款忽略；`revoked` 绑定的退款仍扣回。
- 发放：到期发放、未到期不动；门槛（绑定 held / revoked、账户冻结、邀请人未实名 / 封禁）→ 跳过且下轮重试；`reward == 0` → void；欠账抵销（欠账 > 返利全抵、欠账 < 返利部分抵 + 入账）；`wallet_ledger_ref_uniq` 重放。
- 扫描：水位推进规则（< 500 行推进、= 500 不推进）；1 小时回看不重复处理（`source_ledger_id`）；非绑定用户流水不处理；`rescan` 拨回水位后不重复计提。
- 后台：冻结 / 解冻 / 通过 / 撤销（void 金额、TOTP 要求、状态机 `referral_binding_state`）；列表筛选与排序；CSV 字段；角色矩阵（`permissions_test.go`）；用户详情 `referral` 块。
- 控制台 API：`/referral/me` 懒生成只建一行（并发 10 次）；脱敏；分页；`codes/{code}` 限流。
- 不变量检查函数 `referral.CheckInvariants(ctx, pool)`（§14）供上面所有测试在结束时调用。
- 迁移：Up / Down 往返；Down 在有 referral 流水时拒绝。

### 16.2 集成（Go，`internal/app`，进程内 fake Agent）
- **并发资金**：1 个邀请人、5 个被邀请人（已实名、有余额），50 个 goroutine 随机执行：下单（1d）/ 续费 / 后台退款并释放（随机金额）/ 预订后取消 / 推进时钟 8 天 / 跑 `Settle` / 后台冻结解冻；60s 超时；结束后再推进 10 天跑 `Settle` 到静止 → `CheckInvariants` 全部成立、无死锁、`wallet_accounts.balance ≥ 0`、每条 `order` 流水恰处理一次。
- **全链路**：A 打开邀请页拿码 → B 带码注册（`is_new`）→ B 实名 → B 充值 ¥1000 → B 租 1 天（¥69）→ `Settle`：reward 690 分 pending，`settle_after = +7d` → 推进 7 天 `Settle` → A 余额 +690、流水 `referral_rebate`、`settled` → 后台对 B 的实例「退款并释放」退 ¥30 → `Settle`：target = floor(3900×0.1) = 390，扣回 300，A 余额 −300 → A 先把余额花光再退款的变体 → 欠账 300 → B 再租 1 天、7 天后发放 690 中 300 抵欠账、入账 390。
- **预订**：B 预订 3 天后开始的 1d → reward `settle_after = S`；B 冷静期取消 → `refund` 全额 → reward 0 → 到 S 时 void、A 无流水。
- **开通失败**：fake Agent 对 Create 回报失败 → `refundAll`（无后缀 ref）→ reward 0 → void。
- **自刷**：B 用与 A 相同的 `X-Device-Id` 注册 → held → 消费后到期不发放 → 后台 approve → 下一轮发放；C 用 A 的身份证号实名 → revoked，待发放 void。
- **上限**：费率版本改为 `max_per_invitee = 1000 分` → 第二笔只计 310 → 第三笔 `capped`；30 天上限同理并在推进 31 天后恢复。
- **返利期**：绑定时版本 `period_days = 1`，第 2 天的消费 `capped(period_over)`。

### 16.3 端到端（Playwright）
钩子：`/hooks/users` 增 `invite_code?`、`device_id?`（走真实 `Login` 路径以产生绑定）；新增 `POST /hooks/referral/config {rate_bp?, hold_days?, …}`（直接插一版 `effective_from = now`）；`/hooks/sweep` 已含 `Settle`。
覆盖表新增（登记到 `coverage.json`）：
- `C-REF-01`～`08`：带 `?ic=` 落地后登录页显示邀请人并注册成功提示「已绑定」；无效码静默；老用户带码登录不提示；邀请页显示码 / 链接 / 二维码并可复制；未实名提示条；被邀请人列表与脱敏；返利明细 pending → 推进 7 天 + sweep → 已发放且钱包流水「邀请返利」；后台退款后明细显示「已扣回」与钱包流水「返利扣回」。
- `C-ACC-03`：我的页「邀请返利」入口与本月金额。
- `C-WAL-05`：流水筛选新类型。
- `A-REF-01`～`07`：概览数字；待审核通过 / 撤销（TOTP、作废金额提示）；邀请人冻结 / 解冻（冻结后被邀请人注册不绑定）；配置新增版本（super、TOTP、追溯生效被拒）；返利明细筛选与 CSV 下载；用户详情邀请区块；`ops` 看不到菜单与接口 403。
- `A-FIN-05`：流水页筛选「邀请返利」。
- 注意：改时钟的用例按 `playwright.config.ts` 的顺序说明处理；返利用例推进时间 ≤ 8 天。

### 16.4 开发平台验收清单（Mac 192.168.1.218 `make run` + mock 支付 / 短信 / 实名；全部通过才算完成）
不需要真机（本计划不涉及 Agent）；实例用 fake Agent 或开发环境的单台真机（192.168.1.221）均可。
1. **分享**：账号 A 登录 → 我的 → 邀请返利 → 看到 8 位码、链接 `http://localhost:8080/?ic=…`、二维码；复制链接。
2. **注册绑定**：新浏览器（或隐私窗口，保证不同 `X-Device-Id`）打开链接 → 登录页显示「通过 1xx****xxxx 的邀请注册」→ 新手机号 B 注册 → toast「已绑定」；A 的邀请页「被邀请人」出现 B（未实名）。
3. **老用户不绑定**：已有账号 C 用链接登录 → 无提示、A 页无 C。
4. **计提**：B 实名（mock 通过）→ mock 充值 ¥200 → 租 1 天 ¥69 → 等一轮清扫（≤ 1 分钟）→ A 的返利明细出现 ¥6.90 待发放，到账时间 = 7 天后；后台 `/referral` 概览待发放 ¥6.90。
5. **发放**：开发库执行 `UPDATE referral_rewards SET settle_after = now()` → 下一轮清扫 → A 钱包 +¥6.90、流水「邀请返利 1xx****xxxx 订单 ord-…」；后台流水页可筛到。
6. **扣回**：后台对 B 的实例「退款并释放」退 ¥30 → 下一轮清扫 → A 钱包 −¥3.00「返利扣回」，明细显示应得 ¥3.90、已扣回 ¥3.00。
7. **欠账**：A 先把余额用掉（租 1 天）再重复 4～6（这次退全额 ¥69）→ A 余额不够扣 → 后台邀请人详情欠账 ¥6.90 → B 再租 1 天、发放时欠账抵销、A 实际入账 ¥0 且欠账清零（明细 settled ¥6.90、钱包无入账）。
8. **自刷**：同一浏览器（同 `X-Device-Id`）用新号 D 带 A 的码注册 → 后台待审核出现 D（`same_device`）；D 消费后到期不发放；后台「通过」→ 下一轮发放。用 B 的身份证号给新号 E 实名 → E 的绑定自动撤销（事件 `same_identity`），待发放作废。
9. **冻结**：后台冻结 A → 新号 F 带码注册提示「邀请码无效」且注册成功；A 的待发放到期不发放；解冻后下一轮发放。
10. **配置**：super 新增版本 15%、生效时间 now → 之后 B 的消费按 15%；之前的明细不变；尝试 `effective_from` 为昨天 → 被拒。
11. **导出**：返利明细与绑定 CSV 下载，Excel 打开中文不乱码，金额两位小数。
12. **不变量**：`go test ./internal/referral -run TestInvariantsAgainstDevDB`（连接开发库，只读）通过。

验收记录写到 `docs/superpowers/acceptance-plan8.md`，格式同计划 6。

## 17. 范围与遗留

**本计划做**：§2～§16 全部。

**明确不做（遗留清单）**
- 被邀请人的奖励（首充赠送 / 优惠券）；多级（二级）分销；团队 / 代理商结算。
- 返利提现、对公结算、发票与税务。
- 独立返利余额与抵扣优先级。
- 按充值返利、固定注册奖励、阶梯费率、按商品差异化费率。
- 注册后补填邀请码；更换邀请码；自定义邀请码；邀请海报生成。
- 短信 / 站内信 / 邮件通知（绑定成功、返利到账、月度汇总）。
- 定期全量同身份扫描；更强的设备指纹（Canvas / 字体）；手机号段 / 虚拟号识别；行为模型风控。
- 撤销时自动扣回已发放返利（现由财务调账）。
- 返利期到期前提醒；返利期延长 / 续约。
- 后台代用户生成码；运营活动码（非个人码）。
- 邀请页的 H5 分享卡片 / 微信小程序。
- 对账报表按费率版本汇总（CSV 已含版本列，可外部汇总）。

## 18. 任务拆分（供实施计划引用，18 项）
1. 迁移 00020（含 Down 拒绝条件）；`wallet` 新类型常量、`ledgerKinds`、`labels.ts`；`apperr` 新错误码。
2. `wallet.ApplyUpTo`；单测。
3. `referral` 包骨架：`Deps`、常量、码生成与规范化、`EnsureCode / LookupCode`、`ConfigAt`；单测。
4. `identity`：`Login` 的 `is_new / invite_code / device / last_login_*`、`user_devices`；`ReferralHook` 接口；单测。
5. `referral.Bind`（风险标记、held、审计）与 identity 接入；单测。
6. KYC 指纹（HKDF 派生、两条路径写入）+ `OnKYCVerified` 同身份撤销；`backfill-fingerprints` 子命令；单测。
7. `rental.OrderForReferral`；`referral` 计提（费率版本、上限、`settle_after`）；单测。
8. 退款处理与扣回 / 欠账；`ref_id` 解析；单测。
9. 发放（门槛、欠账抵销、`ref_id`）；单测。
10. 扫描与水位、`Settle` 编排、`CheckInvariants`；`main.go` / `e2ehooks` 接入；单测。
11. 后台裁决：冻结 / 解冻 / 通过 / 撤销 / 重扫（TOTP、审计）；单测。
12. 控制台 API（`auth/login` 扩展、`codes/{code}` 限流、`referral/me / invitees / rewards`、`/me` 字段）；单测。
13. 后台 API（summary / config / inviters / bindings / rewards / exports / users 详情块 / ledger 筛选 / dashboard）；`permissions_test.go`。
14. 集成测试（§16.2 全部）。
15. 控制台：落地参数、`X-Device-Id`、登录页邀请提示、`/invite` 页、我的页入口、钱包标签、帮助页。
16. 后台：`/referral` 五个 Tab、用户详情区块、流水筛选、仪表盘卡片、菜单与 `menu.spec.ts`。
17. e2e：钩子、Playwright 用例、`coverage.json`。
18. 开发平台验收（§16.4）与 `acceptance-plan8.md`、`followups-plan8.md`、README（配置 / 部署注意 / 子命令）。
