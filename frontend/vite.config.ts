import { URL } from 'node:url'
import { isIP } from 'node:net'
import { resolve } from 'node:path'
import { defineConfig } from 'vite'
import type { ProxyOptions } from 'vite'
import vue from '@vitejs/plugin-vue'

const backendOrigin = process.env.VITE_BACKEND_ORIGIN || process.env.BACKEND_ORIGIN || 'http://127.0.0.1:17878'
const backendProxyOrigin = new URL(backendOrigin).origin
const sourceDirectory = resolve(process.cwd(), 'src')

const devClientIPHeaders = [
  'X-FileTrans-Dev-Client-IP',
  'X-Real-IP',
  'X-Forwarded-For',
] as const

type MutableProxyHeaders = {
  removeHeader(name: string): void
  setHeader(name: string, value: string): unknown
}

export function canonicalDevClientIP(remoteAddress: string | undefined): string | null {
  let value = remoteAddress?.trim() || ''
  if (!value) return null
  if (value.includes(':')) {
    const zoneIndex = value.indexOf('%')
    if (zoneIndex >= 0) {
      if (zoneIndex === value.length - 1 || value.indexOf('%', zoneIndex + 1) >= 0) return null
      value = value.slice(0, zoneIndex)
    }
  }
  const family = isIP(value)
  if (family === 4) return value
  if (family !== 6) return null
  let canonical: string
  try {
    const hostname = new URL(`http://[${value}]/`).hostname
    canonical = hostname.slice(1, -1).toLowerCase()
  } catch {
    return null
  }
  const words = expandIPv6(canonical)
  if (words && words.slice(0, 5).every((word) => word === 0) && words[5] === 0xffff) {
    return `${words[6] >>> 8}.${words[6] & 0xff}.${words[7] >>> 8}.${words[7] & 0xff}`
  }
  return canonical
}

function expandIPv6(value: string): number[] | null {
  const halves = value.split('::')
  if (halves.length > 2) return null
  const parseHalf = (half: string) => half === ''
    ? []
    : half.split(':').map((word) => Number.parseInt(word, 16))
  const left = parseHalf(halves[0])
  const right = halves.length === 2 ? parseHalf(halves[1]) : []
  if ([...left, ...right].some((word) => !Number.isInteger(word) || word < 0 || word > 0xffff)) return null
  const omitted = 8 - left.length - right.length
  if (halves.length === 1 && omitted !== 0 || halves.length === 2 && omitted < 1) return null
  return [...left, ...Array.from({ length: omitted }, () => 0), ...right]
}

export function overwriteDevClientIPHeaders(proxyRequest: MutableProxyHeaders, remoteAddress: string | undefined): void {
  const clientIP = canonicalDevClientIP(remoteAddress)
  for (const header of devClientIPHeaders) proxyRequest.removeHeader(header)
  if (!clientIP) return
  for (const header of devClientIPHeaders) proxyRequest.setHeader(header, clientIP)
}

const backendProxy = (target: string): ProxyOptions => ({
  target,
  changeOrigin: true,
  timeout: 0,
  proxyTimeout: 0,
  configure(proxy) {
    proxy.on('proxyReq', (proxyReq, req) => {
      // Never forward or append browser-supplied client-IP headers. The backend
      // accepts this development-only identity only from a loopback socket peer.
      overwriteDevClientIPHeaders(proxyReq, req.socket.remoteAddress)
      // 局域网访问 Vite 时浏览器 Origin 会是 http://局域网IP:5173；开发代理转发到后端前改成后端同源，避免触发后端 CSRF Origin 防护。
      proxyReq.setHeader('Origin', backendProxyOrigin)
    })
  },
})

const longUploadDevServer = () => ({
  name: 'file-trans-long-upload-dev-server',
  configureServer(server: { httpServer?: { requestTimeout?: number; headersTimeout?: number; timeout?: number } }) {
    if (!server.httpServer) return
    // 2G 级上传在开发环境会先经过 Vite 的 Node HTTP 服务器；关闭这里的请求超时，避免代理层尚未报错前连接被 Node 主动断开。
    server.httpServer.requestTimeout = 0
    server.httpServer.headersTimeout = 0
    server.httpServer.timeout = 0
  },
})

export default defineConfig({
  plugins: [vue(), longUploadDevServer()],
  resolve: {
    alias: {
      '@': sourceDirectory,
    },
  },
  server: {
    proxy: {
      '/api': backendProxy(backendOrigin),
      '^/t/': backendProxy(backendOrigin),
    },
  },
})
