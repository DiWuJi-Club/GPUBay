<script setup lang="ts">
import { labelOf, llmModelKindLabel, llmModelStatusLabel, llmModelStatusType, messageOf, type LLMModelAdmin, type LLMModelStatus, type Listed } from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, onMounted, reactive, ref } from 'vue'
import { api } from '../../api'
import { fmtPrice, wantedReplicas } from '../../logic/llm'
import { notifyError } from '../../logic/notify'
import { hasRole } from '../../store/session'
import ModelDrawer from './ModelDrawer.vue'

// 模型 Tab（规格 §11）：所有角色可看；新建 / 编辑 / 上下线（动态码）/ 删除只有 super（接口层同样只允许 super）。
const canWrite = computed(() => hasRole(['super']))

const items = ref<LLMModelAdmin[]>([])
const loading = ref(false)
const error = ref('')

async function load() {
  loading.value = true
  try {
    const out = await api.get<Listed<LLMModelAdmin>>('/llm/models')
    items.value = [...out.items].sort((a, b) => a.sort - b.sort || a.id - b.id)
    error.value = ''
  } catch (e) {
    notifyError(e)
    error.value = messageOf(e)
  } finally {
    loading.value = false
  }
}
onMounted(load)

// ---- 抽屉 ----
const drawer = reactive({ open: false, model: null as LLMModelAdmin | null })
function openDrawer(m: LLMModelAdmin | null) {
  Object.assign(drawer, { open: true, model: m })
}
function onSaved() {
  drawer.open = false
  void load()
}

// ---- 上下线（POST /llm/models/{id}/status：请求层收到 totp_required 时弹全局动态码框并带 totp 重发）----
const status = reactive({ visible: false, target: null as LLMModelAdmin | null, to: 'online' as LLMModelStatus, busy: false })
const statusBlock = computed(() => (status.to === 'online' && (status.target?.replicas ?? 0) < 1 ? '上线前请设置至少 1 个副本' : ''))
function openStatus(m: LLMModelAdmin, to: LLMModelStatus) {
  Object.assign(status, { visible: true, target: m, to, busy: false })
}
async function confirmStatus() {
  const m = status.target
  if (!m || status.busy || statusBlock.value) return
  status.busy = true
  try {
    await api.post(`/llm/models/${m.id}/status`, { status: status.to })
    ElMessage.success(status.to === 'online' ? '模型已上线' : '模型已下线')
    status.visible = false
    await load()
  } catch (e) {
    notifyError(e) // 动态码取消（cancelled）静默，弹窗保留
  } finally {
    status.busy = false
  }
}

// ---- 删除：只有草稿且没有节点（用量引用由服务器判定 llm_model_in_use）----
const canDelete = (m: LLMModelAdmin) => m.status === 'draft' && m.nodes.total === 0
const del = reactive({ visible: false, target: null as LLMModelAdmin | null, busy: false })
async function confirmDelete() {
  const m = del.target
  if (!m || del.busy) return
  del.busy = true
  try {
    await api.del(`/llm/models/${m.id}`, {})
    ElMessage.success('模型已删除')
    del.visible = false
    await load()
  } catch (e) {
    del.visible = false
    notifyError(e)
  } finally {
    del.busy = false
  }
}

defineExpose({ load })
</script>

