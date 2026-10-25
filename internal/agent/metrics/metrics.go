// Package metrics 采集心跳指标：nvidia-smi（GB10 显存为 N/A，不采）、/proc/meminfo、df、/proc/loadavg，
// 以及每个实例容器的 cgroup v2 用量（memory.current / memory.max / cpu.stat）。
package metrics

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

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

// InstanceMount 是一个实例的采集位置（由 executor.Mounts 给出）。
type InstanceMount struct {
	ID        string
	DataDir   string // 数据盘挂载目录（df 取已用）；空 = 不采
	CgroupDir string // 容器 cgroup v2 目录（/sys/fs/cgroup/system.slice/docker-<id>.scope）；空 = 未知，内存 / CPU 为 0
}

// InstanceMetrics 是心跳里一个实例的用量。
type InstanceMetrics struct {
	InstanceID   string
	MemCurrent   uint64  // memory.current（字节）
	MemMax       uint64  // memory.max（字节；字面量 max = 不限，记 0）
	CPUPercent   float64 // 两次采样之间的 CPU 使用率（100 = 一个核满载）；首个样本为 0
	DataDiskUsed uint64
}

type cpuSample struct {
	dir   string // 采样时的 cgroup 目录：容器重建后目录变化，旧样本作废
	usage uint64 // cpu.stat usage_usec
	at    time.Time
}

type Collector struct {
	Run      cmdrun.Runner
	ReadFile func(string) ([]byte, error)
	Now      func() time.Time // nil → time.Now；测试注入
	// IsMountPoint 判断数据目录是否为挂载点；nil → 包级 IsMountPoint。数据盘没挂上时目录只是根分区里的普通目录，
	// df 会报根分区用量，所以非挂载点记 0（也不跑 df）。
	IsMountPoint func(dir string) bool

	mu   sync.Mutex
	prev map[string]cpuSample // 实例 ID → 上一次 CPU 样本；不在本次 mounts 里的实例被丢弃
}

func (c *Collector) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Collector) isMountPoint(dir string) bool {
	if c.IsMountPoint != nil {
		return c.IsMountPoint(dir)
	}
	return IsMountPoint(dir)
}

// IsMountPoint：dir 与其上级目录不在同一设备上即为挂载点（数据盘是 loop 设备上的 ext4，设备号与宿主机根分区不同）。
// 只做两次 stat，比 exec findmnt / mountpoint 便宜；目录不存在或 stat 失败按非挂载点处理。
func IsMountPoint(dir string) bool {
	dir = filepath.Clean(dir)
	parent := filepath.Dir(dir)
	if parent == dir {
		return false
	}
	a, err := os.Stat(dir)
	if err != nil || !a.IsDir() {
		return false
	}
	b, err := os.Stat(parent)
	if err != nil {
		return false
	}
	sa, ok1 := a.Sys().(*syscall.Stat_t)
	sb, ok2 := b.Sys().(*syscall.Stat_t)
	return ok1 && ok2 && sa.Dev != sb.Dev
}

// Collect 永不失败：采不到的项为 0（数据目录不是挂载点时该实例数据盘已用也为 0）。机器级 DataDiskUsed 为各实例数据盘已用之和（语义与单实例时一致）。
func (c *Collector) Collect(ctx context.Context, mounts []InstanceMount) (Metrics, []InstanceMetrics) {
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
	insts := make([]InstanceMetrics, 0, len(mounts))
	for _, mt := range mounts {
		im := InstanceMetrics{InstanceID: mt.ID}
		if mt.DataDir != "" && c.isMountPoint(mt.DataDir) {
			if out, err := c.Run.Run(ctx, "df", "-B1", "--output=size,used", mt.DataDir); err == nil {
				_, im.DataDiskUsed = ParseDF(string(out))
			}
		}
		m.DataDiskUsed += im.DataDiskUsed
		insts = append(insts, im)
	}
	if b, err := c.ReadFile("/proc/loadavg"); err == nil {
		m.Load1 = ParseLoadavg(b)
	}
	c.cgroups(mounts, insts)
	return m, insts
}

// cgroups 填每个实例的内存与 CPU；读不到的文件对应项为 0。
func (c *Collector) cgroups(mounts []InstanceMount, insts []InstanceMetrics) {
	c.mu.Lock()
	defer c.mu.Unlock()
	next := make(map[string]cpuSample, len(mounts))
	for i, mt := range mounts {
		if mt.CgroupDir == "" {
			continue
		}
		im := &insts[i]
		if b, err := c.ReadFile(filepath.Join(mt.CgroupDir, "memory.current")); err == nil {
			im.MemCurrent = ParseCgroupValue(b)
		}
		if b, err := c.ReadFile(filepath.Join(mt.CgroupDir, "memory.max")); err == nil {
			im.MemMax = ParseCgroupValue(b)
		}
		b, err := c.ReadFile(filepath.Join(mt.CgroupDir, "cpu.stat"))
		if err != nil {
			continue
		}
		usage, ok := ParseCPUStatUsage(b)
		if !ok {
			continue
		}
		cur := cpuSample{dir: mt.CgroupDir, usage: usage, at: c.now()}
		if p, ok := c.prev[mt.ID]; ok && p.dir == cur.dir && usage >= p.usage {
			if wall := cur.at.Sub(p.at).Microseconds(); wall > 0 {
				im.CPUPercent = float64(usage-p.usage) / float64(wall) * 100
			}
		}
		next[mt.ID] = cur
	}
	c.prev = next
}

// ParseCgroupValue 解析 memory.current / memory.max 这类单值文件；字面量 max（不限）与无法解析的内容为 0。
func ParseCgroupValue(b []byte) uint64 {
	v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// ParseCPUStatUsage 取 cpu.stat 的 usage_usec（微秒）。
func ParseCPUStatUsage(b []byte) (uint64, bool) {
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == "usage_usec" {
			v, err := strconv.ParseUint(f[1], 10, 64)
			return v, err == nil
		}
	}
	return 0, false
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
