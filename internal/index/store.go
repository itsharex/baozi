package index

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// 磁盘格式 v4：未压缩的定长记录 + arena，整份文件直接 MapViewOfFile 映射进来。
//
// 从 v3 的 gzip 改过来，是为了让 nodes 段从"堆上的 55MB 数组"变成"文件页"：
//
//	v3  Load 时 gzip 解出一整块 55MB 的缓冲，Nodes/Names 用 unsafe.Slice 指向它 ——
//	    看着是零拷贝，实际这块缓冲是 Go 堆上的匿名内存，全量常驻、页还回不去；
//	    再加上增量时 Clone 复制一份可写表，堆里同时躺着两三份。
//	v4  文件不压缩，直接映射。Nodes 指进映射区，堆里只有一个 24 字节的表头和
//	    一个指针；页是 file-backed 的干净页，内存紧的时候系统可以直接丢掉，
//	    下次访问再从文件读回来。
//
// 代价是磁盘变大（gzip 19MB → 未压缩约 55MB）。这笔账是划算的：索引文件躺在
// %LOCALAPPDATA% 里，而省下来的是**每次唤出都在的常驻内存**。
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
	storeVersion = 4
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

// mapping 是一份映射进来的索引文件。Table 活着它就活着，Table 被回收时
// 由 finalizer 解开 —— 搜索那边可能还持有旧快照的引用，所以不能用"换表就关"。
type mapping struct {
	data []byte
	view uintptr
	mapH windows.Handle
	file windows.Handle
	// ref 是引用计数：Clone 出来的表共享同一份映射，最后一个被回收时解开。
	ref int32
}

func (m *mapping) close() {
	if m == nil {
		return
	}
	if m.view != 0 {
		_ = windows.UnmapViewOfFile(m.view)
		m.view = 0
	}
	if m.mapH != 0 {
		_ = windows.CloseHandle(m.mapH)
		m.mapH = 0
	}
	if m.file != 0 {
		_ = windows.CloseHandle(m.file)
		m.file = 0
	}
	m.data = nil
}

