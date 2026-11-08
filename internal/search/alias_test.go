package search

import (
	"strings"
	"testing"
)

// appAlias 只认 ID 最后一段的产品名：整条路径参与匹配会把 AppData\Local
// 里的 "Local" 当成 "cal" 命中（Lo-cal）。
func TestAppAliasStripsNoise(t *testing.T) {
	cases := []struct{ name, id, want string }{
		{"开始菜单里的磁盘 exe", `C:\Users\Administrator\AppData\Local\nvm\nvm.exe`, "nvm"},
		{"python.exe", `C:\Users\Administrator\AppData\Local\Python\pythoncore-3.11-64\python.exe`, "python"},
		{"UWP 的 AUMID", `Microsoft.WindowsCalculator_8wekyb3d8bbwe!App`, "Microsoft WindowsCalculator"},
		{"带版本号的桌面 AUMID", `Doubao.Office_1.0.0.0_x64!App`, "Doubao Office 1 0 0 0 x64"},
		{"空 ID", "", ""},
	}
	for _, c := range cases {
		if got := AppAlias(c.id); got != c.want {
			t.Fatalf("%s：AppAlias(%q)=%q，期望 %q", c.name, c.id, got, c.want)
		}
	}
}

// 关键回归：搜 cal 时 nvm / Python 不该被别名命中，计算器该命中。
func TestAppAliasMatchesCal(t *testing.T) {
	if got := AppAlias(`C:\Users\Administrator\AppData\Local\nvm\nvm.exe`); strings.Contains(strings.ToLower(got), "cal") {
		t.Fatalf("nvm 的别名含 cal：%q", got)
	}
	if got := AppAlias(`C:\Users\Administrator\AppData\Local\Python\pythoncore-3.11-64\python.exe`); strings.Contains(strings.ToLower(got), "cal") {
		t.Fatalf("python 的别名含 cal：%q", got)
	}
	if got := AppAlias(`Microsoft.WindowsCalculator_8wekyb3d8bbwe!App`); !strings.Contains(strings.ToLower(got), "cal") {
		t.Fatalf("计算器的别名应含 cal：%q", got)
	}
}
