package comfywf

import (
	"bytes"
	"encoding/json"
	"errors"
	"path"
	"strings"
	"unicode"
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
	// maxRunFiles 一次运行最多收集的输出文件数。
	maxRunFiles      = 64
	malformedHistory = "ComfyUI 返回了无法解析的结果"
)

// outputKeys 收集输出文件的键（顺序即同一节点内的输出顺序）。ComfyUI v0.3.60：SaveImage / SaveAnimatedWEBP /
// SaveAnimatedPNG → images；SaveVideo / SaveWEBM（comfy_api ui.PreviewVideo）→ images + "animated": [true]，SaveVideo 的
// subfolder 通常是 video；VHS_VideoCombine（自定义节点）→ gifs；videos 留给其它节点。
var outputKeys = []string{"images", "gifs", "videos"}

var videoExts = map[string]bool{".mp4": true, ".webm": true, ".mov": true, ".mkv": true, ".avi": true, ".m4v": true}
var imageExts = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".webp": true, ".gif": true, ".apng": true, ".bmp": true}

// ParseHistory 解析 ComfyUI GET /history/{prompt_id} 的响应（形如 {"<id>": {"prompt": […], "outputs": {…}, "status":
// {"status_str", "completed", "messages": [[event, {…}], …]}, "meta": {…}}}；任务结束前是 {}）。outputs 非空时只取
// 声明的节点（按声明顺序），否则取全部节点（按节点 id 数值顺序）。只收 type = output、扩展名是已知图片 / 视频的文件，
// 文件名 / 子目录不安全（路径穿越、控制字符、引号）的跳过，最多 maxRunFiles 个。条目不是对象 → RunError。
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
	if t := bytes.TrimSpace(raw); len(t) == 0 || t[0] != '{' {
		return HistoryResult{State: RunError, Message: malformedHistory}, nil
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		// 字段类型不对（如 outputs 是数组）：按字段逐个容错，不让前端一直轮询。
		var loose struct {
			Status  json.RawMessage `json:"status"`
			Outputs json.RawMessage `json:"outputs"`
		}
		if json.Unmarshal(raw, &loose) != nil {
			return HistoryResult{State: RunError, Message: malformedHistory}, nil
		}
		_ = json.Unmarshal(loose.Status, &h.Status)
		_ = json.Unmarshal(loose.Outputs, &h.Outputs)
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
				if len(res.Files) >= maxRunFiles {
					break
				}
				var f struct {
					Filename  string `json:"filename"`
					Subfolder string `json:"subfolder"`
					Type      string `json:"type"`
				}
				if json.Unmarshal(it, &f) != nil || f.Type != "output" || !safeFilename(f.Filename) || !safeSubfolder(f.Subfolder) {
					continue
				}
				kind := fileKind(f.Filename)
				if kind == "" {
					continue
				}
				res.Files = append(res.Files, OutputFile{Index: len(res.Files), Filename: f.Filename, Subfolder: f.Subfolder,
					Type: f.Type, Kind: kind})
			}
		}
	}
	return res, nil
}

// fileKind 只按扩展名判断（images 下也可能是 SaveVideo 的 .mp4）；动图 .gif / .webp 记为 image（<img> 能直接播放）；
// 未知扩展名 → ""（跳过：代理 /view 只为图片 / 视频服务）。
func fileKind(filename string) string {
	ext := strings.ToLower(path.Ext(filename))
	switch {
	case videoExts[ext]:
		return KindVideo
	case imageExts[ext]:
		return KindImage
	}
	return ""
}

// hasUnsafeChars 控制字符（< 0x20、0x7f）、引号、反斜杠（会进 Content-Disposition 与 /view 查询参数）。
func hasUnsafeChars(s string) bool {
	for _, c := range s {
		if c < 0x20 || c == 0x7f || c == '"' || c == '\\' {
			return true
		}
	}
	return false
}

func safeFilename(s string) bool {
	return s != "" && len(s) <= 255 && s != "." && s != ".." && !strings.Contains(s, "/") && !hasUnsafeChars(s)
}

func safeSubfolder(s string) bool {
	if s == "" {
		return true
	}
	if len(s) > 255 || strings.HasPrefix(s, "/") || hasUnsafeChars(s) {
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
				if s := strings.TrimSpace(stripControl(d.ExceptionMessage)); s != "" {
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

// stripControl 清理容器给的、要进错误消息的文本：控制字符 → 空格，双向控制字符（unicode.Bidi_Control）删除，非法 UTF-8 删除。
func stripControl(s string) string {
	return strings.Map(func(c rune) rune {
		switch {
		case unicode.Is(unicode.Bidi_Control, c):
			return -1
		case c < 0x20 || c == 0x7f || (c >= 0x80 && c < 0xa0):
			return ' '
		}
		return c
	}, strings.ToValidUTF8(s, ""))
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
