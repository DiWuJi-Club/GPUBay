// Package e2ehooks 为 cmd/e2e-server 提供端到端测试钩子：推进假时钟、驱动步进式 fake Agent（经真实网关）、造数据、切换实名 mock。
// 只被 cmd/e2e-server 引用，生产二进制不包含它。所有钩子用 app.App 暴露的服务实现，不复制业务逻辑。
package e2ehooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/ssh"

	"spark-cloud/internal/access/sshtest"
	"spark-cloud/internal/admin"
	"spark-cloud/internal/agent/fakeagent"
	"spark-cloud/internal/agent/metrics"
	"spark-cloud/internal/agent/state"
	"spark-cloud/internal/app"
	"spark-cloud/internal/audit"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/randx"
)

// SeedPassword 是钩子创建的管理员的固定密码。
const SeedPassword = "e2e-password-123456"

// machineBatch 为未指定前缀的 /hooks/machines 调用编号。
var machineBatch atomic.Int64

type Server struct {
	App       *app.App
	Pool      *pgxpool.Pool
	Clock     *clock.Fake
	KYC       *SwitchableKYC
	Log       *slog.Logger
	AgentAddr string          // 真实网关监听地址；fake Agent 连到这里
	SSHAddr   string          // SSH 网关监听地址；/hooks/ssh 经它登录实例
	Upstream  *sshtest.Server // 假「容器 sshd」（所有 e2e 机器的 lan_ip 都是 127.0.0.1）；/hooks/ingress-upstream 在它上面登记端口

	mu            sync.Mutex
	agents        map[int64]*fakeagent.Agent
	upstreamStops map[int]func() // ingress-upstream 起的回显服务，按端口
}

// Close 停掉全部 fake Agent。
func (s *Server) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.agents {
		a.Stop()
	}
	s.agents = nil
	for _, stop := range s.upstreamStops {
		stop()
	}
	s.upstreamStops = nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]bool{"ok": true}) })
	mux.HandleFunc("GET /hooks/clock", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]any{"now": s.Clock.Now()}) })
	mux.HandleFunc("POST /hooks/clock/advance", s.handle(s.advance))
	mux.HandleFunc("POST /hooks/sweep", s.handle(s.sweep))
	mux.HandleFunc("POST /hooks/agent", s.handle(s.agent))
	mux.HandleFunc("GET /hooks/agent/status", s.agentStatus)
	mux.HandleFunc("POST /hooks/machines", s.handle(s.machines))
	mux.HandleFunc("POST /hooks/users", s.handle(s.users))
	mux.HandleFunc("POST /hooks/admins", s.handle(s.admins))
	mux.HandleFunc("POST /hooks/kyc", s.handle(s.kyc))
	mux.HandleFunc("POST /hooks/ssh", s.handle(s.ssh))
	mux.HandleFunc("POST /hooks/ingress-upstream", s.handle(s.ingressUpstream))
	return loopbackOnly(mux)
}

// loopbackOnly 只接受来自 127.0.0.1 / ::1 的连接（监听地址也是 127.0.0.1，这里是第二道保险）。
func loopbackOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type badRequest string

func (b badRequest) Error() string { return string(b) }

