<script setup lang="ts">
import { computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { authState, isAdmin, logout } from '@/auth'
import { useSessionActivity } from '@/useSessionActivity'

const route = useRoute()
const router = useRouter()

const chromeless = computed(() => route.name === 'login' || route.name === 'share')
const displayName = computed(() => authState.name || (isAdmin.value ? '管理员' : '访客'))
const roleLabel = computed(() => (isAdmin.value ? '管理员会话' : '受信用户会话'))

// 在根组件统一挂载会话活跃监听，所有受保护页面共享同一套空闲保活策略。
useSessionActivity()

async function handleLogout() {
  await logout()
  router.replace({ name: 'login' })
}
</script>

<template>
  <RouterView v-if="chromeless" />
  <div v-else class="app-shell">
    <aside class="sidebar">
      <RouterLink class="brand" to="/files" aria-label="回到文件浏览">
        <span class="brand-mark">FT</span>
        <span>
          <strong>文件传输台</strong>
          <small>临时 · 私有 · 可审计</small>
        </span>
      </RouterLink>

      <nav class="nav-list" aria-label="主导航">
        <RouterLink to="/files">
          <span class="nav-ico nav-files" aria-hidden="true" />
          <span>文件浏览</span>
        </RouterLink>
        <RouterLink to="/upload">
          <span class="nav-ico nav-upload" aria-hidden="true" />
          <span>文件上传</span>
        </RouterLink>
        <template v-if="isAdmin">
          <RouterLink to="/tokens">
            <span class="nav-ico nav-tokens" aria-hidden="true" />
            <span>令牌管理</span>
          </RouterLink>
          <RouterLink to="/audit">
            <span class="nav-ico nav-audit" aria-hidden="true" />
            <span>访问记录</span>
          </RouterLink>
          <RouterLink to="/config">
            <span class="nav-ico nav-config" aria-hidden="true" />
            <span>配置管理</span>
          </RouterLink>
        </template>
      </nav>

      <div class="sidebar-card">
        <div class="avatar" :data-role="isAdmin ? 'admin' : 'user'">{{ displayName.slice(0, 1) }}</div>
        <div class="meta">
          <strong>{{ displayName }}</strong>
          <small>{{ roleLabel }}</small>
        </div>
        <span class="status-dot" :title="'在线'" />
      </div>
      <button class="ghost-btn full" type="button" @click="handleLogout">退出登录</button>
    </aside>

    <main class="content">
      <RouterView />
    </main>
  </div>
</template>
