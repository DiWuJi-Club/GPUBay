package sshgw

import (
	"context"
	"errors"
	"net"
	"time"

	"spark-cloud/internal/platform/apperr"
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
// 数据库瞬时错误（超时、连接断）不是终态：只记日志，绝不因此踢会话。
func (s *Server) ScanNow(ctx context.Context) {
	for _, x := range s.snapshotSessions() {
		if ctx.Err() != nil { // 网关退出中：剩余会话交给 drain
			return
		}
		info, err := s.d.Store.Access(ctx, x.instanceID)
		if err != nil && !errors.Is(err, apperr.ErrNotFound) {
			s.d.Log.Error("ssh scan access", "instance", x.instanceID, "err", err)
			continue
		}
		terminal := errors.Is(err, apperr.ErrNotFound)
		if err == nil {
			switch info.Instance.State {
			case rental.StateStopped, rental.StateWiping, rental.StateReleased, rental.StateFailed:
				terminal = true
			}
		}
		if terminal {
			s.d.Log.Info("ssh session disconnected: instance no longer running", "instance", x.instanceID, "user", x.userID)
			x.disconnect(MsgDisconnected)
		}
	}
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
	for _, x := range s.snapshotSessions() {
		x.disconnect(msgDraining)
	}
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
