import type { WorkflowManifestPublic } from '@spark/shared'
import { readdirSync, readFileSync } from 'node:fs'
import { join } from 'node:path'
import { describe, expect, it } from 'vitest'
import {
  COMFY_MIN_RE, COMMIT_RE, collectRefs, compareNodeID, coverIssue, downloadGB, emptyInputRow, emptyManifest, emptyModelRow, FILE_SEG_RE,
  formToManifest, GIT_HOSTS, GIT_PATH_RE, gitIssue, INPUT_KEY_RE, INPUT_TYPES, manifestIssues, manifestToForm, missingRefs, MODEL_DIRS,
  MODEL_LOADERS, MODEL_NAME_RE, NODE_NAME_RE, NODE_TYPE_RE, OUTPUT_CLASS_TYPES, parseApiFile, parseGraphFile, parseTags, pathToCell,
  REPO_RE, SHA256_RE, SLUG_RE, splitFieldError, suggestInput, hasNaN, nonFiniteIssue, VERSION_RE, versionIssues, workflowIssue, type ApiNode, type ManifestForm,
} from '../workflowManifest'

const root = join(__dirname, '../../../../..')
const goSrc = (f: string) => readFileSync(join(root, 'internal/comfywf', f), 'utf8')
const wfDir = join(root, 'deploy/workflows')
const samples = readdirSync(wfDir, { withFileTypes: true })
  .filter((d) => d.isDirectory())
  .map((d) => ({
    slug: d.name,
    manifest: JSON.parse(readFileSync(join(wfDir, d.name, 'manifest.json'), 'utf8')) as WorkflowManifestPublic,
    graph: readFileSync(join(wfDir, d.name, 'graph.json'), 'utf8'),
    api: readFileSync(join(wfDir, d.name, 'api.json'), 'utf8'),
  }))

describe('与后端 internal/comfywf 保持一致', () => {
  const src = goSrc('manifest.go')
  const goRe = (name: string) => {
    const m = new RegExp(`${name}\\s*=\\s*regexp\\.MustCompile\\(\`([^\`]*)\`\\)`).exec(src)
    expect(m, name).not.toBeNull()
    return m![1]
  }
  it.each([
    ['slugRe', SLUG_RE], ['versionRe', VERSION_RE], ['comfyMinRe', COMFY_MIN_RE], ['modelNameRe', MODEL_NAME_RE], ['repoRe', REPO_RE],
    ['fileSegRe', FILE_SEG_RE], ['sha256Re', SHA256_RE], ['nodeNameRe', NODE_NAME_RE], ['gitPathRe', GIT_PATH_RE], ['commitRe', COMMIT_RE],
    ['nodeTypeRe', NODE_TYPE_RE], ['inputKeyRe', INPUT_KEY_RE],
  ] as const)('正则 %s 与 manifest.go 相同', (name, re) => {
    expect(re.source.replaceAll('\\/', '/')).toBe(goRe(name))
  })

  it('模型目录、Git 主机、输入类型与 consts.go 相同', () => {
    const consts = goSrc('consts.go')
    const list = (name: string) => {
      const m = new RegExp(`var ${name} = \\[\\]string\\{([^}]*)\\}`).exec(consts)
      expect(m, name).not.toBeNull()
      return [...m![1].matchAll(/"([^"]+)"/g)].map((x) => x[1])
    }
    expect([...MODEL_DIRS]).toEqual(list('ModelDirs'))
    expect([...GIT_HOSTS]).toEqual(list('GitHosts'))
    const inputConst = Object.fromEntries([...consts.matchAll(/(Input\w+)\s*=\s*"([a-z]+)"/g)].map((m) => [m[1], m[2]]))
    const types = /var InputTypes = \[\]string\{([^}]*)\}/.exec(consts)![1].split(',').map((s) => inputConst[s.trim()])
    expect([...INPUT_TYPES]).toEqual(types)
  })

  it('输出节点类型与模型加载节点表与 manifest.go 相同', () => {
    const outputs = /outputClassTypes = map\[string\]bool\{([^}]*)\}/.exec(src)![1]
    expect([...outputs.matchAll(/"(\w+)"/g)].map((m) => m[1]).sort()).toEqual([...OUTPUT_CLASS_TYPES].sort())
    const table = (name: string) => {
      const body = new RegExp(`var ${name} = map\\[string\\]\\[\\](?:int|string)\\{([\\s\\S]*?)\\n\\}`).exec(src)![1]
      return Object.fromEntries([...body.matchAll(/"(\w+)":\s*\{([^}]*)\}/g)].map((m) => [m[1], m[2].split(',').map((s) => s.trim().replaceAll('"', ''))]))
    }
    const widgets = table('modelLoaderWidgets')
    const inputs = table('modelLoaderInputs')
    const dirs = table('modelLoaderDirs')
    expect(Object.keys(MODEL_LOADERS).sort()).toEqual(Object.keys(widgets).sort())
    for (const [k, v] of Object.entries(MODEL_LOADERS)) {
      expect(v.widgets.map(String), k).toEqual(widgets[k])
      expect(v.inputs, k).toEqual(inputs[k])
      expect(v.dirs, k).toEqual(dirs[k])
    }
  })
})

