import { describe, expect, it, vi } from 'vitest'
import {
  createUploadItem,
  reconcileConservativeUploadedBytes,
  useUploadQueue,
  type UploadItem,
  type UploadRunOptions,
} from '@/composables/useUploadQueue'

function file(name: string, size = 4) {
  return new File(['x'.repeat(size)], name)
}

function item(name: string, size = 4) {
  return createUploadItem(name, file(name, size))
}

function deferred<T = void>() {
  let resolve!: (value: T | PromiseLike<T>) => void
  let reject!: (reason?: unknown) => void
  const promise = new Promise<T>((res, rej) => {
    resolve = res
    reject = rej
  })
  return { promise, resolve, reject }
}

function queueWith(uploadFile: (item: UploadItem, options: UploadRunOptions) => Promise<void>, overrides: Partial<Parameters<typeof useUploadQueue>[0]> = {}) {
  const releases: ReturnType<typeof vi.fn>[] = []
  const acquireHold = vi.fn(() => {
    const release = vi.fn()
    releases.push(release)
    return release
  })
  const queue = useUploadQueue({
    uploadFile,
    acquireHold,
    errorMessage: (error) => error instanceof Error ? error.message : '失败',
    ...overrides,
  })
  return { ...queue, acquireHold, releases }
}

describe('useUploadQueue', () => {
  it('uploads every pending item serially', async () => {
    let active = 0
    let maxActive = 0
    const calls: string[] = []
    const state = queueWith(async (entry) => {
      active += 1
      maxActive = Math.max(maxActive, active)
      calls.push(entry.id)
      await Promise.resolve()
      active -= 1
    })
    state.add(item('a'))
    state.add(item('b'))

    await state.uploadAll()

    expect(calls).toEqual(['a', 'b'])
    expect(maxActive).toBe(1)
    expect(state.queue.value.map((entry) => entry.status)).toEqual(['success', 'success'])
  })

  it('continues to the second item after the first upload fails', async () => {
    const state = queueWith(vi.fn(async (entry) => {
      if (entry.id === 'a') throw new Error('首项失败')
    }))
    state.add(item('a'))
    state.add(item('b'))

    await state.uploadAll()

    expect(state.queue.value[0]).toMatchObject({ status: 'error', error: '首项失败' })
    expect(state.queue.value[1].status).toBe('success')
  })

  it('canceling the current item stops the batch before later items start', async () => {
    const started: string[] = []
    const state = queueWith((entry, { signal }) => new Promise<void>((resolve, reject) => {
      started.push(entry.id)
      signal.addEventListener('abort', () => reject(new Error('已取消')), { once: true })
      if (entry.id !== 'a') resolve()
    }))
    state.add(item('a'))
    state.add(item('b'))

    const run = state.uploadAll()
    await Promise.resolve()
    expect(state.cancelCurrent()).toBe(true)
    await run

    expect(started).toEqual(['a'])
    expect(state.queue.value[0].status).toBe('error')
    expect(state.queue.value[1].status).toBe('queued')
  })

  it('retries an error item with a fresh controller and clears it afterward', async () => {
    const controllers: AbortSignal[] = []
    let attempt = 0
    const state = queueWith(vi.fn(async (_entry, options) => {
      controllers.push(options.signal)
      attempt += 1
      if (attempt === 1) throw new Error('失败')
    }))
    const entry = item('a')
    state.add(entry)
    await state.uploadAll()
    expect(entry.controller).toBeUndefined()

    await state.retry(entry)

    expect(entry.status).toBe('success')
    expect(entry.controller).toBeUndefined()
    expect(controllers).toHaveLength(2)
    expect(controllers[0]).not.toBe(controllers[1])
  })

  it('never retransmits a successful item', async () => {
    const uploadFile = vi.fn(async () => {})
    const state = queueWith(uploadFile)
    const entry = item('a')
    state.add(entry)
    await state.uploadAll()

    expect(await state.retry(entry)).toBe(false)
    await state.uploadAll()
    expect(uploadFile).toHaveBeenCalledTimes(1)
  })

  it('rejects concurrent uploadAll and retry flows', async () => {
    const pending = deferred()
    const state = queueWith(() => pending.promise)
    const first = item('a')
    const second = item('b')
    state.add(first)
    state.add(second)

    const running = state.uploadAll()
    await Promise.resolve()
    expect(await state.uploadAll()).toBe(false)
    expect(await state.retry(second)).toBe(false)
    pending.resolve()
    await running
  })

  it('rejects add, remove and clear while a deferred success callback keeps the batch busy', async () => {
    const callbackStarted = deferred()
    const finishCallback = deferred()
    const state = queueWith(async () => {}, {
      onItemSuccess: async () => {
        callbackStarted.resolve()
        await finishCallback.promise
      },
    })
    const first = item('a')
    const second = item('b')
    state.add(first)
    state.add(second)

    const running = state.uploadAll()
    await callbackStarted.promise
    expect(state.busy.value).toBe(true)
    expect(state.add(item('c'))).toBe(false)
    expect(state.remove('b')).toBe(false)
    expect(state.clear()).toBe(false)

    finishCallback.resolve()
    await running
  })

  it('skips a stale snapshot item that is no longer present in the queue', async () => {
    const firstStarted = deferred()
    const finishFirst = deferred()
    const uploadFile = vi.fn(async (entry: UploadItem) => {
      if (entry.id === 'a') {
        firstStarted.resolve()
        await finishFirst.promise
      }
    })
    const state = queueWith(uploadFile)
    const first = item('a')
    const removedFromSnapshot = item('b')
    state.add(first)
    state.add(removedFromSnapshot)

    const running = state.uploadAll()
    await firstStarted.promise
    // 模拟外部持有 queue ref 的防御性变更；公开 remove 在 busy 时本身会拒绝。
    state.queue.value = state.queue.value.filter((entry) => entry.id !== removedFromSnapshot.id)
    finishFirst.resolve()
    await running

    expect(uploadFile).toHaveBeenCalledTimes(1)
    expect(uploadFile).toHaveBeenCalledWith(first, expect.any(Object))
  })

  it('calculates progress, instant speed, average speed and ETA', async () => {
    let now = 1_000
    const state = queueWith(async (_entry, { onProgress }) => {
      onProgress({ loaded: 20, total: 100, percent: 20 })
      now = 2_000
      onProgress({ loaded: 60, total: 100, percent: 60 })
      throw new Error('保留进度用于断言')
    }, { now: () => now })
    const entry = item('a', 100)
    state.add(entry)

    await state.uploadAll()

    expect(entry.loaded).toBe(60)
    expect(entry.progress).toBe(60)
    expect(entry.speedBps).toBeGreaterThan(0)
    expect(entry.averageSpeedBps).toBeCloseTo(60)
    expect(entry.etaSeconds).toBeCloseTo(2 / 3)
  })

  it('exposes overall byte and completion statistics', async () => {
    const second = deferred()
    const secondStarted = deferred()
    const state = queueWith(async (entry, { onProgress }) => {
      if (entry.id === 'b') {
        onProgress({ loaded: 5, total: 10, percent: 50 })
        secondStarted.resolve()
        await second.promise
      }
    })
    state.add(item('a', 10))
    state.add(item('b', 10))
    const running = state.uploadAll()
    await secondStarted.promise

    expect(state.totalBytes.value).toBe(20)
    expect(state.finishedCount.value).toBe(1)
    expect(state.finishedBytes.value).toBe(15)
    expect(state.overallProgress.value).toBe(75)
    second.resolve()
    await running
  })

  it('releases its hold exactly once after success, failure and dispose', async () => {
    const state = queueWith(async () => { throw new Error('失败') })
    state.add(item('a'))
    await state.uploadAll()
    expect(state.releases[0]).toHaveBeenCalledTimes(1)

    const pending = deferred()
    const active = queueWith(() => pending.promise)
    active.add(item('b'))
    const running = active.uploadAll()
    await Promise.resolve()
    active.dispose()
    pending.resolve()
    await running
    expect(active.releases[0]).toHaveBeenCalledTimes(1)
  })

  it('dispose does not abort an established upload and suppresses later UI callbacks', async () => {
    const uploadStarted = deferred()
    const finishUpload = deferred()
    const onItemSuccess = vi.fn()
    const shouldStopBatch = vi.fn()
    const onBatchFinished = vi.fn()
    let signal: AbortSignal | undefined
    const state = queueWith(async (_entry, options) => {
      signal = options.signal
      uploadStarted.resolve()
      await finishUpload.promise
    }, { onItemSuccess, shouldStopBatch, onBatchFinished })
    state.add(item('a'))

    const running = state.uploadAll()
    await uploadStarted.promise
    state.dispose()
    expect(signal?.aborted).toBe(false)
    finishUpload.resolve()
    await running

    expect(state.queue.value[0].status).toBe('success')
    expect(onItemSuccess).not.toHaveBeenCalled()
    expect(shouldStopBatch).not.toHaveBeenCalled()
    expect(onBatchFinished).not.toHaveBeenCalled()
    expect(state.releases[0]).toHaveBeenCalledTimes(1)
  })

  it('keeps upload success when success or batch callbacks fail', async () => {
    const state = queueWith(async () => {}, {
      onItemSuccess: async () => { throw new Error('刷新失败') },
      shouldStopBatch: async () => { throw new Error('停止判断失败') },
      onBatchFinished: async () => { throw new Error('通知失败') },
    })
    state.add(item('a'))
    state.add(item('b'))

    await expect(state.uploadAll()).resolves.toBe(true)
    expect(state.queue.value.every((entry) => entry.status === 'success')).toBe(true)
  })

  it('stops before an unauthorized next item when shouldStopBatch requests it', async () => {
    const uploadFile = vi.fn(async () => {})
    const shouldStopBatch = vi.fn(() => true)
    const state = queueWith(uploadFile, { shouldStopBatch })
    state.add(item('authorized'))
    state.add(item('needs-session'))

    await state.uploadAll()

    expect(uploadFile).toHaveBeenCalledTimes(1)
    expect(state.queue.value.map((entry) => entry.status)).toEqual(['success', 'queued'])
    expect(state.stopRequested.value).toBe(true)
  })

  it('removes only inactive items and clears an idle queue', async () => {
    const pending = deferred()
    const state = queueWith(() => pending.promise)
    const entry = item('a')
    state.add(entry)
    const running = state.uploadAll()
    await Promise.resolve()
    expect(state.remove('a')).toBe(false)
    pending.resolve()
    await running
    expect(state.remove('a')).toBe(true)
    expect(state.clear()).toBe(true)
  })

  it('preserves local successful bytes until refreshed server usage catches up', () => {
    expect(reconcileConservativeUploadedBytes(100, 50, 120)).toBe(30)
    expect(reconcileConservativeUploadedBytes(100, 50, 150)).toBe(0)
    expect(reconcileConservativeUploadedBytes(100, 50, 200)).toBe(0)
  })
})
