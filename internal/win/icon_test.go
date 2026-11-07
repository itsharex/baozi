package win

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

// 判断一份 PNG 里"有内容"的区域，测试用。
func contentBox(t *testing.T, data []byte) image.Rectangle {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("解码: %v", err)
	}
	b := img.Bounds()
	minX, minY := b.Max.X, b.Max.Y
	maxX, maxY := b.Min.X-1, b.Min.Y-1
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if _, _, _, a := img.At(x, y).RGBA(); a > 0x1000 {
				if x < minX {
					minX = x
				}
				if y < minY {
					minY = y
				}
				if x > maxX {
					maxX = x
				}
				if y > maxY {
					maxY = y
				}
			}
		}
	}
	return image.Rect(minX, minY, maxX+1, maxY+1)
}

// 系统图标那份"四周一大圈透明边"要裁掉，内容必须落在正中。
func TestTrimIconCentersContent(t *testing.T) {
	// 64x64 的画布，内容只占左上角 24x16 —— 模拟 Windows 图标的偏置。
	src := image.NewNRGBA(image.Rect(0, 0, 64, 64))
	for y := 4; y < 20; y++ {
		for x := 8; x < 32; x++ {
			src.Set(x, y, color.NRGBA{R: 0x07, G: 0xC1, B: 0x60, A: 0xFF})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, src); err != nil {
		t.Fatalf("编码源图: %v", err)
	}
	out, err := TrimIcon(buf.Bytes())
	if err != nil {
		t.Fatalf("裁边: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("解码结果: %v", err)
	}
	if w, h := img.Bounds().Dx(), img.Bounds().Dy(); w != h {
		t.Fatalf("结果应为正方形，实际 %dx%d", w, h)
	}
	box := contentBox(t, out)
	side := img.Bounds().Dx()
	// 左右、上下留白应当对称（正方形由最长边定，短的那一边才有留白，故允许 1px 取整误差）。
	if d := box.Min.X - (side - box.Max.X); d < -1 || d > 1 {
		t.Fatalf("水平没居中：内容 %v，边长 %d，两侧留白差 %d", box, side, d)
	}
	if d := box.Min.Y - (side - box.Max.Y); d < -1 || d > 1 {
		t.Fatalf("垂直没居中：内容 %v，边长 %d，两侧留白差 %d", box, side, d)
	}
	// 内容不能铺满整块，也不能空得离谱。
	if box.Dx() >= side+1 || box.Dx() < side/2 {
		t.Fatalf("内容边长 %d 与方块 %d 比例不对", box.Dx(), side)
	}
}

// 整张都透明时不要返回一张全透明的方图，让调用方退回线框图标。
func TestTrimIconRejectsBlank(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 32, 32))
	var buf bytes.Buffer
	if err := png.Encode(&buf, src); err != nil {
		t.Fatalf("编码: %v", err)
	}
	if _, err := TrimIcon(buf.Bytes()); err == nil {
		t.Fatal("全透明图应当报错")
	}
}
