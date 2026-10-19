package api_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"spark-cloud/internal/api"
	"spark-cloud/internal/platform/dbtest"
)

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func pubLine(t *testing.T) string {
	t.Helper()
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	p, _ := ssh.NewPublicKey(pub)
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(p))) + " test@mac"
}

// runningInstance：下单并模拟 Agent 回报 create 成功（api_test 没有现成的让实例进入 running 的辅助）。
func (c *client) runningInstance(t *testing.T, e *testEnv, phone string) string {
	t.Helper()
	mid := dbtest.CreateMachine(t, e.pool, "SN-"+phone)
	user := c.login(phone)
	c.do("POST", "/kyc/personal", map[string]string{"name": "张三", "id_number": "11010119900101123X"})
	e.wallet.AdminAdjust(t.Context(), 1, int64(user["id"].(float64)), 100000, "fund", "")
	_, out := c.do("POST", "/orders", map[string]any{"product_id": dbtest.ProductID(t, e.pool), "duration": "1d", "image_id": dbtest.ImageID(t, e.pool)})
	id := out["instance"].(map[string]any)["id"].(string)
	cmds, err := e.fleet.PendingCommands(t.Context(), mid)
	if err != nil {
		t.Fatal(err)
	}
	for _, cmd := range cmds {
		if err := e.rental.HandleCommandResult(t.Context(), cmd.ID, true, "ok"); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func TestSSHKeysEndpoints(t *testing.T) {
	e := newTestEnv(t)
	c := e.client()
	c.login("13800000041")
	code, out := c.do("POST", "/ssh-keys", map[string]string{"public_key": pubLine(t)})
	if code != 201 || out["name"] != "test@mac" || !strings.HasPrefix(out["fingerprint"].(string), "SHA256:") || out["public_key"] != nil {
		t.Fatalf("add = %d %v", code, out)
	}
	if code, out := c.do("POST", "/ssh-keys", map[string]string{"public_key": "nope"}); code != 400 || out["code"] != "ssh_key_invalid" {
		t.Fatalf("invalid = %d %v", code, out)
	}
	code, out = c.do("GET", "/ssh-keys", nil)
	items := out["items"].([]any)
	if code != 200 || len(items) != 1 {
		t.Fatalf("list = %d %v", code, out)
	}
	id := int64(items[0].(map[string]any)["id"].(float64))
	other := e.client()
	other.login("13800000042")
	if code, _ := other.do("DELETE", "/ssh-keys/"+itoa(id), nil); code != 404 {
		t.Fatalf("delete other's = %d", code)
	}
	if code, _ := c.do("DELETE", "/ssh-keys/"+itoa(id), nil); code != 200 {
		t.Fatalf("delete = %d", code)
	}
	if _, out := c.do("GET", "/ssh-keys", nil); len(out["items"].([]any)) != 0 {
		t.Fatal("not deleted")
	}
}

func TestInstanceAccessEndpoints(t *testing.T) {
	e := newTestEnvWith(t, func(s *api.Server) { s.SSHPort = 2200 })
	c := e.client()
	id := c.runningInstance(t, e, "13800000043")
	_, inst := c.do("GET", "/instances/"+id, nil)
	if inst["ssh_command"] != "ssh -p 2200 "+id+"@ssh.spark.test" || inst["password_auth"] != true || inst["ssh_host"] != "ssh.spark.test" || inst["ssh_port"].(float64) != 2200 || inst["ports_count"].(float64) != 0 {
		t.Fatalf("view = %v", inst)
	}
	_, before := c.do("GET", "/instances/"+id+"/password", nil)
	code, out := c.do("POST", "/instances/"+id+"/password/reset", nil)
	if code != 200 || len(out["password"].(string)) != 16 || out["password"] == before["password"] {
		t.Fatalf("reset = %d %v", code, out)
	}
	if code, out := c.do("PUT", "/instances/"+id+"/access", map[string]bool{"password_auth": false}); code != 409 || out["code"] != "password_auth_requires_key" {
		t.Fatalf("disable without key = %d %v", code, out)
	}
	c.do("POST", "/ssh-keys", map[string]string{"public_key": pubLine(t)})
	if code, out := c.do("PUT", "/instances/"+id+"/access", map[string]bool{"password_auth": false}); code != 200 || out["password_auth"] != false {
		t.Fatalf("disable = %d %v", code, out)
	}
	// 端口
	code, out = c.do("POST", "/instances/"+id+"/ports", map[string]any{"port": 8888, "note": "lab"})
	if code != 201 || out["visibility"] != "private" || out["url"] != "http://"+id+"-8888.spark.test/" || out["note"] != "lab" {
		t.Fatalf("add port = %d %v", code, out)
	}
	if code, out := c.do("POST", "/instances/"+id+"/ports", map[string]any{"port": 22}); code != 400 || out["code"] != "port_invalid" {
		t.Fatalf("port 22 = %d %v", code, out)
	}
	if code, out := c.do("PUT", "/instances/"+id+"/ports/8888", map[string]any{"visibility": "public"}); code != 200 || out["visibility"] != "public" {
		t.Fatalf("update = %d %v", code, out)
	}
	for p := 8001; p <= 8004; p++ {
		c.do("POST", "/instances/"+id+"/ports", map[string]any{"port": p})
	}
	if code, out := c.do("POST", "/instances/"+id+"/ports", map[string]any{"port": 8005}); code != 409 || out["code"] != "port_limit" {
		t.Fatalf("limit = %d %v", code, out)
	}
	if _, inst := c.do("GET", "/instances/"+id, nil); inst["ports_count"].(float64) != 5 {
		t.Fatalf("ports_count = %v", inst["ports_count"])
	}
	if code, _ := c.do("DELETE", "/instances/"+id+"/ports/8001", nil); code != 200 {
		t.Fatalf("delete port = %d", code)
	}
	code, out = c.do("GET", "/instances/"+id+"/ports", nil)
	if code != 200 || len(out["items"].([]any)) != 4 {
		t.Fatalf("list ports = %d %v", code, out)
	}
	if code, out := c.do("GET", "/instances/"+id+"/ssh-sessions", nil); code != 200 || len(out["items"].([]any)) != 0 {
		t.Fatalf("sessions = %d %v", code, out)
	}
	// 别人的实例一律 404
	other := e.client()
	other.login("13800000044")
	for _, req := range []struct{ m, p string }{{"POST", "/instances/" + id + "/password/reset"}, {"GET", "/instances/" + id + "/ports"}, {"PUT", "/instances/" + id + "/access"}} {
		var body any
		if req.m != "GET" {
			body = map[string]any{"password_auth": true}
		}
		if code, _ := other.do(req.m, req.p, body); code != 404 {
			t.Fatalf("%s %s by other = %d", req.m, req.p, code)
		}
	}
}
