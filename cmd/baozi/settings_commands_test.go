package main

import (
	"testing"

	"baozi/internal/config"
)

// 没打开过「自定义命令」页就点保存，不能把已有命令的激活词清空 ——
// 清空之后 save() 的归一化会把没激活词的条目当空壳丢掉，命令就一条条消失。
func TestSaveWithoutCommandsPageKeepsCommands(t *testing.T) {
	s := &settingsWindow{
		app:     testApp(t),
		cmds:    []config.Command{{Keyword: "睡眠", Aliases: []string{"sleep"}, Name: "睡眠", Exec: `D:\1tools\s.ps1`, Args: []string{"-Sleep", "%1"}}},
		cmdSel:  0,
		cmdSync: -1, // 编辑器还没同步过
	}
	s.commitCommandEditor()
	if s.cmds[0].Keyword != "睡眠" || len(s.cmds[0].Aliases) != 1 {
		t.Fatalf("编辑器没同步却把命令改了：%+v", s.cmds[0])
	}
	if got := s.cmds[0].Args; len(got) != 2 || got[0] != "-Sleep" {
		t.Fatalf("参数被清掉了：%v", got)
	}
}

// 反过来：真的在命令页改过（cmdSync == cmdSel），保存时要写回编辑框里的内容。
func TestSaveOnCommandsPageWritesEditor(t *testing.T) {
	s := &settingsWindow{
		app:      testApp(t),
		cmds:     []config.Command{{Keyword: "睡眠", Name: "睡眠", Exec: `D:\1tools\s.ps1`}},
		cmdSel:   0,
		cmdSync:  0,
		cmdAlias: "睡眠, sleep",
		cmdArgs:  "-Sleep\n%1",
	}
	s.commitCommandEditor()
	if s.cmds[0].Keyword != "睡眠" || len(s.cmds[0].Aliases) != 1 || s.cmds[0].Aliases[0] != "sleep" {
		t.Fatalf("激活词没写回：%+v", s.cmds[0])
	}
	if len(s.cmds[0].Args) != 2 {
		t.Fatalf("参数没写回：%v", s.cmds[0].Args)
	}
}
