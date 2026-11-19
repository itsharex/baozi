package main

import (
	"fmt"
	"log"
	"math"
	"strings"
	"time"
	"unicode"

	"baozi/internal/config"
	"baozi/internal/win"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
)

// settingsWindow 是图形化配置。目标是"点几下就完事"：
// 常用的四项（热键、权重、索引范围、自定义命令）都能在这里改，
// 保存后立即生效，不用手动编辑 config.json。
type settingsWindow struct {
	app *application
	win *mygo.Window

	// 编辑中的副本，只有点"保存并应用"才写回配置。
	doubleCtrl  bool
	interval    float64
	combo       string
	fullscreen  bool
	openAtLogin bool
	// 启动时不弹面板：只留托盘和双击 Ctrl。
	hideOnStart bool

	// 外观：三项各自"开关 + 程度"。值都用"人话单位"（DIP / 百分比），
	// 保存时由 main.go 的 applyAppearance 折算给绘制层。
	radiusOn   bool
	radiusVal  float64
	opacityOn  bool
	opacityVal float64
	shadowOn   bool
	shadowVal  float64

	wFreq float64
	wType float64
	wAcc  float64

	typeOrder  string
	maxResults float64
	// thumbnails 决定图片/视频显示内容缩略图还是线框图标。
	thumbnails bool
	// iconScale 是图标的倍数（1 = 100%）。界面给几个台阶，也能手输。
	iconScale float64
	// iconPreset 是下拉里当前选中的那一项（某个台阶标签，或「自定义」）。
	// 单独记一份，是为了让"选了自定义"这个状态活过每一帧重绘。
	iconPreset string

	roots         string
	excludes      string
	includeHidden bool
	rescanMinutes float64

	// 文件对话框跟随：总开关 + 每一种目录来源各一个勾选
	followOn    bool
	srcExplorer bool
	srcTotalCmd bool
	srcOpus     bool
	srcXyplorer bool
	srcXplorer2 bool
	srcTessoa   bool

	// 自定义命令：结构化编辑（左列选中哪条，右列就改哪条）。
	cmds     []config.Command
	cmdSel   int
	cmdList  ui.ListState
	cmdAlias string // 编辑器里的"激活词"，逗号分隔
	cmdArgs  string // 编辑器里的"参数"，一行一个
	cmdSync  int    // 编辑器上次从哪一条同步来的，-1 表示要重新同步

	// 左栏导航：navSel 是选中的那一项，navList 是它的列表状态（键盘上下也能选）。
	// navPrev 记上一帧选中的是谁，只有变了才滚；navJump 是"这一帧要滚到哪一段"，
	// -1 表示不动——ScrollIntoView 每帧都调会让人没法手动滚走。
	navSel  int
	navList ui.ListState
	navPrev int
	navJump int

	message string
}

func newSettingsWindow(a *application) *settingsWindow {
	s := &settingsWindow{app: a}
	// 左栏默认停在第一项，navPrev 跟它一样，免得一打开就滚一下；
	// navJump 用 -1 起，表示"这帧别动"。
	s.navPrev = secHotkey
	s.navJump = -1
	s.navList.Selected = &s.navSel
	s.win = mygo.NewWindow(mygo.WindowOptions{
		Title: "Baozi 设置",
		// 必须 Hidden：窗口创建即显示，不带这一项的话 Baozi 一启动就弹一个设置窗口，
		// 它会占着任务栏、抢前台，还会让"点任务栏再双击 Ctrl"唤出的面板拿不到焦点。
		Hidden:       true,
		Width:        920,
		Height:       820,
		Vibrancy:     mygo.VibrancyGlass,
		MinWidth:     800,
		MinHeight:    560,
		Content:      ui.View(s.view),
		CornerRadius: 0,
	})
	s.win.OnClose(func(e *mygo.CloseEvent) {
		// 退出流程里必须放行，否则 mygo 的 Quit 会因为"窗口不肯关"被整体取消。
		if s.app.quitting.Load() {
			return
		}
		e.PreventDefault()
		s.win.Hide()
	})
	s.load()
	return s
}

