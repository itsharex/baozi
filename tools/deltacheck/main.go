// deltacheck 把服务写的增量叠到 index.bin 上，检查结果对不对。
//
// 主进程做的是同一件事（copy-on-write + ApplyOps），但它藏在 GUI 里，
// 出问题时不好判断是"索引没更新"还是"搜索没显示"。这个探针只看索引层：
// 叠完增量之后，某个名字在不在、路径是什么、有没有被判成墓碑。
//
// 用法（管理员不必，普通权限即可）：
//
//	go run ./tools/deltacheck -find 某个文件名
//	go run ./tools/deltacheck -data-dir "C:\Users\x\AppData\Local\Baozi" -find 存量
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"baozi/internal/index"
)

func main() {
	dataDir := flag.String("data-dir", "", "数据目录（默认 %LOCALAPPDATA%\\Baozi）")
	find := flag.String("find", "", "按名字子串查找（大小写不敏感）")
	limit := flag.Int("limit", 20, "最多打印多少条命中")
	flag.Parse()

	if *dataDir == "" {
		*dataDir = filepath.Join(os.Getenv("LOCALAPPDATA"), "Baozi")
	}
	binPath := filepath.Join(*dataDir, "index.bin")

	tab, roots, err := index.Load(binPath)
	if err != nil {
		fmt.Printf("读 index.bin: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("index.bin: %d 项，根 %v\n", tab.Len(), roots)

	d, err := index.ReadDelta(filepath.Join(*dataDir, "index.delta"))
	if err != nil {
		fmt.Printf("读 index.delta: %v\n", err)
	}
	if d != nil {
		fmt.Printf("index.delta: base=%d seq=%d ops=%d\n", d.Base, d.Seq, len(d.Ops))
		if len(d.Ops) > 0 {
			next := tab.Clone()
			dead, ok := next.ApplyOps(d.Ops)
			if !ok {
				fmt.Printf("  应用失败：下标错位（要么这代 bin 换了，要么 ops 里有不认识的指令）\n")
			} else {
				fmt.Printf("  应用成功：新增墓碑 %d，叠完 %d 项（原 %d）\n", dead, next.Len(), tab.Len())
				tab = next
			}
		}
	} else {
		fmt.Printf("index.delta: 不存在（这一代还没有增量）\n")
	}

	if *find == "" {
		return
	}
	low := strings.ToLower(*find)
	hits, tombstones := 0, 0
	for i := 0; i < tab.Len(); i++ {
		if !strings.Contains(strings.ToLower(tab.Name(i)), low) {
			continue
		}
		if tab.Dead(int32(i)) {
			tombstones++
			continue
		}
		if hits < *limit {
			fmt.Printf("  %s\n", tab.PathOf(int32(i)))
		}
		hits++
	}
	fmt.Printf("命中 %d 条（另有 %d 条是墓碑/父已删）\n", hits, tombstones)
}
