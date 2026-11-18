package main

import (
	"image"
	"log"
	"math"
	"os"
	"time"

	"github.com/egoist/mygo/ui"

	"baozi/internal/win"
)

// 面板的玻璃底。
//
// 做法：
//  1. 唤出**之前**抓一次全屏快照（那一刻窗口还没上屏，抓到的是"没有面板的世界"），
//     缩到 1/3、模糊、压白，存成一张"整屏磨砂图"；
//  2. 面板里就放这张图，尺寸铺满整个虚拟屏、位置给负值 —— 窗口等于在图上"取景"：
//     面板拖到哪，窗口里露出的就是那块桌面。拖动/改尺寸只改图的偏移，
//     不用重裁、重模糊、重新上传纹理，所以跟手。
//
// 为什么不能用系统磨砂：面板窗口带 WS_EX_NOREDIRECTIONBITMAP（mygo 的原生 UI
// 必然如此，见 window.go:600 的 popts.Surface = opts.Content != nil），
// Win10 的 DWM accent 对这类窗口只返回成功、不画任何东西。

// glassStep 是整屏磨砂图的降采样倍数。玻璃本来就是糊的，GDI 在拷屏的同时
// 就缩到 1/8（2560×1440 → 320×180），只分配两百多 KB —— 既省内存又省模糊时间，
// 显示时放大回去照样是均匀的一片糊。
const glassStep = 8

// deskHalfStep 是"给四角弧外当底色"用的半分辨率快照的降采样倍数。
// 用 2 而不是 8：8 只有 320×180，裁一个 52px 的角块只剩 6 个源像素，放大出来是色块；
// 2 是 1280×720（3.7 MB），面板藏起来时就释放，不值得为它省那点内存。
const deskHalfStep = 2

// glassRadius 是窗口区域的圆角半径（DIP），也是内容那条弧的半径。
//
// 这块窗口带 WS_EX_NOREDIRECTIONBITMAP，圆角以外的像素透不出桌面、DWM 填的是
// 纯白（实测 #FFFFFF），所以"窗口形状"只能靠 SetWindowRgn 给。
// 区域同时决定 DWM 阴影的形状 —— 这也是阴影能跟着圆角走的唯一办法。
//
// 24 DIP（30 物理像素）的弧。16 DIP 时弧只有 20px，一条直边要走近一千像素、
// 到最后 20px 才拐弯，看上去就是"直角被削掉一个小角"，而不是圆润的角
// （用户截图里 71×58 那块右下角：竖线直着下来到 y=24 才起弯、横线直着过来到
// x=38 才起弯，中间就 17~18px 的弧）。26 DIP 那版是十年前的问题——弧太靠里，
// 那条边线看着像画进了边框内部，其实是当时的整圆 bug（弧的判定没限定在角象限，
// 面板内部又画了四分之三圈），那个已经修掉了，所以半径可以放回来。
// UseCornerBitmaps 决定四角那圈"月牙"还要不要画。
//
// 现在是 **false**：四角的弧外那一块已经交给 internal/win/shadow.go 的分层窗口
// 去补（它压在面板之上、不受 SetWindowRgn 裁剪，能把 DWM 填的白整个换掉，还顺带
// 把边框弧画出来）。月牙留在面板里其实干不成这件事 —— 它被区域裁掉，弧外那部分
// 根本画不出去；能被画出去的只有弧里侧那半截，3×3 柔化又把它摊成一条压在玻璃上
// 的浅色弧线，正是用户反复说的"四角内部有一道白色圆弧线"。
const UseCornerBitmaps = false

const glassRadius = 24

// glassClipRadius 是内容（面板底色、边线、四角月牙）的圆角半径（DIP）。
//
// 现在和 glassRadius **相等**：区域和弧同心同半径，弧的端点正好落在区域边界上，
// 四条直边线的起点接上去不会有缝。早期让弧比区域小 2 DIP（区域外全由月牙的
// 不透明部分垫着），那是因为当时月牙铺的是"每角一个采样色"，太厚的环会拉出
// 一条弧形的色差带；现在弧外是逐像素照抄的真实桌面像素（captureCornerPatches），
// 同色是天然的，不需要再让这一圈了 —— 让出去反而让直边线在角上断出一截。
const glassClipRadius = glassRadius

// snapshotScreen 抓一次全屏、做成磨砂图。
// 必须在 Show 之前调用，否则抓到的是面板自己。
// captureCornerPatches 在窗口显示**之前**把四角那一小块桌面逐像素抓下来。
//
// 月牙外侧那一圈铺的是什么色，直接决定圆角看不看得出来。以前每角只存一个
// "角上取色"（deskCorners），壁纸在那块只要有渐变，月牙就成了壁纸上一道亮带：
// 实测左上角弧外铺的是 #79C5F7，紧挨着的真桌面是 #15A4F2，差出一整条弧线，
// 用户说的"还能看出来"就是它。现在直接照抄屏幕上的原始像素，弧外与真桌面
// 1:1 同色，想看出边界也没有色差可看。
//
// 只能在 Show 之前抓：显示之后那块位置就是面板自己了（抓到的是自己的玻璃）。
// 四张 size×size、43×43 一共不到 30 KB。
func (a *application) captureCornerPatches() {
	if a.panel == nil {
		return
	}
	hwnd := a.panel.NativeHandle()
	// 窗口已经可见的时候不要抓：这块窗口是 noRedirect 合成的，它可见时屏幕上
	// 那块就是它自己，抓下来的"桌面色"实际上是面板本身（或者它还没画完时 DWM
	// 填的白）。月牙把它照抄到弧外，四角就出现一块和真桌面不一样的色斑 ——
	// 因为那块是矩形 patch，色斑的内侧会留下一条水平、一条垂直的边，
	// 看上去就是角上凭空多了两直线。宁可用上一次隐藏时抓的那一份。
	if win.IsVisible(hwnd) {
		log.Printf("玻璃: 四角 patch 跳过（窗口已可见，抓到的会是面板自己）")
		return
	}
	s := float64(a.glassScale())
	size := int(math.Round(float64(glassRadius)*s)) + 10
	if size <= 0 {
		return
	}
	cx, cy, cw, ch := win.WindowRectPixels(hwnd)
	if cw < size || ch < size {
		return
	}
	origins := [4][2]int{
		{cx, cy},
		{cx + cw - size, cy},
		{cx, cy + ch - size},
		{cx + cw - size, cy + ch - size},
	}
	kept := 0
	for i := 0; i < 4; i++ {
		img, err := win.CaptureRect(origins[i][0], origins[i][1], size, size)
		if err != nil {
			a.cornerPatches[i] = nil
			log.Printf("玻璃: 四角 patch 抓取失败 角%d %v", i, err)
			continue
		}
		if patchLooksBlank(img) {
			// 兜底：整块近乎纯白基本可以断定抓错了（DWM 白 / 面板自己），
			// 保留上一次那张，不要拿它去铺弧外。
			log.Printf("玻璃: 四角 patch 角%d 近乎纯白，判无效，沿用上一次", i)
			continue
		}
		a.cornerPatches[i] = img
		kept++
	}
	log.Printf("玻璃: 四角 patch %dx%d 已抓 %d/4（窗口 %d,%d %dx%d）", size, size, kept, cx, cy, cw, ch)
}

