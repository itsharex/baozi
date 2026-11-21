// 文件对话框（打开 / 另存为）的定位与导航。
//
// 做法照 Lertaro（https://github.com/lertaro/lertaro，.NET）的
// Plugins/CoreExtensions/FileDialog/StandardFileDialogAdapter.cs：
// 新式 Common Item Dialog 的"文件名"框仍然是一个普通 Edit 控件（不是 DirectUI 自绘），
// 所以可以 WM_SETTEXT 把完整路径写进去 —— 这是从进程外改"已经开着的对话框"目录的可行通道。
// 往地址栏（Breadcrumb Parent 下的 ToolbarWindow32）写字只改显示、列表不动；
// 往文件名框写路径，再触发默认按钮，外壳自己会导航过去。
//
// 步骤：
//  1. EnumChildWindows 找第一个 class == "Edit" 的子窗口；
//  2. WM_SETTEXT 写入路径（目录补尾反斜杠），再给父窗口发 WM_COMMAND(EN_CHANGE)；
//  3. 校验真的写进去了（写不进去就绝不回车，否则会把原来的文件名提交出去）；
//  4. 延时 ~250ms 后给对话框投递 WM_COMMAND(IDOK) 触发导航。
//     这里不能用 PostMessage(WM_KEYDOWN, VK_RETURN)：键盘回车要先经过对话框的消息循环
//     （IsDialogMessage）才会变成"按默认按钮"，直接投给 Edit 的 WM_KEYDOWN 多数时候
//     只停在编辑框里 —— 实测现象就是路径写进了文件名框、地址栏仍停在原目录。
//     默认按钮收到的就是 WM_COMMAND(IDOK)；路径以反斜杠结尾时外壳会把它当目录导航，
//     不会真的保存（实测对话框保持打开、地址栏变成目标目录）。
//
// 一条硬规矩：**绝不 AttachThreadInput**。它把本线程和对话框线程的输入队列绑死，
// 而 Go 的 goroutine 会在 OS 线程之间迁移、那个线程之后还会被 runtime 复用，
// 等于把对方的输入队列挂在一个不再处理消息的线程上 —— 对方窗口就此永久无响应
// （实测在记事本的"另存为"上直接把记事本按死）。所有跨进程消息一律走
// SendMessageTimeout，宁可这次不生效，也不能卡住别人。
package win

import (
	"os"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procGetParent              = user32.NewProc("GetParent")
	procGetDlgCtrlID           = user32.NewProc("GetDlgCtrlID")
	procSendMessageTimeoutW    = user32.NewProc("SendMessageTimeoutW")
	procPostMessageW           = user32.NewProc("PostMessageW")
	procSetFocus               = user32.NewProc("SetFocus")
	procGetWindowThreadProcess = user32.NewProc("GetWindowThreadProcessId")
	procFindWindowExW          = user32.NewProc("FindWindowExW")
	procEnumChildWindows       = user32.NewProc("EnumChildWindows")
)

const (
	wmSetText = 0x000C
	wmGetText = 0x000D
	wmCommand = 0x0111
	enChange  = 0x0300
	maxText   = 2048

	smtoBlock       = 0x0001
	smtoAbortIfHung = 0x0002
	msgTimeoutMs    = 500
)

// sendMsg 是带超时的 SendMessage。
// SMTO_ABORTIFHUNG：对方挂起就立刻放弃；SMTO_BLOCK 保留同步语义（写文本要立刻回读）。
// 超时兜底，所以对方真卡住时最多耽误 500ms，不会把我们（进而把整个跟随器）拖死。
func sendMsg(hwnd, msg, wParam, lParam uintptr) (uintptr, bool) {
	return sendMsgWith(hwnd, msg, wParam, lParam, smtoBlock|smtoAbortIfHung)
}

