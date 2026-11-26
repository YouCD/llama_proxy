<template>
  <div class="kpi-grid">
    <div
      v-for="c in cards"
      :key="c.key"
      class="kpi"
      :class="{ 'kpi-err hot': c.hot }"
      :style="{ '--kpi-color': c.color }"
    >
      <div class="kpi-top">
        <span class="kpi-icon">{{ c.icon }}</span>
        <span class="kpi-label">{{ c.label }}</span>
        <span v-if="c.deltaText" class="kpi-delta" :class="c.deltaTone">
          {{ c.deltaArrow }} {{ c.deltaText }}
        </span>
      </div>
      <div class="kpi-value" :title="c.title">{{ c.text }}<span v-if="c.unit" class="kpi-unit">{{ c.unit }}</span></div>
      <div class="kpi-spark" :ref="(el) => setSparkRef(c.key, el)"></div>
      <div class="kpi-foot">{{ c.foot }}</div>
    </div>
  </div>
</template>

<script setup>
import { computed, nextTick, onMounted, onUnmounted, watch } from 'vue'
import { t } from '../i18n'
import { fmtNum, fmtDuration, fmtPctNum, errRateTone, fmtCompact } from '../utils'
import { echarts, initChart, cssColor, rgba } from '../echarts'

const { LinearGradient } = echarts.graphic

const props = defineProps({
  stats: { type: Object, default: () => ({}) },
  llmStats: { type: Object, default: () => ({}) },
  hasFilters: { type: Boolean, default: false },
  // App 维护的秒级历史环（{ts, rpm, tps, ttft, err, total}，最多 240 点）
  history: { type: Array, default: () => [] },
})

// 与 60 秒前的值比较得出涨跌
function deltaOf(values) {
  const pts = (values || []).filter((v) => Number.isFinite(v))
  if (pts.length < 12) return null
  const cur = pts[pts.length - 1]
  const prev = pts[pts.length - 60]
  if (!Number.isFinite(prev) || prev === 0) return null
  const pct = ((cur - prev) / Math.abs(prev)) * 100
  if (!Number.isFinite(pct) || Math.abs(pct) < 0.5) return { text: '0.5%', arrow: '·', tone: 'flat' }
  return {
    text: `${Math.abs(pct).toFixed(pct >= 100 ? 0 : 1)}%`,
    arrow: pct > 0 ? '↑' : '↓',
    tone: 'flat',
  }
}

