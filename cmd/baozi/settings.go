package main

import (
	"fmt"
	"log"
	"strings"
	"time"

	"baozi/internal/config"

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

	wFreq float64
	wType float64
	wAcc  float64

	typeOrder  string
	maxResults float64

	roots         string
	excludes      string
	includeHidden bool
	rescanMinutes float64

	commands string

	message string
}

func newSettingsWindow(a *application) *settingsWindow {
	s := &settingsWindow{app: a}
	s.win = mygo.NewWindow(mygo.WindowOptions{
		Title: "Baozi 设置",
		// 必须 Hidden：窗口创建即显示，不带这一项的话 Baozi 一启动就弹一个设置窗口，
		// 它会占着任务栏、抢前台，还会让"点任务栏再双击 Ctrl"唤出的面板拿不到焦点。
		Hidden:       true,
		Width:        880,
		Height:       760,
		Vibrancy:     mygo.VibrancyGlass,
		MinWidth:     760,
		MinHeight:    520,
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

	s.wFreq = cfg.Search.Weights.Frequency
	s.wType = cfg.Search.Weights.Type
	s.wAcc = cfg.Search.Weights.Accuracy
	s.typeOrder = strings.Join(cfg.Search.TypeOrder, ", ")
	s.maxResults = float64(cfg.Search.MaxResults)

	s.roots = strings.Join(cfg.Index.Roots, "\n")
	s.excludes = strings.Join(cfg.Index.Excludes, "\n")
	s.includeHidden = cfg.Index.IncludeHidden
	s.rescanMinutes = float64(cfg.Index.RescanMinutes)

	lines := make([]string, 0, len(cfg.Commands))
	for _, c := range cfg.Commands {
		lines = append(lines, strings.Join([]string{
			c.Keyword, c.Name, c.Exec, strings.Join(c.Args, ","),
		}, " | "))
	}
	s.commands = strings.Join(lines, "\n")
	s.message = ""
}

func (s *settingsWindow) show() {
	s.load()
	s.win.Show()
	s.win.Focus()
}

func (s *settingsWindow) view(c *ui.Context) {
	t := c.Theme()
	c.Root().Background(t.Background)

	ui.Column(c).Fill().Children(func() {
		// 顶栏
		ui.Row(c).Padding(20, 24, 12, 24).Gap(10).AlignItems(ui.Center).Children(func() {
			ui.Column(c).Grow(1).Gap(2).Children(func() {
				ui.Text(c, "Baozi 设置").FontSize(20).Bold()
				ui.Text(c, "改动保存后立即生效；配置文件在 "+s.app.cfgPath).FontSize(11).TextColor(t.TextMuted)
			})
		})
		ui.Divider(c)

		ui.Scroll(c).Grow(1).Children(func() {
			ui.Column(c).Padding(24, 24).Gap(22).Children(func() {
				s.hotkeySection(c, t)
				s.rankSection(c, t)
				s.indexSection(c, t)
				s.commandSection(c, t)
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

func (s *settingsWindow) hotkeySection(c *ui.Context, t *ui.Theme) {
	ui.Column(c).Gap(10).Children(func() {
		sectionTitle(c, t, "唤出", "快捷键唤不出多半是被别的程序抢了，双击 Ctrl 走的是低级键盘钩子，更可靠")
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
		ui.Text(c, "自启靠系统的 Run 键，任务管理器「启动」页里能单独禁用；禁用后这里的勾选会跟着变回未勾选。").FontSize(11).TextColor(t.TextMuted)
	})
}

func (s *settingsWindow) rankSection(c *ui.Context, t *ui.Theme) {
	ui.Column(c).Gap(10).Children(func() {
		sectionTitle(c, t, "排序", "三项权重越大越优先：频次 > 类型 > 准确性（默认 0.5 / 0.3 / 0.2）")
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
	})
}

func (s *settingsWindow) indexSection(c *ui.Context, t *ui.Theme) {
	ui.Column(c).Gap(10).Children(func() {
		sectionTitle(c, t, "索引", "每行一个。改动根目录后会自动重建索引（大磁盘需要几分钟）")
		ui.Row(c).Gap(12).Children(func() {
			labelledTextArea(c, t, "索引的磁盘/目录", &s.roots)
			labelledTextArea(c, t, "排除的目录名或路径片段", &s.excludes)
		})
		ui.Row(c).Gap(16).AlignItems(ui.Center).Children(func() {
			ui.Checkbox(c, &s.includeHidden, "包含隐藏文件")
			ui.Text(c, "（默认不索引隐藏文件，可减少噪声；隐藏目录总会进去，不然备份盘里搜不到）").FontSize(11).TextColor(t.TextMuted)
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
			ui.Text(c, "装一次即可，开机自启；不需要时点右边卸载，服务与数据都会清掉").FontSize(11).TextColor(t.TextMuted)
		})
	})
}

func (s *settingsWindow) commandSection(c *ui.Context, t *ui.Theme) {
	ui.Column(c).Gap(10).Children(func() {
		sectionTitle(c, t, "自定义命令", "每行一条：关键字 | 显示名 | 程序 | 参数（参数逗号分隔，可省略）")
		ui.TextArea(c, &s.commands).Height(120).FontSize(12)
		ui.Text(c, "例：term | Windows Terminal | wt.exe    ·    code | VS Code | code").FontSize(11).TextColor(t.TextMuted)
	})
}

func sectionTitle(c *ui.Context, t *ui.Theme, title, desc string) {
	ui.Column(c).Gap(2).Children(func() {
		ui.Text(c, title).FontSize(14).Bold()
		ui.Text(c, desc).FontSize(11).TextColor(t.TextMuted)
	})
}

func sliderRow(c *ui.Context, t *ui.Theme, label string, value *float64) {
	ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
		ui.Text(c, label).FontSize(12).TextColor(t.TextMuted).Width(230)
		ui.Slider(c, value, 0, 1).Grow(1)
		ui.Text(c, fmt.Sprintf("%.2f", *value)).FontSize(12).Width(48)
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

	oldRoots := s.app.cfg.Index.Roots
	cfg.Index.Roots = splitLines(s.roots)
	cfg.Index.Excludes = splitLines(s.excludes)
	cfg.Index.IncludeHidden = s.includeHidden
	cfg.Index.RescanMinutes = int(s.rescanMinutes)

	cfg.Commands = parseCommands(s.commands)

	if err := config.Save(&cfg, s.app.cfgPath); err != nil {
		s.message = "保存失败：" + err.Error()
		log.Printf("保存配置: %v", err)
		return
	}

	oldCombo := s.app.cfg.Hotkey.Combo
	s.app.cfg = &cfg

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
// 只写两段时视为 "关键字 | 程序"，名称取程序名。
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
