import { expect, request, test, type APIRequestContext, type Browser, type Page } from '@playwright/test'
import {
  adminApi, adminPut, clickTwiceSync, createAdmin, drainTasks, enterTotpDialog, expectAdminUrl, expectStillOpen, failOnce, getJSON, loginAdmin, logout, recordRequests, type SeededAdmin, trackToasts,
} from '../helpers/admin'
import { toast } from '../helpers/console'
import { CONSOLE_URL } from '../helpers/hooks'
import { runStamp } from '../helpers/phone'

type AdminPrice = { duration: '1d' | '1w' | '1m'; amount: number; enabled: boolean }
type AdminProduct = { id: number; prices: AdminPrice[] }
type AdminImage = { id: number; name: string; ref: string; description: string; enabled: boolean; sort: number }
type PublicProduct = { id: number; prices: { duration: string; amount: number }[] }
type PublicImage = { id: number; name: string }

const PRICE_RANGE = '价格需在 0.01 元到 100 万元之间'

/** 控制台（用户端）公开接口看到的商品与镜像：只含在售档位与启用的镜像。 */
async function publicCatalog(productId: number) {
  const ctx = await request.newContext({ baseURL: CONSOLE_URL })
  const products = ((await (await ctx.get('/api/v1/products')).json()) as { items: PublicProduct[] }).items
  const images = ((await (await ctx.get('/api/v1/images')).json()) as { items: PublicImage[] }).items
  await ctx.dispose()
  return { product: products.find((p) => p.id === productId), images }
}
const firstProduct = async (ctx: APIRequestContext) => (await getJSON<{ items: AdminProduct[] }>(ctx, '/admin/api/v1/products')).items[0]
const adminImages = async (ctx: APIRequestContext) => (await getJSON<{ items: AdminImage[] }>(ctx, '/admin/api/v1/images')).items

/** 把价格改回测试开始时的值（数据库整轮共享：控制台市场用例依赖初始价格与档位）。 */
async function restorePrices(ctx: APIRequestContext, admin: SeededAdmin, original: AdminProduct) {
  const now = await firstProduct(ctx)
  for (const p of original.prices) {
    const cur = now.prices.find((x) => x.duration === p.duration)
    if (cur?.amount === p.amount && cur.enabled === p.enabled) continue
    await adminPut(ctx, admin, `/admin/api/v1/products/${original.id}/prices/${p.duration}`, { amount: p.amount, enabled: p.enabled })
  }
  expect((await firstProduct(ctx)).prices.map(({ duration, amount, enabled }) => ({ duration, amount, enabled }))).toEqual(
    original.prices.map(({ duration, amount, enabled }) => ({ duration, amount, enabled })),
  )
}

/** 控制台市场页（另开一个未登录的浏览器上下文）。 */
async function consoleMarket(browser: Browser) {
  const ctx = await browser.newContext({ baseURL: CONSOLE_URL, viewport: { width: 1280, height: 720 } })
  const page = await ctx.newPage()
  await page.goto('/market')
  const card = page.getByTestId('market-card').first()
  await expect(card.getByTestId('market-price-1d')).toBeVisible()
  return { page, card, close: () => ctx.close() }
}

const imageRow = (page: Page, name: string) => page.getByRole('row').filter({ has: page.getByTestId('img-name').getByText(name, { exact: true }) })