describe('首批工作流（deploy/workflows）', () => {
  it('有 6 个样例', () => expect(samples).toHaveLength(6))

  it.each(samples.map((s) => [s.slug, s] as const))('%s：manifest 表单往返不变、即时校验无问题、D16 无缺失', (_slug, s) => {
    expect(formToManifest(manifestToForm(s.manifest))).toEqual(s.manifest)
    const g = parseGraphFile(s.graph)
    const a = parseApiFile(s.api)
    expect(g.ok, g.error).toBe(true)
    expect(a.ok, a.error).toBe(true)
    expect(g.nodes).toBeGreaterThan(0)
    expect(manifestIssues(s.manifest, a.nodes)).toEqual([])
    expect(missingRefs(s.manifest, collectRefs(g.value, a.nodes))).toEqual({ models: [], nodeTypes: [] })
  })

  it('sdxl-ultimate-upscale 的自定义节点：去掉 manifest.nodes 后列出缺失的节点类型', () => {
    const s = samples.find((x) => x.slug === 'sdxl-ultimate-upscale')!
    const refs = collectRefs(parseGraphFile(s.graph).value, parseApiFile(s.api).nodes)
    expect(refs.customTypes).toEqual(['UltimateSDUpscale'])
    expect(missingRefs({ ...s.manifest, nodes: [] }, refs).nodeTypes).toEqual(['UltimateSDUpscale'])
  })
})

// ---- 小样例 ----
const api: ApiNode[] = parseApiFile(JSON.stringify({
  10: { class_type: 'SaveImage', inputs: { images: ['8', 0], filename_prefix: 'x' } },
  3: { class_type: 'KSampler', inputs: { seed: 1, steps: 20, cfg: 7.5, sampler_name: 'euler', model: ['4', 0] } },
  4: { class_type: 'CheckpointLoaderSimple', inputs: { ckpt_name: 'a.safetensors' } },
  6: { class_type: 'CLIPTextEncode', inputs: { text: 'hi', clip: ['4', 1] } },
  7: { class_type: 'LoadImage', inputs: { image: 'x.png' } },
  8: { class_type: 'VAEDecode', inputs: { samples: ['3', 0], vae: ['4', 2] } },
})).nodes

const goodModel = { name: 'a.safetensors', dir: 'checkpoints', size_gb: 6.94, modelscope: { repo: 'o/r', file: 'a.safetensors' }, sha256: '' }
const manifest = (over: Partial<WorkflowManifestPublic> = {}): WorkflowManifestPublic => ({ ...emptyManifest(), models: [goodModel], ...over })
const paths = (m: WorkflowManifestPublic, a: ApiNode[] | null = api) => manifestIssues(m, a).map((i) => i.path)
const input = (over: Record<string, unknown>) => ({ key: 'k', label: 'L', type: 'int', node: '3', input: 'steps', min: 1, max: 100, ...over })

