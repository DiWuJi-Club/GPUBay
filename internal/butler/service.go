package butler

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/butler/llm"
	"spark-cloud/internal/butler/tools"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/secretbox"
)

// AlertRaiser 是告警出口（*fleet.Service 满足它）；连续 LLM 失败时去重告警 butler_llm_unavailable。
type AlertRaiser interface {
	RaiseAlertOnce(ctx context.Context, q db.Querier, a fleet.Alert) error
}

// Deps 是 Service 的依赖。Users / Wallet / Rental 只用于每轮的用户快照与 context.instance_id 归属校验；
// 工具的业务调用全部在 Tools（tools.Registry）里。Fleet 为 nil 时不告警；Box 用于写工具 secret 变量的加解密。
type Deps struct {
	Pool         *pgxpool.Pool
	Clock        clock.Clock
	Log          *slog.Logger
	Provider     llm.Provider
	Tools        *tools.Registry
	ToolsEnabled bool // SPARK_LLM_TOOLS：false 时不把工具声明发给模型（只答不做）

	Users  tools.UsersAPI
	Wallet tools.WalletAPI
	Rental tools.RentalAPI

	Fleet AlertRaiser

	Box             *secretbox.Box
	MaxOutputTokens int // ≤ 0 → DefaultMaxOutputTokens
}

// Service 是 AI 管家：对话轮次（Send / Confirm）、清扫（Sweep）与后台查询；嵌入 Store（设置、用量）。
type Service struct {
	*Store
	d   Deps
	log *slog.Logger

	mu      sync.Mutex
	running map[int64]struct{} // 本副本正在运行轮次的用户（每用户同时 1 轮）
	sem     chan struct{}      // 本副本全局并发（MaxConcurrentRuns）

	runTimeout time.Duration // 整轮超时（RunTimeout；测试可缩短）

	failMu     sync.Mutex
	failStreak int // 连续 LLM 失败次数（进程内）；成功一次清零
}

func NewService(d Deps) *Service {
	if d.Clock == nil {
		d.Clock = clock.Real{}
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.MaxOutputTokens <= 0 {
		d.MaxOutputTokens = DefaultMaxOutputTokens
	}
	return &Service{
		Store:   NewStore(d.Pool, d.Clock),
		d:       d,
		log:     d.Log,
		running: map[int64]struct{}{},
		sem:     make(chan struct{}, MaxConcurrentRuns),

		runTimeout: RunTimeout,
	}
}

// acquire 占用该用户的运行名额与一个全局名额；用户已有运行中的轮次 → butler_busy，全局已满 → butler_busy（「管家正忙，请稍后」）。
func (s *Service) acquire(userID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.running[userID]; ok {
		return apperr.ErrButlerBusy
	}
	select {
	case s.sem <- struct{}{}:
	default:
		return errGlobalBusy
	}
	s.running[userID] = struct{}{}
	return nil
}

func (s *Service) release(userID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.running[userID]; ok {
		delete(s.running, userID)
		<-s.sem
	}
}

// ProviderLabel 是给用户看的模型来源：anthropic →「Claude」，其它（openai / mock）→「平台模型」，不暴露模型名。
func (s *Service) ProviderLabel() string {
	if s.d.Provider != nil && s.d.Provider.Name() == "anthropic" {
		return "Claude"
	}
	return "平台模型"
}

// StatusView 是 GET /butler/status 的数据（ingress_domain 由 api 层补）。
type StatusView struct {
	Enabled       bool   `json:"enabled"`
	Notice        string `json:"notice"`
	DayUsed       int64  `json:"day_used"`
	DayLimit      int64  `json:"day_limit"`
	ProviderLabel string `json:"provider_label"`
}

// Status 返回总开关、关闭说明、今日已用 / 限额与模型来源。
func (s *Service) Status(ctx context.Context, userID int64) (StatusView, error) {
	st, err := s.Settings(ctx)
	if err != nil {
		return StatusView{}, err
	}
	used, _, err := s.OverBudget(ctx, userID, st.DailyTokenLimit)
	if err != nil {
		return StatusView{}, err
	}
	return StatusView{Enabled: st.Enabled, Notice: st.Notice, DayUsed: used, DayLimit: st.DailyTokenLimit, ProviderLabel: s.ProviderLabel()}, nil
}

// ListConversations 返回用户最近活动的对话（updated_at 降序）；limit ≤ 0 或 > ListLimit 时取 ListLimit。
func (s *Service) ListConversations(ctx context.Context, userID int64, limit int) ([]Conversation, error) {
	if limit <= 0 || limit > ListLimit {
		limit = ListLimit
	}
	rows, err := s.pool.Query(ctx, `SELECT `+convCols+` FROM butler_conversations WHERE user_id = $1
		ORDER BY updated_at DESC, id LIMIT $2`, userID, limit)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Conversation, error) { return scanConv(r) })
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []Conversation{}
	}
	return out, nil
}

