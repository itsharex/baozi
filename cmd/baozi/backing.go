package main

// 面板的"底图"：整窗自绘。
//
// 为什么改这条路：
//
//	以前的圆角是三层拼出来的 —— 窗口区域（SetWindowRgn，1 位掩码）切形状、
//	四角月牙（面板自己的位图）补弧外的桌面色、阴影层再补一遍。三层的弧各按各的
//	方式算，交界处于是永远有一条看得见的线：弧和直边对不齐（"扇形的两个直边"）、
//	弧外铺的色和真桌面差一两档（"鼓出去一点"）、区域那道 1 位硬边在弧上留台阶
//	（"毛边"）。用户一句"你总说可以做，可是效果很不让人满意"就是冲着这个来的。
//
//	现在的做法是**一层**：把整个客户区当成一张画来画 ——
//	  · 弧外 → 桌面（取唤出那一刻的半分辨率整屏快照，和窗口外面是同一张图）
//	  · 弧上 → 桌面与玻璃按覆盖率相加（覆盖率来自圆角矩形的有符号距离，1px 过渡）
//	  · 弧内 → 玻璃（取整屏磨砂图）
//	  · 再嵌一条 1px 的边框环，同样带覆盖率
//	画出来是一张不透明位图，直接铺满客户区。没有区域、没有月牙、没有补色，
//	弧的每一像素只有一处来源，边界上不可能再出现两种算法打架。

import (
	"image"
	"image/png"
	"log"
	"math"
	"os"
	"time"

	"github.com/egoist/mygo/ui"

	"baozi/internal/win"
)

// bakedPanel 打开后：面板的四角/边全部由 refreshBacking 烘进底图，
// 窗口区域、四角月牙、直边线、阴影层补角一律不再参与形状。
const bakedPanel = true

// backingThrottle 是底图重算的最小间隔。底图要逐像素重采样（1100×660 这一级
// 大约几毫秒），拖动时鼠标每秒报几百次，不节流会把 UI 线程占满 —— 这正是
// 之前"快速拖动卡死"的来源。阴影层挪位、窗口几何照旧每帧走。
const backingThrottle = 16 * time.Millisecond

// testBacking：BAOZI_BACKTEST=1 时把底图渲染成"形状诊断图"（形状外红、
// 边框环蓝、形状内白），用来在截图上量形状的真实边界。正常运行恒为 0。
var testBacking = func() int {
	if os.Getenv("BAOZI_BACKTEST") == "1" {
		return 1
	}
	return 0
}()

// alphaBacking：形状外面不铺"桌面快照"，而是留成透明（alpha=0），
// 让 DWM 自己把真正的背景透出来 —— 面板浮在任意窗口上时四角都不会
// 露出一块"壁纸残影"。实测交换链支持每像素 alpha（面板压在别的窗口上时，
// 弧外就是那个窗口本身），所以默认打开；BAOZI_BACKALPHA=0 退回旧的铺桌面法。
var alphaBacking = func() bool {
	return os.Getenv("BAOZI_BACKALPHA") != "0"
}()

// rowProbe：诊断用。BAOZI_ROWPROBE=1 时把底图最上面两行涂成洋红/青色，
// 用来判断"屏幕最上面那一行到底是图没铺到，还是有别的东西盖在上面"。
var rowProbe = os.Getenv("BAOZI_ROWPROBE") == "1"

// backTime 打开后，每次拖动平移都把分段耗时打进日志（BAOZI_BACKTIME=1）。
var backTime = os.Getenv("BAOZI_BACKTIME") == "1"

// ringMode 决定四角和四条边那条 1px 线怎么画 —— 用户"能看出来"的那道白弧
// 就是它。三种：
//
//	0  不画（现在的默认）：形状边界只剩覆盖率抗锯齿，玻璃直接过渡到背景。
//	   用户对着 iced 的角说"最好做到和 iced 一样"，而 iced 就是没有这条线。
//	1  浅色环（老样子）：#D9D9DE 那种暖灰。直边上像"边框"，弧上就是一道白弧，
//	   用户连着几轮指的就是它。
//	2  深色环：比玻璃暗的灰（#68737D），看上去是"描了一圈边"。
//
// BAOZI_RING=1 / 2 切回老画法。三张对比图在 dist\cmp_ring2.png。
var ringMode = func() int {
	switch os.Getenv("BAOZI_RING") {
	case "1":
		return 1
	case "2", "dark":
		return 2
	}
	return 0
}()

// backKey 是底图"形状"的全部输入：尺寸/半径/桌面快照。**位置不算在内** ——
// 窗口只是挪了个地方的话走增量平移（见 refreshBacking），不重烘。
type backKey struct {
	cw, ch    int
	r         int
	screenGen int
	dark      bool // 桌面明暗参与 key：换了一套配色，旧底图不能复用
}

