// Package executor 执行平台指令：按指令 ID 去重回放、实例指令单队列顺序执行、PullImage 低优先级队列、步骤级幂等。
package executor

import (
	"context"
	"log/slog"
	"slices"
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

	// ReportTimeout 是一次 StateReport 采集（docker ps、挂载检查）的上限：dockerd 卡死时按本地记录报 unknown，
	// 不能拖住与心跳共用的发送循环。
	ReportTimeout = 30 * time.Second
	// RecoverStepTimeout 是 Recover / 恢复重试中单个步骤（建网、规则、某实例的挂载、某实例的启动）的上限；
	// 超时按失败处理，实例进入 recoverPending 等下一轮重试。
	RecoverStepTimeout = 2 * time.Minute
	// RecoverGrace：重启后待重试启动的实例在这段时间内报 unknown（平台不做对账动作，避免误报 container_down）；
	// 超过后如实上报，让持续启动失败的实例照常告警。
	RecoverGrace = 10 * time.Minute
	// stopAfterFailureTimeout：启动后检查失败时停容器的上限（脱离调用方 ctx，调用方已超时也要停）。
	stopAfterFailureTimeout = 30 * time.Second

	// 指令执行时限，与平台侧指令超时（rental.*CommandTimeout）一致；同时作为 cmdrun 10 分钟兜底的显式 deadline，
	// 使大镜像 pull、可能隐式拉取镜像的 docker create 不被兜底误杀。
	createTimeout = 15 * time.Minute
	startTimeout  = 5 * time.Minute
	stopTimeout   = 5 * time.Minute
	wipeTimeout   = 30 * time.Minute
	pullTimeout   = 60 * time.Minute
)

func commandTimeout(typ string) time.Duration {
	switch typ {
	case "create_instance":
		return createTimeout
	case "start_instance":
		return startTimeout
	case "stop_instance":
		return stopTimeout
	case "wipe_instance":
		return wipeTimeout
	}
	return pullTimeout
}

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
	// 以下三项默认取同名常量，测试可调短。
	reportTimeout      time.Duration
	recoverStepTimeout time.Duration
	recoverGrace       time.Duration // 受 mu 保护

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
	// recoverPending：Recover 中期望 running 却没能启动的实例（地址、Docker、数据盘等瞬时条件），
	// 由 refreshRules 逐轮重试直到启动成功；只含 Recover 产生的实例，外部停止的容器不在其中。
	// 值为首次登记的时间（RecoverGrace 宽限期从这里起算）。
	recoverPending map[string]time.Time

	// 两个队列各有一个唤醒通道（容量 1），入队时按类型通知；共用一个通道时 pull 消费者会抢走实例指令的唤醒。
	wakeInstance chan struct{}
	wakePull     chan struct{}
	// rulesWake：白名单变化时唤醒 Run 立即刷新规则（容量 1，非阻塞写入）。
	rulesWake chan struct{}
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
		reportTimeout: ReportTimeout, recoverStepTimeout: RecoverStepTimeout, recoverGrace: RecoverGrace,
		pending: map[string]bool{}, recoverPending: map[string]time.Time{}, unpersisted: map[string]state.Result{},
		wakeInstance: make(chan struct{}, 1), wakePull: make(chan struct{}, 1), rulesWake: make(chan struct{}, 1)}
}

// SetConfig 更新配置；白名单（按顺序逐项）与旧值不同时唤醒 Run 立即刷新规则。
// 它在 gRPC 接收协程里被调用，所以只做非阻塞通知，不碰 iptables。
func (e *Executor) SetConfig(c Config) {
	e.mu.Lock()
	changed := !slices.Equal(e.cfg.AllowCIDRs, c.AllowCIDRs)
	e.cfg = c
	e.mu.Unlock()
	if changed {
		select {
		case e.rulesWake <- struct{}{}:
		default:
		}
	}
}
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
		ctx, cancel := context.WithTimeout(ctx, pullTimeout)
		defer cancel()
		return e.d.Runtime.Pull(ctx, c.ImageRef)
	}
	e.opMu.Lock()
	defer e.opMu.Unlock()
	// 时限从拿到 opMu 起算（排队等待不计入）。
	ctx, cancel := context.WithTimeout(ctx, commandTimeout(c.Type))
	defer cancel()
	// 指令接管该实例的状态：不再由恢复重试处理（在 opMu 内移除，此时没有重试在跑）。
	e.mu.Lock()
	delete(e.recoverPending, c.InstanceID)
	e.mu.Unlock()
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
			e.refreshRules(ctx)
		case <-e.rulesWake:
			e.refreshRules(ctx)
		}
	}
}

