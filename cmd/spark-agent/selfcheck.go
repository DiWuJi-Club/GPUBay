package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"spark-cloud/internal/agent/clusternet/linux"
	"spark-cloud/internal/agent/cmdrun"
	"spark-cloud/internal/clusterspec"
)

const (
	minFreeBytes = uint64(100) << 30
	// minCPUs：共享机 1/4 每份至少 1 核。共享机已搁置，不足只告警。
	minCPUs = 4
	// cgroupControllers 是根 cgroup 的可用控制器文件；共享机按份限额需要 cpu / memory / pids。
	cgroupControllers = "/sys/fs/cgroup/cgroup.controllers"
)

// requiredCommands 是 Agent 运行时会 exec、但上面没有单独检查的系统命令。
var requiredCommands = []string{"iptables-restore", "mkfs.ext4", "blkid", "findmnt", "mountpoint", "nsenter", "chattr", "fstrim", "ip", "truncate", "nvidia-smi"}

// selfcheck 逐项检查并打印 ok/WARN/FAIL；任一 FAIL 返回错误（安装脚本据此中止），WARN 只提示不中止。
func selfcheck(ctx context.Context, run cmdrun.Runner, stateDir string, out io.Writer) error {
	return selfcheckAt(ctx, run, stateDir, "/sys", out)
}

// selfcheckAt 同 selfcheck，sysRoot 是集群网卡探测读的 sysfs 根（测试指向夹具）。
func selfcheckAt(ctx context.Context, run cmdrun.Runner, stateDir, sysRoot string, out io.Writer) error {
	failed := false
	report := func(name string, err error, detail string) {
		if err != nil {
			failed = true
			fmt.Fprintf(out, "FAIL: %s: %v\n", name, err)
			return
		}
		if detail != "" {
			name += " " + detail
		}
		fmt.Fprintf(out, "ok: %s\n", name)
	}
	// warn：只提示、不让自检失败的项。
	warn := func(name string, err error, detail string) {
		if err != nil {
			fmt.Fprintf(out, "WARN: %s: %v\n", name, err)
			return
		}
		report(name, nil, detail)
	}
	v, err := run.Run(ctx, "docker", "version", "--format", "{{.Server.Version}}")
	report("docker", err, strings.TrimSpace(string(v)))
	detail, err := gpuSupport(ctx, run)
	report("nvidia runtime", err, detail)
	_, err = run.Run(ctx, "iptables", "-w", "-S", "DOCKER-USER")
	report("iptables DOCKER-USER", err, "")
	_, err = run.Run(ctx, "tc", "-V")
	report("tc", err, "")
	_, err = run.Run(ctx, "losetup", "-V")
	report("losetup", err, "")
	for _, name := range requiredCommands {
		_, err = run.Run(ctx, "sh", "-c", `command -v "$1"`, "sh", name)
		if err != nil {
			err = errors.New("未找到该命令")
		}
		report("command "+name, err, "")
	}
	ufw, err := run.Run(ctx, "ufw", "status")
	if err != nil {
		err = nil // 未安装即可
	} else if strings.Contains(string(ufw), "Status: active") {
		err = errors.New("ufw 已启用，会干扰 DOCKER-USER 规则，请禁用")
	}
	report("ufw", err, "")
	df, err := run.Run(ctx, "df", "-B1", "--output=avail", existingAncestor(stateDir))
	var free uint64
	if err == nil {
		lines := strings.Split(strings.TrimSpace(string(df)), "\n")
		free, err = strconv.ParseUint(strings.TrimSpace(lines[len(lines)-1]), 10, 64)
		if err == nil && free < minFreeBytes {
			err = fmt.Errorf("剩余 %d GiB，少于 100 GiB", free>>30)
		}
	}
	report("disk free", err, fmt.Sprintf("%d GiB", free>>30))
	detail, err = cpuCount(ctx, run)
	warn("nproc", err, detail) // 只和已搁置的共享机（按份 --cpus）有关，整机不需要
	detail, err = cgroupV2(ctx, run)
	report("cgroup v2", err, detail)
	clusterNICs(ctx, run, sysRoot, out)
	if failed {
		return errors.New("自检未通过")
	}
	return nil
}

