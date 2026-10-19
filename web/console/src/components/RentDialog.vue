<script setup lang="ts">
import {
  durationLabel, fmtTime, fmtYuan, isApiError, labelOf, useIdempotencyKey,
  type Availability, type BookingResult, type CalendarDay, type CalendarPeriod, type Duration, type Group, type GroupMachine, type Image, type Instance, type Listed, type Order, type Product,
} from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, reactive, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api } from '../api'
import { useGuide } from '../composables/useGuide'
import { cstDay, describeDuration, earliestStart, hourOptions, isLegacyImmediate, latestStart, MAX_COUNT, MAX_QUANTITY, startAtISO, totalPrice } from '../logic/booking'
import { notifyError } from '../logic/notify'
import { refreshBalance, session } from '../store/session'
import AppDialog from './AppDialog.vue'
import GuideDialog from './GuideDialog.vue'

const props = defineProps<{ product: Product; images: Image[]; mode: 'immediate' | 'booking'; duration: Duration; imageId: number | null }>()
const visible = defineModel<boolean>({ required: true })
const emit = defineEmits<{ placed: [result: { instances: { id: string }[]; batchId: string | null }] }>()

const route = useRoute()
const router = useRouter()
const guide = useGuide()

// ---- 表单 ----
const form = reactive({
  groupId: null as number | null,
  count: 1,
  immediate: props.mode === 'immediate',
  day: '',
  hour: null as number | null,
  duration: props.duration,
  quantity: 1,
  imageId: props.imageId,
  machineId: null as number | null,
  autoRenew: false,
  autoRenewDuration: props.duration,
})
const step = ref<'form' | 'confirm'>(props.mode === 'immediate' ? 'confirm' : 'form')
const expanded = ref(props.mode === 'booking')
const groups = ref<Group[]>([])
const serverOffset = ref(0) // 服务器时间 − 浏览器时间：最早 / 最晚整点按服务器时间算
const nowServer = () => new Date(Date.now() + serverOffset.value)
const machines = ref<GroupMachine[]>([])
const machinePeriods = ref<CalendarPeriod[]>([])
const calendar = ref<CalendarDay[]>([])
const availability = ref<Availability | null>(null)
const submitting = ref(false)

const NO_GROUP = 0 // el-option 的 value 不能是 null：0 作为「不限机组」哨兵，提交时映射回 null
const groupSel = computed({ get: () => form.groupId ?? NO_GROUP, set: (v: number) => { form.groupId = v === NO_GROUP ? null : v } })
const unitPrice = computed(() => props.product.prices.find((p) => p.duration === form.duration)?.amount ?? null)
const quantityMax = computed(() => MAX_QUANTITY[form.duration])
const total = computed(() => (unitPrice.value == null ? null : totalPrice(unitPrice.value, form.quantity, form.count)))
const balance = computed(() => session.balance ?? 0)
const after = computed(() => (total.value == null ? null : balance.value - total.value))
const insufficient = computed(() => total.value != null && balance.value < total.value)
const imageName = computed(() => props.images.find((i) => i.id === form.imageId)?.name ?? '')
const groupName = computed(() => groups.value.find((g) => g.id === form.groupId)?.name ?? '不限机组')
const machineLabel = computed(() => machines.value.find((m) => m.id === form.machineId)?.label ?? '')
const startAt = computed(() => (form.immediate || !form.day || form.hour == null ? null : startAtISO(form.day, form.hour)))
const hours = computed(() => (form.day ? hourOptions(form.day, nowServer()) : []))
const dayMin = computed(() => cstDay(earliestStart(nowServer())))
const dayMax = computed(() => cstDay(latestStart(nowServer())))
const canSpecifyMachine = computed(() => form.count === 1 && form.groupId !== null)
const notEnough = computed(() => availability.value != null && availability.value.available < form.count)
const legacy = computed(() => isLegacyImmediate({ immediate: form.immediate, count: form.count, quantity: form.quantity, groupId: form.groupId, machineId: form.machineId, autoRenew: form.autoRenew }))
const key = useIdempotencyKey(() => [props.product.id, form.groupId, form.count, form.immediate, form.day, form.hour, form.duration, form.quantity, form.imageId, form.machineId, form.autoRenew, form.autoRenewDuration])

