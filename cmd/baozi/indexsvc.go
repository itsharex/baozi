package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc/mgr"

	"baozi/internal/index"
)

// 索引服务（baozi-index.exe）以 SYSTEM 跑，只有它能打开卷句柄读 USN Journal。
// 它每建完一轮索引就写 index.bin，并且每分钟写一次心跳。
// 主进程这边只做三件事：看心跳决定"要不要自己扫"、盯 index.bin 变化、换了就重载。

const (
	// 心跳多久算失效。服务每分钟写一次，3 分钟给了两轮容错（比如它在重建索引时卡住）。
	serviceHeartbeatTTL = 3 * time.Minute
	// 主进程后台追增量的间隔。服务那边 5 秒攒一批写盘。
	// 这里给 30 秒而不是更短，是因为"叠增量"要先复制一份 75MB 的快照
	// （copy-on-write，见 Table.Clone）——这台机器每两秒就有人动文件，
	// 10 秒一轮会一直复制，内存峰值翻倍、GC 也跟着忙。
	// 真正的及时性靠面板唤出时那一次 refresh（见 application.show），
	// 也就是"用户看得见的那一刻，索引一定是最新的"。
	indexWatchInterval = 30 * time.Second
)

func (a *application) heartbeatFile() string { return filepath.Join(a.dataDir, "index.heartbeat") }
func (a *application) deltaFile() string     { return filepath.Join(a.dataDir, "index.delta") }

// serviceAlive 判断索引服务是否在跑（心跳文件够新）。
// 注意不能拿"服务是否安装"来判断：装了但停了（比如用户手动停掉）也得回退到自己扫。
func (a *application) serviceAlive() bool {
	info, err := os.Stat(a.heartbeatFile())
	if err != nil {
		return false
	}
	return time.Since(info.ModTime()) < serviceHeartbeatTTL
}

// watchIndexFile 定时把索引追到服务的最新状态。
//
// 两层：index.bin 换代就整份重载（服务每次全量重建都换一代），
// 没换代就把 index.delta 里新增的那些指令叠上去（copy-on-write，约 15ms）。
// 没变化时连复制都不做，只是每次 stat 两个文件。
func (a *application) watchIndexFile() {
	go func() {
		if info, err := os.Stat(a.indexFile()); err == nil {
			a.idxMtime = info.ModTime()
		}
		ticker := time.NewTicker(indexWatchInterval)
		defer ticker.Stop()
		for range ticker.C {
			if a.quitting.Load() || a.uninstalling.Load() {
				return
			}
			a.refreshIndex(false)
		}
	}()
}

// refreshIndex 把内存快照追平服务的状态。quiet=true 时不写状态栏/log（面板唤出时用）。
func (a *application) refreshIndex(quiet bool) {
	// 服务没在跑的时候不追：这时候 index.bin 是我自己 rescan 写的，
	// 再加载一遍纯属浪费（刚扫完的表已经在内存里）。
	if !a.serviceAlive() {
		return
	}
	a.idxMu.Lock()
	defer a.idxMu.Unlock()

	// 1) 快照换代：整份重载（19MB，约 90ms）
	if info, err := os.Stat(a.indexFile()); err == nil && !info.ModTime().Equal(a.idxMtime) {
		tab, _, err := index.Load(a.indexFile())
		if err != nil {
			log.Printf("重载索引: %v", err)
			return
		}
		if tab.Len() == 0 {
			return
		}
		a.idxMtime = info.ModTime()
		a.idxBase = readStateBase(a.dataDir)
		a.idxSeq = 0
		a.idxOps = 0
		a.idxReplace(tab)
		if !quiet {
			a.setStatus(fmt.Sprintf("%d 项 · 索引服务已更新", tab.Len()))
			log.Printf("索引: 服务已更新快照，重载 %d 项", tab.Len())
		}
		// 重载会临时堆出解压缓冲，还回去
		releaseMemory()
	}

	// 2) 增量：把这次新增的指令叠上去。
	//
	// 注意 delta.Ops 是**这一代累积的全部** op（服务每次都整份重写），
	// Seq 只是"第几批"，两者不是一回事：不能拿 Seq 当 Ops 的下标用，
	// 否则上一批 20 条、这一批 3 条时，下标会从第 20 条开始取、把应用过的
	// 又叠一遍（ApplyOps 的下标校验会整批拒绝，然后索引永远追不上）。
	d, err := index.ReadDelta(a.deltaFile())
	if err != nil || d == nil {
		return
	}
	if d.Base != a.idxBase {
		// 不是同一代（bin 还没换到这一代），等下一次重载
		return
	}
	from := a.idxOps
	if d.Seq < a.idxSeq || from > len(d.Ops) {
		// 服务那边重置过（理论上 base 会一起变，这里兜个底）
		from, a.idxOps = 0, 0
	}
	if from >= len(d.Ops) {
		a.idxSeq = d.Seq
		return
	}
	cur := a.cat.Load()
	if cur == nil {
		return
	}
	ops := d.Ops[from:]
	next := cur.Clone()
	if _, ok := next.ApplyOps(ops); !ok {
		// 下标对不上：本地这份已经和服务错开了，只能等 bin 换代重来
		a.idxOps = 0
		a.idxSeq = 0
		return
	}
	a.idxReplace(next)
	a.idxOps = len(d.Ops)
	a.idxSeq = d.Seq
	if !quiet {
		a.setStatus(fmt.Sprintf("%d 项 · 已同步 %d 条变化", next.Len(), len(ops)))
		log.Printf("索引: 已同步 %d 条变化，%d 项（累计 %d 条 / %d 批）", len(ops), next.Len(), a.idxOps, d.Seq)
	}
}

