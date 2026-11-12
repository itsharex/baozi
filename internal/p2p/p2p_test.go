package p2p

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 只发一段文本也要走得通：文本不落盘，直接从通道上交给界面去显示和复制。
// "互传"如果只认文件，一条命令、一串地址就得先存成 txt 再让对方翻下载目录。
func TestLoopbackTransferCarriesText(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	offerer, answerer := newLoopbackPair(t, ctx)
	defer offerer.Close()
	defer answerer.Close()

	want := "Baozi 文本互传\ncurl -fsSL https://example.com/x.sh | sh\n中文也不能坏"

	var gotText string
	var gotPaths []string
	done := make(chan error, 1)
	go func() {
		paths, err := Receive(ctx, answerer.Channel(), t.TempDir(), func(s string) {
			gotText = s
		}, nil)
		gotPaths = paths
		done <- err
	}()

	// 让接收端先把 OnMessage 注册好，再开始发。
	time.Sleep(100 * time.Millisecond)

	if err := Send(ctx, offerer.Channel(), nil, want, nil); err != nil {
		t.Fatalf("发送失败：%v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("接收失败：%v", err)
		}
	case <-ctx.Done():
		t.Fatalf("接收超时")
	}

	if gotText != want {
		t.Fatalf("文本对不上：\n期望 %q\n实际 %q", want, gotText)
	}
	if len(gotPaths) != 0 {
		t.Fatalf("只发文本不应该落任何文件，实际落了 %v", gotPaths)
	}
}

// newLoopbackPair 建一对已经连通的本机 PeerConnection（只走 host 候选，不碰网络）。
func newLoopbackPair(t *testing.T, ctx context.Context) (offerer, answerer *Peer) {
	t.Helper()

	offerer, err := New(Options{HostOnly: true})
	if err != nil {
		t.Fatalf("建 offer 方失败：%v", err)
	}
	answerer, err = New(Options{HostOnly: true})
	if err != nil {
		t.Fatalf("建 answer 方失败：%v", err)
	}

	offerSDP, err := offerer.CreateOffer()
	if err != nil {
		t.Fatalf("生成 offer 失败：%v", err)
	}
	answerSDP, err := answerer.AcceptOffer(offerSDP)
	if err != nil {
		t.Fatalf("收 offer 失败：%v", err)
	}
	if err := offerer.AcceptAnswer(answerSDP); err != nil {
		t.Fatalf("收 answer 失败：%v", err)
	}
	if err := offerer.WaitOpen(ctx); err != nil {
		t.Fatalf("offer 方通道没打开：%v", err)
	}
	if err := answerer.WaitOpen(ctx); err != nil {
		t.Fatalf("answer 方通道没打开：%v", err)
	}
	return offerer, answerer
}

// 这条测的是最要命的一环：两条 PeerConnection 在真的握手里能不能开出通道、
// 把一份目录原样搬过去。只走本机候选（HostOnly），不打 STUN、不依赖网络，
// 所以它在断网环境下也必须过。
func TestLoopbackTransferCarriesFiles(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	offerer, err := New(Options{HostOnly: true})
	if err != nil {
		t.Fatalf("建 offer 方失败：%v", err)
	}
	defer offerer.Close()

	answerer, err := New(Options{HostOnly: true})
	if err != nil {
		t.Fatalf("建 answer 方失败：%v", err)
	}
	defer answerer.Close()

	offerSDP, err := offerer.CreateOffer()
	if err != nil {
		t.Fatalf("生成 offer 失败：%v", err)
	}
	if !strings.Contains(offerSDP, "m=application") {
		t.Fatalf("offer 里没有数据通道：%s", offerSDP[:min(64, len(offerSDP))])
	}

	answerSDP, err := answerer.AcceptOffer(offerSDP)
	if err != nil {
		t.Fatalf("收 offer 失败：%v", err)
	}
	if err := offerer.AcceptAnswer(answerSDP); err != nil {
		t.Fatalf("收 answer 失败：%v", err)
	}

	if err := offerer.WaitOpen(ctx); err != nil {
		t.Fatalf("offer 方通道没打开：%v", err)
	}
	if err := answerer.WaitOpen(ctx); err != nil {
		t.Fatalf("answer 方通道没打开：%v", err)
	}

	// 源：一个目录，里面放文本、二进制（跨多个块）、还有一层子目录。
	src := t.TempDir()
	payload := bytes.Repeat([]byte("Baozi 跨网互传\n"), 40*1024) // 约 480 KB，跨 15 个块
	if err := os.WriteFile(filepath.Join(src, "big.txt"), payload, 0o644); err != nil {
		t.Fatalf("造测试文件失败：%v", err)
	}
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatalf("造子目录失败：%v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "note.md"), []byte("# 一句话\n"), 0o644); err != nil {
		t.Fatalf("造子文件失败：%v", err)
	}

	dst := t.TempDir()
	type result struct {
		paths []string
		err   error
	}
	done := make(chan result, 1)
	go func() {
		paths, err := Receive(ctx, answerer.Channel(), dst, nil, nil)
		done <- result{paths, err}
	}()

	// 让接收端先把 OnMessage 注册好，再开始发。
	time.Sleep(100 * time.Millisecond)

	if err := Send(ctx, offerer.Channel(), []string{src}, "", nil); err != nil {
		t.Fatalf("发送失败：%v", err)
	}

	var got result
	select {
	case got = <-done:
	case <-ctx.Done():
		t.Fatalf("接收超时")
	}
	if got.err != nil {
		t.Fatalf("接收失败：%v", got.err)
	}
	if len(got.paths) != 2 {
		t.Fatalf("应该收到 2 个文件，实际 %d 个：%v", len(got.paths), got.paths)
	}

	// 内容必须一字不差。
	rd, err := os.ReadFile(filepath.Join(dst, filepath.Base(src), "big.txt"))
	if err != nil {
		t.Fatalf("读回大文件失败：%v", err)
	}
	if !bytes.Equal(rd, payload) {
		t.Fatalf("大文件内容对不上：收到 %d 字节，发出 %d 字节", len(rd), len(payload))
	}
	if rd, err := os.ReadFile(filepath.Join(dst, filepath.Base(src), "sub", "note.md")); err != nil {
		t.Fatalf("读回子文件失败：%v", err)
	} else if string(rd) != "# 一句话\n" {
		t.Fatalf("子文件内容对不上：%q", string(rd))
	}

	// 目录层级要保住：根文件夹名 + 里面的结构。
	if !strings.HasSuffix(filepath.Dir(got.paths[0]), filepath.Base(src)) {
		t.Logf("落地路径：%v", got.paths)
	}
}

