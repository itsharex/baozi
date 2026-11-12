package p2p

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pion/webrtc/v4"
)

// 传输协议刻意做得极简，只有三种帧：
//
//  1. 文本 {"type":"manifest","items":[{"path":"相对路径","size":字节}]}
//  2. 二进制 [4 字节文件序号][数据块]        —— 靠有序通道保证顺序，所以不用带偏移
//  3. 文本 {"type":"done","total":字节}
//
// 不带校验和：SCTP 自己就是可靠有序传输，链路层的校验已经管到字节级。
const (
	// ChunkSize 32KB：够小不至于让对端缓冲爆掉，够大不至于让每块的开销占比过高。
	ChunkSize = 32 << 10
	// bufferLimit 是本地待发缓冲的上限，超了就等一等——不让发送把内存吃穿。
	bufferLimit = 1 << 20
	// ioPoll 是等待发送缓冲排空的轮询间隔。
	ioPoll = 5 * time.Millisecond
)

// Item 是清单里的一项。
type Item struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

type manifestMsg struct {
	Type  string `json:"type"`
	Items []Item `json:"items"`
}

type doneMsg struct {
	Type  string `json:"type"`
	Total int64  `json:"total"`
}

// textMsg 是一段文本。互传不只传文件：随手发一段话、一个地址、一条命令，
// 对方在接收端直接点复制，比"存成 txt 再打开"省事得多。
type textMsg struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Progress 是给界面看的进度：总字节、已发/已收字节、当前文件。
type Progress struct {
	Done  int64
	Total int64
	File  string
	Index int
	Files int
}

type entry struct {
	abs  string
	rel  string
	size int64
}

// collect 把用户选的文件/文件夹展开成"绝对路径 + 传输用的相对路径"。
// 单个文件就用自己的文件名；文件夹则以文件夹名为根，保住里面的层级。
func collect(paths []string) ([]entry, error) {
	var out []entry
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, fmt.Errorf("读不了 %s：%w", p, err)
		}
		if !info.IsDir() {
			out = append(out, entry{abs: p, rel: filepath.Base(p), size: info.Size()})
			continue
		}
		root := filepath.Base(filepath.Clean(p))
		err = filepath.Walk(p, func(path string, fi os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if fi.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(p, path)
			if err != nil {
				return err
			}
			out = append(out, entry{
				abs:  path,
				rel:  filepath.Join(root, rel),
				size: fi.Size(),
			})
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("遍历 %s 失败：%w", p, err)
		}
	}
	if len(out) == 0 && len(paths) > 0 {
		return nil, fmt.Errorf("没有可传的文件")
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].rel < out[j].rel })
	return out, nil
}

// Send 把 paths（文件或文件夹）和一段可选文本推到对端。
// 文本先走：它小、先到手就能早一点复制出去，也是"只发一段话"时的全部内容。
func Send(ctx context.Context, dc *webrtc.DataChannel, paths []string, text string, onProgress func(Progress)) error {
	entries, err := collect(paths)
	if err != nil {
		return err
	}
	if len(entries) == 0 && text == "" {
		return fmt.Errorf("没有要传的东西")
	}

	if text != "" {
		raw, err := json.Marshal(textMsg{Type: "text", Text: text})
		if err != nil {
			return err
		}
		if err := dc.SendText(string(raw)); err != nil {
			return fmt.Errorf("发文本失败：%w", err)
		}
	}

	var total int64
	items := make([]Item, len(entries))
	for i, e := range entries {
		total += e.size
		items[i] = Item{Path: e.rel, Size: e.size}
	}

	raw, err := json.Marshal(manifestMsg{Type: "manifest", Items: items})
	if err != nil {
		return err
	}
	if err := dc.SendText(string(raw)); err != nil {
		return fmt.Errorf("发清单失败：%w", err)
	}

	var sent int64
	buf := make([]byte, ChunkSize)
	for i, e := range entries {
		if err := drain(ctx, dc); err != nil {
			return err
		}
		if err := sendFile(ctx, dc, uint32(i), e, buf, func(n int) {
			sent += int64(n)
			if onProgress != nil {
				onProgress(Progress{Done: sent, Total: total, File: e.rel, Index: i + 1, Files: len(entries)})
			}
		}); err != nil {
			return err
		}
	}

	raw, _ = json.Marshal(doneMsg{Type: "done", Total: total})
	if err := dc.SendText(string(raw)); err != nil {
		return fmt.Errorf("发结束帧失败：%w", err)
	}
	if onProgress != nil {
		onProgress(Progress{Done: total, Total: total, Index: len(entries), Files: len(entries)})
	}
	return nil
}

