package butler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"runtime/debug"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/butler/llm"
	"spark-cloud/internal/butler/prompt"
	"spark-cloud/internal/butler/tools"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/randx"
)

// 固定文案：既落库为 assistant 消息，也作为 delta 推给前端（清扫器的中断提示只落库）。
const (
	TextIterationLimit = "这个问题我查了太多次还没有结论，请换个方式描述或联系客服"
	TextRefusal        = "抱歉，这个问题我无法回答。"
	TextInterrupted    = "本轮处理中断，请重试"
	TextRunTimeout     = "这次处理时间太长，已停止，请重试"
)

// 写工具未执行时回填模型的结果文案（code：rejected / expired；mock 规则按 code 收尾）。
const (
	msgRejected    = "用户拒绝了该操作"
	msgExpired     = "操作已过期未执行"
	msgUnconfirmed = "用户未确认，操作未执行"
)

// secretStored 是 generate_secret 结果落库时的内容（明文只在本轮内存里给模型）。
const secretStored = `{"secret":"***"}`

var contextInstanceRe = regexp.MustCompile(`^ins-[a-z0-9]{1,32}$`)

// SendRequest 是一条用户消息。ConversationID 为 "" 或 "new" 表示新建对话。
type SendRequest struct {
	UserID         int64
	IP             string
	ConversationID string
	Text           string
	Context        Context
}

// errContent 是平台生成的错误结果 {"code","message","ok":false}（与 tools.ErrorResult 同形）。
func errContent(code, msg string) string {
	b, _ := json.Marshal(map[string]any{"ok": false, "code": code, "message": msg})
	return string(b)
}

// title 是对话标题：首条用户消息前 TitleRunes 个字（空白压成一个空格）。
func title(text string) string {
	t := strings.Join(strings.Fields(text), " ")
	r := []rune(t)
	if len(r) > TitleRunes {
		r = r[:TitleRunes]
	}
	return string(r)
}

