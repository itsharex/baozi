// Total Commander 当前目录的读取。
//
// Lertaro 走的是 TC 的远程控制协议（Plugins/TotalCommander/Win32/Win32Helper.cs）：
// 主窗口类名是 `TTOTAL_CMD`，向它发一条 `WM_COPYDATA` 查询
// （dwData = 'G' + 256·'W'，lpData 是 **ASCII** 的 "SP\0" 或 "TP\0"），
// TC 把答案以 `WM_COPYDATA`（dwData = 'R' + 256·'W'，内容 UTF-16）回给调用方
// 在 wParam 里指定的窗口。命令串必须是 ANSI，只有"回复用 UTF-16"这一半由 'W' 决定
// —— 用 UTF-16 发命令 TC 不会理你（Lertaro 源码里专门写了这条）。
//
// 收发那套（同线程建接收窗口、不带 SMTO_BLOCK）在 copydata.go，TC 与 XYplorer 共用；
// 这里只剩 TC 自己的命令编码和回复校验。
package win

import (
	"path/filepath"
	"strings"
)

const (
	// 查询命令：'G' + 256*'W'（要求 TC 用 UTF-16 回话）
	getRequestW = uintptr('G') + 256*uintptr('W')
	// 回复标记：'R' + 256*'W'
	getReplyW = uintptr('R') + 256*uintptr('W')
)

// TCDebugHook 非 nil 时，TotalCommanderFolder 会把每一步细节报出来（探针 / 排障用）。
var TCDebugHook func(format string, args ...any)

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

	// 命令串必须是 ANSI（这里全 ASCII），末尾带 NUL 并计入 cbData。
	reply, ok := queryCopyData(root, getRequestW, func(uintptr) []byte {
		return []byte{'S', 'P', 0}
	})
	tcDebug("发 %#x：ok=%v dwData=%#x", root, ok, reply.dwData)
	if !ok {
		tcDebug("回复为空")
		return ""
	}
	// TC 的回复带自己的标记；对不上就当作不是答案（例如对方正忙回了别的东西）。
	if reply.dwData != getReplyW {
		tcDebug("回复标记不对：%#x", reply.dwData)
		return ""
	}

	p := strings.TrimSpace(reply.text)
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
