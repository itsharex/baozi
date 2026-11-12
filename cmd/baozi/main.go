// Baozi 是一个 Windows 快速启动器：双击 Ctrl 唤出，搜索文件/应用，
// 按「频次 > 类型 > 准确性」排序，空查询时展示使用历史。
//
// 架构对应关系（与 Lertaro 的差异写在 docs/architecture.md）：
//   - 本进程 = 用户态交互层（对应 Lertaro.App）
//   - baozi-hook = 键盘钩子与窗口适配层（对应 Lertaro.Service --hook）
//   - 索引：当前在进程内后台重建；接入 USN/MFT 后改为独立 SYSTEM 服务（对应 Lertaro.Service）
package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	_ "net/http/pprof" // BAOZI_PPROF=1 时才真的监听，平时只是注册 handler
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"baozi/internal/config"
	"baozi/internal/history"
	"baozi/internal/index"
	"baozi/internal/pinyin"
	"baozi/internal/search"
	"baozi/internal/win"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
)

const (
	panelWidth  = 720
	panelHeight = 480

	// 结果行左侧图标的尺寸不再写死：默认跟着系统缩放走，也能在设置里指定，
	// 见 config.BaseIconSize / Config.IconSizeAt 与 application.iconSize。
	// 之前是固定的 22 DIP，在 125% 缩放的屏上（这台机器就是）显得偏小。

	// panelCols 是首页网格每行的卡片数：720 宽的面板，卡片 84 + 间距 6，一排 7 个。
	// 方向键按行移动（上下各走一行）。
	panelCols = 7
	// cardWidth 是首页卡片宽度。图标最大 64 时仍然放得下（左右各留 6 的内边）。
	cardWidth = 84
	// maxPinned 是首页「已固定」区最多显示几个。
	maxPinned = 12

	// maxAppRun：结果里应用最多连续出现几条，之后就插一条别的类型。
	// 应用仍然排在最前（类型分 1.00），但不允许把整张榜占满。
	maxAppRun = 3
)

