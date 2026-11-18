# toggle.ps1 —— 用合成的"双击 Ctrl"唤出/收起面板（hook 认的是这个手势）。
#
# 用法：
#   pwsh -NoProfile -File D:\work\baozi\tools\toggle.ps1          # 一次（显示或收起）
#   pwsh -NoProfile -File D:\work\baozi\tools\toggle.ps1 -Times 6  # 连按 6 次
#   pwsh -NoProfile -File D:\work\baozi\tools\toggle.ps1 -GapMs 90 # 两次按之间只隔 90ms
#
# 为什么要它：面板"偶尔出现四个直角"只在这类快速连按里复现，而真实按键没法脚本化。
param(
    [int]$Times = 1,
    [int]$GapMs = 150
)

Add-Type @"
using System;
using System.Runtime.InteropServices;
public class BaoziKey {
    [DllImport("user32.dll")] public static extern void keybd_event(byte bVk, byte bScan, uint dwFlags, UIntPtr dwExtraInfo);
}
"@

function Tap-Ctrl {
    [BaoziKey]::keybd_event(0x11, 0, 0, [UIntPtr]::Zero)      # Ctrl down
    Start-Sleep -Milliseconds 30
    [BaoziKey]::keybd_event(0x11, 0, 2, [UIntPtr]::Zero)      # Ctrl up
}

for ($i = 0; $i -lt $Times; $i++) {
    # 一次"双击"：两个 Ctrl，间隔要小于 hook 认的 400ms
    Tap-Ctrl
    Start-Sleep -Milliseconds 120
    Tap-Ctrl
    Start-Sleep -Milliseconds $GapMs
}
