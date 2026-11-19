package win

// 这里管两件小事：窗口的圆角区域，和从一张全屏快照里裁一小块。

import (
	"image"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	// gdi32 / user32 在 thumb.go、fullscreen.go 里已经建过，同一个包共用。
	dwmapi                 = windows.NewLazySystemDLL("dwmapi.dll")
	procCreateRoundRectRgn = gdi32.NewProc("CreateRoundRectRgn")
	procCreateRectRgn      = gdi32.NewProc("CreateRectRgn")
	procSetWindowRgn       = user32.NewProc("SetWindowRgn")
	procGetWindowRgn       = user32.NewProc("GetWindowRgn")
	procGetSystemMetrics   = user32.NewProc("GetSystemMetrics")
	procDwmSetWindowAttr   = dwmapi.NewProc("DwmSetWindowAttribute")
	procDwmExtendFrame     = dwmapi.NewProc("DwmExtendFrameIntoClientArea")
)

// HasRegion 判断窗口当前是否真的带着剪裁区域。
//
// GetWindowRgn 返回 ERROR（0）就是"没有区域"，1/2/3 都是"有"（空/简单/复杂）。
// 用途只有一个：面板的四角偶尔会变回四个直角，说明句柄上的区域被系统清掉了，
// 这时候补设一次就好（见 cmd/baozi 的 ensureShell）。
func HasRegion(hwnd uintptr) bool {
	if hwnd == 0 {
		return false
	}
	// GetWindowRgn 要把结果写进一个区域句柄里，随便造一个 1×1 的当容器。
	rgn, _, _ := procCreateRectRgn.Call(0, 0, 1, 1)
	if rgn == 0 {
		return false
	}
	defer procDeleteObject.Call(rgn)
	r, _, _ := procGetWindowRgn.Call(hwnd, rgn)
	return r != 0
}

// effectWindow 是 DWM 画非客户区时的策略。没有它，这个窗口**根本没有阴影**：
// 实测空闲时窗口外侧整条带全是桌面底色（#FFFFFF），拖动时才临时出现一圈
// #C7C7C7 渐隐的阴影。DWMNCRP_ENABLED 之后阴影常驻，而且跟着窗口区域走 ——
// 圆角区域的四个角，阴影也是圆的（实测角上 +4px 处 #EEEEEE、中段 #C3C3C3）。
const (
	dwmwaNCRenderingPolicy = 2
	dwmNCRPEnabled         = 2
	dwmNCRPDisabled        = 1
)

// DisableViewFrame 关掉 DWM 给这个窗口画的非客户区边框，**保留**边框扩展。
//
// 要的是"不要 DWM 那条边框"，但窗口仍然算"有非客户区"——这样 DWM 的窗口影
// 尽量留着（关掉非客户区渲染会把影一起关掉，实测外侧由 #C800C8 弱到 #EF00EF，
// 那一版用户看到"角上干净了，可面板发飘"）。
//
// 边框扩展的 1px 由我们自己的像素盖住，看不见；真正露出来的是圆角缺口里那一格
// 白线 —— 关掉非客户区边框之后那一格才变回真透明。
func DisableViewFrame(hwnd uintptr) bool {
	if hwnd == 0 {
		return false
	}
	off := uint32(dwmNCRPDisabled)
	r, _, _ := procDwmSetWindowAttr.Call(hwnd, dwmwaNCRenderingPolicy, uintptr(unsafe.Pointer(&off)), unsafe.Sizeof(off))
	// 边框扩展保留 1px：DWM 仍把它当"有边框的窗口"，影才有地方长。
	m := margins{Left: 1, Right: 1, Top: 1, Bottom: 1}
	rf, _, _ := procDwmExtendFrame.Call(hwnd, uintptr(unsafe.Pointer(&m)))
	return r == 0 || rf == 0
}

