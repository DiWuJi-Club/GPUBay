import { ApiError } from '@spark/shared'
import { flushPromises, mount } from '@vue/test-utils'
import ElementPlus, { ElMessage, ElMessageBox } from 'element-plus'
import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createMemoryHistory, createRouter, RouterView, type Router } from 'vue-router'

const { get, post, put } = vi.hoisted(() => ({ get: vi.fn(), post: vi.fn(), put: vi.fn() }))
vi.mock('../../api', () => ({ api: { get, post, put, del: vi.fn(), upload: vi.fn() }, onUnauthorized: vi.fn(), onTotpSetupRequired: vi.fn() }))
const { notifyError } = vi.hoisted(() => ({ notifyError: vi.fn() }))
vi.mock('../../logic/notify', () => ({ notifyError }))

import { session } from '../../store/session'
import WorkflowVersionView from '../WorkflowVersionView.vue'

const dir = join(__dirname, '../../../../../deploy/workflows/sdxl-txt2img')
const read = (f: string) => JSON.parse(readFileSync(join(dir, f), 'utf8'))
const graph = read('graph.json')
const apiPrompt = read('api.json')
const manifest = read('manifest.json')

const wf = { id: 1, slug: 'sdxl-txt2img', name: 'SDXL 文生图', summary: '', description: '', category: 'txt2img', tags: [], has_cover: false, cover_url: null, sort: 0, status: 'draft' }
const version = (over: Record<string, unknown> = {}) => ({
  id: 7, workflow_id: 1, version: '1.0.0', status: 'draft', graph, api_prompt: apiPrompt, manifest, download_gb: 6.94, manifest_hash: 'abc', changelog: '首版',
  published_at: null, created_at: '', updated_at: '', ...over,
})

let router: Router
let wrapper: ReturnType<typeof mount> | undefined
async function mountAt(url: string) {
  router = createRouter({ history: createMemoryHistory(), routes: [{ path: '/workflows/:id/versions/:vid', component: WorkflowVersionView }, { path: '/:p(.*)*', component: { template: '<div />' } }] })
  await router.push(url)
  await router.isReady()
  wrapper = mount(RouterView, { global: { plugins: [router, ElementPlus] }, attachTo: document.body })
  await flushPromises()
}
const $ = (id: string) => document.querySelector(`[data-testid="${id}"]`) as HTMLElement | null
const $$ = (id: string) => [...document.querySelectorAll(`[data-testid="${id}"]`)] as HTMLElement[]
async function click(id: string) {
  $(id)!.click()
  await flushPromises()
}

let current = version()
beforeEach(() => {
  session.admin = { id: 1, username: 'root', role: 'super' } as never
  current = version()
  get.mockReset()
  post.mockReset()
  put.mockReset()
  get.mockImplementation(async (path: string) => {
    if (path === '/workflows/1') return wf
    if (path === '/workflows/1/versions/7') return current
    throw new ApiError(404, 'not_found', '不存在')
  })
  put.mockResolvedValue({ ok: true, manifest_hash: 'abcdef0123456789', download_gb: 6.94, warnings: ['未填 sha256'] })
  notifyError.mockReset()
  vi.restoreAllMocks()
})
afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
  document.body.innerHTML = ''
})