// Save 把快照落盘（未压缩），返回实际写入的文件路径。流式写，不构造整块缓冲。
//
// 表里如果叠了增量（overlay），先合并成一张连续的表再写：磁盘上永远只有
// "一份完整的基座"，读取路径才不用认识 overlay。
//
// 返回路径不一定等于 path：从 v4 起主进程用 mmap 长期持有索引文件，
// 而 Windows 不允许替换/删除正被映射的文件——服务端原地覆盖会失败。
// 这时候退一步写到新一代文件（index-<时间戳>.bin），加载方按 mtime 挑最新的一份。
func Save(path string, t *Table, roots []string) (string, error) {
	if t == nil || t.Len() == 0 {
		return "", fmt.Errorf("索引为空")
	}
	// 增量层（新条目或墓碑）都要先并回基座：磁盘上永远只有"一份完整表"，
	// 读取路径才不用认识 overlay。
	if t.OvLen() > 0 || len(t.dead) > 0 {
		t = t.Merge()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp)
	// 一条记录要写十几次小 Write，直接怼到文件上就是上百万次系统调用；
	// 先用一块缓冲把它们攒起来。
	w := bufio.NewWriterSize(f, 1<<16)

	rootsJSON, err := json.Marshal(roots)
	if err != nil {
		f.Close()
		return "", err
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

	var buf [4]byte
	writeAll := func(p []byte) error {
		_, err := w.Write(p)
		return err
	}
	abort := func() (string, error) {
		f.Close()
		return "", err
	}

	// 固定头
	if err := writeAll([]byte(storeMagic)); err != nil {
		return abort()
	}
	for _, v := range []uint32{h.Version, h.Count, h.NamesLen, h.RawLen, h.RootsLen,
		h.PyCount, h.NodesOff, h.NamesOff, h.PyOff} {
		binary.LittleEndian.PutUint32(buf[:], v)
		if err := writeAll(buf[:]); err != nil {
			return abort()
		}
	}
	if err := writeAll(rootsJSON); err != nil {
		return abort()
	}
	// 补到 nodesOff（对齐，Node 数组要能直接映射）
	if pad := int(nodesOff) - headerLen - int(rootsLen); pad > 0 {
		if err := writeAll(make([]byte, pad)); err != nil {
			return abort()
		}
	}
	// nodes：逐条 20 字节，不依赖 Node 的内存布局
	for i := range t.Nodes {
		n := &t.Nodes[i]
		binary.LittleEndian.PutUint32(buf[:], uint32(n.Parent))
		if err := writeAll(buf[:]); err != nil {
			return abort()
		}
		binary.LittleEndian.PutUint32(buf[:], n.NameOff)
		if err := writeAll(buf[:]); err != nil {
			return abort()
		}
		binary.LittleEndian.PutUint32(buf[:], uint32(n.SizeKB))
		if err := writeAll(buf[:]); err != nil {
			return abort()
		}
		binary.LittleEndian.PutUint32(buf[:], uint32(n.ModSec))
		if err := writeAll(buf[:]); err != nil {
			return abort()
		}
		binary.LittleEndian.PutUint16(buf[:2], n.NameLen)
		buf[2] = byte(n.Kind)
		buf[3] = 0
		if err := writeAll(buf[:]); err != nil {
			return abort()
		}
	}
	if err := writeAll(t.Names); err != nil {
		return abort()
	}
	// py 表：索引 + 拼音串在 names arena 里的偏移/长度
	for idx, v := range t.Py {
		off, n := UnpackPy(v)
		binary.LittleEndian.PutUint32(buf[:], uint32(idx))
		if err := writeAll(buf[:]); err != nil {
			return abort()
		}
		binary.LittleEndian.PutUint32(buf[:], off)
		if err := writeAll(buf[:]); err != nil {
			return abort()
		}
		// 长度只占 2 字节（pyRecLen=10：idx4 + off4 + len2），不能借 4 字节的 buf 写
		binary.LittleEndian.PutUint16(buf[:2], n)
		if err := writeAll(buf[:2]); err != nil {
			return abort()
		}
	}

	if err := w.Flush(); err != nil {
		return abort()
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err == nil {
		return path, nil
	} else if alt := generationPath(path, time.Now()); alt != "" {
		// 目标正被映射（主进程持有上一代），Windows 不允许替换 —— 写新的一代
		if e2 := os.Rename(tmp, alt); e2 == nil {
			return alt, nil
		}
		return "", err
	} else {
		return "", err
	}
}

// generationPath 生成"新一代"文件名：index.bin → index-<秒级时间戳>.bin。
// 反复重建时秒级可能撞上，撞了就等到下一秒。
func generationPath(path string, now time.Time) string {
	dir, base := filepath.Dir(path), filepath.Base(path)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	return filepath.Join(dir, fmt.Sprintf("%s-%d%s", stem, now.Unix(), ext))
}

// Latest 在目录里挑最新的一份索引文件（index.bin 或 index-<时间戳>.bin）。
// 主进程和服务端都在用：服务端写不出 index.bin 时会写代次文件，得能被发现。
func Latest(dataDir string) (string, time.Time, error) {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return "", time.Time{}, err
	}
	var (
		best     string
		bestTime time.Time
	)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// index.bin / index-<ts>.bin；排除 .tmp、备份
		if !strings.HasPrefix(name, "index") || !strings.HasSuffix(name, ".bin") {
			continue
		}
		info, err := e.Info()
		if err != nil || info.Size() < headerLen {
			continue
		}
		if best == "" || info.ModTime().After(bestTime) {
			best, bestTime = filepath.Join(dataDir, name), info.ModTime()
		}
	}
	if best == "" {
		return "", time.Time{}, os.ErrNotExist
	}
	return best, bestTime, nil
}

