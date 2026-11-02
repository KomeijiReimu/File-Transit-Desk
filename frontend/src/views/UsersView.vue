<script setup lang="ts">
import { computed, nextTick, onMounted, reactive, ref } from 'vue'
import { ApiError, api } from '@/api'
import type { AccountUser } from '@/types'
import ConfirmDialog from '@/components/ConfirmDialog.vue'
import EmptyState from '@/components/EmptyState.vue'
import StateBlock from '@/components/StateBlock.vue'
import { formatDate } from '@/utils'

const users = ref<AccountUser[]>([])
const total = ref(0)
const page = ref(1)
const pageSize = 20
const query = ref('')
const status = ref<AccountUser['status'] | ''>('')
const appliedFilters = reactive<{ query: string; status: AccountUser['status'] | '' }>({ query: '', status: '' })
const loading = ref(false)
const saving = ref(false)
const error = ref('')
const notice = ref('')
const formOpen = ref(false)
const formError = ref('')
const form = reactive({ username: '', displayName: '', password: '' })
const resetUser = ref<AccountUser | null>(null)
const resetPassword = ref('')
const resetInput = ref<HTMLInputElement | null>(null)
const usernameInput = ref<HTMLInputElement | null>(null)
const listTitle = ref<HTMLHeadingElement | null>(null)
const pending = ref<{ kind: 'disable' | 'delete' | 'reset'; user: AccountUser } | null>(null)
const actionError = ref('')
const totalPages = computed(() => Math.max(1, Math.ceil(total.value / pageSize)))
const statusLabels = { pending: '待批准', active: '正常', disabled: '已停用', deleted: '已删除' }
const dialogTitle = computed(() => pending.value?.kind === 'delete' ? '删除此账号？' : pending.value?.kind === 'reset' ? '重置此账号的密码？' : '停用此账号？')
const dialogMessage = computed(() => {
  const accountEffect = pending.value?.kind === 'delete'
    ? '此账号将无法再登录，历史记录会保留。删除无法撤销。'
    : pending.value?.kind === 'reset'
      ? '该用户将退出登录，需要使用新密码重新登录。'
      : '该用户将退出登录，并且无法继续登录。你可以稍后重新启用。'
  return `${accountEffect}尚未开始的上传，以及后续下载和断点续传的授权会被撤销；已获准开始的当前连接可以完成。`
})

async function load(nextPage = page.value) {
  if (loading.value) return
  loading.value = true
  error.value = ''
  try {
    const result = await api.users({ query: appliedFilters.query, status: appliedFilters.status || undefined, page: nextPage, pageSize })
    users.value = result.users
    total.value = result.total
    page.value = result.page
  } catch (err) {
    error.value = `${err instanceof ApiError ? err.message : '用户列表加载失败，请重试。'}${users.value.length ? ' 当前显示的是上次加载的结果。' : ''}`
  } finally {
    loading.value = false
  }
}

function searchUsers() {
  if (loading.value) return
  appliedFilters.query = query.value.trim()
  appliedFilters.status = status.value
  void load(1)
}

async function createUser() {
  if (saving.value) return
  saving.value = true
  formError.value = ''
  notice.value = ''
  try {
    await api.createUser({ username: form.username.trim(), displayName: form.displayName.trim() || undefined, password: form.password, status: 'active' })
    Object.assign(form, { username: '', displayName: '', password: '' })
    formOpen.value = false
    notice.value = '账号已创建。'
    await load(1)
    await nextTick()
    listTitle.value?.focus()
  } catch (err) {
    formError.value = err instanceof ApiError ? err.message : '创建失败，请重试。'
  } finally {
    saving.value = false
  }
}

async function activateUser(user: AccountUser) {
  if (saving.value) return
  saving.value = true
  error.value = ''
  notice.value = ''
  try {
    const saved = await api.updateUser(user.id, { status: 'active' })
    users.value = users.value.map((item) => item.id === saved.id ? saved : item)
    notice.value = user.status === 'pending' ? '注册申请已批准。' : '账号已启用。'
    await load()
    await nextTick()
    listTitle.value?.focus()
  } catch (err) {
    error.value = err instanceof ApiError ? err.message : '操作失败，请重试。'
  } finally {
    saving.value = false
  }
}

function requestReset() {
  if (!resetUser.value || !resetPassword.value) return
  actionError.value = ''
  pending.value = { kind: 'reset', user: resetUser.value }
}

async function confirmAction() {
  if (!pending.value || saving.value) return
  const action = pending.value
  saving.value = true
  actionError.value = ''
  notice.value = ''
  try {
    if (action.kind === 'delete') await api.deleteUser(action.user.id)
    else if (action.kind === 'disable') await api.updateUser(action.user.id, { status: 'disabled' })
    else {
      await api.resetUserPassword(action.user.id, resetPassword.value)
      resetPassword.value = ''
      resetUser.value = null
    }
    notice.value = action.kind === 'delete' ? '账号已删除。' : action.kind === 'disable' ? '账号已停用。' : '密码已重置。'
    if (action.kind === 'delete' && resetUser.value?.id === action.user.id) {
      resetUser.value = null
      resetPassword.value = ''
    }
    pending.value = null
    await load(action.kind === 'delete' && users.value.length === 1 ? Math.max(1, page.value - 1) : page.value)
    await nextTick()
    listTitle.value?.focus()
  } catch (err) {
    actionError.value = err instanceof ApiError ? err.message : '操作失败，请重试。'
  } finally {
    saving.value = false
  }
}

