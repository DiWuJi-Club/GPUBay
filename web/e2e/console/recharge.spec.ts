import { expect, test, type Page } from '@playwright/test'
import { isMobile, loginConsole, nav, seedUser, settle, toast, userApi } from '../helpers/console'
import { hooks } from '../helpers/hooks'

async function createRecharge(phone: string, amount = 5000) {
  const api = await userApi(phone)
  const r = await api.post('/api/v1/recharges', { data: { amount, channel: 'alipay' } })
  if (r.status() !== 201) throw new Error(await r.text())
  const body = (await r.json()) as { id: string; qr_code: string }
  return { api, id: body.id, qr: body.qr_code }
}

function setVisibility(page: Page, state: 'visible' | 'hidden') {
  return page.evaluate((s) => {
    Object.defineProperty(document, 'visibilityState', { value: s, configurable: true })
    document.dispatchEvent(new Event('visibilitychange'))
  }, state)
}

/**
 * 统计页面上出现过的某类提示（.el-message--error / --success）条数。提示会自动消失，
 * 只看当前 DOM 里的数量证明不了「没有重复提示 / 没有提示」，所以用 MutationObserver 累计。
 */
async function watchToasts(page: Page, kind: 'error' | 'success') {
  await page.evaluate((k) => {
    const w = window as unknown as Record<string, number>
    const key = `__toasts_${k}`
    w[key] = 0
    new MutationObserver((ms) => {
      for (const m of ms)
        for (const n of m.addedNodes)
          if (n instanceof HTMLElement && (n.matches(`.el-message--${k}`) || n.querySelector(`.el-message--${k}`))) w[key] += 1
    }).observe(document.body, { childList: true, subtree: true })
  }, kind)
  return () => page.evaluate((k) => (window as unknown as Record<string, number>)[`__toasts_${k}`], kind)
}

test('C-RCH-01 发起页金额校验', async ({ page }) => {
  const u = await seedUser()
  let posts = 0
  page.on('request', (r) => {
    if (r.method() === 'POST' && r.url().endsWith('/api/v1/recharges')) posts += 1
  })
  await loginConsole(page, u.phone)
  await page.goto('/wallet/recharge?redirect=/instances')
  await expect(page.getByTestId('recharge-custom')).toHaveValue('100') // 默认 100 元
  await page.getByTestId('recharge-preset-500').click()
  await expect(page.getByTestId('recharge-custom')).toHaveValue('500')
  await page.getByTestId('recharge-preset-1000').click()
  await expect(page.getByTestId('recharge-custom')).toHaveValue('1000')
  await page.getByTestId('recharge-preset-100').click()
  await expect(page.getByTestId('recharge-custom')).toHaveValue('100')
  for (const [value, error] of [
    ['0.5', '充值金额需在 1 元到 100 万元之间'],
    ['1000001', '充值金额需在 1 元到 100 万元之间'],
    ['1.234', '金额格式不正确'],
  ]) {
    await page.getByTestId('recharge-custom').fill(value)
    await page.getByTestId('recharge-submit').click()
    await expect(page.getByTestId('recharge-amount-item').getByTestId('recharge-amount-error')).toHaveText(error)
  }
  await test.step('改动金额后错误提示消失', async () => {
    await page.getByTestId('recharge-custom').fill('1.23')
    await expect(page.getByTestId('recharge-amount-error')).toHaveCount(0)
  })
  expect(posts).toBe(0)
  await page.getByTestId('recharge-custom').fill('88.5')
  await page.getByTestId('recharge-channel-alipay').click()
  await page.getByTestId('recharge-submit').click()
  await expect(page).toHaveURL(/\/wallet\/recharge\/rch-[^?]+\?redirect=(%2F|\/)instances$/) // 保留 redirect
  await expect(page.getByTestId('rchorder-amount')).toHaveText('¥88.50')
  await expect(page.getByTestId('rchorder-channel')).toHaveText('支付宝')
  expect(posts).toBe(1)
  await test.step('支付成功后「返回」回到 redirect 指定的页面', async () => {
    await page.getByTestId('rchorder-mock-pay').click()
    await expect(page.getByTestId('rchorder-paid')).toContainText('¥88.50')
    await page.getByTestId('rchorder-back').click()
    await expect(page).toHaveURL(/\/instances$/)
  })
})

