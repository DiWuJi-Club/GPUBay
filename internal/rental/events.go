package rental

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/catalog"
)

// UserEvent 用户可见的实例事件（白名单；不透出机器 ID、序列号）。
type UserEvent struct {
	ToState string    `json:"to_state"`
	Kind    string    `json:"reason_kind"`
	At      time.Time `json:"at"`
	Detail  string    `json:"detail"`
}

// userEventOf 把 instance_events.reason 翻译成用户文案；不在白名单返回 ok=false。
func userEventOf(e Event) (UserEvent, bool) {
	r := e.Reason
	out := UserEvent{ToState: e.ToState, At: e.At}
	switch {
	case strings.HasPrefix(r, "order "):
		out.Kind, out.Detail = "order", "下单"
		if i := strings.Index(r, " scheduled "); i >= 0 {
			out.Detail = "预订下单，" + r[i+len(" scheduled "):] + " 开始"
		}
	case strings.HasPrefix(r, "scheduled start "):
		out.Kind, out.Detail = "provision", "开始开通"
	case strings.HasPrefix(r, "agent: created"):
		out.Kind, out.Detail = "created", "开通成功"
		if strings.Contains(r, "late by") {
			out.Detail = "开通成功（晚于预定时间）"
		}
	case strings.HasPrefix(r, "late start compensated"):
		out.Kind, out.Detail = "compensated", "迟开通补偿已退回余额"
	case strings.HasPrefix(r, "resume capped compensated"):
		out.Kind, out.Detail = "compensated", "故障恢复后可用时长不足，补偿已退回余额"
	case strings.HasPrefix(r, "reassigned "):
		out.Kind, out.Detail = "reassigned", "已为您更换机器"
	case strings.HasPrefix(r, "create failed, waiting"):
		out.Kind, out.Detail = "waiting", "开通失败，正在等待可用机器"
	case strings.HasPrefix(r, "auto renew failed: "):
		reason, ok := autoRenewFailText[strings.TrimPrefix(r, "auto renew failed: ")]
		if !ok {
			reason = "其他原因"
		}
		out.Kind, out.Detail = "auto_renew_failed", "自动续费失败："+reason
	case strings.HasPrefix(r, "auto renew "):
		d := ""
		if i := strings.LastIndex(r, "("); i >= 0 {
			d = strings.TrimSuffix(r[i+1:], ")")
		}
		out.Kind, out.Detail = "auto_renew", "自动续费成功（"+durationText(catalog.Duration(d))+"）"
	case strings.HasPrefix(r, "renew "):
		out.Kind, out.Detail = "renew", "续费成功"
	case strings.HasPrefix(r, "user cancel"):
		out.Kind, out.Detail = "cancelled", "已取消预订"+refundText(r)
	case strings.HasPrefix(r, "admin cancel"), strings.HasPrefix(r, "unfulfilled"), strings.HasPrefix(r, "cancelled: user cannot rent"):
		out.Kind, out.Detail = "cancelled", "预订已由平台取消"+refundText(r)
	case r == "expired":
		out.Kind, out.Detail = "expired", "到期停机"
	case r == "user released":
		out.Kind, out.Detail = "released", "已释放"
	case r == "retention expired":
		out.Kind, out.Detail = "wiped", "保留期结束，数据已清除"
	case r == "agent: wiped":
		out.Kind, out.Detail = "released", "释放完成"
	default:
		return UserEvent{}, false
	}
	return out, true
}

func durationText(d catalog.Duration) string {
	switch d {
	case catalog.D1d:
		return "1 天"
	case catalog.D1w:
		return "1 周"
	case catalog.D1m:
		return "1 月"
	}
	return string(d)
}

// refundText 从 "..., refund <分>" 取金额。
func refundText(r string) string {
	i := strings.LastIndex(r, "refund ")
	if i < 0 {
		return ""
	}
	var cents int64
	if _, err := fmt.Sscanf(r[i+len("refund "):], "%d", &cents); err != nil || cents <= 0 {
		return ""
	}
	return fmt.Sprintf("，已退款 ¥%d.%02d", cents/100, cents%100)
}

// UserEvents 返回用户自己实例的最近 20 条白名单事件（新到旧）；只扫描最近 200 条原始事件，事件很多的实例也不全表读。
func (s *Service) UserEvents(ctx context.Context, userID int64, id string) ([]UserEvent, error) {
	if _, err := s.GetForUser(ctx, userID, id); err != nil {
		return nil, err
	}
	rows, err := s.d.Pool.Query(ctx, `SELECT id, from_state, to_state, reason, at FROM instance_events WHERE instance_id = $1 ORDER BY id DESC LIMIT 200`, id)
	if err != nil {
		return nil, err
	}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Event, error) {
		var e Event
		err := r.Scan(&e.ID, &e.FromState, &e.ToState, &e.Reason, &e.At)
		return e, err
	})
	if err != nil {
		return nil, err
	}
	out := []UserEvent{}
	for _, e := range all {
		if ue, ok := userEventOf(e); ok {
			out = append(out, ue)
			if len(out) == 20 {
				break
			}
		}
	}
	return out, nil
}
