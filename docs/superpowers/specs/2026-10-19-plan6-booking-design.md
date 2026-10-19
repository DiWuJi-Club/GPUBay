# Spark Cloud — 计划 6：档期预订、机组、一次多台与自动续费设计文档

- 日期：2026-10-19
- 状态：已批准（Fable 评审 2026-10-19；负责人已授权由设计方裁定、评审通过即定稿，不再逐项询问负责人；代负责人作出的裁定集中列在 §1.3，评审意见处理见附录）
- 上位文档：`2026-10-14-spark-cloud-mvp-design.md`（下称「总设计」）§0.1 第 10 条、§2 `machine_leases`、§11、附录竞品调研；`2026-10-18-plan3-access-design.md`（下称「计划 3」）§2.3、§4.1；`2026-10-17-plan5-frontend-design.md`（下称「计划 5」）§4.3、§4.4、§5.4、§5.6、§7
- 范围：用户按机组（或指定机器）查看档期日历、预订未来起租的实例、一单租多台（原子）、自动续费与长期租约；运营管理机组、查看与处置预订；预订与现有即时分配 / 维护 / 换机 / 失联 / 保留期 / 接入层的衔接；迁移、API、控制台与后台页面、测试与真机验收。
- 不在范围：见 §17。

## 0. 现状与约束（实现时不必再查）

**代码事实（`main` 上计划 1～5 已合并，分支 `feat/plan6-booking`）**
- `machine_leases`（`migrations/00007_rental.sql`）：`machine_id`、`instance_id NOT NULL`、`period tstzrange`、`status ∈ {reserved, active, ended, cancelled}`，`EXCLUDE USING gist (machine_id WITH =, period WITH &&) WHERE (status IN ('reserved','active'))`，扩展 `btree_gist` 已装。目前每个实例只有一条 `active` 时段，`period = [创建时刻, occupiedUntil)`，`occupiedUntil`（`internal/rental/leases.go`）= 到期 + `RetainAfterStop`(6h) + `WipeBuffer`(1h)，creating / stopped 带 pending 时按 `CreateTimeout`(20m) + pending + 7h 估算；`refreshLease` 在每次状态变化后把上界改为 `GREATEST(lower, occupiedUntil)`，碰到排他约束返回 `ErrNoCapacity`（目前不可能发生，因为一台机器同时只有一个实例）。`status='reserved'` 的值从未被写入。计划 1 遗留「失联期间上界不随时间推进」与「`occupiedUntil` 回退为 `now` 可能产生空时段」归本计划。
- 分配：`fleet.Allocate`（`internal/fleet/machines.go`）`UPDATE machines SET state='allocated' WHERE id = (SELECT id FROM machines WHERE product_id=$1 AND state='idle' AND agent_online ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED)`，**不看 `machine_leases`**；被 `rental.Place` 与 `rental.AdminMigrate` 调用。`machines.state ∈ {idle, allocated, maintenance, faulty}`；`Release` 只把 `allocated` 改回 `idle`；`SetMaintenanceIn` 结束维护前 `rental.SetMachineMaintenance` 检查机器上没有未结束实例。`machines` 有 `rack`、`slot`，没有机组、没有对外展示名，后台没有修改已登记机器的接口（计划 4 遗留）。
- 实例：`instances.state` 的 CHECK 只允许 `creating / running / stopped / wiping / released / failed`；`instances_machine_active_uniq` 对 `creating / running / stopped / wiping` 生效；`machine_id NOT NULL`、`ssh_password_enc NOT NULL`、`ssh_host_pubkey` / `ssh_host_key_enc` 在 `Place` 时生成（计划 3）。计时从 `startLease` 开始：`expires_at = now + pending_seconds`。`Renew` / `AdminExtend` 走 `addTime`（running 延长 `expires_at`，creating / stopped 加 `pending_seconds`，stopped 下发 Start），之后 `refreshLease`。`Release` 不退款。`failCreating`：实例 failed、`refundAll`、机器 faulty、告警 `create_failed`。
- 订单：`orders` 一单对应一个实例（`instance_id NOT NULL`），`kind ∈ {new, renew}`，`duration ∈ {1d, 1w, 1m}`（CHECK），`amount = 当前价`，`refunded_amount / refund_count`（00015），`idempotency_key` 按 `(user_id, key)` 唯一（00008）。`charge` 用 `Catalog.Quote` 取价；`refund.go` 的 `quote` 用 `seconds(latest.duration)` 折算单价，`refundAll` 用 `seconds(o.duration)` 累计自付秒数——**两处都假设一单一个租期单位**。幂等：`lockIdempotencyKey` 先取事务级咨询锁 `idem:<user>:<key>`，再查 `orders`；并发冲突由唯一索引兜底后 `replayPlace` / `replayRenew`。
- 钱包：`wallet.Apply` 在调用方事务内锁 `wallet_accounts` 行、余额不足 `ErrInsufficientBalance`，`wallet_ledger_ref_uniq (kind, ref_type, ref_id)` 防重复记账；没有「冻结 / 预授权」概念，一切都是立即扣款与退款。
- 目录：`catalog.Duration` 只有 `1d / 1w / 1m`（1m = 30 天），`prices (product_id, duration)` 主键，后台 `SetPrice`。
- 清扫器：`rental.Sweep` 每分钟（`SPARK_SWEEP_INTERVAL`）在咨询锁下顺序执行：离线判定 → 暂停 → 恢复 → `ExpireDue` → `WipeStale` → `FailStuckCreating` → `FailTimedOutCommands` → `ReapSSHSessions` → `SendReminders`，再跑附加任务（`Billing.Sweep`）。`sweepEach` 先无锁选候选、再逐个 `FOR UPDATE SKIP LOCKED` 复核。e2e-server 不启动清扫器，由 `POST /hooks/sweep` 触发（默认先对全部机器心跳）。
- 加锁顺序（`internal/rental/service.go` 头注释）：`instance → agent_commands → orders → wallet`；`machine → wallet`；`AdminMigrate` 是 `原实例 → 新机器 → 指令`。`CancelCommands` / `ClaimPending` / `TouchSent` 按 `id` 排序锁指令行。
- 通知：`rental.Notifier.NotifyExpiry`（`internal/app/app.go` 的 `expiryNotifier` 经短信插件 `sms.TemplateExpiryReminder` 发送，aliyun / mock），24h、1h 各一次。没有站内信、没有邮件。
- 接入层（计划 3）：`ssh_command` 只在 running 输出；sshgw `BannerCallback` 按实例状态给中文说明（§2.3 表），ingress 对非 running 返回 503 页；端口在非 running 状态也可管理。
- 后台：`GET /machines/{id}` 返回 `current_instance` 与 20 条历史；`GET /instances` 按 `state / user_id / machine_id` 筛选，`instanceStates` 列表写死；仪表盘 `DailyOccupancy` 按 `machine_leases` 的 `active / ended` 时段算出租率；审计 `audit.Actor{AdminID, UserID, IP}`；资金操作（退款并释放、调账）走 `stepUp` TOTP（`adminapi.handleRefundRelease`）。角色 `super / ops / finance`，路由表 `allRoles / opsRoles / financeRoles`。
- 前端：控制台路由 `/market`、`/instances`、`/instances/:id`、`/wallet`、`/account/*`、`/help`；市场页每个商品一张卡片内选租期与镜像，`useIdempotencyKey` 生成 `Idempotency-Key`；实例视图 `Instance` 类型在 `web/shared/src/types/instance.ts`；状态中文映射在 `web/shared/src/labels.ts`。后台菜单 `web/admin/src/menu.ts`（`menu.spec.ts` 表驱动核对路由 meta.roles）。
- e2e：Playwright 单 worker、共享假时钟；钩子 `/hooks/clock/advance`、`/hooks/sweep`、`/hooks/agent`（完成全部待处理指令）、`/hooks/machines {count, serial_prefix?, rack?}`（登记 + 结束维护 + 真实接入 fake Agent）、`/hooks/users`、`/hooks/admins`、`/hooks/ssh`。覆盖表 ID 已到 `C-DET-16`、`C-ING-06`、`C-KEY-05`、`A-INS-12`、`A-USR-07`、`A-MCH-09`。
- 迁移编号到 `00018_access.sql`；本计划是 `00019_booking.sql`（带 `+goose Down`）。
- 真机：只有一台 DGX Spark（192.168.1.221，`spark-3479`），平台跑在 Mac 192.168.1.218；多机行为只能用 fake Agent 在 Go 集成测试与 e2e 里验证。

**竞品（总设计附录）**：每台机器占用日历；最多提前 60 天、单次最长 90 天、一次最多 8 台；机组；自动续费；长期租约；洗机中状态。

## 1. 架构与核心决策

```
                 下单 / 预订（控制台）                    后台（机组、预订处置）
                        │                                       │
                        ▼                                       ▼
┌──────────────────────── rental（占用时段是唯一真相）────────────────────────┐
│  PlaceBatch：候选机器 = 机组 ∩ 商品 ∩ 状态允许 ∩ 时段无重叠（lease-aware）        │
│             按机器逐台 savepoint 插 lease；排他约束冲突 → 换下一台；N 台全成或全败  │
│  machine_leases：reserved（预订，未开通）→ active（开通起）→ ended / cancelled    │
│  清扫器新增：ProvisionDue（开始前 1h 开通 / 改派 / 超时取消）、AutoRenewDue        │
│             RefreshCappedLeases（失联 / 停机实例上界随时间推进，但不越过下一档期）   │
│  续费（手动 / 自动 / 后台延长）：到期 + 7h 尾段 ≤ 同机下一预订的下界，否则 renew_blocked│
└───────────────────────────────────────────────────────────────────────────┘
                        │ 指令（不变：Create / Start / Stop / Wipe）
                        ▼
                    agentgw / Agent（本计划无协议改动）
```

### 1.1 决策一览

