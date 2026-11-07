package main

import (
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"baozi/internal/config"
	"baozi/internal/history"
	"baozi/internal/index"
	"baozi/internal/search"
	"baozi/internal/share"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
)

// stubAllIcons 让测试里的图标不走 Shell：Tester 没有 native 消息循环，
// Shell.FileIcon 内部是 onMainValue，会一直阻塞到超时。
// 打开 offline 后应用图标直接返回 nil，视图退回通用线框图标。
func stubAllIcons(a *application) {
	a.offline = true
}

// 用 mygo 的无窗口 Tester 渲染一帧：这类错误（控件 API 用法不对、
// 布局在某个分支里 nil 解引用）只有真的构建这一帧才会暴露。
func testApp(t *testing.T) *application {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Index.Roots = []string{dir}
	h, err := history.Open(filepath.Join(dir, "history.json"), 50, 0)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	return &application{
		cfg:     cfg,
		cfgPath: filepath.Join(dir, "config.json"),
		dataDir: dir,
		hist:    h,
		icons:   map[string]*ui.Bitmap{},
		status:  "测试",
	}
}

func TestPanelViewRenders(t *testing.T) {
	a := testApp(t)
	tester := ui.NewTester(a.view, panelWidth, panelHeight)
	tester.Frame()
	if !tester.HasText("开始输入以搜索文件和应用") {
		t.Fatal("空状态提示没有渲染出来")
	}
}

func TestSettingsViewRenders(t *testing.T) {
	a := testApp(t)
	s := &settingsWindow{app: a}
	s.load()
	tester := ui.NewTester(s.view, 880, 900)
	tester.Frame()
	for _, want := range []string{"Baozi 设置", "双击 Ctrl 唤出面板", "保存并应用"} {
		if !tester.HasText(want) {
			t.Fatalf("设置界面缺少 %q", want)
		}
	}
	// 文件对话框跟随那一块：总开关 + 五种目录来源，一个都不能少。
	for _, want := range []string{
		"文件对话框跟随", "启用跟随",
		"资源管理器", "Total Commander", "Directory Opus", "XYplorer", "xplorer2",
	} {
		if !tester.HasText(want) {
			t.Fatalf("设置界面缺少来源开关 %q", want)
		}
	}
	dumpPreview(t, tester)
}

// dumpPreview 在 BAOZI_PREVIEW=<绝对路径> 时把这一帧存成 PNG，
// 靠肉眼核对布局（挤压、换行、对齐）。平时不写盘。
func dumpPreview(t *testing.T, tr *ui.Tester) {
	t.Helper()
	p := os.Getenv("BAOZI_PREVIEW")
	if p == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("建预览目录: %v", err)
	}
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("建预览文件: %v", err)
	}
	defer f.Close()
	if err := png.Encode(f, tr.Image()); err != nil {
		t.Fatalf("写预览 PNG: %v", err)
	}
	t.Logf("预览已写入 %s", p)
}

func TestSettingsParseCommands(t *testing.T) {
	cases := []struct {
		in   string
		want int
		kw   string
	}{
		{"term | Windows Terminal | wt.exe", 1, "term"},
		{"code | VS Code | code | --new-window", 1, "code"},
		{"term | wt.exe\ncode | code", 2, "term"},
		{"", 0, ""},
	}
	for _, c := range cases {
		got := parseCommands(c.in)
		if len(got) != c.want {
			t.Fatalf("%q 解析出 %d 条，期望 %d", c.in, len(got), c.want)
		}
		if c.want > 0 && got[0].Keyword != c.kw {
			t.Fatalf("%q 首个关键字 = %q", c.in, got[0].Keyword)
		}
		if c.want > 1 && got[1].Exec != "code" {
			t.Fatalf("第二行 exec = %q", got[1].Exec)
		}
	}
}

