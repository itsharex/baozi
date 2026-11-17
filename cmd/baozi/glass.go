package main

import (
	"image"
	"log"
	"math"

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

// glassRadius 是窗口区域的圆角半径（DIP）。
//
// 这块窗口带 WS_EX_NOREDIRECTIONBITMAP，圆角以外的像素透不出桌面、DWM 填的是
// 纯白（实测 #FFFFFF），所以"窗口形状"只能靠 SetWindowRgn 给。区域是 1 位掩码、
// 没有抗锯齿，但它的硬边看不见：半径比内容圆角大 glassRadius-glassClipRadius，
// 硬边正好落在四角月牙铺的纯桌面色里，两侧同色。
// 区域同时决定 DWM 阴影的形状 —— 这也是阴影能跟着圆角走的唯一办法。
const glassRadius = 26

// glassClipRadius 是内容（面板底色、边线、四角月牙）的圆角半径（DIP）。
//
// 必须比 glassRadius 小一圈：月牙把"桌面色 → 边线 → 玻璃"这三层交界画在
// glassClipRadius 这条弧上，而窗口区域按 glassRadius 切 —— 弧整个落在区域里面，
// 区域外全归月牙的不透明部分。反过来（弧比区域大）会出现"弧内却在区域外"的
// 一小片，那片是白的，就是右下角曾经漏的白点。
//
// 差值只留 2 DIP：月牙那块不透明区填的是**单色**（该角窗外读到的桌面色），太厚
// 就会和真实桌面（有渐变、有细节）拉出一条弧形的色差带，看着像漏光。薄到 2 像素
// 之后这点色差就看不出来了。
const glassClipRadius = glassRadius - 2

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
	s := float64(a.glassScale())
	size := int(math.Round(float64(glassRadius)*s)) + 10
	if size <= 0 {
		return
	}
	cx, cy, cw, ch := win.WindowRectPixels(a.panel.NativeHandle())
	if cw < size || ch < size {
		return
	}
	origins := [4][2]int{
		{cx, cy},
		{cx + cw - size, cy},
		{cx, cy + ch - size},
		{cx + cw - size, cy + ch - size},
	}
	for i := 0; i < 4; i++ {
		img, err := win.CaptureRect(origins[i][0], origins[i][1], size, size)
		if err != nil {
			a.cornerPatches[i] = nil
			log.Printf("玻璃: 四角 patch 抓取失败 角%d %v", i, err)
			continue
		}
		a.cornerPatches[i] = img
	}
	log.Printf("玻璃: 四角 patch %dx%d 已抓（窗口 %d,%d %dx%d）", size, size, cx, cy, cw, ch)
}

func (a *application) snapshotScreen() {
	if a.panel == nil {
		return
	}
	// 四角那小块桌面顺手抓下来，给月牙弧外当底色（此刻窗口还没显示）。
	a.captureCornerPatches()
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
func (a *application) refreshCorners() {
	a.pickDeskColor(nil)
	a.refineCornerColors()
	s := float64(a.glassScale())
	edge := ui.Hex("#d9d9de") // 浅色主题的 Border，和面板边线同色
	if a.borderSet {
		edge = a.borderColor
	}
	a.buildCornerBitmaps(
		int(math.Round(float64(glassClipRadius)*s)),
		int(math.Round(float64(glassRadius)*s)),
		edge,
	)
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
	for i := 0; i < 4; i++ {
		var r, g, b, n int
		for _, d := range []int{2, 5, 8} {
			pr, pg, pb, ok := win.ScreenPixel(bases[i][0]+dirs[i][0]*d, bases[i][1]+dirs[i][1]*d)
			if !ok {
				continue
			}
			r, g, b, n = r+int(pr), g+int(pg), b+int(pb), n+1
		}
		if n == 0 {
			continue
		}
		a.deskCorners[i] = ui.RGB(uint8(r/n), uint8(g/n), uint8(b/n))
		a.deskCornersSet = true
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
	_ = edge
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
	masks := make([]float64, size*size)
	blur := make([]float64, size*size)
	for corner := 0; corner < 4; corner++ {
		c := a.deskCorners[corner]
		patch := a.cornerPatches[corner]
		// 1) 覆盖率 mask。
		for y := 0; y < size; y++ {
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
		// 2) 两遍 3×3 柔化。图外按 1（不透明）处理，免得边缘的 alpha 被拉低、
		//    在区域边上露出一圈半透明的白。
		for pass := 0; pass < 2; pass++ {
			for y := 0; y < size; y++ {
				for x := 0; x < size; x++ {
					var sum float64
					for dy := -1; dy <= 1; dy++ {
						for dx := -1; dx <= 1; dx++ {
							xx, yy := x+dx, y+dy
							if xx < 0 || yy < 0 || xx >= size || yy >= size {
								sum += 1
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
				bx, by := float64(x)+0.5, float64(y)+0.5
				if corner == 1 || corner == 3 {
					bx = float64(size) - bx
				}
				if corner == 2 || corner == 3 {
					by = float64(size) - by
				}
				if math.Hypot(float64(region)-bx, float64(region)-by) > float64(region) {
					al = 1
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
	a.cornerSize = size
	log.Printf("玻璃: 月牙重造 arc=%d region=%d size=%d 角0=%02X%02X%02X", arc, region, size, a.deskCorners[0].R, a.deskCorners[0].G, a.deskCorners[0].B)
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
func (a *application) pickDeskColor(small *image.RGBA) {
	var sr, sg, sb, n int
	if a.panel != nil {
		if cx, cy, cw, ch := win.WindowRectPixels(a.panel.NativeHandle()); cw > 0 {
			const gap = 2
			// 顺序固定：左上、右上、左下、右下，和 deskCorners 一一对应。
			corners := [4][2]int{
				{cx - gap, cy - gap},
				{cx + cw + gap, cy - gap},
				{cx - gap, cy + ch + gap},
				{cx + cw + gap, cy + ch + gap},
			}
			for i, c := range corners {
				r, g, b, ok := win.ScreenPixel(c[0], c[1])
				if !ok {
					continue
				}
				a.deskCorners[i] = ui.RGB(r, g, b)
				a.deskCornersSet = true
				sr, sg, sb, n = sr+int(r), sg+int(g), sb+int(b), n+1
			}
			// 四边中点再各取一点，凑成更接近面板四周的平均底色。
			edges := [4][2]int{
				{cx + cw/2, cy - gap},
				{cx + cw/2, cy + ch + gap},
				{cx - gap, cy + ch/2},
				{cx + cw + gap, cy + ch/2},
			}
			for _, c := range edges {
				r, g, b, ok := win.ScreenPixel(c[0], c[1])
				if !ok {
					continue
				}
				sr, sg, sb, n = sr+int(r), sg+int(g), sb+int(b), n+1
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