// 对端构造的名字不能跳出目标目录。这条是防"对方让我把文件写到 C:\Windows"。
func TestCleanRelRejectsTraversal(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"a/b.txt", "a/b.txt", true},
		{`a\b.txt`, "a/b.txt", true},
		{"../evil.txt", "", false},
		{"a/../../evil.txt", "", false},
		{`C:\Windows\system.ini`, "C_/Windows/system.ini", true}, // 冒号被换掉，盘符语义没了
		{"", "", false},
		{".", "", false},
		{"a//b.txt", "a/b.txt", true},
	}
	for _, c := range cases {
		got, ok := cleanRel(c.in)
		if ok != c.ok {
			t.Fatalf("%q：可接受性应为 %v，实际 %v", c.in, c.ok, ok)
		}
		if ok && got != c.want {
			t.Fatalf("%q：清洗后应为 %q，实际 %q", c.in, c.want, got)
		}
	}
}

// 短码要是能被猜出来，房间就等于不设防。
func TestNewCodeShape(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		code := NewCode()
		if len(code) != CodeLen {
			t.Fatalf("短码长度应为 %d，实际 %d：%q", CodeLen, len(code), code)
		}
		if !validCode(code) {
			t.Fatalf("生成了非法短码：%q", code)
		}
		if seen[code] {
			t.Fatalf("200 次里撞了码：%q", code)
		}
		seen[code] = true
	}
}

func TestWSURLNormalizesBase(t *testing.T) {
	cases := []struct {
		base, want string
	}{
		{"https://baozi-signal.abc.workers.dev", "wss://baozi-signal.abc.workers.dev/ws?code=abc234&role=sender"},
		{"https://baozi-signal.abc.workers.dev/", "wss://baozi-signal.abc.workers.dev/ws?code=abc234&role=sender"},
		{"http://127.0.0.1:8787", "ws://127.0.0.1:8787/ws?code=abc234&role=sender"},
		{"wss://x.dev", "wss://x.dev/ws?code=abc234&role=sender"},
	}
	for _, c := range cases {
		got, err := wsURL(c.base, "abc234", "sender")
		if err != nil {
			t.Fatalf("%q 应可用，报错 %v", c.base, err)
		}
		if got != c.want {
			t.Fatalf("%q → %q，期望 %q", c.base, got, c.want)
		}
	}

	if _, err := wsURL("", "abc234", "sender"); err == nil {
		t.Fatal("空地址应当报错")
	}
	// 短视频、非法字符、错误角色都得拦住。
	if _, err := wsURL("https://x.dev", "abc23", "sender"); err == nil {
		t.Fatal("5 位短码应当报错")
	}
	if _, err := wsURL("https://x.dev", "abc23o", "sender"); err == nil {
		t.Fatal("含 o 的短码应当报错（字符集里没有它）")
	}
	if _, err := wsURL("https://x.dev", "abc234", "spy"); err == nil {
		t.Fatal("非法角色应当报错")
	}
}
