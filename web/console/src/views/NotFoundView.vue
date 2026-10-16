<script setup lang="ts">
import { useRouter } from 'vue-router'

const router = useRouter()
</script>

<template>
  <el-result icon="warning" title="页面不存在" sub-title="您访问的地址不存在或已失效" data-testid="notfound-page">
    <template #extra>
      <el-button type="primary" data-testid="notfound-home" @click="router.replace('/market')">返回算力市场</el-button>
    </template>
  </el-result>
</template>