func sendFile(ctx context.Context, dc *webrtc.DataChannel, idx uint32, e entry, buf []byte, onChunk func(int)) error {
	f, err := os.Open(e.abs)
	if err != nil {
		return fmt.Errorf("打不开 %s：%w", e.abs, err)
	}
	defer f.Close()

	for {
		n, err := f.Read(buf)
		if n > 0 {
			// 每块的头部只有文件序号：同一条通道是有序的，接收端按到达顺序往后写就行。
			packet := make([]byte, 4+n)
			binary.LittleEndian.PutUint32(packet[:4], idx)
			copy(packet[4:], buf[:n])

			for dc.BufferedAmount() > bufferLimit {
				if err := drain(ctx, dc); err != nil {
					return err
				}
			}
			if serr := dc.Send(packet); serr != nil {
				return fmt.Errorf("发数据块失败：%w", serr)
			}
			onChunk(n)
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("读 %s 失败：%w", e.abs, err)
		}
	}
}

// drain 等发送缓冲排空一点，别把内存堆起来。
func drain(ctx context.Context, dc *webrtc.DataChannel) error {
	for dc.BufferedAmount() > bufferLimit {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(ioPoll):
		}
	}
	return nil
}

type frame struct {
	data []byte
	text bool
}

// Receive 把对端推来的东西落进 dir。返回落地后的绝对路径。
// 如果对端还发了文本，onText 会被调一次（文本不落盘，交给界面显示）。
func Receive(ctx context.Context, dc *webrtc.DataChannel, dir string, onText func(string), onProgress func(Progress)) ([]string, error) {
	// OnMessage 是回调式的，这里用一条带缓冲的通道把它变成"能 select 的流"。
	// 缓冲给得比较大：接收慢于发送时，靠它顶住，再靠 SCTP 的流控反压。
	ch := make(chan frame, 256)
	dc.OnMessage(func(m webrtc.DataChannelMessage) {
		select {
		case ch <- frame{data: m.Data, text: m.IsString}:
		case <-ctx.Done():
		}
	})

	var (
		items    []Item
		outs     []*os.File
		written  []int64
		final    []string
		total    int64
		sent     int64
		prepared bool
	)

	closeAll := func() {
		for _, f := range outs {
			if f != nil {
				f.Close()
			}
		}
	}
	defer closeAll()

	for {
		var fr frame
		select {
		case <-ctx.Done():
			return final, ctx.Err()
		case fr = <-ch:
		}

		if fr.text {
			var head struct {
				Type  string `json:"type"`
				Items []Item `json:"items"`
				Total int64  `json:"total"`
				Text  string `json:"text"`
			}
			if err := json.Unmarshal(fr.data, &head); err != nil {
				return final, fmt.Errorf("收到看不懂的控制帧：%w", err)
			}
			switch head.Type {
			case "text":
				if onText != nil && head.Text != "" {
					onText(head.Text)
				}
			case "manifest":
				items = head.Items
				total = 0
				for _, it := range items {
					total += it.Size
				}
				if err := prepare(dir, items, &outs, &final); err != nil {
					return final, err
				}
				written = make([]int64, len(items))
				prepared = true
				if onProgress != nil {
					onProgress(Progress{Total: total, Files: len(items)})
				}
			case "done":
				if onProgress != nil {
					onProgress(Progress{Done: sent, Total: total, Index: len(items), Files: len(items)})
				}
				return final, nil
			default:
				return final, fmt.Errorf("意外的控制帧：%s", head.Type)
			}
			continue
		}

		if !prepared {
			return final, fmt.Errorf("还没收到文件清单就先来了数据")
		}
		if len(fr.data) < 5 {
			return final, fmt.Errorf("数据块太短：%d 字节", len(fr.data))
		}
		idx := int(binary.LittleEndian.Uint32(fr.data[:4]))
		if idx < 0 || idx >= len(outs) || outs[idx] == nil {
			return final, fmt.Errorf("数据块指向了不存在的文件 #%d", idx)
		}
		if _, err := outs[idx].Write(fr.data[4:]); err != nil {
			return final, fmt.Errorf("写 %s 失败：%w", final[idx], err)
		}
		written[idx] += int64(len(fr.data) - 4)
		sent += int64(len(fr.data) - 4)
		if onProgress != nil {
			onProgress(Progress{
				Done:  sent,
				Total: total,
				File:  items[idx].Path,
				Index: idx + 1,
				Files: len(items),
			})
		}
	}
}

