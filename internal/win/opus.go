// Directory Opus 当前目录的读取。
//
// 照 Lertaro 的 Plugins/DirectoryOpus/DirectoryOpusPathCollector.cs：
// lister 主窗口类名 `dopus.lister`，每个"文件显示容器"（`dopus.filedisplaycontainer`）
// 对应一个标签页；活动那个标签的容器里有一个地址栏控件 `dopus.ctl.treepath`，
// 它的文本就是完整路径；非活动标签没有地址栏，读容器自己的 window text（Opus 把
// 每个标签的目录放在自己的窗口标题上）。
//
// 双栏 lister 有多个可见容器，要选"用户正在看的那一栏"。Lertaro 的选法有三层：
// 窗口标题唯一匹配 → 焦点控件落在哪个容器里 → 按几何距离猜。这里只保留前两层：
// 猜错的代价是"把对话框跳到另一栏的目录"，比不跟随更糟，所以第三层宁可不做。
package win

import (
	"path/filepath"
	"strings"
)

const (
	opusContainerClass = "dopus.filedisplaycontainer"
	// 活动标签的地址栏控件（Opus 自己的类名）
	opusLocationBarClass = "dopus.ctl.treepath"
)

// OpusFolder 读 Directory Opus 活动标签所在目录。读不到返回空串。
func OpusFolder(root uintptr) string {
	if !IsDirectoryOpus(root) {
		return ""
	}
	containers := opusVisibleContainers(root)
	switch len(containers) {
	case 0:
		return ""
	case 1:
		return opusContainerPath(containers[0])
	}

	// 双栏：先看窗口标题能不能唯一定位某一栏（Opus 的标题前缀就是当前目录名）。
	if p := opusPathByTitle(root, containers); p != "" {
		return p
	}
	// 标题判断不出来时，看键盘焦点落在哪一栏 —— 那是用户正在操作的。
	if p := opusPathByFocus(root, containers); p != "" {
		return p
	}
	// 两条都判不出就不猜：跳到错的目录比不跳更糟。
	return ""
}

// opusVisibleContainers 列出所有可见的文件显示容器（不可见的是非当前视图模式/非活动标签）。
func opusVisibleContainers(root uintptr) []uintptr {
	var out []uintptr
	for _, child := range childWindows(root) {
		if !strings.EqualFold(className(child), opusContainerClass) {
			continue
		}
		if vis, _, _ := procIsWindowVisible.Call(child); vis == 0 {
			continue
		}
		out = append(out, child)
	}
	return out
}

// opusPathByTitle 用 lister 窗口标题匹配容器路径，唯一命中才算。
func opusPathByTitle(root uintptr, containers []uintptr) string {
	title := strings.TrimSpace(WindowTextTimeout(root))
	if title == "" {
		return ""
	}
	matched := ""
	count := 0
	for _, c := range containers {
		p := opusContainerPath(c)
		if p == "" {
			continue
		}
		base := filepath.Base(p)
		if strings.Contains(strings.ToLower(title), strings.ToLower(p)) ||
			(base != "" && strings.HasPrefix(strings.ToLower(title), strings.ToLower(base)+" ")) ||
			(base != "" && strings.EqualFold(title, base)) {
			matched = p
			count++
		}
	}
	if count == 1 {
		return matched
	}
	return ""
}

// opusPathByFocus 看键盘焦点控件落在哪个容器里。
func opusPathByFocus(root uintptr, containers []uintptr) string {
	focused := focusedControl(root)
	if focused == 0 {
		return ""
	}
	for _, c := range containers {
		if isDescendantOf(focused, c, 12) {
			return opusContainerPath(c)
		}
	}
	return ""
}

// opusContainerPath 读一个容器的目录：优先它自己的地址栏，退而用容器 window text。
func opusContainerPath(container uintptr) string {
	if bar := findChildByClass(container, opusLocationBarClass); bar != 0 {
		if t := strings.TrimSpace(WindowTextTimeout(bar)); t != "" {
			return opusResolvePath(t)
		}
	}
	if t := strings.TrimSpace(WindowTextTimeout(container)); t != "" {
		return opusResolvePath(t)
	}
	return ""
}

// opusResolvePath 把容器报出来的文本变成"存在且能用的目录"。
//
// Opus 报的可能是：完整路径、盘根（带尾斜杠）、或者 shell 的特殊名。
// 还有一种情况 Lertaro 专门处理过——它的容器标题是**本地化拼写**（"C:\用户\…"），
// 那不是一个存在的路径。这里过一遍 resolveFolderText（已知文件夹的名字映射）就够了；
// 逐段翻译本地化目录名（LocalizedPathResolver）没有装的 Opus 可验，不做。
func opusResolvePath(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if filepath.IsAbs(text) {
		if len(text) > 3 {
			text = strings.TrimSuffix(text, `\`)
		}
		if isDir(text) {
			return text
		}
	}
	// 不是绝对路径时，可能是「文档 / 下载」这类已知文件夹的名字。
	if p := resolveFolderText(text); p != "" {
		return p
	}
	return ""
}

// isDescendantOf 从 hwnd 沿父链上溯，看是否落在 ancestor 里（深度有限，防环）。
func isDescendantOf(hwnd, ancestor uintptr, depth int) bool {
	for cur := hwnd; cur != 0 && depth > 0; depth-- {
		if cur == ancestor {
			return true
		}
		next, _, _ := procGetParent.Call(cur)
		if next == cur {
			return false
		}
		cur = next
	}
	return false
}
