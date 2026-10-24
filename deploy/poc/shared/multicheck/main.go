// multicheck 在真机上直接驱动 Agent 执行器（与 spark-agent run 同一套 docker / loop / iptables 构造），
// 验收一台机器多个容器：按份配额、端口冲突兜底、Mounts + 每实例 cgroup 指标、容器间隔离、整机实例、洗机。
// 以 root 运行，运行前先停掉 spark-agent 服务。用法见 deploy/poc/shared/README.md。
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"

	"spark-cloud/internal/agent/cmdrun"
	"spark-cloud/internal/agent/disk/loop"
	"spark-cloud/internal/agent/executor"
	"spark-cloud/internal/agent/metrics"
	"spark-cloud/internal/agent/netfilter/iptables"
	"spark-cloud/internal/agent/runtime/docker"
	"spark-cloud/internal/agent/state"
)

type harness struct {
	ctx      context.Context
	run      cmdrun.Exec
	ex       *executor.Executor
	dk       *loop.Disk
	rt       *docker.Runtime
	stateDir string
	lanIP    string
	image    string
	bridge   string
	dataGiB  int
	pubLine  string // 上游公钥（authorized_keys 一行）
	memTotal uint64
	ncpu     int
	failed   int
	created  []string        // 提交过 create 的实例（含失败的 C），清理时逐个洗
	wiped    map[string]bool // 已成功洗掉
}

func main() { os.Exit(run()) }

func run() (code int) {
	now := time.Now().Unix()
	stateDir := flag.String("state-dir", fmt.Sprintf("/var/tmp/spark-multicheck/%d", now), "执行器状态目录（数据盘镜像、ssh 目录、state.json）")
	image := flag.String("image", "spark/cuda-ssh:13.0.1", "实例镜像（须已在本机）")
	lanIP := flag.String("lan-ip", "", "SSH 端口发布地址；空 = 默认路由的源地址")
	dataGiB := flag.Int("data-gib", 2048, "整机数据盘大小（对应 AgentConfig.data_size_gib），共享实例按份等分")
	bridge := flag.String("bridge", "spark0", "容器网桥")
	allow := flag.String("allow", "", "出网白名单 CIDR（逗号分隔）；会覆盖 SPARK-FWD 链，真 Agent 重启收到配置后恢复")
	keep := flag.Bool("keep", false, "结束时不洗机（保留容器与数据盘便于排查）")
	whole := flag.Bool("whole", true, "洗掉 A/B 后再建一个整机实例 D（ssh 端口 2225）检查整机参数")
	flag.Parse()

	if os.Geteuid() != 0 {
		fmt.Println("必须以 root 运行")
		return 2
	}
	if *lanIP == "" {
		*lanIP = defaultRouteIP()
	}
	if !slices.Contains(localIPs(), *lanIP) {
		fmt.Printf("--lan-ip %q 不是本机地址（本机：%v）\n", *lanIP, localIPs())
		return 2
	}
	if err := os.MkdirAll(*stateDir, 0o700); err != nil {
		fmt.Println("创建状态目录失败:", err)
		return 2
	}
	st, err := state.Open(filepath.Join(*stateDir, "state.json"))
	if err != nil {
		fmt.Println("打开状态文件失败:", err)
		return 2
	}
	pubLine, err := upstreamKey()
	if err != nil {
		fmt.Println("生成上游公钥失败:", err)
		return 2
	}

	// 第一次 SIGINT / SIGTERM 取消正在执行的步骤；之后的信号被 NotifyContext 吞掉，保证清理跑完。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 与 cmd/spark-agent runAgent 相同的依赖构造；执行器用步进模式（Submit + RunPending 同步拿结果）。
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	runner := cmdrun.Exec{}
	h := &harness{ctx: ctx, run: runner, stateDir: *stateDir, lanIP: *lanIP, image: *image, bridge: *bridge, dataGiB: *dataGiB,
		pubLine: pubLine, ncpu: goruntime.NumCPU(), wiped: map[string]bool{}}
	h.rt = &docker.Runtime{Run: runner, Bridge: *bridge, Subnet: "10.200.0.0/24"}
	h.dk = &loop.Disk{Run: runner, Stat: os.Stat, StateDir: *stateDir, Log: log}
	nf := &iptables.Netfilter{Run: runner, Log: log}
	h.ex = executor.New(executor.Deps{Runtime: h.rt, Disk: h.dk, Net: nf, State: st, StateDir: *stateDir, Bridge: *bridge, LocalIPs: localIPs, Log: log}, true)
	var cidrs []string
	if *allow != "" {
		cidrs = strings.Split(*allow, ",")
	}
	h.ex.SetConfig(executor.Config{LanIP: *lanIP, AllowCIDRs: cidrs, BandwidthMbps: 200, DataSizeGiB: *dataGiB})
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		h.memTotal, _ = metrics.ParseMeminfo(b)
	}
	fmt.Printf("state-dir=%s image=%s lan-ip=%s data-gib=%d MemTotal=%d NumCPU=%d\n", *stateDir, *image, *lanIP, *dataGiB, h.memTotal, h.ncpu)

	// 实例 ID 必须匹配 ^ins-[a-z0-9]{8}$：ins-m<6 位 base36 时间><a|b|c|d>
	tag := strconv.FormatInt(now, 36)
	tag = strings.Repeat("0", max(0, 6-len(tag))) + tag
	id := func(s string) string { return "ins-m" + tag[len(tag)-6:] + s }
	a, b, c, d := id("a"), id("b"), id("c"), id("d")

	defer func() { // 无论步骤是否中途失败都清理
		if *keep {
			fmt.Printf("\n--keep：保留实例 %v，状态目录 %s\n", h.created, *stateDir)
		} else {
			h.cleanup()
		}
		if h.failed == 0 {
			fmt.Println("\nRESULT: PASS")
		} else {
			fmt.Printf("\nRESULT: FAIL (%d checks failed)\n", h.failed)
			code = 1
		}
	}()
	if err := h.steps(a, b, c, d, *whole); err != nil {
		h.fail("中止", "%v", err)
	}
	return 0 // 清理与结果在 defer 里，失败时改为 1
}

