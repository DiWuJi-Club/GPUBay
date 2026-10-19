// Package client 是 spark-agent 与网关的连接层：重连、Hello、StateReport、心跳、指令接收与结果回报。
package client

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"

	"spark-cloud/internal/agent/executor"
	"spark-cloud/internal/agent/metrics"
	"spark-cloud/internal/agent/state"
	"spark-cloud/internal/agentpb"
)

const (
	// metricsTimeout 是单次指标采集的上限：nvidia-smi 卡死也不能拖住心跳。
	metricsTimeout = 5 * time.Second
	// configTimeout 是建流后等待 AgentConfig 的上限。
	configTimeout = 30 * time.Second
	// maxReceived 是 Received 记住的指令 ID 上限（只用于测试与排障，不影响去重：去重在执行器里）。
	maxReceived = 1000
)

type Config struct {
	Addr              string
	TLS               *tls.Config
	Version           string
	Hostname          string
	LocalIPs          func() []string
	Stepped           bool
	HeartbeatInterval time.Duration // 零值 10s 且采用平台下发的 heartbeat_interval_s；非零时为调用方显式指定，不被覆盖
	ReportInterval    time.Duration // 零值 60s
	Backoff           Backoff
	NoReconnect       bool // 会话结束后不重连；测试里模拟「被顶掉 / 被撤销就退出」的旧 Agent，避免与新 Agent 互相顶替
}

type Client struct {
	cfg     Config
	ex      *executor.Executor
	collect func(context.Context) metrics.Metrics
	log     *slog.Logger

	mu         sync.Mutex
	stream     agentpb.AgentGateway_ConnectClient
	received   map[string]bool
	recvOrder  []string // received 的插入顺序，超过 maxReceived 时丢最旧的
	sendMu     sync.Mutex
	collecting atomic.Bool // 单飞：上一次指标采集尚未返回

	connected      atomic.Bool
	configured     atomic.Bool
	hbInterval     atomic.Int64 // time.Duration
	explicitHB     bool         // Config.HeartbeatInterval 由调用方指定，不被平台下发值覆盖
	metricsTimeout time.Duration
}

func New(cfg Config, ex *executor.Executor, collect func(context.Context) metrics.Metrics, log *slog.Logger) *Client {
	explicitHB := cfg.HeartbeatInterval > 0
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = 10 * time.Second
	}
	if cfg.ReportInterval <= 0 {
		cfg.ReportInterval = 60 * time.Second
	}
	if cfg.LocalIPs == nil {
		cfg.LocalIPs = func() []string { return nil }
	}
	if log == nil {
		log = slog.Default()
	}
	c := &Client{cfg: cfg, ex: ex, collect: collect, log: log, received: map[string]bool{}, explicitHB: explicitHB, metricsTimeout: metricsTimeout}
	c.hbInterval.Store(int64(cfg.HeartbeatInterval))
	return c
}

// Connected：当前有一条网关已接受（已收到 AgentConfig）的流。
func (c *Client) Connected() bool      { return c.connected.Load() }
func (c *Client) ConfigReceived() bool { return c.configured.Load() }

// Received 报告该指令是否已到达本客户端（含重复推送）。
func (c *Client) Received(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.received[id]
}

// WaitConnected 等到当前会话收到 AgentConfig。
func (c *Client) WaitConnected(ctx context.Context) error {
	for !c.configured.Load() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
	return nil
}

