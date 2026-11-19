package update

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		in   string
		want [3]int
		ok   bool
	}{
		{"v0.3.0", [3]int{0, 3, 0}, true},
		{"V1.2.3", [3]int{1, 2, 3}, true},
		{"1.2.3-beta.1", [3]int{1, 2, 3}, true},
		{"v1.2", [3]int{1, 2, 0}, true},
		{"v0.10.0", [3]int{0, 10, 0}, true},
		{"dev", [3]int{}, false},
		{"", [3]int{}, false},
		{"v1.2.x", [3]int{}, false},
		{"v1..2", [3]int{}, false},
	}
	for _, c := range cases {
		got, ok := parse(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("parse(%q) = %v,%v；想要 %v,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestNewer(t *testing.T) {
	cases := []struct {
		cur, latest string
		want        bool
	}{
		{"v0.3.0", "v0.3.0", false},
		{"v0.3.0", "v0.3.1", true},
		{"v0.3.0", "v0.4.0", true},
		{"v0.3.0", "v0.2.9", false},
		{"v0.9.0", "v0.10.0", true}, // 字符串比较会在这栽跟头（"0.9" > "0.10"）
		{"dev", "v9.0.0", false},    // 开发版不跟线上比
		{"", "v9.0.0", false},
		{"v0.3.0", "dev", false}, // 线上说不出版本号就当没有
		{"v0.3.0", "乱写", false},
	}
	for _, c := range cases {
		if got := Newer(c.cur, c.latest); got != c.want {
			t.Errorf("Newer(%q, %q) = %v；想要 %v", c.cur, c.latest, got, c.want)
		}
	}
}

func TestLatest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" {
			t.Errorf("请求没带 User-Agent（GitHub 会直接 403）")
		}
		json.NewEncoder(w).Encode(map[string]any{
			"tag_name":     "v9.9.9",
			"html_url":     "https://example.com/releases/v9.9.9",
			"published_at": "2026-11-20T00:00:00Z",
			"assets": []map[string]string{
				{"name": "notes.txt", "browser_download_url": "https://example.com/x.txt"},
				{"name": "baozi-v9.9.9-windows-amd64.zip", "browser_download_url": "https://example.com/x.zip"},
			},
		})
	}))
	defer srv.Close()

	old := APIURL
	APIURL = srv.URL
	defer func() { APIURL = old }()

	rel, err := Latest(context.Background())
	if err != nil {
		t.Fatalf("Latest 失败：%v", err)
	}
	if rel.Tag != "v9.9.9" {
		t.Errorf("Tag = %q", rel.Tag)
	}
	if rel.PageURL != "https://example.com/releases/v9.9.9" {
		t.Errorf("PageURL = %q", rel.PageURL)
	}
	// 只认 windows 的 zip：那张 txt 不能被误当成安装包。
	if rel.ZipURL != "https://example.com/x.zip" {
		t.Errorf("ZipURL = %q", rel.ZipURL)
	}
}

func TestLatestHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"API rate limit exceeded"}`))
	}))
	defer srv.Close()

	old := APIURL
	APIURL = srv.URL
	defer func() { APIURL = old }()

	if _, err := Latest(context.Background()); err == nil {
		t.Fatal("403 应该返回错误")
	}
}
