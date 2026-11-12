// 探针：win.Thumbnail 对 .exe 能不能给出图标（做首页卡片预览要用真实彩色图标，
// 而 ui.Tester 里 Shell.FileIcon 会永久阻塞，只能走这条自家 COM 路径）。
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"baozi/internal/win"
)

func main() {
	targets := []string{
		`C:\Windows\System32\notepad.exe`,
		`C:\Windows\System32\mspaint.exe`,
		`C:\Windows\System32\calc.exe`,
		`C:\Windows\System32\mstsc.exe`,
		filepath.Join(os.Getenv("ProgramFiles"), `Google\Chrome\Application\chrome.exe`),
	}
	// 命令行给的路径排在最前面，方便点着试某个具体程序。
	targets = append(os.Args[1:], targets...)
	for _, p := range targets {
		if _, err := os.Stat(p); err != nil {
			fmt.Printf("skip  %s（不存在）\n", p)
			continue
		}
		data, err := win.Thumbnail(p, 200)
		fmt.Printf("%-46s %5d bytes  err=%v\n", filepath.Base(p), len(data), err)
		if len(data) == 0 {
			continue
		}
		out := filepath.Join(`D:\work\baozi\dist\probe-icons`, filepath.Base(p)+".png")
		_ = os.MkdirAll(filepath.Dir(out), 0o755)
		_ = os.WriteFile(out, data, 0o644)
		// 顺手过一遍 TrimIcon：白底/灰框那种留白只有它能裁掉，肉眼对比两张图最快。
		if trimmed, err := win.TrimIcon(data); err == nil {
			_ = os.WriteFile(strings.TrimSuffix(out, ".png")+".trim.png", trimmed, 0o644)
			fmt.Printf("    → trim 后 %d bytes\n", len(trimmed))
		}
	}
}
