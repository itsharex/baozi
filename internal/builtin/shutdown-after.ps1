param(
    [Parameter(Position = 0)][string]$Minutes,
    [switch]$Cancel,
    [switch]$Sleep,
    [switch]$DryRun
)

# 给 Baozi 的「定时关机 / 睡眠」做参数换算：shutdown 的 /t 要秒，这里收分钟。
#
# 两种动作：
#   默认    —— 关机，走系统自带倒计时（shutdown /s /t），能用 shutdown /a 取消
#   -Sleep  —— 睡眠 / 待机（S3）：内存保持、屏幕关闭，键鼠唤醒
#
# 不提供真休眠（shutdown /h，写盘掉电）：这台机器休眠被系统禁用
# （powercfg /a 显示"内部系统组件已禁用休眠。从 VHD 启动"，HibernateEnabled=0），
# 做了也用不了，索性不摆出来。
#
# 关机的倒计时由系统做；睡眠没有倒计时参数，只能自己等，等待中的进程号写进
# pending-power.pid，"取消关机"按它杀。
#
# 系统程序一律用绝对路径：Baozi 可能由 Run 键 / 托盘启动，PATH 未必带 System32，
# 按裸名调用会变成"CommandNotFoundException"，表现就是敲了命令什么都没发生。

$pidFile = Join-Path $env:LOCALAPPDATA "Baozi\pending-power.pid"
$sysDir = Join-Path $env:SystemRoot "System32"
$shutdownExe = Join-Path $sysDir "shutdown.exe"
$rundll32Exe = Join-Path $sysDir "rundll32.exe"

function Show-Message([string]$text, [string]$title, [string]$icon) {
    if ($DryRun) { Write-Output $text; return }
    Add-Type -AssemblyName PresentationFramework
    [void][System.Windows.MessageBox]::Show($text, $title, "OK", $icon)
}

function Show-Usage {
    Show-Message "用法：`r`n  关机 <分钟数>    例如 shutdown 30 —— 30 分钟后关机`r`n  睡眠             立即睡眠`r`n  睡眠 <分钟数>    例如 睡眠 30 —— 30 分钟后睡眠`r`n取消（关机和睡眠都算）：取消关机" "Baozi 定时关机" "Warning"
}

function Stop-Pending {
    if (-not (Test-Path -LiteralPath $pidFile)) { return $false }
    $raw = (Get-Content -LiteralPath $pidFile -Raw).Trim()
    Remove-Item -LiteralPath $pidFile -ErrorAction SilentlyContinue
    $old = 0
    if (-not [int]::TryParse($raw, [ref]$old)) { return $false }
    try { Stop-Process -Id $old -Force -ErrorAction Stop } catch { return $false }
    return $true
}

function Enter-Sleep {
    if ($DryRun) { Write-Output "rundll32.exe powrprof.dll,SetSuspendState 0,1,0   （睡眠/待机）"; return }
    & $rundll32Exe powrprof.dll,SetSuspendState 0,1,0
}

if ($Cancel) {
    if ($DryRun) {
        Write-Output "shutdown.exe /a"
        if (Test-Path -LiteralPath $pidFile) {
            Write-Output ("以及杀掉等待中的睡眠进程 PID={0}" -f (Get-Content -LiteralPath $pidFile -Raw))
        }
        exit 0
    }
    $killed = Stop-Pending
    & $shutdownExe /a
    # 杀掉等待中的睡眠之后，shutdown /a 必然报"没有关机计划"，
    # 那种情况是取消成功，不该弹框说没东西可取消。
    if ($LASTEXITCODE -ne 0 -and -not $killed) {
        Show-Message "没有正在进行中的关机，也没有等待中的睡眠（shutdown /a 返回 $LASTEXITCODE）。" "Baozi 取消关机" "Information"
    }
    exit 0
}

# 睡眠：不给分钟就立即睡，给了分钟就等着
if ($Sleep) {
    if ([string]::IsNullOrWhiteSpace($Minutes)) {
        Enter-Sleep
        exit 0
    }
    $min = 0
    if (-not [int]::TryParse($Minutes, [ref]$min) -or $min -le 0 -or $min -gt 1440) {
        Show-Usage
        exit 2
    }
    if ($DryRun) {
        Write-Output ("等待 {0} 秒后睡眠（{1} 分钟后）" -f ($min * 60), $min)
        exit 0
    }
    New-Item -ItemType Directory -Force -Path (Split-Path -Parent $pidFile) | Out-Null
    Set-Content -LiteralPath $pidFile -Value $PID -Encoding ascii
    Start-Sleep -Seconds ($min * 60)
    Remove-Item -LiteralPath $pidFile -ErrorAction SilentlyContinue
    Enter-Sleep
    exit 0
}

# 关机：不给分钟就只显示用法，绝不裸关机
$min2 = 0
if ([string]::IsNullOrWhiteSpace($Minutes) -or
    -not [int]::TryParse($Minutes, [ref]$min2) -or
    $min2 -le 0 -or $min2 -gt 525600) {
    Show-Usage
    exit 2
}

$sec = $min2 * 60
if ($DryRun) {
    Write-Output ("shutdown.exe /s /t {0}   （{1} 分钟后关机）" -f $sec, $min2)
    exit 0
}
& $shutdownExe /s /t $sec
exit $LASTEXITCODE