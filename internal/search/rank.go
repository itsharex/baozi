package search

import (
	"sort"
	"strings"
)

// Kind 是结果类型，排序与图标都靠它。
type Kind uint8

const (
	KindFile Kind = iota
	KindDir
	KindApp
	KindCommand
)

// Band 是结果的硬分段：段号小的永远排前面，段内才比加权分。
//
// 主序为什么是"匹配质量"而不是"扩展名"：
//   - 用户的原话是"大量无用的文件排在搜索结果前列，根本找不到自己真正需要的 app"。
//   - 先试过按扩展名分段（应用 / 命令 / 类型顺序 / 目录 / 普通文件），实测搜 code
//     时前 8 条全是 C:\Program Files 里的 wsdcodegen.exe、frcode.exe、pdfdecode.exe
//     —— 它们是"包含匹配"（acc 0.78），但 .exe 的类型分 0.90 对普通文件 0.20，
//     跨段比较永远是它们赢。类型最能表达"用户想要什么"，却最不能表达
//     "这条到底像不像我在找的东西"。
//   - 所以主序换成匹配档位：名字全等/前缀/词边界在前，"名字里含这个词"的在后，
//     散落子序列垫底；扩展名只在段内帮忙。应用与命令仍然绝对置顶，这是
//     "输 qq 先看到 QQ"的结构性保证，不靠权重平衡。
type Band uint8

const (
	BandApp     Band = iota // 开始菜单应用：匹配多弱都在最前
	BandCommand             // 自定义命令
	BandStrong              // 强匹配：全等 / 前缀 / 词边界 / 拼音全等
	BandMedium              // 中匹配：名字或路径里含这个词
	BandWeak                // 弱匹配：散落子序列
)

const (
	// 与 match.go 的分档对齐：1.00 全等 · 0.95 前缀 · 0.88 词边界 → 强；
	// 0.78 包含 · 0.62 路径包含 → 中；0.35~0.52 散落 → 弱。
	strongAccuracy = 0.88
	mediumAccuracy = 0.62

	// escalateOpens 是"我常用它"的准入门槛：中匹配但打开过 3 次以上，抬到强档。
	escalateOpens = 3

	// noisePenalty 是垃圾后缀 / 缓存目录的段内降权（同时降一档，见 Rank）。
	noisePenalty = 0.6
)

// Item 是一条候选。
type Item struct {
	Name string
	Path string
	Ext  string
	Kind Kind
	// Hint 是结果行第二行的说明（目录、来源或命令描述）。
	Hint string
	// Command 非空时表示这是一条自定义命令。
	Command string
	Args    []string
	// Dir 是自定义命令的工作目录（空则继承本进程）。
	Dir string
	// KeepOpen 让命令执行后不收起面板（开终端这类窗口时用）。
	KeepOpen bool
	// AppIndex 指向已安装应用列表的下标，-1 表示不是应用。
	AppIndex int
}

// Weights 是排序权重，三项内部归一化。
type Weights struct {
	Frequency float64
	Type      float64
	Accuracy  float64
}

// Result 是打分后的结果。
type Result struct {
	Item
	// Band 是定好的硬分段，见 Rank。
	Band Band
	// Opens 是该路径历史上的自然打开次数，只在"常用中匹配抬档"时用。
	Opens     int
	Frequency float64
	TypeScore float64
	Accuracy  float64
	// Noisy 表示命中的是"翻垃圾才会碰到"的后缀或目录（见 IsNoisy）。
	Noisy bool
	Score float64
}

// TypeScore 按用户配置的类型优先级打分。
// 规则是"开始菜单里的应用永远最前"：
//
//	应用（开始菜单/AppsFolder）  1.00
//	自定义命令                  0.95
//	目录                        0.60
//	配置里的类型顺序            0.90 起，每个名次 -0.05，下限 0.15
//	其余文件                    0.20
//
// 类型顺序的最高档只有 0.90，所以 .exe/.lnk 不会和应用并列——否则同分时
// 只能靠准确度/名字长度决胜，应用就沉下去了。
//
// 注意它现在只在**段内**起作用，不再决定段：搜 code 时一条"包含匹配"的
// wsdcodegen.exe 不会再靠 .exe 的 0.90 骑到"前缀匹配"的项目文件前面。
func TypeScore(ext string, kind Kind, order []string) float64 {
	switch kind {
	case KindApp:
		return 1.0
	case KindCommand:
		return 0.95
	case KindDir:
		return 0.60
	}
	ext = strings.ToLower(ext)
	for i, want := range order {
		if strings.ToLower(want) == ext {
			score := 0.90 - 0.05*float64(i)
			if score < 0.15 {
				score = 0.15
			}
			return score
		}
	}
	return 0.2
}

// BandOf 决定一条候选落在哪一段：应用/命令绝对置顶，其余按匹配档位。
func BandOf(accuracy float64, kind Kind, opens int) Band {
	switch kind {
	case KindApp:
		return BandApp
	case KindCommand:
		return BandCommand
	}
	band := BandWeak
	switch {
	case accuracy >= strongAccuracy:
		band = BandStrong
	case accuracy >= mediumAccuracy:
		band = BandMedium
	}
	// 打开过几次的东西，就算只是"名字里含这个词"也应该能被找到。
	if band > BandStrong && opens >= escalateOpens && accuracy >= mediumAccuracy {
		band = BandStrong
	}
	return band
}

