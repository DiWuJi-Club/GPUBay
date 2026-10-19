# Spark Cloud 计划 6：档期预订、机组、一次多台与自动续费 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 用户能按机组（或指定机器）看档期日历、预订未来起租的实例（开始前 1 小时自动开通，机器不可用时同机组改派）、一单租 1～8 台（整单成功或失败）、开关自动续费（到期前 1 小时扣款，被后续预订挡住时降档）；运营能管理机组、查看与处置预订（改派、取消退款）；所有加时路径不再能挤掉已付款的预订；最后在 LAN 上的 DGX Spark（192.168.1.221）完成单机验收。

**Architecture:** 不加新进程、不改 Agent 协议。`machine_leases` 成为占用的唯一真相：预订 = 一条 `status='reserved'` 的 lease + 一个 `state='reserved'` 的实例行（下单时钉机器，排他约束兜底防超卖）；所有分配（即时、预订、换机）走 `rental.pickMachines`（lease-aware 候选 + best-fit 排序 + savepoint 换机器）；`refreshLease` 的上界封顶于同机下一条预订；清扫器新增 `RefreshCappedLeases → ProvisionDue → AutoRenewDue`；一次多台 = `rental_batches` + N 条一单一实例的订单，同事务原子；所有加时共用 `renewFits`。接入层只加 `reserved` / `cancelled` 两个状态分支。

**Tech Stack:** Go 1.27（`~/.local/go/bin`）、pgx v5（savepoint 用 `tx.Begin`）、chi v5、embedded-postgres（`dbtest.Main`）；前端 Vue 3 + Element Plus + Vitest + Playwright 1.64（Chrome 通道）。**不新增任何 Go 模块依赖、不新增 npm 依赖。**

**Spec:** `docs/superpowers/specs/2026-10-19-plan6-booking-design.md`（已批准，Fable 评审 2026-10-19，提交 89bbc37）。上位：`2026-10-14-spark-cloud-mvp-design.md` §0.1 第 10 条、§2；计划 3 规格 §2.3 / §4.1；e2e 规范与覆盖表见计划 5 规格 §7.3、§7.5。

**分支：** `feat/plan6-booking`（规格提交 89bbc37）。

**本计划不包含（规格 §17）：** 多月折扣价 / 机组差异化定价 / 优惠券；预订改期；候补 / 排队；预订开通成功短信；邮件 / 站内信；后台代用户下单、后台替用户开关自动续费；取消手续费与预订上限的后台可配置；指定机器的「硬保证」；跨商品机组限制；即时订单开通失败自动换机；后台修改 `rack / slot / lan_ip / PDU`；ICS 导出；按机组的历史出租率；机器离线时预订的提前预警；更智能的装箱；自动续费的数量 > 1 或升档。

## 执行须知（写给每个子代理）

- 任务**按编号顺序串行执行，一个任务一个子代理**。子代理只看得到自己任务的文字，看不到本文其它任务：每个任务的 Interfaces 必须完整（消费什么、产出什么、精确签名），代码步骤给完整代码，不写「同上」「类似 Task N」。
- 测试机 192.168.1.221（`imac` 用户）。**需要 root 的命令由协调人（controller）在运行时执行，绝不写进任何脚本**；密码由负责人在对话中提供，协调人在**运行时**把它放进仅该条命令的环境变量，经 stdin 送给 `sudo -S`。负责人要求密码不进任何持久化存储：**不写进文件、脚本、提交，也不用 Keychain 之类的凭据库**（评审建议的 `security add-generic-password` 方案因此不采用；会话内的运行时环境变量可以接受）。此前「先 `sudo -v` 缓存凭据再执行」的做法在 `ssh -T` 下不可靠（凭据缓存绑定 tty），改用在远端定义一个每次都从变量读密码的 shell 函数 `s`，形如：
  `SUDO_PW='<运行时提供>' sh -c 'printf "%s\n" "$SUDO_PW" | ssh -T imac@192.168.1.221 "read -r P; s() { printf \"%s\n\" \"\$P\" | sudo -S -p \"\" \"\$@\"; }; s systemctl stop spark-agent"'`
  （远端 `read -r P` 从 stdin 读走密码，之后每个 `s <cmd>` 都把它送给 `sudo -S`；`-p ""` 关掉提示符，`-T` 不分配 tty。）**密码绝不写进任何文件、记录、提交或本计划**；记录前核对输出不含密码（`sudo -S` 不回显，`set -x` 不得开启；`git diff --cached | grep -i sudo_pw` 应为空）。
- 开发 Mac 的 DNS 对 `*.sslip.io` 返回假地址（198.18.x.x）：curl 一律加 `--resolve <host>:<port>:<ip>`；Chrome / Playwright 用 `--host-resolver-rules`。
- 测试机访问不了 Docker Hub：拉镜像用 daocloud 镜像站（`docker.m.daocloud.io/...`），Agent 的镜像 ref 保持登记值不变。
- 开发环境短信验证码（mock，固定 `000000`）按手机号每 60 秒只能发一次：脚本化登录前先等 60 秒或换手机号。
- 每个任务结束前必须实际运行并通过：`go vet ./... && go test ./...`；涉及前端的任务再加 `cd web && npm test && npm run build`；页面任务再加该任务列出的 Playwright 用例。证据是命令输出。
- 提交信息结尾空一行加 `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`。

## Global Constraints

- 仓库根目录 `/Users/rifuyiri/Documents/claude/机房搭建/spark-cloud`；所有命令前置 `export PATH=$HOME/.local/go/bin:$HOME/.local/bin:$PATH`。Go 模块名 `spark-cloud`。开发 Mac 没有 Docker；Go 测试用 embedded-postgres（`dbtest.Main`）。
- 预订载体（规格 §1.1 第 1 条）：预订 = 一条 `status='reserved'` 的 `machine_leases` + 一个 `state='reserved'` 的实例行，下单时钉机器；`instances_machine_active_uniq` **不含** `reserved`。
- 时段（规格 §4.1）：`period = [S − PrepLead, S + D + Tail)`，`PrepLead = 1h`，`Tail = RetainAfterStop (6h) + WipeBuffer (1h) = 7h`；即时订单 `period = [now, now + CreateTimeout + D + Tail)`，进入 running 后收缩为 `expires + Tail`。
- 参数（裁定 B1）：最早 **2 小时后**开始（校验 `S ≥ now + 2h − 5m`，容差 5 分钟）、最多提前 **60 天**、单次 `duration × quantity ≤ 90 天`（`1d×1~90`、`1w×1~12`、`1m×1~3`）、一单 **≤ 8 台**、每用户未开通预订 **≤ 16 台**（预订分支取咨询锁 `booking:<user_id>` 后计数）；开始时刻取**整点**（Asia/Shanghai，等价于 UTC 整点）。
- 候选机器（规格 §4.2）：`product_id` 匹配；`group_id IS NULL`（不限）时 `(m.group_id IS NULL OR g.visible)`，指定机组时 `m.group_id = $group`；`machine_id` 指定时只看该机；预订分支 `state IN ('idle','allocated')`（不要求在线）；即时分支 `state = 'idle' AND agent_online`；`maintenance / faulty` 一律排除；`NOT EXISTS` 与 reserved / active 时段相交；ORDER BY「最紧贴前一档期」（`max(upper)` of 同机 ≤ lower 的 reserved/active/ended 时段）DESC NULLS LAST，再 `id`；`LIMIT want FOR NO KEY UPDATE OF m SKIP LOCKED`（**不能用 `FOR UPDATE`**：插入引用 `machines(id)` 的行——`instances`、`machine_leases`、`agent_commands`、`ssh_sessions`——会对机器行取 `FOR KEY SHARE`，与 `FOR UPDATE` 互斥、与 `NO KEY UPDATE` 不互斥；锁机器行的地方只改非键列 `state / group_id / display_name`，`NO KEY UPDATE` 足够）。每台 savepoint 插实例 + lease，排他冲突（`23P01`）→ 回滚 savepoint、剔除、补取（最多 3 轮）；不足 → 整单回滚 `no_capacity`（消息「该时段只有 X 台可用，请减少数量或更换时段」，X=0 时「该时段暂无可用机器」）。
- 封顶（规格 §4.3）：`refreshLease` 的 `upper = GREATEST(lower + 1s, LEAST(desired, cap))`，`cap` = 同机其它实例 reserved lease 的最小 `lower`（`lower ≥ 自己的 lower`），cap 为 NULL 不封顶；`occupiedUntil` 任何分支至少 `now + 1s`；`reserved` 与预订的 `creating` 实例的 lease 不由 `refreshLease` 改写；`refreshLease` 不再可能撞排他约束，`ErrNoCapacity` 分支删除。`RefreshCappedLeases` 每轮对 `stopped`、`wiping`、`running AND suspended_at IS NOT NULL` 的实例各 `refreshLease` 一次（每轮 ≤ 200 行）。
- 开通（规格 §5.2）：`ProvisionDue` 谓词 `state='reserved' AND scheduled_start_at <= $1`（`$1 = now + PrepLead`）；锁顺序 instance → 自己的 lease → machine；先 `Users.CanRent` 失败 → 平台取消全退（事件 `"cancelled: user cannot rent"`）；机器可用 = `state='idle' AND agent_online AND NOT EXISTS 活动实例`；可用 → 机器 `allocated`、实例 `creating`、`provisioned_at = now`、lease `active`、事件 `reserved → creating "scheduled start <S>"`、`Enqueue CreateInstance{image_ref}`；不可用 → 改派。`FailStuckCreating` 谓词 `COALESCE(provisioned_at, created_at) <= now − CreateTimeout`。
- 计时（规格 §5.3）：预订首次开通（`from == creating AND scheduled_start_at IS NOT NULL AND provisioned_at IS NOT NULL`）：`base = max(S, now)`，`expires = min(base + pending, cap − Tail)`；封顶导致 `expires < base + pending` 时按差额 × 最新可退订单单价退款（remark「迟开通补偿 <id>」），不足 1 分不退；事件 `"agent: created (scheduled S, late by Δ)"`（Δ ≤ 0 省略）。停机续费后的 Start 走现有分支。
- 改派（规格 §5.4）：候选 = 预订分支 + `group_id = instances.group_id`（NULL 不限）+ 排除当前机器；开通时刻的改派再要求 `idle AND agent_online`，后台提前改派只要求 lease 不冲突；成功 → 新 lease（开通时 `active`，提前改派 `reserved`）、旧 lease `cancelled`、`machine_id` 换、`reassign_count + 1`、事件 `"reassigned <old> -> <new>: <原因>"`；无候选且 `now < S + 30m` → 保持 reserved + `RaiseAlertOnce(warning, booking_at_risk, …, "实例 <id> 的预订机器不可用且机组内暂无可改派机器")`；否则平台取消 + `RaiseAlert(critical, booking_unfulfilled, …, "实例 <id> 的预订无法履约，已全额退款")`；取消事务失败 → 独立事务 `RaiseAlertOnce(critical, booking_cancel_failed, …, "实例 <id> 的预订取消失败，详情见日志")`。
- 预订实例 Create 失败 / 超时（规格 §5.4 末段）：**不调用** `failCreating`，调 `reassignAfterCreateFailure`：`MarkFaulty(旧机器)` + `RaiseAlert(critical, create_failed)` + `CancelCommands`；不改 `pending_seconds`、不清 `ssh_host_key_enc`、不退款、lease 不置 ended。有候选 → 旧 lease `cancelled`、新 lease `active`、`machine_id` 换、`provisioned_at = now`、状态仍 `creating`、`reassign_count + 1`、**重新生成主机密钥**、事件 `creating → creating "reassigned <old> -> <new>: create failed"`、`Enqueue CreateInstance`。无候选且未过 grace → 实例回 `reserved`、`provisioned_at = NULL`、旧 lease `status` 改回 `reserved`（钉在 faulty 机器上）、`booking_at_risk`、事件 `creating → reserved "create failed, waiting for machine"`。过 grace → 平台取消。
- 取消（规格 §5.5，裁定 B4）：用户 `POST /instances/{id}/cancel` 仅 `state='reserved' AND now < S − PrepLead`，否则 `cancel_not_allowed`；退款比例：`now < created_at + 15m` → 100%；否则 `S − now ≥ 24h` → 100%，不然 50%（向下取整到分）；后台 `cancel-booking`（ops + TOTP，原因必填）与平台取消 100%。都把实例置 `cancelled`（`cancelled_at = now`、`ssh_host_key_enc = NULL`）、lease `cancelled`、按订单从新到旧 `refundAcross`（`ref_id = <order>-r<n>`）、写事件；审计 `booking.cancel`（user）/ `booking.admin_cancel`（admin）。平台取消发短信 `TemplateBookingCancelled`。
- 批次（规格 §6）：`PlaceBatch` 一个事务：校验 → 幂等咨询锁 → [预订分支 `booking:<uid>` 咨询锁 + 16 上限] → 取镜像 → 挑机器 / 逐台 savepoint → 建批次（仅 `POST /bookings`）→ 每台 `charge(kind='new', quantity)` → 即时每台 `Enqueue`。`POST /orders`（`Place`）= `PlaceBatch{count 1, quantity 1, 立即, 不建批次}`：不写 `rental_batches`、`orders.batch_id = NULL`、`orders.idempotency_key = key`、回放走 `replayPlace`，响应与现状完全相同。
- 幂等（规格 §6.2）：三条路径共用咨询锁 `idem:<user>:<key>`，锁内都先查 `rental_batches` 再查 `orders`：`/bookings` 命中批次回放、命中订单 `idempotency_conflict`；`/orders` 命中 `kind='new' AND batch_id IS NULL` 的订单回放、命中批次或续费单冲突；`/renew` 命中批次冲突、其余 `replayRenew`。子订单键 `key || '#' || n`；`checkIdempotencyKey` **拒绝含 `#` 的键**（400「幂等键不能包含 #」）。并发重复提交由 `rental_batches_user_idempotency_uniq` 兜底回放。
- 续费冲突（规格 §7.1）：`renewFits` 新上界：running 未暂停 `max(expires, now) + secs + Tail`；暂停 `now + paused + secs + Tail`；即时 creating / stopped `now + CreateTimeout + pending + secs + Tail`；reserved / 预订 creating `S + pending + secs + Tail`；要求 `≤ cap`，否则 `renew_blocked`（409「该机器自 <cap + PrepLead> 起已有预订，本次最多可续至 <cap − Tail>」，时间 `2006-01-02 15:04` 北京时间）。`Renew` / `AdminExtend` / 自动续费 / 停机续费都先过它再 `addTime + refreshLease`。实例视图 `renewable_until = cap − Tail`、`renew_options[{duration, quantity_max}]`（受 `renewable_until` 与 90 天双重限制，0 = 不可选）。
- 自动续费（规格 §7.2，裁定 B5）：`auto_renew` / `auto_renew_duration`（默认 = 首单档位）；`AutoRenewDue` 谓词 `state='running' AND suspended_at IS NULL AND auto_renew AND expires_at <= now + 1h AND (auto_renew_failed_at IS NULL OR auto_renew_failed_at <= now − (renew_blocked ? 10m : 1m))`；档位从 `auto_renew_duration` 起按 `1m → 1w → 1d` 找第一个 `renewFits` 的（只降不升）；`charge(kind='renew', auto=true, quantity=1)`；成功清失败标记、事件 `"auto renew <order> (<档位>)"`；失败在**独立事务**写 `auto_renew_failed_at / reason`，首次失败发短信 `TemplateAutoRenewFailed{instance, reason}`；停机后不自动续；`AutoRenewDue` 在 `ExpireDue` 之前、`SendReminders` 之前。
- 机组（规格 §3，裁定 B8 / B9）：`machine_groups(name UNIQUE, region, description, sort, visible)`；`machines.group_id`（NULL = 未分组）、`machines.display_name`（非空时部分唯一 `machines_display_name_uniq`，后台 `duplicate_display_name`）；未分组机器只参与「不限机组」；`visible=false` 的机组控制台不可见、不可下单、不参与不限机组；删除机组要求无机器且无未终态实例引用（`group_not_empty`）；机器编辑接口只开放 `group_id` 与 `display_name`；控制台 `label` = `display_name` 或 `机器 #<id>`。
- 迁移 00019（规格 §2）：建表 / 加列 / 索引 / `instances_state_check` 重建（加 `reserved`、`cancelled`）/ 先清空时段再加 `machine_leases_period_nonempty`；Down 先 `RAISE EXCEPTION` 拒绝仍有 `reserved` 实例、`cancelled → failed`、按外键顺序删列删表、恢复旧 CHECK。
- 枚举同步（规格 §2 M4）：`rental.instanceStates`、`labels.ts`、`types/instance.ts`、`ListIngressRoutes` 状态列表加 `reserved`（不加 `cancelled`）、`ingress.NotRunningPage` 显式 `reserved` 分支、`sshgw.stateBanner` 显式 `reserved` 分支（不能靠 default）、sshgw `terminalState` 加 `cancelled`、Banner / 页面里的 `<S>` 按 Asia/Shanghai `2006-01-02 15:04`；`reconcile.go` 预期实例查询保持四个活动状态。
- 换机（规格 §8）：`AdminMigrate` period `[now, now + CreateTimeout + 剩余 + Tail)`；新实例复制 `auto_renew`、`auto_renew_duration`、`group_id`（`batch_id` 不复制，`scheduled_*` / `provisioned_at` NULL）；预订实例不可换机。
- 退款折算（规格 §8）：`refund.go` 的 `quote` 与 `refundAll` 改用 `seconds(duration) × quantity`。
- 加锁顺序（规格 §14，写进 `service.go` 头注释）：所有机器行锁一律 `FOR NO KEY UPDATE`（见上一条）；PlaceBatch `idem 咨询锁 → [booking 咨询锁] → machines(NO KEY UPDATE SKIP LOCKED) → 插实例 / lease → orders → wallet`；ProvisionDue `instance → 自己的 lease → machine(NO KEY UPDATE) → [改派 machines SKIP LOCKED] → agent_commands`；后台改派 `instance → lease → machines(SKIP LOCKED)`；Renew / AdminExtend / 自动续费 `[idem] → instance → 自己的 lease(读 cap) → orders → wallet → 更新 lease`（不锁机器）；取消 `instance → lease → orders → wallet`；HandleCommandResult `instance → agent_commands → lease → machine(Release)`；`PlaceBatch` 与 `reassign` 遇 `40P01` / `40001` 整个事务重试一次。
- 错误码新增（规格 §10）：`booking_too_soon`（400「开始时间至少在 2 小时之后」）、`booking_too_far`（400「最多提前 60 天预订」）、`booking_too_long`（400「单次租期最长 90 天」）、`booking_not_hour`（400「开始时间需为整点」）、`booking_limit`（409「未开通的预订最多 16 台」）、`count_invalid`（400「一次最多租用 8 台」）、`renew_blocked`（409）、`group_not_found`（404「机组不存在」）、`machine_unavailable`（409「该机器在所选时段不可用」）、`cancel_not_allowed`（409「预订已开始开通，不能取消」）、`group_not_empty`（409「机组内还有机器或未结束的实例」）、`duplicate_group_name`（409「机组名称已存在」）、`duplicate_display_name`（409「展示名已被其他机器使用」）。
- 控制台 API（规格 §10.1）：`GET /groups?product_id=`、`GET /groups/{id}/machines`、`GET /groups/{id}/calendar?from&days`、`GET /machines/{id}/calendar?from&days`、`GET /availability?…`（五个在 `requireUser` 组外、可匿名、按 IP 每分钟 120 次）；`POST /bookings`、`GET /bookings/{batch_id}`、`GET /instances/{id}/cancel-quote`、`POST /instances/{id}/cancel`、`PUT /instances/{id}/auto-renew`、`POST /instances/{id}/renew`（加 `quantity`）、`GET /instances/{id}/events`；`instanceView` 新字段 `scheduled_start_at, scheduled_end_at, cancellable_until, group_id, group_name, machine_label, batch_id, reassign_count, auto_renew, auto_renew_duration, auto_renew_failed_reason, renewable_until, renew_options`；`Order` 视图 `quantity, batch_id, auto`；`productView.idle_count` = 此刻可即时租用台数。
- 后台 API（规格 §10.2）：机组 CRUD（ops）、`POST/DELETE /groups/{id}/machines`、`PUT /machines/{id}`、`GET /machines/{id}` 加 `group / display_name / leases`、`GET /machines/{id}/calendar`、`GET /bookings`、`GET /bookings/upcoming?days=7`（all）、`POST /instances/{id}/cancel-booking`（ops + TOTP）、`POST /instances/{id}/reassign`（ops）、`GET /dashboard` 加 `bookings_upcoming_7d`、`GET /orders` 筛选 `batch_id` + CSV 列、`POST /machines/{id}/maintenance` 响应 `upcoming_bookings`。
- 配置（规格 §13）：常量全在 `internal/rental/booking.go`；`SPARK_SMS_TPL_AUTO_RENEW_FAILED` / `SPARK_SMS_TPL_BOOKING_CANCELLED` 在 `SPARK_SMS_DRIVER=aliyun` 时必填；时区 `Asia/Shanghai` 固定（`internal/platform/tz`）。
- 告警文案稳定（不含数值）：`booking_at_risk`、`booking_unfulfilled`、`booking_cancel_failed`；`create_failed` 沿用。审计动作：`group.create / update / delete / assign / unassign`、`machine.update`、`booking.cancel`（user）、`booking.admin_cancel`、`booking.reassign`、`instance.auto_renew`（user）。
- 时间一律 `clock.Clock`；错误用 `apperr`；生产禁止 mock。
- 端到端规范沿用计划 5：用例标题以覆盖表 ID 开头、`data-testid` 命名 `页面-元素`、禁止 `waitForTimeout`、`workers: 1`；`web/e2e/coverage.json` 与覆盖表一致；现有 Playwright 用例必须保持全绿。

## Review Focus

1. **两个用户同时抢同一时段的最后几台**（50 个并发批次、每单 1～8 台、随机窗口，只有 10 台机器）：任一时刻同机时段两两不交、没有「订单数 ≠ 批次台数」的批次、没有死锁或 500 → Task 16 `TestConcurrentBatchesConsistent`。
2. **`NOT EXISTS` 预筛通过但插 lease 时撞上刚提交的重叠时段**（READ COMMITTED 快照滞后）：savepoint 回滚、换下一台，整单仍成功；候选全部撞光时整单回滚且钱不动 → Task 4 `TestPlaceOnMachinesRetriesOnExclusionViolation`。
3. **开始时间卡在边界**（`now + 2h − 4m59s` 接受、`now + 2h − 5m01s` 拒绝、第 60 天整点接受、60 天 + 1 小时拒绝、`1m × 3` 接受、`1m × 4` 拒绝、非整点拒绝、UTC 偏移后的整点仍是整点）→ Task 5 `TestBatchValidationMatrix`。
4. **续费被同机下一条预订挡住时不能扣钱**（手动续费、后台延长、停机续费、自动续费四条路径）：返回 `renew_blocked`、余额与 `expires_at` 不变、lease 不变 → Task 9 `TestRenewBlockedByReservationDoesNotCharge`（预订已存在）+ `TestRenewRaceWithNewReservationIsBlocked`（`renewFits` 之后才提交的预订：`verifyLeaseFits` 回滚扣款）+ Task 10 `TestAutoRenewFallsBackThenBlocks`。
5. **旧客户端继续用 `POST /orders` + 同一个幂等键重试**：第二次得到第一次的订单与实例（不建批次、不报冲突）；用户提交含 `#` 的键被 400 拒绝而不是撞上批次子单 → Task 5 `TestLegacyOrderIdempotencyUnchanged`。

## 已做出的裁定（规格未写明或与代码核对后确定的细节）

- **`fleet.Allocate` 删除**，替换为 `fleet.MarkAllocated(ctx, q, id)`（只把一台已选中的机器从 idle 置 allocated）；候选选择全部在 `rental.pickMachines`（fleet 不能依赖 rental，反向做不到）。`fleet` 原有 `TestAllocateRequiresIdleAndOnline` / `TestConcurrentAllocationNeverDoubleAssigns` 改为 `TestMarkAllocatedRequiresIdleAndOnline`，并发不重复分配由 Task 16 的 `TestConcurrentBatchesConsistent` 覆盖（现有 `rental` 的 `TestConcurrentPlaceForLastMachines` 仍覆盖单机竞争）。
- **`fleet.IdleCount` 保留**（机组的「现空闲」用它），市场页的 `idle_count` 改用 `rental.AvailableNow(ctx, productID)`（lease-aware）。
- **`no_capacity` 消息里的台数 X 是本次实际拿到的台数**（`placeOnMachines` 的 `len(done)`），不另起一次 `countAvailable`：候选补取 3 轮后拿不到的机器要么已被别人占、要么撞了约束，与「此刻可用台数」在实践中一致，少一次查询；规格 §10 的「可用台数」按此理解。
- **`machines.group_id` 的三态更新**：`fleet.MachineUpdate{GroupID *int64; SetGroup bool; DisplayName *string}`，`SetGroup=true, GroupID=nil` 表示移出机组。
- **机组删除放在 `rental.AdminDeleteGroup`**：先查无未终态实例引用，再调 `fleet.DeleteGroupIn(ctx, tx, id)`（查无机器并删除），同一事务；审计在 rental 写。
- **预订实例的 lease 读写**：`rental.lockLease(ctx, tx, instanceID)` 锁定并返回实例当前的 reserved / active lease 行（`leaseRow{ID, MachineID, Lower, Upper, Status}`）。
- **主机密钥重生成**用现有 `s.d.HostKeyGen()`；改派后 `ssh_host_pubkey` 与 `ssh_host_key_enc` 同时覆盖。
- **自动续费失败的独立事务**：`AutoRenewDue` 的每行先在主事务里尝试，失败时主事务回滚，然后用 `s.d.Pool.Exec` 单独写失败标记（不再锁实例；`UPDATE … WHERE id = $1 AND state = 'running' AND auto_renew`）。
- **短信模板键**：`sms.TemplateAutoRenewFailed = "auto_renew_failed"`（参数 instance、reason）、`sms.TemplateBookingCancelled = "booking_cancelled"`（参数 instance、reason）；`reason` 传中文短句（「余额不足」/「机器已有后续预订」/「无法安排机器」/「账号不可租用」）。
- **`rental.Notifier` 扩展**为三个方法；`internal/app` 的 `expiryNotifier` 实现全部；各测试包的 fake 同步加方法。
- **`Renew` 保持 5 参数包装不变，新增 `RenewQuantity(ctx, userID, instanceID, d, quantity, idempotencyKey)`**（`quantity` 0 视为 1）；现有调用点与 Task 9 / 10 的测试都继续用 5 参数的 `Renew`，不要改它的签名。
- **`POST /orders` 的内部实现**：`Place` 调 `placeBatch(ctx, req, false)`；`PlaceBatch` 调 `placeBatch(ctx, req, true)`；两者差别只有「是否建批次行、子单键是否加 `#n`」。
- **日历 `from` 缺省**：今天（Asia/Shanghai），`days` 缺省 61、上限 61；机器日历只返回 `[from, from + days)` 内相交的时段。
- **`GET /availability` 匿名可用**，`start_at` 为 RFC3339；缺省立即分支（`period = [now, now + CreateTimeout + D + Tail)`）。
- **控制台 `RentDialog`** 是新组件 `web/console/src/components/RentDialog.vue`，市场页「立即租用」与「预订档期」两个按钮都打开它；原 `market-confirm-dialog` 保留在 RentDialog 内（testid 不变，现有 C-MKT 用例不改）。
- **e2e 钩子**：`/hooks/machines` 增加 `group_id?`；新增 `POST /hooks/groups {name, visible?}` → `{id}`（直接 `Fleet.CreateGroup`）。
- **真机检查点 A（Task 17）在前端之前**：后端 + API 完成后先上机用 curl 验证到点开通、续费被挡、自动续费、取消退款；页面问题留到 B。

## 文件结构

```
spark-cloud/
├── migrations/00019_booking.sql                                   # Task 1
├── internal/
│   ├── platform/tz/tz.go                                          # Task 2
│   ├── platform/apperr/apperr.go                                  # Task 1（错误码）
│   ├── rental/service.go、booking.go、leases.go、alloc.go、batch.go、idempotency.go、orders.go、place.go、migrate.go、refund.go、
│   │          cancel.go、provision.go、reassign.go、renew.go、autorenew.go、calendar.go、groups.go、events.go、admin_bookings.go、sweeper.go、results.go、admin_queries.go、ports.go  # Task 1～13、15
│   ├── fleet/machines.go、groups.go                               # Task 3、4
│   ├── plugins/sms/sms.go、platform/config/config.go、app/app.go  # Task 7、12、14
│   ├── sshgw/auth.go、lifecycle.go、ingress/pages.go、api/ingress.go  # Task 15
│   ├── api/server.go、instances.go、market.go、booking.go         # Task 12
│   ├── adminapi/server.go、instances.go、machines.go、groups.go、bookings.go、dashboard.go、finance.go、views.go  # Task 3、13
│   ├── app/booking_e2e_test.go、rental/concurrency_test.go、e2ehooks/hooks.go  # Task 16
├── web/shared/src/types/{instance,admin,booking}.ts、labels.ts   # Task 18、20
├── web/console/src/components/RentDialog.vue、logic/{booking,instance}.ts、views/{MarketView,InstancesView,InstanceDetailView,HelpView}.vue  # Task 18、19
├── web/admin/src/{menu,router}.ts、logic/instanceOps.ts、components/FinancePage.vue、views/{GroupsView,BookingsView,MachineDetailView,InstanceDetailView,DashboardView,OrdersView}.vue  # Task 20
├── web/e2e/{helpers/{hooks,booking}.ts,console/booking.spec.ts,console/instance-detail.spec.ts,console/instances.spec.ts,admin/groups.spec.ts,admin/bookings.spec.ts,admin/instances.spec.ts,coverage.json}、web/playwright.config.ts  # Task 21
├── README.md                                                      # Task 14、23
└── docs/superpowers/{acceptance-plan6.md,followups-plan6.md,followups-plan1.md,followups-plan4.md}      # Task 17、22、23
```

## 任务依赖

Task 1（迁移 + 类型）→ 2（封顶）→ 3（机组）→ 4（候选与分配）→ 5（批次）→ 6（退款折算）→ 7（取消 + 短信）→ 8（开通 + 改派）→ 9（续费冲突）→ 10（自动续费）→ 11（日历）→ 12（控制台 API）→ 13（后台 API）→ 14（Sweep / config / README）→ 15（接入层）→ 16（集成测试 + 钩子）→ **17 真机检查点 A** → 18（RentDialog）→ 19（控制台实例页）→ 20（后台页面）→ 21（Playwright）→ **22 真机验收 B + 记录** → 23（收尾）。按编号顺序执行，共 23 个任务。

---
### Task 1: 迁移 00019、`rental` 新状态 / 字段 / 常量、错误码

**Files:**
- Create: `migrations/00019_booking.sql`、`internal/rental/booking.go`
- Modify: `internal/rental/service.go`（`Instance`、`instanceCols`、`scanInstance`、头注释）、`internal/rental/orders.go`（`Order`、`orderCols`、`scanOrder`）、`internal/rental/admin_queries.go`（`instanceStates`）、`internal/rental/refund.go`（`remainingSeconds`）、`internal/platform/apperr/apperr.go`
- Test: `internal/rental/booking_test.go`（新建）

**Interfaces:**
- Produces:
  - 表 `machine_groups`、`rental_batches`；`machines.group_id BIGINT NULL`、`machines.display_name TEXT NOT NULL DEFAULT ''`、部分唯一索引 `machines_display_name_uniq`；`orders.batch_id TEXT NULL`、`orders.quantity INT NOT NULL DEFAULT 1`、`orders.auto BOOLEAN NOT NULL DEFAULT false`；`instances` 新列 `scheduled_start_at, group_id, batch_id, auto_renew, auto_renew_duration, auto_renew_failed_at, auto_renew_failed_reason, reassign_count, provisioned_at, cancelled_at`；`instances_state_check` 含 `reserved` / `cancelled`；`machine_leases` 新索引与 `machine_leases_period_nonempty`。
  - `rental.StateReserved = "reserved"`、`rental.StateCancelled = "cancelled"`。
  - `rental.Instance` 新字段：`ScheduledStartAt *time.Time; GroupID *int64; BatchID *string; AutoRenew bool; AutoRenewDuration catalog.Duration; AutoRenewFailedAt *time.Time; AutoRenewFailedReason string; ReassignCount int; ProvisionedAt *time.Time; CancelledAt *time.Time`；方法 `(Instance) Booked() bool`（`ScheduledStartAt != nil`）。
  - `rental.Order` 新字段：`Quantity int \`json:"quantity"\``、`BatchID *string \`json:"batch_id"\``、`Auto bool \`json:"auto"\``。
  - `internal/rental/booking.go` 常量：`MinBookingLead = 2h`、`BookingLeadTolerance = 5m`、`MaxBookingAhead = 60 * 24h`、`MaxLeaseSpan = 90 * 24h`、`MaxMachinesPerOrder = 8`、`MaxOpenReservationsPerUser = 16`、`PrepLead = 1h`、`BookingGrace = 30m`、`Tail = RetainAfterStop + WipeBuffer`、`CancelCoolOff = 15m`、`FreeCancelBefore = 24h`、`LateCancelRefundPercent = 50`、`AutoRenewLead = 1h`、`AutoRenewRetry = 1m`、`AutoRenewRetryBlocked = 10m`；函数 `MaxQuantity(d catalog.Duration) int`（1d→90、1w→12、1m→3、其它 0）。
  - `apperr` 新错误：`ErrBookingTooSoon`、`ErrBookingTooFar`、`ErrBookingTooLong`、`ErrBookingNotHour`、`ErrBookingLimit`、`ErrCountInvalid`、`ErrGroupNotFound`、`ErrMachineUnavailable`、`ErrCancelNotAllowed`、`ErrGroupNotEmpty`、`ErrDuplicateGroupName`、`ErrDuplicateDisplayName`（`renew_blocked` 带动态消息，在 Task 10 用 `apperr.New` 现场构造）。

- [ ] **Step 1: 写失败的测试**

`internal/rental/booking_test.go`：

```go
package rental_test

import (
	"testing"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/rental"
)

func TestBookingColumnsDefaults(t *testing.T) {
	e := newEnv(t, 1)
	ord, inst := e.place(catalog.D1d)
	got := e.get(inst.ID)
	if got.Booked() || got.ScheduledStartAt != nil || got.GroupID != nil || got.BatchID != nil || got.AutoRenew ||
		!got.AutoRenewDuration.Valid() || got.ReassignCount != 0 || got.ProvisionedAt != nil || got.CancelledAt != nil {
		t.Fatalf("defaults = %+v", got)
	}
	// 列默认值（Task 4 起 Place 会显式写入首单档位，所以默认值用一行不带该列的裸插入来验）
	var def string
	if err := e.pool.QueryRow(e.ctx, `INSERT INTO instances (id, user_id, machine_id, product_id, image_id, state, ssh_password_enc, created_at, updated_at)
		VALUES ('ins-defaults', $1, $2, $3, $4, 'released', $5, $6, $6) RETURNING auto_renew_duration`,
		e.userID, e.machines[0], e.productID, e.imageID, []byte("x"), e.clock.Now()).Scan(&def); err != nil || def != "1m" {
		t.Fatalf("auto_renew_duration default = %q, %v", def, err)
	}
	if ord.Quantity != 1 || ord.BatchID != nil || ord.Auto {
		t.Fatalf("order defaults = %+v", ord)
	}
	// 新状态能写入；旧状态照常
	if _, err := e.pool.Exec(e.ctx, `UPDATE instances SET state = 'reserved' WHERE id = $1`, inst.ID); err != nil {
		t.Fatalf("reserved rejected by CHECK: %v", err)
	}
	if _, err := e.pool.Exec(e.ctx, `UPDATE instances SET state = 'cancelled' WHERE id = $1`, inst.ID); err != nil {
		t.Fatalf("cancelled rejected by CHECK: %v", err)
	}
	if _, err := e.pool.Exec(e.ctx, `UPDATE instances SET state = 'bogus' WHERE id = $1`, inst.ID); err == nil {
		t.Fatal("bogus state accepted")
	}
	var tables int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM information_schema.tables WHERE table_name IN ('machine_groups', 'rental_batches')`).Scan(&tables); err != nil || tables != 2 {
		t.Fatalf("new tables = %d, %v", tables, err)
	}
	// 空时段被 CHECK 拒绝
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO machine_leases (machine_id, instance_id, period, status, created_at, updated_at)
		VALUES ($1, $2, tstzrange(now(), now()), 'ended', now(), now())`, e.machines[0], inst.ID); err == nil {
		t.Fatal("empty period accepted")
	}
	// display_name 非空时唯一
	if _, err := e.pool.Exec(e.ctx, `UPDATE machines SET display_name = 'A-01' WHERE id = $1`, e.machines[0]); err != nil {
		t.Fatal(err)
	}
	m2 := e.newMachine("SN-DUP")
	if _, err := e.pool.Exec(e.ctx, `UPDATE machines SET display_name = 'A-01' WHERE id = $1`, m2); err == nil {
		t.Fatal("duplicate display_name accepted")
	}
	if _, err := e.pool.Exec(e.ctx, `UPDATE machines SET display_name = '' WHERE id = $1`, m2); err != nil {
		t.Fatalf("empty display_name must not collide: %v", err)
	}
}

func TestMaxQuantity(t *testing.T) {
	cases := map[catalog.Duration]int{catalog.D1d: 90, catalog.D1w: 12, catalog.D1m: 3, catalog.Duration("x"): 0}
	for d, want := range cases {
		if got := rental.MaxQuantity(d); got != want {
			t.Fatalf("MaxQuantity(%s) = %d, want %d", d, got, want)
		}
	}
}
```

`internal/rental/helpers_test.go` 追加一个 helper（放在文件末尾）：

```go
// newMachine 再登记一台空闲在线机器并加入 e.machines。
func (e *env) newMachine(serial string) int64 {
	e.t.Helper()
	id := dbtest.CreateMachine(e.t, e.pool, serial)
	e.machines = append(e.machines, id)
	return id
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/rental/ -run 'TestBookingColumnsDefaults|TestMaxQuantity' 2>&1 | head -20`
Expected: 编译错误（`Booked`、`MaxQuantity`、`Quantity` 未定义）。

- [ ] **Step 3: 写迁移**

`migrations/00019_booking.sql`：

```sql
-- +goose Up
-- 计划 6：机组、预订（reserved 实例 + reserved lease）、批次、自动续费。
-- Down 有损：cancelled 实例映射为 failed；仍有 reserved（已付款未退）实例时 Down 直接拒绝，请先在后台取消预订。

CREATE TABLE machine_groups (
    id          BIGSERIAL PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,            -- 对用户展示，1～32 字符
    region      TEXT NOT NULL DEFAULT '',        -- 展示用位置标签，≤ 32 字符
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
CREATE UNIQUE INDEX machines_display_name_uniq ON machines (display_name) WHERE display_name <> '';

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
    ADD COLUMN scheduled_start_at       TIMESTAMPTZ,                 -- 预订开始时刻；即时订单为 NULL
    ADD COLUMN group_id                 BIGINT REFERENCES machine_groups (id),  -- 下单时选择的机组（改派范围）；NULL = 不限
    ADD COLUMN batch_id                 TEXT REFERENCES rental_batches (id),
    ADD COLUMN auto_renew               BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN auto_renew_duration      TEXT NOT NULL DEFAULT '1m' CHECK (auto_renew_duration IN ('1d', '1w', '1m')),
    ADD COLUMN auto_renew_failed_at     TIMESTAMPTZ,                 -- 最近一次自动续费失败（成功后清空）
    ADD COLUMN auto_renew_failed_reason TEXT NOT NULL DEFAULT '',    -- insufficient_balance / renew_blocked
    ADD COLUMN reassign_count           INT NOT NULL DEFAULT 0,      -- 改派次数
    ADD COLUMN provisioned_at           TIMESTAMPTZ,                 -- 预订开通（reserved → creating）时刻；即时订单为 NULL
    ADD COLUMN cancelled_at             TIMESTAMPTZ;
CREATE INDEX instances_reserved_idx ON instances (scheduled_start_at) WHERE state = 'reserved';
CREATE INDEX instances_auto_renew_idx ON instances (expires_at) WHERE state = 'running' AND auto_renew;

-- 先清掉 occupiedUntil 回退为 now 产生的空时段（isempty 的 range 没有 lower，用 created_at 代替），否则 CHECK 建不起来
UPDATE machine_leases SET period = tstzrange(created_at, created_at + interval '1 second'), status = 'ended' WHERE isempty(period);
CREATE INDEX machine_leases_machine_period_idx ON machine_leases (machine_id, lower(period)) WHERE status IN ('reserved', 'active');
CREATE INDEX machine_leases_reserved_idx ON machine_leases (lower(period)) WHERE status = 'reserved';
ALTER TABLE machine_leases ADD CONSTRAINT machine_leases_period_nonempty CHECK (NOT isempty(period) AND lower(period) < upper(period));

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM instances WHERE state = 'reserved') THEN
        RAISE EXCEPTION '仍有未开通的预订（已付款未退），请先在后台取消预订再回滚';
    END IF;
END $$;
-- +goose StatementEnd
UPDATE machine_leases SET status = 'cancelled' WHERE instance_id IN (SELECT id FROM instances WHERE state = 'cancelled') AND status IN ('reserved', 'active');
UPDATE instances SET state = 'failed' WHERE state = 'cancelled';
ALTER TABLE machine_leases DROP CONSTRAINT machine_leases_period_nonempty;
DROP INDEX machine_leases_reserved_idx;
DROP INDEX machine_leases_machine_period_idx;
DROP INDEX instances_auto_renew_idx;
DROP INDEX instances_reserved_idx;
ALTER TABLE instances
    DROP COLUMN cancelled_at, DROP COLUMN provisioned_at, DROP COLUMN reassign_count, DROP COLUMN auto_renew_failed_reason,
    DROP COLUMN auto_renew_failed_at, DROP COLUMN auto_renew_duration, DROP COLUMN auto_renew, DROP COLUMN batch_id,
    DROP COLUMN group_id, DROP COLUMN scheduled_start_at;
ALTER TABLE instances DROP CONSTRAINT instances_state_check;
ALTER TABLE instances ADD CONSTRAINT instances_state_check
    CHECK (state IN ('creating', 'running', 'stopped', 'wiping', 'released', 'failed'));
DROP INDEX orders_batch_idx;
ALTER TABLE orders DROP COLUMN auto, DROP COLUMN quantity, DROP COLUMN batch_id;
DROP INDEX machines_display_name_uniq;
DROP INDEX machines_group_idx;
ALTER TABLE machines DROP COLUMN display_name, DROP COLUMN group_id;
DROP TABLE rental_batches;
DROP TABLE machine_groups;
```

- [ ] **Step 4: `rental` 常量文件**

`internal/rental/booking.go`：

```go
package rental

import (
	"time"

	"spark-cloud/internal/catalog"
)

// 计划 6：档期预订、批次、取消与自动续费的参数（规格 §13）。全部是代码常量，不做配置。
const (
	MinBookingLead             = 2 * time.Hour       // 最早开始：下单后 2 小时
	BookingLeadTolerance       = 5 * time.Minute     // booking_too_soon 的容差（对话框打开到提交之间）
	MaxBookingAhead            = 60 * 24 * time.Hour // 最多提前 60 天
	MaxLeaseSpan               = 90 * 24 * time.Hour // 单次 duration × quantity 上限
	MaxMachinesPerOrder        = 8
	MaxOpenReservationsPerUser = 16 // 未开通（reserved）实例数
	PrepLead                   = time.Hour      // 开始前多久开通
	BookingGrace               = 30 * time.Minute // 开始后仍无机器 → 平台取消
	Tail                       = RetainAfterStop + WipeBuffer
	CancelCoolOff              = 15 * time.Minute     // 下单后冷静期：全额退
	FreeCancelBefore           = 24 * time.Hour       // 距开始 ≥ 24h 全额退
	LateCancelRefundPercent    = 50                   // 否则退 50%
	AutoRenewLead              = time.Hour            // 到期前 1 小时扣款
	AutoRenewRetry             = time.Minute          // 余额不足的重试间隔
	AutoRenewRetryBlocked      = 10 * time.Minute     // renew_blocked 的重试间隔
)

// MaxQuantity 返回某档位在 90 天上限下最多能买几个单位。
func MaxQuantity(d catalog.Duration) int {
	if !d.Valid() {
		return 0
	}
	return int(MaxLeaseSpan / d.Length())
}
```

- [ ] **Step 5: `rental.Instance` / `Order` 新字段、状态常量、头注释**

`internal/rental/service.go` 头注释改为：

```go
// 各路径的实际加锁顺序（规格 §14）：
//   PlaceBatch：idem 咨询锁 → [booking:<uid> 咨询锁] → machines(SKIP LOCKED) → 插实例 / lease → orders → wallet
//   ProvisionDue：instance → 自己的 lease → machine(NO KEY UPDATE) → [改派：machines SKIP LOCKED] → agent_commands
//   机器行锁一律 FOR NO KEY UPDATE：插入引用 machines(id) 的行会取 FOR KEY SHARE，与 FOR UPDATE 互斥、与 NO KEY UPDATE 不互斥。
//   后台改派：instance → lease → machines(SKIP LOCKED)
//   Renew / AdminExtend / 自动续费：[idem] → instance → 自己的 lease(读 cap) → orders → wallet → 更新 lease（不锁机器）
//   取消：instance → lease → orders → wallet
//   HandleCommandResult：instance → agent_commands → lease(refreshLease) → machine(Release)
//   ExpireDue / WipeStale / Suspend / Resume / RefreshCapped / beginWipe：instance → lease → agent_commands
//   AdminMigrate：原实例 → machines(SKIP LOCKED) → 新 lease → agent_commands → 旧 lease 收缩
// 持机器行锁的事务只会在插入 lease 撞到别的事务未提交的 lease 增长时等待；而让 lease 增长的路径都不等机器锁，等待链不成环。
```

常量块追加：

```go
	StateReserved  = "reserved"  // 计划 6：已预订、未开通
	StateCancelled = "cancelled" // 计划 6：开通前取消（终态）
```

`Instance` 结构体改为：

```go
type Instance struct {
	ID              string
	UserID          int64
	MachineID       int64
	ProductID       int64
	ImageID         int64
	State           string
	PendingSeconds  int64
	ExpiresAt       *time.Time
	StoppedAt       *time.Time
	SuspendedAt     *time.Time
	PausedRemaining *int64
	Reminded24h     bool
	Reminded1h      bool
	CreatedAt       time.Time
	// 计划 3：密码登录开关与平台生成的容器主机公钥（authorized_keys 一行；存量实例为空，首连信任后写回）
	PasswordAuth  bool
	SSHHostPubkey string
	// 计划 6：预订与自动续费
	ScheduledStartAt      *time.Time       // 预订开始时刻；即时订单为 nil
	GroupID               *int64           // 下单时选的机组（改派范围）；nil = 不限
	BatchID               *string          // 一次多台的批次；单台旧接口为 nil
	AutoRenew             bool
	AutoRenewDuration     catalog.Duration // 自动续费档位（默认 = 首单档位）
	AutoRenewFailedAt     *time.Time
	AutoRenewFailedReason string // insufficient_balance / renew_blocked
	ReassignCount         int
	ProvisionedAt         *time.Time // 预订开通（reserved → creating）时刻
	CancelledAt           *time.Time
}

// Booked 表示这是一个预订（有预定开始时刻）的实例。
func (i Instance) Booked() bool { return i.ScheduledStartAt != nil }
```

`instanceCols` 与 `scanInstance`：

```go
const instanceCols = `id, user_id, machine_id, product_id, image_id, state, pending_seconds, expires_at,
	stopped_at, suspended_at, paused_remaining, reminded_24h, reminded_1h, created_at, password_auth, ssh_host_pubkey,
	scheduled_start_at, group_id, batch_id, auto_renew, auto_renew_duration, auto_renew_failed_at, auto_renew_failed_reason,
	reassign_count, provisioned_at, cancelled_at`

func scanInstance(row pgx.Row) (Instance, error) {
	var i Instance
	var autoDur string
	err := row.Scan(&i.ID, &i.UserID, &i.MachineID, &i.ProductID, &i.ImageID, &i.State, &i.PendingSeconds, &i.ExpiresAt,
		&i.StoppedAt, &i.SuspendedAt, &i.PausedRemaining, &i.Reminded24h, &i.Reminded1h, &i.CreatedAt, &i.PasswordAuth, &i.SSHHostPubkey,
		&i.ScheduledStartAt, &i.GroupID, &i.BatchID, &i.AutoRenew, &autoDur, &i.AutoRenewFailedAt, &i.AutoRenewFailedReason,
		&i.ReassignCount, &i.ProvisionedAt, &i.CancelledAt)
	if db.IsNoRows(err) {
		return Instance{}, apperr.ErrNotFound
	}
	i.AutoRenewDuration = catalog.Duration(autoDur)
	return i, err
}
```

`internal/rental/orders.go` 的 `Order`、`orderCols`、`scanOrder`：

```go
type Order struct {
	ID             string           `json:"id"`
	UserID         int64            `json:"-"`
	Kind           string           `json:"kind"`
	InstanceID     string           `json:"instance_id"`
	ProductID      int64            `json:"product_id"`
	Duration       catalog.Duration `json:"duration"`
	Quantity       int              `json:"quantity"`
	Amount         int64            `json:"amount"`
	Status         string           `json:"status"`
	RefundedAmount int64            `json:"refunded_amount"`
	BatchID        *string          `json:"batch_id"`
	Auto           bool             `json:"auto"`
	CreatedAt      time.Time        `json:"created_at"`
}

const orderCols = `id, user_id, kind, instance_id, product_id, duration, quantity, amount, status, refunded_amount, batch_id, auto, created_at`

func scanOrder(row pgx.Row) (Order, error) {
	var o Order
	err := row.Scan(&o.ID, &o.UserID, &o.Kind, &o.InstanceID, &o.ProductID, &o.Duration, &o.Quantity, &o.Amount, &o.Status, &o.RefundedAmount, &o.BatchID, &o.Auto, &o.CreatedAt)
	if db.IsNoRows(err) {
		return Order{}, apperr.ErrNotFound
	}
	return o, err
}
```

`charge` 本任务不改（Task 5 改签名）；它的 INSERT 没列出新列，默认值生效。

`internal/rental/admin_queries.go`：

```go
var instanceStates = []string{StateReserved, StateCreating, StateRunning, StateStopped, StateWiping, StateReleased, StateFailed, StateCancelled}
```

`internal/rental/refund.go` 的 `remainingSeconds`：

```go
func remainingSeconds(inst Instance, now time.Time) int64 {
	switch inst.State {
	case StateRunning:
		return int64(inst.Remaining(now) / time.Second)
	case StateReserved, StateCreating, StateStopped:
		return inst.PendingSeconds
	}
	return 0
}
```

- [ ] **Step 6: 错误码**

`internal/platform/apperr/apperr.go` 的 `var (...)` 块末尾追加：

```go
	// 计划 6
	ErrBookingTooSoon       = New("booking_too_soon", "开始时间至少在 2 小时之后", 400)
	ErrBookingTooFar        = New("booking_too_far", "最多提前 60 天预订", 400)
	ErrBookingTooLong       = New("booking_too_long", "单次租期最长 90 天", 400)
	ErrBookingNotHour       = New("booking_not_hour", "开始时间需为整点", 400)
	ErrBookingLimit         = New("booking_limit", "未开通的预订最多 16 台", 409)
	ErrCountInvalid         = New("count_invalid", "一次最多租用 8 台", 400)
	ErrGroupNotFound        = New("group_not_found", "机组不存在", 404)
	ErrMachineUnavailable   = New("machine_unavailable", "该机器在所选时段不可用", 409)
	ErrCancelNotAllowed     = New("cancel_not_allowed", "预订已开始开通，不能取消", 409)
	ErrGroupNotEmpty        = New("group_not_empty", "机组内还有机器或未结束的实例", 409)
	ErrDuplicateGroupName   = New("duplicate_group_name", "机组名称已存在", 409)
	ErrDuplicateDisplayName = New("duplicate_display_name", "展示名已被其他机器使用", 409)
```

- [ ] **Step 7: 运行测试确认通过**

Run: `go vet ./... && go test ./internal/rental/ ./internal/api/ ./internal/adminapi/ ./internal/app/ ./internal/platform/db/ 2>&1 | tail -8`
Expected: 全部 `ok`。`instanceView` / `orderView` 等 JSON 视图因 `Order` 多了三个字段，`api` 的既有断言只看固定键，仍通过。

- [ ] **Step 8: 提交**

```bash
git add migrations/00019_booking.sql internal/rental/booking.go internal/rental/service.go internal/rental/orders.go internal/rental/admin_queries.go internal/rental/refund.go internal/rental/booking_test.go internal/rental/helpers_test.go internal/platform/apperr/apperr.go
git commit -m "feat(booking): migration 00019 (groups, batches, reserved/cancelled states), rental fields and constants, error codes

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 2: `tz` 包、`occupiedUntil` 非空、`refreshLease` 封顶、`RefreshCappedLeases`

**Files:**
- Create: `internal/platform/tz/tz.go`、`internal/platform/tz/tz_test.go`
- Modify: `internal/rental/leases.go`（整文件重写）、`internal/rental/sweeper.go`（`RefreshCappedLeases`；`Sweep` 顺序在 Task 15 统一改）
- Test: `internal/rental/leases_test.go`（新建）、`internal/rental/helpers_test.go`（追加 `reserveRaw`）

**Interfaces:**
- Consumes: Task 1 的 `Instance.Booked()`、`StateReserved / StateCancelled`、`Tail`、`PrepLead`。
- Produces:
  - `tz.CST *time.Location`（UTC+8，固定）；`tz.FloorHour(t) time.Time`（按 CST 取整到小时，返回 UTC）；`tz.IsHour(t) bool`；`tz.DayStart(t) time.Time`（CST 当天 00:00，返回 UTC）；`tz.Format(t) string`（`2006-01-02 15:04` 北京时间）。
  - `rental.leaseRow{ID, MachineID int64; Lower, Upper time.Time; Status string}`；`lockLease(ctx, tx, instanceID) (leaseRow, error)`（锁定实例当前 reserved / active 的 lease，没有 → `apperr.ErrNotFound`）；`insertLease(ctx, q, machineID, instanceID, lower, upper, status, now) (int64, error)`（原始错误，调用方用 `db.IsExclusionViolation` 判断）；`nextReservedLower(ctx, q, machineID, exclude string, after time.Time) (cap time.Time, ok bool, err error)`；`occupiedUntil(inst, now)` 任何分支 ≥ `now + 1s`；`refreshLease` 封顶且对 `Booked() && (reserved | creating)` 不动；`(*Service).RefreshCappedLeases(ctx) (int, error)`。
  - `openLease` 保留（`AdminMigrate` 与 Task 4 之前的 `Place` 仍用它）。

- [ ] **Step 1: 写失败的测试**

`internal/platform/tz/tz_test.go`：

```go
package tz_test

import (
	"testing"
	"time"

	"spark-cloud/internal/platform/tz"
)

func TestHourHelpers(t *testing.T) {
	// 2026-03-01 23:30 北京时间 = 15:30 UTC
	x := time.Date(2026, 3, 1, 15, 30, 0, 0, time.UTC)
	if tz.IsHour(x) {
		t.Fatal("15:30 is not an hour boundary")
	}
	if got := tz.FloorHour(x); !got.Equal(time.Date(2026, 3, 1, 15, 0, 0, 0, time.UTC)) || got.Location() != time.UTC {
		t.Fatalf("FloorHour = %v", got)
	}
	if !tz.IsHour(tz.FloorHour(x)) {
		t.Fatal("floor must be an hour boundary")
	}
	// 北京时间 3 月 2 日 00:00 = 3 月 1 日 16:00 UTC
	if got := tz.DayStart(x); !got.Equal(time.Date(2026, 3, 1, 16, 0, 0, 0, time.UTC)) {
		t.Fatalf("DayStart = %v", got)
	}
	if got := tz.Format(x); got != "2026-03-01 23:30" {
		t.Fatalf("Format = %q", got)
	}
}
```

`internal/rental/helpers_test.go` 追加：

```go
// reserveRaw 直接写库造一个预订（Task 5 之前没有 PlaceBatch，之后也用于绕过参数校验）：
// reserved 实例 + reserved lease，period 按规格 §4.1 = [start − PrepLead, start + secs + Tail)。
// 第三参数是秒（int64），调用方写 86400 / 3600 这类字面量；不要用 time.Duration，无类型常量会被当成纳秒。
func (e *env) reserveRaw(machineID int64, start time.Time, secs int64) string {
	e.t.Helper()
	d := time.Duration(secs) * time.Second
	id := randx.ID("ins-", 8)
	now := e.clock.Now()
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO instances (id, user_id, machine_id, product_id, image_id, state, pending_seconds,
			ssh_password_enc, scheduled_start_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, 'reserved', $6, $7, $8, $9, $9)`,
		id, e.userID, machineID, e.productID, e.imageID, int64(d/time.Second), []byte("x"), start, now); err != nil {
		e.t.Fatalf("reserveRaw instance: %v", err)
	}
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO machine_leases (machine_id, instance_id, period, status, created_at, updated_at)
		VALUES ($1, $2, tstzrange($3, $4), 'reserved', $5, $5)`, machineID, id, start.Add(-rental.PrepLead), start.Add(d+rental.Tail), now); err != nil {
		e.t.Fatalf("reserveRaw lease: %v", err)
	}
	return id
}
```

（`helpers_test.go` 的 import 需要加 `"spark-cloud/internal/platform/randx"`。）

`internal/rental/leases_test.go`：

```go
package rental_test

import (
	"testing"
	"time"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/rental"
)

func TestFailedInstanceLeaseIsNeverEmpty(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(false) // 开通失败：lease 在同一时刻关闭，上界必须 ≥ 下界 + 1 秒
	lower, upper, status := e.lease(inst.ID)
	if status != "ended" || !upper.Equal(lower.Add(time.Second)) {
		t.Fatalf("lease = [%v, %v) %s", lower, upper, status)
	}
}

func TestRefreshLeaseCapsAtNextReservation(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true) // running，expires = T0 + 24h，lease = [T0, T0 + 31h)
	// 同机下一条预订：T0 + 40h 开始 → lease 下界 T0 + 39h
	resv := e.reserveRaw(e.machines[0], dbtest.T0.Add(40*time.Hour), 86400)

	// 失联 5 小时后暂停：剩余 19h 冻结；desired = now + 19h + 7h = T0 + 31h（未碰到封顶）
	e.clock.Advance(5 * time.Hour)
	if _, err := e.svc.SuspendMachines(e.ctx, e.machines); err != nil {
		t.Fatal(err)
	}
	if _, upper, _ := e.lease(inst.ID); !upper.Equal(dbtest.T0.Add(31 * time.Hour)) {
		t.Fatalf("upper after suspend = %v", upper)
	}
	// 再过 10 小时（仍失联）：desired = T0 + 41h，被封顶到 T0 + 39h
	e.clock.Advance(10 * time.Hour)
	n, err := e.svc.RefreshCappedLeases(e.ctx)
	mustCount(t, n, err, 1, "refresh capped")
	if _, upper, _ := e.lease(inst.ID); !upper.Equal(dbtest.T0.Add(39 * time.Hour)) {
		t.Fatalf("capped upper = %v, want %v", upper, dbtest.T0.Add(39*time.Hour))
	}
	// 预订取消后封顶消失
	if _, err := e.pool.Exec(e.ctx, `UPDATE machine_leases SET status = 'cancelled' WHERE instance_id = $1`, resv); err != nil {
		t.Fatal(err)
	}
	n, err = e.svc.RefreshCappedLeases(e.ctx)
	mustCount(t, n, err, 1, "refresh after cancel")
	if _, upper, _ := e.lease(inst.ID); !upper.Equal(dbtest.T0.Add(41 * time.Hour)) {
		t.Fatalf("uncapped upper = %v", upper)
	}
	// 预订实例自己的 lease 不被 refreshLease 改写
	if lower, upper, status := e.lease(resv); status != "cancelled" || !lower.Equal(dbtest.T0.Add(39*time.Hour)) || !upper.Equal(dbtest.T0.Add(40*time.Hour+24*time.Hour+rental.Tail)) {
		t.Fatalf("reserved lease touched: [%v, %v) %s", lower, upper, status)
	}
}

func TestRefreshCappedLeasesAdvancesWiping(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	if err := e.svc.Release(e.ctx, e.userID, inst.ID); err != nil {
		t.Fatal(err)
	}
	if _, upper, _ := e.lease(inst.ID); !upper.Equal(e.clock.Now().Add(rental.WipeBuffer)) {
		t.Fatalf("wiping upper = %v", upper)
	}
	e.clock.Advance(2 * time.Hour) // Wipe 一直没回报：上界必须跟着时间走
	n, err := e.svc.RefreshCappedLeases(e.ctx)
	mustCount(t, n, err, 1, "refresh wiping")
	if _, upper, _ := e.lease(inst.ID); !upper.Equal(e.clock.Now().Add(rental.WipeBuffer)) {
		t.Fatalf("advanced upper = %v", upper)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/platform/tz/ ./internal/rental/ -run 'TestHourHelpers|TestFailedInstanceLeaseIsNeverEmpty|TestRefreshLeaseCaps|TestRefreshCappedLeasesAdvancesWiping' 2>&1 | head -20`
Expected: `tz` 包不存在；`rental` 编译错误（`RefreshCappedLeases`、`reserveRaw` 未定义）。

- [ ] **Step 3: `tz` 包**

`internal/platform/tz/tz.go`：

```go
// Package tz 固定使用北京时间（Asia/Shanghai，UTC+8，无夏令时）解释整点与自然日。
package tz

import "time"

// CST 北京时间。
var CST = time.FixedZone("CST", 8*3600)

// FloorHour 按北京时间取整到小时，结果转回 UTC。
func FloorHour(t time.Time) time.Time {
	l := t.In(CST)
	return time.Date(l.Year(), l.Month(), l.Day(), l.Hour(), 0, 0, 0, CST).UTC()
}

// IsHour 是否整点（分、秒、纳秒都为 0）。
func IsHour(t time.Time) bool {
	l := t.In(CST)
	return l.Minute() == 0 && l.Second() == 0 && l.Nanosecond() == 0
}

// DayStart 北京时间当天 00:00，结果转回 UTC。
func DayStart(t time.Time) time.Time {
	l := t.In(CST)
	return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, CST).UTC()
}

// Format 北京时间 2006-01-02 15:04。
func Format(t time.Time) string { return t.In(CST).Format("2006-01-02 15:04") }
```

- [ ] **Step 4: 重写 `leases.go`**

`internal/rental/leases.go` 整文件：

```go
package rental

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
)

// WipeBuffer 是预留给洗机的时长，计入机器占用时段。
const WipeBuffer = time.Hour

// leaseRow 是实例当前的占用时段。
type leaseRow struct {
	ID        int64
	MachineID int64
	Lower     time.Time
	Upper     time.Time
	Status    string
}

// lockLease 锁定并返回实例当前（reserved / active）的 lease；调用方须已锁实例。没有 → ErrNotFound。
func lockLease(ctx context.Context, tx pgx.Tx, instanceID string) (leaseRow, error) {
	var l leaseRow
	err := tx.QueryRow(ctx, `SELECT id, machine_id, lower(period), upper(period), status FROM machine_leases
		WHERE instance_id = $1 AND status IN ('reserved', 'active') ORDER BY id DESC LIMIT 1 FOR UPDATE`, instanceID).
		Scan(&l.ID, &l.MachineID, &l.Lower, &l.Upper, &l.Status)
	if db.IsNoRows(err) {
		return leaseRow{}, apperr.ErrNotFound
	}
	return l, err
}

// insertLease 插入一条时段，返回原始错误（排他冲突由调用方用 db.IsExclusionViolation 判断）。
func insertLease(ctx context.Context, q db.Querier, machineID int64, instanceID string, lower, upper time.Time, status string, now time.Time) (int64, error) {
	var id int64
	err := q.QueryRow(ctx, `INSERT INTO machine_leases (machine_id, instance_id, period, status, created_at, updated_at)
		VALUES ($1, $2, tstzrange($3, $4), $5, $6, $6) RETURNING id`, machineID, instanceID, lower, upper, status, now).Scan(&id)
	return id, err
}

// nextReservedLower 返回机器上其它实例的 reserved 时段中、下界不早于 after 的最小下界（封顶值 cap）；没有则 ok=false。
func nextReservedLower(ctx context.Context, q db.Querier, machineID int64, exclude string, after time.Time) (time.Time, bool, error) {
	var cap *time.Time
	if err := q.QueryRow(ctx, `SELECT min(lower(period)) FROM machine_leases
		WHERE machine_id = $1 AND status = 'reserved' AND instance_id <> $2 AND lower(period) >= $3`, machineID, exclude, after).Scan(&cap); err != nil {
		return time.Time{}, false, err
	}
	if cap == nil {
		return time.Time{}, false, nil
	}
	return *cap, true, nil
}

// occupiedUntil 估算实例对机器的占用结束时间（machine_leases 的上界），任何分支都至少 now + 1 秒（时段不能为空）。
func occupiedUntil(inst Instance, now time.Time) time.Time {
	pending := time.Duration(inst.PendingSeconds) * time.Second
	var end time.Time
	switch inst.State {
	case StateCreating:
		end = now.Add(CreateTimeout + pending + Tail)
	case StateRunning:
		if inst.Suspended() && inst.PausedRemaining != nil {
			end = now.Add(time.Duration(*inst.PausedRemaining)*time.Second + Tail)
		} else if inst.ExpiresAt != nil {
			end = inst.ExpiresAt.Add(Tail)
		}
	case StateStopped:
		if pending > 0 {
			end = now.Add(CreateTimeout + pending + Tail)
		} else if inst.StoppedAt != nil {
			end = inst.StoppedAt.Add(Tail)
		}
	case StateWiping:
		end = now.Add(WipeBuffer)
	}
	if !end.After(now) {
		end = now.Add(time.Second)
	}
	return end
}

// openLease 为新实例写入一条 active 占用时段；与已有时段重叠视为无可用机器。
func openLease(ctx context.Context, tx pgx.Tx, inst Instance, now time.Time) error {
	_, err := insertLease(ctx, tx, inst.MachineID, inst.ID, now, occupiedUntil(inst, now), "active", now)
	if db.IsExclusionViolation(err) {
		return apperr.ErrNoCapacity
	}
	return err
}

// refreshLease 在实例状态变化后同步占用时段：结束的实例关闭时段，其余按 occupiedUntil 更新上界，
// 但上界封顶于同机下一条 reserved 时段的下界（规格 §4.3），所以永远不会撞排他约束。
// 预订实例在 reserved 与首次开通的 creating 期间时段固定（下单时定死），不改写。
func (s *Service) refreshLease(ctx context.Context, tx pgx.Tx, id string, now time.Time) error {
	inst, err := lockInstance(ctx, tx, id)
	if err != nil {
		return err
	}
	if inst.Booked() && (inst.State == StateReserved || inst.State == StateCreating) {
		return nil
	}
	desired, status := occupiedUntil(inst, now), "active"
	if inst.State == StateReleased || inst.State == StateFailed || inst.State == StateCancelled {
		desired, status = now, "ended"
	}
	// cap 按 lease 自己的 machine_id 算（不用 inst.MachineID）：改派过渡期 lease 与实例的机器可能短暂不一致
	_, err = tx.Exec(ctx, `WITH own AS (
			SELECT id, machine_id, lower(period) AS lo FROM machine_leases
			WHERE instance_id = $1 AND status = 'active' ORDER BY id DESC LIMIT 1 FOR UPDATE
		), cap AS (
			SELECT min(lower(l.period)) AS c FROM machine_leases l, own
			WHERE l.machine_id = own.machine_id AND l.status = 'reserved' AND l.instance_id <> $1 AND lower(l.period) >= own.lo
		)
		UPDATE machine_leases m
		SET period = tstzrange(own.lo, GREATEST(own.lo + interval '1 second', LEAST($2::timestamptz, COALESCE((SELECT c FROM cap), $2::timestamptz)))),
		    status = $3, updated_at = $4
		FROM own WHERE m.id = own.id`, id, desired, status, now)
	return err
}
```

- [ ] **Step 5: `RefreshCappedLeases`**

`internal/rental/sweeper.go` 在 `ExpireDue` 之前加：

```go
// RefreshCappedLeases 让停机、洗机中与失联暂停实例的占用上界随时间推进（并封顶于下一条预订）：
// 这些实例的 occupiedUntil 以 now 为基准，不刷新的话上界会「过期」，后来的预订会把机器当空闲挑走。
func (s *Service) RefreshCappedLeases(ctx context.Context) (int, error) {
	now := s.d.Clock.Now()
	return s.sweepEach(ctx, `(state IN ('stopped', 'wiping') OR (state = 'running' AND suspended_at IS NOT NULL)) AND created_at <= $1`, now, "updated_at",
		func(tx pgx.Tx, inst Instance) error { return s.refreshLease(ctx, tx, inst.ID, now) })
}
```

- [ ] **Step 6: 运行测试确认通过**

Run: `go vet ./... && go test ./internal/platform/tz/ ./internal/rental/ 2>&1 | tail -6`
Expected: 全部 `ok`（现有 `TestPlaceAllocatesChargesAndEnqueuesCreate` 的 lease 上界断言 `CreateTimeout + 24h + RetainAfterStop + WipeBuffer` 不变）。

- [ ] **Step 7: 提交**

```bash
git add internal/platform/tz internal/rental/leases.go internal/rental/sweeper.go internal/rental/leases_test.go internal/rental/helpers_test.go
git commit -m "feat(booking): tz helpers, non-empty leases, refreshLease capped at next reservation, RefreshCappedLeases sweep

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 3: 机组：`fleet` 机组 CRUD / 分配 / 展示名 / `MarkAllocated`，后台机组 API 与权限矩阵

**Files:**
- Create: `internal/fleet/groups.go`、`internal/fleet/groups_test.go`、`internal/rental/groups.go`、`internal/adminapi/groups.go`、`internal/adminapi/groups_test.go`
- Modify: `internal/fleet/machines.go`（`Machine` 新字段、`machineCols`、`scanMachine`、`Label`、`MarkAllocated`）、`internal/adminapi/server.go`（`s.groupRoutes(g)`）、`internal/adminapi/permissions_test.go`（`expectedRoutes`）

**Interfaces:**
- Consumes: Task 1 的表与错误码。
- Produces:
  - `fleet.Machine` 新字段 `GroupID *int64 \`json:"group_id"\``、`DisplayName string \`json:"display_name"\``；`(Machine) Label() string`（`display_name` 非空返回它，否则 `机器 #<id>`）。
  - `fleet.Group{ID, Name, Region, Description, Sort, Visible, CreatedAt, UpdatedAt}`（json 小写蛇形）；`fleet.GroupInput{Name, Region, Description string; Sort int; Visible bool}`；`fleet.GroupRow{Group; MachineCount int \`json:"machine_count"\`; IdleNow int \`json:"idle_now"\`}`。
  - `(*fleet.Service).ListGroups(ctx, visibleOnly bool) ([]GroupRow, error)`、`GetGroup(ctx, q, id) (Group, error)`、`CreateGroup(ctx, actor, in) (Group, error)`、`UpdateGroup(ctx, actor, id, in) (Group, error)`、`DeleteGroupIn(ctx, q, id) error`（组内有机器 → `ErrGroupNotEmpty`；不存在 → `ErrGroupNotFound`；不写审计）、`AssignMachines(ctx, actor, groupID, ids []int64) error`、`UnassignMachines(ctx, actor, groupID, ids []int64) error`、`fleet.MachineUpdate{SetGroup bool; GroupID *int64; DisplayName *string}`、`UpdateMachine(ctx, actor, id, in MachineUpdate) (Machine, error)`、`MarkAllocated(ctx, q, id) (Machine, error)`（`idle AND agent_online` 才改为 `allocated`，否则 `ErrNoCapacity`）。
  - `(*rental.Service).ReservedCountsByGroup(ctx) (map[int64]int, error)`（`state='reserved'` 按 `group_id` 计数，NULL 组不计）、`AdminDeleteGroup(ctx, actor, id) error`（有未终态实例引用 → `ErrGroupNotEmpty`；再 `Fleet.DeleteGroupIn`；把终态实例的 `group_id` 置 NULL；审计 `group.delete`）。
  - 后台路由：`GET /groups`（all）、`POST /groups`、`PUT /groups/{id}`、`DELETE /groups/{id}`、`POST /groups/{id}/machines`、`DELETE /groups/{id}/machines`、`PUT /machines/{id}`（都 ops）。`GET /groups` 每项为 `GroupRow` 加 `reserved_upcoming`。
  - 审计：`group.create / group.update / group.assign / group.unassign`（fleet 写）、`group.delete`（rental 写）、`machine.update`（fleet 写，diff 含 before / after）。

- [ ] **Step 1: 写失败的测试**

`internal/fleet/groups_test.go`：

```go
package fleet_test

import (
	"context"
	"errors"
	"testing"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/dbtest"
)

var opsActor = audit.Actor{AdminID: 1, IP: "127.0.0.1"}

func TestGroupCRUDAndAssign(t *testing.T) {
	svc, pool, _ := newEnv(t)
	ctx := context.Background()
	g, err := svc.CreateGroup(ctx, opsActor, fleet.GroupInput{Name: " A 区 ", Region: "一楼", Visible: true})
	if err != nil || g.Name != "A 区" || g.Region != "一楼" || !g.Visible {
		t.Fatalf("create = %+v, %v", g, err)
	}
	if _, err := svc.CreateGroup(ctx, opsActor, fleet.GroupInput{Name: "A 区"}); !errors.Is(err, apperr.ErrDuplicateGroupName) {
		t.Fatalf("duplicate name = %v", err)
	}
	if _, err := svc.CreateGroup(ctx, opsActor, fleet.GroupInput{Name: ""}); apperr.As(err) == nil || apperr.As(err).Code != "bad_request" {
		t.Fatalf("empty name = %v", err)
	}
	m1 := dbtest.CreateMachine(t, pool, "SN1")
	m2 := dbtest.CreateMachine(t, pool, "SN2")
	if err := svc.AssignMachines(ctx, opsActor, g.ID, []int64{m1, m2}); err != nil {
		t.Fatal(err)
	}
	if err := svc.AssignMachines(ctx, opsActor, g.ID, []int64{m1, 999999}); apperr.As(err) == nil {
		t.Fatalf("unknown machine = %v", err)
	}
	rows, err := svc.ListGroups(ctx, false)
	if err != nil || len(rows) != 1 || rows[0].MachineCount != 2 || rows[0].IdleNow != 2 {
		t.Fatalf("list = %+v, %v", rows, err)
	}
	if err := svc.DeleteGroupIn(ctx, pool, g.ID); !errors.Is(err, apperr.ErrGroupNotEmpty) {
		t.Fatalf("delete non-empty = %v", err)
	}
	hidden, _ := svc.CreateGroup(ctx, opsActor, fleet.GroupInput{Name: "留池", Visible: false})
	if vis, _ := svc.ListGroups(ctx, true); len(vis) != 1 || vis[0].ID != g.ID {
		t.Fatalf("visible only = %+v", vis)
	}
	// 展示名：唯一、≤ 32 字、三态改组
	name := "A-01"
	m, err := svc.UpdateMachine(ctx, opsActor, m1, fleet.MachineUpdate{DisplayName: &name})
	if err != nil || m.DisplayName != "A-01" || m.Label() != "A-01" || m.GroupID == nil || *m.GroupID != g.ID {
		t.Fatalf("update display = %+v, %v", m, err)
	}
	if _, err := svc.UpdateMachine(ctx, opsActor, m2, fleet.MachineUpdate{DisplayName: &name}); !errors.Is(err, apperr.ErrDuplicateDisplayName) {
		t.Fatalf("dup display = %v", err)
	}
	m, err = svc.UpdateMachine(ctx, opsActor, m2, fleet.MachineUpdate{SetGroup: true, GroupID: &hidden.ID})
	if err != nil || m.GroupID == nil || *m.GroupID != hidden.ID || m.Label() != "机器 #"+itoa(m2) {
		t.Fatalf("move group = %+v, %v", m, err)
	}
	m, err = svc.UpdateMachine(ctx, opsActor, m2, fleet.MachineUpdate{SetGroup: true})
	if err != nil || m.GroupID != nil {
		t.Fatalf("ungroup = %+v, %v", m, err)
	}
	bogus := int64(424242)
	if _, err := svc.UpdateMachine(ctx, opsActor, m2, fleet.MachineUpdate{SetGroup: true, GroupID: &bogus}); !errors.Is(err, apperr.ErrGroupNotFound) {
		t.Fatalf("unknown group = %v", err)
	}
	if err := svc.UnassignMachines(ctx, opsActor, g.ID, []int64{m1}); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteGroupIn(ctx, pool, g.ID); err != nil {
		t.Fatalf("delete empty = %v", err)
	}
	if _, err := svc.GetGroup(ctx, pool, g.ID); !errors.Is(err, apperr.ErrGroupNotFound) {
		t.Fatalf("after delete = %v", err)
	}
	var audits int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE action LIKE 'group.%' OR action = 'machine.update'`).Scan(&audits)
	if audits < 7 { // create×2 + assign + update(display)+update(move)+update(ungroup) + unassign
		t.Fatalf("audits = %d", audits)
	}
}

func TestMarkAllocatedRequiresIdleAndOnline(t *testing.T) {
	svc, pool, _ := newEnv(t)
	ctx := context.Background()
	pid := dbtest.ProductID(t, pool)
	m, _ := svc.Register(ctx, fleet.NewMachine{Serial: "SN001", LanIP: "10.0.1.3", ProductID: pid})
	_ = svc.SetMaintenance(ctx, m.ID, false)
	if _, err := svc.MarkAllocated(ctx, pool, m.ID); !errors.Is(err, apperr.ErrNoCapacity) {
		t.Fatalf("offline machine must not be allocated, got %v", err)
	}
	if _, err := svc.Heartbeat(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	got, err := svc.MarkAllocated(ctx, pool, m.ID)
	if err != nil || got.State != fleet.StateAllocated {
		t.Fatalf("allocate = %+v, %v", got, err)
	}
	if _, err := svc.MarkAllocated(ctx, pool, m.ID); !errors.Is(err, apperr.ErrNoCapacity) {
		t.Fatalf("second allocate want no capacity, got %v", err)
	}
	if err := svc.Release(ctx, pool, m.ID); err != nil {
		t.Fatal(err)
	}
	if n, _ := svc.IdleCount(ctx, pid); n != 1 {
		t.Fatalf("after release IdleCount = %d, want 1", n)
	}
}

func itoa(n int64) string { return fmt.Sprintf("%d", n) }
```

（文件 import 加 `"fmt"`。`newEnv` 已在 `machines_test.go` 里定义，返回 `(svc *fleet.Service, pool *pgxpool.Pool, clk *clock.Fake)`。）

`internal/adminapi/groups_test.go`：

```go
package adminapi_test

import (
	"testing"

	"spark-cloud/internal/admin"
)

func TestGroupAPI(t *testing.T) {
	e := newEnv(t)
	ops := e.loginAs("ops", admin.RoleOps)
	fin := e.loginAs("fin", admin.RoleFinance)
	m1 := e.machine("SN1")
	m2 := e.machine("SN2")

	code, out := ops.do("POST", "/groups", map[string]any{"name": "A 区", "region": "一楼", "visible": true})
	if code != 201 || out["name"] != "A 区" {
		t.Fatalf("create = %d %v", code, out)
	}
	gid := int64(out["id"].(float64))
	if code, out := ops.do("POST", "/groups", map[string]any{"name": "A 区"}); code != 409 || codeOf(out) != "duplicate_group_name" {
		t.Fatalf("dup = %d %v", code, out)
	}
	if code, out := fin.do("POST", "/groups", map[string]any{"name": "B"}); code != 403 || codeOf(out) != "forbidden" {
		t.Fatalf("finance create = %d %v", code, out)
	}
	if code, _ := ops.do("POST", "/groups/"+itoa(gid)+"/machines", map[string]any{"machine_ids": []int64{m1, m2}}); code != 200 {
		t.Fatalf("assign = %d", code)
	}
	code, out = fin.do("GET", "/groups", nil)
	items := out["items"].([]any)
	if code != 200 || len(items) != 1 {
		t.Fatalf("list = %d %v", code, out)
	}
	row := items[0].(map[string]any)
	if row["machine_count"].(float64) != 2 || row["idle_now"].(float64) != 2 || row["reserved_upcoming"].(float64) != 0 {
		t.Fatalf("row = %v", row)
	}
	if code, out := ops.do("PUT", "/machines/"+itoa(m1), map[string]any{"display_name": "A-01"}); code != 200 || out["display_name"] != "A-01" {
		t.Fatalf("update machine = %d %v", code, out)
	}
	if code, out := ops.do("PUT", "/machines/"+itoa(m2), map[string]any{"display_name": "A-01"}); code != 409 || codeOf(out) != "duplicate_display_name" {
		t.Fatalf("dup display = %d %v", code, out)
	}
	if code, out := ops.do("PUT", "/groups/"+itoa(gid), map[string]any{"name": "A 区", "region": "二楼", "visible": false, "sort": 5}); code != 200 || out["region"] != "二楼" || out["visible"] != false {
		t.Fatalf("update group = %d %v", code, out)
	}
	if code, out := ops.do("DELETE", "/groups/"+itoa(gid), map[string]any{}); code != 409 || codeOf(out) != "group_not_empty" {
		t.Fatalf("delete non-empty = %d %v", code, out)
	}
	if code, _ := ops.do("DELETE", "/groups/"+itoa(gid)+"/machines", map[string]any{"machine_ids": []int64{m1, m2}}); code != 200 {
		t.Fatalf("unassign = %d", code)
	}
	if code, _ := ops.do("DELETE", "/groups/"+itoa(gid), map[string]any{}); code != 200 {
		t.Fatalf("delete = %d", code)
	}
	if code, out := ops.do("GET", "/groups", nil); code != 200 || len(out["items"].([]any)) != 0 {
		t.Fatalf("after delete = %d %v", code, out)
	}
	if code, out := ops.do("PUT", "/machines/"+itoa(m1), map[string]any{"group_id": gid}); code != 404 || codeOf(out) != "group_not_found" {
		t.Fatalf("assign to deleted = %d %v", code, out)
	}
}
```

`internal/adminapi/permissions_test.go` 的 `expectedRoutes` 在 `{"GET", "/alerts", all}, {"POST", "/alerts/{id}/resolve", all},` 之后加：

```go
	{"GET", "/groups", all}, {"POST", "/groups", ops}, {"PUT", "/groups/{id}", ops}, {"DELETE", "/groups/{id}", ops},
	{"POST", "/groups/{id}/machines", ops}, {"DELETE", "/groups/{id}/machines", ops}, {"PUT", "/machines/{id}", ops},
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/fleet/ ./internal/adminapi/ -run 'TestGroup|TestMarkAllocated|TestRouteTable' 2>&1 | head -20`
Expected: 编译错误（`CreateGroup` 等未定义）。

- [ ] **Step 3: `fleet.Machine` 新字段与 `MarkAllocated`**

`internal/fleet/machines.go`：`Machine` 结构体在 `CreatedAt` 之后加：

```go
	// 计划 6：机组与对用户展示名
	GroupID     *int64 `json:"group_id"`
	DisplayName string `json:"display_name"`
```

`machineCols` 改为：

```go
const machineCols = `id, serial, rack, slot, lan_ip, product_id, state, agent_online, last_seen_at, pdu_id, pdu_outlet, created_at,
	cert_fingerprint, agent_conn_id, agent_version, agent_connected_at, state_reported_at, last_metrics, group_id, display_name`
```

`scanMachine` 的 `Scan` 末尾追加 `&m.GroupID, &m.DisplayName`。`Machine` 增加方法，`Allocate` 之后加 `MarkAllocated`：

```go
// Label 对用户展示的机器名：后台设置的展示名，缺省「机器 #<id>」。序列号与内网 IP 不对用户展示。
func (m Machine) Label() string {
	if m.DisplayName != "" {
		return m.DisplayName
	}
	return fmt.Sprintf("机器 #%d", m.ID)
}

// MarkAllocated 把一台由 rental.pickMachines 选中并锁定的机器从空闲置为已分配；不再空闲或离线 → ErrNoCapacity。
func (s *Service) MarkAllocated(ctx context.Context, q db.Querier, id int64) (Machine, error) {
	m, err := scanMachine(q.QueryRow(ctx, `UPDATE machines SET state = 'allocated', updated_at = $2
		WHERE id = $1 AND state = 'idle' AND agent_online RETURNING `+machineCols, id, s.clock.Now()))
	if errors.Is(err, apperr.ErrNotFound) {
		return Machine{}, apperr.ErrNoCapacity
	}
	return m, err
}
```

（文件 import 加 `"fmt"`。`Allocate` 在 Task 4 删除。）

- [ ] **Step 4: `fleet/groups.go`**

```go
package fleet

import (
	"context"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
)

// Group 机组：机器的可选标签（规格 §3）。
type Group struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Region      string    `json:"region"`
	Description string    `json:"description"`
	Sort        int       `json:"sort"`
	Visible     bool      `json:"visible"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type GroupInput struct {
	Name        string
	Region      string
	Description string
	Sort        int
	Visible     bool
}

// GroupRow 列表行：机组加机器数与现空闲台数。
type GroupRow struct {
	Group
	MachineCount int `json:"machine_count"`
	IdleNow      int `json:"idle_now"`
}

const groupCols = `id, name, region, description, sort, visible, created_at, updated_at`

func scanGroup(row pgx.Row) (Group, error) {
	var g Group
	err := row.Scan(&g.ID, &g.Name, &g.Region, &g.Description, &g.Sort, &g.Visible, &g.CreatedAt, &g.UpdatedAt)
	if db.IsNoRows(err) {
		return Group{}, apperr.ErrGroupNotFound
	}
	return g, err
}

func (in GroupInput) normalize() (GroupInput, error) {
	in.Name = strings.TrimSpace(in.Name)
	in.Region = strings.TrimSpace(in.Region)
	in.Description = strings.TrimSpace(in.Description)
	if n := utf8.RuneCountInString(in.Name); n < 1 || n > 32 {
		return in, apperr.BadRequest("机组名称需为 1～32 个字符")
	}
	if utf8.RuneCountInString(in.Region) > 32 {
		return in, apperr.BadRequest("位置标签最多 32 个字符")
	}
	if utf8.RuneCountInString(in.Description) > 200 {
		return in, apperr.BadRequest("描述最多 200 个字符")
	}
	return in, nil
}

// ListGroups 按 sort、id 返回机组（visibleOnly 只返回控制台可见的），附机器数与现空闲台数。
func (s *Service) ListGroups(ctx context.Context, visibleOnly bool) ([]GroupRow, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+groupCols+`,
			(SELECT count(*) FROM machines m WHERE m.group_id = g.id),
			(SELECT count(*) FROM machines m WHERE m.group_id = g.id AND m.state = 'idle' AND m.agent_online)
		FROM machine_groups g WHERE NOT $1 OR visible ORDER BY sort, id`, visibleOnly)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (GroupRow, error) {
		var g GroupRow
		err := r.Scan(&g.ID, &g.Name, &g.Region, &g.Description, &g.Sort, &g.Visible, &g.CreatedAt, &g.UpdatedAt, &g.MachineCount, &g.IdleNow)
		return g, err
	})
}

func (s *Service) GetGroup(ctx context.Context, q db.Querier, id int64) (Group, error) {
	return scanGroup(q.QueryRow(ctx, `SELECT `+groupCols+` FROM machine_groups WHERE id = $1`, id))
}

func (s *Service) CreateGroup(ctx context.Context, actor audit.Actor, in GroupInput) (Group, error) {
	in, err := in.normalize()
	if err != nil {
		return Group{}, err
	}
	now := s.clock.Now()
	var g Group
	err = db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		g, err = scanGroup(tx.QueryRow(ctx, `INSERT INTO machine_groups (name, region, description, sort, visible, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $6) RETURNING `+groupCols, in.Name, in.Region, in.Description, in.Sort, in.Visible, now))
		if db.IsUniqueViolation(err) {
			return apperr.ErrDuplicateGroupName
		}
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("group.create", "group", strconv.FormatInt(g.ID, 10),
			map[string]any{"name": g.Name, "region": g.Region, "visible": g.Visible}))
	})
	return g, err
}

func (s *Service) UpdateGroup(ctx context.Context, actor audit.Actor, id int64, in GroupInput) (Group, error) {
	in, err := in.normalize()
	if err != nil {
		return Group{}, err
	}
	now := s.clock.Now()
	var g Group
	err = db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		before, err := scanGroup(tx.QueryRow(ctx, `SELECT `+groupCols+` FROM machine_groups WHERE id = $1 FOR UPDATE`, id))
		if err != nil {
			return err
		}
		g, err = scanGroup(tx.QueryRow(ctx, `UPDATE machine_groups SET name = $2, region = $3, description = $4, sort = $5, visible = $6, updated_at = $7
			WHERE id = $1 RETURNING `+groupCols, id, in.Name, in.Region, in.Description, in.Sort, in.Visible, now))
		if db.IsUniqueViolation(err) {
			return apperr.ErrDuplicateGroupName
		}
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("group.update", "group", strconv.FormatInt(id, 10),
			map[string]any{"before": before, "after": g}))
	})
	return g, err
}

// DeleteGroupIn 删除没有机器的机组（实例引用由 rental.AdminDeleteGroup 在同一事务里先检查）。
func (s *Service) DeleteGroupIn(ctx context.Context, q db.Querier, id int64) error {
	var machines int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM machines WHERE group_id = $1`, id).Scan(&machines); err != nil {
		return err
	}
	if machines > 0 {
		return apperr.ErrGroupNotEmpty
	}
	tag, err := q.Exec(ctx, `DELETE FROM machine_groups WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return apperr.ErrGroupNotFound
	}
	return nil
}

// AssignMachines 把机器加入机组（已在其它机组的直接改组）；任一机器不存在整批失败。
func (s *Service) AssignMachines(ctx context.Context, actor audit.Actor, groupID int64, ids []int64) error {
	if len(ids) == 0 {
		return apperr.BadRequest("请选择机器")
	}
	now := s.clock.Now()
	return db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := s.GetGroup(ctx, tx, groupID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE machines SET group_id = $1, updated_at = $3 WHERE id = ANY($2)`, groupID, ids, now)
		if err != nil {
			return err
		}
		if int(tag.RowsAffected()) != len(ids) {
			return apperr.BadRequest("有机器不存在")
		}
		return audit.Record(ctx, tx, now, actor.Entry("group.assign", "group", strconv.FormatInt(groupID, 10), map[string]any{"machine_ids": ids}))
	})
}

// UnassignMachines 把机器移出机组（只影响当前确实在该组的机器）。
func (s *Service) UnassignMachines(ctx context.Context, actor audit.Actor, groupID int64, ids []int64) error {
	if len(ids) == 0 {
		return apperr.BadRequest("请选择机器")
	}
	now := s.clock.Now()
	return db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE machines SET group_id = NULL, updated_at = $3 WHERE id = ANY($2) AND group_id = $1`, groupID, ids, now); err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("group.unassign", "group", strconv.FormatInt(groupID, 10), map[string]any{"machine_ids": ids}))
	})
}

// MachineUpdate 后台可改的机器字段：SetGroup=true 时按 GroupID 改组（nil = 移出机组）；DisplayName 非 nil 时改展示名。
type MachineUpdate struct {
	SetGroup    bool
	GroupID     *int64
	DisplayName *string
}

// UpdateMachine 修改机组 / 展示名（计划 4 遗留的「修改已登记机器」只开放这两项）。
func (s *Service) UpdateMachine(ctx context.Context, actor audit.Actor, id int64, in MachineUpdate) (Machine, error) {
	var name string
	if in.DisplayName != nil {
		name = strings.TrimSpace(*in.DisplayName)
		if utf8.RuneCountInString(name) > 32 {
			return Machine{}, apperr.BadRequest("展示名最多 32 个字符")
		}
	}
	now := s.clock.Now()
	var out Machine
	err := db.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		before, err := scanMachine(tx.QueryRow(ctx, `SELECT `+machineCols+` FROM machines WHERE id = $1 FOR NO KEY UPDATE`, id))
		if err != nil {
			return err
		}
		groupID, display := before.GroupID, before.DisplayName
		if in.SetGroup {
			groupID = in.GroupID
			if groupID != nil {
				if _, err := s.GetGroup(ctx, tx, *groupID); err != nil {
					return err
				}
			}
		}
		if in.DisplayName != nil {
			display = name
		}
		out, err = scanMachine(tx.QueryRow(ctx, `UPDATE machines SET group_id = $2, display_name = $3, updated_at = $4 WHERE id = $1 RETURNING `+machineCols,
			id, groupID, display, now))
		if db.IsUniqueViolation(err) && db.ConstraintName(err) == "machines_display_name_uniq" {
			return apperr.ErrDuplicateDisplayName
		}
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("machine.update", "machine", strconv.FormatInt(id, 10), map[string]any{
			"before": map[string]any{"group_id": before.GroupID, "display_name": before.DisplayName},
			"after":  map[string]any{"group_id": out.GroupID, "display_name": out.DisplayName},
		}))
	})
	return out, err
}
```

- [ ] **Step 5: `rental/groups.go`**

```go
package rental

import (
	"context"
	"strconv"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
)

// ReservedCountsByGroup 返回每个机组未开通预订（reserved）的实例数；下单时未选机组的不计。
func (s *Service) ReservedCountsByGroup(ctx context.Context) (map[int64]int, error) {
	rows, err := s.d.Pool.Query(ctx, `SELECT group_id, count(*) FROM instances WHERE state = 'reserved' AND group_id IS NOT NULL GROUP BY group_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int{}
	for rows.Next() {
		var gid int64
		var n int
		if err := rows.Scan(&gid, &n); err != nil {
			return nil, err
		}
		out[gid] = n
	}
	return out, rows.Err()
}

// AdminDeleteGroup 删除机组：组内不能有机器、不能有未终态实例引用（下单时选的机组）；终态实例的引用置 NULL。
func (s *Service) AdminDeleteGroup(ctx context.Context, actor audit.Actor, id int64) error {
	now := s.d.Clock.Now()
	return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		var busy bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM instances WHERE group_id = $1
			AND state IN ('reserved', 'creating', 'running', 'stopped', 'wiping'))`, id).Scan(&busy); err != nil {
			return err
		}
		if busy {
			return apperr.ErrGroupNotEmpty
		}
		if _, err := tx.Exec(ctx, `UPDATE instances SET group_id = NULL, updated_at = $2 WHERE group_id = $1`, id, now); err != nil {
			return err
		}
		if err := s.d.Fleet.DeleteGroupIn(ctx, tx, id); err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("group.delete", "group", strconv.FormatInt(id, 10), nil))
	})
}
```

- [ ] **Step 6: 后台 API**

`internal/adminapi/groups.go`：

```go
package adminapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/httpx"
)

func (s *Server) groupRoutes(g chi.Router) {
	s.handle(g, http.MethodGet, "/groups", allRoles, s.handleListGroups)
	s.handle(g, http.MethodPost, "/groups", opsRoles, s.handleCreateGroup)
	s.handle(g, http.MethodPut, "/groups/{id}", opsRoles, s.handleUpdateGroup)
	s.handle(g, http.MethodDelete, "/groups/{id}", opsRoles, s.handleDeleteGroup)
	s.handle(g, http.MethodPost, "/groups/{id}/machines", opsRoles, s.handleAssignMachines)
	s.handle(g, http.MethodDelete, "/groups/{id}/machines", opsRoles, s.handleUnassignMachines)
	s.handle(g, http.MethodPut, "/machines/{id}", opsRoles, s.handleUpdateMachine)
}

type groupRequest struct {
	Name        string `json:"name"`
	Region      string `json:"region"`
	Description string `json:"description"`
	Sort        int    `json:"sort"`
	Visible     *bool  `json:"visible"`
}

func (q groupRequest) input() fleet.GroupInput {
	visible := true
	if q.Visible != nil {
		visible = *q.Visible
	}
	return fleet.GroupInput{Name: q.Name, Region: q.Region, Description: q.Description, Sort: q.Sort, Visible: visible}
}

// groupView 列表行：fleet.GroupRow 加未开通预订数（rental 统计）。
type groupView struct {
	fleet.GroupRow
	ReservedUpcoming int `json:"reserved_upcoming"`
}

func (s *Server) handleListGroups(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Fleet.ListGroups(r.Context(), false)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	counts, err := s.Rental.ReservedCountsByGroup(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out := make([]groupView, 0, len(rows))
	for _, g := range rows {
		out = append(out, groupView{GroupRow: g, ReservedUpcoming: counts[g.ID]})
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.List(out))
}

func (s *Server) handleCreateGroup(w http.ResponseWriter, r *http.Request) {
	var req groupRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	g, err := s.Fleet.CreateGroup(r.Context(), s.actor(r), req.input())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, g)
}

func (s *Server) handleUpdateGroup(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var req groupRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	g, err := s.Fleet.UpdateGroup(r.Context(), s.actor(r), id, req.input())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, g)
}

func (s *Server) handleDeleteGroup(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.Rental.AdminDeleteGroup(r.Context(), s.actor(r), id); err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.OK(w)
}

type machineIDsRequest struct {
	MachineIDs []int64 `json:"machine_ids"`
}

func (s *Server) handleAssignMachines(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var req machineIDsRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.Fleet.AssignMachines(r.Context(), s.actor(r), id, req.MachineIDs); err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.OK(w)
}

func (s *Server) handleUnassignMachines(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var req machineIDsRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.Fleet.UnassignMachines(r.Context(), s.actor(r), id, req.MachineIDs); err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.OK(w)
}

// handleUpdateMachine 只认 group_id（键存在即改组，null = 移出）与 display_name 两个字段。
func (s *Server) handleUpdateMachine(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var raw map[string]any
	if err := httpx.DecodeJSON(r, &raw); err != nil {
		s.writeError(w, r, err)
		return
	}
	var in fleet.MachineUpdate
	if v, ok := raw["group_id"]; ok {
		in.SetGroup = true
		if v != nil {
			f, isNum := v.(float64)
			if !isNum || f <= 0 || f != float64(int64(f)) {
				s.writeError(w, r, apperr.BadRequest("group_id 必须是正整数或 null"))
				return
			}
			gid := int64(f)
			in.GroupID = &gid
		}
	}
	if v, ok := raw["display_name"]; ok {
		str, isStr := v.(string)
		if !isStr {
			s.writeError(w, r, apperr.BadRequest("display_name 必须是字符串"))
			return
		}
		in.DisplayName = &str
	}
	m, err := s.Fleet.UpdateMachine(r.Context(), s.actor(r), id, in)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, m)
}
```

`internal/adminapi/server.go` 的 `Router()` 在 `s.machineRoutes(g)` 之后加 `s.groupRoutes(g)`。

- [ ] **Step 7: 运行测试确认通过**

Run: `go vet ./... && go test ./internal/fleet/ ./internal/rental/ ./internal/adminapi/ 2>&1 | tail -6`
Expected: 全部 `ok`；`TestRouteTableMatchesPermissionMatrix` 通过（7 条新路由都已登记）。

- [ ] **Step 8: 提交**

```bash
git add internal/fleet/groups.go internal/fleet/groups_test.go internal/fleet/machines.go internal/rental/groups.go internal/adminapi/groups.go internal/adminapi/groups_test.go internal/adminapi/server.go internal/adminapi/permissions_test.go
git commit -m "feat(groups): machine groups CRUD/assign, machine display name, MarkAllocated, admin group API

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 4: `rental.pickMachines`（lease-aware 候选、best-fit、savepoint 换机器）、删除 `fleet.Allocate`、`AdminMigrate` 适配、`AvailableNow`

**Files:**
- Create: `internal/rental/alloc.go`、`internal/rental/alloc_test.go`
- Modify: `internal/rental/place.go`（改用 `placeOnMachines`，仍是单台 / 无批次，Task 5 再重构）、`internal/rental/migrate.go`、`internal/rental/export_test.go`（导出内部函数供测试）、`internal/fleet/machines.go`（删除 `Allocate`）、`internal/fleet/machines_test.go`（删除 `allocate` helper 与两个 Allocate 测试）、`internal/rental/migrate_test.go`（追加复制字段的用例）

**Interfaces:**
- Consumes: Task 2 的 `insertLease`、`Tail`、`PrepLead`；Task 3 的 `Fleet.MarkAllocated`、`Machine.GroupID`。
- Produces:
  - `rental.pickSpec{ProductID int64; GroupID, MachineID *int64; Lower, Upper time.Time; Immediate bool; Exclude []int64}`。
  - `pickMachines(ctx, q, spec, limit int, lock bool) ([]int64, error)`（规格 §4.2 的 SQL；`lock` 为 true 时 `FOR NO KEY UPDATE OF m SKIP LOCKED`——不能用 `FOR UPDATE`，否则测试里另一连接插 `machine_leases` / `instances`（外键 `KEY SHARE`）会与它互相等待而挂死）；`countAvailable(ctx, q, spec) (int, error)`。
  - `placeOnMachines(ctx, tx, spec, want int, fn func(sp pgx.Tx, machineID int64) error) ([]int64, error)`：每台在 savepoint 内执行 `fn`，排他冲突换下一台，最多补取 3 轮；不足 → 即时分支返回 `apperr.ErrNoCapacity`（消息不变，现有测试与前端引导依赖它），预订分支返回 `no_capacity` + 带台数的消息。
  - `immediatePeriod(now, secs int64) (lower, upper)` = `[now, now + CreateTimeout + secs + Tail)`；`bookingPeriod(start, secs) (lower, upper)` = `[start − PrepLead, start + secs + Tail)`。
  - `(*Service).AvailableNow(ctx, productID) (int, error)`：此刻可即时租 1 天的台数（市场页 `idle_count` 的新语义，Task 13 接入）。
  - `AdminMigrate` 新实例复制 `auto_renew`、`auto_renew_duration`、`group_id`；period 按 `immediatePeriod(now, remaining)`；预订实例（`Booked() && state != running`）不可换机（本来就只允许 running）。
  - `fleet.Allocate` **删除**（唯一调用者是 rental）。

- [ ] **Step 1: 写失败的测试**

`internal/rental/export_test.go` 追加（保留现有内容）：

```go
// 计划 6：导出内部分配函数供 rental_test 包直接测试。
type PickSpec = pickSpec

var (
	PickMachines    = pickMachines
	PlaceOnMachines = placeOnMachines
	ImmediatePeriod = immediatePeriod
	BookingPeriod   = bookingPeriod
	CountAvailable  = countAvailable
)
```

`internal/rental/alloc_test.go`：

```go
package rental_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/platform/randx"
	"spark-cloud/internal/rental"
)

// group 直接写库建一个机组并返回 id。
func (e *env) group(name string, visible bool) int64 {
	e.t.Helper()
	var id int64
	if err := e.pool.QueryRow(e.ctx, `INSERT INTO machine_groups (name, visible, created_at, updated_at) VALUES ($1, $2, $3, $3) RETURNING id`,
		name, visible, e.clock.Now()).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *env) setGroup(machineID, groupID int64) {
	e.t.Helper()
	if _, err := e.pool.Exec(e.ctx, `UPDATE machines SET group_id = $2 WHERE id = $1`, machineID, groupID); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) setMachine(machineID int64, state string, online bool) {
	e.t.Helper()
	if _, err := e.pool.Exec(e.ctx, `UPDATE machines SET state = $2, agent_online = $3 WHERE id = $1`, machineID, state, online); err != nil {
		e.t.Fatal(err)
	}
}

func pick(e *env, spec rental.PickSpec) []int64 {
	e.t.Helper()
	ids, err := rental.PickMachines(e.ctx, e.pool, spec, 10, false)
	if err != nil {
		e.t.Fatal(err)
	}
	return ids
}

func TestPickMachinesCandidateSet(t *testing.T) {
	e := newEnv(t, 6)
	ungrouped, visibleM, hiddenM, maint, busy, offline := e.machines[0], e.machines[1], e.machines[2], e.machines[3], e.machines[4], e.machines[5]
	vis := e.group("A", true)
	hid := e.group("留池", false)
	e.setGroup(visibleM, vis)
	e.setGroup(hiddenM, hid)
	e.setMachine(maint, "maintenance", true)
	e.setMachine(offline, "idle", false)
	// busy：一台运行中的实例（lease 到 T0 + 31h）
	e.setMachine(busy, "allocated", true)
	instID := randx.ID("ins-", 8)
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO instances (id, user_id, machine_id, product_id, image_id, state, ssh_password_enc, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, 'running', $6, $7, $7)`, instID, e.userID, busy, e.productID, e.imageID, []byte("x"), dbtest.T0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO machine_leases (machine_id, instance_id, period, status, created_at, updated_at)
		VALUES ($1, $2, tstzrange($3, $4), 'active', $3, $3)`, busy, instID, dbtest.T0, dbtest.T0.Add(31*time.Hour)); err != nil {
		t.Fatal(err)
	}

	far := rental.PickSpec{ProductID: e.productID}
	far.Lower, far.Upper = rental.BookingPeriod(dbtest.T0.Add(48*time.Hour), 86400)
	got := pick(e, far)
	slices.Sort(got)
	if want := []int64{ungrouped, visibleM, busy, offline}; !slices.Equal(got, sortedCopy(want)) {
		t.Fatalf("booking, no group = %v, want %v (busy's lease ends before the window; offline allowed; hidden/maintenance excluded)", got, want)
	}
	near := far
	near.Lower, near.Upper = rental.BookingPeriod(dbtest.T0.Add(10*time.Hour), 86400)
	if got := pick(e, near); slices.Contains(got, busy) {
		t.Fatalf("overlapping window must exclude busy: %v", got)
	}
	imm := rental.PickSpec{ProductID: e.productID, Immediate: true}
	imm.Lower, imm.Upper = rental.ImmediatePeriod(dbtest.T0, 86400)
	got = pick(e, imm)
	slices.Sort(got)
	if want := []int64{ungrouped, visibleM}; !slices.Equal(got, sortedCopy(want)) {
		t.Fatalf("immediate = %v, want %v", got, want)
	}
	g := far
	g.GroupID = &vis
	if got := pick(e, g); !slices.Equal(got, []int64{visibleM}) {
		t.Fatalf("group vis = %v", got)
	}
	g.GroupID = &hid
	if got := pick(e, g); !slices.Equal(got, []int64{hiddenM}) {
		t.Fatalf("group hidden (explicit) = %v", got)
	}
	m := far
	m.MachineID = &hiddenM
	if got := pick(e, m); len(got) != 0 {
		t.Fatalf("hidden machine without its group must be excluded: %v", got)
	}
	m.GroupID = &hid
	if got := pick(e, m); !slices.Equal(got, []int64{hiddenM}) {
		t.Fatalf("hidden machine with its group = %v", got)
	}
	ex := far
	ex.Exclude = []int64{ungrouped, visibleM, busy}
	if got := pick(e, ex); !slices.Equal(got, []int64{offline}) {
		t.Fatalf("exclude = %v", got)
	}
	if n, err := rental.CountAvailable(e.ctx, e.pool, far); err != nil || n != 4 {
		t.Fatalf("count = %d, %v", n, err)
	}
}

func sortedCopy(xs []int64) []int64 {
	out := append([]int64(nil), xs...)
	slices.Sort(out)
	return out
}

func TestPickMachinesBestFit(t *testing.T) {
	e := newEnv(t, 2)
	// m0 上有一段到 T0 + 10h 结束的历史时段，m1 没有：从 T0 + 12h 起的窗口应优先 m0（最紧贴）
	instID := randx.ID("ins-", 8)
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO instances (id, user_id, machine_id, product_id, image_id, state, ssh_password_enc, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, 'released', $6, $7, $7)`, instID, e.userID, e.machines[0], e.productID, e.imageID, []byte("x"), dbtest.T0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO machine_leases (machine_id, instance_id, period, status, created_at, updated_at)
		VALUES ($1, $2, tstzrange($3, $4), 'ended', $3, $3)`, e.machines[0], instID, dbtest.T0, dbtest.T0.Add(10*time.Hour)); err != nil {
		t.Fatal(err)
	}
	spec := rental.PickSpec{ProductID: e.productID}
	spec.Lower, spec.Upper = rental.BookingPeriod(dbtest.T0.Add(12*time.Hour), 86400)
	if got := pick(e, spec); !slices.Equal(got, []int64{e.machines[0], e.machines[1]}) {
		t.Fatalf("best fit order = %v", got)
	}
	// m1 上加一段到 T0 + 11h 的时段：m1 更紧贴，排到前面
	inst2 := randx.ID("ins-", 8)
	_, _ = e.pool.Exec(e.ctx, `INSERT INTO instances (id, user_id, machine_id, product_id, image_id, state, ssh_password_enc, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, 'released', $6, $7, $7)`, inst2, e.userID, e.machines[1], e.productID, e.imageID, []byte("x"), dbtest.T0)
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO machine_leases (machine_id, instance_id, period, status, created_at, updated_at)
		VALUES ($1, $2, tstzrange($3, $4), 'ended', $3, $3)`, e.machines[1], inst2, dbtest.T0, dbtest.T0.Add(11*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := pick(e, spec); !slices.Equal(got, []int64{e.machines[1], e.machines[0]}) {
		t.Fatalf("best fit order after = %v", got)
	}
}

// insertRawInstance 在给定 Querier 上插一个 reserved 实例行（供 PlaceOnMachines 的回调使用）。
func insertRawInstance(ctx context.Context, q db.Querier, e *env, machineID int64) (string, error) {
	id := randx.ID("ins-", 8)
	_, err := q.Exec(ctx, `INSERT INTO instances (id, user_id, machine_id, product_id, image_id, state, pending_seconds, ssh_password_enc, scheduled_start_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, 'reserved', 86400, $6, $7, $8, $8)`, id, e.userID, machineID, e.productID, e.imageID, []byte("x"), dbtest.T0.Add(12*time.Hour), e.clock.Now())
	return id, err
}

func TestPlaceOnMachinesRetriesOnExclusionViolation(t *testing.T) {
	e := newEnv(t, 2)
	start := dbtest.T0.Add(12 * time.Hour)
	spec := rental.PickSpec{ProductID: e.productID}
	spec.Lower, spec.Upper = rental.BookingPeriod(start, 86400)
	first := true
	var placed []string
	err := db.WithTx(e.ctx, e.pool, func(tx pgx.Tx) error {
		ids, err := rental.PlaceOnMachines(e.ctx, tx, spec, 1, func(sp pgx.Tx, mid int64) error {
			id, err := insertRawInstance(e.ctx, sp, e, mid)
			if err != nil {
				return err
			}
			if first { // 另一个连接在我们预筛之后、插 lease 之前提交了重叠的预订（READ COMMITTED 快照看不到）
				first = false
				e.reserveRaw(mid, start.Add(2*time.Hour), 3600)
			}
			if _, err := rental.InsertLease(e.ctx, sp, mid, id, spec.Lower, spec.Upper, "reserved", e.clock.Now()); err != nil {
				return err
			}
			placed = append(placed, id)
			return nil
		})
		if err != nil {
			return err
		}
		if len(ids) != 1 {
			return errors.New("want exactly one machine")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(placed) != 1 {
		t.Fatalf("placed = %v", placed)
	}
	var n int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM instances WHERE state = 'reserved'`).Scan(&n)
	if n != 2 { // 1 个来自 reserveRaw、1 个来自成功的那台；撞约束那台的实例行随 savepoint 回滚
		t.Fatalf("reserved instances = %d, want 2", n)
	}

	// 换一个窗口，两台都在插 lease 前被别人占掉（每轮补取都撞）：整单 no_capacity，事务回滚后什么都不留
	before := n
	start2 := dbtest.T0.Add(5 * 24 * time.Hour)
	spec2 := rental.PickSpec{ProductID: e.productID}
	spec2.Lower, spec2.Upper = rental.BookingPeriod(start2, 86400)
	err = db.WithTx(e.ctx, e.pool, func(tx pgx.Tx) error {
		_, err := rental.PlaceOnMachines(e.ctx, tx, spec2, 2, func(sp pgx.Tx, mid int64) error {
			id, err := insertRawInstance(e.ctx, sp, e, mid)
			if err != nil {
				return err
			}
			e.reserveRaw(mid, start2.Add(3*time.Hour), 3600)
			_, err = rental.InsertLease(e.ctx, sp, mid, id, spec2.Lower, spec2.Upper, "reserved", e.clock.Now())
			return err
		})
		return err
	})
	if ae := apperr.As(err); ae == nil || ae.Code != "no_capacity" {
		t.Fatalf("want no_capacity, got %v", err)
	}
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM instances WHERE state = 'reserved'`).Scan(&n)
	if n != before+2 { // 只多了两条 reserveRaw 造的冲突预订；本事务的实例行全部回滚
		t.Fatalf("reserved instances = %d, want %d", n, before+2)
	}
}

func TestImmediateNoCapacityKeepsLegacyError(t *testing.T) {
	e := newEnv(t, 0)
	_, _, err := e.svc.Place(e.ctx, rental.PlaceRequest{UserID: e.userID, ProductID: e.productID, ImageID: e.imageID, Duration: catalog.D1d})
	if !errors.Is(err, apperr.ErrNoCapacity) {
		t.Fatalf("want ErrNoCapacity, got %v", err)
	}
}

func TestAvailableNowIsLeaseAware(t *testing.T) {
	e := newEnv(t, 2)
	if n, _ := e.svc.AvailableNow(e.ctx, e.productID); n != 2 {
		t.Fatalf("available = %d", n)
	}
	// 机器 0 上 10 小时后有预订：即时租 1 天（含 20 分钟开通 + 7 小时尾段）放不下
	e.reserveRaw(e.machines[0], dbtest.T0.Add(10*time.Hour), 86400)
	if n, _ := e.svc.AvailableNow(e.ctx, e.productID); n != 1 {
		t.Fatalf("available with near reservation = %d, want 1", n)
	}
}
```

`internal/rental/export_test.go` 再加一行导出 `InsertLease = insertLease`（放进上面的 `var (...)` 块）。

`internal/rental/migrate_test.go` 追加：

```go
func TestAdminMigrateCopiesAutoRenewAndGroup(t *testing.T) {
	e := newEnv(t, 2)
	gid := e.group("A", true)
	_, old := e.place(catalog.D1d)
	e.ack(true)
	if _, err := e.pool.Exec(e.ctx, `UPDATE instances SET auto_renew = true, auto_renew_duration = '1w', group_id = $2 WHERE id = $1`, old.ID, gid); err != nil {
		t.Fatal(err)
	}
	moved, err := e.svc.AdminMigrate(e.ctx, opsActor, old.ID, "GPU 报错")
	if err != nil {
		t.Fatal(err)
	}
	got := e.get(moved.ID)
	if !got.AutoRenew || got.AutoRenewDuration != catalog.D1w || got.GroupID == nil || *got.GroupID != gid || got.Booked() || got.ProvisionedAt != nil || got.BatchID != nil {
		t.Fatalf("moved = %+v", got)
	}
	lower, upper, status := e.lease(moved.ID)
	if status != "active" || !lower.Equal(e.clock.Now()) || !upper.Equal(e.clock.Now().Add(rental.CreateTimeout+24*time.Hour+rental.Tail)) {
		t.Fatalf("lease = [%v, %v) %s", lower, upper, status)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/rental/ -run 'TestPickMachines|TestPlaceOnMachines|TestImmediateNoCapacity|TestAvailableNow|TestAdminMigrateCopies' 2>&1 | head -20`
Expected: 编译错误（`PickSpec` 等未定义）。

- [ ] **Step 3: `alloc.go`**

```go
package rental

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
)

// pickSpec 描述一次分配的候选集（规格 §4.2）。
type pickSpec struct {
	ProductID int64
	GroupID   *int64 // nil = 不限机组：未分组机器 + 可见机组
	MachineID *int64 // 指定机器（仍受机组条件约束）
	Lower     time.Time
	Upper     time.Time
	Immediate bool // 即时分支：要求 idle AND agent_online；预订分支允许 allocated 与离线
	Exclude   []int64
}

// pickSQL：候选 = 商品匹配 ∩ 机组条件 ∩ 状态允许 ∩ 与 reserved / active 时段无重叠；
// 按「最紧贴前一档期的机器优先」排序（减少碎片），再按 id。
const pickSQL = `SELECT m.id FROM machines m
	LEFT JOIN machine_groups g ON g.id = m.group_id
	WHERE m.product_id = $1
	  AND (($2::bigint IS NULL AND (m.group_id IS NULL OR g.visible)) OR m.group_id = $2::bigint)
	  AND ($3::bigint IS NULL OR m.id = $3::bigint)
	  AND NOT (m.id = ANY($6::bigint[]))
	  AND (CASE WHEN $7::boolean THEN m.state = 'idle' AND m.agent_online ELSE m.state IN ('idle', 'allocated') END)
	  AND NOT EXISTS (SELECT 1 FROM machine_leases l
	                  WHERE l.machine_id = m.id AND l.status IN ('reserved', 'active') AND l.period && tstzrange($4::timestamptz, $5::timestamptz))
	ORDER BY (SELECT max(upper(l.period)) FROM machine_leases l
	          WHERE l.machine_id = m.id AND l.status IN ('reserved', 'active', 'ended') AND upper(l.period) <= $4::timestamptz) DESC NULLS LAST,
	         m.id`

func pickArgs(spec pickSpec) []any {
	exclude := spec.Exclude
	if exclude == nil {
		exclude = []int64{}
	}
	return []any{spec.ProductID, spec.GroupID, spec.MachineID, spec.Lower, spec.Upper, exclude, spec.Immediate}
}

// pickMachines 返回最多 limit 台候选机器；lock 为 true 时锁行（SKIP LOCKED：并发分配互不等待）。
func pickMachines(ctx context.Context, q db.Querier, spec pickSpec, limit int, lock bool) ([]int64, error) {
	sql := pickSQL + ` LIMIT $8`
	if lock {
		sql += ` FOR NO KEY UPDATE OF m SKIP LOCKED` // 不用 FOR UPDATE：与外键插入的 KEY SHARE 互斥
	}
	rows, err := q.Query(ctx, sql, append(pickArgs(spec), limit)...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

// countAvailable 返回候选台数（只读，供可用性查询与错误消息）。
func countAvailable(ctx context.Context, q db.Querier, spec pickSpec) (int, error) {
	var n int
	err := q.QueryRow(ctx, `SELECT count(*) FROM (`+pickSQL+`) x`, pickArgs(spec)...).Scan(&n)
	return n, err
}

// noCapacity：即时分支保持 ErrNoCapacity（消息与错误值不变，前端引导与现有测试依赖它）；预订分支带台数。
func noCapacity(spec pickSpec, got int) error {
	if spec.Immediate {
		return apperr.ErrNoCapacity
	}
	if got == 0 {
		return apperr.New("no_capacity", "该时段暂无可用机器", 409)
	}
	return apperr.New("no_capacity", fmt.Sprintf("该时段只有 %d 台可用，请减少数量或更换时段", got), 409)
}

// placeOnMachines 挑出 want 台机器，对每台在 savepoint 内执行 fn（插实例 + lease [+ 标记 allocated]）：
// 排他约束冲突（并发事务刚提交了重叠时段，预筛的快照没看到）→ 回滚 savepoint、剔除这台、补取；最多补取 3 轮。
// 机器行锁（NO KEY UPDATE）在外层事务里取（SKIP LOCKED），savepoint 回滚不释放它，并发批次不会互相等待。
// 不足 want 台 → 返回 no_capacity，调用方回滚整个事务（没有部分成功）。
func placeOnMachines(ctx context.Context, tx pgx.Tx, spec pickSpec, want int, fn func(sp pgx.Tx, machineID int64) error) ([]int64, error) {
	var done []int64
	tried := append([]int64(nil), spec.Exclude...)
	for round := 0; round <= 3 && len(done) < want; round++ {
		s := spec
		s.Exclude = tried
		ids, err := pickMachines(ctx, tx, s, want-len(done), true)
		if err != nil {
			return nil, err
		}
		if len(ids) == 0 {
			break
		}
		for _, mid := range ids {
			tried = append(tried, mid)
			sp, err := tx.Begin(ctx)
			if err != nil {
				return nil, err
			}
			if err := fn(sp, mid); err != nil {
				_ = sp.Rollback(ctx)
				if db.IsExclusionViolation(err) {
					continue
				}
				return nil, err
			}
			if err := sp.Commit(ctx); err != nil {
				return nil, err
			}
			done = append(done, mid)
		}
	}
	if len(done) < want {
		return nil, noCapacity(spec, len(done))
	}
	return done, nil
}

// immediatePeriod 即时订单的占用时段：开通估算 + 租期 + 尾段（规格 §4.1）。
func immediatePeriod(now time.Time, secs int64) (time.Time, time.Time) {
	return now, now.Add(CreateTimeout + time.Duration(secs)*time.Second + Tail)
}

// bookingPeriod 预订的占用时段：开始前 1 小时开通，到期后 7 小时尾段。
func bookingPeriod(start time.Time, secs int64) (time.Time, time.Time) {
	return start.Add(-PrepLead), start.Add(time.Duration(secs)*time.Second + Tail)
}

// AvailableNow 此刻可以立即租用 1 天的机器数（市场页展示；按最短租期探测，和下单时的校验一致）。
func (s *Service) AvailableNow(ctx context.Context, productID int64) (int, error) {
	spec := pickSpec{ProductID: productID, Immediate: true}
	spec.Lower, spec.Upper = immediatePeriod(s.d.Clock.Now(), 86400)
	return countAvailable(ctx, s.d.Pool, spec)
}
```

- [ ] **Step 4: `Place` 改用 `placeOnMachines`（单台，无批次；Task 5 再重构）**

`internal/rental/place.go` 的事务体从 `img, err := s.d.Catalog.GetImage(...)` 起改为：

```go
		img, err := s.d.Catalog.GetImage(ctx, tx, req.ImageID)
		if err != nil {
			return err
		}
		spec := pickSpec{ProductID: req.ProductID, Immediate: true}
		spec.Lower, spec.Upper = immediatePeriod(now, seconds(req.Duration))
		var machineID int64
		if _, err := placeOnMachines(ctx, tx, spec, 1, func(sp pgx.Tx, mid int64) error {
			m, err := s.d.Fleet.MarkAllocated(ctx, sp, mid)
			if err != nil {
				return err
			}
			hostPub, hostPEM, err := s.d.HostKeyGen()
			if err != nil {
				return err
			}
			id := randx.ID("ins-", 8)
			inst, err = scanInstance(sp.QueryRow(ctx, `INSERT INTO instances
				(id, user_id, machine_id, product_id, image_id, state, pending_seconds, ssh_password_enc, ssh_host_pubkey, ssh_host_key_enc, auto_renew_duration, created_at, updated_at)
				VALUES ($1, $2, $3, $4, $5, 'creating', $6, $7, $8, $9, $10, $11, $11) RETURNING `+instanceCols,
				id, req.UserID, m.ID, req.ProductID, img.ID, seconds(req.Duration), s.d.Box.Seal([]byte(randx.Password(16))), hostPub, s.d.Box.Seal(hostPEM), string(req.Duration), now))
			if err != nil {
				return err
			}
			if _, err := insertLease(ctx, sp, m.ID, inst.ID, spec.Lower, spec.Upper, "active", now); err != nil {
				return err
			}
			machineID = m.ID
			return nil
		}); err != nil {
			return err
		}
		if ord, err = s.charge(ctx, tx, req.UserID, "new", inst.ID, req.ProductID, req.Duration, req.IdempotencyKey, now); err != nil {
			return err
		}
		if err := recordEvent(ctx, tx, inst.ID, "", StateCreating, "order "+ord.ID, now); err != nil {
			return err
		}
		_, err = s.d.Fleet.Enqueue(ctx, tx, machineID, inst.ID, fleet.CmdCreateInstance, fleet.CreateInstancePayload{ImageRef: img.Ref})
		return err
```

（`openLease` 不再被 `Place` 使用；`AdminMigrate` 下一步也改掉后，`openLease` 整个删除。）

- [ ] **Step 5: `AdminMigrate` 适配**

`internal/rental/migrate.go` 从 `m, err := s.d.Fleet.Allocate(...)` 到 `openLease` 这一段改为：

```go
		spec := pickSpec{ProductID: old.ProductID, Immediate: true}
		spec.Lower, spec.Upper = immediatePeriod(now, remaining)
		hostPub, hostPEM, err := s.d.HostKeyGen()
		if err != nil {
			return err
		}
		id := randx.ID("ins-", 8)
		var newMachine int64
		if _, err := placeOnMachines(ctx, tx, spec, 1, func(sp pgx.Tx, mid int64) error {
			m, err := s.d.Fleet.MarkAllocated(ctx, sp, mid)
			if err != nil {
				return err
			}
			moved, err = scanInstance(sp.QueryRow(ctx, `INSERT INTO instances
				(id, user_id, machine_id, product_id, image_id, state, pending_seconds, ssh_password_enc, password_auth, ssh_host_pubkey, ssh_host_key_enc,
				 auto_renew, auto_renew_duration, group_id, created_at, updated_at)
				VALUES ($1, $2, $3, $4, $5, 'creating', $6, $7, $8, $9, $10, $11, $12, $13, $14, $14) RETURNING `+instanceCols,
				id, old.UserID, m.ID, old.ProductID, old.ImageID, remaining, s.d.Box.Seal([]byte(randx.Password(16))), old.PasswordAuth, hostPub, s.d.Box.Seal(hostPEM),
				old.AutoRenew, string(old.AutoRenewDuration), old.GroupID, now))
			if err != nil {
				return err
			}
			if _, err := insertLease(ctx, sp, m.ID, id, spec.Lower, spec.Upper, "active", now); err != nil {
				return err
			}
			newMachine = m.ID
			return nil
		}); err != nil {
			return err
		}
```

后面的 `Enqueue` 用 `newMachine` 代替 `m.ID`；审计 diff 的 `"to_machine": newMachine`。`openLease` 从 `leases.go` 删除。

- [ ] **Step 6: 删除 `fleet.Allocate`**

`internal/fleet/machines.go` 删除 `Allocate` 方法；`internal/fleet/machines_test.go` 删除 `allocate` helper、`TestAllocateRequiresIdleAndOnline`、`TestConcurrentAllocationNeverDoubleAssigns`（并发不重复分配由 Task 16 `TestConcurrentBatchesConsistent` 与现有 `rental` 的 `TestConcurrentPlaceForLastMachines` 覆盖）。`go vet` 不得报未使用的 import。

- [ ] **Step 7: 运行测试确认通过**

Run: `go vet ./... && go test ./internal/fleet/ ./internal/rental/ ./internal/api/ ./internal/adminapi/ ./internal/app/ 2>&1 | tail -8`
Expected: 全部 `ok`。`TestPlaceAllocatesChargesAndEnqueuesCreate` 与 `TestAdminMigrateMovesRemainingTime` 的现有断言（机器 allocated、lease 上界）不变。

- [ ] **Step 8: 提交**

```bash
git add internal/rental/alloc.go internal/rental/alloc_test.go internal/rental/place.go internal/rental/migrate.go internal/rental/migrate_test.go internal/rental/leases.go internal/rental/export_test.go internal/fleet/machines.go internal/fleet/machines_test.go
git commit -m "feat(booking): lease-aware pickMachines with best-fit and savepoint retry; drop fleet.Allocate; migrate copies auto-renew/group

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 5: `PlaceBatch`（一次多台、预订分支、批次幂等）、`Place` 包装、`quantity`、`RenewQuantity`、重试

**Files:**
- Create: `internal/rental/batch.go`、`internal/rental/batch_test.go`
- Modify: `internal/rental/place.go`（`Place` 变为包装）、`internal/rental/orders.go`（`charge` 签名）、`internal/rental/idempotency.go`、`internal/rental/renew.go`（`RenewQuantity`）、`internal/rental/export_test.go`

**Interfaces:**
- Consumes: Task 4 的 `pickSpec` / `placeOnMachines` / `immediatePeriod` / `bookingPeriod` / `noCapacity`；Task 2 的 `insertLease`、`tz`。
- Produces:
  - `rental.BatchRequest{UserID, ProductID, ImageID int64; GroupID, MachineID *int64; Count int; Duration catalog.Duration; Quantity int; StartAt *time.Time; AutoRenew bool; AutoRenewDuration catalog.Duration; IdempotencyKey string}`（`Quantity` 0 视为 1；`AutoRenewDuration` 空视为 `Duration`；`StartAt` nil = 立即）。
  - `rental.Batch{ID string \`json:"id"\`; UserID int64 \`json:"-"\`; Count int \`json:"count"\`; CreatedAt time.Time \`json:"created_at"\`}`；`rental.BatchResult{Batch *Batch; Orders []Order; Instances []Instance}`（`Orders[i]` 对应 `Instances[i]`，两者都按实例 ID 升序，首单与回放顺序一致）。
  - `(*Service).PlaceBatch(ctx, req BatchRequest) (BatchResult, error)`；`(*Service).GetBatch(ctx, userID int64, id string) (BatchResult, error)`；`(*Service).Place` 不变（内部 = 不建批次的单台即时）。
  - `charge(ctx, tx, userID int64, kind, instanceID string, productID int64, d catalog.Duration, quantity int, auto bool, batchID *string, idemKey string, now) (Order, error)`，`amount = Quote × quantity`。
  - `(*Service).RenewQuantity(ctx, userID int64, instanceID string, d catalog.Duration, quantity int, idempotencyKey string) (Order, error)`；`Renew` = `RenewQuantity(…, 1, …)`。
  - `checkIdempotencyKey` 拒绝含 `#`；`lockIdempotencyKey` 在命中批次键时返回 `errIdempotencyConflict`；`isBatchIdempotencyViolation(err)`；`withRetry(fn func() error) error`（`40P01` / `40001` 重试一次）。
  - 错误：`booking_*`、`count_invalid`、`booking_limit`、`machine_unavailable`（指定机器且放不下时替代 `no_capacity`）。

- [ ] **Step 1: 写失败的测试**

`internal/rental/batch_test.go`：

```go
package rental_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/rental"
)

func (e *env) batchReq(count int, start *time.Time) rental.BatchRequest {
	return rental.BatchRequest{UserID: e.userID, ProductID: e.productID, ImageID: e.imageID, Count: count, Duration: catalog.D1d, Quantity: 1, StartAt: start}
}

func ptr[T any](v T) *T { return &v }

// codeOf 已在 admin_ops_test.go 里定义（同包），这里直接复用，不要重复声明。

func TestBatchValidationMatrix(t *testing.T) {
	e := newEnv(t, 8) // 接受的用例各自真的下单：机器要够，窗口彼此错开或落在不同机器
	dbtest.Fund(t, e.pool, e.userID, 5_000_000) // 1m×3（479,700 分）+ 1d×90（621,000 分）+ 其余单日单，startFunds 不够
	h := func(d time.Duration) *time.Time { x := dbtest.T0.Add(d); return &x } // T0 是整点
	cases := []struct {
		name string
		mut  func(r *rental.BatchRequest)
		code string
	}{
		{"exactly 2h accepted", func(r *rental.BatchRequest) { r.StartAt = h(2 * time.Hour) }, ""},
		{"too soon", func(r *rental.BatchRequest) { r.StartAt = h(time.Hour) }, "booking_too_soon"},
		{"day 60 accepted", func(r *rental.BatchRequest) { r.StartAt = h(60 * 24 * time.Hour) }, ""},
		{"day 60 + 1h rejected", func(r *rental.BatchRequest) { r.StartAt = h(60*24*time.Hour + time.Hour) }, "booking_too_far"},
		{"not hour", func(r *rental.BatchRequest) { r.StartAt = h(3*time.Hour + 30*time.Minute) }, "booking_not_hour"},
		{"1m x3 accepted", func(r *rental.BatchRequest) { r.StartAt = h(3 * time.Hour); r.Duration, r.Quantity = catalog.D1m, 3 }, ""},
		{"1m x4 rejected", func(r *rental.BatchRequest) { r.StartAt = h(3 * time.Hour); r.Duration, r.Quantity = catalog.D1m, 4 }, "booking_too_long"},
		{"1d x90 accepted immediate", func(r *rental.BatchRequest) { r.Duration, r.Quantity = catalog.D1d, 90 }, ""},
		{"1d x91 rejected", func(r *rental.BatchRequest) { r.Duration, r.Quantity = catalog.D1d, 91 }, "booking_too_long"},
		{"count 9", func(r *rental.BatchRequest) { r.Count = 9 }, "count_invalid"},
		{"count 0", func(r *rental.BatchRequest) { r.Count = 0 }, "count_invalid"},
		{"machine with count 2", func(r *rental.BatchRequest) { r.MachineID = ptr(e.machines[0]); r.Count = 2 }, "bad_request"},
		{"key with #", func(r *rental.BatchRequest) { r.IdempotencyKey = "abc#1" }, "bad_request"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := e.batchReq(1, nil)
			c.mut(&req)
			_, err := e.svc.PlaceBatch(e.ctx, req)
			if c.code == "" {
				if err != nil {
					t.Fatalf("want ok, got %v", err)
				}
				return
			}
			if got := codeOf(err); got != c.code {
				t.Fatalf("want %s, got %v", c.code, err)
			}
		})
	}
	// 非 UTC 时区传入的整点仍是整点（北京时间 11:00 = 03:00 UTC）
	loc := time.FixedZone("CST", 8*3600)
	req := e.batchReq(1, ptr(time.Date(2026, 1, 1, 11, 0, 0, 0, loc)))
	if _, err := e.svc.PlaceBatch(e.ctx, req); err != nil {
		t.Fatalf("CST hour = %v", err)
	}
	// 5 分钟容差：now = T0 + 5m 时 S = T0 + 2h（距开始 1h55m）接受；再过 1 秒拒绝
	e.clock.Advance(5 * time.Minute)
	if _, err := e.svc.PlaceBatch(e.ctx, e.batchReq(1, h(2*time.Hour))); err != nil {
		t.Fatalf("within tolerance = %v", err)
	}
	e.clock.Advance(time.Second)
	if _, err := e.svc.PlaceBatch(e.ctx, e.batchReq(1, h(2*time.Hour))); !errors.Is(err, apperr.ErrBookingTooSoon) {
		t.Fatalf("past tolerance = %v", err)
	}
}

func TestPlaceBatchBookingReservesWithoutAllocating(t *testing.T) {
	e := newEnv(t, 3)
	start := dbtest.T0.Add(3 * time.Hour)
	req := e.batchReq(2, &start)
	req.AutoRenew, req.AutoRenewDuration = true, catalog.D1w
	res, err := e.svc.PlaceBatch(e.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Batch == nil || res.Batch.Count != 2 || len(res.Orders) != 2 || len(res.Instances) != 2 {
		t.Fatalf("result = %+v", res)
	}
	for i, inst := range res.Instances {
		if inst.State != rental.StateReserved || !inst.Booked() || !inst.ScheduledStartAt.Equal(start) || inst.PendingSeconds != 86400 ||
			inst.BatchID == nil || *inst.BatchID != res.Batch.ID || !inst.AutoRenew || inst.AutoRenewDuration != catalog.D1w || inst.ExpiresAt != nil {
			t.Fatalf("instance[%d] = %+v", i, inst)
		}
		if ord := res.Orders[i]; ord.InstanceID != inst.ID || ord.Amount != 6900 || ord.Quantity != 1 || ord.BatchID == nil || *ord.BatchID != res.Batch.ID {
			t.Fatalf("order[%d] = %+v", i, ord)
		}
		if e.machineState(inst.MachineID) != fleet.StateIdle {
			t.Fatalf("booked machine must stay idle, got %s", e.machineState(inst.MachineID))
		}
		lower, upper, status := e.lease(inst.ID)
		if status != "reserved" || !lower.Equal(start.Add(-rental.PrepLead)) || !upper.Equal(start.Add(24*time.Hour+rental.Tail)) {
			t.Fatalf("lease = [%v, %v) %s", lower, upper, status)
		}
		if n := countPending(e, inst.MachineID, fleet.CmdCreateInstance); n != 0 {
			t.Fatalf("booking must not enqueue create, got %d", n)
		}
	}
	if res.Instances[0].MachineID == res.Instances[1].MachineID {
		t.Fatal("two machines expected")
	}
	if b := e.balance(e.userID); b != startFunds-2*6900 {
		t.Fatalf("balance = %d", b)
	}
	got, err := e.svc.GetBatch(e.ctx, e.userID, res.Batch.ID)
	if err != nil || len(got.Orders) != 2 || len(got.Instances) != 2 || got.Orders[1].InstanceID != got.Instances[1].ID {
		t.Fatalf("GetBatch = %+v, %v", got, err)
	}
	if _, err := e.svc.GetBatch(e.ctx, e.userID+1, res.Batch.ID); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("other user = %v", err)
	}
	// 第三台在同一时段还能订；第四台不够：整单失败，消息带台数，且前面成功的不受影响
	if _, err := e.svc.PlaceBatch(e.ctx, e.batchReq(2, &start)); codeOf(err) != "no_capacity" || !strings.Contains(err.Error(), "只有 1 台可用") {
		t.Fatalf("want no_capacity with count, got %v", err)
	}
	var reserved int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM instances WHERE state = 'reserved'`).Scan(&reserved)
	if reserved != 2 || e.balance(e.userID) != startFunds-2*6900 {
		t.Fatalf("partial batch leaked: reserved=%d balance=%d", reserved, e.balance(e.userID))
	}
}

func TestPlaceBatchImmediateIsAtomic(t *testing.T) {
	e := newEnv(t, 3)
	res, err := e.svc.PlaceBatch(e.ctx, e.batchReq(3, nil))
	if err != nil || len(res.Instances) != 3 {
		t.Fatalf("batch = %+v, %v", res, err)
	}
	for _, inst := range res.Instances {
		if inst.State != rental.StateCreating || e.machineState(inst.MachineID) != fleet.StateAllocated || countPending(e, inst.MachineID, fleet.CmdCreateInstance) != 1 {
			t.Fatalf("instance = %+v", inst)
		}
	}
	e.ack(true)
	for _, inst := range res.Instances {
		if e.get(inst.ID).State != rental.StateRunning {
			t.Fatal("expected running")
		}
	}
	// 余额不够付 2 台：整单回滚，机器仍空闲
	poor := e.newUser("13600000002", 7000)
	e.machines = append(e.machines, dbtest.CreateMachine(t, e.pool, "SN-X"), dbtest.CreateMachine(t, e.pool, "SN-Y"))
	req := e.batchReq(2, nil)
	req.UserID = poor
	if _, err := e.svc.PlaceBatch(e.ctx, req); !errors.Is(err, apperr.ErrInsufficientBalance) {
		t.Fatalf("want insufficient balance, got %v", err)
	}
	if e.machineState(e.machines[3]) != fleet.StateIdle || e.machineState(e.machines[4]) != fleet.StateIdle || e.balance(poor) != 7000 {
		t.Fatal("insufficient balance must roll back everything")
	}
}

func TestBatchIdempotencyReplayAndConflict(t *testing.T) {
	e := newEnv(t, 2)
	start := dbtest.T0.Add(3 * time.Hour)
	req := e.batchReq(2, &start)
	req.IdempotencyKey = "k1"
	first, err := e.svc.PlaceBatch(e.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := e.svc.PlaceBatch(e.ctx, req)
	if err != nil || again.Batch.ID != first.Batch.ID || len(again.Orders) != 2 || again.Instances[0].ID != first.Instances[0].ID || again.Instances[1].ID != first.Instances[1].ID {
		t.Fatalf("replay = %+v, %v", again, err) // 两边都按实例 ID 排序，顺序必须一致
	}
	if b := e.balance(e.userID); b != startFunds-2*6900 {
		t.Fatalf("replay charged again: %d", b)
	}
	var keys []string
	rows, _ := e.pool.Query(e.ctx, `SELECT idempotency_key FROM orders WHERE batch_id = $1 ORDER BY idempotency_key`, first.Batch.ID)
	for rows.Next() {
		var k string
		_ = rows.Scan(&k)
		keys = append(keys, k)
	}
	rows.Close()
	if len(keys) != 2 || keys[0] != "k1#1" || keys[1] != "k1#2" {
		t.Fatalf("sub-order keys = %v", keys)
	}
	// 同一个键走旧接口 → 冲突；走续费 → 冲突
	if _, _, err := e.svc.Place(e.ctx, rental.PlaceRequest{UserID: e.userID, ProductID: e.productID, ImageID: e.imageID, Duration: catalog.D1d, IdempotencyKey: "k1"}); codeOf(err) != "idempotency_conflict" {
		t.Fatalf("legacy with batch key = %v", err)
	}
	if _, err := e.svc.Renew(e.ctx, e.userID, first.Instances[0].ID, catalog.D1d, "k1"); codeOf(err) != "idempotency_conflict" {
		t.Fatalf("renew with batch key = %v", err)
	}
}

func TestLegacyOrderIdempotencyUnchanged(t *testing.T) {
	e := newEnv(t, 1)
	req := rental.PlaceRequest{UserID: e.userID, ProductID: e.productID, ImageID: e.imageID, Duration: catalog.D1d, IdempotencyKey: "legacy-1"}
	ord, inst, err := e.svc.Place(e.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	ord2, inst2, err := e.svc.Place(e.ctx, req)
	if err != nil || ord2.ID != ord.ID || inst2.ID != inst.ID {
		t.Fatalf("replay = %+v %+v %v", ord2, inst2, err)
	}
	if ord.BatchID != nil || ord.Quantity != 1 || ord.Auto {
		t.Fatalf("legacy order = %+v", ord)
	}
	var batches int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM rental_batches`).Scan(&batches)
	if batches != 0 {
		t.Fatalf("legacy path must not create batches, got %d", batches)
	}
	var key string
	_ = e.pool.QueryRow(e.ctx, `SELECT idempotency_key FROM orders WHERE id = $1`, ord.ID).Scan(&key)
	if key != "legacy-1" {
		t.Fatalf("legacy key stored as %q", key)
	}
	req.IdempotencyKey = "legacy#1"
	if _, _, err := e.svc.Place(e.ctx, req); codeOf(err) != "bad_request" {
		t.Fatalf("key with # = %v", err)
	}
	// 键被批次占用后旧接口报冲突（上面的 TestBatchIdempotencyReplayAndConflict 覆盖反向）
}

func TestBookingLimitPerUser(t *testing.T) {
	e := newEnv(t, 1)
	for i := 0; i < rental.MaxOpenReservationsPerUser; i++ {
		start := dbtest.T0.Add(3*time.Hour + time.Duration(i)*48*time.Hour)
		if _, err := e.svc.PlaceBatch(e.ctx, e.batchReq(1, &start)); err != nil {
			t.Fatalf("booking %d: %v", i, err)
		}
	}
	start := dbtest.T0.Add(3*time.Hour + 16*48*time.Hour)
	if _, err := e.svc.PlaceBatch(e.ctx, e.batchReq(1, &start)); !errors.Is(err, apperr.ErrBookingLimit) {
		t.Fatalf("17th = %v", err)
	}
}

func TestSpecificMachineUnavailable(t *testing.T) {
	e := newEnv(t, 2)
	start := dbtest.T0.Add(3 * time.Hour)
	e.reserveRaw(e.machines[0], start, 86400)
	req := e.batchReq(1, &start)
	req.MachineID = &e.machines[0]
	if _, err := e.svc.PlaceBatch(e.ctx, req); !errors.Is(err, apperr.ErrMachineUnavailable) {
		t.Fatalf("want machine_unavailable, got %v", err)
	}
	req.MachineID = &e.machines[1]
	if res, err := e.svc.PlaceBatch(e.ctx, req); err != nil || res.Instances[0].MachineID != e.machines[1] {
		t.Fatalf("specific machine = %+v, %v", res, err)
	}
}

func TestRenewQuantityChargesMultiple(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	ord, err := e.svc.RenewQuantity(e.ctx, e.userID, inst.ID, catalog.D1d, 3, "")
	if err != nil || ord.Amount != 3*6900 || ord.Quantity != 3 {
		t.Fatalf("renew x3 = %+v, %v", ord, err)
	}
	if got := e.get(inst.ID); !got.ExpiresAt.Equal(dbtest.T0.Add(4 * 24 * time.Hour)) {
		t.Fatalf("expires = %v", got.ExpiresAt)
	}
	if _, err := e.svc.RenewQuantity(e.ctx, e.userID, inst.ID, catalog.D1m, 4, ""); !errors.Is(err, apperr.ErrBookingTooLong) {
		t.Fatalf("1m x4 = %v", err)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/rental/ -run 'TestBatch|TestPlaceBatch|TestLegacyOrder|TestBookingLimit|TestSpecificMachine|TestRenewQuantity' 2>&1 | head -20`
Expected: 编译错误（`BatchRequest`、`PlaceBatch`、`GetBatch`、`RenewQuantity` 未定义）。

- [ ] **Step 3: 幂等与重试辅助**

`internal/rental/idempotency.go` 整文件：

```go
package rental

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
)

const maxIdempotencyKey = 64

var errIdempotencyConflict = apperr.New("idempotency_conflict", "幂等键已被其他请求使用", 409)

// checkIdempotencyKey：长度 ≤ 64，且不能含 '#'（批次子订单的键是 "<key>#<序号>"，用户若能提交含 # 的键就可能撞上别人的子单键）。
func checkIdempotencyKey(key string) error {
	if len(key) > maxIdempotencyKey {
		return apperr.BadRequest("幂等键长度不能超过 64")
	}
	if strings.Contains(key, "#") {
		return apperr.BadRequest("幂等键不能包含 #")
	}
	return nil
}

// lockIdempotency 取事务级咨询锁串行化同一用户同一幂等键的请求。咨询锁在事务内最先获取，之后才按头注释的顺序加行锁。
func lockIdempotency(ctx context.Context, tx pgx.Tx, userID int64, key string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "idem:"+strconv.FormatInt(userID, 10)+":"+key)
	return err
}

// lockIdempotencyKey（单台下单与续费用）：加锁后先查批次——键若被 POST /bookings 用过即冲突——再查订单。
func lockIdempotencyKey(ctx context.Context, tx pgx.Tx, userID int64, key string) (Order, bool, error) {
	if err := lockIdempotency(ctx, tx, userID, key); err != nil {
		return Order{}, false, err
	}
	if _, found, err := findBatchByKey(ctx, tx, userID, key); err != nil {
		return Order{}, false, err
	} else if found {
		return Order{}, false, errIdempotencyConflict
	}
	return findByIdempotencyKey(ctx, tx, userID, key)
}

func findByIdempotencyKey(ctx context.Context, q db.Querier, userID int64, key string) (Order, bool, error) {
	ord, err := scanOrder(q.QueryRow(ctx, `SELECT `+orderCols+` FROM orders WHERE user_id = $1 AND idempotency_key = $2`, userID, key))
	if errors.Is(err, apperr.ErrNotFound) {
		return Order{}, false, nil
	}
	if err != nil {
		return Order{}, false, err
	}
	return ord, true, nil
}

func findBatchByKey(ctx context.Context, q db.Querier, userID int64, key string) (Batch, bool, error) {
	var b Batch
	err := q.QueryRow(ctx, `SELECT id, user_id, count, created_at FROM rental_batches WHERE user_id = $1 AND idempotency_key = $2`, userID, key).
		Scan(&b.ID, &b.UserID, &b.Count, &b.CreatedAt)
	if db.IsNoRows(err) {
		return Batch{}, false, nil
	}
	if err != nil {
		return Batch{}, false, err
	}
	return b, true, nil
}

func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// isIdempotencyViolation 判断是否为并发重复请求触发的幂等键唯一约束冲突（订单）。
func isIdempotencyViolation(err error) bool {
	return pgCode(err) == "23505" && db.ConstraintName(err) == "orders_user_idempotency_uniq"
}

// isBatchIdempotencyViolation 判断是否为并发重复请求触发的幂等键唯一约束冲突（批次）。
func isBatchIdempotencyViolation(err error) bool {
	return pgCode(err) == "23505" && db.ConstraintName(err) == "rental_batches_user_idempotency_uniq"
}

// withRetry 对死锁检测（40P01）与序列化失败（40001）把整个事务重试一次；其它错误原样返回。
func withRetry(fn func() error) error {
	err := fn()
	if c := pgCode(err); c == "40P01" || c == "40001" {
		err = fn()
	}
	return err
}
```

- [ ] **Step 4: `charge` 签名**

`internal/rental/orders.go` 的 `charge` 改为：

```go
// charge 在事务内按当前价格 × quantity 扣款并创建订单（idemKey 为空表示不带幂等键）。实例行必须已存在（orders.instance_id 外键）。
func (s *Service) charge(ctx context.Context, tx pgx.Tx, userID int64, kind, instanceID string, productID int64, d catalog.Duration, quantity int,
	auto bool, batchID *string, idemKey string, now time.Time) (Order, error) {
	if quantity < 1 {
		quantity = 1
	}
	unit, err := s.d.Catalog.Quote(ctx, tx, productID, d)
	if err != nil {
		return Order{}, err
	}
	amount := unit * int64(quantity)
	id := randx.ID("ord-", 12)
	remark := "新购 " + instanceID
	switch {
	case kind == "renew" && auto:
		remark = "自动续费 " + instanceID
	case kind == "renew":
		remark = "续费 " + instanceID
	}
	if _, err := s.d.Wallet.Apply(ctx, tx, wallet.Entry{
		UserID: userID, Amount: -amount, Kind: wallet.KindOrderPay, RefType: "order", RefID: id, Remark: remark,
	}); err != nil {
		return Order{}, err
	}
	return scanOrder(tx.QueryRow(ctx, `INSERT INTO orders (id, user_id, kind, instance_id, product_id, duration, quantity, amount, status,
			idempotency_key, batch_id, auto, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'paid', NULLIF($9, ''), $10, $11, $12, $12) RETURNING `+orderCols,
		id, userID, kind, instanceID, productID, string(d), quantity, amount, idemKey, batchID, auto, now))
}
```

- [ ] **Step 5: `batch.go`**

```go
package rental

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/randx"
	"spark-cloud/internal/platform/tz"
)

// BatchRequest 一次下单：count 台、同一商品 / 镜像 / 租期；StartAt 为 nil 表示立即开通，否则为预订（整点，北京时间）。
type BatchRequest struct {
	UserID            int64
	ProductID         int64
	ImageID           int64
	GroupID           *int64 // nil = 不限机组
	MachineID         *int64 // 指定机器（Count 须为 1）
	Count             int
	Duration          catalog.Duration
	Quantity          int // 租期单位数，0 视为 1
	StartAt           *time.Time
	AutoRenew         bool
	AutoRenewDuration catalog.Duration // 空 = Duration
	IdempotencyKey    string
}

type Batch struct {
	ID        string    `json:"id"`
	UserID    int64     `json:"-"`
	Count     int       `json:"count"`
	CreatedAt time.Time `json:"created_at"`
}

// BatchResult：Orders[i] 对应 Instances[i]；Batch 在旧接口（POST /orders）路径为 nil。
type BatchResult struct {
	Batch     *Batch
	Orders    []Order
	Instances []Instance
}

// validateBatch 校验并规范化请求（规格 §6.1、裁定 B1），返回总秒数。
func validateBatch(req *BatchRequest, now time.Time) (int64, error) {
	if !req.Duration.Valid() {
		return 0, apperr.BadRequest("租期不正确")
	}
	if req.Quantity == 0 {
		req.Quantity = 1
	}
	if req.Quantity < 1 || req.Quantity > MaxQuantity(req.Duration) {
		return 0, apperr.ErrBookingTooLong
	}
	if req.Count < 1 || req.Count > MaxMachinesPerOrder {
		return 0, apperr.ErrCountInvalid
	}
	if req.MachineID != nil && req.Count != 1 {
		return 0, apperr.BadRequest("指定机器时只能租用 1 台")
	}
	if req.AutoRenewDuration == "" {
		req.AutoRenewDuration = req.Duration
	}
	if !req.AutoRenewDuration.Valid() {
		return 0, apperr.BadRequest("自动续费档位不正确")
	}
	if err := checkIdempotencyKey(req.IdempotencyKey); err != nil {
		return 0, err
	}
	if req.StartAt != nil {
		start := req.StartAt.UTC()
		if !tz.IsHour(start) {
			return 0, apperr.ErrBookingNotHour
		}
		if start.Before(now.Add(MinBookingLead - BookingLeadTolerance)) {
			return 0, apperr.ErrBookingTooSoon
		}
		if start.After(now.Add(MaxBookingAhead)) {
			return 0, apperr.ErrBookingTooFar
		}
		req.StartAt = &start
	}
	return seconds(req.Duration) * int64(req.Quantity), nil
}

// PlaceBatch 一次租 Count 台（规格 §6）：整个事务要么全部成功要么全部失败；建批次行；子订单键为 "<key>#<n>"。
func (s *Service) PlaceBatch(ctx context.Context, req BatchRequest) (BatchResult, error) {
	return s.placeBatch(ctx, req, true)
}

func (s *Service) placeBatch(ctx context.Context, req BatchRequest, withBatch bool) (BatchResult, error) {
	now := s.d.Clock.Now()
	secs, err := validateBatch(&req, now)
	if err != nil {
		return BatchResult{}, err
	}
	if err := s.d.Users.CanRent(ctx, req.UserID); err != nil {
		return BatchResult{}, err
	}
	var out BatchResult
	run := func() error {
		out = BatchResult{}
		return db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
			if req.IdempotencyKey != "" {
				if err := lockIdempotency(ctx, tx, req.UserID, req.IdempotencyKey); err != nil {
					return err
				}
				prev, found, err := s.replayByKey(ctx, tx, req.UserID, req.IdempotencyKey, withBatch)
				if found || err != nil {
					out = prev
					return err
				}
			}
			if req.StartAt != nil {
				if err := s.reserveSlots(ctx, tx, req.UserID, req.Count); err != nil {
					return err
				}
			}
			img, err := s.d.Catalog.GetImage(ctx, tx, req.ImageID)
			if err != nil {
				return err
			}
			if req.GroupID != nil {
				var visible bool
				if err := tx.QueryRow(ctx, `SELECT visible FROM machine_groups WHERE id = $1`, *req.GroupID).Scan(&visible); db.IsNoRows(err) || (err == nil && !visible) {
					return apperr.ErrGroupNotFound
				} else if err != nil {
					return err
				}
			}
			spec := pickSpec{ProductID: req.ProductID, GroupID: req.GroupID, MachineID: req.MachineID, Immediate: req.StartAt == nil}
			if spec.Immediate {
				spec.Lower, spec.Upper = immediatePeriod(now, secs)
			} else {
				spec.Lower, spec.Upper = bookingPeriod(*req.StartAt, secs)
			}
			var batchID *string
			if withBatch {
				id := randx.ID("bat-", 12)
				batchID = &id
			}
			var insts []Instance
			_, err = placeOnMachines(ctx, tx, spec, req.Count, func(sp pgx.Tx, mid int64) error {
				if spec.Immediate {
					if _, err := s.d.Fleet.MarkAllocated(ctx, sp, mid); err != nil {
						return err
					}
				}
				inst, err := s.insertInstance(ctx, sp, req, mid, secs, batchID, now)
				if err != nil {
					return err
				}
				status := "reserved"
				if spec.Immediate {
					status = "active"
				}
				if _, err := insertLease(ctx, sp, mid, inst.ID, spec.Lower, spec.Upper, status, now); err != nil {
					return err
				}
				insts = append(insts, inst)
				return nil
			})
			if err != nil {
				if req.MachineID != nil && codeIs(err, "no_capacity") {
					return apperr.ErrMachineUnavailable
				}
				return err
			}
			if withBatch {
				if _, err := tx.Exec(ctx, `INSERT INTO rental_batches (id, user_id, count, idempotency_key, created_at) VALUES ($1, $2, $3, NULLIF($4, ''), $5)`,
					*batchID, req.UserID, req.Count, req.IdempotencyKey, now); err != nil {
					return err
				}
				out.Batch = &Batch{ID: *batchID, UserID: req.UserID, Count: req.Count, CreatedAt: now}
			}
			for i, inst := range insts {
				key := req.IdempotencyKey
				if withBatch && key != "" {
					key = fmt.Sprintf("%s#%d", key, i+1)
				}
				ord, err := s.charge(ctx, tx, req.UserID, "new", inst.ID, req.ProductID, req.Duration, req.Quantity, false, batchID, key, now)
				if err != nil {
					return err
				}
				reason := "order " + ord.ID
				if req.StartAt != nil {
					reason += " scheduled " + tz.Format(*req.StartAt)
				}
				if err := recordEvent(ctx, tx, inst.ID, "", inst.State, reason, now); err != nil {
					return err
				}
				if spec.Immediate {
					if _, err := s.d.Fleet.Enqueue(ctx, tx, inst.MachineID, inst.ID, fleet.CmdCreateInstance, fleet.CreateInstancePayload{ImageRef: img.Ref}); err != nil {
						return err
					}
				}
				out.Orders = append(out.Orders, ord)
			}
			// 顺序唯一：按实例 ID 排序（loadBatch 回放也按 id），避免「首单按挑机顺序、回放按 id」不一致
			sort.Slice(insts, func(i, j int) bool { return insts[i].ID < insts[j].ID })
			sort.Slice(out.Orders, func(i, j int) bool { return out.Orders[i].InstanceID < out.Orders[j].InstanceID })
			out.Instances = insts
			return nil
		})
	}
	err = withRetry(run)
	if isIdempotencyViolation(err) || isBatchIdempotencyViolation(err) {
		// 并发的重复请求已先提交：本事务已回滚，返回先提交的那笔。
		prev, found, ferr := s.replayByKey(ctx, s.d.Pool, req.UserID, req.IdempotencyKey, withBatch)
		if ferr != nil {
			return BatchResult{}, ferr
		}
		if found {
			return prev, nil
		}
	}
	if err != nil {
		return BatchResult{}, err
	}
	return out, nil
}

func codeIs(err error, code string) bool {
	ae := apperr.As(err)
	return ae != nil && ae.Code == code
}

// insertInstance 插入一台新实例（即时 creating / 预订 reserved），密码与主机密钥在此生成。
func (s *Service) insertInstance(ctx context.Context, q db.Querier, req BatchRequest, machineID int64, secs int64, batchID *string, now time.Time) (Instance, error) {
	hostPub, hostPEM, err := s.d.HostKeyGen()
	if err != nil {
		return Instance{}, err
	}
	state := StateCreating
	if req.StartAt != nil {
		state = StateReserved
	}
	id := randx.ID("ins-", 8)
	return scanInstance(q.QueryRow(ctx, `INSERT INTO instances
		(id, user_id, machine_id, product_id, image_id, state, pending_seconds, ssh_password_enc, ssh_host_pubkey, ssh_host_key_enc,
		 scheduled_start_at, group_id, batch_id, auto_renew, auto_renew_duration, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $16) RETURNING `+instanceCols,
		id, req.UserID, machineID, req.ProductID, req.ImageID, state, secs, s.d.Box.Seal([]byte(randx.Password(16))), hostPub, s.d.Box.Seal(hostPEM),
		req.StartAt, req.GroupID, batchID, req.AutoRenew, string(req.AutoRenewDuration), now))
}

// reserveSlots 预订分支：按用户串行化（咨询锁）后检查未开通预订上限（裁定 B1：16 台）。
func (s *Service) reserveSlots(ctx context.Context, tx pgx.Tx, userID int64, count int) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "booking:"+strconv.FormatInt(userID, 10)); err != nil {
		return err
	}
	var open int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM instances WHERE user_id = $1 AND state = 'reserved'`, userID).Scan(&open); err != nil {
		return err
	}
	if open+count > MaxOpenReservationsPerUser {
		return apperr.ErrBookingLimit
	}
	return nil
}

// replayByKey 按幂等键回放（调用方已持咨询锁或在事务外兜底）：命中批次 → 只有 /bookings 形态可回放；
// 命中订单 → 只有 /orders 形态且是无批次的新购单可回放；其它组合冲突。
func (s *Service) replayByKey(ctx context.Context, q db.Querier, userID int64, key string, withBatch bool) (BatchResult, bool, error) {
	if b, found, err := findBatchByKey(ctx, q, userID, key); err != nil {
		return BatchResult{}, false, err
	} else if found {
		if !withBatch {
			return BatchResult{}, true, errIdempotencyConflict
		}
		res, err := s.loadBatch(ctx, q, b)
		return res, true, err
	}
	ord, found, err := findByIdempotencyKey(ctx, q, userID, key)
	if err != nil || !found {
		return BatchResult{}, false, err
	}
	if withBatch || ord.Kind != "new" || ord.BatchID != nil {
		return BatchResult{}, true, errIdempotencyConflict
	}
	o, inst, err := s.replayPlace(ctx, q, ord)
	if err != nil {
		return BatchResult{}, true, err
	}
	return BatchResult{Orders: []Order{o}, Instances: []Instance{inst}}, true, nil
}

// loadBatch 返回批次的订单与实例当前状态（按实例 ID 对齐）。
func (s *Service) loadBatch(ctx context.Context, q db.Querier, b Batch) (BatchResult, error) {
	rows, err := q.Query(ctx, `SELECT `+orderCols+` FROM orders WHERE batch_id = $1 AND kind = 'new' ORDER BY instance_id`, b.ID)
	if err != nil {
		return BatchResult{}, err
	}
	orders, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Order, error) { return scanOrder(r) })
	if err != nil {
		return BatchResult{}, err
	}
	insts, err := collectInstances(q.Query(ctx, `SELECT `+instanceCols+` FROM instances WHERE batch_id = $1 ORDER BY id`, b.ID))
	if err != nil {
		return BatchResult{}, err
	}
	if len(orders) != len(insts) {
		return BatchResult{}, errors.New("batch orders and instances out of sync")
	}
	return BatchResult{Batch: &b, Orders: orders, Instances: insts}, nil
}

// GetBatch 返回用户自己的批次。
func (s *Service) GetBatch(ctx context.Context, userID int64, id string) (BatchResult, error) {
	var b Batch
	err := s.d.Pool.QueryRow(ctx, `SELECT id, user_id, count, created_at FROM rental_batches WHERE id = $1 AND user_id = $2`, id, userID).
		Scan(&b.ID, &b.UserID, &b.Count, &b.CreatedAt)
	if db.IsNoRows(err) {
		return BatchResult{}, apperr.ErrNotFound
	}
	if err != nil {
		return BatchResult{}, err
	}
	return s.loadBatch(ctx, s.d.Pool, b)
}
```

- [ ] **Step 6: `Place` 变为包装**

`internal/rental/place.go` 整文件：

```go
package rental

import (
	"context"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/platform/db"
)

type PlaceRequest struct {
	UserID    int64
	ProductID int64
	ImageID   int64
	Duration  catalog.Duration
	// IdempotencyKey 非空时，同一用户重复提交同一键只会生成一笔订单，重试返回首次创建的订单与实例。
	IdempotencyKey string
}

// Place 新购一台整机（旧接口 POST /orders）：等价于一台、一个单位、立即开通、不建批次的 PlaceBatch；
// 幂等键原样写在订单上，回放行为与计划 1 完全相同。
func (s *Service) Place(ctx context.Context, req PlaceRequest) (Order, Instance, error) {
	res, err := s.placeBatch(ctx, BatchRequest{UserID: req.UserID, ProductID: req.ProductID, ImageID: req.ImageID, Count: 1,
		Duration: req.Duration, Quantity: 1, IdempotencyKey: req.IdempotencyKey}, false)
	if err != nil {
		return Order{}, Instance{}, err
	}
	return res.Orders[0], res.Instances[0], nil
}

// replayPlace 返回幂等键对应的已有新购订单及其实例的当前状态；键被续费订单占用时报冲突。
func (s *Service) replayPlace(ctx context.Context, q db.Querier, prev Order) (Order, Instance, error) {
	if prev.Kind != "new" {
		return Order{}, Instance{}, errIdempotencyConflict
	}
	inst, err := scanInstance(q.QueryRow(ctx, `SELECT `+instanceCols+` FROM instances WHERE id = $1`, prev.InstanceID))
	if err != nil {
		return Order{}, Instance{}, err
	}
	return prev, inst, nil
}
```

- [ ] **Step 7: `RenewQuantity`**

`internal/rental/renew.go`：把 `Renew` 改为包装，新增 `RenewQuantity`（原 `Renew` 的函数体整体移入，差别只有 quantity 校验、`charge` 参数与 `addTime` 的秒数）：

```go
// Renew 续费一个单位；见 RenewQuantity。
func (s *Service) Renew(ctx context.Context, userID int64, instanceID string, d catalog.Duration, idempotencyKey string) (Order, error) {
	return s.RenewQuantity(ctx, userID, instanceID, d, 1, idempotencyKey)
}

// RenewQuantity 续费 quantity 个单位（quantity 0 视为 1，受 90 天上限）：扣款建单后按实例当前状态延长时长。
// wiping/released/failed/cancelled 状态不允许续费且不扣款。idempotencyKey 非空时同一用户重复提交同一键只扣款一次。
func (s *Service) RenewQuantity(ctx context.Context, userID int64, instanceID string, d catalog.Duration, quantity int, idempotencyKey string) (Order, error) {
	if !d.Valid() {
		return Order{}, apperr.BadRequest("租期不正确")
	}
	if quantity == 0 {
		quantity = 1
	}
	if quantity < 1 || quantity > MaxQuantity(d) {
		return Order{}, apperr.ErrBookingTooLong
	}
	if err := checkIdempotencyKey(idempotencyKey); err != nil {
		return Order{}, err
	}
	now := s.d.Clock.Now()
	secs := seconds(d) * int64(quantity)
	var ord Order
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		if idempotencyKey != "" {
			prev, found, err := lockIdempotencyKey(ctx, tx, userID, idempotencyKey)
			if err != nil {
				return err
			}
			if found {
				ord, err = replayRenew(prev, instanceID)
				return err
			}
		}
		inst, err := lockInstance(ctx, tx, instanceID)
		if err != nil {
			return err
		}
		if inst.UserID != userID {
			return apperr.ErrNotFound
		}
		if inst.State != StateReserved && inst.State != StateCreating && inst.State != StateRunning && inst.State != StateStopped {
			return apperr.ErrInvalidState
		}
		if ord, err = s.charge(ctx, tx, userID, "renew", inst.ID, inst.ProductID, d, quantity, false, nil, idempotencyKey, now); err != nil {
			return err
		}
		if err := s.addTime(ctx, tx, inst, secs, now); err != nil {
			return err
		}
		if err := recordEvent(ctx, tx, inst.ID, inst.State, inst.State, "renew "+ord.ID, now); err != nil {
			return err
		}
		if err := s.refreshLease(ctx, tx, inst.ID, now); err != nil {
			return err
		}
		return s.verifyLeaseFits(ctx, tx, inst.ID, now)
	})
	if isIdempotencyViolation(err) {
		prev, found, ferr := findByIdempotencyKey(ctx, s.d.Pool, userID, idempotencyKey)
		if ferr != nil {
			return Order{}, ferr
		}
		if found {
			return replayRenew(prev, instanceID)
		}
	}
	if err != nil {
		return Order{}, err
	}
	return ord, nil
}
```

`addTime` 加 `reserved` 分支（在 `case inst.State == StateCreating:` 之前）：

```go
	case inst.State == StateReserved:
		_, err = tx.Exec(ctx, `UPDATE instances SET pending_seconds = pending_seconds + $2, updated_at = $3 WHERE id = $1`, inst.ID, secs, now)
		if err == nil { // 预订的占用时段按新时长推后（refreshLease 对 reserved 不动，这里直接改）
			_, err = tx.Exec(ctx, `UPDATE machine_leases SET period = tstzrange(lower(period), upper(period) + ($2::bigint * interval '1 second')), updated_at = $3
				WHERE instance_id = $1 AND status = 'reserved'`, inst.ID, secs, now)
		}
```

（排他冲突在 Task 10 由 `renewFits` 预先拦住；此处若仍撞约束则按 `db.IsExclusionViolation` 映射为 `renew_blocked`——Task 10 实现，本任务先原样返回错误。）

`Place` 的旧实现里 `s.charge(...)` 调用已随 Step 6 消失；`AdminMigrate` 不调 `charge`。`internal/rental/export_test.go` 不需要再加导出。

- [ ] **Step 8: 运行测试确认通过**

Run: `go vet ./... && go test ./internal/rental/ ./internal/api/ ./internal/adminapi/ ./internal/app/ 2>&1 | tail -8`
Expected: 全部 `ok`。`TestPlaceAllocatesChargesAndEnqueuesCreate`（旧接口）与 `idempotency_test.go` 不改一行仍通过。

- [ ] **Step 9: 提交**

```bash
git add internal/rental/batch.go internal/rental/batch_test.go internal/rental/place.go internal/rental/orders.go internal/rental/idempotency.go internal/rental/renew.go internal/rental/export_test.go
git commit -m "feat(booking): PlaceBatch (multi-machine, booking branch, batch idempotency), quantity pricing, RenewQuantity, deadlock retry

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 6: 退款折算按 `duration × quantity`

**Files:**
- Modify: `internal/rental/refund.go`（`refundable`、`refundableOrders`、`quote`、`refundAll`）
- Test: `internal/rental/refund_test.go`（追加）

**Interfaces:**
- Consumes: Task 5 的 `orders.quantity`、`RenewQuantity`、`PlaceBatch`。
- Produces: `refundable` 新增 `quantity int`；`quote` 的单价折算用 `seconds(latest.duration) × latest.quantity`；`refundAll` 的自付秒数累计 `seconds(o.duration) × o.quantity`。对外签名不变。

- [ ] **Step 1: 写失败的测试**

`internal/rental/refund_test.go` 追加：

```go
func TestRefundQuoteUsesQuantity(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	renew, err := e.svc.RenewQuantity(e.ctx, e.userID, inst.ID, catalog.D1d, 3, "")
	if err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(12 * time.Hour)
	q, _ := e.svc.RefundQuote(e.ctx, inst.ID)
	remaining := int64(4*86400 - 12*3600)
	// 单价按最新订单：20700 分 / (3 × 86400 秒)
	if q.UnitOrderID != renew.ID || q.RemainingSeconds != remaining || q.Suggested != remaining*20700/(3*86400) || q.Max != 6900+20700 {
		t.Fatalf("quote = %+v", q)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/rental/ -run 'TestRefundQuoteUsesQuantity' 2>&1 | tail -6`
Expected: 失败（`Suggested` 按 1 个单位折算，偏大 3 倍）。`refundAll` 的 `ownSecs` 只影响换机后的退款上限：不单独加「改前就能通过」的测试，由现有 `refund_test.go` 的换机退款用例在改后继续通过来保证。

- [ ] **Step 3: 改 `refund.go`**

```go
type refundable struct {
	id          string
	amount      int64
	refunded    int64
	refundCount int
	duration    catalog.Duration
	quantity    int
}
```

`refundableOrders` 的 SQL 改为 `SELECT id, amount, refunded_amount, refund_count, duration, quantity FROM orders …`，`Scan` 末尾加 `&o.quantity`。

`quote` 里：

```go
	if len(orders) > 0 {
		latest := orders[0]
		q.UnitOrderID = latest.id
		if secs := seconds(latest.duration) * int64(max(latest.quantity, 1)); secs > 0 {
			q.Suggested = q.RemainingSeconds * latest.amount / secs
		}
	}
```

`refundAll` 的查询改为 `SELECT id, amount - refunded_amount, duration, quantity FROM orders …`，`paid` 结构体加 `quantity int`，`Scan` 加 `&p.quantity`，累计改为 `ownSecs += seconds(o.duration) * int64(max(o.quantity, 1))`。

- [ ] **Step 4: 运行测试确认通过**

Run: `go vet ./... && go test ./internal/rental/ 2>&1 | tail -4`
Expected: `ok`。

- [ ] **Step 5: 提交**

```bash
git add internal/rental/refund.go internal/rental/refund_test.go
git commit -m "fix(rental): refund quote and create-failure refund account for order quantity

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 7: 取消（用户两档 + 冷静期 / 后台 / 平台）、`cancel-quote`、`Notifier` 扩展与短信模板

**Files:**
- Create: `internal/rental/cancel.go`、`internal/rental/cancel_test.go`
- Modify: `internal/rental/service.go`（`Notifier` 接口）、`internal/rental/alerts.go`（三条新告警文案）、`internal/plugins/sms/sms.go`（两个模板常量）、`internal/app/app.go`（`expiryNotifier` 实现新方法）、`internal/rental/helpers_test.go`（`fakeNotifier`）、`internal/api/api_test.go`（`rentalNoop`）、`internal/adminapi/helpers_test.go`（`noopNotifier`）、`internal/agentgw/helpers_test.go`（`noopNotifier`，它也实现 `rental.Notifier`）

**Interfaces:**
- Consumes: Task 5 的 `BatchRequest` / `PlaceBatch`；Task 1 的 `StateCancelled`、`CancelCoolOff` 等常量；现有 `refundableOrders` / `refundAcross`。
- Produces:
  - `rental.Notifier` 接口增加 `NotifyAutoRenewFailed(ctx, userID int64, instanceID, reason string) error`、`NotifyBookingCancelled(ctx, userID int64, instanceID, reason string) error`。
  - `sms.TemplateAutoRenewFailed = "auto_renew_failed"`（参数 `instance`、`reason`）、`sms.TemplateBookingCancelled = "booking_cancelled"`（参数 `instance`、`reason`）。
  - `rental.CancelQuote{Refund int64 \`json:"refund"\`; Percent int \`json:"percent"\`; Deadline time.Time \`json:"deadline"\`; Max int64 \`json:"max"\`}`；`(*Service).CancelQuote(ctx, userID, id) (CancelQuote, error)`（非 reserved → `ErrInvalidState`）。
  - `(*Service).Cancel(ctx, userID int64, id, ip string) (Instance, error)`：reserved 且 `now < S − PrepLead` 才允许，过期 → `ErrCancelNotAllowed`，其它状态 → `ErrInvalidState`；比例 `cancelPercent(inst, now)`（冷静期 100 / ≥ 24h 100 / 否则 50）；审计 `booking.cancel`（user actor）。
  - `(*Service).AdminCancelBooking(ctx, actor, id, reason string) (refunded int64, err error)`：仅 reserved；100%；审计 `booking.admin_cancel`；提交后发短信 `NotifyBookingCancelled(…, "平台取消")`。
  - `cancelInstance(ctx, tx, inst, percent int, operatorID int64, reason, remark string, now) (int64, error)`：lease → `cancelled`，实例 → `cancelled`（`cancelled_at`、`ssh_host_key_enc = NULL`），`refundAcross`（金额 = 未退总额 × percent / 100，向下取整），事件 `<from> → cancelled "<reason>, refund <分>"`。
  - `cancelPlatform(ctx, tx, inst, reason string, now) (int64, error)`：`cancelInstance(…, 100, 0, reason, "平台取消预订 <id>", now)`；短信由调用方在事务提交后发（Task 8 的 `ProvisionDue` 用）。
  - 告警文案：`AlertBookingAtRisk(id)` = `实例 <id> 的预订机器不可用且机组内暂无可改派机器`、`AlertBookingUnfulfilled(id)` = `实例 <id> 的预订无法履约，已全额退款`、`AlertBookingCancelFailed(id)` = `实例 <id> 的预订取消失败，详情见日志`。

- [ ] **Step 1: 写失败的测试**

`internal/rental/helpers_test.go` 的 `fakeNotifier` 增加两个方法（放在 `Calls` 之前）：

```go
func (n *fakeNotifier) NotifyAutoRenewFailed(_ context.Context, _ int64, instanceID, reason string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, "auto_renew_failed:"+instanceID+":"+reason)
	return nil
}

func (n *fakeNotifier) NotifyBookingCancelled(_ context.Context, _ int64, instanceID, reason string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, "booking_cancelled:"+instanceID+":"+reason)
	return nil
}
```

`internal/api/api_test.go` 的 `rentalNoop` 与 `internal/adminapi/helpers_test.go` 的 `noopNotifier` 各加：

```go
func (rentalNoop) NotifyAutoRenewFailed(context.Context, int64, string, string) error  { return nil }
func (rentalNoop) NotifyBookingCancelled(context.Context, int64, string, string) error { return nil }
```

（adminapi 与 agentgw 的 `helpers_test.go` 里接收者都写 `noopNotifier`；三个文件缺一个都会让 `go vet ./...` 失败。）

`internal/rental/cancel_test.go`：

```go
package rental_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/rental"
)

func (e *env) book(start time.Time) rental.Instance {
	e.t.Helper()
	res, err := e.svc.PlaceBatch(e.ctx, e.batchReq(1, &start))
	if err != nil {
		e.t.Fatalf("book: %v", err)
	}
	return res.Instances[0]
}

func (e *env) auditCountOf(action string) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM audit_logs WHERE action = $1`, action).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func TestCancelQuoteTiers(t *testing.T) {
	e := newEnv(t, 2)
	near := e.book(dbtest.T0.Add(3 * time.Hour)) // 距开始 3h：冷静期内全退，之后 50%
	q, err := e.svc.CancelQuote(e.ctx, e.userID, near.ID)
	if err != nil || q.Percent != 100 || q.Refund != 6900 || q.Max != 6900 || !q.Deadline.Equal(dbtest.T0.Add(2*time.Hour)) {
		t.Fatalf("cool-off quote = %+v, %v", q, err)
	}
	e.clock.Advance(16 * time.Minute)
	if q, _ = e.svc.CancelQuote(e.ctx, e.userID, near.ID); q.Percent != 50 || q.Refund != 3450 {
		t.Fatalf("late quote = %+v", q)
	}
	far := e.book(dbtest.T0.Add(40 * time.Hour)) // 距开始 > 24h：全退
	e.clock.Advance(16 * time.Minute)
	if q, _ = e.svc.CancelQuote(e.ctx, e.userID, far.ID); q.Percent != 100 || q.Refund != 6900 {
		t.Fatalf("far quote = %+v", q)
	}
	if _, err := e.svc.CancelQuote(e.ctx, e.userID+1, far.ID); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("other user = %v", err)
	}
}

func TestUserCancelRefundsAndClosesLease(t *testing.T) {
	e := newEnv(t, 1)
	inst := e.book(dbtest.T0.Add(3 * time.Hour))
	e.clock.Advance(20 * time.Minute) // 过了冷静期，距开始 2h40m → 50%
	got, err := e.svc.Cancel(e.ctx, e.userID, inst.ID, "1.2.3.4")
	if err != nil || got.State != rental.StateCancelled || got.CancelledAt == nil {
		t.Fatalf("cancel = %+v, %v", got, err)
	}
	if b := e.balance(e.userID); b != startFunds-6900+3450 {
		t.Fatalf("balance = %d", b)
	}
	if _, _, status := e.lease(inst.ID); status != "cancelled" {
		t.Fatalf("lease status = %s", status)
	}
	var keyEnc []byte
	_ = e.pool.QueryRow(e.ctx, `SELECT ssh_host_key_enc FROM instances WHERE id = $1`, inst.ID).Scan(&keyEnc)
	if keyEnc != nil {
		t.Fatal("host key must be cleared")
	}
	if e.auditCountOf("booking.cancel") != 1 {
		t.Fatal("audit missing")
	}
	events, _ := e.svc.InstanceEvents(e.ctx, inst.ID)
	if last := events[len(events)-1]; last.ToState != rental.StateCancelled || last.Reason != "user cancel, refund 3450" {
		t.Fatalf("event = %+v", last)
	}
	if _, err := e.svc.Cancel(e.ctx, e.userID, inst.ID, ""); !errors.Is(err, apperr.ErrInvalidState) {
		t.Fatalf("cancel twice = %v", err)
	}
	// 机器可以被别的预订再次使用
	if _, err := e.svc.PlaceBatch(e.ctx, e.batchReq(1, ptr(dbtest.T0.Add(3*time.Hour)))); err != nil {
		t.Fatalf("rebook after cancel = %v", err)
	}
}

func TestCancelNotAllowedAfterProvisionTime(t *testing.T) {
	e := newEnv(t, 1)
	inst := e.book(dbtest.T0.Add(3 * time.Hour))
	e.clock.Advance(2 * time.Hour) // 正好到开通时刻（S − 1h）
	if _, err := e.svc.Cancel(e.ctx, e.userID, inst.ID, ""); !errors.Is(err, apperr.ErrCancelNotAllowed) {
		t.Fatalf("at deadline = %v", err)
	}
	if e.get(inst.ID).State != rental.StateReserved {
		t.Fatal("must stay reserved")
	}
}

func TestCancelRefundsPreStartRenewalNewestFirst(t *testing.T) {
	e := newEnv(t, 1)
	inst := e.book(dbtest.T0.Add(3 * time.Hour))
	renew, err := e.svc.Renew(e.ctx, e.userID, inst.ID, catalog.D1d, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := e.get(inst.ID); got.PendingSeconds != 2*86400 {
		t.Fatalf("pending after pre-start renew = %d", got.PendingSeconds)
	}
	if _, upper, _ := e.lease(inst.ID); !upper.Equal(dbtest.T0.Add(3*time.Hour + 48*time.Hour + rental.Tail)) {
		t.Fatalf("lease upper after renew = %v", upper)
	}
	e.clock.Advance(20 * time.Minute) // 50%：13800 × 50% = 6900，先退最新的续费单
	if _, err := e.svc.Cancel(e.ctx, e.userID, inst.ID, ""); err != nil {
		t.Fatal(err)
	}
	if status, refunded := e.orderState(renew.ID); status != "refunded" || refunded != 6900 {
		t.Fatalf("renew order = %s %d", status, refunded)
	}
	if b := e.balance(e.userID); b != startFunds-13800+6900 {
		t.Fatalf("balance = %d", b)
	}
}

func TestAdminCancelBookingFullRefund(t *testing.T) {
	e := newEnv(t, 1)
	inst := e.book(dbtest.T0.Add(3 * time.Hour))
	e.clock.Advance(20 * time.Minute)
	if _, err := e.svc.AdminCancelBooking(e.ctx, audit.Actor{AdminID: 9}, inst.ID, ""); apperr.As(err) == nil {
		t.Fatalf("empty reason = %v", err)
	}
	refunded, err := e.svc.AdminCancelBooking(e.ctx, audit.Actor{AdminID: 9}, inst.ID, "机房断电")
	if err != nil || refunded != 6900 {
		t.Fatalf("admin cancel = %d, %v", refunded, err)
	}
	if b := e.balance(e.userID); b != startFunds {
		t.Fatalf("balance = %d", b)
	}
	if e.auditCountOf("booking.admin_cancel") != 1 || !slices.Contains(e.notifier.Calls(), "booking_cancelled:"+inst.ID+":平台取消") {
		t.Fatalf("audit / sms missing: %v", e.notifier.Calls())
	}
	if _, err := e.svc.AdminCancelBooking(e.ctx, audit.Actor{AdminID: 9}, inst.ID, "again"); !errors.Is(err, apperr.ErrInvalidState) {
		t.Fatalf("cancel twice = %v", err)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/rental/ -run 'TestCancel|TestUserCancel|TestAdminCancel' 2>&1 | head -20`
Expected: 编译错误（`CancelQuote`、`Cancel`、`AdminCancelBooking` 未定义；`fakeNotifier` 多出的方法暂无接口要求但能编译）。

- [ ] **Step 3: `Notifier`、短信模板、告警文案**

`internal/rental/service.go`：

```go
type Notifier interface {
	NotifyExpiry(ctx context.Context, userID int64, instanceID string, remaining time.Duration) error
	// 计划 6：自动续费失败（reason：余额不足 / 机器已有后续预订）与预订被平台取消（reason：无法安排机器 / 账号不可租用 / 平台取消）
	NotifyAutoRenewFailed(ctx context.Context, userID int64, instanceID, reason string) error
	NotifyBookingCancelled(ctx context.Context, userID int64, instanceID, reason string) error
}
```

`internal/plugins/sms/sms.go` 常量块：

```go
const (
	TemplateLoginCode        = "login_code"        // 参数：code
	TemplateExpiryReminder   = "expiry_reminder"   // 参数：instance、hours
	TemplateAutoRenewFailed  = "auto_renew_failed" // 参数：instance、reason
	TemplateBookingCancelled = "booking_cancelled" // 参数：instance、reason
)
```

`internal/app/app.go` 的 `expiryNotifier` 追加：

```go
func (n expiryNotifier) NotifyAutoRenewFailed(ctx context.Context, userID int64, instanceID, reason string) error {
	u, err := n.ids.GetUser(ctx, userID)
	if err != nil {
		return err
	}
	return n.sms.Send(ctx, u.Phone, sms.TemplateAutoRenewFailed, map[string]string{"instance": instanceID, "reason": reason})
}

func (n expiryNotifier) NotifyBookingCancelled(ctx context.Context, userID int64, instanceID, reason string) error {
	u, err := n.ids.GetUser(ctx, userID)
	if err != nil {
		return err
	}
	return n.sms.Send(ctx, u.Phone, sms.TemplateBookingCancelled, map[string]string{"instance": instanceID, "reason": reason})
}
```

`internal/rental/alerts.go` 追加：

```go
func AlertBookingAtRisk(instanceID string) string {
	return fmt.Sprintf("实例 %s 的预订机器不可用且机组内暂无可改派机器", instanceID)
}
func AlertBookingUnfulfilled(instanceID string) string {
	return fmt.Sprintf("实例 %s 的预订无法履约，已全额退款", instanceID)
}
func AlertBookingCancelFailed(instanceID string) string {
	return fmt.Sprintf("实例 %s 的预订取消失败，详情见日志", instanceID)
}
```

- [ ] **Step 4: `cancel.go`**

```go
package rental

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
)

// CancelQuote 取消预订的退款预览（规格 §5.5，裁定 B4）。
type CancelQuote struct {
	Refund   int64     `json:"refund"`
	Percent  int       `json:"percent"`
	Deadline time.Time `json:"deadline"` // 可取消的截止时刻 = 开始 − PrepLead
	Max      int64     `json:"max"`      // 已付未退总额
}

// cancelPercent：下单 15 分钟内全退（冷静期）；之后距开始 ≥ 24h 全退，否则 50%。
func cancelPercent(inst Instance, now time.Time) int {
	if now.Before(inst.CreatedAt.Add(CancelCoolOff)) {
		return 100
	}
	if inst.ScheduledStartAt != nil && inst.ScheduledStartAt.Sub(now) >= FreeCancelBefore {
		return 100
	}
	return LateCancelRefundPercent
}

func unrefunded(orders []refundable) int64 {
	var max int64
	for _, o := range orders {
		max += o.amount - o.refunded
	}
	return max
}

// CancelQuote 不修改任何数据；只对 reserved 实例有意义。
func (s *Service) CancelQuote(ctx context.Context, userID int64, id string) (CancelQuote, error) {
	inst, err := s.GetForUser(ctx, userID, id)
	if err != nil {
		return CancelQuote{}, err
	}
	if inst.State != StateReserved || !inst.Booked() {
		return CancelQuote{}, apperr.ErrInvalidState
	}
	orders, err := refundableOrders(ctx, s.d.Pool, []string{inst.ID}, false)
	if err != nil {
		return CancelQuote{}, err
	}
	now := s.d.Clock.Now()
	pct := cancelPercent(inst, now)
	max := unrefunded(orders)
	return CancelQuote{Refund: max * int64(pct) / 100, Percent: pct, Deadline: inst.ScheduledStartAt.Add(-PrepLead), Max: max}, nil
}

// Cancel 用户取消预订：只在开通时刻（开始前 1 小时）之前允许；退款比例见 cancelPercent。
func (s *Service) Cancel(ctx context.Context, userID int64, id, ip string) (Instance, error) {
	now := s.d.Clock.Now()
	var out Instance
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		inst, err := lockInstance(ctx, tx, id)
		if err != nil {
			return err
		}
		if inst.UserID != userID {
			return apperr.ErrNotFound
		}
		if inst.State != StateReserved || !inst.Booked() {
			return apperr.ErrInvalidState
		}
		if !now.Before(inst.ScheduledStartAt.Add(-PrepLead)) {
			return apperr.ErrCancelNotAllowed
		}
		pct := cancelPercent(inst, now)
		refund, err := s.cancelInstance(ctx, tx, inst, pct, 0, "user cancel", "取消预订 "+inst.ID, now)
		if err != nil {
			return err
		}
		if err := audit.Record(ctx, tx, now, audit.Actor{UserID: userID, IP: ip}.Entry("booking.cancel", "instance", inst.ID,
			map[string]any{"refund": refund, "percent": pct})); err != nil {
			return err
		}
		out, err = lockInstance(ctx, tx, inst.ID)
		return err
	})
	return out, err
}

// AdminCancelBooking 后台取消预订（ops + 动态码由 API 层校验）：全额退款；提交后发短信。
func (s *Service) AdminCancelBooking(ctx context.Context, actor audit.Actor, id, reason string) (int64, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return 0, apperr.BadRequest("请填写取消原因")
	}
	now := s.d.Clock.Now()
	var refund int64
	var userID int64
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		inst, err := lockInstance(ctx, tx, id)
		if err != nil {
			return err
		}
		if inst.State != StateReserved || !inst.Booked() {
			return apperr.ErrInvalidState
		}
		userID = inst.UserID
		refund, err = s.cancelInstance(ctx, tx, inst, 100, actor.AdminID, "admin cancel: "+reason, "后台取消预订 "+inst.ID+"："+reason, now)
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, actor.Entry("booking.admin_cancel", "instance", inst.ID, map[string]any{"reason": reason, "refund": refund}))
	})
	if err != nil {
		return 0, err
	}
	if err := s.d.Notifier.NotifyBookingCancelled(ctx, userID, id, "平台取消"); err != nil {
		s.d.Log.Warn("booking cancelled sms failed", "instance", id, "err", err)
	}
	return refund, nil
}

// cancelInstance 把 reserved（或改派失败后的 creating）实例取消：lease 与实例置 cancelled、清主机私钥、按比例退款、写事件。
// 调用方已持实例锁。返回实际退款金额。
func (s *Service) cancelInstance(ctx context.Context, tx pgx.Tx, inst Instance, percent int, operatorID int64, reason, remark string, now time.Time) (int64, error) {
	if _, err := tx.Exec(ctx, `UPDATE machine_leases SET status = 'cancelled', updated_at = $2
		WHERE instance_id = $1 AND status IN ('reserved', 'active')`, inst.ID, now); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `UPDATE instances SET state = 'cancelled', cancelled_at = $2, ssh_host_key_enc = NULL, updated_at = $2 WHERE id = $1`, inst.ID, now); err != nil {
		return 0, err
	}
	if err := s.d.Fleet.CancelCommands(ctx, tx, inst.ID, "booking cancelled"); err != nil {
		return 0, err
	}
	orders, err := refundableOrders(ctx, tx, []string{inst.ID}, true)
	if err != nil {
		return 0, err
	}
	amount := unrefunded(orders) * int64(percent) / 100
	if _, err := s.refundAcross(ctx, tx, inst.UserID, orders, amount, operatorID, remark, now); err != nil {
		return 0, err
	}
	return amount, recordEvent(ctx, tx, inst.ID, inst.State, StateCancelled, fmt.Sprintf("%s, refund %d", reason, amount), now)
}

// cancelPlatform 平台原因取消（无法履约、用户失去租用资格）：全额退；短信由调用方在事务提交后发。
func (s *Service) cancelPlatform(ctx context.Context, tx pgx.Tx, inst Instance, reason string, now time.Time) (int64, error) {
	return s.cancelInstance(ctx, tx, inst, 100, 0, reason, "平台取消预订 "+inst.ID, now)
}
```

- [ ] **Step 5: 运行测试确认通过**

Run: `go vet ./... && go test ./internal/rental/ ./internal/api/ ./internal/adminapi/ ./internal/agentgw/ ./internal/app/ 2>&1 | tail -8`
Expected: 全部 `ok`。

- [ ] **Step 6: 提交**

```bash
git add internal/rental/cancel.go internal/rental/cancel_test.go internal/rental/service.go internal/rental/alerts.go internal/rental/helpers_test.go internal/plugins/sms/sms.go internal/app/app.go internal/api/api_test.go internal/adminapi/helpers_test.go internal/agentgw/helpers_test.go
git commit -m "feat(booking): user/admin/platform cancellation with cool-off and two-tier refund, notifier and sms templates

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 8: 到点开通 `ProvisionDue`、预订计时与迟开通补偿、改派（开通时 / Create 失败 / 超时）

**Files:**
- Create: `internal/rental/provision.go`、`internal/rental/reassign.go`、`internal/rental/provision_test.go`
- Modify: `internal/rental/sweeper.go`（`sweepEachAfter`、`FailStuckCreating` 谓词与分支）、`internal/rental/results.go`（`HandleCommandResult` 的 Create 失败分支、`startLease` 预订分支）

**Interfaces:**
- Consumes: Task 2 `lockLease` / `insertLease` / `nextReservedLower` / `tz`；Task 4 `pickSpec` / `placeOnMachines`；Task 7 `cancelPlatform`、`Notifier.NotifyBookingCancelled`、告警文案；`Fleet.MarkAllocated` / `MarkFaulty` / `CancelCommands` / `RaiseAlert` / `RaiseAlertOnce` / `Enqueue`；`Catalog.ImageRef`；`Users.CanRent`。
- Produces:
  - `(*Service).sweepEachAfter(ctx, pred string, arg any, orderBy string, fn func(pgx.Tx, Instance) (after func(), err error), onErr func(id string, err error)) (int, error)`：与 `sweepEach` 相同，但回调可返回「提交后执行」的动作，单行失败先回调 `onErr`；`sweepEach` 改为它的包装。
  - `(*Service).ProvisionDue(ctx) (int, error)`：谓词 `state = 'reserved' AND scheduled_start_at <= $1`（`$1 = now + PrepLead`）。
  - `provision(ctx, tx, inst, now) (after func(), err error)`；`machineReady(ctx, tx, machineID) (bool, error)`（锁机器行）；`startProvision(ctx, tx, inst, lease, now) error`。
  - `reassign(ctx, tx, inst *Instance, lease *leaseRow, reason string, needOnline bool, now) (moved bool, err error)`：成功时就地更新 `inst.MachineID / ReassignCount` 与 `lease.ID / MachineID`，新 lease 以 `reserved` 插入（开通路径随后由 `startProvision` / Create 失败路径置 `active`），旧 lease `cancelled`，事件 `"reassigned <old> -> <new>: <reason>"`。
  - `reassignAfterCreateFailure(ctx, tx, inst, reason string, now, after *func()) error`（规格 §5.4 末段）。
  - `(Instance).Remaining(now)`：预订实例在 `now < S` 时返回 `expires_at − S`（早开通期间剩余时长不递减；`api.view` 的 `remaining_seconds` 与退款折算 `remainingSeconds` 都随之正确）。
  - `startLease` 对预订首次开通走 `startBooked`：`expires = min(max(S, now) + pending, cap − Tail)`；差额 `compensateShortfall`（按最新可退订单单价退款，remark「迟开通补偿 <id>」，事件 `"late start compensated: <secs>s refund <amount>"`）；事件 `"agent: created (scheduled <S>)"` 或 `"… late by <Δ>)"`。
  - 类型 `cancelFailure{err}`（`Unwrap`），`ProvisionDue` 对它在独立事务里 `RaiseAlertOnce(critical, booking_cancel_failed)`。

- [ ] **Step 1: 写失败的测试**

`internal/rental/provision_test.go`：

```go
package rental_test

import (
	"slices"
	"testing"
	"time"

	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/rental"
)

func (e *env) alerts(kind string) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM alerts WHERE kind = $1`, kind).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) lastEvent(id string) rental.Event {
	e.t.Helper()
	events, err := e.svc.InstanceEvents(e.ctx, id)
	if err != nil || len(events) == 0 {
		e.t.Fatalf("events: %v", err)
	}
	return events[len(events)-1]
}

func TestProvisionDueStartsOneHourBefore(t *testing.T) {
	e := newEnv(t, 1)
	start := dbtest.T0.Add(3 * time.Hour)
	inst := e.book(start)
	e.clock.Advance(time.Hour + 59*time.Minute)
	n, err := e.svc.ProvisionDue(e.ctx)
	mustCount(t, n, err, 0, "provision before T-1h")
	e.clock.Advance(time.Minute)
	e.heartbeatAll()
	n, err = e.svc.ProvisionDue(e.ctx)
	mustCount(t, n, err, 1, "provision at T-1h")
	got := e.get(inst.ID)
	if got.State != rental.StateCreating || got.ProvisionedAt == nil || !got.ProvisionedAt.Equal(e.clock.Now()) || got.PendingSeconds != 86400 {
		t.Fatalf("after provision = %+v", got)
	}
	if e.machineState(e.machines[0]) != fleet.StateAllocated || countPending(e, e.machines[0], fleet.CmdCreateInstance) != 1 {
		t.Fatal("machine must be allocated with a create command")
	}
	if _, _, status := e.lease(inst.ID); status != "active" {
		t.Fatalf("lease = %s", status)
	}
	if ev := e.lastEvent(inst.ID); ev.FromState != rental.StateReserved || ev.ToState != rental.StateCreating || ev.Reason != "scheduled start 2026-01-01 11:00" {
		t.Fatalf("event = %+v", ev)
	}
	n, err = e.svc.ProvisionDue(e.ctx)
	mustCount(t, n, err, 0, "provision twice")

	// 早开通：5 分钟后 Agent 回报 running，到期 = S + 1d（不是 now + 1d），这段时间可用但不计费
	e.clock.Advance(5 * time.Minute)
	e.ack(true)
	got = e.get(inst.ID)
	if got.State != rental.StateRunning || !got.ExpiresAt.Equal(start.Add(24*time.Hour)) {
		t.Fatalf("after create = %+v", got)
	}
	if ev := e.lastEvent(inst.ID); ev.Reason != "agent: created (scheduled 2026-01-01 11:00)" {
		t.Fatalf("event = %+v", ev)
	}
	if _, upper, _ := e.lease(inst.ID); !upper.Equal(start.Add(24*time.Hour + rental.Tail)) {
		t.Fatalf("lease upper = %v", upper)
	}
	if r := e.get(inst.ID).Remaining(e.clock.Now()); r != 24*time.Hour {
		t.Fatalf("remaining before S must not tick: %v", r)
	}
	e.clock.Advance(55 * time.Minute) // 到 S
	if r := e.get(inst.ID).Remaining(e.clock.Now()); r != 24*time.Hour {
		t.Fatalf("remaining at S = %v", r)
	}
	e.clock.Advance(time.Hour)
	if r := e.get(inst.ID).Remaining(e.clock.Now()); r != 23*time.Hour {
		t.Fatalf("remaining after S = %v", r)
	}
}

func TestProvisionLateStartIsCompensatedAndCapped(t *testing.T) {
	e := newEnv(t, 1)
	start := dbtest.T0.Add(3 * time.Hour)
	inst := e.book(start)
	// 同机紧随其后的预订：下界 = 第一单上界 = S + 1d + 7h
	next := e.book(start.Add(24*time.Hour + rental.Tail + rental.PrepLead))
	e.clock.Advance(2 * time.Hour)
	e.heartbeatAll()
	if n, err := e.svc.ProvisionDue(e.ctx); err != nil || n != 1 {
		t.Fatalf("provision = %d, %v", n, err)
	}
	e.clock.Advance(time.Hour + 30*time.Minute) // Agent 晚了 30 分钟：now = S + 30m
	balance := e.balance(e.userID)
	e.ack(true)
	got := e.get(inst.ID)
	// base = S + 30m，想要 S + 30m + 24h；封顶 cap − Tail = (S + 31h) − 7h = S + 24h；差额 30 分钟
	if !got.ExpiresAt.Equal(start.Add(24 * time.Hour)) {
		t.Fatalf("expires = %v, want %v (capped)", got.ExpiresAt, start.Add(24*time.Hour))
	}
	refund := int64(1800) * 6900 / 86400
	if b := e.balance(e.userID); b != balance+refund {
		t.Fatalf("balance = %d, want compensation %d", b, refund)
	}
	events, _ := e.svc.InstanceEvents(e.ctx, inst.ID)
	var reasons []string
	for _, ev := range events {
		reasons = append(reasons, ev.Reason)
	}
	if !slices.Contains(reasons, "agent: created (scheduled 2026-01-01 11:00, late by 30m0s)") || !slices.Contains(reasons, "late start compensated: 1800s refund 143") {
		t.Fatalf("events = %v", reasons)
	}
	if _, upper, _ := e.lease(inst.ID); !upper.Equal(start.Add(24*time.Hour + rental.Tail)) {
		t.Fatalf("lease upper = %v", upper)
	}
	if e.get(next.ID).State != rental.StateReserved {
		t.Fatal("next booking untouched")
	}
}

func TestProvisionReassignsWhenMachineUnavailable(t *testing.T) {
	e := newEnv(t, 2)
	start := dbtest.T0.Add(3 * time.Hour)
	req := e.batchReq(1, &start)
	req.MachineID = &e.machines[0]
	res, err := e.svc.PlaceBatch(e.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	inst := res.Instances[0]
	e.setMachine(e.machines[0], "maintenance", true)
	e.clock.Advance(2 * time.Hour)
	e.heartbeatAll()
	if n, err := e.svc.ProvisionDue(e.ctx); err != nil || n != 1 {
		t.Fatalf("provision = %d, %v", n, err)
	}
	got := e.get(inst.ID)
	if got.State != rental.StateCreating || got.MachineID != e.machines[1] || got.ReassignCount != 1 {
		t.Fatalf("after reassign = %+v", got)
	}
	if e.machineState(e.machines[1]) != fleet.StateAllocated || countPending(e, e.machines[1], fleet.CmdCreateInstance) != 1 {
		t.Fatal("new machine must be allocated with create")
	}
	var leases []string
	rows, _ := e.pool.Query(e.ctx, `SELECT status FROM machine_leases WHERE instance_id = $1 ORDER BY id`, inst.ID)
	for rows.Next() {
		var st string
		_ = rows.Scan(&st)
		leases = append(leases, st)
	}
	rows.Close()
	if !slices.Equal(leases, []string{"cancelled", "active"}) {
		t.Fatalf("leases = %v", leases)
	}
	events, _ := e.svc.InstanceEvents(e.ctx, inst.ID)
	found := false
	for _, ev := range events {
		if ev.Reason == "reassigned "+itoa64(e.machines[0])+" -> "+itoa64(e.machines[1])+": machine unavailable at start" {
			found = true
		}
	}
	if !found {
		t.Fatalf("reassign event missing: %+v", events)
	}
}

func itoa64(n int64) string { return fmt.Sprintf("%d", n) }

func TestProvisionWaitsThenCancelsAfterGrace(t *testing.T) {
	e := newEnv(t, 1)
	start := dbtest.T0.Add(3 * time.Hour)
	inst := e.book(start)
	e.setMachine(e.machines[0], "maintenance", true)
	e.clock.Advance(2 * time.Hour)
	if n, err := e.svc.ProvisionDue(e.ctx); err != nil || n != 1 {
		t.Fatalf("provision = %d, %v", n, err)
	}
	if e.get(inst.ID).State != rental.StateReserved || e.alerts("booking_at_risk") != 1 {
		t.Fatalf("expected reserved + at-risk alert, got %s / %d", e.get(inst.ID).State, e.alerts("booking_at_risk"))
	}
	e.clock.Advance(time.Hour + 29*time.Minute) // S + 29m：仍在宽限期
	if _, err := e.svc.ProvisionDue(e.ctx); err != nil {
		t.Fatal(err)
	}
	if e.get(inst.ID).State != rental.StateReserved || e.alerts("booking_at_risk") != 1 {
		t.Fatal("still waiting, alert not duplicated")
	}
	e.clock.Advance(2 * time.Minute) // S + 31m：取消、全退、告警、短信
	if _, err := e.svc.ProvisionDue(e.ctx); err != nil {
		t.Fatal(err)
	}
	got := e.get(inst.ID)
	if got.State != rental.StateCancelled || e.balance(e.userID) != startFunds || e.alerts("booking_unfulfilled") != 1 {
		t.Fatalf("after grace = %+v balance=%d", got, e.balance(e.userID))
	}
	if _, _, status := e.lease(inst.ID); status != "cancelled" {
		t.Fatalf("lease = %s", status)
	}
	if !slices.Contains(e.notifier.Calls(), "booking_cancelled:"+inst.ID+":无法安排机器") {
		t.Fatalf("sms = %v", e.notifier.Calls())
	}
	if ev := e.lastEvent(inst.ID); ev.Reason != "unfulfilled: no machine by grace, refund 6900" {
		t.Fatalf("event = %+v", ev)
	}
}

func TestProvisionCancelsWhenUserCannotRent(t *testing.T) {
	e := newEnv(t, 1)
	inst := e.book(dbtest.T0.Add(3 * time.Hour))
	if _, err := e.pool.Exec(e.ctx, `UPDATE users SET status = 'banned' WHERE id = $1`, e.userID); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(2 * time.Hour)
	if n, err := e.svc.ProvisionDue(e.ctx); err != nil || n != 1 {
		t.Fatalf("provision = %d, %v", n, err)
	}
	if got := e.get(inst.ID); got.State != rental.StateCancelled || e.balance(e.userID) != startFunds {
		t.Fatalf("banned user booking = %+v", got)
	}
	if ev := e.lastEvent(inst.ID); ev.Reason != "cancelled: user cannot rent, refund 6900" {
		t.Fatalf("event = %+v", ev)
	}
	if !slices.Contains(e.notifier.Calls(), "booking_cancelled:"+inst.ID+":账号不可租用") {
		t.Fatalf("sms = %v", e.notifier.Calls())
	}
}

func TestCreateFailureReassignsBookedInstance(t *testing.T) {
	e := newEnv(t, 2)
	start := dbtest.T0.Add(3 * time.Hour)
	req := e.batchReq(1, &start)
	req.MachineID = &e.machines[0]
	res, _ := e.svc.PlaceBatch(e.ctx, req)
	inst := res.Instances[0]
	e.clock.Advance(2 * time.Hour)
	e.heartbeatAll()
	_, _ = e.svc.ProvisionDue(e.ctx)
	before := e.get(inst.ID)
	balance := e.balance(e.userID)
	e.ack(false) // 机器 0 上 Create 失败
	got := e.get(inst.ID)
	if got.State != rental.StateCreating || got.MachineID != e.machines[1] || got.ReassignCount != 1 || got.PendingSeconds != 86400 ||
		got.ProvisionedAt == nil || got.SSHHostPubkey == before.SSHHostPubkey {
		t.Fatalf("after create failure = %+v (before %+v)", got, before)
	}
	if e.balance(e.userID) != balance {
		t.Fatal("booked instance must not be refunded on create failure")
	}
	if e.machineState(e.machines[0]) != fleet.StateFaulty || e.alerts("create_failed") != 1 {
		t.Fatal("old machine faulty + create_failed alert expected")
	}
	if countPending(e, e.machines[1], fleet.CmdCreateInstance) != 1 || e.machineState(e.machines[1]) != fleet.StateAllocated {
		t.Fatal("create must be re-enqueued on the new machine")
	}
	if _, _, status := e.lease(inst.ID); status != "active" {
		t.Fatalf("lease = %s", status)
	}
	if ev := e.lastEvent(inst.ID); ev.FromState != rental.StateCreating || ev.ToState != rental.StateCreating ||
		ev.Reason != "reassigned "+itoa64(e.machines[0])+" -> "+itoa64(e.machines[1])+": create failed" {
		t.Fatalf("event = %+v", ev)
	}
	e.ack(true)
	if e.get(inst.ID).State != rental.StateRunning {
		t.Fatal("expected running on the new machine")
	}
}

func TestCreateFailureNoCandidateGoesBackToReserved(t *testing.T) {
	e := newEnv(t, 1)
	start := dbtest.T0.Add(3 * time.Hour)
	inst := e.book(start)
	e.clock.Advance(2 * time.Hour)
	e.heartbeatAll()
	_, _ = e.svc.ProvisionDue(e.ctx)
	e.ack(false)
	got := e.get(inst.ID)
	if got.State != rental.StateReserved || got.ProvisionedAt != nil || got.MachineID != e.machines[0] || got.PendingSeconds != 86400 {
		t.Fatalf("after create failure without candidates = %+v", got)
	}
	var n int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM machine_leases WHERE instance_id = $1 AND status = 'reserved'`, inst.ID).Scan(&n)
	if n != 1 || e.alerts("booking_at_risk") != 1 || e.machineState(e.machines[0]) != fleet.StateFaulty {
		t.Fatalf("lease reserved=%d at_risk=%d machine=%s", n, e.alerts("booking_at_risk"), e.machineState(e.machines[0]))
	}
	if ev := e.lastEvent(inst.ID); ev.FromState != rental.StateCreating || ev.ToState != rental.StateReserved || ev.Reason != "create failed, waiting for machine" {
		t.Fatalf("event = %+v", ev)
	}
	// 运维修好机器（结束维护回到 idle）后下一轮重新开通
	e.setMachine(e.machines[0], "idle", true)
	e.clock.Advance(time.Minute)
	e.heartbeatAll()
	if n, err := e.svc.ProvisionDue(e.ctx); err != nil || n != 1 {
		t.Fatalf("re-provision = %d, %v", n, err)
	}
	if e.get(inst.ID).State != rental.StateCreating {
		t.Fatal("expected creating again")
	}
}

func TestCreateTimeoutUsesProvisionedAt(t *testing.T) {
	e := newEnv(t, 2)
	start := dbtest.T0.Add(30 * time.Hour)
	inst := e.book(start)
	e.clock.Advance(29 * time.Hour)
	e.heartbeatAll()
	_, _ = e.svc.ProvisionDue(e.ctx)
	e.clock.Advance(rental.CreateTimeout - time.Minute)
	e.heartbeatAll()
	n, err := e.svc.FailStuckCreating(e.ctx)
	mustCount(t, n, err, 0, "timeout before provisioned_at + CreateTimeout (created_at is 29h old)")
	e.clock.Advance(2 * time.Minute)
	n, err = e.svc.FailStuckCreating(e.ctx)
	mustCount(t, n, err, 1, "timeout after")
	got := e.get(inst.ID)
	if got.State != rental.StateCreating || got.MachineID != e.machines[1] || got.ReassignCount != 1 {
		t.Fatalf("after timeout = %+v", got)
	}
}
```

（文件 import 需要 `"fmt"`。`e.setMachine` 在 `alloc_test.go`，`e.book` 在 `cancel_test.go`。）

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/rental/ -run 'TestProvision|TestCreateFailure|TestCreateTimeoutUsesProvisionedAt' 2>&1 | head -20`
Expected: 编译错误（`ProvisionDue` 未定义）。

- [ ] **Step 3: `sweepEachAfter` 与 `FailStuckCreating`**

`internal/rental/sweeper.go`：把 `sweepEach` 改为包装，新增 `sweepEachAfter`：

```go
// sweepEach 先不加锁地选出满足 pred（以 $1 引用 arg）的候选实例，再逐个在独立事务内处理：
// 加锁时用 SKIP LOCKED 并重新校验 pred，已被其他清扫处理或状态已变的行直接跳过；
// 单行失败只回滚该行，错误汇总返回，其余行继续处理。返回成功处理的行数。
func (s *Service) sweepEach(ctx context.Context, pred string, arg any, orderBy string, fn func(pgx.Tx, Instance) error) (int, error) {
	return s.sweepEachAfter(ctx, pred, arg, orderBy, func(tx pgx.Tx, inst Instance) (func(), error) { return nil, fn(tx, inst) }, nil)
}

// sweepEachAfter 同 sweepEach，但回调可返回一个在该行事务提交后执行的动作（发短信等外部调用不进事务），
// 单行失败时先回调 onErr（用于在独立事务里告警）。
func (s *Service) sweepEachAfter(ctx context.Context, pred string, arg any, orderBy string,
	fn func(pgx.Tx, Instance) (func(), error), onErr func(id string, err error)) (int, error) {
	rows, err := s.d.Pool.Query(ctx, `SELECT id FROM instances WHERE `+pred+` ORDER BY `+orderBy+` LIMIT $2`, arg, sweepBatch)
	if err != nil {
		return 0, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, err
	}
	n := 0
	var errs []error
	for _, id := range ids {
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		done := false
		var after func()
		err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
			inst, err := scanInstance(tx.QueryRow(ctx, `SELECT `+instanceCols+` FROM instances
				WHERE id = $2 AND `+pred+` FOR UPDATE SKIP LOCKED`, arg, id))
			if errors.Is(err, apperr.ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			after, err = fn(tx, inst)
			if err != nil {
				return err
			}
			done = true
			return nil
		})
		if err != nil {
			if onErr != nil {
				onErr(id, err)
			}
			errs = append(errs, fmt.Errorf("instance %s: %w", id, err))
			continue
		}
		if after != nil {
			after()
		}
		if done {
			n++
		}
	}
	return n, errors.Join(errs...)
}
```

`FailStuckCreating` 改为：

```go
// FailStuckCreating 把开通超时的实例判为失败并全额退款；预订实例改为改派（规格 §5.4）。
// 超时从预订的开通时刻（provisioned_at）起算，即时订单仍从 created_at 起算。
func (s *Service) FailStuckCreating(ctx context.Context) (int, error) {
	now := s.d.Clock.Now()
	return s.sweepEachAfter(ctx, `state = 'creating' AND COALESCE(provisioned_at, created_at) <= $1`, now.Add(-CreateTimeout), "created_at",
		func(tx pgx.Tx, inst Instance) (func(), error) {
			if inst.Booked() {
				var after func()
				err := s.reassignAfterCreateFailure(ctx, tx, inst, "开通超时", now, &after)
				return after, err
			}
			return nil, s.failCreating(ctx, tx, inst, "开通超时", now)
		}, nil)
}
```

- [ ] **Step 4: `results.go`：Create 失败分支与 `startLease` 预订分支；`Remaining` 的早开通分支**

`internal/rental/service.go` 的 `Remaining` 改为：

```go
// Remaining 返回剩余可用时长；暂停中返回冻结的剩余时长；预订实例在开始时刻之前返回整段预订时长（早开通不计费）；非运行状态返回 0。
func (i Instance) Remaining(now time.Time) time.Duration {
	if i.State != StateRunning {
		return 0
	}
	if i.SuspendedAt != nil && i.PausedRemaining != nil {
		return time.Duration(*i.PausedRemaining) * time.Second
	}
	if i.ExpiresAt == nil {
		return 0
	}
	if i.ScheduledStartAt != nil && now.Before(*i.ScheduledStartAt) {
		return i.ExpiresAt.Sub(*i.ScheduledStartAt)
	}
	if r := i.ExpiresAt.Sub(now); r > 0 {
		return r
	}
	return 0
}
```

`HandleCommandResult` 改为在事务外持有一个 `after func()`：

```go
func (s *Service) HandleCommandResult(ctx context.Context, commandID string, ok bool, result string) error {
	var after func()
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		// …（原有内容不变，直到 switch）
		switch cmd.Type {
		case fleet.CmdCreateInstance:
			if inst.State != StateCreating {
				return nil
			}
			if ok {
				return s.startLease(ctx, tx, inst, StateCreating, "agent: created", now)
			}
			if inst.Booked() {
				return s.reassignAfterCreateFailure(ctx, tx, inst, "开通失败: "+result, now, &after)
			}
			return s.failCreating(ctx, tx, inst, "开通失败: "+result, now)
		// …其余分支不变
		}
		return nil
	})
	if err == nil && after != nil {
		after()
	}
	return err
}
```

`startLease` 开头加预订分支：

```go
// startLease 把实例转为 running，并从此刻开始计时 pending_seconds；预订的首次开通按规格 §5.3 计时。
func (s *Service) startLease(ctx context.Context, tx pgx.Tx, inst Instance, from, reason string, now time.Time) error {
	if from == StateCreating && inst.Booked() && inst.ProvisionedAt != nil {
		return s.startBooked(ctx, tx, inst, now)
	}
	// …原有实现不变
}

// startBooked：早开通不早计费（base = max(S, now)），迟开通从实际开通算，但到期封顶于同机下一条预订（cap − Tail），
// 封顶吃掉的时长按最新订单单价退款（迟开通补偿）。
func (s *Service) startBooked(ctx context.Context, tx pgx.Tx, inst Instance, now time.Time) error {
	start := *inst.ScheduledStartAt
	base := start
	if now.After(base) {
		base = now
	}
	pending := time.Duration(inst.PendingSeconds) * time.Second
	want := base.Add(pending)
	expires := want
	if cap, ok, err := nextReservedLower(ctx, tx, inst.MachineID, inst.ID, start.Add(-PrepLead)); err != nil {
		return err
	} else if ok && expires.After(cap.Add(-Tail)) {
		expires = cap.Add(-Tail)
	}
	r24, r1 := reminderFlags(expires.Sub(now))
	if _, err := tx.Exec(ctx, `UPDATE instances SET state = 'running', expires_at = $2, pending_seconds = 0,
			stopped_at = NULL, reminded_24h = $3, reminded_1h = $4, updated_at = $5 WHERE id = $1`,
		inst.ID, expires, r24, r1, now); err != nil {
		return err
	}
	reason := "agent: created (scheduled " + tz.Format(start) + ")"
	if now.After(start) {
		reason = fmt.Sprintf("agent: created (scheduled %s, late by %s)", tz.Format(start), now.Sub(start).Round(time.Second))
	}
	if err := recordEvent(ctx, tx, inst.ID, StateCreating, StateRunning, reason, now); err != nil {
		return err
	}
	if short := want.Sub(expires); short > 0 {
		if err := s.compensateShortfall(ctx, tx, inst, int64(short/time.Second), now); err != nil {
			return err
		}
	}
	return s.refreshLease(ctx, tx, inst.ID, now)
}

// compensateShortfall 迟开通被封顶吃掉的 secs 秒按最新可退订单的单价退款；不足 1 分不退。
func (s *Service) compensateShortfall(ctx context.Context, tx pgx.Tx, inst Instance, secs int64, now time.Time) error {
	orders, err := refundableOrders(ctx, tx, []string{inst.ID}, true)
	if err != nil || len(orders) == 0 {
		return err
	}
	latest := orders[0]
	unit := seconds(latest.duration) * int64(max(latest.quantity, 1))
	amount := secs * latest.amount / unit
	if amount < 1 {
		return nil
	}
	if _, err := s.refundAcross(ctx, tx, inst.UserID, orders, amount, 0, "迟开通补偿 "+inst.ID, now); err != nil {
		return err
	}
	return recordEvent(ctx, tx, inst.ID, StateRunning, StateRunning, fmt.Sprintf("late start compensated: %ds refund %d", secs, amount), now)
}
```

（`results.go` import 加 `"fmt"`、`"spark-cloud/internal/platform/tz"`。）

- [ ] **Step 5: `provision.go`**

```go
package rental

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/tz"
)

// cancelFailure 标记「平台取消本身失败」（例如退款流水冲突）：ProvisionDue 据此在独立事务里告警，否则只有日志可见。
type cancelFailure struct{ err error }

func (c cancelFailure) Error() string { return "booking cancel failed: " + c.err.Error() }
func (c cancelFailure) Unwrap() error { return c.err }

// ProvisionDue 到点开通（规格 §5.2）：开始前 PrepLead 的预订实例逐个开通或改派；开始后仍无机器超过 BookingGrace 则平台取消。
// 返回值 n 是「本轮处理过的行」：开通、改派、取消，以及「无候选仍在宽限期内等待」的行都计入；测试按状态断言，不按 n。
func (s *Service) ProvisionDue(ctx context.Context) (int, error) {
	now := s.d.Clock.Now()
	return s.sweepEachAfter(ctx, `state = 'reserved' AND scheduled_start_at <= $1`, now.Add(PrepLead), "scheduled_start_at",
		func(tx pgx.Tx, inst Instance) (func(), error) { return s.provision(ctx, tx, inst, now) },
		func(id string, err error) {
			var cf cancelFailure
			if !errors.As(err, &cf) {
				return
			}
			s.d.Log.Error("booking cancel failed", "instance", id, "err", err)
			if aerr := s.d.Fleet.RaiseAlertOnce(ctx, s.d.Pool, fleet.Alert{Level: "critical", Kind: "booking_cancel_failed", InstanceID: id,
				Message: AlertBookingCancelFailed(id)}); aerr != nil {
				s.d.Log.Error("raise booking_cancel_failed", "instance", id, "err", aerr)
			}
		})
}

// provision 对一个到点的预订实例（调用方已锁实例）：复核租用资格 → 机器可用则开通，否则同机组改派；
// 都不行：宽限期内保持 reserved 并告警，过了宽限期平台取消。返回事务提交后要执行的动作（发短信）。
func (s *Service) provision(ctx context.Context, tx pgx.Tx, inst Instance, now time.Time) (func(), error) {
	if err := s.d.Users.CanRent(ctx, inst.UserID); err != nil {
		if apperr.As(err) == nil {
			return nil, err
		}
		if _, cerr := s.cancelPlatform(ctx, tx, inst, "cancelled: user cannot rent", now); cerr != nil {
			return nil, cancelFailure{cerr}
		}
		return s.smsCancelled(inst, "账号不可租用"), nil
	}
	lease, err := lockLease(ctx, tx, inst.ID)
	if err != nil {
		return nil, err
	}
	ready, err := s.machineReady(ctx, tx, inst.MachineID)
	if err != nil {
		return nil, err
	}
	if !ready {
		moved, err := s.reassign(ctx, tx, &inst, &lease, "machine unavailable at start", true, now)
		if err != nil {
			return nil, err
		}
		if !moved {
			if now.Before(inst.ScheduledStartAt.Add(BookingGrace)) {
				return nil, s.d.Fleet.RaiseAlertOnce(ctx, tx, fleet.Alert{Level: "warning", Kind: "booking_at_risk",
					MachineID: inst.MachineID, InstanceID: inst.ID, Message: AlertBookingAtRisk(inst.ID)})
			}
			if _, cerr := s.cancelPlatform(ctx, tx, inst, "unfulfilled: no machine by grace", now); cerr != nil {
				return nil, cancelFailure{cerr}
			}
			if err := s.d.Fleet.RaiseAlert(ctx, tx, fleet.Alert{Level: "critical", Kind: "booking_unfulfilled",
				MachineID: inst.MachineID, InstanceID: inst.ID, Message: AlertBookingUnfulfilled(inst.ID)}); err != nil {
				return nil, cancelFailure{err}
			}
			return s.smsCancelled(inst, "无法安排机器"), nil
		}
	}
	return nil, s.startProvision(ctx, tx, inst, lease, now)
}

func (s *Service) smsCancelled(inst Instance, reason string) func() {
	return func() {
		if err := s.d.Notifier.NotifyBookingCancelled(context.Background(), inst.UserID, inst.ID, reason); err != nil {
			s.d.Log.Warn("booking cancelled sms failed", "instance", inst.ID, "err", err)
		}
	}
}

// machineReady 锁机器行（NO KEY UPDATE：随后要插 agent_commands，FOR UPDATE 会与其外键 KEY SHARE 冲突）并判断能否现在开通：空闲、在线、没有活动实例。
func (s *Service) machineReady(ctx context.Context, tx pgx.Tx, machineID int64) (bool, error) {
	var state string
	var online bool
	err := tx.QueryRow(ctx, `SELECT state, agent_online FROM machines WHERE id = $1 FOR NO KEY UPDATE`, machineID).Scan(&state, &online)
	if db.IsNoRows(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if state != fleet.StateIdle || !online {
		return false, nil
	}
	var busy bool
	err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM instances WHERE machine_id = $1 AND state IN ('creating', 'running', 'stopped', 'wiping'))`, machineID).Scan(&busy)
	return !busy, err
}

// startProvision：机器 allocated、实例 creating（provisioned_at = now）、lease active、入队 Create（规格 §5.2 第 3 步）。
func (s *Service) startProvision(ctx context.Context, tx pgx.Tx, inst Instance, lease leaseRow, now time.Time) error {
	if _, err := s.d.Fleet.MarkAllocated(ctx, tx, inst.MachineID); err != nil {
		return err
	}
	ref, err := s.d.Catalog.ImageRef(ctx, tx, inst.ImageID)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE instances SET state = 'creating', provisioned_at = $2, updated_at = $2 WHERE id = $1`, inst.ID, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE machine_leases SET status = 'active', updated_at = $2 WHERE id = $1`, lease.ID, now); err != nil {
		return err
	}
	if err := recordEvent(ctx, tx, inst.ID, StateReserved, StateCreating, "scheduled start "+tz.Format(*inst.ScheduledStartAt), now); err != nil {
		return err
	}
	_, err = s.d.Fleet.Enqueue(ctx, tx, inst.MachineID, inst.ID, fleet.CmdCreateInstance, fleet.CreateInstancePayload{ImageRef: ref})
	return err
}
```

- [ ] **Step 6: `reassign.go`**

```go
package rental

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/fleet"
)

// reassign 把预订换到同机组（instances.group_id；nil 不限）另一台机器（规格 §5.4）：
// 新 lease 以 reserved 插入（同一 period）、旧 lease cancelled、实例换机器并 reassign_count+1、写事件。
// needOnline：开通时刻的改派要求 idle AND agent_online；后台提前改派只要求时段不冲突。
// 成功时就地更新 *inst 与 *lease；没有候选返回 false（不改任何数据）。
func (s *Service) reassign(ctx context.Context, tx pgx.Tx, inst *Instance, lease *leaseRow, reason string, needOnline bool, now time.Time) (bool, error) {
	spec := pickSpec{ProductID: inst.ProductID, GroupID: inst.GroupID, Lower: lease.Lower, Upper: lease.Upper,
		Immediate: needOnline, Exclude: []int64{inst.MachineID}}
	var newLease, newMachine int64
	_, err := placeOnMachines(ctx, tx, spec, 1, func(sp pgx.Tx, mid int64) error {
		id, err := insertLease(ctx, sp, mid, inst.ID, lease.Lower, lease.Upper, "reserved", now)
		if err != nil {
			return err
		}
		newLease, newMachine = id, mid
		return nil
	})
	if codeIs(err, "no_capacity") {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE machine_leases SET status = 'cancelled', updated_at = $2 WHERE id = $1`, lease.ID, now); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE instances SET machine_id = $2, reassign_count = reassign_count + 1, updated_at = $3 WHERE id = $1`,
		inst.ID, newMachine, now); err != nil {
		return false, err
	}
	if err := recordEvent(ctx, tx, inst.ID, inst.State, inst.State, fmt.Sprintf("reassigned %d -> %d: %s", inst.MachineID, newMachine, reason), now); err != nil {
		return false, err
	}
	inst.MachineID, inst.ReassignCount = newMachine, inst.ReassignCount+1
	lease.ID, lease.MachineID, lease.Status = newLease, newMachine, "reserved"
	return true, nil
}

// reassignAfterCreateFailure 预订实例 Create 失败 / 超时（规格 §5.4 末段）：机器 faulty + 告警 + 取消指令（与 failCreating 相同），
// 但不改 pending_seconds、不清主机密钥、不退款、lease 不置 ended。有候选 → 换机器继续 creating（主机密钥重生成）；
// 无候选且未过宽限期 → 回 reserved（lease 回 reserved，钉在故障机器上等下一轮）；过了宽限期 → 平台取消（短信经 *after 在提交后发）。
func (s *Service) reassignAfterCreateFailure(ctx context.Context, tx pgx.Tx, inst Instance, reason string, now time.Time, after *func()) error {
	old := inst.MachineID
	if err := s.d.Fleet.MarkFaulty(ctx, tx, old); err != nil {
		return err
	}
	if err := s.d.Fleet.CancelCommands(ctx, tx, inst.ID, "instance create failed"); err != nil {
		return err
	}
	if err := s.d.Fleet.RaiseAlert(ctx, tx, fleet.Alert{Level: "critical", Kind: "create_failed", MachineID: old, InstanceID: inst.ID, Message: reason}); err != nil {
		return err
	}
	lease, err := lockLease(ctx, tx, inst.ID)
	if err != nil {
		return err
	}
	moved, err := s.reassign(ctx, tx, &inst, &lease, "create failed", true, now)
	if err != nil {
		return err
	}
	if moved {
		hostPub, hostPEM, err := s.d.HostKeyGen()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE instances SET ssh_host_pubkey = $2, ssh_host_key_enc = $3, provisioned_at = $4, updated_at = $4 WHERE id = $1`,
			inst.ID, hostPub, s.d.Box.Seal(hostPEM), now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE machine_leases SET status = 'active', updated_at = $2 WHERE id = $1`, lease.ID, now); err != nil {
			return err
		}
		if _, err := s.d.Fleet.MarkAllocated(ctx, tx, inst.MachineID); err != nil {
			return err
		}
		ref, err := s.d.Catalog.ImageRef(ctx, tx, inst.ImageID)
		if err != nil {
			return err
		}
		_, err = s.d.Fleet.Enqueue(ctx, tx, inst.MachineID, inst.ID, fleet.CmdCreateInstance, fleet.CreateInstancePayload{ImageRef: ref})
		return err
	}
	if now.Before(inst.ScheduledStartAt.Add(BookingGrace)) {
		if _, err := tx.Exec(ctx, `UPDATE instances SET state = 'reserved', provisioned_at = NULL, updated_at = $2 WHERE id = $1`, inst.ID, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE machine_leases SET status = 'reserved', updated_at = $2 WHERE id = $1`, lease.ID, now); err != nil {
			return err
		}
		if err := recordEvent(ctx, tx, inst.ID, StateCreating, StateReserved, "create failed, waiting for machine", now); err != nil {
			return err
		}
		return s.d.Fleet.RaiseAlertOnce(ctx, tx, fleet.Alert{Level: "warning", Kind: "booking_at_risk", MachineID: old, InstanceID: inst.ID, Message: AlertBookingAtRisk(inst.ID)})
	}
	if _, err := s.cancelPlatform(ctx, tx, inst, "unfulfilled: create failed past grace", now); err != nil {
		return err
	}
	if err := s.d.Fleet.RaiseAlert(ctx, tx, fleet.Alert{Level: "critical", Kind: "booking_unfulfilled", MachineID: old, InstanceID: inst.ID, Message: AlertBookingUnfulfilled(inst.ID)}); err != nil {
		return err
	}
	*after = s.smsCancelled(inst, "无法安排机器")
	return nil
}
```

- [ ] **Step 7: 运行测试确认通过**

Run: `go vet ./... && go test ./internal/rental/ ./internal/app/ 2>&1 | tail -6`
Expected: 全部 `ok`。`TestCreatingTimeoutRefundsAndIgnoresLateAck`（即时订单）行为不变。

- [ ] **Step 8: 提交**

```bash
git add internal/rental/provision.go internal/rental/reassign.go internal/rental/provision_test.go internal/rental/sweeper.go internal/rental/results.go internal/rental/service.go
git commit -m "feat(booking): ProvisionDue with reassignment and grace cancel, booked start timing with shortfall compensation, create-failure reassignment

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 9: 续费冲突 `renewFits`、`renewable_until` / `renew_options`，接入 `RenewQuantity` 与 `AdminExtend`

**Files:**
- Modify: `internal/rental/renew.go`（`renewBase`、`renewInfo`、`RenewInfoFor`、`renewFits`、`RenewQuantity` / `addTime` 接入）、`internal/rental/admin_ops.go`（`AdminExtend` 接入）
- Test: `internal/rental/renew_test.go`（追加）

**Interfaces:**
- Consumes: Task 2 `nextReservedLower`、`tz`；Task 5 `RenewQuantity`、`MaxQuantity`。
- Produces:
  - `rental.RenewOption{Duration catalog.Duration \`json:"duration"\`; QuantityMax int \`json:"quantity_max"\`}`；`rental.RenewInfo{RenewableUntil *time.Time \`json:"renewable_until"\`; Options []RenewOption \`json:"renew_options"\`}`。
  - `(*Service).RenewInfoFor(ctx, inst Instance) (RenewInfo, error)`（只读；API 视图用）。
  - `renewBase(inst, now) time.Time`（规格 §7.1 表的「新上界 − secs − Tail」）；`renewFits(ctx, q, inst, secs, now) error`（不满足返回 `renew_blocked`，消息「该机器自 <cap + PrepLead> 起已有预订，本次最多可续至 <cap − Tail>」，时间按 `tz.Format`）。
  - `RenewQuantity`、`AdminExtend`、停机续费（同一条路径）在扣款前先 `renewFits`，并在 `refreshLease` 之后 `verifyLeaseFits`（lease 上界被并发提交的预订封顶 → `errRenewBlockedGeneric`，事务回滚不扣款）；`addTime` 的 reserved 分支若仍撞排他约束，映射为 `renew_blocked`（通用消息「该机器已有后续预订，无法续费」）。`renewHook`（测试钩子）与 `SetRenewHookForTest`。

- [ ] **Step 1: 写失败的测试**

`internal/rental/renew_test.go` 追加：

```go
func TestRenewInfoReflectsNextReservation(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true) // expires = T0 + 24h
	info, err := e.svc.RenewInfoFor(e.ctx, e.get(inst.ID))
	if err != nil || info.RenewableUntil != nil || len(info.Options) != 3 || info.Options[0].QuantityMax != 90 || info.Options[1].QuantityMax != 12 || info.Options[2].QuantityMax != 3 {
		t.Fatalf("no reservation info = %+v, %v", info, err)
	}
	e.reserveRaw(e.machines[0], dbtest.T0.Add(72*time.Hour), 86400) // 下一预订 S = T0 + 3d → cap = S − 1h，可续至 = S − 8h = T0 + 64h
	info, _ = e.svc.RenewInfoFor(e.ctx, e.get(inst.ID))
	until := dbtest.T0.Add(64 * time.Hour)
	if info.RenewableUntil == nil || !info.RenewableUntil.Equal(until) {
		t.Fatalf("renewable_until = %v, want %v", info.RenewableUntil, until)
	}
	// base = T0 + 24h，余量 40h：1d 最多 1、1w / 1m 为 0
	if info.Options[0].QuantityMax != 1 || info.Options[1].QuantityMax != 0 || info.Options[2].QuantityMax != 0 {
		t.Fatalf("options = %+v", info.Options)
	}
}

func TestRenewBlockedByReservationDoesNotCharge(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	e.reserveRaw(e.machines[0], dbtest.T0.Add(72*time.Hour), 86400)
	balance := e.balance(e.userID)
	_, _, upperBefore := e.lease(inst.ID)
	_, err := e.svc.Renew(e.ctx, e.userID, inst.ID, catalog.D1w, "")
	ae := apperr.As(err)
	if ae == nil || ae.Code != "renew_blocked" || ae.Message != "该机器自 2026-01-04 08:00 起已有预订，本次最多可续至 2026-01-04 00:00" {
		t.Fatalf("renew 1w = %v", err)
	}
	got := e.get(inst.ID)
	if e.balance(e.userID) != balance || !got.ExpiresAt.Equal(dbtest.T0.Add(24*time.Hour)) {
		t.Fatal("blocked renewal must not charge or extend")
	}
	if _, _, upper := e.lease(inst.ID); upper != upperBefore {
		t.Fatal("lease must not change")
	}
	// 后台延长 7 天同样被挡；1 天可以
	if _, err := e.svc.AdminExtend(e.ctx, opsActor, inst.ID, 7*86400, "补偿"); apperr.As(err) == nil || apperr.As(err).Code != "renew_blocked" {
		t.Fatalf("extend 7d = %v", err)
	}
	if _, err := e.svc.Renew(e.ctx, e.userID, inst.ID, catalog.D1d, ""); err != nil {
		t.Fatalf("renew 1d = %v", err)
	}
	if !e.get(inst.ID).ExpiresAt.Equal(dbtest.T0.Add(48 * time.Hour)) {
		t.Fatal("1d renewal must extend")
	}
	// 停机期续费：base = now + 20m + pending；停机在 T0 + 48h，续 2d → T0 + 96h20m > 可续至 T0 + 64h → renew_blocked
	e.clock.Advance(48 * time.Hour)
	if _, err := e.svc.ExpireDue(e.ctx); err != nil {
		t.Fatal(err)
	}
	e.ack(true)
	if _, err := e.svc.RenewQuantity(e.ctx, e.userID, inst.ID, catalog.D1d, 2, ""); apperr.As(err) == nil || apperr.As(err).Code != "renew_blocked" {
		t.Fatalf("stopped renew 2d = %v", err)
	}
	if e.get(inst.ID).PendingSeconds != 0 {
		t.Fatal("blocked stopped renewal must not add pending time")
	}
}

// 评审 M2：renewFits 通过之后、本事务提交之前，另一连接提交了同机紧随的预订 → 必须 renew_blocked 且不扣款。
func TestRenewRaceWithNewReservationIsBlocked(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true) // expires = T0 + 24h，lease 上界 T0 + 31h
	restore := rental.SetRenewHookForTest(func() {
		e.reserveRaw(e.machines[0], dbtest.T0.Add(32*time.Hour), 86400) // 下界 T0 + 31h：紧贴当前 lease，续 1d 就越界
	})
	defer restore()
	balance := e.balance(e.userID)
	_, err := e.svc.Renew(e.ctx, e.userID, inst.ID, catalog.D1d, "")
	if ae := apperr.As(err); ae == nil || ae.Code != "renew_blocked" {
		t.Fatalf("want renew_blocked, got %v", err)
	}
	got := e.get(inst.ID)
	if e.balance(e.userID) != balance || !got.ExpiresAt.Equal(dbtest.T0.Add(24*time.Hour)) {
		t.Fatalf("race must roll back charge and expiry: balance=%d expires=%v", e.balance(e.userID), got.ExpiresAt)
	}
	if _, upper, _ := e.lease(inst.ID); !upper.Equal(dbtest.T0.Add(31 * time.Hour)) {
		t.Fatalf("lease upper = %v", upper)
	}
	restore()
	if _, err := e.svc.Renew(e.ctx, e.userID, inst.ID, catalog.D1d, ""); apperr.As(err) == nil || apperr.As(err).Code != "renew_blocked" {
		t.Fatalf("after the reservation exists renewFits itself must block: %v", err)
	}
}

func TestRenewReservedExtendsLeaseUnlessBlocked(t *testing.T) {
	e := newEnv(t, 1)
	first := e.book(dbtest.T0.Add(3 * time.Hour)) // lease [T0+2h, T0+34h)
	next := e.reserveRaw(e.machines[0], dbtest.T0.Add(35*time.Hour), 3600) // lease 下界 T0 + 34h：紧随其后
	if _, err := e.svc.Renew(e.ctx, e.userID, first.ID, catalog.D1d, ""); apperr.As(err) == nil || apperr.As(err).Code != "renew_blocked" {
		t.Fatalf("reserved renew blocked = %v", err)
	}
	if _, err := e.pool.Exec(e.ctx, `UPDATE machine_leases SET status = 'cancelled' WHERE instance_id = $1`, next); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Renew(e.ctx, e.userID, first.ID, catalog.D1d, ""); err != nil {
		t.Fatalf("reserved renew after cancel = %v", err)
	}
	if got := e.get(first.ID); got.PendingSeconds != 2*86400 {
		t.Fatalf("pending = %d", got.PendingSeconds)
	}
	if _, upper, _ := e.lease(first.ID); !upper.Equal(dbtest.T0.Add(3*time.Hour + 48*time.Hour + rental.Tail)) {
		t.Fatalf("lease upper = %v", upper)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/rental/ -run 'TestRenewInfo|TestRenewBlocked|TestRenewReserved' 2>&1 | head -20`
Expected: 编译错误（`RenewInfoFor` 未定义）。

- [ ] **Step 3: `renew.go` 新增**

在 `renew.go` 末尾加：

```go
// RenewOption 续费表单的一个档位：在可续时间与 90 天上限下最多能买的单位数（0 = 不可选）。
type RenewOption struct {
	Duration    catalog.Duration `json:"duration"`
	QuantityMax int              `json:"quantity_max"`
}

// RenewInfo 实例视图里的续费约束（规格 §7.1）。
type RenewInfo struct {
	RenewableUntil *time.Time    `json:"renewable_until"` // 同机下一条预订的 cap − Tail；无预订为 nil
	Options        []RenewOption `json:"renew_options"`
}

// renewBase 加时前的到期基准：新上界 = base + secs + Tail（规格 §7.1 表）。
func renewBase(inst Instance, now time.Time) time.Time {
	pending := time.Duration(inst.PendingSeconds) * time.Second
	switch {
	case inst.State == StateRunning && inst.Suspended():
		var paused int64
		if inst.PausedRemaining != nil {
			paused = *inst.PausedRemaining
		}
		return now.Add(time.Duration(paused) * time.Second)
	case inst.State == StateRunning:
		if inst.ExpiresAt != nil && inst.ExpiresAt.After(now) {
			return *inst.ExpiresAt
		}
		return now
	case inst.Booked() && (inst.State == StateReserved || inst.State == StateCreating):
		return inst.ScheduledStartAt.Add(pending)
	default: // 即时 creating / stopped：与 occupiedUntil 的估算一致
		return now.Add(CreateTimeout + pending)
	}
}

// renewInfo 读取同机下一条预订并算出各档位最多能买的单位数（不加锁；q 可以是事务）。
func (s *Service) renewInfo(ctx context.Context, q db.Querier, inst Instance, now time.Time) (RenewInfo, error) {
	var lower *time.Time
	if err := q.QueryRow(ctx, `SELECT lower(period) FROM machine_leases WHERE instance_id = $1 AND status IN ('reserved', 'active') ORDER BY id DESC LIMIT 1`, inst.ID).Scan(&lower); err != nil && !db.IsNoRows(err) {
		return RenewInfo{}, err
	}
	info := RenewInfo{}
	if lower != nil {
		cap, ok, err := nextReservedLower(ctx, q, inst.MachineID, inst.ID, *lower)
		if err != nil {
			return RenewInfo{}, err
		}
		if ok {
			until := cap.Add(-Tail)
			info.RenewableUntil = &until
		}
	}
	base := renewBase(inst, now)
	for _, d := range []catalog.Duration{catalog.D1d, catalog.D1w, catalog.D1m} {
		max := MaxQuantity(d)
		if info.RenewableUntil != nil {
			room := info.RenewableUntil.Sub(base)
			if room < d.Length() {
				max = 0
			} else if n := int(room / d.Length()); n < max {
				max = n
			}
		}
		info.Options = append(info.Options, RenewOption{Duration: d, QuantityMax: max})
	}
	return info, nil
}

// RenewInfoFor 实例视图用。
func (s *Service) RenewInfoFor(ctx context.Context, inst Instance) (RenewInfo, error) {
	return s.renewInfo(ctx, s.d.Pool, inst, s.d.Clock.Now())
}

// renewFits 所有加时路径共用：加 secs 秒后的到期不得越过同机下一条预订（规格 §7.1）。
func (s *Service) renewFits(ctx context.Context, q db.Querier, inst Instance, secs int64, now time.Time) error {
	info, err := s.renewInfo(ctx, q, inst, now)
	if err != nil {
		return err
	}
	if info.RenewableUntil == nil || !renewBase(inst, now).Add(time.Duration(secs)*time.Second).After(*info.RenewableUntil) {
		return nil
	}
	nextStart := info.RenewableUntil.Add(Tail + PrepLead)
	return apperr.New("renew_blocked", fmt.Sprintf("该机器自 %s 起已有预订，本次最多可续至 %s", tz.Format(nextStart), tz.Format(*info.RenewableUntil)), 409)
}

var errRenewBlockedGeneric = apperr.New("renew_blocked", "该机器已有后续预订，无法续费", 409)
```

（`renew.go` import 加 `"fmt"`、`"spark-cloud/internal/platform/tz"`；`db` 已经导入，不要重复加。）

`RenewQuantity` 在状态检查之后、`charge` 之前插入（`renewHook` 是只给测试用的钩子，用来在 `renewFits` 通过之后、扣款之前制造并发预订）：

```go
		if err := s.renewFits(ctx, tx, inst, secs, now); err != nil {
			return err
		}
		if renewHook != nil {
			renewHook()
		}
```

**竞态复核（评审 M2）**：`renewFits` 只读 cap、不锁任何行；若另一事务在它之后、本事务提交之前提交了同机紧随的预订，`refreshLease` 会把 lease 上界封顶而不是报错，结果是扣了款、`expires_at` 却越过了已付的预订。所以三条加时路径（`RenewQuantity`、`AdminExtend`、`autoRenewOne`）在 `refreshLease` **之后**都再做一次复核：lease 上界必须等于按新状态算出的 `occupiedUntil`，否则返回 `errRenewBlockedGeneric`，整个事务（含扣款）回滚。`renew.go` 追加：

```go
// renewHook 测试钩子：在 renewFits 通过后、扣款前调用（制造「并发预订刚提交」的竞态）；生产为 nil。
var renewHook func()

// verifyLeaseFits 加时并 refreshLease 之后复核：lease 上界若被同机新提交的预订封顶（小于按新状态算出的 occupiedUntil），
// 说明 renewFits 读到的 cap 已过期——返回 renew_blocked 让事务回滚，不能既扣款又让到期越过别人已付的预订。
func (s *Service) verifyLeaseFits(ctx context.Context, tx pgx.Tx, id string, now time.Time) error {
	inst, err := lockInstance(ctx, tx, id)
	if err != nil {
		return err
	}
	if inst.Booked() && inst.State == StateReserved { // reserved 的 lease 由 addTime 直接推后，撞约束时已经报错
		return nil
	}
	var upper time.Time
	if err := tx.QueryRow(ctx, `SELECT upper(period) FROM machine_leases WHERE instance_id = $1 AND status = 'active' ORDER BY id DESC LIMIT 1`, id).Scan(&upper); err != nil {
		return err
	}
	if upper.Before(occupiedUntil(inst, now)) {
		return errRenewBlockedGeneric
	}
	return nil
}
```

`internal/rental/export_test.go` 追加：

```go
// SetRenewHookForTest 设置 renewFits 之后的钩子，返回恢复函数。
func SetRenewHookForTest(f func()) func() {
	renewHook = f
	return func() { renewHook = nil }
}
```

`addTime` 的 reserved 分支末尾：`if db.IsExclusionViolation(err) { return errRenewBlockedGeneric }`。

`internal/rental/admin_ops.go` 的 `AdminExtend` 在 `lockInstance` 之后、`addTime` 之前插入：

```go
		if err := s.renewFits(ctx, tx, inst, seconds, now); err != nil {
			return err
		}
```

并把它原来的 `if err := s.refreshLease(ctx, tx, inst.ID, now); err != nil { return err }` 之后、审计之前加一行 `if err := s.verifyLeaseFits(ctx, tx, inst.ID, now); err != nil { return err }`。

- [ ] **Step 4: 运行测试确认通过**

Run: `go vet ./... && go test ./internal/rental/ 2>&1 | tail -4`
Expected: `ok`。

- [ ] **Step 5: 提交**

```bash
git add internal/rental/renew.go internal/rental/admin_ops.go internal/rental/renew_test.go internal/rental/export_test.go
git commit -m "feat(booking): renewFits blocks extensions past the next reservation; renewable_until and renew options

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 10: 自动续费（`SetAutoRenew`、`AutoRenewDue`、降档、失败标记与短信）

**Files:**
- Create: `internal/rental/autorenew.go`、`internal/rental/autorenew_test.go`

**Interfaces:**
- Consumes: Task 9 `renewFits`；Task 7 `Notifier.NotifyAutoRenewFailed`；Task 8 `sweepEachAfter`；Task 5 `charge`。
- Produces:
  - `(*Service).SetAutoRenew(ctx, userID int64, id string, enabled bool, d catalog.Duration, ip string) (Instance, error)`：允许 `reserved / creating / running / stopped`；清失败标记；审计 `instance.auto_renew`（user actor，diff `{enabled, duration}`）。
  - `(*Service).AutoRenewDue(ctx) (int, error)`：谓词见 Global Constraints；每行：savepoint 内选档位（`1m → 1w → 1d` 从配置档位起、只降不升）→ `charge(kind='renew', auto=true, quantity=1)` → `addTime` → 事件 `"auto renew <order> (<档位>)"` → `refreshLease` → 清失败标记；余额不足 / 全部被挡 → 回滚 savepoint，在行事务里写 `auto_renew_failed_at / reason`，首次失败写事件 `"auto renew failed: <reason>"` 并在提交后发短信（reason 文案「余额不足」/「机器已有后续预订」）。
  - 失败标记写在行事务里而不是「独立小事务」：savepoint 回滚了扣款尝试，行事务本身提交；效果等价且不多开连接（裁定，写入 followups）。

- [ ] **Step 1: 写失败的测试**

`internal/rental/autorenew_test.go`：

```go
package rental_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/rental"
)

func (e *env) autoRenewOn(id string, d catalog.Duration) {
	e.t.Helper()
	if _, err := e.svc.SetAutoRenew(e.ctx, e.userID, id, true, d, "1.1.1.1"); err != nil {
		e.t.Fatalf("SetAutoRenew: %v", err)
	}
}

func TestAutoRenewChargesOneHourBeforeExpiry(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true)
	e.autoRenewOn(inst.ID, catalog.D1d)
	if got := e.get(inst.ID); !got.AutoRenew || got.AutoRenewDuration != catalog.D1d || e.auditCountOf("instance.auto_renew") != 1 {
		t.Fatalf("after switch = %+v", got)
	}
	e.clock.Advance(22*time.Hour + 59*time.Minute)
	n, err := e.svc.AutoRenewDue(e.ctx)
	mustCount(t, n, err, 0, "before lead")
	e.clock.Advance(time.Minute)
	n, err = e.svc.AutoRenewDue(e.ctx)
	mustCount(t, n, err, 1, "at lead")
	got := e.get(inst.ID)
	if !got.ExpiresAt.Equal(dbtest.T0.Add(48*time.Hour)) || got.AutoRenewFailedAt != nil || got.AutoRenewFailedReason != "" {
		t.Fatalf("after auto renew = %+v", got)
	}
	orders, _ := e.svc.ListOrders(e.ctx, e.userID, 10)
	if len(orders) != 2 || orders[0].Kind != "renew" || !orders[0].Auto || orders[0].Amount != 6900 || orders[0].Quantity != 1 {
		t.Fatalf("orders = %+v", orders)
	}
	if ev := e.lastEvent(inst.ID); ev.Reason != "auto renew "+orders[0].ID+" (1d)" {
		t.Fatalf("event = %+v", ev)
	}
	if _, upper, _ := e.lease(inst.ID); !upper.Equal(dbtest.T0.Add(48*time.Hour + rental.Tail)) {
		t.Fatalf("lease upper = %v", upper)
	}
	n, err = e.svc.AutoRenewDue(e.ctx)
	mustCount(t, n, err, 0, "not due again")
	// 到期扫描不会停机（到期已推后）
	e.clock.Advance(time.Hour)
	if n, _ := e.svc.ExpireDue(e.ctx); n != 0 {
		t.Fatal("must not expire after auto renew")
	}
}

func TestAutoRenewInsufficientBalanceRetriesAndNotifiesOnce(t *testing.T) {
	e := newEnv(t, 1)
	poor := e.newUser("13600000002", 7000)
	e.userID = poor
	_, inst := e.place(catalog.D1d) // 余额剩 100
	e.ack(true)
	e.autoRenewOn(inst.ID, catalog.D1d)
	e.clock.Advance(23 * time.Hour)
	if _, err := e.svc.AutoRenewDue(e.ctx); err != nil {
		t.Fatal(err)
	}
	got := e.get(inst.ID)
	if got.AutoRenewFailedAt == nil || !got.AutoRenewFailedAt.Equal(e.clock.Now()) || got.AutoRenewFailedReason != "insufficient_balance" || !got.ExpiresAt.Equal(dbtest.T0.Add(24*time.Hour)) {
		t.Fatalf("after failure = %+v", got)
	}
	sms := func() int {
		n := 0
		for _, c := range e.notifier.Calls() {
			if strings.HasPrefix(c, "auto_renew_failed:"+inst.ID+":余额不足") {
				n++
			}
		}
		return n
	}
	if sms() != 1 || e.lastEvent(inst.ID).Reason != "auto renew failed: insufficient_balance" {
		t.Fatalf("sms = %d, event = %+v", sms(), e.lastEvent(inst.ID))
	}
	e.clock.Advance(30 * time.Second)
	n, err := e.svc.AutoRenewDue(e.ctx)
	mustCount(t, n, err, 0, "retry interval not elapsed")
	e.clock.Advance(31 * time.Second)
	n, err = e.svc.AutoRenewDue(e.ctx)
	mustCount(t, n, err, 1, "retried")
	if sms() != 1 || e.balance(poor) != 100 {
		t.Fatalf("repeat failure must not resend sms or charge: sms=%d balance=%d", sms(), e.balance(poor))
	}
	dbtest.Fund(t, e.pool, poor, 7000)
	e.clock.Advance(time.Minute)
	n, err = e.svc.AutoRenewDue(e.ctx)
	mustCount(t, n, err, 1, "after top-up")
	got = e.get(inst.ID)
	if got.AutoRenewFailedAt != nil || got.AutoRenewFailedReason != "" || !got.ExpiresAt.Equal(dbtest.T0.Add(48*time.Hour)) || e.balance(poor) != 200 {
		t.Fatalf("after success = %+v balance=%d", got, e.balance(poor))
	}
}

func TestAutoRenewFallsBackThenBlocks(t *testing.T) {
	e := newEnv(t, 1)
	_, inst := e.place(catalog.D1d)
	e.ack(true) // expires T0 + 24h
	e.autoRenewOn(inst.ID, catalog.D1m)
	// 下一预订：可续至 = T0 + 24h + 8d → cap = +7h → S = cap + 1h = T0 + 24h + 8d + 8h
	e.reserveRaw(e.machines[0], dbtest.T0.Add(24*time.Hour+8*24*time.Hour+8*time.Hour), 3600)
	e.clock.Advance(23 * time.Hour)
	balance := e.balance(e.userID)
	if n, err := e.svc.AutoRenewDue(e.ctx); err != nil || n != 1 {
		t.Fatalf("first = %d, %v", n, err)
	}
	got := e.get(inst.ID)
	if !got.ExpiresAt.Equal(dbtest.T0.Add(24*time.Hour+7*24*time.Hour)) || e.balance(e.userID) != balance-41400 {
		t.Fatalf("expected fallback to 1w: %+v balance=%d", got, e.balance(e.userID))
	}
	if !strings.HasSuffix(e.lastEvent(inst.ID).Reason, "(1w)") {
		t.Fatalf("event = %+v", e.lastEvent(inst.ID))
	}
	// 再过 7 天：余量 1 天 → 降到 1d
	e.clock.Advance(7 * 24 * time.Hour)
	balance = e.balance(e.userID)
	if n, err := e.svc.AutoRenewDue(e.ctx); err != nil || n != 1 {
		t.Fatalf("second = %d, %v", n, err)
	}
	if got := e.get(inst.ID); !got.ExpiresAt.Equal(dbtest.T0.Add(24*time.Hour+8*24*time.Hour)) || e.balance(e.userID) != balance-6900 {
		t.Fatalf("expected fallback to 1d: %+v", got)
	}
	// 再过 1 天：余量 0 → renew_blocked，失败标记 + 短信；10 分钟内不重试；手动续费同样被挡且不扣款
	e.clock.Advance(24 * time.Hour)
	balance = e.balance(e.userID)
	if _, err := e.svc.AutoRenewDue(e.ctx); err != nil {
		t.Fatal(err)
	}
	got = e.get(inst.ID)
	if got.AutoRenewFailedReason != "renew_blocked" || e.balance(e.userID) != balance || !slices.Contains(e.notifier.Calls(), "auto_renew_failed:"+inst.ID+":机器已有后续预订") {
		t.Fatalf("blocked = %+v calls=%v", got, e.notifier.Calls())
	}
	e.clock.Advance(5 * time.Minute)
	n, err := e.svc.AutoRenewDue(e.ctx)
	mustCount(t, n, err, 0, "blocked retry within 10m")
	e.clock.Advance(6 * time.Minute)
	n, err = e.svc.AutoRenewDue(e.ctx)
	mustCount(t, n, err, 1, "blocked retry after 10m")
	if _, err := e.svc.Renew(e.ctx, e.userID, inst.ID, catalog.D1d, ""); err == nil || e.balance(e.userID) != balance {
		t.Fatalf("manual renew must be blocked without charge: %v", err)
	}
}

func TestAutoRenewSkipsStoppedAndSuspended(t *testing.T) {
	e := newEnv(t, 2)
	_, a := e.place(catalog.D1d)
	_, b := e.place(catalog.D1d)
	e.ack(true)
	e.autoRenewOn(a.ID, catalog.D1d)
	e.autoRenewOn(b.ID, catalog.D1d)
	// 到期前 1h：b 所在机器失联 → b 暂停（running + suspended_at，剩余 1h 冻结）；a 暂时关掉自动续费免得被续
	if _, err := e.pool.Exec(e.ctx, `UPDATE instances SET auto_renew = false WHERE id = $1`, a.ID); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(23 * time.Hour)
	if _, err := e.svc.SuspendMachines(e.ctx, []int64{b.MachineID}); err != nil {
		t.Fatal(err)
	}
	if got := e.get(b.ID); got.State != rental.StateRunning || got.SuspendedAt == nil {
		t.Fatalf("b must be suspended: %+v", got)
	}
	n, err := e.svc.AutoRenewDue(e.ctx)
	mustCount(t, n, err, 0, "suspended skipped")
	if got := e.get(b.ID); !got.ExpiresAt.Equal(dbtest.T0.Add(24 * time.Hour)) {
		t.Fatalf("suspended instance must not be auto-renewed: %+v", got)
	}
	// 到期：a 停机（b 暂停中不被判到期）；再打开 a 的自动续费也不会续
	e.clock.Advance(time.Hour)
	if n, err := e.svc.ExpireDue(e.ctx); err != nil || n != 1 {
		t.Fatalf("expire = %d, %v", n, err)
	}
	if _, err := e.pool.Exec(e.ctx, `UPDATE instances SET auto_renew = true WHERE id = $1`, a.ID); err != nil {
		t.Fatal(err)
	}
	n, err = e.svc.AutoRenewDue(e.ctx)
	mustCount(t, n, err, 0, "stopped skipped")
	if e.get(a.ID).State != rental.StateStopped {
		t.Fatal("stopped instance must not be auto-renewed")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/rental/ -run 'TestAutoRenew' 2>&1 | head -20`
Expected: 编译错误（`SetAutoRenew`、`AutoRenewDue` 未定义）。

- [ ] **Step 3: `autorenew.go`**

```go
package rental

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
)

// SetAutoRenew 用户开关自动续费并设档位；开关时清掉上次的失败标记。
func (s *Service) SetAutoRenew(ctx context.Context, userID int64, id string, enabled bool, d catalog.Duration, ip string) (Instance, error) {
	if !d.Valid() {
		return Instance{}, apperr.BadRequest("自动续费档位不正确")
	}
	now := s.d.Clock.Now()
	var out Instance
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		inst, err := lockInstance(ctx, tx, id)
		if err != nil {
			return err
		}
		if inst.UserID != userID {
			return apperr.ErrNotFound
		}
		switch inst.State {
		case StateReserved, StateCreating, StateRunning, StateStopped:
		default:
			return apperr.ErrInvalidState
		}
		out, err = scanInstance(tx.QueryRow(ctx, `UPDATE instances SET auto_renew = $2, auto_renew_duration = $3,
			auto_renew_failed_at = NULL, auto_renew_failed_reason = '', updated_at = $4 WHERE id = $1 RETURNING `+instanceCols, id, enabled, string(d), now))
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, audit.Actor{UserID: userID, IP: ip}.Entry("instance.auto_renew", "instance", id,
			map[string]any{"enabled": enabled, "duration": d}))
	})
	return out, err
}

// autoRenewPred：到期前 AutoRenewLead 内、运行中未暂停、开了自动续费；失败后按原因间隔重试（$1 = now；间隔来自常量）。
var autoRenewPred = fmt.Sprintf(`state = 'running' AND suspended_at IS NULL AND auto_renew AND expires_at <= $1 + interval '%d seconds'
	AND (auto_renew_failed_at IS NULL OR auto_renew_failed_at <= $1
	     - CASE WHEN auto_renew_failed_reason = 'renew_blocked' THEN interval '%d seconds' ELSE interval '%d seconds' END)`,
	int(AutoRenewLead/time.Second), int(AutoRenewRetryBlocked/time.Second), int(AutoRenewRetry/time.Second))

// AutoRenewDue 自动续费（规格 §7.2）：到期前 AutoRenewLead 扣款并延长；余额不足或被预订挡住时记失败标记，首次失败发短信。
func (s *Service) AutoRenewDue(ctx context.Context) (int, error) {
	now := s.d.Clock.Now()
	return s.sweepEachAfter(ctx, autoRenewPred, now, "expires_at",
		func(tx pgx.Tx, inst Instance) (func(), error) { return s.autoRenewOne(ctx, tx, inst, now) }, nil)
}

// autoRenewOne：扣款与加时在 savepoint 里做，业务失败只回滚 savepoint，失败标记随行事务提交。
func (s *Service) autoRenewOne(ctx context.Context, tx pgx.Tx, inst Instance, now time.Time) (func(), error) {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	d, err := s.autoRenewDuration(ctx, sp, inst, now)
	if err != nil {
		_ = sp.Rollback(ctx)
		return nil, err
	}
	failReason := ""
	var ord Order
	if d == "" {
		failReason = "renew_blocked"
	} else if ord, err = s.charge(ctx, sp, inst.UserID, "renew", inst.ID, inst.ProductID, d, 1, true, nil, "", now); errors.Is(err, apperr.ErrInsufficientBalance) {
		failReason = "insufficient_balance"
	} else if err != nil {
		_ = sp.Rollback(ctx)
		return nil, err
	}
	if failReason != "" {
		_ = sp.Rollback(ctx)
		return s.markAutoRenewFailed(ctx, tx, inst, failReason, now)
	}
	steps := []func() error{
		func() error { return s.addTime(ctx, sp, inst, seconds(d), now) },
		func() error { return recordEvent(ctx, sp, inst.ID, StateRunning, StateRunning, fmt.Sprintf("auto renew %s (%s)", ord.ID, d), now) },
		func() error { return s.refreshLease(ctx, sp, inst.ID, now) },
		func() error { return s.verifyLeaseFits(ctx, sp, inst.ID, now) }, // 评审 M2：并发预订刚提交时不能既扣款又越过它
		func() error {
			_, err := sp.Exec(ctx, `UPDATE instances SET auto_renew_failed_at = NULL, auto_renew_failed_reason = '', updated_at = $2 WHERE id = $1`, inst.ID, now)
			return err
		},
	}
	for _, step := range steps {
		if err := step(); err != nil {
			_ = sp.Rollback(ctx)
			return nil, err
		}
	}
	return nil, sp.Commit(ctx)
}

// autoRenewDuration 从配置档位起按 1m → 1w → 1d 找第一个放得下的档位（只降不升）；都放不下返回空串。
func (s *Service) autoRenewDuration(ctx context.Context, q db.Querier, inst Instance, now time.Time) (catalog.Duration, error) {
	order := []catalog.Duration{catalog.D1m, catalog.D1w, catalog.D1d}
	start := slices.Index(order, inst.AutoRenewDuration)
	if start < 0 {
		start = 0
	}
	for _, d := range order[start:] {
		err := s.renewFits(ctx, q, inst, seconds(d), now)
		if err == nil {
			return d, nil
		}
		if !codeIs(err, "renew_blocked") {
			return "", err
		}
	}
	return "", nil
}

var autoRenewFailText = map[string]string{"insufficient_balance": "余额不足", "renew_blocked": "机器已有后续预订"}

// markAutoRenewFailed 写失败标记；首次失败（标记原为空）写事件并在提交后发短信。
func (s *Service) markAutoRenewFailed(ctx context.Context, tx pgx.Tx, inst Instance, reason string, now time.Time) (func(), error) {
	var first bool
	if err := tx.QueryRow(ctx, `WITH old AS (SELECT auto_renew_failed_at FROM instances WHERE id = $1)
		UPDATE instances SET auto_renew_failed_at = $2, auto_renew_failed_reason = $3, updated_at = $2 FROM old WHERE instances.id = $1
		RETURNING old.auto_renew_failed_at IS NULL`, inst.ID, now, reason).Scan(&first); err != nil {
		return nil, err
	}
	if !first {
		return nil, nil
	}
	if err := recordEvent(ctx, tx, inst.ID, StateRunning, StateRunning, "auto renew failed: "+reason, now); err != nil {
		return nil, err
	}
	text := autoRenewFailText[reason]
	return func() {
		if err := s.d.Notifier.NotifyAutoRenewFailed(context.Background(), inst.UserID, inst.ID, text); err != nil {
			s.d.Log.Warn("auto renew failed sms", "instance", inst.ID, "err", err)
		}
	}, nil
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go vet ./... && go test ./internal/rental/ 2>&1 | tail -4`
Expected: `ok`。

- [ ] **Step 5: 提交**

```bash
git add internal/rental/autorenew.go internal/rental/autorenew_test.go
git commit -m "feat(booking): auto-renew sweep with tier fallback, failure markers and one-time sms

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 11: 可用性与日历查询（机组日历、机器日历、机组机器列表、`Availability`）

**Files:**
- Create: `internal/rental/calendar.go`、`internal/rental/calendar_test.go`

**Interfaces:**
- Consumes: Task 4 `pickSpec` / `countAvailable` / `immediatePeriod` / `bookingPeriod`；Task 2 `tz`；`fleet.Group` / `GetGroup`。
- Produces:
  - `rental.AvailabilityQuery{ProductID int64; GroupID, MachineID *int64; StartAt *time.Time; Duration catalog.Duration; Quantity int}`；`rental.Availability{Available int \`json:"available"\`; Lower time.Time \`json:"lower"\`; Upper time.Time \`json:"upper"\`}`；`(*Service).Availability(ctx, q AvailabilityQuery) (Availability, error)`（只校验租期 / 数量 / 机组可见，不校验开始时间的提前量）。
  - `rental.CalendarDay{Date string \`json:"date"\`; Free, Total int}`；`(*Service).GroupCalendar(ctx, groupID int64, from time.Time, days int, visibleOnly bool) ([]CalendarDay, error)`（`days` 1～61，`from` 取北京时间当天 00:00；`total` = 组内 idle / allocated 机器数）。
  - `rental.CalendarPeriod{Lower, Upper time.Time; Mine bool; InstanceID string \`json:"instance_id,omitempty"\`; UserID int64 \`json:"user_id,omitempty"\`}`；`(*Service).MachineCalendar(ctx, machineID, userID int64, from time.Time, days int, admin bool) ([]CalendarPeriod, error)`（非 admin 只标 `mine`，不透出别人的实例 / 用户）。
  - `rental.MachineBrief{ID int64; Label string; BusyUntil *time.Time}`；`(*Service).GroupMachines(ctx, groupID int64, visibleOnly bool) ([]MachineBrief, error)`。
  - `(*Service).VisibleGroup(ctx, id int64) (fleet.Group, error)`（不存在或不可见 → `ErrGroupNotFound`）。

- [ ] **Step 1: 写失败的测试**

`internal/rental/calendar_test.go`：

```go
package rental_test

import (
	"errors"
	"testing"
	"time"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/rental"
)

func TestGroupCalendarCountsFreeMachinesPerDay(t *testing.T) {
	e := newEnv(t, 3)
	gid := e.group("A", true)
	for _, m := range e.machines {
		e.setGroup(m, gid)
	}
	e.setMachine(e.machines[2], "maintenance", true) // 不在 total 里
	// 机器 0：T0 + 2d 11:00 CST 起租 1 天 → 占 [T0+2d+2h, T0+2d+3h+24h+7h)，跨北京时间第 3、4 天
	e.reserveRaw(e.machines[0], dbtest.T0.Add(48*time.Hour+3*time.Hour), 86400)
	from := dbtest.T0 // 2026-01-01 08:00 CST；DayStart → 1 月 1 日 00:00 CST
	days, err := e.svc.GroupCalendar(e.ctx, gid, from, 5, true)
	if err != nil || len(days) != 5 {
		t.Fatalf("calendar = %+v, %v", days, err)
	}
	want := []rental.CalendarDay{
		{Date: "2026-01-01", Free: 2, Total: 2}, {Date: "2026-01-02", Free: 2, Total: 2}, {Date: "2026-01-03", Free: 1, Total: 2},
		{Date: "2026-01-04", Free: 1, Total: 2}, {Date: "2026-01-05", Free: 2, Total: 2},
	}
	for i, d := range days {
		if d != want[i] {
			t.Fatalf("day[%d] = %+v, want %+v", i, d, want[i])
		}
	}
	hidden := e.group("留池", false)
	if _, err := e.svc.GroupCalendar(e.ctx, hidden, from, 5, true); !errors.Is(err, apperr.ErrGroupNotFound) {
		t.Fatalf("hidden group (console) = %v", err)
	}
	if _, err := e.svc.GroupCalendar(e.ctx, hidden, from, 5, false); err != nil {
		t.Fatalf("hidden group (admin) = %v", err)
	}
	if _, err := e.svc.GroupCalendar(e.ctx, gid, from, 62, true); apperr.As(err) == nil {
		t.Fatalf("days > 61 = %v", err)
	}
}

func TestMachineCalendarMasksOtherUsers(t *testing.T) {
	e := newEnv(t, 1)
	gid := e.group("A", true)
	e.setGroup(e.machines[0], gid)
	mine := e.book(dbtest.T0.Add(3 * time.Hour))
	other := e.newUser("13600000002", startFunds)
	req := e.batchReq(1, ptr(dbtest.T0.Add(72*time.Hour)))
	req.UserID = other
	if _, err := e.svc.PlaceBatch(e.ctx, req); err != nil {
		t.Fatal(err)
	}
	periods, err := e.svc.MachineCalendar(e.ctx, e.machines[0], e.userID, dbtest.T0, 10, false)
	if err != nil || len(periods) != 2 {
		t.Fatalf("periods = %+v, %v", periods, err)
	}
	if !periods[0].Mine || periods[0].InstanceID != mine.ID || periods[1].Mine || periods[1].InstanceID != "" || periods[1].UserID != 0 {
		t.Fatalf("masking = %+v", periods)
	}
	adminView, _ := e.svc.MachineCalendar(e.ctx, e.machines[0], 0, dbtest.T0, 10, true)
	if adminView[1].InstanceID == "" || adminView[1].UserID != other {
		t.Fatalf("admin view = %+v", adminView)
	}
	// 窗口外的时段不返回
	if later, _ := e.svc.MachineCalendar(e.ctx, e.machines[0], e.userID, dbtest.T0.Add(10*24*time.Hour), 5, false); len(later) != 0 {
		t.Fatalf("outside window = %+v", later)
	}
	ms, err := e.svc.GroupMachines(e.ctx, gid, true)
	if err != nil || len(ms) != 1 || ms[0].Label != "机器 #"+itoa64(e.machines[0]) || ms[0].BusyUntil == nil || !ms[0].BusyUntil.Equal(dbtest.T0.Add(72*time.Hour+24*time.Hour+rental.Tail)) {
		t.Fatalf("group machines = %+v, %v", ms, err)
	}
}

func TestAvailabilityMatchesPlacement(t *testing.T) {
	e := newEnv(t, 3)
	start := dbtest.T0.Add(5 * time.Hour)
	e.reserveRaw(e.machines[0], start, 86400)
	q := rental.AvailabilityQuery{ProductID: e.productID, StartAt: &start, Duration: catalog.D1d, Quantity: 1}
	av, err := e.svc.Availability(e.ctx, q)
	if err != nil || av.Available != 2 || !av.Lower.Equal(start.Add(-rental.PrepLead)) {
		t.Fatalf("availability = %+v, %v", av, err)
	}
	for i := 0; i < av.Available; i++ {
		if _, err := e.svc.PlaceBatch(e.ctx, e.batchReq(1, &start)); err != nil {
			t.Fatalf("booking %d should succeed: %v", i, err)
		}
	}
	if _, err := e.svc.PlaceBatch(e.ctx, e.batchReq(1, &start)); codeOf(err) != "no_capacity" {
		t.Fatalf("one more must fail: %v", err)
	}
	if av, _ = e.svc.Availability(e.ctx, q); av.Available != 0 {
		t.Fatalf("after booking = %+v", av)
	}
	imm := rental.AvailabilityQuery{ProductID: e.productID, Duration: catalog.D1d, Quantity: 1}
	if av, _ = e.svc.Availability(e.ctx, imm); av.Available != 0 { // 三台都在 1 天 + 7h 内有预订
		t.Fatalf("immediate = %+v", av)
	}
	imm.Quantity = 91
	if _, err := e.svc.Availability(e.ctx, imm); !errors.Is(err, apperr.ErrBookingTooLong) {
		t.Fatalf("quantity 91 = %v", err)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/rental/ -run 'TestGroupCalendar|TestMachineCalendar|TestAvailabilityMatches' 2>&1 | head -20`
Expected: 编译错误。

- [ ] **Step 3: `calendar.go`**

```go
package rental

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/tz"
)

const maxCalendarDays = 61

type AvailabilityQuery struct {
	ProductID int64
	GroupID   *int64
	MachineID *int64
	StartAt   *time.Time // nil = 立即
	Duration  catalog.Duration
	Quantity  int
}

type Availability struct {
	Available int       `json:"available"`
	Lower     time.Time `json:"lower"`
	Upper     time.Time `json:"upper"`
}

// VisibleGroup 控制台可用的机组：不存在或 visible=false 都按不存在处理。
func (s *Service) VisibleGroup(ctx context.Context, id int64) (fleet.Group, error) {
	g, err := s.d.Fleet.GetGroup(ctx, s.d.Pool, id)
	if err != nil {
		return fleet.Group{}, err
	}
	if !g.Visible {
		return fleet.Group{}, apperr.ErrGroupNotFound
	}
	return g, nil
}

// Availability 所选时段可租台数（只读预筛，下单以事务内结果为准）。
func (s *Service) Availability(ctx context.Context, q AvailabilityQuery) (Availability, error) {
	if !q.Duration.Valid() {
		return Availability{}, apperr.BadRequest("租期不正确")
	}
	if q.Quantity == 0 {
		q.Quantity = 1
	}
	if q.Quantity < 1 || q.Quantity > MaxQuantity(q.Duration) {
		return Availability{}, apperr.ErrBookingTooLong
	}
	if q.GroupID != nil {
		if _, err := s.VisibleGroup(ctx, *q.GroupID); err != nil {
			return Availability{}, err
		}
	}
	secs := seconds(q.Duration) * int64(q.Quantity)
	spec := pickSpec{ProductID: q.ProductID, GroupID: q.GroupID, MachineID: q.MachineID, Immediate: q.StartAt == nil}
	if spec.Immediate {
		spec.Lower, spec.Upper = immediatePeriod(s.d.Clock.Now(), secs)
	} else {
		spec.Lower, spec.Upper = bookingPeriod(q.StartAt.UTC(), secs)
	}
	n, err := countAvailable(ctx, s.d.Pool, spec)
	if err != nil {
		return Availability{}, err
	}
	return Availability{Available: n, Lower: spec.Lower, Upper: spec.Upper}, nil
}

type CalendarDay struct {
	Date  string `json:"date"`
	Free  int    `json:"free"`
	Total int    `json:"total"`
}

func calendarRange(from time.Time, days int) (time.Time, error) {
	if days < 1 || days > maxCalendarDays {
		return time.Time{}, apperr.BadRequest(fmt.Sprintf("days 需在 1～%d 之间", maxCalendarDays))
	}
	return tz.DayStart(from), nil
}

// GroupCalendar 机组日历：每一天（北京时间自然日）组内与任何 reserved / active 时段都不相交的机器数；total 只算 idle / allocated 机器。
func (s *Service) GroupCalendar(ctx context.Context, groupID int64, from time.Time, days int, visibleOnly bool) ([]CalendarDay, error) {
	if visibleOnly {
		if _, err := s.VisibleGroup(ctx, groupID); err != nil {
			return nil, err
		}
	} else if _, err := s.d.Fleet.GetGroup(ctx, s.d.Pool, groupID); err != nil {
		return nil, err
	}
	start, err := calendarRange(from, days)
	if err != nil {
		return nil, err
	}
	rows, err := s.d.Pool.Query(ctx, `
		WITH d AS (SELECT generate_series($2::timestamptz, $2::timestamptz + ($3::int - 1) * interval '24 hours', interval '24 hours') AS day),
		     m AS (SELECT id FROM machines WHERE group_id = $1 AND state IN ('idle', 'allocated'))
		SELECT d.day,
		       count(m.id) FILTER (WHERE NOT EXISTS (SELECT 1 FROM machine_leases l WHERE l.machine_id = m.id
		                                           AND l.status IN ('reserved', 'active') AND l.period && tstzrange(d.day, d.day + interval '24 hours'))),
		       count(m.id)
		FROM d LEFT JOIN m ON true GROUP BY d.day ORDER BY d.day`, groupID, start, days)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (CalendarDay, error) {
		var day time.Time
		var c CalendarDay
		err := r.Scan(&day, &c.Free, &c.Total)
		c.Date = day.In(tz.CST).Format("2006-01-02")
		return c, err
	})
}

type CalendarPeriod struct {
	Lower      time.Time `json:"lower"`
	Upper      time.Time `json:"upper"`
	Mine       bool      `json:"mine"`
	InstanceID string    `json:"instance_id,omitempty"`
	UserID     int64     `json:"user_id,omitempty"`
}

// MachineCalendar 机器在 [from 当天 00:00, + days) 内的 reserved / active 时段；非 admin 只标 mine，不透出别人的实例与用户。
func (s *Service) MachineCalendar(ctx context.Context, machineID, userID int64, from time.Time, days int, admin bool) ([]CalendarPeriod, error) {
	start, err := calendarRange(from, days)
	if err != nil {
		return nil, err
	}
	rows, err := s.d.Pool.Query(ctx, `SELECT lower(l.period), upper(l.period), i.id, i.user_id
		FROM machine_leases l JOIN instances i ON i.id = l.instance_id
		WHERE l.machine_id = $1 AND l.status IN ('reserved', 'active') AND l.period && tstzrange($2::timestamptz, $2::timestamptz + $3::int * interval '24 hours')
		ORDER BY lower(l.period)`, machineID, start, days)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (CalendarPeriod, error) {
		var p CalendarPeriod
		var inst string
		var uid int64
		if err := r.Scan(&p.Lower, &p.Upper, &inst, &uid); err != nil {
			return p, err
		}
		p.Mine = uid == userID && userID != 0
		if admin || p.Mine {
			p.InstanceID, p.UserID = inst, uid
		}
		return p, nil
	})
}

type MachineBrief struct {
	ID        int64      `json:"id"`
	Label     string     `json:"label"`
	BusyUntil *time.Time `json:"busy_until"` // 现有 / 未来时段的最晚结束；nil = 当前没有占用
}

// GroupMachines 机组内可租机器（idle / allocated）与展示名，供控制台指定机器。
func (s *Service) GroupMachines(ctx context.Context, groupID int64, visibleOnly bool) ([]MachineBrief, error) {
	if visibleOnly {
		if _, err := s.VisibleGroup(ctx, groupID); err != nil {
			return nil, err
		}
	}
	rows, err := s.d.Pool.Query(ctx, `SELECT m.id, m.display_name,
			(SELECT max(upper(l.period)) FROM machine_leases l WHERE l.machine_id = m.id AND l.status IN ('reserved', 'active') AND upper(l.period) > $2)
		FROM machines m WHERE m.group_id = $1 AND m.state IN ('idle', 'allocated') ORDER BY m.display_name, m.id`, groupID, s.d.Clock.Now())
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (MachineBrief, error) {
		var b MachineBrief
		var name string
		if err := r.Scan(&b.ID, &name, &b.BusyUntil); err != nil {
			return b, err
		}
		b.Label = fleet.Machine{ID: b.ID, DisplayName: name}.Label()
		return b, nil
	})
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go vet ./... && go test ./internal/rental/ 2>&1 | tail -4`
Expected: `ok`。

- [ ] **Step 5: 提交**

```bash
git add internal/rental/calendar.go internal/rental/calendar_test.go
git commit -m "feat(booking): availability, group/machine calendars, group machine list

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 12: 控制台 API（机组 / 日历 / 可用性匿名端点、`/bookings`、取消、自动续费、`renew` 数量、事件、实例视图字段、`idle_count` 新语义）

**Files:**
- Create: `internal/api/booking.go`、`internal/api/booking_test.go`、`internal/rental/events.go`、`internal/rental/events_test.go`
- Modify: `internal/api/server.go`（路由、`CalendarLimit` 字段）、`internal/api/instances.go`（`instanceView` 字段、`views` 填充、`handleRenew` 的 `quantity`）、`internal/api/market.go`（`idle_count`）、`internal/app/app.go`（`CalendarLimit`）

**Interfaces:**
- Consumes: Task 5 `PlaceBatch` / `GetBatch` / `RenewQuantity`；Task 7 `Cancel` / `CancelQuote`；Task 9 `RenewInfoFor`；Task 10 `SetAutoRenew`；Task 11 `Availability` / `GroupCalendar` / `MachineCalendar` / `GroupMachines` / `VisibleGroup`；Task 4 `AvailableNow`；`Fleet.ListGroups(ctx, true)`、`Fleet.Get`、`Fleet.GetGroup`。
- Produces:
  - `rental.UserEvent{ToState string \`json:"to_state"\`; Kind string \`json:"reason_kind"\`; At time.Time \`json:"at"\`; Detail string \`json:"detail"\`}`；`(*rental.Service).UserEvents(ctx, userID int64, id string) ([]UserEvent, error)`（白名单映射，最近 20 条，新到旧；不透出机器 ID）。
  - `api.Server.CalendarLimit int`（匿名日历 / 可用性端点按 IP 每分钟上限；零值 120）。
  - 路由（`requireUser` 组外，`httpx.NoStore` + 按 IP 限流）：`GET /groups`（响应 `{items, now}`，`now` 为服务器时间，对话框据此算最早 / 最晚整点）、`GET /groups/{id}/machines`、`GET /groups/{id}/calendar`、`GET /machines/{id}/calendar`、`GET /availability`；组内：`POST /bookings`、`GET /bookings/{id}`、`GET /instances/{id}/cancel-quote`、`POST /instances/{id}/cancel`、`PUT /instances/{id}/auto-renew`、`GET /instances/{id}/events`；`POST /instances/{id}/renew` 请求体新增 `quantity`。
  - `instanceView` 新字段：`scheduled_start_at *time.Time`、`scheduled_end_at *time.Time`、`cancellable_until *time.Time`、`group_id *int64`、`group_name string`、`machine_label string`、`batch_id *string`、`reassign_count int`、`auto_renew bool`、`auto_renew_duration string`、`auto_renew_failed_reason string`、`renewable_until *time.Time`、`renew_options []rental.RenewOption`（终态实例为空数组）。
  - `productView.idle_count` = `Rental.AvailableNow`。
  - `POST /bookings` 请求体 `{product_id, image_id, group_id?, machine_id?, count, duration, quantity, start_at?, auto_renew?, auto_renew_duration?}`；`start_at` RFC3339；响应 201 `{batch, orders, instances}`。

- [ ] **Step 1: 写失败的测试**

`internal/rental/events_test.go`：

```go
package rental_test

import (
	"testing"
	"time"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/platform/dbtest"
)

func TestUserEventsWhitelistAndMasking(t *testing.T) {
	e := newEnv(t, 2)
	start := dbtest.T0.Add(3 * time.Hour)
	req := e.batchReq(1, &start)
	req.MachineID = &e.machines[0]
	res, _ := e.svc.PlaceBatch(e.ctx, req)
	inst := res.Instances[0]
	e.setMachine(e.machines[0], "maintenance", true)
	e.clock.Advance(2 * time.Hour)
	e.heartbeatAll()
	_, _ = e.svc.ProvisionDue(e.ctx) // 改派到机器 1
	e.ack(true)
	if _, err := e.svc.Renew(e.ctx, e.userID, inst.ID, catalog.D1d, ""); err != nil {
		t.Fatal(err)
	}
	events, err := e.svc.UserEvents(e.ctx, e.userID, inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, ev := range events {
		kinds = append(kinds, ev.Kind)
		if ev.Detail == "" {
			t.Fatalf("empty detail: %+v", ev)
		}
		for _, id := range e.machines {
			if contains(ev.Detail, itoa64(id)) && ev.Kind == "reassigned" {
				t.Fatalf("machine id leaked: %+v", ev)
			}
		}
	}
	want := []string{"renew", "created", "provision", "reassigned", "order"} // 新到旧：开通链路先记 reassigned 再记 scheduled start
	if len(kinds) != len(want) {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("kinds = %v, want %v", kinds, want)
		}
	}
	if events[4].Detail != "预订下单，2026-01-01 11:00 开始" || events[3].Detail != "已为您更换机器" || events[2].Detail != "开始开通" {
		t.Fatalf("details = %+v", events)
	}
	if _, err := e.svc.UserEvents(e.ctx, e.userID+1, inst.ID); err == nil {
		t.Fatal("other user must get not_found")
	}
}

func contains(s, sub string) bool { return len(sub) > 0 && len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0) }

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
```

`internal/api/booking_test.go`：

```go
package api_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/dbtest"
)

// verifiedClient 登录、实名并充值 ¥10,000。
func (e *testEnv) verifiedClient(phone string) (*client, int64) {
	e.t.Helper()
	c := e.client()
	user := c.login(phone)
	if code, out := c.do("POST", "/kyc/personal", map[string]string{"name": "张三", "id_number": "11010119900101123X"}); code != 200 {
		e.t.Fatalf("kyc = %d %v", code, out)
	}
	uid := int64(user["id"].(float64))
	if _, err := e.wallet.AdminAdjust(context.Background(), 1, uid, 1_000_000, "测试充值", ""); err != nil {
		e.t.Fatal(err)
	}
	return c, uid
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
	if code != 200 || len(out["items"].([]any)) != 1 || out["items"].([]any)[0].(map[string]any)["idle_now"].(float64) != 2 {
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

	c, _ := e.verifiedClient("13800000010")
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
	if code, out := c.do("GET", "/instances/"+id, nil); code != 200 || out["renew_options"] == nil || out["scheduled_end_at"] == nil {
		t.Fatalf("instance = %d %v", code, out)
	}
	if code, out := c.do("GET", "/instances/"+id+"/events", nil); code != 200 || len(out["items"].([]any)) != 1 {
		t.Fatalf("events = %d %v", code, out)
	}
	if code, out := c.do("PUT", "/instances/"+id+"/auto-renew", map[string]any{"enabled": false, "duration": "1d"}); code != 200 || out["auto_renew"] != false {
		t.Fatalf("auto-renew = %d %v", code, out)
	}
	if code, out := c.do("POST", "/instances/"+id+"/renew", map[string]any{"duration": "1d", "quantity": 2}); code != 200 || out["order"].(map[string]any)["quantity"].(float64) != 2 {
		t.Fatalf("renew x2 = %d %v", code, out)
	}
	code, out = c.do("GET", "/instances/"+id+"/cancel-quote", nil)
	if code != 200 || out["percent"].(float64) != 100 || out["refund"].(float64) != 3*6900 {
		t.Fatalf("cancel-quote = %d %v", code, out)
	}
	if code, out := c.do("POST", "/instances/"+id+"/cancel", map[string]any{}); code != 200 || out["state"] != "cancelled" {
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
	// 机器日历：匿名也能看，但不带实例 ID（看仍有预订的那台：insts[1] 所在机器）
	other := int64(insts[1].(map[string]any)["machine_id"].(float64))
	code, out = anon.do("GET", "/machines/"+itoa(other)+"/calendar?days=5", nil)
	periods := out["periods"].([]any)
	if code != 200 || len(periods) != 1 || periods[0].(map[string]any)["instance_id"] != nil {
		t.Fatalf("machine calendar = %d %v", code, out)
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

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
```

（文件 import 需要 `"strconv"` 与 `"spark-cloud/internal/api"`；`e.fleet` 已在 `testEnv`。）

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/rental/ ./internal/api/ -run 'TestUserEvents|TestBookingEndpoints|TestCalendarEndpoints' 2>&1 | head -20`
Expected: 编译错误（`UserEvents`、`CalendarLimit` 未定义）。

- [ ] **Step 3: `rental/events.go`**

```go
package rental

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/catalog"
)

// UserEvent 用户可见的实例事件（白名单；不透出机器 ID、序列号）。
type UserEvent struct {
	ToState string    `json:"to_state"`
	Kind    string    `json:"reason_kind"`
	At      time.Time `json:"at"`
	Detail  string    `json:"detail"`
}

// userEventOf 把 instance_events.reason 翻译成用户文案；不在白名单返回 ok=false。
func userEventOf(e Event) (UserEvent, bool) {
	r := e.Reason
	out := UserEvent{ToState: e.ToState, At: e.At}
	switch {
	case strings.HasPrefix(r, "order "):
		out.Kind, out.Detail = "order", "下单"
		if i := strings.Index(r, " scheduled "); i >= 0 {
			out.Detail = "预订下单，" + r[i+len(" scheduled "):] + " 开始"
		}
	case strings.HasPrefix(r, "scheduled start "):
		out.Kind, out.Detail = "provision", "开始开通"
	case strings.HasPrefix(r, "agent: created"):
		out.Kind, out.Detail = "created", "开通成功"
		if strings.Contains(r, "late by") {
			out.Detail = "开通成功（晚于预定时间）"
		}
	case strings.HasPrefix(r, "late start compensated"):
		out.Kind, out.Detail = "compensated", "迟开通补偿已退回余额"
	case strings.HasPrefix(r, "reassigned "):
		out.Kind, out.Detail = "reassigned", "已为您更换机器"
	case strings.HasPrefix(r, "create failed, waiting"):
		out.Kind, out.Detail = "waiting", "开通失败，正在等待可用机器"
	case strings.HasPrefix(r, "auto renew failed: "):
		reason := autoRenewFailText[strings.TrimPrefix(r, "auto renew failed: ")]
		out.Kind, out.Detail = "auto_renew_failed", "自动续费失败："+reason
	case strings.HasPrefix(r, "auto renew "):
		d := ""
		if i := strings.LastIndex(r, "("); i >= 0 {
			d = strings.TrimSuffix(r[i+1:], ")")
		}
		out.Kind, out.Detail = "auto_renew", "自动续费成功（"+durationText(catalog.Duration(d))+"）"
	case strings.HasPrefix(r, "renew "):
		out.Kind, out.Detail = "renew", "续费成功"
	case strings.HasPrefix(r, "user cancel"):
		out.Kind, out.Detail = "cancelled", "已取消预订"+refundText(r)
	case strings.HasPrefix(r, "admin cancel"), strings.HasPrefix(r, "unfulfilled"), strings.HasPrefix(r, "cancelled: user cannot rent"):
		out.Kind, out.Detail = "cancelled", "预订已由平台取消"+refundText(r)
	case r == "expired":
		out.Kind, out.Detail = "expired", "到期停机"
	case r == "user released":
		out.Kind, out.Detail = "released", "已释放"
	case r == "retention expired":
		out.Kind, out.Detail = "wiped", "保留期结束，数据已清除"
	case r == "agent: wiped":
		out.Kind, out.Detail = "released", "释放完成"
	default:
		return UserEvent{}, false
	}
	return out, true
}

func durationText(d catalog.Duration) string {
	switch d {
	case catalog.D1d:
		return "1 天"
	case catalog.D1w:
		return "1 周"
	case catalog.D1m:
		return "1 月"
	}
	return string(d)
}

// refundText 从 "..., refund <分>" 取金额。
func refundText(r string) string {
	i := strings.LastIndex(r, "refund ")
	if i < 0 {
		return ""
	}
	var cents int64
	if _, err := fmt.Sscanf(r[i+len("refund "):], "%d", &cents); err != nil || cents <= 0 {
		return ""
	}
	return fmt.Sprintf("，已退款 ¥%d.%02d", cents/100, cents%100)
}

// UserEvents 返回用户自己实例的最近 20 条白名单事件（新到旧）。
func (s *Service) UserEvents(ctx context.Context, userID int64, id string) ([]UserEvent, error) {
	if _, err := s.GetForUser(ctx, userID, id); err != nil {
		return nil, err
	}
	rows, err := s.d.Pool.Query(ctx, `SELECT id, from_state, to_state, reason, at FROM instance_events WHERE instance_id = $1 ORDER BY id DESC`, id)
	if err != nil {
		return nil, err
	}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Event, error) {
		var e Event
		err := r.Scan(&e.ID, &e.FromState, &e.ToState, &e.Reason, &e.At)
		return e, err
	})
	if err != nil {
		return nil, err
	}
	out := []UserEvent{}
	for _, e := range all {
		if ue, ok := userEventOf(e); ok {
			out = append(out, ue)
			if len(out) == 20 {
				break
			}
		}
	}
	return out, nil
}
```

- [ ] **Step 4: `api/booking.go`**

```go
package api

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/tz"
	"spark-cloud/internal/rental"
)

const defaultCalendarLimit = 120

// ipLimiter 进程内按 IP 的滑动一分钟限流（匿名的机组 / 日历 / 可用性端点）。
type ipLimiter struct {
	mu    sync.Mutex
	limit int
	hits  map[string][]time.Time
}

func (l *ipLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.hits == nil {
		l.hits = map[string][]time.Time{}
	}
	ts := l.hits[key]
	i := 0
	for i < len(ts) && now.Sub(ts[i]) >= time.Minute {
		i++
	}
	ts = ts[i:]
	if len(ts) >= l.limit {
		l.hits[key] = ts
		return false
	}
	l.hits[key] = append(ts, now)
	// 匿名端点按 IP 建键：顺手清掉一个已空的键，map 不会无界增长（每次调用摊销 O(1)）
	for k, v := range l.hits {
		if k != key && (len(v) == 0 || now.Sub(v[len(v)-1]) >= time.Minute) {
			delete(l.hits, k)
			break
		}
	}
	return true
}

func (s *Server) calendarRateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.calendarLim.allow(clientIP(r), s.Clock.Now()) {
			s.writeError(w, r, apperr.ErrRateLimited)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// optionalUserID 匿名端点里尽力识别当前用户（机器日历的 mine 标记）；没有或无效会话返回 0。
func (s *Server) optionalUserID(r *http.Request) int64 {
	c, err := r.Cookie(s.cookieName())
	if err != nil {
		return 0
	}
	u, err := s.Identity.Authenticate(r.Context(), c.Value)
	if err != nil {
		return 0
	}
	return u.ID
}

func queryInt64(r *http.Request, name string) (*int64, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return nil, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return nil, apperr.BadRequest(name + " 必须是正整数")
	}
	return &n, nil
}

// calendarWindow 解析 from（YYYY-MM-DD，北京时间；缺省今天）与 days（缺省 61）。
func (s *Server) calendarWindow(r *http.Request) (time.Time, int, error) {
	q := r.URL.Query()
	from := s.Clock.Now()
	if v := q.Get("from"); v != "" {
		t, err := time.ParseInLocation("2006-01-02", v, tz.CST)
		if err != nil {
			return time.Time{}, 0, apperr.BadRequest("from 的格式应为 2006-01-02")
		}
		from = t.UTC()
	}
	days := 61
	if v := q.Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return time.Time{}, 0, apperr.BadRequest("days 必须是整数")
		}
		days = n
	}
	return from, days, nil
}

type groupView struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Region       string `json:"region"`
	Description  string `json:"description"`
	MachineCount int    `json:"machine_count"`
	IdleNow      int    `json:"idle_now"`
}

func (s *Server) handleGroups(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Fleet.ListGroups(r.Context(), true)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out := make([]groupView, 0, len(rows))
	for _, g := range rows {
		out = append(out, groupView{ID: g.ID, Name: g.Name, Region: g.Region, Description: g.Description, MachineCount: g.MachineCount, IdleNow: g.IdleNow})
	}
	// now：服务器时间。预订对话框的「最早 / 最晚整点」按它算（e2e 的假时钟与浏览器时间可能相差数天）。
	writeJSON(w, http.StatusOK, map[string]any{"items": out, "now": s.Clock.Now()})
}

func pathInt64(r *http.Request, name string) (int64, error) {
	n, err := strconv.ParseInt(chi.URLParam(r, name), 10, 64)
	if err != nil || n <= 0 {
		return 0, apperr.BadRequest("ID 不正确")
	}
	return n, nil
}

func (s *Server) handleGroupMachines(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	items, err := s.Rental.GroupMachines(r.Context(), id, true)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list(items))
}

func (s *Server) handleGroupCalendar(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	from, days, err := s.calendarWindow(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out, err := s.Rental.GroupCalendar(r.Context(), id, from, days, true)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"days": out})
}

func (s *Server) handleMachineCalendar(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	m, err := s.Fleet.Get(r.Context(), s.Fleet.Pool(), id)
	if err != nil || m.GroupID == nil {
		s.writeError(w, r, apperr.ErrNotFound)
		return
	}
	if _, err := s.Rental.VisibleGroup(r.Context(), *m.GroupID); err != nil {
		s.writeError(w, r, apperr.ErrNotFound)
		return
	}
	from, days, err := s.calendarWindow(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out, err := s.Rental.MachineCalendar(r.Context(), id, s.optionalUserID(r), from, days, false)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"periods": out})
}

func parseStartAt(v string) (*time.Time, error) {
	if v == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return nil, apperr.BadRequest("start_at 的格式应为 RFC3339，例如 2026-10-20T10:00:00+08:00")
	}
	t = t.UTC()
	return &t, nil
}

func (s *Server) handleAvailability(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	productID, err := queryInt64(r, "product_id")
	if err != nil || productID == nil {
		s.writeError(w, r, apperr.BadRequest("product_id 必须是正整数"))
		return
	}
	groupID, err := queryInt64(r, "group_id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	machineID, err := queryInt64(r, "machine_id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	start, err := parseStartAt(q.Get("start_at"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	quantity, _ := strconv.Atoi(q.Get("quantity"))
	out, err := s.Rental.Availability(r.Context(), rental.AvailabilityQuery{ProductID: *productID, GroupID: groupID, MachineID: machineID,
		StartAt: start, Duration: catalog.Duration(q.Get("duration")), Quantity: quantity})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

type bookingRequest struct {
	ProductID         int64  `json:"product_id"`
	ImageID           int64  `json:"image_id"`
	GroupID           *int64 `json:"group_id"`
	MachineID         *int64 `json:"machine_id"`
	Count             int    `json:"count"`
	Duration          string `json:"duration"`
	Quantity          int    `json:"quantity"`
	StartAt           string `json:"start_at"`
	AutoRenew         bool   `json:"auto_renew"`
	AutoRenewDuration string `json:"auto_renew_duration"`
}

func (s *Server) batchResult(ctx context.Context, res rental.BatchResult) (map[string]any, error) {
	views, err := s.views(ctx, res.Instances)
	if err != nil {
		return nil, err
	}
	return map[string]any{"batch": res.Batch, "orders": res.Orders, "instances": views}, nil
}

func (s *Server) handleCreateBooking(w http.ResponseWriter, r *http.Request) {
	var req bookingRequest
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	start, err := parseStartAt(req.StartAt)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	// 规格 §10.1：指定机器时 group_id 必须是该机器所在机组（validateBatch 不管这条，后台改派与 rental 测试依赖它的宽松）
	if req.MachineID != nil {
		m, err := s.Fleet.Get(r.Context(), s.Fleet.Pool(), *req.MachineID)
		if err != nil || m.GroupID == nil || req.GroupID == nil || *m.GroupID != *req.GroupID {
			s.writeError(w, r, apperr.BadRequest("指定机器时 group_id 必须是该机器所在机组"))
			return
		}
	}
	res, err := s.Rental.PlaceBatch(r.Context(), rental.BatchRequest{
		UserID: currentUser(r).ID, ProductID: req.ProductID, ImageID: req.ImageID, GroupID: req.GroupID, MachineID: req.MachineID,
		Count: req.Count, Duration: catalog.Duration(req.Duration), Quantity: req.Quantity, StartAt: start,
		AutoRenew: req.AutoRenew, AutoRenewDuration: catalog.Duration(req.AutoRenewDuration), IdempotencyKey: r.Header.Get(idempotencyHeader),
	})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out, err := s.batchResult(r.Context(), res)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) handleBatch(w http.ResponseWriter, r *http.Request) {
	res, err := s.Rental.GetBatch(r.Context(), currentUser(r).ID, chi.URLParam(r, "id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	out, err := s.batchResult(r.Context(), res)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCancelQuote(w http.ResponseWriter, r *http.Request) {
	q, err := s.Rental.CancelQuote(r.Context(), currentUser(r).ID, chi.URLParam(r, "id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, q)
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	inst, err := s.Rental.Cancel(r.Context(), currentUser(r).ID, chi.URLParam(r, "id"), clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	v, err := s.viewOne(r.Context(), inst)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleAutoRenew(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled  bool   `json:"enabled"`
		Duration string `json:"duration"`
	}
	if err := decodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	inst, err := s.Rental.SetAutoRenew(r.Context(), currentUser(r).ID, chi.URLParam(r, "id"), req.Enabled, catalog.Duration(req.Duration), clientIP(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	v, err := s.viewOne(r.Context(), inst)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	items, err := s.Rental.UserEvents(r.Context(), currentUser(r).ID, chi.URLParam(r, "id"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list(items))
}
```

（import 加 `"context"`。）

- [ ] **Step 5: 路由、`CalendarLimit`、实例视图、`idle_count`、`renew` 数量**

`internal/api/server.go`：`Server` 加字段 `CalendarLimit int // 匿名机组 / 日历 / 可用性端点按 IP 每分钟上限；零值 120` 与 `calendarLim *ipLimiter`；`Router()` 里 `s.authorizeLim = …` 之后加：

```go
	climit := s.CalendarLimit
	if climit <= 0 {
		climit = defaultCalendarLimit
	}
	s.calendarLim = &ipLimiter{limit: climit}
```

`r.Get("/pay/channels", …)` 之后加（组外、匿名）：

```go
		r.Group(func(r chi.Router) {
			r.Use(httpx.NoStore, s.calendarRateLimit)
			r.Get("/groups", s.handleGroups)
			r.Get("/groups/{id}/machines", s.handleGroupMachines)
			r.Get("/groups/{id}/calendar", s.handleGroupCalendar)
			r.Get("/machines/{id}/calendar", s.handleMachineCalendar)
			r.Get("/availability", s.handleAvailability)
		})
```

`requireUser` 组内 `r.Post("/instances/{id}/release", …)` 之后加：

```go
			r.Post("/bookings", s.handleCreateBooking)
			r.Get("/bookings/{id}", s.handleBatch)
			r.Get("/instances/{id}/cancel-quote", s.handleCancelQuote)
			r.Post("/instances/{id}/cancel", s.handleCancel)
			r.Put("/instances/{id}/auto-renew", s.handleAutoRenew)
			r.Get("/instances/{id}/events", s.handleEvents)
```

`internal/api/instances.go`：`instanceView` 在 `CreatedAt` 之前加：

```go
	// 计划 6
	ScheduledStartAt      *time.Time           `json:"scheduled_start_at"`
	ScheduledEndAt        *time.Time           `json:"scheduled_end_at"`
	CancellableUntil      *time.Time           `json:"cancellable_until"`
	GroupID               *int64               `json:"group_id"`
	GroupName             string               `json:"group_name"`
	MachineLabel          string               `json:"machine_label"`
	BatchID               *string              `json:"batch_id"`
	ReassignCount         int                  `json:"reassign_count"`
	AutoRenew             bool                 `json:"auto_renew"`
	AutoRenewDuration     string               `json:"auto_renew_duration"`
	AutoRenewFailedReason string               `json:"auto_renew_failed_reason"`
	RenewableUntil        *time.Time           `json:"renewable_until"`
	RenewOptions          []rental.RenewOption `json:"renew_options"`
```

`view()` 末尾（`return v` 之前）加：

```go
	v.GroupID, v.BatchID, v.ReassignCount = i.GroupID, i.BatchID, i.ReassignCount
	v.AutoRenew, v.AutoRenewDuration, v.AutoRenewFailedReason = i.AutoRenew, string(i.AutoRenewDuration), i.AutoRenewFailedReason
	v.RenewOptions = []rental.RenewOption{}
	if i.ScheduledStartAt != nil {
		v.ScheduledStartAt = i.ScheduledStartAt
		end := i.ScheduledStartAt.Add(time.Duration(i.PendingSeconds) * time.Second)
		v.ScheduledEndAt = &end
		if i.State == rental.StateReserved {
			until := i.ScheduledStartAt.Add(-rental.PrepLead)
			v.CancellableUntil = &until
		}
	}
```

`nameCache` 加 `machines map[int64]string`（机器展示名）与 `groups map[int64]string`；`newNameCache` 初始化它们；`viewNamed` 末尾加：

```go
	if v.MachineLabel, err = nc.lookup(nc.machines, i.MachineID, func() (string, error) {
		m, err := s.Fleet.Get(ctx, s.Fleet.Pool(), i.MachineID)
		if err != nil {
			return "", err
		}
		return m.Label(), nil
	}); err != nil {
		return v, err
	}
	if i.GroupID != nil {
		if v.GroupName, err = nc.lookup(nc.groups, *i.GroupID, func() (string, error) {
			g, err := s.Fleet.GetGroup(ctx, s.Fleet.Pool(), *i.GroupID)
			if err != nil {
				return "", err
			}
			return g.Name, nil
		}); err != nil {
			return v, err
		}
	}
	switch i.State {
	case rental.StateReserved, rental.StateCreating, rental.StateRunning, rental.StateStopped:
		info, err := s.Rental.RenewInfoFor(ctx, i)
		if err != nil {
			return v, err
		}
		v.RenewableUntil, v.RenewOptions = info.RenewableUntil, info.Options
	}
```

（`nameCache.lookup` 对 `ErrNotFound` 退回空名，机组 `ErrGroupNotFound` 不是 `ErrNotFound`：在 `lookup` 里把 `errors.Is(err, apperr.ErrNotFound) || errors.Is(err, apperr.ErrGroupNotFound)` 都视为空名。）

`handleRenew` 请求体加 `Quantity int \`json:"quantity"\``，调用改为 `s.Rental.RenewQuantity(r.Context(), currentUser(r).ID, chi.URLParam(r, "id"), catalog.Duration(req.Duration), req.Quantity, r.Header.Get(idempotencyHeader))`。

`internal/api/market.go` 的 `handleProducts`：`n, err := s.Rental.AvailableNow(r.Context(), p.ID)` 代替 `s.Fleet.IdleCount`。

`internal/app/app.go` 组装 `api.Server` 时加 `CalendarLimit: authorizeLimit`（`DisableRateLimits` 时同样是 `noRateLimit`）。

- [ ] **Step 6: 运行测试确认通过**

Run: `go vet ./... && go test ./internal/rental/ ./internal/api/ ./internal/app/ 2>&1 | tail -6`
Expected: 全部 `ok`。现有 `TestOrderAndInstanceEndpoints` 断言 `idle_count == 1`（1 台空闲且无预订）仍成立。

- [ ] **Step 7: 提交**

```bash
git add internal/api/booking.go internal/api/booking_test.go internal/api/server.go internal/api/instances.go internal/api/market.go internal/rental/events.go internal/rental/events_test.go internal/app/app.go
git commit -m "feat(api): booking endpoints, anonymous group/machine calendars and availability, cancel, auto-renew, instance view fields, user-visible events

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 13: 后台 API（预订列表 / 即将开始、取消预订 + 动态码、改派、机器档期、仪表盘、订单批次筛选与 CSV、维护提示）

**Files:**
- Create: `internal/rental/admin_bookings.go`、`internal/adminapi/bookings.go`、`internal/adminapi/bookings_test.go`
- Modify: `internal/adminapi/server.go`（`s.bookingRoutes(g)`）、`internal/adminapi/views.go`（`instanceView` 新字段、`orderView` 新字段）、`internal/adminapi/machines.go`（`handleMachine` 增加 `group / leases / upcoming_bookings`；`handleMaintenance` 响应）、`internal/adminapi/dashboard.go`（`bookings_upcoming_7d`）、`internal/adminapi/finance.go`（`batch_id` 筛选与 CSV 列）、`internal/rental/admin_queries.go`（`OrderFilter.BatchID`）、`internal/adminapi/permissions_test.go`

**Interfaces:**
- Consumes: Task 7 `AdminCancelBooking`；Task 8 `reassign` / `lockLease`；Task 11 `MachineCalendar`；Task 5 `Order` 字段；`paging`。
- Produces:
  - `rental.BookingFilter{State string; UserID, GroupID, MachineID int64; From, To *time.Time}`（`From/To` 作用于 `scheduled_start_at`；`State` 缺省 `reserved`，允许 `reserved / cancelled`）；`(*Service).AdminListBookings(ctx, f, page) ([]Instance, int, error)`（按 `scheduled_start_at, id`）。
  - `(*Service).UpcomingBookings(ctx, from time.Time, days int) ([]int, error)`（北京时间逐日 reserved 开始台数）；`(*Service).UpcomingBookingsForMachine(ctx, machineID) (int, error)`；`(*Service).MachineStates(ctx, ids []int64) (map[int64]string, error)`；`GET /bookings` 每行为 `instanceView` 加 `machine_state`。
  - `(*Service).AdminReassign(ctx, actor, id string, machineID *int64, reason string) (Instance, error)`：仅 reserved；`machineID` 为 nil 时同组自动挑选（不要求在线），否则目标机器必须 `idle / allocated` 且时段无冲突（任意机组，含不可见），不满足 → `ErrMachineUnavailable`；审计 `booking.reassign`（diff `from_machine / to_machine / reason`）。
  - `rental.OrderFilter.BatchID string`。
  - 后台路由：`GET /bookings`（all）、`GET /bookings/upcoming?days=`（all）、`POST /instances/{id}/cancel-booking`（ops，body `{reason, totp}`）、`POST /instances/{id}/reassign`（ops，body `{machine_id?, reason}`）、`GET /machines/{id}/calendar?from&days`（ops）。
  - `adminapi.instanceView` 新字段：`scheduled_start_at, scheduled_end_at, group_id, batch_id, reassign_count, auto_renew, auto_renew_duration, auto_renew_failed_reason, provisioned_at, cancelled_at`；`orderView` 新字段 `quantity, batch_id, auto`。
  - `GET /machines/{id}` 响应新增 `group`（`fleet.Group` 或 null）、`leases`（60 天 `CalendarPeriod` 数组，含 `instance_id / user_id`）、`upcoming_bookings`；`POST /machines/{id}/maintenance` 响应为机器 JSON 加 `upcoming_bookings`。
  - `GET /dashboard` 新增 `bookings_upcoming_7d`。
  - `GET /orders` / `GET /orders/export` 支持 `batch_id` 筛选；CSV 列增加「批次」「数量」「自动续费」。

- [ ] **Step 1: 写失败的测试**

`internal/adminapi/bookings_test.go`：

```go
package adminapi_test

import (
	"testing"
	"time"

	"spark-cloud/internal/admin"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/rental"
)

// smsCount 按模板统计已发送短信条数。
func (e *env) smsCount(template string) int {
	n := 0
	for _, m := range e.sms.Sent() {
		if m.Template == template {
			n++
		}
	}
	return n
}

func (e *env) book(uid int64, start time.Time) rental.Instance {
	e.t.Helper()
	res, err := e.rental.PlaceBatch(e.ctx, rental.BatchRequest{UserID: uid, ProductID: dbtest.ProductID(e.t, e.pool), ImageID: dbtest.ImageID(e.t, e.pool),
		Count: 1, Duration: catalog.D1d, Quantity: 1, StartAt: &start})
	if err != nil {
		e.t.Fatalf("book: %v", err)
	}
	return res.Instances[0]
}

func TestBookingAdminAPI(t *testing.T) {
	e := newEnv(t)
	m1 := e.machine("SN1")
	m2 := e.machine("SN2")
	uid := e.renter("13600000001")
	start := dbtest.T0.Add(3 * time.Hour)
	inst := e.book(uid, start)
	ops := e.loginAs("ops", admin.RoleOps)
	fin := e.loginAs("fin", admin.RoleFinance)

	code, out := fin.do("GET", "/bookings", nil)
	if code != 200 || out["total"].(float64) != 1 {
		t.Fatalf("list = %d %v", code, out)
	}
	row := out["items"].([]any)[0].(map[string]any)
	if row["id"] != inst.ID || row["state"] != "reserved" || row["scheduled_start_at"] == nil || row["scheduled_end_at"] == nil {
		t.Fatalf("row = %v", row)
	}
	if code, out := fin.do("GET", "/bookings?state=cancelled", nil); code != 200 || out["total"].(float64) != 0 {
		t.Fatalf("cancelled list = %d %v", code, out)
	}
	if code, out := fin.do("GET", "/bookings?state=running", nil); code != 400 {
		t.Fatalf("bad state = %d %v", code, out)
	}
	code, out = fin.do("GET", "/bookings/upcoming?days=7", nil)
	if code != 200 || len(out["days"].([]any)) != 7 || out["days"].([]any)[0].(map[string]any)["count"].(float64) != 1 {
		t.Fatalf("upcoming = %d %v", code, out)
	}
	if code, out := fin.do("GET", "/dashboard", nil); code != 200 || out["bookings_upcoming_7d"].(float64) != 1 {
		t.Fatalf("dashboard = %d %v", code, out)
	}
	code, out = ops.do("GET", "/machines/"+itoa(inst.MachineID), nil)
	leases := out["leases"].([]any)
	if code != 200 || len(leases) != 1 || leases[0].(map[string]any)["instance_id"] != inst.ID || out["upcoming_bookings"].(float64) != 1 {
		t.Fatalf("machine detail = %d %v", code, out)
	}
	// 改派：指定机器
	target := m2
	if inst.MachineID == m2 {
		target = m1
	}
	code, out = ops.do("POST", "/instances/"+inst.ID+"/reassign", map[string]any{"machine_id": target, "reason": "换机柜"})
	if code != 200 || out["machine_id"].(float64) != float64(target) || out["reassign_count"].(float64) != 1 {
		t.Fatalf("reassign = %d %v", code, out)
	}
	if code, out := ops.do("POST", "/instances/"+inst.ID+"/reassign", map[string]any{"machine_id": target, "reason": "再来"}); code != 409 || codeOf(out) != "machine_unavailable" {
		t.Fatalf("reassign to busy machine = %d %v", code, out)
	}
	if code, out := ops.do("POST", "/instances/"+inst.ID+"/reassign", map[string]any{"reason": "自动"}); code != 200 || out["reassign_count"].(float64) != 2 {
		t.Fatalf("auto reassign = %d %v", code, out)
	}
	if code, out := fin.do("POST", "/instances/"+inst.ID+"/reassign", map[string]any{"reason": "x"}); code != 403 || codeOf(out) != "forbidden" {
		t.Fatalf("finance reassign = %d %v", code, out)
	}
	// 进入维护时的提示数：对预订当前所在的机器（自动改派后要重读）
	cur, _ := e.rental.Get(e.ctx, inst.ID)
	code, out = ops.do("POST", "/machines/"+itoa(cur.MachineID)+"/maintenance", map[string]any{"on": true})
	if code != 200 || out["state"] != "maintenance" || out["upcoming_bookings"].(float64) != 1 {
		t.Fatalf("maintenance = %d %v", code, out)
	}
	// 订单按批次筛选与 CSV
	batchID := ""
	_ = e.pool.QueryRow(e.ctx, `SELECT batch_id FROM instances WHERE id = $1`, inst.ID).Scan(&batchID)
	if code, out := fin.do("GET", "/orders?batch_id="+batchID, nil); code != 200 || out["total"].(float64) != 1 || out["items"].([]any)[0].(map[string]any)["quantity"].(float64) != 1 {
		t.Fatalf("orders by batch = %d %v", code, out)
	}
	resp, _ := fin.send("GET", "/orders/export?batch_id="+batchID, nil)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/csv; charset=utf-8" {
		t.Fatalf("export = %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	// 取消预订：需要动态码；全额退款；审计
	if code, out := ops.do("POST", "/instances/"+inst.ID+"/cancel-booking", map[string]any{"reason": "机房断电"}); code != 403 || codeOf(out) != "totp_required" {
		t.Fatalf("cancel without totp = %d %v", code, out)
	}
	code, out = ops.do("POST", "/instances/"+inst.ID+"/cancel-booking", map[string]any{"reason": "机房断电", "totp": ops.totp()})
	if code != 200 || out["refunded"].(float64) != 6900 {
		t.Fatalf("cancel = %d %v", code, out)
	}
	if code, out := fin.do("GET", "/bookings?state=cancelled", nil); code != 200 || out["total"].(float64) != 1 {
		t.Fatalf("cancelled list after = %d %v", code, out)
	}
	var audits int
	_ = e.pool.QueryRow(e.ctx, `SELECT count(*) FROM audit_logs WHERE action IN ('booking.reassign', 'booking.admin_cancel')`).Scan(&audits)
	if audits != 3 {
		t.Fatalf("audits = %d", audits)
	}
}
```

`internal/adminapi/permissions_test.go` 的 `expectedRoutes` 追加：

```go
	{"GET", "/bookings", all}, {"GET", "/bookings/upcoming", all}, {"POST", "/instances/{id}/cancel-booking", ops},
	{"POST", "/instances/{id}/reassign", ops}, {"GET", "/machines/{id}/calendar", ops},
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/adminapi/ -run 'TestBookingAdminAPI|TestRouteTable' 2>&1 | head -20`
Expected: 编译错误 / 路由矩阵缺少新路由。

- [ ] **Step 3: `rental/admin_bookings.go`**

```go
package rental

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/paging"
)

type BookingFilter struct {
	State     string // reserved（缺省）/ cancelled
	UserID    int64
	GroupID   int64
	MachineID int64
	From      *time.Time // scheduled_start_at >=
	To        *time.Time // scheduled_start_at <（不含）
}

// AdminListBookings 后台预订列表：只含预订实例（scheduled_start_at 非空），按开始时间排序。
func (s *Service) AdminListBookings(ctx context.Context, f BookingFilter, p paging.Page) ([]Instance, int, error) {
	if f.State == "" {
		f.State = StateReserved
	}
	if !slices.Contains([]string{StateReserved, StateCancelled}, f.State) {
		return nil, 0, apperr.BadRequest("预订状态只能是 reserved 或 cancelled")
	}
	var w db.Where
	w.Add("scheduled_start_at IS NOT NULL")
	w.Add("state = ?", f.State)
	if f.UserID > 0 {
		w.Add("user_id = ?", f.UserID)
	}
	if f.GroupID > 0 {
		w.Add("group_id = ?", f.GroupID)
	}
	if f.MachineID > 0 {
		w.Add("machine_id = ?", f.MachineID)
	}
	if f.From != nil {
		w.Add("scheduled_start_at >= ?", *f.From)
	}
	if f.To != nil {
		w.Add("scheduled_start_at < ?", *f.To)
	}
	var total int
	if err := s.d.Pool.QueryRow(ctx, `SELECT count(*) FROM instances WHERE `+w.SQL(), w.Args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	n := len(w.Args)
	list, err := collectInstances(s.d.Pool.Query(ctx, `SELECT `+instanceCols+` FROM instances WHERE `+w.SQL()+
		fmt.Sprintf(` ORDER BY scheduled_start_at, id LIMIT $%d OFFSET $%d`, n+1, n+2), append(w.Args, p.Limit(), p.Offset())...))
	return list, total, err
}

// UpcomingBookings 从 from 当天（北京时间）起连续 days 天、每天开始的未开通预订台数。
func (s *Service) UpcomingBookings(ctx context.Context, from time.Time, days int) ([]int, error) {
	start, err := calendarRange(from, days)
	if err != nil {
		return nil, err
	}
	rows, err := s.d.Pool.Query(ctx, `
		SELECT (SELECT count(*) FROM instances i WHERE i.state = 'reserved'
		          AND i.scheduled_start_at >= d.day AND i.scheduled_start_at < d.day + interval '24 hours')
		FROM generate_series($1::timestamptz, $1::timestamptz + ($2::int - 1) * interval '24 hours', interval '24 hours') AS d(day) ORDER BY d.day`, start, days)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int])
}

// MachineStates 批量读取机器状态（预订列表里标出钉在 faulty / maintenance 机器上的预订，便于宽限期前手动改派）。
func (s *Service) MachineStates(ctx context.Context, ids []int64) (map[int64]string, error) {
	out := map[int64]string{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.d.Pool.Query(ctx, `SELECT id, state FROM machines WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var st string
		if err := rows.Scan(&id, &st); err != nil {
			return nil, err
		}
		out[id] = st
	}
	return out, rows.Err()
}

// UpcomingBookingsForMachine 机器上未开通预订的台数（进入维护时提示）。
func (s *Service) UpcomingBookingsForMachine(ctx context.Context, machineID int64) (int, error) {
	var n int
	err := s.d.Pool.QueryRow(ctx, `SELECT count(*) FROM instances WHERE machine_id = $1 AND state = 'reserved'`, machineID).Scan(&n)
	return n, err
}

// AdminReassign 后台提前改派：machineID 为 nil 时同机组自动挑（不要求在线）；指定时目标必须空闲 / 已分配且时段无冲突（任意机组）。
func (s *Service) AdminReassign(ctx context.Context, actor audit.Actor, id string, machineID *int64, reason string) (Instance, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return Instance{}, apperr.BadRequest("请填写改派原因")
	}
	now := s.d.Clock.Now()
	var out Instance
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		inst, err := lockInstance(ctx, tx, id)
		if err != nil {
			return err
		}
		if inst.State != StateReserved || !inst.Booked() {
			return apperr.ErrInvalidState
		}
		lease, err := lockLease(ctx, tx, inst.ID)
		if err != nil {
			return err
		}
		from := inst.MachineID
		if machineID == nil {
			moved, err := s.reassign(ctx, tx, &inst, &lease, "admin: "+reason, false, now)
			if err != nil {
				return err
			}
			if !moved {
				return apperr.ErrMachineUnavailable
			}
		} else if err := s.reassignTo(ctx, tx, &inst, &lease, *machineID, "admin: "+reason, now); err != nil {
			return err
		}
		if err := audit.Record(ctx, tx, now, actor.Entry("booking.reassign", "instance", inst.ID,
			map[string]any{"from_machine": from, "to_machine": inst.MachineID, "reason": reason})); err != nil {
			return err
		}
		out, err = lockInstance(ctx, tx, inst.ID)
		return err
	})
	return out, err
}

// reassignTo 改派到指定机器：机器存在、同商品、idle / allocated、与 lease 时段无冲突（排他约束兜底）。
// 与自动挑选不对称（有意）：不要求 agent_online（提前改派，开通时再看），也允许 visible=false 机组的机器（运维留池）；后台页面的说明文字同此。
func (s *Service) reassignTo(ctx context.Context, tx pgx.Tx, inst *Instance, lease *leaseRow, target int64, reason string, now time.Time) error {
	if target == inst.MachineID {
		return apperr.ErrMachineUnavailable
	}
	var ok bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM machines m WHERE m.id = $1 AND m.product_id = $2 AND m.state IN ('idle', 'allocated')
			AND NOT EXISTS (SELECT 1 FROM machine_leases l WHERE l.machine_id = m.id AND l.status IN ('reserved', 'active')
			                AND l.period && tstzrange($3::timestamptz, $4::timestamptz)))`,
		target, inst.ProductID, lease.Lower, lease.Upper).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return apperr.ErrMachineUnavailable
	}
	newLease, err := insertLease(ctx, tx, target, inst.ID, lease.Lower, lease.Upper, "reserved", now)
	if db.IsExclusionViolation(err) {
		return apperr.ErrMachineUnavailable
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE machine_leases SET status = 'cancelled', updated_at = $2 WHERE id = $1`, lease.ID, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE instances SET machine_id = $2, reassign_count = reassign_count + 1, updated_at = $3 WHERE id = $1`, inst.ID, target, now); err != nil {
		return err
	}
	if err := recordEvent(ctx, tx, inst.ID, inst.State, inst.State, fmt.Sprintf("reassigned %d -> %d: %s", inst.MachineID, target, reason), now); err != nil {
		return err
	}
	inst.MachineID, inst.ReassignCount = target, inst.ReassignCount+1
	lease.ID, lease.MachineID = newLease, target
	return nil
}

// MachineLeases 机器从 from 起 days 天内的全部 reserved / active 时段（后台视图，含实例与用户）。
func (s *Service) MachineLeases(ctx context.Context, machineID int64, from time.Time, days int) ([]CalendarPeriod, error) {
	return s.MachineCalendar(ctx, machineID, 0, from, days, true)
}
```

`internal/rental/admin_queries.go` 的 `OrderFilter` 加 `BatchID string`，`where()` 加：

```go
	if f.BatchID != "" {
		w.Add("batch_id = ?", f.BatchID)
	}
```

- [ ] **Step 4: `adminapi/bookings.go`**

```go
package adminapi

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/httpx"
	"spark-cloud/internal/platform/tz"
	"spark-cloud/internal/rental"
)

func (s *Server) bookingRoutes(g chi.Router) {
	s.handle(g, http.MethodGet, "/bookings", allRoles, s.handleListBookings)
	s.handle(g, http.MethodGet, "/bookings/upcoming", allRoles, s.handleUpcomingBookings)
	s.handle(g, http.MethodPost, "/instances/{id}/cancel-booking", opsRoles, s.handleCancelBooking)
	s.handle(g, http.MethodPost, "/instances/{id}/reassign", opsRoles, s.handleReassign)
	s.handle(g, http.MethodGet, "/machines/{id}/calendar", opsRoles, s.handleMachineCalendar)
}

func (s *Server) handleListBookings(w http.ResponseWriter, r *http.Request) {
	p, err := pageOf(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	uid, err := queryInt(r, "user_id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	gid, err := queryInt(r, "group_id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	mid, err := queryInt(r, "machine_id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	from, to, err := parseRange(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	list, total, err := s.Rental.AdminListBookings(r.Context(), rental.BookingFilter{State: r.URL.Query().Get("state"), UserID: uid, GroupID: gid, MachineID: mid, From: from, To: to}, p)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	ids := make([]int64, 0, len(list))
	for _, i := range list {
		ids = append(ids, i.MachineID)
	}
	states, err := s.Rental.MachineStates(r.Context(), ids)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	views := make([]bookingView, 0, len(list))
	for _, i := range list {
		views = append(views, bookingView{instanceView: s.instanceView(i), MachineState: states[i.MachineID]})
	}
	httpx.WriteJSON(w, http.StatusOK, httpx.Page(views, total))
}

// bookingView 预订列表行：实例视图加当前机器状态（faulty / maintenance 上的预订需要运维在宽限期前改派）。
type bookingView struct {
	instanceView
	MachineState string `json:"machine_state"`
}

type upcomingDay struct {
	Date  string `json:"date"`
	Count int    `json:"count"`
}

// upcoming 从今天（北京时间）起 days 天。
func (s *Server) upcoming(r *http.Request, days int) ([]upcomingDay, error) {
	today := tz.DayStart(s.Clock.Now())
	counts, err := s.Rental.UpcomingBookings(r.Context(), today, days)
	if err != nil {
		return nil, err
	}
	out := make([]upcomingDay, len(counts))
	for i, n := range counts {
		out[i] = upcomingDay{Date: today.Add(time.Duration(i) * 24 * time.Hour).In(tz.CST).Format("2006-01-02"), Count: n}
	}
	return out, nil
}

func (s *Server) handleUpcomingBookings(w http.ResponseWriter, r *http.Request) {
	days := 7
	if v := r.URL.Query().Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 61 {
			s.writeError(w, r, apperr.BadRequest("days 需在 1～61 之间"))
			return
		}
		days = n
	}
	out, err := s.upcoming(r, days)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"days": out})
}

func (s *Server) handleCancelBooking(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Reason string `json:"reason"`
		TOTP   string `json:"totp"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.stepUp(r, req.TOTP); err != nil {
		s.writeError(w, r, err)
		return
	}
	refunded, err := s.Rental.AdminCancelBooking(r.Context(), s.actor(r), chi.URLParam(r, "id"), req.Reason)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]int64{"refunded": refunded})
}

func (s *Server) handleReassign(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MachineID *int64 `json:"machine_id"`
		Reason    string `json:"reason"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		s.writeError(w, r, err)
		return
	}
	inst, err := s.Rental.AdminReassign(r.Context(), s.actor(r), chi.URLParam(r, "id"), req.MachineID, req.Reason)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, s.instanceView(inst))
}

func (s *Server) handleMachineCalendar(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	q := r.URL.Query()
	from := s.Clock.Now()
	if v := q.Get("from"); v != "" {
		t, err := time.ParseInLocation("2006-01-02", v, tz.CST)
		if err != nil {
			s.writeError(w, r, apperr.BadRequest("from 的格式应为 2006-01-02"))
			return
		}
		from = t.UTC()
	}
	days := 61
	if v := q.Get("days"); v != "" {
		if days, err = strconv.Atoi(v); err != nil {
			s.writeError(w, r, apperr.BadRequest("days 必须是整数"))
			return
		}
	}
	out, err := s.Rental.MachineLeases(r.Context(), id, from, days)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"periods": out})
}
```

- [ ] **Step 5: 视图、机器详情、维护响应、仪表盘、订单筛选 / CSV**

`internal/adminapi/views.go`：`instanceView` 在 `PasswordAuth` 之后加

```go
	ScheduledStartAt      *time.Time `json:"scheduled_start_at"`
	ScheduledEndAt        *time.Time `json:"scheduled_end_at"`
	GroupID               *int64     `json:"group_id"`
	BatchID               *string    `json:"batch_id"`
	ReassignCount         int        `json:"reassign_count"`
	AutoRenew             bool       `json:"auto_renew"`
	AutoRenewDuration     string     `json:"auto_renew_duration"`
	AutoRenewFailedReason string     `json:"auto_renew_failed_reason"`
	ProvisionedAt         *time.Time `json:"provisioned_at"`
	CancelledAt           *time.Time `json:"cancelled_at"`
```

`instanceView()` 构造后补：

```go
	v.ScheduledStartAt, v.GroupID, v.BatchID, v.ReassignCount = i.ScheduledStartAt, i.GroupID, i.BatchID, i.ReassignCount
	v.AutoRenew, v.AutoRenewDuration, v.AutoRenewFailedReason = i.AutoRenew, string(i.AutoRenewDuration), i.AutoRenewFailedReason
	v.ProvisionedAt, v.CancelledAt = i.ProvisionedAt, i.CancelledAt
	if i.ScheduledStartAt != nil {
		end := i.ScheduledStartAt.Add(time.Duration(i.PendingSeconds) * time.Second)
		v.ScheduledEndAt = &end
	}
```

（把现有的单行 `return instanceView{...}` 改成 `v := instanceView{...}` + 上述补充 + `return v`。）

`orderView` 加 `Quantity int \`json:"quantity"\``、`BatchID *string \`json:"batch_id"\``、`Auto bool \`json:"auto"\``，`toOrderView` 复制。

`internal/adminapi/machines.go` 的 `handleMachine`：在返回前加

```go
	var group any
	if m.GroupID != nil {
		if g, err := s.Fleet.GetGroup(r.Context(), s.Fleet.Pool(), *m.GroupID); err == nil {
			group = g
		}
	}
	leases, err := s.Rental.MachineLeases(r.Context(), id, s.Clock.Now(), 60)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	upcoming, err := s.Rental.UpcomingBookingsForMachine(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"machine": m, "current_instance": current, "history": history,
		"group": group, "leases": leases, "upcoming_bookings": upcoming})
```

（替换原来的 `httpx.WriteJSON(...)`。）`handleMaintenance` 的响应改为：

```go
	upcoming, err := s.Rental.UpcomingBookingsForMachine(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, struct {
		fleet.Machine
		UpcomingBookings int `json:"upcoming_bookings"`
	}{m, upcoming})
```

`internal/adminapi/dashboard.go`：在 `util` 计算之后加

```go
	upcoming, err := s.upcoming(r, 7)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	upcoming7 := 0
	for _, d := range upcoming {
		upcoming7 += d.Count
	}
```

并在响应 map 里加 `"bookings_upcoming_7d": upcoming7`。

`internal/adminapi/finance.go`：`orderFilter` 返回值加 `BatchID: q.Get("batch_id")`；`handleExportOrders` 表头改为 `[]string{"订单号", "用户ID", "类型", "实例", "租期", "数量", "金额(元)", "已退(元)", "状态", "批次", "自动续费", "下单时间"}`，行改为：

```go
				batch := ""
				if o.BatchID != nil {
					batch = *o.BatchID
				}
				auto := ""
				if o.Auto {
					auto = "是"
				}
				return write([]string{o.ID, strconv.FormatInt(o.UserID, 10), o.Kind, o.InstanceID, string(o.Duration), strconv.Itoa(o.Quantity),
					csvx.Yuan(o.Amount), csvx.Yuan(o.RefundedAmount), o.Status, batch, auto, csvx.Time(o.CreatedAt)})
```

`internal/adminapi/server.go` 的 `Router()`：`s.groupRoutes(g)` 之后加 `s.bookingRoutes(g)`。

- [ ] **Step 6: 运行测试确认通过**

Run: `go vet ./... && go test ./internal/rental/ ./internal/adminapi/ 2>&1 | tail -6`
Expected: 全部 `ok`。现有 `finance_test` 的 CSV 表头断言若写死了旧列，按新表头更新（只允许这一处改动）。

- [ ] **Step 7: 提交**

```bash
git add internal/rental/admin_bookings.go internal/rental/admin_queries.go internal/adminapi/bookings.go internal/adminapi/bookings_test.go internal/adminapi/server.go internal/adminapi/views.go internal/adminapi/machines.go internal/adminapi/dashboard.go internal/adminapi/finance.go internal/adminapi/permissions_test.go internal/adminapi/finance_test.go
git commit -m "feat(adminapi): bookings list/upcoming, cancel-booking with totp, reassign, machine leases, dashboard and order batch columns

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 14: 清扫顺序整合、短信模板配置、README 配置表与部署注意

**Files:**
- Modify: `internal/rental/sweeper.go`（`Sweep` 顺序）、`internal/platform/config/config.go`（`AliyunConfig` 两个模板、`checkPlugins`）、`internal/platform/config/config_test.go`（追加）、`internal/app/app.go`（阿里云模板映射）、`README.md`
- Test: `internal/rental/sweeper_test.go`（追加 `TestSweepOrderProvisionsBeforeExpiry`）

**Interfaces:**
- Consumes: Task 2 `RefreshCappedLeases`、Task 8 `ProvisionDue`、Task 10 `AutoRenewDue`。
- Produces:
  - `rental.Sweep` 顺序：离线判定 → 暂停 → 恢复 → `RefreshCappedLeases` → `ProvisionDue` → `AutoRenewDue` → `ExpireDue` → `WipeStale` → `FailStuckCreating` → `FailTimedOutCommands` → `ReapSSHSessions` → `SendReminders`。
  - `config.AliyunConfig.SMSTplAutoRenewFailed`、`SMSTplBookingCancelled`（环境变量 `SPARK_SMS_TPL_AUTO_RENEW_FAILED`、`SPARK_SMS_TPL_BOOKING_CANCELLED`；`SPARK_SMS_DRIVER=aliyun` 时必填）。
  - README 配置表两行、「部署注意」两条（一次性重启全部副本；预订功能的阿里云模板）。

- [ ] **Step 1: 写失败的测试**

`internal/rental/sweeper_test.go` 追加：

```go
// 同一轮 Sweep：到期前 1 小时的自动续费先于到期停机；到点的预订在同一轮里开通。
func TestSweepOrderProvisionsAndAutoRenewsBeforeExpiry(t *testing.T) {
	e := newEnv(t, 2)
	_, running := e.place(catalog.D1d)
	e.ack(true)
	e.autoRenewOn(running.ID, catalog.D1d)
	booked := e.book(dbtest.T0.Add(25 * time.Hour)) // 开通时刻 T0 + 24h
	e.clock.Advance(24 * time.Hour)                   // running 恰好到期；自动续费本该在 1 小时前触发，这里让两者同一轮
	e.heartbeatAll()
	if err := e.svc.Sweep(e.ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.get(running.ID); got.State != rental.StateRunning || !got.ExpiresAt.Equal(dbtest.T0.Add(48*time.Hour)) {
		t.Fatalf("auto renew must run before expiry: %+v", got)
	}
	if got := e.get(booked.ID); got.State != rental.StateCreating {
		t.Fatalf("booking must be provisioned in the same sweep: %+v", got)
	}
}
```

`internal/platform/config/config_test.go` 追加（参照文件里已有的「aliyun 缺变量」用例写法，`load` helper 以文件现有的为准）：

```go
func TestAliyunRequiresBookingTemplates(t *testing.T) {
	env := map[string]string{
		"SPARK_DATABASE_URL": "postgres://x", "SPARK_SECRET_KEY": "MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE=",
		"SPARK_SMS_DRIVER": "aliyun", "SPARK_ALIYUN_ACCESS_KEY_ID": "k", "SPARK_ALIYUN_ACCESS_KEY_SECRET": "s",
		"SPARK_SMS_SIGN": "sign", "SPARK_SMS_TPL_LOGIN": "L", "SPARK_SMS_TPL_EXPIRY": "E",
	}
	_, err := config.Load(func(k string) string { return env[k] })
	if err == nil || !strings.Contains(err.Error(), "SPARK_SMS_TPL_AUTO_RENEW_FAILED") || !strings.Contains(err.Error(), "SPARK_SMS_TPL_BOOKING_CANCELLED") {
		t.Fatalf("want missing template error, got %v", err)
	}
	env["SPARK_SMS_TPL_AUTO_RENEW_FAILED"], env["SPARK_SMS_TPL_BOOKING_CANCELLED"] = "A", "B"
	cfg, err := config.Load(func(k string) string { return env[k] })
	if err != nil || cfg.Aliyun.SMSTplAutoRenewFailed != "A" || cfg.Aliyun.SMSTplBookingCancelled != "B" {
		t.Fatalf("load = %+v, %v", cfg.Aliyun, err)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/rental/ ./internal/platform/config/ -run 'TestSweepOrder|TestAliyunRequiresBookingTemplates' 2>&1 | tail -8`
Expected: `TestSweepOrder…` 失败（running 被 `ExpireDue` 停机）；config 编译错误。

- [ ] **Step 3: `Sweep` 顺序**

`internal/rental/sweeper.go` 的 `Sweep`：在 `s.resumeOnline(ctx)` 之后、`s.ExpireDue(ctx)` 之前插入：

```go
	if _, err := s.RefreshCappedLeases(ctx); err != nil {
		errs = append(errs, fmt.Errorf("refresh capped leases: %w", err))
	}
	if _, err := s.ProvisionDue(ctx); err != nil {
		errs = append(errs, fmt.Errorf("provision: %w", err))
	}
	if _, err := s.AutoRenewDue(ctx); err != nil {
		errs = append(errs, fmt.Errorf("auto renew: %w", err))
	}
```

- [ ] **Step 4: 配置与模板映射**

`internal/platform/config/config.go`：`AliyunConfig` 加 `SMSTplAutoRenewFailed string` 与 `SMSTplBookingCancelled string`；`Load` 里 `Aliyun: AliyunConfig{...}` 加 `SMSTplAutoRenewFailed: getenv("SPARK_SMS_TPL_AUTO_RENEW_FAILED"), SMSTplBookingCancelled: getenv("SPARK_SMS_TPL_BOOKING_CANCELLED"),`；`checkPlugins` 的 aliyun 短信 `requireAll` 追加两对 `"SPARK_SMS_TPL_AUTO_RENEW_FAILED", a.SMSTplAutoRenewFailed, "SPARK_SMS_TPL_BOOKING_CANCELLED", a.SMSTplBookingCancelled`。

`internal/app/app.go` 的 `Templates` map 加：

```go
					sms.TemplateAutoRenewFailed:  cfg.Aliyun.SMSTplAutoRenewFailed,
					sms.TemplateBookingCancelled: cfg.Aliyun.SMSTplBookingCancelled,
```

- [ ] **Step 5: README**

配置表 `SPARK_SMS_SIGN / …` 那一行之后加：

```markdown
| `SPARK_SMS_TPL_AUTO_RENEW_FAILED` / `SPARK_SMS_TPL_BOOKING_CANCELLED` | 自动续费失败模板（参数 instance、reason）、预订被平台取消模板（参数 instance、reason）；`SPARK_SMS_DRIVER=aliyun` 时必填 | 空 |
```

「部署注意」末尾加两条：

```markdown
- 计划 6（档期预订）发布时**一次性重启全部 platform 副本**：旧副本分配机器时不看占用时段，滚动期间可能把带预订的机器分给即时订单。本次不改 Agent 协议，Agent 不需要升级。
- 预订与自动续费的短信模板（`SPARK_SMS_TPL_AUTO_RENEW_FAILED`、`SPARK_SMS_TPL_BOOKING_CANCELLED`）在阿里云侧申请通过后再切 `SPARK_SMS_DRIVER=aliyun`，否则启动校验失败。
```

- [ ] **Step 6: 运行测试确认通过**

Run: `go vet ./... && go test ./... 2>&1 | tail -15`
Expected: 全部 `ok`（含 `internal/app` 的 `TestFullRentalLifecycle`：`Sweep` 多了三个任务，现有流程不受影响）。

- [ ] **Step 7: 提交**

```bash
git add internal/rental/sweeper.go internal/rental/sweeper_test.go internal/platform/config/config.go internal/platform/config/config_test.go internal/app/app.go README.md
git commit -m "feat(booking): sweep order (capped leases, provision, auto-renew before expiry), aliyun booking sms templates, README

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 15: 接入层：路由表含 reserved、sshgw Banner 与终态集合、ingress 状态页、authorize 状态列表

**Files:**
- Modify: `internal/rental/ports.go`（`IngressRoute.ScheduledStartAt`、`ListIngressRoutes`）、`internal/sshgw/auth.go`（`BannerReservedFmt`、`stateBanner`）、`internal/sshgw/lifecycle.go`（`terminalState`）、`internal/sshgw/export_test.go`（`TerminalStateForTest`）、`internal/ingress/pages.go`（`PageReservedFmt`、`NotRunningPage`）、`internal/api/ingress.go`（authorize 的状态列表）
- Test: `internal/rental/ports_test.go`（追加）、`internal/sshgw/auth_test.go`（`TestBannerByState` 追加两例）、`internal/sshgw/lifecycle_terminal_test.go`（新建）、`internal/ingress/pages_test.go`（`TestNotRunningPage` 追加两例）

**Interfaces:**
- Consumes: Task 1 `StateReserved / StateCancelled`、`Instance.ScheduledStartAt`；Task 2 `tz.Format`。
- Produces:
  - `rental.IngressRoute.ScheduledStartAt *time.Time`；`ListIngressRoutes` 状态列表 `('reserved', 'creating', 'running', 'stopped')`。
  - `sshgw.BannerReservedFmt = "实例预订于 %s 开始，开始前 1 小时自动开通"`（`%s` 为 `tz.Format`，缺失为 `—`）；`stateBanner` 显式 `reserved` 分支；`terminalState` 含 `cancelled`。
  - `ingress.PageReservedFmt`（同文案）；`NotRunningPage` 显式 `reserved` 分支（`ScheduledStartAt` 为 nil 时 `—`）。
  - authorize 对 `reserved` 实例不再回 410（按可用状态签票，入口页会显示 503 状态页）。

- [ ] **Step 1: 写失败的测试**

`internal/rental/ports_test.go` 追加：

```go
func TestIngressRoutesIncludeReservedWithStart(t *testing.T) {
	e := newEnv(t, 1)
	start := dbtest.T0.Add(3 * time.Hour)
	inst := e.book(start)
	if _, err := e.svc.AddPort(e.ctx, e.userID, inst.ID, 8888, "private", "lab", ""); err != nil {
		t.Fatal(err)
	}
	routes, err := e.svc.ListIngressRoutes(e.ctx)
	if err != nil || len(routes) != 1 || routes[0].State != rental.StateReserved || routes[0].ScheduledStartAt == nil || !routes[0].ScheduledStartAt.Equal(start) {
		t.Fatalf("routes = %+v, %v", routes, err)
	}
	if _, err := e.svc.Cancel(e.ctx, e.userID, inst.ID, ""); err != nil {
		t.Fatal(err)
	}
	if routes, _ = e.svc.ListIngressRoutes(e.ctx); len(routes) != 0 {
		t.Fatalf("cancelled instance must leave the route table: %+v", routes)
	}
}
```

`internal/sshgw/auth_test.go` 的 `TestBannerByState` cases 追加：

```go
		{"reserved", func(i *rental.AccessInfo) {
			i.Instance.State = rental.StateReserved
			s := time.Date(2026, 3, 4, 3, 0, 0, 0, time.UTC)
			i.Instance.ScheduledStartAt = &s
		}, "实例预订于 2026-03-04 11:00 开始，开始前 1 小时自动开通"},
		{"cancelled", func(i *rental.AccessInfo) { i.Instance.State = rental.StateCancelled }, sshgw.BannerReleased},
```

`internal/sshgw/lifecycle_terminal_test.go`：

```go
package sshgw_test

import (
	"testing"

	"spark-cloud/internal/rental"
	"spark-cloud/internal/sshgw"
)

func TestTerminalStatesIncludeCancelled(t *testing.T) {
	want := map[string]bool{
		rental.StateReserved: false, rental.StateCreating: false, rental.StateRunning: false,
		rental.StateStopped: true, rental.StateWiping: true, rental.StateReleased: true, rental.StateFailed: true, rental.StateCancelled: true,
	}
	for state, terminal := range want {
		if got := sshgw.TerminalStateForTest(state); got != terminal {
			t.Fatalf("terminalState(%s) = %v, want %v", state, got, terminal)
		}
	}
}
```

`internal/ingress/pages_test.go` 的 `TestNotRunningPage` cases 追加：

```go
		{rental.IngressRoute{State: rental.StateReserved, ScheduledStartAt: ptrTime(time.Date(2026, 3, 4, 3, 0, 0, 0, time.UTC))}, "实例预订于 2026-03-04 11:00 开始，开始前 1 小时自动开通"},
		{rental.IngressRoute{State: rental.StateReserved}, "实例预订于 — 开始，开始前 1 小时自动开通"},
		{rental.IngressRoute{State: rental.StateCancelled}, ingress.PageReleased},
```

并在文件末尾加 `func ptrTime(t time.Time) *time.Time { return &t }`（import 加 `"time"`）。

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/rental/ ./internal/sshgw/ ./internal/ingress/ -run 'TestIngressRoutesIncludeReserved|TestBannerByState|TestTerminalStates|TestNotRunningPage' 2>&1 | head -20`
Expected: 编译错误（`ScheduledStartAt` 字段 / `TerminalStateForTest` 未定义）或断言失败。

- [ ] **Step 3: 代码**

`internal/rental/ports.go`：`IngressRoute` 加 `ScheduledStartAt *time.Time`；`ListIngressRoutes` 改为：

```go
func (s *Service) ListIngressRoutes(ctx context.Context) ([]IngressRoute, error) {
	rows, err := s.d.Pool.Query(ctx, `SELECT p.instance_id, i.user_id, p.port, p.visibility, i.state, i.suspended_at IS NOT NULL, m.lan_ip, i.ssh_host_pubkey, i.scheduled_start_at
		FROM instance_ports p JOIN instances i ON i.id = p.instance_id JOIN machines m ON m.id = i.machine_id
		WHERE i.state IN ('reserved', 'creating', 'running', 'stopped') ORDER BY p.instance_id, p.port`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (IngressRoute, error) {
		var x IngressRoute
		err := r.Scan(&x.InstanceID, &x.UserID, &x.Port, &x.Visibility, &x.State, &x.Suspended, &x.LanIP, &x.HostPubkey, &x.ScheduledStartAt)
		return x, err
	})
}
```

（`ports.go` import 加 `"time"`。）

`internal/sshgw/auth.go`：常量块加 `BannerReservedFmt = "实例预订于 %s 开始，开始前 1 小时自动开通"`；`stateBanner` 在 `case inst.State == rental.StateCreating:` 之前加：

```go
	case inst.State == rental.StateReserved:
		when := "—"
		if inst.ScheduledStartAt != nil {
			when = tz.Format(*inst.ScheduledStartAt)
		}
		return fmt.Sprintf(BannerReservedFmt, when)
```

（import 加 `"spark-cloud/internal/platform/tz"`；同一文件的 `stopped` 分支 `info.RetainUntil.In(cst).Format("2006-01-02 15:04")` 改为 `tz.Format(*info.RetainUntil)` 并删掉包级 `cst`，一个文件只留一个时区来源；`adminapi/finance.go` 的 `cst` 同理改用 `tz.CST`。）

`internal/sshgw/lifecycle.go` 的 `terminalState`：`case rental.StateStopped, rental.StateWiping, rental.StateReleased, rental.StateFailed, rental.StateCancelled:`。

`internal/sshgw/export_test.go` 追加：`var TerminalStateForTest = terminalState`。

`internal/ingress/pages.go`：常量块加 `PageReservedFmt = "实例预订于 %s 开始，开始前 1 小时自动开通"`；`NotRunningPage` 在 `case r.State == rental.StateCreating:` 之前加：

```go
	case r.State == rental.StateReserved:
		when := "—"
		if r.ScheduledStartAt != nil {
			when = tz.Format(*r.ScheduledStartAt)
		}
		return fmt.Sprintf(PageReservedFmt, when)
```

（import 加 `"spark-cloud/internal/platform/tz"`。）

`internal/api/ingress.go` 的状态 switch：`case rental.StateReserved, rental.StateCreating, rental.StateRunning, rental.StateStopped:`。

- [ ] **Step 4: 运行测试确认通过**

Run: `go vet ./... && go test ./internal/rental/ ./internal/sshgw/ ./internal/ingress/ ./internal/api/ 2>&1 | tail -8`
Expected: 全部 `ok`。

- [ ] **Step 5: 提交**

```bash
git add internal/rental/ports.go internal/rental/ports_test.go internal/sshgw/auth.go internal/sshgw/auth_test.go internal/sshgw/lifecycle.go internal/sshgw/export_test.go internal/sshgw/lifecycle_terminal_test.go internal/ingress/pages.go internal/ingress/pages_test.go internal/api/ingress.go
git commit -m "feat(access): reserved instances in route table, ssh banner and ingress page for bookings, cancelled as terminal state

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 16: Go 集成测试（并发批次一致性、HTTP 全链路、即时不抢预订机器）+ e2e 钩子（机组、机器入组）

**Files:**
- Create: `internal/rental/concurrency_test.go`、`internal/app/booking_e2e_test.go`
- Modify: `internal/e2ehooks/hooks.go`（`/hooks/machines` 的 `group_id`、新增 `POST /hooks/groups`）、`internal/e2ehooks/hooks_test.go`（追加）

**Interfaces:**
- Consumes: 全部 rental 功能；`app.Build`；`e2ehooks.Server`。
- Produces:
  - `POST /hooks/groups {name, visible?}` → `{id}`（`Fleet.CreateGroup`，`visible` 缺省 true）；`POST /hooks/machines` 请求体新增 `group_id?: number`（登记后 `Fleet.AssignMachines`）。
  - 测试：`TestConcurrentBatchesConsistent`（Review Focus 1）、`TestBookingLifecycleHTTP`、`TestImmediateOrderDoesNotTakeBookedMachine`。

- [ ] **Step 1: 并发一致性测试**

`internal/rental/concurrency_test.go`：

```go
package rental_test

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/rental"
)

// TestConcurrentBatchesConsistent：10 台机器、50 个并发批次（1～8 台、随机整点窗口、1d × 1～3）。
// 事后：同机 reserved / active 时段两两不交；批次订单数 == count；reserved 实例恰有一条 reserved lease；
// 失败只允许 no_capacity / booking_limit / machine_unavailable；整体在 60 秒内完成（没有死锁）。
func TestConcurrentBatchesConsistent(t *testing.T) {
	e := newEnv(t, 10)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	rng := rand.New(rand.NewSource(42))
	type job struct {
		uid   int64
		req   rental.BatchRequest
		start time.Time
	}
	jobs := make([]job, 50)
	for i := range jobs {
		uid := e.newUser(fmt.Sprintf("1370000%04d", i), 100_000_000)
		start := dbtest.T0.Add(time.Duration(3+rng.Intn(10*24)) * time.Hour)
		req := rental.BatchRequest{UserID: uid, ProductID: e.productID, ImageID: e.imageID, Count: 1 + rng.Intn(8),
			Duration: catalog.D1d, Quantity: 1 + rng.Intn(3), StartAt: &start, IdempotencyKey: fmt.Sprintf("c-%d", i)}
		if i%5 == 0 { // 五分之一是即时订单
			req.StartAt = nil
		}
		jobs[i] = job{uid: uid, req: req, start: start}
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	okCount, failCodes := 0, map[string]int{}
	for _, j := range jobs {
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			res, err := e.svc.PlaceBatch(ctx, j.req)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				okCount++
				// 排他约束已保证「同机时段不交」；真正能抓问题的是这两条：批次内机器两两不同、返回台数 == 批次 count
				seen := map[int64]bool{}
				for _, inst := range res.Instances {
					if seen[inst.MachineID] {
						t.Errorf("batch %s has two instances on machine %d", res.Batch.ID, inst.MachineID)
					}
					seen[inst.MachineID] = true
				}
				if len(res.Instances) != res.Batch.Count || len(res.Orders) != res.Batch.Count || res.Batch.Count != j.req.Count {
					t.Errorf("batch %s returned %d instances / %d orders for count %d", res.Batch.ID, len(res.Instances), len(res.Orders), res.Batch.Count)
				}
				return
			}
			ae := apperr.As(err)
			if ae == nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			failCodes[ae.Code]++
		}(j)
	}
	wg.Wait()
	for code := range failCodes {
		if code != "no_capacity" && code != "booking_limit" && code != "machine_unavailable" {
			t.Fatalf("unexpected failure code %s: %v", code, failCodes)
		}
	}
	if okCount == 0 {
		t.Fatalf("no batch succeeded: %v", failCodes)
	}
	var overlaps, badBatches, badReserved int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM machine_leases a JOIN machine_leases b ON a.machine_id = b.machine_id AND a.id < b.id
		WHERE a.status IN ('reserved', 'active') AND b.status IN ('reserved', 'active') AND a.period && b.period`).Scan(&overlaps); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM rental_batches b WHERE (SELECT count(*) FROM orders o WHERE o.batch_id = b.id) <> b.count
		OR (SELECT count(*) FROM instances i WHERE i.batch_id = b.id) <> b.count`).Scan(&badBatches); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM instances i WHERE i.state = 'reserved'
		AND (SELECT count(*) FROM machine_leases l WHERE l.instance_id = i.id AND l.status = 'reserved') <> 1`).Scan(&badReserved); err != nil {
		t.Fatal(err)
	}
	if overlaps != 0 || badBatches != 0 || badReserved != 0 {
		t.Fatalf("overlaps=%d badBatches=%d badReserved=%d (ok=%d fails=%v)", overlaps, badBatches, badReserved, okCount, failCodes)
	}
	t.Logf("ok=%d fails=%v", okCount, failCodes)
}
```

- [ ] **Step 2: HTTP 全链路与「即时不抢预订机器」**

`internal/app/booking_e2e_test.go`（复用 `e2e_test.go` 的 `client`、`upstreamKeyFile`）：

```go
package app_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"testing"
	"time"

	"spark-cloud/internal/app"
	"spark-cloud/internal/audit"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/config"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/plugins/sms"
)

type bookingEnv struct {
	t        *testing.T
	ctx      context.Context
	a        *app.App
	clk      *clock.Fake
	sms      *sms.Mock
	c        *client
	machines []int64
	products []any
	images   []any
}

func newBookingEnv(t *testing.T, machines int) *bookingEnv {
	t.Helper()
	ctx := context.Background()
	pool := dbtest.New(t)
	clk := clock.NewFake(dbtest.T0)
	smsMock := &sms.Mock{}
	cfg := config.Config{UpstreamSSHKeyFile: upstreamKeyFile(t), Env: "dev", UploadDir: t.TempDir(), Domain: "spark.test", SecretKey: dbtest.Key, SMSDriver: "mock", KYCDriver: "mock"}
	a, err := app.Build(cfg, pool, slog.New(slog.NewTextHandler(io.Discard, nil)), app.Options{Clock: clk, SMS: smsMock})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(a.Handler)
	t.Cleanup(ts.Close)
	jar, _ := cookiejar.New(nil)
	c := &client{t: t, base: ts.URL + "/api/v1", http: &http.Client{Jar: jar}}
	e := &bookingEnv{t: t, ctx: ctx, a: a, clk: clk, sms: smsMock, c: c}
	for i := 0; i < machines; i++ {
		e.machines = append(e.machines, dbtest.CreateMachine(t, pool, "SN00"+string(rune('1'+i))))
	}
	c.do("POST", "/auth/sms-code", map[string]string{"phone": "13800009999"})
	c.do("POST", "/auth/login", map[string]string{"phone": "13800009999", "code": "000000"})
	c.do("POST", "/kyc/personal", map[string]string{"name": "李四", "id_number": "110101199001011234"})
	rch := c.do("POST", "/recharges", map[string]any{"amount": 1_000_000, "channel": "wechat"})
	c.do("POST", "/dev/recharges/"+rch["id"].(string)+"/pay", map[string]any{})
	e.products = c.do("GET", "/products", nil)["items"].([]any)
	e.images = c.do("GET", "/images", nil)["items"].([]any)
	return e
}

// agent 模拟全部机器的 Agent：心跳并成功执行待处理指令。
func (e *bookingEnv) agent() {
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

func (e *bookingEnv) sweep() {
	e.t.Helper()
	for _, mid := range e.machines {
		if _, err := e.a.Fleet.Heartbeat(e.ctx, mid); err != nil {
			e.t.Fatal(err)
		}
	}
	if err := e.a.Rental.Sweep(e.ctx); err != nil {
		e.t.Fatal(err)
	}
}

func (e *bookingEnv) bookingBody(start time.Time, extra map[string]any) map[string]any {
	body := map[string]any{"product_id": e.products[0].(map[string]any)["id"], "image_id": e.images[0].(map[string]any)["id"],
		"count": 1, "duration": "1d", "quantity": 1, "start_at": start.Format(time.RFC3339)}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

func (e *bookingEnv) state(id string) map[string]any { return e.c.do("GET", "/instances/"+id, nil) }

// TestBookingLifecycleHTTP：预订 → 到点开通 → 运行（到期 = 开始 + 1d）→ 自动续费 → 同机下一预订挡住自动续费（短信）→ 到期停机 → 保留期洗机 → 下一预订按时开通。
func TestBookingLifecycleHTTP(t *testing.T) {
	e := newBookingEnv(t, 1)
	g, err := e.a.Fleet.CreateGroup(e.ctx, audit.Actor{AdminID: 1}, fleet.GroupInput{Name: "A 区", Visible: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.a.Fleet.AssignMachines(e.ctx, audit.Actor{AdminID: 1}, g.ID, e.machines); err != nil {
		t.Fatal(err)
	}
	start := dbtest.T0.Add(3 * time.Hour)
	out := e.c.do("POST", "/bookings", e.bookingBody(start, map[string]any{"group_id": g.ID, "auto_renew": true, "auto_renew_duration": "1m"}))
	id := out["instances"].([]any)[0].(map[string]any)["id"].(string)
	if st := e.state(id); st["state"] != "reserved" || st["group_name"] != "A 区" {
		t.Fatalf("reserved view = %v", st)
	}
	e.clk.Advance(2 * time.Hour)
	e.sweep()
	if st := e.state(id); st["state"] != "creating" {
		t.Fatalf("after T-1h sweep = %v", st)
	}
	e.clk.Advance(5 * time.Minute)
	e.agent()
	st := e.state(id)
	if st["state"] != "running" || st["remaining_seconds"].(float64) != 86400 || st["ssh_command"] == "" {
		t.Fatalf("after create = %v", st)
	}
	// 到 S：剩余仍是 24h（早开通不计费）
	e.clk.Advance(55 * time.Minute)
	if st := e.state(id); st["remaining_seconds"].(float64) != 86400 {
		t.Fatalf("at S = %v", st)
	}
	// 自动续费：到期前 1h，1m 放得下 → 到期 = S + 1d + 30d
	e.clk.Advance(23 * time.Hour)
	e.sweep()
	st = e.state(id)
	expires1, _ := time.Parse(time.RFC3339, st["expires_at"].(string))
	if !expires1.Equal(start.Add(31 * 24 * time.Hour)) {
		t.Fatalf("after auto renew expires = %v", expires1)
	}
	// 同机紧随其后的预订：S2 = expires1 + 8h → 可续至 = expires1；下一次自动续费全部被挡
	s2 := expires1.Add(8 * time.Hour)
	out2 := e.c.do("POST", "/bookings", e.bookingBody(s2, map[string]any{"group_id": g.ID}))
	id2 := out2["instances"].([]any)[0].(map[string]any)["id"].(string)
	if st := e.state(id); st["renewable_until"] == nil {
		t.Fatalf("renewable_until missing: %v", st)
	}
	e.clk.Advance(30 * 24 * time.Hour) // now = expires1 − 1h：自动续费到点
	e.sweep()
	st = e.state(id)
	if st["auto_renew_failed_reason"] != "renew_blocked" || e.smsCount(sms.TemplateAutoRenewFailed) != 1 { // 同一轮可能还有到期提醒短信，按模板计数
		t.Fatalf("blocked auto renew = %v sms=%+v", st, e.sms.Sent())
	}
	// 到期停机 → 6h 保留 → 洗机 → 释放；下一预订在 S2 − 1h 开通
	e.clk.Advance(time.Hour)
	e.sweep()
	e.agent()
	if e.smsCount(sms.TemplateAutoRenewFailed) != 1 { // 10 分钟重试间隔内到期：被挡的自动续费只发过一条短信（规格 S15）
		t.Fatalf("blocked auto renew must notify once, sms=%+v", e.sms.Sent())
	}
	if st := e.state(id); st["state"] != "stopped" {
		t.Fatalf("expired = %v", st)
	}
	e.clk.Advance(6 * time.Hour)
	e.sweep()
	e.agent()
	if st := e.state(id); st["state"] != "released" {
		t.Fatalf("wiped = %v", st)
	}
	e.clk.Advance(time.Hour) // = S2 − 1h
	e.sweep()
	if st := e.state(id2); st["state"] != "creating" {
		t.Fatalf("next booking at T-1h = %v", st)
	}
	e.agent()
	if st := e.state(id2); st["state"] != "running" {
		t.Fatalf("next booking running = %v", st)
	}
}

func TestImmediateOrderDoesNotTakeBookedMachine(t *testing.T) {
	e := newBookingEnv(t, 1)
	e.c.do("POST", "/bookings", e.bookingBody(dbtest.T0.Add(10*time.Hour), nil))
	body := map[string]any{"product_id": e.products[0].(map[string]any)["id"], "duration": "1d", "image_id": e.images[0].(map[string]any)["id"]}
	resp := e.c.raw("POST", "/orders", body)
	if resp["code"] != "no_capacity" {
		t.Fatalf("immediate order with near booking = %v", resp)
	}
	// 预订挪远（40h）后即时订单成功
	id := e.c.do("GET", "/instances", nil)["items"].([]any)[0].(map[string]any)["id"].(string)
	e.c.do("POST", "/instances/"+id+"/cancel", map[string]any{})
	e.c.do("POST", "/bookings", e.bookingBody(dbtest.T0.Add(40*time.Hour), nil))
	if out := e.c.do("POST", "/orders", body); out["instance"].(map[string]any)["state"] != "creating" {
		t.Fatalf("immediate order with far booking = %v", out)
	}
}
```

`e2e_test.go` 的 `client` 没有「允许非 2xx」的方法：在 `booking_e2e_test.go` 末尾补一个：

```go
// raw 与 do 相同但不因非 2xx 失败，返回响应体。
func (c *client) raw(method, path string, body any) map[string]any {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+path, rd)
	if method != http.MethodGet {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out
}
```

（文件 import 加 `"bytes"`、`"encoding/json"`。）

- [ ] **Step 3: e2e 钩子**

`internal/e2ehooks/hooks.go`：`Handler()` 加 `mux.HandleFunc("POST /hooks/groups", s.handle(s.groups))`；`machines` 请求体加 `GroupID *int64 \`json:"group_id"\``，循环末尾（`ids = append(...)` 之前）加：

```go
		if req.GroupID != nil {
			if err := s.App.Fleet.AssignMachines(ctx, audit.Actor{}, *req.GroupID, []int64{m.ID}); err != nil {
				return nil, err
			}
		}
```

新增：

```go
// groups 建一个机组（visible 缺省 true）。
func (s *Server) groups(ctx context.Context, body json.RawMessage) (any, error) {
	req, err := decode[struct {
		Name    string `json:"name"`
		Visible *bool  `json:"visible"`
	}](body)
	if err != nil {
		return nil, err
	}
	visible := req.Visible == nil || *req.Visible
	g, err := s.App.Fleet.CreateGroup(ctx, audit.Actor{}, fleet.GroupInput{Name: req.Name, Visible: visible})
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": g.ID}, nil
}
```

`internal/e2ehooks/hooks_test.go` 追加一个用例（参照文件里已有的钩子测试写法调用 `Handler()`）：`POST /hooks/groups {"name":"A"}` 返回 200 与 `id`；`POST /hooks/machines {"count":1,"group_id":<id>}` 后 `machines.group_id` 等于该 id。

- [ ] **Step 4: 运行测试确认通过**

Run: `go vet ./... && go test ./internal/rental/ -run TestConcurrentBatchesConsistent -count=1 -race 2>&1 | tail -4 && go test ./internal/app/ ./internal/e2ehooks/ 2>&1 | tail -4`
Expected: 全部 `ok`；并发用例在 60 秒内完成，日志打印 `ok=N fails=map[...]`。

- [ ] **Step 5: 提交**

```bash
git add internal/rental/concurrency_test.go internal/app/booking_e2e_test.go internal/e2ehooks/hooks.go internal/e2ehooks/hooks_test.go
git commit -m "test(booking): concurrent batch consistency, HTTP booking lifecycle, immediate orders respect bookings; e2e hooks for groups

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 17: 真机检查点 A（192.168.1.221）——后端 + API：到点开通、即时被挡、取消退款、续费被挡、自动续费、接入层 Banner

**Files:**
- Create: `docs/superpowers/acceptance-plan6.md`（本任务写「检查点 A」；Task 22 补齐）
- 可能修改：验收中发现的缺陷在对应包修复并补单测（单独提交）

**Interfaces:**
- Consumes: Task 1～16 的 platform；Agent 不需要升级（无协议改动），沿用计划 3 验收时安装的版本。
- 前提与分工：平台跑在开发 Mac（`MAC=$(ipconfig getifaddr en0)`，验收时是 192.168.1.218），Agent 跑在 Spark。**测试机上需要 root 的命令由协调人按「执行须知」的模式经 ssh 执行**（远端 `read -r P; s() {...}` 函数，密码只在运行时的环境变量里，绝不写进任何文件 / 记录 / 提交）。本检查点不需要 root：只用 `ssh imac@192.168.1.221 'docker ps'` 之类的只读命令。DNS 对 `*.sslip.io` 返回假地址：所有 curl 都加 `--resolve`。开发环境短信验证码 60 秒一次。

- [ ] **Step 0: 准备**

```bash
export PATH=$HOME/.local/go/bin:$HOME/.local/bin:$PATH
MAC=$(ipconfig getifaddr en0); DOM=$(echo $MAC | tr . -).sslip.io; echo $MAC $DOM
make devdb &
SPARK_DATABASE_URL=postgres://postgres:postgres@localhost:5433/spark?sslmode=disable \
SPARK_SECRET_KEY=MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE= \
SPARK_DOMAIN=$DOM SPARK_PUBLIC_BASE_URL=http://$DOM:8080 SPARK_SSH_ADDR=:2222 SPARK_SSH_PUBLIC_PORT=2222 \
SPARK_AGENT_ADDR=:9443 SPARK_AGENT_HOSTS=$MAC SPARK_UPSTREAM_SSH_KEY_FILE=$PWD/.devdata/upstream.key SPARK_AGENT_DATA_SIZE_GIB=200 \
SPARK_SWEEP_INTERVAL=1m go run ./cmd/platform
# 另一终端：psql 与 curl 的快捷方式
PSQL=".devdata/runtime/bin/psql postgres://postgres:postgres@localhost:5433/spark"
API="curl -s --resolve $DOM:8080:$MAC -b .devdata/cj -c .devdata/cj -H 'Content-Type: application/json'"
```

Agent 应已连上（后台机器 1 `connected=true`）；不在线时按计划 3 验收记录重新执行接入命令（需要 root，协调人执行）。用户登录：`$API -X POST http://$DOM:8080/api/v1/auth/sms-code -d '{"phone":"13800000001"}'` → `/auth/login` 填 `000000`（60 秒内不要重复发码）。实名与充值（mock 支付）照 `internal/app/booking_e2e_test.go` 的顺序用 curl 做一遍。后台管理员用 `platform admin create-admin`（计划 4）或已有账号登录 `http://127.0.0.1:8081/admin/`。

- [ ] **Step 1: 机组与展示名（§16.4 第 1 项，API 部分）**

后台 API：`POST /admin/api/v1/groups {"name":"A 区","region":"一楼","visible":true}` → `POST /groups/<id>/machines {"machine_ids":[1]}` → `PUT /machines/1 {"display_name":"A-01"}`。控制台 API：`GET /api/v1/groups` 列出 `A 区`（`machine_count 1, idle_now 1`）；`GET /api/v1/groups/<id>/machines` 的 `label` 为 `A-01`；`GET /api/v1/groups/<id>/calendar?days=3` 三天 `free 1 / total 1`。

- [ ] **Step 2: 到点开通（§16.4 第 2 项）——需要真实等待约 1 小时**

`S` = 当前时间 + 2h 向上取整到整点（北京时间），`POST /api/v1/bookings {"product_id":1,"image_id":<id>,"group_id":<gid>,"machine_id":1,"count":1,"duration":"1d","quantity":1,"start_at":"<S RFC3339>"}` → 201，实例 `reserved`，`cancellable_until = S − 1h`，`machine_label = A-01`；`SELECT status, lower(period), upper(period) FROM machine_leases WHERE instance_id='<id>'` 为 `reserved [S−1h, S+1d+7h)`；后台 `GET /admin/api/v1/machines/1` 的 `leases` 有这一条。等到 `S − 1h`（看 platform 日志的 sweep；±1 分钟）：实例 `creating`、`provisioned_at` 已写、机器 `allocated`、Spark 上 `docker ps` 出现容器；运行中后 `expires_at = S + 1d`（后台核对），此时 SSH 已可登录但 `remaining_seconds = 86400` 不递减；到 `S` 后开始递减。记录实际开通时刻与 `S − 1h` 的偏差。

- [ ] **Step 3: 即时订单被预订挡住（§16.4 第 3 项）**

在第 2 步开通前（或另订一个 10 小时后的预订）：用另一账号 `POST /api/v1/orders {"product_id":1,"duration":"1d","image_id":<id>}` → 409 `no_capacity`（机器在 1d + 7h 内有预订）；`GET /api/v1/products` 的 `idle_count` 为 0。取消该预订（`POST /instances/<id>/cancel`）→ 再下即时单 → 201；随后释放它。

- [ ] **Step 4: 取消退款（§16.4 第 4 项）**

预订 `S = now + 30h` → `GET /instances/<id>/cancel-quote` → `percent 100`；等 16 分钟再查（已过冷静期但 ≥ 24h）仍 100 → 取消 → `wallet/ledger` 出现 `refund` 全额。预订 `S = now + 3h` → 立刻查 quote 100（冷静期）→ 等 16 分钟 → quote 50 → 取消 → 退一半，`orders` 状态 `partially_refunded`。后台 `GET /admin/api/v1/audit?action=booking.cancel` 有两条。

- [ ] **Step 5: 续费被挡 + 可续至（§16.4 第 5 项）**

第 2 步的实例运行中（`expires ≈ S + 1d`）。再在 `A-01` 上预订 `S3 = S + 3d`（`machine_id: 1`）→ `GET /instances/<id>` 的 `renewable_until = S3 − 8h`，`renew_options` 为 `1d: 1, 1w: 0, 1m: 0`；`POST /instances/<id>/renew {"duration":"1w"}` → 409 `renew_blocked`，消息「该机器自 <S3 北京时间> 起已有预订，本次最多可续至 <S3 − 8h>」；余额不变；`{"duration":"1d"}` → 200。随后取消 `S3` 预订（为第 6 步腾出余量）。

- [ ] **Step 6: 自动续费（§16.4 第 6 项）**

`PUT /instances/<id>/auto-renew {"enabled":true,"duration":"1d"}` → `$PSQL -c "UPDATE instances SET expires_at = now() + interval '65 minutes' WHERE id='<id>'"` → 2 分钟内 platform 日志出现 `auto renew`，`GET /orders` 新增 `kind=renew, auto=true` 的订单，`expires_at` 延后 1 天，mock 短信日志没有失败记录。把余额调为 0（后台 `POST /users/<uid>/adjust` 负数或 `$PSQL` 直接改 `wallet_accounts`，记录做法）并再把 `expires_at` 改到 65 分钟后 → 实例 `auto_renew_failed_reason = insufficient_balance`，platform 日志有 mock 短信 `auto_renew_failed`（只一条；再等 2 分钟仍只一条）；充值后下一轮成功、失败字段清空。

- [ ] **Step 7: 接入层（§16.4 第 8 项）**

对一个 `reserved` 实例：`ssh -p 2222 <id>@$MAC`（`HostKeyAlias=ssh.$DOM`）看到 Banner「实例预订于 <S> 开始，开始前 1 小时自动开通」；给它加端口 8888 后 `curl --resolve ins-<id>-8888.$DOM:8080:$MAC http://ins-<id>-8888.$DOM:8080/` 返回 503 页同文案；开通并 running 后两者恢复。取消后的实例 Banner「实例已释放」。

- [ ] **Step 8: 对账无噪声（§16.4 第 9 项）**

`GET /admin/api/v1/alerts?resolved=false`：没有 `booking_*` 告警；改派链路（单机不可测）以 Task 16 集成测试与 Task 21 的 e2e 为准，记录里注明。

- [ ] **Step 9: 记录与提交**

把每项的命令、关键输出与结论写进 `docs/superpowers/acceptance-plan6.md`「检查点 A」（格式同 `acceptance-plan3.md`）；发现的缺陷修复后单独提交并在记录里写明提交号。写入前核对记录与提交里没有任何密码或 `SUDO_PW` 的值。

```bash
git add docs/superpowers/acceptance-plan6.md
git commit -m "docs: plan 6 real-machine checkpoint A (booking backend + API) on 192.168.1.221

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 18: 共享类型与文案、`logic/booking.ts`、`RentDialog`（机组 / 数量 / 时间 / 租期 × 数量 / 指定机器 / 自动续费 / 可用性 / 日历）、市场页接入

**Files:**
- Create: `web/shared/src/types/booking.ts`、`web/console/src/logic/booking.ts`、`web/console/src/logic/__tests__/booking.spec.ts`、`web/console/src/components/RentDialog.vue`
- Modify: `web/shared/src/types/instance.ts`、`web/shared/src/types/index.ts`、`web/shared/src/labels.ts`、`web/shared/src/__tests__/labels.spec.ts`（`auditActions` 长度 37 → 47，并 `toContain('booking.cancel')`）、`web/console/src/views/MarketView.vue`

**Interfaces:**
- Consumes: Task 12 的控制台 API。
- Produces:
  - `InstanceState` 增加 `'reserved' | 'cancelled'`；`Instance` 新字段 `scheduled_start_at, scheduled_end_at, cancellable_until: string | null; group_id: number | null; group_name: string; machine_label: string; batch_id: string | null; reassign_count: number; auto_renew: boolean; auto_renew_duration: Duration; auto_renew_failed_reason: string; renewable_until: string | null; renew_options: RenewOption[]`；`Order` 新字段 `quantity: number; batch_id: string | null; auto: boolean`。
  - `types/booking.ts`：`Group`、`GroupMachine`、`CalendarDay`、`CalendarPeriod`、`Availability`、`Batch`、`BookingResult`、`CancelQuote`、`RenewOption`、`UserEvent`。
  - `labels.ts`：`instanceStateLabel` 加 `reserved: '已预订'`、`cancelled: '已取消'`；`instanceStateType` 加 `reserved: 'primary'`、`cancelled: 'info'`；`alertKindLabel` 加 `booking_at_risk: '预订机器不可用'`、`booking_unfulfilled: '预订无法履约'`、`booking_cancel_failed: '预订取消失败'`；`auditActions` 加 `group.create / group.update / group.delete / group.assign / group.unassign / machine.update / booking.cancel / booking.admin_cancel / booking.reassign / instance.auto_renew`。
  - `logic/booking.ts`：`MAX_COUNT = 8`、`MAX_QUANTITY`、`earliestStart(now: Date): Date`、`latestStart(now: Date): Date`、`cstDay(t: Date): string`、`cstHour(t: Date): number`、`hourOptions(day: string, now: Date): number[]`、`startAtISO(day: string, hour: number): string`、`totalDays(duration, quantity): number`、`describeDuration(duration, quantity): string`、`totalPrice(unit, quantity, count): number`、`isLegacyImmediate(form): boolean`。
  - `RentDialog.vue`：props `product: Product; images: Image[]; mode: 'immediate' | 'booking'; duration: Duration; imageId: number | null`，`v-model`（visible）；事件 `placed(result: { instances: { id: string }[]; batchId: string | null })`；内部按 §11.1 的 9 个步骤；默认单台即时且未选机组 / 指定机器时走 `POST /orders`（保持旧接口与现有 e2e），否则 `POST /bookings`；最早 / 最晚整点按 `GET /groups` 返回的服务器时间 `now` 计算（`serverOffset`）。

- [ ] **Step 1: 写失败的测试**

`web/console/src/logic/__tests__/booking.spec.ts`：

```ts
import { describe, expect, it } from 'vitest'
import { cstDay, cstHour, describeDuration, earliestStart, hourOptions, isLegacyImmediate, latestStart, startAtISO, totalDays, totalPrice } from '../booking'

describe('booking time helpers (U-20)', () => {
  // 2026-01-01 00:00 UTC = 北京时间 08:00
  const now = new Date('2026-01-01T00:00:00Z')
  it('最早整点 = now + 2h 向上取整；正好整点不再加', () => {
    expect(earliestStart(now).toISOString()).toBe('2026-01-01T02:00:00.000Z')
    expect(earliestStart(new Date('2026-01-01T00:00:01Z')).toISOString()).toBe('2026-01-01T03:00:00.000Z')
    expect(latestStart(now).toISOString()).toBe('2026-03-02T00:00:00.000Z')
  })
  it('北京时间日期与小时', () => {
    expect(cstDay(now)).toBe('2026-01-01')
    expect(cstHour(now)).toBe(8)
    expect(cstDay(new Date('2026-01-01T17:00:00Z'))).toBe('2026-01-02')
  })
  it('当天可选小时：今天只从最早整点起；60 天那天只到最晚整点', () => {
    expect(hourOptions('2026-01-01', now)).toEqual([10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23])
    expect(hourOptions('2026-01-02', now)).toHaveLength(24)
    expect(hourOptions('2026-03-02', now)).toEqual([0, 1, 2, 3, 4, 5, 6, 7, 8])
    expect(hourOptions('2026-03-03', now)).toEqual([])
    expect(hourOptions('2025-12-31', now)).toEqual([])
  })
  it('RFC3339 带 +08:00', () => {
    expect(startAtISO('2026-01-02', 9)).toBe('2026-01-02T09:00:00+08:00')
  })
  it('租期 × 数量', () => {
    expect(totalDays('1m', 2)).toBe(60)
    expect(totalDays('1w', 3)).toBe(21)
    expect(describeDuration('1m', 2)).toBe('1 月 × 2 = 60 天')
    expect(describeDuration('1d', 1)).toBe('1 天')
    expect(totalPrice(6900, 3, 2)).toBe(41400)
  })
  it('单台即时且未选机组 / 机器走旧接口', () => {
    expect(isLegacyImmediate({ immediate: true, count: 1, quantity: 1, groupId: null, machineId: null, autoRenew: false })).toBe(true)
    expect(isLegacyImmediate({ immediate: true, count: 1, quantity: 1, groupId: null, machineId: null, autoRenew: true })).toBe(false)
    expect(isLegacyImmediate({ immediate: true, count: 2, quantity: 1, groupId: null, machineId: null, autoRenew: false })).toBe(false)
    expect(isLegacyImmediate({ immediate: false, count: 1, quantity: 1, groupId: null, machineId: null, autoRenew: false })).toBe(false)
  })
})
```

- [ ] **Step 2: 运行测试确认失败**

Run: `cd web && npx vitest run console/src/logic/__tests__/booking.spec.ts 2>&1 | tail -5`
Expected: 模块不存在。

- [ ] **Step 3: 共享类型与文案**

`web/shared/src/types/instance.ts`：

```ts
import type { Duration } from './catalog'

export type InstanceState = 'reserved' | 'creating' | 'running' | 'stopped' | 'wiping' | 'released' | 'failed' | 'cancelled'

export interface RenewOption {
  duration: Duration
  quantity_max: number
}

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
  password_auth: boolean
  ssh_host: string
  ssh_port: number
  ports_count: number
  scheduled_start_at: string | null
  scheduled_end_at: string | null
  cancellable_until: string | null
  group_id: number | null
  group_name: string
  machine_label: string
  batch_id: string | null
  reassign_count: number
  auto_renew: boolean
  auto_renew_duration: Duration
  auto_renew_failed_reason: string
  renewable_until: string | null
  renew_options: RenewOption[]
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
  quantity: number
  amount: number
  status: OrderStatus
  refunded_amount: number
  batch_id: string | null
  auto: boolean
  created_at: string
}
```

`web/shared/src/types/booking.ts`：

```ts
import type { Instance, Order } from './instance'

export interface Group {
  id: number
  name: string
  region: string
  description: string
  machine_count: number
  idle_now: number
}

export interface GroupMachine {
  id: number
  label: string
  busy_until: string | null
}

export interface CalendarDay {
  date: string
  free: number
  total: number
}

export interface CalendarPeriod {
  lower: string
  upper: string
  mine: boolean
  instance_id?: string
  user_id?: number
}

export interface Availability {
  available: number
  lower: string
  upper: string
}

export interface Batch {
  id: string
  count: number
  created_at: string
}

export interface BookingResult {
  batch: Batch | null
  orders: Order[]
  instances: Instance[]
}

export interface CancelQuote {
  refund: number
  percent: number
  deadline: string
  max: number
}

export interface UserEvent {
  to_state: string
  reason_kind: string
  at: string
  detail: string
}
```

`web/shared/src/types/index.ts` 加 `export * from './booking'`。

`web/shared/src/labels.ts`：`instanceStateLabel` 改为 `{ reserved: '已预订', creating: '开通中', running: '运行中', stopped: '已到期停机', wiping: '洗机中', released: '已释放', failed: '开通失败', cancelled: '已取消' }`；`instanceStateType` 改为 `{ reserved: 'primary', creating: 'primary', running: 'success', stopped: 'warning', wiping: 'info', released: 'info', failed: 'danger', cancelled: 'info' }`；`alertKindLabel` 末尾加 `booking_at_risk: '预订机器不可用', booking_unfulfilled: '预订无法履约', booking_cancel_failed: '预订取消失败'`；`auditActions` 在 `'image.create', 'image.update',` 之后加 `'group.create', 'group.update', 'group.delete', 'group.assign', 'group.unassign', 'machine.update', 'booking.cancel', 'booking.admin_cancel', 'booking.reassign', 'instance.auto_renew',`。

- [ ] **Step 4: `logic/booking.ts`**

```ts
import type { Duration } from '@spark/shared'

export const MAX_COUNT = 8
export const MAX_QUANTITY: Record<Duration, number> = { '1d': 90, '1w': 12, '1m': 3 }
export const DAYS_OF: Record<Duration, number> = { '1d': 1, '1w': 7, '1m': 30 }
const HOUR = 3600_000
const CST_OFFSET = 8 * HOUR
const MIN_LEAD = 2 * HOUR
const MAX_AHEAD = 60 * 24 * HOUR

/** 最早可预订的整点：now + 2h 向上取整到整点（北京时间整点与 UTC 整点重合）。 */
export function earliestStart(now: Date): Date {
  return new Date(Math.ceil((now.getTime() + MIN_LEAD) / HOUR) * HOUR)
}
/** 最晚可预订的整点：now + 60d 向下取整到整点。 */
export function latestStart(now: Date): Date {
  return new Date(Math.floor((now.getTime() + MAX_AHEAD) / HOUR) * HOUR)
}
/** 北京时间日期 YYYY-MM-DD。 */
export function cstDay(t: Date): string {
  return new Date(t.getTime() + CST_OFFSET).toISOString().slice(0, 10)
}
/** 北京时间小时 0～23。 */
export function cstHour(t: Date): number {
  return new Date(t.getTime() + CST_OFFSET).getUTCHours()
}
/** 某一天（北京时间）里可选的整点：在 [earliestStart, latestStart] 内。 */
export function hourOptions(day: string, now: Date): number[] {
  const lo = earliestStart(now).getTime()
  const hi = latestStart(now).getTime()
  const out: number[] = []
  for (let h = 0; h < 24; h++) {
    const t = new Date(startAtISO(day, h)).getTime()
    if (t >= lo && t <= hi) out.push(h)
  }
  return out
}
/** RFC3339（+08:00），后端按整点校验。 */
export function startAtISO(day: string, hour: number): string {
  return `${day}T${String(hour).padStart(2, '0')}:00:00+08:00`
}
export function totalDays(duration: Duration, quantity: number): number {
  return DAYS_OF[duration] * quantity
}
export function describeDuration(duration: Duration, quantity: number): string {
  const label = { '1d': '1 天', '1w': '1 周', '1m': '1 月' }[duration]
  return quantity === 1 ? label : `${label} × ${quantity} = ${totalDays(duration, quantity)} 天`
}
export function totalPrice(unit: number, quantity: number, count: number): number {
  return unit * quantity * count
}

export interface RentForm {
  immediate: boolean
  count: number
  quantity: number
  groupId: number | null
  machineId: number | null
  autoRenew: boolean
}
/** 与计划 5 完全相同的请求（单台、一个单位、立即、不限机组、不开自动续费）走旧接口 POST /orders。 */
export function isLegacyImmediate(f: RentForm): boolean {
  return f.immediate && f.count === 1 && f.quantity === 1 && f.groupId === null && f.machineId === null && !f.autoRenew
}
```

- [ ] **Step 5: `RentDialog.vue`**

`web/console/src/components/RentDialog.vue`：

```vue
<script setup lang="ts">
import {
  durationLabel, fmtTime, fmtYuan, isApiError, labelOf, useIdempotencyKey,
  type Availability, type BookingResult, type CalendarDay, type CalendarPeriod, type Duration, type Group, type GroupMachine, type Image, type Instance, type Listed, type Order, type Product,
} from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, reactive, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api } from '../api'
import { useGuide } from '../composables/useGuide'
import { cstDay, describeDuration, earliestStart, hourOptions, isLegacyImmediate, MAX_COUNT, MAX_QUANTITY, startAtISO, totalPrice } from '../logic/booking'
import { notifyError } from '../logic/notify'
import { refreshBalance, session } from '../store/session'
import AppDialog from './AppDialog.vue'
import GuideDialog from './GuideDialog.vue'

const props = defineProps<{ product: Product; images: Image[]; mode: 'immediate' | 'booking'; duration: Duration; imageId: number | null }>()
const visible = defineModel<boolean>({ required: true })
const emit = defineEmits<{ placed: [result: { instances: { id: string }[]; batchId: string | null }] }>()

const route = useRoute()
const router = useRouter()
const guide = useGuide()

// ---- 表单 ----
const form = reactive({
  groupId: null as number | null,
  count: 1,
  immediate: props.mode === 'immediate',
  day: '',
  hour: null as number | null,
  duration: props.duration,
  quantity: 1,
  imageId: props.imageId,
  machineId: null as number | null,
  autoRenew: false,
  autoRenewDuration: props.duration,
})
const step = ref<'form' | 'confirm'>(props.mode === 'immediate' ? 'confirm' : 'form')
const expanded = ref(props.mode === 'booking')
const groups = ref<Group[]>([])
const serverOffset = ref(0) // 服务器时间 − 浏览器时间：最早 / 最晚整点按服务器时间算
const nowServer = () => new Date(Date.now() + serverOffset.value)
const machines = ref<GroupMachine[]>([])
const machinePeriods = ref<CalendarPeriod[]>([])
const calendar = ref<CalendarDay[]>([])
const availability = ref<Availability | null>(null)
const submitting = ref(false)

const NO_GROUP = 0 // el-option 的 value 不能是 null：0 作为「不限机组」哨兵，提交时映射回 null
const groupSel = computed({ get: () => form.groupId ?? NO_GROUP, set: (v: number) => { form.groupId = v === NO_GROUP ? null : v } })
const unitPrice = computed(() => props.product.prices.find((p) => p.duration === form.duration)?.amount ?? null)
const quantityMax = computed(() => MAX_QUANTITY[form.duration])
const total = computed(() => (unitPrice.value == null ? null : totalPrice(unitPrice.value, form.quantity, form.count)))
const balance = computed(() => session.balance ?? 0)
const after = computed(() => (total.value == null ? null : balance.value - total.value))
const insufficient = computed(() => total.value != null && balance.value < total.value)
const imageName = computed(() => props.images.find((i) => i.id === form.imageId)?.name ?? '')
const groupName = computed(() => groups.value.find((g) => g.id === form.groupId)?.name ?? '不限机组')
const machineLabel = computed(() => machines.value.find((m) => m.id === form.machineId)?.label ?? '')
const startAt = computed(() => (form.immediate || !form.day || form.hour == null ? null : startAtISO(form.day, form.hour)))
const hours = computed(() => (form.day ? hourOptions(form.day, nowServer()) : []))
const canSpecifyMachine = computed(() => form.count === 1 && form.groupId !== null)
const notEnough = computed(() => availability.value != null && availability.value.available < form.count)
const legacy = computed(() => isLegacyImmediate({ immediate: form.immediate, count: form.count, quantity: form.quantity, groupId: form.groupId, machineId: form.machineId, autoRenew: form.autoRenew }))
const key = useIdempotencyKey(() => [props.product.id, form.groupId, form.count, form.immediate, form.day, form.hour, form.duration, form.quantity, form.imageId, form.machineId, form.autoRenew, form.autoRenewDuration])

watch(() => form.duration, (d) => {
  if (form.quantity > MAX_QUANTITY[d]) form.quantity = MAX_QUANTITY[d]
  form.autoRenewDuration = d
})
watch(() => form.count, () => {
  if (form.count !== 1) form.machineId = null
})
watch(() => form.groupId, () => {
  form.machineId = null
  machines.value = []
  if (form.groupId !== null) void loadGroupDetails()
})
watch(() => form.immediate, (imm) => {
  if (!imm && !form.day) {
    const e = earliestStart(nowServer())
    form.day = cstDay(e)
    form.hour = hourOptions(form.day, nowServer())[0] ?? null
  }
})
watch(() => form.day, () => {
  const opts = hours.value
  if (form.hour == null || !opts.includes(form.hour)) form.hour = opts[0] ?? null
})
watch(() => form.machineId, (id) => {
  machinePeriods.value = []
  if (id !== null) void loadMachinePeriods(id)
})

async function loadGroups() {
  try {
    const out = await api.get<Listed<Group> & { now: string }>('/groups', { product_id: props.product.id })
    groups.value = out.items
    serverOffset.value = Date.parse(out.now) - Date.now()
    if (!form.immediate && !form.day) {
      const e = earliestStart(nowServer())
      form.day = cstDay(e)
      form.hour = hourOptions(form.day, nowServer())[0] ?? null
    }
  } catch (e) {
    notifyError(e)
  }
}
async function loadGroupDetails() {
  const gid = form.groupId
  if (gid === null) return
  try {
    const [m, c] = await Promise.all([api.get<Listed<GroupMachine>>(`/groups/${gid}/machines`), api.get<{ days: CalendarDay[] }>(`/groups/${gid}/calendar`, { days: 61 })])
    if (form.groupId !== gid) return
    machines.value = m.items
    calendar.value = c.days
  } catch (e) {
    notifyError(e)
  }
}
async function loadMachinePeriods(id: number) {
  try {
    const out = await api.get<{ periods: CalendarPeriod[] }>(`/machines/${id}/calendar`, { days: 61 })
    if (form.machineId === id) machinePeriods.value = out.periods
  } catch (e) {
    notifyError(e)
  }
}

// ---- 可用性：参数变化后 300ms 查一次，只采用最后一次 ----
let availSeq = 0
let availTimer: ReturnType<typeof setTimeout> | undefined
async function loadAvailability() {
  const mine = ++availSeq
  try {
    const out = await api.get<Availability>('/availability', {
      product_id: props.product.id, group_id: form.groupId ?? undefined, machine_id: form.machineId ?? undefined,
      start_at: startAt.value ?? undefined, duration: form.duration, quantity: form.quantity,
    })
    if (mine === availSeq) availability.value = out
  } catch (e) {
    if (mine === availSeq) availability.value = null
    if (!(isApiError(e) && e.code.startsWith('booking_'))) notifyError(e)
  }
}
watch([() => form.groupId, () => form.machineId, () => form.immediate, startAt, () => form.duration, () => form.quantity], () => {
  clearTimeout(availTimer)
  availTimer = setTimeout(() => void loadAvailability(), 300)
})

watch(visible, (v) => {
  if (!v) return
  step.value = props.mode === 'immediate' ? 'confirm' : 'form'
  expanded.value = props.mode === 'booking'
  Object.assign(form, { groupId: null, count: 1, immediate: props.mode === 'immediate', duration: props.duration, quantity: 1, imageId: props.imageId, machineId: null, autoRenew: false, autoRenewDuration: props.duration })
  availability.value = null
  form.day = ''
  form.hour = null
  void loadGroups() // 拿到服务器时间后再填默认开始时刻
  void loadAvailability()
}, { immediate: true })

function pickDay(d: CalendarDay) {
  if (d.free === 0) return
  form.day = d.date
}

function toConfirm() {
  if (!form.immediate && !startAt.value) return void ElMessage.warning('请选择开始时间')
  step.value = 'confirm'
}

async function submit() {
  if (submitting.value || form.imageId == null) return
  if (insufficient.value) {
    visible.value = false
    await router.push({ path: '/wallet/recharge', query: { redirect: route.fullPath } })
    return
  }
  submitting.value = true
  try {
    let placed: { instances: { id: string }[]; batchId: string | null }
    if (legacy.value) {
      const out = await api.post<{ order: Order; instance: Instance }>('/orders', { product_id: props.product.id, duration: form.duration, image_id: form.imageId }, { idempotencyKey: key.current() })
      placed = { instances: [out.instance], batchId: null }
    } else {
      const out = await api.post<BookingResult>('/bookings', {
        product_id: props.product.id, image_id: form.imageId, group_id: form.groupId, machine_id: form.machineId, count: form.count,
        duration: form.duration, quantity: form.quantity, start_at: startAt.value ?? undefined, auto_renew: form.autoRenew, auto_renew_duration: form.autoRenewDuration,
      }, { idempotencyKey: key.current() })
      placed = { instances: out.instances, batchId: out.batch?.id ?? null }
    }
    key.rotate()
    visible.value = false
    emit('placed', placed)
  } catch (e) {
    if (isApiError(e) && (e.code === 'no_capacity' || e.code === 'machine_unavailable')) {
      ElMessage.warning(e.message)
      step.value = 'form'
      void loadAvailability()
    } else if (isApiError(e) && e.code.startsWith('booking_')) {
      ElMessage.warning(e.message)
      step.value = 'form'
    } else {
      if (isApiError(e) && ['kyc_required', 'insufficient_balance', 'banned'].includes(e.code)) visible.value = false
      guide.handle(e)
    }
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <AppDialog v-model="visible" kind="form" :title="step === 'confirm' ? '确认下单' : '租用 / 预订'" :data-testid="step === 'confirm' ? 'market-confirm-dialog' : 'rent-dialog'">
    <template v-if="step === 'form'">
      <el-form label-position="top">
        <el-form-item label="机组">
          <el-select v-model="groupSel" data-testid="rent-group" placeholder="不限机组">
            <el-option :value="NO_GROUP" label="不限机组" />
            <el-option v-for="g in groups" :key="g.id" :value="g.id" :label="`${g.name}${g.region ? ' · ' + g.region : ''}（共 ${g.machine_count} · 现空闲 ${g.idle_now}）`" />
          </el-select>
        </el-form-item>
        <el-form-item label="数量（台）">
          <el-input-number v-model="form.count" :min="1" :max="MAX_COUNT" data-testid="rent-count" />
        </el-form-item>
        <el-form-item label="开始">
          <el-radio-group v-model="form.immediate">
            <el-radio-button :value="true" data-testid="rent-start-now">立即</el-radio-button>
            <el-radio-button :value="false" data-testid="rent-start-later">指定时间</el-radio-button>
          </el-radio-group>
          <div v-if="!form.immediate" class="row">
            <input v-model="form.day" type="date" class="el-input__inner native-date" data-testid="rent-day" />
            <el-select v-model="form.hour" data-testid="rent-hour" placeholder="整点" style="width: 110px">
              <el-option v-for="h in hours" :key="h" :value="h" :label="`${String(h).padStart(2, '0')}:00`" />
            </el-select>
            <span class="muted">北京时间整点；最早 2 小时后，最多提前 60 天</span>
          </div>
        </el-form-item>
        <el-form-item label="租期">
          <el-radio-group v-model="form.duration">
            <el-radio-button v-for="pr in product.prices" :key="pr.duration" :value="pr.duration" :data-testid="`rent-duration-${pr.duration}`">{{ labelOf(durationLabel, pr.duration) }} {{ fmtYuan(pr.amount) }}</el-radio-button>
          </el-radio-group>
          <div class="row">
            <span>× </span><el-input-number v-model="form.quantity" :min="1" :max="quantityMax" data-testid="rent-quantity" />
            <span class="muted" data-testid="rent-duration-text">{{ describeDuration(form.duration, form.quantity) }}（单次最长 90 天）</span>
          </div>
        </el-form-item>
        <el-form-item label="镜像">
          <el-select v-model="form.imageId" data-testid="rent-image"><el-option v-for="img in images" :key="img.id" :label="img.name" :value="img.id" /></el-select>
        </el-form-item>
        <el-form-item v-if="canSpecifyMachine" label="指定机器（可选）">
          <el-select v-model="form.machineId" clearable placeholder="由系统分配" data-testid="rent-machine">
            <el-option v-for="m in machines" :key="m.id" :value="m.id" :label="m.busy_until ? `${m.label}（占用至 ${fmtTime(m.busy_until)}）` : m.label" />
          </el-select>
          <ul v-if="form.machineId !== null" class="periods" data-testid="rent-machine-periods">
            <li v-for="p in machinePeriods" :key="p.lower" :class="{ mine: p.mine }">{{ fmtTime(p.lower) }} ～ {{ fmtTime(p.upper) }}{{ p.mine ? '（我的）' : '' }}</li>
            <li v-if="!machinePeriods.length" class="muted">未来 60 天无占用</li>
          </ul>
        </el-form-item>
        <el-form-item label="自动续费">
          <el-switch v-model="form.autoRenew" data-testid="rent-auto-renew" />
          <el-select v-if="form.autoRenew" v-model="form.autoRenewDuration" data-testid="rent-auto-renew-duration" style="width: 110px; margin-left: 8px">
            <el-option v-for="pr in product.prices" :key="pr.duration" :value="pr.duration" :label="labelOf(durationLabel, pr.duration)" />
          </el-select>
          <span class="muted">到期前 1 小时自动扣款；被后续预订挡住时按能放下的档位办理</span>
        </el-form-item>
      </el-form>
      <p class="avail" :class="{ warn: notEnough }" data-testid="rent-availability">
        <template v-if="availability">该时段可{{ form.immediate ? '租' : '预订' }} {{ availability.available }} 台<template v-if="notEnough">，不足 {{ form.count }} 台，请减少数量或更换时段</template></template>
        <template v-else>正在查询可用机器…</template>
      </p>
      <div v-if="!form.immediate && form.groupId !== null && calendar.length" class="calendar" data-testid="rent-calendar">
        <button v-for="d in calendar" :key="d.date" type="button" class="day" :class="{ full: d.free === 0, picked: d.date === form.day }" :data-testid="`rent-day-${d.date}`" :disabled="d.free === 0" @click="pickDay(d)">
          <span class="date">{{ d.date.slice(5) }}</span><span class="free">{{ d.free }}/{{ d.total }}</span>
        </button>
      </div>
      <template #footer>
        <el-button data-testid="rent-cancel" @click="visible = false">取消</el-button>
        <el-button type="primary" :disabled="notEnough || !availability" data-testid="rent-next" @click="toConfirm">下一步</el-button>
      </template>
    </template>

    <template v-else>
      <el-descriptions :column="1" border>
        <el-descriptions-item label="商品"><span data-testid="market-confirm-product">{{ product.name }}</span></el-descriptions-item>
        <el-descriptions-item label="机组"><span data-testid="market-confirm-group">{{ groupName }}<template v-if="machineLabel"> · {{ machineLabel }}</template></span></el-descriptions-item>
        <el-descriptions-item label="数量"><span data-testid="market-confirm-count">{{ form.count }} 台</span></el-descriptions-item>
        <el-descriptions-item label="开始"><span data-testid="market-confirm-start">{{ form.immediate ? '立即' : `${form.day} ${String(form.hour ?? 0).padStart(2, '0')}:00` }}</span></el-descriptions-item>
        <el-descriptions-item label="租期"><span data-testid="market-confirm-duration">{{ form.quantity === 1 ? labelOf(durationLabel, form.duration) : describeDuration(form.duration, form.quantity) }}</span></el-descriptions-item>
        <el-descriptions-item label="镜像"><span data-testid="market-confirm-image">{{ imageName }}</span></el-descriptions-item>
        <el-descriptions-item label="价格"><span data-testid="market-confirm-price">{{ total == null ? '—' : fmtYuan(total) }}</span></el-descriptions-item>
        <el-descriptions-item label="当前余额"><span data-testid="market-confirm-balance">{{ fmtYuan(balance) }}</span></el-descriptions-item>
        <el-descriptions-item label="扣款后余额">
          <span data-testid="market-confirm-after" :class="{ warn: insufficient }">{{ after == null ? '—' : fmtYuan(after) }}<template v-if="insufficient">（余额不足）</template></span>
        </el-descriptions-item>
      </el-descriptions>
      <el-button v-if="!expanded" link type="primary" data-testid="market-confirm-more" @click="expanded = true; step = 'form'">更多选项（机组 / 多台 / 预订 / 自动续费）</el-button>
      <template #footer>
        <el-button v-if="expanded" data-testid="market-confirm-back" @click="step = 'form'">上一步</el-button>
        <el-button data-testid="market-confirm-cancel" @click="visible = false">取消</el-button>
        <el-button type="primary" :loading="submitting" data-testid="market-confirm-ok" @click="submit">{{ insufficient ? '去充值' : '确认下单' }}</el-button>
      </template>
    </template>
  </AppDialog>
  <!-- 放在 AppDialog 外（fragment）：el-dialog 带 destroy-on-close，余额不足时先关对话框再弹引导，放在里面会随插槽销毁 -->
  <GuideDialog :guide="guide" />
</template>

<style scoped>
.row { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; margin-top: 8px; }
.muted { color: #909399; font-size: 13px; }
.warn { color: var(--el-color-warning); }
.avail { margin: 8px 0; font-weight: 600; }
.native-date { height: 32px; border: 1px solid #dcdfe6; border-radius: 4px; padding: 0 8px; font-size: 14px; background: #fff; }
.calendar { display: grid; grid-template-columns: repeat(7, 1fr); gap: 4px; max-height: 220px; overflow-y: auto; }
.day { border: 1px solid #dcdfe6; border-radius: 4px; background: #fff; padding: 4px 0; display: flex; flex-direction: column; align-items: center; cursor: pointer; font-size: 12px; }
.day.full { color: #c0c4cc; background: #f5f7fa; cursor: not-allowed; }
.day.picked { border-color: var(--el-color-primary); color: var(--el-color-primary); }
.periods { list-style: none; padding: 0; margin: 6px 0 0; font-size: 12px; color: #606266; }
.periods .mine { color: var(--el-color-primary); }
</style>
```

- [ ] **Step 6: 市场页接入**

`web/console/src/views/MarketView.vue`：删除原来的确认框逻辑（`confirm` / `confirmSel` / `price` / `imageName` / `balance` / `after` / `insufficient` / `submitting` / `key` / `submit` 与模板里的 `AppDialog`），改为：

```ts
import RentDialog from '../components/RentDialog.vue'
// ...
const dialog = reactive<{ visible: boolean; product: Product | null; mode: 'immediate' | 'booking' }>({ visible: false, product: null, mode: 'immediate' })

async function open(p: Product, mode: 'immediate' | 'booking') {
  if (p.prices.length === 0) return
  if (session.status !== 'authed') {
    await router.push({ path: '/login', query: { redirect: route.fullPath } })
    return
  }
  if (session.user?.kyc_status !== 'verified') {
    guide.kycRequired()
    return
  }
  try {
    await refreshBalance()
  } catch (e) {
    guide.handle(e)
    return
  }
  dialog.product = p
  dialog.mode = mode
  dialog.visible = true
}
const rent = (p: Product) => open(p, 'immediate')
const book = (p: Product) => open(p, 'booking')

async function onPlaced(r: { instances: { id: string }[]; batchId: string | null }) {
  if (r.batchId && r.instances.length > 1) {
    ElMessage.success(`已下单 ${r.instances.length} 台`)
    await router.push({ path: '/instances', query: { batch: r.batchId } })
    return
  }
  ElMessage.success(dialog.mode === 'booking' ? '预订成功' : '下单成功，正在开通')
  await router.push(`/instances/${r.instances[0].id}`)
}
```

模板：卡片头部 `空闲 <b>X</b> 台` 改为 `现在可租 <b data-testid="market-idle">{{ p.idle_count }}</b> 台`；按钮区：

```vue
          <el-button type="primary" class="rent" data-testid="market-rent-btn" :disabled="p.prices.length === 0 || p.idle_count === 0" @click="rent(p)">
            {{ p.prices.length === 0 ? '暂未开放' : p.idle_count === 0 ? '暂无空闲' : '立即租用' }}
          </el-button>
          <el-button class="rent" data-testid="market-book-btn" :disabled="p.prices.length === 0" @click="book(p)">预订档期</el-button>
```

`AppDialog` 处替换为：

```vue
    <RentDialog v-if="dialog.product" v-model="dialog.visible" :product="dialog.product" :images="images" :mode="dialog.mode" :duration="sel[dialog.product.id].duration" :image-id="sel[dialog.product.id].imageId" @placed="onPlaced" />
```

（`GuideDialog` 仍由市场页渲染一次用于未实名 / 余额引导；`RentDialog` 内部的 `GuideDialog` 处理下单错误。）

- [ ] **Step 7: 运行测试确认通过**

Run: `cd web && npm test 2>&1 | tail -8 && npm run build 2>&1 | tail -3`
Expected: Vitest 全绿（含 `booking.spec.ts`、既有 `instance.spec.ts` 因 `Instance` 新字段需在 `base` 里补齐：`scheduled_start_at: null, scheduled_end_at: null, cancellable_until: null, group_id: null, group_name: '', machine_label: '', batch_id: null, reassign_count: 0, auto_renew: false, auto_renew_duration: '1d', auto_renew_failed_reason: '', renewable_until: null, renew_options: []`）；`vue-tsc` 与 Vite 构建通过。

- [ ] **Step 8: 提交**

```bash
git add web/shared/src/types web/shared/src/labels.ts web/shared/src/__tests__/labels.spec.ts web/console/src/logic/booking.ts web/console/src/logic/__tests__ web/console/src/components/RentDialog.vue web/console/src/views/MarketView.vue
git commit -m "feat(console): booking types, RentDialog (group/count/time/quantity/machine/auto-renew/availability/calendar), market page

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 19: 控制台实例列表 / 详情 / 帮助页（预订状态、批次、取消、续费数量与可续至、自动续费、时间线）

**Files:**
- Modify: `web/console/src/logic/instance.ts`、`web/console/src/logic/__tests__/instance.spec.ts`、`web/console/src/views/InstancesView.vue`、`web/console/src/views/InstanceDetailView.vue`、`web/console/src/views/HelpView.vue`

**Interfaces:**
- Consumes: Task 12 API；Task 18 类型。
- Produces（`logic/instance.ts`）：`instanceTimeText` 增加 reserved（`<S> 开始 · 共 <D>`）与 cancelled（`已取消`）；`canRenew` 包含 `reserved`；`canCancel(i, nowMs): boolean`（reserved 且未过 `cancellable_until`）；`canSetAutoRenew(s)`（reserved / creating / running / stopped）；`autoRenewFailureText(i): string`（`insufficient_balance` → 「自动续费失败：余额不足，请充值后系统会自动重试」；`renew_blocked` → 「自动续费失败：机器已有后续预订，可续至 <renewable_until>」）；`renewQuantityMax(i, d): number`（无 `renew_options` 时按 90 天上限）。
- 页面行为按规格 §11.2～§11.4；`data-testid`：`instance-batch`、`instances-batch-head`、`instances-cancel-all`、`detail-group`、`detail-machine`、`detail-start`、`detail-end`、`detail-batch`、`detail-reassigned`、`detail-cancel-btn`、`detail-cancel-dialog`、`detail-cancel-text`、`detail-cancel-ok`、`detail-renew-quantity`、`detail-renew-until`、`detail-auto-renew`、`detail-auto-renew-duration`、`detail-auto-renew-failed`、`detail-timeline`、`detail-event`、`help-booking`、`help-batch`、`help-auto-renew`。

- [ ] **Step 1: 写失败的测试**

`web/console/src/logic/__tests__/instance.spec.ts` 追加（`base` 已按 Task 18 补齐新字段）：

```ts
describe('booking states (U-21)', () => {
  const reserved: Instance = { ...base, state: 'reserved', expires_at: null, remaining_seconds: 0, pending_seconds: 86400, ssh_command: '',
    scheduled_start_at: '2026-10-20T02:00:00Z', scheduled_end_at: '2026-10-21T02:00:00Z', cancellable_until: '2026-10-20T01:00:00Z',
    renewable_until: '2026-10-25T00:00:00Z', renew_options: [{ duration: '1d', quantity_max: 3 }, { duration: '1w', quantity_max: 0 }, { duration: '1m', quantity_max: 0 }] }
  it('预订 / 取消的时间文案', () => {
    expect(instanceTimeText(reserved, 0)).toBe('2026-10-20 10:00 开始 · 共 1 天')
    expect(instanceTimeText({ ...base, state: 'cancelled' }, 0)).toBe('已取消')
  })
  it('可取消：reserved 且未过截止', () => {
    expect(canCancel(reserved, Date.parse('2026-10-20T00:59:00Z'))).toBe(true)
    expect(canCancel(reserved, Date.parse('2026-10-20T01:00:00Z'))).toBe(false)
    expect(canCancel({ ...reserved, state: 'creating' }, 0)).toBe(false)
  })
  it('续费数量上限来自 renew_options', () => {
    expect(renewQuantityMax(reserved, '1d')).toBe(3)
    expect(renewQuantityMax(reserved, '1w')).toBe(0)
    expect(renewQuantityMax({ ...reserved, renew_options: [] }, '1m')).toBe(3)
  })
  it('自动续费失败文案', () => {
    expect(autoRenewFailureText({ ...base, auto_renew_failed_reason: 'insufficient_balance' })).toBe('自动续费失败：余额不足，请充值后系统会自动重试')
    expect(autoRenewFailureText({ ...reserved, auto_renew_failed_reason: 'renew_blocked' })).toBe('自动续费失败：机器已有后续预订，可续至 2026-10-25 08:00')
    expect(autoRenewFailureText(base)).toBe('')
  })
  it('按钮矩阵含 reserved / cancelled', () => {
    expect(canRenew('reserved')).toBe(true)
    expect(canRelease('reserved')).toBe(false)
    expect(canRenew('cancelled')).toBe(false)
    expect(canSetAutoRenew('reserved')).toBe(true)
    expect(canSetAutoRenew('released')).toBe(false)
  })
})
```

（import 行加 `autoRenewFailureText, canCancel, canSetAutoRenew, renewQuantityMax`。）

- [ ] **Step 2: 运行测试确认失败**

Run: `cd web && npx vitest run console/src/logic/__tests__/instance.spec.ts 2>&1 | tail -5`
Expected: 失败（函数未定义）。

- [ ] **Step 3: `logic/instance.ts`**

```ts
import { fmtDuration, fmtTime, type Duration, type Instance, type InstanceState } from '@spark/shared'
import { MAX_QUANTITY } from './booking'

/** 以拿到数据的本地时刻为基准递减；暂停中与非运行状态不递减。 */
export function remainingNow(i: Instance, fetchedAt: number, now: number): number {
  if (i.state !== 'running' || i.suspended) return i.remaining_seconds
  return Math.max(0, i.remaining_seconds - Math.floor((now - fetchedAt) / 1000))
}

export function instanceTimeText(i: Instance, remaining: number): string {
  if (i.state === 'reserved' && i.scheduled_start_at) return `${fmtTime(i.scheduled_start_at)} 开始 · 共 ${fmtDuration(i.pending_seconds)}`
  if (i.state === 'cancelled') return '已取消'
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

export const canRenew = (s: InstanceState) => s === 'reserved' || s === 'creating' || s === 'running' || s === 'stopped'
export const canRelease = (s: InstanceState) => s === 'running' || s === 'stopped'
export const canSetAutoRenew = (s: InstanceState) => s === 'reserved' || s === 'creating' || s === 'running' || s === 'stopped'
/** 预订开通前（开始前 1 小时）可取消。 */
export const canCancel = (i: Instance, nowMs: number) => i.state === 'reserved' && !!i.cancellable_until && nowMs < Date.parse(i.cancellable_until)

/** 某档位最多能买的单位数：后端 renew_options 为准，没有时按 90 天上限。 */
export function renewQuantityMax(i: Instance, d: Duration): number {
  const opt = i.renew_options?.find((o) => o.duration === d)
  return opt ? opt.quantity_max : MAX_QUANTITY[d]
}

export function autoRenewFailureText(i: Instance): string {
  if (i.auto_renew_failed_reason === 'insufficient_balance') return '自动续费失败：余额不足，请充值后系统会自动重试'
  if (i.auto_renew_failed_reason === 'renew_blocked') return `自动续费失败：机器已有后续预订，可续至 ${i.renewable_until ? fmtTime(i.renewable_until) : '—'}`
  return ''
}
```

- [ ] **Step 4: 实例列表**

`web/console/src/views/InstancesView.vue` 的 `<script>`：

```ts
import { instanceStateLabel, instanceStateType, labelOf, messageOf, useMobile, type Instance, type Listed } from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, onMounted, ref } from 'vue'
import { useRoute } from 'vue-router'
import { api } from '../api'
import { usePolling } from '../composables/usePolling'
import { useTicker } from '../composables/useTicker'
import { canCancel, instanceTimeText, remainingNow } from '../logic/instance'
import { notifyError } from '../logic/notify'

const route = useRoute()
const mobile = useMobile()
const items = ref<Instance[]>([])
const fetchedAt = ref(0)
const loading = ref(true)
const loadError = ref('')
const now = useTicker()
let failing = false
const batchFilter = computed(() => (typeof route.query.batch === 'string' ? route.query.batch : ''))

async function load() {
  try {
    items.value = (await api.get<Listed<Instance>>('/instances')).items // 后端已按创建时间倒序、最多 100 条
    fetchedAt.value = Date.now()
    loadError.value = ''
    failing = false
  } catch (e) {
    if (!failing) notifyError(e)
    failing = true
    if (fetchedAt.value === 0) loadError.value = messageOf(e)
  } finally {
    loading.value = false
  }
}
async function retry() {
  loading.value = true
  loadError.value = ''
  await load()
}
const busy = computed(() => items.value.some((i) => i.state === 'creating' || i.state === 'wiping' || i.state === 'reserved'))
usePolling(load, 10_000, { enabled: () => busy.value })
onMounted(load)

const shown = computed(() => (batchFilter.value ? items.value.filter((i) => i.batch_id === batchFilter.value) : items.value))
const rows = computed(() =>
  shown.value.map((i) => {
    const remaining = remainingNow(i, fetchedAt.value, now.value)
    return { ...i, remaining, timeText: instanceTimeText(i, remaining), cancellable: canCancel(i, now.value) }
  }),
)
const cancellableCount = computed(() => rows.value.filter((r) => r.cancellable).length)
const cancelling = ref(false)
/** 取消本批次全部未开通实例：逐台调用，成功 / 失败各自提示。 */
async function cancelAll() {
  if (cancelling.value) return
  cancelling.value = true
  let ok = 0
  const failed: string[] = []
  for (const r of rows.value.filter((x) => x.cancellable)) {
    try {
      await api.post(`/instances/${r.id}/cancel`, {})
      ok++
    } catch (e) {
      failed.push(`${r.id}：${messageOf(e)}`)
    }
  }
  cancelling.value = false
  if (ok) ElMessage.success(`已取消 ${ok} 台`)
  for (const f of failed) ElMessage.error(f)
  await load()
}
```

模板：在 `el-skeleton` 之前（`loadError` 之后）加批次头：

```vue
    <el-card v-if="batchFilter" class="batch-head" data-testid="instances-batch-head">
      <span>批次 <code class="mono">{{ batchFilter }}</code> · {{ rows.length }} 台</span>
      <span class="grow" />
      <router-link to="/instances"><el-button size="small">查看全部实例</el-button></router-link>
      <el-button v-if="cancellableCount" size="small" type="danger" plain :loading="cancelling" data-testid="instances-cancel-all" @click="cancelAll">取消全部未开通（{{ cancellableCount }}）</el-button>
    </el-card>
```

手机卡片在 `instance-image` 行后加 `<div v-if="i.batch_id" class="muted"><router-link :to="{ path: '/instances', query: { batch: i.batch_id } }" data-testid="instance-batch">批次 {{ i.batch_id }}</router-link></div>`，`instance-time` 行后加 `<div v-if="i.reassign_count > 0" class="muted" data-testid="instance-reassigned">已更换机器</div>` 与 `<div v-if="i.cancellable" class="muted" data-testid="instance-cancellable">可取消</div>`。电脑表格「时长」列后加一列：

```vue
      <el-table-column label="批次 / 备注" min-width="200">
        <template #default="{ row }">
          <router-link v-if="row.batch_id" :to="{ path: '/instances', query: { batch: row.batch_id } }" class="mono" data-testid="instance-batch">{{ row.batch_id }}</router-link>
          <span v-if="row.reassign_count > 0" class="muted" data-testid="instance-reassigned"> 已更换机器</span>
          <span v-if="row.cancellable" class="muted" data-testid="instance-cancellable"> 可取消</span>
        </template>
      </el-table-column>
```

样式加 `.batch-head { display: flex; align-items: center; gap: 8px; margin-bottom: 12px; } .grow { flex: 1; }`。

- [ ] **Step 5: 实例详情**

`web/console/src/views/InstanceDetailView.vue` 的改动点（按现有结构插入）：

`<script>` 增加 import：`onUnmounted` 加进现有的 vue 导入列表（`import { computed, onMounted, onUnmounted, ref, watch } from 'vue'`）；`type CancelQuote, type Duration, type UserEvent` 来自 `@spark/shared`；`autoRenewFailureText, canCancel, canSetAutoRenew, renewQuantityMax` 来自 `../logic/instance`；`describeDuration` 来自 `../logic/booking`。新增状态与方法：

```ts
const nowMs = ref(Date.now())
const ticker = setInterval(() => (nowMs.value = Date.now()), 1000)
onUnmounted(() => clearInterval(ticker))

// ---- 取消预订 ----
const cancelVisible = ref(false)
const cancelQuote = ref<CancelQuote | null>(null)
const cancelling = ref(false)
async function openCancel() {
  try {
    cancelQuote.value = await api.get<CancelQuote>(`/instances/${id.value}/cancel-quote`)
    cancelVisible.value = true
  } catch (e) {
    notifyError(e)
  }
}
async function confirmCancel() {
  if (cancelling.value) return
  cancelling.value = true
  try {
    inst.value = await api.post<Instance>(`/instances/${id.value}/cancel`, {})
    cancelVisible.value = false
    ElMessage.success('已取消预订，退款已退回余额')
    await Promise.all([refreshBalance(), loadEvents()])
  } catch (e) {
    cancelVisible.value = false
    notifyError(e) // cancel_not_allowed 等原文
    await load()
  } finally {
    cancelling.value = false
  }
}

// ---- 自动续费 ----
const togglingAutoRenew = ref(false)
async function setAutoRenew(enabled: boolean, duration?: Duration) {
  if (togglingAutoRenew.value || !inst.value) return
  togglingAutoRenew.value = true
  try {
    inst.value = await api.put<Instance>(`/instances/${id.value}/auto-renew`, { enabled, duration: duration ?? inst.value.auto_renew_duration })
    ElMessage.success(enabled ? '已开启自动续费' : '已关闭自动续费')
  } catch (e) {
    notifyError(e)
  } finally {
    togglingAutoRenew.value = false
  }
}

// ---- 时间线 ----
const events = ref<UserEvent[]>([])
async function loadEvents() {
  const of = id.value
  try {
    const { items } = await api.get<Listed<UserEvent>>(`/instances/${of}/events`)
    if (of === id.value) events.value = items
  } catch (e) {
    notifyError(e)
  }
}
```

`load()` 里状态变化时追加 `void loadEvents()`（与 `loadPorts` 同一处）；`onMounted` 后 `void loadEvents()`；`watch(id)` 里清空 `events.value = []`、`cancelVisible.value = false`。

续费表单：`renewDuration` 之外加 `const renewQuantity = ref(1)`；`renewKey` 的依赖加 `renewQuantity.value`；`renewPrice` 改为 `单价 × renewQuantity`；`const renewMax = computed(() => (inst.value ? renewQuantityMax(inst.value, renewDuration.value) : 1))`；`watch(renewDuration, () => { renewQuantity.value = Math.min(Math.max(renewQuantity.value, 1), Math.max(renewMax.value, 1)) })`；`openRenew` 里默认档位取第一个 `quantity_max > 0` 的；`confirmRenew` 的请求体加 `quantity: renewQuantity.value`，并把 `renew_blocked` 原文用 `ElMessage.warning` 显示后 `await load()`。

模板改动：

```vue
        <el-alert v-if="inst.auto_renew_failed_reason" type="error" :title="autoRenewFailureText(inst)" show-icon :closable="false" data-testid="detail-auto-renew-failed">
          <router-link v-if="inst.auto_renew_failed_reason === 'insufficient_balance'" to="/wallet/recharge">去充值</router-link>
        </el-alert>
        <el-alert v-if="inst.state === 'cancelled'" type="info" title="预订已取消，退款已退回余额" show-icon :closable="false" data-testid="detail-cancelled-hint" />
```

（放在 `detail-failed-hint` 之后。）`el-descriptions` 里 `镜像` 之后加：

```vue
          <el-descriptions-item v-if="inst.group_name" label="机组"><span key="group" data-testid="detail-group">{{ inst.group_name }}</span></el-descriptions-item>
          <el-descriptions-item label="机器"><span data-testid="detail-machine">{{ inst.machine_label }}<template v-if="inst.reassign_count > 0">（<span data-testid="detail-reassigned">已更换机器 {{ inst.reassign_count }} 次</span>）</template></span></el-descriptions-item>
          <el-descriptions-item v-if="inst.scheduled_start_at" label="预订开始"><span key="start" data-testid="detail-start">{{ fmtTime(inst.scheduled_start_at) }}</span></el-descriptions-item>
          <el-descriptions-item v-if="inst.scheduled_end_at" label="预订结束"><span key="end" data-testid="detail-end">{{ fmtTime(inst.scheduled_end_at) }}</span></el-descriptions-item>
          <el-descriptions-item v-if="inst.batch_id" label="批次"><router-link key="batch" :to="{ path: '/instances', query: { batch: inst.batch_id } }" class="mono" data-testid="detail-batch">{{ inst.batch_id }}</router-link></el-descriptions-item>
```

操作区：

```vue
          <el-button v-if="canRenew(inst.state)" type="primary" data-testid="detail-renew-btn" @click="openRenew">{{ inst.state === 'reserved' ? '续费（加时）' : '续费' }}</el-button>
          <el-button v-if="canCancel(inst, nowMs)" type="danger" plain data-testid="detail-cancel-btn" @click="openCancel">取消预订</el-button>
          <el-button v-if="canRelease(inst.state)" type="danger" plain data-testid="detail-release-btn" @click="releaseVisible = true">释放</el-button>
```

「SSH 连接」卡片的非 running 提示改为：`<p v-else class="muted" data-testid="detail-ssh-hint">{{ inst.state === 'reserved' ? '开通后可连接' : '实例运行中才能连接' }}</p>`；`canManagePorts` 改为 `s === 'reserved' || s === 'creating' || s === 'running' || s === 'stopped'`。

新卡片（放在「Web 访问」卡片之后）：

```vue
      <el-card v-if="canSetAutoRenew(inst.state)" header="自动续费" data-testid="detail-auto-renew-card">
        <div class="row">
          <el-switch :model-value="inst.auto_renew" :loading="togglingAutoRenew" data-testid="detail-auto-renew" @change="(v: string | number | boolean) => setAutoRenew(v === true)" />
          <el-select :model-value="inst.auto_renew_duration" :disabled="!inst.auto_renew" data-testid="detail-auto-renew-duration" style="width: 110px" @change="(d: Duration) => setAutoRenew(true, d)">
            <el-option v-for="p in product?.prices ?? []" :key="p.duration" :value="p.duration" :label="labelOf(durationLabel, p.duration)" />
          </el-select>
          <span class="muted">到期前 1 小时自动扣款续 1 个单位；余额不足会重试到到期，被后续预订挡住时按能放下的档位办理；停机后不自动续。</span>
        </div>
      </el-card>

      <el-card header="动态" data-testid="detail-timeline">
        <el-timeline>
          <el-timeline-item v-for="(ev, i) in events" :key="i" :timestamp="fmtTime(ev.at)" placement="top">
            <span data-testid="detail-event" :data-kind="ev.reason_kind">{{ ev.detail }}</span>
          </el-timeline-item>
        </el-timeline>
        <p v-if="!events.length" class="muted">暂无记录</p>
      </el-card>
```

（自动续费档位下拉需要商品价格：`onMounted` 时同样加载 `/products` 到 `product`，与 `openRenew` 共用。）

续费对话框：租期单选后加数量与可续至：

```vue
        <el-form-item label="数量">
          <el-input-number v-model="renewQuantity" :min="1" :max="Math.max(renewMax, 1)" :disabled="renewMax === 0" data-testid="detail-renew-quantity" />
          <span class="muted">{{ describeDuration(renewDuration, renewQuantity) }}</span>
        </el-form-item>
        <p v-if="inst?.renewable_until" class="muted" data-testid="detail-renew-until">可续至 {{ fmtTime(inst.renewable_until) }}（机器之后已有预订）</p>
```

档位按钮加 `:disabled="renewQuantityMax(inst!, p.duration) === 0"`，并在不可选档位旁显示「超出可续时间」（`<span v-if="renewQuantityMax(inst!, p.duration) === 0" class="muted small">超出可续时间</span>`）。

取消对话框（放在释放对话框之后）：

```vue
    <AppDialog v-model="cancelVisible" kind="confirm" title="取消预订" data-testid="detail-cancel-dialog">
      <p v-if="cancelQuote" data-testid="detail-cancel-text">将退款 {{ fmtYuan(cancelQuote.refund) }}（{{ cancelQuote.percent }}%），可取消至 {{ fmtTime(cancelQuote.deadline) }}。开通后将不能取消。</p>
      <template #footer>
        <el-button data-testid="detail-cancel-cancel" @click="cancelVisible = false">再想想</el-button>
        <el-button type="danger" :loading="cancelling" data-testid="detail-cancel-ok" @click="confirmCancel">确定取消</el-button>
      </template>
    </AppDialog>
```

- [ ] **Step 6: 帮助页**

`web/console/src/views/HelpView.vue` 在「数据保留规则」卡片之前加三张卡：

```vue
    <el-card>
      <h2 data-testid="help-booking">档期预订</h2>
      <ul>
        <li>可提前 2 小时～60 天预订，开始时间为北京时间整点；开始前 1 小时系统自动开通，开通后即可登录，开始时刻起计费。</li>
        <li>预订下单时扣款，价格按下单时锁定，之后改价不影响已付订单。</li>
        <li>取消规则：下单 15 分钟内全额退；之后距开始 24 小时以上全额退，不足 24 小时退 50%；开通（开始前 1 小时）后不能取消，只能释放（不退款）。</li>
        <li>预订的是机组，指定的机器只是优先：机器故障或仍被占用时会在同机组为您更换机器，详情页会提示。</li>
        <li>可续至：机器之后已有别人的预订时，续费最多到「下一预订开始前 8 小时」；详情页会显示可续至时间。</li>
      </ul>
    </el-card>
    <el-card>
      <h2 data-testid="help-batch">一次多台</h2>
      <ul>
        <li>一单最多 8 台，整单要么全部成功要么全部失败；成功后逐台管理，批次页可一键取消未开通的预订。</li>
      </ul>
    </el-card>
    <el-card>
      <h2 data-testid="help-auto-renew">自动续费</h2>
      <ul>
        <li>到期前 1 小时自动扣款续 1 个单位；余额不足会每分钟重试到到期，首次失败发短信提醒。</li>
        <li>被后续预订挡住时按能放下的档位办理（1 月 → 1 周 → 1 天），都放不下则失败并提示。</li>
        <li>到期停机后不再自动续费，手动续费恢复后继续生效。</li>
      </ul>
    </el-card>
```

- [ ] **Step 7: 运行测试确认通过**

Run: `cd web && npm test 2>&1 | tail -6 && npm run build 2>&1 | tail -3`
Expected: Vitest 全绿；构建通过。

- [ ] **Step 8: 提交**

```bash
git add web/console/src/logic/instance.ts web/console/src/logic/__tests__/instance.spec.ts web/console/src/views/InstancesView.vue web/console/src/views/InstanceDetailView.vue web/console/src/views/HelpView.vue
git commit -m "feat(console): booking states in instance list/detail, cancel with quote, renew quantity and renewable-until, auto-renew card, timeline, help

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 20: 后台页面（机组、预订、机器详情档期与编辑、实例页预订操作、仪表盘、订单批次）

**Files:**
- Create: `web/admin/src/views/GroupsView.vue`、`web/admin/src/views/BookingsView.vue`
- Modify: `web/shared/src/types/admin.ts`、`web/admin/src/menu.ts`、`web/admin/src/router.ts`、`web/admin/src/__tests__/menu.spec.ts`、`web/admin/src/logic/instanceOps.ts`、`web/admin/src/logic/__tests__/instanceOps.spec.ts`、`web/admin/src/views/MachineDetailView.vue`、`web/admin/src/views/InstanceDetailView.vue`、`web/admin/src/views/DashboardView.vue`、`web/admin/src/views/OrdersView.vue`、`web/admin/src/components/FinancePage.vue`

**Interfaces:**
- Consumes: Task 3 / 13 的后台 API；Task 18 的共享类型。
- Produces:
  - `types/admin.ts`：`AdminGroup{id, name, region, description, sort, visible, created_at, updated_at, machine_count, idle_now, reserved_upcoming}`、`GroupInput{name, region, description, sort, visible}`；`Machine` 加 `group_id: number | null; display_name: string`；`AdminInstance` 加 `scheduled_start_at, scheduled_end_at: string | null; group_id: number | null; batch_id: string | null; reassign_count: number; auto_renew: boolean; auto_renew_duration: Duration; auto_renew_failed_reason: string; provisioned_at, cancelled_at: string | null`；`MachineDetail` 加 `group: AdminGroupBrief | null; leases: CalendarPeriod[]; upcoming_bookings: number`（`AdminGroupBrief = Pick<AdminGroup, 'id' | 'name' | 'region' | 'visible'>`）；`AdminOrder` 加 `quantity, batch_id, auto`；`Dashboard` 加 `bookings_upcoming_7d: number`；`UpcomingDay{date, count}`。
  - 菜单 / 路由：`/groups`（机组，`OPS_ROLES`）、`/bookings`（预订，`ALL_ROLES`）；`menu.spec.ts` 计数 super 15、ops 10、finance 9（finance 缺 `machines, pdus, kyc, catalog, admins, groups`）。
  - `logic/instanceOps.ts`：`showReassign(role, state)`（ops/super 且 reserved）、`showCancelBooking(role, state)`（同）、`showMigrate` 排除预订实例不变（仅 running）。
  - `data-testid`：机组页 `groups-create-btn / group-row / group-name / group-count / group-idle / group-upcoming / group-visible / group-edit / group-delete / group-machines / group-dialog / group-form-name / group-form-region / group-form-visible / group-form-submit / group-machines-drawer / gm-row / gm-remove / gm-add-input / gm-add / gm-display-input / gm-display-save`；预订页 `bookings-filter-state / bookings-filter-user / bookings-search / booking-row / booking-id / booking-user / booking-machine / booking-start / booking-reassign / booking-cancel / reassign-dialog / reassign-machine / reassign-reason / reassign-submit / cancel-booking-dialog / cancel-booking-reason / cancel-booking-submit`；机器详情 `mdetail-group / mdetail-display / mdetail-edit-btn / medit-dialog / medit-group / medit-display / medit-submit / mdetail-lease-row / mdetail-maintenance-upcoming`；实例详情 `adetail-start / adetail-batch / adetail-reassigned / adetail-auto-renew / adetail-reassign-btn / adetail-cancel-booking-btn`；仪表盘 `dash-bookings-7d`；订单 `ord-quantity / ord-batch / ord-auto / fin-filter-batch`。

- [ ] **Step 1: 写失败的测试**

`web/admin/src/__tests__/menu.spec.ts` 的 `it.each` 改为：

```ts
    ['super', 15, []],
    ['ops', 10, ['orders', 'ledger', 'recharges', 'catalog', 'admins']],
    ['finance', 9, ['machines', 'pdus', 'kyc', 'catalog', 'admins', 'groups']],
```

`web/admin/src/logic/__tests__/instanceOps.spec.ts` 追加：

```ts
  it('预订操作只对 reserved 且 ops/super 可见', () => {
    expect(showReassign('ops', 'reserved')).toBe(true)
    expect(showReassign('super', 'reserved')).toBe(true)
    expect(showReassign('finance', 'reserved')).toBe(false)
    expect(showReassign('ops', 'creating')).toBe(false)
    expect(showCancelBooking('ops', 'reserved')).toBe(true)
    expect(showCancelBooking('ops', 'running')).toBe(false)
    expect(showMigrate('ops', 'reserved')).toBe(false)
    expect(showExtend('ops', 'reserved')).toBe(true)
    expect(showRefund('finance', 'reserved')).toBe(false)
  })
```

（import 加 `showCancelBooking, showReassign`。）

- [ ] **Step 2: 运行测试确认失败**

Run: `cd web && npx vitest run admin/src 2>&1 | tail -6`
Expected: 两个用例失败。

- [ ] **Step 3: 类型、菜单、路由、`instanceOps`**

`web/shared/src/types/admin.ts` 追加 / 修改：

```ts
export interface AdminGroup {
  id: number
  name: string
  region: string
  description: string
  sort: number
  visible: boolean
  created_at: string
  updated_at: string
  machine_count: number
  idle_now: number
  reserved_upcoming: number
}
export type AdminGroupBrief = Pick<AdminGroup, 'id' | 'name' | 'region' | 'visible'>
export interface GroupInput {
  name: string
  region: string
  description: string
  sort: number
  visible: boolean
}
export interface UpcomingDay {
  date: string
  count: number
}
```

`Machine` 加 `group_id: number | null` 与 `display_name: string`；`AdminInstance` 加 `scheduled_start_at: string | null; scheduled_end_at: string | null; group_id: number | null; batch_id: string | null; reassign_count: number; auto_renew: boolean; auto_renew_duration: Duration; auto_renew_failed_reason: string; provisioned_at: string | null; cancelled_at: string | null`；`MachineDetail` 加 `group: AdminGroupBrief | null; leases: CalendarPeriod[]; upcoming_bookings: number`（import `CalendarPeriod` from `./booking`）；`AdminOrder` 加 `quantity: number; batch_id: string | null; auto: boolean`；`Dashboard` 加 `bookings_upcoming_7d: number`。

`web/admin/src/menu.ts` 在 `pdus` 之后加 `{ key: 'groups', path: '/groups', label: '机组', roles: OPS_ROLES }`，在 `instances` 之后加 `{ key: 'bookings', path: '/bookings', label: '预订', roles: ALL_ROLES }`。`web/admin/src/router.ts` 对应加 `{ path: '/groups', component: () => import('./views/GroupsView.vue'), meta: { title: '机组', roles: OPS_ROLES } }` 与 `{ path: '/bookings', component: () => import('./views/BookingsView.vue'), meta: { title: '预订', roles: ALL_ROLES } }`。

`web/admin/src/logic/instanceOps.ts` 的 `extendable` 改为 `s === 'reserved' || s === 'creating' || s === 'running' || s === 'stopped'`（后台免费延长允许 reserved），`showRefund` 保持 `creating / running / stopped`（单独定义 `const refundable = (s) => s === 'creating' || s === 'running' || s === 'stopped'`），并追加：

```ts
export const showReassign = (r: Role, s: InstanceState) => opsRole(r) && s === 'reserved'
export const showCancelBooking = (r: Role, s: InstanceState) => opsRole(r) && s === 'reserved'
```

- [ ] **Step 4: 机组页**

`web/admin/src/views/GroupsView.vue`：

```vue
<script setup lang="ts">
import { messageOf, type AdminGroup, type GroupInput, type Listed, type Machine, type Paged } from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, onMounted, reactive, ref } from 'vue'
import { api } from '../api'
import { notifyError } from '../logic/notify'

const items = ref<AdminGroup[]>([])
const loading = ref(false)
const error = ref('')
const visible = ref(false)
const editing = ref<AdminGroup | null>(null)
const busy = ref(false)
const blank = (): GroupInput => ({ name: '', region: '', description: '', sort: 0, visible: true })
const form = reactive<GroupInput>(blank())
const deleting = ref<AdminGroup | null>(null)

async function load() {
  loading.value = true
  try {
    items.value = (await api.get<Listed<AdminGroup>>('/groups')).items
    error.value = ''
  } catch (e) {
    notifyError(e)
    items.value = []
    error.value = messageOf(e)
  } finally {
    loading.value = false
  }
}
onMounted(load)

function openCreate() {
  editing.value = null
  Object.assign(form, blank())
  visible.value = true
}
function openEdit(g: AdminGroup) {
  editing.value = g
  Object.assign(form, { name: g.name, region: g.region, description: g.description, sort: g.sort, visible: g.visible })
  visible.value = true
}
async function submit() {
  if (busy.value) return
  busy.value = true
  try {
    if (editing.value) await api.put(`/groups/${editing.value.id}`, form)
    else await api.post('/groups', form)
    ElMessage.success(editing.value ? '机组已更新' : '机组已创建')
    visible.value = false
    await load()
  } catch (e) {
    notifyError(e) // duplicate_group_name / bad_request 原文；弹窗保留
  } finally {
    busy.value = false
  }
}
async function confirmDelete() {
  if (!deleting.value || busy.value) return
  busy.value = true
  try {
    await api.del(`/groups/${deleting.value.id}`, {})
    ElMessage.success('机组已删除')
    deleting.value = null
    await load()
  } catch (e) {
    deleting.value = null
    notifyError(e) // group_not_empty 原文
  } finally {
    busy.value = false
  }
}

// ---- 管理机器（抽屉）----
const drawer = reactive({ visible: false, group: null as AdminGroup | null, members: [] as Machine[], addInput: '', rack: '', candidates: [] as Machine[] })
const displayDraft = reactive<Record<number, string>>({})
async function openMachines(g: AdminGroup) {
  drawer.group = g
  drawer.addInput = ''
  drawer.rack = ''
  drawer.candidates = []
  drawer.visible = true
  await loadMembers()
}
async function loadMembers() {
  if (!drawer.group) return
  try {
    const all = (await api.get<Paged<Machine>>('/machines', { page: 1, page_size: 100 })).items
    drawer.members = all.filter((m) => m.group_id === drawer.group!.id)
    for (const m of drawer.members) displayDraft[m.id] = m.display_name
  } catch (e) {
    notifyError(e)
  }
}
async function searchCandidates() {
  try {
    const all = (await api.get<Paged<Machine>>('/machines', { rack: drawer.rack || undefined, page: 1, page_size: 100 })).items
    drawer.candidates = all.filter((m) => m.group_id !== drawer.group?.id)
  } catch (e) {
    notifyError(e)
  }
}
const addIDs = computed(() => drawer.addInput.split(/[\s,，]+/).map((s) => Number(s)).filter((n) => Number.isInteger(n) && n > 0))
async function addMachines(ids: number[]) {
  if (!drawer.group || !ids.length || busy.value) return
  busy.value = true
  try {
    await api.post(`/groups/${drawer.group.id}/machines`, { machine_ids: ids })
    ElMessage.success(`已加入 ${ids.length} 台`)
    drawer.addInput = ''
    await Promise.all([loadMembers(), load()])
    if (drawer.candidates.length) await searchCandidates()
  } catch (e) {
    notifyError(e)
  } finally {
    busy.value = false
  }
}
async function removeMachine(m: Machine) {
  if (!drawer.group || busy.value) return
  busy.value = true
  try {
    await api.del(`/groups/${drawer.group.id}/machines`, { machine_ids: [m.id] })
    await Promise.all([loadMembers(), load()])
  } catch (e) {
    notifyError(e)
  } finally {
    busy.value = false
  }
}
async function saveDisplay(m: Machine) {
  try {
    await api.put(`/machines/${m.id}`, { display_name: displayDraft[m.id] ?? '' })
    ElMessage.success('展示名已保存')
    await loadMembers()
  } catch (e) {
    notifyError(e) // duplicate_display_name 原文
  }
}
</script>

<template>
  <el-card>
    <div class="toolbar"><el-button type="primary" data-testid="groups-create-btn" @click="openCreate">新建机组</el-button></div>
    <el-alert v-if="error" type="error" :title="error" show-icon :closable="false" class="error" data-testid="groups-error">
      <el-button size="small" :loading="loading" data-testid="groups-retry" @click="load">重试</el-button>
    </el-alert>
    <el-table :data="items" v-loading="loading" row-key="id">
      <el-table-column label="名称" min-width="140"><template #default="{ row }"><span data-testid="group-row"><span data-testid="group-name">{{ row.name }}</span></span></template></el-table-column>
      <el-table-column label="位置" width="120"><template #default="{ row }"><span data-testid="group-region">{{ row.region || '—' }}</span></template></el-table-column>
      <el-table-column label="机器数" width="90"><template #default="{ row }"><span data-testid="group-count">{{ row.machine_count }}</span></template></el-table-column>
      <el-table-column label="现空闲" width="90"><template #default="{ row }"><span data-testid="group-idle">{{ row.idle_now }}</span></template></el-table-column>
      <el-table-column label="未开通预订" width="110"><template #default="{ row }"><span data-testid="group-upcoming">{{ row.reserved_upcoming }}</span></template></el-table-column>
      <el-table-column label="可见" width="80"><template #default="{ row }"><el-tag :type="row.visible ? 'success' : 'info'" size="small" data-testid="group-visible">{{ row.visible ? '可见' : '仅后台' }}</el-tag></template></el-table-column>
      <el-table-column label="排序" width="70"><template #default="{ row }">{{ row.sort }}</template></el-table-column>
      <el-table-column label="操作" width="240">
        <template #default="{ row }">
          <el-button size="small" data-testid="group-machines" @click="openMachines(row as AdminGroup)">管理机器</el-button>
          <el-button size="small" data-testid="group-edit" @click="openEdit(row as AdminGroup)">编辑</el-button>
          <el-button size="small" type="danger" plain data-testid="group-delete" @click="deleting = row as AdminGroup">删除</el-button>
        </template>
      </el-table-column>
    </el-table>

    <el-dialog v-model="visible" :title="editing ? '编辑机组' : '新建机组'" width="520" align-center destroy-on-close data-testid="group-dialog">
      <el-form label-position="top" @submit.prevent="submit">
        <el-form-item label="名称（对用户展示，1～32 字）"><el-input v-model="form.name" maxlength="32" data-testid="group-form-name" /></el-form-item>
        <el-form-item label="位置标签（可选）"><el-input v-model="form.region" maxlength="32" data-testid="group-form-region" /></el-form-item>
        <el-form-item label="描述（可选）"><el-input v-model="form.description" type="textarea" maxlength="200" data-testid="group-form-description" /></el-form-item>
        <div class="two">
          <el-form-item label="排序"><el-input-number v-model="form.sort" :controls="false" data-testid="group-form-sort" /></el-form-item>
          <el-form-item label="控制台可见"><el-switch v-model="form.visible" data-testid="group-form-visible" /></el-form-item>
        </div>
        <p class="muted">不可见的机组留给运维 / 大客户：用户看不到、不能下单，其机器也不参与「不限机组」的分配。</p>
        <el-button type="primary" native-type="submit" :loading="busy" data-testid="group-form-submit">保存</el-button>
      </el-form>
    </el-dialog>

    <el-dialog :model-value="!!deleting" title="删除机组" width="420" align-center data-testid="group-delete-dialog" @update:model-value="(v: boolean) => { if (!v) deleting = null }">
      <p>删除「{{ deleting?.name }}」？组内不能有机器，也不能有未结束的实例引用它。</p>
      <template #footer>
        <el-button @click="deleting = null">取消</el-button>
        <el-button type="danger" :loading="busy" data-testid="group-delete-ok" @click="confirmDelete">确定删除</el-button>
      </template>
    </el-dialog>

    <el-drawer v-model="drawer.visible" :title="`管理机器 · ${drawer.group?.name ?? ''}`" size="720px" data-testid="group-machines-drawer">
      <h4>组内机器（{{ drawer.members.length }}）</h4>
      <table class="grid">
        <thead><tr><th>ID</th><th>序列号</th><th>展示名（对用户）</th><th>状态</th><th></th></tr></thead>
        <tbody>
          <tr v-for="m in drawer.members" :key="m.id" data-testid="gm-row" :data-id="m.id">
            <td>{{ m.id }}</td>
            <td class="mono">{{ m.serial }}</td>
            <td class="row">
              <el-input v-model="displayDraft[m.id]" size="small" maxlength="32" placeholder="机器 #id" style="width: 140px" data-testid="gm-display-input" />
              <el-button size="small" data-testid="gm-display-save" @click="saveDisplay(m)">保存</el-button>
            </td>
            <td>{{ m.state }}</td>
            <td><el-button size="small" type="danger" plain data-testid="gm-remove" @click="removeMachine(m)">移出</el-button></td>
          </tr>
          <tr v-if="!drawer.members.length"><td colspan="5" class="muted">暂无机器</td></tr>
        </tbody>
      </table>
      <h4>加入机器</h4>
      <div class="row">
        <el-input v-model="drawer.addInput" placeholder="机器 ID，逗号或空格分隔" data-testid="gm-add-input" style="width: 300px" />
        <el-button type="primary" :disabled="!addIDs.length" :loading="busy" data-testid="gm-add" @click="addMachines(addIDs)">加入</el-button>
      </div>
      <div class="row">
        <el-input v-model="drawer.rack" placeholder="按机柜筛选" data-testid="gm-rack" style="width: 160px" />
        <el-button data-testid="gm-search" @click="searchCandidates">查找</el-button>
      </div>
      <table v-if="drawer.candidates.length" class="grid">
        <thead><tr><th>ID</th><th>序列号</th><th>机柜</th><th>当前机组</th><th></th></tr></thead>
        <tbody>
          <tr v-for="m in drawer.candidates" :key="m.id" data-testid="gm-candidate">
            <td>{{ m.id }}</td><td class="mono">{{ m.serial }}</td><td>{{ m.rack }}</td><td>{{ m.group_id ?? '—' }}</td>
            <td><el-button size="small" data-testid="gm-candidate-add" @click="addMachines([m.id])">加入</el-button></td>
          </tr>
        </tbody>
      </table>
    </el-drawer>
  </el-card>
</template>

<style scoped>
.toolbar { margin-bottom: 12px; display: flex; justify-content: flex-end; }
.error { margin-bottom: 12px; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.muted { color: #909399; font-size: 13px; }
.two { display: grid; grid-template-columns: 1fr 1fr; gap: 12px; }
.row { display: flex; align-items: center; gap: 8px; margin: 8px 0; }
.grid { width: 100%; border-collapse: collapse; font-size: 13px; }
.grid th, .grid td { padding: 6px 8px; text-align: left; border-bottom: 1px solid var(--el-border-color-lighter); }
</style>
```

- [ ] **Step 5: 预订页**

`web/admin/src/views/BookingsView.vue`：

```vue
<script setup lang="ts">
import { fmtDuration, fmtTime, fmtYuan, labelOf, machineStateLabel, type AdminGroup, type AdminInstance, type Listed, type Paged } from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, onMounted, reactive, ref } from 'vue'
import { api } from '../api'
import DateRange from '../components/DateRange.vue'
import IdLink from '../components/IdLink.vue'
import { usePaged } from '../composables/usePaged'
import { showCancelBooking, showReassign } from '../logic/instanceOps'
import { notifyError } from '../logic/notify'
import { session } from '../store/session'

const digits = (v: string) => v.replace(/\D/g, '')
const filter = reactive({ state: 'reserved', user_id: '', group_id: '', machine_id: '', range: { from: '', to: '' } })
let applied = { ...filter, range: { ...filter.range } }
type BookingRow = AdminInstance & { machine_state: string }
const list = usePaged<BookingRow>((page, size) =>
  api.get<Paged<BookingRow>>('/bookings', { state: applied.state, user_id: applied.user_id, group_id: applied.group_id, machine_id: applied.machine_id, from: applied.range.from, to: applied.range.to, page, page_size: size }),
)
const groups = ref<AdminGroup[]>([])
const groupName = computed(() => new Map(groups.value.map((g) => [g.id, g.name])))
const role = computed(() => session.admin?.role ?? 'finance')
const isFinance = computed(() => role.value === 'finance')

async function loadGroups() {
  try {
    groups.value = (await api.get<Listed<AdminGroup>>('/groups')).items
  } catch (e) {
    notifyError(e)
  }
}
onMounted(() => {
  void list.load()
  void loadGroups()
})
function search() {
  applied = { ...filter, range: { ...filter.range } }
  list.reset()
}
function resetFilter() {
  Object.assign(filter, { state: 'reserved', user_id: '', group_id: '', machine_id: '', range: { from: '', to: '' } })
  search()
}

// ---- 改派（ops）----
const reassign = reactive({ visible: false, target: null as AdminInstance | null, machineId: '', reason: '', busy: false })
function openReassign(b: AdminInstance) {
  Object.assign(reassign, { visible: true, target: b, machineId: '', reason: '' })
}
async function submitReassign() {
  if (reassign.busy || !reassign.target) return
  if (!reassign.reason.trim()) return void ElMessage.warning('请填写改派原因')
  reassign.busy = true
  try {
    const body: Record<string, unknown> = { reason: reassign.reason.trim() }
    if (reassign.machineId) body.machine_id = Number(reassign.machineId)
    const out = await api.post<AdminInstance>(`/instances/${reassign.target.id}/reassign`, body)
    ElMessage.success(`已改派到机器 ${out.machine_id}`)
    reassign.visible = false
    await list.load()
  } catch (e) {
    notifyError(e) // machine_unavailable 原文；弹窗保留
  } finally {
    reassign.busy = false
  }
}

// ---- 取消预订（ops；动态码由请求层弹出）----
const cancel = reactive({ visible: false, target: null as AdminInstance | null, reason: '', busy: false })
function openCancel(b: AdminInstance) {
  Object.assign(cancel, { visible: true, target: b, reason: '' })
}
async function submitCancel() {
  if (cancel.busy || !cancel.target) return
  if (!cancel.reason.trim()) return void ElMessage.warning('请填写取消原因')
  cancel.busy = true
  try {
    const out = await api.post<{ refunded: number }>(`/instances/${cancel.target.id}/cancel-booking`, { reason: cancel.reason.trim() })
    ElMessage.success(`已取消预订，退款 ${fmtYuan(out.refunded)}`)
    cancel.visible = false
    await list.load()
  } catch (e) {
    notifyError(e)
  } finally {
    cancel.busy = false
  }
}
</script>

<template>
  <el-card>
    <el-form inline class="filters" @submit.prevent="search">
      <el-form-item label="状态">
        <el-select v-model="filter.state" data-testid="bookings-filter-state" style="width: 120px">
          <el-option label="未开通" value="reserved" /><el-option label="已取消" value="cancelled" />
        </el-select>
      </el-form-item>
      <el-form-item label="用户 ID"><el-input :model-value="filter.user_id" clearable data-testid="bookings-filter-user" style="width: 110px" @update:model-value="(v: string) => (filter.user_id = digits(v))" /></el-form-item>
      <el-form-item label="机组">
        <el-select v-model="filter.group_id" clearable placeholder="全部" data-testid="bookings-filter-group" style="width: 140px">
          <el-option v-for="g in groups" :key="g.id" :label="g.name" :value="String(g.id)" />
        </el-select>
      </el-form-item>
      <el-form-item label="机器 ID"><el-input :model-value="filter.machine_id" clearable data-testid="bookings-filter-machine" style="width: 110px" @update:model-value="(v: string) => (filter.machine_id = digits(v))" /></el-form-item>
      <el-form-item label="开始日期"><DateRange v-model="filter.range" prefix="bookings" /></el-form-item>
      <el-button type="primary" native-type="submit" data-testid="bookings-search">查询</el-button>
      <el-button data-testid="bookings-reset" @click="resetFilter">重置</el-button>
    </el-form>
    <el-alert v-if="list.error.value" type="error" :title="list.error.value" show-icon :closable="false" class="error" data-testid="bookings-error">
      <el-button size="small" :loading="list.loading.value" data-testid="bookings-retry" @click="list.load()">重试</el-button>
    </el-alert>
    <el-table :data="list.items.value" v-loading="list.loading.value" row-key="id">
      <el-table-column label="实例" min-width="150"><template #default="{ row }"><span data-testid="booking-row"><span data-testid="booking-id"><IdLink kind="instance" :id="row.id" /></span></span></template></el-table-column>
      <el-table-column label="用户" width="90"><template #default="{ row }"><span data-testid="booking-user"><IdLink kind="user" :id="row.user_id" /></span></template></el-table-column>
      <el-table-column label="机组" width="120"><template #default="{ row }"><span data-testid="booking-group">{{ row.group_id ? (groupName.get(row.group_id) ?? row.group_id) : '不限' }}</span></template></el-table-column>
      <el-table-column label="机器" width="150">
        <template #default="{ row }">
          <span data-testid="booking-machine"><IdLink kind="machine" :id="row.machine_id" :disabled="isFinance" /></span>
          <el-tag v-if="row.machine_state === 'faulty' || row.machine_state === 'maintenance'" type="warning" size="small" data-testid="booking-machine-state">{{ labelOf(machineStateLabel, row.machine_state) }}</el-tag>
        </template>
      </el-table-column>
      <el-table-column label="开始" width="150"><template #default="{ row }"><span data-testid="booking-start">{{ fmtTime(row.scheduled_start_at) }}</span></template></el-table-column>
      <el-table-column label="时长" width="110"><template #default="{ row }"><span data-testid="booking-duration">{{ fmtDuration(row.pending_seconds) }}</span></template></el-table-column>
      <el-table-column label="批次" min-width="150"><template #default="{ row }"><span class="mono" data-testid="booking-batch">{{ row.batch_id ?? '—' }}</span></template></el-table-column>
      <el-table-column label="改派" width="70"><template #default="{ row }"><span data-testid="booking-reassigned">{{ row.reassign_count }}</span></template></el-table-column>
      <el-table-column label="下单" width="150"><template #default="{ row }">{{ fmtTime(row.created_at) }}</template></el-table-column>
      <el-table-column label="操作" width="180">
        <template #default="{ row }">
          <el-button v-if="showReassign(role, row.state)" size="small" data-testid="booking-reassign" @click="openReassign(row as AdminInstance)">改派</el-button>
          <el-button v-if="showCancelBooking(role, row.state)" size="small" type="danger" plain data-testid="booking-cancel" @click="openCancel(row as AdminInstance)">取消预订</el-button>
        </template>
      </el-table-column>
    </el-table>
    <div class="pager-row">
      <span class="muted" data-testid="bookings-total">共 {{ list.total.value }} 个</span>
      <el-pagination v-model:current-page="list.page.value" v-model:page-size="list.size.value" :total="list.total.value" layout="prev, pager, next" data-testid="bookings-pager" />
    </div>

    <el-dialog v-model="reassign.visible" title="改派" width="460" align-center data-testid="reassign-dialog">
      <p class="muted">留空机器 ID 由系统在同机组内自动挑选（只挑可见机组）；指定机器须空闲 / 已分配且所需时段无冲突，可以是任意机组（含仅后台可见的留池），不要求此刻在线——开通时再判断。</p>
      <el-form label-position="top" @submit.prevent="submitReassign">
        <el-form-item label="目标机器 ID（可空）"><el-input :model-value="reassign.machineId" data-testid="reassign-machine" @update:model-value="(v: string) => (reassign.machineId = digits(v))" /></el-form-item>
        <el-form-item label="原因（必填）"><el-input v-model="reassign.reason" data-testid="reassign-reason" /></el-form-item>
        <el-button type="primary" native-type="submit" :loading="reassign.busy" data-testid="reassign-submit">确定改派</el-button>
      </el-form>
    </el-dialog>
    <el-dialog v-model="cancel.visible" title="取消预订" width="460" align-center data-testid="cancel-booking-dialog">
      <p class="muted">平台原因取消：全额退款到用户余额，并发短信通知；需要动态码。</p>
      <el-form label-position="top" @submit.prevent="submitCancel">
        <el-form-item label="原因（必填，写入审计）"><el-input v-model="cancel.reason" data-testid="cancel-booking-reason" /></el-form-item>
        <el-button type="danger" native-type="submit" :loading="cancel.busy" data-testid="cancel-booking-submit">取消并退款</el-button>
      </el-form>
    </el-dialog>
  </el-card>
</template>

<style scoped>
.filters { display: flex; flex-wrap: wrap; gap: 8px 16px; align-items: center; margin-bottom: 12px; }
.error { margin-bottom: 12px; }
.pager-row { display: flex; justify-content: space-between; align-items: center; margin-top: 12px; }
.muted { color: #909399; font-size: 13px; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
</style>
```

- [ ] **Step 6: 机器详情、实例详情、仪表盘、订单**

`web/admin/src/views/MachineDetailView.vue`：
- `<script>` 加 `import { type AdminGroup, type Listed } from '@spark/shared'`（并入现有 import），vue 导入列表加 `reactive`（`import { computed, onMounted, reactive, ref } from 'vue'`）；状态 `const groups = ref<AdminGroup[]>([])`、`const edit = reactive({ visible: false, groupId: '' as string, displayName: '', busy: false })`；`onMounted` 时顺带 `api.get<Listed<AdminGroup>>('/groups')` 填 `groups`；方法：

```ts
function openEdit() {
  Object.assign(edit, { visible: true, groupId: data.value?.machine.group_id ? String(data.value.machine.group_id) : '', displayName: data.value?.machine.display_name ?? '' })
}
async function submitEdit() {
  if (edit.busy) return
  edit.busy = true
  try {
    await api.put(`/machines/${id.value}`, { group_id: edit.groupId ? Number(edit.groupId) : null, display_name: edit.displayName.trim() })
    ElMessage.success('已保存')
    edit.visible = false
    await load()
  } catch (e) {
    notifyError(e) // duplicate_display_name / group_not_found 原文
  } finally {
    edit.busy = false
  }
}
```

- 模板：第一张 `el-descriptions` 加两项 `<el-descriptions-item label="机组"><span data-testid="mdetail-group">{{ data.group?.name ?? '未分组' }}</span></el-descriptions-item>`、`<el-descriptions-item label="展示名"><span data-testid="mdetail-display">{{ data.machine.display_name || `机器 #${data.machine.id}` }}</span></el-descriptions-item>`；操作区加 `<el-button data-testid="mdetail-edit-btn" @click="openEdit">编辑机组 / 展示名</el-button>`；维护确认框文案加一段 `<p v-if="!inMaintenance && data.upcoming_bookings > 0" class="warn" data-testid="mdetail-maintenance-upcoming">该机器有 {{ data.upcoming_bookings }} 个未开通的预订，将在开通时自动改派。</p>`；「历史实例」卡片之前加：

```vue
      <el-card header="档期（未来 60 天）" data-testid="mdetail-leases">
        <table class="grid">
          <thead><tr><th>开始</th><th>结束</th><th>实例</th><th>用户</th></tr></thead>
          <tbody>
            <tr v-for="l in data.leases" :key="l.lower" data-testid="mdetail-lease-row">
              <td>{{ fmtTime(l.lower) }}</td><td>{{ fmtTime(l.upper) }}</td>
              <td><IdLink kind="instance" :id="l.instance_id" /></td><td><IdLink kind="user" :id="l.user_id" /></td>
            </tr>
            <tr v-if="!data.leases.length"><td colspan="4" class="muted">无占用</td></tr>
          </tbody>
        </table>
      </el-card>
```

- 编辑弹窗：

```vue
      <el-dialog v-model="edit.visible" title="编辑机组 / 展示名" width="460" align-center data-testid="medit-dialog">
        <el-form label-position="top" @submit.prevent="submitEdit">
          <el-form-item label="机组">
            <el-select v-model="edit.groupId" clearable placeholder="未分组" data-testid="medit-group"><el-option v-for="g in groups" :key="g.id" :label="g.name" :value="String(g.id)" /></el-select>
          </el-form-item>
          <el-form-item label="展示名（对用户，≤ 32 字；留空显示「机器 #ID」）"><el-input v-model="edit.displayName" maxlength="32" data-testid="medit-display" /></el-form-item>
          <el-button type="primary" native-type="submit" :loading="edit.busy" data-testid="medit-submit">保存</el-button>
        </el-form>
      </el-dialog>
```

（样式加 `.grid` 同实例详情、`.warn { color: var(--el-color-warning); }`。）

`web/admin/src/views/InstanceDetailView.vue`：
- import 加 `showCancelBooking, showReassign`；`el-descriptions` 加 `<el-descriptions-item v-if="inst.scheduled_start_at" label="预订开始"><span key="start" data-testid="adetail-start">{{ fmtTime(inst.scheduled_start_at) }} ～ {{ fmtTime(inst.scheduled_end_at) }}</span></el-descriptions-item>`、`<el-descriptions-item v-if="inst.batch_id" label="批次"><span key="batch" class="mono" data-testid="adetail-batch">{{ inst.batch_id }}</span></el-descriptions-item>`、`<el-descriptions-item label="改派次数"><span data-testid="adetail-reassigned">{{ inst.reassign_count }}</span></el-descriptions-item>`、`<el-descriptions-item label="自动续费"><span data-testid="adetail-auto-renew">{{ inst.auto_renew ? `开启（${labelOf(durationLabel, inst.auto_renew_duration)}）` : '关闭' }}{{ inst.auto_renew_failed_reason ? ` · 最近失败：${inst.auto_renew_failed_reason}` : '' }}</span></el-descriptions-item>`；订单表加「数量」「自动」两列（`data-testid="adetail-order-quantity"` / `adetail-order-auto`）。
- 操作区加 `<el-button v-if="showReassign(role, inst.state)" data-testid="adetail-reassign-btn" @click="openReassign">改派</el-button>` 与 `<el-button v-if="showCancelBooking(role, inst.state)" type="danger" plain data-testid="adetail-cancel-booking-btn" @click="openCancelBooking">取消预订</el-button>`；两个弹窗与 `BookingsView` 相同的 `reassign` / `cancel` 状态与提交函数（复制过来，`target` 固定为 `inst`，成功后 `await load()`），testid 同名 `reassign-dialog / reassign-machine / reassign-reason / reassign-submit / cancel-booking-dialog / cancel-booking-reason / cancel-booking-submit`。

`web/admin/src/views/DashboardView.vue`：`instanceStates` 数组加 `'reserved'`、`'cancelled'`（渲染用 `instanceStateLabel`），「实例」卡片里加 `<li><span>未来 7 天预订开始</span><b data-testid="dash-bookings-7d">{{ data.bookings_upcoming_7d }}</b></li>`。

`web/admin/src/views/OrdersView.vue`：租期列之后加 `<el-table-column label="数量" width="70"><template #default="{ row }"><span data-testid="ord-quantity">{{ row.quantity }}</span></template></el-table-column>`，状态列之后加 `<el-table-column label="批次" min-width="150"><template #default="{ row }"><span class="mono" data-testid="ord-batch">{{ row.batch_id ?? '—' }}</span></template></el-table-column>` 与 `<el-table-column label="自动" width="60"><template #default="{ row }"><span data-testid="ord-auto">{{ row.auto ? '是' : '' }}</span></template></el-table-column>`。

`web/admin/src/components/FinancePage.vue`：`emptyFilter` 加 `batch_id: ''`（从 `route.query.batch_id` 预填，仅 `orders` 端点发出）；`query()` 在 `endpoint === 'orders'` 时加 `batch_id: filter.batch_id`；筛选栏在「状态」之后加 `<el-form-item v-if="endpoint === 'orders'" label="批次"><el-input v-model="filter.batch_id" clearable data-testid="fin-filter-batch" style="width: 170px" /></el-form-item>`。

- [ ] **Step 7: 运行测试确认通过**

Run: `cd web && npm test 2>&1 | tail -6 && npm run build 2>&1 | tail -3`
Expected: Vitest 全绿（`menu.spec`、`router.spec` 若按路由数断言也同步 +2）；构建通过。

- [ ] **Step 8: 提交**

```bash
git add web/shared/src/types/admin.ts web/admin/src
git commit -m "feat(admin): groups page, bookings page (reassign/cancel), machine group/display-name edit and leases, instance booking ops, dashboard and order batch columns

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 21: Playwright 用例（控制台预订 / 详情 / 列表，后台机组 / 预订）、钩子、覆盖表

**Files:**
- Create: `web/e2e/console/booking.spec.ts`、`web/e2e/admin/groups.spec.ts`、`web/e2e/admin/bookings.spec.ts`、`web/e2e/helpers/booking.ts`
- Modify: `web/e2e/helpers/hooks.ts`（`machines` 的 `group_id`、`groups`）、`web/e2e/console/instance-detail.spec.ts`（追加 C-DET-17～24）、`web/e2e/console/instances.spec.ts`（追加 C-INS-05～07）、`web/e2e/admin/instances.spec.ts`（追加 A-INS-13）、`web/e2e/coverage.json`、`web/playwright.config.ts`（顺序说明注释）

**Interfaces:**
- Consumes: Task 16 的钩子；Task 18～20 的页面与 `data-testid`。
- Produces:
  - `hooks.machines(count, serial_prefix?, rack?, group_id?)`、`hooks.groups(name, visible = true) → { id }`。
  - `helpers/booking.ts`：`uniqueGroupName()`、`bookViaApi(api, opts: { startAt: string; count?: number; groupId?: number; machineId?: number; duration?: Duration; quantity?: number; autoRenew?: boolean; autoRenewDuration?: Duration })`（`POST /api/v1/bookings`，返回 `{ batchId, ids }`）、`startAfter(hours): Promise<{ iso: string; day: string; hour: number; date: Date }>`（服务器时间 + hours 向上取整到整点，北京时间日 / 时）、`groupMachineIds(groupId): Promise<number[]>`、`pickOption(page, testid, text)`。
  - 覆盖表 ID：`C-BKG-01`～`10`、`C-DET-17`～`24`、`C-INS-05`～`07`、`A-GRP-01`～`05`、`A-BKG-01`～`06`、`A-INS-13`，登记到 `coverage.json`。

- [ ] **Step 1: 钩子与 helper**

`web/e2e/helpers/hooks.ts`：`machines` 改为

```ts
  /** 造已接入并连上 fake Agent 的空闲机器；rack 缺省为 E2E；group_id 指定时登记后加入机组。 */
  async machines(count = 1, serial_prefix?: string, rack?: string, group_id?: number): Promise<number[]> {
    return (await post<{ ids: number[] }>('/hooks/machines', { count, serial_prefix, rack, group_id })).ids
  },
  /** 建一个机组（visible 缺省 true）。 */
  groups(name: string, visible = true) {
    return post<{ id: number }>('/hooks/groups', { name, visible })
  },
```

`web/e2e/helpers/booking.ts`：

```ts
import { expect, request, type APIRequestContext, type Page } from '@playwright/test'
import { CONSOLE_URL, hooks } from './hooks'
import { runStamp } from './phone'
import { cstDate } from './time'

let groupSeq = 0
export function uniqueGroupName(): string {
  groupSeq += 1
  return `G${runStamp}-${groupSeq}`
}

const HOUR = 3600_000
/** 服务器时间 + hours 向上取整到整点；返回 RFC3339、北京时间日期与小时。 */
export async function startAfter(hours: number) {
  const now = await hooks.clock()
  const t = Math.ceil((now.getTime() + hours * HOUR) / HOUR) * HOUR
  const date = new Date(t)
  const cst = new Date(t + 8 * HOUR)
  return { iso: date.toISOString(), day: cstDate(date), hour: cst.getUTCHours(), date }
}

export async function bookViaApi(api: APIRequestContext, opts: { startAt?: string; count?: number; groupId?: number; machineId?: number; duration?: '1d' | '1w' | '1m'; quantity?: number; autoRenew?: boolean; autoRenewDuration?: '1d' | '1w' | '1m' }) {
  const products = (await (await api.get('/api/v1/products')).json()).items as { id: number }[]
  const images = (await (await api.get('/api/v1/images')).json()).items as { id: number }[]
  const r = await api.post('/api/v1/bookings', {
    data: {
      product_id: products[0].id, image_id: images[0].id, group_id: opts.groupId, machine_id: opts.machineId, count: opts.count ?? 1,
      duration: opts.duration ?? '1d', quantity: opts.quantity ?? 1, start_at: opts.startAt, auto_renew: opts.autoRenew ?? false, auto_renew_duration: opts.autoRenewDuration,
    },
    headers: { 'Idempotency-Key': crypto.randomUUID() },
  })
  if (r.status() !== 201) throw new Error(`booking ${r.status()} ${await r.text()}`)
  const out = (await r.json()) as { batch: { id: string } | null; instances: { id: string; machine_label: string }[] }
  return { batchId: out.batch?.id ?? null, ids: out.instances.map((i) => i.id), labels: out.instances.map((i) => i.machine_label) }
}

/** 控制台匿名接口：机组内机器 ID（用于指定机器）。 */
export async function groupMachineIds(groupId: number): Promise<number[]> {
  const ctx = await request.newContext({ baseURL: CONSOLE_URL })
  const r = await ctx.get(`/api/v1/groups/${groupId}/machines`)
  if (!r.ok()) throw new Error(`group machines ${r.status()} ${await r.text()}`)
  return ((await r.json()).items as { id: number }[]).map((m) => m.id)
}

/** 整串精确匹配的正则（机组名都以 G<stamp>- 开头，子串 / 前缀匹配会命中多个选项或行）。 */
export function exact(text: string): RegExp {
  return new RegExp(`^${text.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}$`)
}

/** Element Plus 下拉：点开后选择文本匹配的选项。 */
export async function pickOption(page: Page, testid: string, text: string | RegExp) {
  await page.getByTestId(testid).click()
  await page.getByRole('option', { name: text }).click()
  await expect(page.getByRole('option', { name: text })).toBeHidden()
}
```

- [ ] **Step 2: 控制台预订用例**

`web/e2e/console/booking.spec.ts`：

```ts
import { expect, test, type Page } from '@playwright/test'
import { clickTwiceSync } from '../helpers/admin'
import { bookViaApi, exact, groupMachineIds, pickOption, startAfter, uniqueGroupName } from '../helpers/booking'
import { dialog, loginConsole, nav, seedUser, toast, userApi } from '../helpers/console'
import { hooks } from '../helpers/hooks'
import { cstDate } from '../helpers/time'

const card = (page: Page) => page.getByTestId('market-card').first()

async function openBooking(page: Page) {
  await card(page).getByTestId('market-book-btn').click()
  await expect(page.getByTestId('rent-dialog')).toBeVisible()
}

/** 填入「指定时间」：日期与第一个可选整点（或指定小时）。 */
async function fillStart(page: Page, day: string, hour?: number) {
  await page.getByTestId('rent-start-later').click()
  await page.getByTestId('rent-day').fill(day)
  if (hour != null) await pickOption(page, 'rent-hour', `${String(hour).padStart(2, '0')}:00`)
}

test('C-BKG-01 预订对话框：机组 / 数量 / 租期联动与可用台数', async ({ page }, info) => {
  const name = uniqueGroupName()
  const g = await hooks.groups(name)
  await hooks.machines(2, undefined, undefined, g.id)
  const u = await seedUser({ balance: 1_000_000 })
  await loginConsole(page, u.phone)
  await nav(page, info, 'market')
  await openBooking(page)
  await pickOption(page, 'rent-group', exact(`${name}（共 2 · 现空闲 2）`))
  await expect(page.getByTestId('rent-availability')).toHaveText(/可预订 2 台/)
  await page.getByTestId('rent-count').locator('input').fill('3')
  await page.getByTestId('rent-count').locator('input').blur()
  await expect(page.getByTestId('rent-availability')).toHaveText(/不足 3 台/)
  await expect(page.getByTestId('rent-next')).toBeDisabled()
  await page.getByTestId('rent-count').locator('input').fill('1')
  await page.getByTestId('rent-count').locator('input').blur()
  await page.getByTestId('rent-duration-1m').click()
  await page.getByTestId('rent-quantity').locator('input').fill('4')
  await page.getByTestId('rent-quantity').locator('input').blur()
  await expect(page.getByTestId('rent-duration-text')).toHaveText(/1 月 × 3 = 90 天/)
  await expect(page.getByTestId('rent-next')).toBeEnabled()
})

test('C-BKG-02 机组日历：点击日期填入开始日期', async ({ page }, info) => {
  const name = uniqueGroupName()
  const g = await hooks.groups(name)
  await hooks.machines(1, undefined, undefined, g.id)
  const u = await seedUser()
  const tomorrow = cstDate(new Date((await hooks.clock()).getTime() + 24 * 3600e3))
  await loginConsole(page, u.phone)
  await nav(page, info, 'market')
  await openBooking(page)
  await pickOption(page, 'rent-group', exact(`${name}（共 1 · 现空闲 1）`))
  await page.getByTestId('rent-start-later').click()
  await expect(page.getByTestId('rent-calendar')).toBeVisible()
  await page.getByTestId(`rent-day-${tomorrow}`).click()
  await expect(page.getByTestId('rent-day')).toHaveValue(tomorrow)
})

test('C-BKG-03 指定机器与占用条带', async ({ page }, info) => {
  const name = uniqueGroupName()
  const g = await hooks.groups(name)
  await hooks.machines(1, undefined, undefined, g.id)
  const [mid] = await groupMachineIds(g.id)
  const u = await seedUser({ balance: 1_000_000 })
  const api = await userApi(u.phone)
  const s = await startAfter(30)
  await bookViaApi(api, { startAt: s.iso, groupId: g.id, machineId: mid })
  await loginConsole(page, u.phone)
  await nav(page, info, 'market')
  await openBooking(page)
  await pickOption(page, 'rent-group', exact(`${name}（共 1 · 现空闲 1）`))
  await expect(page.getByTestId('rent-machine')).toBeVisible()
  await pickOption(page, 'rent-machine', /机器 #/)
  await expect(page.getByTestId('rent-machine-periods').locator('li')).toHaveCount(1)
  await expect(page.getByTestId('rent-machine-periods')).toContainText('（我的）')
  await page.getByTestId('rent-count').locator('input').fill('2')
  await page.getByTestId('rent-count').locator('input').blur()
  await expect(page.getByTestId('rent-machine')).toHaveCount(0)
})

test('C-BKG-04 预订成功跳转详情并显示已预订', async ({ page }, info) => {
  const name = uniqueGroupName() // 库整轮共享：只有按本用例的机组计数才可控
  const g = await hooks.groups(name)
  await hooks.machines(1, undefined, undefined, g.id)
  const u = await seedUser({ balance: 1_000_000 })
  const s = await startAfter(26)
  await loginConsole(page, u.phone)
  await nav(page, info, 'market')
  await openBooking(page)
  await pickOption(page, 'rent-group', exact(`${name}（共 1 · 现空闲 1）`))
  await fillStart(page, s.day, s.hour)
  await expect(page.getByTestId('rent-availability')).toHaveText(/可预订 1 台/)
  await page.getByTestId('rent-next').click()
  await expect(page.getByTestId('market-confirm-start')).toHaveText(`${s.day} ${String(s.hour).padStart(2, '0')}:00`)
  await expect(page.getByTestId('market-confirm-price')).toHaveText('¥69.00')
  await page.getByTestId('market-confirm-ok').click()
  await toast(page, '预订成功')
  await expect(page).toHaveURL(/\/instances\/ins-[a-z0-9]{8}$/)
  await expect(page.getByTestId('detail-state')).toHaveText('已预订')
  await expect(page.getByTestId('detail-start')).toBeVisible()
  await expect(page.getByTestId('detail-cancel-btn')).toBeVisible()
  await expect(page.getByTestId('detail-ssh-hint')).toHaveText('开通后可连接')
})

test('C-BKG-05 一次 3 台即时下单，按批次筛选显示', async ({ page }, info) => {
  const name = uniqueGroupName()
  const g = await hooks.groups(name)
  await hooks.machines(3, undefined, undefined, g.id)
  const u = await seedUser({ balance: 1_000_000 })
  await loginConsole(page, u.phone)
  await nav(page, info, 'market')
  await openBooking(page)
  await page.getByTestId('rent-start-now').click() // 预订档期按钮打开的是「指定时间」模式，这里要即时
  await pickOption(page, 'rent-group', exact(`${name}（共 3 · 现空闲 3）`))
  await page.getByTestId('rent-count').locator('input').fill('3')
  await page.getByTestId('rent-count').locator('input').blur()
  await expect(page.getByTestId('rent-availability')).toHaveText(/可租 3 台/)
  await page.getByTestId('rent-next').click()
  await expect(page.getByTestId('market-confirm-count')).toHaveText('3 台')
  await expect(page.getByTestId('market-confirm-price')).toHaveText('¥207.00')
  await page.getByTestId('market-confirm-ok').click()
  await toast(page, '已下单 3 台')
  await expect(page).toHaveURL(/\/instances\?batch=bat-/)
  await expect(page.getByTestId('instances-batch-head')).toContainText('3 台')
  await expect(page.getByTestId('instance-id')).toHaveCount(3)
})

test('C-BKG-06 台数不足：按钮禁用并提示', async ({ page }, info) => {
  const name = uniqueGroupName()
  const g = await hooks.groups(name)
  await hooks.machines(1, undefined, undefined, g.id)
  const [mid] = await groupMachineIds(g.id)
  const u = await seedUser({ balance: 1_000_000 })
  const api = await userApi(u.phone)
  const s = await startAfter(26)
  await bookViaApi(api, { startAt: s.iso, groupId: g.id, machineId: mid }) // 订满该组唯一的机器
  await loginConsole(page, u.phone)
  await nav(page, info, 'market')
  await openBooking(page)
  await pickOption(page, 'rent-group', exact(`${name}（共 1 · 现空闲 1）`))
  await fillStart(page, s.day, s.hour)
  await expect(page.getByTestId('rent-availability')).toHaveText(/可预订 0 台，不足 1 台/)
  await expect(page.getByTestId('rent-next')).toBeDisabled()
})

test('C-BKG-07 余额不足改为去充值', async ({ page }, info) => {
  await hooks.machines(2)
  const u = await seedUser({ balance: 5000 })
  await loginConsole(page, u.phone)
  await nav(page, info, 'market')
  await openBooking(page)
  await page.getByTestId('rent-count').locator('input').fill('2')
  await page.getByTestId('rent-count').locator('input').blur()
  await page.getByTestId('rent-next').click()
  await expect(page.getByTestId('market-confirm-after')).toContainText('余额不足')
  await expect(page.getByTestId('market-confirm-ok')).toHaveText('去充值')
  await page.getByTestId('market-confirm-ok').click()
  await expect(page).toHaveURL(/\/wallet\/recharge\?redirect=/)
})

test('C-BKG-08 开始时间过近：后端文案回到表单', async ({ page }, info) => {
  await hooks.machines(1)
  const u = await seedUser({ balance: 1_000_000 })
  await loginConsole(page, u.phone)
  await nav(page, info, 'market')
  await openBooking(page)
  const s = await startAfter(2)
  await fillStart(page, s.day, s.hour)
  await page.getByTestId('rent-next').click()
  await hooks.advance(3 * 3600) // 提交前服务器时间已过去 3 小时：该整点不再满足「2 小时后」
  await page.getByTestId('market-confirm-ok').click()
  await toast(page, '开始时间至少在 2 小时之后')
  await expect(page.getByTestId('rent-dialog')).toBeVisible()
})

test('C-BKG-09 预订确认双击只发一次请求', async ({ page }, info) => {
  await hooks.machines(2)
  const u = await seedUser({ balance: 1_000_000 })
  const posts: string[] = []
  page.on('request', (r) => {
    if (r.method() === 'POST' && new URL(r.url()).pathname === '/api/v1/bookings') posts.push(r.headers()['idempotency-key'] ?? '')
  })
  await loginConsole(page, u.phone)
  await nav(page, info, 'market')
  await openBooking(page)
  await page.getByTestId('rent-count').locator('input').fill('2')
  await page.getByTestId('rent-count').locator('input').blur()
  await page.getByTestId('rent-next').click()
  await clickTwiceSync(page.getByTestId('market-confirm-ok'))
  await expect(page).toHaveURL(/\/instances\?batch=/)
  expect(posts).toHaveLength(1)
  expect(posts[0]).toMatch(/^[0-9a-f-]{36}$/)
})

test('C-BKG-10 未登录与未实名点预订的引导', async ({ page }, info) => {
  await hooks.machines(1)
  await page.goto('/market')
  await card(page).getByTestId('market-book-btn').click()
  await expect(page).toHaveURL(/\/login\?redirect=/)
  const u = await seedUser({ verified: false })
  await loginConsole(page, u.phone)
  await nav(page, info, 'market')
  await card(page).getByTestId('market-book-btn').click()
  await expect(dialog(page).filter({ has: page.getByTestId('guide-dialog') })).toContainText('请先完成实名认证')
  await expect(page.getByTestId('rent-dialog')).toHaveCount(0)
})
```

- [ ] **Step 3: 实例详情 / 列表追加**

`web/e2e/console/instance-detail.spec.ts` 追加（import 加 `bookViaApi, groupMachineIds, startAfter, uniqueGroupName` from `../helpers/booking`、`cstTime` from `../helpers/time`）：

```ts
test('C-DET-17 预订详情：开始 / 结束 / 机器 / 批次，无释放按钮', async ({ page }) => {
  const g = await hooks.groups(uniqueGroupName())
  await hooks.machines(1, undefined, undefined, g.id)
  const u = await seedUser({ balance: 1_000_000 })
  const api = await userApi(u.phone)
  const s = await startAfter(30)
  const { ids, batchId } = await bookViaApi(api, { startAt: s.iso, groupId: g.id })
  await loginConsole(page, u.phone)
  await page.goto(`/instances/${ids[0]}`)
  await expect(page.getByTestId('detail-state')).toHaveText('已预订')
  await expect(page.getByTestId('detail-group')).toHaveText(/^G/)
  await expect(page.getByTestId('detail-machine')).toHaveText(/机器 #\d+/)
  await expect(page.getByTestId('detail-start')).toHaveText(cstTime(s.iso))
  await expect(page.getByTestId('detail-end')).toHaveText(cstTime(new Date(s.date.getTime() + 86400e3).toISOString()))
  await expect(page.getByTestId('detail-batch')).toHaveText(batchId!)
  await expect(page.getByTestId('detail-release-btn')).toHaveCount(0)
  await expect(page.getByTestId('detail-cancel-btn')).toBeVisible()
})

test('C-DET-18 取消（距开始 ≥ 24h）：全额退款', async ({ page }) => {
  await hooks.machines(1)
  const u = await seedUser({ balance: 100000 })
  const api = await userApi(u.phone)
  const s = await startAfter(40)
  const { ids } = await bookViaApi(api, { startAt: s.iso })
  await hooks.advance(16 * 60) // 过冷静期，仍 ≥ 24h
  await loginConsole(page, u.phone)
  await page.goto(`/instances/${ids[0]}`)
  await page.getByTestId('detail-cancel-btn').click()
  await expect(page.getByTestId('detail-cancel-text')).toContainText('将退款 ¥69.00（100%）')
  await page.getByTestId('detail-cancel-ok').click()
  await toast(page, '已取消预订')
  await expect(page.getByTestId('detail-state')).toHaveText('已取消')
  await expect(page.getByTestId('detail-cancel-btn')).toHaveCount(0)
  expect((await (await api.get('/api/v1/wallet')).json()).balance).toBe(100000)
})

test('C-DET-19 取消（距开始 < 24h，已过冷静期）：退 50%', async ({ page }) => {
  await hooks.machines(1)
  const u = await seedUser({ balance: 100000 })
  const api = await userApi(u.phone)
  const s = await startAfter(3)
  const { ids } = await bookViaApi(api, { startAt: s.iso })
  await hooks.advance(16 * 60)
  await loginConsole(page, u.phone)
  await page.goto(`/instances/${ids[0]}`)
  await page.getByTestId('detail-cancel-btn').click()
  await expect(page.getByTestId('detail-cancel-text')).toContainText('将退款 ¥34.50（50%）')
  await page.getByTestId('detail-cancel-ok').click()
  await toast(page, '已取消预订')
  expect((await (await api.get('/api/v1/wallet')).json()).balance).toBe(100000 - 6900 + 3450)
})

test('C-DET-20 开通后取消按钮消失，续费仍可用', async ({ page }) => {
  await hooks.machines(1)
  const u = await seedUser({ balance: 1_000_000 })
  const api = await userApi(u.phone)
  const s = await startAfter(3)
  const { ids } = await bookViaApi(api, { startAt: s.iso })
  await hooks.advance(Math.ceil((s.date.getTime() - (await hooks.clock()).getTime()) / 1000) - 3600)
  await hooks.sweep()
  await loginConsole(page, u.phone)
  await page.goto(`/instances/${ids[0]}`)
  await expect(page.getByTestId('detail-state')).toHaveText('开通中')
  await expect(page.getByTestId('detail-cancel-btn')).toHaveCount(0)
  await expect(page.getByTestId('detail-renew-btn')).toBeVisible()
  await hooks.agent()
  await page.reload()
  await expect(page.getByTestId('detail-state')).toHaveText('运行中')
})

test('C-DET-21 续费表单显示可续至并禁用超限档位；接口提交超限返回 renew_blocked', async ({ page }) => {
  const g = await hooks.groups(uniqueGroupName())
  await hooks.machines(1, undefined, undefined, g.id)
  const [mid] = await groupMachineIds(g.id)
  const u = await seedUser({ balance: 1_000_000 })
  const api = await userApi(u.phone)
  const { ids } = await bookViaApi(api, { groupId: g.id, machineId: mid }) // 即时分支、钉在 mid（runningInstance 会落在任意空闲机器）
  await hooks.agent()
  const id = ids[0]
  const s = await startAfter(72)
  await bookViaApi(api, { startAt: s.iso, groupId: g.id, machineId: mid })
  await loginConsole(page, u.phone)
  await page.goto(`/instances/${id}`)
  await page.getByTestId('detail-renew-btn').click()
  await expect(page.getByTestId('detail-renew-until')).toContainText('可续至')
  // testid 在 el-radio-button 的 <label> 上，disabled 落在内部 <input>
  await expect(page.getByTestId('detail-renew-duration-1w').locator('input')).toBeDisabled()
  await expect(page.getByTestId('detail-renew-duration-1d').locator('input')).toBeEnabled()
  const r = await api.post(`/api/v1/instances/${id}/renew`, { data: { duration: '1w', quantity: 1 }, headers: { 'Idempotency-Key': crypto.randomUUID() } })
  expect(r.status()).toBe(409)
  expect(((await r.json()) as { code: string; message: string }).message).toMatch(/^该机器自 .* 起已有预订，本次最多可续至 /)
})

test('C-DET-22 自动续费开关；余额不足时失败提示', async ({ page }) => {
  const u = await seedUser({ balance: 7000 })
  const { id } = await runningInstance(u.phone) // 余额剩 1 元
  await loginConsole(page, u.phone)
  await page.goto(`/instances/${id}`)
  await page.getByTestId('detail-auto-renew').click()
  await toast(page, '已开启自动续费')
  await hooks.advance(23 * 3600)
  await hooks.sweep()
  await page.reload()
  await expect(page.getByTestId('detail-auto-renew-failed')).toContainText('自动续费失败：余额不足')
  await expect(page.getByTestId('detail-state')).toHaveText('运行中')
})

test('C-DET-23 自动续费被预订挡住时降档，动态显示实际档位', async ({ page }) => {
  const g = await hooks.groups(uniqueGroupName())
  await hooks.machines(1, undefined, undefined, g.id)
  const [mid] = await groupMachineIds(g.id)
  const u = await seedUser({ balance: 1_000_000 })
  const api = await userApi(u.phone)
  const { ids } = await bookViaApi(api, { groupId: g.id, machineId: mid }) // 即时、钉在 mid；expires = now + 1d
  await hooks.agent()
  const id = ids[0]
  await api.put(`/api/v1/instances/${id}/auto-renew`, { data: { enabled: true, duration: '1m' } })
  // 下一预订开始 = now + 1d + 8d + 8h（整点）→ 可续至 now + 1d + 8d：1m 放不下、1w 放得下
  const s = await startAfter(24 + 8 * 24 + 8)
  await bookViaApi(api, { startAt: s.iso, groupId: g.id, machineId: mid })
  await hooks.advance(23 * 3600)
  await hooks.sweep()
  await loginConsole(page, u.phone)
  await page.goto(`/instances/${id}`)
  await expect(page.getByTestId('detail-event').filter({ hasText: '自动续费成功（1 周）' })).toBeVisible()
})

test('C-DET-24 时间线：下单、开通、开通成功', async ({ page }) => {
  await hooks.machines(1)
  const u = await seedUser({ balance: 1_000_000 })
  const api = await userApi(u.phone)
  const s = await startAfter(3)
  const { ids } = await bookViaApi(api, { startAt: s.iso })
  await hooks.advance(Math.ceil((s.date.getTime() - (await hooks.clock()).getTime()) / 1000) - 3600)
  await hooks.sweep()
  await hooks.agent()
  await loginConsole(page, u.phone)
  await page.goto(`/instances/${ids[0]}`)
  const kinds = page.getByTestId('detail-event')
  await expect(kinds).toHaveCount(3)
  await expect(kinds.nth(0)).toHaveAttribute('data-kind', 'created')
  await expect(kinds.nth(1)).toHaveAttribute('data-kind', 'provision')
  await expect(kinds.nth(2)).toHaveAttribute('data-kind', 'order')
  await expect(kinds.nth(2)).toContainText('预订下单')
})
```

`web/e2e/console/instances.spec.ts` 追加（import 加 `bookViaApi, groupMachineIds, startAfter, uniqueGroupName`、`adminApi, adminPost, createAdmin`）：

```ts
test('C-INS-05 预订到点变为开通中，Agent 完成后运行中', async ({ page }, info) => {
  await hooks.machines(1)
  const u = await seedUser({ balance: 1_000_000 })
  const api = await userApi(u.phone)
  const s = await startAfter(3)
  const { ids } = await bookViaApi(api, { startAt: s.iso })
  await loginConsole(page, u.phone)
  await nav(page, info, 'instances')
  const row = instanceRow(page, info, ids[0])
  await expect(row.getByTestId('instance-state')).toHaveText('已预订')
  await expect(row.getByTestId('instance-cancellable')).toBeVisible()
  await hooks.advance(Math.ceil((s.date.getTime() - (await hooks.clock()).getTime()) / 1000) - 3600)
  await hooks.sweep()
  await page.reload()
  await expect(instanceRow(page, info, ids[0]).getByTestId('instance-state')).toHaveText('开通中')
  await hooks.agent()
  await page.reload()
  await expect(instanceRow(page, info, ids[0]).getByTestId('instance-state')).toHaveText('运行中')
})

test('C-INS-06 预订机器进入维护后到点改派，列表显示已更换机器', async ({ page }, info) => {
  const g = await hooks.groups(uniqueGroupName())
  await hooks.machines(2, undefined, undefined, g.id)
  const [m1] = await groupMachineIds(g.id)
  const u = await seedUser({ balance: 1_000_000 })
  const api = await userApi(u.phone)
  const s = await startAfter(3)
  const { ids } = await bookViaApi(api, { startAt: s.iso, groupId: g.id, machineId: m1 })
  const admin = await createAdmin('ops')
  await adminPost(await adminApi(admin), admin, `/admin/api/v1/machines/${m1}/maintenance`, { on: true })
  await hooks.advance(Math.ceil((s.date.getTime() - (await hooks.clock()).getTime()) / 1000) - 3600)
  await hooks.sweep()
  await loginConsole(page, u.phone)
  await nav(page, info, 'instances')
  const row = instanceRow(page, info, ids[0])
  await expect(row.getByTestId('instance-state')).toHaveText('开通中')
  await expect(row.getByTestId('instance-reassigned')).toBeVisible()
})

test('C-INS-07 批次视图一键取消全部未开通预订', async ({ page }, info) => {
  await hooks.machines(2)
  const u = await seedUser({ balance: 1_000_000 })
  const api = await userApi(u.phone)
  const s = await startAfter(30)
  const { batchId } = await bookViaApi(api, { startAt: s.iso, count: 2 })
  await loginConsole(page, u.phone)
  await page.goto(`/instances?batch=${batchId}`)
  await expect(page.getByTestId('instances-batch-head')).toContainText('2 台')
  await page.getByTestId('instances-cancel-all').click()
  await toast(page, '已取消 2 台')
  await expect(page.getByTestId('instance-state')).toHaveText(['已取消', '已取消'])
  await expect(page.getByTestId('instances-cancel-all')).toHaveCount(0)
  expect(info.project.name).toMatch(/console/)
})
```

`web/e2e/admin/instances.spec.ts` 追加（import 加 `bookViaApi, startAfter`、`userApi`）：

```ts
test('A-INS-13 实例详情显示预订字段与只读自动续费', async ({ page }) => {
  await hooks.machines(1)
  const u = await seedUser({ balance: 1_000_000 })
  const api = await userApi(u.phone)
  const s = await startAfter(30)
  const { ids, batchId } = await bookViaApi(api, { startAt: s.iso, autoRenew: true, autoRenewDuration: '1w' })
  const admin = await createAdmin('finance')
  await loginAdmin(page, admin)
  await page.goto(`/admin/instances/${ids[0]}`)
  await expect(page.getByTestId('adetail-state')).toHaveText('已预订')
  await expect(page.getByTestId('adetail-start')).toContainText(cstTime(s.iso))
  await expect(page.getByTestId('adetail-batch')).toHaveText(batchId!)
  await expect(page.getByTestId('adetail-auto-renew')).toHaveText('开启（1 周）')
  await expect(page.getByTestId('adetail-reassign-btn')).toHaveCount(0) // finance 无操作
  await expect(page.getByTestId('adetail-cancel-booking-btn')).toHaveCount(0)
})
```

- [ ] **Step 4: 后台机组与预订用例**

`web/e2e/admin/groups.spec.ts`：

```ts
import { expect, test } from '@playwright/test'
import { adminApi, createAdmin, getJSON, loginAdmin } from '../helpers/admin'
import { exact, pickOption, uniqueGroupName } from '../helpers/booking'
import { toast } from '../helpers/console'
import { hooks } from '../helpers/hooks'

test('A-GRP-01 新建 / 编辑机组与可见开关', async ({ page }) => {
  const admin = await createAdmin('ops')
  const name = uniqueGroupName()
  await loginAdmin(page, admin)
  await page.getByTestId('menu-groups').click()
  await page.getByTestId('groups-create-btn').click()
  await page.getByTestId('group-form-name').fill(name)
  await page.getByTestId('group-form-region').fill('一楼')
  await page.getByTestId('group-form-submit').click()
  await toast(page, '机组已创建')
  const row = page.getByRole('row').filter({ has: page.getByTestId('group-name').filter({ hasText: exact(name) }) })
  await expect(row.getByTestId('group-region')).toHaveText('一楼')
  await expect(row.getByTestId('group-visible')).toHaveText('可见')
  await row.getByTestId('group-edit').click()
  await page.getByTestId('group-form-visible').click()
  await page.getByTestId('group-form-submit').click()
  await toast(page, '机组已更新')
  await expect(row.getByTestId('group-visible')).toHaveText('仅后台')
  // 重名
  await page.getByTestId('groups-create-btn').click()
  await page.getByTestId('group-form-name').fill(name)
  await page.getByTestId('group-form-submit').click()
  await toast(page, '机组名称已存在')
})

test('A-GRP-02 删除：有机器时拒绝，移出后可删', async ({ page }) => {
  const admin = await createAdmin('ops')
  const name = uniqueGroupName()
  const g = await hooks.groups(name)
  await hooks.machines(1, undefined, undefined, g.id)
  await loginAdmin(page, admin)
  await page.goto('/admin/groups')
  const row = page.getByRole('row').filter({ has: page.getByTestId('group-name').filter({ hasText: exact(name) }) })
  await row.getByTestId('group-delete').click()
  await page.getByTestId('group-delete-ok').click()
  await toast(page, '机组内还有机器或未结束的实例')
  await row.getByTestId('group-machines').click()
  await page.getByTestId('gm-remove').first().click()
  await expect(page.getByTestId('gm-row')).toHaveCount(0)
  await page.keyboard.press('Escape')
  await row.getByTestId('group-delete').click()
  await page.getByTestId('group-delete-ok').click()
  await toast(page, '机组已删除')
  await expect(row).toHaveCount(0)
})

test('A-GRP-03 批量加入 / 移出机器', async ({ page }) => {
  const admin = await createAdmin('ops')
  const name = uniqueGroupName()
  const g = await hooks.groups(name)
  const ids = await hooks.machines(2)
  await loginAdmin(page, admin)
  await page.goto('/admin/groups')
  const row = page.getByRole('row').filter({ has: page.getByTestId('group-name').filter({ hasText: exact(name) }) })
  await row.getByTestId('group-machines').click()
  await page.getByTestId('gm-add-input').fill(ids.join(','))
  await page.getByTestId('gm-add').click()
  await toast(page, '已加入 2 台')
  await expect(page.getByTestId('gm-row')).toHaveCount(2)
  await expect(row.getByTestId('group-count')).toHaveText('2')
  await page.getByTestId('gm-remove').first().click()
  await expect(page.getByTestId('gm-row')).toHaveCount(1)
  await expect(row.getByTestId('group-count')).toHaveText('1')
})

test('A-GRP-04 展示名：保存、重名拒绝', async ({ page }) => {
  const admin = await createAdmin('ops')
  const name = uniqueGroupName()
  const g = await hooks.groups(name)
  await hooks.machines(2, undefined, undefined, g.id)
  await loginAdmin(page, admin)
  await page.goto('/admin/groups')
  const row = page.getByRole('row').filter({ has: page.getByTestId('group-name').filter({ hasText: exact(name) }) })
  await row.getByTestId('group-machines').click()
  const label = `${name}-01`
  await page.getByTestId('gm-display-input').first().fill(label)
  await page.getByTestId('gm-display-save').first().click()
  await toast(page, '展示名已保存')
  await page.getByTestId('gm-display-input').nth(1).fill(label)
  await page.getByTestId('gm-display-save').nth(1).click()
  await toast(page, '展示名已被其他机器使用')
})

test('A-GRP-05 机器详情编辑机组与展示名', async ({ page }) => {
  const admin = await createAdmin('ops')
  const name = uniqueGroupName()
  await hooks.groups(name)
  const [mid] = await hooks.machines(1)
  await loginAdmin(page, admin)
  await page.goto(`/admin/machines/${mid}`)
  await expect(page.getByTestId('mdetail-group')).toHaveText('未分组')
  await page.getByTestId('mdetail-edit-btn').click()
  await pickOption(page, 'medit-group', name)
  await page.getByTestId('medit-display').fill(`${name}-X`)
  await page.getByTestId('medit-submit').click()
  await toast(page, '已保存')
  await expect(page.getByTestId('mdetail-group')).toHaveText(name)
  await expect(page.getByTestId('mdetail-display')).toHaveText(`${name}-X`)
  const m = await getJSON<{ machine: { group_id: number | null; display_name: string } }>(await adminApi(admin), `/admin/api/v1/machines/${mid}`)
  expect(m.machine.display_name).toBe(`${name}-X`)
  expect(m.machine.group_id).not.toBeNull()
})
```

`web/e2e/admin/bookings.spec.ts`：

```ts
import { expect, test } from '@playwright/test'
import { adminApi, createAdmin, enterTotpDialog, getJSON, loginAdmin } from '../helpers/admin'
import { bookViaApi, groupMachineIds, startAfter, uniqueGroupName } from '../helpers/booking'
import { seedUser, toast, userApi } from '../helpers/console'
import { hooks } from '../helpers/hooks'
import { cstTime } from '../helpers/time'

async function seedBooking(groupMachines = 2) {
  const g = await hooks.groups(uniqueGroupName())
  await hooks.machines(groupMachines, undefined, undefined, g.id)
  const mids = await groupMachineIds(g.id)
  const u = await seedUser({ balance: 1_000_000 })
  const api = await userApi(u.phone)
  const s = await startAfter(30)
  const { ids } = await bookViaApi(api, { startAt: s.iso, groupId: g.id, machineId: mids[0] })
  return { g, mids, u, api, s, id: ids[0] }
}

test('A-BKG-01 预订列表：按用户筛选，显示开始 / 机组 / 机器', async ({ page }) => {
  const { u, s, id, g } = await seedBooking()
  const admin = await createAdmin('finance')
  await loginAdmin(page, admin)
  await page.getByTestId('menu-bookings').click()
  await page.getByTestId('bookings-filter-user').fill(String(u.id))
  await page.getByTestId('bookings-search').click()
  const row = page.getByTestId('booking-row').filter({ hasText: id })
  await expect(row).toHaveCount(1)
  const tr = page.getByRole('row').filter({ has: row })
  await expect(tr.getByTestId('booking-start')).toHaveText(cstTime(s.iso))
  await expect(tr.getByTestId('booking-group')).toHaveText(/^G/)
  await expect(tr.getByTestId('booking-reassign')).toHaveCount(0) // finance 无操作
  expect(g.id).toBeGreaterThan(0)
})

test('A-BKG-02 改派到指定机器并写审计；再改回同一台被拒', async ({ page }) => {
  const { u, mids, id } = await seedBooking()
  const admin = await createAdmin('ops')
  await loginAdmin(page, admin)
  await page.goto('/admin/bookings')
  await page.getByTestId('bookings-filter-user').fill(String(u.id))
  await page.getByTestId('bookings-search').click()
  const tr = page.getByRole('row').filter({ has: page.getByTestId('booking-row').filter({ hasText: id }) })
  await tr.getByTestId('booking-reassign').click()
  await page.getByTestId('reassign-machine').fill(String(mids[1]))
  await page.getByTestId('reassign-reason').fill('换机柜')
  await page.getByTestId('reassign-submit').click()
  await toast(page, `已改派到机器 ${mids[1]}`)
  await expect(tr.getByTestId('booking-machine')).toHaveText(String(mids[1]))
  await expect(tr.getByTestId('booking-reassigned')).toHaveText('1')
  await tr.getByTestId('booking-reassign').click()
  await page.getByTestId('reassign-machine').fill(String(mids[1]))
  await page.getByTestId('reassign-reason').fill('再来')
  await page.getByTestId('reassign-submit').click()
  await toast(page, '该机器在所选时段不可用')
  const audit = await getJSON<{ total: number }>(await adminApi(admin), `/admin/api/v1/audit?action=booking.reassign&target_id=${id}`)
  expect(audit.total).toBe(1)
})

test('A-BKG-03 取消预订需要动态码，全额退款', async ({ page }) => {
  const { u, api, id } = await seedBooking(1)
  const admin = await createAdmin('ops')
  await loginAdmin(page, admin)
  await page.goto('/admin/bookings')
  await page.getByTestId('bookings-filter-user').fill(String(u.id))
  await page.getByTestId('bookings-search').click()
  const tr = page.getByRole('row').filter({ has: page.getByTestId('booking-row').filter({ hasText: id }) })
  await tr.getByTestId('booking-cancel').click()
  await page.getByTestId('cancel-booking-reason').fill('机房断电')
  await page.getByTestId('cancel-booking-submit').click()
  await enterTotpDialog(page, admin.totp_secret!)
  await toast(page, '已取消预订，退款 ¥69.00')
  await expect(tr).toHaveCount(0)
  expect((await (await api.get('/api/v1/wallet')).json()).balance).toBe(1_000_000)
})

test('A-BKG-04 机器详情显示档期', async ({ page }) => {
  const { mids, id } = await seedBooking(1)
  const admin = await createAdmin('ops')
  await loginAdmin(page, admin)
  await page.goto(`/admin/machines/${mids[0]}`)
  await expect(page.getByTestId('mdetail-lease-row')).toHaveCount(1)
  await expect(page.getByTestId('mdetail-lease-row')).toContainText(id)
})

test('A-BKG-05 进入维护时提示未开通预订数量', async ({ page }) => {
  const { mids } = await seedBooking(1)
  const admin = await createAdmin('ops')
  await loginAdmin(page, admin)
  await page.goto(`/admin/machines/${mids[0]}`)
  await page.getByTestId('mdetail-maintenance-btn').click()
  await expect(page.getByTestId('mdetail-maintenance-upcoming')).toHaveText('该机器有 1 个未开通的预订，将在开通时自动改派。')
  await page.getByTestId('mdetail-maintenance-ok').click()
  await toast(page, '已进入维护')
})

test('A-BKG-06 仪表盘显示未来 7 天预订开始台数', async ({ page }) => {
  await seedBooking(1)
  const admin = await createAdmin('finance')
  await loginAdmin(page, admin)
  const n = Number(await page.getByTestId('dash-bookings-7d').textContent())
  expect(n).toBeGreaterThanOrEqual(1)
  const ctx = await adminApi(admin)
  const dash = await getJSON<{ bookings_upcoming_7d: number }>(ctx, '/admin/api/v1/dashboard')
  expect(dash.bookings_upcoming_7d).toBe(n)
})
```

- [ ] **Step 5: 覆盖表与配置注释**

`web/e2e/coverage.json`：`console` 数组追加 `"C-BKG-01"`～`"C-BKG-10"`、`"C-DET-17"`～`"C-DET-24"`、`"C-INS-05"`～`"C-INS-07"`；`admin` 数组追加 `"A-GRP-01"`～`"A-GRP-05"`、`"A-BKG-01"`～`"A-BKG-06"`、`"A-INS-13"`。

`web/playwright.config.ts` 的 `projects` 上方注释末尾追加一句：`计划 6 的预订用例（C-BKG / C-DET-17～24 / C-INS-05～07 / A-BKG）按服务器时间（hooks.clock）算开始时刻，每个用例最多推进约 1 天（C-DET-22 / 23 推进 23 小时；没有用例推进到 3 天以上），都只用自己新登记的机器与机组。`

- [ ] **Step 6: 运行**

Run: `cd web && npm run e2e:coverage && npx playwright test e2e/console/booking.spec.ts e2e/console/instance-detail.spec.ts e2e/console/instances.spec.ts e2e/admin/groups.spec.ts e2e/admin/bookings.spec.ts e2e/admin/instances.spec.ts 2>&1 | tail -15`
Expected: 覆盖表核对通过；三个 project 下相关 spec 全部通过。再跑完整 `make e2e` 确认原有用例仍全绿（`C-MKT-*` 走的 `market-rent-btn` → 确认框路径未变）。

- [ ] **Step 7: 提交**

```bash
git add web/e2e web/playwright.config.ts
git commit -m "test(e2e): booking dialog, instance booking states, groups and bookings admin pages, coverage table

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 22: 真机验收 B（192.168.1.221）——规格 §16.4 逐项 + 验收记录

**Files:**
- Modify: `docs/superpowers/acceptance-plan6.md`（补「验收 B」与总结论）
- 可能修改：验收中发现的缺陷在对应包修复并补单测（单独提交）

**Interfaces:**
- Consumes: 全部任务。前提同 Task 17（平台在 Mac、`SPARK_DOMAIN=<mac>.sslip.io`、Agent 为计划 3 版本）；控制台 / 后台用浏览器（Chrome `--host-resolver-rules=MAP *.<mac>.sslip.io <mac>`，因为 Mac 的 DNS 对 sslip.io 返回假地址）。需要 root 的命令按「执行须知」的模式由协调人执行，密码不进任何文件 / 记录 / 提交。**本任务逐项执行规格 §16.4 的 1～9 项并记录；A 阶段已用 API 验证过的项在界面上复做一遍（页面为准）。**

- [ ] **Step 1: 机组与展示名（§16.4 第 1 项）**

后台 `/admin/groups` 新建 `A 区`，「管理机器」把机器 1 加入并设展示名 `A-01`；控制台市场页「预订档期」对话框机组下拉显示 `A 区 · 共 1 · 现空闲 1`，指定机器列表显示 `A-01`。

- [ ] **Step 2: 到点开通（§16.4 第 2 项）**

对话框预订 `A-01`：开始 = 当前时间 + 2h 向上取整到整点、1d、自动续费关 → 确认框显示开始时刻与 ¥69.00 → 下单 → 详情「已预订」「可取消」；后台机器详情「档期」出现该时段。等到 S − 1h（±1 分钟，看 platform 日志）：详情自动刷新为「开通中」→「运行中」，SSH 可登录但剩余时长停在 24 小时；到 S 后倒计时开始递减。记录实际开通时刻。

- [ ] **Step 3: 即时订单被预订挡住（§16.4 第 3 项）**

另一账号市场页：`现在可租 0 台`，「立即租用」按钮为「暂无空闲」；取消预订后刷新 → `现在可租 1 台` → 立即租用成功 → 释放。

- [ ] **Step 4: 取消退款（§16.4 第 4 项）**

预订 S = +30h → 详情「取消预订」确认框「将退款 ¥69.00（100%）」→ 钱包流水 refund；预订 S = +3h → 立即取消框显示 100%（冷静期）→ 等 16 分钟 → 50% → 取消 → 退 ¥34.50，订单「部分退款」；后台审计有 `booking.cancel` 两条。

- [ ] **Step 5: 续费被挡 + 可续至（§16.4 第 5 项）**

第 2 步实例运行中；用对话框在 `A-01` 上预订 S3 = S + 3d；实例详情续费表单显示「可续至 <S3 − 8h>」，1w 灰显并提示「超出可续时间」，1d 可选；接口提交 1w → 409 `renew_blocked`（curl）；余额不变；1d 续费成功。随后取消 S3。

- [ ] **Step 6: 自动续费（§16.4 第 6 项）**

详情「自动续费」开关打开（1d）；`UPDATE instances SET expires_at = now() + interval '65 minutes'` → 2 分钟内订单页新增「自动」续费单、到期延后 1 天、无短信失败；余额调 0 + 再改 `expires_at` → 详情顶部红色提示「自动续费失败：余额不足…」+ 「去充值」，mock 短信日志一条；充值 → 下一轮成功、提示消失；后台实例详情「自动续费」显示「开启（1 天）」。

- [ ] **Step 7: 改派不可测（§16.4 第 7 项）**

后台机器详情「进入维护」确认框显示「该机器有 N 个未开通的预订，将在开通时自动改派」（先订一个预订再点）；点取消不进入维护。改派本身以 Task 16 / 21 为准，记录注明。

- [ ] **Step 8: 接入层（§16.4 第 8 项）**

reserved 实例 `ssh` Banner「实例预订于 … 开始，开始前 1 小时自动开通」；Web 端口 503 页同文案；开通后恢复；取消后「实例已释放」。

- [ ] **Step 9: 对账无噪声（§16.4 第 9 项）**

后台告警页：没有 `booking_*`；整个过程只允许 `orphan_container` / `container_down` 类已知噪声。

- [ ] **Step 10: 记录、结论、提交**

补齐 `acceptance-plan6.md`：每项命令 / 关键输出 / 结论，缺陷表（编号、现象、原因、修复提交），「观察（列入遗留）」。写入前核对记录与提交里没有任何密码或 `SUDO_PW` 的值。

```bash
git add docs/superpowers/acceptance-plan6.md
git commit -m "docs: plan 6 real-machine acceptance record (checkpoint B) on 192.168.1.221

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---
### Task 23: 收尾：遗留清单、计划 1 / 4 遗留勾选、README 复核、`make check` 全量

**Files:**
- Create: `docs/superpowers/followups-plan6.md`
- Modify: `docs/superpowers/followups-plan1.md`（两条 lease 遗留勾选）、`docs/superpowers/followups-plan4.md`（「修改已登记机器」部分认领）、`README.md`（如仍有遗漏）

**Interfaces:**
- Consumes: 全部任务（含 Task 17 / 22 验收记录里的缺陷修复提交与裁定）。
- Produces: 遗留清单与上线前核对项；`make check` 全绿是本计划的完成判据。

- [ ] **Step 1: 勾选前序遗留**

`followups-plan1.md`：「机器失联期间 `machine_leases` 上界不随时间推进」与「`occupiedUntil` 在状态不一致时回退为 `now`」两条前加 `✅ 计划 6 已处理：`（分别指向 `RefreshCappedLeases` 与 `machine_leases_period_nonempty` / `occupiedUntil ≥ now + 1s`）。`followups-plan4.md`：「后台没有修改已登记机器」一条改为「✅ 计划 6 部分处理：后台可改机组与展示名（`PUT /machines/{id}`）；机柜 / IP / PDU 仍只能导入或改库」。

- [ ] **Step 2: `followups-plan6.md`**

```markdown
# 计划 6 遗留事项

来源：计划 6 设计（2026-10-19）§17、评审附录与实施 / 验收过程。均不阻塞合并。

## 上线前需要核对
- 阿里云短信两个新模板（自动续费失败、预订被平台取消）申请通过并填入 `SPARK_SMS_TPL_AUTO_RENEW_FAILED` / `SPARK_SMS_TPL_BOOKING_CANCELLED`，否则 `SPARK_SMS_DRIVER=aliyun` 启动校验失败。
- 发布时一次性重启全部 platform 副本（旧副本分配不看占用时段）；Agent 不需要升级。
- 决定先把 200 台分组还是先全部作为「不限机组」上线；`visible=false` 机组的用途说明给运营；`display_name` 命名规范。
- 控制台市场页 `idle_count` 语义已改为「此刻可即时租 1 天」，运营看到的数字会比后台「空闲」少。

## 规格列出的遗留（本计划不做）
- 多月折扣价 / 机组差异化定价 / 优惠券。
- 预订改期（取消后重订）；候补 / 排队；预订开通成功短信；邮件 / 站内信。
- 后台代用户下单、后台替用户开关自动续费。
- 取消手续费与预订上限的后台可配置（常量）。
- 指定机器的「硬保证」；跨商品机组限制。
- 即时订单开通失败自动换机（仍是失败 + 退款）。
- 把 `rack / slot / lan_ip / PDU` 的修改开放到后台。
- 机器日历的 ICS 导出；控制台按机组的历史出租率。
- 机器离线时预订的提前预警（只有开通时刻才改派）；更智能的装箱（best-fit 单次贪心）。
- 自动续费的数量 > 1、升档。

## 可以继续延后
- 即时分支的 `no_capacity` 保持旧消息与错误值（前端引导依赖），只有预订分支带台数。
- 控制台实例列表的「批次折叠」实现为批次列 / 标签 + `?batch=` 筛选视图，不做视觉折叠。
- 后台机器详情的「档期」只有列表，没有规格 §12 的 60 天横向时间轴；机组「管理机器」抽屉用机器 ID 输入与机柜筛选代替勾选表。
- 候选机器改为 best-fit 排序后，`placeOrder` / `runningInstance` 拿到哪台空闲机器不再严格按 id：现有 e2e 里按 `ids[0]` 操作的用例在 Task 21 的全量 `make e2e` 中复核过。
- 后台取消预订也发短信（规格表只写了平台无法履约 / 失去资格两种；取消原因不进短信）。
- 自动续费失败标记写在该行事务里（savepoint 回滚扣款尝试），不是独立连接的事务；效果等价。
- 机器日历匿名可见会暴露单机占用情况（设计已接受）。
- 实例视图每台实例查两次（展示名 / 机组名有请求内缓存，`renewable_until` 每台一次）；列表上限 100 条可接受。
- `UserEvents` 读取全部事件再过滤前 20 条；事件量大的实例可改为 SQL 侧过滤。

## 实施中确定的产品行为（裁定）
- `fleet.Allocate` 删除，候选选择全部在 `rental.pickMachines`；`fleet.MarkAllocated` 只做状态切换。
- `POST /orders` 不建批次、键原样写在订单上；`POST /bookings` 建批次、子单键 `<key>#<n>`；用户键禁止含 `#`。
- 自动续费降档只在被预订挡住时发生，订单与事件记实际档位。
- 迟开通被下一预订封顶的差额按最新订单单价自动退款（remark「迟开通补偿」）。
（实施中新增的裁定按实际补充。）
```

- [ ] **Step 3: 全量验证**

Run:
```bash
export PATH=$HOME/.local/go/bin:$HOME/.local/bin:$PATH
go vet ./... && go test -race ./... && make check
```
Expected: `go test -race` 全部通过；`make check` = `test`（Go + `web-test` + `install_test.sh`）+ `e2e`（`make web` → `npm run e2e:coverage` → Playwright 三个 project 全部通过，原有用例 + 本计划 30 个新用例）。失败项逐个修到绿，不允许 skip。

- [ ] **Step 4: 提交**

```bash
git add docs/superpowers/followups-plan1.md docs/superpowers/followups-plan4.md docs/superpowers/followups-plan6.md README.md
git commit -m "docs: plan 6 followups, close plan 1 lease followups, final make check

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

## 自检记录（写计划时完成）

**规格覆盖**（§ → 任务）：§0 代码事实（`Allocate` 不看 lease、`refreshLease` 的 `ErrNoCapacity`、`quote` 按单个租期单位、`sweepEach` 模式、锁顺序、迁移编号）→ Task 1、2、4、6、14；§1.1 第 1 条（预订 = reserved lease + reserved 实例）→ Task 1、5；第 2 条 / B2（指定机器只承诺机组）→ Task 5（`machine_id` 校验）、8（改派）；第 3 条（开通提前 1h、早开通不计费、迟开通补偿）→ Task 8（`TestProvisionDueStartsOneHourBefore`、`TestProvisionLateStartIsCompensatedAndCapped`）；第 4 条 / B10（批次 = N 单 N 实例）→ Task 5；第 5 条 / B3（三档 × 数量、无折扣价）→ Task 1（`MaxQuantity`）、5、6；第 6 条 / B6（下单即扣）→ Task 5；第 7 条 / B4（冷静期 + 两档 + 平台全退）→ Task 7（`TestCancelQuoteTiers`）；第 8 条（续费冲突、可续至）→ Task 9；第 9 条 / B5（自动续费降档、重试、短信）→ Task 10；第 10 条（即时 lease-aware）→ Task 4（`TestAvailableNowIsLeaseAware`）、16（`TestImmediateOrderDoesNotTakeBookedMachine`）；第 11 条（封顶）→ Task 2（`TestRefreshLeaseCapsAtNextReservation`）；第 12 条（grace 取消）→ Task 8（`TestProvisionWaitsThenCancelsAfterGrace`）；第 13 条 / B11（短信）→ Task 7、10、14；B1（参数）→ Task 5（`TestBatchValidationMatrix`）；B7（后台角色 / TOTP）→ Task 13；B8 / B9（展示名、未分组、不可见机组）→ Task 3、4（`TestPickMachinesCandidateSet`）；B12（维护提示）→ Task 13、20；§2 迁移与同步清单 → Task 1、15；§3 机组 → Task 3、20；§4.1～4.4 → Task 2、4、11；§5.1～5.7 → Task 7、8、15；§6 → Task 5；§7.1～7.3 → Task 9、10；§8 交互表（换机复制、退款折算、仪表盘、CSV）→ Task 4、6、13；§9 清扫顺序 → Task 14（`TestSweepOrderProvisionsAndAutoRenewsBeforeExpiry`）；§10 错误码与接口 → Task 1、12、13；§11 控制台页面 → Task 18、19；§12 后台页面 → Task 20；§13 常量 / 配置 → Task 1、14；§14 并发与一致性 → Task 4（savepoint 重试）、5（40P01 / 40001 重试）、16（并发一致性）；§15 迁移兼容（Down 拒绝 reserved、`POST /orders` 不变、`idle_count`）→ Task 1、5、12、14（README）；§16.1 单元 → 各任务；§16.2 集成 → Task 16；§16.3 e2e（钩子、覆盖表 ID）→ Task 16、21；§16.4 真机 9 项 → Task 17（1～6、8、9 的 API 部分）、22（全部页面）；§17 遗留 / 上线前核对 → Task 23；§18 任务拆分 22 项 → 本计划 23 项（取消先于开通、真机检查点 A 独立成 Task 17）；附录评审 M1～M4、S1～S15 → M1 Task 5、M2 Task 8、M3 Task 9 / 22、M4 Task 15；S1 Task 7、S2 Task 8、S3 Task 1、S4 Task 8、S5 Task 1（头注释）/ 5（重试）、S6 Task 2、S7 Task 5、S8 Task 4、S9 Task 8、S10 Task 8、S11 Task 1 / 3、S12 Task 5、S13 Task 8（改派按可见机组）、S14 Task 8、S15 Task 10。

**占位符扫描**：全文无 TBD / TODO / 「类似 Task N」；每个代码步骤都有完整代码块；需要改动现有文件的步骤都给出了改动后的完整函数或精确的插入位置与内容；Task 17 第 7 项明确写「单机不可测，以 Task 16 / 21 为准」而不是留空；A-BKG-06 等用例无未使用导入。

**类型一致性**：`rental.Instance` 新字段（Task 1）↔ `scanInstance` / `insertInstance`（Task 5）/ `AdminMigrate` INSERT（Task 4）/ 视图（Task 12、13）；`rental.Order{Quantity, BatchID *string, Auto}`（Task 1）↔ `charge(…, quantity int, auto bool, batchID *string, idemKey, now)`（Task 5）↔ `refundable.quantity`（Task 6）↔ `orderView`（Task 13）；`leaseRow{ID, MachineID, Lower, Upper, Status}` / `lockLease` / `insertLease` / `nextReservedLower`（Task 2）↔ `provision` / `reassign` / `reassignTo`（Task 8、13）↔ `renewInfo`（Task 9）；`pickSpec{ProductID, GroupID, MachineID, Lower, Upper, Immediate, Exclude}` / `pickMachines` / `placeOnMachines` / `countAvailable` / `noCapacity` / `immediatePeriod` / `bookingPeriod`（Task 4）↔ `placeBatch`（Task 5）↔ `reassign`（Task 8）↔ `Availability`（Task 11）；`BatchRequest` / `Batch` / `BatchResult` / `PlaceBatch` / `GetBatch` / `RenewQuantity`（Task 5）↔ `api/booking.go`（Task 12）↔ `bookViaApi`（Task 21）；`CancelQuote` / `Cancel` / `AdminCancelBooking` / `cancelPlatform` / `cancelInstance`（Task 7）↔ `provision`（Task 8）↔ API（Task 12、13）；`RenewInfo{RenewableUntil, Options []RenewOption}` / `RenewInfoFor` / `renewFits`（Task 9）↔ `autoRenewDuration`（Task 10）↔ `instanceView.renew_options`（Task 12）↔ `Instance.renew_options`（Task 18）；`SetAutoRenew(ctx, userID, id, enabled, d, ip)` / `AutoRenewDue`（Task 10）↔ `handleAutoRenew`（Task 12）；`sweepEachAfter(ctx, pred, arg, orderBy, fn func(pgx.Tx, Instance) (func(), error), onErr)`（Task 8）↔ `ProvisionDue` / `FailStuckCreating` / `AutoRenewDue`（Task 8、10）；`Notifier{NotifyExpiry, NotifyAutoRenewFailed, NotifyBookingCancelled}`（Task 7）↔ `expiryNotifier`（Task 7）↔ `fakeNotifier` / `rentalNoop` / `noopNotifier`（Task 7）；`fleet.Group` / `GroupInput` / `GroupRow` / `MachineUpdate` / `MarkAllocated` / `Machine.GroupID / DisplayName / Label()`（Task 3）↔ `adminapi/groups.go`（Task 3）↔ `calendar.go`（Task 11）↔ e2e 钩子（Task 16）；`UserEvent` / `UserEvents`（Task 12）↔ `detail-event`（Task 19）；`BookingFilter` / `AdminListBookings` / `UpcomingBookings` / `UpcomingBookingsForMachine` / `AdminReassign` / `MachineLeases` / `OrderFilter.BatchID`（Task 13）↔ 后台页面（Task 20）；`api.Server.CalendarLimit`（Task 12）↔ `app.Build`（Task 12）；`tz.CST / FloorHour / IsHour / DayStart / Format`（Task 2）↔ Task 5、8、9、11、12、13、15；`IngressRoute.ScheduledStartAt`（Task 15）↔ `NotRunningPage`（Task 15）；`sms.TemplateAutoRenewFailed / TemplateBookingCancelled`（Task 7）↔ `config.AliyunConfig.SMSTplAutoRenewFailed / SMSTplBookingCancelled`（Task 14）；前端 `Instance` / `Order` / `booking.ts` 类型（Task 18）↔ `RentDialog` / `logic/booking.ts` / `logic/instance.ts`（Task 18、19）↔ `AdminInstance` / `MachineDetail` / `AdminGroup` / `Dashboard`（Task 20）↔ e2e `data-testid`（Task 21）。

**Review Focus 落点**：1 → Task 16 `TestConcurrentBatchesConsistent`（50 并发 / 10 机、一致性三条 SQL、失败码白名单、60 秒超时）；2 → Task 4 `TestPlaceOnMachinesRetriesOnExclusionViolation`（第一台撞约束换第二台；换窗口后两台都撞 → `no_capacity` 且事务回滚无残留）；3 → Task 5 `TestBatchValidationMatrix`（整 2h 接受 / 1h 拒绝 / 第 60 天接受 / +1h 拒绝 / 非整点 / 1m×3 与 ×4 / 1d×90 与 ×91 / count 9、0 / 指定机器 count 2 / 键含 # / CST 整点 / 5 分钟容差两端）；4 → Task 9 `TestRenewBlockedByReservationDoesNotCharge`（手动 1w、后台延长 7d、停机续 2d 都被挡且不扣款、不改 lease；1d 通过）+ `TestRenewRaceWithNewReservationIsBlocked`（并发预订在 `renewFits` 后提交 → `renew_blocked`、扣款回滚）+ Task 10 `TestAutoRenewFallsBackThenBlocks`（1m→1w→1d 降档、全挡时 `renew_blocked` + 短信 + 10 分钟重试、手动续费同样被挡）；5 → Task 5 `TestLegacyOrderIdempotencyUnchanged`（同键回放同订单 / 实例、不建批次、键原样存储、含 # 被 400）+ `TestBatchIdempotencyReplayAndConflict`（批次键回放、子单键 `#n`、旧接口 / 续费用批次键冲突）。
