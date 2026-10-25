import { beforeEach, describe, expect, it, vi } from 'vitest'

const get = vi.hoisted(() => vi.fn())
vi.mock('../../api', () => ({ api: { get } }))

// resetModules 后 @spark/shared 也是新的一份：ApiError 要和 store 用同一份（instanceof）
async function load() {
  const { ApiError } = await import('@spark/shared')
  return { ApiError, ...(await import('../butler')) }
}

beforeEach(() => {
  get.mockReset()
  vi.resetModules() // 每个用例拿一份新的模块状态（settled 是模块级的）
})

describe('store/butler：平台是否关闭了 AI 管家', () => {
  it('404 → butlerOff，之后不再探测', async () => {
    const { ApiError, butlerOff, probeButlerOff } = await load()
    get.mockRejectedValue(new ApiError(404, 'not_found', '接口不存在', 'r1'))
    await probeButlerOff()
    expect(butlerOff.value).toBe(true)
    expect(get).toHaveBeenCalledWith('/butler/settings', undefined, { quiet: true })
    await probeButlerOff()
    expect(get).toHaveBeenCalledTimes(1)
  })

  it('200 → 显示菜单，之后不再探测', async () => {
    const { butlerOff, probeButlerOff } = await load()
    get.mockResolvedValue({ enabled: false })
    await probeButlerOff()
    await probeButlerOff()
    expect(butlerOff.value).toBe(false)
    expect(get).toHaveBeenCalledTimes(1)
  })

  it('其它错误（403 / 网络）→ 照常显示，下次再探测', async () => {
    const { ApiError, butlerOff, probeButlerOff } = await load()
    get.mockRejectedValueOnce(new ApiError(0, 'network', '网络异常，请稍后重试', ''))
    await probeButlerOff()
    expect(butlerOff.value).toBe(false)
    get.mockRejectedValueOnce(new ApiError(404, 'not_found', '接口不存在', 'r2'))
    await probeButlerOff()
    expect(butlerOff.value).toBe(true)
    expect(get).toHaveBeenCalledTimes(2)
  })
})
