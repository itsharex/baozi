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
	Frequency float64
	TypeScore float64
	Accuracy  float64
	Score     float64
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

// Combine 把三项乘权重相加，权重和不为 0 时归一化。
func Combine(w Weights, frequency, typeScore, accuracy float64) float64 {
	sum := w.Frequency + w.Type + w.Accuracy
	if sum <= 0 {
		sum = 1
	}
	return (w.Frequency*frequency + w.Type*typeScore + w.Accuracy*accuracy) / sum
}

// Less 是最终比较器：总分高者在前；同分按准确度、再按频次、最后按名字长度与字典序。
func Less(a, b Result) bool {
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
// 典型场景：输入单个字母 d，191 个开始菜单应用里几十个都命中，
// 按分数排下来前 80 条有 72 条是应用——用户看到的就是"只显示应用"。
//
// 规则：应用最多连续 maxRun 条，之后插入一条非应用（若还有）。
// 两类各自的相对顺序不变，也不丢任何结果，只是把原来被应用挤到很后面的
// 文件/目录往前挪。maxRun <= 0 表示不处理。
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
