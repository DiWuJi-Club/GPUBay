package rental

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"spark-cloud/internal/audit"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/paging"
	"spark-cloud/internal/platform/randx"
)

const (
	SSHSessionStaleAfter = 5 * time.Minute     // last_seen_at 超过此值仍未结束的会话由清扫器收尾
	SSHSessionRetention  = 90 * 24 * time.Hour // 已结束会话的保留期
)

// ResetPassword 重新生成实例 SSH 密码（released / failed 不允许），返回明文。进行中的会话不受影响。
func (s *Service) ResetPassword(ctx context.Context, userID int64, id, ip string) (string, error) {
	now := s.d.Clock.Now()
	pw := randx.Password(16)
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		inst, err := lockInstance(ctx, tx, id)
		if err != nil {
			return err
		}
		if inst.UserID != userID {
			return apperr.ErrNotFound
		}
		if inst.State == StateReleased || inst.State == StateFailed {
			return apperr.ErrInvalidState
		}
		if _, err := tx.Exec(ctx, `UPDATE instances SET ssh_password_enc = $2, updated_at = $3 WHERE id = $1`, id, s.d.Box.Seal([]byte(pw)), now); err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, audit.Actor{UserID: userID, IP: ip}.Entry("instance.password_reset", "instance", id, nil))
	})
	if err != nil {
		return "", err
	}
	return pw, nil
}

// SetPasswordAuth 打开 / 关闭实例的密码登录；关闭要求用户至少有一把公钥。
func (s *Service) SetPasswordAuth(ctx context.Context, userID int64, id string, enabled bool, ip string) (Instance, error) {
	now := s.d.Clock.Now()
	var out Instance
	err := db.WithTx(ctx, s.d.Pool, func(tx pgx.Tx) error {
		inst, err := lockInstance(ctx, tx, id)
		if err != nil {
			return err
		}
		if inst.UserID != userID {
			return apperr.ErrNotFound
		}
		if inst.State == StateReleased || inst.State == StateFailed {
			return apperr.ErrInvalidState
		}
		if !enabled {
			n := 0
			if s.d.Keys != nil {
				if n, err = s.d.Keys.CountSSHKeys(ctx, userID); err != nil {
					return err
				}
			}
			if n == 0 {
				return apperr.ErrPasswordAuthRequiresKey
			}
		}
		out, err = scanInstance(tx.QueryRow(ctx, `UPDATE instances SET password_auth = $2, updated_at = $3 WHERE id = $1 RETURNING `+instanceCols, id, enabled, now))
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, now, audit.Actor{UserID: userID, IP: ip}.Entry("instance.password_auth", "instance", id, map[string]any{"enabled": enabled}))
	})
	return out, err
}

// ErrHostKeyUndecryptable：实例主机密钥密文无法解密（数据损坏或密钥轮换错误），重试无意义。
var ErrHostKeyUndecryptable = errors.New("实例主机密钥无法解密")

// HostKeyPEM 解密返回实例容器的主机私钥（网关投递 Create 指令时填入 host_key_pem）；已清空返回空串。
func (s *Service) HostKeyPEM(ctx context.Context, instanceID string) (string, error) {
	var enc []byte
	err := s.d.Pool.QueryRow(ctx, `SELECT ssh_host_key_enc FROM instances WHERE id = $1`, instanceID).Scan(&enc)
	if db.IsNoRows(err) {
		return "", apperr.ErrNotFound
	}
	if err != nil || enc == nil {
		return "", err
	}
	plain, err := s.d.Box.Open(enc)
	if err != nil {
		return "", fmt.Errorf("%w: 实例 %s: %v", ErrHostKeyUndecryptable, instanceID, err)
	}
	return string(plain), nil
}

