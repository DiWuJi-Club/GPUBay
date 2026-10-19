package cmdrun

import (
	"context"
	"strings"
	"sync"
)

type stub struct {
	prefix string
	out    string
	err    error
}

// Recorder 记录每次调用的命令行，并按前缀返回脚本化的输出；后注册的 stub 优先。
type Recorder struct {
	mu    sync.Mutex
	Calls []string
	// Inputs 按顺序记录每次 RunInput 的 stdin，供快照断言。
	Inputs []string
	stubs  []stub
}

func (r *Recorder) Stub(cmdPrefix, out string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stubs = append(r.stubs, stub{prefix: cmdPrefix, out: out, err: err})
}

func (r *Recorder) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return r.run(nil, false, name, args...)
}

func (r *Recorder) RunInput(_ context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	return r.run(stdin, true, name, args...)
}

func (r *Recorder) run(stdin []byte, hasInput bool, name string, args ...string) ([]byte, error) {
	line := Line(name, args...)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Calls = append(r.Calls, line)
	if hasInput {
		r.Inputs = append(r.Inputs, string(stdin))
	}
	for i := len(r.stubs) - 1; i >= 0; i-- {
		if strings.HasPrefix(line, r.stubs[i].prefix) {
			return []byte(r.stubs[i].out), r.stubs[i].err
		}
	}
	return nil, nil
}

func (r *Recorder) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Calls, r.Inputs = nil, nil
}

func (r *Recorder) Lines() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.Calls, "\n")
}
