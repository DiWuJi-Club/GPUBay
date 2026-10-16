// 139 + 启动时间戳后 4 位 + 4 位自增：同一轮测试内唯一，worker 重启后戳变化也不会与之前的号码重复
const stamp = String(Date.now()).slice(-4)
let seq = 0

export function uniquePhone(): string {
  seq += 1
  return `139${stamp}${String(seq).padStart(4, '0')}`
}

export const runStamp = stamp