| # | 决策 | 结论 | 理由 |
|---|---|---|---|
| 1 | 预订的载体 | **预订 = 一条 `status='reserved'` 的 `machine_leases` + 一个 `state='reserved'` 的实例行**，下单时就把机器钉死 | `machine_leases.instance_id NOT NULL` 且排他约束按机器判重叠，只有钉机器才能用数据库保证不超卖；用户看到的「我的实例」列表天然包含预订；开通时沿用现有 `creating → running` 链路，实例 ID 从预订起就不变 |
| 2 | 可否指定机器 | **允许**（竞品有）。指定机器与指定机组都只是「下单时的候选集」；故障时同机组改派，用户事先被告知 | 不允许会让「每台机器的日历」失去意义；允许但不承诺，运维才有处置余地 |
| 3 | 开通时机 | 开始前 **1 小时**（`PrepLead`）开通；开通成功早于开始时刻也立即可用（不计费）；到期 = `max(开始, 实际开通) + 时长`，迟开通的补偿以不侵占下一档期为限（§5.3） | 20 分钟开通超时 + 一次改派仍能在开始前完成；「开机准备时间不计入」与总设计一致 |
| 4 | 一次多台 | **一个批次（`rental_batches`）= N 条订单 + N 个实例**，同一事务全成或全败；订单仍一单一实例 | 退款、换机、对账、后台订单页全部沿用一单一实例的既有逻辑；只新增批次 ID 用于幂等与展示 |
| 5 | 租期与长期租约 | 保留 `1d / 1w / 1m` 三档，**新增数量 `quantity`**（`1d×1~90`、`1w×1~12`、`1m×1~3`，单次 ≤ 90 天），价格 = 单价 × 数量；长期租约 = `1m×N` + 自动续费；**本计划不加多月折扣价** | 不改价格表结构；折扣是运营策略（后台改 1m 单价即可），加一档就要同时改退款折算、后台价格页与 e2e |
| 6 | 预订付款 | **下单即全额扣款**（与即时租用一致），没有冻结 / 预授权 | 钱包没有 hold 概念；全额预付天然抑制占坑 |
| 7 | 取消 | 用户可在**开通前**（开始前 1h 之前）取消：下单 15 分钟内全额退（冷静期）；之后距开始 ≥ 24h 全额退，< 24h 退 50%；开通后按普通实例处理（释放不退款）；平台取消（无法履约、后台取消、用户失去租用资格）一律全额退 | 两档规则够简单，也让临近取消有成本；冷静期兜住误操作；平台原因不让用户承担 |
| 8 | 续费 vs 预订冲突 | 任何加时（手动续费、自动续费、后台延长、停机期续费）都要求 `新到期 + 7h ≤ 同机下一条 reserved 的下界`，不满足返回 `renew_blocked`；实例视图带 `renewable_until`，控制台在续费前就显示「可续至」 | 预订是已付款的承诺，不能被续费挤掉；提前展示避免用户临到期才发现 |
| 9 | 自动续费 | 到期前 **1 小时**扣款；余额不足每分钟重试到到期为止，首次失败发一条短信并在控制台置顶提示；被下一预订挡住时**自动降档**（1m → 1w → 1d 取能放下的最大档）仍不行则失败并说明 | 1 小时给用户补充余额的窗口；降档比直接停机对用户更友好，且订单与短信都写明实际档位 |
| 10 | 即时租用 | 保持「立即分配」语义，但分配改为 **lease-aware**（不把带近期预订的机器分给放不下的即时订单），并同样支持数量与一次多台 | 否则即时订单会撞排他约束整单失败，或抢走预订机器 |
| 11 | 失联 / 停机实例占用上界 | 上界随每轮清扫推进，但**封顶于同机下一条 reserved 的下界**；到开通时刻机器仍被占着 → 预订改派，老租户不动 | 预订已付款并有时间承诺；老租户的问题由现有「换机 / 退款」处置 |
| 12 | 改派无机器 | 开始前 1h 起每分钟重试；到 `开始 + 30 分钟`（`BookingGrace`）仍无机器 → 平台取消、全额退款、告警 `booking_unfulfilled`、短信通知 | 不让预订无限期悬着；30 分钟给运维解维护 / 修机器的机会（告警 `booking_at_risk` 在首次改派失败时就发） |
| 13 | 通知渠道 | 只有短信（已有插件）。新增两个模板：自动续费失败、预订被平台取消；预订开通成功不发短信，控制台可见 | 与资金相关的两件事必须主动告知；其他信息控制台足够 |

### 1.2 不做的替代方案（评审时不必再提）
- 「预订不钉机器、开通时再分配」：需要自建容量模型并与排他约束脱钩，超卖只能靠应用层计数；放弃。
- 「预订走独立的 `bookings` 表、开通时才建实例」：`machine_leases.instance_id` 外键、控制台列表、后台实例页都要分叉；放弃。
- 「一单多实例（`orders.instance_id` 可空 + `order_items`）」：退款（`refundAcross`）、换机链路（`instance_transfers` 只认实例）、财务导出都要改；放弃。
- 「冻结余额、开通时再扣」：钱包要加 hold 账户与过期释放；放弃。
- 「自动续费改用 `expires_at` 到时再扣」：失败就直接停机，没有补余额的窗口；放弃。

### 1.3 代业主裁定（负责人已授权，事后汇报）

| # | 裁定 | 理由 |
|---|---|---|
| B1 | 预订参数：最早 **2 小时后**开始、最多提前 **60 天**、单次 **≤ 90 天**、一单 **≤ 8 台**、每用户未开通预订 **≤ 16 个实例**；开始时刻取**整点**（Asia/Shanghai） | 60 / 90 / 8 对齐竞品；2 小时保证开通点（开始前 1h）至少在下单 1h 后；16 个防占坑（已付款，上限主要防误操作）；整点让日历简单 |
| B2 | 允许指定机器，但只承诺机组：故障 / 占用时同机组改派并在控制台与事件里注明；「不限机组」的预订可改派到任意**可见**机组 | 见 §1.1 第 2 条 |
| B3 | 租期沿用三档 × 数量，不加多月折扣价；长期租约 = `1m×N` + 自动续费 | 见 §1.1 第 5 条 |
| B4 | 取消退款：**下单后 15 分钟内（冷静期）一律全退**；此后距开始 ≥ 24h 全退，< 24h 退 50%；开通（开始前 1h）后不可取消；平台原因全退 | 见 §1.1 第 7 条；冷静期是评审建议（S1）：最早只能订 2 小时后，没有冷静期的话临近预订从下单那一秒起就是 50% 档，选错镜像 1 分钟内取消也要损失一半 |
| B5 | 自动续费在到期前 1h 扣款、余额不足重试到到期、被预订挡住时自动降档 | 见 §1.1 第 9 条 |
| B6 | 预订付款即全额扣款，不做冻结 | 见 §1.1 第 6 条 |
| B7 | 后台取消预订需要 ops 角色 + 动态码（会退款）；改派机器只需 ops；机组管理 ops | 与现有「涉及资金走 TOTP」一致 |
| B8 | 机器对外展示名 `display_name` 由后台设置（默认空 → 控制台显示 `机器 #<id>`），序列号与内网 IP 不对用户展示 | 用户需要一个稳定的名字看日历，序列号属于资产信息 |
| B9 | 未分组的机器只参与「不限机组」的分配，不出现在机组列表；机组可设「仅后台可见」（`visible=false`，用于留给运维 / 大客户） | 让分组上线可以渐进，不要求一次性把 200 台都分好 |
| B10 | 即时订单也用「租期 × 数量」并受 90 天上限约束；现有 `POST /orders` 保留并等价于 `count=1, quantity=1, 立即开始` | 一套下单逻辑；老接口不破坏 |
| B11 | 预订开通成功不发短信；自动续费失败与平台取消预订发短信 | 见 §1.1 第 13 条 |
| B12 | 机器进入维护时若有未开通的预订，后台弹出提示（数量 + 最早开始时间）但允许进入；预订会在开通时自动改派 | 维护往往是紧急的，不能被预订卡住；改派机制兜底 |

## 2. 数据模型（迁移 `00019_booking.sql`，带 `+goose Down`）

```sql
-- 机组
CREATE TABLE machine_groups (
    id          BIGSERIAL PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,            -- 对用户展示，1～32 字符
    region      TEXT NOT NULL DEFAULT '',        -- 展示用位置标签（机房 / 机柜区），≤ 32 字符
    description TEXT NOT NULL DEFAULT '',        -- ≤ 200 字符
    sort        INT  NOT NULL DEFAULT 0,
    visible     BOOLEAN NOT NULL DEFAULT true,   -- false：控制台不展示，仅后台可见与分配
    created_at  TIMESTAMPTZ NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL
);

ALTER TABLE machines
    ADD COLUMN group_id     BIGINT REFERENCES machine_groups (id),   -- NULL = 未分组
    ADD COLUMN display_name TEXT NOT NULL DEFAULT '';                  -- 对用户展示名，≤ 32 字符
CREATE INDEX machines_group_idx ON machines (group_id, id);
CREATE UNIQUE INDEX machines_display_name_uniq ON machines (display_name) WHERE display_name <> '';  -- 后台返回 duplicate_display_name

-- 批次：一次下单的 N 台
CREATE TABLE rental_batches (
    id              TEXT PRIMARY KEY,            -- 'bat-' + 12 位
    user_id         BIGINT NOT NULL REFERENCES users (id),
    count           INT NOT NULL CHECK (count BETWEEN 1 AND 8),
    idempotency_key TEXT,
    created_at      TIMESTAMPTZ NOT NULL
);
CREATE UNIQUE INDEX rental_batches_user_idempotency_uniq ON rental_batches (user_id, idempotency_key) WHERE idempotency_key IS NOT NULL;

ALTER TABLE orders
    ADD COLUMN batch_id TEXT REFERENCES rental_batches (id),
    ADD COLUMN quantity INT NOT NULL DEFAULT 1 CHECK (quantity BETWEEN 1 AND 90),  -- 租期单位数：时长 = duration × quantity
    ADD COLUMN auto     BOOLEAN NOT NULL DEFAULT false;                              -- 自动续费产生的续费单
CREATE INDEX orders_batch_idx ON orders (batch_id) WHERE batch_id IS NOT NULL;

ALTER TABLE instances DROP CONSTRAINT instances_state_check;
ALTER TABLE instances ADD CONSTRAINT instances_state_check
    CHECK (state IN ('reserved', 'creating', 'running', 'stopped', 'wiping', 'released', 'failed', 'cancelled'));
ALTER TABLE instances
    ADD COLUMN scheduled_start_at      TIMESTAMPTZ,                 -- 预订开始时刻；即时订单为 NULL
    ADD COLUMN group_id                BIGINT REFERENCES machine_groups (id),  -- 下单时选择的机组（改派范围）；NULL = 不限
    ADD COLUMN batch_id                TEXT REFERENCES rental_batches (id),
    ADD COLUMN auto_renew              BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN auto_renew_duration     TEXT NOT NULL DEFAULT '1m' CHECK (auto_renew_duration IN ('1d', '1w', '1m')),
    ADD COLUMN auto_renew_failed_at    TIMESTAMPTZ,                 -- 最近一次自动续费失败（成功后清空）
    ADD COLUMN auto_renew_failed_reason TEXT NOT NULL DEFAULT '',   -- insufficient_balance / renew_blocked
    ADD COLUMN reassign_count          INT NOT NULL DEFAULT 0,      -- 改派次数（控制台据此提示「已为您更换机器」）
    ADD COLUMN provisioned_at          TIMESTAMPTZ,                 -- 预订开通（reserved → creating）时刻；即时订单为 NULL
    ADD COLUMN cancelled_at            TIMESTAMPTZ;
CREATE INDEX instances_reserved_idx ON instances (scheduled_start_at) WHERE state = 'reserved';
CREATE INDEX instances_auto_renew_idx ON instances (expires_at) WHERE state = 'running' AND auto_renew;

-- machine_leases：status 已有 reserved；补索引供日历与开通扫描。
-- 先清掉 occupiedUntil 回退为 now 产生的空时段（isempty 的 range 没有 lower，用 created_at 代替），否则下面的 CHECK 建不起来
UPDATE machine_leases SET period = tstzrange(created_at, created_at + interval '1 second'), status = 'ended' WHERE isempty(period);
CREATE INDEX machine_leases_machine_period_idx ON machine_leases (machine_id, lower(period)) WHERE status IN ('reserved', 'active');
CREATE INDEX machine_leases_reserved_idx ON machine_leases (lower(period)) WHERE status = 'reserved';
ALTER TABLE machine_leases ADD CONSTRAINT machine_leases_period_nonempty CHECK (NOT isempty(period) AND lower(period) < upper(period));
```

