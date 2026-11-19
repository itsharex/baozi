package main

import (
	"log"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"baozi/internal/win"
)

// subGlass 是"子窗口版"的玻璃：设置窗、文件互传窗各持一份。
//
// 底图、投影、刷新这些**和面板共用同一份实现**（pane.go 的 paneState）：
// 形状键、平移快路径、四条带重算、投影门控、投影 z 带全都只有一份代码。
// 子窗真正独有的只剩三样：干净快照的时机（needCleanSnap）、WM_MOVE 跟随、
// 表面上的自绘拖动。
//
// 历史上这里是 second copy（pix/pw/ph/key/keySet/shadow/visible/hostShown/
// dragCx… 各来一遍），于是"抽出来时漏一处"就漏出一次观感问题：backKey 漏 fade
// （拖动不糊）、shiftTo 漏带状重绘（贴边一条亮线）、投影漏 hostShown 门控
// （关掉后留空框）、投影 z 带没跟宿主（空心框）。现在这些结构上不可能再分叉。
//
// 磨砂图仍然是"屏幕坐标系"的整屏图，窗口只是从上面取一块：取景基准、明暗判定、
// 桌面兜底色、边框色、DPI 缩放、圆角半径全是应用级共享的，所以子窗和面板看到的
// 就是同一块玻璃。
type subGlass struct {
	app  *application
	win  *mygo.Window
	name string

	// pane 是与启动面板**同一个类型**的玻璃状态（pane.go 的 paneState）：
	// 底图、合成缓冲、形状键、取景原点、投影层、visible/hostShown 全在里面，
	// 刷新与投影的实现也只有那一份。
	//
	// 这里只留子窗真正独有的东西：干净快照的时机、WM_MOVE 跟随、表面拖动、
	// 挂载重试计数。以前这一整块状态是 second copy（pix/pw/ph/key/keySet/shadow/
	// visible/hostShown/dragCx…），抽的时候漏一处就漏出一次观感问题。
	pane paneState

	// needCleanSnap 表示下一次 refresh 要先抓一张"不含自家窗口"的桌面快照。
	// 抓的时候把启动面板设成 WDA_EXCLUDEFROMCAPTURE，底片里才不会印着它。
	needCleanSnap bool

	// follower 是挂在顶层窗口上的 WM_MOVE 子类化（见 subfollow.go）。
	follower *shadowFollower

	// drag 是挂在 MyGoSurface 上的自绘拖动（见 subdrag.go）。系统拖动会把
	// 自绘 alpha 的窗口当矩形搬运，四角立刻变直 —— 顶栏必须自己拖。
	drag *subDrag

	// dragRetry 是"还没挂上，隔一拍再试"的剩余次数（surface 建得比 Show 晚）。
	dragRetry int
}

// newSubGlass 给一个已建好的窗口配一份玻璃（含阴影层）。
// 必须在窗口创建之后调：阴影层要窗口句柄才能定位。
func (a *application) newSubGlass(w *mygo.Window, name string) *subGlass {
	g := &subGlass{app: a, win: w, name: name, dragRetry: 8}
	g.pane = paneState{ap: a, name: name, win: w, shadow: win.NewShadow()}
	// 子窗和面板的两处差异，只有这两个回调不一样：
	//   · gate：这个窗该不该在屏上（visible 是"想显示"，hostShown 是"系统那边确实显示着"）
	//   · spec：投影位图怎么给参数 —— 子窗只要窗口外那一圈投影，角和弧都在底图里
	g.pane.gate = func() bool { return g.pane.visible && g.pane.hostShown }
	g.pane.spec = func(hwnd uintptr, x, y, w, h, r int) win.ShadowSpec {
		gl := a.pickGlassColors(x, y, w, h)
		return win.ShadowSpec{
			X: x, Y: y, W: w, H: h,
			Radius:       r,
			Margin:       a.shadowMargin(),
			Desk:         uint32(a.deskColor.R)<<16 | uint32(a.deskColor.G)<<8 | uint32(a.deskColor.B),
			Glass:        gl[0],
			GlassCorners: [4]uint32{gl[0], gl[1], gl[2], gl[3]},
			EdgeGlass:    a.pickEdgeGlass(x, y, w, h),
			BorderW:      0,
			// 弧和弧外都在底图里，这一层只负责窗口外那一圈投影。
			SkipPatch: true,
		}
	}
	return g
}

