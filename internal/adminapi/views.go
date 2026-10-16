package adminapi

import (
	"time"

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
}

func (s *Server) instanceView(i rental.Instance) instanceView {
	return instanceView{ID: i.ID, UserID: i.UserID, MachineID: i.MachineID, ProductID: i.ProductID, ImageID: i.ImageID,
		State: i.State, Suspended: i.Suspended(), PendingSeconds: i.PendingSeconds,
		RemainingSeconds: int64(i.Remaining(s.Clock.Now()) / time.Second), ExpiresAt: i.ExpiresAt, StoppedAt: i.StoppedAt, CreatedAt: i.CreatedAt}
}

func (s *Server) instanceViews(list []rental.Instance) []instanceView {
	out := make([]instanceView, 0, len(list))
	for _, i := range list {
		out = append(out, s.instanceView(i))
	}
	return out
}
