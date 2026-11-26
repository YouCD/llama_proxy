<template>
  <div class="sched-grid">
    <!-- 左列：模型路由 -->
    <div class="scol">
      <div class="scol-title">
        {{ t('schedModels') }}
        <span class="count">{{ (models || []).length }}</span>
        <span v-if="modeLabel" class="mode-badge" :class="'mode-' + (data && data.mode)">{{ modeLabel }}</span>
      </div>

      <div v-if="data === null" class="scol-list">
        <div class="mrow">
          <span class="mrow-dot loading"></span>
          <span class="mrow-sub">{{ t('schedLoading') }}…</span>
        </div>
      </div>

      <div v-else-if="data.enabled" class="scol-list">
        <div
          v-for="m in models"
          :key="m.id"
          class="mrow"
          :class="{ routed: m.active }"
          :title="rowTitle(m)"
        >
          <span class="mrow-dot" :class="dotState(m)"></span>
          <div class="mrow-main">
            <div class="mrow-name">{{ m.id }}</div>
            <div class="mrow-sub-row">
              <span v-if="m.mode" class="mode-tag" :class="'mode-' + m.mode">{{ m.mode }}</span>
              <span v-if="rowSub(m)" class="mrow-sub">{{ rowSub(m) }}</span>
            </div>
          </div>
          <span v-if="m.gpu" class="dev-badge">{{ m.gpu }}</span>
          <span v-else class="dev-badge cpu">CPU</span>
          <span v-if="m.ready && m.load_ms != null" class="load-badge" :title="t('schedLoadTimeTip')">{{ fmtMs(m.load_ms) }}</span>
          <span v-if="m.active" class="routed-tag">{{ t('schedEnabled') }}</span>
          <button
            v-if="!m.active"
            class="route-btn"
            :disabled="switching"
            :title="t('schedSwitch')"
            @click="onSwitch(m.id)"
          >→</button>
        </div>
        <div v-if="!models.length" class="scol-foot">-</div>
      </div>

      <div v-else class="scol-foot">{{ data.error || t('schedStopped') }}</div>
    </div>

    <!-- 右列：在途请求 -->
    <div class="scol">
      <div class="scol-title">
        <span class="live-dot" :class="live ? 'live' : ''"></span>
        {{ t('inflightTitle') }}
        <span v-if="shown.length" class="count">{{ shown.length }}</span>
      </div>

      <div class="scol-list">
        <div v-if="shown.length" class="radar-wrap">
          <div
            v-for="r in shown"
            :key="r.id"
            class="req-card"
            :class="cardClass(r)"
          >
            <div class="req-line1">
              <span class="lg-model">{{ r.model || '-' }}</span>
              <span class="req-status" :class="'st-' + statusKey(r)">{{ statusLabel(r) }}</span>
            </div>
            <div class="req-line2">
              <span class="kv" v-if="r.status === 'waiting'">{{ t('inflightWait') }} <b>{{ waitSec(r) }}s</b></span>
              <span class="kv" :title="r.user_agent">{{ r.client_ip || '-' }} · {{ shortenBackendUrl(r.backend_url) || '-' }}</span>
              <span class="kv" v-if="r.max_tokens">
                <b>{{ fmtNum(r.completion_tokens || 0) }}</b>/{{ fmtNum(r.max_tokens) }} {{ t('provTok') }}
              </span>
              <span class="kv" v-else-if="r.completion_tokens">
                <b>{{ fmtNum(r.completion_tokens) }}</b> {{ t('provTok') }}
              </span>
              <span class="kv" v-if="r.first_byte_ms">{{ t('colTtft') }} <b>{{ fmtMs(r.first_byte_ms) }}</b></span>
            </div>
            <div v-if="progress(r) !== null" class="req-progress">
              <div class="req-progress-fill" :style="{ width: progress(r) + '%' }"></div>
            </div>
          </div>
        </div>

        <div v-else class="radar-empty">
          <div class="radar">
            <span class="radar-ring"></span>
            <span class="radar-ring"></span>
            <span class="radar-ring"></span>
            <span class="radar-core"></span>
          </div>
          {{ t('inflightEmpty') }}
        </div>
      </div>
    </div>
  </div>
</template>

<script setup>
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { t } from '../i18n'
import { switchSchedulerModel } from '../api'
import { fmtNum, shortenBackendUrl } from '../utils'

const props = defineProps({
  data: { type: Object, default: null },
  inflight: { type: Array, default: () => [] },
  // 最近请求列表：在途请求结束时用于判定成功/失败闪烁
  requests: { type: Array, default: () => [] },
})