// backingPad 是底图四周比客户区多烘出来的物理像素数（见 refreshBacking 里的
// 注释：盖住 mygo 摆放位图时零点几像素的取整偏差）。
const backingPad = 1

// signed distance 用的圆角矩形参数：中心 + 半宽高（去半径后）+ 半径。
type roundRect struct {
	hx, hy, r float64
}

// glassFallback 是拿不到磨砂图时兜底的"玻璃色"：亮桌面是浅白，暗桌面是深灰。
// 由 frostFromScaled 在判完明暗之后设置，composeBacking 直接读。
var glassFallback = [3]uint8{247, 248, 252}

// dragWhiteBitmap 是**拖动时**盖在玻璃上的那块面板：形状和底图同一套圆角
// 覆盖率（直边一致、弧上抗锯齿），内部实心、外面全透明。
//
// 为什么要有它：面板挪动 (SetWindowPos) 和内容提交 (DComp commit) 分属相邻两帧，
// 快速拖动时那一帧的相对位移有十几像素，眼睛靠"玻璃里的桌面细节"就能看出错位。
// 拖动时把面板变成一块实心，里面没有桌面可比，误差就看不见了 —— 这也是
// Windows 自己拖动毛玻璃窗口时退化成纯色的同一个思路。
//
// 颜色跟着桌面明暗走（亮桌面铺白、暗桌面铺深灰，取 glassFallback），否则
// 暗色壁纸上一拖就闪出一块白板。
func (a *application) dragWhiteBitmap() *ui.Bitmap {
	if a.backPix == nil {
		return nil
	}
	cw, ch := a.backPix.Rect.Dx(), a.backPix.Rect.Dy()
	r := float64(int(math.Round(float64(glassRadius) * float64(a.glassScale()))))
	if a.dragWhite != nil && a.dragWhiteW == cw && a.dragWhiteH == ch &&
		a.dragWhiteR == r && a.dragWhiteDark == a.deskDark {
		return a.dragWhite
	}
	rr := newRoundRect(cw, ch, backingPad, r)
	img := image.NewRGBA(image.Rect(0, 0, cw, ch))
	hx, hy := float64(cw)/2, float64(ch)/2
	for y := 0; y < ch; y++ {
		row := img.Pix[y*img.Stride:]
		py := float64(y) + 0.5 - hy
		for x := 0; x < cw; x++ {
			// 一像素宽的覆盖率过渡带：和底图的弧用的是同一个 sd，切点不会错开。
			c := 0.5 - rr.sd(float64(x)+0.5-hx, py)
			if c <= 0 {
				continue
			}
			if c > 1 {
				c = 1
			}
			i := x * 4
			row[i], row[i+1], row[i+2] = glassFallback[0], glassFallback[1], glassFallback[2]
			row[i+3] = uint8(c * 255)
		}
	}
	if a.dragWhite != nil && a.dragWhiteW == cw && a.dragWhiteH == ch {
		a.dragWhite.Update(img)
	} else {
		a.dragWhite = ui.NewBitmap(img)
	}
	a.dragWhiteW, a.dragWhiteH, a.dragWhiteR = cw, ch, r
	a.dragWhiteDark = a.deskDark
	return a.dragWhite
}

func newRoundRect(cw, ch, inset int, radius float64) roundRect {
	w := float64(cw) - 2*float64(inset)
	h := float64(ch) - 2*float64(inset)
	// 半径**不减 inset**：形状只是整体往里挪了 inset 像素，四个圆角还是同一个
	// 半径（圆心也跟着挪）。减掉 inset 会让角比直边多收 1 像素，弧和直边的切点
	// 就对不上，接缝处露出一小段台阶。
	r := radius
	if r < 0 {
		r = 0
	}
	return roundRect{hx: w/2 - r, hy: h/2 - r, r: r}
}

// sd 返回点到圆角矩形边界的有符号距离（负=内部），坐标原点是矩形外框的中心。
func (rr roundRect) sd(x, y float64) float64 {
	// 手写 min/max 而不是 math.Min/math.Max：后者要照 NaN 语义走一遍
	// 比较+分支（profile 里 archMin/archMax 占了一半的时间），而这里的
	// 参数都是有限数，不会出现 NaN。一行像素里 sd 要算几十万次（4×4 超
	// 采样更是 16 次/像素），省下来的是每帧几毫秒。
	qx := x
	if qx < 0 {
		qx = -qx
	}
	qx -= rr.hx
	qy := y
	if qy < 0 {
		qy = -qy
	}
	qy -= rr.hy
	in := qx
	if qy > in {
		in = qy
	}
	if in > 0 {
		in = 0
	}
	ox := qx
	if ox < 0 {
		ox = 0
	}
	oy := qy
	if oy < 0 {
		oy = 0
	}
	return in + math.Sqrt(ox*ox+oy*oy) - rr.r
}

