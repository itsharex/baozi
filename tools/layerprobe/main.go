// 独立的 layered 窗口探针：在屏幕左上角画一块半透明红方块，用来确认
// "UpdateLayeredWindow + AC_SRC_ALPHA" 在这台机器上到底能不能被合成。
//
// 用法：cd D:\work\baozi; go run tools\layerprobe\main.go
// 然后截图看 (60,60)-(360,360) 里有没有红色 —— 用 tools\shot.ps1 抓不到这块，
// 直接 CopyFromScreen 抓整屏即可。
package main

import (
	"fmt"
	"syscall"
	"time"
	"unsafe"
)

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procCreateWindowExW      = user32.NewProc("CreateWindowExW")
	procDefWindowProc        = user32.NewProc("DefWindowProcW")
	procRegisterClassW       = user32.NewProc("RegisterClassExW")
	procGetModuleHandleW     = kernel32.NewProc("GetModuleHandleW")
	procUpdateLayeredWindow  = user32.NewProc("UpdateLayeredWindow")
	procSetWindowPos         = user32.NewProc("SetWindowPos")
	procShowWindow           = user32.NewProc("ShowWindow")
	procCreateCompatibleDC   = gdi32.NewProc("CreateCompatibleDC")
	procCreateDIBSection     = gdi32.NewProc("CreateDIBSection")
	procSelectObject         = gdi32.NewProc("SelectObject")
	procGetDC                = user32.NewProc("GetDC")
	procMessageBoxW          = user32.NewProc("MessageBoxW")
	procPeekMessageW         = user32.NewProc("PeekMessageW")
	procTranslateMessage     = user32.NewProc("TranslateMessage")
	procDispatchMessageW     = user32.NewProc("DispatchMessageW")
)

type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   uintptr
	Icon       uintptr
	Cursor     uintptr
	Background uintptr
	MenuName   *uint16
	Classname  *uint16
	IconSm     uintptr
}

type point struct{ X, Y int32 }
type size struct{ CX, CY int32 }
type rect struct{ Left, Top, Right, Bottom int32 }
type msg struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
}
type blendFunction struct {
	BlendOp             byte
	BlendFlags          byte
	SourceConstantAlpha byte
	AlphaFormat         byte
}

func main() {
	const (
		wsPopup      = 0x80000000
		wsExLayered  = 0x00080000
		wsExTopmost  = 0x00000008
		wsExTool     = 0x00000080
		wsExNoActive = 0x08000000
		ulwAlpha     = 2
		swShowNoAct  = 4
	)

	inst, _, _ := procGetModuleHandleW.Call(0)
	name, _ := syscall.UTF16PtrFromString("BaoziLayerProbe")
	wc := wndClassEx{
		Size:      uint32(unsafe.Sizeof(wndClassEx{})),
		Style:     0x0002 | 0x0001, // CS_HREDRAW|CS_VREDRAW
		WndProc:   procDefWindowProc.Addr(),
		Instance:  inst,
		Classname: name,
	}
	if r, _, err := procRegisterClassW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		fmt.Println("RegisterClass 失败:", err)
		return
	}
	hwnd, _, err := procCreateWindowExW.Call(
		wsExLayered|wsExTopmost|wsExTool|wsExNoActive,
		uintptr(unsafe.Pointer(name)), 0, wsPopup,
		0, 0, 0, 0, 0, 0, inst, 0)
	if hwnd == 0 {
		fmt.Println("CreateWindowEx 失败:", err)
		return
	}

	const tw, th = 300, 300
	memDC, _, _ := procCreateCompatibleDC.Call(0)
	bmi := struct {
		Size        uint32
		Width       int32
		Height      int32
		Planes      uint16
		BitCount    uint16
		Compression uint32
		SizeImage   uint32
		XPels       int32
		YPels       int32
		ClrUsed     uint32
		ClrImportant uint32
	}{Width: tw, Height: -th, Planes: 1, BitCount: 32}
	bmi.Size = uint32(unsafe.Sizeof(bmi))
	var bits uintptr
	dib, _, _ := procCreateDIBSection.Call(memDC, uintptr(unsafe.Pointer(&bmi)), 0,
		uintptr(unsafe.Pointer(&bits)), 0, 0)
	if dib == 0 {
		fmt.Println("CreateDIBSection 失败")
		return
	}
	procSelectObject.Call(memDC, dib)

	// 全块 alpha=128 的纯红（预乘：R=255*128/255=128）
	buf := unsafe.Slice((*byte)(unsafe.Pointer(bits)), tw*th*4)
	for i := 0; i < tw*th; i++ {
		buf[i*4+0] = 128
		buf[i*4+1] = 0
		buf[i*4+2] = 0
		buf[i*4+3] = 128
	}

	dst := point{X: 60, Y: 60}
	sz := size{CX: tw, CY: th}
	src := point{}
	blend := blendFunction{SourceConstantAlpha: 255, AlphaFormat: 1}
	dc, _, _ := procGetDC.Call(0)
	r, _, err := procUpdateLayeredWindow.Call(hwnd, dc,
		uintptr(unsafe.Pointer(&dst)), uintptr(unsafe.Pointer(&sz)),
		memDC, uintptr(unsafe.Pointer(&src)), 0,
		uintptr(unsafe.Pointer(&blend)), ulwAlpha)
	fmt.Println("UpdateLayeredWindow ->", r, err)
	pr, _, _ := procSetWindowPos.Call(hwnd, ^uintptr(0) /* HWND_TOPMOST */,
		uintptr(dst.X), uintptr(dst.Y), uintptr(tw), uintptr(th), 0x0010)
	fmt.Println("SetWindowPos ->", pr)
	sw, _, _ := procShowWindow.Call(hwnd, swShowNoAct)
	fmt.Println("ShowWindow ->", sw)
	fmt.Println("方块在 (60,60)-(360,360)，5 秒后关掉")

	go func() {
		var m msg
		for {
			if rr, _, _ := procPeekMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0, 1); rr == 0 {
				time.Sleep(5 * time.Millisecond)
				continue
			}
			procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
			procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
		}
	}()
	time.Sleep(6 * time.Second)
}
