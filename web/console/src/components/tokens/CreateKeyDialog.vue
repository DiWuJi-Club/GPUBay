<script setup lang="ts">
import { isApiError, messageOf, type LLMKey, type LLMKeyCreated, type LLMModelPublic } from '@spark/shared'
import { ElMessage, ElMessageBox } from 'element-plus'
import 'element-plus/theme-chalk/el-message-box.css' // 显式引入的 ElMessageBox 不经自动引入插件，样式自己带上
import { computed, reactive, ref, watch } from 'vue'
import { api } from '../../api'
import AppDialog from '../AppDialog.vue'
import { copyText } from '../../logic/clipboard'
import { notifyError } from '../../logic/notify'
import { beijingToday, emptyKeyForm, keyFormOf, KEY_NAME_MAX, validateKeyInput, type KeyFormErrors } from '../../logic/tokens'

// 创建 / 编辑 API Key。editing 为 null 时是创建：成功后切换到「只显示一次」面板展示明文 Key，关闭前二次确认。
const props = defineProps<{ models: LLMModelPublic[]; editing: LLMKey | null }>()
const emit = defineEmits<{ saved: [] }>()
const visible = defineModel<boolean>({ required: true })

const form = reactive(emptyKeyForm())
const errors = ref<KeyFormErrors>({})
const serverError = ref('')
const submitting = ref(false)
const plain = ref('') // 明文 Key：只在本次对话框里保留，关闭即清空
const phase = computed(() => (plain.value ? 'secret' : 'form'))
const isEdit = computed(() => props.editing != null)
const title = computed(() => (phase.value === 'secret' ? 'API Key 已创建' : isEdit.value ? '编辑 API Key' : '创建 API Key'))

// 编辑时保留 Key 上已有、但当前已不在线的模型，免得一打开编辑就被悄悄去掉
const modelOptions = computed(() => {
  const slugs = props.models.map((m) => ({ value: m.slug, label: m.name === m.slug ? m.slug : `${m.name}（${m.slug}）` }))
  const extra = (props.editing?.model_slugs ?? []).filter((s) => !props.models.some((m) => m.slug === s)).map((s) => ({ value: s, label: `${s}（已下线）` }))
  return [...slugs, ...extra]
})

watch(
  visible,
  (v) => {
    if (!v) {
      plain.value = ''
      return
    }
    Object.assign(form, props.editing ? keyFormOf(props.editing) : emptyKeyForm())
    errors.value = {}
    serverError.value = ''
    plain.value = ''
  },
  { immediate: true },
)

// 日期选择器给的是本地零点的 Date：按本地年月日与北京时间的今天比较
const localYmd = (d: Date) => `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`
const disabledDate = (d: Date) => localYmd(d) < beijingToday(new Date())

async function submit() {
  if (submitting.value) return
  const r = validateKeyInput(form, new Date(), isEdit.value ? 'edit' : 'create')
  serverError.value = ''
  if ('errors' in r) {
    errors.value = r.errors
    return
  }
  errors.value = {}
  submitting.value = true
  try {
    if (props.editing) {
      await api.put<LLMKey>(`/llm/keys/${props.editing.id}`, r.body)
      ElMessage.success('已保存')
      emit('saved')
      visible.value = false
    } else {
      const created = await api.post<LLMKeyCreated>('/llm/keys', r.body)
      plain.value = created.key
      emit('saved')
    }
  } catch (e) {
    // 业务错误（llm_key_limit「最多创建 20 个 API Key」、llm_key_invalid、kyc_required…）在对话框里直接显示后端文案
    if (isApiError(e) && e.code !== 'unauthorized' && e.code !== 'cancelled') serverError.value = messageOf(e)
    else notifyError(e)
  } finally {
    submitting.value = false
  }
}

