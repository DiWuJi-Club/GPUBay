package butler

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"spark-cloud/internal/appdeploy"
	"spark-cloud/internal/butler/llm"
	"spark-cloud/internal/butler/llm/mock"
	"spark-cloud/internal/butler/tools"
	"spark-cloud/internal/catalog"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/identity"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/platform/db"
	"spark-cloud/internal/platform/dbtest"
	"spark-cloud/internal/platform/secretbox"
	"spark-cloud/internal/rental"
	"spark-cloud/internal/wallet"
)

// 手写 fake（tools.Deps 的小接口）：数据按 userID 归属，他人资源 → not_found；记录写调用供断言。

type fakeRental struct {
	mu         sync.Mutex
	instances  []rental.Instance
	renewCalls []string // 幂等键
	renewRes   rental.Order
	renewErr   error
	released   []string
}

func (f *fakeRental) own(userID int64, id string) (rental.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, i := range f.instances {
		if i.ID == id && i.UserID == userID {
			return i, nil
		}
	}
	return rental.Instance{}, apperr.ErrNotFound
}

func (f *fakeRental) ListForUser(_ context.Context, userID int64) ([]rental.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []rental.Instance{}
	for _, i := range f.instances {
		if i.UserID == userID {
			out = append(out, i)
		}
	}
	return out, nil
}
func (f *fakeRental) GetForUser(_ context.Context, userID int64, id string) (rental.Instance, error) {
	return f.own(userID, id)
}
func (f *fakeRental) UserEvents(_ context.Context, userID int64, id string) ([]rental.UserEvent, error) {
	_, err := f.own(userID, id)
	return nil, err
}
func (f *fakeRental) ListPorts(context.Context, string) ([]rental.Port, error) { return nil, nil }
func (f *fakeRental) RenewInfoFor(context.Context, rental.Instance) (rental.RenewInfo, error) {
	return rental.RenewInfo{Options: []rental.RenewOption{{Duration: catalog.D1d, QuantityMax: 90}}}, nil
}
func (f *fakeRental) CancelQuote(context.Context, int64, string) (rental.CancelQuote, error) {
	return rental.CancelQuote{}, apperr.ErrCancelNotAllowed
}
func (f *fakeRental) ListOrders(context.Context, int64, int) ([]rental.Order, error) { return nil, nil }
func (f *fakeRental) AvailableNow(context.Context, int64) (int, error)              { return 1, nil }
func (f *fakeRental) Availability(context.Context, rental.AvailabilityQuery) (rental.Availability, error) {
	return rental.Availability{}, nil
}
func (f *fakeRental) PlaceBatch(context.Context, rental.BatchRequest) (rental.BatchResult, error) {
	return rental.BatchResult{}, apperr.ErrNoCapacity
}
func (f *fakeRental) RenewQuantity(_ context.Context, _ int64, _ string, _ catalog.Duration, _ int, idem string) (rental.Order, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renewCalls = append(f.renewCalls, idem)
	return f.renewRes, f.renewErr
}
func (f *fakeRental) Release(_ context.Context, _ int64, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released = append(f.released, id)
	return nil
}
func (f *fakeRental) Cancel(_ context.Context, _ int64, id, _ string) (rental.Instance, error) {
	return rental.Instance{ID: id}, nil
}
func (f *fakeRental) releasedN() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.released)
}
func (f *fakeRental) renewN() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.renewCalls)
}

type fakeApps struct {
	mu      sync.Mutex
	catalog []appdeploy.CatalogView
	created []appdeploy.CreateRequest
}

