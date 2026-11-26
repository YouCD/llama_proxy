<template>
  <div v-if="data && data.available && data.gpus && data.gpus.length" ref="gridRef" class="gpu-grid">
    <div v-for="g in data.gpus" :key="g.index" class="gpu-card">
      <div class="gpu-head">
        <span class="gpu-index">GPU {{ g.index }}</span>
        <span class="gpu-name" :title="g.name">{{ g.name }}</span>
        <span class="gpu-dev" v-if="devOf(g)" :title="devOf(g)">{{ devOf(g) }}</span>
      </div>

      <div class="gpu-body">
        <div class="gauge" :class="utilTier(g.util)" :ref="(el) => setChartEl('gauge', g.index, el)"></div>

        <div class="mem-block">
          <div class="mem-label">
            <span>{{ t('gpuMem') }}</span>
            <span class="mono">{{ fmtMem(g.mem_used) }} / {{ fmtMem(g.mem_total) }}</span>
          </div>
          <div class="mem-bar">
            <div class="mem-fill" :style="{ width: memPct(g) + '%' }"></div>
          </div>
          <div class="spark-wrap" :class="utilTier(g.util)">
            <div class="spark-label">{{ t('gpuUtil') }} · {{ t('gpuHist') }}</div>
            <div class="gpu-spark" :ref="(el) => setChartEl('spark', g.index, el)"></div>
          </div>
        </div>

        <div class="mini-stats">
          <div class="mini-stat">
            <span class="ms-icon fan">
              <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
                <path d="M10.827 16.379a6.082 6.082 0 0 1-8.618-7.002l5.412 1.45a6.082 6.082 0 0 1 7.002-8.618l-1.45 5.412a6.082 6.082 0 0 1 8.618 7.002l-5.412-1.45a6.082 6.082 0 0 1-7.002 8.618l1.45-5.412Z" />
                <path d="M12 12v.01" />
              </svg>
            </span>
            <div class="ms-body">
              <div class="ms-top">
                <span class="ms-label">{{ t('gpuFan') }}</span>
                <span class="ms-val">{{ g.fan }}%</span>
              </div>
              <span class="ms-bar"><span class="ms-fill" :style="{ width: Math.min(100, g.fan) + '%' }"></span></span>
            </div>
          </div>
          <div class="mini-stat">
            <span class="ms-icon temp" :class="g.temp >= 80 ? 'bad' : g.temp >= 70 ? 'warn' : ''">
              <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
                <path d="M14 4v10.54a4 4 0 1 1-4 0V4a2 2 0 0 1 4 0Z" />
              </svg>
            </span>
            <div class="ms-body">
              <div class="ms-top">
                <span class="ms-label">{{ t('gpuTemp') }}</span>
                <span class="ms-val">{{ g.temp }}°C</span>
              </div>
              <span class="ms-bar"><span class="ms-fill" :style="{ width: Math.min(100, g.temp) + '%' }"></span></span>
            </div>
          </div>
          <div class="mini-stat">
            <span class="ms-icon power">
              <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
                <path d="M12 22v-5" />
                <path d="M9 8V2" />
                <path d="M15 8V2" />
                <path d="M18 8v5a4 4 0 0 1-4 4h-4a4 4 0 0 1-4-4V8Z" />
              </svg>
            </span>
            <div class="ms-body">
              <div class="ms-top">
                <span class="ms-label">{{ t('gpuPower') }}</span>
                <span class="ms-val">{{ g.power ? g.power.toFixed(0) + 'W' : '-' }}</span>
              </div>
              <span class="ms-bar"><span class="ms-fill" :style="{ width: Math.min(100, g.power) + '%' }"></span></span>
            </div>
          </div>
        </div>
      </div>
    </div>
  </div>
  <div v-else-if="data && data.available === false" class="gpu-unavailable">
    {{ data.error || 'nvidia-smi unavailable' }}
  </div>
  <div v-else class="gpu-unavailable">{{ t('schedLoading') }}…</div>
</template>

<script setup>
import { nextTick, onMounted, onUnmounted, ref, watch } from 'vue'
import { t } from '../i18n'
import { echarts, initChart, cssColor, rgba } from '../echarts'

