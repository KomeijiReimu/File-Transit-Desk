export function formatBytes(value?: number) {
  if (value === undefined || Number.isNaN(value)) return '—'
  if (value === 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  const index = Math.min(Math.floor(Math.log(value) / Math.log(1024)), units.length - 1)
  return `${(value / 1024 ** index).toFixed(index === 0 ? 0 : 1)} ${units[index]}`
}

export function formatDate(value?: string) {
  if (!value) return '—'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return value
  return date.toLocaleString('zh-CN', { hour12: false })
}

export function joinPath(base: string, name: string) {
  return [base.replace(/\/+$/, ''), name.replace(/^\/+/, '')].filter(Boolean).join('/')
}

export function parentPath(path: string) {
  const parts = path.split('/').filter(Boolean)
  parts.pop()
  return parts.join('/')
}

/**
 * Extract a bare share token string from arbitrary token-link representations.
 * Accepts: "abcd", "/t/abcd", "/t/abcd/download", "/share/abcd",
 * "https://host/t/abcd/upload", full URLs, etc.
 */
export function extractShareToken(raw?: string): string {
  if (!raw) return ''
  let value = raw.trim()
  try {
    if (/^https?:\/\//i.test(value)) value = new URL(value).pathname
  } catch {
    /* keep raw */
  }
  const shareMatch = value.match(/\/share\/([^/?#]+)/)
  if (shareMatch) return decodeURIComponent(shareMatch[1])
  const tokenMatch = value.match(/\/t\/([^/?#]+)/)
  if (tokenMatch) return decodeURIComponent(tokenMatch[1])
  return value.replace(/^\/+/, '').replace(/\/.+$/, '')
}

/**
 * Build a user-facing share URL pointing at the SPA share page.
 * Prefers /share/{token}; falls back to anything reasonable.
 */
export function buildShareUrl(token: string, origin?: string): string {
  const base = origin || (typeof window !== 'undefined' ? window.location.origin : '')
  if (!token) return base
  return `${base}/share/${encodeURIComponent(token)}`
}

export async function copyToClipboard(text: string): Promise<boolean> {
  if (!text) return false
  try {
    if (typeof navigator !== 'undefined' && navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(text)
      return true
    }
  } catch {
    // fall through
  }
  try {
    const el = document.createElement('textarea')
    el.value = text
    el.setAttribute('readonly', '')
    el.style.position = 'fixed'
    el.style.opacity = '0'
    document.body.appendChild(el)
    el.select()
    const ok = document.execCommand('copy')
    document.body.removeChild(el)
    return ok
  } catch {
    return false
  }
}