说明：
- 排他约束不改：`reserved` 本来就在 `WHERE status IN ('reserved','active')` 内。
- `machine_leases_period_nonempty` 顺带关闭计划 1 遗留「`occupiedUntil` 回退为 `now` 产生空时段」：`occupiedUntil` 改为在所有分支至少返回 `now + 1 秒`，并在 `refreshLease` 用 `GREATEST(lower(period) + interval '1 second', …)`。
- 实例新状态：`reserved`（已预订，未开通）、`cancelled`（开通前取消，终态）。`instances_machine_active_uniq` **不含** `reserved`——同一机器可同时挂多条不同时段的预订；`creating` 起一台机器仍只允许一个实例。
- 所有涉及状态枚举的代码位置要同步：`rental.instanceStates`（后台筛选）、`web/shared/src/labels.ts`（中文：`reserved` 已预订、`cancelled` 已取消）、`web/shared/src/types/instance.ts`；`rental.ListIngressRoutes`（`ports.go`）的状态列表加 `reserved`（否则预订实例的端口不进路由表，ingress 会回 404 而不是状态页；`cancelled` 不加，终态按 410）；`ingress.NotRunningPage` 显式加 `reserved` 分支；`sshgw.stateBanner`（`auth.go`）显式加 `reserved` 分支（**不能靠 `default`**，默认分支显示的是「实例已释放」）；sshgw 会话扫描的「终态集合」加 `cancelled`；Banner / 页面里的 `<S>` 用 `Asia/Shanghai` 格式化（同 `BannerStoppedFmt`）；`reconcile.go` 的「预期实例」查询保持只查四个活动状态（`reserved` 的容器不存在是正常的）。
- `orders.kind` 不加值：预订单与即时单都是 `new`，区别在 `instances.scheduled_start_at`；自动续费单 `kind='renew', auto=true`。
- **Down**：先 `DO $$ … IF EXISTS (SELECT 1 FROM instances WHERE state = 'reserved') THEN RAISE EXCEPTION '仍有未开通的预订（已付款未退），请先在后台取消' … $$`——`reserved` 实例有已付未退订单，静默改成 `failed` 会让账面看起来像「开通失败已退款」；`cancelled` 实例（已退款）映射为 `failed`，其 lease 已是 `cancelled`。然后按外键顺序：删 `machine_leases_period_nonempty` 与新索引 → `instances` / `orders` 删新列（含 `group_id`、`batch_id`）→ `machines` 删 `group_id`、`display_name` → `DROP TABLE rental_batches, machine_groups` → 恢复旧 CHECK。Down 有损（cancelled → failed），迁移文件头注释写明。

**存量数据**：现有实例全部是即时订单（`scheduled_start_at NULL`，`quantity=1`，`auto_renew=false`），`machine_leases` 现有行语义不变；现有 `machines.group_id` 为 NULL，按 B9 只参与「不限机组」分配——上线当天控制台市场页行为与今天一致（§15）。

## 3. 机组

### 3.1 规则
- 机组是机器的可选标签：一台机器最多属于一个机组；未分组机器只在「不限机组」下分配（B9）。
- 机组跨商品无约束（分组是位置 / 运营概念），但候选集查询总是先按商品过滤。
- 删除机组要求组内没有机器，且没有 `instances.group_id` 指向它的**未终态**实例（reserved / creating / running / stopped / wiping）；否则 `invalid_state`。终态实例保留 `group_id`（历史），删除时置 NULL（`ON DELETE` 不用级联，服务层先检查再 `UPDATE … SET group_id = NULL`）。
- `visible=false` 的机组：控制台 `GET /groups` 不返回，也不能作为 `group_id` 下单（`not_found`）；其机器**不参与**「不限机组」分配（这是留给运维 / 大客户的池子）。后台可见并可改派到其中。
- 改变机器的机组：允许随时改（审计 `machine.update`）；已有预订的 `instances.group_id` 不跟着变（它记录的是用户下单时的选择，改派范围按它算）；运维若想把预订一起挪走用「改派」。

### 3.2 后台管理（§10.2）
- 机组列表 / 新建 / 编辑 / 删除；机器加入 / 移出（单台在机器详情改；批量在机组页按机器 ID 列表或按机柜筛选勾选）。
- 机器编辑接口只开放 `group_id` 与 `display_name` 两个字段（计划 4 遗留的「修改已登记机器」只做这一点；机柜 / IP / PDU 仍不开放，理由同计划 4）。

### 3.3 控制台展示
- 市场页下单对话框里的机组选择：`不限机组` + 可见机组列表，每项显示 `名称 · region`、`共 N 台`、`现在空闲 X 台`（`idle AND agent_online`）；选定时段后显示 `该时段可预订 Y 台`（§4.4）。
- 指定机器：选了机组且数量为 1 时可展开机器列表（`display_name` 或 `机器 #id`，显示该机器 60 天内的占用条带）。
- 实例视图带 `group_name`（下单时选的机组，可能为空）与 `machine_label`（当前机器展示名）。

## 4. 档期与分配算法

### 4.1 时段定义
对一个「起租时刻 `S`、时长 `D` 秒」的租约，机器被占用的时段是：

```
period = [ S - PrepLead,  S + D + Tail )
PrepLead = 1h          // 开始前开通
Tail     = RetainAfterStop (6h) + WipeBuffer (1h) = 7h
```

- 即时订单：`S = now`，`PrepLead` 取 0，开通阶段的估算上界仍按现状多加 `CreateTimeout`（`period = [now, now + CreateTimeout + D + Tail)`，进入 running 后 `refreshLease` 收缩为 `expires + Tail`），避免开通慢的实例在尾段与紧随其后的预订相撞。
- 预订：`lower = S - 1h` 是「开通时刻」，也是用户可取消的截止时刻。
- 续费 / 延长：把 `upper` 推到 `新到期 + Tail`。
- 停机保留 / 洗机：`upper` 不变（本来就包含 7h 尾段）；洗机晚于尾段结束（机器 faulty、洗机失败）时由 §4.3 的封顶与 §5.4 的改派处理。

### 4.2 候选机器与挑选（`rental.pickMachines`）

```sql
-- 参数：product_id, group_id (NULL=不限), machine_id (NULL=不指定), period, want (1..8), immediate (bool)
SELECT m.id FROM machines m
LEFT JOIN machine_groups g ON g.id = m.group_id
WHERE m.product_id = $1
  AND ($2::bigint IS NULL AND (m.group_id IS NULL OR g.visible) OR m.group_id = $2)
  AND ($3::bigint IS NULL OR m.id = $3)
  AND m.state IN ('idle', 'allocated')                -- 即时订单再加 AND m.state = 'idle' AND m.agent_online
  AND NOT EXISTS (SELECT 1 FROM machine_leases l
                  WHERE l.machine_id = m.id AND l.status IN ('reserved', 'active') AND l.period && $4)
ORDER BY
  (SELECT max(upper(l.period)) FROM machine_leases l            -- 最紧贴前一档期的机器优先（减少碎片）
     WHERE l.machine_id = m.id AND l.status IN ('reserved','active','ended') AND upper(l.period) <= lower($4)) DESC NULLS LAST,
  m.id
LIMIT $5 FOR NO KEY UPDATE OF m SKIP LOCKED   -- 不能用 FOR UPDATE：插入引用 machines(id) 的行会取 FOR KEY SHARE，与 FOR UPDATE 互斥（计划评审 M1）
```

- 预订允许候选 `allocated`（现租户会在时段开始前结束，由 lease 判断）与离线机器（开通时再看在线）；即时订单要求 `idle AND agent_online`（与现状一致）。`maintenance / faulty` 一律不是候选。
- 挑出的机器行已加锁（按查询顺序取锁，机器 ID 不保证递增；但同一事务对每台机器只锁一次且 `SKIP LOCKED`，两个并发批次不会互相等待——它们要么跳过对方锁住的机器，要么拿到不同机器；详见 §14）。跳过被锁行导致挑到的台数不足时，短暂退避（20～80ms 抖动）后重挑，一次分配最多 3 次（实施中裁定：否则并发下单时空闲机器只是被别的事务短暂锁住，也会报假的 `no_capacity`）。
- 对每台候选机器：`SAVEPOINT` → 插入实例行 → 插入 lease（`reserved` 或 `active`）→ 若排他约束冲突（并发事务刚提交了重叠时段，`NOT EXISTS` 没看到）→ `ROLLBACK TO SAVEPOINT`，记为失败并从候选里剔除。候选不够 `want` 台时再用同一查询补取（`LIMIT 剩余台数`，排除已试过的 ID），最多补取 3 轮；仍不足 → 整个事务回滚，`no_capacity`（消息带可用台数：`该时段只有 X 台可用`）。
- 即时订单同时 `UPDATE machines SET state='allocated'`（现 `fleet.Allocate` 的后半段）；预订不改机器状态。`fleet.Allocate` 改为调用 `rental.pickMachines` 的即时分支（`AdminMigrate` 继续用它，period 为 `[now, now + 剩余 + Tail)`），不再有「不看 lease」的分配路径。

### 4.3 时段上界封顶（关闭计划 1 遗留）
`refreshLease` 改为：

```
desired = occupiedUntil(inst, now)                 -- 至少 now + 1s
cap     = (SELECT min(lower(period)) FROM machine_leases WHERE machine_id = inst.machine_id
            AND status = 'reserved' AND instance_id <> inst.id AND lower(period) >= lower(own.period))
upper   = GREATEST(lower(own.period) + 1s, LEAST(desired, cap))     -- cap 为 NULL 时不封顶
```

- 上界不越过已提交的下一条预订。唯一的例外是并发：`cap` 按语句快照计算，另一个事务正在插入、尚未提交的预订看不到，UPDATE 会等它提交后撞排他约束。所以 UPDATE 放在保存点里（`updateLeaseCapped`），撞约束时回滚保存点**重试一次**（新语句的快照能看到已提交的预订，封顶生效）；仍冲突才返回 `ErrNoCapacity`（实施中修订，计划 6 Task 2 评审）。结果与现值相同时不写。
- 新清扫任务 `RefreshCappedLeases`：对 `running AND suspended_at IS NOT NULL`、`stopped` 与 `wiping` 的实例每轮 `refreshLease`（`wiping` 的 `occupiedUntil = now + WipeBuffer` 只在 `beginWipe` 写过一次，机器离线、Wipe 一直 pending 时上界 1 小时就「过期」，之后的预订会把它当空闲挑走，开通时再改派——徒增 `booking_at_risk`）（失联的实例剩余时长冻结，上界必须随时间推进才能继续挡住后来的预订——这就是计划 1 遗留的「失联期间上界不随时间推进」）。每轮最多 200 行，按 `updated_at` 轮转：处理完把实例的 `updated_at` 推到 `now`（仅当更早时），否则候选超过一批时总是同一批最旧的行被处理。因此对 `stopped` / `wiping` / 暂停中的实例，`instances.updated_at` 是清扫轮转游标，不代表「最近一次业务变更」。
- `reserved` 与「预订的 creating」实例的 lease 不由 `refreshLease` 改写（时段在下单时定死；开通时只改 `status`）。`occupiedUntil` 对这两种情况直接返回现有 `upper`。

### 4.4 可用性与日历查询（只读）
- **时段可用台数** `availability(product, group, machine, period)`：§4.2 查询去掉 `FOR UPDATE` 后 `count(*)`（预订分支）。下单对话框每次改参数都查一次（防抖 300ms），返回 `{available: n}`；下单时以事务内结果为准。
- **机组日历** `GET /groups/{id}/calendar?from=YYYY-MM-DD&days=N`（N ≤ 61）：对每一天 `d`，返回该机组中「`[d 00:00, d+1 00:00)` 与任何 reserved / active 时段都不相交」的机器数 `free`，以及 `total`。一条 SQL：`generate_series` × `machines` LEFT JOIN `machine_leases` 用 `&&` 判交集后 `count(*) FILTER (WHERE l.id IS NULL)`。日粒度只用于展示（页面注明「以提交时校验为准」）。
- **机器日历** `GET /machines/{id}/calendar?from&days`：返回该机器在窗口内的 reserved / active 时段列表 `{lower, upper, mine: bool}`（`mine` = 当前用户自己的实例；别人的时段不带实例 ID、不带用户信息）。
- 日期解释一律按 `Asia/Shanghai`（后台已有 `cst`，提取到 `internal/platform/tz`）。

