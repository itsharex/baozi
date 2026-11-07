//go:build ignore

package main

// 一次性图标生成器：把 Baozi 的图标画成多尺寸 ICO + 托盘 PNG。
// 用法：D:\Go\bin\go.exe run tmpgen\main.go
//
// 为什么程序化画而不是找一张图：托盘是 16px，任务栏是 32px，资源管理器大图标
// 是 256px，同一张位图缩到 16px 会糊成色块。这里每个尺寸都按解析式重画一遍
// （4×4 超采样抗锯齿），小尺寸自动退化掉细节。

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
)

func main() {
	outDir := filepath.Join("cmd", "baozi", "assets")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fmt.Println("mkdir:", err)
		return
	}

	// 托盘：32×32 的 PNG（mygo TrayOptions.Icon 要 PNG）
	tray := render(32)
	if err := writePNG(filepath.Join(outDir, "tray.png"), tray); err != nil {
		fmt.Println("tray:", err)
		return
	}
	// 留一张 256 的预览给文档/README 用
	preview := render(256)
	if err := writePNG(filepath.Join(outDir, "icon-256.png"), preview); err != nil {
		fmt.Println("preview:", err)
		return
	}

	// ICO：Windows 会挑最接近的尺寸，所以给全
	sizes := []int{16, 24, 32, 48, 64, 128, 256}
	var pngs [][]byte
	for _, s := range sizes {
		var buf bytes.Buffer
		if err := png.Encode(&buf, render(s)); err != nil {
			fmt.Println("encode", s, err)
			return
		}
		pngs = append(pngs, buf.Bytes())
	}
	icoPath := filepath.Join(outDir, "baozi.ico")
	if err := writeICO(icoPath, sizes, pngs); err != nil {
		fmt.Println("ico:", err)
		return
	}
	fi, _ := os.Stat(icoPath)
	fmt.Printf("已生成 %s（%d 字节）与 tray.png、icon-256.png\n", icoPath, fi.Size())
}

// render 按解析式画一张 size×size 的图标：暖橙圆角底 + 奶油色包子 + 顶部三道褶。
func render(size int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	const ss = 4 // 每像素 4×4 超采样
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			var r, g, b, a float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					u := (float64(x) + (float64(sx)+0.5)/ss) / float64(size)
					v := (float64(y) + (float64(sy)+0.5)/ss) / float64(size)
					cr, cg, cb, ca := shade(u, v, size)
					r += cr * ca
					g += cg * ca
					b += cb * ca
					a += ca
				}
			}
			n := float64(ss * ss)
			if a > 0 {
				r, g, b = r/a, g/a, b/a
			}
			a /= n
			img.Set(x, y, colorNRGBA(r, g, b, a))
		}
	}
	return img
}

// shade 返回该点的颜色（未乘 alpha）与覆盖度。坐标归一化到 [0,1]。
func shade(u, v float64, size int) (float64, float64, float64, float64) {
	// 1) 圆角方块底：占中间 88%，留一点边，托盘上不顶格
	const inset = 0.06
	const radius = 0.20
	d := sdRoundBox(u, v, 0.5, 0.5, 0.5-inset, 0.5-inset, radius)
	if d > 0 {
		return 0, 0, 0, 0
	}
	// 暖橙渐变：左上偏亮、右下偏深
	t := clamp01((u + v) * 0.5)
	br := lerp(1.00, 0.91, t) * 255
	bg := lerp(0.66, 0.30, t) * 255
	bb := lerp(0.30, 0.11, t) * 255
	cov := clamp01(0.5 - d*float64(size)) // 边缘 1 像素内做抗锯齿
	if cov <= 0 {
		return 0, 0, 0, 0
	}

	// 2) 包子：上凸的半圆 + 平底。小尺寸时把褶子关掉，免得糊成一团。
	body := sdBaozi(u, v)
	if body < 0 {
		// 底部 18% 给一点暖阴影，让包子有体积
		shadeDark := clamp01((v - 0.60) / 0.10)
		cr := lerp(1.00, 0.93, shadeDark) * 255
		cg := lerp(0.955, 0.80, shadeDark) * 255
		cb := lerp(0.885, 0.66, shadeDark) * 255
		cov2 := clamp01(0.5 - body*float64(size))
		if cov2 > 0 {
			br, bg, bb = blend(br, cr, cov2), blend(bg, cg, cov2), blend(bb, cb, cov2)
		}
		// 3) 顶部三道褶：从中心向外下方放射，只在 40px 以上画（16/32px 画了就糊成三条竖杠）
		if size >= 40 {
			if fold(u, v) {
				br, bg, bb = blend(br, 0.88*255, 0.6), blend(bg, 0.72*255, 0.6), blend(bb, 0.52*255, 0.6)
			}
		}
	}
	return br, bg, bb, cov
}

