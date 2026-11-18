# 快速拖动面板，同时用 SendMessageTimeout(WM_NULL) 探 UI 线程有没有卡住。
#
# 模式：
#   move          拖面板空白处（整窗移动）
#   resizeBottom  拖下边缘（只改高度）
#   resizeRight   拖右边缘（只改宽度）
#   resizeCorner  拖右下角（同时改宽高；角上有 30px 圆角区域，很窄，容易点空）
param(
  [ValidateSet("move","resizeBottom","resizeRight","resizeCorner")]
  [string]$Mode = "move",
  [int]$Steps = 80,
  [int]$Dx = 4,
  [int]$Dy = 3,
  [int]$Step = 5,
  [int]$HangTimeoutMs = 40,
  [int]$ProbeEvery = 1,
  [int]$WatchAfterMs = 1500,
  [switch]$NoSummon
)

if (-not ("DragProbe2" -as [type])) {
Add-Type @'
using System;
using System.Text;
using System.Runtime.InteropServices;

public class DragProbe2 {
  public delegate bool EnumProc(IntPtr h, IntPtr l);
  [StructLayout(LayoutKind.Sequential)] public struct RECT { public int Left, Top, Right, Bottom; }

  [DllImport("user32.dll")] public static extern bool SetProcessDPIAware();
  [DllImport("user32.dll")] public static extern bool EnumWindows(EnumProc cb, IntPtr l);
  [DllImport("user32.dll", CharSet=CharSet.Unicode)] public static extern int GetWindowText(IntPtr h, StringBuilder s, int n);
  [DllImport("user32.dll")] public static extern uint GetWindowThreadProcessId(IntPtr h, out uint pid);
  [DllImport("user32.dll")] public static extern bool IsWindowVisible(IntPtr h);
  [DllImport("user32.dll")] public static extern bool GetWindowRect(IntPtr h, out RECT r);
  [DllImport("user32.dll")] public static extern bool SetCursorPos(int x, int y);
  [DllImport("user32.dll")] public static extern void mouse_event(uint f, int dx, int dy, uint data, IntPtr extra);
  [DllImport("user32.dll")] public static extern void keybd_event(byte vk, byte scan, uint flags, IntPtr extra);
  [DllImport("user32.dll")] public static extern IntPtr SendMessageTimeout(IntPtr h, uint msg, IntPtr wp, IntPtr lp, uint flags, uint timeout, out IntPtr res);

  public static IntPtr Find(uint pid) {
    SetProcessDPIAware();
    IntPtr P = IntPtr.Zero;
    EnumWindows((h,l)=>{
      uint p; GetWindowThreadProcessId(h, out p);
      if (p != pid) return true;
      var t = new StringBuilder(256); GetWindowText(h, t, 256);
      if (t.ToString() == "Baozi" && IsWindowVisible(h)) { P = h; return false; }
      return true;
    }, IntPtr.Zero);
    return P;
  }

  public static void DoubleCtrl() {
    keybd_event((byte)0x11, (byte)0, 0, IntPtr.Zero);
    System.Threading.Thread.Sleep(40);
    keybd_event((byte)0x11, (byte)0, 2, IntPtr.Zero);
    System.Threading.Thread.Sleep(240);
    keybd_event((byte)0x11, (byte)0, 0, IntPtr.Zero);
    System.Threading.Thread.Sleep(40);
    keybd_event((byte)0x11, (byte)0, 2, IntPtr.Zero);
  }

  public static string RectStr(IntPtr h) {
    RECT r; if (!GetWindowRect(h, out r)) return "?";
    return (r.Right - r.Left) + "x" + (r.Bottom - r.Top) + "@" + r.Left + "," + r.Top;
  }

  public static int[] Rect(IntPtr h) {
    RECT r; GetWindowRect(h, out r);
    return new int[] { r.Left, r.Top, r.Right - r.Left, r.Bottom - r.Top };
  }

  public static bool Alive(IntPtr h, uint timeoutMs) {
    IntPtr res;
    IntPtr ok = SendMessageTimeout(h, 0, IntPtr.Zero, IntPtr.Zero, 2, timeoutMs, out res);
    return ok != IntPtr.Zero;
  }
}
'@
}