## 5. 预订生命周期

### 5.1 状态
```
reserved ──(开通时刻, 机器可用)──▶ creating ──▶ running ──▶ …（与现有状态机相同）
   │  ▲                              │
   │  │                              ├──(Create 失败 / 超时, 有候选：改派)──▶ creating（换机器，状态不变）
   │  └──(Create 失败 / 超时, 无候选且未过 grace)──┘   -- 回 reserved，钉在原（已 faulty 的）机器上等下一轮
   ├──(用户取消 / 后台取消 / 无法履约 / 失去租用资格)──▶ cancelled（终态；lease cancelled；退款见 §5.5）
```
- `reserved` 实例：`machine_id` = 当前钉住的机器，`pending_seconds = D`，`expires_at NULL`，`scheduled_start_at = S`，密码与主机密钥已生成（与 `Place` 一样，开通时不再生成）。
- 用户可见字段：`scheduled_start_at`、`scheduled_end_at = S + D`（视图计算）、`machine_label`、`group_name`、`cancellable_until = S - PrepLead`。

### 5.2 开通（清扫任务 `ProvisionDue`）
每轮取 `state='reserved' AND scheduled_start_at <= $1`（`$1 = now + PrepLead`，谓词不对列做运算，能走 `instances_reserved_idx`），逐个独立事务（`sweepEach` 模式）：

1. 锁实例（`SKIP LOCKED` 复核谓词）→ 锁自己的 reserved lease 行 → 锁机器行（`SELECT … FOR NO KEY UPDATE`；顺序 instance → lease → machine 与 `AdminMigrate` 的 instance → machine 一致）。
2. 先复核 `Users.CanRent(user_id)`：用户已被封禁或实名被撤销 → 按 §5.5 第 3 种平台取消（全额退款），事件 `"cancelled: user cannot rent"`，不再往下走。然后机器可用判定：`state='idle' AND agent_online AND NOT EXISTS 活动实例（creating/running/stopped/wiping）`。
3. 可用 → `machines.state='allocated'`，实例 `state='creating'`（`created_at` 不变，新增 `provisioned_at = now` 用于开通超时判定，见下），lease `status='active'`，事件 `reserved → creating "scheduled start <S>"`，`Enqueue CreateInstance{image_ref}`。
4. 不可用 → 改派（§5.4）。

`FailStuckCreating` 的谓词从 `created_at <= now - CreateTimeout` 改为 `COALESCE(provisioned_at, created_at) <= now - CreateTimeout`（`instances.provisioned_at` 加进 00019；即时订单为 NULL）。

### 5.3 计时
`startLease` 对**预订的首次开通**（`from == creating AND scheduled_start_at IS NOT NULL AND provisioned_at IS NOT NULL`；停机续费后的 Start 走现有分支，不再写「late by」事件）：

```
base    = max(S, now)                              -- 早开通不早计费；迟开通从实际开通算
expires = base + pending_seconds
cap     = 同机下一条 reserved 的 lower - Tail        -- 无则不封顶
expires = min(expires, cap)                         -- 下单（§4.2）与开通前加时（§7.1）都保证 cap - Tail ≥ S + pending，所以永远不少于承诺的时长
```
写事件 `"agent: created (scheduled S, late by Δ)"`（Δ ≤ 0 时省略）。早于 `S` 进入 running 的实例立即可以登录（SSH / Web 按 running 放行），这段时间不计费。

**封顶补偿**：若封顶导致 `expires < base + pending`（迟开通且后面紧跟别人的预订），差额 `short = base + pending - expires` 秒在同一事务内按该实例最新一笔可退订单的单价折算退款（`refundAcross`，remark「迟开通补偿 <id>」，`ref_id` 规则不变），事件 `"late start compensated: <short>s refund <X>"`；`short` 折算不足 1 分钱时不退。这是用户不可见的平台过失，不能既少给时长又不退钱。

### 5.4 改派（`reassign`）
触发：开通时机器不可用（§5.2 第 4 步）、预订实例 Create 失败或超时（见下「预订实例的 Create 失败 / 超时」）、后台「改派」操作。

过程（同一事务内，已持有实例锁）：
1. 候选 = §4.2 预订分支，`group_id = instances.group_id`（NULL 则不限），`machine_id = NULL`，period = 自己 lease 的 `period`，排除当前机器；**开通时刻的改派额外要求 `idle AND agent_online`**（现在就要开通），而后台提前改派只要求 lease 不冲突。
2. 选中 → 新插一条 lease（开通时 `active`，提前改派 `reserved`），旧 lease `cancelled`；`instances.machine_id = 新机器`，`reassign_count + 1`；事件 `"reassigned <old> -> <new>: <原因>"`；开通时继续 §5.2 第 3 步。
3. 没有候选：
   - `now < S + BookingGrace (30m)` → 保持 `reserved`，`RaiseAlertOnce(warning, booking_at_risk, machine, instance, "实例 <id> 的预订机器不可用且机组内暂无可改派机器")`，下一轮再试。
   - 否则 → 平台取消（§5.5 第 3 种），`RaiseAlert(critical, booking_unfulfilled, …, "实例 <id> 的预订无法履约，已全额退款")`。平台取消与退款同一事务；若事务失败（如流水唯一键冲突），`sweepEach` 回调外层在**独立事务**里 `RaiseAlertOnce(critical, booking_cancel_failed, machine, instance, "实例 <id> 的预订取消失败，详情见日志")`，否则实例会每分钟静默重试、永远卡在 reserved 而只有日志可见。
4. 旧机器上若已经创建了容器（Create 失败的情况），由现有对账处理：旧机器的 StateReport 报出该实例 ID 而 `inst.MachineID != m.ID` → `wipeOrphan`（计划 2 已实现，`reconcile.go`）。密码、端口记录都跟实例走，不需要复制；主机密钥在 Create 失败改派时重生成（见下）。

**预订实例的 Create 失败 / 超时**（`HandleCommandResult` 的 Create 失败分支与 `FailStuckCreating` 对 `scheduled_start_at IS NOT NULL` 的实例**不调用** `failCreating`，改调 `reassignAfterCreateFailure`）：
1. 与 `failCreating` 相同的部分：`MarkFaulty(旧机器)`、`RaiseAlert(critical, create_failed)`、`CancelCommands`。**不做**的部分：不改 `pending_seconds`（时长不能丢）、不清 `ssh_host_key_enc`、不退款、不把 lease 置 `ended`。
2. 有候选（§4.2 预订分支 + `idle AND agent_online`，排除旧机器）：旧 lease `cancelled`，新 lease `active`（period 不变）；实例 `machine_id` 换、`provisioned_at = now`（开通超时重新起算）、状态仍 `creating`、`reassign_count + 1`；**重新生成主机密钥**（旧容器可能已用旧密钥起过；用户还没连过，没有 known_hosts 问题）；事件 `creating → creating "reassigned <old> -> <new>: create failed"`；`Enqueue CreateInstance`。
3. 无候选且 `now < S + BookingGrace`：实例回 `reserved`、`provisioned_at = NULL`，**旧 lease 的 `status` 改回 `reserved`**（仍钉在已 faulty 的旧机器上——候选集不含 faulty，所以不会再被别人挑中；这样 §14「reserved 实例恰有一条 reserved lease」成立，下一轮 `ProvisionDue` 能锁到自己的 lease 行并从 §5.2 第 2 步重新判定）；`booking_at_risk` 一次；事件 `creating → reserved "create failed, waiting for machine"`。
4. 无候选且已过 grace：§5.5 第 3 种平台取消。

### 5.5 取消与退款
三种取消，都把实例置 `cancelled`（`cancelled_at = now`，并与 released 一样 `ssh_host_key_enc = NULL`）、lease 置 `cancelled`、写事件，并按订单从新到旧退款（复用 `refundableOrders` + `refundAcross`，退款流水 `ref_id = <order>-r<n>`）：

| 触发 | 允许条件 | 退款比例 | 审计 / 事件 |
|---|---|---|---|
| 用户 `POST /instances/{id}/cancel` | `state='reserved'` 且 `now < S - PrepLead`（清扫器还没来得及开通但已过开通时刻的也不能取消）；否则 `cancel_not_allowed` | `now < created_at + CancelCoolOff (15m)`：100%（冷静期）；否则 `S - now ≥ 24h`：100%，不然 50%（向下取整到分；比例常量 `LateCancelRefundPercent = 50`） | 事件 `reserved → cancelled "user cancel, refund X"`；审计 `booking.cancel`（user actor） |
| 后台 `POST /instances/{id}/cancel-booking` | `state='reserved'`，ops + TOTP，原因必填 | 100% | 审计 `booking.admin_cancel`（含原因、金额） |
| 平台无法履约（§5.4 第 3 步）/ 用户失去租用资格（§5.2 第 2 步） | — | 100% | 事件 `"unfulfilled: no machine by grace"` / `"cancelled: user cannot rent"`；短信 `TemplateBookingCancelled` |

- 退款基数 = 该实例全部已付订单的未退部分（预订单 + 开通前已买的续费单）。
- `GET /instances/{id}/cancel-quote` → `{refund, percent, deadline}` 供确认框显示。
- 批次里逐台取消（每台一个请求）；控制台提供「取消该批次全部未开通实例」按钮，前端串行调用。

### 5.6 预订期间允许的操作
| 操作 | reserved | 说明 |
|---|---|---|
| 续费（加时） | 允许（加到 `pending_seconds`，同时把 lease `upper` 推后；受 §7.1 冲突规则约束） | 与 creating 一致 |
| 释放 | 不允许（用取消） | |
| 改自动续费 | 允许 | |
| 端口 / 密码重置 / 密码登录开关 | 允许（计划 3 已允许非 running 管理端口） | SSH / Web 仍要 running |
| 后台免费延长 | 允许（规则同续费） | |
| 后台换机 | 不允许（用改派） | `AdminMigrate` 仍只对 running |
| 后台退款并释放 | 不允许（用取消预订） | 状态列表不含 reserved |

### 5.7 接入层（计划 3）衔接
- sshgw §2.3 表新增一行：`reserved` → Banner「实例预订于 <S> 开始，开始前 1 小时自动开通」，拒绝；`cancelled` 与 released 同文案「实例已释放」。
- ingress 对 `reserved` 返回 503 页同文案；`cancelled` 按终态 410。
- `ssh_command` 仍只在 running 输出（现状），控制台预订卡片显示「开通后可连接」。

## 6. 一次多台（批次）

### 6.1 请求与原子性
`PlaceBatch(req)`：`count ∈ [1, 8]`，一个事务：
1. 校验（租期、数量、时间窗、`CanRent`）；幂等键咨询锁（§6.2）；预订分支再取事务级咨询锁 `booking:<user_id>`（在 idem 锁之后、机器锁之前）并查「用户现有 reserved 实例数 + count ≤ 16」（否则两个并发批次各读到 10 再各加 8 会到 26）。
2. 取镜像、按 §4.2 挑机器、逐台插实例 + lease（savepoint）。
3. 建 `rental_batches` 行（`POST /bookings` 时）；对每台 `charge(kind='new', quantity)`：N 次 `Wallet.Apply`，钱包行在首个 `Apply` 时 `FOR UPDATE` 锁住，后续同事务重入；任一台余额不足 → 整个事务回滚 → `insufficient_balance`（消息带总价）。
4. 即时订单：每台 `Enqueue CreateInstance`；预订：不入队。
5. 返回 `{batch_id, orders[], instances[]}`。