// refreshBacking 重算底图。force 为 true 时不理会节流间隔。
//
//	调用时机：view 里（每帧布局都会经过），以及 finishResize / 唤出之后。
func (a *application) refreshBacking(force bool) {
	if !bakedPanel || a.frost == nil || a.panel == nil {
		return
	}
	hwnd := a.panel.NativeHandle()
	if hwnd == 0 {
		return
	}
	cx, cy, _, _ := win.ClientRectPixels(hwnd)
	a.refreshBackingFor(cx, cy, force)
}

// refreshBackingFor 按**指定**的客户区原点（物理像素）烘底图。
//
// 自绘拖动（dragwin.go 的 dragTo）要在窗口挪动**之前**就把内容按目标位置算好：
// 先更新纹理、再 SetWindowPos、再 Present，DWM 才会把"新位置"和"新内容"当同一帧。
// 所以这里必须能把坐标从外面传进来，而不是像 refreshBacking 那样现取窗口位置。
func (a *application) refreshBackingFor(cx, cy int, force bool) {
	if !bakedPanel || a.frost == nil || a.panel == nil {
		return
	}
	hwnd := a.panel.NativeHandle()
	if hwnd == 0 {
		return
	}
	_, _, w, h := win.ClientRectPixels(hwnd)
	if w <= 0 || h <= 0 {
		return
	}
	s := a.glassScale()
	r := int(math.Round(float64(glassRadius) * float64(s)))
	k := backKey{cw: w, ch: h, r: r, screenGen: a.screenGen, dark: a.deskDark}

	// 形状没变、只是窗口挪了地方：**不整张重烘**，把内容整体平移到位就行。
	//
	// 这是拖动跟手的关键。磨砂图和桌面快照都是"整屏固定"的图，底图里每个像素
	// 记的是它对应的屏幕位置；窗口挪 (dx,dy)，就把旧图的像素按 (dx,dy) 搬一遍，
	// 每个像素照样落在正确的桌面位置上 —— 除了贴着窗边的那一圈：形状边界的
	// 覆盖率、弧外那段 alpha=0、边框环，都要按新坐标重算。
	// 所以每帧只重算四条带（约 11 万像素，整张是 74 万），1~2ms；整张重烘要 8~10ms。
	if a.back != nil && a.backKeySet && a.backKeyVal == k && a.backPix != nil {
		dx, dy := cx-a.backCx, cy-a.backCy
		if dx == 0 && dy == 0 {
			return
		}
		pw0, ph0 := a.backPix.Rect.Dx(), a.backPix.Rect.Dy()
		band := r + 4
		// 位移超过四成尺寸时，重算面积跟整张差不多，直接走整张。
		if dx > -(pw0*2/5) && dx < pw0*2/5 && dy > -(ph0*2/5) && dy < ph0*2/5 {
			t0 := time.Now()
			shiftPix(a.backPix, dx, dy)
			t1 := time.Now()
			a.repaintBackingBands(cx, cy, pw0, ph0, r, band, dx, dy)
			t2 := time.Now()
			a.backCx, a.backCy = cx, cy
			a.back.Update(a.backPix)
			t3 := time.Now()
			if backTime {
				log.Printf("玻璃: 平移 shift %.2fms rebake %.2fms update %.2fms（%+d,%+d）",
					float64(t1.Sub(t0).Microseconds())/1000,
					float64(t2.Sub(t1).Microseconds())/1000,
					float64(t3.Sub(t2).Microseconds())/1000, dx, dy)
			}
			if d := time.Since(t0); d > 8*time.Millisecond {
				log.Printf("玻璃: 底图平移 %.1fms（%+d,%+d）",
					float64(d.Microseconds())/1000, dx, dy)
			}
			return
		}
	}
	if !force {
		now := time.Now()
		if !a.lastBackAt.IsZero() && now.Sub(a.lastBackAt) < backingThrottle {
			return
		}
	}
	a.lastBackAt = time.Now()

	// 四周各多烘 1 个物理像素（backingPad），再把这张图整个往外铺 1 像素：
	// mygo 把这张图摆下来的时候会带零点几像素的取整偏差 —— 客户区最上面那一行
	// 曾经就整行没被盖住，露的是 mygo 清屏的纯白（BAOZI_ROWPROBE=1 的探针实测：
	// 图往外铺一像素后，屏幕第 0 行才轮到图自己的第 1 行）。多烘一圈、外面再
	// 压一像素，无论它往上还是往下取整，客户区都被图盖满，不会再露白。
	// 四个角只有一个来源：这张图。弧外的 alpha=0 由 DWM 透出真背景，
	// 弧上是覆盖率相加，没有第二层来跟它打架。
	pw, ph := w+2*backingPad, h+2*backingPad
	t0 := time.Now()
	pix := a.composeBacking(cx, cy, pw, ph, r, backingPad, image.Rectangle{})
	if pix == nil {
		return
	}
	if a.back != nil && a.backPixW == pw && a.backPixH == ph {
		a.back.Update(pix)
	} else {
		a.back = ui.NewBitmap(pix)
		a.backPixW, a.backPixH = pw, ph
	}
	a.backCx, a.backCy = cx, cy
	a.backKeySet = true
	a.backKeyVal = k
	if d := time.Since(t0); d > 8*time.Millisecond {
		log.Printf("玻璃: 底图 %.1fms（%dx%d）", float64(d.Microseconds())/1000, w, h)
	}
	// 诊断：把这一帧烘好的底图导成 PNG（面板是 noRedirect 合成的，屏幕截图
	// 会被别的窗口干扰，量弧的覆盖率只能看这份原始数据）。
	//   BAOZI_BACKDUMP=<路径>  每次重烘覆盖一次
	if p := os.Getenv("BAOZI_BACKDUMP"); p != "" {
		if f, err := os.Create(p); err == nil {
			_ = png.Encode(f, pix)
			_ = f.Close()
		}
	}
}

