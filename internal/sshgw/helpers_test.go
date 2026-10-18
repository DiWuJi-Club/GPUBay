package sshgw_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"spark-cloud/internal/access/sshtest"
	"spark-cloud/internal/identity"
	"spark-cloud/internal/platform/apperr"
	"spark-cloud/internal/platform/clock"
	"spark-cloud/internal/rental"
	"spark-cloud/internal/sshgw"
)

const instA = "ins-aaaaaaaa"

func newSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type fakeStore struct {
	mu           sync.Mutex
	infos        map[string]rental.AccessInfo
	failNext     error
	failLeft     int // failNext 还要生效的次数
	nextID       int64
	opened       []rental.SSHSessionStart
	closed       map[int64][2]int64
	touched      [][]int64
	alerts       []string
	accessed     int
	stateQueries int
	// tofuRace 非空：模拟其它副本抢先写回该公钥，SetSSHHostPubkeyIfEmpty 返回 false
	tofuRace string
}

func newFakeStore() *fakeStore {
	return &fakeStore{infos: map[string]rental.AccessInfo{}, closed: map[int64][2]int64{}}
}

func (f *fakeStore) set(id string, info rental.AccessInfo) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.infos[id] = info
}

// setFailNext 让下一次 Access 返回该错误（模拟数据库瞬时故障）。
func (f *fakeStore) setFailNext(err error) { f.setFailFor(err, 1) }

// setFailFor 让接下来 n 次 Access 返回该错误（模拟数据库持续故障）。
func (f *fakeStore) setFailFor(err error, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failNext, f.failLeft = err, n
}

func (f *fakeStore) Access(_ context.Context, id string) (rental.AccessInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accessed++
	if f.failNext != nil && f.failLeft > 0 {
		f.failLeft--
		return rental.AccessInfo{}, f.failNext
	}
	info, ok := f.infos[id]
	if !ok {
		return rental.AccessInfo{}, apperr.ErrNotFound
	}
	return info, nil
}

// SessionStates 与 Access 共用失败注入：注入的错误对下一次（批量）查询生效。
func (f *fakeStore) SessionStates(_ context.Context, ids []string) (map[string]rental.SessionState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stateQueries++
	if f.failNext != nil && f.failLeft > 0 {
		f.failLeft--
		return nil, f.failNext
	}
	out := map[string]rental.SessionState{}
	for _, id := range ids {
		if info, ok := f.infos[id]; ok {
			out[id] = rental.SessionState{State: info.Instance.State, Suspended: info.Instance.Suspended()}
		}
	}
	return out, nil
}

func (f *fakeStore) SetSSHHostPubkeyIfEmpty(_ context.Context, id, pub string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	info := f.infos[id]
	if f.tofuRace != "" && info.Instance.SSHHostPubkey == "" {
		info.Instance.SSHHostPubkey = f.tofuRace
		f.infos[id] = info
	}
	if info.Instance.SSHHostPubkey != "" {
		return false, nil
	}
	info.Instance.SSHHostPubkey = pub
	f.infos[id] = info
	return true, nil
}

func (f *fakeStore) SSHHostPubkey(_ context.Context, id string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	info, ok := f.infos[id]
	if !ok {
		return "", apperr.ErrNotFound
	}
	return info.Instance.SSHHostPubkey, nil
}

func (f *fakeStore) hostPub(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.infos[id].Instance.SSHHostPubkey
}

func (f *fakeStore) OpenSSHSession(_ context.Context, st rental.SSHSessionStart) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.opened = append(f.opened, st)
	return f.nextID, nil
}

func (f *fakeStore) TouchSSHSessions(_ context.Context, ids []int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touched = append(f.touched, append([]int64(nil), ids...))
	return nil
}

func (f *fakeStore) CloseSSHSession(_ context.Context, id, up, down int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed[id] = [2]int64{up, down}
	return nil
}

func (f *fakeStore) RaiseSSHBruteforce(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.alerts = append(f.alerts, "bruteforce:"+id)
	return nil
}

func (f *fakeStore) RaiseUpstreamUnreachable(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.alerts = append(f.alerts, "unreachable:"+id)
	return nil
}

func (f *fakeStore) snapshot() (opened []rental.SSHSessionStart, closed map[int64][2]int64, alerts []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := map[int64][2]int64{}
	for k, v := range f.closed {
		c[k] = v
	}
	return append([]rental.SSHSessionStart(nil), f.opened...), c, append([]string(nil), f.alerts...)
}

type fakeKeys struct {
	mu      sync.Mutex
	keys    map[int64]map[string]identity.SSHKey // user → fingerprint → key
	touched []int64
}

