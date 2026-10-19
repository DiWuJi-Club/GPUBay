<script setup lang="ts">
import { fmtTime, instanceStateLabel, labelOf, machineStateLabel, messageOf, type MachineDetail } from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, onMounted, ref } from 'vue'
import { onBeforeRouteUpdate, useRoute } from 'vue-router'
import { api } from '../api'
import IdLink from '../components/IdLink.vue'
import { notifyError } from '../logic/notify'

const route = useRoute()
const id = computed(() => String(route.params.id))
const data = ref<MachineDetail | null>(null)
const loading = ref(false)
const error = ref('')
const maintenanceVisible = ref(false)
const powerVisible = ref(false)
const busy = ref(false)
const inMaintenance = computed(() => data.value?.machine.state === 'maintenance' || data.value?.machine.state === 'faulty')

let seq = 0
/**
 * 首次加载失败：页内错误 + 重试按钮（不自动重试）；操作后的刷新失败只提示，保留已显示的数据。
 * 只采用最后一次请求的结果：从一台机器的详情切到另一台时，先发后到的旧响应被丢弃。
 */
async function load(target = id.value) {
  const mine = ++seq
  loading.value = true
  try {
    const d = await api.get<MachineDetail>(`/machines/${target}`)
    if (mine !== seq) return
    data.value = d
    error.value = ''
  } catch (e) {
    if (mine !== seq) return
    notifyError(e)
    if (!data.value) error.value = messageOf(e)
  } finally {
    if (mine === seq) loading.value = false
  }
}
onMounted(() => void load())
// 同一组件换了机器（/machines/1 → /machines/2）：清掉旧机器的数据与弹窗，按新 ID 重新加载。
// 用 onBeforeRouteUpdate 而不是 watch(route.params.id)：离开到实例详情时 params.id 也会变，watch 会拿实例 ID 去请求机器
onBeforeRouteUpdate((to) => {
  if (to.params.id === route.params.id) return
  data.value = null
  error.value = ''
  maintenanceVisible.value = false
  powerVisible.value = false
  void load(String(to.params.id))
})

async function toggleMaintenance() {
  if (busy.value) return // 同步守卫：连点只发一次
  busy.value = true
  const on = !inMaintenance.value
  try {
    await api.post(`/machines/${id.value}/maintenance`, { on })
    ElMessage.success(on ? '已进入维护' : '已结束维护')
    maintenanceVisible.value = false
    await load()
  } catch (e) {
    maintenanceVisible.value = false
    notifyError(e) // 「该机器上还有未结束的实例，不能结束维护」等原文
  } finally {
    busy.value = false
  }
}
async function powerCycle() {
  if (busy.value) return // 同步守卫：断电重启不是幂等操作，连点只发一次
  busy.value = true
  try {
    await api.post(`/machines/${id.value}/power-cycle`, {})
    ElMessage.success('已通过 PDU 断电重启')
    powerVisible.value = false
  } catch (e) {
    powerVisible.value = false
    notifyError(e) // 「该机器未配置 PDU」等原文
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <div>
    <el-alert v-if="error && !data" type="error" :title="error" show-icon :closable="false" data-testid="mdetail-error">
      <el-button size="small" :loading="loading" data-testid="mdetail-retry" @click="load()">重试</el-button>
    </el-alert>
    <div v-if="data" class="detail">
      <el-card>
        <template #header><div class="head"><span class="mono" data-testid="mdetail-serial">{{ data.machine.serial }}</span><el-tag data-testid="mdetail-state">{{ labelOf(machineStateLabel, data.machine.state) }}</el-tag></div></template>
        <el-descriptions :column="3" border>
          <el-descriptions-item label="机柜 / 槽位">{{ data.machine.rack }} / {{ data.machine.slot }}</el-descriptions-item>
          <el-descriptions-item label="内网 IP"><span class="mono">{{ data.machine.lan_ip }}</span></el-descriptions-item>
          <el-descriptions-item label="在线"><span data-testid="mdetail-online">{{ data.machine.agent_online ? '在线' : '离线' }}</span>（{{ fmtTime(data.machine.last_seen_at) || '从未心跳' }}）</el-descriptions-item>
          <el-descriptions-item label="PDU">{{ data.machine.pdu_id ? `#${data.machine.pdu_id} 插座 ${data.machine.pdu_outlet}` : '未配置' }}</el-descriptions-item>
          <el-descriptions-item label="登记时间">{{ fmtTime(data.machine.created_at) }}</el-descriptions-item>
          <el-descriptions-item label="当前实例">
            <span v-if="data.current_instance" data-testid="mdetail-current"><IdLink kind="instance" :id="data.current_instance.id" /></span>
            <span v-else class="muted" data-testid="mdetail-none">无</span>
          </el-descriptions-item>
        </el-descriptions>
        <div class="actions">
          <el-button data-testid="mdetail-maintenance-btn" @click="maintenanceVisible = true">{{ inMaintenance ? '结束维护' : '进入维护' }}</el-button>
          <el-button type="danger" plain data-testid="mdetail-power-btn" @click="powerVisible = true">断电重启</el-button>
        </div>
      </el-card>

      <el-card header="历史实例">
        <el-table :data="data.history" row-key="id">
          <el-table-column label="实例"><template #default="{ row }"><span data-testid="mdetail-history-row"><span data-testid="mdetail-history-id"><IdLink kind="instance" :id="row.id" /></span></span></template></el-table-column>
          <el-table-column label="用户" width="100"><template #default="{ row }"><IdLink kind="user" :id="row.user_id" /></template></el-table-column>
          <el-table-column label="状态" width="120"><template #default="{ row }">{{ labelOf(instanceStateLabel, row.state) }}</template></el-table-column>
          <el-table-column label="创建时间" width="160"><template #default="{ row }">{{ fmtTime(row.created_at) }}</template></el-table-column>
        </el-table>
      </el-card>

      <el-dialog v-model="maintenanceVisible" :title="inMaintenance ? '结束维护' : '进入维护'" width="420" align-center data-testid="mdetail-maintenance-dialog">
        <p>{{ inMaintenance ? '结束维护后机器重新进入可售库存。' : '进入维护后该机器不再分配给新订单。' }}</p>
        <template #footer>
          <el-button @click="maintenanceVisible = false">取消</el-button>
          <el-button type="primary" :loading="busy" data-testid="mdetail-maintenance-ok" @click="toggleMaintenance">确定</el-button>
        </template>
      </el-dialog>
      <el-dialog v-model="powerVisible" title="断电重启" width="420" align-center data-testid="mdetail-power-dialog">
        <p data-testid="mdetail-power-text">将通过 PDU 对该机器断电重启，机器上的实例会中断，确定执行？</p>
        <template #footer>
          <el-button data-testid="mdetail-power-cancel" @click="powerVisible = false">取消</el-button>
          <el-button type="danger" :loading="busy" data-testid="mdetail-power-ok" @click="powerCycle">确定执行</el-button>
        </template>
      </el-dialog>
    </div>
  </div>
</template>

<style scoped>
.detail { display: grid; gap: 16px; }
.head { display: flex; justify-content: space-between; align-items: center; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.muted { color: #909399; }
.actions { display: flex; gap: 12px; margin-top: 16px; }
</style>
