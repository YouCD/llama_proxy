<template>
  <div class="daily-chart">
    <div class="charts-grid">
      <div class="chart-section">
        <h3 class="chart-title">{{ mode === 'minute' ? t('minuteTokenTitle') : t('dailyTokenTitle') }}</h3>
        <div ref="tokenEl" class="chart-box"></div>
      </div>
      <div class="chart-section">
        <h3 class="chart-title">{{ mode === 'minute' ? t('minuteStatusTitle') : t('dailyStatusTitle') }}</h3>
        <div ref="statusEl" class="chart-box"></div>
      </div>
      <div class="chart-section">
        <h3 class="chart-title">{{ mode === 'minute' ? t('minuteRequestsTitle') : t('dailyRequestsTitle') }}</h3>
        <div ref="requestsEl" class="chart-box"></div>
      </div>
    </div>
  </div>
</template>

<script setup>
import { ref, watch, onMounted, onBeforeUnmount } from 'vue'
import * as echarts from 'echarts/core'
import { LineChart, BarChart } from 'echarts/charts'
import { GridComponent, TooltipComponent, LegendComponent, DataZoomComponent } from 'echarts/components'
import { CanvasRenderer } from 'echarts/renderers'
import { t } from '../i18n'
import { fmtCompact } from '../utils'
import { currentTheme } from '../theme'
import { attachDragZoom } from '../dragZoom'

echarts.use([LineChart, BarChart, GridComponent, TooltipComponent, LegendComponent, DataZoomComponent, CanvasRenderer])

const props = defineProps({
  items: { type: Array, default: () => [] },
  // 'day'：按天聚合（item.date）；'minute'：按分钟聚合（item.minute，'YYYY-MM-DD HH:MM'）
  mode: { type: String, default: 'day' },
})
const emit = defineEmits(['rangechange'])

const tokenEl = ref(null)
const statusEl = ref(null)
const requestsEl = ref(null)
let tokenChart = null
let statusChart = null
let requestsChart = null
let dragZoomCleanups = []

const DATE_COLORS = {
  prompt: '#3b82f6',
  cached: '#06b6d4',
  completion: '#22c55e',
  total: '#f59e0b',
}

const statusColor = (v) => {
  if (v <= 399) return '#22c55e'
  if (v < 500) return '#f59e0b'
  return '#ef4444'
}

// 每张主图：滚轮缩放（inside dataZoom）+ 拖拽框选缩放（src/dragZoom.js 实现，
// 因 ECharts 6 的 dataZoom.select 是 toolbox 专用组件、未进公共导出）。
// 注：moveOnMouseMove 默认 true（拖拽会平移缩放窗口），与拖拽框选冲突，
// 故显式置 false —— 拖拽专用于框选缩放（平移不提供，缩放/重置用滚轮与双击）。
function dataZooms() {
  return [{ type: 'inside', xAxisIndex: 0, zoomOnMouseWheel: true, moveOnMouseMove: false }]
}

// 加载动画：三图错峰入场（0/150/300ms），数据更新时平滑过渡且不额外延迟
function animOpts(delay = 0) {
  return {
    animation: true,
    animationDuration: 900,
    animationEasing: 'cubicOut',
    animationDelay: delay,
    animationDurationUpdate: 500,
    animationEasingUpdate: 'cubicInOut',
    animationDelayUpdate: 0,
  }
}

// 轴/图例文字与网格线颜色跟随主题（深/浅）
function textColor() {
  return currentTheme.value === 'dark' ? '#94a3b8' : '#6b7280'
}
const GRID_LINE = 'rgba(100, 116, 139, 0.2)'

function render() {
  const items = props.items || []
  const dates = items.map((d) => (props.mode === 'minute' ? (d.minute || '').slice(11) : d.date || ''))
  const labels = {
    totalTokens: t('chartTotalTokens'),
    promptTokens: t('chartPromptTokens'),
    cachedTokens: t('chartCachedTokens'),
    completionTokens: t('chartCompletionTokens'),
    ok: t('chartOk'),
    err4xx: t('chartErr4xx'),
    err5xx: t('chartErr5xx'),
    totalReq: t('chartTotalRequests'),
  }

  renderToken(dates, items, labels)
  renderStatus(dates, items, labels)
  renderRequests(dates, items, labels)
}

