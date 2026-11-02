<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { onBeforeRouteLeave, useRouter } from 'vue-router'
import { ApiError, api } from '@/api'
import AppIcon from '@/components/AppIcon.vue'
import ConfirmDialog from '@/components/ConfirmDialog.vue'
import EmptyState from '@/components/EmptyState.vue'
import GlassSelect from '@/components/GlassSelect.vue'
import ServiceAddressesCard from '@/components/ServiceAddressesCard.vue'
import ServerFilePicker from '@/components/ServerFilePicker.vue'
import StateBlock from '@/components/StateBlock.vue'
import type { DirectoryInfo, FilePickerSelection, ResourcePayload, SafeConfig, UploadPolicyPayload } from '@/types'

const configData = ref<SafeConfig | null>(null)
const router = useRouter()
const loading = ref(true)
const saving = ref(false)
const policySaving = ref(false)
const error = ref('')
const loadError = ref('')
const lastSuccessfulAt = ref<Date | null>(null)
const dataStale = ref(false)
const success = ref('')
const editingId = ref<string | null>(null)
const pendingDelete = ref<DirectoryInfo | null>(null)
const pendingUploadPolicy = ref<UploadPolicyPayload | null>(null)
const deleteError = ref('')
const policyError = ref('')
const pickerOpen = ref(false)
const resourceFormOpen = ref(false)
const accountSaving = ref(false)
const accountError = ref('')
const accountPolicy = reactive({ registrationEnabled: false, registrationApprovalRequired: true, totpEnabled: false })
const accountBaseline = ref(JSON.stringify(accountPolicy))
const accountDirty = computed(() => JSON.stringify(accountPolicy) !== accountBaseline.value)
const resourceFormRef = ref<HTMLFormElement | null>(null)
const idInputRef = ref<HTMLInputElement | null>(null)
const nameInputRef = ref<HTMLInputElement | null>(null)
const pathInputRef = ref<HTMLInputElement | null>(null)
const permissionGroupRef = ref<HTMLFieldSetElement | null>(null)
const pendingEdit = ref<DirectoryInfo | null>(null)
const pendingDiscardAction = ref<'leave' | 'reset' | 'reload' | ''>('')
const pendingLeavePath = ref('')
let allowNextNavigation = false

const form = reactive<ResourcePayload>({
  id: '',
  name: '',
  type: 'directory',
  path: '',
  allowDownload: true,
  allowUpload: false,
})

const uploadPolicy = reactive({
  allowedText: '',
  blockedText: '',
})
const fieldErrors = reactive({ id: '', name: '', path: '', permissions: '' })

const resources = computed(() => configData.value?.resources || [])
const storage = computed(() => configData.value?.storage)
const tokens = computed(() => configData.value?.tokens)
const downloads = computed(() => configData.value?.downloads)
const editing = computed(() => Boolean(editingId.value))
const formSignature = computed(() => JSON.stringify(form))
const policySignature = computed(() => JSON.stringify(uploadPolicy))
const formBaseline = ref(formSignature.value)
const policyBaseline = ref(policySignature.value)
const formDirty = computed(() => formSignature.value !== formBaseline.value)
const policyDirty = computed(() => policySignature.value !== policyBaseline.value)
const hasUnsavedChanges = computed(() => formDirty.value || policyDirty.value || accountDirty.value)
const discardDialogOpen = computed(() => Boolean(pendingEdit.value) || Boolean(pendingDiscardAction.value))
const typeOptions = [
  { label: '目录', value: 'directory' },
  { label: '单文件', value: 'file' },
]

function resourceType(resource: DirectoryInfo) {
  return resource.type === 'file' ? 'file' : 'directory'
}

function resourceTypeLabel(resource: DirectoryInfo | ResourcePayload) {
  return resource.type === 'file' ? '单文件' : '目录'
}

function resetForm() {
  resourceFormOpen.value = false
  editingId.value = null
  Object.assign(form, { id: '', name: '', type: 'directory', path: '', allowDownload: true, allowUpload: false })
  Object.assign(fieldErrors, { id: '', name: '', path: '', permissions: '' })
  formBaseline.value = formSignature.value
}

