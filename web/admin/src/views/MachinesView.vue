<script setup lang="ts">
import { fmtTime, isApiError, labelOf, machineStateLabel, type AdminProduct, type ImportError, type Listed, type Machine, type Paged, type PDU } from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, onMounted, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { api } from '../api'
import { usePaged } from '../composables/usePaged'
import { downloadText, importBody, readCSVFile, templateCSV } from '../logic/csv'
import { notifyError } from '../logic/notify'

const router = useRouter()
const filter = reactive({ state: '', online: '', rack: '' })
const list = usePaged<Machine>((page, size) =>
  api.get<Paged<Machine>>('/machines', { state: filter.state, online: filter.online, rack: filter.rack.trim(), page, page_size: size }),
)
const products = ref<AdminProduct[]>([])
const pdus = ref<PDU[]>([])
const productName = computed(() => new Map(products.value.map((p) => [p.id, p.name])))
const pduName = computed(() => new Map(pdus.value.map((p) => [p.id, p.name])))
const stateType = (s: string) => ({ idle: 'success', allocated: 'primary', maintenance: 'warning', faulty: 'danger' })[s] ?? 'info'

/** 商品 / PDU 只用于显示名称与登记下拉：失败提示一次，列表照常显示（名称退回 ID）。 */
async function loadOptions() {
  try {
    const [p, d] = await Promise.all([api.get<Listed<AdminProduct>>('/products'), api.get<Listed<PDU>>('/pdus')])
    products.value = p.items
    pdus.value = d.items
  } catch (e) {
    notifyError(e)
  }
}
onMounted(() => {
  void list.load()
  void loadOptions()
})
function resetFilter() {
  Object.assign(filter, { state: '', online: '', rack: '' })
  list.reset()
}

// ---- 登记 ----
const registerVisible = ref(false)
const form = reactive<{ serial: string; rack: string; slot: string; lan_ip: string; product_id: number | null; pdu_id: number | null; pdu_outlet: number | null }>({
  serial: '', rack: '', slot: '', lan_ip: '', product_id: null, pdu_id: null, pdu_outlet: null,
})
const registering = ref(false)
function openRegister() {
  Object.assign(form, { serial: '', rack: '', slot: '', lan_ip: '', product_id: products.value[0]?.id ?? null, pdu_id: null, pdu_outlet: null })
  registerVisible.value = true
}
async function register() {
  if (registering.value) return // 同步守卫：连点只发一次
  registering.value = true
  try {
    const body: Record<string, unknown> = { serial: form.serial.trim(), rack: form.rack.trim(), slot: form.slot.trim(), lan_ip: form.lan_ip.trim(), product_id: form.product_id }
    if (form.pdu_id != null) body.pdu_id = form.pdu_id
    if (form.pdu_outlet != null) body.pdu_outlet = form.pdu_outlet
    await api.post('/machines', body)
    ElMessage.success('机器已登记')
    registerVisible.value = false
    list.reset()
  } catch (e) {
    notifyError(e) // duplicate_serial / pdu_outlet_taken / bad_request 原文；弹窗保留便于修改
  } finally {
    registering.value = false
  }
}

// ---- CSV 导入 ----
const importVisible = ref(false)
const importText = ref('')
const importName = ref('')
const fileError = ref('')
const importErrors = ref<ImportError[]>([])
const importing = ref(false)
function openImport() {
  importText.value = ''
  importName.value = ''
  fileError.value = ''
  importErrors.value = []
  importVisible.value = true
}
async function pickFile(ev: Event) {
  const input = ev.target as HTMLInputElement
  const file = input.files?.[0]
  importText.value = ''
  importName.value = ''
  fileError.value = ''
  importErrors.value = []
  if (!file) return
  try {
    const text = await readCSVFile(file)
    importBody(text) // 提交前就按实际请求体大小校验
    importText.value = text
    importName.value = file.name
  } catch (e) {
    fileError.value = e instanceof Error ? e.message : '读取文件失败'
  } finally {
    // 读完即清空：管理员按逐行错误改好后重新选择同一个文件时，浏览器才会再次触发 change，读到新内容
    input.value = ''
  }
}
async function doImport() {
  if (importing.value || !importText.value) return
  importing.value = true
  try {
    const out = await api.post<Listed<Machine>>('/machines/import', importBody(importText.value))
    ElMessage.success(`成功导入 ${out.items.length} 台机器`)
    importVisible.value = false
    list.reset()
  } catch (e) {
    // 逐行错误在错误响应体的 errors 字段里，ApiError.details 保留了完整响应体（不重放请求）
    importErrors.value = isApiError(e) && e.code === 'import_invalid' && Array.isArray(e.details.errors) ? (e.details.errors as ImportError[]) : []
    notifyError(e)
  } finally {
    importing.value = false
  }
}
</script>

