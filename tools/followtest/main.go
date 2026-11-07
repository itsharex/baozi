// 验证"文件对话框跟着资源管理器走"的真实流程：
//
//	另存为框停在「文档」→ 打开资源管理器切到「下载」→ 切回另存为框 → 它应当自己变成「下载」。
//
// 全程在一个进程里做完：中间只要隔一次工具调用，前台就会被别的窗口抢走，
// 而 Baozi 的跟随器看的就是 GetForegroundWindow，前台一乱整条链路就断。
//
// 用法：go run ./tools/followtest [目标目录]
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"baozi/internal/win"
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
	procSetFocus               = user32.NewProc("SetFocus")
	procAttachThread           = user32.NewProc("AttachThreadInput")
	procGetWindowThreadProcess = user32.NewProc("GetWindowThreadProcessId")
	procGetForegroundWindow    = user32.NewProc("GetForegroundWindow")
	procIsWindowVisible        = user32.NewProc("IsWindowVisible")
	procSwitchToThisWindow     = user32.NewProc("SwitchToThisWindow")
	procBringToTop             = user32.NewProc("BringWindowToTop")
	procPostMessage            = user32.NewProc("PostMessageW")
	procIsWindow               = user32.NewProc("IsWindow")
	procIsHung                 = user32.NewProc("IsHungAppWindow")
	procKeybdEvent             = user32.NewProc("keybd_event")
	procGetCurrentThreadID     = kernel32.NewProc("GetCurrentThreadId")
	procSendMessageTimeout     = user32.NewProc("SendMessageTimeoutW")
	procFindWindow             = user32.NewProc("FindWindowW")
)

const (
	wmGetText = 0x000D
	wmClose   = 0x0010
	swRestore = 9
	keyUpFlag = 0x0002

	wmCopyData = 0x004A
	// TC 的"换目录"命令：dwData = 'C' + 256*'D'
	tcChangeDir = uintptr('C') + 256*uintptr('D')
	smtoAbort   = 0x0002
)

// copyDataStruct 是 COPYDATASTRUCT 的布局。
type copyDataStruct struct {
	dwData uintptr
	cbData uint32
	_      uint32
	lpData unsafe.Pointer
}

// foreground 返回当前前台窗口。
func foreground() uintptr {
	h, _, _ := procGetForegroundWindow.Call()
	return h
}

// tcChangePanelDir 让 Total Commander 的活动面板切到 path。
// 照 Lertaro 的 Win32Helper.ChangeSourcePanelDirectory：lpData 是
// [UTF-8 BOM]path 0x0D 0x00 (flags" S") 0x00，flags 后的尾 NUL 必须计入 cbData。
// TC 只在前台时才会执行这条命令。
func tcChangePanelDir(tc uintptr, path string) bool {
	buf := make([]byte, 0, len(path)+8)
	buf = append(buf, 0xEF, 0xBB, 0xBF)
	buf = append(buf, []byte(path)...)
	buf = append(buf, 0x0D, 0x00)
	buf = append(buf, 'S')
	buf = append(buf, 0x00)
	cds := copyDataStruct{
		dwData: tcChangeDir,
		cbData: uint32(len(buf)),
		lpData: unsafe.Pointer(&buf[0]),
	}
	var out uintptr
	r, _, _ := procSendMessageTimeout.Call(tc, wmCopyData, 0,
		uintptr(unsafe.Pointer(&cds)), smtoAbort, 1000, uintptr(unsafe.Pointer(&out)))
	return r != 0
}

// findTotalCommander 找 TC 主窗口（类名 TTOTAL_CMD）。
func findTotalCommander() uintptr {
	name, _ := syscall.UTF16PtrFromString("TTOTAL_CMD")
	h, _, _ := procFindWindow.Call(uintptr(unsafe.Pointer(name)), 0)
	return h
}

func className(h uintptr) string {
	buf := make([]uint16, 256)
	procGetClassNameW.Call(h, uintptr(unsafe.Pointer(&buf[0])), 256)
	return windows.UTF16ToString(buf)
}

func text(h uintptr) string {
	buf := make([]uint16, 1024)
	procSendMessageW.Call(h, wmGetText, 1024, uintptr(unsafe.Pointer(&buf[0])))
	return strings.TrimSpace(windows.UTF16ToString(buf))
}