// GetConversation 返回用户自己的对话、全部消息与待确认的写调用（没有则 nil）。他人 / 不存在 → not_found。
// 待确认调用的 Summary 是含真实 secret 的确认卡文案（从 secret_env_enc 解密；没有 secret 时就是 summary 列），
// SecretEnvEnc 不随返回值带出。
func (s *Service) GetConversation(ctx context.Context, userID int64, id string) (Conversation, []Message, *ToolCall, error) {
	conv, err := s.getConv(ctx, s.pool, userID, id)
	if err != nil {
		return Conversation{}, nil, nil, err
	}
	msgs, err := s.messages(ctx, s.pool, id)
	if err != nil {
		return Conversation{}, nil, nil, err
	}
	var pending *ToolCall
	if conv.Status == ConvAwaitingConfirm {
		tc, err := s.pendingCall(ctx, s.pool, id, false)
		if err != nil && err != errNoPending {
			return Conversation{}, nil, nil, err
		}
		if err == nil {
			if len(tc.SecretEnvEnc) > 0 {
				if _, summary, err := tools.OpenCard(s.d.Box, tc.SecretEnvEnc); err == nil && summary != "" {
					tc.Summary = summary
				} else if err != nil {
					s.log.Warn("butler: open card failed", "conversation", id, "tool_call", tc.ID, "err", err)
				}
			}
			tc.SecretEnvEnc = nil
			pending = &tc
		}
	}
	return conv, msgs, pending, nil
}

// DeleteConversation 硬删用户自己的对话（级联消息与工具调用）；运行中 → butler_busy，他人 / 不存在 → not_found。
func (s *Service) DeleteConversation(ctx context.Context, userID int64, id string) error {
	return s.withTx(ctx, func(tx pgx.Tx) error {
		conv, err := s.lockConversation(ctx, tx, userID, id)
		if err != nil {
			return err
		}
		if conv.Status == ConvRunning {
			return apperr.ErrButlerBusy
		}
		_, err = tx.Exec(ctx, `DELETE FROM butler_conversations WHERE id = $1 AND user_id = $2`, id, userID)
		return err
	})
}

// ConfirmView 是确认卡（SSE confirm 事件的 tool_call 与 GET 会话详情的 pending_tool_call 同款）。
type ConfirmView struct {
	ID          string          `json:"id"`
	Tool        string          `json:"tool"`
	Label       string          `json:"label"`
	Summary     string          `json:"summary"`
	QuoteAmount *int64          `json:"quote_amount"`
	Args        json.RawMessage `json:"args"`
	ExpiresAt   time.Time       `json:"expires_at"`
	ExpiresIn   int64           `json:"expires_in"` // 按服务端时钟剩余的整秒数（向上取整，≥ 0）；前端倒计时以它为准
	Danger      bool            `json:"danger"`
}

// ConfirmViewOf 把工具调用行转成确认卡视图；Summary 原样取 tc.Summary（GetConversation 返回的 pending 已是真实文案）。
func (s *Service) ConfirmViewOf(tc ToolCall) ConfirmView {
	v := ConfirmView{ID: tc.ID, Tool: tc.Tool, Label: tools.Label(tc.Tool), Summary: tc.Summary, QuoteAmount: tc.QuoteAmount, Args: tc.Args}
	if len(v.Args) == 0 {
		v.Args = json.RawMessage(`{}`)
	}
	if tc.ExpiresAt != nil {
		v.ExpiresAt = *tc.ExpiresAt
		if left := tc.ExpiresAt.Sub(s.d.Clock.Now()); left > 0 {
			v.ExpiresIn = int64((left + time.Second - 1) / time.Second)
		}
	}
	if s.d.Tools != nil {
		if t, ok := s.d.Tools.Get(tc.Tool); ok {
			v.Danger = t.Danger
		}
	}
	return v
}

// dbctx 返回不随轮次 / 请求取消的短超时 ctx，用于轮次结束时的回写（轮次 ctx 超时后仍要把状态写回 idle）。
func dbctx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
}
