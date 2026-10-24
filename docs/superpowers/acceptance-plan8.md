# 计划 8 开发平台验收记录（邀请返利，规格 §16.4 全部 12 项）

- 日期：2026-10-20 04:28～05:18（北京时间）。
- 分支 / 提交：`feat/plan8-referral` @ `8f84f7c`（Task 1～17 全部合入）。平台二进制与前端产物均构建自该提交（`cd web && npm run build` + `go build ./cmd/platform`）。
- 规格：`docs/superpowers/specs/2026-10-20-plan8-referral-design.md` §16.4；裁定 §1.3 R1～R14；过程裁定见 `.superpowers/sdd/2026-10-20-plan8-referral/progress.md`。
- 结论：**12 项全部通过**，未发现阻断或中等以上缺陷；1 个低级别缺陷（规格内部不一致）、若干观察，见文末。

## 环境（与共享开发库 / 开发平台完全隔离）

按 Task 17 的裁定：本分支的迁移是 `00021`，而 main 上计划 7 的 `00020` 尚未合入，**不能**让本分支的平台连共享开发库 `:5433`（goose 乱序）。因此验收用一套独立环境，全程没有碰开发库与正在运行的开发平台（8080 / 8081 / 2222 / 9443 等进程未动）。

| 项 | 值 |
|---|---|
| PostgreSQL | 嵌入式 PG 18.3，**端口 5434**，全新数据目录（scratch 目录），启动程序是 `cmd/devdb/main.go` 的临时副本（改端口与数据目录；验收后已删除、未提交） |
| 迁移 | 启动时自动跑到 `00021`（`goose_db_version` 最高 21；本分支没有 00020，属预期） |
| 平台 | `SPARK_ENV=dev`，`SPARK_HTTP_ADDR=127.0.0.1:8280`，`SPARK_ADMIN_ADDR=127.0.0.1:8281`，`SPARK_AGENT_ADDR=127.0.0.1:9643`，`SPARK_SSH_ADDR=127.0.0.1:2422`，`SPARK_SWEEP_INTERVAL=1m`，`SPARK_PUBLIC_BASE_URL=http://localhost:8280`，`SPARK_DOMAIN=localhost`（配置要求 PublicBaseURL 的域名等于 SPARK_DOMAIN），短信 / 实名 / 支付 / PDU 全部 mock，`SPARK_SECRET_KEY` 为本次随机生成 |
| 租户 Web 入口 | 没有独立监听端口（按 Host 走 8280），任务说明里的「ingress 8643」不适用 |
| 管理员 | `platform admin create` 建 `root`（super）与 `fin`（finance），各自绑定 TOTP；凭据只存在 scratch 目录，本文不含密码与密钥 |
| 机器 | 本计划不涉及 Agent，用占位机器：后台 `POST /machines` 登记 `P8ACC-1～5`（`lan_ip 127.0.0.1`），`POST /machines/{id}/enroll-command` 取接入令牌后，用一个临时 Go 程序（在仓库模块内的临时 `cmd/` 目录，验收后删除）以 `internal/agent/fakeagent` 的**自动模式**接入 9643 网关；实例下单后几秒进入 `running`，「退款并释放」后洗机结束回到空闲。与 Task 14 / 17 的 e2e 做法相同，只是跑在真实的 `cmd/platform` 进程外 |
| 浏览器 | Playwright 1.64.1 + 本机 Chrome（headless），`127.0.0.1 / localhost` 直连；A 与 B 各用一个持久化 profile（`localStorage.sc_device` 不变，供第 8 项「同设备」），其余账号用全新 context（相当于隐私窗口）；截图在 scratch 目录 |
| 账号 | A `138****0001`（邀请人）、B `138****0002`、C `138****0003`（老用户）、D `138****0004`（同设备）、E `138****0005`（同身份）、F `138****0006` / G `138****0007`（冻结期注册）；实名用 mock（任意 18 位号码通过） |

### 手工 SQL（全部在验收库 5434 上，只用于推进时间）

