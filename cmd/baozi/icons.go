package main

import (
	"image/color"
	"sync"

	"github.com/egoist/mygo/ui"
)

// 底栏「设置」「互传」两个入口的图标。
//
// 画的都是粗描线而不是实心块，但线在圆点处特意断开：16px 这个尺寸下，
// 一条线穿一个圆必然连成一坨黑块（试过，成了"03"）。断开留出的 2 个网格单位
// 换算过来不到 1.5 像素，正好够让圆点和滑杆分开认。
//
// 齿轮也试过，8 个齿 + 双圆在这个尺寸下就是一枚雪花，认不出是设置——所以设置用
// 两条滑杆，那是当代软件里通行的设置符号，两笔就够。
const (
	svgSettings = `<svg xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24">
<g fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round">
<line x1="3" y1="8" x2="6.2" y2="8"/>
<line x1="13.8" y1="8" x2="21" y2="8"/>
<line x1="3" y1="16" x2="11.4" y2="16"/>
<line x1="18.6" y1="16" x2="21" y2="16"/>
</g>
<circle cx="10" cy="8" r="2.6" fill="currentColor"/>
<circle cx="15" cy="16" r="2.6" fill="currentColor"/>
</svg>`

	// 两条方向相反的箭头 = 互传。比"分享"那种单向外发箭头更准：这条功能收也收、发也发。
	// 箭头头画长一点（4.2 个网格单位），小尺寸下才不会退化成一个点。
	svgTransfer = `<svg xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24">
<g fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
<path d="M2.8 8.4h18"/>
<path d="M16.2 4.2l4.2 4.2-4.2 4.2"/>
<path d="M21.2 15.6h-18"/>
<path d="M7.8 11.4l-4.2 4.2 4.2 4.2"/>
</g></svg>`
)

type iconSpec int

const (
	iconGear iconSpec = iota
	iconSwitch
)

// iconBitmapSize 是光栅化的像素边长。显示时缩到 16 DIP，按 2 倍画，
// 在 125% / 150% 缩放下也不会糊。
const iconBitmapSize = 32

var (
	iconSvgs map[iconSpec]*ui.SVG
	iconOnce sync.Once
	iconHold sync.Map // iconKey -> *ui.Bitmap
)

type iconKey struct {
	spec    iconSpec
	r, g, b uint8
	px      int
}

// footerIcon 把图标光栅化成位图再交给 ui.Image。
//
// 为什么不用 ui.Icon：它把图标当字形塞进文字图集（svg.go 的 drawIcon 走
// rt.text.Mask），图集不可用的时候它直接 return——在 ui.Tester 这种没有文字图集
// 的渲染里就是一片空白，连"画出来了没有"都验证不了。位图走图片通路，两条路都稳。
//
// 颜色和尺寸都进缓存键：主题变了要换一张，不能复用上一张的灰。
func footerIcon(spec iconSpec, col ui.Color, px int) *ui.Bitmap {
	iconOnce.Do(func() {
		iconSvgs = map[iconSpec]*ui.SVG{
			iconGear:   ui.MustParseSVG([]byte(svgSettings)),
			iconSwitch: ui.MustParseSVG([]byte(svgTransfer)),
		}
	})
	key := iconKey{spec: spec, r: col.R, g: col.G, b: col.B, px: px}
	if v, ok := iconHold.Load(key); ok {
		return v.(*ui.Bitmap)
	}
	bmp := ui.NewBitmap(iconSvgs[spec].Image(px, px, color.NRGBA{R: col.R, G: col.G, B: col.B, A: 255}))
	iconHold.Store(key, bmp)
	return bmp
}
