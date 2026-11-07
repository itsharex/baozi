// tcprobe：单测 Total Commander 的 WM_COPYDATA 远程查询，比较几种投递方式。
//
// 背景：Baozi 里 TC 查询一直拿不到回复。Lertaro 的做法是
// "receiver 窗口和发送在同一线程 + 普通 SendMessage"，靠嵌套投递同步拿回复。
// 这个探针把几种组合都跑一遍，确定哪种在这台机器上真的能拿到路径。
//
// 用法：D:\Go\bin\go.exe run ./tools/tcprobe
package main

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"baozi/internal/win"
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procRegisterClassW              = user32.NewProc("RegisterClassW")
	procCreateWindowExW             = user32.NewProc("CreateWindowExW")
	procDefWindowProcW              = user32.NewProc("DefWindowProcW")
	procSendMessageW                = user32.NewProc("SendMessageW")
	procSendMessageTimeoutW         = user32.NewProc("SendMessageTimeoutW")
	procGetModuleHandleW            = kernel32.NewProc("GetModuleHandleW")
	procChangeWindowMessageFilterEx = user32.NewProc("ChangeWindowMessageFilterEx")
	procGetClassName                = user32.NewProc("GetClassNameW")
	procFindWindow                  = user32.NewProc("FindWindowW")
)

const (
	wmCopyData  = 0x004A
	getRequestW = uintptr('G') + 256*uintptr('W')
	getReplyW   = uintptr('R') + 256*uintptr('W')
	// TC 的"换目录"命令：dwData = 'C' + 256*'D'
	tcChangeDirCmd = uintptr('C') + 256*uintptr('D')
	hwndMessage    = ^uintptr(2)
	msgfltAllow    = 1
	smtoBlock      = 0x0001
	smtoAbort      = 0x0002
)

type wndClass struct {
	style         uint32
	_             uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     uintptr
	hIcon         uintptr
	hCursor       uintptr
	hbrBackground uintptr
	lpszMenuName  uintptr
	lpszClassName uintptr
}

type copyDataStruct struct {
	dwData uintptr
	cbData uint32
	_      uint32
	lpData unsafe.Pointer
}

var (
	gotPath   string
	gotAny    int
	wndProcCb = syscall.NewCallback(wndProc)
)

func wndProc(hwnd, msg, wParam, lParam uintptr) uintptr {
	if msg == wmCopyData && lParam != 0 {
		gotAny++
		// 回调参数只能是 uintptr；直接写 unsafe.Pointer(lParam) 会被 vet 判成误用，
		// 这里用位重解释把指针值取回（WM_COPYDATA 的数据是系统复制进本进程的）。
		cds := (*copyDataStruct)(*(*unsafe.Pointer)(unsafe.Pointer(&lParam)))
		fmt.Printf("   >> WndProc 收到 WM_COPYDATA dwData=%#x cbData=%d\n", cds.dwData, cds.cbData)
		if cds.dwData == getReplyW && cds.lpData != nil && cds.cbData > 0 {
			n := int(cds.cbData) / 2
			s := windows.UTF16ToString(unsafe.Slice((*uint16)(cds.lpData), n))
			gotPath = strings.TrimRight(s, "\x00")
		}
		return 1
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wParam, lParam)
	return r
}

// createReceiver 在当前 OS 线程上建一个 message-only 窗口。
func createReceiver(cls string) uintptr {
	instance, _, _ := procGetModuleHandleW.Call(0)
	name, _ := syscall.UTF16PtrFromString(cls)
	wc := wndClass{
		lpfnWndProc:   wndProcCb,
		hInstance:     instance,
		lpszClassName: uintptr(unsafe.Pointer(name)),
	}
	if r, _, err := procRegisterClassW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		fmt.Printf("   RegisterClassW 失败: %v\n", err)
	}
	hwnd, _, err := procCreateWindowExW.Call(0, uintptr(unsafe.Pointer(name)), 0, 0,
		0, 0, 0, 0, hwndMessage, 0, instance, 0)
	if hwnd == 0 {
		fmt.Printf("   CreateWindowExW 失败: %v\n", err)
	}
	procChangeWindowMessageFilterEx.Call(hwnd, wmCopyData, msgfltAllow, 0)
	return hwnd
}

