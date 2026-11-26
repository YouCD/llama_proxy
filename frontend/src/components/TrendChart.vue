<template>
  <div class="trend">
    <div class="trend-head">
      <div class="trend-title">
        <span class="dot" :style="{ background: metric.color, boxShadow: `0 0 8px ${metric.color}` }"></span>
        {{ t('trendTitle') }} · {{ metric.label }}
      </div>
      <div class="trend-ctrl">
        <div class="seg" :style="{ '--seg-color': metric.color }">
          <button v-for="m in metrics" :key="m.key" :class="{ active: m.key === metricKey }" @click="metricKey = m.key">
            {{ m.label }}
          </button>
        </div>
        <div class="seg">
          <button v-for="w in windows" :key="w.secs" :class="{ active: w.secs === winSecs }" @click="winSecs = w.secs">
            {{ w.label }}
          </button>
        </div>
        <button class="trend-btn" @click="localPaused = !localPaused">
          {{ localPaused ? t('trendResume') : t('trendPause') }}
        </button>
      </div>
    </div>

    <div class="trend-stats">
      <div class="trend-stat" v-for="s in trendStats" :key="s.label">
        <div class="ts-label">{{ s.label }}</div>
        <div class="ts-value">{{ s.value }}</div>
      </div>
    </div>

    <div class="trend-canvas-wrap" ref="wrapRef" @dblclick="refresh">
      <div ref="chartRef" class="trend-echart"></div>
      <div v-if="!loading && !points.length" class="trend-empty">{{ t('trendNoData') }}</div>
    </div>
  </div>
</template>

<script setup>
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { t } from '../i18n'
import { fetchMinuteStats, fetchDailyStats } from '../api'
import { fmtNum, fmtCompact, fmtDuration, fmtPercent, loadUI, saveUI } from '../utils'
import { echarts, initChart, cssColor, rgba } from '../echarts'

const { LinearGradient } = echarts.graphic

const props = defineProps({
  paused: { type: Boolean, default: false },
  filters: { type: Object, default: () => ({}) },
})

const METRIC_DEFS = [
  { key: 'req', labelKey: 'trendReq', color: 'var(--cyan)', colorVar: '--cyan', field: 'total_requests', kind: 'count' },
  { key: 'tok', labelKey: 'trendTok', color: 'var(--green)', colorVar: '--green', field: 'total_tokens', kind: 'count' },
  { key: 'lat', labelKey: 'trendLat', color: 'var(--blue)', colorVar: '--blue', field: 'avg_first_byte_ms', kind: 'ms' },
  { key: 'err', labelKey: 'trendErr', color: 'var(--red)', colorVar: '--red', field: 'errors', kind: 'count' },
]
const WINDOW_DEFS = [
  { secs: 3600, labelKey: 'trendWin1h' },
  { secs: 86400, labelKey: 'trendWin24h' },
  { secs: 30 * 86400, labelKey: 'trendWin30d', daily: true, days: 30 },
]

// Token 拆分：总计 / 输入 / 输出 / 缓存命中（后端已返回这些字段）
const TOKEN_SUBS = [
  { key: 'total', field: 'total_tokens', labelKey: 'trendTokTotal', colorVar: '--green' },
  { key: 'prompt', field: 'prompt_tokens', labelKey: 'trendTokPrompt', colorVar: '--blue' },
  { key: 'completion', field: 'completion_tokens', labelKey: 'trendTokCompletion', colorVar: '--violet' },
  { key: 'cached', field: 'cached_prompt_tokens', labelKey: 'trendTokCached', colorVar: '--amber' },
]

const metrics = computed(() => METRIC_DEFS.map((m) => ({ ...m, label: t(m.labelKey) })))
const windows = computed(() => WINDOW_DEFS.map((w) => ({ ...w, label: t(w.labelKey) })))
// 指标/时间窗/暂停状态持久化：刷新后恢复到上次选中的档位
const _savedTrend = loadUI('trend', {})
const metricKey = ref(METRIC_DEFS.some((m) => m.key === _savedTrend.metric) ? _savedTrend.metric : 'req')
const winSecs = ref(WINDOW_DEFS.some((w) => w.secs === _savedTrend.win) ? _savedTrend.win : 30 * 86400) // 默认 30 天
const localPaused = ref(!!_savedTrend.paused)
watch([metricKey, winSecs, localPaused], () => {
  saveUI('trend', { metric: metricKey.value, win: winSecs.value, paused: localPaused.value })
})
const paused = computed(() => props.paused || localPaused.value)

const metric = computed(() => metrics.value.find((m) => m.key === metricKey.value) || metrics.value[0])
const fmtVal = (v) => {
  if (metric.value.kind === 'ms') return fmtDuration(v)
  return fmtCompact(v)
}

const items = ref([])
const loading = ref(false)

const points = computed(() =>
  items.value.map((it) => {
    let v = it[metric.value.field]
    if (metric.value.field === 'errors') v = (it.err4xx || 0) + (it.err5xx || 0)
    return { time: it.minute || it.date, v: Number(v) || 0 }
  })
)

