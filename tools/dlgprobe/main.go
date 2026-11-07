package main

// dlgprobe 探针：找出当前打开的「打开/另存为」对话框，打印它的控件树，
// 并可选地把某个路径写进它的地址栏，用来确认外部程序能不能改动已经开着的对话框目录。
//
// 用法：
//   go run ./tools/dlgprobe                 列出所有 #32770 窗口与子控件
//   go run ./tools/dlgprobe "D:\work\baozi"  把第一个对话框跳到这个目录

import (
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	pEnumWindows      = user32.NewProc("EnumWindows")
	pEnumChildWindows = user32.NewProc("EnumChildWindows")
	pGetClassName     = user32.NewProc("GetClassNameW")
	pGetWindowText    = user32.NewProc("GetWindowTextW")
	pGetWindowTextLen = user32.NewProc("GetWindowTextLengthW")
	pGetDlgCtrlID     = user32.NewProc("GetDlgCtrlID")
	pIsWindowVisible  = user32.NewProc("IsWindowVisible")
	pSetWindowText    = user32.NewProc("SetWindowTextW")
	pPostMessage      = user32.NewProc("PostMessageW")
	pSendMessage      = user32.NewProc("SendMessageW")
	pGetThreadProcess = user32.NewProc("GetWindowThreadProcessId")
	pGetParent        = user32.NewProc("GetParent")
	pFindWindowEx     = user32.NewProc("FindWindowExW")

	pOpenProcess        = kernel32.NewProc("OpenProcess")
	pQueryFull          = kernel32.NewProc("QueryFullProcessImageNameW")
	pCloseHandle        = kernel32.NewProc("CloseHandle")
	pGetClientRect      = user32.NewProc("GetClientRect")
	pSetForeground      = user32.NewProc("SetForegroundWindow")
	pGetForeground      = user32.NewProc("GetForegroundWindow")
	pAttachThreadInput  = user32.NewProc("AttachThreadInput")
	pGetCurrentThreadID = kernel32.NewProc("GetCurrentThreadId")
	pSendInput          = user32.NewProc("SendInput")
	pGetWindowThreadID  = user32.NewProc("GetWindowThreadProcessId")
	pSetFocus           = user32.NewProc("SetFocus")
	pGetFocus           = user32.NewProc("GetFocus")
	pGetGUIThreadInfo   = user32.NewProc("GetGUIThreadInfo")
	pShowWindow         = user32.NewProc("ShowWindow")
	pSetActive          = user32.NewProc("SetActiveWindow")
	pBringToTop         = user32.NewProc("BringWindowToTop")
	pGetWindowRect      = user32.NewProc("GetWindowRect")
	pGetSystemMetrics   = user32.NewProc("GetSystemMetrics")
	pSetWindowPos       = user32.NewProc("SetWindowPos")
)

// INPUT 的 64 位布局是 40 字节：type(4) + 对齐(4) + KEYBDINPUT(24) + union 余量(8)。
type INPUT struct {
	Type  uint32
	_     uint32
	Vk    uint16
	Scan  uint16
	Flags uint32
	Time  uint32
	Extra uintptr
	_     [8]byte
}

const (
	wmSetText     = 0x000C
	wmKeyDown     = 0x0100
	wmKeyUp       = 0x0101
	wmChar        = 0x0102
	wmLButtonDown = 0x0201
	wmLButtonUp   = 0x0202
	vkReturn      = 0x0D
	vkControl     = 0x11
	processName   = 0x1000
)

type RECT struct{ Left, Top, Right, Bottom int32 }

func className(hw uintptr) string {
	buf := make([]uint16, 256)
	n, _, _ := pGetClassName.Call(hw, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 {
		return ""
	}
	return syscall.UTF16ToString(buf[:n])
}

func windowText(hw uintptr) string {
	n, _, _ := pGetWindowTextLen.Call(hw)
	if n == 0 {
		return ""
	}
	buf := make([]uint16, int(n)+1)
	pGetWindowText.Call(hw, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return syscall.UTF16ToString(buf)
}

func ctrlID(hw uintptr) int32 {
	v, _, _ := pGetDlgCtrlID.Call(hw)
	return int32(v)
}

func visible(hw uintptr) bool {
	v, _, _ := pIsWindowVisible.Call(hw)
	return v != 0
}

func parent(hw uintptr) uintptr {
	v, _, _ := pGetParent.Call(hw)
	return v
}

func procName(hw uintptr) string {
	var pid uint32
	pGetThreadProcess.Call(hw, uintptr(unsafe.Pointer(&pid)))
	h, _, _ := pOpenProcess.Call(processName|0x0400, 0, uintptr(pid))
	if h == 0 {
		return fmt.Sprintf("pid=%d", pid)
	}
	defer pCloseHandle.Call(h)
	buf := make([]uint16, 512)
	var size uint32 = uint32(len(buf))
	_, _, _ = pQueryFull.Call(h, 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)))
	return syscall.UTF16ToString(buf[:size])
}

