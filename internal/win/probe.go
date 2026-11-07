// 窗口结构探查的公共小工具：枚举后代、读焦点控件。
// 三个文件管理器 collector 都要用，单独放一处。
package win

import (
	"syscall"
	"unsafe"
)

var (
	procGetGUIThreadInfo = user32.NewProc("GetGUIThreadInfo")
)

// rect 是 Win32 的 RECT（四个 int32）。
type rect struct {
	Left, Top, Right, Bottom int32
}

// guiThreadInfo 是 GUITHREADINFO 的布局（只用到 hwndFocus / rcCaret）。
type guiThreadInfo struct {
	cbSize        uint32
	flags         uint32
	hwndActive    uintptr
	hwndFocus     uintptr
	hwndCapture   uintptr
	hwndMenuOwner uintptr
	hwndMoveSize  uintptr
	hwndCaret     uintptr
	rcCaret       rect
}

// childWindows 递归列出 parent 的所有后代窗口。
//
// 用 EnumChildWindows 而不是 FindWindowExW：后者只扫直接子窗口，而 Opus 的
// 文件显示容器、xplorer2 的 ComboBox 都在两三层以下。
func childWindows(parent uintptr) []uintptr {
	var out []uintptr
	if parent == 0 {
		return out
	}
	cb := syscall.NewCallback(func(child uintptr, _ uintptr) uintptr {
		out = append(out, child)
		return 1
	})
	procEnumChildWindows.Call(parent, cb, 0)
	return out
}

// focusedControl 返回键盘焦点控件（属于 hwnd 所在线程的那个）。
// 目标线程卡住时返回 0 —— GetGUIThreadInfo 不会阻塞在目标进程里。
func focusedControl(hwnd uintptr) uintptr {
	if hwnd == 0 {
		return 0
	}
	tid, _, _ := procGetWindowThreadProcess.Call(hwnd, 0)
	if tid == 0 {
		return 0
	}
	gui := guiThreadInfo{cbSize: uint32(unsafe.Sizeof(guiThreadInfo{}))}
	r, _, _ := procGetGUIThreadInfo.Call(tid, uintptr(unsafe.Pointer(&gui)))
	if r == 0 {
		return 0
	}
	return gui.hwndFocus
}
