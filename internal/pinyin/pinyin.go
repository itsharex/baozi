// Package pinyin 把中文名转成可搜索的拉丁串：
//   - Initials：每字取首字母，"豆包工作" → "dbgz"，给用户敲首字母用
//   - Full：每字取全拼，"豆包工作" → "doubaogongzuo"，给敲全拼用
//
// 只保留字母和数字并统一小写，空格、括号、点号这些一律丢掉——
// 这样 "豆包工作 (Beta)" 和 "豆包工作(Beta)" 得到同一个串，输入不用纠结分隔符。
package pinyin

import (
	"strings"
	"sync"
	"unicode"

	gp "github.com/mozillazg/go-pinyin"
)

var (
	argsInitial = gp.NewArgs()
	argsFull    = gp.NewArgs()
)

func init() {
	argsInitial.Style = gp.FirstLetter
	argsFull.Style = gp.Normal
	for _, a := range []*gp.Args{&argsInitial, &argsFull} {
		a.Heteronym = false
		// 非汉字原样回传，由下面统一过滤
		a.Fallback = func(r rune, _ gp.Args) []string { return []string{string(r)} }
	}
}

var (
	muInitial sync.Map // map[string]string
	muFull    sync.Map
)

// HasHan 判断字符串里是否有汉字，没有就不必做转换（省掉索引构建时的绝大部分开销）。
func HasHan(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

// hasHan 是 HasHan 的快路径：先按字节扫一遍，整串都是 ASCII 就直接判否。
// 建索引要过 174 万个文件名，其中绝大多数是英文/数字，这一层能免掉
// 逐 rune 的 unicode 判定。
func hasHan(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return HasHan(s)
		}
	}
	return false
}

// Initials 返回首字母串；不含汉字时返回空串（调用方按 ASCII 匹配即可）。
func Initials(s string) string {
	if !hasHan(s) {
		return ""
	}
	if v, ok := muInitial.Load(s); ok {
		return v.(string)
	}
	out := join(gp.Pinyin(s, argsInitial))
	muInitial.Store(s, out)
	return out
}

// Full 返回全拼串；不含汉字时返回空串。
func Full(s string) string {
	if !hasHan(s) {
		return ""
	}
	if v, ok := muFull.Load(s); ok {
		return v.(string)
	}
	out := join(gp.Pinyin(s, argsFull))
	muFull.Store(s, out)
	return out
}

// join 把逐字结果拼成只含 [a-z0-9] 的小写串。
func join(perRune [][]string) string {
	var b strings.Builder
	b.Grow(len(perRune))
	for _, cand := range perRune {
		if len(cand) == 0 {
			continue
		}
		for _, r := range strings.ToLower(cand[0]) {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}