watch(() => form.duration, (d) => {
  if (form.quantity > MAX_QUANTITY[d]) form.quantity = MAX_QUANTITY[d]
  form.autoRenewDuration = d
})
watch(() => form.count, () => {
  if (form.count !== 1) form.machineId = null
})
watch(() => form.groupId, () => {
  form.machineId = null
  machines.value = []
  if (form.groupId !== null) void loadGroupDetails()
})
watch(() => form.immediate, (imm) => {
  if (!imm && !form.day) {
    const e = earliestStart(nowServer())
    form.day = cstDay(e)
    form.hour = hourOptions(form.day, nowServer())[0] ?? null
  }
})
watch(() => form.day, () => {
  const opts = hours.value
  if (form.hour == null || !opts.includes(form.hour)) form.hour = opts[0] ?? null
})
watch(() => form.machineId, (id) => {
  machinePeriods.value = []
  if (id !== null) void loadMachinePeriods(id)
})

async function loadGroups() {
  try {
    const out = await api.get<Listed<Group> & { now: string }>('/groups', { product_id: props.product.id })
    groups.value = out.items
    serverOffset.value = Date.parse(out.now) - Date.now()
    // 用户可能在 /groups 返回前就切到「指定时间」（当时按旧偏移算）：按服务器时间重新校验
    if (!form.immediate && (!form.day || form.hour == null || !hourOptions(form.day, nowServer()).includes(form.hour))) {
      const e = earliestStart(nowServer())
      form.day = cstDay(e)
      form.hour = hourOptions(form.day, nowServer())[0] ?? null
    }
  } catch (e) {
    notifyError(e)
  }
}
async function loadGroupDetails() {
  const gid = form.groupId
  if (gid === null) return
  try {
    const [m, c] = await Promise.all([api.get<Listed<GroupMachine>>(`/groups/${gid}/machines`), api.get<{ days: CalendarDay[] }>(`/groups/${gid}/calendar`, { days: 61 })])
    if (form.groupId !== gid) return
    machines.value = m.items
    calendar.value = c.days
  } catch (e) {
    notifyError(e)
  }
}
async function loadMachinePeriods(id: number) {
  try {
    const out = await api.get<{ periods: CalendarPeriod[] }>(`/machines/${id}/calendar`, { days: 61 })
    if (form.machineId === id) machinePeriods.value = out.periods
  } catch (e) {
    notifyError(e)
  }
}

// ---- 可用性：参数变化后 300ms 查一次，只采用最后一次 ----
let availSeq = 0
let availTimer: ReturnType<typeof setTimeout> | undefined
async function loadAvailability() {
  const mine = ++availSeq
  try {
    const out = await api.get<Availability>('/availability', {
      product_id: props.product.id, group_id: form.groupId ?? undefined, machine_id: form.machineId ?? undefined,
      start_at: startAt.value ?? undefined, duration: form.duration, quantity: form.quantity,
    })
    if (mine === availSeq) availability.value = out
  } catch (e) {
    if (mine === availSeq) availability.value = null
    if (!(isApiError(e) && e.code.startsWith('booking_'))) notifyError(e)
  }
}
watch([() => form.groupId, () => form.machineId, () => form.immediate, startAt, () => form.duration, () => form.quantity], () => {
  clearTimeout(availTimer)
  availTimer = setTimeout(() => void loadAvailability(), 300)
})

