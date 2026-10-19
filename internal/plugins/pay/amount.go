package pay

import (
	"fmt"
	"regexp"
	"strconv"
)

// 支付宝金额为「元」字符串，最多两位小数；用整数解析，不经过浮点。
var yuanRe = regexp.MustCompile(`^(\d{1,10})(?:\.(\d{1,2}))?$`)

func yuanToFen(s string) (int64, error) {
	m := yuanRe.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("pay: 金额格式不正确 %q", s)
	}
	yuan, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("pay: 金额格式不正确 %q", s)
	}
	frac := m[2]
	for len(frac) < 2 {
		frac += "0"
	}
	fen, _ := strconv.ParseInt(frac, 10, 64)
	return yuan*100 + fen, nil
}

func fenToYuan(fen int64) string { return fmt.Sprintf("%d.%02d", fen/100, fen%100) }
