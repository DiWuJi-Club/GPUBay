import { beforeEach, describe, expect, it } from 'vitest'
import { butler, closeButler, consumeContext, openButler, resetButler } from '../butler'

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