// cornerPatchesAt 按**当前位置**从半分辨率整屏快照里裁出四角那一小块桌面。
//
// 阴影层拿它铺"弧外那块"（面板被 SetWindowRgn 裁掉、自己画不出去的地方）。
// 用快照而不是现场 BitBlt：面板可见时现场抓到的就是面板自己，抓不了；
// 而唤出前抓的那一份是固定的全屏图，无论面板后来被拖到哪、缩成多大，
// 都能从同一张图里按新坐标重新裁 —— 弧外颜色永远和真实桌面一致。
// 快照没有（抓失败）时退回 captureCornerPatches 存的那一份。
func (a *application) cornerPatchesAt(cx, cy, cw, ch, size int) [4]*image.RGBA {
	shot := a.deskHalf
	if shot == nil || size <= 0 || cw < size || ch < size {
		return a.cornerPatches
	}
	step := a.deskHalfStep
	if step < 1 {
		step = 1
	}
	origins := [4][2]int{
		{cx, cy},
		{cx + cw - size, cy},
		{cx, cy + ch - size},
		{cx + cw - size, cy + ch - size},
	}
	var out [4]*image.RGBA
	for i := 0; i < 4; i++ {
		// 快照坐标：屏幕原点已经在抓图时归一化掉了，所以只减虚拟屏原点。
		sx := float64(origins[i][0]-a.deskHalfX) / float64(step)
		sy := float64(origins[i][1]-a.deskHalfY) / float64(step)
		sw := float64(size) / float64(step)
		sh := float64(size) / float64(step)
		if sx < 0 || sy < 0 || sx+sw > float64(shot.Rect.Dx()) || sy+sh > float64(shot.Rect.Dy()) {
			out[i] = a.cornerPatches[i]
			continue
		}
		out[i] = samplePatch(shot, sx, sy, sw, sh, size)
	}
	return out
}

// samplePatch 从 src 的 (sx,sy,sw,sh) 这块里双线性采样出 size×size 的图。
// 放大倍数是 step（2），双线性够用；要的是"糊得均匀"，不是细节。
func samplePatch(src *image.RGBA, sx, sy, sw, sh float64, size int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		fy := sy + (float64(y)+0.5)*sh/float64(size) - 0.5
		y0 := int(math.Floor(fy))
		wy := fy - float64(y0)
		if y0 < 0 {
			y0, wy = 0, 0
		}
		if y0 >= src.Rect.Dy()-1 {
			y0, wy = src.Rect.Dy()-1, 0
		}
		for x := 0; x < size; x++ {
			fx := sx + (float64(x)+0.5)*sw/float64(size) - 0.5
			x0 := int(math.Floor(fx))
			wx := fx - float64(x0)
			if x0 < 0 {
				x0, wx = 0, 0
			}
			if x0 >= src.Rect.Dx()-1 {
				x0, wx = src.Rect.Dx()-1, 0
			}
			pick := func(px, py int) (float64, float64, float64) {
				o := src.PixOffset(px, py)
				return float64(src.Pix[o]), float64(src.Pix[o+1]), float64(src.Pix[o+2])
			}
			r00, g00, b00 := pick(x0, y0)
			r10, g10, b10 := pick(x0+1, y0)
			r01, g01, b01 := pick(x0, y0+1)
			r11, g11, b11 := pick(x0+1, y0+1)
			mix := func(v00, v10, v01, v11 float64) float64 {
				return (v00*(1-wx)+v10*wx)*(1-wy) + (v01*(1-wx)+v11*wx)*wy
			}
			o := dst.PixOffset(x, y)
			dst.Pix[o] = byte(mix(r00, r10, r01, r11) + 0.5)
			dst.Pix[o+1] = byte(mix(g00, g10, g01, g11) + 0.5)
			dst.Pix[o+2] = byte(mix(b00, b10, b01, b11) + 0.5)
			dst.Pix[o+3] = 255
		}
	}
	return dst
}

// patchLooksBlank 判断抓下来的角块是不是"一片白"。
//
// 采样点里绝大多数都接近 #FFFFFF 时当作无效：真桌面上确实可能有白色窗口，
// 但那种情况下月牙退回 deskCorners 铺的同样是白，结果一样，所以这个判断
// 不会误伤 —— 它只在"抓错人"的时候起作用。
func patchLooksBlank(img *image.RGBA) bool {
	if img == nil || len(img.Pix) == 0 {
		return true
	}
	white, total := 0, 0
	for y := 0; y < img.Rect.Dy(); y += 2 {
		for x := 0; x < img.Rect.Dx(); x += 2 {
			o := (y-img.Rect.Min.Y)*img.Stride + (x-img.Rect.Min.X)*4
			if o+3 >= len(img.Pix) {
				continue
			}
			total++
			if img.Pix[o] > 245 && img.Pix[o+1] > 245 && img.Pix[o+2] > 245 {
				white++
			}
		}
	}
	return total > 0 && white*100/total > 70
}

func (a *application) snapshotScreen() {
	if a.panel == nil {
		return
	}
	// 四角那小块桌面顺手抓下来（此刻窗口还没显示）。
	a.captureCornerPatches()
	// 半分辨率整屏快照：四角"弧外"那块以后就靠它现场裁，面板拖动/缩放都不会过期。
	if vx, vy, vw, vh := win.VirtualScreenRect(); vw > 0 && vh > 0 {
		if half, err := win.CaptureScreenScaled(vx, vy, vw, vh, deskHalfStep); err == nil {
			a.deskHalf = half
			a.deskHalfX, a.deskHalfY, a.deskHalfStep = vx, vy, deskHalfStep
		} else {
			log.Printf("玻璃: 半分辨率快照失败 %v（四角弧外改用现场 patch）", err)
			a.deskHalf = nil
		}
	}
	if win.SystemGlass() {
		// Win11 22H2 起这层材质由 DWM 自己画（mygo 的 vibrancy 走系统 backdrop），
		// 用不上我们这张图。
		a.frost = nil
		return
	}
	x, y, w, h := win.VirtualScreenRect()
	if w <= 0 || h <= 0 {
		return
	}
	small, err := win.CaptureScreenScaled(x, y, w, h, glassStep)
	if err != nil {
		log.Printf("玻璃: 全屏快照失败 %v", err)
		a.frost = nil
		return
	}
	a.screenW, a.screenH = w, h
	a.screenX, a.screenY = x, y
	a.frost = a.frostFromScaled(small)
	if a.frost == nil {
		return
	}
	log.Printf("玻璃: 全屏快照 %dx%d（屏幕原点 %d,%d）→ 磨砂图 %dx%d", w, h, x, y, w/glassStep, h/glassStep)
}

