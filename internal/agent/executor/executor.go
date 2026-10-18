// Package executor 执行平台指令：按指令 ID 去重回放、实例指令单队列顺序执行、PullImage 低优先级队列、步骤级幂等。
package executor

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"spark-cloud/internal/agent/disk"
	"spark-cloud/internal/agent/netfilter"
	"spark-cloud/internal/agent/runtime"
	"spark-cloud/internal/agent/state"
)

const (
	ReserveBytes     = uint64(100) << 30 // 创建数据盘时根分区必须额外留出的余量
	HostReserveBytes = uint64(8) << 30   // 容器内存上限 = 机器内存 − 8GiB
	SSHMountDir      = "/.spark"
	rulesInterval    = 60 * time.Second
)

type Config struct {
	LanIP         string
	DNS           []string
	AllowCIDRs    []string
	BandwidthMbps int
	DataSizeGiB   int
}

type Command struct {
	ID             string
	InstanceID     string
	Type           string // create_instance / start_instance / stop_instance / wipe_instance / pull_image
	ImageRef       string
	UpstreamPubKey string
	BandwidthMbps  int
	SSHPort        int
	DataSizeGiB    int
}

type ResultMsg struct {
	ID      string
	OK      bool
	Message string
}

type InstanceState struct {
	InstanceID string
	Phase      string // running / stopped / missing_data / unknown
}

type Deps struct {
	Runtime    runtime.Runtime
	Disk       disk.Disk
	Net        netfilter.Netfilter
	State      *state.Store
	StateDir   string
	Bridge     string
	LocalIPs   func() []string
	MemTotal   func() uint64
	HostKeyGen func(dir string) error
	Log        *slog.Logger
}

type Executor struct {
	d             Deps
	stepped       bool
	results       chan ResultMsg
	rulesInterval time.Duration

	// opMu 串行化所有改动实例的操作（实例指令、Recover），拉取镜像不受它约束。
	opMu sync.Mutex

	mu      sync.Mutex
	cfg     Config
	queue   []Command   // 实例指令，顺序执行
	pulls   []Command   // 低优先级
	replays []ResultMsg // 步进模式下暂存的回放结果，随下一次 RunPending 返回
	// pending：已入队或正在执行、结果尚未落盘的指令 ID。结果落盘之后才移除，
	// 所以任何时刻同一 ID 要么在 pending 里、要么在状态文件里，重复推送不会被执行第二次。
	pending map[string]bool
	// unpersisted：结果写状态文件失败时的内存兜底，本进程内仍按已完成回放。
	unpersisted map[string]state.Result
	// recoverBlocked：Recover 因隔离规则失败而没有启动实例，规则刷新成功后重试。
	recoverBlocked bool

	// 两个队列各有一个唤醒通道（容量 1），入队时按类型通知；共用一个通道时 pull 消费者会抢走实例指令的唤醒。
	wakeInstance chan struct{}
	wakePull     chan struct{}
}

func New(d Deps, stepped bool) *Executor {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Bridge == "" {
		d.Bridge = "spark0"
	}
	if d.MemTotal == nil {
		d.MemTotal = procMemTotal
	}
	if d.HostKeyGen == nil {
		d.HostKeyGen = generateHostKey
	}
	if d.LocalIPs == nil {
		d.LocalIPs = func() []string { return nil }
	}
	return &Executor{d: d, stepped: stepped, results: make(chan ResultMsg, 256), rulesInterval: rulesInterval,
		pending: map[string]bool{}, unpersisted: map[string]state.Result{},
		wakeInstance: make(chan struct{}, 1), wakePull: make(chan struct{}, 1)}
}

func (e *Executor) SetConfig(c Config)        { e.mu.Lock(); e.cfg = c; e.mu.Unlock() }
func (e *Executor) Config() Config            { e.mu.Lock(); defer e.mu.Unlock(); return e.cfg }
func (e *Executor) Results() <-chan ResultMsg { return e.results }
func (e *Executor) StateStore() *state.Store  { return e.d.State }

// Submit：已执行过的指令直接回放结果（自动模式送 Results()，步进模式暂存给 RunPending）；
// 已入队或正在执行的忽略（结果出来后照常送出）；否则按类型入队。
func (e *Executor) Submit(c Command) {
	e.mu.Lock()
	r, done := e.d.State.Result(c.ID)
	if !done {
		r, done = e.unpersisted[c.ID]
	}
	if done {
		msg := ResultMsg{ID: c.ID, OK: r.OK, Message: r.Message}
		if e.stepped {
			e.replays = append(e.replays, msg)
			e.mu.Unlock()
		} else {
			e.mu.Unlock()
			e.emit(msg)
		}
		return
	}
	defer e.mu.Unlock()
	if e.pending[c.ID] {
		e.d.Log.Info("duplicate command ignored (already queued or running)", "command", c.ID)
		return
	}
	e.pending[c.ID] = true
	wake := e.wakeInstance
	if c.Type == "pull_image" {
		e.pulls = append(e.pulls, c)
		wake = e.wakePull
	} else {
		e.queue = append(e.queue, c)
	}
	select {
	case wake <- struct{}{}:
	default:
	}
}

func (e *Executor) emit(msg ResultMsg) {
	select {
	case e.results <- msg:
	default:
		e.d.Log.Warn("result channel full, dropping (persisted unsent, will be resent)", "command", msg.ID)
	}
}

func (e *Executor) pop(pull bool) (Command, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	q := &e.queue
	if pull {
		q = &e.pulls
	}
	if len(*q) == 0 {
		return Command{}, false
	}
	c := (*q)[0]
	*q = (*q)[1:]
	return c, true
}

