<script setup lang="ts">
import type { AppCatalogItem, Duration, Image, Listed, Product, WorkflowCatalogItem } from '@spark/shared'
import { ElMessage } from 'element-plus'
import { reactive, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api } from '../api'
import { useGuide } from '../composables/useGuide'
import { compatibleImages } from '../logic/apps'
import { firstDuration, rentChoice, WORKFLOW_APP_SLUG, WORKFLOW_RENT_ENV } from '../logic/workflows'
import { refreshBalance, session } from '../store/session'
import GuideDialog from './GuideDialog.vue'
import RentDialog, { type PlacedResult } from './RentDialog.vue'
import WorkflowInstallDialog from './WorkflowInstallDialog.vue'

/**
 * 市场页与详情页共用的「一键安装」流程：未登录 → 登录（回来后 ?install=<slug> 自动再打开）；
 * 已登录 → WorkflowInstallDialog；其中「新开一台实例」→ RentDialog（预选 ComfyUI、MODEL_ID=none、预装该工作流）。
 * 父组件经 ref 调 open(workflow)。
 */
type Wf = Pick<WorkflowCatalogItem, 'id' | 'slug' | 'name' | 'version'>
const route = useRoute()
const router = useRouter()
const guide = useGuide()

const installVisible = ref(false)
const workflow = ref<Wf | null>(null)

async function open(w: Wf) {
  if (session.status !== 'authed') {
    await router.push({ path: '/login', query: { redirect: `${route.path}?install=${encodeURIComponent(w.slug)}` } })
    return
  }
  workflow.value = w
  installVisible.value = true
}
defineExpose({ open })

// ---- 新开一台实例 ----
const rent = reactive<{
  visible: boolean; product: Product | null; mode: 'immediate' | 'booking'; duration: Duration; imageId: number | null
  images: Image[]; apps: AppCatalogItem[]; appId: number | null; workflowIds: number[]
}>({ visible: false, product: null, mode: 'immediate', duration: '1d', imageId: null, images: [], apps: [], appId: null, workflowIds: [] })
const preparing = ref(false)

async function onRent(workflowId: number) {
  if (preparing.value) return
  if (session.user?.kyc_status !== 'verified') {
    guide.kycRequired() // 同市场页：不发下单请求
    return
  }
  preparing.value = true
  try {
    const [p, i, a] = await Promise.all([api.get<Listed<Product>>('/products'), api.get<Listed<Image>>('/images'), api.get<Listed<AppCatalogItem>>('/apps')])
    const comfy = a.items.find((x) => x.slug === WORKFLOW_APP_SLUG)
    if (!comfy) return void ElMessage.warning('ComfyUI 应用暂未上架')
    const imageId = compatibleImages(comfy, i.items)[0]?.id ?? null
    if (imageId == null) return void ElMessage.warning('暂无 ComfyUI 可用的镜像')
    const choice = rentChoice(p.items)
    if (!choice) return void ElMessage.warning('暂无可租的商品')
    if (choice.mode === 'booking') ElMessage.info('现在没有空闲机器，可以预订档期')
    await refreshBalance()
    Object.assign(rent, {
      product: choice.product, mode: choice.mode, duration: firstDuration(choice.product), imageId, images: i.items, apps: a.items, appId: comfy.id, workflowIds: [workflowId],
    })
    rent.visible = true
  } catch (e) {
    guide.handle(e) // 余额等引导；其余提示
  } finally {
    preparing.value = false
  }
}

async function onPlaced(r: PlacedResult) {
  if (r.batchId && r.instances.length > 1) {
    ElMessage.success(`已下单 ${r.instances.length} 台`)
    await router.push({ path: '/instances', query: { batch: r.batchId } })
    return
  }
  ElMessage.success(r.immediate ? '下单成功，正在开通，ComfyUI 与工作流会自动安装' : '预订成功')
  await router.push({ path: `/instances/${r.instances[0].id}`, query: { highlight: 'workflows' } })
}
</script>

<template>
  <WorkflowInstallDialog v-if="workflow" v-model="installVisible" :workflow="workflow" @rent="onRent" />
  <RentDialog
    v-if="rent.product" v-model="rent.visible" :product="rent.product" :images="rent.images" :mode="rent.mode" :duration="rent.duration" :image-id="rent.imageId"
    :apps="rent.apps" :app-id="rent.appId" :preset-env="WORKFLOW_RENT_ENV" :preset-workflow-ids="rent.workflowIds" @placed="onPlaced"
  />
  <GuideDialog :guide="guide" />
</template>