function syncUploadPolicy() {
  uploadPolicy.allowedText = storage.value?.allowedExtensions.join('\n') || ''
  uploadPolicy.blockedText = storage.value?.blockedExtensions.join('\n') || ''
  policyBaseline.value = policySignature.value
}

function applyEditResource(resource: DirectoryInfo) {
  resourceFormOpen.value = true
  editingId.value = resource.id
  Object.assign(form, {
    id: resource.id,
    name: resource.name,
    type: resourceType(resource),
    path: resource.root || '',
    allowDownload: resource.allowDownload !== false && resource.canDownload !== false,
    allowUpload: resourceType(resource) === 'file' ? false : Boolean(resource.allowUpload || resource.canUpload),
  })
  success.value = ''
  error.value = ''
  Object.assign(fieldErrors, { id: '', name: '', path: '', permissions: '' })
  formBaseline.value = formSignature.value
  void nextTick(() => {
    const reduceMotion = window.matchMedia('(prefers-reduced-motion: reduce)').matches
    resourceFormRef.value?.scrollIntoView({ behavior: reduceMotion ? 'auto' : 'smooth', block: 'start' })
    nameInputRef.value?.focus({ preventScroll: true })
  })
}

function requestEditResource(resource: DirectoryInfo) {
  if (formDirty.value) {
    pendingEdit.value = resource
    pendingDiscardAction.value = ''
    return
  }
  applyEditResource(resource)
}

function requestResetForm() {
  if (!formDirty.value) {
    resetForm()
    return
  }
  pendingEdit.value = null
  pendingDiscardAction.value = 'reset'
}

function requestReload() {
  if (!hasUnsavedChanges.value) {
    void load()
    return
  }
  pendingEdit.value = null
  pendingDiscardAction.value = 'reload'
}

async function load(preserveEdits = false) {
  loading.value = true
  loadError.value = ''
  error.value = ''
  try {
    configData.value = await api.safeConfig()
    if (!preserveEdits || !policyDirty.value) syncUploadPolicy()
    if (!preserveEdits || !accountDirty.value) {
      Object.assign(accountPolicy, configData.value.accounts)
      accountBaseline.value = JSON.stringify(accountPolicy)
    }
    lastSuccessfulAt.value = new Date()
    dataStale.value = false
  } catch (err) {
    loadError.value = err instanceof ApiError ? err.message : '配置加载失败。'
    dataStale.value = lastSuccessfulAt.value !== null
  } finally {
    loading.value = false
  }
}

async function validateResourceForm() {
  fieldErrors.id = !form.id.trim()
    ? '请输入资源 ID。'
    : /^[a-zA-Z0-9_-]+$/.test(form.id.trim()) ? '' : '资源 ID 只能包含字母、数字、短横线和下划线。'
  fieldErrors.name = form.name.trim() ? '' : '请输入显示名称。'
  fieldErrors.path = form.path.trim() ? '' : '请输入或选择服务端路径。'
  fieldErrors.permissions = form.type === 'file' || form.allowDownload || form.allowUpload ? '' : '目录资源至少需要允许下载或上传其中一项。'
  await nextTick()
  if (fieldErrors.id) idInputRef.value?.focus()
  else if (fieldErrors.name) nameInputRef.value?.focus()
  else if (fieldErrors.path) pathInputRef.value?.focus()
  else if (fieldErrors.permissions) permissionGroupRef.value?.focus()
  return !Object.values(fieldErrors).some(Boolean)
}

function parseExtensions(text: string) {
  const values = text.split(/[\s,，;；]+/).map((item) => item.trim()).filter(Boolean)
  const normalized: string[] = []
  const seen = new Set<string>()
  for (const raw of values) {
    let ext = raw.toLowerCase()
    if (ext === '*') throw new Error('不支持使用 *；白名单留空即可表示允许所有未被黑名单阻断的扩展名。')
    if (!ext.startsWith('.')) ext = `.${ext}`
    if (!/^\.[a-z0-9][a-z0-9+_-]{0,31}$/.test(ext)) throw new Error(`扩展名格式不正确：${raw}`)
    if (!seen.has(ext)) {
      seen.add(ext)
      normalized.push(ext)
    }
  }
  return normalized
}