onMounted(() => load())
</script>

<template>
  <section class="page-stack">
    <header class="page-header"><h1 ref="listTitle" tabindex="-1">用户管理<span class="count-badge">{{ total }}</span></h1><button class="primary-btn" type="button" :disabled="saving" @click="formOpen = !formOpen; formError = ''; form.password = ''; nextTick(() => usernameInput?.focus())">{{ formOpen ? '收起' : '创建账号' }}</button></header>
    <div v-if="notice" class="alert success" role="status">{{ notice }}</div>

    <form v-if="formOpen" class="panel form-grid" @submit.prevent="createUser">
      <h2>创建账号</h2>
      <div class="inline-fields">
        <label>账号<input ref="usernameInput" v-model="form.username" autocomplete="off" autocapitalize="none" spellcheck="false" required :disabled="saving" /></label>
        <label>显示名称（选填）<input v-model="form.displayName" autocomplete="off" :disabled="saving" /></label>
      </div>
      <label>初始密码<input v-model="form.password" type="password" autocomplete="new-password" required :disabled="saving" /></label>
      <div v-if="formError" class="alert error" role="alert">{{ formError }}</div>
      <div class="form-actions"><button class="ghost-btn" type="button" :disabled="saving" @click="formOpen = false; form.password = ''">取消</button><button class="primary-btn" type="submit" :disabled="saving">{{ saving ? '创建中…' : '创建账号' }}</button></div>
    </form>

    <form v-if="resetUser" class="panel form-grid" @submit.prevent="requestReset">
      <h2>重置 {{ resetUser.displayName || resetUser.username }} 的密码</h2>
      <label>新密码<input ref="resetInput" v-model="resetPassword" type="password" autocomplete="new-password" required :disabled="saving" /></label>
      <div class="form-actions"><button class="ghost-btn" type="button" :disabled="saving" @click="resetUser = null; resetPassword = ''">取消</button><button class="primary-btn" type="submit" :disabled="saving || !resetPassword">重置密码</button></div>
    </form>

    <form class="panel filter-bar" @submit.prevent="searchUsers">
      <label>搜索用户<input v-model="query" type="search" placeholder="账号或显示名称" :disabled="loading" /></label>
      <label>状态<select v-model="status" :disabled="loading"><option value="">全部用户</option><option value="pending">待批准</option><option value="active">正常</option><option value="disabled">已停用</option></select></label>
      <button class="ghost-btn" type="submit" :disabled="loading">搜索</button>
    </form>
    <StateBlock :loading="loading && !users.length" :error="error" retry-label="重试" @retry="load()" />
    <section class="table-card" aria-label="用户列表" :aria-busy="loading">
      <div v-if="users.length" class="table-scroll">
        <table class="data-table users-table">
          <thead><tr><th scope="col">账号</th><th scope="col">显示名称</th><th scope="col">状态</th><th scope="col">注册时间</th><th scope="col">操作</th></tr></thead>
          <tbody><tr v-for="user in users" :key="user.id">
            <td data-label="账号">{{ user.username }}</td><td data-label="显示名称">{{ user.displayName || '—' }}</td>
            <td data-label="状态"><span class="pill" :class="user.status === 'active' ? 'ok' : user.status === 'disabled' ? 'danger' : ''">{{ statusLabels[user.status] }}</span></td>
            <td data-label="注册时间">{{ formatDate(user.createdAt) }}</td>
            <td data-label="操作"><div v-if="user.status !== 'deleted'" class="row-actions">
              <button v-if="user.status === 'pending' || user.status === 'disabled'" class="mini-btn" type="button" :disabled="saving" @click="activateUser(user)">{{ user.status === 'pending' ? '批准' : '启用' }}</button>
              <button v-if="user.status === 'active'" class="mini-btn" type="button" :disabled="saving" @click="pending = { kind: 'disable', user }; actionError = ''">停用</button>
              <button class="mini-btn" type="button" :disabled="saving" @click="resetUser = user; resetPassword = ''; nextTick(() => resetInput?.focus())">重置密码</button>
              <button class="mini-btn danger" type="button" :disabled="saving" @click="pending = { kind: 'delete', user }; actionError = ''">删除</button>
            </div></td>
          </tr></tbody>
        </table>
      </div>
      <EmptyState v-else-if="!loading && !error" title="没有匹配的用户" />
      <footer v-if="total > pageSize" class="pagination-bar"><span>第 {{ page }} / {{ totalPages }} 页</span><div class="row-actions"><button class="mini-btn" type="button" :disabled="loading || page <= 1" @click="load(page - 1)">上一页</button><button class="mini-btn" type="button" :disabled="loading || page >= totalPages" @click="load(page + 1)">下一页</button></div></footer>
    </section>
    <ConfirmDialog :open="Boolean(pending)" :title="dialogTitle" :message="dialogMessage" :detail="pending?.user.username" :error="actionError" :loading="saving" :confirm-label="pending?.kind === 'delete' ? '删除账号' : pending?.kind === 'reset' ? '重置密码' : '停用账号'" danger @cancel="pending = null" @confirm="confirmAction" />
  </section>
</template>
