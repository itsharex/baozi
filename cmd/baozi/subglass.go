package main

import (
	"image"
	"log"
	"math"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"baozi/internal/win"
)

// subGlass 是"子窗口版"的玻璃：设置窗、文件互传窗各持一份。
//
// 为什么不把面板那套整个抽成基类：面板的玻璃状态（磨砂图、四角月牙、拖动糊透、
// 平移快路径、窗口区域、拖动接管）全都长在 application 上，和"面板这个窗口"绑得
// 很死。硬抽一个基类要把几十个方法、几十个字段一起搬家，风险全落在他最在意的
// 那块面板上。
//
// 这里走另一条：**合成逻辑完全共享**（composeBackingInto），子窗只多持有自己的
// 两样东西 —— 本窗的底图和本窗的阴影层。磨砂图是"屏幕坐标系"的整屏图，窗口只是
// 在上面取一块，没必要每窗重做；取景基准、明暗判定、桌面兜底色、边框色、DPI 缩放、
// 圆角半径全是应用级共享的，于是子窗和面板看到的就是同一块玻璃。
type subGlass struct {
	app  *application
	win  *mygo.Window
	name string

	back   *ui.Bitmap // 本窗的整窗底图（圆角覆盖率 + 边框 + 逐像素 alpha）
	pix    *image.RGBA
	pw     int
	ph     int
	key    backKey
	keySet bool

	shadow *win.Shadow // 本窗的自绘投影层（layered 窗口，垫在窗口底下）

	// visible 是"这个窗口现在该不该出现在屏幕上"。
	//
	// 不能问系统的 IsWindowVisible：noRedirect 窗口上它会撒谎（实测一直报 false）。
	// 而这个标志决定投影层能不能上屏 —— 投影层是**独立的 layered 窗口**，宿主窗口
	// 藏着的时候它照样能显示：没有这道门，启动时两个隐藏的子窗就会各闪出一个
	// "只有边框和投影的空框"。由 show()/hide() 维护。
	visible bool

	// follower 是挂在顶层窗口上的 WM_MOVE 子类化（见 subfollow.go）。
	follower *shadowFollower

	// drag 是挂在 MyGoSurface 上的自绘拖动（见 subdrag.go）。系统拖动会把
	// 自绘 alpha 的窗口当矩形搬运，四角立刻变直 —— 顶栏必须自己拖。
	drag *subDrag
}

// newSubGlass 给一个已建好的窗口配一份玻璃（含阴影层）。
// 必须在窗口创建之后调：阴影层要窗口句柄才能定位。
func (a *application) newSubGlass(w *mygo.Window, name string) *subGlass {
	g := &subGlass{app: a, win: w, name: name}
	g.shadow = win.NewShadow()
	return g
}

// bitmap 返回这一帧该铺的底图（还没烘过就是 nil）。
func (g *subGlass) bitmap() *ui.Bitmap { return g.back }

