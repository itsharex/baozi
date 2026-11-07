// Package share 是"局域网把文件递给另一台设备"的最小实现。
//
// 与 dashbeam 的差别（dashbeam 用 iroh：QUIC + 打洞 + 公网 relay）：
// 这里只做局域网直连 —— 随机端口上的 HTTP 服务 + 一次性访问码 + 有效期，
// 对方用浏览器或任意 HTTP 客户端就能下载，**只能下载，不能上传**（没有任何写入通道），
// 不需要装客户端。链接到点自动失效：分享是临时通道，不做常驻。
package share

import (
	"archive/zip"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 访问码规则：
//   - 固定 4 位，要能从窗口/二维码手工敲进手机，太长没法输
//   - 只从"看着不会认错"的字符里取：排掉 0 O o、1 I l i、5 S、2 Z、8 B
//   - 校验区分大小写（用户要求：大小写算不同的码，别把 aB 和 Ab 当成同一个）
//
// 代价要说清楚：4 位 × 26 个有效字符 ≈ 18.8 bit，比原来的 16 字节令牌弱得多，
// 所以配了 maxTokenFails —— 连续输错这么多次就直接停掉共享，避免被在线枚举。
const (
	tokenLen      = 4
	tokenAlphabet = "34679" + "ACDEFGHJKLMNPQRTUVWXY" + "acdefghjkmnpqrtuvwxy"
	maxTokenFails = 20
)

// newToken 用 crypto/rand + 拒绝采样生成访问码（拒绝采样是为了避免取模偏差）。
func newToken() (string, error) {
	n := len(tokenAlphabet)
	limit := 256 - 256%n
	out := make([]byte, tokenLen)
	var buf [1]byte
	for i := 0; i < tokenLen; i++ {
		for {
			if _, err := rand.Read(buf[:]); err != nil {
				return "", err
			}
			if int(buf[0]) < limit {
				out[i] = tokenAlphabet[int(buf[0])%n]
				break
			}
		}
	}
	return string(out), nil
}

// Item 是一项被共享的内容。
type Item struct {
	Path string
	Name string
	Size int64
	Dir  bool
}

// DefaultTTL 是分享链接的默认有效期。链接是一次性的临时通道：
// 到点自动失效，不做"只要不停止就一直分享"。
const DefaultTTL = time.Hour

// Session 是一次共享会话：一个监听端口 + 一组条目。
//
// 只发不收：这个服务只把本机选中的文件流给对方，没有任何写入通道。
// （早先版本带过"对方上传到收件箱"的表单，等于把自己的磁盘对局域网开了个门，
// 用户不需要这个方向，已经整条拆掉：没有 /u 路由、没有 handleUpload、没有 inbox 目录。）
type Session struct {
	token string
	port  int
	bind  string // 绑定的网卡 IPv4；空 = 所有网卡

	// deadline 是这条链接的失效时刻；到点自动 Close，之后连都连不上。
	deadline time.Time
	expired  atomic.Bool
	timer    *time.Timer

	mu       sync.RWMutex
	items    []Item
	fails    int  // 连续输错次数
	breached bool // 输错太多，已主动停止

	srv *http.Server
	ln  net.Listener
}

// Start 在随机端口上开始共享。bind 是要绑定的网卡 IPv4（空或 "0.0.0.0" 表示所有网卡），
// ttl 是链接有效期（<=0 用 DefaultTTL）。
func Start(bind string, ttl time.Duration) (*Session, error) {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	tok, err := newToken()
	if err != nil {
		return nil, err
	}
	host := strings.TrimSpace(bind)
	if host == "0.0.0.0" || host == "::" {
		host = ""
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		return nil, err
	}
	s := &Session{
		token:    tok,
		port:     ln.Addr().(*net.TCPAddr).Port,
		bind:     host,
		deadline: time.Now().Add(ttl),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/s/", s.handleIndex)
	mux.HandleFunc("/s", s.handleIndex)
	s.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	s.ln = ln
	// 到点自己关掉监听：链接立刻失效，不需要用户手动停止。
	s.timer = time.AfterFunc(ttl, s.closeExpired)
	go func() { _ = s.srv.Serve(ln) }()
	return s, nil
}

// closeExpired 是到期回调：标记失效并关服务。
func (s *Session) closeExpired() {
	s.expired.Store(true)
	if s.srv != nil {
		_ = s.srv.Close()
	}
}

// ExpiresAt 返回链接失效的时刻。
func (s *Session) ExpiresAt() time.Time { return s.deadline }

// Expired 报告链接是否已经到期。
func (s *Session) Expired() bool {
	return s.expired.Load() || time.Now().After(s.deadline)
}

// Remaining 返回还能用多久（已到期就是 0）。
func (s *Session) Remaining() time.Duration {
	if s.Expired() {
		return 0
	}
	left := time.Until(s.deadline)
	if left < 0 {
		return 0
	}
	return left
}

// URL 返回给对方的访问地址：绑了哪块网卡就发哪个 IP，否则自动挑一个能用的。
// 端口是随机的，必须显式拼上，否则链接会落到 80 端口打不开。
func (s *Session) URL() string {
	return fmt.Sprintf("http://%s/s/%s/", net.JoinHostPort(s.host(), strconv.Itoa(s.port)), s.token)
}

// Bind 返回绑定的网卡 IPv4（空 = 所有网卡）。
func (s *Session) Bind() string { return s.bind }

func (s *Session) host() string {
	if s.bind != "" {
		return s.bind
	}
	return LANIP()
}

// Token 返回 4 位访问码，供界面单独大字展示。
func (s *Session) Token() string { return s.token }

// Breached 报告是否因为连续输错访问码而主动停止了共享。
func (s *Session) Breached() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.breached
}

// Inbox 已移除：这个服务不收文件，只发。

// Stop 关闭监听。
func (s *Session) Stop() error {
	if s.timer != nil {
		s.timer.Stop()
	}
	if s.srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// 已经因为输错/到期被 Close 过的服务再 Shutdown 会返回 ErrServerClosed，不算错。
	if err := s.srv.Shutdown(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// ItemOf 把路径整理成 Item（不存在或不可读的跳过）。目录的体积是递归统计的。
func ItemOf(paths ...string) []Item {
	out := make([]Item, 0, len(paths))
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			continue
		}
		it := Item{Path: p, Name: filepath.Base(p), Size: st.Size(), Dir: st.IsDir()}
		if it.Dir {
			it.Size = dirSize(p)
		}
		out = append(out, it)
	}
	return out
}

// Add 把文件或目录加入共享列表（同名去重）。
func (s *Session) Add(paths ...string) []Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	added := make([]Item, 0, len(paths))
	for _, it := range ItemOf(paths...) {
		dup := false
		for i := range s.items {
			if strings.EqualFold(s.items[i].Path, it.Path) {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		s.items = append(s.items, it)
		added = append(added, it)
	}
	return added
}

// Remove 按路径移除一项。
func (s *Session) Remove(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.items[:0]
	for _, it := range s.items {
		if it.Path != path {
			kept = append(kept, it)
		}
	}
	s.items = kept
}

// Items 返回当前共享列表。
func (s *Session) Items() []Item {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Item, len(s.items))
	copy(out, s.items)
	return out
}

func (s *Session) handleIndex(w http.ResponseWriter, r *http.Request) {
	if s.Expired() {
		// 到期后监听已经关了，这里是给"刚好卡在边界上"的请求一个明白话。
		http.Error(w, "这个分享链接已经到期失效了，让对方重新分享一次。", http.StatusGone)
		return
	}
	// 路径形如 /s/<token>/ 或 /s/<token>/d/<i> 或 /s/<token>/u
	trimmed := strings.TrimPrefix(r.URL.Path, "/s/")
	parts := strings.SplitN(trimmed, "/", 2)
	if len(parts) == 0 || !s.checkToken(parts[0]) {
		http.NotFound(w, r)
		return
	}
	rest := ""
	hasSlash := len(parts) > 1
	if hasSlash {
		rest = parts[1]
	}
	switch {
	case !hasSlash:
		// 请求是 /s/<token>（没有结尾斜杠）：跳过去，地址栏规范一些。
		http.Redirect(w, r, "/s/"+s.token+"/", http.StatusMovedPermanently)
	case rest == "" || rest == "/":
		// 只分享了一个东西：打开链接就直接下载，不用再点一次列表。
		items := s.Items()
		if len(items) == 1 {
			http.Redirect(w, r, "/s/"+s.token+"/d/"+url.PathEscape(items[0].Name), http.StatusFound)
			return
		}
		s.renderIndex(w)
	case strings.HasPrefix(rest, "d/"):
		s.handleDownload(w, r, strings.TrimPrefix(rest, "d/"))
	default:
		// 包括旧的 /u 上传入口：共享只发不收，这里一律 404。
		http.NotFound(w, r)
	}
}

func (s *Session) checkToken(tok string) bool {
	// 区分大小写：直接比原始字节。先把长度不同的挡掉（ConstantTimeCompare 长度不等也返回 0，
	// 但提前返回能少做一次比较），再由 noteFailure 负责熔断计数。
	if len(tok) != len(s.token) {
		s.noteFailure()
		return false
	}
	if subtle.ConstantTimeCompare([]byte(tok), []byte(s.token)) == 1 {
		s.mu.Lock()
		s.fails = 0
		s.mu.Unlock()
		return true
	}
	s.noteFailure()
	return false
}

// noteFailure 记录一次输错；超过上限就主动关掉服务（4 位码必须防在线枚举）。
func (s *Session) noteFailure() {
	s.mu.Lock()
	if s.breached {
		s.mu.Unlock()
		return
	}
	s.fails++
	over := s.fails >= maxTokenFails
	if over {
		s.breached = true
	}
	s.mu.Unlock()
	if over {
		// 稍微延迟再关，让这次请求把 404 写回去。
		time.AfterFunc(200*time.Millisecond, func() {
			if s.srv != nil {
				_ = s.srv.Close()
			}
		})
	}
}

func (s *Session) handleDownload(w http.ResponseWriter, r *http.Request, name string) {
	items := s.Items()
	var target *Item
	for i := range items {
		if items[i].Name == name {
			target = &items[i]
			break
		}
	}
	if target == nil {
		http.NotFound(w, r)
		return
	}
	if !target.Dir {
		f, err := os.Open(target.Path)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer f.Close()
		st, _ := f.Stat()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", contentDisposition(target.Name))
		http.ServeContent(w, r, target.Name, st.ModTime(), f)
		return
	}
	// 目录：实时打 zip 流，不落临时文件。
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", contentDisposition(target.Name+".zip"))
	s.zipDir(w, target.Path)
}

func contentDisposition(name string) string {
	return fmt.Sprintf("attachment; filename=%q; filename*=UTF-8''%s",
		asciiFallback(name), urlPathEscape(name))
}

func asciiFallback(name string) string {
	var b strings.Builder
	for _, r := range name {
		if r < 128 && r != '"' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	return b.String()
}

func urlPathEscape(s string) string { return url.PathEscape(s) }

// zipDir 把目录实时打成 zip 流式写回，不产生临时文件。
func (s *Session) zipDir(w io.Writer, root string) {
	zw := zip.NewWriter(w)
	defer zw.Close()
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		hdr, err := zip.FileInfoHeader(info)
		if err != nil {
			return nil
		}
		hdr.Name = filepath.ToSlash(filepath.Join(filepath.Base(root), rel))
		hdr.Method = zip.Deflate
		entry, err := zw.CreateHeader(hdr)
		if err != nil {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return nil
		}
		_, _ = io.Copy(entry, f)
		f.Close()
		return nil
	})
}