<template>
  <div class="models">
    <div class="bar">
      <span class="hint">价格单位：元 / 百万 token。来源、镜像、上下文与节点参数的修改只影响之后新建的节点。</span>
      <span>
        <el-button :loading="loading" data-testid="llm-models-refresh" @click="load">刷新</el-button>
        <el-button v-if="canWrite" type="primary" data-testid="llm-model-create" @click="openDrawer(null)">新建模型</el-button>
      </span>
    </div>
    <el-alert v-if="error" type="error" :title="error" show-icon :closable="false" data-testid="llm-models-error">
      <el-button size="small" :loading="loading" @click="load">重试</el-button>
    </el-alert>
    <el-table v-loading="loading" :data="items" row-key="id" data-testid="llm-models-table">
      <template #empty><el-empty description="还没有模型" :image-size="60" /></template>
      <el-table-column label="slug" min-width="160">
        <template #default="{ row }"><span class="mono" data-testid="llm-model-row" :data-slug="row.slug">{{ row.slug }}</span></template>
      </el-table-column>
      <el-table-column label="名称" prop="name" min-width="140" />
      <el-table-column label="类型" width="70"><template #default="{ row }">{{ labelOf(llmModelKindLabel, row.kind) }}</template></el-table-column>
      <el-table-column label="输入价格" width="100"><template #default="{ row }"><span data-testid="llm-model-row-input-price">{{ fmtPrice(row.input_price) }}</span></template></el-table-column>
      <el-table-column label="输出价格" width="100"><template #default="{ row }"><span data-testid="llm-model-row-output-price">{{ fmtPrice(row.output_price) }}</span></template></el-table-column>
      <el-table-column label="上下文" width="90"><template #default="{ row }">{{ row.context_len.toLocaleString('en-US') }}</template></el-table-column>
      <el-table-column label="副本 期望 / 就绪" width="170">
        <template #default="{ row }">
          <!-- 期望 = 实际期望（只有上线才按 replicas 建节点），与总览一致；未上线时另标设置值 -->
          <span data-testid="llm-model-row-replicas">{{ wantedReplicas(row as LLMModelAdmin) }} / {{ row.nodes.ready }}</span>
          <span v-if="row.status !== 'online' && row.replicas > 0" class="hint" data-testid="llm-model-row-replicas-set">（上线后 {{ row.replicas }}）</span>
          <span v-if="row.nodes.total > row.nodes.ready" class="hint">（共 {{ row.nodes.total }} 个节点）</span>
        </template>
      </el-table-column>
      <el-table-column label="状态" width="90">
        <template #default="{ row }"><el-tag size="small" :type="labelOf(llmModelStatusType, row.status) as 'primary'" data-testid="llm-model-row-status">{{ labelOf(llmModelStatusLabel, row.status) }}</el-tag></template>
      </el-table-column>
      <el-table-column label="操作" :width="canWrite ? 220 : 90" fixed="right">
        <template #default="{ row }">
          <template v-if="canWrite">
            <el-button size="small" data-testid="llm-model-edit" @click="openDrawer(row as LLMModelAdmin)">编辑</el-button>
            <el-button v-if="row.status !== 'online'" size="small" type="success" plain data-testid="llm-model-online" @click="openStatus(row as LLMModelAdmin, 'online')">上线</el-button>
            <el-button v-else size="small" type="warning" plain data-testid="llm-model-offline" @click="openStatus(row as LLMModelAdmin, 'offline')">下线</el-button>
            <el-button v-if="canDelete(row as LLMModelAdmin)" size="small" type="danger" plain data-testid="llm-model-delete" @click="Object.assign(del, { visible: true, target: row, busy: false })">删除</el-button>
          </template>
          <el-button v-else size="small" data-testid="llm-model-view" @click="openDrawer(row as LLMModelAdmin)">查看</el-button>
        </template>
      </el-table-column>
    </el-table>

    <ModelDrawer :open="drawer.open" :model="drawer.model" :readonly="!canWrite" @close="drawer.open = false" @saved="onSaved" />

    <el-dialog v-model="status.visible" :title="status.to === 'online' ? '上线模型' : '下线模型'" width="460" align-center data-testid="llm-model-status-dialog">
      <p v-if="status.to === 'online'" data-testid="llm-model-status-text">
        上线后「{{ status.target?.name }}」（{{ status.target?.slug }}）在控制台与 /v1/models 可见，平台按期望副本数（{{ status.target?.replicas }}）自动建节点。需要输入动态码。
      </p>
      <p v-else data-testid="llm-model-status-text">
        下线后「{{ status.target?.name }}」（{{ status.target?.slug }}）不能再调用，现有节点将被排空释放。需要输入动态码。
      </p>
      <el-alert v-if="statusBlock" type="error" :title="statusBlock" show-icon :closable="false" data-testid="llm-model-status-block" />
      <template #footer>
        <el-button data-testid="llm-model-status-cancel" @click="status.visible = false">取消</el-button>
        <el-button :type="status.to === 'online' ? 'primary' : 'warning'" :loading="status.busy" :disabled="!!statusBlock" data-testid="llm-model-status-ok" @click="confirmStatus">
          {{ status.to === 'online' ? '确定上线' : '确定下线' }}
        </el-button>
      </template>
    </el-dialog>

    <el-dialog v-model="del.visible" title="删除模型" width="420" align-center data-testid="llm-model-delete-dialog">
      <p data-testid="llm-model-delete-text">确定删除草稿模型「{{ del.target?.name }}」（{{ del.target?.slug }}）？删除后不可恢复。</p>
      <template #footer>
        <el-button data-testid="llm-model-delete-cancel" @click="del.visible = false">取消</el-button>
        <el-button type="danger" :loading="del.busy" data-testid="llm-model-delete-ok" @click="confirmDelete">确定删除</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
.models { display: grid; gap: 12px; }
.bar { display: flex; justify-content: space-between; align-items: center; gap: 12px; }
.hint { color: #909399; font-size: 12px; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
</style>
