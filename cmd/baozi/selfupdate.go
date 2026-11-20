package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"baozi/internal/update"
)

// installUpdate 是"下载并更新"这件事的编排：下载 → 解压 → 交一个 PowerShell
// 脚本去覆盖 → 自己退出。
//
// 为什么覆盖必须交给另一个进程：Windows 不允许改正在运行的可执行文件，
// 而我们自己就是那个可执行文件。脚本干四件事：
//
//  1. 等本进程退出（最多等 15 秒，等不到就直接 taskkill）；
//  2. 把 baozi / hook / index 三个进程都停掉（index 若是服务，尝试停服务）；
//  3. 备份旧的 exe（*.old），再把新的复制过去；
//  4. 重新拉起 baozi.exe。
//
// 整条链路每一步都写进 %LOCALAPPDATA%\Baozi\update.log，"关于"页下次启动会读它 ——
// 更新失败时用户至少能看到失败在哪一步，而不是"点了按钮，然后什么都没发生"。
func (a *application) installUpdate(snap updateSnapshot) {
	if !a.upd.beginInstall() {
		return
	}
	go func() {
		defer a.upd.endInstall()

		if snap.ZipURL == "" {
			a.setUpdProgress("安装包里没有 Windows 版本，只能打开下载页")
			return
		}
		exe, err := os.Executable()
		if err != nil {
			a.setUpdProgress("定位自身失败：" + err.Error())
			return
		}
		if !strings.EqualFold(filepath.Base(exe), "baozi.exe") {
			// 调试版/改名版不参与自更新：脚本按三个固定文件名覆盖，
			// 对着 baozi.debug.exe 覆盖等于把用户换回发布版，太容易误解。
			a.setUpdProgress("当前不是发布版（" + filepath.Base(exe) + "），请用 zip 替换")
			return
		}
		installDir := filepath.Dir(exe)

		stage, err := update.StageDir()
		if err != nil {
			a.setUpdProgress("建临时目录失败：" + err.Error())
			return
		}

		a.setUpdProgress("正在下载 " + snap.Tag + "…")
		zipPath := filepath.Join(stage, "update.zip")
		// 下载给 5 分钟：这个包十来兆，但用户可能正在走代理/慢网。
		dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Minute)
		n, err := update.Download(dctx, snap.ZipURL, zipPath)
		dcancel()
		if err != nil {
			a.setUpdProgress("下载失败：" + err.Error())
			writeUpdateLog("下载失败：%v", err)
			return
		}
		writeUpdateLog("下载完成：%d 字节 → %s", n, zipPath)

		a.setUpdProgress("正在解压…")
		if _, err := update.ExtractZip(zipPath, stage); err != nil {
			a.setUpdProgress("解压失败：" + err.Error())
			writeUpdateLog("解压失败：%v", err)
			return
		}

		script := filepath.Join(stage, "apply-update.ps1")
		// 必须带 UTF-8 BOM：Windows PowerShell 5.1 读 .ps1 时，没有 BOM 就按 ANSI(GBK)
		// 解码。脚本里全是中文，一旦按 GBK 读，引号会被中文的第二个字节吃掉，
		// 整个脚本变成一堆语法错误 —— 更新就"点了没反应"。（干跑时真踩到了。）
		if err := os.WriteFile(script, append([]byte("\xef\xbb\xbf"), updateScript(installDir, stage, os.Getpid())...), 0o644); err != nil {
			a.setUpdProgress("写更新脚本失败：" + err.Error())
			writeUpdateLog("写脚本失败：%v", err)
			return
		}
		writeUpdateLog("准备覆盖：%s → %s（新版本 %s）", stage, installDir, snap.Tag)

		a.setUpdProgress("正在退出并替换文件…")
		// 隐藏窗口、独立进程组：脚本要在我们退出之后继续活下去。
		cmd := exec.Command("powershell.exe",
			"-NoProfile", "-ExecutionPolicy", "Bypass", "-WindowStyle", "Hidden",
			"-File", script)
		cmd.SysProcAttr = &syscall.SysProcAttr{
			HideWindow:    true,
			CreationFlags: 0x00000008 | 0x00000200, // DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP
		}
		if err := cmd.Start(); err != nil {
			a.setUpdProgress("拉起更新脚本失败：" + err.Error())
			writeUpdateLog("拉起脚本失败：%v", err)
			return
		}
		_ = cmd.Process.Release()
		writeUpdateLog("更新脚本已启动（pid=%d），本进程退出", cmd.Process.Pid)

		// 给界面留一帧把"正在替换"写出去，然后正常退出（走 shutdown，落盘历史/位置）。
		time.AfterFunc(600*time.Millisecond, a.quit)
	}()
}

