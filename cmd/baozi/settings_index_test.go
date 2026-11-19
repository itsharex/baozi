package main

import "testing"

// 排除规则改没改，决定保存后要不要后台重建索引。这里钉住三种"看起来改了、
// 其实语义没变"的编辑：换顺序、多敲空行、改大小写 —— 它们都不该触发一次
// 分钟级的全量重建；真的加/删/改一条规则时必须报出来。
func TestExcludesChanged(t *testing.T) {
	base := []string{"node_modules", `D:\Temp`, "\\.git\\"}

	cases := []struct {
		name string
		old  []string
		want []string
		ret  bool
	}{
		{"完全一样", base, []string{"node_modules", `D:\Temp`, "\\.git\\"}, false},
		{"换顺序", base, []string{"\\.git\\", "node_modules", `D:\Temp`}, false},
		{"多空行和空格", base, []string{"", " node_modules ", `D:\Temp`, "\\.git\\", "   "}, false},
		{"改大小写", base, []string{"NODE_MODULES", `d:\temp`, "\\.GIT\\"}, false},
		{"加一条", base, append([]string{"dist"}, base...), true},
		{"删一条", base, base[1:], true},
		{"改内容", base, []string{"node_modules", `D:\Tmp`, "\\.git\\"}, true},
		{"全清空", base, nil, true},
	}
	for _, c := range cases {
		if got := excludesChanged(c.old, c.want); got != c.ret {
			t.Errorf("%s：excludesChanged=%v，想要 %v", c.name, got, c.ret)
		}
	}
}
