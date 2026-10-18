package agentgw

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"spark-cloud/internal/agentpb"
	"spark-cloud/internal/fleet"
	"spark-cloud/internal/pki"
	"spark-cloud/internal/platform/apperr"
)

const helloTimeout = 10 * time.Second

// conn 是本副本持有的一条 Agent 流。
type conn struct {
	id          string
	machineID   int64
	serial      string
	fingerprint string
	stream      agentpb.AgentGateway_ConnectServer
	sendMu      sync.Mutex

	mu     sync.Mutex
	pushed map[string]bool // 已向该流推送、尚未收到结果的指令 id（补推集合）

	ready      atomic.Bool // 首个 StateReport 已处理，可投递
	readyTick  atomic.Int64
	reports    atomic.Int64
	heartbeats atomic.Int64

	done     chan struct{}
	doneOnce sync.Once
	abortErr error
}

func (c *conn) abort(err error) {
	c.doneOnce.Do(func() {
		c.abortErr = err
		close(c.done)
	})
}

func (c *conn) send(m *agentpb.PlatformMessage) error {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	return c.stream.Send(m)
}

func (c *conn) markPushed(id string) {
	c.mu.Lock()
	c.pushed[id] = true
	c.mu.Unlock()
}

func (c *conn) wasPushed(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pushed[id]
}

func (c *conn) unpush(id string) {
	c.mu.Lock()
	delete(c.pushed, id)
	c.mu.Unlock()
}

// retainPushed 只保留仍为 sent 的指令：被 CancelCommands 取消或已超时的指令不会再有结果，否则集合在连接存活期内只增不减。
func (c *conn) retainPushed(ids []string) {
	keep := make(map[string]bool, len(ids))
	for _, id := range ids {
		keep[id] = true
	}
	c.mu.Lock()
	for id := range c.pushed {
		if !keep[id] {
			delete(c.pushed, id)
		}
	}
	c.mu.Unlock()
}

type registry struct {
	mu        sync.Mutex
	byMachine map[int64]*conn
	byConn    map[string]*conn
}

func newRegistry() *registry {
	return &registry{byMachine: map[int64]*conn{}, byConn: map[string]*conn{}}
}

// replace 登记新流并返回被顶掉的旧流（同一台机器）。
func (r *registry) replace(c *conn) *conn {
	r.mu.Lock()
	defer r.mu.Unlock()
	old := r.byMachine[c.machineID]
	r.byMachine[c.machineID] = c
	r.byConn[c.id] = c
	if old != nil {
		delete(r.byConn, old.id)
	}
	return old
}

// remove 只在该流仍是机器当前流时移除。
func (r *registry) remove(c *conn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byMachine[c.machineID] == c {
		delete(r.byMachine, c.machineID)
	}
	delete(r.byConn, c.id)
}

func (r *registry) byID(id string) *conn {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byConn[id]
}

func (r *registry) get(machineID int64) *conn {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byMachine[machineID]
}

func (r *registry) snapshot() []*conn {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*conn, 0, len(r.byMachine))
	for _, c := range r.byMachine {
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b *conn) int { return int(a.machineID - b.machineID) })
	return out
}

func (r *registry) abortAll(err error) {
	for _, c := range r.snapshot() {
		c.abort(err)
	}
}

// Connected 报告本副本是否持有该机器的流。
func (s *Server) Connected(machineID int64) bool { return s.conns.get(machineID) != nil }

func (s *Server) ReportCount(machineID int64) int64 {
	if c := s.conns.get(machineID); c != nil {
		return c.reports.Load()
	}
	return 0
}

func (s *Server) HeartbeatCount(machineID int64) int64 {
	if c := s.conns.get(machineID); c != nil {
		return c.heartbeats.Load()
	}
	return 0
}

// ReadyTick 返回该机器当前流处理完首个 StateReport 时的 tick 号。
func (s *Server) ReadyTick(machineID int64) (int64, bool) {
	c := s.conns.get(machineID)
	if c == nil || !c.ready.Load() {
		return 0, false
	}
	return c.readyTick.Load(), true
}

func (s *Server) Ticks() int64 { return s.ticks.Load() }

