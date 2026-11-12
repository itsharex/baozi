package main

import (
	"fmt"
	"log"
	"time"

	"baozi/internal/config"
	"baozi/internal/share"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
	"github.com/skip2/go-qrcode"
)

// shareWindow 是"文件互传"的入口：点「开始共享」后开一个局域网 HTTP 服务，
// 生成一条带有效期的临时链接（默认 1 小时，到点自动失效）。
// 只发不收：对方用浏览器打开就能下载，没有任何上传通道。
type shareWindow struct {
	app     *application
	win     *mygo.Window
	session *share.Session

	qrURL string
	qr    *ui.Bitmap

	// 网卡下拉：labels/values 一一对应，values 是要绑的 IPv4（"" = 全部网卡）。
	labels  []string
	values  []string
	bindSel string

	// pending 是"还没点开始共享"时先攒着的条目：先挑文件，再生成链接。
	pending []share.Item

	status string
}

// ttlChoices 是链接有效期可选项，默认 1 小时。
var ttlChoices = []struct {
	Label   string
	Minutes int
}{
	{"10 分钟", 10},
	{"30 分钟", 30},
	{"1 小时", 60},
	{"2 小时", 120},
	{"8 小时", 480},
	{"24 小时", 1440},
}

// ttlMinutes 当前选中的有效期（分钟）。配置里没值时按 60 走。
func (s *shareWindow) ttlMinutes() int {
	if m := s.app.cfg.Share.TTLMinutes; m > 0 {
		return m
	}
	return 60
}

func ttlLabel(minutes int) string {
	for _, c := range ttlChoices {
		if c.Minutes == minutes {
			return c.Label
		}
	}
	return fmt.Sprintf("%d 分钟", minutes)
}

func newShareWindow(a *application) *shareWindow {
	s := &shareWindow{app: a}
	s.win = mygo.NewWindow(mygo.WindowOptions{
		Title:     "Baozi 文件互传",
		Width:     760,
		Height:    680,
		Hidden:    true,
		Vibrancy:  mygo.VibrancyGlass,
		MinWidth:  640,
		MinHeight: 520,
		Content:   ui.View(s.view),
	})
	s.win.OnClose(func(e *mygo.CloseEvent) {
		// 退出流程里必须放行，否则 mygo 的 Quit 会因为"窗口不肯关"被整体取消。
		if s.app.quitting.Load() {
			return
		}
		e.PreventDefault()
		s.win.Hide()
	})
	return s
}

func (a *application) openSettings() {
	if a.settings != nil {
		a.settings.show()
	}
}

func (a *application) openShare() {
	if a.share != nil {
		a.share.show()
	}
}

func (s *shareWindow) show() {
	// 每次打开都重新枚举网卡（用户可能刚插网线/连了 WiFi）。
	s.labels, s.values = bindOptions()
	// 不再一打开就开服务：分享是一次性的，要用户自己点「生成链接」。
	s.win.Show()
	s.win.Focus()
}

// list 是当前要分享的条目：已经开始共享就以会话里的为准，否则是攒着的候选。
func (s *shareWindow) list() []share.Item {
	if s.session != nil {
		return s.session.Items()
	}
	return s.pending
}

// bindOptions 生成"绑定网卡"的下拉项：第一项永远是全部网卡。
func bindOptions() (labels, values []string) {
	labels = []string{"全部网卡（默认）"}
	values = []string{""}
	for _, b := range share.Bindings() {
		name := b.Name
		if b.Virtual {
			name += "·虚拟"
		}
		labels = append(labels, fmt.Sprintf("%s · %s", name, b.IP))
		values = append(values, b.IP)
	}
	return labels, values
}

// applyBind 记住新网卡并重启会话（换端口、换访问码，旧链接立即失效）。
func (s *shareWindow) applyBind(bind string) {
	s.app.cfg.Share.Bind = bind
	if s.app.cfgPath != "" {
		if err := config.Save(s.app.cfg, s.app.cfgPath); err != nil {
			log.Printf("保存网卡设置: %v", err)
		}
	}
	restart := s.session != nil
	go s.win.Update(func() {
		if restart {
			s.stop()
			s.start()
			s.status = "已切换网卡，链接和访问码都换新了"
			return
		}
		s.status = "已切换网卡，下次开始共享生效"
	})
}

