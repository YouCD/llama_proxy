<template>
  <el-config-provider :locale="locale">
    <div class="app-shell">
      <header class="app-header">
        <div class="header-left">
          <img class="header-logo" src="/favicon.svg" alt="logo"/>
          <div>
            <h1 class="title">{{ t('appTitle') }}</h1>
            <p class="subtitle">{{ t('heroText') }}</p>
          </div>
        </div>
        <div class="header-actions">
          <span class="lang-btn" :title="t('themeToggle')" @click="toggleTheme">
            <span class="lang-icon">{{ currentTheme === 'dark' ? '☀️' : '🌙' }}</span>
          </span>
          <el-dropdown trigger="hover" @command="onLangCommand">
            <span class="lang-btn">
              <span class="lang-icon">🌐</span>
              <span>{{ currentLang === 'zh' ? '中文' : 'English' }}</span>
            </span>
            <template #dropdown>
              <el-dropdown-menu>
                <el-dropdown-item command="zh" :disabled="currentLang === 'zh'">中文</el-dropdown-item>
                <el-dropdown-item command="en" :disabled="currentLang === 'en'">English</el-dropdown-item>
              </el-dropdown-menu>
            </template>
          </el-dropdown>
        </div>
      </header>

      <StatsCards :stats="stats" :llm-stats="llmStats" :has-filters="hasFilters" :output-sec="outputSec"
                  :window-label="timeWindow.label"/>

      <SchedulerStatus :data="scheduler"/>

      <BackendStats :items="backendStats" :window-sec="timeWindow.secs" @select="onBackendSelect"/>

      <div class="panel">
        <section class="daily-section">
          <h2 class="daily-heading">{{ t('dailyTitle') }}</h2>
          <DailyChart :items="dailyStats"/>
        </section>
      </div>

      <FilterPanel ref="filterPanel" :models="models" :backends="backends" :quick-counts="quickCounts"
                   @apply="onApply"/>

      <RequestTable
          :items="items"
          :has-more="hasMore"
          :loading-more="loadingMore"
          :page-size="pageSize"
          @load-more="loadMore"
          @select="openDetails"
          @select-backend="onBackendSelect"
          @select-client-ip="onClientIPSelect"
          @select-user-agent="onUserAgentSelect"
      />

      <RequestDrawer
          v-model:visible="drawerVisible"
          :request="selectedRequest"
          @deleted="onDeleted"
      />
    </div>
  </el-config-provider>
</template>

<script setup>
import {computed, onMounted, onUnmounted, reactive, ref} from 'vue'
import {Refresh} from '@element-plus/icons-vue'
import StatsCards from './components/StatsCards.vue'
import SchedulerStatus from './components/SchedulerStatus.vue'
import BackendStats from './components/BackendStats.vue'
import DailyChart from './components/DailyChart.vue'
import FilterPanel from './components/FilterPanel.vue'
import RequestTable from './components/RequestTable.vue'
import RequestDrawer from './components/RequestDrawer.vue'
import {currentLang, elementLocale, setLang, t} from './i18n'
import {currentTheme, toggleTheme} from './theme'
import {
  fetchBackends,
  fetchDailyStats,
  fetchModels,
  fetchRequest,
  fetchRequests,
  fetchScheduler,
  fetchStats,
  fetchStatsByBackend,
} from './api'
import {fmtDate, statusBucket, todayRange} from './utils'

const pageSize = 100
const locale = computed(() => elementLocale())

const [dayStart, dayEnd] = todayRange()
const filters = reactive({
  time_from: dayStart.toISOString(),
  time_to: dayEnd.toISOString(),
})
const stats = reactive({})
const llmStats = reactive({})
const items = ref([])
const backendStats = ref([])
const models = ref([])
const backends = ref([])
const dailyStats = ref([])
const scheduler = ref(null)
const outputSec = ref(0)
const hasMore = ref(false)
const loadingMore = ref(false)
const autoRefresh = ref(true)
const lastUpdated = ref('-')
const liveState = ref('retry')
const liveText = ref(t('connecting'))
const drawerVisible = ref(false)
const selectedRequest = ref(null)
const filterPanel = ref(null)

const hasFilters = computed(() => Object.keys(filters).length > 0)

// 总览/提供商/每日趋势使用固定的默认窗口，不受时间筛选影响（时间筛选只作用于请求列表）
const timeWindow = computed(() => ({secs: 3600, label: t('filterLast1h')}))

const quickCounts = computed(() => {
  const c = {ok: 0, err4xx: 0, err5xx: 0, stream: 0}
  for (const it of items.value) {
    const b = statusBucket(it.status_code || 0)
    if (b === 'ok') c.ok++
    else if (b === 'err4xx') c.err4xx++
    else if (b === 'err5xx') c.err5xx++
    if (it.is_streaming) c.stream++
  }
  return c
})

let eventSource = null

function setLive(mode, text) {
  liveState.value = mode
  liveText.value = text
}

