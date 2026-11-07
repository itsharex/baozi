// folderprobe：打印 Baozi 对各文件管理器"当前目录"的读取结果。
//
// 用来验证两件事：
//  1. 资源管理器在「下载 / 文档 / 音乐」这类已知文件夹的根上，地址栏给的是本地化名字，
//     win.ExplorerFolder 是否已把它还原成真实路径；
//  2. Total Commander（TTOTAL_CMD）能否通过 WM_COPYDATA 远程协议读到当前活动面板目录。
//
// 用法：D:\Go\bin\go.exe run ./tools/folderprobe
package main

import (
	"fmt"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"

	"baozi/internal/win"
)

var (
	user32  = windows.NewLazySystemDLL("user32.dll")
	shell32 = windows.NewLazySystemDLL("shell32.dll")
	ole32   = windows.NewLazySystemDLL("ole32.dll")

	procEnumWindows            = user32.NewProc("EnumWindows")
	procEnumChildWindows       = user32.NewProc("EnumChildWindows")
	procGetClassName           = user32.NewProc("GetClassNameW")
	procGetWindowText          = user32.NewProc("GetWindowTextW")
	procGetParent              = user32.NewProc("GetParent")
	procIsWindowVisible        = user32.NewProc("IsWindowVisible")
	procSHGetKnownFolderIDList = shell32.NewProc("SHGetKnownFolderIDList")
	procSHGetNameFromIDList    = shell32.NewProc("SHGetNameFromIDList")
	procSHGetPathFromIDListW   = shell32.NewProc("SHGetPathFromIDListW")
	procILFree                 = shell32.NewProc("ILFree")
	procCLSIDFromString        = ole32.NewProc("CLSIDFromString")
	procCoTaskMemFree          = ole32.NewProc("CoTaskMemFree")
	procSendMessageW           = user32.NewProc("SendMessageW")
)

// WM_GETTEXT：xplorer2 的地址栏 ComboBox 只有这个能拿到文本（GetWindowText 返回空）。
const wmGetText = 0x000D

// SIGDN_NORMALDISPLAY：shell 给这个条目"人看到的名字"（对已知文件夹就是本地化名）。
const sigdnNormalDisplay = 0

type guid struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

// 与 internal/win/knownfolder.go 的 knownFolderIDs 前几项对齐，用来对比两种 API 拿到的名字。
var probeIDs = []struct{ name, clsid string }{
	{"Desktop", "{B4BFCC3A-DB2C-424C-B029-7FE99A87C641}"},
	{"Downloads", "{374DE290-123F-4565-9164-39C4925E467B}"},
	{"Documents", "{FDD39AD0-238F-46AF-ADB4-6C85480369C7}"},
	{"Music", "{4BD8D571-6D19-48D3-BE97-422220080E43}"},
	{"Pictures", "{33E28130-4E1E-4676-835A-98395C3BC3BB}"},
	{"Videos", "{18989B1D-99B5-455B-841C-AB7C74E4DDFC}"},
}

func className(h uintptr) string {
	var buf [256]uint16
	procGetClassName.Call(h, uintptr(unsafe.Pointer(&buf[0])), 256)
	return windows.UTF16ToString(buf[:])
}

func windowText(h uintptr) string {
	var buf [512]uint16
	procGetWindowText.Call(h, uintptr(unsafe.Pointer(&buf[0])), 512)
	return windows.UTF16ToString(buf[:])
}

func main() {
	win.TCDebugHook = func(f string, a ...any) { fmt.Printf("[TC] "+f+"\n", a...) }

	fmt.Println("--- 两种 API 拿已知文件夹名字的对比 ---")
	for _, it := range probeIDs {
		disp, path := probeName(it.clsid)
		fmt.Printf("  %-10s path=%-28s SHGetNameFromIDList=%q\n", it.name, path, disp)
	}
	fmt.Println()

	var found int
	cb := syscall.NewCallback(func(h uintptr, _ uintptr) uintptr {
		if vis, _, _ := procIsWindowVisible.Call(h); vis == 0 {
			return 1
		}
		cls := className(h)
		switch {
		case strings.EqualFold(cls, "CabinetWClass") || strings.EqualFold(cls, "ExploreWClass"):
			found++
			fmt.Printf("[资源管理器] %#x title=%q\n             地址栏原文=%q\n             解析目录=%q\n",
				h, windowText(h), addressRaw(h), win.ExplorerFolder(h))
		case strings.EqualFold(cls, "TTOTAL_CMD"):
			found++
			fmt.Printf("[TotalCommander] %#x title=%q\n             目录=%q\n",
				h, windowText(h), win.TotalCommanderFolder(h))
		case win.IsDirectoryOpus(h):
			found++
			fmt.Printf("[DirectoryOpus] %#x title=%q\n             容器：%s\n             目录=%q\n",
				h, windowText(h), opusDump(h), win.OpusFolder(h))
		case win.IsXyplorer(h):
			found++
			fmt.Printf("[XYplorer] %#x title=%q\n             目录=%q\n",
				h, windowText(h), win.XyplorerFolder(h))
		case win.IsXplorer2(h):
			found++
			fmt.Printf("[xplorer2] %#x title=%q\n             地址栏：%s\n             目录=%q\n",
				h, windowText(h), xplorer2Dump(h), win.Xplorer2Folder(h))
		}
		return 1
	})
	procEnumWindows.Call(cb, 0)
	if found == 0 {
		fmt.Println("没找到任何受支持的文件管理器窗口")
		fmt.Println("（Directory Opus / XYplorer / xplorer2 未安装时自然不会出现；" +
			"这也正是「装了别的程序不会被误判成来源」的检查）")
	}
}