// updateScript 生成那段"等退出 → 停进程 → 备份 → 覆盖 → 重启"的 PowerShell。
//
// 用单引号字符串包路径：PowerShell 单引号里不展开变量，路径里有 $ 或空格都安全；
// 单引号本身用两个单引号转义。
func updateScript(installDir, stage string, pid int) string {
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	logPath := filepath.Join(os.Getenv("LOCALAPPDATA"), "Baozi", "update.log")
	return fmt.Sprintf(`
$ErrorActionPreference = 'Continue'
$install = %s
$stage   = %s
$log     = %s
$waitPid = %d

function Say($msg) {
  $line = (Get-Date).ToString('yyyy-MM-dd HH:mm:ss') + '  ' + $msg
  # 用 .NET 直接追加：PowerShell 5.1 的 Add-Content -Encoding UTF8 每次追加都会再写
  # 一个 BOM，日志中段就会出现乱码字节，主程序按 UTF-8 读回去会读到坏行。
  # 目录也可能还不存在（全新机器上 profile 目录是空的），先补上再写。
  try {
    $dir = Split-Path -Parent $log
    if (-not (Test-Path -LiteralPath $dir)) { New-Item -ItemType Directory -Force -Path $dir | Out-Null }
    [System.IO.File]::AppendAllText($log, $line + [Environment]::NewLine, (New-Object System.Text.UTF8Encoding($false)))
  } catch {}
}

Say '=== 开始安装更新 ==='
Say ('安装目录：' + $install)

# 1) 等本进程退出。它自己在 600ms 后会正常 quit，正常情况下一两秒就没了。
$deadline = (Get-Date).AddSeconds(15)
while ((Get-Process -Id $waitPid -ErrorAction SilentlyContinue) -and ((Get-Date) -lt $deadline)) {
  Start-Sleep -Milliseconds 250
}
if (Get-Process -Id $waitPid -ErrorAction SilentlyContinue) {
  Say '主进程 15 秒没退出，直接结束它'
  Stop-Process -Id $waitPid -Force -ErrorAction SilentlyContinue
}

# 2) 只结束"装在这个目录里的"三个进程。用户可能同时开着一份别的拷贝（开发时的
#    dist/、另一个盘上的旧版），按进程名一律打死会把人家的窗口也关掉。
#    index 可能是服务，先试服务再试进程。
$procs = Get-CimInstance Win32_Process -Filter "Name='baozi.exe' OR Name='baozi-hook.exe' OR Name='baozi-index.exe'" -ErrorAction SilentlyContinue
foreach ($p in $procs) {
  if ($p.ExecutablePath -and $p.ExecutablePath.StartsWith($install, [StringComparison]::OrdinalIgnoreCase)) {
    Say ('结束进程 ' + $p.Name + '#' + $p.ProcessId)
    Stop-Process -Id $p.ProcessId -Force -ErrorAction SilentlyContinue
  }
}
$svc = Get-Service -Name 'baozi-index' -ErrorAction SilentlyContinue
if ($svc -and $svc.Status -ne 'Stopped') {
  try {
    Stop-Service -Name 'baozi-index' -Force -ErrorAction Stop
    Say '已停止索引服务（下次启动会由主程序重新拉起）'
  } catch {
    Say ('索引服务停不下来：' + $_.Exception.Message + '（这一步需要管理员）')
  }
}
Start-Sleep -Milliseconds 800

# 3) 备份 + 覆盖。任何一步失败都记下来，别静默。
$failed = $false
foreach ($f in 'baozi.exe','baozi-hook.exe','baozi-index.exe') {
  $src = Join-Path $stage $f
  $dst = Join-Path $install $f
  if (-not (Test-Path -LiteralPath $src)) { Say ('缺少 ' + $f + '，跳过'); $failed = $true; continue }
  try {
    if (Test-Path -LiteralPath $dst) { Copy-Item -LiteralPath $dst -Destination ($dst + '.old') -Force -ErrorAction Stop }
    Copy-Item -LiteralPath $src -Destination $dst -Force -ErrorAction Stop
    Say ('已替换 ' + $f)
  } catch {
    $failed = $true
    Say ('替换 ' + $f + ' 失败：' + $_.Exception.Message)
  }
}

# 4) 重新起来。主程序不在的话，用户会以为更新把软件弄没了。
$exe = Join-Path $install 'baozi.exe'
if (Test-Path -LiteralPath $exe) {
  try {
    Start-Process -FilePath $exe -WorkingDirectory $install
    Say '已重新启动 baozi'
  } catch {
    Say ('重启失败：' + $_.Exception.Message)
  }
} else {
  Say '安装目录里没有 baozi.exe，无法重启'
}

# 5) 收尾：删掉自己的临时目录（脚本自己也在里面，所以放到最后，
#    用 Remove-Item 删自己所在的目录在 Windows 上通常没问题）。
if ($failed) {
  Say '=== 安装结束（有失败项，临时文件保留以便排查）==='
} else {
  Say '=== 安装成功 ==='
  try { Remove-Item -LiteralPath $stage -Recurse -Force -ErrorAction SilentlyContinue } catch {}
}
`, q(installDir), q(stage), q(logPath), pid)
}

// writeUpdateLog 往同一份 update.log 里补一行（主进程这一侧写的部分）。
func writeUpdateLog(format string, args ...any) {
	dir := filepath.Join(os.Getenv("LOCALAPPDATA"), "Baozi")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, "update.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s  %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
}

// lastUpdateResult 读 update.log 的最后几行，给"关于"页显示上次更新的结局。
// 只挑"成功/结束/失败"这类关键行，日志本身留在磁盘上给人排查。
func lastUpdateResult() string {
	path := filepath.Join(os.Getenv("LOCALAPPDATA"), "Baozi", "update.log")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	for i := len(lines) - 1; i >= 0 && i >= len(lines)-40; i-- {
		s := strings.TrimSpace(lines[i])
		switch {
		case strings.Contains(s, "安装成功"):
			return "上次更新成功"
		case strings.Contains(s, "安装结束（有失败项"):
			// 把失败那几行带上，用户才知道哪一步没成。
			return "上次更新有失败项（详见 update.log）：" + firstFailure(lines[i:])
		}
	}
	return ""
}

// firstFailure 从尾部那段日志里挑第一条"失败"相关的行。
func firstFailure(tail []string) string {
	for _, l := range tail {
		s := strings.TrimSpace(l)
		if strings.Contains(s, "失败") || strings.Contains(s, "停不下来") || strings.Contains(s, "无法重启") {
			if i := strings.Index(s, "  "); i >= 0 && i+2 < len(s) {
				return strings.TrimSpace(s[i+2:])
			}
			return s
		}
	}
	return "原因未记录"
}
