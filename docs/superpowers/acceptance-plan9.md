# 计划 9 真机验收记录（192.168.1.221）

## 检查点 A：后端 + API（§16.4 第 1～9 项，计划 Task 16）

- 日期：2026-10-25 03:12～04:07（北京时间）。持有 `spark.lock` 的时段为 03:12:18～04:07:34。
- 机器：DGX Spark `spark-3479`（GB10，128 GB 统一内存），后台显示为「B 区」B-01（machine 1）。与计划 2/3/6/7 验收是同一台。
- Agent：沿用机器上已装的版本，没有重装。`/usr/local/bin/spark-agent` 的 SHA-256 为 `13bc98a7dcda8be7f5511f77edf56585c32430991b9a0127d50ceb78c8ff1ef0`，与 `.devdata/dl/spark-agent-linux-arm64` 一致。平台的 `SPARK_AGENT_BINARY_SHA256` 设为这个值，Mac 上用 `python3 -m http.server 8000` 提供下载目录，所以不会触发升级。后台 `agent_version = 9f0e8f0`。
- 平台：分支 `p9/accept`，基于 `feat/plan9-token-mall`（计划 7、8 已合入，计划 9 全部任务已完成）。`make web && go build ./cmd/platform` 构建。
  - 03:23 用 HEAD `2699092` 启动过一次。
  - 03:28 换成含缺陷修复 A-1、A-2 的 `0c7e3ad` 重启，之后到验收结束没有再重启。
  - 运行在开发 Mac 192.168.1.218 上。
  - 环境变量与计划 7 验收相同：`SPARK_ENV=dev`、`SPARK_DOMAIN=192-168-1-218.sslip.io`、HTTP `:8080`、后台 `127.0.0.1:8081`、SSH `:2222`、Agent 网关 `:9443`、`SPARK_AGENT_DATA_SIZE_GIB=200`、`SPARK_APP_PIP_INDEX_URL=https://mirrors.aliyun.com/pypi/simple`。
  - 另加 `SPARK_SWEEP_INTERVAL=1m`、`SPARK_AGENT_BANDWIDTH_MBPS=1000`、`SPARK_LLM_GATEWAY=on`、`SPARK_LLM_CONTROLLER=on`。
- 数据库：在开发 PostgreSQL（:5433）上用 `CREATE DATABASE spark_p9 TEMPLATE spark` 克隆开发库，机器接入、CA、管理员和测试用户都随之带过来。
  - `spark` 原来的迁移版本是 20。平台启动时 goose 顺序执行了 `00021`（计划 8）和 `00022`（计划 9），没有乱序，也不需要手工补迁移。
  - `SPARK_DATABASE_URL` 指向 `spark_p9`，并按 README 加了 `pool_max_conns=20`。这一项触发了缺陷 A-1。
  - 开发库 `spark` 本身、计划 10 / 11 的克隆库（`spark_p10` / `spark_p11`）都没有改动。
- 镜像：`spark/llm-serving:25.09` 在机器本地，计划 7 已登记为 `LLM Serving`（image 5）。
- 账号：
  - 后台：`acceptor`（super）。登录和每次动态码都通过后台 API 完成，脚本按 `accept-admin.json` 里的 TOTP 密钥现算动态码。凭据不写入本文。
  - 控制台用户 A：13800000221（已实名）。验收前余额 ¥448.00。
  - 控制台用户 B：13800000222。第 9 项需要在用户 A 欠费期间继续发请求，所以给 B 做了 mock 实名，后台调账 ¥5.00。
- 访问方式：
  - 开发 Mac 的 DNS 会把 `*.sslip.io` 解析到假地址，curl 一律加 `--resolve 192-168-1-218.sslip.io:8080:192.168.1.218`。
  - OpenAI Python SDK（openai 2.48.0，装在 scratch 目录的 venv 里）在脚本里改写了 `socket.getaddrinfo`，效果与 `--resolve` 相同。
