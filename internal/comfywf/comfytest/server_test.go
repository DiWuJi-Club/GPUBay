package comfytest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func getJSON(t *testing.T, url string, v any) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode
}

func TestServer(t *testing.T) {
	s, ts := Start()
	defer ts.Close()

	var stats map[string]any
	if getJSON(t, ts.URL+"/system_stats", &stats) != 200 {
		t.Fatal("system_stats")
	}
	resp, err := http.Post(ts.URL+"/prompt", "application/json",
		strings.NewReader(`{"prompt":{"3":{"class_type":"KSampler","inputs":{}},"12":{"class_type":"SaveImage","inputs":{}}},"client_id":"run-abc"}`))
	if err != nil {
		t.Fatal(err)
	}
	var pr struct {
		PromptID string `json:"prompt_id"`
	}
	json.NewDecoder(resp.Body).Decode(&pr)
	resp.Body.Close()
	if pr.PromptID == "" || s.PromptIDForClient("run-abc") != pr.PromptID || len(s.Prompts()) != 1 {
		t.Fatalf("prompt = %+v", pr)
	}

	var h map[string]json.RawMessage
	if getJSON(t, ts.URL+"/history/"+pr.PromptID, &h); len(h) != 0 {
		t.Fatalf("queued history = %v", h)
	}
	var q struct {
		Running [][]json.RawMessage `json:"queue_running"`
		Pending [][]json.RawMessage `json:"queue_pending"`
	}
	getJSON(t, ts.URL+"/queue", &q)
	if len(q.Running) != 0 || len(q.Pending) != 1 {
		t.Fatalf("queue = %+v", q)
	}
	s.SetHistory(pr.PromptID, RunningState())
	getJSON(t, ts.URL+"/queue", &q)
	if len(q.Running) != 1 || string(q.Running[0][1]) != `"`+pr.PromptID+`"` {
		t.Fatalf("queue = %+v", q)
	}

	s.SetHistory(pr.PromptID, DoneN(2))
	var done map[string]struct {
		Outputs map[string]struct {
			Images []File `json:"images"`
		} `json:"outputs"`
		Status struct {
			Completed bool `json:"completed"`
		} `json:"status"`
	}
	getJSON(t, ts.URL+"/history/"+pr.PromptID, &done)
	if d := done[pr.PromptID]; !d.Status.Completed || len(d.Outputs["12"].Images) != 2 || d.Outputs["12"].Images[1].Filename != "ComfyUI_00002_.png" {
		t.Fatalf("done = %+v", d)
	}

	if resp, err := http.Get(ts.URL + "/view?filename=other.png&type=output"); err != nil || resp.StatusCode != 404 {
		t.Fatalf("unknown file: %v %v", resp, err)
	}
	resp, err = http.Get(ts.URL + "/view?filename=ComfyUI_00001_.png&type=output")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.Header.Get("Content-Type") != "image/png" || string(b) != string(OnePixelPNG) || s.Views()[1].Get("filename") != "ComfyUI_00001_.png" ||
		resp.Header.Get("Content-Disposition") != `filename="ComfyUI_00001_.png"` {
		t.Fatalf("view: %v %d", resp.Header, len(b))
	}

	// Range（同真实 ComfyUI 的 aiohttp FileResponse）：206 + Content-Range；Size 模式也支持
	rangeGet := func(rng string) (*http.Response, []byte) {
		t.Helper()
		req, _ := http.NewRequest("GET", ts.URL+"/view?filename=ComfyUI_00001_.png&type=output", nil)
		req.Header.Set("Range", rng)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, b
	}
	if resp, b := rangeGet("bytes=1-3"); resp.StatusCode != 206 || string(b) != string(OnePixelPNG[1:4]) ||
		resp.Header.Get("Content-Range") != fmt.Sprintf("bytes 1-3/%d", len(OnePixelPNG)) || resp.Header.Get("Accept-Ranges") != "bytes" ||
		resp.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("range view: %d %v %q", resp.StatusCode, resp.Header, b)
	}
	s.SetView(ViewConfig{Size: 100, ContentType: "video/mp4"})
	if resp, b := rangeGet("bytes=90-"); resp.StatusCode != 206 || len(b) != 10 || resp.Header.Get("Content-Range") != "bytes 90-99/100" {
		t.Fatalf("range sized view: %d %v %q", resp.StatusCode, resp.Header, b)
	}
	s.SetView(ViewConfig{})

	// 队列项与 history 回显提交的 prompt 与 client_id
	var full map[string]struct {
		Prompt []json.RawMessage `json:"prompt"`
	}
	getJSON(t, ts.URL+"/history/"+pr.PromptID, &full)
	if p := full[pr.PromptID].Prompt; len(p) != 5 || !strings.Contains(string(p[2]), "SaveImage") || string(p[3]) != `{"client_id":"run-abc"}` {
		t.Fatalf("history prompt = %s", p)
	}

	s.RejectPrompts("bad ckpt")
	resp, err = http.Post(ts.URL+"/prompt", "application/json", strings.NewReader(`{"prompt":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("reject status = %d", resp.StatusCode)
	}

	s.RejectMissingNode("FakeNodeType")
	resp, err = http.Post(ts.URL+"/prompt", "application/json", strings.NewReader(`{"prompt":{"1":{}}}`))
	if err != nil {
		t.Fatal(err)
	}
	mb, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 400 || !strings.Contains(string(mb), "missing_node_type") || !strings.Contains(string(mb), `"node_errors":{}`) {
		t.Fatalf("missing node: %d %s", resp.StatusCode, mb)
	}
	s.RejectMissingNode("")
	resp, err = http.Post(ts.URL+"/prompt", "application/json", strings.NewReader(`{"client_id":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	mb, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 400 || !strings.Contains(string(mb), "no_prompt") {
		t.Fatalf("no prompt: %d %s", resp.StatusCode, mb)
	}

	s.Reset()
	if len(s.Prompts()) != 0 || len(s.Requests()) != 0 || s.PromptIDForClient("run-abc") != "" {
		t.Fatal("reset")
	}
}

func TestIndexAndRequestCap(t *testing.T) {
	s, ts := Start()
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") || !strings.Contains(string(b), "Fake ComfyUI") {
		t.Fatalf("GET / = %d %s %q", resp.StatusCode, resp.Header.Get("Content-Type"), b)
	}
	for i := 0; i < maxRequests+50; i++ {
		r, err := http.Get(ts.URL + "/system_stats")
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
	}
	if got := s.Requests(); len(got) != maxRequests || got[len(got)-1] != "GET /system_stats" {
		t.Fatalf("requests = %d", len(got))
	}
}
