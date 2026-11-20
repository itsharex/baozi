package main

import "testing"

// TestIsUpdateNow 只验参数识别：正常启动（无参数）和其它参数都不能进无窗口更新分支，
// 否则双击启动会忽然变成"查更新然后退出"。
func TestIsUpdateNow(t *testing.T) {
	yes := []string{"--update-now", "-update-now", " Update-Now "}
	for _, a := range yes {
		if !isUpdateNow([]string{a}) {
			t.Errorf("%q 应该识别为更新命令", a)
		}
	}
	no := [][]string{nil, {}, {"--help"}, {"debug"}, {"--update-nowx"}, {"x", "--update-now"}}
	for _, args := range no {
		if isUpdateNow(args) {
			t.Errorf("%v 不该被识别为更新命令", args)
		}
	}
}