func (h *harness) steps(a, b, c, d string, whole bool) error {
	header("1 建共享实例 A（4 份，2223）与 B（4 份，2224）")
	for _, x := range []struct {
		id   string
		port int
	}{{a, 2223}, {b, 2224}} {
		r := h.create(x.id, 4, x.port)
		h.check(r.OK, "create "+x.id, "ok=%v msg=%q", r.OK, r.Message)
		if !r.OK {
			return errors.New("建共享实例失败")
		}
	}

	header("2 docker inspect 与数据盘")
	mem := map[string]int64{}
	for _, x := range []struct {
		id   string
		port int
	}{{a, 2223}, {b, 2224}} {
		mem[x.id] = h.verifySpec(x.id, 4, x.port)
	}

	header("3 C 用已占用端口 2223 → 端口冲突")
	r := h.create(c, 4, 2223)
	h.check(!r.OK && strings.Contains(r.Message, "SSH 端口冲突"), "create "+c+" rejected", "ok=%v msg=%q", r.OK, r.Message)
	ci, err := h.rt.Inspect(h.ctx, c)
	h.check(err == nil && !ci.Exists, "C 无容器", "exists=%v err=%v", ci.Exists, err)
	ok, err := h.dk.Exists(h.ctx, c)
	h.check(err == nil && !ok, "C 无数据盘", "exists=%v err=%v", ok, err)

	header("4 Mounts() 与每实例 cgroup 指标")
	mounts := h.ex.Mounts()
	h.check(len(mounts) == 2, "Mounts 两项", "n=%d", len(mounts))
	for _, m := range mounts {
		st, err := os.Stat(m.CgroupDir)
		h.check(m.CgroupDir != "" && err == nil && st.IsDir(), "cgroup 目录 "+m.ID, "dir=%q data=%q err=%v", m.CgroupDir, m.DataDir, err)
	}
	col := &metrics.Collector{Run: h.run, ReadFile: os.ReadFile}
	idle := h.sample(col, mounts)
	for _, im := range idle {
		h.check(im.MemMax == uint64(mem[im.InstanceID]), "mem_max "+im.InstanceID, "mem_max=%d docker Memory=%d", im.MemMax, mem[im.InstanceID])
	}
	fmt.Println("  在 A 内跑两个 8 秒的空转循环 …")
	burn := exec.CommandContext(h.ctx, "docker", "exec", a, "sh", "-c",
		`timeout 8 sh -c "while :; do :; done" & timeout 8 sh -c "while :; do :; done" & wait`)
	if err := burn.Start(); err != nil {
		return fmt.Errorf("启动 CPU 负载失败: %w", err)
	}
	time.Sleep(time.Second)
	busy := h.sample(col, mounts)
	_ = burn.Wait()
	for _, im := range busy {
		if im.InstanceID == a {
			h.check(im.CPUPercent >= 150 && im.CPUPercent <= 250, "A cpu_percent ≈ 200", "cpu_percent=%.1f", im.CPUPercent)
		} else {
			h.check(im.CPUPercent < 20, "B cpu_percent < 20", "cpu_percent=%.1f", im.CPUPercent)
		}
	}

	header("5 容器内看到的 cgroup 限额")
	for _, id := range []string{a, b} {
		memMax := strings.TrimSpace(h.out("docker", "exec", id, "cat", "/sys/fs/cgroup/memory.max"))
		cpuMax := strings.TrimSpace(h.out("docker", "exec", id, "cat", "/sys/fs/cgroup/cpu.max"))
		wantCPU := fmt.Sprintf("%d 100000", h.wantNano(4)/10000) // NanoCpus × 100000µs / 1e9
		h.check(memMax == strconv.FormatInt(mem[id], 10), "memory.max "+id, "%s", memMax)
		h.check(cpuMax == wantCPU, "cpu.max "+id, "%q（期望 %q）", cpuMax, wantCPU)
	}

	header("6 隔离")
	bIP := h.inspect(b).ipOn(h.bridge)
	err = h.runErr("docker", "exec", a, "timeout", "3", "bash", "-c", "exit 0") // 对照：镜像里有 bash / timeout，下面的失败才有意义
	h.check(err == nil, "A 内可执行 bash / timeout", "err=%v", err)
	err = h.runErr("docker", "exec", a, "timeout", "3", "bash", "-c", "</dev/tcp/"+bIP+"/22")
	h.check(bIP != "" && err != nil, "A → B 容器地址 "+bIP+":22 不通", "err=%v", err)
	err = h.runErr("docker", "exec", a, "timeout", "3", "bash", "-c", "</dev/tcp/"+h.lanIP+"/2224")
	h.check(err != nil, "A → 宿主机 "+h.lanIP+":2224 不通", "err=%v", err)
	for _, x := range []struct {
		id   string
		port int
	}{{a, 2223}, {b, 2224}} { // 正向对照：宿主机能连到各自的 sshd（说明上面的「不通」不是 sshd 没起来）
		addr := net.JoinHostPort(h.lanIP, strconv.Itoa(x.port))
		h.check(dialRetry(addr, 15*time.Second) == nil, "宿主机 → "+addr+" 可连", "%s", x.id)
	}

	header("7 洗掉 A、B（C）")
	for _, id := range []string{a, b, c} {
		h.wipe(id)
	}
	if !whole || h.ctx.Err() != nil {
		return h.ctx.Err()
	}
	header("7b 整机实例 D（share_slots 1，2225）")
	r = h.create(d, 1, 2225)
	h.check(r.OK, "create "+d, "ok=%v msg=%q", r.OK, r.Message)
	if r.OK {
		h.verifySpec(d, 1, 2225)
	}
	h.wipe(d)
	return nil
}

