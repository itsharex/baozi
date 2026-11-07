package main

import (
	_ "embed"

	"strings"

	"baozi/internal/search"

	"github.com/egoist/mygo/ui"
)

// 图标资源（同一个包子形象，三个地方各取所需）：
//
//	assets/tray.png      托盘/通知栏，32×32 PNG（TrayOptions.Icon 要 PNG）
//	assets/baozi.ico     16/24/32/48/64/128/256 多尺寸，由 rsrc 打进 exe
//	assets/icon-256.png  256 预览，只用于文档
//
// 每个尺寸都是按解析式重画的（圆角方块 + 包子 + 顶部三道褶，4×4 超采样），
// 不是把一张大图缩下来——16px 上褶会自动省掉，缩图会糊成三条竖杠。
// 生成器在 tools/icongen（//go:build ignore），改配色/形状后跑一遍即可。

//go:embed assets/tray.png
var trayPNG []byte

// trayIcon 返回托盘图标。
func trayIcon() []byte { return trayPNG }

// 结果列表里的类型图标：单色线框，颜色跟着主题走（TextColor）。
//
// 之前用的是系统给的文件类型图标（IShellItemImageFactory 按 24px 要的 PNG），
// 问题很实际：那是"资源管理器里长什么样"的彩色位图，白色纸片在深色玻璃底上
// 极刺眼，24px 又不是图标原生尺寸，按 DPI 放大后边缘糊，同一排里花花绿绿
// 也不成体系。Pane 的文件行是同一套单色线框图标，这里照它的做法来。
//
// 应用例外：应用的彩色图标是辨识度本身，继续用系统图标（见 appIcon）。
var (
	glyphFile = svg(`<path d="M15 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V7Z"/><path d="M14 2v4a2 2 0 0 0 2 2h4"/>`)
	glyphDoc  = svg(`<path d="M15 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V7Z"/><path d="M14 2v4a2 2 0 0 0 2 2h4"/><path d="M9 13h6"/><path d="M9 17h4"/>`)
	glyphDir  = svg(`<path d="M20 20a2 2 0 0 0 2-2V8a2 2 0 0 0-2-2h-7.9a2 2 0 0 1-1.69-.9L9.6 3.9A2 2 0 0 0 7.93 3H4a2 2 0 0 0-2 2v13a2 2 0 0 0 2 2Z"/>`)
	glyphImg  = svg(`<rect width="18" height="18" x="3" y="3" rx="2"/><circle cx="9" cy="9" r="2"/><path d="m21 15-3.1-3.1a2 2 0 0 0-2.8 0L6 21"/>`)
	glyphVid  = svg(`<path d="M15 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V7Z"/><path d="M14 2v4a2 2 0 0 0 2 2h4"/><path d="m10 11 5 2.5-5 2.5Z"/>`)
	glyphAud  = svg(`<path d="M9 18V6l12-2v12"/><circle cx="6" cy="18" r="3"/><circle cx="18" cy="16" r="3"/>`)
	glyphZip  = svg(`<rect width="20" height="5" x="2" y="3" rx="1"/><path d="M4 8v11a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8"/><path d="M10 12h4"/>`)
	glyphCode = svg(`<path d="M15 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V7Z"/><path d="M14 2v4a2 2 0 0 0 2 2h4"/><path d="m11 12-2 2 2 2"/><path d="m15 12 2 2-2 2"/>`)
	glyphTerm = svg(`<polyline points="4 17 10 11 4 5"/><line x1="12" x2="20" y1="19" y2="19"/>`)
)

// svg 把一段路径包成 24 网格的线框图标（描边取当前文字色）。
func svg(shapes string) *ui.SVG {
	return ui.MustParseSVG([]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" ` +
		`stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">` + shapes + `</svg>`))
}

// 扩展名分组：决定用哪个线框图标。查不到就退回通用文件图标。
var (
	docExts   = toSet("txt md markdown pdf doc docx dot rtf odt xls xlsx xlsm csv tsv ppt pptx pps ppsx wps et dps epub")
	imageExts = toSet("png jpg jpeg jfif gif bmp dib webp ico svg tif tiff heic heif raw cr2 nef avif")
	videoExts = toSet("mp4 mkv mov avi wmv flv webm ts m4v mpg mpeg 3gp rmvb vob")
	audioExts = toSet("mp3 wav flac aac m4a ogg opus wma ape mid midi amr")
	zipExts   = toSet("zip rar 7z tar gz tgz bz2 xz z zst lz4 iso cab jar apk whl")
	codeExts  = toSet("go js jsx ts tsx py pyw java kt kts c h cc cpp cxx hpp hxx cs rs " +
		"rb php swift lua pl sql sh bash zsh ps1 psm1 bat cmd " +
		"json yaml yml toml ini cfg conf xml html htm css scss less vue svelte proto gradle cmake")
)

func toSet(names string) map[string]bool {
	m := make(map[string]bool, 64)
	for _, n := range strings.Fields(names) {
		m[n] = true
	}
	return m
}

// glyphFor 返回结果左侧的类型图标。应用返回 nil —— 它用系统彩色图标。
func glyphFor(r search.Result) *ui.SVG {
	switch r.Kind {
	case search.KindApp:
		return nil
	case search.KindDir:
		return glyphDir
	case search.KindCommand:
		return glyphTerm
	}
	ext := strings.ToLower(strings.TrimPrefix(r.Ext, "."))
	switch {
	case docExts[ext]:
		return glyphDoc
	case imageExts[ext]:
		return glyphImg
	case videoExts[ext]:
		return glyphVid
	case audioExts[ext]:
		return glyphAud
	case zipExts[ext]:
		return glyphZip
	case codeExts[ext]:
		return glyphCode
	}
	return glyphFile
}
