// xplorer2 当前目录的读取。
//
// 照 Lertaro 的 Plugins/Xplorer2/Win32/{Win32Helper,WindowProbe}.cs。xplorer2 没有任何
// 远程控制接口（没有 WM_COPYDATA 处理器、没有 DDE、没有 IDispatch），只能读窗口：
// 它的地址栏是一个 `ComboBox`，**WM_GETTEXT** 的回复就是活动面板的完整路径
// （同一句柄用 GetWindowText 拿回来是空的 —— 这一点 Lertaro 专门量过）。
//
// 两个坑：
//
//  1. 一帧里有**两个** ComboBox —— 传统地址栏（显示面包屑时它被隐藏）和面包屑自己那个。
//     隐藏的那个还留着上一次访问的路径（实测活动面板在 D:\Notes 时，隐藏的传统栏
//     仍报 C:\Windows）。所以必须**只看可见的**，看不见的一律不算 —— 拿一个指错目录
//     的结果去导航，比不导航更糟。
//  2. 编辑中途 ComboBox 会短暂报出纯数字之类的垃圾，还有 `::` 开头的 shell 命名空间
//     （此电脑之类），都不是文件系统路径，要滤掉。
package win

import (
	"path/filepath"
	"strconv"
	"strings"
)

const (
	xplorer2FrameClass = "ATL:ExplorerFrame"
	// 面包屑自己在 ATL 里是无名窗口，类名带地址（ATL:%p），只能按**文字**认它
	xplorer2CrumbBarText = "CrumbBar"
)

// Xplorer2Folder 读 xplorer2 活动面板的当前目录。读不到返回空串。
func Xplorer2Folder(root uintptr) string {
	if !IsXplorer2(root) {
		return ""
	}

	var crumbBar uintptr
	var fallback string
	for _, child := range childWindows(root) {
		cls := className(child)
		if crumbBar == 0 && strings.HasPrefix(strings.ToUpper(cls), "ATL:") &&
			strings.EqualFold(strings.TrimSpace(WindowTextTimeout(child)), xplorer2CrumbBarText) {
			crumbBar = child
			continue
		}
		if !strings.EqualFold(cls, "ComboBox") {
			continue
		}
		if vis, _, _ := procIsWindowVisible.Call(child); vis == 0 {
			continue // 隐藏的那条报的是上一次访问的路径，不能用
		}
		p := xplorer2ChoosePath(WindowTextTimeout(child))
		if p == "" {
			continue
		}
		// 面包屑自己的那条 ComboBox 才是跟着焦点面板走的，优先用它。
		parent, _, _ := procGetParent.Call(child)
		if crumbBar != 0 && parent == crumbBar {
			return p
		}
		if fallback == "" {
			fallback = p
		}
	}
	return fallback
}

// xplorer2ChoosePath 挑出一个可用的路径，顺便做规范化（盘根保留尾斜杠，其余去掉）。
func xplorer2ChoosePath(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	// 编辑中途报出的纯数字
	if _, err := strconv.ParseInt(text, 10, 64); err == nil {
		return ""
	}
	// shell 命名空间（"::::{CLSID}" 这种），不是文件系统路径
	if strings.HasPrefix(text, "::") {
		return ""
	}
	if !filepath.IsAbs(text) {
		return ""
	}
	if len(text) > 3 {
		text = strings.TrimSuffix(text, `\`)
	}
	if !isDir(text) {
		return ""
	}
	return text
}
