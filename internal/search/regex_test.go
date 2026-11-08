package search

import (
	"testing"
)

// 预筛绝不能比真匹配更严：对每个模式，凡"Maybe 判死"的名字，Match 也必须不命中。
// 这是这套加速唯一的正确性约束，所以把它做成表驱动 + 一批刁钻名字。
func TestRegexScreenNeverTooStrict(t *testing.T) {
	samples := []string{
		"a.log", "b.LOG", "dot.log.bak", "log", "logs.txt", "rpt", "backup",
		"backup.tar", "2026-report.jpg", "2026-report.png", "2026.log",
		"Äpfel.log", "äpfel.log", "中文.log", "x", "", ".log", "abc",
		"System32\\drivers\\etc\\hosts", "users\\Ä", "report2026",
	}

	patterns := []string{
		`^.*\.log$`,
		`\d+\.log$`,
		`^2026.*\.(jpg|png)$`,
		`rpt|backup`,
		`(jpg|png)$`,
		`^log`,
		`(?i)äpfel`,
		`(?i)LOG$`,
		`a?b`,
		`a*b`,
		`.`,
		``,
		`^(a|b)+\.log`,
		`[^x]`,
		`^中文`,
	}

	for _, pat := range patterns {
		rx, err := CompileRegex(pat)
		if err != nil {
			t.Fatalf("%q 编译失败: %v", pat, err)
		}
		for _, s := range samples {
			if !rx.Maybe([]byte(s)) && rx.Match([]byte(s)) {
				t.Fatalf("%q: 预筛把真命中的 %q 筛掉了", pat, s)
			}
		}
	}
}

func TestRegexMatch(t *testing.T) {
	cases := []struct {
		pat, name string
		want      bool
	}{
		{`^.*\.log$`, "a.log", true},
		{`^.*\.log$`, "a.txt", false},
		{`^.*\.log$`, "dot.log.bak", false},
		{`\.log$`, "dot.log.bak", false},
		{`^2026.*\.png$`, "2026-report.png", true},
		{`^2026.*\.png$`, "2027-report.png", false},
		{`rpt|backup`, "rpt", true},
		{`rpt|backup`, "backup.tar", true},
		{`rpt|backup`, "report", false},
		{`(?i)LOG`, "my.LOG", true},
		{`\.log`, "my.LOG", false}, // 正则默认区分大小写，用户要忽略就自己写 (?i)
		{`^中文$`, "中文", true},
	}
	for _, c := range cases {
		rx, err := CompileRegex(c.pat)
		if err != nil {
			t.Fatalf("%q 编译失败: %v", c.pat, err)
		}
		if got := rx.Match([]byte(c.name)); got != c.want {
			t.Fatalf("%q vs %q: 期望 %v，实际 %v", c.pat, c.name, c.want, got)
		}
	}
}

func TestRegexInvalidPattern(t *testing.T) {
	for _, bad := range []string{`(`, `[a-`, `*`, `a{2,1}`} {
		if _, err := CompileRegex(bad); err == nil {
			t.Fatalf("%q 是非法正则，应当报错", bad)
		}
	}
}

// 预筛求出来的条件要真的省事：典型的扩展名查询必须拿到 ".log" 这类字面。
func TestRegexScreenPicksUsefulLiteral(t *testing.T) {
	cases := []struct{ pat, want string }{
		{`^.*\.log$`, ".log"},
		{`\d+\.log$`, ".log"},
		{`^2026.*\.(jpg|png)$`, "2026"},
		{`rpt|backup`, "backup"}, // 分支取最强的那一条
		{`^\.(jpg)$`, "jpg"},     // 单字符字面（那个点）不并入 AND，只剩 jpg
		{`.`, ""},                // 全是元字符，求不出
		{`(?i)LOG$`, "log"},      // FoldCase 的 ASCII 字面仍可用（预筛更宽松）
	}
	for _, c := range cases {
		rx, err := CompileRegex(c.pat)
		if err != nil {
			t.Fatalf("%q 编译失败: %v", c.pat, err)
		}
		// 条件可能拆成多组（分支），只要字面在任意一组里就算求出来了。
		found := ""
		for _, g := range rx.screen {
			for _, lit := range g {
				if len(lit) > len(found) {
					found = lit
				}
			}
		}
		if found != c.want {
			t.Fatalf("%q: 预筛字面期望 %q，实际 %q（全部 %v）", c.pat, c.want, found, rx.screen)
		}
	}
}

// 忽略大小写的非 ASCII 字面必须放弃（Go 正则会折叠 ä/Ä，预筛只折 ASCII）。
func TestRegexScreenSkipsNonASCIIFoldCase(t *testing.T) {
	rx, err := CompileRegex(`(?i)Äpfel`)
	if err != nil {
		t.Fatalf("编译失败: %v", err)
	}
	if len(rx.screen) != 0 {
		t.Fatalf("非 ASCII + 忽略大小写不该拿来预筛，实际 %v", rx.screen)
	}
	if !rx.Match([]byte("äpfel")) {
		t.Fatal("正则本身应当命中 äpfel")
	}
}

func TestRegexMaybeEmptyPatternAlwaysPasses(t *testing.T) {
	rx, err := CompileRegex("")
	if err != nil {
		t.Fatalf("空模式: %v", err)
	}
	if !rx.Maybe([]byte("anything")) {
		t.Fatal("空模式没有预筛条件，应当一律放行")
	}
	if !rx.Match([]byte("anything")) {
		t.Fatal("空正则可以匹配任何字符串")
	}
	if len(rx.screen) != 0 {
		t.Fatalf("空模式不该产生预筛条件：%v", rx.screen)
	}
}
