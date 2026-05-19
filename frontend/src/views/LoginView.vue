<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ApiError } from '@/api'
import { adminLogin, login } from '@/auth'

const router = useRouter()
const route = useRoute()
const mode = ref<'totp' | 'admin'>('totp')
const code = ref('')
const username = ref('')
const password = ref('')
const loading = ref(false)
const error = ref('')

const normalized = computed(() => code.value.replace(/\D/g, '').slice(0, 6))
const totpValid = computed(() => normalized.value.length === 6)
const adminValid = computed(() => username.value.trim().length > 0 && password.value.length > 0)
const canSubmit = computed(() => (mode.value === 'totp' ? totpValid.value : adminValid.value))

watch(mode, () => {
  error.value = ''
})

function safeRedirect(value: unknown) {
  const target = typeof value === 'string' ? value : '/files'
  if (!target.startsWith('/') || target.startsWith('//')) return '/files'
  return target
}

async function submit() {
  error.value = ''
  if (mode.value === 'totp') {
    code.value = normalized.value
    if (!totpValid.value) {
      error.value = '请输入 6 位动态验证码。'
      return
    }
  } else if (!adminValid.value) {
    error.value = '请输入管理员账号与密码。'
    return
  }
  loading.value = true
  try {
    if (mode.value === 'totp') {
      await login(code.value)
    } else {
      await adminLogin(username.value.trim(), password.value)
    }
    router.replace(safeRedirect(route.query.redirect))
  } catch (err) {
    error.value = err instanceof ApiError ? err.message : '登录失败，请确认输入后重试。'
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <main class="login-page">
    <section class="login-hero">
      <p class="eyebrow">文件传输台</p>
      <h1>安全访问文件传输台</h1>
    </section>

    <form class="login-card" @submit.prevent="submit">
      <div>
        <p class="eyebrow">{{ mode === 'totp' ? 'TOTP 验证' : 'Admin Console' }}</p>
        <h2>{{ mode === 'totp' ? '登录文件传输台' : '管理员登录' }}</h2>
      </div>

      <div class="tabs" role="tablist" aria-label="登录方式">
        <button
          type="button"
          role="tab"
          :aria-selected="mode === 'totp'"
          :class="['tab', { active: mode === 'totp' }]"
          @click="mode = 'totp'"
        >动态验证码</button>
        <button
          type="button"
          role="tab"
          :aria-selected="mode === 'admin'"
          :class="['tab', { active: mode === 'admin' }]"
          @click="mode = 'admin'"
        >管理员账号</button>
      </div>

      <template v-if="mode === 'totp'">
        <label>
          动态验证码
          <input
            v-model="code"
            inputmode="numeric"
            autocomplete="one-time-code"
            maxlength="6"
            placeholder="例如 123456"
            autofocus
          />
        </label>
      </template>

      <template v-else>
        <label>
          管理员账号
          <input
            v-model="username"
            autocomplete="username"
            placeholder="例如 admin"
            autofocus
          />
        </label>
        <label>
          密码
          <input
            v-model="password"
            type="password"
            autocomplete="current-password"
            placeholder="••••••••"
          />
        </label>
      </template>

      <p v-if="error" class="alert error">{{ error }}</p>
      <button class="primary-btn full" :disabled="loading || !canSubmit" type="submit">
        {{ loading ? '正在验证…' : '登录' }}
      </button>
    </form>
  </main>
</template>
