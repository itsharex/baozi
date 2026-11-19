package main

// 底图"增量平移"的验收：窗口挪 (dx,dy) 时，走"平移 + 只重算贴边那圈"
// 得到的结果，必须和"在新位置整张重烘"逐像素一样 —— 否则拖动时面板
// 边上就会慢慢积累出错的像素（用户看到的就是"跟手了但边变脏"）。

import (
	"image"
	"testing"
)

// testBackingApp 造一个够 composeBacking 跑的 application：
// 整屏磨砂图 + 半分辨率桌面快照 + 常见的 1.25 倍缩放。
func testBackingApp() *application {
	a := &application{}
	a.scale = 1.25
	a.screenX, a.screenY = 0, 0
	a.glassOffX, a.glassOffY = 0, 0
	a.deskHalfX, a.deskHalfY = 0, 0
	a.deskHalfStep = 2
	a.screenGen = 1

	// 磨砂图：整屏 2560×1440 的 1/8，做点横向渐变，平移取整错了才看得出来。
	fw, fh := 320, 180
	frost := image.NewRGBA(image.Rect(0, 0, fw, fh))
	for y := 0; y < fh; y++ {
		for x := 0; x < fw; x++ {
			o := frost.PixOffset(x, y)
			frost.Pix[o+0] = uint8(60 + x%17*7)
			frost.Pix[o+1] = uint8(90 + y%13*5)
			frost.Pix[o+2] = uint8(130 + (x+y)%23*3)
			frost.Pix[o+3] = 255
		}
	}
	a.frostSmall = frost

	// 桌面快照：1280×720（step=2），只给"弧外那条边"当底色用（alphaBacking 下
	// 其实用不上，但留着保证两条代码路径都正常）。
	dw, dh := 1280, 720
	desk := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < dh; y++ {
		for x := 0; x < dw; x++ {
			o := desk.PixOffset(x, y)
			desk.Pix[o+0] = uint8(30 + x%31)
			desk.Pix[o+1] = uint8(120 + y%29)
			desk.Pix[o+2] = uint8(200 - x%23)
			desk.Pix[o+3] = 255
		}
	}
	a.deskHalf = desk
	a.frostW, a.frostH = 2560, 1440
	return a
}

func cloneRGBA(src *image.RGBA) *image.RGBA {
	dst := image.NewRGBA(src.Rect)
	copy(dst.Pix, src.Pix)
	return dst
}

// 平移后逐像素比较，返回"差得最多的通道差"和"不一致的像素数"。
func diffRGBA(x, y *image.RGBA) (maxDelta, bad int) {
	if x.Rect != y.Rect {
		return 255, x.Rect.Dx() * x.Rect.Dy()
	}
	for i := 0; i+3 < len(x.Pix); i += 4 {
		for c := 0; c < 4; c++ {
			d := int(x.Pix[i+c]) - int(y.Pix[i+c])
			if d < 0 {
				d = -d
			}
			if d > maxDelta {
				maxDelta = d
			}
			if d > 1 {
				bad++
				break
			}
		}
	}
	return maxDelta, bad
}

func TestBackingShiftMatchesFullRebake(t *testing.T) {
	a := testBackingApp()
	cx, cy := 800, 600
	w, h := 963, 771
	pw, ph := w+2*backingPad, h+2*backingPad
	r := int(24 * 1.25) // glassRadius × scale

	// 1) 原位置的整张底图
	base := cloneRGBA(a.composeBacking(cx, cy, pw, ph, r, backingPad, image.Rectangle{}))

	// 位移取 step 的整数倍（磨砂图 1/8、桌面 1/2 都在整块上），
	// 这样"搬过来的像素"和"重新采样的像素"应当严格相同。
	for _, d := range [][2]int{{16, 24}, {-8, -16}, {32, 0}, {0, 8}, {-24, 40}} {
		dx, dy := d[0], d[1]
		// 2) 增量：平移 + 只重算贴边那圈
		a.pane.pix = cloneRGBA(base)
		shiftPix(a.pane.pix, dx, dy)
		a.repaintBackingBands(cx+dx, cy+dy, pw, ph, r, r+4, dx, dy)
		got := cloneRGBA(a.pane.pix)

		// 3) 参照：在新位置整张重烘
		a.pane.pix = nil
		want := cloneRGBA(a.composeBacking(cx+dx, cy+dy, pw, ph, r, backingPad, image.Rectangle{}))

		maxDelta, bad := diffRGBA(got, want)
		if maxDelta > 2 || bad > 0 {
			// 打印不一致像素的包围盒和逐行计数，定位是哪一圈没重算到。
			minX, minY, maxX, maxY := pw, ph, -1, -1
			var rows []int
			rowHits := map[int]int{}
			for y := 0; y < ph; y++ {
				for x := 0; x < pw; x++ {
					o := got.PixOffset(x, y)
					diff := false
					for c := 0; c < 4; c++ {
						d := int(got.Pix[o+c]) - int(want.Pix[o+c])
						if d < 0 {
							d = -d
						}
						if d > 1 {
							diff = true
						}
					}
					if diff {
						rowHits[y]++
						if x < minX {
							minX = x
						}
						if x > maxX {
							maxX = x
						}
						if y < minY {
							minY = y
						}
						if y > maxY {
							maxY = y
						}
					}
				}
			}
			for y := 0; y < ph; y++ {
				if rowHits[y] > 0 {
					rows = append(rows, y)
				}
			}
			t.Logf("不一致包围盒 = x[%d,%d] y[%d,%d]，涉及行数 %d，首/末行 %v/%v",
				minX, maxX, minY, maxY, len(rows), rows[:min(5, len(rows))], rows[max(0, len(rows)-5):])
			t.Fatalf("平移 (%+d,%+d) 与整张重烘不一致：最大通道差 %d，超阈值像素 %d（共 %d）",
				dx, dy, maxDelta, bad, pw*ph)
		}
	}
}

func TestShiftPixOverlap(t *testing.T) {
	// 往四个方向各搬一次，搬不进来的那条边应当是"垃圾"（调用方随后重算），
	// 搬进来的部分必须严格等于源像素。
	const w, h = 9, 7
	for _, d := range [][2]int{{2, 1}, {-2, -1}, {3, -2}, {-4, 2}, {w - 1, 0}, {0, h - 1}} {
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		for i := 0; i+3 < len(img.Pix); i += 4 {
			img.Pix[i] = uint8(i/4 + 1)
		}
		src := cloneRGBA(img)
		shiftPix(img, d[0], d[1])
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				sx, sy := x+d[0], y+d[1]
				if sx < 0 || sy < 0 || sx >= w || sy >= h {
					continue
				}
				if img.Pix[img.PixOffset(x, y)] != src.Pix[src.PixOffset(sx, sy)] {
					t.Fatalf("平移 (%+d,%+d) 后 (%d,%d) 的像素不是源 (%d,%d)", d[0], d[1], x, y, sx, sy)
				}
			}
		}
	}
}
