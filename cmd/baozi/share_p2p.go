package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"baozi/internal/config"
	"baozi/internal/p2p"
	"baozi/internal/share"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
	"github.com/skip2/go-qrcode"
)

// 跨网互传：和局域网那条路并列的第二套通道。
//
// 分工很明确——
//   - 局域网（share.go）：开个 HTTP 服务，对方浏览器打开就能下，简单、零配置；
//   - 跨网（这里）：WebRTC 打洞直连，两端各跑一个 Baozi，靠一个 6 位短码配对。
//
// 两条路的共同点：都只做"我发你收"，都不经手云盘；不同点是跨网那条
// 需要一个信令服务帮忙交换一次 SDP（Cloudflare Worker，免费额度足够）。

type p2pStage int

const (
	p2pIdle     p2pStage = iota // 没在跑
	p2pWaiting                  // 连上信令了，等对方进来
	p2pLinking                  // 两边都在了，交换 SDP 中
	p2pTransfer                 // 正在传
	p2pDone                     // 传完
	p2pFailed                   // 出错
)

// linkTimeout 是"等对方"/"交换描述"这两个阶段的死线。
// 打洞本身很快，卡住基本是网络不允许，等久了没意义。
const linkTimeout = 5 * time.Minute

// netShare 装跨网这一半的全部状态。
// 后台 goroutine 改它，视图读它，所以每处访问都得过锁。
type netShare struct {
	mu sync.Mutex

	stage p2pStage
	role  string // sender / receiver
	code  string // 本次的房间码
	dir   string // 接收方落盘的目录
	prog  p2p.Progress
	note  string // 出错原因，或者"传完了"这类结论
	via   string // direct / relay：数据有没有绕道中继

	// text 是这次顺带要发出去的一段文本；gotText 是收到的那段。
	// 文本不落盘——它就是拿来复制的，铺开成界面上的一个框更顺手。
	text    string
	gotText string

	cancel context.CancelFunc
	win    *shareWindow

	// 下面两个只在视图线程里读写，不跨 goroutine。
	joinCode  string
	lastPaint time.Time
}

// snap 一次性拷出视图要用的那几个字段，避免边读边被后台改。
func (n *netShare) snap() (stage p2pStage, code string, prog p2p.Progress, note, via string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.stage, n.code, n.prog, n.note, n.via
}

// set 改状态并请求重绘。progress 回调很密（每 32KB 一次），所以这里限流，
// 免得把 UI 线程刷爆——文件再大，界面也只需要一秒几次。
func (n *netShare) set(fn func()) {
	n.mu.Lock()
	fn()
	n.mu.Unlock()
	n.paint(false)
}

func (n *netShare) paint(force bool) {
	if n.win == nil || n.win.win == nil {
		return
	}
	if !force && time.Since(n.lastPaint) < 150*time.Millisecond {
		return
	}
	n.lastPaint = time.Now()
	n.win.win.Update(func() {})
}

func (n *netShare) fail(err error) {
	if err == nil {
		return
	}
	n.set(func() {
		n.stage = p2pFailed
		n.note = err.Error()
	})
	log.Printf("跨网互传失败: %v", err)
}

func (n *netShare) finish(note string) {
	n.set(func() {
		n.stage = p2pDone
		n.note = note
	})
}

func (n *netShare) onProgress(p p2p.Progress) {
	n.mu.Lock()
	n.prog = p
	n.mu.Unlock()
	n.paint(false)
}

// setText 是 p2p 收到文本帧时的回调：文本一落地就叫界面重绘。
func (n *netShare) setText(t string) {
	n.mu.Lock()
	n.gotText = t
	n.mu.Unlock()
	n.paint(true)
}

func (n *netShare) got() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.gotText
}

// reset 把上一次的痕迹清干净，供"再来一次"用。
func (n *netShare) reset() {
	n.set(func() {
		n.stage = p2pIdle
		n.code = ""
		n.dir = ""
		n.prog = p2p.Progress{}
		n.note = ""
		n.via = ""
		n.text = ""
		n.gotText = ""
	})
}

