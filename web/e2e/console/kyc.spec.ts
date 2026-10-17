import { expect, test, type Page, type Request } from '@playwright/test'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { adminApi, adminPost, createAdmin } from '../helpers/admin'
import { loginConsole, seedUser, settle, toast, toastLocator } from '../helpers/console'
import { hooks } from '../helpers/hooks'

const fixture = (name: string) => fileURLToPath(new URL(`../fixtures/${name}`, import.meta.url))
const LICENSE_NO = '91110000MA01ABCD2X'

const fail500 = (requestId: string) => ({
  status: 500,
  contentType: 'application/json',
  body: JSON.stringify({ code: 'internal', message: '服务器内部错误，请稍后重试', request_id: requestId }),
})

/** 记录满足条件的请求（方法 + 路径），用于「只发一次、不循环」断言。 */
function track(page: Page, method: string, pathname: string): Request[] {
  const out: Request[] = []
  page.on('request', (r) => {
    if (r.method() === method && new URL(r.url()).pathname === pathname) out.push(r)
  })
  return out
}

async function submitEnterprise(page: Page, file = 'license.png') {
  await page.getByRole('tab', { name: '企业' }).click()
  await page.getByTestId('kyc-company').fill('测试科技有限公司')
  await page.getByTestId('kyc-license-no').fill(LICENSE_NO)
  await page.getByTestId('kyc-upload').locator('input[type=file]').setInputFiles(fixture(file))
  await expect(page.getByTestId('kyc-file-name')).toHaveText(file)
  await page.getByTestId('kyc-enterprise-submit').click()
  await expect(page.getByTestId('kyc-status')).toHaveText('审核中，通常 1 个工作日内完成')
}

async function reviewLatest(userId: number, approve: boolean, reason = '') {
  const admin = await createAdmin('ops')
  const ctx = await adminApi(admin)
  const pending = ((await (await ctx.get('/admin/api/v1/kyc/pending?page_size=100')).json()) as { items: { id: number; user_id: number }[] }).items
  const rec = pending.find((r) => r.user_id === userId)
  if (!rec) throw new Error('pending kyc record not found')
  await adminPost(ctx, admin, `/admin/api/v1/kyc/${rec.id}/review`, { approve, reason })
}

test('C-KYC-01 个人实名成功并按 redirect 回到市场', async ({ page }) => {
  const ID_NO = '11010119900101123X'
  const u = await seedUser({ verified: false })
  const urls: string[] = []
  page.on('request', (r) => urls.push(r.url()))
  const posts = track(page, 'POST', '/api/v1/kyc/personal')
  await loginConsole(page, u.phone)
  await page.goto('/kyc?redirect=/market')
  await expect(page.getByTestId('kyc-status')).toHaveText('未认证')
  await expect(page.getByTestId('kyc-idno')).toHaveAttribute('autocomplete', 'off')
  await page.getByTestId('kyc-name').fill('张三')
  await page.getByTestId('kyc-idno').fill(ID_NO.toLowerCase()) // 小写 x 按大写提交
  await page.getByTestId('kyc-personal-submit').click()
  await toast(page, '实名认证成功')
  await expect(page).toHaveURL((url) => url.pathname === '/market' && url.search === '') // 不能只匹配 ?redirect=/market 结尾

  await test.step('身份证号不落本地存储、不进 URL，只在 POST 请求体里', async () => {
    expect(posts).toHaveLength(1)
    expect(posts[0].postDataJSON()).toEqual({ name: '张三', id_number: ID_NO })
    const stored = await page.evaluate(() => {
      const dump = (s: Storage) => Array.from({ length: s.length }, (_, i) => `${s.key(i)}=${s.getItem(s.key(i)!)}`).join('\n')
      return `${dump(localStorage)}\n${dump(sessionStorage)}`
    })
    expect(stored.toUpperCase()).not.toContain(ID_NO)
    expect(urls.some((x) => decodeURIComponent(x).toUpperCase().includes(ID_NO))).toBe(false)
    expect(page.url().toUpperCase()).not.toContain(ID_NO)
  })

  await page.goto('/kyc')
  await expect(page.getByTestId('kyc-status')).toHaveText('已认证')
  await expect(page.getByTestId('kyc-verified-name')).toContainText('个人：张*')
  await expect(page.getByTestId('kyc-name')).toHaveCount(0)

  await test.step('没有 redirect：认证成功后原地刷新为已认证并显示姓名，不重新加载页面', async () => {
    await page.context().clearCookies()
    const u2 = await seedUser({ verified: false })
    await loginConsole(page, u2.phone)
    await page.goto('/kyc')
    await expect(page.getByTestId('kyc-status')).toHaveText('未认证')
    await page.evaluate(() => ((window as unknown as { __noReload: boolean }).__noReload = true))
    await page.getByTestId('kyc-name').fill('赵六')
    await page.getByTestId('kyc-idno').fill('110101199001011234')
    await page.getByTestId('kyc-personal-submit').click()
    await toast(page, '实名认证成功')
    await expect(page.getByTestId('kyc-status')).toHaveText('已认证')
    await expect(page.getByTestId('kyc-verified-name')).toContainText('个人：赵*')
    await expect(page.getByTestId('kyc-name')).toHaveCount(0)
    expect(new URL(page.url()).pathname).toBe('/kyc')
    expect(await page.evaluate(() => (window as unknown as { __noReload?: boolean }).__noReload)).toBe(true) // 同一个文档，没有 reload / goto
  })
})

