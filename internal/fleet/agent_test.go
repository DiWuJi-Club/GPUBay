package fleet_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"spark-cloud/internal/fleet"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/dbtest"
)

func TestAttachDetachConnAndOwners(t *testing.T) {
	svc, pool, clk := newEnv(t)
	ctx := context.Background()
	a := dbtest.CreateMachine(t, pool, "SN-A")
	b := dbtest.CreateMachine(t, pool, "SN-B")
	if err := svc.AttachAgentConn(ctx, a, "conn-1"); err != nil {
		t.Fatal(err)
	}
	if err := svc.AttachAgentConn(ctx, 9999, "conn-x"); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("attach unknown = %v", err)
	}
	m, _ := svc.Get(ctx, pool, a)
	if !m.Connected || m.AgentConnectedAt == nil || !m.AgentConnectedAt.Equal(clk.Now()) || m.Enrolled {
		t.Fatalf("after attach = %+v", m)
	}
	owners, err := svc.ConnOwners(ctx, []int64{a, b})
	if err != nil || owners[a] != (fleet.ConnOwner{ConnID: "conn-1"}) || owners[b] != (fleet.ConnOwner{}) {
		t.Fatalf("owners = %+v, %v", owners, err)
	}
	// 只清自己的：别的连接 id 不生效
	if err := svc.DetachAgentConn(ctx, a, "conn-2"); err != nil {
		t.Fatal(err)
	}
	if m, _ = svc.Get(ctx, pool, a); !m.Connected {
		t.Fatal("detach with other conn id must not clear")
	}
	if err := svc.DetachAgentConn(ctx, a, "conn-1"); err != nil {
		t.Fatal(err)
	}
	if m, _ = svc.Get(ctx, pool, a); m.Connected {
		t.Fatal("detach must clear own conn")
	}
	if got, _ := svc.MachineBySerial(ctx, "SN-B"); got.ID != b {
		t.Fatalf("by serial = %+v", got)
	}
	if _, err := svc.MachineBySerial(ctx, "nope"); !errors.Is(err, apperr.ErrNotFound) {
		t.Fatalf("unknown serial = %v", err)
	}
}