func (n *netShare) stop() {
	n.mu.Lock()
	cancel := n.cancel
	n.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// startSend 是发送方入口：生成短码、连信令、等对方输码。
func (s *shareWindow) startSend() {
	items := s.list()
	text := strings.TrimSpace(s.textDraft)
	if len(items) == 0 && text == "" {
		s.status = "先添加要发送的文件/文件夹，或者写一段文本"
		s.win.Update(func() {})
		return
	}
	base := s.signalBase()
	if base == "" {
		s.status = "先填信令服务地址（部署 signal 目录里的 Worker）"
		s.win.Update(func() {})
		return
	}

	if s.net == nil {
		s.net = &netShare{win: s}
	}
	s.net.stop()
	n := s.net

	paths := make([]string, 0, len(items))
	for _, it := range items {
		paths = append(paths, it.Path)
	}

	ctx, cancel := context.WithCancel(context.Background())
	n.set(func() {
		n.cancel = cancel
		n.role = "sender"
		n.code = p2p.NewCode()
		n.stage = p2pWaiting
		n.note = "把这 6 位给对方，等它输进来"
		n.prog = p2p.Progress{}
		n.via = ""
		n.text = text
		n.gotText = ""
	})
	n.paint(true)

	go n.send(ctx, base, paths, text, n.code)
}

// startRecv 是接收方入口：拿对方的码连进去，把东西落到指定目录。
func (s *shareWindow) startRecv() {
	base := s.signalBase()
	if base == "" {
		s.status = "先填信令服务地址（部署 signal 目录里的 Worker）"
		s.win.Update(func() {})
		return
	}
	if s.net == nil {
		s.net = &netShare{win: s}
	}
	code := strings.ToLower(strings.TrimSpace(s.net.joinCode))
	if len(code) != p2p.CodeLen {
		s.status = fmt.Sprintf("短码是 %d 位，现在填了 %d 位", p2p.CodeLen, len(code))
		s.win.Update(func() {})
		return
	}
	if s.net.dir == "" {
		s.status = "先选一个保存位置"
		s.win.Update(func() {})
		return
	}

	s.net.stop()
	n := s.net
	ctx, cancel := context.WithCancel(context.Background())
	n.set(func() {
		n.cancel = cancel
		n.role = "receiver"
		n.code = code
		n.stage = p2pWaiting
		n.note = "正在连信令，等对方把连接信息递过来"
		n.prog = p2p.Progress{}
		n.via = ""
	})
	n.paint(true)

	go n.recv(ctx, base, code, n.dir)
}

// send 是发送方的整条流水线：等对方 → 出 offer → 收 answer → 传文本和文件。
func (n *netShare) send(ctx context.Context, base string, paths []string, text, code string) {
	sig, err := p2p.Dial(ctx, base, code, "sender")
	if err != nil {
		n.fail(err)
		return
	}
	defer sig.Close()

	linkCtx, cancelLink := context.WithTimeout(ctx, linkTimeout)
	defer cancelLink()

	// 先等对方进来。没人进来之前发 offer 也没地方送。
	for {
		m, err := sig.Recv(linkCtx)
		if err != nil {
			n.fail(fmt.Errorf("等待对方时连接断了：%w", err))
			return
		}
		if m.Type == "peer_joined" {
			break
		}
	}

	n.set(func() {
		n.stage = p2pLinking
		n.note = "对方进来了，正在交换连接信息"
	})

	peer, err := p2p.New(p2p.Options{})
	if err != nil {
		n.fail(err)
		return
	}
	defer peer.Close()

	offer, err := peer.CreateOffer()
	if err != nil {
		n.fail(err)
		return
	}
	if err := sig.Send(ctx, p2p.Msg{Type: "offer", SDP: offer}); err != nil {
		n.fail(fmt.Errorf("发连接信息失败：%w", err))
		return
	}

	for {
		m, err := sig.Recv(linkCtx)
		if err != nil {
			n.fail(fmt.Errorf("交换连接信息失败：%w", err))
			return
		}
		switch m.Type {
		case "answer":
			if err := peer.AcceptAnswer(m.SDP); err != nil {
				n.fail(err)
				return
			}
		case "peer_left":
			n.fail(fmt.Errorf("对方中途离开了"))
			return
		default:
			continue
		}
		break
	}

	if err := peer.WaitOpen(linkCtx); err != nil {
		n.fail(fmt.Errorf("直连没建立起来：%w", err))
		return
	}
	via := peer.CandidateType()
	n.set(func() {
		n.stage = p2pTransfer
		n.via = via
		n.note = "直连已建立，开始发送"
	})

	if err := p2p.Send(ctx, peer.Channel(), paths, text, n.onProgress); err != nil {
		n.fail(fmt.Errorf("发送中断：%w", err))
		return
	}
	if text != "" && len(paths) > 0 {
		n.finish(fmt.Sprintf("已发送 1 段文本和 %d 项文件", len(paths)))
	} else if text != "" {
		n.finish("文本已送达，对方在界面上能直接复制")
	} else {
		n.finish(fmt.Sprintf("已发送 %d 项，对方那边应该收到了", len(paths)))
	}
}

// recv 是接收方：等 offer → 回 answer → 收文件。
func (n *netShare) recv(ctx context.Context, base, code, dir string) {
	sig, err := p2p.Dial(ctx, base, code, "receiver")
	if err != nil {
		n.fail(err)
		return
	}
	defer sig.Close()

	linkCtx, cancelLink := context.WithTimeout(ctx, linkTimeout)
	defer cancelLink()

	var offer string
	for {
		m, err := sig.Recv(linkCtx)
		if err != nil {
			n.fail(fmt.Errorf("等对方发连接信息时断了：%w", err))
			return
		}
		if m.Type == "offer" {
			offer = m.SDP
			break
		}
	}

	n.set(func() {
		n.stage = p2pLinking
		n.note = "收到对方的连接信息，正在应答"
	})

	peer, err := p2p.New(p2p.Options{})
	if err != nil {
		n.fail(err)
		return
	}
	defer peer.Close()

	answer, err := peer.AcceptOffer(offer)
	if err != nil {
		n.fail(err)
		return
	}
	if err := sig.Send(ctx, p2p.Msg{Type: "answer", SDP: answer}); err != nil {
		n.fail(fmt.Errorf("回连接信息失败：%w", err))
		return
	}

	if err := peer.WaitOpen(linkCtx); err != nil {
		n.fail(fmt.Errorf("直连没建立起来：%w", err))
		return
	}
	via := peer.CandidateType()
	n.set(func() {
		n.stage = p2pTransfer
		n.via = via
		n.note = "直连已建立，开始接收"
	})

	paths, err := p2p.Receive(ctx, peer.Channel(), dir, n.setText, n.onProgress)
	if err != nil {
		n.fail(fmt.Errorf("接收中断：%w", err))
		return
	}
	switch {
	case n.got() != "" && len(paths) > 0:
		n.finish(fmt.Sprintf("收到 1 段文本和 %d 个文件，文件已存到 %s", len(paths), dir))
	case n.got() != "":
		n.finish("收到一段文本，可以直接复制")
	case len(paths) > 0:
		n.finish(fmt.Sprintf("收到 %d 个文件，已存到 %s", len(paths), dir))
	default:
		n.finish("对方发完了，但没收到任何内容")
	}
}

// p2pTab 画跨网这一页。
func (s *shareWindow) p2pTab(c *ui.Context, t *ui.Theme) {
	if s.net == nil {
		s.net = &netShare{win: s}
	}
	n := s.net

	if !s.signalLoaded {
		s.signalInput = s.signalBase()
		s.signalLoaded = true
	}

	s.signalRow(c, t)

	stage, code, prog, note, via := n.snap()

	switch stage {
	case p2pWaiting, p2pLinking, p2pTransfer:
		s.p2pRunning(c, t, stage, code, prog, note, via)
	case p2pDone, p2pFailed:
		s.p2pResult(c, t, stage, note, via)
	default:
		s.p2pIdle(c, t)
	}
}

// signalRow 是信令地址那一行：填一次，以后都记在配置里。
func (s *shareWindow) signalRow(c *ui.Context, t *ui.Theme) {
	ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
		ui.Text(c, "信令服务").FontSize(12).TextColor(t.TextMuted).Width(80)
		ui.TextInput(c, &s.signalInput).Grow(1).SingleLine()
		if ui.Button(c, "保存").Clicked() {
			s.app.cfg.Share.Signal = strings.TrimSpace(s.signalInput)
			if s.app.cfgPath != "" {
				if err := config.Save(s.app.cfg, s.app.cfgPath); err != nil {
					s.status = "保存信令地址失败：" + err.Error()
					log.Printf("保存信令地址: %v", err)
				} else {
					s.status = "信令地址已保存"
				}
			}
			s.win.Update(func() {})
		}
		if ui.Button(c, "测试").Clicked() {
			go s.testSignal()
		}
	})
	ui.Text(c, "跨网只用它交换一次连接信息；两端连上后文件直连，一个字节都不经过它").
		FontSize(11).TextColor(t.TextMuted)
	s.signalResult(c, t)
}

