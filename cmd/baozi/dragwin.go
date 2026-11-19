package main

// 自绘拖动：把面板的拖动从系统的模态拖动循环里摘出来。
//
// 为什么必须这么做：系统的拖动（mygo 的 DragWindow = ReleaseCapture +
// SendMessage(WM_NCLBUTTONDOWN, HTCAPTION)）由 Windows 自己处理鼠标。窗口位置是
// 系统**立即**设的，玻璃内容却要等我们算完 → 提交 → DWM 合成，实测帧周期
// 24~36ms（每帧 RedrawWindow 都在等 vsync）。错位量 = 拖动速度 × 帧周期，
// 快拖时十几像素，一眼就看出来。
//
// 自己接管以后顺序可以反过来：**先把内容画好、再挪窗口、再 Present**，让
// "窗口新位置"和"新内容"落在同一次合成里 —— 相对位移为 0，怎么快都不错位。
//
// 消息处理上的取舍：
//   · 左键按下照旧转发给 mygo（图标、搜索框、底部按钮的按下态和聚焦都保持原样）；
//   · 位移超过 6px 才开始接管，之前照旧转发（单击、拖选不受影响）；
//   · 接管之后 move 不再转发（免得 mygo 还在做 hover），up 也不转发
//     （否则会把"在拖动终点的那个图标"当成点击打开）；
//   · 于是 mygo 认为鼠标还按着 —— 松手后它会等下一次按下自己复位，视觉上
//     最坏是某个按钮保持按下色，不影响使用。

import (
	"log"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"baozi/internal/win"
)

var (
	dragUser32      = windows.NewLazySystemDLL("user32.dll")
	procSetWndProc  = dragUser32.NewProc("SetWindowLongPtrW")
	procCallWndProc = dragUser32.NewProc("CallWindowProcW")
	procSetCapture  = dragUser32.NewProc("SetCapture")
	procRelCapture  = dragUser32.NewProc("ReleaseCapture")
	procGetCursor   = dragUser32.NewProc("GetCursorPos")
	procSetWinPos   = dragUser32.NewProc("SetWindowPos")
	procGetKeyState = dragUser32.NewProc("GetKeyState")
)

const (
	wmLButtonDown = 0x0201
	wmLButtonUp   = 0x0202
	wmMouseMove   = 0x0200
	wmCancelMode  = 0x001F

	// GWLP_WNDPROC 在 64 位上就是 -4。
	gwlpWndProc = ^uintptr(3)

	swpNoZOrder   = 0x0004
	swpNoActivate = 0x0010
	swpNoSize     = 0x0001

	vkLButton = 0x01

	// 开始接管的位移阈值（物理像素）。小于它是普通点击/拖选，照旧交给 mygo。
	dragThreshold = 6
)

type point struct{ X, Y int32 }

// selfDrag 是面板自绘拖动的状态机。
type selfDrag struct {
	app   *application
	hwnd  uintptr
	proc  uintptr // 原来的 WndProc（mygo 的），转发消息给它
	tramp uintptr // 我们注册的 Go 回调

	pending bool // 按下但还没到阈值
	active  bool // 已接管

	startCursorX, startCursorY int32
	startWinX, startWinY       int32
	lastX, lastY               int32

	frames int
	t0     time.Time

	// clientDX/clientDY 是客户区相对窗口左上角的偏移。窗口有 THICKFRAME
	// （那圈看不见的缩放边框），客户区原点不等于窗口原点；底图取景用的是**客户区**
	// 原点，直接用 SetWindowPos 的 x,y 会让整张玻璃偏出几个像素。
	clientDX, clientDY int32
}

// installSelfDrag 在面板窗口上装一份自己的 WndProc。必须在窗口建好之后调用。
func (a *application) installSelfDrag(hwnd uintptr) {
	if hwnd == 0 || a.selfDrag != nil {
		return
	}
	d := &selfDrag{app: a, hwnd: hwnd}
	d.tramp = windows.NewCallback(d.wndProc)
	prev, _, _ := procSetWndProc.Call(hwnd, gwlpWndProc, d.tramp)
	if prev == 0 {
		log.Printf("拖动: 装不上自己的 WndProc，退回系统拖动")
		return
	}
	d.proc = prev
	a.selfDrag = d
	log.Printf("拖动: 已接管（位移超过 %d 像素才真正开始拖）", dragThreshold)
}

// detach 在窗口销毁前把原来的 WndProc 放回去。
func (d *selfDrag) detach() {
	if d.hwnd != 0 && d.proc != 0 {
		procSetWndProc.Call(d.hwnd, gwlpWndProc, d.proc)
	}
	d.hwnd = 0
}

