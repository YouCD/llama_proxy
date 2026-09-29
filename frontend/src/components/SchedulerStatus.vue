<template>
  <div class="panel" v-if="data && data.enabled">
    <div class="section-title">{{ t('schedulerTitle') }}</div>
    <div class="sched-row">
      <span class="sched-dot" :class="modeDot" :title="modeTitle"></span>
      <span class="sched-mode">{{ modeLabel }}</span>
      <span class="sched-cell mono" v-if="data.model">{{ data.model }}</span>
      <span class="sched-sep">·</span>
      <span class="sched-ready" :class="readyTone">{{ readyLabel }}</span>
      <span class="sched-sep">·</span>
      <span class="sched-cell mono">{{ t('schedPid') }} {{ data.pid || '-' }}</span>
    </div>
    <div class="sched-meta">
      <span class="sched-cell" :title="t('schedBase')">
        <strong class="mono">{{ data.active_base_url || '-' }}</strong>
      </span>
      <template v-if="!isBackground">
        <span class="sched-cell mono">{{ t('schedLease') }} {{ fmtSec(data.lease_seconds) }}</span>
        <span class="sched-cell mono">{{ t('schedRemaining') }} {{ fmtSec(data.lease_remaining_seconds) }}</span>
        <span class="sched-cell mono">{{ t('schedIdle') }} {{ fmtSec(data.idle_seconds) }}</span>
      </template>
    </div>
  </div>
</template>

<script setup>
import { computed } from 'vue'
import { t } from '../i18n'

const props = defineProps({
  data: { type: Object, default: null },
})

function fmtSec(v) {
  if (v === undefined || v === null || v < 0) return '-'
  if (v >= 60) return `${Math.floor(v / 60)}m${v % 60 ? v % 60 + 's' : ''}`
  return `${v}s`
}

const modeDot = computed(() => {
  if (!props.data) return 'dot-idle'
  if (props.data.mode === 'coding') return 'dot-coding'
  return 'dot-ok'
})
const modeTitle = computed(() => {
  if (!props.data) return ''
  return props.data.mode === 'coding' ? t('schedModeCoding') : t('schedModeBackground')
})
const modeLabel = computed(() => {
  if (!props.data) return '-'
  return props.data.mode === 'coding' ? t('schedModeCoding') : t('schedModeBackground')
})
const readyLabel = computed(() => {
  if (!props.data) return '-'
  return props.data.ready ? t('schedReady') : t('schedNotReady')
})
const readyTone = computed(() => (props.data && props.data.ready ? 'tone-good' : 'tone-warn'))
const isBackground = computed(() => !!(props.data && props.data.mode === 'background'))
</script>

<style scoped>
.sched-row {
  display: flex;
  align-items: center;
  gap: 6px;
  margin-bottom: 6px;
}
.sched-dot {
  width: 10px;
  height: 10px;
  border-radius: 50%;
  display: inline-block;
  flex: none;
}
.dot-ok { background: #67c23a; }
.dot-coding { background: #409eff; }
.dot-idle { background: #909399; }
.sched-mode { font-weight: 600; }
.sched-sep { color: var(--el-text-color-placeholder); }
.sched-meta {
  display: flex;
  flex-wrap: wrap;
  gap: 4px 14px;
  align-items: center;
}
.sched-cell { font-size: 13px; color: var(--el-text-color-regular); }
.sched-ready { font-size: 13px; font-weight: 600; }
.mono { font-family: var(--el-font-family-mono, monospace); }
.tone-good { color: #67c23a; }
.tone-warn { color: #e6a23c; }
</style>