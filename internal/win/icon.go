package win

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/png"
)

// TrimIcon 把图标的空白边裁掉，再把内容居中放进正方形，返回新的 PNG 数据。
//
// Windows 给的图标（UWP 应用尤其明显）四周带一大圈留白：直接塞进圆角方块里
// 看着既偏又小——那圈留白是位图自己的，不是布局算错的。裁掉再居中才是真居中。
// 内容紧贴边会显得过大，所以四周补 3% 呼吸边。
//
// 留白有两种：透明的那种按 alpha 裁（alphaCut）；另一种更隐蔽——一整块**不透明白底
// 或灰底**上画一个小 logo（商店/UWP 的 tile 资源常这样），alpha 全是 255，
// 按透明边一个像素都裁不掉，显示出来就是"白框里一个小图标"。所以裁完 alpha 还要
// 再裁一圈"整行整列颜色都一样"的平板边（flatCut）。
func TrimIcon(data []byte) ([]byte, error) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	b := img.Bounds()
	if b.Dx() == 0 || b.Dy() == 0 {
		return nil, errors.New("空位图")
	}

	alpha := alphaCut(img, b)
	if alpha.Empty() {
		return nil, errors.New("整张都是透明的")
	}
	// 平板边要在 alpha 裁出来的那块里量：外面那圈透明本来就不算内容。
	box := flatCut(img, alpha)
	w, h := box.Dx(), box.Dy()

	square := max(w, h)
	square += square*3/100 + 1
	dst := image.NewNRGBA(image.Rect(0, 0, square, square))
	ox, oy := (square-w)/2, (square-h)/2
	draw.Draw(dst, image.Rect(ox, oy, ox+w, oy+h), img, box.Min, draw.Src)

	var buf bytes.Buffer
	if err := png.Encode(&buf, dst); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// solidAlpha 是"这像素算实心内容"的 alpha 门槛（0..0xffff 的 30%）。
//
// 为什么不能只用 1.6%：Windows 给的 tile 图标（商店/UWP/游戏启动器都这样）外面常带
// 一圈 alpha 只有 15%~25% 的淡阴影，整张图 alpha 都不是 0。门槛定在 1.6% 时那圈淡阴影
// 被当成内容，bbox 就是整张图 —— 显示出来正是"白框里一个小图标"，怎么裁都裁不动。
// 30% 刚好跨过那圈阴影，又留得住抗锯齿边缘。
const solidAlpha = 0x5000

// alphaCut 裁掉透明边：先按"实心内容"找 bbox，找不到（或在原图上几乎占满不到 2%）
// 再退回老的 1.6% 门槛，避免半透明描边/低透明度图标被裁成一团。
func alphaCut(img image.Image, b image.Rectangle) image.Rectangle {
	if box := alphaBox(img, b, solidAlpha); !box.Empty() && box.Dx() > 0 && box.Dy() > 0 {
		return box
	}
	return alphaBox(img, b, 0x1000)
}

// alphaBox 返回 alpha 大于门槛的像素的外接矩形。
func alphaBox(img image.Image, b image.Rectangle, floor uint32) image.Rectangle {
	minX, minY := b.Max.X, b.Max.Y
	maxX, maxY := b.Min.X-1, b.Min.Y-1
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if _, _, _, a := img.At(x, y).RGBA(); a > floor {
				minX = min(minX, x)
				minY = min(minY, y)
				maxX = max(maxX, x)
				maxY = max(maxY, y)
			}
		}
	}
	if maxX < minX || maxY < minY {
		return image.Rectangle{}
	}
	return image.Rect(minX, minY, maxX+1, maxY+1)
}

// flatTol 是"这一行算不算同一个颜色"的容差（0..0xffff 的 16 位通道）。
// 0x0c00 约等于 4.7%，够跨过抗锯齿和轻微渐变，又不至于把 logo 的浅色区当成边。
const flatTol = 0x0c00

// flatCut 从四边往里裁"整行/整列都是同一个颜色"的平板边。
//
// 判据是"行内自洽"而不是"等于角落颜色"：图标外面常常先一圈白底再一圈浅灰 tile 边框，
// 两圈各自都不等于对方，但每一圈自己是一致的，所以逐层都能裁掉。
//
// 两条保护：一眼望去整张都是同一个颜色（纯色方块图标）就原样返回；
// 裁到只剩一小块（<15%）也当作裁过头，退回裁前的结果。
func flatCut(img image.Image, b image.Rectangle) image.Rectangle {
	if b.Dx() <= 0 || b.Dy() <= 0 {
		return b
	}
	start := b
	for {
		changed := false
		for b.Dy() > 1 && flatRow(img, b, b.Min.Y) {
			b.Min.Y++
			changed = true
		}
		for b.Dy() > 1 && flatRow(img, b, b.Max.Y-1) {
			b.Max.Y--
			changed = true
		}
		for b.Dx() > 1 && flatCol(img, b, b.Min.X) {
			b.Min.X++
			changed = true
		}
		for b.Dx() > 1 && flatCol(img, b, b.Max.X-1) {
			b.Max.X--
			changed = true
		}
		if !changed {
			break
		}
	}
	// 裁过头：剩下的内容不到原来的 15%，说明这本来就是一张纯色/近纯色的图。
	if b.Dx() < start.Dx()*15/100 || b.Dy() < start.Dy()*15/100 {
		return start
	}
	return b
}

func flatRow(img image.Image, b image.Rectangle, y int) bool {
	ref := img.At(b.Min.X, y)
	for x := b.Min.X + 1; x < b.Max.X; x++ {
		if !nearColor(ref, img.At(x, y)) {
			return false
		}
	}
	return true
}

func flatCol(img image.Image, b image.Rectangle, x int) bool {
	ref := img.At(x, b.Min.Y)
	for y := b.Min.Y + 1; y < b.Max.Y; y++ {
		if !nearColor(ref, img.At(x, y)) {
			return false
		}
	}
	return true
}

// nearColor 比较两个像素（含 alpha）在容差内是否同色。
func nearColor(a, b color.Color) bool {
	ar, ag, ab, aa := a.RGBA()
	br, bg, bb, ba := b.RGBA()
	return withinTol(ar, br) && withinTol(ag, bg) && withinTol(ab, bb) && withinTol(aa, ba)
}

func withinTol(a, b uint32) bool {
	if a > b {
		return a-b <= flatTol
	}
	return b-a <= flatTol
}