test('C-RCH-02 渠道按设备类型：电脑两个渠道且缩窄视口不变；手机只有支付宝', async ({ page }, info) => {
  const u = await seedUser()
  await loginConsole(page, u.phone)
  await test.step('已实名用户在钱包页点「充值」进入发起页', async () => {
    await nav(page, info, 'wallet')
    await page.getByTestId('wallet-recharge-btn').click()
    await expect(page).toHaveURL(/\/wallet\/recharge$/)
    await expect(page.getByTestId('recharge-submit')).toBeVisible()
  })
  if (isMobile(info)) {
    await expect(page.getByTestId('recharge-channel-alipay')).toBeVisible()
    await expect(page.getByTestId('recharge-channel-wechat')).toHaveCount(0)
  } else {
    await expect(page.getByTestId('recharge-channel-wechat')).toBeVisible()
    await expect(page.getByTestId('recharge-channel-alipay')).toBeVisible()
    await page.setViewportSize({ width: 390, height: 844 })
    await expect(page.getByTestId('recharge-channel-wechat')).toBeVisible()
    await expect(page.getByTestId('recharge-channel-alipay')).toBeVisible()
  }
  await test.step('后端只开通微信：手机显示「暂不支持手机端充值」且不能提交，电脑只显示微信', async () => {
    await page.route('**/api/v1/pay/channels', (route) => route.fulfill({ status: 200, contentType: 'application/json', body: '{"items":["wechat"]}' }))
    await page.reload()
    if (isMobile(info)) {
      await expect(page.getByTestId('recharge-unavailable')).toContainText('当前暂不支持手机端充值')
      await expect(page.getByTestId('recharge-channel-wechat')).toHaveCount(0)
      await expect(page.getByTestId('recharge-submit')).toBeDisabled()
    } else {
      await expect(page.getByTestId('recharge-channel-wechat')).toBeVisible()
      await expect(page.getByTestId('recharge-channel-alipay')).toHaveCount(0)
      await expect(page.getByTestId('recharge-unavailable')).toHaveCount(0)
      await expect(page.getByTestId('recharge-submit')).toBeEnabled()
    }
    await page.unroute('**/api/v1/pay/channels')
  })
  await test.step('渠道加载失败：显示错误与重试（不误报「暂不支持」），重试后恢复', async () => {
    let calls = 0
    await page.route('**/api/v1/pay/channels', (route) => {
      calls += 1
      return route.fulfill({ status: 500, contentType: 'application/json', body: '{"code":"internal","message":"服务器内部错误，请稍后重试"}' })
    })
    await page.reload()
    await expect(page.getByTestId('recharge-error')).toContainText('服务器内部错误，请稍后重试')
    await expect(page.getByTestId('recharge-unavailable')).toHaveCount(0)
    await expect(page.getByTestId('recharge-submit')).toBeDisabled()
    expect(calls).toBe(1)
    await page.unroute('**/api/v1/pay/channels')
    await page.getByTestId('recharge-retry').click()
    await expect(page.getByTestId('recharge-error')).toHaveCount(0)
    await expect(page.getByTestId('recharge-channel-alipay')).toBeVisible()
    await expect(page.getByTestId('recharge-submit')).toBeEnabled()
  })
})

