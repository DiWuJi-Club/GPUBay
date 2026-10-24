package executor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"spark-cloud/internal/agent/metrics"
	"spark-cloud/internal/agent/runtime"
	"spark-cloud/internal/agent/state"
)

// instanceIDRe 与 disk/loop 的校验一致；实例 ID 会拼进 ssh 目录路径，必须先校验（防路径穿越）。
var instanceIDRe = regexp.MustCompile(`^ins-[a-z0-9]{8}$`)

func checkID(id string) error {
	if !instanceIDRe.MatchString(id) {
		return fmt.Errorf("非法实例 ID %q", id)
	}
	return nil
}

func (e *Executor) sshDir(id string) string { return filepath.Join(e.d.StateDir, "ssh", id) }

// slots 是指令的份数：0 / 1（及非法的负数）都按整机。
func slots(c Command) int { return max(1, c.ShareSlots) }

// spec 按份数 N 等分配额（规格 §6.2）：内存 (MemTotal − 8GiB) / N、shm 为内存上限的一半、CPU nproc / N、
// pids 32768 / N，共享机去掉 SYS_NICE。N = 1 时与整机完全一致（不传 --cpus，shm = min(MemTotal/2, 内存上限)）。
func (e *Executor) spec(c Command, cfg Config) (runtime.Spec, error) {
	mem := e.d.MemTotal()
	if mem <= HostReserveBytes {
		return runtime.Spec{}, fmt.Errorf("无法确定机器内存（读到 %d 字节）", mem)
	}
	n := slots(c)
	memLimit := (mem - HostReserveBytes) / uint64(n)
	shm := min(mem/2, memLimit) // /dev/shm 计入容器内存，不能超过内存上限
	var cpus float64
	if n > 1 {
		shm = memLimit / 2
		cpus = float64(e.d.NumCPU()) / float64(n)
	}
	return runtime.Spec{InstanceID: c.InstanceID, ImageRef: c.ImageRef, LanIP: cfg.LanIP, SSHPort: c.SSHPort, DNS: cfg.DNS,
		SSHDir: e.sshDir(c.InstanceID), DataDir: e.d.Disk.MountDir(c.InstanceID),
		MemoryBytes: int64(memLimit), ShmBytes: int64(shm), CPUs: cpus, PidsLimit: MaxPids / n, DropNice: n > 1}, nil
}

// portConflict：本机状态文件里别的实例已占用该 SSH 端口（平台分配槽位出错时的兜底）。
func (e *Executor) portConflict(c Command) error {
	for _, inst := range e.d.State.Instances() {
		if inst.ID != c.InstanceID && inst.SSHPort == c.SSHPort {
			return fmt.Errorf("SSH 端口冲突：%d 已被实例 %s 使用", c.SSHPort, inst.ID)
		}
	}
	return nil
}

