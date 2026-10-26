import { ApiError, type Admin, type LLMNode, type LLMPool, type Machine, type Role } from '@spark/shared'
import { flushPromises, mount } from '@vue/test-utils'
import ElementPlus, { ElMessage } from 'element-plus'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createMemoryHistory, createRouter, type Router } from 'vue-router'

const { get, post, put, del } = vi.hoisted(() => ({ get: vi.fn(), post: vi.fn(), put: vi.fn(), del: vi.fn() }))
vi.mock('../../api', () => ({ api: { get, post, put, del }, onUnauthorized: vi.fn(), onTotpSetupRequired: vi.fn() }))

import { fmtTime } from '@spark/shared'
import { session } from '../../store/session'
import NodesTab from '../llm/NodesTab.vue'

const LEASE = '2026-11-01T02:00:00Z'
const node = (over: Partial<LLMNode> = {}): LLMNode => ({
  id: 'n1', model_id: 3, model_slug: 'qwen3-32b', machine_id: 11, machine_name: 'A01-01', instance_id: 'i-sys1', deployment_id: 'd1', state: 'ready',
  deployment: { state: 'running', health: 'ok', phase: '', progress: 100 }, inflight: 2, probe_fail_count: 0, rebuild_count: 1,
  unhealthy_since: null, drain_since: null, last_probe_at: null, last_ready_at: null, error: '', created_at: '', updated_at: '',
  ...over,
})
const poolData = (): LLMPool => ({
  group_id: 9, free: 1, total: 2,
  machines: [
    { machine: { id: 11, serial: 'SN-11', display_name: 'A01-01', state: 'allocated', agent_online: true }, node: { id: 'n1', model_slug: 'qwen3-32b', state: 'ready' } },
    { machine: { id: 12, serial: 'SN-12', display_name: '', state: 'allocated', agent_online: false }, lease_until: LEASE },
  ],
})
const candidate = (id: number): Machine => ({ id, serial: `SN-${id}`, rack: 'B', slot: '1', display_name: `B-${id}`, state: 'idle', agent_online: true, group_id: null } as Machine)

let nodes: LLMNode[]
let wrapper: ReturnType<typeof mount> | undefined

function login(role: Role) {
  session.status = 'authed'
  session.admin = { id: 1, username: 'a', role, status: 'active', totp_confirmed: true, last_login_at: null, last_login_ip: '', created_at: '' } as Admin
}
let router: Router
async function mountTab(active = true, url = '/llm') {
  router = createRouter({ history: createMemoryHistory(), routes: [{ path: '/:p(.*)*', component: { template: '<div />' } }] })
  await router.push(url)
  wrapper = mount(NodesTab, { props: { active }, global: { plugins: [router, ElementPlus] }, attachTo: document.body })
  await flushPromises()
}
const byId = (id: string) => wrapper!.find(`[data-testid="${id}"]`)
const allById = (id: string) => wrapper!.findAll(`[data-testid="${id}"]`)
/** 对话框 / 抽屉挂在 body 上（teleport） */
const inBody = (id: string) => document.body.querySelector<HTMLElement>(`[data-testid="${id}"]`)
const click = async (el: HTMLElement | null) => {
  expect(el).not.toBeNull()
  el!.click()
  await flushPromises()
}
const calls = (fn: typeof get, path: string) => fn.mock.calls.filter((c) => c[0] === path)

beforeEach(() => {
  login('ops')
  nodes = [node(), node({ id: 'n2', state: 'failed', machine_id: 13, machine_name: 'A01-03', deployment: null, error: '镜像拉取失败', rebuild_count: 3 })]
  for (const f of [get, post, put, del]) f.mockReset()
  get.mockImplementation(async (path: string, query?: Record<string, unknown>) => {
    if (path === '/llm/pool') return poolData()
    if (path === '/llm/nodes') return { items: nodes }
    if (path === '/llm/models') return { items: [{ id: 3, slug: 'qwen3-32b' }] }
    if (path === '/groups') return { items: [{ id: 9, name: 'Token 服务池', purpose: 'llm' }, { id: 2, name: '华东一区', purpose: 'rental' }] }
    if (path === '/machines') return { items: query?.group_id === 'none' ? [candidate(21), candidate(22)] : [candidate(31)], total: 2 }
    if (path.startsWith('/llm/nodes/')) return { node: node(), events: [], deployment_events: [] }
    if (path.startsWith('/deployments/')) return { deployment: { id: 'd1', state: 'running', health: 'ok', phase: '', progress: 100, log_tail: 'ok', app_name: 'vllm-node', version: '1' }, events: [] }
    throw new Error(`unexpected GET ${path}`)
  })
  post.mockResolvedValue({})
  del.mockResolvedValue({})
})
afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
  document.body.innerHTML = ''
  vi.useRealTimers()
  vi.restoreAllMocks()
})