没有「部分成功」：机器不够、余额不够、任一约束失败都整单失败。`Place`（旧接口 `POST /orders`）= `PlaceBatch{count:1, quantity:1, immediate, 不建批次}`：**不写 `rental_batches` 行**，`orders.batch_id = NULL`，`orders.idempotency_key = key`，响应与回放（`replayPlace`）完全保持现状；只有 `POST /bookings` 建批次（count=1 也建）。

### 6.2 幂等
- 三条下单 / 续费路径共用一把事务级咨询锁 `idem:<user>:<key>`，锁内**都**先查 `rental_batches` 再查 `orders`：
  - `POST /bookings`：命中批次 → 回放该批次（订单 + 实例当前状态）；命中单台订单（键被 `POST /orders` 或续费用过）→ `idempotency_conflict`。
  - `POST /orders`：命中 `kind='new'` 且 `batch_id IS NULL` 的订单 → `replayPlace`（现状）；命中批次或续费单 → `idempotency_conflict`。
  - `POST /instances/{id}/renew`：命中批次 → `idempotency_conflict`（现在只查 `orders`，要补这一查，否则批次用过的键再用于续费会静默建单）；其余按现有 `replayRenew`。
- 新建批次时 `rental_batches.idempotency_key = key`，子订单 `idempotency_key = key || '#' || 序号`（满足 `orders_user_idempotency_uniq`，并让单键查询查不到批次子单）。为此 `checkIdempotencyKey` 新增：**拒绝含 `#` 的键**（400「幂等键不能包含 #」）——否则用户提交 `abc#1` 会撞上批次 `abc` 的子单键并回放到别人的请求。现有 e2e / 前端生成的键是 UUID，不受影响。
- 并发重复提交由 `rental_batches_user_idempotency_uniq` 兜底：事务回滚后按 `isIdempotencyViolation` 的同一套路（约束名加入判断）回放批次。
- 现有 `idempotency_test.go` 与 e2e「双击只下一单」保持通过（`POST /orders` 的行为未变）。

### 6.3 展示
- 控制台实例列表按批次折叠（同 `batch_id` 的卡片连在一起，头部显示「批次 bat-xxx · N 台」）；详情页显示同批次其他实例的链接。
- 后台订单列表加 `batch_id` 列与筛选；实例详情显示批次。

## 7. 续费、自动续费与长期租约

### 7.1 冲突规则（所有加时路径共用 `renewFits`）
加 `secs` 秒后的新上界：

| 实例状态 | 新上界 |
|---|---|
| running（未暂停） | `max(expires_at, now) + secs + Tail` |
| running（暂停） | `now + paused_remaining + secs + Tail`（估算，恢复后以 §4.3 封顶为准） |
| creating（即时）/ stopped | `now + CreateTimeout + pending + secs + Tail`（与 `occupiedUntil` 的估算一致） |
| reserved / creating（预订） | `S + pending + secs + Tail` |

要求 `新上界 ≤ cap`（cap = 同机下一条 reserved 的 lower，§4.3）。不满足 → `renew_blocked`（409，消息「该机器自 <下一预订开始时刻 = cap + PrepLead> 起已有预订，本次最多可续至 <renewable_until = cap - Tail>」）。`Renew` / `AdminExtend` / 自动续费 / 停机期续费都先过这一关，再 `addTime` + `refreshLease`（后者不再可能失败）。

实例视图新增：
- `renewable_until`（nullable）：`cap - Tail`，即允许的最晚到期时刻；
- `renew_options: [{duration, quantity_max}]`：三档各自在不超过 `renewable_until` 且单次 ≤ 90 天下能买的最大数量（0 = 不可选）。控制台续费表单按它禁用档位 / 限制数量，并在表单顶部显示「可续至 <renewable_until>」；为 NULL 时不显示。

### 7.2 自动续费
- 字段：`auto_renew`、`auto_renew_duration`（默认 = 实例首单 `duration`），用户在实例详情开关；`PUT /instances/{id}/auto-renew {enabled, duration}`，允许状态 `reserved / creating / running / stopped`。数量固定为 1 个单位（续费粒度；长期租约是 `1m` 的自动续费）。
- 清扫任务 `AutoRenewDue`：`state='running' AND suspended_at IS NULL AND auto_renew AND expires_at <= now + AutoRenewLead (1h) AND (auto_renew_failed_at IS NULL OR auto_renew_failed_at <= now - CASE auto_renew_failed_reason WHEN 'renew_blocked' THEN 10 分钟 ELSE 1 分钟 END)`（余额不足要快重试——用户在充值；被预订挡住只会在那条预订被取消 / 改派时才变化，10 分钟足够），逐个独立事务：
  1. 锁实例；重新校验条件；
  2. 档位：从 `auto_renew_duration` 开始，按 `1m → 1w → 1d` 顺序找第一个满足 `renewFits` 的档位（只能降档不能升档）；都不满足 → 失败 `renew_blocked`；
  3. `charge(kind='renew', auto=true, quantity=1)`，余额不足 → 失败 `insufficient_balance`（事务回滚后单独写失败标记，见 4）；
  4. 成功：`addTime` + 事件 `"auto renew <order> (<实际档位>)"`，清空 `auto_renew_failed_*`，提醒标记重算（`addTime` 已做）；失败：独立小事务 `UPDATE instances SET auto_renew_failed_at = now, auto_renew_failed_reason = …`，并在**首次**失败（之前 `auto_renew_failed_at IS NULL`）时发送短信 `TemplateAutoRenewFailed{instance, reason}`。
  5. 之后每轮都重试（间隔由上面的 `<= now - 1 分钟` 决定），直到成功或实例到期停机。到期停机后 `auto_renew` 保持 true，但停机实例不自动续费（避免用户忘了而被持续扣款）；用户手动续费恢复后，自动续费继续生效。
- 与提醒的关系：开启自动续费的实例仍收 24h 提醒（文案不变）；1h 提醒改为在 `AutoRenewDue` 处理之后才判定——若 1h 前已自动续费成功，`addTime` 重算的 `reminded_1h` 会让它不再发；失败才会收到 1h 提醒 + 自动续费失败短信（两条，可接受）。
- 降档只发生在被预订挡住时，订单 `duration` 记实际档位、事件与控制台提示「因后续预订，本次自动续费按 <档位> 办理」。
- 后台实例详情显示自动续费开关与最近失败原因（只读，不能替用户改，与密码登录开关同理）。

### 7.3 长期租约
- 定义：`1m × N`（N ≤ 3）一次买断，或 `1m` + 自动续费。不做新价格档（B3）；不做合同 / 发票（子项目 4）。
- 运营手段：机组 `visible=false` 留池 + 后台替用户下单？——**不做**后台代下单（遗留），运营可用后台「免费延长」与改价满足大客户。

## 8. 与现有行为的交互

| 场景 | 行为 |
|---|---|
| 即时租用 | 走 `PlaceBatch` 即时分支：候选只要 `idle AND agent_online` 且 `[now, now+D+Tail)` 无重叠；一台机器若在 D+7h 内有预订就不会被分给这单（用户看到的 `idle_count` 改为「此刻可即时租用」= 同一查询的 count，而不是单纯 idle 台数） |
| 维护模式 | 进入维护只允许 `idle / faulty`（现状）；机器上有未开通预订时后台提示（B12），预订在开通时改派。结束维护的检查不变 |
| 换机（`AdminMigrate`） | 新机器用 §4.2 即时分支（period 与 `openLease` 对 creating 的估算一致：`[now, now + CreateTimeout + 剩余 + Tail)`），不会挑到放不下的机器；新实例**复制** `auto_renew`、`auto_renew_duration`（否则用户开了自动续费换一次机就静默失效）；`group_id` 取**新机器的机组**（实施中裁定，见 `followups-plan6.md`：展示名与机组保持一致，删除机组的检查也以实际机器为准）；`batch_id` 不复制，`scheduled_*` / `provisioned_at` 为 NULL；旧实例 lease 由 `beginWipe → refreshLease` 处理（上界封顶）。预订实例不可换机（§5.6） |
| 机器失联（暂停） | 上界每轮推进并封顶（§4.3）；若封顶导致到预订开通时刻机器仍被占 → 预订改派，老租户不动（第 11 条决策） |
| 到期停机 + 6h 保留 | 尾段已计入时段；停机期间续费受 §7.1 约束（用户若想续但被挡 → 控制台提示可续至，或等数据保留结束） |
| 用户释放（不退款） | 不变；释放后 `beginWipe` 上界封顶为洗机估算 |
| 开通失败 | 即时订单：不变（failed + 退款 + 机器 faulty）；预订：改派（§5.4） |
| 洗机失败 / 机器 faulty | 机器退出候选集；其上的预订在开通时改派 |
| 对账（`Reconcile`） | 「预期实例」仍只查四个活动状态；reserved 实例的容器不存在不告警；改派后旧机器报出的容器按孤儿清理 |
| 计划 3 访问 | 见 §5.7；端口、密码、公钥在 reserved 可管理 |
| 后台退款并释放 | 允许状态不变（creating / running / stopped）；`refund.go` 的 `quote` 与 `refundAll` 改用 `seconds(duration) × quantity` |
| 仪表盘出租率 | `DailyOccupancy` 继续只算 `active / ended`（预订不算出租）；新增「未来 7 天预订开始台数」卡片 |
| 后台实例列表 / 详情 | `state` 筛选加 `reserved / cancelled`；详情显示预订信息、批次、改派次数、自动续费 |
| CSV 导出订单 | 加 `batch_id`、`quantity`、`auto` 列 |

## 9. 清扫器（`rental.Sweep` 顺序）
```
离线判定 → 暂停 → 恢复 → RefreshCappedLeases（新）→ ProvisionDue（新，含 §5.4 第 3 步的超时取消）
→ AutoRenewDue（新）→ ExpireDue → WipeStale → FailStuckCreating → FailTimedOutCommands → ReapSSHSessions → SendReminders
```
- `ProvisionDue` 在 `ExpireDue` 之前：同一轮里前一租户刚到期停机不会立刻空出机器（还有 7h 尾段），顺序不影响正确性；放前面只是让预订先占到咨询锁的时间。
- `AutoRenewDue` 在 `ExpireDue` 之前：到期前 1 小时触发，二者不会同一轮命中同一实例；放前面保证最后一轮重试先于停机。
- 每个任务的单行失败只回滚该行并汇总日志（`sweepEach` 现有语义）。
- e2e：`/hooks/sweep` 不变（调用 `Sweep`），新任务自动包含。

## 10. API

错误码（`apperr`）新增：
`booking_too_soon`（400「开始时间至少在 2 小时之后」；校验为 `S ≥ now + MinBookingLead - 5 分钟`，给对话框打开到提交之间留容差，整点规则仍严格）、`booking_too_far`（400「最多提前 60 天预订」）、`booking_too_long`（400「单次租期最长 90 天」）、`booking_not_hour`（400「开始时间需为整点」）、`booking_limit`（409「未开通的预订最多 16 台」）、`count_invalid`（400「一次最多租用 8 台」）、`renew_blocked`（409，消息见 §7.1）、`group_not_found`（404「机组不存在」）、`machine_unavailable`（409「该机器在所选时段不可用」，指定机器时用，替代 `no_capacity` 的通用文案）、`cancel_not_allowed`（409「预订已开始开通，不能取消」）、`group_not_empty`（409「机组内还有机器或未结束的实例」）。`no_capacity` 的消息改为带台数：「该时段只有 X 台可用，请减少数量或更换时段」（X 为 0 时「该时段暂无可用机器」）。

