// Package docker 通过 exec docker CLI 实现 runtime.Runtime（不引入 Docker SDK）。
package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"spark-cloud/internal/agent/cmdrun"
	"spark-cloud/internal/agent/runtime"
	"spark-cloud/internal/platform/imageref"
)

type Runtime struct {
	Run    cmdrun.Runner
	Bridge string // spark0
	Subnet string // 10.200.0.0/24
}

func isNotFound(err error) bool {
	var ce *cmdrun.CmdError
	if !errors.As(err, &ce) {
		return false
	}
	s := strings.ToLower(ce.Stderr)
	return strings.Contains(s, "no such container") || strings.Contains(s, "no such object") || strings.Contains(s, "no such network") ||
		(strings.Contains(s, "network") && strings.Contains(s, "not found")) // docker 29: "network X not found"
}

func (r *Runtime) EnsureNetwork(ctx context.Context) error {
	_, err := r.Run.Run(ctx, "docker", "network", "inspect", "--format", "{{.Id}}", r.Bridge)
	if err == nil {
		return nil
	}
	if !isNotFound(err) {
		return err
	}
	_, err = r.Run.Run(ctx, "docker", "network", "create", "--driver", "bridge", "--subnet", r.Subnet, "--ipv6=false",
		"-o", "com.docker.network.bridge.name="+r.Bridge, "-o", "com.docker.network.bridge.enable_icc=false",
		"-o", "com.docker.network.bridge.enable_ip_masquerade=true", r.Bridge)
	return err
}

// Images 列出本机已缓存镜像（StateReport.images）；规范化规则与网关、覆盖率统计共用 imageref。
func (r *Runtime) Images(ctx context.Context) ([]string, error) {
	out, err := r.Run.Run(ctx, "docker", "image", "ls", "--format", "{{.Repository}}:{{.Tag}}", "--filter", "dangling=false")
	if err != nil {
		return nil, err
	}
	return imageref.Normalize(strings.Split(string(out), "\n")), nil
}

func (r *Runtime) Pull(ctx context.Context, ref string) error {
	_, err := r.Run.Run(ctx, "docker", "pull", ref)
	return err
}

// Create 只创建容器，不启动；容器规格见设计 §4.2。
func (r *Runtime) Create(ctx context.Context, s runtime.Spec) error {
	if err := validateSpec(s); err != nil {
		return err
	}
	args := []string{"create", "--name", s.InstanceID, "--label", runtime.Label + "=" + s.InstanceID, "--gpus", "all",
		"--shm-size", strconv.FormatInt(s.ShmBytes, 10), "--memory", strconv.FormatInt(s.MemoryBytes, 10), "--memory-swap", strconv.FormatInt(s.MemoryBytes, 10)}
	if s.CPUs > 0 { // 共享机按份限 CPU；整机不传，命令行与计划 2 一致
		args = append(args, "--cpus", strconv.FormatFloat(s.CPUs, 'f', 2, 64))
	}
	args = append(args, "--pids-limit", strconv.Itoa(s.PidsLimit), "--oom-score-adj", "500", "--cap-add", "IPC_LOCK")
	if !s.DropNice {
		args = append(args, "--cap-add", "SYS_NICE")
	}
	args = append(args, "--cap-add", "SYS_PTRACE",
		"--ulimit", "memlock=-1:-1", "--ulimit", "stack=67108864:67108864",
		"--network", r.Bridge)
	for _, d := range s.DNS {
		args = append(args, "--dns", d)
	}
	args = append(args, "--dns-search", ".", "--restart", "no",
		"-p", fmt.Sprintf("%s:%d:22/tcp", s.LanIP, s.SSHPort),
		"-v", s.SSHDir+":/.spark:ro", "-v", s.DataDir+":/root/data",
		"--entrypoint", "/.spark/init.sh", s.ImageRef)
	_, err := r.Run.Run(ctx, "docker", args...)
	return err
}

func (r *Runtime) Start(ctx context.Context, id string) error {
	_, err := r.Run.Run(ctx, "docker", "start", id)
	return err
}

func (r *Runtime) Stop(ctx context.Context, id string) error {
	_, err := r.Run.Run(ctx, "docker", "stop", "-t", "30", id)
	if isNotFound(err) {
		return nil
	}
	return err
}

func (r *Runtime) Remove(ctx context.Context, id string) error {
	_, err := r.Run.Run(ctx, "docker", "rm", "-f", "-v", id)
	if isNotFound(err) {
		return nil
	}
	return err
}

