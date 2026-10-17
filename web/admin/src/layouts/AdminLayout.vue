<script setup lang="ts">
import { brand, isApiError, labelOf, messageOf, roleLabel } from '@spark/shared'
import { ElMessage } from 'element-plus'
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api } from '../api'
import { menuFor } from '../menu'
import { refreshUnresolved, unresolvedAlerts } from '../store/alerts'
import { clearSession, session } from '../store/session'

const route = useRoute()
const router = useRouter()

// 侧边栏折叠是 localStorage 里唯一保存的东西（界面偏好）；隐私模式下读写可能抛错，按未折叠处理
const COLLAPSE_KEY = 'sc_admin_sidebar_collapsed'
function readCollapsed(): boolean {
  try {
    return localStorage.getItem(COLLAPSE_KEY) === '1'
  } catch {
    return false
  }
}
const collapsed = ref(readCollapsed())
function toggle() {
  collapsed.value = !collapsed.value
  try {
    localStorage.setItem(COLLAPSE_KEY, collapsed.value ? '1' : '0')
  } catch {
    /* 偏好保存失败不影响使用 */
  }
}

const items = computed(() => (session.admin ? menuFor(session.admin.role) : []))
const active = computed(() => items.value.find((m) => (m.path === '/' ? route.path === '/' : route.path === m.path || route.path.startsWith(m.path + '/')))?.key ?? '')
const logo = import.meta.env.BASE_URL + brand.logoUrl

// 未处理告警角标（store/alerts.ts）：进入时取一次，之后 60 秒刷新一次 total；失败保留上次的数、不提示、不立即重试，等下一个周期
let timer: ReturnType<typeof setInterval> | undefined
onMounted(() => {
  void refreshUnresolved()
  timer = setInterval(refreshUnresolved, 60_000)
})
onUnmounted(() => clearInterval(timer))

// 退出请求失败（网络 / 5xx）时服务器上的会话仍然有效：保持登录状态并提示重试，
// 而不是只清本地状态——那样界面显示已退出，刷新一下又自动登录回来。401 说明会话已失效，照常回登录页。
const loggingOut = ref(false)
async function logout() {
  if (loggingOut.value) return
  loggingOut.value = true
  try {
    await api.post('/auth/logout')
  } catch (e) {
    if (!(isApiError(e) && e.code === 'unauthorized')) {
      ElMessage.error(`退出失败：${messageOf(e)}。服务器上的登录可能仍然有效，请重试退出；如仍失败请关闭浏览器`)
      loggingOut.value = false
      return
    }
  }
  clearSession()
  await router.replace('/login')
}
</script>

<template>
  <div class="shell">
    <aside class="side" :class="{ collapsed }" data-testid="layout-side">
      <div class="logo"><img :src="logo" alt="" width="28" height="28" /><span v-if="!collapsed">{{ brand.name }}</span></div>
      <nav>
        <router-link v-for="m in items" :key="m.key" :to="m.path" class="menu-item" :class="{ active: active === m.key }" :title="m.label" :data-testid="`menu-${m.key}`">
          <span class="label">{{ collapsed ? m.label.slice(0, 2) : m.label }}</span>
          <el-badge v-if="m.key === 'alerts' && unresolvedAlerts > 0" :value="unresolvedAlerts" :max="99" class="badge" data-testid="menu-alerts-badge" />
        </router-link>
      </nav>
      <button class="collapse" type="button" :title="collapsed ? '展开菜单' : '收起菜单'" data-testid="layout-collapse" @click="toggle">{{ collapsed ? '»' : '«' }}</button>
    </aside>
    <div class="main">
      <header class="topbar">
        <span class="title">{{ route.meta.title }}</span>
        <span class="right">
          <span data-testid="layout-username">{{ session.admin?.username }}</span>
          <el-tag size="small" data-testid="layout-role">{{ labelOf(roleLabel, session.admin?.role) }}</el-tag>
          <router-link to="/settings" data-testid="layout-settings">个人设置</router-link>
          <el-button link :loading="loggingOut" data-testid="layout-logout" @click="logout">退出</el-button>
        </span>
      </header>
      <main class="content"><slot /></main>
    </div>
  </div>
</template>

<style scoped>
.shell { display: flex; min-height: 100vh; }
.side { width: 200px; background: #1f2d3d; color: #cfd8e3; display: flex; flex-direction: column; flex-shrink: 0; }
.side.collapsed { width: 64px; }
.logo { display: flex; align-items: center; gap: 8px; padding: 16px; color: #fff; font-weight: 600; }
.menu-item { display: flex; align-items: center; justify-content: space-between; padding: 10px 16px; color: #cfd8e3; text-decoration: none; }
.menu-item.active { background: var(--el-color-primary); color: #fff; }
.collapse { margin-top: auto; background: none; border: 0; color: #cfd8e3; padding: 12px; cursor: pointer; }
.main { flex: 1; display: flex; flex-direction: column; min-width: 0; }
.topbar { height: 56px; display: flex; align-items: center; justify-content: space-between; padding: 0 24px; background: #fff; border-bottom: 1px solid #ebeef5; }
.title { font-weight: 600; }
.right { display: flex; align-items: center; gap: 12px; }
.content { flex: 1; padding: 24px; }
</style>
