package main

import (
	"context"
	"log"
	"os"
	"sync"
	"time"

	"baozi/internal/update"
	"baozi/internal/version"

	"golang.org/x/sys/windows"
)

// openExternal 用系统默认浏览器打开一个链接。
//
// 只走 ShellExecute("open")：不自己拼命令行、不经过 cmd.exe —— 链接里有 "&"
// 或 "?" 的时候，拼命令行那条路会把参数吃掉一半。
func openExternal(url string) {
	if url == "" {
		return
	}
	verb := windows.StringToUTF16Ptr("open")
	file := windows.StringToUTF16Ptr(url)
	if err := windows.ShellExecute(0, verb, file, nil, nil, 0); err != nil {
		log.Printf("打开链接失败（%s）：%v", url, err)
	}
}

// updateState 是"查新版本"这件事的全部状态。
//
// 为什么单开一个类型：这份状态要同时被三处读（面板底栏、关于页、日志），
// 而被一个后台 goroutine 写。用一把小锁把"写结果"和"读快照"圈起来，
// 比零散的 atomic 字段好维护。
type updateState struct {
	mu   sync.Mutex
	busy bool            // 正在查
	last *update.Release // 最近一次成功的结果（可能是"已是最新"）
	err  string          // 最近一次失败的原因（空表示没失败过）
	at   time.Time       // 最近一次发起检查的时刻

	// 下载/覆盖阶段的状态。"关于"页在 installing 时把按钮换成一句进度，
	// 用户能看见下到哪一步了（下载慢的时候尤其重要）。
	installing bool
	progress   string
}

// updateSnapshot 是给界面读的一帧快照（值拷贝，出了锁就不用再管并发）。
type updateSnapshot struct {
	Busy       bool
	Installing bool
	Progress   string
	Tag        string
	PageURL    string
	ZipURL     string
	Newer      bool
	Err        string
	At         time.Time
	Current    string
}

// startUpdateCheck 在启动后**后台**查一次新版本。
//
// 三条硬约束（用户明确要求"一定不要阻塞正常功能"）：
//  1. 只起 goroutine，调用方立刻返回 —— 线程池里那个 goroutine 先睡 3 秒，
//     让窗口、索引、钩子先起来，别跟它们抢网络和 CPU；
//  2. 整个请求有超时（8 秒），失败就当没查过，界面写"检查失败"；
//  3. 拿不到网、机场、代理挂了，都只是这一条日志的事，没有任何重试。
//
// 关掉的方式：配置里的 updates.autoCheck=false，或环境变量 BAOZI_NOUPDATE=1
// （后者给调试/离线场景用，不落盘）。
func (a *application) startUpdateCheck() {
	if a.cfg == nil || !a.cfg.UpdateAutoCheck() {
		return
	}
	if os.Getenv("BAOZI_NOUPDATE") == "1" {
		return
	}
	if os.Getenv("BAOZI_HEADLESS") == "1" {
		// 测试/自动截图这类没有真实界面的运行，没必要打扰 GitHub。
		return
	}
	go func() {
		// 启动后先让一让：这时候正在抓屏、扫索引、拉起钩子，网络请求晚几秒毫无损失。
		time.Sleep(3 * time.Second)
		a.runUpdateCheck(false)
	}()
}

// checkUpdatesNow 是"关于"页那个按钮：立刻查一次（仍然是后台）。
//
// 返回前不做任何网络动作，所以按钮回调不会卡住绘制。
func (a *application) checkUpdatesNow(onDone func()) {
	go func() {
		a.runUpdateCheck(false)
		if onDone != nil {
			onDone()
		}
	}()
}

// runUpdateCheck 真正跑一次查询。manual 只影响日志措辞。
func (a *application) runUpdateCheck(manual bool) {
	if !a.upd.begin() {
		// 上一次还没回来：不叠请求，直接复用那一次的结果。
		return
	}
	defer a.upd.end()

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	rel, err := update.Latest(ctx)
	if err != nil {
		a.upd.fail(err)
		log.Printf("更新: 检查失败（%v）", err)
		return
	}
	newer := update.Newer(version.Version, rel.Tag)
	a.upd.ok(rel)
	if newer {
		log.Printf("更新: 发现新版本 %s（当前 %s），下载页 %s", rel.Tag, version.Display(), rel.PageURL)
	} else {
		log.Printf("更新: 已是最新（%s，当前 %s）", rel.Tag, version.Display())
	}
	if manual {
		log.Printf("更新: 手动检查完成（newer=%v）", newer)
	}
	// 结果变了：面板底栏的提示和关于页的状态都得跟上。
	// 这里只是排一帧，不重烘玻璃 —— 与外观无关的状态变化不该触发整张底图重算。
	if a.panelUp.Load() {
		a.panel.Update(func() {})
	}
}

// begin 抢到"这一轮归我查"，返回 false 表示已经有人在查了。
func (s *updateState) begin() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy {
		return false
	}
	s.busy = true
	s.at = time.Now()
	return true
}

func (s *updateState) end() {
	s.mu.Lock()
	s.busy = false
	s.mu.Unlock()
}

func (s *updateState) ok(rel *update.Release) {
	s.mu.Lock()
	s.last = rel
	s.busy = false
	s.err = ""
	s.mu.Unlock()
}

func (s *updateState) fail(err error) {
	s.mu.Lock()
	s.err = err.Error()
	s.busy = false
	s.mu.Unlock()
}

// snapshot 给界面拿一份当前状态。
func (s *updateState) snapshot() updateSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := version.Version
	out := updateSnapshot{
		Busy: s.busy, Installing: s.installing, Progress: s.progress,
		Err: s.err, At: s.at, Current: cur,
	}
	if s.last != nil {
		out.Tag = s.last.Tag
		out.PageURL = s.last.PageURL
		out.ZipURL = s.last.ZipURL
		out.Newer = update.Newer(cur, s.last.Tag)
	}
	return out
}

// beginInstall 抢到"这次更新归我装"，返回 false 表示已经在装或正在查。
func (s *updateState) beginInstall() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.installing {
		return false
	}
	s.installing = true
	s.progress = "准备更新…"
	return true
}

func (s *updateState) endInstall() {
	s.mu.Lock()
	s.installing = false
	s.mu.Unlock()
}

// setUpdProgress 改一行进度，并请界面重画一帧（"关于"页正开着的话立刻能看到）。
func (a *application) setUpdProgress(msg string) {
	a.upd.mu.Lock()
	a.upd.progress = msg
	a.upd.mu.Unlock()
	if a.settings != nil && a.settings.win != nil {
		a.settings.win.Update(func() {})
	}
}

// latestTag 是给面板底栏用的：有新版本就返回版本号，否则空。
func (a *application) latestTag() string {
	s := a.upd.snapshot()
	if s.Newer {
		return s.Tag
	}
	return ""
}
