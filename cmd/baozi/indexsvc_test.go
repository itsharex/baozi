package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"baozi/internal/index"
)

// 造一份小快照 + 一份增量，验证主进程侧的合并：叠完之后新条目能被还原出路径，
// 删掉的条目会被墓碑挡掉（搜索侧就是靠 Tab.Dead 过滤的）。
func TestRefreshIndexAppliesDelta(t *testing.T) {
	dir := t.TempDir()

	// 快照：C:\ → docs → a.txt
	tb := &index.Table{Names: make([]byte, 0, 256), Py: make(map[int32]uint64, 4)}
	m := index.NewMutable(tb, index.NewFRNMap(8))
	root := m.Add(`C:\`, -1, index.KindDir, 0, 0, 5)
	docs := m.Add("docs", root, index.KindDir, 0, 0, 100)
	m.Add("a.txt", docs, index.KindFile, 0, 0, 101)
	if err := index.Save(filepath.Join(dir, "index.bin"), tb, []string{`C:\`}); err != nil {
		t.Fatal(err)
	}

	// 增量：基于这一代（base 随便给个非零值），加一个 b.txt、删掉 a.txt
	const base uint64 = 777
	delta := &index.Delta{Base: base, Seq: 1, Ops: []index.DeltaOp{
		{Op: index.OpAdd, Idx: 3, PIdx: docs, Name: "b.txt", Kind: uint8(index.KindFile)},
		{Op: index.OpDelete, Idx: 2},
	}}
	if err := index.WriteDelta(filepath.Join(dir, "index.delta"), delta); err != nil {
		t.Fatal(err)
	}
	// 服务活着（心跳文件够新），否则 refreshIndex 直接跳过
	writeJSON(t, filepath.Join(dir, "index.progress.json"), map[string]any{
		"volumes":   []any{},
		"total":     3,
		"heartbeat": time.Now(),
		"base":      base,
	})
	if err := os.WriteFile(filepath.Join(dir, "index.heartbeat"), []byte(time.Now().Format(time.RFC3339Nano)), 0o644); err != nil {
		t.Fatal(err)
	}

	a := &application{dataDir: dir}
	tab, _, err := index.Load(filepath.Join(dir, "index.bin"))
	if err != nil {
		t.Fatal(err)
	}
	a.idxReplace(tab)
	if info, e := os.Stat(filepath.Join(dir, "index.bin")); e == nil {
		a.idxMtime = info.ModTime()
	}
	a.idxBase = readStateBase(dir)
	a.idxSeq = 0
	a.idxOps = 0
	if a.idxBase != base {
		t.Fatalf("代次没读出来: %d", a.idxBase)
	}

	a.refreshIndex(true)

	got := a.cat.Load()
	if got == nil {
		t.Fatal("合并后没有表")
	}
	if got.Len() != 4 {
		t.Fatalf("合并后条目数应为 4，实际 %d", got.Len())
	}
	if p := got.PathOf(3); p != `C:\docs\b.txt` {
		t.Fatalf("新增条目路径不对: %s", p)
	}
	if !got.Dead(2) {
		t.Fatal("增量里删掉的条目没被标成墓碑")
	}
	if got.Dead(3) {
		t.Fatal("新增条目被判成了失效")
	}

	// 再来一次不应该重复追加（seq 没变）
	a.refreshIndex(true)
	if again := a.cat.Load(); again.Len() != 4 {
		t.Fatalf("重复刷新又追加了: %d", again.Len())
	}
}

// 服务每次 flush 都把这一代**累积的** ops 整份重写，只把批号 seq 递增。
// 主进程必须按"已叠了多少条"续着取，不能拿 seq 当下标（差一点就会把叠过的
// 再叠一遍，而 ApplyOps 的下标校验会整批拒绝——表现是索引永远追不上）。
func TestRefreshIndexFollowsBatches(t *testing.T) {
	dir := t.TempDir()
	tb := &index.Table{Names: make([]byte, 0, 256), Py: make(map[int32]uint64, 4)}
	m := index.NewMutable(tb, index.NewFRNMap(8))
	root := m.Add(`C:\`, -1, index.KindDir, 0, 0, 5)
	m.Add("a.txt", root, index.KindFile, 0, 0, 101)
	if err := index.Save(filepath.Join(dir, "index.bin"), tb, []string{`C:\`}); err != nil {
		t.Fatal(err)
	}
	const base uint64 = 4242
	writeJSON(t, filepath.Join(dir, "index.progress.json"), map[string]any{"base": base})
	if err := os.WriteFile(filepath.Join(dir, "index.heartbeat"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	a := &application{dataDir: dir}
	tab, _, err := index.Load(filepath.Join(dir, "index.bin"))
	if err != nil {
		t.Fatal(err)
	}
	a.idxReplace(tab)
	if info, e := os.Stat(filepath.Join(dir, "index.bin")); e == nil {
		a.idxMtime = info.ModTime()
	}
	a.idxBase = readStateBase(dir)

	// 第一批：三条
	first := &index.Delta{Base: base, Seq: 1, Ops: []index.DeltaOp{
		{Op: index.OpAdd, Idx: 2, PIdx: root, Name: "b.txt", Kind: uint8(index.KindFile)},
		{Op: index.OpAdd, Idx: 3, PIdx: root, Name: "c.txt", Kind: uint8(index.KindFile)},
		{Op: index.OpAdd, Idx: 4, PIdx: root, Name: "d.txt", Kind: uint8(index.KindFile)},
	}}
	if err := index.WriteDelta(filepath.Join(dir, "index.delta"), first); err != nil {
		t.Fatal(err)
	}
	a.refreshIndex(true)
	if got := a.cat.Load().Len(); got != 5 {
		t.Fatalf("第一批后应为 5 条，实际 %d", got)
	}

	// 第二批：服务把累积的五条整份重写，只在后面多了两条
	second := &index.Delta{Base: base, Seq: 2, Ops: []index.DeltaOp{
		first.Ops[0], first.Ops[1], first.Ops[2],
		{Op: index.OpAdd, Idx: 5, PIdx: root, Name: "e.txt", Kind: uint8(index.KindFile)},
		{Op: index.OpAdd, Idx: 6, PIdx: root, Name: "f.txt", Kind: uint8(index.KindFile)},
	}}
	if err := index.WriteDelta(filepath.Join(dir, "index.delta"), second); err != nil {
		t.Fatal(err)
	}
	a.refreshIndex(true)

	got := a.cat.Load()
	if got.Len() != 7 {
		t.Fatalf("第二批后应为 7 条（只该追加 e/f），实际 %d", got.Len())
	}
	if p := got.PathOf(6); p != `C:\f.txt` {
		t.Fatalf("第二批的条目路径不对: %s", p)
	}
	if a.idxOps != 5 {
		t.Fatalf("已叠条数应为 5，实际 %d", a.idxOps)
	}
}

// 换代（base 不一致）时不能往上叠——否则下标全错。
func TestRefreshIndexSkipsStaleDelta(t *testing.T) {
	dir := t.TempDir()
	tb := &index.Table{Names: make([]byte, 0, 128), Py: make(map[int32]uint64, 2)}
	m := index.NewMutable(tb, index.NewFRNMap(4))
	m.Add(`C:\`, -1, index.KindDir, 0, 0, 5)
	if err := index.Save(filepath.Join(dir, "index.bin"), tb, []string{`C:\`}); err != nil {
		t.Fatal(err)
	}
	// delta 属于另一代
	delta := &index.Delta{Base: 999, Seq: 1, Ops: []index.DeltaOp{
		{Op: index.OpAdd, Idx: 1, PIdx: 0, Name: "x.txt", Kind: uint8(index.KindFile)},
	}}
	if err := index.WriteDelta(filepath.Join(dir, "index.delta"), delta); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(dir, "index.progress.json"), map[string]any{"base": uint64(1000)})
	if err := os.WriteFile(filepath.Join(dir, "index.heartbeat"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	a := &application{dataDir: dir}
	tab, _, err := index.Load(filepath.Join(dir, "index.bin"))
	if err != nil {
		t.Fatal(err)
	}
	a.idxReplace(tab)
	if info, e := os.Stat(filepath.Join(dir, "index.bin")); e == nil {
		a.idxMtime = info.ModTime()
	}
	a.idxBase = readStateBase(dir)
	a.refreshIndex(true)

	if got := a.cat.Load(); got.Len() != 1 {
		t.Fatalf("base 不匹配时不该应用增量，条目数 %d", got.Len())
	}
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
