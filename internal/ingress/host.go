// Package ingress 是租户 Web 端口的入口：按 Host（ins-<id>-<port>.<ingress_domain>）分流，私有端口用票据 + 每实例 Cookie 鉴权，
// 请求经容器 sshd 的 direct-tcpip 通道转发到容器内 127.0.0.1:<port>。
package ingress

import (
	"net"
	"regexp"
	"strconv"
	"strings"
)

// 端口不允许前导零（同一端口只有一种写法），1～5 位，范围另校验。
var hostRe = regexp.MustCompile(`^ins-([a-z0-9]{8})-([1-9][0-9]{0,4})$`)

// ParseHost 从 Host 头解析实例 ID 与端口：去端口、去尾点、小写；父域必须精确等于 ingressDomain。
// 只接受 ASCII（先于小写化检查，防止 Kelvin 号 K 之类的 Unicode 折叠成 ASCII 字母）；
// 端口 22 与容器 sshd 冲突，不作为 Web 端口。
func ParseHost(host, ingressDomain string) (instanceID string, port int, ok bool) {
	h := strings.TrimSpace(host)
	if !isASCII(h) {
		return "", 0, false
	}
	h = strings.ToLower(h)
	if strings.Contains(h, ":") {
		hp, hostPort, err := net.SplitHostPort(h)
		if err != nil || !validHostPort(hostPort) {
			return "", 0, false
		}
		h = hp
	}
	h = strings.TrimSuffix(h, ".")
	domain := strings.ToLower(ingressDomain)
	if domain == "" || strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") {
		return "", 0, false
	}
	suffix := "." + domain
	if !strings.HasSuffix(h, suffix) {
		return "", 0, false
	}
	m := hostRe.FindStringSubmatch(strings.TrimSuffix(h, suffix))
	if m == nil {
		return "", 0, false
	}
	p, err := strconv.Atoi(m[2])
	if err != nil || p < 1 || p > 65535 || p == 22 {
		return "", 0, false
	}
	return "ins-" + m[1], p, true
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// validHostPort 校验 Host 头里携带的端口段（入口自身的监听端口）是 1～65535 的纯数字。
func validHostPort(s string) bool {
	if s == "" || len(s) > 5 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	n, err := strconv.Atoi(s)
	return err == nil && n >= 1 && n <= 65535
}
