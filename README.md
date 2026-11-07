# Baozi

Windows 上的快速启动器：**双击 Ctrl 唤出**，搜文件、搜应用、执行自定义命令、和小范围内传文件。
用 Go 重写 Lertaro 的核心体验，界面用 [mygo](https://github.com/egoist/mygo) 的原生 UI（Direct3D 11，无 WebView2）。
视觉上参照 [Pane](https://github.com/LBEILC/Pane) 的路子：磨砂玻璃浮窗、大圆角、跟随系统深浅色，空查询时先给"最近使用"。

```
双击 Ctrl  →  浮窗  →  输入  →  ↑↓ 选择  →  Enter 打开
```

## 现在能用的功能

| # | 能力 | 状态 |
|---|---|---|
| 1 | 双击 Ctrl 唤出、全屏应用自动不响应 | 已实现 |
| 2 | 空查询显示使用历史（最近 + 常用） | 已实现，跨进程持久化 |
| 3 | 结果排序：频次 > 类型 > 准确性，权重可配 | 已实现 |
| 4 | 局域网文件互传（类似 dashbeam，只做局域网） | 已实现（浏览器收发型） |
| — | 图形化设置，不用手改 JSON | 已实现 |
| — | Listary 式自定义命令（`term`、`code` …） | 已实现 |
| — | Pane 式界面：首页图标网格（已固定 / 最近使用）、底栏键帽提示、`Ctrl+K` 固定、`Ctrl+Enter` 开文件夹、`Ctrl+Shift+C` 复制目录 |
| 5 | 读取文件管理器当前目录 | 已实现五种来源：资源管理器（地址栏面包屑 + 已知文件夹本地化名还原）、Total Commander（`WM_COPYDATA` 远程协议）、Directory Opus、XYplorer、xplorer2；每种都能在设置里单独关掉 |
| 6 | 打开 / 保存框跟随：文件管理器换目录后，切回那个框，路径自动变过去 | 已实现（照 Lertaro 的 `FileDialogNavigationTracker`），见 `docs/architecture.md` §8 |
| — | 开机自动启动（登录后静默待在托盘，不弹面板） | 已实现，设置里可开关 |
| — | 快速索引服务（直接读 NTFS USN Journal，174 万条 17 秒） | 已实现，设置里一键装/卸 |

## 跑起来

```powershell
# 编译（Go 1.27.3+，本机在 D:\Go）
# -H=windowsgui 必须带上：不带的话 exe 是 console 子系统，双击会先弹一个黑色终端窗口。
D:\Go\bin\go.exe build -trimpath -ldflags "-H=windowsgui -s -w" -o dist\baozi.exe       ./cmd\baozi
D:\Go\bin\go.exe build -trimpath -ldflags "-s -w"              -o dist\baozi-hook.exe  ./cmd\baozi-hook
D:\Go\bin\go.exe build -trimpath -ldflags "-s -w"              -o dist\baozi-index.exe ./cmd\baozi-index

# 直接跑
.\dist\baozi.exe
```

三个 exe 必须放在同一目录：`baozi.exe` 启动时会在自己旁边找 `baozi-hook.exe`
（可选）与 `baozi-index.exe`（装服务时用）。钩子进程带 `CreateNoWindow` 启动，自己也不会弹窗。

托盘菜单里可以「设置…」「文件互传…」「重新建立索引」「打开配置文件」「开机启动」「卸载：清除自启与数据…」「退出 Baozi」。
点「退出 Baozi」会先关钩子进程、历史强制落盘，再关窗口退进程。

### 卸载

托盘里的「卸载：清除自启与数据…」会先弹一个系统确认框（默认按钮是「取消」，Enter 不会误删），
确认后做四件事，然后退出进程：

1. 摘掉开机自启：删 `Run` 里的 `Baozi`，连 `StartupApproved\Run` 里的记录一起删（只删前者的话，
   任务管理器「启动」页会留一条"已禁用"的空壳）；
2. 停掉历史防抖和互传服务——这两者手里可能还攥着数据目录；
3. 退出钩子进程，跳过"历史落盘"（数据马上要删，落盘等于白写）；
4. 关掉日志文件句柄，再 `RemoveAll` 整个 `%LOCALAPPDATA%\Baozi`（config.json、history.json、
   index.bin、baozi.log）。

**保留** `baozi.exe` 与 `baozi-hook.exe`。下次双击就是全新状态，索引会在后台重建。

两个"删了又长回来"的坑已经堵掉了（都是先删目录、后有别的代码把文件写回去）：

- 历史落盘有 400ms 防抖，最后一笔 `Touch` 的定时器会在目录删掉之后触发 `Save`，
  而 `Save` 里有 `MkdirAll`，于是 `history.json` 带着目录一起重生。修法是 `history.Store.Close()`
  先停定时器，并让之后的 `Save` 变成空操作。
- 后台索引扫描被取消后，旧代码仍会把"已扫到的半份结果"写进 `index.bin`——这不只是卸载时的问题，
  上次启动就会加载一份残缺索引（搜索结果缺东西）。现在取消后直接返回，不落盘；
  卸载还会等扫描 goroutine 真正结束再删目录。

命令行兜底：`$env:BAOZI_UNINSTALL=1; .\dist\baozi.exe`，跳过确认框直接卸（托盘点不出来时用）。

## 快速索引服务（NTFS / USN）

默认走目录扫描（普通权限，能用但慢）。设置里「索引」分区可以一键装上 `baozi-index.exe`，
它不遍历目录，直接读 NTFS 的 USN Journal（`$MFT` 记录），整机 174 万条 **17 秒**建完。

| | 目录扫描（默认） | 索引服务 |
|---|---|---|
| 权限 | 普通用户 | 服务以 SYSTEM 跑，**主程序仍是普通权限** |
| 全量 | 分钟级 | 17 秒 |
| 新文件可见 | 下次重建 | ≤2 分钟（或攒够 200 条变化） |
| 内存 | 主进程 238 MB | 主进程 238 MB + 服务 62 MB |

怎么用：

1. 设置 →「索引」→ **启用快速索引（需管理员）** → 弹一次 UAC，装成自动启动的服务并立即建索引；
2. 装好后不用管，服务自己盯增量、自己重建；主进程只在索引文件变了时整体重载一次；
3. 不想用了点 **停用并卸载**（同样一次 UAC）：先停服务再删除注册项，不删你的索引与配置。

几条要知道的：

- 服务是**可选**的。没装、或者装了但停了，主程序会按心跳文件（`index.heartbeat`）自动回退到
  自己扫，不会卡在一个不响应的服务上；
- 判断依据是心跳而不是"装没装"，所以服务崩了也能自动兜底；
- USN 记录里没有文件大小和修改时间（那在 `$STANDARD_INFORMATION` 里），
  所以服务建出来的索引这两项是 0；搜索结果不显示它们，排序也不依赖；
- 只支持 NTFS 卷。配置里加了 exFAT 的 U 盘或网络盘，服务会跳过该卷，其余卷照常走 USN；
- 想立刻重建：设置里的「立即重建索引」在服务模式下只是给服务放一个重建请求（`index.rebuild`），
  不会让主程序自己再扫一遍。

排障看 `%LOCALAPPDATA%\Baozi\baozi-index.log`（服务的日志，和主程序的分开写）。


### 开机自启与图标

自启写在 **`HKCU\Software\Microsoft\Windows\CurrentVersion\Run`**（值名 `Baozi`，
内容是 `"<exe 的绝对路径>" --mygo-opened-at-login`），不是计划任务、也不用管理员。
任务管理器「启动」页里可以单独禁用；禁用后 Baozi 下次启动读到的就是"未启用"，
设置里的勾选会跟着变回未勾选——以系统状态为准，不另存一份状态在 config.json 里。

那条 `--mygo-opened-at-login` 是"这次是系统拉起来的"标记：`ready()` 里看到它就**不弹面板**，
直接建托盘和钩子进程待在后台（日志写 `启动：来自开机自启，面板保持隐藏`）。
手动双击 exe 启动则照常弹出面板。

两个开关位置等价：托盘「开机启动」勾选项、设置里「唤出」分区的同一个勾选，改一边另一边会同步。
失败（比如注册表被策略锁了）会写日志并把勾选拨回去，不会假装成功。

图标是程序化画的同一个包子形象（暖橙圆角底 + 奶油色包子 + 顶部三道褶），换掉之前那个橙色放大镜：

| 文件 | 用在哪 | 说明 |
|---|---|---|
| `cmd/baozi/assets/tray.png` | 托盘/通知栏（`go:embed`） | 32×32 PNG |
| `cmd/baozi/assets/baozi.ico` | exe 图标（任务栏、Alt+Tab、资源管理器） | 16/24/32/48/64/128/256 |
| `cmd/baozi/rsrc_windows_amd64.syso` | 上面那个 ico 编进 PE 的 COFF 资源 | `go build` 自动链接，别删 |

每个尺寸都是**按解析式单独画**的，不是把 256 缩下来——16px 上会自动省掉褶子，
缩图会糊成三条竖杠。生成器在 `tools/icongen/main.go`（`//go:build ignore`），
改完配色跑 `D:\Go\bin\go.exe run tools\icongen\main.go`，再重新生成 syso：

```powershell
$env:GOARCH = "amd64"
D:\Go\bin\go.exe run github.com/akavel/rsrc@v0.10.2 -ico cmd\baozi\assets\baozi.ico -o cmd\baozi\rsrc_windows_amd64.syso
```


写成 GUI 子系统后没有控制台，日志都在 `%LOCALAPPDATA%\Baozi\baozi.log`（超过 2MB 自动清空重来）。
排障先看这个文件。

### 唤出后打字打不进去？

不会。面板拿到前台走的是自己的 `forceForeground()`（`cmd/baozi/foreground_windows.go`），
不是 mygo `Window.Focus()` 那一个裸的 `SetForegroundWindow`：后者会被 Windows 前台锁拒绝——
从别的应用或任务栏切回来时，我们既不是前台进程、最近也没收到输入，于是它只让任务栏图标闪一下，
面板出现了但键盘还在原来的窗口里。现在的做法是把自己附到当前前台窗口线程的输入队列上
（`AttachThreadInput`）再取焦点，改完立刻成对脱离。

日志里能看到的两种结果：`toggle: 显示面板`（正常）和 `唤出：抢前台失败（退化为普通显示）`
（真抢不到，只有极少数情况比如前台是 UAC 提权窗口/UIPI 隔离）。如果遇到后者，把日志发我。

## 三个窗口

| 窗口 | 入口 | 用途 |
|---|---|---|
| 搜索面板 | 双击 Ctrl / 托盘 | 搜文件、应用、跑命令 |
| Baozi 设置 | 面板底栏「设置」/ 托盘「设置…」 | 所有配置项，改完即时生效 |
| Baozi 文件互传 | 面板底栏「文件互传」/ 托盘「文件互传…」 | 生成一条带有效期的局域网链接（默认 1 小时），手机浏览器**直接下载**共享内容 |

设置界面的分区：**唤出**（双击 Ctrl 开关、间隔、备用组合键、全屏抑制、开机自动启动）、**文件对话框跟随**（总开关 + 五种目录来源逐项开关）、**排序**（三条权重滑杆、类型优先级、最多结果数）、**索引**（根目录、排除目录、包含隐藏文件、重建间隔、立即重建）、**自定义命令**（每行 `关键字 | 显示名 | 程序 | 参数`，2/3/4 段都能解析）。
点「保存并应用」后会：写盘 → 组合键重新注册 → 重启钩子进程（它读的是同一份 config.json）→ 根目录变了就重建索引。不必退出软件。

## 文件互传怎么用

分享是**一次性的临时链接**：先挑文件 → 点「生成链接」→ 拿到一条带有效期的地址，到点自动失效。

1. 面板底栏或托盘打开「Baozi 文件互传」；
2. 点「添加文件 / 添加文件夹」把要分享的东西加进来（还没生成链接也可以先加）；
3. 「链接有效期」下拉选多久过期，默认 **1 小时**（另有 10 分钟 / 30 分钟 / 2 小时 / 8 小时 / 24 小时）；
4. 点「生成链接」，窗口里会显示：
   - 一个 **4 位访问码**（单独大字显示，例如 `ghcM`），手敲进手机比抄地址快；
   - 形如 `http://192.168.1.30:53211/s/ghcM/` 的地址，和一个二维码，以及"到期：21:35（还剩 12:34）"；
   - 「绑定网卡」下拉：默认 **全部网卡**；机器上有 VMware / MuMu / Hyper-V 这类虚拟网卡时，
     自动挑 IP 可能挑到它们（那种地址对端根本不存在），在这里固定成真正联网的那块。
     换网卡会重启服务，**链接和访问码都会换新**。
5. 到期或点「停止共享」后，监听端口直接关闭，链接立刻打不开——不做"只要不停止就一直分享"。

访问码规则（`internal/share/share.go`）：固定 4 位，只从"看着不会认错"的字符里取
（排掉 `0 O o`、`1 I l i`、`5 S`、`2 Z`、`8 B`），字母表是 `3467 9` + 25 个大写 + 25 个小写，
校验**区分大小写**——`aBcD` 和 `ABCD` 是两个不同的码，键盘上照抄窗口里显示的样子即可。
4 位码的搜索空间约 55⁴，所以还配了硬限制：**连续输错 20 次就自动停止共享**，
防止被局域网内脚本穷举；窗口会提示，点「重新生成链接」换一个新码。

- 只分享一项时，打开链接就**直接开始下载**（不用再点一次页面）；多项时给一个列表页；
- 点文件直接下载；点文件夹会**实时打包成 zip 流**下载（不落临时文件、没有体积上限，zip 里带一层同名文件夹）；
- **只发不收**：页面里没有任何上传入口，对方不能往你这台电脑写文件（`/u` 路由已移除，POST 一律 404）；
- 「复制链接」「复制访问码」放到剪贴板；「停止共享」立刻关端口、作废访问码。

首次共享时 Windows 防火墙会弹一次放行提示，允许后同一局域网才能访问。只做局域网，不做公网打洞。

## 历史为什么不会再丢

历史写在 `%LOCALAPPDATA%\Baozi\history.json`。每次打开条目后 **400ms 防抖落盘**，
不依赖"正常退出"——任务管理器强杀、崩溃、断电都不会再把它清空。

每条记录不只存路径，还存 `name`、`kind`（file/dir/app/command）、`ext`、`hint`（所在目录）：
应用记的是 `shell:AppsFolder\<AppUserModelID>`，靠这份元数据重启后仍能显示正确的应用名和真实图标。
老版本只存路径的历史也能直接读，缺少元数据时再按路径回退。

## 环境变量（调试/多份配置）

| 变量 | 作用 |
|---|---|
| `BAOZI_DATA_DIR` | 覆盖数据目录，做一套互不干扰的配置/历史/索引 |
| `BAOZI_HEADLESS=1` | 进程起来但不弹面板（冒烟、自启动） |
| `BAOZI_OPEN=settings` / `share` | 启动后直接打开对应窗口 |
| `BAOZI_DEBUG=1` | 日志里打印每次查询和每次打开，排查键盘/索引问题 |
| `BAOZI_AUTOQUIT=3s` | 到点走一遍真实退出流程（和托盘「退出 Baozi」同一条路），用于验证退出是否干净 |
| `BAOZI_REINDEX=1` | 强制重建一次索引（改了根目录/排除规则、或索引规则本身升级后用），不必手删 `index.bin` |
| `BAOZI_UNINSTALL=1` | 无人值守卸载：摘自启 + 删数据目录 + 退出，**跳过确认框**。托盘点不出「卸载」时的命令行退路 |
| `BAOZI_MEMSTAT=8s` | 每隔 N 秒往日志写一行 `heap/inuse/sys/objs/gc`，用来盯内存曲线 |
| `BAOZI_PPROF=1` | 起 `127.0.0.1:6060` 的 pprof，用 `go tool pprof -top -inuse_space http://127.0.0.1:6060/debug/pprof/heap` 看谁在占内存 |

## 配置文件

`%LOCALAPPDATA%\Baozi\config.json`（首次运行自动生成）。所有字段都有默认值，删掉也不会崩。

```jsonc
{
  "hotkey": {
    "doubleCtrl": true,          // 双击 Ctrl 唤出
    "doubleCtrlIntervalMs": 400, // 两次 Ctrl 的最大间隔
    "combo": "Alt+Shift+Space",  // 备用组合键；留空则不注册
    "disableInFullscreen": true  // 全屏应用/游戏前台时不响应
  },
  "search": {
    "weights": { "frequency": 0.5, "type": 0.3, "accuracy": 0.2 },
    "typeOrder": [".exe", ".lnk", ".msc", ".bat", ".cmd", ".ps1"], // 越靠前类型分越高（最高 0.90，仍低于应用）
    "maxResults": 80
  },
  "index": {
    "roots": ["C:\\", "D:\\"],
    "excludes": ["node_modules", ".git", "Windows\\WinSxS", "AppData\\Local\\Packages"],
    "includeHidden": false,     // 只管隐藏**文件**；隐藏**目录**总会进去（备份盘常被设成 Hidden）
    "rescanMinutes": 0          // 0 = 只在启动时按需建一次
  },
  "history": { "enabled": true, "maxItems": 200 },
  "share": {
    "bind": ""                  // "" = 所有网卡；填某块网卡的 IPv4 就只绑它（如 "192.168.0.108"）
  },
  "commands": [
    { "keyword": "term", "name": "Windows Terminal", "exec": "wt.exe", "args": [] },
    { "keyword": "code", "name": "VS Code",          "exec": "code",   "args": [] }
  ]
}
```

自定义命令用法：输入 `term` 回车执行；输入 `term 项目路径` 会把后面这段作为参数追加。

## 快捷键

| 键 | 作用 |
|---|---|
| 双击 Ctrl | 显示 / 隐藏面板。面板还在但焦点已经被别的窗口拿走（比如刚点过任务栏）时，这一下是**取回焦点**，不会把它收起来 |
| `↑` `↓` | 选择结果（首页按**行**移动，一行 7 个卡片） |
| `←` `→` | 左右移动一格（焦点在搜索框里也生效） |
| `Enter` | 打开选中项（应用走 Shell 启动，文件走默认程序，命令带参数） |
| `Ctrl+Enter` | 打开选中项**所在的文件夹**（文件开父目录，目录开自身） |
| `Ctrl+Shift+C` | 把选中项**所在目录**复制到剪贴板（个别程序自己的"浏览"框不走系统对话框、自动跟随覆盖不到时，可以 `Ctrl+L` → `Ctrl+V` → 回车手工跳） |
| `Ctrl+K` | 把选中项**固定**到首页「已固定」区 / 取消固定，立即写进 `history.json` |
| `Esc` | 先清空查询并回到首页 |

**鼠标**：结果行和首页卡片单击就打开；右键弹出菜单，可以「固定到首页 / 取消固定」和「从历史删除」——
首页的「最近使用」和「已固定」两段都能从这里删掉（删除是整条从 `history.json` 里移除；
`internal/history` 的 `Remove()` 会顺手把 `maxCount` 重算回去，免得剩下的条目得分被压低）。

底栏右侧直接写着 `Enter 打开 / Ctrl+Enter 文件夹 / Ctrl+Shift+C 复制路径 / Ctrl+K 固定 / 右键 删除·固定`，
左边一行说选中项会发生什么（`打开 抖音`、`已固定 · 记事本`）。「设置」「互传」是底栏的胶囊按钮，不用去托盘翻菜单。

## 打开 / 保存框跟着文件管理器走（Lertaro、Listary 的那个功能）

**不用经过 Baozi，也不用唤出面板。** 只要框开着：

1. 打开记事本「另存为」，框停在「我的文档」；
2. 打开资源管理器，把目录切到「下载」；
3. 切回那个「另存为」框 —— **它的路径自己变成了「下载」**。

目录来源支持五种，都能在设置里单独关掉（`settings` → 「文件对话框跟随」）：

| 来源 | 怎么读 |
|---|---|
| 资源管理器 | `CabinetWClass` 里 `Breadcrumb Parent → ToolbarWindow32` 的 window text |
| Total Commander | `TTOTAL_CMD` + `WM_COPYDATA` 远程协议（`'G'+256*'W'` 查询，UTF-16 回复） |
| Directory Opus | `dopus.lister` → `dopus.filedisplaycontainer` → `dopus.ctl.treepath` 的 window text |
| XYplorer | `WM_COPYDATA`（`0x00400001`）发 UTF-16 脚本，脚本自己 `copydata` 回投 `<curpath>` |
| xplorer2 | `ATL:ExplorerFrame` 里**可见**那个 `ComboBox` 的 `WM_GETTEXT` |

规则照 [Lertaro](https://github.com/lertaro/lertaro) 的
`Core/Hook/ExplorerIntegration/FileDialogNavigationTracker.cs` 原样搬（每种来源的读法
照它 `Plugins\<Name>\` 下对应的 `*PathCollector.cs`）：

- 记「最近一次文件管理器待过的目录」，目录一变就把版本号 `+1`；
- 一个对话框**第一次**成为前台时**只登记**、不导航——程序刚弹出它时往往自己指定了目录，
  不该立刻覆盖；
- 之后它再成为前台、而版本号变过 → 把记住的目录送过去；
- 切回对话框的那一刻**现场重读**一次文件管理器的目录，不用轮询缓存的旧值 ——
  否则你在文件管理器里换目录的瞬间没被采到，就会出现"第一次能跟、再换一个目录就不跟了"；
- 版本没变就不动（否则用户手动在框里换过目录，下次切回来会被拽回旧目录）。
  这条是 Lertaro 源码里明写的坑，一并照抄。

读资源管理器当前目录用的是**地址栏**：`CabinetWClass` 里 `Breadcrumb Parent → ToolbarWindow32`
的 window text 就是完整路径（实测 `"地址: D:\"`，剥掉 `地址: ` 前缀即可）——
和文件对话框那套控件是同一个，所以走同一条读法，不碰 `ShellWindows` COM。
`SendMessageTimeout` 带 500ms 超时，资源管理器在慢目录上卡住也拖不死 Baozi。

**「下载 / 文档 / 音乐」这些已知文件夹要单独还原**：在它们的根上，地址栏给的不是路径
而是本地化名字（实测 `地址: 音乐`、`地址: 文档`）。`internal/win/knownfolder.go` 对 12 个
常用 FOLDERID 取路径，再从注册表 `FolderDescriptions\{GUID}\LocalizedName` 拿
shell 自己的本地化字符串（`@…\windows.storage.dll,-21770`），用 `shlwapi.dll` 的
`SHLoadIndirectString` 解析成"文档 / 下载 / 音乐"，建一张表缓存。用
`SHGetFileInfoW(路径)` 拿不到这些名字——文档被重定向到 `D:\Backup\Documents` 时它只给 `Documents`。

**Total Commander** 走它自己的远程控制协议（照 Lertaro `Plugins\TotalCommander\Win32\Win32Helper.cs`）：
向 `TTOTAL_CMD` 发 `WM_COPYDATA`（`dwData = 'G'+256*'W'`，内容是 ASCII 的 `"SP\0"`），
TC 以 `WM_COPYDATA`（`dwData = 'R'+256*'W'`，UTF-16）回当前活动面板的路径。
两个实测硬约束：接收窗口必须和发送在**同一个 OS 线程**（回复靠嵌套投递送回来），
且只能用 `SMTO_ABORTIFHUNG`、**不能带 `SMTO_BLOCK`**（带了回复直接被丢掉）。
XYplorer 是同一个形状（脚本自己回投），所以收发那一套在 `internal/win/copydata.go` 里共用。

**Directory Opus / XYplorer / xplorer2 这三种没在真机上验证过** —— 用户这几样一个都没装。
窗口类名、协议、字段全按 Lertaro 的 collector 写；能验证的只有：没装时判定为假、
不会把普通窗口误判成来源；读不到时返回空，不导航也不崩。装了的人读得不对，
`tools/folderprobe` 会把容器 / `ComboBox` / 地址栏原文打出来。
OneCommander 与 Files 没做：两者都要 UI Automation 才拿得到路径。

动别人的窗口这件事只有一条铁律：**不 `AttachThreadInput`**，不抢前台——它会把两个线程的
输入队列绑死，Go 的线程还会被 runtime 换走，结果就是对方的窗口永久无响应
（这就是"切回记事本卡死"的真正原因）。

往前台送路径是照 `StandardFileDialogAdapter`：往**文件名框**
（`ComboBoxEx32 → ComboBox → Edit`）`WM_SETTEXT` 写完整路径，补 `WM_COMMAND(EN_CHANGE)`，
回读校验后再发回车。**不是**注入 DLL，也**不是**往地址栏写（地址栏写了只改显示、列表不动，
那批失败手段记在 [`docs/architecture.md`](docs/architecture.md) §8）。

实测（`tools/followtest`，一个进程内连跑两轮，第二轮就是"再换个目录再切回来"）：

```
① 另存为框 0x4c0426 初始地址: "地址: 文档"
② 资源管理器切到 D:\work\baozi        → ③ +500ms 地址: "地址: D:\work\baozi"      卡死=false
④ 资源管理器切到 C:\Windows\System32 → ⑤ +500ms 地址: "地址: C:\Windows\System32" 卡死=false
```

已知文件夹（就是"切到下载路径不变"那个 bug 的回归）：

```
① 另存为框 0xa135a 初始地址: "地址: 文档"
② 资源管理器切到 D:\work\baozi    → ③ +500ms 地址: "地址: D:\work\baozi"
④ 资源管理器切到 D:\Backup\Downloads（地址栏显示"地址: 下载"）
                                   → ⑤ +500ms 地址: "地址: 下载"  卡死=false
```

Total Commander（`FOLLOW_TC=1`，TC 11.50 x64）：

```
② TC 切到 D:\work\baozi            → ③ +500ms 地址: "地址: D:\work\baozi"
④ TC 切到 D:\work\baozi\internal   → ⑤ +500ms 地址: "地址: D:\work\baozi\internal"
```

接完五种来源之后的重跑（收发抽到 `copydata.go`、分派改成按来源开关过滤的方法，
所以两条链路都重测了一遍）：

```
# TC 当来源
② TC 切到 D:\work\baozi            → ③ +500ms 地址: "地址: D:\work\baozi"      卡死=false
④ TC 切到 D:\work\baozi\internal   → ⑤ +500ms 地址: "地址: D:\work\baozi\internal" 卡死=false

# 资源管理器 + 已知文件夹
④ 资源管理器切到 D:\Backup\Downloads（地址栏显示"地址: 下载"）
                                   → ⑤ +500ms 地址: "地址: 下载"  卡死=false
```

跟随器启动时的日志会把实际启用的来源列出来：

```
对话框跟随: 已启动（来源：资源管理器 / Total Commander / Directory Opus / XYplorer / xplorer2）
```

Baozi 侧同一时刻的日志：

```
跟随: 第一次见到对话框 0x4c0426，只登记（版本 0）
跟随: 记住目录 "D:\work\baozi"（版本 1）
跟随: 对话框 0x4c0426 版本 0→1，送到 "D:\work\baozi"
跟随: 记住目录 "C:\Windows\System32"（版本 2）
跟随: 对话框 0x4c0426 版本 1→2，送到 "C:\Windows\System32"
```

局限：只认标准 `#32770` + 面包屑地址栏的对话框。Electron / Qt 自绘的、AutoCAD 那类专有对话框不覆盖
（Lertaro 是靠适配器注册表逐个适配的，这里先不做）。`config.json` 里 `"dialogFollow": false`
可整个关掉，`"pathSources": {"opus": false}` 这类可以逐个关掉某种来源（省略即开）；
两种都在设置界面「文件对话框跟随」里，改完保存立刻生效，不用重启。
关掉之后 Baozi 不再读那个程序的窗口。

## 界面（借鉴 Pane）

界面结构照着 [Pane](https://github.com/LBEILC/Pane)（WinUI 3 的磨砂玻璃启动器）做的，
但只借它「怎么看」，不借它的功能集合：

| 借了什么 | 落成了什么 |
|---|---|
| 空查询首页是**图标网格**，「最近使用」在上、「已固定」在下 | `homeGrid()`：按行自己拼 `ui.Row`，每行 7 个卡片，卡片 84 宽（32 的圆角图标块 + 10.5 号、最多三行的名字）；名字由 `wrapName()` 按**字宽**自己折行（中文 10.6 DIP/字、拉丁 5.7 DIP/字），满三行才省略，悬停还有 `Tooltip` 给全名 |
| 选中项的**底栏语义提示**：左边说会做什么，右边写 `Enter` / `Ctrl+Enter` / `Ctrl-Shift-C` / `Ctrl+K` | `footer()` + `keyCap()`：键帽是淡色小圆角块 |
| 选中行是**浅色胶囊 + 细高光边**，不是整块高饱和填充 | `row()`：`Accent.Alpha(0.16)` 底 + `Accent.Alpha(0.5)` 1px 边 |
| 名称粗、路径细、右端一个很淡的类型标签 | 名称 14 加粗、路径 11 灰、`badge()` 10px |
| 顶栏左上放大镜、右上品牌胶囊 | `ui.Icon(searchIcon)` + 右端「Baozi」胶囊 |
| `Ctrl+K` 把应用固定到首页 | 固定标记存在 `history.Entry.Pinned`，随 `history.json` 落盘，`trim()` 不会删固定项 |

**明确没抄**：剪贴板历史、鼠标手势、超级面板、AI 对话、批量重命名、定时关机、
`Tab` 展开的三栏文件窗口（左分类/中结果/右预览）、`Alt+数字` 动作排。
这些要么和 Baozi 的定位不重合，要么需要先有「读当前目录」那套能力（见架构文档 §7）。

Pane 的视觉细节（磨砂玻璃、大圆角、细高光边、柔和阴影）靠 mygo 的
`VibrancyGlass` + `CornerRadius` 实现，跟随系统深浅色；没有引入 MiSans 字体（用系统默认，
避免多带一份字体文件）。

## 怎么搜

- **中文名敲拼音首字母**：`dbgz` → 豆包工作，`jsq` → 计算器；也能敲全拼 `doubaogongzuo`。
  首字母串在**建索引时**就存进节点里（纯 ASCII 名字不占空间），所以敲拼音不会比敲英文慢。
- **开始菜单里的应用永远排第一**：应用、命令的类型分（1.00 / 0.95）高于类型顺序的最高档 0.90，
  所以 `doubao` 这种"文件名更准、但应用只是全拼命中"的场景，应用仍然在前。
- **但不会只剩应用**：80 条结果里固定留 20 条给文件/目录，应用最多连续 3 条就插一条别的。
  输入单个字母 `d` 实测是 `60 应用 + 20 文件`，第 4 条就是 exe；普通文件类型分只有 0.20，
  不给这份保底名额就永远挤不进前 80。
- 类型顺序、三项权重都在设置界面里改，改完立即生效。多音字取字典默认读音，
  首字母只做包含匹配（`dz` 匹配不到 `dbgz`）。
- **多词用空格分开**，每个词都要命中：`snowshot video` 直接把 `SnowShot_Video_*.mp4` 顶到第 1 条，
  而只敲 `snowshot` 时那 12 张 `SnowShot_*.png` 会排在它前面（同样前缀、名字更短）。

### 明明有这个文件却搜不到？

先看它是不是藏在**索引范围外**。索引按 `index.roots` 全盘走，但有三道过滤：

1. `excludes`：默认排掉依赖目录（`node_modules`、`.git`）、系统维护目录（`WinSxS`）、
   以及 AppData 下的缓存/编译产物（`go-build`、`pnpm`、`npm-cache`、`pip`、`python`、
   `doubaowork`、`Thorium`、`Tencent`、`Windows\Explorer`、`INetCache`）。
   这些规则**永远生效**，用户列表只能往上加、删不掉。
2. **`AppData\Roaming\Microsoft\Windows\Recent` 也在排除里**：那里全是"最近打开"的 `.lnk`，
   Baozi 自己就记历史，索引它只会让 `.lnk` 把真正的文件挤出结果。
3. `includeHidden=false` 只跳过隐藏**文件**。隐藏**目录**一律递归进去——
   真实案例：`D:\Backup` 被设了 Hidden 属性，旧版本整棵跳过，里面几十个 SnowShot 视频一个都搜不到。

改了 `roots`/`excludes` 后不用手删索引文件，`BAOZI_REINDEX=1` 启动一次就会按新规则重建。

**装了索引服务时**还有一条额外规则：非 NTFS 卷（exFAT 的 U 盘、网络盘）读不到 USN，
服务会跳过整卷——那种盘上的文件搜不到是预期的，把它从 `roots` 里删掉或者先别装服务。
另外服务只看属性位判断隐藏，和目录扫描的口径一致。

## 目录结构

```
cmd/baozi/          主程序：mygo 原生 UI 面板、设置窗口、互传窗口、托盘、搜索、启动
cmd/baozi-hook/     钩子进程：WH_KEYBOARD_LL 双击 Ctrl、全屏判定
cmd/baozi-index/    索引服务（可选）：读 NTFS USN Journal 建索引，装/卸都在这里
internal/config/    单文件配置与默认值、数据目录
internal/history/   频次 + 最近使用（排序第一权重），元数据 + 防抖落盘
internal/index/     MFT 式节点树索引（名字进 arena + 父节点编号）、WalkDir 扫描、定长记录落盘
internal/usn/       USN Journal 三条 FSCTL + 从 $MFT 记录还原路径建索引
internal/pinyin/    汉字 → 首字母 / 全拼（只留字母数字、统一小写）
internal/search/    fzf 式匹配打分（含拼音档位）+ 频次/类型/准确性加权排序
internal/share/     局域网互传：HTTP + 一次性令牌 + 只发不收（没有任何上传通道）
internal/win/       Win32：前台窗口、全屏判定
docs/architecture.md 白盒架构、公式、与 Lertaro 的差异、未实现项的落地方案
```

## 测试

```powershell
D:\Go\bin\go.exe test ./...
D:\Go\bin\go.exe vet  ./...
```

- `internal/search`：匹配分级（含拼音首字母/全拼）、三项权重的优先级、应用压过 .exe
- `internal/pinyin`：`豆包工作` → `dbgz`、`doubaogongzuo`，混排与纯 ASCII 的边界
- `internal/index`：路径还原、gob 落盘往返、拼音列落盘后仍在
- `internal/history`：频次/时间排序、裁剪、**元数据往返**、**防抖自动落盘**
- `internal/share`：真实 HTTP 往返（首页、下载内容一致、错误令牌 404、`POST /u` 已 404 且文件没落盘）
- `cmd/baozi`：用 mygo 的 `ui.Tester` 无窗口渲染面板/设置/互传三个界面并断言关键文案，另校验两段式命令解析与"应用历史还原成应用名"
