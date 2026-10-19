package referral

import (
	"context"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/platform/db"
)

type BindRequest struct {
	Code      string
	InviteeID int64
	IP        string
	DeviceID  string
	UserAgent string
	Now       time.Time
}

type BindResult struct {
	Bound   bool   `json:"bound"`
	Reason  string `json:"reason"`
	Inviter string `json:"inviter,omitempty"` // 脱敏手机号
}

// SanitizeUA 把任意 User-Agent 变成可安全写入 PG TEXT 的值：丢弃非法 UTF-8 字节与 NUL（两者都会让 INSERT 报 22021
// 并回滚整个注册事务），再截到至多 MaxUserAgent 字节、且不切断多字节字符。
func SanitizeUA(ua string) string {
	ua = strings.ReplaceAll(strings.ToValidUTF8(ua, ""), "\x00", "")
	if len(ua) <= MaxUserAgent {
		return ua
	}
	cut := MaxUserAgent
	for cut > 0 && !utf8.RuneStart(ua[cut]) {
		cut--
	}
	return ua[:cut]
}

const (
	ReasonOK            = "ok"
	ReasonInvalidCode   = "invalid_code"
	ReasonInviterFrozen = "inviter_frozen"
	ReasonExistingUser  = "existing_user"
)

// Bind 在 identity 的注册事务内把新用户绑定到邀请码的主人。业务上不可绑定（码无效 / 码主冻结或封禁 / 已绑定）返回 Bound=false，
// 不返回 error（注册不能被邀请码问题挡住）；数据库错误照常返回让调用方回滚。
func (s *Service) Bind(ctx context.Context, tx pgx.Tx, req BindRequest) (BindResult, error) {
	code, ok := NormalizeCode(req.Code) // 只放行 8 位 ASCII 字母表字符：非法 UTF-8 / 乱码在查库前就落到 invalid_code
	if !ok {
		return BindResult{Reason: ReasonInvalidCode}, nil
	}
	var inviterID int64
	var accStatus, userStatus, phone string
	err := tx.QueryRow(ctx, `SELECT a.user_id, a.status, u.status, u.phone FROM referral_accounts a JOIN users u ON u.id = a.user_id
		WHERE a.code = $1 FOR SHARE OF a`, code).Scan(&inviterID, &accStatus, &userStatus, &phone)
	if db.IsNoRows(err) {
		return BindResult{Reason: ReasonInvalidCode}, nil
	}
	if err != nil {
		return BindResult{}, err
	}
	if inviterID == req.InviteeID {
		return BindResult{Reason: ReasonInvalidCode}, nil
	}
	if accStatus != AccountActive || userStatus != "active" {
		return BindResult{Reason: ReasonInviterFrozen}, nil
	}
	// 已绑定过：先查再插。撞 invitee_id 唯一索引会让 PG 把 identity 的事务置为失败态（之后写 sessions 会报 25P02），
	// 所以唯一索引只作并发兜底（兜底命中时整个登录事务报错回滚）。
	var bound bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM referral_bindings WHERE invitee_id = $1)`, req.InviteeID).Scan(&bound); err != nil {
		return BindResult{}, err
	}
	if bound {
		return BindResult{Reason: ReasonInvalidCode}, nil
	}
	v, err := s.ConfigAt(ctx, tx, req.Now)
	if err != nil {
		return BindResult{}, err
	}
	inviter, err := s.d.Users.UserState(ctx, tx, inviterID)
	if err != nil {
		return BindResult{}, err
	}
	flags := []string{}
	if req.IP != "" && req.IP == inviter.LastLoginIP && inviter.LastLoginAt != nil && inviter.LastLoginAt.After(req.Now.Add(-SameIPWindow)) {
		flags = append(flags, FlagSameIP)
	}
	if req.DeviceID != "" {
		has, err := s.d.Users.HasDevice(ctx, tx, inviterID, req.DeviceID)
		if err != nil {
			return BindResult{}, err
		}
		if has {
			flags = append(flags, FlagSameDevice)
		}
	}
	var recent int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM referral_bindings WHERE inviter_id = $1 AND bound_at > $2`, inviterID, req.Now.Add(-SameIPWindow)).Scan(&recent); err != nil {
		return BindResult{}, err
	}
	if recent >= BurstThreshold-1 {
		flags = append(flags, FlagBurst)
	}
	status, hold := BindingActive, ""
	for _, f := range flags {
		if f == FlagSameDevice {
			status, hold = BindingHeld, FlagSameDevice
		}
	}
	ua := SanitizeUA(req.UserAgent) // UA 只在这里落库；identity 原样透传
	var id int64
	err = tx.QueryRow(ctx, `INSERT INTO referral_bindings (inviter_id, invitee_id, code, status, risk_flags, hold_reason, rate_version_id, bound_at, rebate_until, ip, device_id, user_agent, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8::timestamptz + make_interval(days => $9), $10, $11, $12, $8, $8) RETURNING id`,
		inviterID, req.InviteeID, code, status, flags, hold, v.ID, req.Now, v.PeriodDays, req.IP, req.DeviceID, ua).Scan(&id)
	if err != nil {
		return BindResult{}, err // 含 invitee_id 唯一冲突（并发兜底）：让 identity 回滚整个登录
	}
	if err := audit.Record(ctx, tx, req.Now, audit.Actor{UserID: req.InviteeID, IP: req.IP}.Entry("referral.bind", "referral_binding", strconv.FormatInt(id, 10),
		map[string]any{"inviter_id": inviterID, "code": code, "flags": flags, "status": status})); err != nil {
		return BindResult{}, err
	}
	return BindResult{Bound: true, Reason: ReasonOK, Inviter: MaskPhone(phone)}, nil
}