// create 的每一步都先自检：Agent 在任意一步重启后整条重跑也不会出错。
func (e *Executor) create(ctx context.Context, c Command) error {
	if err := checkID(c.InstanceID); err != nil {
		return err
	}
	cfg := e.Config()
	if c.DataSizeGiB <= 0 {
		c.DataSizeGiB = cfg.DataSizeGiB
	}
	if c.BandwidthMbps <= 0 {
		c.BandwidthMbps = cfg.BandwidthMbps
	}
	if c.SSHPort <= 0 {
		c.SSHPort = 2222
	}
	if err := e.portConflict(c); err != nil {
		return err
	}
	// 数据盘按份等分（向下取整到 GiB）；之后的余量检查、建盘与本地记录都用等分后的大小。
	n := slots(c)
	size := c.DataSizeGiB / n
	if size < 1 {
		return fmt.Errorf("数据盘 %d GiB 不足以分成 %d 份", c.DataSizeGiB, n)
	}
	c.DataSizeGiB = size
	if strings.TrimSpace(c.UpstreamPubKey) == "" {
		return errors.New("缺少上游 SSH 公钥")
	}
	if !slices.Contains(e.d.LocalIPs(), cfg.LanIP) {
		return fmt.Errorf("内网 IP 不匹配：本机没有地址 %s", cfg.LanIP)
	}
	exists, err := e.d.Disk.Exists(ctx, c.InstanceID)
	if err != nil {
		return err
	}
	if !exists {
		free, err := e.d.Disk.FreeBytes(ctx)
		if err != nil {
			return err
		}
		need := uint64(c.DataSizeGiB)<<30 + ReserveBytes
		if free < need {
			return fmt.Errorf("磁盘空间不足：剩余 %d GiB，需要 %d GiB（数据盘 %d GiB + 预留 %d GiB）", free>>30, need>>30, c.DataSizeGiB, ReserveBytes>>30)
		}
	}
	if err := e.d.Disk.Create(ctx, c.InstanceID, c.DataSizeGiB); err != nil {
		return err
	}
	if err := e.writeSSHDir(c.InstanceID, c.UpstreamPubKey, c.HostKeyPEM); err != nil {
		return err
	}
	if err := e.d.Runtime.EnsureNetwork(ctx); err != nil {
		return err
	}
	if err := e.EnsureRules(ctx); err != nil { // 隔离规则不就绪绝不建 / 起容器
		return fmt.Errorf("网络隔离规则未就绪：%w", err)
	}
	spec, err := e.spec(c, cfg)
	if err != nil {
		return err
	}
	// docker create 遇到同名容器会报冲突，所以先 inspect：一致则复用，不一致删掉重建。
	cur, err := e.d.Runtime.Inspect(ctx, c.InstanceID)
	if err != nil {
		return err
	}
	if cur.Exists && !(cur.Label == c.InstanceID && cur.Image == c.ImageRef && e.ownDataBind(cur, c.InstanceID)) {
		if err := e.d.Runtime.Remove(ctx, c.InstanceID); err != nil {
			return err
		}
		cur = runtime.Container{}
	}
	if !cur.Exists {
		if err := e.d.Runtime.Create(ctx, spec); err != nil {
			return err
		}
	}
	inst := state.Instance{ID: c.InstanceID, Desired: "stopped", ImageRef: c.ImageRef, LanIP: cfg.LanIP, SSHPort: c.SSHPort, BandwidthMbps: c.BandwidthMbps,
		DataSizeGiB: c.DataSizeGiB, ShareSlots: c.ShareSlots}
	if err := e.d.State.PutInstance(inst); err != nil {
		return err
	}
	return e.startChecked(ctx, inst)
}

// startChecked：docker start 前校验数据盘已挂载且挂载源是该实例自己的 loop 设备（disk.Mounted 负责），
// 并且容器确实把 /root/data 绑定了进来；绝不以未挂载状态启动容器。校验出错按失败处理，不当成「未挂载」也不当成「已挂载」。
func (e *Executor) startChecked(ctx context.Context, inst state.Instance) error {
	mounted, err := e.d.Disk.Mounted(ctx, inst.ID)
	if err != nil {
		return fmt.Errorf("检查数据盘挂载失败：%w", err)
	}
	if !mounted {
		return fmt.Errorf("数据盘未挂载：%s", e.d.Disk.MountDir(inst.ID))
	}
	c, err := e.d.Runtime.Inspect(ctx, inst.ID)
	if err != nil {
		return err
	}
	e.noteContainer(inst.ID, c)
	if !c.Exists {
		return fmt.Errorf("容器 %s 不存在", inst.ID)
	}
	if !e.ownDataBind(c, inst.ID) {
		return fmt.Errorf("数据盘未挂载：容器 %s 的 /root/data 来源是 %q，期望 %s", inst.ID, c.DataSource, e.d.Disk.MountDir(inst.ID))
	}
	// 预防：发布地址（LAN IP）还没配置时 docker start 会失败，且 Docker 会顺带丢掉容器的网络挂接；等地址出现再启动。
	// 0.0.0.0 / :: 表示所有地址，总是存在。
	local := e.d.LocalIPs()
	for _, ip := range c.PublishIPs {
		if p := net.ParseIP(ip); p != nil && p.IsUnspecified() {
			continue
		}
		if !slices.Contains(local, ip) {
			return fmt.Errorf("宿主机尚未配置地址 %s，暂不启动", ip)
		}
	}
	// 修复：网络挂接已丢失的容器，docker start 会成功但容器里没有网卡。
	if !slices.Contains(c.Networks, e.d.Bridge) {
		if err := e.d.Runtime.ConnectNetwork(ctx, inst.ID, e.d.Bridge); err != nil {
			return fmt.Errorf("恢复容器网络挂接失败: %w", err)
		}
		e.d.Log.Warn("container network attachment restored", "instance", inst.ID, "network", e.d.Bridge)
	}
	if err := e.d.Runtime.Start(ctx, inst.ID); err != nil {
		return err
	}
	inst.Desired = "running"
	if err := e.d.State.PutInstance(inst); err != nil {
		return err
	}
	// 兜底：没有网卡 / 限速的容器不能留在运行状态（限速属于租户隔离，fail closed）。
	veth, err := e.d.Runtime.VethOf(ctx, inst.ID)
	if err != nil {
		err = fmt.Errorf("查找容器网卡失败: %w", err)
		e.stopAfterFailure(ctx, inst.ID)
		return err
	}
	if err := e.d.Net.Limit(ctx, veth, inst.BandwidthMbps); err != nil {
		e.stopAfterFailure(ctx, inst.ID)
		return err
	}
	return nil
}