test('C-KYC-02 个人实名失败：红框原因与未通过状态', async ({ page }) => {
  const u = await seedUser({ verified: false })
  const posts = track(page, 'POST', '/api/v1/kyc/personal')
  await loginConsole(page, u.phone)
  await page.goto('/kyc')
  await page.getByTestId('kyc-name').fill('李四')
  await page.getByTestId('kyc-idno').fill('110101199001011234')

  await test.step('提交请求失败（500）：只提示一次、不重试，按钮恢复可再提交，双击只发一次', async () => {
    let release!: () => void
    const gate = new Promise<void>((r) => (release = r))
    await page.route('**/api/v1/kyc/personal', async (route) => {
      await gate
      await route.fulfill(fail500('req-kyc-p-500'))
    })
    await page.getByTestId('kyc-personal-submit').dblclick()
    await settle(page)
    expect(posts).toHaveLength(1) // 请求进行中第二次点击不发请求
    // 同一事件循环内连续两次提交（按钮来不及进入 loading）：靠同步置位的 submitting 拦住第二次
    await page.getByTestId('kyc-idno').evaluate((el) => {
      const form = el.closest('form')!
      form.requestSubmit()
      form.requestSubmit()
    })
    await settle(page)
    expect(posts).toHaveLength(1)
    release()
    await toast(page, '服务器内部错误，请稍后重试（请求号 req-kyc-p-500）')
    await settle(page, 30)
    expect(posts).toHaveLength(1)
    await expect(toastLocator(page, '请求号 req-kyc-p-500')).toHaveCount(1)
    await expect(page.getByTestId('kyc-rejected-box')).toHaveCount(0)
    await expect(page.getByTestId('kyc-status')).toHaveText('未认证')
    await expect(page.getByTestId('kyc-personal-submit')).toBeEnabled()
    await page.unroute('**/api/v1/kyc/personal')
  })

  await hooks.kyc(true)
  try {
    await page.getByTestId('kyc-personal-submit').click()
    await expect(page.getByTestId('kyc-rejected-box')).toContainText('实名核验未通过：')
    await expect(page.getByTestId('kyc-status')).toHaveText('未通过')
    await expect(page.getByTestId('kyc-reason')).toContainText('姓名、身份证号与手机号不一致')
    await expect(page.getByTestId('kyc-personal-submit')).toBeVisible() // 可重新提交
    expect(posts).toHaveLength(2)
  } finally {
    await hooks.kyc(false)
  }
})

