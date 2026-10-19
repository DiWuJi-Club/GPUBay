package appdeploy_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"mvdan.cc/sh/v3/syntax"

	"spark-cloud/internal/appdeploy"
)

// appsDir 是仓库里随平台发布的初始应用目录（platform admin app import deploy/apps 导入）。
const appsDir = "../../deploy/apps"

// varRefRe 扫描脚本里引用的大写变量（$VAR / ${VAR}）；小写的脚本局部变量（_dir 等）不在检查范围内。
var varRefRe = regexp.MustCompile(`\$\{?([A-Z][A-Z0-9_]*)`)

// allowedVar 平台注入的保留变量与 sh 自带变量：不需要在 spec 里声明。
func allowedVar(name string) bool {
	switch {
	case strings.HasPrefix(name, "SPARK_"), strings.HasPrefix(name, "HF_"):
		return true
	}
	return slices.Contains([]string{"MODELSCOPE_CACHE", "PIP_INDEX_URL", "PATH", "HOME"}, name)
}

// importAppJSON 与 importer.go 的 appJSON 字段一致；未知字段报错（拼写错误不会被导入静默忽略）。
type importAppJSON struct {
	Name        string   `json:"name"`
	Summary     string   `json:"summary"`
	Description string   `json:"description"`
	Category    string   `json:"category"`
	Icon        string   `json:"icon"`
	Sort        int      `json:"sort"`
	Version     string   `json:"version"`
	Images      []string `json:"images"`
	Changelog   string   `json:"changelog"`
}

// 后台登记应用镜像时用的名称（images.name），导入按名称找镜像。
var knownImages = []string{"LLM Serving", "ComfyUI", "PyTorch"}

func TestAppsDir(t *testing.T) {
	entries, err := os.ReadDir(appsDir)
	if err != nil {
		t.Fatalf("read %s: %v", appsDir, err)
	}
	var slugs []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			slugs = append(slugs, e.Name())
		}
	}
	for _, want := range []string{"comfyui", "jupyterlab", "open-webui", "vllm"} {
		if !slices.Contains(slugs, want) {
			t.Errorf("deploy/apps/%s 不存在", want)
		}
	}
	for _, slug := range slugs {
		t.Run(slug, func(t *testing.T) { checkAppDir(t, slug, filepath.Join(appsDir, slug)) })
	}
}

