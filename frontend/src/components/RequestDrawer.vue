<template>
  <el-drawer :model-value="visible" size="min(720px, 94vw)" :with-header="false" @update:model-value="v => $emit('update:visible', v)" @closed="reset">
    <div v-if="request" class="drawer-body">
      <div class="head-title">
        <div class="head-main">
          <span :class="'method-badge ' + methodClass(request.method)">{{ request.method || '-' }}</span>
          <span class="head-path mono">{{ request.path || '-' }}</span>
        </div>
        <div class="head-right">
          <span class="head-time mono">{{ fmtDateTime(request.created_at) }}</span>
          <div class="head-actions">
            <el-tooltip :content="t('copyRequest')" placement="top">
              <el-button text :icon="CopyDocument" @click="onCopy" />
            </el-tooltip>
            <el-tooltip :content="t('deleteRequest')" placement="top">
              <el-button text :disabled="!canDelete" class="btn-danger" :icon="Delete" @click="onDelete" />
            </el-tooltip>
          </div>
        </div>
      </div>

      <div class="head-meta">
        <span class="mono">{{ shortenId(request.id) }}</span>
        <span class="dot">•</span>
        <span>{{ request.client_ip || '-' }}</span>
        <span class="arrow">→</span>
        <span class="mono">{{ shortenBackendUrl(request.backend_url) || '-' }}</span>
        <span class="dot">•</span>
        <span>{{ request.model || '-' }}</span>
        <template v-if="request.user_agent">
          <span class="dot">•</span>
          <span class="mono" :title="request.user_agent">{{ request.user_agent }}</span>
        </template>
      </div>

      <div v-if="request.query" class="head-query mono">
        <span class="q-mark">?</span>{{ request.query }}
      </div>
      <div v-if="request.error_text" class="head-error">
        <span class="tone-critical">{{ request.error_text }}</span>
      </div>

      <div class="stat-grid">
        <div class="kpi-card">
          <div class="kpi-label">{{ t('colStatus') }}</div>
          <div class="kpi-value" :class="statusClass(request.status_code || 0)">{{ statusLabel }}</div>
          <div class="kpi-foot">{{ t('colTotal') }} {{ fmtDuration(request.total_ms) }}</div>
        </div>
        <div class="kpi-card">
          <div class="kpi-label">{{ t('colTotalTok') }}</div>
          <div class="kpi-value">{{ fmtNum(request.total_tokens || 0) }}</div>
          <div class="kpi-foot">{{ fmtNum(request.prompt_tokens || 0) }} → {{ fmtNum(request.completion_tokens || 0) }}</div>
        </div>
        <div class="kpi-card">
          <div class="kpi-label">{{ t('metricOutputSec') }}</div>
          <div class="kpi-value">{{ fmtRate(request.decode_tok_per_sec || 0) }} <span class="kpi-unit">tok/s</span></div>
          <div class="kpi-foot">{{ t('metricPromptSpeed') }} {{ fmtRate(request.prompt_tok_per_sec || 0) }} tok/s</div>
        </div>
        <div class="kpi-card">
          <div class="kpi-label">{{ t('metricReqBytes') }}</div>
          <div class="kpi-value kpi-sm">{{ fmtBytes(request.request_bytes || 0) }} <span class="arrow">→</span> {{ fmtBytes(request.response_bytes || 0) }}</div>
          <div class="kpi-foot">{{ t('metricRespBytes') }}</div>
        </div>
        <div class="kpi-card">
          <div class="kpi-label">{{ t('colCache') }}</div>
          <div class="kpi-value">{{ fmtNum(request.cached_prompt_tokens || 0) }} <span class="kpi-sub">/ {{ fmtNum(request.prompt_tokens || 0) }}</span></div>
          <div class="kpi-foot">{{ t('cacheHitRate') }} {{ fmtPctNum(request.cache_hit_pct || 0) }}</div>
        </div>
      </div>

      <div v-if="tl" class="timeline">
        <div class="tl-bar">
          <div class="tl-seg tl-ttft" :class="{ warn: tl.ttftWarn }" :style="{ width: tl.ttftPct + '%' }">
            <span v-if="tl.ttftPct > 14">{{ tl.ttftText }}</span>
          </div>
          <div class="tl-seg tl-gen" :style="{ width: tl.genPct + '%' }">
            <span v-if="tl.genPct > 26">{{ tl.genText }}</span>
          </div>
        </div>
        <div class="tl-legend">
          <span>{{ t('tlArrive') }}</span>
          <span v-if="tl.ttftPct > 0" class="tl-ttft-label">{{ tl.ttftText }}</span>
          <span class="tl-done">{{ t('tlDone') }}</span>
        </div>
      </div>

      <el-tabs v-model="activeTab" class="pill-tabs">
        <el-tab-pane :label="t('tabRawReq')" name="request">
          <div class="code-block">
            <button class="code-copy" :title="t('copyJson')" @click="copyJson(rawReq)">📋</button>
            <pre class="json-view" v-html="highlightJSON(rawReq)"></pre>
          </div>
        </el-tab-pane>
        <el-tab-pane :label="t('tabRawResp')" name="response">
          <template #label>
            <span class="tab-label-text">{{ t('tabRawResp') }}</span>
            <span v-if="respBadge" :class="'tab-badge ' + respBadge"></span>
          </template>
          <div class="code-block">
            <button class="code-copy" :title="t('copyJson')" @click="copyJson(rawResp)">📋</button>
            <pre class="json-view" v-html="highlightJSON(rawResp)"></pre>
          </div>
        </el-tab-pane>
      </el-tabs>
    </div>
  </el-drawer>