const { LinearGradient } = echarts.graphic

const props = defineProps({
  data: { type: Object, default: null },
  models: { type: Array, default: () => [] },
})

// 利用率档位阈值：≥ TIER_BAD 为高危（#FF0000），≥ TIER_WARN 为偏高（黄），其余正常（青）
const TIER_WARN = 60
const TIER_BAD = 90
function utilTier(util) {
  if (util >= TIER_BAD) return 'bad'
  if (util >= TIER_WARN) return 'warn'
  return 'ok'
}

function tierColor(tier) {
  if (tier === 'bad') return '#ff0000'
  if (tier === 'warn') return cssColor('--warn')
  return cssColor('--cyan')
}

// 利用率 0–100 整体色阶：颜色随数值位置渐变（蓝→青→琥珀→红）；每次渲染读取，跟随主题切换
function scaleStops() {
  return [
    [0, cssColor('--blue')],
    [0.4, cssColor('--cyan')],
    [0.6, cssColor('--warn')],
    [0.9, cssColor('--red')],
    [1, '#ff0000'],
  ]
}

function lerpColor(a, b, k) {
  const pa = parseInt(a.slice(1), 16)
  const pb = parseInt(b.slice(1), 16)
  const c = (sh) => Math.round(((pa >> sh) & 255) + (((pb >> sh) & 255) - ((pa >> sh) & 255)) * k)
  return `rgb(${c(16)},${c(8)},${c(0)})`
}

// 色阶上位置 p（0~1）处的颜色（相邻节点线性插值）
function scaleColorAt(scale, p) {
  for (let i = 1; i < scale.length; i++) {
    if (p <= scale[i][0]) {
      const [p0, c0] = scale[i - 1]
      const [p1, c1] = scale[i]
      return lerpColor(c0, c1, (p - p0) / (p1 - p0 || 1))
    }
  }
  return scale[scale.length - 1][1]
}

// ---- ECharts 实例（每卡一个利用率环 + 一个 60s 曲线） ----
const gridRef = ref(null)
const chartEls = { gauge: {}, spark: {} }
const charts = { gauge: new Map(), spark: new Map() }
let ro = null

function setChartEl(kind, index, el) {
  if (el) chartEls[kind][index] = el
  else delete chartEls[kind][index]
}

function gaugeOption(g) {
  const tier = utilTier(g.util)
  const numColor = tier === 'ok' ? cssColor('--cyan') : tierColor(tier)
  // axisLine 色段不支持渐变，用细分段模拟 0–100 平滑色阶；v 处精确切到轨道色
  const v = Math.max(0, Math.min(100, g.util)) / 100
  const track = rgba(cssColor('--muted'), 0.14)
  const scale = scaleStops()
  const N = 100
  const stops = []
  for (let i = 0; i < N; i++) {
    const a = i / N
    const b = (i + 1) / N
    if (a >= v) {
      stops.push([b, track])
    } else if (b <= v) {
      stops.push([b, scaleColorAt(scale, (a + b) / 2)])
    } else {
      stops.push([v, scaleColorAt(scale, (a + b) / 2)])
      stops.push([b, track])
    }
  }
  return {
    animationDuration: 600,
    animationDurationUpdate: 600,
    series: [
      {
        type: 'gauge',
        center: ['50%', '50%'],
        radius: '92%',
        startAngle: 90,
        endAngle: -270,
        min: 0,
        max: 100,
        pointer: { show: false },
        anchor: { show: false },
        progress: { show: false },
        axisLine: { lineStyle: { width: 9, color: stops } },
        axisTick: { show: false },
        splitLine: { show: false },
        axisLabel: { show: false },
        title: { show: false },
        detail: {
          valueAnimation: true,
          offsetCenter: [0, 0],
          formatter: (v) => `{v|${Math.round(v)}%}\n{u|${t('gpuUtil')}}`,
          rich: {
            v: {
              fontSize: 26,
              fontWeight: 700,
              color: numColor,
              fontFamily: cssColor('--font-mono'),
              lineHeight: 34,
              align: 'center',
            },
            u: {
              fontSize: 10.5,
              color: cssColor('--muted'),
              lineHeight: 18,
              align: 'center',
            },
          },
        },
        data: [{ value: g.util }],
      },
    ],
  }
}

