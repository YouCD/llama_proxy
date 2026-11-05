// Grafana 式拖拽框选缩放：在绘图区按住拖动出现高亮选区带，松手时把像素区间
// 换算成数据索引，dispatch 给 inside dataZoom（触发 datazoom 事件，由调用方
// 监听同步其余图表并抛出范围变更）。选区为半透明蓝色带，与图表内容区分明显。
//
// 为何自实现：ECharts 6 的 dataZoom.select 是 toolbox 专用组件，未进任何公共
// 导出（components 桶注释 "Do not install './dataZoomSelect'"），无法直接挂载。
//
// 事件字段说明：zrender 鼠标事件包（makeEventPacket）无顶层 button 字段，
// which===1 为左键（2=中键 3=右键）；offsetX/offsetY 为画布相对像素坐标。
import * as echarts from 'echarts/core'

const DRAG_THRESHOLD = 5 // px，小于此位移视为点击，不触发缩放

// 把拖拽框选交互挂到图表的 zrender 层，返回清理函数。
export function attachDragZoom(chart) {
  const zr = chart.getZr()
  let startX = null
  let selRect = null

  const gridRect = () => chart.getModel().getComponent('grid', 0).coordinateSystem.getRect()
  const inGrid = (x, y) => {
    const r = gridRect()
    return x >= r.x && x <= r.x + r.width && y >= r.y && y <= r.y + r.height
  }
  const pixelToIdx = (x) => {
    const n = (chart.getOption().xAxis[0].data || []).length
    if (!n) return 0
    return Math.max(0, Math.min(Math.round(chart.convertFromPixel({ xAxisIndex: 0 }, x)), n - 1))
  }
  const ensureSelRect = (x) => {
    if (selRect) return selRect
    const r = gridRect()
    selRect = new echarts.graphic.Rect({
      silent: true,
      z: 100,
      shape: { x, y: r.y, width: 0, height: r.height },
      style: { fill: 'rgba(59, 130, 246, 0.15)', stroke: 'rgba(59, 130, 246, 0.65)', lineWidth: 1 },
    })
    zr.add(selRect)
    return selRect
  }
  const removeSelRect = () => {
    if (selRect) {
      zr.remove(selRect)
      selRect = null
    }
  }

  const onDown = (e) => {
    if (e.which !== 1) return
    if (!inGrid(e.offsetX, e.offsetY)) return
    startX = e.offsetX
  }
  const onMove = (e) => {
    if (startX === null) return
    if (Math.abs(e.offsetX - startX) < DRAG_THRESHOLD) return
    const r = gridRect()
    const x0 = Math.max(r.x, Math.min(startX, e.offsetX))
    const x1 = Math.min(r.x + r.width, Math.max(startX, e.offsetX))
    ensureSelRect(x0).attr({ shape: { x: x0, y: r.y, width: x1 - x0, height: r.height } })
  }
  const onUp = (e) => {
    if (startX === null) return
    const moved = Math.abs(e.offsetX - startX) >= DRAG_THRESHOLD
    const x0 = Math.min(startX, e.offsetX)
    const x1 = Math.max(startX, e.offsetX)
    removeSelRect()
    startX = null
    if (!moved) return
    let i0 = pixelToIdx(x0)
    let i1 = pixelToIdx(x1)
    if (i1 <= i0) i1 = Math.min(i0 + 1, (chart.getOption().xAxis[0].data || []).length - 1)
    chart.dispatchAction({ type: 'dataZoom', dataZoomIndex: 0, startValue: i0, endValue: i1 })
  }
  // 拖拽中鼠标移出画布：丢弃选区，避免残留
  const onGlobalOut = () => {
    startX = null
    removeSelRect()
  }

  zr.on('mousedown', onDown)
  zr.on('mousemove', onMove)
  zr.on('mouseup', onUp)
  zr.on('globalout', onGlobalOut)
  return () => {
    zr.off('mousedown', onDown)
    zr.off('mousemove', onMove)
    zr.off('mouseup', onUp)
    zr.off('globalout', onGlobalOut)
    removeSelRect()
  }
}
