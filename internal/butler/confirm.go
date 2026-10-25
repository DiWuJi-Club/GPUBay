package butler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/butler/llm"
	"spark-cloud/internal/butler/tools"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/randx"
)

// Confirm 处理确认卡（规格 §5.3）：approve 执行写工具（幂等键 "butler:" + tc.ID），reject 回填「用户拒绝」；然后续跑工具循环让模型收尾。
// 非 awaiting_confirm / 调用非 pending / 已过期 / 不属于该对话 → butler_confirm_invalid（过期时顺带标 expired、对话回 idle）；
// 他人 / 不存在的对话 → not_found。错误都在任何 Emit 之前返回。
func (s *Service) Confirm(ctx context.Context, userID int64, ip, convID, tcID string, approve bool, sink Sink) (Conversation, error) {
	if sink == nil {
		sink = NopSink{}
	}
	st, err := s.Settings(ctx)
	if err != nil {
		return Conversation{}, err
	}
	if !st.Enabled {
		return Conversation{}, disabledErr(st.Notice)
	}
	// 只读预检：明显无效（他人 / 不存在 / 已不是待确认）的请求不占运行名额
	var convStatus, callStatus string
	err = s.pool.QueryRow(ctx, `SELECT c.status, coalesce(t.status, '') FROM butler_conversations c
		LEFT JOIN butler_tool_calls t ON t.id = $3 AND t.conversation_id = c.id
		WHERE c.id = $1 AND c.user_id = $2`, convID, userID, tcID).Scan(&convStatus, &callStatus)
	if db.IsNoRows(err) {
		return Conversation{}, apperr.ErrNotFound
	}
	if err != nil {
		return Conversation{}, err
	}
	if convStatus != ConvAwaitingConfirm || callStatus != CallPending {
		return Conversation{}, apperr.ErrButlerConfirmInvalid
	}
	if err := s.acquire(userID); err != nil {
		return Conversation{}, err
	}
	defer s.release(userID)

	now := s.d.Clock.Now()
	token := randx.Token()
	var conv Conversation
	var tc ToolCall
	expired := false
	err = s.withTx(ctx, func(tx pgx.Tx) error {
		var err error
		if conv, err = s.lockConversation(ctx, tx, userID, convID); err != nil {
			return err
		}
		tc, err = scanCall(tx.QueryRow(ctx, `SELECT `+callCols+` FROM butler_tool_calls WHERE id = $1 AND conversation_id = $2 FOR UPDATE`, tcID, convID))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return apperr.ErrButlerConfirmInvalid
			}
			return err
		}
		if conv.Status != ConvAwaitingConfirm || tc.Status != CallPending {
			return apperr.ErrButlerConfirmInvalid
		}
		if tc.ExpiresAt == nil || !now.Before(*tc.ExpiresAt) {
			expired = true
			if err := s.expireCall(ctx, tx, tc, msgExpired, now); err != nil {
				return err
			}
			return setStatus(ctx, tx, conv.ID, ConvIdle, now)
		}
		status := CallApproved
		if !approve {
			status = CallRejected
		}
		if _, err := tx.Exec(ctx, `UPDATE butler_tool_calls SET status = $2, decided_at = $3,
			secret_env_enc = CASE WHEN $2 = 'rejected' THEN NULL ELSE secret_env_enc END WHERE id = $1`, tc.ID, status, now); err != nil {
			return err
		}
		if !approve {
			res := Block{Type: BlockToolResult, ToolCallID: tc.ID, ProviderCallID: tc.ProviderCallID, Name: tc.Tool, OK: false,
				Content: errContent("rejected", msgRejected)}
			if _, err := s.appendMessage(ctx, tx, conv.ID, RoleTool, []Block{res}, llm.EstimateTokens(res.Content), now); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `UPDATE butler_conversations SET status = 'running', run_token = $2, run_started_at = $3, updated_at = $3
			WHERE id = $1`, conv.ID, token, now)
		return err
	})
	if err != nil {
		return Conversation{}, err
	}
	if expired {
		return Conversation{}, apperr.ErrButlerConfirmInvalid
	}
	used, _, err := s.OverBudget(ctx, userID, st.DailyTokenLimit)
	if err != nil {
		s.log.Warn("butler: usage today", "user", userID, "err", err)
	}
	s.log.Info("butler: confirm", "conversation", conv.ID, "tool_call", tc.ID, "tool", tc.Tool, "approve", approve)
	sink.Emit(EvStart, StartEvent{ConversationID: conv.ID})

	// 续跑不检查每日预算（预算只在 Send 开始时判断，一轮内允许超出）。
	runCtx, cancel := s.runContext(ctx)
	defer cancel()
	r := &run{userID: userID, convID: conv.ID, instanceID: conv.Context.InstanceID, token: token, ip: ip, sink: sink,
		used: used, limit: st.DailyTokenLimit, secrets: map[string]string{}}
	if !approve || s.executeCall(runCtx, r, tc) {
		s.runLoop(runCtx, r)
	}
	return s.finalConv(ctx, userID, conv.ID)
}

