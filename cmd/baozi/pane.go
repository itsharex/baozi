package main

// pane.go —— 三块玻璃（启动面板、设置窗、文件互传窗）共用的一份状态与一套算法。
//
// 之前是两套：面板的状态散在 application 上（back/backPix/backPixW/backKeyVal/
// backCx/shadow…），子窗另有一份 subGlass（pix/pw/ph/key/keySet/shadow/visible…）。
// 两边方法名一样、字段名不一样，于是"抽出来的时候漏一处"就漏出一整个观感问题：
// backKey 少了个 fade、shiftTo 少了带状重绘、投影门控少看了 hostShown、
// 投影 z 带没跟着宿主走（空心框）。每一次都是同一个病根：同一件事写了两遍。
//
// 现在两块都持有同一个 paneState，底图和投影的决策只有这一份实现。每个窗只保留
// 自己真正不同的那点东西，用两个回调注入：
//
//	· spec：投影位图怎么给参数（面板要四角月牙 + 边框弧色，子窗只要外圈投影）
//	· gate：投影该不该露面（子窗要 visible+hostShown 双门控，面板不看这个，
//	  它由 hidePanel 显式收）
//
// 窗口本体、窗口名、底图与投影层的持有关系全部在这一个结构里。
import (
	"image"
	"image/png"
	"log"
	"math"
	"os"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"baozi/internal/win"
)

// paneState 是一块玻璃窗的全部"共享状态"。三块玻璃各持有一个，类型相同、字段相同。
type paneState struct {
	ap   *application
	name string
	win  *mygo.Window

	// 底图：整窗自绘那一张（含 backingPad 的内边距），pix 是复用的合成缓冲。
	back   *ui.Bitmap
	pix    *image.RGBA
	pw, ph int
	key    backKey
	keySet bool
	// cx/cy 是这张底图"取景"时客户区左上角的屏幕坐标。窗口只是挪地方时，
	// 底图不重烘，只把内容平移 (cx-backCx, cy-backCy)。
	cx, cy int
	// bakAt 是上一次整张重烘的时刻（节流用）。
	bakAt time.Time

	// shadow 是这一层自己的自绘投影（一个独立的 layered 窗口）。
	shadow *win.Shadow

	// visible 是"这个窗口现在该不该出现在屏幕上"；hostShown 记的是系统那边的事实
	// （顶层窗口收到过 WM_SHOWWINDOW(SW_SHOW)）。
	//
	// 不能问系统的 IsWindowVisible：noRedirect 合成窗口上它会撒谎。而这个标志
	// 决定投影层能不能上屏 —— 投影层是独立窗口，宿主藏着的时候它照样能显示。
	// 面板不靠这两个标志（它的开关是 panelUp 与 hidePanel 的显式调用），
	// 所以面板那份的 gate 是 nil。
	visible   bool
	hostShown bool

	// 两个差异点，见文件头。
	spec func(hwnd uintptr, x, y, w, h, r int) win.ShadowSpec
	gate func() bool
}

// hwnd 返回宿主窗口句柄（0 表示还没有）。
func (p *paneState) hwnd() uintptr {
	if p == nil || p.win == nil {
		return 0
	}
	return p.win.NativeHandle()
}

// bitmap 返回当前该铺的那张底图（还没烘过就是 nil）。
func (p *paneState) bitmap() *ui.Bitmap { return p.back }

// shadowShown 报投影层当前是不是已经在屏上。
func (p *paneState) shadowShown() bool {
	return p.shadow != nil && p.shadow.Shown()
}

// shouldShowShadow 是投影层的门：没有 gate 就一律允许（面板）。
func (p *paneState) shouldShowShadow() bool {
	if p == nil || p.shadow == nil {
		return false
	}
	if p.gate != nil && !p.gate() {
		return false
	}
	return true
}

