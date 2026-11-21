package win

import (
	"encoding/json"
	"os"
	"syscall"
	"testing"
)

// findArkeWindow 枚举顶层窗口找 ArkeWindow。
// 不用 FindWindowW：Arke 的窗口类没有注册成全局类，按类名跨进程查不到
// （实测返回 0，但 GetClassName 明明就是 ArkeWindow）。
func findArkeWindow() uintptr {
	var found uintptr
	cb := syscall.NewCallback(func(h uintptr, _ uintptr) uintptr {
		if IsArke(h) {
			found = h
			return 0
		}
		return 1
	})
	procEnumWindowsForTest.Call(cb, 0)
	return found
}

// TestArkeLive 是手测：本机开着 Arke 时设 BAOZI_ARKE_LIVE=1 跑，
// 验证"按类名认窗口 → 起 arke.exe --api 问 state → 拿到活动窗格目录"整条链路。
// 默认跳过，CI 上没有 Arke。
func TestArkeLive(t *testing.T) {
	if os.Getenv("BAOZI_ARKE_LIVE") == "" {
		t.Skip("设 BAOZI_ARKE_LIVE=1 且本机开着 Arke 时才跑")
	}
	hwnd := findArkeWindow()
	if hwnd == 0 {
		t.Skip("没找到 ArkeWindow")
	}
	if !IsArke(hwnd) {
		t.Fatalf("枚举到的 %#x 不是 IsArke", hwnd)
	}
	dir := ArkeFolderFresh(hwnd)
	if dir == "" {
		t.Fatal("ArkeFolderFresh 没读到目录")
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		t.Fatalf("读到的不是目录: %q (%v)", dir, err)
	}
	t.Logf("Arke 活动窗格: %s", dir)
}

// arkeStateJSON 用结构体序列化，省得手写反斜杠路径。
func arkeStateJSON(t *testing.T, st arkeState) []byte {
	t.Helper()
	data, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}

func folderPane(active bool, path string) arkePane {
	return arkePane{Active: active, Location: arkeLocation{Kind: "folder", Path: path}}
}

// 双栏：活动窗格在第二栏时取第二栏的目录，不能取第一栏。
func TestArkeFolderActivePane(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	st := arkeState{V: 1, Windows: []arkeWindow{{
		HWND: "0x1234", Active: true,
		Tabs: []arkeTab{{Active: true, Panes: []arkePane{
			folderPane(false, dirA),
			folderPane(true, dirB),
		}}},
	}}}
	if got := arkeFolderFromJSON(arkeStateJSON(t, st), 0); got != dirB {
		t.Fatalf("活动窗格目录: 期望 %q，实际 %q", dirB, got)
	}
}

// 活动标签不是第一个：取活动标签里的活动窗格。
func TestArkeFolderActiveTab(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	st := arkeState{V: 1, Windows: []arkeWindow{{
		HWND: "0x1234", Active: true,
		Tabs: []arkeTab{
			{Active: false, Panes: []arkePane{folderPane(true, dirA)}},
			{Active: true, Panes: []arkePane{folderPane(true, dirB)}},
		},
	}}}
	if got := arkeFolderFromJSON(arkeStateJSON(t, st), 0); got != dirB {
		t.Fatalf("活动标签目录: 期望 %q，实际 %q", dirB, got)
	}
}

// 有多个窗口时按 hwnd 找调用方那个窗口，而不是永远拿最前面的活动窗口。
func TestArkeFolderMatchWindow(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	st := arkeState{V: 1, Windows: []arkeWindow{
		{HWND: "0x2A", Active: true, Tabs: []arkeTab{{Active: true, Panes: []arkePane{folderPane(true, dirA)}}}},
		{HWND: "0x2B", Active: false, Tabs: []arkeTab{{Active: true, Panes: []arkePane{folderPane(true, dirB)}}}},
	}}
	if got := arkeFolderFromJSON(arkeStateJSON(t, st), 0x2B); got != dirB {
		t.Fatalf("按 hwnd 选窗口: 期望 %q，实际 %q", dirB, got)
	}
}

// 活动窗格不是 folder（home / search / 回收站……）就不猜目录，返回空。
// 接口文档建议：只有 kind=folder 才有能送进对话框的路径。
func TestArkeFolderNonFolderKind(t *testing.T) {
	st := arkeState{V: 1, Windows: []arkeWindow{{
		HWND: "0x1", Active: true,
		Tabs: []arkeTab{{Active: true, Panes: []arkePane{{
			Active:   true,
			Location: arkeLocation{Kind: "home"},
		}}}},
	}}}
	if got := arkeFolderFromJSON(arkeStateJSON(t, st), 0); got != "" {
		t.Fatalf("home 不应当给出目录，实际 %q", got)
	}
}

// 路径必须真实存在：接口若回了已删除目录 / 不存在的路径，不能拿去导航对话框。
func TestArkeFolderMissingDir(t *testing.T) {
	dir := t.TempDir()
	st := arkeState{V: 1, Windows: []arkeWindow{{
		HWND: "0x1", Active: true,
		Tabs: []arkeTab{{Active: true, Panes: []arkePane{folderPane(true, dir+`\nope`)}}},
	}}}
	if got := arkeFolderFromJSON(arkeStateJSON(t, st), 0); got != "" {
		t.Fatalf("不存在的目录不应当被认下，实际 %q", got)
	}
}

// 空回复（没开 Arke）和版本不对都不能崩，也不能瞎认一个路径。
func TestArkeFolderEmptyOrWrongVersion(t *testing.T) {
	for _, data := range [][]byte{[]byte(`{"v":1,"windows":[]}`), []byte(`{"v":2,"windows":[]}`), []byte(`not json`)} {
		if got := arkeFolderFromJSON(data, 0); got != "" {
			t.Fatalf("%s 应当返回空，实际 %q", data, got)
		}
	}
}

// \\?\ 前缀（长路径）要能还原成 isDir 认得的普通路径。
func TestArkeFolderLongPathPrefix(t *testing.T) {
	dir := t.TempDir()
	st := arkeState{V: 1, Windows: []arkeWindow{{
		HWND: "0x1", Active: true,
		Tabs: []arkeTab{{Active: true, Panes: []arkePane{folderPane(true, `\\?\`+dir)}}},
	}}}
	if got := arkeFolderFromJSON(arkeStateJSON(t, st), 0); got != dir {
		t.Fatalf("长路径前缀: 期望 %q，实际 %q", dir, got)
	}
}
