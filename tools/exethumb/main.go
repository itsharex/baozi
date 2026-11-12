// 探针：win.Thumbnail 对 .exe 能不能给出图标（做首页卡片预览要用真实彩色图标，
// 而 ui.Tester 里 Shell.FileIcon 会永久阻塞，只能走这条自家 COM 路径）。
package main

import (
	"fmt"
	"os"
	"path/filepath"

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
	for _, p := range targets {
		if _, err := os.Stat(p); err != nil {
			fmt.Printf("skip  %s（不存在）\n", p)
			continue
		}
		data, err := win.Thumbnail(p, 200)
		fmt.Printf("%-46s %5d bytes  err=%v\n", filepath.Base(p), len(data), err)
		if len(data) > 0 {
			out := filepath.Join(`D:\work\baozi\dist\probe-icons`, filepath.Base(p)+".png")
			_ = os.MkdirAll(filepath.Dir(out), 0o755)
			_ = os.WriteFile(out, data, 0o644)
		}
	}
}