func (f *fakeApps) CatalogViews(context.Context, bool) ([]appdeploy.CatalogView, error) { return f.catalog, nil }
func (f *fakeApps) CatalogViewBySlug(_ context.Context, slug string) (appdeploy.CatalogView, error) {
	for _, v := range f.catalog {
		if v.Slug == slug {
			return v, nil
		}
	}
	return appdeploy.CatalogView{}, apperr.ErrAppNotFound
}
func (f *fakeApps) ListForInstance(context.Context, string) ([]appdeploy.Deployment, error) { return nil, nil }
func (f *fakeApps) Views(context.Context, []appdeploy.Deployment) ([]appdeploy.View, error) { return nil, nil }
func (f *fakeApps) Summaries(context.Context, []string) (map[string][]appdeploy.Summary, error) {
	return map[string][]appdeploy.Summary{}, nil
}
func (f *fakeApps) GetForUser(context.Context, int64, string, string) (appdeploy.Deployment, error) {
	return appdeploy.Deployment{}, apperr.ErrNotFound
}
func (f *fakeApps) Log(context.Context, int64, string, string) (appdeploy.LogResult, error) {
	return appdeploy.LogResult{}, apperr.ErrNotFound
}
func (f *fakeApps) Events(context.Context, string, int) ([]appdeploy.Event, error) { return nil, nil }
func (f *fakeApps) Create(_ context.Context, req appdeploy.CreateRequest) (appdeploy.Deployment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, req)
	return appdeploy.Deployment{ID: "dep-new", InstanceID: req.InstanceID, UserID: req.UserID, AppID: req.AppID, State: "queued"}, nil
}
func (f *fakeApps) Redeploy(context.Context, appdeploy.RedeployRequest) (appdeploy.Deployment, error) {
	return appdeploy.Deployment{}, apperr.ErrNotFound
}
func (f *fakeApps) Stop(context.Context, int64, string, string, string) (appdeploy.Deployment, error) {
	return appdeploy.Deployment{}, apperr.ErrNotFound
}
func (f *fakeApps) Start(context.Context, int64, string, string, string) (appdeploy.Deployment, error) {
	return appdeploy.Deployment{}, apperr.ErrNotFound
}
func (f *fakeApps) Remove(context.Context, int64, string, string, string) (appdeploy.Deployment, error) {
	return appdeploy.Deployment{}, apperr.ErrNotFound
}

type fakeCatalog struct{}

func (fakeCatalog) ListProducts(context.Context) ([]catalog.ProductWithPrices, error) {
	return []catalog.ProductWithPrices{{Product: catalog.Product{ID: 1, Code: "spark-1", Name: "DGX Spark 整机"},
		Prices: []catalog.Price{{Duration: catalog.D1d, Amount: 6900}}}}, nil
}
func (fakeCatalog) Quote(_ context.Context, _ db.Querier, _ int64, d catalog.Duration) (int64, error) {
	if d == catalog.D1d {
		return 6900, nil
	}
	return 0, apperr.BadRequest("该规格暂不支持此租期")
}
func (fakeCatalog) ProductName(context.Context, int64) (string, error) { return "DGX Spark 整机", nil }
func (fakeCatalog) ImageName(context.Context, int64) (string, error)   { return "PyTorch", nil }
func (fakeCatalog) ListImages(context.Context) ([]catalog.Image, error) { return nil, nil }

type fakeWallet struct{}

func (fakeWallet) Balance(context.Context, int64) (int64, error) { return 13800, nil }
func (fakeWallet) ListLedger(context.Context, int64, int64, int) ([]wallet.LedgerRow, error) {
	return nil, nil
}

type fakeUsers struct{}

func (fakeUsers) GetUser(_ context.Context, id int64) (identity.User, error) {
	return identity.User{ID: id, Phone: "13800001234", KYCStatus: "verified"}, nil
}

// sliceSink 收集事件；deadAfter > 0 时收到第 deadAfter 个 delta 后像断开的 SSE 一样丢弃后续事件。
type sliceSink struct {
	mu        sync.Mutex
	events    []sinkEvent
	deadAfter int
	deltas    int
	dead      bool
}

type sinkEvent struct {
	Name string
	V    any
}

func (s *sliceSink) Emit(name string, v any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dead {
		return
	}
	s.events = append(s.events, sinkEvent{name, v})
	if name == EvDelta {
		s.deltas++
		if s.deadAfter > 0 && s.deltas >= s.deadAfter {
			s.dead = true
		}
	}
}

func (s *sliceSink) names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, e := range s.events {
		if e.Name == EvDelta && len(out) > 0 && out[len(out)-1] == EvDelta {
			continue // 连续 delta 合并
		}
		out = append(out, e.Name)
	}
	return out
}

func (s *sliceSink) find(name string) []any {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []any
	for _, e := range s.events {
		if e.Name == name {
			out = append(out, e.V)
		}
	}
	return out
}

