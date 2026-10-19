<script setup lang="ts">
import { useRoute } from 'vue-router'
import TotpDialog from './components/TotpDialog.vue'
import AdminLayout from './layouts/AdminLayout.vue'

const route = useRoute()
const bare = ['/login', '/403']
</script>

<template>
  <!-- 首次导航（含会话探测）完成前 route 是 START_LOCATION（matched 为空）：什么都不渲染，避免登录页先闪出后台布局 -->
  <template v-if="route.matched.length">
    <router-view v-if="bare.includes(route.path)" />
    <AdminLayout v-else><router-view /></AdminLayout>
  </template>
  <TotpDialog />
</template>