async function saveUploadPolicy(payload: UploadPolicyPayload) {
  policySaving.value = true
  policyError.value = ''
  success.value = ''
  try {
    const saved = await api.updateUploadPolicy(payload)
    if (configData.value) {
      configData.value.storage.allowedExtensions = saved.allowedExtensions
      configData.value.storage.blockedExtensions = saved.blockedExtensions
    }
    syncUploadPolicy()
    success.value = '上传限制已保存。'
  } catch (err) {
    policyError.value = err instanceof ApiError ? err.message : '上传扩展名策略保存失败。'
  } finally {
    policySaving.value = false
    pendingUploadPolicy.value = null
  }
}

function submitUploadPolicy() {
  policyError.value = ''
  let payload: UploadPolicyPayload
  try {
    payload = {
      allowedExtensions: parseExtensions(uploadPolicy.allowedText),
      blockedExtensions: parseExtensions(uploadPolicy.blockedText),
    }
  } catch (err) {
    policyError.value = err instanceof Error ? err.message : '扩展名格式不正确。'
    return
  }
  const overlap = payload.allowedExtensions.find((ext) => payload.blockedExtensions.includes(ext))
  if (overlap) {
    policyError.value = `${overlap} 不能同时出现在允许和阻断列表。`
    return
  }
  if ((configData.value?.storage.blockedExtensions.length ?? 0) > 0 && !payload.blockedExtensions.length) {
    pendingUploadPolicy.value = payload
    return
  }
  void saveUploadPolicy(payload)
}

function handlePick(selection: FilePickerSelection) {
  form.type = selection.type
  form.path = selection.absolutePath
  const leaf = selection.absolutePath.split(/[\\/]/).filter(Boolean).pop() || selection.rootId
  if (!form.name.trim()) form.name = leaf
  if (!form.id.trim()) form.id = leaf.replace(/\.[^.]+$/, '').replace(/[^a-zA-Z0-9_-]+/g, '-').replace(/^-+|-+$/g, '').slice(0, 48) || selection.rootId
  if (selection.type === 'file') {
    form.allowDownload = true
    form.allowUpload = false
  }
}

async function submitResource() {
  error.value = ''
  success.value = ''
  if (!(await validateResourceForm())) return
  const payload: ResourcePayload = { ...form }
  if (payload.type === 'file') {
    // 单文件资源只作为下载入口，避免把上传目标误写到文件路径上。
    payload.allowDownload = true
    payload.allowUpload = false
  }
  saving.value = true
  try {
    if (editingId.value) await api.updateResource(editingId.value, payload)
    else await api.createResource(payload)
    success.value = editingId.value ? '共享位置已更新。' : '共享位置已添加。'
    resetForm()
    await load(true)
  } catch (err) {
    error.value = err instanceof ApiError ? err.message : '保存共享资源失败。'
  } finally {
    saving.value = false
  }
}

function cancelDiscard() {
  pendingEdit.value = null
  pendingDiscardAction.value = ''
  pendingLeavePath.value = ''
}

function confirmDiscard() {
  const editTarget = pendingEdit.value
  const action = pendingDiscardAction.value
  const leavePath = pendingLeavePath.value
  cancelDiscard()
  if (editTarget) applyEditResource(editTarget)
  else if (action === 'reset') resetForm()
  else if (action === 'reload') {
    resetForm()
    void load()
  }
  else if (action === 'leave' && leavePath) {
    allowNextNavigation = true
    void router.push(leavePath)
  }
}

function handleBeforeUnload(event: BeforeUnloadEvent) {
  if (!hasUnsavedChanges.value) return
  event.preventDefault()
  event.returnValue = '配置尚未保存。'
}

async function confirmDelete() {
  const resource = pendingDelete.value
  if (!resource || saving.value) return
  deleteError.value = ''
  saving.value = true
  try {
    await api.deleteResource(resource.id)
    pendingDelete.value = null
    success.value = '共享位置已删除。'
    await load(true)
  } catch (err) {
    deleteError.value = err instanceof ApiError ? err.message : '删除共享资源失败。'
  } finally {
    saving.value = false
  }
}

