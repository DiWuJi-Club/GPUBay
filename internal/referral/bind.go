package referral

import "time"

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

const (
	ReasonOK            = "ok"
	ReasonInvalidCode   = "invalid_code"
	ReasonInviterFrozen = "inviter_frozen"
	ReasonExistingUser  = "existing_user"
)
