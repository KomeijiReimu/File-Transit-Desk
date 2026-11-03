<script setup lang="ts">
import { computed, nextTick, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ApiError, api } from '@/api'
import { accountLogin, adminLogin, authState, login, restoreSession } from '@/auth'
import AppIcon from '@/components/AppIcon.vue'

const router = useRouter()
const route = useRoute()
type LoginMode = 'account' | 'totp' | 'admin'
const mode = ref<LoginMode>('account')
const options = ref<Awaited<ReturnType<typeof api.authOptions>> | null>(null)
const optionsLoading = ref(true)
const optionsError = ref('')
const registering = ref(false)
const registrationDone = ref<'pending' | 'active' | ''>('')
const code = ref('')
const username = ref('')
const displayName = ref('')
const password = ref('')
const confirmPassword = ref('')
const loading = ref(false)
const reconnecting = ref(false)
const error = ref('')
const reconnectError = ref('')
const reconnectAnnouncement = ref('')
const codeError = ref('')
const usernameError = ref('')
const passwordError = ref('')
const confirmationError = ref('')
const codeInput = ref<HTMLInputElement | null>(null)
const usernameInput = ref<HTMLInputElement | null>(null)
const passwordInput = ref<HTMLInputElement | null>(null)
const confirmationInput = ref<HTMLInputElement | null>(null)
const reconnectButton = ref<HTMLButtonElement | null>(null)
const registrationBackButton = ref<HTMLButtonElement | null>(null)
const tabsRef = ref<HTMLElement | null>(null)
const modes = computed<Array<{ value: LoginMode; label: string }>>(() => [
  { value: 'account', label: '账号登录' },
  ...(options.value?.totpEnabled ? [{ value: 'totp' as const, label: '动态验证码' }] : []),
  { value: 'admin', label: '管理员' },
])
const sessionUnavailable = computed(() => authState.status === 'unavailable')
const showUnavailableState = computed(() => sessionUnavailable.value || (reconnecting.value && authState.status === 'authenticated'))

async function loadOptions() {
  optionsLoading.value = true
  optionsError.value = ''
  try {
    options.value = await api.authOptions()
    if (mode.value === 'totp' && !options.value.totpEnabled) mode.value = 'account'
    if (!options.value.registrationEnabled) registering.value = false
  } catch (err) {
    optionsError.value = err instanceof ApiError ? err.message : '无法获取登录方式，请重试。'
  } finally {
    optionsLoading.value = false
  }
}

function safeRedirect(value: unknown) {
  const target = typeof value === 'string' ? value : '/files'
  return target.startsWith('/') && !target.startsWith('//') ? target : '/files'
}

function setMode(nextMode: LoginMode, focusTab = false) {
  mode.value = nextMode
  registering.value = false
  error.value = ''
  codeError.value = ''
  usernameError.value = ''
  passwordError.value = ''
  password.value = ''
  confirmPassword.value = ''
  void nextTick(() => {
    if (focusTab) tabsRef.value?.querySelector<HTMLButtonElement>(`[data-mode="${nextMode}"]`)?.focus()
    else (nextMode === 'totp' ? codeInput.value : usernameInput.value)?.focus()
  })
}

