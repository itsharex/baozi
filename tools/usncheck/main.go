//go:build ignore

// usncheck 校验 USN 建出来的索引：查几个词、打印几条完整路径与拼音。
// 用法：go run tools/usncheck/main.go <index.bin> [查询词...]
package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"baozi/internal/index"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("用法: usncheck <index.bin> [词...]")
		os.Exit(2)
	}
	path := os.Args[1]
	terms := os.Args[2:]
	if len(terms) == 0 {
		terms = []string{"snowshot", "db", "s"}
	}

	t0 := time.Now()
	tab, roots, err := index.Load(path)
	if err != nil {
		fmt.Printf("加载失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("加载: %d 项 roots=%v 耗时 %v\n", tab.Len(), roots, time.Since(t0).Round(time.Millisecond))

	// 类型分布
	var files, dirs int
	for i := 0; i < tab.Len(); i++ {
		if tab.Nodes[i].Kind == index.KindDir {
			dirs++
		} else {
			files++
		}
	}
	fmt.Printf("类型: 文件 %d 目录 %d\n", files, dirs)

	// 路径非空的抽样
	fmt.Println("抽样（前 6 条带完整路径的文件）:")
	shown := 0
	for i := 0; i < tab.Len() && shown < 6; i++ {
		if tab.Nodes[i].Kind == index.KindDir {
			continue
		}
		p := tab.PathOf(int32(i))
		if !strings.Contains(p, `\`) || len(p) < 8 {
			continue
		}
		fmt.Printf("  %s\n", p)
		shown++
	}

	// 拼音样本
	fmt.Println("拼音样本:")
	shown = 0
	for i := 0; i < tab.Len() && shown < 6; i++ {
		if py := tab.PyOf(i); py != "" {
			fmt.Printf("  %s → %s\n", tab.Name(i), py)
			shown++
		}
	}

	for _, term := range terms {
		t1 := time.Now()
		hits := 0
		var first []string
		for i := 0; i < tab.Len(); i++ {
			if !tab.Hits(i, []string{term}) {
				continue
			}
			hits++
			if len(first) < 3 {
				first = append(first, tab.PathOf(int32(i)))
			}
		}
		fmt.Printf("查 %q: 命中 %d 耗时 %v\n", term, hits, time.Since(t1).Round(time.Millisecond))
		for _, p := range first {
			fmt.Printf("    %s\n", p)
		}
	}
}
