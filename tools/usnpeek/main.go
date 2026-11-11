// usnpeek 看一眼索引服务保存的增量位点和卷上 Journal 当前位置差多远，
// 再试着读一次增量，确认"到底有多少记录没被读到"。
// 只在排查时手动跑（需要管理员/服务权限打开卷）。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"baozi/internal/config"
	"baozi/internal/usn"
)

func main() {
	dir, err := config.Dir()
	if err != nil {
		fmt.Println("数据目录:", err)
		return
	}
	path := filepath.Join(dir, "index.progress.json")
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Println("读位点文件:", err)
		return
	}
	var s struct {
		Volumes []usn.Progress `json:"volumes"`
		Total   int            `json:"total"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		fmt.Println("解析位点:", err)
		return
	}
	fmt.Printf("位点文件 %s (总 %d 项)\n", path, s.Total)
	for _, p := range s.Volumes {
		fmt.Printf("  %s: nextUsn=%d journalID=%d 建自 %s\n", p.Letter, p.NextUsn, p.JournalID, p.BuiltAt.Format("15:04:05"))
	}

	for _, p := range s.Volumes {
		v, err := usn.Open(p.Letter)
		if err != nil {
			fmt.Printf("%s 打开卷失败: %v\n", p.Letter, err)
			continue
		}
		j, err := v.Journal()
		if err != nil {
			fmt.Printf("%s 查 journal 失败: %v\n", p.Letter, err)
			v.Close()
			continue
		}
		fmt.Printf("%s: journal next=%d first=%d，位点差 %d 字节\n",
			p.Letter, j.NextUsn, j.FirstUsn, j.NextUsn-p.NextUsn)

		// 单次读（不等），看一次能拿到多少条、位点推到哪，并确认它立刻就回
		t1 := time.Now()
		next, n, err := v.Read(p.NextUsn, j.ID, 0, func(r *usn.Record) bool { return true })
		fmt.Printf("  单次读: 耗时 %v next=%d count=%d err=%v（剩余 %d 字节未读）\n",
			time.Since(t1).Round(time.Microsecond), next, n, err, j.NextUsn-next)

		// 连续读几轮，看累积后能否追平
		start := p.NextUsn
		total := 0
		t0 := time.Now()
		for i := 0; i < 20; i++ {
			next, n, err = v.Read(start, j.ID, 0, func(r *usn.Record) bool { return true })
			if err != nil || n == 0 || next <= start {
				break
			}
			start = next
			total += n
		}
		fmt.Printf("  连读 20 轮: 共 %d 条，位点 %d（剩 %d 字节），耗时 %v\n", total, start, j.NextUsn-start, time.Since(t0).Round(time.Millisecond))
		v.Close()
	}
}
