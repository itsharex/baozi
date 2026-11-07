package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
