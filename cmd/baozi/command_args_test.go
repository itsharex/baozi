package main

import (
	"testing"

	"baozi/internal/config"
)

// 「关机 30」这类带参数的自定义命令：关键字整词命中，空格后面的词追加成参数。
// 这里守的是参数链路 —— 分钟数必须原样落到 Args 末尾，不能被吞掉或被当名字匹配。
func TestShutdownCommandKeepsMinutes(t *testing.T) {
	a := testApp(t)
	base := []string{
		"-NoProfile", "-ExecutionPolicy", "Bypass", "-WindowStyle", "Hidden",
		"-File", `D:\1tools\scripts\shutdown-after.ps1`,
	}
	a.cfg.Commands = []config.Command{
		{Keyword: "关机", Name: "定时关机", Exec: "powershell.exe", Args: base},
		{Keyword: "shut", Name: "定时关机", Exec: "powershell.exe", Args: base},
		{Keyword: "shutdown", Name: "定时关机", Exec: "powershell.exe", Args: base},
	}

	for _, q := range []string{"关机 30", "shut 30", "shutdown 30"} {
		cmd := firstCommand(a.searchAll(q))
		if cmd == nil {
			t.Fatalf("%q 没有命中任何自定义命令", q)
		}
		if got := cmd.Args; len(got) != len(base)+1 || got[len(got)-1] != "30" {
			t.Fatalf("%q 的参数应是 base+30，实际 %v", q, got)
		}
		if cmd.Command != "powershell.exe" {
			t.Fatalf("%q 的程序应是 powershell.exe，实际 %q", q, cmd.Command)
		}
	}

	// 裸关键字：命令也会命中，但没有任何追加参数 —— 脚本这时只弹用法、不会关机。
	cmd := firstCommand(a.searchAll("关机"))
	if cmd == nil {
		t.Fatal("裸「关机」应命中命令")
	}
	if len(cmd.Args) != len(base) {
		t.Fatalf("不带分钟数时不该追加参数，实际 %v", cmd.Args)
	}

	// 参数必须能被数字解析（脚本按分钟算秒），这里顺带守住"多段追加"的行为。
	cmd = firstCommand(a.searchAll("关机 30 40"))
	if cmd == nil {
		t.Fatal("多参数时也应命中命令")
	}
	if got := cmd.Args[len(base):]; len(got) != 2 || got[0] != "30" || got[1] != "40" {
		t.Fatalf("两个后续词应各自成为一个参数，实际 %v", got)
	}
}
