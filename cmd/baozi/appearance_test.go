package main

import (
	"testing"

	"baozi/internal/config"
)

// applyAppearance 把设置页的"人话单位"折算成绘制层读的包级变量。
// 这里钉住三条：默认值等于原来的观感、透明度越大越透、以及"禁止完全透明"。
func TestApplyAppearance(t *testing.T) {
	// 这些是包级变量，测完要放回去，别污染别的用例。
	savedR, savedClip := glassRadius, glassClipRadius
	savedAlpha, savedBlur := glassTintAlpha, glassBlurRadius
	defer func() {
		glassRadius, glassClipRadius = savedR, savedClip
		glassTintAlpha, glassBlurRadius = savedAlpha, savedBlur
	}()

	cfg := config.Default()
	applyAppearance(cfg)
	if glassRadius != config.DefaultPanelRadius || glassClipRadius != config.DefaultPanelRadius-1 {
		t.Fatalf("缺省半径应 %d/%d，实际 %d/%d",
			config.DefaultPanelRadius, config.DefaultPanelRadius-1, glassRadius, glassClipRadius)
	}
	// 255 - 59*165/100 = 158；模糊保持原来的 5。
	if glassTintAlpha != 158 {
		t.Fatalf("缺省压白 alpha 应 158，实际 %d", glassTintAlpha)
	}
	if glassBlurRadius != 5 {
		t.Fatalf("缺省模糊半径应 5，实际 %d", glassBlurRadius)
	}

	// 透明度 0 = 完全不透明：白层盖满、模糊最重。
	zero := 0
	cfg.Appearance.Opacity.Value = &zero
	applyAppearance(cfg)
	if glassTintAlpha != 255 {
		t.Fatalf("透明度 0 应完全不透明（alpha 255），实际 %d", glassTintAlpha)
	}
	if glassBlurRadius != 7 {
		t.Fatalf("透明度 0 模糊应最重（7），实际 %d", glassBlurRadius)
	}

	// 透明度 100 = 最透，但 alpha 有 90 的下限（禁止完全透明），模糊也只减到 3。
	hundred := 100
	cfg.Appearance.Opacity.Value = &hundred
	applyAppearance(cfg)
	if glassTintAlpha != 90 {
		t.Fatalf("透明度 100 应夹在 alpha 90，实际 %d", glassTintAlpha)
	}
	if glassBlurRadius != 3 {
		t.Fatalf("透明度 100 模糊应最轻（3），实际 %d", glassBlurRadius)
	}

	// 把开关关掉 = 不要玻璃能看穿 → 等同透明度 0（实心）。
	off := false
	cfg.Appearance.Opacity.Enabled = &off
	applyAppearance(cfg)
	if glassTintAlpha != 255 {
		t.Fatalf("关掉透明度开关应完全不透明，实际 alpha %d", glassTintAlpha)
	}

	// 圆角关掉：半径 0，内容层也不再留那 1 DIP 的缝。
	cfg.Appearance.Opacity.Enabled = nil
	cfg.Appearance.Radius.Enabled = &off
	applyAppearance(cfg)
	if glassRadius != 0 || glassClipRadius != 0 {
		t.Fatalf("关掉圆角应 0/0，实际 %d/%d", glassRadius, glassClipRadius)
	}
}
