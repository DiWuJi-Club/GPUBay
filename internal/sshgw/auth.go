package sshgw

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"golang.org/x/crypto/ssh"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/rental"
)

const (
	BannerUnknown          = "用户名应为实例 ID，例如 ins-a1b2c3d4"
	BannerSuspended        = "实例所在机器暂时失联，计时已暂停，请稍后重试"
	BannerCreating         = "实例正在开通，请稍后再试"
	BannerStoppedFmt       = "实例已到期停机，数据保留至 %s，续费后可继续使用"
	BannerReleased         = "实例已释放"
	MsgTooManySessions     = "连接数过多"
	MsgUpstreamUnreachable = "无法连接到实例，请稍后重试"
	MsgDisconnected        = "实例已停机 / 已释放，连接已断开"
	MsgPasswordAuthOff     = "实例已关闭密码登录，请使用公钥登录"
)

var (
	instanceIDRe = regexp.MustCompile(`^ins-[a-z0-9]{8}$`)
	errAuth      = errors.New("authentication failed")
	cst          = time.FixedZone("CST", 8*3600)
)

const lookupTimeout = 5 * time.Second

// lookup 按用户名查实例；用户名不合法或实例不存在返回 ok=false（二者对客户端不可区分）。
func (s *Server) lookup(user string) (rental.AccessInfo, bool) {
	info, ok, _ := s.lookupDefinitive(user)
	return info, ok
}

// lookupDefinitive 同 lookup；definitive=false 表示数据库瞬时错误（结果不可缓存）。
func (s *Server) lookupDefinitive(user string) (info rental.AccessInfo, ok, definitive bool) {
	if !instanceIDRe.MatchString(user) {
		return rental.AccessInfo{}, false, true
	}
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()
	info, err := s.d.Store.Access(ctx, user)
	if err != nil {
		if !errors.Is(err, apperr.ErrNotFound) {
			s.d.Log.Error("ssh access lookup", "instance", user, "err", err)
			return rental.AccessInfo{}, false, false
		}
		return rental.AccessInfo{}, false, true
	}
	return info, true, true
}

// connLookup：握手期间（banner 与各次 password / publickey 回调）按连接缓存 lookup 结果，
// 同一连接同一用户名只查一次库、只解密一次实例密码（OpenSSH 会把 agent 里每把钥匙各试一次）。
// 缓存只活到握手结束（handleConn 随后清空）；认证通过后 serve 仍会重新 lookup 复核状态。
// 数据库瞬时错误不缓存：下一次回调重查。
func (s *Server) connLookup(c ssh.ConnMetadata) (rental.AccessInfo, bool) {
	v, found := s.pending.Load(c.RemoteAddr().String())
	if !found {
		return s.lookup(c.User())
	}
	a := v.(*attempt)
	user := c.User()
	a.mu.Lock()
	if a.cached && a.cachedUser == user {
		info, ok := a.cachedInfo, a.cachedOK
		a.mu.Unlock()
		return info, ok
	}
	a.mu.Unlock()
	info, ok, definitive := s.lookupDefinitive(user)
	if definitive {
		a.mu.Lock()
		a.cached, a.cachedUser, a.cachedInfo, a.cachedOK = true, user, info, ok
		a.mu.Unlock()
	}
	return info, ok
}

// allowed：只有 running 且未暂停的实例可以认证。
func allowed(info rental.AccessInfo) bool {
	return info.Instance.State == rental.StateRunning && !info.Instance.Suspended()
}

// stateBanner 返回认证前给用户看的说明；允许连接时为空。
func stateBanner(info rental.AccessInfo) string {
	inst := info.Instance
	switch {
	case inst.State == rental.StateRunning && inst.Suspended():
		return BannerSuspended
	case inst.State == rental.StateRunning:
		return ""
	case inst.State == rental.StateCreating:
		return BannerCreating
	case inst.State == rental.StateStopped:
		until := "—"
		if info.RetainUntil != nil {
			until = info.RetainUntil.In(cst).Format("2006-01-02 15:04")
		}
		return fmt.Sprintf(BannerStoppedFmt, until)
	default:
		return BannerReleased
	}
}

func (s *Server) banner(c ssh.ConnMetadata) string {
	info, ok := s.connLookup(c)
	if !ok {
		return BannerUnknown + "\r\n"
	}
	if b := stateBanner(info); b != "" {
		return b + "\r\n"
	}
	return ""
}

func perms(info rental.AccessInfo, method string, keyID int64) *ssh.Permissions {
	return &ssh.Permissions{Extensions: map[string]string{
		"instance": info.Instance.ID, "user_id": strconv.FormatInt(info.Instance.UserID, 10), "method": method, "key_id": strconv.FormatInt(keyID, 10),
	}}
}

func (s *Server) passwordCallback(c ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
	info, ok := s.connLookup(c)
	if ok && !allowed(info) {
		return nil, errAuth // 不校验凭据、猜也不可能成功：不计失败（停机实例的 IDE 反复重连不能封用户 IP）
	}
	if ok && !info.Instance.PasswordAuth {
		s.d.Log.Info("ssh password refused: password_auth disabled", "instance", info.Instance.ID) // 同上不计失败
		return nil, errAuth
	}
	s.noteAttempt(c, ok)
	if !ok {
		return nil, errAuth
	}
	// 先各取 SHA-256 再比较：ConstantTimeCompare 在长度不同时会提前返回，摘要定长可避免泄露密码长度
	want, got := sha256.Sum256([]byte(info.Password)), sha256.Sum256(pw)
	if info.Password == "" || subtle.ConstantTimeCompare(want[:], got[:]) != 1 {
		return nil, errAuth
	}
	return perms(info, "password", 0), nil
}

func (s *Server) publicKeyCallback(c ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
	info, ok := s.connLookup(c)
	if ok && !allowed(info) {
		return nil, errAuth // 同上：不可登录状态下不查钥匙、不计失败
	}
	s.noteAttempt(c, ok)
	if !ok {
		return nil, errAuth
	}
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()
	k, err := s.d.Keys.FindSSHKey(ctx, info.Instance.UserID, ssh.FingerprintSHA256(key))
	if err != nil {
		return nil, errAuth
	}
	return perms(info, "publickey", k.ID), nil
}
