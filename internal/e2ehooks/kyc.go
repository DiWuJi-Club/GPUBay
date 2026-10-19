package e2ehooks

import (
	"context"
	"sync/atomic"

	"spark-cloud/internal/plugins/kyc"
)

// SwitchableKYC 是可在运行中切换「全部通过 / 全部不通过」的实名核验 mock。
// kyc.Mock 是值类型、Reject 是普通字段，不能在服务运行时修改（数据竞争），所以端到端服务用它代替。
type SwitchableKYC struct {
	reject atomic.Bool
}

func (k *SwitchableKYC) SetReject(v bool) { k.reject.Store(v) }

func (k *SwitchableKYC) VerifyPerson(context.Context, string, string, string) (kyc.Result, error) {
	if k.reject.Load() {
		return kyc.Result{Passed: false, Reason: "姓名、身份证号与手机号不一致"}, nil
	}
	return kyc.Result{Passed: true}, nil
}
