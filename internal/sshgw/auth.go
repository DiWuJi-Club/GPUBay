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
)

var (
	instanceIDRe = regexp.MustCompile(`^ins-[a-z0-9]{8}$`)
	errAuth      = errors.New("authentication failed")
	cst          = time.FixedZone("CST", 8*3600)
)

const lookupTimeout = 5 * time.Second

// lookup 按用户名查实例；用户名不合法或实例不存在返回 ok=false（二者对客户端不可区分）。
func (s *Server) lookup(user string) (rental.AccessInfo, bool) {
	if !instanceIDRe.MatchString(user) {
		return rental.AccessInfo{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()
	info, err := s.d.Store.Access(ctx, user)
	if err != nil {
		if !errors.Is(err, apperr.ErrNotFound) {
			s.d.Log.Error("ssh access lookup", "instance", user, "err", err)
		}
		return rental.AccessInfo{}, false
	}
	return info, true
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
	s.noteUser(c)
	info, ok := s.lookup(c.User())
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
	s.noteUser(c)
	info, ok := s.lookup(c.User())
	if !ok || !allowed(info) || !info.Instance.PasswordAuth {
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
	s.noteUser(c)
	info, ok := s.lookup(c.User())
	if !ok || !allowed(info) {
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