// 「测试」有几种结果。测试中也要占一格，否则点了按钮到出结果之间是空白，
// 用户会以为按钮没反应、再点两下。
const (
	signalIdle = iota
	signalTesting
	signalOK
	signalBad
)

// signalResult 把测试结果摆在信令行下面：一个色点 + 一句人话（成功还带耗时）。
// 不做 toast：这东西要能一直在，用户回头看一眼就知道上次测的是什么、什么时候。
func (s *shareWindow) signalResult(c *ui.Context, t *ui.Theme) {
	if s.signalMsg == "" {
		return
	}
	color, mark := t.TextMuted, "○"
	switch s.signalTest {
	case signalOK:
		color, mark = t.Accent, "●"
	case signalBad:
		color, mark = t.Danger, "●"
	}
	ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
		ui.Text(c, mark).FontSize(11).TextColor(color)
		ui.Text(c, s.signalMsg).FontSize(12).TextColor(color).Grow(1)
	})
}

func (s *shareWindow) testSignal() {
	base := strings.TrimSpace(s.app.cfg.Share.Signal)
	if base == "" {
		base = strings.TrimSpace(s.signalInput)
	}
	if base == "" {
		s.win.Update(func() {
			s.signalTest = signalBad
			s.signalMsg = "还没有填信令地址"
		})
		return
	}
	// 先亮"正在测"，再去连。这个顺序不能反：连不上时要等 8 秒超时，
	// 期间界面上必须已经有动静。
	s.win.Update(func() {
		s.signalTest = signalTesting
		s.signalMsg = "正在测 " + base + " …"
	})

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	start := time.Now()
	err := p2p.Health(ctx, base)
	cost := time.Since(start).Round(time.Millisecond)

	s.win.Update(func() {
		if err != nil {
			s.signalTest = signalBad
			s.signalMsg = "连不上 " + base + "：" + err.Error()
			s.status = "信令服务连不上"
			return
		}
		s.signalTest = signalOK
		s.signalMsg = fmt.Sprintf("%s 能用，%s 就有回应", base, cost)
		s.status = "信令服务可用"
	})
}

