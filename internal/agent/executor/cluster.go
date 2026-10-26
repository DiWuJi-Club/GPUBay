package executor

// 计划 13 集群成员（规格 §3.3～§3.6、§4.3）：create 时的容器参数、docker start 之后建 / 修 spc0（clusternet.Apply）、
// ConfigureCluster、60 秒巡检、洗机清理、cluster.{json,env} 与 InstanceState.cluster。
//
// 关键约束：Recover / start 会对已在运行的容器重跑 startChecked，所以这里每次都直接调 Apply——
// clusternet 保证 Apply 差量、非破坏性（容器里已有 spc0 时绝不 link del / add），正在跑的 NCCL / Ray 连接不受影响。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"spark-cloud/internal/agent/clusternet"
	"spark-cloud/internal/agent/state"
	"spark-cloud/internal/clusterspec"
)

const (
	configureClusterTimeout = 2 * time.Minute // 与 rental.ConfigureClusterCommandTimeout 一致

	clusterStatusOK      = "ok"
	clusterStatusPending = "pending" // 容器未运行，或本进程还没 Apply 过（Agent 刚重启）
	maxClusterStatus     = 200       // InstanceState.cluster 上限（字节，合法 UTF-8）

	clusterJSONFile = "cluster.json" // 容器内 /.spark/cluster.json（init.sh 软链到 /etc/spark-cluster.json）
	clusterEnvFile  = "cluster.env"  // 容器内 /.spark/cluster.env（登录 shell 经 /etc/profile.d 导入）

	pingGroupRange = "0 2147483647" // 去掉 NET_RAW 后保留 ICMP 数据报套接字（镜像里有 ping 时可用，不保证）

	noteModeChanged = "集群网络模式变更需重建实例"
	noteRDMAChanged = "RDMA 变更需重建实例"
	msgNoGID        = "未找到 RoCE GID，已退回 TCP" // Apply 只返回裸 ErrNoGID 时的文案
)

// noGIDStatus：RDMA 退回 TCP 的状态文案取 Apply 错误本身（未找到 GID / 网卡无 RDMA 设备 / 设备编号变化需重建），
// 去掉末尾包装的 ErrNoGID 文本。
func noGIDStatus(err error) string {
	msg := strings.TrimPrefix(err.Error(), clusterspec.ErrPrefix)
	for _, sep := range []string{"：", ": "} {
		msg = strings.TrimSuffix(msg, sep+clusternet.ErrNoGID.Error())
	}
	if msg == "" || msg == clusternet.ErrNoGID.Error() {
		msg = msgNoGID
	}
	return statusError(msg)
}

// clusterErr 给集群网络相关的失败加 clusterspec.ErrPrefix（平台据此走集群失败路径、不判机器故障）。
func clusterErr(err error) error {
	if err == nil || strings.HasPrefix(err.Error(), clusterspec.ErrPrefix) {
		return err
	}
	return fmt.Errorf("%s%w", clusterspec.ErrPrefix, err)
}

func clusterErrf(format string, args ...any) error {
	return errors.New(clusterspec.ErrPrefix + fmt.Sprintf(format, args...))
}

// statusError 把错误转成 InstanceState.cluster 的 "error: <原因>"：去掉 ErrPrefix、压成一行、截到 200 字节（不截断多字节字符）。
func statusError(msg string) string {
	msg = strings.TrimPrefix(msg, clusterspec.ErrPrefix)
	msg = strings.Join(strings.Fields(msg), " ")
	if msg == "" {
		msg = "集群网络异常"
	}
	s := "error: " + msg
	if len(s) <= maxClusterStatus {
		return s
	}
	s = s[:maxClusterStatus]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// ClusterCapable：Agent 接了集群数据面（Hello.features 据此带 features.Cluster）。
func (e *Executor) ClusterCapable() bool { return e.d.Cluster != nil }

// NetInfo 探测本机网卡（Hello.net / StateReport.net）。cfg.LanIP 在 AgentConfig 到达前为空，照样探测（LanIface 为空，
// 平台按 machines.lan_ip 认管理网卡）。不支持集群或探测出错 → nil（该次消息不带 net，平台保留旧记录）。受 reportTimeout 约束。
func (e *Executor) NetInfo(ctx context.Context) *clusterspec.NetInfo {
	if e.d.Cluster == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, e.reportTimeout)
	defer cancel()
	n, err := e.d.Cluster.Probe(ctx, e.Config().LanIP)
	if err != nil {
		e.d.Log.Warn("probe network interfaces", "err", err)
		return nil
	}
	return &n
}

