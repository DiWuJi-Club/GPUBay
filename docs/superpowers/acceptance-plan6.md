# 计划 6 真机验收记录（192.168.1.221）

- 机器：DGX Spark `spark-3479`（GB10），Ubuntu 24.04，Docker 29.6.2，与计划 2/3 验收同一台；Agent 版本 `1f5d935`（计划 6 无协议改动，沿用）。
- 平台：开发 Mac 192.168.1.218，`SPARK_ENV=dev`，`SPARK_SWEEP_INTERVAL=1m`，平台二进制构建自 `9d853ef`（后端 Task 1～15 全部合入），开发数据库已迁移到 00019。
- 规格：`docs/superpowers/specs/2026-10-19-plan6-booking-design.md` §16.4。
- 说明：
  - 只有一台真机。为测「取消退款」，在同一机组另登记了一台不接 Agent 的占位机器 `A-02占位`（id 2，离线、空闲，只用于预订）。预订阶段不需要真机，开通前即取消。
  - 改派与一次多台无法在单机上演示，以 Task 16 的 Go 集成测试和 Task 21 的 e2e 为准，详见文末。

## 检查点 A：后端 + API（§16.4 第 1～4、5、6、8、9 项）

- 日期：2026-10-19
- 结论：全部通过，未发现新缺陷。

### 1. 机组与展示名（§16.4.1）——通过

- 后台：`POST /groups {"name":"A 区","region":"一楼","visible":true}` 得到 id 1；`POST /groups/1/machines {"machine_ids":[1]}` 返回 200；`PUT /machines/1 {"display_name":"A-01"}` 返回 200。
- 控制台：
  - `GET /api/v1/groups` 列出 `A 区`，`machine_count 1`、`idle_now 1`，并带服务端时间 `now`。
  - `GET /groups/1/machines` 返回 `label A-01`、`busy_until null`。
  - `GET /groups/1/calendar?days=3` 三天均为 `free 1 / total 1`。

### 2. 到点开通（§16.4.2）——通过

18:49 下单预订，参数：`S = 2026-10-19 21:00`（北京时间），`machine_id 1`，`1d × 1`。

- **下单后**：
  - 返回 201。实例 `ins-5mz7co2j` 状态 `reserved`，`cancellable_until 20:00`，`machine_label A-01`。
  - lease 为 `reserved [10-19 20:00, 10-21 04:00)`，即 `[S−1h, S+1d+7h)`。
  - 后台机器详情的 `leases` 里有这一条，`upcoming_bookings 1`。
- **开通**：
  - 20:00:26 进入 `creating`，`provisioned_at = 20:00:26`，比 `S−1h` 晚 26 秒（清扫间隔 1 分钟）。
  - 20:00:28 Agent 回报 `created`，实例变为 `running`；机器转为 `allocated`，Spark 上 `docker ps` 出现 `ins-5mz7co2j`。
- **计时**：
  - `expires_at = 2026-10-20 21:00`，正好是 `S + 1d`。
  - 运行中但未到 `S` 时，`remaining_seconds = 86400`，不递减。
  - SSH 可以登录，`nvidia-smi` 显示 NVIDIA GB10。

### 3. 即时订单被预订挡住（§16.4.3）——通过（解除部分见注）

- 预订存在且尚未开通时，即时下单 `POST /orders {"product_id":1,"duration":"1d","image_id":4}` 返回 409 `no_capacity`。`GET /products` 的 `idle_count` 为 0。
- 用的是同一测试账号（第二个账号未实名，`kyc_required`），被测行为与账号无关。
- 注：取消预订后即时单恢复可下这一半，在单机上与第 2 项冲突（要取消正在验收的预订），没有在真机上做。由 Go 集成测试 `TestImmediateOrderDoesNotTakeBookedMachine` 覆盖（Task 16）。

### 4. 取消退款（§16.4.4）——通过

18:51 在占位机器上下两单：预订 A `S = 10-21 06:00`（约 35 小时后），预订 B `S = 10-19 22:00`（3 小时后）。

