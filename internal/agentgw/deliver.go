package agentgw

import (
	"context"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"spark-cloud/internal/agentpb"
	"spark-cloud/internal/fleet"
)

func (s *Server) runTicks(ctx context.Context) {
	t := time.NewTicker(s.cfg.Tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.TickNow(ctx)
		}
	}
}

// TickNow 执行一次投递 tick（设计 §3.1）：
//  1. 核对本副本每条流的归属（agent_conn_id）与指纹，不符即断流（跨副本顶替、重新接入、撤销接入）；
//  2. 对已处理完首个 StateReport 的流原子认领 pending 指令并推送；
//  3. 对这些流补推 sent 且不在已推送集合里的指令，并刷新 sent_at。
func (s *Server) TickNow(ctx context.Context) {
	n := s.ticks.Add(1)
	conns := s.conns.snapshot()
	if len(conns) == 0 {
		return
	}
	ids := make([]int64, 0, len(conns))
	for _, c := range conns {
		ids = append(ids, c.machineID)
	}
	owners, err := s.d.Fleet.ConnOwners(ctx, ids)
	if err != nil {
		s.d.Log.Error("tick: conn owners", "err", err)
		return
	}
	var ready []*conn
	var readyIDs []string
	for _, c := range conns {
		o := owners[c.machineID]
		if o.ConnID != c.id || o.Fingerprint != c.fingerprint {
			c.abort(status.Error(codes.Aborted, "connection superseded or enrollment revoked"))
			continue
		}
		if c.ready.Load() {
			ready = append(ready, c)
			readyIDs = append(readyIDs, c.id)
		}
	}
	claimed, err := s.d.Fleet.ClaimPending(ctx, readyIDs)
	if err != nil {
		s.d.Log.Error("tick: claim", "err", err)
		return
	}
	for _, cmd := range claimed {
		c := s.conns.byID(cmd.ConnID)
		if c == nil || c.machineID != cmd.MachineID {
			continue // 认领期间被顶掉：保持 sent，由接管的副本补推
		}
		s.push(ctx, c, cmd.Command, n)
	}
	for _, c := range ready {
		sent, err := s.d.Fleet.SentCommands(ctx, c.machineID)
		if err != nil {
			s.d.Log.Error("tick: sent commands", "machine", c.machineID, "err", err)
			continue
		}
		var touched, sentIDs []string
		for _, cmd := range sent {
			sentIDs = append(sentIDs, cmd.ID)
			if c.wasPushed(cmd.ID) {
				continue
			}
			if s.push(ctx, c, cmd, n) {
				touched = append(touched, cmd.ID)
			}
		}
		if err := s.d.Fleet.TouchSent(ctx, touched); err != nil {
			s.d.Log.Error("tick: touch sent", "machine", c.machineID, "err", err)
		}
		c.retainPushed(sentIDs) // 被取消 / 超时的指令从已推送集合里清掉
	}
}

// push 把指令推给流；发送失败则断流（指令保持 sent，重连后补推）。
func (s *Server) push(ctx context.Context, c *conn, cmd fleet.Command, tick int64) bool {
	msg, err := toProtoCommand(cmd, s.cfg)
	if err != nil {
		s.d.Log.Error("tick: convert command", "command", cmd.ID, "err", err)
		return false
	}
	if err := c.send(&agentpb.PlatformMessage{Msg: &agentpb.PlatformMessage_Command{Command: msg}}); err != nil {
		c.abort(err)
		return false
	}
	c.markPushed(cmd.ID)
	s.deliveries.Store(cmd.ID, tick)
	return true
}

// DeliveryTick 返回指令最近一次被本副本推送时的 tick 号。
func (s *Server) DeliveryTick(cmdID string) (int64, bool) {
	v, ok := s.deliveries.Load(cmdID)
	if !ok {
		return 0, false
	}
	return v.(int64), true
}
