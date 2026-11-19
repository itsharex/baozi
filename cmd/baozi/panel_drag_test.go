package main

import (
	"image"
	"testing"

	"github.com/egoist/mygo/ui"
)

// TestPanelDragIsNotHandledByView 钉住"拖动只有一处，在 hwnd 层"。
//
// 以前面板靠视图里挂 DragWindow 让系统去拖：窗口位置是系统立刻设的，玻璃内容要
// 等我们算完 → 提交 → DWM 合成，实测帧周期 24~36ms（28~44fps），快拖时错位十几
// 像素，一眼就能看出来。现在拖动由 cmd/baozi/dragwin.go 在 WndProc 里接管
// （先算内容、再挪窗口、再提交一帧），视图里一个 DragWindow 都不该再有 ——
// 两套同时生效会互相抢鼠标，表现为"拖一下跳两下"。
func TestPanelDragIsNotHandledByView(t *testing.T) {
	width, height := 878, 510
	a := testApp(t)
	a.cfg.Panel.Width, a.cfg.Panel.Height = width, height
	a.frost = fakeFrost()
	a.frostW, a.frostH = 2048, 1112
	a.glassOffX, a.glassOffY = -500, -400

	tester := ui.NewTester(a.view, width, height)
	tester.Frame()

	w, h := float32(width), float32(height)
	spots := []struct {
		name string
		x, y float32
	}{
		{"上边", w / 2, 6},
		{"左边", 6, h / 2},
		{"右边", w - 6, h / 2},
		{"下边", w / 2, h - 6},
		{"左上角", 6, 6},
		{"右上角", w - 6, 6},
		{"左下角", 6, h - 6},
		{"右下角", w - 6, h - 6},
		{"图标区下方的空白", w / 2, h * 0.72},
	}
	for _, s := range spots {
		before := tester.Drags()
		tester.Press(s.x, s.y)
		tester.Frame()
		tester.Release(s.x, s.y)
		tester.Frame()
		if got := tester.Drags(); got > before {
			t.Errorf("%s（%.0f, %.0f）仍在视图层处理拖动，会跟自绘拖动抢鼠标",
				s.name, s.x, s.y)
		}
	}
}

// TestSelfDragThreshold 钉住"位移不到阈值不算拖动"：那样单击图标、在搜索框里
// 拖选文字才不会被当成拖窗口。
func TestSelfDragThreshold(t *testing.T) {
	if dragThreshold <= 0 || dragThreshold > 24 {
		t.Fatalf("拖动阈值 %d 不合理：太小会抢掉点击，太大手感发黏", dragThreshold)
	}
}

// fakeFrost 造一张够用的"磨砂全屏图"：只要求是有效位图，颜色内容无所谓。
func fakeFrost() *ui.Bitmap {
	return ui.NewBitmap(image.NewRGBA(image.Rect(0, 0, 8, 8)))
}
