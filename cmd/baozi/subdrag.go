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

	// 拖动中重烘底图的节流间隔。整张 1150x1025 约 8ms，60Hz 的鼠标移动也就是
	// 16ms 一帧 —— 12ms 能让背景基本跟着手走，又不至于把 UI 线程占满。
	subBakeInterval = 12 * time.Millisecond

	wmCaptureChanged = 0x0215

	// GetAsyncKeyState 的左键位。
	vkLeftButton = 0x01
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
	if g.drag != nil {
		// 已经挂上了。每次重新挂一次，把自己提到子类化链的**最前面**：
		// SetWindowSubclass 是后进先出，mygo 在渲染/重设时可能又挂了自己的那层，
		// 挡在我们前面把按下吃掉（表现就是"我们的拖动根本没收到消息"）。
		g.drag.rearm()
		return
	}
	host := w.NativeHandle()
	if host == 0 {
		g.retrySubDrag(w)
		return
	}
	surf := findMyGoSurface(host)
	if surf == 0 {
		// mygo 的 MyGoSurface 是 Show 之后才 createWindow 的，时序偶尔差一拍。
		// 摸空就隔 50ms 再试几拍 —— 主面板当年就栽在这上面：日志只留一句
		// "没挂上"，用户那边就是"又拖不动了"，然后又退回系统拖动，四角继续变直。
		g.retrySubDrag(w)
		return
	}
	d := &subDrag{win: w, host: host, hwnd: surf, glass: g}
	d.tramp = windows.NewCallback(d.proc)
	if r, _, err := procSetSubclass.Call(surf, d.tramp, subDragSubclassID, 0); r == 0 {
		log.Printf("子窗拖动: %s 没挂上（%v）", g.name, err)
		g.retrySubDrag(w)
		return
	}
	g.drag = d
	log.Printf("子窗拖动: %s 已接管（surface=%d）", g.name, surf)
}

// retrySubDrag 隔一拍再试（最多几次）。没有它，surface 建得比 Show 晚一点，
// 这一辈子就都挂不上。
func (g *subGlass) retrySubDrag(w *mygo.Window) {
	if w == nil || g.dragRetry <= 0 {
		return
	}
	g.dragRetry--
	time.AfterFunc(50*time.Millisecond, func() {
		w.Update(func() {})
		installSubDrag(w, g)
	})
}

// rearm 把我们的子类化重新挂一次，保证它在链首（后注册先收到）。
func (d *subDrag) rearm() {
	if d.hwnd == 0 || d.tramp == 0 {
		return
	}
	procDelSubclass.Call(d.hwnd, d.tramp, subDragSubclassID, 0)
	procSetSubclass.Call(d.hwnd, d.tramp, subDragSubclassID, 0)
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
	// 和主面板一样：一按下就切到"糊透"的玻璃（setDragFade），松手再恢复。
	//
	// 这一步才是主面板拖动"看着正常"的真相：拖动时玻璃被糊成一整片，
	// 里面的桌面色本来就看不出位移，所以不会出现"背景停在原地"的观感。
	// 子窗之前没做这步，玻璃还停在起点那块桌面上 —— 用户一眼就看出来。
	d.glass.app.setDragFade(true)
	d.glass.refresh(true)
	log.Printf("子窗拖动: %s begin（光标 %d,%d 窗口 %d,%d）",
		d.glass.name, d.startCurX, d.startCurY, d.startWinX, d.startWinY)
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

// leftButtonDown 问一下"左键现在还按着吗"。捕获丢了要靠它分辨是拖完了还是被抢了。
func leftButtonDown() bool {
	r, _, _ := procGetAsyncKey.Call(vkLeftButton)
	return int16(r) < 0
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
			d.glass.app.setDragFade(false)
			d.glass.refresh(true)
			d.glass.refreshShadow()
			return 0
		}
		if d.pending {
			// 没到阈值：当普通点击。仍然要把糊化退掉。
			d.pending = false
			d.glass.app.setDragFade(false)
			d.glass.refresh(true)
			break
		}

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
			log.Printf("子窗拖动: %s 开始移动", d.glass.name)
		}
		// 每帧确认捕获还在自己手上：窗口一挪，系统/别的路径就可能把它拿走，
		// 拿走了就再也收不到 mousemove，拖动看着就是"动一下就停住"。
		procSetCapture.Call(d.hwnd)
		nx := d.startWinX + dx
		ny := d.startWinY + dy
		procSetWinPos.Call(d.host, 0, uintptr(nx), uintptr(ny), 0, 0,
			swpNoSize|swpNoZOrder|swpNoActivate)
		d.glass.followShadow()
		if time.Since(d.lastBake) >= subBakeInterval {
			d.lastBake = time.Now()
			d.glass.refresh(true)
		}
		// 关键的一步：把这一帧**真的推上屏**。主面板的 dragTo() 里也是这么做的
		// （panel.FrameNow()）。少了它，窗口被 SetWindowPos 搬走、我们这边也把
		// 底图烘好了，画面却还停在上一帧上 —— 用户看到的就是"拖动时背景根本
		// 不动，要松开鼠标才变"，而且搬走的那块还是矩形（四个直角）。
		d.win.FrameNow()
		// 拖动中不要交给 mygo：它的 hover/点击逻辑会把光标当成"在顶栏上"。
		return 0

	case wmCaptureChanged:
		// 捕获丢了。如果左键还按着，那就是被别的窗口/系统抢走了（窗口移动、
		// 提最前、mygo 的重设都会来这一下），必须抢回来继续拖 —— 否则"拖一下
		// 就停在那里"，用户那边就是"又拖不动了"、而且是拖一点点就卡住的怪样子。
		if d.active || d.pending {
			if leftButtonDown() {
				procSetCapture.Call(d.hwnd)
				break
			}
			d.active = false
			d.pending = false
			procRelCapture.Call()
			d.glass.app.setDragFade(false)
			d.glass.refresh(true)
			d.glass.refreshShadow()
		}
	}
	r, _, _ := procDefSubclass.Call(hwnd, msg, wparam, lparam, id, data)
	return r
}
