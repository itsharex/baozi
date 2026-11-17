package main

import (
	"testing"

	"github.com/egoist/mygo/ui"
)

// 左栏点一项，右栏要滚到那一段：选中项跟着变，滚动请求只在那一帧发出。
func TestSettingsNavJumpsToSection(t *testing.T) {
	a := testApp(t)
	s := &settingsWindow{app: a}
	s.load()
	tr := ui.NewTester(s.view, 900, 820)
	tr.Frame()
	if s.navSel != secHotkey {
		t.Fatalf("左栏默认应停在「唤出」，实际 %d", s.navSel)
	}

	if err := tr.Click(settingsNavItems[secRank]); err != nil {
		t.Fatalf("点左栏「%s」失败：%v", settingsNavItems[secRank], err)
	}
	tr.Frame()
	if s.navSel != secRank {
		t.Fatalf("点左栏「%s」没选中它，navSel=%d", settingsNavItems[secRank], s.navSel)
	}
	if s.navJump != -1 {
		t.Fatalf("滚动请求没在那一帧用掉，navJump=%d", s.navJump)
	}

	// 「自定义命令」是独立的一页，点它要切过去，右栏得出现命令表单。
	if err := tr.Click(settingsNavItems[secCommands]); err != nil {
		t.Fatalf("点左栏「自定义命令」失败：%v", err)
	}
	tr.Frame()
	if !tr.HasText("激活词") || !tr.HasText("将执行") {
		t.Fatal("切到「自定义命令」后右栏没有命令表单")
	}
	dumpPreview(t, tr)
}
