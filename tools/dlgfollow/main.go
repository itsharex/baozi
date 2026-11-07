// 端到端验证「文件对话框跟随」：
// 一个进程里依次完成——把「打开」框设成前台 → 模拟双击 Ctrl 唤起 Baozi 面板 →
// 敲入关键词 → 回车 → 回头读对话框地址栏，看它是不是真的跳过去了。
//
// 为什么要一个进程做完：分几次用 PowerShell 拼，每次工具调用之间前台都会被抢回别的
// 窗口（日志里 show() 看到的前台是 Qt 客户端窗口），hook 收到按键那一刻前台不是对话框，
// 采集自然采不到。所以前台切换、发键、读取必须在同一个进程里连着做。
//
// 用法：go run ./tools/dlgfollow "lertaro"
package main

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32                     = windows.NewLazySystemDLL("user32.dll")
	kernel32                   = windows.NewLazySystemDLL("kernel32.dll")
	procEnumWindows            = user32.NewProc("EnumWindows")
	procEnumChildWin           = user32.NewProc("EnumChildWindows")
	procGetClassNameW          = user32.NewProc("GetClassNameW")
	procGetWindowTextW         = user32.NewProc("GetWindowTextW")
	procGetParent              = user32.NewProc("GetParent")
	procGetDlgCtrlID           = user32.NewProc("GetDlgCtrlID")
	procSendMessageW           = user32.NewProc("SendMessageW")
	procSetForeground          = user32.NewProc("SetForegroundWindow")
	procShowWindow             = user32.NewProc("ShowWindow")
	procIsWindowVisible        = user32.NewProc("IsWindowVisible")
	procSetFocus               = user32.NewProc("SetFocus")
	procAttachThread           = user32.NewProc("AttachThreadInput")
	procGetWindowThreadProcess = user32.NewProc("GetWindowThreadProcessId")
	procGetForegroundWindow    = user32.NewProc("GetForegroundWindow")
	procSendInput              = user32.NewProc("SendInput")
	procGetCurrentThreadID     = kernel32.NewProc("GetCurrentThreadId")
	procSleep                  = kernel32.NewProc("Sleep")
)

const (
	inputKeyboard    = 1
	keyeventfKeyup   = 0x0002
	keyeventfUnicode = 0x0004
	vkControl        = 0x11
	vkReturn         = 0x0D
	swRestore        = 9
	wmGetText        = 0x000D
)

type keyboardInput struct {
	Vk    uint16
	Scan  uint16
	Flags uint32
	Time  uint32
	Extra uintptr
}

type input struct {
	Type uint32
	_    uint32
	Ki   keyboardInput
	_    [8]byte
}

func className(h uintptr) string {
	buf := make([]uint16, 256)
	procGetClassNameW.Call(h, uintptr(unsafe.Pointer(&buf[0])), 256)
	return windows.UTF16ToString(buf)
}

func text(h uintptr) string {
	buf := make([]uint16, 1024)
	procSendMessageW.Call(h, wmGetText, 1024, uintptr(unsafe.Pointer(&buf[0])))
	return windows.UTF16ToString(buf)
}

func findFileDialog() uintptr {
	var res uintptr
	cb := syscall.NewCallback(func(h uintptr, _ uintptr) uintptr {
		if res != 0 {
			return 1
		}
		if !strings.EqualFold(className(h), "#32770") {
			return 1
		}
		// 有 Breadcrumb Parent 才算文件对话框（普通消息框没有这个容器）。
		var bread uintptr
		cb2 := syscall.NewCallback(func(c uintptr, _ uintptr) uintptr {
			if bread == 0 && strings.EqualFold(className(c), "Breadcrumb Parent") {
				bread = c
				return 0
			}
			return 1
		})
		procEnumChildWin.Call(h, cb2, 0)
		if bread != 0 {
			res = h
			return 0
		}
		return 1
	})
	procEnumWindows.Call(cb, 0)
	return res
}