test('C-RCH-03 支付态：倒计时、3 秒轮询、模拟支付成功、余额更新、离开后停止轮询', async ({ page }, info) => {
  const u = await seedUser({ balance: 100000 })
  await page.clock.install()
  await loginConsole(page, u.phone)
  await page.goto('/wallet/recharge')
  await page.getByTestId('recharge-preset-50').click()
  await page.getByTestId('recharge-channel-alipay').click()
  await page.getByTestId('recharge-submit').click()
  await expect(page).toHaveURL(/\/wallet\/recharge\/rch-/)
  const rid = page.url().split('/').pop()!
  let polls = 0
  page.on('request', (r) => {
    if (r.method() === 'GET' && r.url().endsWith(`/api/v1/recharges/${rid}`)) polls += 1
  })
  await expect(page.getByTestId('rchorder-amount')).toHaveText('¥50.00')
  await expect(page.getByTestId('rchorder-channel')).toHaveText('支付宝')
  await expect(page.getByTestId('rchorder-countdown')).toHaveText('15:00')
  await page.clock.runFor(1000)
  await expect(page.getByTestId('rchorder-countdown')).toHaveText('14:59')
  const before = polls
  await test.step('3 秒轮询一次，并按服务器剩余秒数重新校准倒计时', async () => {
    await hooks.advance(60) // 服务器时间前进 1 分钟：剩余 840 秒
    await page.clock.runFor(3000)
    await expect.poll(() => polls).toBe(before + 1)
    await expect(page.getByTestId('rchorder-countdown')).toHaveText('14:00')
  })
  await expect(page.getByTestId('rchorder-mock')).toContainText('开发环境模拟支付')
  if (isMobile(info)) {
    await expect(page.getByTestId('rchorder-qr')).toHaveCount(0)
    await expect(page.getByTestId('rchorder-alipay-link')).toBeVisible()
  } else {
    await expect(page.getByTestId('rchorder-qr')).toBeVisible()
    expect(await page.getByTestId('rchorder-qr').evaluate((c) => (c as HTMLCanvasElement).toDataURL().length)).toBeGreaterThan(1000) // 画布上确实画了二维码
    await expect(page.getByTestId('rchorder-alipay-link')).toHaveCount(0)
  }
  await test.step('轮询失败：只提示一次、不重复刷屏，仍按 3 秒间隔查询，恢复后照常', async () => {
    const errToasts = await watchToasts(page, 'error')
    const isPoll = (url: URL) => url.pathname === `/api/v1/recharges/${rid}`
    await page.route(isPoll, (route) =>
      route.request().method() === 'GET'
        ? route.fulfill({ status: 500, contentType: 'application/json', body: '{"code":"internal","message":"服务器内部错误，请稍后重试"}' })
        : route.fallback(),
    )
    const p0 = polls
    await page.clock.runFor(3000)
    await expect.poll(() => polls).toBe(p0 + 1)
    await toast(page, '服务器内部错误，请稍后重试')
    await page.clock.runFor(6000)
    await expect.poll(() => polls).toBe(p0 + 3) // 固定间隔，没有因失败而紧密重试
    expect(await errToasts()).toBe(1)
    await expect(page.getByTestId('rchorder-pending')).toBeVisible() // 已显示的支付态不被错误替换
    await page.unroute(isPoll)
    await page.clock.runFor(3000)
    await expect.poll(() => polls).toBe(p0 + 4)
    await expect(page.getByTestId('rchorder-pending')).toBeVisible()
    expect(await errToasts()).toBe(1)
  })
  await page.getByTestId('rchorder-mock-pay').click()
  await toast(page, '充值成功')
  await expect(page.getByTestId('rchorder-paid')).toBeVisible()
  await page.getByTestId('rchorder-back').click()
  await expect(page).toHaveURL(/\/wallet$/)
  await expect(page.getByTestId('wallet-balance')).toHaveText('¥1,050.00')
  const afterLeave = polls
  await page.clock.runFor(9000)
  expect(polls).toBe(afterLeave) // 离开页面后定时器已清理
})