// shiftPix 把底图里的内容整体挪 (dx,dy)：窗口往右下挪 dx,dy，底图里的内容
// 就要在窗口坐标系里往左上挪同样的量，所以新图的 (x,y) 取旧图的 (x+dx,y+dy)。
// 搬不进来的那一条（新露出来的桌面）是旧数据，调用方随后会把它重算掉。
// 逐行 memcpy，读取的行/列还没被写过，所以不需要临时缓冲。
func shiftPix(img *image.RGBA, dx, dy int) {
	w, h := img.Rect.Dx(), img.Rect.Dy()
	if w == 0 || h == 0 || (dx == 0 && dy == 0) {
		return
	}
	if dx <= -w || dx >= w || dy <= -h || dy >= h {
		return
	}
	row := w * 4
	switch {
	case dy > 0:
		for y := 0; y < h-dy; y++ {
			copy(img.Pix[y*row:(y+1)*row], img.Pix[(y+dy)*row:(y+dy+1)*row])
		}
	case dy < 0:
		for y := h - 1; y >= -dy; y-- {
			copy(img.Pix[y*row:(y+1)*row], img.Pix[(y+dy)*row:(y+dy+1)*row])
		}
	}
	switch {
	case dx > 0:
		for y := 0; y < h; y++ {
			r := img.Pix[y*row : (y+1)*row]
			copy(r[:row-dx*4], r[dx*4:])
		}
	case dx < 0:
		for y := 0; y < h; y++ {
			r := img.Pix[y*row : (y+1)*row]
			copy(r[-dx*4:], r[:row+dx*4])
		}
	}
}

// repaintBackingBands 只重算底图里"必须按新坐标画"的那几块。
//
// 需要重算的像素只有两类来源：
//
//	· 形状边界附近那 1~2 像素（抗锯齿过渡、边框环、弧外那段 alpha=0）；
//	· 老图那一圈"按边界覆盖率算出来的"像素被整体搬进了内部（成脏像素），
//	  以及平移之后新露出来的那条桌面 —— 两张都只需 |d| 宽。
//
// 所以边条的宽度是 3+|d|，不是 r+4；四角那 r 见方的一块（弧 + 弧外三角）
// 单独补。整张是 74 万像素，这里约 4~5 万，一次平移 2ms 上下。
func (a *application) repaintBackingBands(cx, cy, cw, ch, r, band, dx, dy int) {
	_ = band
	abs := func(v int) int {
		if v < 0 {
			return -v
		}
		return v
	}
	ex, ey := 3+abs(dx), 3+abs(dy)
	if ex*2 >= cw || ey*2 >= ch || ex >= cw/2 || ey >= ch/2 {
		a.composeBacking(cx, cy, cw, ch, r, backingPad, image.Rectangle{})
		return
	}
	// 上下横条（全宽）、左右竖条（避开横条已经盖过的两头）
	a.composeBacking(cx, cy, cw, ch, r, backingPad, image.Rect(0, 0, cw, ey))
	a.composeBacking(cx, cy, cw, ch, r, backingPad, image.Rect(0, ch-ey, cw, ch))
	a.composeBacking(cx, cy, cw, ch, r, backingPad, image.Rect(0, ey, ex, ch-ey))
	a.composeBacking(cx, cy, cw, ch, r, backingPad, image.Rect(cw-ex, ey, cw, ch-ey))
	// 四个角块：弧、弧外三角，**以及老角块被搬进来落下的那块** —— 老角块
	// 在旧坐标里是 [0,cr)×[0,cr)，内容出现在新坐标为止减 (dx,dy) 的位置，
	// 所以角落要重算的并集是 cr+|d| 见方，不只是 cr。
	cr := r + 2
	cq, chq := cr+abs(dx), cr+abs(dy)
	if cq >= cw || chq >= ch {
		a.composeBacking(cx, cy, cw, ch, r, backingPad, image.Rectangle{})
		return
	}
	a.composeBacking(cx, cy, cw, ch, r, backingPad, image.Rect(0, 0, cq, chq))
	a.composeBacking(cx, cy, cw, ch, r, backingPad, image.Rect(cw-cq, 0, cw, chq))
	a.composeBacking(cx, cy, cw, ch, r, backingPad, image.Rect(0, ch-chq, cq, ch))
	a.composeBacking(cx, cy, cw, ch, r, backingPad, image.Rect(cw-cq, ch-chq, cw, ch))
}

