//go:build ignore

// usnprobe 是 USN Journal 的联调工具：打开卷、查 journal、全量枚举并统计。
// 用法（需管理员）：go run tools/usnprobe/main.go [盘符] [打印条数]
package main

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"baozi/internal/usn"
)

func main() {
	drive := "C"
	if len(os.Args) > 1 {
		drive = os.Args[1]
	}
	show := 8
	if len(os.Args) > 2 {
		show, _ = strconv.Atoi(os.Args[2])
	}

	vol, err := usn.Open(drive)
	if err != nil {
		fmt.Printf("打开失败: %v\n", err)
		os.Exit(1)
	}
	defer vol.Close()

	j, err := vol.Journal()
	if err != nil {
		fmt.Printf("journal 读不到: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("卷 %s: journalID=%#x first=%d next=%d\n", drive, j.ID, j.FirstUsn, j.NextUsn)

	start := time.Now()
	files, dirs, others := 0, 0, 0
	hidden := 0
	var first []string
	err = vol.Enum(func(r *usn.Record) bool {
		switch {
		case r.IsDir():
			dirs++
		case r.Attrs&usn.AttrHidden != 0:
			hidden++
			files++
		default:
			files++
		}
		if r.Attrs&usn.AttrReparsePoint != 0 {
			others++
		}
		if len(first) < show {
			kind := "文件"
			if r.IsDir() {
				kind = "目录"
			}
			first = append(first, fmt.Sprintf("  [%s] frn=%d parent=%d %s", kind, r.FRN, r.ParentFRN, r.Name))
		}
		return true
	})
	if err != nil {
		fmt.Printf("枚举出错: %v\n", err)
	}
	fmt.Println("前几条:")
	for _, s := range first {
		fmt.Println(s)
	}
	fmt.Printf("统计: 文件 %d 目录 %d 隐藏 %d 重解析点 %d 耗时 %v\n",
		files, dirs, hidden, others, time.Since(start).Round(time.Millisecond))

	// 增量：立刻读一次（wait=0），换个文件看看会不会报错
	next, n, err := vol.Read(j.NextUsn, j.ID, 0, func(r *usn.Record) bool { return true })
	fmt.Printf("增量读: next=%d 条数=%d err=%v\n", next, n, err)
}
