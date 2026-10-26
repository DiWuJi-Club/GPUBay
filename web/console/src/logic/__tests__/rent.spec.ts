import { describe, expect, it } from 'vitest'
import { effectScope, nextTick, reactive, ref } from 'vue'
import { bookingBody, orderBody, useRentKey, type RentForm } from '../rent'

const form = (over: Partial<RentForm> = {}): RentForm => ({
  groupId: null, count: 1, immediate: true, day: '', hour: null, duration: '1d', quantity: 1, imageId: 3, machineId: null,
  autoRenew: false, autoRenewDuration: '1d', appId: null, appWorkflowIds: [], ...over,
})

describe('下单请求体（/orders、/bookings）', () => {
  it('/orders 不预装：与计划 6 完全一致，不带 app_id / app_env', () => {
    expect(orderBody(1, form(), null, { MODEL_ID: 'x' })).toStrictEqual({ product_id: 1, duration: '1d', image_id: 3 })
  })
  it('/orders 预装：带 app_id 与去掉空值的 app_env', () => {
    expect(orderBody(1, form({ appId: 4 }), 4, { JUPYTER_TOKEN: 'tok12345678', OPTIONAL: '' }))
      .toStrictEqual({ product_id: 1, duration: '1d', image_id: 3, app_id: 4, app_env: { JUPYTER_TOKEN: 'tok12345678' } })
  })
  it('/bookings 不预装：不带 app_id / app_env', () => {
    const f = form({ groupId: 2, count: 2, immediate: false, quantity: 3, duration: '1w', autoRenew: true, autoRenewDuration: '1w' })
    expect(bookingBody(1, f, '2026-10-21T02:00:00Z', null, {})).toStrictEqual({
      product_id: 1, image_id: 3, group_id: 2, machine_id: null, count: 2, duration: '1w', quantity: 3, start_at: '2026-10-21T02:00:00Z',
      auto_renew: true, auto_renew_duration: '1w',
    })
  })
  it('/bookings 预装：带 app_id 与 app_env；立即开始时没有 start_at', () => {
    const body = bookingBody(1, form({ appId: 1 }), null, 1, { MODEL_ID: 'Qwen/Qwen3-4B', API_KEY: '' })
    expect(body).toStrictEqual({
      product_id: 1, image_id: 3, group_id: null, machine_id: null, count: 1, duration: '1d', quantity: 1, start_at: undefined,
      auto_renew: false, auto_renew_duration: '1d', app_id: 1, app_env: { MODEL_ID: 'Qwen/Qwen3-4B' },
    })
  })
  it('预装工作流：有应用时带 app_workflow_ids；空列表或不预装应用时不带', () => {
    expect(orderBody(1, form({ appId: 3, appWorkflowIds: [7, 9] }), 3, { MODEL_ID: 'none' }))
      .toStrictEqual({ product_id: 1, duration: '1d', image_id: 3, app_id: 3, app_env: { MODEL_ID: 'none' }, app_workflow_ids: [7, 9] })
    expect(bookingBody(1, form({ appId: 3, appWorkflowIds: [7] }), null, 3, {})).toMatchObject({ app_id: 3, app_workflow_ids: [7] })
    expect(orderBody(1, form({ appId: 3 }), 3, {})).not.toHaveProperty('app_workflow_ids')
    expect(orderBody(1, form({ appWorkflowIds: [7] }), null, {})).toStrictEqual({ product_id: 1, duration: '1d', image_id: 3 })
    expect(bookingBody(1, form({ appWorkflowIds: [7] }), null, null, {})).not.toHaveProperty('app_workflow_ids')
  })
})

describe('集群下单（计划 13）', () => {
  it('/bookings 集群：cluster=true、count=台数，不带 start_at / machine_id / 预装应用', () => {
    const f = form({ clusterSize: 2, count: 1, immediate: false, machineId: 5, appId: 3, appWorkflowIds: [7], quantity: 2, groupId: 4 })
    expect(bookingBody(1, f, '2026-10-21T02:00:00Z', 3, { MODEL_ID: 'x' })).toStrictEqual({
      product_id: 1, image_id: 3, group_id: 4, machine_id: null, count: 2, duration: '1d', quantity: 2,
      auto_renew: false, auto_renew_duration: '1d', cluster: true,
    })
  })
  it('单机（clusterSize 0 / 缺省）请求体不变，不带 cluster', () => {
    expect(bookingBody(1, form({ clusterSize: 0 }), null, null, {})).not.toHaveProperty('cluster')
  })
  it('切换租用方式换幂等键', async () => {
    const f = reactive(form())
    const scope = effectScope()
    const key = scope.run(() => useRentKey(() => 1, f, () => ({})))!
    const k1 = key.current()
    f.clusterSize = 4
    await nextTick()
    expect(key.current()).not.toBe(k1)
    scope.stop()
  })
})

describe('幂等键（RentDialog 的派生参数）', () => {
  function setup() {
    const f = reactive(form({ appId: 4 }))
    const env = ref<Record<string, string>>({ JUPYTER_TOKEN: 'aaaaaaaa' })
    const scope = effectScope()
    const key = scope.run(() => useRentKey(() => 1, f, () => env.value))!
    return { f, env, key, scope }
  }
  it('参数不变的重试复用同一个键', async () => {
    const { key, env, scope } = setup()
    const k1 = key.current()
    env.value = { ...env.value } // 同值的新对象（变量表单每次输入都会替换）
    await nextTick()
    env.value = { JUPYTER_TOKEN: 'x' }
    env.value = { JUPYTER_TOKEN: 'aaaaaaaa' } // 改了又改回
    await nextTick()
    expect(key.current()).toBe(k1)
    scope.stop()
  })
  it('改变量值、换应用、取消预装都换键', async () => {
    const { f, env, key, scope } = setup()
    const k1 = key.current()
    env.value = { JUPYTER_TOKEN: 'bbbbbbbb' }
    await nextTick()
    const k2 = key.current()
    expect(k2).not.toBe(k1)
    f.appId = 1
    await nextTick()
    const k3 = key.current()
    expect(k3).not.toBe(k2)
    f.appId = null
    await nextTick()
    expect(key.current()).not.toBe(k3)
    scope.stop()
  })
  it('空值不影响键（提交时本来就去掉）', async () => {
    const { env, key, scope } = setup()
    const k1 = key.current()
    env.value = { ...env.value, OPTIONAL: '' }
    await nextTick()
    expect(key.current()).toBe(k1)
    scope.stop()
  })
  it('改预装工作流（增、减、换）都换键', async () => {
    const { f, key, scope } = setup()
    const k1 = key.current()
    f.appWorkflowIds = [7]
    await nextTick()
    const k2 = key.current()
    expect(k2).not.toBe(k1)
    f.appWorkflowIds = [7, 9]
    await nextTick()
    expect(key.current()).not.toBe(k2)
    f.appWorkflowIds = [7]
    await nextTick()
    expect(key.current()).not.toBe(k2) // 派生参数又变了一次：再换（不回到旧键）
    scope.stop()
  })
})
