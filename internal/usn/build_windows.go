//go:build windows

package usn

import (
	"log"
	"os"
	"sort"
	"strings"
	"time"

	"baozi/internal/config"
	"baozi/internal/index"
	"baozi/internal/pinyin"
)

// idxProfile 由 BAOZI_IDXPROFILE=1 打开：每条卷的枚举/排序/建表分段打一行耗时。
// 平时关着——重建本身就在跟 CPU 计较，不该再为打点付钱。
var idxProfile = os.Getenv("BAOZI_IDXPROFILE") != ""

// 卷上的 NTFS 元数据文件（$MFT、$RmMetadata、$Txf…）不是用户能点的东西，
// 而且它们的父链指向卷根之外，留着只会污染结果。
const metaPrefix = "$"

// USN 记录里的 FileReferenceNumber 是 64 位：高 16 位是序列号，低 48 位才是
// $MFT 里的记录号。路径还原只认记录号，比较前必须先掩码——
// 否则父节点写的是 "序列号|5"，而根目录常量是 5，永远对不上（实测就卡在这）。
const frnMask uint64 = 0x0000FFFFFFFFFFFF

// rec 是枚举过程中留在内存里的最小记录。
type rec struct {
	frn    uint64
	parent uint64
	attrs  uint32
	name   string
}

// Progress 是某个卷的增量位点。服务每轮重建后把它写进伴随文件，
// 主进程据此判断"索引归服务管、不是我自己扫的"。
type Progress struct {
	Letter    string    `json:"letter"`
	JournalID uint64    `json:"journalId"`
	NextUsn   int64     `json:"nextUsn"`
	Count     int       `json:"count"`
	BuiltAt   time.Time `json:"builtAt"`
}

// BuildAll 枚举多个卷，合成一张索引快照，同时给出 FRN → 条目下标 的映射。
// 映射是给"之后按 USN 增量维护这张表"用的：没有它，一条记录过来不知道要改哪个条目。
// 全量枚举本来就握着每条记录的 FRN，顺手建起来不比单独扫一遍贵。
func BuildAll(vols []*Volume, cfg config.Index, progress func(scanned int)) (*index.Table, *index.FRNMap, []Progress, error) {
	hint := 0
	for range vols {
		hint += 1 << 20
	}
	b := index.NewBuilder(hint)

	// 规则里有没有"路径片段"那类规则，决定 DFS 时要不要为每条拼一次 string。
	hasPathRules := false
	for _, rule := range cfg.Excludes {
		if strings.ContainsAny(strings.TrimSpace(rule), `\/`) {
			hasPathRules = true
			break
		}
	}

	rootPrefixes := make([]string, 0, len(cfg.Roots))
	for _, r := range cfg.Roots {
		if r == "" {
			continue
		}
		rootPrefixes = append(rootPrefixes, strings.ToLower(strings.TrimRight(r, `\/`)))
	}

	var out []Progress
	for _, vol := range vols {
		p, err := buildOne(b, vol, cfg, rootPrefixes, hasPathRules, progress)
		if err != nil {
			return nil, nil, out, err
		}
		out = append(out, p)
	}
	if progress != nil {
		progress(b.Len())
	}
	return b.Table(), b.FRNs(), out, nil
}

