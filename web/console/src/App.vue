<script setup lang="ts">
import { onMounted } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import ConsoleLayout from './layouts/ConsoleLayout.vue'
import { parseInviteParam, storeInvite } from './logic/invite'
import { probeSession, session } from './store/session'

const route = useRoute()
const router = useRouter()

// 邀请落地：任意路由带 ?ic= 时记住邀请码（30 天，供登录页使用），并从地址栏去掉参数；已登录的老用户不存
onMounted(async () => {
  if (!new URLSearchParams(window.location.search).has('ic')) return
  const code = parseInviteParam(window.location.search)
  await probeSession()
  if (code && session.status !== 'authed') storeInvite(code)
  await router.isReady()
  const cur = router.currentRoute.value
  if (cur.query.ic === undefined) return
  const query = { ...cur.query }
  delete query.ic
  await router.replace({ path: cur.path, query, hash: cur.hash })
})
</script>

<template>
  <!-- 首次导航（含会话探测）完成前 route 是 START_LOCATION（matched 为空）：什么都不渲染，避免登录页先闪出控制台布局 -->
  <template v-if="route.matched.length">
    <router-view v-if="route.path === '/login'" />
    <ConsoleLayout v-else>
      <router-view />
    </ConsoleLayout>
  </template>
</template>
