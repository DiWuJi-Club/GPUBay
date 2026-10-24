package comfywf

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"spark-cloud/internal/appdeploy"
)

// BundleWorkflow 部署包里的一个工作流（集合里的一项 + 其版本）。Manifest 应是已校验、规范化的清单。
type BundleWorkflow struct {
	Slug, Version string
	Graph         json.RawMessage // UI 格式，原文写入 workflows/<slug>.json
	Manifest      Manifest
}

// 容器内 workflows/manifest.json 的形状（规格 §5.2）；app.sh install 用 python3 展开它。
type bundleManifest struct {
	Workflows []bundleEntry `json:"workflows"`
}

type bundleEntry struct {
	Slug    string       `json:"slug"`
	Version string       `json:"version"`
	File    string       `json:"file"` // 包内 workflows/ 下的文件名 <slug>.json；app.sh 装成 user/default/workflows/spark-<slug>.json
	Models  []ModelSpec  `json:"models"`
	Nodes   []bundleNode `json:"nodes"`
}

type bundleNode struct {
	Name   string `json:"name"`
	Git    string `json:"git"`
	Commit string `json:"commit"`
}

// BundleFiles 生成部署包里的工作流文件：workflows/manifest.json（规格 §5.2）与每个工作流的 workflows/<slug>.json
// （graph 原文）。按 slug 排序，输出可重复。
//
//   - 模型按目标位置 <dir>/<name> 去重：多个工作流引用同一文件时，后出现的条目改用先出现的那条（同名不同来源以先出现的
//     为准），各工作流仍各自列出（app.sh 再按目标路径去重）；两边都给了 sha256 且不同 → workflow_version_invalid。
//   - 节点按 name（custom_nodes 下的目录名）去重：Git 地址或 commit 不同 → workflow_version_invalid。
//   - ws 为空时仍生成 {"workflows":[]}：app.sh 据此清掉 user/default/workflows 里残留的 spark-*.json（移除最后一个工作流）。
func BundleFiles(ws []BundleWorkflow) ([]appdeploy.BundleFile, error) {
	if len(ws) > MaxWorkflowsPerDeployment {
		return nil, invalid("workflows", fmt.Sprintf("每个 ComfyUI 最多 %d 个工作流", MaxWorkflowsPerDeployment))
	}
	sorted := slices.Clone(ws)
	slices.SortStableFunc(sorted, func(a, b BundleWorkflow) int { return strings.Compare(a.Slug, b.Slug) })

	out := bundleManifest{Workflows: make([]bundleEntry, 0, len(sorted))}
	files := []appdeploy.BundleFile{{}} // [0] 留给 manifest.json
	type modelSeen struct {
		slug string
		m    ModelSpec
	}
	models := map[string]modelSeen{} // <dir>/<name> → 先出现的条目
	type nodeSeen struct {
		slug string
		n    NodeSpec
	}
	nodes := map[string]nodeSeen{}
	for i, w := range sorted {
		if !ValidSlug(w.Slug) {
			return nil, invalid("workflows", "工作流标识 "+w.Slug+" 不合法")
		}
		if i > 0 && sorted[i-1].Slug == w.Slug {
			return nil, invalid("workflows", "工作流 "+w.Slug+" 重复")
		}
		if !ValidVersion(w.Version) {
			return nil, invalid("workflows."+w.Slug+".version", "版本号不合法")
		}
		if len(w.Graph) == 0 || !json.Valid(w.Graph) {
			return nil, invalid("workflows."+w.Slug+".graph", "graph 不是合法 JSON")
		}
		// 纵深防御：app.sh 按空格分隔的行读清单，字段必须仍满足清单正则（版本入库时已校验，这里再查一遍）。
		if _, err := validateModels(w.Manifest.Models); err != nil {
			return nil, err
		}
		if _, err := validateNodes(w.Manifest.Nodes); err != nil {
			return nil, err
		}
		e := bundleEntry{Slug: w.Slug, Version: w.Version, File: w.Slug + ".json",
			Models: make([]ModelSpec, 0, len(w.Manifest.Models)), Nodes: make([]bundleNode, 0, len(w.Manifest.Nodes))}
		for _, md := range w.Manifest.Models {
			key := md.Dir + "/" + md.Name
			if first, ok := models[key]; ok {
				if first.m.SHA256 != "" && md.SHA256 != "" && first.m.SHA256 != md.SHA256 {
					return nil, invalid("manifest.models", fmt.Sprintf("模型 %s 在两个工作流里 sha256 不同（%s：%s，%s：%s）",
						key, first.slug, first.m.SHA256, w.Slug, md.SHA256))
				}
				md = first.m
			} else {
				models[key] = modelSeen{slug: w.Slug, m: md}
			}
			e.Models = append(e.Models, md)
		}
		for _, n := range w.Manifest.Nodes {
			if prev, ok := nodes[n.Name]; ok {
				if prev.n.Commit != n.Commit {
					return nil, invalid("manifest.nodes", fmt.Sprintf("节点 %s 在两个工作流里 commit 不同（%s：%s，%s：%s）",
						n.Name, prev.slug, prev.n.Commit, w.Slug, n.Commit))
				}
				if prev.n.Git != n.Git {
					return nil, invalid("manifest.nodes", fmt.Sprintf("节点 %s 在两个工作流里 Git 地址不同（%s：%s，%s：%s）",
						n.Name, prev.slug, prev.n.Git, w.Slug, n.Git))
				}
			} else {
				nodes[n.Name] = nodeSeen{slug: w.Slug, n: n}
			}
			e.Nodes = append(e.Nodes, bundleNode{Name: n.Name, Git: n.Git, Commit: n.Commit})
		}
		out.Workflows = append(out.Workflows, e)
		files = append(files, appdeploy.BundleFile{Name: BundleDir + "/" + e.File, Mode: 0o644, Body: w.Graph})
	}
	body, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	files[0] = appdeploy.BundleFile{Name: BundleManifestName, Mode: 0o644, Body: append(body, '\n')}
	return files, nil
}
