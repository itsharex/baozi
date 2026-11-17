//go:build ignore

// uiprobe 是一次性测量工具：只建一个 mygo 面板窗口，不做任何业务，
// 用来量出"框架 + D3D/DComp 的固定内存成本"，好把 baozi 主进程的 WS
// 拆成"框架底线"和"我们自己的部分"。
//
//	go run tools\memprobe\uiprobe.go [plain|glass]
package main

import (
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
)

func main() {
	mode := "plain"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	mygo.App.SetName("uiprobe")
	mygo.App.SetActivationPolicy(mygo.ActivationPolicyAccessory)
	mygo.App.OnWindowAllClosed(func() {})
	mygo.App.WhenReady(func() {
		opts := mygo.WindowOptions{
			Title:             "uiprobe-" + mode,
			Panel:             true,
			TitleBarStyle:     mygo.TitleBarHidden,
			HideWindowButtons: true,
			Transparent:       true,
			Width:             878,
			Height:            510,
			Content: ui.View(func(c *ui.Context) {
				ui.Text(c, "uiprobe "+mode)
			}),
		}
		if mode == "glass" {
			opts.Vibrancy = mygo.VibrancyGlass
			opts.CornerRadius = 16
		}
		w := mygo.NewWindow(opts)
		w.Show()
		// 每 3 秒把 Go 自己的统计打一遍，测量方从外面读 WS。
		go func() {
			for i := 0; i < 7; i++ {
				time.Sleep(3 * time.Second)
				var m runtime.MemStats
				runtime.ReadMemStats(&m)
				fmt.Printf("[%ds] heapInuse=%.1fMB sys=%.1fMB objs=%d\n",
					(i+1)*3, float64(m.HeapInuse)/(1<<20), float64(m.Sys)/(1<<20), m.HeapObjects)
			}
			mygo.App.Quit()
		}()
	})
	if err := mygo.App.Run(); err != nil {
		fmt.Println("run:", err)
	}
}