// Send 处理一条用户消息并同步跑完本轮（返回时对话已是 idle / awaiting_confirm，或被清扫器接管）。
// 校验错误（消息非法、关闭、他人上下文实例、额度、busy、轮数上限）在任何 Emit 之前以 error 返回；
// 一旦推出 start，之后的问题（模型不可用等）都以 SSE error 事件报告，返回 err = nil。
// 本轮在脱离请求 ctx 的 RunTimeout ctx 里运行：客户端断开不影响落库。
func (s *Service) Send(ctx context.Context, req SendRequest, sink Sink) (Conversation, error) {
	if sink == nil {
		sink = NopSink{}
	}
	text := strings.TrimSpace(req.Text)
	if n := utf8.RuneCountInString(text); n == 0 || n > MaxUserChars || strings.ContainsRune(text, 0) || !utf8.ValidString(text) {
		return Conversation{}, apperr.ErrButlerMessageInvalid
	}
	st, err := s.Settings(ctx)
	if err != nil {
		return Conversation{}, err
	}
	if !st.Enabled {
		return Conversation{}, disabledErr(st.Notice)
	}
	if id := req.Context.InstanceID; id != "" {
		if !contextInstanceRe.MatchString(id) || s.d.Rental == nil {
			return Conversation{}, apperr.ErrNotFound
		}
		if _, err := s.d.Rental.GetForUser(ctx, req.UserID, id); err != nil {
			return Conversation{}, err
		}
	}
	used, over, err := s.OverBudget(ctx, req.UserID, st.DailyTokenLimit)
	if err != nil {
		return Conversation{}, err
	}
	if over {
		return Conversation{}, apperr.ErrButlerQuota
	}
	if err := s.acquire(req.UserID); err != nil {
		return Conversation{}, err
	}
	defer s.release(req.UserID)

	now := s.d.Clock.Now()
	token := randx.Token()
	var conv Conversation
	var userMsg Message
	err = s.withTx(ctx, func(tx pgx.Tx) error {
		var err error
		if req.ConversationID == "" || req.ConversationID == "new" {
			conv, err = s.insertConversation(ctx, tx, req.UserID, title(text), s.d.Provider.Name(), s.d.Provider.Model(), now)
		} else {
			conv, err = s.lockConversation(ctx, tx, req.UserID, req.ConversationID)
		}
		if err != nil {
			return err
		}
		switch conv.Status {
		case ConvRunning:
			return apperr.ErrButlerBusy
		case ConvAwaitingConfirm:
			// 用户没点确认就发了新消息：pending 作废，结果回填「用户未确认」
			tc, err := s.pendingCall(ctx, tx, conv.ID, true)
			if err == nil {
				err = s.expireCall(ctx, tx, tc, msgUnconfirmed, now)
			}
			if err != nil && err != errNoPending {
				return err
			}
		}
		if conv.UserTurns >= st.MaxTurns {
			return apperr.ErrButlerTurnLimit
		}
		blocks := []Block{{Type: BlockText, Text: text}}
		if req.Context.InstanceID != "" {
			blocks = append(blocks, Block{Type: BlockContext, InstanceID: req.Context.InstanceID})
		}
		if userMsg, err = s.appendMessage(ctx, tx, conv.ID, RoleUser, blocks, llm.EstimateTokens(text), now); err != nil {
			return err
		}
		ctxJSON, err := json.Marshal(req.Context)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE butler_conversations SET status = 'running', run_token = $2, run_started_at = $3,
			user_turns = user_turns + 1, context = $4, updated_at = $3 WHERE id = $1`, conv.ID, token, now, ctxJSON)
		return err
	})
	if err != nil {
		return Conversation{}, err
	}
	s.log.Info("butler: send", "conversation", conv.ID, "user", req.UserID, "chars", utf8.RuneCountInString(text))
	sink.Emit(EvStart, StartEvent{ConversationID: conv.ID, MessageID: userMsg.ID})

	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.runTimeout)
	defer cancel()
	s.runLoop(runCtx, &run{userID: req.UserID, convID: conv.ID, instanceID: req.Context.InstanceID, token: token, ip: req.IP,
		sink: sink, used: used, limit: st.DailyTokenLimit, secrets: map[string]string{}})
	return s.finalConv(ctx, req.UserID, conv.ID)
}

// finalConv 读回轮次结束后的对话（请求 ctx 可能已断开，用不随它取消的 ctx）。
func (s *Service) finalConv(ctx context.Context, userID int64, id string) (Conversation, error) {
	c, cancel := dbctx(ctx)
	defer cancel()
	return s.getConv(c, s.pool, userID, id)
}

// run 是一轮（Send 或 Confirm 之后的续跑）的状态。secrets：本轮 generate_secret 的明文结果（tool_call_id → 内容），
// 只在内存里替换历史中落库的 ***，让模型能在同一轮里用它部署。secretVals：本轮出现过的密钥明文（生成的密钥、
// 确认卡 / 执行时的 secret 变量值），落库的 assistant 文本里把它们换成 ***（已流式推出的 delta 不变）。
type run struct {
	userID     int64
	convID     string
	instanceID string
	token      string
	ip         string
	sink       Sink
	used       int64 // 今日已用（轮开始时 + 本轮累加）
	limit      int64
	secrets    map[string]string
	secretVals []string
}

// addSecret 记下一个需要从落库文本里抹掉的密钥明文（过短的值不记，避免误伤普通文字）。
func (r *run) addSecret(v string) {
	if len(v) >= 6 && !slices.Contains(r.secretVals, v) {
		r.secretVals = append(r.secretVals, v)
	}
}

// mask 把文本里出现的本轮密钥明文换成 ***（长的先换）。
func (r *run) mask(text string) string {
	if len(r.secretVals) == 0 || text == "" {
		return text
	}
	vals := append([]string(nil), r.secretVals...)
	sort.Slice(vals, func(i, j int) bool { return len(vals[i]) > len(vals[j]) })
	for _, v := range vals {
		text = strings.ReplaceAll(text, v, tools.SecretMask)
	}
	return text
}

type chatOut struct {
	text  string
	calls []llm.ToolCall
	usage *llm.Usage
	stop  string
}

// runLoop 是工具循环（规格 §5.2 第 4 步）：至多 MaxIterations 次模型请求；每次回写都比对 run_token，不匹配即放弃本轮。
// 整轮超时（RunTimeout）→ 固定文案「这次处理时间太长，已停止，请重试」结束（不计 LLM 失败）；panic → 内部错误结束。
// 续跑（Confirm 之后）不再检查每日预算：预算只在轮开始（Send）时判断，一轮内允许超出。
func (s *Service) runLoop(ctx context.Context, r *run) {
	defer func() {
		if p := recover(); p != nil {
			s.internalFail(ctx, r, fmt.Errorf("panic: %v\n%s", p, debug.Stack()))
		}
	}()
	for iter := 0; iter < MaxIterations; iter++ {
		if ctx.Err() != nil {
			s.finishText(ctx, r, "", TextRunTimeout, 0, 0, DoneEndTurn)
			return
		}
		hist, err := s.loadHistory(ctx, r.convID, r.secrets)
		if err != nil {
			s.internalFail(ctx, r, err)
			return
		}
		req := llm.Request{
			System:        prompt.Fixed(),
			SystemDynamic: prompt.Dynamic(s.snapshot(ctx, r.userID, r.instanceID)),
			Messages:      hist,
			MaxTokens:     s.d.MaxOutputTokens,
		}
		if s.toolsOn() {
			req.Tools = s.d.Tools.Defs()
		}
		out, err := s.chat(ctx, r, req)
		if err != nil {
			if ctx.Err() != nil { // 整轮超时（不是模型单次请求超时）：不算模型失败
				s.finishText(ctx, r, out.text, TextRunTimeout, 0, 0, DoneEndTurn)
				return
			}
			s.llmFailed(ctx, r, err)
			return
		}
		s.llmSucceeded()
		in, outTok := s.account(ctx, r, req, out)
		switch {
		case out.stop == llm.StopRefusal:
			s.finishText(ctx, r, out.text, TextRefusal, in, outTok, DoneRefusal)
			return
		case len(out.calls) == 0:
			reason := DoneEndTurn
			if out.stop == llm.StopMaxTokens {
				reason = DoneMaxTokens
			}
			s.finishText(ctx, r, out.text, "", in, outTok, reason)
			return
		}
		if !s.handleTools(ctx, r, out, in, outTok) {
			return
		}
	}
	s.finishText(ctx, r, "", TextIterationLimit, 0, 0, DoneIterationLimit)
}

// chat 调一次模型（LLMTimeout）；文本增量即时推 delta。
func (s *Service) chat(ctx context.Context, r *run, req llm.Request) (chatOut, error) {
	cctx, cancel := context.WithTimeout(ctx, LLMTimeout)
	defer cancel()
	var out chatOut
	var b strings.Builder
	err := s.d.Provider.Chat(cctx, req, func(e llm.Event) {
		switch e.Type {
		case llm.EvText:
			if e.Text != "" {
				b.WriteString(e.Text)
				r.sink.Emit(EvDelta, DeltaEvent{Text: e.Text})
			}
		case llm.EvTool:
			if e.ToolCall != nil {
				out.calls = append(out.calls, *e.ToolCall)
			}
		case llm.EvUsage:
			if e.Usage != nil {
				u := *e.Usage
				if out.usage != nil {
					u.InputTokens += out.usage.InputTokens
					u.OutputTokens += out.usage.OutputTokens
				}
				out.usage = &u
			}
		case llm.EvDone:
			out.stop = e.StopReason
		}
	})
	out.text = b.String()
	return out, err
}

// account 记一次成功请求的用量（butler_usage 直接累加，不随 run_token 丢弃——模型确实被调用了）并推 usage 事件。
// 提供方没报用量时按 EstimateTokens 估算。
func (s *Service) account(ctx context.Context, r *run, req llm.Request, out chatOut) (in, outTok int64) {
	if out.usage != nil {
		in, outTok = out.usage.InputTokens, out.usage.OutputTokens
	} else {
		n := llm.EstimateTokens(req.System + req.SystemDynamic)
		for _, m := range req.Messages {
			n += msgTokens(Message{Content: m.Blocks})
		}
		in = int64(n)
		var b strings.Builder
		b.WriteString(out.text)
		for _, c := range out.calls {
			b.Write(c.Args)
		}
		outTok = int64(llm.EstimateTokens(b.String()))
	}
	wctx, cancel := dbctx(ctx)
	defer cancel()
	if err := s.AddUsage(wctx, s.pool, r.userID, s.DayOf(s.d.Clock.Now()), Usage{Requests: 1, InputTokens: in, OutputTokens: outTok}); err != nil {
		s.log.Error("butler: add usage", "conversation", r.convID, "err", err)
	}
	r.used += in + outTok
	r.sink.Emit(EvUsage, UsageEvent{InputTokens: in, OutputTokens: outTok, DayUsed: r.used, DayLimit: r.limit})
	return in, outTok
}

// writeback 在短事务里执行 fn，前提是对话的 run_token 仍是本轮的（同时刷新 updated_at 并锁住对话行）；
// 不匹配 → errStaleRun（记一条日志），fn 不执行。
func (s *Service) writeback(ctx context.Context, r *run, fn func(ctx context.Context, tx pgx.Tx, now time.Time) error) error {
	wctx, cancel := dbctx(ctx)
	defer cancel()
	now := s.d.Clock.Now()
	err := s.withTx(wctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(wctx, `UPDATE butler_conversations SET updated_at = $3 WHERE id = $1 AND run_token = $2`, r.convID, r.token, now)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errStaleRun
		}
		return fn(wctx, tx, now)
	})
	if errors.Is(err, errStaleRun) {
		s.log.Warn("butler: stale run discarded", "conversation", r.convID)
	}
	return err
}

// setStatus 把对话置为 idle / awaiting_confirm 并清空 run_token。
func setStatus(ctx context.Context, tx pgx.Tx, convID, status string, now time.Time) error {
	_, err := tx.Exec(ctx, `UPDATE butler_conversations SET status = $2, run_token = '', run_started_at = NULL, updated_at = $3
		WHERE id = $1`, convID, status, now)
	return err
}

func addConvTokens(ctx context.Context, tx pgx.Tx, convID string, in, out int64) error {
	if in == 0 && out == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `UPDATE butler_conversations SET input_tokens = input_tokens + $2, output_tokens = output_tokens + $3
		WHERE id = $1`, convID, in, out)
	return err
}

// finishText 以一条 assistant 文本结束本轮：text 是已流式推出的部分，fixed 是追加的固定文案（落库并补推 delta）。
func (s *Service) finishText(ctx context.Context, r *run, text, fixed string, in, outTok int64, reason string) {
	full, extra := text, ""
	if fixed != "" {
		if full != "" {
			extra = "\n\n"
		}
		extra += fixed
		full += extra
	}
	stored := r.mask(full)
	err := s.writeback(ctx, r, func(ctx context.Context, tx pgx.Tx, now time.Time) error {
		if err := addConvTokens(ctx, tx, r.convID, in, outTok); err != nil {
			return err
		}
		if stored != "" {
			if _, err := s.appendMessage(ctx, tx, r.convID, RoleAssistant, []Block{{Type: BlockText, Text: stored}}, int(outTok), now); err != nil {
				return err
			}
		}
		return setStatus(ctx, tx, r.convID, ConvIdle, now)
	})
	if err != nil {
		if !errors.Is(err, errStaleRun) {
			s.internalFail(ctx, r, err)
		}
		return
	}
	if extra != "" {
		r.sink.Emit(EvDelta, DeltaEvent{Text: extra})
	}
	r.sink.Emit(EvDone, DoneEvent{Reason: reason})
}

// llmFailed：模型不可用 / 请求错误 / 超时（D16）——failures + 1、不计预算、对话回到 idle（已落库的 user 消息保留）、推 error 事件。
func (s *Service) llmFailed(ctx context.Context, r *run, err error) {
	var reqErr *llm.RequestError
	if errors.As(err, &reqErr) {
		s.log.Error("butler: llm request error", "conversation", r.convID, "provider", s.d.Provider.Name(), "err", err)
	} else {
		s.log.Warn("butler: llm unavailable", "conversation", r.convID, "provider", s.d.Provider.Name(), "err", err)
	}
	wctx, cancel := dbctx(ctx)
	defer cancel()
	if e := s.AddUsage(wctx, s.pool, r.userID, s.DayOf(s.d.Clock.Now()), Usage{Requests: 1, Failures: 1}); e != nil {
		s.log.Error("butler: add usage", "conversation", r.convID, "err", e)
	}
	s.noteFailure(wctx)
	if e := s.writeback(ctx, r, func(ctx context.Context, tx pgx.Tx, now time.Time) error {
		return setStatus(ctx, tx, r.convID, ConvIdle, now)
	}); e != nil {
		if !errors.Is(e, errStaleRun) {
			s.log.Error("butler: reset after llm failure", "conversation", r.convID, "err", e)
		}
		return
	}
	e := apperr.ErrButlerLLM
	r.sink.Emit(EvError, ErrorEvent{Code: e.Code, Message: e.Message})
}

// internalFail：本轮的内部错误（数据库等）——记日志、尽力回到 idle、推 error 事件。
func (s *Service) internalFail(ctx context.Context, r *run, err error) {
	s.log.Error("butler: run failed", "conversation", r.convID, "err", err)
	if e := s.writeback(ctx, r, func(ctx context.Context, tx pgx.Tx, now time.Time) error {
		return setStatus(ctx, tx, r.convID, ConvIdle, now)
	}); e != nil {
		if !errors.Is(e, errStaleRun) {
			s.log.Error("butler: reset after run failure", "conversation", r.convID, "err", e)
		}
		return
	}
	r.sink.Emit(EvError, ErrorEvent{Code: "internal", Message: "服务器内部错误，请稍后重试"})
}

// noteFailure 累计连续失败；达到 LLMFailAlertAfter 起每次都 RaiseAlertOnce（同文案未处理时去重，处理后仍失败会再告警）。
func (s *Service) noteFailure(ctx context.Context) {
	s.failMu.Lock()
	s.failStreak++
	n := s.failStreak
	s.failMu.Unlock()
	if n < LLMFailAlertAfter || s.d.Fleet == nil {
		return
	}
	msg := fmt.Sprintf("AI 管家模型连续 %d 次调用失败：%s/%s", LLMFailAlertAfter, s.d.Provider.Name(), s.d.Provider.Model())
	if err := s.d.Fleet.RaiseAlertOnce(ctx, s.pool, fleet.Alert{Level: "warning", Kind: AlertLLMDown, Message: msg}); err != nil {
		s.log.Error("butler: raise alert", "err", err)
	}
}

func (s *Service) llmSucceeded() {
	s.failMu.Lock()
	s.failStreak = 0
	s.failMu.Unlock()
}

// ---- 工具调用 ----

// pendingCard 是本轮生成的确认卡：row 落库（summary 列为 *** 版），real 是含真实 secret 的文案（只进 SSE confirm 事件）。
type pendingCard struct {
	row  ToolCall
	real string
}

// isInvalidJSON：适配器把非法 JSON 参数替换为 {"_invalid_json": "..."}。
func isInvalidJSON(args json.RawMessage) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(args, &m); err != nil {
		return true
	}
	_, ok := m["_invalid_json"]
	return ok && len(m) == 1
}

// maskEnvArgs：写工具没生成确认卡时，落库的 tool_use 参数里 env / app_env 的值一律换成 ***（其中可能有生成的密钥）。
func maskEnvArgs(args json.RawMessage) json.RawMessage {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(args, &m); err != nil {
		return json.RawMessage(`{}`)
	}
	if _, ok := m["_invalid_json"]; ok {
		return json.RawMessage(`{"_invalid_json":"***"}`)
	}
	for _, f := range []string{"env", "app_env"} {
		raw, ok := m[f]
		if !ok {
			continue
		}
		var e map[string]json.RawMessage
		if json.Unmarshal(raw, &e) != nil {
			m[f] = json.RawMessage(`"***"`)
			continue
		}
		for k := range e {
			e[k] = json.RawMessage(`"***"`)
		}
		b, _ := json.Marshal(e)
		m[f] = b
	}
	b, err := json.Marshal(m)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

func validJSONOr(args json.RawMessage, def string) json.RawMessage {
	if len(args) == 0 || !json.Valid(args) {
		return json.RawMessage(def)
	}
	return args
}

// handleTools 处理一次模型回复里的全部工具调用并回写（assistant 消息 + tool_calls 行 + tool 消息）：
// 读工具直接执行；写工具只生成确认卡（同一轮至多 1 张，其余回填 one_write_at_a_time）。
// 返回 true 表示继续循环；生成了确认卡或回写失败时返回 false（本轮结束）。
func (s *Service) handleTools(ctx context.Context, r *run, out chatOut, in, outTok int64) bool {
	now := s.d.Clock.Now()
	caller := tools.Caller{UserID: r.userID, IP: r.ip, Now: now}
	var uses, results []Block
	var rows []ToolCall
	var events []ToolEvent
	var card *pendingCard
	executed := 0
	for _, c := range out.calls {
		tcID := randx.ID("tc-", 12)
		use := Block{Type: BlockToolUse, ToolCallID: tcID, ProviderCallID: c.ProviderCallID, Name: c.Name, Args: validJSONOr(c.Args, `{}`)}
		res := Block{Type: BlockToolResult, ToolCallID: tcID, ProviderCallID: c.ProviderCallID, Name: c.Name}
		ev := ToolEvent{ID: tcID, Name: c.Name, Label: tools.Label(c.Name), Status: "ok"}
		fail := func(content string) {
			res.OK, res.Content = false, content
			ev.Status = "error"
			ev.Message = errMessage(content)
		}
		var t tools.Tool
		known := false
		if s.toolsOn() { // SPARK_LLM_TOOLS=off：模型仍发来的工具调用一律按未知工具回填
			t, known = s.d.Tools.Get(c.Name)
		}
		var norm json.RawMessage
		var verr error
		switch {
		case !known:
			fail(errContent("unknown_tool", "未知工具 "+c.Name))
		case isInvalidJSON(c.Args):
			fail(errContent("invalid_args", "参数不是合法 JSON"))
		default:
			if norm, verr = s.d.Tools.Validate(c.Name, c.Args); verr != nil {
				fail(tools.ErrorResult(verr).Content)
			}
		}
		failed := res.Content != ""

		switch {
		case known && t.Kind == tools.Read:
			row := ToolCall{ID: tcID, ConversationID: r.convID, UserID: r.userID, ProviderCallID: c.ProviderCallID, Tool: c.Name,
				Kind: KindRead, Args: use.Args, Status: CallExecuted, ExecutedAt: &now, CreatedAt: now}
			if !failed {
				row.Args = norm
				rctx, cancel := context.WithTimeout(ctx, ReadToolTimeout)
				got, err := t.Exec(rctx, caller, norm)
				cancel()
				if err != nil {
					if !tools.IsBusinessError(err) {
						s.log.Warn("butler: read tool failed", "conversation", r.convID, "tool", c.Name, "err", err)
					}
					fail(tools.ErrorResult(err).Content)
				} else {
					executed++
					res.OK, res.Content, row.ResultRef = true, got.Content, got.Ref
					if got.Secret {
						r.secrets[tcID] = got.Content
						r.addSecret(secretValue(got.Content))
						res.Content = secretStored
					}
				}
			}
			if !res.OK {
				row.Status, row.Result = CallFailed, json.RawMessage(res.Content)
			}
			rows = append(rows, row)
		case known && t.Kind == tools.Write:
			if !failed && card != nil {
				fail(errContent("one_write_at_a_time", "一次只能提议一个需要确认的操作"))
			}
			if res.Content == "" {
				sctx, cancel := context.WithTimeout(ctx, ReadToolTimeout)
				cd, err := t.Summary(sctx, caller, norm)
				cancel()
				var enc []byte
				if err == nil && len(cd.SecretEnv) > 0 {
					enc, err = tools.SealCard(s.d.Box, cd.SecretEnv, cd.Summary)
				}
				if err != nil {
					if !tools.IsBusinessError(err) {
						s.log.Warn("butler: write tool summary failed", "conversation", r.convID, "tool", c.Name, "err", err)
					}
					fail(tools.ErrorResult(err).Content)
				} else {
					for _, v := range cd.SecretEnv {
						r.addSecret(v)
					}
					args := cd.ArgsStored
					if len(args) == 0 || !json.Valid(args) {
						args = norm
					}
					stored := cd.StoredSummary
					if stored == "" {
						stored = cd.Summary
					}
					exp := now.Add(ConfirmTTL)
					use.Args = args
					card = &pendingCard{real: cd.Summary, row: ToolCall{ID: tcID, ConversationID: r.convID, UserID: r.userID,
						ProviderCallID: c.ProviderCallID, Tool: c.Name, Kind: KindWrite, Args: args, Summary: stored,
						QuoteAmount: cd.Quote, Status: CallPending, SecretEnvEnc: enc, ExpiresAt: &exp, CreatedAt: now}}
				}
			}
			if res.Content != "" {
				// 没生成确认卡的写提议也落一行 failed（参数脱敏），后台轨迹与写操作数里能看到
				use.Args = maskEnvArgs(use.Args)
				rows = append(rows, ToolCall{ID: tcID, ConversationID: r.convID, UserID: r.userID, ProviderCallID: c.ProviderCallID,
					Tool: c.Name, Kind: KindWrite, Args: use.Args, Status: CallFailed, Result: json.RawMessage(res.Content), CreatedAt: now})
			}
		}
		uses = append(uses, use)
		if res.Content != "" {
			results = append(results, res)
			events = append(events, ev)
		}
	}
	if text := r.mask(out.text); text != "" {
		uses = append([]Block{{Type: BlockText, Text: text}}, uses...)
	}

	err := s.writeback(ctx, r, func(ctx context.Context, tx pgx.Tx, now time.Time) error {
		if err := addConvTokens(ctx, tx, r.convID, in, outTok); err != nil {
			return err
		}
		am, err := s.appendMessage(ctx, tx, r.convID, RoleAssistant, uses, int(outTok), now)
		if err != nil {
			return err
		}
		if card != nil {
			rows = append(rows, card.row)
		}
		for _, row := range rows {
			row.MessageID = am.ID
			if err := s.insertToolCall(ctx, tx, row); err != nil {
				return err
			}
		}
		if len(results) > 0 {
			tok := 0
			for _, b := range results {
				tok += llm.EstimateTokens(b.Content)
			}
			if _, err := s.appendMessage(ctx, tx, r.convID, RoleTool, results, tok, now); err != nil {
				return err
			}
		}
		if card != nil {
			return setStatus(ctx, tx, r.convID, ConvAwaitingConfirm, now)
		}
		return nil
	})
	if executed > 0 { // 读工具确实执行了：计数不随 run_token 丢弃
		wctx, cancel := dbctx(ctx)
		if e := s.AddUsage(wctx, s.pool, r.userID, s.DayOf(now), Usage{ToolCalls: executed}); e != nil {
			s.log.Error("butler: add usage", "conversation", r.convID, "err", e)
		}
		cancel()
	}
	if err != nil {
		if !errors.Is(err, errStaleRun) {
			s.internalFail(ctx, r, err)
		}
		return false
	}
	for _, ev := range events { // 提交之后再推，前端看到的步骤都已落库
		r.sink.Emit(EvTool, ev)
	}
	if card != nil {
		row := card.row
		row.Summary = card.real
		r.sink.Emit(EvConfirm, ConfirmEvent{ToolCall: s.ConfirmViewOf(row)})
		r.sink.Emit(EvDone, DoneEvent{Reason: DoneAwaitingConfirm})
		return false
	}
	return true
}

// secretValue 取 generate_secret 结果里的 secret 值。
func secretValue(content string) string {
	var m struct {
		Secret string `json:"secret"`
	}
	_ = json.Unmarshal([]byte(StripWrapper(content)), &m)
	return m.Secret
}

// toolsOn：开放工具（SPARK_LLM_TOOLS=on 且有注册表）。
func (s *Service) toolsOn() bool { return s.d.ToolsEnabled && s.d.Tools != nil }

// errMessage 取错误结果 JSON 的 message（取不到返回空）。
func errMessage(content string) string {
	var m struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal([]byte(StripWrapper(content)), &m)
	return m.Message
}
