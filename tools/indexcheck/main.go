// indexcheck 直接映射 %LOCALAPPDATA%\Baozi 里的真实索引，抽查几条，
// 用来确认"mmap 读出来的字节"和"当初写进去的"对得上。
//
//	BAOZI_DATA_DIR=<目录> go run ./tools/indexcheck [-q doc]
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"baozi/internal/index"
)

func main() {
	dir := os.Getenv("BAOZI_DATA_DIR")
	if dir == "" {
		dir = filepath.Join(os.Getenv("LOCALAPPDATA"), "Baozi")
	}
	path, mt, err := index.Latest(dir)
	if err != nil {
		fmt.Printf("找不到索引: %v\n", err)
		os.Exit(1)
	}
	t0 := time.Now()
	tab, roots, err := index.Load(path)
	if err != nil {
		fmt.Printf("加载失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("文件 %s（%.1f MB，改于 %s）\n", filepath.Base(path),
		float64(mustStat(path))/1e6, mt.Format("2006-01-02 15:04:05"))
	fmt.Printf("条目 %d，根 %v，加载耗时 %v\n", tab.Len(), roots, time.Since(t0).Round(time.Microsecond))

	// 抽查：前 5 条、目录数、拼音覆盖、路径还原
	dirs, files := 0, 0
	withPy := 0
	for i := 0; i < tab.Len(); i++ {
		switch tab.KindOf(int32(i)) {
		case index.KindDir:
			dirs++
		case index.KindFile:
			files++
		}
		if len(tab.PyOf(i)) > 0 {
			withPy++
		}
	}
	fmt.Printf("目录 %d / 文件 %d / 带拼音 %d\n", dirs, files, withPy)

	for i := 0; i < 5 && i < tab.Len(); i++ {
		fmt.Printf("  [%d] %-28q 类型=%d 扩展=%-8s 父目录=%s\n", i, tab.Name(i), tab.KindOf(int32(i)),
			tab.Ext(i), tab.Dir(int32(i)))
	}

	q := "doc"
	if len(os.Args) > 2 {
		q = os.Args[2]
	}
	tokens := []string{strings.ToLower(q)}
	t1 := time.Now()
	hits := 0
	for i := 0; i < tab.Len(); i++ {
		if tab.Hits(i, tokens) {
			hits++
		}
	}
	fmt.Printf("预筛 %q 命中 %d 条，耗时 %v\n", q, hits, time.Since(t1).Round(time.Millisecond))

	// 拼音：主程序里走的是 PyOf 而不是 Hits（Hits 只比文件名），这里照同一条路径验一遍
	pyHits := 0
	samples := 0
	for i := 0; i < tab.Len(); i++ {
		py := tab.PyOf(i)
		if py == "" {
			continue
		}
		if strings.HasPrefix(py, q) || strings.Contains(py, q) {
			pyHits++
			if samples < 3 {
				samples++
				fmt.Printf("  拼音命中 %q → %q（%s）\n", tab.Name(i), py, tab.PathOf(int32(i)))
			}
		}
	}
	fmt.Printf("拼音 %q 命中 %d 条\n", q, pyHits)
}

func mustStat(p string) int64 {
	if info, err := os.Stat(p); err == nil {
		return info.Size()
	}
	return 0
}
