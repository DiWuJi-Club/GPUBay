package clusterspec

import "strconv"

// 宿主机侧网卡名（Linux 上限 15 字符）：前缀 + 实例 ID 后 8 位。

// HostVeth 是 routed 模式宿主机侧 veth：sch + 实例 ID 后 8 位。
func HostVeth(instanceID string) string { return "sch" + tail8(instanceID) }

// TempName 是 veth 容器侧在改名 spc0 之前的临时名：scc + 后 8 位。
func TempName(instanceID string) string { return "scc" + tail8(instanceID) }

// MacvlanName 是 macvlan 子接口在移入容器之前的名字：scm + 后 8 位。
func MacvlanName(instanceID string) string { return "scm" + tail8(instanceID) }

// VLANDev 是 vlan fabric 的 VLAN 子接口：scv + vlan（不用 <iface>.<vlan>，CX-7 网卡名加后缀会超过 15）。
func VLANDev(vlan int) string { return "scv" + strconv.Itoa(vlan) }

func tail8(id string) string {
	if len(id) > 8 {
		return id[len(id)-8:]
	}
	return id
}