func (s *Session) renderIndex(w http.ResponseWriter) {
	items := s.Items()
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	var b strings.Builder
	b.WriteString(`<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Baozi 文件互传</title><style>
:root{color-scheme:light dark}
body{margin:0;padding:32px;font:15px/1.6 -apple-system,"Segoe UI","Microsoft YaHei",sans-serif;
background:linear-gradient(160deg,#f5f7fb,#e9edf5);min-height:100vh}
@media (prefers-color-scheme:dark){body{background:linear-gradient(160deg,#1b1d22,#111316);color:#e6e6e6}}
.card{max-width:640px;margin:0 auto;padding:24px;border-radius:18px;background:rgba(255,255,255,.72);
box-shadow:0 18px 40px rgba(0,0,0,.12);backdrop-filter:blur(14px)}
@media (prefers-color-scheme:dark){.card{background:rgba(40,42,48,.7)}}
h1{font-size:20px;margin:0 0 4px}p.hint{margin:0 0 20px;opacity:.65;font-size:13px}
a.file{display:flex;justify-content:space-between;padding:12px 14px;margin:8px 0;border-radius:12px;
background:rgba(127,127,127,.12);text-decoration:none;color:inherit}
a.file:hover{background:rgba(127,127,127,.2)}
.size{opacity:.6;font-size:18px}
.foot{margin-top:22px;padding-top:16px;border-top:1px solid rgba(127,127,127,.25);opacity:.55;font-size:12px}
</style></head><body><div class="card">
<h1>给你分享的文件</h1><p class="hint">点一下就开始下载 · 关掉 Baozi 或对方停止共享后，这个链接立即失效</p>`)
	if len(items) == 0 {
		b.WriteString(`<p style="opacity:.6">对方还没有选择要分享的文件，稍等片刻再刷新这个页面。</p>`)
	}
	for _, it := range items {
		icon := "📄"
		if it.Dir {
			icon = "📁"
		}
		// 链接写成绝对路径：页面地址少了结尾斜杠时，相对链接 "d/x" 会被浏览器
		// 解析成 /s/d/x（把访问码那一段当成了文件名），点下去就是 404。
		b.WriteString(fmt.Sprintf(`<a class="file" href="/s/%s/d/%s"><span>%s %s</span><span class="size">%s</span></a>`,
			s.token, url.PathEscape(it.Name), icon, html.EscapeString(it.Name), humanSize(it.Size)))
	}
	b.WriteString(`<p class="foot">这个页面只能下载，没有任何上传入口——文件只会从对方那里流向你。</p>
</div></body></html>`)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, b.String())
}

