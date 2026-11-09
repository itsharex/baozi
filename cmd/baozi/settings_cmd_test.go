package main

import (
	"testing"

	"github.com/egoist/mygo/ui"

	"baozi/internal/config"
)

// 命令编辑页：左边清单、右边表单。这里守"该有的栏位都在"，
// 布局是否挤压靠 BAOZI_PREVIEW 的实拍看。
func TestSettingsCommandsPageRenders(t *testing.T) {
	a := testApp(t)
	// 加一条"程序直接填脚本"的命令：界面要能把它连同别名一起摆出来，
	// 「将执行」还要显示自动包上的 powershell。
	a.cfg.Commands = append(a.cfg.Commands, config.Command{
		Keyword: "关机",
		Aliases: []string{"shut", "shutdown"},
		Name:    "定时关机（分钟后）",
		Exec:    `D:\1tools\scripts\shutdown-after.ps1`,
		Args:    []string{"%1"},
	})
	s := &settingsWindow{app: a}
	s.load()
	s.tab = 1
	s.cmdSel = len(s.cmds) - 1
	s.cmdSync = -1
	tr := ui.NewTester(s.view, 900, 820)
	tr.Frame()
	for _, want := range []string{
		"自定义命令", "命令", "激活词", "显示名", "程序", "参数", "工作目录",
		"执行后不收起面板", "将执行", "保存并应用",
	} {
		if !tr.HasText(want) {
			t.Fatalf("命令编辑页缺少 %q", want)
		}
	}
	// 左列要列出默认命令和刚加进来的那条，右列的编辑框里是脚本路径与 %1。
	if !tr.HasText("VS Code") || !tr.HasText("关机 · shut · shutdown") {
		t.Fatal("左列没有列出命令（或别名没并进去）")
	}
	if !tr.HasText("%1") || !tr.HasText("shutdown-after.ps1") {
		t.Fatal("右列编辑框没搬进这条命令的脚本/参数")
	}
	dumpPreview(t, tr)
}
