<script setup lang="ts">
import { computed, nextTick, ref, watch } from 'vue'
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
const codeError = ref('')
const usernameError = ref('')
const passwordError = ref('')
const codeInput = ref<HTMLInputElement | null>(null)
const usernameInput = ref<HTMLInputElement | null>(null)
const passwordInput = ref<HTMLInputElement | null>(null)
const totpTab = ref<HTMLButtonElement | null>(null)
const adminTab = ref<HTMLButtonElement | null>(null)

const normalized = computed(() => code.value.replace(/\D/g, '').slice(0, 6))
const totpValid = computed(() => normalized.value.length === 6)
const adminValid = computed(() => username.value.trim().length > 0 && password.value.length > 0)

watch(mode, () => {
  error.value = ''
  codeError.value = ''
  usernameError.value = ''
  passwordError.value = ''
  void nextTick(() => (mode.value === 'totp' ? codeInput.value : usernameInput.value)?.focus())
})
watch(code, () => { codeError.value = '' })
watch(username, () => { usernameError.value = '' })
watch(password, () => { passwordError.value = '' })

function setMode(nextMode: 'totp' | 'admin', focusTab = false) {
  mode.value = nextMode
  if (focusTab) void nextTick(() => (nextMode === 'totp' ? totpTab.value : adminTab.value)?.focus())
}

function handleTabKeydown(event: KeyboardEvent) {
  if (!['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(event.key)) return
  event.preventDefault()
  if (event.key === 'Home') setMode('totp', true)
  else if (event.key === 'End') setMode('admin', true)
  else setMode(mode.value === 'totp' ? 'admin' : 'totp', true)
}

function safeRedirect(value: unknown) {
  // 登录后只允许跳回站内相对路径，避免 redirect 参数被构造成开放跳转。
  const target = typeof value === 'string' ? value : '/files'
  if (!target.startsWith('/') || target.startsWith('//')) return '/files'
  return target
}

async function submit() {
  error.value = ''
  codeError.value = ''
  usernameError.value = ''
  passwordError.value = ''
  if (mode.value === 'totp') {
    // TOTP 只保留数字，允许用户从验证器复制带空格的验证码。
    code.value = normalized.value
    if (!totpValid.value) {
      codeError.value = '请输入 6 位动态验证码。'
      await nextTick()
      codeInput.value?.focus()
      return
    }
  } else if (!adminValid.value) {
    usernameError.value = username.value.trim() ? '' : '请输入管理员账号。'
    passwordError.value = password.value ? '' : '请输入管理员密码。'
    await nextTick()
    if (usernameError.value) usernameInput.value?.focus()
    else passwordInput.value?.focus()
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
  <main id="main-content" class="login-page" tabindex="-1" data-route-focus>
    <section class="login-hero">
      <p class="eyebrow">文件传输台</p>
      <h1>安全访问文件传输台</h1>
    </section>

    <form class="login-card" @submit.prevent="submit">
      <div>
        <p class="eyebrow">{{ mode === 'totp' ? '动态验证码' : '管理员入口' }}</p>
        <h2>{{ mode === 'totp' ? '登录文件传输台' : '管理员登录' }}</h2>
      </div>

      <div class="tabs" role="tablist" aria-label="登录方式">
        <button
          id="login-tab-totp"
          ref="totpTab"
          type="button"
          role="tab"
          :aria-selected="mode === 'totp'"
          aria-controls="login-panel-totp"
          :tabindex="mode === 'totp' ? 0 : -1"
          :class="['tab', { active: mode === 'totp' }]"
          @click="setMode('totp')"
          @keydown="handleTabKeydown"
        >动态验证码</button>
        <button
          id="login-tab-admin"
          ref="adminTab"
          type="button"
          role="tab"
          :aria-selected="mode === 'admin'"
          aria-controls="login-panel-admin"
          :tabindex="mode === 'admin' ? 0 : -1"
          :class="['tab', { active: mode === 'admin' }]"
          @click="setMode('admin')"
          @keydown="handleTabKeydown"
        >管理员账号</button>
      </div>

      <div v-if="mode === 'totp'" id="login-panel-totp" role="tabpanel" aria-labelledby="login-tab-totp">
        <label>
          动态验证码
          <input
            ref="codeInput"
            v-model="code"
            inputmode="numeric"
            autocomplete="one-time-code"
            maxlength="6"
            placeholder="例如 123456"
            :aria-invalid="Boolean(codeError)"
            aria-describedby="login-code-error"
            autofocus
          />
          <small v-if="codeError" id="login-code-error" class="field-error">{{ codeError }}</small>
        </label>
      </div>

      <div v-else id="login-panel-admin" class="login-fields" role="tabpanel" aria-labelledby="login-tab-admin">
        <label>
          管理员账号
          <input
            ref="usernameInput"
            v-model="username"
            autocomplete="username"
            placeholder="例如 admin"
            :aria-invalid="Boolean(usernameError)"
            aria-describedby="login-username-error"
            autofocus
          />
          <small v-if="usernameError" id="login-username-error" class="field-error">{{ usernameError }}</small>
        </label>
        <label>
          密码
          <input
            ref="passwordInput"
            v-model="password"
            type="password"
            autocomplete="current-password"
            placeholder="••••••••"
            :aria-invalid="Boolean(passwordError)"
            aria-describedby="login-password-error"
          />
          <small v-if="passwordError" id="login-password-error" class="field-error">{{ passwordError }}</small>
        </label>
      </div>

      <p v-if="error" class="alert error" role="alert">{{ error }}</p>
      <button class="primary-btn full" :disabled="loading" type="submit">
        {{ loading ? '正在验证…' : '登录' }}
      </button>
    </form>
  </main>
</template>