describe('NodesTab 机器池', () => {
  it('池表：展示名 / 序列号 / 在线 / 节点 / 租约（无节点时「等待租约结束」）', async () => {
    await mountTab()
    expect(byId('llm-pool-free').text()).toBe('1')
    expect(byId('llm-pool-total').text()).toBe('2')
    const rows = allById('llm-pool-row')
    expect(rows).toHaveLength(2)
    expect(rows[0].find('[data-testid="llm-pool-name"]').text()).toBe('A01-01')
    expect(rows[0].find('[data-testid="llm-pool-serial"]').text()).toBe('SN-11')
    expect(rows[0].find('[data-testid="llm-pool-node"]').text()).toContain('qwen3-32b')
    expect(rows[0].find('[data-testid="llm-pool-node"]').text()).toContain('就绪')
    expect(rows[0].find('[data-testid="llm-pool-lease"]').text()).toBe('—')
    expect(rows[1].find('[data-testid="llm-pool-name"]').text()).toBe('机器 #12')
    expect(rows[1].find('[data-testid="llm-pool-online"]').text()).toBe('离线')
    expect(rows[1].find('[data-testid="llm-pool-lease"]').text()).toBe(`等待租约结束（${fmtTime(LEASE)}）`)
  })

  it('加入机器：默认列未分组机器，可切到出租机组；勾选后 POST /groups/{池}/machines', async () => {
    await mountTab()
    await byId('llm-pool-add').trigger('click')
    await flushPromises()
    expect(get).toHaveBeenCalledWith('/machines', { group_id: 'none', rack: undefined, page: 1, page_size: 50 })
    expect(document.body.querySelectorAll('[data-testid="llm-pool-add-row"]')).toHaveLength(2)
    const checks = document.body.querySelectorAll<HTMLElement>('[data-testid="llm-pool-add-check"] input')
    checks[1].click()
    await flushPromises()
    expect(inBody('llm-pool-add-count')!.textContent).toBe('1')
    const before = calls(get, '/llm/pool').length
    await click(inBody('llm-pool-add-submit'))
    expect(post).toHaveBeenCalledWith('/groups/9/machines', { machine_ids: [22] })
    expect(calls(get, '/llm/pool').length).toBe(before + 1) // 加入后刷新池
  })

  it('移出：DELETE /groups/{池}/machines；409 文案原样提示', async () => {
    const err = vi.spyOn(ElMessage, 'error')
    await mountTab()
    await allById('llm-pool-remove')[1].trigger('click')
    await flushPromises()
    await click(inBody('llm-pool-remove-ok'))
    expect(del).toHaveBeenCalledWith('/groups/9/machines', { machine_ids: [12] })

    // 前端看不到的节点（并发刚建）：后端 409 文案原样提示
    del.mockRejectedValueOnce(new ApiError(409, 'llm_pool_machine_busy', '机器上有运行中的节点，请先下线'))
    await allById('llm-pool-remove')[1].trigger('click')
    await flushPromises()
    await click(inBody('llm-pool-remove-ok'))
    expect(del).toHaveBeenCalledTimes(2)
    expect(err).toHaveBeenCalledWith('机器上有运行中的节点，请先下线')
  })

  it('机器上有未结束的节点：「移出」禁用', async () => {
    await mountTab()
    const btns = allById('llm-pool-remove')
    expect(btns[0].attributes('disabled')).toBeDefined() // 节点 ready
    expect(btns[1].attributes('disabled')).toBeUndefined() // 没有节点
  })
})