### 10.1 控制台（`/api/v1`，均在 `requireUser` 组内；`GET /groups*`、两个日历与 `GET /availability` 注册在组外、可匿名，并按 IP 限流每分钟 120 次）

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/groups?product_id=` | `{items:[{id,name,region,description,machine_count,idle_now}], now}`，只含 `visible` 机组；匿名可访问（市场页未登录也能看）；带 `product_id` 时两个计数只算该商品的机器（机组可跨商品，§3.1；终审 M1），不带则不过滤 |
| GET | `/groups/{id}/machines?product_id=` | `{items:[{id,label,busy_until}]}`，`label` = `display_name` 或 `机器 #id`；`busy_until` = 最近一条 active / reserved 时段的 upper（无则 null）；`product_id` 同上 |
| GET | `/groups/{id}/calendar?product_id=&from=&days=` | `{days:[{date,free,total}]}`；`product_id` 同上 |
| GET | `/machines/{id}/calendar?from=&days=` | `{periods:[{lower,upper,mine}]}`；机器不在可见机组 → 404；匿名可访问（`mine` 恒为 false） |
| GET | `/availability?product_id=&group_id?=&machine_id?=&start_at?=&duration=&quantity=` | `{available:n, period:{lower,upper}}`；`start_at` 缺省 = 立即；带 `machine_id` 时与 `POST /bookings` 指定机器同一校验（机器在可见机组且 `group_id` 与之一致，否则 `not_found`；终审 M2） |
| POST | `/bookings` | body `{product_id, image_id, group_id?, machine_id?, count, duration, quantity, start_at?, auto_renew?, auto_renew_duration?}` + `Idempotency-Key` → 201 `{batch:{id,count,created_at}, orders:[Order], instances:[Instance]}`；`start_at` 缺省 = 立即（即时分支）；`machine_id` 要求 `count=1` 且 `group_id` 为该机器所在机组 |
| POST | `/orders` | 保留，语义 = `count 1 / quantity 1 / 立即`，响应不变（内部走 `PlaceBatch`） |
| GET | `/bookings/{batch_id}` | `{batch, orders, instances}`（只看自己的） |
| GET | `/instances/{id}/cancel-quote` | `{refund, percent, deadline}` |
| POST | `/instances/{id}/cancel` | → 实例视图；错误 `cancel_not_allowed` / `invalid_state` |
| PUT | `/instances/{id}/auto-renew` | `{enabled, duration}` → 实例视图 |
| POST | `/instances/{id}/renew` | body 增加 `quantity?`（默认 1）；错误新增 `renew_blocked` |
| GET | `/instances/{id}/events` | `{items:[{to_state,reason_kind,at,detail}]}` 最近 20 条，只输出白名单种类（下单 / 开通 / 改派 / 自动续费 / 取消 / 到期 / 释放），`detail` 为可展示文案（不透出机器 ID、序列号） |

`instanceView` 新增字段：`scheduled_start_at`、`scheduled_end_at`、`cancellable_until`、`group_id`、`group_name`、`machine_label`、`batch_id`、`reassign_count`、`auto_renew`、`auto_renew_duration`、`auto_renew_failed_reason`、`renewable_until`、`renew_options`。`Order` 视图新增 `quantity`、`batch_id`、`auto`。`productView.idle_count` 语义改为「此刻可即时租用台数」（§8 第 1 行）。

### 10.2 后台（`/admin/api/v1`）

| 方法 | 路径 | 角色 | 说明 |
|---|---|---|---|
| GET | `/groups` | all | 全部机组（含 `visible=false`），每项带 `machine_count`、`idle_now`、`reserved_upcoming`（未开通预订数） |
| POST | `/groups` | ops | `{name, region, description, sort, visible}`；审计 `group.create` |
| PUT | `/groups/{id}` | ops | 同上；审计 `group.update` |
| DELETE | `/groups/{id}` | ops | 条件见 §3.1；审计 `group.delete` |
| POST | `/groups/{id}/machines` | ops | `{machine_ids:[…]}` 批量加入（已在其他机组的机器直接改组）；审计 `group.assign`（含列表） |
| DELETE | `/groups/{id}/machines` | ops | `{machine_ids}` 移出（置 NULL）；审计 `group.unassign` |
| PUT | `/machines/{id}` | ops | `{group_id?, display_name?}` 只认这两个字段；审计 `machine.update`（before / after） |
| GET | `/machines/{id}` | ops | 响应新增 `group`、`display_name`、`leases:[{id,instance_id,status,lower,upper}]`（未来 60 天 + 当前）；`current_instance` 只取活动态（creating / running / stopped / wiping），`reserved` 既不进 `current_instance` 也不进 `history`（只在 `leases` 里出现；终审 I1） |
| GET | `/machines/{id}/calendar?from&days` | ops | 同控制台但带 `instance_id` / `user_id` |
| GET | `/bookings?state=&user_id=&group_id=&machine_id=&from=&to=&page=` | all | 预订列表（`state ∈ reserved / cancelled`，默认 reserved）：实例 ID、用户、机组、机器、开始、结束、批次、改派次数、下单时间 |
| GET | `/bookings/upcoming?days=7` | all | 仪表盘用：未来 N 天每天开始的预订台数 |
| POST | `/instances/{id}/cancel-booking` | ops + TOTP | `{reason, totp}` → `{refunded}`；审计 `booking.admin_cancel` |
| POST | `/instances/{id}/reassign` | ops | `{machine_id?, reason}`：指定目标机器（须在候选集内，否则 `machine_unavailable`；实例 `group_id` 改为目标机器的机组，之后的自动改派以新机组为准——实施中裁定）或自动挑选；仅 `reserved`；审计 `booking.reassign` |
| GET | `/instances/{id}` | all | 响应 `instance` 新增 §10.1 的预订 / 自动续费字段，`orders[]` 带 `quantity / batch_id / auto` |
| GET | `/dashboard` | all | 新增 `bookings_upcoming_7d`、`instances.reserved`（状态计数自然包含） |
| GET | `/orders` | finance | 筛选新增 `batch_id`；CSV 列新增 |
| POST | `/instances/{id}/extend` | ops | 错误新增 `renew_blocked` |
| POST | `/machines/{id}/maintenance` | ops | 响应新增 `upcoming_bookings`（进入维护时的数量，前端用于提示；不阻止） |

## 11. 控制台页面

### 11.1 市场页 `/market`（改造下单对话框）
卡片保留商品信息与三档单价；`空闲 X 台` 改为 `现在可租 X 台`。按钮「立即租用」与「预订档期」都打开同一个 `RentDialog`（手机全屏），差别只是默认 `start` 模式：

1. **机组**：`不限机组` / 机组列表（`名称 · region`，`共 N · 现空闲 X`）。
2. **数量**：1～8（`el-input-number`）。
3. **开始**：`立即` / `指定时间`（日期 + 整点下拉；范围 `[now+2h 向上取整到整点, now+60d]`）。
4. **租期**：档位单选（1d / 1w / 1m）× 数量（档位切换时数量上限随之变化：90 / 12 / 3），显示换算天数。
5. **指定机器**（可折叠，仅 数量=1 且选了机组时出现）：机器列表 + 该机器 60 天占用条带（§4.4 机器日历），点击选定。
6. **自动续费**：开关 + 档位（默认与租期档位相同）。
7. **可用性**：参数变化即查 `/availability`，显示「该时段可预订 Y 台」，Y < 数量时提交按钮禁用并提示；`指定时间` 模式下方展示机组日历（两个月网格，每格 `free/total`，不可预订的天灰显；点击日期把开始日期填入）。
8. **确认**：商品、机组 / 机器、数量、开始（立即 / 时间）、租期（`1m × 2 = 60 天`）、单台价、总价、余额、扣后余额；余额不足 → 「去充值」。提交 `POST /bookings`（`Idempotency-Key` 由 `useIdempotencyKey` 按全部参数派生）。
9. 成功：数量 1 → 跳实例详情；多台 → 跳 `/instances?batch=<id>` 并提示「已下单 N 台」。错误：`no_capacity` / `machine_unavailable` 刷新可用性并保持对话框；`booking_*` 显示后端文案。

未登录点任一按钮 → 登录；未实名 → 实名引导（现状）。

### 11.2 我的实例 `/instances`
- 新状态标签：`已预订`（青色）、`已取消`（灰）。
- 预订卡片时间文案：`<S> 开始 · 共 <D>`；`cancellable_until` 之前显示「可取消」小字；`reassign_count > 0` 显示「已更换机器」标记。
- 同批次卡片折叠显示批次头（`批次 bat-xxx · N 台`），`?batch=` 参数时只显示该批次并有「取消全部未开通」按钮（逐台调用 `/cancel`，成功 / 失败各自提示）。
- 有 `reserved` 实例时与 creating 一样每 10 秒刷新（开通时刻会自动变为开通中）。

### 11.3 实例详情 `/instances/:id`
- 基本信息新增：机组、机器名、预订开始 / 结束、批次链接、「已更换机器 N 次」。
- 预订状态：操作区为「取消预订」（确认框显示 `cancel-quote`：「将退款 ¥X（Y%），截止 <deadline>」，红色确认按钮）和「续费（加时）」；不显示释放。
- 续费表单：档位 × 数量；顶部「可续至 <renewable_until>」（有值时）；不可选档位灰显并提示「超出可续时间」；`renew_blocked` 错误文案原样显示。
- 「自动续费」卡片：开关 + 档位；`auto_renew_failed_reason` 非空时顶部红色提示条（`insufficient_balance`：「自动续费失败：余额不足，请充值后系统会自动重试」+ 去充值；`renew_blocked`：「自动续费失败：机器已有后续预订，可续至 …」）。「可续至」一律显示后端的 `renewable_until`（= 下一预订开通时刻 − 7h = 下一预订开始时刻 − 8h），前端不自行推算。
- 时间线（新增，简版）：从 `GET /instances/{id}/events`（新接口，用户可见事件的白名单：下单、开通、改派、自动续费、取消）显示最近 20 条。

### 11.4 帮助页
新增三节：档期预订（提前 2 小时～60 天、整点开始、开始前 1 小时自动开通、取消规则：15 分钟冷静期全退 / 距开始 ≥ 24h 全退 / 不足 24h 退 50% / 开通后不可取消、机器可能更换、价格在下单时锁定——预订到开通之间改价不影响已付订单、可续至 = 下一预订开始前 8 小时）、一次多台（整单成功或失败、逐台管理）、自动续费（到期前 1 小时扣款、余额不足重试、被预订挡住时降档、停机后不自动续）。

## 12. 后台页面
- 菜单新增 `机组 /groups`（ops）与 `预订 /bookings`（all）；`menu.spec.ts` 表同步。
- `/groups`：表格（名称、位置、机器数、现空闲、未开通预订、可见、排序）、新建 / 编辑弹窗、删除确认、「管理机器」抽屉（左：组内机器列表可勾选移出；右：按机柜筛选未分组 / 其他组机器勾选加入；显示每台的 `display_name` 可直接编辑）。
- `/bookings`：筛选（状态、用户、机组、机器、开始日期范围）、表格、行操作：「改派」（弹窗：自动 / 选择目标机器（下拉只列候选集，由 `GET /machines/{id}/calendar` 与 `/availability` 的后台版校验）、原因必填）、「取消预订」（原因必填，提交后弹动态码，成功提示退款金额）。
- `/machines/:id`：新增机组与展示名（编辑按钮，`PUT /machines/{id}`）、「档期」区块：未来 60 天时间轴（横向条带，active 绿 / reserved 蓝，悬停显示实例与用户）+ 列表；「进入维护」确认框在 `upcoming_bookings > 0` 时追加「该机器有 N 个未开通的预订，将在开通时自动改派」。
- `/instances`：状态筛选加已预订 / 已取消；列表与详情展示预订字段；详情操作区：`reserved` 时显示「改派」「取消预订」，隐藏换机 / 退款并释放；自动续费只读。
- 仪表盘：新增「未来 7 天预订开始」卡片与「已预订」实例计数。
- 订单页：`batch_id`、`quantity`、`auto`（显示「自动」标记）列与筛选。

