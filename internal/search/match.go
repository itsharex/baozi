// Package search 提供 fzf 风格的模糊匹配与排序。
// 匹配只回答"这条像不像"，排序回答"该排第几"，两件事分开，方便按权重调。
package search

import "strings"

// Accuracy 返回 0..1 的匹配质量；ok 为 false 表示不匹配。
// 规则按"人找文件时的直觉"从强到弱：
//
//	1.00 文件名完全等于查询
//	0.95 文件名以查询开头
//	0.88 命中词边界（空格/点/横线/下划线/括号之后）
//	0.78 文件名包含查询
//	0.62 目录路径包含查询
//	0.35~0.50 字符按顺序散落在文件名里（vsc → Visual Studio Code）
//	0.90~0.97 拼音首字母命中（dbgz → 豆包工作，见 AccuracyPy）
func Accuracy(query, name, path string) (float64, bool) {
	return AccuracyPy(query, name, path, "", "")
}

// AccuracyPy 在 ASCII 规则之外补一路拼音匹配。
// py 是名字的拼音首字母串、fullPy 是名字的全拼串（都可能为空）。
// 两路都算，取分高的那个：中文名经常"ASCII 规则也勉强命中"（散落子序列 0.35），
// 但真正的意图是拼音，不该被这种弱命中带偏。
func AccuracyPy(query, name, path, py, fullPy string) (float64, bool) {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return 1, true
	}
	lowName := strings.ToLower(name)
	lowPath := strings.ToLower(path)
	tokens := strings.Fields(q)
	if len(tokens) == 0 {
		return 1, true
	}

	worst := 1.0
	matched := true
	for _, token := range tokens {
		score := tokenScore(token, lowName, lowPath)
		if score < 0 {
			matched = false
			break
		}
		if score < worst {
			worst = score
		}
	}
	// 名字越长，同样的匹配越不精确，给一点轻惩罚，避免长路径把短名字顶掉。
	if matched && len(lowName) > 40 {
		worst *= 0.95
	}
	if !matched {
		worst = 0
	}

	// 拼音只在单关键词、且输入是纯字母数字时尝试（"dbg z" 这种多词输入不按拼音走）。
	if len(tokens) == 1 && isAlphaNum(tokens[0]) {
		if ps := pinyinScore(tokens[0], py, fullPy); ps > worst {
			return ps, true
		}
	}
	if !matched {
		return 0, false
	}
	return worst, true
}

// pinyinScore 给拼音命中分档。首字母命中优先于全拼：
// 敲 4 个字母命中 4 个字的名字，比敲全拼更"精准"，也更符合用户的实际习惯。
func pinyinScore(q, py, full string) float64 {
	best := -1.0
	if py != "" {
		switch {
		case py == q:
			best = 0.97
		case strings.HasPrefix(py, q):
			best = 0.90
		default:
			if _, ok := subsequence(py, q); ok {
				best = 0.45
			}
		}
	}
	if full != "" {
		switch {
		case full == q:
			best = max(best, 0.95)
		case strings.HasPrefix(full, q):
			best = max(best, 0.88)
		case strings.Contains(full, q):
			best = max(best, 0.70)
		}
	}
	return best
}

func isAlphaNum(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}

func max(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func tokenScore(token, name, path string) float64 {
	switch {
	case name == token:
		return 1.0
	case strings.HasPrefix(name, token):
		return 0.95
	case atWordStart(name, token):
		return 0.88
	case strings.Contains(name, token):
		return 0.78
	case path != "" && strings.Contains(path, token):
		return 0.62
	}
	if sub, ok := subsequence(name, token); ok {
		return sub
	}
	return -1
}

// atWordStart 判断 token 是否出现在某个"词首"。
func atWordStart(name, token string) bool {
	if i := strings.Index(name, token); i > 0 {
		switch name[i-1] {
		case ' ', '.', '-', '_', '(', '[', '\\', '/', '@', '+', ',', '{':
			return true
		}
	}
	return false
}

// subsequence 判断 token 的字符是否按顺序出现在 name 中，并按连续程度给分。
func subsequence(name, token string) (float64, bool) {
	ri := []rune(name)
	ti := []rune(token)
	if len(ti) == 0 {
		return 0.5, true
	}
	var (
		matched int
		run     int
		bestRun int
		pos     int
	)
	for _, tr := range ti {
		found := false
		for ; pos < len(ri); pos++ {
			if ri[pos] == tr {
				found = true
				matched++
				run++
				if run > bestRun {
					bestRun = run
				}
				pos++
				break
			}
			run = 0
		}
		if !found {
			return 0, false
		}
	}
	if matched == 0 {
		return 0, false
	}
	// 连续字符越多越像"真命中"；整体得分落在 0.35..0.52。
	consecutive := float64(bestRun) / float64(len(ti))
	return 0.35 + 0.17*consecutive, true
}