describe('NodesTab 节点', () => {
  it('节点表列与按状态的行操作', async () => {
    await mountTab()
    const rows = allById('llm-node-row')
    expect(rows).toHaveLength(2)
    expect(rows[0].find('[data-testid="llm-node-row-state"]').text()).toBe('就绪')
    expect(rows[0].find('[data-testid="llm-node-row-health"]').text()).toBe('正常')
    expect(rows[0].find('[data-testid="llm-node-row-inflight"]').text()).toBe('2')
    expect(rows[0].find('[data-testid="llm-node-row-progress"]').text()).toBe('运行中')
    expect(rows[1].find('[data-testid="llm-node-row-error"]').text()).toBe('镜像拉取失败')
    expect(rows[1].find('[data-testid="llm-node-row-rebuilds"]').text()).toBe('3')
    // ready：排空 / 重建 / 移除；failed：重建 / 移除
    expect(rows[0].findAll('button').map((b) => b.text()).slice(1)).toEqual(['排空', '重建', '移除'])
    expect(rows[1].find('[data-testid="llm-node-drain"]').exists()).toBe(false)
    expect(rows[1].find('[data-testid="llm-node-rebuild"]').exists()).toBe(true)
  })

  it('排空 / 重建：POST /llm/nodes/{id}/drain|rebuild；409 原文提示且弹窗保留', async () => {
    const err = vi.spyOn(ElMessage, 'error')
    await mountTab()
    await allById('llm-node-drain')[0].trigger('click')
    await flushPromises()
    await click(inBody('llm-node-op-ok'))
    expect(post).toHaveBeenCalledWith('/llm/nodes/n1/drain', {})

    post.mockRejectedValueOnce(new ApiError(409, 'llm_node_invalid_state', '节点当前状态不能执行该操作'))
    await allById('llm-node-rebuild')[1].trigger('click')
    await flushPromises()
    await click(inBody('llm-node-op-ok'))
    expect(post).toHaveBeenLastCalledWith('/llm/nodes/n2/rebuild', {})
    expect(err).toHaveBeenCalledWith('节点当前状态不能执行该操作')
    expect(inBody('llm-node-op-dialog')).not.toBeNull()
  })

  it('移除：原因必填；提交 {reason}（动态码由请求层补）', async () => {
    await mountTab()
    await allById('llm-node-remove')[0].trigger('click')
    await flushPromises()
    await click(inBody('llm-node-op-ok'))
    expect(inBody('llm-node-op-error')!.textContent).toBe('请填写移除原因')
    expect(post).not.toHaveBeenCalled()
    const el = inBody('llm-node-remove-reason')!
    const input = (el.tagName === 'INPUT' ? el : el.querySelector('input')!) as HTMLInputElement
    input.value = '  机器下架  '
    input.dispatchEvent(new Event('input'))
    await flushPromises()
    await click(inBody('llm-node-op-ok'))
    expect(post).toHaveBeenCalledWith('/llm/nodes/n1/remove', { reason: '机器下架' })
  })

  it('筛选：模型 / 状态作为 GET /llm/nodes 的查询参数', async () => {
    await mountTab()
    expect(get).toHaveBeenCalledWith('/llm/nodes', { model_id: undefined, state: undefined })
  })

  it('点节点打开抽屉：节点详情 + 部署详情', async () => {
    await mountTab()
    await allById('llm-node-open')[0].trigger('click')
    await flushPromises()
    expect(get).toHaveBeenCalledWith('/llm/nodes/n1')
    expect(get).toHaveBeenCalledWith('/deployments/d1')
    expect(inBody('llm-node-deployment-log')!.textContent).toBe('ok')
  })

  it('抽屉「错误」只显示错误码对应文案：运行中部署的 message（healthy）不当作错误', async () => {
    let dep: Record<string, unknown> = { id: 'd1', state: 'running', health: 'ok', phase: 'health', progress: 100, message: 'healthy', error_code: '', log_tail: '', app_name: 'vllm-node', version: '1' }
    const base = get.getMockImplementation()!
    get.mockImplementation(async (path: string, query?: Record<string, unknown>) => (path.startsWith('/deployments/') ? { deployment: dep, events: [] } : base(path, query)))
    await mountTab()
    await allById('llm-node-open')[0].trigger('click')
    await flushPromises()
    expect(inBody('llm-node-deployment-err')!.textContent).toBe('—')
    wrapper!.unmount()
    document.body.innerHTML = ''
    dep = { ...dep, state: 'failed', error_code: 'crashed', message: '服务进程已退出' }
    await mountTab()
    await allById('llm-node-open')[0].trigger('click')
    await flushPromises()
    expect(inBody('llm-node-deployment-err')!.textContent).toContain('服务进程已退出')
  })

  it('?node=<id> 直接打开抽屉；关闭后去掉参数', async () => {
    await mountTab(true, '/llm?tab=nodes&node=n2')
    expect(get).toHaveBeenCalledWith('/llm/nodes/n2')
    expect(inBody('llm-node-drawer')).not.toBeNull()
    ;(wrapper!.findComponent({ name: 'NodeDrawer' }).vm as unknown as { $emit: (e: string) => void }).$emit('close')
    await flushPromises()
    expect(router.currentRoute.value.query).toEqual({ tab: 'nodes' })
  })

  it('finance（无运维权限）不显示加入 / 移出 / 行操作', async () => {
    login('finance')
    await mountTab()
    expect(byId('llm-pool-add').exists()).toBe(false)
    expect(allById('llm-pool-remove')).toHaveLength(0)
    expect(allById('llm-node-drain')).toHaveLength(0)
    expect(allById('llm-node-remove')).toHaveLength(0)
  })
})

