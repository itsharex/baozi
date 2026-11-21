// Arke 是 Rust + GPUI 的文件管理器（Koimsurai/Arke，本机 v0.8.0-rc.1）。
// 它对外只有一个 JSON 接口（docs/integration.md）：`arke.exe --api -` 从标准输入
// 读一条 {"v":1,"cmd":"state"}，标准输出回一行 JSON，列出每个窗口、标签页、
// 窗格，以及活动窗格当前的 location。
//
// 为什么不读它的 settings.json：那只是上次退出时的会话快照，运行中切标签、
// 切窗格、进目录都不会写回；而且窗口可能有多个。接口是它承诺稳定的那一份。
//
// state 在没有实例时直接回空列表、**不会把 Arke 启动起来**，所以跟随器在
// 用户根本没开 Arke 时问一次也只是起一个短命进程，不会莫名弹出窗口。
// 这和 tessoa 的 --print-cwd 不同（那个没实例时会自己开窗，必须先 IsWindow）。
package win

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// arkeQueryTimeout：一次 state 查询的上限。Arke 已有实例时是走它的单实例
	// 管道转发，实测几十毫秒；3s 只是防止它卡死时拖住跟随器。
	arkeQueryTimeout = 3 * time.Second
	// arkeCacheTTL 让"轮询顺手读一次"和"对话框弹出时现场重读"共用一次结果。
	arkeCacheTTL = 400 * time.Millisecond
	// arkeWindowClass 是接口文档承诺的主窗口类名；弹出菜单是 ArkeWindow.Popup，
	// 所以按精确类名认不会认错。
	arkeWindowClass = "ArkeWindow"
)

type arkeHit struct {
	hwnd uintptr
	dir  string
	at   time.Time
}

type arkeState struct {
	V       int          `json:"v"`
	Windows []arkeWindow `json:"windows"`
}

type arkeWindow struct {
	HWND   string    `json:"hwnd"`
	Active bool      `json:"active"`
	Tabs   []arkeTab `json:"tabs"`
}

type arkeTab struct {
	Active bool       `json:"active"`
	Panes  []arkePane `json:"panes"`
}

type arkePane struct {
	Active    bool         `json:"active"`
	Location  arkeLocation `json:"location"`
	Selection []string     `json:"selection"`
}

type arkeLocation struct {
	Kind  string `json:"kind"`
	Path  string `json:"path"`
	URL   string `json:"url"`
	Query string `json:"query"`
}

var (
	// arkeRunQuery 单独拎出来，方便测试替换掉真实进程。
	arkeRunQuery = func(exe string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), arkeQueryTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, exe, "--api", "-")
		cmd.Stdin = strings.NewReader(`{"v":1,"cmd":"state"}`)
		// 它自己不会弹控制台，但子进程别带着我们的窗口句柄跑。
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		return cmd.Output()
	}

	arkeMu   sync.Mutex
	arkeLast arkeHit
)

// IsArke 判断是不是 Arke 的主窗口。类名是接口文档写死的 ArkeWindow，
// 菜单 / 提示框是 ArkeWindow.Popup，不会混进来。
func IsArke(root uintptr) bool {
	if root == 0 {
		return false
	}
	return strings.EqualFold(className(root), arkeWindowClass)
}

// ArkeFolder 读 Arke 当前活动窗格的目录（前台切换时顺手读一次）。
func ArkeFolder(root uintptr) string { return arkeFolder(root, false) }

// ArkeFolderFresh 是"现在就重新问一次"，不吃缓存。
// 对话框出现那一刻必须给到焦点窗格的最新目录：Arke 是双栏的，
// 在同一个窗口里把焦点换到另一栏不会改变前台 hwnd，缓存里很可能是旧栏。
func ArkeFolderFresh(root uintptr) string { return arkeFolder(root, true) }

