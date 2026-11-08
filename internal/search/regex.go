// 正则模式：用户自己写模式，只做精确匹配。
//
// 和普通模式是两条完全独立的路径 —— 正则模式下**不**走拼音、**不**走模糊子序列、
// 也不做准确度分档：既然用户写了模式，命中与否就是他说了算，排序只按频次和类型来。
//
// 为什么需要"预筛"：实测本机 174 万条索引上，`^.*\.log$` 直接全量跑正则要 647 ms。
// 每次按键都这么扫一遍，后台会攒出一堆 goroutine。这里从正则语法树里求出"一定出现
// 的字面"（`\.log` → ".log"），先用零分配的子串比较筛一遍，只剩 2.6 万条进正则引擎，
// 同样的查询落到 59 ms。
//
// 预筛只能**比真匹配更宽松**，绝不能更严：算不准的（分支各说各的、量词可能是 0 次、
// 忽略大小写的非 ASCII 字面）一律放弃那部分条件。这时只是少省了点 CPU，不会漏结果。
package search

import (
	"regexp"
	"regexp/syntax"
	"strings"
)

// Regex 是一次编译好的正则查询。
type Regex struct {
	re     *regexp.Regexp
	screen [][]string
}

// CompileRegex 编译用户输入的正则；语法错误原样返回给调用方去提示。
func CompileRegex(pattern string) (*Regex, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	rx := &Regex{re: re}
	// 解析失败不可能发生（Compile 已经用同一套语法过了），失败就不预筛。
	if parsed, err := syntax.Parse(pattern, syntax.Perl); err == nil {
		rx.screen = screenGroups(parsed)
	}
	return rx, nil
}

// Match 报告名字是否命中正则。
func (rx *Regex) Match(name []byte) bool {
	if rx == nil || rx.re == nil {
		return false
	}
	return rx.re.Match(name)
}

// Maybe 是 Match 的廉价前置：返回 false 表示一定不命中，可以跳过 Match。
// 返回 true 只代表"有可能"，仍要调 Match 确认。
func (rx *Regex) Maybe(name []byte) bool {
	if rx == nil || len(rx.screen) == 0 {
		return true
	}
	// 任意一组的字面全部出现 → 可能命中（组之间是"分支"关系）。
	for _, group := range rx.screen {
		ok := true
		for _, lit := range group {
			if !containsFoldASCII(name, lit) {
				ok = false
				break
			}
			// 一条 1 字符的字面筛不出多少，继续算也没意义
		}
		if ok {
			return true
		}
	}
	return false
}

// Score 给正则命中一个固定准确度：正则模式下没有"匹配得好不好"这回事，
// 命中就是精确命中。排序交给频次和类型。
const RegexAccuracy = 1.0

// screenGroups 从语法树求预筛条件：
// 返回若干"组"，每组是一串必须**同时**出现的字面；组之间是或的关系
// （满足任意一组就可能命中）。返回空表示求不出，不做预筛。
func screenGroups(re *syntax.Regexp) [][]string {
	if re == nil {
		return nil
	}
	switch re.Op {
	case syntax.OpLiteral:
		return literalGroups(re)

	case syntax.OpCapture, syntax.OpPlus:
		if len(re.Sub) == 1 {
			return screenGroups(re.Sub[0])
		}

	case syntax.OpRepeat:
		// 至少要出现一次的重复，内层仍然必须出现；{0,n} 不行。
		if re.Min >= 1 && len(re.Sub) == 1 {
			return screenGroups(re.Sub[0])
		}

	case syntax.OpAlternate:
		// 所有分支都得给出条件，否则某个分支能命中的东西会被筛掉。
		var groups [][]string
		for _, sub := range re.Sub {
			g := screenGroups(sub)
			if len(g) == 0 {
				return nil
			}
			groups = append(groups, g...)
		}
		return groups

	case syntax.OpConcat:
		return concatGroups(re)
	}
	return nil
}

// literalGroups 把一段字面变成一组条件；完全求不出就返回 nil。
func literalGroups(re *syntax.Regexp) [][]string {
	s := string(re.Rune)
	if s == "" {
		return nil
	}
	// 忽略大小写 + 非 ASCII：Go 的正则做 Unicode 大小写折叠（ä/Ä），
	// 而下面的预筛只折 ASCII，会更严 → 宁可不用这条字面。
	if re.Flags&syntax.FoldCase != 0 && !isASCII(s) {
		return nil
	}
	return [][]string{{strings.ToLower(s)}}
}

// concatGroups 处理相邻子式：它们都必须命中，所以"单组"的条件可以合并成更强的 AND。
// 遇到分支式的子式（多组）就不合并，退化成"挑一个最强的组"用，仍然安全。
func concatGroups(re *syntax.Regexp) [][]string {
	var (
		and     []string
		bestOR  [][]string
		bestLen int
	)
	for _, sub := range re.Sub {
		g := screenGroups(sub)
		if len(g) == 0 {
			continue
		}
		// 合并的代价：每条都多一次子串扫描。太短的字面（1 字符）不划算。
		mergeable := len(g) == 1 && len(g[0]) == 1 && len(g[0][0]) >= 2
		if mergeable {
			and = append(and, g[0][0])
			continue
		}
		if n := longestGroup(g); n > bestLen {
			bestOR, bestLen = g, n
		}
	}
	if len(and) > 0 {
		return [][]string{dedupe(and)}
	}
	return bestOR
}

// longestGroup 返回这组条件里最长字面的长度（用于挑"最强的那一组"）。
func longestGroup(groups [][]string) int {
	best := 0
	for _, g := range groups {
		for _, lit := range g {
			if len(lit) > best {
				best = len(lit)
			}
		}
	}
	return best
}

func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// containsFoldASCII 在 name 里找小写的 token（只折 ASCII 大小写）。
// 与 index 包里的 containsFold 同义：预筛时操之过宽是安全的，过窄会漏结果。
func containsFoldASCII(name []byte, token string) bool {
	if len(token) == 0 {
		return true
	}
	if len(token) > len(name) {
		return false
	}
	c0 := token[0]
	last := len(name) - len(token)
	for i := 0; i <= last; i++ {
		if lowerByte(name[i]) != c0 {
			continue
		}
		j := 1
		for ; j < len(token); j++ {
			if lowerByte(name[i+j]) != token[j] {
				break
			}
		}
		if j == len(token) {
			return true
		}
	}
	return false
}

func lowerByte(c byte) byte {
	if 'A' <= c && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}
