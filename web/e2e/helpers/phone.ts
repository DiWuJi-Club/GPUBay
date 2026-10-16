import { randomInt } from 'node:crypto'

// 手机号 139 + 8 位：每个进程（worker 重启也是新进程）随机取一个 8 位起点，再逐个自增。
// 一次运行只造几百个号码，两段随机起点区间重叠的概率约为 (2·n)/10^8，可忽略；而仅用时间戳后几位在重启时可能撞上。
let next = randomInt(0, 100_000_000)

export function uniquePhone(): string {
  const n = next
  next = (next + 1) % 100_000_000
  return `139${String(n).padStart(8, '0')}`
}

/** 本进程的运行戳（8 位随机），用于用户名、序列号等需要跨 worker 重启唯一的标识。 */
export const runStamp = String(randomInt(0, 100_000_000)).padStart(8, '0')
