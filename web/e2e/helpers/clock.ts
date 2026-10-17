import type { Page } from '@playwright/test'

/** 控制台与后台共用。冻结页面时间（page.clock.install 之后时间仍按真实流逝）：此后只有 runFor 会触发页面定时器。 */
export async function pauseClock(page: Page) {
  // 读时间与 pauseAt 之间页面时间仍在走：往返超过 1ms 时 pauseAt 会报「Cannot fast-forward to the past」，重读后再试（每次多留几毫秒余量）
  for (let attempt = 0; ; attempt++) {
    const now = await page.evaluate(() => Date.now())
    try {
      await page.clock.pauseAt(now + 1 + attempt * 5)
      return
    } catch (e) {
      if (attempt >= 4 || !String(e).includes('fast-forward to the past')) throw e
    }
  }
}