func children(hw uintptr) []uintptr {
	var out []uintptr
	cb := syscall.NewCallback(func(child, _ uintptr) uintptr {
		out = append(out, child)
		return 1
	})
	pEnumChildWindows.Call(hw, cb, 0)
	return out
}

func dump(hw uintptr, depth int) {
	indent := strings.Repeat("  ", depth)
	fmt.Printf("%s%-18s id=%-6d vis=%v %q\n", indent, className(hw), ctrlID(hw), visible(hw), trunc(windowText(hw), 60))
	if depth >= 9 {
		return
	}
	for _, c := range children(hw) {
		if parent(c) != hw {
			continue
		}
		dump(c, depth+1)
	}
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func main() {
	if err := user32.Load(); err != nil {
		fmt.Println("user32:", err)
		return
	}
	setPath := ""
	if len(os.Args) > 1 {
		setPath = os.Args[1]
	}

	var dialogs []uintptr
	cb := syscall.NewCallback(func(hw, _ uintptr) uintptr {
		cls := className(hw)
		if strings.EqualFold(cls, "#32770") && visible(hw) {
			dialogs = append(dialogs, hw)
		}
		return 1
	})
	pEnumWindows.Call(cb, 0)

	fmt.Printf("找到 %d 个可见的 #32770 窗口\n\n", len(dialogs))
	for _, hw := range dialogs {
		fmt.Printf("=== hw=%#x  %q\n    proc=%s\n", hw, windowText(hw), procName(hw))
		dump(hw, 1)
		fmt.Println()
	}

	if setPath == "" || len(dialogs) == 0 {
		return
	}
	dlg := dialogs[0]
	fmt.Printf("把 %s 送进 hw=%#x 的对话框：Ctrl+L 聚焦地址栏 → 全选 → 打字 → 回车\n", setPath, dlg)
	fmt.Println("导航前地址栏：", addressText(dlg))

	focusWindow(dlg)
	time.Sleep(200 * time.Millisecond)
	// Ctrl+L 在这版对话框上不切编辑态（实测 visible 仍是 false），
	// 点一下面包屑空白处才行：点完 Edit 会现身并拿到焦点。
	clickBreadcrumb(dlg)
	time.Sleep(350 * time.Millisecond)

	target := findAddressEdit(dlg)
	if target == 0 {
		fmt.Println("没找到地址栏 Edit")
		return
	}
	fmt.Printf("点面包屑后：Edit id=%d visible=%v，内容=%q\n",
		ctrlID(target), visible(target), windowText(target))

	// 后台进程改不了别人的前台（SetForegroundWindow 被前台锁拒绝，实测前台仍是 Chrome）。
	// 唯一可靠的办法是用真实鼠标点它的标题栏把它激活——鼠标事件按键位生效，不看调用者是谁。
	fgBefore, _, _ := pGetForeground.Call()
	fmt.Printf("点击前：前台=%#x %s，对话框焦点=%#x\n", fgBefore, className(fgBefore), dialogFocus(dlg))
	clickTitleBar(dlg)
	time.Sleep(500 * time.Millisecond)
	fgAfter, _, _ := pGetForeground.Call()
	fmt.Printf("点击后：前台=%#x %s，对话框焦点=%#x %s\n",
		fgAfter, className(fgAfter), dialogFocus(dlg), className(dialogFocus(dlg)))
	if fgAfter != dlg {
		fmt.Println("对话框没能成为前台，后面的按键会落到别的窗口，停止。")
		return
	}

	pressCtrlL()
	time.Sleep(350 * time.Millisecond)
	target = findAddressEdit(dlg)
	fmt.Printf("Ctrl+L 后：Edit visible=%v，内容=%q\n", visible(target), windowText(target))

	// Ctrl+L 在这版对话框上不切编辑态，按键又只在控件自己拿焦点时才进得去，
	// 所以最后一步是完整模拟用户：真实鼠标点地址栏 → 全选 → 打字 → 回车。
	clickBreadcrumbReal(dlg)
	time.Sleep(450 * time.Millisecond)
	target = findAddressEdit(dlg)
	fmt.Printf("真实点击后：Edit visible=%v，内容=%q\n", visible(target), windowText(target))

	pressCtrlA()
	time.Sleep(120 * time.Millisecond)
	n := typeUnicode(setPath)
	fmt.Printf("打进 %d 个事件；编辑框 = %q\n", n, windowText(target))
	time.Sleep(200 * time.Millisecond)
	sendEnter()
	time.Sleep(900 * time.Millisecond)
	fmt.Println("最终地址栏：", addressText(dlg))
}

// clickBreadcrumbReal 用真实鼠标点面包屑右侧空白处，让地址栏切成可编辑状态。
func clickBreadcrumbReal(dlg uintptr) {
	toolbar := findToolbar1001(dlg)
	if toolbar == 0 {
		fmt.Println("没找到面包屑")
		return
	}
	var rc RECT
	pGetWindowRect.Call(toolbar, uintptr(unsafe.Pointer(&rc)))
	x := rc.Right - 14
	y := (rc.Top + rc.Bottom) / 2
	fmt.Printf("面包屑屏幕矩形 (%d,%d)-(%d,%d)，点 (%d,%d)\n",
		int32(rc.Left), int32(rc.Top), int32(rc.Right), int32(rc.Bottom), int32(x), int32(y))
	moveMouse(int32(x), int32(y))
	time.Sleep(150 * time.Millisecond)
	mouseClick()
}

func findToolbar1001(dlg uintptr) uintptr {
	var walk func(h uintptr, depth int) uintptr
	walk = func(h uintptr, depth int) uintptr {
		if depth > 8 {
			return 0
		}
		for _, c := range children(h) {
			if strings.EqualFold(className(c), "ToolbarWindow32") && ctrlID(c) == 1001 {
				return c
			}
			if v := walk(c, depth+1); v != 0 {
				return v
			}
		}
		return 0
	}
	return walk(dlg, 0)
}

// clickTitleBar 在对话框标题栏正中点一下（真实鼠标事件），把它激活。
// 先把窗口提到最上层：它可能被别的窗口盖住，那样鼠标会点到盖住它的窗口上。
func clickTitleBar(hw uintptr) {
	const hwndTopmost = ^uintptr(0)
	pSetWindowPos.Call(hw, hwndTopmost, 0, 0, 0, 0, 0x0043)               // NOMOVE|NOSIZE|NOACTIVATE… 这里要 ACTIVE
	pSetWindowPos.Call(hw, hwndTopmost, 0, 0, 0, 0, 0x0001|0x0002|0x0040) // NOSIZE|NOMOVE|SHOWWINDOW
	var rc RECT
	pGetWindowRect.Call(hw, uintptr(unsafe.Pointer(&rc)))
	fmt.Printf("对话框矩形 (%d,%d)-(%d,%d)\n", int32(rc.Left), int32(rc.Top), int32(rc.Right), int32(rc.Bottom))
	x := int32((rc.Left + rc.Right) / 2)
	y := rc.Top + 14
	fmt.Printf("点击标题栏 (%d,%d)\n", x, y)
	moveMouse(x, y)
	time.Sleep(120 * time.Millisecond)
	mouseClick()
}

func moveMouse(x, y int32) {
	// MOUSEINPUT: dx(4) dy(4) mouseData(4) dwFlags(4) time(4) extra(8)
	var mi struct {
		Type      uint32
		Pad       uint32
		Dx        int32
		Dy        int32
		MouseData uint32
		Flags     uint32
		Time      uint32
		Pad2      uint32
		Extra     uintptr
	}
	mi.Type = 0
	mi.Flags = 0x0001 | 0x8000 // MOUSEEVENTF_MOVE | MOUSEEVENTF_ABSOLUTE
	// ABSOLUTE 的归一化基于整个虚拟桌面（多屏时不能只拿主屏尺寸算）。
	vx, _, _ := pGetSystemMetrics.Call(76) // SM_XVIRTUALSCREEN
	vy, _, _ := pGetSystemMetrics.Call(77) // SM_YVIRTUALSCREEN
	vw, _, _ := pGetSystemMetrics.Call(78) // SM_CXVIRTUALSCREEN
	vh, _, _ := pGetSystemMetrics.Call(79) // SM_CYVIRTUALSCREEN
	fmt.Printf("虚拟桌面 %d,%d %dx%d\n", int32(vx), int32(vy), int32(vw), int32(vh))
	mi.Dx = int32((int64(x) - int64(int32(vx))) * 65535 / int64(int32(vw)))
	mi.Dy = int32((int64(y) - int64(int32(vy))) * 65535 / int64(int32(vh)))
	pSendInput.Call(1, uintptr(unsafe.Pointer(&mi)), unsafe.Sizeof(mi))
}

func mouseClick() {
	var mi struct {
		Type      uint32
		Pad       uint32
		Dx        int32
		Dy        int32
		MouseData uint32
		Flags     uint32
		Time      uint32
		Pad2      uint32
		Extra     uintptr
	}
	mi.Type = 0
	mi.Flags = 0x0002 // MOUSEEVENTF_LEFTDOWN
	pSendInput.Call(1, uintptr(unsafe.Pointer(&mi)), unsafe.Sizeof(mi))
	mi.Flags = 0x0004 // MOUSEEVENTF_LEFTUP
	pSendInput.Call(1, uintptr(unsafe.Pointer(&mi)), unsafe.Sizeof(mi))
}

// GUITHREADINFO 是 GetGUIThreadInfo 的输出结构（x64 上 72 字节）。
type GUITHREADINFO struct {
	CbSize        uint32
	Flags         uint32
	HwndActive    uintptr
	HwndFocus     uintptr
	HwndCapture   uintptr
	HwndMenuOwner uintptr
	HwndMoveSize  uintptr
	HwndCaret     uintptr
	RcCaret       RECT
	_             [4]byte
}

// dialogFocus 读对话框线程真实的焦点窗口。AttachThreadInput 期间 GetFocus 会失真，
// 这个 API 不受影响。
func dialogFocus(dlg uintptr) uintptr {
	dt, _, _ := pGetWindowThreadID.Call(dlg, 0)
	if dt == 0 {
		return 0
	}
	var info GUITHREADINFO
	info.CbSize = uint32(unsafe.Sizeof(info))
	ok, _, _ := pGetGUIThreadInfo.Call(dt, uintptr(unsafe.Pointer(&info)))
	if ok == 0 {
		return 0
	}
	return info.HwndFocus
}

// pressCtrlL 注入 Ctrl+L：资源管理器与文件对话框都用它聚焦地址栏并全选当前路径。
func pressCtrlL() {
	keyEvent(vkControl, 0x1D, false)
	keyEvent('L', 0x26, false)
	keyEvent('L', 0x26, true)
	keyEvent(vkControl, 0x1D, true)
}

func pressCtrlA() {
	keyEvent(vkControl, 0x1D, false)
	keyEvent('A', 0x1E, false)
	keyEvent('A', 0x1E, true)
	keyEvent(vkControl, 0x1D, true)
}

// selectAll 兜底：直接给编辑框发 EM_SETSEL，选中全部内容，接下来打字就是整体替换。
func selectAll(edit uintptr) {
	const emSetSel = 0x00B1
	pSendMessage.Call(edit, emSetSel, 0, ^uintptr(0))
}

// typeUnicode 逐字符注入：KEYEVENTF_UNICODE 让扫描码直接是字符本身，
// 中文路径也不需要键盘布局映射，和粘贴进去的效果一样。
func typeUnicode(s string) int {
	var total int
	for _, r := range s {
		for _, up := range []bool{false, true} {
			in := INPUT{Type: 1, Scan: uint16(r), Flags: 0x0004}
			if up {
				in.Flags |= 0x0002
			}
			got, _, _ := pSendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in))
			total += int(got)
		}
	}
	return total
}

