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
