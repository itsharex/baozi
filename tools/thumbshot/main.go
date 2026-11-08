// thumbshot 用产品代码（win.Thumbnail，SIIGBF_CROPTOSQUARE+BIGGERSIZEOK）取几张
// 预览图存成 PNG，用来肉眼确认「拿到的是内容缩略图、方向对、被裁成正方形」。
//
//	go run ./tools/thumbshot <输出目录> <图片...>
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"baozi/internal/win"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Println("用法: thumbshot <输出目录> <图片...>")
		return
	}
	out := os.Args[1]
	if err := os.MkdirAll(out, 0o755); err != nil {
		fmt.Println("建目录失败:", err)
		return
	}
	for _, src := range os.Args[2:] {
		t0 := time.Now()
		data, err := win.Thumbnail(src, 48)
		if err != nil {
			fmt.Printf("%-50s 失败: %v\n", filepath.Base(src), err)
			continue
		}
		dst := filepath.Join(out, filepath.Base(src)+".thumb.png")
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			fmt.Println("写出失败:", err)
			continue
		}
		fmt.Printf("%-50s %dB %s -> %s\n", filepath.Base(src), len(data), time.Since(t0).Round(time.Millisecond), dst)
	}
}
