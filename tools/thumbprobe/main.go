// thumbprobe 量"给文件取缩略图"到底多贵。
//
// 背景：搜索结果里 .png 之类文件现在画的是线框图标，不是资源管理器里那张
// 能看见图片内容的小图。mygo 的 Shell.FileIcon 内部固定传 SIIGBF_ICONONLY
// （"without thumbnails"），所以走它永远只能拿到扩展名的类型图标。要看内容
// 必须自己调 IShellItemImageFactory::GetImage 且不带 ICONONLY。
//
// 这个探针就干这一件事：同一批文件分别用「只要图标」和「允许缩略图」两种
// flags 取一次，报单张耗时、命中率，以及拿回来的是不是真缩略图（按不透明
// 像素占比判断——类型图标四周全是透明边，缩略图基本铺满）。
//
//	go run ./tools/thumbprobe <目录或文件> [更多路径...]
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	siigbfResizeToFit = 0x0 // 允许缩略图，没有就退回图标
	siigbfIconOnly    = 0x4 // 只要类型图标（mygo FileIcon 的做法）
	iconSize          = 48  // 行里 22 逻辑 px × 2.2 倍
)

var (
	shell32 = windows.NewLazySystemDLL("shell32.dll")
	ole32   = windows.NewLazySystemDLL("ole32.dll")
	gdi32   = windows.NewLazySystemDLL("gdi32.dll")

	procSHCreateItemFromParsingName = shell32.NewProc("SHCreateItemFromParsingName")
	procCoInitializeEx              = ole32.NewProc("CoInitializeEx")
	procDeleteObject                = gdi32.NewProc("DeleteObject")
	procGetObjectW                  = gdi32.NewProc("GetObjectW")

	iidIShellItemImageFactory = &windows.GUID{
		Data1: 0xbcc18b79,
		Data2: 0xba16,
		Data3: 0x442f,
		Data4: [8]byte{0x80, 0xc4, 0x8a, 0x59, 0xc3, 0x0c, 0x46, 0x3b},
	}
)

type dibSection struct {
	bmType, bmWidth, bmHeight, bmWidthBytes int32
	bmPlanes, bmBitsPixel                   uint16
	bmBits                                  uintptr
	biSize                                  uint32
	biWidth, biHeight                       int32
	rest                                    [7]uint32
	_                                       [3]uint32
	_                                       uintptr
	_                                       uint32
}

func comCall(obj uintptr, index int, args ...uintptr) uintptr {
	vtbl := *(*unsafe.Pointer)(ptrOfUintptr(obj))
	target := *(*uintptr)(unsafe.Add(vtbl, uintptr(index)*8))
	r, _, _ := syscall.SyscallN(target, append([]uintptr{obj}, args...)...)
	return r
}

// ptrOfUintptr 把整数参数还原成指针，绕开 go vet 的 unsafeptr 误报。
func ptrOfUintptr(p uintptr) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Pointer(&p))
}

func u16(s string) *uint16 {
	p, err := syscall.UTF16PtrFromString(s)
	if err != nil {
		return nil
	}
	return p
}

// thumb 取一张图，返回 (位图句柄, 耗时)。flags 决定要不要缩略图。
func thumb(path string, flags uintptr) (uintptr, time.Duration) {
	t0 := time.Now()
	var factory uintptr
	if r, _, _ := procSHCreateItemFromParsingName.Call(
		uintptr(unsafe.Pointer(u16(path))), 0,
		uintptr(unsafe.Pointer(iidIShellItemImageFactory)),
		uintptr(unsafe.Pointer(&factory)),
	); r != 0 || factory == 0 {
		return 0, time.Since(t0)
	}
	defer comCall(factory, 2) // Release
	var hbm uintptr
	sz := uintptr(uint32(iconSize)) | uintptr(uint32(iconSize))<<32 // SIZE, by value
	if r := comCall(factory, 3, sz, flags, uintptr(unsafe.Pointer(&hbm))); r != 0 {
		return 0, time.Since(t0)
	}
	return hbm, time.Since(t0)
}