// sendMsgNoBlock 同上，但**不带 SMTO_BLOCK**。
//
// 这个区别对 Total Commander 是决定性的：带 SMTO_BLOCK 时 Windows 不允许调用线程
// 处理"回到本线程队列的"消息，而 TC 的 WM_COPYDATA 回复正是这样投回来的
// （Lertaro 的注释：the reply is delivered synchronously (nested) before it returns）。
// 结果就是请求发出去了、TC 也处理了（SendMessageTimeout 返回成功），路却永远是空的。
func sendMsgNoBlock(hwnd, msg, wParam, lParam uintptr) (uintptr, bool) {
	return sendMsgWith(hwnd, msg, wParam, lParam, smtoAbortIfHung)
}

func sendMsgWith(hwnd, msg, wParam, lParam, flags uintptr) (uintptr, bool) {
	var out uintptr
	r, _, _ := procSendMessageTimeoutW.Call(hwnd, msg, wParam, lParam,
		flags, msgTimeoutMs, uintptr(unsafe.Pointer(&out)))
	return out, r != 0
}

// DialogReport 描述一次"前台文件对话框"探测结果。
type DialogReport struct {
	// HWND 是对话框窗口；0 表示没找到。
	HWND uintptr
	// Title 是对话框标题（"打开" / "另存为" …）。
	Title string
	// Folder 是地址栏第一段文字（用来确认窗口确实是文件对话框）。
	Folder string
}

// IsFileDialog 判断 hwnd 是不是新式文件对话框：
// class 是 #32770，且内部有 Breadcrumb Parent（地址栏容器）。
// Lertaro 用同一个判据把"文件对话框"和普通 #32770 窗口分开。
func IsFileDialog(hwnd uintptr) bool {
	if hwnd == 0 {
		return false
	}
	if cls := className(hwnd); !strings.EqualFold(cls, "#32770") {
		return false
	}
	return findChildByClass(hwnd, "Breadcrumb Parent") != 0
}

// ActiveFileDialog 从当前前台窗口往上找最近的文件对话框。
// 面板被唤起时调用一次即可：那一刻前台还是原来那个对话框（或它的子窗口）。
func ActiveFileDialog() DialogReport {
	fg, _, _ := procGetForegroundWindow.Call()
	hwnd := fg
	for hwnd != 0 {
		if IsFileDialog(hwnd) {
			return DialogReport{HWND: hwnd, Title: WindowText(hwnd), Folder: dialogFolder(hwnd)}
		}
		parent, _, _ := procGetParent.Call(hwnd)
		if parent == 0 || parent == hwnd {
			break
		}
		hwnd = parent
	}
	return DialogReport{}
}

