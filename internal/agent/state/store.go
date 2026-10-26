// Package state 是 Agent 的本地状态文件：已执行指令的结果（去重回放、补发）与实例期望状态（重启恢复）。
package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"

	"spark-cloud/internal/clusterspec"
)

// maxResults 由 export_test.go 的 SetMaxResultsForTest 在测试里调小（macOS 上每次 fsync 要 10～20ms，1000 条会跑十几秒）。
var maxResults = 1000

type Result struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
	Sent    bool   `json:"sent"`
}

type ResultEntry struct {
	ID string `json:"id"`
	Result
}

type Instance struct {
	ID            string `json:"id"`
	Desired       string `json:"desired"` // running / stopped
	ImageRef      string `json:"image_ref"`
	LanIP         string `json:"lan_ip"`
	SSHPort       int    `json:"ssh_port"`
	BandwidthMbps int    `json:"bandwidth_mbps"`
	DataSizeGiB   int    `json:"data_size_gib"` // 实际数据盘大小（共享机为等分后的值）
	// ShareSlots：建容器时的份数（0 / 1 = 整机）。计划 10 新增，旧状态文件没有该字段（读出为 0）；
	// Recover 不需要它（配额已固化在容器参数里）。
	ShareSlots int `json:"share_slots,omitempty"`
	// 计划 13 集群成员（旧状态文件没有这些字段，读出为 nil / 空）：
	// Cluster：当前生效的 ClusterSpec（create 时的，之后被 ConfigureCluster 更新）；nil = 非集群实例。
	Cluster *clusterspec.Spec `json:"cluster,omitempty"`
	// ClusterRuntime：最近一次成功 Apply 的结果（RDMA HCA / GID 索引），cluster.env 据此渲染。
	ClusterRuntime *clusterspec.Runtime `json:"cluster_runtime,omitempty"`
	// ClusterNote：ConfigureCluster 发现的「需重建实例才能生效」的变更（模式 / 网卡 / VLAN 变更、RDMA 由无变有），
	// 非空时集群状态恒为 "error: <note>"（网络按 create 时的形态继续工作）。
	ClusterNote string `json:"cluster_note,omitempty"`
}

// clone 深拷贝指针字段：Store 内部与调用方各持一份，调用方改动返回值不影响 Store。
func (i Instance) clone() Instance {
	if i.Cluster != nil {
		c := *i.Cluster
		c.Members = slices.Clone(c.Members)
		i.Cluster = &c
	}
	if i.ClusterRuntime != nil {
		rt := *i.ClusterRuntime
		i.ClusterRuntime = &rt
	}
	return i
}

type file struct {
	Results   []ResultEntry       `json:"results"` // 旧 → 新
	Instances map[string]Instance `json:"instances"`
}

type Store struct {
	path string
	mu   sync.Mutex
	f    file
}

func Open(path string) (*Store, error) {
	s := &Store{path: path, f: file{Instances: map[string]Instance{}}}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &s.f); err != nil {
		return nil, err
	}
	if s.f.Instances == nil {
		s.f.Instances = map[string]Instance{}
	}
	return s, nil
}

// save 先写临时文件并 fsync，再 rename，保证状态文件要么旧要么新。
func (s *Store) save() error {
	b, err := json.MarshalIndent(s.f, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // rename 成功后不存在
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		return err
	}
	// 同步目录项，保证 rename 在掉电后也持久。
	d, err := os.Open(filepath.Dir(s.path))
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		d.Close()
		return err
	}
	return d.Close()
}

func (s *Store) Result(id string) (Result, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.f.Results) - 1; i >= 0; i-- {
		if s.f.Results[i].ID == id {
			return s.f.Results[i].Result, true
		}
	}
	return Result{}, false
}

func (s *Store) PutResult(id string, r Result) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.f.Results {
		if s.f.Results[i].ID == id {
			s.f.Results[i].Result = r
			return s.save()
		}
	}
	s.f.Results = append(s.f.Results, ResultEntry{ID: id, Result: r})
	if len(s.f.Results) > maxResults {
		s.f.Results = s.f.Results[len(s.f.Results)-maxResults:]
	}
	return s.save()
}

func (s *Store) MarkSent(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.f.Results {
		if s.f.Results[i].ID == id {
			s.f.Results[i].Sent = true
			return s.save()
		}
	}
	return nil
}

func (s *Store) UnsentResults() []ResultEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []ResultEntry
	for _, r := range s.f.Results {
		if !r.Sent {
			out = append(out, r)
		}
	}
	return out
}

func (s *Store) Instance(id string) (Instance, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i, ok := s.f.Instances[id]
	return i.clone(), ok
}

func (s *Store) PutInstance(i Instance) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.f.Instances[i.ID] = i.clone()
	return s.save()
}

func (s *Store) DeleteInstance(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.f.Instances, id)
	return s.save()
}

// LanIPs 返回实例记录里的管理地址（去重、去空、升序）。Agent 重启后、AgentConfig / Probe 之前，
// 集群网络（macvlan）据此认出管理网卡，不清它的地址。
func (s *Store) LanIPs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, i := range s.f.Instances {
		if i.LanIP != "" && !slices.Contains(out, i.LanIP) {
			out = append(out, i.LanIP)
		}
	}
	slices.Sort(out)
	return out
}

func (s *Store) Instances() []Instance {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Instance, 0, len(s.f.Instances))
	for _, i := range s.f.Instances {
		out = append(out, i.clone())
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out
}
