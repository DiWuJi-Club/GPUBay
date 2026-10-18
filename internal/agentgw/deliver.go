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

// TickNow 执行一次投递 tick（设计 §3.1），多次调用之间串行：
//  1. 核对本副本每条流的归属（agent_conn_id）与指纹，不符即断流（跨副本顶替、重新接入、撤销接入）；
//  2. 对已处理完首个 StateReport 的流原子认领 pending 指令（改为 sent）；
//  3. 对这些流按机器创建顺序推送所有 sent 且不在已推送集合里的指令，并刷新补推指令的 sent_at。
//     认领的和此前已认领但未推到本流的指令统一按 created_at 推送，较新的指令不会越过较旧的。
func (s *Server) TickNow(ctx context.Context) {
	s.tickMu.Lock()
	defer s.tickMu.Unlock()
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
	var readyConnIDs []string
	var readyMachines []int64
	for _, c := range conns {
		o := owners[c.machineID]
		if o.ConnID != c.id || o.Fingerprint != c.fingerprint {
			c.abort(status.Error(codes.Aborted, "connection superseded or enrollment revoked"))
			continue
		}
		if c.ready.Load() {
			ready = append(ready, c)
			readyConnIDs = append(readyConnIDs, c.id)
			readyMachines = append(readyMachines, c.machineID)
		}
	}
	if len(ready) == 0 {
		return
	}
	// 认领的指令不在这里直接推送：统一由下面按创建顺序推送（SQL 只认领归属仍为这些流的机器）
	if _, err := s.d.Fleet.ClaimPending(ctx, readyConnIDs); err != nil {
		s.d.Log.Error("tick: claim", "err", err)
		return
	}
	sent, err := s.d.Fleet.SentCommands(ctx, readyMachines)
	if err != nil {
		s.d.Log.Error("tick: sent commands", "err", err)
		return
	}
	byMachine := map[int64][]fleet.Command{}
	for _, cmd := range sent {
		byMachine[cmd.MachineID] = append(byMachine[cmd.MachineID], cmd)
	}
	var touched []string
	for _, c := range ready {
		if cur := s.conns.byID(c.id); cur != c {
			continue // 认领期间被本副本的新流顶掉：指令保持 sent，下个 tick 推给新流
		}
		cmds := byMachine[c.machineID]
		sentIDs := make([]string, 0, len(cmds))
		for _, cmd := range cmds {
			sentIDs = append(sentIDs, cmd.ID)
			if c.aborted() || c.wasPushed(cmd.ID) {
				continue
			}
			if s.push(ctx, c, cmd, n) {
				touched = append(touched, cmd.ID)
			}
		}
		// 被取消 / 超时的指令从已推送集合里清掉
		for _, id := range c.retainPushed(sentIDs) {
			s.deliveries.Delete(id)
		}
	}
	if err := s.d.Fleet.TouchSent(ctx, touched); err != nil {
		s.d.Log.Error("tick: touch sent", "err", err)
	}
}

// push 把指令推给流；Create 指令在这里读实例主机私钥（不经过 agent_commands.payload）。发送失败则断流（指令保持 sent，重连后补推）。
func (s *Server) push(ctx context.Context, c *conn, cmd fleet.Command, tick int64) bool {
	hostKey := ""
	if cmd.Type == fleet.CmdCreateInstance && s.d.HostKeys != nil {
		pem, err := s.d.HostKeys.HostKeyPEM(ctx, cmd.InstanceID)
		if err != nil {
			s.d.Log.Error("tick: load instance host key", "command", cmd.ID, "instance", cmd.InstanceID, "err", err)
			return false
		}
		hostKey = pem
	}
	msg, err := toProtoCommand(cmd, s.cfg, hostKey)
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
