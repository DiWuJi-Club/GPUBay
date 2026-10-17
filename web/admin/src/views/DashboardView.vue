<script setup lang="ts">
import { fmtYuan, instanceStateLabel, machineStateLabel, messageOf, type Dashboard } from '@spark/shared'
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { api } from '../api'
import { barsOf, CHART } from '../logic/chart'
import { notifyError } from '../logic/notify'

const data = ref<Dashboard | null>(null)
const loadError = ref('')
const retrying = ref(false)
const machineStates = ['idle', 'allocated', 'maintenance', 'faulty'] as const
const instanceStates = ['creating', 'running', 'stopped', 'wiping', 'released', 'failed'] as const

// 60 秒定时刷新。故障期间定时器照常每 60 秒取一次（恢复后自动显示），但同一次故障只提示一次，成功后复位；
// 首次就失败（还没有数据）显示占位与重试按钮；已有数据时刷新失败保留旧数据。
// 定时刷新与手动重试可能同时在途：只采用最后发出的那次，先发后到的旧结果（成功或失败）一律丢弃。
let failing = false
let seq = 0
async function load() {
  const mine = ++seq
  try {
    const out = await api.get<Dashboard>('/dashboard')
    if (mine !== seq) return
    data.value = out
    loadError.value = ''
    failing = false
  } catch (e) {
    if (mine !== seq) return
    if (!failing) notifyError(e)
    failing = true
    if (!data.value) loadError.value = messageOf(e)
  }
}
/** 手动重试是用户操作：失败要再提示一次 */
async function retry() {
  retrying.value = true
  failing = false
  await load()
  retrying.value = false
}
let timer: ReturnType<typeof setInterval> | undefined
onMounted(() => {
  void load()
  timer = setInterval(load, 60_000)
})
onUnmounted(() => clearInterval(timer))

// 近 30 天出租率柱状图：自绘 SVG，几何见 logic/chart.ts
const W = CHART.width
const H = CHART.height
const bars = computed(() => barsOf(data.value?.utilization ?? []))
</script>

<template>
  <div v-if="data" class="dash">
    <div class="grid">
      <el-card header="机器">
        <div class="big"><span data-testid="dash-machines-total">{{ data.machines.total }}</span> 台 · 在线 <span data-testid="dash-machines-online">{{ data.machines.online }}</span></div>
        <ul class="kv">
          <li v-for="s in machineStates" :key="s"><span>{{ machineStateLabel[s] }}</span><b :data-testid="`dash-machine-${s}`">{{ data.machines.by_state[s] ?? 0 }}</b></li>
        </ul>
      </el-card>
      <el-card header="实例">
        <ul class="kv">
          <li v-for="s in instanceStates" :key="s"><span>{{ instanceStateLabel[s] }}</span><b :data-testid="`dash-instance-${s}`">{{ data.instances[s] ?? 0 }}</b></li>
        </ul>
      </el-card>
      <el-card header="充值金额">
        <ul class="kv">
          <li><span>今日</span><b data-testid="dash-recharge-today">{{ fmtYuan(data.recharge_today) }}</b></li>
          <li><span>本月</span><b data-testid="dash-recharge-month">{{ fmtYuan(data.recharge_month) }}</b></li>
        </ul>
      </el-card>
      <el-card header="下单金额">
        <ul class="kv">
          <li><span>今日</span><b data-testid="dash-order-today">{{ fmtYuan(data.order_today) }}</b></li>
          <li><span>本月</span><b data-testid="dash-order-month">{{ fmtYuan(data.order_month) }}</b></li>
        </ul>
      </el-card>
    </div>
    <el-card header="近 30 天出租率（最后一根为今天，截至当前）">
      <el-empty v-if="bars.length === 0" description="暂无出租率数据" :image-size="60" data-testid="dash-chart-empty" />
      <svg v-else :viewBox="`0 0 ${W} ${H}`" class="chart" data-testid="dash-chart" role="img" aria-label="近 30 天出租率">
        <line x1="0" :y1="H - 0.5" :x2="W" :y2="H - 0.5" stroke="#dcdfe6" />
        <rect v-for="b in bars" :key="b.date" :x="b.x" :y="b.y" :width="CHART.barWidth" :height="b.h" fill="var(--el-color-primary)" data-testid="dash-bar" :data-date="b.date" :data-pct="b.pct">
          <title>{{ b.date }} 出租率 {{ b.pct }}%</title>
        </rect>
      </svg>
    </el-card>
  </div>
  <el-alert v-else-if="loadError" type="error" :title="loadError" show-icon :closable="false" data-testid="dash-error">
    <el-button size="small" :loading="retrying" data-testid="dash-retry" @click="retry">重试</el-button>
  </el-alert>
  <el-skeleton v-else :rows="6" animated />
</template>

<style scoped>
.dash { display: grid; gap: 16px; }
.grid { display: grid; grid-template-columns: repeat(4, 1fr); gap: 16px; }
.big { font-size: 20px; font-weight: 600; margin-bottom: 8px; }
.kv { list-style: none; margin: 0; padding: 0; display: grid; gap: 6px; }
.kv li { display: flex; justify-content: space-between; }
.chart { width: 100%; height: 200px; }
</style>