// refreshCorners 取四角的桌面颜色并重画月牙。必须在窗口**已经移到本次唤出的
// 位置之后**调用：取色读的是窗口外侧 2px 的像素，窗口还停在上一处时读到的是
// 上一处的桌面，月牙就铺成一个和旁边对不上的色块（实测左上、左下读到 #E0E0E0，
// 而窗外那片蓝是 #9BC9EA）。
func (a *application) refreshCorners(force bool) {
	s := float64(a.glassScale())
	edge := ui.Hex("#d9d9de") // 浅色主题的 Border，和面板边线同色
	if a.borderSet {
		edge = a.borderColor
	}
	// 圆角那段边框弧现在由阴影层画（面板自己的像素被 SetWindowRgn 裁掉了），
	// 颜色走这里传给它，和四条直边同一色。
	a.cornerEdge = uint32(edge.R)<<16 | uint32(edge.G)<<8 | uint32(edge.B)
	// 四角现在是"静止时交给阴影层、拖动时交给面板自己画"（见 main.go 里画月牙那段）：
	// 静止时四角由自绘阴影层补，最漂亮；可拖动/缩放的那 0.x 秒阴影层是收起的，
	// 四角就只剩窗口区域那道 1 位硬边，弧上是一格一格的台阶，右边和下边更明显
	// （它们正是跟着拖动手柄在变的那两条边，台阶一眼就能看见，用户报的"拖动时
	// 右上右下变方角"就是这个）。所以拖动期间让面板自己把四张月牙画出来。
	//
	// 但只在**唤出**（force）时造这四张图：造完要 panel.Update 整屏重绘一次（40ms
	// 上下），放在"缩放松手"那一帧上就是停手时那一下卡顿。月牙只由半径、四角桌面
	// 色和边线色决定，和面板多大无关，所以一次唤出造一遍够用了。
	if !UseCornerBitmaps && !force {
		return
	}
	a.pickDeskColor(nil)
	a.refineCornerColors()
	arc := int(math.Round(float64(glassClipRadius) * s))
	region := int(math.Round(float64(glassRadius) * s))
	// 四个角位图只由"半径 + 四处桌面色 + 边线色"决定，和面板多大、在哪无关。
	// 参数一模一样时也跳过重造（例如只是把面板原地显示了一次）。
	key := cornerKey{arc: arc, region: region, edge: edge, corners: a.deskCorners}
	if !force && a.cornerKeySet && a.cornerKeySame(key) {
		return
	}
	a.cornerKeyVal, a.cornerKeySet = key, true
	a.buildCornerBitmaps(arc, region, edge)
	// 造好新图必须**马上**重绘一次：不重绘就只是把新 Bitmap 挂上去，屏上仍旧是
	// 上一帧的样子 —— 进程启动后第一次唤出时那里还没有图，四角就整块糊着面板底色
	// （桌面色），要等到某次 resize 才会换过来。Update 只重排重画，不改尺寸，
	// 不会再触发 OnResize。
	if a.panel != nil {
		a.panel.Update(func() {})
	}
}

// refineCornerColors 把四角的桌面色重新取一遍：每个角沿对角线往外取三个距离
// （2、5、8 像素）求平均，替换 pickDeskColor 的单点结果。
//
// 单点取色会恰好踩在壁纸上一处亮斑或暗斑上，那一角的月牙就比旁边的桌面亮一截，
// 看着像角上漏了光。月牙只有 2 像素宽，这点色差也会把弧形边界显出来。
func (a *application) refineCornerColors() {
	if a.panel == nil {
		return
	}
	cx, cy, cw, ch := win.WindowRectPixels(a.panel.NativeHandle())
	if cw <= 0 || ch <= 0 {
		return
	}
	bases := [4][2]int{
		{cx, cy}, {cx + cw, cy}, {cx, cy + ch}, {cx + cw, cy + ch},
	}
	dirs := [4][2]int{{-1, -1}, {1, -1}, {-1, 1}, {1, 1}}
	// 12 个点：同样先查内存里的唤出快照（以前每点一次 GetDC，这一趟就要
	// 40~90ms —— 正是"缩放松手那一下"和"唤出慢半拍"的账）。
	got := make([]win.PointColor, 12)
	for i := 0; i < 4; i++ {
		for k, d := range []int{2, 5, 8} {
			if c, ok := a.colorAt(bases[i][0]+dirs[i][0]*d, bases[i][1]+dirs[i][1]*d); ok {
				got[i*3+k] = c
			}
		}
	}
	for i := 0; i < 4; i++ {
		// 这一个角三个取样里有没有非白的：都没有说明桌面本来就是白的，照收。
		whiteish := func(c win.PointColor) bool {
			return c.OK && c.R > 245 && c.G > 245 && c.B > 245
		}
		allWhite := true
		for k := 0; k < 3; k++ {
			if !whiteish(got[i*3+k]) {
				allWhite = false
				break
			}
		}
		var r, g, b, n int
		for k := 0; k < 3; k++ {
			c := got[i*3+k]
			if !c.OK {
				continue
			}
			if !allWhite && whiteish(c) {
				continue
			}
			r, g, b, n = r+int(c.R), g+int(c.G), b+int(c.B), n+1
		}
		if n == 0 {
			continue
		}
		a.deskCorners[i] = ui.RGB(uint8(r/n), uint8(g/n), uint8(b/n))
		a.deskCornersSet = true
	}
}

// cornerMaskKey 是月牙覆盖率掩码的缓存键。掩码只由这三个量决定：弧半径、
// 区域半径、位图边长（都是物理像素）。桌面色和面板尺寸都不参与 —— 桌面色是
// 合成时才乘上去的，面板尺寸只影响位图摆在哪。
type cornerMaskKey struct {
	arc, region, size int
}

// cornerKey 是"四角位图还能不能沿用上一份"的判据：只有这四样都相同才省掉
// 重造。桌面色放进来是因为它本身就是位图里铺的底色。
type cornerKey struct {
	arc, region int
	edge        ui.Color
	corners     [4]ui.Color
}

// cornerKeySame 比一份新算出来的键和上一份是否一样。
// （没有直接拿结构体比大小：内含数组，写成显式逐项比更好读，也更省事。）
func (a *application) cornerKeySame(k cornerKey) bool {
	v := a.cornerKeyVal
	if v.arc != k.arc || v.region != k.region || v.edge != k.edge {
		return false
	}
	for i := range k.corners {
		if v.corners[i] != k.corners[i] {
			return false
		}
	}
	return true
}

// cornerOuterSide 报一个越界坐标是不是落在"窗口之外"的那一侧。
//
// 四角位图的四个方向不是同一种东西：两条边贴着窗口边（再往外就是桌面/白），
// 另外两条边朝面板内部（再往外就是玻璃）。柔化采样越界时，前者要补不透明、
// 后者要补透明，混为一谈就会在面板内部画出一条方形的桌面色带。
//
// corner：0 左上、1 右上、2 左下、3 右下（与 captureCornerPatches 的 origins 一致）。
func cornerOuterSide(corner, size, x, y int) bool {
	switch corner {
	case 0:
		return x < 0 || y < 0
	case 1:
		return x >= size || y < 0
	case 2:
		return x < 0 || y >= size
	default:
		return x >= size || y >= size
	}
}

