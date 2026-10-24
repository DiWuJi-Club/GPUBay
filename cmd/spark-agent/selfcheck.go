package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"spark-cloud/internal/agent/cmdrun"
)

const (
	minFreeBytes = uint64(100) << 30
	// minCPUs：共享机 1/4 每份至少 1 核。
	minCPUs = 4
	// cgroupControllers 是根 cgroup 的可用控制器文件；共享机按份限额需要 cpu / memory / pids。
	cgroupControllers = "/sys/fs/cgroup/cgroup.controllers"
)

// requiredCommands 是 Agent 运行时会 exec、但上面没有单独检查的系统命令。
var requiredCommands = []string{"iptables-restore", "mkfs.ext4", "blkid", "findmnt", "mountpoint", "nsenter", "chattr", "fstrim", "ip", "truncate", "nvidia-smi"}

// selfcheck 逐项检查并打印 ok/FAIL；任一失败返回错误（安装脚本据此中止）。
func selfcheck(ctx context.Context, run cmdrun.Runner, stateDir string, out io.Writer) error {
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
	report("nproc", err, detail)
	detail, err = cgroupV2(ctx, run)
	report("cgroup v2", err, detail)
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

// cpuCount 检查 CPU 核数 ≥ 4（共享机按份 --cpus）。
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
