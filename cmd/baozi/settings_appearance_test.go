package main

import (
	"testing"

	"github.com/egoist/mygo/ui"

	"baozi/internal/config"
)

// configToggle 造一个"关掉 + 指定程度"的开关：用来验证 load 读程度值时不看开关。
func configToggle(off bool, v int) config.Toggle {
	return config.Toggle{Enabled: &off, Value: &v}
}

// 左栏多了「界面外观」这一项：点它右栏要滚到那一段，并且三项内容都在。
func TestSettingsAppearanceSection(t *testing.T) {
	a := testApp(t)
	s := &settingsWindow{app: a}
	s.load()
	tr := ui.NewTester(s.view, 900, 820)
	tr.Frame()

	if len(settingsNavItems) != secCommands+1 {
		t.Fatalf("左栏项数（%d）和 sec* 常量对不上（secCommands=%d）",
			len(settingsNavItems), secCommands)
	}
	if err := tr.Click(settingsNavItems[secAppearance]); err != nil {
		t.Fatalf("点左栏「%s」失败：%v", settingsNavItems[secAppearance], err)
	}
	tr.Frame()
	if s.navSel != secAppearance {
		t.Fatalf("没选中「界面外观」，navSel=%d", s.navSel)
	}
	for _, want := range []string{"界面外观", "圆角", "玻璃不透明度", "投影"} {
		if !tr.HasText(want) {
			t.Fatalf("外观段里没找到「%s」", want)
		}
	}
	dumpPreview(t, tr)
}

// load 要把存着的程度值读出来：关掉再打开不能跳回默认。
func TestSettingsAppearanceLoadKeepsValue(t *testing.T) {
	a := testApp(t)
	off := false
	r := 8
	a.cfg.Appearance.Radius = configToggle(off, r)
	a.cfg.Appearance.Opacity = configToggle(off, 12)
	a.cfg.Appearance.Shadow = configToggle(off, 3)
	hide := true
	a.cfg.Startup.HideOnStart = &hide

	s := &settingsWindow{app: a}
	s.load()
	if s.radiusOn || s.opacityOn || s.shadowOn {
		t.Fatalf("三项都关了，load 后应当都是 false（%v/%v/%v）",
			s.radiusOn, s.opacityOn, s.shadowOn)
	}
	if s.radiusVal != 8 || s.opacityVal != 12 || s.shadowVal != 3 {
		t.Fatalf("程度值没读对：%g/%g/%g", s.radiusVal, s.opacityVal, s.shadowVal)
	}
	if !s.hideOnStart {
		t.Fatal("启动隐藏没读出来")
	}
}
