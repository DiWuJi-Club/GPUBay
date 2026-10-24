package butler

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/randx"
)

// Store 是管家的数据访问层：用量、设置等基础 SQL；Service 嵌入它。
type Store struct {
	pool *pgxpool.Pool
	clk  clock.Clock

	// 设置的进程内缓存（SettingsCacheTTL）；gen 在失效 / 更新时递增，防止并发读把旧值写回缓存。
	setMu  sync.Mutex
	setVal Settings
	setAt  time.Time
	setOK  bool
	setGen uint64
}

func NewStore(pool *pgxpool.Pool, clk clock.Clock) *Store {
	return &Store{pool: pool, clk: clk}
}

// withTx 在事务中执行 fn；fn 返回错误时回滚并原样返回。
func (s *Store) withTx(ctx context.Context, fn func(pgx.Tx) error) error {
	return db.WithTx(ctx, s.pool, fn)
}

// ---- 对话 / 消息 / 工具调用的 SQL ----

const convCols = `id, user_id, title, status, run_token, run_started_at, context, provider, model, user_turns,
	input_tokens, output_tokens, last_message_at, created_at, updated_at`

func scanConv(r pgx.Row) (Conversation, error) {
	var c Conversation
	var ctxJSON []byte
	err := r.Scan(&c.ID, &c.UserID, &c.Title, &c.Status, &c.RunToken, &c.RunStartedAt, &ctxJSON, &c.Provider, &c.Model,
		&c.UserTurns, &c.InputTokens, &c.OutputTokens, &c.LastMessageAt, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return Conversation{}, err
	}
	if len(ctxJSON) > 0 {
		_ = json.Unmarshal(ctxJSON, &c.Context)
	}
	return c, nil
}

// getConv 读用户自己的对话（userID = 0 表示不限用户，后台用）；不存在 → not_found。
func (s *Store) getConv(ctx context.Context, q db.Querier, userID int64, id string) (Conversation, error) {
	c, err := scanConv(q.QueryRow(ctx, `SELECT `+convCols+` FROM butler_conversations WHERE id = $1 AND ($2 = 0 OR user_id = $2)`, id, userID))
	if db.IsNoRows(err) {
		return Conversation{}, apperr.ErrNotFound
	}
	return c, err
}

// lockConversation 在事务里 FOR UPDATE 锁用户自己的对话行（userID = 0 不限用户，清扫器用）；不存在 → not_found。
func (s *Store) lockConversation(ctx context.Context, tx pgx.Tx, userID int64, id string) (Conversation, error) {
	c, err := scanConv(tx.QueryRow(ctx, `SELECT `+convCols+` FROM butler_conversations WHERE id = $1 AND ($2 = 0 OR user_id = $2) FOR UPDATE`, id, userID))
	if db.IsNoRows(err) {
		return Conversation{}, apperr.ErrNotFound
	}
	return c, err
}

func (s *Store) insertConversation(ctx context.Context, tx pgx.Tx, userID int64, title, provider, model string, now time.Time) (Conversation, error) {
	return scanConv(tx.QueryRow(ctx, `INSERT INTO butler_conversations (id, user_id, title, status, provider, model, created_at, updated_at)
		VALUES ($1, $2, $3, 'idle', $4, $5, $6, $6) RETURNING `+convCols, randx.ID("cv-", 12), userID, title, provider, model, now))
}

const msgCols = `id, conversation_id, seq, role, content, tokens, created_at`

func scanMsg(r pgx.Row) (Message, error) {
	var m Message
	var content []byte
	if err := r.Scan(&m.ID, &m.ConversationID, &m.Seq, &m.Role, &content, &m.Tokens, &m.CreatedAt); err != nil {
		return Message{}, err
	}
	if err := json.Unmarshal(content, &m.Content); err != nil {
		return Message{}, err
	}
	return m, nil
}

func collectMsgs(rows pgx.Rows, err error) ([]Message, error) {
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Message, error) { return scanMsg(r) })
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []Message{}
	}
	return out, nil
}

// messages 返回对话全部消息（seq 升序）。
func (s *Store) messages(ctx context.Context, q db.Querier, convID string) ([]Message, error) {
	return collectMsgs(q.Query(ctx, `SELECT `+msgCols+` FROM butler_messages WHERE conversation_id = $1 ORDER BY seq`, convID))
}

