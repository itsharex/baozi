package main

import (
	"strings"
	"testing"

	"baozi/internal/config"
	"baozi/internal/search"
)

// 占位符：%1…%9 取第几个词，%* 取全部，{dir} 取跟随目录，
// 没写占位符就走"追加到末尾"的老行为。
func TestExpandArgsPlaceholders(t *testing.T) {
	dir := `D:\1tools`
	cases := []struct {
		name  string
		args  []string
		words []string
		want  []string
	}{
		{"无占位符：追加", []string{"-a"}, []string{"x", "y"}, []string{"-a", "x", "y"}},
		{"无占位符且没参数", nil, []string{"x"}, []string{"x"}},
		{"%1", []string{"%1"}, []string{"30"}, []string{"30"}},
		{"%1 %2 分开", []string{"%1", "%2"}, []string{"30", "40"}, []string{"30", "40"}},
		{"%* 合并成一条", []string{"%*"}, []string{"a", "b"}, []string{"a b"}},
		{"越界的 %3 变空串", []string{"%3"}, []string{"only"}, []string{""}},
		{"{dir}", []string{"{dir}"}, nil, []string{dir}},
		{"带前缀的占位符", []string{"--out=%1"}, []string{"x"}, []string{"--out=x"}},
		{"不是占位符的 %", []string{"100%"}, nil, []string{"100%"}},
	}
	for _, tc := range cases {
		got := expandArgs(tc.args, tc.words, dir)
		if len(got) != len(tc.want) {
			t.Fatalf("%s：期望 %d 个参数 %v，实际 %d 个 %v", tc.name, len(tc.want), tc.want, len(got), got)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("%s：第 %d 个参数期望 %q，实际 %q（全量 %v）", tc.name, i, tc.want[i], got[i], got)
			}
		}
	}
}

// 激活词：主词与别名都算命中，且必须是整词。
func TestCommandWordsMatchesAliases(t *testing.T) {
	cmd := config.Command{Keyword: "关机", Aliases: []string{"shut", "shutdown"}}
	cases := []struct {
		q     string
		hit   bool
		words []string
	}{
		{"关机", true, nil},
		{"关机 30", true, []string{"30"}},
		{"shut", true, nil},
		{"shut 45", true, []string{"45"}},
		{"shutdown 90", true, []string{"90"}},
		{"SHUT 45", true, []string{"45"}}, // 大小写不敏感
		{"关", false, nil},
		{"shu", false, nil},
		{"关机了", false, nil},
		{"shutdownx 1", false, nil},
	}
	for _, tc := range cases {
		words, ok := commandWords(cmd, tc.q)
		if ok != tc.hit {
			t.Fatalf("%q 命中期望 %v，实际 %v", tc.q, tc.hit, ok)
		}
		if !tc.hit {
			continue
		}
		if strings.Join(words, "|") != strings.Join(tc.words, "|") {
			t.Fatalf("%q 的参数期望 %v，实际 %v", tc.q, tc.words, words)
		}
	}
}

// 同一条命令用占位符写参数时，用户多敲的词不该再追加一遍。
func TestCommandResultUsesPlaceholders(t *testing.T) {
	a := testApp(t)
	a.cfg.Commands = []config.Command{{
		Keyword: "关机",
		Aliases: []string{"shut"},
		Name:    "定时关机",
		Exec:    "powershell.exe",
		Args:    []string{"-File", `D:\1tools\scripts\shutdown-after.ps1`, "%1"},
		Dir:     "{dir}",
	}}
	cmd := firstCommand(a.searchAll("shut 30"))
	if cmd == nil {
		t.Fatal("别名 shut 没命中命令")
	}
	want := []string{"-File", `D:\1tools\scripts\shutdown-after.ps1`, "30"}
	if strings.Join(cmd.Args, "|") != strings.Join(want, "|") {
		t.Fatalf("占位符展开错误：期望 %v，实际 %v", want, cmd.Args)
	}
	if len(cmd.Args) != 3 {
		t.Fatalf("占位符模式不该追加多余的词，实际 %v", cmd.Args)
	}
}

func firstCommand(rs []search.Result) *search.Result {
	for i := range rs {
		if rs[i].Kind == search.KindCommand {
			return &rs[i]
		}
	}
	return nil
}
