export const SESSION_ACTIVITY_WINDOW_MS = 75_000

export interface SessionActivityPermit {
  generation: number
  recordedAt: number
}

let generation = 0
let lastActivityAt = Number.NEGATIVE_INFINITY
let initialNavigationSeen = false

export function recordSessionActivity(now = Date.now()) {
  generation += 1
  lastActivityAt = now
  return { generation, recordedAt: lastActivityAt }
}

export function recentSessionActivity(now = Date.now()): SessionActivityPermit | undefined {
  const age = now - lastActivityAt
  if (age < 0 || age > SESSION_ACTIVITY_WINDOW_MS) return undefined
  return { generation, recordedAt: lastActivityAt }
}

export function recordInitialNavigationActivity(visible: boolean, now = Date.now()) {
  if (initialNavigationSeen) return undefined
  initialNavigationSeen = true
  return visible ? recordSessionActivity(now) : undefined
}

export function resetSessionActivity() {
  generation += 1
  lastActivityAt = Number.NEGATIVE_INFINITY
  initialNavigationSeen = false
}