// stopAfterFailure 停掉启动后检查失败的容器；Stop 本身出错只记日志，调用方仍返回原错误。
// 检查失败往往正是调用方 ctx 到期造成的，所以脱离调用方的取消，另给 30 秒上限。
func (e *Executor) stopAfterFailure(ctx context.Context, id string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopAfterFailureTimeout)
	defer cancel()
	if err := e.d.Runtime.Stop(ctx, id); err != nil {
		e.d.Log.Error("stop container after failed start checks", "instance", id, "err", err)
	}
}

// ownDataBind：容器的 /root/data 绑定的是该实例自己的数据盘挂载点。
func (e *Executor) ownDataBind(c runtime.Container, id string) bool {
	return c.DataMounted && filepath.Clean(c.DataSource) == filepath.Clean(e.d.Disk.MountDir(id))
}

func (e *Executor) start(ctx context.Context, id string) error {
	if err := checkID(id); err != nil {
		return err
	}
	inst, ok := e.d.State.Instance(id)
	if !ok {
		inst = state.Instance{ID: id, BandwidthMbps: e.Config().BandwidthMbps}
	}
	if err := e.EnsureRules(ctx); err != nil {
		return fmt.Errorf("网络隔离规则未就绪：%w", err)
	}
	if err := e.writeInitScript(id); err != nil {
		return fmt.Errorf("更新 init.sh 失败：%w", err)
	}
	if err := e.d.Disk.Mount(ctx, id); err != nil {
		return fmt.Errorf("数据盘未挂载：%w", err)
	}
	return e.startChecked(ctx, inst)
}

func (e *Executor) stop(ctx context.Context, id string) error {
	if err := e.d.Runtime.Stop(ctx, id); err != nil {
		return err
	}
	if inst, ok := e.d.State.Instance(id); ok {
		inst.Desired = "stopped"
		return e.d.State.PutInstance(inst)
	}
	return nil
}

// wipe：删容器（含匿名卷）→ 销毁数据盘 → 删 ssh 目录 → 校验全部消失 → 删本地记录。
func (e *Executor) wipe(ctx context.Context, id string) error {
	if err := checkID(id); err != nil {
		return err
	}
	if err := e.d.Runtime.Remove(ctx, id); err != nil {
		return err
	}
	e.noteContainer(id, runtime.Container{})
	if err := e.d.Disk.Destroy(ctx, id); err != nil {
		return err
	}
	if err := os.RemoveAll(e.sshDir(id)); err != nil {
		return err
	}
	var problems []string
	if c, err := e.d.Runtime.Inspect(ctx, id); err != nil {
		problems = append(problems, "无法确认容器状态: "+err.Error())
	} else if c.Exists {
		problems = append(problems, "容器仍存在")
	}
	if ok, err := e.d.Disk.Exists(ctx, id); err != nil {
		problems = append(problems, "无法确认数据盘镜像: "+err.Error())
	} else if ok {
		problems = append(problems, "数据盘镜像仍存在")
	}
	if m, err := e.d.Disk.Mounted(ctx, id); err != nil {
		problems = append(problems, "无法确认挂载状态: "+err.Error())
	} else if m {
		problems = append(problems, "挂载点仍挂载")
	}
	if _, err := os.Stat(e.sshDir(id)); !errors.Is(err, os.ErrNotExist) {
		problems = append(problems, "ssh 目录仍存在")
	}
	if len(problems) > 0 {
		return fmt.Errorf("洗机校验失败：%s", strings.Join(problems, "；"))
	}
	return e.d.State.DeleteInstance(id)
}