describe('parseApiFile / parseGraphFile', () => {
  it('api：节点按 id 数值排序，只列非连线入参', () => {
    expect(api.map((n) => n.id)).toEqual(['3', '4', '6', '7', '8', '10'])
    expect(api[0]).toMatchObject({ class_type: 'KSampler', inputs: ['seed', 'steps', 'cfg', 'sampler_name'] })
    expect(api.find((n) => n.id === '8')!.inputs).toEqual([])
  })
  it('api：空文本 / null = 无快速出图', () => {
    expect(parseApiFile('')).toMatchObject({ ok: true, empty: true, nodes: [], value: null })
    expect(parseApiFile(' null ')).toMatchObject({ ok: true, empty: true })
  })
  it.each([
    ['{', '不是合法的 JSON'],
    ['[]', '应为 API 格式'],
    ['{}', '至少要有一个节点'],
    ['{"nodes":[],"links":[]}', 'UI 格式'],
    ['{"3":{"inputs":{}}}', '缺少 class_type'],
    ['{"3":{"class_type":"X","inputs":[]}}', 'inputs 应为对象'],
  ])('api 错误：%s', (text, msg) => {
    const r = parseApiFile(text)
    expect(r.ok).toBe(false)
    expect(r.error).toContain(msg)
  })
  it('api：超过 256KB（紧凑形式）报错', () => {
    const big = JSON.stringify({ 1: { class_type: 'CLIPTextEncode', inputs: { text: 'x'.repeat(262144) } } })
    expect(parseApiFile(big).error).toContain('256 KB')
  })
  it('graph：节点数与紧凑字节数；缺 nodes / links 报错', () => {
    const text = JSON.stringify({ nodes: [{ id: 1, type: 'KSampler' }, { id: 2, type: 'VAEDecode' }], links: [] }, null, 2)
    const g = parseGraphFile(text)
    expect(g).toMatchObject({ ok: true, nodes: 2 })
    expect(g.bytes).toBe(JSON.stringify(JSON.parse(text)).length)
    expect(g.bytes).toBeLessThan(text.length)
    expect(parseGraphFile('')).toMatchObject({ ok: false, empty: true, error: '不能为空' })
    expect(parseGraphFile('{"links":[]}').error).toContain('nodes')
    expect(parseGraphFile('{"nodes":[]}').error).toContain('links')
    expect(parseGraphFile('nope').error).toBe('不是合法的 JSON')
  })
})

describe('collectRefs / missingRefs（D16）', () => {
  const graph = {
    nodes: [
      { id: 4, type: 'CheckpointLoaderSimple', widgets_values: ['a.safetensors'], properties: { cnr_id: 'comfy-core' } },
      { id: 5, type: 'DualCLIPLoader', widgets_values: ['t5.safetensors', 'clip_l.safetensors', 'flux'] },
      { id: 6, type: 'LoraLoader', mode: 4, widgets_values: ['bypassed.safetensors', 1, 1] },
      { id: 7, type: 'UltimateSDUpscale', properties: { cnr_id: 'comfyui_ultimatesdupscale' } },
      { id: 8, type: 'Foo', properties: { aux_id: 'someone/foo' } },
    ],
    links: [],
    definitions: { subgraphs: [{ id: 'x', nodes: [{ id: 1, type: 'VAELoader', widgets_values: { vae_name: 'ae.safetensors' } }] }] },
  }
  it('列出全部缺失的模型（含子图、对象形式的 widgets_values、api_prompt），跳过旁路节点；建议目录取加载节点的第一个', () => {
    const refs = collectRefs(graph, parseApiFile('{"9":{"class_type":"UpscaleModelLoader","inputs":{"model_name":"esr.pth"}}}').nodes)
    const miss = missingRefs(manifest(), refs)
    expect(miss.models.map((m) => [m.name, m.dir])).toEqual([
      ['t5.safetensors', 'text_encoders'], ['clip_l.safetensors', 'text_encoders'], ['ae.safetensors', 'vae'], ['esr.pth', 'upscale_models'],
    ])
    expect(miss.nodeTypes).toEqual(['UltimateSDUpscale', 'Foo'])
  })
  it('声明了 node_types 的节点不再列出', () => {
    const m = manifest({ nodes: [{ name: 'usd', git: 'https://github.com/a/b', commit: 'a'.repeat(40), node_types: ['UltimateSDUpscale'] }] })
    expect(missingRefs(m, collectRefs(graph, [])).nodeTypes).toEqual(['Foo'])
  })
})