// buildCornerBitmaps 画四角的"月牙"：弧外铺该角真实的桌面色，弧内全透明（露出
// 玻璃），弧上靠 4×4 超采样做成柔和过渡、**不描任何边线**。
//
// 为什么非要这么画：这块窗口是 noRedirect 合成的，clip 之外透不出桌面，DWM 填
// 的是纯白（实测四角 #FFFFFF）。所以形状只能靠窗口区域切，而区域是 1 位掩码，
// 弧上全是整像素台阶。台阶两侧同色就看不见，而"桌面色 → 玻璃"这条边界只能
// 自己画。这是 Win10 上把圆角做圆滑的唯一办法 —— 试过的两条捷径都不成：
// DirectComposition 的 CreateRectangleClip 是几何裁剪、边缘照样是硬的；GPU 那套
// ClipInset 把弧画得很圆，但它画在窗口自己的像素里，弧外依旧是一片白。
//
// 每个像素做 8×8 子采样，alpha 就是"子采样里有多少落在弧外"的覆盖率。这给出
// 真正按面积加权的边缘：任何方向的斜边都只过渡 1 个像素，弧与四条边的切点也一样。
//
// arc 是月牙那条弧的半径（物理像素），region 是窗口区域的半径。两者圆心不同
// （区域大一圈），"弧内"和"区域外"有小片重叠 —— 那片必须也铺满桌面色，否则
// DWM 的白从缝里漏出来。
func (a *application) buildCornerBitmaps(arc, region int, edge ui.Color) {
	if arc <= 0 || region <= 0 || !a.deskCornersSet {
		log.Printf("玻璃: 月牙跳过 arc=%d region=%d 取色就绪=%v", arc, region, a.deskCornersSet)
		return
	}
	size := region + 10
	if a.cornerPx > size {
		// 面板改小时沿用大图，画小一点就是。
		size = a.cornerPx
	}
	a.cornerPx = size
	// 每轮都造一张新的 Bitmap，**不能复用旧的再用 Update 换像素**：Update 只改
	// 了 CPU 侧那份像素，屏上的纹理纹丝不动 —— 实测老进程里改了十几轮，四角
	// 显示的始终是进程启动后第一次生成的那张图，也正是"白色圆弧线怎么都消不掉"
	// 的真正原因。一张 43×43、四张不到 8 KB，GPU 上传之后像素还会被释放。
	// 这里**不描边**：曾经沿弧画过一条 2 像素的边线（颜色取面板边框的 #D9D9DE），
	// 那条线紧挨着深色壁纸时会显得比玻璃亮得多，看起来就是四角内部各有一道
	// 白色圆弧。圆角的分界现在完全靠"桌面色 → 玻璃"这个色差本身，弧上只有
	// 超采样给出的柔和过渡。
	// 三件套：4×4 子采样算出"这个像素有多少面积在弧外"，再在 mask 上做两遍 3×3
	// 柔化，最后按 mask 把桌面色铺上去。
	//
	// 为什么要绕这一圈，两步都不能省：
	//   · 只用"到圆弧的有符号距离做 ±2 像素线性渐变"—— 在弧与四条边的**切点**
	//     处，这个距离随坐标几乎不变，过渡被拖成十几个像素长的一条竖带（实测
	//     x=1 列从 y=25 一直混到 y=35），成了用户看到的那枚"亮刺"。
	//   · 只用覆盖率 —— 各方向都只过渡 1 像素，可壁纸是深蓝、玻璃是浅灰，一个
	//     像素跨 165 级亮度，那就是一道硬台阶，还是锯齿（实测 (0,30)→(0,31)，
	//     #15A4F2 直接跳到 #A2D4F5）。
	// 覆盖率保证"形状正确、各向同性"，mask 上的 3×3 柔化再把 1 像素的硬边摊成
	// 约 3 像素的过渡。柔化发生在图像里，跟弧的方向无关，所以切点也不会被拉长。
	const ss = 4
	sub := 1.0 / ss
	// 覆盖率掩码只跟 (arc, region, size) 有关 —— 和桌面色无关，也和面板尺寸无关。
	// 缩放的时候 OnResize 每帧都会走到这里，而每个角要算 size²×16 次 hypot 再
	// 加两遍 3×3 柔化：40×40 就是 10 万次 hypot 加两轮柔化、两块临时切片，四个角
	// 乘四，全砸在 UI 线程上 —— 这就是"缩放时卡卡的"主因（半径从 16 DIP 提到
	// 24 DIP 之后 size 从 30 涨到 40，这一项的代价翻了将近一倍）。
	// 缓存后一帧只剩合成：四个角一共 6400 个像素。
	key := cornerMaskKey{arc: arc, region: region, size: size}
	fresh := a.maskKey != key
	var masks, blur []float64
	for corner := 0; corner < 4; corner++ {
		c := a.deskCorners[corner]
		patch := a.cornerPatches[corner]
		// 1) 覆盖率 mask，只在缓存失效时算。
		if fresh {
			masks = make([]float64, size*size)
			blur = make([]float64, size*size)
		} else {
			masks = a.maskCache[corner]
		}
		for y := 0; fresh && y < size; y++ {
			for x := 0; x < size; x++ {
				// Mirror into the top-left corner's own frame.
				bx, by := float64(x), float64(y)
				if corner == 1 || corner == 3 {
					bx = float64(size - 1 - x)
				}
				if corner == 2 || corner == 3 {
					by = float64(size - 1 - y)
				}
				hit := 0
				for sy := 0; sy < ss; sy++ {
					for sx := 0; sx < ss; sx++ {
						fx := bx + (float64(sx)+0.5)*sub
						fy := by + (float64(sy)+0.5)*sub
						// >0 在弧外（该角这一侧），<0 在弧内（面板这一侧）。
						//
						// 只在角那个象限里算：这里量的是"到圆心的距离"，对整圆
						// 都成立。圆心在 (arc, arc)，半径 arc 的圆在面板内部还有
						// 半个（(2*arc, arc) 就是圆上一点），那些点同样满足 r>arc，
						// 会被当成"弧外"去铺桌面色 —— 面板里就多出一段弯的色带，
						// 看着正是"圆弧跟边框交完界还继续画"。超过圆心的那一侧
						// 属于面板内部，一律是玻璃。
						if fx > float64(arc) || fy > float64(arc) {
							continue
						}
						sd := math.Hypot(float64(arc)-fx, float64(arc)-fy) - float64(arc)
						// 区域外一律铺满桌面色：那一圈是 DWM 填的白。
						if sd > 0 || math.Hypot(float64(region)-fx, float64(region)-fy) > float64(region) {
							hit++
						}
					}
				}
				masks[y*size+x] = float64(hit) / float64(ss*ss)
			}
		}
		// 2) 两遍 3×3 柔化。
		//
		// 采样越界时补什么值，**必须分方向**：
		//   · 贴着窗口边的那两侧（可以理解为"窗口之外"）补 1（不透明）。那里是
		//     DWM 填的白，月牙必须不透明地盖住它，否则弧外漏一圈白。
		//   · 朝面板内部的那两侧补 0（透明）。这里曾经一律补 1，代价很具体：
		//     月牙是 40×40 的位图，弧半径只有 30，朝内那两条边离弧还有 10 像素
		//     的空档，本来就该全透明；补 1 之后柔化把这条边上的 mask 拉到 0.33
		//     左右，于是位图的内边缘沿着整个 40 像素铺出一条"桌面色"带 ——
		//     因为位图是方的，这条带在面板内部就是一条水平、一条垂直的边。
		//     用户看到并描出来的那两条框线，就是它。
		for pass := 0; fresh && pass < 2; pass++ {
			for y := 0; y < size; y++ {
				for x := 0; x < size; x++ {
					var sum float64
					for dy := -1; dy <= 1; dy++ {
						for dx := -1; dx <= 1; dx++ {
							xx, yy := x+dx, y+dy
							if xx < 0 || yy < 0 || xx >= size || yy >= size {
								if cornerOuterSide(corner, size, xx, yy) {
									sum += 1
								}
								continue
							}
							sum += masks[yy*size+xx]
						}
					}
					blur[y*size+x] = sum / 9
				}
			}
			copy(masks, blur)
		}
		if fresh {
			a.maskCache[corner] = masks
		}
		// 3) 合成：颜色是该像素真实的桌面色（patch 里连壁纸的渐变一起搬过来），
		//    alpha 用柔化后的 mask。
		img := image.NewNRGBA(image.Rect(0, 0, size, size))
		for y := 0; y < size; y++ {
			for x := 0; x < size; x++ {
				cr, cg, cb := float64(c.R), float64(c.G), float64(c.B)
				if patch != nil && x < patch.Rect.Dx() && y < patch.Rect.Dy() {
					o := patch.PixOffset(x, y)
					cr, cg, cb = float64(patch.Pix[o]), float64(patch.Pix[o+1]), float64(patch.Pix[o+2])
				}
				al := masks[y*size+x]
				// 区域外必须完全不透明：那里是 DWM 的白，透出来就是漏光。
				// 同样只在角象限内判 —— 圆心的另一侧是面板内部，玻璃。
				bx, by := float64(x)+0.5, float64(y)+0.5
				if corner == 1 || corner == 3 {
					bx = float64(size) - bx
				}
				if corner == 2 || corner == 3 {
					by = float64(size) - by
				}
				inCorner := bx <= float64(arc) && by <= float64(arc)
				if inCorner && math.Hypot(float64(region)-bx, float64(region)-by) > float64(region) {
					al = 1
				}
				// 阴影压暗：月牙铺的是**抓图时**的桌面（那时外侧还没有阴影），而
				// 阴影一出来，紧挨着月牙外侧立刻暗下去 —— 月牙于是成了面板边缘
				// 内侧一道更亮的弧，看着正是"四角内部漏光/多了条白弧"。这里把
				// 同一份 alpha 曲线乘进去，月牙和外面的阴影就接成一体。
				if dark := win.CornerDarken(int(bx), int(by), region, a.shadowMargin()); dark > 0 {
					f := 1 - dark
					cr *= f
					cg *= f
					cb *= f
				}
				// 弧上那 1 DIP 的边线：颜色、宽度都和四条直边线一致，两段在弧的
				// 端点接上，整圈框就是连续的（早期只画了四条直边、跳过圆角段，
				// 所以四个角看着"没有边框"）。
				//
				// 带子放在**弧内侧**：弧半径等于区域半径，带子若以弧线为中心，
				// 外侧那半边会被窗口区域裁掉，只剩半个像素的线，接不上直边。
				// 内侧 0..edgeW 正好和直边线的位置对齐（直边也画在内侧 1 DIP）。
				//
				// 0..edgeW 这段取**满覆盖**，不做"距弧远近"的衰减：衰减会让弧上
				// 那条线比直边淡一档（实测弧上 #CDD6DD、直边 #D9D9DE），在角上
				// 看起来就是"边框到了弧就变浅了"。现在只在带子的内外各留 1 DIP
				// 的柔化，弧上和直边是同一条实线。
				//
				// 必须限定在角象限里，否则边线会沿着整个圆再画一圈，面板内部
				// 就多出一段弧形边线。
				if inCorner {
					edgeW := float64(a.glassScale())
					sdC := math.Hypot(float64(arc)-bx, float64(arc)-by) - float64(arc)
					t := -sdC // 弧内为正
					w := 0.0
					switch {
					case t >= 0 && t <= edgeW:
						w = 1
					case t > edgeW && t < edgeW*2:
						w = 1 - (t-edgeW)/edgeW
					case t < 0 && t > -edgeW:
						w = 1 + t/edgeW
					}
					if w > 0 {
						cr = cr*(1-w) + float64(edge.R)*w
						cg = cg*(1-w) + float64(edge.G)*w
						cb = cb*(1-w) + float64(edge.B)*w
						if w > al {
							al = w
						}
					}
				}
				off := img.PixOffset(x, y)
				img.Pix[off] = uint8(math.Round(cr))
				img.Pix[off+1] = uint8(math.Round(cg))
				img.Pix[off+2] = uint8(math.Round(cb))
				img.Pix[off+3] = uint8(math.Round(al * 255))
			}
		}
		a.cornerBitmaps[corner] = ui.NewBitmap(img)
	}
	if fresh {
		a.maskKey = key
	}
	a.cornerSize = size
	log.Printf("玻璃: 月牙重造 arc=%d region=%d size=%d 角0=%02X%02X%02X", arc, region, size, a.deskCorners[0].R, a.deskCorners[0].G, a.deskCorners[0].B)
}

