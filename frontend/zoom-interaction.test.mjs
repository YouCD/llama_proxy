// 验证 DailyChart 重构后的交互链路（ECharts SSR 模式模拟）：
//  拖拽框选缩放是手动实现（zrender 鼠标事件），mouseup 时 dispatch 给 inside
//  dataZoom（index 0）startValue/endValue —— 本测试从这一步开始模拟，验证：
//   1. datazoom 事件 batch 格式 → handler 解析窗口索引
//   2. 三主图 + 滑块统一同步到同一窗口
//   3. 框选放大后用滚轮缩回更大范围不被钳制
//   4. 双击重置 → 全窗口
import * as echarts from 'echarts'

const N = 30 // 模拟 30 个数据点
const dates = Array.from({ length: N }, (_, i) => `2026-07-${String(i + 1).padStart(2, '0')}`)

// 与 DailyChart.vue 完全一致的主图 dataZoom 配置（仅 inside）
function dataZooms() {
  return [{ type: 'inside', xAxisIndex: 0, zoomOnMouseWheel: true }]
}

function initMainChart() {
  const chart = echarts.init(null, null, { renderer: 'svg', ssr: true, width: 800, height: 300 })
  chart.setOption({
    tooltip: { trigger: 'axis', axisPointer: { type: 'line' } },
    grid: { left: 10, right: 10, top: 20, bottom: 30, containLabel: true },
    dataZoom: dataZooms(),
    xAxis: { type: 'category', data: dates, boundaryGap: false },
    yAxis: { type: 'value' },
    series: [{ type: 'line', data: dates.map((_, i) => i * 7) }],
  })
  return chart
}

// 与 makeZoomHandler 中相同的窗口索引计算
function handlerIndices(e) {
  const batch = (e.batch && e.batch[0]) || e
  let startIdx, endIdx
  if (typeof batch.startValue === 'number' && typeof batch.endValue === 'number') {
    startIdx = batch.startValue
    endIdx = batch.endValue
  } else {
    startIdx = Math.round(((batch.start ?? 0) / 100) * (N - 1))
    endIdx = Math.round(((batch.end ?? 100) / 100) * (N - 1))
  }
  startIdx = Math.max(0, Math.min(startIdx, N - 1))
  endIdx = Math.max(startIdx, Math.min(endIdx, N - 1))
  return { startIdx, endIdx }
}

// 与 applyZoomToAll 相同：统一写全部图表的 index 0
function applyZoomToAll(charts, startIdx, endIdx) {
  for (const chart of charts) {
    chart.dispatchAction({ type: 'dataZoom', dataZoomIndex: 0, startValue: startIdx, endValue: endIdx })
  }
}

// 读取图表当前 dataZoom 窗口
function visibleWindow(chart) {
  const opt = chart.getOption()
  return opt.dataZoom.map((d) => [Math.round(d.startValue ?? 0), Math.round(d.endValue ?? N - 1)])
}

let failed = 0
function assert(cond, msg) {
  if (cond) console.log('  ✓ ' + msg)
  else { failed++; console.error('  ✗ ' + msg) }
}

const tokenChart = initMainChart()
const statusChart = initMainChart()
const requestsChart = initMainChart()
const charts = [tokenChart, statusChart, requestsChart]
let syncingZoom = false
const events = []
for (const c of charts) {
  c.on('datazoom', (e) => {
    if (syncingZoom) return
    events.push(e)
  })
}

console.log('1) 拖拽框选：mouseup 时 dispatch index0 [5,15]（模拟 attachDragZoom 的 onUp）')
tokenChart.dispatchAction({ type: 'dataZoom', dataZoomIndex: 0, startValue: 5, endValue: 15 })
assert(events.length === 1, '框选缩放触发了 datazoom 事件')
const { startIdx, endIdx } = handlerIndices(events[0])
assert(startIdx === 5 && endIdx === 15, `handler 解析出窗口 [${startIdx}, ${endIdx}]`)

syncingZoom = true
applyZoomToAll(charts, startIdx, endIdx)
syncingZoom = false
for (const [name, c] of charts.entries()) {
  const w = visibleWindow(c)
  assert(w.every((r) => r[0] === 5 && r[1] === 15), `图表${name} 同步到 [5,15]：${JSON.stringify(w)}`)
}

console.log('2) 框选放大后用滚轮缩回 [0, 29]')
events.length = 0
tokenChart.dispatchAction({ type: 'dataZoom', dataZoomIndex: 0, startValue: 0, endValue: 29 })
const { startIdx: s2, endIdx: e2 } = handlerIndices(events[0])
syncingZoom = true
applyZoomToAll(charts, s2, e2)
syncingZoom = false
for (const [name, c] of charts.entries()) {
  const w = visibleWindow(c)
  assert(w.every((r) => r[0] === 0 && r[1] === 29), `图表${name} 恢复全窗口（不被钳制）：${JSON.stringify(w)}`)
}

console.log('3) 双击重置：dispatch index0 全窗口 → handler 统一所有图表')
// 先制造缩放状态
syncingZoom = true
applyZoomToAll(charts, 3, 8)
syncingZoom = false
events.length = 0
tokenChart.dispatchAction({ type: 'dataZoom', dataZoomIndex: 0, startValue: 0, endValue: N - 1 })
const ev2 = events[0]
const r2 = ev2 ? handlerIndices(ev2) : null
assert(r2 && r2.startIdx === 0 && r2.endIdx === N - 1, '双击事件被解析为全窗口')
if (r2) {
  syncingZoom = true
  applyZoomToAll(charts, r2.startIdx, r2.endIdx)
  syncingZoom = false
}
for (const [name, c] of charts.entries()) {
  const w = visibleWindow(c)
  assert(w.every((r) => r[0] === 0 && r[1] === N - 1), `图表${name} 双击后全窗口：${JSON.stringify(w)}`)
}

for (const c of charts) c.dispose()
console.log(failed === 0 ? '\n全部通过' : `\n${failed} 项失败`)
process.exit(failed === 0 ? 0 : 1)