test('C-RCH-04 手机拉起支付宝链接与回页立即查询；电脑不可见时暂停轮询、恢复时立即查一次', async ({ page }, info) => {
  const u = await seedUser({ balance: 100000 })
  const { api, id, qr } = await createRecharge(u.phone)
  // 两个 project 都接管浏览器定时器：否则 3 秒轮询自己就能查到已支付，「回到页面立即查询」这条逻辑有没有都会通过
  await page.clock.install()
  await loginConsole(page, u.phone)
  let polls = 0
  page.on('request', (r) => {
    if (r.method() === 'GET' && r.url().endsWith(`/api/v1/recharges/${id}`)) polls += 1
  })
  await page.goto(`/wallet/recharge/${id}`)
  await expect(page.getByTestId('rchorder-pending')).toBeVisible()
  if (isMobile(info)) {
    const href = await page.getByTestId('rchorder-alipay-link').getAttribute('href')
    expect(href).toBe(`alipays://platformapi/startapp?saId=10000007&qrcode=${encodeURIComponent(qr)}`)
    await expect(page.getByTestId('rchorder-open-browser')).toHaveAttribute('href', qr)
  } else {
    await expect(page.getByTestId('rchorder-alipay-link')).toHaveCount(0)
    await expect(page.getByTestId('rchorder-open-browser')).toHaveCount(0)
  }
  // 用户切到支付宝（页面不可见）：轮询暂停
  await setVisibility(page, 'hidden')
  const paused = polls
  await page.clock.runFor(9000)
  expect(polls).toBe(paused)
  // 支付宝侧完成支付
  await api.post(`/api/v1/dev/recharges/${id}/pay`, { data: {} })
  await expect(page.getByTestId('rchorder-pending')).toBeVisible() // 页面不可见期间没有自己查到
  // 回到页面：不等下一个 3 秒，立即查询一次并显示成功
  await setVisibility(page, 'visible')
  await expect.poll(() => polls).toBe(paused + 1)
  await expect(page.getByTestId('rchorder-paid')).toBeVisible()
  await toast(page, '充值成功')
  // 已支付后不再轮询
  await page.clock.runFor(9000)
  expect(polls).toBe(paused + 1)
})

test('C-RCH-05 关单后显示已关闭', async ({ page }) => {
  const u = await seedUser()
  const { id } = await createRecharge(u.phone)
  await page.clock.install()
  await loginConsole(page, u.phone)
  let polls = 0
  page.on('request', (r) => {
    if (r.method() === 'GET' && r.url().endsWith(`/api/v1/recharges/${id}`)) polls += 1
  })
  await page.goto(`/wallet/recharge/${id}`)
  await expect(page.getByTestId('rchorder-countdown')).toHaveText(/^\d{2}:\d{2}$/)
  await test.step('倒计时归零再查一次，仍待支付则显示已过期，之后不再轮询', async () => {
    await setVisibility(page, 'hidden') // 暂停 3 秒轮询，只留倒计时自己的「归零再查」
    await hooks.advance(900)
    const p0 = polls
    await page.clock.runFor(900_000)
    await expect(page.getByTestId('rchorder-expired')).toContainText('二维码已过期，请重新发起')
    expect(polls).toBe(p0 + 1)
    await expect(page.getByTestId('rchorder-countdown')).toHaveText('00:00')
    await expect(page.getByTestId('rchorder-qr')).toHaveCount(0)
    await expect(page.getByTestId('rchorder-mock')).toHaveCount(0)
    await setVisibility(page, 'visible')
    await page.clock.runFor(9000)
    expect(polls).toBe(p0 + 1)
    await page.getByTestId('rchorder-expired').getByTestId('rchorder-again').click()
    await expect(page).toHaveURL(/\/wallet\/recharge$/)
  })
  await hooks.advance(7200)
  await hooks.sweep()
  await page.goto(`/wallet/recharge/${id}`)
  await expect(page.getByTestId('rchorder-closed')).toContainText('已关闭，请重新发起')
  await expect(page.getByTestId('rchorder-pending')).toHaveCount(0)
  await page.getByTestId('rchorder-again').click()
  await expect(page).toHaveURL(/\/wallet\/recharge$/)
})