// handle 统一解析 JSON 请求体、输出结果或错误（400 参数错误，其余 500 并记日志）。
func (s *Server) handle(fn func(ctx context.Context, body json.RawMessage) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body json.RawMessage
		if r.ContentLength != 0 {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				writeJSON(w, 400, map[string]string{"error": "请求体必须是 JSON"})
				return
			}
		}
		if len(body) == 0 {
			body = json.RawMessage("{}")
		}
		out, err := fn(r.Context(), body)
		var br badRequest
		switch {
		case errors.As(err, &br):
			writeJSON(w, 400, map[string]string{"error": err.Error()})
		case err != nil:
			s.Log.Error("hook failed", "path", r.URL.Path, "err", err)
			writeJSON(w, 500, map[string]string{"error": err.Error()})
		default:
			writeJSON(w, 200, out)
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func decode[T any](body json.RawMessage) (T, error) {
	var v T
	if err := json.Unmarshal(body, &v); err != nil {
		return v, badRequest("请求参数格式不正确: " + err.Error())
	}
	return v, nil
}

func (s *Server) advance(_ context.Context, body json.RawMessage) (any, error) {
	req, err := decode[struct {
		Seconds int64 `json:"seconds"`
	}](body)
	if err != nil {
		return nil, err
	}
	if req.Seconds <= 0 {
		return nil, badRequest("seconds 必须为正数（假时钟只能向前）")
	}
	s.Clock.Advance(time.Duration(req.Seconds) * time.Second)
	return map[string]any{"now": s.Clock.Now()}, nil
}

const hookWait = 10 * time.Second

var fakeMetrics = metrics.Metrics{GPUUtil: 12, GPUTempC: 55, GPUPowerW: 30, MemTotal: 128 << 30, MemUsed: 8 << 30, RootDiskTotal: 4000 << 30, RootDiskUsed: 1600 << 30, Load1: 0.5}

// agentFor 返回机器的步进式 fake Agent；没有（或机器已被撤销接入）时签发令牌、接入、建流并等首个 StateReport 被处理。
func (s *Server) agentFor(ctx context.Context, machineID int64) (*fakeagent.Agent, error) {
	m, err := s.App.Fleet.Get(ctx, s.Pool, machineID)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.agents == nil {
		s.agents = map[int64]*fakeagent.Agent{}
	}
	ag, ok := s.agents[machineID]
	s.mu.Unlock()
	if ok && m.Enrolled {
		return ag, nil
	}
	if ok {
		ag.Stop()
	}
	tok, err := s.App.Fleet.IssueEnrollToken(ctx, audit.Actor{}, machineID)
	if err != nil {
		return nil, err
	}
	ag, err = fakeagent.Start(ctx, fakeagent.Options{Addr: s.AgentAddr, CAPEM: s.App.Gateway.CAPEM(), Token: tok.Plain, Stepped: true,
		IPs: []string{m.LanIP}, Metrics: fakeMetrics, Log: s.Log})
	if err != nil {
		return nil, fmt.Errorf("fake agent for machine %d: %w", machineID, err)
	}
	s.mu.Lock()
	s.agents[machineID] = ag
	s.mu.Unlock()
	if err := s.waitUntil(ctx, "first state report", func() bool { return s.App.Gateway.ReportCount(machineID) >= 1 }); err != nil {
		return nil, err
	}
	return ag, nil
}

func (s *Server) agentIDs() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]int64, 0, len(s.agents))
	for id := range s.agents {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func (s *Server) waitUntil(ctx context.Context, what string, cond func() bool) error {
	deadline := time.Now().Add(hookWait)
	for time.Now().Before(deadline) {
		if cond() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	return fmt.Errorf("timeout waiting for %s", what)
}

// heartbeatAll 让每个仍连着的 fake Agent 发一次心跳并等网关处理完（心跳走真实流，但由钩子触发，假时钟保持确定）。
// 整轮 Playwright 下来进程里会累积几百个 fake Agent：先并发发送、再统一等计数增加，避免串行几百次往返。
func (s *Server) heartbeatAll(ctx context.Context) error {
	ids := s.agentIDs()
	before := make(map[int64]int64, len(ids))
	var wg sync.WaitGroup
	errs := make(chan error, len(ids))
	for _, id := range ids {
		s.mu.Lock()
		ag := s.agents[id]
		s.mu.Unlock()
		if ag == nil || !ag.Client.Connected() { // 被撤销接入的 Agent 不再心跳
			continue
		}
		before[id] = s.App.Gateway.HeartbeatCount(id)
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			if err := ag.Client.SendHeartbeat(ctx); err != nil {
				errs <- fmt.Errorf("heartbeat %d: %w", id, err)
			}
		}(id)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		return err
	}
	for id, n := range before {
		if err := s.waitUntil(ctx, fmt.Sprintf("heartbeat %d", id), func() bool { return s.App.Gateway.HeartbeatCount(id) > n }); err != nil {
			return err
		}
	}
	return nil
}

// sweep 先让全部 fake Agent 心跳（除非 heartbeat=false），再跑一轮 Rental.Sweep + Billing.Sweep。
// Rental.Sweep 先做离线检测与暂停、再做到期停机，而到期停机只处理未暂停的实例：推进 24 小时后不心跳直接清扫，
// 机器会被判失联、实例被暂停而永远不会停机。
func (s *Server) sweep(ctx context.Context, body json.RawMessage) (any, error) {
	req, err := decode[struct {
		Heartbeat *bool `json:"heartbeat"`
	}](body)
	if err != nil {
		return nil, err
	}
	if req.Heartbeat == nil || *req.Heartbeat {
		if err := s.heartbeatAll(ctx); err != nil {
			return nil, err
		}
	}
	if err := s.App.Rental.Sweep(ctx); err != nil {
		return nil, fmt.Errorf("rental sweep: %w", err)
	}
	if err := s.App.Billing.Sweep(ctx); err != nil {
		return nil, fmt.Errorf("billing sweep: %w", err)
	}
	return map[string]bool{"ok": true}, nil
}

// agent 让步进式 fake Agent 执行当前队列：先等网关把 pending 指令投递到 Agent，再执行并上报，最后等网关处理完本次报告。
func (s *Server) agent(ctx context.Context, body json.RawMessage) (any, error) {
	req, err := decode[struct {
		MachineID *int64 `json:"machine_id"`
		Fail      bool   `json:"fail"`
		Result    string `json:"result"`
	}](body)
	if err != nil {
		return nil, err
	}
	var ids []int64
	if req.MachineID != nil {
		ids = []int64{*req.MachineID}
	} else {
		ids = s.agentIDs()
	}
	var override *state.Result
	if req.Fail {
		msg := req.Result
		if msg == "" {
			msg = "e2e: simulated failure"
		}
		override = &state.Result{OK: false, Message: msg}
	}
	completed := 0
	for _, mid := range ids {
		ag, err := s.agentFor(ctx, mid)
		if err != nil {
			return nil, err
		}
		if err := s.waitUntil(ctx, fmt.Sprintf("delivery to machine %d", mid), func() bool {
			cmds, err := s.App.Fleet.PendingCommands(ctx, mid)
			if err != nil {
				return false
			}
			for _, c := range cmds {
				if c.Status != "sent" || !ag.Client.Received(c.ID) {
					return false
				}
			}
			return true
		}); err != nil {
			return nil, err
		}
		before := s.App.Gateway.ReportCount(mid)
		n, err := ag.Client.Step(ctx, override)
		if err != nil {
			return nil, fmt.Errorf("step machine %d: %w", mid, err)
		}
		if err := s.waitUntil(ctx, fmt.Sprintf("report of machine %d", mid), func() bool { return s.App.Gateway.ReportCount(mid) > before }); err != nil {
			return nil, err
		}
		completed += n
	}
	return map[string]int{"completed": completed}, nil
}

// agentStatus 报告 fake Agent 是否仍连着；机器已被撤销接入时顺手停掉它的 Agent（否则它会以 ≤1s 退避无限重试 Unauthenticated 刷日志）。
func (s *Server) agentStatus(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.URL.Query().Get("machine_id"), 10, 64)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "machine_id 不正确"})
		return
	}
	s.mu.Lock()
	ag := s.agents[id]
	s.mu.Unlock()
	if m, err := s.App.Fleet.Get(r.Context(), s.Pool, id); err == nil && !m.Enrolled && ag != nil {
		ag.Stop()
		s.mu.Lock()
		delete(s.agents, id)
		s.mu.Unlock()
		ag = nil
	}
	connected := ag != nil && ag.Client.Connected() && s.App.Gateway.Connected(id)
	writeJSON(w, 200, map[string]any{"connected": connected})
}

