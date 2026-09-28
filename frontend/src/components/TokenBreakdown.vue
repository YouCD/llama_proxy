<template>
  <div class="panel">
    <div class="section-title">{{ t('tokenBreakdownTitle') }}</div>

    <!-- 总览：三段式构成条 -->
    <div class="tb-overview">
      <div class="tb-total">
        <span class="tb-total-num mono">{{ fmtCompact(total.total) }}</span>
        <span class="tb-total-label">{{ t('metricTotalTokens') }}</span>
      </div>
      <div class="tb-bar" :title="barTitle(total)">
        <div class="tb-seg seg-input" :style="{ width: pct(total.input) }"></div>
        <div class="tb-seg seg-cached" :style="{ width: pct(total.cached) }"></div>
        <div class="tb-seg seg-output" :style="{ width: pct(total.output) }"></div>
      </div>
      <div class="tb-legend">
        <span class="tb-legend-item" :title="t('tokenLegendInput', { n: fmtNum(total.input) })">
          <i class="tb-dot seg-input"></i>{{ t('tokenInput') }} <strong class="mono">{{ fmtCompact(total.input) }}</strong>
        </span>
        <span class="tb-legend-item" :title="t('tokenLegendCached', { n: fmtNum(total.cached) })">
          <i class="tb-dot seg-cached"></i>{{ t('tokenCached') }} <strong class="mono">{{ fmtCompact(total.cached) }}</strong>
        </span>
        <span class="tb-legend-item" :title="t('tokenLegendOutput', { n: fmtNum(total.output) })">
          <i class="tb-dot seg-output"></i>{{ t('tokenOutput') }} <strong class="mono">{{ fmtCompact(total.output) }}</strong>
        </span>
        <span class="tb-hit mono" :title="t('tokenHitRateTip', { pct: fmtPctNum(total.hit) })">
          {{ t('tokenHitRate') }} <strong :class="hitTone(total.hit)">{{ fmtPctNum(total.hit) }}</strong>
        </span>
      </div>
    </div>

    <!-- 按模型 -->
    <div class="tb-models" v-if="models.length">
      <div class="tb-model-row" v-for="m in models" :key="m.model" @click="$emit('select-model', m.model)">
        <span class="tb-model-name" :title="m.model">{{ m.model }}</span>
        <div class="tb-bar tb-bar-sm" :title="barTitle(m)">
          <div class="tb-seg seg-input" :style="{ width: pct(m.input) }"></div>
          <div class="tb-seg seg-cached" :style="{ width: pct(m.cached) }"></div>
          <div class="tb-seg seg-output" :style="{ width: pct(m.output) }"></div>
        </div>
        <span class="tb-model-total mono" :title="fmtNum(m.total)">{{ fmtCompact(m.total) }}</span>
        <span class="tb-model-hit mono" :class="hitTone(m.hit)">{{ fmtPctNum(m.hit) }}</span>
      </div>
    </div>
  </div>
</template>

<script setup>
import { computed } from 'vue'
import { t } from '../i18n'
import { fmtNum, fmtCompact, fmtPctNum } from '../utils'

const props = defineProps({
  stats: { type: Object, default: () => ({}) },
  hasFilters: { type: Boolean, default: false },
})
defineEmits(['select-model'])

// 有筛选时用窗口内数据，无筛选时用全生命周期数据（与总览卡片口径一致）
const total = computed(() => {
  const s = props.stats || {}
  const prefix = props.hasFilters ? 'total' : 'lifetime'
  const input = s[`${prefix}_input_tokens`] ?? 0
  const cached = s[`${prefix}_cached_tokens`] ?? 0
  const output = s[`${prefix}_completion_tokens`] ?? 0
  const total = s[`${prefix}_total_tokens`] || (input + cached + output)
  const hit = s[`${prefix}_cache_hit_pct`] ?? (cached + input > 0 ? (cached / (cached + input)) * 100 : 0)
  return { input, cached, output, total, hit }
})

