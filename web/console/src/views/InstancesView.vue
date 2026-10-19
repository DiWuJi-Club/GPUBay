<script setup lang="ts">
import { instanceStateLabel, instanceStateType, labelOf, messageOf, useMobile, type Instance, type Listed } from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, onMounted, ref } from 'vue'
import { useRoute } from 'vue-router'
import { api } from '../api'
import { usePolling } from '../composables/usePolling'
import { useTicker } from '../composables/useTicker'
import { canCancel, instanceTimeText, remainingNow } from '../logic/instance'
import { notifyError } from '../logic/notify'

const route = useRoute()
const mobile = useMobile()
const items = ref<Instance[]>([])
const fetchedAt = ref(0)
const loading = ref(true)
const loadError = ref('')
const now = useTicker()
let failing = false
const batchFilter = computed(() => (typeof route.query.batch === 'string' ? route.query.batch : ''))

async function load() {
  try {
    items.value = (await api.get<Listed<Instance>>('/instances')).items // 后端已按创建时间倒序、最多 100 条
    fetchedAt.value = Date.now()
    loadError.value = ''
    failing = false
  } catch (e) {
    if (!failing) notifyError(e) // 持续故障只提示一次，成功后复位
    failing = true
    if (fetchedAt.value === 0) loadError.value = messageOf(e) // 首次加载失败才占位；轮询失败只提示
  } finally {
    loading.value = false
  }
}

async function retry() {
  loading.value = true
  loadError.value = ''
  await load()
}

// 有开通中 / 洗机中 / 已预订的实例时每 10 秒刷新（预订到开通时刻会自动变为开通中）
const busy = computed(() => items.value.some((i) => i.state === 'creating' || i.state === 'wiping' || i.state === 'reserved'))
usePolling(load, 10_000, { enabled: () => busy.value })
onMounted(load)

const shown = computed(() => (batchFilter.value ? items.value.filter((i) => i.batch_id === batchFilter.value) : items.value))
const rows = computed(() =>
  shown.value.map((i) => {
    const remaining = remainingNow(i, fetchedAt.value, now.value)
    return { ...i, remaining, timeText: instanceTimeText(i, remaining), cancellable: canCancel(i, now.value) }
  }),
)
const cancellableCount = computed(() => rows.value.filter((r) => r.cancellable).length)
const cancelling = ref(false)
/** 取消本批次全部未开通实例：逐台调用，成功 / 失败各自提示。 */
async function cancelAll() {
  if (cancelling.value) return
  cancelling.value = true
  let ok = 0
  const failed: string[] = []
  for (const r of rows.value.filter((x) => x.cancellable)) {
    try {
      await api.post(`/instances/${r.id}/cancel`, {})
      ok++
    } catch (e) {
      failed.push(`${r.id}：${messageOf(e)}`)
    }
  }
  cancelling.value = false
  if (ok) ElMessage.success(`已取消 ${ok} 台`)
  for (const f of failed) ElMessage.error(f)
  await load()
}
</script>

<template>
  <div class="instances">
    <el-card v-if="batchFilter" class="batch-head" data-testid="instances-batch-head">
      <div class="batch-row">
        <span>批次 <code class="mono">{{ batchFilter }}</code> · {{ rows.length }} 台</span>
        <span class="grow" />
        <router-link to="/instances"><el-button size="small">查看全部实例</el-button></router-link>
        <el-button v-if="cancellableCount" size="small" type="danger" plain :loading="cancelling" data-testid="instances-cancel-all" @click="cancelAll">取消全部未开通（{{ cancellableCount }}）</el-button>
      </div>
    </el-card>
    <el-alert v-if="loadError" type="error" :title="loadError" show-icon :closable="false" data-testid="instances-error">
      <el-button size="small" data-testid="instances-retry" @click="retry">重试</el-button>
    </el-alert>
    <el-skeleton v-else-if="loading" :rows="4" animated />
    <el-empty v-else-if="rows.length === 0" :description="batchFilter ? '该批次没有实例' : '还没有实例，去算力市场租一台吧'" data-testid="instances-empty">
      <router-link to="/market"><el-button type="primary">去算力市场</el-button></router-link>
    </el-empty>

    <div v-else-if="mobile" class="cards" data-testid="instances-cards">
      <el-card v-for="i in rows" :key="i.id" class="card" data-testid="instance-card" :data-id="i.id" :data-state="i.state">
        <div class="card-head">
          <router-link :to="`/instances/${i.id}`" class="mono" data-testid="instance-id">{{ i.id }}</router-link>
          <el-tag :type="labelOf(instanceStateType, i.state) as 'primary'" data-testid="instance-state">{{ labelOf(instanceStateLabel, i.state) }}</el-tag>
        </div>
        <div class="muted" data-testid="instance-image">{{ i.image_name }}</div>
        <div v-if="i.batch_id" class="muted"><router-link :to="{ path: '/instances', query: { batch: i.batch_id } }" data-testid="instance-batch">批次 {{ i.batch_id }}</router-link></div>
        <div data-testid="instance-time" :data-remaining="i.remaining">{{ i.timeText }}</div>
        <div v-if="i.reassign_count > 0" class="muted" data-testid="instance-reassigned">已更换机器</div>
        <div v-if="i.cancellable" class="muted" data-testid="instance-cancellable">可取消</div>
        <div v-if="i.suspended" class="warn" data-testid="instance-suspended">计时已暂停（机器维护中）</div>
      </el-card>
    </div>

    <el-table v-else :data="rows" data-testid="instances-table" row-key="id">
      <el-table-column label="实例 ID" min-width="170">
        <template #default="{ row }"><router-link :to="`/instances/${row.id}`" class="mono" data-testid="instance-id">{{ row.id }}</router-link></template>
      </el-table-column>
      <el-table-column label="镜像" min-width="160">
        <template #default="{ row }"><span data-testid="instance-image">{{ row.image_name }}</span></template>
      </el-table-column>
      <el-table-column label="状态" width="130">
        <template #default="{ row }">
          <el-tag :type="labelOf(instanceStateType, row.state) as 'primary'" data-testid="instance-state">{{ labelOf(instanceStateLabel, row.state) }}</el-tag>
          <div v-if="row.suspended" class="warn" data-testid="instance-suspended">计时已暂停（机器维护中）</div>
        </template>
      </el-table-column>
      <el-table-column label="时长" min-width="220">
        <template #default="{ row }"><span data-testid="instance-time" :data-remaining="row.remaining">{{ row.timeText }}</span></template>
      </el-table-column>
      <el-table-column label="批次 / 备注" min-width="200">
        <template #default="{ row }">
          <router-link v-if="row.batch_id" :to="{ path: '/instances', query: { batch: row.batch_id } }" class="mono" data-testid="instance-batch">{{ row.batch_id }}</router-link>
          <span v-if="row.reassign_count > 0" class="muted" data-testid="instance-reassigned"> 已更换机器</span>
          <span v-if="row.cancellable" class="muted" data-testid="instance-cancellable"> 可取消</span>
        </template>
      </el-table-column>
    </el-table>
  </div>
</template>

<style scoped>
.cards { display: grid; gap: 12px; }
.card-head { display: flex; justify-content: space-between; align-items: center; margin-bottom: 6px; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.muted { color: #909399; font-size: 13px; margin-bottom: 4px; }
.warn { color: var(--el-color-warning); font-size: 12px; }
.batch-head { margin-bottom: 12px; }
.batch-row { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.grow { flex: 1; }
</style>