describe('WorkflowVersionView', () => {
  it('草稿：结构化表单载入清单，校验发 PUT ?validate=1（完整内容），显示通过与提示', async () => {
    await mountAt('/workflows/1/versions/7')
    expect($$('adm-wf-model-row')).toHaveLength(1)
    expect($$('adm-wf-input-row')).toHaveLength(manifest.inputs.length)
    expect($('adm-wf-graph-info')!.textContent).toContain('个节点')
    expect($('adm-wf-missing')).toBeNull()
    await click('adm-wf-validate')
    expect(put).toHaveBeenCalledTimes(1)
    const [path, body] = put.mock.calls[0]
    expect(path).toBe('/workflows/1/versions/7?validate=1')
    expect(body).toEqual({ version: '1.0.0', graph, api_prompt: apiPrompt, manifest, changelog: '首版' })
    expect($('adm-wf-validate-ok')!.textContent).toContain('校验通过')
    expect($('adm-wf-warnings')!.textContent).toContain('未填 sha256')
  })

  it('后端 workflow_version_invalid：显示错误并定位到表格行', async () => {
    put.mockRejectedValue(new ApiError(400, 'workflow_version_invalid', 'manifest.models[0].modelscope.repo：仓库应形如 owner/name，≤ 128 字符'))
    await mountAt('/workflows/1/versions/7')
    await click('adm-wf-validate')
    expect($('adm-wf-error')!.textContent).toContain('manifest.models[0].modelscope.repo')
    expect($$('adm-wf-model-row')[0].classList.contains('err-row')).toBe(true)
    expect($$('adm-wf-field-error').some((e) => e.textContent!.includes('第 1 行 ModelScope 仓库'))).toBe(true)
  })

  it('D16：去掉模型后列出 graph 引用的缺失模型，可一键补成模型行', async () => {
    current = version({ manifest: { ...manifest, models: [], inputs: [] } })
    await mountAt('/workflows/1/versions/7')
    expect($('adm-wf-missing-models')!.textContent).toContain('sd_xl_base_1.0.safetensors')
    await click('adm-wf-add-missing')
    expect($$('adm-wf-model-row')).toHaveLength(1)
    expect(($$('adm-wf-model-row')[0].querySelector('[data-testid="adm-wf-model-name"]') as HTMLInputElement).value).toBe('sd_xl_base_1.0.safetensors')
    expect($('adm-wf-missing')).toBeNull()
  })

  it('非数字的大小：本地拦下，不请求后端', async () => {
    current = version({ manifest: { ...manifest, models: [{ ...manifest.models[0], size_gb: 'x' }] } })
    await mountAt('/workflows/1/versions/7')
    await click('adm-wf-validate')
    expect(put).not.toHaveBeenCalled()
    expect($('adm-wf-error')!.textContent).toContain('manifest.models[0].size_gb')
  })

  it('已发布：只读，保存只发更新说明；可退役；没有校验 / 发布按钮', async () => {
    current = version({ status: 'published', published_at: '2026-10-20T00:00:00Z' })
    put.mockResolvedValue(current)
    await mountAt('/workflows/1/versions/7')
    expect($('adm-wf-readonly')).not.toBeNull()
    expect($('adm-wf-validate')).toBeNull()
    expect($('adm-wf-publish')).toBeNull()
    expect($('adm-wf-retire')).not.toBeNull()
    expect(($('adm-wf-graph') as HTMLTextAreaElement).readOnly).toBe(true)
    expect(($$('adm-wf-model-row')[0].querySelector('[data-testid="adm-wf-model-name"]') as HTMLInputElement).disabled).toBe(true)
    expect($('adm-wf-model-add')).toBeNull()
    await click('adm-wf-save')
    expect(put).toHaveBeenCalledWith('/workflows/1/versions/7', { changelog: '首版' })
  })

  it('发布：先校验并显示下载量，确认后 POST publish', async () => {
    post.mockResolvedValue(version({ status: 'published' }))
    await mountAt('/workflows/1/versions/7')
    await click('adm-wf-publish')
    expect($('adm-wf-totp')).not.toBeNull()
    expect($('adm-wf-publish-download')!.textContent).toBe('6.94 GB')
    expect($('adm-wf-publish-check')!.textContent).toContain('校验通过')
    await click('adm-wf-publish-ok')
    expect(post).toHaveBeenCalledWith('/workflows/1/versions/7/publish', {})
  })

  it('新建（从版本复制）：不带版本号与更新说明；空 api.json 发 api_prompt: null，不能加参数', async () => {
    current = version({ api_prompt: null, manifest: { ...manifest, inputs: [], outputs: [] } })
    post.mockResolvedValue({ ok: true, manifest_hash: 'x'.repeat(16), download_gb: 6.94, warnings: [] })
    await mountAt('/workflows/1/versions/new?copy_from=7')
    expect(($('adm-wf-version-number') as HTMLInputElement).value).toBe('')
    expect($('adm-wf-input-add')).toBeNull()
    expect($('adm-wf-api-info')!.textContent).toContain('不支持快速出图')
    const input = $('adm-wf-version-number') as HTMLInputElement
    input.value = '1.1.0'
    input.dispatchEvent(new Event('input'))
    await click('adm-wf-validate')
    expect(post).toHaveBeenCalledTimes(1)
    expect(post.mock.calls[0][0]).toBe('/workflows/1/versions?validate=1')
    expect(post.mock.calls[0][1]).toMatchObject({ version: '1.1.0', api_prompt: null, changelog: '' })
  })

  it('ops：只读，没有保存按钮', async () => {
    session.admin = { id: 2, username: 'ops', role: 'ops' } as never
    await mountAt('/workflows/1/versions/7')
    expect($('adm-wf-save')).toBeNull()
    expect($('adm-wf-validate')).toBeNull()
    expect(($('adm-wf-graph') as HTMLTextAreaElement).readOnly).toBe(true)
  })
})

async function typeInto(id: string, v: string) {
  const el = $(id) as HTMLInputElement | HTMLTextAreaElement
  el.value = v
  el.dispatchEvent(new Event('input'))
  await flushPromises()
}

