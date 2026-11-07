//go:build ignore

// memprobe 是内存诊断小工具：加载 index.gob，量出"索引本身"到底占多少。
// 只读不写，改完索引结构后用它对比前后数字。
//
//	go run tools\memprobe\main.go [index.gob 路径]
package main

import (
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"unsafe"

	"baozi/internal/index"
)

func mb(v uint64) float64 { return float64(v) / (1 << 20) }

func dump(tag string) *runtime.MemStats {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	fmt.Printf("%-14s heapAlloc=%7.1fMB heapInuse=%7.1fMB heapSys=%7.1fMB sys=%7.1fMB objs=%d gc=%d\n",
		tag, mb(m.HeapAlloc), mb(m.HeapInuse), mb(m.HeapSys), mb(m.Sys), m.HeapObjects, m.NumGC)
	return &m
}

func main() {
	path := `C:\Users\Administrator\AppData\Local\Baozi\index.gob`
	if len(os.Args) > 1 {
		path = os.Args[1]
	}

	fmt.Printf("Node 结构大小: %d 字节\n", unsafe.Sizeof(index.Node{}))
	dump("启动后")

	nodes, roots, err := index.Load(path)
	if err != nil {
		fmt.Println("加载失败:", err)
		os.Exit(1)
	}
	fmt.Printf("条目数: %d  根目录: %v\n", len(nodes), roots)
	dump("加载后")

	var nameBytes, pyBytes int64
	pyCount, cnCount := 0, 0
	for i := range nodes {
		nameBytes += int64(len(nodes[i].Name))
		if len(nodes[i].Name) > 0 && nodes[i].Name[0] > 0x7f {
			cnCount++
		}
		if nodes[i].Py != "" {
			pyCount++
			pyBytes += int64(len(nodes[i].Py))
		}
	}
	fmt.Printf("结构本体: %.1fMB   名字内容: %.1fMB（%d 条，平均 %.1f 字节）\n",
		float64(len(nodes))*float64(unsafe.Sizeof(index.Node{}))/(1<<20), mb(uint64(nameBytes)),
		len(nodes), float64(nameBytes)/float64(len(nodes)))
	fmt.Printf("拼音串: %d 条 / %.1fMB（占 %.1f%%）  非 ASCII 首字符条目: %d\n",
		pyCount, mb(uint64(pyBytes)), 100*float64(pyCount)/float64(len(nodes)), cnCount)

	// 模拟 catalog：main 里给每条名字预先小写一份（搜索预筛用），看它到底占多少。
	lowers := make([]string, len(nodes))
	for i := range nodes {
		lowers[i] = strings.ToLower(nodes[i].Name)
	}
	dump("加 lowers 后")

	runtime.GC()
	dump("GC 后")
	debug.FreeOSMemory()
	dump("FreeOSMemory")
	// 保活，防止被优化掉
	if len(nodes) == 0 || roots == nil || len(lowers) == 0 {
		os.Exit(2)
	}
}
