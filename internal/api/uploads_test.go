package api_test

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"regexp"
	"testing"

	"spark-cloud/internal/storage"
)

// upload 以 multipart/form-data 上传一个文件字段。
func (c *client) upload(field string, content []byte) (int, map[string]any) {
	c.t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile(field, "license.png")
	if err != nil {
		c.t.Fatal(err)
	}
	_, _ = fw.Write(content)
	_ = mw.Close()
	req, _ := http.NewRequest("POST", c.base+"/uploads", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

var png = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")

func TestUploadLicense(t *testing.T) {
	e := newTestEnv(t)
	c := e.client()
	if code, _ := c.upload("file", png); code != 401 {
		t.Fatalf("anonymous upload = %d", code)
	}
	c.login("13800000001")
	code, out := c.upload("file", png)
	if code != 201 || out["content_type"] != "image/png" || !regexp.MustCompile(`^\d{4}/\d{2}/[a-z0-9]{32}\.png$`).MatchString(out["key"].(string)) {
		t.Fatalf("upload = %d %v", code, out)
	}
	if _, ok := out["user_id"]; ok {
		t.Fatal("user_id must not be exposed")
	}
}

func TestUploadRejects(t *testing.T) {
	e := newTestEnv(t)
	c := e.client()
	c.login("13800000001")
	if code, out := c.upload("file", []byte("plain text pretending to be png")); code != 415 || out["code"] != "unsupported_file_type" {
		t.Fatalf("fake png = %d %v", code, out)
	}
	if code, out := c.upload("document", png); code != 400 || out["code"] != "bad_request" {
		t.Fatalf("wrong field = %d %v", code, out)
	}
	big := append(append([]byte{}, png...), make([]byte, storage.MaxUploadSize)...)
	if code, out := c.upload("file", big); code != 413 || out["code"] != "file_too_large" {
		t.Fatalf("too large = %d %v", code, out)
	}
	// JSON 请求体不能用于上传接口
	if code, out := c.do("POST", "/uploads", map[string]string{"file": "x"}); code != 400 || out["code"] != "bad_request" {
		t.Fatalf("json upload = %d %v", code, out)
	}
	// 其他写接口仍然只接受 JSON
	req, _ := http.NewRequest("POST", c.base+"/orders", bytes.NewReader([]byte("--x--")))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("multipart to /orders = %d", resp.StatusCode)
	}
}
