# Baozi 架构与取舍

本文写给维护者：机制、公式、边界、已知问题。用户操作看 `README.md`。

## 1. 进程模型

```
baozi.exe           用户态交互层（对应 Lertaro.App）
 ├─ mygo 原生 UI     浮窗 / 托盘 / 设置
 ├─ 索引             读 index.bin 快照（服务模式）；没有服务时自己 WalkDir（见 §9）
 └─ 子进程 baozi-hook.exe   低级键盘钩子 + 全屏判定（对应 Lertaro.Service --hook）
      └─ stdout 一行事件 → 主进程读 → toggle

baozi-index.exe     可选：Windows 服务（LocalSystem），读 NTFS USN Journal 建索引
 └─ 写 index.bin / index.progress.json / index.heartbeat，主进程只读不写
```

`baozi-index.exe` 不在跑的时候，一切行为与之前完全一致（普通权限、目录扫描）。

为什么钩子必须独立进程：

- `WH_KEYBOARD_LL` 需要一个独立的 Windows 消息循环，和 UI 主线程共用会互相拖慢；
- 钩子被反作弊 / 安全软件干掉时，主进程还能活，重启钩子即可；
- 主进程退出时关闭子进程 stdin，钩子读到 EOF 自杀，不留孤儿（已实测）。

**不抄的地方**：Lertaro 把钩子做成 `LocalSystem` 服务拉起的提权进程，用来向管理员权限窗口发消息（UIPI 突破）。
Baozi 暂时不做提权钩子：双击 Ctrl 只需要读按键，普通权限就够；真正需要操作高权限窗口的是第 6 条（文件对话框跳转），
到那一步再按需把 hook 以 `runas` 启动，而不是无条件提权。

## 2. 唤出链路与全屏抑制

双击 Ctrl 的状态机（`cmd/baozi-hook/main.go`）：

```
Ctrl↓ Ctrl↑                    → armed = true，记录 lastCtrlUp
Ctrl↓ 且 now-lastCtrlUp ≤ 400ms → 触发 toggle，armed = false
中间出现任何非修饰键按下         → armed = false（避免 Ctrl+C 被误判）
```

全屏抑制（`internal/win/fullscreen.go`），命中任一即不唤出：

1. `SHQueryUserNotificationState()` 返回 `QUNS_BUSY(2)` / `QUNS_RUNNING_D3D_FULL_SCREEN(3)` / `QUNS_PRESENTATION_MODE(4)`
   —— 覆盖 D3D 独占游戏与演示模式；
2. 前台窗口矩形覆盖所在显示器（容差 2px），且 `GWL_STYLE` 没有 `WS_CAPTION` —— 覆盖无边框全屏的播放器/游戏；
3. 前台窗口是 `Progman` / `WorkerW` / `Shell_TrayWnd` 时永远不算全屏（那是桌面本身）。

主进程 `show()` 里会再判一次，所以走 `RegisterHotKey` 的备用组合键也享受同样的抑制。

为什么快捷键会"唤不出"（Lertaro 的老毛病）：
`RegisterHotKey` 只能注册组合键，被别的程序抢占时注册失败且无提示，高权限窗口或独占全屏下也常不触发。
Baozi 主路径不依赖它，组合键只是兜底。

## 3. 索引

### 数据结构

照 NTFS `$MFT` 的存法：不存完整路径，只存"名字在 arena 里的位置 + 父节点编号"。

```go
// Node 定长 20 字节
type Node struct {
    Parent  int32  // 父节点下标，-1 表示根
    NameOff uint32 // 名字在 Table.Names 里的偏移
    SizeKB  int32  // 文件大小（KB）；目录为 0
    ModSec  int32  // 相对 2020-01-01 的秒数
    NameLen uint16 // 名字字节数
    Kind    Kind
}

type Table struct {
    Nodes []Node
    Names []byte           // arena：所有文件名首尾相接
    Py    map[int32]uint64 // 只对含汉字的条目有值，打包了"偏移+长度"
}
```

取路径时顺着父链往上拼（`Table.PathOf`），代价 O(深度)。

**为什么名字要进 arena 而不是每条一个 `string`**：179 万条如果各自分配，
40.6 MB 的实际内容会占到 ~90 MB（16 字节 string 头、分配器按 size class 向上取整、
每个对象的管理开销），还附带上百万个对象让 GC 每次都得扫。进 arena 之后是连续一块，
对象数从 209 万降到 4.9 万。

拼音首字母串同理：只有 9 千条含汉字的名字需要，旧结构却给每条都留了一个 16 字节字符串头
（约 28.6 MB 纯浪费）。现在只给这 9 千条在 arena 尾部留位置，`Py` 里存打包的偏移+长度。

### 扫描

两条数据源，产出同一张 `Table`（同一个 `Builder`，同一套排除规则）：

- **服务模式**：`internal/usn.BuildAll()` 读 `$MFT` 记录还原路径（16 秒 / 174 万条，见 §9）；
- **进程内模式**：`index.Build()` 按配置根目录 `filepath.WalkDir`，规则：

- 排除规则不带分隔符 → 按目录名/文件名匹配（支持通配）；带 `\` → 按路径片段匹配；
- `defaultExcludes()` 那几条**永远生效**，`normalize()` 把用户的列表 `mergeExcludes()` 上去
  （大小写不敏感去重）。原因是老配置里不可能有新增项，而隐藏目录解禁后 AppData 下几万个
  缓存文件会立刻涌进结果：实测解禁前后索引从 141 万涨到 192 万，其中 AppData 占 18.7 万，
  且 `Recent\*.lnk` 会靠类型分（0.85）把真正的文件挤出前列；
- 不跟随 reparse point（符号链接、目录联接），避免环路和重复索引；
- `includeHidden=false` **只**跳过带 `FILE_ATTRIBUTE_HIDDEN` 的**文件**；隐藏**目录**照常递归。
  这条是被真实案例逼出来的：`D:\Backup` 带 Hidden 属性，旧逻辑整棵 `SkipDir`，
  用户备份盘里几十个文件一条都没进索引，表现为"明明有这个文件却搜不到"；
- 单个条目权限不足只跳过该条目，不中断整盘；
- 根目录去重并去掉互相嵌套的项（`C:\` 和 `C:\Users` 同时配置时只扫前者）。

### 落盘

`index.bin`（v3，定长记录 + arena），不再是 gob。格式：

```
0..40  固定头：magic "BZID" | ver | count | namesLen | rawLen | rootsLen | pyCount | nodesOff | namesOff | pyOff
40..   roots JSON
..     [补齐到 8 字节]
..     nodes  count × 20 字节
..     names  namesLen（所有文件名首尾相接）
..     py     pyCount × 10 字节（索引 u32 + 偏移 u32 + 长度 u16）
```

整文件用 gzip 压（23.4 MB / 179 万条，比 gob 还小一点），加载时一次解压到 `rawLen` 指定的缓冲，
然后 `nodes` 段直接 `unsafe.Slice` 成 `[]Node`、`names` 段直接切片——**零解码、零拷贝**。
版本不匹配仍直接判为需要重建。

为什么换掉 gob，见第 15 节（三个理由都跟内存有关）。

### 搜索预筛：不再预存小写表

旧实现给每条名字预存一份 `strings.ToLower` 的结果（`catalog.lowers`，实测 39 MB + 42 万个对象）。
现在直接在 arena 字节上做大小写不敏感的子串比较（`Table.Hits`）：ASCII 折大小写，
汉字的 UTF-8 字节都 >0x7f、原样比较才是对的。零分配，也不用为 179 万条多存一份。

搜索主循环因此从"读预生成的小写串"变成"读 arena 的一段字节"，
命中的少数候选才取 `Name(i)` 构造 string（会分配一次，但候选最多 5000 条）。

## 4. 排序（可直接复算）

```
Freq  = 0.7 * log1p(count)/log1p(maxCount) + 0.3 * exp(-ageHours/168)
Type  = 开始菜单应用 1.00；自定义命令 0.95；目录 0.60
        类型优先级第 i 名 → 0.90 - 0.05*i（下限 0.15）；未列入类型 0.20