func TestShareViewRenders(t *testing.T) {
	a := testApp(t)
	sess, err := share.Start("", time.Hour)
	if err != nil {
		t.Fatalf("share.Start: %v", err)
	}
	defer sess.Stop()
	src := filepath.Join(a.dataDir, "demo.txt")
	if err := os.WriteFile(src, []byte("demo"), 0o644); err != nil {
		t.Fatal(err)
	}
	sess.Add(src)

	s := &shareWindow{app: a, session: sess}
	tester := ui.NewTester(s.view, 760, 680)
	tester.Frame()
	// 访问码要单独可见（4 位、手敲用），网卡下拉也要在。
	// 「添加文件 / 添加文件夹」固定在滚动区外面，任何窗口高度下都得看得见
	// ——这两个按钮之前在滚动区底部，内容一多就被顶出可视范围，用户以为按钮没了。
	for _, want := range []string{"文件互传", "demo.txt", "复制链接", "访问码", sess.Token(), "绑定网卡", "添加文件", "添加文件夹", "链接有效期", "到期"} {
		if !tester.HasText(want) {
			t.Fatalf("互传界面缺少 %q", want)
		}
	}
	// 窗口缩到最小高度也要看得到
	small := ui.NewTester(s.view, 640, 520)
	small.Frame()
	for _, want := range []string{"添加文件", "添加文件夹", "停止共享"} {
		if !small.HasText(want) {
			t.Fatalf("最小尺寸下缺少 %q", want)
		}
	}

	// 还没生成链接时（session 为空）：入口按钮是「生成链接」，且已挑好的文件要能看见。
	stopped := &shareWindow{app: a, pending: []share.Item{{Path: src, Name: "demo.txt", Size: 4}}}
	st := ui.NewTester(stopped.view, 640, 520)
	st.Frame()
	for _, want := range []string{"生成链接", "添加文件", "添加文件夹", "待分享", "demo.txt", "链接有效期"} {
		if !st.HasText(want) {
			t.Fatalf("未共享状态缺少 %q", want)
		}
	}
}

func TestHistoryToResults(t *testing.T) {
	a := testApp(t)
	a.hist.TouchEntry(history.Entry{
		Path: `shell:AppsFolder\Microsoft.WindowsCalculator_8wekyb3d8bbwe!App`,
		Name: "计算器", Kind: history.KindApp, Ext: ".exe", Hint: "应用",
	})
	a.hist.TouchEntry(history.Entry{
		Path: `D:\docs\合同.docx`,
		Name: "合同.docx", Kind: history.KindFile, Ext: ".docx", Hint: `D:\docs`,
	})
	got := a.historyResults(5)
	if len(got) != 2 {
		t.Fatalf("应有 2 条历史，实际 %d", len(got))
	}
	for _, r := range got {
		switch r.Path {
		case `shell:AppsFolder\Microsoft.WindowsCalculator_8wekyb3d8bbwe!App`:
			if r.Name != "计算器" {
				t.Fatalf("应用历史名字被还原成 %q", r.Name)
			}
			if r.Kind != 2 { // search.KindApp
				t.Fatalf("应用历史类型 = %d，期望 KindApp", r.Kind)
			}
		case `D:\docs\合同.docx`:
			if r.Name != "合同.docx" || r.Kind != 0 {
				t.Fatalf("文件历史还原错误: %+v", r)
			}
		}
	}
}

// seedSearchApp 造一个"开始菜单应用 + 同名文件"的场景，用来验证
// (1) 拼音首字母能搜到中文应用名，(2) 类型权重让应用压过更准确的文件。
func seedSearchApp(t *testing.T, a *application) {
	t.Helper()
	appPath := `shell:AppsFolder\Doubao.Office_1.0.0.0_x64!App`
	apps := []appEntry{{
		InstalledApp: mygo.InstalledApp{Name: "豆包工作", ID: "Doubao.Office_1.0.0.0_x64!App", Path: appPath},
		initials:     "dbgz",
		full:         "doubaogongzuo",
	}}
	a.apps.Store(&apps)

	tab := &index.Table{
		Nodes: []index.Node{
			{Parent: -1, Kind: index.KindDir, NameOff: 0, NameLen: 3},
			{Parent: 0, Kind: index.KindFile, NameOff: 3, NameLen: 10},
			{Parent: 0, Kind: index.KindDir, NameOff: 13, NameLen: 12},
		},
		Names: []byte(`C:\doubao.exe豆包工作` + "dbgz"), // 拼音串占 arena 尾部（off 25，长 4）
		Py:    map[int32]uint64{2: index.PackPy(25, 4)},
	}
	a.cat.Store(tab)
}

