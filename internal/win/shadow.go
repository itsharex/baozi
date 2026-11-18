// 自绘阴影：一个独立的 layered 窗口，垫在面板底下一层，画一圈带 alpha 的黑色。
//
// 为什么不用 DWM 的阴影：Win10 上**只要窗口被 SetWindowRgn 设了区域，DWM 就不投
// 阴影**。这一点是逐条实测出来的 —— 空闲（区域在）时窗口外侧整条带全是桌面底色；
// 拖动缩放的那一瞬间（区域被撤掉）才短暂冒出 #C7C7C7→#FFFFFF 的一圈影；停手把区域
// 设回去，影立刻消失。DwmSetWindowAttribute(DWMWA_NCRENDERING_POLICY, ENABLED) 和
// DwmExtendFrameIntoClientArea(1px) 都返回成功，但影依然不出来（属性是真设上了，
// 只是"有区域的窗口"这个分类在 DWM 里就没有影）。
//
// 而圆角和抗锯齿必须靠区域（SetWindowRgn 是 1 位掩码，只能用区域，Win10 没有
// 别的圆角手段），所以两者不能共存 —— 只剩自己画一条路。
//
// 形状：UpdateLayeredWindow 交一张 32 位预乘 BGRA 的位图，alpha 由"到圆角矩形的
// 有符号距离"算出来：矩形外 d>0，alpha 按 (1-d/m)^2 衰减到 0；矩形内 d<0 留 0
// （那块被面板盖住）。位图比面板四周各大一圈 margin，正好是阴影扩散的范围。
package win

import (
	"fmt"
	"image"
	"image/png"
	"math"
	"os"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

func pngEncode(path string, img image.Image) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}

const (
	wsPopup         = 0x80000000
	wsExLayered     = 0x00080000
	wsExTransparent = 0x00000020
	wsExNoActivate  = 0x08000000
	wsExToolWindow  = 0x00000080

	swShowNoActivate = 4
	swHide           = 0
	swpNoActivate    = 0x0010
	hwndTopmost      = ^uintptr(0)

	ulwAlpha = 2

	shadowClassName = "BaoziShadowWnd"
	// ShadowMargin 是阴影从面板边缘往外扩散的宽度（DIP）：DWM 在 Win10 上大约是
	// 10~14px，这里取 16 让它再软一点。
	ShadowMargin = 16
	// constMaxAlpha 是阴影最深处的黑度。面板底色很浅，再深就显脏。
	constMaxAlpha = 0.42
)

var (
	procUpdateLayeredWindow = user32.NewProc("UpdateLayeredWindow")
	procShowWindow          = user32.NewProc("ShowWindow")
	procSetWindowPos        = user32.NewProc("SetWindowPos")

	shadowClassOnce  sync.Once
	shadowClassReady bool
	shadowWndProcCb  = syscall.NewCallback(shadowWndProc)
)

// blendFunction 是 BLENDFUNCTION 的布局。
type blendFunction struct {
	BlendOp             byte
	BlendFlags          byte
	SourceConstantAlpha byte
	AlphaFormat         byte
}

// point 是 POINT，size 是 SIZE。
type point struct{ X, Y int32 }
type size struct{ CX, CY int32 }

// Shadow 是一层自绘阴影窗口。
type Shadow struct {
	mu   sync.Mutex
	hwnd uintptr

	memDC uintptr
	dib   uintptr
	old   uintptr
	bits  []byte
	w, h  int32
	x, y  int32 // 阴影窗口左上角的屏幕坐标（物理像素）

	shown bool

	// 诊断：最近一次 UpdateLayeredWindow 的结果。
	dbg     bool
	dbgInfo string
	// 最近一次 Place 的分段耗时：位图重画 / UpdateLayeredWindow。
	buildCost, ulwCost time.Duration
}

// NewShadow 建一个隐藏状态的阴影窗口。失败返回 nil。
func NewShadow() *Shadow {
	if !shadowEnsureClass() {
		return nil
	}
	instance, _, _ := procGetModuleHandleW.Call(0)
	name, err := syscall.UTF16PtrFromString(shadowClassName)
	if err != nil {
		return nil
	}
	// 不带 WS_EX_TRANSPARENT：带着它这块分层窗口在 Win10 上整片不合成（下面的
	// 探针脚本能画出半透明方块，把 TRANSPARENT 加进去就画不出来）。鼠标穿透改用
	// WM_NCHITTEST 回 HTTRANSPARENT，效果一样、不影响合成。
	ex := uintptr(wsExLayered | wsExNoActivate | wsExToolWindow)
	hwnd, _, _ := procCreateWindowExW.Call(
		ex, uintptr(unsafe.Pointer(name)), 0, wsPopup,
		0, 0, 0, 0, 0, 0, instance, 0)
	if hwnd == 0 {
		return nil
	}
	return &Shadow{hwnd: hwnd}
}

