package update

import (
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/sys/windows/registry"
)

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
func ProxyFunc() func(*http.Request) (*url.URL, error) {
	sys := readSystemProxy()
	return func(req *http.Request) (*url.URL, error) {
		// 先用 Go 自带的那套（它只看环境变量）。
		if u, err := http.ProxyFromEnvironment(req); err != nil || u != nil {
			return u, err
		}
		if sys == "" || bypassed(req.URL.Hostname(), sysBypass) {
			return nil, nil
		}
		// 系统代理写的是 host:port，没有 scheme；Go 要求 http 代理用 http:// 前缀。
		return &url.URL{Scheme: "http", Host: sys}, nil
	}
}

// sysBypass 是 registry 里 ProxyOverride 的值的副本（读一次就固定，
// 一次更新检查期间代理设置不会变）。
var sysBypass string

// readSystemProxy 读出系统代理的 host:port；没开或读不到就返回空。
func readSystemProxy() string {
	sysBypass = ""
	k, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()

	if enable, _, err := k.GetIntegerValue("ProxyEnable"); err != nil || enable == 0 {
		return ""
	}
	server, _, err := k.GetStringValue("ProxyServer")
	if err != nil {
		return ""
	}
	if override, _, err := k.GetStringValue("ProxyOverride"); err == nil {
		sysBypass = override
	}
	return proxyFor(server, "https")
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
