// Package anthropic 是 Anthropic Messages API（流式）的 llm.Provider 适配器（规格 §3.3，可选）。
// 本包是全仓库唯一导入 anthropic-sdk-go 的地方。
// 约定（裁定「anthropic 适配器」）：固定系统提示带 ephemeral cache_control，动态快照段不缓存；
// 工具 input_schema 原样映射（顶层 additionalProperties = false），**不发 strict**——strict 模式不支持
// minimum / maxLength 等关键字且有数量限制，参数由服务端 tools.Validate 校验；output_config.effort = low；
// 不设 thinking、不设 tool_choice（保持默认 auto）、不开 fallbacks。
package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"spark-cloud/internal/butler/llm"
)

const (
	defaultMaxTokens = 2048
	maxErrBodyBytes  = 4096
	invalidArgRunes  = 200
)

// Config 是适配器配置；BaseURL 为空时用 SDK 默认端点（https://api.anthropic.com），请求发到 BaseURL + "/v1/messages"。
type Config struct {
	BaseURL   string
	APIKey    string
	Model     string
	MaxTokens int
	HTTP      *http.Client // nil → SDK 默认 Client（超时由 ctx 控制）

	opts []option.RequestOption // 测试用附加选项（如关闭重试）
}

// Provider 实现 llm.Provider。
type Provider struct {
	cfg    Config
	client sdk.Client
}

func New(cfg Config) *Provider {
	opts := []option.RequestOption{
		// 只认 SPARK_LLM_* 配置：不读 ANTHROPIC_API_KEY / ANTHROPIC_BASE_URL / 本机 profile 等环境凭据
		option.WithoutEnvironmentDefaults(),
		// 不产生 OpenTelemetry span（span 可能带消息内容）
		option.WithoutOpenTelemetry(),
		option.WithAPIKey(cfg.APIKey),
	}
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	if cfg.HTTP != nil {
		opts = append(opts, option.WithHTTPClient(cfg.HTTP))
	}
	opts = append(opts, cfg.opts...)
	return &Provider{cfg: cfg, client: sdk.NewClient(opts...)}
}

func (p *Provider) Name() string  { return "anthropic" }
func (p *Provider) Model() string { return p.cfg.Model }

// ---- 请求映射 ----

// callID 取模型给的调用 ID；历史里缺失时退回本系统的 tool_call_id（两端一致即可配对）。
func callID(b llm.Block) string { return llm.CallID(b) }

// buildParams 组装请求；工具 Schema 不是合法 JSON 对象时返回 *llm.RequestError（本地配置错误，不发请求）。
func (p *Provider) buildParams(req llm.Request) (sdk.MessageNewParams, error) {
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = p.cfg.MaxTokens
	}
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}
	params := sdk.MessageNewParams{
		Model:        sdk.Model(p.cfg.Model),
		MaxTokens:    int64(maxTokens),
		Messages:     mapMessages(llm.PairToolCalls(req.Messages)),
		OutputConfig: sdk.OutputConfigParam{Effort: sdk.OutputConfigEffortLow},
	}
	// 固定前缀带 cache_control（缓存断点在它末尾）；每轮变化的快照段放在断点之后，不影响缓存命中
	if req.System != "" {
		params.System = append(params.System, sdk.TextBlockParam{Text: req.System, CacheControl: sdk.NewCacheControlEphemeralParam()})
	}
	if strings.TrimSpace(req.SystemDynamic) != "" {
		params.System = append(params.System, sdk.TextBlockParam{Text: req.SystemDynamic})
	}
	// Tools 为空（SPARK_LLM_TOOLS=off）时不发 tools
	for _, t := range req.Tools {
		schema, err := inputSchema(t.Schema)
		if err != nil {
			return params, &llm.RequestError{Body: fmt.Sprintf("工具 %s 的 schema 无效: %v", t.Name, err)}
		}
		tp := sdk.ToolParam{Name: t.Name, InputSchema: schema}
		if t.Description != "" {
			tp.Description = sdk.String(t.Description)
		}
		params.Tools = append(params.Tools, sdk.ToolUnionParam{OfTool: &tp})
	}
	return params, nil
}

// inputSchema 把完整的 JSON Schema 对象映射到 SDK 结构：properties / required 走具名字段，
// 其余键（additionalProperties、嵌套约束等）走 ExtraFields，序列化结果与原 schema 一致。
// 顶层 additionalProperties 未写时补 false。空 schema → 无参数对象；非对象 JSON → 错误。
func inputSchema(raw json.RawMessage) (sdk.ToolInputSchemaParam, error) {
	var m map[string]any
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &m); err != nil {
			return sdk.ToolInputSchemaParam{}, err
		}
		if m == nil {
			return sdk.ToolInputSchemaParam{}, errors.New("不是 JSON 对象")
		}
	}
	s := sdk.ToolInputSchemaParam{Properties: map[string]any{}, ExtraFields: map[string]any{}}
	for k, v := range m {
		switch k {
		case "type": // 固定 object
		case "properties":
			if v != nil {
				s.Properties = v
			}
		case "required":
			if arr, ok := v.([]any); ok {
				for _, x := range arr {
					if str, ok := x.(string); ok {
						s.Required = append(s.Required, str)
					}
				}
			}
		default:
			s.ExtraFields[k] = v
		}
	}
	if _, ok := s.ExtraFields["additionalProperties"]; !ok {
		s.ExtraFields["additionalProperties"] = false
	}
	return s, nil
}

