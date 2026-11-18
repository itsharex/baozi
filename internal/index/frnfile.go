package index

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// FRN → 下标 的旁挂文件（sidecar）。
//
// 为什么要有这个文件：服务进程维护增量时必须回答"这条 USN 记录的 FRN 对应哪个
// 条目"。那张开放寻址表在 177 万条时是**堆上 30MB 常驻**（2.6M 槽 × 12 字节，
// 装载率 0.8 必然要翻一次倍），是 baozi-index 里最大的一块 —— 索引本体（Nodes /
// Names）早就改成文件映射了，就它还压在堆里。
//
// 现在改成：重建时把这 177 万对 (frn, idx) 排好序写成一个旁挂文件，服务端把它
// 映射进来，用二分查找（21 步、每次只摸两三页）。堆里只剩"增量新增的那几十上百
// 条"，那 30MB 变成 file-backed 页 —— 空闲时系统随时可以丢掉，下次访问再从文件
// 读回来。
//
// 为什么不直接并进 index.bin：那要改主索引的格式版本，而主进程（面板）也在读
// 同一份文件，为一个只有服务端用得上的表去动公共格式不划算。旁挂文件与 bin 同代
// 命名（index-<代>.frn），服务端启动时总会跟着重建，不存在版本错配。
//
// 布局（小端）：
//
//	0..16   头（magic "BZFR" / ver / count / 保留）
//	16..    count × 12（frn u64 + idx u32），frn 升序
const (
	frnFileMagic = "BZFR"
	frnFileVer   = 1
	frnHeaderLen = 16
	frnRecLen    = 12
)

type frnPair struct {
	frn uint64
	idx int32
}

// FRNPathFor 给出与某个 bin 同代的旁挂文件名：index.bin → index.frn，
// index-1794999783.bin → index-1794999783.frn。
func FRNPathFor(binPath string) string {
	return strings.TrimSuffix(binPath, ".bin") + ".frn"
}

// SaveFRNFile 把 FRN 表排好序写盘，返回实际写入的路径（正被映射时写成新代次，
// 规则和 Save 一样）。m 为空时返回错误，调用方据此退回堆上那份。
func SaveFRNFile(path string, m *FRNMap) (string, error) {
	if m == nil || m.used == 0 {
		return "", fmt.Errorf("FRN 表为空")
	}
	// 先收敛成有序的一维数组：1.77M × 16 字节 ≈ 28MB，只在重建时存活一小会儿。
	pairs := make([]frnPair, 0, m.used)
	for i, k := range m.keys {
		if k != 0 {
			pairs = append(pairs, frnPair{frn: k, idx: m.vals[i]})
		}
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].frn < pairs[j].frn })

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp)
	w := bufio.NewWriterSize(f, 1<<16)
	var head [frnHeaderLen]byte
	copy(head[:4], frnFileMagic)
	binary.LittleEndian.PutUint32(head[4:], frnFileVer)
	binary.LittleEndian.PutUint32(head[8:], uint32(len(pairs)))
	if _, err := w.Write(head[:]); err != nil {
		f.Close()
		return "", err
	}
	var rec [frnRecLen]byte
	for _, p := range pairs {
		binary.LittleEndian.PutUint64(rec[0:], p.frn)
		binary.LittleEndian.PutUint32(rec[8:], uint32(p.idx))
		if _, err := w.Write(rec[:]); err != nil {
			f.Close()
			return "", err
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err == nil {
		return path, nil
	} else if alt := generationPath(path, time.Now()); alt != "" {
		// 上一代正被服务自己映射着，Windows 不允许替换 —— 写新的一代
		if e2 := os.Rename(tmp, alt); e2 == nil {
			return alt, nil
		}
		return "", err
	} else {
		return "", err
	}
}

// FRNFile 是一份映射进来的 FRN 表（只读）。
// 所有查询都直接读映射区，堆里只有一个 20 来字节的描述符。
type FRNFile struct {
	mp   *mapping
	data []byte
	n    uint32
}

// LoadFRNFile 映射一份 FRN 表；文件不存在/版本不对时返回错误。
func LoadFRNFile(path string) (*FRNFile, error) {
	mp, err := mapIndexFile(path)
	if err != nil {
		return nil, err
	}
	if len(mp.data) < frnHeaderLen || string(mp.data[:4]) != frnFileMagic {
		mp.close()
		return nil, fmt.Errorf("不是 Baozi FRN 表")
	}
	if v := binary.LittleEndian.Uint32(mp.data[4:]); v != frnFileVer {
		mp.close()
		return nil, fmt.Errorf("FRN 表版本 %d 与当前 %d 不匹配", v, frnFileVer)
	}
	n := binary.LittleEndian.Uint32(mp.data[8:])
	if frnHeaderLen+int(n)*frnRecLen > len(mp.data) {
		mp.close()
		return nil, fmt.Errorf("FRN 表头损坏（长度不自洽）")
	}
	return &FRNFile{mp: mp, data: mp.data, n: n}, nil
}

// Len 返回表里的条目数。
func (f *FRNFile) Len() int {
	if f == nil {
		return 0
	}
	return int(f.n)
}

// KeyAt 返回第 i 条记录的文件号（诊断用；记录按文件号升序）。
func (f *FRNFile) KeyAt(i int) uint64 {
	if f == nil || i < 0 || i >= int(f.n) {
		return 0
	}
	return f.keyAt(i)
}

// ValAt 返回第 i 条记录的下标（诊断用）。
func (f *FRNFile) ValAt(i int) int32 {
	if f == nil || i < 0 || i >= int(f.n) {
		return -1
	}
	return f.valAt(i)
}

func (f *FRNFile) keyAt(i int) uint64 {
	off := frnHeaderLen + i*frnRecLen
	return binary.LittleEndian.Uint64(f.data[off:])
}

func (f *FRNFile) valAt(i int) int32 {
	off := frnHeaderLen + i*frnRecLen + 8
	return int32(binary.LittleEndian.Uint32(f.data[off:]))
}

// Get 二分查找 FRN 对应的条目下标。
func (f *FRNFile) Get(frn uint64) (int32, bool) {
	if f == nil || frn == 0 || f.n == 0 {
		return -1, false
	}
	lo, hi := 0, int(f.n)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if f.keyAt(mid) < frn {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo < int(f.n) && f.keyAt(lo) == frn {
		return f.valAt(lo), true
	}
	return -1, false
}

// Close 解开映射。换代次时由 Mutable.Close 调用。
func (f *FRNFile) Close() {
	if f == nil || f.mp == nil {
		return
	}
	f.mp.close()
	f.mp = nil
	f.data = nil
	f.n = 0
}
