package main

// 子窗（设置、文件互传）的自绘拖动。
//
// 为什么不用 mygo 的顶栏拖动（系统拖动）：那块窗口是 noRedirect + 自绘 alpha 的，
// 系统拖动时是 DWM 直接"搬运"上一帧表面 —— 它不认 alpha，也不给我们重画的机会：
// 搬过去的是一块矩形，四个角立刻变直，玻璃（模糊）也跟着没了，看起来就是
// "拖动时又变直角、又没模糊"。主面板早就避开了这条路（它一直是自绘拖动）。
//
// 这里做的是同一件事的精简版：按住顶栏空白处 → SetCapture 接管 → 每组鼠标移动
// 都把窗口摆到新位置、顺手把投影跟过去、并按 25ms 节流重烘一次底图（底图是
// 窗口坐标系的，弧永远在窗口边缘上）。松手再精确重烘一次、重画投影。
//
// 只接管**顶栏空白区**：顶栏右边是「✕ 关闭」，客户区下面是各种控件，那些位置
// 的按下必须原样交给 mygo，否则按钮就点不动了。

import (
	"log"
	"time"
	"unsafe"

	"github.com/egoist/mygo"
	"golang.org/x/sys/windows"

	"baozi/internal/win"
)

const (
	subDragSubclassID = 0x53444732 // "SDG2"

	// 位移超过这么多物理像素才算拖动，否则当普通点击。
	subDragThreshold = 5

	// 顶栏（含标题、副标题）的高度，DIP。下面是内容区，不接管按下。
	subTopBarDip = 58

	// 顶栏右侧「✕ 关闭」占的宽度，DIP。这块要让给按钮。
	subCloseAreaDip = 120

	// 拖动中重烘底图的节流间隔。
	subBakeInterval = 25 * time.Millisecond

	wmCaptureChanged = 0x0215
)

type subDrag struct {
	win   *mygo.Window
	host  uintptr // 顶层窗口：真正的移动目标是它
	hwnd  uintptr // 挂子类化的窗口：MyGoSurface，鼠标消息落点
	glass *subGlass
	tramp uintptr

	pending bool // 按下但还没过阈值
	active  bool // 已接管

	startCurX, startCurY int32
	startWinX, startWinY int32
	lastBake             time.Time
}

func installSubDrag(w *mygo.Window, g *subGlass) {
	if w == nil || g == nil {
		return
	}
	// 句柄是 Show 时才建出来的：摸空就安静返回，show()/refreshShadow() 会再调。
	if g.drag != nil {
		return
	}
	host := w.NativeHandle()
	if host == 0 {
		return
	}
	surf := findMyGoSurface(host)
	if surf == 0 {
		return
	}
	d := &subDrag{win: w, host: host, hwnd: surf, glass: g}
	d.tramp = windows.NewCallback(d.proc)
	if r, _, err := procSetSubclass.Call(surf, d.tramp, subDragSubclassID, 0); r == 0 {
		log.Printf("子窗拖动: %s 没挂上（%v）", g.name, err)
		return
	}
	g.drag = d
	log.Printf("子窗拖动: %s 已接管（surface=%d）", g.name, surf)
}

func abs32(v int32) int32 {
	if v < 0 {
		return -v
	}
	return v
}

// begin 开始一次拖动（记下光标和窗口的起点），并接管鼠标捕获。
//
// 两条路都会走到这里：mygo 走客户区消息（surface 的 WM_LBUTTONDOWN），或者
// 它把顶栏声明成 HTCAPTION、让系统直接发 WM_NCLBUTTONDOWN 给顶层窗口。
func (d *subDrag) begin() {
	var p point
	procGetCursor.Call(uintptr(unsafe.Pointer(&p)))
	wx, wy, _, _ := win.WindowRectPixels(d.host)
	d.startCurX, d.startCurY = p.X, p.Y
	d.startWinX, d.startWinY = int32(wx), int32(wy)
	d.pending = true
	d.active = false
	procSetCapture.Call(d.hwnd)
}

// inTopBar 判断这次按下是不是落在"顶栏空白区"（能当拖动把手的地方）。
func (d *subDrag) inTopBar(x, y int32) bool {
	_, _, cw, chh := win.ClientRectPixels(d.hwnd)
	if cw <= 0 || chh <= 0 {
		return false
	}
	s := d.glass.app.glassScale()
	if s <= 0 {
		s = 1
	}
	topH := int32(subTopBarDip * s)
	closeW := int32(subCloseAreaDip * s)
	return y >= 0 && y < topH && x >= 0 && x < int32(cw)-closeW
}

func (d *subDrag) proc(hwnd, msg, wparam, lparam, id, data uintptr) uintptr {
	switch msg {
	case wmLButtonDown:
		x := int32(int16(lparam & 0xFFFF))
		y := int32(int16((lparam >> 16) & 0xFFFF))
		if d.inTopBar(x, y) {
			d.begin()
			// 吃掉这次按下：不让 mygo 再走它自己的（系统）拖动。
			return 0
		}

	case wmLButtonUp:
		if d.active {
			d.active = false
			d.pending = false
			procRelCapture.Call()
			d.glass.refresh(true)
			d.glass.refreshShadow()
			return 0
		}
		d.pending = false

	case wmMouseMove:
		if !d.pending && !d.active {
			break
		}
		var p point
		procGetCursor.Call(uintptr(unsafe.Pointer(&p)))
		dx := p.X - d.startCurX
		dy := p.Y - d.startCurY
		if !d.active {
			if abs32(dx) < subDragThreshold && abs32(dy) < subDragThreshold {
				return 0
			}
			d.active = true
			procSetCapture.Call(d.hwnd)
		}
		nx := d.startWinX + dx
		ny := d.startWinY + dy
		procSetWinPos.Call(d.host, 0, uintptr(nx), uintptr(ny), 0, 0,
			swpNoSize|swpNoZOrder|swpNoActivate)
		d.glass.followShadow()
		if time.Since(d.lastBake) >= subBakeInterval {
			d.lastBake = time.Now()
			d.glass.refresh(true)
		}
		// 拖动中不要交给 mygo：它的 hover/点击逻辑会把光标当成"在顶栏上"。
		return 0

	case wmCaptureChanged:
		// 被系统/别的窗口抢走了捕获：结束这次拖动，别留下半截状态。
		if d.active {
			d.active = false
			d.pending = false
			d.glass.refresh(true)
			d.glass.refreshShadow()
		}
	}
	r, _, _ := procDefSubclass.Call(hwnd, msg, wparam, lparam, id, data)
	return r
}
