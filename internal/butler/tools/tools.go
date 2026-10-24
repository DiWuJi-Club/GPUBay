// Package tools 是 AI 管家的工具注册表、参数校验器与工具实现（规格 §4）。
// 依赖方向：tools → llm / guard / help / rental / appdeploy / catalog / wallet / identity；从不导入 butler。
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"spark-cloud/internal/butler/llm"
	"spark-cloud/internal/platform/apperr"
)

// Kind 是工具种类：读工具自动执行；写工具先生成确认卡，用户确认后才执行。
type Kind string

const (
	Read  Kind = "read"
	Write Kind = "write"
)

// Caller 是工具执行时的调用方（永远是对话所属用户本人）。IdemKey 只对写工具有意义（"butler:" + tc.ID，由 Task 7 填）。
type Caller struct {
	UserID  int64
	IP      string
	IdemKey string
	Now     time.Time // 零值 = Deps.Clock.Now()
}

// Tool 是一个工具。Schema 必须是根为 object、additionalProperties=false 的 JSON Schema（注册时编译校验）。
type Tool struct {
	Name, Description string
	Kind              Kind
	Schema            json.RawMessage
	Danger            bool // 前端红色 + 勾选（release_instance / remove_app / cancel_booking）
	// Exec 执行工具（读工具在模型调用时执行；写工具在 Confirm 时执行）。args 已经过 Registry.Validate 规范化。
	// 读工具：业务错误（apperr）与其它错误都以 error 返回，调用方用 ErrorResult 转为回填模型的结果。
	Exec func(ctx context.Context, c Caller, args json.RawMessage) (Result, error)
	// Summary 仅写工具：生成确认卡（在 pending 落库前调用）。
	Summary func(ctx context.Context, c Caller, args json.RawMessage) (Card, error)
}

// Result 是工具结果。
type Result struct {
	Content string // 已包裹 / 净化的最终文本（回填模型、落库）
	Ref     string // 结果涉及的资源 ID（实例 / 部署 / 订单），写入 butler_tool_calls.result_ref
	Secret  bool   // true：落库时整段替换为 {"secret":"***"}（generate_secret）
}

// Card 是写工具的确认卡（服务端按真实 args 渲染）。
type Card struct {
	Summary       string            // 给用户看，含真实 secret（只进 SSE confirm 事件与 GET 会话的 pending_tool_call）
	StoredSummary string            // 落库，secret 为 ***
	Quote         *int64            // 预计扣款 / 退款（分）；nil = 不涉及金额
	ArgsStored    json.RawMessage   // 落库的 args，secret 为 ***
	SecretEnv     map[string]string // secret 变量真实值（secretbox 后进 secret_env_enc）
}

// Registry 是全部工具（读 15 + 写 8）。
type Registry struct {
	tools   map[string]Tool
	schemas map[string]*Schema
	names   []string // 按名字排序
}

// NewRegistry 注册全部工具。工具名、Label、schema 不合法时 panic（编程错误，启动即暴露）。
func NewRegistry(d Deps) *Registry {
	r := &Registry{tools: map[string]Tool{}, schemas: map[string]*Schema{}}
	for _, t := range readTools(d) {
		r.register(t)
	}
	for _, t := range writeTools(d) {
		r.register(t)
	}
	sort.Strings(r.names)
	return r
}

func (r *Registry) register(t Tool) {
	if _, dup := r.tools[t.Name]; dup {
		panic("butler tools: duplicate tool " + t.Name)
	}
	if Labels[t.Name] == "" {
		panic("butler tools: no label for " + t.Name)
	}
	if t.Kind != Read && t.Kind != Write {
		panic("butler tools: bad kind for " + t.Name)
	}
	if t.Exec == nil || (t.Kind == Write && t.Summary == nil) {
		panic("butler tools: missing Exec / Summary for " + t.Name)
	}
	s, err := Compile(t.Schema)
	if err != nil {
		panic(fmt.Sprintf("butler tools: schema of %s: %v", t.Name, err))
	}
	if s.Type != "object" || !s.Closed {
		panic("butler tools: schema of " + t.Name + " must be a closed object")
	}
	r.tools[t.Name] = t
	r.schemas[t.Name] = s
	r.names = append(r.names, t.Name)
}

// Defs 返回给模型的工具声明，按名字排序（同一部署内字节稳定，利于提示缓存）。
func (r *Registry) Defs() []llm.ToolDef {
	out := make([]llm.ToolDef, 0, len(r.names))
	for _, n := range r.names {
		t := r.tools[n]
		out = append(out, llm.ToolDef{Name: t.Name, Description: t.Description, Schema: t.Schema})
	}
	return out
}

// Get 按名字取工具。
func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// Names 返回全部工具名（排序）。
func (r *Registry) Names() []string { return append([]string(nil), r.names...) }

// Validate 按工具 schema 校验并规范化参数（去 null、填默认）。未知工具 → 「未知工具 <name>」错误（IsUnknownTool 为 true，
// 不是 *ValidationError）；校验失败 → *ValidationError，文案「参数 <path>：<原因>」。
func (r *Registry) Validate(name string, args json.RawMessage) (json.RawMessage, error) {
	s, ok := r.schemas[name]
	if !ok {
		return nil, &unknownToolError{name: name}
	}
	return s.Validate(args)
}

type unknownToolError struct{ name string }

func (e *unknownToolError) Error() string { return "未知工具 " + e.name }

// IsUnknownTool 判断 Validate 的错误是否为「未知工具」。
func IsUnknownTool(err error) bool {
	var u *unknownToolError
	return errors.As(err, &u)
}

// ErrorResult 把工具错误转成回填模型的结果 {"ok":false,"code","message"}（平台生成，不包裹）：
// apperr → 其 code / 文案；参数错误 → invalid_args；未知工具 → unknown_tool；其它 → tool_failed + 通用文案（不泄露内部错误）。
func ErrorResult(err error) Result {
	code, msg := "tool_failed", "工具执行失败，请稍后再试"
	var ae *apperr.Error
	var ve *ValidationError
	switch {
	case errors.As(err, &ae):
		code, msg = ae.Code, ae.Message
	case errors.As(err, &ve):
		code, msg = "invalid_args", ve.Error()
	case IsUnknownTool(err):
		code, msg = "unknown_tool", err.Error()
	case errors.Is(err, context.DeadlineExceeded):
		code, msg = "tool_timeout", "工具执行超时，请稍后再试"
	}
	b, _ := encodeValue(map[string]any{"ok": false, "code": code, "message": msg})
	return Result{Content: string(b)}
}

// IsBusinessError 判断 err 是否为可展示给用户的业务错误（apperr / 参数错误）；否则调用方按内部错误记 failed。
func IsBusinessError(err error) bool {
	var ae *apperr.Error
	var ve *ValidationError
	return errors.As(err, &ae) || errors.As(err, &ve) || IsUnknownTool(err)
}

// Label 返回工具中文名；未知工具返回原名。
func Label(name string) string {
	if l, ok := Labels[name]; ok {
		return l
	}
	return name
}