// keyAt 是底图"形状"的全部输入。**位置不算在内** —— 窗口只是挪地方走增量平移。
//
// 这个 key 必须三块玻璃逐字段一样：少一个字段就意味着"那个条件变了却还吃着旧图"。
// 历史上漏过 fade（拖动糊化切了不重烘，于是拖动时背景不变），所以它现在只有这一份。
func (p *paneState) keyAt(w, h, r int) backKey {
	return backKey{
		cw: w, ch: h, r: r,
		screenGen: p.ap.screenGen,
		dark:      p.ap.deskDark,
		fade:      p.ap.dragFade,
	}
}

// bakeAt 按**指定**的客户区原点（物理像素）准备底图。
//
// 三种结果：
//   - 形状没变、只是位置变了 → 平移内容 + 只重算贴边的带（1~2ms，拖动跟手靠它）
//   - 形状变了（或没有旧图）→ 整张重烘（8~10ms）
//   - 被节流挡掉 → 什么都不做
//
// throttle 为 0 表示不节流（子窗用）。返回 true 表示这一帧确实动过底图。
func (p *paneState) bakeAt(cx, cy int, force bool, throttle time.Duration) bool {
	if p == nil || p.win == nil || p.ap == nil {
		return false
	}
	hwnd := p.hwnd()
	if hwnd == 0 {
		return false
	}
	_, _, w, h := win.ClientRectPixels(hwnd)
	if w <= 0 || h <= 0 {
		return false
	}
	s := p.ap.glassScale()
	r := int(math.Round(float64(glassRadius) * float64(s)))
	k := p.keyAt(w, h, r)

	if p.back != nil && p.keySet && p.key == k && p.pix != nil {
		dx, dy := cx-p.cx, cy-p.cy
		if dx == 0 && dy == 0 {
			return false
		}
		pw0, ph0 := p.pix.Rect.Dx(), p.pix.Rect.Dy()
		// 位移超过四成尺寸时，重算面积跟整张差不多，直接走整张。
		if dx > -(pw0*2/5) && dx < pw0*2/5 && dy > -(ph0*2/5) && dy < ph0*2/5 {
			t0 := time.Now()
			shiftPix(p.pix, dx, dy)
			t1 := time.Now()
			// 平移之后**必须**补那四条带和四角（抗锯齿过渡、边框环、弧外 alpha=0
			// 都按旧坐标算过，搬进内部就成脏像素，贴边一圈会跟着窗口跑）。
			p.pix = p.ap.repaintBandsInto(p.pix, cx, cy, pw0, ph0, r, backingPad, dx, dy)
			t2 := time.Now()
			p.cx, p.cy = cx, cy
			p.back.Update(p.pix)
			t3 := time.Now()
			if backTime {
				log.Printf("玻璃: %s平移 shift %.2fms rebake %.2fms update %.2fms（%+d,%+d）",
					p.prefix(),
					float64(t1.Sub(t0).Microseconds())/1000,
					float64(t2.Sub(t1).Microseconds())/1000,
					float64(t3.Sub(t2).Microseconds())/1000, dx, dy)
			}
			if d := time.Since(t0); d > 8*time.Millisecond {
				log.Printf("玻璃: %s底图平移 %.1fms（%+d,%+d）",
					p.prefix(), float64(d.Microseconds())/1000, dx, dy)
			}
			return true
		}
	}
	if !force && throttle > 0 {
		now := time.Now()
		if !p.bakAt.IsZero() && now.Sub(p.bakAt) < throttle {
			return false
		}
	}
	p.bakAt = time.Now()

	// 四周各多烘 1 个物理像素（backingPad），再把这张图整个往外铺 1 像素：
	// mygo 把图摆下来时会带零点几像素的取整偏差，客户区最上面那一行曾经就
	// 整行没被盖住，露的是 mygo 清屏的纯白。
	pw, ph := w+2*backingPad, h+2*backingPad
	t0 := time.Now()
	pix := p.ap.composeBackingInto(p.pix, cx, cy, pw, ph, r, backingPad, image.Rectangle{})
	if pix == nil {
		return false
	}
	p.pix = pix
	if p.back != nil && p.pw == pw && p.ph == ph {
		// 尺寸没变只能 Update 像素：ui.Image 渲染一次就把纹理缓存住了，
		// 换对象屏上不会变（见 main.go 的 cornerSize 注释，同一个坑）。
		p.back.Update(pix)
	} else {
		p.back = ui.NewBitmap(pix)
	}
	p.pw, p.ph = pw, ph
	p.cx, p.cy = cx, cy
	p.key = k
	p.keySet = true
	if d := time.Since(t0); d > 8*time.Millisecond {
		log.Printf("玻璃: %s底图 %.1fms（%dx%d）", p.prefix(), float64(d.Microseconds())/1000, w, h)
	}
	// 诊断：把这一帧烘好的底图导成 PNG（面板是 noRedirect 合成的，屏幕截图会被
	// 别的窗口干扰，量弧的覆盖率只能看这份原始数据）。
	//   BAOZI_BACKDUMP=<路径>  每次重烘覆盖一次
	if path := os.Getenv("BAOZI_BACKDUMP"); path != "" {
		if f, err := os.Create(path); err == nil {
			_ = png.Encode(f, pix)
			_ = f.Close()
		}
	}
	return true
}

