<script setup lang="ts">
import {
  deploymentErrorText, deploymentStateLabel, deploymentStateType, fmtTime, fmtTimeSec, labelOf, llmNodeStateLabel, llmNodeStateType, messageOf,
  type AdminDeploymentDetail, type LLMNodeDetail,
} from '@spark/shared'
import { computed, reactive, watch } from 'vue'
import { api } from '../../api'
import { deploymentHealthLabel, progressText } from '../../logic/deployments'
import { notifyError } from '../../logic/notify'
import IdLink from '../IdLink.vue'

// 节点详情抽屉（规格 §11）：节点信息 + 事件时间线（GET /llm/nodes/{id}，最近 50 条）+ 部署状态与日志尾部
// （复用计划 7 的后台部署详情接口 GET /deployments/{dep}；部署事件优先用节点详情里带的，没有时用部署详情的）。
const props = defineProps<{ nodeId: string | null }>()
const emit = defineEmits<{ close: [] }>()

const st = reactive({
  loading: false,
  error: '',
  data: null as LLMNodeDetail | null,
  dep: null as AdminDeploymentDetail | null,
  depError: '',
})
const node = computed(() => st.data?.node ?? null)
const dep = computed(() => st.dep?.deployment ?? null)
const depEvents = computed(() => (st.data?.deployment_events?.length ? st.data.deployment_events : (st.dep?.events ?? [])))

let seq = 0
/**
 * 只采用最后一次请求的结果：快速切换节点时，先发后到的旧详情被丢弃；刷新失败只提示、保留已显示的数据。
 * quiet（节点 Tab 的自动刷新）时失败不弹提示。
 */
async function load(quiet = false) {
  const id = props.nodeId
  if (!id) return
  const mine = ++seq
  st.loading = true
  try {
    const d = await api.get<LLMNodeDetail>(`/llm/nodes/${encodeURIComponent(id)}`)
    if (mine !== seq) return
    st.data = d
    st.error = ''
    const depId = d.node.deployment_id
    if (!depId) {
      st.dep = null
      st.depError = ''
      return
    }
    try {
      const dd = await api.get<AdminDeploymentDetail>(`/deployments/${encodeURIComponent(depId)}`)
      if (mine !== seq) return
      st.dep = dd
      st.depError = ''
    } catch (e) {
      if (mine !== seq) return
      st.depError = messageOf(e) // 部署详情失败不影响节点信息，就地显示
    }
  } catch (e) {
    if (mine !== seq) return
    if (!quiet) notifyError(e)
    if (!st.data) st.error = messageOf(e)
  } finally {
    if (mine === seq) st.loading = false
  }
}

watch(
  () => props.nodeId,
  (id) => {
    seq++ // 旧节点在途的响应作废
    Object.assign(st, { loading: false, error: '', data: null, dep: null, depError: '' })
    if (id) void load()
  },
  { immediate: true },
)

function close() {
  seq++
  emit('close')
}

const nodeEvent = (e: { from_state: string; to_state: string }) => `${labelOf(llmNodeStateLabel, e.from_state) || '—'} → ${labelOf(llmNodeStateLabel, e.to_state)}`

defineExpose({ load })
</script>

