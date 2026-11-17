package main

import (
	"image"
	"testing"

	"github.com/egoist/mygo/ui"
)

// TestPanelDragsFromEveryEdge 钉住"面板任意空白处都能拖着走"。
//
// 玻璃模式下内容全铺在 Overlay 里的一层 Absolute Column 上，拖动靠它挂着的
// DragWindow。这层的宽高一miss 铺不到面板右下，鼠标从右边或下边按下就会命中
// 下层那条链（磨砂图，没有 DragWindow，命中的又是最上层的第一个包含点），
// 表现就是"只能从上边和左边拖，右边和下边按下去没反应"。
func TestPanelDragsFromEveryEdge(t *testing.T) {
	// 用用户拖过的实际尺寸（878×510）而不是默认的 720×480：内容层一旦比窗口
	// 小，缺口正好落在右边和下边，那才是"只能从上边和左边拖"的来源。
	width, height := 878, 510
	a := testApp(t)
	a.cfg.Panel.Width, a.cfg.Panel.Height = width, height
	a.frost = fakeFrost()
	// 真机上这张图是整屏的（2048×1112 DIP），而且按窗口在屏上的位置带负偏移。
	// 它是一张 Absolute 的巨大图，尺寸比面板大得多 —— 这个差异不能省。
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
	}
	for _, s := range spots {
		before := tester.Drags()
		tester.Press(s.x, s.y)
		tester.Frame()
		tester.Release(s.x, s.y)
		tester.Frame()
		if got := tester.Drags(); got == before {
			t.Errorf("%s（%.0f, %.0f）拖不动面板", s.name, s.x, s.y)
		}
	}
}

// TestPanelDragsBeyondContentBox 证明"内容层盖不满窗口"时右下也不会是死区。
//
// 窗口被撑到 1000×600，而视图仍按配置里的 878×510 铺内容，右下那圈空白只能由
// 最里面那层面板底色（Fill 铺满 + 挂 DragWindow）接住。这正是面板被拖大之后
// "从右边和下边按下去拖不动"的成因。
func TestPanelDragsBeyondContentBox(t *testing.T) {
	a := testApp(t)
	a.cfg.Panel.Width, a.cfg.Panel.Height = 878, 510
	a.frost = fakeFrost()
	a.frostW, a.frostH = 2048, 1112
	a.glassOffX, a.glassOffY = -500, -400

	tester := ui.NewTester(a.view, 1000, 600)
	tester.Frame()

	spots := []struct {
		name string
		x, y float32
	}{
		{"右下（内容层之外）", 990, 590},
		{"右边（内容层之外）", 990, 300},
		{"下边（内容层之外）", 500, 590},
	}
	for _, s := range spots {
		before := tester.Drags()
		tester.Press(s.x, s.y)
		tester.Frame()
		tester.Release(s.x, s.y)
		tester.Frame()
		if got := tester.Drags(); got == before {
			t.Errorf("%s（%.0f, %.0f）拖不动面板", s.name, s.x, s.y)
		}
	}
}

// fakeFrost 造一张够用的"磨砂全屏图"：只要求是有效位图，颜色内容无所谓。
func fakeFrost() *ui.Bitmap {
	return ui.NewBitmap(image.NewRGBA(image.Rect(0, 0, 8, 8)))
}

// TestPanelDragMap 打印一张"哪块空白能拖"的分布图，用来定位拖动区的真实边界。
// 「.」能拖，「X」拖不动，「#」落在可交互元素上（卡片、输入框、按钮，本来就不该拖）。
func TestPanelDragMap(t *testing.T) {
	width, height := 878, 510
	a := testApp(t)
	a.cfg.Panel.Width, a.cfg.Panel.Height = width, height
	a.frost = fakeFrost()
	a.frostW, a.frostH = 2048, 1112
	a.glassOffX, a.glassOffY = -500, -400

	tester := ui.NewTester(a.view, width, height)
	tester.Frame()

	step := float32(28)
	for y := step / 2; y < float32(height); y += step {
		row := ""
		for x := step / 2; x < float32(width); x += step {
			before := tester.Drags()
			tester.Press(x, y)
			tester.Frame()
			tester.Release(x, y)
			tester.Frame()
			if tester.Drags() > before {
				row += "."
			} else {
				row += "X"
			}
		}
		t.Logf("%4.0f %s", y, row)
	}
}