// authenticate 校验客户端证书：由 CA 签发（TLS 层已验）→ CN 对应机器 → 指纹非空且匹配。
func (s *Server) authenticate(ctx context.Context) (fleet.Machine, string, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return fleet.Machine{}, "", status.Error(codes.Unauthenticated, "no peer")
	}
	ti, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(ti.State.VerifiedChains) == 0 || len(ti.State.PeerCertificates) == 0 {
		return fleet.Machine{}, "", status.Error(codes.Unauthenticated, "client certificate required")
	}
	leaf := ti.State.PeerCertificates[0]
	fp := pki.Fingerprint(leaf.Raw)
	if now := s.d.CertClock.Now(); now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
		return fleet.Machine{}, "", status.Error(codes.Unauthenticated, "certificate expired")
	}
	m, err := s.d.Fleet.MachineBySerial(ctx, leaf.Subject.CommonName)
	if errors.Is(err, apperr.ErrNotFound) || (err == nil && (m.CertFingerprint == "" || m.CertFingerprint != fp)) {
		return fleet.Machine{}, "", status.Error(codes.Unauthenticated, "certificate not recognised")
	}
	if err != nil {
		return fleet.Machine{}, "", status.Error(codes.Internal, "lookup failed")
	}
	return m, fp, nil
}

// Connect 处理一条 Agent 流：建流校验 → Hello → 登记归属与心跳 → 下发 AgentConfig → 循环处理消息。
func (s *Server) Connect(stream agentpb.AgentGateway_ConnectServer) error {
	ctx := stream.Context()
	m, fp, err := s.authenticate(ctx)
	if err != nil {
		return err
	}
	first, err := recvTimeout(ctx, stream, helloTimeout)
	if err != nil {
		return err
	}
	hello := first.GetHello()
	if hello == nil {
		return status.Error(codes.InvalidArgument, "first message must be Hello")
	}
	c := &conn{id: uuid.NewString(), machineID: m.ID, serial: m.Serial, fingerprint: fp, stream: stream, pushed: map[string]bool{}, done: make(chan struct{})}
	if err := s.d.Fleet.AttachAgentConn(ctx, m.ID, c.id); err != nil {
		return status.Error(codes.Internal, "attach failed")
	}
	// 归属已写入数据库：从这里起任何返回路径都要清掉自己的归属（只清自己的）
	defer func() {
		c.abort(nil) // 释放仍阻塞在 msgs 上的接收协程
		s.conns.remove(c)
		if err := s.d.Fleet.DetachAgentConn(context.WithoutCancel(ctx), m.ID, c.id); err != nil {
			s.d.Log.Error("detach agent conn", "machine", m.ID, "err", err)
		}
	}()
	if _, err := s.d.Fleet.Heartbeat(ctx, m.ID); err != nil {
		return status.Error(codes.Internal, "heartbeat failed")
	}
	if old := s.conns.replace(c); old != nil {
		old.abort(status.Error(codes.Aborted, "replaced by a newer connection"))
	}
	if err := s.d.Fleet.RecordMetrics(ctx, m.ID, hello.GetAgentVersion(), nil); err != nil {
		s.d.Log.Error("record agent version", "machine", m.ID, "err", err)
	}
	if err := c.send(&agentpb.PlatformMessage{Msg: &agentpb.PlatformMessage_Config{Config: s.agentConfig(m)}}); err != nil {
		return err
	}
	s.checkLanIP(ctx, m, hello.GetIps())

	msgs := make(chan *agentpb.AgentMessage)
	recvErr := make(chan error, 1)
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				recvErr <- err
				return
			}
			select {
			case msgs <- msg:
			case <-c.done:
				return
			}
		}
	}()
	for {
		select {
		case <-c.done:
			return c.abortErr
		case <-ctx.Done():
			return nil
		case err := <-recvErr:
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		case msg := <-msgs:
			s.handleMessage(ctx, c, msg)
		}
	}
}

