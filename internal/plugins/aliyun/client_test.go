package aliyun_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"spark-cloud/internal/plugins/aliyun"
)

// 阿里云文档中 RPC 签名的示例（ECS DescribeRegions），用于校验签名算法。
func TestSignMatchesDocumentedExample(t *testing.T) {
	params := map[string]string{
		"AccessKeyId": "testid", "Action": "DescribeRegions", "Format": "XML", "SignatureMethod": "HMAC-SHA1",
		"SignatureNonce": "3ee8c1b8-83d3-44af-a94f-4e0ad82fd6cf", "SignatureVersion": "1.0",
		"Timestamp": "2016-02-23T12:46:24Z", "Version": "2014-05-26",
	}
	if got := aliyun.Sign("testsecret", "GET", params); got != "OLeaidS1JvxuMvnyHOwuJ+uX5qY=" {
		t.Fatalf("signature = %s", got)
	}
}

func TestCallSendsSignedRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		params := map[string]string{}
		for k := range q {
			if k != "Signature" {
				params[k] = q.Get(k)
			}
		}
		if q.Get("Signature") != aliyun.Sign("sk", "GET", params) {
			http.Error(w, `{"Code":"SignatureDoesNotMatch","Message":"bad sig","RequestId":"r0"}`, 400)
			return
		}
		if q.Get("Action") != "Ping" || q.Get("Version") != "2020-01-01" || q.Get("AccessKeyId") != "ak" ||
			q.Get("Format") != "JSON" || q.Get("Timestamp") != "2026-01-01T08:00:00Z" || q.Get("SignatureNonce") != "n-1" ||
			q.Get("Name") != "张 三+1" {
			http.Error(w, `{"Code":"BadParams","Message":"`+r.URL.RawQuery+`"}`, 400)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"Code": "OK", "Echo": q.Get("Name")})
	}))
	defer srv.Close()
	c := &aliyun.Client{Endpoint: srv.URL, AccessKeyID: "ak", AccessKeySecret: "sk",
		Now:   func() time.Time { return time.Date(2026, 1, 1, 16, 0, 0, 0, time.FixedZone("CST", 8*3600)) },
		Nonce: func() string { return "n-1" }}
	var out struct{ Code, Echo string }
	if err := c.Call(context.Background(), "Ping", "2020-01-01", map[string]string{"Name": "张 三+1"}, &out); err != nil {
		t.Fatal(err)
	}
	if out.Code != "OK" || out.Echo != "张 三+1" {
		t.Fatalf("out = %+v", out)
	}
}

func TestCallMapsHTTPErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"Code":"InvalidAccessKeyId.NotFound","Message":"Specified access key is not found.","RequestId":"r1"}`))
	}))
	defer srv.Close()
	c := &aliyun.Client{Endpoint: srv.URL, AccessKeyID: "ak", AccessKeySecret: "sk"}
	err := c.Call(context.Background(), "Ping", "2020-01-01", nil, &struct{}{})
	var ae *aliyun.Error
	if !errors.As(err, &ae) || ae.Status != 404 || ae.Code != "InvalidAccessKeyId.NotFound" || ae.RequestID != "r1" {
		t.Fatalf("err = %v", err)
	}
}