// DisableWindowFrame 把非客户区彻底关掉：边框不画，扩展也收回 0。
func DisableWindowFrame(hwnd uintptr) bool {
	if hwnd == 0 {
		return false
	}
	off := uint32(dwmNCRPDisabled)
	r, _, _ := procDwmSetWindowAttr.Call(hwnd, dwmwaNCRenderingPolicy, uintptr(unsafe.Pointer(&off)), unsafe.Sizeof(off))
	var m margins
	rf, _, _ := procDwmExtendFrame.Call(hwnd, uintptr(unsafe.Pointer(&m)))
	return r == 0 || rf == 0
}

// EnableWindowFrameShadow 让 DWM 给这个窗口画阴影：先开非客户区绘制，再把
// 边框往客户区里扩 1px。
//
// 两步缺一不可。这个窗口是 captionless + noRedirect 的：客户区铺满整个窗口、
// 边框宽度为 0，DWM 眼里就没有"非客户区"可画 —— 实测空闲时窗口外侧整条带
// 全是桌面底色，一点阴影都没有。扩 1px 相当于告诉它"这里有 1px 的边框"，
// DWM 才会在窗口外面投阴影。扩出来的那 1px 由我们的合成内容盖住，看不见。
func EnableWindowFrameShadow(hwnd uintptr) bool {
	if hwnd == 0 {
		return false
	}
	on := uint32(dwmNCRPEnabled)
	procDwmSetWindowAttr.Call(hwnd, dwmwaNCRenderingPolicy, uintptr(unsafe.Pointer(&on)), unsafe.Sizeof(on))
	m := margins{Left: 1, Right: 1, Top: 1, Bottom: 1}
	r, _, _ := procDwmExtendFrame.Call(hwnd, uintptr(unsafe.Pointer(&m)))
	return r == 0
}

// margins 是 DwmExtendFrameIntoClientArea 的参数（顺序：左、右、上、下）。
type margins struct {
	Left, Right, Top, Bottom int32
}

// EnableViewFrame 让 DWM 画这个窗口的非客户区（也就是给它阴影）。
//
// 每次唤出都要调：mygo 在 Show/隐藏之间会动窗口样式，DWM 那边不一定留着。
// 返回 false 说明系统不认（Win7 以前的系统没有这个属性）。
func EnableViewFrame(hwnd uintptr) bool {
	if hwnd == 0 {
		return false
	}
	v := uint32(dwmNCRPEnabled)
	r, _, _ := procDwmSetWindowAttr.Call(hwnd, dwmwaNCRenderingPolicy, uintptr(unsafe.Pointer(&v)), unsafe.Sizeof(v))
	return r == 0
}

const (
	smXVirtualScreen  = 76
	smYVirtualScreen  = 77
	smCXVirtualScreen = 78
	smCYVirtualScreen = 79
)

