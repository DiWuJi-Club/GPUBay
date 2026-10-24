<script setup lang="ts">
import { fmtTime, useMobile, type Deployment } from '@spark/shared'
import { nextTick, ref, watch } from 'vue'
import { api } from '../api'
import { notifyError } from '../logic/notify'

/**
 * 部署日志抽屉：本次部署的日志（每次部署 log 轮换为 log.1，平台只读 log），≤ 64KB，已按 secret 脱敏。
 * 纯文本显示（插值，不用 v-html）；加载后自动滚到底。
 */
const props = defineProps<{ instanceId: string; deployment: Pick<Deployment, 'id' | 'app_name'> | null }>()
const visible = defineModel<boolean>({ required: true })
const mobile = useMobile()

const text = ref('')
const live = ref<boolean | null>(null)
const at = ref('')
const loading = ref(false)
const pre = ref<HTMLElement | null>(null)
let seq = 0

async function load() {
  const d = props.deployment
  if (!d) return
  const mine = ++seq
  loading.value = true
  try {
    const res = await api.get<{ log: string; live: boolean; at: string }>(`/instances/${props.instanceId}/apps/${d.id}/log`)
    if (mine !== seq) return // 换了部署 / 再次刷新：丢弃迟到的旧响应
    text.value = res.log
    live.value = res.live
    at.value = res.at
    await nextTick()
    if (pre.value) pre.value.scrollTop = pre.value.scrollHeight
  } catch (e) {
    if (mine === seq) notifyError(e)
  } finally {
    if (mine === seq) loading.value = false
  }
}

watch(
  () => [visible.value, props.deployment?.id] as const,
  ([v]) => {
    if (!v) return
    text.value = ''
    live.value = null
    at.value = ''
    void load()
  },
  { immediate: true },
)
</script>

<template>
  <el-drawer v-model="visible" :title="`${deployment?.app_name ?? ''} 日志`" :size="mobile ? '100%' : '60%'" append-to-body destroy-on-close data-testid="app-log-drawer">
    <div class="bar">
      <el-button size="small" :loading="loading" data-testid="app-log-refresh" @click="load">刷新</el-button>
      <el-tag v-if="live !== null" :type="live ? 'success' : 'info'" data-testid="app-log-live" :data-live="String(live)">{{ live ? '实时' : '数据库缓存' }}</el-tag>
      <span v-if="at" class="muted">{{ fmtTime(at) }}</span>
    </div>
    <p class="muted small">只显示本次部署的日志（最多 64KB，密钥已替换为 ***）；上一次部署的日志在容器里 <code>/root/data/.spark-apps/&lt;应用&gt;/log.1</code>。</p>
    <pre ref="pre" class="log" data-testid="app-log-text">{{ text || (loading ? '加载中…' : '（暂无日志）') }}</pre>
  </el-drawer>
</template>

<style scoped>
.bar { display: flex; align-items: center; gap: 8px; margin-bottom: 8px; flex-wrap: wrap; }
.muted { color: #909399; font-size: 12px; }
.small { margin: 4px 0 8px; }
.log {
  margin: 0; padding: 8px; background: #1e1e1e; color: #e5e5e5; border-radius: 4px;
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: 12px; line-height: 1.5;
  white-space: pre-wrap; word-break: break-all; overflow: auto; height: calc(100vh - 200px); min-height: 200px;
}
</style>