func (d *selfDrag) call(hwnd, msg, wp, lp uintptr) uintptr {
	if d.proc == 0 {
		return 0
	}
	ret, _, _ := procCallWndProc.Call(d.proc, hwnd, msg, wp, lp)
	return ret
}

func cursorPos() (int32, int32) {
	var p point
	procGetCursor.Call(uintptr(unsafe.Pointer(&p)))
	return p.X, p.Y
}

func windowXY(hwnd uintptr) (int32, int32) {
	x, y, _, _ := win.WindowRectPixels(hwnd)
	return int32(x), int32(y)
}

func setWindowXY(hwnd uintptr, x, y int32) {
	procSetWinPos.Call(hwnd, 0, uintptr(x), uintptr(y), 0, 0,
		swpNoSize|swpNoZOrder|swpNoActivate)
}

func (d *selfDrag) wndProc(hwnd, msg, wp, lp uintptr) uintptr {
	switch msg {
	case wmLButtonDown:
		d.pending = true
		d.active = false
		d.frames = 0
		d.t0 = time.Now()
		d.startCursorX, d.startCursorY = cursorPos()
		d.startWinX, d.startWinY = windowXY(hwnd)
		d.lastX, d.lastY = d.startWinX, d.startWinY
		// 照旧转发：图标/搜索框/按钮的按下态、聚焦都要走 mygo 那一套。
		return d.call(hwnd, msg, wp, lp)

	case wmMouseMove:
		if d.active {
			d.dragTo(hwnd)
			return 0
		}
		if d.pending {
			cx, cy := cursorPos()
			dx, dy := cx-d.startCursorX, cy-d.startCursorY
			if dx > dragThreshold || dx < -dragThreshold || dy > dragThreshold || dy < -dragThreshold {
				if r, _, _ := procSetCapture.Call(hwnd); r == 0 {
					return 0
				}
				d.active = true
				d.measureClientOffset(hwnd)
				d.dragTo(hwnd)
				return 0
			}
		}
		return d.call(hwnd, msg, wp, lp)

	case wmLButtonUp:
		was := d.active
		d.pending = false
		d.active = false
		procRelCapture.Call()
		if was {
			if n := d.frames; n > 1 {
				if el := time.Since(d.t0); el > 0 {
					log.Printf("拖动: 自绘拖动 %d 帧 / %dms = %.0f fps",
						n, el.Milliseconds(), float64(n)/el.Seconds())
				}
			}
			d.app.panelGeometryChanged()
			// 不转发 up：否则 mygo 会把"拖动终点"下的那个图标当点击。
			return 0
		}
		return d.call(hwnd, msg, wp, lp)

	case wmCancelMode:
		if d.active || d.pending {
			d.pending = false
			d.active = false
			procRelCapture.Call()
		}
		return d.call(hwnd, msg, wp, lp)
	}
	return d.call(hwnd, msg, wp, lp)
}

// measureClientOffset 记下客户区相对窗口左上角的偏移（见 clientDX 的说明）。
func (d *selfDrag) measureClientOffset(hwnd uintptr) {
	wx, wy, _, _ := win.WindowRectPixels(hwnd)
	cx, cy, _, _ := win.ClientRectPixels(hwnd)
	d.clientDX, d.clientDY = int32(cx-wx), int32(cy-wy)
}

// dragTo 按当前光标把窗口挪到位。顺序是刻意的：
//
//  1. 先按**目标**位置把底图算好并上传（此时窗口还在旧位置，但纹理里已经是新取景）；
//  2. 再 SetWindowPos 挪窗口；
//  3. 立刻 RedrawNow 提交一帧。
//
// DWM 下一次合成看到的是"窗口在新位置 + 内容也是新位置"的同一组数据，所以没有
// 相对错位 —— 这正是系统拖动做不到的地方（它先挪窗口，我们只能追。
func (d *selfDrag) dragTo(hwnd uintptr) {
	cx, cy := cursorPos()
	nx := d.startWinX + (cx - d.startCursorX)
	ny := d.startWinY + (cy - d.startCursorY)
	if nx == d.lastX && ny == d.lastY {
		return
	}
	d.lastX, d.lastY = nx, ny

	d.app.refreshBackingFor(int(nx)+int(d.clientDX), int(ny)+int(d.clientDY), false)
	setWindowXY(hwnd, nx, ny)
	win.RedrawNow(hwnd)
	d.app.followShadow()
	d.app.uiTick.Store(time.Now().UnixNano())
	d.app.dragUntil.Store(time.Now().Add(600 * time.Millisecond).UnixNano())
	d.frames++
}