// load 把当前配置拷进编辑区。
func (s *settingsWindow) load() {
	cfg := s.app.cfg
	s.doubleCtrl = cfg.Hotkey.DoubleCtrl
	s.interval = float64(cfg.Hotkey.DoubleCtrlIntervalMs)
	s.combo = cfg.Hotkey.Combo
	s.fullscreen = cfg.Hotkey.DisableInFullscreen
	// 自启状态不存 config.json：真正的开关在系统里（Windows 是 Run 键），
	// 用户可能从任务管理器禁用。状态在 ready() 里问过系统一次并缓存下来了
	// （application.openAtLogin），这里直接读缓存——App.OpenAtLogin 会走
	// mygo 主循环，在 ui.Tester 下没有主循环，调它会永久阻塞。
	s.openAtLogin = s.app.openAtLogin.Load()
	s.hideOnStart = cfg.HideOnStart()

	// 外观：开关和程度都读出来。程度不看开关、直接读存着的值 —— 关掉再打开
	// 得回到用户上次调到的那个数，而不是跳回默认。
	s.radiusOn = cfg.Appearance.Radius.On(true)
	s.radiusVal = float64(cfg.Appearance.Radius.Int(config.DefaultPanelRadius, 0, config.MaxPanelRadius))
	s.opacityOn = cfg.Appearance.Opacity.On(true)
	s.opacityVal = float64(cfg.Appearance.Opacity.Int(config.DefaultGlassTransparency, 0, 100))
	s.shadowOn = cfg.Appearance.Shadow.On(true)
	s.shadowVal = float64(cfg.Appearance.Shadow.Int(config.DefaultShadowPct, 0, 100))

	s.wFreq = cfg.Search.Weights.Frequency
	s.wType = cfg.Search.Weights.Type
	s.wAcc = cfg.Search.Weights.Accuracy
	s.typeOrder = strings.Join(cfg.Search.TypeOrder, ", ")
	s.maxResults = float64(cfg.Search.MaxResults)
	s.thumbnails = cfg.ThumbnailsOn()
	s.iconScale = iconScaleOf(cfg, s.app.scale)
	s.iconPreset = iconPresetLabel(s.iconScale)

	s.roots = strings.Join(cfg.Index.Roots, "\n")
	s.excludes = strings.Join(cfg.Index.Excludes, "\n")
	s.includeHidden = cfg.Index.IncludeHidden
	s.rescanMinutes = float64(cfg.Index.RescanMinutes)

	s.followOn = cfg.DialogFollowOn()
	s.srcExplorer = cfg.PathSourceEnabled(win.SourceExplorer)
	s.srcTotalCmd = cfg.PathSourceEnabled(win.SourceTotalCmd)
	s.srcOpus = cfg.PathSourceEnabled(win.SourceOpus)
	s.srcXyplorer = cfg.PathSourceEnabled(win.SourceXyplorer)
	s.srcXplorer2 = cfg.PathSourceEnabled(win.SourceXplorer2)
	s.srcTessoa = cfg.PathSourceEnabled(win.SourceTessoa)

	// 自定义命令：编辑器直接改结构化的清单，不再让用户拼竖线。
	s.cmds = append([]config.Command(nil), cfg.Commands...)
	s.cmdSel = 0
	if len(s.cmds) == 0 {
		s.cmdSel = -1
	}
	s.cmdList.Selected = &s.cmdSel
	s.cmdSync = -1
	s.cmdAlias, s.cmdArgs = "", ""

	s.message = ""
}

func (s *settingsWindow) show() {
	s.load()
	s.win.Show()
	s.win.Focus()
}

// settingsNavItems 是左栏的设置项，顺序和右栏的段落一一对应。
// 「自定义命令」要一整屏（左清单 + 右表单），所以它是单独一页；其余四项同在一页，
// 点哪一项就把右栏滚到哪一段——不切页，别的段还在原地，想接着往下看不用点回来。
var settingsNavItems = []string{"唤出", "界面外观", "文件对话框跟随", "排序", "索引", "自定义命令"}

const (
	secHotkey = iota
	secAppearance
	secFollow
	secRank
	secIndex
	secCommands
)

func (s *settingsWindow) view(c *ui.Context) {
	t := c.Theme()
	// 和面板同一套字体，切窗口时不换字形。
	usePanelFont(t)
	c.Root().Background(t.Background)

	// 测试里是直接 &settingsWindow{app: a} 构造的，未必走过 newSettingsWindow，
	// 这里补一次，保证左栏能选、选中项就是 navSel。
	if s.navList.Selected == nil {
		s.navList.Selected = &s.navSel
	}

	// 左栏换了项：这一帧把右栏滚过去，随即清掉标记，免得每帧都把人拽回某一段。
	if s.navSel != s.navPrev {
		s.navPrev = s.navSel
		s.navJump = s.navSel
	}

	ui.Column(c).Fill().Children(func() {
		// 顶栏
		ui.Row(c).Padding(20, 24, 14, 24).Gap(10).AlignItems(ui.Center).Children(func() {
			ui.Column(c).Grow(1).Gap(2).Children(func() {
				ui.Text(c, "Baozi 设置").FontSize(20).Bold()
				ui.Text(c, "改动保存后立即生效；配置文件在 "+s.app.cfgPath).FontSize(11).TextColor(t.TextMuted)
			})
		})
		ui.Divider(c)

		// 左右两栏：左边是设置项，右边是实际内容。
		ui.Row(c).Grow(1).Children(func() {
			s.navColumn(c, t)
			ui.Column(c).Width(1).FillHeight().Background(t.Border)
			ui.Column(c).Grow(1).FillHeight().Children(func() {
				if s.navSel == secCommands {
					s.commandsPage(c, t)
				} else {
					s.sectionsColumn(c, t)
				}
			})
		})

		ui.Divider(c)
		ui.Row(c).Padding(12, 24).Gap(12).AlignItems(ui.Center).Children(func() {
			ui.Text(c, s.message).FontSize(11).TextColor(t.TextMuted).Grow(1).SingleLine()
			if ui.Button(c, "重新载入").Clicked() {
				s.load()
			}
			if ui.PrimaryButton(c, "保存并应用").Clicked() {
				s.save()
			}
		})
	})
}