// deskHalfAt 从唤出时那张半分辨率整屏快照里读一个点（物理像素坐标）。
//
// 为什么优先用它：屏幕 DC 的 GetPixel 在 DWM 合成下每读一个点都要等一次桌面
// 回读，实测 20 个点 60~120ms；而这张快照本来就在内存里（唤出时抓的，
// 面板一收起就释放），读一个点就是一次数组寻址。
func (a *application) deskHalfAt(x, y int) (win.PointColor, bool) {
	src := a.deskHalf
	if src == nil || a.deskHalfStep < 1 {
		return win.PointColor{}, false
	}
	sx := (x - a.deskHalfX) / a.deskHalfStep
	sy := (y - a.deskHalfY) / a.deskHalfStep
	if sx < 0 || sy < 0 || sx >= src.Rect.Dx() || sy >= src.Rect.Dy() {
		return win.PointColor{}, false
	}
	o := src.PixOffset(sx, sy)
	return win.PointColor{R: src.Pix[o], G: src.Pix[o+1], B: src.Pix[o+2], OK: true}, true
}

// colorAt 先查内存里的快照，查不到才去读屏幕（贴屏边、快照缺失时才会走到后者）。
func (a *application) colorAt(x, y int) (win.PointColor, bool) {
	if c, ok := a.deskHalfAt(x, y); ok {
		return c, true
	}
	r, g, b, ok := win.ScreenPixel(x, y)
	if !ok {
		return win.PointColor{}, false
	}
	return win.PointColor{R: r, G: g, B: b, OK: true}, true
}

