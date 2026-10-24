package comfytest

import (
	"encoding/json"
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

	resp, err = http.Get(ts.URL + "/view?filename=a.png&type=output")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.Header.Get("Content-Type") != "image/png" || string(b) != string(OnePixelPNG) || s.Views()[0].Get("filename") != "a.png" {
		t.Fatalf("view: %v %d", resp.Header, len(b))
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

	s.Reset()
	if len(s.Prompts()) != 0 || len(s.Requests()) != 0 || s.PromptIDForClient("run-abc") != "" {
		t.Fatal("reset")
	}
}
