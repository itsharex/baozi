package win

import (
	"errors"
	"image"
	"unsafe"
)

// 面板窗口是 DirectComposition 合成的（WS_EX_NOREDIRECTIONBITMAP），
// Win10 的 SetWindowCompositionAttribute 不会铺在这种窗口后面，
// 所以玻璃底只能自己抓：面板显示之前把屏幕对应那块拷下来，糊一糊再当背景画。
//
// 只抓面板自己要占的那块矩形，不碰别的地方。

var (
	procGetDC              = user32.NewProc("GetDC")
	procReleaseDC          = user32.NewProc("ReleaseDC")
	procGetClientRect      = user32.NewProc("GetClientRect")
	procClientToScreen     = user32.NewProc("ClientToScreen")
	procCreateCompatibleDC = gdi32.NewProc("CreateCompatibleDC")
	procCreateDIBSection   = gdi32.NewProc("CreateDIBSection")
	procSelectObject       = gdi32.NewProc("SelectObject")
	procBitBlt             = gdi32.NewProc("BitBlt")
	procStretchBlt         = gdi32.NewProc("StretchBlt")
	procSetStretchBltMode  = gdi32.NewProc("SetStretchBltMode")
	procDeleteDC           = gdi32.NewProc("DeleteDC")
	procGetPixel           = gdi32.NewProc("GetPixel")
)

const (
	srcCopy  = 0x00CC0020
	biRGB    = 0
	dibRGB32 = 32
	// halftone：StretchBlt 缩图前必须设的模式，否则 GDI 用"扔掉行/列"的
	// COLORONCOLOR，缩出来会有一堆锯齿和摩尔纹。
	halftone = 4
)

type bitmapInfoHeader struct {
	Size          uint32
	Width         int32
	Height        int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
}

// WindowRectPixels 返回窗口当前在屏幕上的物理像素矩形（含 DWM 的阴影边框）。
func WindowRectPixels(hwnd uintptr) (x, y, w, h int) {
	if hwnd == 0 {
		return 0, 0, 0, 0
	}
	var r rect
	r0, _, _ := procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&r)))
	if r0 == 0 {
		return 0, 0, 0, 0
	}
	return int(r.Left), int(r.Top), int(r.Right - r.Left), int(r.Bottom - r.Top)
}

// ClientRectPixels 返回窗口客户区在屏幕上的物理像素矩形。
//
// 玻璃底必须按这个抓，不能用 WindowRectPixels：窗口矩形比客户区各边多出
// 一圈 DWM 阴影（这台机器上是 9px），阴影的四角本来就是圆的，而且多出来的
// 那圈会让整张图被缩掉 1%～2% —— 铺上去就是"面板四角变圆、背景和桌面错位"。
func ClientRectPixels(hwnd uintptr) (x, y, w, h int) {
	if hwnd == 0 {
		return 0, 0, 0, 0
	}
	var r rect
	r0, _, _ := procGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&r)))
	if r0 == 0 {
		return 0, 0, 0, 0
	}
	w, h = int(r.Right-r.Left), int(r.Bottom-r.Top)
	if w <= 0 || h <= 0 {
		return 0, 0, 0, 0
	}
	pt := struct{ X, Y int32 }{}
	ok, _, _ := procClientToScreen.Call(hwnd, uintptr(unsafe.Pointer(&pt)))
	if ok == 0 {
		return 0, 0, 0, 0
	}
	return int(pt.X), int(pt.Y), w, h
}

