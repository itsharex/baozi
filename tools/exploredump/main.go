// 读资源管理器（CabinetWClass）当前目录的探针。
//
// 文件对话框的地址栏是 `Breadcrumb Parent → ToolbarWindow32`，它的 window text 就是
// "地址: D:\work\baozi" 这种完整路径。资源管理器的地址栏是同一个控件，
// 先确认它也能这么读——能读的话就不用碰 ShellWindows COM（Go 里手搓 IDispatch 又贵又脆）。
//
// 用法：go run ./tools/exploredump
package main

import (
	"fmt"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32              = windows.NewLazySystemDLL("user32.dll")
	procEnumWindows     = user32.NewProc("EnumWindows")
	procEnumChildWin    = user32.NewProc("EnumChildWindows")
	procGetClassNameW   = user32.NewProc("GetClassNameW")
	procGetWindowTextW  = user32.NewProc("GetWindowTextW")
	procGetParent       = user32.NewProc("GetParent")
	procGetDlgCtrlID    = user32.NewProc("GetDlgCtrlID")
	procGetAncestor     = user32.NewProc("GetAncestor")
	procSendMessageW    = user32.NewProc("SendMessageW")
	procIsWindowVisible = user32.NewProc("IsWindowVisible")
)

const (
	wmGetText   = 0x000D
	gaRootOwner = 2
)

func className(h uintptr) string {
	buf := make([]uint16, 256)
	procGetClassNameW.Call(h, uintptr(unsafe.Pointer(&buf[0])), 256)
	return windows.UTF16ToString(buf)
}

func winText(h uintptr) string {
	buf := make([]uint16, 1024)
	procGetWindowTextW.Call(h, uintptr(unsafe.Pointer(&buf[0])), 1024)
	return strings.TrimSpace(windows.UTF16ToString(buf))
}

func text(h uintptr) string {
	buf := make([]uint16, 1024)
	procSendMessageW.Call(h, wmGetText, 1024, uintptr(unsafe.Pointer(&buf[0])))
	return strings.TrimSpace(windows.UTF16ToString(buf))
}

// breadcrumbPath 找 Breadcrumb Parent 下的 ToolbarWindow32，返回去掉 "地址: " 前缀的文本。
func breadcrumbPath(hw uintptr) string {
	var path string
	probe := func() {
		var toolbar uintptr
		cb := syscall.NewCallback(func(c uintptr, _ uintptr) uintptr {
			if toolbar != 0 {
				return 1
			}
			if !strings.EqualFold(className(c), "ToolbarWindow32") {
				return 1
			}
			parent, _, _ := procGetParent.Call(c)
			if !strings.EqualFold(className(parent), "Breadcrumb Parent") {
				return 1
			}
			toolbar = c
			return 0
		})
		procEnumChildWin.Call(hw, cb, 0)
		if toolbar == 0 {
			return
		}
		t := text(toolbar)
		t = strings.TrimPrefix(t, "地址: ")
		t = strings.TrimPrefix(t, "Address: ")
		path = t
	}
	probe()
	return path
}

func main() {
	cb := syscall.NewCallback(func(h uintptr, _ uintptr) uintptr {
		if vis, _, _ := procIsWindowVisible.Call(h); vis == 0 {
			return 1
		}
		cls := className(h)
		if !strings.EqualFold(cls, "CabinetWClass") && !strings.EqualFold(cls, "ExploreWClass") {
			return 1
		}
		title := winText(h)
		root, _, _ := procGetAncestor.Call(h, gaRootOwner)
		fmt.Printf("hw=%#x cls=%q title=%q rootOwner=%#x(%q)\n", h, cls, title, root, className(root))
		fmt.Printf("    面包屑路径 = %q\n", breadcrumbPath(h))
		return 1
	})
	procEnumWindows.Call(cb, 0)

	// 顺便看看被 shell 嵌进去的子窗口（多标签时活动标签在 ShellTabWindowClass 里）
	fmt.Println("— 活动标签探测 —")
	cb2 := syscall.NewCallback(func(h uintptr, _ uintptr) uintptr {
		if vis, _, _ := procIsWindowVisible.Call(h); vis == 0 {
			return 1
		}
		cls := className(h)
		if !strings.EqualFold(cls, "CabinetWClass") {
			return 1
		}
		cb3 := syscall.NewCallback(func(c uintptr, _ uintptr) uintptr {
			if strings.EqualFold(className(c), "ShellTabWindowClass") {
				fmt.Printf("  tab=%#x title=%q 面包屑=%q\n", c, winText(c), breadcrumbPath(c))
			}
			return 1
		})
		procEnumChildWin.Call(h, cb3, 0)
		return 1
	})
	procEnumWindows.Call(cb2, 0)
}