test('A-CAT-01 修改 1 天价格：元精确转分、非法 / 0 / 负数本地拦截、动态码后原请求带 totp 只重发一次、控制台价格同步；首次加载失败可重试；保存失败只提示一次并保留输入', async ({ page }) => {
  const admin = await createAdmin('super')
  const ctx = await adminApi(admin)
  const original = await firstProduct(ctx)
  const orig1d = original.prices.find((p) => p.duration === '1d')!
  const toasts = await trackToasts(page)
  const productGets = recordRequests(page, 'GET', '/products')
  const puts = recordRequests(page, 'PUT', `/products/${original.id}/prices/1d`)
  const amount = page.getByTestId('price-amount')
  const product = page.locator(`[data-testid="cat-product"][data-product-id="${original.id}"]`)
  try {
    await loginAdmin(page, admin)

    await test.step('首次加载失败：页内错误 + 重试，不自动重试；重试后显示价格', async () => {
      await failOnce(page, '/products', 'req-cat-1')
      await page.getByTestId('menu-catalog').click()
      await expectAdminUrl(page, '/catalog')
      await expect(page.getByTestId('cat-error')).toContainText('服务器内部错误（请求号 req-cat-1）')
      await expect(page.getByTestId('cat-product')).toHaveCount(0)
      await drainTasks(page)
      expect(productGets).toHaveLength(1)
      expect(await toasts()).toEqual(['服务器内部错误（请求号 req-cat-1）'])
      await page.getByTestId('cat-retry').click()
      await expect(page.getByTestId('cat-error')).toHaveCount(0)
      await expect(product.getByTestId('cat-price-amount-1d')).toHaveText('¥69.00')
      expect(productGets).toHaveLength(2)
    })

    await test.step('格式错误、0、负数、超上限：页内提示，不发请求', async () => {
      await product.getByTestId('cat-price-edit-1d').click()
      await expect(amount).toHaveValue('69.00')
      for (const bad of ['', 'abc', '-5', '-0.01', '1.234', '1e3', '６９', '69.']) {
        await amount.fill(bad)
        await page.getByTestId('price-submit').click()
        await expect(page.getByTestId('price-error')).toHaveText('金额格式不正确')
      }
      for (const bad of ['0', '0.00', '1000000.01']) {
        await amount.fill(bad)
        await page.getByTestId('price-submit').click()
        await expect(page.getByTestId('price-error')).toHaveText(PRICE_RANGE)
      }
      await drainTasks(page)
      expect(puts).toHaveLength(0)
    })

    await test.step('取消动态码：只发出不带 totp 的一次，无提示，弹窗与输入保留', async () => {
      await amount.fill(' 79.1 ')
      await page.getByTestId('price-submit').click()
      await expect(page.getByTestId('price-error')).toHaveCount(0)
      await expect(page.getByTestId('totp-dialog')).toBeVisible()
      await page.getByTestId('totp-cancel').click()
      await expect(page.getByTestId('totp-dialog')).toBeHidden()
      await expect(page.getByTestId('price-dialog')).toBeVisible()
      await expect(amount).toHaveValue(' 79.1 ')
      await drainTasks(page)
      expect(puts.map((p) => p.body)).toEqual([{ amount: 7910, enabled: true }])
      expect(await toasts()).toEqual(['服务器内部错误（请求号 req-cat-1）'])
      expect((await publicCatalog(original.id)).product?.prices.find((p) => p.duration === '1d')?.amount).toBe(orig1d.amount)
    })

    await test.step('连点保存：弹动态码，原请求带 totp 只重发一次；¥79.10 = 7910 分，控制台接口与市场页同步', async () => {
      await clickTwiceSync(page.getByTestId('price-submit'))
      await enterTotpDialog(page, admin.totp_secret!)
      await toast(page, '价格已保存')
      await expect(page.getByTestId('price-dialog')).toBeHidden()
      await expect(product.getByTestId('cat-price-amount-1d')).toHaveText('¥79.10')
      await drainTasks(page)
      expect(puts.slice(1).map((p) => p.body)).toEqual([
        { amount: 7910, enabled: true },
        { amount: 7910, enabled: true, totp: expect.stringMatching(/^\d{6}$/) },
      ])
      expect((await publicCatalog(original.id)).product?.prices.find((p) => p.duration === '1d')?.amount).toBe(7910)
      const market = await consoleMarket(page.context().browser()!)
      await expect(market.card.getByTestId('market-price-1d')).toHaveText('1 天 ¥79.10')
      await market.close()
    })

    await test.step('保存失败（500）：只提示一次，弹窗与输入保留；再次保存（5 分钟内免动态码）改回 ¥69.00', async () => {
      await product.getByTestId('cat-price-edit-1d').click()
      await expect(amount).toHaveValue('79.10')
      await amount.fill('69')
      await failOnce(page, `/products/${original.id}/prices/1d`, 'req-price-1')
      const before = (await toasts()).length
      await page.getByTestId('price-submit').click()
      await toast(page, '服务器内部错误（请求号 req-price-1）')
      await expectStillOpen(page, 'price-dialog', '服务器内部错误（请求号 req-price-1）')
      await expect(amount).toHaveValue('69')
      expect((await toasts()).slice(before)).toEqual(['服务器内部错误（请求号 req-price-1）'])
      await page.getByTestId('price-submit').click()
      await toast(page, '价格已保存')
      await expect(page.getByTestId('totp-dialog')).toBeHidden()
      await expect(product.getByTestId('cat-price-amount-1d')).toHaveText('¥69.00')
      await drainTasks(page)
      expect(puts.slice(3).map((p) => p.body)).toEqual([{ amount: 6900, enabled: true }, { amount: 6900, enabled: true }])
      expect((await publicCatalog(original.id)).product?.prices.find((p) => p.duration === '1d')?.amount).toBe(6900)
    })
  } finally {
    await restorePrices(ctx, admin, original)
  }
})