// checkClusterCommand 校验 create / configure 带来的 Spec：Agent 支持集群、字段合法、self 就是这个实例。
func (e *Executor) checkClusterSpec(id string, s clusterspec.Spec) error {
	if e.d.Cluster == nil {
		return clusterErrf("本机 Agent 未启用集群网络")
	}
	if err := s.Validate(); err != nil {
		return clusterErr(err)
	}
	if self := s.Self(); self.InstanceID != id {
		return clusterErrf("集群参数的 rank %d 是实例 %s，不是 %s", s.Rank, self.InstanceID, id)
	}
	return nil
}

// clusterCreateArgs 是集群容器的 docker create 附加参数（规格 §3.5 / M16）：去掉 NET_RAW、保留 ICMP 数据报套接字、RDMA 设备。
func (e *Executor) clusterCreateArgs(s clusterspec.Spec) (capDrop []string, sysctls map[string]string, devices []string, err error) {
	devices, err = e.d.Cluster.Devices(s)
	if err != nil {
		return nil, nil, nil, clusterErr(fmt.Errorf("RDMA 设备不可用：%w", err))
	}
	return []string{"NET_RAW"}, map[string]string{"net.ipv4.ping_group_range": pingGroupRange}, devices, nil
}

// sameClusterArgs：已存在的容器是否按同样的集群参数创建（Inspect 不比较 cap-drop / 设备，以本地记录为准）。
func sameClusterArgs(prev, next *clusterspec.Spec) bool {
	if prev == nil || next == nil {
		return prev == nil && next == nil
	}
	return prev.RDMA == next.RDMA
}

func runtimeOf(inst state.Instance) clusterspec.Runtime {
	if inst.ClusterRuntime != nil {
		return *inst.ClusterRuntime
	}
	return clusterspec.Runtime{}
}

