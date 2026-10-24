<script setup lang="ts">
import {
  digitsOnly, fmtTime, fmtYuan, labelOf, llmKeyStatusLabel, messageOf,
  type LLMAccountAdmin, type LLMAccountUpdate, type LLMKeyAdmin, type Listed, type Paged,
} from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, onMounted, reactive, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import { api } from '../../api'
import { usePaged } from '../../composables/usePaged'
import { firstOf } from '../../logic/deployments'
import {
  accountFilterFromQuery, accountsQuery, blockBody, buildLimitPatch, emptyAccountFilter, fmtCount, keyMonthCostText, LIMIT_BOUNDS, LIMIT_DEFAULTS, LIMIT_KEYS, LIMIT_LABEL, LIMIT_SHORT, limitFormOf, limitText,
  unblockBody, validateReason, type LimitForm, type LimitKey,
} from '../../logic/llm'
import { notifyError } from '../../logic/notify'
import { FIN_ROLES, OPS_ROLES } from '../../menu'
import { hasRole } from '../../store/session'

// 账户 Tab（规格 §11，ops / finance 查看）：搜索 + 欠费 / 停用筛选；ops 编辑限额（空 = 恢复默认）、停用 / 解除停用（原因必填）。
// 展开行查看该用户的 Key（GET /llm/keys：ops / finance / super），停用 Key 只有 ops。?user_id= 按精确用户 ID 筛选并自动展开该用户。
const canOps = computed(() => hasRole(OPS_ROLES))
const canFinance = computed(() => hasRole(FIN_ROLES))
const canSeeKeys = computed(() => canOps.value || canFinance.value)

const route = useRoute()
const filter = reactive(accountFilterFromQuery(route.query))
// 已生效的筛选：只在查询 / 重置 / 地址变化时取快照，翻页与表单里未提交的改动无关
let applied = { ...filter }
const focusUser = ref(firstOf(route.query.user_id))
const list = usePaged<LLMAccountAdmin>((page, size) => api.get<Paged<LLMAccountAdmin>>('/llm/accounts', accountsQuery(applied, page, size)))
onMounted(list.load)
function search() {
  applied = { ...filter }
  list.reset()
}
function resetFilter() {
  Object.assign(filter, emptyAccountFilter())
  focusUser.value = ''
  search()
}
/** 关闭精确用户标签：回到全部用户（其它筛选保留） */
function clearUser() {
  filter.user_id = ''
  focusUser.value = ''
  search()
}
// 用户详情的链接（?user_id=）在 /llm 内切换时：按新地址重新预填
// （el-tabs 切走后本 Tab 仍挂载：只在当前是账户 Tab 时响应，切到用量 Tab 带的 user_id 不影响这里）
watch(
  () => [route.query.tab, route.query.user_id] as const,
  ([tab, v]) => {
    const id = firstOf(v)
    if (firstOf(tab) !== 'accounts' || id === focusUser.value) return
    focusUser.value = id
    Object.assign(filter, accountFilterFromQuery(route.query))
    search()
  },
)

// ---- 展开行：Key 列表（ops / finance 查看，ops 停用）----
const keys = reactive<Record<number, { loading: boolean; error: string; items: LLMKeyAdmin[] }>>({})
const expanded = ref<string[]>([]) // el-table 的 expand-row-keys 是字符串
async function loadKeys(userId: number) {
  if (!keys[userId]) keys[userId] = { loading: false, error: '', items: [] }
  const st = keys[userId] // 从 reactive 里取回代理（`??=` 的值是原始对象，改它不会触发更新）
  st.loading = true
  try {
    st.items = (await api.get<Listed<LLMKeyAdmin>>('/llm/keys', { user_id: userId })).items
    st.error = ''
  } catch (e) {
    notifyError(e)
    st.error = messageOf(e)
  } finally {
    st.loading = false
  }
}
function onExpand(row: LLMAccountAdmin, rows: LLMAccountAdmin[] | boolean) {
  if (typeof rows === 'boolean') return // 只有 type="expand" 列，回调参数是展开行数组
  expanded.value = rows.map((r) => String(r.user_id))
  if (rows.includes(row) && !keys[row.user_id]) void loadKeys(row.user_id)
}
// ?user_id= 命中的那一行自动展开
watch(list.items, (items) => {
  if (!canSeeKeys.value || !focusUser.value) return
  const hit = items.find((a) => String(a.user_id) === focusUser.value)
  if (hit && !expanded.value.includes(String(hit.user_id))) {
    expanded.value = [...expanded.value, String(hit.user_id)]
    void loadKeys(hit.user_id)
  }
})
const modelsText = (k: LLMKeyAdmin) => (k.model_slugs?.length ? k.model_slugs.join('、') : '全部')

