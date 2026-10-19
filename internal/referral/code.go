package referral

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"

	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/db"
)

// newCode 是 EnsureCode 使用的码生成器；测试经 export_test.go 替换以制造撞码。
var newCode = GenerateCode

// GenerateCode 生成 8 位 Crockford Base32 随机码（crypto/rand）。
func GenerateCode() string {
	b := make([]byte, CodeLen)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand 失败意味着系统熵源损坏
	}
	out := make([]byte, CodeLen)
	for i, x := range b {
		out[i] = CodeAlphabet[int(x)%len(CodeAlphabet)]
	}
	return string(out)
}

// NormalizeCode 去空白、转大写、O→0、I/L→1；长度不是 8 或含字母表外字符返回 ok=false。
func NormalizeCode(s string) (string, bool) {
	s = strings.ToUpper(strings.Join(strings.Fields(s), ""))
	s = strings.NewReplacer("O", "0", "I", "1", "L", "1").Replace(s)
	if len(s) != CodeLen {
		return "", false
	}
	for i := 0; i < len(s); i++ {
		if !strings.ContainsRune(CodeAlphabet, rune(s[i])) {
			return "", false
		}
	}
	return s, true
}

// MaskPhone 138****5678；非 11 位时保留前 3 位 + ****。
func MaskPhone(phone string) string {
	if len(phone) == 11 {
		return phone[:3] + "****" + phone[7:]
	}
	if len(phone) > 3 {
		phone = phone[:3]
	}
	return phone + "****"
}

// EnsureCode 返回用户的邀请账户，没有则生成码并插入（撞码重试 CodeRetries 次）；并发调用只会建一行。
func (s *Service) EnsureCode(ctx context.Context, userID int64) (Account, error) {
	a, err := scanAccount(s.d.Pool.QueryRow(ctx, `SELECT `+accountCols+` FROM referral_accounts WHERE user_id = $1`, userID))
	if err == nil {
		return a, nil
	}
	if !db.IsNoRows(err) {
		return a, err
	}
	now := s.d.Clock.Now()
	for i := 0; i < CodeRetries; i++ {
		// ON CONFLICT (user_id) DO NOTHING 处理并发的首次调用；撞到 code 唯一索引仍会报错 → 换码重试
		_, err := s.d.Pool.Exec(ctx, `INSERT INTO referral_accounts (user_id, code, created_at, updated_at) VALUES ($1, $2, $3, $3)
			ON CONFLICT (user_id) DO NOTHING`, userID, newCode(), now)
		if err != nil {
			if db.IsUniqueViolation(err) {
				continue
			}
			if db.IsForeignKeyViolation(err) {
				return a, apperr.ErrNotFound
			}
			return a, err
		}
		return scanAccount(s.d.Pool.QueryRow(ctx, `SELECT `+accountCols+` FROM referral_accounts WHERE user_id = $1`, userID))
	}
	return a, fmt.Errorf("referral: invite code collided %d times for user %d", CodeRetries, userID)
}

// LookupCode 匿名查码：码存在、账户未冻结、码主未封禁才有效；无效不区分原因。
func (s *Service) LookupCode(ctx context.Context, code string) (bool, string, error) {
	c, ok := NormalizeCode(code)
	if !ok {
		return false, "", nil
	}
	var phone string
	err := s.d.Pool.QueryRow(ctx, `SELECT u.phone FROM referral_accounts a JOIN users u ON u.id = a.user_id
		WHERE a.code = $1 AND a.status = 'active' AND u.status = 'active'`, c).Scan(&phone)
	if db.IsNoRows(err) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	return true, MaskPhone(phone), nil
}