// navColumn 是左栏：一列设置项，选中的那一条由 List 画成和结果列表一样的选中块。
func (s *settingsWindow) navColumn(c *ui.Context, t *ui.Theme) {
	ui.Column(c).Width(212).FillHeight().Padding(14, 12).Gap(6).Children(func() {
		ui.Text(c, "设置项").FontSize(11).TextColor(t.TextMuted).Padding(8, 8, 2, 4)
		s.navList.Key = func(i int) any { return settingsNavItems[i] }
		s.navList.Label = func(i int) string { return settingsNavItems[i] }
		ui.List(c, &s.navList, len(settingsNavItems), func(i int) {
			ui.Text(c, settingsNavItems[i]).FontSize(12).Padding(8, 9).SingleLine()
		}).Grow(1)
	})
}

// sectionsColumn 是右栏正文：四个大项目一段接一段，段与段之间用分隔线断开来。
func (s *settingsWindow) sectionsColumn(c *ui.Context, t *ui.Theme) {
	ui.Scroll(c).Grow(1).Children(func() {
		ui.Column(c).Gap(0).Children(func() {
			s.section(c, secHotkey, func() { s.hotkeySection(c, t) })
			ui.Divider(c)
			s.section(c, secAppearance, func() { s.appearanceSection(c, t) })
			ui.Divider(c)
			s.section(c, secFollow, func() { s.followSection(c, t) })
			ui.Divider(c)
			s.section(c, secRank, func() { s.rankSection(c, t) })
			ui.Divider(c)
			s.section(c, secIndex, func() { s.indexSection(c, t) })
			ui.Divider(c)
		})
	})
}

// section 给一段设置套上留白；左栏刚点中这一段时，在这一帧把它滚进视野。
// ScrollIntoView 只在调用那一帧生效，否则用户自己滚走会被一直拽回来。
func (s *settingsWindow) section(c *ui.Context, idx int, build func()) {
	wrap := ui.Column(c).Padding(24, 24).Gap(0).Children(build)
	if s.navJump == idx {
		s.navJump = -1
		wrap.ScrollIntoView(ui.ScrollIntoViewOptions{Block: ui.ScrollStart, Animation: &ui.Smooth})
	}
}

func (s *settingsWindow) hotkeySection(c *ui.Context, t *ui.Theme) {
	ui.Column(c).Gap(10).Children(func() {
		sectionTitle(c, t, "唤出", "")
		ui.Checkbox(c, &s.doubleCtrl, "双击 Ctrl 唤出面板")
		ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
			ui.Text(c, "两次 Ctrl 的最大间隔（毫秒）").FontSize(12).TextColor(t.TextMuted)
			ui.NumberInput(c, &s.interval, 150, 900, 50).Width(120)
			ui.Text(c, fmt.Sprintf("%.0f ms", s.interval)).FontSize(12).TextColor(t.TextMuted)
		})
		ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
			ui.Text(c, "备用组合键").FontSize(12).TextColor(t.TextMuted)
			ui.TextInput(c, &s.combo).Width(240).Placeholder("Alt+Shift+Space，留空则不注册")
		})
		ui.Checkbox(c, &s.fullscreen, "全屏应用（游戏、全屏视频）前台时不响应")
		ui.Checkbox(c, &s.openAtLogin, "开机自动启动（登录后静默待在托盘，不弹面板）")
		ui.Checkbox(c, &s.hideOnStart, "启动时不弹面板")
	})
}

// followSection 是"文件对话框跟随"的开关：一个总开关 + 每一种目录来源各一个勾选。
// 关掉的来源连读都不读（跟随器每次轮询都会问一遍，改完保存立刻生效，不用重启）。
func (s *settingsWindow) followSection(c *ui.Context, t *ui.Theme) {
	ui.Column(c).Gap(10).Children(func() {
		sectionTitle(c, t, "文件对话框跟随", "")
		ui.Checkbox(c, &s.followOn, "启用跟随")

		ui.Row(c).Gap(18).AlignItems(ui.Center).Children(func() {
			ui.Text(c, "目录来源").FontSize(12).TextColor(t.TextMuted).Width(70)
			ui.Checkbox(c, &s.srcExplorer, "资源管理器")
			ui.Checkbox(c, &s.srcTotalCmd, "Total Commander")
			ui.Checkbox(c, &s.srcOpus, "Directory Opus")
		})
		ui.Row(c).Gap(18).AlignItems(ui.Center).Children(func() {
			ui.Text(c, "").Width(70)
			ui.Checkbox(c, &s.srcXyplorer, "XYplorer")
			ui.Checkbox(c, &s.srcXplorer2, "xplorer2")
			ui.Checkbox(c, &s.srcTessoa, "tessoa")
		})
	})
}

