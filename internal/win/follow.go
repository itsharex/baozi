// 读资源管理器当前目录 + 文件对话框自动跟随。
//
// 对应 Lertaro 的 `IActivePathCollector`（ExplorerPathCollector）与
// `Core/Hook/ExplorerIntegration/FileDialogNavigationTracker.cs`：
//
//  1. 记住"最近一次资源管理器所在的目录"（lastDir），目录一变就把版本号 +1；
//  2. 每当一个「打开 / 另存为」框成为前台：
//     - 第一次见到它，只登记"它已经同步到当前版本"，不导航
//     （程序刚弹出的对话框往往自己指定了目录，不该立刻覆盖）；
//     - 之后它再次成为前台、而版本号变过（期间资源管理器换过目录）→ 把 lastDir 送过去。
//
// 于是用户的实际感受就是：另存为框默认在我的文档 → 去资源管理器切到下载 → 切回另存为框，
// 路径自己变成下载。Lertaro 注释里另有一条经验也照抄了：
// 只有"上一次激活的确实是资源管理器"时才重读它的路径，否则用户手动在对话框里选的目录
// 会在下一次激活时被打回旧目录。
package win

import (
	"context"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	procGetAncestor = user32.NewProc("GetAncestor")
)

const (
	gaRootOwner      = 2
	explorerPollTick = 200 * time.Millisecond
)

// RootOwner 沿 GA_ROOTOWNER 上溯：多标签资源管理器里前台常是 ShellTabWindowClass 这类子窗口，
// 上溯到 CabinetWClass 才能读地址栏。
func RootOwner(hwnd uintptr) uintptr {
	if hwnd == 0 {
		return 0
	}
	if root, _, _ := procGetAncestor.Call(hwnd, gaRootOwner); root != 0 {
		return root
	}
	return hwnd
}

// IsExplorer 判断是不是资源管理器窗口（单窗口 CabinetWClass，老式 ExploreWClass）。
func IsExplorer(root uintptr) bool {
	cls := className(root)
	return strings.EqualFold(cls, "CabinetWClass") || strings.EqualFold(cls, "ExploreWClass")
}

// ExplorerFolder 读资源管理器当前目录。
// 地址栏和文件对话框是同一套控件（Breadcrumb Parent → ToolbarWindow32），
// 那个工具条的 window text 就是完整路径，形如 "地址: D:\work\baozi"。
func ExplorerFolder(root uintptr) string {
	bread := findChildByClass(root, "Breadcrumb Parent")
	if bread == 0 {
		return ""
	}
	var toolbar uintptr
	// 直接子层只有一个 id=1001 的 ToolbarWindow32；用递归枚举以免以后换了层级。
	cb := syscall.NewCallback(func(c uintptr, _ uintptr) uintptr {
		if toolbar != 0 {
			return 1
		}
		if strings.EqualFold(className(c), "ToolbarWindow32") {
			parent, _, _ := procGetParent.Call(c)
			if strings.EqualFold(className(parent), "Breadcrumb Parent") {
				toolbar = c
				return 0
			}
		}
		return 1
	})
	procEnumChildWindows.Call(bread, cb, 0)
	if toolbar == 0 {
		return ""
	}
	t := WindowTextTimeout(toolbar)
	t = strings.TrimPrefix(t, "地址: ")
	t = strings.TrimPrefix(t, "Address: ")
	if !isDir(t) {
		return ""
	}
	return t
}

// WindowTextTimeout 带超时读文本：目标进程卡住时不把调用方一起拖死
// （资源管理器在大目录 / 网络盘上确实会卡）。
func WindowTextTimeout(hwnd uintptr) string {
	buf := make([]uint16, maxText)
	sendMsg(hwnd, wmGetText, maxText, uintptr(unsafe.Pointer(&buf[0])))
	return windows.UTF16ToString(buf)
}

// FollowEvent 是一次"把对话框送到某目录"。
type FollowEvent struct {
	HWND uintptr
	Dir  string
}

// DialogFollow 复刻 Lertaro 的 FileDialogNavigationTracker。
//
// 关键一点（Lertaro 源码里那行 `previousWasPathProvider && readProviderPath()` 的含义）：
// 轮询期间读到的目录可能已经过时 —— 用户在资源管理器里换目录的那一刻不一定被采到。
// 所以**对话框重新成为前台时，要现场再读一次"上一次活动的那个资源管理器窗口"的目录**。
// 只记 hwnd 不记内容，读的时刻永远是最新的，这比"轮询时顺手读一次再存起来"稳得多。
// 反过来，如果上一次活动的是别的程序，就不重读（否则用户手动在框里换的目录会被拽回旧目录）。
type DialogFollow struct {
	mu      sync.Mutex
	lastDir string
	version int
	seen    map[uintptr]int
	lastFg  uintptr

	// lastActive 是上一次观察到的前台窗口根（用来判断"上一次活动的是不是资源管理器"）。
	// lastExplorer 是最近一次活动的资源管理器窗口。
	lastActive   uintptr
	lastExplorer uintptr

	onNavigate func(FollowEvent)
	dbg        func(format string, args ...any)
}