// addressText 读地址栏那条 toolbar 的文本，例如 "地址: D:\work\baozi"。
func addressText(dlg uintptr) string {
	var addr uintptr
	cb := syscall.NewCallback(func(h uintptr, _ uintptr) uintptr {
		if addr != 0 {
			return 1
		}
		if !strings.EqualFold(className(h), "ToolbarWindow32") {
			return 1
		}
		id, _, _ := procGetDlgCtrlID.Call(h)
		if int32(id) != 1001 {
			return 1
		}
		parent, _, _ := procGetParent.Call(h)
		if !strings.EqualFold(className(parent), "Breadcrumb Parent") {
			return 1
		}
		addr = h
		return 0
	})
	procEnumChildWin.Call(dlg, cb, 0)
	if addr == 0 {
		return ""
	}
	return strings.TrimSpace(text(addr))
}

func findBaoziPanel() uintptr {
	var res uintptr
	cb := syscall.NewCallback(func(h uintptr, _ uintptr) uintptr {
		if res != 0 {
			return 1
		}
		if !strings.EqualFold(className(h), "Win32Window") && !strings.EqualFold(text(h), "Baozi") {
			return 1
		}
		if vis, _, _ := procIsWindowVisible.Call(h); vis != 0 {
			res = h
			return 0
		}
		return 1
	})
	procEnumWindows.Call(cb, 0)
	return res
}

func sendKey(vk uint16, up bool) {
	in := input{Type: inputKeyboard}
	in.Ki.Vk = vk
	if up {
		in.Ki.Flags = keyeventfKeyup
	}
	procSendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in))
}

func sendRune(r rune) {
	in := input{Type: inputKeyboard}
	in.Ki.Flags = keyeventfUnicode
	in.Ki.Scan = uint16(r)
	procSendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in))
	in.Ki.Flags = keyeventfUnicode | keyeventfKeyup
	procSendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in))
}

func doubleCtrl() {
	sendKey(vkControl, false)
	sendKey(vkControl, true)
	procSleep.Call(120)
	sendKey(vkControl, false)
	sendKey(vkControl, true)
}

func focus(hwnd uintptr) {
	procShowWindow.Call(hwnd, swRestore)
	cur, _, _ := procGetCurrentThreadID.Call()
	tid, _, _ := procGetWindowThreadProcess.Call(hwnd, 0)
	if cur != 0 && tid != 0 && cur != tid {
		procAttachThread.Call(cur, tid, 1)
	}
	procSetForeground.Call(hwnd)
}

func main() {
	keyword := "lertaro"
	if len(os.Args) > 1 {
		keyword = os.Args[1]
	}
	dlg := findFileDialog()
	if dlg == 0 {
		fmt.Println("没有找到文件对话框")
		os.Exit(1)
	}
	fmt.Printf("对话框 %#x 导航前地址: %q\n", dlg, addressText(dlg))

	// 先把面板收掉（若可见），再把前台交回对话框，保证唤起那一帧前台就是对话框。
	if panel := findBaoziPanel(); panel != 0 {
		focus(dlg)
		doubleCtrl()
		procSleep.Call(700)
	}
	focus(dlg)
	procSleep.Call(300)
	focus(dlg)
	doubleCtrl()
	procSleep.Call(1500)

	// 唤起后前台应当是 Baozi 面板；如果不是，说明这次抢前台失败，输入打不进面板，
	// 直接退出而不是把回车乱发到对话框上（那会把用户原来要选的文件确认掉）。
	fg, _, _ := procGetForegroundWindow.Call()
	fmt.Printf("唤起后前台 %#x cls=%q title=%q\n", fg, className(fg), text(fg))
	if fg == dlg || fg == 0 {
		fmt.Println("面板没抢到前台，这次不输入")
		os.Exit(1)
	}
	focus(fg)
	procSleep.Call(400)

	fmt.Printf("输入 %q 并回车\n", keyword)
	for _, r := range keyword {
		sendRune(r)
	}
	procSleep.Call(900)
	sendKey(vkReturn, false)
	sendKey(vkReturn, true)
	procSleep.Call(2200)

	fmt.Printf("导航后地址: %q\n", addressText(dlg))
}