// pickDeskColor 取"窗口外面那块桌面"的颜色，给四角的月牙（buildCornerBitmaps）
// 当底色用：客户区四角本来是空的，但这块窗口是 noRedirect 合成的，空的地方透不
// 出桌面，只能把桌面自己的颜色铺上去。
//
// 取样用屏幕 DC **实时**读窗口外侧 2px 的像素，不走缩略图：缩略是 5 像素步长的
// 平均，和角上那一点的真实桌面能差十几级。四角各存一份（deskCorners），因为
// 角上的月牙要铺的是**这个角附近**的色 —— 用一个平均色垫四个角，壁纸色调一不均
// 就显出一块和旁边不一样的色，圆角的硬边（窗口区域是 1 位掩码、没有抗锯齿）
// 立刻露出来。
// pickEdgeGlass 取四条直边**中点**内侧的玻璃色（上、右、下、左）。
//
// 最外那一圈像素要用玻璃色补齐（右边、下边那半格不补就被 DWM 补成白色），
// 只拿四角颜色插值会在边中点偏色（实测右侧边中点真实玻璃 (181,191,201)，
// 插出来 (167,174,177)，深了一档，看着就是一条细边）。磨砂在面板上是随壁纸
// 渐变的，所以每条边中间也得采一个点。
func (a *application) pickEdgeGlass(x, y, w, h int) [4]uint32 {
	var out [4]uint32
	small := a.frostSmall
	if small == nil || small.Rect.Dx() == 0 || a.screenW <= 0 || a.screenH <= 0 {
		return out
	}
	sw, sh := small.Rect.Dx(), small.Rect.Dy()
	const inset = 12
	pts := [4][2]int{
		{x + w/2, y + inset},
		{x + w - 1 - inset, y + h/2},
		{x + w/2, y + h - 1 - inset},
		{x + inset, y + h/2},
	}
	for i, p := range pts {
		sx := (p[0] - a.screenX) * sw / a.screenW
		sy := (p[1] - a.screenY) * sh / a.screenH
		if sx < 0 || sy < 0 || sx >= sw || sy >= sh {
			continue
		}
		o := small.PixOffset(sx, sy)
		out[i] = uint32(small.Pix[o])<<16 | uint32(small.Pix[o+1])<<8 | uint32(small.Pix[o+2])
	}
	return out
}

// pickGlassColors 取四角各自的"面板玻璃本体"颜色（左上、右上、左下、右下），
// 给阴影层铺弧那一圈的斜坡用。数据源是唤出前做好的磨砂小图：那上面任意一点
// 就是面板玻璃在该位置的颜色（磨砂 + 压白之后的结果），不需要拍屏幕 —— 面板
// 自己正挡着那里。
//
// 取点沿对角线往弧内缩 10 物理像素：再往外就进了弧带本身，再往内磨砂图的分辨
// 率又太糊。四个角分开取是因为壁纸是照片时四角明暗差得远。
func (a *application) pickGlassColors(x, y, w, h int) [4]uint32 {
	var out [4]uint32
	small := a.frostSmall
	if small == nil || small.Rect.Dx() == 0 || a.screenW <= 0 || a.screenH <= 0 {
		return out
	}
	sw, sh := small.Rect.Dx(), small.Rect.Dy()
	inset := 10
	pts := [4][2]int{
		{x + inset, y + inset},
		{x + w - 1 - inset, y + inset},
		{x + inset, y + h - 1 - inset},
		{x + w - 1 - inset, y + h - 1 - inset},
	}
	for i, p := range pts {
		sx := (p[0] - a.screenX) * sw / a.screenW
		sy := (p[1] - a.screenY) * sh / a.screenH
		if sx < 0 || sy < 0 || sx >= sw || sy >= sh {
			continue
		}
		o := small.PixOffset(sx, sy)
		out[i] = uint32(small.Pix[o])<<16 | uint32(small.Pix[o+1])<<8 | uint32(small.Pix[o+2])
	}
	return out
}

func (a *application) pickDeskColor(small *image.RGBA) {
	var sr, sg, sb, n int
	if a.panel != nil {
		if cx, cy, cw, ch := win.WindowRectPixels(a.panel.NativeHandle()); cw > 0 {
			const gap = 2
			// 四角 + 四边中点一共 8 个点，见下面的批量读色。
			// 顺序固定：左上、右上、左下、右下，和 deskCorners 一一对应。
			pts := [8][2]int{
				{cx - gap, cy - gap},
				{cx + cw + gap, cy - gap},
				{cx - gap, cy + ch + gap},
				{cx + cw + gap, cy + ch + gap},
			}
			// 四边中点再取一点，凑成更接近面板四周的平均底色。
			edges := [4][2]int{
				{cx + cw/2, cy - gap},
				{cx + cw/2, cy + ch + gap},
				{cx - gap, cy + ch/2},
				{cx + cw + gap, cy + ch/2},
			}
			copy(pts[4:], edges[:])
			// 8 个点：优先从内存里的唤出快照读，缺了才读屏幕（见 colorAt）。
			got := make([]win.PointColor, len(pts))
			for i, p := range pts {
				if c, ok := a.colorAt(p[0], p[1]); ok {
					got[i] = c
				}
			}
			// 近纯白的取样只在"另有非白取样"时才丢掉。
			//
			// 丢掉的理由：抓图失败 / 背面正好是白窗口时，那一点读到的是 #FFFFFF，
			// 认下来这个角弧外就铺一块纯白 —— 用户说的"角上漏光"。
			// 但要是八个点全是白的，那桌面本来就是白的，再丢就等于把角涂黑，
			// 所以那种情况照收（见下面的 allWhite）。
			allWhite := true
			for i := range got {
				c := got[i]
				if c.OK && !(c.R > 245 && c.G > 245 && c.B > 245) {
					allWhite = false
					break
				}
			}
			blank := func(c win.PointColor) bool {
				return !allWhite && c.R > 245 && c.G > 245 && c.B > 245
			}
			for i := 0; i < 4; i++ {
				c := got[i]
				if !c.OK {
					continue
				}
				if blank(c) {
					continue
				}
				a.deskCorners[i] = ui.RGB(c.R, c.G, c.B)
				a.deskCornersSet = true
				sr, sg, sb, n = sr+int(c.R), sg+int(c.G), sb+int(c.B), n+1
			}
			for i := 4; i < 8; i++ {
				c := got[i]
				if !c.OK {
					continue
				}
				if blank(c) {
					continue
				}
				sr, sg, sb, n = sr+int(c.R), sg+int(c.G), sb+int(c.B), n+1
			}
			if n > 0 {
				a.deskColor = ui.RGB(uint8(sr/n), uint8(sg/n), uint8(sb/n))
				a.deskSet = true
				log.Printf("玻璃: 桌面底色 #%02X%02X%02X（%d 个点实时取样）", sr/n, sg/n, sb/n, n)
				return
			}
		}
	}
	// 面板贴屏边、窗外一个点都读不到时，退回缩略图兜底。
	if small == nil {
		return
	}
	r, g, bl, _ := small.At(2, 2).RGBA()
	sr, sg, sb, n = int(r>>8), int(g>>8), int(bl>>8), 1
	a.deskColor = ui.RGB(uint8(sr/n), uint8(sg/n), uint8(sb/n))
	a.deskSet = true
	log.Printf("玻璃: 桌面底色 #%02X%02X%02X（缩略图兜底）", sr/n, sg/n, sb/n)
}

