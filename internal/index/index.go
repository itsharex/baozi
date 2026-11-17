// Package index 是内存索引：每个条目只存"名字在 arena 里的位置 + 父节点编号"，
// 不存完整路径——这正是 NTFS $MFT 的存法，也是 Lertaro 索引层的思路。
//
// 为什么名字要放进 arena 而不是每条一个 string：179 万条的名字如果各自分配
// （string header 16 字节 + 分配器按 size class 向上取整 + 每个对象的管理开销），
// 40 MB 的实际内容会占到 ~90 MB，还附带上百万个对象让 GC 每次都要扫。
// 现在名字是 Names 里连续的一段字节，Node 只有定长 20 字节。
// 定长的好处不止省内存：整个 nodes 段可以 unsafe.Slice 成一个结构数组，
// 加载时零拷贝。
//
// 从 v4 起索引是"只读基座 + 增量层"两层：
//
//	基座  Nodes/Names/Py 指向 mmap 进来的文件（file-backed 页，堆里只留指针）。
//	      只读：绝不 append、绝不改元素。
//	增量  ovNodes/ovNames/ovPy/dead 是堆上的小数组，按 USN 到来的顺序追加；
//	      墓碑记在 dead 集合里，不回头改基座。
//
// 这样增量不再是"复制一份 78MB 的表再往上改"，而是"往一个几十 KB 的尾巴上追加"——
// 那个 30 秒一次的 copy-on-write 复制（实测让 WS 多挂 100MB）就没有了。
// 攒够阈值再全量重建（见 Mutable.Compact），把尾巴并回基座。
package index