// Token 模式下，每个时间点的 4 个分量
const isTokMode = computed(() => metricKey.value === 'tok')
const tokenPoints = computed(() =>
  items.value.map((it) => ({
    time: it.minute || it.date,
    total: Number(it.total_tokens) || 0,
    prompt: Number(it.prompt_tokens) || 0,
    completion: Number(it.completion_tokens) || 0,
    cached: Number(it.cached_prompt_tokens) || 0,
  }))
)

const trendStats = computed(() => {
  const vals = points.value.map((p) => p.v)
  if (!vals.length) return []
  const sum = vals.reduce((a, b) => a + b, 0)
  const avg = sum / vals.length
  const out = [
    { label: t('trendMax'), value: fmtVal(Math.max(...vals)) },
    { label: t('trendAvg'), value: fmtVal(avg) },
    { label: t('trendMin'), value: fmtVal(Math.min(...vals)) },
  ]
  if (metric.value.kind === 'count') out.push({ label: t('trendSum'), value: fmtCompact(sum) })
  else out.push({ label: t('trendCur'), value: fmtVal(vals[vals.length - 1]) })
  // Token 模式：追加窗口内缓存命中率（缓存命中 / 输入）
  if (isTokMode.value) {
    const tp = tokenPoints.value
    const sumPrompt = tp.reduce((a, p) => a + p.prompt, 0)
    const sumCached = tp.reduce((a, p) => a + p.cached, 0)
    out.push({ label: t('trendCacheHit'), value: sumPrompt > 0 ? fmtPercent(sumCached / sumPrompt) : '0%' })
  }
  return out
})

// ---- ECharts 绘制 ----
const wrapRef = ref(null)
const chartRef = ref(null)
let chart = null
let ro = null

// 时间轴短标签：仅 30 天档（日级数据）取 MM-DD，其余（1h/24h 分钟级）取 HH:mm
function labelOf(p) {
  const m = String(p.time || '').slice(5)
  return winSecs.value > 86400 ? m.slice(0, 5) : m.slice(6) || m
}