// create 按平台 CreateInstance → client.toCommand 的映射构造指令（平台总下发主机私钥 PEM 与带宽、数据盘大小）。
func (h *harness) create(id string, slots, port int) executor.ResultMsg {
	h.created = append(h.created, id)
	hostKey, err := hostKeyPEM()
	if err != nil {
		return executor.ResultMsg{Message: err.Error()}
	}
	return h.submit(executor.Command{ID: "mc-" + id + "-create", InstanceID: id, Type: "create_instance", ImageRef: h.image,
		UpstreamPubKey: h.pubLine, BandwidthMbps: 200, SSHPort: port, DataSizeGiB: h.dataGiB, HostKeyPEM: hostKey, ShareSlots: slots})
}

func (h *harness) wipe(id string) {
	if h.wiped[id] || !h.leftover(id) { // 什么都没建出来（如被拒的 C）就不洗：对不存在的镜像跑 losetup -j 没有意义
		return
	}
	r := h.submit(executor.Command{ID: fmt.Sprintf("mc-%s-wipe-%d", id, time.Now().UnixNano()), InstanceID: id, Type: "wipe_instance"})
	h.check(r.OK, "wipe "+id, "ok=%v msg=%q", r.OK, r.Message)
	if r.OK {
		h.wiped[id] = true
	}
}

// leftover：容器、数据盘镜像或本地记录任一仍在（查询出错也按仍在处理，交给洗机去报错）。
func (h *harness) leftover(id string) bool {
	c, err := h.rt.Inspect(h.ctx, id)
	img, derr := h.dk.Exists(h.ctx, id)
	_, rec := h.ex.StateStore().Instance(id)
	return err != nil || c.Exists || derr != nil || img || rec
}

