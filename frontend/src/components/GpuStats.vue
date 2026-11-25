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
        <div class="gpu-body">
          <div class="gpu-metrics">
            <div class="gpu-metric">
              <div class="gpu-metric-head">
                <span class="gpu-label">{{ t('gpuUtil') }}</span>
                <span class="mono gpu-value">{{ g.util }}%</span>
              </div>
              <el-progress :percentage="g.util" :show-text="false" :stroke-width="8" :color="utilColor(g.util)" :style="{ color: utilColor(g.util) }" />
            </div>
            <div class="gpu-metric">
              <div class="gpu-metric-head">
                <span class="gpu-label">{{ t('gpuMem') }}</span>
                <span class="mono gpu-value">{{ fmtMem(g.mem_used) }} / {{ fmtMem(g.mem_total) }}</span>
              </div>
              <el-progress :percentage="memPct(g)" :show-text="false" :stroke-width="8" color="var(--app-accent)" :style="{ color: 'var(--app-accent)' }" />
            </div>
          </div>
          <div class="gpu-mini">
            <div class="gpu-mini-stat">
              <svg class="stat-icon fan-icon" :style="fanAnim(g.fan)" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
                <path d="M10.827 16.379a6.082 6.082 0 0 1-8.618-7.002l5.412 1.45a6.082 6.082 0 0 1 7.002-8.618l-1.45 5.412a6.082 6.082 0 0 1 8.618 7.002l-5.412-1.45a6.082 6.082 0 0 1-7.002 8.618l1.45-5.412Z" />
                <path d="M12 12v.01" />
              </svg>
              <span class="gpu-stat-label">{{ t('gpuFan') }}</span>
              <b class="mono">{{ g.fan }}%</b>
            </div>
            <div class="gpu-mini-stat">
              <svg class="stat-icon temp-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
                <path d="M14 4v10.54a4 4 0 1 1-4 0V4a2 2 0 0 1 4 0Z" />
              </svg>
              <span class="gpu-stat-label">{{ t('gpuTemp') }}</span>
              <b class="mono">{{ g.temp }}°C</b>
            </div>
            <div class="gpu-mini-stat">
              <el-icon class="stat-icon power-icon"><Lightning /></el-icon>
              <span class="gpu-stat-label">{{ t('gpuPower') }}</span>
              <b class="mono">{{ g.power.toFixed(0) }}W</b>
            </div>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>

<script setup>
import { t } from '../i18n'
import { Lightning } from '@element-plus/icons-vue'

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

// 风扇图标转速随 fan% 变化：0%（或不支持）停转，100% 最快 0.5s/圈
function fanAnim(fan) {
  const v = Number(fan)
  if (!Number.isFinite(v) || v <= 0) return { animationPlayState: 'paused' }
  return { animationDuration: `${Math.max(0.5, 4 - v * 0.035)}s` }
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
  gap: 12px;
  padding: 14px 16px;
  border: 1px solid var(--app-line);
  border-radius: 12px;
  background: linear-gradient(180deg, rgba(148, 163, 184, 0.05), rgba(148, 163, 184, 0.015));
  transition: border-color 0.2s, box-shadow 0.2s;
}
.gpu-card:hover {
  border-color: color-mix(in srgb, var(--app-accent) 45%, var(--app-line));
  box-shadow: 0 0 0 1px color-mix(in srgb, var(--app-accent) 12%, transparent), 0 4px 18px rgba(0, 0, 0, 0.25);
}
.gpu-head {
  display: flex;
  align-items: center;
  gap: 10px;
  min-width: 0;
}
.gpu-id {
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  font-size: 11px;
  font-weight: 700;
  letter-spacing: 0.4px;
  flex: none;
  padding: 2px 8px;
  border-radius: 6px;
  color: var(--app-cyan);
  background: rgba(34, 211, 238, 0.08);
  border: 1px solid rgba(34, 211, 238, 0.3);
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
  gap: 6px;
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
  font-size: 12.5px;
  font-weight: 600;
}
.gpu-metric :deep(.el-progress-bar__inner) {
  box-shadow: 0 0 8px -1px currentColor;
}
.gpu-body {
  display: flex;
  flex-wrap: wrap;
  gap: 10px 18px;
}
.gpu-metrics {
  flex: 1 1 140px;
  min-width: 120px;
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.gpu-mini {
  flex: 0 1 auto;
  min-width: 108px;
  display: flex;
  flex-direction: column;
  justify-content: center;
  gap: 9px;
  padding-left: 18px;
  border-left: 1px solid var(--app-line);
}
.gpu-mini-stat {
  display: flex;
  align-items: baseline; /* 基线对齐：CJK 与数字字形度量不同，center 会随字体产生上下偏差；共享基线则跨环境一致 */
  gap: 5px;
  font-size: 12px;
}
.gpu-stat-label {
  color: var(--app-muted);
  font-size: 11.5px;
  line-height: 1;
}
.gpu-mini-stat b {
  font-weight: 700;
  font-size: 13px;
  line-height: 1;
  margin-left: auto; /* 数值贴右缘 */
}
.stat-icon {
  width: 14px;
  height: 14px;
  flex: none;
  align-self: center; /* 图标垂直居中，不参与基线 */
}
.fan-icon {
  color: var(--app-cyan);
  animation: fan-rotate 2s linear infinite;
  transform-origin: center;
}
.temp-icon {
  color: var(--app-bad);
}
.power-icon {
  color: var(--app-warn);
}
@keyframes fan-rotate {
  to {
    transform: rotate(360deg);
  }
}
</style>