func keyEvent(vk, scan uint16, up bool) {
	in := INPUT{Type: 1, Vk: vk, Scan: scan}
	if up {
		in.Flags = 0x0002
	}
	pSendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in))
}

// focusedWindow 返回对话框所在线程里当前真正有焦点的窗口。
func focusedWindow(dlg uintptr) uintptr {
	dt, _, _ := pGetWindowThreadID.Call(dlg, 0)
	cur, _, _ := pGetCurrentThreadID.Call()
	pAttachThreadInput.Call(cur, dt, 1)
	defer pAttachThreadInput.Call(cur, dt, 0)
	v, _, _ := pGetFocus.Call()
	return v
}

// focusControl 把键盘焦点交给对话框里的某个控件。跨进程 SetFocus 必须先 AttachThreadInput。
func focusControl(dlg, ctrl uintptr) {
	cur, _, _ := pGetCurrentThreadID.Call()
	dt, _, _ := pGetWindowThreadID.Call(dlg, 0)
	if dt != 0 && dt != cur {
		pAttachThreadInput.Call(cur, dt, 1)
		defer pAttachThreadInput.Call(cur, dt, 0)
	}
	pSetFocus.Call(ctrl)
}

// sendEnter 注入一个真实的回车：文件对话框会忽略 SendMessage 伪造的按键，
// 只认输入队列里的键（用户手打就是这个路径）。
func sendEnter() {
	in := INPUT{Type: 1, Vk: vkReturn, Scan: 0x1C}
	pSendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in))
	in.Flags = 0x0002 // KEYEVENTF_KEYUP
	pSendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in))
}