func (s *settingsWindow) rankSection(c *ui.Context, t *ui.Theme) {
	ui.Column(c).Gap(10).Children(func() {
		sectionTitle(c, t, "排序", "")
		sliderRow(c, t, "频次（用得多的靠前）", &s.wFreq)
		sliderRow(c, t, "类型（可执行文件、快捷方式靠前）", &s.wType)
		sliderRow(c, t, "准确性（名字匹配得越准越靠前）", &s.wAcc)
		ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
			ui.Text(c, "类型优先级").FontSize(12).TextColor(t.TextMuted).Width(150)
			ui.TextInput(c, &s.typeOrder).Grow(1).Placeholder(".exe, .lnk, .msc（逗号分隔，越靠前越优先）")
		})
		ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
			ui.Text(c, "最多显示结果数").FontSize(12).TextColor(t.TextMuted).Width(150)
			ui.NumberInput(c, &s.maxResults, 10, 500, 10).Width(120)
		})
		iconLabels := iconScaleLabels()
		iconSel := s.iconPreset
		ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
			ui.Text(c, "图标大小").FontSize(12).TextColor(t.TextMuted).Width(150)
			ui.Select(c, &iconSel, iconLabels).Width(130)
			if iconSel == iconCustomLabel {
				ui.NumberInput(c, &s.iconScale, config.MinIconScale, config.MaxIconScale, 0.05).Width(90)
				ui.Text(c, "倍").FontSize(12).TextColor(t.TextMuted)
			}
			ui.Text(c, s.iconPreview()).FontSize(11).TextColor(t.TextMuted)
		})
		// 台阶 + 自定义：选台阶直接落到那个倍数，选「自定义」才露出输入框。
		// 选择本身记在 s.iconPreset 里——否则手输一个不在台阶上的值之后，
		// 下拉会自己跳回最近的台阶，输入框跟着消失。
		if iconSel != s.iconPreset {
			s.iconPreset = iconSel
			if v, ok := iconPresetValue(iconSel); ok {
				s.iconScale = v
			}
		}
		ui.Checkbox(c, &s.thumbnails, "图片、视频、PDF 显示内容缩略图")
	})
}

// 面板尺寸没有设置项：直接拖面板的四条边就改，松手后落盘，下次启动照旧。

func (s *settingsWindow) indexSection(c *ui.Context, t *ui.Theme) {
	ui.Column(c).Gap(10).Children(func() {
		sectionTitle(c, t, "索引", "")
		ui.Row(c).Gap(12).Children(func() {
			labelledTextArea(c, t, "索引的磁盘/目录", &s.roots)
			labelledTextArea(c, t, "排除的目录名或路径片段", &s.excludes)
		})
		ui.Row(c).Gap(16).AlignItems(ui.Center).Children(func() {
			ui.Checkbox(c, &s.includeHidden, "包含隐藏文件")
		})
		ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
			ui.Text(c, "自动重建间隔（分钟，0 = 只在启动时建一次）").FontSize(12).TextColor(t.TextMuted)
			ui.NumberInput(c, &s.rescanMinutes, 0, 1440, 30).Width(120)
			if ui.Button(c, "立即重建索引").Clicked() {
				go s.app.rescan()
				if s.app.serviceAlive() {
					s.message = "已请求索引服务重建"
				} else {
					s.message = "已开始后台重建索引"
				}
			}
		})
		// 索引服务：直接读 NTFS 的 USN Journal，比目录扫描快一个量级，代价是要装一个服务。
		ui.Divider(c)
		ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
			state := "未安装（用目录扫描，普通权限即可）"
			if s.app.serviceAlive() {
				state = "运行中（USN 增量维护，主程序无需管理员权限）"
			} else if indexServiceInstalled() {
				state = "已安装但未运行"
			}
			ui.Text(c, "快速索引服务："+state).FontSize(12).TextColor(t.TextMuted)
		})
		ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
			if ui.Button(c, "启用快速索引（需管理员）").Clicked() {
				if err := s.app.runIndexService("--install"); err != nil {
					s.message = "启用失败：" + err.Error()
				} else {
					s.message = "请在 UAC 弹窗里确认，服务装好后索引会自动重建"
				}
			}
			if ui.Button(c, "停用并卸载").Clicked() {
				if err := s.app.runIndexService("--uninstall"); err != nil {
					s.message = "卸载失败：" + err.Error()
				} else {
					s.message = "请在 UAC 弹窗里确认；之后回到目录扫描"
				}
			}
		})
	})
}

