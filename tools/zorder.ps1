# 一次性探针：看 baozi 的窗口在 z 序里排在哪、阴影有没有紧跟宿主。
# 只读窗口元信息和像素取样，不截屏（用户明确要求不用截图取证）。
#
# 用法：
#   pwsh -NoProfile -File tools\zorder.ps1            # 只打印当前 z 序
#   pwsh -NoProfile -File tools\zorder.ps1 -Show      # 先模拟双击 Ctrl 把面板唤出来
#   pwsh -NoProfile -File tools\zorder.ps1 -Sample    # 在面板左边缘取几个像素
param([switch]$Show, [switch]$Sample)

Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;
using System.Text;
public class WinZ3 {
  [StructLayout(LayoutKind.Sequential)] public struct RECT { public int L, T, R, B; }
  [DllImport("user32.dll")] static extern bool EnumWindows(EnumProc cb, IntPtr p);
  delegate bool EnumProc(IntPtr h, IntPtr p);
  [DllImport("user32.dll", CharSet=CharSet.Unicode)] static extern int GetClassName(IntPtr h, StringBuilder s, int n);
  [DllImport("user32.dll")] static extern bool GetWindowRect(IntPtr h, out RECT r);
  [DllImport("user32.dll")] static extern IntPtr GetWindow(IntPtr h, uint cmd);
  [DllImport("user32.dll")] static extern int GetWindowLong(IntPtr h, int idx);
  [DllImport("user32.dll")] static extern int GetWindowThreadProcessId(IntPtr h, out int pid);
  [DllImport("user32.dll")] static extern bool IsWindowVisible(IntPtr h);
  [DllImport("user32.dll")] static extern void keybd_event(byte vk, byte scan, uint flags, UIntPtr extra);
  public static string Cls(IntPtr h) { var sb = new StringBuilder(64); GetClassName(h, sb, 64); return sb.ToString(); }
  public static string ZOrder(int pid) {
    var sb = new StringBuilder(); int idx = 0;
    EnumWindows(delegate(IntPtr h, IntPtr p) {
      int pid2; GetWindowThreadProcessId(h, out pid2);
      if (pid2 == pid) {
        RECT r; GetWindowRect(h, out r);
        var owner = GetWindow(h, 4);
        sb.Append(idx.ToString().PadLeft(3) + "  " + h.ToString("X8") + "  " + Cls(h).PadRight(18) + " ex=0x" + GetWindowLong(h,-20).ToString("X4") + " vis=" + IsWindowVisible(h) + " owner=" + (owner==IntPtr.Zero? "-" : Cls(owner)) + " [" + r.L + "," + r.T + "," + r.R + "," + r.B + "]" + (Cls(h)=="BaoziShadowWnd" ? "  <-- 阴影" : "") + "\n");
      }
      idx++; return true;
    }, IntPtr.Zero);
    return sb.ToString();
  }
  public static IntPtr FindCls(int pid, string cls) {
    IntPtr found = IntPtr.Zero;
    EnumWindows(delegate(IntPtr h, IntPtr p) {
      int pid2; GetWindowThreadProcessId(h, out pid2);
      if (pid2 == pid && Cls(h) == cls) { found = h; return false; }
      return true;
    }, IntPtr.Zero);
    return found;
  }
  public static string Neighbor(IntPtr h, uint cmd) {
    var n = GetWindow(h, cmd);
    return (n==IntPtr.Zero? "0" : n.ToString("X8")) + " " + (n==IntPtr.Zero? "-" : Cls(n));
  }
  public static int[] Rect(IntPtr h) {
    RECT r; GetWindowRect(h, out r);
    return new int[] { r.L, r.T, r.R, r.B };
  }
  public static void CtrlTwice() {
    for (int i = 0; i < 2; i++) {
      keybd_event(0x11, 0, 0, UIntPtr.Zero); System.Threading.Thread.Sleep(60);
      keybd_event(0x11, 0, 2, UIntPtr.Zero); System.Threading.Thread.Sleep(120);
    }
  }
}
'@

$proc = (Get-Process baozi -ErrorAction SilentlyContinue | Select-Object -First 1)
if (-not $proc) { throw 'baozi 没在跑' }

if ($Show) { [WinZ3]::CtrlTwice(); Start-Sleep -Milliseconds 900 }

[WinZ3]::ZOrder([int]$proc.Id)

if ($Sample) {
  $panel = [WinZ3]::FindCls([int]$proc.Id, 'MyGoWindow')
  $pr = [WinZ3]::Rect($panel)
  Add-Type -AssemblyName System.Drawing
  $pad = 140
  $w = ($pr[2] - $pr[0]) + $pad * 2; $h = ($pr[3] - $pr[1]) + $pad * 2
  $bmp = New-Object System.Drawing.Bitmap $w, $h
  $g = [System.Drawing.Graphics]::FromImage($bmp)
  $g.CopyFromScreen($pr[0] - $pad, $pr[1] - $pad, 0, 0, (New-Object System.Drawing.Size $w, $h))
  # 面板左边缘外侧：6px 在阴影圈里（margin 16 DIP），40px 已经在圈外。
  $midY = [int](($pr[1] + $pr[3]) / 2 - ($pr[1] - $pad))
  $c1 = $bmp.GetPixel(($pr[0] - 10) - ($pr[0] - $pad), $midY)
  $c2 = $bmp.GetPixel(($pr[0] - 40) - ($pr[0] - $pad), $midY)
  "左边缘外 10px（影内）: $($c1.R),$($c1.G),$($c1.B)"
  "左边缘外 40px（影外）: $($c2.R),$($c2.G),$($c2.B)"
  "亮度差（正数=影内更暗）: $([int]$c2.R - $c1.R)"
  # 对照：面板内部（玻璃）、以及屏幕左上角。用来判断"整屏全白"是采集不到合成，
  # 还是屏幕上真的什么都没有。
  $c3 = $bmp.GetPixel([int](($pr[0] + $pr[2]) / 2 - ($pr[0] - $pad)), $midY)
  $c4 = $bmp.GetPixel(10, 10)
  "面板内部（中心）: $($c3.R),$($c3.G),$($c3.B)"
  "屏幕左上角 10,10 : $($c4.R),$($c4.G),$($c4.B)"
  $g.Dispose(); $bmp.Dispose()
}