// opaqueRatio 统计不透明像素占比：类型图标只有中间一块不透明，缩略图铺满。
func opaqueRatio(hbm uintptr) float64 {
	var ds dibSection
	if r, _, _ := procGetObjectW.Call(hbm, unsafe.Sizeof(ds), uintptr(unsafe.Pointer(&ds))); r == 0 || ds.bmBits == 0 || ds.bmBitsPixel != 32 {
		return 0
	}
	w, h := int(ds.bmWidth), int(ds.bmHeight)
	if w == 0 || h == 0 {
		return 0
	}
	stride := int(ds.bmWidthBytes)
	src := unsafe.Slice((*byte)(ptrOfUintptr(ds.bmBits)), stride*h)
	hit := 0
	for y := 0; y < h; y++ {
		s := src[y*stride:]
		for x := 0; x < w; x++ {
			if s[4*x+3] > 8 {
				hit++
			}
		}
	}
	return float64(hit) / float64(w*h)
}

func collect(args []string) []string {
	var files []string
	for _, arg := range args {
		info, err := os.Stat(arg)
		if err != nil {
			continue
		}
		if !info.IsDir() {
			files = append(files, arg)
			continue
		}
		filepath.WalkDir(arg, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			switch strings.ToLower(filepath.Ext(p)) {
			case ".png", ".jpg", ".jpeg", ".webp", ".bmp", ".gif":
				files = append(files, p)
			}
			return nil
		})
	}
	sort.Strings(files)
	return files
}

func main() {
	// COM 是 STA，必须钉住线程。
	runtime.LockOSThread()
	procCoInitializeEx.Call(0, 2 /*COINIT_APARTMENTTHREADED*/)

	args := os.Args[1:]
	if len(args) == 0 {
		args = []string{os.Getenv("WINDIR") + `\Web`}
	}
	files := collect(args)
	if len(files) == 0 {
		fmt.Println("没有找到图片文件")
		return
	}
	fmt.Printf("样本 %d 个文件，图标边长 %dpx，flags: iconOnly=%#x / allowThumb=%#x\n\n",
		len(files), iconSize, siigbfIconOnly, siigbfResizeToFit)

	var (
		iconTotal, thumbTotal time.Duration
		iconOK, thumbOK       int
		iconWorst, thumbWorst time.Duration
	)
	type row struct {
		path                string
		iconDT, thumbDT     time.Duration
		iconOK, thumbOK     bool
		iconRatio, thbRatio float64
	}
	var rows []row
	for _, f := range files {
		hbmIcon, dtIcon := thumb(f, siigbfIconOnly)
		var ratioIcon float64
		if hbmIcon != 0 {
			ratioIcon = opaqueRatio(hbmIcon)
			procDeleteObject.Call(hbmIcon)
			iconOK++
			iconTotal += dtIcon
			if dtIcon > iconWorst {
				iconWorst = dtIcon
			}
		}
		hbmThumb, dtThumb := thumb(f, siigbfResizeToFit)
		var ratioThumb float64
		if hbmThumb != 0 {
			ratioThumb = opaqueRatio(hbmThumb)
			procDeleteObject.Call(hbmThumb)
			thumbOK++
			thumbTotal += dtThumb
			if dtThumb > thumbWorst {
				thumbWorst = dtThumb
			}
		}
		rows = append(rows, row{f, dtIcon, dtThumb, hbmIcon != 0, hbmThumb != 0, ratioIcon, ratioThumb})
	}

	// 只打前 12 条明细，全量数字看汇总。
	shown := 0
	for _, r := range rows {
		if shown >= 12 {
			break
		}
		fmt.Printf("%-58s iconOnly %6s %5.0f%%   allowThumb %6s %5.0f%%\n",
			shorten(r.path, 58), millis(r.iconDT), r.iconRatio*100, millis(r.thumbDT), r.thbRatio*100)
		shown++
	}
	fmt.Printf("\niconOnly   : %d/%d 成功, 平均 %s, 最慢 %s, 合计 %s\n",
		iconOK, len(files), avg(iconTotal, iconOK), millis(iconWorst), millis(iconTotal))
	fmt.Printf("allowThumb : %d/%d 成功, 平均 %s, 最慢 %s, 合计 %s\n",
		thumbOK, len(files), avg(thumbTotal, thumbOK), millis(thumbWorst), millis(thumbTotal))
}

func millis(d time.Duration) string {
	return fmt.Sprintf("%.1fms", float64(d)/float64(time.Millisecond))
}

func avg(total time.Duration, n int) string {
	if n == 0 {
		return "-"
	}
	return millis(total / time.Duration(n))
}

func shorten(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n-3]) + "..."
}
