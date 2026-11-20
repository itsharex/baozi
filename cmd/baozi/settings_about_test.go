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
	// 这一页只留内容：版本号、GitHub 入口、状态行与按钮、开关。
	// 灰色标签（"版本""GitHub""更新""网络""上一次"）和括号补充都去掉了。
	for _, want := range []string{"关于", "立即检查", "启动时自动检查更新"} {
		if !tr.HasText(want) {
			t.Fatalf("关于页里没找到「%s」", want)
		}
	}
	// GitHub 地址要原样写出来（用户拿它当入口，不能只写个"项目主页"）。
	if !tr.HasText("github.com/" + update.Repo) {
		t.Fatalf("关于页里没有 GitHub 地址 github.com/%s", update.Repo)
	}
	// 版本号自己就是内容，直接摆出来。
	if !tr.HasText(version.Display()) {
		t.Fatalf("关于页里没有版本号 %s", version.Display())
	}
	for _, gone := range []string{"本地编译，不参与更新比较", "源码、Release 和安装包都在这里", "上一次"} {
		if tr.HasText(gone) {
			t.Fatalf("关于页里还留着灰字「%s」", gone)
		}
	}
}

// 「关于」页现在摆一个"使用代理 地址"的复选框；地址在这一页打开时现读，
// 关掉开关后更新一律直连。
func TestAboutProxyCheckbox(t *testing.T) {
	a := testApp(t)
	s := &settingsWindow{app: a}
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:7892")
	s.load()
	if s.proxyAddr != "127.0.0.1:7892" {
		t.Fatalf("打开设置时没读到代理地址：%q", s.proxyAddr)
	}
	if !s.useProxy {
		t.Fatal("缺省应该是勾上的")
	}

	tr := ui.NewTester(s.view, 900, 820)
	tr.Frame()
	_ = tr.Click(settingsNavItems[secAbout])
	tr.Frame()
	if !tr.HasText("使用代理 127.0.0.1:7892") {
		t.Fatal("关于页里没有「使用代理 127.0.0.1:7892」")
	}

	// 取消勾选立刻生效，并落进配置。
	s.useProxy = false
	update.SetUseProxy(false)
	defer update.SetUseProxy(true)
	if update.ProxySummary() != "直连（已关掉代理）" {
		t.Fatalf("关掉后还在走代理：%q", update.ProxySummary())
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
		{"已最新", updateSnapshot{Tag: "v0.3.0"}, "已是最新"},
		{"失败", updateSnapshot{Err: "github 返回 403"}, "检查失败：github 返回 403"},
		{"没查过", updateSnapshot{}, "还没查过"},
		{"倒计时中", updateSnapshot{Tag: "v0.4.0", Newer: true, Installing: true, Progress: "正在下载 v0.4.0…"}, "正在下载 v0.4.0…"},
		{"装完但还没散", updateSnapshot{Tag: "v0.4.0", Newer: true, Progress: "下载失败：代理连不上"}, "下载失败：代理连不上"},
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
	if !strings.Contains(got, "开发版") {
		t.Fatalf("开发版应当标出来，实际 %q", got)
	}
	// 「关于」页不摆括号补充，状态行自己就不能带括号。
	if strings.ContainsAny(got, "（）") {
		t.Fatalf("状态行不该带括号：%q", got)
	}
}

// TestUpdateStatusNoParens 是「关于」页那条要求（删掉灰色字和括号内容）的守门：
// 任何状态下都不许带括号。
func TestUpdateStatusNoParens(t *testing.T) {
	snaps := []updateSnapshot{
		{Busy: true},
		{Tag: "v0.4.0", Newer: true},
		{Tag: "v0.3.0"},
		{Err: "403"},
		{},
		{Tag: "v0.4.0", Newer: true, Installing: true, Progress: "正在下载 v0.4.0…"},
	}
	for _, s := range snaps {
		if got := updateStatusText(s); strings.ContainsAny(got, "（）") {
			t.Errorf("状态 %+v 说出带括号的话：%q", s, got)
		}
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

// 代理开关缺省也是开的，并且能落盘；老配置里没有 useProxy 就当开。
func TestUpdateUseProxyDefaultAndPersist(t *testing.T) {
	cfg := config.Default()
	if !cfg.UpdatesUseProxy() {
		t.Fatal("缺省应该用代理")
	}
	off := false
	cfg.Updates = config.Updates{UseProxy: &off}
	if cfg.UpdatesUseProxy() {
		t.Fatal("写了 false 就该关掉")
	}

	path := filepath.Join(t.TempDir(), "config.json")
	if err := config.Save(cfg, path); err != nil {
		t.Fatalf("保存失败：%v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读回失败：%v", err)
	}
	if !strings.Contains(string(raw), `"useProxy": false`) {
		t.Fatalf("config.json 里没写 useProxy=false：\n%s", string(raw))
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
