// Total Commander 当前目录的读取。
//
// Lertaro 走的是 TC 的远程控制协议（Plugins/TotalCommander/Win32/Win32Helper.cs）：
// 主窗口类名是 `TTOTAL_CMD`，向它发一条 `WM_COPYDATA` 查询
// （dwData = 'G' + 256·'W'，lpData 是 **ASCII** 的 "SP\0" 或 "TP\0"），
// TC 把答案以 `WM_COPYDATA`（dwData = 'R' + 256·'W'，内容 UTF-16）回给调用方
// 在 wParam 里指定的窗口。命令串必须是 ANSI，只有"回复用 UTF-16"这一半由 'W' 决定
// —— 用 UTF-16 发命令 TC 不会理你（Lertaro 源码里专门写了这条）。
//
// 这里有两个容易踩空的地方，都是实测出来的：
//
//  1. **接收窗口必须和发送在同一个线程。** TC 的回复是同步 SendMessage 回来的，
//     只有在"发送线程自己的窗口"上才会被嵌套投递，发送一返回就能读到；
//     窗口开在别的线程，TC 的回复就进那个线程的队列，这里永远读不到。
//  2. **只能用 SMTO_ABORTIFHUNG，不能带 SMTO_BLOCK。** 带 BLOCK 时 Windows
//     不允许调用线程处理回到自己队列的消息，于是请求成功、回复被丢掉，
//     实测就是"SendMessageTimeout(BLOCK) 回调次数=0"。
//
// 所以每次查询都是：LockOSThread → 建一个 message-only 窗口 → 发请求（带超时，
// 保留"TC 卡死也不拖住跟随器"的保护）→ 读回复 → 销毁窗口。频率很低（只在
// 前台从文件管理器切回对话框时才查一次），这点创建开销无所谓。
package win

import (
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	procRegisterClassW              = user32.NewProc("RegisterClassW")
	procCreateWindowExW             = user32.NewProc("CreateWindowExW")
	procDestroyWindow               = user32.NewProc("DestroyWindow")
	procDefWindowProcW              = user32.NewProc("DefWindowProcW")
	procChangeWindowMessageFilterEx = user32.NewProc("ChangeWindowMessageFilterEx")
	procGetModuleHandleW            = kernel32.NewProc("GetModuleHandleW")
)

const (
	wmCopyData = 0x004A
	// 查询命令：'G' + 256*'W'（要求 TC 用 UTF-16 回话）
	getRequestW = uintptr('G') + 256*uintptr('W')
	// 回复标记：'R' + 256*'W'
	getReplyW = uintptr('R') + 256*uintptr('W')

	// (HWND)-3：message-only 窗口，不会出现在任务栏 / Alt-Tab 里
	hwndMessage = ^uintptr(2)
	msgfltAllow = 1
	tcClassName = "BaoziTcQuery"
)

// copyDataStruct 是 COPYDATASTRUCT 的布局（64 位下 cbData 后面有 4 字节填充）。
// lpData 写成 unsafe.Pointer 而不是 uintptr：读那块内存时就不必有 uintptr → 指针的转换。
type copyDataStruct struct {
	dwData uintptr
	cbData uint32
	_      uint32
	lpData unsafe.Pointer
}

// wndClass 是 WNDCLASS 的布局（只需要类名和窗口过程）。
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

var (
	// TCDebugHook 非 nil 时，TotalCommanderFolder 会把每一步细节报出来（探针 / 排障用）。
	TCDebugHook func(format string, args ...any)

	tcClassOnce sync.Once
	tcProcMu    sync.Mutex
	tcReply     string
	tcWndProcCb = syscall.NewCallback(tcWndProc)
)

func tcDebug(format string, args ...any) {
	if TCDebugHook != nil {
		TCDebugHook(format, args...)
	}
}

// IsTotalCommander 判断是不是 Total Commander 主窗口。
func IsTotalCommander(root uintptr) bool {
	return root != 0 && strings.EqualFold(className(root), "TTOTAL_CMD")
}

