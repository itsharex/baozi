// Package index 是内存索引：每个条目只存"名字在 arena 里的位置 + 父节点编号"，
// 不存完整路径——这正是 NTFS $MFT 的存法，也是 Lertaro 索引层的思路。
//
// 为什么名字要放进 arena 而不是每条一个 string：179 万条的名字如果各自分配
// （string header 16 字节 + 分配器按 size class 向上取整 + 每个对象的管理开销），
// 40 MB 的实际内容会占到 ~90 MB，还附带上百万个对象让 GC 每次都要扫。
// 现在名字是 Names 里连续的一段字节，Node 只有定长 20 字节。
// 定长的好处不止省内存：整个 nodes 段可以 unsafe.Slice 成一个结构数组，
// 加载时零拷贝、将来换成 mmap 也不用解码。
package index

import (
	"strings"
	"sync"
	"time"
)

// Kind 是条目类型。
type Kind uint8

const (
	KindFile Kind = iota
	KindDir
)

// ModBase 是 ModSec 的基准时间（2020-01-01T00:00:00Z）。
// ModSec 用 int32 存秒数，能表示到 2088 年，够用；比 int64 每条省 4 字节。
const ModBase int64 = 1577836800

// Node 是索引中的一个条目，定长 20 字节。
type Node struct {
	Parent  int32  // 根节点为 -1
	NameOff uint32 // 名字在 Table.Names 里的起始偏移
	SizeKB  int32  // 文件大小（KB），目录为 0
	ModSec  int32  // 相对 ModBase 的秒数
	NameLen uint16 // 名字字节数（NTFS 单个文件名上限 255）
	Kind    Kind
	pad     uint8 // 显式占位，把结构固定成 20 字节
}

// NodeSize 是 Node 的字节宽度，store 与测试共用。
const NodeSize = 20

// Table 是一次完整的索引快照。
// Nodes 与 Names 可能指向 raw（从磁盘整块读进/映射进来的缓冲），
// 所以 raw 必须跟着一起持有，否则切片会悬垂。
type Table struct {
	Nodes []Node
	Names []byte
	// Py 只对名字含汉字的条目存在（179 万条约 9 千条），
	// 值把"在 Names 里的偏移（高 32 位）+ 长度（低 16 位）"打包成一个 uint64，
	// 这样既不用给每条 Node 挂一个 16 字节的 string 头，读回来也不用二次查找。
	Py  map[int32]uint64
	raw []byte
}

// PackPy 把拼音串在 arena 里的位置打包。
func PackPy(off uint32, n uint16) uint64 { return uint64(off)<<16 | uint64(n) }

// UnpackPy 拆开 PackPy 的打包值。
func UnpackPy(v uint64) (off uint32, n uint16) { return uint32(v >> 16), uint16(v & 0xffff) }

// Len 返回条目数。
func (t *Table) Len() int {
	if t == nil {
		return 0
	}
	return len(t.Nodes)
}

// NameBytes 返回名字的字节切片（零拷贝，不要修改）。
func (t *Table) NameBytes(i int) []byte {
	if t == nil || i < 0 || i >= len(t.Nodes) {
		return nil
	}
	n := &t.Nodes[i]
	end := int64(n.NameOff) + int64(n.NameLen)
	if end > int64(len(t.Names)) {
		return nil
	}
	return t.Names[n.NameOff:end]
}

// Name 返回名字字符串（会分配一次，只在渲染结果/少数候选上调用）。
func (t *Table) Name(i int) string {
	b := t.NameBytes(i)
	if b == nil {
		return ""
	}
	return string(b)
}

// PyOf 返回拼音首字母串，没有时返回空串。
func (t *Table) PyOf(i int) string {
	if t == nil || t.Py == nil {
		return ""
	}
	v, ok := t.Py[int32(i)]
	if !ok {
		return ""
	}
	off, n := UnpackPy(v)
	if off+uint32(n) > uint32(len(t.Names)) {
		return ""
	}
	return string(t.Names[off : off+uint32(n)])
}