// CaptureScreenScaled 抓屏幕上的 (x, y, w, h) 那块，同时直接缩成 1/step。
//
// 玻璃底本来就要糊，全尺寸抓下来再缩纯属浪费：整屏 2560×1440 抓一次要
// 14.7MB 内存，而且 Go 的大对象堆不还给系统，唤出几次内存就涨一截。
// 这里让 GDI 在拷屏的同时缩小（HALFTONE 采样，比事后抽点平滑得多），
// 只分配缩略图那点内存 —— 玻璃图越小，后面的模糊也越快。
func CaptureScreenScaled(x, y, w, h, step int) (*image.RGBA, error) {
	if step < 1 {
		step = 1
	}
	dw, dh := w/step, h/step
	if dw <= 0 || dh <= 0 || w <= 0 || h <= 0 {
		return nil, errors.New("win: 抓图区域为空")
	}
	screenDC, _, _ := procGetDC.Call(0)
	if screenDC == 0 {
		return nil, errors.New("win: 拿不到屏幕 DC")
	}
	defer procReleaseDC.Call(screenDC)

	memDC, _, _ := procCreateCompatibleDC.Call(screenDC)
	if memDC == 0 {
		return nil, errors.New("win: 建不了内存 DC")
	}
	defer procDeleteDC.Call(memDC)

	bmi := struct {
		Header bitmapInfoHeader
		Colors [256]uint32
	}{}
	bmi.Header = bitmapInfoHeader{
		Size:        uint32(unsafe.Sizeof(bitmapInfoHeader{})),
		Width:       int32(dw),
		Height:      int32(-dh),
		Planes:      1,
		BitCount:    dibRGB32,
		Compression: biRGB,
		SizeImage:   uint32(dw * dh * 4),
	}
	var bits unsafe.Pointer
	bmp, _, _ := procCreateDIBSection.Call(screenDC, uintptr(unsafe.Pointer(&bmi)), 1,
		uintptr(unsafe.Pointer(&bits)), 0, 0)
	if bmp == 0 || bits == nil {
		return nil, errors.New("win: 建不了 DIB 位图")
	}
	// 位图归到我们的 DC 之后由 DeleteObject 释放。
	defer procDeleteObject.Call(bmp)

	old, _, _ := procSelectObject.Call(memDC, bmp)
	defer procSelectObject.Call(memDC, old)

	procSetStretchBltMode.Call(memDC, halftone)

	ok, _, _ := procStretchBlt.Call(memDC, 0, 0, uintptr(dw), uintptr(dh),
		screenDC, uintptr(x), uintptr(y), uintptr(w), uintptr(h), srcCopy)
	if ok == 0 {
		return nil, errors.New("win: StretchBlt 失败")
	}

	src := unsafe.Slice((*byte)(bits), dw*dh*4)
	img := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for i := 0; i < dw*dh; i++ {
		s := i * 4
		img.Pix[s+0] = src[s+2]
		img.Pix[s+1] = src[s+1]
		img.Pix[s+2] = src[s+0]
		img.Pix[s+3] = 255
	}
	return img, nil
}

// CaptureRect 把屏幕上 (x, y) 起 w×h 的一块抓成 RGBA。
// 屏幕坐标是物理像素，不是 DIP。
//
// 走的是 CreateDIBSection：位图画在可以直接读的内存里，BitBlt 之后不用再
// GetDIBits 拷一次 —— 全屏那么大（2048×1112）的时候 GetDIBits 会返回 0，
// 小窗口却没事，所以统一换成这条更稳的路。
func CaptureRect(x, y, w, h int) (*image.RGBA, error) {
	if w <= 0 || h <= 0 {
		return nil, errors.New("win: 抓图区域为空")
	}
	screenDC, _, _ := procGetDC.Call(0)
	if screenDC == 0 {
		return nil, errors.New("win: 拿不到屏幕 DC")
	}
	defer procReleaseDC.Call(screenDC)

	memDC, _, _ := procCreateCompatibleDC.Call(screenDC)
	if memDC == 0 {
		return nil, errors.New("win: 建不了内存 DC")
	}
	defer procDeleteDC.Call(memDC)

	// 32 位 top-down DIB：负高度表示第一行就是屏幕最上面那行。
	bmi := struct {
		Header bitmapInfoHeader
		Colors [256]uint32
	}{}
	bmi.Header = bitmapInfoHeader{
		Size:        uint32(unsafe.Sizeof(bitmapInfoHeader{})),
		Width:       int32(w),
		Height:      int32(-h),
		Planes:      1,
		BitCount:    dibRGB32,
		Compression: biRGB,
		SizeImage:   uint32(w * h * 4),
	}
	var bits unsafe.Pointer
	bmp, _, _ := procCreateDIBSection.Call(screenDC, uintptr(unsafe.Pointer(&bmi)), 1, /* DIB_RGB_COLORS */
		uintptr(unsafe.Pointer(&bits)), 0, 0)
	if bmp == 0 || bits == nil {
		return nil, errors.New("win: 建不了 DIB 位图")
	}
	defer procDeleteObject.Call(bmp)

	old, _, _ := procSelectObject.Call(memDC, bmp)
	defer procSelectObject.Call(memDC, old)

	// 从屏幕直接拷到内存位图：这块区域此刻不属于 Baozi（面板还没显示）。
	ok, _, _ := procBitBlt.Call(memDC, 0, 0, uintptr(w), uintptr(h), screenDC, uintptr(x), uintptr(y), srcCopy)
	if ok == 0 {
		return nil, errors.New("win: BitBlt 失败")
	}

	// DIB 里是 BGRA（A 那字节没用），补成 255 给 RGBA。
	src := unsafe.Slice((*byte)(bits), w*h*4)
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < w*h; i++ {
		s := i * 4
		d := i * 4
		img.Pix[d+0] = src[s+2] // R
		img.Pix[d+1] = src[s+1] // G
		img.Pix[d+2] = src[s+0] // B
		img.Pix[d+3] = 255
	}
	return img, nil
}