function setFilters(f) {
  Object.keys(filters).forEach((k) => delete filters[k])
  Object.assign(filters, f)
}

// 总览类面板使用的筛选条件：去掉时间范围（时间范围只影响请求列表）
function overviewFilters() {
  const {time_from, time_to, ...rest} = filters
  return rest
}

async function loadStats() {
  try {
    const base = overviewFilters()
    const [data, llm] = await Promise.all([
      fetchStats(base),
      fetchStats({...base, chat_completions_only: 'true'}),
    ])
    Object.keys(stats).forEach((k) => delete stats[k])
    Object.assign(stats, data)
    Object.keys(llmStats).forEach((k) => delete llmStats[k])
    Object.assign(llmStats, llm)
    lastUpdated.value = fmtDate(new Date().toISOString())
  } catch (err) {
    setLive('error', t('refreshFailed', {msg: err.message}))
  }
}

async function loadRequests() {
  loadingMore.value = false
  try {
    // 首页刷新恒用 pageSize：不能用 items.value.length（筛选后行数变少会限制取数上限）
    const data = await fetchRequests(pageSize, 0, {...filters})
    items.value = data.items || []
    hasMore.value = (data.items || []).length >= pageSize
    const rates = items.value
        .map((it) => Number(it.decode_tok_per_sec || 0))
        .filter((v) => Number.isFinite(v) && v > 0)
    outputSec.value = rates.length ? rates.reduce((a, b) => a + b, 0) / rates.length : 0
  } catch (err) {
    setLive('error', t('refreshFailed', {msg: err.message}))
  }
}

async function loadMore() {
  if (loadingMore.value || !hasMore.value) return
  loadingMore.value = true
  try {
    const data = await fetchRequests(pageSize, items.value.length, {...filters})
    items.value = items.value.concat(data.items || [])
    hasMore.value = (data.items || []).length >= pageSize
  } catch (err) {
    ElMessage.error(t('loadMoreFailed', {msg: err.message}))
  } finally {
    loadingMore.value = false
  }
}

async function loadBackendStats() {
  try {
    backendStats.value = await fetchStatsByBackend(overviewFilters())
  } catch {
    /* ignore */
  }
}

async function loadDailyStats() {
  try {
    const data = await fetchDailyStats(30, overviewFilters())
    dailyStats.value = data.items || []
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

async function loadOptions() {
  try {
    models.value = await fetchModels()
  } catch { /* ignore */
  }
  try {
    backends.value = await fetchBackends()
  } catch { /* ignore */
  }
}

async function refreshAll() {
  await Promise.all([loadStats(), loadRequests(), loadBackendStats(), loadDailyStats(), loadScheduler()])
}

function onApply(f) {
  setFilters(f)
  refreshAll().catch(() => {
  })
}

function onBackendSelect(url) {
  if (filterPanel.value) {
    filterPanel.value.setBackend(url)
  }
  filters.backend = url
  refreshAll().catch(() => {
  })
}

function onClientIPSelect(ip) {
  if (filterPanel.value) {
    filterPanel.value.setClientIP(ip)
  }
  filters.client_ip = ip
  refreshAll().catch(() => {
  })
}

function onUserAgentSelect(ua) {
  if (filterPanel.value) {
    filterPanel.value.setUserAgent(ua)
  }
  filters.user_agent = ua
  refreshAll().catch(() => {
  })
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
  refreshAll().catch(() => {
  })
}

function onLangCommand(lang) {
  if (lang === currentLang.value) return
  setLang(lang)
}

function connectEvents() {
  eventSource = new EventSource(`${API_PROXY}events`)
  eventSource.onopen = () => setLive('live', t('liveConnected'))
  eventSource.onerror = () => setLive('retry', t('reconnecting'))
  // 监听不同事件类型，直接更新对应数据
  eventSource.addEventListener('stats', (e) => {
    const data = JSON.parse(e.data)
    Object.keys(stats).forEach((k) => delete stats[k])
    Object.assign(stats, data)
    lastUpdated.value = fmtDate(new Date().toISOString())
  })
  eventSource.addEventListener('llmStats', (e) => {
    const data = JSON.parse(e.data)
    Object.keys(llmStats).forEach((k) => delete llmStats[k])
    Object.assign(llmStats, data)
  })
  eventSource.addEventListener('request', (e) => {
    // 请求事件：刷新请求列表
    loadRequests().catch(() => {})
  })
  eventSource.addEventListener('scheduler', (e) => {
    scheduler.value = JSON.parse(e.data)
  })
}

function refreshNow() {
  refreshAll().catch((err) => setLive('error', t('refreshFailed', {msg: err.message})))
}

onMounted(async () => {
  setLang(currentLang.value)
  connectEvents()
  await refreshAll()
  await loadOptions()
  // 不再使用轮询定时器，所有数据更新由 SSE 事件驱动
})

onUnmounted(() => {
  if (eventSource) eventSource.close()
})
</script>