// composeBacking 返回客户区大小的位图：桌面 + 玻璃 + 边框环，按覆盖率相加。
// cw/ch 是**含内边距**的尺寸，inset 是四周多出来的物理像素数（backingPad），
// 形状本身仍然铺在 [inset, inset+cw-2inset] 这一段，也就是客户的真实范围。
func (a *application) composeBacking(cx, cy, cw, ch, r, inset int, region image.Rectangle) *image.RGBA {
	if a.backPix == nil || a.backPix.Rect.Dx() != cw || a.backPix.Rect.Dy() != ch {
		a.backPix = image.NewRGBA(image.Rect(0, 0, cw, ch))
	}
	if region.Empty() {
		region = image.Rect(0, 0, cw, ch)
	} else {
		region = region.Intersect(image.Rect(0, 0, cw, ch))
	}
	out := a.backPix
	// 所有像素都会被写到（形状外铺桌面），不用清零。

	br, bg, bb := uint8(217), uint8(217), uint8(222)
	if a.borderSet {
		br, bg, bb = a.borderColor.R, a.borderColor.G, a.borderColor.B
	}
	if ringMode == 2 {
		// 深色环：比玻璃暗一档，看上去像"描了边"，不像"发着光的白弧"。
		br, bg, bb = 104, 115, 125
	}

	// 边框宽度按物理像素：1 DIP × scale，至少 1 像素。
	bw := int(math.Round(float64(a.glassScale())))
	if bw < 1 {
		bw = 1
	}

	outer := newRoundRect(cw, ch, inset, float64(r))

	halfW := float64(cw) / 2
	halfH := float64(ch) / 2

	// 玻璃 / 桌面采样用的原点（相对客户区左上角的屏幕坐标）。
	// 多出来的那一圈内边距要把坐标系挪回去：位图的 (inset,inset) 才是客户区的 (0,0)。
	gox := cx - a.screenX - inset
	goy := cy - a.screenY - inset
	dox := cx - a.deskHalfX - inset
	doy := cy - a.deskHalfY - inset
	step := glassStep
	if step < 1 {
		step = 1
	}
	dstep := a.deskHalfStep
	if dstep < 1 {
		dstep = 1
	}

	// 玻璃采样：横向的源列号和权重只跟 x 有关，先算一遍存下来；纵向每行算一次。
	// 逐像素就只剩 4 次查表和几次乘加（早先每像素都走一遍通用双线性，
	// 1100×660 一次 15~18ms，拖动时节流都救不回来）。
	gimg := a.frostSmall
	var gx0i, gx1i []int
	var gfxs []float64
	// 内部（离弧很远、覆盖率必为 1 的那一大片）走**最近邻**：磨砂图本身已经在
	// 1/8 分辨率上糊过（Blur 半径 3 ≈ 屏幕上 24 像素），放大时双线性和最近邻的
	// 差别是 8 像素块里的一两档色差，看不出来；但主循环占了 95% 的像素，省下
	// 每像素三次乘加就是省下一半的合成时间（拖动时这一趟每 50ms 就跑一次）。
	var gxn []int
	if gimg != nil && gimg.Rect.Dx() > 0 {
		gxn = make([]int, cw)
		w := gimg.Rect.Dx()
		for x := 0; x < cw; x++ {
			i := int(float64(gox+x) / float64(step))
			if i < 0 {
				i = 0
			} else if i > w-1 {
				i = w - 1
			}
			gxn[x] = i
		}
	}
	if gimg != nil {
		gx0i = make([]int, cw)
		gx1i = make([]int, cw)
		gfxs = make([]float64, cw)
		gw, gh := gimg.Rect.Dx(), gimg.Rect.Dy()
		for x := 0; x < cw; x++ {
			f := float64(gox+x) / float64(step)
			i0 := int(f)
			fx := f - float64(i0)
			i1 := i0 + 1
			if i0 < 0 {
				i0, i1, fx = 0, 0, 0
			} else if i0 > gw-1 {
				i0, i1, fx = gw-1, gw-1, 0
			}
			if i1 > gw-1 {
				i1 = gw - 1
			}
			if i1 < 0 {
				i1 = 0
			}
			if gh <= 0 {
				i0, i1 = 0, 0
			}
			gx0i[x], gx1i[x], gfxs[x] = i0, i1, fx
		}
		if gh <= 0 {
			gimg = nil
		}
	}

	for y := region.Min.Y; y < region.Max.Y; y++ {
		py := float64(y) + 0.5 - halfH
		// 这一行的纵向采样（磨砂图的源行号 + 权重）
		var gRow0, gRow1 []uint8
		gfy := 0.0
		if gimg != nil {
			f := float64(goy+y) / float64(step)
			j0 := int(f)
			gfy = f - float64(j0)
			j1 := j0 + 1
			gh := gimg.Rect.Dy()
			if j0 < 0 {
				j0, j1, gfy = 0, 0, 0
			} else if j0 > gh-1 {
				j0, j1, gfy = gh-1, gh-1, 0
			}
			if j1 > gh-1 {
				j1 = gh - 1
			}
			if j1 < 0 {
				j1 = 0
			}
			gRow0 = gimg.Pix[(j0-gimg.Rect.Min.Y)*gimg.Stride:]
			gRow1 = gimg.Pix[(j1-gimg.Rect.Min.Y)*gimg.Stride:]
		}
		gbx := gimg.Rect.Min.X
		// 这一行的最近邻源行（内部快路径用）与双线性的两行（边缘带用）。
		var gNearRow []uint8
		if gimg != nil {
			gj := 0
			if gxn != nil {
				gj = int(float64(goy+y) / float64(step))
				gh := gimg.Rect.Dy()
				if gj < 0 {
					gj = 0
				} else if gj > gh-1 {
					gj = gh - 1
				}
			}
			gNearRow = gimg.Pix[(gj-gimg.Rect.Min.Y)*gimg.Stride:]
		}
		// 这个像素要不要精确的形状覆盖率？只有两类要：最外 3 圈（抗锯齿过渡、
		// 边框环、弧外那段 alpha=0）和四角的 r 见方块（弧 + 弧外三角）。
		// 其余一律最近邻 —— 磨砂图本来就在 1/8 分辨率上糊过，放大时双线性和
		// 最近邻的差别是 8 像素块里的一两档色差，看不出来。
		//
		// 判定只看 (x,y)、与窗口位置无关 —— 拖动时"平移内容"能成立全靠这一点：
		// 同一个像素，在旧图和搬过来的像素里必须走同一条采样算法，否则平移值和
		// 重新采样值对不上，四角那圈会看出一道分界线。
		edgeY := y < 3 || y >= ch-3
		cornerY := y < r+2 || y >= ch-r-2
		for x := region.Min.X; x < region.Max.X; x++ {
			o := out.PixOffset(x, y)
			px := float64(x) + 0.5 - halfW

			edgeX := x < 3 || x >= cw-3
			cornerX := x < r+2 || x >= cw-r-2
			exact := edgeX || edgeY || (cornerX && cornerY)
			sdOut := -2.0
			if exact {
				sdOut = outer.sd(px, py)
			}
			if !exact || sdOut < -1.2 {
				// 覆盖率必为 1（离边界超过一个像素）：最近邻取玻璃色就是全部工作。
				if gNearRow != nil {
					o0 := (gxn[x] - gbx) * 4
					out.Pix[o+0] = gNearRow[o0]
					out.Pix[o+1] = gNearRow[o0+1]
					out.Pix[o+2] = gNearRow[o0+2]
					out.Pix[o+3] = 255
				} else {
					out.Pix[o+0], out.Pix[o+1], out.Pix[o+2], out.Pix[o+3] = glassFallback[0], glassFallback[1], glassFallback[2], 255
				}
				continue
			}

			var gr, gg, gb float64
			if gRow0 != nil {
				o0 := (gx0i[x] - gbx) * 4
				o1 := (gx1i[x] - gbx) * 4
				fx := gfxs[x]
				rr := float64(gRow0[o0])*(1-fx) + float64(gRow0[o1])*fx
				r2 := float64(gRow1[o0])*(1-fx) + float64(gRow1[o1])*fx
				gr = rr*(1-gfy) + r2*gfy
				rr = float64(gRow0[o0+1])*(1-fx) + float64(gRow0[o1+1])*fx
				r2 = float64(gRow1[o0+1])*(1-fx) + float64(gRow1[o1+1])*fx
				gg = rr*(1-gfy) + r2*gfy
				rr = float64(gRow0[o0+2])*(1-fx) + float64(gRow0[o1+2])*fx
				r2 = float64(gRow1[o0+2])*(1-fx) + float64(gRow1[o1+2])*fx
				gb = rr*(1-gfy) + r2*gfy
			} else {
				gr, gg, gb = float64(glassFallback[0]), float64(glassFallback[1]), float64(glassFallback[2])
			}

			// 覆盖率。
			//
			// 以前是"距离边界 0.5px 以内线性过渡"——那只是**直边**的正确抗锯齿模型：
			// 直边上一行像素的覆盖率确实只跟一个坐标有关，线性就准。但弧上不是：
			// 弧在一个像素里转了十几度，像素中心到边界的那点距离推不出真正被盖住了
			// 多少面积，于是每个像素差一两档、排成肉眼能数出来的台阶（"毛边"）。
			//
			// 现在贴着边界的那一圈（|sd| < 1.2px）改成 4×4 超采样：把像素切成 16 个
			// 子格，各自判内/外，覆盖率是"数出来的面积"而不是"中心的距离"——这就是
			// iced/cairo 那套覆盖率抗锯齿的做法，弧上也是 16 档过渡，不再有台阶。
			const sub = 4
			var cOut, ringC float64
			switch {
			case sdOut > 1.2:
				cOut = 0
			default:
				o16 := 0
				r16 := 0
				for sy := 0; sy < sub; sy++ {
					oy := (float64(sy)+0.5)/sub - 0.5
					yy := py + oy
					for sx := 0; sx < sub; sx++ {
						xx := px + (float64(sx)+0.5)/sub - 0.5
						d := outer.sd(xx, yy)
						if d < 0 {
							o16++
							// 边框环单独数：只数"边界往里 1px"那一窄条。
							//
							// 以前用 cOut - cIn 当环的覆盖率 —— 那是"两个面积之差"，
							// 直边上正好 1，弧上却被抗锯齿摊薄了：一条 45° 的弧，
							// 1px 宽的带子在每个像素里只占七成，于是直边的线是实的、
							// 弧上的线是虚的，看起来就是"圆角的地方没有边框了"。
							// 现在直接数这条带子压住了多少子格，弧上也能到满格。
							if d > -1 {
								r16++
							}
						}
					}
				}
				cOut = float64(o16) / (sub * sub)
				ringC = float64(r16) / (sub * sub)
			}
			if cOut <= 0 {
				// 完全在形状外面（四角三角）：铺桌面像素，和窗口外面的桌面同源。
				if testBacking != 0 {
					out.Pix[o+0] = 255
					out.Pix[o+1] = 0
					out.Pix[o+2] = 0
					out.Pix[o+3] = 255
					continue
				}
				if alphaBacking {
					// 形状外面留透明：让真正的背景透出来，四角不再靠"壁纸残影"。
					out.Pix[o+0] = 0
					out.Pix[o+1] = 0
					out.Pix[o+2] = 0
					out.Pix[o+3] = 0
					continue
				}
				dr, dg, db := a.deskAt(float64(dox+x), float64(doy+y), dstep)
				out.Pix[o+0] = clamp8(dr)
				out.Pix[o+1] = clamp8(dg)
				out.Pix[o+2] = clamp8(db)
				out.Pix[o+3] = 255
				continue
			}

			// 边框带：宽度 1px、贴着形状边界、覆盖率是数出来的（见上面 r16），
			// 所以直边和弧上是同一条实线，弧上不会再"淡掉""断掉"。
			ring := ringC
			if ring > cOut {
				ring = cOut
			}
			if ringMode == 0 {
				// 不描线：这一像素整块按覆盖率当玻璃算，边界就只剩抗锯齿本身。
				ring = 0
			}
			wGlass := cOut - ring
			wDesk := 1 - cOut

			if testBacking != 0 {
				// 诊断配色（BAOZI_BACKTEST=1）：不管真实颜色，只标出三个区域，
				// 截图上量"形状到底铺到哪儿"最省事。
				rrr, rgg, rbb := 255, 255, 255
				if ring > 0.5 {
					rrr, rgg, rbb = 0, 0, 255
				}
				out.Pix[o+0] = uint8(rrr)
				out.Pix[o+1] = uint8(rgg)
				out.Pix[o+2] = uint8(rbb)
				out.Pix[o+3] = 255
				continue
			}

			var dr, dg, db float64
			if wDesk > 0.002 && !alphaBacking {
				dr, dg, db = a.deskAt(float64(dox+x), float64(doy+y), dstep)
			} else {
				dr, dg, db = gr, gg, gb
			}

			if alphaBacking {
				// 交给 DWM 混合：颜色按 alpha 预乘（ring + glass 的权重加起来正好
				// 就是覆盖率 cOut），形状外面 alpha=0，透出真正的背景。
				out.Pix[o+0] = clamp8(ring*float64(br) + wGlass*gr)
				out.Pix[o+1] = clamp8(ring*float64(bg) + wGlass*gg)
				out.Pix[o+2] = clamp8(ring*float64(bb) + wGlass*gb)
				out.Pix[o+3] = clamp8(cOut * 255)
				continue
			}

			rr := wDesk*dr + ring*float64(br) + wGlass*gr
			rgc := wDesk*dg + ring*float64(bg) + wGlass*gg
			rb := wDesk*db + ring*float64(bb) + wGlass*gb
			out.Pix[o+0] = clamp8(rr)
			out.Pix[o+1] = clamp8(rgc)
			out.Pix[o+2] = clamp8(rb)
			out.Pix[o+3] = 255
		}
	}
	if rowProbe && region.Min.X == 0 && region.Min.Y == 0 {
		// 最上面四行涂成洋红/青/黄/绿：屏幕上看到哪一行，就说明这张图的第几行
		// 落在了客户区第 0 行（判断 mygo 摆放位图时到底偏了几个像素）。
		probe := [4][3]uint8{{255, 0, 255}, {0, 255, 255}, {255, 255, 0}, {0, 255, 0}}
		for row := 0; row < 4 && row < ch; row++ {
			for x := 0; x < cw; x++ {
				o := out.PixOffset(x, row)
				out.Pix[o+0] = probe[row][0]
				out.Pix[o+1] = probe[row][1]
				out.Pix[o+2] = probe[row][2]
				out.Pix[o+3] = 255
			}
		}
	}
	return out
}

