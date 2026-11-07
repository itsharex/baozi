package index

import (
	"context"
	"errors"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"baozi/internal/config"
	"baozi/internal/pinyin"
)

// ErrStop 用于内部提前结束遍历。
var ErrStop = errors.New("stop")

// Build 遍历配置里的根目录，产出一张快照表。
// 这是"无服务"路径：不需要管理员权限，但全盘首次扫描比读 $MFT 慢。
// 后续接入 USN Journal 后，这里会换成增量源，接口不变。
func Build(ctx context.Context, cfg config.Index, progress func(scanned int)) (*Table, error) {
	// 名字 arena 一开始给 32MB（C+D 盘实际约 41MB），避免十来次翻倍拷贝。
	t := &Table{
		Nodes: make([]Node, 0, 1<<21),
		Names: make([]byte, 0, 1<<25),
		Py:    make(map[int32]uint64, 1<<13),
	}
	dirIDs := make(map[string]int32, 1<<16)

	roots := dedupeRoots(cfg.Roots)
	lastReport := time.Now()

	// addName 把名字追加进 arena，返回偏移与长度。
	addName := func(s string) (uint32, uint16) {
		off := uint32(len(t.Names))
		t.Names = append(t.Names, s...)
		return off, uint16(len(s))
	}

	for _, root := range roots {
		if ctx.Err() != nil {
			return t, ctx.Err()
		}
		st, statErr := os.Stat(root)
		if statErr != nil || !st.IsDir() {
			continue
		}
		root = filepath.Clean(root)
		rootID := int32(len(t.Nodes))
		off, nlen := addName(root)
		t.Nodes = append(t.Nodes, Node{Parent: -1, NameOff: off, NameLen: nlen, Kind: KindDir})
		dirIDs[root] = rootID

		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil // 权限不足的单个条目不该中断整盘
			}
			if p == root {
				return nil
			}
			if ctx.Err() != nil {
				return ErrStop
			}
			name := d.Name()
			if excluded(cfg.Excludes, name, p) {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			// 符号链接/目录联接不跟随，避免环路和重复索引。
			if d.Type()&fs.ModeSymlink != 0 {
				return nil
			}
			info, infoErr := d.Info()
			if infoErr != nil {
				return nil
			}
			// 隐藏文件默认不索引（desktop.ini、.env 这类没人想在启动器里点）。
			// 隐藏**目录**照常进去：目录的 Hidden 属性经常被"备份/整理"工具或用户
			// 随手设上，整棵跳过会让里面所有正常文件都搜不到——真实案例是
			// D:\Backup 带 Hidden，导致备份里的 SnowShot 视频一条都没进索引。
			if !cfg.IncludeHidden && !d.IsDir() && isHidden(info) {
				return nil
			}
			parent, ok := dirIDs[filepath.Dir(p)]
			if !ok {
				return nil
			}
			kind := KindFile
			if d.IsDir() {
				kind = KindDir
			}
			sizeKB := int32(0)
			if !d.IsDir() {
				if kb := info.Size() >> 10; kb < math.MaxInt32 {
					sizeKB = int32(kb)
				} else {
					sizeKB = math.MaxInt32
				}
			}
			modSec := info.ModTime().Unix() - ModBase
			switch {
			case modSec < 0:
				modSec = 0
			case modSec > math.MaxInt32:
				modSec = math.MaxInt32
			}

			id := int32(len(t.Nodes))
			off, nlen := addName(name)
			t.Nodes = append(t.Nodes, Node{
				Parent:  parent,
				NameOff: off,
				NameLen: nlen,
				SizeKB:  sizeKB,
				ModSec:  int32(modSec),
				Kind:    kind,
			})
			if d.IsDir() {
				dirIDs[p] = id
			}
			// 纯 ASCII 名字的拼音串是空串，不建表项（179 万条里只有 9 千条约需拼音）。
			if py := pinyin.Initials(name); py != "" {
				pyOff, pyLen := addName(py)
				t.Py[id] = PackPy(pyOff, pyLen)
			}
			if progress != nil && time.Since(lastReport) > time.Second {
				lastReport = time.Now()
				progress(len(t.Nodes))
			}
			return nil
		})
		if err != nil && !errors.Is(err, ErrStop) {
			return t, err
		}
		if errors.Is(err, ErrStop) {
			return t, ctx.Err()
		}
	}
	if progress != nil {
		progress(len(t.Nodes))
	}
	return t, nil
}

// dedupeRoots 去掉重复与嵌套的根目录，避免同一批文件被索引两遍。
func dedupeRoots(roots []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range roots {
		if r == "" {
			continue
		}
		clean := strings.ToLower(filepath.Clean(r))
		if seen[clean] {
			continue
		}
		seen[clean] = true
		out = append(out, filepath.Clean(r))
	}
	// 去掉被其他根目录包含的项
	var kept []string
	for _, r := range out {
		nested := false
		for _, other := range out {
			if other == r {
				continue
			}
			lowR := strings.ToLower(r)
			lowO := strings.ToLower(other)
			if strings.HasPrefix(lowR, strings.TrimRight(lowO, `\/`)+`\`) {
				nested = true
				break
			}
		}
		if !nested {
			kept = append(kept, r)
		}
	}
	return kept
}

// excluded 判断路径是否命中排除规则。
// 规则里不带路径分隔符时按目录名/文件名匹配（支持 * 通配）；
// 带分隔符时按路径片段匹配。
func excluded(rules []string, name, path string) bool {
	return ExcludedNameOnly(rules, name) || ExcludedPathOnly(rules, path)
}

// ExcludedNameOnly 只跑"按名字匹配"的那部分规则（不带分隔符的规则，支持通配）。
// 给"还没有完整路径"的数据源（USN 只有名字）做便宜的第一道过滤。
func ExcludedNameOnly(rules []string, name string) bool {
	lowName := strings.ToLower(name)
	for _, rule := range rules {
		rule = strings.TrimSpace(rule)
		if rule == "" || strings.ContainsAny(rule, `\/`) {
			continue
		}
		if ok, _ := filepath.Match(strings.ToLower(rule), lowName); ok {
			return true
		}
	}
	return false
}

// ExcludedPathOnly 只跑"按路径片段匹配"的那部分规则（带分隔符的规则）。
func ExcludedPathOnly(rules []string, path string) bool {
	lowPath := strings.ToLower(path)
	for _, rule := range rules {
		rule = strings.TrimSpace(rule)
		if rule == "" || !strings.ContainsAny(rule, `\/`) {
			continue
		}
		if strings.Contains(lowPath, strings.ReplaceAll(strings.ToLower(rule), "/", `\`)) {
			return true
		}
	}
	return false
}

func isHidden(info fs.FileInfo) bool {
	if info == nil {
		return false
	}
	data, ok := info.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return false
	}
	return data.FileAttributes&syscall.FILE_ATTRIBUTE_HIDDEN != 0
}
