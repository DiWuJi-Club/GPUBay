import { expect, test, type Locator } from '@playwright/test'
import { isMobile, loginConsole, seedUser } from '../helpers/console'
import { runningInstance } from '../helpers/instances'

/** Element Plus 打开对话框时外层包装有 dialog-fade-in 动画（translateY -20px），几何断言前等页面上所有动画结束（被取消的动画也算结束）。 */
const settled = (l: Locator) => l.evaluate(() => Promise.allSettled(document.getAnimations().map((a) => a.finished)).then(() => undefined))

test('C-NAV-01 响应式布局：电脑侧边菜单、手机底部五个标签（含 Token 商城）', async ({ page }, info) => {
  const u = await seedUser()
  await loginConsole(page, u.phone)
  const tabs = ['market', 'instances', 'wallet', 'account', 'tokens']
  if (isMobile(info)) {
    await expect(page.getByTestId('layout-tabbar')).toBeVisible()
    for (const t of tabs) await expect(page.getByTestId(`tabbar-${t}`)).toBeVisible()
    await expect(page.getByTestId('layout-tabbar').locator('a')).toHaveCount(5)
    await expect(page.getByTestId('tabbar-tokens')).toContainText('Token 商城')
    await expect(page.getByTestId('layout-side')).toHaveCount(0)
    const box = await page.getByTestId('tabbar-wallet').boundingBox()
    expect(box?.height ?? 0).toBeGreaterThanOrEqual(44)
  } else {
    await expect(page.getByTestId('layout-side')).toBeVisible()
    for (const t of tabs) await expect(page.getByTestId(`menu-${t}`)).toBeVisible()
    await expect(page.getByTestId('menu-tokens')).toContainText('Token 商城')
    await expect(page.getByTestId('menu-tokens')).toHaveAttribute('href', '/tokens')
    await expect(page.getByTestId('layout-tabbar')).toHaveCount(0)
    await expect(page.getByTestId('layout-help')).toBeVisible()
  }
  await expect(page.getByTestId('layout-phone')).toHaveText(`${u.phone.slice(0, 3)}****${u.phone.slice(7)}`)
})

test('C-NAV-03 404 页并可返回市场', async ({ page }) => {
  await page.goto('/nope')
  await expect(page.getByTestId('notfound-page')).toBeVisible()
  await expect(page).toHaveTitle('页面不存在 - Spark Cloud')
  await page.getByTestId('notfound-home').click()
  await expect(page).toHaveURL(/\/market$/)
  await expect(page).toHaveTitle('算力市场 - Spark Cloud')
})

test('C-NAV-02 手机：确认框底部弹出不全屏、表单弹窗全屏；电脑：都居中不全屏', async ({ page }, info) => {
  const u = await seedUser()
  const { id } = await runningInstance(u.phone)
  await loginConsole(page, u.phone)
  await page.goto(`/instances/${id}`)
  await page.getByTestId('detail-release-btn').click()
  // Element Plus 的 role="dialog" 在 .el-overlay-dialog 包装层上；sheet / is-fullscreen 类与 data-testid 都在内层 .el-dialog 上，
  // 所以类名与位置断言必须用 AppDialog 的 testid 定位，用 getByRole('dialog') 断言类名会在错误的元素上「通过」
  const dlg = page.getByTestId('detail-release-dialog')
  await expect(dlg).toBeVisible()
  await expect(dlg).toHaveClass(/\bel-dialog\b/) // 确认定位到的确是 .el-dialog 本体
  await settled(dlg)
  if (isMobile(info)) {
    await expect(dlg).toHaveClass(/\bsheet\b/)
    await expect(dlg).not.toHaveClass(/is-fullscreen/)
    const box = await dlg.boundingBox()
    const viewport = page.viewportSize()!
    expect(Math.round(box!.y + box!.height)).toBeGreaterThanOrEqual(viewport.height - 1) // 贴底
    expect(Math.round(box!.width)).toBe(viewport.width) // 通栏
    expect(box!.height).toBeLessThan(viewport.height * 0.8) // 不是全屏
  } else {
    await expect(dlg).not.toHaveClass(/\bsheet\b/)
    await expect(dlg).not.toHaveClass(/is-fullscreen/)
    const box = await dlg.boundingBox()
    const viewport = page.viewportSize()!
    expect(Math.abs(box!.x + box!.width / 2 - viewport.width / 2)).toBeLessThan(4) // 水平居中
    expect(Math.abs(box!.y + box!.height / 2 - viewport.height / 2)).toBeLessThan(4) // 垂直居中（align-center）
    expect(Math.round(box!.width)).toBe(420)
  }
  await page.getByTestId('detail-release-cancel').click()
  await expect(dlg).toBeHidden()
  await page.getByTestId('detail-renew-btn').click()
  const form = page.getByTestId('detail-renew-dialog')
  await expect(form).toBeVisible()
  await settled(form)
  if (isMobile(info)) {
    await expect(form).toHaveClass(/is-fullscreen/)
    const box = await form.boundingBox()
    const viewport = page.viewportSize()!
    expect(Math.round(box!.width)).toBe(viewport.width)
    expect(Math.round(box!.height)).toBe(viewport.height)
  } else {
    await expect(form).not.toHaveClass(/is-fullscreen/)
    const box = await form.boundingBox()
    const viewport = page.viewportSize()!
    expect(Math.round(box!.width)).toBe(560)
    expect(Math.abs(box!.x + box!.width / 2 - viewport.width / 2)).toBeLessThan(4)
    expect(Math.abs(box!.y + box!.height / 2 - viewport.height / 2)).toBeLessThan(4)
    expect(box!.height).toBeLessThan(viewport.height) // 不是全屏
  }
})