// toolResultBlock：空内容不发空文本块（API 拒绝空 text 块）。
func toolResultBlock(b llm.Block) sdk.ContentBlockParamUnion {
	if b.Content == "" {
		return sdk.ContentBlockParamUnion{OfToolResult: &sdk.ToolResultBlockParam{ToolUseID: callID(b), IsError: sdk.Bool(!b.OK)}}
	}
	return sdk.NewToolResultBlock(callID(b), b.Content, !b.OK)
}

// mapMessages 把内部历史映射为 Anthropic 消息：tool 消息作为 user 消息回填 tool_result；
// 连续同角色（含相邻多条 tool 消息）合并成一条——Anthropic 要求角色交替、一次性回填全部 tool_result；
// 合并后的 user 消息里 tool_result 块排在文本之前。
func mapMessages(msgs []llm.Message) []sdk.MessageParam {
	type turn struct {
		role    sdk.MessageParamRole
		results []sdk.ContentBlockParamUnion // 仅 user：tool_result
		blocks  []sdk.ContentBlockParamUnion
	}
	var turns []*turn
	add := func(role sdk.MessageParamRole, results, blocks []sdk.ContentBlockParamUnion) {
		if len(results) == 0 && len(blocks) == 0 {
			return
		}
		if n := len(turns); n > 0 && turns[n-1].role == role {
			turns[n-1].results = append(turns[n-1].results, results...)
			turns[n-1].blocks = append(turns[n-1].blocks, blocks...)
			return
		}
		turns = append(turns, &turn{role: role, results: results, blocks: blocks})
	}
	for _, m := range msgs {
		var blocks, results []sdk.ContentBlockParamUnion
		switch m.Role {
		case llm.RoleUser:
			for _, b := range m.Blocks {
				if b.Type == llm.BlockText && strings.TrimSpace(b.Text) != "" { // context 块不作为用户文本；空白文本块 API 拒绝
					blocks = append(blocks, sdk.NewTextBlock(b.Text))
				}
			}
			add(sdk.MessageParamRoleUser, nil, blocks)
		case llm.RoleAssistant:
			for _, b := range m.Blocks {
				switch b.Type {
				case llm.BlockText:
					if strings.TrimSpace(b.Text) != "" {
						blocks = append(blocks, sdk.NewTextBlock(b.Text))
					}
				case llm.BlockToolUse:
					args := json.RawMessage(strings.TrimSpace(string(b.Args)))
					if len(args) == 0 || !json.Valid(args) {
						args = json.RawMessage(`{}`)
					}
					blocks = append(blocks, sdk.NewToolUseBlock(callID(b), args, b.Name))
				}
			}
			add(sdk.MessageParamRoleAssistant, nil, blocks)
		case llm.RoleTool:
			for _, b := range m.Blocks {
				if b.Type == llm.BlockToolResult {
					results = append(results, toolResultBlock(b))
				}
			}
			add(sdk.MessageParamRoleUser, results, nil)
		}
	}
	out := make([]sdk.MessageParam, 0, len(turns))
	for _, t := range turns {
		out = append(out, sdk.MessageParam{Role: t.role, Content: append(t.results, t.blocks...)})
	}
	return out
}

// ---- 流式响应 ----

// validOrMarker：参数是合法 JSON 对象 → 原文；空 → {}；否则 {"_invalid_json": "<前 200 rune>"}（Run 层据此回填错误）。
func validOrMarker(s string) json.RawMessage {
	t := strings.TrimSpace(s)
	if t == "" {
		return json.RawMessage(`{}`)
	}
	if json.Valid([]byte(t)) && strings.HasPrefix(t, "{") {
		return json.RawMessage(t)
	}
	r := []rune(t)
	if len(r) > invalidArgRunes {
		r = r[:invalidArgRunes]
	}
	b, _ := json.Marshal(map[string]string{"_invalid_json": string(r)})
	return b
}

// mapStop 把 Anthropic stop_reason 映射为 llm.Stop*。
func mapStop(s string) string {
	switch s {
	case string(sdk.StopReasonToolUse):
		return llm.StopToolUse
	case string(sdk.StopReasonMaxTokens), string(sdk.StopReasonModelContextWindowExceeded):
		return llm.StopMaxTokens
	case string(sdk.StopReasonRefusal):
		return llm.StopRefusal
	}
	return llm.StopEndTurn // end_turn / stop_sequence / pause_turn / 未知值
}