// commandsPage 是「自定义命令」页：左列一排名单，右列编辑选中的那条。
// 左边只负责选，右边才改内容 —— 这样一屏里能同时看到"有哪些命令"和"这条长什么样"。
func (s *settingsWindow) commandsPage(c *ui.Context, t *ui.Theme) {
	s.syncCommandEditor()

	if dup := s.duplicateKeywords(); len(dup) > 0 {
		ui.Row(c).Padding(8, 18).Children(func() {
			ui.Text(c, "激活词重复："+strings.Join(dup, "、")+" —— 这几条会同时出现在结果里，容易点错。").
				FontSize(11).TextColor(t.Warning)
		})
	}

	ui.Row(c).Grow(1).Children(func() {
		// 左列：清单
		ui.Column(c).Width(250).FillHeight().Children(func() {
			ui.Row(c).Padding(16, 16, 6, 12).Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Text(c, "命令").FontSize(13).Bold()
				ui.Text(c, fmt.Sprintf("%d 条", len(s.cmds))).FontSize(11).TextColor(t.TextMuted)
			})
			ui.Column(c).Grow(1).Padding(6, 8).Children(func() {
				if len(s.cmds) == 0 {
					ui.Text(c, "还没有命令，点下面「新建」").FontSize(12).TextColor(t.TextMuted)
					return
				}
				lst := ui.List(c, &s.cmdList, len(s.cmds), func(i int) {
					ui.Column(c).Padding(6, 10).Gap(2).Children(func() {
						ui.Text(c, s.cmds[i].Name).FontSize(13).SingleLine()
						ui.Text(c, strings.Join(s.cmds[i].Keywords(), " · ")).FontSize(11).TextColor(t.TextMuted).SingleLine()
					})
				}).Grow(1)
				if lst.Changed() {
					// 换了选中的那条：下一个帧把右列的编辑框搬过去。
					s.cmdSync = -1
				}
			})
			ui.Row(c).Padding(8, 10).Gap(8).Children(func() {
				if ui.Button(c, "新建").Clicked() {
					s.newCommand()
				}
				if ui.Button(c, "删除").Clicked() {
					s.deleteCommand()
				}
			})
		})

		// 分隔
		ui.Column(c).Width(1).FillHeight().Background(t.Border)

		// 右列：表单
		ui.Column(c).Grow(1).Children(func() {
			if len(s.cmds) == 0 || s.cmdSel < 0 || s.cmdSel >= len(s.cmds) {
				ui.Column(c).Grow(1).Children(func() {
					ui.Text(c, "左边选一条命令，或者点「新建」加一条。").FontSize(13).TextColor(t.TextMuted)
				})
				return
			}
			cmd := &s.cmds[s.cmdSel]
			ui.Scroll(c).Grow(1).Children(func() {
				ui.Column(c).Padding(22, 18).Gap(14).Children(func() {
					formField(c, t, "激活词", "")
					ui.TextInput(c, &s.cmdAlias).Placeholder("关机, shut, shutdown")

					formField(c, t, "显示名", "")
					ui.TextInput(c, &cmd.Name).Placeholder("定时关机")

					formField(c, t, "程序", "")
					ui.TextInput(c, &cmd.Exec).Placeholder(`D:\1tools\scripts\shutdown-after.ps1`)

					formField(c, t, "参数", "")
					ui.TextArea(c, &s.cmdArgs).Height(110).FontSize(12)

					formField(c, t, "工作目录", "")
					ui.TextInput(c, &cmd.Dir).Placeholder(`D:\1tools  或  {dir}`)

					ui.Checkbox(c, &cmd.KeepOpen, "执行后不收起面板（开终端这类窗口时勾上）")

					// 实时预览这条到底会执行成什么（含脚本自动包的解释器），省得反复试。
					prog, pargs := launchSpec(cmd.Exec, splitLines(s.cmdArgs))
					preview := strings.TrimSpace(prog + " " + strings.Join(pargs, " "))
					ui.Text(c, "将执行："+preview).FontSize(11).TextColor(t.TextMuted)

					if strings.Contains(s.cmdArgs, "%") && !hasAnyNumberPlaceholder(s.cmdArgs) {
						ui.Text(c, "提示：参数里有 % 但没写 %1…%9 或 %*，那个 % 会被原样传给程序。").FontSize(11).TextColor(t.Warning)
					}
				})
			})
		})
	})
}

// duplicateKeywords 找出被两条以上命令共用的激活词（只在界面上提示，不拦保存）。
func (s *settingsWindow) duplicateKeywords() []string {
	seen := make(map[string]int, 16)
	var dup []string
	for _, cmd := range s.cmds {
		for _, k := range cmd.Keywords() {
			low := strings.ToLower(k)
			seen[low]++
			if seen[low] == 2 {
				dup = append(dup, k)
			}
		}
	}
	return dup
}

// formField 是右列每一栏的小标题 + 说明。
func formField(c *ui.Context, t *ui.Theme, label, desc string) {
	ui.Column(c).Gap(4).Children(func() {
		ui.Text(c, label).FontSize(12).Bold()
		if desc != "" {
			ui.Text(c, desc).FontSize(11).TextColor(t.TextMuted)
		}
	})
}

func hasAnyNumberPlaceholder(s string) bool {
	for i := byte('1'); i <= '9'; i++ {
		if strings.Contains(s, "%"+string(i)) {
			return true
		}
	}
	return false
}

// syncCommandEditor 把选中那条的"文本型"字段搬进编辑框。
// 只在这一条换了的时候搬，否则每帧都会把用户正在敲的字覆盖掉。
func (s *settingsWindow) syncCommandEditor() {
	if len(s.cmds) == 0 {
		s.cmdSel = -1
		s.cmdSync = -1
		s.cmdAlias, s.cmdArgs = "", ""
		return
	}
	if s.cmdSel < 0 || s.cmdSel >= len(s.cmds) {
		s.cmdSel = 0
	}
	if s.cmdSync == s.cmdSel {
		return
	}
	c := s.cmds[s.cmdSel]
	s.cmdAlias = strings.Join(c.Keywords(), ", ")
	s.cmdArgs = strings.Join(c.Args, "\n")
	s.cmdSync = s.cmdSel
}

// commitCommandEditor 把编辑框里的文本写回选中的那条（保存、新建、删除、切条目前都先调）。
func (s *settingsWindow) commitCommandEditor() {
	if len(s.cmds) == 0 || s.cmdSel < 0 || s.cmdSel >= len(s.cmds) {
		return
	}
	cmd := &s.cmds[s.cmdSel]
	cmd.Args = splitLines(s.cmdArgs)
	kws := splitKeywords(s.cmdAlias)
	if len(kws) == 0 {
		cmd.Keyword, cmd.Aliases = "", nil
		return
	}
	cmd.Keyword, cmd.Aliases = kws[0], kws[1:]
}

