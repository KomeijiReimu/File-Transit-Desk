import { onMounted, onUnmounted, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ApiError, api } from '@/api'
import { authState, clearUser } from '@/auth'

const HEARTBEAT_INTERVAL_MS = 30_000
let uploadSessionHold = false

export function setUploadSessionHold(active: boolean) {
  uploadSessionHold = active
}

export function useSessionActivity() {
  const route = useRoute()
  const router = useRouter()
  let lastHeartbeatAt = 0
  let heartbeatRunning = false

  // 只有登录态、非公开页、页面可见时才保活；用户离开页面后不持续刷新空闲会话。
  const shouldTrack = () => authState.authenticated && route.meta.public !== true && document.visibilityState === 'visible'

  function expireLocally() {
    clearUser()
    if (route.meta.public === true || route.name === 'login') return
    router.replace({ name: 'login', query: { redirect: route.fullPath } })
  }

  async function sendHeartbeat(force = false) {
    if (!shouldTrack() || heartbeatRunning) return
    const now = Date.now()
    // 高频输入事件只触发节流后的心跳，避免滚动或键盘长按造成请求风暴。
    if (!force && now - lastHeartbeatAt < HEARTBEAT_INTERVAL_MS) return
    heartbeatRunning = true
    lastHeartbeatAt = now
    try {
      const result = await api.heartbeat()
      // 后端返回新的空闲过期时间，前端只用于展示和本地判断；最终仍以后端鉴权为准。
      if (authState.user && result.idleExpiresAt) authState.user.idleExpiresAt = result.idleExpiresAt
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) {
        if (uploadSessionHold) {
          window.dispatchEvent(new CustomEvent('ft:upload-session-expired'))
        } else {
          expireLocally()
        }
      }
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
    // 监听真实用户活动，而不是定时无条件保活，和后端 idle_timeout 语义保持一致。
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
      // 登录成功或路由切换时立即补一次心跳，减少刚进入页面就空闲过期的误判。
      if (shouldTrack()) sendHeartbeat(true)
    },
    { immediate: true },
  )
}