- 需要 root 的命令只有 `systemctl stop/start spark-agent`，在 Spark 上用 sudo 执行。
- 模型只在 Spark 上的系统实例容器里下载（modelscope）。Mac 上没有下载任何模型。
- 结论：**9 项全部通过**。
  - 第 7 项按调整后的步骤完成，原因见缺陷 A-3。
  - 发现并修复 2 个中等缺陷：A-1、A-2，各自单独提交。
  - 另有 1 个低严重度缺陷 A-3，是规格清单写法的问题，不改代码。
  - 观察 O-1～O-10 见文末。
  - 页面相关内容（控制台 `/tokens`、后台 `/llm` 各页）留给检查点 B。

### 1. 导入与发布 `vllm-node`（§16.4.1）——通过（修复 A-2 后）

- 03:24:01 第一次执行 `platform admin app import deploy/apps`，输出 `comfyui: duplicate: 版本号已存在` 后退出，`vllm-node` **没有导入**（缺陷 A-2）。
  - 原因：这台平台已经导入过计划 7 的应用。`ImportDir` 按目录名顺序导入，遇到第一个已存在的版本就整体报错退出。
- 修复后（`0c7e3ad`）于 03:28:12 重新执行，输出：

  ```
  comfyui skipped: 版本 1.0.0 已存在且内容相同，已跳过
  jupyterlab skipped: 版本 1.0.0 已存在且内容相同，已跳过
  open-webui skipped: 版本 1.0.1 已存在且内容相同，已跳过
  vllm skipped: 版本 1.0.4 已存在且内容相同，已跳过
  vllm-node 1.0.0 created
  ```

- 后台 `GET /apps` 显示 `vllm-node`（app 5）为 `draft`，`internal = true`。
- 03:29:00 发布：`POST /apps/5/versions/12/publish {totp}`，返回 `prepull_machines: 1`，版本变为 `published`。随后用 `PUT /apps/5` 把应用状态改为 `published`。
- 控制台 `GET /api/v1/apps` 返回 `['vllm', 'open-webui', 'comfyui', 'jupyterlab']`，不含 `vllm-node`。`GET /api/v1/apps/vllm-node` 返回 404 `app_not_found`「应用不存在或未上架」。

### 2. 建模型与池（§16.4.2）——通过

- 03:29:36 调 `POST /llm/models` 建立模型 `qwen3-4b`（id 1），参数如下：

  | 字段 | 值 |
  |---|---|
  | 名称 | Qwen3-4B |
  | `kind` | `chat` |
  | 来源 | `modelscope`，`Qwen/Qwen3-4B` |
  | 镜像 | `image_id 5`（LLM Serving） |
  | `context_len` | 32768 |
  | `max_output_tokens` | 8192 |
  | `default_max_tokens` | 1024 |
  | 价格（分 / 百万 token） | 输入 50、输出 150，即 ¥0.50 / ¥1.50 |
  | `replicas` | 1 |
  | `max_concurrency` | 32 |
  | `features` | `tools`、`reasoning` |
  | `node_env` | `REASONING_PARSER=qwen3`、`MODEL_SIZE_GB=8` |

  `app_version_id` 留空，表示用最新的已发布版本。返回的模型状态为 `draft`。
- 03:29:4x 调 `POST /llm/models/1/status {online, totp}`，模型变为 `online`。
  - 此时池里还没有机器，Controller 于 03:29:54 发出告警 `llm_pool_short`「Token 服务池没有空闲机器，模型节点缺额」，见观察 O-2。
- 上线后、加入机器前，控制台 `GET /api/v1/llm/models` 返回 `qwen3-4b`，价格 50 / 150，`available: false`（暂无节点）。
- 03:30:06 调 `POST /groups/3/machines {"machine_ids":[1]}`（3 是「Token 服务池」），返回 `{"ok":true}`。
  - 紧接着 `GET /llm/pool` 返回 `machines: [{id 1, B-01, idle, agent_online}]`、`free 1`、`total 1`，此时还没有节点。

### 3. 节点拉起（§16.4.3）——通过（6 分 48 秒）