watch(visible, (v) => {
  if (!v) return
  step.value = props.mode === 'immediate' ? 'confirm' : 'form'
  expanded.value = props.mode === 'booking'
  Object.assign(form, { groupId: null, count: 1, immediate: props.mode === 'immediate', duration: props.duration, quantity: 1, imageId: props.imageId, machineId: null, autoRenew: false, autoRenewDuration: props.duration })
  availability.value = null
  form.day = ''
  form.hour = null
  void loadGroups() // 拿到服务器时间后再填默认开始时刻
  void loadAvailability()
}, { immediate: true })

function pickDay(d: CalendarDay) {
  if (d.free === 0) return
  form.day = d.date
}

function toConfirm() {
  if (!form.immediate && !startAt.value) return void ElMessage.warning('请选择开始时间')
  step.value = 'confirm'
}

async function submit() {
  if (submitting.value || form.imageId == null) return
  if (insufficient.value) {
    visible.value = false
    await router.push({ path: '/wallet/recharge', query: { redirect: route.fullPath } })
    return
  }
  submitting.value = true
  try {
    let placed: { instances: { id: string }[]; batchId: string | null }
    if (legacy.value) {
      const out = await api.post<{ order: Order; instance: Instance }>('/orders', { product_id: props.product.id, duration: form.duration, image_id: form.imageId }, { idempotencyKey: key.current() })
      placed = { instances: [out.instance], batchId: null }
    } else {
      const out = await api.post<BookingResult>('/bookings', {
        product_id: props.product.id, image_id: form.imageId, group_id: form.groupId, machine_id: form.machineId, count: form.count,
        duration: form.duration, quantity: form.quantity, start_at: startAt.value ?? undefined, auto_renew: form.autoRenew, auto_renew_duration: form.autoRenewDuration,
      }, { idempotencyKey: key.current() })
      placed = { instances: out.instances, batchId: out.batch?.id ?? null }
    }
    key.rotate()
    visible.value = false
    emit('placed', placed)
  } catch (e) {
    if (isApiError(e) && (e.code === 'no_capacity' || e.code === 'machine_unavailable')) {
      ElMessage.warning(e.message)
      step.value = 'form'
      void loadAvailability()
    } else if (isApiError(e) && e.code.startsWith('booking_')) {
      ElMessage.warning(e.message)
      step.value = 'form'
    } else {
      if (isApiError(e) && ['kyc_required', 'insufficient_balance', 'banned'].includes(e.code)) visible.value = false
      guide.handle(e)
    }
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <AppDialog v-model="visible" kind="form" :title="step === 'confirm' ? '确认下单' : '租用 / 预订'" :data-testid="step === 'confirm' ? 'market-confirm-dialog' : 'rent-dialog'">
    <template v-if="step === 'form'">
      <el-form label-position="top">
        <el-form-item label="机组">
          <el-select v-model="groupSel" data-testid="rent-group" placeholder="不限机组">
            <el-option :value="NO_GROUP" label="不限机组" />
            <el-option v-for="g in groups" :key="g.id" :value="g.id" :label="`${g.name}${g.region ? ' · ' + g.region : ''}（共 ${g.machine_count} · 现空闲 ${g.idle_now}）`" />
          </el-select>
        </el-form-item>
        <el-form-item label="数量（台）">
          <el-input-number v-model="form.count" :min="1" :max="MAX_COUNT" data-testid="rent-count" />
        </el-form-item>
        <el-form-item label="开始">
          <el-radio-group v-model="form.immediate">
            <el-radio-button :value="true" data-testid="rent-start-now">立即</el-radio-button>
            <el-radio-button :value="false" data-testid="rent-start-later">指定时间</el-radio-button>
          </el-radio-group>
          <div v-if="!form.immediate" class="row">
            <input v-model="form.day" type="date" :min="dayMin" :max="dayMax" class="el-input__inner native-date" data-testid="rent-day" />
            <el-select v-model="form.hour" data-testid="rent-hour" placeholder="整点" style="width: 110px">
              <el-option v-for="h in hours" :key="h" :value="h" :label="`${String(h).padStart(2, '0')}:00`" />
            </el-select>
            <span class="muted">北京时间整点；最早 2 小时后，最多提前 60 天</span>
          </div>
        </el-form-item>
        <el-form-item label="租期">
          <el-radio-group v-model="form.duration">
            <el-radio-button v-for="pr in product.prices" :key="pr.duration" :value="pr.duration" :data-testid="`rent-duration-${pr.duration}`">{{ labelOf(durationLabel, pr.duration) }} {{ fmtYuan(pr.amount) }}</el-radio-button>
          </el-radio-group>
          <div class="row">
            <span>× </span><el-input-number v-model="form.quantity" :min="1" :max="quantityMax" data-testid="rent-quantity" />
            <span class="muted" data-testid="rent-duration-text">{{ describeDuration(form.duration, form.quantity) }}（单次最长 90 天）</span>
          </div>
        </el-form-item>
        <el-form-item label="镜像">
          <el-select v-model="form.imageId" data-testid="rent-image"><el-option v-for="img in images" :key="img.id" :label="img.name" :value="img.id" /></el-select>
        </el-form-item>
        <el-form-item v-if="canSpecifyMachine" label="指定机器（可选）">
          <el-select v-model="form.machineId" clearable placeholder="由系统分配" data-testid="rent-machine">
            <el-option v-for="m in machines" :key="m.id" :value="m.id" :label="m.busy_until ? `${m.label}（占用至 ${fmtTime(m.busy_until)}）` : m.label" />
          </el-select>
          <ul v-if="form.machineId !== null" class="periods" data-testid="rent-machine-periods">
            <li v-for="p in machinePeriods" :key="p.lower" :class="{ mine: p.mine }">{{ fmtTime(p.lower) }} ～ {{ fmtTime(p.upper) }}{{ p.mine ? '（我的）' : '' }}</li>
            <li v-if="!machinePeriods.length" class="muted">未来 60 天无占用</li>
          </ul>
        </el-form-item>
        <el-form-item label="自动续费">
          <el-switch v-model="form.autoRenew" data-testid="rent-auto-renew" />
          <el-select v-if="form.autoRenew" v-model="form.autoRenewDuration" data-testid="rent-auto-renew-duration" style="width: 110px; margin-left: 8px">
            <el-option v-for="pr in product.prices" :key="pr.duration" :value="pr.duration" :label="labelOf(durationLabel, pr.duration)" />
          </el-select>
          <span class="muted">到期前 1 小时自动扣款；被后续预订挡住时按能放下的档位办理</span>
        </el-form-item>
      </el-form>
      <p class="avail" :class="{ warn: notEnough }" data-testid="rent-availability">
        <template v-if="availability">该时段可{{ form.immediate ? '租' : '预订' }} {{ availability.available }} 台<template v-if="notEnough">，不足 {{ form.count }} 台，请减少数量或更换时段</template></template>
        <template v-else>正在查询可用机器…</template>
      </p>
      <div v-if="!form.immediate && form.groupId !== null && calendar.length" class="calendar" data-testid="rent-calendar">
        <button v-for="d in calendar" :key="d.date" type="button" class="day" :class="{ full: d.free === 0, picked: d.date === form.day }" :data-testid="`rent-day-${d.date}`" :disabled="d.free === 0" @click="pickDay(d)">
          <span class="date">{{ d.date.slice(5) }}</span><span class="free">{{ d.free }}/{{ d.total }}</span>
        </button>
      </div>
    </template>

    <template v-else>
      <el-descriptions :column="1" border>
        <el-descriptions-item label="商品"><span data-testid="market-confirm-product">{{ product.name }}</span></el-descriptions-item>
        <el-descriptions-item label="机组"><span data-testid="market-confirm-group">{{ groupName }}<template v-if="machineLabel"> · {{ machineLabel }}</template></span></el-descriptions-item>
        <el-descriptions-item label="数量"><span data-testid="market-confirm-count">{{ form.count }} 台</span></el-descriptions-item>
        <el-descriptions-item label="开始"><span data-testid="market-confirm-start">{{ form.immediate ? '立即' : `${form.day} ${String(form.hour ?? 0).padStart(2, '0')}:00` }}</span></el-descriptions-item>
        <el-descriptions-item label="租期"><span data-testid="market-confirm-duration">{{ form.quantity === 1 ? labelOf(durationLabel, form.duration) : describeDuration(form.duration, form.quantity) }}</span></el-descriptions-item>
        <el-descriptions-item label="镜像"><span data-testid="market-confirm-image">{{ imageName }}</span></el-descriptions-item>
        <el-descriptions-item v-if="form.count > 1" label="单台价"><span data-testid="market-confirm-unit">{{ unitPrice == null ? '—' : fmtYuan(unitPrice * form.quantity) }}</span></el-descriptions-item>
        <el-descriptions-item :label="form.count > 1 ? '总价' : '价格'"><span data-testid="market-confirm-price">{{ total == null ? '—' : fmtYuan(total) }}</span></el-descriptions-item>
        <el-descriptions-item label="当前余额"><span data-testid="market-confirm-balance">{{ fmtYuan(balance) }}</span></el-descriptions-item>
        <el-descriptions-item label="扣款后余额">
          <span data-testid="market-confirm-after" :class="{ warn: insufficient }">{{ after == null ? '—' : fmtYuan(after) }}<template v-if="insufficient">（余额不足）</template></span>
        </el-descriptions-item>
      </el-descriptions>
      <el-button v-if="!expanded" link type="primary" data-testid="market-confirm-more" @click="expanded = true; step = 'form'">更多选项（机组 / 多台 / 预订 / 自动续费）</el-button>
    </template>
    <!-- 具名插槽只能放在组件的直接子级：按步骤切换按钮 -->
    <template #footer>
      <template v-if="step === 'form'">
        <el-button data-testid="rent-cancel" @click="visible = false">取消</el-button>
        <el-button type="primary" :disabled="notEnough || !availability" data-testid="rent-next" @click="toConfirm">下一步</el-button>
      </template>
      <template v-else>
        <el-button v-if="expanded" data-testid="market-confirm-back" @click="step = 'form'">上一步</el-button>
        <el-button data-testid="market-confirm-cancel" @click="visible = false">取消</el-button>
        <el-button type="primary" :loading="submitting" data-testid="market-confirm-ok" @click="submit">{{ insufficient ? '去充值' : '确认下单' }}</el-button>
      </template>
    </template>
  </AppDialog>
  <!-- 放在 AppDialog 外（fragment）：el-dialog 带 destroy-on-close，余额不足时先关对话框再弹引导，放在里面会随插槽销毁 -->
  <GuideDialog :guide="guide" />
</template>

<style scoped>
.row { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; margin-top: 8px; }
.muted { color: #909399; font-size: 13px; }
.warn { color: var(--el-color-warning); }
.avail { margin: 8px 0; font-weight: 600; }
.native-date { height: 32px; border: 1px solid #dcdfe6; border-radius: 4px; padding: 0 8px; font-size: 14px; background: #fff; }
.calendar { display: grid; grid-template-columns: repeat(7, 1fr); gap: 4px; max-height: 220px; overflow-y: auto; }
.day { border: 1px solid #dcdfe6; border-radius: 4px; background: #fff; padding: 4px 0; display: flex; flex-direction: column; align-items: center; cursor: pointer; font-size: 12px; }
.day.full { color: #c0c4cc; background: #f5f7fa; cursor: not-allowed; }
.day.picked { border-color: var(--el-color-primary); color: var(--el-color-primary); }
.periods { list-style: none; padding: 0; margin: 6px 0 0; font-size: 12px; color: #606266; }
.periods .mine { color: var(--el-color-primary); }
</style>
