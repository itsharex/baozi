// 用真实的自绘阴影实现（baozi/internal/win.Shadow）在面板所在的位置画一圈影，
// 用来判断"代码对不对"和"App 里为什么看不见"这两件事。
//
// 用法：cd D:\work\baozi; go run tools\shadowprobe\main.go 948 481 1103 724
// 期间截屏即可（分层窗口只有走 DWM 合成才可见，GetPixel 拍不到）。
package main

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"baozi/internal/win"
)

func main() {
	args := os.Args[1:]
	if len(args) < 4 {
		fmt.Println("用法: shadowprobe x y w h")
		return
	}
	var v [4]int
	for i := 0; i < 4; i++ {
		v[i], _ = strconv.Atoi(args[i])
	}
	s := win.NewShadow()
	if s == nil {
		fmt.Println("NewShadow 失败")
		return
	}
	ok := s.Place(0, win.ShadowSpec{X: v[0], Y: v[1], W: v[2], H: v[3], Radius: 30, Margin: 20})
	fmt.Println("Place ->", ok, s.Debug())
	if err := s.DumpAlpha(`D:\work\baozi\dist\shadow_alpha.png`); err != nil {
		fmt.Println("DumpAlpha 失败:", err)
	} else {
		fmt.Println("alpha 图已写到 dist\\shadow_alpha.png")
	}
	s.Show()
	fmt.Println("Show 完成，8 秒后退出")
	time.Sleep(8 * time.Second)
	s.Close()
}
