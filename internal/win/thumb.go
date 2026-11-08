package win

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Thumbnail 取资源管理器在文件夹里画的那张小图：图片是内容缩略图，视频是第一帧。
//
// 为什么要自己调：mygo 的 Shell.FileIcon 内部固定传 SIIGBF_ICONONLY（源码注释
// "without thumbnails"），只能拿到扩展名的类型图标 —— 所有 png 都是同一枚蓝色
// 图片图标。要看内容就必须走 IShellItemImageFactory::GetImage 且不带 ICONONLY。
func Thumbnail(path string, px int) ([]byte, error) {
	if px <= 0 {
		px = 48
	}
	// COM 的 STA 与线程绑定，而这个函数跑在后台 goroutine 里，必须钉住线程。
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if _, uninit := coInit(); uninit {
		defer procCoUninitialize.Call()
	}

	u, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	var factory uintptr
	if r, _, _ := procSHCreateItemFromParsingName.Call(
		uintptr(unsafe.Pointer(u)), 0,
		uintptr(unsafe.Pointer(iidIShellItemImageFactory)),
		uintptr(unsafe.Pointer(&factory)),
	); hfailed(r) || factory == 0 {
		return nil, hresultErr(path, r)
	}
	defer comCall(factory, 2) // Release

	// CROPTOSQUARE：照片是 4:3 的，直接按比例缩会拉成方块；裁中间更接近资源管理器。
	// BIGGERSIZEOK：让 Shell 从更大的缓存图缩下来，22px 的小图才不糊。
	var hbm uintptr
	sz := uintptr(uint32(px)) | uintptr(uint32(px))<<32 // SIZE, by value
	if r := comCall(factory, 3, sz, siigbfCropToSquare|siigbfBiggerSizeOK, uintptr(unsafe.Pointer(&hbm))); hfailed(r) || hbm == 0 {
		return nil, hresultErr(path, r)
	}
	defer procDeleteObject.Call(hbm)

	img, err := imageOfDIB(hbm)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

const (
	siigbfBiggerSizeOK = 0x1
	siigbfCropToSquare = 0x20
)

var (
	gdi32 = windows.NewLazySystemDLL("gdi32.dll")

	procSHCreateItemFromParsingName = shell32.NewProc("SHCreateItemFromParsingName")
	procGetObjectW                  = gdi32.NewProc("GetObjectW")
	procDeleteObject                = gdi32.NewProc("DeleteObject")
	procCoInitializeEx              = ole32.NewProc("CoInitializeEx")
	procCoUninitialize              = ole32.NewProc("CoUninitialize")

	iidIShellItemImageFactory = &windows.GUID{
		Data1: 0xbcc18b79, Data2: 0xba16, Data3: 0x442f,
		Data4: [8]byte{0x80, 0xc4, 0x8a, 0x59, 0xc3, 0x0c, 0x46, 0x3b},
	}
)

// coInit 初始化当前线程的 COM。返回 true 表示这次真的初始化了，要配对 CoUninitialize。
func coInit() (uintptr, bool) {
	hr, _, _ := procCoInitializeEx.Call(0, 2 /* COINIT_APARTMENTTHREADED */)
	switch hr {
	case 0, 1: // S_OK / S_FALSE（本线程已初始化，同样要配对 Uninitialize）
		return hr, true
	}
	return hr, false // RPC_E_CHANGED_MODE 等：线程已被别人初始化，不好动它
}

// comCall 调 COM 对象的第 index 个 vtable 方法。
func comCall(obj uintptr, index int, args ...uintptr) uintptr {
	vtbl := *(*unsafe.Pointer)(ptrOfUintptr(obj))
	target := *(*uintptr)(unsafe.Add(vtbl, uintptr(index)*8))
	r, _, _ := syscall.SyscallN(target, append([]uintptr{obj}, args...)...)
	return r
}

func hfailed(r uintptr) bool { return int32(r) < 0 }

func hresultErr(path string, r uintptr) error {
	return fmt.Errorf("win: 取 %q 的缩略图失败（HRESULT %#x）", path, uint32(r))
}

// dibSection 是 GetObject 填充的 DIBSECTION。
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

// imageOfDIB 把 Shell 给的 32 位 DIB 拷成 Go 的 NRGBA（像素是 BGRA + 直通 alpha）。
func imageOfDIB(hbm uintptr) (*image.NRGBA, error) {
	var ds dibSection
	if r, _, _ := procGetObjectW.Call(hbm, unsafe.Sizeof(ds), uintptr(unsafe.Pointer(&ds))); r == 0 || ds.bmBits == 0 || ds.bmBitsPixel != 32 {
		return nil, fmt.Errorf("win: 缩略图不是 32 位 DIB（%d bits）", ds.bmBitsPixel)
	}
	w, h := int(ds.bmWidth), int(ds.bmHeight)
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("win: 缩略图尺寸异常（%dx%d）", w, h)
	}
	stride := int(ds.bmWidthBytes)
	src := unsafe.Slice((*byte)(ptrOfUintptr(ds.bmBits)), stride*h)
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		row := y
		if ds.biHeight > 0 { // 正高度是自下而上
			row = h - 1 - y
		}
		s := src[row*stride:]
		d := img.Pix[y*img.Stride:]
		for x := 0; x < w; x++ {
			d[4*x], d[4*x+1], d[4*x+2], d[4*x+3] = s[4*x+2], s[4*x+1], s[4*x], s[4*x+3]
		}
	}
	return img, nil
}
