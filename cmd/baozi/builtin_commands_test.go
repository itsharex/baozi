package main

import (
	"path/filepath"
	"strings"
	"testing"

	"baozi/internal/builtin"
	"baozi/internal/config"
)

// 配置里写的是 builtin:shutdown，运行时才换成落盘路径再包 powershell。
func TestLaunchSpecBuiltinShutdown(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	prog, args := launchSpec(builtin.ShutdownRef, []string{"30"})
	if !strings.Contains(strings.ToLower(filepath.Base(prog)), "powershell.exe") {
		t.Fatalf(".ps1 应该由 powershell 跑，实际 %q", prog)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, builtin.ShutdownName) {
		t.Fatalf("参数里没有内置脚本路径：%v", args)
	}
	if !strings.Contains(joined, "-File") || !strings.HasSuffix(joined, "30") {
		t.Fatalf("参数包装不对：%v", args)
	}
}

// 启动时把指向旧外部路径的命令改成 builtin:shutdown，并报告"配置改了"。
func TestEnsureBuiltinCommandsMigrates(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	cfg := config.Default()
	cfg.Commands = []config.Command{
		{Keyword: "关机", Name: "关机", Exec: `D:\1tools\scripts\shutdown-after.ps1`, Args: []string{"%1"}},
		{Keyword: "term", Name: "终端", Exec: `C:\Windows\System32\cmd.exe`, Args: []string{"/c"}},
	}
	if !ensureBuiltinCommands(cfg) {
		t.Fatal("改了 exec 却没报告变化")
	}
	if cfg.Commands[0].Exec != builtin.ShutdownRef {
		t.Fatalf("没迁移成内置引用：%s", cfg.Commands[0].Exec)
	}
	if cfg.Commands[1].Exec != `C:\Windows\System32\cmd.exe` {
		t.Fatalf("无关命令被改动了：%s", cfg.Commands[1].Exec)
	}
	// 已经迁移过的再跑一次不该再报变化。
	if ensureBuiltinCommands(cfg) {
		t.Fatal("第二次调用不该再有变化")
	}
}

// 一条命令都没有时（比如被误保存清空过），补上三条电源命令。
func TestEnsureBuiltinCommandsSeeds(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	cfg := config.Default()
	cfg.Commands = nil // config.Default 自带三条常用命令，这里模拟"清单被清空"
	if !ensureBuiltinCommands(cfg) {
		t.Fatal("空清单应该被补上默认命令")
	}
	if len(cfg.Commands) != 3 {
		t.Fatalf("补了 %d 条，期望 3 条", len(cfg.Commands))
	}
	for _, c := range cfg.Commands {
		if c.Exec != builtin.ShutdownRef {
			t.Fatalf("默认命令没指向内置脚本：%+v", c)
		}
	}
	if cfg.Commands[0].Keyword != "关机" || cfg.Commands[1].Keyword != "睡眠" {
		t.Fatalf("顺序不对：%+v", cfg.Commands)
	}
}
