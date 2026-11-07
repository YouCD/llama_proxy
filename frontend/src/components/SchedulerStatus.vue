<template>
  <!-- data 尚未加载（null）：渲染等高的占位骨架，避免面板闪现消失、页面跳动 -->
  <div v-if="data === null" class="panel sched-panel-fixed">
    <div class="section-title">{{ t('schedulerTitle') }}</div>
    <div class="sched-card sched-card-ph">
      <span class="dot-big loading"></span>
      <span class="ph-text">{{ t('schedLoading') }}…</span>
    </div>
    <div class="inflight-section">
      <div class="inflight-heading">
        <span class="live-dot"></span>
        {{ t('inflightTitle') }}
      </div>
      <div class="table-box"><div class="inflight-empty">{{ t('schedLoading') }}…</div></div>
    </div>
  </div>

  <!-- 调度器已启用 -->
  <div v-else-if="data.enabled" class="panel sched-panel-fixed">
    <div class="section-title">{{ t('schedulerTitle') }}</div>

    <!-- 状态卡片：上行 = 标题（左）+ 切换按钮（右）恒同一行；下行 = 常驻进程 chips，
         溢出自行换行，避免旧版「标题 / chips / 按钮」三行折行留白 -->
    <div class="sched-card">
      <div class="sched-top">
        <div class="sched-id">
          <span class="dot-big" :class="stateDot"></span>
          <span class="sched-mode">{{ modeLabel }}</span>
          <span class="sched-badge" :class="stateDot">{{ badgeLabel }}</span>
        </div>

        <div class="sched-right">
          <el-dropdown v-if="switchable" trigger="click" popper-class="mi-popper" @command="onSwitch">
            <el-button size="small" class="switch-btn" :loading="switching">
              {{ t('schedSwitch') }} <el-icon><ArrowDown /></el-icon>
            </el-button>
            <template #dropdown>
              <el-dropdown-menu>
                <el-dropdown-item
                  v-for="m in data.models"
                  :key="m.id"
                  :command="m.id"
                  :disabled="m.active"
                  class="mi-item"
                  :class="{ 'mi-current': m.active }"
                >
                  <span class="mi-id mono" :title="m.id">{{ m.id }}</span>
                  <span v-if="m.active" class="mi-mode mi-cur-chip">
                    {{ m.mode }} · {{ t('schedCurrent') }}
                  </span>
                  <span v-else class="mi-mode" :class="m.mode === 'coding' ? 'mi-coding' : 'mi-bg'">
                    {{ m.mode }}
                  </span>
                </el-dropdown-item>
              </el-dropdown-menu>
            </template>
          </el-dropdown>
        </div>
      </div>

      <!-- 常驻进程 chips：只列正在运行/已就绪的模型（background 组态下为常驻组成员，
           coding 态下仅当前 coding 模型），完整模型列表由上方「切换模型」下拉承担。
           “当前”只标在 coding 模型上——background 常驻态所有节点同时在线，不存在唯一“当前” -->
      <div class="sched-models">
        <div
          v-for="m in residentModels"
          :key="m.id"
          class="model-chip"
          :class="'mc-' + mcState(m)"
          :title="mcTitle(m)"
        >
          <span class="mc-dot"></span>
          <span class="mono mc-id" :title="m.id">{{ m.id }}</span>
          <span v-if="m.pid" class="mono mc-pid">{{ m.pid }}</span>
          <span v-if="m.running && !m.ready && !m.sleeping && !m.fail_count && m.load_started_at" class="mono mc-load">{{ t('schedLoadElapsed', { n: elapsed(m) }) }}</span>
          <span v-else-if="m.ready && m.load_ms != null" class="mono mc-load ok" :title="t('schedLoadTimeTip')">{{ t('schedLoadDone', { n: fmtMs(m.load_ms) }) }}</span>
          <span v-if="m.mode === 'coding' && m.active" class="mc-cur">{{ t('schedCurrent') }}</span>
        </div>
        <span v-if="!residentModels.length" class="cell-subtle">-</span>
      </div>
    </div>

    <!-- 当前请求 -->
    <div class="inflight-section">
      <div class="inflight-heading">
        <span class="live-dot" :class="inflight.length ? 'live' : ''"></span>
        {{ t('inflightTitle') }}
        <!-- “N 个进行中”跟随它描述的请求列表，不再孤立在卡片右下角 -->
        <span v-if="inflight.length" class="inflight-pill">
          {{ inflight.length }} {{ t('inflightPill') }}
        </span>
      </div>

      <div class="table-box">
        <el-table
          v-if="inflight.length"
          :data="inflight"
          size="small"
          row-key="id"
          :max-height="160"
          class="inflight-table"
        >
          <el-table-column min-width="150">
            <template #header>{{ t('colClient') }}</template>
            <template #default="{ row }">
              <span class="ip-dot"></span>
              <span class="mono ip-val">{{ row.client_ip || '-' }}</span>
            </template>
          </el-table-column>

          <el-table-column min-width="260">
            <template #header>User-Agent</template>
            <template #default="{ row }">
              <span v-if="row.user_agent" class="mono ua-plain"
                :title="row.user_agent">{{ row.user_agent }}</span>
              <span v-else class="cell-subtle">-</span>
            </template>
          </el-table-column>

          <el-table-column min-width="150">
            <template #header>{{ t('colModel') }}</template>
            <template #default="{ row }">
              <span class="model-plain">{{ row.model || '-' }}</span>
            </template>
          </el-table-column>
        </el-table>
        <div v-else class="inflight-empty">{{ t('inflightEmpty') }}</div>
      </div>
    </div>
  </div>