describe('manifestToForm / formToManifest', () => {
  it('去空白、dir / sha256 / commit 小写；空的来源不写；节点类型逗号或换行分隔', () => {
    const f: ManifestForm = {
      comfyui_min: ' 0.3.60 ',
      models: [{ ...emptyModelRow(), name: ' a.safetensors ', dir: 'CheckPoints', size_gb: '6.9', hf_repo: ' o/r ', hf_file: 'a.safetensors', sha256: 'AB'.repeat(32) }],
      nodes: [{ name: 'n', git: 'https://github.com/a/b.git', commit: 'F'.repeat(40), node_types: 'A, B\nC，D' }],
      inputs: [],
      outputs: [],
    }
    expect(formToManifest(f)).toEqual({
      comfyui_min: '0.3.60',
      models: [{ name: 'a.safetensors', dir: 'checkpoints', size_gb: 6.9, hf: { repo: 'o/r', file: 'a.safetensors' }, sha256: 'ab'.repeat(32) }],
      nodes: [{ name: 'n', git: 'https://github.com/a/b.git', commit: 'f'.repeat(40), node_types: ['A', 'B', 'C', 'D'] }],
      inputs: [],
      outputs: [],
    })
  })
  it('输入：按类型取列；数字转换；bool 默认值；text 空默认值保留为空串；image 无默认值', () => {
    const row = (over: Partial<ReturnType<typeof emptyInputRow>>) => ({ ...emptyInputRow(), key: 'k', label: 'L', node: '3', input: 'x', ...over })
    const out = formToManifest({
      comfyui_min: '', models: [], nodes: [], outputs: [],
      inputs: [
        row({ type: 'int', default: '20', min: '1', max: '100', step: '', max_len: '99', options: 'a' }),
        row({ type: 'float', default: '7.5', min: '1', max: '30', step: '0.5' }),
        row({ type: 'textarea', default: '', max_len: '2000', min: '1' }),
        row({ type: 'enum', default: 'euler', options: 'euler\n\n dpmpp_2m \n' }),
        row({ type: 'bool', default: 'true' }),
        row({ type: 'seed', default: '' }),
        row({ type: 'image', default: 'x.png' }),
      ],
    }).inputs
    expect(out).toEqual([
      { key: 'k', label: 'L', type: 'int', node: '3', input: 'x', default: 20, min: 1, max: 100 },
      { key: 'k', label: 'L', type: 'float', node: '3', input: 'x', default: 7.5, min: 1, max: 30, step: 0.5 },
      { key: 'k', label: 'L', type: 'textarea', node: '3', input: 'x', default: '', max_len: 2000 },
      { key: 'k', label: 'L', type: 'enum', node: '3', input: 'x', default: 'euler', options: ['euler', 'dpmpp_2m'] },
      { key: 'k', label: 'L', type: 'bool', node: '3', input: 'x', default: true },
      { key: 'k', label: 'L', type: 'seed', node: '3', input: 'x' },
      { key: 'k', label: 'L', type: 'image', node: '3', input: 'x' },
    ])
  })
  it('非数字的大小 / 范围变成 NaN，由即时校验报出', () => {
    const m = formToManifest({ comfyui_min: '', models: [{ ...emptyModelRow(), name: 'a.safetensors', size_gb: 'abc', ms_repo: 'o/r', ms_file: 'a' }], nodes: [], inputs: [], outputs: [] })
    expect(m.models[0].size_gb).toBeNaN()
    expect(paths(m)).toEqual(['manifest.models[0].size_gb'])
  })
})