async function saveAccountPolicy() {
  if (accountSaving.value || !configData.value?.configWritable) return
  accountSaving.value = true
  accountError.value = ''
  success.value = ''
  try {
    const saved = await api.updateAccountPolicy({ ...accountPolicy })
    configData.value.accounts = saved
    Object.assign(accountPolicy, saved)
    accountBaseline.value = JSON.stringify(accountPolicy)
    success.value = '登录与注册设置已保存。'
  } catch (err) {
    accountError.value = err instanceof ApiError ? err.message : '保存失败，请重试。'
  } finally {
    accountSaving.value = false
  }
}

watch(() => form.type, (type) => {
  if (type === 'file') {
    form.allowDownload = true
    form.allowUpload = false
  }
})
watch(() => form.id, () => { fieldErrors.id = '' })
watch(() => form.name, () => { fieldErrors.name = '' })
watch(() => form.path, () => { fieldErrors.path = '' })
watch(() => [form.allowDownload, form.allowUpload], () => { fieldErrors.permissions = '' })

onBeforeRouteLeave((to) => {
  if (allowNextNavigation || !hasUnsavedChanges.value) return true
  pendingEdit.value = null
  pendingDiscardAction.value = 'leave'
  pendingLeavePath.value = to.fullPath
  return false
})

onMounted(() => {
  void load()
  window.addEventListener('beforeunload', handleBeforeUnload)
})
onBeforeUnmount(() => window.removeEventListener('beforeunload', handleBeforeUnload))
</script>

