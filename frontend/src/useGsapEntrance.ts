import { nextTick, onMounted, onUnmounted, type Ref, watch } from 'vue'
import { gsap } from 'gsap'

interface EntranceOptions {
  selector?: string
  refreshKey?: () => unknown
}

// 统一管理页面入场动效：只作用于组件根节点内部，并在卸载时彻底回收 GSAP 上下文。
export function useGsapEntrance(root: Ref<HTMLElement | null>, options: EntranceOptions = {}) {
  let ctx: gsap.Context | undefined
  const selector = options.selector || '[data-motion]'

  async function play() {
    await nextTick()
    ctx?.revert()
    if (!root.value) return
    ctx = gsap.context(() => {
      const reduceMotion = window.matchMedia('(prefers-reduced-motion: reduce)').matches
      if (reduceMotion) return
      gsap.from(selector, {
        autoAlpha: 0,
        y: 14,
        scale: 0.985,
        duration: 0.36,
        ease: 'power2.out',
        stagger: 0.045,
        clearProps: 'opacity,visibility,transform',
      })
    }, root.value)
  }

  onMounted(() => void play())
  if (options.refreshKey) {
    watch(options.refreshKey, (key) => {
      if (key === '' || key === null || key === undefined || key === false) return
      void play()
    })
  }
  onUnmounted(() => ctx?.revert())
}