func (h *harness) submit(c executor.Command) executor.ResultMsg {
	h.ex.Submit(c)
	for _, r := range h.ex.RunPending(h.ctx, nil) {
		if r.ID == c.ID {
			return r
		}
	}
	return executor.ResultMsg{ID: c.ID, Message: "没有拿到执行结果"}
}

// verifySpec 检查容器参数与数据盘，返回 docker 的 Memory 值。slots = 1 为整机。
func (h *harness) verifySpec(id string, slots, port int) int64 {
	n := uint64(slots)
	wantMem := int64((h.memTotal - executor.HostReserveBytes) / n)
	wantShm := min(int64(h.memTotal/2), wantMem)
	if slots > 1 {
		wantShm = wantMem / 2
	}
	in := h.inspect(id)
	hc := in.HostConfig
	pids := int64(-1)
	if hc.PidsLimit != nil {
		pids = *hc.PidsLimit
	}
	caps := make([]string, 0, len(hc.CapAdd))
	for _, c := range hc.CapAdd {
		caps = append(caps, strings.TrimPrefix(strings.ToUpper(c), "CAP_"))
	}
	fmt.Printf("  %s: Memory=%d MemorySwap=%d NanoCpus=%d PidsLimit=%d ShmSize=%d CapAdd=%v PortBindings=%v\n",
		id, hc.Memory, hc.MemorySwap, hc.NanoCpus, pids, hc.ShmSize, hc.CapAdd, hc.PortBindings)
	h.check(hc.Memory == wantMem, id+" Memory", "%d（期望 %d）", hc.Memory, wantMem)
	h.check(hc.MemorySwap == wantMem, id+" MemorySwap", "%d", hc.MemorySwap)
	h.check(hc.NanoCpus == h.wantNano(slots), id+" NanoCpus", "%d（期望 %d）", hc.NanoCpus, h.wantNano(slots))
	h.check(pids == int64(executor.MaxPids/slots), id+" PidsLimit", "%d（期望 %d）", pids, executor.MaxPids/slots)
	h.check(slices.Contains(caps, "IPC_LOCK") && slices.Contains(caps, "SYS_PTRACE") && slices.Contains(caps, "SYS_NICE") == (slots == 1),
		id+" CapAdd", "%v（SYS_NICE 期望 %v）", caps, slots == 1)
	h.check(hc.ShmSize == wantShm, id+" ShmSize", "%d（期望 %d）", hc.ShmSize, wantShm)
	bind := hc.PortBindings["22/tcp"]
	h.check(len(bind) == 1 && bind[0].HostIp == h.lanIP && bind[0].HostPort == strconv.Itoa(port), id+" 端口发布",
		"%v（期望 %s:%d → 22/tcp）", bind, h.lanIP, port)
	img := filepath.Join(h.stateDir, "data", id+".img") // 与 loop.Disk 的镜像路径一致
	wantSize := int64(h.dataGiB/slots) << 30
	var size int64 = -1
	if fi, err := os.Stat(img); err == nil {
		size = fi.Size()
	}
	h.check(size == wantSize, id+" 数据盘镜像大小", "%d = %d GiB（期望 %d GiB）", size, size>>30, wantSize>>30)
	mounted, err := h.dk.Mounted(h.ctx, id)
	fm := strings.TrimSpace(h.out("findmnt", "-n", "-o", "SOURCE,TARGET,SIZE", h.dk.MountDir(id)))
	h.check(err == nil && mounted, id+" 数据盘已挂载", "%s err=%v", fm, err)
	return hc.Memory
}

// wantNano：docker CLI 收到 --cpus %.2f 后换算的 NanoCpus；整机不传 --cpus，为 0。
func (h *harness) wantNano(slots int) int64 {
	if slots <= 1 {
		return 0
	}
	v, _ := strconv.ParseFloat(strconv.FormatFloat(float64(h.ncpu)/float64(slots), 'f', 2, 64), 64)
	return int64(math.Round(v * 1e9))
}

// sample 采两次（间隔 3 秒），打印并返回第二次的每实例指标（CPU 百分比取两次之间）。
func (h *harness) sample(col *metrics.Collector, mounts []metrics.InstanceMount) []metrics.InstanceMetrics {
	col.Collect(h.ctx, mounts)
	time.Sleep(3 * time.Second)
	_, insts := col.Collect(h.ctx, mounts)
	for _, im := range insts {
		fmt.Printf("  %s: mem_current=%d mem_max=%d cpu_percent=%.1f data_disk_used=%d\n",
			im.InstanceID, im.MemCurrent, im.MemMax, im.CPUPercent, im.DataDiskUsed)
	}
	return insts
}