// Snapshot 采集一份 StateReport 的内容：实例状态与已缓存镜像共用一个 reportTimeout，
// dockerd 卡死时整份报告最多等 reportTimeout（实例状态先采；超时后不再列镜像，该次报告不带 images）。
func (e *Executor) Snapshot(ctx context.Context) ([]InstanceState, []string) {
	ctx, cancel := context.WithTimeout(ctx, e.reportTimeout)
	defer cancel()
	return e.report(ctx), e.images(ctx)
}

// Images 本机已缓存镜像（StateReport.images），受 reportTimeout 约束；出错或超时返回 nil（该次报告不带 images，平台保留旧记录）。
func (e *Executor) Images(ctx context.Context) []string {
	ctx, cancel := context.WithTimeout(ctx, e.reportTimeout)
	defer cancel()
	return e.images(ctx)
}

func (e *Executor) images(ctx context.Context) []string {
	if ctx.Err() != nil { // Report 已用完时限：不再调用 docker
		return nil
	}
	imgs, err := e.d.Runtime.Images(ctx)
	if err != nil {
		e.d.Log.Warn("list images", "err", err)
		return nil
	}
	return imgs
}

// Report 上报本机实例状态：以 docker 列表为准，本地记录里有但容器不存在的不上报（平台视为 missing）。
// docker 出错时按本地记录报 unknown；挂载检查出错的实例也报 unknown（平台对 unknown 不做对账动作）。
// 整个采集受 reportTimeout 约束：dockerd 卡死（超时）与 docker 出错同样处理。
// 重启后待重试启动（recoverPending）且仍在宽限期内的停止实例报 unknown，避免平台误报 container_down。
func (e *Executor) Report(ctx context.Context) []InstanceState {
	ctx, cancel := context.WithTimeout(ctx, e.reportTimeout)
	defer cancel()
	return e.report(ctx)
}

func (e *Executor) report(ctx context.Context) []InstanceState {
	e.mu.Lock()
	recovering := map[string]bool{}
	for id, since := range e.recoverPending {
		if time.Since(since) < e.recoverGrace {
			recovering[id] = true
		}
	}
	e.mu.Unlock()
	list, err := e.d.Runtime.List(ctx)
	if err != nil {
		e.d.Log.Error("list containers", "err", err)
		var out []InstanceState
		for _, inst := range e.d.State.Instances() {
			out = append(out, InstanceState{InstanceID: inst.ID, Phase: "unknown"})
		}
		return out
	}
	var out []InstanceState
	for _, c := range list {
		if c.Label == "" {
			continue
		}
		phase := "stopped"
		if c.Running {
			phase = "running"
		}
		mounted, err := e.d.Disk.Mounted(ctx, c.Label)
		switch {
		case err != nil:
			e.d.Log.Error("check data mount", "instance", c.Label, "err", err)
			phase = "unknown"
		case !mounted:
			phase = "missing_data"
		case phase == "stopped" && recovering[c.Label]:
			phase = "unknown"
		}
		out = append(out, InstanceState{InstanceID: c.Label, Phase: phase})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].InstanceID < out[j].InstanceID })
	return out
}

// noteContainer 记下 Inspect 得到的容器完整 ID（容器不存在时清除），供 Mounts 拼 cgroup 路径。
func (e *Executor) noteContainer(id string, c runtime.Container) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if c.Exists && c.FullID != "" {
		e.fullIDs[id] = c.FullID
	} else {
		delete(e.fullIDs, id)
	}
}

