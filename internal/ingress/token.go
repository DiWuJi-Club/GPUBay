package ingress

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"spark-cloud/internal/platform/clock"
)

const (
	TicketTTL = 60 * time.Second
	CookieTTL = 12 * time.Hour
)

// ErrToken：票据 / Cookie 验签失败、过期或已使用。
var ErrToken = errors.New("ingress: invalid token")

// Claims 是票据与 Cookie 的内容（JSON 编码后签名；Cookie 不带 Nonce）。
type Claims struct {
	InstanceID  string `json:"i"`
	Port        int    `json:"p"`
	UserID      int64  `json:"u"`
	SessionHash string `json:"s"`
	Exp         int64  `json:"e"`
	Nonce       string `json:"n,omitempty"`
}

// Signer 用 SPARK_SECRET_KEY 经 HKDF 派生两把 HMAC-SHA256 密钥：票据（60 秒、进程内单次）与 Cookie（12 小时、可重复）。
// 两把密钥不同，票据不能当 Cookie 用，反之亦然。
type Signer struct {
	ticketKey []byte
	cookieKey []byte
	clk       clock.Clock
	mu        sync.Mutex
	used      map[string]time.Time // 已用 nonce → 票据过期时间；过期即删除，大小以 60 秒内签发的票据数为上限
}

func derive(secret []byte, info string) []byte {
	key, err := hkdf.Key(sha256.New, secret, nil, info, 32)
	if err != nil {
		panic(err) // 只在请求长度超限时出错，32 字节不会
	}
	return key
}

func NewSigner(secret []byte, clk clock.Clock) *Signer {
	return &Signer{ticketKey: derive(secret, "ingress-ticket"), cookieKey: derive(secret, "ingress-cookie"), clk: clk, used: map[string]time.Time{}}
}

func sign(key []byte, c Claims) string {
	body, _ := json.Marshal(c)
	mac := hmac.New(sha256.New, key)
	mac.Write(body)
	return base64.RawURLEncoding.EncodeToString(body) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func verify(key []byte, tok string, now time.Time) (Claims, error) {
	i := strings.IndexByte(tok, '.')
	if i <= 0 {
		return Claims{}, ErrToken
	}
	body, err := base64.RawURLEncoding.DecodeString(tok[:i])
	if err != nil {
		return Claims{}, ErrToken
	}
	sig, err := base64.RawURLEncoding.DecodeString(tok[i+1:])
	if err != nil {
		return Claims{}, ErrToken
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(body)
	if !hmac.Equal(sig, mac.Sum(nil)) { // 常数时间比较
		return Claims{}, ErrToken
	}
	var c Claims
	if err := json.Unmarshal(body, &c); err != nil || c.InstanceID == "" || c.Port <= 0 || c.UserID <= 0 || c.SessionHash == "" || now.Unix() >= c.Exp {
		return Claims{}, ErrToken
	}
	return c, nil
}

func (s *Signer) Ticket(c Claims) string {
	nonce := make([]byte, 16)
	rand.Read(nonce)
	c.Exp = s.clk.Now().Add(TicketTTL).Unix()
	c.Nonce = base64.RawURLEncoding.EncodeToString(nonce)
	return sign(s.ticketKey, c)
}

// VerifyTicket 验签 + 未过期 + nonce 未用过。用过的 nonce 保留到票据过期（之后票据本身已因过期被拒），过期的 nonce 每次验证时清理。
func (s *Signer) VerifyTicket(tok string) (Claims, error) {
	now := s.clk.Now()
	c, err := verify(s.ticketKey, tok, now)
	if err != nil || c.Nonce == "" {
		return Claims{}, ErrToken
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for n, exp := range s.used {
		if !now.Before(exp) {
			delete(s.used, n)
		}
	}
	if _, dup := s.used[c.Nonce]; dup {
		return Claims{}, ErrToken
	}
	s.used[c.Nonce] = time.Unix(c.Exp, 0)
	return c, nil
}

func (s *Signer) Cookie(c Claims) string {
	c.Exp = s.clk.Now().Add(CookieTTL).Unix()
	c.Nonce = ""
	return sign(s.cookieKey, c)
}

func (s *Signer) VerifyCookie(tok string) (Claims, error) {
	c, err := verify(s.cookieKey, tok, s.clk.Now())
	if err != nil || c.Nonce != "" {
		return Claims{}, ErrToken
	}
	return c, nil
}