// bitmap 返回这一帧该铺的底图（还没烘过就是 nil）。
func (g *subGlass) bitmap() *ui.Bitmap { return g.pane.bitmap() }

// ensureFrost 保证应用级那份磨砂图（frostSmall）是有的。
//
// 关键：**不重新抓屏**，除非连桌面底片都没有。抓屏抓的是"当前屏幕上有什么"，
// 而这时候子窗往往已经显示出来了 —— 重抓就会把自己的窗口糊进玻璃里。
// 面板唤出时那张底片（frostBase）是干净的（那一刻窗口还没上屏），拿它重做一张
// 磨砂图就够了。
func (g *subGlass) ensureFrost() {
	a := g.app
	if a.frostBase == nil {
		// 抓屏时把自己也让开：这个子窗此刻正在上屏，抓进来就是它自己的影子。
		a.snapshotScreenExcept(g)
		a.pickDeskColor(nil)
	}
	if a.frostSmall == nil {
		a.rebuildFrost()
	}
}

// refresh 按窗口当前位置重烘底图。force 为 true 时不看缓存 key。
func (g *subGlass) refresh(force bool) {
	a := g.app
	if !bakedPanel || g.win == nil {
		return
	}
	g.ensureFrost()
	if a.frostSmall == nil {
		return
	}
	hwnd := g.win.NativeHandle()
	if hwnd == 0 {
		return
	}
	cx, cy, w, h := win.ClientRectPixels(hwnd)
	if w <= 0 || h <= 0 {
		return
	}
	// 第一次上屏（或重新打开）先抓一张不含自家窗口的桌面快照，底片才干净。
	//
	// 排除自家窗口这件事已经由 snapshotScreen 自己做了（见 glass.go 的
	// excludeSelfCapture），这里只管"值不值得重抓"。
	if g.needCleanSnap {
		g.needCleanSnap = false
		a.snapshotScreenExcept(g)
		a.pickDeskColor(nil)
		force = true
	}
	// 剩下的全交给共享实现（pane.go 的 bakeAt）：形状键、平移快路径、四条带、
	// 还原缓冲、日志。子窗不节流（throttle=0）。
	g.pane.bakeAt(cx, cy, force, 0)
}

// shiftTo 把底图按 (cx,cy) 相对上次的位置整体平移。
//
// 这是主面板拖动跟手的关键手法：底图里每个像素记的是它对应的屏幕位置，窗口
// 挪 (dx,dy)，就把像素按 (dx,dy) 搬一遍 —— 1~2ms，比整张重烘（8~10ms）便宜
// 一个量级。一次挪太远、或者形状键变了（比如拖动糊化开关切换），共享实现自己
// 会退回整张重烘。
func (g *subGlass) shiftTo(cx, cy int) {
	g.pane.bakeAt(cx, cy, false, 0)
}

// drawBitmap 把底图铺满客户区（按物理像素 1:1，和面板同一套摆法）。
func (g *subGlass) drawBitmap(c *ui.Context) {
	if g.pane.back == nil || g.pane.pw <= 0 || g.pane.ph <= 0 {
		return
	}
	s := g.app.glassScale()
	pad := 1 / s
	ui.Image(c, g.pane.back).Absolute().Left(-pad).Top(-pad).
		Size(float32(g.pane.pw)/s, float32(g.pane.ph)/s)
}

// shadowShown 报投影层当前是不是已经在屏上（view 里用来判断要不要补一次）。
func (g *subGlass) shadowShown() bool {
	return g.pane.shadowShown()
}

