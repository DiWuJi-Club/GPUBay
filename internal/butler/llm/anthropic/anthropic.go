// Package anthropic 是 Anthropic Messages API（流式）的 llm.Provider 适配器（规格 §3.3，可选）。
// 本包是全仓库唯一导入 anthropic-sdk-go 的地方。
// 约定（裁定「anthropic 适配器」）：系统提示带 ephemeral cache_control；工具 strict = true、
// input_schema 原样映射（additionalProperties = false）；output_config.effort = low；
// 不设 thinking、不设 tool_choice（保持默认 auto）、不开 fallbacks。
package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

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
func callID(b llm.Block) string {
	if b.ProviderCallID != "" {
		return b.ProviderCallID
	}
	return b.ToolCallID
}

func (p *Provider) buildParams(req llm.Request) sdk.MessageNewParams {
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
		Messages:     mapMessages(req.Messages),
		OutputConfig: sdk.OutputConfigParam{Effort: sdk.OutputConfigEffortLow},
	}
	if req.System != "" {
		params.System = []sdk.TextBlockParam{{Text: req.System, CacheControl: sdk.NewCacheControlEphemeralParam()}}
	}
	// Tools 为空（SPARK_LLM_TOOLS=off）时不发 tools
	for _, t := range req.Tools {
		tp := sdk.ToolParam{Name: t.Name, InputSchema: inputSchema(t.Schema), Strict: sdk.Bool(true)}
		if t.Description != "" {
			tp.Description = sdk.String(t.Description)
		}
		params.Tools = append(params.Tools, sdk.ToolUnionParam{OfTool: &tp})
	}
	return params
}

// inputSchema 把完整的 JSON Schema 对象映射到 SDK 结构：properties / required 走具名字段，
// 其余键（additionalProperties 等）走 ExtraFields，序列化结果与原 schema 一致。
// strict 工具要求 additionalProperties = false：schema 未写时补上。
func inputSchema(raw json.RawMessage) sdk.ToolInputSchemaParam {
	var m map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &m) // 非法 schema → 按空对象处理
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
	return s
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
				if b.Type == llm.BlockText && b.Text != "" { // context 块不作为用户文本
					blocks = append(blocks, sdk.NewTextBlock(b.Text))
				}
			}
			add(sdk.MessageParamRoleUser, nil, blocks)
		case llm.RoleAssistant:
			for _, b := range m.Blocks {
				switch b.Type {
				case llm.BlockText:
					if b.Text != "" {
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
	args     strings.Builder
}

func (p *Provider) Chat(ctx context.Context, req llm.Request, emit func(llm.Event)) error {
	stream := p.client.Messages.NewStreaming(ctx, p.buildParams(req))
	defer stream.Close()

	tools := map[int64]*toolBlock{} // 块 index → 累积中的 tool_use
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
				tools[ev.Index] = &toolBlock{id: ev.ContentBlock.ID, name: ev.ContentBlock.Name}
			}
		case sdk.ContentBlockDeltaEvent:
			switch d := ev.Delta.AsAny().(type) {
			case sdk.TextDelta:
				if d.Text != "" {
					emit(llm.Event{Type: llm.EvText, Text: d.Text})
				}
			case sdk.InputJSONDelta:
				if tb, ok := tools[ev.Index]; ok {
					tb.args.WriteString(d.PartialJSON)
				}
			}
		case sdk.ContentBlockStopEvent:
			if tb, ok := tools[ev.Index]; ok {
				delete(tools, ev.Index)
				emit(llm.Event{Type: llm.EvTool, ToolCall: &llm.ToolCall{ProviderCallID: tb.id, Name: tb.name, Args: validOrMarker(tb.args.String())}})
			}
		case sdk.MessageDeltaEvent:
			// message_delta.usage 是累计值：输出取最新；输入若给出则以它为准
			u := ev.Usage
			if in := u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens; in > 0 {
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
	emit(llm.Event{Type: llm.EvUsage, Usage: &usage})
	emit(llm.Event{Type: llm.EvDone, StopReason: stop})
	return nil
}

// classify：ctx 结束 → ctx.Err()；API 错误 4xx（除 429）→ RequestError；5xx / 429 / 流中 error 事件 / 连接错误 → UnavailableError。
func classify(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var apiErr *sdk.Error
	if errors.As(err, &apiErr) {
		code := apiErr.StatusCode
		if code >= 400 && code < 500 && code != http.StatusTooManyRequests {
			body := apiErr.RawJSON()
			if len(body) > maxErrBodyBytes {
				body = body[:maxErrBodyBytes]
			}
			return &llm.RequestError{Status: code, Body: body}
		}
		status := code
		if status/100 == 2 { // 流中 error 事件：HTTP 200 已发出，按连接级故障处理
			status = 0
		}
		return &llm.UnavailableError{Status: status, Cause: errors.New(apiErr.RawJSON())}
	}
	return &llm.UnavailableError{Cause: err}
}