// Run 重连循环：每次会话结束后按退避重试；平台不可达期间不做任何自主停机。
func (c *Client) Run(ctx context.Context) error {
	attempt := 0
	for {
		start := time.Now()
		err := c.session(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if c.cfg.NoReconnect {
			return err
		}
		if time.Since(start) > time.Minute {
			attempt = 0 // 稳定连接过一段时间，退避从头算
		}
		d := c.cfg.Backoff.Next(attempt)
		attempt++
		if status.Code(err) == codes.Unauthenticated {
			// 证书被撤销或未被识别：仍按退避重连，管理员重新接入后能自动恢复
			c.log.Error("platform rejected agent certificate; re-enroll this machine", "err", err, "after", d)
		} else {
			c.log.Warn("agent session ended, reconnecting", "err", err, "after", d)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(d):
		}
	}
}

// session 一次建流：Hello → StateReport → 等 AgentConfig → 补发未发送结果 → 循环。
func (c *Client) session(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cc, err := grpc.NewClient(c.cfg.Addr, grpc.WithTransportCredentials(credentials.NewTLS(c.cfg.TLS)),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 30 * time.Second, Timeout: 10 * time.Second, PermitWithoutStream: true}))
	if err != nil {
		return err
	}
	defer cc.Close()
	stream, err := agentpb.NewAgentGatewayClient(cc).Connect(ctx)
	if err != nil {
		return err
	}
	// 网关要求第一条消息是 Hello，并且处理完第一份 StateReport 之后才开始投递指令。
	// 先在本地流上发完这两条再公开 c.stream，避免并发的 Step / SendHeartbeat 抢在 Hello 之前发出。
	if err := c.sendOn(stream, &agentpb.AgentMessage{Msg: &agentpb.AgentMessage_Hello{Hello: &agentpb.Hello{AgentVersion: c.cfg.Version, Hostname: c.cfg.Hostname, Ips: c.cfg.LocalIPs()}}}); err != nil {
		return streamErr(stream, err)
	}
	if err := c.sendOn(stream, c.stateReport(ctx)); err != nil {
		return streamErr(stream, err)
	}
	c.mu.Lock()
	c.stream = stream
	c.mu.Unlock()
	defer func() {
		c.connected.Store(false)
		c.configured.Store(false)
		c.mu.Lock()
		c.stream = nil
		c.mu.Unlock()
	}()

	configured := make(chan struct{})
	errc := make(chan error, 1)
	go func() {
		first := true
		for {
			msg, err := stream.Recv()
			if err != nil {
				errc <- err
				return
			}
			if first {
				first = false
				if msg.GetConfig() == nil {
					errc <- errors.New("first platform message is not AgentConfig")
					return
				}
				c.applyConfig(msg.GetConfig())
				close(configured)
				continue
			}
			c.handle(msg)
		}
	}()
	select {
	case <-ctx.Done():
		return nil
	case err := <-errc:
		return err
	case <-time.After(configTimeout):
		return errors.New("timed out waiting for AgentConfig")
	case <-configured:
	}
	c.configured.Store(true)
	c.connected.Store(true)
	// 补发断线期间（或 Results() 满时）没发出去的结果
	if err := c.resendUnsent(ctx); err != nil {
		return err
	}

	hbEvery := time.Duration(c.hbInterval.Load())
	hb := time.NewTicker(hbEvery)
	rep := time.NewTicker(c.cfg.ReportInterval)
	defer hb.Stop()
	defer rep.Stop()
	// 步进模式下结果由 Step 发送；这里不监听 Results()（nil 通道永远阻塞）
	var results <-chan executor.ResultMsg
	if !c.cfg.Stepped {
		results = c.ex.Results()
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-errc:
			return err
		case r := <-results:
			if err := c.sendResult(r.ID, state.Result{OK: r.OK, Message: r.Message}); err != nil {
				return err
			}
			if err := c.SendStateReport(ctx); err != nil {
				return err
			}
		case <-hb.C:
			if d := time.Duration(c.hbInterval.Load()); d != hbEvery { // 平台下发了新的心跳间隔
				hbEvery = d
				hb.Reset(d)
			}
			if c.cfg.Stepped {
				continue
			}
			if err := c.SendHeartbeat(ctx); err != nil {
				return err
			}
			// Results() 满时结果只落在 state.json（Sent=false）：每次心跳顺手补发，不必等到重连
			if err := c.resendUnsent(ctx); err != nil {
				return err
			}
		case <-rep.C:
			if c.cfg.Stepped {
				continue
			}
			if err := c.SendStateReport(ctx); err != nil {
				return err
			}
		}
	}
}

// streamErr：流被服务端终止时 SendMsg 只返回 io.EOF，真实状态要用 Recv 取；Recv 意外无错时仍返回原错误。
func streamErr(s agentpb.AgentGateway_ConnectClient, err error) error {
	if errors.Is(err, io.EOF) {
		if _, rerr := s.Recv(); rerr != nil {
			return rerr
		}
	}
	return err
}

// resendUnsent 发送 state.json 里所有未标记已发送的结果（发送成功才标记），有补发时再跟一份 StateReport。
func (c *Client) resendUnsent(ctx context.Context) error {
	unsent := c.ex.StateStore().UnsentResults()
	for _, r := range unsent {
		if err := c.sendResult(r.ID, r.Result); err != nil {
			return err
		}
	}
	if len(unsent) == 0 {
		return nil
	}
	return c.SendStateReport(ctx)
}

func (c *Client) applyConfig(cfg *agentpb.AgentConfig) {
	c.ex.SetConfig(executor.Config{LanIP: cfg.GetLanIp(), DNS: cfg.GetDnsServers(), AllowCIDRs: cfg.GetAllowCidrs(),
		BandwidthMbps: int(cfg.GetBandwidthMbps()), DataSizeGiB: int(cfg.GetDataSizeGib())})
	// 平台下发的心跳间隔只在调用方没有显式指定时生效（测试与 fakeagent 用很短的间隔）
	if s := cfg.GetHeartbeatIntervalS(); s > 0 && !c.explicitHB {
		c.hbInterval.Store(int64(time.Duration(s) * time.Second))
	}
}

// handle 处理建流后的平台消息。同一指令可能被推送多次，由执行器去重 / 回放结果。
func (c *Client) handle(msg *agentpb.PlatformMessage) {
	switch m := msg.Msg.(type) {
	case *agentpb.PlatformMessage_Config:
		c.applyConfig(m.Config)
	case *agentpb.PlatformMessage_Command:
		cmd := m.Command
		c.markReceived(cmd.GetId())
		c.ex.Submit(toCommand(cmd))
	}
}

func (c *Client) markReceived(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.received[id] {
		return
	}
	c.received[id] = true
	c.recvOrder = append(c.recvOrder, id)
	if len(c.recvOrder) > maxReceived {
		delete(c.received, c.recvOrder[0])
		c.recvOrder = c.recvOrder[1:]
	}
}

