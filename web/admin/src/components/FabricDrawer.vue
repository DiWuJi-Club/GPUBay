<script setup lang="ts">
import { fabricIsolationLabel, fabricKindLabel, type Fabric, type FabricKind } from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, ref, watch } from 'vue'
import { api } from '../api'
import { emptyFabricForm, fabricFormOf, isolationOptions, normalizeFabricForm, validateFabricForm, type FabricForm } from '../logic/clusters'
import { notifyError } from '../logic/notify'

/**
 * 集群网络新建 / 编辑抽屉（规格 §3.1、§11）：kind 决定可选的 isolation（lan → 三层路由、不能开 RDMA；cx7 → 直连 / VLAN），
 * VLAN 隔离时填范围。前端预校验与 fleet 一致，服务器的 fabric_invalid / fabric_in_use / fabric_full 文案原样提示。
 */
const props = defineProps<{ open: boolean; fabric: Fabric | null }>()
const emit = defineEmits<{ close: []; saved: [] }>()

const form = ref<FabricForm>(emptyFabricForm())
const errors = ref<Partial<Record<keyof FabricForm, string>>>({})
const busy = ref(false)
const isolations = computed(() => isolationOptions(form.value.kind))
/** 第一条校验错误（表单项下方也会标出） */
const firstError = computed(() => Object.values(errors.value).find(Boolean) ?? '')

watch(
  () => props.open,
  (open) => {
    if (!open) return
    form.value = props.fabric ? fabricFormOf(props.fabric) : emptyFabricForm()
    errors.value = {}
  },
  { immediate: true },
)

function setKind(kind: FabricKind) {
  form.value = normalizeFabricForm({ ...form.value, kind })
}
function setIsolation(v: string) {
  form.value = normalizeFabricForm({ ...form.value, isolation: v as FabricForm['isolation'] })
}

async function save() {
  if (busy.value) return
  const out = validateFabricForm(form.value)
  if (!out.ok) {
    errors.value = out.errors
    return
  }
  errors.value = {}
  busy.value = true
  try {
    if (props.fabric) await api.put(`/fabrics/${props.fabric.id}`, out.input)
    else await api.post('/fabrics', out.input)
    ElMessage.success(props.fabric ? '已保存' : '集群网络已创建')
    emit('saved')
  } catch (e) {
    notifyError(e) // fabric_invalid（按字段的文案）/ fabric_in_use / fabric_full 原文；抽屉与输入保留
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <el-drawer :model-value="open" :title="fabric ? `编辑集群网络：${fabric.name}` : '新建集群网络'" size="520px" destroy-on-close data-testid="fabric-drawer" @close="emit('close')">
    <el-form label-position="top" @submit.prevent="save">
      <el-form-item label="名称（1～32 字）" :error="errors.name" required>
        <el-input v-model="form.name" maxlength="32" data-testid="fabric-drawer-name" />
      </el-form-item>
      <el-form-item label="类型" :error="errors.kind">
        <el-radio-group :model-value="form.kind" data-testid="fabric-drawer-kind" @update:model-value="(v) => setKind(v as FabricKind)">
          <el-radio-button v-for="k in ['lan', 'cx7'] as FabricKind[]" :key="k" :value="k">{{ fabricKindLabel[k] }}</el-radio-button>
        </el-radio-group>
        <div class="help">{{ form.kind === 'lan' ? '机器的管理网卡（或指定网卡）所在的普通以太网：容器经宿主机三层路由互通，只有同一网段的机器能组成集群。' : 'ConnectX-7 高速网卡：两台 QSFP 直连，或经交换机 trunk 用 VLAN 隔离；可开 RDMA。' }}</div>
      </el-form-item>
      <el-form-item label="隔离方式" :error="errors.isolation">
        <el-radio-group :model-value="form.isolation" data-testid="fabric-drawer-isolation" @update:model-value="(v) => setIsolation(String(v))">
          <el-radio-button v-for="i in isolations" :key="i" :value="i">{{ fabricIsolationLabel[i] }}</el-radio-button>
        </el-radio-group>
        <div v-if="form.isolation === 'direct'" class="help">直连网络最多 2 台机器。</div>
      </el-form-item>
      <div v-if="form.isolation === 'vlan'" class="two">
        <el-form-item label="VLAN 起始（2～4094）" :error="errors.vlan_min" required>
          <el-input-number v-model="form.vlan_min" :min="2" :max="4094" :precision="0" :controls="false" data-testid="fabric-drawer-vlan-min" />
        </el-form-item>
        <el-form-item label="VLAN 结束" :error="errors.vlan_max" required>
          <el-input-number v-model="form.vlan_max" :min="2" :max="4094" :precision="0" :controls="false" data-testid="fabric-drawer-vlan-max" />
        </el-form-item>
      </div>
      <el-form-item label="RDMA" :error="errors.rdma">
        <el-switch v-model="form.rdma" :disabled="form.kind === 'lan'" data-testid="fabric-drawer-rdma" />
        <div class="help">{{ form.kind === 'lan' ? '只有 ConnectX-7 网络支持 RDMA' : '开启后，网卡 RDMA 可用的机器组成的集群走 RDMA（NCCL IB）；不可用时自动退回以太网' }}</div>
      </el-form-item>
      <el-form-item label="MTU（0 = 继承网卡，否则 1280～9216）" :error="errors.mtu">
        <el-input-number v-model="form.mtu" :min="0" :max="9216" :precision="0" :controls="false" data-testid="fabric-drawer-mtu" />
      </el-form-item>
      <el-form-item label="说明（≤ 200 字）" :error="errors.description">
        <el-input v-model="form.description" type="textarea" :rows="2" maxlength="200" data-testid="fabric-drawer-description" />
      </el-form-item>
      <p v-if="firstError" class="error" data-testid="fabric-drawer-error">{{ firstError }}</p>
    </el-form>
    <template #footer>
      <el-button data-testid="fabric-drawer-cancel" @click="emit('close')">取消</el-button>
      <el-button type="primary" :loading="busy" data-testid="fabric-drawer-save" @click="save">保存</el-button>
    </template>
  </el-drawer>
</template>

<style scoped>
.two { display: grid; grid-template-columns: 1fr 1fr; gap: 0 16px; }
.error { color: var(--el-color-danger); font-size: 13px; margin: 0; }
.help { color: #909399; font-size: 12px; line-height: 1.5; width: 100%; margin-top: 4px; }
</style>
