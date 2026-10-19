// Package metrics 采集心跳指标：nvidia-smi（GB10 显存为 N/A，不采）、/proc/meminfo、df、/proc/loadavg。
package metrics

import (
	"context"
	"math"
	"strconv"
	"strings"

	"spark-cloud/internal/agent/cmdrun"
)

type Metrics struct {
	GPUUtil       float64
	GPUTempC      float64
	GPUPowerW     float64
	MemTotal      uint64
	MemUsed       uint64
	RootDiskTotal uint64
	RootDiskUsed  uint64
	DataDiskUsed  uint64
	Load1         float64
}

type Collector struct {
	Run      cmdrun.Runner
	ReadFile func(string) ([]byte, error)
}

// Collect 永不失败：采不到的项为 0。
func (c *Collector) Collect(ctx context.Context, dataMount string) Metrics {
	var m Metrics
	if out, err := c.Run.Run(ctx, "nvidia-smi", "--query-gpu=utilization.gpu,temperature.gpu,power.draw", "--format=csv,noheader,nounits"); err == nil {
		m.GPUUtil, m.GPUTempC, m.GPUPowerW = ParseNvidiaSMI(string(out))
	}
	if b, err := c.ReadFile("/proc/meminfo"); err == nil {
		m.MemTotal, m.MemUsed = ParseMeminfo(b)
	}
	if out, err := c.Run.Run(ctx, "df", "-B1", "--output=size,used", "/"); err == nil {
		m.RootDiskTotal, m.RootDiskUsed = ParseDF(string(out))
	}
	if dataMount != "" {
		if out, err := c.Run.Run(ctx, "df", "-B1", "--output=size,used", dataMount); err == nil {
			_, m.DataDiskUsed = ParseDF(string(out))
		}
	}
	if b, err := c.ReadFile("/proc/loadavg"); err == nil {
		m.Load1 = ParseLoadavg(b)
	}
	return m
}

func num(s string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0 // "[N/A]"、NaN、Inf 等
	}
	return v
}

// ParseNvidiaSMI 解析 "12, 45, 30.50" 形式的一行；"[N/A]" 视为 0。
func ParseNvidiaSMI(out string) (util, temp, power float64) {
	line := strings.TrimSpace(strings.SplitN(out, "\n", 2)[0])
	parts := strings.Split(line, ",")
	if len(parts) < 3 {
		return 0, 0, 0
	}
	return num(parts[0]), num(parts[1]), num(parts[2])
}

// ParseMeminfo 返回 MemTotal 与 MemTotal - MemAvailable（字节）。
func ParseMeminfo(b []byte) (total, used uint64) {
	var avail uint64
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		v, _ := strconv.ParseUint(f[1], 10, 64)
		switch f[0] {
		case "MemTotal:":
			total = v * 1024
		case "MemAvailable:":
			avail = v * 1024
		}
	}
	if total >= avail {
		used = total - avail
	}
	return total, used
}

// ParseDF 解析 `df -B1 --output=size,used` 的第二行。
func ParseDF(out string) (total, used uint64) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		return 0, 0
	}
	f := strings.Fields(lines[len(lines)-1])
	if len(f) < 2 {
		return 0, 0
	}
	total, _ = strconv.ParseUint(f[0], 10, 64)
	used, _ = strconv.ParseUint(f[1], 10, 64)
	return total, used
}

func ParseLoadavg(b []byte) float64 {
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0
	}
	return num(f[0])
}
