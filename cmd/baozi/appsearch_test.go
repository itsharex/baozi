package main

import (
	"testing"

	"baozi/internal/pinyin"
	"baozi/internal/search"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
)

// mkApp 按真实 ID 造一条应用条目（name/alias 与运行时同一套算法）。
func mkApp(name, id string) appEntry {
	return appEntry{
		InstalledApp: mygo.InstalledApp{Name: name, ID: id, Path: `shell:AppsFolder\` + id},
		initials:     pinyin.Initials(name),
		full:         pinyin.Full(name),
		alias:        search.AppAlias(id),
	}
}

// 回归：应用别名不再吃掉散落子序列的假命中，否则应用段硬置顶会让
// "arke" 先把夸克网盘（quark cloud drive）顶到最前、"kk" 顶出 WorkBuddy。
func TestAppAliasNoFalseTop(t *testing.T) {
	a := testApp(t)
	apps := []appEntry{
		mkApp("夸克网盘", `D:\Program Files\QuarkCloudDrive\quark_cloud_drive.exe`),
		mkApp("WorkBuddy", `WorkBuddy.WorkBuddy`),
		mkApp("Arke", `C:\Program Files\Arke\arke.exe`),
	}
	a.apps.Store(&apps)

	got := a.searchAll("arke")
	if len(got) == 0 {
		t.Fatal("搜 arke 什么都没匹配到")
	}
	if got[0].Name != "Arke" {
		t.Fatalf("搜 arke 第一条应是 Arke，实际 %q（共 %d 条）", got[0].Name, len(got))
	}
	for _, r := range got {
		if r.Name == "夸克网盘" {
			t.Fatalf("夸克网盘不该被 arke 命中（别名准确度 %.2f）", r.Accuracy)
		}
	}

	for _, r := range a.searchAll("kk") {
		if r.Name == "WorkBuddy" {
			t.Fatalf("WorkBuddy 不该被 kk 命中（别名准确度 %.2f）", r.Accuracy)
		}
	}
	for _, r := range a.searchAll("kk") {
		if r.Name == "夸克网盘" && r.Accuracy < 0.88 {
			t.Fatalf("夸克网盘应由拼音首字母 kk 命中，准确度 %.2f", r.Accuracy)
		}
	}
}

// 回归：搜 cal 时，nvm / Python 这些装在 AppData\Local 下的应用不该冒出来。
// 它们此前是靠路径里的 "Local"（Lo-cal）命中的，且带着应用类型权重排到最前。
func TestAppSearchIgnoresPathNoise(t *testing.T) {
	a := testApp(t)
	apps := []appEntry{
		mkApp("nvm", `C:\Users\Administrator\AppData\Local\nvm\nvm.exe`),
		mkApp("Python 3.11", `C:\Users\Administrator\AppData\Local\Python\pythoncore-3.11-64\python.exe`),
		mkApp("Python 3.12", `C:\Users\Administrator\AppData\Local\Python\pythoncore-3.12-64\python.exe`),
		mkApp("计算器", `Microsoft.WindowsCalculator_8wekyb3d8bbwe!App`),
		mkApp("豆包工作", `Doubao.Office_1.0.0.0_x64!App`),
	}
	a.apps.Store(&apps)

	got := a.searchAll("cal")
	if len(got) == 0 {
		t.Fatal("搜 cal 什么都没匹配到")
	}
	first := got[0]
	if first.Kind != search.KindApp || first.Name != "计算器" {
		t.Fatalf("第一条应是「计算器」，实际 %q（kind=%v，共 %d 条）", first.Name, first.Kind, len(got))
	}
	for _, r := range got {
		if r.Name == "nvm" || r.Name == "Python 3.11" || r.Name == "Python 3.12" {
			t.Fatalf("%q 不该被 cal 命中（分数 %.2f）", r.Name, r.Accuracy)
		}
	}

	// 反过来：搜 nvm / python 仍然精准命中。
	if first := a.searchAll("nvm")[0]; first.Name != "nvm" || first.Accuracy < 0.95 {
		t.Fatalf("搜 nvm 第一条应是 nvm 且高分，实际 %q %.2f", first.Name, first.Accuracy)
	}
	if first := a.searchAll("python")[0]; first.Accuracy < 0.95 {
		t.Fatalf("搜 python 应有 ≥0.95 的命中，实际 %q %.2f", first.Name, first.Accuracy)
	}

	// 留一张实拍给验收：BAOZI_PREVIEW=<绝对路径> go test -run TestAppSearchIgnoresPathNoise
	a.query = "cal"
	a.results = got
	a.chosen = 0
	stubAllIcons(a)
	tr := ui.NewTester(a.view, panelWidth, panelHeight)
	tr.Frame()
	dumpPreview(t, tr)
}
