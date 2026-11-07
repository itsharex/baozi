package index

import (
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
	"unsafe"
)

// 磁盘格式 v3 是"定长记录 + arena"，从 v2 的 gob 换过来。
//
// 换的理由有三个，都在内存上：
//  1. gob 解码 []Node 时用 reflect 逐元素 append，179 万条会反复复制切片
//     （实测 reflect.growslice 占 86MB 峰值）；定长记录可以整段一次读进来。
//  2. gob 会把一条 message 的全部字节读进内存（saferio.ReadData 74MB），
//     大表天然吃亏。
//  3. gob 解码每个 string 都是一次独立分配，回来还是上百万个小对象。
//
// 定长 + arena 之后，Load 只需一次 gzip 解压 + 一次 unsafe.Slice：
// nodes 段和 names 段都直接指向那块缓冲，加载时间和内存都接近"打开文件"。
//
// 布局（小端）：
//
//	0..40   固定头（magic/ver/count/namesLen/rawLen/rootsLen/pyCount/nodesOff/namesOff/pyOff）
//	40..    roots JSON
//	..      [补齐到 8 字节，见 nodesOff]
//	..      nodes   count × 20
//	..      names   namesLen
//	..      py      pyCount × 10（idx u32 + off u32 + len u16）
const (
	storeMagic   = "BZID"
	storeVersion = 3
	headerLen    = 40
	pyRecLen     = 10
)

type storeHeader struct {
	Version  uint32
	Count    uint32
	NamesLen uint32
	RawLen   uint32
	RootsLen uint32
	PyCount  uint32
	NodesOff uint32
	NamesOff uint32
	PyOff    uint32
}

