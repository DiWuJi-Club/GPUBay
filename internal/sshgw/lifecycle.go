package sshgw

import (
	"context"
	"net"
	"sync"
	"time"

	"spark-cloud/internal/rental"
)

const (
	scanInterval  = 10 * time.Second
	touchInterval = 60 * time.Second
	msgDraining   = "网关正在重启，连接已断开"
)

func (s *Server) snapshotSessions() []*session {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*session, 0, len(s.sessions))
	for x := range s.sessions {
		out = append(out, x)
	}
	return out
}

func (s *Server) trackConn(nc net.Conn, add bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if add {
		s.conns[nc] = struct{}{}
	} else {
		delete(s.conns, nc)
	}
}

// ScanNow：实例离开 running（到期停机、释放、换机后的旧实例）或已不存在 → 提示并断开；暂停（机器失联）不断。
// 一次批量查询全部会话涉及的实例（不解密密码）；查询出错（超时、连接断）不是终态：只记日志、本轮跳过，绝不因此踢会话。
// 断开并发进行（每条最多卡 disconnectWriteWait），不读数据的客户端拖不住整轮扫描。
func (s *Server) ScanNow(ctx context.Context) {
	all := s.snapshotSessions()
	if len(all) == 0 {
		return
	}
	seen := map[string]bool{}
	ids := make([]string, 0, len(all))
	for _, x := range all {
		if !seen[x.instanceID] {
			seen[x.instanceID] = true
			ids = append(ids, x.instanceID)
		}
	}
	states, err := s.d.Store.SessionStates(ctx, ids)
	if err != nil {
		s.d.Log.Error("ssh scan session states", "instances", len(ids), "err", err)
		return
	}
	var victims []*session
	for _, x := range all {
		st, ok := states[x.instanceID]
		if !ok || terminalState(st.State) {
			s.d.Log.Info("ssh session disconnected: instance no longer running", "instance", x.instanceID, "user", x.userID, "found", ok)
			victims = append(victims, x)
		}
	}
	disconnectAll(victims, MsgDisconnected)
}

// terminalState：停机、清盘、释放、失败的实例不再允许保持 SSH 会话（creating 不会有会话；暂停仍是 running）。
func terminalState(state string) bool {
	switch state {
	case rental.StateStopped, rental.StateWiping, rental.StateReleased, rental.StateFailed:
		return true
	}
	return false
}

// disconnectAll 并发断开并等待全部完成。
func disconnectAll(xs []*session, msg string) {
	var wg sync.WaitGroup
	for _, x := range xs {
		wg.Add(1)
		go func() { defer wg.Done(); x.disconnect(msg) }()
	}
	wg.Wait()
}

// TouchNow 批量刷新会话 last_seen_at（清扫器据此收尾崩溃残留）。
func (s *Server) TouchNow(ctx context.Context) {
	var ids []int64
	for _, x := range s.snapshotSessions() {
		if x.id != 0 {
			ids = append(ids, x.id)
		}
	}
	if len(ids) == 0 {
		return
	}
	if err := s.d.Store.TouchSSHSessions(ctx, ids); err != nil {
		s.d.Log.Error("touch ssh sessions", "err", err)
	}
}

// runLoops 是网关唯一的后台循环：状态扫描、last_seen_at 刷新、限流表清扫，ctx 结束即返回。
func (s *Server) runLoops(ctx context.Context) {
	scan := time.NewTicker(scanInterval)
	touch := time.NewTicker(touchInterval)
	sweep := time.NewTicker(limiterSweepEvery)
	defer scan.Stop()
	defer touch.Stop()
	defer sweep.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-scan.C:
			c, cancel := context.WithTimeout(ctx, lookupTimeout)
			s.ScanNow(c)
			cancel()
		case <-touch.C:
			c, cancel := context.WithTimeout(ctx, lookupTimeout)
			s.TouchNow(c)
			cancel()
		case <-sweep.C:
			s.lim.sweep(s.d.Clock.Now())
		}
	}
}

// drain：最多等 DrainTimeout 让连接自然结束，然后向剩余会话写提示并断开，
// 再关闭其余底层连接（握手中、拒绝提示中、正在拨上游的），保证 Serve 在 DrainTimeout 左右返回。部署会断 SSH（README 注明）。
func (s *Server) drain() {
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	t := time.NewTimer(s.cfg.DrainTimeout)
	defer t.Stop()
	select {
	case <-done:
		return
	case <-t.C:
	}
	disconnectAll(s.snapshotSessions(), msgDraining)
	s.mu.Lock()
	conns := make([]net.Conn, 0, len(s.conns))
	for nc := range s.conns {
		conns = append(conns, nc)
	}
	s.mu.Unlock()
	for _, nc := range conns {
		nc.Close()
	}
	<-done
}