// noisyExts 是"命中这些后缀基本是在翻垃圾"的类型。判成噪声不隐藏结果，
// 只是降一档、并在所在段内降权——用户真要找 .log 也还找得到。
var noisyExts = map[string]struct{}{
	".log": {}, ".tmp": {}, ".bak": {}, ".old": {}, ".dll": {}, ".sys": {},
	".ini": {}, ".db": {}, ".cache": {}, ".pid": {}, ".etl": {}, ".dmp": {},
	".pdb": {}, ".ilk": {}, ".obj": {}, ".class": {}, ".pyc": {}, ".lock": {},
	".swp": {}, ".swo": {}, ".part": {}, ".crdownload": {}, ".download": {},
	".nfo": {}, ".torrent": {},
}

// noisyPathFrags 是系统目录、缓存目录、包管理器的落盘目录。
// 按"实际在机器上抓到的东西"补：第三方 CLI 的 .exe 常常埋在
// `.bun\install\cache\@openai\codex@...\vendor\` 这种地方。
//
// 刻意**不**包含 \Program Files\：那里装的是用户真正会用的程序（git、
// notepad++、GuoheView…），靠匹配档位已经足够把它们的辅助程序压下去，
// 整片拉黑会让"搜 git 找我装的 git"变得很难。
var noisyPathFrags = []string{
	`\windows\`, `\appdata\`, `\programdata\`, `\node_modules\`,
	`\program files\common files\`, `\$recycle.bin\`, `\windows.old\`,
	`\packages\`, `\temp\`, `\.git\`, `\.svn\`, `\.cache\`,
	`\cache\`, `\.bun\`, `\site-packages\`, `\vendor\`, `\obj\`,
	`\.nuget\`, `\go-build\`, `\.gradle\`, `\dist-info\`,
}

// IsNoisy 判断这条命中是不是"垃圾堆里翻出来的"。
func IsNoisy(ext, path string) bool {
	if _, ok := noisyExts[strings.ToLower(ext)]; ok {
		return true
	}
	low := strings.ToLower(path)
	for _, frag := range noisyPathFrags {
		if strings.Contains(low, frag) {
			return true
		}
	}
	return false
}

// Rank 给一条候选定段并算分。所有构造点都走这里，避免有人漏设 Band。
//
// 调用方先填 Item（含 Kind/Ext/Path）、Frequency、Accuracy、Opens，
// 段、噪声标记、类型分和总分都由这里补齐。
func Rank(w Weights, order []string, r Result) Result {
	r.TypeScore = TypeScore(r.Ext, r.Kind, order)
	r.Noisy = IsNoisy(r.Ext, r.Path)
	r.Band = BandOf(r.Accuracy, r.Kind, r.Opens)
	if r.Noisy && r.Band < BandWeak {
		r.Band++ // 降一档：垃圾后缀/缓存目录不该站在最前
	}
	r.Score = Combine(w, r.Frequency, r.TypeScore, r.Accuracy)
	if r.Noisy {
		r.Score *= noisePenalty
	}
	return r
}

// Combine 把三项乘权重相加，权重和不为 0 时归一化。
func Combine(w Weights, frequency, typeScore, accuracy float64) float64 {
	sum := w.Frequency + w.Type + w.Accuracy
	if sum <= 0 {
		sum = 1
	}
	return (w.Frequency*frequency + w.Type*typeScore + w.Accuracy*accuracy) / sum
}

// Less 是最终比较器：先比段，段内总分高者在前；同分按准确度、再按频次、
// 最后按名字长度与字典序。
func Less(a, b Result) bool {
	if a.Band != b.Band {
		return a.Band < b.Band
	}
	if a.Score != b.Score {
		return a.Score > b.Score
	}
	if a.Accuracy != b.Accuracy {
		return a.Accuracy > b.Accuracy
	}
	if a.Frequency != b.Frequency {
		return a.Frequency > b.Frequency
	}
	if len(a.Name) != len(b.Name) {
		return len(a.Name) < len(b.Name)
	}
	return strings.ToLower(a.Name) < strings.ToLower(b.Name)
}

// Sort 就地排序。
func Sort(results []Result) {
	sort.SliceStable(results, func(i, j int) bool { return Less(results[i], results[j]) })
}

// Balance 把"某一类占满整张榜"的结果重新交错。
//
// 注意：有了 Band 之后主程序不再调用它（分段已经把类型排成硬顺序，
// 再交错等于把文件硬塞回应用前面）。保留它是给测试和别处留的工具。
//
// 规则：应用最多连续 maxRun 条，之后插入一条非应用（若还有）。
// 两类各自的相对顺序不变，也不丢任何结果。maxRun <= 0 表示不处理。
func Balance(sorted []Result, maxRun int) []Result {
	if maxRun <= 0 {
		return sorted
	}
	var apps, others []Result
	for _, r := range sorted {
		if r.Kind == KindApp {
			apps = append(apps, r)
		} else {
			others = append(others, r)
		}
	}
	if len(apps) == 0 || len(others) == 0 {
		return sorted
	}
	out := make([]Result, 0, len(sorted))
	run := 0
	for len(apps) > 0 || len(others) > 0 {
		if len(apps) > 0 && (run < maxRun || len(others) == 0) {
			out = append(out, apps[0])
			apps = apps[1:]
			run++
			continue
		}
		out = append(out, others[0])
		others = others[1:]
		run = 0
	}
	return out
}