| 时刻 | 语句 | 目的 |
|---|---|---|
| 04:39:50 | `UPDATE referral_rewards SET settle_after = now() WHERE status='pending'`（命中 `ord-wvkhv91c7n7o`） | 第 5 项：让 7 天持有期立即到期 |
| 04:48:34 | 同上（命中 `ord-h5nkwodctrnw`） | 第 7 项第一轮发放 |
| 04:55 | 同上（命中 `ord-ltzqutyjbdnb`） | 第 7 项欠账抵销 |
| 05:01 | 同上（命中 `ord-23rvlsf6u0m7`） | 第 8 项：搁置的绑定到期不发放 |
| 05:09 | 同上（命中 `ord-1y6t3ihr8qx4`） | 第 9 项：冻结期到期不发放 |

除此之外没有改过任何数据；其余全部通过控制台 / 后台页面或公开 API 完成。

## 逐项结果

### 1. 分享——通过

A 在 Chrome 登录（验证码 `000000`）→「我的」→「邀请返利」：

- 邀请码 `DJZP1DQ5`（8 位，Crockford 字母表），链接 `http://localhost:8280/?ic=DJZP1DQ5`，二维码 `<img data-testid=invite-qr>` 为 data:image/png。
- 未实名时顶部提示「实名认证后才能收到返利 → 去实名」（R11）。
- 点「复制链接」→ toast「已复制」，剪贴板内容与链接一致。
- 规则摘要：10%、12 个月、扣款 7 天后到账、单人 ¥500.00、30 天 ¥5,000.00（迁移写入的首版，R1～R3、R5）。

### 2. 注册绑定——通过

全新 context 打开链接：

- 落地后 `localStorage.sc_invite={"code":"DJZP1DQ5","at":…}`，地址栏参数被去掉（最终 URL `/market`；见观察 2）。
- `/login` 显示「通过 138****0001 的邀请注册 ×」。
- 新号 B 注册 → toast「已绑定邀请关系」，`sc_invite` 清空。
- A 的邀请页「被邀请人」：`138****0002 · 有效 · 未实名 · 累计贡献 ¥0.00 · 返利截止 2027-10-20`，统计「已邀请 1」。
- 库：`referral_bindings` 1 行，`status=active`，`risk_flags={same_ip}`（同机验收必然同 IP，R8 只打标记不搁置），`rebate_until = bound_at + 365d`；审计 `referral.bind`（actor=user）。

### 3. 老用户不绑定——通过

先用 API 把 C 注册成老用户，再在新 context 用链接登录 C：登录页同样显示邀请人提示，提交体带 `invite_code`，登录后**无 toast**，A 的「已邀请」仍为 1，`SELECT count(*) FROM referral_bindings WHERE invitee_id=<C>` = 0。

### 4. 计提——通过

B：实名（mock 通过）→ 充值页自定义 ¥200 → 「模拟支付成功」→ 余额 ¥200.00 → 市场页「立即租用」1 天（确认框 ¥69.00，余额 ¥200.00 → ¥131.00）→ 201，实例 `ins-l3w2p9od` 几秒后 `running`。

- 下一轮清扫（04:37:05，≤ 1 分钟）后 A 的「返利明细」：`138****0002 · ord-wvkhv91c7n7o ¥6.90 消费 ¥69.00 · 待发放`，统计「待发放 ¥6.90」。
- `referral_rewards`：`rate_bp 1000, order_amount 6900, reward_cap 690, reward 690, status pending, paid_at 04:36:41, settle_after 04:36:41 + 7 天`（`settle_after − paid_at` 恰为 7 天）。
- 事件 `accrue 690 raw=690 room_invitee=50000 room_inviter=500000 rate_bp=1000 version=1`。
- 后台（finance）`/admin/referral` 概览：待发放 ¥6.90、净额 ¥0.00、本月 ¥0.00、欠账 ¥0.00、待审核 0。

### 5. 发放——通过

A 先实名（R6：邀请人未实名不发放）；执行上表第一条 `UPDATE` 后下一轮清扫（04:40:05）：