func query(tc uintptr, recv uintptr, mode string) {
	gotPath, gotAny = "", 0
	cmd := []byte{'S', 'P', 0}
	cds := copyDataStruct{
		dwData: getRequestW,
		cbData: uint32(len(cmd)),
		lpData: unsafe.Pointer(&cmd[0]),
	}
	lp := uintptr(unsafe.Pointer(&cds))
	switch mode {
	case "SendMessage":
		procSendMessageW.Call(tc, wmCopyData, recv, lp)
	case "SendMessageTimeout(no BLOCK)":
		var out uintptr
		procSendMessageTimeoutW.Call(tc, wmCopyData, recv, lp, smtoAbort, 500, uintptr(unsafe.Pointer(&out)))
	case "SendMessageTimeout(BLOCK)":
		var out uintptr
		procSendMessageTimeoutW.Call(tc, wmCopyData, recv, lp, smtoBlock|smtoAbort, 500, uintptr(unsafe.Pointer(&out)))
	case "SendMessage(wParam=0)":
		procSendMessageW.Call(tc, wmCopyData, 0, lp)
	}
	fmt.Printf("   %s -> 回调次数=%d 路径=%q\n", mode, gotAny, gotPath)
}

func className(h uintptr) string {
	var buf [256]uint16
	procGetClassName.Call(h, uintptr(unsafe.Pointer(&buf[0])), 256)
	return windows.UTF16ToString(buf[:])
}

func main() {
	// 接收窗口和发送必须在同一个 OS 线程，嵌套投递才会发生。
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	tc, _, _ := procFindWindow.Call(uintptr(unsafe.Pointer(mustPtr("TTOTAL_CMD"))), 0)
	if tc == 0 {
		fmt.Println("没找到 TTOTAL_CMD 窗口，先打开 Total Commander")
		return
	}
	fmt.Printf("TC 窗口=%#x 类名=%q\n", tc, className(tc))

	// -cd <路径>：只让 TC 换目录（测试后恢复现场用）
	if len(os.Args) > 2 && os.Args[1] == "-cd" {
		target := os.Args[2]
		fmt.Printf("CD -> %q 发送=%v\n", target, tcChangeDir(tc, target))
		time.Sleep(300 * time.Millisecond)
		fmt.Printf("TC 当前目录=%q\n", win.TotalCommanderFolder(tc))
		return
	}

	recv := createReceiver("BaoziTcProbe")
	fmt.Printf("接收窗口=%#x\n", recv)
	if recv == 0 {
		return
	}

	query(tc, recv, "SendMessage")
	query(tc, recv, "SendMessageTimeout(no BLOCK)")
	query(tc, recv, "SendMessageTimeout(BLOCK)")
	query(tc, recv, "SendMessage(wParam=0)")
}

// tcChangeDir 让 Total Commander 的活动面板切到 path。
// lpData = [UTF-8 BOM]path 0x0D 0x00 "S" 0x00（尾 NUL 必须计入 cbData）。
func tcChangeDir(tc uintptr, path string) bool {
	buf := make([]byte, 0, len(path)+8)
	buf = append(buf, 0xEF, 0xBB, 0xBF)
	buf = append(buf, []byte(path)...)
	buf = append(buf, 0x0D, 0x00)
	buf = append(buf, 'S')
	buf = append(buf, 0x00)
	cds := copyDataStruct{
		dwData: tcChangeDirCmd,
		cbData: uint32(len(buf)),
		lpData: unsafe.Pointer(&buf[0]),
	}
	var out uintptr
	r, _, _ := procSendMessageTimeoutW.Call(tc, wmCopyData, 0,
		uintptr(unsafe.Pointer(&cds)), smtoAbort, 1000, uintptr(unsafe.Pointer(&out)))
	return r != 0
}

func mustPtr(s string) *uint16 {
	p, _ := syscall.UTF16PtrFromString(s)
	return p
}
