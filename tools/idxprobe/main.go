//go:build ignore

// idxprobe 是索引正确性的烟测工具：加载快照，跑几条真实查询，
// 打印排在最前的几条结果，确认"换成新存储格式之后还搜得出来"。
// 用法：go run tools/idxprobe/main.go [索引文件] [查询词...]
package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"baozi/internal/index"
	"baozi/internal/search"
)

func main() {
	path := os.Args[1]
	queries := os.Args[2:]
	if len(queries) == 0 {
		queries = []string{"snowshot", "snowshot video", "dbgz"}
	}

	tab, roots, err := index.Load(path)
	if err != nil {
		fmt.Printf("加载失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("条目 %d，根 %v\n", tab.Len(), roots)
	fmt.Printf("带拼音的条目 %d 条\n", len(tab.Py))
	if len(queries) > 0 && queries[0] == "-py" {
		shown := 0
		for i := 0; i < tab.Len() && shown < 12; i++ {
			if py := tab.PyOf(i); py != "" {
				fmt.Printf("  %s → %s\n", tab.Name(i), py)
				shown++
			}
		}
		return
	}

	weights := search.Weights{Frequency: 0.5, Type: 0.3, Accuracy: 0.2}
	for _, q := range queries {
		tokens := strings.Fields(strings.ToLower(strings.TrimSpace(q)))
		var hits []search.Result
		start := time.Now()
		for i := 0; i < tab.Len(); i++ {
			if !tab.Hits(i, tokens) && !pyHas(tab.PyOf(i), tokens) {
				continue
			}
			name := tab.Name(i)
			acc, ok := search.AccuracyPy(q, name, "", tab.PyOf(i), "")
			if !ok {
				continue
			}
			ext := tab.Ext(i)
			kind := search.KindFile
			if tab.Nodes[i].Kind == index.KindDir {
				kind = search.KindDir
			}
			ts := search.TypeScore(ext, kind, nil)
			hits = append(hits, search.Result{
				Item:      search.Item{Name: name, Path: tab.PathOf(int32(i)), Ext: ext, Kind: kind},
				TypeScore: ts,
				Accuracy:  acc,
				Score:     search.Combine(weights, 0, ts, acc),
			})
			if len(hits) >= 4000 {
				break
			}
		}
		sort.SliceStable(hits, func(a, b int) bool { return hits[a].Score > hits[b].Score })
		fmt.Printf("\n查询 %q：命中 %d，耗时 %v\n", q, len(hits), time.Since(start).Round(100*time.Microsecond))
		for i, r := range hits {
			if i >= 8 {
				break
			}
			fmt.Printf("  %d. [%s] %s\n     %s\n", i+1, kindName(r.Kind), r.Name, r.Path)
		}
	}
}

// pyHas 是拼音预筛：拼音首字母串里包含全部 token。
func pyHas(py string, tokens []string) bool {
	if py == "" {
		return false
	}
	for _, t := range tokens {
		if !strings.Contains(py, t) {
			return false
		}
	}
	return true
}

func kindName(k search.Kind) string {
	switch k {
	case search.KindDir:
		return "目录"
	case search.KindFile:
		return "文件"
	case search.KindApp:
		return "应用"
	default:
		return "命令"
	}
}