func (s *sliceSink) text() string {
	var b strings.Builder
	for _, v := range s.find(EvDelta) {
		b.WriteString(v.(DeltaEvent).Text)
	}
	return b.String()
}

// hookProvider 在调用 mock 之前执行 before（模拟轮次进行中的外部变化）。
type hookProvider struct {
	*mock.Mock
	before func()
}

func (h *hookProvider) Chat(ctx context.Context, req llm.Request, emit func(llm.Event)) error {
	if h.before != nil {
		h.before()
	}
	return h.Mock.Chat(ctx, req, emit)
}

// lockedBuf 是并发安全的日志缓冲。
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type env struct {
	t      *testing.T
	svc    *Service
	clk    *clock.Fake
	mock   *mock.Mock
	hook   *hookProvider
	rental *fakeRental
	apps   *fakeApps
	logs   *lockedBuf
	owner  int64
	other  int64
}

var testBoxKey = bytes.Repeat([]byte{7}, 32)

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := dbtest.New(t)
	clk := clock.NewFake(dbtest.T0.Add(2 * time.Hour)) // 北京时间 10:00
	owner := dbtest.CreateUser(t, pool, "13800001234", true)
	other := dbtest.CreateUser(t, pool, "13800005678", true)
	exp := clk.Now().Add(20 * time.Hour)
	fr := &fakeRental{
		instances: []rental.Instance{
			{ID: "ins-run", UserID: owner, MachineID: 3, ProductID: 1, ImageID: 2, State: rental.StateRunning, ExpiresAt: &exp, CreatedAt: clk.Now()},
			{ID: "ins-other", UserID: other, MachineID: 5, ProductID: 1, ImageID: 2, State: rental.StateRunning, ExpiresAt: &exp},
		},
		renewRes: rental.Order{ID: "ord-renew", Amount: 6900},
	}
	fa := &fakeApps{catalog: []appdeploy.CatalogView{{ID: 12, Slug: "jupyterlab", Name: "JupyterLab", Category: "dev",
		Version: appdeploy.CatalogVersionView{ID: 22, Version: "4.0", Env: []appdeploy.EnvSpec{{Name: "TOKEN", Label: "访问令牌", Type: "secret", Required: true, MaxLen: 256}}},
		Images:  []appdeploy.ImageBrief{{ID: 2, Name: "PyTorch"}}}}}
	box, err := secretbox.New(testBoxKey)
	if err != nil {
		t.Fatal(err)
	}
	reg := tools.NewRegistry(tools.Deps{Rental: fr, Apps: fa, Catalog: fakeCatalog{}, Wallet: fakeWallet{}, Users: fakeUsers{},
		Pool: pool, Clock: clk, IngressDomain: "spark.test", Box: box})
	m := mock.New()
	hp := &hookProvider{Mock: m}
	logs := &lockedBuf{}
	svc := NewService(Deps{Pool: pool, Clock: clk, Log: slog.New(slog.NewTextHandler(logs, nil)), Provider: hp, Tools: reg, ToolsEnabled: true,
		Users: fakeUsers{}, Wallet: fakeWallet{}, Rental: fr, Fleet: fleet.NewService(pool, clk), Box: box})
	return &env{t: t, svc: svc, clk: clk, mock: m, hook: hp, rental: fr, apps: fa, logs: logs, owner: owner, other: other}
}

func (e *env) send(convID, text string) (Conversation, *sliceSink) {
	e.t.Helper()
	sink := &sliceSink{}
	c, err := e.svc.Send(context.Background(), SendRequest{UserID: e.owner, IP: "1.2.3.4", ConversationID: convID, Text: text}, sink)
	if err != nil {
		e.t.Fatalf("Send: %v", err)
	}
	return c, sink
}

func (e *env) msgs(convID string) []Message {
	e.t.Helper()
	m, err := e.svc.messages(context.Background(), e.svc.pool, convID)
	if err != nil {
		e.t.Fatal(err)
	}
	return m
}

func (e *env) count(sql string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.svc.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func call(name, args string) llm.ToolCall { return llm.ToolCall{Name: name, Args: []byte(args)} }

func codeOf(err error) string {
	if e := apperr.As(err); e != nil {
		return e.Code
	}
	return ""
}

var errInsufficient = apperr.ErrInsufficientBalance
