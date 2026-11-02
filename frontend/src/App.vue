<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import { authState, isAdmin, logout } from '@/auth'
import { authenticationEpoch, invalidateSessionSubject } from '@/authEpoch'
import AppIcon from '@/components/AppIcon.vue'
import { useSessionActivity } from '@/useSessionActivity'

const route = useRoute()

const chromeless = computed(() => route.name === 'login' || route.name === 'share')
const protectedSessionBinding = computed(() => authState.user?.sessionBinding?.trim() || '')
const protectedSubjectReady = computed(() => (
  authState.status === 'authenticated'
  && authState.authenticated
  && protectedSessionBinding.value !== ''
))
const displayName = computed(() => authState.name || (isAdmin.value ? '管理员' : '访客'))
const roleLabel = computed(() => (isAdmin.value ? '管理员' : '普通用户'))
const mobileNavOpen = ref(false)

watch(() => route.fullPath, () => { mobileNavOpen.value = false })

let missingBindingRevalidationRequested = false
watch(
  () => [authState.status, protectedSessionBinding.value] as const,
  ([status, binding]) => {
    if (binding) {
      missingBindingRevalidationRequested = false
      return
    }
    if (status === 'anonymous' || status === 'unavailable') {
      missingBindingRevalidationRequested = false
      return
    }
    if (status !== 'authenticated' || missingBindingRevalidationRequested) return
    missingBindingRevalidationRequested = true
    invalidateSessionSubject(authenticationEpoch(), undefined, 'session_subject_changed')
  },
  { flush: 'post', immediate: true },
)

// 在根组件统一挂载会话活跃监听，所有受保护页面共享同一套空闲保活策略。
useSessionActivity()

async function handleLogout() {
  if (!protectedSubjectReady.value) return
  await logout()
}
</script>

<template>
  <a class="skip-link" href="#main-content">跳到主要内容</a>
  <RouterView v-if="chromeless" />
  <div v-else class="app-shell">
    <aside class="sidebar" @keydown.esc="mobileNavOpen = false">
      <RouterLink class="brand" to="/files" aria-label="回到文件浏览">
        <span class="brand-mark">FT</span>
        <strong>文件传输台</strong>
      </RouterLink>

      <button
        class="mobile-nav-toggle ghost-btn"
        type="button"
        aria-controls="main-navigation"
        :aria-expanded="mobileNavOpen"
        :aria-label="mobileNavOpen ? '收起菜单' : '展开菜单'"
        @click="mobileNavOpen = !mobileNavOpen"
      >
        <AppIcon name="menu" :size="20" />
        <span>{{ mobileNavOpen ? '收起菜单' : '展开菜单' }}</span>
      </button>

      <nav id="main-navigation" class="nav-list" :class="{ 'is-open': mobileNavOpen }" aria-label="主导航">
        <RouterLink to="/files">
          <AppIcon class="nav-ico" name="folder" :size="18" />
          <span>文件</span>
        </RouterLink>
        <RouterLink to="/upload">
          <AppIcon class="nav-ico" name="arrow-up" :size="18" />
          <span>上传</span>
        </RouterLink>
        <RouterLink to="/chat">
          <AppIcon class="nav-ico" name="message-circle" :size="22" />
          <span>交流</span>
        </RouterLink>
        <template v-if="isAdmin">
          <span class="nav-section-label">管理</span>
          <RouterLink to="/tokens">
            <AppIcon class="nav-ico" name="link" :size="18" />
            <span>分享链接</span>
          </RouterLink>
          <RouterLink to="/transfers">
            <AppIcon class="nav-ico" name="download" :size="18" />
            <span>正在传输</span>
          </RouterLink>
          <RouterLink to="/audit">
            <AppIcon class="nav-ico" name="file" :size="18" />
            <span>访问记录</span>
          </RouterLink>
          <RouterLink to="/config">
            <AppIcon class="nav-ico" name="folder-cog" :size="18" />
            <span>设置</span>
          </RouterLink>
          <RouterLink to="/users">
            <AppIcon class="nav-ico" name="shield-check" :size="18" />
            <span>用户管理</span>
          </RouterLink>
        </template>
        <RouterLink v-if="authState.user?.authMethod === 'account'" to="/account">
          <AppIcon class="nav-ico" name="file-cog" :size="18" />
          <span>账号设置</span>
        </RouterLink>
      </nav>

      <div class="sidebar-card">
        <div class="avatar" :data-role="isAdmin ? 'admin' : 'user'">{{ displayName.slice(0, 1) }}</div>
        <div class="meta">
          <strong>{{ displayName }}</strong>
          <span class="sidebar-role-label">{{ roleLabel }}</span>
        </div>
        <span class="status-dot" title="已登录" />
      </div>
      <button class="ghost-btn full" type="button" :disabled="!protectedSubjectReady" @click="handleLogout">退出登录</button>
    </aside>

    <main id="main-content" class="content" tabindex="-1" data-route-focus>
      <RouterView
        v-if="protectedSubjectReady"
        :key="protectedSessionBinding"
      />
    </main>
  </div>
</template>
