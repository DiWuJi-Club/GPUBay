<script setup lang="ts">
import { fmtDate, fmtTime, fmtYuan, messageOf, useMobile, type LLMKey, type LLMModelPublic } from '@spark/shared'
import { ElMessage, ElMessageBox } from 'element-plus'
import { computed, onMounted, ref } from 'vue'
import { useRoute } from 'vue-router'
import { api } from '../../api'
import { notifyError } from '../../logic/notify'
import { keyHintMask, keyStatusTag } from '../../logic/tokens'
import { session } from '../../store/session'
import CreateKeyDialog from './CreateKeyDialog.vue'

// API Keys Tab（规格 §9.2 第 2 项）：GET /llm/keys 列表；创建 / 编辑走 CreateKeyDialog；停用 / 启用、撤销在行内。
const props = defineProps<{ models: LLMModelPublic[] }>()
const route = useRoute()
const mobile = useMobile()
const MAX_KEYS = 20 // 后端上限（active + disabled），超出返回 llm_key_limit

const keys = ref<LLMKey[]>([])
const loading = ref(true)
const loadError = ref('')
const busy = ref<number | null>(null)
const dialog = ref(false)
const editing = ref<LLMKey | null>(null)

const kycVerified = computed(() => session.user?.kyc_status === 'verified')
const kycLink = computed(() => ({ path: '/kyc', query: { redirect: route.fullPath } }))
const liveCount = computed(() => keys.value.filter((k) => k.status !== 'revoked').length)

const rows = computed(() => {
  const now = new Date()
  return keys.value.map((k) => ({
    k,
    revoked: k.status === 'revoked',
    status: keyStatusTag(k, now),
    hint: keyHintMask(k.key_hint),
    cap: k.monthly_cap == null ? '不限' : fmtYuan(k.monthly_cap),
    cost: fmtYuan(k.month_cost),
    lastUsed: k.last_used_at ? fmtTime(k.last_used_at) : '从未使用',
    expires: k.expires_at ? fmtDate(k.expires_at) : '永不过期',
  }))
})

async function load() {
  loadError.value = ''
  try {
    keys.value = (await api.get<{ items: LLMKey[] }>('/llm/keys')).items
  } catch (e) {
    notifyError(e)
    loadError.value = messageOf(e)
  } finally {
    loading.value = false
  }
}
onMounted(load)

function openCreate() {
  if (!kycVerified.value) return
  editing.value = null
  dialog.value = true
}
function openEdit(k: LLMKey) {
  editing.value = k
  dialog.value = true
}

async function toggle(k: LLMKey) {
  if (busy.value != null) return
  const status = k.status === 'active' ? 'disabled' : 'active'
  busy.value = k.id
  try {
    await api.put<LLMKey>(`/llm/keys/${k.id}`, { status })
    ElMessage.success(status === 'disabled' ? '已停用' : '已启用')
    await load()
  } catch (e) {
    notifyError(e)
  } finally {
    busy.value = null
  }
}

async function revoke(k: LLMKey) {
  if (busy.value != null) return
  try {
    await ElMessageBox.confirm(`撤销「${k.name}」后，使用该 Key 的请求将被拒绝（最多 30 秒内生效），且无法恢复。确定撤销？`, '撤销 API Key', {
      type: 'warning',
      confirmButtonText: '撤销',
      cancelButtonText: '取消',
      confirmButtonClass: 'el-button--danger',
    })
  } catch {
    return // 取消
  }
  busy.value = k.id
  try {
    await api.del(`/llm/keys/${k.id}`)
    ElMessage.success('已撤销')
    await load()
  } catch (e) {
    notifyError(e)
  } finally {
    busy.value = null
  }
}

</script>

