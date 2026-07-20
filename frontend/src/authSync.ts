export const AUTH_SYNC_STORAGE_KEY = 'ft-auth-sync-v1'
const AUTH_SYNC_CHANNEL = 'ft-auth-sync-v1'

export type AuthSyncReason = 'login' | 'logout' | 'expired' | 'subject_changed'

export interface AuthSyncEvent {
  id: string
  reason: AuthSyncReason
}

type AuthSyncListener = (event: AuthSyncEvent) => void

const listeners = new Set<AuthSyncListener>()
const seenEventIDs = new Set<string>()
let channel: BroadcastChannel | undefined
let started = false

function randomEventID() {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') return crypto.randomUUID()
  if (typeof crypto !== 'undefined' && typeof crypto.getRandomValues === 'function') {
    const bytes = crypto.getRandomValues(new Uint8Array(16))
    return Array.from(bytes, (value) => value.toString(16).padStart(2, '0')).join('')
  }
  return `${Date.now().toString(36)}-${Math.random().toString(36).slice(2)}`
}

function validEvent(value: unknown): value is AuthSyncEvent {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return false
  const keys = Object.keys(value)
  if (keys.length !== 2 || keys.some((key) => key !== 'id' && key !== 'reason')) return false
  const candidate = value as Partial<AuthSyncEvent>
  return typeof candidate.id === 'string'
    && candidate.id.length >= 8
    && candidate.id.length <= 128
    && (candidate.reason === 'login' || candidate.reason === 'logout' || candidate.reason === 'expired' || candidate.reason === 'subject_changed')
}

function rememberEvent(id: string) {
  seenEventIDs.add(id)
  if (seenEventIDs.size <= 64) return
  const oldest = seenEventIDs.values().next().value
  if (typeof oldest === 'string') seenEventIDs.delete(oldest)
}

// Only transport metadata is accepted. Session bindings, roles and credentials
// are intentionally absent from the cross-tab protocol.
export function acceptExternalAuthSyncEvent(value: unknown) {
  if (!validEvent(value) || seenEventIDs.has(value.id)) return false
  rememberEvent(value.id)
  listeners.forEach((listener) => listener(value))
  return true
}

function startAuthSyncTransport() {
  if (started || typeof window === 'undefined') return
  started = true
  // Always keep the storage transport active. BroadcastChannel can succeed at
  // startup and later fail, and mixed browser contexts may support only one of
  // the two mechanisms.
  window.addEventListener('storage', (event) => {
    if (event.key !== AUTH_SYNC_STORAGE_KEY || !event.newValue) return
    try {
      acceptExternalAuthSyncEvent(JSON.parse(event.newValue))
    } catch {
      // Ignore malformed data written by unrelated scripts or older clients.
    }
  })
  if (typeof BroadcastChannel === 'function') {
    try {
      channel = new BroadcastChannel(AUTH_SYNC_CHANNEL)
      channel.addEventListener('message', (event) => acceptExternalAuthSyncEvent(event.data))
    } catch {
      channel = undefined
    }
  }
}

export function subscribeAuthSync(listener: AuthSyncListener) {
  listeners.add(listener)
  startAuthSyncTransport()
  return () => listeners.delete(listener)
}

export function publishAuthSync(reason: AuthSyncReason) {
  const event: AuthSyncEvent = { id: randomEventID(), reason }
  rememberEvent(event.id)
  startAuthSyncTransport()
  if (channel) {
    try {
      channel.postMessage(event)
    } catch {
      try {
        channel.close()
      } catch {
        // Ignore a transport that was already closed.
      }
      channel = undefined
    }
  }
  if (typeof window !== 'undefined') {
    try {
      localStorage.setItem(AUTH_SYNC_STORAGE_KEY, JSON.stringify(event))
      localStorage.removeItem(AUTH_SYNC_STORAGE_KEY)
    } catch {
      // A blocked storage fallback must not break local authentication flows.
    }
  }
  return event
}