// focusWindow 把对话框切成前台。SetForegroundWindow 有前台锁，
// 用 AttachThreadInput 借当前前台线程的输入状态绕过去（Baozi 唤出面板时也是这套）。
func focusWindow(hw uintptr) {
	fg, _, _ := pGetForeground.Call()
	if fg == hw {
		return
	}
	cur, _, _ := pGetCurrentThreadID.Call()
	ft, _, _ := pGetWindowThreadID.Call(fg, 0)
	if ft != 0 && ft != cur {
		pAttachThreadInput.Call(cur, ft, 1)
		defer pAttachThreadInput.Call(cur, ft, 0)
		pSetForeground.Call(hw)
		return
	}
	pSetForeground.Call(hw)
}

// clickBreadcrumb 在地址栏面包屑的空白处单击，让它切换成可编辑的地址框。
func clickBreadcrumb(dlg uintptr) {
	var toolbar uintptr
	for _, c := range children(dlg) {
		if strings.EqualFold(className(c), "ToolbarWindow32") && ctrlID(c) == 1001 {
			toolbar = c
			break
		}
	}
	if toolbar == 0 {
		fmt.Println("没找到面包屑 ToolbarWindow32(1001)")
		return
	}
	var rc RECT
	pGetClientRect.Call(toolbar, uintptr(unsafe.Pointer(&rc)))
	// 面包屑靠左排布，右侧是空白：点右侧一点，避免点到某个段上又跳去别的目录。
	x := rc.Right - 12
	y := (rc.Top + rc.Bottom) / 2
	fmt.Printf("点面包屑空白处 (%d,%d)，toolbar 尺寸 %dx%d\n", x, y, rc.Right-rc.Left, rc.Bottom-rc.Top)
	pSendMessage.Call(toolbar, wmLButtonDown, 1, uintptr(uint32(y)<<16|uint32(x)))
	pSendMessage.Call(toolbar, wmLButtonUp, 0, uintptr(uint32(y)<<16|uint32(x)))
}

func addressText(dlg uintptr) string {
	for _, c := range children(dlg) {
		if strings.EqualFold(className(c), "ToolbarWindow32") && ctrlID(c) == 1001 {
			return windowText(c)
		}
	}
	return "(没找到)"
}

func findAddressEdit(root uintptr) uintptr {
	// Win11 的文件对话框：地址栏的编辑框是 ComboBoxEx32(id=41477) → ComboBox → Edit，
	// 平时隐藏（显示面包屑），拿到焦点或切到编辑态才现身。
	// 注意不能按"ComboBox 里的第一个 Edit"去找——那是 id=1148 的文件名框。
	for _, c := range children(root) {
		if !strings.EqualFold(className(c), "Edit") {
			continue
		}
		if id := ctrlID(c); id == 41477 || id == 1001 {
			return c
		}
		if strings.Contains(strings.ToLower(ancestorClass(c, 4)), "address band root") {
			return c
		}
	}
	return 0
}

func ancestorClass(hw uintptr, levels int) string {
	var out []string
	for i := 0; i < levels; i++ {
		hw = parent(hw)
		if hw == 0 {
			break
		}
		out = append(out, className(hw))
	}
	return strings.Join(out, " / ")
}
