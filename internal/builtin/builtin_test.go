package builtin

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// 内置脚本落盘时必须带 BOM：PS 5.1 没有 BOM 就按 ANSI/GBK 读，中文提示会把
// 引号吃掉，脚本一跑就是语法错误。
func TestEnsureShutdownWritesBOM(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	path, err := EnsureShutdown()
	if err != nil {
		t.Fatalf("EnsureShutdown: %v", err)
	}
	if path != ShutdownPath() {
		t.Fatalf("路径不对：%s vs %s", path, ShutdownPath())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("没写出文件：%v", err)
	}
	if len(data) < 3 || data[0] != 0xEF || data[1] != 0xBB || data[2] != 0xBF {
		t.Fatal("文件开头没有 UTF-8 BOM")
	}
	if !strings.Contains(string(data), "取消关机") {
		t.Fatal("写出来的不是关机脚本")
	}
	if bytes.Count(data, []byte{0xEF, 0xBB, 0xBF}) != 1 {
		t.Fatal("BOM 叠了不止一层")
	}
}

// 内容没变就不该重写（每次启动都写会把修改时间搅乱，也让"是不是被人改过"看不出来）。
func TestEnsureShutdownIdempotent(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	if _, err := EnsureShutdown(); err != nil {
		t.Fatal(err)
	}
	fi1, err := os.Stat(ShutdownPath())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureShutdown(); err != nil {
		t.Fatal(err)
	}
	fi2, err := os.Stat(ShutdownPath())
	if err != nil {
		t.Fatal(err)
	}
	if !fi1.ModTime().Equal(fi2.ModTime()) {
		t.Fatal("内容一样却重写了文件")
	}
}

// 内容被改坏（比如 PS 5.1 按 GBK 存回去）要能自愈：再 Ensure 一次就恢复成内置版。
func TestEnsureShutdownRepairs(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	if _, err := EnsureShutdown(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ShutdownPath(), []byte("broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureShutdown(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(ShutdownPath())
	if len(data) < 3 || data[0] != 0xEF {
		t.Fatal("没恢复成内置脚本")
	}
}

func TestIsShutdownScript(t *testing.T) {
	cases := map[string]bool{
		`D:\1tools\scripts\shutdown-after.ps1`: true,
		`c:/tools/SHUTDOWN-AFTER.PS1`:          true,
		`D:\1tools\scripts\other.ps1`:          false,
		`builtin:shutdown`:                     false, // 引用写法不是文件路径
		``:                                     false,
	}
	for in, want := range cases {
		if got := IsShutdownScript(in); got != want {
			t.Errorf("IsShutdownScript(%q)=%v, want %v", in, got, want)
		}
	}
}

func TestResolve(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	path, ok := Resolve(ShutdownRef)
	if !ok {
		t.Fatal("builtin:shutdown 应该认得出来")
	}
	if !strings.HasSuffix(path, ShutdownName) {
		t.Fatalf("解析出来的路径不对：%s", path)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("Resolve 应该顺手把文件写好：%v", err)
	}
	if _, ok := Resolve(`D:\1tools\scripts\shutdown-after.ps1`); ok {
		t.Fatal("普通路径不该被当成内置引用")
	}
}
