import { ApiError, type QuickRun, type WorkflowInput } from '@spark/shared'
import { describe, expect, it } from 'vitest'
import {
  MAX_SEED, buildRunInputs, defaultValues, fileKind, inputRequired, pollDeadlineMs, pollDelayMs, pollExpired, quickRunErrorText, runFinished,
  runStatusText, safeFileURL, validateValue, validateValues,
} from '../quickrun'

const inp = (over: Partial<WorkflowInput> & Pick<WorkflowInput, 'key' | 'type'>): WorkflowInput => ({ label: over.key, node: '1', input: over.key, ...over })

// 与规格 §3.2 的示例清单一致
const inputs: WorkflowInput[] = [
  inp({ key: 'prompt', label: '提示词', type: 'textarea', default: 'a photo of a cat', max_len: 2000 }),
  inp({ key: 'negative', label: '反向提示词', type: 'textarea', default: '', max_len: 2000 }),
  inp({ key: 'width', label: '宽', type: 'int', default: 1024, min: 512, max: 2048, step: 64 }),
  inp({ key: 'steps', label: '步数', type: 'int', default: 25, min: 1, max: 100 }),
  inp({ key: 'cfg', label: 'CFG', type: 'float', default: 7, min: 1, max: 30 }),
  inp({ key: 'sampler', label: '采样器', type: 'enum', default: 'euler', options: ['euler', 'dpmpp_2m'] }),
  inp({ key: 'hires', label: '高清', type: 'bool', default: true }),
  inp({ key: 'seed', label: '种子', type: 'seed' }),
  inp({ key: 'image', label: '参考图', type: 'image' }),
]

const run = (over: Partial<QuickRun> = {}): QuickRun => ({
  id: 'r1', prompt_id: 'p1', seed: 42, state: 'queued', files: [], message: '', created_at: '2026-10-24T10:00:00Z', finished_at: null, ...over,
})

describe('常量', () => {
  it('2 秒轮询、30 分钟上限（D10）', () => {
    expect(pollDelayMs).toBe(2000)
    expect(pollDeadlineMs).toBe(30 * 60 * 1000)
    expect(MAX_SEED).toBe(2 ** 53 - 1)
  })
  it('pollExpired', () => {
    expect(pollExpired(0, pollDeadlineMs - 1)).toBe(false)
    expect(pollExpired(0, pollDeadlineMs)).toBe(true)
  })
})

describe('defaultValues', () => {
  it('按类型取默认值；seed 为空 = 随机；没有默认值的 bool 为 false，其余为空', () => {
    expect(defaultValues(inputs)).toEqual({
      prompt: 'a photo of a cat', negative: '', width: 1024, steps: 25, cfg: 7, sampler: 'euler', hires: true, seed: '', image: '',
    })
  })
  it('无默认值', () => {
    const v = defaultValues([inp({ key: 't', type: 'text' }), inp({ key: 'n', type: 'int', min: 1, max: 2 }), inp({ key: 'b', type: 'bool' }), inp({ key: 'e', type: 'enum', options: ['a'] })])
    expect(v).toEqual({ t: '', n: null, b: false, e: '' })
  })
  it('seed 有默认值时回显（-1 也显示为空 = 随机）', () => {
    expect(defaultValues([inp({ key: 's', type: 'seed', default: 7 })])).toEqual({ s: '7' })
    expect(defaultValues([inp({ key: 's', type: 'seed', default: -1 })])).toEqual({ s: '' })
  })
})

