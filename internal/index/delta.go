package index

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Delta 是 index.bin 之后攒下来的增量指令，由服务写、主进程读。
//
// 为什么要这个文件：服务现在实时改内存表，但不能每改一条就把 19MB 的
// index.bin 重写一遍（那是 0.9 秒 IO）。所以全量快照仍按低频写，
// 中间的变化走这个小文件（几千条 = 一两百 KB）。
//
// Base 是"这份增量是基于哪一次全量"的标识（用 index.progress.json 的 builtAt）。
// 主进程只有在自己载入的快照正好是那一代时才敢往上叠——否则下标会错位，
// 叠出来的路径全是错的。
type Delta struct {
	Base uint64    `json:"base"`
	Seq  uint64    `json:"seq"`
	Ops  []DeltaOp `json:"ops"`
}

// WriteDelta 原子写增量文件（先写临时文件再改名，避免主进程读到半个文件）。
func WriteDelta(path string, d *Delta) error {
	if d == nil {
		return fmt.Errorf("增量为空")
	}
	data, err := json.Marshal(d)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ReadDelta 读增量文件；不存在时返回 (nil, nil)——
// 那不是错误，只是"这份快照之后还没有变化"。
func ReadDelta(path string) (*Delta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	d := &Delta{}
	if err := json.Unmarshal(data, d); err != nil {
		return nil, err
	}
	return d, nil
}

// ApplyOps 把增量指令叠到表上，返回新产生的墓碑数。
//
// 调用方必须保证 t 是一张"可写"的表：Clone 出来的（只复制了增量层那个小尾巴），
// 或者服务自己持有的构建产物。共享给搜索的那份快照不能在这里改。
//
// 从 v4 起不再"复制整表再原地改"：基座是只读的（多半还是映射区，改了会脏页、
// 而且 Clone 出来的几代表共享同一份），所以新增条目进增量层（ovNodes/ovNames），
// 删除记进墓碑集合（dead）。基座一个字节都不动。
//
// 下标校验：主进程要靠 ops 里的下标和服务端的状态对齐。只要发现一条对不上
// （通常是主进程载入的是旧一代快照），就停止并把 ok=false 报回去，
// 让调用方放弃这次合并、等下一次全量载入。
func (t *Table) ApplyOps(ops []DeltaOp) (dead int, ok bool) {
	for _, o := range ops {
		switch o.Op {
		case OpDelete:
			if o.Idx < 0 || int(o.Idx) >= t.Len() {
				return dead, false
			}
			if t.KindOf(o.Idx) == KindDeleted {
				continue
			}
			t.markDead(o.Idx)
			dead++
		case OpAdd:
			if o.Kill > 0 && int(o.Kill) < t.Len() && t.KindOf(o.Kill) != KindDeleted {
				t.markDead(o.Kill)
				dead++
			}
			// 追加位置必须正好是下一个空位，否则两边下标已经错开了。
			if o.Idx != int32(t.Len()) {
				return dead, false
			}
			t.appendOverlay(o.Name, o.PIdx, Kind(o.Kind), 0, 0, o.Py)
		default:
			return dead, false
		}
	}
	return dead, true
}

// markDead 把一条记进墓碑集合。基座里的条目只能这样标：
// 那是只读的映射区，改一个字节就是把整个文件页弄脏。
func (t *Table) markDead(i int32) {
	if i < 0 {
		return
	}
	if t.dead == nil {
		t.dead = make(map[int32]struct{}, 64)
	}
	t.dead[i] = struct{}{}
}

// appendOverlay 往增量层追加一条，返回它的逻辑下标。
func (t *Table) appendOverlay(name string, parent int32, kind Kind, sizeKB, modSec int32, py string) int32 {
	off := uint32(len(t.ovNames))
	t.ovNames = append(t.ovNames, name...)
	idx := int32(len(t.Nodes) + len(t.ovNodes))
	t.ovNodes = append(t.ovNodes, Node{
		Parent:  parent,
		NameOff: off,
		SizeKB:  sizeKB,
		ModSec:  modSec,
		NameLen: uint16(len(name)),
		Kind:    kind,
	})
	if py != "" {
		pyOff := uint32(len(t.ovNames))
		t.ovNames = append(t.ovNames, py...)
		if t.ovPy == nil {
			t.ovPy = make(map[int32]uint64, 64)
		}
		t.ovPy[idx] = PackPy(pyOff, uint16(len(py)))
	}
	return idx
}