$p = Get-Process -Name baozi,baozi.debug -ErrorAction SilentlyContinue | Select-Object -First 1
if (-not $p) { Write-Output "找不到 baozi 进程"; exit 1 }
$hwnd = [DragProbe2]::Find([uint32]$p.Id)
if ($hwnd -eq [IntPtr]::Zero) {
  if ($NoSummon) { Write-Output "面板不可见，且要求不唤出"; exit 1 }
  [DragProbe2]::DoubleCtrl() | Out-Null
  Start-Sleep -Milliseconds 1800
  $hwnd = [DragProbe2]::Find([uint32]$p.Id)
}
if ($hwnd -eq [IntPtr]::Zero) { Write-Output "找不到面板窗口"; exit 1 }

Start-Sleep -Milliseconds 350
$rect = [DragProbe2]::Rect($hwnd)
$L = $rect[0]; $T = $rect[1]; $W = $rect[2]; $H = $rect[3]
$R = $L + $W; $B = $T + $H

switch ($Mode) {
  "move"         { $x = $L + [int]($W * 0.55); $y = $T + [int]($H * 0.62) }
  "resizeBottom" { $x = $L + [int]($W * 0.5);  $y = $B - 4 }
  "resizeRight"  { $x = $R - 4;               $y = $T + [int]($H * 0.5) }
  "resizeCorner" { $x = $R - 9;               $y = $B - 9 }
}

$rect0 = [DragProbe2]::RectStr($hwnd)

$baseFrozen = 0
for ($i = 0; $i -lt 10; $i++) {
  if (-not [DragProbe2]::Alive($hwnd, [uint32]$HangTimeoutMs)) { $baseFrozen++ }
  Start-Sleep -Milliseconds $Step
}

[void][DragProbe2]::SetCursorPos($x, $y)
Start-Sleep -Milliseconds 60
[DragProbe2]::mouse_event(0x0002, 0, 0, 0, [IntPtr]::Zero)
Start-Sleep -Milliseconds 40

$frozen = 0; $maxWait = 0; $lateRects = 0; $rectChanges = 0; $lastRect = $rect0
for ($i = 0; $i -lt $Steps; $i++) {
  $x += $Dx; $y += $Dy
  [void][DragProbe2]::SetCursorPos($x, $y)
  Start-Sleep -Milliseconds $Step
  $t0 = Get-Date
  $alive = $true
  if (($i % $ProbeEvery) -eq 0) { $alive = [DragProbe2]::Alive($hwnd, [uint32]$HangTimeoutMs) }
  $wait = ((Get-Date) - $t0).TotalMilliseconds
  if (-not $alive) { $frozen++; if ($wait -gt $maxWait) { $maxWait = $wait } }
  $moved = [DragProbe2]::RectStr($hwnd)
  if ($moved -ne $lastRect) { $rectChanges++; $lastRect = $moved }
}

[DragProbe2]::mouse_event(0x0004, 0, 0, 0, [IntPtr]::Zero)
# 松手之后再盯一会儿：如果拖动期间往主线程队列里堆了活儿，卡顿会出现在松手后。
$postFrozen = 0; $postMax = 0
$deadline = (Get-Date).AddMilliseconds($WatchAfterMs)
while ((Get-Date) -lt $deadline) {
  $t0 = Get-Date
  if (-not [DragProbe2]::Alive($hwnd, [uint32]$HangTimeoutMs)) {
    $postFrozen++
    $w = ((Get-Date) - $t0).TotalMilliseconds
    if ($w -gt $postMax) { $postMax = $w }
  }
  Start-Sleep -Milliseconds 60
}
Start-Sleep -Milliseconds 900
$rect1 = [DragProbe2]::RectStr($hwnd)

Write-Output ("模式 $Mode，起点 $x,$y")
Write-Output ("窗口矩形: $rect0 → $rect1（鼠标走了 " + ($Dx*$Steps) + "," + ($Dy*$Steps) + "），矩形变了 $rectChanges 次")
Write-Output ("静止基线卡死: $baseFrozen/10")
Write-Output ("拖动 $Steps 步: 卡死 $frozen 次，最长一次等了 " + [math]::Round($maxWait) + "ms（阈值 ${HangTimeoutMs}ms）")
Write-Output ("松手后 ${WatchAfterMs}ms 内卡死: $postFrozen 次，最长 " + [math]::Round($postMax) + "ms")

