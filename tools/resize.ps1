# resize.ps1 —— 用 SetWindowPos 连续改面板尺寸，量"缩放回调"的耗时，
# 并在过程中截一张图（用来确认缩放的**过程中**四角仍然是圆的）。
#
# 用法：pwsh -NoProfile -File D:\work\baozi\tools\resize.ps1
Add-Type @'
using System;
using System.Text;
using System.Threading;
using System.Runtime.InteropServices;
public class BaoziResize {
  [StructLayout(LayoutKind.Sequential)] public struct RECT { public int Left, Top, Right, Bottom; }
  public delegate bool EnumProc(IntPtr h, IntPtr l);
  [DllImport("user32.dll")] public static extern bool SetProcessDPIAware();
  [DllImport("user32.dll")] public static extern bool EnumWindows(EnumProc cb, IntPtr l);
  [DllImport("user32.dll", CharSet=CharSet.Unicode)] public static extern int GetWindowText(IntPtr h, StringBuilder s, int n);
  [DllImport("user32.dll")] public static extern uint GetWindowThreadProcessId(IntPtr h, out uint pid);
  [DllImport("user32.dll")] public static extern bool GetWindowRect(IntPtr h, out RECT r);
  [DllImport("user32.dll")] public static extern bool IsWindowVisible(IntPtr h);
  [DllImport("user32.dll")] public static extern bool SetWindowPos(IntPtr h, IntPtr after, int x, int y, int cx, int cy, uint flags);
  public static string Rect(uint pid) {
    SetProcessDPIAware();
    IntPtr P = IntPtr.Zero;
    EnumWindows((h, l) => { uint p; GetWindowThreadProcessId(h, out p); if (p != pid) return true;
      var t = new StringBuilder(256); GetWindowText(h, t, 256);
      if (t.ToString() == "Baozi" && IsWindowVisible(h)) { P = h; return false; }
      return true; }, IntPtr.Zero);
    if (P == IntPtr.Zero) return "";
    RECT r; GetWindowRect(P, out r);
    return r.Left + "," + r.Top + "," + r.Right + "," + r.Bottom;
  }
  public static void Frames(uint pid, int n, int dw, int dh) {
    SetProcessDPIAware();
    IntPtr P = IntPtr.Zero;
    EnumWindows((h, l) => { uint p; GetWindowThreadProcessId(h, out p); if (p != pid) return true;
      var t = new StringBuilder(256); GetWindowText(h, t, 256);
      if (t.ToString() == "Baozi" && IsWindowVisible(h)) { P = h; return false; }
      return true; }, IntPtr.Zero);
    if (P == IntPtr.Zero) return;
    RECT r; GetWindowRect(P, out r);
    int w = r.Right - r.Left, h = r.Bottom - r.Top;
    for (int i = 0; i < n; i++) {
      w += dw; h += dh;
      SetWindowPos(P, IntPtr.Zero, r.Left, r.Top, w, h, 0x0004); // SWP_NOZORDER
      Thread.Sleep(22);
    }
  }
}
'@

$pidBaozi = [uint32]((Get-Process -Name baozi,baozi.debug -ErrorAction SilentlyContinue | Select-Object -First 1).Id)
$before = [BaoziResize]::Rect($pidBaozi)
Write-Output "before=$before"
# 先放大 16 帧、再缩回 16 帧
[BaoziResize]::Frames($pidBaozi, 16, 24, 16)
Start-Sleep -Milliseconds 700
[BaoziResize]::Frames($pidBaozi, 16, -24, -16)
Start-Sleep -Milliseconds 300
$after = [BaoziResize]::Rect($pidBaozi)
Write-Output "after=$after"
Write-Output (Get-Content -LiteralPath "$env:LOCALAPPDATA\Baozi\baozi.log" -Encoding utf8 | Select-String "缩放:" | Select-Object -Last 20)