describe('manifestIssues', () => {
  it('合法的最小清单无问题', () => expect(manifestIssues(manifest(), api)).toEqual([]))

  it.each([
    [{ comfyui_min: '0.3' }, 'manifest.comfyui_min'],
    [{ models: [{ ...goodModel, name: 'a/b.safetensors' }] }, 'manifest.models[0].name'],
    [{ models: [goodModel, goodModel] }, 'manifest.models[1].name'],
    [{ models: [{ ...goodModel, dir: 'models' }] }, 'manifest.models[0].dir'],
    [{ models: [{ ...goodModel, size_gb: 0 }] }, 'manifest.models[0].size_gb'],
    [{ models: [{ ...goodModel, size_gb: 201 }] }, 'manifest.models[0].size_gb'],
    [{ models: [{ ...goodModel, modelscope: undefined }] }, 'manifest.models[0]'],
    [{ models: [{ ...goodModel, modelscope: { repo: 'norepo', file: 'a' } }] }, 'manifest.models[0].modelscope.repo'],
    [{ models: [{ ...goodModel, hf: { repo: 'o/r', file: '../a' } }] }, 'manifest.models[0].hf.file'],
    [{ models: [{ ...goodModel, sha256: 'abc' }] }, 'manifest.models[0].sha256'],
    [{ models: Array.from({ length: 17 }, (_, i) => ({ ...goodModel, name: `m${i}` })) }, 'manifest.models'],
    [{ nodes: [{ name: '.x', git: 'https://github.com/a/b', commit: 'a'.repeat(40), node_types: ['T'] }] }, 'manifest.nodes[0].name'],
    [{ nodes: [{ name: 'x', git: 'https://evil.com/a/b', commit: 'a'.repeat(40), node_types: ['T'] }] }, 'manifest.nodes[0].git'],
    [{ nodes: [{ name: 'x', git: 'https://github.com/a/b', commit: 'abc', node_types: ['T'] }] }, 'manifest.nodes[0].commit'],
    [{ nodes: [{ name: 'x', git: 'https://github.com/a/b', commit: 'a'.repeat(40), node_types: [] }] }, 'manifest.nodes[0].node_types'],
    [{ nodes: [{ name: 'x', git: 'https://github.com/a/b', commit: 'a'.repeat(40), node_types: ['ok', 'bad/type'] }] }, 'manifest.nodes[0].node_types[1]'],
  ] as [Partial<WorkflowManifestPublic>, string][])('%j → %s', (over, path) => {
    expect(paths(manifest(over))).toEqual([path])
  })

  it('gitIssue：https、白名单主机、/owner/repo(.git)', () => {
    expect(gitIssue('https://github.com/ssitu/ComfyUI_UltimateSDUpscale.git')).toBe('')
    expect(gitIssue('https://gitee.com/a/b')).toBe('')
    expect(gitIssue('http://github.com/a/b')).toContain('https')
    expect(gitIssue('https://user@github.com/a/b')).toContain('用户名')
    expect(gitIssue('https://github.com:8443/a/b')).toContain('端口')
    expect(gitIssue('https://github.com/a/b?x=1')).toContain('查询参数')
    expect(gitIssue('https://gitlab.com/a/b')).toContain('github.com')
    expect(gitIssue('https://github.com/a/b/c')).toContain('/owner/repo')
    expect(gitIssue('https://github.com/.a/b')).toContain('不能以 .')
  })

  it.each([
    [input({ key: 'Bad' }), 'manifest.inputs[0].key'],
    [input({ label: '' }), 'manifest.inputs[0].label'],
    [input({ type: 'color' }), 'manifest.inputs[0].type'],
    [input({ node: '99' }), 'manifest.inputs[0].node'],
    [input({ input: 'nope' }), 'manifest.inputs[0].input'],
    [input({ input: 'model' }), 'manifest.inputs[0].input'], // 连线
    [input({ type: 'text', input: 'steps', min: undefined, max: undefined }), 'manifest.inputs[0].input'], // 现值类型不符
    [input({ min: undefined }), 'manifest.inputs[0].min'],
    [input({ max: undefined }), 'manifest.inputs[0].max'],
    [input({ min: 10, max: 1 }), 'manifest.inputs[0].min'],
    [input({ min: 1.5 }), 'manifest.inputs[0].min'],
    [input({ step: 0 }), 'manifest.inputs[0].step'],
    [input({ default: 101 }), 'manifest.inputs[0].default'],
    [input({ default: 4, min: 1, step: 2 }), 'manifest.inputs[0].default'],
    [input({ type: 'enum', input: 'sampler_name', min: undefined, max: undefined, options: [] }), 'manifest.inputs[0].options'],
    [input({ type: 'enum', input: 'sampler_name', min: undefined, max: undefined, options: ['a', 'a'] }), 'manifest.inputs[0].options[1]'],
    [input({ type: 'enum', input: 'sampler_name', min: undefined, max: undefined, options: ['a'], default: 'b' }), 'manifest.inputs[0].default'],
    [input({ type: 'text', node: '4', input: 'ckpt_name', min: undefined, max: undefined }), 'manifest.inputs[0].type'], // 模型文件名只能 enum
    [input({ type: 'textarea', node: '6', input: 'text', min: undefined, max: undefined, max_len: 5000 }), 'manifest.inputs[0].max_len'],
    [input({ type: 'seed', input: 'seed', min: undefined, max: undefined, default: -2 }), 'manifest.inputs[0].default'],
  ])('输入 %j → %s', (inp, path) => {
    expect(paths(manifest({ inputs: [inp as never] }))).toEqual([path])
  })

  it('合法的各类输入', () => {
    const inputs = [
      input({ default: 20 }),
      input({ key: 'cfg', type: 'float', input: 'cfg', min: 1, max: 30, step: 0.5, default: 7.5 }),
      input({ key: 'seed', type: 'seed', input: 'seed', min: undefined, max: undefined, default: -1 }),
      input({ key: 'sampler', type: 'enum', input: 'sampler_name', min: undefined, max: undefined, options: ['euler', 'dpmpp_2m'], default: 'euler' }),
      input({ key: 'prompt', type: 'textarea', node: '6', input: 'text', min: undefined, max: undefined, max_len: 2000, default: '' }),
      input({ key: 'image', type: 'image', node: '7', input: 'image', min: undefined, max: undefined }),
    ]
    expect(manifestIssues(manifest({ inputs: inputs as never, outputs: [{ node: '10' }] }), api)).toEqual([])
  })

  it('同一个 node.input 不能给两个参数；image 最多 2 个', () => {
    expect(paths(manifest({ inputs: [input({}), input({ key: 'k2' })] as never }))).toEqual(['manifest.inputs[1].input'])
    const img = (k: string) => input({ key: k, type: 'image', node: '7', input: 'image', min: undefined, max: undefined })
    const api2 = parseApiFile(JSON.stringify({ 7: { class_type: 'LoadImage', inputs: { image: 'a', image2: 'b', image3: 'c' } } })).nodes
    const m = manifest({ inputs: [img('a'), { ...img('b'), input: 'image2' }, { ...img('c'), input: 'image3' }] as never })
    expect(paths(m, api2)).toEqual(['manifest.inputs'])
  })

  it('outputs：须是 api_prompt 里保存图片 / 视频的节点，不能重复', () => {
    expect(paths(manifest({ outputs: [{ node: '10' }] }))).toEqual([])
    expect(paths(manifest({ outputs: [{ node: '3' }] }))).toEqual(['manifest.outputs[0].node'])
    expect(paths(manifest({ outputs: [{ node: '99' }] }))).toEqual(['manifest.outputs[0].node'])
    expect(paths(manifest({ outputs: [{ node: '10' }, { node: '10' }] }))).toEqual(['manifest.outputs[1].node'])
  })

  it('没有 api_prompt 时不能有 inputs / outputs', () => {
    expect(paths(manifest({ inputs: [input({})] as never, outputs: [{ node: '10' }] }), null)).toEqual(['manifest.inputs', 'manifest.outputs'])
  })
})