// Binding 是一块可以绑定的网卡（一块网卡可能有多 IP，这里只取第一个 IPv4）。
type Binding struct {
	Name string // 接口名，如 "以太网"、"WLAN"、"VMware Network Adapter VMnet8"
	IP   string
	// Virtual 标记看起来是虚拟网卡（VMware / VirtualBox / Hyper-V / WSL / 容器 / 手机模拟器）。
	Virtual bool
}

// Bindings 列出本机可用于共享的 IPv4，供界面/配置选择。
func Bindings() []Binding {
	var out []Binding
	for _, c := range candidates() {
		out = append(out, Binding{Name: c.iface, IP: c.ip, Virtual: isVirtualIface(c.iface)})
	}
	return out
}

type candidate struct {
	iface string
	ip    string
}

// candidates 收集所有可用的网卡 IPv4（排除回环与 169.254 链路本地）。
func candidates() []candidate {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []candidate
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP.To4()
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			out = append(out, candidate{iface: iface.Name, ip: ip.String()})
			break // 一块网卡只取一个 IPv4
		}
	}
	return out
}

// 虚拟网卡的接口名特征。VMware / MuMu / Hyper-V 这些地址在同一台机器上也会
// 出现在候选列表里，但它们对端的"另一台设备"根本不在，链接发过去必然打不开。
var virtualHints = []string{
	"vmware", "virtualbox", "vbox", "hyper-v", "vethernet", "wsl", "docker",
	"tap-windows", "npcap", "loopback", "bluetooth", "vmnet", "mumu", "virtual",
}

func isVirtualIface(name string) bool {
	lower := strings.ToLower(name)
	for _, h := range virtualHints {
		if strings.Contains(lower, h) {
			return true
		}
	}
	return false
}

func isPrivate(ip string) bool {
	p := net.ParseIP(ip)
	if p == nil {
		return false
	}
	p4 := p.To4()
	if p4 == nil {
		return false
	}
	switch {
	case p4[0] == 10:
		return true
	case p4[0] == 192 && p4[1] == 168:
		return true
	case p4[0] == 172 && p4[1] >= 16 && p4[1] <= 31:
		return true
	}
	return false
}

// LANIP 返回本机在局域网中"最可能是真实线路"的 IPv4：
// 先挑私有地址且非虚拟网卡，再退回任意私有地址，最后退到第一个候选。
func LANIP() string {
	cs := candidates()
	for _, c := range cs {
		if isPrivate(c.ip) && !isVirtualIface(c.iface) {
			return c.ip
		}
	}
	for _, c := range cs {
		if isPrivate(c.ip) {
			return c.ip
		}
	}
	if len(cs) > 0 {
		return cs[0].ip
	}
	return "127.0.0.1"
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

func dirSize(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}
