package butler

import (
	"errors"
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

// errGlobalBusy：本副本同时进行的轮次已满（MaxConcurrentRuns）；错误码与 butler_busy 相同，文案不同。
var errGlobalBusy = apperr.New(apperr.ErrButlerBusy.Code, "管家正忙，请稍后", apperr.ErrButlerBusy.Status)

// errStaleRun：回写时 run_token 已不匹配（清扫器已接管或另一轮已开始），本轮的结果丢弃。
var errStaleRun = errors.New("butler: stale run token")

// errNoPending：对话没有待确认的工具调用。
var errNoPending = errors.New("butler: no pending tool call")