type inspectDoc struct {
	ID    string `json:"Id"`
	State struct {
		Running bool `json:"Running"`
		Pid     int  `json:"Pid"`
	} `json:"State"`
	Config struct {
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	HostConfig struct {
		PortBindings map[string][]struct {
			HostIp string `json:"HostIp"`
		} `json:"PortBindings"`
	} `json:"HostConfig"`
	NetworkSettings struct {
		Networks map[string]json.RawMessage `json:"Networks"`
	} `json:"NetworkSettings"`
	Mounts []struct {
		Destination string `json:"Destination"`
		Source      string `json:"Source"`
	} `json:"Mounts"`
}

func (r *Runtime) Inspect(ctx context.Context, id string) (runtime.Container, error) {
	out, err := r.Run.Run(ctx, "docker", "inspect", "--type", "container", "--format", "{{json .}}", id)
	if isNotFound(err) {
		return runtime.Container{}, nil
	}
	if err != nil {
		return runtime.Container{}, err
	}
	var d inspectDoc
	if err := json.Unmarshal(out, &d); err != nil {
		return runtime.Container{}, fmt.Errorf("parse docker inspect: %w", err)
	}
	c := runtime.Container{Exists: true, FullID: d.ID, Running: d.State.Running, Image: d.Config.Image, Label: d.Config.Labels[runtime.Label], Pid: d.State.Pid}
	for _, m := range d.Mounts {
		if m.Destination == "/root/data" {
			c.DataMounted = true
			c.DataSource = m.Source
		}
	}
	for name := range d.NetworkSettings.Networks {
		c.Networks = append(c.Networks, name)
	}
	sort.Strings(c.Networks)
	for _, bs := range d.HostConfig.PortBindings {
		for _, b := range bs {
			if b.HostIp != "" && !slices.Contains(c.PublishIPs, b.HostIp) {
				c.PublishIPs = append(c.PublishIPs, b.HostIp)
			}
		}
	}
	sort.Strings(c.PublishIPs)
	return c, nil
}

// ConnectNetwork：启动失败（如发布地址不存在）后 Docker 会丢掉容器的网络挂接，需手动接回。
func (r *Runtime) ConnectNetwork(ctx context.Context, id, network string) error {
	_, err := r.Run.Run(ctx, "docker", "network", "connect", network, id)
	return err
}

type psDoc struct {
	Names  string `json:"Names"`
	State  string `json:"State"`
	Labels string `json:"Labels"`
	Image  string `json:"Image"`
}

func (r *Runtime) List(ctx context.Context) ([]runtime.Container, error) {
	out, err := r.Run.Run(ctx, "docker", "ps", "-a", "--filter", "label="+runtime.Label, "--format", "{{json .}}")
	if err != nil {
		return nil, err
	}
	var list []runtime.Container
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		var d psDoc
		if err := json.Unmarshal([]byte(line), &d); err != nil {
			return nil, fmt.Errorf("parse docker ps: %w", err)
		}
		label := ""
		for _, kv := range strings.Split(d.Labels, ",") {
			if k, v, ok := strings.Cut(kv, "="); ok && k == runtime.Label {
				label = v
			}
		}
		list = append(list, runtime.Container{Exists: true, Running: d.State == "running", Image: d.Image, Label: label})
	}
	return list, nil
}

// VethOf 找容器 eth0 在宿主机上的对端 veth：在容器 netns 里 `ip -o link show dev eth0` 输出 `eth0@ifN`，
// N 即宿主机侧接口索引。不能读 /sys/class/net/eth0/iflink：nsenter -n 只换网络命名空间，/sys 仍是宿主机的 sysfs。
func (r *Runtime) VethOf(ctx context.Context, id string) (string, error) {
	out, err := r.Run.Run(ctx, "docker", "inspect", "--type", "container", "--format", "{{.State.Pid}}", id)
	if err != nil {
		return "", err
	}
	pid := strings.TrimSpace(string(out))
	if pid == "" || pid == "0" {
		return "", errors.New("container is not running")
	}
	out, err = r.Run.Run(ctx, "nsenter", "-t", pid, "-n", "ip", "-o", "link", "show", "dev", "eth0")
	if err != nil {
		return "", err
	}
	m := peerIndexRe.FindStringSubmatch(string(out))
	if m == nil {
		return "", fmt.Errorf("container %s eth0 has no veth peer: %q", id, strings.TrimSpace(string(out)))
	}
	idx := m[1]
	out, err = r.Run.Run(ctx, "ip", "-o", "link", "show")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(line, idx+": ") {
			continue
		}
		name := strings.TrimPrefix(line, idx+": ")
		name = strings.SplitN(name, ":", 2)[0]
		name = strings.SplitN(name, "@", 2)[0]
		return name, nil
	}
	return "", fmt.Errorf("host interface %s for %s not found", idx, id)
}

// peerIndexRe 取 `16: eth0@if17: <...>` 里的对端索引 17。
var peerIndexRe = regexp.MustCompile(`^\d+: eth0@if(\d+):`)

// validateSpec 拒绝会让 docker 静默放宽限制的空值（空 LanIP 会绑定 0.0.0.0，--memory 0、--pids-limit 0 表示不限）。
func validateSpec(s runtime.Spec) error {
	switch {
	case s.InstanceID == "":
		return errors.New("runtime: empty InstanceID")
	case s.ImageRef == "":
		return errors.New("runtime: empty ImageRef")
	case s.LanIP == "":
		return errors.New("runtime: empty LanIP")
	case s.SSHPort <= 0:
		return errors.New("runtime: invalid SSHPort")
	case s.SSHDir == "" || s.DataDir == "":
		return errors.New("runtime: empty SSHDir or DataDir")
	case s.MemoryBytes <= 0 || s.ShmBytes <= 0:
		return errors.New("runtime: MemoryBytes and ShmBytes must be positive")
	case s.PidsLimit <= 0: // --pids-limit 0 / -1 表示不限
		return errors.New("runtime: PidsLimit must be positive")
	case s.CPUs < 0:
		return errors.New("runtime: negative CPUs")
	}
	return nil
}