func buildOne(b *index.Builder, vol *Volume, cfg config.Index, rootPrefixes []string, hasPathRules bool, progress func(int)) (Progress, error) {
	// 1) 全量枚举，先收进扁平数组（每卷 100 万条起步）。
	tEnum := time.Now()
	recs := make([]rec, 0, 1<<20)
	j, err := vol.Journal()
	if err != nil {
		return Progress{}, err
	}
	err = vol.Enum(func(r *Record) bool {
		frn := r.FRN & frnMask
		if frn == RootFRN {
			return true // 根目录自己加
		}
		if strings.HasPrefix(r.Name, metaPrefix) {
			return true
		}
		recs = append(recs, rec{frn: frn, parent: r.ParentFRN & frnMask, attrs: r.Attrs, name: r.Name})
		return true
	})
	if err != nil {
		return Progress{}, err
	}
	if idxProfile {
		log.Printf("建索引分段 %s: 枚举 %v，%d 条", vol.Letter(), time.Since(tEnum).Round(time.Millisecond), len(recs))
	}

	// 2) 按父编号排序，DFS 时用二分找子集——比给每个目录建一个 map 省得多
	//    （147 万目录各带一个 slice header 就要 35 MB）。
	tSort := time.Now()
	sort.Slice(recs, func(i, j int) bool {
		if recs[i].parent != recs[j].parent {
			return recs[i].parent < recs[j].parent
		}
		return recs[i].frn < recs[j].frn
	})
	if idxProfile {
		log.Printf("建索引分段 %s: 排序 %v", vol.Letter(), time.Since(tSort).Round(time.Millisecond))
	}
	tWalk := time.Now()

	// 3) 卷根节点
	rootName := vol.Letter() + `:\`
	rootID := b.AddFRN(rootName, -1, index.KindDir, 0, 0, volKey(vol.Letter(), RootFRN))
	if py := pinyin.Initials(rootName); py != "" {
		b.SetPy(rootID, py)
	}
	rootPath := []byte(vol.Letter() + `:`)

	// 配置根就是这个卷的整盘（"C:\"），DFS 已从卷根出发，没必要每条再判一次。
	wholeVolume := false
	for _, r := range rootPrefixes {
		if len(r) == 2 && r[1] == ':' && strings.EqualFold(r[:1], vol.Letter()) {
			wholeVolume = true
			break
		}
	}

	// 4) DFS：从卷根往下走，边走边累积路径。
	//    只有"父节点已经在索引里"的条目才可能被收录——父目录被排除/删掉的子树整棵不可达，
	//    这和 WalkDir 的行为一致。
	//
	//    路径用一条共享缓冲按层 append/回退，不再每条 make 一个切片：
	//    174 万条每次都分配一次，光这一项就够 DFS 建表慢一截。
	pathBuf := append([]byte(nil), rootPath...)
	var walk func(parentFRN uint64, parentID int32)
	walk = func(parentFRN uint64, parentID int32) {
		lo := sort.Search(len(recs), func(i int) bool { return recs[i].parent >= parentFRN })
		for i := lo; i < len(recs) && recs[i].parent == parentFRN; i++ {
			r := &recs[i]
			isDir := r.attrs&AttrDirectory != 0

			// NTFS 的 reparse point（符号链接、目录联接）不跟随，避免环路。
			if r.attrs&AttrReparsePoint != 0 {
				continue
			}
			// 隐藏文件默认不收；隐藏**目录**照常进（备份盘经常整棵带 Hidden）。
			if !isDir && !cfg.IncludeHidden && r.attrs&AttrHidden != 0 {
				continue
			}
			if index.ExcludedNameOnly(cfg.Excludes, r.name) {
				continue
			}

			base := len(pathBuf)
			pathBuf = append(pathBuf, '\\')
			pathBuf = append(pathBuf, r.name...)
			if hasPathRules && index.ExcludedPathOnly(cfg.Excludes, string(pathBuf)) {
				pathBuf = pathBuf[:base]
				continue
			}
			// 只收起在配置根目录之下的条目（roots 通常是整盘，配子目录时这里起作用）。
			// 整盘根时从卷根出发的每条都必然在根下，跳过逐条转小写+前缀比较。
			if !wholeVolume && !underAny(pathBuf, rootPrefixes) {
				pathBuf = pathBuf[:base]
				continue
			}

			kind := index.KindFile
			if isDir {
				kind = index.KindDir
			}
			id := b.AddFRN(r.name, parentID, kind, 0, 0, volKey(vol.Letter(), r.frn))
			if py := pinyin.Initials(r.name); py != "" {
				b.SetPy(id, py)
			}
			if isDir {
				walk(r.frn, id)
			}
			pathBuf = pathBuf[:base]
			if progress != nil && b.Len()%20000 == 0 {
				progress(b.Len())
			}
		}
	}
	walk(RootFRN, rootID)
	if idxProfile {
		log.Printf("建索引分段 %s: DFS 建表 %v，%d 项", vol.Letter(), time.Since(tWalk).Round(time.Millisecond), b.Len())
	}

	return Progress{
		Letter:    vol.Letter(),
		JournalID: j.ID,
		NextUsn:   j.NextUsn,
		Count:     len(recs),
		BuiltAt:   time.Now(),
	}, nil
}

// underAny 判断路径是否落在某个根目录之下（大小写不敏感）。
func underAny(path []byte, roots []string) bool {
	if len(roots) == 0 {
		return true
	}
	low := make([]byte, len(path))
	for i := 0; i < len(path); i++ {
		c := path[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		low[i] = c
	}
	s := string(low)
	for _, r := range roots {
		if r == "" {
			continue
		}
		if strings.HasPrefix(s, r) && (len(s) == len(r) || s[len(r)] == '\\') {
			return true
		}
	}
	return false
}
