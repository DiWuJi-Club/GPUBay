package linux

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"spark-cloud/internal/clusterspec"
)

// excludedPrefixes：不是物理网卡（或是 Agent / Docker 自己建的）的网卡名前缀；spark0 精确匹配。
var excludedPrefixes = []string{"lo", "docker", "br-", "veth", "sch", "scm", "scv", "scc"}

func excluded(name string) bool {
	if name == bridge {
		return true
	}
	for _, p := range excludedPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// Probe（规格 §3.2）：只读 sysfs（<SysRoot>/class/net/*）+ `ip -4 -o addr show` + `ip -4 route show default`。
// 物理网卡 = 名字不在排除表里，且 <if>/device 存在或持有 IPv4 默认路由。IPv4 为 CIDR。
// lanIP 为空（AgentConfig 尚未到达）时 LanIface = ""，其余照常；非空时记下，供 macvlan 拒绝管理网卡。
func (n *Net) Probe(ctx context.Context, lanIP string) (clusterspec.NetInfo, error) {
	if lanIP != "" {
		n.mu.Lock()
		n.lanIP = lanIP
		n.mu.Unlock()
	}
	addrOut, err := n.ip(ctx, "-4", "-o", "addr", "show")
	if err != nil {
		return clusterspec.NetInfo{}, cerr("%w", err)
	}
	addrs := parseAddrs(string(addrOut))
	defaults := map[string]bool{}
	if out, err := n.ip(ctx, "-4", "route", "show", "default"); err == nil {
		for _, d := range routeDevs(string(out)) {
			defaults[d] = true
		}
	}
	netDir := filepath.Join(n.opt.SysRoot, "class", "net")
	ents, err := os.ReadDir(netDir)
	if err != nil {
		return clusterspec.NetInfo{}, cerr("读取 %s 失败：%v", netDir, err)
	}
	info := clusterspec.NetInfo{Ifaces: []clusterspec.Iface{}}
	for _, e := range ents {
		name := e.Name()
		dir := filepath.Join(netDir, name)
		if excluded(name) {
			continue
		}
		_, devErr := os.Stat(filepath.Join(dir, "device"))
		if devErr != nil && !defaults[name] {
			continue
		}
		f := clusterspec.Iface{
			Name:      name,
			MAC:       readTrim(filepath.Join(dir, "address")),
			Up:        readTrim(filepath.Join(dir, "operstate")) == "up" && readTrim(filepath.Join(dir, "carrier")) == "1",
			SpeedMbps: max(atoi(readTrim(filepath.Join(dir, "speed"))), 0),
			MTU:       atoi(readTrim(filepath.Join(dir, "mtu"))),
			IPv4:      addrs[name],
			Wireless:  exists(filepath.Join(dir, "wireless")) || exists(filepath.Join(dir, "phy80211")),
		}
		if devErr == nil {
			f.PCI = linkBase(filepath.Join(dir, "device"))
			f.Driver = linkBase(filepath.Join(dir, "device", "driver"))
			if hca, uv := n.rdmaOf(name); hca != "" {
				port := filepath.Join(n.opt.SysRoot, "class", "infiniband", hca, "ports", "1")
				f.RDMADevice, f.Uverbs = hca, uv
				f.LinkLayer = readTrim(filepath.Join(port, "link_layer"))
				f.RDMAActive = readTrim(filepath.Join(port, "state")) == "4: ACTIVE" && f.LinkLayer == "Ethernet"
			}
		}
		if lanIP != "" && info.LanIface == "" {
			for _, c := range f.IPv4 {
				if ip, _, _ := strings.Cut(c, "/"); ip == lanIP {
					info.LanIface = name
				}
			}
		}
		info.Ifaces = append(info.Ifaces, f)
	}
	slices.SortFunc(info.Ifaces, func(a, b clusterspec.Iface) int { return strings.Compare(a.Name, b.Name) })
	return info, nil
}

// parseAddrs 解析 `ip -4 -o addr show`：每行 "7: wlP9s9    inet 192.168.1.221/23 brd … scope global … wlP9s9\ …"。
func parseAddrs(out string) map[string][]string {
	m := map[string][]string{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || f[2] != "inet" {
			continue
		}
		name, _, _ := strings.Cut(f[1], "@")
		m[name] = append(m[name], f[3])
	}
	return m
}

func atoi(s string) int {
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return v
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// linkBase 是符号链接目标的最后一段（device → PCI 地址，device/driver → 驱动名）。
func linkBase(p string) string {
	t, err := os.Readlink(p)
	if err != nil {
		return ""
	}
	return filepath.Base(t)
}
