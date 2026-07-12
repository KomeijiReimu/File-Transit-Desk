import { computed, ref, type Ref } from 'vue'

export type UploadStatus = 'queued' | 'uploading' | 'success' | 'error'

export interface UploadItem {
  id: string
  file: File
  status: UploadStatus
  progress: number
  loaded: number
  total: number
  error?: string
  controller?: AbortController
  speedBps?: number
  averageSpeedBps?: number
  etaSeconds?: number
  startedAt?: number
  lastLoaded?: number
  lastProgressAt?: number
}

export interface UploadProgress {
  loaded: number
  total: number
  percent: number
}

export interface UploadRunOptions {
  signal: AbortSignal
  onProgress: (progress: UploadProgress) => void
}

export interface UploadBatchResult<T extends UploadItem> {
  queue: T[]
  stopped: boolean
  succeeded: number
  failed: number
  pending: number
}

export interface UseUploadQueueOptions<T extends UploadItem> {
  uploadFile: (item: T, options: UploadRunOptions) => Promise<void>
  acquireHold: () => () => void
  errorMessage: (error: unknown) => string
  onItemSuccess?: (item: T) => void | Promise<void>
  shouldStopBatch?: (item: T, queue: T[]) => boolean | Promise<boolean>
  onBatchFinished?: (result: UploadBatchResult<T>) => void | Promise<void>
  now?: () => number
}

export function createUploadItem(id: string, file: File): UploadItem {
  return { id, file, status: 'queued', progress: 0, loaded: 0, total: file.size }
}

export function reconcileConservativeUploadedBytes(serverBytes: number, localSuccessfulBytes: number, refreshedServerBytes: number) {
  return Math.max(0, serverBytes + localSuccessfulBytes - refreshedServerBytes)
}