- 时间线（每 5 秒查一次库，下表是状态变化的时刻）：

  | 时刻 | 事件 |
  |---|---|
  | 03:30:06 | 机器加入池 |
  | 03:30:14 | Controller 建节点 `node-tvwfa1s4kw43`（`provisioning`）和系统实例 `ins-h962of9s`，距加入池 8 秒 |
  | 03:30:17 | 实例 `running` |
  | 03:30:24 | 建部署 `dep-19ux184wn513`（`queued`） |
  | 03:30:29 | `installing / prepare` |
  | 03:30:43 | `download 5%`「开始下载 Qwen/Qwen3-4B（约 8GB）」 |
  | 03:34:35 | `download 68%`「7GB / 约 8GB」 |
  | 03:34:50 | `health 90%`，下载约 4 分钟，约 33 MB/s |
  | 03:36:44 | 部署 `running`，vLLM 启动约 2 分钟 |
  | 03:36:54 | 节点 `ready`，`last_probe_at` 开始刷新 |

  从机器加入池到 `ready` 共 **6 分 48 秒**，要求 ≤ 15 分钟。
- 系统实例 `ins-h962of9s`：
  - `system = true`，用户为系统账号（user 19，`system:llm`）。
  - `expires_at` 为空。
  - 订单数 0，`orders WHERE instance_id = …` 返回 0 行。
  - 后台 `GET /instances?q=ins-h962of9s` 能查到，`system: true`，`order_id: null`。
- 机器详情 `GET /machines/1` 的 `group` 为「Token 服务池」（`purpose llm`，`visible false`）。
- Spark 上的进程：
  - `nvidia-smi` 显示 `VLLM::EngineCore`，占用 101,005 MiB（默认 `GPU_MEMORY_UTILIZATION 0.85`，见观察 O-8）。
  - 容器内的 vLLM 命令行：

    ```
    vllm serve /root/data/models/modelscope/Qwen/Qwen3-4B --host 127.0.0.1 --port 8000 --served-model-name qwen3-4b
      --max-model-len 32768 --gpu-memory-utilization 0.85 --max-num-seqs 64 --reasoning-parser qwen3
      --enable-auto-tool-choice --tool-call-parser hermes
    ```

  - 命令行里没有密钥，`/proc/<pid>/environ` 里有 `VLLM_API_KEY`。
- 控制台 `GET /api/v1/llm/models`：`qwen3-4b` 变为 `available: true`。

### 4. Key 与调用（§16.4.4）——通过

- 03:37:30 用户 A 在控制台调 `POST /api/v1/llm/keys {"name":"test"}`，返回明文 Key（49 字符，`sk-spark-` 开头）和 `key_hint: sk-spark-…Z3vx`。之后 `GET /llm/keys` 只返回 hint。
- 调用结果：

  | 请求 | 结果 |
  |---|---|
  | `GET /v1/models` | 200，`{"object":"list","data":[{"id":"qwen3-4b","object":"model","created":1792870176,"owned_by":"spark-cloud"}]}` |
  | 非流式 `POST /v1/chat/completions`（「用一句话介绍 DGX Spark」，`max_tokens 64`） | 200，用时 3.3 秒，`X-Request-Id: req-wpkvfum64zqi8epp`，`usage {prompt 14, completion 64, total 78}`，`finish_reason length`。正文是被截断的思考内容，见观察 O-1 |
  | 流式（`"stream":true`，`max_tokens 64`，提示词加 `/no_think`） | 200，`Content-Type: text/event-stream`，`X-Accel-Buffering: no`，`X-Request-Id: req-hcjaxx7dg6lo6gmo`。共 41 个 `data:` 事件，倒数第二个是 `usage {prompt 18, completion 40, total 58}`（网关注入的 `include_usage`），最后一个是 `data: [DONE]`。拼出的正文为「DGX Spark 是一种高性能计算平台，专为……」 |
  | OpenAI Python SDK 流式（`base_url=http://192-168-1-218.sslip.io:8080/v1`，`stream_options.include_usage`） | 成功，`id chatcmpl-req-dttg5ioqnvbf1mli`，正文「DGX Spark 配备了 128 千兆字节（GB）的统一内存。」，`usage {completion 28, prompt 23, total 51}` |

- 控制台 `GET /api/v1/llm/usage` 返回 3 条（`total 3`），token 数与上表一致。费用都等于 token 数乘单价：

  | request_id | stream | prompt / completion | cost_micro | 核算（输入 × 50 + 输出 × 150） |
  |---|---|---|---|---|
  | `req-wpkvfum64zqi8epp` | false | 14 / 64 | 10300 | 700 + 9600 |
  | `req-hcjaxx7dg6lo6gmo` | true | 18 / 40 | 6900 | 900 + 6000 |
  | `req-dttg5ioqnvbf1mli` | true | 23 / 28 | 5350 | 1150 + 4200 |

  两条流式请求另有 `ttft_ms` 167 / 162。`GET /v1/models` 不记用量。