// opusDump 列出 Opus lister 里的文件显示容器、它们的可见性与地址栏原文。
func opusDump(root uintptr) string {
	var parts []string
	cb := syscall.NewCallback(func(c uintptr, _ uintptr) uintptr {
		if !strings.EqualFold(className(c), "dopus.filedisplaycontainer") {
			return 1
		}
		vis, _, _ := procIsWindowVisible.Call(c)
		bar := ""
		var barCb = syscall.NewCallback(func(b uintptr, _ uintptr) uintptr {
			if bar == "" && strings.EqualFold(className(b), "dopus.ctl.treepath") {
				bar = windowText(b)
			}
			return 1
		})
		procEnumChildWindows.Call(c, barCb, 0)
		parts = append(parts, fmt.Sprintf("%#x(visible=%v, bar=%q, text=%q)", c, vis != 0, bar, windowText(c)))
		return 1
	})
	procEnumChildWindows.Call(root, cb, 0)
	if len(parts) == 0 {
		return "无"
	}
	return strings.Join(parts, " ")
}

// xplorer2Dump 列出 xplorer2 里所有 ComboBox 的可见性、父窗口与 WM_GETTEXT 结果。
func xplorer2Dump(root uintptr) string {
	var parts []string
	cb := syscall.NewCallback(func(c uintptr, _ uintptr) uintptr {
		if !strings.EqualFold(className(c), "ComboBox") {
			return 1
		}
		vis, _, _ := procIsWindowVisible.Call(c)
		parent, _, _ := procGetParent.Call(c)
		text := ""
		var buf [512]uint16
		procSendMessageW.Call(c, wmGetText, 512, uintptr(unsafe.Pointer(&buf[0])))
		text = windows.UTF16ToString(buf[:])
		parts = append(parts, fmt.Sprintf("%#x(visible=%v, parent=%#x[%s], getText=%q)",
			c, vis != 0, parent, className(parent), text))
		return 1
	})
	procEnumChildWindows.Call(root, cb, 0)
	if len(parts) == 0 {
		return "无"
	}
	return strings.Join(parts, " ")
}

// probeName 用 KnownFolder 的 PIDL 问 shell"这个名字叫什么"，同时拿真实路径。
func probeName(clsid string) (display, path string) {
	p, err := syscall.UTF16PtrFromString(clsid)
	if err != nil {
		return "", ""
	}
	var g guid
	if hr, _, _ := procCLSIDFromString.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&g))); hr != 0 {
		return "", ""
	}
	var pidl unsafe.Pointer
	if hr, _, _ := procSHGetKnownFolderIDList.Call(
		uintptr(unsafe.Pointer(&g)), 0, 0, uintptr(unsafe.Pointer(&pidl))); hr != 0 || pidl == nil {
		return "", ""
	}
	defer procILFree.Call(uintptr(pidl))

	var name *uint16
	if hr, _, _ := procSHGetNameFromIDList.Call(
		uintptr(pidl), sigdnNormalDisplay,
		uintptr(unsafe.Pointer(&name))); hr == 0 && name != nil {
		display = windows.UTF16PtrToString(name)
		procCoTaskMemFree.Call(uintptr(unsafe.Pointer(name)))
	}

	var buf [260]uint16
	if r, _, _ := procSHGetPathFromIDListW.Call(uintptr(pidl), uintptr(unsafe.Pointer(&buf[0]))); r != 0 {
		path = windows.UTF16ToString(buf[:])
	}
	return display, path
}

// addressRaw 直接读资源管理器地址栏的原文（不解析），用来核对 ExplorerFolder 的输入。
func addressRaw(root uintptr) string {
	var toolbar uintptr
	cb := syscall.NewCallback(func(c uintptr, _ uintptr) uintptr {
		if toolbar != 0 {
			return 1
		}
		if strings.EqualFold(className(c), "ToolbarWindow32") {
			parent, _, _ := procGetParent.Call(c)
			if parent != 0 && strings.EqualFold(className(parent), "Breadcrumb Parent") {
				toolbar = c
				return 0
			}
		}
		return 1
	})
	procEnumChildWindows.Call(root, cb, 0)
	if toolbar == 0 {
		return ""
	}
	return windowText(toolbar)
}
