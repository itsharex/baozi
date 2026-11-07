package pinyin

import "testing"

func TestInitials(t *testing.T) {
	cases := []struct{ in, want string }{
		{"豆包工作", "dbgz"},
		{"豆包", "db"},
		{"计算器", "jsq"},
		{"微信", "wx"},
		{"豆包工作 (Beta)", "dbgzbeta"},
		{"Chrome", ""},        // 纯 ASCII 不转换
		{"合同.docx", "htdocx"}, // 混排：汉字取首字母，扩展名原样
	}
	for _, c := range cases {
		if got := Initials(c.in); got != c.want {
			t.Errorf("Initials(%q) = %q, 期望 %q", c.in, got, c.want)
		}
	}
}

func TestFull(t *testing.T) {
	if got := Full("豆包工作"); got != "doubaogongzuo" {
		t.Fatalf("Full(豆包工作) = %q", got)
	}
	if got := Full("Chrome"); got != "" {
		t.Fatalf("纯 ASCII 不该返回全拼，得到 %q", got)
	}
}

func TestCached(t *testing.T) {
	a := Initials("计算器")
	b := Initials("计算器")
	if a != b || a == "" {
		t.Fatalf("缓存前后不一致: %q vs %q", a, b)
	}
}