// Prune 删掉除 keep 之外的旧索引代次。被人映射着的文件删不掉（这正是它还在的原因），
// 那种情况留着下次再删，不当成错误。
func Prune(dataDir, keep string) {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return
	}
	keepName := filepath.Base(keep)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || name == keepName || !strings.HasPrefix(name, "index") || !strings.HasSuffix(name, ".bin") {
			continue
		}
		_ = os.Remove(filepath.Join(dataDir, name))
	}
}

// Load 映射一份快照；文件不存在或版本不匹配时返回错误。
//
// 除了固定头和 py 表（九千来条，解析成 map 很便宜），其余一个字节都不碰：
// Nodes 和 Names 都是映射区的切片，堆占用跟条目数无关。
func Load(path string) (*Table, []string, error) {
	m, err := mapIndexFile(path)
	if err != nil {
		return nil, nil, err
	}
	raw := m.data
	var h storeHeader
	if len(raw) < headerLen {
		m.close()
		return nil, nil, fmt.Errorf("索引文件太短")
	}
	if string(raw[:4]) != storeMagic {
		m.close()
		return nil, nil, fmt.Errorf("不是 Baozi 索引文件")
	}
	fields := []*uint32{&h.Version, &h.Count, &h.NamesLen, &h.RawLen, &h.RootsLen,
		&h.PyCount, &h.NodesOff, &h.NamesOff, &h.PyOff}
	for i, p := range fields {
		*p = binary.LittleEndian.Uint32(raw[4+i*4:])
	}
	if h.Version != storeVersion {
		m.close()
		return nil, nil, fmt.Errorf("索引版本 %d 与当前 %d 不匹配，需要重建", h.Version, storeVersion)
	}
	if h.RawLen < headerLen || h.NodesOff > h.RawLen || h.NamesOff > h.RawLen || h.PyOff > h.RawLen ||
		uint32(len(raw)) < h.RawLen {
		m.close()
		return nil, nil, fmt.Errorf("索引头损坏（长度不自洽）")
	}

	t := &Table{mp: m}
	// nodes 段直接映射成结构数组（零拷贝、零解码）。对齐与记录宽度都对得上才映射，
	// 否则退回逐条解析，保证还能用。
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
	t.Names = raw[h.NamesOff : h.NamesOff+h.NamesLen]
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

	var roots []string
	if h.RootsLen > 0 {
		if err := json.Unmarshal(raw[headerLen:headerLen+h.RootsLen], &roots); err != nil {
			roots = nil
		}
	}
	// 映射跟着表走：搜索线程可能还拿着旧快照，不能在换表时关掉。
	setTableFinalizer(t)
	return t, roots, nil
}

// mapIndexFile 把文件映射成只读视图。
func mapIndexFile(path string) (*mapping, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	f, err := windows.CreateFile(p,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, err
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(f, &info); err != nil {
		windows.CloseHandle(f)
		return nil, err
	}
	size := uint64(info.FileSizeHigh)<<32 | uint64(info.FileSizeLow)
	if size == 0 {
		windows.CloseHandle(f)
		return nil, fmt.Errorf("索引文件是空的")
	}
	mh, err := windows.CreateFileMapping(f, nil, windows.PAGE_READONLY, uint32(size>>32), uint32(size&0xffffffff), nil)
	if err != nil {
		windows.CloseHandle(f)
		return nil, err
	}
	addr, err := windows.MapViewOfFile(mh, windows.FILE_MAP_READ, 0, 0, uintptr(size))
	if err != nil {
		windows.CloseHandle(mh)
		windows.CloseHandle(f)
		return nil, err
	}
	return &mapping{
		// unsafe.Add 从 nil 起偏移：MapViewOfFile 给的是裸地址，
		// 这样写能避开 uintptr→Pointer 的直接转换。
		data: unsafe.Slice((*byte)(unsafe.Add(nil, addr)), int(size)),
		view: addr,
		mapH: mh,
		file: f,
		ref:  1,
	}, nil
}

// Info 返回快照的基本信息（条目数、构建时间），不需要读整张表。
func Info(path string) (count int, builtAt time.Time, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, time.Time{}, err
	}
	defer f.Close()
	var head [headerLen]byte
	if _, err := f.Read(head[:]); err != nil {
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
