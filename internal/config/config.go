// Package config 读取用户配置。目标是"像 Listary 一样简单"：
// 只有一个 config.json，所有项都有合理默认值，缺项不用管。
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config 是全部配置。字段与 JSON 一一对应，缺省项使用 Default() 的值。
type Config struct {
	Hotkey   Hotkey    `json:"hotkey"`
	Search   Search    `json:"search"`
	Index    Index     `json:"index"`
	History  History   `json:"history"`
	Share    Share     `json:"share"`
	Commands []Command `json:"commands"`
	// DialogFollow 打开"文件对话框跟着资源管理器走"：另存为框开着时去资源管理器换目录，
	// 再切回那个框，路径自动变成新目录（Lertaro / Listary 的行为）。
	// 用指针是为了区分"没写"（默认开）和"写了 false"（明确关掉，Baozi 就不再读别人的窗口）。
	DialogFollow *bool `json:"dialogFollow,omitempty"`
}

// DialogFollowOn 表示"文件对话框跟随"是否开启：配置里没写就是开（和产品默认一致）。
func (c *Config) DialogFollowOn() bool {
	if c.DialogFollow == nil {
		return true
	}
	return *c.DialogFollow
}

// Share 控制局域网互传。
type Share struct {
	// Bind 指定绑哪块网卡（填它的 IPv4 地址）。留空 = 所有网卡（默认）。
	// 机器上有 VMware / MuMu / Hyper-V 这类虚拟网卡时，自动挑选可能挑到它们，
	// 那种情况下在这里固定成实际联网的那块。
	Bind string `json:"bind"`
	// TTLMinutes 是分享链接的有效期（分钟），默认 60。到点自动失效——
	// 分享是一次性的临时通道，不做"只要不停止就一直挂着"。
	TTLMinutes int `json:"ttlMinutes"`
}

// Hotkey 控制唤出方式。
type Hotkey struct {
	// DoubleCtrl 双击 Ctrl 唤出（Lertaro 的默认交互）。
	DoubleCtrl bool `json:"doubleCtrl"`
	// DoubleCtrlIntervalMs 两次 Ctrl 按下的最大间隔。
	DoubleCtrlIntervalMs int `json:"doubleCtrlIntervalMs"`
	// Combo 备用组合键，走 RegisterHotKey；留空表示不注册。
	Combo string `json:"combo"`
	// DisableInFullscreen 全屏应用（游戏/播放器）前台时不响应。
	DisableInFullscreen bool `json:"disableInFullscreen"`
}

// Search 控制结果排序。权重顺序即用户要的优先级：频次 > 类型 > 准确性。
type Search struct {
	Weights    Weights  `json:"weights"`
	TypeOrder  []string `json:"typeOrder"`
	MaxResults int      `json:"maxResults"`
}

// Weights 三项权重，内部会归一化，不需要凑成 1。
type Weights struct {
	Frequency float64 `json:"frequency"`
	Type      float64 `json:"type"`
	Accuracy  float64 `json:"accuracy"`
}

// Index 控制索引范围。
type Index struct {
	Roots    []string `json:"roots"`
	Excludes []string `json:"excludes"`
	// IncludeHidden 只对隐藏**文件**生效；隐藏**目录**一律递归进去，
	// 否则被工具或用户随手设了 Hidden 的备份目录会整棵搜不到。
	IncludeHidden bool `json:"includeHidden"`
	// RescanMinutes 全量重建索引的间隔；0 表示只在启动时建一次。
	RescanMinutes int `json:"rescanMinutes"`
}

// History 控制使用历史（频次与最近使用）。
type History struct {
	Enabled  bool `json:"enabled"`
	MaxItems int  `json:"maxItems"`
}

// Command 是 Listary 式的自定义命令：输入 keyword 空格后，回车执行。
type Command struct {
	Keyword string   `json:"keyword"`
	Name    string   `json:"name"`
	Exec    string   `json:"exec"`
	Args    []string `json:"args"`
	// KeepOpen 执行后不隐藏面板（例如打开一个命令行窗口）。
	KeepOpen bool `json:"keepOpen"`
}

// Default 给出一份能直接跑的配置：索引所有固定磁盘，排除系统噪声目录。
func Default() *Config {
	return &Config{
		Hotkey: Hotkey{
			DoubleCtrl:           true,
			DoubleCtrlIntervalMs: 400,
			Combo:                "Alt+Shift+Space",
			DisableInFullscreen:  true,
		},
		Search: Search{
			Weights: Weights{Frequency: 0.5, Type: 0.3, Accuracy: 0.2},
			TypeOrder: []string{
				".exe", ".lnk", ".msc", ".bat", ".cmd", ".ps1",
				".url", ".appref-ms", ".ps1xml", ".com", ".jar",
			},
			MaxResults: 80,
		},
		Index: Index{
			Roots:         []string{},
			Excludes:      defaultExcludes(),
			IncludeHidden: false,
			RescanMinutes: 0,
		},
		History: History{Enabled: true, MaxItems: 200},
		Share:   Share{Bind: "", TTLMinutes: 60}, // 默认所有网卡、链接活 1 小时
		Commands: []Command{
			{Keyword: "term", Name: "Windows Terminal", Exec: "wt.exe"},
			{Keyword: "code", Name: "VS Code", Exec: "code"},
			{Keyword: "explore", Name: "打开资源管理器", Exec: "explorer.exe"},
		},
	}
}