// executeCall 执行已批准的写工具并回写结果：tool_calls 行状态 + 审计 butler.tool_execute + 结果 tool 消息在一个事务里无条件提交
// （操作已经发生；清扫器已把它标 interrupted 时以真实结果覆盖），失败重试一次，仍失败则单独更新调用行兜底。
// 返回 false 表示本轮到此为止（run_token 已被接管、内部错误或 panic；panic 时调用定为 interrupted，同清扫器接管）。
func (s *Service) executeCall(ctx context.Context, r *run, tc ToolCall) (cont bool) {
	defer func() {
		if p := recover(); p != nil {
			err := fmt.Errorf("panic: %v\n%s", p, debug.Stack())
			// 尽力而为：与清扫器接管相同地把调用定为「执行结果未知」（清密文、事实 tool 结果、审计 status unknown），
			// 否则它会停在 approved（密文残留、历史缺 tool_result）。真实结果已回写（executed_at 非空）时不动。
			wctx, cancel := dbctx(ctx)
			if e := s.withTx(wctx, func(tx pgx.Tx) error {
				if _, err := s.lockConversation(wctx, tx, 0, r.convID); err != nil {
					return err
				}
				_, err := s.interruptCall(wctx, tx, tc, audit.Actor{UserID: r.userID, IP: r.ip}, s.d.Clock.Now())
				return err
			}); e != nil {
				s.log.Error("butler: mark call interrupted after panic", "conversation", r.convID, "tool_call", tc.ID, "err", e)
			}
			cancel()
			s.internalFail(ctx, r, err)
			cont = false
		}
	}()
	now := s.d.Clock.Now()
	var res tools.Result
	var execErr error
	t, ok := tools.Tool{}, false
	if s.d.Tools != nil {
		t, ok = s.d.Tools.Get(tc.Tool)
	}
	switch {
	case !ok || t.Kind != tools.Write:
		res = tools.ErrorResult(apperr.ErrButlerConfirmInvalid)
	default:
		args, secrets, err := s.openArgs(tc)
		if err != nil {
			s.log.Error("butler: open secret env", "conversation", r.convID, "tool_call", tc.ID, "err", err)
			res = tools.ErrorResult(apperr.ErrButlerConfirmInvalid)
			break
		}
		for _, v := range secrets {
			r.addSecret(v)
		}
		// 已批准的写操作不随整轮超时 / 停机中途取消（半途取消只会留下「执行结果未知」）：只受 WriteToolTimeout 限制
		wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), WriteToolTimeout)
		res, execErr = t.Exec(wctx, tools.Caller{UserID: r.userID, IP: r.ip, IdemKey: "butler:" + tc.ID, Now: now, QuotedCents: tc.QuoteAmount}, args)
		cancel()
		if execErr != nil {
			s.log.Error("butler: write tool failed", "conversation", r.convID, "tool_call", tc.ID, "tool", tc.Tool, "err", execErr)
			res = tools.ErrorResult(execErr)
		}
	}
	status, okRes := CallExecuted, true
	code := ""
	if c, isErr := resultError(res.Content); isErr || execErr != nil {
		status, okRes, code = CallFailed, false, c
	}
	result := resultJSON(res.Content)
	diff := map[string]any{"tool": tc.Tool, "args": validJSONOr(tc.Args, `{}`), "status": status, "result_ref": res.Ref,
		"conversation_id": tc.ConversationID, "error_code": code}
	blk := Block{Type: BlockToolResult, ToolCallID: tc.ID, ProviderCallID: tc.ProviderCallID, Name: tc.Tool, OK: okRes, Content: res.Content}

	// 回写：调用行状态 + 审计 + 事实结果的 tool 消息都无条件提交（操作已经发生）；run_token 不匹配只意味着不再续跑。
	record := func() (stale bool, err error) {
		wctx, cancel := dbctx(ctx)
		defer cancel()
		err = s.withTx(wctx, func(tx pgx.Tx) error {
			if _, err := s.lockConversation(wctx, tx, 0, r.convID); err != nil {
				return err
			}
			tag, err := tx.Exec(wctx, `UPDATE butler_conversations SET updated_at = $3 WHERE id = $1 AND run_token = $2`, r.convID, r.token, now)
			if err != nil {
				return err
			}
			stale = tag.RowsAffected() == 0
			if _, err := tx.Exec(wctx, `UPDATE butler_tool_calls SET status = $2, result = $3, result_ref = $4, executed_at = $5,
				secret_env_enc = NULL WHERE id = $1`, tc.ID, status, []byte(result), res.Ref, now); err != nil {
				return err
			}
			if err := audit.Record(wctx, tx, now, audit.Actor{UserID: r.userID, IP: r.ip}.Entry(AuditToolExecute, AuditTargetCall, tc.ID, diff)); err != nil {
				return err
			}
			_, err = s.appendMessage(wctx, tx, r.convID, RoleTool, []Block{blk}, llm.EstimateTokens(res.Content), now)
			return err
		})
		return stale, err
	}
	stale, err := record()
	if err != nil {
		s.log.Warn("butler: record tool result, retrying", "conversation", r.convID, "tool_call", tc.ID, "err", err)
		stale, err = record()
	}
	if u := s.addUsageNow(ctx, r, Usage{ToolCalls: 1}); u != nil {
		s.log.Error("butler: add usage", "conversation", r.convID, "err", u)
	}
	if err != nil {
		// 兜底：至少把调用行定下来（真实结果）并清掉密文，审计尽力而为；本轮按内部错误结束
		wctx, cancel := dbctx(ctx)
		if _, e := s.pool.Exec(wctx, `UPDATE butler_tool_calls SET status = $2, result = $3, result_ref = $4, executed_at = $5,
			secret_env_enc = NULL WHERE id = $1`, tc.ID, status, []byte(result), res.Ref, now); e != nil {
			s.log.Error("butler: record tool result fallback", "tool_call", tc.ID, "err", e)
		}
		if e := audit.Record(wctx, s.pool, now, audit.Actor{UserID: r.userID, IP: r.ip}.Entry(AuditToolExecute, AuditTargetCall, tc.ID, diff)); e != nil {
			s.log.Error("butler: audit fallback", "tool_call", tc.ID, "err", e)
		}
		cancel()
		s.internalFail(ctx, r, err)
		return false
	}
	if stale {
		s.log.Warn("butler: stale run discarded", "conversation", r.convID)
		return false
	}
	ev := ToolEvent{ID: tc.ID, Name: tc.Tool, Label: tools.Label(tc.Tool), Status: "ok"}
	if !okRes {
		ev.Status, ev.Message = "error", errMessage(res.Content)
	}
	r.sink.Emit(EvTool, ev)
	return true
}

