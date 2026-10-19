import { defineConfig, devices } from '@playwright/test'

const channel = process.env.E2E_BROWSER_CHANNEL ?? 'chrome' // CI 可设为 chromium 使用 Playwright 自带浏览器
const browser = channel === 'chromium' ? {} : { channel }
// 控制台 project 专用：spark.test（控制台对外地址）与 ins-*-<port>.spark.test（实例 Web 入口）都解析到本机，
// 用例才能走真实的「实例域名 → authorize → 登录 → callback」跳转。Chrome 与 Playwright 自带 Chromium 都支持该参数。
const consoleLaunch = { launchOptions: { args: ['--host-resolver-rules=MAP *.spark.test 127.0.0.1, MAP spark.test 127.0.0.1'] } }

export const CONSOLE_URL = 'http://127.0.0.1:18080'
export const ADMIN_URL = 'http://127.0.0.1:18081'

export default defineConfig({
  testDir: 'e2e',
  testMatch: /e2e\/(console|admin)\/.*\.spec\.ts$/,
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
    // 默认是 SIGKILL 整个进程组：e2e-server 来不及执行 pg.Stop()，而 pg_ctl 拉起的 postgres 已脱离进程组，
    // 会变成孤儿进程长期占用 SysV 共享内存（macOS 只有 32 段），积累后新的嵌入式数据库无法启动
    gracefulShutdown: { signal: 'SIGTERM', timeout: 20_000 },
    stdout: 'pipe',
    stderr: 'pipe',
  },
  // project 顺序依赖：只在 console-desktop → console-mobile → admin 这个顺序下验证过（workers: 1 时按声明顺序执行），不要调换。
  // 整个运行共享一个嵌入式数据库和一个假时钟，以下用例会改全局状态且不恢复：A-INS-07 推进 301 秒、A-FIN-04 推进接近 24 小时、
  // A-AUTH / A-DASH 的会话过期用例推进 13 小时、A-ALT-01 让全部机器失联一轮；控制台也推进约 1 天：console/instances.spec.ts
  // 第 18、94 行（C-INS-01、C-INS-04），console/instance-detail.spec.ts 第 76、136、169 行（C-DET-04 / 07 / 09）与 C-DET-10
  // （第 274、308 行，各推进到实例只剩 20 秒，随后再推进 20 秒）；console/ingress.spec.ts 的 C-ING-04 推进 1 天；C-ING 用例在
  // spark.test:18080（CONSOLE_SITE_URL）登录，会话 Cookie 与其它用例在 127.0.0.1 的会话互不影响；e2e 机器的 lan_ip 都是 127.0.0.1
  // （所有实例共用 e2e-server 里同一个假容器 sshd）；C-MKT-06 / A-INS-04 的 drainIdleMachines 把当时所有空闲机器置为维护。admin 用例推进后都会重新登录（后台会话 2 小时空闲失效），控制台用例只用自己新登记的机器、
  // 按自己实例的剩余时间断言。新增依赖全局时间或空闲机器的用例、或调整 project 顺序时，要跑完整 make check 并同步更新
  // 这里与 docs/superpowers/followups-plan5.md。
  projects: [
    {
      name: 'console-desktop',
      testMatch: /e2e\/console\/.*\.spec\.ts$/,
      use: { ...devices['Desktop Chrome'], ...browser, ...consoleLaunch, baseURL: CONSOLE_URL, viewport: { width: 1280, height: 720 } },
    },
    {
      name: 'console-mobile',
      testMatch: /e2e\/console\/.*\.spec\.ts$/,
      use: { ...devices['iPhone 13'], defaultBrowserType: 'chromium', ...browser, ...consoleLaunch, baseURL: CONSOLE_URL },
    },
    {
      name: 'admin',
      testMatch: /e2e\/admin\/.*\.spec\.ts$/,
      use: { ...devices['Desktop Chrome'], ...browser, baseURL: ADMIN_URL, viewport: { width: 1440, height: 900 } },
    },
  ],
})
