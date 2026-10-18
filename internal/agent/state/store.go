// Package state 是 Agent 的本地状态文件：已执行指令的结果（去重回放、补发）与实例期望状态（重启恢复）。
package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
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
	DataSizeGiB   int    `json:"data_size_gib"`
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
	return os.Rename(tmp.Name(), s.path)
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
	return i, ok
}

func (s *Store) PutInstance(i Instance) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.f.Instances[i.ID] = i
	return s.save()
}

func (s *Store) DeleteInstance(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.f.Instances, id)
	return s.save()
}

func (s *Store) Instances() []Instance {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Instance, 0, len(s.f.Instances))
	for _, i := range s.f.Instances {
		out = append(out, i)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out
}