</template>

<script setup>
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { ArrowDown } from '@element-plus/icons-vue'
import { ElMessage } from 'element-plus'
import { t } from '../i18n'
import { switchSchedulerModel } from '../api'

const props = defineProps({
  data: { type: Object, default: null },
  inflight: { type: Array, default: () => [] },
})

const switching = ref(false)

// 每秒跳动的时钟：驱动模板中“加载中 Ns”实时刷新（后端仅在状态变化时推事件，
// 加载期间的秒级计时必须由前端自行驱动）。
const nowTs = ref(Date.now())
let tickTimer = null
onMounted(() => {
  tickTimer = setInterval(() => (nowTs.value = Date.now()), 1000)
})
onUnmounted(() => {
  if (tickTimer) clearInterval(tickTimer)
})

// 进行中的加载已持续秒数（load_started_at 为 RFC3339 UTC）。
function elapsed(m) {
  const start = new Date(m.load_started_at).getTime()
  if (Number.isNaN(start)) return 0
  return Math.max(0, Math.floor((nowTs.value - start) / 1000))
}

// 毫秒时长人读格式：<60s 保留一位小数，否则分秒。
function fmtMs(ms) {
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

// 存在至少一个非当前的模型时才显示切换按钮（models 字段仅新版后端返回）。
const switchable = computed(() =>
  (props.data?.models || []).some((m) => !m.active)
)

// 左侧 chips 只展示常驻进程（运行中或已就绪）：coding 态下仅当前 coding 模型，
// 避免与右侧「切换模型」下拉重复罗列全部模型。
const residentModels = computed(() =>
  (props.data?.models || []).filter((m) => m.running || m.active)
)

const modeLabel = computed(() => {
  if (!props.data) return ''
  if (props.data.mode === 'coding') return t('schedModeCoding')
  if (props.data.mode === 'background') return t('schedModeBackground')
  return '-'
})

function mcState(m) {
  if (m.error) return 'err'
  if (m.fail_count > 0) return 'fail'
  if (m.sleeping) return 'sleeping'
  if (m.ready) return 'ok'
  return m.running ? 'loading' : 'stopped'
}

function mcTitle(m) {
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

const stateDot = computed(() => {
  if (props.data?.error) return 'err'
  if (props.data?.ready) return 'ok'
  return 'loading'
})

const badgeLabel = computed(() => {
  if (props.data?.error) return t('schedError')
  if (props.data?.ready) return t('schedEnabled')
  return t('schedLoading')
})

</script>

<style scoped>
/* 面板自适应高度：未加载时渲染同构占位（不消失），加载后按内容自然收缩，
   列表超高才内部滚动，不留大片空白 */

/* —— 状态卡片 —— */
.sched-card {
  display: flex;
  flex-direction: column;
  gap: 10px;
  margin-top: 10px;
  padding: 14px 18px;
  border: 1px solid var(--app-line);
  border-radius: 10px;
  background: rgba(255, 255, 255, 0.015);
}
/* 加载占位保持单行居中，不被卡片纵向布局叠行 */
.sched-card-ph {
  flex-direction: row;
  align-items: center;
  justify-content: center;
}
.ph-text {
  font-size: 13px;
  color: var(--app-muted);
}

.dot-big {
  display: inline-block;
  width: 14px;
  height: 14px;
  border-radius: 50%;
  flex: none;
  background: var(--app-muted);
}
.dot-big.ok {
  background: #22c55e;
  box-shadow: 0 0 10px rgba(34, 197, 94, 0.8);
}
.dot-big.loading {
  background: var(--app-warn);
  box-shadow: 0 0 8px rgba(245, 158, 11, 0.5);
}
.dot-big.err {
  background: var(--app-bad);
  box-shadow: 0 0 8px rgba(220, 80, 80, 0.6);
}

/* 上行：标题（左）+ 切换按钮（右）恒同一行，不再折行 */
.sched-top {
  display: flex;
  align-items: center;
  gap: 12px;
  min-width: 0;
}
.sched-id {
  display: flex;
  align-items: center;
  gap: 8px;
  min-width: 0;
  flex: none;
}
.sched-mode {
  font-weight: 700;
  font-size: 15px;
  color: var(--app-text);
}
.sched-badge {
  font-size: 11px;
  font-weight: 600;
  padding: 1px 8px;
  border-radius: 9px;
  border: 1px solid;
}
.sched-badge.ok {
  color: #4ade80;
  border-color: rgba(34, 197, 94, 0.5);
  background: rgba(34, 197, 94, 0.1);
}
.sched-badge.loading {
  color: var(--app-warn);
  border-color: rgba(245, 158, 11, 0.5);
  background: rgba(245, 158, 11, 0.1);
}
.sched-badge.err {
  color: var(--app-bad);
  border-color: rgba(220, 80, 80, 0.5);
  background: rgba(220, 80, 80, 0.1);
}
/* —— 模型成员 chips —— */
.sched-models {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}
.model-chip {
  display: inline-flex;
  align-items: center;
  gap: 7px;
  padding: 4px 10px;
  border: 1px solid var(--app-line);
  border-radius: 14px;
  background: rgba(255, 255, 255, 0.015);
}
.mc-id {
  font-size: 12.5px;
  font-weight: 600;
  color: var(--app-text);
  /* 自适应宽度：短名不拉齐、长名到上限才省略，悬停（title）看全名 */
  display: inline-block;
  min-width: 0;
  max-width: 160px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  vertical-align: middle;
}

@media (max-width: 767px) {
  /* 移动端：收窄上限，保证 chip 行内放下多个模型 */
  .mc-id {
    max-width: 110px;
  }
}
.mc-pid {
  font-size: 11px;
  color: var(--app-muted);
}
.mc-load {
  font-size: 11px;
  color: var(--app-warn);
  background: rgba(217, 119, 6, 0.12);
  padding: 1px 6px;
  border-radius: 8px;
}
.mc-load.ok {
  color: var(--app-ok);
  background: rgba(22, 163, 74, 0.12);
}
.mc-cur {
  font-size: 10.5px;
  font-weight: 600;
  color: #4ade80;
  background: rgba(34, 197, 94, 0.12);
  padding: 1px 7px;
  border-radius: 8px;
}
.mc-dot {
  width: 8px;
  height: 8px;
  border-radius: 50%;
  flex: none;
  background: var(--app-muted);
}
.model-chip.mc-ok .mc-dot {
  background: #22c55e;
  box-shadow: 0 0 7px rgba(34, 197, 94, 0.7);
}
.model-chip.mc-loading {
  border-color: rgba(245, 158, 11, 0.55);
}
.model-chip.mc-loading .mc-dot {
  background: var(--app-warn);
  box-shadow: 0 0 6px rgba(245, 158, 11, 0.5);
}
.model-chip.mc-loading .mc-id {
  color: var(--app-warn);
}
.model-chip.mc-err {
  border-color: rgba(220, 80, 80, 0.5);
}
.model-chip.mc-err .mc-dot {
  background: var(--app-bad);
}
.model-chip.mc-fail {
  border-color: rgba(245, 80, 120, 0.55);
}
.model-chip.mc-fail .mc-dot {
  background: var(--app-bad);
  box-shadow: 0 0 7px rgba(245, 80, 120, 0.7);
  animation: mc-fail-pulse 1.2s ease-in-out infinite alternate;
}
.model-chip.mc-fail .mc-id {
  color: var(--app-bad);
}
@keyframes mc-fail-pulse {
  from { opacity: 1; }
  to { opacity: 0.45; }
}
.model-chip.mc-sleeping {
  border-color: rgba(147, 112, 219, 0.5);
}
.model-chip.mc-sleeping .mc-dot {
  background: #9370db;
  box-shadow: 0 0 6px rgba(147, 112, 219, 0.55);
}
.model-chip.mc-sleeping .mc-id {
  color: #b09be0;
}

.sched-right {
  margin-left: auto;
  display: flex;
  align-items: center;
  gap: 10px;
}
.inflight-pill {
  font-size: 12px;
  font-weight: 600;
  color: #6ab0f0;
  padding: 3px 12px;
  border-radius: 12px;
  border: 1px solid rgba(106, 176, 240, 0.45);
  background: rgba(106, 176, 240, 0.08);
  white-space: nowrap;
}

/* 下拉列表排版：模型名左对齐占满，模式标签右对齐等宽，当前模型整行高亮 */
.mi-item {
  display: flex;
  align-items: center;
  gap: 10px;
}
.mi-id {
  flex: 1;
  font-size: 13px;
  font-weight: 600;
}
.mi-mode {
  flex: none;
  min-width: 84px;
  text-align: center;
  font-size: 11px;
  padding: 2px 0;
  border-radius: 4px;
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
  background: var(--app-muted);
  flex: none;
}
.live-dot.live {
  background: #22c55e;
  box-shadow: 0 0 7px rgba(34, 197, 94, 0.7);
}
.inflight-empty {
  font-size: 12px;
  color: var(--app-muted);
  font-style: italic;
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
