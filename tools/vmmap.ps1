# vmmap 是一次性诊断脚本：枚举某个进程的提交地址空间，按类型（私有/映像/映射文件）
# 汇总大小，并把最大的若干块单独列出来，好判断"私有内存到底花在哪"。
#
#	pwsh -NoProfile -File tools\vmmap.ps1 -Name baozi
param(
    [Parameter(Mandatory = $true)][string]$Name,
    [int]$Top = 25
)

Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;
using System.Text;

public static class MemMap {
    [StructLayout(LayoutKind.Sequential)]
    public struct MEMORY_BASIC_INFORMATION {
        public IntPtr BaseAddress;
        public IntPtr AllocationBase;
        public uint AllocationProtect;
        public IntPtr RegionSize;
        public uint State;
        public uint Protect;
        public uint Type;
    }

    [DllImport("kernel32.dll")]
    public static extern int VirtualQueryEx(IntPtr hProcess, IntPtr lpAddress, out MEMORY_BASIC_INFORMATION lpBuffer, IntPtr dwLength);

    [DllImport("psapi.dll", CharSet = CharSet.Unicode)]
    public static extern int GetMappedFileNameW(IntPtr hProcess, IntPtr lpAddress, StringBuilder lpFilename, int nSize);
}
'@

$proc = Get-Process -Name $Name -ErrorAction Stop | Select-Object -First 1
$h = $proc.Handle

$MBI_SIZE = [System.Runtime.InteropServices.Marshal]::SizeOf([type][MemMap+MEMORY_BASIC_INFORMATION])
$addr = [IntPtr][Int64]0
$rows = New-Object System.Collections.Generic.List[object]
$maxAddr = [uint64]0x7FFFFFFEFFFF

while ($true) {
    $mbi = New-Object MemMap+MEMORY_BASIC_INFORMATION
    $ok = [MemMap]::VirtualQueryEx($h, $addr, [ref]$mbi, [IntPtr]$MBI_SIZE)
    if ($ok -eq 0) { break }
    $size = [uint64]$mbi.RegionSize.ToInt64()
    if ($size -eq 0) { break }
    if ($mbi.State -eq 0x1000) {
        $kind = switch ($mbi.Type) {
            0x20000 { 'PRIVATE' }
            0x40000 { 'MAPPED' }
            0x1000000 { 'IMAGE' }
            default { 'OTHER' }
        }
        $file = ''
        if ($kind -ne 'PRIVATE') {
            $sb = New-Object System.Text.StringBuilder 1024
            $n = [MemMap]::GetMappedFileNameW($h, $mbi.BaseAddress, $sb, 1024)
            if ($n -gt 0) { $file = $sb.ToString() }
        }
        $rows.Add([pscustomobject]@{
            Base = [uint64]$mbi.BaseAddress.ToInt64()
            Size = $size
            Kind = $kind
            Protect = $mbi.Protect
            File = $file
        })
    }
    $next = [uint64]$mbi.BaseAddress.ToInt64() + $size
    if ($next -le [uint64]$addr.ToInt64()) { break }
    if ($next -gt $maxAddr) { break }
    $addr = [IntPtr][Int64]$next
}

"进程: $($proc.Name) PID=$($proc.Id)  区域数=$($rows.Count)"
""
"=== 按类型汇总（提交内存） ==="
$rows | Group-Object Kind | Sort-Object { ($_.Group | Measure-Object -Property Size -Sum).Sum } -Descending | ForEach-Object {
    $sum = ($_.Group | Measure-Object -Property Size -Sum).Sum
    "{0,-8} {1,10:N1} MB  区块 {2}" -f $_.Name, ($sum / 1MB), $_.Count
}
""
"=== 私有区块里最大的若干块（按起始地址附近能否读出模块名辅助判断） ==="
$rows | Where-Object Kind -eq 'PRIVATE' | Sort-Object Size -Descending | Select-Object -First $Top | ForEach-Object {
    $sb = New-Object System.Text.StringBuilder 1024
    $n = [MemMap]::GetMappedFileNameW($h, [IntPtr]$_.Base, $sb, 1024)
    $hint = if ($n -gt 0) { $sb.ToString() } else { '' }
    "0x{0:X12}  {1,9:N2} MB  prot=0x{2:X}  {3}" -f $_.Base, ($_.Size / 1MB), $_.Protect, $hint
}
""
"=== 映射文件（按文件汇总，前 15） ==="
$rows | Where-Object { $_.Kind -ne 'PRIVATE' -and $_.File -ne '' } |
    Group-Object { Split-Path -Leaf $_.File } |
    Sort-Object { ($_.Group | Measure-Object -Property Size -Sum).Sum } -Descending |
    Select-Object -First 15 | ForEach-Object {
        $sum = ($_.Group | Measure-Object -Property Size -Sum).Sum
        "{0,-45} {1,8:N1} MB" -f $_.Name, ($sum / 1MB)
    }
