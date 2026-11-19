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

	// 顶栏（含标题、副标题）的高度，DIP。下面是内容区，不接管按下。
	// 实测顶栏（两行标题）在 1.25 缩放下约 90 物理像素 = 72 DIP，要留够，
	// 不然在副标题那一行按下就不接管，退回系统拖动 —— 拖动时背景不动。
	subTopBarDip = 78

	// 顶栏右侧「✕ 关闭」占的宽度，DIP。
	//
	// 这个数必须**大于按钮真实宽度**：按钮在 1.25 缩放下从 x≈1050 到 1140
	// （约 112 DIP）。之前给 60 DIP，把手区一直伸到 1075 —— 按钮左半边落在
	// 把手区里，按下就被我们吃掉、单击永远传不到 mygo，用户点关闭毫无反应。
	//
	// 后来又发现"刚好够"也不行：按钮宽度会随字号/文案变，把手区边界只差十几像素，
	// 换个 DPI 就又压回按钮上。现在直接留到 200 DIP（1.25 缩放下 250px），
	// 换算出来永远盖住整个按钮，代价只是顶栏右边这一截不能当把手 —— 顶栏够宽，
	// 拖窗口从来不缺地方。
	subCloseAreaDip = 200

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

	// faded 记"这一次拖动已经切到糊透的玻璃了"。面板那边叫 selfDrag.faded，
	// 作用一样：第一帧切一次、松手退一次，见 glass.go 的 setDragFadeSub。
	faded bool

	startCurX, startCurY int32
	startWinX, startWinY int32
}