// start 生成一条新的临时链接（只发不收，有有效期，到点自动失效）。
func (s *shareWindow) start() {
	ttl := time.Duration(s.ttlMinutes()) * time.Minute
	sess, err := share.Start(s.app.cfg.Share.Bind, ttl)
	if err != nil {
		s.status = "启动共享失败：" + err.Error()
		log.Printf("启动文件互传: %v", err)
		return
	}
	s.session = sess
	for _, it := range s.pending {
		sess.Add(it.Path)
	}
	s.pending = nil
	s.qrURL = ""
	s.qr = nil
	s.status = fmt.Sprintf("链接已生成，%s 后自动失效", ttlLabel(s.ttlMinutes()))
	log.Printf("文件互传已开启: %s（有效期 %s）", sess.URL(), ttlLabel(s.ttlMinutes()))
}

func (s *shareWindow) stop() {
	if s.session == nil {
		return
	}
	if err := s.session.Stop(); err != nil {
		log.Printf("停止共享: %v", err)
	}
	s.session = nil
	s.qrURL = ""
	s.qr = nil
	s.status = "已停止共享"
}

func (s *shareWindow) view(c *ui.Context) {
	t := c.Theme()
	// 和面板同一套字体，切窗口时不换字形。
	usePanelFont(t)
	c.Root().Background(t.Background)

	ui.Column(c).Fill().Children(func() {
		ui.Row(c).Padding(20, 24, 12, 24).Gap(12).AlignItems(ui.Center).Children(func() {
			ui.Column(c).Grow(1).Gap(2).Children(func() {
				ui.Text(c, "文件互传").FontSize(20).Bold()
				ui.Text(c, "局域网直连，不经过云端；生成一条带有效期的临时链接，到期自动失效").FontSize(11).TextColor(t.TextMuted)
			})
		})
		ui.Divider(c)

		ui.Scroll(c).Grow(1).Children(func() {
			ui.Column(c).Padding(24, 24).Gap(18).Children(func() {
				s.bindRow(c, t)
				s.ttlRow(c, t)
				ui.Divider(c)
				// 链接到点就自动停：这里先把会话收掉，界面立刻回到"未共享"。
				if s.session != nil && s.session.Expired() {
					s.stop()
					s.status = "链接已到期，已自动停止共享"
				}
				if s.session == nil {
					s.stoppedView(c, t)
					return
				}
				s.activeView(c, t)
			})
		})

		// 底部一条就够：左边放"加东西"的入口，中间状态，右边主操作。
		// 和主面板的底栏同构（Pane 也是底部一条），不再上下叠两层按钮。
		ui.Divider(c)
		ui.Row(c).Padding(10, 20).Gap(10).AlignItems(ui.Center).Children(func() {
			// 加东西的入口一直在：先挑文件、再生成链接，是这个窗口的正常顺序。
			if ui.Button(c, "添加文件").Clicked() {
				go s.pickFiles()
			}
			if ui.Button(c, "添加文件夹").Clicked() {
				go s.pickFolder()
			}
			ui.Text(c, s.status).FontSize(11).TextColor(t.TextMuted).Grow(1).SingleLine()
			switch {
			case s.session == nil:
				if ui.PrimaryButton(c, "生成链接").Clicked() {
					s.start()
					s.win.Update(func() {})
				}
			case s.session.Breached():
				// 输错太多次被自动停掉：换一个访问码重开。
				if ui.PrimaryButton(c, "重新生成链接").Clicked() {
					s.stop()
					s.start()
					s.win.Update(func() {})
				}
			default:
				if ui.Button(c, "停止共享").Clicked() {
					s.stop()
					s.win.Update(func() {})
				}
			}
		})
	})
}

// ttlRow 是"链接有效期"的下拉：到期自动失效，不要常驻分享。
func (s *shareWindow) ttlRow(c *ui.Context, t *ui.Theme) {
	labels := make([]string, 0, len(ttlChoices))
	for _, ch := range ttlChoices {
		labels = append(labels, ch.Label)
	}
	cur := ttlLabel(s.ttlMinutes())
	sel := cur
	ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
		ui.Text(c, "链接有效期").FontSize(12).TextColor(t.TextMuted).Width(80)
		ui.Select(c, &sel, labels).Width(360)
		ui.Text(c, "到期自动失效；默认 1 小时").FontSize(11).TextColor(t.TextMuted)
	})
	if sel != cur {
		for _, ch := range ttlChoices {
			if ch.Label == sel {
				s.app.cfg.Share.TTLMinutes = ch.Minutes
				if s.app.cfgPath != "" {
					if err := config.Save(s.app.cfg, s.app.cfgPath); err != nil {
						log.Printf("保存有效期设置: %v", err)
					}
				}
				if s.session != nil {
					s.status = "有效期已改，下次「生成链接」时生效"
				} else {
					s.status = "有效期已设为 " + ch.Label
				}
				s.win.Update(func() {})
				break
			}
		}
	}
}

