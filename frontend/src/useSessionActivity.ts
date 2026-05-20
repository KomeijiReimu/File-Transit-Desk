import { onMounted, onUnmounted, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ApiError, api } from '@/api'
import { authState, clearUser } from '@/auth'

const HEARTBEAT_INTERVAL_MS = 30_000

export function useSessionActivity() {
  const route = useRoute()
  const router = useRouter()
  let lastHeartbeatAt = 0
  let heartbeatRunning = false

  const shouldTrack = () => authState.authenticated && route.meta.public !== true && document.visibilityState === 'visible'

  function expireLocally() {
    clearUser()
    if (route.meta.public === true || route.name === 'login') return
    router.replace({ name: 'login', query: { redirect: route.fullPath } })
  }

  async function sendHeartbeat(force = false) {
    if (!shouldTrack() || heartbeatRunning) return
    const now = Date.now()
    if (!force && now - lastHeartbeatAt < HEARTBEAT_INTERVAL_MS) return
    heartbeatRunning = true
    lastHeartbeatAt = now
    try {
      const result = await api.heartbeat()
      // 后端返回新的空闲过期时间，前端只用于展示和本地判断；最终仍以后端鉴权为准。
      if (authState.user && result.idleExpiresAt) authState.user.idleExpiresAt = result.idleExpiresAt
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) expireLocally()
    } finally {
      heartbeatRunning = false
    }
  }

  function markActivity() {
    if (!shouldTrack()) return
    sendHeartbeat()
  }

  function handleVisibilityChange() {
    if (document.visibilityState === 'visible') sendHeartbeat(true)
  }

  const events = ['pointerdown', 'keydown', 'scroll', 'touchstart'] as const

  onMounted(() => {
    events.forEach((event) => window.addEventListener(event, markActivity, { passive: true }))
    document.addEventListener('visibilitychange', handleVisibilityChange)
    window.addEventListener('focus', handleVisibilityChange)
  })

  onUnmounted(() => {
    events.forEach((event) => window.removeEventListener(event, markActivity))
    document.removeEventListener('visibilitychange', handleVisibilityChange)
    window.removeEventListener('focus', handleVisibilityChange)
  })

  watch(
    () => [authState.authenticated, route.fullPath],
    () => {
      if (shouldTrack()) sendHeartbeat(true)
    },
    { immediate: true },
  )
}
