<script setup lang="ts">
import { useRoute } from 'vue-router'
import ConsoleLayout from './layouts/ConsoleLayout.vue'

const route = useRoute()
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