</template>

<script setup>
import { ref, computed, watch } from 'vue'
import { CopyDocument, Delete } from '@element-plus/icons-vue'
import { t } from '../i18n'
import { fetchRaw, deleteRequest } from '../api'
import {
  fmtNum, fmtBytes, fmtRate, fmtPctNum, fmtDateTime, fmtDuration,
  isCompleted, statusClass, statusLabel as _statusLabel,
  shortenBackendUrl, shortenId, highlightJSON, methodClass, copyText,
} from '../utils'

const props = defineProps({
  visible: { type: Boolean, default: false },
  request: { type: Object, default: null },
})
const emit = defineEmits(['update:visible', 'deleted'])

const activeTab = ref('request')
const rawReq = ref(t('noSelection'))
const rawResp = ref(t('noSelection'))

const statusLabel = computed(() => {
  const code = Number(props.request?.status_code) || 0
  if (code <= 0) return t('live')
  const label = _statusLabel(code)
  return label ? `${code} ${label}` : String(code)
})
const canDelete = computed(() => isCompleted(props.request))
const respBadge = computed(() => {
  const text = rawResp.value
  if (!text || text === '-' || text === t('noSelection') || text === t('loadingRespPayload')) return ''
  try {
    const parsed = JSON.parse(text)
    if (parsed && typeof parsed === 'object' && parsed.error) return 'err'
  } catch { /* not json */ }
  return 'ok'
})
const tl = computed(() => {
  const r = props.request
  if (!r) return null
  const ttft = Number(r.first_byte_ms) || 0
  const total = Number(r.total_ms) || 0
  if (ttft <= 0 && total <= 0) return null
  const gen = total > ttft ? total - ttft : 0
  const span = Math.max(total, ttft) || 1
  const ttftPct = Math.min(100, Math.max(0, (ttft / span) * 100))
  const genPct = total > 0 ? Math.max(0, (gen / span) * 100) : 0
  const genRate = Number(r.decode_tok_per_sec) || 0
  const ttftText = `TTFT ${fmtDuration(ttft)}`
  const genText = gen > 0
    ? `${fmtNum(Number(r.completion_tokens) || 0)} tok @ ${fmtRate(genRate)} tok/s`
    : ''
  return { ttftPct, genPct, ttftText, genText, ttftWarn: ttft >= 8000 }
})

watch(
  () => [props.visible, props.request],
  async ([visible, req]) => {
    if (visible && req) {
      rawReq.value = t('loadingReqPayload')
      rawResp.value = t('loadingRespPayload')
      const [rr, rp] = await Promise.all([
        fetchRaw(req.id, 'request'),
        fetchRaw(req.id, 'response'),
      ])
      rawReq.value = rr || '-'
      rawResp.value = rp || '-'
    }
  },
  { immediate: true },
)

function reset() {
  activeTab.value = 'request'
  rawReq.value = t('noSelection')
  rawResp.value = t('noSelection')
}

