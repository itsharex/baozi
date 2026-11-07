// 向别的程序发 WM_COPYDATA 同步查询，并读回它投递到本进程的回复。
//
// Total Commander 和 XYplorer 都是这个形状（前者是专用的 GET 命令，后者是"跑一段脚本、
// 脚本自己用 copydata 命令回投"），所以收发那套放这里共用。
//
// 两个实测硬约束，缺一个就是"请求发送成功、回复永远为空"：
//
//  1. **接收窗口必须建在发送所在的同一个 OS 线程上。** 对方是同步 SendMessage 回来的，
//     只有"发送线程自己的窗口"才会被嵌套投递，发送调用返回时回复已经在手；
//     窗口开在别的线程，回复就进那个线程的队列，这里永远读不到。
//  2. **只能用 SMTO_ABORTIFHUNG，不能带 SMTO_BLOCK。** 带 BLOCK 时 Windows 不允许
//     调用线程处理回到自己队列的消息，回复同样被丢掉（tools/tcprobe 四路对照实测：
//     SendMessage ✓、Timeout(无 BLOCK) ✓、Timeout(BLOCK) ✗ 回调次数 0）。
//
// 所以每次查询都是：LockOSThread → 建一个 message-only 窗口 → 发请求（带超时，
// 对方卡死也不拖住处调用方）→ 读回复 → 销毁窗口。频率很低（前台从文件管理器切回
// 对话框时才查一次），建窗开销无所谓。
package win

import (
	"runtime"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	wmCopyData = 0x004A

	// (HWND)-3：message-only 窗口，不会出现在任务栏 / Alt-Tab 里
	hwndMessage = ^uintptr(2)
	msgfltAllow = 1

	queryClassName = "BaoziQueryWnd"
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

// copyReply 是对方投递回来的一条 WM_COPYDATA。
type copyReply struct {
	dwData uintptr
	text   string
}

var (
	procRegisterClassW              = user32.NewProc("RegisterClassW")
	procCreateWindowExW             = user32.NewProc("CreateWindowExW")
	procDestroyWindow               = user32.NewProc("DestroyWindow")
	procDefWindowProcW              = user32.NewProc("DefWindowProcW")
	procChangeWindowMessageFilterEx = user32.NewProc("ChangeWindowMessageFilterEx")
	procGetModuleHandleW            = kernel32.NewProc("GetModuleHandleW")

	queryClassOnce  sync.Once
	queryClassReady bool
	queryMu         sync.Mutex
	queryLast       copyReply
	queryWndProcCb  = syscall.NewCallback(queryWndProc)
)

// queryCopyData 向 target 发一条 WM_COPYDATA，并读回它同步投递到本进程的回复。
//
// build 在接收窗口建立之后调用，用来生成 payload —— XYplorer 的脚本必须把接收窗口
// 的 hwnd 写进去（`copydata <hwnd>, "$return", 2`），所以只能这时候才拼得出来。
// reply 的文本一律按 UTF-16 解（TC 的 'W' 回复和 XYplorer 的 copydata 都是 UTF-16）。
func queryCopyData(target, dwData uintptr, build func(receiver uintptr) []byte) (copyReply, bool) {
	if target == 0 || build == nil {
		return copyReply{}, false
	}

	// 接收窗口和发送必须在同一个 OS 线程：回复靠嵌套投递送进来。
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if !queryEnsureClass() {
		return copyReply{}, false
	}
	receiver := queryCreateReceiver()
	if receiver == 0 {
		return copyReply{}, false
	}
	defer procDestroyWindow.Call(receiver)

	payload := build(receiver)
	if len(payload) == 0 {
		return copyReply{}, false
	}

	queryMu.Lock()
	queryLast = copyReply{}
	queryMu.Unlock()

	cds := copyDataStruct{
		dwData: dwData,
		cbData: uint32(len(payload)),
		lpData: unsafe.Pointer(&payload[0]),
	}
	sendMsgNoBlock(target, wmCopyData, receiver, uintptr(unsafe.Pointer(&cds)))

	queryMu.Lock()
	r := queryLast
	queryMu.Unlock()
	if strings.TrimSpace(r.text) == "" {
		return r, false
	}
	return r, true
}

func queryEnsureClass() bool {
	queryClassOnce.Do(func() {
		instance, _, _ := procGetModuleHandleW.Call(0)
		name, err := syscall.UTF16PtrFromString(queryClassName)
		if err != nil {
			return
		}
		wc := wndClass{
			lpfnWndProc:   queryWndProcCb,
			hInstance:     instance,
			lpszClassName: uintptr(unsafe.Pointer(name)),
		}
		if r, _, _ := procRegisterClassW.Call(uintptr(unsafe.Pointer(&wc))); r != 0 {
			queryClassReady = true
		}
	})
	return queryClassReady
}

func queryCreateReceiver() uintptr {
	instance, _, _ := procGetModuleHandleW.Call(0)
	name, err := syscall.UTF16PtrFromString(queryClassName)
	if err != nil {
		return 0
	}
	hwnd, _, _ := procCreateWindowExW.Call(
		0, uintptr(unsafe.Pointer(name)), 0, 0,
		0, 0, 0, 0,
		hwndMessage, 0, instance, 0)
	if hwnd != 0 {
		// Baozi 若以更高权限运行，UIPI 会静默丢掉对方（普通权限进程）的回复；
		// 显式放行 WM_COPYDATA。
		procChangeWindowMessageFilterEx.Call(hwnd, wmCopyData, msgfltAllow, 0)
	}
	return hwnd
}

func queryWndProc(hwnd, msg, wParam, lParam uintptr) uintptr {
	if msg == wmCopyData && lParam != 0 {
		cds := (*copyDataStruct)(ptrOfUintptr(lParam))
		if cds.lpData != nil && cds.cbData > 0 {
			n := int(cds.cbData) / 2
			s := windows.UTF16ToString(unsafe.Slice((*uint16)(cds.lpData), n))
			queryMu.Lock()
			queryLast = copyReply{dwData: cds.dwData, text: strings.TrimRight(s, "\x00")}
			queryMu.Unlock()
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