func (s *settingsWindow) newCommand() {
	s.commitCommandEditor()
	s.cmds = append(s.cmds, config.Command{Name: "新命令"})
	s.cmdSel = len(s.cmds) - 1
	s.cmdSync = -1
}

func (s *settingsWindow) deleteCommand() {
	if len(s.cmds) == 0 || s.cmdSel < 0 {
		return
	}
	s.cmds = append(s.cmds[:s.cmdSel], s.cmds[s.cmdSel+1:]...)
	if len(s.cmds) == 0 {
		s.cmdSel = -1
	} else if s.cmdSel >= len(s.cmds) {
		s.cmdSel = len(s.cmds) - 1
	}
	s.cmdSync = -1
}

// splitKeywords 拆"激活词"那一栏：逗号、竖线、顿号、空格都算分隔。
func splitKeywords(text string) []string {
	return strings.FieldsFunc(text, func(r rune) bool {
		return r == ',' || r == '，' || r == '|' || r == '、' || unicode.IsSpace(r)
	})
}

func sectionTitle(c *ui.Context, t *ui.Theme, title, desc string) {
	ui.Column(c).Gap(2).Children(func() {
		ui.Text(c, title).FontSize(14).Bold()
		// desc 为空就整行不画：界面上只留标题，说明文字一律不摆出来。
		if desc != "" {
			ui.Text(c, desc).FontSize(11).TextColor(t.TextMuted)
		}
	})
}

// iconCustomLabel 是下拉里"自定义"那一项：选它才露出倍数输入框。
const iconCustomLabel = "自定义"

// iconScaleSteps 是图标大小的几个台阶。100% 就是系统缩放下的默认大小，
// 所以这几个数不是"绝对尺寸"，而是"在默认基础上再放大缩小多少"。
var iconScaleSteps = []struct {
	Label string
	Value float64
}{
	{"75%", 0.75},
	{"100%", 1},
	{"125%", 1.25},
	{"150%", 1.5},
	{"175%", 1.75},
	{"200%", 2},
}

func iconScaleLabels() []string {
	out := make([]string, 0, len(iconScaleSteps)+1)
	for _, s := range iconScaleSteps {
		out = append(out, s.Label)
	}
	return append(out, iconCustomLabel)
}

// iconPresetLabel 报告当前倍数落在哪个台阶上；不在任何一个台阶上就是"自定义"。
func iconPresetLabel(k float64) string {
	for _, s := range iconScaleSteps {
		if math.Abs(s.Value-k) < 0.005 {
			return s.Label
		}
	}
	return iconCustomLabel
}

// iconPresetValue 把台阶标签换回倍数；"自定义"没有对应值。
func iconPresetValue(label string) (float64, bool) {
	for _, s := range iconScaleSteps {
		if s.Label == label {
			return s.Value, true
		}
	}
	return 0, false
}

// iconScaleOf 是"配置里到底该按几倍画"：新字段优先；老配置里存的是像素，
// 按当时的默认大小折成倍数，免得升级后图标突然变了个样。
func iconScaleOf(cfg *config.Config, sysScale float32) float64 {
	if k := cfg.Search.IconScale; k > 0 {
		return config.ClampIconScale(k)
	}
	if n := cfg.Search.IconSize; n > 0 {
		base := float64(config.BaseIconSize)
		if sysScale > 0 {
			base *= float64(sysScale)
		}
		return config.ClampIconScale(float64(n) / base)
	}
	return 1
}

// iconPreview 在"图标大小"旁边说清现在到底会画多大：只给实际像素。
// 不再带箭头、也不再报系统缩放比例 —— 那是环境参数，用户改不了它，
// 摆在设置页里只会让人以为它是个可调的东西。
func (s *settingsWindow) iconPreview() string {
	scale := s.app.scale
	if scale <= 0 {
		scale = 1
	}
	base := float64(config.BaseIconSize) * float64(scale)
	return fmt.Sprintf("%.1f 像素", base*config.ClampIconScale(s.iconScale))
}

func sliderRow(c *ui.Context, t *ui.Theme, label string, value *float64) {
	ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
		ui.Text(c, label).FontSize(12).TextColor(t.TextMuted).Width(230)
		ui.Slider(c, value, 0, 1).Grow(1)
		ui.Text(c, fmt.Sprintf("%.2f", *value)).FontSize(12).Width(48)
	})
}

// appearanceSection 是面板外观：圆角、玻璃不透明度、投影，各自"开关 + 程度"。
//
// 只影响主面板，不影响设置窗和文件互传窗：那两个是 mygo 的系统玻璃窗口，
// 形状由系统管；面板是整窗自绘的（见 glass.go / backing.go），因此这三项都能自己调。
func (s *settingsWindow) appearanceSection(c *ui.Context, t *ui.Theme) {
	ui.Column(c).Gap(14).Children(func() {
		sectionTitle(c, t, "界面外观", "")
		// 三项横向并排：它们互不相干、各自只有一句标签，竖着排会白占两行。
		ui.Row(c).Gap(24).AlignItems(ui.Center).Children(func() {
			toggleRow(c, t, &s.radiusOn, "圆角")
			toggleRow(c, t, &s.opacityOn, "透明度")
			toggleRow(c, t, &s.shadowOn, "投影")
		})
	})
}