function renderToken(dates, items, labels) {
  if (!tokenChart) return
  tokenChart.setOption({
    ...animOpts(0),
    tooltip: { trigger: 'axis', axisPointer: { type: 'line' }, valueFormatter: (v) => fmtCompact(v) },
    legend: {
      data: [labels.promptTokens, labels.cachedTokens, labels.completionTokens, labels.totalTokens],
      bottom: 0,
      itemWidth: 12,
      itemHeight: 8,
      itemGap: 12,
      textStyle: { color: textColor(), fontSize: 11 },
    },
    grid: { left: 10, right: 10, top: 20, bottom: 30, containLabel: true },
    dataZoom: dataZooms(),
    xAxis: {
      type: 'category',
      data: dates,
      boundaryGap: false,
      axisLabel: { color: textColor() },
    },
    yAxis: {
      type: 'value',
      axisLabel: { color: textColor() },
      splitLine: { lineStyle: { color: GRID_LINE } },
    },
    series: [
      {
        name: labels.totalTokens,
        type: 'line',
        smooth: true,
        showSymbol: false,
        data: items.map((d) => d.total_tokens || 0),
        lineStyle: { width: 2, color: DATE_COLORS.total },
        itemStyle: { color: DATE_COLORS.total },
      },
      {
        name: labels.promptTokens,
        type: 'line',
        smooth: true,
        showSymbol: false,
        data: items.map((d) => d.prompt_tokens || 0),
        lineStyle: { width: 2, color: DATE_COLORS.prompt },
        itemStyle: { color: DATE_COLORS.prompt },
      },
      {
        name: labels.cachedTokens,
        type: 'line',
        smooth: true,
        showSymbol: false,
        data: items.map((d) => d.cached_prompt_tokens || 0),
        lineStyle: { width: 2, color: DATE_COLORS.cached },
        itemStyle: { color: DATE_COLORS.cached },
      },
      {
        name: labels.completionTokens,
        type: 'line',
        smooth: true,
        showSymbol: false,
        data: items.map((d) => d.completion_tokens || 0),
        lineStyle: { width: 2, color: DATE_COLORS.completion },
        itemStyle: { color: DATE_COLORS.completion },
      },
    ],
  })
}

function renderStatus(dates, items, labels) {
  if (!statusChart) return
  statusChart.setOption({
    ...animOpts(150),
    tooltip: { trigger: 'axis', axisPointer: { type: 'line' } },
    legend: {
      data: [labels.ok, labels.err4xx, labels.err5xx],
      bottom: 0,
      itemWidth: 14,
      itemHeight: 8,
      textStyle: { color: textColor() },
    },
    grid: { left: 10, right: 10, top: 20, bottom: 30, containLabel: true },
    dataZoom: dataZooms(),
    xAxis: {
      type: 'category',
      data: dates,
      axisLabel: { color: textColor() },
    },
    yAxis: {
      type: 'value',
      axisLabel: { color: textColor() },
      splitLine: { lineStyle: { color: GRID_LINE } },
    },
    series: [
      {
        name: labels.ok,
        type: 'bar',
        stack: 'status',
        barMaxWidth: 28,
        data: items.map((d) => d.ok_requests || 0),
        itemStyle: { color: statusColor(200) },
      },
      {
        name: labels.err4xx,
        type: 'bar',
        stack: 'status',
        barMaxWidth: 28,
        data: items.map((d) => d.err4xx || 0),
        itemStyle: { color: statusColor(404) },
      },
      {
        name: labels.err5xx,
        type: 'bar',
        stack: 'status',
        barMaxWidth: 28,
        data: items.map((d) => d.err5xx || 0),
        itemStyle: { color: statusColor(500) },
      },
    ],
  })
}

function renderRequests(dates, items, labels) {
  if (!requestsChart) return
  requestsChart.setOption({
    ...animOpts(300),
    tooltip: { trigger: 'axis', axisPointer: { type: 'line' } },
    grid: { left: 10, right: 10, top: 20, bottom: 30, containLabel: true },
    dataZoom: dataZooms(),
    xAxis: {
      type: 'category',
      data: dates,
      boundaryGap: false,
      axisLabel: { color: textColor() },
    },
    yAxis: {
      type: 'value',
      axisLabel: { color: textColor() },
      splitLine: { lineStyle: { color: GRID_LINE } },
    },
    series: [
      {
        name: labels.totalReq,
        type: 'line',
        smooth: true,
        showSymbol: false,
        data: items.map((d) => d.total_requests || 0),
        lineStyle: { width: 2, color: DATE_COLORS.total },
        itemStyle: { color: DATE_COLORS.total },
      },
    ],
  })
}

function resizeAll() {
  tokenChart?.resize()
  statusChart?.resize()
  requestsChart?.resize()
}

let syncingZoom = false
let silentZoom = false
let lastRangeKey = ''

// 把所有图表的 dataZoom 统一写到 [startIdx, endIdx]（含源图）。
// 每张图只有一个 inside dataZoom 实例，显式写全部图表保证任意入口
// （滚轮 / 拖拽框选 / 双击）后窗口一致。
function applyZoomToAll(startIdx, endIdx) {
  for (const chart of [tokenChart, statusChart, requestsChart]) {
    chart?.dispatchAction({ type: 'dataZoom', dataZoomIndex: 0, startValue: startIdx, endValue: endIdx })
  }
}

// 外部重置（如筛选面板“重置”）：恢复全窗口。静默执行，不触发 rangechange 回写筛选，
// 避免把重置后的默认时间范围（今天）覆盖成全窗口。
function resetZoom() {
  const n = (props.items || []).length
  if (!n) return
  silentZoom = true
  try {
    applyZoomToAll(0, n - 1)
  } finally {
    silentZoom = false
  }
}
defineExpose({ resetZoom })