describe('WorkflowVersionView（T14 评审修正）', () => {
  it('I-1：text / textarea 无默认值显示为「必填」并保持无默认值', async () => {
    const m = { ...manifest, inputs: manifest.inputs.map((i: Record<string, unknown>) => (i.key === 'negative' ? { ...i, default: undefined } : i)) }
    current = version({ manifest: m })
    await mountAt('/workflows/1/versions/7')
    const rows = $$('adm-wf-input-row')
    const idx = m.inputs.findIndex((i: Record<string, unknown>) => i.key === 'negative')
    const box = rows[idx].querySelector('[data-testid="adm-wf-input-required"]')!
    expect(box.querySelector('input[type=checkbox]')!.hasAttribute('checked') || box.classList.contains('is-checked')).toBe(true)
    expect(rows[0].querySelector('[data-testid="adm-wf-input-required"]')!.classList.contains('is-checked')).toBe(false)
    await click('adm-wf-validate')
    const sent = put.mock.calls[0][1].manifest.inputs[idx]
    expect('default' in sent).toBe(false)
  })

  it('I-2：有未保存的修改时离开页面要确认；取消则留下，确认才离开', async () => {
    await mountAt('/workflows/1/versions/7')
    const confirm = vi.spyOn(ElMessageBox, 'confirm').mockRejectedValueOnce('cancel')
    await router.push('/workflows/1') // 未修改：不确认
    expect(confirm).not.toHaveBeenCalled()
    await router.push('/workflows/1/versions/7')
    await flushPromises()
    await typeInto('adm-wf-changelog', '改了')
    await router.push('/workflows/1')
    expect(confirm).toHaveBeenCalledTimes(1)
    expect(router.currentRoute.value.path).toBe('/workflows/1/versions/7')
    confirm.mockResolvedValueOnce('confirm' as never)
    await router.push('/workflows/1')
    expect(router.currentRoute.value.path).toBe('/workflows/1')
  })

  it('I-2：有未保存的修改时关闭 / 刷新浏览器页签会提示（beforeunload）', async () => {
    await mountAt('/workflows/1/versions/7')
    const clean = new Event('beforeunload', { cancelable: true })
    window.dispatchEvent(clean)
    expect(clean.defaultPrevented).toBe(false)
    await typeInto('adm-wf-changelog', '改了')
    const dirty = new Event('beforeunload', { cancelable: true })
    window.dispatchEvent(dirty)
    expect(dirty.defaultPrevented).toBe(true)
  })

  it('I-2：已发布版本改了更新说明未保存时，退役先提示保存', async () => {
    current = version({ status: 'published' })
    const warn = vi.spyOn(ElMessage, 'warning')
    await mountAt('/workflows/1/versions/7')
    await typeInto('adm-wf-changelog', '改了')
    await click('adm-wf-retire')
    expect(warn).toHaveBeenCalled()
    expect(($('adm-wf-retire-dialog')?.closest('.el-overlay') as HTMLElement | null)?.style.display ?? 'none').toBe('none')
  })

  it('修改表单后清掉上一次后端错误的定位', async () => {
    put.mockRejectedValue(new ApiError(400, 'workflow_version_invalid', 'manifest.models[0].modelscope.repo：仓库应形如 owner/name'))
    await mountAt('/workflows/1/versions/7')
    await click('adm-wf-validate')
    expect($$('adm-wf-model-row')[0].classList.contains('err-row')).toBe(true)
    await typeInto('adm-wf-model-ms-repo', 'a/b')
    expect($$('adm-wf-model-row')[0].classList.contains('err-row')).toBe(false)
    expect($('adm-wf-error')).toBeNull()
  })

  it('本地拦截时显示真正拦住请求的那一项', async () => {
    current = version({ version: '', manifest: { ...manifest, models: [{ ...manifest.models[0], size_gb: 'x' }] } })
    await mountAt('/workflows/1/versions/7')
    await click('adm-wf-validate')
    expect(put).not.toHaveBeenCalled()
    expect($('adm-wf-error')!.textContent).toContain('manifest.models[0].size_gb')
  })

  it('保存遇到 workflow_version_invalid：页面内显示，不再重复弹提示', async () => {
    put.mockRejectedValue(new ApiError(400, 'workflow_version_invalid', 'version：只能由字母…'))
    await mountAt('/workflows/1/versions/7')
    await click('adm-wf-save')
    expect($('adm-wf-error')!.textContent).toContain('version')
    expect(notifyError).not.toHaveBeenCalled()
    put.mockRejectedValue(new ApiError(409, 'duplicate', '版本号已存在'))
    await click('adm-wf-save')
    expect(notifyError).toHaveBeenCalledTimes(1)
  })
})
