package main

import (
	"fmt"
	"log"
	"os"

	"baozi/internal/win"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
)

// iconprobe 在真实的 mygo 主循环里对给定路径调 Shell.FileIcon，
// 看它到底返回了什么（多少字节、什么错误、各尺寸是否都成功）。
// 用法：go run ./tools/iconprobe "D:\path\app.exe" [...]
func main() {
	paths := os.Args[1:]
	if len(paths) == 0 {
		log.Fatal("用法：iconprobe <path> [path...]")
	}
	mygo.App.SetName("IconProbe")
	mygo.App.SetActivationPolicy(mygo.ActivationPolicyAccessory)
	mygo.App.OnWindowAllClosed(func() {})
	mygo.App.WhenReady(func() {
		for _, p := range paths {
			fmt.Printf("== %s\n", p)
			for _, s := range []int{16, 32, 44, 57, 64, 96, 256} {
				data, err := mygo.Shell.FileIcon(p, s)
				line := fmt.Sprintf("   req=%-3d bytes=%-6d", s, len(data))
				if err != nil {
					line += " err=" + err.Error()
				}
				if len(data) > 0 {
					if trimmed, terr := win.TrimIcon(data); terr == nil {
						line += fmt.Sprintf(" trimmed=%d", len(trimmed))
						data = trimmed
					} else {
						line += " trimErr=" + terr.Error()
					}
					bm, derr := ui.DecodeBitmap(data)
					line += fmt.Sprintf(" decode=%v nil=%v", derr, bm == nil)
				}
				fmt.Println(line)
			}
			if data, err := mygo.Shell.FileIcon(p, 57); err == nil && len(data) > 0 {
				name := fmt.Sprintf(`D:\work\baozi\dist\probe-%d.png`, len(paths))
				_ = os.WriteFile(name, data, 0o644)
				fmt.Printf("   raw png -> %s\n", name)
			}
		}
		os.Exit(0)
	})
	if err := mygo.App.Run(); err != nil {
		log.Fatal(err)
	}
}