func winText(h uintptr) string {
	buf := make([]uint16, 512)
	procGetWindowTextW.Call(h, uintptr(unsafe.Pointer(&buf[0])), 512)
	return strings.TrimSpace(windows.UTF16ToString(buf))
}

func findFileDialog() uintptr {
	return findFileDialogIn(0)
}

// findFileDialogIn 只找属于 pid 的文件对话框（pid=0 表示不限）。
func findFileDialogIn(pid uint32) uintptr {
	var res uintptr
	cb := syscall.NewCallback(func(h uintptr, _ uintptr) uintptr {
		if res != 0 {
			return 1
		}
		if !strings.EqualFold(className(h), "#32770") {
			return 1
		}
		if pid != 0 {
			var owner uint32
			procGetWindowThreadProcess.Call(h, uintptr(unsafe.Pointer(&owner)))
			if owner != pid {
				return 1
			}
		}
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

// findFileDialogOf 按进程找文件对话框。
func findFileDialogOf(pid uint32) uintptr {
	return findFileDialogIn(pid)
}

// addressText 读地址栏那条 toolbar（id=1001）的文本，例如 "地址: C:\Users\...\Downloads"。
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

// focus 把 hwnd 抢成前台。关键在 AttachThreadInput 要接「当前前台窗口」的线程，
// 借它的前台权限；接到目标窗口自己的线程上没有用，SetForegroundWindow 照样被拒。
func focus(hwnd uintptr) bool {
	fg, _, _ := procGetForegroundWindow.Call()
	if fg == hwnd {
		return true
	}
	owner, _, _ := procGetWindowThreadProcess.Call(fg, 0)
	self, _, _ := procGetCurrentThreadID.Call()
	if owner != 0 && owner != self {
		if ok, _, _ := procAttachThread.Call(self, owner, 1); ok != 0 {
			// 脱离必须传 0，否则两个线程的输入队列会永久绑在一起。
			defer procAttachThread.Call(self, owner, 0)
		}
	}
	procShowWindow.Call(hwnd, swRestore)
	procBringToTop.Call(hwnd)
	procSetForeground.Call(hwnd)
	procSetFocus.Call(hwnd)
	now, _, _ := procGetForegroundWindow.Call()
	return now == hwnd
}

func main() {
	dst := `D:\work\baozi`
	if len(os.Args) > 1 {
		dst = os.Args[1]
	}
	ps := `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`
	script := `D:\work\baozi\dist\dlg-target.ps1`

	// 清掉上一轮开的资源管理器窗口，否则一堆同名窗口会干扰"谁是前台"
	closeWindowsTitled("Downloads")
	closeWindowsTitled(filepath.Base(dst))

	// 两种靶子：默认是 WinForms 的 OpenFileDialog；FOLLOW_NOTEPAD=1 用真的记事本，
	// 因为它才是用户实际踩到卡死的那个程序。
	useNotepad := os.Getenv("FOLLOW_NOTEPAD") != ""
	var targetCmd *exec.Cmd
	if useNotepad {
		targetCmd = exec.Command(`C:\Windows\System32\notepad.exe`)
	} else {
		targetCmd = exec.Command(ps, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", script)
	}
	if err := targetCmd.Start(); err != nil {
		fmt.Println("起靶子失败:", err)
		os.Exit(1)
	}
	time.Sleep(1500 * time.Millisecond)

	if useNotepad {
		// 用进程 ID 认窗口比按标题稳：记事本的标题是"无标题 - 记事本"，
		// 而且它可能还没把窗口建好。
		var nw uintptr
		for i := 0; i < 10 && nw == 0; i++ {
			time.Sleep(500 * time.Millisecond)
			nw = firstWindowOfProcess(uint32(targetCmd.Process.Pid))
		}
		if nw == 0 {
			fmt.Println("没找到记事本窗口")
			os.Exit(1)
		}
		fmt.Printf("   记事本 %#x title=%q\n", nw, winText(nw))
		pid := uint32(targetCmd.Process.Pid)
		var dlg uintptr
		for i := 0; i < 4 && dlg == 0; i++ {
			if i > 0 {
				fmt.Println("   Ctrl+Shift+S 没弹出对话框，重试一次")
			}
			ok := focus(nw)
			fmt.Printf("   抢前台=%v\n", ok)
			time.Sleep(500 * time.Millisecond)
			// 记事本的"另存为"是 Ctrl+Shift+S
			keyCombo(0x11, 0x10, 0x53)
			time.Sleep(1500 * time.Millisecond)
			dlg = findFileDialogOf(pid)
		}
		if dlg == 0 {
			fmt.Println("记事本没能弹出另存为框（自动化发键在这台机器上抢不到前台）")
			os.Exit(1)
		}
	} else {
		time.Sleep(2500 * time.Millisecond)
	}

	var dlg uintptr
	if useNotepad {
		dlg = findFileDialogOf(uint32(targetCmd.Process.Pid))
	} else {
		dlg = findFileDialog()
	}
	if dlg == 0 {
		fmt.Println("没找到文件对话框")
		os.Exit(1)
	}
	fmt.Printf("① 另存为框 %#x 初始地址: %q\n", dlg, addressText(dlg))
	// 让对话框在前台停一会儿：跟随器第一次看到它只做登记（不导航），
	// 这一步必须真的被它看到，否则后面那次激活也只会被当成"第一次"。
	hold(dlg, 2000)
	time.Sleep(300 * time.Millisecond)

	// 跑两轮：用户报的就是"第一轮能跟，换了个目录再切回来就不跟了"。
	cycle(dlg, dst, "②", "③")
	second := os.Getenv("FOLLOW_SECOND")
	if second == "" {
		second = `C:\Windows`
	}
	cycle(dlg, second, "④", "⑤")
}

// cycle 做一轮：资源管理器切到 dst → 切回对话框 → 连采 6 次地址栏。
// FOLLOW_TC=1 时改用 Total Commander 当目录来源（TC 前台 + CD 命令换目录）。
func cycle(dlg uintptr, dst, stepTag, obsTag string) {
	if os.Getenv("FOLLOW_TC") != "" {
		tc := findTotalCommander()
		if tc == 0 {
			fmt.Println(stepTag, "没找到 Total Commander 窗口")
			return
		}
		fmt.Println(stepTag, "让 Total Commander 前台并切到", dst)
		hold(tc, 1200)
		ok := tcChangePanelDir(tc, dst)
		fmt.Printf("   CD 命令已发=%v\n", ok)
		hold(tc, 1500)
		// 读回 TC 现在的目录，确认 CD 真的生效了（没生效的话后面测的就不是新目录）
		fmt.Printf("   TC 当前目录=%q 前台=%#x\n", win.TotalCommanderFolder(tc), foreground())

		focus(dlg)
		hold(dlg, 3000)
		for i := 0; i < 6; i++ {
			alive, _, _ := procIsWindow.Call(dlg)
			hung, _, _ := procIsHung.Call(dlg)
			fmt.Printf("%s +%dms 地址: %q 对话框还在=%v 卡死=%v\n", obsTag, i*500, addressText(dlg), alive != 0, hung != 0)
			time.Sleep(500 * time.Millisecond)
		}
		return
	}

	// ② 资源管理器切到目标目录（它会成为前台，Baozi 的跟随器这时记下新目录）
	// 用绝对路径：exec.LookPath 只查 PATH，explorer.exe 不在里面。
	if err := exec.Command(`C:\Windows\explorer.exe`, dst).Start(); err != nil {
		fmt.Println("起资源管理器失败:", err)
	}
	time.Sleep(1200 * time.Millisecond)
	fmt.Println(stepTag, "已让资源管理器切到", dst)
	dumpExplorers()

	// 抢住前台：这台机器上客户端时不时把前台抢回去，而跟随器是按前台判断的，
	// 前台一乱就什么都看不到。真实使用里没有这个干扰，测试里得自己压住。
	// 已知文件夹（下载/文档/音乐…）的窗口标题是本地化名字，跟目录名对不上，
	// 所以允许用 FOLLOW_TITLE 直接指定要找的标题。
	title := os.Getenv("FOLLOW_TITLE")
	if title == "" {
		title = filepath.Base(dst)
	}
	hold(firstVisibleWithTitle(title), 2400)

	// ③ 切回对话框，等跟随器送路径
	focus(dlg)
	hold(dlg, 3000)
	for i := 0; i < 6; i++ {
		alive, _, _ := procIsWindow.Call(dlg)
		hung, _, _ := procIsHung.Call(dlg)
		fmt.Printf("%s +%dms 地址: %q 对话框还在=%v 卡死=%v\n", obsTag, i*500, addressText(dlg), alive != 0, hung != 0)
		time.Sleep(500 * time.Millisecond)
	}
}

// hold 反复把前台抢到 hwnd（0 表示"抢到标题含 sub 的那个"），持续 d。
func hold(hwnd uintptr, d time.Duration) {
	if hwnd == 0 {
		hwnd = firstVisibleWithTitle("")
	}
	if hwnd == 0 {
		return
	}
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !focus(hwnd) {
			fmt.Printf("   （抢前台失败，目标 %#x）\n", hwnd)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// firstWindowOfProcess 找某个进程的可见顶层窗口（记事本这类程序用它比按标题稳）。
func firstWindowOfProcess(pid uint32) uintptr {
	var res uintptr
	cb := syscall.NewCallback(func(h uintptr, _ uintptr) uintptr {
		if res != 0 {
			return 1
		}
		if vis, _, _ := procIsWindowVisible.Call(h); vis == 0 {
			return 1
		}
		if winText(h) == "" {
			return 1
		}
		var owner uint32
		procGetWindowThreadProcess.Call(h, uintptr(unsafe.Pointer(&owner)))
		if owner == pid {
			res = h
			return 0
		}
		return 1
	})
	procEnumWindows.Call(cb, 0)
	return res
}

// keyCombo 发一个"修饰键 + 键"的组合（keybd_event，够用且不用拼 INPUT 结构体）。
func keyCombo(mod1, mod2, key uintptr) {
	procKeybdEvent.Call(mod1, 0, 0, 0)
	procKeybdEvent.Call(mod2, 0, 0, 0)
	procKeybdEvent.Call(key, 0, 0, 0)
	procKeybdEvent.Call(key, 0, keyUpFlag, 0)
	procKeybdEvent.Call(mod2, 0, keyUpFlag, 0)
	procKeybdEvent.Call(mod1, 0, keyUpFlag, 0)
}

// closeWindowsTitled 关掉标题含 sub 的资源管理器窗口（sub 为空则全关）。
func closeWindowsTitled(sub string) {
	cb := syscall.NewCallback(func(h uintptr, _ uintptr) uintptr {
		if cls := className(h); !strings.EqualFold(cls, "CabinetWClass") {
			return 1
		}
		if sub != "" && !strings.Contains(winText(h), sub) {
			return 1
		}
		procPostMessage.Call(h, wmClose, 0, 0)
		return 1
	})
	procEnumWindows.Call(cb, 0)
}

func firstVisibleWithTitle(sub string) uintptr {
	var res uintptr
	cb := syscall.NewCallback(func(h uintptr, _ uintptr) uintptr {
		if res != 0 {
			return 1
		}
		if vis, _, _ := procIsWindowVisible.Call(h); vis == 0 {
			return 1
		}
		if cls := className(h); !strings.EqualFold(cls, "CabinetWClass") {
			return 1
		}
		if strings.Contains(winText(h), sub) {
			res = h
			return 0
		}
		return 1
	})
	procEnumWindows.Call(cb, 0)
	return res
}

// dumpExplorers 打印此刻所有资源管理器窗口的面包屑路径，以及当前前台是谁。
// 跟随器读不到目录时，第一步就是看这里：窗口有没有、路径读不读得出。
func dumpExplorers() {
	fg, _, _ := procGetForegroundWindow.Call()
	fmt.Printf("   前台 %#x cls=%q\n", fg, className(fg))
	cb := syscall.NewCallback(func(h uintptr, _ uintptr) uintptr {
		if vis, _, _ := procIsWindowVisible.Call(h); vis == 0 {
			return 1
		}
		cls := className(h)
		if !strings.EqualFold(cls, "CabinetWClass") && !strings.EqualFold(cls, "ExploreWClass") {
			return 1
		}
		fmt.Printf("   资源管理器 %#x title=%q 面包屑=%q\n", h, winText(h), breadcrumb(h))
		return 1
	})
	procEnumWindows.Call(cb, 0)
}

// breadcrumb 找 Breadcrumb Parent 下的 ToolbarWindow32 读文本（和跟随器同一条读法）。
func breadcrumb(root uintptr) string {
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
	procEnumChildWin.Call(root, cb, 0)
	if toolbar == 0 {
		return ""
	}
	return strings.TrimSpace(text(toolbar))
}
