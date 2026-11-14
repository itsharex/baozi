package main

import (
	"testing"

	"github.com/egoist/mygo"
)

// 记下的面板位置必须夹回工作区：拔掉一块显示器、把 4K 上的坐标搬到 1080p 上之后，
// 旧坐标可能整块落在屏外——面板一唤出就在看不见的地方，启动器等于失效了。
func TestClampPanelTo(t *testing.T) {
	// 1920×1080 的屏，任务栏 40 高。
	wa := mygo.Rectangle{X: 0, Y: 0, Width: 1920, Height: 1040}
	cases := []struct {
		name         string
		x, y, w, h   int
		wantX, wantY int
	}{
		{"完全在区内就不动", 600, 200, 720, 480, 600, 200},
		{"右边越界拉回来", 1900, 200, 720, 480, 1920 - 720, 200},
		{"下边越界拉回来", 600, 1000, 720, 480, 600, 1040 - 480},
		{"左上负坐标拉回原点", -300, -200, 720, 480, 0, 0},
		// 面板比工作区还宽（竖屏 + 很宽的面板）：宁可右边溢出，左上角也得在区内
		{"比工作区还宽时贴住左边", -100, 50, 3000, 480, 0, 50},
		{"比工作区还高时贴住上边", 50, -100, 720, 2000, 50, 0},
		{"正好贴着右下角", 1200, 560, 720, 480, 1200, 560},
	}
	for _, c := range cases {
		gotX, gotY := clampPanelTo(c.x, c.y, c.w, c.h, wa)
		if gotX != c.wantX || gotY != c.wantY {
			t.Fatalf("%s：想要 %d,%d，实际 %d,%d", c.name, c.wantX, c.wantY, gotX, gotY)
		}
	}
}