- A 钱包余额 ¥6.90，流水一行「邀请返利 +¥6.90 变动后 ¥6.90 备注：邀请返利 138****0002 订单 ord-wvkhv91c7n7o」（R13，脱敏手机号 + 订单号）。
- A 邀请页：待发放 ¥0.00、累计净返利 ¥6.90、本月返利 ¥6.90，明细「已发放」。
- 后台「流水」筛「邀请返利」只剩这一行；`GET /admin/api/v1/ledger?kind=referral_rebate` 返回 1 条（`ref_type=referral, ref_id=ord-…`）。
- 库：`status=settled, settled_amount 690, settled_at 04:40:05`；事件 `settle 690 offset=0`。

### 6. 扣回——通过

finance 在后台实例详情 `ins-l3w2p9od`「退款并释放」：对话框提示「建议 ¥68.73（按剩余时长折算），最多可退 ¥69.00」，填 ¥30 + 原因，提交后弹动态码 → toast「已退款 ¥30.00，实例进入洗机」，订单「已退款 ¥30.00」。下一轮清扫（04:44:05）：

- A 钱包 ¥3.90，流水「返利扣回 −¥3.00 变动后 ¥3.90 备注：返利扣回 138****0002 订单 ord-…」。
- A 明细：`¥3.90 消费 ¥69.00（已退 ¥30.00） · 已发放（已扣回 ¥3.00）`。
- 库：`refunded_amount 3000, reward 390, settled_amount 690, clawed_amount 300`；事件 `refund 300 refund=3000 target=390`、`clawback 300`。

### 7. 欠账——通过

1. B 再租 1 天（`ord-h5nkwodctrnw`，实例 `ins-qflovvzf`）→ 计提 ¥6.90 → `UPDATE` → 发放，A 余额 ¥3.90 + ¥6.90 = ¥10.80。
2. A 充值 ¥58.20 到正好 ¥69.00 → 租 1 天 → 余额 ¥0.00。
3. finance 对 `ins-qflovvzf` 全退 ¥69.00 → 清扫（04:52:05）：A 余额不足，**没有**写钱包流水；`referral_accounts.debt = 690`；事件 `refund 690 target=0`、`debt_add 690 "balance short on clawback"`、`clawback 690`。A 邀请页出现「当前欠账 ¥6.90，将从之后的返利中抵扣」；后台「邀请人」列表欠账列 ¥6.90，详情抽屉「欠账 ¥6.90」，事件流显示「记欠账」。
4. B 再租 1 天（`ord-ltzqutyjbdnb`）→ 计提 → `UPDATE` → 发放（04:56:05）：事件 `debt_offset 690`、`settle 690 offset=690`；`referral_rewards.settled_amount 690`；**A 钱包没有新流水，余额仍 ¥0.00**；`debt = 0`；后台详情欠账 ¥0.00；A 页面「累计净返利 ¥10.80」（含抵欠账部分，口径与页面注释一致）。

### 8. 自刷——通过

**同设备（搁置 → 通过）**：A 在自己的 Chrome profile 退出（`sc_device` 不变）→ 用 A 的链接注册 D → toast「已绑定邀请关系」。

- 后台「待审核」出现 D，标记「同IP 同设备」；概览「待审核 1」；库 `status=held, hold_reason=same_device, device_id` 与 A 相同；审计 `referral.bind` 的 diff `status:held`。
- D 实名、充值 ¥69、租 1 天 → 计提 ¥6.90 → `UPDATE` → 清扫：**仍 pending**，A 余额不变。
- finance「通过」（备注「验收：人工核实后通过」；没有 kyc 未判定告警）→ toast「已通过」、待审核列表清空；库 `status=active, reviewed_by=2, review_note` 写入；审计 `referral.approve {hold_reason:same_device, kyc_check_unresolved:false}`。
- 下一轮清扫（05:04:05）发放：A +¥6.90「邀请返利 138****0004 订单 ord-23rvlsf6u0m7」。

**同身份（自动撤销）**：新 context 用 A 的链接注册 E（新设备）→ 已绑定；E 用 **B 的身份证号**实名 → mock 通过的同一事务内：

