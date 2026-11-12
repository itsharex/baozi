package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 图标尺寸的缺省语义：没配置就跟着系统缩放，配了就照配置（不再乘缩放）。
// 面板尺寸：没配置走默认 720×480；填了就照填的来，越界夹到上下限。
// 夹取的目的是"填错也别把面板撑出屏幕或缩成一条缝"。
func TestPanelSize(t *testing.T) {
	var c Config
	if w, h := c.PanelSize(); w != DefaultPanelWidth || h != DefaultPanelHeight {
		t.Fatalf("默认应为 %d×%d，实际 %d×%d", DefaultPanelWidth, DefaultPanelHeight, w, h)
	}

	c.Panel.Width, c.Panel.Height = 1200, 900
	if w, h := c.PanelSize(); w != 1200 || h != 900 {
		t.Fatalf("填了 1200×900 应原样生效，实际 %d×%d", w, h)
	}

	c.Panel.Width, c.Panel.Height = 100, 100
	if w, h := c.PanelSize(); w != MinPanelWidth || h != MinPanelHeight {
		t.Fatalf("过小应夹到 %d×%d，实际 %d×%d", MinPanelWidth, MinPanelHeight, w, h)
	}

	c.Panel.Width, c.Panel.Height = 9999, 9999
	if w, h := c.PanelSize(); w != MaxPanelWidth || h != MaxPanelHeight {
		t.Fatalf("过大应夹到 %d×%d，实际 %d×%d", MaxPanelWidth, MaxPanelHeight, w, h)
	}
}

func TestIconSizeAt(t *testing.T) {
	var c Config
	if got := c.IconSizeAt(1); got != BaseIconSize {
		t.Fatalf("100%% 缩放应为 %d，实际 %v", BaseIconSize, got)
	}
	if got := c.IconSizeAt(1.25); got != BaseIconSize*1.25 {
		t.Fatalf("125%% 缩放应为 %v，实际 %v", BaseIconSize*1.25, got)
	}
	// scale 拿不到（0）不能算出 0 —— 那样图标会整个消失。
	if got := c.IconSizeAt(0); got != BaseIconSize {
		t.Fatalf("缩放未知时应退回 %d，实际 %v", BaseIconSize, got)
	}

	c.Search.IconSize = 34
	if got := c.IconSizeAt(1.25); got != 34 {
		t.Fatalf("用户填了 34 就该是 34（不再乘缩放），实际 %v", got)
	}
}

// 目录来源开关的缺省语义：配置里没有这一块 → 全部启用（老配置的行为不变）。
func TestPathSourcesDefaultOn(t *testing.T) {
	var c Config
	for _, kind := range []string{"explorer", "totalcmd", "opus", "xyplorer", "xplorer2"} {
		if !c.PathSourceEnabled(kind) {
			t.Fatalf("%s 在缺省配置下应当是启用的", kind)
		}
	}
}

// 写了明确 false 的来源要真的关掉，写了 true 的要开；没写的仍按"缺省即开"。
func TestPathSourcesExplicitFalse(t *testing.T) {
	off := false
	on := true
	c := Config{PathSources: &PathSources{
		Opus:     &off,
		Xyplorer: &off,
		Xplorer2: &on,
	}}
	cases := map[string]bool{
		"explorer": true,  // 没写
		"totalcmd": true,  // 没写
		"opus":     false, // 明确关
		"xyplorer": false, // 明确关
		"xplorer2": true,  // 明确开
	}
	for kind, want := range cases {
		if got := c.PathSourceEnabled(kind); got != want {
			t.Fatalf("%s: 期望 %v，实际 %v", kind, want, got)
		}
	}
}

// 未知来源（拼错的 kind）不能因为认不出来就一律关掉 —— 当作启用，行为最接近"没影响"。
func TestPathSourcesUnknownKindOn(t *testing.T) {
	off := false
	c := Config{PathSources: &PathSources{Explorer: &off}}
	if !c.PathSourceEnabled("files") {
		t.Fatal("未知来源应当按启用处理")
	}
}

// 设置页写进去的 JSON 必须是设置页读得出来的形状（键名与 win.Source* 一致）。
func TestPathSourcesJSONRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BAOZI_DATA_DIR", dir)

	off := false
	on := true
	follow := false
	c := Default()
	c.DialogFollow = &follow
	c.PathSources = &PathSources{Explorer: &on, TotalCmd: &on, Opus: &off, Xyplorer: &on, Xplorer2: &off}

	path := filepath.Join(dir, "config.json")
	if err := Save(c, path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, gotPath, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if gotPath != path {
		t.Fatalf("配置路径: 期望 %s，实际 %s", path, gotPath)
	}
	if got.DialogFollowOn() {
		t.Fatal("dialogFollow=false 应当读回关闭")
	}
	if got.PathSourceEnabled("opus") {
		t.Fatal("opus=false 应当读回关闭")
	}
	if !got.PathSourceEnabled("xyplorer") {
		t.Fatal("xyplorer=true 应当读回开启")
	}

	// 落盘的键名要和跟随器 / 设置页用的字面量一致，否则改名之后静默失效。
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读回 config.json: %v", err)
	}
	for _, key := range []string{`"explorer"`, `"totalcmd"`, `"opus"`, `"xyplorer"`, `"xplorer2"`} {
		if !strings.Contains(string(data), key) {
			t.Fatalf("pathSources 缺少键 %s：%s", key, data)
		}
	}
}
