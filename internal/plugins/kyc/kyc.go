// Package kyc 定义实名核验插件接口；生产实现（阿里云三要素）在计划 4 中加入。
package kyc

import "context"

type Result struct {
	Passed bool
	Reason string
}

type Verifier interface {
	VerifyPerson(ctx context.Context, name, idNumber, phone string) (Result, error)
}

// Mock 默认全部通过；Reject 为 true 时全部不通过。
type Mock struct {
	Reject bool
}

func (m Mock) VerifyPerson(context.Context, string, string, string) (Result, error) {
	if m.Reject {
		return Result{Passed: false, Reason: "姓名、身份证号与手机号不一致"}, nil
	}
	return Result{Passed: true}, nil
}
