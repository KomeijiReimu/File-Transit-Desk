export const AUTH_EXPIRED_EVENT = 'ft:auth-expired'

export type AuthenticationClearMode = 'anonymous' | 'unknown'
export type SessionSubjectChangeReason = 'session_subject_changed' | 'external_auth_change'

interface AuthExpiredDetail {
  epoch: number
  stateCleared: true
}

let epoch = 0
let clearAuthentication: ((mode?: AuthenticationClearMode) => void) | undefined
let invalidateSubject: ((reason: SessionSubjectChangeReason) => void) | undefined
let sessionBinding: string | undefined
const expiredEpochs = new Set<number>()

export function authenticationEpoch() {
  return epoch
}

export function advanceAuthenticationEpoch() {
  epoch += 1
  return epoch
}

export function registerAuthenticationClear(handler: (mode?: AuthenticationClearMode) => void) {
  clearAuthentication = handler
}

export function currentSessionBinding() {
  return sessionBinding
}

export function setCurrentSessionBinding(binding?: string) {
  const normalized = typeof binding === 'string' ? binding.trim() : ''
  sessionBinding = normalized || undefined
}

export function registerSessionSubjectInvalidation(handler: (reason: SessionSubjectChangeReason) => void) {
  invalidateSubject = handler
}

// A request may invalidate only the exact epoch/subject it captured. This
// keeps concurrent stale responses from clearing a newer login twice.
export function invalidateSessionSubject(
  requestEpoch: number,
  expectedBinding: string | undefined,
  reason: SessionSubjectChangeReason = 'session_subject_changed',
) {
  if (requestEpoch !== epoch) return false
  if (expectedBinding !== undefined && expectedBinding !== sessionBinding) return false
  if (invalidateSubject) {
    invalidateSubject(reason)
  } else if (clearAuthentication) {
    clearAuthentication('unknown')
  } else {
    advanceAuthenticationEpoch()
    setCurrentSessionBinding(undefined)
  }
  return true
}

export function isInternallyHandledAuthExpiry(event: Event) {
  if (!(event instanceof CustomEvent)) return false
  const detail = event.detail as Partial<AuthExpiredDetail> | undefined
  return detail?.stateCleared === true && typeof detail.epoch === 'number'
}

// 同一认证 epoch 只清理和广播一次；清理同步发生在任何路由跳转之前。
export function expireSessionOnce(requestEpoch: number) {
  if (requestEpoch !== epoch || expiredEpochs.has(requestEpoch)) return false
  expiredEpochs.add(requestEpoch)
  clearAuthentication?.('anonymous')
  if (typeof window !== 'undefined') {
    window.dispatchEvent(new CustomEvent<AuthExpiredDetail>(AUTH_EXPIRED_EVENT, {
      detail: { epoch: requestEpoch, stateCleared: true },
    }))
  }
  return true
}