// p2pIdle 是"还没开始"的样子：发送和接收两栏并排。
func (s *shareWindow) p2pIdle(c *ui.Context, t *ui.Theme) {
	items := s.list()
	ui.Row(c).Gap(28).AlignItems(ui.Start).Children(func() {
		// 左：发送
		ui.Column(c).Grow(1).Gap(10).Children(func() {
			ui.Text(c, "发送").FontSize(14).Bold()
			if len(items) == 0 {
				ui.Text(c, "还没加东西。选文件或文件夹，加完点「生成短码」。").
					FontSize(12).TextColor(t.TextMuted)
			} else {
				ui.Text(c, fmt.Sprintf("要发的 %d 项，共 %s", len(items), humanSize(sumSize(items)))).
					FontSize(12).TextColor(t.TextMuted)
			}

			ui.Row(c).Gap(8).Children(func() {
				if ui.Button(c, "添加文件").Clicked() {
					go s.pickFiles()
				}
				if ui.Button(c, "添加文件夹").Clicked() {
					go s.pickFolder()
				}
			})

			// 加了什么得看得见，加错了能就地移除——不然只能凭记忆。
			s.itemsBrief(c, t)

			// 顺手发一段文本：一条命令、一串地址、一份口令。
			// 这种时候存成 txt 让对方再去翻下载目录，纯属多绕两步。
			ui.Text(c, "也可以只发一段文本").FontSize(12).TextColor(t.TextMuted)
			ui.TextArea(c, &s.textDraft).Height(72).FontSize(12).
				Placeholder("贴一条命令、一个地址、一段口令…… 对方在接收端直接复制")

			if ui.PrimaryButton(c, "生成短码").Clicked() {
				s.startSend()
			}
			ui.Text(c, "生成 6 位短码；对方输码，或者用浏览器打开链接都能收").
				FontSize(11).TextColor(t.TextMuted)
		})

		// 右：接收。只要一个短码，保存位置等点了「开始接收」再问。
		ui.Column(c).Width(300).Gap(10).Children(func() {
			ui.Text(c, "接收").FontSize(14).Bold()
			ui.Text(c, "对方给了短码就填在这里").FontSize(12).TextColor(t.TextMuted)
			ui.TextInput(c, &s.net.joinCode).SingleLine().Grow(1)
			if ui.PrimaryButton(c, "开始接收").Clicked() {
				go s.pickDirThenRecv()
			}
			ui.Text(c, "点了会先让你选保存位置，选好才开始连").
				FontSize(11).TextColor(t.TextMuted)
		})
	})
	if s.net.joinCode != "" {
		// 只留短码字符集里的，顺手转小写：手输容易带空格和大写。
		clean := sanitizeCode(s.net.joinCode)
		if clean != s.net.joinCode {
			s.net.joinCode = clean
		}
	}
}

