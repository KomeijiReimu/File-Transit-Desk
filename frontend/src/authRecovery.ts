import { authenticationEpoch } from '@/authEpoch'
import { SESSION_ACTIVITY_WINDOW_MS } from '@/sessionActivity'

export interface SessionHeartbeatPayload {
  ok: boolean
  idleExpiresAt?: string
  sessionBinding?: string
}

export type SessionRecoveryResult =
  | { recovered: true; value: SessionHeartbeatPayload; authEpoch: number }
  | { recovered: false; error: unknown; authFailure: boolean; authEpoch: number }
  | { recovered: false; denied: true; authEpoch: number }
  | { recovered: false; stale: true; authEpoch: number }
  | { recovered: false; subjectChanged: true; authEpoch: number }

export interface SessionHeartbeatContext {
  authEpoch: number
  observedAttempt: number
  sessionBinding?: string
  activityGeneration?: number
  activityRecordedAt?: number
}

interface CompletedHeartbeat {
  authEpoch: number
  sessionBinding?: string
  activityAuthorized: boolean
  result: SessionRecoveryResult
}

interface RunningHeartbeat {
  authEpoch: number
  sessionBinding?: string
  generation: number
  activityAuthorized: boolean
  promise: Promise<SessionRecoveryResult>
}

let completedAttempts = 0
const completedResults = new Map<number, CompletedHeartbeat>()
let running: RunningHeartbeat | undefined
let recoveryGeneration = 0

function isUnauthorized(error: unknown) {
  return Boolean(error && typeof error === 'object' && 'status' in error && (error as { status?: unknown }).status === 401)
}

function isSubjectChanged(error: unknown) {
  return Boolean(error && typeof error === 'object'
    && 'status' in error && (error as { status?: unknown }).status === 409
    && 'code' in error && (error as { code?: unknown }).code === 'session_subject_changed')
}

function staleResult(authEpoch: number): SessionRecoveryResult {
  return { recovered: false, stale: true, authEpoch }
}

function deniedResult(authEpoch: number): SessionRecoveryResult {
  return { recovered: false, denied: true, authEpoch }
}

function subjectChangedResult(authEpoch: number): SessionRecoveryResult {
  return { recovered: false, subjectChanged: true, authEpoch }
}

function remember(attempt: number, completed: CompletedHeartbeat) {
  completedResults.set(attempt, completed)
  if (completedResults.size <= 16) return
  const oldest = completedResults.keys().next().value
  if (typeof oldest === 'number') completedResults.delete(oldest)
}

export function sessionRecoveryAttempt() {
  return completedAttempts
}

export function invalidateSessionRecovery() {
  recoveryGeneration += 1
  completedAttempts += 1
  completedResults.clear()
  running = undefined
}

function coordinateHeartbeat(
  context: SessionHeartbeatContext,
  heartbeat: () => Promise<SessionHeartbeatPayload>,
): Promise<SessionRecoveryResult> {
  if (context.authEpoch !== authenticationEpoch()) return Promise.resolve(staleResult(context.authEpoch))

  const activityAge = context.activityRecordedAt === undefined ? Number.POSITIVE_INFINITY : Date.now() - context.activityRecordedAt
  const hasActivity = context.activityGeneration !== undefined
    && activityAge >= 0
    && activityAge <= SESSION_ACTIVITY_WINDOW_MS
  const completed = completedResults.get(context.observedAttempt)
  if (completed && completed.authEpoch === context.authEpoch && completed.sessionBinding === context.sessionBinding) {
    if (hasActivity || completed.activityAuthorized) return Promise.resolve(completed.result)
    return Promise.resolve(deniedResult(context.authEpoch))
  }

  if (running) {
    if (running.authEpoch !== context.authEpoch) return Promise.resolve(staleResult(context.authEpoch))
    if (running.sessionBinding !== context.sessionBinding) return Promise.resolve(subjectChangedResult(context.authEpoch))
    if (hasActivity) running.activityAuthorized = true
    if (!hasActivity && !running.activityAuthorized) {
      return Promise.resolve(deniedResult(context.authEpoch))
    }
    return running.promise
  }

  if (!hasActivity) return Promise.resolve(deniedResult(context.authEpoch))
  if (context.observedAttempt !== completedAttempts) return Promise.resolve(deniedResult(context.authEpoch))

  const attempt = completedAttempts
  const generation = recoveryGeneration
  const entry = {} as RunningHeartbeat
  entry.authEpoch = context.authEpoch
  entry.sessionBinding = context.sessionBinding
  entry.generation = generation
  entry.activityAuthorized = hasActivity
  entry.promise = (async (): Promise<SessionRecoveryResult> => {
    let result: SessionRecoveryResult
    try {
      const value = await heartbeat()
      if (recoveryGeneration !== generation || authenticationEpoch() !== context.authEpoch) {
        result = staleResult(context.authEpoch)
      } else if (context.sessionBinding !== undefined && value.sessionBinding !== context.sessionBinding) {
        result = subjectChangedResult(context.authEpoch)
      } else {
        result = { recovered: true, value, authEpoch: context.authEpoch }
      }
    } catch (error) {
      if (recoveryGeneration !== generation || authenticationEpoch() !== context.authEpoch) {
        result = staleResult(context.authEpoch)
      } else if (isSubjectChanged(error)) {
        result = subjectChangedResult(context.authEpoch)
      } else {
        result = { recovered: false, error, authFailure: isUnauthorized(error), authEpoch: context.authEpoch }
      }
    }
    if (recoveryGeneration === generation && authenticationEpoch() === context.authEpoch) {
      completedAttempts += 1
      remember(attempt, {
        authEpoch: context.authEpoch,
        sessionBinding: context.sessionBinding,
        activityAuthorized: entry.activityAuthorized,
        result,
      })
    }
    return result
  })()

  running = entry
  void entry.promise.finally(() => {
    if (running === entry) running = undefined
  })
  return entry.promise
}

export function recoverIdleSession(
  context: SessionHeartbeatContext,
  heartbeat: () => Promise<SessionHeartbeatPayload>,
) {
  return coordinateHeartbeat(context, heartbeat)
}

export function runSessionHeartbeat(
  context: SessionHeartbeatContext,
  heartbeat: () => Promise<SessionHeartbeatPayload>,
) {
  // 公共 heartbeat 也必须持有真实活动许可，不能依赖调用方自律。
  return coordinateHeartbeat(context, heartbeat)
}