type toolBlock struct {
	id, name string
	start    any // content_block_start 里的 input（没有 input_json_delta 时用它）
	deltas   bool
	args     strings.Builder
}

// argsJSON：有增量用拼接结果；否则退回 content_block_start 的 input。
func (tb *toolBlock) argsJSON() json.RawMessage {
	if tb.deltas || tb.start == nil {
		return validOrMarker(tb.args.String())
	}
	b, err := json.Marshal(tb.start)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return validOrMarker(string(b))
}

func (p *Provider) Chat(ctx context.Context, req llm.Request, emit func(llm.Event)) error {
	params, err := p.buildParams(req)
	if err != nil {
		return err
	}
	stream := p.client.Messages.NewStreaming(ctx, params)
	defer stream.Close()

	open := map[int64]*toolBlock{} // 块 index → 累积中的 tool_use
	var calls []llm.ToolCall       // 已完整结束的 tool_use；流成功结束后才发出
	var usage llm.Usage
	stop := llm.StopEndTurn
	finished := false // 收到 message_stop；没有就断流 → 不可用
	for stream.Next() {
		switch ev := stream.Current().AsAny().(type) {
		case sdk.MessageStartEvent:
			u := ev.Message.Usage
			usage.InputTokens = u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
			usage.OutputTokens = u.OutputTokens
		case sdk.ContentBlockStartEvent:
			if ev.ContentBlock.Type == "tool_use" {
				open[ev.Index] = &toolBlock{id: ev.ContentBlock.ID, name: ev.ContentBlock.Name, start: ev.ContentBlock.Input}
			}
		case sdk.ContentBlockDeltaEvent:
			switch d := ev.Delta.AsAny().(type) {
			case sdk.TextDelta:
				if d.Text != "" {
					emit(llm.Event{Type: llm.EvText, Text: d.Text})
				}
			case sdk.InputJSONDelta:
				if tb, ok := open[ev.Index]; ok {
					tb.deltas = true
					tb.args.WriteString(d.PartialJSON)
				}
			}
		case sdk.ContentBlockStopEvent:
			if tb, ok := open[ev.Index]; ok {
				delete(open, ev.Index)
				calls = append(calls, llm.ToolCall{ProviderCallID: tb.id, Name: tb.name, Args: tb.argsJSON()})
			}
		case sdk.MessageDeltaEvent:
			// message_delta.usage 是累计值：输出取最新；输入只取更大值（有的实现在这里只给不含缓存的 input_tokens）
			u := ev.Usage
			if in := u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens; in > usage.InputTokens {
				usage.InputTokens = in
			}
			usage.OutputTokens = u.OutputTokens
			if ev.Delta.StopReason != "" {
				stop = mapStop(string(ev.Delta.StopReason))
			}
		case sdk.MessageStopEvent:
			finished = true
		}
	}
	if err := stream.Err(); err != nil {
		return classify(ctx, err)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if !finished {
		return &llm.UnavailableError{Cause: errors.New("模型响应流意外结束")}
	}
	switch stop {
	case llm.StopMaxTokens, llm.StopRefusal:
		// 被截断 / 拒绝：工具调用一律丢弃（参数可能不完整，不能执行），停止原因保持不变
	default:
		for i := range calls {
			emit(llm.Event{Type: llm.EvTool, ToolCall: &calls[i]})
		}
		if len(calls) > 0 {
			stop = llm.StopToolUse
		}
	}
	emit(llm.Event{Type: llm.EvUsage, Usage: &usage})
	emit(llm.Event{Type: llm.EvDone, StopReason: stop})
	return nil
}

// cutUTF8 按字节上限截断，不切开多字节字符。
func cutUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// errBody 取 API 错误体（截断到 4KB）；非 JSON / 空体时退回 HTTP 状态文本，保证非空。
func errBody(apiErr *sdk.Error) string {
	body := strings.TrimSpace(apiErr.RawJSON())
	if body == "" {
		body = http.StatusText(apiErr.StatusCode)
		if body == "" {
			body = fmt.Sprintf("HTTP %d", apiErr.StatusCode)
		}
	}
	return cutUTF8(body, maxErrBodyBytes)
}

// classify：ctx 结束 → ctx.Err()；API 错误 4xx（除 408 / 409 / 429）→ RequestError；
// 5xx / 408 / 409 / 429 / 流中 error 事件 / 连接错误 → UnavailableError。
func classify(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var apiErr *sdk.Error
	if errors.As(err, &apiErr) {
		code := apiErr.StatusCode
		switch {
		case code == http.StatusRequestTimeout, code == http.StatusConflict, code == http.StatusTooManyRequests:
		case code >= 400 && code < 500:
			return &llm.RequestError{Status: code, Body: errBody(apiErr)}
		}
		status := code
		if status/100 == 2 { // 流中 error 事件：HTTP 200 已发出，按连接级故障处理
			status = 0
		}
		return &llm.UnavailableError{Status: status, Cause: errors.New(errBody(apiErr))}
	}
	return &llm.UnavailableError{Cause: err}
}
