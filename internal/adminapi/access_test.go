package adminapi_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"spark-cloud/internal/admin"
	"spark-cloud/internal/rental"
)

func TestAdminAccessViewsAndPortActions(t *testing.T) {
	e := newEnv(t)
	e.machine("SN-ACC")
	uid := e.renter("13800000051")
	inst := e.place(uid)
	e.ack()
	if _, err := e.rental.AddPort(e.ctx, uid, inst.ID, 8888, "public", "lab", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.identity.AddSSHKey(e.ctx, uid, "mac", pubLineAdmin(t), ""); err != nil {
		t.Fatal(err)
	}
	sid, _ := e.rental.OpenSSHSession(e.ctx, rental.SSHSessionStart{InstanceID: inst.ID, UserID: uid, MachineID: inst.MachineID, AuthMethod: "password", ClientIP: "203.0.113.1"})
	fin := e.loginAs("fin", admin.RoleFinance)
	code, out := fin.do("GET", "/instances/"+inst.ID, nil)
	ports := out["ports"].([]any)
	if code != 200 || out["instance"].(map[string]any)["password_auth"] != true || len(ports) != 1 || ports[0].(map[string]any)["url"] != "http://"+inst.ID+"-8888.spark.test/" ||
		out["ssh_host_pubkey_fingerprint"] == "" {
		t.Fatalf("detail = %d %v", code, out)
	}
	if code, out := fin.do("GET", "/instances/"+inst.ID+"/ssh-sessions?page=1&page_size=10", nil); code != 200 || out["total"].(float64) != 1 || out["items"].([]any)[0].(map[string]any)["id"].(float64) != float64(sid) {
		t.Fatalf("sessions = %d %v", code, out)
	}
	if code, out := fin.do("GET", "/users/"+itoa(uid), nil); code != 200 || len(out["ssh_keys"].([]any)) != 1 || out["ssh_keys"].([]any)[0].(map[string]any)["name"] != "mac" {
		t.Fatalf("user detail = %d %v", code, out)
	}
	if code, out := fin.do("GET", "/dashboard", nil); code != 200 || out["ssh_sessions_open"].(float64) != 1 {
		t.Fatalf("dashboard = %d %v", code, out)
	}
	// 财务不能处置端口
	if code, _ := fin.do("POST", "/instances/"+inst.ID+"/ports/8888/force-private", map[string]string{"reason": "x"}); code != 403 {
		t.Fatalf("finance force-private = %d", code)
	}
	ops := e.loginAs("ops", admin.RoleOps)
	if code, _ := ops.do("POST", "/instances/"+inst.ID+"/ports/8888/force-private", map[string]string{"reason": ""}); code != 400 {
		t.Fatalf("empty reason = %d", code)
	}
	code, out = ops.do("POST", "/instances/"+inst.ID+"/ports/8888/force-private", map[string]string{"reason": "钓鱼"})
	if code != 200 || out["forced_private"] != true || out["visibility"] != "private" {
		t.Fatalf("force = %d %v", code, out)
	}
	if code, _ := ops.do("DELETE", "/instances/"+inst.ID+"/ports/8888", map[string]string{"reason": "abuse"}); code != 200 {
		t.Fatalf("delete = %d", code)
	}
	if code, _ := ops.do("DELETE", "/instances/"+inst.ID+"/ports/8888", map[string]string{"reason": "abuse"}); code != 404 {
		t.Fatalf("delete twice = %d", code)
	}
	var n int
	e.pool.QueryRow(e.ctx, `SELECT count(*) FROM audit_logs WHERE action IN ('instance.port_force_private', 'instance.port_delete') AND actor_type = 'admin'`).Scan(&n)
	if n != 2 {
		t.Fatalf("audit = %d", n)
	}
}

func pubLineAdmin(t *testing.T) string {
	t.Helper()
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	p, _ := ssh.NewPublicKey(pub)
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(p))) + " test@mac"
}
