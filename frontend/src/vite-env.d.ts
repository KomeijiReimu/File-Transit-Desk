/// <reference types="vite/client" />

interface ImportMetaEnv {
  readonly VITE_PUBLIC_SHARE_ORIGIN?: string
  readonly VITE_TRANSFER_ORIGIN?: string
}

interface ImportMeta {
  readonly env: ImportMetaEnv
}
