package search

import "testing"

// TestRankOrder 验证用户定的优先级：频次 > 类型 > 准确性。
func TestRankOrder(t *testing.T) {
	w := Weights{Frequency: 0.5, Type: 0.3, Accuracy: 0.2}
	order := []string{".exe", ".lnk", ".msc"}

	// 同类型同准确度，频次高的排前面
	a := Result{Item: Item{Name: "note.txt", Ext: ".txt"}, Frequency: 0.9, TypeScore: 0.2, Accuracy: 0.78}
	b := Result{Item: Item{Name: "note2.txt", Ext: ".txt"}, Frequency: 0.1, TypeScore: 0.2, Accuracy: 0.78}
	a.Score = Combine(w, a.Frequency, a.TypeScore, a.Accuracy)
	b.Score = Combine(w, b.Frequency, b.TypeScore, b.Accuracy)
	if !Less(a, b) {
		t.Fatal("频次高的应当在前")
	}

	// 频次相同：可执行文件要压过准确度更高但类型普通的文件
	exe := Result{Item: Item{Name: "tool.exe", Ext: ".exe"}, Frequency: 0, TypeScore: TypeScore(".exe", KindFile, order), Accuracy: 0.78}
	txt := Result{Item: Item{Name: "tool-manual.txt", Ext: ".txt"}, Frequency: 0, TypeScore: TypeScore(".txt", KindFile, order), Accuracy: 0.95}
	exe.Score = Combine(w, exe.Frequency, exe.TypeScore, exe.Accuracy)
	txt.Score = Combine(w, txt.Frequency, txt.TypeScore, txt.Accuracy)
	if !Less(exe, txt) {
		t.Fatalf("exe(%.3f) 应当排到 txt(%.3f) 前面", exe.Score, txt.Score)
	}

	// 类型也相同：准确度高的在前
	near := Result{Item: Item{Name: "report.docx"}, TypeScore: 0.2, Accuracy: 0.78}
	exact := Result{Item: Item{Name: "report.docx"}, TypeScore: 0.2, Accuracy: 1.0}
	near.Score = Combine(w, 0, near.TypeScore, near.Accuracy)
	exact.Score = Combine(w, 0, exact.TypeScore, exact.Accuracy)
	if !Less(exact, near) {
		t.Fatal("准确度高的应当在前")
	}
}

// TestBalanceBreaksAppRun 是对"只显示应用"的回归测试：
// 8 个应用 + 3 个文件，交错后应用最多连续 3 条，文件必须出现在前 6 条里。
func TestBalanceBreaksAppRun(t *testing.T) {
	var in []Result
	for i := 0; i < 8; i++ {
		in = append(in, Result{Item: Item{Name: "app", Kind: KindApp}, Score: 1 - float64(i)/100})
	}
	for i := 0; i < 3; i++ {
		in = append(in, Result{Item: Item{Name: "file.txt", Kind: KindFile}, Score: 0.1})
	}

	got := Balance(in, 3)
	if len(got) != len(in) {
		t.Fatalf("不应丢结果：%d != %d", len(got), len(in))
	}
	// 前 8 条里必须出现文件
	files := 0
	for _, r := range got[:8] {
		if r.Kind != KindApp {
			files++
		}
	}
	if files == 0 {
		t.Fatal("文件被应用挤光了")
	}
	// 不允许出现 4 条连续应用
	run := 0
	for _, r := range got {
		if r.Kind == KindApp {
			run++
			if run > 3 {
				t.Fatalf("应用连续 %d 条，超过上限", run)
			}
		} else {
			run = 0
		}
	}
	// 应用内部顺序保持不变
	var apps []Result
	for _, r := range in {
		if r.Kind == KindApp {
			apps = append(apps, r)
		}
	}
	var gotApps []Result
	for _, r := range got {
		if r.Kind == KindApp {
			gotApps = append(gotApps, r)
		}
	}
	for i := range apps {
		if gotApps[i].Score != apps[i].Score {
			t.Fatalf("应用相对顺序被打乱：第 %d 条", i)
		}
	}
}

// 只有一类结果时不该乱动。
func TestBalanceNoopSingleKind(t *testing.T) {
	in := []Result{
		{Item: Item{Kind: KindApp}, Score: 1},
		{Item: Item{Kind: KindApp}, Score: 0.9},
	}
	got := Balance(in, 2)
	if len(got) != 2 || got[0].Score != 1 || got[1].Score != 0.9 {
		t.Fatalf("单一类型不该重排: %+v", got)
	}
	if got := Balance(in, 0); len(got) != 2 {
		t.Fatal("maxRun=0 应原样返回")
	}
}