// gpuSupport 判断 docker run --gpus 是否可用：docker 注册了 nvidia 运行时，或者 docker 的 CDISpecDirs
// 里有 NVIDIA 的 CDI 规格（DGX Spark 默认走 CDI：没有 nvidia 运行时，/var/run/cdi/nvidia.yaml 由 nvidia-ctk 生成）。
func gpuSupport(ctx context.Context, run cmdrun.Runner) (string, error) {
	info, err := run.Run(ctx, "docker", "info", "--format", "{{json .Runtimes}}")
	if err != nil {
		return "", err
	}
	if strings.Contains(string(info), `"nvidia"`) {
		return "", nil
	}
	raw, err := run.Run(ctx, "docker", "info", "--format", "{{json .CDISpecDirs}}")
	if err != nil {
		return "", err
	}
	var dirs []string
	if json.Unmarshal(bytes.TrimSpace(raw), &dirs) != nil {
		dirs = nil
	}
	noGPU := errors.New("docker 既没有 nvidia 运行时，CDISpecDirs 里也没有 NVIDIA 的 CDI 规格，--gpus 不可用" +
		"（安装 nvidia-container-toolkit 后执行 nvidia-ctk cdi generate --output=/var/run/cdi/nvidia.yaml）")
	if len(dirs) == 0 {
		return "", noGPU
	}
	// 目录不存在时 grep 退出码为 2，但只要输出了文件名就说明找到了，所以只看输出。
	out, _ := run.Run(ctx, "grep", append([]string{"-rls", "--include=*.yaml", "--include=*.json", "nvidia.com/gpu"}, dirs...)...)
	if files := strings.Fields(string(out)); len(files) > 0 {
		return "CDI " + files[0], nil
	}
	return "", noGPU
}

// cpuCount 检查 CPU 核数 ≥ 4（共享机按份 --cpus；调用方只告警）。
func cpuCount(ctx context.Context, run cmdrun.Runner) (string, error) {
	out, err := run.Run(ctx, "nproc")
	if err != nil {
		return "", err
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return "", fmt.Errorf("无法解析 nproc 输出 %q", strings.TrimSpace(string(out)))
	}
	if n < minCPUs {
		return "", fmt.Errorf("只有 %d 个 CPU，少于 %d", n, minCPUs)
	}
	return strconv.Itoa(n), nil
}

// cgroupV2 检查 docker 使用 cgroup v2，且 cpu / memory / pids 控制器可用（容器 --cpus / --memory / --pids-limit 与每实例指标依赖它们）。
func cgroupV2(ctx context.Context, run cmdrun.Runner) (string, error) {
	out, err := run.Run(ctx, "docker", "info", "--format", "{{.CgroupVersion}}")
	if err != nil {
		return "", err
	}
	if v := strings.TrimSpace(string(out)); v != "2" {
		return "", fmt.Errorf("docker 的 Cgroup Version 为 %q，需要 2", v)
	}
	raw, err := run.Run(ctx, "cat", cgroupControllers)
	if err != nil {
		return "", fmt.Errorf("读取 %s 失败：%w", cgroupControllers, err)
	}
	have := strings.Fields(string(raw))
	var missing []string
	for _, c := range []string{"cpu", "memory", "pids"} {
		if !slices.Contains(have, c) {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		return "", fmt.Errorf("cgroup 缺少控制器 %s", strings.Join(missing, " "))
	}
	return "cpu memory pids", nil
}

// existingAncestor 返回 dir 本身或其最近的已存在上级目录：首次安装时状态目录还没建，df 要查它将来所在的分区。
func existingAncestor(dir string) string {
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		if _, err := os.Stat(d); err == nil || d == filepath.Dir(d) {
			return d
		}
	}
}