describe('versionIssues', () => {
  const ok = { version: '1.0.0', changelog: '', graph: parseGraphFile('{"nodes":[],"links":[]}'), api: parseApiFile('') }
  it('合法', () => expect(versionIssues(ok)).toEqual([]))
  it('版本号、更新说明、graph、api_prompt', () => {
    expect(versionIssues({ ...ok, version: '-1' }).map((i) => i.path)).toEqual(['version'])
    expect(versionIssues({ ...ok, changelog: '字'.repeat(1001) }).map((i) => i.path)).toEqual(['changelog'])
    expect(versionIssues({ ...ok, graph: parseGraphFile('') })).toEqual([{ path: 'graph', reason: '不能为空' }])
    expect(versionIssues({ ...ok, api: parseApiFile('[]') }).map((i) => i.path)).toEqual(['api_prompt'])
  })
})

describe('downloadGB', () => {
  it('Σ size_gb 两位小数；非数字不计', () => {
    expect(downloadGB([{ size_gb: 6.94 }, { size_gb: 0.07 }, { size_gb: '0.33' }])).toBe(7.34)
    expect(downloadGB([{ size_gb: 'x' }, { size_gb: 1.006 }])).toBe(1.01)
    expect(downloadGB([])).toBe(0)
  })
})

describe('splitFieldError / pathToCell', () => {
  it.each([
    ['manifest.models[0].modelscope.repo：仓库应形如 owner/name', 'manifest.models[0].modelscope.repo', '仓库应形如 owner/name'],
    ['graph.nodes[3]：节点 #4 UltimateSDUpscale 不是内置节点，须在 manifest.nodes[].node_types 里声明', 'graph.nodes[3]', '节点 #4 UltimateSDUpscale 不是内置节点，须在 manifest.nodes[].node_types 里声明'],
    ['api_prompt.12:3.class_type：缺少 class_type', 'api_prompt.12:3.class_type', '缺少 class_type'],
    ['version：只能由字母…', 'version', '只能由字母…'],
    ['manifest.inputs：最多 24 个参数', 'manifest.inputs', '最多 24 个参数'],
  ])('%s', (msg, path, reason) => {
    expect(splitFieldError(msg)).toEqual({ path, reason })
  })
  it('不是「路径：原因」格式返回 null', () => {
    expect(splitFieldError('版本号已存在')).toBeNull()
    expect(splitFieldError('已发布的工作流版本不能修改')).toBeNull()
  })
  it.each([
    ['manifest.models[2].modelscope.repo', { table: 'models', index: 2, field: 'ms_repo' }],
    ['manifest.models[0].hf.file', { table: 'models', index: 0, field: 'hf_file' }],
    ['manifest.models[1]', { table: 'models', index: 1, field: '' }],
    ['manifest.nodes[0].node_types[3]', { table: 'nodes', index: 0, field: 'node_types' }],
    ['manifest.inputs[5].options[1]', { table: 'inputs', index: 5, field: 'options' }],
    ['manifest.outputs[0].node', { table: 'outputs', index: 0, field: 'node' }],
  ] as const)('%s → 单元格', (path, cell) => {
    expect(pathToCell(path)).toEqual(cell)
  })
  it('不是表格行返回 null', () => {
    expect(pathToCell('manifest.inputs')).toBeNull()
    expect(pathToCell('graph.nodes[0]')).toBeNull()
  })
})

