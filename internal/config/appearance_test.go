package config

import "testing"

// 外观三项的缺省语义：零值（老配置里没有 appearance 这一块）必须等同于
// "现在的样子" —— 圆角 24、不透明度 59%、投影 57%。
func TestAppearanceDefaults(t *testing.T) {
	var c Config
	if r := c.PanelRadius(); r != DefaultPanelRadius {
		t.Fatalf("圆角缺省应为 %d，实际 %d", DefaultPanelRadius, r)
	}
	if p := c.GlassTransparencyPct(); p != DefaultGlassTransparency {
		t.Fatalf("透明度缺省应为 %d，实际 %d", DefaultGlassTransparency, p)
	}
	if p := c.ShadowPct(); p != DefaultShadowPct {
		t.Fatalf("投影缺省应为 %d，实际 %d", DefaultShadowPct, p)
	}
	if c.HideOnStart() {
		t.Fatal("启动隐藏缺省应为关")
	}
}

// Default() 要把三项写成具体数，好让 config.json 里一眼看到现状。
func TestAppearanceInDefault(t *testing.T) {
	c := Default()
	if c.Appearance.Radius.Value == nil || *c.Appearance.Radius.Value != DefaultPanelRadius {
		t.Fatalf("Default 里应写死圆角 %d", DefaultPanelRadius)
	}
	if c.PanelRadius() != DefaultPanelRadius {
		t.Fatal("Default 读回来应当还是缺省值")
	}
}

// 关掉就是"不要这项效果"：圆角 0（直角）、不透明度 0（只剩模糊）、投影 0（没有影）。
func TestAppearanceDisabled(t *testing.T) {
	c := Default()
	off := false
	c.Appearance.Radius.Enabled = &off
	c.Appearance.Opacity.Enabled = &off
	c.Appearance.Shadow.Enabled = &off
	if c.PanelRadius() != 0 || c.GlassTransparencyPct() != 0 || c.ShadowPct() != 0 {
		t.Fatalf("全关之后应全为 0，实际 %d/%d/%d",
			c.PanelRadius(), c.GlassTransparencyPct(), c.ShadowPct())
	}
	// 关掉不影响存着的程度值：用户再打开时应回到原来的数。
	if got := c.Appearance.Radius.Int(DefaultPanelRadius, 0, MaxPanelRadius); got != DefaultPanelRadius {
		t.Fatalf("关掉后程度值应保留 %d，实际 %d", DefaultPanelRadius, got)
	}
}

// 手改配置写进去的离谱值要夹住：半径不能把面板裁成 -5，百分比不能超 100。
func TestAppearanceClamp(t *testing.T) {
	c := Default()
	neg, big := -5, 999
	c.Appearance.Radius.Value = &neg
	c.Appearance.Opacity.Value = &big
	c.Appearance.Shadow.Value = &neg
	if r := c.PanelRadius(); r != 0 {
		t.Fatalf("负半径应夹到 0，实际 %d", r)
	}
	if p := c.GlassTransparencyPct(); p != 100 {
		t.Fatalf("超上限应夹到 100，实际 %d", p)
	}
	if p := c.ShadowPct(); p != 0 {
		t.Fatalf("负强度应夹到 0，实际 %d", p)
	}
	over := MaxPanelRadius + 10
	c.Appearance.Radius.Value = &over
	if r := c.PanelRadius(); r != MaxPanelRadius {
		t.Fatalf("半径应夹到 %d，实际 %d", MaxPanelRadius, r)
	}
}

// 启动隐藏：明确勾上才算，缺省不勾。
func TestHideOnStart(t *testing.T) {
	c := Default()
	if c.HideOnStart() {
		t.Fatal("缺省不该隐藏")
	}
	on := true
	c.Startup.HideOnStart = &on
	if !c.HideOnStart() {
		t.Fatal("明确勾上后应当隐藏")
	}
}
