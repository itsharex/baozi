package main

import (
	"os"
	"path/filepath"
	"strings"

	"baozi/internal/config"
)

// commandWords 判断查询 q 是否命中这条命令的某个激活词（主词或别名）。
// 命中时返回激活词后面那些词 —— 也就是用户这次随手补的参数。
//
// 命中规则是"整词"，不做事后模糊：敲 term 出，敲 ter 不出。
// 中文激活词同理（"关机 30" 命中，"关 30" 不命中）。
func commandWords(cmd config.Command, q string) ([]string, bool) {
	lowQ := strings.ToLower(q)
	for _, kw := range cmd.Keywords() {
		low := strings.ToLower(kw)
		if lowQ == low {
			return nil, true
		}
		if strings.HasPrefix(lowQ, low+" ") {
			// 按原词的字节长度切：ToLower 一般不改变长度，中文更不受影响。
			if len(q) < len(kw) {
				continue
			}
			return strings.Fields(q[len(kw):]), true
		}
	}
	return nil, false
}

// hasPlaceholder 报告一段文本里有没有 %1…%9 / %* / {dir}。
func hasPlaceholder(s string) bool {
	if strings.Contains(s, "{dir}") {
		return true
	}
	if strings.Contains(s, "%*") {
		return true
	}
	for i := byte('1'); i <= '9'; i++ {
		if strings.Contains(s, "%"+string(i)) {
			return true
		}
	}
	return false
}

// expandTemplate 把 {dir} 与 %N 换掉。dir 是当前跟随到的文件管理器目录，
// 没有就是空串（跟随没开、或还没读到目录）。
func expandTemplate(s string, words []string, dir string) string {
	if s == "" {
		return ""
	}
	s = strings.ReplaceAll(s, "{dir}", dir)
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '%' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		next := s[i+1]
		switch {
		case next == '*':
			b.WriteString(strings.Join(words, " "))
			i++
		case next >= '1' && next <= '9':
			n := int(next - '0')
			if n <= len(words) {
				b.WriteString(words[n-1])
			}
			i++
		default:
			// 不是占位符（比如 "100%"），原样留着。
			b.WriteByte('%')
		}
	}
	return b.String()
}

// expandArgs 算出这次要传给程序的参数。
//
// 参数里写了占位符 → 只按模板替换，用户多敲的词不再追加（写入方自己决定放哪）；
// 一个占位符都没写   → 老行为：把用户多敲的词按顺序追到末尾。
func expandArgs(args []string, words []string, dir string) []string {
	templated := false
	for _, a := range args {
		if hasPlaceholder(a) {
			templated = true
			break
		}
	}
	if templated {
		out := make([]string, 0, len(args))
		for _, a := range args {
			out = append(out, expandTemplate(a, words, dir))
		}
		return out
	}
	out := make([]string, 0, len(args)+len(words))
	out = append(out, args...)
	out = append(out, words...)
	return out
}

// launchSpec 把"程序"那一栏翻译成真正要启动的 exe 和参数。
//
// 直接填脚本路径时自动包一层解释器 —— 这是自定义命令里最容易被劝退的一步：
// 想跑个 .ps1 得自己拼 powershell.exe -NoProfile -ExecutionPolicy Bypass
// -WindowStyle Hidden -File …，一长串。填 D:\x\y.ps1 就不用管了。
// 认不出来的扩展名原样交给 exec（.exe / .cmd 在 PATH 里的短名都走这条路）。
func launchSpec(program string, args []string) (string, []string) {
	low := strings.ToLower(strings.TrimSpace(program))
	switch {
	case strings.HasSuffix(low, ".ps1"):
		base := []string{"-NoProfile", "-ExecutionPolicy", "Bypass", "-WindowStyle", "Hidden", "-File", program}
		return systemExe(filepath.Join("WindowsPowerShell", "v1.0", "powershell.exe")), append(base, args...)
	case strings.HasSuffix(low, ".bat"), strings.HasSuffix(low, ".cmd"):
		return systemExe("cmd.exe"), append([]string{"/c", program}, args...)
	}
	return program, args
}

// systemExe 拼系统目录下的 exe 绝对路径。
// 不直接用裸名交给 PATH：Baozi 可能由 Run 键自启、从托盘拉起，环境变量未必和
// 手动开的终端一样；这条找不到解释器，命令就"点了没反应"。
// 拼出来的文件不存在（极老/精简系统）才退回裸名。
func systemExe(sub string) string {
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	p := filepath.Join(root, "System32", sub)
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return filepath.Base(sub)
}