test('C-KYC-03 字段校验：身份证 17 位、信用代码格式、未上传不能提交', async ({ page }) => {
  const u = await seedUser({ verified: false })
  let posts = 0
  page.on('request', (r) => {
    if (r.method() === 'POST' && r.url().includes('/api/v1/kyc/')) posts += 1
  })
  await loginConsole(page, u.phone)
  await page.goto('/kyc')
  await page.getByTestId('kyc-personal-submit').click()
  await expect(page.getByTestId('kyc-name-item').getByTestId('kyc-name-error')).toHaveText('请填写真实姓名')
  await page.getByTestId('kyc-name').fill('王五')
  await page.getByTestId('kyc-idno').fill('11010119900101123')
  await page.getByTestId('kyc-personal-submit').click()
  await expect(page.getByTestId('kyc-idno-item').getByTestId('kyc-idno-error')).toHaveText('身份证号格式不正确')
  await expect(page.getByTestId('kyc-name-error')).toHaveCount(0)
  await page.getByRole('tab', { name: '企业' }).click()
  await expect(page.getByTestId('kyc-enterprise-submit')).toBeDisabled()
  await page.getByTestId('kyc-license-no').fill('123')
  await page.getByTestId('kyc-upload').locator('input[type=file]').setInputFiles(fixture('license.png'))
  await expect(page.getByTestId('kyc-enterprise-submit')).toBeEnabled()
  await page.getByTestId('kyc-enterprise-submit').click()
  await expect(page.getByTestId('kyc-company-item').getByTestId('kyc-company-error')).toHaveText('请填写企业名称')
  await expect(page.getByTestId('kyc-license-item').getByTestId('kyc-license-error')).toHaveText('统一社会信用代码格式不正确')
  await page.getByTestId('kyc-company').fill('测试科技有限公司')
  await page.getByTestId('kyc-enterprise-submit').click()
  await expect(page.getByTestId('kyc-company-error')).toHaveCount(0)
  await expect(page.getByTestId('kyc-license-error')).toHaveText('统一社会信用代码格式不正确')
  await settle(page)
  expect(posts).toBe(0)
})

