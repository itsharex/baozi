# resize_shot.ps1 —— 在"缩放正在进行"的瞬间截一张图，用来确认缩放过程中
# 四角仍然是圆的、阴影还在。
#
# 做法：子进程跑 resize 的帧序列，父进程等半拍再截屏（截屏脚本是 tools\shot.ps1，
# 它发现面板已经可见就不会再按 Ctrl 唤出，不会打乱状态）。
param(
  [int]$DelayMs = 900
)
$frames = {
  param($d)
  Start-Sleep -Milliseconds $d
  pwsh -NoProfile -File "D:\work\baozi\tools\resize.ps1" | Out-Null
}
Start-Job -ScriptBlock $frames -ArgumentList 200 | Out-Null
Start-Sleep -Milliseconds $DelayMs
pwsh -NoProfile -File "D:\work\baozi\tools\shot.ps1"
Copy-Item "D:\work\baozi\dist\shot_panel.png" "D:\work\baozi\dist\shot_resizing.png" -Force
Get-Job | Wait-Job | Remove-Job -Force
Write-Output "shot=D:\work\baozi\dist\shot_resizing.png"