func TestPinyinFindsChineseApp(t *testing.T) {
	a := testApp(t)
	seedSearchApp(t, a)

	got := a.searchAll("dbgz")
	if len(got) == 0 {
		t.Fatal("输入 dbgz 应当命中「豆包工作」")
	}
	first := got[0]
	if first.Kind != search.KindApp {
		t.Fatalf("首条应是应用，实际 Kind=%d（%s）", first.Kind, first.Name)
	}
	if first.Name != "豆包工作" {
		t.Fatalf("首条名字 = %q，期望「豆包工作」", first.Name)
	}
	if first.Accuracy < 0.95 {
		t.Fatalf("拼音全等的准确度只有 %.3f", first.Accuracy)
	}

	// 同名的目录也在结果里，但类型分低，必须排到应用后面
	for i, r := range got {
		if r.Name == "豆包工作" && r.Kind == search.KindDir && i < 1 {
			t.Fatal("目录不该压过应用")
		}
	}
}

func TestAppBeatsExeInSearch(t *testing.T) {
	a := testApp(t)
	seedSearchApp(t, a)

	// doubao 只命中了应用的全拼（0.88）和文件的名字前缀（0.95），
	// 准确度是文件更高；但用户要的第一权重是类型，所以应用仍该在前。
	got := a.searchAll("doubao")
	if len(got) < 2 {
		t.Fatalf("应有应用和文件两条结果，实际 %d", len(got))
	}
	if got[0].Kind != search.KindApp {
		t.Fatalf("首条应是应用，实际 %s (Kind=%d)", got[0].Name, got[0].Kind)
	}
}

// 首页网格：固定项排在前面，pinnedCount 与「已固定」段一致。
func TestHomeGridSplitsPinned(t *testing.T) {
	a := testApp(t)
	for _, p := range []string{"a", "b", "c"} {
		a.hist.TouchEntry(history.Entry{Path: p, Name: p, Kind: history.KindApp})
	}
	if _, ok := a.hist.TogglePin("b"); !ok {
		t.Fatal("固定失败")
	}
	a.refilter()
	if a.pinnedCount != 1 {
		t.Fatalf("pinnedCount=%d，应为 1", a.pinnedCount)
	}
	if len(a.results) != 3 {
		t.Fatalf("总数 %d，应为 3", len(a.results))
	}
	if a.results[len(a.results)-1].Path != "b" {
		t.Fatalf("末条应是固定的 b（固定沉底），实际 %s", a.results[len(a.results)-1].Path)
	}
}

// Ctrl+K 在输入框有焦点时也要能固定 / 取消固定（Pane 的交互）。
func TestCtrlKPinsFromKeyboard(t *testing.T) {
	a := testApp(t)
	const exe = `C:\Windows\notepad.exe`
	a.hist.TouchEntry(history.Entry{Path: exe, Name: "记事本", Kind: history.KindApp, Ext: ".exe", Hint: "应用"})
	a.refilter()
	stubAllIcons(a)
	tr := ui.NewTester(a.view, panelWidth, panelHeight)
	tr.Frame()
	tr.Key(ui.Ctrl, ui.KeyK)
	tr.Frame()
	if !a.hist.IsPinned(exe) {
		t.Fatal("Ctrl+K 没有固定选中的条目")
	}
	tr.Key(ui.Ctrl, ui.KeyK)
	tr.Frame()
	if a.hist.IsPinned(exe) {
		t.Fatal("再按一次 Ctrl+K 应该取消固定")
	}
}

// 首页方向键按行移动（一行 panelCols 个）。
func TestHomeGridArrowMovesByRow(t *testing.T) {
	a := testApp(t)
	for i := 0; i < 10; i++ {
		a.hist.TouchEntry(history.Entry{Path: fmt.Sprintf("p%d", i), Name: fmt.Sprintf("item%d", i), Kind: history.KindApp})
	}
	a.refilter()
	stubAllIcons(a)
	tr := ui.NewTester(a.view, panelWidth, panelHeight)
	tr.Frame()
	tr.Key(0, ui.KeyDown)
	tr.Frame()
	if a.chosen != panelCols {
		t.Fatalf("Down 之后 chosen=%d，应为 %d", a.chosen, panelCols)
	}
	tr.Key(0, ui.KeyUp)
	tr.Frame()
	if a.chosen != 0 {
		t.Fatalf("Up 之后 chosen=%d，应为 0", a.chosen)
	}
	// 左右是 ±1（首行从左往右走）。
	tr.Key(0, ui.KeyRight)
	tr.Frame()
	if a.chosen != 1 {
		t.Fatalf("Right 之后 chosen=%d，应为 1", a.chosen)
	}
	tr.Key(0, ui.KeyLeft)
	tr.Frame()
	if a.chosen != 0 {
		t.Fatalf("Left 之后 chosen=%d，应为 0", a.chosen)
	}
}

