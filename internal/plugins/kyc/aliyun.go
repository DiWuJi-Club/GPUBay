package kyc

import (
	"context"
	"fmt"

	"spark-cloud/internal/plugins/aliyun"
)

// Aliyun 调用阿里云号码百科「运营商三要素核验」（姓名 + 身份证号 + 手机号）。
type Aliyun struct {
	Client   *aliyun.Client
	AuthCode string // 号码百科控制台申请的授权码
}

func (a *Aliyun) VerifyPerson(ctx context.Context, name, idNumber, phone string) (Result, error) {
	var resp struct {
		Code, Message, RequestId string
		Data                     struct {
			IsConsistent *int
		}
	}
	if err := a.Client.Call(ctx, "ThreeElementsVerification", "2020-02-17", map[string]string{
		"AuthCode": a.AuthCode, "InputNumber": phone, "IdentifyNum": idNumber, "UserName": name, "Mask": "NORMAL",
	}, &resp); err != nil {
		return Result{}, err
	}
	if resp.Code != "OK" {
		return Result{}, fmt.Errorf("aliyun kyc: %s %s (request %s)", resp.Code, resp.Message, resp.RequestId)
	}
	if resp.Data.IsConsistent == nil {
		return Result{}, fmt.Errorf("aliyun kyc: 响应缺少 IsConsistent (request %s)", resp.RequestId)
	}
	switch *resp.Data.IsConsistent {
	case 1:
		return Result{Passed: true}, nil
	case 0:
		return Result{Passed: false, Reason: "姓名、身份证号与手机号不一致"}, nil
	case 2:
		return Result{Passed: false, Reason: "该手机号未查到实名信息，请使用本人实名登记的手机号"}, nil
	}
	return Result{}, fmt.Errorf("aliyun kyc: 未知的 IsConsistent=%d (request %s)", *resp.Data.IsConsistent, resp.RequestId)
}