// toggleRow 是"一个勾选"的一行。外观三项不再让用户填数、也不再挂说明：
// 面板上真能看出来的只有"有/没有"，一个 px 或一个百分比对眼睛没有意义。
// 程度值仍在配置里（沿用上次保存的），只是不在界面上摆出来。
func toggleRow(c *ui.Context, t *ui.Theme, on *bool, label string) {
	ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
		ui.Checkbox(c, on, label)
	})
}

// toggleSliderRow 是"一个勾选 + 一条滑杆 + 一个数"的一行：勾选管要不要，
// 滑杆管程度。滑杆和数值在第二行，第一行留给说明文字，读起来不挤。
//
// show 把滑杆里的值换成给用户看的文字（像素、百分比……）—— 配置里存的是 DIP，
// 屏幕上看到的是像素，两者差一个 DPI 缩放，不换算用户根本对不上。
func toggleSliderRow(c *ui.Context, t *ui.Theme, on *bool, val *float64,
	label string, lo, hi float64, hint string, show func(float64) string) {
	if show == nil {
		show = func(v float64) string { return fmt.Sprintf("%.0f", v) }
	}
	ui.Column(c).Gap(6).Children(func() {
		ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
			ui.Checkbox(c, on, label)
			ui.Text(c, hint).FontSize(11).TextColor(t.TextMuted)
		})
		ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
			ui.Slider(c, val, lo, hi).Grow(1)
			ui.Text(c, show(*val)).FontSize(12).Width(84)
		})
	})
}

func labelledTextArea(c *ui.Context, t *ui.Theme, label string, value *string) {
	ui.Column(c).Grow(1).Gap(6).Children(func() {
		ui.Text(c, label).FontSize(12).TextColor(t.TextMuted)
		ui.TextArea(c, value).Height(110).FontSize(12)
	})
}

// save 解析编辑区、写盘、热更新配置并重启钩子。
func (s *settingsWindow) save() {
	cfg := *s.app.cfg // 浅拷贝：只改本窗口负责的字段，别的字段原样保留
	cfg.Hotkey.DoubleCtrl = s.doubleCtrl
	cfg.Hotkey.DoubleCtrlIntervalMs = int(s.interval)
	cfg.Hotkey.Combo = strings.TrimSpace(s.combo)
	cfg.Hotkey.DisableInFullscreen = s.fullscreen

	cfg.Search.Weights = config.Weights{Frequency: s.wFreq, Type: s.wType, Accuracy: s.wAcc}
	cfg.Search.TypeOrder = splitList(s.typeOrder)
	cfg.Search.MaxResults = int(s.maxResults)
	cfg.Search.Thumbnails = boolPtr(s.thumbnails)
	cfg.Search.IconScale = config.ClampIconScale(s.iconScale)
	// 旧字段清零：两套值同时存在时没人说得清该信哪个。
	cfg.Search.IconSize = 0

	oldRoots := s.app.cfg.Index.Roots
	cfg.Index.Roots = splitLines(s.roots)
	cfg.Index.Excludes = splitLines(s.excludes)
	cfg.Index.IncludeHidden = s.includeHidden
	cfg.Index.RescanMinutes = int(s.rescanMinutes)

	// 自定义命令：先把编辑器里的文本写回，再归一化 —— 激活词去重、
	// 空壳条目（新建后没填程序的）不落盘，免得配置里留一堆跑不起来的命令。
	s.commitCommandEditor()
	cmds := make([]config.Command, 0, len(s.cmds))
	for _, c := range s.cmds {
		kws := config.Command{Keyword: c.Keyword, Aliases: c.Aliases}.Keywords()
		c.Keyword, c.Aliases = "", nil
		if len(kws) > 0 {
			c.Keyword, c.Aliases = kws[0], kws[1:]
		}
		c.Name = strings.TrimSpace(c.Name)
		c.Exec = strings.TrimSpace(c.Exec)
		c.Dir = strings.TrimSpace(c.Dir)
		if c.Exec == "" || len(kws) == 0 {
			continue
		}
		if c.Name == "" {
			c.Name = c.Exec
		}
		cmds = append(cmds, c)
	}
	cfg.Commands = cmds
	s.cmds = append([]config.Command(nil), cmds...)
	if s.cmdSel >= len(s.cmds) {
		s.cmdSel = len(s.cmds) - 1
	}
	s.cmdSync = -1

	// 外观：三项"开关 + 程度"都写具体值（指针全填上）。不写的话读回来会落到
	// "缺省即开 / 用默认值"，用户刚调的数就没了。
	rOn, oOn, shOn := s.radiusOn, s.opacityOn, s.shadowOn
	rVal := int(math.Round(s.radiusVal))
	oVal := int(math.Round(s.opacityVal))
	shVal := int(math.Round(s.shadowVal))
	cfg.Appearance = config.Appearance{
		Radius:  config.Toggle{Enabled: &rOn, Value: &rVal},
		Opacity: config.Toggle{Enabled: &oOn, Value: &oVal},
		Shadow:  config.Toggle{Enabled: &shOn, Value: &shVal},
	}
	hide := s.hideOnStart
	cfg.Startup = config.Startup{HideOnStart: &hide}

	// 文件对话框跟随：总开关 + 各来源。写具体布尔值（不是省略），
	// 这样用户一旦手动关掉某个来源，下次读配置不会被"缺省即开"又打开。
	followOn := s.followOn
	cfg.DialogFollow = &followOn
	cfg.PathSources = &config.PathSources{
		Explorer: boolPtr(s.srcExplorer),
		TotalCmd: boolPtr(s.srcTotalCmd),
		Opus:     boolPtr(s.srcOpus),
		Xyplorer: boolPtr(s.srcXyplorer),
		Xplorer2: boolPtr(s.srcXplorer2),
		Tessoa:   boolPtr(s.srcTessoa),
	}

	if err := config.Save(&cfg, s.app.cfgPath); err != nil {
		s.message = "保存失败：" + err.Error()
		log.Printf("保存配置: %v", err)
		return
	}

	oldCombo := s.app.cfg.Hotkey.Combo
	s.app.cfg = &cfg

	// 外观：先让包级变量跟着配置走（圆角 / 不透明度 / 阴影强度），再把屏幕上
	// 已经画着的东西重建一遍 —— 底图的弧、内容层那条弧、投影层。
	applyAppearance(s.app.cfg)
	s.app.applyAppearanceNow()

	// 自启开关落到系统里（不写 config.json）。失败要把原因说出来，不能假装成功。
	if s.openAtLogin != s.app.openAtLogin.Load() {
		if err := mygo.App.SetOpenAtLogin(s.openAtLogin); err != nil {
			log.Printf("设置开机自启: %v", err)
			s.message = "开机自启设置失败：" + err.Error()
			s.win.Update(func() {})
			return
		}
		s.app.openAtLogin.Store(s.openAtLogin)
		log.Printf("开机自启: %v", s.openAtLogin)
		// 托盘里那个「开机启动」勾选要同步，否则两个入口显示相反的状态。
		if s.app.menu != nil {
			if it := s.app.menu.ItemByID("login"); it != nil {
				it.SetChecked(s.openAtLogin)
			}
		}
	}

	// 组合键要重新注册；钩子进程读的是 config.json，必须重启才能拿到新间隔。
	s.app.restartHotkeys(oldCombo)
	s.app.restartHook()
	// 跟随器的总开关 / 来源勾选：不重启进程，改完立刻按新配置走。
	s.app.syncFollow()

	rootsDirty := rootsChanged(oldRoots, cfg.Index.Roots)
	if rootsDirty {
		s.message = "已保存，正在按新的索引范围重建"
		go s.app.rescan()
	} else {
		s.message = "已保存并生效"
	}
	s.app.refilter()
	s.win.Update(func() {})
}