async function onCopy() {
  const method = (props.request.method || 'GET').toUpperCase()
  let url = `${window.location.origin}${props.request.path || ''}`
  if (props.request.query) url += `?${props.request.query}`
  const body = rawReq.value
  const placeholder = t('noSelection')
  const loading = t('loadingReqPayload')
  const hasBody = body && body !== '-' && body !== placeholder && body !== loading
  const esc = (s) => s.replace(/'/g, `'\\''`)
  const lines = [`curl -X ${method} '${esc(url)}'`, `  -H 'Content-Type: application/json'`]
  if (hasBody) {
    lines.push(`  --data-raw '${esc(body)}'`)
  }
  try {
    const ok = await copyText(lines.join(' \\\n'))
    if (ok) ElMessage.success(t('copied'))
    else ElMessage.warning(t('copyFailed'))
  } catch {
    ElMessage.warning(t('copyFailed'))
  }
}

async function copyJson(text) {
  try {
    const ok = await copyText(text)
    if (ok) ElMessage.success(t('copied'))
    else ElMessage.warning(t('copyFailed'))
  } catch {
    ElMessage.warning(t('copyFailed'))
  }
}

async function onDelete() {
  const ok = await ElMessageBox.confirm(
    t('confirmDelete', { id: props.request.id }),
    t('deleteRequest'),
    { type: 'warning', confirmButtonText: t('deleteRequest'), cancelButtonText: t('close') },
  ).catch(() => false)
  if (!ok) return
  await deleteRequest(props.request.id)
  ElMessage.success('OK')
  emit('deleted')
  emit('update:visible', false)
}
</script>

<style scoped>
.drawer-body {
  padding: 8px 4px;
}

.head-title {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  flex-wrap: wrap;
}
.head-main {
  display: flex;
  align-items: center;
  gap: 8px;
  min-width: 0;
}
.head-path {
  font-size: 15px;
  color: var(--app-text);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.head-right {
  display: flex;
  align-items: center;
  gap: 12px;
  flex-shrink: 0;
}
.head-time {
  color: var(--app-muted);
  font-size: 13px;
  white-space: nowrap;
}
.head-actions {
  display: flex;
  gap: 2px;
}
.btn-danger {
  color: #f87171;
}
.btn-danger:hover {
  color: #fc9b93;
  background: rgba(248, 81, 73, 0.1);
}
.btn-danger:disabled {
  color: var(--app-muted);
}

.head-meta {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 7px;
  color: var(--app-muted);
  font-size: 12.5px;
  margin: 8px 0 0;
}
.head-meta .dot {
  opacity: 0.6;
}
.head-meta .arrow {
  color: var(--app-cyan);
  margin: 0 2px;
}

.head-query {
  margin-top: 8px;
  padding: 6px 10px;
  background: var(--app-panel);
  border: 1px solid var(--app-line);
  border-radius: 6px;
  color: var(--app-text);
  font-size: 12px;
  word-break: break-all;
}
.q-mark {
  color: var(--app-cyan);
  margin-right: 6px;
}
.head-error {
  margin-top: 8px;
  padding: 6px 10px;
  background: rgba(248, 81, 73, 0.08);
  border: 1px solid rgba(248, 81, 73, 0.35);
  border-radius: 6px;
  font-size: 12px;
  word-break: break-all;
}

.stat-grid {
  display: grid;
  grid-template-columns: repeat(5, 1fr);
  gap: 8px;
  margin-top: 14px;
}
.kpi-card {
  background: var(--app-panel);
  border: 1px solid var(--app-line);
  border-radius: 8px;
  padding: 10px 12px;
  min-width: 0;
}
.kpi-label {
  color: var(--app-muted);
  font-size: 10px;
  text-transform: uppercase;
  letter-spacing: 0.05em;
}
.kpi-value {
  font-size: 22px;
  font-weight: 700;
  font-variant-numeric: tabular-nums;
  margin-top: 4px;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.kpi-sm {
  font-size: 18px;
}
.kpi-unit {
  font-size: 12px;
  font-weight: 600;
  color: var(--app-muted);
}
.kpi-sub {
  font-size: 13px;
  font-weight: 600;
  color: var(--app-muted);
}
.kpi-foot {
  color: var(--app-muted);
  font-size: 11px;
  margin-top: 4px;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.kpi-value .arrow {
  color: var(--app-cyan);
  margin: 0 3px;
  font-weight: 600;
}

.timeline {
  margin-top: 14px;
  background: var(--app-panel);
  border: 1px solid var(--app-line);
  border-radius: 8px;
  padding: 12px;
}
.tl-bar {
  display: flex;
  height: 18px;
  border-radius: 5px;
  overflow: hidden;
  gap: 2px;
}
.tl-seg {
  display: flex;
  align-items: center;
  justify-content: center;
  color: #0d1117;
  font-size: 11px;
  font-weight: 700;
  white-space: nowrap;
  overflow: hidden;
}
.tl-ttft {
  background: linear-gradient(90deg, rgba(248, 81, 73, 0.55), rgba(210, 153, 34, 0.85));
  color: #0d1117;
}
.tl-ttft.warn {
  background: linear-gradient(90deg, #d29922, #f85149);
}
.tl-gen {
  background: linear-gradient(90deg, #39c5cf, #2ea043);
}
.tl-legend {
  display: flex;
  justify-content: space-between;
  gap: 8px;
  color: var(--app-muted);
  font-size: 11px;
  margin-top: 6px;
}
.tl-legend .tl-ttft-label {
  color: var(--app-warn);
}
.tl-legend .tl-done {
  color: var(--app-ok);
}

.pill-tabs {
  margin-top: 4px;
}
.pill-tabs :deep(.el-tabs__nav-wrap::after) {
  display: none;
}
.pill-tabs :deep(.el-tabs__header) {
  margin-bottom: 12px;
}
.pill-tabs :deep(.el-tabs__nav) {
  position: relative;
}
.pill-tabs :deep(.el-tabs__item) {
  position: relative;
  z-index: 1;
  height: 28px;
  line-height: 28px;
  min-width: 104px;
  text-align: center;
  border-radius: 999px;
  padding: 0 16px;
  margin-right: 6px;
  color: #6b7280;
  font-weight: 400;
  transition: color 0.15s, background 0.15s;
}
.pill-tabs :deep(.el-tabs__item:hover:not(.is-active)) {
  background: rgba(255, 255, 255, 0.05);
  color: #d1d5db;
}
.pill-tabs :deep(.el-tabs__item.is-active) {
  color: #f9fafb;
  font-weight: 500;
}
.pill-tabs :deep(.el-tabs__item:focus-visible) {
  outline: 2px solid #3b82f6;
  outline-offset: 2px;
  border-radius: 999px;
}
.pill-tabs :deep(.el-tabs__active-bar) {
  height: 28px;
  bottom: 0;
  background: #374151;
  border-bottom: none;
  border-radius: 999px;
  z-index: 0;
  transition: transform 0.25s cubic-bezier(0.4, 0, 0.2, 1);
}
.pill-tabs :deep(.el-tab-pane) {
  animation: paneFadeUp 0.2s ease;
}
@keyframes paneFadeUp {
  from {
    opacity: 0;
    transform: translateY(4px);
  }
  to {
    opacity: 1;
    transform: none;
  }
}

.tab-label-text {
  display: inline-block;
}
.tab-badge {
  display: inline-block;
  width: 6px;
  height: 6px;
  border-radius: 50%;
  margin-left: 6px;
  vertical-align: 2px;
}
.tab-badge.err {
  background: #f85149;
}
.tab-badge.ok {
  background: #3fb950;
}

.code-block {
  position: relative;
}
.code-copy {
  position: absolute;
  top: 8px;
  right: 8px;
  z-index: 2;
  border: 1px solid var(--app-line);
  background: var(--app-panel);
  color: var(--app-muted);
  border-radius: 6px;
  width: 28px;
  height: 28px;
  cursor: pointer;
  opacity: 0;
  transition: opacity 0.15s;
}
.code-block:hover .code-copy {
  opacity: 1;
}
.code-copy:hover {
  color: var(--app-cyan);
  border-color: var(--app-cyan);
}
.json-view {
  background: var(--json-bg, #0d1117);
  border: 1px solid var(--app-line);
  border-radius: 6px;
  padding: 12px 12px 12px 0;
  max-height: 380px;
  overflow: auto;
  font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
  font-size: 12px;
  line-height: 1.6;
  white-space: pre;
  margin: 0;
}

/* 浅色主题下 JSON 背景更亮 */
html:not(.dark) .json-view {
  --json-bg: #f8f9fa;
}

/* 深色主题 JSON 高亮（保持原样） */
html.dark .json-view :deep(.jk) { color: #7dd3fc; }
html.dark .json-view :deep(.js) { color: #4ade80; }
html.dark .json-view :deep(.jn) { color: #fb923c; }
html.dark .json-view :deep(.jb) { color: #9da7b1; }

/* 浅色主题 JSON 高亮 */
html:not(.dark) .json-view :deep(.jk) { color: #2563eb; }
html:not(.dark) .json-view :deep(.js) { color: #16a34a; }
html:not(.dark) .json-view :deep(.jn) { color: #d97706; }
html:not(.dark) .json-view :deep(.jb) { color: #6b7280; }

.json-view :deep(.ln) {
  display: inline-block;
  width: 2.6em;
  text-align: right;
  margin-right: 12px;
  color: var(--app-muted);
  user-select: none;
  font-variant-numeric: tabular-nums;
}

@media (max-width: 900px) {
  .stat-grid {
    grid-template-columns: repeat(3, 1fr);
  }
}
@media (max-width: 640px) {
  .stat-grid {
    grid-template-columns: repeat(2, 1fr);
  }
}
</style>