<template>
  <div class="keys">
    <div class="head">
      <div>
        <span class="title">API Keys</span>
        <span class="muted" data-testid="tokens-key-count">{{ liveCount }} / {{ MAX_KEYS }}</span>
      </div>
      <div class="create">
        <router-link v-if="!kycVerified" :to="kycLink" data-testid="tokens-key-kyc-link">去实名认证</router-link>
        <el-tooltip :disabled="kycVerified" content="完成实名认证后才能创建 API Key" placement="top">
          <!-- 禁用的按钮收不到鼠标事件，tooltip 挂在外层 span 上 -->
          <span>
            <el-button type="primary" :disabled="!kycVerified" data-testid="tokens-key-create" @click="openCreate">创建 Key</el-button>
          </span>
        </el-tooltip>
      </div>
    </div>
    <p class="muted intro">
      调用时在请求头带 <code>Authorization: Bearer &lt;Key&gt;</code>。一个账号下所有 Key 共享限额与钱包余额；明文 Key 只在创建时显示一次。
    </p>

    <el-alert v-if="loadError" type="error" :title="loadError" show-icon :closable="false" data-testid="tokens-keys-error">
      <el-button size="small" data-testid="tokens-keys-retry" @click="load">重试</el-button>
    </el-alert>
    <el-skeleton v-else-if="loading" :rows="3" animated />
    <el-empty v-else-if="rows.length === 0" description="还没有 API Key" data-testid="tokens-keys-empty" />

    <!-- 手机：卡片 -->
    <div v-else-if="mobile" class="cards" data-testid="tokens-keys-cards">
      <el-card v-for="r in rows" :key="r.k.id" :data-testid="`tokens-key-row-${r.k.id}`">
        <div class="card-head">
          <b data-testid="tokens-key-name-cell">{{ r.k.name }}</b>
          <el-tag :type="r.status.type" size="small" :data-testid="`tokens-key-status-${r.k.id}`">{{ r.status.label }}</el-tag>
        </div>
        <code class="mono" data-testid="tokens-key-hint">{{ r.hint }}</code>
        <ul class="meta">
          <li>
            <span class="label">允许模型</span>
            <span v-if="r.k.model_slugs.length === 0" data-testid="tokens-key-models-all">全部</span>
            <span v-else class="tags"><el-tag v-for="s in r.k.model_slugs" :key="s" size="small" effect="plain">{{ s }}</el-tag></span>
          </li>
          <li><span class="label">月上限</span><span data-testid="tokens-key-cap-cell">{{ r.cap }}</span></li>
          <li><span class="label">本月已用</span><span data-testid="tokens-key-cost">{{ r.cost }}</span></li>
          <li><span class="label">最近使用</span>{{ r.lastUsed }}</li>
          <li><span class="label">到期</span>{{ r.expires }}</li>
        </ul>
        <div v-if="!r.revoked" class="actions">
          <el-button size="small" :loading="busy === r.k.id" :data-testid="`tokens-key-disable-${r.k.id}`" @click="toggle(r.k)">{{ r.k.status === 'active' ? '停用' : '启用' }}</el-button>
          <el-button size="small" :data-testid="`tokens-key-edit-${r.k.id}`" @click="openEdit(r.k)">编辑</el-button>
          <el-button size="small" type="danger" plain :data-testid="`tokens-key-revoke-${r.k.id}`" @click="revoke(r.k)">撤销</el-button>
        </div>
      </el-card>
    </div>

    <!-- 电脑：表格 -->
    <el-table v-else :data="rows" row-key="k.id" data-testid="tokens-keys-table">
      <el-table-column label="名称" min-width="120">
        <template #default="{ row }"><span :data-testid="`tokens-key-row-${row.k.id}`">{{ row.k.name }}</span></template>
      </el-table-column>
      <el-table-column label="Key" min-width="150">
        <template #default="{ row }"><code class="mono" data-testid="tokens-key-hint">{{ row.hint }}</code></template>
      </el-table-column>
      <el-table-column label="允许模型" min-width="160">
        <template #default="{ row }">
          <span v-if="row.k.model_slugs.length === 0" data-testid="tokens-key-models-all">全部</span>
          <span v-else class="tags"><el-tag v-for="s in row.k.model_slugs" :key="s" size="small" effect="plain">{{ s }}</el-tag></span>
        </template>
      </el-table-column>
      <el-table-column label="月上限" min-width="100">
        <template #default="{ row }"><span data-testid="tokens-key-cap-cell">{{ row.cap }}</span></template>
      </el-table-column>
      <el-table-column label="本月已用" min-width="100">
        <template #default="{ row }"><span data-testid="tokens-key-cost">{{ row.cost }}</span></template>
      </el-table-column>
      <el-table-column label="最近使用" min-width="140">
        <template #default="{ row }">{{ row.lastUsed }}</template>
      </el-table-column>
      <el-table-column label="到期" min-width="110">
        <template #default="{ row }">{{ row.expires }}</template>
      </el-table-column>
      <el-table-column label="状态" width="90">
        <template #default="{ row }">
          <el-tag :type="row.status.type" size="small" :data-testid="`tokens-key-status-${row.k.id}`">{{ row.status.label }}</el-tag>
        </template>
      </el-table-column>
      <el-table-column label="操作" min-width="190">
        <template #default="{ row }">
          <template v-if="!row.revoked">
            <el-button link type="primary" :loading="busy === row.k.id" :data-testid="`tokens-key-disable-${row.k.id}`" @click="toggle(row.k)">
              {{ row.k.status === 'active' ? '停用' : '启用' }}
            </el-button>
            <el-button link type="primary" :data-testid="`tokens-key-edit-${row.k.id}`" @click="openEdit(row.k)">编辑</el-button>
            <el-button link type="danger" :data-testid="`tokens-key-revoke-${row.k.id}`" @click="revoke(row.k)">撤销</el-button>
          </template>
        </template>
      </el-table-column>
    </el-table>

    <CreateKeyDialog v-model="dialog" :models="props.models" :editing="editing" @saved="load" />
  </div>
</template>

<style scoped>
.keys { display: grid; gap: 12px; }
.head { display: flex; justify-content: space-between; align-items: center; gap: 12px; flex-wrap: wrap; }
.title { font-weight: 600; margin-right: 8px; }
.create { display: flex; align-items: center; gap: 12px; }
.intro { margin: 0; }
.muted { color: #909399; font-size: 12px; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; word-break: break-all; font-size: 12px; }
.tags { display: inline-flex; flex-wrap: wrap; gap: 4px; }
.cards { display: grid; gap: 12px; }
.card-head { display: flex; justify-content: space-between; align-items: center; gap: 8px; margin-bottom: 6px; }
.meta { list-style: none; padding: 0; margin: 8px 0; display: grid; gap: 4px; font-size: 14px; }
.meta .label { display: inline-block; width: 64px; color: #909399; }
.actions { display: flex; gap: 8px; flex-wrap: wrap; }
</style>
