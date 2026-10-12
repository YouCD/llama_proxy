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

    <!-- 状态卡片 -->
    <div class="sched-card">
      <div class="sched-id">
        <span class="dot-big" :class="stateDot"></span>
        <div class="sched-id-text">
          <div class="sched-mode-line">
            <span class="sched-mode">{{ modeLabel }}</span>
            <span class="sched-badge" :class="stateDot">{{ badgeLabel }}</span>
          </div>
          <div class="sched-sub">
            <span class="mono">{{ t('schedModelLabel') }}: {{ data.model || '-' }}</span>
          </div>
        </div>
      </div>

      <div class="vsep"></div>

      <div class="sched-stat">
        <div class="stat-txt">
          <div class="stat-label">PID</div>
          <div class="stat-val mono">{{ data.pid || '-' }}</div>
        </div>
      </div>

      <div class="vsep"></div>

      <div class="sched-stat">
        <div class="stat-txt">
          <div class="stat-label">{{ t('schedAddr') }}</div>
          <div class="stat-val mono url">{{ data.active_base_url || '-' }}</div>
        </div>
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
        <span v-if="inflight.length" class="inflight-pill">
          {{ inflight.length }} {{ t('inflightPill') }}
        </span>
      </div>
    </div>

    <!-- 当前请求 -->
    <div class="inflight-section">
      <div class="inflight-heading">
        <span class="live-dot" :class="inflight.length ? 'live' : ''"></span>
        {{ t('inflightTitle') }}
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
import { computed, ref } from 'vue'
import { ArrowDown } from '@element-plus/icons-vue'
import { ElMessage } from 'element-plus'
import { t } from '../i18n'
import { switchSchedulerModel } from '../api'

const props = defineProps({
  data: { type: Object, default: null },
  inflight: { type: Array, default: () => [] },
})

const switching = ref(false)

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

const modeLabel = computed(() => {
  if (!props.data) return ''
  if (props.data.mode === 'coding') return t('schedModeCoding')
  if (props.data.mode === 'background') return t('schedModeBackground')
  return '-'
})

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
  align-items: center;
  gap: 18px;
  margin-top: 10px;
  padding: 14px 18px;
  border: 1px solid var(--color-rule);
  border-radius: 10px;
  background: rgba(255, 255, 255, 0.015);
  flex-wrap: wrap;
}
.sched-card-ph {
  align-items: center;
}
.ph-text {
  font-size: 13px;
  color: var(--color-faint);
}

.dot-big {
  display: inline-block;
  width: 14px;
  height: 14px;
  border-radius: 50%;
  flex: none;
  background: var(--color-faint);
}
.dot-big.ok {
  background: #22c55e;
  box-shadow: 0 0 10px rgba(34, 197, 94, 0.8);
}
.dot-big.loading {
  background: var(--color-amber);
  box-shadow: 0 0 8px rgba(245, 158, 11, 0.5);
}
.dot-big.err {
  background: var(--color-crimson);
  box-shadow: 0 0 8px rgba(220, 80, 80, 0.6);
}

.sched-id {
  display: flex;
  align-items: center;
  gap: 12px;
  min-width: 0;
}
.sched-mode-line {
  display: flex;
  align-items: center;
  gap: 8px;
}
.sched-mode {
  font-weight: 700;
  font-size: 15px;
  color: var(--color-text);
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
  color: var(--color-amber);
  border-color: rgba(245, 158, 11, 0.5);
  background: rgba(245, 158, 11, 0.1);
}
.sched-badge.err {
  color: var(--color-crimson);
  border-color: rgba(220, 80, 80, 0.5);
  background: rgba(220, 80, 80, 0.1);
}
.sched-sub {
  margin-top: 4px;
  font-size: 12px;
  color: var(--color-dim);
  display: flex;
  gap: 6px;
  align-items: center;
}
.dot-sep { color: var(--color-faint); }

.vsep {
  width: 1px;
  align-self: stretch;
  background: var(--color-rule);
  flex: none;
}

.sched-stat {
  display: flex;
  align-items: center;
}
.stat-label {
  font-size: 11px;
  color: var(--color-faint);
  letter-spacing: 0.4px;
}
.stat-val {
  font-size: 14px;
  font-weight: 700;
  color: var(--color-text);
  margin-top: 2px;
}
.stat-val.url {
  color: #5ba3e0;
  font-weight: 500;
  font-size: 13px;
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
.mi-coding { color: var(--color-gold); background: rgba(200, 160, 90, 0.12); }
.mi-bg { color: var(--color-steel); background: rgba(140, 160, 190, 0.12); }
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
  color: var(--color-text);
}
.live-dot {
  display: inline-block;
  width: 8px;
  height: 8px;
  border-radius: 50%;
  background: var(--color-faint);
  flex: none;
}
.live-dot.live {
  background: #22c55e;
  box-shadow: 0 0 7px rgba(34, 197, 94, 0.7);
}
.inflight-empty {
  font-size: 12px;
  color: var(--color-faint);
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
  color: var(--color-text);
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

.cell-subtle { color: var(--color-faint); }
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
/* 菜单固定 300px 宽；模型过多时限高内部滚动 */
.mi-popper .el-dropdown-menu {
  width: 300px;
  max-height: 300px;
  overflow-y: auto;
}
</style>
