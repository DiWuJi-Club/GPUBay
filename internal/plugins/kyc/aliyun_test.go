package kyc_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"spark-cloud/internal/plugins/aliyun"
	"spark-cloud/internal/plugins/kyc"
)

func newAliyunKYC(t *testing.T, body string, seen map[string]string) *kyc.Aliyun {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		for _, k := range []string{"Action", "Version", "AuthCode", "InputNumber", "IdentifyNum", "UserName", "Mask"} {
			seen[k] = q.Get(k)
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &kyc.Aliyun{Client: &aliyun.Client{Endpoint: srv.URL, AccessKeyID: "ak", AccessKeySecret: "sk"}, AuthCode: "auth-1"}
}

func TestAliyunThreeElements(t *testing.T) {
	cases := []struct {
		body    string
		passed  bool
		reason  string
		wantErr bool
	}{
		{`{"Code":"OK","Data":{"IsConsistent":1}}`, true, "", false},
		{`{"Code":"OK","Data":{"IsConsistent":0}}`, false, "姓名、身份证号与手机号不一致", false},
		{`{"Code":"OK","Data":{"IsConsistent":2}}`, false, "该手机号未查到实名信息，请使用本人实名登记的手机号", false},
		{`{"Code":"OK","Data":{"IsConsistent":9}}`, false, "", true},
		{`{"Code":"OK","Data":{}}`, false, "", true},
		{`{"Code":"Account.Arrears","Message":"欠费"}`, false, "", true},
	}
	for _, c := range cases {
		seen := map[string]string{}
		v := newAliyunKYC(t, c.body, seen)
		res, err := v.VerifyPerson(context.Background(), "张三", "11010119900101123X", "13800001234")
		if (err != nil) != c.wantErr || res.Passed != c.passed || res.Reason != c.reason {
			t.Fatalf("%s: res=%+v err=%v", c.body, res, err)
		}
		if seen["Action"] != "ThreeElementsVerification" || seen["Version"] != "2020-02-17" || seen["AuthCode"] != "auth-1" ||
			seen["InputNumber"] != "13800001234" || seen["IdentifyNum"] != "11010119900101123X" || seen["UserName"] != "张三" || seen["Mask"] != "NORMAL" {
			t.Fatalf("request = %v", seen)
		}
	}
}