// writeClusterFiles 原子写 ssh/<id>/cluster.{json,env}（0644）；内容未变时不写（巡检每轮都会调用）。
func (e *Executor) writeClusterFiles(id string, s clusterspec.Spec, rt clusterspec.Runtime) error {
	dir := e.sshDir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, f := range []struct {
		name string
		data []byte
	}{{clusterJSONFile, clusterspec.RenderJSON(s, rt)}, {clusterEnvFile, clusterspec.RenderEnv(s, rt)}} {
		p := filepath.Join(dir, f.name)
		if cur, err := os.ReadFile(p); err == nil && bytes.Equal(cur, f.data) {
			continue
		}
		if err := writeFileAtomic(p, f.data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func (e *Executor) removeClusterFiles(id string) error {
	var errs []error
	for _, name := range []string{clusterJSONFile, clusterEnvFile} {
		if err := os.Remove(filepath.Join(e.sshDir(id), name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// otherClusterSpecs：本机除 id 以外全部集群实例的 Spec（Teardown 用来判断 VLAN 子接口是否还有人用）。
func (e *Executor) otherClusterSpecs(id string) []clusterspec.Spec {
	var out []clusterspec.Spec
	for _, inst := range e.d.State.Instances() {
		if inst.ID != id && inst.Cluster != nil {
			out = append(out, *inst.Cluster)
		}
	}
	return out
}

// syncClusterRules 按本机全部「routed 且期望 running」的成员重建 SPARK-CLU；exclude 非空时排除该实例（失败清理 / 洗机）。
func (e *Executor) syncClusterRules(ctx context.Context, exclude string) error {
	routed := map[string]clusterspec.Spec{}
	for _, inst := range e.d.State.Instances() {
		if inst.ID != exclude && inst.Cluster != nil && inst.Cluster.Mode == clusterspec.ModeRouted && inst.Desired == "running" {
			routed[inst.ID] = *inst.Cluster
		}
	}
	return e.d.Cluster.SyncRules(ctx, routed)
}

func (e *Executor) setClusterStatus(id, s string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if s == "" {
		delete(e.clusterStatus, id)
	} else {
		e.clusterStatus[id] = s
	}
}

func (e *Executor) clusterStatusOf(id string) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s, ok := e.clusterStatus[id]
	return s, ok
}

// reportedClusterStatus 是 InstanceState.cluster：非集群 ""；容器未运行或本进程还没 Apply 过 "pending"；否则最近一次 Apply / Check 的结果。
func (e *Executor) reportedClusterStatus(inst state.Instance, running bool) string {
	if inst.Cluster == nil {
		return ""
	}
	if !running {
		return clusterStatusPending
	}
	if s, ok := e.clusterStatusOf(inst.ID); ok {
		return s
	}
	return clusterStatusPending
}

// applyCluster 在 startChecked 末尾（限速成功之后）配置 spc0：Apply → 记下 Runtime → 按 Runtime 重写文件 → routed 同步规则。
// creating（create 调用链）时失败返回 ErrPrefix 错误，并在本机清理（Teardown + SyncRules + 停容器）；
// 其它调用链（start / Recover / ConfigureCluster / 巡检）失败只记状态 error、返回 nil，容器保持运行（隔离不受影响：
// routed 缺规则时 FORWARD 默认丢弃，macvlan 不经宿主），由 60 秒巡检重试。
func (e *Executor) applyCluster(ctx context.Context, inst state.Instance, creating bool) error {
	id := inst.ID
	fail := func(err error) error {
		if creating {
			e.clusterCreateCleanup(ctx, id)
			return clusterErr(err)
		}
		e.d.Log.Error("cluster network apply failed", "instance", id, "err", err)
		e.setClusterStatus(id, statusError(err.Error()))
		return nil
	}
	if e.d.Cluster == nil {
		return fail(errors.New("本机 Agent 未启用集群网络"))
	}
	spec := *inst.Cluster
	c, err := e.d.Runtime.Inspect(ctx, id)
	if err != nil {
		return fail(fmt.Errorf("查询容器失败：%w", err))
	}
	if !c.Exists || !c.Running || c.Pid <= 0 {
		return fail(errors.New("容器未运行"))
	}
	rt, applyErr := e.d.Cluster.Apply(ctx, id, c.Pid, spec)
	noGID := errors.Is(applyErr, clusternet.ErrNoGID)
	if applyErr != nil && !noGID {
		// RDMA 不可用时先把文件切到 TCP（NCCL_IB_DISABLE=1），免得旧文件里的 GID 让 NCCL 走坏掉的 RDMA。
		if !creating && spec.RDMA && !rt.RDMAActive {
			if err := e.saveClusterRuntime(id, spec, rt); err != nil {
				e.d.Log.Error("write cluster files", "instance", id, "err", err)
			}
		}
		return fail(applyErr)
	}
	if err := e.saveClusterRuntime(id, spec, rt); err != nil {
		return fail(fmt.Errorf("写集群信息文件失败：%w", err))
	}
	if spec.Mode == clusterspec.ModeRouted {
		if err := e.syncClusterRules(ctx, ""); err != nil {
			return fail(fmt.Errorf("同步集群隔离规则失败：%w", err))
		}
	}
	switch cur, _ := e.d.State.Instance(id); {
	case cur.ClusterNote != "":
		e.setClusterStatus(id, statusError(cur.ClusterNote))
	case noGID:
		e.setClusterStatus(id, noGIDStatus(applyErr))
	default:
		e.setClusterStatus(id, clusterStatusOK)
	}
	return nil
}

// saveClusterRuntime 记下 Apply 得到的 Runtime（变化时才写状态文件）并按它重写 cluster.{json,env}。
func (e *Executor) saveClusterRuntime(id string, spec clusterspec.Spec, rt clusterspec.Runtime) error {
	if cur, ok := e.d.State.Instance(id); ok && (cur.ClusterRuntime == nil || *cur.ClusterRuntime != rt) {
		cur.ClusterRuntime = &rt
		if err := e.d.State.PutInstance(cur); err != nil {
			return err
		}
	}
	return e.writeClusterFiles(id, spec, rt)
}

// clusterCreateCleanup：集群 create 失败后的本机清理——Teardown（对端路由、VLAN 子接口）+ SyncRules（去掉该实例）+ 停容器，
// 期望状态改为 stopped（不再进规则、不被恢复启动）。容器与数据盘由平台随后的洗机删除。脱离调用方 ctx（它可能已超时）。
func (e *Executor) clusterCreateCleanup(ctx context.Context, id string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopAfterFailureTimeout)
	defer cancel()
	inst, ok := e.d.State.Instance(id)
	if ok && inst.Cluster != nil && e.d.Cluster != nil {
		if err := e.d.Cluster.Teardown(ctx, id, *inst.Cluster, e.otherClusterSpecs(id)); err != nil {
			e.d.Log.Error("cluster teardown after failed create", "instance", id, "err", err)
		}
		if err := e.syncClusterRules(ctx, id); err != nil {
			e.d.Log.Error("cluster rules after failed create", "instance", id, "err", err)
		}
	}
	e.stopAfterFailure(ctx, id)
	if ok && inst.Desired != "stopped" {
		inst.Desired = "stopped"
		if err := e.d.State.PutInstance(inst); err != nil {
			e.d.Log.Error("persist instance after failed cluster create", "instance", id, "err", err)
		}
	}
	e.setClusterStatus(id, "")
}

// configureCluster 执行 ConfigureCluster（规格 §4.3）。
func (e *Executor) configureCluster(ctx context.Context, c Command) error {
	id := c.InstanceID
	if err := checkID(id); err != nil {
		return err
	}
	inst, ok := e.d.State.Instance(id)
	if !ok {
		e.d.Log.Info("configure_cluster for unknown instance ignored", "instance", id)
		return nil
	}
	var target clusterspec.Spec
	if c.Cluster != nil {
		target = *c.Cluster
	}
	if target.ClusterID == "" {
		return e.leaveCluster(ctx, inst)
	}
	if err := e.checkClusterSpec(id, target); err != nil {
		return err
	}
	if inst.Cluster == nil {
		return clusterErrf("实例 %s 创建时不是集群成员（缺少集群容器参数），需重建实例", id)
	}
	old := *inst.Cluster
	if old.ClusterID != target.ClusterID {
		return clusterErrf("实例 %s 属于集群 %s，不能改配为 %s", id, old.ClusterID, target.ClusterID)
	}
	if target.Version < old.Version {
		e.d.Log.Info("stale configure_cluster ignored", "instance", id, "version", target.Version, "local", old.Version)
		return nil
	}
	rt := runtimeOf(inst)
	// 设备与网卡形态只能在 create 时决定：
	//   模式 / 网卡 / VLAN 变了 → 只更新文件（成员信息），网络按 create 时的形态继续，状态提示重建；
	//   RDMA 由无变有 → 按 RDMA=false 生效并提示重建；由有变无 → 退回 TCP（NCCL_IB_DISABLE=1）。
	if target.Mode != old.Mode || target.Iface != old.Iface || target.VLAN != old.VLAN {
		files := target
		files.RDMA = target.RDMA && old.RDMA
		inst.ClusterNote = noteModeChanged
		if err := e.d.State.PutInstance(inst); err != nil {
			return err
		}
		if err := e.writeClusterFiles(id, files, rt); err != nil {
			return clusterErr(fmt.Errorf("写集群信息文件失败：%w", err))
		}
		e.setClusterStatus(id, statusError(noteModeChanged))
		return nil
	}
	merged := target
	merged.MTU = old.MTU // MTU 在建网卡时决定
	inst.ClusterNote = ""
	if target.RDMA && !old.RDMA {
		merged.RDMA = false
		inst.ClusterNote = noteRDMAChanged
	}
	inst.Cluster = &merged
	if err := e.d.State.PutInstance(inst); err != nil {
		return err
	}
	if err := e.writeClusterFiles(id, merged, rt); err != nil {
		return clusterErr(fmt.Errorf("写集群信息文件失败：%w", err))
	}
	cur, err := e.d.Runtime.Inspect(ctx, id)
	if err != nil {
		return err
	}
	if !cur.Exists || !cur.Running {
		if inst.ClusterNote != "" {
			e.setClusterStatus(id, statusError(inst.ClusterNote))
		}
		return nil // 下次启动时 startChecked 按新 Spec Apply
	}
	return e.applyCluster(ctx, inst, false)
}

// leaveCluster：ConfigureCluster 的 cluster_id 为空——清理网络（Teardown 删 spc0 与对端路由）、删文件、清 state、同步规则。
// macvlan 成员的容器在运行时拒绝（失败、state 不变）：容器内的 spc0 由 netns 持有，Teardown 拿不到 pid 删不掉
// （direct 口 / 仍有人用的 VLAN 上它会继续留在原二层里，之后同 fabric 的新集群会与它同网），只有停容器才会消失。
func (e *Executor) leaveCluster(ctx context.Context, inst state.Instance) error {
	id := inst.ID
	var errs []error
	if inst.Cluster != nil && inst.Cluster.Mode == clusterspec.ModeMacvlan {
		cur, err := e.d.Runtime.Inspect(ctx, id)
		if err != nil {
			return err
		}
		if cur.Exists && cur.Running {
			return clusterErrf("macvlan 集群成员运行中不能退出集群（容器内 spc0 只能随容器停止移除），请先停止实例")
		}
	}
	if inst.Cluster != nil && e.d.Cluster != nil {
		if err := e.d.Cluster.Teardown(ctx, id, *inst.Cluster, e.otherClusterSpecs(id)); err != nil {
			return clusterErr(fmt.Errorf("清理集群网络失败：%w", err)) // state 保留，平台重发时再清
		}
	}
	if err := e.removeClusterFiles(id); err != nil {
		errs = append(errs, err)
	}
	inst.Cluster, inst.ClusterRuntime, inst.ClusterNote = nil, nil, ""
	if err := e.d.State.PutInstance(inst); err != nil {
		errs = append(errs, err)
	}
	e.setClusterStatus(id, "")
	if e.d.Cluster != nil {
		if err := e.syncClusterRules(ctx, ""); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return clusterErr(err)
	}
	return nil
}

// wipeCluster：洗机删容器之后清理该实例的集群网络（规格 §4.3）；返回的问题计入洗机校验失败。
func (e *Executor) wipeCluster(ctx context.Context, inst state.Instance) []string {
	e.setClusterStatus(inst.ID, "")
	if inst.Cluster == nil || e.d.Cluster == nil {
		return nil
	}
	var problems []string
	if err := e.d.Cluster.Teardown(ctx, inst.ID, *inst.Cluster, e.otherClusterSpecs(inst.ID)); err != nil {
		problems = append(problems, "清理集群网络失败: "+err.Error())
	}
	if err := e.syncClusterRules(ctx, inst.ID); err != nil {
		problems = append(problems, "同步集群隔离规则失败: "+err.Error())
	}
	return problems
}

// ensureClusters 是 60 秒巡检（refreshRules 内，与 ensureLimits 同样 opMu.TryLock）：对每个运行中的集群实例 Check，
// 不通过或上一轮状态不是 ok（error / Agent 重启后还没 Apply）→ 重新 Apply；每轮都回写状态。
// Check 发现问题而 Apply 修好的那一轮仍如实回写 "error: <Check 的描述>"（例如父接口被 NM 重新加了地址、已清除），
// 让平台看见并告警一次；下一轮因上轮 error 再 Apply 一次，Check 通过后恢复 ok。最后按当前成员同步 SPARK-CLU。
func (e *Executor) ensureClusters(ctx context.Context) {
	if e.d.Cluster == nil || !e.opMu.TryLock() {
		return
	}
	defer e.opMu.Unlock()
	for _, inst := range e.d.State.Instances() {
		if inst.Cluster == nil || inst.Desired != "running" {
			continue
		}
		if ctx.Err() != nil {
			return
		}
		_ = e.withStep(ctx, func(ctx context.Context) error { e.repairCluster(ctx, inst); return nil })
	}
	if err := e.withStep(ctx, func(ctx context.Context) error { return e.syncClusterRules(ctx, "") }); err != nil {
		e.d.Log.Error("sync cluster rules", "err", err)
		for _, inst := range e.d.State.Instances() {
			if inst.Cluster != nil && inst.Cluster.Mode == clusterspec.ModeRouted && inst.Desired == "running" {
				e.setClusterStatus(inst.ID, statusError("同步集群隔离规则失败："+err.Error()))
			}
		}
	}
}

func (e *Executor) repairCluster(ctx context.Context, inst state.Instance) {
	c, err := e.d.Runtime.Inspect(ctx, inst.ID)
	if err != nil {
		e.d.Log.Error("cluster check: inspect", "instance", inst.ID, "err", err)
		return
	}
	e.noteContainer(inst.ID, c)
	if !c.Exists || !c.Running {
		return // 报 pending；容器起来时 startChecked 会 Apply
	}
	ok, checkErr := e.d.Cluster.Check(ctx, inst.ID, c.Pid, *inst.Cluster)
	prev, _ := e.clusterStatusOf(inst.ID)
	if ok && prev == clusterStatusOK {
		return
	}
	if !ok {
		e.d.Log.Warn("cluster network check failed, re-applying", "instance", inst.ID, "err", checkErr)
	}
	_ = e.applyCluster(ctx, inst, false)
	if !ok {
		if cur, _ := e.clusterStatusOf(inst.ID); cur == clusterStatusOK {
			msg := "集群网络巡检未通过，已修复"
			if checkErr != nil {
				msg = checkErr.Error()
			}
			e.setClusterStatus(inst.ID, statusError(msg))
		}
	}
}
