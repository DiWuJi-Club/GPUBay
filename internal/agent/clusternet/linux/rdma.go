package linux

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"spark-cloud/internal/agent/clusternet"
	"spark-cloud/internal/clusterspec"
)

// rdmaOf 从 sysfs 找网卡的 RDMA 设备（<SysRoot>/class/net/<iface>/device/infiniband/<hca>）与对应的 uverbsN
// （<SysRoot>/class/infiniband_verbs/uverbs*/ibdev == hca）。没有 RDMA 设备时 hca = ""。
func (n *Net) rdmaOf(iface string) (hca, uverbs string) {
	ents, err := os.ReadDir(filepath.Join(n.opt.SysRoot, "class", "net", iface, "device", "infiniband"))
	if err != nil {
		return "", ""
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		if !strings.HasPrefix(e.Name(), ".") {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return "", ""
	}
	slices.Sort(names)
	hca = names[0]
	return hca, n.uverbsOf(hca)
}

// uverbsOf 反查 hca 对应的 uverbsN。
func (n *Net) uverbsOf(hca string) string {
	ents, err := os.ReadDir(filepath.Join(n.opt.SysRoot, "class", "infiniband_verbs"))
	if err != nil {
		return ""
	}
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "uverbs") && n.ibdev(e.Name()) == hca {
			return e.Name()
		}
	}
	return ""
}

func (n *Net) ibdev(uverbs string) string {
	return readTrim(filepath.Join(n.opt.SysRoot, "class", "infiniband_verbs", uverbs, "ibdev"))
}

// Devices：RDMA 时返回 [<DevRoot>/infiniband/rdma_cm, <DevRoot>/infiniband/<uverbsN>]（按 s.Iface 的探测结果）；任一不存在 → 错误。
func (n *Net) Devices(s clusterspec.Spec) ([]string, error) {
	if !s.RDMA {
		return nil, nil
	}
	hca, uv := n.rdmaOf(s.Iface)
	if hca == "" {
		return nil, cerr("网卡 %s 没有 RDMA 设备", s.Iface)
	}
	if uv == "" {
		return nil, cerr("RDMA 设备 %s 没有对应的 uverbs 设备", hca)
	}
	devs := []string{filepath.Join(n.opt.DevRoot, "infiniband", "rdma_cm"), filepath.Join(n.opt.DevRoot, "infiniband", uv)}
	for _, d := range devs {
		if _, err := os.Stat(d); err != nil {
			return nil, cerr("RDMA 设备文件 %s 不存在", d)
		}
	}
	return devs, nil
}

// containerUverbs 列出容器内 /dev/infiniband 下的 uverbs*（<ProcRoot>/<pid>/root/dev/infiniband；docker --device 保留宿主机路径名）。
func (n *Net) containerUverbs(pid int) []string {
	ents, err := os.ReadDir(filepath.Join(n.opt.ProcRoot, strconv.Itoa(pid), "root", "dev", "infiniband"))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "uverbs") {
			out = append(out, e.Name())
		}
	}
	return out
}

// uverbsChanged：容器建时拿到的 uverbsN（容器内 /dev/infiniband 的名字）现在是否仍指向 hca。
// 宿主机重启后 uverbsN 编号可能变，docker start 会按路径重新映射到别的 HCA（规格 §3.4）。
func (n *Net) uverbsChanged(pid int, hca string) bool {
	uvs := n.containerUverbs(pid)
	if len(uvs) == 0 {
		return true
	}
	for _, uv := range uvs {
		if n.ibdev(uv) != hca {
			return true
		}
	}
	return false
}

// gidFor 是 IPv4 地址对应的 IPv4 映射 GID 在 sysfs 里的写法：0000:0000:0000:0000:0000:ffff:0ad2:030c。
func gidFor(ip string) string {
	a := netip.MustParseAddr(ip).As4()
	return fmt.Sprintf("0000:0000:0000:0000:0000:ffff:%02x%02x:%02x%02x", a[0], a[1], a[2], a[3])
}

// findGID 遍历 <SysRoot>/class/infiniband/<hca>/ports/1/gids/<i>，取等于 ::ffff:ip 且 gid_attrs/types/<i> 为 "RoCE v2" 的最小 i。
func (n *Net) findGID(hca, ip string) (int, bool) {
	port := filepath.Join(n.opt.SysRoot, "class", "infiniband", hca, "ports", "1")
	ents, err := os.ReadDir(filepath.Join(port, "gids"))
	if err != nil {
		return 0, false
	}
	want := gidFor(ip)
	var idx []int
	for _, e := range ents {
		if i, err := strconv.Atoi(e.Name()); err == nil {
			idx = append(idx, i)
		}
	}
	slices.Sort(idx)
	for _, i := range idx {
		s := strconv.Itoa(i)
		if strings.EqualFold(readTrim(filepath.Join(port, "gids", s)), want) && readTrim(filepath.Join(port, "gid_attrs", "types", s)) == "RoCE v2" {
			return i, true
		}
	}
	return 0, false
}

// waitGID 每 200ms 查一次 GID，最多等 GIDWait（GID 表在 spc0 配好地址后异步填充）。
func (n *Net) waitGID(ctx context.Context, hca, ip string) (int, bool) {
	deadline := time.Now().Add(n.opt.GIDWait)
	for {
		if i, ok := n.findGID(hca, ip); ok {
			return i, true
		}
		left := time.Until(deadline)
		if left <= 0 {
			return 0, false
		}
		t := time.NewTimer(min(gidPoll, left))
		select {
		case <-ctx.Done():
			t.Stop()
			return 0, false
		case <-t.C:
		}
	}
}

// applyRDMA：spc0 已配好之后，核对 uverbs 映射未变并查 RoCE v2 GID。任何一步不成都返回 Runtime{RDMAActive: false}
// 与包装了 clusternet.ErrNoGID 的错误（调用方据此退回 TCP、NCCL_IB_DISABLE=1，不当作失败；集群网络本身已配好）。
func (n *Net) applyRDMA(ctx context.Context, pid int, s clusterspec.Spec) (clusterspec.Runtime, error) {
	hca, _ := n.rdmaOf(s.Iface)
	if hca == "" {
		return clusterspec.Runtime{}, fmt.Errorf("%s网卡 %s 没有 RDMA 设备，已退回 TCP：%w", clusterspec.ErrPrefix, s.Iface, clusternet.ErrNoGID)
	}
	if n.uverbsChanged(pid, hca) {
		return clusterspec.Runtime{HCA: hca}, fmt.Errorf("%sRDMA 设备编号变化，需重建实例：%w", clusterspec.ErrPrefix, clusternet.ErrNoGID)
	}
	ip := s.Self().IP
	idx, ok := n.waitGID(ctx, hca, ip)
	if !ok {
		return clusterspec.Runtime{HCA: hca}, fmt.Errorf("%s未找到 RoCE GID（%s 上的 %s），已退回 TCP：%w", clusterspec.ErrPrefix, hca, ip, clusternet.ErrNoGID)
	}
	return clusterspec.Runtime{HCA: hca, GIDIndex: idx, RDMAActive: true}, nil
}

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