// agentOwnedPrefixes：Agent 自己为集群建的宿主机网卡（veth sch*、macvlan scm*、VLAN 子接口 scv*、临时名 scc*），
// 它们的地址与路由本来就在集群网段里，不算冲突。
var agentOwnedPrefixes = []string{"sch", "scm", "scv", "scc"}

func agentOwned(dev string) bool {
	for _, p := range agentOwnedPrefixes {
		if strings.HasPrefix(dev, p) {
			return true
		}
	}
	return false
}

// clusterNICs 是「集群网卡」一节（计划 13）：列出物理网卡与 RDMA 状态，提示 CX-7 口的地址 / NetworkManager 托管、
// ip_forward、集群网段冲突。集群是可选能力，这里只打印 ok / WARN，从不让自检失败。
func clusterNICs(ctx context.Context, run cmdrun.Runner, sysRoot string, out io.Writer) {
	ok := func(format string, a ...any) { fmt.Fprintf(out, "ok: "+format+"\n", a...) }
	warn := func(format string, a ...any) { fmt.Fprintf(out, "WARN: "+format+"\n", a...) }
	routes, routeErr := run.Run(ctx, "ip", "-4", "route", "show")
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	info, err := linux.New(run, linux.Options{SysRoot: sysRoot, Log: quiet}).Probe(ctx, "")
	if err != nil {
		warn("集群网卡: %v", err)
	} else {
		for _, f := range info.Ifaces {
			rdma := f.RDMADevice
			if rdma == "" {
				rdma = "-"
			}
			ok("iface %s up=%t speed=%d driver=%s rdma=%s active=%t", f.Name, f.Up, f.SpeedMbps, f.Driver, rdma, f.RDMAActive)
		}
		cx7NICs(ctx, run, info, defaultDevs(string(routes)), ok, warn)
	}

	raw, err := run.Run(ctx, "cat", "/proc/sys/net/ipv4/ip_forward")
	switch v := strings.TrimSpace(string(raw)); {
	case err != nil:
		warn("ip_forward: 读取 /proc/sys/net/ipv4/ip_forward 失败：%v", err)
	case v != "1":
		warn("ip_forward: /proc/sys/net/ipv4/ip_forward 为 %s，lan 集群（routed）需要 1：sysctl -w net.ipv4.ip_forward=1（Docker 启动时通常会打开）", v)
	default:
		ok("ip_forward 1")
	}

	addrs, addrErr := run.Run(ctx, "ip", "-4", "-o", "addr", "show")
	if addrErr != nil || routeErr != nil {
		warn("集群网段 %s: 读取本机地址 / 路由失败：%v", clusterspec.DefaultPool, errors.Join(addrErr, routeErr))
		return
	}
	if hits := poolConflicts(string(addrs), string(routes)); len(hits) > 0 {
		warn("集群网段 %s: 与集群网段冲突：%s", clusterspec.DefaultPool, strings.Join(hits, "；"))
	} else {
		ok("集群网段 %s 无冲突", clusterspec.DefaultPool)
	}
}

