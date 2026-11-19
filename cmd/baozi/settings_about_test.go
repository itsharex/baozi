package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"baozi/internal/config"
	"baozi/internal/update"
	"baozi/internal/version"

	"github.com/egoist/mygo/ui"
)

// 「关于」是左栏最后一项：点进去要能看到版本、GitHub 地址、更新状态和那个开关。
func TestSettingsAboutSection(t *testing.T) {
	a := testApp(t)
	s := &settingsWindow{app: a}
	s.load()
	tr := ui.NewTester(s.view, 900, 820)
	tr.Frame()

	if settingsNavItems[secAbout] != "关于" {
		t.Fatalf("左栏最后一项应该是「关于」，实际 %q", settingsNavItems[secAbout])
	}
	if err := tr.Click(settingsNavItems[secAbout]); err != nil {
		t.Fatalf("点左栏「关于」失败：%v", err)
	}
	tr.Frame()
	if s.navSel != secAbout {
		t.Fatalf("没选中「关于」，navSel=%d", s.navSel)
	}
	for _, want := range []string{"关于", "版本", "GitHub", "更新", "启动时自动检查更新"} {
		if !tr.HasText(want) {
			t.Fatalf("关于页里没找到「%s」", want)
		}
	}
	// GitHub 地址要原样写出来（用户拿它当入口，不能只写个"项目主页"）。
	if !tr.HasText("github.com/" + update.Repo) {
		t.Fatalf("关于页里没有 GitHub 地址 github.com/%s", update.Repo)
	}
}

// 状态行：四种情况各自说人话，别把原始错误或空串直接摆上去。
func TestUpdateStatusText(t *testing.T) {
	// 测试里没有注入版本号，默认是 dev —— 先钉一个真版本，才能验"已是最新"那句话。
	old := version.Version
	version.Version = "v0.3.0"
	defer func() { version.Version = old }()

	cases := []struct {
		name  string
		snap  updateSnapshot
		wants string
	}{
		{"正在查", updateSnapshot{Busy: true}, "正在检查…"},
		{"有新版", updateSnapshot{Tag: "v0.4.0", Newer: true}, "发现新版本 v0.4.0"},
		{"已最新", updateSnapshot{Tag: "v0.3.0"}, "已是最新（v0.3.0）"},
		{"失败", updateSnapshot{Err: "github 返回 403"}, "检查失败：github 返回 403"},
		{"没查过", updateSnapshot{}, "还没查过"},
	}
	for _, c := range cases {
		got := updateStatusText(c.snap)
		if c.wants == "" {
			continue
		}
		if len(got) < len(c.wants) || got[:len(c.wants)] != c.wants {
			t.Errorf("%s：状态是 %q，想要以 %q 开头", c.name, got, c.wants)
		}
	}
}

// 开发版查到线上版本时，不能说"已是最新"——它跟线上没有可比的身份。
func TestUpdateStatusDevBuild(t *testing.T) {
	old := version.Version
	version.Version = "dev"
	defer func() { version.Version = old }()

	got := updateStatusText(updateSnapshot{Tag: "v0.3.0"})
	if !strings.Contains(got, "不参与比较") {
		t.Fatalf("开发版应当说明不参与比较，实际 %q", got)
	}
}

// 错误串会被截断：API 的报错能长到几百字，一行放不下。
func TestUpdateStatusTruncatesError(t *testing.T) {
	long := ""
	for i := 0; i < 200; i++ {
		long += "错"
	}
	got := updateStatusText(updateSnapshot{Err: long})
	if r := []rune(got); len(r) > 100 {
		t.Fatalf("状态行没截断，长度 %d", len(r))
	}
}

// 自动检查更新缺省是开的：老配置里没有 updates 这一节，读进来也要检查。
func TestUpdateAutoCheckDefault(t *testing.T) {
	cfg := config.Default()
	if !cfg.UpdateAutoCheck() {
		t.Fatal("缺省应该自动检查更新")
	}
	off := false
	cfg.Updates = config.Updates{AutoCheck: &off}
	if cfg.UpdateAutoCheck() {
		t.Fatal("写了 false 就该关掉")
	}
}

// 关掉自动检查要能落到 config.json 里（关于页保存的是真实偏好，不是仅本进程）。
func TestUpdateAutoCheckPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	cfg := config.Default()
	off := false
	cfg.Updates = config.Updates{AutoCheck: &off}
	if err := config.Save(cfg, path); err != nil {
		t.Fatalf("保存失败：%v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读回失败：%v", err)
	}
	if !strings.Contains(string(raw), `"autoCheck": false`) {
		t.Fatalf("config.json 里没写 autoCheck=false：\n%s", string(raw))
	}
}

// 开发版不提示更新：本地自己编的版本跟线上比没有意义。
func TestDevBuildNeverNags(t *testing.T) {
	old := version.Version
	version.Version = "dev"
	defer func() { version.Version = old }()

	a := testApp(t)
	rel := &update.Release{Tag: "v9.9.9", PageURL: "https://example.com"}
	a.upd.ok(rel)
	if a.latestTag() != "" {
		t.Fatalf("开发版不该报新版本，latestTag=%q", a.latestTag())
	}
}
