<template>
  <div class="stats-groups">
    <div class="stats-grid">
      <div
        class="stat-card"
        :class="{ [c.tone]: !!c.tone, ['tier-' + (c.tier || 'secondary')]: true }"
        v-for="c in allCards"
        :key="c.key"
      >
        <div class="label">{{ c.label }}</div>
        <div class="value" :title="c.exact" :class="{ 'value-sm': String(c.format(c.value)).length > 6 }">
          <AnimatedNumber :value="c.value" :format="c.format" :step="c.step || 0" />
        </div>
        <div class="foot" :title="c.foot">{{ c.foot }}</div>
      </div>
    </div>
  </div>
</template>

<script setup>
import { computed } from 'vue'
import AnimatedNumber from './AnimatedNumber.vue'
import { t } from '../i18n'
import { fmtNum, fmtDuration, fmtPctNum, errRateTone } from '../utils'

const props = defineProps({
  stats: { type: Object, default: () => ({}) },
  llmStats: { type: Object, default: () => ({}) },
  hasFilters: { type: Boolean, default: false },
  outputSec: { type: Number, default: 0 },
  windowLabel: { type: String, default: '' },
})

const groups = computed(() => {
  const s = props.stats || {}
  const llm = props.llmStats || {}
  const windowLabel = props.windowLabel || ''
  const totalMatching = props.hasFilters
    ? (s.matching_total_requests || 0)
    : (s.lifetime_total_requests || 0)

  const llmError = (llm.error_rate || 0) * 100
  const totalError = (s.error_rate || 0) * 100
  const llmTone = errRateTone(llm.error_rate || 0)
  const errFoot = totalError > 0 && Math.abs(totalError - llmError) > 0.001
    ? t('metricErrProbeTip', { pct: fmtPctNum(totalError) })
    : windowLabel

  return [
    {
      title: t('groupFlow'),
      cards: [
        { key: 'reqhour', label: t('metricReqHour'), value: Math.round((s.requests_per_minute || 0) * 60), format: fmtNum, foot: windowLabel, step: 1, tier: 'primary' },
        { key: 'output', label: t('metricOutputSec'), value: props.outputSec, format: (v) => Math.round(v).toString(), foot: t('metricShownRows'), tier: 'primary' },
      ],
    },
    {
      title: t('groupQuality'),
      cards: [
        { key: 'ttft', label: t('metricAvgTtft'), value: s.avg_first_byte_ms || 0, format: fmtDuration, foot: t('metricFirstToken'), tier: 'secondary' },
        { key: 'err', label: t('metricErrorRateLLM'), value: llmError, format: fmtPctNum, tone: llmTone, foot: errFoot, tier: 'secondary' },
      ],
    },
    {
      title: t('groupResource'),
      cards: [
        { key: 'total', label: t('metricTotalReq'), value: totalMatching, format: fmtNum, foot: windowLabel, step: 1, tier: 'summary' },
      ],
    },
  ]
})

const allCards = computed(() => groups.value.flatMap(g => g.cards))
</script>

<style scoped>
.stats-grid {
  display: grid;
  gap: 12px;
  /* 固定宽卡片：每张 220px，左右顶格，卡间距等分剩余空间 */
  grid-template-columns: repeat(auto-fit, 220px);
  justify-content: space-between;
}
@media (max-width: 799px) {
  /* 手机宽度：两列等分（单卡约 169px），间距固定 8px；
     不用 space-between——固定宽卡片被推到两端会在中间挤出大缝隙 */
  .stats-grid {
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: 8px;
  }
}
@media (max-width: 360px) {
  /* 超窄屏兑底：单列流式 */
  .stats-grid {
    grid-template-columns: minmax(0, 1fr);
  }
}
</style>