// itemsBrief 是发送栏里的文件清单：加了什么看得见，加错了能就地移除。
func (s *shareWindow) itemsBrief(c *ui.Context, t *ui.Theme) {
	for _, it := range s.list() {
		itPath, itName, itSize, itDir := it.Path, it.Name, it.Size, it.Dir
		ui.Row(c).Padding(6, 10).Gap(10).AlignItems(ui.Center).Radius(8).Background(t.Surface).
			Children(func() {
				label := "文件"
				if itDir {
					label = "文件夹"
				}
				ui.Text(c, itName).FontSize(12).SingleLine().Grow(1)
				ui.Text(c, fmt.Sprintf("%s · %s", label, humanSize(itSize))).
					FontSize(11).TextColor(t.TextMuted)
				if ui.Button(c, "移除").Clicked() {
					s.remove(itPath)
					s.win.Update(func() {})
				}
			})
	}
}

func (s *shareWindow) p2pRunning(c *ui.Context, t *ui.Theme, stage p2pStage, code string, prog p2p.Progress, note, via string) {
	if s.net.role == "sender" && code != "" {
		pageURL := ""
		if base := strings.TrimRight(s.signalBase(), "/"); base != "" {
			pageURL = base + "/r?code=" + code
		}
		// 链接一变就重画二维码。按 256 出图再缩到 140，边上不会糊。
		if pageURL != "" && s.qrURL != pageURL {
			if png, err := qrcode.Encode(pageURL, qrcode.Medium, 256); err == nil {
				if bm, err := ui.DecodeBitmap(png); err == nil {
					s.qr, s.qrURL = bm, pageURL
				}
			}
		}

		ui.Row(c).Gap(18).AlignItems(ui.Start).Children(func() {
			ui.Column(c).Grow(1).Gap(10).Children(func() {
				ui.Row(c).Gap(12).AlignItems(ui.Center).Children(func() {
					ui.Text(c, "短码").FontSize(12).TextColor(t.TextMuted)
					ui.Text(c, code).FontSize(32).Bold()
					if ui.Button(c, "复制短码").Clicked() {
						mygo.Clipboard.WriteText(code)
						s.status = "短码已复制"
					}
				})

				if pageURL != "" {
					ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
						ui.Text(c, "链接").FontSize(12).TextColor(t.TextMuted)
						ui.Text(c, pageURL).FontSize(13).SingleLine().Grow(1)
						if ui.Button(c, "复制链接").Clicked() {
							mygo.Clipboard.WriteText(pageURL)
							s.status = "接收页链接已复制"
						}
					})
					ui.Text(c, "手机扫右边的码，或把链接发给对方——浏览器打开就能收，不用装 Baozi").
						FontSize(11).TextColor(t.TextMuted)
				}
				ui.Text(c, "装了 Baozi 的一方也可以直接输这 6 位短码").FontSize(11).TextColor(t.TextMuted)
			})
			if s.qr != nil {
				ui.Image(c, s.qr).Size(140, 140)
			}
		})
	}

	ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
		ui.Spinner(c)
		ui.Text(c, note).FontSize(12).TextColor(t.TextMuted).Grow(1).SingleLine()
		if via != "" {
			label := "直连"
			if via == "relay" {
				label = "经中继"
			}
			ui.Badge(c, label)
		}
	})

	if stage == p2pTransfer {
		ratio := 0.0
		if prog.Total > 0 {
			ratio = float64(prog.Done) / float64(prog.Total)
		}
		ui.Progress(c, ratio).Width(480)
		ui.Text(c, fmt.Sprintf("%s / %s　%s",
			humanSize(prog.Done), humanSize(prog.Total), shortName(prog.File))).
			FontSize(11).TextColor(t.TextMuted).SingleLine()
	}

	// 取消按钮不该占满整行：它是个次要动作，宽度按文字来。
	ui.Row(c).Children(func() {
		if ui.Button(c, "取消").Clicked() {
			s.net.stop()
			s.net.reset()
			s.status = "已取消"
			s.win.Update(func() {})
		}
	})
}