const switching = ref(false)
// 已启用（active）排最前，其次 coding 模型，组内保持后端原有顺序
const models = computed(() => {
  const list = props.data && props.data.enabled ? props.data.models || [] : []
  return [...list].sort(
    (a, b) =>
      Number(b.active) - Number(a.active) ||
      Number(b.mode === 'coding') - Number(a.mode === 'coding'),
  )
})
const modeLabel = computed(() => {
  if (!props.data) return ''
  if (props.data.mode === 'coding') return t('schedModeCoding')
  if (props.data.mode === 'background') return t('schedModeBackground')
  return ''
})

const live = computed(() => props.inflight.length > 0)

// 每秒跳动：驱动“加载中 Ns”
const nowTs = ref(Date.now())
let tickTimer = null
onMounted(() => {
  tickTimer = setInterval(() => (nowTs.value = Date.now()), 1000)
})
onUnmounted(() => {
  if (tickTimer) clearInterval(tickTimer)
})

// 已消失的在途请求：保留 450ms 做收起动画
const removed = ref([]) // {id, ok, ts}
let prevIds = new Set()
watch(
  () => props.inflight,
  (list) => {
    const cur = new Set((list || []).map((r) => r.id))
    const gone = [...prevIds].filter((id) => !cur.has(id))
    for (const id of gone) {
      const rec = (props.requests || []).find((r) => r.id === id)
      removed.value.push({ id, ok: !rec || (rec.status_code || 0) < 400, ts: Date.now() })
    }
    removed.value = removed.value.filter((r) => Date.now() - r.ts < 450)
    prevIds = cur
  },
)

const shown = computed(() => {
  const cur = props.inflight.map((r) => ({ ...r, _removed: false }))
  for (const r of removed.value) {
    cur.push({ id: r.id, _removed: true, _ok: r.ok })
  }
  return cur
})

function statusKey(r) {
  if (r._removed) return r._ok ? 'ok' : 'bad'
  if (r.status === 'streaming') return 'stream'
  if (r.status === 'waiting') return 'wait'
  if (r.streaming) return 'run'
  return 'run'
}

function statusLabel(r) {
  if (r._removed) return r._ok ? '200 ✓' : '✗'
  if (r.status === 'streaming') return t('stStreaming')
  if (r.status === 'waiting') return t('stWaiting')
  return t('stRunning')
}

// 等待首 token 的已用时长（nowTs 每秒跳动驱动刷新）
function waitSec(r) {
  if (!r.started_at) return 0
  const start = new Date(r.started_at).getTime()
  if (Number.isNaN(start)) return 0
  return Math.max(0, Math.floor((nowTs.value - start) / 1000))
}

function progress(r) {
  if (r._removed) return null
  if (r.max_tokens && r.max_tokens > 0) {
    const tokens = r.completion_tokens || 0
    if (tokens <= 0) return 0
    // 平方根比例：max_tokens 是模型上限而非预期长度，线性时几百 token 只有 ~1%；
    // 平方根让低进度也可见增长，达到上限时为 100%。
    return Math.min(100, Math.round(100 * Math.sqrt(tokens / r.max_tokens)))
  }
  return null
}

function cardClass(r) {
  if (r._removed) return ['removing', r._ok ? 'done-ok' : 'done-bad']
  return ''
}

function dotState(m) {
  if (m.error) return 'bad'
  if (m.ready || m.active) return 'ok'
  if (m.running) return 'loading'
  return ''
}

function rowTitle(m) {
  if (m.error) return t('schedError')
  if (m.fail_count > 0) {
    let s = `${t('schedFail')} #${m.fail_count}`
    if (m.next_retry) s += ` · ${m.next_retry}`
    return s
  }
  if (m.sleeping) return t('schedSleeping')
  if (m.ready) return t('schedReady')
  if (m.running) return t('schedLoading')
  return t('schedStopped')
}

function rowSub(m) {
  const parts = []
  if (m.pid) parts.push(`${t('schedPid')} ${m.pid}`)
  if (m.running && !m.ready && !m.sleeping && !m.fail_count && m.load_started_at) {
    const start = new Date(m.load_started_at).getTime()
    if (!Number.isNaN(start)) parts.push(t('schedLoadElapsed', { n: Math.max(0, Math.floor((nowTs.value - start) / 1000)) }))
  }
  return parts.join(' · ')
}

function fmtMs(ms) {
  if (!Number.isFinite(ms) || ms <= 0) return '-'
  const s = ms / 1000
  if (s < 60) return s.toFixed(1) + 's'
  return Math.floor(s / 60) + 'm ' + Math.round(s % 60) + 's'
}

async function onSwitch(modelId) {
  switching.value = true
  try {
    const res = await switchSchedulerModel(modelId)
    if (res.status === 'skipped') {
      ElMessage.warning(t('schedSwitchSkipped'))
    } else {
      ElMessage.info(t('schedSwitching', { model: modelId }))
    }
  } catch (e) {
    ElMessage.error(t('schedSwitchFailed', { msg: e.message || e }))
  } finally {
    switching.value = false
  }
}
</script>

