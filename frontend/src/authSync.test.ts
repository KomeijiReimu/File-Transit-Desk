import { afterEach, describe, expect, it, vi } from 'vitest'

class MockBroadcastChannel {
  static instances: MockBroadcastChannel[] = []
  static throwOnPost = false

  readonly name: string
  readonly messages: unknown[] = []
  readonly listeners: Array<(event: MessageEvent) => void> = []
  readonly close = vi.fn()

  constructor(name: string) {
    this.name = name
    MockBroadcastChannel.instances.push(this)
  }

  postMessage(value: unknown) {
    if (MockBroadcastChannel.throwOnPost) throw new Error('broadcast failed')
    this.messages.push(value)
  }

  addEventListener(type: string, listener: (event: MessageEvent) => void) {
    if (type === 'message') this.listeners.push(listener)
  }

  emit(value: unknown) {
    this.listeners.forEach((listener) => listener(new MessageEvent('message', { data: value })))
  }
}

function storageDouble(blocked = false) {
  return {
    setItem: blocked
      ? vi.fn(() => { throw new Error('storage blocked') })
      : vi.fn(),
    removeItem: vi.fn(),
  }
}

async function loadAuthSync() {
  vi.resetModules()
  return import('@/authSync')
}

describe('authSync', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
    MockBroadcastChannel.instances.length = 0
    MockBroadcastChannel.throwOnPost = false
  })

  it('broadcasts only random metadata and does not deliver its own event locally', async () => {
    vi.stubGlobal('BroadcastChannel', MockBroadcastChannel)
    vi.stubGlobal('localStorage', storageDouble())
    const authSync = await loadAuthSync()
    const listener = vi.fn()
    const unsubscribe = authSync.subscribeAuthSync(listener)

    const event = authSync.publishAuthSync('login')

    expect(Object.keys(event).sort()).toEqual(['id', 'reason'])
    expect(event.id.length).toBeGreaterThanOrEqual(8)
    expect(event.reason).toBe('login')
    expect(JSON.stringify(event)).not.toMatch(/binding|role|credential|cookie|admin|user/i)
    expect(listener).not.toHaveBeenCalled()
    expect(authSync.acceptExternalAuthSyncEvent(event)).toBe(false)
    expect(listener).not.toHaveBeenCalled()
    unsubscribe()
  })

  it('deduplicates the same external event delivered by BroadcastChannel and storage', async () => {
    vi.stubGlobal('BroadcastChannel', MockBroadcastChannel)
    vi.stubGlobal('localStorage', storageDouble())
    const authSync = await loadAuthSync()
    const listener = vi.fn()
    authSync.subscribeAuthSync(listener)
    const event = { id: 'dual-transport-event', reason: 'subject_changed' as const }

    MockBroadcastChannel.instances[0].emit(event)
    window.dispatchEvent(new StorageEvent('storage', {
      key: authSync.AUTH_SYNC_STORAGE_KEY,
      newValue: JSON.stringify(event),
    }))

    expect(listener).toHaveBeenCalledTimes(1)
    expect(listener).toHaveBeenCalledWith(event)
  })

  it('falls back to storage for other tabs when BroadcastChannel post later fails', async () => {
    vi.stubGlobal('BroadcastChannel', MockBroadcastChannel)
    const storage = storageDouble()
    vi.stubGlobal('localStorage', storage)
    const publisher = await loadAuthSync()
    publisher.subscribeAuthSync(() => undefined)

    const receiver = await loadAuthSync()
    const received = vi.fn()
    receiver.subscribeAuthSync(received)
    MockBroadcastChannel.throwOnPost = true

    const event = publisher.publishAuthSync('logout')
    expect(storage.setItem).toHaveBeenCalledWith(publisher.AUTH_SYNC_STORAGE_KEY, JSON.stringify(event))
    window.dispatchEvent(new StorageEvent('storage', {
      key: publisher.AUTH_SYNC_STORAGE_KEY,
      newValue: JSON.stringify(event),
    }))

    expect(received).toHaveBeenCalledTimes(1)
    expect(received).toHaveBeenCalledWith(event)
  })

  it('keeps BroadcastChannel delivery working when storage is blocked', async () => {
    vi.stubGlobal('BroadcastChannel', MockBroadcastChannel)
    vi.stubGlobal('localStorage', storageDouble(true))
    const authSync = await loadAuthSync()
    authSync.subscribeAuthSync(() => undefined)

    expect(() => authSync.publishAuthSync('login')).not.toThrow()
    expect(MockBroadcastChannel.instances[0].messages).toHaveLength(1)
  })

  it('rejects malformed external data and duplicate event IDs', async () => {
    vi.stubGlobal('BroadcastChannel', MockBroadcastChannel)
    vi.stubGlobal('localStorage', storageDouble())
    const authSync = await loadAuthSync()
    const listener = vi.fn()
    authSync.subscribeAuthSync(listener)
    const event = { id: 'external-random-event', reason: 'subject_changed' as const }

    expect(authSync.acceptExternalAuthSyncEvent(event)).toBe(true)
    expect(authSync.acceptExternalAuthSyncEvent(event)).toBe(false)
    expect(authSync.acceptExternalAuthSyncEvent({ ...event, id: 'external-with-secret', sessionBinding: 'secret' })).toBe(false)
    expect(listener).toHaveBeenCalledTimes(1)
  })
})