// Save 把快照压缩落盘。流式写，不构造整块缓冲。
func Save(path string, t *Table, roots []string) error {
	if t == nil {
		return fmt.Errorf("索引为空")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)

	rootsJSON, err := json.Marshal(roots)
	if err != nil {
		f.Close()
		return err
	}

	count := uint32(len(t.Nodes))
	namesLen := uint32(len(t.Names))
	pyCount := uint32(len(t.Py))
	rootsLen := uint32(len(rootsJSON))
	nodesOff := align8(headerLen + rootsLen)
	namesOff := nodesOff + count*NodeSize
	pyOff := namesOff + namesLen
	rawLen := pyOff + pyCount*pyRecLen

	h := storeHeader{
		Version:  storeVersion,
		Count:    count,
		NamesLen: namesLen,
		RawLen:   rawLen,
		RootsLen: rootsLen,
		PyCount:  pyCount,
		NodesOff: nodesOff,
		NamesOff: namesOff,
		PyOff:    pyOff,
	}

	gz := gzip.NewWriter(f)
	var buf [4]byte
	writeAll := func(p []byte) error {
		_, err := gz.Write(p)
		return err
	}

	// 固定头
	writeAll([]byte(storeMagic))
	for _, v := range []uint32{h.Version, h.Count, h.NamesLen, h.RawLen, h.RootsLen,
		h.PyCount, h.NodesOff, h.NamesOff, h.PyOff} {
		binary.LittleEndian.PutUint32(buf[:], v)
		if err := writeAll(buf[:]); err != nil {
			f.Close()
			return err
		}
	}
	if err := writeAll(rootsJSON); err != nil {
		f.Close()
		return err
	}
	// 补到 nodesOff（对齐，方便将来直接映射成结构数组）
	pad := make([]byte, nodesOff-(headerLen+rootsLen))
	if err := writeAll(pad); err != nil {
		f.Close()
		return err
	}
	// nodes：逐条 20 字节，不依赖 Node 的内存布局
	for i := range t.Nodes {
		n := &t.Nodes[i]
		binary.LittleEndian.PutUint32(buf[:], uint32(n.Parent))
		if err := writeAll(buf[:]); err != nil {
			f.Close()
			return err
		}
		binary.LittleEndian.PutUint32(buf[:], n.NameOff)
		if err := writeAll(buf[:]); err != nil {
			f.Close()
			return err
		}
		binary.LittleEndian.PutUint32(buf[:], uint32(n.SizeKB))
		if err := writeAll(buf[:]); err != nil {
			f.Close()
			return err
		}
		binary.LittleEndian.PutUint32(buf[:], uint32(n.ModSec))
		if err := writeAll(buf[:]); err != nil {
			f.Close()
			return err
		}
		binary.LittleEndian.PutUint16(buf[:2], n.NameLen)
		buf[2] = byte(n.Kind)
		buf[3] = 0
		if err := writeAll(buf[:]); err != nil {
			f.Close()
			return err
		}
	}
	if err := writeAll(t.Names); err != nil {
		f.Close()
		return err
	}
	// py 表：索引 + 拼音串在 names arena 里的偏移/长度
	for idx, v := range t.Py {
		off, n := UnpackPy(v)
		binary.LittleEndian.PutUint32(buf[:], uint32(idx))
		if err := writeAll(buf[:]); err != nil {
			f.Close()
			return err
		}
		binary.LittleEndian.PutUint32(buf[:], off)
		if err := writeAll(buf[:]); err != nil {
			f.Close()
			return err
		}
		// 长度只占 2 字节（pyRecLen=10：idx4 + off4 + len2），不能借 4 字节的 buf 写
		binary.LittleEndian.PutUint16(buf[:2], n)
		if err := writeAll(buf[:2]); err != nil {
			f.Close()
			return err
		}
	}

	if err := gz.Close(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Load 读取快照；文件不存在或版本不匹配时返回错误。
// 同时返回建索引时的根目录，供调用方判断配置是否变过。
func Load(path string) (*Table, []string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, nil, err
	}
	defer gz.Close()

	var h storeHeader
	var head [headerLen]byte
	if _, err := io.ReadFull(gz, head[:]); err != nil {
		return nil, nil, fmt.Errorf("读取索引头: %w", err)
	}
	if string(head[:4]) != storeMagic {
		return nil, nil, fmt.Errorf("不是 Baozi 索引文件")
	}
	fields := []*uint32{&h.Version, &h.Count, &h.NamesLen, &h.RawLen, &h.RootsLen,
		&h.PyCount, &h.NodesOff, &h.NamesOff, &h.PyOff}
	for i, p := range fields {
		*p = binary.LittleEndian.Uint32(head[4+i*4:])
	}
	if h.Version != storeVersion {
		return nil, nil, fmt.Errorf("索引版本 %d 与当前 %d 不匹配，需要重建", h.Version, storeVersion)
	}
	if h.RawLen < headerLen || h.NodesOff > h.RawLen || h.NamesOff > h.RawLen || h.PyOff > h.RawLen {
		return nil, nil, fmt.Errorf("索引头损坏（长度不自洽）")
	}

	// 一次读满剩余部分：rawLen 已知，不会反复扩容。
	raw := make([]byte, h.RawLen)
	copy(raw, head[:])
	if _, err := io.ReadFull(gz, raw[headerLen:]); err != nil {
		return nil, nil, fmt.Errorf("读取索引体: %w", err)
	}

	t := &Table{
		Names: raw[h.NamesOff : h.NamesOff+h.NamesLen],
		raw:   raw,
	}
	if h.PyCount > 0 {
		t.Py = make(map[int32]uint64, h.PyCount)
		for i := uint32(0); i < h.PyCount; i++ {
			rec := raw[h.PyOff+i*pyRecLen:]
			idx := binary.LittleEndian.Uint32(rec)
			off := binary.LittleEndian.Uint32(rec[4:])
			n := binary.LittleEndian.Uint16(rec[8:])
			if off+uint32(n) > h.NamesLen {
				continue
			}
			t.Py[int32(idx)] = PackPy(off, n)
		}
	}

	// nodes 段整段映射成结构数组（零拷贝、零解码）。
	// 前提是对齐和记录宽度都对得上；对不上就退回逐条解析，保证还能用。
	if h.NodesOff%4 == 0 && unsafe.Sizeof(Node{}) == NodeSize && int(h.NodesOff)+int(h.Count)*NodeSize <= len(raw) {
		p := unsafe.Pointer(&raw[h.NodesOff])
		t.Nodes = unsafe.Slice((*Node)(p), h.Count)
	} else {
		t.Nodes = make([]Node, h.Count)
		for i := uint32(0); i < h.Count; i++ {
			rec := raw[h.NodesOff+i*NodeSize:]
			t.Nodes[i] = Node{
				Parent:  int32(binary.LittleEndian.Uint32(rec)),
				NameOff: binary.LittleEndian.Uint32(rec[4:]),
				SizeKB:  int32(binary.LittleEndian.Uint32(rec[8:])),
				ModSec:  int32(binary.LittleEndian.Uint32(rec[12:])),
				NameLen: binary.LittleEndian.Uint16(rec[16:]),
				Kind:    Kind(rec[18]),
			}
		}
	}

	var roots []string
	if h.RootsLen > 0 {
		if err := json.Unmarshal(raw[headerLen:headerLen+h.RootsLen], &roots); err != nil {
			roots = nil
		}
	}
	return t, roots, nil
}

// Info 返回快照的基本信息（条目数、构建时间），不需要解码整表。
func Info(path string) (count int, builtAt time.Time, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, time.Time{}, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return 0, time.Time{}, err
	}
	defer gz.Close()
	var head [headerLen]byte
	if _, err := io.ReadFull(gz, head[:]); err != nil {
		return 0, time.Time{}, err
	}
	if string(head[:4]) != storeMagic {
		return 0, time.Time{}, fmt.Errorf("不是 Baozi 索引文件")
	}
	count = int(binary.LittleEndian.Uint32(head[8:]))
	st, err := f.Stat()
	if err != nil {
		return count, time.Time{}, nil
	}
	return count, st.ModTime(), nil
}

func align8(v uint32) uint32 { return (v + 7) &^ 7 }