func shadowEnsureClass() bool {
	shadowClassOnce.Do(func() {
		instance, _, _ := procGetModuleHandleW.Call(0)
		name, err := syscall.UTF16PtrFromString(shadowClassName)
		if err != nil {
			return
		}
		wc := wndClass{
			lpfnWndProc:   shadowWndProcCb,
			hInstance:     instance,
			lpszClassName: uintptr(unsafe.Pointer(name)),
		}
		if r, _, _ := procRegisterClassW.Call(uintptr(unsafe.Pointer(&wc))); r != 0 {
			shadowClassReady = true
			return
		}
		// 类已经注册过（同一个进程重复 NewShadow）也算可用。
		shadowClassReady = true
	})
	return shadowClassReady
}

func shadowWndProc(hwnd, msg, wParam, lParam uintptr) uintptr {
	const wmNCHitTest = 0x0084
	const htTransparent = ^uintptr(0) // -1：让鼠标落到下面的窗口
	if msg == wmNCHitTest {
		return htTransparent
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wParam, lParam)
	return r
}

// Place 重建阴影位图并把它挪到面板正下方（z 序上紧贴面板底下）。
//
// x、y、w、h、radius、margin 都是**物理像素**：radius 是面板圆角半径，margin 是
// 阴影从面板边缘往外扩散的宽度（调用方按 DPI 折算好再传，这里不再乘）。
// ShadowSpec 是一次阴影层布局的全部参数。
//
// 阴影层不只画影子：四角"弧外那一块"也归它 —— 面板被 SetWindowRgn 切掉形状
// 之后，弧外是 DWM 填的白、面板自己的像素又被区域丢弃，只有这一层（压在面板之
// 上、不受区域裁剪）能把那块白换成真实桌面色，并把圆角的边框弧补上。
type ShadowSpec struct {
	X, Y, W, H     int
	Radius, Margin int
	// Corners 是四角那一小块**真实桌面**的逐像素副本（左上、右上、左下、右下），
	// 可以给 nil：那就退化成不做填充，只画阴影。
	Corners [4]*image.RGBA
	// Border 是圆角边框弧的颜色（0xRRGGBB），BorderW 是线宽（物理像素）。
	Border  uint32
	BorderW int
	// Desk 是"抓不到四角桌面图块时"用的兜底底色（0xRRGGBB）。给 0 表示不兜底，
	// 弧外留空（只有阴影压着，边缘会发毛、还会漏 DWM 的白）。
	Desk uint32
}