/** 明文面板的关闭：二次确认后才关（右上角 / Esc / 遮罩 / 「我已保存」按钮都走这里）。 */
async function confirmCloseSecret(): Promise<boolean> {
  try {
    await ElMessageBox.confirm('关闭后将无法再次查看这个 Key。确认已经复制并妥善保存？', '确认关闭', {
      type: 'warning',
      confirmButtonText: '已保存，关闭',
      cancelButtonText: '返回',
    })
    return true
  } catch {
    return false
  }
}
async function closeSecret() {
  if (await confirmCloseSecret()) visible.value = false
}
function beforeClose(done: () => void) {
  if (phase.value !== 'secret') return done()
  void confirmCloseSecret().then((ok) => ok && done())
}
</script>

<template>
  <AppDialog v-model="visible" kind="form" :title="title" :before-close="beforeClose" data-testid="tokens-key-dialog">
    <template v-if="phase === 'secret'">
      <el-alert type="warning" show-icon :closable="false" title="关闭后不再显示，请立即复制保存" data-testid="tokens-key-warning" />
      <div class="secret">
        <code class="mono" data-testid="tokens-key-plain">{{ plain }}</code>
        <el-button type="primary" data-testid="tokens-key-copy" @click="copyText(plain)">复制</el-button>
      </div>
      <p class="muted">平台只保存 Key 的哈希，无法找回明文；丢失后请撤销并重新创建。</p>
    </template>
    <el-form v-else label-position="top" @submit.prevent="submit">
      <el-form-item label="名称" required :error="errors.name">
        <el-input v-model="form.name" :maxlength="KEY_NAME_MAX" show-word-limit placeholder="例如：生产环境" data-testid="tokens-key-name" />
      </el-form-item>
      <el-form-item label="允许的模型" :error="errors.modelSlugs">
        <el-select v-model="form.modelSlugs" multiple clearable placeholder="不选 = 全部在线模型" style="width: 100%" data-testid="tokens-key-models">
          <el-option v-for="o in modelOptions" :key="o.value" :label="o.label" :value="o.value" :data-testid="`tokens-key-model-option-${o.value}`" />
        </el-select>
      </el-form-item>
      <el-form-item label="月消费上限（元）" :error="errors.monthlyCap">
        <el-input v-model="form.monthlyCap" inputmode="decimal" placeholder="留空 = 不限" data-testid="tokens-key-cap">
          <template #prefix>¥</template>
        </el-input>
      </el-form-item>
      <el-form-item v-if="!isEdit" label="到期日期" :error="errors.expiresOn">
        <el-date-picker
          v-model="form.expiresOn" type="date" value-format="YYYY-MM-DD" placeholder="留空 = 永不过期" :disabled-date="disabledDate" clearable
          style="width: 100%" data-testid="tokens-key-expires"
        />
      </el-form-item>
      <p v-if="!isEdit" class="muted">到期日当天 23:59（北京时间）后失效；到期日创建后不能修改。</p>
      <p v-if="serverError" class="danger" data-testid="tokens-key-error">{{ serverError }}</p>
    </el-form>
    <template #footer>
      <el-button v-if="phase === 'secret'" type="primary" data-testid="tokens-key-done" @click="closeSecret">我已保存，关闭</el-button>
      <template v-else>
        <el-button data-testid="tokens-key-cancel" @click="visible = false">取消</el-button>
        <el-button type="primary" :loading="submitting" data-testid="tokens-key-submit" @click="submit">{{ isEdit ? '保存' : '创建' }}</el-button>
      </template>
    </template>
  </AppDialog>
</template>

<style scoped>
.secret { display: flex; align-items: center; gap: 12px; margin: 16px 0 8px; }
.secret code { flex: 1; min-width: 0; background: #f4f4f5; padding: 8px 10px; border-radius: 4px; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; word-break: break-all; font-size: 13px; }
.muted { color: #909399; font-size: 12px; margin: 4px 0 0; }
.danger { color: var(--el-color-danger); font-size: 13px; }
</style>
