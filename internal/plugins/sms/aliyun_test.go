package sms_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"spark-cloud/internal/plugins/aliyun"
	"spark-cloud/internal/plugins/sms"
)

func newAliyunSMS(t *testing.T, handler http.HandlerFunc) *sms.Aliyun {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &sms.Aliyun{
		Client:    &aliyun.Client{Endpoint: srv.URL, AccessKeyID: "ak", AccessKeySecret: "sk"},
		SignName:  "Spark算力",
		Templates: map[string]string{sms.TemplateLoginCode: "SMS_100", sms.TemplateExpiryReminder: "SMS_200"},
	}
}

func TestAliyunSendSms(t *testing.T) {
	var got map[string]string
	s := newAliyunSMS(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		got = map[string]string{"Action": q.Get("Action"), "Version": q.Get("Version"), "PhoneNumbers": q.Get("PhoneNumbers"),
			"SignName": q.Get("SignName"), "TemplateCode": q.Get("TemplateCode"), "TemplateParam": q.Get("TemplateParam")}
		_, _ = w.Write([]byte(`{"Code":"OK","Message":"OK","BizId":"9","RequestId":"r"}`))
	})
	if err := s.Send(context.Background(), "13800001234", sms.TemplateExpiryReminder, map[string]string{"instance": "ins-1", "hours": "24"}); err != nil {
		t.Fatal(err)
	}
	var params map[string]string
	if err := json.Unmarshal([]byte(got["TemplateParam"]), &params); err != nil {
		t.Fatal(err)
	}
	if got["Action"] != "SendSms" || got["Version"] != "2017-05-25" || got["PhoneNumbers"] != "13800001234" ||
		got["SignName"] != "Spark算力" || got["TemplateCode"] != "SMS_200" || params["instance"] != "ins-1" || params["hours"] != "24" {
		t.Fatalf("request = %v", got)
	}
}

func TestAliyunSendSmsFailures(t *testing.T) {
	s := newAliyunSMS(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"Code":"isv.BUSINESS_LIMIT_CONTROL","Message":"触发分钟级流控","RequestId":"r"}`))
	})
	err := s.Send(context.Background(), "13800001234", sms.TemplateLoginCode, map[string]string{"code": "123456"})
	if err == nil || !strings.Contains(err.Error(), "isv.BUSINESS_LIMIT_CONTROL") {
		t.Fatalf("business error = %v", err)
	}
	if err := s.Send(context.Background(), "13800001234", "unknown_template", nil); err == nil {
		t.Fatal("unknown template must fail")
	}
	down := newAliyunSMS(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "oops", 500) })
	if err := down.Send(context.Background(), "13800001234", sms.TemplateLoginCode, map[string]string{"code": "1"}); err == nil {
		t.Fatal("HTTP 500 must fail")
	}
}