func toCommand(cmd *agentpb.Command) executor.Command {
	out := executor.Command{ID: cmd.GetId(), InstanceID: cmd.GetInstanceId()}
	switch b := cmd.Body.(type) {
	case *agentpb.Command_Create:
		out.Type = "create_instance"
		out.ImageRef, out.UpstreamPubKey = b.Create.GetImageRef(), b.Create.GetUpstreamPubkey()
		out.BandwidthMbps, out.SSHPort, out.DataSizeGiB = int(b.Create.GetBandwidthMbps()), int(b.Create.GetSshPort()), int(b.Create.GetDataSizeGib())
		out.HostKeyPEM = b.Create.GetHostKeyPem()
	case *agentpb.Command_Start:
		out.Type = "start_instance"
	case *agentpb.Command_Stop:
		out.Type = "stop_instance"
	case *agentpb.Command_Wipe:
		out.Type = "wipe_instance"
	case *agentpb.Command_Pull:
		out.Type = "pull_image"
		out.ImageRef = b.Pull.GetImageRef()
	}
	return out
}

func (c *Client) send(m *agentpb.AgentMessage) error {
	c.mu.Lock()
	s := c.stream
	c.mu.Unlock()
	if s == nil {
		return errors.New("not connected")
	}
	return c.sendOn(s, m)
}

func (c *Client) sendOn(s agentpb.AgentGateway_ConnectClient, m *agentpb.AgentMessage) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	return s.Send(m)
}

// sendResult 发送一条结果，发送成功后才在 state.json 里标记已发送。
// 只有发送失败才算会话错误：标记落盘失败（如根分区写满）只记日志，否则每次重连补发后都会断流，机器反复掉线。
func (c *Client) sendResult(id string, r state.Result) error {
	if err := c.send(&agentpb.AgentMessage{Msg: &agentpb.AgentMessage_Result{Result: &agentpb.CommandResult{CommandId: id, Ok: r.OK, Message: r.Message}}}); err != nil {
		return err
	}
	if err := c.ex.StateStore().MarkSent(id); err != nil {
		c.log.Error("persist result sent flag", "command", id, "err", err)
	}
	return nil
}

// collectMetrics 在限定时间内采集指标；超时（采集函数不理会 ctx 也一样）返回空指标，心跳照发。
// 上一次采集仍未返回时不再启动新的采集（单飞），卡死的采集最多占一个 goroutine。
func (c *Client) collectMetrics(ctx context.Context) metrics.Metrics {
	if !c.collecting.CompareAndSwap(false, true) {
		c.log.Warn("previous metrics collection still running, sending heartbeat without metrics")
		return metrics.Metrics{}
	}
	ctx, cancel := context.WithTimeout(ctx, c.metricsTimeout)
	defer cancel()
	ch := make(chan metrics.Metrics, 1)
	go func() {
		defer c.collecting.Store(false)
		ch <- c.collect(ctx)
	}()
	select {
	case m := <-ch:
		return m
	case <-ctx.Done():
		c.log.Warn("metrics collection timed out, sending heartbeat without metrics", "timeout", c.metricsTimeout)
		return metrics.Metrics{}
	}
}

func (c *Client) SendHeartbeat(ctx context.Context) error {
	m := c.collectMetrics(ctx)
	return c.send(&agentpb.AgentMessage{Msg: &agentpb.AgentMessage_Heartbeat{Heartbeat: &agentpb.Heartbeat{Metrics: &agentpb.Metrics{
		GpuUtil: m.GPUUtil, GpuTempC: m.GPUTempC, GpuPowerW: m.GPUPowerW, MemTotal: m.MemTotal, MemUsed: m.MemUsed,
		RootDiskTotal: m.RootDiskTotal, RootDiskUsed: m.RootDiskUsed, DataDiskUsed: m.DataDiskUsed, Load1: m.Load1}}}})
}

func (c *Client) SendStateReport(ctx context.Context) error {
	return c.send(c.stateReport(ctx))
}

func (c *Client) stateReport(ctx context.Context) *agentpb.AgentMessage {
	var states []*agentpb.InstanceState
	for _, s := range c.ex.Report(ctx) {
		states = append(states, &agentpb.InstanceState{InstanceId: s.InstanceID, Phase: s.Phase})
	}
	return &agentpb.AgentMessage{Msg: &agentpb.AgentMessage_State{State: &agentpb.StateReport{Instances: states, Images: c.ex.Images(ctx)}}}
}

// Step 步进模式：执行队列（override 非空时直接记录该结果）、逐条发送结果（含回放）、再发 StateReport。
// 返回执行与回放的条数；发送失败时结果仍在 state.json 里未标记 Sent，重连后补发。
func (c *Client) Step(ctx context.Context, override *state.Result) (int, error) {
	results := c.ex.RunPending(ctx, override)
	for i, r := range results {
		if err := c.sendResult(r.ID, state.Result{OK: r.OK, Message: r.Message}); err != nil {
			return i, err
		}
	}
	return len(results), c.SendStateReport(ctx)
}
