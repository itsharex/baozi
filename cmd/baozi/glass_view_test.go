package main

import (
	"image"
	"testing"

	"github.com/egoist/mygo/ui"
)

// 玻璃底走的是"整屏磨砂图铺满 + 窗口在上面取景"，图的位置是负偏移。
// 这套写法最容易踩的坑是父容器把溢出的部分裁掉：那样面板里就只剩一片空白。
// 这个测试直接造一张四色分块的"整屏图"，给一个已知的负偏移，看窗口里
// 是不是露出了图上对应那一块 —— 露对了，取景就是对的。
func TestGlassViewportShowsOffsetRegion(t *testing.T) {
	if bakedPanel {
		// 底图（整窗自绘）方案下，面板不再是"整屏磨砂图 + 取景偏移"，玻璃色
		// 直接由 refreshBacking 逐像素烘进底图，这条测试覆盖的旧路径已经不走。
		t.Skip("bakedPanel 下玻璃不走取景偏移，旧路径不参与布局")
	}
	a := testApp(t)
	// 800×500 的图，左红右蓝、上深下浅，四个色块边界很好认。
	const w, h = 800, 500
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var r, g, b uint8
			switch {
			case x < w/2 && y < h/2:
				r = 200 // 左上：红
			case x >= w/2 && y < h/2:
				b = 200 // 右上：蓝
			case x < w/2:
				r, g = 200, 200 // 左下：黄
			default:
				g = 200 // 右下：绿
			}
			i := img.PixOffset(x, y)
			img.Pix[i+0] = r
			img.Pix[i+1] = g
			img.Pix[i+2] = b
			img.Pix[i+3] = 255
		}
	}
	a.frost = ui.NewBitmap(img)
	// 图按 2 倍放大铺成 1600×1000 DIP，窗口 800×600 从图的 DIP (400,200) 开始取景。
	a.frostW, a.frostH = 1600, 1000
	a.glassOffX, a.glassOffY = -400, -200

	tr := ui.NewTester(a.view, 800, 600)
	tr.Frame()
	dumpPreview(t, tr)

	shot := tr.Image()
	at := func(x, y int) (r, g, b uint8) {
		rr, gg, bb, _ := shot.At(x, y).RGBA()
		return uint8(rr >> 8), uint8(gg >> 8), uint8(bb >> 8)
	}

	// 取景对的话，窗口左上角落在图的物理 (200,100)，是左上那块红。
	// 采样点往内收一点：四角被圆角裁掉了，角上那几个像素是透明的。
	r, _, b := at(100, 100)
	if r < 120 || b > 120 {
		t.Fatalf("左上角应是红色块，实际 r=%d b=%d —— 取景偏移没生效或被父容器裁掉了", r, b)
	}
	// 窗口右上角落在图的物理 (600,100)，是右上那块蓝。
	r2, _, b2 := at(700, 100)
	if b2 < 120 || r2 > 120 {
		t.Fatalf("右上角应是蓝色块，实际 r=%d b=%d —— 图没有被放大铺满", r2, b2)
	}
	// 下边落在图的物理 y=400，是浅色那半（黄/绿），不该是上面那种深红蓝。
	_, g3, _ := at(100, 540)
	if g3 < 120 {
		t.Fatalf("左下角应是黄块（g=200），实际 g=%d —— 纵向取景不对", g3)
	}
}