test('C-RCH-06 待支付过多', async ({ page }) => {
  const u = await seedUser()
  const api = await userApi(u.phone)
  for (let i = 0; i < 5; i++) {
    const r = await api.post('/api/v1/recharges', { data: { amount: 1000, channel: 'alipay' } })
    expect(r.status()).toBe(201)
  }
  let posts = 0
  page.on('request', (r) => {
    if (r.method() === 'POST' && r.url().endsWith('/api/v1/recharges')) posts += 1
  })
  await loginConsole(page, u.phone)
  await page.goto('/wallet/recharge')
  await page.getByTestId('recharge-preset-50').click()
  await test.step('创建失败（500）：只发一次请求、只提示一次，留在发起页可再次提交', async () => {
    await page.route('**/api/v1/recharges', (route) =>
      route.fulfill({ status: 500, contentType: 'application/json', body: JSON.stringify({ code: 'internal', message: '服务器内部错误，请稍后重试', request_id: 'req-rch-500' }) }),
    )
    const errToasts = await watchToasts(page, 'error')
    await page.getByTestId('recharge-submit').click()
    await toast(page, '服务器内部错误，请稍后重试（请求号 req-rch-500）')
    await settle(page)
    expect(posts).toBe(1)
    expect(await errToasts()).toBe(1)
    await expect(page).toHaveURL(/\/wallet\/recharge$/)
    await expect(page.getByTestId('recharge-submit')).toBeEnabled()
  })
  await test.step('未实名（kyc_required）：弹出实名引导', async () => {
    await page.unroute('**/api/v1/recharges')
    await page.route('**/api/v1/recharges', (route) =>
      route.fulfill({ status: 403, contentType: 'application/json', body: '{"code":"kyc_required","message":"请先完成实名认证"}' }),
    )
    await page.getByTestId('recharge-submit').click()
    await expect(page.getByTestId('guide-dialog')).toHaveText('按监管要求，下单与充值前需要完成实名认证。')
    await page.getByTestId('guide-cancel').click()
    await page.unroute('**/api/v1/recharges')
  })
  const p0 = posts
  await page.getByTestId('recharge-submit').click()
  await toast(page, '待支付的充值单过多')
  await expect(page).toHaveURL(/\/wallet\/recharge$/)
  expect(posts).toBe(p0 + 1)
})

test('C-RCH-07 已支付单重进直接显示成功页', async ({ page }) => {
  const u = await seedUser()
  const { api, id } = await createRecharge(u.phone, 8850)
  await api.post(`/api/v1/dev/recharges/${id}/pay`, { data: {} })
  await loginConsole(page, u.phone)
  const successToasts = await watchToasts(page, 'success') // 在站内跳转前装好，覆盖充值单页的首次加载
  await page.evaluate((path) => {
    history.pushState({}, '', path)
    dispatchEvent(new PopStateEvent('popstate'))
  }, `/wallet/recharge/${id}`)
  await expect(page.getByTestId('rchorder-paid')).toContainText('¥88.50')
  await expect(page.getByTestId('rchorder-paid-at')).toHaveText(/到账时间 \d{4}-\d{2}-\d{2} \d{2}:\d{2}/)
  await expect(page.getByTestId('rchorder-pending')).toHaveCount(0)
  await settle(page)
  expect(await successToasts()).toBe(0) // 不是在本页由待支付变为已支付，不弹「充值成功」
  await test.step('首次加载失败：显示错误与重试，重试后显示成功页', async () => {
    let calls = 0
    const isOrder = (url: URL) => url.pathname === `/api/v1/recharges/${id}`
    await page.route(isOrder, (route) => {
      calls += 1
      return route.fulfill({ status: 500, contentType: 'application/json', body: '{"code":"internal","message":"服务器内部错误，请稍后重试"}' })
    })
    await page.reload()
    await expect(page.getByTestId('rchorder-error')).toContainText('服务器内部错误，请稍后重试')
    await expect(page.getByTestId('rchorder-paid')).toHaveCount(0)
    await settle(page)
    expect(calls).toBe(1)
    await page.unroute(isOrder)
    await page.getByTestId('rchorder-retry').click()
    await expect(page.getByTestId('rchorder-paid')).toContainText('¥88.50')
    await expect(page.getByTestId('rchorder-error')).toHaveCount(0)
  })
})
