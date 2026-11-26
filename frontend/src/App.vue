<template>
  <el-config-provider :locale="locale">
    <div class="bg-aurora"></div>
    <div class="bg-grid"></div>
    <div class="bg-noise"></div>

    <!-- 顶栏：品牌 + 分区导航 + 实时状态 + 工具 -->
    <header class="topbar">
      <div class="topbar-inner">
        <div class="brand">
          <img :src="logoUrl" class="brand-logo" alt="" aria-hidden="true" />
          <div class="brand-text">
            <div class="brand-name">{{ t('appTitle') }} <span class="brand-ver">monitor</span></div>
            <div class="brand-sub">{{ t('brandSub') }}</div>
          </div>
        </div>

        <nav class="topnav">
          <a v-for="s in sections" :key="s.id" :href="'#' + s.id" :class="{ active: activeSection === s.id }" @click="onNavClick(s.id)">{{ s.label }}</a>
        </nav>

        <div class="topbar-tools">
          <button
            class="live-pill"
            :class="{ paused: livePaused }"
            :title="livePaused ? t('pausedHint') : t('liveNow')"
            @click="userPaused = !userPaused"
          >
            <span class="lp-dot"></span>
            <span>{{ livePaused ? t('livePaused') : t('liveNow') }}</span>
            <span v-if="eventAgeMs > 0" class="lp-ms">{{ eventAgeMs }}ms</span>
          </button>
          <button class="icon-btn" :title="t('themeToggle')" @click="toggleTheme">
            {{ currentTheme === 'dark' ? '☀️' : '🌙' }}
          </button>
          <button class="icon-btn" :title="currentLang === 'zh' ? 'English' : '中文'" @click="toggleLang">
            {{ currentLang === 'zh' ? 'EN' : '中' }}
          </button>
        </div>
      </div>
    </header>

    <main class="shell">
      <!-- ① 总览 -->
      <section id="overview" class="section reveal">
        <SectionHead :title="t('navOverview')" :sub="t('overviewSub')">
          <template #meta><span class="sec-pill mono">{{ t('kpiUptime') }} {{ uptimeText }}</span></template>
        </SectionHead>
        <StatsCards :stats="stats" :llm-stats="llmStats" :has-filters="hasFilters" :history="kpiHistory" />
        <TrendChart :paused="livePaused" :filters="overviewFiltersObj" />
      </section>

      <!-- ② 推理调度 -->
      <section id="scheduling" class="section sec-green reveal">
        <SectionHead :title="t('navSched')" :sub="t('schedSub')">
          <template #meta v-if="scheduler && scheduler.enabled && scheduler.mode">
            <span class="mono" :class="scheduler.mode === 'coding' ? 'pill-warn' : 'pill-dim'">{{ scheduler.mode === 'coding' ? t('schedModeCoding') : t('schedModeBackground') }}</span>
          </template>
        </SectionHead>
        <SchedulerStatus :data="scheduler" :inflight="inflight" :requests="items" />
      </section>

      <!-- ③ GPU 状态 -->
      <section id="gpu" class="section sec-blue reveal">
        <SectionHead :title="t('navGpu')">
          <template #sub><span class="sec-time mono" v-if="gpu && gpu.time">{{ fmtClock(gpu.time) }}</span></template>
          <template #meta v-if="gpu && gpu.available">
            <span class="sec-pill mono">{{ t('gpuSample', { sec: (gpu && gpu.sample_interval_sec) || 1 }) }}</span>
          </template>
        </SectionHead>
        <GpuStats :data="gpu" :models="scheduler && scheduler.models" />
      </section>

      <!-- ④ 流量统计 -->
      <section id="providers" class="section sec-violet reveal">
        <SectionHead :title="t('provTitle')" :sub="t('provSub')">
          <template #meta><span class="sec-pill mono">{{ t('provEndpoints', { n: backendStats.length }) }}</span></template>
        </SectionHead>
        <TrafficStats :items="backendStats" :all-items="backendStatsAll" :window-sec="24 * 3600" :stats="stats" @select="onBackendSelect" @select-model="onModelSelect" />
      </section>

      <!-- ⑤ 请求日志 -->
      <section id="logs" class="section sec-amber reveal">
        <SectionHead :title="t('navLogs')" :sub="t('logsSub')">
          <template #meta><span class="sec-pill mono">{{ t('logCount', { n: fmtNum(total) }) }}</span></template>
        </SectionHead>
        <RequestTable
          :items="items"
          :total="total"
          :page="page"
          :page-size="pageSize"
          @page-change="onPageChange"
          @size-change="onPageSizeChange"
          @select="openDetails"
          @select-backend="onBackendSelect"
          @select-client-ip="onClientIPSelect"
          @select-path="onPathSelect"
          @select-model="onModelSelect"
          @toast="(x) => toast(x.msg, x.icon, x.tc)"
        >
          <template #top>
            <FilterPanel
              ref="filterPanel"
              :models="models"
              :backends="backends"
              :paths="paths"
              :client-ips="clientIPs"
              :user-agents="userAgents"
              :quick-counts="quickCounts"
              @apply="onApply"
              @reset="onFilterReset"
            />
          </template>
        </RequestTable>
      </section>
    </main>

    <footer class="foot">
      <span class="mono">{{ t('appTitle') }}</span>
      <span class="sep">·</span>
      <span>{{ t('footerNote') }}</span>
      <span class="sep">·</span>
      <span>{{ t('kpiUptime') }} <span class="mono">{{ uptimeText }}</span></span>
    </footer>

    <RequestDrawer v-model:visible="drawerVisible" :request="selectedRequest" @deleted="onDeleted" />

    <div class="toasts">
      <div v-for="toast in toasts" :key="toast.id" class="toast" :style="{ '--tc': toast.tc }">
        <span class="t-icon">{{ toast.icon }}</span>
        <span class="t-msg">{{ toast.msg }}</span>
        <button v-if="toast.actionLabel" class="t-action" @click="toast.action()">{{ toast.actionLabel }}</button>
      </div>
    </div>
  </el-config-provider>
