// frnprobe 是诊断小工具：看某个文件号在 FRN 旁挂表里到底能不能找到。
//
//	go run ./tools/frnprobe -file <index-xxx.frn> -key 0x00020000000b083a
//
// 不带 -key 时打印表头几条记录的键，用来确认"键里到底有没有盘符标记"。
package main

import (
	"flag"
	"fmt"
	"log"

	"baozi/internal/index"
)

func main() {
	path := flag.String("file", "", "FRN 旁挂表路径（index-xxx.frn）")
	keyHex := flag.String("key", "", "要查的文件号，十六进制")
	flag.Parse()
	if *path == "" {
		log.Fatal("缺 -file")
	}
	ff, err := index.LoadFRNFile(*path)
	if err != nil {
		log.Fatalf("映射失败: %v", err)
	}
	defer ff.Close()
	fmt.Printf("表 %s：%d 条\n", *path, ff.Len())
	n := ff.Len()
	if n > 6 {
		n = 6
	}
	for i := 0; i < n; i++ {
		fmt.Printf("  #%d 键=0x%016x 下标=%d\n", i, ff.KeyAt(i), ff.ValAt(i))
	}
	fmt.Printf("  末尾 #%d 键=0x%016x 下标=%d\n", ff.Len()-1, ff.KeyAt(ff.Len()-1), ff.ValAt(ff.Len()-1))
	if *keyHex != "" {
		var key uint64
		if _, err := fmt.Sscanf(*keyHex, "%v", &key); err != nil {
			// Sscanf 不认 0x 前缀以外的格式差异，这里自己按 16 进制兜底
		}
		key = parseHex(*keyHex)
		idx, ok := ff.Get(key)
		fmt.Printf("查 0x%016x → 下标 %d，命中=%v\n", key, idx, ok)
	}
}

func parseHex(s string) uint64 {
	var v uint64
	start := 0
	if len(s) > 2 && (s[0:2] == "0x" || s[0:2] == "0X") {
		start = 2
	}
	for i := start; i < len(s); i++ {
		c := s[i]
		var d uint64
		switch {
		case c >= '0' && c <= '9':
			d = uint64(c - '0')
		case c >= 'a' && c <= 'f':
			d = uint64(c-'a') + 10
		case c >= 'A' && c <= 'F':
			d = uint64(c-'A') + 10
		default:
			return v
		}
		v = v<<4 | d
	}
	return v
}