func (f *fakeKeys) add(userID int64, pub ssh.PublicKey) identity.SSHKey {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.keys == nil {
		f.keys = map[int64]map[string]identity.SSHKey{}
	}
	if f.keys[userID] == nil {
		f.keys[userID] = map[string]identity.SSHKey{}
	}
	k := identity.SSHKey{ID: int64(len(f.keys[userID]) + 1), UserID: userID, Fingerprint: ssh.FingerprintSHA256(pub), PublicKey: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))}
	f.keys[userID][k.Fingerprint] = k
	return k
}

func (f *fakeKeys) FindSSHKey(_ context.Context, userID int64, fp string) (identity.SSHKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if k, ok := f.keys[userID][fp]; ok {
		return k, nil
	}
	return identity.SSHKey{}, apperr.ErrNotFound
}

func (f *fakeKeys) TouchSSHKey(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touched = append(f.touched, id)
	return nil
}

type env struct {
	t        *testing.T
	ctx      context.Context
	cancel   context.CancelFunc
	clk      *clock.Fake
	store    *fakeStore
	keys     *fakeKeys
	upstream *sshtest.Server
	srv      *sshgw.Server
	hostKey  ssh.Signer
	addr     string
	done     chan struct{}
	logs     *syncBuffer
}

// syncBuffer 收集网关日志，供断言「日志里没有密码」。
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func runningInfo(e *env, id string, userID int64, password string) rental.AccessInfo {
	return rental.AccessInfo{
		Instance:      rental.Instance{ID: id, UserID: userID, MachineID: 1, State: rental.StateRunning, PasswordAuth: true, SSHHostPubkey: e.upstream.HostPubLine()},
		Password:      password,
		MachineLanIP:  "127.0.0.1",
		MachineSerial: "SN-TEST",
		AgentOnline:   true,
	}
}

func newEnvWith(t *testing.T, tweak func(*sshgw.Config)) *env { return newEnvLis(t, tweak, nil) }

// newEnvLis 同 newEnvWith，wrap 非空时用它包装监听器（模拟 Accept 错误）。
func newEnvLis(t *testing.T, tweak func(*sshgw.Config), wrap func(net.Listener) net.Listener) *env {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	up := newSigner(t)
	us, err := sshtest.New(sshtest.Options{AuthorizedKey: up.PublicKey()})
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, ctx: ctx, cancel: cancel, clk: clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)), store: newFakeStore(), keys: &fakeKeys{}, upstream: us, hostKey: newSigner(t), done: make(chan struct{}), logs: &syncBuffer{}}
	cfg := sshgw.Config{HostKey: e.hostKey, Upstream: up, UpstreamPort: us.Port(), DrainTimeout: time.Second}
	if tweak != nil {
		tweak(&cfg)
	}
	e.srv, err = sshgw.New(cfg, sshgw.Deps{Store: e.store, Keys: e.keys, Clock: e.clk, Log: slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	if err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	e.addr = lis.Addr().String()
	if wrap != nil {
		lis = wrap(lis)
	}
	go func() { defer close(e.done); _ = e.srv.Serve(ctx, lis) }()
	e.store.set(instA, runningInfo(e, instA, 7, "pw-correct-16chr"))
	t.Cleanup(func() { cancel(); <-e.done; us.Close() })
	return e
}

func newEnv(t *testing.T) *env { return newEnvWith(t, nil) }

// dial 以密码或公钥连接网关；返回 banner 文本与错误。
func (e *env) dial(user string, auth ssh.AuthMethod) (*ssh.Client, string, error) {
	e.t.Helper()
	var banner string
	c, err := ssh.Dial("tcp", e.addr, &ssh.ClientConfig{User: user, Auth: []ssh.AuthMethod{auth},
		HostKeyCallback: ssh.FixedHostKey(e.hostKey.PublicKey()), Timeout: 5 * time.Second,
		BannerCallback: func(m string) error { banner += m; return nil }})
	if err == nil {
		e.t.Cleanup(func() { c.Close() })
	}
	return c, strings.TrimSpace(banner), err
}

func (e *env) mustDial(user string, auth ssh.AuthMethod) *ssh.Client {
	e.t.Helper()
	c, _, err := e.dial(user, auth)
	if err != nil {
		e.t.Fatalf("dial %s: %v", user, err)
	}
	return c
}

func run(t *testing.T, c *ssh.Client, cmd string) (stdout, stderr string, err error) {
	t.Helper()
	sess, err := c.NewSession()
	if err != nil {
		return "", "", err
	}
	defer sess.Close()
	var o, se strings.Builder
	sess.Stdout, sess.Stderr = &o, &se
	err = sess.Run(cmd)
	return o.String(), se.String(), err
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

// 生产实现必须满足网关依赖的接口。
var (
	_ sshgw.Store = (*rental.Service)(nil)
	_ sshgw.Keys  = (*identity.Service)(nil)
)