// bindRow 是"绑哪块网卡"的下拉。默认全部网卡；机器上有 VMware / MuMu 这类
// 虚拟网卡时，自动挑 IP 可能挑错，这里手动固定成真正联网的那块。
func (s *shareWindow) bindRow(c *ui.Context, t *ui.Theme) {
	if len(s.labels) == 0 {
		s.labels, s.values = bindOptions()
	}
	cur := s.labels[0]
	for i, v := range s.values {
		if v == s.app.cfg.Share.Bind {
			cur = s.labels[i]
		}
	}
	sel := cur
	ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
		ui.Text(c, "绑定网卡").FontSize(12).TextColor(t.TextMuted).Width(80)
		ui.Select(c, &sel, s.labels).Width(360)
		if ui.Button(c, "刷新").Clicked() {
			s.labels, s.values = bindOptions()
			s.win.Update(func() {})
		}
		ui.Text(c, "默认全部网卡，换网卡会重新生成访问码").FontSize(11).TextColor(t.TextMuted)
	})
	if sel != cur {
		for i, l := range s.labels {
			if l == sel {
				s.applyBind(s.values[i])
				break
			}
		}
	}
}

func (s *shareWindow) stoppedView(c *ui.Context, t *ui.Theme) {
	ui.Text(c, "还没有生成分享链接。").FontSize(14)
	ui.Text(c, "先把要分享的文件或文件夹加进来，再点下面的「生成链接」。"+
		"链接有效期按上面选的（默认 1 小时），到点自动失效；对方打开就直接下载，文件夹会打包成 zip。").FontSize(12).TextColor(t.TextMuted)
	s.itemsView(c, t, "待分享")
}

// remainText 把剩余时间写成 12:34 / 45 秒 这种样子。
func remainText(d time.Duration) string {
	if d <= 0 {
		return "0 秒"
	}
	sec := int(d.Seconds())
	if sec < 60 {
		return fmt.Sprintf("%d 秒", sec)
	}
	return fmt.Sprintf("%d:%02d", sec/60, sec%60)
}

func (s *shareWindow) activeView(c *ui.Context, t *ui.Theme) {
	url := s.session.URL()

	// 倒计时需要每秒重绘一次（c.After 只请求下一帧）。
	c.After(time.Second)

	if s.session.Breached() {
		ui.Text(c, "访问码连续输错太多次，共享已自动停止（防止被在线穷举）。"+
			"点下面的「重新开始共享」会换一个新的访问码。").FontSize(12).TextColor(t.Danger)
	}

	// 二维码（只在地址变化时重新生成）
	if s.qrURL != url {
		png, err := qrcode.Encode(url, qrcode.Medium, 256)
		if err == nil {
			if bm, err := ui.DecodeBitmap(png); err == nil {
				s.qr, s.qrURL = bm, url
			}
		}
	}

	// 访问码 / 链接在左，二维码在右。上下排的话光二维码就占 180px，
	// 窗口一缩到最小高度它先被裁掉一半，看着像界面坏了。
	ui.Row(c).Gap(18).AlignItems(ui.Start).Children(func() {
		ui.Column(c).Grow(1).Gap(10).Children(func() {
			// 4 位访问码单独大字显示：手敲进手机比抄 URL 快。
			ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
				ui.Text(c, "访问码").FontSize(12).TextColor(t.TextMuted)
				ui.Text(c, s.session.Token()).FontSize(28).Bold()
				ui.Text(c, "4 位，区分大小写").FontSize(11).TextColor(t.TextMuted)
				if ui.Button(c, "复制访问码").Clicked() {
					mygo.Clipboard.WriteText(s.session.Token())
					s.status = "访问码已复制"
				}
			})

			// 地址 + 复制
			ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
				ui.Text(c, url).FontSize(15).SingleLine().Grow(1)
				if ui.Button(c, "复制链接").Clicked() {
					mygo.Clipboard.WriteText(url)
					s.status = "链接已复制"
				}
			})

			ui.Text(c, "手机扫码，或把链接发给对方").FontSize(11).TextColor(t.TextMuted)
			ui.Text(c, "同一局域网内才打得开；对方只能下载，不能上传").FontSize(11).TextColor(t.TextMuted)
			// 有效期：默认 1 小时，到点这条链接就彻底打不开了。
			ui.Text(c, fmt.Sprintf("到期：%s（还剩 %s）",
				s.session.ExpiresAt().Format("15:04"), remainText(s.session.Remaining()))).
				FontSize(11).TextColor(t.TextMuted)
		})
		if s.qr != nil {
			ui.Image(c, s.qr).Size(150, 150)
		}
	})

	s.itemsView(c, t, "正在分享")
}

