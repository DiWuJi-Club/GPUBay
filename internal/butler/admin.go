package butler

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/paging"
)

// ConvFilter 是后台对话列表的筛选：UserID = 0 不限；From / To 为 updated_at 的 [From, To) 区间（nil 不限）。
type ConvFilter struct {
	UserID int64
	From   *time.Time
	To     *time.Time
}

// AdminConvRow 是后台对话列表的一行。Phone 为完整手机号（脱敏由展示层做）；Messages = 消息条数；WriteCalls = 写工具调用数（任意状态）。
type AdminConvRow struct {
	Conversation
	Phone      string
	Messages   int
	WriteCalls int
}

// AdminListConversations 返回后台对话列表（updated_at 降序）与总数。
func (s *Service) AdminListConversations(ctx context.Context, f ConvFilter, p paging.Page) ([]AdminConvRow, int, error) {
	p = p.Normalize()
	var w db.Where
	if f.UserID != 0 {
		w.Add("c.user_id = ?", f.UserID)
	}
	if f.From != nil {
		w.Add("c.updated_at >= ?", *f.From)
	}
	if f.To != nil {
		w.Add("c.updated_at < ?", *f.To)
	}
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM butler_conversations c WHERE `+w.SQL(), w.Args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args := append(append([]any(nil), w.Args...), p.Limit(), p.Offset())
	n := len(w.Args)
	rows, err := s.pool.Query(ctx, `SELECT c.id, c.user_id, c.title, c.status, c.run_token, c.run_started_at, c.context, c.provider, c.model,
		c.user_turns, c.input_tokens, c.output_tokens, c.last_message_at, c.created_at, c.updated_at, u.phone,
		(SELECT count(*) FROM butler_messages m WHERE m.conversation_id = c.id),
		(SELECT count(*) FROM butler_tool_calls t WHERE t.conversation_id = c.id AND t.kind = 'write')
		FROM butler_conversations c JOIN users u ON u.id = c.user_id
		WHERE `+w.SQL()+` ORDER BY c.updated_at DESC, c.id LIMIT $`+strconv.Itoa(n+1)+` OFFSET $`+strconv.Itoa(n+2), args...)
	if err != nil {
		return nil, 0, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (AdminConvRow, error) {
		var x AdminConvRow
		var ctxJSON []byte
		c := &x.Conversation
		err := r.Scan(&c.ID, &c.UserID, &c.Title, &c.Status, &c.RunToken, &c.RunStartedAt, &ctxJSON, &c.Provider, &c.Model,
			&c.UserTurns, &c.InputTokens, &c.OutputTokens, &c.LastMessageAt, &c.CreatedAt, &c.UpdatedAt, &x.Phone, &x.Messages, &x.WriteCalls)
		if err == nil && len(ctxJSON) > 0 {
			_ = json.Unmarshal(ctxJSON, &c.Context)
		}
		return x, err
	})
	if err != nil {
		return nil, 0, err
	}
	if out == nil {
		out = []AdminConvRow{}
	}
	return out, total, nil
}

// AdminConversation 返回任意对话（后台只读）、全部消息与全部工具调用（不含 secret_env_enc）；不存在 → not_found。
func (s *Service) AdminConversation(ctx context.Context, id string) (Conversation, []Message, []ToolCall, error) {
	conv, err := s.getConv(ctx, s.pool, 0, id)
	if err != nil {
		return Conversation{}, nil, nil, err
	}
	msgs, err := s.messages(ctx, s.pool, id)
	if err != nil {
		return Conversation{}, nil, nil, err
	}
	calls, err := s.toolCalls(ctx, id)
	if err != nil {
		return Conversation{}, nil, nil, err
	}
	return conv, msgs, calls, nil
}

// AdminConvRowOf 返回单个对话的后台列表行（详情页头部用）；不存在 → not_found。
func (s *Service) AdminConvRowOf(ctx context.Context, id string) (AdminConvRow, error) {
	conv, err := s.getConv(ctx, s.pool, 0, id)
	if err != nil {
		return AdminConvRow{}, err
	}
	x := AdminConvRow{Conversation: conv}
	err = s.pool.QueryRow(ctx, `SELECT (SELECT phone FROM users WHERE id = $2),
		(SELECT count(*) FROM butler_messages WHERE conversation_id = $1),
		(SELECT count(*) FROM butler_tool_calls WHERE conversation_id = $1 AND kind = 'write')`, id, conv.UserID).
		Scan(&x.Phone, &x.Messages, &x.WriteCalls)
	return x, err
}

// Counts 返回 day 所在东八区自然日的全站请求数、token（input + output）与失败数（仪表盘）。
func (s *Service) Counts(ctx context.Context, day time.Time) (requests int, tokens int64, failures int, err error) {
	err = s.pool.QueryRow(ctx, `SELECT coalesce(sum(requests), 0), coalesce(sum(input_tokens + output_tokens), 0), coalesce(sum(failures), 0)
		FROM butler_usage WHERE day = $1::date`, dateParam(s.DayOf(day))).Scan(&requests, &tokens, &failures)
	return
}