const cards = computed(() => {
  const s = props.stats || {}
  const llm = props.llmStats || {}
  const h = props.history || []
  const pick = (k) => h.map((p) => p[k])

  const rpm = s.requests_per_minute || 0
  const tps = s.decode_tokens_per_second || 0
  const ttft = s.avg_first_byte_ms || 0
  const errPct = (llm.error_rate || 0) * 100
  const total = props.hasFilters
    ? (s.matching_total_requests || 0)
    : (s.lifetime_total_requests || 0)
  const windowFoot = props.hasFilters ? t('kpiFootWindow') : t('kpiFootWindow')

  const errTone = errRateTone(llm.error_rate || 0)
  const errHot = (llm.error_rate || 0) > 0.05

  const mkDelta = (d, invert) => {
    if (!d) return { deltaText: '', deltaArrow: '', deltaTone: '' }
    // 错误率上升是坏事：颜色语义与涨跌相反
    let tone = 'flat'
    if (d.tone !== 'flat') tone = d.arrow === '↑' ? (invert ? 'bad' : 'good') : invert ? 'good' : 'bad'
    return { deltaText: d.text, deltaArrow: d.arrow, deltaTone: tone }
  }

  return [
    {
      key: 'req', icon: '⇄', label: t('kpiReqMin'), color: 'var(--cyan)', colorVar: '--cyan',
      text: fmtNum(Math.round(rpm)), unit: 'req', title: fmtNum(Math.round(rpm)),
      foot: windowFoot,
      values: pick('rpm'),
      ...mkDelta(deltaOf(pick('rpm'))),
    },
    {
      key: 'tok', icon: '⚡', label: t('kpiTokSec'), color: 'var(--green)', colorVar: '--green',
      text: tps >= 100 ? fmtNum(Math.round(tps)) : tps.toFixed(1), unit: 'tok', title: `${tps.toFixed(2)}`,
      foot: t('kpiFootWindow'),
      values: pick('tps'),
      ...mkDelta(deltaOf(pick('tps'))),
    },
    {
      key: 'ttft', icon: '◷', label: t('metricAvgTtft'), color: 'var(--blue)', colorVar: '--blue',
      text: fmtDuration(ttft).replace(/m?s$/, ''), unit: fmtDuration(ttft).match(/m?s$/) ? fmtDuration(ttft).match(/m?s$/)[0] : '',
      title: fmtDuration(ttft), foot: t('metricFirstToken'),
      values: pick('ttft'),
      ...mkDelta(deltaOf(pick('ttft')), true),
    },
    {
      key: 'err', icon: '⚠', label: t('metricErrorRateLLM'), color: 'var(--amber)', colorVar: '--amber',
      text: fmtPctNum(llm.error_rate || 0), unit: '', title: fmtPctNum(llm.error_rate || 0),
      foot: errTone ? t('metricErrProbeTip', { pct: fmtPctNum(s.error_rate || 0) }) : windowFoot,
      hot: errHot,
      values: pick('err'),
      ...mkDelta(deltaOf(pick('err')), true),
    },
    {
      key: 'total', icon: '∑', label: t('metricTotalReq'), color: 'var(--violet)', colorVar: '--violet',
      text: fmtCompact(total), unit: '', title: fmtNum(total),
      foot: props.hasFilters ? t('kpiFootWindow') : t('kpiFootLife'),
      values: pick('total'),
      ...mkDelta(deltaOf(pick('total'))),
    },
  ]
})

// ---- KPI 迷你趋势（ECharts） ----
const sparkEls = {}
const sparkCharts = new Map()
let ro = null

function setSparkRef(key, el) {
  if (el) sparkEls[key] = el
  else delete sparkEls[key]
}

function sparkOption(c) {
  const vals = (c.values || []).filter((v) => Number.isFinite(v))
  if (vals.length < 2) return { animation: false, series: [] }
  const color = cssColor(c.colorVar)
  return {
    animation: false,
    grid: { left: 0, right: 0, top: 2, bottom: 2 },
    xAxis: { type: 'category', show: false, boundaryGap: false, data: vals.map((_, i) => i) },
    yAxis: { type: 'value', show: false, min: Math.min(...vals, 0), max: Math.max(...vals, 1) },
    series: [
      {
        type: 'line',
        data: vals,
        showSymbol: false,
        lineStyle: { width: 1.6, color, opacity: 0.9 },
        areaStyle: {
          color: new LinearGradient(0, 0, 0, 1, [
            { offset: 0, color: rgba(color, 0.18) },
            { offset: 1, color: rgba(color, 0) },
          ]),
        },
      },
    ],
  }
}

function renderSparks() {
  for (const c of cards.value) {
    const el = sparkEls[c.key]
    if (!el) continue
    if (!sparkCharts.has(c.key)) sparkCharts.set(c.key, initChart(el))
    sparkCharts.get(c.key).setOption(sparkOption(c))
  }
}

// kpiHistory 在 App 里原地 push/shift，引用不变，watch 引用不会触发；
// cards 依赖该数组内容，每次重算即重新渲染迷你趋势
watch(cards, () => nextTick(renderSparks))

onMounted(() => {
  nextTick(() => {
    renderSparks()
    ro = new ResizeObserver(() => sparkCharts.forEach((c) => c.resize()))
    Object.values(sparkEls).forEach((el) => ro.observe(el))
  })
  window.addEventListener('themechange', renderSparks)
})

onUnmounted(() => {
  window.removeEventListener('themechange', renderSparks)
  if (ro) ro.disconnect()
  sparkCharts.forEach((c) => c.dispose())
  sparkCharts.clear()
})
</script>