describe('工作流基本信息与封面', () => {
  const base = { slug: 'sdxl-txt2img', name: 'SDXL 文生图', summary: '', description: '', category: 'txt2img' as const, tags: 'SDXL, 文生图', sort: 10 }
  it('合法', () => expect(workflowIssue(base, true)).toBe(''))
  it.each([
    [{ slug: 'A' }, 'slug'],
    [{ slug: 'a' }, 'slug'],
    [{ name: ' ' }, '名称'],
    [{ name: '名'.repeat(49) }, '名称'],
    [{ summary: '简'.repeat(121) }, '简介'],
    [{ tags: 'a,b,c,d,e,f,g,h,i' }, '标签最多 8 个'],
    [{ tags: '标'.repeat(17) }, '超过 16 个字符'],
    [{ sort: 10001 }, '排序'],
  ])('%j → %s', (over, msg) => {
    expect(workflowIssue({ ...base, ...over }, true)).toContain(msg)
  })
  it('编辑时不查 slug', () => expect(workflowIssue({ ...base, slug: '' }, false)).toBe(''))
  it('parseTags：中英文逗号分隔、去空白去重', () => expect(parseTags(' a ,b，a,, c ')).toEqual(['a', 'b', 'c']))
  it('coverIssue：PNG / JPG / WebP ≤ 2MB', () => {
    expect(coverIssue({ type: 'image/png', size: 2 << 20 })).toBe('')
    expect(coverIssue({ type: 'image/webp', size: 1 })).toBe('')
    expect(coverIssue({ type: 'image/gif', size: 1 })).toContain('PNG')
    expect(coverIssue({ type: 'image/jpeg', size: (2 << 20) + 1 })).toContain('2MB')
  })
})

describe('compareNodeID', () => {
  it('数字按数值，其余字典序', () => {
    expect(['10', '9', 'b', '3', 'a'].sort(compareNodeID)).toEqual(['3', '9', '10', 'a', 'b'])
  })
})

