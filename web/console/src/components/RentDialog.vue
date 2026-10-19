<script setup lang="ts">
import {
  durationLabel, fmtTime, fmtYuan, isApiError, labelOf,
  type AppCatalogItem, type Availability, type BookingResult, type CalendarDay, type CalendarPeriod, type Duration, type Group, type GroupMachine, type Image, type Instance, type Listed, type Order, type Product,
} from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, onUnmounted, reactive, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api } from '../api'
import { useGuide } from '../composables/useGuide'
import { appConfirmText, appOptionLabel, compatibleImages, envDefaults } from '../logic/apps'
import { cstDay, describeDuration, earliestStart, hourOptions, isLegacyImmediate, latestStart, MAX_COUNT, MAX_QUANTITY, startAtISO, totalPrice } from '../logic/booking'
import { notifyError } from '../logic/notify'
import { bookingBody, orderBody, useRentKey } from '../logic/rent'
import { refreshBalance, session } from '../store/session'
import AppDialog from './AppDialog.vue'
import AppEnvForm from './AppEnvForm.vue'
import GuideDialog from './GuideDialog.vue'

// apps：可预装的应用（GET /apps）；appId：市场页应用卡片预选的应用
const props = withDefaults(defineProps<{ product: Product; images: Image[]; mode: 'immediate' | 'booking'; duration: Duration; imageId: number | null; apps?: AppCatalogItem[]; appId?: number | null }>(), {
  apps: () => [],
  appId: null,
})
const visible = defineModel<boolean>({ required: true })
export interface PlacedResult { instances: { id: string }[]; batchId: string | null; immediate: boolean }
const emit = defineEmits<{ placed: [result: PlacedResult]; noCapacity: [] }>()

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
  appId: props.appId,
})
// 预选了应用时先停在表单步：要锁定镜像、填变量（如 JupyterLab 必填令牌）
const step = ref<'form' | 'confirm'>(props.mode === 'immediate' && props.appId == null ? 'confirm' : 'form')
const expanded = ref(props.mode === 'booking' || props.appId != null)
const groups = ref<Group[]>([])
const serverOffset = ref(0) // 服务器时间 − 浏览器时间：最早 / 最晚整点按服务器时间算
// 每分钟走一格：hours / dayMin / dayMax / 日历可选天依赖它，对话框久开也不会给出已过期的整点
const tick = ref(0)
const ticker = setInterval(() => {
  tick.value++
  revalidateStart()
}, 60_000)
onUnmounted(() => clearInterval(ticker))
const nowServer = () => {
  void tick.value
  return new Date(Date.now() + serverOffset.value)
}
const machines = ref<GroupMachine[]>([])
const machinePeriods = ref<CalendarPeriod[]>([])
const calendar = ref<CalendarDay[]>([])
const availability = ref<Availability | null>(null)
const availError = ref('') // 可用性查询失败的提示；空 = 无错误
const submitting = ref(false)

const NO_GROUP = 0 // el-option 的 value 不能是 null：0 作为「不限机组」哨兵，提交时映射回 null
const groupSel = computed({ get: () => form.groupId ?? NO_GROUP, set: (v: number) => { form.groupId = v === NO_GROUP ? null : v } })