// sdRoundBox 是圆角矩形的有符号距离（内部为负）。
func sdRoundBox(px, py, cx, cy, hw, hh, r float64) float64 {
	qx := math.Abs(px-cx) - (hw - r)
	qy := math.Abs(py-cy) - (hh - r)
	ax, ay := math.Max(qx, 0), math.Max(qy, 0)
	return math.Hypot(ax, ay) + math.Min(math.Max(qx, qy), 0) - r
}

// sdBaozi：椭圆主体（宽 0.52、高 0.46），底边切平在 v=0.705。
func sdBaozi(u, v float64) float64 {
	if v > 0.705 {
		return 1
	}
	cx, cy := 0.5, 0.545
	rx, ry := 0.265, 0.235
	dx := (u - cx) / rx
	dy := (v - cy) / ry
	r := math.Hypot(dx, dy)
	return (r - 1) * math.Min(rx, ry)
}

// fold 判断该点是否落在顶部三道褶里：从 (0.5,0.345) 向下方放射，等宽弧线。
func fold(u, v float64) bool {
	const cx, cy = 0.5, 0.345
	dx, dy := u-cx, v-cy
	if dy < 0.030 {
		return false
	}
	dist := math.Hypot(dx, dy)
	if dist < 0.055 || dist > 0.145 { // 中心留一圈不画：那是包子的捏口
		return false
	}
	ang := math.Atan2(dx, dy)                        // 向下为 0，向右为正
	for _, base := range []float64{-0.45, 0, 0.45} { // ±26°，向外略弯
		if math.Abs(ang-(base+dist*0.85))*dist < 0.012 {
			return true
		}
	}
	return false
}

func writeICO(path string, sizes []int, pngs [][]byte) error {
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.LittleEndian, uint16(0))         // reserved
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))         // type: icon
	_ = binary.Write(&buf, binary.LittleEndian, uint16(len(pngs))) // count
	offset := 6 + 16*len(pngs)
	for i, p := range pngs {
		s := sizes[i]
		b := byte(s)
		if s >= 256 {
			b = 0 // ICO 里 256 记成 0
		}
		buf.WriteByte(b)                                            // width
		buf.WriteByte(b)                                            // height
		buf.WriteByte(0)                                            // palette
		buf.WriteByte(0)                                            // reserved
		_ = binary.Write(&buf, binary.LittleEndian, uint16(1))      // planes
		_ = binary.Write(&buf, binary.LittleEndian, uint16(32))     // bpp
		_ = binary.Write(&buf, binary.LittleEndian, uint32(len(p))) // size
		_ = binary.Write(&buf, binary.LittleEndian, uint32(offset)) // offset
		offset += len(p)
	}
	for _, p := range pngs {
		buf.Write(p)
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

func writePNG(path string, img image.Image) error {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

func colorNRGBA(r, g, b, a float64) color.NRGBA {
	return color.NRGBA{
		R: uint8(math.Round(clamp01(r/255) * 255)),
		G: uint8(math.Round(clamp01(g/255) * 255)),
		B: uint8(math.Round(clamp01(b/255) * 255)),
		A: uint8(math.Round(clamp01(a) * 255)),
	}
}

func lerp(a, b, t float64) float64 { return a + (b-a)*t }

func blend(base, over, t float64) float64 { return base*(1-t) + over*t }

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
