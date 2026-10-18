package ingress_test

import (
	"testing"

	"spark-cloud/internal/ingress"
)

func TestParseHostNormalizes(t *testing.T) {
	ok := []struct {
		host string
		id   string
		port int
	}{
		{"ins-a1b2c3d4-8888.spark.test", "ins-a1b2c3d4", 8888},
		{"INS-a1b2c3d4-8888.Spark.Test.:18080", "ins-a1b2c3d4", 8888},
		{"ins-a1b2c3d4-80.spark.test:443", "ins-a1b2c3d4", 80},
		{"ins-zzzzzzzz-65535.spark.test", "ins-zzzzzzzz", 65535},
	}
	for _, c := range ok {
		id, port, found := ingress.ParseHost(c.host, "spark.test")
		if !found || id != c.id || port != c.port {
			t.Fatalf("%q → %q %d %v", c.host, id, port, found)
		}
	}
	for _, bad := range []string{"spark.test", "ins-a1b2c3d4.spark.test", "ins-a1b2c3d4-8888.other.test", "ins-a1b2c3d4-0.spark.test",
		"ins-a1b2c3d4-70000.spark.test", "ins-a1b2c3d-8888.spark.test", "x.ins-a1b2c3d4-8888.spark.test", "ins-a1b2c3d4-8888.spark.test.evil.com", ""} {
		if _, _, found := ingress.ParseHost(bad, "spark.test"); found {
			t.Fatalf("%q accepted", bad)
		}
	}
	// 独立注册域
	if id, port, found := ingress.ParseHost("ins-a1b2c3d4-7860.sparkapp.example", "sparkapp.example"); !found || id != "ins-a1b2c3d4" || port != 7860 {
		t.Fatal("separate ingress domain not matched")
	}
}

// TestParseHostRejects 覆盖安全相关的拒绝路径：每一类各一行。
func TestParseHostRejects(t *testing.T) {
	cases := []struct{ name, host string }{
		// 大小写 / Unicode / punycode
		{"kelvin sign lowercases to k", "ins-a1b2c3dK-8888.spark.test"},
		{"dotted capital I", "İns-a1b2c3d4-8888.spark.test"},
		{"fullwidth digit", "ins-a1b2c3d4-８８８８.spark.test"},
		{"unicode in domain", "ins-a1b2c3d4-8888.spark.tést"},
		{"punycode instance label", "xn--ins-a1b2c3d4-8888.spark.test"},
		{"punycode domain", "ins-a1b2c3d4-8888.xn--spark-test.test"},
		{"underscore in id", "ins-a1b2_3d4-8888.spark.test"},
		// 尾点
		{"double trailing dot", "ins-a1b2c3d4-8888.spark.test.."},
		{"double trailing dot with port", "ins-a1b2c3d4-8888.spark.test..:443"},
		{"only dot", "."},
		// 多余标签
		{"extra leading label", "a.ins-a1b2c3d4-8888.spark.test"},
		{"empty leading label", ".ins-a1b2c3d4-8888.spark.test"},
		{"extra middle label", "ins-a1b2c3d4-8888.x.spark.test"},
		{"bare domain", "spark.test"},
		{"domain with trailing dot", "spark.test."},
		// Host 中的端口
		{"non-numeric host port", "ins-a1b2c3d4-8888.spark.test:abc"},
		{"empty host port", "ins-a1b2c3d4-8888.spark.test:"},
		{"double port", "ins-a1b2c3d4-8888.spark.test:443:443"},
		{"host port out of range", "ins-a1b2c3d4-8888.spark.test:70000"},
		{"ipv6 literal", "[::1]:8080"},
		{"userinfo", "user@ins-a1b2c3d4-8888.spark.test"},
		{"whitespace inside", "ins-a1b2c3d4-8888 .spark.test"},
		// 实例 ID 形态
		{"id too short", "ins-a1b2c3d-8888.spark.test"},
		{"id too long", "ins-a1b2c3d45-8888.spark.test"},
		{"wrong prefix", "inst-a1b2c3d4-8888.spark.test"},
		{"no prefix", "a1b2c3d4-8888.spark.test"},
		{"missing port", "ins-a1b2c3d4.spark.test"},
		{"missing port digits", "ins-a1b2c3d4-.spark.test"},
		// 端口范围
		{"port 0", "ins-a1b2c3d4-0.spark.test"},
		{"port 22", "ins-a1b2c3d4-22.spark.test"},
		{"port 65536", "ins-a1b2c3d4-65536.spark.test"},
		{"port 6 digits", "ins-a1b2c3d4-100000.spark.test"},
		{"port leading zero", "ins-a1b2c3d4-08888.spark.test"},
		{"port all zeros", "ins-a1b2c3d4-00000.spark.test"},
		{"negative port", "ins-a1b2c3d4--1.spark.test"},
		{"plus port", "ins-a1b2c3d4-+80.spark.test"},
		// 父域
		{"other domain", "ins-a1b2c3d4-8888.other.test"},
		{"suffix confusion", "ins-a1b2c3d4-8888.evilspark.test"},
		{"suffix confusion glued", "ins-a1b2c3d4-8888evil.spark.test"},
		{"domain as prefix", "ins-a1b2c3d4-8888.spark.test.evil.com"},
		{"parent of domain", "ins-a1b2c3d4-8888.test"},
		{"empty", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if id, port, found := ingress.ParseHost(c.host, "spark.test"); found {
				t.Fatalf("%q accepted → %q %d", c.host, id, port)
			}
		})
	}
	// 配置异常：空父域不能让任意 Host 通过。
	for _, dom := range []string{"", ".", "spark.test."} {
		if _, _, found := ingress.ParseHost("ins-a1b2c3d4-8888", dom); found {
			t.Fatalf("empty-ish domain %q accepted bare label", dom)
		}
	}
	// 父域本身大小写不敏感（配置已小写化，这里再兜一次）。
	if _, _, found := ingress.ParseHost("ins-a1b2c3d4-8888.spark.test", "SPARK.Test"); !found {
		t.Fatal("mixed-case configured domain not matched")
	}
	// 最小合法端口。
	if _, port, found := ingress.ParseHost("ins-a1b2c3d4-1.spark.test", "spark.test"); !found || port != 1 {
		t.Fatal("port 1 rejected")
	}
}
