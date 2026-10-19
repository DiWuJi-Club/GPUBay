package sshgw

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	keepaliveInterval = 60 * time.Second
	keepaliveMisses   = 3
	keepaliveTimeout  = 15 * time.Second
	maxPendingOpens   = 64 // per session, per direction：每个会话、每个方向上未确认（还在等对端回应）的通道打开数上限
)

// 策略：白名单 + 默认拒绝。网关不解析任何通道 / 请求的载荷，原样转给对端。
func clientChannelAllowed(t string) bool { return t == "session" || t == "direct-tcpip" }
func upstreamChannelAllowed(t string) bool {
	return t == "forwarded-tcpip" || t == "auth-agent@openssh.com"
}

// 全局请求按方向分别放行：tcpip-forward 等只能由用户发往容器；容器发来的只放行保活。
var (
	clientGlobalAllowed   = map[string]bool{"tcpip-forward": true, "cancel-tcpip-forward": true, "keepalive@openssh.com": true, "no-more-sessions@openssh.com": true}
	upstreamGlobalAllowed = map[string]bool{"keepalive@openssh.com": true}
)

// relay 双向转发直到任一侧连接结束。
func (s *Server) relay(sess *session, down *ssh.ServerConn, downChans <-chan ssh.NewChannel, downReqs <-chan *ssh.Request,
	up ssh.Conn, upChans <-chan ssh.NewChannel, upReqs <-chan *ssh.Request) {
	var wg sync.WaitGroup
	wg.Add(4)
	go func() { defer wg.Done(); forwardGlobal(downReqs, up, clientGlobalAllowed, nil) }()
	go func() {
		defer wg.Done()
		forwardGlobal(upReqs, down, upstreamGlobalAllowed, func(name string) {
			if name == "hostkeys-00@openssh.com" {
				s.hostkeysDropped.Add(1)
			}
		})
	}()
	// 打开对端通道要等确认：每个都异步，慢的不挡后面的；每个会话每个方向（per session, per direction）未确认的打开数有上限，超出回 ResourceShortage
	var downPending, upPending atomic.Int32
	go func() { defer wg.Done(); s.acceptOpens(sess, downChans, up, true, &downPending) }()
	go func() { defer wg.Done(); s.acceptOpens(sess, upChans, down, false, &upPending) }()
	stop := make(chan struct{})
	go s.keepalive(down, stop)
	go s.keepalive(up, stop)
	ended := make(chan bool, 2) // true：上游先结束
	go func() { down.Wait(); ended <- false }()
	go func() { up.Wait(); ended <- true }()
	if <-ended && !sess.closing.Load() {
		s.checkUpstreamDrop(sess)
	}
	close(sess.verdict)
	close(stop)
	down.Close()
	up.Close()
	wg.Wait()
}

// dropCheckTimeout：上游意外断开后复查实例状态的时限（期间客户端的 session 通道保持未关闭）。
const dropCheckTimeout = 2 * time.Second

// checkUpstreamDrop：用户没断、上游却断了。到期停机时 agent 立即停容器，上游会在 10 秒扫描之前断开，
// 所以在这里复查状态：已不在 running（停机、清盘、释放、失败、不存在）→ 先写 MsgDisconnected 再断开（§10.4.9）。
// 仍是 running 或查询出错：按上游故障处理，照旧直接断开。
func (s *Server) checkUpstreamDrop(sess *session) {
	ctx, cancel := context.WithTimeout(context.Background(), dropCheckTimeout)
	defer cancel()
	states, err := s.d.Store.SessionStates(ctx, []string{sess.instanceID})
	if err != nil {
		s.d.Log.Error("ssh upstream drop state check", "instance", sess.instanceID, "err", err)
		return
	}
	st, ok := states[sess.instanceID]
	if ok && !terminalState(st.State) {
		s.d.Log.Info("ssh upstream dropped", "instance", sess.instanceID, "user", sess.userID)
		return
	}
	s.d.Log.Info("ssh session disconnected: instance no longer running", "instance", sess.instanceID, "user", sess.userID, "found", ok)
	sess.disconnect(MsgDisconnected)
}

