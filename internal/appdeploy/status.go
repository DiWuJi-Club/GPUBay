package appdeploy

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
)

// Status 是容器内 status.json 的内容（run.sh 写）。
type Status struct {
	Seq        int64  `json:"seq"`
	State      string `json:"state"`
	Phase      string `json:"phase"`
	Progress   int    `json:"progress"`
	Message    string `json:"message"`
	Pid        int    `json:"pid"`
	ServicePid int    `json:"service_pid"`
	SpecHash   string `json:"spec_hash"`
	ExitCode   int    `json:"exit_code"`
	At         string `json:"at"`
}

// ParseStatusOutput 解析 Runner 状态组合命令（`sh run.sh status; rc=$?; echo ---; tail -c 65536 log; exit $rc`）的输出：
// 第一行 status.json 原文（超过 16KB、不是合法 JSON、没有 state → err），随后 `alive=` / `log_size=` 行，
// `---` 之后全部是日志尾部（只保留最后 MaxLogTail 字节）。
func ParseStatusOutput(out []byte) (st Status, alive bool, logTail string, err error) {
	head, tail, _ := bytes.Cut(out, []byte("\n---\n"))
	lines := strings.Split(strings.TrimRight(string(head), "\n"), "\n")
	if lines[0] == "" || len(lines[0]) > MaxStatusBytes {
		return Status{}, false, "", errors.New("status.json 缺失或超过 16KB")
	}
	if err := json.Unmarshal([]byte(lines[0]), &st); err != nil {
		return Status{}, false, "", errors.New("status.json 不是合法 JSON")
	}
	if st.State == "" { // run.sh 在 status.json 缺失时输出 {"error":"no status"}
		return Status{}, false, "", errors.New("status.json 没有 state")
	}
	for _, l := range lines[1:] {
		if l == "alive=1" {
			alive = true
		}
	}
	if len(tail) > MaxLogTail {
		tail = tail[len(tail)-MaxLogTail:]
	}
	return st, alive, string(tail), nil
}