</template>

<script setup>
import { computed, nextTick, onMounted, onUnmounted, reactive, ref, watch } from 'vue'
import SectionHead from './components/SectionHead.vue'
import StatsCards from './components/StatsCards.vue'
import TrendChart from './components/TrendChart.vue'
import SchedulerStatus from './components/SchedulerStatus.vue'
import GpuStats from './components/GpuStats.vue'
import TrafficStats from './components/TrafficStats.vue'
import FilterPanel from './components/FilterPanel.vue'
import RequestTable from './components/RequestTable.vue'
import RequestDrawer from './components/RequestDrawer.vue'
import { currentLang, elementLocale, setLang, t, toggleLang } from './i18n'
import { currentTheme, toggleTheme } from './theme'
import {
  fetchBackends,
  fetchGpu,
  fetchInflight,
  fetchModels,
  fetchPaths,
  fetchClientIPs,
  fetchUserAgents,
  fetchRequest,
  fetchRequests,
  fetchScheduler,
  fetchStats,
  fetchStatsByBackend,
} from './api'
import { statusBucket, todayRange, fmtNum, loadUI, saveUI } from './utils'
import logoUrl from './assets/logo.svg'

const locale = computed(() => elementLocale())

const total = ref(0)

// 请求日志的筛选/页码/每页条数持久化：刷新后恢复
const _savedLogs = loadUI('logs', null)
const _pf = (_savedLogs && _savedLogs.filters) || {}
const pageSize = ref([50, 100, 200, 500].includes(_savedLogs && _savedLogs.pageSize) ? _savedLogs.pageSize : 100)
const page = ref(Number.isInteger(_savedLogs && _savedLogs.page) && _savedLogs.page >= 1 ? _savedLogs.page : 1)
const [dayStart, dayEnd] = todayRange()
const filters = reactive({
  time_from: _pf.time_from || dayStart.toISOString(),
  time_to: _pf.time_to || dayEnd.toISOString(),
  ...(_pf.path ? { path: _pf.path } : {}),
  ...(_pf.model ? { model: _pf.model } : {}),
  ...(_pf.backend ? { backend: _pf.backend } : {}),
  ...(_pf.status ? { status: _pf.status } : {}),
  ...(_pf.client_ip ? { client_ip: _pf.client_ip } : {}),
  ...(_pf.user_agent ? { user_agent: _pf.user_agent } : {}),
  ...(_pf.stream !== undefined && _pf.stream !== '' ? { stream: _pf.stream } : {}),
})

// 筛选/页码变化即持久化，刷新后恢复
watch([filters, page, pageSize], () => {
  saveUI('logs', { filters: { ...filters }, page: page.value, pageSize: pageSize.value })
}, { deep: true })