// dipSize 返回客户区的 DIP 尺寸（内容层铺开和裁圆角都用它）。
func (g *subGlass) dipSize() (float32, float32) {
	if g.win == nil {
		return 0, 0
	}
	_, _, w, h := win.ClientRectPixels(g.win.NativeHandle())
	s := g.app.glassScale()
	if s <= 0 {
		s = 1
	}
	return float32(w) / s, float32(h) / s
}

// show 标记"这个窗口要上屏了"：投影层这时才允许显示。
func (g *subGlass) show() {
	g.pane.visible = true
	// 顶层窗口马上要 Show 了；先按"已显示"记上，随后的 WM_SHOWWINDOW 会以系统为准
	// 校一次（万一 Show 没生效，那条消息会把这里改回 false）。
	g.pane.hostShown = true
	// 下一次 refresh 顺便抓一张**干净**的桌面快照（抓的时候把启动面板设成
	// "捕获不可见"）。否则底片里会印着启动面板自己 —— 用户看到的就是
	// "背后明明是我的桌面，玻璃里却是启动面板"。
	g.needCleanSnap = true
	// 主面板在建窗时就调了 win.DisableViewFrame，把 DWM 画的那一圈非客户区关掉。
	// 抽 subGlass 时漏了这一步 —— 于是设置窗/互传窗的边上还留着 DWM 的一圈
	// （深底上是白亮的 1px 直边），用户看到的就是"四角还是直角"。补上。
	if g.win != nil {
		if hwnd := g.win.NativeHandle(); hwnd != 0 {
			win.DisableViewFrame(hwnd)
		}
	}
}

// hide 标记"窗口收起来了"：投影层必须一起收，否则它会自己留在屏幕上。
func (g *subGlass) hide() {
	g.pane.visible = false
	// 拖动状态必须一并清掉：SetCapture 还攥着、active 还是 true 的话，用户
	// 下次打开这个窗，所有按下都被当成"继续拖"，点击全被吞 —— 表现就是
	// "面板打开了，关闭按钮点不动"。
	if g.drag != nil {
		g.drag.cancel()
	}
	g.hideShadow()
	// 再补两拍：第一次关窗口时，宿主还没真正隐藏，投影层可能刚好压在
	// win.Hide() 后面又被打了一次 Show —— 用户看到的就是"关掉之后桌面上
	// 还留着一个空框"（第二次打开再关就正常了）。
	if g.win != nil {
		time.AfterFunc(120*time.Millisecond, func() {
			if !g.pane.visible {
				g.hideShadow()
			}
		})
	}
}

// refreshAppearance 外观（透明度/模糊/圆角/投影强度）改完之后，把这扇窗的玻璃
// 也重来一遍：磨砂图已经按新设置重做过（application.frostGen +1），底图整张重烘，
// 推一帧上屏，投影按新强度/新半径重画。
//
// 没在屏上的窗口不用管：下次 show() 走的是同一套 refresh，本来就会按新设置烘。
func (g *subGlass) refreshAppearance() {
	if !g.pane.visible || !g.pane.hostShown {
		return
	}
	g.refresh(true)
	if g.win != nil {
		g.win.FrameNow()
	}
	g.refreshShadow()
}

// refreshShadow 让投影层按当前窗口位置尺寸重画一次并显示。
func (g *subGlass) refreshShadow() {
	if g.pane.shadow == nil || g.win == nil {
		return
	}
	// 句柄这时肯定有了：顺手把两个子类化挂上（都幂等）。
	installShadowFollower(g.win, g)
	installSubDrag(g.win, g)
	// 门控（visible && hostShown）、投影关掉时收起、Place 参数、Show —— 全在共享实现里。
	// 子窗和面板的差别只有 spec 回调（这里只要窗口外那一圈投影）。
	placed, attempted := g.pane.placeShadow()
	if attempted && !placed {
		log.Printf("阴影: %s 图层布局失败", g.name)
	}
}

// hideShadow 收起投影层（窗口藏起来或正在缩放时）。
func (g *subGlass) hideShadow() {
	g.pane.hideShadow()
}

// followShadow 窗口只是挪了位置：只挪投影层，不重画位图。
func (g *subGlass) followShadow() {
	g.pane.followShadow(true)
}
