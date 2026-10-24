// Package apperr 定义可以直接展示给用户的业务错误。
package apperr

import (
	"errors"
	"fmt"
	"strings"
)

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
	// ErrRepeatedSubmit 管理员在短时间内重复提交完全相同的资金操作（双击、重试）。
	ErrRepeatedSubmit          = New("duplicate", "60 秒内已提交过相同的操作，请勿重复提交", 409)
	ErrRateLimited             = New("rate_limited", "操作过于频繁，请稍后再试", 429)
	ErrInvalidCode             = New("invalid_code", "验证码错误或已过期", 400)
	ErrSSHKeyInvalid           = New("ssh_key_invalid", "公钥格式不正确或不受支持（支持 ed25519、ecdsa、rsa≥2048）", 400)
	ErrSSHKeyLimit             = New("ssh_key_limit", "最多添加 10 个公钥", 409)
	ErrSSHKeyExists            = New("ssh_key_exists", "该公钥已添加", 409)
	ErrPasswordAuthRequiresKey = New("password_auth_requires_key", "关闭密码登录前请先添加 SSH 公钥", 409)
	ErrPortInvalid             = New("port_invalid", "端口必须是 1～65535 之间且不为 22 的整数", 400)
	ErrPortLimit               = New("port_limit", "每个实例最多开放 5 个端口", 409)
	ErrPortExists              = New("port_exists", "该端口已添加", 409)
	ErrPortForcedPrivate       = New("port_forced_private", "该端口已被平台限制为私有", 409)
	// 计划 6
	ErrBookingTooSoon       = New("booking_too_soon", "开始时间至少在 2 小时之后", 400)
	ErrBookingTooFar        = New("booking_too_far", "最多提前 60 天预订", 400)
	ErrBookingTooLong       = New("booking_too_long", "单次租期最长 90 天", 400)
	ErrBookingNotHour       = New("booking_not_hour", "开始时间需为整点", 400)
	ErrBookingLimit         = New("booking_limit", "未开通的预订最多 16 台", 409)
	ErrCountInvalid         = New("count_invalid", "一次最多租用 8 台", 400)
	ErrGroupNotFound        = New("group_not_found", "机组不存在", 404)
	ErrMachineUnavailable   = New("machine_unavailable", "该机器在所选时段不可用", 409)
	ErrCancelNotAllowed     = New("cancel_not_allowed", "预订已开始开通，不能取消", 409)
	ErrGroupNotEmpty        = New("group_not_empty", "机组内还有机器或未结束的实例", 409)
	ErrDuplicateGroupName   = New("duplicate_group_name", "机组名称已存在", 409)
	ErrDuplicateDisplayName = New("duplicate_display_name", "展示名已被其他机器使用", 409)
	// 计划 8
	ErrInviteCodeInvalid    = New("invite_code_invalid", "邀请码无效", 400)
	ErrReferralFrozen       = New("referral_frozen", "邀请功能已暂停", 409)
	ErrReferralBindingState = New("referral_binding_state", "该绑定当前状态不允许此操作", 409)
	// 计划 7
	ErrAppNotFound      = New("app_not_found", "应用不存在或未上架", 404)
	ErrAppUnavailable   = New("app_unavailable", "该应用当前没有可用版本", 409)
	ErrAppLimit         = New("app_limit", "每个实例最多部署 3 个应用", 409)
	ErrAppExists        = New("app_exists", "该应用已在此实例上部署", 409)
	ErrAppInvalidState  = New("app_invalid_state", "当前状态不允许该操作", 409)
	ErrAppVersionFrozen = New("app_version_frozen", "已发布的版本不能修改", 409)
	// 计划 12：AI 管家。ErrButlerDisabled 由 handler 把后台设置的 notice 拼到消息后（「AI 管家暂未开放：<notice>」）
	ErrButlerDisabled       = New("butler_disabled", "AI 管家暂未开放", 503)
	ErrButlerBusy           = New("butler_busy", "上一条消息还在处理中，请稍候", 409)
	ErrButlerTurnLimit      = New("butler_turn_limit", "这个对话太长了，请新建对话", 409)
	ErrButlerQuota          = New("butler_quota_exceeded", "今日 AI 管家额度已用完，明天再来", 429)
	ErrButlerLLM            = New("butler_llm_unavailable", "AI 管家暂时不可用，请稍后再试", 503)
	ErrButlerConfirmInvalid = New("butler_confirm_invalid", "该操作已失效，请重新发起", 409)
	ErrButlerMessageInvalid = New("butler_message_invalid", "消息不能为空且不超过 2000 字", 400)
)

// As 取出错误链中的 *Error；不是业务错误时返回 nil。
func As(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return nil
}

// AppImageMismatch 该应用需要 need 中某个镜像，实例当前镜像为 have。
func AppImageMismatch(need []string, have string) *Error {
	return New("app_image_mismatch", "该应用需要以下镜像之一："+strings.Join(need, "、")+"，当前实例镜像为 "+have, 409)
}

func AppPortConflict(port int) *Error {
	return New("app_port_conflict", fmt.Sprintf("应用端口 %d 已被该实例的其它端口占用", port), 409)
}

func AppEnvInvalid(label, reason string) *Error {
	return New("app_env_invalid", label+"："+reason, 400)
}

func AppEnvRequired(label string) *Error { return New("app_env_required", "请填写 "+label, 400) }

func AppVersionInvalid(path, reason string) *Error {
	return New("app_version_invalid", path+"："+reason, 400)
}