// ---- 预装应用：选了应用后镜像只剩兼容镜像（默认第一个），下方展开变量表单 ----
const NO_APP = 0 // 同 NO_GROUP：「不预装」
const appSel = computed({ get: () => form.appId ?? NO_APP, set: (v: number) => { form.appId = v === NO_APP ? null : v } })
const selectedApp = computed(() => props.apps.find((a) => a.id === form.appId) ?? null)
const imageOptions = computed(() => (selectedApp.value ? compatibleImages(selectedApp.value, props.images) : props.images))
const envValues = ref<Record<string, string>>({})
const envForm = ref<InstanceType<typeof AppEnvForm> | null>(null)
const appError = ref('') // 后端 app_* 错误文案，显示在表单里
/** 按所选应用重置镜像与变量；取消预装时镜像恢复为打开时的选择。 */
function applyApp() {
  const app = selectedApp.value
  appError.value = ''
  form.imageId = app ? (compatibleImages(app, props.images)[0]?.id ?? null) : props.imageId
  envValues.value = app ? envDefaults(app.version.env) : {}
}
watch(() => form.appId, applyApp)
watch(envValues, () => { appError.value = '' })
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
// 派生参数（含应用与去掉空值的变量）的值变了才换键；同参数重试复用
const key = useRentKey(() => props.product.id, form, () => envValues.value)

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
  calendar.value = []
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
    revalidateStart()
  } catch (e) {
    notifyError(e)
  }
}
/** 指定时间模式下，当前开始时刻为空或已不在可选范围内 → 改为最早整点。 */
function revalidateStart() {
  if (form.immediate) return
  if (form.day && form.hour != null && hourOptions(form.day, nowServer()).includes(form.hour)) return
  const e = earliestStart(nowServer())
  form.day = cstDay(e)
  form.hour = hourOptions(form.day, nowServer())[0] ?? null
}
/** 回到表单步：重新拿服务器时间，校验开始时刻。 */
function backToForm() {
  step.value = 'form'
  void loadGroups()
}
async function loadGroupDetails() {
  const gid = form.groupId
  if (gid === null) return
  try {
    // product_id：机组可跨商品，机器列表与日历只算本商品的机器（与可用性 / 下单同口径）
    const pid = props.product.id
    const [m, c] = await Promise.all([
      api.get<Listed<GroupMachine>>(`/groups/${gid}/machines`, { product_id: pid }),
      api.get<{ days: CalendarDay[] }>(`/groups/${gid}/calendar`, { product_id: pid, days: 61 }),
    ])
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
    if (mine === availSeq) {
      availability.value = out
      availError.value = ''
    }
  } catch (e) {
    if (mine !== availSeq) return
    availability.value = null
    // booking_* 是对所选时间 / 租期的校验，直接展示后端文案；其余错误给通用提示 + 重试
    availError.value = isApiError(e) && e.code.startsWith('booking_') ? e.message : '查询可用机器失败'
  }
}
function retryAvailability() {
  availError.value = ''
  void loadAvailability()
}
watch([() => form.groupId, () => form.machineId, () => form.immediate, startAt, () => form.duration, () => form.quantity], () => {
  availability.value = null // 旧结果不再对应当前参数
  availError.value = ''
  clearTimeout(availTimer)
  availTimer = setTimeout(() => void loadAvailability(), 300)
})

watch(visible, (v) => {
  if (!v) return
  step.value = props.mode === 'immediate' && props.appId == null ? 'confirm' : 'form'
  expanded.value = props.mode === 'booking' || props.appId != null
  Object.assign(form, { groupId: null, count: 1, immediate: props.mode === 'immediate', duration: props.duration, quantity: 1, imageId: props.imageId, machineId: null, autoRenew: false, autoRenewDuration: props.duration, appId: props.appId })
  applyApp() // appId 与上次相同时 watch 不触发：显式重置镜像与变量
  availability.value = null
  availError.value = ''
  form.day = ''
  form.hour = null
  void loadGroups() // 拿到服务器时间后再填默认开始时刻
  void loadAvailability()
}, { immediate: true })

/** 日历上不可选的天：没有空闲机器，或当天没有落在 [最早, 最晚] 内的整点。 */
function dayDisabled(d: CalendarDay): boolean {
  return d.free === 0 || hourOptions(d.date, nowServer()).length === 0
}
function pickDay(d: CalendarDay) {
  if (dayDisabled(d)) return
  form.day = d.date
}

function toConfirm() {
  if (!form.immediate && !startAt.value) return void ElMessage.warning('请选择开始时间')
  if (selectedApp.value && form.imageId == null) return void ElMessage.warning('暂无该应用可用的镜像')
  if (envForm.value && !envForm.value.validate()) return
  step.value = 'confirm'
}

/** 预装的应用 ID：不预装（或应用不在目录里）时为 null，请求体不带 app_id / app_env（与计划 6 一致）。 */
const appIdForBody = () => selectedApp.value?.id ?? null

