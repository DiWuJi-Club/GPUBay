<script setup lang="ts">
import { useRoute, useRouter } from 'vue-router'

// 未登录时 Keys / 用量 / 请求日志 Tab 的登录引导（不强制跳转，与 /market 一致）
const route = useRoute()
const router = useRouter()
const login = () => router.push({ path: '/login', query: { redirect: route.fullPath } })
</script>

<template>
  <el-empty description="登录后可创建 API Key、查看用量与请求日志" data-testid="tokens-login-prompt">
    <el-button type="primary" data-testid="tokens-login-btn" @click="login">去登录</el-button>
  </el-empty>
</template>