// ---- 滚动位置持久化：刷新后精确回到上次停留处（如流量趋势图）----
let scrollSaveTimer = null
function saveScroll() {
  if (scrollSaveTimer) clearTimeout(scrollSaveTimer)
  scrollSaveTimer = setTimeout(() => saveUI('scroll', { y: Math.round(window.scrollY) }), 150)
}
function restoreScroll() {
  const s = loadUI('scroll', null)
  if (s && s.y > 0) window.scrollTo(0, s.y)
}
const stats = reactive({})
const llmStats = reactive({})
const items = ref([])
const backendStats = ref([])
const backendStatsAll = ref([])
const models = ref([])
const backends = ref([])
const paths = ref([])
const clientIPs = ref([])
const userAgents = ref([])
const scheduler = ref(null)
const inflight = ref([])
const gpu = ref(null)
const drawerVisible = ref(false)
const selectedRequest = ref(null)
const filterPanel = ref(null)

// KPI 秒级历史环（最多 240 点 ≈ 4 分钟），供迷你趋势线
const kpiHistory = ref([])

// ---- 分区导航 ----
const sections = computed(() => [
  { id: 'overview', label: t('navOverview') },
  { id: 'scheduling', label: t('navSched') },
  { id: 'gpu', label: t('navGpu') },
  { id: 'providers', label: t('navProv') },
  { id: 'logs', label: t('navLogs') },
])
// 刷新后从 URL hash 恢复当前分区（导航链接是 #id，浏览器会自动滚到对应 section）
const _initHash = (location.hash || '').replace(/^#/, '')
const activeSection = ref(sections.value.some((s) => s.id === _initHash) ? _initHash : 'overview')

// 点导航立即选中（不等有缺陷的中央横带判断），并在滚动动画期间抑制横带覆盖
let navClickTs = 0
function onNavClick(id) {
  activeSection.value = id
  navClickTs = Date.now()
}
// 持久化当前分区：直接打开首页（无 hash）时恢复到上次停留的分区
watch(activeSection, (id) => saveUI('section', id))
let spyObs = null
let revealObs = null

// ---- 实时状态 ----
const liveState = ref('retry')
const userPaused = ref(false)
const hidden = ref(document.hidden)
const lastEventTs = ref(0)
const eventAgeMs = ref(0)
let ageTimer = null
let eventSource = null

const livePaused = computed(() => userPaused.value || hidden.value)

function onVis() {
  hidden.value = document.hidden
}

// ---- Toast ----
const toasts = ref([])
let toastSeq = 0
function toast(msg, icon = '⚠', tc = 'var(--warn)', action = null, actionLabel = '') {
  const id = ++toastSeq
  toasts.value.push({ id, msg, icon, tc, action, actionLabel })
  setTimeout(() => {
    const i = toasts.value.findIndex((x) => x.id === id)
    if (i >= 0) toasts.value.splice(i, 1)
  }, 5000)
}

// ---- 计算属性 ----
const hasFilters = computed(() => Object.keys(filters).length > 0)

const uptimeText = computed(() => fmtUptime(stats.uptime_seconds))

const quickCounts = computed(() => {
  const c = { ok: 0, err4xx: 0, err5xx: 0, stream: 0 }
  for (const it of items.value) {
    const b = statusBucket(it.status_code || 0)
    if (b === 'ok') c.ok++
    else if (b === 'err4xx') c.err4xx++
    else if (b === 'err5xx') c.err5xx++
    if (it.is_streaming) c.stream++
  }
  return c
})

// 总览类面板使用的筛选条件：去掉时间范围（时间范围只影响请求列表）。
// computed 保证引用稳定，TrendChart 不会因 App 重渲染重复拉取。
const overviewFiltersObj = computed(() => {
  const { time_from, time_to, ...rest } = filters
  return rest
})

function fmtUptime(sec) {
  if (!Number.isFinite(sec) || sec < 0) return '-'
  const h = Math.floor(sec / 3600)
  const m = Math.floor((sec % 3600) / 60)
  const s = Math.floor(sec % 60)
  if (h) return `${h}h${m}m`
  if (m) return `${m}m${s}s`
  return `${s}s`
}

function fmtClock(iso) {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return ''
  const p = (n) => String(n).padStart(2, '0')
  return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`
}

// ---- 数据加载 ----
function pushHistory() {
  kpiHistory.value.push({
    ts: Date.now(),
    rpm: stats.requests_per_minute || 0,
    tps: stats.decode_tokens_per_second || 0,
    ttft: stats.avg_first_byte_ms || 0,
    err: (llmStats.error_rate || 0) * 100,
    total: stats.lifetime_total_requests || 0,
  })
  if (kpiHistory.value.length > 240) kpiHistory.value.shift()
}

async function loadStats() {
  try {
    const base = overviewFiltersObj.value
    const [data, llm] = await Promise.all([
      fetchStats(base),
      fetchStats({ ...base, chat_completions_only: 'true' }),
    ])
    Object.keys(stats).forEach((k) => delete stats[k])
    Object.assign(stats, data)
    Object.keys(llmStats).forEach((k) => delete llmStats[k])
    Object.assign(llmStats, llm)
    pushHistory()
  } catch (err) {
    toast(t('refreshFailed', { msg: err.message }), '✗', 'var(--bad)')
  }
}

async function loadRequests() {
  try {
    const data = await fetchRequests(pageSize.value, (page.value - 1) * pageSize.value, { ...filters })
    items.value = data.items || []
    total.value = data.total || 0
  } catch (err) {
    toast(t('refreshFailed', { msg: err.message }), '✗', 'var(--bad)')
  }
}

function onPageChange(p) {
  page.value = p
  loadRequests().catch(() => {})
}

function onPageSizeChange(s) {
  pageSize.value = s
  page.value = 1
  loadRequests().catch(() => {})
}

async function loadBackendStats() {
  try {
    // 行内数据用 24h 窗口；合计行统计全部历史记录（无其他筛选）。
    const [rows, all] = await Promise.all([
      fetchStatsByBackend(overviewFiltersObj.value),
      fetchStatsByBackend({ time_from: '1970-01-01T00:00:00Z' }),
    ])
    backendStats.value = rows
    backendStatsAll.value = all
  } catch {
    /* ignore */
  }
}

async function loadScheduler() {
  try {
    scheduler.value = await fetchScheduler()
  } catch {
    /* ignore */
  }
}

async function loadGpu() {
  try {
    gpu.value = await fetchGpu()
  } catch {
    /* ignore */
  }
}

async function loadInflight() {
  try {
    inflight.value = await fetchInflight()
  } catch {
    /* ignore */
  }
}

async function loadOptions() {
  try {
    models.value = await fetchModels()
  } catch { /* ignore */ }
  try {
    backends.value = await fetchBackends()
  } catch { /* ignore */ }
  try {
    paths.value = await fetchPaths()
  } catch { /* ignore */ }
  try {
    clientIPs.value = await fetchClientIPs()
  } catch { /* ignore */ }
  try {
    userAgents.value = await fetchUserAgents()
  } catch { /* ignore */ }
}

async function refreshAll() {
  await Promise.all([loadStats(), loadRequests(), loadBackendStats(), loadScheduler(), loadInflight()])
}

// ---- 筛选 ----
function onApply(f) {
  Object.keys(filters).forEach((k) => delete filters[k])
  Object.assign(filters, f)
  page.value = 1
  refreshAll().catch(() => {})
}

function onFilterReset() {
  /* FilterPanel 重置时会自行 emit apply（默认时间范围），无需额外处理 */
}

function onBackendSelect(url) {
  if (filterPanel.value) filterPanel.value.setBackend(url)
  filters.backend = url
  page.value = 1
  refreshAll().catch(() => {})
}

function onModelSelect(model) {
  if (filterPanel.value) filterPanel.value.setModel(model)
  filters.model = model
  page.value = 1
  refreshAll().catch(() => {})
}

function onPathSelect(path) {
  if (filterPanel.value) filterPanel.value.setPath?.(path)
  filters.path = path
  page.value = 1
  refreshAll().catch(() => {})
}

function onClientIPSelect(ip) {
  if (filterPanel.value) filterPanel.value.setClientIP(ip)
  filters.client_ip = ip
  page.value = 1
  refreshAll().catch(() => {})
}

async function openDetails(row) {
  try {
    selectedRequest.value = await fetchRequest(row.id)
  } catch {
    selectedRequest.value = row
  }
  drawerVisible.value = true
}

function onDeleted() {
  refreshAll().catch(() => {})
}

// ---- SSE ----
function connectEvents() {
  eventSource = new EventSource(`${API_PROXY}events`)
  eventSource.onopen = () => {
    liveState.value = 'live'
  }
  eventSource.onerror = () => {
    liveState.value = 'retry'
    // 断线期间可能错过 inflight 事件，重连后拉取一次最新在途列表对账。
    loadInflight().catch(() => {})
  }
  const markEvent = () => {
    lastEventTs.value = Date.now()
  }
  eventSource.addEventListener('stats', (e) => {
    const data = JSON.parse(e.data)
    Object.keys(stats).forEach((k) => delete stats[k])
    Object.assign(stats, data)
    markEvent()
    pushHistory()
  })
  eventSource.addEventListener('llmStats', (e) => {
    const data = JSON.parse(e.data)
    Object.keys(llmStats).forEach((k) => delete llmStats[k])
    Object.assign(llmStats, data)
    markEvent()
  })
  eventSource.addEventListener('request', (e) => {
    markEvent()
    loadRequests().catch(() => {})
    loadBackendStats().catch(() => {})
  })
  eventSource.addEventListener('scheduler', (e) => {
    const data = JSON.parse(e.data)
    // kind 事件（如 manual_switch）只是通知，不是完整状态：覆盖会让面板消失，
    // 改为重新拉取完整状态。
    if (data.kind) {
      loadScheduler().catch(() => {})
      return
    }
    scheduler.value = data
    markEvent()
  })
  eventSource.addEventListener('inflight', (e) => {
    const data = JSON.parse(e.data)
    inflight.value = Array.isArray(data.items) ? data.items : []
    markEvent()
  })
  eventSource.addEventListener('gpu', (e) => {
    gpu.value = JSON.parse(e.data)
    markEvent()
  })
}

// ---- 生命周期 ----
onMounted(async () => {
  setLang(currentLang.value)
  connectEvents()
  await refreshAll()
  await loadOptions()
  loadGpu().catch(() => {})

  // 刷新恢复：把持久化的筛选回填到筛选面板（filters 已在初始化时用于首次请求）
  if (filterPanel.value && _pf && Object.keys(_pf).length) {
    filterPanel.value.restore(_pf)
  }

  // 内容渲染完成后，确定要滚到的目标分区：
  // 优先 URL hash；无 hash 则用上次停留的分区（localStorage）。
  // 原生锚点滚动发生在内容（图表/数据/KPI）渲染前、页面高度偏小时会落错位置，
  // 这里在数据加载后重新滚动，scroll-margin-top 会自动避开 sticky 导航。
  const _target =
    (_initHash && sections.value.some((s) => s.id === _initHash)) ||
    (loadUI('section', null) || null)
  if (_target && sections.value.some((s) => s.id === _target)) {
    navClickTs = Date.now() // 抑制 IO 在滚动期间覆盖
    activeSection.value = _target
    if (location.hash !== '#' + _target) history.replaceState(null, '', '#' + _target)
    await nextTick()
    requestAnimationFrame(() => {
      document.getElementById(_target)?.scrollIntoView({ behavior: 'auto', block: 'start' })
    })
  }

  // 滚动高亮导航（同时同步 URL hash，刷新/分享时导航与地址栏都跟手）
  spyObs = new IntersectionObserver(
    (entries) => {
      // 点导航后 700ms 内以点击结果为准，避免中央横带误判覆盖
      if (Date.now() - navClickTs < 700) return
      for (const en of entries) {
        if (en.isIntersecting) {
          activeSection.value = en.target.id
          if (location.hash !== '#' + en.target.id) {
            history.replaceState(null, '', '#' + en.target.id)
          }
        }
      }
    },
    { rootMargin: '-35% 0px -55% 0px' },
  )
  for (const s of sections.value) {
    const el = document.getElementById(s.id)
    if (el) spyObs.observe(el)
  }

  // 分区入场动画
  revealObs = new IntersectionObserver(
    (entries) => {
      for (const en of entries) {
        if (en.isIntersecting) {
          en.target.classList.add('in')
          revealObs.unobserve(en.target)
        }
      }
    },
    { threshold: 0.04 },
  )
  document.querySelectorAll('.reveal').forEach((el, i) => {
    el.style.setProperty('--i', i)
    revealObs.observe(el)
  })

  document.addEventListener('visibilitychange', onVis)
  ageTimer = setInterval(() => {
    eventAgeMs.value = lastEventTs.value ? Date.now() - lastEventTs.value : 0
  }, 500)
})

onUnmounted(() => {
  if (eventSource) eventSource.close()
  if (spyObs) spyObs.disconnect()
  if (revealObs) revealObs.disconnect()
  if (ageTimer) clearInterval(ageTimer)
  window.removeEventListener('scroll', saveScroll)
  document.removeEventListener('visibilitychange', onVis)
})
</script>
