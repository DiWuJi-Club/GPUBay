// Package fakeagent 在进程内跑一个完整的 spark-agent（fake 运行时 / 数据盘 / 网络），供集成测试与 e2e-server 使用。
package fakeagent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"spark-cloud/internal/agent/client"
	diskfake "spark-cloud/internal/agent/disk/fake"
	"spark-cloud/internal/agent/executor"
	"spark-cloud/internal/agent/metrics"
	netfake "spark-cloud/internal/agent/netfilter/fake"
	rtfake "spark-cloud/internal/agent/runtime/fake"
	"spark-cloud/internal/agent/state"
	"spark-cloud/internal/pki"
)

// Creds 是一次接入得到的凭据，可在重连 / 顶替测试里复用。
type Creds struct {
	Key     *ecdsa.PrivateKey
	CertDER []byte
	CADER   []byte
}

type Options struct {
	Addr              string
	CAPEM             []byte
	Token             string
	Creds             *Creds // 非空时不 Enroll，直接用这套凭据建流
	Dir               string // 空则建临时目录并在 Stop 时删除；要用同一状态目录重新 Start 模拟重连，调用方须自带 Dir
	Stepped           bool
	NoReconnect       bool // 被顶掉 / 被撤销后不重连（顶替类测试里的旧 Agent）
	Version           string
	IPs               []string // Hello 上报的本机地址；为空时取 "10.0.0.10"
	FreeBytes         uint64   // 0 → 4000 GiB
	Metrics           metrics.Metrics
	HeartbeatInterval time.Duration // 自动模式心跳间隔，零值 100ms（测试里推进假时钟后很快就能同步）
	Images            []string      // StateReport.images（本机已缓存镜像）；nil 时不上报，同旧 Agent
	Log               *slog.Logger
}

type Agent struct {
	Client  *client.Client
	Exec    *executor.Executor
	Runtime *rtfake.Runtime
	Disk    *diskfake.Disk
	Net     *netfake.Netfilter
	Dir     string
	tempDir bool // Dir 由 Start 创建，Stop 时删除
	creds   *Creds
	cancel  context.CancelFunc
	done    chan struct{}  // Client.Run 已返回
	wg      sync.WaitGroup // Client.Run 与（自动模式下）Executor.Run
}

func (a *Agent) Creds() *Creds { return a.creds }

// SetImages 设置之后的 StateReport 上报的已缓存镜像；nil 时不带 images（同旧 Agent）。
func (a *Agent) SetImages(imgs []string) { a.Runtime.SetImages(imgs) }

// Start 接入（或复用凭据）并建流，直到收到 AgentConfig 才返回；10 秒内连不上则停掉并返回错误。
func Start(ctx context.Context, o Options) (*Agent, error) {
	if o.Log == nil {
		o.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if o.Version == "" {
		o.Version = "fake-0.1"
	}
	if len(o.IPs) == 0 {
		o.IPs = []string{"10.0.0.10"}
	}
	if o.FreeBytes == 0 {
		o.FreeBytes = 4000 << 30
	}
	if o.HeartbeatInterval <= 0 {
		o.HeartbeatInterval = 100 * time.Millisecond
	}
	dir, tempDir := o.Dir, false
	if dir == "" {
		var err error
		if dir, err = os.MkdirTemp("", "fakeagent-"); err != nil {
			return nil, err
		}
		tempDir = true
	}
	a, err := start(ctx, o, dir)
	if err != nil {
		if tempDir {
			os.RemoveAll(dir)
		}
		return nil, err
	}
	a.tempDir = tempDir
	return a, nil
}

func start(ctx context.Context, o Options, dir string) (*Agent, error) {
	creds := o.Creds
	if creds == nil {
		block, _ := pem.Decode(o.CAPEM)
		if block == nil {
			return nil, errors.New("fakeagent: bad CA PEM")
		}
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, err
		}
		certDER, caDER, err := client.Enroll(ctx, o.Addr, o.Token, pki.Fingerprint(block.Bytes), key)
		if err != nil {
			return nil, err
		}
		creds = &Creds{Key: key, CertDER: certDER, CADER: caDER}
	}
	host, _, err := net.SplitHostPort(o.Addr)
	if err != nil {
		return nil, err
	}
	tc, err := client.TLSFor(creds.Key, creds.CertDER, creds.CADER, host)
	if err != nil {
		return nil, err
	}
	st, err := state.Open(filepath.Join(dir, "state.json"))
	if err != nil {
		return nil, err
	}
	a := &Agent{Runtime: rtfake.New(), Disk: diskfake.New(o.FreeBytes), Net: netfake.New(), Dir: dir, creds: creds, done: make(chan struct{})}
	a.Runtime.SetImages(o.Images)
	ips := o.IPs
	a.Exec = executor.New(executor.Deps{Runtime: a.Runtime, Disk: a.Disk, Net: a.Net, State: st, StateDir: dir, Bridge: "spark0",
		LocalIPs: func() []string { return ips }, MemTotal: func() uint64 { return 128 << 30 }, Log: o.Log}, o.Stepped)
	m := o.Metrics
	a.Client = client.New(client.Config{Addr: o.Addr, TLS: tc, Version: o.Version, Hostname: "fake", LocalIPs: func() []string { return ips },
		Stepped: o.Stepped, NoReconnect: o.NoReconnect, HeartbeatInterval: o.HeartbeatInterval,
		Backoff: client.Backoff{Min: 50 * time.Millisecond, Max: time.Second}},
		a.Exec, func(context.Context) metrics.Metrics { return m }, o.Log)
	runCtx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	var runErr error
	a.wg.Add(1)
	go func() { defer a.wg.Done(); defer close(a.done); runErr = a.Client.Run(runCtx) }()
	if !o.Stepped {
		a.wg.Add(1)
		go func() { defer a.wg.Done(); a.Exec.Run(runCtx) }()
	}
	waitCtx, waitCancel := context.WithTimeout(ctx, 10*time.Second)
	defer waitCancel()
	go func() { // NoReconnect 时会话失败 Run 就返回，不必等满 10 秒
		select {
		case <-a.done:
			waitCancel()
		case <-waitCtx.Done():
		}
	}()
	if err := a.Client.WaitConnected(waitCtx); err != nil {
		a.Stop()
		if runErr != nil { // Stop 等到 done 关闭后读取，无竞争
			return nil, fmt.Errorf("fakeagent: not connected: %w", runErr)
		}
		if ctx.Err() != nil { // 调用方取消，而不是 10 秒超时
			return nil, fmt.Errorf("fakeagent: not connected: %w", ctx.Err())
		}
		return nil, errors.New("fakeagent: not connected within 10s")
	}
	return a, nil
}

// Stop 断开并停止执行器；调用方给的 Dir 保留，Start 自建的临时目录删除。
func (a *Agent) Stop() {
	a.cancel()
	a.wg.Wait()
	if a.tempDir {
		os.RemoveAll(a.Dir)
	}
}
