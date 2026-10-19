package adminapi

import (
	"time"

	"golang.org/x/crypto/ssh"

	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/rental"
)

// instanceView 是后台展示的实例信息（不含 SSH 密码）。
type instanceView struct {
	ID               string     `json:"id"`
	UserID           int64      `json:"user_id"`
	MachineID        int64      `json:"machine_id"`
	ProductID        int64      `json:"product_id"`
	ImageID          int64      `json:"image_id"`
	State            string     `json:"state"`
	Suspended        bool       `json:"suspended"`
	PendingSeconds   int64      `json:"pending_seconds"`
	RemainingSeconds int64      `json:"remaining_seconds"`
	ExpiresAt        *time.Time `json:"expires_at"`
	StoppedAt        *time.Time `json:"stopped_at"`
	CreatedAt        time.Time  `json:"created_at"`
	PasswordAuth     bool       `json:"password_auth"`
	// 计划 6：预订与自动续费
	ScheduledStartAt      *time.Time `json:"scheduled_start_at"`
	ScheduledEndAt        *time.Time `json:"scheduled_end_at"`
	GroupID               *int64     `json:"group_id"`
	BatchID               *string    `json:"batch_id"`
	ReassignCount         int        `json:"reassign_count"`
	AutoRenew             bool       `json:"auto_renew"`
	AutoRenewDuration     string     `json:"auto_renew_duration"`
	AutoRenewFailedReason string     `json:"auto_renew_failed_reason"`
	ProvisionedAt         *time.Time `json:"provisioned_at"`
	CancelledAt           *time.Time `json:"cancelled_at"`
}

func (s *Server) instanceView(i rental.Instance) instanceView {
	v := instanceView{ID: i.ID, UserID: i.UserID, MachineID: i.MachineID, ProductID: i.ProductID, ImageID: i.ImageID,
		State: i.State, Suspended: i.Suspended(), PendingSeconds: i.PendingSeconds,
		RemainingSeconds: int64(i.Remaining(s.Clock.Now()) / time.Second), ExpiresAt: i.ExpiresAt, StoppedAt: i.StoppedAt, CreatedAt: i.CreatedAt,
		PasswordAuth: i.PasswordAuth}
	v.ScheduledStartAt, v.GroupID, v.BatchID, v.ReassignCount = i.ScheduledStartAt, i.GroupID, i.BatchID, i.ReassignCount
	v.AutoRenew, v.AutoRenewDuration, v.AutoRenewFailedReason = i.AutoRenew, string(i.AutoRenewDuration), i.AutoRenewFailedReason
	v.ProvisionedAt, v.CancelledAt = i.ProvisionedAt, i.CancelledAt
	if i.ScheduledStartAt != nil {
		switch i.State {
		case rental.StateReserved, rental.StateCreating, rental.StateCancelled:
			// 尚未开始计时：开始时刻 + 已付时长
			end := i.ScheduledStartAt.Add(time.Duration(i.PendingSeconds) * time.Second)
			v.ScheduledEndAt = &end
		default:
			// 已开始计时（running / stopped 及之后）：以实际到期为准（迟开通、续费、补偿、故障封顶都反映在 expires_at）
			v.ScheduledEndAt = i.ExpiresAt
		}
	}
	return v
}

func (s *Server) instanceViews(list []rental.Instance) []instanceView {
	out := make([]instanceView, 0, len(list))
	for _, i := range list {
		out = append(out, s.instanceView(i))
	}
	return out
}

// orderView 是后台展示的订单（比用户端多 user_id）。
type orderView struct {
	ID             string           `json:"id"`
	UserID         int64            `json:"user_id"`
	Kind           string           `json:"kind"`
	InstanceID     string           `json:"instance_id"`
	ProductID      int64            `json:"product_id"`
	Duration       catalog.Duration `json:"duration"`
	Amount         int64            `json:"amount"`
	RefundedAmount int64            `json:"refunded_amount"`
	Status         string           `json:"status"`
	CreatedAt      time.Time        `json:"created_at"`
	Quantity       int              `json:"quantity"`
	BatchID        *string          `json:"batch_id"`
	Auto           bool             `json:"auto"`
}

func toOrderView(o rental.Order) orderView {
	return orderView{ID: o.ID, UserID: o.UserID, Kind: o.Kind, InstanceID: o.InstanceID, ProductID: o.ProductID, Duration: o.Duration,
		Amount: o.Amount, RefundedAmount: o.RefundedAmount, Status: o.Status, CreatedAt: o.CreatedAt,
		Quantity: o.Quantity, BatchID: o.BatchID, Auto: o.Auto}
}

func orderViews(list []rental.Order) []orderView {
	out := make([]orderView, 0, len(list))
	for _, o := range list {
		out = append(out, toOrderView(o))
	}
	return out
}

type commandView struct {
	ID        string    `json:"id"`
	MachineID int64     `json:"machine_id"`
	Type      string    `json:"type"`
	Status    string    `json:"status"`
	Result    string    `json:"result"`
	Attempts  int       `json:"attempts"`
	CreatedAt time.Time `json:"created_at"`
}

func commandViews(list []fleet.Command) []commandView {
	out := make([]commandView, 0, len(list))
	for _, c := range list {
		out = append(out, commandView{ID: c.ID, MachineID: c.MachineID, Type: string(c.Type), Status: c.Status, Result: c.Result,
			Attempts: c.Attempts, CreatedAt: c.CreatedAt})
	}
	return out
}

type portView struct {
	Port          int       `json:"port"`
	Visibility    string    `json:"visibility"`
	ForcedPrivate bool      `json:"forced_private"`
	Note          string    `json:"note"`
	URL           string    `json:"url"`
	CreatedAt     time.Time `json:"created_at"`
}

func (s *Server) portViews(instanceID string, list []rental.Port) []portView {
	out := make([]portView, 0, len(list))
	for _, p := range list {
		url := ""
		if s.PortURL != nil {
			url = s.PortURL(instanceID, p.Port)
		}
		out = append(out, portView{Port: p.Port, Visibility: p.Visibility, ForcedPrivate: p.ForcedPrivate, Note: p.Note, URL: url, CreatedAt: p.CreatedAt})
	}
	return out
}

// hostKeyFingerprint 返回容器主机公钥的 SHA256 指纹；为空（存量实例未首连）返回空串。
func hostKeyFingerprint(pubLine string) string {
	if pubLine == "" {
		return ""
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(pubLine))
	if err != nil {
		return ""
	}
	return ssh.FingerprintSHA256(pub)
}