func (s *shareWindow) p2pResult(c *ui.Context, t *ui.Theme, stage p2pStage, note, via string) {
	color := t.Text
	if stage == p2pFailed {
		color = t.Danger
	}
	ui.Text(c, note).FontSize(13).TextColor(color)
	if via != "" {
		ui.Text(c, "本条连接方式："+via).FontSize(11).TextColor(t.TextMuted)
	}

	// 收到的文本直接铺开：它存在的意义就是被复制走，塞进一个文件里没道理。
	if text := s.net.got(); text != "" {
		ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
			ui.Text(c, "收到的文本").FontSize(12).TextColor(t.TextMuted)
			if ui.Button(c, "复制文本").Clicked() {
				mygo.Clipboard.WriteText(text)
				s.status = "文本已复制"
			}
			ui.Text(c, fmt.Sprintf("%d 个字符", len([]rune(text)))).
				FontSize(11).TextColor(t.TextMuted)
		})
		// 只读：要改也是在对方那边改，这里只负责看和复制。
		shown := text
		ui.TextArea(c, &shown).Height(96).FontSize(12).ReadOnly(true)
	}

	if ui.PrimaryButton(c, "再来一次").Clicked() {
		s.net.reset()
		s.win.Update(func() {})
	}
}

// pickDirThenRecv 是接收方的入口：先问存哪儿，选好了才真正开始连。
// 顺序反过来的话，用户点了"开始接收"却不知道文件会落到哪，还得回头找那个目录开关。
func (s *shareWindow) pickDirThenRecv() {
	paths, err := mygo.Dialog.Open(mygo.OpenDialogOptions{
		Parent:    s.win,
		Title:     "选择接收保存位置",
		Directory: true,
	})
	if err != nil {
		s.win.Update(func() { s.status = "选目录失败：" + err.Error() })
		return
	}
	if len(paths) == 0 {
		return // 取消了就别进"等对方"的状态
	}
	s.net.set(func() { s.net.dir = paths[0] })
	s.startRecv()
}

func (s *shareWindow) pickSaveDir() {
	paths, err := mygo.Dialog.Open(mygo.OpenDialogOptions{Parent: s.win, Title: "选择接收保存位置", Directory: true})
	if err != nil {
		s.win.Update(func() { s.status = "选目录失败：" + err.Error() })
		return
	}
	if len(paths) == 0 {
		return
	}
	dir := paths[0]
	s.net.set(func() { s.net.dir = dir })
	s.win.Update(func() { s.status = "保存位置：" + dir })
}

// sumSize 把待发的条目加起来，给"要发多少"用。
func sumSize(items []share.Item) int64 {
	var n int64
	for _, it := range items {
		n += it.Size
	}
	return n
}

func dirLabel(dir string) string {
	if dir == "" {
		return "还没选保存位置"
	}
	return dir
}

// shortName 只留文件名，进度那行不显示整条路径。
func shortName(p string) string {
	if p == "" {
		return ""
	}
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

// sanitizeCode 只保留短码字符集里的小写字符，并截到 6 位。
func sanitizeCode(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune("23456789abcdefghjkmnpqrstuvwxyz", r) {
			b.WriteRune(r)
		}
		if b.Len() >= p2p.CodeLen {
			break
		}
	}
	return b.String()
}

// signalBase 是当前该用的信令地址：配置里填过就用它，没填过就用内置默认。
// 空串只在用户把它删干净之后才可能出现，那时界面上会提示去填。
func (s *shareWindow) signalBase() string {
	if v := strings.TrimSpace(s.app.cfg.Share.Signal); v != "" {
		return v
	}
	return config.DefaultSignal
}