// refreshRules 是定时器与白名单变化共用的一轮：刷新规则 → 必要时重试 Recover → 巡检限速。
func (e *Executor) refreshRules(ctx context.Context) {
	if err := e.EnsureRules(ctx); err != nil {
		e.d.Log.Error("ensure rules", "err", err)
		return
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
	e.retryRecoverPending(ctx)
	e.ensureLimits(ctx)
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

// retryRecoverPending 重试 Recover 时启动失败的实例（重启后网卡还没拿到 IP 等瞬时条件）。
// 有实例指令正在执行时（opMu 被占）跳过本轮，与 ensureLimits 一致；期望状态不再是 running、
// 记录已被删除或容器已在运行的实例直接移出；失败的保留到下一轮。
func (e *Executor) retryRecoverPending(ctx context.Context) {
	e.mu.Lock()
	n := len(e.recoverPending)
	e.mu.Unlock()
	if n == 0 || !e.opMu.TryLock() {
		return
	}
	defer e.opMu.Unlock()
	e.mu.Lock()
	ids := make([]string, 0, len(e.recoverPending))
	for id := range e.recoverPending {
		ids = append(ids, id)
	}
	e.mu.Unlock()
	for _, id := range ids {
		if ctx.Err() != nil { // Agent 正在退出：不再启动新的重试
			return
		}
		done := e.retryRecoverOne(ctx, id)
		if done {
			e.mu.Lock()
			delete(e.recoverPending, id)
			e.mu.Unlock()
		}
	}
}

// retryRecoverOne 返回 true 表示该实例不必再重试。查询、挂载、启动各自受 recoverStepTimeout 约束（持有 opMu，不能被卡死的 dockerd 拖住）。
func (e *Executor) retryRecoverOne(ctx context.Context, id string) bool {
	inst, ok := e.d.State.Instance(id)
	if !ok || inst.Desired != "running" {
		return true
	}
	running := false
	e.withStep(ctx, func(ctx context.Context) error {
		c, err := e.d.Runtime.Inspect(ctx, id)
		running = err == nil && c.Exists && c.Running
		return err
	})
	if running {
		return true // 限速交给 ensureLimits
	}
	err := e.withStep(ctx, func(ctx context.Context) error { return e.d.Disk.Mount(ctx, id) })
	if err == nil {
		err = e.withStep(ctx, func(ctx context.Context) error { return e.startChecked(ctx, inst) })
	}
	if err != nil {
		e.d.Log.Error("recover retry failed", "instance", id, "err", err)
		return false
	}
	e.d.Log.Info("recovered instance started on retry", "instance", id)
	return true
}

// withStep 在 recoverStepTimeout 时限内执行一个恢复步骤。
func (e *Executor) withStep(ctx context.Context, f func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, e.recoverStepTimeout)
	defer cancel()
	return f(ctx)
}

// ensureLimits 给运行中却丢了限速的容器补上限速。有实例指令正在执行时（opMu 被占）跳过本轮，
// 免得与 create/start 内的 Limit 并发；单个实例出错只记日志，继续下一个。不改期望状态，不启停容器。
func (e *Executor) ensureLimits(ctx context.Context) {
	if !e.opMu.TryLock() {
		return
	}
	defer e.opMu.Unlock()
	for _, inst := range e.d.State.Instances() {
		// 持有 opMu：每个实例同样受单步时限约束，卡死的 dockerd 不能把实例指令永远挡在锁外。
		if err := e.withStep(ctx, func(ctx context.Context) error { return e.ensureLimit(ctx, inst) }); err != nil {
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