// RoundRegion 把窗口裁成圆角，radius 是**物理像素**的半径。
//
// Windows 10 没有 Win11 那个给窗口圆角的 DWM 属性（DWMWA_WINDOW_CORNER_PREFERENCE
// 会被直接忽略），只能自己给窗口设一个圆角区域。
//
// 这里的关键是**只拿它裁阴影**：窗口是 D3D 合成的（noRedirect），窗口区域裁不掉
// 交换链里的内容（实测：内容照旧铺满四个角），但 DWM 画阴影时是按窗口区域来的 ——
// 设了圆角区域之后，阴影的四个角跟着变圆，方角阴影压在圆角内容上的"看着还是直角"
// 就没了。内容那一层的圆角由 ClipInset 负责，两边各管一件。
// 尺寸用客户区：GetWindowRect 会多出 DWM 那圈 9px 扩展框，按它设区域会偏大。
func RoundRegion(hwnd uintptr, radius int) bool {
	if hwnd == 0 || radius <= 0 {
		return false
	}
	// 区域必须覆盖**整个窗口**，包括那圈边框带。
	//
	// 面板是"隐藏标题栏"的窗口，四边各留了一圈缩放热区（1098×638 的窗口里，
	// 右边和下边那圈是 9px，左边也是 9px，上边 0）。用户拖那圈就是在改面板大小。
	// 区域若只铺客户区，右边 18px、下边 9px 落在区域外面 —— 区域外的鼠标事件
	// 不属于这个窗口，热区等于被切掉，表现就是"从面板右侧、下侧拉不动大小，
	// 上边和左边正常"（左边（上边）从 0 起，正好不缺）。
	//
	// 区域圆角在窗口外沿，而内容层的 ClipInset 圆角在客户区，两者圆心差 9px，
	// 客户区四角会露出一小片月牙。那片月牙用的是 deskColor（从面板四周的桌面
	// 取样，见 glass.go），所以看不出来。
	_, _, w, h := WindowRectPixels(hwnd)
	if w <= 0 || h <= 0 {
		return false
	}
	// 区域就取窗口矩形本身 (0,0,w,h)：以前写的是 w+1/h+1（"右下角那条线要包进来"），
	// 结果右边和下边外侧多露出 1 个像素 —— 那一列没有内容可画，DWM 合成时填白，
	// 屏幕上就是紧贴边框外侧一道 1px 的亮线（截图实测右侧 #F5F5F5、下侧 #FFFFFF，
	// 而上边左边从 0 起、不存在这一格）。这道亮线正是"下边和右边在边框内部多一条线"。
	// 边框线本身画在窗口的最后一格（w-1），本来就在区域里面，不需要多包一格。
	//
	// 区域半径必须和内容那条弧**一样大**。
	//
	// 以前这里是 radius+2，用来把 GDI 区域的硬台阶推到弧外面去。但弧外那 2px
	// 里内容已经被内容层自己的圆角裁掉了、又是透明的，DWM 就把它填成纯白 ——
	// 屏幕上就是"圆弧比边框鼓出一圈浅色"。现在抗锯齿完全由自绘层那条斜坡负责
	// （不透明，能盖住 1 位台阶），区域和弧对齐就不会再留出那条白带。
	const regionRadiusPad = 0
	rgnRadius := radius + regionRadiusPad
	// 右边多留的那一列在**客户区之外**（窗口矩形比客户区宽 1px），里面没有内容，
	// DWM 合成时把它填成纯白 —— 以前靠自绘层描一圈边框盖着，边框去掉之后就是贴着
	// 边沿的一道白线。区域收到 w-1，这一列直接被切掉，问题从根上没了。
	// 只收右边：实测下边那格内容能盖满（(224,224,224)），收掉会少一行内容。
	rw := w
	if rw > 2 {
		rw--
	}
	rgn, _, _ := procCreateRoundRectRgn.Call(0, 0, uintptr(rw), uintptr(h), uintptr(rgnRadius*2), uintptr(rgnRadius*2))
	if rgn == 0 {
		return false
	}
	// bRedraw 传 0：这个区域只决定"DWM 阴影的形状 + 鼠标命中"，不参与内容绘制
	// （内容在交换链里，区域裁不掉）。传 1 的话每次调用都会让整个窗口失效重画，
	// 1100×724 一次 40ms 上下；缩放每帧一次、松手再来一次，那就是"缩放卡一下、
	// 停手再卡一下"。面板自己该重绘的时候会重绘，这里不必替它安排。
	r, _, _ := procSetWindowRgn.Call(hwnd, rgn, 0)
	if r == 0 {
		// 设置失败时区域的归属还在我们手上，得自己释放。
		procDeleteObject.Call(rgn)
		return false
	}
	// 成功之后区域归系统管，不能删。
	return true
}

// ClearRegion 撤掉窗口区域，窗口恢复成整个矩形。
// 目前没有调用方，留着是因为"临时量一个没有区域的形状"在排查时很方便。
func ClearRegion(hwnd uintptr) bool {
	if hwnd == 0 {
		return false
	}
	r, _, _ := procSetWindowRgn.Call(hwnd, 0, 1)
	return r != 0
}

