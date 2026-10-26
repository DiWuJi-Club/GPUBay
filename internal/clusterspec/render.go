package clusterspec

import (
	"bytes"
	"encoding/json"
	"net/netip"
	"strconv"
	"strings"
)

// 容器内集群信息（规格 §3.6）：Agent 写 ssh/<id>/cluster.json 与 cluster.env（容器内 /.spark/），
// init.sh 把前者软链到 /etc/spark-cluster.json、后者在登录 shell 里导入（每行 export）。
// 两份文件都不含宿主机 IP（members[].host_ip 只给 Agent 配路由用）。

const (
	NetworkRDMA     = "rdma"
	NetworkEthernet = "ethernet"
)

// Network 返回实际网络类型：spec 要 RDMA 且运行时 RDMA 可用（找到 GID）才是 rdma。
func Network(s Spec, rt Runtime) string {
	if s.RDMA && rt.RDMAActive {
		return NetworkRDMA
	}
	return NetworkEthernet
}

// headIP：rank 0 在位取其 IP；缺位时按子网推算（换机后的新 head 保留同一 IP）。
func headIP(s Spec) string {
	if h, ok := s.Head(); ok {
		return h.IP
	}
	if sub, err := netip.ParsePrefix(s.Subnet); err == nil && sub.Addr().Is4() {
		return MemberIP(sub, 0).String()
	}
	return ""
}

type jsonMember struct {
	Rank       int    `json:"rank"`
	InstanceID string `json:"instance_id"`
	IP         string `json:"ip"`
}

type jsonRDMA struct {
	HCA      string `json:"hca"`
	GIDIndex int    `json:"gid_index"`
}

type jsonDoc struct {
	ClusterID string       `json:"cluster_id"`
	Version   int          `json:"version"`
	Size      int          `json:"size"`
	Rank      int          `json:"rank"`
	IfName    string       `json:"ifname"`
	Subnet    string       `json:"subnet"`
	IP        string       `json:"ip"`
	HeadIP    string       `json:"head_ip"`
	Network   string       `json:"network"`
	Members   []jsonMember `json:"members"`
	RDMA      *jsonRDMA    `json:"rdma"`
}

// RenderJSON 渲染 /etc/spark-cluster.json（两空格缩进，末尾换行）。
func RenderJSON(s Spec, rt Runtime) []byte {
	d := jsonDoc{
		ClusterID: s.ClusterID, Version: s.Version, Size: s.Size, Rank: s.Rank, IfName: IfName,
		Subnet: s.Subnet, IP: s.Self().IP, HeadIP: headIP(s), Network: Network(s, rt),
		Members: make([]jsonMember, 0, len(s.Members)),
	}
	for _, m := range s.Members {
		d.Members = append(d.Members, jsonMember{Rank: m.Rank, InstanceID: m.InstanceID, IP: m.IP})
	}
	if d.Network == NetworkRDMA {
		d.RDMA = &jsonRDMA{HCA: rt.HCA, GIDIndex: rt.GIDIndex}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(d) // 只含字符串 / 数字，不会失败；Encode 自带末尾换行
	return buf.Bytes()
}

// RenderEnv 渲染 /.spark/cluster.env：固定顺序的 export KEY='value' 行，末尾换行；
// 每行都 export，任何 `. /.spark/cluster.env` 都把变量导出给子进程（ray start、vllm serve、torchrun）。
// 值里的单引号按 POSIX sh 惯例转义：先结束引号，写反斜杠加单引号，再重新开引号。
func RenderEnv(s Spec, rt Runtime) []byte {
	ips := make([]string, 0, len(s.Members))
	for _, m := range s.Members {
		ips = append(ips, m.IP)
	}
	self, head, network := s.Self().IP, headIP(s), Network(s, rt)
	kv := [][2]string{
		{"SPARK_CLUSTER_ID", s.ClusterID},
		{"SPARK_CLUSTER_SIZE", strconv.Itoa(s.Size)},
		{"SPARK_CLUSTER_RANK", strconv.Itoa(s.Rank)},
		{"SPARK_CLUSTER_IP", self},
		{"SPARK_CLUSTER_HEAD_IP", head},
		{"SPARK_CLUSTER_IPS", strings.Join(ips, ",")},
		{"SPARK_CLUSTER_IFNAME", IfName},
		{"SPARK_CLUSTER_NETWORK", network},
		{"NCCL_SOCKET_IFNAME", IfName},
		{"GLOO_SOCKET_IFNAME", IfName},
		{"TP_SOCKET_IFNAME", IfName},
		{"VLLM_HOST_IP", self},
		{"MASTER_ADDR", head},
		{"MASTER_PORT", strconv.Itoa(MasterPort)},
		{"NNODES", strconv.Itoa(s.Size)},
		{"NODE_RANK", strconv.Itoa(s.Rank)},
	}
	if network == NetworkRDMA {
		kv = append(kv,
			[2]string{"NCCL_IB_HCA", rt.HCA},
			[2]string{"NCCL_IB_GID_INDEX", strconv.Itoa(rt.GIDIndex)},
			[2]string{"NCCL_IB_DISABLE", "0"})
	} else {
		kv = append(kv, [2]string{"NCCL_IB_DISABLE", "1"})
	}
	var b strings.Builder
	for _, p := range kv {
		b.WriteString("export ")
		b.WriteString(p[0])
		b.WriteString("='")
		b.WriteString(strings.ReplaceAll(p[1], "'", `'\''`))
		b.WriteString("'\n")
	}
	return []byte(b.String())
}