async function submit() {
  if (submitting.value || form.imageId == null) return
  if (insufficient.value) {
    visible.value = false
    await router.push({ path: '/wallet/recharge', query: { redirect: route.fullPath } })
    return
  }
  submitting.value = true
  try {
    let placed: PlacedResult
    if (legacy.value) {
      const out = await api.post<{ order: Order; instance: Instance }>('/orders', orderBody(props.product.id, form, appIdForBody(), envValues.value), { idempotencyKey: key.current() })
      placed = { instances: [out.instance], batchId: null, immediate: true }
    } else {
      const out = await api.post<BookingResult>('/bookings', bookingBody(props.product.id, form, startAt.value, appIdForBody(), envValues.value), { idempotencyKey: key.current() })
      placed = { instances: out.instances, batchId: out.batch?.id ?? null, immediate: form.immediate }
    }
    key.rotate()
    visible.value = false
    emit('placed', placed)
  } catch (e) {
    if (isApiError(e) && (e.code === 'no_capacity' || e.code === 'machine_unavailable')) {
      ElMessage.warning(e.message)
      if (legacy.value && e.code === 'no_capacity') emit('noCapacity') // 市场页刷新「现在可租」台数
      backToForm()
      void loadAvailability()
    } else if (isApiError(e) && e.code.startsWith('app_')) {
      // app_image_mismatch / app_env_* / app_not_found / app_unavailable：显示后端文案，回到表单改
      ElMessage.warning(e.message)
      backToForm()
      appError.value = e.message
    } else if (isApiError(e) && e.code.startsWith('booking_')) {
      ElMessage.warning(e.message)
      backToForm() // 多半是开始时间过近 / 过远：刷新服务器时间并校正默认开始时刻
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
        <el-form-item v-if="apps.length" label="预装应用">
          <el-select v-model="appSel" data-testid="rent-app">
            <el-option :value="NO_APP" label="不预装" />
            <el-option v-for="a in apps" :key="a.id" :value="a.id" :label="appOptionLabel(a)" :disabled="!compatibleImages(a, images).length" />
          </el-select>
        </el-form-item>
        <el-form-item label="镜像">
          <el-select v-model="form.imageId" data-testid="rent-image"><el-option v-for="img in imageOptions" :key="img.id" :label="img.name" :value="img.id" /></el-select>
          <div v-if="selectedApp" class="muted" data-testid="rent-image-locked">该应用需要此镜像</div>
        </el-form-item>
        <template v-if="selectedApp">
          <el-alert v-if="appError" type="error" :title="appError" :closable="false" show-icon data-testid="rent-app-error" />
          <AppEnvForm v-if="selectedApp.version.env.length" :key="selectedApp.id" ref="envForm" v-model="envValues" :spec="selectedApp.version.env" />
        </template>
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
      <p class="avail" :class="{ warn: notEnough || availError }" data-testid="rent-availability">
        <template v-if="availError">{{ availError }} <el-button link type="primary" data-testid="rent-availability-retry" @click="retryAvailability">重试</el-button></template>
        <template v-else-if="availability">该时段可{{ form.immediate ? '租' : '预订' }} {{ availability.available }} 台<template v-if="notEnough">，不足 {{ form.count }} 台，请减少数量或更换时段</template></template>
        <template v-else>正在查询可用机器…</template>
      </p>
      <div v-if="!form.immediate && form.groupId !== null && calendar.length" class="calendar" data-testid="rent-calendar">
        <button v-for="d in calendar" :key="d.date" type="button" class="day" :class="{ full: dayDisabled(d), picked: d.date === form.day }" :data-testid="`rent-day-${d.date}`" :disabled="dayDisabled(d)" @click="pickDay(d)">
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
        <el-descriptions-item v-if="selectedApp" label="预装应用"><span data-testid="market-confirm-app">{{ appConfirmText(selectedApp, envValues) }}</span></el-descriptions-item>
        <el-descriptions-item v-if="form.count > 1" label="单台价"><span data-testid="market-confirm-unit">{{ unitPrice == null ? '—' : fmtYuan(unitPrice * form.quantity) }}</span></el-descriptions-item>
        <el-descriptions-item :label="form.count > 1 ? '总价' : '价格'"><span data-testid="market-confirm-price">{{ total == null ? '—' : fmtYuan(total) }}</span></el-descriptions-item>
        <el-descriptions-item label="当前余额"><span data-testid="market-confirm-balance">{{ fmtYuan(balance) }}</span></el-descriptions-item>
        <el-descriptions-item label="扣款后余额">
          <span data-testid="market-confirm-after" :class="{ warn: insufficient }">{{ after == null ? '—' : fmtYuan(after) }}<template v-if="insufficient">（余额不足）</template></span>
        </el-descriptions-item>
      </el-descriptions>
      <el-button v-if="!expanded" link type="primary" data-testid="market-confirm-more" @click="expanded = true; backToForm()">更多选项（机组 / 多台 / 预订 / 自动续费{{ apps.length ? ' / 预装应用' : '' }}）</el-button>
    </template>
    <!-- 具名插槽只能放在组件的直接子级：按步骤切换按钮 -->
    <template #footer>
      <template v-if="step === 'form'">
        <el-button data-testid="rent-cancel" @click="visible = false">取消</el-button>
        <el-button type="primary" :disabled="notEnough" data-testid="rent-next" @click="toConfirm">下一步</el-button>
      </template>
      <template v-else>
        <el-button v-if="expanded" data-testid="market-confirm-back" @click="backToForm">上一步</el-button>
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