test('C-KYC-04 企业提交显示缩略图与审核中，后台通过后已认证', async ({ page }) => {
  const u = await seedUser({ verified: false })
  const kycGets = track(page, 'GET', '/api/v1/kyc')
  const enterprisePosts = track(page, 'POST', '/api/v1/kyc/enterprise')
  await loginConsole(page, u.phone)

  await test.step('首次加载 GET /kyc 失败：错误占位与重试，只请求一次、只提示一次', async () => {
    await page.route('**/api/v1/kyc', (route) => route.fulfill(fail500('req-kyc-get-500')))
    await page.goto('/kyc')
    await expect(page.getByTestId('kyc-load-error')).toContainText('请求号 req-kyc-get-500')
    await toast(page, '服务器内部错误，请稍后重试（请求号 req-kyc-get-500）')
    await expect(page.getByTestId('kyc-name')).toHaveCount(0) // 加载失败不显示表单
    await settle(page, 30)
    expect(kycGets).toHaveLength(1)
    await expect(toastLocator(page, '请求号 req-kyc-get-500')).toHaveCount(1)
    await page.unroute('**/api/v1/kyc')
    await page.getByTestId('kyc-retry').click()
    await expect(page.getByTestId('kyc-name')).toBeVisible()
    await expect(page.getByTestId('kyc-load-error')).toHaveCount(0)
    expect(kycGets).toHaveLength(2)
  })

  await page.getByRole('tab', { name: '企业' }).click()
  await page.getByTestId('kyc-upload').locator('input[type=file]').setInputFiles(fixture('license.png'))
  await expect(page.getByTestId('kyc-file-preview')).toBeVisible()
  await expect(page.getByTestId('kyc-file-preview')).toHaveAttribute('src', /^blob:/)
  await expect(page.getByTestId('kyc-file-pdf')).toHaveCount(0)
  await page.getByTestId('kyc-company').fill('测试科技有限公司')
  await page.getByTestId('kyc-license-no').fill(LICENSE_NO.toLowerCase())

  await test.step('企业提交失败（500）：只提示一次，表单保留可再次提交', async () => {
    await page.route('**/api/v1/kyc/enterprise', (route) => route.fulfill(fail500('req-kyc-e-500')))
    // 同一事件循环内连续两次提交：只发一个请求
    await page.getByTestId('kyc-company').evaluate((el) => {
      const form = el.closest('form')!
      form.requestSubmit()
      form.requestSubmit()
    })
    await toast(page, '服务器内部错误，请稍后重试（请求号 req-kyc-e-500）')
    await settle(page, 30)
    expect(enterprisePosts).toHaveLength(1)
    await expect(toastLocator(page, '请求号 req-kyc-e-500')).toHaveCount(1)
    await expect(page.getByTestId('kyc-status')).toHaveText('未认证')
    await expect(page.getByTestId('kyc-enterprise-submit')).toBeEnabled()
    await page.unroute('**/api/v1/kyc/enterprise')
  })

  await page.getByTestId('kyc-enterprise-submit').click()
  await toast(page, '已提交，等待审核')
  await expect(page.getByTestId('kyc-status')).toHaveText('审核中，通常 1 个工作日内完成')
  await expect(page.getByTestId('kyc-company')).toHaveCount(0)
  expect(enterprisePosts).toHaveLength(2)
  const body = enterprisePosts[1].postDataJSON() as { company_name: string; license_no: string; license_file: string }
  expect(body.company_name).toBe('测试科技有限公司')
  expect(body.license_no).toBe(LICENSE_NO) // 小写按大写提交
  expect(body.license_file).not.toBe('')
  await reviewLatest(u.id, true)
  await page.reload()
  await expect(page.getByTestId('kyc-status')).toHaveText('已认证')
  await expect(page.getByTestId('kyc-verified-name')).toContainText('企业：测试科技有限公司')
})

test('C-KYC-05 企业驳回后显示原因并可重新提交', async ({ page }) => {
  const u = await seedUser({ verified: false })
  await loginConsole(page, u.phone)
  await page.goto('/kyc')
  await submitEnterprise(page)
  await reviewLatest(u.id, false, '执照不清晰')
  await page.reload()
  await expect(page.getByTestId('kyc-status')).toHaveText('未通过')
  await expect(page.getByTestId('kyc-reason')).toContainText('执照不清晰')
  await test.step('重新提交后回到审核中', async () => {
    await submitEnterprise(page, 'license.pdf')
    await expect(page.getByTestId('kyc-reason')).toHaveCount(0)
  })
})