describe('validateValue（与后端 BuildPrompt / coerceValue 一致）', () => {
  const by = (k: string) => inputs.find((i) => i.key === k)!
  it('int：整数、区间、step 从 min 起对齐', () => {
    expect(validateValue(by('width'), 1024)).toBeNull()
    expect(validateValue(by('width'), 512)).toBeNull()
    expect(validateValue(by('width'), 2048)).toBeNull()
    expect(validateValue(by('width'), 1000)).toBe('必须是 512 加 64 的整数倍')
    expect(validateValue(by('width'), 448)).toBe('应在 512～2048')
    expect(validateValue(by('width'), 2112)).toBe('应在 512～2048')
    expect(validateValue(by('steps'), 2.5)).toBe('必须是整数')
    expect(validateValue(by('steps'), 'x')).toBe('必须是整数')
    // Wan22 length：min 1、step 4 → 1、5、9…
    const len = inp({ key: 'length', type: 'int', default: 81, min: 1, max: 121, step: 4 })
    expect(validateValue(len, 81)).toBeNull()
    expect(validateValue(len, 80)).toBe('必须是 1 加 4 的整数倍')
  })
  it('float：数字与区间', () => {
    expect(validateValue(by('cfg'), 7.5)).toBeNull()
    expect(validateValue(by('cfg'), 0.5)).toBe('应在 1～30')
    expect(validateValue(by('cfg'), Number.NaN)).toBe('必须是数字')
  })
  it('text / textarea：max_len（默认 1000，按字符数）、NUL', () => {
    expect(validateValue(by('prompt'), '猫'.repeat(2000))).toBeNull()
    expect(validateValue(by('prompt'), '猫'.repeat(2001))).toBe('不能超过 2000 字')
    const t = inp({ key: 't', type: 'text', default: '' })
    expect(validateValue(t, 'a'.repeat(1000))).toBeNull()
    expect(validateValue(t, 'a'.repeat(1001))).toBe('不能超过 1000 字')
    expect(validateValue(t, '😀'.repeat(1000))).toBeNull() // 代理对按一个字符计（Go utf8.RuneCountInString）
    expect(validateValue(t, 'a\0b')).toBe('含非法字符')
    expect(validateValue(t, 3)).toBe('必须是文本')
  })
  it('enum / bool', () => {
    expect(validateValue(by('sampler'), 'dpmpp_2m')).toBeNull()
    expect(validateValue(by('sampler'), 'ddim')).toBe('不是可选值')
    expect(validateValue(by('hires'), false)).toBeNull()
    expect(validateValue(by('hires'), 'true')).toBe('必须是布尔值')
  })
  it('seed：空 / -1 = 随机；否则 0～2^53-1 的整数（D11）', () => {
    const s = by('seed')
    for (const v of ['', '  ', '-1', -1, null, '0', '123', 123, String(2 ** 53 - 1)]) expect(validateValue(s, v)).toBeNull()
    for (const v of ['-2', '1.5', 'abc', String(2 ** 53), '9007199254740993', -2, 1.5]) {
      expect(validateValue(s, v)).toBe('种子应为 -1（随机）或 0～9007199254740991 的整数')
    }
  })
  it('image：须是上传接口返回的名字', () => {
    const im = by('image')
    expect(validateValue(im, 'spark-quickrun/abcdef012345.png')).toBeNull()
    expect(validateValue(im, 'spark-quickrun/abcdef012345.webp')).toBeNull()
    expect(validateValue(im, 'spark-quickrun/abcdef012345.gif')).toBe('请先上传图片')
    expect(validateValue(im, '../x.png')).toBe('请先上传图片')
    expect(validateValue(im, '')).toBe('必填')
  })
  it('没有默认值的输入必填（seed、文本除外）；有默认值时留空用默认', () => {
    // 文本：空串是合法值（后端 coerceValue 接受 ""），表单总会发送字符串
    expect(validateValue(inp({ key: 't', type: 'text' }), '')).toBeNull()
    expect(validateValue(inp({ key: 't', type: 'textarea' }), '')).toBeNull()
    expect(validateValue(inp({ key: 't', type: 'text' }), null)).toBe('必填')
    expect(validateValue(inp({ key: 'n', type: 'int', min: 1, max: 9 }), null)).toBe('必填')
    expect(validateValue(inp({ key: 'e', type: 'enum', options: ['a'] }), '')).toBe('必填')
    expect(validateValue(by('steps'), null)).toBeNull()
    expect(validateValue(by('sampler'), '')).toBeNull()
    expect(validateValue(by('negative'), '')).toBeNull()
  })
})

describe('inputRequired（表单星号）', () => {
  it('无默认值的数字 / 枚举 / 图片必填；文本、bool、seed 不标', () => {
    expect(inputRequired(inp({ key: 'n', type: 'int', min: 1, max: 2 }))).toBe(true)
    expect(inputRequired(inp({ key: 'e', type: 'enum', options: ['a'] }))).toBe(true)
    expect(inputRequired(inp({ key: 'i', type: 'image' }))).toBe(true)
    expect(inputRequired(inp({ key: 't', type: 'text' }))).toBe(false)
    expect(inputRequired(inp({ key: 't', type: 'textarea' }))).toBe(false)
    expect(inputRequired(inp({ key: 'b', type: 'bool' }))).toBe(false)
    expect(inputRequired(inp({ key: 's', type: 'seed' }))).toBe(false)
    expect(inputRequired(inp({ key: 'n', type: 'int', default: 1, min: 1, max: 2 }))).toBe(false)
  })
})