Acc   = 见下表
Score = (0.5*Freq + 0.3*Type + 0.2*Acc) / (0.5+0.3+0.2)
```

类型顺序的最高档只到 **0.90**，不是 1.00：这是"开始菜单应用永远排最前"的实现方式。
若 `.exe` 也给 1.00，它会和应用同分，胜负只能落到准确度上，应用就沉下去了。
应用、命令这两类不吃 `typeOrder`，它们的档位在代码里写死（`search.TypeScore`）。

| 匹配形态 | Acc |
|---|---|
| 文件名完全等于查询 | 1.00 |
| 拼音首字母串完全等于查询（`dbgz` → 豆包工作） | 0.97 |
| 文件名以查询开头 | 0.95 |
| 拼音全拼完全等于查询（`doubaogongzuo`） | 0.95 |
| 拼音首字母串以查询开头（`dbg`） | 0.90 |
| 命中词边界（空格 `.` `-` `_` `(` `[` `\` `/` `@` `+` `,` `{` 之后） | 0.88 |
| 拼音全拼以查询开头 | 0.88 |
| 文件名包含查询 | 0.78 |
| 拼音全拼包含查询 | 0.70 |
| 目录路径包含查询 | 0.62（只在查询里带 `\` 或 `/` 时生效；应用不传路径，见下） |
| 字符按顺序散落（`vsc` → Visual Studio Code） | 0.35–0.52（按连续度） |
| 拼音首字母散落命中 | 0.45 |
| 多关键词 | 取各 token 最小值（AND 语义）；拼音只在单关键词时启用 |

**应用的 Path 不参与匹配**：开始菜单应用的 Path 是 `shell:AppsFolder\<AUMID>` 或磁盘路径，
而 0.62 这一档是"整条路径包含查询"，会把路径中间任意子串都算命中 —— 实测搜 `cal` 时，
`C:\Users\…\AppData\Local\nvm\nvm.exe` 里的 `Local` 含 `cal`（Lo-cal），于是 nvm、Python 3.11/3.12/3.14
全冒出来（还带着应用类型权重排在最前），真正的计算器反被挤到第四。
现在应用只走两路：显示名（含拼音）+ `search.AppAlias(ID)` 抽出来的产品名
（`Microsoft.WindowsCalculator_8wekyb3d8bbwe!App` → `Microsoft WindowsCalculator`，
`…\nvm\nvm.exe` → `nvm`），取分高的那个；`{GUID}` 这种自动生成的 AUMID 直接返回空，不参与匹配。
改完搜 `cal`：只有「计算器」，nvm 与 Python 全部消失。

同分比较顺序：总分 → Acc → Freq → 名字更短 → 字典序（`search.Less`）。

用户要的"第一频次、第二类型、最后准确性"就是默认权重 0.5 / 0.3 / 0.2；
权重可改，改完不用重启（下次打开配置时生效于新查询）。

### 结果构成：应用优先，但不能只剩应用

只靠类型分挡不住"应用铺满全榜"：输入单个字母 `d`，191 个开始菜单应用里有几十个命中，
且应用类型分 1.00，前 80 条一度是 72 应用 + 8 文件，用户看到的字面效果就是"只显示应用"。
所以截断前后各做一次结构性处理：

1. **非应用保底**（`truncateMixed`）：80 条里固定留 `max/4`（默认 20 条）给文件/目录，
   应用最多占 60 条。普通文件类型分只有 0.20，不给名额就永远进不了前 80。
2. **打散应用连续块**（`search.Balance`，`maxAppRun = 3`）：应用最多连续 3 条，
   之后插一条别的类型。两类各自保持原分数顺序，一个结果都不丢。

实测 `d`：`结果 80: 应用 60, 文件 20`，第 4 条就是 `DuplicateDesktop.exe`（应用 0.980 → 0.956 → 0.956 → 文件 0.460）。
代价是**分数序被局部打破**（第 4 条文件分数低于第 5 条应用），这是"优先但不独占"必然的取舍；
真要严格按分数看，把 `maxAppRun` 调大或干脆设 0 即可（`cmd/baozi/main.go` 的常量）。

### 拼音怎么接进来的

- `internal/pinyin` 用 `mozillazg/go-pinyin`，只保留 `[a-z0-9]` 并统一小写：
  `豆包工作 (Beta)` → `dbgzbeta`，输入不用纠结空格和括号。
- **索引节点存一份首字母串**（`index.Node.Py`），只对含汉字的名字算，纯 ASCII 名字留空。
  这样敲 `dbgz` 时是字符串比较，不用在百万条目上逐条转拼音。
  落盘版本因此从 1 升到 2，旧快照会自动重建一次。
- 预筛也要跟着改：`dbgz` 在文件名里找不到字面，`containsAll` 会把它全滤掉，
  所以加了 `pyContains`——拼音首字母串包含全部 token 就放行。
- 应用（开始菜单，几百个）在加载时同时算首字母串和全拼串，三路都能命中。
- 弱点：多音字取字典默认读音（"重"可能算成 `z` 而不是 `c`）；
  首字母只做"包含"不做散落子序列（`dz` 匹配不到 `dbgz`），因为后者在全表上太贵。

### 正则模式（`internal/search/regex.go` + `searchRegex`）

搜索框右边的 `.*` 按钮把查询从"模糊检索"切成"正则精确匹配"。两条路在
`searchAll` 第一行就分流，正则模式**完全不碰**下面那些东西：拼音串、token 拆词、
词首/子序列打分档位、`byPath` 的路径包含判定。

口径：

- 模式按 Go `regexp`（Perl 语法）编译，**区分大小写**，要忽略就写 `(?i)`。
- 只匹配**文件名**（索引 arena 里那一段字节），不匹配完整路径。
  普通模式靠"查询里有没有 `\` 或 `/`"判断要不要搜路径，但正则里 `\.log$` 这种
  反斜杠到处都是，那个判断在这条路上不成立；顺带省掉了 179 万次 `PathOf`。
- `Acc` 恒为 `1.0`（`search.RegexAccuracy`）：命中就是命中，没有"匹配得好不好"。
  分数退化成 `Freq` + `Type` 两项，也就是用户定的"频次第一"。
- 收集上限 `maxRegexHits = 20000`（普通模式是 5000）：正则的命中面更宽
  （`\.log$` 一项就 8600 多条），上限太小会在排序之前把该靠前的丢掉。
- 每 2048 条回看一眼查询是否还等于发起时的字符串，变了就直接放弃这一遍
  （清空键盘后旧的正则还在扫，会和新的抢 CPU）。
- 编译失败不静默：错误存进 `regexErr`，结果区显示"正则无效：……"。

**为什么必须做字面预筛**：直接 `re.Match(nameBytes)` 全量 174 万条，
`^.*\.log$` 要 647 ms。做法是用 `regexp/syntax` 解析语法树，求出"一定出现的字面"，
先用零分配的子串比较（`containsFoldASCII`）筛一遍：

| 语法节点 | 取字面的规则 |
|---|---|
| `Literal` | 整段字面本身；`(?i)` + 非 ASCII 时**放弃**（Go 会折 ä/Ä，预筛只折 ASCII） |
| `Concat` | 各子式都必须命中 → 多个"单组"字面**合并**成更强的 AND |
| `Alternate`（`\|`） | 每个分支都得给出字面，合并成**或**的组；任一分支求不出就整体放弃 |
| `Plus` / `Repeat{Min>=1}` | 内层必须出现，沿用它 |
| `Star` / `Quest` / `Repeat{Min=0}` | 可能出现 0 次，**放弃** |
| `Capture` | 内层 |

`rpt|backup` 是最好的反例：只取"整条正则里最长的字面"（`backup`）会漏掉
141 条只含 `rpt` 的命中（263 vs 404）。按分支求或组之后，预筛只放行 518 条，
命中 404 条，一条不少。

实测（本机 174 万条，`tools/regexprobe` 跑的完整循环：预筛 + 正则 + 命中后 `PathOf`/`Ext`）：

| 模式 | 命中 | 耗时 |
|---|---|---|
| `^.*\.log$` | 8636 | 67 ms |
| `\d+\.log$` | 6064 | 65 ms |
| `rpt\|backup` | 404 | 110 ms |
| `\.(jpg\|png)$` | 103108（截到 20000） | 52 ms |
| `\d{4}-\d{2}-\d{2}`（无字面，全量扫） | 1486 | 213 ms |

没有字面的模式最贵也就 200 毫秒出头，而且和普通搜索一样跑在
`refilter` 的 goroutine 里，不占 UI 线程。

## 5. 历史

`history.json` 每条是 `{path, name, kind, ext, hint, exec, args, count, lastUsed, pinned, pinOrder}`。

- **元数据**：`kind` 是 `file` / `dir` / `app` / `command`。应用记录的是 `shell:AppsFolder\<AppUserModelID>`，
  只看路径的话 `filepath.Base` 会得到一串 AUMID、按扩展名取图标只会拿到通用文件图标——这就是"名字和图标不对劲"的根因。
  写入时用 `TouchEntry(Entry)` 合并元数据（新值为空不覆盖旧值），读出时优先用记录里的 `name/kind/ext/hint`，
  老记录只有 `path` 才回退推断。图标因此可以按"路径/AUMID"取 `Shell.FileIcon`，而不是按扩展名取 `TypeIcon`。
- **持久化时机**：每次 `Touch` 后 400ms 防抖 `Save()`（`time.AfterFunc` + `Reset`），`Flush()` 供退出前补一刀。
  早先只在 `OnWillQuit` 落盘，进程被强杀/崩溃就全丢——现在不依赖"正常退出"。
- **排序**：`0.7*log1p(count)/log1p(maxCount) + 0.3*exp(-ageHours/168)`；`lastUsed` 单调递增（同一毫秒内两次打开也不会乱序）；
  超上限按次数裁剪。空查询时 `Recent(12)` 就是"快捷菜单里的历史"。
- **已固定的位置是显式的**（`pinned` + `pinOrder`）：`pinOrder` 从 1 起，越小越靠左，由首页拖动决定；
  固定时取 `pinSeq+1` 追加到末尾，取消固定归 0，重新固定又排到最后。
  **为什么不能沿用"最近使用倒序"**：那样每用一次某个固定项，它就跳到最前面，用户摆好的排版自己会跑。
  `pinOrder == 0` 只出现在老版本的历史里，`Open` 时按 `lastUsed` 倒序补一遍再落盘。

## 6. 局域网文件互传（对应 dashbeam，已实现）

dashbeam 的骨架是 iroh（QUIC + TLS1.3 + BLAKE3 分块 + ticket + mDNS + 可漫游续传）。
Go 侧一期**没有**照搬这套，落地的是"浏览器下载型"，理由是手机端零安装、实现面最小：

> **方向是单向的：只发不收。** 早先的 `/u` multipart 上传、`%LOCALAPPDATA%\Baozi\inbox\` 落盘目录、
> 首页底部的上传表单已经整条拆除——共享页面对局域网里的人是只读的，任何 POST 都返回 404。
> 保留上传等于把自己的磁盘对整段局域网开一个写入口，用户明确不需要，也不再是"分享"的语义。

1. **服务端**：`internal/share` 起随机端口的 `http.Server`，首页在 `/`，访问码挂在路径前缀 `/s/<code>/`。
   - **链接是一次性的**：`Start(bind, ttl)` 起服务时就定下失效时刻（`<0` 用 `DefaultTTL = 1 小时`），
     `time.AfterFunc(ttl, closeExpired)` 到点直接 `srv.Close()`，之后连端口都连不上；
     请求路径里另有 `Expired()` 检查兜边界（返回 410）。界面「链接有效期」下拉默认 1 小时，
     可选 10 分钟 / 30 分钟 / 2 小时 / 8 小时 / 24 小时，存进 `config.share.ttlMinutes`。
     不做"只要不停止就一直分享"——分享是临时通道，用完即弃。
   - **不再一打开窗口就开服务**：先「添加文件 / 添加文件夹」攒候选（`shareWindow.pending`），
     再点「生成链接」，才是"选文件 → 分享 → 得到一条链接"的顺序。
   - **访问码是 4 位**，字符集排除易混淆字符（`0 O o`、`1 I l i`、`5 S`、`2 Z`、`8 B`），
     用户要能照着窗口把码敲进手机；**校验区分大小写**（同码不同大小写视为不同的码，
     路由层 `subtle.ConstantTimeCompare` 直接比原始字节，不做折叠）。
     生成用 `crypto/rand` + 拒绝采样消除取模偏差。
   - 4 位 ≈ 19 bit，远弱于原来的 16 字节 hex，所以加了**失败熔断**：连续输错 `maxTokenFails = 20`
     次直接 `srv.Close()` 停服，`Breached()` 供界面提示，点「重新生成链接」换新码。
   - **URL 必须带端口**：`http://<ip>:<port>/s/<code>/`。端口是随机的，早先漏拼端口导致链接落到 80 端口打不开
     （`TestStartBindSpecific` 就是被这个 bug 抓出来的）。
2. **绑定网卡**：`Start(bind, ttl)`，`bind` 为空 = 所有网卡（`":0"`），否则听 `bind:0`。
   界面下拉列出 `Bindings()`（接口名 + IPv4，标注虚拟网卡），选择存进 `config.share.bind`，换网卡会重启会话。
   `LANIP()` 的挑选顺序：私有地址且非虚拟网卡 → 任意私有地址 → 第一个候选；
   虚拟网卡按接口名关键词判定（`vmware` / `vmeter` / `virtualbox` / `hyper-v` / `wsl` / `docker` / `mumu` 等）。
   这一步是必须的：装了 VMware / MuMu 的机器上，自动挑到的很可能是虚拟网卡地址，对端根本不存在。
3. **下载**：`/d/<name>`，文件走 `http.ServeContent`（支持 Range）；目录**实时打包成 zip 流**边压边发，不落临时文件。
   只分享一项时根路径直接 302 到 `d/<name>`（打开链接就下载）；多项才渲染列表页。
   **下载链接一律写成绝对路径 `/s/<code>/d/<name>`**：早先写的是相对 `d/<name>`，用户把地址栏结尾斜杠
   丢掉（`/s/ABCD`）再打开页面时，浏览器会把链接解析成 `/s/d/<name>`（把访问码那段当成文件名），点下去必然 404。
   现在 `/s/<code>`（无斜杠）301 到带斜杠版本，两条路都兜住。
4. **单向**：路由只有 `""`/`/`（列表页或单项直下）与 `d/<name>`（下载）两支，其余一律 404。
   服务端不持有任何目录用于写入，`Session` 里没有 inbox 字段，`Start` 也不接收落盘路径。
5. **发现**：不广播 mDNS，直接展示绑定的（或自动挑的）IPv4 + 二维码（`skip2/go-qrcode`，纯 Go 无 cgo），手机扫码即可。

与 dashbeam 的差距（明确没做）：无 QUIC、无 NAT 打洞、无公网中继、无分块哈希与断点续传、无端到端加密
（明文 HTTP + 一次性令牌，仅限可信局域网）。Windows 防火墙首次放行需用户点一次确认。

## 6b. 跨网直传（WebRTC，已实现）

局域网那套只在同一网段有用。跨网另起一条：`internal/p2p` + `signal/`（一个 Cloudflare Worker）。

**分工**：局域网走 HTTP（对端零安装、浏览器下载）；跨网走 WebRTC（两端都要装 Baozi、要打洞）。
两条路并存，互传窗口里用页签切换。

**信令**：`signal/src/index.ts` 是一个 Worker + 一个 `Room` Durable Object。
短码就是房间名，`idFromName(code)` 保证同一个码落进同一个实例，两端各挂一条 WebSocket，
DO 只做纯转发（自己只发 `peer_joined` / `peer_left`）。免费计划限制是 **只能用 SQLite 后端的 DO**，
所以 `wrangler.toml` 的 migration 必须是 `new_sqlite_classes = ["Room"]`。

**为什么不用 workers.dev**：`*.workers.dev` 在大陆被 SNI 阻断（TCP 通、TLS 握手失败）。
绑自有域名（`signal.1jia.life`）走 Anycast 就正常，这是实测出来的，不是猜的。

**为什么不做 trickle ICE**：双方把候选收全了再塞进 SDP 一次性发过去，信令上只剩
`offer` / `answer` 两条消息。多等一两秒，换掉"候选先到、描述后到"的乱序处理——
那类 bug 只在特定网络下偶发，非常难查。

**传输协议**：三种帧。
1. 文本 `{"type":"manifest","items":[{"path","size"}]}`；
2. 二进制 `[4 字节文件序号][32KB 数据]`——靠 SCTP 的有序可靠省掉偏移和逐块确认；
3. 文本 `{"type":"done"}`。

没有校验和：链路层已经管到字节级，再加一层只是自我安慰。
发送端按 `dc.BufferedAmount()` 做反压（超过 1MB 就等），接收端按序号写盘。
**路径必须清洗**（`cleanRel`）：对端构造 `../` 或 `C:` 都跳不出目标目录，重名自动加 `(1)`。

**没做的**：TURN 中继。`Options.TURN` 留了口子，Cloudflare Realtime TURN 免费额度 1TB/月，
需要在账号里生成短期凭证。对称 NAT 现在会直接报"两端网络都没打通"。

**浏览器当接收端**（`signal/src/receiver.ts`）：信令服务顺带托管一个页面 `/r?code=<短码>`，
里面那段 JS 自己 `new RTCPeerConnection`、自己 `createAnswer`、自己解析同一套帧格式。
于是接收方不用装 Baozi——这对手机最省事，也把"跨网"这件事的入口从"先装个软件"降到"点开一条链接"。

几个必须对齐的点，改任何一处都要两边一起改：

- **帧格式照抄** `internal/p2p/transfer.go`：小端 4 字节文件序号 + 32KB 数据，
  文本帧是 manifest / done。所以 `binary.LittleEndian` 与 `DataView.getUint32(0, true)` 必须成对；
- **不做 trickle**：页面也要等 `iceGatheringState === "complete"` 再把 answer 发出去，
  给 6 秒兜底（Go 端 `gatherBudget` 是 8 秒，留余量）。发早了 Go 会拿到一条没有候选的 answer；
- **角色固定**：发送方永远是装 Baozi 的那台（它有文件、也只有它能读磁盘），
  浏览器永远 `role=receiver`。所以 Go 侧不用改一行——它发 offer，浏览器回 answer；
- **落盘是攒出来的**：页面把每个文件的块塞进 `Blob`，收完再 `a.download` 触发保存。
  Chrome 桌面实测会自动落到下载目录；手机浏览器会拦无手势的下载，所以收完同时亮一个
  「保存文件」按钮作为兜底。这也是为什么大文件建议用客户端收——那边是边收边写。

## 7. 读取文件管理器当前目录（对应 Lertaro `IActivePathCollector`）

只读、不接管对方搜索。按目标进程分路：

1. **资源管理器（Explorer）** —— 已实现。读地址栏控件（见 8.1），
   不照 Lertaro 那样走 `ShellWindows` COM。
2. **Total Commander** —— 已实现。走 TC 的 `WM_COPYDATA` 远程控制协议
   （`TTOTAL_CMD` + `'G'+256*'W'` 查询 / `'C'+256*'D'` 换目录，见 8.1.2），
   不解析窗口标题 —— 用户自定义标题模板会让标题解析失效，只留作兜底。
3. **Directory Opus** —— 已实现（`opus.go`）。读文件显示容器的地址栏
   （`dopus.lister` → `dopus.filedisplaycontainer` → `dopus.ctl.treepath`，见 8.1.3）。
   不用 `dopusrt.exe /acmd`：那要起一个进程、走剪贴板或临时文件，比读窗口慢得多。
4. **XYplorer** —— 已实现（`xyplorer.go`）。没有专用查询命令，走它的脚本通道
   （`WM_COPYDATA` + UTF-16 脚本，脚本自己 `copydata` 回投答案，见 8.1.4）。
5. **xplorer2** —— 已实现（`xplorer2.go`）。没有远程接口，只能读地址栏 `ComboBox`
   的 `WM_GETTEXT`，且必须只看可见的那个（见 8.1.5）。
6. **tessoa** —— 已实现（`tessoa.go`）。它是 Rust + wgpu **自绘**的跨平台文件管理器
   （tessoa.cn，本机实测 v0.30.2），窗口里没有任何标准控件：类名是自建的 `Window Class`，
   UIA 树用 ControlViewWalker 和 RawViewWalker 走下来**都只有顶层一个节点**，
   地址栏那套读法完全用不上。改走它自己提供的命令行 RPC：`tessoa.exe --print-cwd`
   会把查询转给主实例、打印当前活动窗格的目录后立即退出（见 8.1.6）。
7. **OneCommander / Files** —— **不做**。两者都只能走 UI Automation（类名由 WPF /
   WinAppSDK 运行时生成，含 guid 或全机共用，进程名 + UIA 是唯一可靠路径）。
   Go 里手搓 IUIAutomation 的代价与本机收益不成比例，且这两种用户都没装。
8. **Double Commander** —— 未做。Lertaro 没有它的 collector，没有可靠读法可抄。

统一出口是 `internal/win/sources.go` 的 `sourceKind(hwnd)` + `sourceFolder(kind, hwnd)`，
跟随器侧再包一层 `(*DialogFollow).isPathSource` / `folderOf`（见第 8 节），
新增一种来源只要在 `SourceNames` / `sourceKind` / `sourceFolder` 三处加一行。
六种来源都能在设置里单独关掉（`config.pathSources`，见第 11 节）。

## 8. 打开/保存对话框跟随文件管理器（已实现，对应 Lertaro `IFileDialogAdapter` + `IActivePathCollector`）

语义是**全局自动跟随**，不经面板、不需要用户在 Baozi 里选任何东西：

> 打开记事本「另存为」，框停在「我的文档」→ 打开资源管理器把目录切到「下载」→
> 切回那个「另存为」框，**它的路径自己变成「下载」**。

三块拼起来：每隔 200ms 看一眼前台窗口（`internal/win/follow.go`）、
前台是可提供目录的程序（六种来源，见第 7 节）就读出它的当前目录、
前台是文件对话框就按版本号决定要不要跳过去。

### 8.1 记住文件管理器的目录

#### 8.1.1 资源管理器（`ExplorerFolder`）

读的是**地址栏控件**，不是 `ShellWindows` COM：

```
EnumChildWindows( CabinetWClass ) → class == "Breadcrumb Parent" → 它的 ToolbarWindow32
  → WM_GETTEXT（SendMessageTimeout，500ms 超时）
  → 剥掉 "地址: " / "Address: " 前缀
  → resolveFolderText()：绝对路径就校验存在性；本地化名字查已知文件夹表
```

Lertaro 的 `ExplorerPathCollector` 走的是 `ShellWindows`（CLSID `9BA05972-…`）+
`Document.Folder.Self.Path`，还要用 `ShellTabWindowClass` 判多标签里的活动标签。
Go 里手搓 `IDispatch` 又贵又脆，而**资源管理器的地址栏和文件对话框是同一套控件**，
window text 直接就是路径，一条 `EnumChildWindows` 解决。多标签时读 `CabinetWClass`
顶窗（`ShellTabWindowClass` 自己那条读出来是空字符串），所以先用 `GetAncestor(GA_ROOTOWNER)` 上溯。

**已知文件夹要单独还原**（`internal/win/knownfolder.go`）：在「下载 / 文档 / 音乐 /
图片 / 视频 / 桌面」这类文件夹的根上，地址栏给的不是路径而是本地化名字 ——
实测读到 `地址: 音乐`、`地址: 文档`。`isDir("音乐")` 为假，跟随器就只能当没读到，
这正是用户报的"切到下载/文档路径不变"。

名字的来源必须和 shell 用的一致，试过三种只有一种对：

| 取法 | 本机结果 |
|---|---|
| `SHGetFileInfoW(路径, SHGFI_DISPLAYNAME)` | 只给 `Documents`。本机文档被重定向到 `D:\Backup\Documents`，重定向过的目录拿不到本地化名 |
| `SHGetNameFromIDList(pidl, SIGDN_NORMALDISPLAY)` | 对这批目录返回空（`hr != 0`） |
| **`FolderDescriptions\{FOLDERID}\LocalizedName` + `SHLoadIndirectString`** | **`文档` / `下载` / `音乐` …，正是地址栏上那几个字** |

所以做法是：12 个 FOLDERID → `SHGetKnownFolderIDList` 取路径，同时读注册表
`HKLM\SOFTWARE\Microsoft\Windows\CurrentVersion\Explorer\FolderDescriptions\{GUID}\LocalizedName`
（值形如 `@C:\Windows\system32\windows.storage.dll,-21770`），用 **`shlwapi.dll` 的
`SHLoadIndirectString`** 解析成本地化名。这是 shell 自己的字符串资源，换任何系统语言
都自动跟着走，不用硬编码"文档"这种词。表缓存一次（`knownOnce`），之后只是查 map；
另外把目录末段名也塞进去，给英文系统 / 没注册本地化名的条目兜底。

注意 `SHLoadIndirectString` 在 **`shlwapi.dll`**，不在 `shell32.dll`（写错会
`Failed to find ... procedure` 直接 panic）。

#### 8.1.2 Total Commander（`TotalCommanderFolder`）

照 Lertaro `Plugins\TotalCommander\Win32\Win32Helper.cs` 的远程控制协议：

| 环节 | 细节 |
|---|---|
| 窗口 | 类名 `TTOTAL_CMD`（主窗口，不是面板子窗） |
| 请求 | `WM_COPYDATA`，`dwData = 'G'+256*'W'`（22343），`wParam = 接收窗口`，`lpData` 是 **ASCII** 的 `"SP\0"`（源/活动面板）或 `"TP\0"` |
| 回复 | `WM_COPYDATA`，`dwData = 'R'+256*'W'`（22354），内容是 **UTF-16** 路径，回给 `wParam` 指定的窗口 |
| 换目录 | `dwData = 'C'+256*'D'`（17475），`lpData = [UTF-8 BOM]path 0x0D 0x00 "S" 0x00`（`"S"` 表示作用于活动面板；**`S` 后的尾 NUL 必须计入 `cbData`**，否则标志位解析不稳）。Baozi 只读不写，这条只给测试探针用 |

命令串必须是 **ANSI** —— 只有"回复用 UTF-16"这一半由 `'W'` 决定，
用 UTF-16 发命令 TC 不理你（Lertaro 源码原话）。

两个实测踩出来的硬约束，缺一个就是"请求发成功、路径永远为空"：

1. **接收窗口必须和发送在同一个 OS 线程。** TC 是同步 `SendMessage` 回来的，
   只有"发送线程自己的窗口"才会被**嵌套投递**，发完立刻就能读到；
   窗口开在别的线程，回复进那个线程的队列，这边读不到。
2. **只能用 `SMTO_ABORTIFHUNG`，不能带 `SMTO_BLOCK`。** 带 BLOCK 时 Windows
   不允许调用线程处理回到自己队列的消息，回复照样被丢掉。
   `tools/tcprobe` 把四种组合跑了一遍，结论是：

   ```
   SendMessage                      -> 回调次数=1 路径="D:\Backup\Documents\"
   SendMessageTimeout(no BLOCK)     -> 回调次数=1 路径="D:\Backup\Documents\"
   SendMessageTimeout(BLOCK)        -> 回调次数=0 路径=""          ← 之前用的就是这个
   SendMessage(wParam=0)            -> 回调次数=1 路径="D:\Backup\Documents\"
   ```

这两条不是 TC 独有的，凡"发一条消息、对方同步回投答案"的协议都适用，
所以收发那一套抽到了 `internal/win/copydata.go`（`queryCopyData`），
**Total Commander 和 XYplorer 共用**：`LockOSThread` → 注册一次窗口类
`BaoziQueryWnd` → 建 message-only 窗口 → `build(receiver)` 生成 payload →
`sendMsgNoBlock` → 读回复 → `DestroyWindow`。`build` 是回调而不是现成的字节：
XYplorer 的脚本必须把接收窗口的 hwnd 拼进去，只能建完窗口才生成。
`sendMsg` 的语义因此仍是两个：`sendMsg`（带 BLOCK，给对话框写文本用）和
`sendMsgNoBlock`（查询专用）。两者都是 `SendMessageTimeoutW` + 500ms 超时，
对方卡住也不会拖住跟随器。

高权限场景另外调了 `ChangeWindowMessageFilterEx(receiver, WM_COPYDATA, MSGFLT_ALLOW, 0)`：
Baozi 若以管理员身份运行，UIPI 会静默丢掉普通权限进程的回复。

#### 8.1.3 Directory Opus（`OpusFolder`）

照 Lertaro `Plugins\DirectoryOpus\DirectoryOpusPathCollector.cs`。窗口层次是：

```
dopus.lister（lister 主窗口，一个 Opus 窗口）
  └ dopus.filedisplaycontainer（文件显示容器，一个标签页一个）
       └ dopus.ctl.treepath（活动标签的地址栏，window text 就是完整路径）
```

非活动标签没有地址栏，读容器自己的 window text（Opus 把每个标签的目录放在自己的标题上）。

**双栏怎么选"用户正在看的那一栏"**：先按 lister 标题唯一定位某一栏（标题前缀就是
目录名），标题判断不出来再看键盘焦点落在哪个容器里（`GetGUIThreadInfo` 取
`hwndFocus`，再沿父链上溯）。两条都判不出就**返回空、不导航** ——
Lertaro 的第三层"按几何距离猜横竖分屏"故意没做：跳到另一栏的目录，
比这一下不跳更让人难受。

另外过一遍 `resolveFolderText`：Opus 报出来的可能是本地化拼写（`C:\用户\…`），
那不是存在的路径。Lertaro 的 `LocalizedPathResolver`（逐段翻译目录名）没做，
没有装 Opus 的机器可验，宁可不写这段猜逻辑。

#### 8.1.4 XYplorer（`XyplorerFolder`）

照 Lertaro `Plugins\Xyplorer\Win32\Win32Helper.cs`。窗口类名 `ThunderRT6FormDC`
（VB6 主窗体，任何 VB6 程序都用这个），所以必须再过一道**进程名 `xyplorer*`** 才算数。

XYplorer 没有 TC 那样的查询命令，它暴露的是"跑一段 XYplorer 脚本"：

| 环节 | 细节 |
|---|---|
| 请求 | `WM_COPYDATA`，`dwData = 0x00400001`（RUN_SCRIPT），`lpData` 是 **UTF-16** 的脚本文本 |
| 脚本 | `::$return = "<curpath>"; copydata <接收窗口>, "$return", 2;` |
| 回复 | 脚本自己用 `copydata` 回投，`dwData = 2`，内容是 UTF-16 路径 |

`$return` 在引号里是给 XYplorer 做字符串替换的，不是字面量；接收窗口的 hwnd 必须
以十进制数字直接写进脚本 —— 这就是 `queryCopyData` 的 `build` 要拿到 receiver 的原因。
脚本按 UTF-16LE 编码、不带尾 NUL（和 .NET `Encoding.Unicode.GetBytes` 一致）。

#### 8.1.5 xplorer2（`Xplorer2Folder`）

照 Lertaro `Plugins\Xplorer2\Win32\{Win32Helper,WindowProbe}.cs`。框架类
`ATL:ExplorerFrame`，同样要配进程名 `xplorer2*`。xplorer2 没有远程控制接口，
只能读窗口：地址栏是一个 `ComboBox`，**`WM_GETTEXT` 的回复才是路径**，
同一个句柄 `GetWindowText` 返回空（Lertaro 专门量过这一点）。

两个坑：

1. 一帧里有**两个** `ComboBox` —— 传统地址栏（显示面包屑时它被隐藏）和面包屑自己那个。
   隐藏的那个还留着上一次访问的路径（实测活动面板在 `D:\Notes` 时，隐藏的传统栏
   仍报 `C:\Windows`）。所以**只看可见的**：看不见的一律不算，宁可返回空。
2. 编辑中途 `ComboBox` 会短暂报出纯数字垃圾，还有 `::` 开头的 shell 命名空间
   （此电脑之类），都不是文件系统路径，要滤掉（还要 `filepath.IsAbs` + `isDir`）。

面包屑那个自己的窗口在 ATL 里是**无名类**（`ATL:%p`），只能按**文字**认它 ——
类名对不上，window text 是 `CrumbBar`。认出来之后，焦点面板那条 `ComboBox`
的父窗口就是它，优先用；其余可见的 `ComboBox` 当兜底。

> **这三种（Opus / XYplorer / xplorer2）没有在真机上验证过** —— 用户明确说这几样
> 他一个都没装。实现完全照 Lertaro 的 collector 写，能验证的只有：
> 未安装时 `IsDirectoryOpus` / `IsXyplorer` / `IsXplorer2` 一律为假（`tools/folderprobe`
> 在本机跑出来只有资源管理器与 TC），因此不会把普通窗口误判成目录来源、不会乱跳；
> 读不到时返回空串，跟随器按"没读到"处理，不导航也不崩。
> 装了这几样的人如果读出来不对，`tools/folderprobe` 会打印容器 / `ComboBox` /
> 地址栏原文，照着改就行。

#### 8.1.6 tessoa（`TessoaFolder`）

**已在本机真机验证**（v0.30.2，管理员权限运行）。tessoa 是 GPU 自绘的，
所以第一步先确认常规手段用不了：

- 窗口类名是自建的 `Window Class`（不是 `CabinetWClass` 那类系统类），
  按其类名认人不可靠，只能按进程名认；
- 窗口里没有 `Breadcrumb Parent` / `ToolbarWindow32`；
- 它**确实**加载了 `uiautomationcore.dll`，但 `AutomationElement.FromHandle`
  之后用 `ControlViewWalker`、`RawViewWalker` 两种视图走，都只有顶层 Window 一个节点
  （自绘 UI 没建无障碍树）。所以 UIA 这条路也是死的。

它自己给了命令行 RPC。二进制里能看到这条命令族的名字：
`open / open-window / reveal-window / dev-extension / open-url / query-cwd`，
命令行参数是 `--print-cwd`（还有 `--print-cwd=` 的带参形式）。实测：

```
D:\1tools\tessoa.exe --print-cwd        →  打印当前活动窗格的目录后 exit 0
D:\1tools\tessoa.exe --open D:\work\baozi → 日志里 "已转发给主实例（pid=7016）→ 本进程退出"
```

也就是说 `--print-cwd` 问的是**已经在跑的那个实例**，不是"新进程自己算出来的目录"：
实测 `--open` 换目录之后立刻再 `--print-cwd`，返回值跟着变了（`D:\` → `D:\work\baozi`）。

代价与缓存：

| 场景 | 单次 | 说明 |
|---|---|---|
| 热态 | **16 ms** | 0.1MB 进程启动 + 管道往返，实测 6 次 16/16/16/16/16/16 |
| 冷态 | **43 ms** | 文件不在系统缓存里时 |

跟随器三处会调它：前台切到 tessoa 那一下、**tessoa 的窗口标题变了**（见下）、
以及文件对话框弹出时对"上一次活动的那个来源"现场重读一次（这一次不吃缓存）。
`tessoaCacheTTL = 400ms` 让挨得很近的轮询调用共用一个结果，省掉一次进程启动。

**4 窗格 / 切布局：前台窗口根本不变。** tessoa 是单窗口自绘，切窗格、切布局、
在同一个窗格里进目录，`GetForegroundWindow` 一直是同一个 hwnd。跟随器原来只按
"前台窗口变化"决定要不要读目录，于是 4 窗格布局下把焦点挪到左下角、或者切换布局，
它完全看不见 —— 日志里只有"记住目录"停在上一次，打开框自然不跟着走。
现在拿**窗口标题当变更信号**：tessoa 的标题是 `<活动目录名> — <布局名> — tessoa (管理员)`
（实测 `1tools — normal — tessoa (管理员)`），标题一变就先 `--print-cwd` 问完整路径
（节流 500ms，读标题本身要发 WM_GETTEXT，常态零开销）。标题只有目录名、两个不同目录
同名时会漏一次，靠对话框弹出时那次现场重读兜住。

三个必须处理的坑：

1. **自更新会让进程镜像改名**。tessoa 更新时先把正在运行的 exe 改名成 `tessoa.old.exe`，
   再写入新的 `tessoa.exe` —— 进程还活着，但它现在的镜像名是 **`tessoa.old`**。
   实测本机 pid 7016 就是 `D:\1tools\tessoa.old.exe`。所以：
   - `IsTessoa` 的判据是 `tessoa` 本身或 `tessoa.*`（排除插件进程 `tessoa-av` / `tessoa-office`）；
   - 发查询时**优先用窗口所在目录里的新 `tessoa.exe`**（它启动时认得同一个主实例锁，
     会转发过去而不是再开一个窗口），新 exe 不在才退回进程自己的镜像。
2. **没有实例可转发时会自己开窗口**。也就是说如果 tessoa 已经被关掉而我们还在问，
   等于替用户把软件打开了。所以查询前先 `IsWindow(root)` 确认窗口还在；
   拿不到窗口句柄直接返回空。
3. **欢迎页没有真实目录**。tessoa 停在 `@Welcome` 标签时 `--print-cwd` 给的是盘根
   （本机是 `D:\`）。它仍然是个合法目录，跟随器照用；这种情况下不要指望它跟着切。

多开实例的边界：`--print-cwd` 总会找"主实例"，所以用户同时开了两个 tessoa 时，
读到的是主实例的目录，可能与当前前台那个窗口不一致。这是它 CLI 的语义决定的，
我们不做额外猜测（猜错会把对话框拽到用户没在看的目录）。

读回来的输出还要过一遍 `tessoaParse`：挡空行、多行、带引号、带 `\\?\` 前缀；
**并且必须是真实存在的目录**才认。盘根 `D:\` 先按原样试，试不过才去掉尾部分隔符 ——
否则 `D:\` 会被削成 `D:`，那是"当前盘当前目录"，不是盘根。

### 8.2 什么时候跳（照抄 `FileDialogNavigationTracker.cs`）

状态只有两样：`lastDir`（最近一次文件管理器待过的目录）和 `version`（目录变一次 +1），
外加每对话框一个 `seen[hwwnd] = 它上次同步时的 version`：

| 情形 | 动作 |
|---|---|
| 前台是文件管理器（六种来源之一） | 记下它的窗口（`lastSource`），顺手读一次目录更新 `lastDir` |
| **对话框成为前台，且上一次活动的是那个文件管理器窗口** | **现场重读**它的目录（不吃缓存），再按下面的版本规则决定跳不跳 |
| 前台是文件管理器，目录与 `lastDir` 不同 | `lastDir = dir`，`version++` |
| 对话框**第一次**成为前台 | 直接 `NavigateDialog(hwnd, lastDir)` |
| 再见到该对话框，且 `seen[hwnd] != version` | 记新版本 + `NavigateDialog(hwnd, lastDir)` |
| 再见到该对话框，版本没变 | 不动 |

「第一次就直接跳」是唯一偏离 Lertaro 原文的地方，也是必须的：原来照抄"第一次只登记、
等版本再变一次才跳"，而版本在框出现那一刻已经被现场重读升到最新了 —— 于是**每个新
框都只会被登记、永远不跳**，用户看到的就是"打开框根本不跟随"（本机日志里 34 条
"送到"全是"框开着的时候又在文件管理器里换了目录"这种碰巧成立的路径）。
第一次跳用的是**刚刚现读**的 `lastDir`，所以库里那个"程序自己指定了目录，不该覆盖"
的担心并不成立：我们要跳的正是用户此刻在文件管理器里看的位置。

「版本没变就不动」是 Lertaro 源码里明写的坑：用户手动在框里换过目录，
下次切回来不能被打回旧目录。

**「切回时现场重读」这一条是这次补上的**，对应 Lertaro 里那行
`previousWasPathProvider && readProviderPath()`。原因：如果只在"轮询看到文件管理器"
那一刻读目录，用户换目录的瞬间可能正好落不到那一次采样上 ——
表现就是"第一次能跟，换个目录再切回去就不跟了"。改成切回对话框的时刻现读，
读到的永远是最新的；而且只记住 hwnd、不缓存内容。反过来，上一次活动的若不是
文件管理器（比如中间切过浏览器），就不重读、也不跳，避免把人拽回旧目录。

`lastExplorer` 已改名 `lastSource`：现在"目录来源"有六种，判断"上一次活动的是不是
同一个来源窗口"和"能不能现读"都走 `(*DialogFollow).isPathSource()` /
`folderOf()` 两个分派点（它们是方法不是包级函数了，因为要先按设置过滤来源）。

### 8.3 怎么跳（`NavigateDialog`，照 `StandardFileDialogAdapter`）

不是注入 DLL，而是**往"文件名"框写完整路径，再让它回车**
（新式 Common Item Dialog 的文件名框仍是一个普通 `Edit`，进程外能写）。

```
IsFileDialog(hwnd)?                       ← #32770 且内部有 "Breadcrumb Parent"
EnumChildWindows（递归） → 第一个 class == "Edit"    ← 文件名框（ComboBoxEx32/1148 里面）
  → 目录补尾 "\"，WM_SETTEXT 写入
  → 父窗口 WM_COMMAND((EN_CHANGE<<16)|ctrlID)
  → 回读校验；不一致就绝不回车（否则会把框里原来那个文件名提交掉）
  → 250ms 后 SetFocus + EM_SETSEL + WM_KEYDOWN/WM_KEYUP VK_RETURN
```

三个必须注意的点：

- **必须 `EnumChildWindows` 递归**。`FindWindowExW` 只扫直接子窗口，而 `Breadcrumb Parent`
  和文件名框都在两三层以下 —— 用它就会"明明有对话框却认不出来"。
- **绝不 `AttachThreadInput`**。它把本线程和对话框线程的输入队列绑死；
  而 Go 的 goroutine 会在 OS 线程之间迁移、那个线程之后还会被 runtime 复用，
  等于把对方的输入队列挂在一个不再处理消息的线程上 —— 对方窗口就此永久无响应。
  实测代价：在记事本的"另存为"上直接把记事本按死（用户报的"切回 notepad 卡死"）。
  现在一个 `AttachThreadInput` 都没有，也不自己抢前台（切回对话框这个动作本身
  就保证了它是前台）。
- **跨进程消息一律带超时**：`SendMessageTimeoutW` + `SMTO_BLOCK|SMTO_ABORTIFHUNG` + 500ms。
  裸 `SendMessage` 只要对方卡一下就把调用方（进而把整个跟随器）拖死。

面板那条手动路（唤起时记住对话框、`Enter` 改成"送过去"）已经删掉：它不符合这个语义，
而且会顶掉 `Enter` 原本的"打开文件"。开关是 `config.json` 的 `"dialogFollow": false`。

实测（2026-11-07，WinForms `OpenFileDialog` 靶子 `dist\dlg-target.ps1`，
`tools/followtest` 在一个进程里连跑两轮 —— 第二轮专门覆盖"再换个目录再切回来"）：

```
① 另存为框 0x4c0426 初始地址: "地址: 文档"
② 资源管理器切到 D:\work\baozi
③ +500ms 地址: "地址: D:\work\baozi" 对话框还在=true 卡死=false
④ 资源管理器切到 C:\Windows\System32
⑤ +500ms 地址: "地址: C:\Windows\System32" 对话框还在=true 卡死=false
```

（六次采样里 `卡死` 全是 false，`IsHungAppWindow` 判的。）

**已知文件夹那条**（这一轮补的，正是用户报的"切到下载/文档路径不变"）：

```
① 另存为框 0xa135a 初始地址: "地址: 文档"
② 资源管理器切到 D:\work\baozi
③ +500ms 地址: "地址: D:\work\baozi"      对话框还在=true 卡死=false
④ 资源管理器切到 D:\Backup\Downloads → 地址栏显示 "地址: 下载"
⑤ +500ms 地址: "地址: 下载"                对话框还在=true 卡死=false
```

**Total Commander 那条**（`FOLLOW_TC=1`，TC 11.50 x64；用 CD 命令替它换目录）：

```
② 让 TC 前台并切到 D:\work\baozi        → TC 当前目录="D:\work\baozi"
③ +500ms 地址: "地址: D:\work\baozi"    对话框还在=true 卡死=false
④ 让 TC 前台并切到 D:\work\baozi\internal → TC 当前目录="D:\work\baozi\internal"
⑤ +500ms 地址: "地址: D:\work\baozi\internal" 对话框还在=true 卡死=false
```

Baozi 同一时刻的日志：

```
跟随: 第一次见到对话框 0x4c0426，只登记（版本 0）
跟随: 记住目录 "D:\work\baozi"（版本 1）
跟随: 对话框 0x4c0426 版本 0→1，送到 "D:\work\baozi"
对话框跟随: 0x4c0426 -> D:\work\baozi
跟随: 记住目录 "C:\Windows\System32"（版本 2）
跟随: 对话框 0x4c0426 版本 1→2，送到 "C:\Windows\System32"
对话框跟随: 0x4c0426 -> C:\Windows\System32
```

**接完五种来源之后的回归**（2026-11-07 23:35 / 23:37，同一天连跑两轮；
这一轮把收发抽成 `copydata.go`、分派改成按来源开关过滤的方法，所以必须重测一遍）：

```
# FOLLOW_TC=1（TC 当来源）
① 另存为框 0x25e0e5e 初始地址: "地址: 文档"
② TC 切到 D:\work\baozi            → TC 当前目录="D:\work\baozi"
③ +500ms 地址: "地址: D:\work\baozi"        对话框还在=true 卡死=false
④ TC 切到 D:\work\baozi\internal    → TC 当前目录="D:\work\baozi\internal"
⑤ +500ms 地址: "地址: D:\work\baozi\internal" 对话框还在=true 卡死=false

# 资源管理器 + 已知文件夹（FOLLOW_TITLE=下载）
① 另存为框 0xc7114c 初始地址: "地址: 文档"
④ 资源管理器切到 D:\Backup\Downloads → 面包屑显示 "地址: 下载"
⑤ +500ms 地址: "地址: 下载"          对话框还在=true 卡死=false
```

两轮各六次采样，`卡死` 全是 false；跟随器启动日志同时也报了来源清单：

```
对话框跟随: 已启动（来源：资源管理器 / Total Commander / Directory Opus / XYplorer / xplorer2）
```

**五种来源的识别在同一天用 `tools/folderprobe` 核过**（本机没装 Opus / XYplorer /
xplorer2，所以只应看到资源管理器与 TC，一个都不能多）：

```
[TC] 发 0x1d07fc：ok=true dwData=0x5752（='R'+256*'W'） 回复路径="D:\Backup\Documents\"
[TotalCommander] 0x1d07fc  目录="D:\Backup\Documents"
[资源管理器] 0xa1214 title="System32" 地址栏原文="地址: C:\Windows\System32" 解析目录="C:\Windows\System32"
[资源管理器] 0x71278 title="梦境寻忆" 地址栏原文="地址: D:\Backup\Downloads\梦境寻忆" 解析目录同上
[资源管理器] 0x8830e02 title="本地磁盘 (D:)" 地址栏原文="地址: D: \" 解析目录="D:\"
```

（这同时是"不会误判"的证据：机器上跑着一堆普通程序，Opus / XYplorer / xplorer2
三条判定一个都没触发。）

留下的探针：`tools/followtest`（整条链路，`FOLLOW_NOTEPAD=1` 改用真记事本、
`FOLLOW_SECOND=<目录>` 指定第二轮目标、`FOLLOW_TC=1` 改用 Total Commander、
`FOLLOW_TITLE=<标题>` 指定已知文件夹窗口的本地化标题）、`tools/exploredump`
（资源管理器目录）、**`tools/folderprobe`（五种来源的识别 + 目录读取 + 已知文件夹映射表；
装了 Opus / XYplorer / xplorer2 时还会打印容器、`ComboBox` 与地址栏原文）**、
**`tools/tcprobe`（TC 协议单测，四种投递方式对比；`-cd <路径>` 只换目录）**、
`tools/dlgprobe2`（枚举 + 单次导航 + 读文件名框）、`tools/dlgdump`（打印对话框全部子控件）。

> 记事本的自动化发键在这台机器上抢不到前台（`Ctrl+Shift+S` 四次都没弹出另存为框），
> 所以"记事本现场"只能由用户手动复现；功能路径与 WinForms 靶子完全相同
> （都是 `#32770` + Common Item Dialog）。

### 8.4 那些弯路：为什么只能走"文件名框 + 回车"

DLL 注入那条（`WH_CBT` 捕获 + `IShellView::Navigate`）需要往目标进程里塞代码，
本机没有 C 编译器（`gcc/g++/cl/tcc/zig/clang` 全无），Go 的 `-buildmode=c-shared` 出 DLL
再改 COM vtable 也不现实，还会被杀软盯上。放弃。

早期对着一台真实 Win11「打开」对话框（记事本）试过的所有手段：

| 手段 | 结果 |
|---|---|
| `SetWindowTextW` 写地址栏 `Edit(id=41477)` | **文本进去了**（读回来确认），但地址栏与列表不动 |
| `WM_COMMAND(CBN_SELENDOK / CBN_KILLFOCUS)` 发给 band / ComboBoxEx32 / 对话框 | 全部被无视 |
| `SendMessage`/`PostMessage` 造 `VK_RETURN`（补扫描码、补 `WM_CHAR`） | 无反应 |
| 真实 `SendInput`（Unicode 键事件，26 个全部被接受）写**地址栏** | 编辑框内容不变 |
| `SetFocus`（含 `AttachThreadInput`） | 无效；`GetGUIThreadInfo` 读对话框线程 `HwndFocus = 0x0` |
| `Ctrl+L` | 这版对话框不切编辑态（地址栏 `Edit` 仍 `visible=false`） |
| 真实鼠标点面包屑右侧空白 | **有效**：地址栏 `Edit` 变成 `visible=true`，但打字仍进不去 |
| 真实鼠标点标题栏抢前台 + `SetForegroundWindow` | 后台进程会被前台锁拒绝（前台仍是别的程序） |

差别只有一处：**这些试的都是"地址栏"（`Edit(id=41477)`），而不是"文件名"框**。
地址栏写进去只改显示、不回灌给 Shell 视图；文件名框是对话框真正会读的那个输入，
写它 + 回车，Shell 自己就会导航。

### 现在也给的后备

`Ctrl+Shift+C` 复制选中项所在目录到剪贴板：在框里 `Ctrl+L` → `Ctrl+V` → 回车也能跳。
只在"那套 WM_SETTEXT 被外壳拒掉 / 对话框被别的程序挡住"时还要手工救一次。
`dirOf()` 负责"目录给自身、文件给所在目录、`shell:` 虚拟项无目录"的口径。

## 9. 索引服务化（已实现）

最初索引跑在主进程里（`filepath.WalkDir`），179 万条要分钟级，且必须是普通权限。
现在多了一个可选的服务进程，形态如下：

```
BaoziIndex（服务，LocalSystem）                    baozi.exe（用户，普通权限）
  FSCTL_QUERY_USN_JOURNAL   → 位点                读 index.bin（<300ms）
  FSCTL_ENUM_USN_DATA       → 全量 $MFT 记录      再叠 index.delta
  FSCTL_READ_USN_JOURNAL    → 增量（**直接改表**）  搜索只读这份快照
        ↓ 写                                      ↑
  index.bin + index.delta + index.progress.json + index.heartbeat
        └──────────── 30s 轮询 + 面板唤出时立刻追 ────────┘
```

增量不再是"什么时候重扫"的触发器，而是直接改服务内存里那张表；
`index.bin` 只在整理（墓碑/垃圾攒够）和兜底（6 小时）时重写，
中间的变化走 `index.delta` 这个几十 KB 的小文件。详见「增量：实时改表」。

### 三条原语

| FSCTL | 作用 | 实测 |
|---|---|---|
| `0x000900f4` QUERY_USN_JOURNAL | 拿 journalID / FirstUsn / NextUsn | 立即 |
| `0x000900b3` ENUM_USN_DATA | 按 FRN 顺序吐整个卷的 $MFT 记录 | C 盘 103 万条 **3.05 秒** |
| `0x000900bb` READ_USN_JOURNAL | 从 NextUsn 起读增量 | 每条变化都拿到（**不等内核**，见下） |

`USN_RECORD_V2` 的布局（偏移即代码里用的）：`RecordLength@0` `Major@4`
`FRN@8` `ParentFRN@16` `Usn@24` `Reason@40` `FileAttributes@52` `NameLen@56`
`NameOff@58`，名字是 UTF-16。只认 MajorVersion==2（卷上极少见 3/4）。

### 三件必须知道的坑

1. **FRN 要掩码**。记录里的 FileReferenceNumber 是 64 位，高 16 位是序列号，
   低 48 位才是 $MFT 记录号。路径还原必须 `frn & 0x0000FFFFFFFFFFFF` 再比——
   否则父节点写的是「序列号|5」，而根目录常量是 5，整棵树一条都接不上
   （实测就是这里挂了，索引只剩两个盘符）。
2. **USN 记录没有大小和时间**。这两项在 `$STANDARD_INFORMATION` 属性里，要另读；
   所以由 USN 建出来的索引 `SizeKB/ModSec` 一律为 0。搜索排序只看名字/类型/频次，
   界面也不显示这两项，所以不补。
3. **隐藏文件的判定不能靠路径**。USN 只给属性位，直接用 `FILE_ATTRIBUTE_HIDDEN`；
   隐藏**目录**照常进（和 WalkDir 一致，见 §3 的扫描段）。

### 路径还原

没有目录可走，只能靠父子关系拼：

1. 枚举时丢掉 `$` 开头的 NTFS 元数据（`$Mft`/`$RmMetadata`/`$Txf`…，父链指向卷根外）；
2. 按 `parent` 排序，DFS 时用 `sort.Search` 二分定位子集——**不给每个目录建 map**
   （147 万目录各带一个 slice header 就要 35 MB）；
3. 从 `RootFRN=5` 往下走，边走边用 `[]byte` 累积路径（一个 buffer 反复截断，
   不为每条都分配一个字符串）；
4. 父节点被排除/删掉的子树整棵不可达，行为与 WalkDir 跳过 `SkipDir` 一致；
   reparse point（符号链接、目录联接）不跟随，避免环路。

### 增量：实时改表（append-only + 墓碑 + FRN 映射）

索引是 arena + 定长记录（`Parent` 存数组下标）的快照。早先因此不敢原地增删，
增量只当"什么时候值得重扫"的触发器，代价是每 5 分钟来一次 16 秒的全量重建
（1515 次 / 累计 ~7 小时 CPU，就是任务管理器里那个常年 8%）。

现在改成 Everything 那套：**只追加、不压缩**。

- 新增条目 append 到 `Nodes`/`Names` 末尾，已有下标一个都不动；
- 删除只把 `Kind` 改成 `KindDeleted`（墓碑），槽位留着。这一步不能省：NTFS 删一个
  目录时 USN 里只有目录本身一条记录，里面几千个文件没有记录，父子链必须留着，
  才能让子条目跟着失效（`Table.Dead(i)` 沿 `Parent` 上溯，深度上限 256）；
- 按 FRN 找条目靠 `index.FRNMap`（开放寻址哈希，键/值拆成两张并行数组，
  12 字节一槽、装载率 0.8，179 万条约 34MB）。**服务每次启动都会全量一次**，
  顺便把这张表建起来，所以它不落盘、`index.bin` 格式完全不变（仍 v3）。

**跨卷的坑**：NTFS 的 FRN 只在卷内唯一，两盘的卷根 FRN 都是 5。直接按 FRN 查
会把 C 盘根下的新文件认成"父在 D 盘根"。所以登记映射时把盘符编进高 16 位
（`usn.volKey`：`A`=0…`Z`=25，低 48 位是 FRN 本体，正好和 `frnMask` 对齐）。

**什么时候还是得全量**：墓碑 ≥ 20000、名字 arena 追加 ≥ 8MB、增量条数 ≥ 5000，
或者离上次全量超过 6 小时（兜底，防止增量路径漏记/错记）。正常使用下一天
一两次，摊开就是 0.0x%。

**主进程怎么知道有新条目**：服务把每条变化攒成一条 `DeltaOp` 写进 `index.delta`
（`{base, seq, ops}`，每 5 秒一批，通常几十条）。`base` 是这次全量的代次
（`index.progress.json` 的 `base`，纳秒时间戳），`seq` 是这一代的第几批。

`DeltaOp` 带的是**下标**而不是 FRN：服务端和主进程从同一份 `index.bin` 出发、
按同一顺序 append，下标天然对齐，主进程因此不用再维护一张 25MB 的 FRN 表。
改名用 `Kill` 字段承载"先失效旧条目"，枚举感觉就是一条 op。

主进程合并走 **copy-on-write**：复制当前表（35MB Nodes + 40MB Names，约 15ms）
→ `ApplyOps` → 原子替换 `a.cat`。为什么不原地改：搜索是无锁遍历 179 万条的
（几十毫秒），身后有人 append 会让它读到越界数据。`ApplyOps` 会校验 `Idx` 正好
等于下一个空位，对不上（说明载入的是旧一代）就整体放弃、等 bin 换代。

触发节奏：后台 30 秒看一次（`indexWatchInterval`），**面板唤出时再立刻追一次**
（`application.show` → `refreshIndex(true)`）。所以"用户看得见的那一刻，索引一定
是最新的"，平时每两秒就有人动文件的机器也不会被反复复制 75MB。

**数据目录必须排除**：服务每 5 秒写 `index.delta`（还有 `.tmp` 改名、heartbeat、
progress），每个动作都有 USN 记录。不排除的话它把自己的写入当成"用户动了文件"
再索引一遍、再攒增量、再写盘——自己喂自己。默认规则里已经有
`AppData\Local\Baozi`，`ruleFor()` 再按 `--data-dir` 的绝对路径补一道。

**为什么阈值这么定**（这是"常驻 8% CPU"的根因，留作历史）：一次重建 15~16 秒、
几乎占满一个核，`baozi-index.log` 里累计 1515 次、平均 16.59 秒、总计约 7.0 小时
CPU。删 4000 个文件时变化连续涌来，于是出现「重建 15 秒 → 攒 16 秒 → 再重建」的
背靠背连发（21:23:04 / 21:23:21 / 21:23:37 / 21:23:54 / 21:24:18 就是五连发）。
先按"攒 500 条 / 冷却 60 秒 / 安静 30 秒 / 上限 5 分钟"节流把平均压到 0.33% 总线，
再换成现在的实时增量，全量本身就不再是常驻成本。

**重建成本（只在整理/兜底时付）**：枚举 C 2.55s / 排序 0.15s / DFS 2.18s，
枚举 D 5.07s / 排序 0.29s / DFS 3.95s，落盘 0.9s，合计约 16 秒。已经做过三处
优化——DFS 共享一条 `pathBuf`（按层 append/截断，不再每条 `make([]byte)`）、
整盘根时跳过逐条 `underAny`、`pinyin.hasHan` 先扫 ASCII 字节再走 `unicode.Is`——
合计只省 6~8%，大头是 USN 枚举的 IO，单次成本砍不到几秒级。**这也是为什么解法
必须落在"不要那么频繁全量"上，而不是"把全量做快"。**

### 读增量：不等内核，自己 sleep

`READ_USN_JOURNAL_DATA.Timeout` 的单位是 **100 纳秒**。原代码把 `wait` 的秒数直接塞进去
（`5` 就是 500 纳秒），实测等于"无限等待"：卷上没有变化时 `DeviceIoControl` 会一直挂到
下次真有记录为止——观测到 D 卷单次挂 **4 分 47 秒**，两张卷串行读，C 卷的变化就跟着被
拖住。按 100 纳秒换算（5 秒 → `50000000`）后仍然整轮卡死：日志停更、进程 CPU 冻在
16.31 秒不动。

结论是内核这条等待路径不能用来限时。现在 `usn.Read(wait<=0)` 把 `Timeout` 和
`BytesToWaitFor` 都置 0——内核立即返回现有记录，等待交给调用方：`runIndexLoop` 每轮
读一次、然后 `time.Sleep(readPollInterval = 2s)`。超时行为完全可控，空闲时也不占 CPU。

主进程侧的交接同样是文件：服务每轮重建写 `index.bin`，每分钟写 `index.heartbeat`，
主进程 30 秒 stat 一次 mtime，变了就 `index.Load` 后 `idxReplace`（原子换指针，
正在进行的搜索不受影响）。

### 服务本身降优先级

`runIndexLoop` 一进来就 `SetPriorityClass(BELOW_NORMAL_PRIORITY_CLASS)`：重建那 15 秒
即使吃满一个核，也不会跟用户正在用的前台程序抢调度。

### 权限与开关

- 服务以 `LocalSystem` 跑（`sc create` 自动启动），只有它能打开 `\\.\C:` 读 USN；
  主进程始终是普通权限——这是服务化真正的收益。
- 安装/卸载在设置页「索引」分区，「启用快速索引（需管理员）」会 `ShellExecute
  "runas"` 拉起 `baozi-index.exe --install --data-dir <数据目录>`，弹一次 UAC。
  卸载同样走 `--uninstall`（停服务 → 等 Stopped → Delete）。
- **心跳在才用服务**。主进程不看"服务装没装"，只看 `index.heartbeat` 是否在 3 分钟内
  ——装了但停了（或服务崩了）就自动回退到自己扫，不会卡在等一个死的服务。
- 服务首次建索引期间 vs 之后内存差很大：建完必须 `debug.FreeOSMemory()`，
  否则构建期的 recs/排序/arena 会一直挂在 working set 上（实测残留 556 MB）。
  回收后常驻的就是那张活表（Nodes 35 MB + arena 40 MB + FRN 表 34 MB ≈ 110 MB，
  加上 Go runtime 的堆开销，任务管理器里显示约 185 MB）。

### 实测数字

| | WalkDir（主进程） | USN（服务） |
|---|---|---|
| 全量 174 万条 | 分钟级 | **16.8 秒**（只在首次、整理和兜底时做） |
| 运行权限 | 普通用户 | 服务（SYSTEM） |
| 常驻内存 | 主进程 280 MB | 主进程 280 MB + 服务 185 MB |
| 新建文件可见 | 下次重建 | **实时**（服务写 `index.delta`，面板唤出时追平） |
| 索引文件 | 23.4 MB | 20.0 MB（无 size/mod，压缩率更高） |

## 10. 已知边界

- 没有提权钩子：管理员权限窗口里的文件对话框暂时适配不到（见 §7/§8）。
- **Directory Opus / XYplorer / xplorer2 三种来源没有在真机上验证过**（用户自述这几样
  一个都没装）。窗口类名、协议、字段全按 Lertaro 的 collector 写；能验证的只有
  "没装时不会被误判、读不到时返回空不崩"。
- OneCommander 与 Files 不做：两者都要 UI Automation 才拿得到路径（见 §7 第 6 条）。
- tessoa 的目录来源是"问主实例"（`--print-cwd`），所以同时开两个 tessoa 时读到的是
  主实例的目录，可能不是当前前台那个窗口；每次查询还要起一个它的进程（热态 16ms），
  只在切到它和对话框弹出时各问一次，不在后台轮询。
- tessoa 停在欢迎页（`@Welcome`）时 `--print-cwd` 给的是盘根，这时跟随会跳到那个盘根。
- tessoa 若以**更高权限**运行（管理员）而 Baozi 是普通权限，`--print-cwd` 的转发可能被
  跨权限挡住：读不到就返回空、跟随器按"没读到"处理，不会乱跳也不会崩。两者权限一致时
  （都用普通权限，或都用管理员）没有这个问题。
- Directory Opus 的两个高级判法没做：本地化路径逐段还原（`LocalizedPathResolver`）、
  双栏按几何距离猜哪一栏是活动的。双栏判不出时宁可不动。
- 索引是实时更新的（服务把每条变化直接改进表，主进程面板唤出时追平 `index.delta`），
  但服务**停机期间**的变化要靠下次启动的全量才能补上；`rescanMinutes` 只在没有装服务时起作用。
- 服务常驻内存比原来高（62 MB → 约 185 MB）：现在它要一直握着整张表（35 MB Nodes +
  40 MB arena + 34 MB FRN 表）才能实时改，以前只是建完就扔、把 `index.bin` 当唯一真相。
  主进程也高了一些（对半是 copy-on-write 那一份：叠增量时复制 75 MB、改完原子换指针）。
- 路径查询（输入含 `\` 或 `/`）需要逐条还原路径，比纯名字查询慢，已限制候选上限 5000 条。
- 正则模式只匹配**文件名**，不匹配目录路径（普通模式那条"含 `\` 或 `/` 就走路径"的判定
  在正则里不成立，因为 `\.log$` 的反斜杠到处都是）；也没有"正则 + 拼音"的组合。
  正则语法错误只有结果区一行提示，不会自动帮你修模式。
- 正则模式是会话级状态（`application.regexOn`），不写 `config.json`：重启回到普通搜索。
- 缩略图只给 `.png/.jpg/.mp4/.pdf` 这类有意义的文件取；`.txt/.docx/.xlsx` 不取
  （它们的缩略图多半是白纸或空白页，还不如线框图标好认）。取不到时按线框显示，不会反复重试。
- 拖动排序只在首页「已固定」段有效，搜索态没有这个入口；拖动期间其它卡片不会让位，
  松手才整段重排（`Draggable` 的语义就是"元素跟指针走"，不做拖拽占位动画）。
- 缩略图缓存没有磁盘层，进程重启后要按系统 thumbcache 重新取（热态 1 ms/张，可接受）。
- 服务建出来的索引没有大小和时间（`SizeKB/ModSec=0`），按大小/时间排序用不了这两项。
- 非 NTFS 卷（U 盘 exFAT、网络盘）不能走 USN，配置里加这种根目录时服务会跳过它，
  主进程回退到 WalkDir。
- 互传是明文 HTTP + 一次性令牌，只适合可信局域网；大文件没有断点续传。
- 设置界面只覆盖常用项，`config.json` 里的冷门字段仍可手改；界面保存是整份覆盖，不保留手写注释。
- 内存：空索引时进程约 190MB（mygo 原生 UI + Go 运行时 + D3D），索引本身另算。

## 11. 图形化设置与热更新

第二个 mygo 窗口（920×820，`VibrancyGlass`），顶栏下按 `ui.Tabs` 分成两个页签：

| 页签 / 分区 | 控件 | 写回字段 |
|---|---|---|
| 常规·唤出 | 开关、`NumberInput`（间隔）、`TextInput`（备用组合键）、开关（全屏抑制） | `hotkey.*` |

| 常规·文件对话框跟随 | 开关（总）、两行 `Checkbox`（六种目录来源：资源管理器 / Total Commander / Directory Opus / XYplorer / xplorer2 / tessoa） | `dialogFollow` / `pathSources.*` |
| 常规·排序 | 三条 `Slider`（频次/类型/准确性）、`TextArea`（类型优先级，每行一个扩展名）、`NumberInput`（最多结果）、`Select` + `NumberInput`（图标大小：台阶/自定义倍数）、`Checkbox`（图片/视频缩略图） | `search.weights` / `typeOrder` / `maxResults` / `search.iconScale` / `search.thumbnails` |
| 常规·索引 | 两个 `TextArea`（根目录、排除目录，每行一条）、开关（隐藏）、`NumberInput`（重建间隔）、按钮（立即重建） | `index.*` |
| 自定义命令 | 左列 `ui.List`（每条两行：显示名 + 全部激活词）＋ 新建/删除；右列表单：`TextInput`×3（激活词 / 显示名 / 程序）、`TextArea`（参数，一行一个）、`TextInput`（工作目录）、`Checkbox`（执行后不收起）、「将执行：」实时预览 | `commands`（结构化，不再是竖线文本） |

命令页的编辑口径（为什么这么设计）：

- **左边只负责选，右边才改内容**。以前一整个 `TextArea` 让人手拼 `关键字 | 显示名 | 程序 | 参数`，
  分隔符错一个整行就废，而且看不出哪条是哪条；现在一条命令就是一屏表单，用户只填不拼。
- **激活词一栏写多个**（逗号/顿号/竖线/空格都算分隔），第一个是主词、其余进 `aliases`；
  这样「关机 / shut / shutdown」是一条命令的三个叫法，不再是配置里三条重复的副本。
- **参数一行一个**，不用再考虑逗号分隔（旧格式里参数里出现逗号就没法写）。
- **`{dir}` / `%1…%9` / `%*`** 在 `cmd/baozi/command.go` 展开：有占位符 → 只按模板替换；
  没有占位符 → 回落老行为（把用户多敲的词追加到末尾），所以老命令的用法一点没变。
- 新建后没填程序/激活词的空壳**不落盘**（`save()` 里跳过），否则配置里会积一堆跑不起来的条目。
- 激活词被两条命令共用的，页顶给一行 `Warning` 提示（不拦保存）。
- 编辑器里"正在改的那条"用 `cmdSync` 记住上次同步自哪条：只有选中项真的换了才把文本搬进输入框，
  否则每帧都会把用户正在敲的字覆盖掉。
- **程序栏填 `.ps1` / `.bat` 自动带解释器**（`launchSpec`）：`.ps1` → `powershell.exe
  -NoProfile -ExecutionPolicy Bypass -WindowStyle Hidden -File <脚本>`，`.bat/.cmd` → `cmd.exe /c`。
  解释器用 `systemExe` 拼 `%SystemRoot%\System32\…` 的绝对路径，不走 PATH——Baozi 可能由 Run 键
  或托盘拉起，环境变量未必和手动开的终端一致，找不到解释器的表现就是"点了没反应"。
  设置页的「将执行：」用同一个 `launchSpec`，看到的和真正跑的一致。

「目录来源」是逐项写的 `true`/`false`（不是省略），否则用户关掉某一项之后，
下次读配置会因为"缺省即开"又给它打开。运行时的取法：

- **总开关**：`syncFollow()` 按 `dialogFollow` 起/停跟随器，改完保存立刻生效，不用重启进程；
- **来源勾选**：`application.sources` 是一份 `atomic.Pointer[map[string]bool]` 快照
  （配置指针在保存时整体替换，拿它给另一个 goroutine 每 200ms 读一次会数据竞争），
  跟随器每次轮询先问一遍，关掉的来源连窗口都不读。

「保存并应用」的执行顺序（顺序不能反）：

1. 写 `config.json`；
2. 组合键变了 → 先 `Unregister` 旧键再 `Register` 新键（直接覆盖注册会静默失败）；
3. 重启钩子进程：关掉它的 stdin 让它自杀，等 2s，仍在就 `Kill`，再重新拉起——钩子读的是同一份 `config.json`，不重启就还是旧间隔/旧全屏策略；
4. 根目录变了 → 触发一次重建（`index.Build`）；
5. `syncFollow()` 刷新来源快照（必要时起停跟随器）。

界面本身的可测性：`ui.NewTester(view, w, h)` 可以不开窗口就把视图渲染出来并断言文案，
所以面板、设置、互传三个界面都有快照式测试；键盘链路（双击 Ctrl → 输入 → Enter → 落盘）则用
`BAOZI_DEBUG=1` 配合模拟按键跑端到端。

踩过的坑，都写进了代码注释：

- 窗口创建后立刻 `Show()` 会被忽略（窗口存在但永远隐藏），必须延迟到事件循环里（`time.AfterFunc` + `panel.Update`）。
- 面板 `Show()` 之后还要 `Focus()` 窗口、且回车要在输入框上 `OnSubmit` 才可靠：
  `c.Shortcut(0, ui.KeyEnter)` 在焦点位于 `TextInputBase` 时收不到回车。
- **打包必须带 `-H=windowsgui`**：Go 在 Windows 上默认给 console 子系统，双击 exe 会先弹一个黑色终端窗口。
  换成 GUI 子系统后没有控制台，日志改写到 `%LOCALAPPDATA%\Baozi\baozi.log`（`dualWriter` 同时喂 stderr 和文件，
  stderr 写失败不能让文件写入跟着失败）。钩子进程仍是 console 子系统，但主程序用
  `SysProcAttr{HideWindow: true, CreationFlags: CREATE_NO_WINDOW}` 拉起，不会弹。
- **`OnClose` 里 `PreventDefault()` 会把整个退出取消**：mygo 的 `Application.Quit` 是
  `prepareQuit` → 逐个 `w.close()`，任意窗口的 `OnClose` 拒绝关闭就让 `prepareQuit` 返回 false、退出流程整体作废。
  我们的面板/设置/互传三个窗口都是"关闭=隐藏"，于是托盘「退出」表现为**日志写了收尾、钩子也退了，但主进程还在**。
  修法：`application.quitting`（`atomic.Bool`）在真正退出前置真，三个窗口的 `OnClose` 见到它为真就直接放行。
  另有 `BAOZI_AUTOQUIT=3s` 走同一条 `a.quit()` 路径，实测退出后 `baozi`/`baozi-hook` 两个进程都不残留。
- 托盘图标在 `OnWillQuit` 里 `Destroy()`，不要放 `OnQuit`：`prepareQuit` 末尾会 `stopNativeCalls()`，
  到 `OnQuit` 时 native 通道已经关了。
- **`SetForegroundWindow` 会被 Windows 前台锁拒绝**：mygo 的 `Window.Focus()` 内部就这一个调用。
  从别的应用或任务栏切回来时，我们的进程既不是前台进程、最近也没收到输入，于是
  `SetForegroundWindow` 只让任务栏闪一下，面板显示了但打字进不去（用户报的"点任务栏再双击 Ctrl 无法输入"）。
  修法在 `cmd/baozi/foreground_windows.go`：先 `AttachThreadInput(自己线程, 前台窗口线程, TRUE)`
  把两者输入队列接到一起，再 `SetForegroundWindow` + `BringWindowToTop` + `SetFocus`，最后成对脱离。
  焦点还要等窗口真正成为前台之后交给输入框（`panel.Update(func(){ input.Focus() })`），
  否则会被刚被激活的窗口抢回去。实测四种前台：记事本、任务栏（`Shell_TrayWnd`，无标题）、
  设置窗口、面板自己——都能唤出后直接打字。
- **新建窗口必须带 `Hidden: true`**：`mygo.NewWindow` 不带这一项就是创建即显示。设置窗口漏了这一个字段，
  结果 Baozi 一启动就弹一个 880×760 的设置窗口，占着任务栏、抢着前台——这也是"点任务栏后唤出不聚焦"的
  一半原因。互传窗口一开始就写了 `Hidden: true`，所以没暴露。
- 面板可见但已经不是前台时，`toggle()` 这一下应当**取回焦点**而不是收起：否则用户按了快捷键看到面板消失，
  还得再按一次。判断用 `panel.IsFocused()`。
- **`SHQueryUserNotificationState` 的状态从出参拿，返回值是 HRESULT**：
  `HRESULT SHQueryUserNotificationState(QUERY_USER_NOTIFICATION_STATE *pquns)`。
  之前按 `proc.Call()` 的返回值当状态读，那个值是 `E_INVALIDARG` 之类的错误码，永远不可能等于 2/3/4，
  等于这条判定从来没起过作用（D3D 独占全屏只能靠窗口矩形那条兜底）。
  现在传 `&quns` 并且只在 `hr == 0` 时采信。
- **全屏判定要把「最大化」排除掉**：这是用户报"启动后不弹面板 / 有时唤不出"的另一半原因。
  `D:\work\baozi` 这一版之前，判定是「窗口矩形覆盖整个显示器 + 没有 `WS_CAPTION`」。
  任务栏设成自动隐藏时，最大化窗口的矩形就等于整个显示器（甚至越过顶边 8px）；
  Total Commander 这类能关掉系统标题栏、自绘标题的程序 `WS_CAPTION` 也是 0，
  于是它明明只是个最大化窗口，却被判成全屏应用，`show()` 直接"忽略唤出"。
  修法是先 `IsZoomed(hw)`，最大化就返回——真正的全屏（浏览器 F11、游戏无边框全屏）用的是
  `SetWindowPos` 铺满屏幕，不是最大化状态，所以不会误伤。
  实测两种前台对照：无边框覆盖全屏的窗口 → `fullscreen=true`；Total Commander 最大化 → `fullscreen=false`。

## 12. 界面：从 Pane 借了什么

参考对象是 [Pane](https://github.com/LBEILC/Pane)（Windows 11 / WinUI 3 / .NET 10 的磨砂玻璃效率工具）。
它和 Baozi 的重合面只有「唤出 + 搜索 + 结果列表」这一段，所以只借**这一段的视觉与交互**：

| Pane 的做法 | Baozi 的落点 |
|---|---|
| 首页是图标网格，「最近使用」+「已固定」两段，不是空白也不是纯列表 | `application.homeGrid()`：自己拼行（卡片 84 + 间距 6，一排几个由 `gridCols()` 按面板宽度算，默认 720 → 7），不用 `ui.Grid`——Grid 均分列宽会让卡片左右各空一截；`pinnedCount` 记录 results 里属于「已固定」的后缀长度 |
| 选中应用 `Ctrl+K` 固定到首页 | `application.togglePin()` + `history.Store.TogglePin()`；标记存 `Entry.Pinned`，`Pinned(n)` 取固定项，`trim()` 优先保留固定项 |
| 底栏左边写「会做什么」，右边写键位 | `footer()` + `keyCap()`：左边 `打开 抖音` / `已固定 · 记事本`，右边几组键帽 |
| 底栏分两层：键帽只提示，入口是按钮 | 键帽是"按了会怎样"的说明，故意画轻（`Surface` 底、无边框、10 号 `TextMuted`）；「设置」「互传」是能点的入口，用 `ui.Button`（白底 + `Border` 边 + 深色字，自带悬停/按下态），中间空开一截分组。早先两者同一套浅灰方块，看上去完全一样——这是反例，别再混回去 |
| 选中行是「浅色胶囊 + 细边」 | `row()` 用 `Accent.Alpha(0.16)` 底 + `Accent.Alpha(0.5)` 1px 边；名称 14 粗、路径 11 灰 |
| 顶栏放大镜 + 右上品牌胶囊 | 同一个 `SearchIcon` SVG + 右端「Baozi」胶囊 |
| `Ctrl+Enter` 打开所在文件夹 | `application.openFolder()`：文件开父目录，目录开自身，`shell:` 路径直接跳过 |

**搜索框右边的 `.*` 按钮**（`application.regexButton()`）：输入框与品牌胶囊之间一个
`ui.Button(c, ".*")`，开着时 `Accent.Alpha(0.28)` 底 + `Accent.Alpha(0.6)` 细边把状态标出来；
关着是 `Surface` 底、`TextMuted` 字。点一下改 `a.regexOn` 并 `refilter()`，
同时把底栏状态写成「正则模式：只精确匹配文件名」——正则和普通搜索的结果差得很远，
状态不显式说明，用户会以为搜索坏了。占位提示也跟着换成
`正则匹配文件名，如 ^2026.*\.log$`。

**为什么要自己画选中态**：mygo 的 `ui.ListState.Selected` 一旦设置，List 会用整块 accent 填充画选中行
（还压在行自己的背景上）。要让「浅色胶囊 + 细边」成立，就把 `Selected` 留空（`a.list.Selected = nil`），
选中由 `row()` 自己画，滚动仍然靠 `choose()` 里的 `ScrollIntoView`。

**首页方向键按行移动**：网格一行几个由 `gridCols()` 决定（默认面板 720 宽时是 7），
`↑`/`↓` 走 `±gridCols()`，`←`/`→` 走 `±1`；
有查询时回到单列语义（`±1`）。这一条写在 `view()` 的 `home := a.query == ""` 分支里。

**没有抄的部分**：剪贴板历史、鼠标手势、超级面板、AI、批量重命名、定时关机、`Tab` 展开的三栏文件窗口
（左分类/中结果/右预览）、`Alt+数字` 动作排。前四类与 Baozi 的定位不重合；三栏文件窗口依赖
「读第三方文件管理器当前目录」（§7）尚未实现；动作排需要一套「对任意文字的动作注册表」，
现在只有「打开 / 打开文件夹 / 固定」三个确定动作，做成键帽更诚实。

**可测性**：界面仍用 `ui.NewTester` 在内存里渲染，新增三条行为测试——
`TestHomeGridSplitsPinned`（固定项进「已固定」段）、`TestCtrlKPinsFromKeyboard`（焦点在输入框时 `Ctrl+K` 也生效）、
`TestHomeGridArrowMovesByRow`（`↓` 走一行）。
注意 Tester 环境**没有 native 消息循环**，`icon()` 里的 `Shell.FileIcon` 会一直阻塞，
所以这类测试要先 `stubAllIcons()` 把图标缓存填上纯色位图。
同理，`mygo.App.OpenAtLogin` 走的是 `onMainValue`，在 Tester 下没有主循环也会永久阻塞——
自启状态因此在 `ready()` 里问系统一次存进 `application.openAtLogin`，设置窗口和托盘只读缓存。

### 内容缩略图（`internal/win/thumb.go`）

结果行左侧对 `.png/.jpg/.mp4/.pdf` 这类文件显示的是**内容缩略图**，不是扩展名图标。
两条限制叠在一起，才逼出这个自实现的 `win.Thumbnail`：

1. **mygo 的 `Shell.FileIcon` 拿不到缩略图**。它内部固定传 `SIIGBF_ICONONLY`（源码注释原话
   "as FileIcon draws its icon, without thumbnails"），所以哪怕放开 `isProgram` 的判断，
   png 也只会拿到同一枚蓝色"图片类型"图标。要看内容必须自己调
   `IShellItemImageFactory::GetImage`，且 flags 不带 `ICONONLY`。
2. **成本差一个量级**。同一个函数按两种 flags 各取一次（`tools/thumbprobe`，28 张系统壁纸、48px）：

   | 场景 | 每张 | 首屏 10 张 | 全部 80 条 |
   |---|---|---|---|
   | 类型图标（`ICONONLY`） | 平均 2.0 ms（首张 26 ms） | ~20 ms | ~110 ms |
   | 缩略图 · 冷 | 平均 19.1 ms，最慢 80 ms | ~190 ms | ~1.5 s |
   | 缩略图 · 热（系统 thumbcache 已建） | 平均 1.1 ms | ~11 ms | ~90 ms |

   冷态同步取一屏就是几百毫秒白屏。所以**搜索完全不碰图标**（不受影响），要控的只是渲染：
   渲染时只"下单"，图回来再 `panel.Update` 重绘，第一帧仍是线框图标。

实现要点：

- `Thumbnail(path, px)` 自己组 COM 调用：`SHCreateItemFromParsingName` +
  `IShellItemImageFactory::GetImage(SIIGBF_CROPTOSQUARE|BIGGERSIZEOK)`。
  `CROPTOSQUARE` 是因为照片是 4:3 的，按比例缩进方块会拉变形；`BIGGERSIZEOK` 让 Shell
  从更大的缓存图缩下来，22 逻辑 px 才不糊。
- **COM 的 STA 与线程绑定**，而这个函数跑在后台 goroutine 里：进去先 `runtime.LockOSThread()`，
  `CoInitializeEx(COINIT_APARTMENTTHREADED)` 返回 `S_OK`/`S_FALSE` 时配对 `CoUninitialize`；
  `RPC_E_CHANGED_MODE` 说明线程已被别人初始化，不要动它。
- 返回值是 `HBITMAP`，用 `GetObjectW` 取 `DIBSECTION` 直接读 `bmBits`（32 位 BGRA + 直通 alpha），
  转 `image.NRGBA` 再 `png.Encode`，交给 `ui.DecodeBitmap`。
- **并发与去重**：`thumbSem` 容量 4（Shell 那边是 COM 走 explorer 的缩略图提供者，
  放几十个进去只会互相拖慢）；`thumbAsk` 记住请求过的 key（含失败的），避免每帧重复下单。
- **缓存**：`icons` 复用应用图标那张 map，key 是 `thumb:<px>:<path>`。
  成功的存位图、失败的也存（值为 nil），这样"系统给不出缩略图"的文件不会每次滚动都重试。
  超过 900 条就整批丢掉缩略图部分——重建一张只要 1 ms，不值得维护 LRU。
- **`ui.List` 是虚拟列表**（只有可见行调 `row()`），所以"只给可见行取图"不需要额外判断。
- 开关：`search.thumbnails`（`*bool`，nil 视为开），设置界面的「排序」区有勾选。
  关掉后一律线框图标。

### 图标多大（`search.iconScale` + `application.iconSize`）

结果行左边那个图标，尺寸过去是写死的 22 DIP。22 在 720 宽的面板里占 3%，在 125% 缩放的屏上
（这台机器就是 120 DPI = 125%）看着偏小——虽然 mygo 是 PerMonitorV2 DPI-aware、22 DIP 已经
被系统渲染成 27.5 物理像素，但**相对面板的比例没变**，用户看到的还是"小"。

**单位是倍数，不是像素**（用户的原话："为什么设定的是 px，改为倍数"）。像素说法有个绕不过去的
问题：换台显示器、改一下系统缩放，同一个数字看起来就不一样了，用户没法表达"我要比默认大一圈"。

- `config.BaseIconSize = 22` 是 100% 缩放下的基数；
- `search.iconScale` 是倍数（1 = 100%），`IconSizeAt` 算 `BaseIconSize × 系统缩放 × 倍数`。
  于是 100% 在任何屏上都是"这块屏的默认样子"，125% 屏上是 27.5、150% 是 33；
- 倍数夹在 `MinIconScale = 0.5` ~ `MaxIconScale = 3`：再小认不出图标，再大格子撑爆面板；
- 设置页给 75/100/125/150/175/200% 六个台阶，选「自定义」才露出手输框（步进 0.05）；
- **老配置兼容**：`search.iconSize`（像素）字段留着，`IconSizeAt` 的优先级是
  `iconScale → iconSize → 跟随系统缩放`。老用户升级时看到的尺寸不会跳一下；
  一旦在设置页保存过，就写 `iconScale` 并把 `iconSize` 清零（两套值同时存在没人说得清信哪个）。

`application.iconSize()` 只做一件事：`a.cfg.IconSizeAt(a.scale)`。`a.scale` 是 `ready()` 里
`mygo.Screen.Displays()` 取到的最大缩放（App 级 API，在 `ui.Tester` 下会阻塞，所以只算一次缓存）。

两个跟尺寸耦合的地方要一起改，否则会出现"设置生效了但布局错位"：

1. **首页卡片**（`cardIcon` / `cellWidth`）：图标按"结果行图标 ×2.2"放大，卡片宽度
   `cellWidth() = max(86, 图标 + 16)`——**卡片跟着图标变宽**，不再是一个固定 86。
   早先卡片写死 84 宽、图标夹在 64 以内，倍数一调大图标就会顶出底块；现在宽度、列数
   （`gridCols`）、拖动换位的步长（`pinMoveFor.colStep`）全部从 `cellWidth()` 来，三者同源，
   所以"图标放大 → 一排少放几张"是自然结果，不是裁掉。
   图标只夹下限 32：上限不夹，因为夹了就等于把"倍数"这个说法废掉一半。
   卡片高度是"上下 padding 8 + 图标 + 6 + 两行字 + 行距 6"，`pinMoveFor` 用
   `icon + 49` 跟着算，不然拖动重排会跳格。
2. **结果行行高**：行高是"图标"和"名字 + 提示两行字"取高的那个，而名字那列本身就有 38 左右。
   所以 22 → 27.5 时**行高不变、显示行数也不变，只是图标变大**；只有图标超过 38 才会把行撑开。
   这不是 bug，是想要的：默认换大一号不该让一屏少显示两行。`TestIconSizeSettingGrowsRows`
   把这两段都钉住了（44 时行高几乎不动，60 时才明显涨），`TestIconScaleWidensCards`
   钉住"倍数放大 → 卡片变宽 → 一排变少"这条连锁。

**卡片里图标只有程序是彩色的**：`KindApp` 与 `.exe/.lnk/.msi/...` 走 `appIcon`（系统彩色图标），
目录/文档/命令仍是 `icon.go` 的单色线框（那是 Pane 式的统一语言，也是当初换掉系统文件图标的
理由：白纸片在深色玻璃底上刺眼、同排花花绿绿不成体系）。所以首页如果是纯应用/游戏，
看到的就是一整排"大彩色图标"；混进文档时线框会显得轻一些，这是有意的区分。

实拍：`BAOZI_SCALE=1.25 BAOZI_PREVIEW=… go test -run TestIconPreviewShot`，这张图就是
125% 屏上结果行的默认样子；`BAOZI_SCALE=1` 再跑一次即得 100% 的对照。
卡片网格另有 `TestCardIconPreview`：它用 `win.Thumbnail` 取真实 `.exe` 图标塞进缓存再渲染
（`appIcon` 现在先查缓存再问 Shell，所以离线也能命中），跑出来的就是真机上首页的样子。
`BAOZI_PANEL_W` / `BAOZI_PANEL_H` 可以在预览里换面板尺寸，顺便验证列数跟着变。

### 图标为什么曾经是"白框里一个小图标"（`win.TrimIcon`）

用户报过：首页卡片上「遗忘之海」显示成一个方形浅灰细框 + 白底 + 中间一个小 logo，
而游戏启动器里它是满格的彩色图标。`win.Thumbnail` 拿这个 exe 的图标一验，**位图本身就长这样**。

拆开看那张 200×200 的 PNG：

- 四角 `alpha` 只有 58~64（约 23%~25%），是 Windows tile 图标自带的一圈**淡阴影**；
- 中间 logo 区 `alpha` 是 255；
- 两者之间是 `(0,0,0,0)` 的纯透明。

旧的裁边只按 `alpha > 1.6%` 找内容边界。那圈 25% 的淡阴影跨过了门槛，于是 bbox 等于整张图，
**一个像素都裁不掉**——图形只占中间一小块，显示出来就是"白框小图标"。`flatCut` 单独也救不了：
阴影不是纯色平板，每行颜色都在渐变。

修法是两级门槛：

1. `alphaCut` 先按 `solidAlpha = 30%`（`0x5000`）找"实心内容"的外接框——这刚好跨过那圈淡阴影；
2. 实心区找不到（半透明描边图标）才退回老的 1.6% 门槛；
3. 之后再叠 `flatCut`：从四边往里裁"整行/整列颜色一致"的不透明平板边（真正的白底/灰 tile 边框）。
   判据是**行内自洽**而不是"等于角落色"，这样"白底 + 浅灰边框"两层能逐层剥掉；
   两条保护：整张几乎一个颜色（纯色方块图标）就原样返回，裁到剩不到 15% 也当作裁过头退回。

实测 `PCLauncher.exe`：裁前 8149 字节（白框小图）→ 裁后 6549 字节，图形铺满整格。
单测：`TestTrimIconCentersContent`（透明边 + 居中）、`TestTrimIconTrimsOpaqueTile`（128 的白底+灰框被裁到 logo 大小）、
`TestTrimIconKeepsSolidSquare`（纯色方块不许被裁小）、`TestTrimIconRejectsBlank`。

配套还有一条：历史里出现过 `shell:AppsFolder\D:\Games\x\PCLauncher.exe` 这种**畸形 Path**
（前缀后面跟的是真路径，不是 AUMID）。`iconPath()` 会把这类剥成普通路径再取图标，
其余 `shell:AppsFolder\<AUMID>` 原样交给 Shell——按扩展名当普通文件取只会拿到一个无关的通用图标。

### 面板多大（`panel.width` / `panel.height`）

面板尺寸过去是 `main.go` 里的两个常量（720×480），`WindowOptions.DisableResize: true`。
4K 屏上嫌小、笔记本竖屏上嫌宽，而且首页一排固定 7 张卡，宽窄都不跟着变。

- `config.Panel{Width, Height}`（json `panel`），0 或缺省 = 默认 720×480；
- `Config.PanelSize()` 夹到 420~2400 / 260~1600，越界的数存盘前就夹好，配置文件里不会躺一个永远不生效的值；
- **没有设置项**：用户直接拖面板的四条边（`TitleBarHidden` + `HideWindowButtons` 才有 Windows 的缩放边缘，
  见 `ready()`）。拖动中 `panel.Size()` 实时变，`gridCols()` 直接问它，列数跟着重排；
- 停手 0.8 秒后 `panelResized()` 攒下的尺寸由 `savePanelSize()` 写进 `config.json`（尺寸没变就不写，
  否则每次唤出的 `SetSize`/`SetPosition` 都会触发一次无谓的落盘）；
- 不用 mygo 的 `StateKey`：它只在窗口真正 close 时写盘，而 Baozi 的"关闭"只是 `PreventDefault` 隐藏；
- **列数不再是常量**：`application.gridCols()` = `(宽 - 2*cardGutter + cardGap) / (cardWidth + cardGap)`，
  720 → 7 列、1200 → 13 列、480 → 5 列。`homeGrid()` 的排布、`pinMoveFor()` 的行列换算、
  `↑`/`↓` 的步长全部走它，三者必须同源，否则"一排看得到 13 个、方向键却跳 7 格"。
- `panelCols` 常量只留作"默认宽度下的列数"，给测试和默认布局当基准。

设置页右侧实时算给用户看："一排 7 列、约 10 行；物理 900×600"（物理值乘系统缩放）。
`TestGridColsFollowsPanelWidth`、`TestPanelSize` 钉住换算与夹取。

### 已固定区的拖动排序

首页「已固定」的卡片可以拖动重排，松手即保存。三个不能省的细节：

- **不能在卡片渲染途中改 `results`**：那一帧还在遍历它，改了就越界。
  松手时只记下 `pendingPinMove{from,to}`，下一帧 `view()` 开头的 `applyPending()` 统一结算
  （和右键菜单的"固定/删除"走同一条通道）。
- **`Draggable` 的 offset 指针必须跨帧稳定**：`pinDragPts map[int]*ui.Point` 按卡片缓存指针，
  每帧新建一个 `Point` 会把拖动中的卡片拽回原位。结算前要 `*pt = ui.Point{}` 归零，
  否则卡片会粘在拖到的位置；重排后 `pinDragPts` 整表清空，避免旧下标对上新的卡片。
- **位移怎么换算成落点**：一列 = 卡片 84 + 间距 6 = `cardWidth + cardGap`（90 DIP），
  一行 = `cardIcon() + 49`（图标 + 上下 padding + 间距 + 两行字 + 行距，默认约 89）。
  `round(dx/90)` 加 `round(dy/行高) * gridCols()` 就是"挪了几格"，再夹到 `[0, pinnedCount-1]`。
  这几个数字必须和 `card()`、`homeGrid()` 保持一致，改布局时一起改。

`Draggable` 的语义是"元素跟着指针走、松手才停"，所以拖动期间其它卡片不会让位——
出现的是"被拖的卡片盖住目标位"的观感，松手后整段重排。测试 `TestPinDragReorders` 用
`ui.Tester` 的 `Press/Move/Release` 真跑一遍：往右拖一格，「甲甲」从第一位变成第二位，
且 `hist.Pinned` 的顺序在重开后依然是新顺序。

## 13. 启动方式与图标资源

**开机自启**用 mygo 的 `App.SetOpenAtLogin`，Windows 上就是写用户的 Run 键：
`HKCU\Software\Microsoft\Windows\CurrentVersion\Run`，值名 `Baozi`，
内容 `"<绝对路径>" --mygo-opened-at-login`。选 Run 键而不是计划任务，是因为 Baozi 不需要提权；
需要管理员权限的程序才应该用「最高可用权限」的登录任务。

`--mygo-opened-at-login` 是"被系统拉起来的"标记，mygo 在 `init` 里读 `os.Args` 后会把它剥掉，
主程序只通过 `App.WasOpenedAtLogin()` 看到布尔值。`ready()` 里据此决定首帧要不要 `show()`：
自启就只建托盘 + 钩子进程待在后台。状态不存 `config.json`——系统的 Run 键才是唯一事实，
用户可能在任务管理器里禁用，存一份副本必然两边打架。

**图标**：同一个包子形象（暖橙圆角底 + 奶油白包子 + 顶部三褶），三处复用：

| 落点 | 载体 | 机制 |
|---|---|---|
| 托盘/通知栏 | `cmd/baozi/assets/tray.png`（32×32） | `go:embed` 进 `trayPNG`，`TrayOptions.Icon` 要的就是 PNG |
| exe 图标 | `cmd/baozi/assets/baozi.ico`（7 个尺寸） | 由 `rsrc` 编成 `rsrc_windows_amd64.syso`（COFF 资源），`go build` 自动链接 |
| 窗口图标 | 无单独设置 | mygo 的 `WindowOptions` 没有 Icon 字段，窗口图标直接取 exe 的资源，所以上一条改完窗口/任务栏/Alt+Tab 一起变 |

为什么要 syso 而不能只放旁边一个 ico：PE 的图标在 `.rsrc` 节里，Windows 读的是那一段，
旁边放文件只会被忽略。`rsrc` 本地没有二进制时用 `go run github.com/akavel/rsrc@v0.10.2`。

为什么要程序化画：托盘 16px、任务栏 32px、资源管理器大图标 256px，同一张位图缩到 16px 会糊；
`tools/icongen` 对每个尺寸按解析式重画（4×4 超采样、边缘 1px 抗锯齿），小尺寸自动省掉褶子。

## 14. 卸载

入口在托盘「卸载：清除自启与数据…」，范围是"摘自启 + 删数据目录"，**exe 保留**（`cmd/baozi/uninstall.go`）。
先走 `mygo.Dialog.Message`（`MessageWarning`、按钮 `取消 / 卸载`、`DefaultButton: 0`、
`CancelButton: 0`、`DestructiveButtons: [1]`、`Detail` 里逐条列出会删什么），`res.Button != 1` 就当取消。
调用方用 `go a.uninstall()`：模态对话框不该占着主线程的消息循环。

确认后的顺序（顺序不能反）：

```
SetOpenAtLogin(false)  → history.Close() → share.stop() → uninstalling=true → quitting=true
→ shutdown()（关钩子、取消扫描、跳过历史落盘）→ 等扫描 goroutine 结束
→ closeLog()（放开日志句柄）→ os.RemoveAll(dataDir) → App.Quit()
```

几个必须这样排的理由：

- **`history.Close()` 和 `uninstalling` 标记都是为了"删完又长回来"**。历史 `Touch` 后有 400ms
  防抖落盘，最后一笔可能在目录删掉之后才触发；`Save` 里有 `os.MkdirAll(filepath.Dir(path))`，
  于是 `history.json` 带着目录一起被重建。`Close()` 停定时器并让之后的 `Save` 变空操作；
  `shutdown()` 里也加了 `!a.uninstalling.Load()` 才 `Flush`。
- **`closeLog()` 必须在 `RemoveAll` 之前**：Windows 不允许删除正在写入的文件，句柄不放开会删一半。
  `logFile` 存的是 `setupLog()` 打开的那个句柄，关完把 `log.SetOutput` 换成 stderr（GUI 下等于丢弃，反正马上退）。
- **要等后台扫描结束**：`shutdown()` 的 `scanOff()` 只是取消 context，扫描 goroutine 可能正处在落盘阶段。
  现在 `rescan()` 有 `scanWG.Add/Done`，卸载用 `waitScan` 等它（上限 5s）。
- **顺带修正的真 bug**：`rescan()` 原来在 `ctx.Err() != nil` 时仍会 `index.Save` 已扫到的部分结果。
  这不只影响卸载——用户点「重新建立索引」又立刻退出，也会留下半份 `index.gob`，
  下次启动当完整索引加载，搜索结果就缺东西。现在取消即返回，不落盘。
- `config.Dir()` 与 `history.Save()` 都有 `MkdirAll`，所以卸载流程里 `RemoveAll` 之后不能再有任何
  触碰它们的调用。`quitting` 置真让三个窗口的 `OnClose` 放行，`App.Quit` 才能正常关窗退出。

验证方式（隔离，不动真实数据）：`BAOZI_DATA_DIR=<临时目录> BAOZI_HEADLESS=1 BAOZI_UNINSTALL=1`，
实测结果是临时目录整个消失、`Run` 里没有 `Baozi`、`baozi`/`baozi-hook` 两个进程都退出。
真实数据目录（含 179 万条索引）未被这一路测试碰到。

## 15. 内存：从 526 MB 压到 265 MB

用户报"占用了 500 多 M，太多了"。先把事实量出来，再谈怎么改。

### 诊断

`tools/memprobe`（`//go:build ignore`）加载真实索引后逐段打印：`Node` 结构 56 字节、
1,791,246 条、结构本体 95.7 MB、名字内容 40.6 MB（平均 23.8 B/条）、
`Py` 只有 9,228 条（0.1 MB，却给每条留了 16 字节字符串头）、
模拟 `catalog.lowers` 再加 39 MB。

pprof（`BAOZI_PPROF=1`）`-inuse_space` 抓到 `reflect.growslice` 86 MB、
`internal/saferio.ReadData` 74 MB、`encoding/gob.decString` 25 MB，
`index.Load` cum 185 MB（占全部活数据 95.8%）——索引几乎就是全部，gob 解码另产 160 MB 垃圾。

`BAOZI_MEMSTAT=8s` 的曲线（`heap=456MB inuse=459 sys=496 objs=2092272 gc=8`）说明 GC 根本没被触发：
活数据大 + 默认 GOGC=100。

### 第一轮：GC 策略 + 归还空闲页

- `main()` 里 `debug.SetGCPercent(40)`：让 GC 更早介入，代价是 CPU；
- 新增 `releaseMemory()`：命中快照不扫描时、扫描结束后各调一次，
  延迟 2s 再 `debug.FreeOSMemory()`（别让 STW 卡首帧）。

结果：`Private 526 → 369 MB`、`WS 490 → 326 MB`、`heap 457 → 209 MB`。但索引本身仍是 185 MB。

### 第二轮：改存储格式（v3）

三件事一起做，因为它们的收益互相独立：

1. **`Node` 56 → 20 字节**：去掉 `Name string`（改偏移+长度）、去掉 `Py string`
   （改 `map[int32]uint64` 打包偏移，只有 9 千条）、`Size int64` → `int32 KiB`、
   `Mod int64` → `int32`（相对 2020-01-01，够用到 2088 年）。
   `179 万 × 36 字节 = 省 64 MB`。
2. **名字进 arena**：名字不再每条一个字符串对象，而是 `Names []byte` 里连续的一段。
   省的其实不是内容（40.6 MB 不变），是 179 万个小对象的头和按 size class 取整的浪费。
3. **去掉 `catalog.lowers`**：预存小写表 39 MB。改成在 arena 字节上直接做大小写不敏感比较
   （`containsFold`：ASCII 折大小写，汉字字节 >0x7f 原样比）。

顺带把 `gob` 换成定长记录 + arena + gzip：加载变成"一次解压 + `unsafe.Slice`"，
零解码、零拷贝，也不再产生 growslice / ReadData 那两笔峰值。

引擎编号 `storeVersion = 3`，文件名从 `index.gob` 改成 `index.bin`（旧文件不会自动删，
它已经不被读取，可以手工清掉）。

### 结果（真机实测，1,791,388 条）

| 指标 | 原始 | 第一轮止血 | 第二轮改格式 |
|---|---|---|---|
| Private | 526 MB | 369 MB | **269 MB** |
| WorkingSet | 490 MB | 327 MB | **223 MB** |
| Go heap | 457 MB | 209 MB | **97 MB** |
| 堆对象数 | 2,092,272 | 2,092,272 | **48,7xx** |
| 索引文件 | 23.47 MB | 23.47 MB | 23.42 MB |

对象数降 97.7% 比字节数更关键：GC 每次要扫的活对象少了一个数量级，
`gc=65` 之后曲线完全躺平（`heap=97 inuse=111 sys=264`，几十轮采样不变）。

### 踩过的坑

- **py 记录宽度写错**：`Save` 里借 4 字节的 `buf` 写 2 字节长度，实际写了 12 字节，
  而 `pyRecLen=10`，整段错位——读回来的拼音串是一大截乱码（`PyOf` 返回跨几千字节的字符串）。
  症状是拼音搜索全部失效。定长格式必须让"写出去的字节数"与"声明的记录宽度"严格相等。
- **`config.normalize()` 会把空 `roots` 复原成 `SystemRoots()`**，所以拿不到"零索引基线"，
  想量纯 UI 只能改代码或换数据目录。
- `unsafe.Slice` 要求段起点按 4 字节对齐，所以 `nodesOff` 要 `align8`；
  对齐不满足时代码回退到逐条解析，不会崩。
- 新格式读回来之后 `Nodes`/`Names` 都指向那块 `raw`，`Table` 必须一起持有 `raw`，
  否则切片会悬垂——`raw` 字段就是为这个留的。
