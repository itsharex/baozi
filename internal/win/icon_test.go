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

// 不透明白底 + 浅灰 tile 边框 + 中央小 logo：这是商店/UWP 图标常见的形状，
// alpha 全是 255，按透明边一个像素都裁不掉，以前显示出来就是"白框里一个小图标"。
// 现在要裁到 logo，边长应当从 128 掉到 logo 尺寸附近（48 加 3% 呼吸边）。
func TestTrimIconTrimsOpaqueTile(t *testing.T) {
	const side = 128
	src := image.NewNRGBA(image.Rect(0, 0, side, side))
	flat := func(r image.Rectangle, c color.NRGBA) {
		for y := r.Min.Y; y < r.Max.Y; y++ {
			for x := r.Min.X; x < r.Max.X; x++ {
				src.Set(x, y, c)
			}
		}
	}
	white := color.NRGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0xFF}
	grey := color.NRGBA{R: 0xE6, G: 0xE6, B: 0xE6, A: 0xFF}
	flat(image.Rect(0, 0, side, side), white)
	// 1px 浅灰内框（离边 8px，模拟 tile 的边距）
	grey4 := 4
	for i := 8; i < side-8; i++ {
		for _, r := range []image.Rectangle{
			image.Rect(i, 8, i+1, 9), image.Rect(i, side-9, i+1, side-8),
			image.Rect(8, i, 9, i+1), image.Rect(side-9, i, side-8, i+1),
		} {
			flat(r, grey)
		}
	}
	_ = grey4
	// 中央 48x48 的斜向花纹：每行每列颜色都不同，才不会被当成平板边继续裁。
	for y := 40; y < 88; y++ {
		for x := 40; x < 88; x++ {
			src.Set(x, y, color.NRGBA{
				R: uint8(0x20 + (x*3+y*5)%200),
				G: uint8(0x30 + (x*7+y*2)%180),
				B: uint8(0x40 + (x*2+y*9)%170),
				A: 0xFF,
			})
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
	got := img.Bounds().Dx()
	// 48 的内容 + 3% 呼吸边 ≈ 50。放宽到 46~56，免得留一点算法调整空间。
	if got < 46 || got > 56 {
		t.Fatalf("白底 tile 应裁到 logo 大小（约 50），实际 %d（原图 %d）", got, side)
	}
}

// 纯色方块图标（整个是一块同色）不能被裁成空：裁过头要退回裁前的结果。
func TestTrimIconKeepsSolidSquare(t *testing.T) {
	const side = 64
	src := image.NewNRGBA(image.Rect(0, 0, side, side))
	for y := 0; y < side; y++ {
		for x := 0; x < side; x++ {
			src.Set(x, y, color.NRGBA{R: 0x2B, G: 0x6C, B: 0xF5, A: 0xFF})
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
	img, _ := png.Decode(bytes.NewReader(out))
	if w := img.Bounds().Dx(); w < side {
		t.Fatalf("纯色方块被裁小了：%d < %d", w, side)
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
