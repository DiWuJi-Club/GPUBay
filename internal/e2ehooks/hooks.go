// Package e2ehooks 为 cmd/e2e-server 提供端到端测试钩子：推进假时钟、模拟 Agent、造数据、切换实名 mock。
// 只被 cmd/e2e-server 引用，生产二进制不包含它。所有钩子用 app.App 暴露的服务实现，不复制业务逻辑。
package e2ehooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"spark-cloud/internal/admin"
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
	App   *app.App
	Pool  *pgxpool.Pool
	Clock *clock.Fake
	KYC   *SwitchableKYC
	Log   *slog.Logger
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]bool{"ok": true}) })
	mux.HandleFunc("GET /hooks/clock", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]any{"now": s.Clock.Now()}) })
	mux.HandleFunc("POST /hooks/clock/advance", s.handle(s.advance))
	mux.HandleFunc("POST /hooks/sweep", s.handle(s.sweep))
	mux.HandleFunc("POST /hooks/agent", s.handle(s.agent))
	mux.HandleFunc("POST /hooks/machines", s.handle(s.machines))
	mux.HandleFunc("POST /hooks/users", s.handle(s.users))
	mux.HandleFunc("POST /hooks/admins", s.handle(s.admins))
	mux.HandleFunc("POST /hooks/kyc", s.handle(s.kyc))
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

func (s *Server) machineIDs(ctx context.Context) ([]int64, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id FROM machines ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Server) heartbeatAll(ctx context.Context) error {
	ids, err := s.machineIDs(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := s.App.Fleet.Heartbeat(ctx, id); err != nil {
			return fmt.Errorf("heartbeat %d: %w", id, err)
		}
	}
	return nil
}

// sweep 先对全部机器心跳（除非 heartbeat=false），再跑一轮 Rental.Sweep + Billing.Sweep。
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

// agent 模拟 Agent：对指定机器（缺省全部）心跳，并把全部待处理指令按成功 / 失败回报。
func (s *Server) agent(ctx context.Context, body json.RawMessage) (any, error) {
	req, err := decode[struct {
		MachineID *int64 `json:"machine_id"`
		Fail      bool   `json:"fail"`
		Result    string `json:"result"`
	}](body)
	if err != nil {
		return nil, err
	}
	ids := []int64{}
	if req.MachineID != nil {
		ids = append(ids, *req.MachineID)
	} else if ids, err = s.machineIDs(ctx); err != nil {
		return nil, err
	}
	result := req.Result
	if result == "" {
		if req.Fail {
			result = "e2e: simulated failure"
		} else {
			result = "ok"
		}
	}
	completed := 0
	for _, mid := range ids {
		if _, err := s.App.Fleet.Heartbeat(ctx, mid); err != nil {
			return nil, fmt.Errorf("heartbeat %d: %w", mid, err)
		}
		cmds, err := s.App.Fleet.PendingCommands(ctx, mid)
		if err != nil {
			return nil, err
		}
		for _, c := range cmds {
			if err := s.App.Rental.HandleCommandResult(ctx, c.ID, !req.Fail, result); err != nil {
				return nil, fmt.Errorf("command %s (%s): %w", c.ID, c.Type, err)
			}
			completed++
		}
	}
	return map[string]int{"completed": completed}, nil
}

func (s *Server) machines(ctx context.Context, body json.RawMessage) (any, error) {
	req, err := decode[struct {
		Count        int    `json:"count"`
		SerialPrefix string `json:"serial_prefix"`
	}](body)
	if err != nil {
		return nil, err
	}
	if req.Count <= 0 || req.Count > 50 {
		return nil, badRequest("count 必须在 1～50 之间")
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
		m, err := s.App.Fleet.Register(ctx, fleet.NewMachine{Serial: fmt.Sprintf("%s%d", prefix, i), Rack: "E2E", Slot: fmt.Sprint(i),
			LanIP: fmt.Sprintf("10.99.%d.%d", (len(ids)/250)%250, len(ids)%250+1), ProductID: pid})
		if err != nil {
			return nil, err
		}
		// Register 登记的机器处于维护中；造数据直接结束维护回到空闲（不写审计，与 dbtest.CreateMachine 一致）。
		if err := s.App.Fleet.SetMaintenance(ctx, m.ID, false); err != nil {
			return nil, err
		}
		if _, err := s.App.Fleet.Heartbeat(ctx, m.ID); err != nil {
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