// SetSSHHostPubkeyIfEmpty 存量实例首连信任：只在公钥为空时写入，返回是否写入。
func (s *Service) SetSSHHostPubkeyIfEmpty(ctx context.Context, id, pub string) (bool, error) {
	tag, err := s.d.Pool.Exec(ctx, `UPDATE instances SET ssh_host_pubkey = $2, updated_at = $3 WHERE id = $1 AND ssh_host_pubkey = ''`, id, pub, s.d.Clock.Now())
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// SSHHostPubkey 返回实例登记的主机公钥（authorized_keys 一行，未登记为空串）；首连信任竞争失败后复读用。
func (s *Service) SSHHostPubkey(ctx context.Context, id string) (string, error) {
	var pub string
	err := s.d.Pool.QueryRow(ctx, `SELECT ssh_host_pubkey FROM instances WHERE id = $1`, id).Scan(&pub)
	if db.IsNoRows(err) {
		return "", apperr.ErrNotFound
	}
	return pub, err
}

// AccessInfo 是 SSH 网关 / Web 入口判定与拨号所需的全部信息。
type AccessInfo struct {
	Instance      Instance
	Password      string // 明文（只在认证时比较）
	MachineLanIP  string
	MachineSerial string
	AgentOnline   bool
	RetainUntil   *time.Time // stopped 时的数据保留截止
}

// LogValue 在 slog 中隐藏明文密码。
func (a AccessInfo) LogValue() slog.Value {
	a.Password = "[REDACTED]"
	type plain AccessInfo // 去掉方法，避免递归
	return slog.AnyValue(plain(a))
}

func (s *Service) Access(ctx context.Context, instanceID string) (AccessInfo, error) {
	var info AccessInfo
	var enc []byte
	// 先按 instanceCols 取实例，再单独取机器：JOIN 会使 id / state / created_at 等列同名歧义。
	inst, err := s.Get(ctx, instanceID)
	if err != nil {
		return AccessInfo{}, err
	}
	info.Instance = inst
	i := &info.Instance
	if err := s.d.Pool.QueryRow(ctx, `SELECT ssh_password_enc FROM instances WHERE id = $1`, instanceID).Scan(&enc); err != nil {
		if db.IsNoRows(err) {
			return AccessInfo{}, apperr.ErrNotFound
		}
		return AccessInfo{}, err
	}
	if err := s.d.Pool.QueryRow(ctx, `SELECT lan_ip, serial, agent_online FROM machines WHERE id = $1`, inst.MachineID).
		Scan(&info.MachineLanIP, &info.MachineSerial, &info.AgentOnline); err != nil {
		if db.IsNoRows(err) {
			return AccessInfo{}, apperr.ErrNotFound
		}
		return AccessInfo{}, err
	}
	plain, err := s.d.Box.Open(enc)
	if err != nil {
		return AccessInfo{}, fmt.Errorf("解密实例 %s 密码: %w", instanceID, err)
	}
	info.Password = string(plain)
	if i.State == StateStopped && i.StoppedAt != nil && i.PendingSeconds == 0 {
		t := i.StoppedAt.Add(RetainAfterStop)
		info.RetainUntil = &t
	}
	return info, nil
}

// SessionState 是 SSH 网关周期扫描所需的实例状态（不含密码等需解密的字段）。
type SessionState struct {
	State     string
	Suspended bool
}

// SessionStates 一次查询返回多个实例的状态；不存在的 ID 不出现在结果里。
func (s *Service) SessionStates(ctx context.Context, ids []string) (map[string]SessionState, error) {
	out := make(map[string]SessionState, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.d.Pool.Query(ctx, `SELECT i.id, i.state, i.suspended_at IS NOT NULL FROM instances i WHERE i.id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var st SessionState
		if err := rows.Scan(&id, &st.State, &st.Suspended); err != nil {
			return nil, err
		}
		out[id] = st
	}
	return out, rows.Err()
}

// ---- SSH 会话记录 ----

type SSHSessionStart struct {
	InstanceID string
	UserID     int64
	MachineID  int64
	AuthMethod string // password / publickey
	KeyID      *int64
	ClientIP   string
}

type SSHSession struct {
	ID         int64      `json:"id"`
	InstanceID string     `json:"instance_id"`
	UserID     int64      `json:"user_id"`
	MachineID  int64      `json:"machine_id"`
	AuthMethod string     `json:"auth_method"`
	KeyID      *int64     `json:"key_id"`
	ClientIP   string     `json:"client_ip"`
	BytesUp    int64      `json:"bytes_up"`
	BytesDown  int64      `json:"bytes_down"`
	StartedAt  time.Time  `json:"started_at"`
	LastSeenAt time.Time  `json:"last_seen_at"`
	EndedAt    *time.Time `json:"ended_at"`
}

const sshSessionCols = `id, instance_id, user_id, machine_id, auth_method, key_id, client_ip, bytes_up, bytes_down, started_at, last_seen_at, ended_at`

func scanSSHSession(row pgx.Row) (SSHSession, error) {
	var x SSHSession
	err := row.Scan(&x.ID, &x.InstanceID, &x.UserID, &x.MachineID, &x.AuthMethod, &x.KeyID, &x.ClientIP, &x.BytesUp, &x.BytesDown, &x.StartedAt, &x.LastSeenAt, &x.EndedAt)
	return x, err
}

func (s *Service) OpenSSHSession(ctx context.Context, st SSHSessionStart) (int64, error) {
	now := s.d.Clock.Now()
	var id int64
	err := s.d.Pool.QueryRow(ctx, `INSERT INTO ssh_sessions (instance_id, user_id, machine_id, auth_method, key_id, client_ip, started_at, last_seen_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $7) RETURNING id`, st.InstanceID, st.UserID, st.MachineID, st.AuthMethod, st.KeyID, st.ClientIP, now).Scan(&id)
	return id, err
}

func (s *Service) TouchSSHSessions(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := s.d.Pool.Exec(ctx, `UPDATE ssh_sessions SET last_seen_at = $2 WHERE id = ANY($1) AND ended_at IS NULL`, ids, s.d.Clock.Now())
	return err
}

func (s *Service) CloseSSHSession(ctx context.Context, id, up, down int64) error {
	now := s.d.Clock.Now()
	_, err := s.d.Pool.Exec(ctx, `UPDATE ssh_sessions SET ended_at = $2, last_seen_at = $2, bytes_up = $3, bytes_down = $4 WHERE id = $1 AND ended_at IS NULL`, id, now, up, down)
	return err
}

func (s *Service) ListSSHSessions(ctx context.Context, instanceID string, limit int) ([]SSHSession, error) {
	rows, err := s.d.Pool.Query(ctx, `SELECT `+sshSessionCols+` FROM ssh_sessions WHERE instance_id = $1 ORDER BY started_at DESC, id DESC LIMIT $2`, instanceID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (SSHSession, error) { return scanSSHSession(r) })
}

func (s *Service) AdminListSSHSessions(ctx context.Context, instanceID string, p paging.Page) ([]SSHSession, int, error) {
	var total int
	if err := s.d.Pool.QueryRow(ctx, `SELECT count(*) FROM ssh_sessions WHERE instance_id = $1`, instanceID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.d.Pool.Query(ctx, `SELECT `+sshSessionCols+` FROM ssh_sessions WHERE instance_id = $1 ORDER BY started_at DESC, id DESC LIMIT $2 OFFSET $3`,
		instanceID, p.Limit(), p.Offset())
	if err != nil {
		return nil, 0, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (SSHSession, error) { return scanSSHSession(r) })
	return list, total, err
}

func (s *Service) OpenSSHSessionCount(ctx context.Context) (int, error) {
	var n int
	err := s.d.Pool.QueryRow(ctx, `SELECT count(*) FROM ssh_sessions WHERE ended_at IS NULL`).Scan(&n)
	return n, err
}

// ReapSSHSessions：收尾超过 SSHSessionStaleAfter 未刷新的会话（副本崩溃 / 重建改名），删除超过保留期的已结束会话。
func (s *Service) ReapSSHSessions(ctx context.Context) (closed, deleted int, err error) {
	now := s.d.Clock.Now()
	tag, err := s.d.Pool.Exec(ctx, `UPDATE ssh_sessions SET ended_at = last_seen_at WHERE ended_at IS NULL AND last_seen_at < $1`, now.Add(-SSHSessionStaleAfter))
	if err != nil {
		return 0, 0, err
	}
	closed = int(tag.RowsAffected())
	tag, err = s.d.Pool.Exec(ctx, `DELETE FROM ssh_sessions WHERE ended_at IS NOT NULL AND ended_at < $1`, now.Add(-SSHSessionRetention))
	if err != nil {
		return closed, 0, err
	}
	return closed, int(tag.RowsAffected()), nil
}

// ---- 告警（文案在 alerts.go，稳定、无数值）----

func (s *Service) RaiseSSHBruteforce(ctx context.Context, instanceID string) error {
	inst, err := s.Get(ctx, instanceID)
	if err != nil {
		return err
	}
	return s.d.Fleet.RaiseAlertOnce(ctx, s.d.Pool, fleet.Alert{Level: "warning", Kind: "ssh_bruteforce",
		MachineID: inst.MachineID, InstanceID: instanceID, Message: AlertSSHBruteforce(instanceID)})
}

func (s *Service) RaiseUpstreamUnreachable(ctx context.Context, instanceID string) error {
	inst, err := s.Get(ctx, instanceID)
	if err != nil {
		return err
	}
	m, err := s.d.Fleet.Get(ctx, s.d.Pool, inst.MachineID)
	if err != nil {
		return err
	}
	return s.d.Fleet.RaiseAlertOnce(ctx, s.d.Pool, fleet.Alert{Level: "warning", Kind: "upstream_unreachable",
		MachineID: inst.MachineID, InstanceID: instanceID, Message: AlertUpstreamUnreachable(m.Serial, instanceID)})
}
