import { expect, test } from '@playwright/test'
import { loginConsole, nav, seedUser, toast, userApi } from '../helpers/console'
import { hooks } from '../helpers/hooks'
import { runningInstance } from '../helpers/instances'
import { freshPublicKey, XSS, xssFired } from '../helpers/sshkeys'

// 固定的测试密钥对，只在 e2e 里使用（入库无妨，不对应任何真实机器）：ssh-keygen -t ed25519 -C e2e@laptop -N ''
const PRIV = `-----BEGIN OPENSSH PRIVATE KEY-----
b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW
QyNTUxOQAAACDQicQhQGjRqhD7cPZ0dzND/L3btTYIhbqhEF9iePV8NgAAAJBFB16ORQde
jgAAAAtzc2gtZWQyNTUxOQAAACDQicQhQGjRqhD7cPZ0dzND/L3btTYIhbqhEF9iePV8Ng
AAAED841C4Q5mAYns7fwWBwPrPJy8BrLIWzu8BGe3O1nAlBNCJxCFAaNGqEPtw9nR3M0P8
vdu1NgiFuqEQX2J49Xw2AAAACmUyZUBsYXB0b3ABAgM=
-----END OPENSSH PRIVATE KEY-----
`
const PUB = 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINCJxCFAaNGqEPtw9nR3M0P8vdu1NgiFuqEQX2J49Xw2 e2e@laptop'

test('C-KEY-01 添加公钥：从「我的」进入，名称缺省取注释，列表显示指纹与数量', async ({ page }, info) => {
  const u = await seedUser()
  await loginConsole(page, u.phone)
  await nav(page, info, 'account')
  await page.getByTestId('account-ssh-keys').click()
  await expect(page).toHaveURL(/\/account\/ssh-keys$/)
  await expect(page.getByTestId('keys-empty')).toBeVisible()
  await expect(page.getByTestId('keys-count')).toHaveText('0 / 10')
  await page.getByTestId('keys-add-btn').click()
  await page.getByTestId('keys-input').fill(PUB + '\r\n') // 从 Windows 文件复制带 CRLF：接受
  await page.getByTestId('keys-submit').click()
  await toast(page, '已添加')
  await expect(page.getByTestId('keys-row')).toHaveCount(1)
  await expect(page.getByTestId('keys-name')).toHaveText('e2e@laptop')
  await expect(page.getByTestId('keys-fp')).toHaveText(/^SHA256:[A-Za-z0-9+/]{43}$/)
  await expect(page.getByTestId('keys-count')).toHaveText('1 / 10')
  await expect(page.getByTestId('keys-empty')).toHaveCount(0)
})

test('C-KEY-02 无效公钥：前端拦截私钥 / 多行，后端拒绝格式错误，都显示原因且不关闭弹窗', async ({ page }) => {
  const u = await seedUser()
  await loginConsole(page, u.phone)
  await page.goto('/account/ssh-keys')
  let posts = 0
  page.on('request', (r) => {
    if (r.method() === 'POST' && r.url().endsWith('/api/v1/ssh-keys')) posts += 1
  })
  await page.getByTestId('keys-add-btn').click()
  await page.getByTestId('keys-input').fill('-----BEGIN OPENSSH PRIVATE KEY-----')
  await page.getByTestId('keys-submit').click()
  await expect(page.getByTestId('keys-error')).toContainText('请粘贴一行公钥')
  await page.getByTestId('keys-input').fill(`${PUB}\n${freshPublicKey()}`)
  await page.getByTestId('keys-submit').click()
  await expect(page.getByTestId('keys-error')).toContainText('请粘贴一行公钥')
  expect(posts).toBe(0)
  await page.getByTestId('keys-input').fill('ssh-ed25519 AAAAnotvalidbase64')
  await page.getByTestId('keys-submit').click()
  await expect(page.getByTestId('keys-error')).toContainText('公钥格式不正确')
  expect(posts).toBe(1)
  await expect(page.getByTestId('keys-input')).toHaveValue('ssh-ed25519 AAAAnotvalidbase64')
  await page.getByTestId('keys-cancel').click()
  await expect(page.getByTestId('keys-empty')).toBeVisible()
})

test('C-KEY-03 重复公钥提示已添加；名称里的 HTML 按文本显示', async ({ page }) => {
  const u = await seedUser()
  await loginConsole(page, u.phone)
  await page.goto('/account/ssh-keys')
  await page.getByTestId('keys-add-btn').click()
  await page.getByTestId('keys-input').fill(PUB)
  await page.getByTestId('keys-name-input').fill(XSS)
  await page.getByTestId('keys-submit').click()
  await toast(page, '已添加')
  await expect(page.getByTestId('keys-name')).toHaveText(XSS)
  await expect(page.getByTestId('keys-row').locator('img')).toHaveCount(0)
  expect(await xssFired(page)).toBe(false)
  await page.getByTestId('keys-add-btn').click()
  await page.getByTestId('keys-input').fill(PUB)
  await page.getByTestId('keys-submit').click()
  await expect(page.getByTestId('keys-error')).toHaveText('该公钥已添加')
  await page.getByTestId('keys-cancel').click()
  await expect(page.getByTestId('keys-row')).toHaveCount(1)
})

test('C-KEY-04 公钥能登录实例；删除后不能', async ({ page }) => {
  const u = await seedUser()
  const { id } = await runningInstance(u.phone)
  expect((await hooks.ssh({ instance_id: id, key_pem: PRIV, command: 'echo hi' })).error).not.toBe('') // 添加之前不能
  await loginConsole(page, u.phone)
  await page.goto('/account/ssh-keys')
  await page.getByTestId('keys-add-btn').click()
  await page.getByTestId('keys-input').fill(PUB)
  await page.getByTestId('keys-submit').click()
  await expect(page.getByTestId('keys-row')).toHaveCount(1)
  const ok = await hooks.ssh({ instance_id: id, key_pem: PRIV, command: 'echo hi' })
  expect(ok.error).toBe('')
  expect(ok.output).toBe('hi\n')
  await page.getByTestId('keys-delete').click()
  await page.getByTestId('keys-delete-ok').click()
  await toast(page, '已删除')
  await expect(page.getByTestId('keys-empty')).toBeVisible()
  const denied = await hooks.ssh({ instance_id: id, key_pem: PRIV, command: 'echo hi' })
  expect(denied.error).not.toBe('')
  expect(denied.output).not.toContain('hi')
})

test('C-KEY-05 公钥上限 10 个：按钮禁用', async ({ page }) => {
  const u = await seedUser()
  const api = await userApi(u.phone)
  for (let i = 0; i < 10; i++) {
    const r = await api.post('/api/v1/ssh-keys', { data: { public_key: freshPublicKey(), name: `k${i}` } })
    expect(r.status(), await r.text()).toBe(201)
  }
  const over = await api.post('/api/v1/ssh-keys', { data: { public_key: freshPublicKey() } })
  expect(over.status()).toBe(409)
  expect(((await over.json()) as { code: string }).code).toBe('ssh_key_limit')
  await loginConsole(page, u.phone)
  await page.goto('/account/ssh-keys')
  await expect(page.getByTestId('keys-row')).toHaveCount(10)
  await expect(page.getByTestId('keys-count')).toHaveText('10 / 10')
  await expect(page.getByTestId('keys-add-btn')).toBeDisabled()
})