const models = computed(() => {
  const list = (props.stats || {}).token_by_model || []
  return list
    .map((m) => ({
      model: m.model,
      input: m.input_tokens || 0,
      cached: m.cached_tokens || 0,
      output: m.completion_tokens || 0,
      total: m.total_tokens || 0,
      hit: m.cache_hit_pct || 0,
    }))
    .filter((m) => m.total > 0)
})

function pct(part) {
  const sum = total.value.total
  if (!sum) return '0%'
  return (part / sum) * 100 + '%'
}

function barTitle(v) {
  return `${t('tokenInput')} ${fmtNum(v.input)} · ${t('tokenCached')} ${fmtNum(v.cached)} · ${t('tokenOutput')} ${fmtNum(v.output)}`
}

// 命中率颜色分级：≥50% 绿 / 10–50% 琥珀 / <10% 灰
function hitTone(hit) {
  if (hit >= 50) return 'hit-good'
  if (hit >= 10) return 'hit-warn'
  return 'hit-low'
}
</script>

<style scoped>
.tb-overview {
  display: flex;
  align-items: center;
  gap: 16px;
  flex-wrap: wrap;
}

.tb-total {
  display: flex;
  align-items: baseline;
  gap: 6px;
}

.tb-total-num {
  font-size: 22px;
  font-weight: 700;
  color: var(--app-text);
}

.tb-total-label {
  font-size: 12px;
  color: var(--app-muted, #94a3b8);
}

.tb-bar {
  display: flex;
  flex: 1 1 240px;
  min-width: 200px;
  height: 14px;
  border-radius: 7px;
  overflow: hidden;
  background: var(--app-line, rgba(100, 116, 139, 0.25));
}

.tb-bar-sm {
  flex: 1 1 0;
  min-width: 100px;
  height: 6px;
  border-radius: 3px;
}

.tb-seg {
  height: 100%;
  min-width: 0;
  transition: width 0.4s ease;
}

/* 配色与每日趋势图保持一致：输入蓝 / 缓存青 / 输出绿 */
.seg-input { background: #3b82f6; }
.seg-cached { background: #06b6d4; }
.seg-output { background: #22c55e; }

.tb-legend {
  display: flex;
  align-items: center;
  gap: 14px;
  flex-wrap: wrap;
  font-size: 12px;
  color: var(--app-muted, #94a3b8);
}

.tb-legend-item {
  display: inline-flex;
  align-items: center;
  gap: 5px;
}

.tb-legend-item .mono,
.tb-model-total,
.tb-model-hit {
  color: var(--app-text);
}

.tb-dot {
  display: inline-block;
  width: 9px;
  height: 9px;
  border-radius: 3px;
}

.tb-hit {
  font-weight: 600;
}

.tb-models {
  margin-top: 14px;
  display: flex;
  flex-direction: column;
  gap: 8px;
  max-height: 200px;
  overflow-y: auto;
  padding-right: 4px;
}

/* 细滚动条，避免大面积默认滚动条噪声 */
.tb-models::-webkit-scrollbar {
  width: 6px;
}
.tb-models::-webkit-scrollbar-thumb {
  background: rgba(100, 116, 139, 0.35);
  border-radius: 3px;
}
.tb-models::-webkit-scrollbar-track {
  background: transparent;
}

.tb-model-row {
  display: flex;
  align-items: center;
  gap: 12px;
  cursor: pointer;
  padding: 2px 4px;
  border-radius: 6px;
  transition: background 0.15s;
}

.tb-model-row:hover {
  background: var(--app-hover, rgba(100, 116, 139, 0.12));
}

.tb-model-name {
  flex: 0 0 260px;
  font-size: 12px;
  font-weight: 600;
  color: var(--app-text);
  text-align: right;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

.tb-model-total {
  flex: 0 0 64px;
  text-align: right;
  font-size: 12px;
}

.tb-model-hit {
  flex: 0 0 56px;
  text-align: right;
  font-size: 12px;
}

/* 命中率分级配色 */
.hit-good { color: #22c55e; }
.hit-warn { color: #f59e0b; }
.hit-low { color: var(--app-muted, #94a3b8); }

@media (max-width: 900px) {
  .tb-model-name { flex-basis: 100px; }
}
</style>