test('A-CAT-02 下架 1 周档位（动态码）后控制台市场不再显示，再上架恢复；连点只发一次；失败回到原状态；运维 / 财务无菜单且 403', async ({ page, browser }) => {
  await test.step('运维、财务：没有「价格与镜像」菜单，直接访问显示 403', async () => {
    for (const role of ['ops', 'finance'] as const) {
      await loginAdmin(page, await createAdmin(role))
      await expect(page.getByTestId('menu-audit')).toBeVisible()
      await expect(page.getByTestId('menu-catalog')).toHaveCount(0)
      await page.goto('/admin/catalog')
      await expect(page.getByTestId('forbidden-page')).toBeVisible()
      await expectAdminUrl(page, '/403', { from: '/catalog' })
      await logout(page)
    }
  })

  const admin = await createAdmin('super')
  const ctx = await adminApi(admin)
  const original = await firstProduct(ctx)
  const orig1w = original.prices.find((p) => p.duration === '1w')!
  expect(orig1w.enabled).toBe(true)
  const toasts = await trackToasts(page)
  const puts = recordRequests(page, 'PUT', `/products/${original.id}/prices/1w`)
  const product = page.locator(`[data-testid="cat-product"][data-product-id="${original.id}"]`)
  const swEl = product.getByTestId('cat-price-enabled-1w')
  const sw = swEl.getByRole('switch') // aria-checked 在内层 input 上
  const durations = async () => (await publicCatalog(original.id)).product?.prices.map((p) => p.duration)
  try {
    await loginAdmin(page, admin)
    await page.getByTestId('menu-catalog').click()
    await expect(sw).toBeChecked()

    await test.step('取消动态码：开关回到「启用」，只发一次不带 totp 的请求，无提示，市场不变', async () => {
      await swEl.click()
      await expect(page.getByTestId('totp-dialog')).toBeVisible()
      await page.getByTestId('totp-cancel').click()
      await expect(page.getByTestId('totp-dialog')).toBeHidden()
      await expect(sw).toBeChecked()
      await drainTasks(page)
      expect(puts.map((p) => p.body)).toEqual([{ amount: orig1w.amount, enabled: false }])
      expect(await toasts()).toEqual([])
      expect(await durations()).toEqual(['1d', '1w', '1m'])
    })

    await test.step('连点下架：动态码后原请求带 totp 只重发一次；控制台接口与市场页都不再有 1 周', async () => {
      await clickTwiceSync(swEl)
      await enterTotpDialog(page, admin.totp_secret!)
      await toast(page, '价格已保存')
      await expect(sw).not.toBeChecked()
      await expect(product.getByTestId('cat-price-amount-1w')).toHaveText('¥414.00')
      await drainTasks(page)
      expect(puts.slice(1).map((p) => p.body)).toEqual([
        { amount: orig1w.amount, enabled: false },
        { amount: orig1w.amount, enabled: false, totp: expect.stringMatching(/^\d{6}$/) },
      ])
      expect(await durations()).toEqual(['1d', '1m'])
      const market = await consoleMarket(browser)
      await expect(market.card.getByTestId('market-price-1m')).toBeVisible()
      await expect(market.card.getByTestId('market-price-1w')).toHaveCount(0)
      await market.close()
    })

    await test.step('上架失败（500）：只提示一次，开关仍为下架', async () => {
      await failOnce(page, `/products/${original.id}/prices/1w`, 'req-1w')
      const before = (await toasts()).length
      await swEl.click()
      await toast(page, '服务器内部错误（请求号 req-1w）')
      await expect(sw).not.toBeChecked()
      await drainTasks(page)
      expect((await toasts()).slice(before)).toEqual(['服务器内部错误（请求号 req-1w）'])
      expect(puts).toHaveLength(4)
      expect(await durations()).toEqual(['1d', '1m'])
    })

    await test.step('再上架（5 分钟内免动态码）：市场恢复 1 周 ¥414.00', async () => {
      await swEl.click()
      await toast(page, '价格已保存')
      await expect(sw).toBeChecked()
      await expect(page.getByTestId('totp-dialog')).toBeHidden()
      await drainTasks(page)
      expect(puts.slice(4).map((p) => p.body)).toEqual([{ amount: orig1w.amount, enabled: true }])
      expect(await durations()).toEqual(['1d', '1w', '1m'])
      const market = await consoleMarket(browser)
      await expect(market.card.getByTestId('market-price-1w')).toHaveText('1 周 ¥414.00')
      await market.close()
    })
  } finally {
    await restorePrices(ctx, admin, original)
  }
})