describe('suggestInput / hasNaN', () => {
  const node = (id: string) => api.find((n) => n.id === id)
  const row = (input: string, over = {}) => ({ ...emptyInputRow(), node: '3', input, ...over })
  it('按现值猜类型、填 key / 名称 / 默认值 / 范围（只填空着的格子）', () => {
    expect(suggestInput(row('steps'), node('3'))).toEqual({ key: 'steps', label: 'steps', type: 'int', default: '20', min: '0', max: '100' })
    expect(suggestInput(row('cfg', { key: 'c', label: 'CFG', min: '1' }), node('3'))).toEqual({ type: 'float', default: '7.5', max: '30' })
    expect(suggestInput(row('seed'), node('3'))).toMatchObject({ type: 'seed', default: '' })
    expect(suggestInput(row('sampler_name'), node('3'))).toMatchObject({ type: 'text', default: 'euler' })
    expect(suggestInput(row('image', { node: '7' }), node('7'))).toMatchObject({ key: 'image', type: 'image', default: '' })
    expect(suggestInput(row('ckpt_name', { node: '4' }), node('4'))).toMatchObject({ type: 'enum', default: 'a.safetensors', options: 'a.safetensors' })
    expect(suggestInput(row('nope'), node('3'))).toEqual({})
    expect(suggestInput(row('steps'), undefined)).toEqual({})
  })
  it('hasNaN：大小 / 范围 / 默认值不是数字时不能提交', () => {
    expect(hasNaN(manifest())).toBe(false)
    expect(hasNaN(manifest({ models: [{ ...goodModel, size_gb: NaN }] }))).toBe(true)
    expect(hasNaN(manifest({ inputs: [input({ min: NaN })] as never }))).toBe(true)
  })
})

describe('T14 评审修正', () => {
  it('I-1：text / textarea 没有默认值（必填）往返后仍没有默认值；有空串默认值的仍是空串', () => {
    const m = manifest({
      inputs: [
        { key: 'a', label: 'A', type: 'textarea', node: '6', input: 'text', max_len: 1000 },
        { key: 'b', label: 'B', type: 'text', node: '6', input: 'text', default: '', max_len: 1000 },
      ],
    })
    const f = manifestToForm(m)
    expect(f.inputs.map((r) => r.required)).toEqual([true, false])
    expect(formToManifest(f)).toEqual(m)
    expect('default' in formToManifest(f).inputs[0]).toBe(false)
  })
  it('I-1：必填的 text 即使表单里残留默认值也不写 default；其它类型 required 无影响', () => {
    const out = formToManifest({
      comfyui_min: '', models: [], nodes: [], outputs: [],
      inputs: [
        { ...emptyInputRow(), key: 'a', label: 'A', type: 'text', node: '6', input: 'text', default: 'x', required: true },
        { ...emptyInputRow(), key: 'b', label: 'B', type: 'int', node: '3', input: 'steps', default: '5', min: '1', max: '9', required: true },
      ],
    }).inputs
    expect('default' in out[0]).toBe(false)
    expect(out[1].default).toBe(5)
    expect(emptyInputRow().required).toBe(false)
  })
  it('非有限数（Infinity）也当作不能提交，并给出具体字段', () => {
    expect(hasNaN(manifest({ models: [{ ...goodModel, size_gb: Infinity }] }))).toBe(true)
    expect(nonFiniteIssue(manifest({ models: [{ ...goodModel, size_gb: Infinity }] }))).toEqual({ path: 'manifest.models[0].size_gb', reason: '需为数字' })
    expect(nonFiniteIssue(manifest({ models: [{ ...goodModel, size_gb: NaN }] }))).toEqual({ path: 'manifest.models[0].size_gb', reason: '请填写大小（GB）' })
    expect(nonFiniteIssue(manifest({ inputs: [input({ max: NaN })] as never }))).toEqual({ path: 'manifest.inputs[0].max', reason: '需为数字' })
    expect(nonFiniteIssue(manifest())).toBeNull()
    expect(paths(manifest({ inputs: [input({ max: Infinity })] as never }))).toEqual(['manifest.inputs[0].max'])
    expect(paths(manifest({ models: [{ ...goodModel, size_gb: Infinity }] }))).toEqual(['manifest.models[0].size_gb'])
  })
  it('suggestInput：文本输入建议 max_len（不小于现值长度，上限 4000）', () => {
    const long = parseApiFile(JSON.stringify({ 6: { class_type: 'CLIPTextEncode', inputs: { text: 'x'.repeat(1500) } } })).nodes[0]
    expect(suggestInput({ ...emptyInputRow(), node: '6', input: 'text' }, long)).toMatchObject({ type: 'text', max_len: '1500' })
    const short = api.find((n) => n.id === '6')
    expect(suggestInput({ ...emptyInputRow(), node: '6', input: 'text' }, short)).toMatchObject({ type: 'text', max_len: '1000' })
    expect(suggestInput({ ...emptyInputRow(), node: '6', input: 'text', max_len: '200' }, short).max_len).toBeUndefined()
  })
})
