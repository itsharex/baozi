// frnreplay 用服务端那套结构（映射索引 + 映射 FRN 基座 + 增量覆盖层）回放一段
// USN 记录，把每条记录为什么被收录/被拒逐条打出来。
//
// 用途只有一个：增量"读到了却一条都没用上"的时候，定位是哪一道判断挡住的。
//
//	go run ./tools/frnreplay -bin <index-xxx.bin> -frn <index-xxx.frn> -vol C -from <usn> -limit 30
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"

	"baozi/internal/config"
	"baozi/internal/index"
	"baozi/internal/usn"
)

type state struct {
	Volumes []struct {
		Letter    string `json:"letter"`
		JournalID uint64 `json:"journalId"`
		NextUsn   int64  `json:"nextUsn"`
	} `json:"volumes"`
}

func main() {
	binPath := flag.String("bin", "", "索引文件（index-xxx.bin）")
	frnPath := flag.String("frn", "", "FRN 旁挂表（index-xxx.frn）")
	vol := flag.String("vol", "C", "卷盘符")
	from := flag.Int64("from", 0, "起始 USN（0 表示用 -state 里的位点）")
	jid := flag.Uint64("jid", 0, "日记 ID（0 表示用 -state 里的）")
	statePath := flag.String("state", "", "index.progress.json（用来取位点）")
	limit := flag.Int("limit", 30, "最多打印几条")
	dataDir := flag.String("data", "", "数据目录（会加入排除规则）")
	flag.Parse()

	var st state
	if *statePath != "" {
		b, err := os.ReadFile(*statePath)
		if err != nil {
			log.Fatalf("读位点: %v", err)
		}
		if err := json.Unmarshal(b, &st); err != nil {
			log.Fatalf("解析位点: %v", err)
		}
		for _, v := range st.Volumes {
			if v.Letter == *vol {
				if *from == 0 {
					*from = v.NextUsn
				}
				if *jid == 0 {
					*jid = v.JournalID
				}
			}
		}
	}
	fmt.Printf("位点 usn=%d jid=%d\n", *from, *jid)

	mapped, _, err := index.Load(*binPath)
	if err != nil {
		log.Fatalf("载入索引: %v", err)
	}
	ff, err := index.LoadFRNFile(*frnPath)
	if err != nil {
		log.Fatalf("映射 FRN 表: %v", err)
	}
	defer ff.Close()
	mut := index.NewMutableBase(mapped, ff)
	fmt.Printf("表 %d 项，基座 FRN %d 条\n", mapped.Len(), mut.FRNBaseLen())

	ex := []string{}
	if *dataDir != "" {
		ex = append(ex, *dataDir)
	}
	_ = config.Index{} // 只是让 import 有效，规则本身不需要配置里的其它字段
	rule := index.Rule{Excludes: ex, IncludeHidden: false}

	v, err := usn.Open(*vol)
	if err != nil {
		log.Fatalf("打开卷 %s: %v", *vol, err)
	}
	defer v.Close()

	n := 0
	_, _, err = v.Read(*from, *jid, 0, func(r *usn.Record) bool {
		if n >= *limit {
			return false
		}
		n++
		c := r.Change(*vol)
		parentIdx, parentOK := mut.LookupFRN(c.ParentFRN)
		selfIdx, selfOK := mut.LookupFRN(c.FRN)
		op, ok := mut.Apply(c, rule)
		fmt.Printf("%2d %-34q frn=%016x%s parent=%016x%s create=%v del=%v renOld=%v renNew=%v dir=%v reparse=%v hidden=%v → 应用=%v op=%s\n",
			n, c.Name,
			c.FRN, hit(selfOK), c.ParentFRN, hit(parentOK),
			c.Create, c.Delete, c.RenameOld, c.RenameNew, c.IsDir, c.IsReparse, c.IsHidden,
			ok, op.Op)
		if parentOK {
			fmt.Printf("     父下标 %d，父种类 %v\n", parentIdx, mut.Table().KindOf(parentIdx))
		}
		if selfOK {
			fmt.Printf("     自身下标 %d\n", selfIdx)
		}
		if c.Name == "" {
			fmt.Print("     （名字为空，非建/改名记录）\n")
		}
		return true
	})
	if err != nil {
		log.Fatalf("读增量: %v", err)
	}
	fmt.Printf("共 %d 条\n", n)
}

func hit(ok bool) string {
	if ok {
		return "(命中)"
	}
	return "(查不到)"
}
