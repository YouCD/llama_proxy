import { onMounted, onUnmounted, reactive } from 'vue'

// 响应式视口断点：isMobile ≤767px / isNarrow ≤480px，
// 用于移动端隐藏低价值表格列、调整表格高度等布局开关。
export function useViewport() {
  const state = reactive({ isMobile: false, isNarrow: false })
  const mqMobile = window.matchMedia('(max-width: 767px)')
  const mqNarrow = window.matchMedia('(max-width: 480px)')

  function sync() {
    state.isMobile = mqMobile.matches
    state.isNarrow = mqNarrow.matches
  }

  onMounted(() => {
    sync()
    mqMobile.addEventListener('change', sync)
    mqNarrow.addEventListener('change', sync)
  })
  onUnmounted(() => {
    mqMobile.removeEventListener('change', sync)
    mqNarrow.removeEventListener('change', sync)
  })

  return state
}