// 焦点在搜索框里时方向键也要能动：TextInput 会把左右键吃掉（光标移动），
// 所以首页的方向键读的是按住状态，而不是 Shortcut。
func TestHomeArrowsWorkWithInputFocus(t *testing.T) {
	a := testApp(t)
	for i := 0; i < 10; i++ {
		a.hist.TouchEntry(history.Entry{Path: fmt.Sprintf("p%d", i), Name: fmt.Sprintf("item%d", i), Kind: history.KindApp})
	}
	a.refilter()
	stubAllIcons(a)
	tr := ui.NewTester(a.view, panelWidth, panelHeight)
	tr.Frame()
	tr.ClickAt(300, 30)
	tr.Frame()
	if !tr.Focused("搜索") {
		t.Fatal("点击没能把焦点落到搜索框，测试前提没成立")
	}
	tr.Key(0, ui.KeyRight)
	tr.Frame()
	if a.chosen != 1 {
		t.Fatalf("焦点在搜索框时 Right 之后 chosen=%d，应为 1", a.chosen)
	}
	tr.Key(0, ui.KeyDown)
	tr.Frame()
	if a.chosen != 1+panelCols {
		t.Fatalf("焦点在搜索框时 Down 之后 chosen=%d，应为 %d", a.chosen, 1+panelCols)
	}
}

// 首页卡片的名字要尽量完整显示：37 个字符的英文文件名在三行里能全放进去，
// 只有真正放不下的才加省略号。
func TestWrapName(t *testing.T) {
	if got := wrapName("微信", 76, 3); got != "微信" {
		t.Fatalf("短名字不该被折行: %q", got)
	}
	long := "SnowShot_Video_2026-10-29_13-31-04.mp4"
	got := wrapName(long, 76, 3)
	if strings.Contains(got, "…") {
		t.Fatalf("37 个字符的名字应当完整放进三行: %q", got)
	}
	if n := strings.Count(got, "\n") + 1; n != 3 {
		t.Fatalf("应当折成 3 行，实际 %d 行: %q", n, got)
	}
	if !strings.HasSuffix(strings.ReplaceAll(got, "\n", ""), ".mp4") {
		t.Fatalf("折行后不该丢字: %q", got)
	}
	if huge := wrapName(strings.Repeat("A", 60), 76, 3); !strings.Contains(huge, "…") {
		t.Fatalf("三行放不下的长名要加省略号: %q", huge)
	}
	// 中文按整字折，不能把一个汉字劈成两半。
	cn := "一个特别长的中文项目名字用来测试换行效果"
	if got := wrapName(cn, 76, 3); strings.Contains(got, "…") {
		t.Fatalf("20 个汉字应当完整放进三行: %q", got)
	}
}

// 搜索结果的鼠标点击要能打开：之前行上没有任何点击处理，点了像没反应。
func TestResultRowClickOpens(t *testing.T) {
	a := testApp(t)
	a.query = "demo"
	a.results = []search.Result{{
		Path: `D:\x\demo.txt`, Name: "demo.txt", Kind: search.KindFile, Hint: `D:\x`,
	}}
	a.chosen = 0
	stubAllIcons(a) // offline：open() 不再真去 Shell.OpenPath（会永久阻塞）
	tr := ui.NewTester(a.view, panelWidth, panelHeight)
	tr.Frame()

	rect, ok := tr.Find("demo.txt")
	if !ok {
		t.Fatal("渲染后找不到 demo.txt 这一行")
	}
	tr.ClickAt(rect.X+rect.W/2, rect.Y+rect.H/2)
	tr.Frame()
	if a.hist.Count(`D:\x\demo.txt`) == 0 {
		t.Fatal("点了结果行没有打开（历史里没有记一笔）")
	}
}

