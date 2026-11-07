// "这个窗口能不能提供当前目录"，以及它属于哪一种来源。
//
// 跟随器只需要两个动作：问"这个窗口是不是目录来源"、问"那它的目录是什么"。
// 每一种文件管理器在这里加一行分派即可，状态机（follow.go）不用动。
package win

// 来源标识。config.PathSourceEnabled 用同样的字面量，
// 与设置界面里的勾选项一一对应。
const (
	SourceExplorer = "explorer"
	SourceTotalCmd = "totalcmd"
	SourceOpus     = "opus"
	SourceXyplorer = "xyplorer"
	SourceXplorer2 = "xplorer2"
)

// SourceNames 是全部来源，按设置界面里的展示顺序。
func SourceNames() []string {
	return []string{SourceExplorer, SourceTotalCmd, SourceOpus, SourceXyplorer, SourceXplorer2}
}

// SourceLabel 是给界面 / 日志用的名字。
func SourceLabel(kind string) string {
	switch kind {
	case SourceExplorer:
		return "资源管理器"
	case SourceTotalCmd:
		return "Total Commander"
	case SourceOpus:
		return "Directory Opus"
	case SourceXyplorer:
		return "XYplorer"
	case SourceXplorer2:
		return "xplorer2"
	}
	return kind
}

// sourceKind 判断这个窗口是哪一种目录来源；都不是就返回空串。
func sourceKind(hwnd uintptr) string {
	switch {
	case IsExplorer(hwnd):
		return SourceExplorer
	case IsTotalCommander(hwnd):
		return SourceTotalCmd
	case IsDirectoryOpus(hwnd):
		return SourceOpus
	case IsXyplorer(hwnd):
		return SourceXyplorer
	case IsXplorer2(hwnd):
		return SourceXplorer2
	}
	return ""
}

// sourceFolder 按来源类型选对应的读法。
func sourceFolder(kind string, hwnd uintptr) string {
	switch kind {
	case SourceExplorer:
		return ExplorerFolder(hwnd)
	case SourceTotalCmd:
		return TotalCommanderFolder(hwnd)
	case SourceOpus:
		return OpusFolder(hwnd)
	case SourceXyplorer:
		return XyplorerFolder(hwnd)
	case SourceXplorer2:
		return Xplorer2Folder(hwnd)
	}
	return ""
}
