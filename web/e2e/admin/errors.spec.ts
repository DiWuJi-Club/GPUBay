import { expect, test } from '@playwright/test'
import { adminApi, adminPut, createAdmin, drainTasks, expectAdminUrl, loginAdmin, recordRequests, trackToasts } from '../helpers/admin'
import { toast } from '../helpers/console'

test('A-ERR-01 会话中途被降权：接口 403 以提示显示而不是白屏', async ({ page }) => {
  // 真实路径：super 正在看机器页，另一位 super 把他降为 finance。后端每次请求都从 admins 表重读角色（降级不删会话），
  // 页面里缓存的角色仍是 super，所以下一次 GET /machines 得到真实的 403 forbidden。不用 page.route 篡改请求。
  const root = await createAdmin('super')
  const victim = await createAdmin('super')
  const toasts = await trackToasts(page)
  const lists = recordRequests(page, 'GET', '/machines')
  await loginAdmin(page, victim)
  await page.getByTestId('menu-machines').click()
  await expect(page.getByTestId('machines-total')).toHaveText(/共 \d+ 台/)
  await adminPut(await adminApi(root), root, `/admin/api/v1/admins/${victim.id}`, { role: 'finance' })
  const before = lists.length
  const forbidden = page.waitForResponse((r) => new URL(r.url()).pathname === '/admin/api/v1/machines' && r.status() === 403)
  await page.getByTestId('machines-search').click()
  expect(((await (await forbidden).json()) as { code: string }).code).toBe('forbidden')
  await toast(page, '无权执行该操作')
  await drainTasks(page)
  expect(lists).toHaveLength(before + 1) // 不自动重试
  expect(await toasts()).toEqual(['无权执行该操作']) // 只提示一次
  // 不白屏：筛选栏、布局、用户名都还在，没有被踢到登录页
  await expect(page.getByTestId('machines-search')).toBeVisible()
  await expect(page.getByTestId('layout-username')).toHaveText(victim.username)
  await expectAdminUrl(page, '/machines')
  // 刷新后前端拿到新角色：机器页变成 403 页，菜单里不再有「机器」
  await page.reload()
  await expect(page.getByTestId('forbidden-page')).toBeVisible()
  await page.getByTestId('forbidden-home').click()
  await expect(page.getByTestId('layout-role')).toHaveText('财务')
  await expect(page.getByTestId('menu-machines')).toHaveCount(0)
  await expect(page.getByTestId('menu-instances')).toBeVisible()
})
