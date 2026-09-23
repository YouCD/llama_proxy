<template>
  <div class="daily-chart">
    <div class="chart-section">
      <h3 class="chart-title">{{ t('dailyTokenTitle') }}</h3>
      <div ref="tokenEl" class="chart-box"></div>
    </div>
    <div class="chart-section">
      <h3 class="chart-title">{{ t('dailyStatusTitle') }}</h3>
      <div ref="statusEl" class="chart-box"></div>
    </div>
    <div class="chart-section">
      <h3 class="chart-title">{{ t('dailyRequestsTitle') }}</h3>
      <div ref="requestsEl" class="chart-box"></div>
    </div>
  </div>
</template>

<script setup>
import { ref, watch, onMounted, onBeforeUnmount } from 'vue'
import * as echarts from 'echarts/core'
import { LineChart, BarChart } from 'echarts/charts'
import { GridComponent, TooltipComponent, LegendComponent } from 'echarts/components'
import { CanvasRenderer } from 'echarts/renderers'
import { t } from '../i18n'
import { fmtCompact } from '../utils'
import { currentTheme } from '../theme'

echarts.use([LineChart, BarChart, GridComponent, TooltipComponent, LegendComponent, CanvasRenderer])

const props = defineProps({
  items: { type: Array, default: () => [] },
})

const tokenEl = ref(null)
const statusEl = ref(null)
const requestsEl = ref(null)
let tokenChart = null
let statusChart = null
let requestsChart = null

const DATE_COLORS = {
  prompt: '#3b82f6',
  completion: '#22c55e',
  total: '#f59e0b',
}

const statusColor = (v) => {
  if (v <= 399) return '#22c55e'
  if (v < 500) return '#f59e0b'
  return '#ef4444'
}

// 轴/图例文字与网格线颜色跟随主题（深/浅）
function textColor() {
  return currentTheme.value === 'dark' ? '#94a3b8' : '#6b7280'
}
const GRID_LINE = 'rgba(100, 116, 139, 0.2)'

function render() {
  const items = props.items || []
  const dates = items.map((d) => d.date || '')
  const labels = {
    totalTokens: t('chartTotalTokens'),
    promptTokens: t('chartPromptTokens'),
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
    tooltip: { trigger: 'axis', valueFormatter: (v) => fmtCompact(v) },
    legend: {
      data: [labels.promptTokens, labels.completionTokens, labels.totalTokens],
      bottom: 0,
      itemWidth: 14,
      itemHeight: 8,
      textStyle: { color: textColor() },
    },
    grid: { left: 10, right: 10, top: 20, bottom: 30, containLabel: true },
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
    tooltip: { trigger: 'axis', axisPointer: { type: 'shadow' } },
    legend: {
      data: [labels.ok, labels.err4xx, labels.err5xx],
      bottom: 0,
      itemWidth: 14,
      itemHeight: 8,
      textStyle: { color: textColor() },
    },
    grid: { left: 10, right: 10, top: 20, bottom: 30, containLabel: true },
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
    tooltip: { trigger: 'axis', axisPointer: { type: 'shadow' } },
    grid: { left: 10, right: 10, top: 20, bottom: 30, containLabel: true },
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

watch(() => props.items, render, { deep: true })
watch(currentTheme, render)

onMounted(() => {
  tokenChart = echarts.init(tokenEl.value)
  statusChart = echarts.init(statusEl.value)
  requestsChart = echarts.init(requestsEl.value)
  window.addEventListener('resize', resizeAll)
  render()
})

onBeforeUnmount(() => {
  window.removeEventListener('resize', resizeAll)
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
  display: grid;
  grid-template-columns: repeat(3, 1fr);
  gap: 16px;
  margin-top: 4px;
}

@media (max-width: 900px) {
  .daily-chart {
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