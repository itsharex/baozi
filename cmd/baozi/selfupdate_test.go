package main

import (
	"os"
	"strings"
	"testing"
)

// TestUpdateScript 只验脚本里该有的东西都在：路径、pid、日志、四个关键动作。
// 真正的覆盖行为没法在单测里跑（它会 Stop-Process），所以这里只保证"生成的文本
// 是那段设计好的流程"，不要让它悄悄退化成少一步。
func TestUpdateScript(t *testing.T) {
	s := updateScript(`D:\Program Files\baozi`, `C:\Temp\baozi-update-123`, 4242)

	checks := map[string]string{
		"安装目录":  "'D:\\Program Files\\baozi'",
		"临时目录":  "'C:\\Temp\\baozi-update-123'",
		"进程号":   "4242",
		"等退出":   "Get-Process -Id $waitPid",
		"停进程":   "Stop-Process -Id $p.ProcessId",
		"只动本目录": "$p.ExecutablePath.StartsWith($install",
		"停服务":   "Stop-Service -Name 'baozi-index'",
		"备份":    "'.old'",
		"复制":    "Copy-Item -LiteralPath $src -Destination $dst",
		"重启":    "Start-Process -FilePath $exe",
		"成功标记":  "=== 安装成功 ===",
		"失败标记":  "=== 安装结束（有失败项",
	}
	for why, want := range checks {
		if !strings.Contains(s, want) {
			t.Errorf("脚本里缺 %s（期望含 %q）", why, want)
		}
	}
}

// TestUpdateScriptQuotesSingle 路径里有单引号时必须转义，否则脚本一跑就语法错误。
func TestUpdateScriptQuotesSingle(t *testing.T) {
	s := updateScript(`D:\it's\baozi`, `C:\Temp\it's\stage`, 7)
	if !strings.Contains(s, `'D:\it''s\baozi'`) {
		t.Fatalf("单引号没转义：\n%s", s)
	}
	if strings.Count(s, "$install = '") != 1 {
		t.Fatal("安装目录那一行被写坏了")
	}
}

// TestLastUpdateResultEmpty 没日志时不该编造结果。
func TestLastUpdateResultEmpty(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	if got := lastUpdateResult(); got != "" {
		t.Fatalf("没有 update.log 时返回了 %q", got)
	}
}

// TestDumpUpdateScript 不是断言，是整个脚本的"干跑"入口：
// 设了 BAOZI_DUMP_PS1 就把生成的脚本写到那个路径，好在沙盒目录里真跑一遍。
func TestDumpUpdateScript(t *testing.T) {
	out := os.Getenv("BAOZI_DUMP_PS1")
	if out == "" {
		t.Skip("未设置 BAOZI_DUMP_PS1")
	}
	script := updateScript(os.Getenv("BAOZI_DUMP_INSTALL"), os.Getenv("BAOZI_DUMP_STAGE"), 999999)
	// 和 installUpdate 写出去的完全一致：带 BOM，否则 PS 5.1 按 GBK 读会炸。
	if err := os.WriteFile(out, append([]byte("\xef\xbb\xbf"), script...), 0o644); err != nil {
		t.Fatal(err)
	}
}