// ensureFrost 保证应用级那份磨砂图（frostSmall）是有的。
//
// 关键：**不重新抓屏**，除非连桌面底片都没有。抓屏抓的是"当前屏幕上有什么"，
// 而这时候子窗往往已经显示出来了 —— 重抓就会把自己的窗口糊进玻璃里。
// 面板唤出时那张底片（frostBase）是干净的（那一刻窗口还没上屏），拿它重做一张
// 磨砂图就够了。
func (g *subGlass) ensureFrost() {
	a := g.app
	if a.frostBase == nil {
		a.refreshGlass()
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
	s := a.glassScale()
	r := int(math.Round(float64(glassRadius) * float64(s)))
	k := backKey{cw: w, ch: h, r: r, screenGen: a.screenGen, dark: a.deskDark}
	if !force && g.keySet && g.key == k {
		return
	}
	pw, ph := w+2*backingPad, h+2*backingPad
	t0 := time.Now()
	pix := a.composeBackingInto(g.pix, cx, cy, pw, ph, r, backingPad, image.Rectangle{})
	if pix == nil {
		return
	}
	g.pix = pix
	if g.back != nil && g.pw == pw && g.ph == ph {
		g.back.Update(pix)
	} else {
		g.back = ui.NewBitmap(pix)
	}
	g.pw, g.ph = pw, ph
	g.key = k
	g.keySet = true
	if d := time.Since(t0); d > 8*time.Millisecond {
		log.Printf("玻璃: %s 底图 %.1fms（%dx%d）", g.name, float64(d.Microseconds())/1000, w, h)
	}
}

// drawBitmap 把底图铺满客户区（按物理像素 1:1，和面板同一套摆法）。
func (g *subGlass) drawBitmap(c *ui.Context) {
	if g.back == nil || g.pw <= 0 || g.ph <= 0 {
		return
	}
	s := g.app.glassScale()
	pad := 1 / s
	ui.Image(c, g.back).Absolute().Left(-pad).Top(-pad).
		Size(float32(g.pw)/s, float32(g.ph)/s)
}

// shadowShown 报投影层当前是不是已经在屏上（view 里用来判断要不要补一次）。
func (g *subGlass) shadowShown() bool {
	return g.shadow != nil && g.shadow.Shown()
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
	g.visible = true
}

// hide 标记"窗口收起来了"：投影层必须一起收，否则它会自己留在屏幕上。
func (g *subGlass) hide() {
	g.visible = false
	g.hideShadow()
}

// refreshShadow 让投影层按当前窗口位置尺寸重画一次并显示。
func (g *subGlass) refreshShadow() {
	a := g.app
	if g.shadow == nil || g.win == nil {
		return
	}
	// 窗口还没（或已经不再）在屏幕上：投影层不许露面。见 visible 的注释。
	if !g.visible {
		g.shadow.Hide()
		return
	}
	// 句柄这时肯定有了：顺手把两个子类化挂上（都幂等）。
	installShadowFollower(g.win, g)
	installSubDrag(g.win, g)
	hwnd := g.win.NativeHandle()
	if hwnd == 0 {
		return
	}
	x, y, w, h := win.WindowRectPixels(hwnd)
	if w <= 0 || h <= 0 {
		return
	}
	if !a.shadowEnabled() {
		g.shadow.Hide()
		return
	}
	s := a.glassScale()
	r := int(math.Round(float64(glassRadius) * float64(s)))
	gl := a.pickGlassColors(x, y, w, h)
	desk := uint32(a.deskColor.R)<<16 | uint32(a.deskColor.G)<<8 | uint32(a.deskColor.B)
	ok := g.shadow.Place(hwnd, win.ShadowSpec{
		X: x, Y: y, W: w, H: h,
		Radius:       r,
		Margin:       a.shadowMargin(),
		Desk:         desk,
		Glass:        gl[0],
		GlassCorners: [4]uint32{gl[0], gl[1], gl[2], gl[3]},
		EdgeGlass:    a.pickEdgeGlass(x, y, w, h),
		BorderW:      0,
		// 弧和弧外都在底图里，这一层只负责窗口外那一圈投影。
		SkipPatch: true,
	})
	if !ok {
		log.Printf("阴影: %s 图层布局失败", g.name)
	}
	g.shadow.Show()
}

// hideShadow 收起投影层（窗口藏起来或正在缩放时）。
func (g *subGlass) hideShadow() {
	if g.shadow != nil {
		g.shadow.Hide()
	}
}

// followShadow 窗口只是挪了位置：只挪投影层，不重画位图。
func (g *subGlass) followShadow() {
	if g.shadow == nil || !g.shadow.Shown() || g.win == nil {
		return
	}
	hwnd := g.win.NativeHandle()
	if hwnd == 0 {
		return
	}
	x, y, _, _ := win.WindowRectPixels(hwnd)
	g.shadow.Nudge(x, y, int(math.Round(float64(win.ShadowMargin)*float64(g.app.glassScale()))))
}