// frostFromScaled 把一张已缩好的整屏底片做成磨砂图：模糊 → 压白。
func (a *application) frostFromScaled(small *image.RGBA) *ui.Bitmap {
	if small == nil {
		return nil
	}
	// 缩略图上模糊半径按同一比例小（放大了看是一片均匀的糊）。
	small = win.Blur(small, 5, 3)
	// 压一层近似白（247,248,252，alpha 150 ≈ 盖住六成）：牛奶白磨砂，
	// 背后透得出颜色和明暗。
	small = win.Tint(small, 247, 248, 252, 150)
	// 阴影层取"玻璃色"用：磨砂图上任意一点就是面板玻璃在那里的颜色。
	a.frostSmall = small
	// 图在界面上的尺寸 = 整个虚拟屏的 DIP 尺寸，窗口在里面取景。
	s := a.glassScale()
	a.frostW, a.frostH = float32(a.screenW)/s, float32(a.screenH)/s
	// 尺寸没变就复用同一张 Bitmap：mygo 的 Update 让 GPU 留着这张纹理、只换像素，
	// 而 NewBitmap 每建一张就是一条新纹理，而且"每条都会留一会儿"（bitmap.go 原话），
	// 唤出几十次就会攒下一堆 —— 这也是之前内存涨的其中一个来源。
	if a.frost != nil && a.frostPixW == small.Rect.Dx() && a.frostPixH == small.Rect.Dy() {
		a.frost.Update(small)
		return a.frost
	}
	a.frostPixW, a.frostPixH = small.Rect.Dx(), small.Rect.Dy()
	return ui.NewBitmap(small)
}

// glassOffset 返回整屏磨砂图相对窗口客户区左上角的 DIP 偏移（总是 ≤ 0）。
// 面板拖到哪，这个偏移就跟着变，窗口里露出的桌面也就跟着变。
func (a *application) glassOffset() (float32, float32) {
	if a.panel == nil {
		return a.glassOffX, a.glassOffY
	}
	cx, cy, _, _ := win.ClientRectPixels(a.panel.NativeHandle())
	s := a.glassScale()
	return -float32(cx-a.screenX) / s, -float32(cy-a.screenY) / s
}

// refreshGlass 是"唤出前那一套"：抓一次快照。
func (a *application) refreshGlass() {
	a.snapshotScreen()
}

// glassScale 是这块屏的 DPI 缩放，没问到就按 1 算。
func (a *application) glassScale() float32 {
	if a.scale > 0 {
		return a.scale
	}
	return 1
}

// applyRoundRegion 给面板窗口设圆角区域。
//
// 它裁不动交换链里的内容（那是 noRedirect 窗口，内容归 DirectComposition 管），
// 但 **DWM 的阴影是按窗口区域画的** —— 不设的话阴影四角是方的，方角阴影糊在
// ClipInset 裁出来的圆角上，看上去仍然是直角。两边各管一半：内容靠 ClipInset，
// 阴影靠这里。半径用 DIP × DPI，窗口区域要的是物理像素。
func (a *application) applyRoundRegion() {
	if a.frost == nil {
		// Win11 的系统玻璃：圆角交给 DWM 的 WindowCornerPreference，不必插手。
		return
	}
	hwnd := a.panel.NativeHandle()
	if hwnd == 0 {
		return
	}
	// 区域半径取 glassRadius，内容那条弧（glassClipRadius）小它一圈：弧整个落在
	// 区域里面，区域外全是四角月牙的不透明部分（该角真实桌面色），硬边两侧同色。
	// 反过来让弧比区域大，会切出"弧内却在区域外"的一小片白 —— 那就是右下角
	// 一直漏白点、四角看着发脆的根。
	r := int(math.Round(float64(glassRadius) * float64(a.glassScale())))
	if !win.RoundRegion(hwnd, r) {
		log.Print("玻璃: 设置窗口圆角区域失败")
		return
	}
	// 设完立刻复查一次：这一行有两个用处 —— 一是证明"查得到区域"（ensureShell 的
	// 判断就靠这个），二是把句柄记进日志，排查时能直接用外部工具对它做手脚。
	if !win.HasRegion(hwnd) && !a.regionQueryLogged {
		a.regionQueryLogged = true
		log.Printf("玻璃: 区域设完复查不到（hwnd=0x%X），GetWindowRgn 可能不认这个窗口", hwnd)
	}
	// 这里**不再**给子窗口设区域。
	//
	// SetWindowRgn 是真的改变窗口形状，不只是画法：区域之外的鼠标事件不属于这个
	// 窗口，会直接穿透过去，UI 层连 pointer down 都收不到。原生 UI 画在铺满客户区
	// 的子窗口里 —— 给它套 20 物理像素的圆角区域，四个角那一小块就成了"按下去毫无
	// 反应"的死区。最要命的是右下角：那里正是拖面板时手最容易落下去的地方。
	// 实测（程序化鼠标，从各边中点按下拖 120×80）：上、左、右、下、中下都能拖动，
	// 只有右下角 (w-25, h-25) 拖不动 —— 就是被这块区域挖掉的。
	//
	// 四角不需要靠子窗口区域来"挖空"：顶层区域已经把圆角切出来了，区域外的像素
	// 系统根本不显示，内容画到客户区四角上也不会露出来。DWM 的阴影同理，按顶层
	// 区域画，四角是圆的。
}