- 库 `status=revoked, hold_reason=same_identity, risk_flags={same_ip,same_identity}, reviewed_at` 写入；审计 `referral.auto_revoke`（actor=system，diff `{flag:same_identity, reason:same_identity, voided:0}`）。
- 后台邀请人详情绑定列表：`138****0005 已失效 同IP 同身份`；A 的被邀请人列表显示「已失效 · 已实名」。
- E 没有消费，所以没有待发放可作废（`voided:0`）；「待发放作废」这一半由单测 / 集成测试覆盖（Task 14 `same-identity pending void`）。

### 9. 冻结——通过

- finance 在邀请人详情「冻结」（原因必填）→ toast「已冻结」，状态「已冻结（验收：冻结测试）」；审计 `referral.freeze`。
- 新 context 用链接注册 F：`GET /referral/codes/DJZP1DQ5` 返回 `{"valid":false,"inviter":""}` → 登录页**不显示**邀请提示、`sc_invite` 被清；注册成功、无 toast、无绑定。
- 模拟「页面已缓存邀请码」：直接 `POST /auth/login` 带 `invite_code` 注册 G → 200，`invite:{bound:false, reason:"inviter_frozen"}`，注册照常成功、无绑定（R7）。
- A 的邀请页显示「邀请功能已暂停，如有疑问请联系客服」，码与链接隐藏（§8.2）。
- B 再租 1 天（`ord-1y6t3ihr8qx4`）→ 冻结期间照常计提 ¥6.90（R9「继续累计」）→ `UPDATE` → 清扫：**仍 pending**。
- finance「解冻」→ toast「已解冻」，状态「正常」；审计 `referral.unfreeze {previous_reason:…}`；下一轮清扫（05:12:05）发放，A +¥6.90，余额 ¥13.80。

### 10. 配置——通过

super 的「配置」Tab（finance 看不到该 Tab）：当前生效 10% / ¥500.00 / ¥5,000.00 / 7 天 / 365 天 / 2000-01-01。

- 填 15%、生效时间 **昨天**（`2026-10-19 05:12:00`）→ `POST /referral/config` 直接 400 `referral_config_invalid`「生效时间不能早于当前时间」（校验先于动态码），页面以通知条原文提示，版本数不变。
- 清空生效时间、15% → 403 `totp_required` → 动态码弹框 → 200：当前生效 15%，`effective_from` = 提交时刻；历史版本表两行（2 当前 15% / 1 10%）；审计 `referral.config`。
- B 再租 1 天（`ord-n7nnl2y5taax`）→ 计提 **¥10.35**（`rate_bp 1500, rate_version_id 2`）；此前 5 条明细 `rate_bp 1000 / version 1` 与金额全部不变；A 页面「待发放 ¥10.35」。

### 11. 导出——通过

finance 在「返利明细」「邀请人」Tab 点导出，浏览器下载 `referral-rewards-20261020-050544.csv`、`referral-bindings-20261020-050544.csv`：

- 文件头 `EF BB BF`（UTF-8 BOM），`file` 识别为 CSV text；中文表头完整（`订单号,邀请人ID,被邀请人ID,消费(元),退款(元),费率(%),应得(元),状态,发放时间,已扣回(元),费率版本` / `绑定ID,邀请人ID,被邀请人ID,邀请码,状态,标记,绑定时间,返利截止,IP,设备`）。
- 金额两位小数（`69.00,30.00,10.00,3.90,…,3.00`），时间 `2026-10-20 04:40:05`。
- 没有在本机用 Excel / Numbers 实际打开；BOM + UTF-8 是 Excel 正确识别中文的条件，已核对（见观察 4）。

### 12. 不变量——通过（方式见说明）

