package index

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFRNFileRoundTrip 检查"堆表写盘 → 映射回来 → 二分查找"和原表完全一致。
// 这是服务端增量能不能跑的前提：查不到 FRN，所有 USN 记录都会被当成"不认识"丢掉。
func TestFRNFileRoundTrip(t *testing.T) {
	m := NewFRNMap(1 << 12)
	const n = 5000
	for i := 0; i < n; i++ {
		m.Set(uint64(i)*3+1, int32(i))
	}
	// 一个不存在的 FRN 必须查不到
	if _, ok := m.Get(999999); ok {
		t.Fatalf("堆表里不该有 999999")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "index.frn")
	got, err := SaveFRNFile(path, m)
	if err != nil {
		t.Fatalf("写盘: %v", err)
	}
	if got != path {
		t.Fatalf("返回路径 %q，期望 %q", got, path)
	}
	ff, err := LoadFRNFile(got)
	if err != nil {
		t.Fatalf("映射: %v", err)
	}
	defer ff.Close()
	if ff.Len() != n {
		t.Fatalf("映射后 %d 条，期望 %d", ff.Len(), n)
	}
	for i := 0; i < n; i++ {
		frn := uint64(i)*3 + 1
		idx, ok := ff.Get(frn)
		if !ok {
			t.Fatalf("FRN %d 查不到", frn)
		}
		if idx != int32(i) {
			t.Fatalf("FRN %d 下标 %d，期望 %d", frn, idx, i)
		}
	}
	for _, frn := range []uint64{0, 2, 3, 999997, 1 << 40} {
		if _, ok := ff.Get(frn); ok {
			t.Fatalf("FRN %d 不该被查到", frn)
		}
	}
	// 空表必须报错（调用方据此退回堆表）
	if _, err := SaveFRNFile(filepath.Join(dir, "empty.frn"), NewFRNMap(4)); err == nil {
		t.Fatalf("空表应当报错")
	}
	// 清理（Windows 下映射未关时删不掉，这里 Close 之后就该能删）
	if err := os.Remove(path); err == nil {
		_ = os.Remove(path + ".tmp")
	}
}

// TestMutableBaseLookup 检查基座（映射）+ 增量（堆）两层的优先级：
// 同名 FRN 在增量里被重指到新下标后，必须返回新的那个。
func TestMutableBaseLookup(t *testing.T) {
	m := NewFRNMap(1 << 10)
	m.Set(101, 7)   // 基座：FRN 101 → 下标 7
	m.Set(102, 8)   // 基座：FRN 102 → 下标 8
	dir := t.TempDir()
	path, err := SaveFRNFile(filepath.Join(dir, "index.frn"), m)
	if err != nil {
		t.Fatalf("写盘: %v", err)
	}
	ff, err := LoadFRNFile(path)
	if err != nil {
		t.Fatalf("映射: %v", err)
	}
	defer ff.Close()

	mut := NewMutableBase(&Table{}, ff)
	if idx, ok := mut.lookupFRN(101); !ok || idx != 7 {
		t.Fatalf("基座查 FRN 101 得 %d,%v，期望 7", idx, ok)
	}
	mut.setFRN(101, 99) // 改名：同一个 FRN 指向新追加的下标
	if idx, ok := mut.lookupFRN(101); !ok || idx != 99 {
		t.Fatalf("增量该赢：得 %d,%v，期望 99", idx, ok)
	}
	if idx, ok := mut.lookupFRN(102); !ok || idx != 8 {
		t.Fatalf("基座查 FRN 102 得 %d,%v，期望 8", idx, ok)
	}
	if _, ok := mut.lookupFRN(555); ok {
		t.Fatalf("FRN 555 不该被查到")
	}
	if mut.FRNBaseLen() != 2 || mut.FRNOvLen() != 1 {
		t.Fatalf("基座 %d 条 / 增量 %d 条，期望 2 / 1", mut.FRNBaseLen(), mut.FRNOvLen())
	}
}