<template>
  <el-drawer :model-value="!!props.nodeId" size="680px" title="节点详情" data-testid="llm-node-drawer" @close="close">
    <el-alert v-if="st.error && !node" type="error" :title="st.error" show-icon :closable="false" data-testid="llm-node-load-error">
      <el-button size="small" :loading="st.loading" data-testid="llm-node-retry" @click="load()">重试</el-button>
    </el-alert>
    <el-skeleton v-else-if="!node" :rows="6" animated />
    <div v-if="node && st.data" class="drawer">
      <div class="head">
        <span><b data-testid="llm-node-model">{{ node.model_slug }}</b> <span class="mono muted" data-testid="llm-node-id">{{ node.id }}</span></span>
        <span>
          <el-tag :type="labelOf(llmNodeStateType, node.state) as 'primary'" data-testid="llm-node-state" :data-state="node.state">{{ labelOf(llmNodeStateLabel, node.state) }}</el-tag>
          <el-button size="small" class="gap" :loading="st.loading" data-testid="llm-node-refresh" @click="load()">刷新</el-button>
        </span>
      </div>
      <el-descriptions :column="2" border size="small">
        <el-descriptions-item label="机器"><span data-testid="llm-node-machine">{{ node.machine_name }}（<IdLink kind="machine" :id="node.machine_id" />）</span></el-descriptions-item>
        <el-descriptions-item label="实例"><span data-testid="llm-node-instance"><IdLink kind="instance" :id="node.instance_id" /></span></el-descriptions-item>
        <el-descriptions-item label="在途请求"><span data-testid="llm-node-inflight">{{ node.inflight }}</span></el-descriptions-item>
        <el-descriptions-item label="探测失败 / 重建次数"><span data-testid="llm-node-counts">{{ node.probe_fail_count }} / {{ node.rebuild_count }}</span></el-descriptions-item>
        <el-descriptions-item label="最近探测">{{ fmtTime(node.last_probe_at) || '—' }}</el-descriptions-item>
        <el-descriptions-item label="最近就绪">{{ fmtTime(node.last_ready_at) || '—' }}</el-descriptions-item>
        <el-descriptions-item v-if="node.unhealthy_since" label="不健康起始">{{ fmtTime(node.unhealthy_since) }}</el-descriptions-item>
        <el-descriptions-item v-if="node.drain_since" label="排空开始">{{ fmtTime(node.drain_since) }}</el-descriptions-item>
        <el-descriptions-item label="创建">{{ fmtTime(node.created_at) }}</el-descriptions-item>
        <el-descriptions-item label="更新">{{ fmtTime(node.updated_at) }}</el-descriptions-item>
        <el-descriptions-item label="最近错误" :span="2"><span class="error-text" data-testid="llm-node-error">{{ node.error || '—' }}</span></el-descriptions-item>
      </el-descriptions>

      <h4>节点事件（最近 50 条）</h4>
      <el-timeline v-if="st.data.events.length" data-testid="llm-node-events">
        <el-timeline-item v-for="e in st.data.events" :key="e.id" :timestamp="fmtTimeSec(e.at)">
          <span data-testid="llm-node-event" :data-to="e.to_state">{{ nodeEvent(e) }}</span><span v-if="e.detail" class="muted"> · {{ e.detail }}</span>
        </el-timeline-item>
      </el-timeline>
      <p v-else class="muted" data-testid="llm-node-events-empty">无事件</p>

      <h4>部署</h4>
      <p v-if="!node.deployment_id" class="muted" data-testid="llm-node-no-deployment">还没有创建部署（实例开通完成后由平台创建）</p>
      <template v-else>
        <div class="head">
          <span class="mono" data-testid="llm-node-deployment-id">{{ node.deployment_id }}</span>
          <router-link :to="{ path: '/deployments', query: { dep: node.deployment_id } }" data-testid="llm-node-deployment-link">在部署列表中查看</router-link>
        </div>
        <el-alert v-if="st.depError" type="warning" :title="`部署详情加载失败：${st.depError}`" show-icon :closable="false" data-testid="llm-node-deployment-error" />
        <el-descriptions v-if="dep" :column="2" border size="small">
          <el-descriptions-item label="状态">
            <el-tag size="small" :type="labelOf(deploymentStateType, dep.state) as 'primary'" data-testid="llm-node-deployment-state">{{ labelOf(deploymentStateLabel, dep.state) }}</el-tag>
          </el-descriptions-item>
          <el-descriptions-item label="健康"><span data-testid="llm-node-deployment-health">{{ labelOf(deploymentHealthLabel, dep.health) }}</span></el-descriptions-item>
          <el-descriptions-item label="进度"><span data-testid="llm-node-deployment-progress">{{ progressText(dep) || '—' }}</span></el-descriptions-item>
          <el-descriptions-item label="版本">{{ dep.app_name }} · {{ dep.version }}</el-descriptions-item>
          <el-descriptions-item label="错误" :span="2">
            <span data-testid="llm-node-deployment-err">{{ dep.error_code ? deploymentErrorText(dep.error_code, dep.message) : dep.message || '—' }}</span>
          </el-descriptions-item>
        </el-descriptions>
        <h4>部署事件</h4>
        <table class="grid" data-testid="llm-node-deployment-events">
          <tbody>
            <tr v-for="e in depEvents" :key="e.id" data-testid="llm-node-deployment-event">
              <td class="time">{{ fmtTimeSec(e.at) }}</td>
              <td>{{ labelOf(deploymentStateLabel, e.from_state) || '—' }} → {{ labelOf(deploymentStateLabel, e.to_state) }}</td>
              <td>{{ e.detail }}</td>
            </tr>
            <tr v-if="!depEvents.length"><td class="muted empty">无事件</td></tr>
          </tbody>
        </table>
        <h4>日志尾部（已脱敏）</h4>
        <!-- 按文本渲染（插值），从不 v-html -->
        <pre class="log" data-testid="llm-node-deployment-log">{{ dep?.log_tail || '（暂无日志）' }}</pre>
      </template>
    </div>
  </el-drawer>
</template>

<style scoped>
.drawer { display: grid; gap: 12px; }
.drawer h4 { margin: 8px 0 0; }
.head { display: flex; justify-content: space-between; align-items: center; gap: 8px; }
.gap { margin-left: 8px; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.muted { color: #909399; font-size: 13px; }
.error-text { color: var(--el-color-danger); }
.grid { width: 100%; border-collapse: collapse; font-size: 13px; }
.grid td { padding: 6px 8px; text-align: left; border-bottom: 1px solid var(--el-border-color-lighter); vertical-align: top; }
.time { white-space: nowrap; color: #909399; }
.empty { text-align: center; }
.log { background: #1e1e1e; color: #d4d4d4; padding: 12px; border-radius: 4px; max-height: 360px; overflow: auto; font-size: 12px; white-space: pre-wrap; word-break: break-all; margin: 0; }
</style>
