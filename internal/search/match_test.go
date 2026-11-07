package search

import (
	"testing"
)

func TestAccuracyTiers(t *testing.T) {
	cases := []struct {
		query, name, path string
		wantMin           float64
		wantOK            bool
	}{
		{"report", "report", "", 1.0, true},
		{"report", "report-2026.docx", "", 0.95, true},
		{"report", "my-report.docx", "", 0.88, true},
		{"report", "my_report_final.docx", "", 0.78, true},
		{"report", "photo.jpg", `D:\docs\report\`, 0.62, true},
		{"vsc", "Visual Studio Code", "", 0.35, true},
		{"vsc", "photo.jpg", "", 0, false},
		{"报表", "月度报表.xlsx", "", 0.78, true},
	}
	for _, c := range cases {
		acc, ok := Accuracy(c.query, c.name, c.path)
		if ok != c.wantOK {
			t.Fatalf("%q vs %q: ok=%v want %v", c.query, c.name, ok, c.wantOK)
		}
		if ok && acc < c.wantMin-0.001 {
			t.Fatalf("%q vs %q: acc=%.3f want >= %.3f", c.query, c.name, acc, c.wantMin)
		}
	}
}

func TestAccuracyMultipleTokens(t *testing.T) {
	// 多关键词是 AND，且取最差的那个 token 作为整体准确度。
	acc, ok := Accuracy("proj report", "report-proj-final.xlsx", "")
	if !ok {
		t.Fatal("应当命中")
	}
	if acc < 0.87 || acc > 0.89 {
		t.Fatalf("acc=%.3f，期望词边界级别的 0.88", acc)
	}
	if _, ok := Accuracy("proj photo", "report-proj.xlsx", ""); ok {
		t.Fatal("缺一个 token 就不该命中")
	}
}

// TestPinyinAccuracy 覆盖"敲拼音首字母找中文名"：
// 拼音全等要压过 ASCII 散落子序列（0.35），否则真命中会被弱命中带偏。
func TestPinyinAccuracy(t *testing.T) {
	cases := []struct {
		query, name, py, full string
		wantMin               float64
		wantOK                bool
	}{
		{"dbgz", "豆包工作", "dbgz", "doubaogongzuo", 0.95, true},
		{"dbg", "豆包工作", "dbgz", "doubaogongzuo", 0.88, true},
		{"doubaogongzuo", "豆包工作", "dbgz", "doubaogongzuo", 0.93, true},
		{"doubao", "豆包工作", "dbgz", "doubaogongzuo", 0.85, true},
		{"gz", "豆包工作", "dbgz", "", 0.45, true}, // 中间段：散落匹配
		{"xyz", "豆包工作", "dbgz", "doubaogongzuo", 0, false},
		{"dbgz", "Another App", "", "", 0, false}, // 纯 ASCII 名不吃拼音
	}
	for _, c := range cases {
		acc, ok := AccuracyPy(c.query, c.name, "", c.py, c.full)
		if ok != c.wantOK {
			t.Fatalf("%q vs %q: ok=%v want %v", c.query, c.name, ok, c.wantOK)
		}
		if ok && acc < c.wantMin-0.001 {
			t.Fatalf("%q vs %q: acc=%.3f want >= %.3f", c.query, c.name, acc, c.wantMin)
		}
	}
}

// 拼音精确命中必须赢过"名字里恰好散落着这些字母"的弱命中。
func TestPinyinBeatsWeakAsciiMatch(t *testing.T) {
	pinyinAcc, _ := AccuracyPy("dbgz", "豆包工作", "", "dbgz", "")
	weakAcc, weakOK := AccuracyPy("dbgz", "daily-build-gz.log", "", "", "")
	if !weakOK || weakAcc >= pinyinAcc {
		t.Fatalf("拼音(%.2f) 应高于弱命中的 ASCII 名(%.2f, ok=%v)", pinyinAcc, weakAcc, weakOK)
	}
}
