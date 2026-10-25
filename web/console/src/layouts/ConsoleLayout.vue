<script setup lang="ts">
import { brand, useMobile } from '@spark/shared'
import { computed } from 'vue'
import { useRoute } from 'vue-router'
import ButlerDrawer from '../components/ButlerDrawer.vue'
import ButlerLauncher from '../components/ButlerLauncher.vue'
import { maskPhone } from '../logic/login'
import { session } from '../store/session'

const mobile = useMobile()
const route = useRoute()
/** short：手机底部标签的短名（缺省同 label） */
const tabs: readonly { key: string; path: string; label: string; short?: string }[] = [
  // 手机底栏 6 个标签，320px 宽每格约 53px：四个字以上的用短名
  { key: 'market', path: '/market', label: '算力市场', short: '市场' },
  { key: 'workflows', path: '/workflows', label: '工作流市场', short: '工作流' },
  { key: 'instances', path: '/instances', label: '我的实例', short: '实例' },
  { key: 'butler', path: '/butler', label: 'AI 管家', short: '管家' },
  { key: 'wallet', path: '/wallet', label: '钱包' },
  { key: 'account', path: '/account', label: '我的' },
]
/** AI 管家浮动按钮与抽屉只在桌面端、且不在 /butler 全页时出现（D13） */
const showButler = computed(() => !mobile.value && route.path !== '/butler')
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
      <main class="content" :class="{ 'with-launcher': showButler }"><slot /></main>
    </div>
    <nav v-if="mobile" class="tabbar" data-testid="layout-tabbar">
      <router-link v-for="t in tabs" :key="t.key" :to="t.path" :class="{ active: active === t.key }" :data-testid="`tabbar-${t.key}`">
        {{ t.short ?? t.label }}
      </router-link>
    </nav>
    <template v-if="showButler">
      <ButlerLauncher />
      <ButlerDrawer v-if="session.status === 'authed'" />
    </template>
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
/* 浮动按钮占右下角：内容区底部留白，页面最底部的按钮不被挡住 */
.content.with-launcher { padding-bottom: 80px; }
.shell.mobile .content { padding: 12px 16px calc(var(--sc-tabbar-height) + 16px + env(safe-area-inset-bottom)); }
.tabbar {
  position: fixed; left: 0; right: 0; bottom: 0; display: flex; background: #fff; border-top: 1px solid #ebeef5;
  height: calc(var(--sc-tabbar-height) + env(safe-area-inset-bottom)); padding-bottom: env(safe-area-inset-bottom); box-sizing: border-box;
}
.tabbar a { flex: 1; min-width: 0; white-space: nowrap; display: flex; align-items: center; justify-content: center; min-height: 44px; color: #606266; text-decoration: none; font-size: 13px; }
.tabbar a.active { color: var(--el-color-primary); }
</style>