// Dir 返回配置与数据目录（%LOCALAPPDATA%\Baozi）。
// BAOZI_DATA_DIR 可覆盖，用于隔离测试或多份互不干扰的配置。
func Dir() (string, error) {
	if d := strings.TrimSpace(os.Getenv("BAOZI_DATA_DIR")); d != "" {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return "", err
		}
		return d, nil
	}
	base, err := os.UserCacheDir() // Windows: %LOCALAPPDATA%
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "Baozi")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// Path 返回 config.json 的完整路径。
func Path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// Load 读取配置；文件不存在时落盘一份默认配置，保证用户能直接编辑。
func Load() (*Config, string, error) {
	path, err := Path()
	if err != nil {
		return nil, "", err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		cfg := Default()
		cfg.Index.Roots = SystemRoots()
		if saveErr := Save(cfg, path); saveErr != nil {
			return nil, path, fmt.Errorf("写入默认配置: %w", saveErr)
		}
		return cfg, path, nil
	}
	if err != nil {
		return nil, path, err
	}
	cfg := Default()
	cfg.Index.Roots = SystemRoots()
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, path, fmt.Errorf("解析 %s: %w", path, err)
	}
	cfg.normalize()
	return cfg, path, nil
}

// Save 原子写入配置。
func Save(cfg *Config, path string) error {
	if cfg.Index.Roots == nil {
		cfg.Index.Roots = SystemRoots()
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (c *Config) normalize() {
	if c.Hotkey.DoubleCtrlIntervalMs <= 0 {
		c.Hotkey.DoubleCtrlIntervalMs = 400
	}
	if c.Search.MaxResults <= 0 {
		c.Search.MaxResults = 80
	}
	if len(c.Search.TypeOrder) == 0 {
		c.Search.TypeOrder = Default().Search.TypeOrder
	}
	if c.Search.Weights == (Weights{}) {
		c.Search.Weights = Default().Search.Weights
	}
	if c.History.MaxItems <= 0 {
		c.History.MaxItems = 200
	}
	if c.Share.TTLMinutes <= 0 {
		// 老配置里没有这一项：默认 1 小时。
		c.Share.TTLMinutes = 60
	}
	if len(c.Index.Roots) == 0 {
		c.Index.Roots = SystemRoots()
	}
	// 把"一定不该索引"的缓存目录并进用户列表：老配置里没有这些项，
	// 不合并的话隐藏目录解禁后 AppData 里几万个缓存文件会立刻进来抢结果。
	c.Index.Excludes = mergeExcludes(defaultExcludes(), c.Index.Excludes)
	c.History.Enabled = true // 目前历史是排序权重的一部分，恒开
}

// defaultExcludes 是永远不该进索引的目录：回收站、系统维护目录、依赖目录，
// 以及 AppData 里的各类缓存/编译产物。用户可以在自己的列表里追加，但删不掉这些。
func defaultExcludes() []string {
	return []string{
		"$Recycle.Bin", "System Volume Information", "$WinREAgent",
		"node_modules", ".git", ".svn", ".hg", ".cache",
		"Windows\\WinSxS", "Windows\\servicing", "Windows\\component store",
		"AppData\\Local\\Temp", "AppData\\Local\\Packages",
		// 缓存与编译产物：双击这些文件没有意义，只会把真正的文件挤出结果。
		"AppData\\Local\\go-build", "AppData\\Local\\pnpm", "AppData\\Local\\npm-cache",
		"AppData\\Local\\pip", "AppData\\Local\\python", "AppData\\Local\\Thorium",
		"AppData\\Local\\doubaowork", "AppData\\Local\\Microsoft\\Windows\\Explorer",
		"AppData\\Local\\Microsoft\\Windows\\INetCache",
		// "最近使用"的快捷方式集合：Baozi 自己就记历史，索引它只是把 .lnk 塞满结果。
		"AppData\\Roaming\\Microsoft\\Windows\\Recent",
		"AppData\\Roaming\\Tencent",
	}
}

// mergeExcludes 让 base 始终生效，再补上用户自己的规则（大小写不敏感去重）。
func mergeExcludes(base, user []string) []string {
	seen := make(map[string]bool, len(base)+len(user))
	out := make([]string, 0, len(base)+len(user))
	for _, list := range [][]string{base, user} {
		for _, rule := range list {
			rule = strings.TrimSpace(rule)
			if rule == "" {
				continue
			}
			key := strings.ToLower(rule)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, rule)
		}
	}
	return out
}

// SystemRoots 探测本机固定磁盘盘符。
func SystemRoots() []string {
	var roots []string
	for _, r := range "CDEFGHIJKLMNOPQRSTUVWXYZ" {
		root := string(r) + `:\`
		if st, err := os.Stat(root); err == nil && st.IsDir() {
			roots = append(roots, root)
		}
	}
	return roots
}

// Quote 供帮助文案使用。
func Quote(s string) string { return strings.TrimSpace(s) }