var searchIcon = ui.MustParseSVG([]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="11" cy="11" r="8"/><path d="m21 21-4.3-4.3"/></svg>`))

// debugKeys 打开后在 stderr 打印每次查询，排查"输入到底有没有到面板"。
var debugKeys = os.Getenv("BAOZI_DEBUG") != ""

// appEntry 是开始菜单应用 + 预先算好的拼音串（应用只有几百个，值得每次启动算一遍）。
type appEntry struct {
	mygo.InstalledApp
	initials string // "豆包工作" → "dbgz"，空串表示名字是纯 ASCII
	full     string // "豆包工作" → "doubaogongzuo"
	// alias 是从 ID 抽出来的产品名（Microsoft.WindowsCalculator → "Microsoft WindowsCalculator"）。
	// 它代替 Path 参与匹配，理由见 search.AppAlias。
	alias string
}

type application struct {
	cfg     *config.Config
	cfgPath string
	dataDir string

	panel *mygo.Window
	tray  *mygo.Tray
	// menu 是托盘菜单：设置里改了自启后要回头同步勾选状态，不然两处会打架。
	menu *mygo.Menu

	hist *history.Store

	// cat 是当前索引快照。快照自己带着名字 arena 一起换，
	// 旧快照在被替换后依然有效（搜索进行中的那次读的是旧表，不会看到半新半旧的数据）。
	cat  atomic.Pointer[index.Table]
	apps atomic.Pointer[[]appEntry]

	// 增量同步状态（只由 refreshIndex 读写，用 idxMu 串行化）。
	// idxMtime 是已载入的那份 index.bin 的修改时间；idxBase 是它的代次标识；
	// idxSeq 是已经叠到服务写出的第几批；idxOps 是这一代里已经叠了多少条指令
	// —— 服务每次都整份重写 delta 的 ops，所以进度必须按"条"记，不能按"批"。
	idxMu    sync.Mutex
	idxMtime time.Time
	idxBase  uint64
	idxSeq   uint64
	idxOps   int

	query   string
	results []search.Result
	chosen  int
	// pinnedCount 是 results 末尾属于「已固定」的条数（首页网格分段用，
	// 前 len(results)-pinnedCount 条是最近使用）。
	pinnedCount int
	list        ui.ListState
	input       ui.Handle
	icons       map[string]*ui.Bitmap
	// thumbAsk 记录"已经去要过缩略图的键"（失败的也算），避免每帧重复下单。
	// thumbSem 限制同时在取的缩略图个数：Shell 那边是 COM 走 explorer 的缩略图
	// 提供者，一次放几十个进去只会互相拖慢，4 个足够铺满一屏。
	thumbAsk map[string]bool
	thumbSem chan struct{}
	// offline 为 true 时不向 Shell 要图标（视图测试里没有消息循环，要了会永久阻塞）。
	offline bool
	// scale 是显示器的最大缩放比例，启动时问一次就缓存：Screen.Displays 也走
	// onMainValue，视图测试里现问会永久阻塞。0 表示还没问过，按 1 处理。
	scale float32

	// 首页四向键的按住状态：焦点在搜索框时 Shortcut 收不到左右键（被输入框吃掉），
	// 只能读按住状态做边沿检测（keyStep）。
	heldLeft, heldRight, heldUp, heldDown bool

	// follow 是文件对话框跟随：记住文件管理器最近待过的目录，那个「打开 / 另存为」框
	// 再次回到前台时把路径送过去（Lertaro 的 IActivePathCollector + NavigationTracker）。
	follow       *win.DialogFollow
	followCancel context.CancelFunc
	// sources 是"现在哪些目录来源开着"。跟随器在自己的 goroutine 里每 200ms 问一次，
	// 而配置指针在保存时会被整体替换，所以这里用原子指针存一份快照，避免数据竞争。
	sources atomic.Pointer[map[string]bool]

	status string // 索引/钩子状态，显示在底栏

	// regexOn 是搜索框右边那个 ".*" 按钮的状态：开着时查询按正则解释。
	// 正则模式下只做精确匹配 —— 不走拼音、不走模糊子序列、不做准确度分档。
	// 不落盘：这是这次打开面板期间的一个模式，重启回到普通搜索。
	regexOn bool
	// regexErr 是上一次正则的语法错误，显示在结果区上方（空表示没问题）。
	regexErr string

	// quitting 为 true 表示正在真正退出：窗口的 OnClose 不再拦关闭。
	// mygo 的 Quit 是逐个关窗口，任何窗口 PreventDefault 都会让整个 Quit 作废。
	quitting atomic.Bool

	hook     *exec.Cmd
	hookIn   io.WriteCloser
	quitOnce sync.Once
	scanMu   sync.Mutex
	scanOff  context.CancelFunc
	// scanWG 跟踪后台扫描。卸载 / 退出时等它收尾，否则 RemoveAll 会和索引落盘打架
	// （表现就是数据目录"删了又回来"）。
	scanWG sync.WaitGroup

	settings *settingsWindow
	share    *shareWindow

	// uninstalling 为 true 表示正在卸载：收尾时不再落盘历史（数据目录马上要整个删掉）。
	uninstalling atomic.Bool

	// openAtLogin 是开机自启的当前状态。它在 ready() 里问系统一次就缓存住：
	// mygo 的 App.OpenAtLogin 走 onMainValue，必须在 native 主循环里调用，
	// 而设置窗口的视图在 ui.Tester 下渲染（没有主循环），每次 load 去问会永久阻塞。
	openAtLogin atomic.Bool

	// 右键菜单里选的动作不能在菜单回调里直接改 a.results：
	// 那一帧正在渲染这条列表，边遍历边重建会越界。先记下来，视图开头统一处理。
	pendingOp  string // "" | "delete" | "pin" | "unpin"
	pendingIdx int
	// 首页「已固定」区里拖动排序：同样不能在卡片渲染途中改 results，
	// 松手那一帧记下起点与落点，下一帧 view 开头结算。
	pendingPinMove *pinMove

	// pinDragPts 存每张固定卡片本帧的拖动偏移。Draggable 要求这个指针跨帧稳定，
	// 每帧新建一个 Point 会把拖动中的卡片拽回原位。
	pinDragPts    map[int]*ui.Point
	pinDragFrom   int
	pinDragActive bool
	pinDragX      float32
	pinDragY      float32
}

// pinMove 是一次拖动排序的意图，from/to 都是 results 的下标。
type pinMove struct {
	from, to int
}

// resultKey 是结果在历史里的键：普通项用路径，命令用关键字。
func resultKey(r search.Result) string {
	if r.Path != "" {
		return r.Path
	}
	return r.Command
}

// applyPending 执行上一帧右键菜单里选的动作、以及拖动排序的结果。
// 只在 view 开头调用（那时还没开始遍历结果）。
func (a *application) applyPending() {
	if mv := a.pendingPinMove; mv != nil {
		a.pendingPinMove = nil
		a.applyPinMove(*mv)
	}
	if a.pendingOp == "" {
		return
	}
	op, idx := a.pendingOp, a.pendingIdx
	a.pendingOp = ""
	if idx < 0 || idx >= len(a.results) {
		return
	}
	r := a.results[idx]
	key := resultKey(r)
	switch op {
	case "delete":
		if a.hist.Remove(key) {
			a.status = "已从历史删除：" + r.Name
		} else {
			a.status = "这条不在历史里：" + r.Name
		}
	case "unpin":
		if a.hist.Unpin(key) {
			a.status = "已取消固定：" + r.Name
		}
	case "pin":
		if a.hist.Count(key) == 0 {
			e := entryOf(r)
			if e.Path == "" {
				e.Path = key
			}
			a.hist.TouchEntry(e)
		}
		if pinned, ok := a.hist.TogglePin(key); ok {
			if pinned {
				a.status = "已固定到首页：" + r.Name
			} else {
				a.status = "已取消固定：" + r.Name
			}
		}
	}
	// 首页的两段（最近 / 已固定）都由历史推出来，删/固定之后必须重建。
	if a.query == "" {
		a.results = a.historyResults(12)
		a.choose(0)
		return
	}
	// 搜索态下结果来自索引：删的只是历史记录，命中项下次空查询才消失。
	if op == "delete" {
		a.status = "已从历史删除（搜索仍能搜到）：" + r.Name
	}
}

// applyPinMove 把一次拖动排序落进历史，然后重建首页。
func (a *application) applyPinMove(mv pinMove) {
	recentTo := len(a.results) - a.pinnedCount
	if a.pinnedCount < 2 || mv.from < recentTo || mv.to < recentTo ||
		mv.from >= len(a.results) || mv.to >= len(a.results) {
		return
	}
	keys := make([]string, 0, a.pinnedCount)
	for i := recentTo; i < len(a.results); i++ {
		keys = append(keys, resultKey(a.results[i]))
	}
	relFrom, relTo := mv.from-recentTo, mv.to-recentTo
	k := keys[relFrom]
	keys = append(keys[:relFrom], keys[relFrom+1:]...)
	keys = slices.Insert(keys, relTo, k)
	if !a.hist.ReorderPinned(keys) {
		return
	}
	a.status = fmt.Sprintf("已保存固定顺序：%s 移到第 %d 位", a.results[mv.from].Name, relTo+1)
	a.results = a.historyResults(maxPinned)
	a.choose(recentTo + relTo)
}

// pinDragPoint 取某张固定卡片跨帧复用的拖动偏移指针。
func (a *application) pinDragPoint(i int) *ui.Point {
	if a.pinDragPts == nil {
		a.pinDragPts = map[int]*ui.Point{}
	}
	pt, ok := a.pinDragPts[i]
	if !ok {
		pt = &ui.Point{}
		a.pinDragPts[i] = pt
	}
	return pt
}

// trackPinDrag 把这张卡片的拖动状态接起来：拖动中记录最新偏移，
// 松手那一帧把偏移归零（否则卡片会粘在拖到的位置），并记下这次移动的意图。
func (a *application) trackPinDrag(i int, el ui.Element) {
	pt := a.pinDragPoint(i)
	if el.Pressed() {
		if !a.pinDragActive || a.pinDragFrom != i {
			a.pinDragActive, a.pinDragFrom = true, i
		}
		o := el.DragOffset()
		a.pinDragX, a.pinDragY = o.X, o.Y
		return
	}
	if !a.pinDragActive || a.pinDragFrom != i {
		return
	}
	a.pinDragActive = false
	*pt = ui.Point{}
	a.pendingPinMove = a.pinMoveFor(i, a.pinDragX, a.pinDragY)
	a.pinDragX, a.pinDragY = 0, 0
}

// pinMoveFor 把拖动位移换算成落点。
// 卡片 84 宽、行内间距 6，所以一列 90；行高随图标块的尺寸走（块 + 57，
// 其中 57 = 上下 padding 8 + 图标块与名字的间距 5 + 三行字 38 + 行间距 6），
// 图标块 32 时正好是 89。这两组数字要跟 card() 与 homeGrid() 保持一致。
func (a *application) pinMoveFor(from int, dx, dy float32) *pinMove {
	recentTo := len(a.results) - a.pinnedCount
	if a.pinnedCount < 2 || from < recentTo {
		return nil
	}
	// 设置里把图标调大了，卡片跟着变高，落点判定也得跟着换，否则拖一格会跳两格。
	// 行高 = 上下 padding 8 + 图标 + 图标与名字的间距 6 + 两行字 28.8 + 行间距 6。
	rowStep := a.cardIcon() + 49
	colShift := int(math.Round(float64(dx) / 90))
	rowShift := int(math.Round(float64(dy) / float64(rowStep)))
	if colShift == 0 && rowShift == 0 {
		return nil
	}
	rel := from - recentTo
	to := rel + colShift + rowShift*panelCols
	if to < 0 {
		to = 0
	}
	if to >= a.pinnedCount {
		to = a.pinnedCount - 1
	}
	if to == rel {
		return nil
	}
	a.pinDragPts = nil // 位置要变了，旧的偏移指针不能再对号入座
	return &pinMove{from: from, to: recentTo + to}
}

func (a *application) view(c *ui.Context) {
	t := c.Theme()
	root := c.Root()
	if c.Vibrancy() {
		root.Background(ui.Transparent)
	} else {
		root.Background(t.Background)
	}
	// 右键菜单选的"删除 / 固定"在这里落地：此时还没开始遍历 results，改它不会越界。
	a.applyPending()

	home := a.query == ""
	if a.query != "" && c.Shortcut(0, ui.KeyEscape) {
		a.query = ""
		a.refilter()
	}
	// Ctrl+K 固定/取消固定选中项（Pane 用 Ctrl+K 打开应用的「更多操作」）。
	if c.Shortcut(ui.Ctrl, ui.KeyK) {
		a.togglePin()
	}
	// Ctrl+Enter 打开所在文件夹（Pane 底栏写明的那一条）。
	if c.Shortcut(ui.Ctrl, ui.KeyEnter) {
		a.openFolder()
	}
	// Ctrl+Shift+C 复制所在目录：Windows 的打开/另存为框里 Ctrl+L → Ctrl+V → 回车，
	// 那个框就直接跳到这个目录（外部程序改不动已经开着的对话框，见 docs/architecture.md）。
	if c.Shortcut(ui.Ctrl|ui.Shift, ui.KeyC) {
		a.copyDir(c)
	}
	if home {
		// 首页方向键不能走 Shortcut：搜索框有焦点时 TextInput 会把左右键吃掉
		// （光标在移动、卡片选中却不动），表现就是"首页左右键失效"。
		// 这里直接读按键按住状态做边沿检测——查询为空时光标动也没意义。
		a.keyStep(c, &a.heldLeft, ui.KeyLeft, -1)
		a.keyStep(c, &a.heldRight, ui.KeyRight, +1)
		a.keyStep(c, &a.heldUp, ui.KeyUp, -panelCols) // 网格：上下按一行走
		a.keyStep(c, &a.heldDown, ui.KeyDown, +panelCols)
	} else {
		if c.Shortcut(0, ui.KeyDown) {
			a.choose(a.chosen + 1)
		}
		if c.Shortcut(0, ui.KeyUp) {
			a.choose(a.chosen - 1)
		}
	}
	if c.Shortcut(0, ui.KeyEnter) {
		a.open()
	}

	ui.Column(c).Fill().Children(func() {
		// 顶栏：放大镜 + 大字输入 + 正则开关 + 右端品牌胶囊（Pane 右上角那个 "Pane" 标）。
		ui.Row(c).Padding(16, 20).Gap(14).AlignItems(ui.Center).Children(func() {
			ui.Icon(c, searchIcon).FontSize(20).TextColor(t.TextMuted)
			ph := "搜索应用、文件，或输入命令"
			if a.regexOn {
				ph = "正则匹配文件名，如 ^2026.*\\.log$"
			}
			changed := ui.TextInputBase(c, &a.query).Bind(&a.input).Grow(1).FontSize(20).
				Placeholder(ph).Label("搜索").
				OnSubmit(a.open).Changed()
			a.regexButton(c)
			if changed {
				a.refilter()
			}
			ui.Text(c, "Baozi").FontSize(11).TextColor(t.TextMuted).
				Padding(5, 10).Radius(9).Background(t.Surface)
		})
		ui.Divider(c)
		if home {
			a.homeGrid(c)
		} else {
			a.resultList(c)
		}
		ui.Divider(c)
		a.footer(c)
	})
}

// regexButton 是搜索框右边那个 ".*" 切换按钮：点一下进正则模式，再点回普通搜索。
// 开着时整块用 Accent 底标出来 —— 正则模式和普通搜索结果差得很远，
// 状态必须一眼可辨，否则用户会以为"搜索坏了"。
func (a *application) regexButton(c *ui.Context) {
	t := c.Theme()
	b := ui.Button(c, ".*").FontSize(12).Padding(3, 9).Radius(7).
		Tooltip("正则模式：只按正则精确匹配文件名，不走拼音与模糊")
	if a.regexOn {
		b.TextColor(t.Text).Background(t.Accent.Alpha(0.28)).
			BorderWidth(1).BorderColor(t.Accent.Alpha(0.6))
	} else {
		b.TextColor(t.TextMuted).Background(t.Surface)
	}
	if !b.Clicked() {
		return
	}
	a.regexOn = !a.regexOn
	a.regexErr = ""
	if a.regexOn {
		a.setStatus("正则模式：只精确匹配文件名")
	} else {
		a.setStatus("已回到普通搜索")
	}
	// 鼠标点过按钮之后焦点可能从输入框跑掉，用户接着就要敲正则——把焦点还回去。
	if a.panel != nil {
		a.panel.Update(func() { a.input.Focus() })
	}
	a.refilter()
}

// homeGrid 是空查询时的首页：「最近使用」在上、「已固定」在下两段网格卡片。
// 顺序照 Pane——常用的贴手，固定的沉底。
func (a *application) homeGrid(c *ui.Context) {
	t := c.Theme()
	ui.Column(c).Grow(1).Children(func() {
		if len(a.results) == 0 {
			ui.Text(c, "开始输入以搜索文件和应用；打开过的项目会留在这里").
				TextColor(t.TextMuted).Padding(20)
			return
		}
		section := func(title string) {
			ui.Text(c, title).FontSize(11).TextColor(t.TextMuted).Padding(10, 18, 3, 18)
		}
		// 不用 Grid：它的列宽是均分的，720 宽下每列 114，卡片 84 居中会左右各空 15，
		// 看着"占位很大"。这里自己拼行，卡片固定 84、间距 6，一排刚好 7 个。
		cards := func(from, to int) {
			ui.Column(c).Gap(6).Padding(2, 18).Children(func() {
				for start := from; start < to; start += panelCols {
					end := start + panelCols
					if end > to {
						end = to
					}
					ui.Row(c).Gap(6).Children(func() {
						for i := start; i < end; i++ {
							a.card(c, i)
						}
					})
				}
			})
		}
		recentTo := len(a.results) - a.pinnedCount
		if recentTo > 0 {
			section("最近使用")
			cards(0, recentTo)
		}
		// 这行弹性空白把「已固定」压到面板最底下（Column 的子项不会自己撑开）。
		// 内容多到放不下时它会被压成 0，固定段照样紧接其后。
		ui.Row(c).Grow(1)
		if a.pinnedCount > 0 {
			section("已固定")
			cards(recentTo, len(a.results))
		}
	})
}

// wrapName 把名字折成卡片能放下的样子，按"字宽"而不是"字符数"算。
// 卡片内容宽 76 DIP、名字 10.5 号：中日韩字符约 10.5 DIP，拉丁字符约 5.5 DIP。
// 为什么必须自己折：英文长文件名（SnowShot_Video_2026-10-29_13-31-04.mp4）没有
// 空格可断，mygo 不会按字符硬断，只会横向溢出到隔壁卡片上叠成一团。
// 断行优先落在 _ - . 空格 这些分隔符后面，切掉的部分不超过行宽的 45%，
// 这样 "SnowShot_Video_" 不会被拆成 "SnowShot_Vide" + "o_..." 那种样子。
// 满 lines 行还放不下才加省略号；完整名字另有 Tooltip 兜底。
func wrapName(name string, width float32, lines int) string {
	const cjkWidth, latinWidth = 10.5, 5.5
	wOf := func(r rune) float32 {
		if r > 0x2e80 {
			return cjkWidth
		}
		return latinWidth
	}
	isBreak := func(r rune) bool {
		switch r {
		case ' ', '_', '-', '.', '/', '\\':
			return true
		}
		return false
	}
	runes := []rune(name)
	var parts []string
	start := 0
	for start < len(runes) {
		var used float32
		end := start
		lastBreak := -1
		for end < len(runes) {
			w := wOf(runes[end])
			if used+w > width {
				break
			}
			used += w
			end++
			if isBreak(runes[end-1]) {
				lastBreak = end
			}
		}
		if end >= len(runes) {
			parts = append(parts, string(runes[start:]))
			break
		}
		if len(parts) == lines-1 {
			parts = append(parts, string(runes[start:end])+"…")
			break
		}
		cut := end
		// 分隔符断行只损失一两格宽度时才值得：回退太多会让最后一行提前截断，
		// 反而少显示字（37 个字符的文件名要在三行里装完，一格都不能浪费）。
		if lastBreak > start && end-lastBreak <= 2 {
			cut = lastBreak
		}
		parts = append(parts, string(runes[start:cut]))
		start = cut
	}
	return strings.Join(parts, "\n")
}

// card 是首页网格里的一格：圆角图标块 + 居中的名字。
// 尺寸照启动器那种"大图标 + 名字"的样子：卡片 84 宽，图标 55（跟着设置缩放），
// 名字 12 号、最多两行。不再给图标套浅灰方块——那层底色会把彩色图标压小一圈。
// 卡片宽不跟着图标变：哪怕图标调到 64，84 宽也还放得下，首页保持整齐。
func (a *application) card(c *ui.Context, i int) {
	r := a.results[i]
	t := c.Theme()
	bg, border := ui.Transparent, ui.Transparent
	if i == a.chosen {
		bg = t.SurfaceHover
		border = t.Accent.Alpha(0.6)
	}
	// 「已固定」区的卡片可以拖着重排：拖动中的那张换底色+亮边，让人知道"手上有东西"。
	pinned := i >= len(a.results)-a.pinnedCount
	dragging := pinned && a.pinDragActive && a.pinDragFrom == i
	if dragging {
		bg = t.SurfaceHover
		border = t.Accent.Alpha(0.8)
	}
	col := ui.Column(c).Width(cardWidth).Padding(6, 4).Gap(6).AlignItems(ui.Center).Radius(12).
		Background(bg).BorderWidth(1).BorderColor(border).
		OnClick(func() {
			a.chosen = i
			a.open()
		}).
		ContextMenu(func(m *ui.Menu) { a.itemMenu(m, i) }).
		Children(func() {
			// 图标直接铺在卡片上，不再套那个浅灰方块：应用/程序的彩色图标本身就是
			// 辨识度，套个小方块只会把它压小、还多出一圈底色（用户要的就是启动器
			// 那种"大图标 + 名字"的样子，见 cardIcon）。
			a.resultIcon(c, r, a.cardIcon())
			ui.Text(c, wrapName(r.Name, cardWidth-12, 2)).FontSize(12).TextColor(t.Text).
				TextAlign(ui.Center).MaxLines(2).LineHeight(1.2).Tooltip(r.Name)
		})
	if !pinned {
		return
	}
	// 拖动本身不改数据：松手时才在下一帧的 applyPending 里重排（渲染途中不能改 results）。
	col = col.Draggable(a.pinDragPoint(i), ui.DraggableOptions{})
	a.trackPinDrag(i, col)
}

// itemMenu 是首页卡片和结果行共用的右键菜单：固定 / 取消固定、从历史删除。
func (a *application) itemMenu(m *ui.Menu, i int) {
	if i < 0 || i >= len(a.results) {
		return
	}
	r := a.results[i]
	key := resultKey(r)
	if a.hist.IsPinned(key) {
		if m.Item("取消固定").Chosen() {
			a.pendingOp, a.pendingIdx = "unpin", i
		}
	} else {
		if m.Item("固定到首页").Shortcut(ui.Ctrl, ui.KeyK).Chosen() {
			a.pendingOp, a.pendingIdx = "pin", i
		}
	}
	m.Separator()
	if m.Item("从历史删除").Danger().Chosen() {
		a.pendingOp, a.pendingIdx = "delete", i
	}
}

// resultList 是有查询时的结果列表：单列，行式。
func (a *application) resultList(c *ui.Context) {
	t := c.Theme()
	// 不把 Selected 交给 List：List 会用整块 accent 填充画选中行，
	// 我们要的是 Pane 那种「浅色胶囊 + 细边」，选中态由 row 自己画。
	a.list.Selected = nil
	a.list.Key = func(i int) any { return a.results[i].Path + a.results[i].Command }
	a.list.Label = func(i int) string { return a.results[i].Name }
	list := ui.List(c, &a.list, len(a.results), func(i int) { a.row(c, i) }).Grow(1).Padding(8).Children(func() {
		if len(a.results) == 0 {
			switch {
			case a.regexOn && a.regexErr != "":
				// 正则写错了要指出来，否则"搜不到"像是功能没生效
				ui.Text(c, "正则无效："+a.regexErr).TextColor(t.TextMuted).Padding(18)
			case a.regexOn:
				ui.Text(c, "没有文件名命中这个正则").TextColor(t.TextMuted).Padding(18)
			default:
				ui.Text(c, "没有匹配项").TextColor(t.TextMuted).Padding(18)
			}
		}
	})
	if list.Submitted() {
		a.open()
	}
}

// footer 是底栏：左边说选中项会做什么，右边用键帽写清 Enter / Ctrl+Enter / Ctrl+K。
func (a *application) footer(c *ui.Context) {
	t := c.Theme()
	ui.Row(c).Padding(9, 18).Gap(10).AlignItems(ui.Center).Children(func() {
		ui.Text(c, a.footerText()).FontSize(11).TextColor(t.TextMuted).Grow(1).SingleLine()
		a.keyCap(c, "Enter", "打开")
		a.keyCap(c, "Ctrl+Enter", "文件夹")
		a.keyCap(c, "Ctrl+Shift+C", "复制路径")
		a.keyCap(c, "Ctrl+K", "固定")
		a.keyCap(c, "右键", "删除/固定")
		ui.Text(c, "设置").FontSize(11).TextColor(t.TextMuted).
			Padding(4, 8).Radius(6).Background(t.Surface).OnClick(a.openSettings)
		ui.Text(c, "互传").FontSize(11).TextColor(t.TextMuted).
			Padding(4, 8).Radius(6).Background(t.Surface).OnClick(a.openShare)
	})
}

func (a *application) footerText() string {
	if a.chosen >= 0 && a.chosen < len(a.results) {
		r := a.results[a.chosen]
		if r.Path != "" && a.hist.IsPinned(r.Path) {
			return "已固定 · " + r.Name
		}
		return "打开 " + r.Name
	}
	return a.status
}

func (a *application) keyCap(c *ui.Context, key, what string) {
	t := c.Theme()
	ui.Row(c).Gap(4).AlignItems(ui.Center).Children(func() {
		ui.Text(c, key).FontSize(10).TextColor(t.TextMuted).
			Padding(3, 6).Radius(5).Background(t.Surface)
		ui.Text(c, what).FontSize(10).TextColor(t.TextMuted)
	})
}

func (a *application) row(c *ui.Context, i int) {
	r := a.results[i]
	t := c.Theme()
	// Pane 的选中态是"整行浅色胶囊 + 细边"，不是整块高饱和填充。
	bg, border := ui.Transparent, ui.Transparent
	if i == a.chosen {
		bg = t.Accent.Alpha(0.16)
		border = t.Accent.Alpha(0.5)
	}
	ui.Row(c).Padding(9, 12).MarginY(2).Gap(12).AlignItems(ui.Center).Radius(10).
		Background(bg).BorderWidth(1).BorderColor(border).
		OnClick(func() {
			// 鼠标点一下就打开：之前这里没有任何点击处理，点了像没反应。
			a.chosen = i
			a.open()
		}).
		ContextMenu(func(m *ui.Menu) { a.itemMenu(m, i) }).
		Children(func() {
			a.resultIcon(c, r, a.iconSize())
			ui.Column(c).Grow(1).Gap(2).Children(func() {
				ui.Text(c, r.Name).TextColor(t.Text).FontSize(14).Bold().SingleLine().Tooltip(r.Name)
				ui.Text(c, r.Hint).FontSize(11).TextColor(t.TextMuted).SingleLine()
			})
			ui.Text(c, badge(r)).FontSize(10).TextColor(t.TextMuted)
		})
}

// iconSize 是结果行左侧图标的边长。没在设置里指定就跟着系统缩放走
// （100% → 22、125% → 27.5），这块逻辑在 config.Search.IconSizeAt 里，
// 因为它同时要被设置页的"当前会显示多大"预览用到。
func (a *application) iconSize() float32 {
	return a.cfg.IconSizeAt(a.scale)
}

// cardIcon 是首页卡片里图标的边长。卡片 84 宽，图标按"结果行图标 ×2.2"放大
// ——125% 缩放下结果行是 27.5、卡片就是 60.5，接近启动器那种"图标占满整格"的比例
// （再大名字就只剩一行了）。上下都夹一下：太小认不出、太大名字没地方放。
func (a *application) cardIcon() float32 {
	icon := a.iconSize() * 2.2
	if icon < 32 {
		icon = 32
	}
	if icon > 64 {
		icon = 64
	}
	return icon
}

// resultIcon 画结果左侧的类型图标：文件/目录/命令用单色线框（icon.go），
// 应用用系统的彩色图标——应用的图标本身就是辨识度，换成线框反而认不出。
// .exe / .lnk 虽然记成文件，但它本身就是个程序，同样走彩色图标：线框认不出是哪个程序。
// size 是图标显示边长：行里 22，首页卡片 23。
func (a *application) resultIcon(c *ui.Context, r search.Result, size float32) {
	t := c.Theme()
	if g := glyphFor(r); g != nil {
		if isProgram(r) {
			if bm := a.appIcon(r, int(size*2.2)); bm != nil {
				ui.Image(c, bm).Size(size, size)
				return
			}
		} else if thumbable(r) {
			if bm := a.thumbIcon(r, size); bm != nil {
				ui.Image(c, bm).Size(size, size)
				return
			}
		}
		ui.Icon(c, g).FontSize(size).TextColor(t.TextMuted)
		return
	}
	// 位图按 2.2 倍请求再缩下来：系统图标四周那圈透明边裁掉后剩下的内容往往只有
	// 原图的六七成，按 1.6 倍要图裁完就不够 26 用了，会糊。
	if bm := a.appIcon(r, int(size*2.2)); bm != nil {
		ui.Image(c, bm).Size(size, size)
		return
	}
	ui.Icon(c, glyphFile).FontSize(size).TextColor(t.TextMuted)
}

// isProgram 判断这个"文件"其实是个可执行程序：.exe/.lnk/.msi/.scr/.com。
// 这类条目在历史里记成文件，但用户期望看到的是它自己的图标。
// .bat/.cmd 不算：它们的 shell 图标是通用的齿轮，还不如统一走线框。
func isProgram(r search.Result) bool {
	if r.Kind != search.KindFile {
		return false
	}
	switch strings.ToLower(r.Ext) {
	case ".exe", ".lnk", ".msi", ".scr", ".com":
		return true
	}
	return false
}

func badge(r search.Result) string {
	switch r.Kind {
	case search.KindApp:
		return "应用"
	case search.KindDir:
		return "目录"
	case search.KindCommand:
		return "命令"
	}
	if r.Ext != "" {
		return strings.TrimPrefix(r.Ext, ".")
	}
	return "文件"
}

// appIcon 取应用的彩色图标并缓存。只有应用还向 Shell 要图：它的 Path 是
// shell:AppsFolder\<AUMID>，按扩展名取会拿到一个无关的通用图标（"图标和名称
// 对不上"就是这么来的）。文件与目录已改成 icon.go 里的单色线框图标，
// 不再走这里——少了几万次 COM 调用，也不会再冒出深色底上的白色纸片。
func (a *application) appIcon(r search.Result, px int) *ui.Bitmap {
	scale := a.scale
	if scale <= 0 {
		scale = 1
	}
	px = int(float32(px) * scale)
	key := fmt.Sprintf("app:%d:%s", px, r.Path)
	// 先看缓存：已经解码好的图标不依赖 Shell，离线（视图测试）也能照常画出来。
	if b, ok := a.icons[key]; ok {
		return b
	}
	if a.offline {
		return nil
	}
	data, _ := mygo.Shell.FileIcon(r.Path, px)
	if trimmed, err := win.TrimIcon(data); err == nil {
		data = trimmed
	}
	bitmap, _ := ui.DecodeBitmap(data)
	// 失败不写缓存：否则这张图标在本进程里永远是空的，看着就是"这程序没图标"。
	if bitmap != nil {
		a.icons[key] = bitmap
	}
	return bitmap
}

// thumbable 判断这条结果要不要显示内容缩略图：图片、视频、PDF。
// 其它文档不取 —— .txt/.docx 的缩略图多半是白纸或空白页，还不如统一线框好认。
func thumbable(r search.Result) bool {
	if r.Kind != search.KindFile || r.Path == "" {
		return false
	}
	ext := strings.ToLower(strings.TrimPrefix(r.Ext, "."))
	return imageExts[ext] || videoExts[ext] || ext == "pdf"
}

// thumbIcon 返回这条结果的缩略图；还没取到就先返回 nil（本帧画线框），
// 并在后台下单，取回后经 panel.Update 重绘。
//
// 为什么异步：冷态一张 19ms、最慢 80ms，一屏十几行同步取就是几百毫秒的白屏。
// 宁可先看到线框图标，也别为了小图把面板唤出的手感拖慢。
func (a *application) thumbIcon(r search.Result, size float32) *ui.Bitmap {
	// 与 appIcon 同样按 2.2 倍请求：22 逻辑 px 的方块，2.2 倍后缩下来才不糊。
	px := int(size * 2.2)
	if s := a.scale; s > 1 {
		px = int(float32(px) * s)
	}
	key := fmt.Sprintf("thumb:%d:%s", px, r.Path)
	// 先看缓存：已经取到的图不依赖 Shell，离线（视图测试）也能照常画出来。
	if bm, ok := a.icons[key]; ok {
		return bm // ok 但 bm 为 nil：这张系统给不出缩略图，别再反复试
	}
	if a.offline || !a.cfg.ThumbnailsOn() {
		return nil
	}
	if a.thumbAsk == nil {
		a.thumbAsk = map[string]bool{}
		a.thumbSem = make(chan struct{}, 4)
	}
	if a.thumbAsk[key] {
		return nil
	}
	select {
	case a.thumbSem <- struct{}{}:
		a.thumbAsk[key] = true
		go a.loadThumb(key, r.Path, px)
	default:
		// 四个在飞已经满了：这次不排队，等下一帧有空位再来
	}
	return nil
}

// loadThumb 在后台线程取一张缩略图。COM 的 STA 与线程绑定，所以进去第一件事
// 是把线程钉住（win.Thumbnail 内部自己 LockOSThread，不用这里管）。
func (a *application) loadThumb(key, path string, px int) {
	defer func() { <-a.thumbSem }()
	data, err := win.Thumbnail(path, px)
	if err != nil {
		a.writeIcon(key, nil)
		return
	}
	bm, _ := ui.DecodeBitmap(data)
	a.writeIcon(key, bm) // 解码失败时 bm 为 nil，同样记成"取不到"
}

// writeIcon 把异步取回的图塞进缓存并在 UI 线程重绘。
// icons 只在 UI 线程读写（渲染、以及这里的 Update 回调），所以不用加锁。
func (a *application) writeIcon(key string, bm *ui.Bitmap) {
	stash := func() {
		a.icons[key] = bm
		// 滚过几百张图之后缓存会一直涨；超过上限就把缩略图整批丢掉（程序图标留着）。
		// 不做 LRU：缩略图重建只要 1ms，为省这点钱维护链表不值。
		if len(a.icons) > 900 {
			a.dropThumbs()
		}
	}
	if a.panel == nil {
		stash()
		return
	}
	a.panel.Update(stash)
}

func (a *application) dropThumbs() {
	for k := range a.icons {
		if strings.HasPrefix(k, "thumb:") {
			delete(a.icons, k)
		}
	}
	for k := range a.thumbAsk {
		delete(a.thumbAsk, k)
	}
}

// screenScale 取所有显示器里最大的缩放比例（图标要按物理像素请求，缩下来才清晰）。
func screenScale() float32 {
	scale := float32(1)
	for _, d := range mygo.Screen.Displays() {
		scale = max(scale, float32(d.ScaleFactor))
	}
	return scale
}

// refilter 处理用户输入：空查询给历史，否则走搜索。
func (a *application) refilter() {
	q := strings.TrimSpace(a.query)
	if debugKeys {
		log.Printf("query: %q", q)
	}
	if q == "" {
		a.results = a.historyResults(12)
		a.choose(0)
		return
	}
	// 输入即搜索：放到后台，避免几百万条目卡住 UI；结果回来时校验查询没变。
	go func() {
		res := a.searchAll(q)
		a.panel.Update(func() {
			if strings.TrimSpace(a.query) == q {
				a.results = res
				a.choose(0)
				if debugKeys {
					for i := 0; i < len(res) && i < 5; i++ {
						log.Printf("  #%d %s (kind=%d, acc=%.2f, type=%.2f, score=%.3f)",
							i+1, res[i].Name, res[i].Kind, res[i].Accuracy, res[i].TypeScore, res[i].Score)
					}
					var ac, fc, dc, cc int
					for _, r := range res {
						switch r.Kind {
						case search.KindApp:
							ac++
						case search.KindDir:
							dc++
						case search.KindCommand:
							cc++
						default:
							fc++
						}
					}
					log.Printf("  结果 %d: 应用 %d, 文件 %d, 目录 %d, 命令 %d", len(res), ac, fc, dc, cc)
					if len(res) == 0 {
						log.Print("  （没有结果）")
					}
				}
			}
		})
	}()
}

// historyResults 把历史记录还原成可展示的条目（空查询时）。
// 优先用记录里的元数据；老记录（只有 path）才回退到按路径猜。
func (a *application) historyResults(n int) []search.Result {
	// 首页顺序 = 「已固定」在前（按最近使用），后面接「最近使用」（去掉已固定的）。
	pinned := a.hist.Pinned(maxPinned)
	pinnedOnly := make(map[string]bool, len(pinned))
	for _, e := range pinned {
		pinnedOnly[e.Path] = true
	}
	// 顺序是「最近使用」在前、「已固定」在后（Pane 的首页就是这样：常用的贴手，
	// 固定的沉底）。索引与显示顺序保持一致，方向键按行移动才不会跳段。
	entries := make([]history.Entry, 0, len(pinned)+n)
	for _, e := range a.hist.Recent(0) {
		if pinnedOnly[e.Path] {
			continue
		}
		entries = append(entries, e)
		if len(entries) >= n {
			break
		}
	}
	entries = append(entries, pinned...)
	a.pinnedCount = len(pinned)
	out := make([]search.Result, 0, len(entries))
	if debugKeys {
		log.Printf("history: %d 条（其中固定 %d）", len(entries), len(pinned))
	}
	for _, e := range entries {
		name, kind, ext, hint := e.Name, kindOf(e.Kind), e.Ext, e.Hint
		if name == "" {
			name = filepath.Base(e.Path)
		}
		if hint == "" {
			if kind == search.KindApp {
				hint = "应用"
			} else if kind == search.KindCommand {
				hint = "命令：" + e.Exec
			} else {
				hint = filepath.Dir(e.Path)
			}
		}
		if ext == "" && kind == search.KindFile {
			ext = index.Ext(name)
		}
		item := search.Item{
			Name: name,
			Path: e.Path,
			Ext:  ext,
			Kind: kind,
			Hint: hint,
		}
		if kind == search.KindCommand {
			item.Command = e.Exec
			item.Args = e.Args
		}
		out = append(out, search.Result{
			Item:      item,
			Frequency: a.hist.Score(e.Path),
			Accuracy:  1,
			TypeScore: search.TypeScore(ext, kind, a.cfg.Search.TypeOrder),
		})
	}
	return out
}

func kindOf(k history.Kind) search.Kind {
	switch k {
	case history.KindApp:
		return search.KindApp
	case history.KindDir:
		return search.KindDir
	case history.KindCommand:
		return search.KindCommand
	}
	return search.KindFile
}

// searchAll 把命令、已安装应用、索引条目放在一起打分排序。
func (a *application) searchAll(q string) []search.Result {
	// 正则模式是另一条路：不拼音、不模糊、不打分档。这里一分流，下面所有
	// "token / 子序列 / 拼音" 的逻辑对它就完全不参与。
	if a.regexOn {
		return a.searchRegex(q)
	}
	lowQ := strings.ToLower(q)
	tokens := strings.Fields(lowQ)
	weights := search.Weights(a.cfg.Search.Weights)

	results := make([]search.Result, 0, 256)

	// 1) 自定义命令：激活词（主词或别名）整词命中优先（Listary 式）
	for _, cmd := range a.cfg.Commands {
		words, ok := commandWords(cmd, q)
		if !ok {
			continue
		}
		dir := a.lastFollowDir()
		args := expandArgs(cmd.Args, words, dir)
		results = append(results, search.Result{
			Item: search.Item{
				Name:     cmd.Name,
				Path:     cmd.Exec,
				Hint:     "命令：" + cmd.Exec + " " + strings.Join(args, " "),
				Kind:     search.KindCommand,
				Command:  cmd.Exec,
				Args:     args,
				Dir:      expandTemplate(cmd.Dir, words, dir),
				KeepOpen: cmd.KeepOpen,
			},
			TypeScore: 1,
			Accuracy:  1,
			Score:     search.Combine(weights, 0.5, 1, 1),
		})
	}

	// 2) 已安装应用：名字（含拼音）一路，产品名一路，取分高的那个。
	// 不把 Path 交给匹配器——理由见 search.AppAlias。
	if apps := a.apps.Load(); apps != nil {
		for i, app := range *apps {
			acc, ok := search.AccuracyPy(q, app.Name, "", app.initials, app.full)
			if app.alias != "" {
				if alt, ok2 := search.AccuracyPy(q, app.alias, "", "", ""); ok2 && alt > acc {
					acc, ok = alt, true
				}
			}
			if !ok {
				continue
			}
			freq := a.hist.Score(app.Path)
			ts := search.TypeScore(".exe", search.KindApp, a.cfg.Search.TypeOrder)
			results = append(results, search.Result{
				Item: search.Item{
					Name:     app.Name,
					Path:     app.Path,
					Ext:      ".exe",
					Kind:     search.KindApp,
					Hint:     "应用",
					AppIndex: i,
				},
				Frequency: freq,
				TypeScore: ts,
				Accuracy:  acc,
				Score:     search.Combine(weights, freq, ts, acc),
			})
		}
	}

	// 3) 索引条目
	tab := a.cat.Load()
	if tab == nil || tab.Len() == 0 || len(tokens) == 0 {
		return a.finish(results)
	}
	byPath := strings.ContainsAny(q, `\/`)
	matched := 0
	for i := 0; i < tab.Len(); i++ {
		var path string
		if byPath {
			path = tab.PathOf(int32(i))
			if !strings.Contains(strings.ToLower(path), lowQ) {
				continue
			}
		} else if !tab.Hits(i, tokens) && !pyContains(tab.PyOf(i), tokens) {
			// 拼音查询（dbgz）在名字里当然找不到字面，得靠预存的拼音首字母串放行
			continue
		} else if tab.Dead(int32(i)) {
			// 墓碑/被删目录的子条目。放在预筛之后才划算：全表上溯一遍要多花
			// 几十毫秒，而这里每条查询只有几十个候选。
			// （按路径查的那条分支走 PathOf，它自带同样的判定）
			continue
		}
		name := tab.Name(i)
		acc, ok := search.AccuracyPy(q, name, path, tab.PyOf(i), "")
		if !ok {
			continue
		}
		if path == "" {
			path = tab.PathOf(int32(i))
		}
		freq := a.hist.Score(path)
		ext := tab.Ext(i)
		ts := search.TypeScore(ext, toSearchKind(tab.Nodes[i].Kind), a.cfg.Search.TypeOrder)
		results = append(results, search.Result{
			Item: search.Item{
				Name: name,
				Path: path,
				Ext:  ext,
				Kind: toSearchKind(tab.Nodes[i].Kind),
				Hint: tab.Dir(int32(i)),
			},
			Frequency: freq,
			TypeScore: ts,
			Accuracy:  acc,
			Score:     search.Combine(weights, freq, ts, acc),
		})
		matched++
		if matched >= 5000 { // 上限保护：极端宽泛查询不至于卡死
			break
		}
	}
	return a.finish(results)
}

// maxRegexHits 是正则模式收集结果的上限。模糊搜索那边是 5000（子串预筛本身就很窄），
// 正则命中面更宽（`\.log$` 就是八千多条），收得太少会在排序之前就把该在前面的丢掉。
const maxRegexHits = 20000

// searchRegex 是正则模式的搜索：把整串查询当 Go 正则，只做**精确匹配**。
// 不叠拼音、不叠模糊子序列、不做准确度分档 —— 用户写了模式，命中与否他说了算，
// 准确度一律 1.0，排序只剩频次与类型这两项。
//
// 只匹配**文件名**。普通模式靠"查询里有没有 \ 或 /"来决定要不要匹配路径，
// 正则里的反斜杠到处都是（`\.log$`），那个判断在这里不成立；想按路径找就自己写
// 目录部分的模式。代价是路径匹配（PathOf 顺父链还原，很贵）在这条路上完全不发生。
func (a *application) searchRegex(q string) []search.Result {
	rx, err := search.CompileRegex(q)
	if err != nil {
		// 语法错误必须显式说：否则用户看到空结果会以为"正则没生效"。
		a.setRegexErr(q, err.Error())
		return nil
	}
	a.setRegexErr(q, "")

	weights := search.Weights(a.cfg.Search.Weights)
	acc := search.RegexAccuracy
	results := make([]search.Result, 0, 256)

	// 1) 自定义命令：按名字匹配。普通模式那套"激活词"在正则里没有意义。
	for _, cmd := range a.cfg.Commands {
		name := []byte(cmd.Name)
		if !rx.Maybe(name) || !rx.Match(name) {
			continue
		}
		dir := a.lastFollowDir()
		results = append(results, search.Result{
			Item: search.Item{
				Name:     cmd.Name,
				Path:     cmd.Exec,
				Hint:     "命令：" + cmd.Exec,
				Kind:     search.KindCommand,
				Command:  cmd.Exec,
				Args:     expandArgs(cmd.Args, nil, dir),
				Dir:      expandTemplate(cmd.Dir, nil, dir),
				KeepOpen: cmd.KeepOpen,
			},
			TypeScore: 1,
			Accuracy:  acc,
			Score:     search.Combine(weights, 0.5, 1, acc),
		})
	}

	// 2) 已安装应用：只认名字。拼音首字母 / 全拼在这条路上一次都不查。
	if apps := a.apps.Load(); apps != nil {
		for i, app := range *apps {
			name := []byte(app.Name)
			if !rx.Maybe(name) || !rx.Match(name) {
				continue
			}
			freq := a.hist.Score(app.Path)
			ts := search.TypeScore(".exe", search.KindApp, a.cfg.Search.TypeOrder)
			results = append(results, search.Result{
				Item: search.Item{
					Name:     app.Name,
					Path:     app.Path,
					Ext:      ".exe",
					Kind:     search.KindApp,
					Hint:     "应用",
					AppIndex: i,
				},
				Frequency: freq,
				TypeScore: ts,
				Accuracy:  acc,
				Score:     search.Combine(weights, freq, ts, acc),
			})
		}
	}

	// 3) 索引条目
	tab := a.cat.Load()
	if tab == nil || tab.Len() == 0 {
		return a.finish(results)
	}
	matched := 0
	for i := 0; i < tab.Len(); i++ {
		// 全量扫一条不筛的正则要几百毫秒，用户早换关键字了就别把这遍跑完：
		// refilter 每敲一个键都会起一个 goroutine，旧的不退出就会互相抢 CPU。
		if i&2047 == 0 && strings.TrimSpace(a.query) != q {
			return nil
		}
		name := tab.NameBytes(i)
		if !rx.Maybe(name) || !rx.Match(name) {
			continue
		}
		// 命中之后再判墓碑：正则能命中几千条，上溯这几千次约 1ms，
		// 放在预筛之前就对 179 万条都做了。
		if tab.Dead(int32(i)) {
			continue
		}
		path := tab.PathOf(int32(i))
		ext := tab.Ext(i)
		freq := a.hist.Score(path)
		ts := search.TypeScore(ext, toSearchKind(tab.Nodes[i].Kind), a.cfg.Search.TypeOrder)
		results = append(results, search.Result{
			Item: search.Item{
				Name: tab.Name(i),
				Path: path,
				Ext:  ext,
				Kind: toSearchKind(tab.Nodes[i].Kind),
				Hint: tab.Dir(int32(i)),
			},
			Frequency: freq,
			TypeScore: ts,
			Accuracy:  acc,
			Score:     search.Combine(weights, freq, ts, acc),
		})
		matched++
		if matched >= maxRegexHits {
			break
		}
	}
	return a.finish(results)
}

// setRegexErr 记住（或清空）上一次正则的语法错误。
// 和 refilter 一样校验查询没变：搜索在后台 goroutine 里跑，回来时用户可能已经改了输入。
func (a *application) setRegexErr(q, msg string) {
	if a.panel == nil {
		a.regexErr = msg
		return
	}
	a.panel.Update(func() {
		if strings.TrimSpace(a.query) == q {
			a.regexErr = msg
		}
	})
}

func (a *application) truncate(results []search.Result) []search.Result {
	max := a.cfg.Search.MaxResults
	if max > 0 && len(results) > max {
		return results[:max]
	}
	return results
}

// finish 是搜索的最后一步：排序 → 截到 MaxResults → 打散应用的连续块。
// 打散不够：输入 d 这种宽泛查询时，普通文件（type 0.20）分数天然低于应用，
// 连前 80 都进不来，用户看到的就是"只显示应用"。所以截断时给非应用留一份保底名额。
func (a *application) finish(results []search.Result) []search.Result {
	search.Sort(results)
	return search.Balance(a.truncateMixed(results), maxAppRun)
}

// truncateMixed 按配额截断：应用最多占 (max - 非应用保底) 条，剩下的名额强制留给文件/目录。
// max<=0 或结果本来就不超限时原样返回。
func (a *application) truncateMixed(results []search.Result) []search.Result {
	max := a.cfg.Search.MaxResults
	if max <= 0 || len(results) <= max {
		return results
	}
	var apps, others []search.Result
	for _, r := range results {
		if r.Kind == search.KindApp {
			apps = append(apps, r)
		} else {
			others = append(others, r)
		}
	}
	if len(others) == 0 {
		return results[:max]
	}
	reserve := max / 4 // 默认 80 条里留出 20 条给非应用
	if reserve > len(others) {
		reserve = len(others)
	}
	appQuota := max - reserve
	if appQuota > len(apps) {
		appQuota = len(apps)
	}
	out := make([]search.Result, 0, max)
	out = append(out, apps[:appQuota]...)
	othersQuota := max - appQuota
	if othersQuota > len(others) {
		othersQuota = len(others)
	}
	out = append(out, others[:othersQuota]...)
	search.Sort(out)
	return out
}

func toSearchKind(k index.Kind) search.Kind {
	if k == index.KindDir {
		return search.KindDir
	}
	return search.KindFile
}

// pyContains 是拼音预筛：名字的拼音首字母串里包含全部 token 才放行。
// 只做"包含"不做散落子序列——后者在百万条目上太贵，收益也低。
// 只做"包含"不做散落子序列——后者在百万条目上太贵，收益也低。
func pyContains(py string, tokens []string) bool {
	if py == "" {
		return false
	}
	for _, t := range tokens {
		if !isAlphaNum(t) || !strings.Contains(py, t) {
			return false
		}
	}
	return true
}

func isAlphaNum(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}

func (a *application) choose(i int) {
	a.chosen = max(0, min(i, len(a.results)-1))
	if len(a.results) > 0 {
		a.list.ScrollIntoView(a.chosen)
	}
}

// keyStep 是首页的方向键：按下的那一下把选中项挪 step 格。
// 用按住状态做边沿检测，而不是 Shortcut——焦点在搜索框里时左右键会被输入框吃掉。
func (a *application) keyStep(c *ui.Context, held *bool, key ui.Key, step int) {
	now := c.KeyDown(key)
	if now && !*held {
		a.choose(a.chosen + step)
	}
	*held = now
}

// open 执行选中的结果并隐藏面板。
func (a *application) open() {
	if a.chosen < 0 || a.chosen >= len(a.results) {
		return
	}
	r := a.results[a.chosen]
	entry := entryOf(r)
	if a.offline {
		// 视图测试（ui.Tester 没有 native 主循环）：Shell.OpenPath / LaunchApp 会永久阻塞，
		// 这里只记一笔历史，让"点了有没有反应"可被断言。
		a.hist.TouchEntry(entry)
		return
	}
	// 命令可以要求"执行后别收面板"（KeepOpen）：面板先收再跑，是默认行为。
	if !(r.Kind == search.KindCommand && r.KeepOpen) {
		a.panel.Hide()
	}

	// 历史要带着"这是什么"一起记，否则重启后还原不出正确的名字和图标。
	log.Printf("open: %s (%s)", r.Name, r.Path)

	if r.Kind == search.KindCommand {
		prog, args := launchSpec(r.Command, r.Args)
		cmd := exec.Command(prog, args...)
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: false}
		if r.Dir != "" {
			cmd.Dir = r.Dir
		}
		if err := cmd.Start(); err != nil {
			log.Printf("运行命令 %s %v（工作目录 %q）: %v", prog, args, r.Dir, err)
			return
		}
		a.hist.TouchEntry(entry)
		return
	}

	a.hist.TouchEntry(entry)
	if r.Kind == search.KindApp {
		if apps := a.apps.Load(); apps != nil {
			for _, app := range *apps {
				if app.Path == r.Path || app.ID == r.Path {
					if err := mygo.Shell.LaunchApp(app.InstalledApp); err != nil {
						log.Printf("启动 %s: %v", r.Name, err)
					}
					return
				}
			}
		}
	}
	if err := mygo.Shell.OpenPath(r.Path); err != nil {
		log.Printf("打开 %s: %v", r.Path, err)
	}
}

// togglePin 把选中项固定到首页 / 取消固定（Pane 的 Ctrl+K）。
// 固定状态存在 history.Entry 上，随 history.json 一起落盘。
func (a *application) togglePin() {
	if a.chosen < 0 || a.chosen >= len(a.results) {
		return
	}
	r := a.results[a.chosen]
	key := r.Path
	if key == "" {
		key = r.Command
	}
	if key == "" {
		return
	}
	if a.hist.Count(key) == 0 {
		// 没打开过的项在历史里还不存在，先记一条再去固定（否则无处存标记）。
		e := entryOf(r)
		if e.Path == "" {
			e.Path = key
		}
		a.hist.TouchEntry(e)
	}
	pinned, ok := a.hist.TogglePin(key)
	if !ok {
		return
	}
	log.Printf("pin: %s -> %v", r.Name, pinned)
	if pinned {
		a.setStatus("已固定到首页：" + r.Name)
	} else {
		a.setStatus("已取消固定：" + r.Name)
	}
	// 首页要把固定项挪进「已固定」区，所以重建结果，并把选中落回原来那一项。
	if a.query == "" {
		a.refilter()
		for i := range a.results {
			if a.results[i].Path == r.Path && a.results[i].Name == r.Name {
				a.chosen = i
				break
			}
		}
	}
}

// openFolder 打开选中项所在的文件夹（文件开父目录，目录开自身）。
func (a *application) openFolder() {
	if a.chosen < 0 || a.chosen >= len(a.results) {
		return
	}
	r := a.results[a.chosen]
	if r.Kind == search.KindCommand || r.Path == "" || strings.HasPrefix(r.Path, "shell:") {
		a.setStatus("这一项没有所在文件夹")
		return
	}
	target := r.Path
	if r.Kind != search.KindDir {
		target = filepath.Dir(r.Path)
	}
	if err := mygo.Shell.OpenPath(target); err != nil {
		a.setStatus("打开文件夹失败：" + err.Error())
		return
	}
	a.panel.Hide()
}

// copyDir 把选中项所在目录放进剪贴板。对话框跟随是自动的（见 win.DialogFollow），
// 这条留作后备：对话框被别的程序挡住、或那套 WM_SETTEXT 被外壳拒掉时还能手工粘一次
// （框里 Ctrl+L → Ctrl+V → 回车）。
func (a *application) copyDir(c *ui.Context) {
	if a.offline {
		return
	}
	if a.chosen < 0 || a.chosen >= len(a.results) {
		a.setStatus("没有选中项")
		return
	}
	dir := dirOf(a.results[a.chosen])
	if dir == "" {
		a.setStatus("这一项没有路径")
		return
	}
	c.WriteClipboardThen(dir, func(err error) {
		if err != nil {
			a.setStatus("复制失败：" + err.Error())
			return
		}
		a.setStatus("已复制目录：" + dir)
	})
}

// dirOf 是一条结果对应的目录：目录给自身，文件给所在目录，虚拟项（shell:）没有目录。
func dirOf(r search.Result) string {
	if r.Path == "" || strings.HasPrefix(r.Path, "shell:") {
		return ""
	}
	if r.Kind == search.KindDir {
		return r.Path
	}
	return filepath.Dir(r.Path)
}

// entryOf 把一条搜索结果转成历史条目：保留名字、类型、扩展名与提示。
func entryOf(r search.Result) history.Entry {
	return history.Entry{
		Path: r.Path,
		Name: r.Name,
		Kind: historyKind(r.Kind),
		Ext:  r.Ext,
		Hint: r.Hint,
		Exec: r.Command,
		Args: r.Args,
	}
}

func historyKind(k search.Kind) history.Kind {
	switch k {
	case search.KindApp:
		return history.KindApp
	case search.KindDir:
		return history.KindDir
	case search.KindCommand:
		return history.KindCommand
	}
	return history.KindFile
}

// show 在鼠标所在显示器居中偏上位置显示面板。
func (a *application) show() {
	if a.cfg.Hotkey.DisableInFullscreen && win.ForegroundInfo().Fullscreen {
		log.Print("前台是全屏应用，忽略唤出")
		return
	}
	d := mygo.Screen.DisplayNearestPoint(mygo.Screen.CursorScreenPoint())
	w, h := a.panel.Size()
	a.panel.SetPosition(d.WorkArea.X+(d.WorkArea.Width-w)/2, d.WorkArea.Y+(d.WorkArea.Height-h)/3)
	a.query = ""
	// 唤出前把索引追一次：这是"刚存的文件马上能搜到"的关键一步。
	// 没变化时只是两次 stat（微秒级）；有变化才复制快照叠增量（约 15ms），
	// 而这点时间藏在面板动画里，感知不到。
	a.refreshIndex(true)
	a.refilter()
	a.panel.Show()
	// Show 内部只调 SetForegroundWindow，从任务栏/别的窗口切回来时会被前台锁拒绝，
	// 所以这里自己带上 AttachThreadInput 再抢一次；拿不到也不影响面板显示。
	if !forceForeground(a.panel.NativeHandle()) {
		log.Print("唤出：抢前台失败（退化为普通显示）")
	}
	// 焦点要等窗口真的成为前台之后再交给输入框，否则焦点会被重新激活的窗口抢走。
	a.panel.Update(func() { a.input.Focus() })
	log.Print("toggle: 显示面板")
}

func (a *application) toggle() {
	if a.panel.IsVisible() {
		// 可见但已经不是前台（例如刚点过任务栏）时，这一下应当"取回焦点"，
		// 而不是把面板收起来——否则用户按了快捷键看到面板消失，还得再按一次。
		if a.panel.IsFocused() {
			log.Print("toggle: 隐藏面板")
			a.panel.Hide()
			return
		}
		log.Print("toggle: 面板可见但未聚焦，取回焦点")
		if !forceForeground(a.panel.NativeHandle()) {
			log.Print("唤出：抢前台失败（退化为普通显示）")
		}
		a.panel.Update(func() { a.input.Focus() })
		return
	}
	a.show()
}

func (a *application) ready() {
	a.icons = map[string]*ui.Bitmap{}
	// 显示器缩放只能在 mygo 起来之后问（App 初始化前调 Screen.Displays 会 panic），
	// 所以放这里算一次缓存住，视图里不再现问。
	a.scale = screenScale()

	a.panel = mygo.NewWindow(mygo.WindowOptions{
		Title:         "Baozi",
		Panel:         true,
		Hidden:        true,
		Frameless:     true,
		Transparent:   true,
		Vibrancy:      mygo.VibrancyGlass,
		CornerRadius:  16,
		Width:         panelWidth,
		Height:        panelHeight,
		DisableResize: true,
		Content:       ui.View(a.view),
	})
	a.panel.OnClose(func(e *mygo.CloseEvent) {
		// 真正退出时不能拦：mygo 的 Quit 是一个个关窗口，
		// 只要有窗口在 OnClose 里 PreventDefault，整个 Quit 就被取消，
		// 表现就是"点了退出，进程还在"。
		if a.quitting.Load() {
			return
		}
		e.PreventDefault()
		a.panel.Hide()
	})
	a.panel.OnBlur(a.panel.Hide)

	// 1) 索引：先用上次快照秒开，再按配置决定是否重扫
	a.bootIndex()

	// 2) 应用列表：读开始菜单，几十毫秒。拼音串在这里一次算好。
	go func() {
		raw, err := mygo.Shell.InstalledApps()
		if err != nil {
			log.Printf("读取已安装应用: %v", err)
			return
		}
		entries := make([]appEntry, 0, len(raw))
		for _, app := range raw {
			entries = append(entries, appEntry{
				InstalledApp: app,
				initials:     pinyin.Initials(app.Name),
				full:         pinyin.Full(app.Name),
				alias:        search.AppAlias(app.ID),
			})
		}
		a.apps.Store(&entries)
		if debugKeys {
			log.Printf("应用: %d 个", len(entries))
		}
	}()

	// 3) 钩子：双击 Ctrl 走低级键盘钩子，组合键走 RegisterHotKey 兜底
	a.startHook()
	if a.cfg.Hotkey.Combo != "" {
		if err := mygo.GlobalShortcut.Register(a.cfg.Hotkey.Combo, a.toggle); err != nil {
			log.Printf("注册组合键 %s 失败: %v", a.cfg.Hotkey.Combo, err)
		}
	}

	// 开机自启的开关状态问系统一次（后面托盘/设置都读缓存，不再走主循环）。
	a.openAtLogin.Store(mygo.App.OpenAtLogin())
	log.Printf("开机自启: %v", a.openAtLogin.Load())

	a.setUpTray()
	a.settings = newSettingsWindow(a)
	a.share = newShareWindow(a)

	// 文件对话框跟随：记住文件管理器最近待过的目录，那个「打开 / 另存为」框再回到前台时
	// 自动把路径送过去（Lertaro 的 IActivePathCollector + FileDialogNavigationTracker）。
	// 目录来源六种（资源管理器 / TC / Opus / XYplorer / xplorer2 / tessoa），设置里可逐个关掉；
	// 关掉总开关（config.dialogFollow = false）就完全不碰别人的窗口。
	a.syncFollow()

	mygo.App.OnWillQuit(func(*mygo.QuitEvent) {
		a.shutdown()
	})
	// 开机自启拉起时不弹面板，直接待在托盘（静默启动）。
	// BAOZI_HEADLESS=1 给冒烟测试留下同一条路径，不用真去改注册表。
	autoLogin := mygo.App.WasOpenedAtLogin()
	if !autoLogin && os.Getenv("BAOZI_HEADLESS") == "" {
		a.show()
	} else {
		log.Printf("启动：来自开机自启，面板保持隐藏（托盘可用，双击 Ctrl 随时唤出）")
	}
	// 命令行直达某个窗口：BAOZI_OPEN=settings / share。
	// 必须延迟到事件循环里再 Show：窗口刚创建完时 Show 会被忽略（表现为窗口存在但一直隐藏）。
	if want := strings.ToLower(os.Getenv("BAOZI_OPEN")); want != "" {
		time.AfterFunc(700*time.Millisecond, func() {
			a.panel.Update(func() {
				switch want {
				case "settings":
					a.openSettings()
				case "share":
					a.openShare()
				}
			})
		})
	}

	// BAOZI_MEMSTAT=10s：周期性打内存数字，调内存时用（默认不开）。
	if d := os.Getenv("BAOZI_MEMSTAT"); d != "" {
		if dur, err := time.ParseDuration(d); err == nil && dur > 0 {
			go func() {
				for range time.Tick(dur) {
					var m runtime.MemStats
					runtime.ReadMemStats(&m)
					log.Printf("内存: heap=%.0fMB inuse=%.0fMB sys=%.0fMB objs=%d gc=%d",
						float64(m.HeapAlloc)/(1<<20), float64(m.HeapInuse)/(1<<20),
						float64(m.HeapSys)/(1<<20), m.HeapObjects, m.NumGC)
				}
			}()
		}
	}

	// BAOZI_PPROF=1：开一个 127.0.0.1:6060 的 pprof 端点，查内存/CPU 真实去向。
	if os.Getenv("BAOZI_PPROF") != "" {
		go func() {
			log.Printf("pprof: http://127.0.0.1:6060/debug/pprof/")
			if err := http.ListenAndServe("127.0.0.1:6060", nil); err != nil {
				log.Printf("pprof: %v", err)
			}
		}()
	}

	// BAOZI_UNINSTALL=1：无人值守卸载（摘自启 + 删数据目录 + 退出），跳过确认框。
	// 给冒烟和"托盘点不出来时"留一条命令行退路；正常用户走托盘那一项。
	if os.Getenv("BAOZI_UNINSTALL") != "" {
		time.AfterFunc(time.Second, a.doUninstall)
	}

	// BAOZI_AUTOQUIT=2s：冒烟用，到点走一遍真实退出流程（和托盘「退出」同一条路）。
	if d := os.Getenv("BAOZI_AUTOQUIT"); d != "" {
		if dur, err := time.ParseDuration(d); err == nil {
			time.AfterFunc(dur, func() {
				a.panel.Update(func() { a.quit() })
			})
		}
	}
}

// indexFile 是索引快照文件。从 v3 起不再用 gob，改叫 .bin 以免和老文件混淆。
func (a *application) indexFile() string   { return filepath.Join(a.dataDir, "index.bin") }
func (a *application) historyFile() string { return filepath.Join(a.dataDir, "history.json") }

func (a *application) bootIndex() {
	alive := a.serviceAlive()
	tab, roots, err := index.Load(a.indexFile())
	n := 0
	if tab != nil {
		n = tab.Len()
	}
	if err == nil && n > 0 {
		a.idxReplace(tab)
		// 记下这份快照的"代次"，好让增量知道能不能往上叠
		if info, e := os.Stat(a.indexFile()); e == nil {
			a.idxMtime = info.ModTime()
		}
		a.idxBase = readStateBase(a.dataDir)
		a.idxSeq = 0
		a.idxOps = 0
		if alive {
			a.setStatus(fmt.Sprintf("%d 项 · USN 索引", n))
			log.Printf("索引: 服务模式，%d 项（来自 %s）", n, a.indexFile())
		} else {
			a.setStatus(fmt.Sprintf("%d 项 · 索引已就绪", n))
			log.Printf("索引: 快照 %d 项（目录扫描模式）", n)
		}
	} else {
		if err != nil && !os.IsNotExist(err) {
			log.Printf("读取索引: %v", err)
		}
		if alive {
			a.setStatus("索引服务正在建索引…")
		} else {
			a.setStatus("正在建立索引…")
		}
	}
	needScan := n == 0 || rootsChanged(roots, a.cfg.Index.Roots)
	// BAOZI_REINDEX=1 强制重建一次（改了索引规则/排除了新目录后用，不必删索引文件）。
	if os.Getenv("BAOZI_REINDEX") != "" {
		needScan = true
	}
	// 服务在跑的时候索引归它维护：主进程既不自己扫，也不去覆盖它刚写出来的快照。
	if alive {
		needScan = false
	}
	if !needScan && !alive && a.cfg.Index.RescanMinutes > 0 {
		if info, err := os.Stat(a.indexFile()); err == nil {
			needScan = time.Since(info.ModTime()) > time.Duration(a.cfg.Index.RescanMinutes)*time.Minute
		}
	}
	if needScan {
		go a.rescan()
	} else if n > 0 {
		// 命中上次快照：解压时的临时缓冲已经用完，把空闲页还回去，
		// 否则任务管理器会长期显示那个加载峰值。
		releaseMemory()
	}
	// 无论服务当前在不在跑都挂上监听：用户随时可能在设置里把它打开，
	// 打开后心跳一出现，这里就自动接管（自己扫的时候它会按心跳门控跳过）。
	a.watchIndexFile()
	// 启动时立刻追一次增量：index.bin 可能是几小时前写的，
	// 服务攒下来的新条目都在 index.delta 里（这一步只叠指令，不重新读盘）。
	if alive {
		go a.refreshIndex(false)
	}
}

// rootsChanged 判断索引快照覆盖的根目录是否与当前配置一致。
func rootsChanged(old, want []string) bool {
	if len(old) != len(want) {
		return true
	}
	for i := range old {
		if !strings.EqualFold(old[i], want[i]) {
			return true
		}
	}
	return false
}

// idxReplace 换索引快照。名字 arena 和 nodes 一起换，旧快照被替换后依然完整。
func (a *application) idxReplace(t *index.Table) {
	a.cat.Store(t)
}

// rescan 后台全量扫描；结束落盘，供下次秒开。
func (a *application) rescan() {
	// 索引服务在跑时索引归它管：这里硬扫一遍既慢（WalkDir 分钟级），
	// 写回去的快照还会被服务的下一轮覆盖。改成给它放一个"现在就重建"的信标。
	if a.serviceAlive() {
		if err := os.WriteFile(filepath.Join(a.dataDir, "index.rebuild"), []byte(time.Now().Format(time.RFC3339)), 0o644); err != nil {
			log.Printf("请求服务重建: %v", err)
		}
		a.setStatus("已请求索引服务重建")
		return
	}

	a.scanWG.Add(1)
	defer a.scanWG.Done()

	a.scanMu.Lock()
	if a.scanOff != nil {
		a.scanOff()
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.scanOff = cancel
	a.scanMu.Unlock()
	defer cancel()

	start := time.Now()
	progress := func(n int) {
		a.panel.Update(func() { a.status = fmt.Sprintf("正在建立索引… %d 项", n) })
	}
	tab, err := index.Build(ctx, a.cfg.Index, progress)
	if err != nil && ctx.Err() == nil {
		log.Printf("建立索引: %v", err)
		a.setStatus("索引失败，见日志")
		return
	}
	// 被取消（退出、卸载、或又点了一次重建）时不能落盘：
	// 半份索引一旦写进索引文件，下次启动会当完整索引加载，搜出来就缺东西。
	// 卸载时这条尤其致命——RemoveAll 删完目录，这里又把索引文件写回来。
	if ctx.Err() != nil {
		return
	}
	if tab.Len() == 0 {
		return
	}
	a.idxReplace(tab)
	if err := index.Save(a.indexFile(), tab, a.cfg.Index.Roots); err != nil {
		log.Printf("保存索引: %v", err)
	}
	a.setStatus(fmt.Sprintf("%d 项 · 扫描 %s", tab.Len(), time.Since(start).Round(time.Second)))
	releaseMemory()
}

// releaseMemory 把加载/扫描期的临时峰值还给系统。
// Go 拿到 OS 的内存不会主动退：179 万条的 gob 解码会堆出几百 MB 垃圾，
// 不在关键节点踩一脚 FreeOSMemory，任务管理器就一直显示那个峰值。
// 延迟 2s 再跑，别让 GC 的 STW 卡住首帧。
func releaseMemory() {
	time.AfterFunc(2*time.Second, func() {
		debug.FreeOSMemory()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		log.Printf("内存: 已归还空闲页，heap=%.0fMB sys=%.0fMB",
			float64(m.HeapAlloc)/(1<<20), float64(m.HeapSys)/(1<<20))
	})
}

func (a *application) setStatus(s string) {
	if a.panel == nil {
		a.status = s
		return
	}
	a.panel.Update(func() { a.status = s })
}

// startHook 拉起 baozi-hook.exe，读它的 stdout。
func (a *application) startHook() {
	exe, err := os.Executable()
	if err != nil {
		log.Printf("定位自身: %v", err)
		return
	}
	hookPath := filepath.Join(filepath.Dir(exe), "baozi-hook.exe")
	if _, err := os.Stat(hookPath); err != nil {
		a.setStatus("提示：未找到 baozi-hook.exe，双击 Ctrl 不可用")
		return
	}
	cmd := exec.Command(hookPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		log.Printf("钩子 stdin: %v", err)
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Printf("钩子 stdout: %v", err)
		return
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		log.Printf("启动钩子: %v", err)
		a.setStatus("钩子启动失败，双击 Ctrl 不可用")
		return
	}
	a.hook, a.hookIn = cmd, stdin

	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			switch strings.TrimSpace(scanner.Text()) {
			case "toggle":
				a.toggle()
			case "ready":
				a.setStatus("双击 Ctrl 已就绪")
			case "error":
				a.setStatus("钩子初始化失败")
			}
		}
	}()
}

// syncFollow 按当前配置启停跟随器，并刷新"哪些来源开着"的快照。
// lastFollowDir 返回跟随器记住的文件管理器目录，供命令里的 {dir} 用。
// 跟随没开（或还没读到目录）时是空串——占位符就替换成空，不留一个假的路径。
func (a *application) lastFollowDir() string {
	if a.follow == nil {
		return ""
	}
	return a.follow.LastDir()
}

// 设置页保存后调它：总开关一改就真的停/起，来源勾选改了下次轮询立刻生效。
func (a *application) syncFollow() {
	a.refreshPathSources()

	if !a.cfg.DialogFollowOn() {
		if a.followCancel != nil {
			a.followCancel()
			a.followCancel = nil
			a.follow = nil
			log.Print("对话框跟随: 已停止")
		}
		return
	}
	if a.follow != nil {
		return // 已经在跑，来源开关通过 sources 快照热更新
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.followCancel = cancel
	a.follow = win.NewDialogFollow(func(ev win.FollowEvent) {
		log.Printf("对话框跟随: %#x -> %s", ev.HWND, ev.Dir)
	}, log.Printf, a.pathSourceOn)
	a.follow.Start(ctx)
	log.Print("对话框跟随: 已启动（来源：" + a.enabledSourceSummary() + "）")
}

// refreshPathSources 把当前配置里的来源开关拍成一份快照给跟随器读。
func (a *application) refreshPathSources() {
	m := make(map[string]bool, len(win.SourceNames()))
	for _, kind := range win.SourceNames() {
		m[kind] = a.cfg.PathSourceEnabled(kind)
	}
	a.sources.Store(&m)
}

// pathSourceOn 是给跟随器的查询接口：这个来源现在开不开。
func (a *application) pathSourceOn(kind string) bool {
	m := a.sources.Load()
	if m == nil {
		return true
	}
	return (*m)[kind]
}

// enabledSourceSummary 只用于日志：把开着的来源列出来，关掉的标出来。
func (a *application) enabledSourceSummary() string {
	parts := make([]string, 0, len(win.SourceNames()))
	for _, kind := range win.SourceNames() {
		if a.pathSourceOn(kind) {
			parts = append(parts, win.SourceLabel(kind))
		}
	}
	if len(parts) == 0 {
		return "无（只剩跟随框架，不会读任何窗口）"
	}
	return strings.Join(parts, " / ")
}

func (a *application) shutdown() {
	a.quitOnce.Do(func() {
		log.Printf("收尾: 停掉对话框跟随")
		if a.followCancel != nil {
			a.followCancel()
		}
		log.Printf("收尾: 取消索引扫描")
		if a.scanOff != nil {
			a.scanOff()
		}
		log.Printf("收尾: 通知钩子退出")
		if a.hookIn != nil {
			_ = a.hookIn.Close() // 钩子读到 EOF 自己退出
		}
		if a.hook != nil && a.hook.Process != nil {
			done := make(chan struct{})
			go func() { _ = a.hook.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				_ = a.hook.Process.Kill()
			}
		}
		log.Printf("收尾: 历史落盘")
		// 卸载时跳过：数据目录马上就要删掉，落盘只会把 history.json 又写回来。
		if a.hist != nil && !a.uninstalling.Load() {
			if err := a.hist.Flush(); err != nil {
				log.Printf("保存历史: %v", err)
			}
		}
		log.Printf("收尾: 完成")
	})
}

func (a *application) setUpTray() {
	tray, err := mygo.NewTray(mygo.TrayOptions{Icon: trayIcon(), ToolTip: "Baozi"})
	if err != nil {
		log.Print("托盘不可用: ", err)
		return
	}
	a.tray = tray
	menu := mygo.NewMenu([]*mygo.MenuItem{
		{Label: "显示 Baozi", Accelerator: a.cfg.Hotkey.Combo, Click: func(*mygo.MenuItem, *mygo.Window) { a.show() }},
		{Label: "文件互传…", Click: func(*mygo.MenuItem, *mygo.Window) { a.openShare() }},
		{Label: "设置…", Click: func(*mygo.MenuItem, *mygo.Window) { a.openSettings() }},
		mygo.Separator(),
		{Label: "重新建立索引", Click: func(*mygo.MenuItem, *mygo.Window) { go a.rescan() }},
		{Label: "打开配置文件", Click: func(*mygo.MenuItem, *mygo.Window) {
			if err := mygo.Shell.OpenPath(a.cfgPath); err != nil {
				log.Printf("打开配置: %v", err)
			}
		}},
		mygo.Separator(),
		{Label: "开机启动", Type: mygo.MenuItemCheckbox, ID: "login", Checked: a.openAtLogin.Load(),
			Click: func(it *mygo.MenuItem, _ *mygo.Window) {
				if err := mygo.App.SetOpenAtLogin(it.Checked); err != nil {
					log.Printf("开机自启: %v", err)
					// 失败就把勾选拨回去：mygo 已经把状态翻过了，不纠正会骗人。
					it.SetChecked(!it.Checked)
					return
				}
				a.openAtLogin.Store(it.Checked)
				log.Printf("开机自启: %v", it.Checked)
				// 设置窗口开着的话，勾选状态要跟着变。
				if a.settings != nil {
					a.settings.load()
					a.settings.win.Update(func() {})
				}
			}},
		mygo.Separator(),
		// 卸载：摘自启 + 删数据目录，程序文件留着。会先弹系统确认框。
		// 用 go 起 goroutine：确认框是模态的，别把主线程的消息循环占了。
		{Label: "卸载：清除自启与数据…", Click: func(*mygo.MenuItem, *mygo.Window) { go a.uninstall() }},
		mygo.Separator(),
		// 不用 RoleQuit：它的行为绑在菜单的角色机制上，出问题时既看不到日志也不好接管。
		// 这里显式收口到 a.quit()，退出前后的动作都自己掌控。
		{Label: "退出 Baozi", Click: func(*mygo.MenuItem, *mygo.Window) { a.quit() }},
	})
	a.tray.SetMenu(menu)
	a.menu = menu
	// 托盘图标要在 native 调用还活着的时候删掉，所以放在 OnWillQuit（不是 OnQuit）。
	mygo.App.OnWillQuit(func(*mygo.QuitEvent) {
		a.shutdown()
		if a.tray != nil {
			a.tray.Destroy()
		}
	})
}

// quit 是唯一的退出入口：托盘「退出」走这里，BAOZI_AUTOQUIT 也走这里。
// 先把 quitting 置真（让窗口的 OnClose 放行），再做收尾，最后让 mygo 关窗口停循环。
func (a *application) quit() {
	log.Printf("退出：开始收尾")
	a.quitting.Store(true)
	a.shutdown()
	log.Printf("退出：收尾完成，请求关闭窗口")
	mygo.App.Quit()
}

func main() {
	if !mygo.App.RequestSingleInstanceLock() {
		return
	}
	// 常驻后台的小工具，堆不该按 GOGC=100 涨到活数据的两倍。
	// 索引是只读的（整表替换），存活量基本恒定，压低 GOGC 只是用一点 CPU 换几百 MB。
	debug.SetGCPercent(40)
	app := &application{icons: map[string]*ui.Bitmap{}}
	setupLog()

	cfg, cfgPath, err := config.Load()
	if err != nil {
		log.Fatalf("读取配置: %v", err)
	}
	dir, err := config.Dir()
	if err != nil {
		log.Fatalf("数据目录: %v", err)
	}
	app.cfg, app.cfgPath, app.dataDir = cfg, cfgPath, dir

	hist, err := history.Open(app.historyFile(), cfg.History.MaxItems, 400*time.Millisecond)
	if err != nil {
		log.Fatalf("读取历史: %v", err)
	}
	app.hist = hist

	mygo.App.SetName("Baozi")
	mygo.App.SetActivationPolicy(mygo.ActivationPolicyAccessory)
	mygo.App.OnWindowAllClosed(func() {})
	mygo.App.OnSecondInstance(func([]string, string) { app.show() })
	mygo.App.WhenReady(app.ready)

	log.Printf("配置: %s", cfgPath)
	if err := mygo.App.Run(); err != nil {
		log.Fatal(err)
	}
}

// dualWriter 把日志同时送到 stderr 和日志文件。
// 打包成 windowsgui 后进程没有控制台，stderr 写不进去也不能让写文件跟着失败。
type dualWriter struct{ f *os.File }

func (d dualWriter) Write(p []byte) (int, error) {
	if d.f != nil {
		_, _ = d.f.Write(p)
	}
	_, _ = os.Stderr.Write(p)
	return len(p), nil
}

// logFile 是当前日志文件句柄。卸载时要先关掉它：Windows 不删正在写入的文件，
// 留着句柄 RemoveAll 数据目录会失败（或删一半）。
var logFile *os.File

// setupLog 给 GUI 版本留一个能查的日志：%LOCALAPPDATA%\Baozi\baozi.log。
// 超过 2MB 直接清空重来（不做轮转，这个文件只用来排障）。
func setupLog() {
	dir, err := config.Dir()
	if err != nil {
		return
	}
	path := filepath.Join(dir, "baozi.log")
	if st, err := os.Stat(path); err == nil && st.Size() > 2<<20 {
		_ = os.Remove(path)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	logFile = f
	log.SetOutput(dualWriter{f: f})
}

// closeLog 放开日志句柄。之后的日志只走 stderr（GUI 子系统下等于丢弃，此时进程也快退了）。
func closeLog() {
	if logFile != nil {
		_ = logFile.Close()
		logFile = nil
	}
	log.SetOutput(os.Stderr)
}
