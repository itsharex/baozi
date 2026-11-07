package history

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 首页卡片的删除/取消固定：删了要落盘，且 maxCount 要跟着回退。
func TestRemoveAndUnpin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.json")
	s, err := Open(path, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		s.Touch(`D:\hot.txt`)
	}
	s.Touch(`D:\cold.txt`)

	if !s.Remove(`D:\hot.txt`) {
		t.Fatal("删除应返回 true")
	}
	if s.Count(`D:\hot.txt`) != 0 {
		t.Fatal("删了之后计数应归零")
	}
	if !s.Remove(`D:\cold.txt`) {
		t.Fatal("冷门项也应能删")
	}
	if s.Remove(`D:\cold.txt`) {
		t.Fatal("已删掉的条目再删应返回 false")
	}

	// 重开一份，确认真的落盘了。
	reopened, err := Open(path, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Len() != 0 {
		t.Fatalf("重开后还剩 %d 条，应为 0", reopened.Len())
	}

	// 取消固定：条目留着，只是不再进「已固定」。
	s.Unpin(`D:\cold.txt`) // 不在历史里
	if s.Unpin(`D:\cold.txt`) {
		t.Fatal("不在历史里的项取消固定应返回 false")
	}
}

func TestUnpinKeepsEntry(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "history.json"), 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	s.Touch(`D:\a.txt`)
	if pinned, ok := s.TogglePin(`D:\a.txt`); !ok || !pinned {
		t.Fatal("应当固定成功")
	}
	if got := len(s.Pinned(10)); got != 1 {
		t.Fatalf("固定后 Pinned() 应有 1 条，实际 %d", got)
	}
	if !s.Unpin(`D:\a.txt`) {
		t.Fatal("取消固定应返回 true")
	}
	if s.IsPinned(`D:\a.txt`) {
		t.Fatal("取消后仍是固定状态")
	}
	if s.Count(`D:\a.txt`) != 1 {
		t.Fatal("取消固定不该把条目从历史里删掉")
	}
	if got := len(s.Pinned(10)); got != 0 {
		t.Fatalf("取消后 Pinned() 应为空，实际 %d", got)
	}
}

func TestTouchAndScore(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "history.json"), 5, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		s.Touch(`D:\docs\hot.docx`)
	}
	s.Touch(`D:\docs\cold.docx`)

	hot := s.Score(`D:\docs\hot.docx`)
	cold := s.Score(`D:\docs\cold.docx`)
	if hot <= cold {
		t.Fatalf("常用项得分应更高: hot=%.3f cold=%.3f", hot, cold)
	}
	if s.Score(`D:\never.docx`) != 0 {
		t.Fatal("没用过的项得分应为 0")
	}
	if hot <= 0.5 || hot > 1.0 {
		t.Fatalf("得分应在 0.5..1 之间（含上限 1.0），得到 %.3f", hot)
	}
}

func TestRecentOrderAndPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.json")
	s, err := Open(path, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	s.Touch(`D:\a.txt`)
	s.Touch(`D:\b.txt`)
	recent := s.Recent(2)
	if len(recent) != 2 || recent[0].Path != `D:\b.txt` {
		t.Fatalf("最近列表顺序不对: %+v", recent)
	}
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	reloaded, err := Open(path, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.Count(`D:\a.txt`); got != 1 {
		t.Fatalf("重载后计数 = %d", got)
	}
}

func TestTrim(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "history.json"), 2, 0)
	for _, p := range []string{`D:\1`, `D:\2`, `D:\3`} {
		s.Touch(p)
	}
	if s.Len() > 2 {
		t.Fatalf("条目数应被裁剪到 2，实际 %d", s.Len())
	}
}