仓库里**没有** `TestInvariantsAgainstDevDB`（Task 18 简报 Step 12 要求加在 `internal/referral/invariants_test.go`，本次验收只提交记录、不改代码，列入遗留）。用同样的调用 `referral.NewService(referral.Deps{Pool, Clock: clock.Real{}}).CheckInvariants(ctx, pool)`（临时工具，只读）在清扫刚结束的静止态（05:18:18，水位 05:16:05，一条 pending 明细 `ord-n7nnl2y5taax`）对验收库执行：**`INVARIANTS OK`**（inv1 钱包净额 = Σ(settled − clawed) + debt、inv2 行内一致、inv3 退款额 = Σ 已处理退款流水、inv4 无未处理流水与事件种类一致）。

对账：A 的钱包流水 `+690 −300 +690 +690 +690 = 2460`，加欠账抵销不入账的 690，与 6 条明细 Σ(settled_amount − clawed_amount) = `390 + 0 + 690 + 690 + 690 + 0(pending)` = 2460 一致；余额 1380 = 2460 − 1080（充值 5820 − 租用 6900）。

## 缺陷

| 编号 | 现象 | 原因 / 分析 | 严重度 | 处理 |
|---|---|---|---|---|
| P8-A1 | 控制台「返利明细」待发放的行不显示预计到账时间，§16.4 第 4 项写「到账时间 = 7 天后」 | §10.1 的 `GET /referral/rewards` 响应没有 `settle_after`，§8.2 的明细列也没有「到账时间」，是规格内部不一致；后端 `settle_after` 本身正确（= 扣款 + 7 天） | 低 | 未改代码。建议 Task 19 决定：在 `RewardView` 增 `settle_after` 并在待发放行显示「预计 MM-DD 到账」，或改 §16.4 措辞 |

没有发现其它缺陷；没有修改产品代码。

## 观察（列入遗留，不影响通过）

1. **缺 `TestInvariantsAgainstDevDB`**：简报要求的用例未实现；本次用等价的只读调用验证。建议 Task 19 补上（`SPARK_DATABASE_URL` 为空时 `t.Skip`）。
2. **落地页参数**：`/?ic=…` 落地后先 `replace` 到 `/market?ic=…`，再去掉参数到 `/market`（约 1 秒内完成，多次访问均如此）；功能正确，只是比 §8.1「`router.replace` 去掉参数」多一跳。
3. **搁置绑定的提示**：同设备注册 D 时 toast 仍是「已绑定邀请关系」（与 §8.1 `bound=true` 的定义一致），用户侧看不出「审核中」，只有在邀请人页面才显示状态；是否要提示由运营决定。
4. **CSV 的状态 / 标记列是原始代码**（`settled`、`active`、`same_ip same_device`），不是页面上的中文标签；财务在 Excel 里筛选可用，但与页面不一致。
5. **统计口径**：被撤销的 E 仍计入 A 页面的「已邀请 3 / 已实名 3」和后台「被邀请人 3」；「本月返利（已到账）¥27.60」是到账毛额（含后被扣回的 ¥3.00 与 ¥6.90），「累计净返利 ¥24.60」含抵欠账的 ¥6.90——与页面注释和 Task 12 的裁定一致，但两个数都不等于钱包实际净入账（¥13.80 + 消费），财务核对时需按注释理解。
6. **退款的返利调整在清扫时处理**：后台退款后，明细「已扣回」与钱包「返利扣回」要等下一轮清扫（≤ 1 分钟）才出现，与 §5.1 设计一致；后台退款成功的 toast 没有提示这一点。
7. **搁置审核「通过」不要求动态码**，「撤销」要（规格 §10.2 如此）；冻结 / 解冻也不要动态码。符合规格，记录供上线前核对。
8. 同机验收所有账号同 IP，所以每个绑定都带 `same_ip` 标记（R8 只标记不搁置）；真实环境家庭 / 公司 NAT 下也会大量出现，后台列表默认不要按此标记筛「可疑」。

## 验收后清理

- 停掉本次启动的平台进程、5 个 fake Agent 进程与 5434 的 PostgreSQL；共享开发库与开发平台全程未被连接或重启。
- 删除临时 `cmd/devdb-p8acc/` 与 `cmd/p8acc-tool/`（均未提交）；scratch 目录保留截图、CSV 与日志。
