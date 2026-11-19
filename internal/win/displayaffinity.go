package win

import "golang.org/x/sys/windows"

var procSetWindowDisplayAffinity = windows.NewLazySystemDLL("user32.dll").NewProc("SetWindowDisplayAffinity")

const (
	// wdaNone 是默认：窗口照常出现在截屏/录屏里。
	wdaNone = 0x00000000
	// wdaExcludeFromCapture（Win10 2004 起）：窗口照常显示给眼睛，但屏幕捕获
	// （BitBlt / PrintScreen / 录屏）里看不到它。
	wdaExcludeFromCapture = 0x00000011
)

// SetDisplayAffinity 让某个窗口在"抓屏"里消失或恢复。
//
// 为什么需要它：子窗的玻璃底片是靠抓整屏得到的。抓的时候启动面板还挂在屏幕
// 上，于是底片里就印着一块模糊的启动面板 —— 用户看到的就是"背后明明是我的
// 桌面，玻璃里却是启动面板"。抓之前把自家窗口设成"捕获不可见"，抓完恢复，
// 底片里就只剩真正的桌面。
func SetDisplayAffinity(hwnd uintptr, exclude bool) bool {
	if hwnd == 0 {
		return false
	}
	v := uintptr(wdaNone)
	if exclude {
		v = wdaExcludeFromCapture
	}
	r, _, _ := procSetWindowDisplayAffinity.Call(hwnd, v)
	return r != 0
}
