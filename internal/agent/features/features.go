// Package features 定义 Agent 在 Hello.features 里上报的能力位图，Agent 与网关共用。
package features

// SharedSlots（bit0）：Agent 支持 CreateInstance.share_slots，按份等分内存 / CPU / 数据盘配额，
// 可在一台机器上承载多个共享实例。共享机已搁置（计划 10 PoC no-go），平台目前不读取该位，见 docs/superpowers/followups-plan10.md。
const SharedSlots uint32 = 1 << 0