// prefix 给日志用：面板那份名字为空，保持原来的 "玻璃: 底图 …" 字样；
// 子窗是 "玻璃: 设置 底图 …"。
func (p *paneState) prefix() string {
	if p.name == "" {
		return ""
	}
	return p.name + " "
}

// placeShadow 让投影层按当前窗口位置尺寸重画一次并显示。
//
// 返回 (是否画成功, 是否真的尝试过)。attempted=false 表示被门控挡掉或投影关了
// —— 那种情况不该记"布局失败"。
func (p *paneState) placeShadow() (placed, attempted bool) {
	if p == nil || p.shadow == nil || p.win == nil || p.spec == nil {
		return false, false
	}
	if !p.shouldShowShadow() {
		p.shadow.Hide()
		return false, false
	}
	hwnd := p.hwnd()
	if hwnd == 0 {
		return false, false
	}
	x, y, w, h := win.WindowRectPixels(hwnd)
	if w <= 0 || h <= 0 {
		return false, false
	}
	if !p.ap.shadowEnabled() {
		// 设置页把投影关掉了（或强度调到 0）：整层不显示。弧和弧外都在底图里，
		// 关掉不影响圆角，只是窗外没有影子。
		p.shadow.Hide()
		return false, false
	}
	s := p.ap.glassScale()
	r := int(math.Round(float64(glassRadius) * float64(s)))
	ok := p.shadow.Place(hwnd, p.spec(hwnd, x, y, w, h, r))
	p.shadow.Show()
	return ok, true
}

// hideShadow 收起投影层（窗口藏起来、或者正在缩放时）。
func (p *paneState) hideShadow() {
	if p != nil && p.shadow != nil {
		p.shadow.Hide()
	}
}

// followShadow 窗口只是挪了位置：只挪投影，不重画位图。
//
// light 表示用 Nudge（只 SetWindowPos，不重传位图）。拖动期间每帧 UpdateLayeredWindow
// 整张提交是延迟的来源，所以拖动走 light、程序自己摆窗口走 Move。
//
// 窗口已经不在了就别跟着挪 —— 投影层是独立窗口，挪过去就是一块没人认领的空框。
func (p *paneState) followShadow(light bool) {
	if p == nil || p.shadow == nil || p.win == nil {
		return
	}
	if !p.shouldShowShadow() {
		p.shadow.Hide()
		return
	}
	if !p.shadow.Shown() {
		return
	}
	hwnd := p.hwnd()
	if hwnd == 0 {
		return
	}
	// 用窗口矩形（不是客户区）：圆角区域、阴影位图的参照矩形都是窗口矩形，
	// 混着用会在拖动时错开那圈边框带的宽度，阴影相对面板跳一下。
	x, y, _, _ := win.WindowRectPixels(hwnd)
	margin := p.ap.shadowMargin()
	if light {
		p.shadow.Nudge(x, y, margin)
	} else {
		// 拖动中尺寸不变，宽度按上次建好的位图来（由 Place 记着）。
		p.shadow.Move(x, y, margin)
	}
}