## 13. 配置与常量

| 项 | 值 | 说明 |
|---|---|---|
| `MinBookingLead` | 2h | 最早开始 |
| `MaxBookingAhead` | 60d | 最晚开始 |
| `MaxLeaseSpan` | 90d | 单次 `duration × quantity` 上限（即时与预订都受限） |
| `MaxMachinesPerOrder` | 8 | |
| `MaxOpenReservationsPerUser` | 16 | 未开通（reserved）实例数 |
| `PrepLead` | 1h | 开通提前量 |
| `BookingGrace` | 30m | 开始后仍无机器 → 取消 |
| `Tail` | 7h | `RetainAfterStop + WipeBuffer`（现有常量） |
| `CancelCoolOff` / `FreeCancelBefore` / `LateCancelRefundPercent` | 15m / 24h / 50 | 冷静期全退；之后两档 |
| `BookingLeadTolerance` | 5m | `booking_too_soon` 容差 |
| `AutoRenewRetryBlocked` | 10m | `renew_blocked` 的自动续费重试间隔（余额不足为 1 分钟） |
| `AutoRenewLead` | 1h | |
| `SPARK_SMS_TPL_AUTO_RENEW_FAILED` / `SPARK_SMS_TPL_BOOKING_CANCELLED` | 环境变量 | `SPARK_SMS_DRIVER=aliyun` 时必填（`requireAll` 同现有模板）；mock 驱动记日志 |

全部常量在代码里（`internal/rental/booking.go`），不做环境变量；时区 `Asia/Shanghai` 固定。

## 14. 并发与正确性

- **真相在数据库**：超卖由排他约束兜底；应用层的 `NOT EXISTS` 只是预筛。任何插入 / 修改 reserved / active 时段的路径（下单、改派、续费 `refreshLease`）都要能处理 `IsExclusionViolation`：下单 / 改派换下一台候选，续费路径因 §4.3 封顶理论上不会触发，但仍映射为 `renew_blocked` 而不是 500。
- **各路径的实际加锁顺序**（如实写进 `service.go` 头注释，不强求一条总序）：
  | 路径 | 顺序 | 等待点 |
  |---|---|---|
  | PlaceBatch | idem 咨询锁 → [booking:<user> 咨询锁] → machines（`SKIP LOCKED`）→ 插实例 / lease → orders → wallet | 插 lease 可能等待别的事务**未提交的 lease 增长** |
  | ProvisionDue | instance → 自己的 lease → machine（`FOR NO KEY UPDATE` 等待）→ [改派：machines `SKIP LOCKED`] → agent_commands | 等批次 / 心跳短暂持有的机器锁 |
  | 后台改派 | instance → lease → machines（自动挑选 `SKIP LOCKED`；指定机器 `FOR NO KEY UPDATE` 等待该行，实施中修订） | 同上 |
  | Renew / AdminExtend / 自动续费 | [idem] → instance → 自己的 lease（读 cap）→ orders → wallet → 更新 lease | 不锁机器 |
  | 取消 | instance → lease → orders → wallet | — |
  | HandleCommandResult | instance → agent_commands → lease（`refreshLease`，收缩或等长）→ machine（`Fleet.Release`） | Release 等机器锁 |
  | ExpireDue / WipeStale / Suspend / Resume / RefreshCapped / beginWipe | instance → lease → agent_commands | — |
  | AdminMigrate | 原实例 → machines（`SKIP LOCKED`）→ 新 lease → agent_commands → 旧 lease 收缩 | — |
- **无死锁论证**：持机器行锁的事务（批次、改派、换机、开通）只会在「插入 lease 撞到别的事务未提交的 lease 增长」时等待（排他约束对未提交的重叠时段是等待而不是立刻报错）；而所有会让 lease **增长**的路径（`refreshLease` 各调用点、续费）都不等机器锁，所以等待链不成环。`Fleet.Release` 虽然等机器锁，但它之前的 `refreshLease` 是收缩到 `ended`，没有人会等它。并发批次之间只用 `SKIP LOCKED`，互不等待。
- **兜底**：PG 的死锁检测（`40P01`）与序列化失败（`40001`）仍可能在未预见的路径上出现；`PlaceBatch` 与 `reassign` 遇到这两个错误码**整个事务重试一次**，再失败才返回 500。
- **无死锁论证**：并发批次之间只用 `SKIP LOCKED` 取机器，不会互相等待；批次与清扫器开通之间：开通先锁实例（reserved 实例不是批次的目标），再锁机器行；批次在机器行上 `SKIP LOCKED`，碰到开通锁住的机器直接跳过。续费 / 取消 / 结果回报都从实例锁开始，彼此按同一顺序。
- **一致性检查**（集成测试与 `make check` 断言）：任一时刻同一机器的 reserved / active 时段两两不交；`reserved` 实例恰有一条 reserved lease；`creating / running / stopped / wiping` 实例恰有一条 active lease；`cancelled / released / failed` 没有 reserved / active lease；批次内订单数 == `rental_batches.count` 或批次不存在。
- **幂等**：§6.2；清扫任务用 `sweepEach` 的「锁内复核谓词」；自动续费的失败标记写在独立事务里，不会因主事务回滚而丢失。
- **时钟**：所有判定用注入的 `clock.Clock`；整点取整用 `Asia/Shanghai`。

## 15. 迁移与兼容
1. 迁移 00019 对存量数据：无 UPDATE（新列有默认值；`instances_state_check` 重建不影响现有值）。
2. 代码发布顺序：platform 单独发布即可（本计划不改 Agent 协议）；多副本滚动期间旧副本的 `fleet.Allocate` 不看 lease——窗口内新旧副本同时下单可能把带预订的机器分给即时订单，但预订功能本身在新副本才有，建议一次性重启所有副本（README 注明）。
3. 存量实例：`quantity=1`，退款折算不变；`renewable_until` 为 NULL（机器上没有预订）。存量订单的幂等键若含 `#`（理论上可能，前端生成的是 UUID）不受影响：新规则只拒绝新提交的键。
4. 现有 e2e 用例：`idle_count` 语义变化不影响（测试机器上没有预订）；`POST /orders` 响应不变。
5. 回滚：Down 有损（§2）；上线后若需回滚，先在后台取消全部预订再 Down。

## 16. 测试与验收

### 16.1 单元（Go，嵌入式 PG，假时钟）
- `rental/booking`：参数校验矩阵（2h / 60d / 90d / 整点 / count 1～8 / 16 上限 / 指定机器需 count=1 且同组）；`pickMachines` 候选集（商品、机组可见性、未分组、状态、重叠、best-fit 排序、`machine_id`）；排他冲突 → 换机器（用第二个事务先插重叠 lease 模拟）；候选不足整单回滚（订单、实例、lease、流水都不存在）；余额不足回滚；批次幂等回放、形态不一致 `idempotency_conflict`、并发重复提交只建一个批次；即时分支与旧 `Place` 等价（现有 `place_test.go` 全部通过）。
- `ProvisionDue`：到点开通（T-1h ± 1 分钟）、未到点不动；开通前 `CanRent` 失败 → 平台取消全退；机器 faulty / maintenance / 离线 / 被占 → 改派到同组空闲机器（事件、`reassign_count`、旧 lease cancelled、新 lease active）；不限机组改派到任意可见组；无候选 → `booking_at_risk` 一次；超过 grace → cancelled + 全额退款 + `booking_unfulfilled` + 短信；取消事务失败 → `booking_cancel_failed` 告警；Create 失败 / 超时 → `reassignAfterCreateFailure`：有候选时 `creating → creating` 换机、主机密钥重生成、`provisioned_at` 重置、`pending_seconds` 与 `ssh_host_key_enc` 不动、不退款、机器 faulty；无候选时回 `reserved` 且旧 lease 回 `reserved`（一致性检查通过）；`FailStuckCreating` 用 `COALESCE(provisioned_at, created_at)`。
- `startLease`：早开通到期 = S + D；迟开通补偿；补偿被下一预订封顶时按差额退款并写事件；停机续费后的 Start 不走预订分支（无「late by」事件）。
- `refreshLease` 封顶：失联实例上界随 `RefreshCappedLeases` 推进且不越过预订；`occupiedUntil` 永不为空区间。
- 取消：冷静期 15 分钟全退、两档比例、`cancel-quote`、creating 后不可取消、后台取消全退 + 审计、含开通前续费单的退款分摊（`refundAcross` 顺序）、取消后 `ssh_host_key_enc` 为 NULL。
- `renewFits` / `renewable_until` / `renew_options`：running / 暂停 / stopped / reserved 各状态（断言用 `renewable_until = 下一预订 S − 8h`）；`Renew`、`AdminExtend`、停机续费被挡返回 `renew_blocked` 且不扣款。
- 自动续费：到期前 1h 扣款并延长；余额不足 → 失败标记 + 一条短信、补足后下一轮成功并清标记；被预订挡住降档（1m→1w→1d）、都不行 → `renew_blocked`；停机后不续；暂停中不续；`orders.auto=true`。
- 机组：CRUD、删除条件、分配 / 移出、`display_name` 校验、`visible=false` 不可下单也不参与不限机组。
- 日历查询：机组日历计数、机器日历 `mine` 标记与脱敏、`availability` 与下单结果一致（属性测试：随机生成 20 条时段后，对 100 个随机窗口 `availability` 的值等于真能下成功的最大台数）。
- 迁移：Up / Down 往返；Down 对 reserved 实例的降级。
- `api` / `adminapi`：路由、角色矩阵（`permissions_test.go`）、错误码、TOTP 要求、`idle_count` 语义、`booking_too_soon` 的 5 分钟容差、`duplicate_display_name`。
- `sshgw` / `ingress` / `rental.ListIngressRoutes`：reserved 实例的 Banner 与 503 页文案各一条断言（含 `<S>` 的 Asia/Shanghai 格式）、路由表含 reserved 实例的端口、cancelled 实例 410 与会话断开。
- `rental/migrate`：换机复制自动续费，`group_id` 取新机器的机组；`PlaceBatch` / `reassign` 遇 40P01 / 40001 重试一次。

### 16.2 集成（Go，进程内 fake Agent，`internal/app`）
- 10 台机器、50 个 goroutine 并发提交随机窗口 / 数量（1～8）的批次 → 事后一致性检查（§14）全部成立、没有部分批次、没有死锁（测试超时 60s）。
- 全链路：预订（S = now + 3h）→ 推进到 T-1h → 清扫 → creating → fake Agent 完成 → running、`expires = S + D` → 推进到到期前 1h → 自动续费 → 推进到到期 → 未停机 → 关闭自动续费 → 到期停机 → 6h 后洗机 → released；期间同机下一条预订使续费 `renew_blocked`。
- 改派链路：预订机器在 T-2h 进入维护 → T-1h 改派到同组另一台 → 开通成功；旧机器 StateReport 报出容器 → 孤儿清理。Create 失败链路：fake Agent 对 Create 回报失败 → 同事务改派到第三台（主机密钥指纹变化）→ 成功；再让所有候选都 faulty → 回 reserved 且 lease 回 reserved → 一致性检查通过 → 过 grace 后取消全退。
- 即时订单不抢预订机器：1 台机器、预订 S = now + 10h、即时下 1d 单 → `no_capacity`；预订 S = now + 40h → 成功。

