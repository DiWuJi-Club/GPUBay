import { defineConfig, devices } from '@playwright/test'

const channel = process.env.E2E_BROWSER_CHANNEL ?? 'chrome' // CI 可设为 chromium 使用 Playwright 自带浏览器
const browser = channel === 'chromium' ? {} : { channel }

export const CONSOLE_URL = 'http://127.0.0.1:18080'
export const ADMIN_URL = 'http://127.0.0.1:18081'

export default defineConfig({
  testDir: 'e2e',
  testMatch: /(console|admin)\/.*\.spec\.ts$/,
  outputDir: 'test-results',
  // 假时钟全局共享：串行执行，每个测试自行登录、自行造数据
  workers: 1,
  fullyParallel: false,
  retries: 0,
  forbidOnly: true,
  timeout: 90_000,
  expect: { timeout: 10_000 },
  reporter: [['list'], ['html', { open: 'never', outputFolder: 'playwright-report' }]],
  use: {
    headless: true,
    locale: 'zh-CN',
    timezoneId: 'Asia/Shanghai',
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
    permissions: ['clipboard-read', 'clipboard-write'],
    ...browser,
  },
  webServer: {
    command: 'go run ./cmd/e2e-server',
    cwd: '..',
    url: 'http://127.0.0.1:18090/healthz',
    timeout: 180_000,
    // 永远由 Playwright 自己拉起：e2e-server 里 embed 的是启动那一刻的前端产物，复用手工起的旧进程会悄悄测到上一次的页面；
    // 端口被占用时直接报错，比「看似通过」好
    reuseExistingServer: false,
    stdout: 'pipe',
    stderr: 'pipe',
  },
  projects: [
    {
      name: 'console-desktop',
      testMatch: /console\/.*\.spec\.ts$/,
      use: { ...devices['Desktop Chrome'], ...browser, baseURL: CONSOLE_URL, viewport: { width: 1280, height: 720 } },
    },
    {
      name: 'console-mobile',
      testMatch: /console\/.*\.spec\.ts$/,
      use: { ...devices['iPhone 13'], defaultBrowserType: 'chromium', ...browser, baseURL: CONSOLE_URL },
    },
    {
      name: 'admin',
      testMatch: /admin\/.*\.spec\.ts$/,
      use: { ...devices['Desktop Chrome'], ...browser, baseURL: ADMIN_URL, viewport: { width: 1440, height: 900 } },
    },
  ],
})
