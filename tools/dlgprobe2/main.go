// 文件对话框导航探针：在真实进程里用 internal/win 的那套调用，
// 把一个已打开的"打开/另存为"对话框导到指定目录。
// 用法：go run ./tools/dlgprobe2 [path]
package main

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"baozi/internal/win"

	"golang.org/x/sys/windows"
)

var (
	user32                       = windows.NewLazySystemDLL("user32.dll")
	procEnumWindows              = user32.NewProc("EnumWindows")
	procEnumChildWin             = user32.NewProc("EnumChildWindows")
	procGetClassNameW            = user32.NewProc("GetClassNameW")
	procGetWindowTextW           = user32.NewProc("GetWindowTextW")
	procGetWindowThreadProcessID = user32.NewProc("GetWindowThreadProcessId")
	procGetDlgCtrlID             = user32.NewProc("GetDlgCtrlID")
	procGetParent                = user32.NewProc("GetParent")
	procGetWindowRect            = user32.NewProc("GetWindowRect")
	procSendMessageW             = user32.NewProc("SendMessageW")
)

type winRect struct{ L, T, R, B int32 }

// dumpChildren 打印对话框的所有子控件：确认"文件名框"是哪一个 Edit。
// 写进搜索框会把回车变成"确认打开"，直接把对话框关掉。
func dumpChildren(hw uintptr) {
	cb := syscall.NewCallback(func(child uintptr, _ uintptr) uintptr {
		id, _, _ := procGetDlgCtrlID.Call(child)
		parent, _, _ := procGetParent.Call(child)
		var r winRect
		procGetWindowRect.Call(child, uintptr(unsafe.Pointer(&r)))
		buf := make([]uint16, 1024)
		procSendMessageW.Call(child, 0x000D, 1024, uintptr(unsafe.Pointer(&buf[0])))
		fmt.Printf("  child=%#x cls=%-20q id=%-5d parent=%-18q rect=(%d,%d,%d,%d) text=%q\n",
			child, className(child), int32(id), className(parent), r.L, r.T, r.R, r.B, windows.UTF16ToString(buf))
		return 1
	})
	procEnumChildWin.Call(hw, cb, 0)
}

type topWindow struct {
	hwnd  uintptr
	cls   string
	title string
	pid   uint32
	bread bool
}

func className(h uintptr) string {
	buf := make([]uint16, 256)
	procGetClassNameW.Call(h, uintptr(unsafe.Pointer(&buf[0])), 256)
	return windows.UTF16ToString(buf)
}

func title(h uintptr) string {
	buf := make([]uint16, 512)
	procGetWindowTextW.Call(h, uintptr(unsafe.Pointer(&buf[0])), 512)
	return windows.UTF16ToString(buf)
}

func hasBreadcrumb(h uintptr) bool {
	cls, _ := syscall.UTF16PtrFromString("Breadcrumb Parent")
	found := false
	cb := syscall.NewCallback(func(child uintptr, _ uintptr) uintptr {
		c := className(child)
		if strings.EqualFold(c, "Breadcrumb Parent") {
			found = true
			return 0
		}
		return 1
	})
	_ = cls
	procEnumChildWin.Call(h, cb, 0)
	return found
}

func tops() []topWindow {
	var list []topWindow
	cb := syscall.NewCallback(func(h uintptr, _ uintptr) uintptr {
		var pid uint32
		procGetWindowThreadProcessID.Call(h, uintptr(unsafe.Pointer(&pid)))
		list = append(list, topWindow{hwnd: h, cls: className(h), title: title(h), pid: pid, bread: hasBreadcrumb(h)})
		return 1
	})
	procEnumWindows.Call(cb, 0)
	return list
}

func main() {
	listOnly := false
	target := `D:\work\baozi`
	for _, a := range os.Args[1:] {
		if a == "--list" {
			listOnly = true
			continue
		}
		target = a
	}
	for _, w := range tops() {
		if w.title == "" && w.cls == "" {
			continue
		}
		if strings.EqualFold(w.cls, "#32770") || w.bread {
			fmt.Printf("  hwnd=%#x cls=%q title=%q pid=%d breadcrumb=%v\n", w.hwnd, w.cls, w.title, w.pid, w.bread)
		}
	}
	var dialogs []uintptr
	for _, w := range tops() {
		if win.IsFileDialog(w.hwnd) {
			dialogs = append(dialogs, w.hwnd)
		}
	}
	fmt.Printf("文件对话框 %d 个\n", len(dialogs))
	if len(dialogs) == 0 {
		os.Exit(1)
	}
	if listOnly {
		for _, h := range dialogs {
			fmt.Printf("对话框 %#x 的子控件：\n", h)
			dumpChildren(h)
		}
		return
	}
	hw := dialogs[0]
	edit := findEditText(hw)
	fmt.Printf("导航前文件名框 = %q\n", edit)
	ok := win.NavigateDialog(hw, target)
	fmt.Printf("navigate %q -> %v\n", target, ok)
	time.Sleep(1200 * time.Millisecond)
	fmt.Printf("导航后仍在: %v（title=%q）\n", win.IsFileDialog(hw), win.WindowText(hw))
	fmt.Printf("导航后文件名框 = %q（被外壳清空说明路径已被消费、目录跳过去了）\n", findEditText(hw))
}

// findEditText 读对话框里"文件名"框当前内容（第一个 Edit 子控件，id 1148 那个）。
func findEditText(hw uintptr) string {
	var first uintptr
	cb := syscall.NewCallback(func(child uintptr, _ uintptr) uintptr {
		if first == 0 && strings.EqualFold(className(child), "Edit") {
			first = child
			return 0
		}
		return 1
	})
	procEnumChildWin.Call(hw, cb, 0)
	if first == 0 {
		return "<没有 Edit>"
	}
	buf := make([]uint16, 1024)
	procSendMessageW.Call(first, 0x000D, 1024, uintptr(unsafe.Pointer(&buf[0])))
	return windows.UTF16ToString(buf)
}
