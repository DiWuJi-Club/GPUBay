package comfywf

import (
	"encoding/json"
	"errors"
	"path"
	"strings"
)

// HistoryResult 是 GET /history/{prompt_id} 解析结果。State：""（history 里还没有该 id，调用方再查 /queue）、
// RunDone、RunError。
type HistoryResult struct {
	State   string
	Message string
	Files   []OutputFile
}

// OutputFile 一个输出文件；Filename / Subfolder / Type 原样用于代理 GET /view（不接受客户端传文件名）。Kind：image / video。
type OutputFile struct {
	Index     int
	Filename  string
	Subfolder string
	Type      string
	Kind      string
}

const (
	KindImage = "image"
	KindVideo = "video"
	// maxRunMessage 错误消息截断长度（字符）。
	maxRunMessage = 500
)

// outputKeys 收集输出文件的键（顺序即同一节点内的输出顺序）。ComfyUI v0.3.60：SaveImage / SaveAnimatedWEBP /
// SaveAnimatedPNG → images；SaveVideo / SaveWEBM（comfy_api ui.PreviewVideo）→ images + "animated": [true]，SaveVideo 的
// subfolder 通常是 video；VHS_VideoCombine（自定义节点）→ gifs；videos 留给其它节点。
var outputKeys = []string{"images", "gifs", "videos"}

var videoExts = map[string]bool{".mp4": true, ".webm": true, ".mov": true, ".mkv": true, ".avi": true, ".m4v": true}
var imageExts = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".webp": true, ".gif": true, ".apng": true, ".bmp": true}

// ParseHistory 解析 ComfyUI GET /history/{prompt_id} 的响应（形如 {"<id>": {"prompt": […], "outputs": {…}, "status":
// {"status_str", "completed", "messages": [[event, {…}], …]}, "meta": {…}}}；任务结束前是 {}）。outputs 非空时只取
// 声明的节点（按声明顺序），否则取全部节点（按节点 id 数值顺序）。只收 type = output 的文件；文件名 / 子目录不安全的跳过。
// 响应体视为不可信，长度（≤ 1MB）由调用方保证。
func ParseHistory(body []byte, promptID string, outputs []OutputSpec) (HistoryResult, error) {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(body, &all); err != nil {
		return HistoryResult{}, errors.New("comfywf: history 不是 JSON 对象")
	}
	raw, ok := all[promptID]
	if !ok {
		return HistoryResult{}, nil
	}
	var h struct {
		Status *struct {
			StatusStr string            `json:"status_str"`
			Completed bool              `json:"completed"`
			Messages  []json.RawMessage `json:"messages"`
		} `json:"status"`
		Outputs map[string]json.RawMessage `json:"outputs"`
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		return HistoryResult{}, errors.New("comfywf: history 条目格式不对")
	}
	if h.Status != nil && h.Status.StatusStr == "error" {
		return HistoryResult{State: RunError, Message: errorMessage(h.Status.Messages)}, nil
	}
	// history 里出现即已结束（execution.PromptQueue.task_done）；status 缺失按完成处理。
	if h.Status != nil && !h.Status.Completed && h.Status.StatusStr != "success" {
		return HistoryResult{}, nil
	}
	var nodeIDs []string
	if len(outputs) > 0 {
		for _, o := range outputs {
			nodeIDs = append(nodeIDs, o.Node)
		}
	} else {
		nodeIDs = sortedKeys(h.Outputs)
	}
	res := HistoryResult{State: RunDone, Files: []OutputFile{}}
	for _, id := range nodeIDs {
		var node map[string]json.RawMessage
		if json.Unmarshal(h.Outputs[id], &node) != nil {
			continue
		}
		for _, key := range outputKeys {
			var items []json.RawMessage
			if json.Unmarshal(node[key], &items) != nil {
				continue
			}
			for _, it := range items {
				var f struct {
					Filename  string `json:"filename"`
					Subfolder string `json:"subfolder"`
					Type      string `json:"type"`
				}
				if json.Unmarshal(it, &f) != nil || f.Type != "output" || !safeFilename(f.Filename) || !safeSubfolder(f.Subfolder) {
					continue
				}
				res.Files = append(res.Files, OutputFile{Index: len(res.Files), Filename: f.Filename, Subfolder: f.Subfolder,
					Type: f.Type, Kind: fileKind(key, f.Filename)})
			}
		}
	}
	return res, nil
}

// fileKind 按扩展名判断（images 下也可能是 SaveVideo 的 .mp4）；未知扩展名：images → image，gifs / videos → video。
// 动图 .gif / .webp 记为 image（<img> 能直接播放）。
func fileKind(key, filename string) string {
	ext := strings.ToLower(path.Ext(filename))
	switch {
	case videoExts[ext]:
		return KindVideo
	case imageExts[ext]:
		return KindImage
	case key == "images":
		return KindImage
	}
	return KindVideo
}

func safeFilename(s string) bool {
	return s != "" && len(s) <= 255 && s != "." && s != ".." && !strings.ContainsAny(s, "/\\\x00")
}

func safeSubfolder(s string) bool {
	if s == "" {
		return true
	}
	if len(s) > 255 || strings.HasPrefix(s, "/") || strings.ContainsAny(s, "\\\x00") {
		return false
	}
	for _, seg := range strings.Split(s, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// errorMessage 取 status.messages 里 execution_error 的 exception_message（去首尾空白，截 500 字符）；
// 被中断（execution_interrupted）或找不到时给固定文案。
func errorMessage(msgs []json.RawMessage) string {
	interrupted := false
	for _, m := range msgs {
		var pair []json.RawMessage
		if json.Unmarshal(m, &pair) != nil || len(pair) != 2 {
			continue
		}
		var event string
		if json.Unmarshal(pair[0], &event) != nil {
			continue
		}
		switch event {
		case "execution_error":
			var d struct {
				ExceptionMessage string `json:"exception_message"`
			}
			if json.Unmarshal(pair[1], &d) == nil {
				if s := strings.TrimSpace(d.ExceptionMessage); s != "" {
					return truncateRunes(s, maxRunMessage)
				}
			}
		case "execution_interrupted":
			interrupted = true
		}
	}
	if interrupted {
		return "任务已被中断"
	}
	return "ComfyUI 执行失败"
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