import (
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Kind 是条目类型。
type Kind uint8

const (
	KindFile Kind = iota
	KindDir
	// KindDeleted 是墓碑：条目已经被删掉，但槽位不能抹掉——Parent 存的是数组下标，
	// 抹掉会让所有后继下标错位。搜索时看到它就丢弃（连同它下面的子树，
	// 见 Table.Dead）。
	//
	// 为什么不能真删：NTFS 删一个目录时，USN 里只有目录本身一条记录，里面那几千个
	// 文件不会有记录。所以"父被删"必须能被子条目看见，父子链就得留着。
	KindDeleted
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

// Table 是一次完整的索引快照：只读基座 + 小的增量层。
//
// Nodes 与 Names 可能指向 mmap 进来的文件，所以 Table 必须持有那份映射
// （mp 字段和它的引用计数），否则切片会悬垂。
type Table struct {
	// 基座（只读）。构建期它们是堆上还没保存的数组，那时可以 append；
	// 一旦 Load 过就是映射区，只读。
	Nodes []Node
	Names []byte
	// Py 只对名字含汉字的条目存在（179 万条约 9 千条），
	// 值把"偏移（高 32 位）+ 长度（低 16 位）"打包成一个 uint64。
	Py map[int32]uint64

	// 增量层（堆上，可写）
	ovNodes []Node
	ovNames []byte
	ovPy    map[int32]uint64
	// dead 是墓碑集合：逻辑下标 → 已删除。基座里的条目只能这样标，
	// 不然就要把映射区改成可写（那是共享的文件页，不能动）。
	dead map[int32]struct{}

	mp *mapping
}

// PackPy 把拼音串在 arena 里的位置打包。
func PackPy(off uint32, n uint16) uint64 { return uint64(off)<<16 | uint64(n) }

// UnpackPy 拆开 PackPy 的打包值。
func UnpackPy(v uint64) (off uint32, n uint16) { return uint32(v >> 16), uint16(v & 0xffff) }

// retain/release 让映射跟着"还在用它"的表走：Clone 出来的表共享同一份映射，
// 谁最后被回收谁负责解开。
func (m *mapping) retain() {
	if m != nil {
		atomic.AddInt32(&m.ref, 1)
	}
}

func (m *mapping) release() {
	if m == nil {
		return
	}
	if atomic.AddInt32(&m.ref, -1) <= 0 {
		m.close()
	}
}

// setTableFinalizer 给表挂上"回收时解开映射"的终结器。
// 换表那一刻还有搜索线程拿着旧表，不能当场关，只能等它没人用。
func setTableFinalizer(t *Table) {
	if t.mp == nil {
		return
	}
	mp := t.mp
	runtime.SetFinalizer(t, func(*Table) { mp.release() })
}

// Clone 复制一份**可写**的表，用来把增量叠上去。
//
// 关键变化：不再复制基座。基座是只读的（多半还是映射区），共享就好；
// 要复制的只有增量层那个小尾巴。代价从"76MB 复制 ≈ 15ms + 15MB 常驻"
// 变成"几 KB ~ 几 MB"。
//
// 为什么不原地改增量层：搜索是在没有锁的情况下遍历当前快照的（179 万条，
// 走几十毫秒），一旦它身后有人在 append，切片长度和底层数组都会变，
// 正在遍历的 goroutine 就会读到越界数据。复制一份、改完再原子换指针，
// 搜索侧完全不用管锁。
func (t *Table) Clone() *Table {
	if t == nil {
		return nil
	}
	nt := &Table{
		Nodes: t.Nodes,
		Names: t.Names,
		Py:    t.Py,
		mp:    t.mp,
	}
	nt.mp.retain()
	if n := len(t.ovNodes); n > 0 {
		nt.ovNodes = make([]Node, n, n+4096)
		copy(nt.ovNodes, t.ovNodes)
	} else {
		nt.ovNodes = make([]Node, 0, 1024)
	}
	if n := len(t.ovNames); n > 0 {
		nt.ovNames = make([]byte, n, n+(1<<20))
		copy(nt.ovNames, t.ovNames)
	} else {
		nt.ovNames = make([]byte, 0, 1<<16)
	}
	if n := len(t.ovPy); n > 0 {
		nt.ovPy = make(map[int32]uint64, n+16)
		for k, v := range t.ovPy {
			nt.ovPy[k] = v
		}
	}
	if n := len(t.dead); n > 0 {
		nt.dead = make(map[int32]struct{}, n+16)
		for k := range t.dead {
			nt.dead[k] = struct{}{}
		}
	}
	setTableFinalizer(nt)
	return nt
}

// Merge 把基座和增量层合成一张连续的、纯堆上的表。
// 只在 Save 和"全量重建"时用：那一刻本来就要付一次 O(n) 的代价，
// 换来的是磁盘上永远只有一份完整基座，读取路径不用认识增量层。
func (t *Table) Merge() *Table {
	if t == nil {
		return nil
	}
	if t.OvLen() == 0 && len(t.dead) == 0 {
		return t
	}
	baseN := len(t.Nodes)
	baseNames := len(t.Names)
	nodes := make([]Node, baseN+len(t.ovNodes))
	copy(nodes, t.Nodes)
	// 增量层的名字整体追加在基座 arena 之后：这样 ovNames 里的每个偏移
	// 只是整体平移 baseNames，不用逐条重算。
	names := make([]byte, baseNames, baseNames+len(t.ovNames))
	names = append(names, t.ovNames...)
	for i := range t.ovNodes {
		nd := t.ovNodes[i]
		nd.NameOff += uint32(baseNames)
		nodes[baseN+i] = nd
	}
	py := make(map[int32]uint64, len(t.Py)+len(t.ovPy))
	for k, v := range t.Py {
		py[k] = v
	}
	for k, v := range t.ovPy {
		off, n := UnpackPy(v)
		py[k] = PackPy(off+uint32(baseNames), n)
	}
	for idx := range t.dead {
		if int(idx) >= 0 && int(idx) < len(nodes) {
			nodes[idx].Kind = KindDeleted
		}
	}
	return &Table{Nodes: nodes, Names: names, Py: py}
}

// Len 返回条目数（基座 + 增量层）。
func (t *Table) Len() int {
	if t == nil {
		return 0
	}
	return len(t.Nodes) + len(t.ovNodes)
}

// OvLen 返回增量层里的条目数。
func (t *Table) OvLen() int {
	if t == nil {
		return 0
	}
	return len(t.ovNodes)
}

// At 返回第 i 条的指针。i < 基座长度时指向基座，否则指向增量层。
// 只读调用方不能改它的内容；要改走 Mutable / ApplyOps。
func (t *Table) At(i int32) *Node {
	if t == nil || i < 0 {
		return nil
	}
	if int(i) < len(t.Nodes) {
		return &t.Nodes[i]
	}
	j := int(i) - len(t.Nodes)
	if j < len(t.ovNodes) {
		return &t.ovNodes[j]
	}
	return nil
}

// NameBytes 返回名字的字节切片（零拷贝，不要修改）。
func (t *Table) NameBytes(i int) []byte {
	if t == nil || i < 0 || i >= t.Len() {
		return nil
	}
	var n *Node
	var arena []byte
	if i < len(t.Nodes) {
		n, arena = &t.Nodes[i], t.Names
	} else {
		n, arena = &t.ovNodes[i-len(t.Nodes)], t.ovNames
	}
	end := int64(n.NameOff) + int64(n.NameLen)
	if end > int64(len(arena)) {
		return nil
	}
	return arena[n.NameOff:end]
}

// Name 返回名字字符串（会分配一次，只在渲染结果/少数候选上调用）。
func (t *Table) Name(i int) string {
	b := t.NameBytes(i)
	if b == nil {
		return ""
	}
	return string(b)
}

// pyRange 返回第 i 条拼音串所属的 arena 和位置。
func (t *Table) pyRange(i int) (arena []byte, off uint32, n uint16, ok bool) {
	if t == nil {
		return nil, 0, 0, false
	}
	var v uint64
	if ov, has := t.ovPy[int32(i)]; has {
		v, arena = ov, t.ovNames
	} else {
		if t.Py == nil {
			return nil, 0, 0, false
		}
		if v, has = t.Py[int32(i)]; !has {
			return nil, 0, 0, false
		}
		arena = t.Names
	}
	off, n = UnpackPy(v)
	if off+uint32(n) > uint32(len(arena)) {
		return nil, 0, 0, false
	}
	return arena, off, n, true
}

// PyOf 返回拼音首字母串，没有时返回空串。
func (t *Table) PyOf(i int) string {
	arena, off, n, ok := t.pyRange(i)
	if !ok {
		return ""
	}
	return string(arena[off : off+uint32(n)])
}

// pyBytes 是 PyOf 的零拷贝版。
func (t *Table) pyBytes(i int) []byte {
	arena, off, n, ok := t.pyRange(i)
	if !ok {
		return nil
	}
	return arena[off : off+uint32(n)]
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

// KindOf 返回条目的类型，墓碑（基座里标不了，记在 dead 集合）也算 KindDeleted。
// 调用方不必先查 Dead 再查 Kind。
func (t *Table) KindOf(i int32) Kind {
	if _, ok := t.dead[i]; ok {
		return KindDeleted
	}
	if n := t.At(i); n != nil {
		return n.Kind
	}
	return KindDeleted
}

// PathOf 顺着父链还原完整路径。
func (t *Table) PathOf(i int32) string {
	if t == nil || i < 0 || int(i) >= t.Len() {
		return ""
	}
	parts := make([]string, 0, 12)
	for cur := i; cur >= 0 && int(cur) < t.Len(); {
		n := t.At(cur)
		if n == nil || t.KindOf(cur) == KindDeleted { // 已删除的条目不该再拼出路径
			return ""
		}
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

// Dead 判断条目是不是已经失效：自己或它的某个祖先被标了墓碑。
// 只在**已经命中预筛的候选**上调用——全表都上溯会白白多花几十毫秒，
// 而命中集通常只有几十条。
func (t *Table) Dead(i int32) bool {
	if t == nil || i < 0 || int(i) >= t.Len() {
		return true
	}
	if _, ok := t.dead[i]; ok {
		return true
	}
	// 深度上限：正常路径不会超过几十层，超了说明数据本身有问题，当失效处理。
	for cur, depth := i, 0; cur >= 0 && int(cur) < t.Len() && depth < 256; depth++ {
		if _, ok := t.dead[cur]; ok {
			return true
		}
		n := t.At(cur)
		if n == nil {
			return true
		}
		if n.Kind == KindDeleted {
			return true
		}
		cur = n.Parent
	}
	return false
}

// Dir 返回条目的父目录路径，用于结果行的提示文字。
func (t *Table) Dir(i int32) string {
	if t == nil || i < 0 || int(i) >= t.Len() {
		return ""
	}
	n := t.At(i)
	if n == nil || n.Parent < 0 {
		return ""
	}
	return t.PathOf(n.Parent)
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
		// 名字比词短就不可能包含它：这里先挡掉，省下 containsFold 的扫描。
		// 这是最便宜的一道预筛（179 万条里绝大多数名字只有一两个字符的差别）。
		if len(tok) > len(name) {
			return false
		}
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
// 旧快照在替换后依然有效（它自己持有自己的映射引用），读取无需加锁。
func (ix *Index) Table() *Table {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.t
}

// Len 返回条目数。
func (ix *Index) Len() int {
	return ix.Table().Len()
}