// ---- 编辑限额 ----
const limits = reactive({ visible: false, target: null as LLMAccountAdmin | null, form: { rpm: '', tpm: '', concurrency: '' } as LimitForm, errors: {} as Partial<Record<LimitKey, string>>, busy: false })
function openLimits(a: LLMAccountAdmin) {
  Object.assign(limits, { visible: true, target: a, form: limitFormOf(a), errors: {}, busy: false })
}
async function submitLimits() {
  const a = limits.target
  if (!a || limits.busy) return
  const r = buildLimitPatch(a, limits.form)
  if (!r.ok) {
    limits.errors = r.errors
    return
  }
  limits.errors = {}
  if (!Object.keys(r.patch).length) {
    ElMessage.info('没有改动')
    limits.visible = false
    return
  }
  limits.busy = true
  try {
    await api.put(`/llm/accounts/${a.user_id}`, r.patch satisfies LLMAccountUpdate)
    ElMessage.success('限额已更新')
    limits.visible = false
    await list.load()
  } catch (e) {
    notifyError(e) // 弹窗保留，输入不丢
  } finally {
    limits.busy = false
  }
}

// ---- 停用 / 解除停用 ----
const block = reactive({ visible: false, target: null as LLMAccountAdmin | null, reason: '', error: '', busy: false })
function openBlock(a: LLMAccountAdmin) {
  Object.assign(block, { visible: true, target: a, reason: '', error: '', busy: false })
}
async function submitBlock() {
  const a = block.target
  if (!a || block.busy) return
  block.error = validateReason(block.reason, '停用原因') ?? ''
  if (block.error) return
  block.busy = true
  try {
    await api.put(`/llm/accounts/${a.user_id}`, blockBody(block.reason))
    ElMessage.success('已停用该账户的 Token 服务')
    block.visible = false
    await list.load()
  } catch (e) {
    notifyError(e)
  } finally {
    block.busy = false
  }
}
const unblock = reactive({ visible: false, target: null as LLMAccountAdmin | null, busy: false })
function openUnblock(a: LLMAccountAdmin) {
  Object.assign(unblock, { visible: true, target: a, busy: false })
}
async function submitUnblock() {
  const a = unblock.target
  if (!a || unblock.busy) return
  unblock.busy = true
  try {
    await api.put(`/llm/accounts/${a.user_id}`, unblockBody())
    ElMessage.success('已恢复 Token 服务')
    unblock.visible = false
    await list.load()
  } catch (e) {
    notifyError(e)
  } finally {
    unblock.busy = false
  }
}

// ---- 停用 Key ----
const keyOff = reactive({ visible: false, target: null as LLMKeyAdmin | null, reason: '', error: '', busy: false })
function openKeyOff(k: LLMKeyAdmin) {
  Object.assign(keyOff, { visible: true, target: k, reason: '', error: '', busy: false })
}
async function submitKeyOff() {
  const k = keyOff.target
  if (!k || keyOff.busy) return
  keyOff.error = validateReason(keyOff.reason, '停用原因') ?? ''
  if (keyOff.error) return
  keyOff.busy = true
  try {
    await api.post(`/llm/keys/${k.id}/disable`, { reason: keyOff.reason.trim() })
    ElMessage.success('Key 已停用')
    keyOff.visible = false
    await loadKeys(k.user_id)
  } catch (e) {
    notifyError(e)
  } finally {
    keyOff.busy = false
  }
}
</script>

