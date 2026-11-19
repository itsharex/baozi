package main

// 子窗（设置、文件互传）的投影跟随。
//
// 投影层是一块**独立的 layered 窗口**（BaoziShadowWnd），它不会跟着宿主窗口走。
// 主面板不怕这个：它是自绘拖动，每一帧都顺手 followShadowNudge。两个子窗用的是
// mygo 的顶栏拖动（系统拖动：WM_ENTERSIZEMOVE → 一串 WM_MOVE → WM_EXITSIZEMOVE），
// 没有那一步 —— 于是窗口一挪开，原来那块位置就留下一个"只有投影的空框"，
// 四周还是直的，看着就是"拖动后原地留了个框，还带直角"。
//
// 处理办法：拖动开始就把投影收起来（拖动中本来也没人注意投影），窗口挪动时
// 顺便把投影挪到新位置（不拖动时的位置变化，比如程序自己摆窗口），拖动结束再
// 重画一次并显示。这样任何时刻屏幕上都不会有"没人认领的投影"。

import (
	"log"
	"time"

	"github.com/egoist/mygo"
	"golang.org/x/sys/windows"

	"baozi/internal/win"
)

const (
	wmMove                    = 0x0003
	wmShowWindow              = 0x0018
	wmEnterSizeMove           = 0x0231
	wmExitSizeMove            = 0x0232
	wmNonClientLeftButtonDown = 0x00A1

	// 子类化的 id，一个窗口挂一份。
	shadowSubclassID = 0x53484744 // "SHGD"
)

var procInvalidate = dragUser32.NewProc("InvalidateRect")

type shadowFollower struct {
	win   *mygo.Window
	host  uintptr
	glass *subGlass
	tramp uintptr
	drag  bool
}

// installShadowFollower 给子窗的**顶层窗口**挂 WM_MOVE 子类化。
//
// 注意必须挂顶层：WM_MOVE 是发给顶层窗口的，子窗口（MyGoSurface）在父窗口移动时
// 客户区坐标不变，收不到这条消息。
func installShadowFollower(w *mygo.Window, g *subGlass) {
	if w == nil || g == nil {
		return
	}
	// 挂过就不再挂：show() 每次都会调一遍，重复子类化会叠成多层。
	if g.follower != nil && g.follower.host != 0 {
		return
	}
	host := w.NativeHandle()
	if host == 0 {
		// 窗口是 Hidden 创建的，句柄这时可能还没建出来（实测打开设置窗时才第一次
		// 有值）—— 所以 show() 和 refreshShadow() 里还要再调一次，这里安静返回就行。
		return
	}
	f := &shadowFollower{win: w, host: host, glass: g}
	f.tramp = windows.NewCallback(f.proc)
	if r, _, err := procSetSubclass.Call(host, f.tramp, shadowSubclassID, 0); r == 0 {
		log.Printf("投影跟随: %s 没挂上（%v），下一拍再试", g.name, err)
		time.AfterFunc(50*time.Millisecond, func() {
			w.Update(func() { installShadowFollower(w, g) })
		})
		return
	}
	g.follower = f
	log.Printf("投影跟随: %s 已接管（host=%d）", g.name, host)
}

func (f *shadowFollower) proc(hwnd, msg, wparam, lparam, id, data uintptr) uintptr {
	switch msg {
	case wmShowWindow:
		// 窗口在系统层面的显示/隐藏。这是**唯一可靠**的"它到底还在不在屏上"的
		// 信号 —— 收窗口不一定走 glass.hide()：mygo 自己收、系统恢复、别的路径
		// 直接 ShowWindow 都会绕过去，那之后 visible 还停在 true，投影层就又被
		// 画回屏幕上，用户看到的就是"关掉之后桌面上留着一个带阴影的空框"。
		//
		// 藏起来时把三样东西一起收：投影层、visible/hostShown 两个标志、拖动状态。
		// 拖动状态尤其关键：SetCapture 还攥在手里、active 还是 true 的话，窗口
		// 重新打开后每一次按下都被当成"继续拖"，点击全被吞 —— 表现就是"打开了
		// 却点不动关闭"。
		if f.glass != nil {
			if wparam == 0 {
				f.glass.hostShown = false
				f.glass.visible = false
				f.glass.hideShadow()
				if f.glass.drag != nil {
					f.glass.drag.cancel()
				}
				log.Printf("子窗: %s 已隐藏（投影/拖动状态一并收起）", f.glass.name)
			} else {
				f.glass.hostShown = true
			}
		}
	case wmEnterSizeMove:
		f.drag = true
		// 万一这条路径真的被走到（系统拖动），也要和自绘拖动表现一致：
		// 切糊化、重烘一次（子窗版：只烘这一块，不烘面板）、并且把这一帧推上屏。
		if f.glass != nil && f.glass.app != nil {
			f.glass.app.setDragFadeSub(f.glass, true)
		}
		f.glass.refreshShadow()
		if f.win != nil {
			f.win.FrameNow()
		}
	case wmMove:
		if f.drag {
			// 系统拖动中：投影跟着走、底图按位移整体平移、每帧推上屏。
			if cx, cy, _, _ := win.WindowRectPixels(f.host); cx != 0 || cy != 0 {
				f.glass.shiftTo(cx, cy)
			}
			f.glass.followShadow()
			if f.win != nil {
				f.win.FrameNow()
			}
			r, _, _ := procDefSubclass.Call(hwnd, msg, wparam, lparam, id, data)
			return r
		}
		// 不是拖动（程序自己摆窗口、恢复位置）：按新位置重烘一次、跟上投影，
		// 并且立刻推一帧上屏（不然窗口已经挪了，画面还停在原地）。
		f.glass.refresh(true)
		f.glass.followShadow()
		if f.win != nil {
			f.win.FrameNow()
		}
	case wmExitSizeMove:
		f.drag = false
		// 停手：糊化退掉、底图按最终位置重烘一次（都在 setDragFadeSub 里），
		// 推一帧上屏，投影按新位置重画。
		if f.glass != nil && f.glass.app != nil {
			f.glass.app.setDragFadeSub(f.glass, false)
		}
		if f.win != nil {
			f.win.FrameNow()
		}
		f.glass.refreshShadow()
	case wmNonClientLeftButtonDown:
		// 顶栏被当成了标题栏（hitTest == HTCAPTION）：系统准备自己搬窗口了。
		// 不能让它搬 —— noRedirect + 自绘 alpha 的窗口被 DWM 搬运时不认 alpha，
		// 四角立刻变直、玻璃也没了。改成我们自己拖，并吃掉这条消息。
		if f.glass != nil && f.glass.drag != nil && wparam&0xFFFF == 2 {
			f.glass.drag.begin()
			return 0
		}
		procInvalidate.Call(hwnd, 0, 0)
	}
	r, _, _ := procDefSubclass.Call(hwnd, msg, wparam, lparam, id, data)
	return r
}