<template>
  <section class="page-stack config-page">
    <header class="page-header split">
      <h1>设置</h1>
      <div class="header-actions">
        <button class="ghost-btn" type="button" :disabled="loading || saving || policySaving || accountSaving" @click="requestReload">刷新</button>
        <button v-if="!resourceFormOpen && configData" class="primary-btn" type="button" @click="resourceFormOpen = true; nextTick(() => nameInputRef?.focus())">添加共享位置</button>
      </div>
    </header>

    <StateBlock :loading="loading && !lastSuccessfulAt" :error="!lastSuccessfulAt ? loadError : ''" retry-label="重新加载" @retry="load" />
    <div v-if="dataStale" class="alert info stale-alert" role="status">
      <span>刷新失败，当前显示的是 {{ lastSuccessfulAt?.toLocaleTimeString() }} 获取的旧配置。</span>
      <button class="ghost-btn" type="button" :disabled="loading" @click="requestReload">立即重试</button>
    </div>
    <div v-if="error" class="alert error" role="alert">{{ error }}</div>
    <div v-if="success" class="alert success" role="status" aria-live="polite">{{ success }}</div>
    <div v-if="configData && !configData.configWritable" class="alert error" role="alert">当前配置无法在线保存。请检查配置文件；若仍使用旧版配置，请先完成迁移。</div>

    <div v-if="configData && resourceFormOpen">
      <form ref="resourceFormRef" class="panel form-grid resource-form" @submit.prevent="submitResource">
        <div class="form-title-row">
          <h2>{{ editing ? '编辑共享位置' : '添加共享位置' }}</h2>
          <button class="ghost-btn" type="button" @click="requestResetForm">取消</button>
        </div>
        <label>类型
          <GlassSelect v-model="form.type" :options="typeOptions" aria-label="选择资源类型" />
        </label>
        <div class="inline-fields">
          <label>标识
            <input ref="idInputRef" v-model.trim="form.id" :disabled="editing" placeholder="例如 photos 或 manual" :aria-invalid="Boolean(fieldErrors.id)" aria-describedby="resource-id-help resource-id-error" />
            <small id="resource-id-help">字母、数字、短横线或下划线</small>
            <small v-if="fieldErrors.id" id="resource-id-error" class="field-error">{{ fieldErrors.id }}</small>
          </label>
          <label>显示名称
            <input ref="nameInputRef" v-model.trim="form.name" placeholder="例如 照片 或 使用说明" :aria-invalid="Boolean(fieldErrors.name)" aria-describedby="resource-name-error" />
            <small v-if="fieldErrors.name" id="resource-name-error" class="field-error">{{ fieldErrors.name }}</small>
          </label>
        </div>
        <label>服务器路径
          <div class="path-picker-line">
            <input ref="pathInputRef" v-model.trim="form.path" :placeholder="form.type === 'file' ? '/data/manual.pdf' : '/data/photos'" :aria-invalid="Boolean(fieldErrors.path)" aria-describedby="resource-path-help resource-path-error" />
            <button class="ghost-btn" type="button" @click="pickerOpen = true">浏览</button>
          </div>
          <small id="resource-path-help">{{ form.type === 'file' ? '选择服务器上已有的文件。' : '选择服务器上已有的文件夹。' }}</small>
          <small v-if="fieldErrors.path" id="resource-path-error" class="field-error">{{ fieldErrors.path }}</small>
        </label>
        <fieldset ref="permissionGroupRef" class="permission-fieldset" tabindex="-1" :aria-invalid="Boolean(fieldErrors.permissions)" aria-describedby="resource-permission-error">
          <legend>访问权限</legend>
          <div class="permission-row">
          <label class="switch-line"><input v-model="form.allowDownload" type="checkbox" :disabled="form.type === 'file'" /> 允许下载</label>
          <label class="switch-line"><input v-model="form.allowUpload" type="checkbox" :disabled="form.type === 'file'" /> 允许上传</label>
          </div>
          <small v-if="fieldErrors.permissions" id="resource-permission-error" class="field-error">{{ fieldErrors.permissions }}</small>
        </fieldset>
        <button class="primary-btn" type="submit" :disabled="saving || !configData?.configWritable">
          {{ saving ? '保存中…' : editing ? '保存修改' : '添加' }}
        </button>
      </form>
    </div>

    <div v-if="configData" class="section-heading"><h2>共享位置<span class="count-badge">{{ resources.length }}</span></h2></div>
    <section v-if="configData" class="cards-grid" aria-label="共享位置列表">
      <article v-for="resource in resources" :key="resource.id" class="config-card resource-card">
        <div class="config-icon" :data-type="resourceType(resource)" aria-hidden="true">
          <AppIcon :name="resourceType(resource) === 'file' ? 'file-cog' : 'folder-cog'" :size="28" />
        </div>
        <div class="resource-card-body">
          <div class="resource-head">
            <div>
              <h2>{{ resource.name }}</h2>
              <p>{{ resource.id }}</p>
            </div>
            <span class="pill" :class="resourceType(resource) === 'file' ? 'ok' : ''">{{ resourceTypeLabel(resource) }}</span>
          </div>
          <code>{{ resource.root || resource.id }}</code>
          <div class="pill-row">
            <span class="pill" :class="resource.allowDownload !== false && resource.canDownload !== false ? 'ok' : 'muted'">{{ resource.allowDownload !== false && resource.canDownload !== false ? '可下载' : '禁止下载' }}</span>
            <span class="pill" :class="resource.allowUpload || resource.canUpload ? 'ok' : 'muted'">{{ resource.allowUpload || resource.canUpload ? '可上传' : '禁止上传' }}</span>
          </div>
          <div class="row-actions">
            <button class="mini-btn" type="button" @click="requestEditResource(resource)">编辑</button>
            <button class="mini-btn danger" type="button" :disabled="!configData?.configWritable" @click="pendingDelete = resource; deleteError = ''">删除</button>
          </div>
        </div>
      </article>
    </section>
    <EmptyState v-if="configData && !loading && !loadError && !resources.length" title="还没有共享位置" />

    <details v-if="configData" class="panel disclosure">
      <summary>传输与分享限制</summary>
      <div class="policy-grid">
        <div><span>单次请求</span><strong>{{ storage?.uploadMaxMB }} MB</strong></div>
        <div><span>单文件</span><strong>{{ storage?.uploadMaxFileMB }} MB</strong></div>
        <div><span>单次文件数</span><strong>{{ storage?.uploadMaxFiles }}</strong></div>
        <div><span>令牌默认有效期</span><strong>{{ Math.round((tokens?.defaultTTLSeconds || 0) / 60) }} 分钟</strong></div>
        <div><span>令牌最长有效期</span><strong>{{ Math.round((tokens?.maxTTLSeconds || 0) / 3600) }} 小时</strong></div>
        <div><span>下载票据</span><strong>{{ Math.round((downloads?.leaseTTLSeconds || 0) / 60) }} 分钟</strong></div>
        <div><span>内容哈希阈值</span><strong>{{ downloads?.contentHashMaxMB }} MB</strong></div>
      </div>
    </details>

    <section v-if="configData" class="panel policy-panel upload-policy-editor">
      <div class="form-title-row">
        <div>
          <h2>上传文件类型</h2>
          <p v-if="uploadPolicy.allowedText.trim() || uploadPolicy.blockedText.trim()" class="muted-text">允许列表留空时，除禁止类型外均可上传。</p>
        </div>
        <button class="mini-btn" type="button" @click="uploadPolicy.allowedText = ''; uploadPolicy.blockedText = ''">清空</button>
      </div>
      <div v-if="policyError" class="alert error" role="alert">{{ policyError }}</div>
      <p v-if="!uploadPolicy.allowedText.trim() && !uploadPolicy.blockedText.trim()" class="alert info" role="status">两项均为空，保存后将不限制上传文件类型。</p>
      <div class="inline-fields">
        <label>允许的扩展名
          <textarea v-model="uploadPolicy.allowedText" rows="5" placeholder="例如：&#10;.pdf&#10;.zip&#10;.jpg" />
        </label>
        <label>禁止的扩展名
          <textarea v-model="uploadPolicy.blockedText" rows="5" placeholder="例如：&#10;.exe&#10;.sh&#10;.ps1" />
        </label>
      </div>
      <button class="primary-btn" type="button" :disabled="policySaving || !configData.configWritable" @click="submitUploadPolicy">
        {{ policySaving ? '保存中…' : '保存上传限制' }}
      </button>
    </section>

    <form v-if="configData" class="panel policy-panel" @submit.prevent="saveAccountPolicy">
      <h2>登录与注册</h2>
      <div class="settings-switches">
        <label class="switch-line">开放账号注册<input v-model="accountPolicy.registrationEnabled" type="checkbox" :disabled="accountSaving || !configData.configWritable" /></label>
        <label class="switch-line">新注册账号需要管理员批准<input v-model="accountPolicy.registrationApprovalRequired" type="checkbox" :disabled="accountSaving || !configData.configWritable" /></label>
        <label class="switch-line">允许使用动态验证码登录<input v-model="accountPolicy.totpEnabled" type="checkbox" :disabled="accountSaving || !configData.configWritable" /></label>
      </div>
      <div v-if="accountError" class="alert error" role="alert">{{ accountError }}</div>
      <button class="primary-btn" type="submit" :disabled="accountSaving || !accountDirty || !configData.configWritable">{{ accountSaving ? '保存中…' : '保存登录设置' }}</button>
    </form>

    <details class="panel disclosure"><summary>服务访问地址</summary><ServiceAddressesCard /></details>

    <ServerFilePicker v-model:open="pickerOpen" :mode="form.type === 'file' ? 'file' : 'directory'" @confirm="handlePick" />

    <ConfirmDialog
      :open="discardDialogOpen"
      title="放弃未保存的修改？"
      message="当前配置尚未保存。继续后，这些修改会丢失且无法恢复。"
      :detail="pendingEdit ? `随后将编辑：${pendingEdit.name}` : pendingDiscardAction === 'leave' ? '随后将离开配置管理页面。' : pendingDiscardAction === 'reload' ? '随后将重新读取服务端配置。' : '随后将清空当前资源表单。'"
      confirm-label="放弃修改"
      :danger="true"
      @confirm="confirmDiscard"
      @cancel="cancelDiscard"
    />

    <ConfirmDialog
      :open="Boolean(pendingDelete)"
      title="删除此共享位置？"
      message="文件不会被删除，但此位置将不再共享，相关分享链接也将无法使用。"
      :detail="pendingDelete ? `${pendingDelete.name} · ${pendingDelete.root || pendingDelete.id}` : ''"
      :error="deleteError"
      :loading="saving"
      confirm-label="删除共享位置"
      @confirm="confirmDelete"
      @cancel="pendingDelete = null"
    />

    <ConfirmDialog
      :open="Boolean(pendingUploadPolicy)"
      title="清空禁止上传的文件类型？"
      :message="pendingUploadPolicy?.allowedExtensions.length ? '清空后，将仅按允许列表限制文件类型。' : '允许列表也为空，保存后上传文件将不受类型限制。'"
      :loading="policySaving"
      confirm-label="确认保存"
      @confirm="pendingUploadPolicy && saveUploadPolicy(pendingUploadPolicy)"
      @cancel="pendingUploadPolicy = null"
    />
  </section>
</template>
