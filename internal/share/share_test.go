package share

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// 断点续传：下载走 http.ServeContent，文件请求带 Range 必须回 206 + Content-Range。
//
// 这条不是"照着 fsend 抄的"，而是确认我们现在就有：视频/大文件下到一半断网，
// 浏览器自己会从断点继续要（Range 请求），服务端只要照规矩回 206 就行。
// 目录是边压边写的 zip 流，天生没法 seek，续传不了 —— 这一条也写在这里，
// 免得以后有人以为"文件夹也能续"。
func TestDownloadSupportsRangeResume(t *testing.T) {
	dir := t.TempDir()
	// 12 字节，取中间一段最容易看出偏移对不对。
	src := filepath.Join(dir, "chunk.bin")
	const content = "0123456789ab"
	if err := os.WriteFile(src, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Start("", 0)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()
	// 只加一项：首页会 302 直接跳到下载，Range 请求打到的是同一个处理函数。
	s.Add(src)
	client := &http.Client{
		Transport:     &http.Transport{Proxy: nil},
		Timeout:       5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	dlURL := s.URL() + "d/chunk.bin"

	req, _ := http.NewRequest("GET", dlURL, nil)
	req.Header.Set("Range", "bytes=4-7")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET 带 Range: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("带 Range 的下载应回 206，实际 %d", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if string(got) != content[4:8] {
		t.Fatalf("续传片段不对：拿到 %q，期望 %q", got, content[4:8])
	}
	if cr := resp.Header.Get("Content-Range"); cr != "bytes 4-7/12" {
		t.Fatalf("Content-Range = %q，期望 bytes 4-7/12", cr)
	}

	// 不带 Range 时服务端要声明支持（浏览器据此判断"能不能续"）。
	plain, err := client.Do(func() *http.Request {
		r, _ := http.NewRequest("GET", dlURL, nil)
		return r
	}())
	if err != nil {
		t.Fatalf("GET 不带 Range: %v", err)
	}
	defer plain.Body.Close()
	if ar := plain.Header.Get("Accept-Ranges"); ar != "bytes" {
		t.Fatalf("Accept-Ranges = %q，期望 bytes", ar)
	}
}

// 这是一条真实往返：起服务 → 加文件 → 首页 → 下载 → 校验。
//
// 顺带把"只发不收"钉住：早先首页底部有个上传表单（对方可以把文件塞进本机收件箱），
// 用户明确不要这个方向，所以 /u 与任何 multipart 提交都必须被拒。
func TestShareRoundTrip(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "hello.txt")
	if err := os.WriteFile(src, []byte("baozi says hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	extra := filepath.Join(dir, "another.txt")
	if err := os.WriteFile(extra, []byte("second"), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Start("", 0)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()
	// 两项起才渲染列表页；只一项时打开链接直接开始下载（见 TestSingleItemRedirectsToDownload）。
	s.Add(src, extra)

	// 直接用界面会展示的那个链接，端口/IP 一起验；局域网地址要绕过系统代理。
	base := s.URL()
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 5 * time.Second}
	page, err := client.Get(base)
	if err != nil {
		t.Fatalf("GET 首页: %v", err)
	}
	body, _ := io.ReadAll(page.Body)
	page.Body.Close()
	if page.StatusCode != 200 || !strings.Contains(string(body), "hello.txt") {
		t.Fatalf("首页异常: status=%d body=%s", page.StatusCode, body)
	}
	if strings.Contains(string(body), `type="file"`) || strings.Contains(string(body), `action="u"`) ||
		strings.Contains(string(body), "把文件发给这台电脑") {
		t.Fatalf("首页还带着上传入口，必须去掉：%s", body)
	}

	dl, err := http.Get(base + "d/hello.txt")
	if err != nil {
		t.Fatalf("GET 下载: %v", err)
	}
	got, _ := io.ReadAll(dl.Body)
	dl.Body.Close()
	if string(got) != "baozi says hi" {
		t.Fatalf("下载内容不对: %q", got)
	}

	bad, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/s/wrong-token/", s.port))
	if err != nil {
		t.Fatalf("GET 错误令牌: %v", err)
	}
	bad.Body.Close()
	if bad.StatusCode != 404 {
		t.Fatalf("错误令牌应 404，实际 %d", bad.StatusCode)
	}

	// 大小写算不同的码：同一个地址只改大小写也必须 404。
	// 访问码全由数字组成时翻转大小写不变，这种情况没有可测的变体，跳过。
	if flipped := flipCase(s.Token()); flipped != s.Token() {
		caseBad, err := client.Get(strings.Replace(base, s.Token(), flipped, 1))
		if err != nil {
			t.Fatalf("GET 大小写变体: %v", err)
		}
		caseBad.Body.Close()
		if caseBad.StatusCode != 404 {
			t.Fatalf("只改大小写应 404（%q -> %q），实际 %d", s.Token(), flipped, caseBad.StatusCode)
		}
	}

	// 旧的 /u 上传入口必须已经不存在，且提交文件不能在本机落盘。
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, _ := mw.CreateFormFile("f", "from-phone.txt")
	_, _ = io.WriteString(part, "uploaded")
	mw.Close()
	req, _ := http.NewRequest("POST", base+"u", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /u: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("/u 应已移除（期望 404），实际 %d", resp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(dir, "from-phone.txt")); err == nil {
		t.Fatal("文件被写进了本机，上传通道没关干净")
	}
}

// 页面上的下载链接必须是绝对路径。早先写的是相对 "d/x"：用户把地址栏里的
// 结尾斜杠丢掉（/s/ABCD）再打开页面时，浏览器会把链接解析成 /s/d/x，
// 把访问码那一段当成文件名，点下去就是 404。这里把两种地址写法都钉住，
// 同时验证目录是实时打的 zip。
func TestDownloadLinksWork(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "中文名.txt"), []byte("cn"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "一个文件夹")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "inside.txt"), []byte("inside"), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Start("", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	s.Add(filepath.Join(dir, "hello.txt"), filepath.Join(dir, "中文名.txt"), sub)

	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 5 * time.Second}
	base := s.URL()
	hrefRe := regexp.MustCompile(`href="([^"]+)"`)

	for _, pageURL := range []string{base, strings.TrimSuffix(base, "/")} {
		resp, err := client.Get(pageURL)
		if err != nil {
			t.Fatalf("GET %s: %v", pageURL, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s -> %d", pageURL, resp.StatusCode)
		}
		page, _ := url.Parse(pageURL)
		links := hrefRe.FindAllStringSubmatch(string(body), -1)
		if len(links) != 3 {
			t.Fatalf("%s 页面里的链接数 = %d，期望 3", pageURL, len(links))
		}
		for _, m := range links {
			href := m[1]
			// 链接必须是带访问码的绝对路径，不能是相对 "d/x"。
			if !strings.HasPrefix(href, "/s/"+s.Token()+"/d/") {
				t.Fatalf("下载链接不是绝对路径：%q", href)
			}
			ref, _ := url.Parse(href)
			abs := page.ResolveReference(ref).String()
			dl, err := client.Get(abs)
			if err != nil {
				t.Fatalf("GET %s: %v", abs, err)
			}
			dl.Body.Close()
			if dl.StatusCode != 200 {
				t.Fatalf("%s 下载失败：%d", abs, dl.StatusCode)
			}
		}
	}

	// 目录：Content-Type 是 zip，文件名带 .zip，内容是里面那个文件。
	dl, err := client.Get(base + "d/" + url.PathEscape("一个文件夹"))
	if err != nil {
		t.Fatal(err)
	}
	defer dl.Body.Close()
	if ct := dl.Header.Get("Content-Type"); ct != "application/zip" {
		t.Fatalf("目录下载的 Content-Type = %q，期望 application/zip", ct)
	}
	if cd := dl.Header.Get("Content-Disposition"); !strings.Contains(cd, ".zip") {
		t.Fatalf("目录下载的文件名应带 .zip：%q", cd)
	}
	zbody, _ := io.ReadAll(dl.Body)
	zr, err := zip.NewReader(bytes.NewReader(zbody), int64(len(zbody)))
	if err != nil {
		t.Fatalf("不是合法的 zip：%v", err)
	}
	names := make([]string, 0, len(zr.File))
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	if len(names) != 1 || !strings.HasSuffix(names[0], "inside.txt") {
		t.Fatalf("zip 内容不对：%v", names)
	}
}

// 链接是一次性的：到点自动失效，之后连都连不上。
func TestShareExpires(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")
	for _, f := range []string{a, b} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s, err := Start("", 200*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	s.Add(a, b)

	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 2 * time.Second}
	resp, err := client.Get(s.URL())
	if err != nil {
		t.Fatalf("到期前应能访问: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("到期前 status=%d", resp.StatusCode)
	}

	time.Sleep(600 * time.Millisecond)
	if !s.Expired() {
		t.Fatal("过了有效期应当已标记为失效")
	}
	if left := s.Remaining(); left != 0 {
		t.Fatalf("到期后剩余时间应为 0，实际 %v", left)
	}
	if _, err := client.Get(s.URL()); err == nil {
		t.Fatal("到期后端口应当已关闭，链接不该还能打开")
	}
}

// 不传有效期时按默认 1 小时。
func TestShareDefaultTTL(t *testing.T) {
	s, err := Start("", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	if left := s.Remaining(); left < DefaultTTL-time.Minute || left > DefaultTTL {
		t.Fatalf("默认有效期应接近 %v，实际还剩 %v", DefaultTTL, left)
	}
}

// 只分享一项时，打开链接就直接开始下载，不用再点一次列表；多项才渲染列表页。
func TestSingleItemRedirectsToDownload(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "只有这个.txt")
	if err := os.WriteFile(f, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Start("", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	s.Add(f)

	client := &http.Client{
		Transport: &http.Transport{Proxy: nil},
		Timeout:   5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // 不跟随，看第一跳
		},
	}
	resp, err := client.Get(s.URL())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("单项时根路径应重定向到下载，实际 %d", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/s/"+s.Token()+"/d/") {
		t.Fatalf("重定向目标不对：%q", loc)
	}

	// 再放一项：这时才渲染列表页。
	other := filepath.Join(dir, "另一样.txt")
	if err := os.WriteFile(other, []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.Add(other)
	client2 := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 5 * time.Second}
	resp2, err := client2.Get(s.URL())
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("多项时应当渲染列表页，实际 %d", resp2.StatusCode)
	}
}

func TestTokenRequired(t *testing.T) {
	s, err := Start("", 0)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()
	if s.URL() == "" {
		t.Fatal("URL 不应为空")
	}
	if s.checkToken("") || s.checkToken("x") {
		t.Fatal("空/错误令牌不应通过")
	}
	// 区分大小写：换个大小写就是别的码，必须拒绝
	tok := s.Token()
	if flipped := flipCase(tok); flipped != tok && s.checkToken(flipped) {
		t.Fatalf("大小写不同不该通过：%q 被当成 %q", flipped, tok)
	}
	if !s.checkToken(tok) {
		t.Fatalf("原样访问码应当通过：%q", tok)
	}
}

// flipCase 逐字翻转大小写，用来构造"只差大小写"的错误码。
// 访问码里只有大小写字母与数字，数字翻转后不变，所以至少要有一个字母才不等。
func flipCase(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r - 32)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + 32)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// 访问码是手工敲进手机的：必须短、且不含看着会认错的字符。
func TestTokenShape(t *testing.T) {
	s, err := Start("", 0)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	tok := s.Token()
	if len([]rune(tok)) != tokenLen {
		t.Fatalf("访问码长度 = %d，期望 %d：%q", len([]rune(tok)), tokenLen, tok)
	}
	for _, r := range tok {
		if !strings.ContainsRune(tokenAlphabet, r) {
			t.Fatalf("字符 %q 不在允许的字符集里", r)
		}
		for _, bad := range "01lIoO5S2Z8B" {
			if r == bad {
				t.Fatalf("访问码里出现了易混淆字符 %q：%q", r, tok)
			}
		}
	}
	// 两次生成不该相同（4 位空间不大，但连着相同说明随机源有问题）
	other, err := Start("", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Stop()
	if other.Token() == tok {
		t.Logf("两次访问码相同（概率约 1/45万，记录但不判失败）")
	}
}

// 连续输错到上限就必须停服，否则 4 位码会被在线枚举。
func TestTokenFailsClosed(t *testing.T) {
	s, err := Start("", 0)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	for i := 0; i < maxTokenFails; i++ {
		if s.checkToken("zzzz") {
			t.Fatal("错误访问码不该通过")
		}
	}
	if !s.Breached() {
		t.Fatalf("输错 %d 次后应当标记为已停止", maxTokenFails)
	}
	time.Sleep(400 * time.Millisecond)
	if _, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/s/%s/", s.port, s.token)); err != nil {
		// 端口已关，符合预期
		return
	}
	// 如果还能连上，说明没真正停服
	t.Fatal("输错上限后端口应当已关闭")
}

// 绑到某块网卡后，URL 必须用那块网卡的 IP，且从该 IP 真能访问。
func TestStartBindSpecific(t *testing.T) {
	ip := LANIP()
	if ip == "127.0.0.1" {
		t.Skip("本机没有可用的局域网网卡")
	}
	s, err := Start(ip, 0)
	if err != nil {
		t.Fatalf("Start(%s): %v", ip, err)
	}
	defer s.Stop()

	if s.Bind() != ip {
		t.Fatalf("Bind() = %q，期望 %q", s.Bind(), ip)
	}
	url := s.URL()
	if !strings.Contains(url, ip) {
		t.Fatalf("URL 没用绑定网卡的 IP：%s", url)
	}
	// 局域网地址必须绕过系统代理（否则代理会回 502）。
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("按绑定 IP 访问失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}

// 默认（bind 为空）应该是所有网卡，且 URL 自动挑一个私有地址。
func TestStartDefaultAllNICs(t *testing.T) {
	s, err := Start("", 0)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()
	if s.Bind() != "" {
		t.Fatalf("默认应绑全部网卡，实际 %q", s.Bind())
	}
	if got := s.URL(); !strings.Contains(got, LANIP()) {
		t.Fatalf("URL 的 IP 不是自动挑选的那个：%s（LANIP=%s）", got, LANIP())
	}
}

// 局域网也能递文本：页面里直接摊开 + 复制按钮，而不是让人去下一个 txt。
func TestLocalShareShowsText(t *testing.T) {
	s, err := Start("", 0)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	// 正文里带 HTML 尖括号和换行：必须转义，且换行不能丢。
	const text = "第一行 <&> 尖括号\n第二行 curl -fsSL https://example.com/x.sh | sh"
	s.SetText(text)
	if got := s.Text(); got != text {
		t.Fatalf("Text() 回读不一致: %q", got)
	}

	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 5 * time.Second}
	resp, err := client.Get(s.URL())
	if err != nil {
		t.Fatalf("GET 首页: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	html := string(body)

	if resp.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", resp.StatusCode, html)
	}
	if strings.Contains(html, "对方还没有选择要分享的文件") {
		t.Fatal("只发文本时不该提示“还没有选择文件”")
	}
	if !strings.Contains(html, `<button id="copytext">复制</button>`) {
		t.Fatalf("页面缺少复制按钮:\n%s", html)
	}
	if !strings.Contains(html, "&lt;&amp;&gt;") {
		t.Fatalf("文本里的尖括号没被转义:\n%s", html)
	}
	if strings.Contains(html, "<&>") {
		t.Fatalf("未转义的原始尖括号泄漏进 HTML:\n%s", html)
	}
	if !strings.Contains(html, "第一行") || !strings.Contains(html, "第二行") {
		t.Fatalf("文本内容没进页面:\n%s", html)
	}
}