// forwardGlobal 转发全局请求；不在白名单的回 false（onDrop 用于计数）。
func forwardGlobal(in <-chan *ssh.Request, out ssh.Conn, allowed map[string]bool, onDrop func(name string)) {
	for req := range in {
		if !allowed[req.Type] {
			if onDrop != nil {
				onDrop(req.Type)
			}
			if req.WantReply {
				req.Reply(false, nil)
			}
			continue
		}
		ok, payload, err := out.SendRequest(req.Type, req.WantReply, req.Payload)
		if req.WantReply {
			req.Reply(ok && err == nil, payload)
		}
	}
}

// acceptOpens 逐个领取一侧的 NewChannel 并异步打开到另一侧；pending 计未确认的打开数（拿到对端回应即减）。
func (s *Server) acceptOpens(sess *session, chans <-chan ssh.NewChannel, other ssh.Conn, fromClient bool, pending *atomic.Int32) {
	for nc := range chans {
		if pending.Add(1) > maxPendingOpens {
			pending.Add(-1)
			nc.Reject(ssh.ResourceShortage, "too many pending channel opens")
			continue
		}
		sess.pipes.Add(1)
		go func() {
			defer sess.pipes.Done()
			s.openOn(sess, nc, other, fromClient, func() { pending.Add(-1) })
		}()
	}
}

// openOn 把一侧的 NewChannel 原样开到另一侧；fromClient 决定白名单与字节计数方向。
// settled 恰好调用一次，在回复本侧之前：策略拒绝时，或对端已回应（确认 / 拒绝）时——本侧看到结果时额度已归还。
func (s *Server) openOn(sess *session, nc ssh.NewChannel, other ssh.Conn, fromClient bool, settled func()) {
	t := nc.ChannelType()
	if (fromClient && !clientChannelAllowed(t)) || (!fromClient && !upstreamChannelAllowed(t)) {
		settled()
		reason := ssh.Prohibited
		if fromClient {
			reason = ssh.UnknownChannelType
		}
		nc.Reject(reason, "channel type not allowed: "+t)
		return
	}
	och, oreqs, err := other.OpenChannel(t, nc.ExtraData())
	settled()
	if err != nil {
		var oce *ssh.OpenChannelError
		if errors.As(err, &oce) {
			nc.Reject(oce.Reason, oce.Message)
		} else {
			nc.Reject(ssh.ConnectionFailed, err.Error())
		}
		return
	}
	ch, reqs, err := nc.Accept()
	if err != nil {
		och.Close()
		return
	}
	if fromClient && t == "session" {
		sess.addShell(ch)
	}
	sess.pipes.Add(1)
	go func() {
		defer sess.pipes.Done()
		if fromClient {
			var hold func()
			if t == "session" {
				hold = sess.holdIfUpstreamDead
			}
			pipeChannels(ch, reqs, och, oreqs, &sess.bytesUp, &sess.bytesDown, hold)
		} else { // 上游发起（-R 的 forwarded-tcpip、agent）：och 才是客户端侧，用户 → 容器 仍计 up（裁定 D24）
			pipeChannels(och, oreqs, ch, reqs, &sess.bytesUp, &sess.bytesDown, nil)
		}
		if fromClient && t == "session" {
			sess.removeShell(ch) // ControlMaster 长连接会开关成百上千个 session，不能只增不减
		}
	}()
}

// holdIfUpstreamDead：容器侧通道结束时若上游连接已断，等 relay 复查完状态（可能要往本通道 stderr 写停机提示）
// 再让 pipeChannels 给客户端侧发 EOF / Close。容器正常结束通道时上游仍活着，不等待。
func (x *session) holdIfUpstreamDead() {
	select {
	case <-x.upDead:
		<-x.verdict
	default:
	}
}