// Ext 从文件名里取小写扩展名（含点）。
// 历史记录、应用路径这类没有 Table 的地方用它。
func Ext(name string) string {
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		return strings.ToLower(name[i:])
	}
	return ""
}

// Ext 返回小写扩展名（含点）。
func (t *Table) Ext(i int) string {
	b := t.NameBytes(i)
	dot := -1
	for k := len(b) - 1; k > 0; k-- {
		if b[k] == '.' {
			dot = k
			break
		}
	}
	if dot <= 0 {
		return ""
	}
	ext := string(b[dot:])
	// 绝大多数扩展名本来就是小写，ToLower 会在没有变化时原样返回，不额外分配。
	return strings.ToLower(ext)
}

// Age 把 ModSec 转成时间。
func (t *Table) Age(n Node) time.Time {
	return time.Unix(int64(n.ModSec)+ModBase, 0)
}

// Size 返回文件大小（字节）。
func (n Node) Size() int64 { return int64(n.SizeKB) << 10 }

// PathOf 顺着父链还原完整路径。
func (t *Table) PathOf(i int32) string {
	if t == nil || i < 0 || int(i) >= len(t.Nodes) {
		return ""
	}
	parts := make([]string, 0, 12)
	for cur := i; cur >= 0 && int(cur) < len(t.Nodes); {
		n := &t.Nodes[cur]
		if n.Parent < 0 { // 根节点自身就是 "C:\"
			parts = append(parts, strings.TrimRight(t.Name(int(cur)), `\/`))
			break
		}
		parts = append(parts, t.Name(int(cur)))
		cur = n.Parent
	}
	var b strings.Builder
	for i := len(parts) - 1; i >= 0; i-- {
		if b.Len() > 0 {
			b.WriteByte('\\')
		}
		b.WriteString(parts[i])
	}
	return b.String()
}

// Dir 返回条目的父目录路径，用于结果行的提示文字。
func (t *Table) Dir(i int32) string {
	if t == nil || i < 0 || int(i) >= len(t.Nodes) {
		return ""
	}
	p := t.Nodes[i].Parent
	if p < 0 {
		return ""
	}
	return t.PathOf(p)
}

// Hits 是搜索预筛：名字里包含全部 token 才算命中。
// 直接在 arena 字节上做大小写不敏感的子串比较——零分配，
// 也不再为每条名字预存一份小写副本（那要多吃 40 MB）。
// token 必须是已经小写、且不含空格的单个词。
func (t *Table) Hits(i int, tokens []string) bool {
	name := t.NameBytes(i)
	if len(name) == 0 {
		return false
	}
	for _, tok := range tokens {
		if !containsFold(name, tok) {
			return false
		}
	}
	return true
}

// containsFold 在 name 里找 token（大小写不敏感，只对 ASCII 折大小写；
// 汉字的 UTF-8 字节都 >0x7f，原样比较才是对的）。
func containsFold(name []byte, token string) bool {
	if len(token) == 0 {
		return true
	}
	if len(token) > len(name) {
		return false
	}
	c0 := token[0]
	last := len(name) - len(token)
	for i := 0; i <= last; i++ {
		if lowerByte(name[i]) != c0 {
			continue
		}
		j := 1
		for ; j < len(token); j++ {
			if lowerByte(name[i+j]) != token[j] {
				break
			}
		}
		if j == len(token) {
			return true
		}
	}
	return false
}

func lowerByte(c byte) byte {
	if 'A' <= c && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}

// Index 是并发安全的索引容器。写入只发生在整表替换时。
type Index struct {
	mu sync.RWMutex
	t  *Table
}

// New 返回空索引。
func New() *Index { return &Index{} }

// Replace 整表替换（重建索引后调用）。
func (ix *Index) Replace(t *Table) {
	ix.mu.Lock()
	ix.t = t
	ix.mu.Unlock()
}

// Table 返回当前快照。节点表只在 Replace 时整体替换，
// 旧快照在替换后依然有效（它自己持有自己的字节），读取无需加锁。
func (ix *Index) Table() *Table {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.t
}

// Len 返回条目数。
func (ix *Index) Len() int {
	return ix.Table().Len()
}
