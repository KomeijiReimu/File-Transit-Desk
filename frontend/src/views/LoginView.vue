<script setup lang="ts">
import { computed, nextTick, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ApiError } from '@/api'
import { adminLogin, authState, login, restoreSession } from '@/auth'
import AppIcon from '@/components/AppIcon.vue'

const router = useRouter()
const route = useRoute()
const mode = ref<'totp' | 'admin'>('totp')
const code = ref('')
const username = ref('')
const password = ref('')
const loading = ref(false)
const reconnecting = ref(false)
const error = ref('')
const reconnectError = ref('')
const reconnectAnnouncement = ref('')
const codeError = ref('')
const usernameError = ref('')
const passwordError = ref('')
const codeInput = ref<HTMLInputElement | null>(null)
const usernameInput = ref<HTMLInputElement | null>(null)
const passwordInput = ref<HTMLInputElement | null>(null)
const totpTab = ref<HTMLButtonElement | null>(null)
const adminTab = ref<HTMLButtonElement | null>(null)
const reconnectButton = ref<HTMLButtonElement | null>(null)

const normalized = computed(() => code.value.replace(/\D/g, '').slice(0, 6))
const totpValid = computed(() => normalized.value.length === 6)
const adminValid = computed(() => username.value.trim().length > 0 && password.value.length > 0)
const sessionUnavailable = computed(() => authState.status === 'unavailable')
const showUnavailableState = computed(() => sessionUnavailable.value || (reconnecting.value && authState.status === 'authenticated'))

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
watch(sessionUnavailable, (unavailable, wasUnavailable) => {
  reconnectError.value = ''
  void nextTick(() => {
    if (unavailable) reconnectButton.value?.focus()
    else if (wasUnavailable && authState.status === 'anonymous') {
      ;(mode.value === 'totp' ? codeInput.value : usernameInput.value)?.focus()
    }
  })
})

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

function reconnectFailureMessage(reason: unknown) {
  if (reason instanceof ApiError && reason.status === 0) {
    return '仍无法连接服务，请检查网络连接后重试。'
  }
  if (reason instanceof ApiError && reason.status >= 500) {
    return '服务仍未恢复，服务器暂时无法响应，请稍后重试。'
  }
  return '服务暂时无法完成会话检查，请稍后重试。'
}

async function reconnect() {
  if (reconnecting.value) return
  reconnecting.value = true
  reconnectError.value = ''
  reconnectAnnouncement.value = '正在重新连接服务。'
  try {
    const result = await restoreSession()
    if (result.status === 'authenticated') {
      reconnectAnnouncement.value = '连接已恢复，正在返回原页面。'
      await router.replace(safeRedirect(route.query.redirect))
      return
    }
    if (result.status === 'anonymous') {
      reconnectAnnouncement.value = '服务已恢复响应，请重新登录。'
      await nextTick()
      ;(mode.value === 'totp' ? codeInput.value : usernameInput.value)?.focus()
      return
    }
    const reason = 'error' in result ? result.error : undefined
    reconnectError.value = reconnectFailureMessage(reason)
    reconnectAnnouncement.value = '重新连接未成功。'
  } catch (reason) {
    reconnectError.value = reconnectFailureMessage(reason)
    reconnectAnnouncement.value = '重新连接未成功。'
  } finally {
    reconnecting.value = false
    if (authState.status === 'unavailable') {
      await nextTick()
      reconnectButton.value?.focus()
    }
  }
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
      <h1>安全访问文件传输台</h1>
    </section>

    <section v-if="showUnavailableState" class="login-card login-unavailable-card" aria-labelledby="service-unavailable-title">
      <div class="login-unavailable-heading">
        <span class="login-unavailable-icon" aria-hidden="true">
          <AppIcon name="alert-triangle" :size="25" />
        </span>
        <div>
          <h2 id="service-unavailable-title">服务暂时不可用</h2>
        </div>
      </div>

      <div class="login-unavailable-copy">
        <strong>登录状态尚未确认</strong>
        <p>网络连接或服务响应暂时中断。你的登录凭据可能仍然有效，无需立即重新输入密码或动态验证码。</p>
      </div>

      <p class="visually-hidden" aria-live="polite" aria-atomic="true">{{ reconnectAnnouncement }}</p>
      <p v-if="reconnectError" class="alert error login-reconnect-error" role="alert">{{ reconnectError }}</p>

      <button
        ref="reconnectButton"
        class="primary-btn full login-reconnect-btn"
        type="button"
        :disabled="reconnecting"
        :aria-busy="reconnecting"
        @click="reconnect"
      >
        <span v-if="reconnecting" class="login-reconnect-loader" aria-hidden="true" />
        {{ reconnecting ? '正在连接…' : '重新连接' }}
      </button>
    </section>

    <form v-else class="login-card" @submit.prevent="submit">
      <h2>{{ mode === 'totp' ? '登录文件传输台' : '管理员登录' }}</h2>

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

<style scoped>
.login-unavailable-card {
  position: relative;
  overflow: hidden;
  gap: 20px;
  background:
    radial-gradient(circle at 100% 0%, rgba(94, 224, 198, .13), transparent 42%),
    rgba(255, 255, 255, .1);
}

.login-unavailable-card::after {
  content: '';
  position: absolute;
  inset: auto -72px -96px auto;
  width: 220px;
  height: 220px;
  border-radius: 50%;
  background: radial-gradient(circle, rgba(255, 184, 77, .1), transparent 68%);
  pointer-events: none;
}

.login-unavailable-card > * {
  position: relative;
  z-index: 1;
}

.login-unavailable-heading {
  display: grid;
  grid-template-columns: 56px minmax(0, 1fr);
  gap: 15px;
  align-items: center;
}

.login-unavailable-icon {
  width: 56px;
  height: 56px;
  display: grid;
  place-items: center;
  border: 1px solid rgba(255, 184, 77, .3);
  border-radius: 19px;
  color: var(--accent);
  background: linear-gradient(145deg, rgba(255, 184, 77, .16), rgba(94, 224, 198, .08));
  box-shadow: 0 14px 34px rgba(0, 0, 0, .18), inset 0 1px 0 rgba(255, 255, 255, .09);
}

.login-unavailable-copy {
  padding: 16px 17px;
  border: 1px solid rgba(255, 255, 255, .11);
  border-radius: 18px;
  background: rgba(4, 7, 15, .22);
  box-shadow: inset 0 1px 0 rgba(255, 255, 255, .035);
}

.login-unavailable-copy strong {
  display: block;
  margin-bottom: 6px;
  color: var(--text);
  line-height: 1.4;
}

.login-unavailable-copy p {
  margin: 0;
  color: var(--muted);
  line-height: 1.65;
  text-wrap: pretty;
}

.login-reconnect-error {
  margin: 0;
}

.login-reconnect-btn {
  min-height: 48px;
}

.login-reconnect-loader {
  width: 17px;
  height: 17px;
  flex: 0 0 auto;
  border: 2px solid rgba(29, 18, 9, .25);
  border-top-color: #1d1209;
  border-radius: 50%;
  animation: login-reconnect-spin .8s linear infinite;
}

@keyframes login-reconnect-spin {
  to { transform: rotate(360deg); }
}

@media (max-width: 480px) {
  .login-unavailable-heading {
    grid-template-columns: 48px minmax(0, 1fr);
    gap: 12px;
  }

  .login-unavailable-icon {
    width: 48px;
    height: 48px;
    border-radius: 17px;
  }
}
</style>
