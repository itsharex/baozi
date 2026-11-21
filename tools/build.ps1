# build.ps1 —— 本地构建，参数与 Release 工作流保持一致。
#
# 为什么需要它：随手 `go build ./cmd/baozi` 出来的 exe 和发布版差两处：
#   1. 没有 -H=windowsgui，PE 子系统是 Console（3）：启动会带一个控制台窗口，
#      看起来就像"调试版"；
#   2. 没有注入 version.Version，关于页显示"开发版"。
# 这里把两处都补上，版本号默认取 `git describe --tags --long`（例如
# v0.4.1-8-g24630f2），既能看出是在哪个 tag 之后编的，也不会冒充正式版。
#
# 用法：
#   pwsh -NoProfile -File tools\build.ps1
#   pwsh -NoProfile -File tools\build.ps1 -Version v0.4.2
param(
    [string]$Version = ""
)

$ErrorActionPreference = 'Stop'
$root = Split-Path $PSScriptRoot -Parent
Push-Location $root
try {
    if (-not $Version) {
        $Version = (git describe --tags --long --dirty 2>$null)
        if (-not $Version) { $Version = 'dev' }
    }
    New-Item -ItemType Directory -Force dist | Out-Null
    $guiLd = "-H=windowsgui -s -w -X baozi/internal/version.Version=$Version"
    & go build -trimpath "-ldflags=$guiLd" -o dist\baozi.exe ./cmd/baozi
    if ($LASTEXITCODE -ne 0) { throw "编译 baozi.exe 失败" }
    & go build -trimpath "-ldflags=-s -w" -o dist\baozi-hook.exe ./cmd/baozi-hook
    if ($LASTEXITCODE -ne 0) { throw "编译 baozi-hook.exe 失败" }
    & go build -trimpath "-ldflags=-s -w" -o dist\baozi-index.exe ./cmd/baozi-index
    if ($LASTEXITCODE -ne 0) { throw "编译 baozi-index.exe 失败" }

    # 自检：PE 子系统必须是 GUI(2)，否则等于又编出一个带控制台的版本。
    $p = Join-Path $root 'dist\baozi.exe'
    $fs = [IO.File]::OpenRead($p)
    try {
        $br = New-Object IO.BinaryReader($fs)
        $fs.Position = 0x3C
        $pe = $br.ReadInt32()
        $fs.Position = $pe + 4 + 20 + 68
        $sub = $br.ReadUInt16()
    } finally { $fs.Close() }
    if ($sub -ne 2) { throw "baozi.exe 的 PE 子系统是 $sub（应为 2=GUI）" }

    Write-Host "版本号：$Version"
    Get-ChildItem dist\*.exe | Select-Object Name, Length
} finally {
    Pop-Location
}