### 5. 结算（§16.4.5）——通过（等 10 分钟的路径）

- 03:38:38～03:39:59 按 8 并发跑了 3 轮，共 24 次长文请求（`max_tokens 1500`），用来积累用量。
  - 加上前面 3 次和第 6 项通过的 3 次，结算前共 30 条用量。
  - 合计 2,198,300 微分，约 ¥0.022，远低于 ¥1。因此按「最早一条未结算超过 10 分钟」触发。
- 最早一条未结算用量是 03:37:42，结算在 03:48:24 那一轮清扫完成：

  ```
  llm_settlements #1: requests 30, prompt 766, completion 14400, cost_micro 2198300,
                      usage_amount 2, debt_amount 0, charged 2, wallet_ledger_id 43, period 03:37:42 ~ 03:41:15
  wallet_ledger #43:  llm_pay, -2, balance_after 44798, ref llm_settlement/1,
                      remark「Token 消费 30 次请求 · 输入 766 · 输出 14400 token」
  llm_accounts:       unsettled_micro 0, carry_micro 198300
  ```

  核算：floor(2,198,300 / 10^6) = 2 分，余数 198,300 记入 `carry_micro`。
- 控制台 `GET /api/v1/llm/settlements` 返回这条结算，`wallet_ledger_id 43`。`GET /api/v1/wallet/ledger` 能看到流水 #43。
- 连开发库只读检查：

  ```
  SPARK_DATABASE_URL=…/spark_p9 go test ./internal/llm -run TestInvariantsAgainstDevDB -count=1
  ```

  输出 `invariants hold: 1 nodes, 30 usage rows, 1 settlements`，PASS。验收结束前（39 条用量、4 次结算）和节点移除后又各跑了一次，均 PASS。

### 6. 限流（§16.4.6）——通过

- **RPM 2**：03:40:06 调后台 `PUT /llm/accounts/1 {"rpm_limit":2}`，立即生效。
  - 当时滑动窗口里还有 03:39 的 24 次请求，所以紧接着的 3 次都是 429，`Retry-After: 26`。
  - 等窗口清空后，03:41:06～03:41:07 连发 3 次：前 2 次 200，第 3 次 429，响应如下：

    ```
    HTTP/1.1 429 Too Many Requests
    Retry-After: 59
    {"error":{"message":"每分钟请求数超过上限，请稍后再试","type":"rate_limit_error","code":"rate_limit_exceeded"}}
    ```

  - 之后 `PUT {"rpm_limit":null}` 恢复默认的 60。
- **月上限 ¥0.01**：03:41:15 控制台 `PUT /llm/keys/1 {"monthly_cap":1}`。当时该 Key 本月已花 2 分，下一次请求返回：

  ```
  HTTP/1.1 429
  {"error":{"message":"该 API Key 本月消费已达上限","type":"rate_limit_error","code":"monthly_cap_exceeded"}}
  ```

  之后 `PUT {"monthly_cap":null}` 取消上限，下一次请求 200。
- 被 429 拒绝的请求不写 `llm_usage`，见观察 O-7。

### 7. 余额与欠费（§16.4.7）——通过（按调整后的步骤，见 A-3）

- **余额 0**：03:48:58 后台调账 −44798 分（`POST /users/1/adjust`，带动态码），余额变为 0。下一次请求返回：

  ```
  HTTP/1.1 402
  {"error":{"message":"余额不足，本次预估最多 ¥0.01，可用 ¥0.00","type":"insufficient_quota","code":"insufficient_quota"}}
  ```

- **按清单原步骤**：余额 ¥0.01，同时发 10 个 `max_tokens=512` 的请求。
  - 按真实价格，单次费用最多 512 × 150 = 76,800 微分，不到 0.08 分。网关预检以 `max_tokens` 为上界估算，而且「预检 + 登记在途估算」在同一把锁内完成。
  - 因此这一步**不可能**产生欠费，原因见 A-3。
  - 这一步的验收改为：故意让实际输出超出估算（不带 `max_tokens`，并临时调高输出价、调低 `default_max_tokens`），验证欠费链路。