test('A-CAT-03 镜像新增、编辑、上移 / 下移（按显示顺序交换排序值）、停用需确认，与控制台一致；非法地址只提示一次并保留输入', async ({ page }) => {
  const admin = await createAdmin('super')
  const ctx = await adminApi(admin)
  const base = `E2E 镜像 ${runStamp}`
  const nameA = `${base} A`
  const nameA2 = `${base} A v2`
  const nameB = `${base} B`
  const toasts = await trackToasts(page)
  const imageGets = recordRequests(page, 'GET', '/images')
  const posts = recordRequests(page, 'POST', '/images')
  const publicNames = async () => (await publicCatalog(0)).images.map((i) => i.name)
  const byName = async (name: string) => (await adminImages(ctx)).find((i) => i.name === name)!
  try {
    await loginAdmin(page, admin)

    await test.step('镜像列表首次加载失败：页内错误 + 重试，不自动重试', async () => {
      await failOnce(page, '/images', 'req-img-1')
      await page.getByTestId('menu-catalog').click()
      await expect(page.getByTestId('cat-error')).toContainText('服务器内部错误（请求号 req-img-1）')
      await expect(page.getByTestId('img-name')).toHaveCount(0)
      await drainTasks(page)
      expect(imageGets).toHaveLength(1)
      await page.getByTestId('cat-retry').click()
      await expect(page.getByTestId('cat-error')).toHaveCount(0)
      await expect(imageRow(page, 'PyTorch')).toHaveCount(1)
    })

    await test.step('新增：名称为空页内提示；非法地址只提示一次、弹窗与输入保留；连点保存只新增一个，排在最后', async () => {
      const lastSort = (await adminImages(ctx)).reduce((m, i) => Math.max(m, i.sort), 0)
      await page.getByTestId('images-create-btn').click()
      await page.getByTestId('img-form-ref').fill('-bad/ref:1')
      await page.getByTestId('img-form-submit').click()
      await expect(page.getByTestId('img-form-error')).toHaveText('请填写镜像名称')
      await drainTasks(page)
      expect(posts).toHaveLength(0)
      await page.getByTestId('img-form-name').fill(nameA)
      const before = (await toasts()).length
      await page.getByTestId('img-form-submit').click()
      await toast(page, '镜像地址必须以小写字母或数字开头')
      await expectStillOpen(page, 'img-dialog', '镜像地址必须以小写字母或数字开头')
      expect((await toasts()).slice(before)).toEqual(['镜像地址必须以小写字母或数字开头'])
      await expect(page.getByTestId('img-form-name')).toHaveValue(nameA)
      await page.getByTestId('img-form-ref').fill(`registry.spark.internal/e2e/${runStamp}-a:1`)
      await page.getByTestId('img-form-desc').fill('端到端测试镜像 A')
      await clickTwiceSync(page.getByTestId('img-form-submit'))
      await toast(page, '镜像已新增')
      await expect(page.getByTestId('img-dialog')).toBeHidden()
      await drainTasks(page)
      expect(posts.map((p) => p.body)).toEqual([
        { name: nameA, ref: '-bad/ref:1', description: '', enabled: true, sort: lastSort + 10 },
        { name: nameA, ref: `registry.spark.internal/e2e/${runStamp}-a:1`, description: '端到端测试镜像 A', enabled: true, sort: lastSort + 10 },
      ])
      await expect(imageRow(page, nameA).getByTestId('img-sort')).toHaveText(String(lastSort + 10))
      await expect(page.getByTestId('img-name').last()).toHaveText(nameA)
      expect((await publicNames()).at(-1)).toBe(nameA)

      await page.getByTestId('images-create-btn').click()
      await page.getByTestId('img-form-name').fill(nameB)
      await page.getByTestId('img-form-ref').fill(`registry.spark.internal/e2e/${runStamp}-b:1`)
      await page.getByTestId('img-form-submit').click()
      await toast(page, '镜像已新增')
      await expect(imageRow(page, nameB).getByTestId('img-sort')).toHaveText(String(lastSort + 20))
      expect((await publicNames()).slice(-2)).toEqual([nameA, nameB])
    })

    await test.step('编辑：改名与描述，连点只发一个 PUT；控制台只有新名字', async () => {
      const a = await byName(nameA)
      const puts = recordRequests(page, 'PUT', `/images/${a.id}`)
      await imageRow(page, nameA).getByTestId('img-edit').click()
      await expect(page.getByTestId('img-form-name')).toHaveValue(nameA)
      await page.getByTestId('img-form-name').fill(nameA2)
      await page.getByTestId('img-form-desc').fill('改过的描述')
      await clickTwiceSync(page.getByTestId('img-form-submit'))
      await toast(page, '镜像已更新')
      await expect(imageRow(page, nameA2).getByTestId('img-desc')).toHaveText('改过的描述')
      await expect(imageRow(page, nameA)).toHaveCount(0)
      await drainTasks(page)
      expect(puts.map((p) => p.body)).toEqual([{ name: nameA2, ref: a.ref, description: '改过的描述', enabled: true, sort: a.sort }])
      const names = await publicNames()
      expect(names).toContain(nameA2)
      expect(names).not.toContain(nameA)
    })

    await test.step('上移 / 下移：与上一行 / 下一行交换排序值（各一个 PUT，连点不重复）；后台与控制台顺序一致', async () => {
      const a = await byName(nameA2)
      const b = await byName(nameB)
      const putsA = recordRequests(page, 'PUT', `/images/${a.id}`)
      const putsB = recordRequests(page, 'PUT', `/images/${b.id}`)
      await expect(imageRow(page, nameB).getByTestId('img-down')).toBeDisabled() // 最后一行不能下移
      await clickTwiceSync(imageRow(page, nameB).getByTestId('img-up'))
      await toast(page, '顺序已调整')
      await expect(page.getByTestId('img-name').nth(-2)).toHaveText(nameB)
      await expect(page.getByTestId('img-name').last()).toHaveText(nameA2)
      await expect(imageRow(page, nameB).getByTestId('img-sort')).toHaveText(String(a.sort))
      await expect(imageRow(page, nameA2).getByTestId('img-sort')).toHaveText(String(b.sort))
      await drainTasks(page)
      expect(putsB.map((p) => p.body)).toEqual([{ name: nameB, ref: b.ref, description: '', enabled: true, sort: a.sort }])
      expect(putsA.map((p) => p.body)).toEqual([{ name: nameA2, ref: a.ref, description: '改过的描述', enabled: true, sort: b.sort }])
      expect((await publicNames()).slice(-2)).toEqual([nameB, nameA2])

      await clickTwiceSync(imageRow(page, nameB).getByTestId('img-down'))
      await toast(page, '顺序已调整')
      await expect(page.getByTestId('img-name').last()).toHaveText(nameB)
      await drainTasks(page)
      expect(putsB.slice(1).map((p) => p.body)).toEqual([{ name: nameB, ref: b.ref, description: '', enabled: true, sort: b.sort }])
      expect(putsA.slice(1).map((p) => p.body)).toEqual([{ name: nameA2, ref: a.ref, description: '改过的描述', enabled: true, sort: a.sort }])
      expect((await publicNames()).slice(-2)).toEqual([nameA2, nameB])
    })

    await test.step('停用需确认：取消不发请求；连点确定只发一个 PUT，控制台不再有该镜像；启用无需确认', async () => {
      const b = await byName(nameB)
      const puts = recordRequests(page, 'PUT', `/images/${b.id}`)
      const swEl = imageRow(page, nameB).getByTestId('img-enabled')
      const sw = swEl.getByRole('switch')
      await swEl.click()
      await expect(page.getByTestId('img-disable-text')).toHaveText(`停用后控制台市场不再提供「${nameB}」，已创建的实例不受影响。`)
      await page.getByTestId('img-disable-cancel').click()
      await expect(page.getByTestId('img-disable-dialog')).toBeHidden()
      await expect(sw).toBeChecked()
      await drainTasks(page)
      expect(puts).toHaveLength(0)

      await swEl.click()
      await clickTwiceSync(page.getByTestId('img-disable-ok'))
      await toast(page, '镜像已停用')
      await expect(page.getByTestId('img-disable-dialog')).toBeHidden()
      await expect(sw).not.toBeChecked()
      await drainTasks(page)
      expect(puts.map((p) => p.body)).toEqual([{ name: nameB, ref: b.ref, description: '', enabled: false, sort: b.sort }])
      expect(await publicNames()).not.toContain(nameB)

      await swEl.click()
      await toast(page, '镜像已启用')
      await expect(sw).toBeChecked()
      await expect(page.getByTestId('img-disable-dialog')).toBeHidden()
      expect(puts.slice(1).map((p) => p.body)).toEqual([{ name: nameB, ref: b.ref, description: '', enabled: true, sort: b.sort }])
      expect((await publicNames()).at(-1)).toBe(nameB)
    })
  } finally {
    // 没有删除接口：本用例造的镜像一律停用，控制台市场恢复为原来的镜像列表
    for (const i of await adminImages(ctx)) if (i.name.startsWith(base) && i.enabled) await adminPut(ctx, admin, `/admin/api/v1/images/${i.id}`, { name: i.name, ref: i.ref, description: i.description, enabled: false, sort: i.sort })
    expect((await publicCatalog(0)).images.filter((i) => i.name.startsWith(base))).toEqual([])
  }
})
