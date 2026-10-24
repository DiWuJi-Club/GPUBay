import { flushPromises, mount } from '@vue/test-utils'
import ElementPlus, { ElUpload } from 'element-plus'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

const { post, put, upload } = vi.hoisted(() => ({ post: vi.fn(), put: vi.fn(), upload: vi.fn() }))
vi.mock('../../api', () => ({ api: { get: vi.fn(), post, put, del: vi.fn(), upload }, onUnauthorized: vi.fn(), onTotpSetupRequired: vi.fn() }))

import WorkflowFormDialog from '../WorkflowFormDialog.vue'

const saved = { id: 9, slug: 'my-wf', name: '我的', summary: '', description: '', category: 'txt2img', tags: ['a', 'b'], has_cover: false, cover_url: null, sort: 0, status: 'draft' }

let wrapper: ReturnType<typeof mount> | undefined
function mountDialog(workflow: unknown = null) {
  wrapper = mount(WorkflowFormDialog, { props: { modelValue: true, workflow: workflow as never }, global: { plugins: [ElementPlus] }, attachTo: document.body })
  return wrapper
}
const $ = (id: string) => document.querySelector(`[data-testid="${id}"]`) as HTMLElement | null
async function fill(id: string, v: string) {
  const el = $(id) as HTMLInputElement
  el.value = v
  el.dispatchEvent(new Event('input'))
  await flushPromises()
}
async function pickCover(file: File) {
  const up = wrapper!.findComponent(ElUpload)
  ;(up.props('onChange') as (f: unknown) => void)({ raw: file, name: file.name })
  await flushPromises()
}

beforeEach(() => {
  post.mockReset().mockResolvedValue(saved)
  put.mockReset().mockResolvedValue(saved)
  upload.mockReset().mockResolvedValue({ ...saved, has_cover: true, cover_url: '/admin/api/v1/workflows/9/cover?v=1' })
  globalThis.URL.createObjectURL = vi.fn(() => 'blob:preview')
  globalThis.URL.revokeObjectURL = vi.fn()
})
afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
  document.body.innerHTML = ''
})

describe('WorkflowFormDialog', () => {
  it('新建：先 POST 基本信息（标签按逗号拆分），再上传选好的封面', async () => {
    mountDialog()
    await flushPromises()
    await fill('adm-wf-slug', 'my-wf')
    await fill('adm-wf-name', '我的')
    await fill('adm-wf-tags', 'a，b, a')
    await pickCover(new File(['x'], 'c.png', { type: 'image/png' }))
    expect(($('adm-wf-cover-preview') as HTMLImageElement).src).toBe('blob:preview')
    $('adm-wf-submit')!.click()
    await flushPromises()
    expect(post).toHaveBeenCalledWith('/workflows', { slug: 'my-wf', name: '我的', summary: '', description: '', category: 'txt2img', tags: ['a', 'b'], sort: 0 })
    expect(upload).toHaveBeenCalledWith('/workflows/9/cover', expect.any(File))
    expect(wrapper!.emitted('saved')![0][0]).toMatchObject({ has_cover: true })
  })

  it('封面类型 / 大小不对：即时提示，不上传', async () => {
    mountDialog()
    await flushPromises()
    await pickCover(new File(['x'], 'c.gif', { type: 'image/gif' }))
    expect($('adm-wf-cover-error')!.textContent).toContain('PNG')
    await pickCover(new File([new Uint8Array((2 << 20) + 1)], 'c.png', { type: 'image/png' }))
    expect($('adm-wf-cover-error')!.textContent).toContain('2MB')
  })

  it('slug 不合法：即时提示，不提交', async () => {
    mountDialog()
    await flushPromises()
    await fill('adm-wf-slug', 'Bad')
    await fill('adm-wf-name', '我的')
    $('adm-wf-submit')!.click()
    await flushPromises()
    expect(post).not.toHaveBeenCalled()
    expect($('adm-wf-form-error')!.textContent).toContain('slug')
  })

  it('编辑：slug 只读，PUT 带状态', async () => {
    mountDialog({ ...saved, status: 'published', tags: ['x'] })
    await flushPromises()
    expect(($('adm-wf-slug') as HTMLInputElement).disabled).toBe(true)
    $('adm-wf-submit')!.click()
    await flushPromises()
    expect(put).toHaveBeenCalledWith('/workflows/9', expect.objectContaining({ slug: 'my-wf', tags: ['x'], status: 'published' }))
    expect(upload).not.toHaveBeenCalled()
  })
})