// VirtualScreenRect 返回所有显示器拼起来的那个矩形（物理像素）。
func VirtualScreenRect() (x, y, w, h int) {
	gx, _, _ := procGetSystemMetrics.Call(smXVirtualScreen)
	gy, _, _ := procGetSystemMetrics.Call(smYVirtualScreen)
	gw, _, _ := procGetSystemMetrics.Call(smCXVirtualScreen)
	gh, _, _ := procGetSystemMetrics.Call(smCYVirtualScreen)
	return int(int32(gx)), int(int32(gy)), int(int32(gw)), int(int32(gh))
}

// Crop 从 src 里裁出 (x, y) 起的 w×h。
//
// 越界不报错，直接按边上的像素补：面板可能被拖到屏幕边缘甚至一半在屏幕外，
// 那时候裁出来的那块本来就不完整，补边比抛错好 —— 画面总得铺满。
func Crop(src *image.RGBA, x, y, w, h int) *image.RGBA {
	return CropDown(src, x, y, w, h, 1)
}

// CropDown 是 Crop 的降采样版：源图上每隔 step 个像素取一个，输出 w×h。
//
// 面板拖动时每一帧都要重裁一次，全尺寸（1290×949）光拷贝就 500 万字节，
// 加上模糊和色调根本跟不上鼠标。玻璃本来就是糊的，先降到 1/3 再糊，
// 显示时再放大回去 —— 观感没差，每帧的开销却小了一个数量级。
func CropDown(src *image.RGBA, x, y, w, h, step int) *image.RGBA {
	if src == nil || w <= 0 || h <= 0 {
		return nil
	}
	if step < 1 {
		step = 1
	}
	srcW, srcH := src.Rect.Dx(), src.Rect.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for dy := 0; dy < h; dy++ {
		sy := clampInt(y+dy*step, 0, srcH-1)
		so := src.PixOffset(0, sy)
		do := dst.PixOffset(0, dy)
		for dx := 0; dx < w; dx++ {
			sx := clampInt(x+dx*step, 0, srcW-1)
			si := so + sx*4
			di := do + dx*4
			dst.Pix[di+0] = src.Pix[si+0]
			dst.Pix[di+1] = src.Pix[si+1]
			dst.Pix[di+2] = src.Pix[si+2]
			dst.Pix[di+3] = 255
		}
	}
	return dst
}

// RoundCorners 把图像四角切成圆角：圆外的像素变全透明。
//
// 面板窗口是 DirectComposition 合成的，给它设窗口区域（SetWindowRgn）只裁得掉
// DWM 的阴影、裁不掉交换链里的内容 —— 所以圆角得自己画：把背景图四角挖空，
// 透明的地方 DWM 合成时会露出桌面，看上去就是窗口真的圆了。
func RoundCorners(img *image.RGBA, radius int) {
	if img == nil || radius <= 0 {
		return
	}
	w, h := img.Rect.Dx(), img.Rect.Dy()
	if radius*2 > w {
		radius = w / 2
	}
	if radius*2 > h {
		radius = h / 2
	}
	r := float64(radius)
	setAlpha := func(x, y int, a uint8) {
		if x < 0 || y < 0 || x >= w || y >= h {
			return
		}
		img.Pix[img.PixOffset(x, y)+3] = a
	}
	for y := 0; y < radius; y++ {
		for x := 0; x < radius; x++ {
			// 圆心在 (r, r)，比半径远的点就是圆外；边缘按距离做一点过渡，
			// 免得放大之后是一圈硬阶梯。
			dx := r - float64(x) - 0.5
			dy := r - float64(y) - 0.5
			d := dx*dx + dy*dy
			if d <= (r-1)*(r-1) {
				continue
			}
			var a uint8
			if d < r*r {
				f := (r*r - d) / (r*r - (r-1)*(r-1))
				a = uint8(255 * f)
			}
			setAlpha(x, y, a)
			setAlpha(w-1-x, y, a)
			setAlpha(x, h-1-y, a)
			setAlpha(w-1-x, h-1-y, a)
		}
	}
}
