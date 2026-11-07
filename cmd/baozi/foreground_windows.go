//go:build windows

package main

import "syscall"

var (
	user32ForWin   = syscall.NewLazyDLL("user32.dll")
	kernel32ForWin = syscall.NewLazyDLL("kernel32.dll")

	procGetForegroundWindow      = user32ForWin.NewProc("GetForegroundWindow")
	procGetWindowThreadProcessID = user32ForWin.NewProc("GetWindowThreadProcessId")
	procAttachThreadInput        = user32ForWin.NewProc("AttachThreadInput")
	procSetForegroundWindow      = user32ForWin.NewProc("SetForegroundWindow")
	procBringWindowToTop         = user32ForWin.NewProc("BringWindowToTop")
	procSetFocus                 = user32ForWin.NewProc("SetFocus")
	procShowWindowForWin         = user32ForWin.NewProc("ShowWindow")
	procGetCurrentThreadID       = kernel32ForWin.NewProc("GetCurrentThreadId")
)

const swRestore = 9

// forceForeground 让 hwnd 真正成为前台窗口并拿到键盘焦点。
//
// 为什么不能只靠 mygo 的 Window.Focus()：它内部就是 SetForegroundWindow，
// 而 Windows 的前台锁会拒绝一个"不是当前前台进程、最近也没收到输入"的进程，
// 表现是任务栏图标闪一下、窗口不激活。典型复现：点一下任务栏/别的窗口，
// 再双击 Ctrl 唤出，面板出现了但打字进不去。
//
// 绕开的办法是把自己附到当前前台窗口所属线程的输入队列上（AttachThreadInput），
// 此时两者共享输入状态，SetForegroundWindow 才会被接受；改完必须成对脱离，
// 否则会把自己的窗口和前台窗口的输入队列永久绑在一起。
//
// 返回是否真的成了前台窗口，失败时调用方可以退化为普通 Show。
func forceForeground(hwnd uintptr) bool {
	if hwnd == 0 {
		return false
	}
	fg, _, _ := procGetForegroundWindow.Call()
	if fg == hwnd {
		return true
	}
	owner, _, _ := procGetWindowThreadProcessID.Call(fg, 0)
	self, _, _ := procGetCurrentThreadID.Call()

	if owner != 0 && owner != self {
		// 第三个参数 TRUE = 附加。脱离时必须传 FALSE（0），否则会把两个线程的
		// 输入队列永久绑在一起（表现为两边都收不到/重复收到按键）。
		if ok, _, _ := procAttachThreadInput.Call(self, owner, 1); ok != 0 {
			deferred := procAttachThreadInput.Call
			defer deferred(self, owner, 0)
		}
	}

	procShowWindowForWin.Call(hwnd, swRestore)
	procBringWindowToTop.Call(hwnd)
	procSetForegroundWindow.Call(hwnd)
	procSetFocus.Call(hwnd)

	now, _, _ := procGetForegroundWindow.Call()
	return now == hwnd
}
