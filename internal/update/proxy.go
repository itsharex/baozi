package update

import (
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"

	"golang.org/x/sys/windows/registry"
)

// useProxy 是"要不要用代理"这个开关（缺省开）。
//
// 开的时候按 ProxyFunc 的顺序找代理（环境变量 → 系统代理 → 直连）；
// 关掉就一律直连 —— 用户把代理软件关了、但注册表里那一条还留着的时候，
// 复选框就是他唯一的出口。
var useProxy atomic.Bool

func init() { useProxy.Store(true) }

// SetUseProxy 改这个开关（"关于"页那个复选框调它）。
func SetUseProxy(on bool) { useProxy.Store(on) }

// UseProxy 报当前开关状态。
func UseProxy() bool { return useProxy.Load() }

// ProxyFunc 是更新检查/下载用的代理规则。顺序是：
//
//  1. 环境变量（HTTP_PROXY / HTTPS_PROXY / NO_PROXY）—— 用户显式设的优先，
//     也方便调试时临时指一个；
//  2. Windows"Internet 选项"里的系统代理（Clash / v2rayN 这类工具就是改这里）；
//  3. 都没有，直连。
//
// 只认 registry 里的 ProxyEnable / ProxyServer / ProxyOverride，不解析 PAC 脚本
// （ProxyAutoConfigURL）。PAC 需要 WinHTTP 的 autoproxy 解析器，对"一个人用一台
// 机器"的场景收益太低；公司里强制 PAC 的用户可以把 HTTPS_PROXY 设上，那条路是通的。
//
// 开关关掉时整段跳过，直接返回 nil（不查环境变量、不读注册表）。
func ProxyFunc() func(*http.Request) (*url.URL, error) {
	return func(req *http.Request) (*url.URL, error) {
		if !useProxy.Load() {
			return nil, nil
		}
		// 先用 Go 自带的那套（它只看环境变量）。
		if u, err := http.ProxyFromEnvironment(req); err != nil || u != nil {
			return u, err
		}
		sys, bypass := readSystemProxy()
		if sys == "" || bypassed(req.URL.Hostname(), bypass) {
			return nil, nil
		}
		// 系统代理写的是 host:port，没有 scheme；Go 要求 http 代理用 http:// 前缀。
		return &url.URL{Scheme: "http", Host: sys}, nil
	}
}

// ProxyAddress 报"现在会用哪个代理"，给"关于"页那个复选框当标签用。
//
// 只返回 host:port（环境变量里写成 http://127.0.0.1:7892 的，也削成 127.0.0.1:7892）：
// 标签只有一行，用户要认的是"哪个地址"，不是协议。找不到代理就返回空。
func ProxyAddress() string {
	if u := proxyFromEnv(); u != "" {
		return hostPort(u)
	}
	sys, _ := readSystemProxy()
	return sys
}

// hostPort 把 "http://127.0.0.1:7892" 削成 "127.0.0.1:7892"；已经没 scheme 就原样。
func hostPort(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	if u.Port() == "" {
		// 没写端口就按协议补一个，不然标签上只有个主机名，看不出是什么。
		if u.Scheme == "https" {
			return net.JoinHostPort(u.Hostname(), "443")
		}
		return net.JoinHostPort(u.Hostname(), "80")
	}
	return u.Host
}

// readSystemProxy 读出系统代理的 host:port 和"绕过"清单；没开或读不到就返回空。
//
// 每次调用都重读注册表：用户刚在 Clash 里开关过代理，读缓存就会显示上一条。
// （一次更新检查里只会调一两次，这点开销无所谓。）
func readSystemProxy() (proxy, bypass string) {
	k, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.QUERY_VALUE)
	if err != nil {
		return "", ""
	}
	defer k.Close()

	if enable, _, err := k.GetIntegerValue("ProxyEnable"); err != nil || enable == 0 {
		return "", ""
	}
	server, _, err := k.GetStringValue("ProxyServer")
	if err != nil {
		return "", ""
	}
	if override, _, err := k.GetStringValue("ProxyOverride"); err == nil {
		bypass = override
	}
	return proxyFor(server, "https"), bypass
}

// proxyFor 从 ProxyServer 里挑出某个协议该用的那一条。
//
// 这个值的格式有两种：老的 "127.0.0.1:7890"，以及分协议的
// "http=127.0.0.1:7890;https=127.0.0.1:7890;ftp=..."。分协议时优先取同名那条，
// 没有就用 http 那条（绝大多数代理软件同一个端口什么都接）。
func proxyFor(server, scheme string) string {
	server = strings.TrimSpace(server)
	if server == "" {
		return ""
	}
	if !strings.Contains(server, "=") {
		return server
	}
	var httpOne string
	for _, part := range strings.Split(server, ";") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(kv[0]))
		addr := strings.TrimSpace(kv[1])
		if name == "http" && addr != "" {
			httpOne = addr
		}
		if name == strings.ToLower(scheme) && addr != "" {
			return addr
		}
	}
	return httpOne
}

// bypassed 判断这个主机名要不要绕过代理（ProxyOverride 的常见写法：
// "*.local;10.*;192.168.*;<local>"）。
//
// <local> 是"不带点的主机名"（内网机器名）；我们访问的是 api.github.com 这类
// 外网域名，命中它的可能性很小，但宁可少走代理也不要让内网地址卡在代理上。
func bypassed(host, override string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" || override == "" {
		return false
	}
	for _, pat := range strings.Split(override, ";") {
		pat = strings.ToLower(strings.TrimSpace(pat))
		if pat == "" {
			continue
		}
		if pat == "<local>" {
			if !strings.Contains(host, ".") {
				return true
			}
			continue
		}
		// "10.*" 这种前缀通配，和 "*.example.com" 这种后缀通配都按前缀/后缀处理。
		if strings.HasPrefix(pat, "*") {
			if strings.HasSuffix(host, strings.TrimPrefix(pat, "*")) {
				return true
			}
			continue
		}
		if strings.HasSuffix(pat, "*") {
			if strings.HasPrefix(host, strings.TrimSuffix(pat, "*")) {
				return true
			}
			continue
		}
		if host == pat {
			return true
		}
	}
	return false
}
