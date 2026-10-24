package butler

import (
	"strings"

	"spark-cloud/internal/platform/apperr"
)

// disabledErr 是后台关闭管家时返回的错误：notice 非空时拼到消息后（「AI 管家暂未开放：<notice>」），错误码与状态码不变。
func disabledErr(notice string) error {
	notice = strings.TrimSpace(notice)
	if notice == "" {
		return apperr.ErrButlerDisabled
	}
	e := apperr.ErrButlerDisabled
	return apperr.New(e.Code, e.Message+"："+notice, e.Status)
}
