// rankcheck 用真实索引 + 真实历史跑一遍主程序的排序，把前 N 条连同段号打出来，
// 用来验证"应用在最前、垃圾沉底"这条改造到底有没有生效。
//
//	$env:BAOZI_DATA_DIR 可覆盖数据目录
//	go run ./tools/rankcheck -q qq -n 20
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"baozi/internal/history"
	"baozi/internal/index"
	"baozi/internal/search"
)

func main() {
	q := flag.String("q", "doc", "查询词")
	n := flag.Int("n", 20, "打印前几条")
	flag.Parse()

	dir := os.Getenv("BAOZI_DATA_DIR")
	if dir == "" {
		dir = filepath.Join(os.Getenv("LOCALAPPDATA"), "Baozi")
	}
	path, _, err := index.Latest(dir)
	if err != nil {
		fmt.Printf("找不到索引: %v\n", err)
		os.Exit(1)
	}
	t0 := time.Now()
	tab, _, err := index.Load(path)
	if err != nil {
		fmt.Printf("加载失败: %v\n", err)
		os.Exit(1)
	}
	h, err := history.Open(filepath.Join(dir, "history.json"), 200, 0)
	if err != nil {
		fmt.Printf("历史读不出来: %v\n", err)
		os.Exit(1)
	}
	defer h.Close()

	// 和 config 默认值一致；改过配置的话这里不会反映出来。
	weights := search.Weights{Frequency: 0.5, Type: 0.3, Accuracy: 0.2}
	order := []string{".exe", ".lnk", ".msc", ".bat", ".cmd", ".ps1", ".url", ".appref-ms", ".com", ".jar"}

	tokens := strings.Fields(strings.ToLower(*q))
	res := make([]search.Result, 0, 4096)
	for i := 0; i < tab.Len(); i++ {
		if !tab.Hits(i, tokens) && !pyContains(tab.PyOf(i), tokens) {
			continue
		}
		if tab.Dead(int32(i)) {
			continue
		}
		name := tab.Name(i)
		acc, ok := search.AccuracyPy(*q, name, "", tab.PyOf(i), "")
		if !ok {
			continue
		}
		path := tab.PathOf(int32(i))
		freq, opens := h.ScoreCount(path)
		res = append(res, search.Rank(weights, order, search.Result{
			Item: search.Item{
				Name: name,
				Path: path,
				Ext:  tab.Ext(i),
				Kind: kindOf(tab.KindOf(int32(i))),
				Hint: tab.Dir(int32(i)),
			},
			Frequency: freq,
			Opens:     opens,
			Accuracy:  acc,
		}))
		if len(res) >= 5000 {
			break
		}
	}
	search.Sort(res)
	fmt.Printf("索引 %d 条，查询 %q 命中 %d 条，加载+扫描 %v\n\n",
		tab.Len(), *q, len(res), time.Since(t0).Round(time.Millisecond))
	fmt.Printf("%-4s %-4s %-6s %-5s %-6s %-6s %s\n", "#", "段", "总分", "开过", "准确", "噪声", "名字 / 路径")
	lim := *n
	if lim > len(res) {
		lim = len(res)
	}
	for i := 0; i < lim; i++ {
		r := res[i]
		noisy := ""
		if r.Noisy {
			noisy = "Y"
		}
		fmt.Printf("%-4d %-5d %-6.3f %-6d %-6.2f %-6s %s\n      %s\n",
			i+1, r.Band, r.Score, r.Opens, r.Accuracy, noisy, shorten(r.Name, 46), shorten(r.Path, 90))
	}
	// 段分布：一眼看出"前 80 条是不是全是应用"。
	count := map[search.Band]int{}
	for _, r := range res {
		count[r.Band]++
	}
	var bands []int
	for b := range count {
		bands = append(bands, int(b))
	}
	sort.Ints(bands)
	fmt.Print("\n段分布：")
	for _, b := range bands {
		fmt.Printf(" %s=%d", bandName(search.Band(b)), count[search.Band(b)])
	}
	fmt.Printf("\n排序后前 %d 条里：应用 %d / 其余 %d\n", 80, countAt(res, 80, search.BandApp), 80-countAt(res, 80, search.BandApp))
}

func countAt(res []search.Result, n int, b search.Band) int {
	c := 0
	for i := 0; i < n && i < len(res); i++ {
		if res[i].Band == b {
			c++
		}
	}
	return c
}

func bandName(b search.Band) string {
	switch b {
	case search.BandApp:
		return "应用"
	case search.BandCommand:
		return "命令"
	case search.BandStrong:
		return "强"
	case search.BandMedium:
		return "中"
	}
	return "弱"
}

func shorten(s string, max int) string {
	if len([]rune(s)) <= max {
		return s
	}
	return string([]rune(s)[:max]) + "…"
}

func kindOf(k index.Kind) search.Kind {
	if k == index.KindDir {
		return search.KindDir
	}
	return search.KindFile
}

// pyContains 是主程序里的拼音预筛，这里照抄一份保持同一条路径。
func pyContains(py string, tokens []string) bool {
	if py == "" {
		return false
	}
	for _, t := range tokens {
		if !isAlphaNum(t) || !strings.Contains(py, t) {
			return false
		}
	}
	return true
}

func isAlphaNum(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}
