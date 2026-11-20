package builtin

import (
	"bytes"
	_ "embed"
	"os"
	"path/filepath"
	"strings"
)

// shutdown-after.ps1 里的中文提示必须能被 PowerShell 5.1 正确解码，所以落盘时
// 前面要补 UTF-8 BOM（和 apply-update.ps1 同一个坑：没有 BOM 时 PS 5.1 按系统
// ANSI/GBK 读，中文会把引号吃掉，脚本一跑就是语法错误）。
//
// 仓库里这一份保持不带 BOM 的纯 UTF-8：git diff 干净，编辑器也不会把它当
// "带奇怪字符的文件"。BOM 只在写进 %LOCALAPPDATA% 时加。
//
//go:embed shutdown-after.ps1
var shutdownScript []byte

// ShutdownName 是内置脚本的文件名。自定义命令里靠这个文件名认"这是内置那条"，
// 所以迁移时按 basename 匹配，用户把脚本挪到别处也认得出来。
const ShutdownName = "shutdown-after.ps1"

var bom = []byte{0xEF, 0xBB, 0xBF}

// Dir 是内置脚本落盘的目录：跟着配置走（%LOCALAPPDATA%\Baozi\scripts），
// 不放 Program Files —— 那里没管理员权限写不进去，而这份文件每次启动都要核对。
func Dir() string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "Baozi", "scripts")
}

// ShutdownPath 是内置关机脚本的绝对路径（不保证文件已经写好）。
func ShutdownPath() string { return filepath.Join(Dir(), ShutdownName) }

// EnsureShutdown 把内置脚本写到 ShutdownPath，返回路径。
//
// 内容不一样就重写（升级版本后脚本跟着更新），一样就不动 —— 每次启动写一遍
// 也不会把文件的修改时间搅乱。写完就返回，调用方拿到路径直接当命令的 exec。
func EnsureShutdown() (string, error) {
	dst := ShutdownPath()
	want := append(append([]byte(nil), bom...), stripBOM(shutdownScript)...)
	if old, err := os.ReadFile(dst); err == nil && bytes.Equal(old, want) {
		return dst, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	tmp := dst + ".new"
	if err := os.WriteFile(tmp, want, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return dst, nil
}

// stripBOM 防止仓库里那份万一被编辑器存成带 BOM，落盘时又叠一层。
func stripBOM(b []byte) []byte {
	if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		return b[3:]
	}
	return b
}

// IsShutdownScript 判断一条命令的 exec 是不是这份内置脚本（按文件名认）。
func IsShutdownScript(exec string) bool {
	exec = filepath.Base(filepath.FromSlash(exec))
	return equalFoldASCII(exec, ShutdownName)
}

// ShutdownRef 是配置里指向内置脚本的写法。
//
// 存这一串而不是绝对路径：%LOCALAPPDATA% 换用户、换机器就变了，写死在配置里
// 搬家/换号之后命令就指向一个不存在的文件。运行时由 launchSpec 换成实际路径，
// 所以配置挪到哪台机器上都还能跑。
const ShutdownRef = "builtin:shutdown"

// Resolve 把 "builtin:xxx" 换成落盘后的真实路径；不是内置写法就返回 ok=false。
//
// 每次启动已经 Ensure 过一次，这里再 Ensure 一次是给"脚本被误删"兜底 ——
// 用户点命令那一刻文件不在，就现写一份，而不是报一个没头没尾的路径错误。
func Resolve(ref string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(ref)) {
	case ShutdownRef:
		path, err := EnsureShutdown()
		if err != nil {
			// 写不进去也把路径交出去：至少错误信息里有个具体位置可看。
			return ShutdownPath(), true
		}
		return path, true
	}
	return "", false
}

func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
