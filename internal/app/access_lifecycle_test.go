package app_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"spark-cloud/internal/access/sshtest"
	"spark-cloud/internal/app"
	"spark-cloud/internal/audit"
	"spark-cloud/internal/pki"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/config"
	"spark-cloud/internal/platform/dbtest"
)

// TestAccessLifecycle：下单 → 密码登录 → 公钥 → 关闭密码 → 端口私有 / 公开 → 登出失效 → 到期停机 → 续费 → 换机 → 释放。
// `sshExec` 闭包读取变量 id，换机后把 id 改为新实例即可复用。
func TestAccessLifecycle(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.New(t)
	clk := clock.NewFake(dbtest.T0)
	keyFile := upstreamKeyFile(t)
	upSigner, err := pki.LoadUpstreamSigner(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	hostPub, hostPEM, _ := pki.GenerateInstanceHostKey()
	hostSigner, _ := ssh.ParsePrivateKey(hostPEM)
	upstream, err := sshtest.New(sshtest.Options{AuthorizedKey: upSigner.PublicKey(), HostKey: hostSigner})
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	stop := upstream.ServeHTTP(8888, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "lab:"+r.Host) }))
	defer stop()
	cfg := config.Config{UpstreamSSHKeyFile: keyFile, Env: "dev", UploadDir: t.TempDir(), Domain: "spark.test", SecretKey: dbtest.Key, SMSDriver: "mock", KYCDriver: "mock",
		UpstreamSSHPort: upstream.Port(), SSHPublicPort: 2222}
	a, err := app.Build(cfg, pool, discardLog(), app.Options{Clock: clk, HostKeyGen: func() (string, []byte, error) { return hostPub, hostPEM, nil }})
	if err != nil {
		t.Fatal(err)
	}
	sshLis, _ := net.Listen("tcp", "127.0.0.1:0")
	gwCtx, stopGW := context.WithCancel(ctx)
	gwDone := make(chan struct{})
	go func() { defer close(gwDone); a.SSH.Serve(gwCtx, sshLis) }()
	defer func() { stopGW(); <-gwDone }()
	// 不启动 RunAccessLoops：它的 2 秒定时刷新可能在显式 refresh() 之后写回更早读到的路由快照；本测试每次变更后自己刷新
	ts := httptest.NewServer(a.Handler)
	defer ts.Close()
	// 对外地址按 FillAccessDefaults 是 http://spark.test:8080；Cookie / 跳转校验只看 scheme 与域名格式，这里用 Host 头模拟
	jar, _ := cookiejar.New(nil)
	c := &client{t: t, base: ts.URL + "/api/v1", http: &http.Client{Jar: jar}}
	machine := func(serial string) int64 {
		t.Helper()
		m := dbtest.CreateMachine(t, pool, serial)
		if _, err := pool.Exec(ctx, `UPDATE machines SET lan_ip = '127.0.0.1' WHERE id = $1`, m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	// agentOn 模拟机器上的 Agent：心跳并成功执行全部待处理指令
	agentOn := func(m int64) {
		t.Helper()
		if _, err := a.Fleet.Heartbeat(ctx, m); err != nil {
			t.Fatal(err)
		}
		cmds, err := a.Fleet.PendingCommands(ctx, m)
		if err != nil {
			t.Fatal(err)
		}
		for _, cmd := range cmds {
			if err := a.Rental.HandleCommandResult(ctx, cmd.ID, true, "ok"); err != nil {
				t.Fatal(err)
			}
		}
	}
	// refresh 立即重载入口路由表（生产由 RunAccessLoops 每 2 秒刷新；这里不依赖真实时间）
	refresh := func() {
		t.Helper()
		if err := a.Routes.Refresh(ctx); err != nil {
			t.Fatal(err)
		}
	}
	mid := machine("SN001")
	agent := func() { t.Helper(); agentOn(mid) }
	c.do("POST", "/auth/sms-code", map[string]string{"phone": "13800005678"})
	c.do("POST", "/auth/login", map[string]string{"phone": "13800005678", "code": "000000"})
	c.do("POST", "/kyc/personal", map[string]string{"name": "王五", "id_number": "110101199001011234"})
	rch := c.do("POST", "/recharges", map[string]any{"amount": 100000, "channel": "wechat"})
	c.do("POST", "/dev/recharges/"+rch["id"].(string)+"/pay", map[string]any{})
	products := c.do("GET", "/products", nil)["items"].([]any)
	images := c.do("GET", "/images", nil)["items"].([]any)
	order := c.do("POST", "/orders", map[string]any{"product_id": products[0].(map[string]any)["id"], "duration": "1d", "image_id": images[0].(map[string]any)["id"]})
	id := order["instance"].(map[string]any)["id"].(string)
	agent()
	inst := c.do("GET", "/instances/"+id, nil)
	if inst["state"] != "running" || inst["ssh_command"] != "ssh -p 2222 "+id+"@ssh.spark.test" {
		t.Fatalf("instance = %v", inst)
	}
	password := c.do("GET", "/instances/"+id+"/password", nil)["password"].(string)

	gwAddr := sshLis.Addr().String()
	sshExec := func(auth ssh.AuthMethod, cmd string) (string, error) {
		cl, err := ssh.Dial("tcp", gwAddr, &ssh.ClientConfig{User: id, Auth: []ssh.AuthMethod{auth}, HostKeyCallback: ssh.FixedHostKey(a.SSHHostKey.PublicKey()), Timeout: 5 * time.Second})
		if err != nil {
			return "", err
		}
		defer cl.Close()
		s, err := cl.NewSession()
		if err != nil {
			return "", err
		}
		defer s.Close()
		out, err := s.Output(cmd)
		return string(out), err
	}
	if out, err := sshExec(ssh.Password(password), "echo hi"); err != nil || out != "hi\n" {
		t.Fatalf("password login = %q %v", out, err)
	}
	// 公钥 + 关闭密码
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	userSigner, _ := ssh.NewSignerFromKey(priv)
	sshPub, _ := ssh.NewPublicKey(pub)
	c.do("POST", "/ssh-keys", map[string]string{"public_key": strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))})
	if got := c.do("PUT", "/instances/"+id+"/access", map[string]bool{"password_auth": false}); got["password_auth"] != false {
		t.Fatalf("disable password = %v", got)
	}
	if _, err := sshExec(ssh.Password(password), "echo hi"); err == nil {
		t.Fatal("password accepted while disabled")
	}
	if out, err := sshExec(ssh.PublicKeys(userSigner), "echo key"); err != nil || out != "key\n" {
		t.Fatalf("key login = %q %v", out, err)
	}
	// 端口：私有 → 302 到 authorize → callback → 200；公开 → 直接 200
	c.do("POST", "/instances/"+id+"/ports", map[string]any{"port": 8888, "note": "lab"})
	refresh()
	host := id + "-8888.spark.test"
	noFollow := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	tsURL, _ := url.Parse(ts.URL)
	getHost := func(client *http.Client, h, path string) *http.Response {
		req, _ := http.NewRequest("GET", ts.URL+path, nil)
		req.Host = h
		if h == "spark.test" { // Client 按 req.Host 查 Cookie jar：控制台会话是在 127.0.0.1 上登录的，这里手动带上（浏览器里就是同一个域）
			for _, ck := range jar.Cookies(tsURL) {
				req.AddCookie(ck)
			}
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	r1 := getHost(noFollow, host, "/lab")
	r1.Body.Close()
	if r1.StatusCode != 302 || !strings.Contains(r1.Header.Get("Location"), "/api/v1/ingress/authorize?target=") {
		t.Fatalf("private no cookie = %d %s", r1.StatusCode, r1.Header.Get("Location"))
	}
	target := url.QueryEscape("http://" + host + ":8080/lab")
	r2 := getHost(noFollow, "spark.test", "/api/v1/ingress/authorize?target="+target)
	r2.Body.Close()
	cb, _ := url.Parse(r2.Header.Get("Location"))
	if r2.StatusCode != 302 || cb.Path != "/.spark/callback" {
		t.Fatalf("authorize = %d %s", r2.StatusCode, r2.Header.Get("Location"))
	}
	r3 := getHost(noFollow, host, "/.spark/callback?"+cb.RawQuery)
	r3.Body.Close()
	var ingressCookie *http.Cookie
	for _, ck := range r3.Cookies() {
		if ck.Name == "sc_ingress" {
			ingressCookie = ck
		}
	}
	if r3.StatusCode != 302 || r3.Header.Get("Location") != "/lab" || ingressCookie == nil {
		t.Fatalf("callback = %d %s cookies=%v", r3.StatusCode, r3.Header.Get("Location"), r3.Cookies())
	}
	withCookie := func(h, path string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest("GET", ts.URL+path, nil)
		req.Host = h
		req.AddCookie(ingressCookie)
		resp, err := noFollow.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	// status 发请求后关闭响应体，只返回状态码
	status := func(resp *http.Response) int {
		t.Helper()
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	r4 := withCookie(host, "/lab")
	b4, _ := io.ReadAll(r4.Body)
	r4.Body.Close()
	if r4.StatusCode != 200 || string(b4) != "lab:"+host {
		t.Fatalf("private with cookie = %d %q", r4.StatusCode, b4)
	}
	c.do("PUT", "/instances/"+id+"/ports/8888", map[string]string{"visibility": "public"})
	refresh()
	if code := status(getHost(http.DefaultClient, host, "/")); code != 200 {
		t.Fatalf("public = %d", code)
	}
	// 登出控制台 → 私有端口 Cookie 失效
	c.do("PUT", "/instances/"+id+"/ports/8888", map[string]string{"visibility": "private"})
	c.do("POST", "/auth/logout", map[string]any{})
	refresh()
	// 越过验证码 60 秒限流；入口的会话缓存（2 秒）按注入的时钟计时，推进假时钟即失效，不需要真实等待
	clk.Advance(61 * time.Second)
	if code := status(withCookie(host, "/lab")); code != 302 {
		t.Fatalf("after logout = %d", code)
	}
	c.do("POST", "/auth/sms-code", map[string]string{"phone": "13800005678"})
	c.do("POST", "/auth/login", map[string]string{"phone": "13800005678", "code": "000000"})
	// 到期停机 → SSH 拒绝、Web 503 → 续费恢复
	clk.Advance(25 * time.Hour)
	if _, err := a.Fleet.Heartbeat(ctx, mid); err != nil {
		t.Fatal(err)
	}
	if err := a.Rental.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	agent()
	if _, err := sshExec(ssh.PublicKeys(userSigner), "echo x"); err == nil {
		t.Fatal("ssh allowed while stopped")
	}
	refresh()
	if code := status(getHost(http.DefaultClient, host, "/")); code != 503 {
		t.Fatalf("stopped web = %d", code)
	}
	c.do("POST", "/instances/"+id+"/renew", map[string]string{"duration": "1d"})
	agent()
	if out, err := sshExec(ssh.PublicKeys(userSigner), "echo back"); err != nil || out != "back\n" {
		t.Fatalf("after renew = %q %v", out, err)
	}
	// 换机：新实例保留端口与 password_auth、能用公钥登录；旧 ID 的 Web 走 404、SSH 被拒
	mid2 := machine("SN002")
	moved, err := a.Rental.AdminMigrate(ctx, audit.Actor{AdminID: 1}, id, "bad gpu")
	if err != nil {
		t.Fatal(err)
	}
	agentOn(mid)
	agentOn(mid2)
	ports := c.do("GET", "/instances/"+moved.ID+"/ports", nil)["items"].([]any)
	newInst := c.do("GET", "/instances/"+moved.ID, nil)
	if len(ports) != 1 || ports[0].(map[string]any)["port"].(float64) != 8888 || newInst["password_auth"] != false || newInst["state"] != "running" {
		t.Fatalf("migrated instance = %v ports=%v", newInst, ports)
	}
	refresh()
	if code := status(getHost(http.DefaultClient, host, "/")); code != 404 {
		t.Fatalf("old instance web after migrate = %d", code)
	}
	if _, err := sshExec(ssh.PublicKeys(userSigner), "echo x"); err == nil {
		t.Fatal("old instance ssh allowed after migrate")
	}
	firstID := id
	id = moved.ID
	if out, err := sshExec(ssh.PublicKeys(userSigner), "echo moved"); err != nil || out != "moved\n" {
		t.Fatalf("new instance ssh = %q %v", out, err)
	}
	// 释放 → 拒绝、主机私钥清空
	c.do("POST", "/instances/"+id+"/release", map[string]any{})
	agentOn(mid2)
	if _, err := sshExec(ssh.PublicKeys(userSigner), "echo x"); err == nil {
		t.Fatal("ssh allowed after release")
	}
	if pem, err := a.Rental.HostKeyPEM(ctx, id); err != nil || pem != "" {
		t.Fatalf("host key not cleared after release: %q %v", pem, err)
	}
	// 主机私钥不进任何 Agent 指令（两个实例的开通 / 停机 / 启动 / 洗机）：PEM 原文、PEM 正文、整段 base64（JSON 里 bytes 的编码）都不能出现
	var payloads string
	var n int
	if err := pool.QueryRow(ctx, `SELECT coalesce(string_agg(payload::text, E'\n'), ''), count(*) FROM agent_commands WHERE instance_id IN ($1, $2)`, firstID, id).Scan(&payloads, &n); err != nil {
		t.Fatal(err)
	}
	pemBody := strings.Join(strings.Split(strings.TrimSpace(string(hostPEM)), "\n")[1:2], "")
	if n < 4 || strings.Contains(payloads, "PRIVATE") || strings.Contains(payloads, pemBody) || strings.Contains(payloads, base64.StdEncoding.EncodeToString(hostPEM)) {
		t.Fatalf("host key leaked into command payloads (%d commands)", n)
	}
}
