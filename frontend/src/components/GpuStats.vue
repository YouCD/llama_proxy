<template>
  <!-- 无 GPU / nvidia-smi 不可用时整面板隐藏（available=false） -->
  <div v-if="data && data.available && data.gpus.length" class="panel gpu-panel">
    <div class="section-title gpu-title-row">
      {{ t('gpuTitle') }}
      <span class="gpu-time mono">{{ fmtTime(data.time) }}</span>
    </div>
    <div class="gpu-grid">
      <div v-for="g in data.gpus" :key="g.index" class="gpu-card">
        <div class="gpu-head">
          <span class="gpu-id">GPU {{ g.index }}</span>
          <span class="gpu-name" :title="g.name">{{ g.name }}</span>
        </div>
        <div class="gpu-metric">
          <div class="gpu-metric-head">
            <span class="gpu-label">{{ t('gpuUtil') }}</span>
            <span class="mono gpu-value">{{ g.util }}%</span>
          </div>
          <el-progress :percentage="g.util" :show-text="false" :stroke-width="8" :color="utilColor(g.util)" />
        </div>
        <div class="gpu-metric">
          <div class="gpu-metric-head">
            <span class="gpu-label">{{ t('gpuMem') }}</span>
            <span class="mono gpu-value">{{ fmtMem(g.mem_used) }} / {{ fmtMem(g.mem_total) }}</span>
          </div>
          <el-progress :percentage="memPct(g)" :show-text="false" :stroke-width="8" color="var(--app-accent)" />
        </div>
        <div class="gpu-stats-row">
          <span class="gpu-stat">
            <span class="gpu-stat-label">{{ t('gpuFan') }}</span>
            <b class="mono">{{ g.fan }}%</b>
          </span>
          <span class="gpu-stat">
            <span class="gpu-stat-label">{{ t('gpuTemp') }}</span>
            <b class="mono">{{ g.temp }}°C</b>
          </span>
          <span class="gpu-stat">
            <span class="gpu-stat-label">{{ t('gpuPower') }}</span>
            <b class="mono">{{ g.power.toFixed(0) }}W</b>
          </span>
        </div>
      </div>
    </div>
  </div>
</template>

<script setup>
import { t } from '../i18n'

defineProps({
  data: { type: Object, default: null },
})

// MiB → 人读（≥1024 转 GiB 一位小数）
function fmtMem(mib) {
  if (!Number.isFinite(mib) || mib <= 0) return '0M'
  if (mib >= 1024) return (mib / 1024).toFixed(1) + 'G'
  return mib + 'M'
}

function memPct(g) {
  if (!g.mem_total) return 0
  return Math.min(100, Math.round((g.mem_used / g.mem_total) * 100))
}

// 利用率着色：低绿、中高黄、满红（与主题变量一致）
function utilColor(util) {
  if (util >= 90) return 'var(--app-bad)'
  if (util >= 60) return 'var(--app-warn)'
  return 'var(--app-ok)'
}

function fmtTime(iso) {
  if (!iso) return ''
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return ''
  const p = (n) => String(n).padStart(2, '0')
  return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`
}
</script>

<style scoped>
.gpu-title-row {
  display: flex;
  align-items: baseline;
  gap: 10px;
}
.gpu-time {
  font-size: 12px;
  color: var(--app-muted);
}
.gpu-grid {
  display: flex;
  flex-wrap: wrap;
  gap: 12px;
  margin-top: 10px;
}
.gpu-card {
  flex: 1 1 260px;
  min-width: 240px;
  display: flex;
  flex-direction: column;
  gap: 10px;
  padding: 12px 16px;
  border: 1px solid var(--app-line);
  border-radius: 10px;
  background: rgba(255, 255, 255, 0.015);
}
.gpu-head {
  display: flex;
  align-items: baseline;
  gap: 8px;
  min-width: 0;
}
.gpu-id {
  font-weight: 600;
  flex: none;
}
.gpu-name {
  font-size: 12px;
  color: var(--app-muted);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.gpu-metric {
  display: flex;
  flex-direction: column;
  gap: 5px;
}
.gpu-metric-head {
  display: flex;
  justify-content: space-between;
  align-items: baseline;
}
.gpu-label {
  font-size: 12px;
  color: var(--app-muted);
}
.gpu-value {
  font-size: 12px;
}
.gpu-stats-row {
  display: flex;
  gap: 16px;
  border-top: 1px solid var(--app-line);
  padding-top: 9px;
}
.gpu-stat {
  display: flex;
  flex-direction: column;
  gap: 2px;
  font-size: 12px;
}
.gpu-stat-label {
  color: var(--app-muted);
  font-size: 11px;
}
.gpu-stat b {
  font-weight: 600;
}
</style>
