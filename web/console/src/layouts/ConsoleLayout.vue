<script setup lang="ts">
import { brand, useMobile } from '@spark/shared'
import { computed } from 'vue'
import { useRoute } from 'vue-router'
import { maskPhone } from '../logic/login'
import { session } from '../store/session'

const mobile = useMobile()
const route = useRoute()
const tabs = [
  { key: 'market', path: '/market', label: '算力市场' },
  { key: 'instances', path: '/instances', label: '我的实例' },
  { key: 'wallet', path: '/wallet', label: '钱包' },
  { key: 'account', path: '/account', label: '我的' },
] as const
const active = computed(() => tabs.find((t) => route.path === t.path || route.path.startsWith(t.path + '/'))?.key ?? '')
const logo = import.meta.env.BASE_URL + brand.logoUrl
</script>

<template>
  <div class="shell" :class="{ mobile }" data-testid="layout">
    <aside v-if="!mobile" class="side" data-testid="layout-side">
      <div class="logo"><img :src="logo" alt="" width="28" height="28" /><span>{{ brand.name }}</span></div>
      <nav>
        <router-link v-for="t in tabs" :key="t.key" :to="t.path" class="menu-item" :class="{ active: active === t.key }" :data-testid="`menu-${t.key}`">
          {{ t.label }}
        </router-link>
      </nav>
    </aside>
    <div class="main">
      <header class="topbar" data-testid="layout-topbar">
        <span class="title">{{ route.meta.title }}</span>
        <span class="right">
          <router-link v-if="!mobile" to="/help" data-testid="layout-help">使用帮助</router-link>
          <span v-if="session.user" data-testid="layout-phone">{{ maskPhone(session.user.phone) }}</span>
          <router-link v-else-if="!mobile" to="/login" data-testid="layout-login">登录</router-link>
        </span>
      </header>
      <main class="content"><slot /></main>
    </div>
    <nav v-if="mobile" class="tabbar" data-testid="layout-tabbar">
      <router-link v-for="t in tabs" :key="t.key" :to="t.path" :class="{ active: active === t.key }" :data-testid="`tabbar-${t.key}`">
        {{ t.label }}
      </router-link>
    </nav>
  </div>
</template>

<style scoped>
.shell { display: flex; min-height: 100vh; }
.side { width: 200px; background: #fff; border-right: 1px solid #ebeef5; flex-shrink: 0; }
.logo { display: flex; align-items: center; gap: 8px; padding: 16px; font-weight: 600; }
.menu-item { display: block; padding: 12px 16px; color: #303133; text-decoration: none; }
.menu-item.active { color: var(--el-color-primary); background: #ecf5ff; }
.main { flex: 1; display: flex; flex-direction: column; min-width: 0; }
.topbar { height: 56px; display: flex; align-items: center; justify-content: space-between; padding: 0 16px; background: #fff; border-bottom: 1px solid #ebeef5; }
.title { font-weight: 600; }
.right { display: flex; gap: 16px; align-items: center; }
.content { flex: 1; padding: 16px; }
.shell.mobile .content { padding: 12px 16px calc(var(--sc-tabbar-height) + 16px + env(safe-area-inset-bottom)); }
.tabbar {
  position: fixed; left: 0; right: 0; bottom: 0; display: flex; background: #fff; border-top: 1px solid #ebeef5;
  height: calc(var(--sc-tabbar-height) + env(safe-area-inset-bottom)); padding-bottom: env(safe-area-inset-bottom); box-sizing: border-box;
}
.tabbar a { flex: 1; display: flex; align-items: center; justify-content: center; min-height: 44px; color: #606266; text-decoration: none; font-size: 13px; }
.tabbar a.active { color: var(--el-color-primary); }
</style>
