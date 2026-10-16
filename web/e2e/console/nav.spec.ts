import { expect, test } from '@playwright/test'
import { isMobile, loginConsole, seedUser } from '../helpers/console'

test('C-NAV-01 响应式布局：电脑侧边菜单、手机底部四个标签', async ({ page }, info) => {
  const u = await seedUser()
  await loginConsole(page, u.phone)
  const tabs = ['market', 'instances', 'wallet', 'account']
  if (isMobile(info)) {
    await expect(page.getByTestId('layout-tabbar')).toBeVisible()
    for (const t of tabs) await expect(page.getByTestId(`tabbar-${t}`)).toBeVisible()
    await expect(page.getByTestId('layout-side')).toHaveCount(0)
    const box = await page.getByTestId('tabbar-wallet').boundingBox()
    expect(box?.height ?? 0).toBeGreaterThanOrEqual(44)
  } else {
    await expect(page.getByTestId('layout-side')).toBeVisible()
    for (const t of tabs) await expect(page.getByTestId(`menu-${t}`)).toBeVisible()
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