// restartHotkeys 重新注册备用组合键。
func (a *application) restartHotkeys(oldCombo string) {
	if oldCombo != "" && oldCombo != a.cfg.Hotkey.Combo {
		mygo.GlobalShortcut.Unregister(oldCombo)
	}
	if a.cfg.Hotkey.Combo == "" {
		return
	}
	if err := mygo.GlobalShortcut.Register(a.cfg.Hotkey.Combo, a.toggle); err != nil {
		log.Printf("注册组合键 %s 失败: %v", a.cfg.Hotkey.Combo, err)
		a.setStatus("组合键被占用：" + a.cfg.Hotkey.Combo)
	}
}

// restartHook 杀掉旧钩子再起一个新的（新进程会重新读配置）。
func (a *application) restartHook() {
	if a.hookIn != nil {
		_ = a.hookIn.Close()
		a.hookIn = nil
	}
	if a.hook != nil && a.hook.Process != nil {
		done := make(chan struct{})
		go func() { _ = a.hook.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			_ = a.hook.Process.Kill()
		}
		a.hook = nil
	}
	a.startHook()
}

// boolPtr 给"写了具体值"的配置项用：设置页保存时，每个来源都写明确的 true/false，
// 这样用户关掉某个来源之后，不会因为"缺省即开"又给它打开。
func boolPtr(v bool) *bool { return &v }

func splitList(s string) []string {
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' })
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func splitLines(s string) []string {
	out := make([]string, 0, 16)
	for _, line := range strings.Split(s, "\n") {
		if v := strings.TrimSpace(strings.TrimSuffix(line, "\r")); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// parseCommands 解析 "关键字 | 名称 | 程序 | 参数"。
// 界面已经换成结构化编辑，这个解析只留给旧文本格式的兼容/排查用。
func parseCommands(text string) []config.Command {
	out := make([]config.Command, 0, 16)
	for _, line := range splitLines(text) {
		fields := strings.Split(line, "|")
		for i := range fields {
			fields[i] = strings.TrimSpace(fields[i])
		}
		if len(fields) == 0 || fields[0] == "" {
			continue
		}
		cmd := config.Command{Keyword: fields[0]}
		switch len(fields) {
		case 1:
			cmd.Exec = fields[0]
			cmd.Name = fields[0]
		case 2:
			cmd.Exec = fields[1]
			cmd.Name = fields[1]
		default:
			cmd.Name = fields[1]
			cmd.Exec = fields[2]
			if len(fields) > 3 {
				cmd.Args = splitList(fields[3])
			}
		}
		if cmd.Name == "" {
			cmd.Name = cmd.Exec
		}
		out = append(out, cmd)
	}
	return out
}