test('C-KYC-06 上传限制：GIF 被拒绝，PDF 显示图标', async ({ page }) => {
  const u = await seedUser({ verified: false })
  const uploads = track(page, 'POST', '/api/v1/uploads')
  await loginConsole(page, u.phone)
  await page.goto('/kyc')
  await page.getByRole('tab', { name: '企业' }).click()
  const input = page.getByTestId('kyc-upload').locator('input[type=file]')
  await input.setInputFiles(fixture('bad.gif'))
  await expect(page.getByTestId('kyc-upload-error')).toHaveText('仅支持 JPG、PNG、PDF 文件')
  await expect(page.getByTestId('kyc-enterprise-submit')).toBeDisabled()
  await settle(page)
  expect(uploads).toHaveLength(0) // 本地预检拦下，不发请求
  await input.setInputFiles(fixture('license.pdf'))
  await expect(page.getByTestId('kyc-file-pdf')).toBeVisible()
  await expect(page.getByTestId('kyc-file-preview')).toHaveCount(0)
  await expect(page.getByTestId('kyc-file-name')).toHaveText('license.pdf')
  await expect(page.getByTestId('kyc-upload-error')).toHaveCount(0)
  await expect(page.getByTestId('kyc-enterprise-submit')).toBeEnabled()
  expect(uploads).toHaveLength(1)

  await test.step('本地预检：超过 10MB、.txt 文件都不发请求，并清掉已选文件', async () => {
    uploads.length = 0
    await input.setInputFiles({ name: 'big.png', mimeType: 'image/png', buffer: Buffer.alloc(11 * 1024 * 1024, 1) })
    await expect(page.getByTestId('kyc-upload-error')).toHaveText('文件不能超过 10MB')
    await expect(page.getByTestId('kyc-file-name')).toHaveCount(0)
    await expect(page.getByTestId('kyc-enterprise-submit')).toBeDisabled()
    await input.setInputFiles({ name: 'notes.txt', mimeType: 'text/plain', buffer: Buffer.from('hello') })
    await expect(page.getByTestId('kyc-upload-error')).toHaveText('仅支持 JPG、PNG、PDF 文件')
    await settle(page)
    expect(uploads).toHaveLength(0)
  })

  await test.step('服务端拒绝：伪装成 PNG 的 GIF 返回 415、413 file_too_large 原文显示，只提示一次', async () => {
    uploads.length = 0
    await input.setInputFiles({ name: 'fake.png', mimeType: 'image/png', buffer: readFileSync(fixture('bad.gif')) })
    await expect(page.getByTestId('kyc-upload-error')).toHaveText('仅支持 JPG、PNG、PDF 文件')
    await toast(page, '仅支持 JPG、PNG、PDF 文件')
    expect(uploads).toHaveLength(1)
    await page.route('**/api/v1/uploads', (route) =>
      route.fulfill({ status: 413, contentType: 'application/json', body: JSON.stringify({ code: 'file_too_large', message: '文件不能超过 10MB', request_id: 'req-413' }) }),
    )
    await input.setInputFiles(fixture('license.png'))
    await expect(page.getByTestId('kyc-upload-error')).toHaveText('文件不能超过 10MB')
    await toast(page, '文件不能超过 10MB')
    await settle(page, 30)
    expect(uploads).toHaveLength(2)
    await expect(toastLocator(page, '文件不能超过 10MB')).toHaveCount(1)
    await expect(page.getByTestId('kyc-file-name')).toHaveCount(0)
    await expect(page.getByTestId('kyc-enterprise-submit')).toBeDisabled()
    await page.unroute('**/api/v1/uploads')
  })

  await test.step('上传请求失败（500）：清掉已选文件、只提示一次、不重试；重新选择即可恢复', async () => {
    await input.setInputFiles(fixture('license.pdf'))
    await expect(page.getByTestId('kyc-file-name')).toHaveText('license.pdf')
    uploads.length = 0
    await page.route('**/api/v1/uploads', (route) => route.fulfill(fail500('req-upload-500')))
    await input.setInputFiles(fixture('license.png'))
    await expect(page.getByTestId('kyc-upload-error')).toHaveText('服务器内部错误，请稍后重试（请求号 req-upload-500）')
    await toast(page, '服务器内部错误，请稍后重试（请求号 req-upload-500）')
    await expect(page.getByTestId('kyc-file-name')).toHaveCount(0)
    await expect(page.getByTestId('kyc-enterprise-submit')).toBeDisabled()
    await settle(page, 30)
    expect(uploads).toHaveLength(1)
    await expect(toastLocator(page, '请求号 req-upload-500')).toHaveCount(1)
    await page.unroute('**/api/v1/uploads')
    await input.setInputFiles(fixture('license.png'))
    await expect(page.getByTestId('kyc-file-preview')).toBeVisible()
    await expect(page.getByTestId('kyc-upload-error')).toHaveCount(0)
    await expect(page.getByTestId('kyc-enterprise-submit')).toBeEnabled()
    expect(uploads).toHaveLength(2)
  })

  await test.step('连续选择文件：只有最后一次选择生效，旧上传的结果被丢弃，提交用最新文件的 key', async () => {
    const held = new Set([0, 2, 3]) // 按请求序号挂起，直到测试放行
    const gates: Record<number, () => void> = {}
    const reqs: Request[] = []
    let n = 0
    // 挂起的请求在放行后才真正发出（route.fetch 会破坏 multipart 请求体，所以用 continue）
    await page.route('**/api/v1/uploads', async (route) => {
      const i = n++
      reqs[i] = route.request()
      if (held.has(i)) await new Promise<void>((r) => (gates[i] = r))
      await route.continue()
    })
    const pickButton = page.getByTestId('kyc-upload').locator('button') // 上传中为 loading（禁用）
    const gate = async (i: number) => {
      await expect.poll(() => typeof gates[i]).toBe('function')
      gates[i]()
    }

    // 场景 A：旧上传（0 号 PNG）比新上传（1 号 PDF）晚返回
    await input.setInputFiles(fixture('license.png'))
    await expect.poll(() => typeof gates[0]).toBe('function')
    await expect(page.getByTestId('kyc-enterprise-submit')).toBeDisabled() // 上传中
    await input.setInputFiles(fixture('license.pdf'))
    await expect(page.getByTestId('kyc-file-name')).toHaveText('license.pdf')
    await expect(page.getByTestId('kyc-enterprise-submit')).toBeEnabled()
    await gate(0)
    await reqs[0].response() // 等旧请求真正返回
    await settle(page, 30)
    await expect(page.getByTestId('kyc-file-name')).toHaveText('license.pdf') // 迟到的旧结果没有覆盖
    await expect(page.getByTestId('kyc-file-pdf')).toBeVisible()
    await expect(page.getByTestId('kyc-file-preview')).toHaveCount(0)

    // 场景 B：旧上传（2 号 PNG）先返回，新上传（3 号 second.pdf）仍在进行
    await input.setInputFiles(fixture('license.png'))
    await expect(page.getByTestId('kyc-file-name')).toHaveCount(0) // 换文件时旧文件立即作废
    await input.setInputFiles({ name: 'second.pdf', mimeType: 'application/pdf', buffer: readFileSync(fixture('license.pdf')) })
    await gate(2)
    await reqs[2].response()
    await settle(page, 30)
    await expect(page.getByTestId('kyc-file-name')).toHaveCount(0) // 不显示被取代的文件
    await expect(page.getByTestId('kyc-enterprise-submit')).toBeDisabled()
    await expect(pickButton).toBeDisabled() // 旧结果不能提前结束 uploading
    await gate(3)
    await expect(page.getByTestId('kyc-file-name')).toHaveText('second.pdf')
    await expect(pickButton).toBeEnabled()
    await expect(page.getByTestId('kyc-enterprise-submit')).toBeEnabled()
    await page.unroute('**/api/v1/uploads')

    const posts = track(page, 'POST', '/api/v1/kyc/enterprise')
    await page.getByTestId('kyc-company').fill('测试科技有限公司')
    await page.getByTestId('kyc-license-no').fill(LICENSE_NO)
    await page.getByTestId('kyc-enterprise-submit').click()
    await expect(page.getByTestId('kyc-status')).toHaveText('审核中，通常 1 个工作日内完成')
    expect(posts).toHaveLength(1)
    const latestKey = ((await (await reqs[3].response())!.json()) as { key: string }).key
    const staleKey = ((await (await reqs[2].response())!.json()) as { key: string }).key
    expect(latestKey).not.toBe(staleKey)
    expect((posts[0].postDataJSON() as { license_file: string }).license_file).toBe(latestKey)
  })
})