// cx7NICs 检查 ConnectX-7 口（mlx5_core）：有地址（IPv4 / IPv6）→ 提示 Agent 会清除；受 NetworkManager 管理 → 提示
// install.sh --cluster-iface。持有默认路由的口是管理网卡，Agent 绝不改动它，这里也不提示。
func cx7NICs(ctx context.Context, run cmdrun.Runner, info clusterspec.NetInfo, defaults map[string]bool, ok, warn func(string, ...any)) {
	v6 := map[string][]string{}
	if raw, err := run.Run(ctx, "ip", "-6", "-o", "addr", "show"); err == nil {
		v6 = parseAddrLines(string(raw), "inet6")
	}
	nm, nmErr := nmDeviceStates(ctx, run)
	n := 0
	for _, f := range info.Ifaces {
		if f.Driver != clusterspec.MLX5Driver {
			continue
		}
		n++
		if defaults[f.Name] {
			ok("%s 持有默认路由，视为管理网卡，Agent 不会把它当集群网卡改动", f.Name)
			continue
		}
		if addrs := append(slices.Clone(f.IPv4), v6[f.Name]...); len(addrs) > 0 {
			warn("%s 上有地址 %s，作为集群网卡时 Agent 会清除", f.Name, strings.Join(addrs, " "))
		}
		if st, found := nm[f.Name]; nmErr == nil && found && st != "unmanaged" {
			warn("%s 受 NetworkManager 管理，NM 可能重新加地址；请执行 install.sh --cluster-iface %s", f.Name, f.Name)
		}
	}
	switch {
	case n == 0:
		warn("未发现 ConnectX-7 网卡，集群将只能使用普通以太网")
	case nmErr != nil:
		ok("NetworkManager 未运行，跳过集群网卡受管检查")
	}
}

// nmDeviceStates 解析 `nmcli -t -f DEVICE,STATE device`（每行 "enp1s0f1np1:unmanaged"）；没有 nmcli / NM 未运行 → 错误。
func nmDeviceStates(ctx context.Context, run cmdrun.Runner) (map[string]string, error) {
	raw, err := run.Run(ctx, "nmcli", "-t", "-f", "DEVICE,STATE", "device")
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		if dev, st, found := strings.Cut(strings.TrimSpace(line), ":"); found && dev != "" {
			m[dev] = st
		}
	}
	return m, nil
}

// parseAddrLines 解析 `ip -4|-6 -o addr show`：网卡名 → CIDR 列表（family 为 "inet" 或 "inet6"）。
func parseAddrLines(out, family string) map[string][]string {
	m := map[string][]string{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || f[2] != family {
			continue
		}
		name, _, _ := strings.Cut(f[1], "@")
		m[name] = append(m[name], f[3])
	}
	return m
}

// defaultDevs 是 `ip -4 route show` 里持有默认路由的网卡。
func defaultDevs(routes string) map[string]bool {
	m := map[string]bool{}
	for _, line := range strings.Split(routes, "\n") {
		if f := strings.Fields(line); len(f) > 0 && f[0] == "default" {
			if dev := fieldAfter(f, "dev"); dev != "" {
				m[dev] = true
			}
		}
	}
	return m
}

func fieldAfter(f []string, key string) string {
	if i := slices.Index(f, key); i >= 0 && i+1 < len(f) {
		return f[i+1]
	}
	return ""
}

// poolConflicts 找落在集群网段（clusterspec.DefaultPool）里的本机地址与路由（Agent 自建的网卡除外）。
// 包住整个网段的更大路由（如 10.0.0.0/8）不算：集群的 /32 路由更具体。与地址重复的直连路由（proto kernel）只报地址。
func poolConflicts(addrs, routes string) []string {
	pool := netip.MustParsePrefix(clusterspec.DefaultPool)
	var hits []string
	names := parseAddrLines(addrs, "inet")
	for _, dev := range slices.Sorted(maps.Keys(names)) {
		if agentOwned(dev) {
			continue
		}
		for _, c := range names[dev] {
			if p, err := netip.ParsePrefix(c); err == nil && pool.Contains(p.Addr()) {
				hits = append(hits, fmt.Sprintf("地址 %s（%s）", c, dev))
			}
		}
	}
	for _, line := range strings.Split(routes, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || f[0] == "default" || slices.Contains(f, "kernel") {
			continue
		}
		dev := fieldAfter(f, "dev")
		if agentOwned(dev) {
			continue
		}
		dst := f[0]
		if !strings.Contains(dst, "/") {
			dst += "/32"
		}
		if p, err := netip.ParsePrefix(dst); err == nil && p.Overlaps(pool) && p.Bits() >= pool.Bits() {
			hits = append(hits, fmt.Sprintf("路由 %s（%s）", f[0], dev))
		}
	}
	return hits
}
