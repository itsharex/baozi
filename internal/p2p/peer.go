package p2p

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
)

const (
	// DefaultSTUN 是 Cloudflare 的公共 STUN：只用来问"我在公网上长什么样"，
	// 免费且不限量，也不经手任何数据。
	DefaultSTUN = "stun:stun.cloudflare.com:3478"

	dataChannelLabel = "baozi"
	// gatherBudget 是等候选地址收集完的上限。STUN 正常 1 秒左右就回来了，
	// 网络差或者要过 TURN 时慢一些；到点就用手上已有的候选先走。
	gatherBudget = 8 * time.Second
)

// TURN 是中继兜底：两头都打不通（对称 NAT、公司网络）时，数据从这里转一手。
// 不填就不用，填了也只是"必要时才走"。
type TURN struct {
	URLs       []string
	Username   string
	Credential string
}

// Options 是建连参数。
type Options struct {
	STUN []string
	TURN *TURN
	// HostOnly 只收集本机地址，供测试里两个进程直连用，不打 STUN。
	HostOnly bool
}

// Peer 是一条 WebRTC 连接 + 一条可靠有序的 DataChannel。
//
// 交换方式刻意做得最笨：**不用 trickle**。双方把候选地址收全了再塞进 SDP 一次性发过去，
// 于是信令上只有 offer 和 answer 两条消息，没有"候选先到、描述后到"的乱序要处理。
// 代价是多等一两秒，换来的是这套东西没有一处需要靠时序侥幸。
type Peer struct {
	pc  *webrtc.PeerConnection
	dc  *webrtc.DataChannel
	cfg Options

	mu   sync.Mutex
	open bool

	openCh   chan struct{}
	openOnce sync.Once
	failCh   chan error
	failOnce sync.Once
}

// New 建一条连接。offer 方和 answer 方都要先调它。
func New(o Options) (*Peer, error) {
	if len(o.STUN) == 0 && o.TURN == nil && !o.HostOnly {
		o.STUN = []string{DefaultSTUN}
	}

	cfg := webrtc.Configuration{}
	for _, s := range o.STUN {
		cfg.ICEServers = append(cfg.ICEServers, webrtc.ICEServer{URLs: []string{s}})
	}
	if o.TURN != nil && len(o.TURN.URLs) > 0 {
		cfg.ICEServers = append(cfg.ICEServers, webrtc.ICEServer{
			URLs:       o.TURN.URLs,
			Username:   o.TURN.Username,
			Credential: o.TURN.Credential,
		})
	}

	pc, err := webrtc.NewPeerConnection(cfg)
	if err != nil {
		return nil, fmt.Errorf("建 WebRTC 连接失败：%w", err)
	}

	p := &Peer{
		pc:     pc,
		cfg:    o,
		openCh: make(chan struct{}),
		failCh: make(chan error, 1),
	}

	// answer 方不主动建通道，靠这个回调拿到对面开的那条。
	pc.OnDataChannel(func(dc *webrtc.DataChannel) { p.attach(dc) })

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		switch state {
		case webrtc.PeerConnectionStateConnected:
			// 通道的 open 才是真的能发数据，这里只做失败上报。
		case webrtc.PeerConnectionStateFailed:
			p.fail(errors.New("直连失败：两端网络都没打通（对称 NAT 或防火墙），需要中继兜底"))
		case webrtc.PeerConnectionStateClosed:
			p.fail(errors.New("连接已关闭"))
		}
	})

	return p, nil
}

func (p *Peer) attach(dc *webrtc.DataChannel) {
	p.mu.Lock()
	p.dc = dc
	p.mu.Unlock()

	dc.OnOpen(func() {
		p.mu.Lock()
		p.open = true
		p.mu.Unlock()
		p.openOnce.Do(func() { close(p.openCh) })
	})
	dc.OnError(func(err error) {
		p.fail(fmt.Errorf("数据通道出错：%w", err))
	})
}

func (p *Peer) fail(err error) {
	p.failOnce.Do(func() {
		select {
		case p.failCh <- err:
		default:
		}
	})
}

// CreateOffer 建通道、生成本端描述。返回的 SDP 已经带上了收集到的候选地址。
func (p *Peer) CreateOffer() (string, error) {
	dc, err := p.pc.CreateDataChannel(dataChannelLabel, nil)
	if err != nil {
		return "", fmt.Errorf("建数据通道失败：%w", err)
	}
	p.attach(dc)

	offer, err := p.pc.CreateOffer(nil)
	if err != nil {
		return "", fmt.Errorf("生成 offer 失败：%w", err)
	}
	if err := p.pc.SetLocalDescription(offer); err != nil {
		return "", fmt.Errorf("设置本地描述失败：%w", err)
	}
	waitGather(p.pc)

	local := p.pc.LocalDescription()
	if local == nil {
		return "", errors.New("拿不到本地描述")
	}
	return local.SDP, nil
}

// AcceptOffer 收下对面的 offer，回一个 answer。
func (p *Peer) AcceptOffer(sdp string) (string, error) {
	if err := p.pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  sdp,
	}); err != nil {
		return "", fmt.Errorf("对方发来的 offer 用不了：%w", err)
	}
	answer, err := p.pc.CreateAnswer(nil)
	if err != nil {
		return "", fmt.Errorf("生成 answer 失败：%w", err)
	}
	if err := p.pc.SetLocalDescription(answer); err != nil {
		return "", fmt.Errorf("设置本地描述失败：%w", err)
	}
	waitGather(p.pc)

	local := p.pc.LocalDescription()
	if local == nil {
		return "", errors.New("拿不到本地描述")
	}
	return local.SDP, nil
}

// AcceptAnswer 收下对面的 answer，连接进入握手。
func (p *Peer) AcceptAnswer(sdp string) error {
	if err := p.pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  sdp,
	}); err != nil {
		return fmt.Errorf("对方发来的 answer 用不了：%w", err)
	}
	return nil
}

// WaitOpen 等通道真正打开（可以发数据了）。
func (p *Peer) WaitOpen(ctx context.Context) error {
	select {
	case <-p.openCh:
		return nil
	case err := <-p.failCh:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Channel 是那条数据通道。WaitOpen 之后才有效。
func (p *Peer) Channel() *webrtc.DataChannel {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.dc
}

// CandidateType 报告本次连接用的是哪一类候选（host / srflx / relay）。
// 直连成功时用它是为了在界面上诚实地说明"数据有没有绕道服务器"。
func (p *Peer) CandidateType() string {
	stats := p.pc.GetStats()
	for _, s := range stats {
		if pair, ok := s.(webrtc.ICECandidatePairStats); ok {
			if pair.State != webrtc.StatsICECandidatePairStateSucceeded && pair.Nominated == false {
				continue
			}
			for _, cs := range stats {
				cand, ok := cs.(webrtc.ICECandidateStats)
				if !ok {
					continue
				}
				if cand.ID == pair.LocalCandidateID || cand.ID == pair.RemoteCandidateID {
					if cand.CandidateType == webrtc.ICECandidateTypeRelay {
						return "relay"
					}
				}
			}
		}
	}
	return "direct"
}

// Close 拆掉这条连接。对端会收到通道关闭。
func (p *Peer) Close() error {
	if p == nil || p.pc == nil {
		return nil
	}
	return p.pc.Close()
}

// waitGather 等候选收集完，最多等 gatherBudget；到点不抛弃已收集的部分。
func waitGather(pc *webrtc.PeerConnection) {
	done := webrtc.GatheringCompletePromise(pc)
	select {
	case <-done:
	case <-time.After(gatherBudget):
	}
}