// 任意图表缩放后（滚轮 / 拖拽框选）：统一全部图表的窗口，
// 并把选中日期区间转成时间范围向外抛出
function makeZoomHandler() {
  return (e) => {
    if (syncingZoom || silentZoom) return
    const batch = (e.batch && e.batch[0]) || e
    const items = props.items || []
    const n = items.length
    if (!n) return
    let startIdx
    let endIdx
    if (typeof batch.startValue === 'number' && typeof batch.endValue === 'number') {
      startIdx = batch.startValue
      endIdx = batch.endValue
    } else {
      startIdx = Math.round(((batch.start ?? 0) / 100) * (n - 1))
      endIdx = Math.round(((batch.end ?? 100) / 100) * (n - 1))
    }
    startIdx = Math.max(0, Math.min(startIdx, n - 1))
    endIdx = Math.max(startIdx, Math.min(endIdx, n - 1))
    syncingZoom = true
    try {
      applyZoomToAll(startIdx, endIdx)
    } finally {
      syncingZoom = false
    }
    const fromDate = items[startIdx] && (props.mode === 'minute' ? items[startIdx].minute : items[startIdx].date)
    const toDate = items[endIdx] && (props.mode === 'minute' ? items[endIdx].minute : items[endIdx].date)
    if (!fromDate || !toDate) return
    let from
    let to
    if (props.mode === 'minute') {
      // 分钟桶：'YYYY-MM-DD HH:MM' → 本地时间该分钟起点 ~ 该分钟 59.999s
      from = new Date(new Date(fromDate.replace(' ', 'T')).getTime()).toISOString()
      to = new Date(new Date(toDate.replace(' ', 'T')).getTime() + 59999).toISOString()
    } else {
      from = new Date(`${fromDate}T00:00:00`).toISOString()
      to = new Date(`${toDate}T23:59:59.999`).toISOString()
    }
    const key = `${from}|${to}`
    if (key === lastRangeKey) return
    lastRangeKey = key
    emit('rangechange', { from, to })
  }
}

watch(() => props.items, render, { deep: true })
watch(currentTheme, render)

onMounted(() => {
  tokenChart = echarts.init(tokenEl.value)
  statusChart = echarts.init(statusEl.value)
  requestsChart = echarts.init(requestsEl.value)
  tokenChart.on('datazoom', makeZoomHandler())
  statusChart.on('datazoom', makeZoomHandler())
  requestsChart.on('datazoom', makeZoomHandler())
  // 双击任意图表重置全窗口（经 makeZoomHandler 统一其余图表与实例并抛 rangechange）
  const onDbl = () => {
    const items = props.items || []
    if (!items.length) return
    tokenChart.dispatchAction({ type: 'dataZoom', dataZoomIndex: 0, startValue: 0, endValue: items.length - 1 })
  }
  tokenChart.on('dblclick', onDbl)
  statusChart.on('dblclick', onDbl)
  requestsChart.on('dblclick', onDbl)
  // 拖拽框选缩放（Grafana 式）：挂在三张主图上，选区带高亮、松手放大时间轴。
  dragZoomCleanups = [
    attachDragZoom(tokenChart),
    attachDragZoom(statusChart),
    attachDragZoom(requestsChart),
  ]
  window.addEventListener('resize', resizeAll)
  render()
})

onBeforeUnmount(() => {
  window.removeEventListener('resize', resizeAll)
  for (const cleanup of dragZoomCleanups) cleanup()
  tokenChart?.dispose()
  statusChart?.dispose()
  requestsChart?.dispose()
  tokenChart = null
  statusChart = null
  requestsChart = null
})
</script>

<style scoped>
.daily-chart {
  margin-top: 4px;
}

.charts-grid {
  display: grid;
  grid-template-columns: repeat(3, 1fr);
  gap: 16px;
}

@media (max-width: 900px) {
  .charts-grid {
    grid-template-columns: 1fr;
  }
}

.chart-section {
  background: var(--app-panel);
  border: 1px solid var(--app-line);
  border-radius: var(--app-radius, 12px);
  padding: 16px;
  box-shadow: var(--app-card-shadow);
  transition: transform 0.25s cubic-bezier(0.4, 0, 0.2, 1),
              box-shadow 0.25s cubic-bezier(0.4, 0, 0.2, 1);
  transform: translateY(0);
}

/* 白色主题下图表悬停：轻微抬起 + 虚影加深 */
html:not(.dark) .chart-section {
  box-shadow: 0 2px 4px rgba(16, 24, 40, 0.06),
              0 1px 2px rgba(16, 24, 40, 0.04);
}
html:not(.dark) .chart-section:hover {
  transform: translateY(-2px);
  box-shadow: 0 6px 12px rgba(16, 24, 40, 0.12),
              0 2px 4px rgba(16, 24, 40, 0.06);
}

/* 深色主题下保持原样式 */
html.dark .chart-section:hover {
  box-shadow: 0 1px 2px rgba(0, 0, 0, 0.4);
}

.chart-title {
  margin: 0 0 10px;
  font-size: 13px;
  font-weight: 600;
  color: var(--app-text);
}

.chart-box {
  width: 100%;
  height: 260px;
}
</style>