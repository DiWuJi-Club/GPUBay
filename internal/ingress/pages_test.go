package ingress_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/ingress"
	"spark-cloud/internal/rental"
)

func TestWritePageHeadersAndEscaping(t *testing.T) {
	rec := httptest.NewRecorder()
	ingress.WritePage(rec, http.StatusNotFound, `<script>alert(1)</script>`, `ins-x.spark.test/"><img src=x onerror=alert(1)>`, 0)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
	h := rec.Header()
	if h.Get("Content-Type") != "text/html; charset=utf-8" || h.Get("Cache-Control") != "no-store" || h.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("headers = %v", h)
	}
	body := rec.Body.String()
	for _, raw := range []string{"<script>", "<img", `"><img`} {
		if strings.Contains(body, raw) {
			t.Fatalf("unescaped %q in body:\n%s", raw, body)
		}
	}
	if !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") || !strings.Contains(body, "&lt;img src=x onerror=alert(1)&gt;") {
		t.Fatalf("escaped content missing:\n%s", body)
	}
	if strings.Contains(body, `http-equiv="refresh"`) {
		t.Fatal("refresh meta present with refreshSeconds=0")
	}
}

func TestWritePageRefresh(t *testing.T) {
	rec := httptest.NewRecorder()
	ingress.WritePage(rec, http.StatusServiceUnavailable, "实例未运行", ingress.PageStopped, 15)
	body := rec.Body.String()
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(body, `<meta http-equiv="refresh" content="15">`) || !strings.Contains(body, ingress.PageStopped) {
		t.Fatalf("code=%d body=\n%s", rec.Code, body)
	}
}

func TestPageConstants(t *testing.T) {
	if ingress.PageNotOpen != "该端口未开放" {
		t.Fatal("PageNotOpen text")
	}
	if got := fmt.Sprintf(ingress.PageUpstreamFmt, 8888); got != "无法连接到实例端口，请确认服务已在容器内监听 8888" {
		t.Fatalf("PageUpstreamFmt = %q", got)
	}
}

func TestNotRunningPage(t *testing.T) {
	cases := []struct {
		r    rental.IngressRoute
		want string
	}{
		{rental.IngressRoute{State: rental.StateRunning, Suspended: true}, ingress.PageSuspended},
		{rental.IngressRoute{State: rental.StateCreating}, ingress.PageCreating},
		{rental.IngressRoute{State: rental.StateStopped}, ingress.PageStopped},
		{rental.IngressRoute{State: rental.StateReleased}, ingress.PageReleased},
		{rental.IngressRoute{State: rental.StateWiping}, ingress.PageReleased},
		{rental.IngressRoute{State: rental.StateFailed}, ingress.PageReleased},
		{rental.IngressRoute{State: rental.StateRunning}, ingress.PageReleased}, // 调用方只在非可用时调用；兜底文案
		{rental.IngressRoute{State: rental.StateReserved, ScheduledStartAt: ptrTime(time.Date(2026, 3, 4, 3, 0, 0, 0, time.UTC))}, "实例预订于 2026-03-04 11:00 开始，开始前 1 小时自动开通"},
		{rental.IngressRoute{State: rental.StateReserved}, "实例预订于 — 开始，开始前 1 小时自动开通"},
		{rental.IngressRoute{State: rental.StateCancelled}, ingress.PageReleased},
	}
	for _, c := range cases {
		if got := ingress.NotRunningPage(c.r); got != c.want {
			t.Fatalf("%+v → %q, want %q", c.r, got, c.want)
		}
	}
}

func ptrTime(t time.Time) *time.Time { return &t }