<style scoped>
.mode-badge {
  margin-left: auto;
  font-size: 11px;
  font-weight: 700;
  letter-spacing: 0.02em;
  padding: 2px 10px;
  border-radius: 999px;
  border: 1px solid;
}

.mode-badge.mode-coding {
  color: #0bb47d;
  border-color: color-mix(in srgb, #0bb47d 55%, transparent);
  background: color-mix(in srgb, #0bb47d 16%, transparent);
}

.mode-badge.mode-background {
  color: #fbbf24;
  border-color: color-mix(in srgb, #fbbf24 55%, transparent);
  background: color-mix(in srgb, #fbbf24 16%, transparent);
}

.mrow-dot.loading {
  background: var(--warn);
  animation: breathe 2s ease-in-out infinite;
}

/* 下拉列表排版：模型名左对齐占满，模式标签右对齐等宽，当前模型整行高亮 */
.mi-item {
  display: flex;
  align-items: center;
  gap: 10px;
}
.mi-id {
  flex: 1;
  min-width: 0;
  font-size: 13px;
  font-weight: 600;
  /* 长模型名（带 GPU 徽标时）截断省略，保证右侧 mode 标签不被推出 300px 弹层 */
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.mi-gpu {
  flex: none;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  line-height: 1; /* 关键：消除行高带来的上下不居中 */

  font-size: 11px;
  color: #6ab0f0;
  background: rgba(106, 176, 240, 0.1);
  border: 1px solid rgba(106, 176, 240, 0.35);

  /* 视觉微调：左 padding 6px，右 padding 4px（抵消数字1的右侧空白） */
  padding: 3px 4px 3px 6px;

  border-radius: 8px;
  white-space: nowrap;
  box-sizing: border-box;
}
.mi-mode {
  flex: none;
  /* "background" / "coding" 都短于 84px，min-width 即统一宽度，右边缘自然对齐 */
  min-width: 84px;
  /* 与左侧 GPU 徽标拉开间距（10px gap + 6px） */
  margin-left: 6px;
  text-align: center;
  font-size: 11px;
  padding: 2px 0;
  border-radius: 4px;
  box-sizing: border-box;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.mi-coding { color: var(--app-warn); background: rgba(200, 160, 90, 0.12); }
.mi-bg { color: var(--app-muted); background: rgba(140, 160, 190, 0.12); }
.mi-cur-chip {
  color: #4ade80;
  background: rgba(34, 197, 94, 0.12);
  font-weight: 600;
}

/* —— 当前请求 —— */
.inflight-section { margin-top: 16px; }
.inflight-heading {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 10px;
  font-size: 14px;
  font-weight: 700;
  color: var(--app-text);
}

.live-dot {
  display: inline-block;
  width: 8px;
  height: 8px;
  border-radius: 50%;
  background: var(--faint);
}

.live-dot.live {
  background: var(--ok);
  box-shadow: 0 0 7px rgba(52, 211, 153, 0.7);
}
.inflight-empty {
  font-size: 12px;
  color: var(--app-muted);
  font-style: italic;
  height: 200px;
  display: grid;
  place-items: center;   /* 水平 + 垂直居中 */
  text-align: center;    /* 多行文本时行内内容也居中 */
  box-sizing: border-box;
  padding: 0 12px;       /* 防止多行文本贴边 */
}

/* 表格区：短列表自然高度，长列表由 el-table 的 max-height 表体内部滚动（表头固定） */
.table-box {
  min-height: 60px;
}

.ip-dot {
  display: inline-block;
  width: 7px;
  height: 7px;
  border-radius: 50%;
  background: #22c55e;
  margin-right: 8px;
  vertical-align: middle;
}
.ip-val {
  font-weight: 700;
  font-size: 13px;
  color: var(--app-text);
  vertical-align: middle;
}

.ua-plain {
  font-size: 12.5px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  display: inline-block;
  max-width: 100%;
  vertical-align: middle;
}
.model-plain {
  font-size: 12.5px;
  font-weight: 600;
  display: inline-block;
  max-width: 100%;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  vertical-align: middle;
}

.cell-subtle { color: var(--app-muted); }
</style>

<style>
/* 下拉菜单 teleport 到 body，scoped 样式覆盖不到 el-dropdown-item，需非 scoped 规则 */
.el-dropdown-menu .mi-current {
  background: rgba(34, 197, 94, 0.07);
  cursor: default;
  /* 当前行虽 disabled，但不能被压暗 */
  opacity: 1 !important;
  color: var(--el-text-color-primary) !important;
}
.el-dropdown-menu .mi-current .mi-id {
  color: #4ade80 !important;
}
/* 菜单固定 300px 宽；模型过多时限高内部滚动；窄屏（手机）限宽到视口内 */
.mi-popper .el-dropdown-menu {
  width: 300px;
  max-width: calc(100vw - 24px);
  max-height: 300px;
  overflow-y: auto;
}
</style>