func (s *Shadow) Place(panel uintptr, sp ShadowSpec) bool {
	// 兼容旧签名：参数全在 sp 里。
	x, y, w, h, radius, margin, corners := sp.X, sp.Y, sp.W, sp.H, sp.Radius, sp.Margin, sp.Corners
	if s == nil || s.hwnd == 0 || w <= 0 || h <= 0 || margin <= 0 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	m := margin
	tw, th := int32(w+2*m), int32(h+2*m)
	if !s.reserve(tw, th) {
		return false
	}
	tBuild := time.Now()

	// 阴影曾经整体下移 m/8 模拟"光源在上"，代价很具体：位图只比面板大 m，
	// 下移之后底边那几行离参照矩形的距离还不到 m、alpha 不为 0，于是阴影在
	// 图层最下一行被"切断"，屏幕上就是一道水平的直角硬边（右下角尤其明显）。
	// 现在不偏移：alpha 在 d=m 处正好归零，图层边缘天然干净。
	offY := float64(m)
	r := float64(radius)
	// 这里用 (w-1)/2 而不是 w/2 当中心：面板占的是 [m, m+w-1] 这 w 个像素，
	// 用 w/2 时左右两侧到中心的距离差半格 —— 左边第一格离边缘 1 格、右边第一格
	// 离边缘 0 格，于是右边的阴影从"零"起步，紧贴面板右边和下边的那一列/行完全
	// 没被压暗，露出一条 1px 的亮线（截图实测右侧 #F5F5F5、下侧 #FFFFFF；
	// 左边、上边那一格却压暗到 #A3A3A3）。用户看到的就是"下边和右边在边框内部
	// 还有一条线"。改成 (w-1)/2 之后四条边对称，第一格都有影。
	hw, hh := float64(w-1)/2, float64(h-1)/2
	qw, qh := hw-r, hh-r
	if qw < 0 {
		qw = 0
	}
	if qh < 0 {
		qh = 0
	}
	const maxAlpha = constMaxAlpha
	mf := float64(m)
	cx0, cy0 := hw, hh // 和上面的中心保持一致，见那条注释（差半格会在右下露出一条亮线）
	// 只画"边带"：面板盖住的那一大片内部 alpha 恒为 0，没必要每帧都算一遍。
	// 这一改动把缩放时的一帧从"整张 1140×760 ≈ 87 万像素"降到"四条带"，
	// 是"缩放跟不跟手"的关键（整张算的时候一次回调 15~25ms，肉眼就是卡）。
	//
	// 再往下一层：直边上 alpha 其实只跟**一个**坐标有关 —— 顶/底带里，只要这一列
	// 不在四角范围内（qx<=0），d 就等于 qy-r，一行共用一个值；左右带同理一列一个值。
	// 于是真正要算距离的只剩四个角块，其余都是整段刷同一个字节。
	alphaOf := func(d float64) byte {
		if d <= 0 {
			return 0
		}
		t := d / mf
		if t >= 1 {
			return 0
		}
		f := 1 - t
		return byte(maxAlpha*f*f*f*255 + 0.5)
		// 预乘黑：R=G=B=0 保持不变。
	}
	writePixel := func(px, py int) {
		fx := float64(px) - float64(m)
		fy := float64(py) - offY
		qx := math.Abs(fx-cx0) - qw
		qy := math.Abs(fy-cy0) - qh
		// 圆角矩形的标准有符号距离：
		//   hypot(max(qx,0), max(qy,0)) + min(max(qx,qy), 0) - r
		// 这里曾写成 hypot(qx, qy) 直接用原始值 —— 只要有一轴深入面板内部
		// （qy 是个大负数），hypot 就被那个负数撑爆，d 直接冲到几百，
		// 于是整条直边算出来"离得极远"、alpha 全 0，只有两个轴都为正的
		// 四个角才有影。屏幕上看到的就是"只有角上有影、四条边光秃秃"。
		mx, my := math.Max(qx, 0), math.Max(qy, 0)
		// 用 Sqrt 而不是 Hypot：这里是内层循环，Hypot 为了防溢出做的缩放很贵，
		// 一帧下来能差 2~3ms；这里的量级只有几百像素，用不着那套保护。
		d := math.Sqrt(mx*mx+my*my) + math.Min(math.Max(qx, qy), 0) - r
		s.bits[(py*int(tw)+px)*4+3] = alphaOf(d)
	}
	twi, thi := int(tw), int(th)
	// 中间那一段（两个轴向都算"在内侧"）的列/行区间：这里只有另一个轴起作用。
	midX0 := int(math.Max(0, math.Min(float64(twi), float64(m)+cx0-qw)))
	midX1 := int(math.Max(0, math.Min(float64(twi), float64(m)+cx0+qw)))
	midY0 := int(math.Max(0, math.Min(float64(thi), float64(m)+cy0-qh)))
	midY1 := int(math.Max(0, math.Min(float64(thi), float64(m)+cy0+qh)))

	// 左右两条竖带：中间那些行一列共用一个值。
	colAlpha := func(px int) byte {
		return alphaOf(math.Abs(float64(px)-float64(m)-cx0) - qw - r)
	}
	for px := 0; px < m; px++ {
		a := colAlpha(px)
		for py := midY0; py < midY1; py++ {
			s.bits[(py*twi+px)*4+3] = a
		}
	}
	for px := m + w; px < twi; px++ {
		a := colAlpha(px)
		for py := midY0; py < midY1; py++ {
			s.bits[(py*twi+px)*4+3] = a
		}
	}
	// 顶/底两条横带：中间那些列一行共用一个值；四角范围留给逐点算。
	rowAlpha := func(py int) byte {
		return alphaOf(math.Abs(float64(py)-offY-cy0) - qh - r)
	}
	for py := 0; py < thi; py++ {
		if py >= midY0 && py < midY1 {
			continue // 这些行只有左右竖带，已经刷过了
		}
		a := rowAlpha(py)
		for px := midX0; px < midX1; px++ {
			s.bits[(py*twi+px)*4+3] = a
		}
		for px := 0; px < midX0; px++ {
			writePixel(px, py)
		}
		for px := midX1; px < twi; px++ {
			writePixel(px, py)
		}
	}

	// 四角那圈"弧外"必须自己补上桌面色。
	//
	// 为什么必须在**这一层**补：面板被 SetWindowRgn 切掉形状之后，弧外那块是
	// DWM 填的白，而面板自己的内容全都被区域裁掉了（画在区域外的像素直接丢弃），
	// 所以 App 里那四张"月牙"位图根本盖不到那块白 —— 壁纸是深色时它就是一圈
	// 白边（用户反复说的"漏光/白色圆弧线"）。这块分层窗口不受区域裁剪，压在
	// 面板之上（面板内 alpha=0，不影响玻璃），正好能把那块白换成桌面色。
	//
	// 覆盖率掩码只跟半径和扩散宽度有关，和面板尺寸无关 —— 用角部局部坐标算一份，
	// 四个角镜像复用，缩放时一帧都不用重算。
	if radius > 0 && len(corners) > 0 {
		cov := cornerCoverage(radius, m, sp.BorderW)
		k := cov.size
		br := float64((sp.Border >> 16) & 0xFF)
		bg := float64((sp.Border >> 8) & 0xFF)
		bb := float64(sp.Border & 0xFF)
		// 兜底色：抓不到角的桌面图块时用它顶。以前这里直接 continue，弧外就留成
		// 透明 —— 那一小片既漏 DWM 的白，又只有阴影的半透明黑压着，弧边看上去
		// 就是"毛边 + 漏光"，而且四个角各缺一块、缺的还不一样（patch 常常只抓到
		// 1/4），越看越不齐。
		dr := float64((sp.Desk >> 16) & 0xFF)
		dg := float64((sp.Desk >> 8) & 0xFF)
		db := float64(sp.Desk & 0xFF)
		for c := 0; c < 4; c++ {
			patch := corners[c]
			pw, ph := 0, 0
			if patch != nil {
				pw, ph = patch.Rect.Dx(), patch.Rect.Dy()
			}
			for ly := 0; ly < k; ly++ {
				for lx := 0; lx < k; lx++ {
					cc := cov.cov[ly*k+lx]
					// band 是圆角边框弧那一条（弧**内侧** borderW 像素）的覆盖率：
					// 直边的边框线由面板自己画，弧这一段以前靠月牙描，现在也归这层。
					bb2 := cov.band[ly*k+lx]
					// seam 是紧贴弧边界的那 1 像素（弧内侧 1px）。
					// 为什么必须把它涂实：面板在区域边界上会漏一格 #FFFFFF
					// （mygo 那套圆角是"给客户端画个白底再裁形"，边界那一列没被内容盖住），
					// 半透明地叠什么色都会透出那道白 —— 用户看到的就是"角上一道白线/漏光"。
					// 涂成不透明的边框色，白线就彻底没了，观感也和四条直边的边框线一致。
					sm := cov.seam[ly*k+lx]
					if cc <= 0 && bb2 <= 0 && sm <= 0 {
						continue // 弧内深处：面板自己的玻璃，别插手
					}
					px, py := m+lx, m+ly
					switch c {
					case 1:
						px = m + w - 1 - lx
					case 2:
						py = m + h - 1 - ly
					case 3:
						px = m + w - 1 - lx
						py = m + h - 1 - ly
					}
					// 只补面板矩形**以内**那块缺口；矩形以外（外圈）保持纯黑阴影，
					// 否则拖动时旧 patch 会在影子里印出一块过期壁纸。
					if px < m || px >= m+w || py < m || py >= m+h {
						continue
					}
					off := (py*int(tw) + px) * 4
					if cc <= 0 {
						// 边框弧：seam 段涂实，band 段按覆盖率做抗锯齿。
						a := bb2
						if sm > 0 {
							a = 1
						}
						s.bits[off+0] = byte(bb * a)
						s.bits[off+1] = byte(bg * a)
						s.bits[off+2] = byte(br * a)
						s.bits[off+3] = byte(a*255 + 0.5)
						continue
					}
					ix, iy := lx, ly
					if c == 1 || c == 3 {
						ix = pw - 1 - lx
					}
					if c == 2 || c == 3 {
						iy = ph - 1 - ly
					}
					cr, cg, cb := dr, dg, db
					if patch != nil && ix >= 0 && ix < pw && iy >= 0 && iy < ph {
						o := patch.PixOffset(ix, iy)
						cr, cg, cb = float64(patch.Pix[o]), float64(patch.Pix[o+1]), float64(patch.Pix[o+2])
					}
					// 这块桌面色同样被阴影压暗，才和紧挨着的外圈接得上。
					sa := cov.shade[ly*k+lx]
					keep := 1 - sa
					// 边界那一个像素必须按 cc（"这个像素有多少面积在弧外"）把两侧
					// 颜色混起来：弧外那一份是压暗的桌面色，剩下那一份是紧挨着的
					// 边框线。以前是不管 cc 多大都整块铺桌面色 —— 覆盖率算了却只当
					// 布尔用，边界实际是 1 位掩码，弧上就是一串 1 像素台阶，也就是
					// 用户说的"角的毛边"。alpha 仍必须是 255：这块在面板轮廓外，
					// 桌面是原样可见的，半透明会把边界那格的白漏出来。
					t := cc
					s.bits[off+0] = byte(cb*keep*t + bb*(1-t))
					s.bits[off+1] = byte(cg*keep*t + bg*(1-t))
					s.bits[off+2] = byte(cr*keep*t + br*(1-t))
					s.bits[off+3] = 255
				}
			}
		}
	}

	// 四条直边的那一圈边线也由这一层描。
	//
	// 为什么不让面板自己画：面板的 DIP 宽高换成物理像素不是整数（909 DIP × 1.25
	// = 1136.25），右边、下边那条边线就压在半像素上 —— 窗口只有 1136 格，最后一格
	// 只盖到一部分，于是右边和下边剩下一格亮底（截图实测右侧 #F5F5F5、下侧
	// #FFFFFF），看上去就是"边框里面还有一条线"；上边、左边从 0 起算，没有这个
	// 偏差，所以只坏在右下两边。这一层是按**整数物理像素**布局的（矩形就落在
	// 面板的窗口矩形上），在这儿描一圈永远对得齐，顺手把那格亮底盖掉。
	if sp.BorderW > 0 && w >= 4 && h >= 4 {
		br2 := byte((sp.Border >> 16) & 0xFF)
		bg2 := byte((sp.Border >> 8) & 0xFF)
		bb2 := byte(sp.Border & 0xFF)
		// 圆角那一段由上面的弧带负责，直边从弧的切点开始。
		rInset := int(math.Ceil(r))
		if rInset > w/2 {
			rInset = w / 2
		}
		if rInset > h/2 {
			rInset = h / 2
		}
		setEdge := func(px, py int) {
			if px < 0 || px >= twi || py < 0 || py >= thi {
				return
			}
			o := (py*twi + px) * 4
			// 位图是 BGRA（和上面弧带那段一致：索引 0 是蓝）
			s.bits[o+0] = bb2
			s.bits[o+1] = bg2
			s.bits[o+2] = br2
			s.bits[o+3] = 255
		}
		bw := sp.BorderW
		if bw > 4 {
			bw = 4
		}
		for k := 0; k < bw; k++ {
			for px := m + rInset; px < m+w-rInset; px++ {
				setEdge(px, m+k)     // 上边
				setEdge(px, m+h-1-k) // 下边
			}
			for py := m + rInset; py < m+h-rInset; py++ {
				setEdge(m+k, py)     // 左边
				setEdge(m+w-1-k, py) // 右边
			}
		}
	}

	src := point{}
	dst := point{X: int32(x - m), Y: int32(y - m)}
	sz := size{CX: tw, CY: th}
	blend := blendFunction{SourceConstantAlpha: 255, AlphaFormat: 1}
	tULW := time.Now()
	// GetDC 必须配 ReleaseDC：这个函数在缩放时**每帧**都会跑，漏一次就是漏一个 DC。
	// 曾经这里直接 screenDC() 不还，拖动几十帧之后进程的 DC 池被吃光，整个桌面卡住
	// （用户实测"缩放时卡死"）。严格说漏的 DC 会被系统在进程退出时回收，但
	// 一帧一次的速度下几百帧就够呛了。
	dc := screenDC()
	ulr, _, ulerr := procUpdateLayeredWindow.Call(s.hwnd, dc,
		uintptr(unsafe.Pointer(&dst)), uintptr(unsafe.Pointer(&sz)),
		s.memDC, uintptr(unsafe.Pointer(&src)), 0,
		uintptr(unsafe.Pointer(&blend)), ulwAlpha)
	if dc != 0 {
		procReleaseDC.Call(0, dc)
	}
	ulwCost := time.Since(tULW)
	s.dbg = ulr == 0
	s.dbgInfo = "err=" + ulerr.Error()
	s.x, s.y = dst.X, dst.Y
	// 压在面板**上面**一层（HWND_TOPMOST），不是垫在下面。
	// 垫在下面实测完全看不见：窗口被 SetWindowRgn 切掉形状之后，DWM 对这块
	// 区域的处理让下面的分层窗口整片不透出来（位图 alpha 明明有值、
	// UpdateLayeredWindow 也返回成功）。放上面就没这个问题 —— 面板里面那块的
	// alpha 恒为 0，所以面板本身一点不受影响；鼠标穿透由 WM_NCHITTEST 兜住。
	pr, _, _ := procSetWindowPos.Call(s.hwnd, hwndTopmost, uintptr(dst.X), uintptr(dst.Y),
		uintptr(tw), uintptr(th), swpNoActivate)
	s.dbgInfo = fmt.Sprintf("ulw=%v swp=%v %s", ulr != 0, pr != 0, s.dbgInfo)
	s.buildCost, s.ulwCost = tULW.Sub(tBuild), ulwCost
	return true
}