// Mounts 返回本地记录里每个实例的指标采集位置（心跳用，不调用 docker）：数据盘挂载目录与容器 cgroup 目录。
// 容器完整 ID 来自最近一次 Inspect（启动、恢复、60 秒一轮的限速巡检都会刷新）；还不知道时 CgroupDir 为空。
func (e *Executor) Mounts() []metrics.InstanceMount {
	insts := e.d.State.Instances()
	e.mu.Lock()
	ids := make(map[string]string, len(e.fullIDs))
	for k, v := range e.fullIDs {
		ids[k] = v
	}
	e.mu.Unlock()
	out := make([]metrics.InstanceMount, 0, len(insts))
	for _, inst := range insts {
		out = append(out, metrics.InstanceMount{ID: inst.ID, DataDir: e.d.Disk.MountDir(inst.ID), CgroupDir: e.cgroupDir(ids[inst.ID])})
	}
	return out
}

// cgroupDir 由容器完整 ID 拼 cgroup v2 目录：优先 systemd 驱动的 system.slice/docker-<id>.scope（DGX Spark 的 Docker 29 默认），
// 不存在而 cgroupfs 驱动的 docker/<id> 存在时用后者；都不存在（容器未运行）时仍给 systemd 路径，采集读不到即为 0。
func (e *Executor) cgroupDir(fullID string) string {
	if fullID == "" || strings.ContainsAny(fullID, "/.") {
		return ""
	}
	sd := filepath.Join(e.d.CgroupRoot, "system.slice", "docker-"+fullID+".scope")
	if _, err := os.Stat(sd); err != nil {
		if fs := filepath.Join(e.d.CgroupRoot, "docker", fullID); dirExists(fs) {
			return fs
		}
	}
	return sd
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// Recover 宿主机重启后：网桥与规则 → 挂载本地记录里全部实例的数据盘（期望 stopped 的也挂，否则会被误报 missing_data）
// → 只对期望 running 的实例校验后启动；任一失败只记录并继续。
// 隔离规则失败时 fail closed：一个租户容器都不启动（保持停止，StateReport 如实上报，平台据此告警），
// 并记下 recoverBlocked，由 Run 在规则刷新成功后重试。
func (e *Executor) Recover(ctx context.Context) error {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	// 每一步都有 recoverStepTimeout 上限：卡死的 dockerd / mount 不能让 Agent 在连上平台之前无限阻塞，
	// 超时按失败处理（实例进入 recoverPending，由规则刷新轮次重试）。
	var errs []error
	if err := e.withStep(ctx, e.d.Runtime.EnsureNetwork); err != nil {
		errs = append(errs, err)
	}
	rulesErr := e.withStep(ctx, e.EnsureRules)
	if rulesErr != nil {
		errs = append(errs, fmt.Errorf("网络隔离规则未就绪：%w", rulesErr))
	}
	blocked := false
	for _, inst := range e.d.State.Instances() {
		if err := e.withStep(ctx, func(ctx context.Context) error { return e.d.Disk.Mount(ctx, inst.ID) }); err != nil {
			errs = append(errs, fmt.Errorf("instance %s: 数据盘未挂载：%w", inst.ID, err))
			if inst.Desired == "running" {
				e.setRecoverPending(inst.ID, true)
			}
			continue
		}
		if err := e.writeInitScript(inst.ID); err != nil {
			errs = append(errs, fmt.Errorf("instance %s: 更新 init.sh 失败：%w", inst.ID, err))
		}
		if inst.Desired != "running" {
			continue
		}
		if rulesErr != nil {
			blocked = true
			errs = append(errs, fmt.Errorf("instance %s: 未启动：网络隔离规则未就绪", inst.ID))
			continue
		}
		err := e.withStep(ctx, func(ctx context.Context) error { return e.startChecked(ctx, inst) })
		if err != nil {
			errs = append(errs, fmt.Errorf("instance %s: %w", inst.ID, err))
		}
		e.setRecoverPending(inst.ID, err != nil)
	}
	e.mu.Lock()
	e.recoverBlocked = blocked
	e.mu.Unlock()
	return errors.Join(errs...)
}

// setRecoverPending 登记 / 移出需要在规则刷新轮次里重试启动的实例；已登记的保留首次登记时间。
func (e *Executor) setRecoverPending(id string, pending bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if pending {
		if _, ok := e.recoverPending[id]; !ok {
			e.recoverPending[id] = time.Now()
		}
	} else {
		delete(e.recoverPending, id)
	}
}

func procMemTotal() uint64 {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "MemTotal:" {
			kb, _ := strconv.ParseUint(f[1], 10, 64)
			return kb * 1024
		}
	}
	return 0
}
