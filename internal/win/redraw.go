package win

// RedrawNow 让窗口立刻重绘一帧（RedrawWindow + RDW_UPDATENOW 会在返回前把
// WM_PAINT 派发掉，mygo 的 surface 收到 WM_PAINT 就画一帧，见
// internal/windows/surface.go 的 wmPaint）。
//
// 为什么需要它：拖动面板走的是系统的模态拖动循环（mygo 的 DragWindow =
// SendMessage(WM_NCLBUTTONDOWN)），那段时间帧请求都在队列里排队 —— 窗口已经
// 挪到新位置，屏幕上还是旧的取景，看起来就是"背景追不上窗口"。回调里自己
// 叫一次 UpdateNow，等于把这一帧插在窗口移动的同一时刻。
func RedrawNow(hwnd uintptr) {
	if hwnd == 0 {
		return
	}
	const (
		rdwInvalidate  = 0x0001
		rdwUpdateNow   = 0x0002
		rdwAllChildren = 0x0040
	)
	_, _, _ = procRedrawWindow.Call(hwnd, 0, 0, rdwInvalidate|rdwUpdateNow|rdwAllChildren)
}