func TestTypeScoreOrder(t *testing.T) {
	order := []string{".exe", ".lnk", ".txt"}
	if got := TypeScore(".exe", KindFile, order); got != 0.90 {
		t.Fatalf("类型顺序第一档应为 0.90，得到 %.2f", got)
	}
	if got := TypeScore(".lnk", KindFile, order); got >= 0.90 || got <= TypeScore(".txt", KindFile, order) {
		t.Fatalf(".lnk 应低于第一档、但要高于 .txt，得到 %.2f", got)
	}
	if got := TypeScore(".zip", KindFile, order); got != 0.2 {
		t.Fatalf("未列入的类型应为 0.2，得到 %.2f", got)
	}
	if got := TypeScore("", KindDir, order); got != 0.60 {
		t.Fatalf("目录应为 0.60，得到 %.2f", got)
	}
	if got := TypeScore(".exe", KindApp, order); got != 1.0 {
		t.Fatalf("应用应为 1.0，得到 %.2f", got)
	}
}

// TestAppBeatsExecutable 是用户明确要的"开始菜单应用排最前"：
// 应用和 .exe 频次、准确度完全相同时，应用必须在前面。
func TestAppBeatsExecutable(t *testing.T) {
	w := Weights{Frequency: 0.5, Type: 0.3, Accuracy: 0.2}
	order := []string{".exe", ".lnk"}

	app := Result{Item: Item{Name: "豆包工作", Kind: KindApp}, Frequency: 0.4, TypeScore: TypeScore("", KindApp, order), Accuracy: 0.97}
	exe := Result{Item: Item{Name: "doubao.exe", Kind: KindFile, Ext: ".exe"}, Frequency: 0.4, TypeScore: TypeScore(".exe", KindFile, order), Accuracy: 0.97}
	app.Score = Combine(w, app.Frequency, app.TypeScore, app.Accuracy)
	exe.Score = Combine(w, exe.Frequency, exe.TypeScore, exe.Accuracy)
	if !Less(app, exe) {
		t.Fatalf("应用(%.3f) 应排在 exe(%.3f) 前面", app.Score, exe.Score)
	}
}

// TestBandOrder 锁死段顺序：应用 < 命令 < 强匹配 < 中匹配 < 弱匹配。
func TestBandOrder(t *testing.T) {
	bApp := BandOf(0.40, KindApp, 0)
	bCmd := BandOf(1.00, KindCommand, 0)
	bStrong := BandOf(0.95, KindFile, 0)
	bMedium := BandOf(0.78, KindFile, 0)
	bWeak := BandOf(0.40, KindFile, 0)
	if !(bApp < bCmd && bCmd < bStrong && bStrong < bMedium && bMedium < bWeak) {
		t.Fatalf("段顺序不对：app=%d cmd=%d strong=%d medium=%d weak=%d", bApp, bCmd, bStrong, bMedium, bWeak)
	}
}

// TestBandKeepsAppAboveHistoryFile 是这次改造的靶心。用户的原话：
// "搜索结果会导致大量无用的文件排列在搜索结果前列，根本找不到自己真正需要的app。"
// 这里是最狠的一档：一条名字全等、还天天在用的文件，也不能盖过一个只是弱命中的应用。
func TestBandKeepsAppAboveHistoryFile(t *testing.T) {
	w := Weights{Frequency: 0.5, Type: 0.3, Accuracy: 0.2}
	order := []string{".exe", ".lnk"}

	// 名字全等 + 打开过很多次：放在老口径下能拿满频次分。
	file := Rank(w, order, Result{
		Item:      Item{Name: "qq", Path: `D:\work\qq`, Ext: ".txt", Kind: KindFile},
		Frequency: 1.0, Opens: 20, Accuracy: 1.0,
	})
	// 只看得出"名字里有 qq" 的应用。
	app := Rank(w, order, Result{
		Item:      Item{Name: "QQ音乐", Path: `C:\QQMusic\QQMusic.exe`, Ext: ".exe", Kind: KindApp},
		Frequency: 0, Opens: 0, Accuracy: 0.66,
	})

	if app.Band != BandApp {
		t.Fatalf("应用应固定在 BandApp，得到 %d", app.Band)
	}
	if file.Band == BandApp {
		t.Fatal("文件不该进应用段")
	}
	if !Less(app, file) {
		t.Fatalf("应用(Band %d, %.3f) 应排在文件(Band %d, %.3f) 前面",
			app.Band, app.Score, file.Band, file.Score)
	}
}