// cleanup 洗掉还没洗的实例，再独立确认容器、镜像文件、loop 设备、挂载、ssh 目录都已消失。
func (h *harness) cleanup() {
	header("8 清理与最终校验")
	h.ctx = context.Background() // 被 SIGINT 取消后仍要能执行
	for _, id := range h.created {
		h.wipe(id)
	}
	for _, id := range h.created {
		c, err := h.rt.Inspect(h.ctx, id)
		img := filepath.Join(h.stateDir, "data", id+".img")
		_, serr := os.Stat(img)
		var loops []string // 镜像已删时不用 losetup -j，列出全部 loop 设备按后备文件找
		for _, l := range strings.Split(h.out("losetup", "-l", "-n", "-O", "NAME,BACK-FILE"), "\n") {
			if strings.Contains(l, img) {
				loops = append(loops, strings.TrimSpace(l))
			}
		}
		mounted, merr := h.dk.Mounted(h.ctx, id)
		_, sshErr := os.Stat(filepath.Join(h.stateDir, "ssh", id))
		h.check(err == nil && !c.Exists && errors.Is(serr, os.ErrNotExist) && len(loops) == 0 && merr == nil && !mounted && errors.Is(sshErr, os.ErrNotExist),
			id+" 已清除", "container=%v img_gone=%v loops=%q mounted=%v ssh_gone=%v", c.Exists, errors.Is(serr, os.ErrNotExist), loops, mounted, errors.Is(sshErr, os.ErrNotExist))
	}
}

type inspectDoc struct {
	HostConfig struct {
		Memory, MemorySwap, NanoCpus, ShmSize int64
		PidsLimit                             *int64
		CapAdd                                []string
		PortBindings                          map[string][]struct{ HostIp, HostPort string }
	}
	NetworkSettings struct {
		Networks map[string]struct{ IPAddress string }
	}
}

func (d inspectDoc) ipOn(network string) string { return d.NetworkSettings.Networks[network].IPAddress }

func (h *harness) inspect(id string) inspectDoc {
	var d inspectDoc
	if err := json.Unmarshal([]byte(h.out("docker", "inspect", "--type", "container", "--format", "{{json .}}", id)), &d); err != nil {
		fmt.Printf("  docker inspect %s 解析失败: %v\n", id, err)
	}
	return d
}

// out 执行命令返回 stdout；出错时打印并返回空串。
func (h *harness) out(name string, args ...string) string {
	b, err := h.run.Run(h.ctx, name, args...)
	if err != nil {
		fmt.Printf("  (%s %s: %v)\n", name, strings.Join(args, " "), err)
	}
	return string(b)
}

func (h *harness) runErr(name string, args ...string) error {
	_, err := h.run.Run(h.ctx, name, args...)
	return err
}

func (h *harness) check(ok bool, name, format string, args ...any) {
	if !ok {
		h.fail(name, format, args...)
		return
	}
	fmt.Printf("  PASS %s: %s\n", name, fmt.Sprintf(format, args...))
}

func (h *harness) fail(name, format string, args ...any) {
	h.failed++
	fmt.Printf("  FAIL %s: %s\n", name, fmt.Sprintf(format, args...))
}

func header(s string) { fmt.Printf("\n== step %s\n", s) }

func dialRetry(addr string, total time.Duration) error {
	deadline := time.Now().Add(total)
	for {
		c, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err == nil {
			return c.Close()
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// upstreamKey 生成一把内存里的 ed25519 公钥（authorized_keys 一行），代替平台网关的上游公钥。
func upstreamKey() (string, error) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	sp, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sp))), nil
}

// hostKeyPEM 生成 OpenSSH 格式的 ed25519 主机私钥（平台下发的 host_key_pem 同格式）。
func hostKeyPEM() (string, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(block)), nil
}

// localIPs 与 spark-agent 相同：本机全部非回环 IPv4。
func localIPs() []string {
	var out []string
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLoopback() {
			out = append(out, ipn.IP.String())
		}
	}
	return out
}

// defaultRouteIP：UDP「连接」不发包，只让内核选出默认路由的源地址。
func defaultRouteIP() string {
	c, err := net.Dial("udp", "8.8.8.8:53")
	if err != nil {
		return ""
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).IP.String()
}