describe('NodesTab 自动刷新', () => {
  it('可见时每 10 秒刷新池与节点；切走 / 卸载后停止；失败不弹提示', async () => {
    vi.useFakeTimers()
    const err = vi.spyOn(ElMessage, 'error')
    await mountTab(true)
    expect(calls(get, '/llm/pool')).toHaveLength(1)
    expect(calls(get, '/llm/nodes')).toHaveLength(1)
    await vi.advanceTimersByTimeAsync(10_000)
    expect(calls(get, '/llm/pool')).toHaveLength(2)
    expect(calls(get, '/llm/nodes')).toHaveLength(2)

    // 自动刷新失败：不弹提示，只在页内显示错误
    get.mockImplementationOnce(async () => {
      throw new ApiError(500, 'internal', '服务器错误')
    })
    await vi.advanceTimersByTimeAsync(10_000)
    expect(calls(get, '/llm/pool')).toHaveLength(3)
    expect(err).not.toHaveBeenCalled()
    expect(byId('llm-pool-error').exists()).toBe(true)

    await wrapper!.setProps({ active: false })
    await vi.advanceTimersByTimeAsync(30_000)
    expect(calls(get, '/llm/pool')).toHaveLength(3)

    await wrapper!.setProps({ active: true }) // 切回来立即刷新并恢复定时
    await flushPromises()
    expect(calls(get, '/llm/pool')).toHaveLength(4)
    await vi.advanceTimersByTimeAsync(10_000)
    expect(calls(get, '/llm/pool')).toHaveLength(5)

    wrapper!.unmount()
    wrapper = undefined
    await vi.advanceTimersByTimeAsync(30_000)
    expect(calls(get, '/llm/pool')).toHaveLength(5)
  })

  it('上一轮还没返回时跳过本轮，不叠加请求', async () => {
    vi.useFakeTimers()
    await mountTab(true)
    expect(calls(get, '/llm/pool')).toHaveLength(1)
    const base = get.getMockImplementation()!
    get.mockImplementation((path: string, q?: Record<string, unknown>) => (path === '/llm/pool' ? new Promise(() => {}) : base(path, q)))
    await vi.advanceTimersByTimeAsync(10_000) // 第 2 次请求挂起
    await vi.advanceTimersByTimeAsync(20_000) // 后两轮都跳过
    expect(calls(get, '/llm/pool')).toHaveLength(2)
  })

  it('抽屉打开时随定时刷新静默重载：不弹提示、不转圈', async () => {
    vi.useFakeTimers()
    const err = vi.spyOn(ElMessage, 'error')
    await mountTab(true)
    await allById('llm-node-open')[0].trigger('click')
    await flushPromises()
    expect(calls(get, '/llm/nodes/n1')).toHaveLength(1)
    const base = get.getMockImplementation()!
    let release: () => void = () => {}
    get.mockImplementation((path: string, q?: Record<string, unknown>) => {
      if (path !== '/llm/nodes/n1') return base(path, q)
      return new Promise((_, reject) => {
        release = () => reject(new ApiError(500, 'internal', '服务器错误'))
      })
    })
    await vi.advanceTimersByTimeAsync(10_000)
    expect(calls(get, '/llm/nodes/n1')).toHaveLength(2)
    const refresh = inBody('llm-node-refresh')!
    expect(refresh.classList.contains('is-loading')).toBe(false) // 静默刷新不转圈
    release()
    await flushPromises()
    expect(err).not.toHaveBeenCalled()
    expect(inBody('llm-node-model')!.textContent).toBe('qwen3-32b') // 失败保留已显示的数据
  })

  it('不可见时挂载不加载、不计时', async () => {
    vi.useFakeTimers()
    await mountTab(false)
    await vi.advanceTimersByTimeAsync(30_000)
    expect(calls(get, '/llm/pool')).toHaveLength(0)
  })
})

