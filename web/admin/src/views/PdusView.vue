<script setup lang="ts">
import { fmtTime, messageOf, type Listed, type PDU, type PDUInput } from '@spark/shared'
import { ElMessage } from 'element-plus'
import { onMounted, reactive, ref } from 'vue'
import { api } from '../api'
import { notifyError } from '../logic/notify'

const items = ref<PDU[]>([])
const loading = ref(false)
const error = ref('')
const visible = ref(false)
const editing = ref<PDU | null>(null)
const busy = ref(false)
const blank = (): PDUInput => ({ name: '', driver: 'snmp', address: '', community: '', oid_prefix: '', cycle_value: 3, on_value: 1 })
const form = reactive<PDUInput>(blank())

/** 加载失败清空表格并显示页内错误与重试（不自动重试）。 */
async function load() {
  loading.value = true
  try {
    items.value = (await api.get<Listed<PDU>>('/pdus')).items
    error.value = ''
  } catch (e) {
    notifyError(e)
    items.value = []
    error.value = messageOf(e)
  } finally {
    loading.value = false
  }
}
onMounted(load)

function openCreate() {
  editing.value = null
  Object.assign(form, blank())
  visible.value = true
}
function openEdit(p: PDU) {
  editing.value = p
  // community 从不下发：编辑时永远从空开始，留空表示不修改
  Object.assign(form, { name: p.name, driver: p.driver, address: p.address, community: '', oid_prefix: p.oid_prefix, cycle_value: p.cycle_value, on_value: p.on_value })
  visible.value = true
}

/** 请求体：community 只在填写了且驱动为 snmp 时发送（编辑时留空 = 保持原值；mock 不需要）。 */
function bodyOf(): Record<string, unknown> {
  const { community, ...rest } = form
  return form.driver === 'snmp' && community !== '' ? { ...rest, community } : rest
}
async function submit() {
  if (busy.value) return // 同步守卫：连点只发一次
  busy.value = true
  try {
    if (editing.value) await api.put(`/pdus/${editing.value.id}`, bodyOf())
    else await api.post('/pdus', bodyOf())
    ElMessage.success(editing.value ? 'PDU 已更新' : 'PDU 已添加')
    visible.value = false
    form.community = ''
    await load()
  } catch (e) {
    notifyError(e) // duplicate_name / 「请填写 SNMP community」/「修改 PDU 地址时请重新填写 SNMP community」原文
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <el-card>
    <div class="toolbar"><el-button type="primary" data-testid="pdus-create-btn" @click="openCreate">新增 PDU</el-button></div>
    <el-alert v-if="error" type="error" :title="error" show-icon :closable="false" class="error" data-testid="pdus-error">
      <el-button size="small" :loading="loading" data-testid="pdus-retry" @click="load">重试</el-button>
    </el-alert>
    <el-table :data="items" v-loading="loading" row-key="id">
      <el-table-column label="名称" min-width="140"><template #default="{ row }"><span data-testid="pdu-name">{{ row.name }}</span></template></el-table-column>
      <el-table-column label="驱动" width="90"><template #default="{ row }"><span data-testid="pdu-driver">{{ row.driver }}</span></template></el-table-column>
      <el-table-column label="地址" min-width="160"><template #default="{ row }"><span class="mono" data-testid="pdu-address">{{ row.address }}</span></template></el-table-column>
      <el-table-column label="community" width="110"><template #default="{ row }"><el-tag v-if="row.has_community" size="small" data-testid="pdu-has-community">已设置</el-tag><span v-else class="muted">—</span></template></el-table-column>
      <el-table-column label="OID 前缀" min-width="200"><template #default="{ row }"><span class="mono">{{ row.oid_prefix }}</span></template></el-table-column>
      <el-table-column label="创建时间" width="160"><template #default="{ row }">{{ fmtTime(row.created_at) }}</template></el-table-column>
      <el-table-column label="操作" width="90"><template #default="{ row }"><el-button size="small" data-testid="pdu-edit" @click="openEdit(row as PDU)">编辑</el-button></template></el-table-column>
    </el-table>

    <el-dialog v-model="visible" :title="editing ? '编辑 PDU' : '新增 PDU'" width="520" align-center destroy-on-close data-testid="pdu-dialog">
      <el-form label-position="top" @submit.prevent="submit">
        <el-form-item label="名称"><el-input v-model="form.name" data-testid="pdu-form-name" /></el-form-item>
        <el-form-item label="驱动">
          <el-radio-group v-model="form.driver">
            <el-radio-button value="snmp" data-testid="pdu-form-driver-snmp">snmp</el-radio-button>
            <el-radio-button value="mock" data-testid="pdu-form-driver-mock">mock（开发）</el-radio-button>
          </el-radio-group>
        </el-form-item>
        <el-form-item label="地址（host 或 host:port）"><el-input v-model="form.address" data-testid="pdu-form-address" /></el-form-item>
        <template v-if="form.driver === 'snmp'">
          <el-form-item label="SNMP community">
            <el-input v-model="form.community" type="password" show-password autocomplete="new-password" data-testid="pdu-form-community" :placeholder="editing ? '留空表示不修改；修改了地址必须重新填写' : ''" />
            <el-tag v-if="editing?.has_community" size="small" class="set-tag" data-testid="pdu-has-community">已设置</el-tag>
          </el-form-item>
          <el-form-item label="OID 前缀"><el-input v-model="form.oid_prefix" data-testid="pdu-form-oid" placeholder="例如 1.3.6.1.4.1.318.1.1.4.4.2.1.3" /></el-form-item>
          <div class="two">
            <el-form-item label="断电重启取值"><el-input-number v-model="form.cycle_value" :controls="false" data-testid="pdu-form-cycle" /></el-form-item>
            <el-form-item label="上电取值"><el-input-number v-model="form.on_value" :controls="false" data-testid="pdu-form-on" /></el-form-item>
          </div>
        </template>
        <el-button type="primary" native-type="submit" :loading="busy" data-testid="pdu-form-submit">保存</el-button>
      </el-form>
    </el-dialog>
  </el-card>
</template>

<style scoped>
.toolbar { margin-bottom: 12px; display: flex; justify-content: flex-end; }
.error { margin-bottom: 12px; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.muted { color: #909399; }
.set-tag { margin-top: 4px; }
.two { display: grid; grid-template-columns: 1fr 1fr; gap: 12px; }
</style>