// NavigateDialog 让对话框跳到 path（目录），或者跳到 file 所在目录。
//
// 调用前提：这一刻对话框**就是**前台（由"用户切回它"这个动作保证）。
// 所以这里不抢前台、不绑输入队列 —— 只往文件名框写路径，再发一个回车。
func NavigateDialog(hwnd uintptr, path string) bool {
	if !IsFileDialog(hwnd) {
		return false
	}
	edit := findChildByClass(hwnd, "Edit")
	if edit == 0 {
		return false
	}
	target := path
	if isDir(target) && !strings.HasSuffix(target, `\`) {
		target += `\`
	}

	if !writeText(edit, target) {
		return false
	}
	// EN_CHANGE 让外壳知道内容变了（只发 WM_SETTEXT 时有些版本不更新"打开"按钮状态）。
	ctrlID, _, _ := procGetDlgCtrlID.Call(edit)
	parent, _, _ := procGetParent.Call(edit)
	if parent != 0 {
		wParam := uintptr(enChange<<16) | (ctrlID & 0xFFFF)
		sendMsg(parent, wmCommand, wParam, edit)
	}
	// 没写进去就不能回车：那会把输入框里原有的文件名提交掉。
	if got, ok := WindowTextW(edit); !ok || !strings.EqualFold(strings.TrimSpace(got), strings.TrimSpace(target)) {
		return false
	}

	// 稍后再发，让外壳先把 WM_SETTEXT / EN_CHANGE 处理完（Lertaro 用 300ms）。
	go func() {
		time.Sleep(250 * time.Millisecond)
		if fg, _, _ := procGetForegroundWindow.Call(); RootOwner(fg) == hwnd {
			// 等价于用户按了这个框的默认按钮。Lertaro 原本发的是回车键，
			// 但 PostMessage 到 Edit 绕过了 IsDialogMessage，回车不会变成默认按钮；
			// 直接送 IDOK 才稳定。target 是已存在的目录且带尾反斜杠，
			// 外壳会导航进去而不是保存。
			procPostMessageW.Call(hwnd, wmCommand, 1, 0)
		}
	}()
	return true
}

// RestoreDialogFocus 把光标还给文件名框。不抢前台、不绑输入队列，
// 对话框不在前台时什么也不做。
func RestoreDialogFocus(hwnd uintptr) bool {
	if !IsFileDialog(hwnd) {
		return false
	}
	if fg, _, _ := procGetForegroundWindow.Call(); RootOwner(fg) != hwnd {
		return false
	}
	edit := findChildByClass(hwnd, "Edit")
	if edit == 0 {
		return false
	}
	procSetFocus.Call(edit)
	return true
}

// WindowText 读窗口标题。
func WindowText(hwnd uintptr) string {
	buf := make([]uint16, maxText)
	sendMsg(hwnd, wmGetText, maxText, uintptr(unsafe.Pointer(&buf[0])))
	return windows.UTF16ToString(buf)
}

// WindowTextW 读指定窗口的文本（单行 Edit 也适用）。
func WindowTextW(hwnd uintptr) (string, bool) {
	buf := make([]uint16, maxText)
	n, ok := sendMsg(hwnd, wmGetText, maxText, uintptr(unsafe.Pointer(&buf[0])))
	if !ok || n == 0 {
		return "", false
	}
	return windows.UTF16ToString(buf), true
}

// dialogFolder 读地址栏第一段文字，仅用于确认窗口身份。
func dialogFolder(hwnd uintptr) string {
	bread := findChildByClass(hwnd, "Breadcrumb Parent")
	if bread == 0 {
		return ""
	}
	cls := uintptrOf("ToolbarWindow32")
	child, _, _ := procFindWindowExW.Call(bread, 0, cls, 0)
	for child != 0 {
		if t := strings.TrimSpace(WindowText(child)); t != "" {
			return t
		}
		child, _, _ = procFindWindowExW.Call(bread, child, cls, 0)
	}
	return ""
}

// writeText 给 Edit 写文本，并回读校验。
func writeText(hwnd uintptr, s string) bool {
	p, err := syscall.UTF16PtrFromString(s)
	if err != nil {
		return false
	}
	if _, ok := sendMsg(hwnd, wmSetText, 0, uintptr(unsafe.Pointer(p))); !ok {
		return false
	}
	got, ok := WindowTextW(hwnd)
	return ok && strings.EqualFold(strings.TrimSpace(got), strings.TrimSpace(s))
}

func findChildByClass(parent uintptr, class string) uintptr {
	// EnumChildWindows 会递归到所有后代——"Breadcrumb Parent" 和文件名框都不在对话框的直接子层，
	// FindWindowExW 只扫直接子窗口，用它永远找不到（这也是最初"有对话框却认不出来"的原因）。
	var found uintptr
	cb := syscall.NewCallback(func(child uintptr, _ uintptr) uintptr {
		if found == 0 && strings.EqualFold(className(child), class) {
			found = child
			return 0
		}
		return 1
	})
	procEnumChildWindows.Call(parent, cb, 0)
	return found
}

func uintptrOf(s string) uintptr {
	p, err := syscall.UTF16PtrFromString(s)
	if err != nil {
		return 0
	}
	return uintptr(unsafe.Pointer(p))
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
