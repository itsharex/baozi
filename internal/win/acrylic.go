package win

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// Win10 的玻璃靠 SetWindowCompositionAttribute：Win11 22H2 起系统有了
// DWMWA_SYSTEMBACKDROP_TYPE（mygo 的 vibrancy 走那条），Win10 没有那个属性，
// 只能调这个未公开、但被大量桌面程序用着的接口。
//
// 它能做两档：
//   - ACCENT_ENABLE_ACRYLICBLURBEHIND（Win10 1803+）：带噪点的磨砂，最像 Pane；
//   - ACCENT_ENABLE_BLURBEHIND：更平滑的模糊，拖动窗口时比亚克力跟手。
//
// 限制在 Win10 的合成器上：窗口必须给客户区留出透明（不铺不透明底），
// 模糊才透得出来；窗口是 layered（SetLayeredWindowAttributes）时这一套不生效。
var (
	procSetWindowCompositionAttribute = user32.NewProc("SetWindowCompositionAttribute")
	procRtlGetVersion                 = windows.NewLazySystemDLL("ntdll.dll").NewProc("RtlGetVersion")
)

const (
	wcaAccentPolicy = 19

	accentEnableBlurBehind        = 3
	accentEnableAcrylicBlurBehind = 4
)

type accentPolicy struct {
	AccentState   uint32
	AccentFlags   uint32
	GradientColor uint32
	AnimationID   uint32
}

type windowCompositionAttributeData struct {
	Attribute  uint32
	Data       unsafe.Pointer
	SizeOfData uint32
}

type osVersionInfo struct {
	OSVersionInfoSize uint32
	MajorVersion      uint32
	MinorVersion      uint32
	BuildNumber       uint32
	PlatformID        uint32
	CSDVersion        [128]uint16
}

// BuildNumber 返回当前 Windows 的 build（19045 = Win10 22H2，22000 = Win11 21H2，
// 22621 = Win11 22H2）。RtlGetVersion 不走兼容层，报的是真实版本；
// GetVersionEx 在没写 manifest 的进程里会谎报成 6.2，不能用来判断。
func BuildNumber() uint32 {
	info := osVersionInfo{OSVersionInfoSize: uint32(unsafe.Sizeof(osVersionInfo{}))}
	r, _, _ := procRtlGetVersion.Call(uintptr(unsafe.Pointer(&info)))
	if r != 0 {
		return 0
	}
	return info.BuildNumber
}

// SystemGlass 说系统自己会不会给窗口铺材质：Win11 22H2（build 22621）起会，
// 那上面交给 mygo 的 vibrancy；这以下的系统要我们自己调。
func SystemGlass() bool {
	b := BuildNumber()
	return b >= 22621
}

// Acrylic 给窗口后面铺一层磨砂。tint 是 AABBGGRR（alpha 决定这层膜有多厚，
// 0 等于没开；颜色决定偏冷还是偏暖）。acrylic 为 false 时用更平滑、更省的那档模糊。
// 返回 false 表示这层没铺上（系统太老、或这个窗口不吃这一套）。
func Acrylic(hwnd uintptr, tint uint32, acrylic bool) bool {
	if hwnd == 0 {
		return false
	}
	state := uint32(accentEnableBlurBehind)
	if acrylic {
		state = accentEnableAcrylicBlurBehind
	}
	p := accentPolicy{
		AccentState:   state,
		AccentFlags:   2, // 让 DWM 把模糊铺满客户区，而不是只沿着边框画一条
		GradientColor: tint,
	}
	data := windowCompositionAttributeData{
		Attribute:  wcaAccentPolicy,
		Data:       unsafe.Pointer(&p),
		SizeOfData: uint32(unsafe.Sizeof(p)),
	}
	r, _, _ := procSetWindowCompositionAttribute.Call(hwnd, uintptr(unsafe.Pointer(&data)))
	return r != 0
}

// ClearAcrylic 把铺上的那层撤掉（AccentState 0 = 关）。
func ClearAcrylic(hwnd uintptr) bool {
	if hwnd == 0 {
		return false
	}
	p := accentPolicy{AccentState: 0}
	data := windowCompositionAttributeData{
		Attribute:  wcaAccentPolicy,
		Data:       unsafe.Pointer(&p),
		SizeOfData: uint32(unsafe.Sizeof(p)),
	}
	r, _, _ := procSetWindowCompositionAttribute.Call(hwnd, uintptr(unsafe.Pointer(&data)))
	return r != 0
}