- **第一轮**（03:50:34 起）：
  - 把输出价临时调成 3000 分 / 百万，`default_max_tokens` 调成 64。
  - 调账 +1 分，余额 ¥0.01。
  - 同时发 10 个不带 `max_tokens` 的长文请求：1 个 200（437 token），9 个 402「余额不足，本次预估最多 ¥0.01，可用 ¥0.00」。
  - 这次的实际费用约 1.31 分，加上进位后只结算 1 分，等于余额，仍然没有欠费。
- **第二轮**（03:51:24 起）：
  - 输出价调成 20000，`default_max_tokens` 调成 32。
  - 调账 +2 分，余额 ¥0.03。
  - 再发 10 个并发请求：1 个 200（467 token，约 9.34 分），9 个 402。
  - 03:51:55 把价格和 `default_max_tokens` 恢复为 150 / 1024。
- **结算**（04:01:24，按最早一条超过 10 分钟触发）：

  ```
  llm_settlements #2: requests 2, prompt 54, completion 904, cost_micro 10653700, usage_amount 10, charged 3, wallet_ledger_id 48
  wallet_ledger #48:  llm_pay, -3, balance_after 0，备注「Token 消费 2 次请求 · 输入 54 · 输出 904 token」
  llm_accounts:       debt 7, carry_micro 852000
  ```

  - 核算：10,653,700 + 进位 198,300 = 10,852,000，结算 10 分、余数 852,000；只扣到 3 分，欠费 7。
  - 平台日志：`WARN llm: user in debt user_id=1 settlement_id=2 debt=7`。
  - 后台 `GET /llm/accounts?user_id=1` 返回 `debt 7`。
- **欠费时请求**：

  ```
  HTTP/1.1 402
  {"error":{"message":"账户有欠费 ¥0.07，请充值后再试","type":"insufficient_quota","code":"insufficient_quota"}}
  ```

- **充值恢复**：
  - 04:04:05 控制台 `POST /recharges {500, alipay}`，再调 `POST /dev/recharges/{id}/pay`（mock），流水 #49 `recharge +500`。
  - 下一轮清扫（04:04:24）补扣欠费：结算 #4 为 `requests 0, debt_amount 7, charged 7`，流水 #50 `llm_pay -7`，`balance_after 493`，备注「Token 消费欠费补扣」（见观察 O-9），`debt` 归零。
  - 之后的请求 200。

### 8. Key 管理（§16.4.8）——通过

- **用户停用**：03:41:58 控制台 `PUT /llm/keys/1 {"status":"disabled"}`。第一次请求就返回 401（0 秒，要求 30 秒内）：

  ```
  {"error":{"message":"API Key 无效、已停用、已撤销或已过期","type":"authentication_error","code":"invalid_api_key"}}
  ```

  `PUT {"status":"active"}` 后下一次请求即恢复 200。
- **后台停用**：`POST /llm/keys/1/disable {"reason":"验收：后台停用"}` 后立即 401。用户在控制台重新启用后恢复 200，与裁定一致：后台停用是普通的 `disabled`，用户可以自己启用。
- **撤销**：
  - 另建 Key `revoke-me`（id 2），先确认它能调用（200）。
  - `DELETE /api/v1/llm/keys/2` 后返回 401 `invalid_api_key`。
  - 列表里 `revoke-me` 的 `status` 为 `revoked`。页面上「已撤销」的显示留给检查点 B。
- **模型白名单**：
  - Key 只能指定非草稿模型。为此建了第二个模型 `other-model`（id 2，Qwen3-0.6B 占位），03:41:49 先上线再立即下线，同一秒完成，没有建节点，也没有触发告警（见观察 O-6）。
  - 新建 Key `only-other`（`model_slugs ["other-model"]`），用它调 `qwen3-4b`：

    ```
    HTTP/1.1 403
    {"error":{"message":"该 API Key 不允许调用模型 qwen3-4b","type":"permission_error","code":"model_not_allowed"}}
    ```

  - 它的 `GET /v1/models` 返回空列表，因为 `other-model` 已下线。
  - `model_slugs ["nope"]` 建 Key 返回 400 `llm_key_invalid`「模型 nope 不存在」。

