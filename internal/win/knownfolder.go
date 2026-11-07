// 把资源管理器地址栏上的"本地化显示名"还原成真实路径。
//
// 为什么需要：地址栏在已知文件夹（下载 / 文档 / 音乐 / 图片 / 桌面 / 视频 …）的根上
// 显示的是**本地化名字**，不是路径 —— 实测读到的是 `地址: 音乐`、`地址: 下载`。
// 拿这种字符串去给文件对话框导航必然失败（`isDir("音乐")` 为假，跟随器只能当没读到）。
// Lertaro 的 ExplorerPathCollector 从 `Folder.Self.Path`（COM 里的真实路径）取值，
// 所以碰不到这个问题；我们用地址栏这条省事的读法，就得自己把显示名映射回路径。
//
// 名字的来源必须是 shell 自己用的那一份，否则对不上号：
//   - `SHGetFileInfoW(路径, SHGFI_DISPLAYNAME)` 不行 —— 目录被重定向过时
//     （本机文档就指向 `D:\Backup\Documents`）它只给目录名 "Documents"；
//   - `SHGetNameFromIDList(pidl, SIGDN_NORMALDISPLAY)` 在本机对这批目录返回失败；
//   - 真正对的是注册表 `FolderDescriptions\{FOLDERID}\LocalizedName`，
//     里面是间接字符串（`@C:\Windows\system32\windows.storage.dll,-21770`），
//     用 shell32 的 `SHLoadIndirectString` 解析出来就是地址栏上那个"文档"。
//     这是 shell 自己的本地化来源，换任何系统语言都自动跟着走。
package win