// CornerDarken 给四角的"月牙"用：返回该点应当叠加的黑度（0 表示没有阴影）。
//
// bx/by 是从**这个角**沿两条面板边往内量的像素距离（角顶点是 0,0），radius 是
// 圆角半径、margin 是阴影扩散宽度，都是物理像素。alpha 的曲线和 Place 里画位图
// 时那条完全一致，所以月牙铺的桌面色能和紧挨着的阴影接上。
//
// 为什么需要它：月牙铺的是**没被阴影压暗**的桌面像素（取色在阴影出现之前抓），
// 一旦外侧压了一圈影，月牙就成了面板边缘内侧一道明显更亮的弧 —— 看起来就是
// "四角内部各有一道白色圆弧线"。把同一份 alpha 乘进去，两边就同色了。
func CornerDarken(bx, by, radius, margin int) float64 {
	if margin <= 0 {
		return 0
	}
	var inside float64 // 到圆角矩形边界的有符号距离，正数=在面板里面
	if bx < radius && by < radius {
		inside = float64(radius) - math.Hypot(float64(radius-bx), float64(radius-by))
	} else {
		inside = float64(min(bx, by))
	}
	d := -inside // 正数=在圆角外面，也就是阴影可见的那一侧
	if d <= 0 || d >= float64(margin) {
		return 0
	}
	f := 1 - d/float64(margin)
	return constMaxAlpha * f * f * f
}