| 时刻 | A 的报价 | B 的报价 |
|---|---|---|
| 下单后立即（冷静期内） | 100%，6900 | 100%，6900 |
| 19:07:29（过冷静期 16 分钟） | 100%，6900（距开始 ≥ 24h） | 50%，3450（距开始 < 24h） |

- 取消后两单均为 `cancelled`。
- 订单：A 为 `refunded`，退 6900；B 为 `partially_refunded`，退 3450。
- 余额对账：10300 + 6900 + 3450 = 20650，与实际一致。
- 后台审计 `booking.cancel` 两条。

### 5. 续费被挡与可续至（§16.4.5）——通过

前提：第 2 项的实例 running，`expires = 10-20 21:00`。在 A-01 上再预订 `S3 = 10-22 21:00`，得到 `ins-4jur7gll`。

- 实例视图显示：
  - `renewable_until = 10-22 13:00`，即 `S3 − 8h`。
  - `renew_options = [1d: 1, 1w: 0, 1m: 0]`。
- `POST /renew {"duration":"1w"}` 返回 409 `renew_blocked`，消息「该机器自 2026-10-22 21:00 起已有预订，本次最多可续至 2026-10-22 13:00」，余额不变。
- `{"duration":"1d"}` 返回 200，`expires` 变为 10-21 21:00。
- 取消 `S3` 预订：在冷静期内，全额退款。

### 6. 自动续费（§16.4.6）——通过

- **成功续费**：
  - 开启 `auto-renew 1d`，把 `expires_at` 改为 `now + 65min`。
  - 20:06:26，正好是 `expires − 1h`，生成订单 `renew auto=true`，`expires` 延后 1 天，失败标记为空。
- **余额不足**：
  - 余额直接在开发库 `wallet_accounts` 中改为 0，`expires_at` 改为 `now + 61min`。
  - 20:08:26 出现 `auto_renew_failed_reason = insufficient_balance` 和事件「auto renew failed: insufficient_balance」。
  - 之后每分钟重试，`auto_renew_failed_at` 随之更新，但失败事件始终只有 1 条（首次失败才写事件、发短信）。
  - 中途把余额恢复到 6850，仍低于 6900，继续失败，符合预期。
- **恢复**：余额改为 20000 后，下一轮 20:15 续费成功，`expires` 延后 1 天，失败字段清空。整个过程共 2 张自动续费订单、1 条失败事件。
- 观察：开发环境的 mock 短信只记在内存里、不写日志，所以「只发一条短信」由「首次失败才写事件」这一同源逻辑来核对，单测 `TestAutoRenew*` 另外断言了短信次数。

### 7. 接入层（§16.4.8）——通过

- 实例处于 `reserved` 时：
  - SSH Banner 为「实例预订于 2026-10-19 21:00 开始，开始前 1 小时自动开通」，并拒绝登录。
  - 加公开端口 8888 后访问入口，返回 503「暂时无法访问」，文案相同。
- 开通并 running 后，SSH 可以登录。
- 已取消的实例 Banner 为「实例已释放」。

### 8. 告警无噪声（§16.4.9）——通过

`GET /alerts?resolved=false` 没有任何 `booking_*` 告警。现有的 `machine_offline` 来自占位机器和此前的计划，`upstream_unreachable` 来自计划 3 验收，都与本计划无关。

### 单机无法覆盖的部分

- **改派**：开通时机器不可用、Create 失败、超时、30 分钟宽限后平台取消，以及告警。由 `internal/rental/provision_test.go`、`internal/app/booking_e2e_test.go`（`TestBookingReassignChainHTTP`）和 Task 21 e2e 覆盖。
- **一次多台与并发**：
  - 由 `TestConcurrentBatchesConsistent` 覆盖：10 台机器、51 个批次，逐时段断言放置台数精确等于 min(容量, 需求)。
  - 由 `TestPlaceRetriesWhenFreeMachineOnlyBrieflyLocked` 覆盖：被其他事务短暂锁住的空闲机器，在重试后仍能分配。