function sparkOption(g) {
  const arr = hist.value.get(g.index) || []
  const color = tierColor(utilTier(g.util))
  return {
    animation: false,
    grid: { left: 0, right: 0, top: 3, bottom: 3 },
    xAxis: { type: 'category', show: false, boundaryGap: false, data: arr.map((_, i) => i) },
    yAxis: { type: 'value', show: false, min: 0, max: 100 },
    series: [
      {
        type: 'line',
        data: arr.map((p) => p.util),
        showSymbol: false,
        lineStyle: { width: 1.5, color },
        areaStyle: {
          color: new LinearGradient(0, 0, 0, 1, [
            { offset: 0, color: rgba(color, 0.2) },
            { offset: 1, color: rgba(color, 0) },
          ]),
        },
      },
    ],
  }
}

function ensureChart(kind, index, el) {
  let c = charts[kind].get(index)
  if (!c) {
    c = initChart(el)
    charts[kind].set(index, c)
  }
  return c
}

function renderCharts() {
  const gpus = props.data && props.data.available ? props.data.gpus || [] : []
  const seen = new Set()
  for (const g of gpus) {
    seen.add(g.index)
    const ge = chartEls.gauge[g.index]
    if (ge) ensureChart('gauge', g.index, ge).setOption(gaugeOption(g))
    const se = chartEls.spark[g.index]
    if (se) ensureChart('spark', g.index, se).setOption(sparkOption(g))
  }
  // GPU 数量变化时销毁多余实例
  for (const kind of ['gauge', 'spark']) {
    for (const [idx, c] of charts[kind]) {
      if (!seen.has(idx)) {
        c.dispose()
        charts[kind].delete(idx)
      }
    }
  }
  // 首次出现图表时挂 ResizeObserver
  if (gridRef.value && !ro) {
    ro = new ResizeObserver(() => {
      for (const kind of ['gauge', 'spark']) charts[kind].forEach((c) => c.resize())
    })
    ro.observe(gridRef.value)
  }
}

function disposeAll() {
  for (const kind of ['gauge', 'spark']) {
    charts[kind].forEach((c) => c.dispose())
    charts[kind].clear()
  }
  if (ro) {
    ro.disconnect()
    ro = null
  }
}

// 每张 GPU 维护 60 点（1s 推送）利用率历史
const hist = ref(new Map()) // index -> [{util, mem}]
let lastTime = ''

watch(
  () => props.data,
  (d) => {
    if (!d || !d.available || !d.time || d.time === lastTime) return
    lastTime = d.time
    const m = hist.value
    for (const g of d.gpus || []) {
      const arr = m.get(g.index) || []
      arr.push({ util: g.util, mem: g.mem_used })
      if (arr.length > 60) arr.shift()
      m.set(g.index, arr)
    }
    hist.value = new Map(m)
    nextTick(renderCharts)
  },
  { immediate: true },
)

onMounted(() => {
  nextTick(renderCharts)
  window.addEventListener('themechange', renderCharts)
})

onUnmounted(() => {
  window.removeEventListener('themechange', renderCharts)
  disposeAll()
  hist.value = new Map()
})

// 该卡当前承载的调度模型（如 CUDA0 · qwen3.6）
function devOf(g) {
  const cuda = 'CUDA' + g.index
  for (const m of props.models || []) {
    if (typeof m.gpu === 'string' && m.gpu.toUpperCase() === cuda && (m.ready || m.active)) {
      return `${cuda} · ${m.id}`
    }
  }
  return cuda
}

function fmtMem(mib) {
  if (!Number.isFinite(mib) || mib <= 0) return '0M'
  if (mib >= 1024) return (mib / 1024).toFixed(1) + 'G'
  return mib + 'M'
}

function memPct(g) {
  if (!g.mem_total) return 0
  return Math.min(100, Math.round((g.mem_used / g.mem_total) * 100))
}
</script>