// itemsView 是"要分享的东西"这一块：没开始共享时显示候选，开始后显示正在分享的。
func (s *shareWindow) itemsView(c *ui.Context, t *ui.Theme, title string) {
	items := s.list()
	ui.Column(c).Gap(8).Children(func() {
		ui.Text(c, title).FontSize(14).Bold()
		if len(items) == 0 {
			ui.Text(c, "还没有文件。点下面的「添加文件 / 添加文件夹」，或直接把路径粘进搜索框后选中分享。").FontSize(12).TextColor(t.TextMuted)
		}
		for _, it := range items {
			itemPath, itemName, itemSize, itemDir := it.Path, it.Name, it.Size, it.Dir
			ui.Row(c).Padding(8, 12).Gap(12).AlignItems(ui.Center).Radius(10).Background(t.Surface).Children(func() {
				label := "文件"
				if itemDir {
					label = "文件夹"
				}
				ui.Column(c).Grow(1).Gap(2).Children(func() {
					ui.Text(c, itemName).FontSize(13).SingleLine()
					ui.Text(c, fmt.Sprintf("%s · %s", label, humanSize(itemSize))).FontSize(11).TextColor(t.TextMuted)
				})
				if ui.Button(c, "移除").Clicked() {
					s.remove(itemPath)
					s.win.Update(func() {})
				}
			})
		}
	})
}

func (s *shareWindow) pickFiles() {
	paths, err := mygo.Dialog.Open(mygo.OpenDialogOptions{
		Parent:   s.win,
		Title:    "选择要共享的文件",
		Multiple: true,
	})
	if err != nil {
		log.Printf("选择文件: %v", err)
		return
	}
	s.add(paths)
}

func (s *shareWindow) pickFolder() {
	paths, err := mygo.Dialog.Open(mygo.OpenDialogOptions{
		Parent:    s.win,
		Title:     "选择要共享的文件夹",
		Directory: true,
	})
	if err != nil {
		log.Printf("选择文件夹: %v", err)
		return
	}
	s.add(paths)
}

func (s *shareWindow) add(paths []string) {
	if len(paths) == 0 {
		return
	}
	items := share.ItemOf(paths...)
	if len(items) == 0 {
		s.status = "没有可分享的文件（路径不存在？）"
		s.win.Update(func() {})
		return
	}
	if s.session != nil {
		added := s.session.Add(paths...)
		if len(added) == 0 {
			s.status = "没有新增（可能已在列表里）"
		} else {
			s.status = fmt.Sprintf("已添加 %d 项", len(added))
		}
		s.win.Update(func() {})
		return
	}
	added := 0
	for _, it := range items {
		dup := false
		for _, p := range s.pending {
			if p.Path == it.Path {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		s.pending = append(s.pending, it)
		added++
	}
	if added == 0 {
		s.status = "没有新增（可能已在列表里）"
	} else {
		s.status = fmt.Sprintf("已添加 %d 项，点「生成链接」开始分享", added)
	}
	s.win.Update(func() {})
}

// remove 从列表里去掉一项（还没开始共享时就是从候选里挑掉）。
func (s *shareWindow) remove(path string) {
	if s.session != nil {
		s.session.Remove(path)
		return
	}
	kept := s.pending[:0]
	for _, it := range s.pending {
		if it.Path != path {
			kept = append(kept, it)
		}
	}
	s.pending = kept
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