// prepare 按清单在目标目录建好文件夹、开好文件句柄。
// 路径全部经过清洗：对端再怎么构造名字，也跳不出 dir 这一层。
func prepare(dir string, items []Item, outs *[]*os.File, final *[]string) error {
	if len(items) == 0 {
		// 只发文本时清单是空的：这回没什么可建，不是错误。
		return nil
	}
	root := filepath.Clean(dir)
	for _, it := range items {
		rel, ok := cleanRel(it.Path)
		if !ok {
			return fmt.Errorf("清单里有不安全的名字：%q", it.Path)
		}
		dst := filepath.Join(root, filepath.FromSlash(rel))
		// 二次确认：拼出来的路径必须还在 root 里面。
		if !strings.HasPrefix(filepath.Clean(dst)+string(os.PathSeparator), root+string(os.PathSeparator)) &&
			filepath.Clean(dst) != root {
			return fmt.Errorf("清单里的名字越界了：%q", it.Path)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("建目录失败：%w", err)
		}
		dst = uniqueName(dst)
		f, err := os.Create(dst)
		if err != nil {
			return fmt.Errorf("建文件失败：%w", err)
		}
		*outs = append(*outs, f)
		*final = append(*final, dst)
	}
	return nil
}

// cleanRel 把对端给的名字收成一个安全的相对路径：拒绝绝对路径、盘符、.. 和非法字符。
func cleanRel(p string) (string, bool) {
	p = strings.ReplaceAll(p, "\\", "/")
	parts := strings.Split(p, "/")
	out := make([]string, 0, len(parts))
	for _, s := range parts {
		if s == "" || s == "." {
			continue
		}
		if s == ".." {
			return "", false
		}
		// 盘符、通配符这些在 Windows 上会出怪事，直接换掉。
		s = strings.Map(func(r rune) rune {
			switch r {
			case ':', '*', '?', '"', '<', '>', '|':
				return '_'
			}
			return r
		}, s)
		if s == "" {
			continue
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return "", false
	}
	return strings.Join(out, "/"), true
}

// uniqueName 不覆盖对方已有的文件：重名就往后加 (1)、(2)。
func uniqueName(p string) string {
	if _, err := os.Lstat(p); os.IsNotExist(err) {
		return p
	}
	ext := filepath.Ext(p)
	base := strings.TrimSuffix(p, ext)
	for i := 1; i < 1000; i++ {
		cand := fmt.Sprintf("%s (%d)%s", base, i, ext)
		if _, err := os.Lstat(cand); os.IsNotExist(err) {
			return cand
		}
	}
	return p
}
