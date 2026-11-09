package main

import (
	"testing"

	"github.com/egoist/mygo/ui"
)

// 命令编辑页：左边清单、右边表单。这里守"该有的栏位都在"，
// 布局是否挤压靠 BAOZI_PREVIEW 的实拍看。
func TestSettingsCommandsPageRenders(t *testing.T) {
	a := testApp(t)
	s := &settingsWindow{app: a}
	s.load()
	s.tab = 1
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
	// 左列默认选中第一条（config.Default 里是 term / code / explore），
	// 它的位置和参数应该已经在编辑框里。
	if !tr.HasText("VS Code") && !tr.HasText("Windows Terminal") {
		t.Fatal("左列没有列出默认命令")
	}
	dumpPreview(t, tr)
}
