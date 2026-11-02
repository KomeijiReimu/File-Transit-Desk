<script setup lang="ts">
import { ref } from 'vue'
import { useRouter } from 'vue-router'
import { ApiError, api } from '@/api'
import { authState, clearUser } from '@/auth'

const router = useRouter()
const currentPassword = ref('')
const newPassword = ref('')
const confirmation = ref('')
const error = ref('')
const saving = ref(false)
const confirmationInput = ref<HTMLInputElement | null>(null)

async function changePassword() {
  if (saving.value) return
  error.value = ''
  if (newPassword.value !== confirmation.value) {
    error.value = '两次输入的新密码不同。'
    confirmationInput.value?.focus()
    return
  }
  saving.value = true
  try {
    await api.changePassword({ currentPassword: currentPassword.value, newPassword: newPassword.value })
    currentPassword.value = ''
    newPassword.value = ''
    confirmation.value = ''
    clearUser()
    await router.replace({ name: 'login', query: { passwordChanged: '1' } })
  } catch (err) {
    error.value = err instanceof ApiError ? err.message : '密码修改失败，请重试。'
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <section class="page-stack account-layout">
    <header class="page-header"><h1>账号设置</h1></header>
    <div class="panel account-identity"><span class="avatar">{{ (authState.user?.name || authState.user?.username || '我').slice(0, 1) }}</span><div><h2>{{ authState.user?.name || authState.user?.username }}</h2><p v-if="authState.user?.name && authState.user.name !== authState.user.username">{{ authState.user?.username }}</p></div></div>
    <form v-if="authState.user?.authMethod === 'account'" class="panel form-grid" @submit.prevent="changePassword">
      <h2>修改密码</h2>
      <label>当前密码<input v-model="currentPassword" type="password" autocomplete="current-password" required :disabled="saving" /></label>
      <label>新密码<input v-model="newPassword" type="password" autocomplete="new-password" required :disabled="saving" /></label>
      <label>确认新密码<input ref="confirmationInput" v-model="confirmation" type="password" autocomplete="new-password" required :disabled="saving" /></label>
      <small>修改成功后，需要在所有设备上重新登录。</small>
      <div v-if="error" class="alert error" role="alert">{{ error }}</div>
      <div class="form-actions"><button class="primary-btn" type="submit" :disabled="saving">{{ saving ? '保存中…' : '修改密码并退出' }}</button></div>
    </form>
    <div v-else class="alert info">当前登录方式不支持在此修改密码。</div>
  </section>
</template>
