<script setup lang="ts">
import { durationLabel, fmtYuan, labelOf, messageOf, validateYuanRange, type AdminImage, type AdminProduct, type Duration, type ImageInput, type Listed } from '@spark/shared'
import { ElMessage } from 'element-plus'
import { onMounted, reactive, ref } from 'vue'
import { api } from '../api'
import { notifyError } from '../logic/notify'

const PRICE_MIN_CENTS = 1 // 与后端 catalog.SetPrice 一致：0.01 元～100 万元
const PRICE_MAX_CENTS = 100_000_000
const PRICE_RANGE = '价格需在 0.01 元到 100 万元之间'

const products = ref<AdminProduct[]>([])
const images = ref<AdminImage[]>([])
const loading = ref(false)
const error = ref('')

/** 加载失败清空列表并显示页内错误与重试（不自动重试）。 */
async function load() {
  loading.value = true
  try {
    const [p, i] = await Promise.all([api.get<Listed<AdminProduct>>('/products'), api.get<Listed<AdminImage>>('/images')])
    products.value = p.items
    images.value = [...i.items].sort((a, b) => a.sort - b.sort || a.id - b.id)
    error.value = ''
  } catch (e) {
    notifyError(e)
    products.value = []
    images.value = []
    error.value = messageOf(e)
  } finally {
    loading.value = false
  }
}
onMounted(load)

// ---- 价格（PUT 需要动态码，由请求层弹出全局动态码框后带 totp 重发一次）----
const price = reactive({ visible: false, productId: 0, duration: '1d' as Duration, amount: '', enabled: true, error: '', busy: false })
function openPrice(p: AdminProduct, d: Duration) {
  const cur = p.prices.find((x) => x.duration === d)
  Object.assign(price, { visible: true, productId: p.id, duration: d, amount: cur ? (cur.amount / 100).toFixed(2) : '', enabled: cur?.enabled ?? true, error: '' })
}
async function savePrice(productId: number, d: Duration, amount: number, enabled: boolean) {
  await api.put(`/products/${productId}/prices/${d}`, { amount, enabled })
  ElMessage.success('价格已保存')
}
async function submitPrice() {
  if (price.busy) return // 同步守卫：连点只发一次
  const v = validateYuanRange(price.amount, PRICE_MIN_CENTS, PRICE_MAX_CENTS, { range: PRICE_RANGE })
  if ('error' in v) {
    price.error = v.error
    return
  }
  price.error = ''
  price.busy = true
  try {
    await savePrice(price.productId, price.duration, v.cents, price.enabled)
    price.visible = false
    await load()
  } catch (e) {
    notifyError(e) // 动态码取消（cancelled）静默：弹窗与输入保留
  } finally {
    price.busy = false
  }
}
/** 正在切换上 / 下架的档位（`商品ID-租期`）；同步守卫，连点只发一次。 */
const toggling = ref<string | null>(null)
async function togglePrice(p: AdminProduct, d: Duration, enabled: boolean) {
  const cur = p.prices.find((x) => x.duration === d)
  if (!cur || toggling.value) return
  toggling.value = `${p.id}-${d}`
  try {
    await savePrice(p.id, d, cur.amount, enabled)
  } catch (e) {
    notifyError(e)
  } finally {
    toggling.value = null
  }
  await load() // 成功取新值；失败 / 取消时开关回到服务端状态
}

// ---- 镜像（后端没有删除接口：「停用」即从控制台市场下架，需确认）----
const img = reactive({ visible: false, editing: null as AdminImage | null, form: { name: '', ref: '', description: '', enabled: true, sort: 0 } as ImageInput, error: '', busy: false })
function openImage(i: AdminImage | null) {
  img.editing = i
  img.form = i ? { name: i.name, ref: i.ref, description: i.description, enabled: i.enabled, sort: i.sort } : { name: '', ref: '', description: '', enabled: true, sort: (images.value.at(-1)?.sort ?? 0) + 10 }
  img.error = ''
  img.visible = true
}
async function submitImage() {
  if (img.busy) return
  img.error = !img.form.name.trim() ? '请填写镜像名称' : !img.form.ref.trim() ? '请填写镜像地址' : ''
  if (img.error) return
  img.busy = true
  try {
    if (img.editing) await api.put(`/images/${img.editing.id}`, img.form)
    else await api.post('/images', img.form)
    ElMessage.success(img.editing ? '镜像已更新' : '镜像已新增')
    img.visible = false
    await load()
  } catch (e) {
    notifyError(e) // 「镜像地址必须以小写字母或数字开头」等后端原文；弹窗与输入保留
  } finally {
    img.busy = false
  }
}
const put = (i: AdminImage, patch: Partial<ImageInput>) => api.put(`/images/${i.id}`, { name: i.name, ref: i.ref, description: i.description, enabled: i.enabled, sort: i.sort, ...patch })

