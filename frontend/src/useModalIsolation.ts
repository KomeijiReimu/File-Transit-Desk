let activeModalCount = 0
let previousBodyOverflow = ''
let previousBodyPaddingRight = ''
let appHadInert = false
let previousAppAriaHidden: string | null = null

export function acquireModalIsolation() {
  if (typeof document === 'undefined') return () => undefined

  if (activeModalCount === 0) {
    const app = document.getElementById('app')
    const scrollbarWidth = Math.max(0, window.innerWidth - document.documentElement.clientWidth)
    previousBodyOverflow = document.body.style.overflow
    previousBodyPaddingRight = document.body.style.paddingRight
    document.body.style.overflow = 'hidden'
    if (scrollbarWidth > 0) document.body.style.paddingRight = `${scrollbarWidth}px`

    if (app) {
      appHadInert = app.hasAttribute('inert')
      previousAppAriaHidden = app.getAttribute('aria-hidden')
      app.setAttribute('inert', '')
      if (document.activeElement instanceof HTMLElement && app.contains(document.activeElement)) {
        document.activeElement.blur()
      }
      app.setAttribute('aria-hidden', 'true')
    }
  }

  activeModalCount += 1
  let released = false

  return () => {
    if (released) return
    released = true
    activeModalCount = Math.max(0, activeModalCount - 1)
    if (activeModalCount > 0) return

    document.body.style.overflow = previousBodyOverflow
    document.body.style.paddingRight = previousBodyPaddingRight
    const app = document.getElementById('app')
    if (!app) return
    if (!appHadInert) app.removeAttribute('inert')
    if (previousAppAriaHidden === null) app.removeAttribute('aria-hidden')
    else app.setAttribute('aria-hidden', previousAppAriaHidden)
  }
}