### 9. 故障与恢复（§16.4.9）——通过

用户 A 此时在欠费验证中，本项的请求都用用户 B 的 Key `fault` 发出。

- **杀 vLLM 进程**：03:52:13 在 Spark 上执行 `docker exec ins-h962of9s sh -c 'kill $(cat /root/data/.spark-apps/vllm-node/service.pid)'`。
  - 03:52:34 告警 `llm_node_unhealthy`（warning）「节点 node-tvwfa1s4kw43（qwen3-4b）健康检查失败」，只有 1 条。
  - 03:52:36 节点变为 `unhealthy`，距杀进程 23 秒，要求 1 分钟内。`error`「部署健康检查未通过」。
  - 请求返回：

    ```
    HTTP/1.1 503
    {"error":{"message":"模型暂无可用节点，请稍后再试","type":"server_error","code":"model_unavailable"}}
    ```

    `llm_usage` 记了 1 条 `status no_node`、`http_status 503`。
  - 03:53:34 部署变为 `failed`，`error_code crashed`「服务进程已退出」。appdeploy 连续两次探测到进程退出才判失败，`crash_count 2`。
- **后台重建**：
  - 03:54:01 调 `POST /llm/nodes/node-tvwfa1s4kw43/rebuild`，节点变为 `provisioning`。同一个部署 Redeploy，`attempt 2`。
  - 部署日志第一行是 `skip install: spec_hash e515989d… and env unchanged`，模型没有重新下载。
  - 03:55:44 部署 `running`，03:55:54 节点 `ready`。从重建到 ready 共 **1 分 53 秒**，要求 ≤ 5 分钟。
  - 之后请求 200。
  - 人工重建不计入 `rebuild_count`（仍为 0），与规定一致。
- **停 Agent 5 分钟以上**：03:56:34 执行 `sudo systemctl stop spark-agent`，Agent 于 03:56:37 退出。
  - 在平台判定失联（`last_seen` 之后 5 分钟）之前，节点保持 `ready`，请求照常 200（见观察 O-4）。
  - 04:02:24 告警 `machine_offline`「Agent 失联超过 5 分钟」，实例 `suspended_at = 04:02:24`。
  - 04:02:54 节点 `unhealthy`，`error`「机器失联，实例已暂停」。此后请求 503 `model_unavailable`（记为 `no_node`）。
  - 同一节点的 `llm_node_unhealthy` 告警仍未处理（03:52 那条），没有重复发。
  - 04:03:32 执行 `sudo systemctl start spark-agent`。Agent 立即重连（`agent_connected_at 04:03:32`），实例的 `suspended_at` 清空。
  - 04:03:36 节点恢复 `ready`，只用了 **4 秒**，要求 ≤ 1 分钟。部署 `attempt` 仍为 2，没有重建。请求 200。

### 安全快查（§16.4.11 中可经 API 验证的部分）——通过

- 在节点容器内执行 `curl 127.0.0.1:8000/v1/models`（不带鉴权），返回 `{"error":"Unauthorized"}`，HTTP 401。`/health` 返回 200，不需要鉴权。`ss -ltnp` 显示 vLLM 只监听 `127.0.0.1:8000`。
- 后台 `GET /deployments/dep-19ux184wn513` 的 `env` 里没有 `API_KEY`，只有 `secret_env: {"API_KEY": true}`，页面据此显示 `***`。
  - 节点密钥共 43 字符。在部署详情 JSON、平台日志、`audit_logs.diff`、`app_deployments.env / log_tail` 里查这个明文，命中都是 0。
  - 容器内的 `env` 文件权限为 `-rw-------`。
- `llm_usage` 共 21 列，没有内容列：`id, request_id, user_id, key_id, model_id, node_id, endpoint, stream, prompt_tokens, completion_tokens, estimated, input_price, output_price, cost_micro, status, http_status, latency_ms, ttft_ms, client_ip, settlement_id, created_at`。
- 平台日志里查不到提示词（「DGX Spark」「GPU 算力租赁」都是 0 次），也查不到用户 Key（`sk-spark-` 0 次）。
- 用户 Key 直接访问 `ins-…-8000` 入口域名的检查（应 302 到登录）需要浏览器 Cookie 流程，留给检查点 B。