func (s *Server) machines(ctx context.Context, body json.RawMessage) (any, error) {
	req, err := decode[struct {
		Count        int    `json:"count"`
		SerialPrefix string `json:"serial_prefix"`
		Rack         string `json:"rack"`
	}](body)
	if err != nil {
		return nil, err
	}
	if req.Count <= 0 || req.Count > 50 {
		return nil, badRequest("count 必须在 1～50 之间")
	}
	if req.Rack == "" {
		req.Rack = "E2E"
	}
	prefix := req.SerialPrefix
	if prefix == "" {
		// 随机 6 位在一次全量运行的几百次调用里会生日碰撞（duplicate_serial）；加进程内递增序号保证唯一
		prefix = fmt.Sprintf("E2E-%s-%d-", randx.Digits(6), machineBatch.Add(1))
	}
	pid, err := s.App.Catalog.ProductIDByCode(ctx, "spark-1")
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, req.Count)
	for i := 1; i <= req.Count; i++ {
		m, err := s.App.Fleet.Register(ctx, fleet.NewMachine{Serial: fmt.Sprintf("%s%d", prefix, i), Rack: req.Rack, Slot: fmt.Sprint(i),
			LanIP: "127.0.0.1", ProductID: pid}) // 网关与 Web 入口拨 lan_ip:UpstreamSSHPort，即进程内的假容器 sshd
		if err != nil {
			return nil, err
		}
		// Register 登记的机器处于维护中；造数据直接结束维护回到空闲（不写审计，与 dbtest.CreateMachine 一致）。
		if err := s.App.Fleet.SetMaintenance(ctx, m.ID, false); err != nil {
			return nil, err
		}
		if _, err := s.agentFor(ctx, m.ID); err != nil { // 真实接入并建流（首个 StateReport 处理完才返回）
			return nil, err
		}
		ids = append(ids, m.ID)
	}
	return map[string]any{"ids": ids}, nil
}

