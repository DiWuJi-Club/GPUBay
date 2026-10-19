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
}

func (s *Server) instanceView(i rental.Instance) instanceView {
	return instanceView{ID: i.ID, UserID: i.UserID, MachineID: i.MachineID, ProductID: i.ProductID, ImageID: i.ImageID,
		State: i.State, Suspended: i.Suspended(), PendingSeconds: i.PendingSeconds,
		RemainingSeconds: int64(i.Remaining(s.Clock.Now()) / time.Second), ExpiresAt: i.ExpiresAt, StoppedAt: i.StoppedAt, CreatedAt: i.CreatedAt,
		PasswordAuth: i.PasswordAuth}
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
}

func toOrderView(o rental.Order) orderView {
	return orderView{ID: o.ID, UserID: o.UserID, Kind: o.Kind, InstanceID: o.InstanceID, ProductID: o.ProductID, Duration: o.Duration,
		Amount: o.Amount, RefundedAmount: o.RefundedAmount, Status: o.Status, CreatedAt: o.CreatedAt}
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
