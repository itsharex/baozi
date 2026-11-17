# shot.ps1 —— 唤出面板并按窗口矩形外扩 pad 像素截屏，存成 dist\shot_panel.png
#
# 只做两件事：判断面板是否已经可见（可见就不动，免得双击 Ctrl 把它切回去），
# 然后用 System.Drawing 的 CopyFromScreen 抓图。分层窗口（自绘阴影层）只有
# 走 DWM 合成才看得见，GetPixel/BitBlt 拍不到 —— 所以验证渲染一律用这个脚本。
#
# 用法：pwsh -NoProfile -File D:\work\baozi\tools\shot.ps1 [-Pad 40]
param([int]$Pad = 40)

Add-Type @'
using System;
using System.Text;
using System.Threading;
using System.Runtime.InteropServices;
public class BaoziShot {
  [StructLayout(LayoutKind.Sequential)] public struct RECT { public int Left, Top, Right, Bottom; }
  public delegate bool EnumProc(IntPtr h, IntPtr l);
  [DllImport("user32.dll")] public static extern bool SetProcessDPIAware();
  [DllImport("user32.dll")] public static extern bool EnumWindows(EnumProc cb, IntPtr l);
  [DllImport("user32.dll", CharSet=CharSet.Unicode)] public static extern int GetWindowText(IntPtr h, StringBuilder s, int n);
  [DllImport("user32.dll")] public static extern uint GetWindowThreadProcessId(IntPtr h, out uint pid);
  [DllImport("user32.dll")] public static extern bool GetWindowRect(IntPtr h, out RECT r);
  [DllImport("user32.dll")] public static extern bool IsWindowVisible(IntPtr h);
  [DllImport("user32.dll")] public static extern void keybd_event(byte vk, byte scan, uint flags, uint extra);

  static IntPtr FindPanel(uint pid) {
    IntPtr P = IntPtr.Zero;
    EnumWindows((h, l) => {
      uint p; GetWindowThreadProcessId(h, out p); if (p != pid) return true;
      var t = new StringBuilder(256); GetWindowText(h, t, 256);
      if (t.ToString() == "Baozi" && IsWindowVisible(h)) { P = h; return false; }
      return true;
    }, IntPtr.Zero);
    return P;
  }

  public static string Ensure(uint pid, bool summonIfHidden) {
    SetProcessDPIAware();
    IntPtr P = FindPanel(pid);
    if (P == IntPtr.Zero && summonIfHidden) {
      keybd_event(0x11,0,0,0); Thread.Sleep(40); keybd_event(0x11,0,2,0);
      Thread.Sleep(240);
      keybd_event(0x11,0,0,0); Thread.Sleep(40); keybd_event(0x11,0,2,0);
      Thread.Sleep(3800);
      P = FindPanel(pid);
    }
    if (P == IntPtr.Zero) return "0,0,0,0";
    RECT r; GetWindowRect(P, out r);
    return r.Left + "," + r.Top + "," + r.Right + "," + r.Bottom;
  }
}
'@

$pidBaozi = [uint32](Get-Process baozi).Id
$rect = [BaoziShot]::Ensure($pidBaozi, $true)
$parts = $rect -split ','
if ($parts[2] -eq '0') { Write-Output "panel=not-visible"; exit 1 }
$l = [int]$parts[0]; $t = [int]$parts[1]; $r = [int]$parts[2]; $b = [int]$parts[3]
$x = [Math]::Max(0, $l - $Pad); $y = [Math]::Max(0, $t - $Pad)
$w = ($r - $l) + $Pad * 2; $h = ($b - $t) + $Pad * 2

Add-Type -AssemblyName System.Drawing
$bmp = New-Object System.Drawing.Bitmap $w, $h
$g = [System.Drawing.Graphics]::FromImage($bmp)
$g.CopyFromScreen($x, $y, 0, 0, $bmp.Size)
$g.Dispose()
$out = "D:\work\baozi\dist\shot_panel.png"
$bmp.Save($out, [System.Drawing.Imaging.ImageFormat]::Png)
$bmp.Dispose()
Write-Output "panel=$rect shot=$out ${w}x${h}"
