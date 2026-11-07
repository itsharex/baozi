// 按窗口句柄查它属于哪个进程（只取 exe 名，小写、不带扩展名）。
//
// XYplorer 与 xplorer2 的窗口类名都是"通用类"（VB6 的 ThunderRT6FormDC、ATL 框架类），
// 别的程序也会用，所以必须再过一道进程名才算数 —— Lertaro 的 collector 也是这么做的。
// OneCommander / Files 的类名更是由框架自动生成（含 guid）或全机共用，进程名是唯一可靠的判据。
package win

import (
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	procOpenProcess               = kernel32.NewProc("OpenProcess")
	procQueryFullProcessImageName = kernel32.NewProc("QueryFullProcessImageNameW")
	procCloseHandle               = kernel32.NewProc("CloseHandle")
)

const (
	// PROCESS_QUERY_LIMITED_INFORMATION：只查信息、不要读写权限，
	// 普通权限下也能问别的用户 / 更高权限进程的 exe 路径。
	processQueryLimitedInformation = 0x1000
)

// processExeName 返回窗口所属进程的 exe 名（小写、去扩展名）。查不到返回空串。
func processExeName(hwnd uintptr) string {
	if hwnd == 0 {
		return ""
	}
	var pid uint32
	procGetWindowThreadProcess.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	if pid == 0 {
		return ""
	}
	h, _, _ := procOpenProcess.Call(processQueryLimitedInformation, 0, uintptr(pid))
	if h == 0 {
		return ""
	}
	defer procCloseHandle.Call(h)

	buf := make([]uint16, windows.MAX_PATH)
	size := uint32(len(buf))
	r, _, _ := procQueryFullProcessImageName.Call(h, 0,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)))
	if r == 0 || size == 0 {
		return ""
	}
	name := filepath.Base(windows.UTF16ToString(buf[:size]))
	name = strings.TrimSuffix(name, filepath.Ext(name))
	return strings.ToLower(name)
}

// IsDirectoryOpus 判断是不是 Directory Opus 的 lister 主窗口。
func IsDirectoryOpus(root uintptr) bool {
	return root != 0 && strings.EqualFold(className(root), "dopus.lister")
}

// IsXyplorer 判断是不是 XYplorer 主窗口。
// 类名 ThunderRT6FormDC 是任何 VB6 程序的主窗体类，靠进程名 xyplorer* 才排除掉别人。
func IsXyplorer(root uintptr) bool {
	if root == 0 || !strings.EqualFold(className(root), "ThunderRT6FormDC") {
		return false
	}
	return strings.HasPrefix(processExeName(root), "xyplorer")
}

// IsXplorer2 判断是不是 xplorer2 的主框架窗口。
func IsXplorer2(root uintptr) bool {
	if root == 0 || !strings.EqualFold(className(root), "ATL:ExplorerFrame") {
		return false
	}
	return strings.HasPrefix(processExeName(root), "xplorer2")
}
