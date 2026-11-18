# move_shot.ps1 —— 把面板整体挪动一段距离，挪动过程中截一张图；
# 用来确认"拖动时四角的形状和阴影"（不是缩放，是移动）。
#
# 注意：pwsh 的 Start-Job 是另一个 runspace，Add-Type 造的类不共享 ——
# 所以截屏交给子进程（Start-Process），滑动留在本进程里跑。
param(
  [int]$Dx = 160,
  [int]$Dy = 110,
  [int]$DelayMs = 600
)
Add-Type @'
using System;
using System.Text;
using System.Threading;
using System.Runtime.InteropServices;
public class BaoziMove2 {
  [StructLayout(LayoutKind.Sequential)] public struct RECT { public int Left, Top, Right, Bottom; }
  public delegate bool EnumProc(IntPtr h, IntPtr l);
  [DllImport("user32.dll")] public static extern bool SetProcessDPIAware();
  [DllImport("user32.dll")] public static extern bool EnumWindows(EnumProc cb, IntPtr l);
  [DllImport("user32.dll", CharSet=CharSet.Unicode)] public static extern int GetWindowText(IntPtr h, StringBuilder s, int n);
  [DllImport("user32.dll")] public static extern uint GetWindowThreadProcessId(IntPtr h, out uint pid);
  [DllImport("user32.dll")] public static extern bool GetWindowRect(IntPtr h, out RECT r);
  [DllImport("user32.dll")] public static extern bool IsWindowVisible(IntPtr h);
  [DllImport("user32.dll")] public static extern bool SetWindowPos(IntPtr h, IntPtr after, int x, int y, int cx, int cy, uint flags);
  static IntPtr Find(uint pid) {
    SetProcessDPIAware();
    IntPtr P = IntPtr.Zero;
    EnumWindows((h, l) => { uint p; GetWindowThreadProcessId(h, out p); if (p != pid) return true;
      var t = new StringBuilder(256); GetWindowText(h, t, 256);
      if (t.ToString() == "Baozi" && IsWindowVisible(h)) { P = h; return false; }
      return true; }, IntPtr.Zero);
    return P;
  }
  public static string Now(uint pid) {
    var P = Find(pid); if (P == IntPtr.Zero) return "";
    RECT r; GetWindowRect(P, out r);
    return r.Left + "," + r.Top + "," + r.Right + "," + r.Bottom;
  }
  public static void Glide(uint pid, int n, int dx, int dy) {
    var P = Find(pid); if (P == IntPtr.Zero) return;
    RECT r; GetWindowRect(P, out r);
    int w = r.Right - r.Left, h = r.Bottom - r.Top, x = r.Left, y = r.Top;
    for (int i = 0; i < n; i++) {
      x += dx; y += dy;
      SetWindowPos(P, IntPtr.Zero, x, y, w, h, 0x0004);
      Thread.Sleep(16);
    }
  }
}
'@
$pidB = [uint32]((Get-Process -Name baozi,baozi.debug -ErrorAction SilentlyContinue | Select-Object -First 1).Id)
Write-Output ("before=" + [BaoziMove2]::Now($pidB))
# 子进程半拍后截屏（截到"正在滑"的那一帧）
$shot = Start-Process pwsh -ArgumentList '-NoProfile','-Command',"Start-Sleep -Milliseconds $DelayMs; pwsh -NoProfile -File 'D:\work\baozi\tools\shot.ps1'; Copy-Item 'D:\work\baozi\dist\shot_panel.png' 'D:\work\baozi\dist\shot_moving.png' -Force" -PassThru -WindowStyle Hidden
Start-Sleep -Milliseconds 150
[BaoziMove2]::Glide($pidB, 20, $Dx/20, $Dy/20)
$shot | Wait-Process -Timeout 20
Write-Output ("after=" + [BaoziMove2]::Now($pidB))
Write-Output "shot=D:\work\baozi\dist\shot_moving.png"