func TestRecordMetricsAndStateReport(t *testing.T) {
	svc, pool, clk := newEnv(t)
	ctx := context.Background()
	id := dbtest.CreateMachine(t, pool, "SN-A")
	if err := svc.RecordMetrics(ctx, id, "0.1.0", json.RawMessage(`{"gpu_temp_c":55}`)); err != nil {
		t.Fatal(err)
	}
	if err := svc.RecordMetrics(ctx, id, "0.1.1", nil); err != nil { // nil 保留旧指标
		t.Fatal(err)
	}
	m, _ := svc.Get(ctx, pool, id)
	if m.AgentVersion != "0.1.1" || string(m.LastMetrics) != `{"gpu_temp_c": 55}` {
		t.Fatalf("metrics = %q version = %q", m.LastMetrics, m.AgentVersion)
	}
	if err := svc.RecordMetrics(ctx, id, "", json.RawMessage(`{"gpu_temp_c":60}`)); err != nil { // 空版本保留旧版本
		t.Fatal(err)
	}
	if m, _ = svc.Get(ctx, pool, id); m.AgentVersion != "0.1.1" || string(m.LastMetrics) != `{"gpu_temp_c": 60}` {
		t.Fatalf("after empty version: metrics = %q version = %q", m.LastMetrics, m.AgentVersion)
	}
	rep := fleet.StateReport{Instances: []fleet.InstanceState{{InstanceID: "ins-aaaaaaaa", Phase: fleet.PhaseRunning}, {InstanceID: "ins-bbbbbbbb", Phase: fleet.PhaseStopped}}}
	if got := rep.RunningIDs(); len(got) != 1 || got[0] != "ins-aaaaaaaa" {
		t.Fatalf("running ids = %v", got)
	}
	// 没有连接时间 → 不可用于兜底恢复
	if err := svc.RecordStateReport(ctx, id, rep); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.ReportsFor(ctx, []int64{id}); len(got) != 0 {
		t.Fatalf("reports without connection = %v", got)
	}
	// 报告早于连接时间 → 不可用；报告晚于或等于连接时间 → 可用
	clk.Advance(time.Second)
	if err := svc.AttachAgentConn(ctx, id, "c1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.ReportsFor(ctx, []int64{id}); len(got) != 0 {
		t.Fatalf("stale report = %v", got)
	}
	if err := svc.RecordStateReport(ctx, id, rep); err != nil {
		t.Fatal(err)
	}
	got, err := svc.ReportsFor(ctx, []int64{id})
	if err != nil || len(got) != 1 || len(got[id].Instances) != 2 {
		t.Fatalf("reports = %+v, %v", got, err)
	}
	m, _ = svc.Get(ctx, pool, id)
	if m.StateReportedAt == nil || !m.StateReportedAt.Equal(clk.Now()) {
		t.Fatalf("state_reported_at = %v", m.StateReportedAt)
	}
	// 离线机器不返回
	if _, err := svc.MarkOfflineStale(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.ReportsFor(ctx, []int64{id}); len(got) != 0 {
		t.Fatalf("offline report = %v", got)
	}
}

func TestRecordCachedImages(t *testing.T) {
	svc, pool, clk := newEnv(t)
	ctx := context.Background()
	id := dbtest.CreateMachine(t, pool, "SN-A")
	m, _ := svc.Get(ctx, pool, id)
	if m.CachedImages == nil || len(m.CachedImages) != 0 || m.CachedImagesAt != nil {
		t.Fatalf("fresh machine = %#v at %v", m.CachedImages, m.CachedImagesAt)
	}
	// nil = 旧 Agent 未上报：不写
	if err := svc.RecordCachedImages(ctx, id, nil); err != nil {
		t.Fatal(err)
	}
	if m, _ = svc.Get(ctx, pool, id); m.CachedImagesAt != nil {
		t.Fatalf("nil wrote cached_images_at = %v", m.CachedImagesAt)
	}
	// 网关侧再规范化一遍：去 <none>、去重、排序、docker.io/library 前缀折叠
	if err := svc.RecordCachedImages(ctx, id, []string{"spark/pytorch:25.09", "<none>:<none>", "docker.io/library/ubuntu:24.04", "spark/pytorch:25.09"}); err != nil {
		t.Fatal(err)
	}
	m, _ = svc.Get(ctx, pool, id)
	if strings.Join(m.CachedImages, ",") != "spark/pytorch:25.09,ubuntu:24.04" || m.CachedImagesAt == nil || !m.CachedImagesAt.Equal(clk.Now()) {
		t.Fatalf("cached = %v at %v", m.CachedImages, m.CachedImagesAt)
	}
	many := make([]string, 201)
	for i := range many {
		many[i] = fmt.Sprintf("img%03d:%s", i, strings.Repeat("x", 300))
	}
	clk.Advance(time.Minute)
	if err := svc.RecordCachedImages(ctx, id, many); err != nil {
		t.Fatal(err)
	}
	m, _ = svc.Get(ctx, pool, id)
	if len(m.CachedImages) != fleet.MaxCachedImages || len(m.CachedImages[0]) != fleet.MaxCachedImageLen || !m.CachedImagesAt.Equal(clk.Now()) {
		t.Fatalf("truncation: %d items, first len %d", len(m.CachedImages), len(m.CachedImages[0]))
	}
	// 空列表（Agent 本机确实没有镜像）照写
	if err := svc.RecordCachedImages(ctx, id, []string{}); err != nil {
		t.Fatal(err)
	}
	if m, _ = svc.Get(ctx, pool, id); m.CachedImages == nil || len(m.CachedImages) != 0 {
		t.Fatalf("empty = %#v", m.CachedImages)
	}
	// 镜像列表不进 last_state_report
	if err := svc.RecordStateReport(ctx, id, fleet.StateReport{Images: []string{"a:1"}}); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := pool.QueryRow(ctx, `SELECT last_state_report::text FROM machines WHERE id = $1`, id).Scan(&raw); err != nil || raw != `{"instances": []}` {
		t.Fatalf("last_state_report = %s %v", raw, err)
	}
}