func installSubDrag(w *mygo.Window, g *subGlass) {
	if w == nil || g == nil {
		return
	}
	host := w.NativeHandle()
	if host == 0 {
		g.retrySubDrag(w)
		return
	}
	// 先摸 surface：窗口藏过再显示，句柄有可能整个换新（mygo 重建渲染面）。
	// 以前是"挂过就不再挂"，于是我们这层子类化还留在那个已经作废的句柄上，
	// 新 surface 上没有把手 —— 拖动迟钝甚至点不动，看着像窗口"半死"。
	surf := findMyGoSurface(host)
	if surf == 0 {
		// mygo 的 MyGoSurface 是 Show 之后才 createWindow 的，时序偶尔差一拍。
		// 摸空就隔 50ms 再试几拍 —— 主面板当年就栽在这上面：日志只留一句
		// "没挂上"，用户那边就是"又拖不动了"，然后又退回系统拖动，四角继续变直。
		g.retrySubDrag(w)
		return
	}
	if g.drag != nil {
		if g.drag.host == host && g.drag.hwnd == surf {
			// 已经挂上了。每次重新挂一次，把自己提到子类化链的**最前面**：
			// SetWindowSubclass 是后进先出，mygo 在渲染/重设时可能又挂了自己的那层，
			// 挡在我们前面把按下吃掉（表现就是"我们的拖动根本没收到消息"）。
			g.drag.rearm()
			return
		}
		// 句柄换了：旧的作废，重新挂一份。
		g.drag.cancel()
		g.drag = nil
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
	// 按下时只重烘一张"清晰"的底图 —— 糊化要等**越过阈值的第一帧**才切，
	// 和主面板 dragTo() 的第 0 步（`if !d.faded { setDragFade(true) }`）一致：
	// 只是点一下顶栏不该看到玻璃闪一下。
	d.glass.refresh(true)
	log.Printf("子窗拖动: %s begin（光标 %d,%d 窗口 %d,%d）",
		d.glass.name, d.startCurX, d.startCurY, d.startWinX, d.startWinY)
}

// inTopBar 判断这次按下是不是落在"顶栏空白区"（能当拖动把手的地方）。
// cancel 强制结束一次拖动。窗口被隐藏/关闭时必须调用。
//
// 不收拾的话，SetCapture 还攥在我们手里、active 还留着 true：用户再次打开
// 面板时，每一次按下都被当成"还在拖"，点击全被吞掉 —— 表现就是"面板打开了
// 却点不动关闭"，窗口还会莫名其妙跟着鼠标走。
func (d *subDrag) cancel() {
	d.active = false
	d.pending = false
	if d.hwnd != 0 {
		procRelCapture.Call()
	}
	// 糊化必须一起退掉：dragFade 是**应用级**开关（磨砂图是共享的），
	// 窗口在半路被藏起来时不清，面板和其他子窗会一直停在糊透的玻璃上。
	if d.faded {
		d.faded = false
		d.glass.app.setDragFadeSub(nil, false)
	}
}

// finish 结束一次拖动并把画面收尾：捕获、糊化、底图、投影各归各位。
//
// 和 cancel 的区别：cancel 是"窗口都要没了，别画了"；finish 是"手松了，把这一
// 帧收拾好"。分开是因为在窗口已经隐藏时重烘底图纯属白花 8~10ms。
func (d *subDrag) finish() {
	d.active = false
	d.pending = false
	if d.hwnd != 0 {
		procRelCapture.Call()
	}
	if d.faded {
		d.faded = false
		// setDragFadeSub(false) 会重烘这一块子窗的底图（清晰版），
		// 然后我们自己推一帧 —— 面板 up 里也是 refreshBacking + Invalidate 同一手。
		d.glass.app.setDragFadeSub(d.glass, false)
	} else {
		d.glass.refresh(true)
	}
	if d.win != nil {
		d.win.FrameNow()
	}
	d.glass.refreshShadow()
}

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
		// 一次新按下。要是上一次拖动的"抬手"根本没送到（在窗口外松手、被系统
		// 抢走了消息），active/pending 还留着，这一按就会被当成"还在拖"，
		// 于是点击全被吞掉 —— 用户看到的就是"窗口在，但怎么点都没反应"。
		// 按下的这一刻无论旧状态是什么都该作废，先清干净再判断。
		if d.active || d.pending {
			d.cancel()
			d.glass.app.setDragFadeSub(nil, false)
		}
		if d.inTopBar(x, y) {
			d.begin()
			// 吃掉这次按下：不让 mygo 再走它自己的（系统）拖动。
			return 0
		}

	case wmLButtonUp:
		if d.active {
			d.finish()
			return 0
		}
		if d.pending {
			// 没到阈值：当普通点击。仍然要把糊化退掉。
			d.pending = false
			d.glass.app.setDragFadeSub(d.glass, false)
			break
		}

	case wmMouseMove:
		if !d.pending && !d.active {
			break
		}
		// 抬手的消息丢了（在窗口外松手、或者被别的窗口抢走）：左键其实早就不
		// 按着了。异步键态是这里唯一可信的"现在到底按没按"，一旦发现松了就地
		// 收尾，别把 active/pending 留给下一次点击。
		if !leftButtonDown() {
			d.finish()
			break
		}
		var p point
		procGetCursor.Call(uintptr(unsafe.Pointer(&p)))
		dx := p.X - d.startCurX
		dy := p.Y - d.startCurY
		if !d.active {
			if abs32(dx) < dragThreshold && abs32(dy) < dragThreshold {
				return 0
			}
			d.active = true
			procSetCapture.Call(d.hwnd)
			// 第 0 步，和面板 dragTo() **一字不差地同一个顺序**：
			// 先切糊化并重烘这一块底图，再挪窗口、再平移内容、最后推帧。
			// 糊化必须在 shiftTo 之前完成，否则这一帧平移的是清晰图，
			// 下一帧才糊 —— 用户看到的就是"拖起来先清了一下再糊"。
			//
			// 漏掉这一步就是用户这次报的"拖动时没有像启动面板那样变模糊，
			// 而是保持背景"：玻璃一直挂着那张清晰的磨砂底片。
			if !d.faded {
				d.faded = true
				d.glass.app.setDragFadeSub(d.glass, true)
			}
			log.Printf("子窗拖动: %s 开始移动（糊化）", d.glass.name)
		}
		// 每帧确认捕获还在自己手上：窗口一挪，系统/别的路径就可能把它拿走，
		// 拿走了就再也收不到 mousemove，拖动看着就是"动一下就停住"。
		procSetCapture.Call(d.hwnd)
		nx := d.startWinX + dx
		ny := d.startWinY + dy
		procSetWinPos.Call(d.host, 0, uintptr(nx), uintptr(ny), 0, 0,
			swpNoSize|swpNoZOrder|swpNoActivate)
		d.glass.followShadow()
		// 和主面板 dragTo() 同一手：把底图按位移整体平移（1~2ms），再推一帧上屏。
		// 底图是按屏幕坐标采样的，平移后每个像素仍落在正确的屏幕上 —— 看起来
		// 就是"背景跟着窗口走"，不是死色。
		d.glass.shiftTo(int(nx), int(ny))
		if d.win != nil {
			d.win.FrameNow()
		}
		// 拖动中不要交给 mygo：它的 hover/点击逻辑会把光标当成"在顶栏上"。
		return 0

	case wmCancelMode:
		// 和面板 selfDrag 的处理一致：系统把这一轮鼠标交互作废了（弹模态、
		// Alt+Tab、UAC……），捕获和 active/pending 就地清掉，再原样传下去。
		if d.active || d.pending {
			d.cancel()
		}

	case wmCaptureChanged:
		// 捕获丢了。如果左键还按着，那就是被别的窗口/系统抢走了（窗口移动、
		// 提最前、mygo 的重设都会来这一下），必须抢回来继续拖 —— 否则"拖一下
		// 就停在那里"，用户那边就是"又拖不动了"、而且是拖一点点就卡住的怪样子。
		if d.active || d.pending {
			if leftButtonDown() {
				procSetCapture.Call(d.hwnd)
				break
			}
			d.finish()
		}
	}
	r, _, _ := procDefSubclass.Call(hwnd, msg, wparam, lparam, id, data)
	return r
}