// DumpRGBA 把当前位图原样导成 PNG（BGRA → RGBA），诊断用：量四角抗锯齿、
// 查弧内侧有没有被涂成纯白，都得看这份真数据，屏幕截图会被别的窗口干扰。
func (s *Shadow) DumpRGBA(path string) error {
	if s == nil {
		return fmt.Errorf("nil shadow")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.bits) == 0 {
		return fmt.Errorf("no bits")
	}
	img := image.NewNRGBA(image.Rect(0, 0, int(s.w), int(s.h)))
	for i, o := 0, 0; i < int(s.w)*int(s.h); i, o = i+1, o+4 {
		img.Pix[o+0] = s.bits[o+2]
		img.Pix[o+1] = s.bits[o+1]
		img.Pix[o+2] = s.bits[o+0]
		img.Pix[o+3] = s.bits[o+3]
	}
	img.Stride = int(s.w) * 4
	return pngEncode(path, img)
}

// DumpAlpha 把当前的 alpha 图导成 PNG（诊断用：分层窗口 GetPixel 拍不到，
// 只能先把"喂给 DWM 的东西"存成文件看）。
func (s *Shadow) DumpAlpha(path string) error {
	if s == nil {
		return fmt.Errorf("nil shadow")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.bits) == 0 {
		return fmt.Errorf("no bits")
	}
	img := image.NewGray(image.Rect(0, 0, int(s.w), int(s.h)))
	for i := 0; i < int(s.w)*int(s.h); i++ {
		img.Pix[i] = s.bits[i*4+3]
	}
	// 顺手把窗口自身的矩形也报出来，确认不是坐标错位。
	return pngEncode(path, img)
}