// deskAt 在唤出那一刻的半分辨率整屏快照上双线性采样（屏幕坐标，未缩放）。
func (a *application) deskAt(sx, sy float64, step int) (float64, float64, float64) {
	if a.deskHalf == nil {
		c := a.deskColor
		return float64(c.R), float64(c.G), float64(c.B)
	}
	return sampleBilinear(a.deskHalf, sx/float64(step), sy/float64(step))
}

// sampleBilinear 在 img 上按 (x,y) 双线性取样，出界时钳位到边缘。
// 这个函数在底图循环里被调用几十万次，所以**不分配任何东西**（早先版本
// 用一个 []*float64 循环写三个通道，每像素一次堆分配，1100×660 一帧就是
// 72 万次 —— 实测 22ms，其中大半是 GC 和分配）。
func sampleBilinear(img *image.RGBA, x, y float64) (float64, float64, float64) {
	w := img.Rect.Dx()
	h := img.Rect.Dy()
	if w == 0 || h == 0 {
		return 247, 248, 252
	}
	x0 := int(x)
	y0 := int(y)
	if x < 0 {
		x0 = -1
	}
	if y < 0 {
		y0 = -1
	}
	fx := x - float64(x0)
	fy := y - float64(y0)
	x1, y1 := x0+1, y0+1
	if x0 < 0 {
		x0, x1, fx = 0, 0, 0
	} else if x0 > w-1 {
		x0, x1, fx = w-1, w-1, 0
	}
	if y0 < 0 {
		y0, y1, fy = 0, 0, 0
	} else if y0 > h-1 {
		y0, y1, fy = h-1, h-1, 0
	}
	if x1 > w-1 {
		x1 = w - 1
	}
	if y1 > h-1 {
		y1 = h - 1
	}
	row0 := img.Pix[(y0-img.Rect.Min.Y)*img.Stride:]
	row1 := img.Pix[(y1-img.Rect.Min.Y)*img.Stride:]
	o00 := (x0 - img.Rect.Min.X) * 4
	o10 := (x1 - img.Rect.Min.X) * 4
	a := float64(row0[o00])
	bb := float64(row0[o10])
	c := float64(row1[o00])
	d := float64(row1[o10])
	r := (a*(1-fx)+bb*fx)*(1-fy) + (c*(1-fx)+d*fx)*fy
	a = float64(row0[o00+1])
	bb = float64(row0[o10+1])
	c = float64(row1[o00+1])
	d = float64(row1[o10+1])
	g := (a*(1-fx)+bb*fx)*(1-fy) + (c*(1-fx)+d*fx)*fy
	a = float64(row0[o00+2])
	bb = float64(row0[o10+2])
	c = float64(row1[o00+2])
	d = float64(row1[o10+2])
	bl := (a*(1-fx)+bb*fx)*(1-fy) + (c*(1-fx)+d*fx)*fy
	return r, g, bl
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func clamp8(v float64) uint8 {
	if v <= 0 {
		return 0
	}
	if v >= 255 {
		return 255
	}
	return uint8(v + 0.5)
}