export function useUploadQueue<T extends UploadItem = UploadItem>(options: UseUploadQueueOptions<T>) {
  const queue = ref<T[]>([]) as Ref<T[]>
  const batchActive = ref(false)
  const stopRequested = ref(false)
  let disposed = false
  let activeRelease: (() => void) | undefined

  const uploading = computed(() => queue.value.some((item) => item.status === 'uploading'))
  const busy = computed(() => batchActive.value || uploading.value)
  const currentUpload = computed(() => queue.value.find((item) => item.status === 'uploading'))
  const hasPendingUploads = computed(() => queue.value.some((item) => item.status === 'queued' || item.status === 'error'))
  const totalBytes = computed(() => queue.value.reduce((sum, item) => sum + item.file.size, 0))
  const finishedBytes = computed(() => queue.value.reduce((sum, item) => sum + (item.status === 'success' ? item.file.size : item.status === 'uploading' ? item.loaded : 0), 0))
  const finishedCount = computed(() => queue.value.filter((item) => item.status === 'success').length)
  const overallProgress = computed(() => totalBytes.value > 0 ? Math.min(100, Math.round((finishedBytes.value / totalBytes.value) * 100)) : 0)
  const currentSpeed = computed(() => queue.value.reduce((sum, item) => sum + (item.status === 'uploading' ? item.speedBps || 0 : 0), 0))

  function add(item: T) {
    if (disposed || busy.value) return false
    queue.value.push(item)
    return true
  }

  function remove(id: string) {
    if (disposed || busy.value) return false
    const item = queue.value.find((entry) => entry.id === id)
    if (!item || item.status === 'uploading') return false
    queue.value = queue.value.filter((entry) => entry.id !== id)
    return true
  }

  function clear() {
    if (disposed || busy.value) return false
    queue.value = []
    return true
  }

  function updateProgress(item: T, progress: UploadProgress) {
    const now = (options.now || Date.now)()
    if (!item.startedAt) item.startedAt = now
    const previousLoaded = item.lastLoaded ?? 0
    const previousAt = item.lastProgressAt ?? now
    const hasPrevious = item.lastProgressAt !== undefined
    const deltaSeconds = Math.max(0.001, (now - previousAt) / 1000)
    const instant = hasPrevious ? Math.max(0, (progress.loaded - previousLoaded) / deltaSeconds) : 0
    item.speedBps = hasPrevious ? (item.speedBps ? item.speedBps * 0.7 + instant * 0.3 : instant) : 0
    item.averageSpeedBps = progress.loaded > 0 ? progress.loaded / Math.max(0.001, (now - item.startedAt) / 1000) : 0
    item.loaded = progress.total > 0 ? Math.min(progress.loaded, progress.total) : progress.loaded
    item.total = progress.total || item.file.size
    item.progress = progress.percent
    item.etaSeconds = item.averageSpeedBps > 0 && item.total > item.loaded ? (item.total - item.loaded) / item.averageSpeedBps : undefined
    item.lastLoaded = progress.loaded
    item.lastProgressAt = now
  }

  function resetForUpload(item: T) {
    item.status = 'uploading'
    item.progress = 0
    item.loaded = 0
    item.total = item.file.size
    item.speedBps = 0
    item.averageSpeedBps = 0
    item.etaSeconds = undefined
    item.startedAt = undefined
    item.lastLoaded = 0
    item.lastProgressAt = undefined
    item.error = undefined
    item.controller = undefined
  }

  async function runItem(item: T) {
    if (item.status === 'uploading' || item.status === 'success') return false
    resetForUpload(item)
    const controller = new AbortController()
    item.controller = controller
    try {
      await options.uploadFile(item, {
        signal: controller.signal,
        onProgress: (progress) => updateProgress(item, progress),
      })
      item.progress = 100
      item.loaded = item.file.size
      item.total = item.file.size
      item.status = 'success'
      if (!disposed) {
        try {
          await options.onItemSuccess?.(item)
        } catch {
          // 后处理失败不能把已经完成的上传回滚成失败。
        }
      }
      return true
    } catch (error) {
      item.status = 'error'
      if (!disposed) {
        try {
          item.error = options.errorMessage(error)
        } catch {
          item.error = '上传失败。'
        }
      }
      return false
    } finally {
      item.controller = undefined
    }
  }

  function acquireRunHold() {
    const release = options.acquireHold()
    let released = false
    const releaseOnce = () => {
      if (released) return
      released = true
      release()
    }
    activeRelease = releaseOnce
    return releaseOnce
  }

  function batchResult(): UploadBatchResult<T> {
    return {
      queue: queue.value,
      stopped: stopRequested.value,
      succeeded: queue.value.filter((item) => item.status === 'success').length,
      failed: queue.value.filter((item) => item.status === 'error').length,
      pending: queue.value.filter((item) => item.status === 'queued' || item.status === 'error').length,
    }
  }

  async function uploadAll() {
    if (disposed || busy.value) return false
    stopRequested.value = false
    batchActive.value = true
    const release = acquireRunHold()
    try {
      const pending = queue.value.filter((item) => item.status === 'queued' || item.status === 'error')
      for (const item of pending) {
        if (disposed || stopRequested.value) break
        if (!queue.value.includes(item) || (item.status !== 'queued' && item.status !== 'error')) continue
        await runItem(item)
        if (disposed || stopRequested.value) break
        let shouldStop = false
        try {
          shouldStop = await options.shouldStopBatch?.(item, queue.value) || false
        } catch {
          shouldStop = false
        }
        if (shouldStop) {
          stopRequested.value = true
          break
        }
      }
      if (!disposed) {
        try {
          await options.onBatchFinished?.(batchResult())
        } catch {
          // 汇总通知失败不改变队列中各文件的最终状态。
        }
      }
      return true
    } finally {
      batchActive.value = false
      release()
      if (activeRelease === release) activeRelease = undefined
    }
  }

  async function retry(item: T) {
    if (disposed || busy.value || item.status === 'success' || item.status === 'uploading') return false
    stopRequested.value = false
    batchActive.value = true
    const release = acquireRunHold()
    try {
      await runItem(item)
      return true
    } finally {
      batchActive.value = false
      release()
      if (activeRelease === release) activeRelease = undefined
    }
  }

  function cancel(item: T) {
    if (item.status !== 'uploading' || !item.controller) return false
    stopRequested.value = true
    item.controller.abort()
    return true
  }

  function cancelCurrent() {
    return currentUpload.value ? cancel(currentUpload.value) : false
  }

  function stopBatch() {
    stopRequested.value = true
  }

  function dispose() {
    disposed = true
    stopRequested.value = true
    activeRelease?.()
    activeRelease = undefined
  }

  return {
    queue,
    batchActive,
    stopRequested,
    uploading,
    busy,
    currentUpload,
    hasPendingUploads,
    totalBytes,
    finishedBytes,
    finishedCount,
    overallProgress,
    currentSpeed,
    add,
    remove,
    clear,
    uploadAll,
    retry,
    cancel,
    cancelCurrent,
    stopBatch,
    dispose,
  }
}
