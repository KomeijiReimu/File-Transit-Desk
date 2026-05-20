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
  // 前端只拼接相对展示路径；真正的越权校验仍由后端 fsutil 负责。
  return [base.replace(/\/+$/, ''), name.replace(/^\/+/, '')].filter(Boolean).join('/')
}

export function parentPath(path: string) {
  // 空路径表示目录根，继续返回空字符串，便于“返回上级”按钮自然禁用。
  const parts = path.split('/').filter(Boolean)
  parts.pop()
  return parts.join('/')
}

/**
 * 从任意分享链接形态中提取裸 token。
 * 兼容："abcd"、"/t/abcd"、"/t/abcd/download"、"/share/abcd"、完整 URL 等。
 */
export function extractShareToken(raw?: string): string {
  if (!raw) return ''
  let value = raw.trim()
  try {
    if (/^https?:\/\//i.test(value)) value = new URL(value).pathname
  } catch {
    // URL 解析失败时保留原始字符串，后续正则仍可能提取到 token。
  }
  const shareMatch = value.match(/\/share\/([^/?#]+)/)
  if (shareMatch) return decodeURIComponent(shareMatch[1])
  const tokenMatch = value.match(/\/t\/([^/?#]+)/)
  if (tokenMatch) return decodeURIComponent(tokenMatch[1])
  return value.replace(/^\/+/, '').replace(/\/.+$/, '')
}

/**
 * 构造面向用户的前端分享页地址，避免把后端兼容 HTML 接口直接暴露给用户。
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
    // Clipboard API 不可用时退回到隐藏 textarea，兼容较旧浏览器或非安全上下文。
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
