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
	"sort"
	"sync"
	"time"

	"spark-cloud/internal/agent/client"
	clusternetfake "spark-cloud/internal/agent/clusternet/fake"
	diskfake "spark-cloud/internal/agent/disk/fake"
	"spark-cloud/internal/agent/executor"
	"spark-cloud/internal/agent/metrics"
	netfake "spark-cloud/internal/agent/netfilter/fake"
	rtfake "spark-cloud/internal/agent/runtime/fake"
	"spark-cloud/internal/agent/state"
	"spark-cloud/internal/clusterspec"
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
	// Features 是 Hello.features；nil → features.SharedSlots | features.Cluster（本版本全部能力）。
	// new(uint32)（即 0）模拟旧 Agent；只给 SharedSlots 模拟不支持集群的 Agent（平台不把集群分给它）。
	Features *uint32
	// Net 是网卡探测结果（Hello.net / StateReport.net）；nil → 一块有线 eth0（IPv4 = IPs[0]/24，up）。
	Net *clusterspec.NetInfo
	// ClusterNet 是执行器的集群数据面（记录 Apply / Teardown / SyncRules，可注入失败）；nil → 新建一个。
	// 传入的 ClusterNet 已 SetInfo 过（有网卡）且 Net 为 nil 时保留其探测结果；否则按 Net / 默认 eth0 设置。
	ClusterNet *clusternetfake.Net
	Log        *slog.Logger
}

type Agent struct {
	Client  *client.Client
	Exec    *executor.Executor
	Runtime *rtfake.Runtime
	Disk    *diskfake.Disk
	Net     *netfake.Netfilter
	Cluster *clusternetfake.Net
	Dir     string
	tempDir bool // Dir 由 Start 创建，Stop 时删除
	creds   *Creds
	cancel  context.CancelFunc
	done    chan struct{}  // Client.Run 已返回
	wg      sync.WaitGroup // Client.Run 与（自动模式下）Executor.Run

	mu       sync.Mutex
	instMets map[string]metrics.InstanceMetrics // SetInstanceMetrics 注入的每实例指标
}

// SetInstanceMetrics 设置之后心跳里该实例的用量（InstanceID 以 id 为准）。心跳的 instances 为本地记录里的全部实例
// （未注入的各项为 0）加上注入了但本地没有记录的实例，按 ID 排序。
func (a *Agent) SetInstanceMetrics(id string, m metrics.InstanceMetrics) {
	a.mu.Lock()
	defer a.mu.Unlock()
	m.InstanceID = id
	a.instMets[id] = m
}

// ClearInstanceMetrics 撤销 SetInstanceMetrics 的注入。
func (a *Agent) ClearInstanceMetrics(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.instMets, id)
}

// instanceMetrics 组装心跳的每实例指标（见 SetInstanceMetrics）。
func (a *Agent) instanceMetrics() []metrics.InstanceMetrics {
	a.mu.Lock()
	defer a.mu.Unlock()
	seen := map[string]bool{}
	var out []metrics.InstanceMetrics
	for _, mt := range a.Exec.Mounts() {
		seen[mt.ID] = true
		m, ok := a.instMets[mt.ID]
		if !ok {
			m = metrics.InstanceMetrics{InstanceID: mt.ID}
		}
		out = append(out, m)
	}
	for id, m := range a.instMets {
		if !seen[id] {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].InstanceID < out[j].InstanceID })
	return out
}

func (a *Agent) Creds() *Creds { return a.creds }

// ClusterNet 返回执行器的 fake 集群数据面（断言 Apply / Teardown / SyncRules，注入失败，改网卡探测结果）。
func (a *Agent) ClusterNet() *clusternetfake.Net { return a.Cluster }

// DefaultNetInfo 是 Options.Net 为 nil 时的探测结果：一块有线 eth0，IPv4 = ip/24（CIDR），up。
func DefaultNetInfo(ip string) clusterspec.NetInfo {
	return clusterspec.NetInfo{Ifaces: []clusterspec.Iface{{Name: "eth0", MAC: "02:00:00:00:00:01", Up: true, SpeedMbps: 1000, MTU: 1500,
		IPv4: []string{ip + "/24"}, Driver: "r8169", PCI: "0000:01:00.0"}}}
}

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
	cn := o.ClusterNet
	if cn == nil {
		cn = clusternetfake.New()
	}
	switch {
	case o.Net != nil:
		cn.SetInfo(*o.Net)
	case o.ClusterNet == nil || len(cn.Info().Ifaces) == 0:
		cn.SetInfo(DefaultNetInfo(o.IPs[0]))
	}
	a := &Agent{Runtime: rtfake.New(), Disk: diskfake.New(o.FreeBytes), Net: netfake.New(), Cluster: cn, Dir: dir, creds: creds, done: make(chan struct{}),
		instMets: map[string]metrics.InstanceMetrics{}}
	a.Runtime.SetImages(o.Images)
	ips := o.IPs
	a.Exec = executor.New(executor.Deps{Runtime: a.Runtime, Disk: a.Disk, Net: a.Net, Cluster: a.Cluster, State: st, StateDir: dir, Bridge: "spark0",
		LocalIPs: func() []string { return ips }, MemTotal: func() uint64 { return 128 << 30 }, NumCPU: func() int { return 20 }, // DGX Spark：128 GiB、20 核
		CgroupRoot: filepath.Join(dir, "cgroup"), Log: o.Log}, o.Stepped)
	m := o.Metrics
	a.Client = client.New(client.Config{Addr: o.Addr, TLS: tc, Version: o.Version, Hostname: "fake", LocalIPs: func() []string { return ips },
		Stepped: o.Stepped, NoReconnect: o.NoReconnect, HeartbeatInterval: o.HeartbeatInterval, Features: o.Features,
		Backoff: client.Backoff{Min: 50 * time.Millisecond, Max: time.Second}},
		a.Exec, func(context.Context) (metrics.Metrics, []metrics.InstanceMetrics) { return m, a.instanceMetrics() }, o.Log)
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