// addUsageNow 直接累加用量（不随 run_token 丢弃）。
func (s *Service) addUsageNow(ctx context.Context, r *run, u Usage) error {
	wctx, cancel := dbctx(ctx)
	defer cancel()
	return s.AddUsage(wctx, s.pool, r.userID, s.DayOf(s.d.Clock.Now()), u)
}

// openArgs 把 secret_env_enc 里的真实值合并回落库的 args（*** 占位），同时返回这些值（用于抹掉后续落库文本里的明文）。
func (s *Service) openArgs(tc ToolCall) (json.RawMessage, map[string]string, error) {
	if len(tc.SecretEnvEnc) == 0 {
		return tc.Args, nil, nil
	}
	secrets, _, err := tools.OpenCard(s.d.Box, tc.SecretEnvEnc)
	if err != nil {
		return nil, nil, err
	}
	args, err := tools.MergeSecretEnv(tc.Args, secrets)
	return args, secrets, err
}

// expireCall 把 pending 调用标 expired（清 secret_env_enc）并追加 tool 消息 {"ok":false,"code":"expired","message":msg}。
// 调用方负责锁住对话行与调整对话状态。
func (s *Service) expireCall(ctx context.Context, tx pgx.Tx, tc ToolCall, msg string, now time.Time) error {
	tag, err := tx.Exec(ctx, `UPDATE butler_tool_calls SET status = 'expired', secret_env_enc = NULL WHERE id = $1 AND status = 'pending'`, tc.ID)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	res := Block{Type: BlockToolResult, ToolCallID: tc.ID, ProviderCallID: tc.ProviderCallID, Name: tc.Tool, OK: false, Content: errContent("expired", msg)}
	_, err = s.appendMessage(ctx, tx, tc.ConversationID, RoleTool, []Block{res}, llm.EstimateTokens(res.Content), now)
	return err
}

// StripWrapper 去掉 guard.JSONWrap 的最外层 <untrusted_data …> 包裹（首行开标签、末行闭标签）；不是包裹格式时原样返回。
func StripWrapper(s string) string {
	if !strings.HasPrefix(s, "<untrusted_data") {
		return s
	}
	i := strings.IndexByte(s, '\n')
	if i < 0 {
		return s
	}
	body := s[i+1:]
	if j := strings.LastIndex(body, "\n</untrusted_data>"); j >= 0 {
		return body[:j]
	}
	return s
}

// resultJSON 是写入 butler_tool_calls.result 的 JSON：去包裹后的结果 JSON；不是合法 JSON（或超 MaxToolResultBytes）时存为 JSON 字符串。
func resultJSON(content string) json.RawMessage {
	b := StripWrapper(content)
	if len(b) <= MaxToolResultBytes && json.Valid([]byte(b)) {
		return json.RawMessage(b)
	}
	if r := []rune(b); len(b) > MaxToolResultBytes {
		n := 0
		for k, ch := range r {
			n += len(string(ch))
			if n > MaxToolResultBytes-64 {
				b = string(r[:k])
				break
			}
		}
	}
	out, _ := json.Marshal(b)
	return out
}

// resultError：结果是 {"ok": false, "code": …} 时返回 code 与 true。
func resultError(content string) (string, bool) {
	var m struct {
		OK   *bool  `json:"ok"`
		Code string `json:"code"`
	}
	if json.Unmarshal([]byte(StripWrapper(content)), &m) != nil || m.OK == nil || *m.OK {
		return "", false
	}
	return m.Code, true
}
