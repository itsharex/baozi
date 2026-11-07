// XYplorer 当前目录的读取。
//
// 照 Lertaro 的 Plugins/Xyplorer/Win32/Win32Helper.cs。XYplorer 没有像 TC 那样的
// 专用查询命令，它暴露的是"跑一段 XYplorer 脚本"：向主窗口发 `WM_COPYDATA`，
// dwData = 0x00400001（RUN_SCRIPT），lpData 是 **UTF-16** 的脚本文本。
// 查询要拿到答案，就让脚本自己用 XYplorer 的 `copydata` 命令把结果回投到我们的
// 接收窗口（第二个方向的 WM_COPYDATA），于是同步 SendMessage 一返回，答案已经在手。
//
// 脚本原文（Lertaro 的写法，逐字照搬，包括那个 `2` 作为回复的 dwData）：
//
//	::$return = "<curpath>"; copydata <接收窗口>, "$return", 2;
//
// 注意 $return 在引号里是给 XYplorer 自己做字符串替换的，不是字面量。
package win

import (
	"encoding/binary"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf16"
)

const (
	// 发往 XYplorer 的 WM_COPYDATA：把 lpData 当作脚本立刻执行
	xyRunScript = 0x00400001
)

// XyplorerFolder 读 XYplorer 活动面板的当前目录。读不到返回空串。
func XyplorerFolder(root uintptr) string {
	if !IsXyplorer(root) {
		return ""
	}

	reply, ok := queryCopyData(root, xyRunScript, func(receiver uintptr) []byte {
		script := "::$return = \"<curpath>\"; copydata " +
			strconv.FormatUint(uint64(receiver), 10) + ", \"$return\", 2;"
		return utf16LEBytes(script)
	})
	if !ok {
		return ""
	}

	p := strings.TrimSpace(reply.text)
	if p == "" || !filepath.IsAbs(p) || !isDir(p) {
		return ""
	}
	// XYplorer 回的是 "D:\Notes\"（带尾斜杠）；盘根 "D:\" 别削成 "D:"。
	if len(p) > 3 {
		p = strings.TrimSuffix(p, `\`)
	}
	return p
}

// utf16LEBytes 把字符串编成 UTF-16LE 字节（不含结尾 NUL）——
// 与 .NET 的 Encoding.Unicode.GetBytes 一致，XYplorer 就是这么读 lpData 的。
func utf16LEBytes(s string) []byte {
	units := utf16.Encode([]rune(s))
	buf := make([]byte, len(units)*2)
	for i, u := range units {
		binary.LittleEndian.PutUint16(buf[i*2:], u)
	}
	return buf
}