// Blur 做几遍分离式盒式模糊（水平、垂直各一遍算一轮）。
// 半径越大越糊；三遍下来已经很像高斯，比真高斯省事得多。
func Blur(img *image.RGBA, radius, passes int) *image.RGBA {
	if img == nil || radius < 1 || passes < 1 {
		return img
	}
	w, h := img.Rect.Dx(), img.Rect.Dy()
	tmp := image.NewRGBA(image.Rect(0, 0, w, h))
	out := img
	for p := 0; p < passes; p++ {
		boxBlurH(out, tmp, w, h, radius)
		boxBlurV(tmp, out, w, h, radius)
	}
	return out
}

func boxBlurH(src, dst *image.RGBA, w, h, r int) {
	div := 2*r + 1
	for y := 0; y < h; y++ {
		var sr, sg, sb int
		row := y * src.Stride
		// 头部的窗口：左边越界按第一像素算
		for k := -r; k <= r; k++ {
			x := clampInt(k, 0, w-1)
			o := row + x*4
			sr += int(src.Pix[o])
			sg += int(src.Pix[o+1])
			sb += int(src.Pix[o+2])
		}
		for x := 0; x < w; x++ {
			o := y*dst.Stride + x*4
			dst.Pix[o] = uint8(sr / div)
			dst.Pix[o+1] = uint8(sg / div)
			dst.Pix[o+2] = uint8(sb / div)
			dst.Pix[o+3] = 255
			// 滑一格：出左边界的那个像素、进右边界的那个像素
			out := clampInt(x-r, 0, w-1)
			oo := row + out*4
			in := clampInt(x+r+1, 0, w-1)
			ii := row + in*4
			sr += int(src.Pix[ii]) - int(src.Pix[oo])
			sg += int(src.Pix[ii+1]) - int(src.Pix[oo+1])
			sb += int(src.Pix[ii+2]) - int(src.Pix[oo+2])
		}
	}
}

func boxBlurV(src, dst *image.RGBA, w, h, r int) {
	div := 2*r + 1
	for x := 0; x < w; x++ {
		var sr, sg, sb int
		for k := -r; k <= r; k++ {
			y := clampInt(k, 0, h-1)
			o := y*src.Stride + x*4
			sr += int(src.Pix[o])
			sg += int(src.Pix[o+1])
			sb += int(src.Pix[o+2])
		}
		for y := 0; y < h; y++ {
			o := y*dst.Stride + x*4
			dst.Pix[o] = uint8(sr / div)
			dst.Pix[o+1] = uint8(sg / div)
			dst.Pix[o+2] = uint8(sb / div)
			dst.Pix[o+3] = 255
			out := clampInt(y-r, 0, h-1)
			oo := out*src.Stride + x*4
			in := clampInt(y+r+1, 0, h-1)
			ii := in*src.Stride + x*4
			sr += int(src.Pix[ii]) - int(src.Pix[oo])
			sg += int(src.Pix[ii+1]) - int(src.Pix[oo+1])
			sb += int(src.Pix[ii+2]) - int(src.Pix[oo+2])
		}
	}
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

// Tint 在图上盖一层半透明色（r,g,b 各 0-255，alpha 0-255，越大越不透明）。
// 玻璃要压暗一点，字才压得住背景。
func Tint(img *image.RGBA, r, g, b, alpha uint8) *image.RGBA {
	if img == nil {
		return nil
	}
	// 先把颜色乘上 alpha，省掉每个像素再算一次
	cr := float32(r) * float32(alpha) / 255
	cg := float32(g) * float32(alpha) / 255
	cb := float32(b) * float32(alpha) / 255
	keep := float32(255-alpha) / 255
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i+0] = uint8(float32(img.Pix[i+0])*keep + cr)
		img.Pix[i+1] = uint8(float32(img.Pix[i+1])*keep + cg)
		img.Pix[i+2] = uint8(float32(img.Pix[i+2])*keep + cb)
		img.Pix[i+3] = 255
	}
	return img
}

// ScreenPixel 读屏幕上一个物理像素的颜色。
//
// 面板显示之前调用，那个位置读到的就是桌面。走屏幕 DC 直接读，而不是"抓全屏 →
// 缩成缩略 → 再取样"：缩略是 glassStep（5 像素）步长的平均，和角上那一点的真实
// 桌面色能差出十几级。而 Windows 的窗口区域是 1 位掩码、四角没有抗锯齿，圆角的
// 硬边只能靠"两侧同色"藏起来 —— 取样偏了，台阶就露出来。
func ScreenPixel(x, y int) (uint8, uint8, uint8, bool) {
	dc, _, _ := procGetDC.Call(0)
	if dc == 0 {
		return 0, 0, 0, false
	}
	defer procReleaseDC.Call(0, dc)
	v, _, _ := procGetPixel.Call(dc, uintptr(int32(x)), uintptr(int32(y)))
	if v == 0xFFFFFFFF { // CLR_INVALID
		return 0, 0, 0, false
	}
	// COLORREF 是 0x00BBGGRR。
	return uint8(v), uint8(v >> 8), uint8(v >> 16), true
}
