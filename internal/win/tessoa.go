// tessoa 是 Rust + wgpu 自绘的跨平台文件管理器（tessoa.cn，本机实测 v0.30.2）。
// 它的窗口里**没有任何标准控件**：类名是自建的 "Window Class"，UIA 树用
// ControlViewWalker 和 RawViewWalker 走下来都只有顶层一个节点 ——
// 所以资源管理器 / TC / Opus 那套"读地址栏文本"在这里完全用不上。
//
// 它自己提供了一条命令行查询：`tessoa.exe --print-cwd`。
// 实测这个进程把自己打印完就退出（打听已有实例时日志里有
// "已转发给主实例（pid=…）→ 本进程退出，窗口由那边负责"），
// 也就是它已经把"问运行中的实例要当前目录"这件事封装好了，
// 我们用它比自己拼命名管道（\\.\pipe\tessoa.ipc.<hash>，协议未公开）可靠得多。
package win

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// tessoaQueryTimeout 是单次查询的上限。它要起一个 17MB 的进程，
	// 实测热态 16ms、冷态 43ms，给到 3s 只是为了防止它卡死时拖住跟随器。
	tessoaQueryTimeout = 3 * time.Second
	// tessoaCacheTTL 让"轮询顺手读一次"和"对话框弹出时现场重读"共用一次结果：
	// 这两次调用常常挨在几百毫秒内，没必要起两个进程。
	tessoaCacheTTL = 400 * time.Millisecond
)

type tessoaHit struct {
	hwnd uintptr
	dir  string
	at   time.Time
}

var (
	procIsWindow = user32.NewProc("IsWindow")
	// tessoaRunQuery 单独拎出来，方便测试替换掉真实进程。
	tessoaRunQuery = func(exe string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), tessoaQueryTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, exe, "--print-cwd")
		// 它自己不会弹控制台，但子进程别带着我们的窗口句柄跑。
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		return cmd.Output()
	}

	tessoaMu   sync.Mutex
	tessoaLast tessoaHit
)

// IsTessoa 判断是不是 tessoa 的主窗口。
// 类名 "Window Class" 是自建的通用名（其它程序也可能这么起），所以只能按进程名认。
//
// 注意 tessoa 的自更新：它先把正在运行的 exe 改名成 tessoa.old.exe，再把新版本写成
// tessoa.exe，**运行中的进程镜像名因此是 tessoa.old**（本机实测 pid 7016 就是这样）。
// 所以判据是 "tessoa" 本身或 "tessoa.*"，但排除插件进程 tessoa-av / tessoa-office。
func IsTessoa(root uintptr) bool {
	if root == 0 {
		return false
	}
	name := processExeName(root)
	return name == "tessoa" || strings.HasPrefix(name, "tessoa.")
}

// TessoaFolder 读 tessoa 当前活动窗格的目录。
// 失败（没装、进程已退、输出不是目录）一律返回空串，跟随器会当它没提供路径。
func TessoaFolder(root uintptr) string {
	if !IsTessoa(root) {
		return ""
	}
	exe := tessoaExe(root)
	if exe == "" {
		return ""
	}
	// 窗口已经没了就别问了：tessoa 的 --print-cwd 在没有实例可转发时会
	// **自己起一个新窗口**，那等于我们替用户打开了软件。
	if r, _, _ := procIsWindow.Call(root); r == 0 {
		return ""
	}

	tessoaMu.Lock()
	if tessoaLast.hwnd == root && time.Since(tessoaLast.at) < tessoaCacheTTL {
		dir := tessoaLast.dir
		tessoaMu.Unlock()
		return dir
	}
	tessoaMu.Unlock()

	out, err := tessoaRunQuery(exe)
	if err != nil {
		return ""
	}
	dir := tessoaParse(string(out))
	if dir == "" {
		return ""
	}
	tessoaMu.Lock()
	tessoaLast = tessoaHit{hwnd: root, dir: dir, at: time.Now()}
	tessoaMu.Unlock()
	return dir
}

// tessoaExe 挑一个可用的 tessoa.exe 去发查询。
// 优先用窗口所在目录里的新 tessoa.exe（自更新后进程镜像还叫 tessoa.old.exe，
// 新 exe 已经就位；它启动时认得同一个主实例锁，会直接转发过去而不是再开一个窗口）。
func tessoaExe(root uintptr) string {
	self := processExePath(root)
	if self == "" {
		return ""
	}
	candidate := filepath.Join(filepath.Dir(self), "tessoa.exe")
	if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
		return candidate
	}
	// 新 exe 不在（比如用户手动重命名过），退回进程自己的镜像。
	if st, err := os.Stat(self); err == nil && !st.IsDir() {
		return self
	}
	return ""
}

// tessoaParse 把 --print-cwd 的输出收拾成目录路径。
// 正常就是一行路径，但仍然挡住空行、多行、带引号、带 \\?\ 前缀这几种情况，
// 并且**必须是真实存在的目录**才认——tessoa 停在欢迎页时给的是盘根之类的位置，
// 至少还落在文件系统里；它要是给别的什么，宁可不认。
func tessoaParse(out string) string {
	out = strings.ReplaceAll(out, "\r\n", "\n")
	for _, line := range strings.Split(out, "\n") {
		p := strings.TrimSpace(line)
		p = strings.Trim(p, `"`)
		if p == "" {
			continue
		}
		p = strings.TrimPrefix(p, `\\?\`)
		// 盘根（"D:\"）是合法目录，先按原样试；只有试不过才去掉尾部分隔符，
		// 否则 "D:\" 会被削成 "D:"，那是"当前盘当前目录"，不是盘根。
		if p == "" {
			continue
		}
		if isDir(p) {
			return p
		}
		if trimmed := strings.TrimSuffix(p, `\`); trimmed != p && isDir(trimmed) {
			return trimmed
		}
	}
	return ""
}
