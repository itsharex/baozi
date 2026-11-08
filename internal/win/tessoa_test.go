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

// 对话框出现那一刻要"现在就问"，不能吃 400ms 缓存：
// tessoa 切窗格 / 切布局都不改变前台窗口，缓存里很可能还是上一个窗格的目录。
func TestTessoaFolderFreshBypassesCache(t *testing.T) {
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
	if calls != 1 {
		t.Fatalf("第一次查询应起一次进程，实际 %d 次", calls)
	}
	if got := TessoaFolderFresh(hw); !strings.EqualFold(got, dir) {
		t.Fatalf("fresh 查询应返回 %q，实际 %q", dir, got)
	}
	if calls != 2 {
		t.Fatalf("fresh 查询必须绕开缓存、再起一次进程，实际共 %d 次", calls)
	}
}

// 4 窗格 / 切布局时前台窗口不变，跟随器只能靠 tessoa 的窗口标题变化来发现"窗格换了"。
func TestTessoaPollsOnTitleChange(t *testing.T) {
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

	f := NewDialogFollow(nil, nil, nil)
	f.lastSource = hw
	f.lastFg = hw
	// 标题与当前一致：不该去起进程。
	real := WindowTextTimeout(hw)
	f.lastTitle = real
	f.lastTitleAt = time.Now().Add(-time.Hour)
	f.pollActiveSource(hw, hw)
	if calls != 0 {
		t.Fatalf("标题没变不该起进程，实际 %d 次", calls)
	}
	// 标题变了（相当于切了窗格）：这次要读，并把目录记住。
	f.lastTitle = real + " ·"
	f.lastTitleAt = time.Now().Add(-time.Hour)
	f.pollActiveSource(hw, hw)
	if calls != 1 {
		t.Fatalf("标题变了应读一次，实际 %d 次", calls)
	}
	if got := f.LastDir(); !strings.EqualFold(got, dir) {
		t.Fatalf("lastDir 应为 %q，实际 %q", dir, got)
	}
}