/** 正在修改的镜像行（启用 / 停用 / 排序共用）：同步守卫，同一时间只处理一个操作。 */
const imgBusy = ref(false)
async function setImageEnabled(i: AdminImage, enabled: boolean): Promise<boolean> {
  if (imgBusy.value) return false
  imgBusy.value = true
  try {
    await put(i, { enabled })
    ElMessage.success(enabled ? '镜像已启用' : '镜像已停用')
    return true
  } catch (e) {
    notifyError(e)
    return false
  } finally {
    imgBusy.value = false
    await load()
  }
}
const disable = reactive({ visible: false, target: null as AdminImage | null })
function toggleImage(i: AdminImage, enabled: boolean) {
  if (imgBusy.value) return
  if (enabled) void setImageEnabled(i, true)
  else Object.assign(disable, { visible: true, target: i })
}
async function confirmDisable() {
  if (!disable.target) return
  if (await setImageEnabled(disable.target, false)) disable.visible = false
}

/**
 * 排序只在「启用」的镜像之间进行（控制台市场只显示启用的镜像）：上移 / 下移跳过已停用的行，
 * 与该方向上最近的启用镜像交换 sort 值；已停用的行不能移动（先启用再排序）。
 */
function neighbour(i: AdminImage, dir: -1 | 1): AdminImage | undefined {
  const idx = images.value.findIndex((x) => x.id === i.id)
  for (let k = idx + dir; k >= 0 && k < images.value.length; k += dir) if (images.value[k].enabled) return images.value[k]
  return undefined
}
const canMove = (i: AdminImage, dir: -1 | 1) => i.enabled && neighbour(i, dir) != null

/** 与相邻启用项交换 sort 值（各发一次 PUT）；两项 sort 相同时先把本项 ±1 保证顺序变化。第二个 PUT 失败时把第一个改回原值。 */
async function move(i: AdminImage, dir: -1 | 1) {
  if (imgBusy.value) return
  const other = canMove(i, dir) ? neighbour(i, dir) : undefined
  if (!other) return
  imgBusy.value = true
  let a = other.sort
  const b = i.sort
  if (a === b) a += dir
  let firstDone = false
  try {
    await put(i, { sort: a })
    firstDone = true
    await put(other, { sort: b })
    ElMessage.success('顺序已调整')
  } catch (e) {
    notifyError(e)
    if (firstDone) await put(i, { sort: b }).catch(() => {}) // 回滚：本项恢复原排序值（失败也只提示一次，随后按服务端状态刷新）
  } finally {
    imgBusy.value = false
  }
  await load()
}
</script>