// TestMatchQualityBeatsType 是实测抓到的第二个毛病：
// 搜 code 时前 8 条全是 C:\Program Files 里的 wsdcodegen.exe / frcode.exe，
// 它们是"名字包含 code"（0.78），靠 .exe 的类型分 0.90 压过"前缀匹配"的项目文件。
// 匹配质量必须在类型之前。
func TestMatchQualityBeatsType(t *testing.T) {
	w := Weights{Frequency: 0.5, Type: 0.3, Accuracy: 0.2}
	order := []string{".exe", ".lnk"}

	// 包含匹配的第三方工具程序：类型分高、准确度低。
	junk := Rank(w, order, Result{
		Item:     Item{Name: "wsdcodegen.exe", Path: `C:\Program Files (x86)\Windows Kits\10\bin\wsdcodegen.exe`, Ext: ".exe", Kind: KindFile},
		Accuracy: 0.78,
	})
	// 前缀匹配的项目文件：类型分低、准确度高。
	doc := Rank(w, order, Result{
		Item:     Item{Name: "code-style.md", Path: `D:\work\docs\code-style.md`, Ext: ".md", Kind: KindFile},
		Accuracy: 0.95,
	})
	if !Less(doc, junk) {
		t.Fatalf("前缀匹配的项目文件(Band %d, %.3f) 应排在包含匹配的 exe(Band %d, %.3f) 前面",
			doc.Band, doc.Score, junk.Band, junk.Score)
	}
}

// TestEscalation：中匹配但常用 → 抬到强档；弱匹配不许借频次上抬。
func TestEscalationRaisesFamiliarFile(t *testing.T) {
	w := Weights{Frequency: 0.5, Type: 0.3, Accuracy: 0.2}
	order := []string{".exe"}

	familiar := Rank(w, order, Result{
		Item:      Item{Name: "月度复盘.md", Path: `D:\doc\月度复盘.md`, Ext: ".md", Kind: KindFile},
		Frequency: 0.9, Opens: 5, Accuracy: 0.78,
	})
	if familiar.Band != BandStrong {
		t.Fatalf("常用中匹配应抬到 BandStrong，得到 %d", familiar.Band)
	}
	cold := Rank(w, order, Result{
		Item:      Item{Name: "月度复盘.md", Path: `D:\doc\月度复盘.md`, Ext: ".md", Kind: KindFile},
		Frequency: 0.4, Opens: 2, Accuracy: 0.78,
	})
	if cold.Band != BandMedium {
		t.Fatalf("只开过 2 次不该抬档，得到 %d", cold.Band)
	}
	weak := Rank(w, order, Result{
		Item:      Item{Name: "会议纪要.md", Path: `D:\doc\会议纪要.md`, Ext: ".md", Kind: KindFile},
		Frequency: 1.0, Opens: 12, Accuracy: 0.40,
	})
	if weak.Band != BandWeak {
		t.Fatalf("弱命中不该抬档，得到 %d", weak.Band)
	}
}

// TestNoise：垃圾后缀与缓存目录降一档、段内降权；Program Files 不算噪声。
func TestNoise(t *testing.T) {
	w := Weights{Frequency: 0.5, Type: 0.3, Accuracy: 0.2}
	order := []string{".log", ".exe"} // 故意把 .log 放进类型顺序

	noisy := Rank(w, order, Result{
		Item:      Item{Name: "app.log", Path: `D:\work\app.log`, Ext: ".log", Kind: KindFile},
		Frequency: 1.0, Accuracy: 0.95,
	})
	if noisy.Band != BandMedium {
		t.Fatalf(".log 应被降一档到 BandMedium，得到 %d", noisy.Band)
	}
	clean := Rank(w, order, Result{
		Item:      Item{Name: "app.txt", Path: `D:\work\app.txt`, Ext: ".txt", Kind: KindFile},
		Frequency: 1.0, Accuracy: 0.95,
	})
	if clean.Band != BandStrong {
		t.Fatalf("同样参数的非噪声文件应在 BandStrong，得到 %d", clean.Band)
	}
	if !Less(clean, noisy) {
		t.Fatalf("非噪声(%.3f) 应排在噪声(%.3f) 前面", clean.Score, noisy.Score)
	}

	for _, p := range []string{
		`C:\Users\x\AppData\Local\Temp\a.txt`,
		`D:\proj\node_modules\a.js`,
		`C:\Users\x\.bun\install\cache\a.exe`,
		`D:\work\vendor\pkg\a.go`,
		`C:\Windows\System32\a.txt`,
	} {
		if !IsNoisy(".txt", p) {
			t.Fatalf("%s 应判为噪声路径", p)
		}
	}
	// Program Files 里是用户真正会用的程序，不许整片拉黑。
	if IsNoisy(".exe", `C:\Program Files\Git\bin\git.exe`) {
		t.Fatal("Program Files 不该判噪声")
	}
	if IsNoisy(".txt", `D:\work\readme.txt`) {
		t.Fatal("普通工作目录不该判噪声")
	}
}