// pipeChannels：a 是客户端侧、b 是容器侧。hold 非空时在 b 结束、关闭 a 的写端之前调用。数据与 stderr 双向拷贝，通道内请求双向转发（含 exit-status、eow）。
// 关闭顺序：一侧的请求流结束（该侧通道已被对端关闭）后，先等「从该侧读出的数据」全部写到另一侧（io.Copy 返回），
// 再 Close 另一侧——否则 Close 发出 channelCloseMsg 后 x/crypto 对后续写入直接返回 io.EOF，`cat 大文件` 的尾部会被截断。
func pipeChannels(a ssh.Channel, aReqs <-chan *ssh.Request, b ssh.Channel, bReqs <-chan *ssh.Request, up, down *atomic.Int64, hold func()) {
	aToB := make(chan struct{}) // a → b 的 stdout + stderr 拷贝都结束
	bToA := make(chan struct{})
	go func() {
		var inner sync.WaitGroup
		inner.Add(2)
		go func() { defer inner.Done(); n, _ := io.Copy(b, a); up.Add(n) }()
		go func() { defer inner.Done(); n, _ := io.Copy(b.Stderr(), a.Stderr()); up.Add(n) }()
		inner.Wait()
		b.CloseWrite()
		close(aToB)
	}()
	go func() {
		var inner sync.WaitGroup
		inner.Add(2)
		go func() { defer inner.Done(); n, _ := io.Copy(a, b); down.Add(n) }()
		go func() { defer inner.Done(); n, _ := io.Copy(a.Stderr(), b.Stderr()); down.Add(n) }()
		inner.Wait()
		if hold != nil {
			hold()
		}
		a.CloseWrite()
		close(bToA)
	}()
	// aBusy / bBusy：正在把 a / b 发来的请求转给对端并等回应。关 a 之前先等 a 的在途请求回完：
	// 秒退的 exec（`exit 3`、`stderr x`）在上游回 exec 成功后立刻 exit-status + Close，若先关 a，
	// 客户端的 exec 请求收不到回应，只得到 EOF。
	var aBusy, bBusy sync.Mutex
	closeAfter := func(c ssh.Channel, busy *sync.Mutex) { busy.Lock(); c.Close(); busy.Unlock() }
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); forwardChannelRequests(aReqs, b, &aBusy); <-aToB; closeAfter(b, &bBusy) }() // 客户端关了：a 读到 EOF 后拷贝结束，再关容器侧
	go func() { defer wg.Done(); forwardChannelRequests(bReqs, a, &bBusy); <-bToA; closeAfter(a, &aBusy) }() // 容器关了：b 的尾部数据写完再关客户端侧
	wg.Wait()
}

// forwardChannelRequests 结束时该侧通道已被对端关闭，读端随之 EOF，所以上面的 <-aToB / <-bToA 不会永久阻塞。
// busy 在转发一条请求并回复期间持有；此时对端通道已关闭，SendRequest 立即返回，所以 closeAfter 不会久等。
func forwardChannelRequests(in <-chan *ssh.Request, out ssh.Channel, busy *sync.Mutex) {
	for req := range in {
		busy.Lock()
		ok, err := out.SendRequest(req.Type, req.WantReply, req.Payload)
		if req.WantReply {
			req.Reply(ok && err == nil, nil)
		}
		busy.Unlock()
	}
}

// keepalive 每 Config.KeepaliveInterval 发一次 keepalive@openssh.com，连续 keepaliveMisses 次无回应即关闭连接。
// 对端回 false（OpenSSH 对未知请求的回复）也算活着；只有发送出错或超时才算未回应。
func (s *Server) keepalive(c ssh.Conn, stop <-chan struct{}) {
	t := time.NewTicker(s.cfg.KeepaliveInterval)
	defer t.Stop()
	misses := 0
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			res := make(chan error, 1)
			go func() { _, _, err := c.SendRequest("keepalive@openssh.com", true, nil); res <- err }()
			select {
			case err := <-res:
				if err == nil {
					misses = 0
					continue
				}
			case <-time.After(s.cfg.KeepaliveTimeout):
			}
			if misses++; misses >= keepaliveMisses {
				c.Close()
				return
			}
		}
	}
}
