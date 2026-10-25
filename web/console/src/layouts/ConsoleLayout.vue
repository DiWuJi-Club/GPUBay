<script setup lang="ts">
import { ChatDotRound, Monitor, Picture, Service, Shop, User, Wallet } from '@element-plus/icons-vue'
import { brand, useMobile } from '@spark/shared'
import { computed } from 'vue'
import { useRoute } from 'vue-router'
import ButlerDrawer from '../components/ButlerDrawer.vue'
import ButlerLauncher from '../components/ButlerLauncher.vue'
import { maskPhone } from '../logic/login'
import { butlerAvailable } from '../store/butler'
import { session } from '../store/session'

const mobile = useMobile()
const route = useRoute()
/** short：手机底部标签的短名 */
const allTabs = [
  // 手机底栏最多 7 个标签（管家开启时），320px 宽每格约 45px（12px 字号最多三个汉字）：四个字以上的用短名（电脑侧边菜单用全名）
  { key: 'market', path: '/market', label: '算力市场', short: '市场', icon: Shop },
  { key: 'workflows', path: '/workflows', label: '工作流市场', short: '工作流', icon: Picture },
  { key: 'instances', path: '/instances', label: '我的实例', short: '实例', icon: Monitor },
  { key: 'butler', path: '/butler', label: 'AI 管家', short: '管家', icon: Service },
  { key: 'wallet', path: '/wallet', label: '钱包', short: '钱包', icon: Wallet },
  { key: 'account', path: '/account', label: '我的', short: '我的', icon: User },
  { key: 'tokens', path: '/tokens', label: 'Token 商城', short: 'Token', icon: ChatDotRound },
] as const
/** 平台未启用管家（或探测结果未出来）时不显示「AI 管家」标签 */
const tabs = computed(() => (butlerAvailable.value ? allTabs : allTabs.filter((t) => t.key !== 'butler')))
/** AI 管家浮动按钮与抽屉只在平台启用了管家、桌面端、且不在 /butler 全页时出现（D13） */
const showButler = computed(() => butlerAvailable.value && !mobile.value && route.path !== '/butler')
const active = computed(() => tabs.value.find((t) => route.path === t.path || route.path.startsWith(t.path + '/'))?.key ?? '')
const logo = import.meta.env.BASE_URL + brand.logoUrl
</script>

<template>
  <div class="shell" :class="{ mobile }" data-testid="layout">
    <aside v-if="!mobile" class="side" data-testid="layout-side">
      <div class="logo"><img :src="logo" alt="" width="28" height="28" /><span>{{ brand.name }}</span></div>
      <nav>
        <router-link v-for="t in tabs" :key="t.key" :to="t.path" class="menu-item" :class="{ active: active === t.key }" :data-testid="`menu-${t.key}`">
          <el-icon><component :is="t.icon" /></el-icon>{{ t.label }}
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
        <el-icon :size="20"><component :is="t.icon" /></el-icon><span>{{ t.short }}</span>
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
.menu-item { display: flex; align-items: center; gap: 8px; padding: 12px 16px; color: #303133; text-decoration: none; }
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
.tabbar a {
  flex: 1; min-width: 0; display: flex; flex-direction: column; align-items: center; justify-content: center; gap: 2px; min-height: 44px;
  color: #606266; text-decoration: none; font-size: 12px; white-space: nowrap;
}
.tabbar a.active { color: var(--el-color-primary); }
</style>
