<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from 'vue'
import { ApiError, api } from '@/api'
import ConfirmDialog from '@/components/ConfirmDialog.vue'
import EmptyState from '@/components/EmptyState.vue'
import GlassSelect from '@/components/GlassSelect.vue'
import ServerFilePicker from '@/components/ServerFilePicker.vue'
import StateBlock from '@/components/StateBlock.vue'
import type { DirectoryInfo, FilePickerSelection, ResourcePayload, SafeConfig, UploadPolicyPayload } from '@/types'

const configData = ref<SafeConfig | null>(null)
const loading = ref(true)
const saving = ref(false)
const policySaving = ref(false)
const error = ref('')
const success = ref('')
const editingId = ref<string | null>(null)
const pendingDelete = ref<DirectoryInfo | null>(null)
const pendingUploadPolicy = ref<UploadPolicyPayload | null>(null)
const deleteError = ref('')
const policyError = ref('')
const pickerOpen = ref(false)

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

const resources = computed(() => configData.value?.resources || [])
const storage = computed(() => configData.value?.storage)
const tokens = computed(() => configData.value?.tokens)
const downloads = computed(() => configData.value?.downloads)
const canSubmit = computed(() => form.id.trim() && form.name.trim() && form.path.trim() && (form.type === 'file' || form.allowDownload || form.allowUpload))
const editing = computed(() => Boolean(editingId.value))
const typeOptions = [
  { label: '目录', value: 'directory', hint: '可浏览文件夹，可按需允许上传' },
  { label: '单文件', value: 'file', hint: '只暴露一个文件，不能上传' },
]

function resourceType(resource: DirectoryInfo) {
  return resource.type === 'file' ? 'file' : 'directory'
}

function resourceTypeLabel(resource: DirectoryInfo | ResourcePayload) {
  return resource.type === 'file' ? '单文件' : '目录'
}

function resetForm() {
  editingId.value = null
  Object.assign(form, { id: '', name: '', type: 'directory', path: '', allowDownload: true, allowUpload: false })
}

function syncUploadPolicy() {
  uploadPolicy.allowedText = storage.value?.allowedExtensions.join('\n') || ''
  uploadPolicy.blockedText = storage.value?.blockedExtensions.join('\n') || ''
}

function editResource(resource: DirectoryInfo) {
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
}

async function load() {
  loading.value = true
  error.value = ''
  try {
    configData.value = await api.safeConfig()
    syncUploadPolicy()
  } catch (err) {
    error.value = err instanceof ApiError ? err.message : '配置加载失败。'
  } finally {
    loading.value = false
  }
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
    success.value = '上传扩展名策略已更新，并已写入配置文件。'
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
  if (!payload.blockedExtensions.length) {
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
  if (!canSubmit.value) {
    error.value = '请填写资源 ID、名称、服务端路径，并至少允许下载或上传其中一项。'
    return
  }
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
    success.value = editingId.value ? '共享资源已更新，并已写入配置文件。' : '共享资源已添加，并已写入配置文件。'
    resetForm()
    await load()
  } catch (err) {
    error.value = err instanceof ApiError ? err.message : '保存共享资源失败。'
  } finally {
    saving.value = false
  }
}

async function confirmDelete() {
  const resource = pendingDelete.value
  if (!resource || saving.value) return
  deleteError.value = ''
  saving.value = true
  try {
    await api.deleteResource(resource.id)
    pendingDelete.value = null
    success.value = '共享资源已删除，并已写入配置文件。'
    await load()
  } catch (err) {
    deleteError.value = err instanceof ApiError ? err.message : '删除共享资源失败。'
  } finally {
    saving.value = false
  }
}

watch(() => form.type, (type) => {
  if (type === 'file') {
    form.allowDownload = true
    form.allowUpload = false
  }
})

onMounted(load)
</script>