func checkAppDir(t *testing.T, slug, dir string) {
	if !appdeploy.ValidSlug(slug) {
		t.Errorf("目录名 %q 不是合法 slug", slug)
	}
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return b
	}
	metaRaw, specRaw, scriptRaw := read("app.json"), read("spec.json"), read("app.sh")

	var meta importAppJSON
	dec := json.NewDecoder(bytes.NewReader(metaRaw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&meta); err != nil {
		t.Fatalf("app.json: %v", err)
	}
	if meta.Name == "" || utf8.RuneCountInString(meta.Name) > 32 || utf8.RuneCountInString(meta.Summary) > 80 || utf8.RuneCountInString(meta.Description) > 4000 {
		t.Errorf("app.json: name 1～32 / summary ≤ 80 / description ≤ 4000 字符")
	}
	if !slices.Contains([]string{"llm", "image", "notebook", "tool"}, meta.Category) {
		t.Errorf("app.json: category %q", meta.Category)
	}
	if !slices.Contains([]string{"app", "llm", "chat", "image", "notebook", "tool", "code", "audio"}, meta.Icon) {
		t.Errorf("app.json: icon %q", meta.Icon)
	}
	if !appdeploy.ValidVersion(meta.Version) {
		t.Errorf("app.json: version %q", meta.Version)
	}
	if len(meta.Images) == 0 {
		t.Errorf("app.json: images 为空")
	}
	for _, img := range meta.Images {
		if !slices.Contains(knownImages, img) {
			t.Errorf("app.json: 镜像名 %q 不在约定的 %v 里", img, knownImages)
		}
	}

	var spec appdeploy.Spec
	sdec := json.NewDecoder(bytes.NewReader(specRaw))
	sdec.DisallowUnknownFields()
	if err := sdec.Decode(&spec); err != nil {
		t.Fatalf("spec.json: %v", err)
	}
	if err := appdeploy.ValidateSpec(&spec); err != nil {
		t.Fatalf("spec.json: %v", err)
	}

	script := string(scriptRaw)
	if len(scriptRaw) > appdeploy.MaxScriptBytes {
		t.Errorf("app.sh %d 字节超过 %d", len(scriptRaw), appdeploy.MaxScriptBytes)
	}
	if err := appdeploy.CheckShellSyntax(context.Background(), script); err != nil {
		t.Fatalf("app.sh: %v", err)
	}
	file, err := syntax.NewParser(syntax.Variant(syntax.LangPOSIX)).Parse(strings.NewReader(script), "app.sh")
	if err != nil {
		t.Fatalf("app.sh: %v", err)
	}

	// 子命令：install / start 必须有，start 分支必须写 service.pid
	branches := caseBranches(file)
	for _, sub := range []string{"install", "start"} {
		if _, ok := branches[sub]; !ok {
			t.Errorf("app.sh 没有 %s) 分支", sub)
		}
	}
	if !redirectsTo(branches["start"], "service.pid") {
		t.Errorf("app.sh 的 start 分支没有把 PID 写进 service.pid（> …/service.pid）")
	}

	// 引用的大写变量：spec 声明的、平台保留的、或脚本自己赋值的
	declared := map[string]bool{}
	for _, e := range spec.Env {
		declared[e.Name] = true
	}
	assigned := assignedVars(file)
	for _, m := range varRefRe.FindAllStringSubmatch(script, -1) {
		name := m[1]
		if !declared[name] && !assigned[name] && !allowedVar(name) {
			t.Errorf("app.sh 引用了 $%s：既不在 spec.env 里，也不是保留变量或脚本内赋值的变量", name)
		}
	}
}

// caseBranches 找出脚本里 case 语句各分支（模式 → 分支语句），用来检查 install / start / stop 子命令。
func caseBranches(file *syntax.File) map[string][]*syntax.Stmt {
	out := map[string][]*syntax.Stmt{}
	syntax.Walk(file, func(n syntax.Node) bool {
		if cc, ok := n.(*syntax.CaseClause); ok {
			for _, item := range cc.Items {
				for _, pat := range item.Patterns {
					if lit := pat.Lit(); lit != "" {
						out[lit] = append(out[lit], item.Stmts...)
					}
				}
			}
		}
		return true
	})
	return out
}

// redirectsTo 语句里有没有输出重定向（> / >> / >|）写到文件名以 name 结尾的文件。
func redirectsTo(stmts []*syntax.Stmt, name string) bool {
	found := false
	for _, st := range stmts {
		syntax.Walk(st, func(n syntax.Node) bool {
			if r, ok := n.(*syntax.Redirect); ok && (r.Op == syntax.RdrOut || r.Op == syntax.AppOut || r.Op == syntax.RdrClob) {
				var w strings.Builder
				if err := syntax.NewPrinter().Print(&w, r.Word); err == nil && strings.HasSuffix(strings.Trim(w.String(), `"'`), name) {
					found = true
				}
			}
			return !found
		})
	}
	return found
}

// assignedVars 脚本里赋值过的变量名（NAME=…、export NAME=…、for NAME in …）。
func assignedVars(file *syntax.File) map[string]bool {
	out := map[string]bool{}
	syntax.Walk(file, func(n syntax.Node) bool {
		switch x := n.(type) {
		case *syntax.Assign:
			if x.Name != nil && !x.Naked { // export NAME（无 =）不算赋值
				out[x.Name.Value] = true
			}
		case *syntax.WordIter:
			if x.Name != nil {
				out[x.Name.Value] = true
			}
		}
		return true
	})
	return out
}