// readStateBase 读服务写的这一代标识（index.progress.json 的 base）。
// 读不到就返回 0——那样 delta 会因为 Base 对不上而被跳过，不会错位。
func readStateBase(dataDir string) uint64 {
	var st struct {
		Base uint64 `json:"base"`
	}
	data, err := os.ReadFile(filepath.Join(dataDir, "index.progress.json"))
	if err != nil {
		return 0
	}
	_ = json.Unmarshal(data, &st)
	return st.Base
}

// indexServiceExe 找同目录下的 baozi-index.exe。
func indexServiceExe() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(exe)
	p := filepath.Join(dir, "baozi-index.exe")
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("没找到 %s", p)
	}
	return p, nil
}

// indexServiceInstalled 查服务是否注册过（普通用户也能查，不需要提权）。
func indexServiceInstalled() bool {
	m, err := mgr.Connect()
	if err != nil {
		return false
	}
	defer m.Disconnect()
	s, err := m.OpenService("BaoziIndex")
	if err != nil {
		return false
	}
	s.Close()
	return true
}

// runIndexService 以管理员身份拉起 baozi-index.exe 做安装/卸载。
// 这一步必然弹 UAC——装服务就是这个代价，所以只在用户点了按钮时才发生。
func (a *application) runIndexService(flag string) error {
	p, err := indexServiceExe()
	if err != nil {
		return err
	}
	args := fmt.Sprintf(`%s --data-dir "%s"`, flag, a.dataDir)
	verb := windows.StringToUTF16Ptr("runas")
	file := windows.StringToUTF16Ptr(p)
	params := windows.StringToUTF16Ptr(args)
	if err := windows.ShellExecute(0, verb, file, params, nil, 0); err != nil {
		return fmt.Errorf("提权启动失败: %w", err)
	}
	return nil
}

// stopIndexService 是卸载专用：要同步等它真的消失，不然接下来删数据目录时
// 服务会把它再建回来（然后留一个空索引在跑）。
// 自己已经是管理员就直接跑，否则提权跑并轮询等待。
func (a *application) stopIndexService() error {
	p, err := indexServiceExe()
	if err != nil {
		return err
	}
	wait := func() bool {
		for i := 0; i < 40; i++ {
			if !indexServiceInstalled() {
				return true
			}
			time.Sleep(500 * time.Millisecond)
		}
		return false
	}
	// 先直接试（进程本身是管理员时会成功，不弹 UAC）
	cmd := exec.Command(p, "--uninstall")
	cmd.SysProcAttr = &windows.SysProcAttr{HideWindow: true}
	if err := cmd.Run(); err == nil && wait() {
		return nil
	}
	// 不行就提权
	verb := windows.StringToUTF16Ptr("runas")
	file := windows.StringToUTF16Ptr(p)
	params := windows.StringToUTF16Ptr("--uninstall")
	if err := windows.ShellExecute(0, verb, file, params, nil, 0); err != nil {
		return fmt.Errorf("提权卸载服务失败: %w", err)
	}
	if !wait() {
		return fmt.Errorf("服务仍在（可能用户在 UAC 里点了取消）")
	}
	return nil
}
