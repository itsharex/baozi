package win

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

var procEnumWindowsForTest = user32.NewProc("EnumWindows")

// findTessoaWindow 枚举顶层窗口，找一个属于 tessoa 进程的。
func findTessoaWindow() uintptr {
	var found uintptr
	cb := syscall.NewCallback(func(h uintptr, _ uintptr) uintptr {
		if IsTessoa(h) {
			found = h
			return 0
		}
		return 1
	})
	procEnumWindowsForTest.Call(cb, 0)
	return found
}

func TestTessoaParsePicksRealDirectory(t *testing.T) {
	dir := t.TempDir()
	root := filepath.VolumeName(dir) + `\`
	cases := []struct{ name, in, want string }{
		{"正常一行", dir + "\r\n", dir},
		{"带空白", "  " + dir + "  \n", dir},
		{"带引号", "\"" + dir + "\"\n", dir},
		{"带 \\\\?\\ 前缀", `\\?\` + dir + "\n", dir},
		{"空输出", "\n\r\n", ""},
		{"不是目录", "hello world\n", ""},
		{"多行取第一个存在的", "hello world\n" + dir + "\n", dir},
		{"盘根原样保留", root + "\n", root},
	}
	for _, c := range cases {
		if got := tessoaParse(c.in); got != c.want {
			t.Fatalf("%s：tessoaParse(%q)=%q，期望 %q", c.name, c.in, got, c.want)
		}
	}
}

// tessoa 没在跑时不能去"问"，否则它的 --print-cwd 会自己拉起一个新窗口。
func TestTessoaFolderIgnoresBadHandle(t *testing.T) {
	if got := TessoaFolder(0); got != "" {
		t.Fatalf("hwnd=0 应返回空串，实际 %q", got)
	}
}

func TestTessoaFolderFromLiveInstance(t *testing.T) {
	hw := findTessoaWindow()
	if hw == 0 {
		t.Skip("这台机器没有正在运行的 tessoa")
	}
	dir := TessoaFolder(hw)
	if dir == "" {
		t.Fatal("tessoa 正在运行，但 --print-cwd 没给出目录")
	}
	if !isDir(dir) {
		t.Fatalf("返回的不是目录：%q", dir)
	}
	if !filepath.IsAbs(dir) {
		t.Fatalf("返回的不是绝对路径：%q", dir)
	}
}

// 整条分派链路：窗口 → 来源类型 → 读目录；来源在设置里关掉后就不读。
func TestTessoaIsPathSource(t *testing.T) {
	hw := findTessoaWindow()
	if hw == 0 {
		t.Skip("这台机器没有正在运行的 tessoa")
	}
	if kind := sourceKind(hw); kind != SourceTessoa {
		t.Fatalf("sourceKind 应认成 %q，实际 %q", SourceTessoa, kind)
	}
	f := NewDialogFollow(nil, nil, nil)
	dir := f.folderOf(hw)
	if dir == "" || !isDir(dir) {
		t.Fatalf("跟随器没从 tessoa 读到目录：%q", dir)
	}
	// 关掉这个来源后连读都不该读。
	off := NewDialogFollow(nil, nil, func(kind string) bool { return kind != SourceTessoa })
	if got := off.folderOf(hw); got != "" {
		t.Fatalf("来源关掉后不该再读，实际读到 %q", got)
	}
}

func TestTessoaFolderCachesQuery(t *testing.T) {
	hw := findTessoaWindow()
	if hw == 0 {
		t.Skip("这台机器没有正在运行的 tessoa")
	}
	orig := tessoaRunQuery
	t.Cleanup(func() {
		tessoaRunQuery = orig
		tessoaMu.Lock()
		tessoaLast = tessoaHit{}
		tessoaMu.Unlock()
	})
	calls := 0
	dir := t.TempDir()
	tessoaRunQuery = func(string) ([]byte, error) {
		calls++
		return []byte(dir + "\r\n"), nil
	}
	tessoaMu.Lock()
	tessoaLast = tessoaHit{}
	tessoaMu.Unlock()

	if got := TessoaFolder(hw); !strings.EqualFold(got, dir) {
		t.Fatalf("第一次查询应返回 %q，实际 %q", dir, got)
	}
	if got := TessoaFolder(hw); !strings.EqualFold(got, dir) {
		t.Fatalf("第二次查询应命中缓存，实际 %q", got)
	}
	if calls != 1 {
		t.Fatalf("缓存窗口内应只起一次进程，实际起了 %d 次", calls)
	}
	// 超过缓存窗口后要重新问
	tessoaMu.Lock()
	tessoaLast.at = time.Now().Add(-time.Second)
	tessoaMu.Unlock()
	_ = TessoaFolder(hw)
	if calls != 2 {
		t.Fatalf("缓存过期后应再问一次，实际共 %d 次", calls)
	}
}
