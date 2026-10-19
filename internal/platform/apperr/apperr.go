// Package apperr 定义可以直接展示给用户的业务错误。
package apperr

import "errors"

type Error struct {
	Code    string
	Message string
	Status  int
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func New(code, message string, status int) *Error {
	return &Error{Code: code, Message: message, Status: status}
}

func BadRequest(message string) *Error { return New("bad_request", message, 400) }

var (
	ErrNotFound            = New("not_found", "资源不存在", 404)
	ErrUnauthorized        = New("unauthorized", "请先登录", 401)
	ErrForbidden           = New("forbidden", "无权执行该操作", 403)
	ErrBanned              = New("banned", "账号已被封禁", 403)
	ErrKYCRequired         = New("kyc_required", "请先完成实名认证", 403)
	ErrInsufficientBalance = New("insufficient_balance", "余额不足，请先充值", 402)
	ErrNoCapacity          = New("no_capacity", "暂无空闲机器，请稍后再试", 409)
	ErrInvalidState        = New("invalid_state", "当前状态不允许该操作", 409)
	ErrDuplicate           = New("duplicate", "重复操作", 409)
	ErrRateLimited         = New("rate_limited", "操作过于频繁，请稍后再试", 429)
	ErrInvalidCode         = New("invalid_code", "验证码错误或已过期", 400)
)

// As 取出错误链中的 *Error；不是业务错误时返回 nil。
func As(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return nil
}