// cornerCoverage 是四角缺口用的覆盖率与压暗系数，只算一次就缓存。
//
// 局部坐标：lx/ly 是从这个角的顶点沿两条边往内量的距离（顶点是 0,0），四个角
// 镜像之后形状完全一样，所以一份就够。cov 是"这个像素有多少面积在圆角矩形之外"
// （用它做抗锯齿），shade 是该点应叠加的阴影黑度（和外圈同一条曲线）。
type cornerCoverageT struct {
	size  int
	cov   []float64 // 边界软化系数：1=完全在弧外、0=完全在弧内
	shade []float64 // 同一位置该压多黑（和外圈阴影同曲线）
	band  []float64 // 弧内侧 borderW 像素的边框覆盖率
	seam  []float64 // 紧贴弧边界的 1px（弧内侧），要涂实遮住面板漏的白
}

var (
	cornerCovKey [3]int
	cornerCovVal *cornerCoverageT
	cornerCovMu  sync.Mutex
)

func cornerCoverage(radius, margin, borderW int) *cornerCoverageT {
	cornerCovMu.Lock()
	defer cornerCovMu.Unlock()
	if cornerCovVal != nil && cornerCovKey == [3]int{radius, margin, borderW} {
		return cornerCovVal
	}
	size := radius + margin + 2
	const ss = 8
	sub := 1.0 / float64(ss)
	cov := cornerCoverageT{
		size:  size,
		cov:   make([]float64, size*size),
		shade: make([]float64, size*size),
		band:  make([]float64, size*size),
		seam:  make([]float64, size*size),
	}
	r := float64(radius)
	mf := float64(margin)
	bw := float64(borderW)
	if bw < 1 {
		bw = 1
	}
	// seamW 是"弧内侧要涂实遮白"的宽度。1.0 而不是 1.5：面板自己那条 1px 边线
	// 就在紧邻的一列里，涂到 1.5 会把它盖成第二列，内侧就和玻璃撞出一个
	// 1 像素的台阶（弧上一个像素的台阶在视觉上就是"毛边"）。
	const seamW = 1.0
	// aaW 是边界软化宽度（物理像素）。这里**不用**面积覆盖率：面积法的过渡带
	// 天生只有 1 个像素宽，弧上每一行的混合值随角度抖一抖，看上去就是一串
	// 1 像素台阶（用户说的"角的毛边"）。改成以弧为中线、宽 aaW 的线性软边：
	// 相邻像素的系数连续变化，弧看起来是"虚"的，台阶就看不出来了。
	const aaW = 2.0
	for ly := 0; ly < size; ly++ {
		for lx := 0; lx < size; lx++ {
			inBand, inSeam := 0, 0
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					fx := float64(lx) + (float64(sx)+0.5)*sub
					fy := float64(ly) + (float64(sy)+0.5)*sub
					id := insideDistance(fx, fy, r)
					if id >= 0 {
						if id < bw {
							inBand++
						}
						if id < seamW {
							inSeam++
						}
					}
				}
			}
			i := ly*size + lx
			cov.cov[i] = math.Max(0, math.Min(1, 0.5-insideDistance(float64(lx)+0.5, float64(ly)+0.5, r)/aaW))
			cov.band[i] = float64(inBand) / float64(ss*ss)
			cov.seam[i] = float64(inSeam) / float64(ss*ss)
			d := -insideDistance(float64(lx)+0.5, float64(ly)+0.5, r)
			if d > 0 && d < mf {
				f := 1 - d/mf
				cov.shade[i] = constMaxAlpha * f * f * f
			}
		}
	}
	cornerCovKey = [3]int{radius, margin, borderW}
	cornerCovVal = &cov
	return &cov
}

