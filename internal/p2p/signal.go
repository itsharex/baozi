package p2p

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// 信令服务只负责把两端的"名片"（SDP + ICE 候选）互相转一次，
// 之后文件走 WebRTC 直连，连一个字节都不过服务器。
//
// 短码就是房间名：发送方生成它、连上去；接收方输同一个码连进来，
// 两边在 Worker 的同一个 Durable Object 实例里配对。

const (
	// codeChars 去掉了 0/o/1/l/i 这些看着像的字符：这码要靠人念、靠手输。
	codeChars = "23456789abcdefghjkmnpqrstuvwxyz"
	CodeLen   = 6
)

// Msg 是两端互传的信封。服务端做纯转发，除了 peer_joined / peer_left 之外不看内容。
type Msg struct {
	Type string `json:"type"`
	// SDP 是 offer / answer 的描述。
	SDP string `json:"sdp,omitempty"`
	// Candidate 是 trickle ICE 的一条候选地址，原样透传。
	Candidate json.RawMessage `json:"candidate,omitempty"`
}

// NewCode 生一个 6 位短码。
func NewCode() string {
	b := make([]byte, CodeLen)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 失败在 Windows 上基本不可能，真发生了也别用可预测的值。
		panic("p2p: 取随机数失败: " + err.Error())
	}
	out := make([]byte, CodeLen)
	for i, v := range b {
		out[i] = codeChars[int(v)%len(codeChars)]
	}
	return string(out)
}

// Signal 是一条到信令服务的 WebSocket。
type Signal struct {
	ws   *websocket.Conn
	code string
}

// Dial 连上信令服务，进入 code 这个房间，扮演 role（"sender" 或 "receiver"）。
// base 是部署出来的 Worker 地址，https:// 或 http:// 都行。
func Dial(ctx context.Context, base, code, role string) (*Signal, error) {
	url, err := wsURL(base, code, role)
	if err != nil {
		return nil, err
	}
	conn, resp, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("连信令服务失败（HTTP %d）：%w", resp.StatusCode, err)
		}
		return nil, fmt.Errorf("连信令服务失败：%w", err)
	}
	// 房间里只有几小段 JSON，放宽一点免得消息被掐。
	conn.SetReadLimit(1 << 20)
	return &Signal{ws: conn, code: code}, nil
}

// wsURL 把用户填的地址变成 WebSocket 端点。用户可能填 https://、http://，
// 也可能把结尾的斜杠一起复制进来，这里统一收。
func wsURL(base, code, role string) (string, error) {
	b := strings.TrimRight(strings.TrimSpace(base), "/")
	if b == "" {
		return "", errors.New("还没填信令服务地址")
	}
	switch {
	case strings.HasPrefix(b, "https://"):
		b = "wss://" + strings.TrimPrefix(b, "https://")
	case strings.HasPrefix(b, "http://"):
		b = "ws://" + strings.TrimPrefix(b, "http://")
	case strings.HasPrefix(b, "wss://"), strings.HasPrefix(b, "ws://"):
		// 已经是 WebSocket 地址，原样用。
	default:
		return "", fmt.Errorf("信令地址要以 https:// 开头：%s", base)
	}
	if !validCode(code) {
		return "", fmt.Errorf("短码不合法（要 %d 位 0-9a-z）：%s", CodeLen, code)
	}
	if role != "sender" && role != "receiver" {
		return "", fmt.Errorf("角色只能是 sender 或 receiver：%s", role)
	}
	return b + "/ws?code=" + code + "&role=" + role, nil
}

func validCode(code string) bool {
	if len(code) != CodeLen {
		return false
	}
	for _, r := range code {
		if !strings.ContainsRune(codeChars, r) {
			return false
		}
	}
	return true
}

// Health 探一下服务在不在，给设置页的"测试"按钮用。
func Health(ctx context.Context, base string) error {
	b := strings.TrimRight(strings.TrimSpace(base), "/")
	if b == "" {
		return errors.New("还没填信令服务地址")
	}
	switch {
	case strings.HasPrefix(b, "wss://"):
		b = "https://" + strings.TrimPrefix(b, "wss://")
	case strings.HasPrefix(b, "ws://"):
		b = "http://" + strings.TrimPrefix(b, "ws://")
	case strings.HasPrefix(b, "https://"), strings.HasPrefix(b, "http://"):
	default:
		return fmt.Errorf("信令地址要以 https:// 开头：%s", base)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("连不上信令服务：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("信令服务返回 HTTP %d（地址对吗）", resp.StatusCode)
	}
	return nil
}

// Send 发一条消息给对端（服务端会转发）。
func (s *Signal) Send(ctx context.Context, m Msg) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return s.ws.Write(ctx, websocket.MessageText, data)
}

// Recv 收一条消息。对端进来/离开是服务端发的控制消息，也一样从这个口出来。
func (s *Signal) Recv(ctx context.Context) (Msg, error) {
	_, data, err := s.ws.Read(ctx)
	if err != nil {
		return Msg{}, err
	}
	var m Msg
	if err := json.Unmarshal(data, &m); err != nil {
		return Msg{}, fmt.Errorf("信令消息格式不对：%w", err)
	}
	return m, nil
}

// Close 关掉这条 WebSocket。对端会收到 peer_left。
func (s *Signal) Close() error {
	if s == nil || s.ws == nil {
		return nil
	}
	return s.ws.Close(websocket.StatusNormalClosure, "bye")
}
