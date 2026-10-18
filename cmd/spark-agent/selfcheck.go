package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"spark-cloud/internal/agent/cmdrun"
)

const minFreeBytes = uint64(100) << 30

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
	info, err := run.Run(ctx, "docker", "info", "--format", "{{json .Runtimes}}")
	if err == nil && !strings.Contains(string(info), `"nvidia"`) {
		err = errors.New("docker 没有 nvidia 运行时，--gpus 不可用（安装 nvidia-container-toolkit 并 nvidia-ctk runtime configure --runtime=docker）")
	}
	report("nvidia runtime", err, "")
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
	if failed {
		return errors.New("自检未通过")
	}
	return nil
}

// existingAncestor 返回 dir 本身或其最近的已存在上级目录：首次安装时状态目录还没建，df 要查它将来所在的分区。
func existingAncestor(dir string) string {
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		if _, err := os.Stat(d); err == nil || d == filepath.Dir(d) {
			return d
		}
	}
}
