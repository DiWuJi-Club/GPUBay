# Token 商城

- 计费：按实际 token 数计费，价格以「元 / 百万 token」标示，输入（提示词）与输出（生成内容）分别计价，向量模型只计输入；请求未指定 max_tokens 时按模型的最大输出长度处理。上游错误、上游超时（未输出内容时）、无可用节点、服务重启中断的请求不收费；流式输出已返回部分内容后因 10 分钟总时长上限被截断的，按已返回的内容计费。
- 结算：费用先累计，累计到 ¥1 或最早一笔满 10 分钟时从钱包余额扣款一次，每次一条「Token 消费」流水；在「Token 商城 → 用量」可查看结算记录并跳转到对应流水。
- 限额（按账户，所有 Key 共享）：默认每分钟 60 次请求、每分钟 200K token、同时 8 个并发请求，超出返回 429；需要更高限额请联系客服。每个 Key 还可以设置月消费上限。
- Key 安全：明文 Key 只在创建时显示一次，平台只保存哈希、无法找回；请勿写进前端代码或公开仓库，一旦泄露请立即在「API Keys」撤销并重新创建。
- 欠费：每次调用前按余额预检，但并发请求可能让实际费用超过余额，差额记为欠费；有欠费时所有调用返回 402，充值后下一次结算自动补扣，补齐后恢复调用。
- 创建、修改、撤销 API Key 请在「Token 商城 → API Keys」（`/tokens?tab=keys`）自己操作；AI 管家不能替您创建或查看 Key。

## 常见错误码

- 400 invalid_request_error：请求参数错误：Content-Type 不是 application/json、缺少 model、端点与模型类型不符（如对话模型调 embeddings）、max_tokens 超过模型的最大输出，或 n / best_of 大于 8
- 401 invalid_api_key：API Key 不存在、格式错误、已停用、已撤销或已过期
- 402 insufficient_quota：余额不足以支付本次预估费用，或账户有欠费；充值后即可继续调用
- 403 account_banned：账号已被封禁
- 403 kyc_required：账号未完成实名认证
- 403 account_blocked：Token 服务已被平台停用（消息中带原因）
- 403 model_not_allowed：该 Key 不允许调用这个模型（创建 Key 时限定了模型）
- 404 model_not_found：模型不存在或未上线
- 404 not_found：接口不存在（只支持 chat/completions、completions、embeddings、models）
- 405 method_not_allowed：请求方法不对：models 用 GET，其余接口用 POST
- 413 request_too_large：请求体超过 2 MiB
- 429 rate_limit_exceeded：超过每分钟请求数、每分钟 token 数或并发上限，按响应头 Retry-After 稍后重试
- 429 monthly_cap_exceeded：该 Key 本月消费已达设定的月上限
- 500 internal_error：平台内部错误，请稍后重试；持续出现请联系客服
- 502 upstream_error：模型节点出错，本次不收费，可直接重试
- 503 model_unavailable：模型暂无可用节点，请稍后再试
- 503 server_overloaded：模型节点繁忙，按响应头 Retry-After 稍后重试
- 504 upstream_timeout：模型节点响应超时（请求总时长上限 10 分钟），未输出内容时不收费，可直接重试