// insideDistance 是到"圆角半径为 r 的圆角矩形"边界的有符号距离（正数=在里面）。
func insideDistance(bx, by, r float64) float64 {
	if bx < r && by < r {
		return r - math.Hypot(r-bx, r-by)
	}
	return math.Min(bx, by)
}

// MaxAlpha 返回位图里最大的 alpha（诊断用）。
func (s *Shadow) MaxAlpha() byte {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var m byte
	for i := 3; i < len(s.bits); i += 4 {
		if s.bits[i] > m {
			m = s.bits[i]
		}
	}
	return m
}

// Debug 返回最近一次布局的诊断串。
func (s *Shadow) Debug() string {
	if s == nil {
		return "nil"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dbgInfo
}

// reserve 保证位图缓冲够大，返回 false 表示没建出来。
// BuildCost / ULWCost 返回最近一次 Place 的分段耗时（诊断用）。
func (s *Shadow) BuildCost() time.Duration {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buildCost
}

// ULWCost 是最近一次 Place 里 UpdateLayeredWindow 那一步的耗时（诊断用）。
func (s *Shadow) ULWCost() time.Duration {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ulwCost
}

func (s *Shadow) reserve(w, h int32) bool {
	if s.dib != 0 && s.w == w && s.h == h {
		// 尺寸没变：清空 alpha 就够，不用重建。
		for i := 3; i < len(s.bits); i += 4 {
			s.bits[i] = 0
		}
		return true
	}
	s.release()
	memDC, _, _ := procCreateCompatibleDC.Call(0)
	if memDC == 0 {
		return false
	}
	bmi := bitmapInfoHeader{
		Size:        uint32(unsafe.Sizeof(bitmapInfoHeader{})),
		Width:       w,
		Height:      -h, // 负高度 = 自上而下的行序
		Planes:      1,
		BitCount:    32,
		Compression: 0, // BI_RGB
	}
	var bits uintptr
	dib, _, _ := procCreateDIBSection.Call(memDC, uintptr(unsafe.Pointer(&bmi)), 0, uintptr(unsafe.Pointer(&bits)), 0, 0)
	if dib == 0 || bits == 0 {
		procDeleteDC.Call(memDC)
		return false
	}
	old, _, _ := procSelectObject.Call(memDC, dib)
	s.memDC, s.dib, s.old = memDC, dib, old
	s.w, s.h = w, h
	s.bits = unsafe.Slice((*byte)(ptrOfUintptr(bits)), int(w)*int(h)*4)
	return true
}

func (s *Shadow) release() {
	if s.memDC != 0 {
		if s.old != 0 {
			procSelectObject.Call(s.memDC, s.old)
			s.old = 0
		}
		if s.dib != 0 {
			procDeleteObject.Call(s.dib)
			s.dib = 0
		}
		procDeleteDC.Call(s.memDC)
		s.memDC = 0
	}
	s.bits = nil
}

// screenDC 取屏幕 DC（UpdateLayeredWindow 的 hdcDst）。用完就还，别让它占着。
func screenDC() uintptr {
	dc, _, _ := procGetDC.Call(0)
	return dc
}

// Bounds 返回阴影窗口当前的实际矩形（诊断用）。
func (s *Shadow) Bounds() (x, y, w, h int) {
	if s == nil || s.hwnd == 0 {
		return 0, 0, 0, 0
	}
	return WindowRectPixels(s.hwnd)
}

// Move 只挪位置（面板被拖动时用，不重算位图）。
func (s *Shadow) Move(x, y, m int) {
	if s == nil || s.hwnd == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.shown {
		return
	}
	dst := point{X: int32(x - m), Y: int32(y - m)}
	dc := screenDC()
	procUpdateLayeredWindow.Call(s.hwnd, dc,
		uintptr(unsafe.Pointer(&dst)), uintptr(unsafe.Pointer(&size{CX: s.w, CY: s.h})),
		s.memDC, uintptr(unsafe.Pointer(&point{})), 0,
		uintptr(unsafe.Pointer(&blendFunction{SourceConstantAlpha: 255, AlphaFormat: 1})), ulwAlpha)
	if dc != 0 {
		procReleaseDC.Call(0, dc)
	}
	s.x, s.y = dst.X, dst.Y
}

// Show 让阴影层上屏。
//
// 这里**不能**只看缓存里的 shown 标志：窗口可能被系统或别的程序藏起来（隐藏/显示
// 面板来回切、被任务栏预览、"显示桌面"之类的动作都会碰到），而我们手里的标志还是
// true —— 那样 Show 会一直空转，阴影和四角那块桌面色就再也不回来，屏幕上就是
// "四个半透明的直角"，只有下次先 Hide 再 Show 才好（这正是用户报的那个偶发毛病）。
func (s *Shadow) Show() {
	if s == nil || s.hwnd == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.shown = true
	if vis, _, _ := procIsWindowVisible.Call(s.hwnd); vis != 0 {
		return // 已经真的在屏上，不必再 ShowWindow
	}
	procShowWindow.Call(s.hwnd, swShowNoActivate)
}

// ForceShow 无条件把阴影层推上屏，并把状态标记拉回"开着"。
//
// 和 Show 的区别：它不因为缓存标志或可见性查询而提前返回 —— 每次真的调一次
// ShowWindow(SW_SHOWNOACTIVATE)（这个调用本身是幂等的，成本可以忽略）。
// 返回值是"推之前它是不是已经可见"：false 说明确实被藏过（系统、别的程序、或者
// 我们自己在缩放里收起来之后没补回来），调用方值得记一行日志、并且重建一次位图。
func (s *Shadow) ForceShow() bool {
	if s == nil || s.hwnd == 0 {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.shown = true
	// 先问一句"现在可见吗"只是为了给调用方一个日志用的返回值，
	// ShowWindow 本身**每次都调** —— 它的可见性查询在这类分层窗口上会撒谎
	// （实测窗口明明在合成、IsWindowVisible 却报 false），拿它当门就等于门永远打不开。
	wasVisible := false
	if vis, _, _ := procIsWindowVisible.Call(s.hwnd); vis != 0 {
		wasVisible = true
	}
	procShowWindow.Call(s.hwnd, swShowNoActivate)
	return wasVisible
}

// Hide 收起阴影层。同样按**真实可见性**决定要不要调 ShowWindow：
// 缓存标志说"已经藏起来了"、窗口其实还亮着的话，就会在桌面上留一块孤零零的影
// （面板藏了、影还在，用户会以为程序没退干净）。
func (s *Shadow) Hide() {
	if s == nil || s.hwnd == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.shown = false
	if vis, _, _ := procIsWindowVisible.Call(s.hwnd); vis == 0 {
		return
	}
	procShowWindow.Call(s.hwnd, swHide)
}

// Shown 报告阴影现在是不是**真的**可见（缓存标志 + 系统实际状态，两者都成立才算）。
func (s *Shadow) Shown() bool {
	if s == nil || s.hwnd == 0 {
		return false
	}
	s.mu.Lock()
	flag := s.shown
	s.mu.Unlock()
	if !flag {
		return false
	}
	vis, _, _ := procIsWindowVisible.Call(s.hwnd)
	return vis != 0
}

// Close 销毁阴影窗口。
func (s *Shadow) Close() {
	if s == nil || s.hwnd == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.release()
	procDestroyWindow.Call(s.hwnd)
	s.hwnd = 0
	s.shown = false
}
