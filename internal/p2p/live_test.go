//go:build live

package p2p

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 这条只用真环境跑：设 BAOZI_SIGNAL=<部署出来的 Worker 地址>，再
//
//	go test ./internal/p2p -tags live -run TestLiveSignal -timeout 90s
//
// 它会真的连一次 Cloudflare 信令、真的打一次 STUN，把两个 PeerConnection
// 撮合到一起再传一个文件。跟单元测试的区别就是"全程走网络"。
func TestLiveSignal(t *testing.T) {
	base := os.Getenv("BAOZI_SIGNAL")
	if base == "" {
		t.Skip("没设 BAOZI_SIGNAL，跳过真机联调")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Second)
	defer cancel()

	if err := Health(ctx, base); err != nil {
		t.Fatalf("信令服务探活失败：%v", err)
	}

	code := NewCode()
	t.Logf("本次短码：%s", code)

	// 源文件
	src := t.TempDir()
	payload := []byte("跨网直连，只走一次信令\n")
	if err := os.WriteFile(filepath.Join(src, "hello.txt"), payload, 0o644); err != nil {
		t.Fatalf("造文件失败：%v", err)
	}
	dst := t.TempDir()

	type res struct{ err error }
	recvDone := make(chan res, 1)

	// 接收方先连上，等着对方把 offer 递过来。
	go func() {
		sig, err := Dial(ctx, base, code, "receiver")
		if err != nil {
			t.Logf("接收方连信令失败：%v", err)
			recvDone <- res{err}
			return
		}
		t.Logf("接收方已连上信令，短码 %s", code)
		defer sig.Close()

		var offer string
		for {
			m, err := sig.Recv(ctx)
			if err != nil {
				t.Logf("接收方等 offer 时断了：%v", err)
				recvDone <- res{err}
				return
			}
			t.Logf("接收方收到信令消息：%s", m.Type)
			if m.Type == "offer" {
				offer = m.SDP
				break
			}
		}

		peer, err := New(Options{})
		if err != nil {
			recvDone <- res{err}
			return
		}
		defer peer.Close()

		answer, err := peer.AcceptOffer(offer)
		if err != nil {
			recvDone <- res{err}
			return
		}
		if err := sig.Send(ctx, Msg{Type: "answer", SDP: answer}); err != nil {
			recvDone <- res{err}
			return
		}
		if err := peer.WaitOpen(ctx); err != nil {
			recvDone <- res{err}
			return
		}
		t.Logf("接收方连接方式：%s", peer.CandidateType())

		paths, err := Receive(ctx, peer.Channel(), dst, nil, nil)
		t.Logf("收到 %d 个文件：%v", len(paths), paths)
		recvDone <- res{err}
	}()

	// 给接收方一点时间挂上去，再开始发送。
	time.Sleep(2 * time.Second)

	sig, err := Dial(ctx, base, code, "sender")
	if err != nil {
		t.Fatalf("发送方连信令失败：%v", err)
	}
	defer sig.Close()

	for {
		m, err := sig.Recv(ctx)
		if err != nil {
			t.Fatalf("等对方时断了：%v", err)
		}
		if m.Type == "peer_joined" {
			break
		}
	}
	t.Log("对方已进入房间")

	peer, err := New(Options{})
	if err != nil {
		t.Fatalf("建连接失败：%v", err)
	}
	defer peer.Close()

	offer, err := peer.CreateOffer()
	if err != nil {
		t.Fatalf("生成 offer 失败：%v", err)
	}
	if err := sig.Send(ctx, Msg{Type: "offer", SDP: offer}); err != nil {
		t.Fatalf("发 offer 失败：%v", err)
	}

	m, err := sig.Recv(ctx)
	if err != nil || m.Type != "answer" {
		t.Fatalf("没收 answer：%v（类型 %q）", err, m.Type)
	}
	if err := peer.AcceptAnswer(m.SDP); err != nil {
		t.Fatalf("收 answer 失败：%v", err)
	}
	if err := peer.WaitOpen(ctx); err != nil {
		t.Fatalf("通道没打开：%v", err)
	}
	t.Logf("发送方连接方式：%s", peer.CandidateType())

	if err := Send(ctx, peer.Channel(), []string{src}, "", nil); err != nil {
		t.Fatalf("发送失败：%v", err)
	}

	select {
	case r := <-recvDone:
		if r.err != nil {
			t.Fatalf("接收失败：%v", r.err)
		}
	case <-ctx.Done():
		t.Fatalf("接收超时")
	}

	got, err := os.ReadFile(filepath.Join(dst, filepath.Base(src), "hello.txt"))
	if err != nil {
		t.Fatalf("读回文件失败：%v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("内容不一致：%q", string(got))
	}
	t.Log("真机联调通过：信令 → STUN → 直连 → 文件落地")
}
