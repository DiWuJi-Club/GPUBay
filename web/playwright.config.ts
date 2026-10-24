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
  // 计划 6 的预订用例（C-BKG / C-DET-17～25 / C-INS-05～07 / A-GRP / A-BKG / A-INS-13）按服务器时间（hooks.clock）算开始时刻，
  // 每个用例最多推进约 1 天（C-DET-22 / 23 推进 23 小时，C-BKG-08 推进 3 小时，到点开通的用例推进 2～3 小时；没有用例推进到 3 天以上），
  // 预订都下在自己新建的机组（helpers/booking.ts privateGroup）里的新机器上，不依赖共享机器池；C-BKG-11 与 C-MKT-06 一样用
  // drainIdleMachines 把当时所有空闲机器置为维护（构造旧接口 /orders 的 no_capacity）。
  // 计划 7 的应用部署用例（C-MKT-09～12 / C-APP / C-ING-07 / A-APP / A-INS-14）：e2e-server 不自己跑 Runner，用例用
  // hooks.appsTick 逐轮推进，每轮把假时钟推进 5 秒（一个部署从排队到运行中约 5～10 轮，即不到 1 分钟）；只有 C-APP-08 推进
  // 25 小时（让 1 天的实例到期停机，与 C-INS-01 同量级）。部署都在用例自己 runningInstance 新开的实例上（机器来自 hooks.machines），
  // 每个用例开始时 seedApps（幂等）重新把 3000 / 8000 / 8188 / 8888 端口转到 fake 应用服务（会覆盖 ingressUpstream 登记的同端口），
  // afterEach 的 cleanupApps 卸载本用例的全部部署并推进到 removed（假容器是 shell 模式，fake 服务是 setsid 出去的 sleep，
  // 不卸载就一直留着；实例已停机的先续费开机再卸载）。要「后台发布新版本」的用例（C-APP-04 / 09）用 helpers/apps.ts 的
  // privateApp 建自己的已上架应用，结束时改为已退役，不动共享的四个 fake 应用。A-APP-04 新建一个启用的镜像，并用
  // hooks.agent({images}) 把所有在线 fake Agent 的已缓存镜像整体设为它（覆盖率断言要求全部在线机器都上报过）；
  // A-MCH-10 只设自己那台机器。
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