func arkeFolder(root uintptr, fresh bool) string {
	if !IsArke(root) {
		return ""
	}
	exe := arkeExe(root)
	if exe == "" {
		return ""
	}
	// 窗口已经没了就别问了（state 会回别的窗口或空列表，那个目录不是这个窗口的）。
	if r, _, _ := procIsWindow.Call(root); r == 0 {
		return ""
	}

	arkeMu.Lock()
	if !fresh && arkeLast.hwnd == root && time.Since(arkeLast.at) < arkeCacheTTL {
		dir := arkeLast.dir
		arkeMu.Unlock()
		return dir
	}
	arkeMu.Unlock()

	out, err := arkeRunQuery(exe)
	if err != nil {
		// 接口回 {"error":...} 时退出码是 1，但 stdout 里仍是合法 JSON；
		// 这里只认成功（ok 时没有 error 字段）。
		return ""
	}
	dir := arkeFolderFromJSON(out, root)
	if dir == "" {
		return ""
	}
	arkeMu.Lock()
	arkeLast = arkeHit{hwnd: root, dir: dir, at: time.Now()}
	arkeMu.Unlock()
	return dir
}

// arkeExe 挑一个可用的 arke.exe 去发查询。
// 优先用窗口所在目录里的 arke.exe（自更新后运行中的镜像可能换了名字），
// 没有再退回进程自己的镜像。
func arkeExe(root uintptr) string {
	self := processExePath(root)
	if self == "" {
		return ""
	}
	candidate := filepath.Join(filepath.Dir(self), "arke.exe")
	if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
		return candidate
	}
	if st, err := os.Stat(self); err == nil && !st.IsDir() {
		return self
	}
	return ""
}

// arkeFolderFromJSON 从 state 回复里取"活动窗口 → 活动标签 → 活动窗格"的目录。
//
// 有多个 Arke 窗口时优先取 hwnd 与调用方窗口一致的那个；找不到（比如窗口刚关）
// 再退回第一个 active 窗口。location.kind 必须是 folder：home / recycle_bin /
// search / remote / other 都没有"一个可以送进文件对话框的目录"，按接口文档的
// 建议当作没有目录，不去猜。
func arkeFolderFromJSON(data []byte, want uintptr) string {
	var st arkeState
	if err := json.Unmarshal(data, &st); err != nil || st.V != 1 {
		return ""
	}
	w, ok := arkePickWindow(st.Windows, want)
	if !ok {
		return ""
	}
	t, ok := arkePickTab(w.Tabs)
	if !ok {
		return ""
	}
	p, ok := arkePickPane(t.Panes)
	if !ok || p.Location.Kind != "folder" {
		return ""
	}
	pth := strings.TrimSpace(p.Location.Path)
	pth = strings.TrimPrefix(pth, `\\?\`)
	if pth == "" || !isDir(pth) {
		return ""
	}
	return pth
}

func arkePickWindow(ws []arkeWindow, want uintptr) (arkeWindow, bool) {
	if want != 0 {
		wantHex := strconv.FormatUint(uint64(want), 16)
		for _, w := range ws {
			h := strings.ToLower(strings.TrimSpace(w.HWND))
			h = strings.TrimPrefix(h, "0x")
			if h == strings.ToLower(wantHex) {
				return w, true
			}
		}
	}
	for _, w := range ws {
		if w.Active {
			return w, true
		}
	}
	if len(ws) > 0 {
		return ws[0], true
	}
	return arkeWindow{}, false
}

func arkePickTab(ts []arkeTab) (arkeTab, bool) {
	for _, t := range ts {
		if t.Active {
			return t, true
		}
	}
	if len(ts) > 0 {
		return ts[0], true
	}
	return arkeTab{}, false
}

func arkePickPane(ps []arkePane) (arkePane, bool) {
	for _, p := range ps {
		if p.Active {
			return p, true
		}
	}
	if len(ps) > 0 {
		return ps[0], true
	}
	return arkePane{}, false
}