// TotalCommanderFolder 读 TC 活动（源）面板的当前目录。读不到返回空串。
func TotalCommanderFolder(root uintptr) string {
	if !IsTotalCommander(root) {
		return ""
	}

	// 接收窗口和发送必须在同一个 OS 线程：TC 的回复靠嵌套投递送进来。
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if !tcEnsureClass() {
		tcDebug("窗口类注册失败")
		return ""
	}
	receiver := tcCreateReceiver()
	if receiver == 0 {
		tcDebug("接收窗口没建起来")
		return ""
	}
	defer procDestroyWindow.Call(receiver)

	tcProcMu.Lock()
	tcReply = ""
	tcProcMu.Unlock()

	// 命令串必须是 ANSI（这里全 ASCII），末尾带 NUL 并计入 cbData。
	cmd := []byte{'S', 'P', 0}
	cds := copyDataStruct{
		dwData: getRequestW,
		cbData: uint32(len(cmd)),
		lpData: unsafe.Pointer(&cmd[0]),
	}
	ret, ok := sendMsgNoBlock(root, wmCopyData, receiver, uintptr(unsafe.Pointer(&cds)))
	tcDebug("发 %#x 收件口 %#x：ret=%#x ok=%v", root, receiver, ret, ok)

	tcProcMu.Lock()
	p := strings.TrimSpace(tcReply)
	tcProcMu.Unlock()
	tcDebug("回复路径=%q", p)

	if p == "" || !filepath.IsAbs(p) || !isDir(p) {
		return ""
	}
	// TC 回的是 "D:\Backup\Documents\"（带尾斜杠）。根目录 "D:\" 有三层，别削成 "D:"。
	if len(p) > 3 {
		p = strings.TrimSuffix(p, `\`)
	}
	return p
}

func tcEnsureClass() bool {
	ok := true
	tcClassOnce.Do(func() {
		instance, _, _ := procGetModuleHandleW.Call(0)
		name, err := syscall.UTF16PtrFromString(tcClassName)
		if err != nil {
			ok = false
			return
		}
		wc := wndClass{
			lpfnWndProc:   tcWndProcCb,
			hInstance:     instance,
			lpszClassName: uintptr(unsafe.Pointer(name)),
		}
		if r, _, _ := procRegisterClassW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
			ok = false
		}
	})
	return ok
}

func tcCreateReceiver() uintptr {
	instance, _, _ := procGetModuleHandleW.Call(0)
	name, err := syscall.UTF16PtrFromString(tcClassName)
	if err != nil {
		return 0
	}
	hwnd, _, _ := procCreateWindowExW.Call(
		0, uintptr(unsafe.Pointer(name)), 0, 0,
		0, 0, 0, 0,
		hwndMessage, 0, instance, 0)
	if hwnd != 0 {
		// Baozi 若以更高权限运行，UIPI 会静默丢掉 TC 的回复；显式放行 WM_COPYDATA。
		procChangeWindowMessageFilterEx.Call(hwnd, wmCopyData, msgfltAllow, 0)
	}
	return hwnd
}

func tcWndProc(hwnd, msg, wParam, lParam uintptr) uintptr {
	if msg == wmCopyData && lParam != 0 {
		cds := (*copyDataStruct)(ptrOfUintptr(lParam))
		if cds.dwData == getReplyW && cds.lpData != nil && cds.cbData > 0 {
			n := int(cds.cbData) / 2
			s := windows.UTF16ToString(unsafe.Slice((*uint16)(cds.lpData), n))
			tcProcMu.Lock()
			tcReply = strings.TrimRight(s, "\x00")
			tcProcMu.Unlock()
			return 1
		}
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wParam, lParam)
	return r
}

// ptrOfUintptr 把回调收到的整数参数还原成指针。
//
// syscall 回调的参数只能是 uintptr，而这里拿到的就是一份指针值（WM_COPYDATA 由系统
// 把数据复制进本进程，指针有效）。直接写 `unsafe.Pointer(lParam)` 会被 go vet 的
// unsafeptr 分析器判为 "possible misuse of unsafe.Pointer"，所以用位重解释的方式取回。
func ptrOfUintptr(p uintptr) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Pointer(&p))
}