function handleTabKeydown(event: KeyboardEvent) {
  if (!['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(event.key)) return
  event.preventDefault()
  const list = modes.value
  const index = list.findIndex((item) => item.value === mode.value)
  const next = event.key === 'Home' ? 0 : event.key === 'End' ? list.length - 1 : (index + (event.key === 'ArrowRight' ? 1 : -1) + list.length) % list.length
  setMode(list[next].value, true)
}

async function reconnect() {
  if (reconnecting.value) return
  reconnecting.value = true
  reconnectError.value = ''
  reconnectAnnouncement.value = '正在重新连接。'
  try {
    const result = await restoreSession()
    if (result.status === 'authenticated') {
      reconnectAnnouncement.value = '连接已恢复。'
      await router.replace(safeRedirect(route.query.redirect))
    } else if (result.status === 'anonymous') {
      reconnectAnnouncement.value = '连接已恢复，请登录。'
      if (!options.value) await loadOptions()
      await nextTick()
      usernameInput.value?.focus()
    } else {
      reconnectError.value = '仍无法连接服务，请稍后重试。'
    }
  } catch (err) {
    reconnectError.value = err instanceof ApiError ? err.message : '连接失败，请重试。'
  } finally {
    reconnecting.value = false
    if (authState.status === 'unavailable') {
      await nextTick()
      reconnectButton.value?.focus()
    }
  }
}

async function submit() {
  if (loading.value || optionsLoading.value || !options.value) return
  error.value = ''
  codeError.value = ''
  usernameError.value = ''
  passwordError.value = ''
  confirmationError.value = ''
  if (mode.value === 'totp') {
    code.value = code.value.replace(/\D/g, '').slice(0, 6)
    if (code.value.length !== 6) {
      codeError.value = '请输入 6 位动态验证码。'
      codeInput.value?.focus()
      return
    }
  } else {
    usernameError.value = username.value.trim() ? '' : '请输入账号。'
    passwordError.value = password.value ? '' : '请输入密码。'
    if (registering.value && password.value !== confirmPassword.value) confirmationError.value = '两次输入的密码不同。'
    if (usernameError.value || passwordError.value || confirmationError.value) {
      await nextTick()
      ;(usernameError.value ? usernameInput.value : passwordError.value ? passwordInput.value : confirmationInput.value)?.focus()
      return
    }
  }
  loading.value = true
  try {
    if (registering.value) {
      const result = await api.register({ username: username.value.trim(), password: password.value, displayName: displayName.value.trim() || undefined })
      registrationDone.value = result.status
      password.value = ''
      confirmPassword.value = ''
      await nextTick()
      registrationBackButton.value?.focus()
    } else {
      if (mode.value === 'totp') await login(code.value)
      else if (mode.value === 'admin') await adminLogin(username.value.trim(), password.value)
      else await accountLogin(username.value.trim(), password.value)
      await router.replace(safeRedirect(route.query.redirect))
    }
  } catch (err) {
    error.value = err instanceof ApiError ? err.message : registering.value ? '注册失败，请重试。' : '登录失败，请重试。'
  } finally {
    loading.value = false
  }
}

watch(code, () => { codeError.value = '' })
watch(username, () => { usernameError.value = '' })
watch(password, () => { passwordError.value = ''; confirmationError.value = '' })
watch(confirmPassword, () => { confirmationError.value = '' })
watch(sessionUnavailable, (unavailable) => {
  reconnectError.value = ''
  if (unavailable) void nextTick(() => reconnectButton.value?.focus())
})
onMounted(loadOptions)
</script>

<template>
  <main id="main-content" class="login-page" tabindex="-1" data-route-focus>
    <div class="login-layout">
      <header class="login-identity">
        <div class="brand"><span class="brand-mark">FT</span><strong>文件传输台</strong></div>
        <div class="login-wordmark" aria-hidden="true">文件<br />传输台<span class="login-wordmark-rule" /></div>
        <div class="login-document-mark" aria-hidden="true"><span /><span /><span /></div>
      </header>
      <div class="login-form-area">

      <section v-if="showUnavailableState" class="login-card login-unavailable-card" aria-labelledby="service-unavailable-title">
        <div class="login-unavailable-heading">
          <AppIcon name="wifi-off" :size="24" />
          <h2 id="service-unavailable-title">暂时无法连接</h2>
        </div>
        <div class="login-unavailable-copy"><p>请先重新连接，恢复后将确认登录状态；必要时需要重新登录。</p></div>
        <p class="visually-hidden" aria-live="polite">{{ reconnectAnnouncement }}</p>
        <p v-if="reconnectError" class="alert error" role="alert">{{ reconnectError }}</p>
        <button ref="reconnectButton" class="primary-btn full" type="button" :disabled="reconnecting" :aria-busy="reconnecting" @click="reconnect">{{ reconnecting ? '连接中…' : '重新连接' }}</button>
      </section>

      <section v-else-if="registrationDone" class="login-card" aria-labelledby="registration-result" role="status">
        <AppIcon name="shield-check" :size="28" />
        <h1 id="registration-result">{{ registrationDone === 'pending' ? '注册申请已提交' : '账号已创建' }}</h1>
        <p class="muted-text">{{ registrationDone === 'pending' ? '管理员批准后，即可使用此账号登录。' : '现在可以使用你的账号和密码登录。' }}</p>
        <button ref="registrationBackButton" class="primary-btn" type="button" @click="registrationDone = ''; setMode('account')">返回登录</button>
      </section>

      <form v-else class="login-card" @submit.prevent="submit">
        <h1>{{ registering ? '创建账号' : mode === 'admin' ? '管理员登录' : '登录' }}</h1>
        <div v-if="route.query.passwordChanged === '1'" class="alert success" role="status">密码已修改，请使用新密码登录。</div>
        <div v-if="optionsLoading" class="state-block" role="status"><span class="loader" />正在连接…</div>
        <div v-else-if="optionsError" class="alert error state-error" role="alert"><span>{{ optionsError }}</span><button class="ghost-btn" type="button" @click="loadOptions">重试</button></div>
        <template v-else>
          <div v-if="!registering" ref="tabsRef" class="tabs" role="tablist" aria-label="登录方式">
            <button v-for="item in modes" :id="`login-tab-${item.value}`" :key="item.value" class="tab" :class="{ active: mode === item.value }" type="button" role="tab" :data-mode="item.value" :aria-selected="mode === item.value" :aria-controls="`login-panel-${item.value}`" :tabindex="mode === item.value ? 0 : -1" :disabled="loading" @click="setMode(item.value)" @keydown="handleTabKeydown">{{ item.label }}</button>
          </div>

          <div :id="`login-panel-${mode}`" class="login-fields" :role="registering ? undefined : 'tabpanel'" :aria-labelledby="registering ? undefined : `login-tab-${mode}`">
            <label v-if="mode === 'totp'">动态验证码
              <input ref="codeInput" v-model="code" inputmode="numeric" autocomplete="one-time-code" maxlength="6" placeholder="6 位数字" :aria-invalid="Boolean(codeError)" aria-describedby="login-code-error" :disabled="loading" />
              <small v-if="codeError" id="login-code-error" class="field-error">{{ codeError }}</small>
            </label>
            <template v-else>
              <label>账号
                <input ref="usernameInput" v-model="username" autocomplete="username" autocapitalize="none" spellcheck="false" :aria-invalid="Boolean(usernameError)" aria-describedby="login-username-error" :disabled="loading" />
                <small v-if="usernameError" id="login-username-error" class="field-error">{{ usernameError }}</small>
              </label>
              <label v-if="registering">显示名称（选填）<input v-model="displayName" autocomplete="nickname" :disabled="loading" /></label>
              <label>密码
                <input ref="passwordInput" v-model="password" type="password" :autocomplete="registering ? 'new-password' : 'current-password'" :aria-invalid="Boolean(passwordError)" aria-describedby="login-password-error" :disabled="loading" />
                <small v-if="passwordError" id="login-password-error" class="field-error">{{ passwordError }}</small>
              </label>
              <label v-if="registering">确认密码
                <input ref="confirmationInput" v-model="confirmPassword" type="password" autocomplete="new-password" :aria-invalid="Boolean(confirmationError)" aria-describedby="login-confirm-error" :disabled="loading" />
                <small v-if="confirmationError" id="login-confirm-error" class="field-error">{{ confirmationError }}</small>
              </label>
            </template>
          </div>
          <p v-if="registering && options?.registrationApprovalRequired" class="muted-text" style="margin: 0; font-size: 12px">注册后需要管理员批准。</p>
          <p v-if="error" class="alert error" role="alert" style="margin: 0">{{ error }}</p>
          <button class="primary-btn full" :disabled="loading" type="submit">{{ loading ? '提交中…' : registering ? '注册' : '登录' }}</button>
          <div v-if="options?.registrationEnabled && mode === 'account'" class="login-footer">
            <span>{{ registering ? '已有账号？' : '还没有账号？' }}</span>
            <button class="text-btn" type="button" :disabled="loading" @click="registering = !registering; mode = 'account'; error = ''; password = ''; confirmPassword = ''; nextTick(() => usernameInput?.focus())">{{ registering ? '返回登录' : '注册账号' }}</button>
          </div>
        </template>
      </form>
      </div>
    </div>
  </main>
</template>
