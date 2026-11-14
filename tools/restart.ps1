# 重启 Baozi，并且让它挂不到当前这个终端上。
#
# 为什么不能用 Start-Process：那样起来的进程是 PowerShell 的子进程，而 PowerShell
# 又是宿主（豆包工作 / 编辑器 / IDE 终端）的子进程。宿主退出时会把自己作业对象里的
# 整棵进程树一起带走（JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE），于是"关掉宿主，Baozi 也
# 跟着没了"。Win32_Process.Create 是让 WMI 服务去起进程，父进程变成 WmiPrvSE，
# 和这个终端没有父子关系，宿主怎么关都波及不到它。
#
# 用法：
#   pwsh -File tools\restart.ps1              # 重启 dist\baozi.exe
#   pwsh -File tools\restart.ps1 -Exe path    # 指定 exe
#   pwsh -File tools\restart.ps1 -ShowLog   # 起来后顺手打日志尾巴

[CmdletBinding()]
param(
    [string]$Exe,
    [switch]$ShowLog
)

$ErrorActionPreference = 'Stop'

if (-not $Exe) {
    $Exe = Join-Path (Split-Path -Parent $PSScriptRoot) 'dist\baozi.exe'
}
$full = (Resolve-Path -LiteralPath $Exe).Path
if (-not (Test-Path -LiteralPath $full -PathType Leaf)) {
    throw "找不到 $full"
}
$dir = Split-Path -Parent $full

# 1. 先把在跑的收掉（钩子进程单独一个 exe，也必须一起退，否则双击 Ctrl 会唤不出新面板）
$old = @(Get-Process -Name baozi, baozi-hook -ErrorAction SilentlyContinue)
if ($old.Count -gt 0) {
    $old | Stop-Process -Force -ErrorAction SilentlyContinue
    Start-Sleep -Seconds 2
    Write-Host ("已停止旧进程：" + (($old | ForEach-Object { "$($_.ProcessName)#$($_.Id)" }) -join '、'))
}

# 2. 让 WMI 起进程 —— 这一步是"脱离宿主"的关键
$res = Invoke-CimMethod -ClassName Win32_Process -MethodName Create -Arguments @{
    CommandLine      = '"' + $full + '"'
    CurrentDirectory = $dir
}
if ($res.ReturnValue -ne 0) {
    throw "WMI 起进程失败，ReturnValue=$($res.ReturnValue)（5=拒绝访问，多半是权限或策略限制）"
}

Start-Sleep -Seconds 3
$new = Get-Process -Name baozi -ErrorAction SilentlyContinue | Select-Object -First 1
if (-not $new) {
    throw '进程起来了又没了，去看日志'
}
$parent = (Get-CimInstance Win32_Process -Filter "ProcessId=$($new.Id)").ParentProcessId
$parentName = (Get-CimInstance Win32_Process -Filter "ProcessId=$parent" -ErrorAction SilentlyContinue).Name
Write-Host "已启动 baozi#$($new.Id)，父进程 $parentName#$parent（不是当前终端，宿主退出不会带走它）"

if ($ShowLog) {
    # 变量别叫 $log：参数 $ShowLog 之外的 $Log 早先是 switch，和字符串同名会撞类型。
    $logPath = Join-Path $env:LOCALAPPDATA 'Baozi\baozi.log'
    if (Test-Path -LiteralPath $logPath) {
        Get-Content -LiteralPath $logPath -Encoding utf8 -Tail 6
    }
}