<template>
  <section class="page-stack config-page">
    <header class="page-header split">
      <div>
        <p class="eyebrow">Configuration</p>
        <h1>配置管理</h1>
        <p>管理员可以在这里管理共享目录和单文件资源；认证、数据库和监听端口等敏感配置仍保留在服务端配置文件中。</p>
      </div>
      <button class="ghost-btn" type="button" @click="load">刷新配置</button>
    </header>

    <StateBlock :loading="loading" :error="error" />
    <div v-if="success" class="alert success">{{ success }}</div>
    <div v-if="configData && !configData.configWritable" class="alert error">当前服务未记录配置文件路径，暂不能在线写回配置。</div>

    <div class="grid two">
      <form class="panel form-grid resource-form" @submit.prevent="submitResource">
        <div class="form-title-row">
          <div>
            <h2>{{ editing ? '编辑共享资源' : '新增共享资源' }}</h2>
            <p class="muted-text">目录资源可以浏览和上传；单文件资源只暴露一个文件用于下载。</p>
          </div>
          <button v-if="editing" class="mini-btn" type="button" @click="resetForm">取消编辑</button>
        </div>
        <label>类型
          <GlassSelect v-model="form.type" :options="typeOptions" aria-label="选择资源类型" />
        </label>
        <div class="inline-fields">
          <label>资源 ID
            <input v-model.trim="form.id" :disabled="editing" placeholder="例如 photos 或 manual" />
            <small>只能包含字母、数字、短横线和下划线。</small>
          </label>
          <label>显示名称
            <input v-model.trim="form.name" placeholder="例如 照片 或 使用说明" />
          </label>
        </div>
        <label>服务端路径
          <div class="path-picker-line">
            <input v-model.trim="form.path" :placeholder="form.type === 'file' ? '/data/manual.pdf' : '/data/photos'" />
            <button class="ghost-btn" type="button" @click="pickerOpen = true">浏览</button>
          </div>
          <small>{{ form.type === 'file' ? '必须是已存在且可读取的具体文件。' : '必须是已存在目录；允许上传时还会校验写入权限。' }}</small>
        </label>
        <div class="permission-row">
          <label class="switch-line"><input v-model="form.allowDownload" type="checkbox" :disabled="form.type === 'file'" /> 允许下载</label>
          <label class="switch-line"><input v-model="form.allowUpload" type="checkbox" :disabled="form.type === 'file'" /> 允许上传</label>
        </div>
        <button class="primary-btn" type="submit" :disabled="saving || !canSubmit || !configData?.configWritable">
          {{ saving ? '保存中…' : editing ? '保存修改' : '添加资源' }}
        </button>
      </form>

      <div class="panel insight-card config-insight">
        <span class="big-number">{{ resources.length }}</span>
        <strong>共享资源</strong>
        <p>目录和单文件资源会写回配置文件，并在保存成功后对新请求立即生效。删除资源会让后续访问该资源的旧令牌和票据失去目录边界。</p>
      </div>
    </div>

    <section class="cards-grid" aria-label="共享资源列表">
      <article v-for="resource in resources" :key="resource.id" class="config-card resource-card">
        <div class="config-icon" :data-type="resourceType(resource)">{{ resourceType(resource) === 'file' ? '文' : '目' }}</div>
        <div class="resource-card-body">
          <div class="resource-head">
            <div>
              <h2>{{ resource.name }}</h2>
              <p>{{ resourceTypeLabel(resource) }} · {{ resource.id }}</p>
            </div>
            <span class="pill" :class="resourceType(resource) === 'file' ? 'ok' : ''">{{ resourceTypeLabel(resource) }}</span>
          </div>
          <code>{{ resource.root || resource.id }}</code>
          <div class="pill-row">
            <span class="pill" :class="resource.allowDownload !== false && resource.canDownload !== false ? 'ok' : 'muted'">下载</span>
            <span class="pill" :class="resource.allowUpload || resource.canUpload ? 'ok' : 'muted'">上传</span>
          </div>
          <div class="row-actions">
            <button class="mini-btn" type="button" @click="editResource(resource)">编辑</button>
            <button class="mini-btn danger" type="button" :disabled="!configData?.configWritable" @click="pendingDelete = resource; deleteError = ''">删除</button>
          </div>
        </div>
      </article>
    </section>
    <EmptyState v-if="!loading && !resources.length" title="没有共享资源" description="添加一个目录或单文件资源后，普通用户即可在文件浏览中看到它。" />

    <section v-if="configData" class="panel policy-panel">
      <h2>运行策略概览</h2>
      <div class="policy-grid">
        <div><span>单次请求</span><strong>{{ storage?.uploadMaxMB }} MB</strong></div>
        <div><span>单文件</span><strong>{{ storage?.uploadMaxFileMB }} MB</strong></div>
        <div><span>单次文件数</span><strong>{{ storage?.uploadMaxFiles }}</strong></div>
        <div><span>令牌默认有效期</span><strong>{{ Math.round((tokens?.defaultTTLSeconds || 0) / 60) }} 分钟</strong></div>
        <div><span>令牌最长有效期</span><strong>{{ Math.round((tokens?.maxTTLSeconds || 0) / 3600) }} 小时</strong></div>
        <div><span>下载票据</span><strong>{{ Math.round((downloads?.leaseTTLSeconds || 0) / 60) }} 分钟</strong></div>
        <div><span>内容哈希阈值</span><strong>{{ downloads?.contentHashMaxMB }} MB</strong></div>
      </div>
      <p class="muted-text">上传扩展名策略：允许 {{ storage?.allowedExtensions.length ? storage.allowedExtensions.join(', ') : '未设置白名单' }}；阻断 {{ storage?.blockedExtensions.join(', ') || '未设置黑名单' }}。</p>
    </section>

    <section v-if="configData" class="panel policy-panel upload-policy-editor">
      <div class="form-title-row">
        <div>
          <h2>上传扩展名策略</h2>
          <p class="muted-text">白名单留空表示不限制；黑名单优先。</p>
        </div>
        <button class="mini-btn" type="button" @click="uploadPolicy.allowedText = ''; uploadPolicy.blockedText = ''">清空策略</button>
      </div>
      <div v-if="policyError" class="alert error">{{ policyError }}</div>
      <div class="inline-fields">
        <label>允许扩展名白名单
          <textarea v-model="uploadPolicy.allowedText" rows="5" placeholder="例如：&#10;.pdf&#10;.zip&#10;.jpg" />
        </label>
        <label>阻断扩展名黑名单
          <textarea v-model="uploadPolicy.blockedText" rows="5" placeholder="例如：&#10;.exe&#10;.sh&#10;.ps1" />
        </label>
      </div>
      <button class="primary-btn" type="button" :disabled="policySaving || !configData.configWritable" @click="submitUploadPolicy">
        {{ policySaving ? '保存中…' : '保存上传策略' }}
      </button>
    </section>

    <ServerFilePicker v-model:open="pickerOpen" :mode="form.type === 'file' ? 'file' : 'directory'" @confirm="handlePick" />

    <ConfirmDialog
      :open="Boolean(pendingDelete)"
      title="删除共享资源？"
      message="删除后会写回配置文件，新请求将不再看到这个资源。已有令牌不会被删除，但会因为找不到资源而无法继续使用。"
      :detail="pendingDelete ? `${pendingDelete.name} · ${pendingDelete.root || pendingDelete.id}` : ''"
      :error="deleteError"
      :loading="saving"
      confirm-label="删除资源"
      @confirm="confirmDelete"
      @cancel="pendingDelete = null"
    />

    <ConfirmDialog
      :open="Boolean(pendingUploadPolicy)"
      title="确认清空阻断黑名单？"
      message="如果白名单也为空，之后的新上传不会按扩展名限制。"
      :loading="policySaving"
      confirm-label="确认保存"
      @confirm="pendingUploadPolicy && saveUploadPolicy(pendingUploadPolicy)"
      @cancel="pendingUploadPolicy = null"
    />
  </section>
</template>