function option() {
  const pts = points.value
  const color = cssColor(metric.value.colorVar)
  // 30 天档（日级）用 auto 自适应宽度；分钟级窗口（1h/24h）手动约 8 个刻度避免过密
  const win = WINDOW_DEFS.find((w) => w.secs === winSecs.value)
  const isDaily = !!win && win.daily
  const xStep = isDaily ? 1 : Math.max(1, Math.round((pts.length - 1) / 7))
  const tokMode = isTokMode.value
  const timeData = pts.map((p) => p.time)

  // Token 模式：4 条线（总计/输入/输出/缓存命中）；其余指标：单线 + 面积 + 末点标记
  let series, legend
  if (tokMode) {
    const tp = tokenPoints.value.map((p) => ({ ...p, fresh: Math.max(0, p.prompt - p.cached) }))
    // 堆积顺序（底→顶）：缓存命中 + 输入·未命中 + 输出 = 总计（柱高）
    const STACK = [
      { key: 'cached', labelKey: 'trendTokCached', colorVar: '--amber' },
      { key: 'fresh', labelKey: 'trendTokFresh', colorVar: '--blue' },
      { key: 'completion', labelKey: 'trendTokCompletion', colorVar: '--violet' },
    ]
    legend = {
      top: 0,
      right: 8,
      itemWidth: 12,
      itemHeight: 8,
      icon: 'roundRect',
      textStyle: { color: cssColor('--muted'), fontSize: 11 },
      data: STACK.map((s) => ({ name: t(s.labelKey) })),
      // 总计：线型图例项，与柱子区分
    }
    series = STACK.map((s) => ({
      name: t(s.labelKey),
      type: 'bar',
      stack: 'tok',
      // 顶层 color + itemStyle：让柱子、tooltip 圆点、图例三者同色
      color: cssColor(s.colorVar),
      itemStyle: { color: cssColor(s.colorVar) },
      data: tp.map((p) => p[s.key]),
      barMaxWidth: 7, // 细柱，突出总计折线
      emphasis: { focus: 'series' },
    }))
    // 总计折线：描出堆积柱顶边（total = cached+fresh+completion）
    series.push({
      name: t('trendTokTotal'),
      type: 'line',
      color: cssColor('--green'),
      data: tp.map((p) => p.total),
      showSymbol: false,
      smooth: true,
      lineStyle: { width: 2, color: cssColor('--green'), join: 'round' },
      z: 10,
    })
    legend.data.push({ name: t('trendTokTotal'), icon: 'roundRect' })
  } else {
    series = [
      {
        type: 'line',
        data: pts.map((p) => p.v),
        showSymbol: false,
        smooth: true,
        lineStyle: { width: 1.8, color, join: 'round' },
        areaStyle: {
          color: new LinearGradient(0, 0, 0, 1, [
            { offset: 0, color: rgba(color, 0.27) },
            { offset: 1, color: rgba(color, 0) },
          ]),
        },
      },
      {
        type: 'scatter',
        data: pts.map((p, i) => (i === pts.length - 1 ? p.v : null)),
        symbolSize: 6,
        itemStyle: { color },
        silent: true,
        tooltip: { show: false },
      },
    ]
  }

  return {
    animation: false,
    grid: { left: 44, right: 8, top: tokMode ? 26 : 10, bottom: 20 },
    legend,
    xAxis: {
      type: 'category',
      boundaryGap: tokMode, // 柱状图需要桶间隔；折线图贴边
      data: timeData,
      axisLine: { show: false },
      axisTick: { show: false },
      axisLabel: {
        color: cssColor('--muted'),
        fontSize: 10,
        fontFamily: cssColor('--font-mono'),
        interval: isDaily ? 'auto' : 0,
        formatter: isDaily
          ? (val) => labelOf({ time: val })
          : (val, idx) => (idx % xStep === 0 || idx === pts.length - 1 ? labelOf(pts[idx]) : ''),
      },
    },
    yAxis: {
      type: 'value',
      min: 0,
      max: Math.max(...pts.map((p) => p.v), 1),
      splitNumber: 3,
      axisLine: { show: false },
      axisTick: { show: false },
      axisLabel: {
        color: cssColor('--muted'),
        fontSize: 10,
        fontFamily: cssColor('--font-mono'),
        formatter: (v) => fmtCompact(v),
      },
      splitLine: { lineStyle: { color: cssColor('--line') } },
    },
    tooltip: {
      trigger: 'axis',
      showDelay: 0,
      backgroundColor: cssColor('--panel'),
      borderColor: cssColor('--line-strong'),
      padding: [6, 10],
      textStyle: { color: cssColor('--text'), fontSize: 12 },
      axisPointer: { type: 'line', lineStyle: { color: cssColor('--line-strong') } },
      formatter: (params) => {
        const arr = params || []
        if (!arr.length) return ''
        const head = `<div style="font-size:11px;color:${cssColor('--muted')}">${arr[0].axisValue}</div>`
        if (tokMode) {
          const totalName = t('trendTokTotal')
          const total = arr.find((p) => p.seriesName === totalName)
          const segs = arr.filter((p) => p.seriesName !== totalName)
          const totalVal = total ? total.value : segs.reduce((a, p) => a + p.value, 0)
          // 表格化行：标记 + 弱化名称 + 等宽右对齐数值 + 占比列
          const line = (marker, name, val, pct) =>
            `<div style="display:flex;align-items:center;gap:6px;line-height:1.7">${marker}<span style="color:${cssColor('--muted')}">${name}</span><b style="margin-left:auto;padding-left:14px;font-variant-numeric:tabular-nums">${fmtCompact(val)}</b><span style="width:42px;text-align:right;font-size:11px;opacity:.55;font-variant-numeric:tabular-nums">${pct}</span></div>`
          const rows = segs
            .map((p) => line(p.marker, p.seriesName, p.value, totalVal > 0 ? fmtPercent(p.value / totalVal) : ''))
            .join('')
          // 总计：实线分隔 + 放大字号，无占比列
          const totalRow =
            `<div style="display:flex;align-items:center;gap:6px;margin-top:5px;padding-top:5px;border-top:1px solid ${cssColor('--line-strong')};line-height:1.7">${total ? total.marker : ''}<span>${totalName}</span><b style="margin-left:auto;padding-left:14px;font-size:13px;font-variant-numeric:tabular-nums">${fmtCompact(totalVal)}</b><span style="width:42px"></span></div>`
          return head + rows + totalRow
        }
        return head + `<div>${metric.value.label}：<b>${fmtVal(arr[0].value)}</b></div>`
      },
    },
    series,
  }
}

function render() {
  if (!chartRef.value) return
  if (!chart) chart = initChart(chartRef.value)
  chart.setOption(option(), { replaceMerge: ['series'] })
}

// ---- 数据加载 ----
async function refresh() {
  loading.value = true
  try {
    const win = WINDOW_DEFS.find((w) => w.secs === winSecs.value)
    if (win && win.daily) {
      const data = await fetchDailyStats(win.days, props.filters)
      items.value = data.items || []
    } else {
      const minutes = Math.min(1440, Math.max(10, Math.round(winSecs.value / 60)))
      const data = await fetchMinuteStats(minutes, props.filters)
      items.value = data.items || []
    }
  } catch {
    items.value = []
  } finally {
    loading.value = false
    render()
  }
}

let timer = null
function startTimer() {
  stopTimer()
  timer = setInterval(() => {
    if (!paused.value && !document.hidden) refresh()
  }, 30000)
}
function stopTimer() {
  if (timer) clearInterval(timer)
  timer = null
}

onMounted(() => {
  render()
  ro = new ResizeObserver(() => chart && chart.resize())
  if (wrapRef.value) ro.observe(wrapRef.value)
  refresh()
  startTimer()
  window.addEventListener('themechange', render)
})

onUnmounted(() => {
  stopTimer()
  if (ro) ro.disconnect()
  window.removeEventListener('themechange', render)
  if (chart) chart.dispose()
  chart = null
})

watch([metricKey, winSecs, () => props.filters], () => refresh())
watch(paused, (p) => {
  if (!p) refresh()
})
</script>