// 应用历史的 Path 是 shell:AppsFolder\<AUMID>，重启后必须靠记录里的
// Name/Kind 还原，否则会显示成怪名字 + 通用图标。
func TestMetadataRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.json")
	s, _ := Open(path, 20, 0)
	s.TouchEntry(Entry{
		Path: `shell:AppsFolder\Microsoft.WindowsCalculator_8wekyb3d8bbwe!App`,
		Name: "计算器", Kind: KindApp, Ext: ".exe", Hint: "应用",
	})
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reloaded, err := Open(path, 20, 0)
	if err != nil {
		t.Fatal(err)
	}
	recent := reloaded.Recent(1)
	if len(recent) != 1 {
		t.Fatalf("应重载 1 条，实际 %d", len(recent))
	}
	if recent[0].Name != "计算器" || recent[0].Kind != KindApp || recent[0].Hint != "应用" {
		t.Fatalf("元数据丢失: %+v", recent[0])
	}
}

// 进程被强杀时 shutdown 不会执行，所以每次使用后必须自己防抖落盘。
func TestAutoSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.json")
	s, _ := Open(path, 10, 50*time.Millisecond)
	s.Touch(`D:\autosave.txt`)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			reloaded, err := Open(path, 10, 0)
			if err == nil && reloaded.Count(`D:\autosave.txt`) == 1 {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("50ms 内应已自动落盘")
}

// 卸载前要 Close：否则最后一笔 Touch 的防抖定时器会在数据目录删掉之后
// 又触发一次 Save，把 history.json 连目录一起重建出来，看起来像没卸干净。
func TestCloseStopsAutosave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.json")
	s, _ := Open(path, 10, 50*time.Millisecond)
	s.Touch(`D:\uninstall.txt`)
	s.Close()
	// 模拟卸载：目录整个删掉
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond) // 超过防抖延迟
	if _, err := os.Stat(path); err == nil {
		t.Fatal("Close 之后不该再落盘（文件不该被重建）")
	}
	if err := s.Save(); err != nil {
		t.Fatalf("Close 之后的 Save 应当是空操作，返回 %v", err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("Close 之后的 Save 不该写文件")
	}
}

// 固定状态要跨进程存活：它是用户显式动作，重启后首页仍该有「已固定」。
func TestPinSurvivesReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.json")
	s, err := Open(path, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	s.TouchEntry(Entry{Path: `C:\Windows\notepad.exe`, Name: "记事本", Kind: KindApp, Ext: ".exe", Hint: "应用"})
	s.TouchEntry(Entry{Path: `C:\Windows\explorer.exe`, Name: "资源管理器", Kind: KindApp, Ext: ".exe", Hint: "应用"})

	// 索引不支持：直接调 TogglePin 时传入的是 map 里存在的路径，这里改用已有条目的路径。
	if _, ok := s.TogglePin(`C:\Windows\explorer.exe`); !ok {
		t.Fatal("TogglePin 应成功")
	}
	if !s.IsPinned(`C:\Windows\explorer.exe`) {
		t.Fatal("TogglePin 后应为已固定")
	}

	reloaded, err := Open(path, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.IsPinned(`C:\Windows\explorer.exe`) {
		t.Fatal("重新打开后固定状态应保留")
	}
	pinned := reloaded.Pinned(0)
	if len(pinned) != 1 || pinned[0].Name != "资源管理器" {
		t.Fatalf("Pinned 应只有资源管理器一条，实际 %+v", pinned)
	}

	// trim 不能把固定项删掉：上限 2，固定项次数最低也要留，淘汰次数高的普通项之外的。
	s2, err := Open(filepath.Join(dir, "h2.json"), 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	s2.TouchEntry(Entry{Path: "keep", Name: "固定的", Kind: KindApp})
	for i := 0; i < 3; i++ {
		s2.TouchEntry(Entry{Path: "drop", Name: "普通的", Kind: KindApp})
	}
	if _, ok := s2.TogglePin("keep"); !ok {
		t.Fatal("固定 keep 失败")
	}
	// 再加一条，条目数 3 > 上限 2，触发 trim；keep 次数最低，但被固定。
	s2.TouchEntry(Entry{Path: "third", Name: "第三条", Kind: KindApp})
	if s2.Count("keep") == 0 {
		t.Fatal("trim 之后固定项不该被删掉")
	}
	if s2.Count("drop") == 0 {
		t.Fatal("次数最高的普通项应留下")
	}
	if s2.Count("third") != 0 {
		t.Fatal("次数最低的普通项应被淘汰")
	}
}