// 首页卡片的右键菜单要能删掉「最近使用」里的条目。
func TestHomeCardContextMenuDelete(t *testing.T) {
	a := testApp(t)
	for i := 0; i < 2; i++ {
		a.hist.TouchEntry(history.Entry{Path: fmt.Sprintf("p%d", i), Name: fmt.Sprintf("item%d", i), Kind: history.KindApp})
	}
	a.refilter()
	stubAllIcons(a)
	tr := ui.NewTester(a.view, panelWidth, panelHeight)
	tr.Frame()

	if err := tr.RightClick("item0"); err != nil {
		t.Fatalf("右键 item0: %v", err)
	}
	menu := tr.Menu()
	if len(menu) == 0 {
		t.Fatal("卡片上没有弹出右键菜单")
	}
	if err := tr.ChooseMenuItem("从历史删除"); err != nil {
		t.Fatalf("选「从历史删除」: %v", err)
	}
	tr.Frame() // applyPending 在下一帧开头执行
	if a.hist.Count("p0") != 0 {
		t.Fatal("删了之后历史里还有这条")
	}
	if tr.HasText("item0") {
		t.Fatal("首页还留着被删掉的卡片")
	}
	if !tr.HasText("item1") {
		t.Fatal("删一条却把别的也删了")
	}
}

// 固定项可以从菜单里取消固定，取消后回到「最近使用」。
func TestPinnedCardUnpin(t *testing.T) {
	a := testApp(t)
	a.hist.TouchEntry(history.Entry{Path: "p0", Name: "item0", Kind: history.KindApp})
	a.hist.TogglePin("p0")
	a.refilter()
	stubAllIcons(a)
	tr := ui.NewTester(a.view, panelWidth, panelHeight)
	tr.Frame()

	if err := tr.RightClick("item0"); err != nil {
		t.Fatalf("右键 item0: %v", err)
	}
	if err := tr.ChooseMenuItem("取消固定"); err != nil {
		t.Fatalf("选「取消固定」: %v", err)
	}
	tr.Frame()
	if a.hist.IsPinned("p0") {
		t.Fatal("取消固定没生效")
	}
	if a.pinnedCount != 0 {
		t.Fatalf("取消固定后 pinnedCount = %d，应为 0", a.pinnedCount)
	}
}

// Ctrl+Shift+C 复制的目录口径：目录给自身，文件给所在目录，虚拟项没有目录。
func TestDirOf(t *testing.T) {
	cases := []struct {
		path, name string
		kind       search.Kind
		want       string
	}{
		{`D:\work\baozi\README.md`, "README.md", search.KindFile, `D:\work\baozi`},
		{`D:\work\baozi`, "baozi", search.KindDir, `D:\work\baozi`},
		{"shell:AppsFolder", "某个应用", search.KindApp, ""},
		{"", "命令", search.KindCommand, ""},
	}
	for _, c := range cases {
		got := dirOf(search.Result{Path: c.path, Name: c.name, Kind: c.kind})
		if got != c.want {
			t.Fatalf("%q (%v) 的目录应为 %q，实际 %q", c.path, c.kind, c.want, got)
		}
	}
}

// 底栏要写出 Ctrl+Shift+C 这条键帽，否则用户不知道有这个动作。
func TestFooterShowsCopyPathKey(t *testing.T) {
	a := testApp(t)
	stubAllIcons(a)
	tr := ui.NewTester(a.view, panelWidth, panelHeight)
	tr.Frame()
	if !tr.HasText("Ctrl+Shift+C") {
		t.Fatal("底栏没有 Ctrl+Shift+C 复制路径的提示")
	}
}

// .exe/.lnk 虽然记成文件，但要取系统彩色图标（线框认不出是哪个程序）。
func TestIsProgram(t *testing.T) {
	cases := []struct {
		path, ext string
		kind      search.Kind
		want      bool
	}{
		{`D:\1tools\HiBitUninstaller.exe`, ".exe", search.KindFile, true},
		{`C:\Users\Administrator\Desktop\微信.lnk`, ".lnk", search.KindFile, true},
		{`D:\setup.msi`, ".msi", search.KindFile, true},
		{`D:\run.bat`, ".bat", search.KindFile, false}, // 齿轮图标，保持线框
		{`D:\work\baozi\README.md`, ".md", search.KindFile, false},
		{"shell:AppsFolder\\某个应用", "", search.KindApp, false}, // 应用本来就走彩色
		{`D:\work\baozi`, "", search.KindDir, false},
	}
	for _, c := range cases {
		got := isProgram(search.Result{Path: c.path, Ext: c.ext, Kind: c.kind})
		if got != c.want {
			t.Fatalf("%q (%s, kind=%v) 应返回 %v，实际 %v", c.path, c.ext, c.kind, c.want, got)
		}
	}
}
