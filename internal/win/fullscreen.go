// Package win 封装本项目需要的少量 Win32 能力。
// 只保留"别的包替不了"的调用：全屏判定、前台窗口探测。
package win

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32  = windows.NewLazySystemDLL("user32.dll")
	shell32 = windows.NewLazySystemDLL("shell32.dll")

	procGetForegroundWindow    = user32.NewProc("GetForegroundWindow")
	procGetWindowRect          = user32.NewProc("GetWindowRect")
	procGetMonitorInfoW        = user32.NewProc("GetMonitorInfoW")
	procMonitorFromWindow      = user32.NewProc("MonitorFromWindow")
	procGetWindowLongPtrW      = user32.NewProc("GetWindowLongPtrW")
	procIsWindowVisible        = user32.NewProc("IsWindowVisible")
	procGetClassNameW          = user32.NewProc("GetClassNameW")
	procQueryNotificationState = shell32.NewProc("SHQueryUserNotificationState")
	procGetDesktopWindow       = user32.NewProc("GetDesktopWindow")
	procSystemParametersInfoW  = user32.NewProc("SystemParametersInfoW")
	procIsZoomed               = user32.NewProc("IsZoomed")
)

// RECT 是 Win32 矩形。
type RECT struct{ Left, Top, Right, Bottom int32 }

// MONITORINFO 只取需要的字段，cbSize 由调用方填。
type MONITORINFO struct {
	CbSize    uint32
	RcMonitor RECT
	RcWork    RECT
	DwFlags   uint32
}

const (
	monitorDefaultToNearest = 2
	wsCaption               = 0x00C00000

	// SHQueryUserNotificationState 的返回值。
	qunsBusy               = 2 // 全屏应用或演示中
	qunsRunningD3DFullScrn = 3
	qunsPresentationMode   = 4
)

// GWL_STYLE 是 -16；uintptr 装不下负数，用按位补码表示。
var gwlStyleIndex = ^uintptr(15)

// 这两类窗口是桌面本身，不能算"全屏应用"。
var shellClasses = map[string]bool{
	"Progman":                true,
	"WorkerW":                true,
	"Shell_TrayWnd":          true,
	"Shell_SecondaryTrayWnd": true,
}

// Foreground 描述当前前台窗口。
type Foreground struct {
	HWND       uintptr
	Class      string
	Fullscreen bool
}

// ForegroundInfo 取前台窗口并判断是否处于全屏状态。
//
// 判定顺序：
//  1. SHQueryUserNotificationState 报告全屏/演示 → 全屏（覆盖 D3D 独占游戏）
//     注意这个 API 的签名是 HRESULT SHQueryUserNotificationState(state*)：
//     状态从出参拿，返回值是 HRESULT。之前把返回值当状态读，这里永远是垃圾值。
//  2. 窗口矩形覆盖整个显示器、没有标题栏、也不在最大化状态 → 全屏。
//     「不在最大化」这条是必须的：任务栏设成自动隐藏时，最大化窗口的矩形等于整个
//     显示器宽度（甚至越过顶边），再加上有的程序（Total Commander 就能关掉）自绘
//     标题栏、没有 WS_CAPTION，会被误判成全屏，结果双击 Ctrl 一直"忽略唤出"。
//     真正的全屏（浏览器 F11、游戏的独占/无边框全屏）是 SetWindowPos 铺满屏幕，
//     不是最大化状态，所以这条不会误伤。
//  3. 桌面/任务栏窗口永远不算
func ForegroundInfo() Foreground {
	var fg Foreground
	hw, _, _ := procGetForegroundWindow.Call()
	fg.HWND = hw
	if hw == 0 {
		return fg
	}
	if vis, _, _ := procIsWindowVisible.Call(hw); vis == 0 {
		return fg
	}
	fg.Class = className(hw)
	if shellClasses[fg.Class] {
		return fg
	}
	if desktop, _, _ := procGetDesktopWindow.Call(); hw == desktop {
		return fg
	}

	var quns uint32
	if hr, _, _ := procQueryNotificationState.Call(uintptr(unsafe.Pointer(&quns))); hr == 0 &&
		(quns == qunsBusy || quns == qunsRunningD3DFullScrn || quns == qunsPresentationMode) {
		fg.Fullscreen = true
		return fg
	}

	var wr RECT
	if ok, _, _ := procGetWindowRect.Call(hw, uintptr(unsafe.Pointer(&wr))); ok == 0 {
		return fg
	}
	mon, _, _ := procMonitorFromWindow.Call(hw, monitorDefaultToNearest)
	if mon == 0 {
		return fg
	}
	mi := MONITORINFO{CbSize: uint32(unsafe.Sizeof(MONITORINFO{}))}
	if ok, _, _ := procGetMonitorInfoW.Call(mon, uintptr(unsafe.Pointer(&mi))); ok == 0 {
		return fg
	}
	zoomed, _, _ := procIsZoomed.Call(hw)
	if zoomed != 0 {
		return fg
	}
	covers := wr.Left <= mi.RcMonitor.Left+2 && wr.Top <= mi.RcMonitor.Top+2 &&
		wr.Right >= mi.RcMonitor.Right-2 && wr.Bottom >= mi.RcMonitor.Bottom-2
	style, _, _ := procGetWindowLongPtrW.Call(hw, gwlStyleIndex)
	fg.Fullscreen = covers && (style&wsCaption == 0)
	return fg
}

func className(hw uintptr) string {
	var buf [256]uint16
	n, _, _ := procGetClassNameW.Call(hw, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 {
		return ""
	}
	return windows.UTF16ToString(buf[:n])
}

// HighContrast 判断系统是否开了高对比度，UI 需要跟随。
func HighContrast() bool {
	const spiGetHighContrast = 0x0042
	var on bool
	procSystemParametersInfoW.Call(spiGetHighContrast, 0, uintptr(unsafe.Pointer(&on)), 0)
	return on
}