describe('safeFileURL', () => {
  it('只接受本站的运行文件代理地址', () => {
    expect(safeFileURL('/api/v1/instances/i/apps/d/workflows/5/runs/r/files/0')).toBe('/api/v1/instances/i/apps/d/workflows/5/runs/r/files/0')
    expect(safeFileURL('https://evil.example/x.png')).toBe('')
    expect(safeFileURL('//evil.example/api/v1/instances/x')).toBe('')
    expect(safeFileURL('javascript:alert(1)')).toBe('')
    expect(safeFileURL('')).toBe('')
  })
})

describe('validateValues', () => {
  it('默认值（除必填的 image 外）全部通过', () => {
    const v = defaultValues(inputs)
    expect(validateValues(inputs, v)).toEqual({ ok: false, errors: { image: '必填' } })
    v.image = 'spark-quickrun/abcdef012345.jpg'
    expect(validateValues(inputs, v)).toEqual({ ok: true, errors: {} })
  })
  it('多项错误', () => {
    const v = { ...defaultValues(inputs), width: 100, sampler: 'x', seed: 'abc', image: 'spark-quickrun/abcdef012345.jpg' }
    const r = validateValues(inputs, v)
    expect(r.ok).toBe(false)
    expect(Object.keys(r.errors).sort()).toEqual(['sampler', 'seed', 'width'])
  })
})

describe('buildRunInputs', () => {
  it('留空且有默认的数字 / 枚举不发送（服务器补默认）；seed 空 = 不发送（随机），否则发数字；文本照发', () => {
    const v = { ...defaultValues(inputs), steps: null, sampler: '', negative: '', image: 'spark-quickrun/abcdef012345.png' }
    expect(buildRunInputs(inputs, v)).toEqual({
      prompt: 'a photo of a cat', negative: '', width: 1024, cfg: 7, hires: true, image: 'spark-quickrun/abcdef012345.png',
    })
    expect(buildRunInputs(inputs, { ...v, seed: '-1' }).seed).toBe(-1)
    expect(buildRunInputs(inputs, { ...v, seed: ' 123 ' }).seed).toBe(123)
  })
  it('只发送表单里的 key（后端对未知参数报错）', () => {
    expect(Object.keys(buildRunInputs([inp({ key: 'a', type: 'text', default: 'x' })], { a: 'y', other: 1 }))).toEqual(['a'])
  })
})

describe('runStatusText', () => {
  it('四种状态', () => {
    expect(runStatusText(run())).toBe('排队中')
    expect(runStatusText(run({ state: 'running' }))).toBe('生成中')
    expect(runStatusText(run({ state: 'done', finished_at: '2026-10-24T10:00:12.400Z' }))).toBe('完成（12 秒）')
    expect(runStatusText(run({ state: 'done' }))).toBe('完成')
    expect(runStatusText(run({ state: 'error', message: 'CUDA out of memory' }))).toBe('失败：CUDA out of memory')
    expect(runStatusText(run({ state: 'error' }))).toBe('失败')
  })
  it('超过一分钟显示分秒', () => {
    expect(runStatusText(run({ state: 'done', finished_at: '2026-10-24T10:02:05Z' }))).toBe('完成（2 分 5 秒）')
  })
  it('runFinished', () => {
    expect(runFinished(run())).toBe(false)
    expect(runFinished(run({ state: 'running' }))).toBe(false)
    expect(runFinished(run({ state: 'done' }))).toBe(true)
    expect(runFinished(run({ state: 'error' }))).toBe(true)
  })
})

describe('fileKind', () => {
  it('以后端 kind 为准，未知时按扩展名', () => {
    expect(fileKind({ kind: 'video', filename: 'a.png' })).toBe('video')
    expect(fileKind({ kind: 'image', filename: 'a.mp4' })).toBe('image')
    expect(fileKind({ filename: 'out_00001.mp4' })).toBe('video')
    expect(fileKind({ filename: 'out_00001.WEBM' })).toBe('video')
    expect(fileKind({ filename: 'out_00001.png' })).toBe('image')
  })
})

describe('quickRunErrorText', () => {
  it('workflow_busy 用固定文案，其余用后端 message', () => {
    expect(quickRunErrorText(new ApiError(429, 'workflow_busy', 'x'))).toBe('ComfyUI 正在处理其它任务，请稍后再试')
    expect(quickRunErrorText(new ApiError(400, 'workflow_input_invalid', '宽：应在 512～2048'))).toBe('宽：应在 512～2048')
    expect(quickRunErrorText(new ApiError(422, 'workflow_prompt_rejected', 'Value not in list'))).toBe('Value not in list')
    expect(quickRunErrorText(new Error('boom'))).toBe('操作失败，请稍后重试')
  })
})