// users 创建用户：用户行直接插入（与 dbtest.CreateUser 相同），余额与分页流水通过 Wallet.AdminAdjust 写入。
// AdminAdjust 拒绝 60 秒内完全相同的调账，而假时钟下这 60 秒永远不会过去，所以每条流水的备注都不同。
func (s *Server) users(ctx context.Context, body json.RawMessage) (any, error) {
	req, err := decode[struct {
		Phone      string `json:"phone"`
		Verified   bool   `json:"verified"`
		Balance    int64  `json:"balance"`
		LedgerRows int    `json:"ledger_rows"`
	}](body)
	if err != nil {
		return nil, err
	}
	if req.Phone == "" {
		req.Phone = "139" + randx.Digits(8)
	}
	if req.LedgerRows < 0 || req.LedgerRows > 200 || int64(req.LedgerRows) > req.Balance {
		return nil, badRequest("ledger_rows 需在 0～200 之间且不大于 balance（分）")
	}
	kyc := "none"
	if req.Verified {
		kyc = "verified"
	}
	now := s.Clock.Now()
	var id int64
	if err := s.Pool.QueryRow(ctx, `INSERT INTO users (phone, kind, kyc_status, created_at, updated_at)
		VALUES ($1, 'personal', $2, $3, $3) RETURNING id`, req.Phone, kyc, now).Scan(&id); err != nil {
		return nil, err
	}
	for i := 1; i <= req.LedgerRows; i++ {
		if _, err := s.App.Wallet.AdminAdjust(ctx, 0, id, 1, fmt.Sprintf("e2e-seed-%d", i), "127.0.0.1"); err != nil {
			return nil, err
		}
	}
	if rest := req.Balance - int64(req.LedgerRows); rest > 0 {
		if _, err := s.App.Wallet.AdminAdjust(ctx, 0, id, rest, "e2e-seed-balance", "127.0.0.1"); err != nil {
			return nil, err
		}
	}
	return map[string]any{"id": id, "phone": req.Phone}, nil
}

// admins 创建管理员；bind_totp 为真时走命令行同款路径 EnrollTOTP → 按假时钟算码 → ConfirmEnrollment。
func (s *Server) admins(ctx context.Context, body json.RawMessage) (any, error) {
	req, err := decode[struct {
		Username string `json:"username"`
		Role     string `json:"role"`
		BindTOTP bool   `json:"bind_totp"`
	}](body)
	if err != nil {
		return nil, err
	}
	a, err := s.App.Admins.Create(ctx, audit.Actor{}, req.Username, SeedPassword, admin.Role(req.Role))
	if err != nil {
		return nil, err
	}
	out := map[string]any{"id": a.ID, "password": SeedPassword}
	if !req.BindTOTP {
		return out, nil
	}
	uri, err := s.App.Admins.EnrollTOTP(ctx, a.ID)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(uri)
	if err != nil {
		return nil, err
	}
	secret := u.Query().Get("secret")
	code, err := admin.TOTPCode(secret, admin.TOTPStep(s.Clock.Now()))
	if err != nil {
		return nil, err
	}
	codes, err := s.App.Admins.ConfirmEnrollment(ctx, a.ID, code)
	if err != nil {
		return nil, err
	}
	out["totp_secret"], out["recovery_codes"] = secret, codes
	return out, nil
}

func (s *Server) kyc(_ context.Context, body json.RawMessage) (any, error) {
	req, err := decode[struct {
		Reject bool `json:"reject"`
	}](body)
	if err != nil {
		return nil, err
	}
	s.KYC.SetReject(req.Reject)
	return map[string]bool{"ok": true}, nil
}