<template>
  <el-card>
    <el-form inline class="filters" @submit.prevent="list.reset()">
      <el-form-item label="状态">
        <el-select v-model="filter.state" clearable placeholder="全部" data-testid="machines-filter-state" style="width: 130px">
          <el-option v-for="(label, v) in machineStateLabel" :key="v" :label="label" :value="v" />
        </el-select>
      </el-form-item>
      <el-form-item label="在线">
        <el-select v-model="filter.online" clearable placeholder="全部" data-testid="machines-filter-online" style="width: 100px">
          <el-option label="是" value="true" /><el-option label="否" value="false" />
        </el-select>
      </el-form-item>
      <el-form-item label="机柜"><el-input v-model="filter.rack" clearable data-testid="machines-filter-rack" style="width: 140px" /></el-form-item>
      <el-button type="primary" native-type="submit" data-testid="machines-search">查询</el-button>
      <el-button data-testid="machines-reset" @click="resetFilter">重置</el-button>
      <span class="spacer" />
      <el-button data-testid="machines-import-btn" @click="openImport">CSV 导入</el-button>
      <el-button type="primary" data-testid="machines-register-btn" @click="openRegister">登记机器</el-button>
    </el-form>

    <el-alert v-if="list.error.value" type="error" :title="list.error.value" show-icon :closable="false" class="error" data-testid="machines-error">
      <el-button size="small" :loading="list.loading.value" data-testid="machines-retry" @click="list.load()">重试</el-button>
    </el-alert>
    <el-table :data="list.items.value" v-loading="list.loading.value" row-key="id" class="clickable" @row-click="(row: Machine) => router.push(`/machines/${row.id}`)">
      <el-table-column label="ID" width="80"><template #default="{ row }"><span data-testid="machine-id">{{ row.id }}</span></template></el-table-column>
      <el-table-column label="序列号" min-width="160"><template #default="{ row }"><span class="mono" data-testid="machine-serial">{{ row.serial }}</span></template></el-table-column>
      <el-table-column label="机柜 / 槽位" width="140"><template #default="{ row }"><span data-testid="machine-rack">{{ row.rack }} / {{ row.slot }}</span></template></el-table-column>
      <el-table-column label="内网 IP" width="140"><template #default="{ row }"><span class="mono" data-testid="machine-ip">{{ row.lan_ip }}</span></template></el-table-column>
      <el-table-column label="商品" min-width="140"><template #default="{ row }"><span data-testid="machine-product">{{ productName.get(row.product_id) ?? row.product_id }}</span></template></el-table-column>
      <el-table-column label="状态" width="100">
        <template #default="{ row }"><el-tag :type="stateType(row.state) as 'success'" data-testid="machine-state">{{ labelOf(machineStateLabel, row.state) }}</el-tag></template>
      </el-table-column>
      <el-table-column label="在线" width="210">
        <template #default="{ row }">
          <span data-testid="machine-online" :data-online="String(row.agent_online)"><i class="dot" :class="row.agent_online ? 'on' : 'off'" />{{ row.agent_online ? '在线' : '离线' }} <small class="muted">{{ fmtTime(row.last_seen_at) || '从未心跳' }}</small></span>
        </template>
      </el-table-column>
      <el-table-column label="PDU" width="180"><template #default="{ row }"><span data-testid="machine-pdu">{{ row.pdu_id ? `${pduName.get(row.pdu_id) ?? row.pdu_id} #${row.pdu_outlet}` : '—' }}</span></template></el-table-column>
    </el-table>
    <div class="pager-row">
      <span class="muted" data-testid="machines-total">共 {{ list.total.value }} 台</span>
      <el-pagination v-model:current-page="list.page.value" v-model:page-size="list.size.value" :page-sizes="[20, 50, 100]" :total="list.total.value" layout="sizes, prev, pager, next" data-testid="machines-pager" />
    </div>

    <el-dialog v-model="registerVisible" title="登记机器" width="520" align-center destroy-on-close data-testid="register-dialog">
      <el-form label-position="top" @submit.prevent="register">
        <el-form-item label="序列号"><el-input v-model="form.serial" data-testid="register-serial" /></el-form-item>
        <div class="two">
          <el-form-item label="机柜"><el-input v-model="form.rack" data-testid="register-rack" /></el-form-item>
          <el-form-item label="槽位"><el-input v-model="form.slot" data-testid="register-slot" /></el-form-item>
        </div>
        <el-form-item label="内网 IP"><el-input v-model="form.lan_ip" data-testid="register-ip" /></el-form-item>
        <el-form-item label="商品">
          <el-select v-model="form.product_id" data-testid="register-product"><el-option v-for="p in products" :key="p.id" :label="p.name" :value="p.id" /></el-select>
        </el-form-item>
        <div class="two">
          <el-form-item label="PDU（可空）">
            <el-select v-model="form.pdu_id" clearable placeholder="不配置" data-testid="register-pdu"><el-option v-for="p in pdus" :key="p.id" :label="p.name" :value="p.id" /></el-select>
          </el-form-item>
          <el-form-item label="插座号（1～256）"><el-input-number v-model="form.pdu_outlet" :min="1" :max="256" :controls="false" data-testid="register-outlet" /></el-form-item>
        </div>
        <el-button type="primary" native-type="submit" :loading="registering" data-testid="register-submit">登记</el-button>
      </el-form>
    </el-dialog>

    <el-dialog v-model="importVisible" title="CSV 导入机器" width="640" align-center destroy-on-close data-testid="import-dialog">
      <p class="muted">表头固定：<code>serial,rack,slot,lan_ip,product_code,pdu_name,pdu_outlet</code>；文件不超过 1MB；任一行有错整批不导入。</p>
      <div class="import-row">
        <input type="file" accept=".csv,text/csv" data-testid="import-file" @change="pickFile" />
        <el-button link type="primary" data-testid="import-template" @click="downloadText('machines-template.csv', templateCSV())">下载模板</el-button>
      </div>
      <el-alert v-if="fileError" type="error" :title="fileError" show-icon :closable="false" data-testid="import-file-error" />
      <p v-if="importName" class="muted" data-testid="import-file-name">已选择：{{ importName }}</p>
      <el-table v-if="importErrors.length" :data="importErrors" size="small" data-testid="import-errors">
        <el-table-column label="行号" width="80"><template #default="{ row }"><span data-testid="import-error-line">{{ row.line }}</span></template></el-table-column>
        <el-table-column label="原因"><template #default="{ row }"><span data-testid="import-error-message">{{ row.message }}</span></template></el-table-column>
      </el-table>
      <template #footer>
        <el-button @click="importVisible = false">取消</el-button>
        <el-button type="primary" :disabled="!importText" :loading="importing" data-testid="import-submit" @click="doImport">导入</el-button>
      </template>
    </el-dialog>
  </el-card>
</template>

<style scoped>
.filters { display: flex; flex-wrap: wrap; gap: 8px 16px; align-items: center; margin-bottom: 12px; }
.spacer { flex: 1; }
.error { margin-bottom: 12px; }
.clickable :deep(tbody tr) { cursor: pointer; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.muted { color: #909399; }
.dot { display: inline-block; width: 8px; height: 8px; border-radius: 50%; margin-right: 6px; }
.dot.on { background: var(--el-color-success); }
.dot.off { background: #c0c4cc; }
.pager-row { display: flex; justify-content: space-between; align-items: center; margin-top: 12px; }
.two { display: grid; grid-template-columns: 1fr 1fr; gap: 12px; }
.import-row { display: flex; align-items: center; gap: 12px; margin: 8px 0; }
</style>
