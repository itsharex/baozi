package index

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"baozi/internal/config"
)

func TestBuildAndPath(t *testing.T) {
	root := t.TempDir()
	mustWrite := func(rel, content string) {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(`docs\report.txt`, "a")
	mustWrite(`docs\2026\q1.xlsx`, "b")
	mustWrite(`node_modules\junk.js`, "c") // 应被排除

	cfg := config.Index{
		Roots:    []string{root},
		Excludes: []string{"node_modules"},
	}
	tab, err := Build(context.Background(), cfg, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	find := func(name string) int32 {
		for i := 0; i < tab.Len(); i++ {
			if tab.Name(i) == name {
				return int32(i)
			}
		}
		return -1
	}
	if find("node_modules") >= 0 {
		t.Fatal("排除规则没生效")
	}
	q1 := find("q1.xlsx")
	if q1 < 0 {
		t.Fatal("没找到 q1.xlsx")
	}
	want := filepath.Join(root, `docs\2026\q1.xlsx`)
	if got := tab.PathOf(q1); got != want {
		t.Fatalf("路径还原错误:\n got=%s\nwant=%s", got, want)
	}
	if dir := tab.Dir(q1); dir != filepath.Join(root, `docs\2026`) {
		t.Fatalf("父目录错误: %s", dir)
	}
}

func TestSaveLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.bin")
	tab := &Table{Nodes: []Node{
		{Parent: -1, Kind: KindDir, NameOff: 0, NameLen: 3},
		{Parent: 0, Kind: KindFile, SizeKB: 1, NameOff: 3, NameLen: 5},
		{Parent: 0, Kind: KindDir, NameOff: 8, NameLen: 3},
		{Parent: 2, Kind: KindFile, SizeKB: 2, NameOff: 11, NameLen: 5},
	}, Names: []byte(`D:\a.txtsubb.txt`)}
	if err := Save(path, tab, []string{`D:\`}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, roots, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Len() != tab.Len() {
		t.Fatalf("条目数 %d != %d", got.Len(), tab.Len())
	}
	if len(roots) != 1 || roots[0] != `D:\` {
		t.Fatalf("根目录未随索引保存: %v", roots)
	}
	if p := got.PathOf(3); p != `D:\sub\b.txt` {
		t.Fatalf("还原路径 = %s", p)
	}
	if name := got.Name(1); name != "a.txt" {
		t.Fatalf("名字还原 = %q", name)
	}
	if sz := got.Nodes[1].Size(); sz != 1024 {
		t.Fatalf("大小还原 = %d", sz)
	}
	count, _, err := Info(path)
	if err != nil || count != tab.Len() {
		t.Fatalf("Info: count=%d err=%v", count, err)
	}
}

// 落盘再读回来之后，nodes 段整段映射进 raw，不能有半点偏移——
// 这条测试专门盯"结构宽度/对齐"改动（现在 Node 固定 20 字节）。
func TestLoadKeepsNodeFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.bin")
	tab := &Table{Nodes: make([]Node, 3), Names: []byte("abcdefgh")}
	tab.Nodes[0] = Node{Parent: -1, NameOff: 0, NameLen: 3, SizeKB: 7, ModSec: 12345, Kind: KindDir}
	tab.Nodes[1] = Node{Parent: 0, NameOff: 3, NameLen: 2, SizeKB: 0, ModSec: 0, Kind: KindFile}
	tab.Nodes[2] = Node{Parent: 0, NameOff: 5, NameLen: 3, SizeKB: 65535, ModSec: -1, Kind: KindFile}
	if err := Save(path, tab, nil); err != nil {
		t.Fatal(err)
	}
	got, _, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := range tab.Nodes {
		if got.Nodes[i] != tab.Nodes[i] {
			t.Fatalf("第 %d 条读回来变了:\n got=%+v\nwant=%+v", i, got.Nodes[i], tab.Nodes[i])
		}
	}
	if got.Nodes[1].Kind != KindFile || got.Nodes[0].Kind != KindDir {
		t.Fatal("Kind 读回来不对")
	}
}

func TestDedupeRoots(t *testing.T) {
	got := dedupeRoots([]string{`D:\work`, `D:\work\baozi`, `D:\work`, `E:\`})
	if len(got) != 2 {
		t.Fatalf("期望 2 个根，得到 %v", got)
	}
}

// 中文名要在建索引时就把拼音首字母串算好，落盘后还得带着，
// 否则每次敲拼音都要重算一遍全表。
func TestPinyinSurvivesStore(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "月度报表.xlsx"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "plain.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	tab, err := Build(context.Background(), config.Index{Roots: []string{root}}, nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	cnIdx, asciiIdx := -1, -1
	for i := 0; i < tab.Len(); i++ {
		switch tab.Name(i) {
		case "月度报表.xlsx":
			cnIdx = i
		case "plain.txt":
			asciiIdx = i
		}
	}
	if cnIdx < 0 || asciiIdx < 0 {
		t.Fatal("没建出预期的节点")
	}
	if py := tab.PyOf(cnIdx); py != "ydbbxlsx" {
		t.Fatalf("中文名拼音 = %q，期望 ydbbxlsx", py)
	}
	if py := tab.PyOf(asciiIdx); py != "" {
		t.Fatalf("纯 ASCII 名不该存拼音，得到 %q", py)
	}

	path := filepath.Join(t.TempDir(), "index.bin")
	if err := Save(path, tab, []string{root}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, _, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	foundCN := false
	for i := 0; i < got.Len(); i++ {
		if got.Name(i) == "月度报表.xlsx" {
			foundCN = true
			if py := got.PyOf(i); py != "ydbbxlsx" {
				t.Fatalf("落盘后拼音丢了: %q", py)
			}
		}
	}
	if !foundCN {
		t.Fatal("落盘后找不到中文节点")
	}
}

// 预筛要大小写不敏感，且汉字不能被折坏。
func TestHitsFoldAndChinese(t *testing.T) {
	tab := &Table{
		Nodes: []Node{
			{Parent: -1, Kind: KindFile, NameOff: 0, NameLen: 18},
			{Parent: -1, Kind: KindFile, NameOff: 18, NameLen: 17},
		},
		Names: []byte(`SnowShot_Video.mp4月度报表.xlsx`),
	}
	if !tab.Hits(0, []string{"snowshot"}) {
		t.Fatal("大小写不敏感的预筛没命中 SnowShot_Video.mp4")
	}
	if !tab.Hits(0, []string{"video"}) {
		t.Fatal("预筛第二个词没命中")
	}
	if !tab.Hits(0, []string{"snowshot", "video"}) {
		t.Fatal("多词预筛没命中")
	}
	if tab.Hits(0, []string{"snowshot", "xlsx"}) {
		t.Fatal("不该命中")
	}
	if !tab.Hits(1, []string{"报表"}) {
		t.Fatal("汉字的预筛没命中")
	}
}