### 16.3 端到端（Playwright）
钩子：`/hooks/machines` 增加 `group_id?`；新增 `POST /hooks/groups {name, visible?}` → `{id}`；其余沿用（推进时钟 + `sweep` + `agent`）。
覆盖表新增（登记到 `coverage.json`）：
- `C-BKG-01`～`11`：对话框机组 / 数量 / 时间 / 租期联动与可用台数；日历点击填开始日期；指定机器与占用条带；预订成功跳转并显示「已预订」；一次 3 台成功并按批次折叠；台数不足提示；余额不足去充值；开始时间不合法文案；幂等（双击只下一单）；未登录 / 未实名引导；`C-BKG-11`（实施中补充）立即租用走旧接口 `POST /orders` 遇 `no_capacity`：提示后回到表单、市场台数随即刷新。
- `C-DET-17`～`25`：预订详情字段；取消（≥24h 全退 / <24h 半退）确认框金额与退款流水；开通后取消按钮消失；续费表单显示可续至并禁用超限档位；`renew_blocked` 文案；自动续费开关与失败提示条（余额不足）；自动续费降档提示；时间线；`C-DET-25`（实施中补充）续费数量清空后回到 1、按 1 个单位扣款。
- `C-INS-05`～`07`：预订到点变开通中（推进 + sweep + agent）；改派后「已更换机器」标记；批次「取消全部」。
- `A-GRP-01`～`05`：机组新建 / 编辑 / 删除条件 / 批量加入移出 / 展示名编辑。
- `A-BKG-01`～`06`：预订列表筛选；改派（自动 / 指定）与审计；取消预订需动态码并退款；机器详情档期时间轴；进入维护提示预订数量；仪表盘预订卡片。`A-BKG-02` 端到端只覆盖「改派到指定机器」；自动改派由 Go 测试覆盖（`TestAdminReassignToMachineAndAuto`、`TestProvisionReassignsWhenMachineUnavailable`、`TestBookingReassignChainHTTP`）。
- `A-INS-13`：实例详情预订 / 自动续费字段只读。
- 注意：改时钟的用例按 `playwright.config.ts` 的顺序说明处理；预订用例推进时间不超过 3 天；只为造场景而下的预订在用例结束时取消（`cancelBookingsAndDispose`），免得后面的用例推进时钟后它们到点开通。

### 16.4 真机验收清单（192.168.1.221，一台机器，全部通过才算完成）
平台跑在 Mac（配置同计划 3 验收），Agent 不需要升级（无协议改动）。只有一台真机，多机行为以 §16.2 / §16.3 为准；真机覆盖「到点开通」「自动续费」「续费被预订挡住」「取消退款」。

1. **机组与展示名**：后台新建机组 `A 区`，把机器 1 加入并设展示名 `A-01`；控制台市场页对话框看到 `A 区 · 共 1 · 现空闲 1`，机器列表显示 `A-01`。
2. **到点开通**：预订 `A-01`，开始 = 当前时间 + 2h 向上取整到整点，1d，自动续费关。控制台显示「已预订」「可取消」；后台机器详情档期时间轴出现蓝色条。在 T-1h ± 1 分钟（看清扫日志）实例变为开通中 → 运行中；此时 SSH 已可登录但 `expires_at = S + 1d`（后台核对）；到 S 时刻剩余时长开始递减（控制台倒计时与后台 `remaining_seconds` 一致）。
3. **即时订单被预订挡住**：在第 2 步开通前，用另一账号即时租 1d → `no_capacity`（机器 10h 内有预订）；取消预订后再试 → 成功（随后释放）。
4. **取消退款**：预订开始 = +30h → 取消 → 流水 `refund` 全额；预订开始 = +3h → 取消 → 退 50%；两次 `cancel-quote` 的金额与实际一致；后台审计有记录。
5. **续费被挡 + 可续至**：实例运行中（1d，`expires ≈ now + 1d`），再在 `A-01` 上预订 S = now + 3d；实例详情显示「可续至 <S − 8h>」（`renewable_until = S − PrepLead − Tail = now + 64h`）；续费 1d（新到期 now + 48h ≤ 64h）可选、1w 被禁用；接口直接提交 1w → `renew_blocked`，余额不变。（S = now + 2d 时 `renewable_until = now + 40h`，连 1d 都放不下——不要用这个数字。）
6. **自动续费**：开启自动续费（1d）；`UPDATE instances SET expires_at = now() + interval '65 minutes'`；2 分钟内清扫日志出现 auto renew，订单页新增 `auto` 续费单，`expires_at` 延后 1d，mock 短信日志无失败记录；把余额调为 0 并再次把 `expires_at` 改到 65 分钟后 → 失败提示条 + mock 短信日志一条「自动续费失败」；充值后下一轮成功、提示条消失。
7. **改派不可测（单机）**：记录为 e2e 覆盖；真机只验证机器进入维护时后台提示「有 N 个未开通的预订」。
8. **接入层**：reserved 实例 `ssh` 看到 Banner「预订于 … 开始」；Web 端口 503 页同文案；开通后恢复。
9. **对账无噪声**：整个过程后台告警列表没有 `orphan_container` / `container_down` 以外的意外告警（`booking_*` 不应出现）。

验收记录写到 `docs/superpowers/acceptance-plan6.md`，格式同计划 3。

## 17. 范围与遗留

**本计划做**：§2～§16 全部；关闭计划 1 遗留两条（失联上界推进、空时段）；计划 4 遗留「修改已登记机器」只做机组与展示名。

**上线前核对（写入 `followups-plan6.md`）**：阿里云短信两个新模板；发布时一次性重启全部副本（§15）；是否先把 200 台分组或先全部作为「不限机组」上线；机组 `visible=false` 的用途说明给运营；`display_name` 命名规范。

**明确不做（遗留清单）**
- 多月折扣价 / 机组差异化定价 / 优惠券。
- 预订改期（取消后重订）；候补 / 排队；预订开通成功短信；邮件 / 站内信。
- 后台代用户下单、后台替用户开关自动续费。
- 取消手续费的后台可配置（常量）；按用户的预订上限可配置。
- 预订机器的「硬保证」（指定机器永不改派）；跨商品机组限制。
- 即时订单开通失败自动换机（仍是失败 + 退款）。
- 把 `rack / slot / lan_ip / PDU` 的修改开放到后台。
- 机器日历的 ICS 导出；控制台按机组的历史出租率。
- 机器离线时预订的提前预警（只有开通时刻才改派）；更智能的装箱（目前 best-fit 单次贪心）。
- 自动续费的数量 > 1、升档。

## 18. 任务拆分（供实施计划引用，22 项）
1. 迁移 00019（含有损 Down 说明）；`rental` 结构体 / 常量 / 新状态；`labels.ts` / 类型；`instanceStates`。
2. `tz` 包（Asia/Shanghai）、`occupiedUntil` 非空、`refreshLease` 封顶、`RefreshCappedLeases`；单测。
3. 机组：`fleet` 机组 CRUD / 分配 / 展示名；后台 API + 角色矩阵；审计。
4. `pickMachines`（即时 / 预订分支、best-fit、savepoint 重试）；`fleet.Allocate` 改为委托；`AdminMigrate` 适配；单测（含排他冲突）。
5. `PlaceBatch` + 批次幂等（三路径两表查、`#` 拒绝、`Renew` 补查批次、并发兜底、40P01 / 40001 重试）；`Place` 改为不建批次的包装；`booking:<user>` 咨询锁；`quantity`；`charge` 多单；单测。
6. 退款折算改 `duration × quantity`（`quote`、`refundAll`）；单测。
7. `ProvisionDue`、`provisioned_at`、`startLease` 预订分支；`FailStuckCreating` 谓词；单测。
8. 改派（开通时 / `reassignAfterCreateFailure` / 后台）、`booking_at_risk` / `booking_unfulfilled` / `booking_cancel_failed`、超时取消、`CanRent` 复核；单测。
9. 取消（用户两档 / 后台 / 平台）、`cancel-quote`、短信模板；单测。
10. `renewFits` / `renewable_until` / `renew_options`；`Renew` / `AdminExtend` / 停机续费接入；单测。
11. 自动续费任务、失败标记、降档、短信；单测。
12. 可用性与日历查询（机组 / 机器 / availability）+ 属性测试。
13. 控制台 API（groups、calendar、availability、bookings、cancel、auto-renew、instanceView 字段、events 白名单）。
14. 后台 API（groups、machines PUT、bookings、reassign、cancel-booking + TOTP、dashboard、orders 筛选 / CSV）。
15. `Sweep` 顺序整合；`config` 短信模板变量；README 配置表。
16. sshgw `stateBanner` 显式 reserved 分支与 cancelled 终态、ingress `NotRunningPage` reserved 分支、`ListIngressRoutes` 加 reserved；单测。
17. 集成测试（§16.2：并发一致性、全链路、改派、即时不抢）。
18. 控制台：`RentDialog`（含日历、指定机器、可用性）、市场页接入。
19. 控制台：实例列表（预订 / 批次折叠 / 取消全部）、详情（取消、续费可续至、自动续费卡片、时间线）、帮助页。
20. 后台：机组页、预订页、机器详情档期与编辑、实例页改动、仪表盘、订单页。
21. e2e：钩子、Playwright 用例、`coverage.json`。
22. 真机验收（§16.4）与 `acceptance-plan6.md`、`followups-plan6.md`。

## 附录：评审意见处理（Fable 评审 2026-10-19）

必须修 M1～M4 全部采纳：
- M1：`POST /orders` 不建批次、键原样写在订单上，回放走现有 `replayPlace`；三条路径都查两张表；`checkIdempotencyKey` 拒绝 `#`；`Renew` 补查 `rental_batches`（§6.1、§6.2）。
- M2：新增 `reassignAfterCreateFailure`，明确保留 / 不做的动作、`creating → creating` 换机、主机密钥重生成、无候选时回 `reserved` 且 lease 回 `reserved`（§5.1、§5.4、§16.1、§16.2）。
- M3：真机第 5 条改 S = now + 3d，「可续至」统一为下一预订开始 − 8h（§11.3、§11.4、§16.1、§16.4）。
- M4：同步清单加 `ListIngressRoutes` / `NotRunningPage` / `stateBanner` 显式分支与 cancelled 终态；取消时清主机私钥；单测各加一条（§2、§5.5、§16.1、§18 第 16 项）。

建议 S1～S15 全部采纳：S1 冷静期（B4、§5.5、§13）；S2 封顶差额自动退款（§5.3）；S3 Down 拒绝 reserved 并按外键顺序（§2）；S4 谓词走索引（§5.2）；S5 §14 改为按路径的实际顺序 + 等待链论证 + 40P01 / 40001 重试；S6 `wiping` 进 `RefreshCappedLeases`（§4.3）；S7 `booking:<user>` 咨询锁（§6.1）；S8 换机复制自动续费 / 机组、period 对齐（§8）；S9 开通前 `CanRent`（§5.2）；S10 `startLease` 预订分支限首次开通（§5.3）；S11 `display_name` 部分唯一索引（§2）；S12 `booking_too_soon` 5 分钟容差（§10、§13）；S13 B2 措辞「可见机组」；S14 `booking_cancel_failed` 告警（§5.4）；S15 `renew_blocked` 重试 10 分钟（§7.2、§13）。评审「请知晓」的两点：机器日历匿名可见已接受；帮助页加「价格下单时锁定」（§11.4）。