<template>
  <div class="catalog">
    <el-alert v-if="error" type="error" :title="error" show-icon :closable="false" data-testid="cat-error">
      <el-button size="small" :loading="loading" data-testid="cat-retry" @click="load">重试</el-button>
    </el-alert>

    <el-card v-for="p in products" :key="p.id" :header="`${p.name}（${p.code}）`" data-testid="cat-product" :data-product-id="p.id">
      <el-table :data="p.prices" row-key="duration">
        <el-table-column label="租期" width="100"><template #default="{ row }">{{ labelOf(durationLabel, row.duration) }}</template></el-table-column>
        <el-table-column label="当前价格" width="140"><template #default="{ row }"><span :data-testid="`cat-price-amount-${row.duration}`">{{ fmtYuan(row.amount) }}</span></template></el-table-column>
        <el-table-column label="启用" width="100">
          <template #default="{ row }">
            <el-switch :model-value="row.enabled" :loading="toggling === `${p.id}-${row.duration}`" :data-testid="`cat-price-enabled-${row.duration}`" @change="(v: string | number | boolean) => togglePrice(p, row.duration, v === true)" />
          </template>
        </el-table-column>
        <el-table-column label="操作"><template #default="{ row }"><el-button size="small" :data-testid="`cat-price-edit-${row.duration}`" @click="openPrice(p, row.duration)">修改</el-button></template></el-table-column>
      </el-table>
    </el-card>

    <el-card>
      <template #header><div class="head"><span>镜像</span><el-button type="primary" data-testid="images-create-btn" @click="openImage(null)">新增镜像</el-button></div></template>
      <el-table :data="images" v-loading="loading" row-key="id">
        <el-table-column label="名称" min-width="160"><template #default="{ row }"><span data-testid="img-name">{{ row.name }}</span></template></el-table-column>
        <el-table-column label="仓库地址" min-width="260"><template #default="{ row }"><code data-testid="img-ref">{{ row.ref }}</code></template></el-table-column>
        <el-table-column label="描述" min-width="160"><template #default="{ row }"><span data-testid="img-desc">{{ row.description }}</span></template></el-table-column>
        <el-table-column label="启用" width="90">
          <template #default="{ row }"><el-switch :model-value="row.enabled" data-testid="img-enabled" @change="(v: string | number | boolean) => toggleImage(row as AdminImage, v === true)" /></template>
        </el-table-column>
        <el-table-column label="排序" width="80"><template #default="{ row }"><span data-testid="img-sort">{{ row.sort }}</span></template></el-table-column>
        <el-table-column label="操作" width="220">
          <template #default="{ row }">
            <el-button size="small" :disabled="!canMove(row as AdminImage, -1)" data-testid="img-up" @click="move(row as AdminImage, -1)">上移</el-button>
            <el-button size="small" :disabled="!canMove(row as AdminImage, 1)" data-testid="img-down" @click="move(row as AdminImage, 1)">下移</el-button>
            <el-button size="small" data-testid="img-edit" @click="openImage(row as AdminImage)">编辑</el-button>
          </template>
        </el-table-column>
      </el-table>
    </el-card>

    <el-dialog v-model="price.visible" :title="`修改价格：${labelOf(durationLabel, price.duration)}`" width="420" align-center destroy-on-close data-testid="price-dialog">
      <el-form label-position="top" @submit.prevent="submitPrice">
        <el-form-item label="价格（元）"><el-input v-model="price.amount" inputmode="decimal" data-testid="price-amount" /></el-form-item>
        <el-form-item label="启用"><el-switch v-model="price.enabled" data-testid="price-enabled" /></el-form-item>
        <p v-if="price.error" class="error" data-testid="price-error">{{ price.error }}</p>
        <el-button type="primary" native-type="submit" :loading="price.busy" data-testid="price-submit">保存</el-button>
      </el-form>
    </el-dialog>

    <el-dialog v-model="img.visible" :title="img.editing ? '编辑镜像' : '新增镜像'" width="520" align-center destroy-on-close data-testid="img-dialog">
      <el-form label-position="top" @submit.prevent="submitImage">
        <el-form-item label="名称"><el-input v-model="img.form.name" data-testid="img-form-name" /></el-form-item>
        <el-form-item label="仓库地址（以小写字母或数字开头）"><el-input v-model="img.form.ref" data-testid="img-form-ref" /></el-form-item>
        <el-form-item label="描述"><el-input v-model="img.form.description" data-testid="img-form-desc" /></el-form-item>
        <div class="two">
          <el-form-item label="启用"><el-switch v-model="img.form.enabled" data-testid="img-form-enabled" /></el-form-item>
          <el-form-item label="排序值"><el-input-number v-model="img.form.sort" :controls="false" data-testid="img-form-sort" /></el-form-item>
        </div>
        <p v-if="img.error" class="error" data-testid="img-form-error">{{ img.error }}</p>
        <el-button type="primary" native-type="submit" :loading="img.busy" data-testid="img-form-submit">保存</el-button>
      </el-form>
    </el-dialog>

    <el-dialog v-model="disable.visible" title="停用镜像" width="420" align-center data-testid="img-disable-dialog">
      <p data-testid="img-disable-text">停用后控制台市场不再提供「{{ disable.target?.name }}」，已创建的实例不受影响。</p>
      <template #footer>
        <el-button data-testid="img-disable-cancel" @click="disable.visible = false">取消</el-button>
        <el-button type="danger" :loading="imgBusy" data-testid="img-disable-ok" @click="confirmDisable">确定停用</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
.catalog { display: grid; gap: 16px; }
.head { display: flex; justify-content: space-between; align-items: center; }
.error { color: var(--el-color-danger); font-size: 13px; }
.two { display: grid; grid-template-columns: 1fr 1fr; gap: 12px; }
</style>