### 收尾：释放机器（为其它计划腾出 Spark）

- 检查点 A 结束时，没有保留 ready 节点，而是恢复到可出租状态，并释放了 `spark.lock`。检查点 B 需要重新拉起节点，按第 3 项的耗时约 7 分钟，其中下载约 4 分钟。
- 释放过程：
  - 04:05:58 把 `qwen3-4b` 下线（动态码）。
  - 2 秒后调 `DELETE /groups/3/machines {"machine_ids":[1]}`，返回 409 `llm_pool_machine_busy`「机器上有运行中的节点，请先下线」。这一点与第 10 项的期望一致。
  - 节点 04:06:07 `draining`，04:06:17 `removing`（实例 `wiping`），04:06:22 实例 `released`，04:06:27 节点 `removed`。
  - 之后移出池成功，机器回到原来的「B 区」（group 2）。控制台 `GET /products` 的 `idle_count` 为 1。
- Spark 上 `docker ps -a` 为空，`nvidia-smi` 没有计算进程，磁盘占用 73G。
- 平台、下载服务和状态记录脚本已停止。04:07:34 释放 `spark.lock`。
- 留在 `spark_p9` 里、供检查点 B 复用的数据：
  - 模型 `qwen3-4b` 与 `other-model`，均为 `offline`。
  - 用户 A 的 Key `test`、`only-other`（可用）和 `revoke-me`（已撤销）。
  - 用户 B 的 Key `fault`。
  - 39 条用量、4 次结算。
  - 用户 A 余额 ¥4.93，用户 B 余额 ¥5.00（B 的用量结算为 0 分）。
  - 3 条未结算用量（A 1 条、B 2 条），下次平台运行时会按 10 分钟规则结算。
  - 告警 22～25 未处理。

### 缺陷

| 编号 | 严重度 | 现象 | 根因 | 处置 |
|---|---|---|---|---|
| A-1 | 中 | 按 README「数据库连接」一节在 `SPARK_DATABASE_URL` 里加 `pool_max_conns=20` 后，platform 启动（以及 `platform admin …`）失败：`migrate up: … FATAL: unrecognized configuration parameter "pool_max_conns"` | `db.Migrate` 把 URL 原样交给 `database/sql` 的 pgx 驱动。`pool_*` 是 pgxpool 专用的参数，驱动会把它当作服务器运行参数发给 PostgreSQL | 已修复（`3ee099f`）：迁移连接改为先 `pgxpool.ParseConfig`，再用 `stdlib.OpenDB(*cfg.ConnConfig)` 打开。新增 `TestMigrateAcceptsPoolParams`，带 `pool_max_conns` / `pool_min_conns` 的 URL 迁移成功，并验证 `MaxConns = 10`。`internal/platform/db` 全部测试通过。修复后的平台带 `pool_max_conns=20` 正常运行 |
| A-2 | 中 | 已有部署上按 README 执行 `platform admin app import deploy/apps`，在第一个应用（comfyui 1.0.0 已存在）就报 `duplicate: 版本号已存在` 并退出，`vllm-node` 没有导入，后台无法创建模型。所有升级到计划 9 的环境都会遇到 | `ImportDir` 遇错即停，把「同版本号已存在」当错误。计划 9 是第一次在已有目录上追加新应用 | 已修复（`0c7e3ad`）：已有应用的同版本号，若 `spec_hash`（spec + 脚本）和镜像都相同，就记为 `skipped: 版本 x 已存在且内容相同，已跳过` 并继续导入后面的应用；同版本号但内容不同仍然报错，提示提高版本号。新增 `TestImportDirSkipsIdenticalExistingVersion`，`internal/appdeploy` 和 `cmd/platform` 全部测试通过。真机重跑输出见第 1 项 |
| A-3 | 低（规格） | §16.4 第 7 项「余额 ¥0.01、10 个 `max_tokens=512` 并发 → 部分成功、结算后欠费 > 0」按真实价格不可能出现 | 预检估算 `ceil((len/3 × 输入价 + max_out × 输出价) / 10^6)` 在带 `max_tokens` 时是费用上界。`TryReserveEstimate` 让「预检 + 登记」在同一把锁内完成，并发也不会超额。欠费只在实际费用超过估算时产生：不带 `max_tokens` 且输出超过 `default_max_tokens`（规格 §5「不改写请求」），或提示词 token 数明显多于 `len(body)/3`。集成测试 `TestLLMOverdraftSettlesIntoDebt` 也是靠假节点返回超量的 `completion_tokens` 才造出欠费 | 不改代码。验收按「不带 `max_tokens` + 临时调价」完成（第 7 项）。建议检查点 B 或 `followups-plan9.md` 把清单第 7 项改成「不带 `max_tokens`、输出超过 `default_max_tokens`」的写法 |

