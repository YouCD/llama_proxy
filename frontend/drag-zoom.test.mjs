// 直接测试 src/dragZoom.js 的真实鼠标事件路径（zr.trigger 注入合成事件，绕过 DOM）。
// 覆盖：左键判断(which)、阈值、选区带创建/清理、像素→索引换算、dispatch dataZoom。
import * as echarts from 'echarts/core'
import { LineChart } from 'echarts/charts'
import { GridComponent, DataZoomComponent } from 'echarts/components'
import { SVGRenderer } from 'echarts/renderers'
import { attachDragZoom } from './src/dragZoom.js'

echarts.use([LineChart, GridComponent, DataZoomComponent, SVGRenderer])

const N = 30
const dates = Array.from({ length: N }, (_, i) => `d${i}`)
// grid: left 30, right 10, width 800 → 绘图区 x∈[30,790]，宽 760
const chart = echarts.init(null, null, { renderer: 'svg', ssr: true, width: 800, height: 300 })
chart.setOption({
  grid: { left: 30, right: 10, top: 20, bottom: 30 },
  dataZoom: [{ type: 'inside', xAxisIndex: 0, moveOnMouseMove: false }],
  xAxis: { type: 'category', data: dates, boundaryGap: false },
  yAxis: { type: 'value' },
  series: [{ type: 'line', data: dates.map((_, i) => i) }],
})

// 索引 i 的像素 x = 30 + i/(N-1) * 760
const idxToX = (i) => 30 + (i / (N - 1)) * 760
const MID_Y = 150 // 绘图区中部

const events = []
chart.on('datazoom', (e) => events.push(e))

const zr = chart.getZr()
const cleanup = attachDragZoom(chart)

// 合成 zrender 鼠标事件包：真实包由 makeEventPacket 构造，带 event 子对象
// （ECharts RoamController 的处理器会对 e.event 调 preventDefault/stopPropagation）。
const mkEvent = (which, offsetX, offsetY) => ({
  type: 'mouse',
  which,
  offsetX,
  offsetY,
  event: { which, preventDefault() {}, stopPropagation() {} },
  stop() {},
})
// 选区带是 graphic.Rect 根元素；图表自身的系列/轴是 Path，不会误判。
const selRectCount = () => zr.storage.getRoots().filter((el) => el instanceof echarts.graphic.Rect).length

let failed = 0
function assert(cond, msg) {
  if (cond) console.log('  ✓ ' + msg)
  else { failed++; console.error('  ✗ ' + msg) }
}

console.log('1) 左键拖拽 d5→d15：应触发 dataZoom [5,15]')
zr.trigger('mousedown', mkEvent(1, idxToX(5), MID_Y))
zr.trigger('mousemove', mkEvent(1, idxToX(10), MID_Y)) // 中途，画选区带
zr.trigger('mousemove', mkEvent(1, idxToX(15), MID_Y))
// 选区带应存在
assert(selRectCount() === 1, '拖拽中创建了选区矩形元素')
zr.trigger('mouseup', mkEvent(1, idxToX(15), MID_Y))
assert(events.length === 1, '松手触发 1 次 datazoom 事件')
if (events[0]) {
  const b = (events[0].batch && events[0].batch[0]) || events[0]
  assert(b.startValue === 5 && b.endValue === 15, `dispatch 出 [${b.startValue}, ${b.endValue}]（期望 [5,15]）`)
}
assert(selRectCount() === 0, '松手后选区矩形已清理')

console.log('2) 右键拖拽：which=3 应被忽略，不触发缩放')
events.length = 0
zr.trigger('mousedown', mkEvent(3, idxToX(0), MID_Y))
zr.trigger('mousemove', mkEvent(3, idxToX(10), MID_Y))
zr.trigger('mouseup', mkEvent(3, idxToX(10), MID_Y))
assert(events.length === 0, '右键不触发 datazoom')

console.log('3) 点击（位移 < 阈值）：不触发缩放')
events.length = 0
zr.trigger('mousedown', mkEvent(1, idxToX(0), MID_Y))
zr.trigger('mouseup', mkEvent(1, idxToX(0) + 3, MID_Y)) // 3px < 5
assert(events.length === 0, '小位移点击不触发 datazoom')

console.log('4) 绘图区外 mousedown：不启动拖拽')
events.length = 0
zr.trigger('mousedown', mkEvent(1, 5, 5)) // 左上角，grid 外
zr.trigger('mousemove', mkEvent(1, idxToX(10), MID_Y))
zr.trigger('mouseup', mkEvent(1, idxToX(10), MID_Y))
assert(events.length === 0, 'grid 外按下不触发 datazoom')

console.log('5) 拖拽中移出画布（globalout）：丢弃选区，不触发缩放')
events.length = 0
zr.trigger('mousedown', mkEvent(1, idxToX(0), MID_Y))
zr.trigger('mousemove', mkEvent(1, idxToX(10), MID_Y))
assert(selRectCount() === 1, '移出前选区带存在')
zr.trigger('globalout', {})
assert(selRectCount() === 0, 'globalout 后选区带已清理')
zr.trigger('mouseup', mkEvent(1, idxToX(10), MID_Y))
assert(events.length === 0, '移出后松手不触发 datazoom')

console.log('6) 反向拖拽（右→左）：仍应得到升序 [5,15]')
// 测试 1 已把图表缩放到 [5,15]，先重置回全窗口，idxToX 的像素映射才有效
chart.dispatchAction({ type: 'dataZoom', dataZoomIndex: 0, startValue: 0, endValue: N - 1 })
events.length = 0
zr.trigger('mousedown', mkEvent(1, idxToX(15), MID_Y))
zr.trigger('mousemove', mkEvent(1, idxToX(5), MID_Y))
zr.trigger('mouseup', mkEvent(1, idxToX(5), MID_Y))
if (events[0]) {
  const b = (events[0].batch && events[0].batch[0]) || events[0]
  assert(b.startValue === 5 && b.endValue === 15, `反向拖拽得 [${b.startValue}, ${b.endValue}]（升序）`)
}

cleanup() // 解绑
chart.dispose()
console.log(failed === 0 ? '\n全部通过' : `\n${failed} 项失败`)
process.exit(failed === 0 ? 0 : 1)
