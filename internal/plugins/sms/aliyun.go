package sms

import (
	"context"
	"encoding/json"
	"fmt"

	"spark-cloud/internal/plugins/aliyun"
)

// Aliyun 通过阿里云短信服务 SendSms 发送短信。
type Aliyun struct {
	Client    *aliyun.Client
	SignName  string
	Templates map[string]string // 业务模板名（TemplateLoginCode 等）→ 阿里云模板编号
}

func (a *Aliyun) Send(ctx context.Context, phone, template string, params map[string]string) error {
	code, ok := a.Templates[template]
	if !ok || code == "" {
		return fmt.Errorf("aliyun sms: 未配置模板 %s", template)
	}
	if params == nil {
		params = map[string]string{}
	}
	tp, err := json.Marshal(params)
	if err != nil {
		return err
	}
	var resp struct {
		Code, Message, BizId, RequestId string
	}
	if err := a.Client.Call(ctx, "SendSms", "2017-05-25", map[string]string{
		"PhoneNumbers": phone, "SignName": a.SignName, "TemplateCode": code, "TemplateParam": string(tp),
	}, &resp); err != nil {
		return err
	}
	if resp.Code != "OK" {
		return fmt.Errorf("aliyun sms: %s %s (request %s)", resp.Code, resp.Message, resp.RequestId)
	}
	return nil
}