// execute 执行一条指令，先把结果写进 state.json（fsync，Sent=false）再返回；发送由调用方负责。
func (e *Executor) execute(ctx context.Context, c Command, override *state.Result) ResultMsg {
	var r state.Result
	if override != nil {
		r = state.Result{OK: override.OK, Message: override.Message}
	} else {
		err := e.dispatch(ctx, c)
		r = state.Result{OK: err == nil}
		if err != nil {
			r.Message = err.Error()
			e.d.Log.Error("command failed", "command", c.ID, "type", c.Type, "instance", c.InstanceID, "err", err)
		}
	}
	perr := e.d.State.PutResult(c.ID, r)
	if perr != nil {
		e.d.Log.Error("persist result", "command", c.ID, "err", perr)
	}
	e.mu.Lock()
	if perr != nil {
		e.unpersisted[c.ID] = r
	}
	delete(e.pending, c.ID)
	e.mu.Unlock()
	return ResultMsg{ID: c.ID, OK: r.OK, Message: r.Message}
}

func (e *Executor) dispatch(ctx context.Context, c Command) error {
	if c.Type == "pull_image" {
		return e.d.Runtime.Pull(ctx, c.ImageRef)
	}
	e.opMu.Lock()
	defer e.opMu.Unlock()
	switch c.Type {
	case "create_instance":
		return e.create(ctx, c)
	case "start_instance":
		return e.start(ctx, c.InstanceID)
	case "stop_instance":
		return e.stop(ctx, c.InstanceID)
	case "wipe_instance":
		return e.wipe(ctx, c.InstanceID)
	}
	return &unknownType{c.Type}
}

type unknownType struct{ t string }

func (u *unknownType) Error() string { return "未知指令类型 " + u.t }

// RunPending 步进模式：先返回暂存的回放，再执行当前队列里的全部指令（先实例指令，再拉取），按顺序返回结果。
func (e *Executor) RunPending(ctx context.Context, override *state.Result) []ResultMsg {
	e.mu.Lock()
	out := e.replays
	e.replays = nil
	e.mu.Unlock()
	for _, pull := range []bool{false, true} {
		for {
			c, ok := e.pop(pull)
			if !ok {
				break
			}
			out = append(out, e.execute(ctx, c, override))
		}
	}
	return out
}

// Run 自动模式：两个消费者 + 定时刷新 iptables 规则；步进模式只刷新规则。
// 每轮规则刷新成功后还会巡检限速（容器被外部 docker stop/start 后换了新 veth，限速会丢）。
// 规则刷新成功且上一次 Recover 因规则失败没有启动实例时，重做 Recover。
func (e *Executor) Run(ctx context.Context) {
	if !e.stepped {
		go e.consume(ctx, false)
		go e.consume(ctx, true)
	}
	t := time.NewTicker(e.rulesInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := e.EnsureRules(ctx); err != nil {
				e.d.Log.Error("ensure rules", "err", err)
				continue
			}
			e.mu.Lock()
			retry := e.recoverBlocked
			e.mu.Unlock()
			if retry {
				e.d.Log.Info("isolation rules restored, retrying recovery")
				if err := e.Recover(ctx); err != nil {
					e.d.Log.Error("recover retry", "err", err)
				}
			}
			e.ensureLimits(ctx)
		}
	}
}

func (e *Executor) consume(ctx context.Context, pull bool) {
	wake := e.wakeInstance
	if pull {
		wake = e.wakePull
	}
	for {
		c, ok := e.pop(pull)
		if !ok {
			// 入队先于通知且通道有 1 个缓冲：pop 落空之后的入队一定留下一次唤醒，不会丢。
			select {
			case <-ctx.Done():
				return
			case <-wake:
			}
			continue
		}
		e.emit(e.execute(ctx, c, nil))
	}
}

func (e *Executor) EnsureRules(ctx context.Context) error {
	cfg := e.Config()
	return e.d.Net.EnsureRules(ctx, netfilter.Rules{Bridge: e.d.Bridge, AllowCIDRs: cfg.AllowCIDRs})
}

// ensureLimits 给运行中却丢了限速的容器补上限速。有实例指令正在执行时（opMu 被占）跳过本轮，
// 免得与 create/start 内的 Limit 并发；单个实例出错只记日志，继续下一个。不改期望状态，不启停容器。
func (e *Executor) ensureLimits(ctx context.Context) {
	if !e.opMu.TryLock() {
		return
	}
	defer e.opMu.Unlock()
	for _, inst := range e.d.State.Instances() {
		if err := e.ensureLimit(ctx, inst); err != nil {
			e.d.Log.Error("ensure bandwidth limit", "instance", inst.ID, "err", err)
		}
	}
}

func (e *Executor) ensureLimit(ctx context.Context, inst state.Instance) error {
	c, err := e.d.Runtime.Inspect(ctx, inst.ID)
	if err != nil {
		return err
	}
	if !c.Exists || !c.Running {
		return nil
	}
	veth, err := e.d.Runtime.VethOf(ctx, inst.ID)
	if err != nil {
		return err
	}
	ok, err := e.d.Net.LimitApplied(ctx, veth)
	if err != nil || ok {
		return err
	}
	mbps := inst.BandwidthMbps
	if mbps <= 0 {
		mbps = e.Config().BandwidthMbps
	}
	if err := e.d.Net.Limit(ctx, veth, mbps); err != nil {
		return err
	}
	e.d.Log.Warn("bandwidth limit reapplied", "instance", inst.ID, "veth", veth)
	return nil
}