// refreshShadow 按面板当前的位置和尺寸重画自绘阴影层，并让它显出来。
//
// 为什么是自绘：Win10 上**窗口只要有 SetWindowRgn 的区域，DWM 就不投阴影** ——
// 空闲时窗口外侧整条带全是桌面底色；只有拖动缩放那一瞬间（区域被撤掉）才冒出
// 一圈 #C7C7C7→#FFFFFF 的影，停手把区域设回去立刻消失。DWMWA_NCRENDERING_POLICY
// 和 DwmExtendFrameIntoClientArea 都返回成功，影依然没有。而圆角和抗锯齿只能靠
// 区域（SetWindowRgn 是 1 位掩码），两者不能共存，所以自己画一层
// （见 internal/win/shadow.go：一个 layered 窗口垫在面板底下，交一张带 alpha 的
// 位图，阴影就跟着圆角走，跟面板形状锁死）。
func (a *application) refreshShadow() {
	if a.shadow == nil {
		return
	}
	hwnd := a.panel.NativeHandle()
	if hwnd == 0 {
		return
	}
	x, y, w, h := win.WindowRectPixels(hwnd)
	if w <= 0 || h <= 0 {
		return
	}
	s := a.glassScale()
	r := int(math.Round(float64(glassRadius) * float64(s)))
	// 调试开关（只在手动跑 dist\baozi.debug.exe 时用）：
	//   BAOZI_NOSHADOW=1  阴影层整块不上屏（看"没有自绘层时四角到底有没有白"）
	//   BAOZI_NOCORNER=1  只保留外圈阴影，不补四角弧外的桌面色/边框弧
	if os.Getenv("BAOZI_NOSHADOW") == "1" {
		a.hideShadow()
		return
	}
	// 四角"弧外"那块要铺的是**当前位置**的真实桌面：用唤出那一刻抓的半分辨率
	// 整屏图现场裁，而不是用旧位置 BitBlt 下来的 patch —— 后者在面板被拖动/缩放
	// 之后就过期了，壁纸是照片时那块颜色会明显串味，看起来就是"角上画了一道圆弧"。
	patches := a.cornerPatchesAt(x, y, w, h, int(math.Round(float64(glassRadius)*float64(s)))+10)
	if os.Getenv("BAOZI_NOCORNER") == "1" {
		patches = [4]*image.RGBA{}
	}
	t0 := time.Now()
	gl := a.pickGlassColors(x, y, w, h)
	ok := a.shadow.Place(hwnd, win.ShadowSpec{
		X: x, Y: y, W: w, H: h,
		Radius:  r,
		Margin:  a.shadowMargin(),
		Corners: patches,
		Border:  a.cornerEdge,
		// 边框线不画了（见 main.go 的 panelBorder）。右边、下边那格白由磨砂图多铺
		// 两个像素盖掉（main.go 里的 extra），这里不再补色 —— 采样色和真实玻璃总差
		// 一两档，1px 的色差本身就是一条细边。
		BorderW:      0,
		Desk:         uint32(a.deskColor.R)<<16 | uint32(a.deskColor.G)<<8 | uint32(a.deskColor.B),
		Glass:        gl[0],
		GlassCorners: [4]uint32{gl[0], gl[1], gl[2], gl[3]},
		EdgeGlass:    a.pickEdgeGlass(x, y, w, h),
	})
	if d := time.Since(t0); d > 3*time.Millisecond {
		log.Printf("阴影: 重建 %.2fms（位图 %.2f + 上传 %.2f）",
			float64(d.Microseconds())/1000,
			float64(a.shadow.BuildCost().Microseconds())/1000,
			float64(a.shadow.ULWCost().Microseconds())/1000)
	}
	if !ok {
		log.Print("阴影: 图层布局失败")
	}
	// 诊断：把这一帧真正交给 DWM 的位图存成 PNG（分层窗口用 GetPixel 拍不到，
	// 想量四角的抗锯齿只能看这份原始数据）。
	//   BAOZI_SHADOWDUMP=<路径>  每次重建覆盖一次
	if p := os.Getenv("BAOZI_SHADOWDUMP"); p != "" {
		if err := a.shadow.DumpRGBA(p); err != nil {
			log.Printf("阴影: 导出失败 %v", err)
		}
	}
	a.shadow.Show()
}

// hideShadow 收起阴影层（面板藏起来、或者正在被拖动缩放时）。
func (a *application) hideShadow() {
	if a.shadow != nil {
		a.shadow.Hide()
	}
}

// followShadow 面板被拖动时只挪阴影，不重算位图 —— 位图尺寸没变，重算一次要
// 遍历几十万像素，拖动中间每帧都算就跟不动了。
func (a *application) followShadow() {
	if a.shadow == nil || !a.shadow.Shown() {
		return
	}
	hwnd := a.panel.NativeHandle()
	if hwnd == 0 {
		return
	}
	// 用窗口矩形（不是客户区）：圆角区域、阴影位图的参照矩形都是窗口矩形，
	// 混着用会在拖动时错开那圈边框带的宽度，阴影相对面板跳一下。
	x, y, _, _ := win.WindowRectPixels(hwnd)
	// 拖动中尺寸不变，宽度按上次建好的位图来（由 Place 记着）。
	a.shadow.Move(x, y, a.shadowMargin())
}

func (a *application) shadowMargin() int {
	return int(math.Round(float64(win.ShadowMargin) * float64(a.glassScale())))
}

// dragging 判断"刚才这几百毫秒里窗口正在被拖动/缩放"。
// OnMove/OnResize 每来一条就把 dragUntil 往后推 600ms，停手之后它自然过期。
func (a *application) dragging() bool {
	until := a.dragUntil.Load()
	return until != 0 && time.Now().UnixNano() < until
}

// ensureShell 检查"圆角区域 + 阴影层"这两件外观状态还在不在，缺了立刻补。
//
// 要这么一个兜底，是因为它们都是**别人手里**的状态：
//
//   - 圆角区域挂在面板的 HWND 上。Show/Hide、被别的窗口顶掉再拉回来、
//     mygo 内部重建窗口时，系统都可能把这个区域清掉；区域一没，四角就是四个
//     半透明的直角（玻璃一直铺到角上），看起来像"圆角坏了"。
//   - 阴影层是另一个独立窗口，可能没上屏或被压到面板下面。
//
// 一次检查就是两次廉价的 Win32 查询（GetWindowRgn + IsWindowVisible），
// 所以唤出之后可以按节拍复查；只有真的缺了才会付重画的代价。
func (a *application) ensureShell(reason string) {
	if a.frost == nil || a.shadow == nil {
		return
	}
	// 用程序自己的开关当门，而不是问系统"窗口可见吗"：面板是 noRedirect 的
	// 合成窗口，IsWindowVisible 在这类窗口上不可靠（实测会报 false），拿它当门
	// 会让这里永远不干活 —— 那正是"偶尔四个直角、要再激活一次才好"活下来的原因。
	if !a.panelUp.Load() {
		return
	}
	hwnd := a.panel.NativeHandle()
	if hwnd == 0 {
		return
	}
	hasRegion := win.HasRegion(hwnd)
	shadowShown := a.shadow.Shown()
	// 每次唤出后的前三次复查各记一行：这样"圆角/阴影什么时候丢、过了多久补上"
	// 在日志里是可见的，不用等用户报现象再猜。三次之后只在真的补的时候记。
	if a.shellProbes < 3 {
		a.shellProbes++
		log.Printf("外观复查 %s: 窗口区域=%v 阴影上屏=%v", reason, hasRegion, shadowShown)
	}
	if !hasRegion {
		log.Printf("玻璃: 圆角区域丢了（%s），补设一次", reason)
		a.applyRoundRegion()
	}
	// 拖动/缩放那 0.x 秒本来就要收起阴影层（见 OnResize 的注释），别在这里叫醒它。
	if a.dragging() {
		return
	}
	// 这里**每次复查都真的推一次** ShowWindow，而不是先问"它可见吗"：
	// 状态标记和系统状态都可能骗人（实测出现过"标记说开着、窗口其实被藏了"，
	// 于是这个自愈一次都不跑，四个直角就一直留着）。推一次的成本可以忽略，
	// 返回值是"推之前是否已可见"，false 才说明真被藏过，这时重建一次位图。
	if !a.shadow.ForceShow() {
		log.Printf("阴影: 层被藏过（%s），强制补回", reason)
		a.refreshShadow()
	}
}

// clamp01 把值夹到 [0,1]。
func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
