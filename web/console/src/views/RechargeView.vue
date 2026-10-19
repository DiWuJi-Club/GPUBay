<script setup lang="ts">
import { isMobileUA, labelOf, messageOf, rechargeChannelLabel, type Listed, type Recharge } from '@spark/shared'
import { computed, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api } from '../api'
import GuideDialog from '../components/GuideDialog.vue'
import { useGuide } from '../composables/useGuide'
import { PRESETS, validateAmount } from '../logic/recharge'

const route = useRoute()
const router = useRouter()
const guide = useGuide()
const mobileUA = isMobileUA() // 渠道按设备类型（User-Agent），不看屏幕宽度
const channels = ref<string[]>([])
const channel = ref('')
const amount = ref('100')
const amountError = ref('')
const submitting = ref(false)
const loading = ref(true)
const loadError = ref('')

const visibleChannels = computed(() => (mobileUA ? channels.value.filter((c) => c === 'alipay') : channels.value))
const unavailable = computed(() => !loading.value && !loadError.value && visibleChannels.value.length === 0)

async function loadChannels() {
  loading.value = true
  loadError.value = ''
  try {
    channels.value = (await api.get<Listed<string>>('/pay/channels')).items
    channel.value = visibleChannels.value[0] ?? ''
  } catch (e) {
    loadError.value = messageOf(e) // 渠道没加载到时不能显示成「暂不支持」，给出错误与重试
  } finally {
    loading.value = false
  }
}
onMounted(loadChannels)

function pick(yuan: number) {
  amount.value = String(yuan)
  amountError.value = ''
}

async function submit() {
  if (submitting.value) return // 双击保护：请求进行中不再发第二个创建请求
  const v = validateAmount(amount.value)
  if ('error' in v) {
    amountError.value = v.error
    return
  }
  amountError.value = ''
  if (!channel.value) return
  submitting.value = true
  try {
    const r = await api.post<Recharge>('/recharges', { amount: v.cents, channel: channel.value })
    await router.push({ path: `/wallet/recharge/${r.id}`, query: route.query.redirect ? { redirect: String(route.query.redirect) } : {} })
  } catch (e) {
    guide.handle(e) // kyc_required 引导；too_many_pending / pay_unavailable / bad_request / 5xx 原文提示一次
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <div class="recharge">
    <el-card>
      <el-form label-position="top" @submit.prevent="submit">
        <el-form-item label="充值金额（元）" :error="amountError" :show-message="false" data-testid="recharge-amount-item">
          <div class="presets">
            <el-button v-for="p in PRESETS" :key="p" :type="amount === String(p) ? 'primary' : 'default'" :data-testid="`recharge-preset-${p}`" @click="pick(p)">{{ p }} 元</el-button>
          </div>
          <el-input v-model="amount" data-testid="recharge-custom" inputmode="decimal" placeholder="自定义金额，1～1,000,000 元，最多两位小数" @input="amountError = ''" />
          <span v-if="amountError" class="field-error" data-testid="recharge-amount-error">{{ amountError }}</span>
        </el-form-item>
        <el-form-item label="支付方式">
          <el-alert v-if="loadError" type="error" :title="loadError" :closable="false" show-icon data-testid="recharge-error">
            <el-button size="small" data-testid="recharge-retry" @click="loadChannels">重试</el-button>
          </el-alert>
          <el-alert v-else-if="unavailable" type="warning" :title="mobileUA ? '当前暂不支持手机端充值' : '暂无可用的支付方式'" :closable="false" show-icon data-testid="recharge-unavailable" />
          <el-radio-group v-else v-model="channel">
            <el-radio-button v-for="c in visibleChannels" :key="c" :value="c" :data-testid="`recharge-channel-${c}`">{{ labelOf(rechargeChannelLabel, c) }}</el-radio-button>
          </el-radio-group>
        </el-form-item>
        <el-button type="primary" native-type="submit" class="submit" :disabled="unavailable || !channel" :loading="submitting" data-testid="recharge-submit">下一步</el-button>
      </el-form>
    </el-card>
    <GuideDialog :guide="guide" />
  </div>
</template>

<style scoped>
.recharge { max-width: 560px; }
.presets { display: flex; flex-wrap: wrap; gap: 8px; margin-bottom: 8px; width: 100%; }
.submit { width: 100%; }
.field-error { color: var(--el-color-danger); font-size: 12px; line-height: 1; padding-top: 4px; }
</style>
