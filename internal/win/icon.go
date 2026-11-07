package win

import (
	"bytes"
	"errors"
	"image"
	"image/draw"
	"image/png"
)

// TrimIcon 把图标的透明边裁掉，再把内容居中放进正方形，返回新的 PNG 数据。
//
// Windows 给的图标（UWP 应用尤其明显）四周带一大圈透明像素：直接塞进圆角方块里
// 看着既偏又小——那圈留白是位图自己的，不是布局算错的。裁掉再居中才是真居中。
// 内容紧贴边会显得过大，所以四周补 3% 呼吸边。
func TrimIcon(data []byte) ([]byte, error) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	b := img.Bounds()
	if b.Dx() == 0 || b.Dy() == 0 {
		return nil, errors.New("空位图")
	}

	minX, minY := b.Max.X, b.Max.Y
	maxX, maxY := b.Min.X-1, b.Min.Y-1
	// alpha 低于 1.6%（0x1000/0xffff）的算透明：抗锯齿边缘那点灰不该算进内容。
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
	if maxX < minX || maxY < minY {
		return nil, errors.New("整张都是透明的")
	}

	w, h := maxX-minX+1, maxY-minY+1
	side := w
	if h > side {
		side = h
	}
	square := side + side*3/100 + 1
	dst := image.NewNRGBA(image.Rect(0, 0, square, square))
	ox, oy := (square-w)/2, (square-h)/2
	draw.Draw(dst, image.Rect(ox, oy, ox+w, oy+h), img, image.Pt(minX, minY), draw.Src)

	var buf bytes.Buffer
	if err := png.Encode(&buf, dst); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
