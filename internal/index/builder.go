package index

// Builder 用来从任何数据源构造一张 Table。
// WalkDir 扫描和 USN Journal 枚举走的是同一个构建器，
// 保证两条路径产出的索引在结构上完全一致（同一套 arena/定长记录）。
type Builder struct {
	t     *Table
	nodes []Node
}

// NewBuilder 预留容量。hint 是预估条目数，只影响一次性分配的大小。
func NewBuilder(hint int) *Builder {
	if hint < 0 {
		hint = 0
	}
	return &Builder{
		t: &Table{
			Names: make([]byte, 0, hint*24),
			Py:    make(map[int32]uint64, hint/64),
		},
		nodes: make([]Node, 0, hint),
	}
}

// Add 追加一个条目，返回它的编号。name 会被拷进名字 arena。
// parent 为 -1 表示这是根（自身名字就是 "C:\"）。
func (b *Builder) Add(name string, parent int32, kind Kind, sizeKB int32, modSec int32) int32 {
	off := uint32(len(b.t.Names))
	b.t.Names = append(b.t.Names, name...)
	id := int32(len(b.nodes))
	b.nodes = append(b.nodes, Node{
		Parent:  parent,
		NameOff: off,
		NameLen: uint16(len(name)),
		SizeKB:  sizeKB,
		ModSec:  modSec,
		Kind:    kind,
	})
	return id
}

// SetPy 记下某个条目的拼音首字母串。拼音串同样放进 arena 尾部。
func (b *Builder) SetPy(id int32, py string) {
	if py == "" {
		return
	}
	off := uint32(len(b.t.Names))
	b.t.Names = append(b.t.Names, py...)
	b.t.Py[id] = PackPy(off, uint16(len(py)))
}

// Len 返回已经追加的条目数。
func (b *Builder) Len() int { return len(b.nodes) }

// Name 返回已追加条目的名字（给构建器外部做过滤/日志用）。
func (b *Builder) Name(id int32) string {
	if id < 0 || int(id) >= len(b.nodes) {
		return ""
	}
	n := &b.nodes[id]
	return string(b.t.Names[n.NameOff : n.NameOff+uint32(n.NameLen)])
}

// Table 收尾，返回不可变快照。
// 返回后不要再用这个 Builder（Nodes 直接交给 Table，再 Add 会破坏快照）。
func (b *Builder) Table() *Table {
	b.t.Nodes = b.nodes
	// 名字 arena 可能因为 append 留了多余容量，收一下，别让 30% 的空隙跟着快照走。
	b.t.Names = b.t.Names[:len(b.t.Names):len(b.t.Names)]
	return b.t
}
