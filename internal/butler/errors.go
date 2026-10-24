package butler

import (
	"strings"

	"spark-cloud/internal/platform/apperr"
)

// disabledErr 是后台关闭管家时返回的错误：notice 非空时拼到消息后（「AI 管家暂未开放：<notice>」），错误码与状态码不变。
// 注意：带 notice 时返回的是新建的 *apperr.Error，不是 apperr.ErrButlerDisabled 本身，
// 调用方 / 测试必须按错误码判断（apperr.As(err).Code == "butler_disabled"），不能用 errors.Is。
func disabledErr(notice string) error {
	notice = strings.TrimSpace(notice)
	if notice == "" {
		return apperr.ErrButlerDisabled
	}
	e := apperr.ErrButlerDisabled
	return apperr.New(e.Code, e.Message+"："+notice, e.Status)
}
