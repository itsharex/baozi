// appsprobe 探针：打印"已安装应用"对某个查询的真实匹配结果，
// 用来核对 appAlias 与 AccuracyPy 的效果（不启动 Baozi 面板）。
//
//	go run ./tools/appsprobe cal
package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"baozi/internal/pinyin"
	"baozi/internal/search"

	"github.com/egoist/mygo"
)

type hit struct {
	name  string
	alias string
	path  string
	acc   float64
}

func main() {
	q := "cal"
	if len(os.Args) > 1 {
		q = os.Args[1]
	}

	mygo.App.SetName("appsprobe")
	mygo.App.SetActivationPolicy(mygo.ActivationPolicyAccessory)
	mygo.App.WhenReady(func() {
		go func() {
			apps, err := mygo.Shell.InstalledApps()
			if err != nil {
				fmt.Println("读取已安装应用失败:", err)
				os.Exit(1)
			}
			var hits []hit
			for _, app := range apps {
				alias := search.AppAlias(app.ID)
				acc, ok := search.AccuracyPy(q, app.Name, "", pinyin.Initials(app.Name), pinyin.Full(app.Name))
				if alias != "" {
					if alt, ok2 := search.AccuracyPy(q, alias, "", "", ""); ok2 && alt > acc {
						acc, ok = alt, true
					}
				}
				if !ok {
					continue
				}
				hits = append(hits, hit{app.Name, alias, app.Path, acc})
			}
			sort.SliceStable(hits, func(i, j int) bool { return hits[i].acc > hits[j].acc })

			fmt.Printf("查询 %q，共 %d 个应用，命中 %d 个：\n", q, len(apps), len(hits))
			for i, h := range hits {
				if i >= 12 {
					break
				}
				fmt.Printf("  %.2f  %-28s  alias=%-26s  %s\n", h.acc, h.name, h.alias, h.path)
			}
			if len(hits) == 0 {
				fmt.Println("  （无命中）")
			}
			_ = strings.TrimSpace
			os.Exit(0)
		}()
	})
	if err := mygo.App.Run(); err != nil {
		fmt.Println("App.Run:", err)
		os.Exit(1)
	}
}