describe('NodesTab 多机节点组（计划 13，A-LLM-11 / 12）', () => {
  const groupNode = (): LLMNode => node({
    id: 'g1', size: 2, cluster_id: 'clu-sys00001', app_cluster_id: 'acd-9', machine_id: 11, machine_name: 'A01-01',
    members: [
      { rank: 0, machine_id: 11, machine_name: 'A01-01', instance_id: 'ins-h0000000', instance_state: 'running', suspended: false, deployment: { state: 'running', health: 'ok', progress: 100 } },
      { rank: 1, machine_id: 14, machine_name: 'A01-04', instance_id: 'ins-w1111111', instance_state: 'running', suspended: false, deployment: { state: 'installing', health: 'unknown', progress: 40 } },
    ],
    app_cluster: { id: 'acd-9', status: 'installing', health: 'unknown', progress: 70 },
  })
  const fabricPool = (): LLMPool => ({
    ...poolData(),
    machines: poolData().machines.map((m, i) => ({ ...m, fabric_id: i === 0 ? 3 : null, fabric_name: i === 0 ? '机房 A 以太网' : '', node: m.node ? { ...m.node, rank: 0, size: 2 } : undefined })),
    fabrics: [{ fabric_id: 3, name: '机房 A 以太网', kind: 'lan', rdma: false, total: 6, free: 4, groups: { '2': 2, '4': 1 } }],
  })

  it('机器池按集群网络汇总可组副本数；机器行显示集群网络与组内 rank', async () => {
    const base = get.getMockImplementation()!
    get.mockImplementation(async (path: string, q?: Record<string, unknown>) => (path === '/llm/pool' ? fabricPool() : base(path, q)))
    await mountTab()
    const rows = wrapper!.findAll('[data-testid="llm-pool-fabrics"] [data-testid="llm-pool-fabric"]')
    expect(rows).toHaveLength(1)
    expect(rows[0].text()).toContain('机房 A 以太网')
    expect(rows[0].find('[data-testid="llm-pool-fabric-free"]').text()).toBe('4 / 6')
    expect(rows[0].find('[data-testid="llm-pool-fabric-groups"]').text()).toBe('2 台 × 2 · 4 台 × 1')
    const pr = allById('llm-pool-row')
    expect(pr[0].find('[data-testid="llm-pool-fabric-name"]').text()).toBe('机房 A 以太网')
    expect(pr[1].find('[data-testid="llm-pool-fabric-name"]').text()).toBe('—')
    expect(pr[0].find('[data-testid="llm-pool-node"]').text()).toContain('rank 0 / 2')
  })

  it('没有 fabric 时不显示汇总表', async () => {
    await mountTab()
    expect(byId('llm-pool-fabrics').exists()).toBe(false)
  })

  it('组节点：「2 台」徽标，展开成员（rank / 机器 / 实例 / 部署进度）；操作作用于整组', async () => {
    nodes = [groupNode()]
    await mountTab()
    expect(byId('llm-node-size').text()).toBe('2 台')
    expect(byId('llm-node-members').exists()).toBe(false)
    await byId('llm-node-members-toggle').trigger('click')
    const ms = wrapper!.findAll('[data-testid="llm-node-members"] [data-testid="llm-node-member"]')
    expect(ms).toHaveLength(2)
    expect(ms[1].text()).toContain('A01-04')
    expect(ms[1].text()).toContain('ins-w1111111')
    expect(ms[1].text()).toContain('部署中 40%')
    await byId('llm-node-drain').trigger('click')
    await flushPromises()
    expect(inBody('llm-node-op-text')!.textContent).toContain('整组 2 台')
  })

  it('抽屉：组节点显示成员表与集群部署聚合，链接到集群页', async () => {
    nodes = [groupNode()]
    const base = get.getMockImplementation()!
    get.mockImplementation(async (path: string, q?: Record<string, unknown>) => (path === '/llm/nodes/g1' ? { node: groupNode(), events: [], deployment_events: [] } : base(path, q)))
    await mountTab()
    await byId('llm-node-open').trigger('click')
    await flushPromises()
    expect(document.body.querySelectorAll('[data-testid="llm-node-drawer-members"] [data-testid="llm-node-drawer-member"]')).toHaveLength(2)
    expect(inBody('llm-node-app-cluster')!.textContent).toContain('部署中')
    expect(inBody('llm-node-app-cluster')!.textContent).toContain('70%')
    expect(inBody('llm-node-cluster-link')!.getAttribute('href')).toContain('/clusters/clu-sys00001')
  })
})