func recvTimeout(ctx context.Context, stream agentpb.AgentGateway_ConnectServer, d time.Duration) (*agentpb.AgentMessage, error) {
	type res struct {
		m   *agentpb.AgentMessage
		err error
	}
	ch := make(chan res, 1)
	go func() { m, err := stream.Recv(); ch <- res{m, err} }()
	select {
	case r := <-ch:
		return r.m, r.err
	case <-time.After(d):
		return nil, status.Error(codes.DeadlineExceeded, "hello timeout")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Server) agentConfig(m fleet.Machine) *agentpb.AgentConfig {
	return &agentpb.AgentConfig{LanIp: m.LanIP, HeartbeatIntervalS: uint32(s.cfg.HeartbeatInterval / time.Second),
		AllowCidrs: s.cfg.AllowCIDRs, DnsServers: s.cfg.DNSServers, BandwidthMbps: uint32(s.cfg.BandwidthMbps), DataSizeGib: uint32(s.cfg.DataSizeGiB)}
}

func (s *Server) checkLanIP(ctx context.Context, m fleet.Machine, ips []string) {
	if slices.Contains(ips, m.LanIP) {
		return
	}
	s.alertOnce(ctx, "warning", "lan_ip_mismatch", m.ID, fmt.Sprintf("机器 %s 登记的内网 IP 与 Agent 上报的地址不一致", m.Serial))
}

func (s *Server) alertOnce(ctx context.Context, level, kind string, machineID int64, message string) {
	if err := s.d.Fleet.RaiseAlertOnce(ctx, s.d.Pool, fleet.Alert{Level: level, Kind: kind, MachineID: machineID, Message: message}); err != nil {
		s.d.Log.Error("raise alert", "kind", kind, "machine", machineID, "err", err)
	}
}

func (s *Server) handleMessage(ctx context.Context, c *conn, msg *agentpb.AgentMessage) {
	switch m := msg.Msg.(type) {
	case *agentpb.AgentMessage_State:
		s.handleReport(ctx, c, m.State)
	case *agentpb.AgentMessage_Heartbeat:
		s.handleHeartbeat(ctx, c, m.Heartbeat)
	case *agentpb.AgentMessage_Result:
		s.handleResult(ctx, c, m.Result)
	case *agentpb.AgentMessage_Hello:
		s.d.Log.Warn("duplicate hello ignored", "machine", c.machineID)
	}
}

// handleReport：先存原文，再对账，再按 running 列表恢复计时；处理完首个报告后才允许投递。
func (s *Server) handleReport(ctx context.Context, c *conn, r *agentpb.StateReport) {
	rep := toStateReport(r)
	if err := s.d.Fleet.RecordStateReport(ctx, c.machineID, rep); err != nil {
		s.d.Log.Error("record state report", "machine", c.machineID, "err", err)
	}
	if err := s.d.Rental.Reconcile(ctx, c.machineID, rep); err != nil {
		s.d.Log.Error("reconcile", "machine", c.machineID, "err", err)
	}
	if _, err := s.d.Rental.ResumeMachine(ctx, c.machineID, rep.RunningIDs()); err != nil {
		s.d.Log.Error("resume machine", "machine", c.machineID, "err", err)
	}
	if c.ready.CompareAndSwap(false, true) {
		c.readyTick.Store(s.ticks.Load())
	}
	c.reports.Add(1)
}

func (s *Server) handleHeartbeat(ctx context.Context, c *conn, h *agentpb.Heartbeat) {
	if _, err := s.d.Fleet.Heartbeat(ctx, c.machineID); err != nil {
		s.d.Log.Error("heartbeat", "machine", c.machineID, "err", err)
	}
	m := h.GetMetrics()
	if m != nil {
		if doc, err := metricsJSON(m); err == nil {
			if err := s.d.Fleet.RecordMetrics(ctx, c.machineID, "", doc); err != nil {
				s.d.Log.Error("record metrics", "machine", c.machineID, "err", err)
			}
		}
		if m.GetRootDiskTotal() > 0 && float64(m.GetRootDiskUsed())/float64(m.GetRootDiskTotal()) > 0.9 {
			s.alertOnce(ctx, "warning", "root_disk_high", c.machineID, fmt.Sprintf("机器 %s 根分区使用率超过 90%%", c.serial))
		}
		if t := m.GetGpuTempC(); t > 90 && t < 1000 {
			s.alertOnce(ctx, "warning", "gpu_temp_high", c.machineID, fmt.Sprintf("机器 %s GPU 温度超过 90°C", c.serial))
		}
	}
	c.heartbeats.Add(1)
}

func (s *Server) handleResult(ctx context.Context, c *conn, r *agentpb.CommandResult) {
	// 先完成指令再从已推送集合移除：反过来会让同一时刻的 tick 把仍为 sent 的指令再推一次并刷新 sent_at
	defer c.unpush(r.GetCommandId())
	err := s.d.Rental.HandleCommandResult(ctx, r.GetCommandId(), r.GetOk(), r.GetMessage())
	if errors.Is(err, apperr.ErrNotFound) {
		s.d.Log.Warn("result for unknown command ignored", "machine", c.machineID, "command", r.GetCommandId())
		return
	}
	if err != nil {
		s.d.Log.Error("handle command result", "machine", c.machineID, "command", r.GetCommandId(), "err", err)
	}
}