<template>
  <div data-testid="llm-accounts">
    <el-form inline class="filters" @submit.prevent="search">
      <el-form-item label="用户 ID / 手机号">
        <el-input :model-value="filter.q" clearable placeholder="只能输入数字" style="width: 180px" data-testid="llm-acc-q" @update:model-value="(v: string) => (filter.q = digitsOnly(v))" />
      </el-form-item>
      <el-form-item label="停用">
        <el-select v-model="filter.blocked" placeholder="全部" style="width: 110px" data-testid="llm-acc-blocked">
          <el-option label="全部" value="" />
          <el-option label="已停用" value="true" />
          <el-option label="正常" value="false" />
        </el-select>
      </el-form-item>
      <el-form-item><el-checkbox v-model="filter.debt" data-testid="llm-acc-debt">只看欠费</el-checkbox></el-form-item>
      <el-form-item v-if="filter.user_id">
        <el-tag closable data-testid="llm-acc-user-tag" @close="clearUser">用户 ID {{ filter.user_id }}</el-tag>
      </el-form-item>
      <el-button type="primary" native-type="submit" data-testid="llm-acc-search">查询</el-button>
      <el-button data-testid="llm-acc-reset" @click="resetFilter">重置</el-button>
    </el-form>

    <el-alert v-if="list.error.value" type="error" :title="list.error.value" show-icon :closable="false" class="error" data-testid="llm-acc-error">
      <el-button size="small" :loading="list.loading.value" data-testid="llm-acc-retry" @click="list.load()">重试</el-button>
    </el-alert>
    <el-table :data="list.items.value" v-loading="list.loading.value" row-key="user_id" :expand-row-keys="expanded" @expand-change="onExpand">
      <el-table-column v-if="canSeeKeys" type="expand">
        <template #default="{ row }">
          <div class="keys" data-testid="llm-acc-keys" :data-user="row.user_id">
            <p class="muted">用户可自行重新启用被停用的 Key；要强制停用请停用该账户的 Token 服务。</p>
            <el-alert v-if="keys[row.user_id]?.error" type="error" :title="keys[row.user_id].error" show-icon :closable="false">
              <el-button size="small" data-testid="llm-acc-keys-retry" @click="loadKeys(row.user_id)">重试</el-button>
            </el-alert>
            <el-table v-else :data="keys[row.user_id]?.items ?? []" v-loading="keys[row.user_id]?.loading" size="small" row-key="id" empty-text="没有 Key">
              <el-table-column label="名称" min-width="120"><template #default="{ row: k }"><span data-testid="llm-key-name">{{ k.name }}</span></template></el-table-column>
              <el-table-column label="Key" width="150"><template #default="{ row: k }"><span class="mono" data-testid="llm-key-hint">{{ k.key_hint }}</span></template></el-table-column>
              <el-table-column label="状态" width="90"><template #default="{ row: k }"><el-tag size="small" :type="k.status === 'active' ? 'success' : 'info'" data-testid="llm-key-status">{{ labelOf(llmKeyStatusLabel, k.status) }}</el-tag></template></el-table-column>
              <el-table-column label="可用模型" min-width="140"><template #default="{ row: k }">{{ modelsText(k as LLMKeyAdmin) }}</template></el-table-column>
              <el-table-column label="月额度" width="110"><template #default="{ row: k }">{{ k.monthly_cap == null ? '不限' : fmtYuan(k.monthly_cap) }}</template></el-table-column>
              <el-table-column label="本月消费" width="110"><template #default="{ row: k }"><span data-testid="llm-key-cost">{{ keyMonthCostText(k as LLMKeyAdmin) }}</span></template></el-table-column>
              <el-table-column label="最近使用" width="150"><template #default="{ row: k }">{{ fmtTime(k.last_used_at) || '—' }}</template></el-table-column>
              <el-table-column label="到期" width="150"><template #default="{ row: k }">{{ fmtTime(k.expires_at) || '永久' }}</template></el-table-column>
              <el-table-column v-if="canOps" label="操作" width="90">
                <template #default="{ row: k }">
                  <el-button v-if="k.status === 'active'" link type="danger" size="small" data-testid="llm-key-disable" @click="openKeyOff(k as LLMKeyAdmin)">停用</el-button>
                  <span v-else class="muted">—</span>
                </template>
              </el-table-column>
            </el-table>
          </div>
        </template>
      </el-table-column>
      <el-table-column label="手机号" min-width="140">
        <template #default="{ row }">
          <router-link :to="`/users/${row.user_id}`" class="mono" data-testid="llm-acc-row" :data-user="row.user_id">{{ row.phone || row.user_id }}</router-link>
        </template>
      </el-table-column>
      <el-table-column label="Key 数" width="80"><template #default="{ row }"><span data-testid="llm-acc-keys-count">{{ row.key_count }}</span></template></el-table-column>
      <el-table-column label="本月消费" width="110"><template #default="{ row }"><span data-testid="llm-acc-month">{{ fmtYuan(row.month_cost) }}</span></template></el-table-column>
      <el-table-column label="未结算" width="100"><template #default="{ row }"><span data-testid="llm-acc-unsettled">{{ fmtYuan(row.unsettled) }}</span></template></el-table-column>
      <el-table-column label="欠费" width="100"><template #default="{ row }"><span data-testid="llm-acc-debt-amount" :class="{ bad: row.debt > 0 }">{{ fmtYuan(row.debt) }}</span></template></el-table-column>
      <el-table-column label="限额" min-width="220">
        <template #default="{ row }">
          <div class="limits" data-testid="llm-acc-limits">
            <span v-for="k in LIMIT_KEYS" :key="k" :data-testid="`llm-acc-limit-${k}`">{{ LIMIT_SHORT[k] }} {{ limitText(row as LLMAccountAdmin, k) }}</span>
          </div>
        </template>
      </el-table-column>
      <el-table-column label="停用" width="110">
        <template #default="{ row }">
          <el-tooltip v-if="row.blocked" :content="row.blocked_reason || '无原因'" placement="top">
            <el-tag type="danger" size="small" data-testid="llm-acc-blocked-tag">已停用</el-tag>
          </el-tooltip>
          <el-tag v-else type="success" size="small" data-testid="llm-acc-blocked-tag">正常</el-tag>
        </template>
      </el-table-column>
      <el-table-column v-if="canOps || canFinance" label="操作" width="200">
        <template #default="{ row }">
          <template v-if="canOps">
            <el-button link type="primary" size="small" data-testid="llm-acc-edit-limits" @click="openLimits(row as LLMAccountAdmin)">编辑限额</el-button>
            <el-button v-if="!row.blocked" link type="danger" size="small" data-testid="llm-acc-block" @click="openBlock(row as LLMAccountAdmin)">停用</el-button>
            <el-button v-else link type="primary" size="small" data-testid="llm-acc-unblock" @click="openUnblock(row as LLMAccountAdmin)">解除停用</el-button>
          </template>
          <router-link v-if="canFinance" :to="{ path: '/llm', query: { tab: 'usage', user_id: row.user_id } }" class="link" data-testid="llm-acc-usage-link">用量</router-link>
        </template>
      </el-table-column>
    </el-table>
    <div class="pager-row">
      <span class="muted" data-testid="llm-acc-total">共 {{ list.total.value }} 个账户</span>
      <el-pagination v-model:current-page="list.page.value" v-model:page-size="list.size.value" :total="list.total.value" layout="prev, pager, next" data-testid="llm-acc-pager" />
    </div>

    <el-dialog v-model="limits.visible" title="编辑限额" width="460" align-center data-testid="llm-limits-dialog">
      <p class="muted">用户 {{ limits.target?.phone }}（ID {{ limits.target?.user_id }}）。留空 = 恢复平台默认值。</p>
      <el-form label-position="top" @submit.prevent="submitLimits">
        <el-form-item v-for="k in LIMIT_KEYS" :key="k" :label="LIMIT_LABEL[k]">
          <el-input
            v-model="limits.form[k]"
            inputmode="numeric"
            clearable
            :placeholder="`留空 = 默认 ${fmtCount(LIMIT_DEFAULTS[k])}`"
            :data-testid="`llm-limit-${k}`"
          />
          <div v-if="limits.errors[k]" class="err" :data-testid="`llm-limit-${k}-error`">{{ limits.errors[k] }}</div>
          <div v-else class="hint">{{ LIMIT_BOUNDS[k][0] }}～{{ LIMIT_BOUNDS[k][1].toLocaleString('en-US') }}</div>
        </el-form-item>
        <el-button type="primary" native-type="submit" :loading="limits.busy" data-testid="llm-limits-submit">保存</el-button>
      </el-form>
    </el-dialog>

    <el-dialog v-model="block.visible" title="停用 Token 服务" width="440" align-center data-testid="llm-block-dialog">
      <p class="muted">停用后该用户的所有 Key 立即无法调用，直到解除停用。</p>
      <el-form label-position="top" @submit.prevent="submitBlock">
        <el-form-item label="原因（必填，最多 200 字）"><el-input v-model="block.reason" maxlength="200" data-testid="llm-block-reason" /></el-form-item>
        <p v-if="block.error" class="err" data-testid="llm-block-error">{{ block.error }}</p>
        <el-button type="danger" native-type="submit" :loading="block.busy" data-testid="llm-block-submit">确定停用</el-button>
      </el-form>
    </el-dialog>
    <el-dialog v-model="unblock.visible" title="解除停用" width="420" align-center data-testid="llm-unblock-dialog">
      <p>恢复用户 {{ unblock.target?.phone }} 的 Token 服务？停用原因将被清除。</p>
      <template #footer>
        <el-button data-testid="llm-unblock-cancel" @click="unblock.visible = false">取消</el-button>
        <el-button type="primary" :loading="unblock.busy" data-testid="llm-unblock-ok" @click="submitUnblock">确定</el-button>
      </template>
    </el-dialog>

    <el-dialog v-model="keyOff.visible" title="停用 Key" width="440" align-center data-testid="llm-key-disable-dialog">
      <p class="muted">停用「{{ keyOff.target?.name }}」（{{ keyOff.target?.key_hint }}）。用户可自行重新启用；要强制停用请停用该账户的 Token 服务。</p>
      <el-form label-position="top" @submit.prevent="submitKeyOff">
        <el-form-item label="原因（必填，最多 200 字）"><el-input v-model="keyOff.reason" maxlength="200" data-testid="llm-key-disable-reason" /></el-form-item>
        <p v-if="keyOff.error" class="err" data-testid="llm-key-disable-error">{{ keyOff.error }}</p>
        <el-button type="danger" native-type="submit" :loading="keyOff.busy" data-testid="llm-key-disable-submit">确定停用</el-button>
      </el-form>
    </el-dialog>
  </div>
</template>

<style scoped>
.filters { display: flex; flex-wrap: wrap; gap: 8px 16px; align-items: center; margin-bottom: 12px; }
.error { margin-bottom: 12px; }
.pager-row { display: flex; justify-content: space-between; align-items: center; margin-top: 12px; }
.muted { color: #909399; font-size: 13px; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.bad { color: var(--el-color-danger); }
.err { color: var(--el-color-danger); font-size: 13px; }
.limits { display: flex; flex-wrap: wrap; gap: 4px 12px; font-size: 13px; }
.keys { padding: 4px 16px 12px 48px; }
.hint { color: #909399; font-size: 12px; line-height: 1.4; }
.link { margin-left: 8px; font-size: 12px; }
</style>
