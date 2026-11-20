package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Client 是所有更新相关请求共用的 HTTP 客户端：代理按 ProxyFunc 解析，
// 拨号/握手/等响应各有一段超时。
//
// 共用一个 client 是为了复用连接池 —— 检查更新和随后下载 zip 打的是同一台主机，
// 第二次请求就不用再握手一次。
var sharedClient = &http.Client{
	Transport: &http.Transport{
		Proxy: ProxyFunc(),
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
		ForceAttemptHTTP2:     true,
	},
}

// Client 返回这个共用客户端。
func Client() *http.Client { return sharedClient }

// Download 把 url 的内容下载到 dst。先写 .part 再改名：
// 中途断了不会留下一个"看起来完整"的半个 zip。
func Download(ctx context.Context, rawurl, dst string) (int64, error) {
	if rawurl == "" {
		return 0, errors.New("没有下载地址")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawurl, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", "baozi-updater")
	req.Header.Set("Accept", "application/octet-stream")

	resp, err := sharedClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("下载失败：HTTP %d", resp.StatusCode)
	}

	part := dst + ".part"
	out, err := os.Create(part)
	if err != nil {
		return 0, err
	}
	n, copyErr := io.Copy(out, resp.Body)
	closeErr := out.Close()
	if copyErr != nil {
		os.Remove(part)
		return n, copyErr
	}
	if closeErr != nil {
		os.Remove(part)
		return n, closeErr
	}
	if err := os.Rename(part, dst); err != nil {
		os.Remove(part)
		return n, err
	}
	return n, nil
}

// ProxySummary 是给日志/诊断看的一句话：现在实际走的是哪条路。
// （"关于"页不再显示它 —— 那一页只摆一个"使用代理 地址"的复选框。）
func ProxySummary() string {
	if !UseProxy() {
		return "直连（已关掉代理）"
	}
	if u := proxyFromEnv(); u != "" {
		return "环境变量：" + u
	}
	if sys, _ := readSystemProxy(); sys != "" {
		return "系统代理：" + sys
	}
	return "直连"
}

// proxyFromEnv 把环境变量里的代理地址抠出来（只为了显示）。
func proxyFromEnv() string {
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// StageDir 建一个本次更新专用的临时目录（zip、解出来的 exe、更新脚本都放这里）。
// 用进程 id 命名：连点两次"下载并更新"也不会互相踩到对方的文件。
func StageDir() (string, error) {
	base := os.TempDir()
	dir := filepath.Join(base, fmt.Sprintf("baozi-update-%d", os.Getpid()))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}