import (
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var (
	ole32   = windows.NewLazySystemDLL("ole32.dll")
	shlwapi = windows.NewLazySystemDLL("shlwapi.dll")

	procSHGetKnownFolderIDList = shell32.NewProc("SHGetKnownFolderIDList")
	procSHGetPathFromIDListW   = shell32.NewProc("SHGetPathFromIDListW")
	procSHLoadIndirectString   = shlwapi.NewProc("SHLoadIndirectString")
	procILFree                 = shell32.NewProc("ILFree")
	procCLSIDFromString        = ole32.NewProc("CLSIDFromString")
)

// guid 是 GUID 的内存布局（和 Windows 的 GUID 一致）。
type guid struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

// folderDescriptions 是 shell 存放"文件夹说明"的注册表位置（本地化名就在这）。
const folderDescriptions = `SOFTWARE\Microsoft\Windows\CurrentVersion\Explorer\FolderDescriptions\`

// knownFolderIDs 是常用已知文件夹的 FOLDERID（字符串形式，用 CLSIDFromString 解析）。
// 只挑本地化显示名会出现在地址栏根上的那些。
var knownFolderIDs = []string{
	"{B4BFCC3A-DB2C-424C-B029-7FE99A87C641}", // 桌面         Desktop
	"{374DE290-123F-4565-9164-39C4925E467B}", // 下载         Downloads
	"{FDD39AD0-238F-46AF-ADB4-6C85480369C7}", // 文档         Documents
	"{4BD8D571-6D19-48D3-BE97-422220080E43}", // 音乐         Music
	"{33E28130-4E1E-4676-835A-98395C3BC3BB}", // 图片         Pictures
	"{18989B1D-99B5-455B-841C-AB7C74E4DDFC}", // 视频         Videos
	"{BFB9D5E0-C6A9-404C-B2B2-AE6DB6AF4968}", // 链接         Links
	"{4C5C32FF-BB9D-43B0-B5B4-2D72E54EAAA4}", // 保存的游戏    SavedGames
	"{56784854-C6CB-462B-8169-88E350ACB882}", // 联系人       Contacts
	"{31C0DD25-9439-4F22-8CEA-BAEF2BA216CC}", // 3D 对象      3D Objects
	"{A52BBA46-E9E1-435F-B3D9-28DAA648C0F6}", // OneDrive
	"{DFDF76A2-C82A-4D63-906A-5644AC457385}", // 公共         Public
}

var (
	knownOnce sync.Once
	knownMap  map[string]string // 小写的显示名 / 末段目录名 → 真实路径
)

// knownFolderByDisplayName 把地址栏上的显示名（"下载"、"音乐" …）还原成绝对路径。
// 查不到返回空串 —— 调用方据此判定"这不是一个文件系统目录"，不做导航。
func knownFolderByDisplayName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	knownOnce.Do(buildKnownFolderMap)
	if p, ok := knownMap[strings.ToLower(name)]; ok {
		return p
	}
	return ""
}

func buildKnownFolderMap() {
	m := make(map[string]string, len(knownFolderIDs)*2)
	for _, id := range knownFolderIDs {
		path := knownFolderPath(id)
		if path == "" {
			continue
		}
		if name := knownFolderName(id); name != "" {
			m[strings.ToLower(name)] = path
		}
		// 双保险：英文系统 / 没注册本地化名的条目上，地址栏会退回目录名本身。
		if base := filepath.Base(strings.TrimSuffix(path, `\`)); base != "" && base != `\` {
			if _, dup := m[strings.ToLower(base)]; !dup {
				m[strings.ToLower(base)] = path
			}
		}
	}
	knownMap = m
}

// KnownFolderTable 返回"显示名 → 真实路径"的映射（探针 / 排障用，返回副本）。
func KnownFolderTable() map[string]string {
	knownOnce.Do(buildKnownFolderMap)
	out := make(map[string]string, len(knownMap))
	for k, v := range knownMap {
		out[k] = v
	}
	return out
}

// knownFolderPath 由 FOLDERID 取真实路径（走 KnownFolder 的 PIDL，重定向后也准）。
func knownFolderPath(clsid string) string {
	var g guid
	if !parseGUID(clsid, &g) {
		return ""
	}
	var pidl unsafe.Pointer
	if hr, _, _ := procSHGetKnownFolderIDList.Call(
		uintptr(unsafe.Pointer(&g)), 0, 0,
		uintptr(unsafe.Pointer(&pidl))); hr != 0 || pidl == nil {
		return ""
	}
	defer procILFree.Call(uintptr(pidl))

	var buf [260]uint16
	if r, _, _ := procSHGetPathFromIDListW.Call(uintptr(pidl), uintptr(unsafe.Pointer(&buf[0]))); r != 0 {
		return strings.TrimSuffix(windows.UTF16ToString(buf[:]), `\`)
	}
	return ""
}

// knownFolderName 由 FOLDERID 取地址栏上会出现的那个本地化名字。
func knownFolderName(clsid string) string {
	src := folderDescriptionString(clsid, "LocalizedName")
	if src == "" {
		// 没有本地化资源时退回 Name（"Documents" / "Downloads" 之类）。
		return folderDescriptionString(clsid, "Name")
	}
	var buf [260]uint16
	p, err := syscall.UTF16PtrFromString(src)
	if err != nil {
		return ""
	}
	if hr, _, _ := procSHLoadIndirectString.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0); hr != 0 {
		return ""
	}
	return strings.TrimSpace(windows.UTF16ToString(buf[:]))
}

// folderDescriptionString 读 FolderDescriptions\{GUID} 下的一个字符串值。
func folderDescriptionString(clsid, value string) string {
	for _, root := range []registry.Key{registry.LOCAL_MACHINE, registry.CURRENT_USER} {
		k, err := registry.OpenKey(root, folderDescriptions+clsid, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		v, _, err := k.GetStringValue(value)
		k.Close()
		if err == nil && v != "" {
			return v
		}
	}
	return ""
}

func parseGUID(clsid string, g *guid) bool {
	p, err := syscall.UTF16PtrFromString(clsid)
	if err != nil {
		return false
	}
	hr, _, _ := procCLSIDFromString.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(g)))
	return hr == 0
}

// resolveFolderText 把地址栏 / 面包屑上的文字变成"确定存在的绝对目录"。
// 已经是绝对路径就直接校验存在性；是本地化显示名就查已知文件夹表；
// 两者都不是（"此电脑"、"网络"、`::{...}` 这类虚拟位置）返回空串。
func resolveFolderText(text string) string {
	text = strings.TrimSpace(text)
	for _, prefix := range []string{"地址: ", "Address: "} {
		text = strings.TrimPrefix(text, prefix)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if filepath.IsAbs(text) {
		if isDir(text) {
			return text
		}
		return ""
	}
	if p := knownFolderByDisplayName(text); p != "" && isDir(p) {
		return p
	}
	return ""
}
