// Package update 查 GitHub Releases 上有没有新版本。
//
// 只做"查"这一件事：不下载、不替换文件。更新这种事一旦会动用户的安装目录，
// 就得先问人 —— 所以这里的职责边界是"发现新版本 + 给出下载页"，
// 具体怎么换由用户点一下决定（见 cmd/baozi 的关于页）。
package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// Repo 是这个项目在 GitHub 上的位置。
	Repo = "l0rraine/baozi"
	// RepoPage 是项目主页。
	RepoPage = "https://github.com/" + Repo
	// ReleasePage 是最新版本的下载页。
	ReleasePage = "https://github.com/" + Repo + "/releases/latest"
)

// APIURL 指向"最新 release"。做成变量是为了测试里能换成本地 httptest 服务。
var APIURL = "https://api.github.com/repos/" + Repo + "/releases/latest"

// Release 是"最新一版"里我们用得上的那几个字段。
type Release struct {
	Tag       string    // 形如 v0.3.0
	PageURL   string    // 这个 release 的网页地址
	ZipURL    string    // windows-amd64 那个 zip 的直链（有就带上）
	Published time.Time // 发布时间
}

// Latest 拉一次最新 release。超时交给我们自己的 ctx，不在包里另设死线。
//
// 这里**故意**不重试、不带轮询：调用方本来就只在启动后跑一次（外加用户手动点一次），
// 失败就失败，界面写"检查失败"即可。自动更新最忌讳的就是在后台反复敲 API
// 把人的限额耗光，然后整个功能一起哑掉。
func Latest(ctx context.Context) (*Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, APIURL, nil)
	if err != nil {
		return nil, err
	}
	// GitHub 对没有 UA 的请求直接 403，这条不是礼貌，是硬要求。
	req.Header.Set("User-Agent", "baozi-update-check")
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return nil, fmt.Errorf("github 返回 %d：%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload struct {
		TagName     string    `json:"tag_name"`
		HTMLURL     string    `json:"html_url"`
		PublishedAt time.Time `json:"published_at"`
		Assets      []struct {
			Name               string `json:"name"`
			BrowserDownloadURL string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return nil, err
	}
	rel := &Release{
		Tag:       strings.TrimSpace(payload.TagName),
		PageURL:   payload.HTMLURL,
		Published: payload.PublishedAt,
	}
	if rel.PageURL == "" {
		rel.PageURL = ReleasePage
	}
	for _, a := range payload.Assets {
		if strings.HasSuffix(strings.ToLower(a.Name), ".zip") &&
			strings.Contains(strings.ToLower(a.Name), "windows") {
			rel.ZipURL = a.BrowserDownloadURL
			break
		}
	}
	if rel.Tag == "" {
		return nil, fmt.Errorf("release 里没有 tag_name")
	}
	return rel, nil
}

// Newer 判断 latest 是否比 current 新。current 是开发版（没有版本号）时返回 false ——
// 本地自己编的版本跟线上比没有意义，只会天天提示"有新版本"。
func Newer(current, latest string) bool {
	if IsDev(current) {
		return false
	}
	c, ok1 := parse(current)
	l, ok2 := parse(latest)
	if !ok1 || !ok2 {
		return false
	}
	for i := 0; i < 3; i++ {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false
}

// IsDev 报这串版本号是不是"本地编译、没有注入版本"。
func IsDev(v string) bool {
	v = strings.TrimSpace(v)
	return v == "" || strings.EqualFold(v, "dev")
}

// parse 把 "v1.2.3" / "1.2.3-beta.1" / "v1.2" 拆成三段数字。
// 预发布后缀直接丢掉：本项目发版只发正式版，带后缀的 tag 只用于自测。
func parse(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(strings.TrimPrefix(v, "v"), "V")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	if v == "" {
		return out, false
	}
	parts := strings.Split(v, ".")
	if len(parts) > 3 {
		parts = parts[:3]
	}
	for i, p := range parts {
		if p == "" {
			return out, false
		}
		n := 0
		for _, r := range p {
			if r < '0' || r > '9' {
				return out, false
			}
			n = n*10 + int(r-'0')
		}
		out[i] = n
	}
	return out, true
}
