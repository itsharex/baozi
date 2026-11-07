// 打印一个对话框窗口的所有子控件：class / 控件 ID / 文本 / 父窗口 class。
// 用来确认"文件名框"到底是哪个 Edit——写进搜索框就会把回车变成"确认打开"，直接把对话框关掉。
// 用法：go run ./tools/dlgdump <hwndHex>
package main

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32            = windows.NewLazySystemDLL("user32.dll")
	procEnumChildWin  = user32.NewProc("EnumChildWindows")
	procGetClassNameW = user32.NewProc("GetClassNameW")
	procGetDlgCtrlID  = user32.NewProc("GetDlgCtrlID")
	procGetParent     = user32.NewProc("GetParent")
	procSendMessageW  = user32.NewProc("SendMessageW")
	procGetWindowRect = user32.NewProc("GetWindowRect")
)

func className(h uintptr) string {
	buf := make([]uint16, 256)
	procGetClassNameW.Call(h, uintptr(unsafe.Pointer(&buf[0])), 256)
	return windows.UTF16ToString(buf)
}

func text(h uintptr) string {
	buf := make([]uint16, 1024)
	procSendMessageW.Call(h, 0x000D, 1024, uintptr(unsafe.Pointer(&buf[0]))) // WM_GETTEXT
	return windows.UTF16ToString(buf)
}

type rect struct{ L, T, R, B int32 }

func main() {
	if len(os.Args) < 2 {
		fmt.Println("用法: dlgdump <hwndHex>")
		os.Exit(1)
	}
	v, err := strconv.ParseUint(os.Args[1], 16, 64)
	if err != nil {
		fmt.Println("hwnd 解析失败:", err)
		os.Exit(1)
	}
	hw := uintptr(v)
	cb := syscall.NewCallback(func(child uintptr, _ uintptr) uintptr {
		var id uintptr
		id, _, _ = procGetDlgCtrlID.Call(child)
		parent, _, _ := procGetParent.Call(child)
		var r rect
		procGetWindowRect.Call(child, uintptr(unsafe.Pointer(&r)))
		fmt.Printf("child=%#x cls=%-22q id=%-6d parent=%-18q rect=(%d,%d,%d,%d) text=%q\n",
			child, className(child), int32(id), className(parent), r.L, r.T, r.R, r.B, text(child))
		return 1
	})
	procEnumChildWin.Call(hw, cb, 0)
}