// historyWindow 返回最近 n 条消息 + 对话的第一条 user 消息（seq 升序；trimHistory 再做配对与 token 裁剪）。
func (s *Store) historyWindow(ctx context.Context, convID string, n int) ([]Message, error) {
	return collectMsgs(s.pool.Query(ctx, `SELECT `+msgCols+` FROM butler_messages WHERE conversation_id = $1
		AND (seq > (SELECT coalesce(max(seq), 0) FROM butler_messages WHERE conversation_id = $1) - $2
			OR id = (SELECT id FROM butler_messages WHERE conversation_id = $1 AND role = 'user' ORDER BY seq LIMIT 1))
		ORDER BY seq`, convID, n))
}

// appendMessage 追加一条消息（调用方必须已在同一事务里锁住对话行，seq 才不会冲突），并刷新对话的 last_message_at / updated_at。
func (s *Store) appendMessage(ctx context.Context, tx pgx.Tx, convID, role string, blocks []Block, tokens int, now time.Time) (Message, error) {
	if blocks == nil {
		blocks = []Block{}
	}
	content, err := json.Marshal(blocks)
	if err != nil {
		return Message{}, err
	}
	m, err := scanMsg(tx.QueryRow(ctx, `INSERT INTO butler_messages (conversation_id, seq, role, content, tokens, created_at)
		VALUES ($1, (SELECT coalesce(max(seq), 0) + 1 FROM butler_messages WHERE conversation_id = $1), $2, $3, $4, $5)
		RETURNING `+msgCols, convID, role, content, tokens, now))
	if err != nil {
		return Message{}, err
	}
	_, err = tx.Exec(ctx, `UPDATE butler_conversations SET last_message_at = $2, updated_at = $2 WHERE id = $1`, convID, now)
	return m, err
}

const callCols = `id, conversation_id, message_id, user_id, provider_call_id, tool, kind, args, summary, quote_amount, status,
	result, result_ref, secret_env_enc, expires_at, decided_at, executed_at, created_at`

func scanCall(r pgx.Row) (ToolCall, error) {
	var c ToolCall
	var args, result []byte
	err := r.Scan(&c.ID, &c.ConversationID, &c.MessageID, &c.UserID, &c.ProviderCallID, &c.Tool, &c.Kind, &args, &c.Summary,
		&c.QuoteAmount, &c.Status, &result, &c.ResultRef, &c.SecretEnvEnc, &c.ExpiresAt, &c.DecidedAt, &c.ExecutedAt, &c.CreatedAt)
	if err != nil {
		return ToolCall{}, err
	}
	c.Args = json.RawMessage(args)
	if len(result) > 0 {
		c.Result = json.RawMessage(result)
	}
	return c, nil
}

func (s *Store) insertToolCall(ctx context.Context, tx pgx.Tx, c ToolCall) error {
	args := c.Args
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	var result any
	if len(c.Result) > 0 {
		result = []byte(c.Result)
	}
	_, err := tx.Exec(ctx, `INSERT INTO butler_tool_calls (id, conversation_id, message_id, user_id, provider_call_id, tool, kind, args,
		summary, quote_amount, status, result, result_ref, secret_env_enc, expires_at, decided_at, executed_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)`,
		c.ID, c.ConversationID, c.MessageID, c.UserID, c.ProviderCallID, c.Tool, c.Kind, []byte(args), c.Summary, c.QuoteAmount,
		c.Status, result, c.ResultRef, c.SecretEnvEnc, c.ExpiresAt, c.DecidedAt, c.ExecutedAt, c.CreatedAt)
	return err
}

// pendingCall 返回对话唯一的 pending 调用（lock 时 FOR UPDATE）；没有 → errNoPending。
func (s *Store) pendingCall(ctx context.Context, q db.Querier, convID string, lock bool) (ToolCall, error) {
	sql := `SELECT ` + callCols + ` FROM butler_tool_calls WHERE conversation_id = $1 AND status = 'pending'`
	if lock {
		sql += ` FOR UPDATE`
	}
	c, err := scanCall(q.QueryRow(ctx, sql, convID))
	if db.IsNoRows(err) {
		return ToolCall{}, errNoPending
	}
	return c, err
}

// toolCalls 返回对话的全部工具调用（created_at 升序），不带 secret_env_enc。
func (s *Store) toolCalls(ctx context.Context, convID string) ([]ToolCall, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+callCols+` FROM butler_tool_calls WHERE conversation_id = $1 ORDER BY created_at, id`, convID)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ToolCall, error) {
		c, err := scanCall(r)
		c.SecretEnvEnc = nil
		return c, err
	})
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []ToolCall{}
	}
	return out, nil
}