// ssh 用进程内 ssh 客户端经网关执行命令（验证页面上显示的密码 / 公钥真的能登录）。
// 连接或认证失败不算钩子错误：返回 200，error 为失败原因，output 里带网关的 Banner。
func (s *Server) ssh(ctx context.Context, body json.RawMessage) (any, error) {
	req, err := decode[struct {
		InstanceID string `json:"instance_id"`
		Password   string `json:"password"`
		KeyPEM     string `json:"key_pem"`
		Command    string `json:"command"`
	}](body)
	if err != nil {
		return nil, err
	}
	if s.SSHAddr == "" {
		return nil, errors.New("ssh gateway not configured")
	}
	var auth ssh.AuthMethod
	if req.KeyPEM != "" {
		signer, err := ssh.ParsePrivateKey([]byte(req.KeyPEM))
		if err != nil {
			return nil, badRequest("key_pem 无法解析: " + err.Error())
		}
		auth = ssh.PublicKeys(signer)
	} else {
		auth = ssh.Password(req.Password)
	}
	var banner strings.Builder
	d := net.Dialer{Timeout: 5 * time.Second}
	nc, err := d.DialContext(ctx, "tcp", s.SSHAddr)
	if err != nil {
		return map[string]any{"output": "", "exit_status": -1, "error": err.Error()}, nil
	}
	_ = nc.SetDeadline(time.Now().Add(hookWait))
	conn, chans, reqs, err := ssh.NewClientConn(nc, s.SSHAddr, &ssh.ClientConfig{User: req.InstanceID, Auth: []ssh.AuthMethod{auth},
		HostKeyCallback: ssh.FixedHostKey(s.App.SSHHostKey.PublicKey()),
		BannerCallback:  func(m string) error { banner.WriteString(m); return nil }})
	if err != nil {
		nc.Close()
		return map[string]any{"output": banner.String(), "exit_status": -1, "error": err.Error()}, nil
	}
	cl := ssh.NewClient(conn, chans, reqs)
	defer cl.Close()
	sess, err := cl.NewSession()
	if err != nil {
		return map[string]any{"output": banner.String(), "exit_status": -1, "error": err.Error()}, nil
	}
	defer sess.Close()
	var out, stderr bytes.Buffer // 分开收：ssh 包用两个 goroutine 分别拷贝 stdout / stderr
	sess.Stdout, sess.Stderr = &out, &stderr
	status, errText := 0, ""
	var exitErr *ssh.ExitError
	if err := sess.Run(req.Command); errors.As(err, &exitErr) {
		status = exitErr.ExitStatus()
	} else if err != nil {
		status, errText = -1, err.Error()
	}
	return map[string]any{"output": banner.String() + out.String() + stderr.String(), "exit_status": status, "error": errText}, nil
}

// ingressUpstream 让假容器 sshd 把到容器内 127.0.0.1:<port> 的 direct-tcpip 转到一个回显服务：
// 响应体形如 `body|host=<Host>|path=<RequestURI>`（断言域名与路径原样透传）。
// 之后立即刷新入口路由表：用例先经 API 加端口、再调本钩子，就不必等 2 秒的定时刷新。
func (s *Server) ingressUpstream(ctx context.Context, body json.RawMessage) (any, error) {
	req, err := decode[struct {
		Port int    `json:"port"`
		Body string `json:"body"`
	}](body)
	if err != nil {
		return nil, err
	}
	if req.Port <= 0 || req.Port > 65535 {
		return nil, badRequest("port 不正确")
	}
	if s.Upstream == nil {
		return nil, errors.New("fake upstream not configured")
	}
	// 同一端口再次登记：先关掉上一个回显服务（它的 stop 会删掉该端口的目标，必须在登记新服务之前调用）
	s.mu.Lock()
	prev := s.upstreamStops[req.Port]
	s.mu.Unlock()
	if prev != nil {
		prev()
	}
	s.mu.Lock()
	if s.upstreamStops == nil {
		s.upstreamStops = map[int]func(){}
	}
	s.upstreamStops[req.Port] = s.Upstream.ServeHTTP(req.Port, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "%s|host=%s|path=%s", req.Body, r.Host, r.URL.RequestURI())
	}))
	s.mu.Unlock()
	if err := s.App.Routes.Refresh(ctx); err != nil {
		return nil, fmt.Errorf("refresh routes: %w", err)
	}
	return map[string]bool{"ok": true}, nil
}