### 观察

- **O-1**：Qwen3 默认开思考模式。请求 `max_tokens` 太小（第 4 项的 64）时，`</think>` 还没出现输出就被截断了。vLLM 0.10.1 的 `qwen3` 解析器在这种情况下把整段（含 `<think>`）放进 `content`，`reasoning_content` 为 `null`。这是 vLLM 的行为，不是网关的问题。提示词加 `/no_think`，或给足 `max_tokens`，输出就正常（流式与 SDK 两次都是这样）。帮助页可以提醒一句。
- **O-2**：模型上线时池里还没有机器，所以 03:29:54 发了一条 `llm_pool_short`。12 秒后机器入池、节点建好，但告警按平台惯例需要人工处理，不会自动关闭。建议 README 的步骤写成「先把机器加入池，再上线模型」，避免上线时的噪声告警。
- **O-3**：03:23:22 平台一启动就发了 `machine_offline`。原因是克隆库里机器的 `last_seen_at` 停在其它计划的平台使用期间，Agent 16 秒后就重连了。这是克隆环境带来的，与计划 9 无关。
- **O-4**：停 Agent 后，平台要等 `last_seen` 之后 5 分钟才判定机器失联、暂停实例（与计划 2/6 相同）。这段时间里网关仍经 SSH 上游直连实例，vLLM 照常服务，节点保持 `ready`，请求 200。从失联判定到节点 `unhealthy` 约 30 秒。这不影响计费与对账。
- **O-5**：这次部署下载 Qwen3-4B（约 7.6 GB）用了约 4 分钟（约 33 MB/s），比计划 7 验收时的 2.6 分钟慢，属于 ModelScope 侧的波动。vLLM 启动约 2 分钟。重建时跳过下载，约 1 分 50 秒。
- **O-6**：Key 的 `model_slugs` 只接受非草稿模型，而草稿只能先上线（要求 `replicas ≥ 1`）才能下线。为了验证白名单，只能把 `other-model` 短暂上线。这次上线和下线在同一秒内完成，Controller 没来得及建节点，也没有告警。如果运营要预先给用户开「只允许某新模型」的 Key，需要先上线该模型。
- **O-7**：被限流（429）、余额 / 欠费预检（402）、Key 鉴权（401 / 403）拒绝的请求不写 `llm_usage`。网关选不到节点的 503 记为 `status no_node`，`cost 0`。全程 39 条 `llm_usage`：37 条 ok，2 条 no_node（第 9 项杀进程后和停 Agent 后各 1 条）。后台 `/llm/usage` 的 `total` 为 39，与库一致。
- **O-8**：`vllm-node` 的默认 `GPU_MEMORY_UTILIZATION 0.85` 让 4B 模型占了约 101 GB 统一内存，剩下的大都是 KV cache。一机一模型时没有问题。小模型若想省内存，可以在模型的 `node_env` 里调低。
- **O-9**：只补欠费、没有新用量的结算，流水备注是「Token 消费欠费补扣」；同时有用量时备注是「……（含欠费 ¥x.xx）」。代码与 `internal/llm/settle_test.go` 的单测一致。清单第 7 项写的「含欠费」只对后一种情况成立，本次充值后没有新用量，属于前一种。
- **O-10**：现场协调方面，03:12 拿到 `spark.lock` 时，计划 11 的平台和一个 ComfyUI 实例还在 Spark 上运行。计划 11 的验收代理当时仍在操作，锁看起来是被提前释放了。本次先持锁等待，03:22 它自己停掉平台、释放实例之后才启动本平台，没有干扰它的数据。
