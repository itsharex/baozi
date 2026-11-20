package update

import (
	"net/http"
	"testing"
)

func mustReq(t *testing.T, raw string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestProxyFor(t *testing.T) {
	cases := []struct{ server, scheme, want string }{
		{"127.0.0.1:7890", "https", "127.0.0.1:7890"},                           // 老式单条
		{" 10.0.0.1:8080 ", "https", "10.0.0.1:8080"},                           // 首尾空格
		{"http=127.0.0.1:7890;https=127.0.0.1:7891", "https", "127.0.0.1:7891"}, // 分协议，取 https
		{"http=127.0.0.1:7890;https=127.0.0.1:7891", "http", "127.0.0.1:7890"},
		{"ftp=127.0.0.1:2121;http=127.0.0.1:7890", "https", "127.0.0.1:7890"}, // 没 https 就用 http
		{"", "https", ""},
		{"   ", "https", ""},
	}
	for _, c := range cases {
		if got := proxyFor(c.server, c.scheme); got != c.want {
			t.Errorf("proxyFor(%q,%q)=%q, want %q", c.server, c.scheme, got, c.want)
		}
	}
}

func TestBypassed(t *testing.T) {
	cases := []struct {
		host, override string
		want           bool
	}{
		{"api.github.com", "", false},
		{"api.github.com", "*.local;10.*;<local>", false},
		{"nas.local", "*.local", true},
		{"10.1.2.3", "10.*", true},
		{"127.0.0.1", "127.*", true},
		{"github.com", "github.com", true},
		{"api.github.com", "github.com", false}, // 精确匹配不是后缀匹配
		{"BAOZI-PC", "<local>", true},
		{"baozi.example.com", "<local>", false},
		{"api.github.com", "*.github.com", true},
		{"API.GitHub.com", "*.github.com", true}, // 大小写不敏感
	}
	for _, c := range cases {
		if got := bypassed(c.host, c.override); got != c.want {
			t.Errorf("bypassed(%q,%q)=%v, want %v", c.host, c.override, got, c.want)
		}
	}
}

// TestProxyFuncPrefersEnv 确认环境变量排在最前面：用户显式设了 HTTPS_PROXY，
// 就不该再被系统代理抢走（调试、命令行临时指代理都靠这条）。
func TestProxyFuncPrefersEnv(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:18080")
	req := mustReq(t, "https://api.github.com/repos/l0rraine/baozi/releases/latest")
	fn := ProxyFunc()
	u, err := fn(req)
	if err != nil {
		t.Fatalf("ProxyFunc: %v", err)
	}
	if u == nil || u.Host != "127.0.0.1:18080" {
		t.Fatalf("环境变量代理没生效，拿到 %v", u)
	}
}

// TestProxyFuncNoEnv 在没有环境变量时，返回的要么是系统代理、要么直连，
// 但绝不能报错、也不能返回一个 host 为空的 URL —— 这条保证"没配代理也能查更新"。
func TestProxyFuncNoEnv(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("ALL_PROXY", "")
	req := mustReq(t, "https://api.github.com/repos/l0rraine/baozi/releases/latest")
	u, err := ProxyFunc()(req)
	if err != nil {
		t.Fatalf("ProxyFunc: %v", err)
	}
	if u != nil && u.Host == "" {
		t.Fatalf("拿到一个空代理地址：%v", u)
	}
}
