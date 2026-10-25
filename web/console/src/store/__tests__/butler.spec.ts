import { ApiError } from '@spark/shared'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const get = vi.hoisted(() => vi.fn())
vi.mock('../../api', () => ({ api: { get }, onUnauthorized: vi.fn() }))

import { butler, butlerAvail, butlerAvailable, closeButler, consumeContext, openButler, probeButler, resetButler } from '../butler'

describe('store/butler', () => {
  beforeEach(() => resetButler())

  it('openButler 带实例上下文与草稿；关闭不清状态', () => {
    butler.conversationId = 'cv-1'
    openButler({ instanceId: 'ins-abc', draft: '这个实例上的应用为什么失败了？' })
    expect(butler).toMatchObject({ open: true, conversationId: 'cv-1', pendingContext: { instance_id: 'ins-abc' }, draft: '这个实例上的应用为什么失败了？' })
    closeButler()
    expect(butler).toMatchObject({ open: false, conversationId: 'cv-1', pendingContext: { instance_id: 'ins-abc' } })
  })

  it('不带参数打开：清掉旧上下文、保留草稿', () => {
    openButler({ instanceId: 'ins-abc', draft: '草稿' })
    openButler()
    expect(butler.pendingContext).toBeNull()
    expect(butler.draft).toBe('草稿')
  })

  it('consumeContext 只取一次', () => {
    openButler({ instanceId: 'ins-abc' })
    expect(consumeContext()).toEqual({ instance_id: 'ins-abc' })
    expect(consumeContext()).toBeNull()
  })

  it('resetButler 清空全部', () => {
    openButler({ instanceId: 'ins-abc', draft: 'x' })
    butler.conversationId = 'cv-1'
    resetButler()
    expect({ ...butler }).toEqual({ open: false, conversationId: null, pendingContext: null, draft: '' })
  })
})

describe('probeButler：平台是否启用了 AI 管家', () => {
  beforeEach(() => {
    get.mockReset()
    butlerAvail.state = 'unknown'
  })

  it('200（含管理员关闭 enabled=false）→ available，只探测一次', async () => {
    get.mockResolvedValue({ enabled: false, notice: '维护中' })
    await Promise.all([probeButler(), probeButler()])
    await probeButler()
    expect(butlerAvail.state).toBe('available')
    expect(butlerAvailable.value).toBe(true)
    expect(get).toHaveBeenCalledTimes(1)
    expect(get).toHaveBeenCalledWith('/butler/status', undefined, { quiet: true }) // quiet：匿名的 401 不提示不跳转
  })

  it('401（未登录，路由已注册）→ available', async () => {
    get.mockRejectedValue(new ApiError(401, 'unauthorized', '请先登录', 'r1'))
    await probeButler()
    expect(butlerAvail.state).toBe('available')
  })

  it('404（SPARK_BUTLER=off，路由不注册）→ unavailable，不再探测', async () => {
    get.mockRejectedValue(new ApiError(404, 'not_found', '接口不存在', 'r2'))
    await probeButler()
    expect(butlerAvail.state).toBe('unavailable')
    expect(butlerAvailable.value).toBe(false)
    await probeButler()
    expect(get).toHaveBeenCalledTimes(1)
  })

  it('网络错误 / 5xx → 保持 unknown（入口隐藏），下次再探测', async () => {
    get.mockRejectedValueOnce(new ApiError(0, 'network', '网络异常，请稍后重试', ''))
    await probeButler()
    expect(butlerAvail.state).toBe('unknown')
    get.mockResolvedValueOnce({ enabled: true })
    await probeButler()
    expect(butlerAvail.state).toBe('available')
    expect(get).toHaveBeenCalledTimes(2)
  })
})
