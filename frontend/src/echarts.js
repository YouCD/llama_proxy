// ECharts 共享入口：tree-shaking 引入（line + gauge + scatter + canvas renderer），
// 全站图表（KPI 迷你趋势、GPU 利用率环、60s 利用率曲线、分钟趋势）统一使用。
import * as echarts from 'echarts/core'
import { LineChart, GaugeChart, ScatterChart, BarChart } from 'echarts/charts'
import { GridComponent, TooltipComponent, LegendComponent } from 'echarts/components'
import { CanvasRenderer } from 'echarts/renderers'

echarts.use([LineChart, GaugeChart, ScatterChart, BarChart, GridComponent, TooltipComponent, LegendComponent, CanvasRenderer])

export { echarts }

/** 读取 CSS 变量实际值（ECharts 画到 canvas，不能用 var()） */
export function cssColor(name) {
  return getComputedStyle(document.documentElement).getPropertyValue(name).trim() || '#888'
}

/** #rrggbb -> rgba(r, g, b, alpha)；无法解析时原样返回 */
export function rgba(hex, alpha) {
  let m = String(hex || '').trim().replace(/^#/, '')
  if (m.length === 3) m = m.split('').map((c) => c + c).join('')
  if (!/^[0-9a-f]{6}$/i.test(m)) return hex
  const n = parseInt(m, 16)
  return `rgba(${(n >> 16) & 255}, ${(n >> 8) & 255}, ${n & 255}, ${alpha})`
}

/** 在 el 上初始化 ECharts 实例。
 * 部分环境（如 Linux X11）的 devicePixelRatio 不反映屏幕实际缩放，
 * canvas 按低倍渲染上屏后被放大出现锯齿，故强制最小 3x 背板（超采样）。 */
export function initChart(el) {
  const dpr = Math.max(window.devicePixelRatio || 1, 3)
  return echarts.init(el, null, { devicePixelRatio: dpr })
}