// NewDialogFollow 建一个跟随器。onNavigate 在不阻塞轮询的 goroutine 里回调，
// 用来打日志 / 更新界面提示；dbg 可以为 nil。
func NewDialogFollow(onNavigate func(FollowEvent), dbg func(string, ...any)) *DialogFollow {
	return &DialogFollow{seen: map[uintptr]int{}, onNavigate: onNavigate, dbg: dbg}
}

func (f *DialogFollow) logf(format string, args ...any) {
	if f.dbg != nil {
		f.dbg(format, args...)
	}
}

// LastDir 是最近一次记住的资源管理器目录（空表示还没见过）。
func (f *DialogFollow) LastDir() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastDir
}

// Start 起轮询。用轮询而不是 SetWinEventHook：前台窗口变化 300ms 内就能看到，
// 而钩子回调里还要跨进程读地址栏，卡一下就把整个 shell 的 WinEvent 线程拖住了。
func (f *DialogFollow) Start(ctx context.Context) {
	go func() {
		t := time.NewTicker(explorerPollTick)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				f.poll()
			}
		}
	}()
}

func (f *DialogFollow) poll() {
	raw, _, _ := procGetForegroundWindow.Call()
	if raw == 0 {
		return
	}
	// 前台没变就不用再往下走：读地址栏要往别人进程发消息，能不读就不读。
	f.mu.Lock()
	if raw == f.lastFg {
		f.mu.Unlock()
		return
	}
	f.lastFg = raw
	f.mu.Unlock()

	root := RootOwner(raw)
	switch {
	case IsExplorer(root):
		f.mu.Lock()
		f.lastActive = root
		f.lastExplorer = root
		f.mu.Unlock()
		// 这里顺手读一次只是为了让"停在资源管理器里不动"也能被记住，
		// 真正决定导航目标的还是 handleDialog 里那次现场重读。
		f.rememberDir(ExplorerFolder(root))

	case IsFileDialog(root):
		f.mu.Lock()
		prev := f.lastActive
		lastExplorer := f.lastExplorer
		f.lastActive = root
		f.mu.Unlock()

		// 上一次活动的就是资源管理器 → 现读它此刻的目录（不是轮询时缓存的那个）。
		// 上一次活动的是别的程序就不重读 —— 否则用户手动在框里换过的目录会被拽回去。
		if prev != 0 && lastExplorer != 0 && prev == lastExplorer && IsExplorer(lastExplorer) {
			f.rememberDir(ExplorerFolder(lastExplorer))
		}
		f.handleDialog(root)

	default:
		f.mu.Lock()
		f.lastActive = root
		f.mu.Unlock()
	}
}

// rememberDir 目录真变了才升版本号。
func (f *DialogFollow) rememberDir(dir string) {
	if dir == "" {
		return
	}
	f.mu.Lock()
	changed := !strings.EqualFold(dir, f.lastDir)
	if changed {
		f.lastDir = dir
		f.version++
	}
	v := f.version
	f.mu.Unlock()
	if changed {
		f.logf("跟随: 记住目录 %q（版本 %d）", dir, v)
	}
}

// handleDialog 是 tracker 的核心规则：
//   - 第一次见到 → 只登记版本，不导航；
//   - 再见到、且版本变过 → 把 lastDir 送过去。
func (f *DialogFollow) handleDialog(hwnd uintptr) {
	f.mu.Lock()
	seen, ok := f.seen[hwnd]
	version, dir := f.version, f.lastDir
	if !ok {
		f.seen[hwnd] = version
		f.mu.Unlock()
		f.logf("跟随: 第一次见到对话框 %#x，只登记（版本 %d）", hwnd, version)
		return
	}
	if seen == version || dir == "" {
		f.mu.Unlock()
		return
	}
	f.seen[hwnd] = version
	f.mu.Unlock()
	f.logf("跟随: 对话框 %#x 版本 %d→%d，送到 %q", hwnd, seen, version, dir)

	// 跨进程写 + 回车，慢且可能被外壳拖着，扔到后台去，别卡住轮询。
	go func() {
		if NavigateDialog(hwnd, dir) && f.onNavigate != nil {
			f.onNavigate(FollowEvent{HWND: hwnd, Dir: dir})
		}
	}()
}
