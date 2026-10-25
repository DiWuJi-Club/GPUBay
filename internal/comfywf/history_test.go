package comfywf

import (
	"strings"
	"testing"
)

const histID = "2f3c5a1e-8b7d-4c1a-9e0f-6d5b4a3c2b1a"

func TestParseHistoryQueued(t *testing.T) {
	for _, body := range []string{string(fixture(t, "history_queued.json")), `{"other-id":{"status":{"status_str":"success","completed":true}}}`} {
		r, err := ParseHistory([]byte(body), histID, nil)
		if err != nil || r.State != "" || len(r.Files) != 0 {
			t.Fatalf("r = %+v, err = %v", r, err)
		}
	}
	if _, err := ParseHistory([]byte(`[`), histID, nil); err == nil {
		t.Fatal("bad json accepted")
	}
	if _, err := ParseHistory([]byte(`[]`), histID, nil); err == nil {
		t.Fatal("non-object accepted")
	}
}

func TestParseHistoryDone(t *testing.T) {
	body := fixture(t, "history_done.json")
	// 未声明 outputs：取全部 type=output 的文件（PreviewImage 的 temp 跳过）。
	r, err := ParseHistory(body, histID, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []OutputFile{
		{Index: 0, Filename: "ComfyUI_00001_.png", Subfolder: "", Type: "output", Kind: "image"},
		{Index: 1, Filename: "ComfyUI_00002_.png", Subfolder: "", Type: "output", Kind: "image"},
	}
	if r.State != RunDone || r.Message != "" || len(r.Files) != 2 || r.Files[0] != want[0] || r.Files[1] != want[1] {
		t.Fatalf("r = %+v", r)
	}
	// 声明了 outputs：只取声明的节点。
	r, _ = ParseHistory(body, histID, []OutputSpec{{Node: "11"}})
	if r.State != RunDone || len(r.Files) != 0 {
		t.Fatalf("r = %+v", r)
	}
	r, _ = ParseHistory(body, histID, []OutputSpec{{Node: "9"}})
	if len(r.Files) != 2 {
		t.Fatalf("r = %+v", r)
	}
}

func TestParseHistoryError(t *testing.T) {
	r, err := ParseHistory(fixture(t, "history_error.json"), histID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.State != RunError || !strings.HasPrefix(r.Message, "Allocation on device") || strings.HasSuffix(r.Message, "\n") || len(r.Files) != 0 {
		t.Fatalf("r = %+v", r)
	}
	// 截 500 字符（按字符，不截断 UTF-8）。
	long := `{"` + histID + `":{"status":{"status_str":"error","completed":false,"messages":[["execution_start",{}],["execution_error",{"exception_message":"` +
		strings.Repeat("错", 600) + `"}]]},"outputs":{}}}`
	r, _ = ParseHistory([]byte(long), histID, nil)
	if r.State != RunError || len([]rune(r.Message)) != 500 {
		t.Fatalf("len = %d", len([]rune(r.Message)))
	}
	// 中断：没有 exception_message。
	intr := `{"` + histID + `":{"status":{"status_str":"error","completed":false,"messages":[["execution_interrupted",{"node_id":"3"}]]},"outputs":{}}}`
	r, _ = ParseHistory([]byte(intr), histID, nil)
	if r.State != RunError || r.Message == "" {
		t.Fatalf("r = %+v", r)
	}
	// messages 形状异常也不 panic。
	odd := `{"` + histID + `":{"status":{"status_str":"error","completed":false,"messages":[1,["x"],[2,{}],["execution_error","s"]]}}}`
	r, err = ParseHistory([]byte(odd), histID, nil)
	if err != nil || r.State != RunError || r.Message == "" {
		t.Fatalf("r = %+v, err = %v", r, err)
	}
}

func TestParseHistoryVideo(t *testing.T) {
	// v0.3.60：SaveVideo / SaveWEBM 走 comfy_api ui.PreviewVideo → {"images":[…],"animated":[true]}；
	// VHS_VideoCombine → {"gifs":[…]}；SaveAnimatedWEBP → {"images":[…],"animated":[true]}（.webp 仍按图片显示）。
	r, err := ParseHistory(fixture(t, "history_video.json"), histID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.State != RunDone || len(r.Files) != 3 {
		t.Fatalf("r = %+v", r)
	}
	want := []OutputFile{
		{Index: 0, Filename: "ComfyUI_00001_.mp4", Subfolder: "video", Type: "output", Kind: "video"},
		{Index: 1, Filename: "AnimateDiff_00001.webm", Subfolder: "", Type: "output", Kind: "video"},
		{Index: 2, Filename: "ComfyUI_00001_.webp", Subfolder: "", Type: "output", Kind: "image"},
	}
	for i := range want {
		if r.Files[i] != want[i] {
			t.Fatalf("files[%d] = %+v, want %+v", i, r.Files[i], want[i])
		}
	}
	// 声明顺序决定输出顺序。
	r, _ = ParseHistory(fixture(t, "history_video.json"), histID, []OutputSpec{{Node: "61"}, {Node: "57"}})
	if len(r.Files) != 2 || r.Files[0].Filename != "ComfyUI_00001_.webp" || r.Files[1].Index != 1 || r.Files[1].Kind != "video" {
		t.Fatalf("r = %+v", r)
	}
}

func TestParseHistoryUntrusted(t *testing.T) {
	// 不安全的文件名 / 子目录、未知扩展名跳过；节点按数字顺序（同值按原文）；status 缺失视为完成。
	body := `{"` + histID + `":{"outputs":{
		"10":{"images":[{"filename":"b.png","subfolder":"","type":"output"}]},
		"010":{"images":[{"filename":"c.png","subfolder":"","type":"output"}]},
		"9":{"images":[{"filename":"../x.png","subfolder":"","type":"output"},{"filename":"a.png","subfolder":"../..","type":"output"},
		     {"filename":"a.png","subfolder":"/abs","type":"output"},{"filename":"","subfolder":"","type":"output"},
		     {"filename":"q\".png","subfolder":"","type":"output"},{"filename":"nl\n.png","subfolder":"","type":"output"},
		     {"filename":"del\u007f.png","subfolder":"","type":"output"},{"filename":"a.png","subfolder":"x\ty","type":"output"},
		     {"filename":"a.png","subfolder":"x\"y","type":"output"},{"filename":"noext","subfolder":"","type":"output"},
		     {"filename":"ok.png","subfolder":"sub/dir","type":"output"},"junk",{"filename":5}],
		     "gifs":[{"filename":"clip.bin","subfolder":"","type":"output"},{"filename":"clip.MP4","subfolder":"","type":"output"}],"text":["hello"]},
		"11":"junk"}}}`
	r, err := ParseHistory([]byte(body), histID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.State != RunDone || len(r.Files) != 4 || r.Files[0].Filename != "ok.png" || r.Files[0].Subfolder != "sub/dir" ||
		r.Files[1].Filename != "clip.MP4" || r.Files[1].Kind != "video" || r.Files[2].Filename != "c.png" || r.Files[3].Filename != "b.png" ||
		r.Files[3].Index != 3 {
		t.Fatalf("r = %+v", r)
	}
	// "010" 与 "10" 数值相同：按原文定序，结果稳定。
	for i := 0; i < 20; i++ {
		r2, _ := ParseHistory([]byte(body), histID, nil)
		if r2.Files[2].Filename != "c.png" {
			t.Fatal("unstable order")
		}
	}
}

func TestParseHistoryCap(t *testing.T) {
	var items []string
	for i := 0; i < 100; i++ {
		items = append(items, `{"filename":"f`+strings.Repeat("x", i)+`.png","subfolder":"","type":"output"}`)
	}
	body := `{"` + histID + `":{"status":{"status_str":"success","completed":true},"outputs":{"9":{"images":[` + strings.Join(items, ",") + `]}}}}`
	r, _ := ParseHistory([]byte(body), histID, nil)
	if len(r.Files) != maxRunFiles || r.Files[maxRunFiles-1].Index != maxRunFiles-1 {
		t.Fatalf("len = %d", len(r.Files))
	}
}

func TestParseHistoryMalformedEntry(t *testing.T) {
	// 条目不是对象 → error（不当作仍在排队，否则前端会一直轮询）。
	for _, e := range []string{`null`, `"x"`, `[1]`, `5`} {
		r, err := ParseHistory([]byte(`{"`+histID+`":`+e+`}`), histID, nil)
		if err != nil || r.State != RunError || r.Message != "ComfyUI 返回了无法解析的结果" {
			t.Fatalf("%s: r = %+v, err = %v", e, r, err)
		}
	}
	// status: null 但条目是对象 → done（history 里出现即已结束）。
	r, err := ParseHistory([]byte(`{"`+histID+`":{"status":null,"outputs":{"9":{"images":[{"filename":"a.png","subfolder":"","type":"output"}]}}}}`), histID, nil)
	if err != nil || r.State != RunDone || len(r.Files) != 1 {
		t.Fatalf("r = %+v, err = %v", r, err)
	}
	// outputs 形状不对 → done 但没有文件。
	r, _ = ParseHistory([]byte(`{"`+histID+`":{"status":{"status_str":"success","completed":true},"outputs":[1]}}`), histID, nil)
	if r.State != RunDone || len(r.Files) != 0 {
		t.Fatalf("r = %+v", r)
	}
}
