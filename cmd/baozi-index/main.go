// baozi-index 是 Baozi 的索引服务：直接读 NTFS 的 USN Journal 维护索引，
// 不遍历目录。它比主程序多出来的能力只有一个——**能在提权下打开卷句柄**，
// 所以主程序自己不需要管理员权限。
//
// 用法（平时由托盘/设置页调起，不手敲）：
//
//	baozi-index.exe --install --data-dir "<用户数据目录>"   装成 Windows 服务并立即启动
//	baozi-index.exe --uninstall                             停止并删除服务
//	baozi-index.exe --once --data-dir "<dir>"               跑一轮建索引就退出（调试）
//	baozi-index.exe                                          控制台模式（前台跑，调试用）
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"baozi/internal/config"
	"baozi/internal/index"
	"baozi/internal/usn"
)

const serviceName = "BaoziIndex"

const (
	// rebuildBatch：攒够这么多条变化才值得整表重扫。一次重扫约 15 秒（两盘 174 万条），
	// 单条摊 80ms；阈值给低了就会变成"天天在重建"，CPU 常年几个点。
	rebuildBatch = 500
	// rebuildCooldown：两次重建之间的最小间隔。删/装大批文件时变化会连续涌来，
	// 冷却让它们攒成一轮，避免背靠背满载。
	rebuildCooldown = 60 * time.Second
	// rebuildIdle：有变化但不多时，安静这么久就扫一次。
	// 删掉一批文件往往只产生几百条记录，等不到条数阈值，靠这个尽快反映。
	rebuildIdle = 30 * time.Second
	// rebuildMaxAge：这台机器每几秒就有零星文件变化，"安静 30 秒"经常满足不了，
	// 所以补一条上限：不管攒了多少，离上次重建超过这么久就扫一次。
	// 它同时是重建频率的闸门——一次 15 秒摊到 5 分钟，平均占用从 8% 降到 0.3% 上下。
	rebuildMaxAge = 5 * time.Minute
	// readPollInterval：多久读一次增量。文件变化最多延迟这么久被看见，
	// 空闲时这里只花一次系统调用，不占 CPU。
	readPollInterval = 2 * time.Second
)

func main() {
	var (
		dataDir   string
		install   bool
		uninstall bool
		once      bool
		service   bool
	)
	flag.StringVar(&dataDir, "data-dir", "", "数据目录（默认 %LOCALAPPDATA%\\Baozi）")
	flag.BoolVar(&install, "install", false, "安装并启动服务（需要管理员）")
	flag.BoolVar(&uninstall, "uninstall", false, "停止并删除服务（需要管理员）")
	flag.BoolVar(&once, "once", false, "只建一轮索引就退出")
	flag.BoolVar(&service, "service", false, "以 Windows 服务方式运行（由 SCM 传入）")
	flag.Parse()

	if dataDir != "" {
		os.Setenv("BAOZI_DATA_DIR", dataDir)
	}

	dir, err := config.Dir()
	if err != nil {
		fail("数据目录不可用: %v", err)
	}
	setupLog(dir)

	switch {
	case install:
		cmdInstall(dir)
	case uninstall:
		cmdUninstall()
	case once:
		runOnce()
	case service:
		runService()
	default:
		if running, _ := svc.IsWindowsService(); running {
			runService()
			return
		}
		// 控制台模式：后台跑，Ctrl+C 结束，方便观察日志
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		log.Printf("索引服务（控制台模式）启动，数据目录 %s", dir)
		runIndexLoop(ctx)
	}
}

func fail(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	// 服务模式下 stderr 没人看，尽量写日志
	log.Printf("失败: %s", msg)
	if running, _ := svc.IsWindowsService(); !running {
		fmt.Fprintln(os.Stderr, msg)
	}
	os.Exit(1)
}

func setupLog(dir string) {
	path := filepath.Join(dir, "baozi-index.log")
	if st, err := os.Stat(path); err == nil && st.Size() > 8<<20 {
		_ = os.Rename(path, path+".old")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	log.SetOutput(f)
}

// ---------------------------------------------------------------- 服务安装

func cmdInstall(dataDir string) {
	exe, err := os.Executable()
	if err != nil {
		fail("取自身路径: %v", err)
	}
	exe, _ = filepath.Abs(exe)

	m, err := mgr.Connect()
	if err != nil {
		fail("连服务管理器（需要管理员）: %v", err)
	}
	defer m.Disconnect()

	cfg := mgr.Config{
		DisplayName: "Baozi 索引服务",
		Description: "直接读取 NTFS USN Journal 维护 Baozi 的文件索引，主程序因此不需要管理员权限。",
		StartType:   mgr.StartAutomatic,
	}

	// 已经装过就更新 binPath（升级后 exe 换了位置也能自愈）。
	if s, err := m.OpenService(serviceName); err == nil {
		if err := s.UpdateConfig(cfg); err != nil {
			s.Close()
			fail("更新服务配置: %v", err)
		}
		s.Close()
	}

	args := []string{"--service", "--data-dir", dataDir}
	s, err := m.CreateService(serviceName, exe, cfg, args...)
	if err != nil {
		// 已存在就退而求其次：按名字打开再启动
		s, err = m.OpenService(serviceName)
		if err != nil {
			fail("创建服务: %v", err)
		}
	}
	defer s.Close()

	if err := s.Start(); err != nil && err != windows.ERROR_SERVICE_ALREADY_RUNNING {
		fail("启动服务: %v", err)
	}
	log.Printf("服务已安装并启动: %s → %s", serviceName, exe)
	fmt.Println("Baozi 索引服务已安装并启动")
}

func cmdUninstall() {
	m, err := mgr.Connect()
	if err != nil {
		fail("连服务管理器（需要管理员）: %v", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		fmt.Println("服务不存在，无需卸载")
		return
	}
	defer s.Close()
	_, err = s.Control(svc.Stop)
	if err != nil && err != windows.ERROR_SERVICE_NOT_ACTIVE {
		log.Printf("停止服务: %v", err)
	}
	// 等服务真的停下来再删，否则 Delete 会被拒
	for i := 0; i < 20; i++ {
		st, err := s.Query()
		if err != nil || st.State == svc.Stopped {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err := s.Delete(); err != nil {
		fail("删除服务: %v", err)
	}
	fmt.Println("Baozi 索引服务已卸载")
}

// ---------------------------------------------------------------- 服务主体

type serviceHandler struct{}

func (serviceHandler) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown
	changes <- svc.Status{State: svc.StartPending}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runIndexLoop(ctx)
	}()

	changes <- svc.Status{State: svc.Running, Accepts: accepted}
	for c := range r {
		switch c.Cmd {
		case svc.Interrogate:
			changes <- c.CurrentStatus
		case svc.Stop, svc.Shutdown:
			changes <- svc.Status{State: svc.StopPending}
			cancel()
			<-done
			return false, 0
		}
	}
	cancel()
	return false, 0
}

func runService() {
	if err := svc.Run(serviceName, serviceHandler{}); err != nil {
		fail("服务运行: %v", err)
	}
}

func runOnce() {
	cfg, _, err := config.Load()
	if err != nil {
		fail("读配置: %v", err)
	}
	vols, err := openVolumes(cfg.Index.Roots)
	if err != nil {
		fail("打开卷: %v", err)
	}
	defer closeVolumes(vols)
	if err := rebuild(vols, cfg, true); err != nil {
		fail("建索引: %v", err)
	}
}

// runIndexLoop 是服务的核心：首轮全量建索引，之后靠 USN 增量判断"什么时候该重建"。
//
// 为什么增量只当触发器、不做逐条改表：索引是 arena + 定长记录的快照，
// 逐条增删会产生碎片和 tombstone；而全量重建只要 6 秒（两盘），
// 比维护"可变索引"的复杂度低得多，也不会出现两份数据对不上。
func runIndexLoop(ctx context.Context) {
	// 后台活：整表重建会连着吃十几秒 CPU。降到 Below Normal 让前台程序优先，
	// 重建慢个两三秒无所谓，但不能在用户敲字/编译的时候抢核。
	_ = windows.SetPriorityClass(windows.CurrentProcess(), windows.BELOW_NORMAL_PRIORITY_CLASS)

	cfg, _, err := config.Load()
	if err != nil {
		log.Printf("读配置失败: %v", err)
		return
	}
	vols, err := openVolumes(cfg.Index.Roots)
	if err != nil {
		log.Printf("打开卷失败: %v", err)
		return
	}
	// 闭包形式，roots 改了重开卷时能关掉最新那批句柄
	defer func() { closeVolumes(vols) }()
	log.Printf("索引服务就绪，卷 %v", letters(vols))

	// 首轮
	if err := rebuild(vols, cfg, true); err != nil {
		log.Printf("首轮建索引失败: %v", err)
	}

	dataDir, _ := config.Dir()
	state := loadState(dataDir)
	lastChange := time.Now()
	lastRebuild := time.Now()
	lastCfg := time.Now()
	pending := 0

	for {
		if ctx.Err() != nil {
			log.Printf("索引服务收到停止信号，退出")
			return
		}
		// 主程序点了"立即重建索引"会放这个信标（服务没有 IPC 端口，用文件最省事）
		if _, err := os.Stat(filepath.Join(dataDir, "index.rebuild")); err == nil {
			_ = os.Remove(filepath.Join(dataDir, "index.rebuild"))
			log.Printf("收到重建请求")
			// 重建前重新读配置：用户点这个按钮往往就是刚改完排除规则
			if c, _, err := config.Load(); err == nil {
				cfg = c
				if vols, err = reopenIfNeeded(vols, cfg.Index.Roots); err != nil {
					log.Printf("重开卷失败: %v", err)
				}
			}
			if err := rebuild(vols, cfg, false); err != nil {
				log.Printf("重建失败: %v", err)
			}
			pending = 0
			state = loadState(dataDir)
			continue
		}
		// 每卷读一小段增量，不等内核（见 usn.Read 的注释：内核等待会整轮卡死）。
		// 没变化就这轮啥也不做，sleep 一小会儿再看。
		var changed int
		for _, v := range vols {
			next, n, err := v.Read(state.nextUsn(v.Letter()), state.journalID(v.Letter()), 0,
				func(r *usn.Record) bool { return true })
			if err != nil {
				log.Printf("读增量 %s: %v", v.Letter(), err)
				continue
			}
			state.setNext(v.Letter(), next)
			changed += n
		}
		pending += changed
		if changed > 0 {
			log.Printf("增量 %d 条（累计等待 %d）", changed, pending)
			lastChange = time.Now()
		}
		// 每轮都喘一口：文件变化最多延迟这么久被看见，空闲时这里只是 sleep。
		// 不放在 else 里——持续有变化时会变成忙转，CPU 反而更难看。
		time.Sleep(readPollInterval)

		writeHeartbeat(dataDir)

		// 节流：为什么要有这么多条——重建一次 15 秒、占满一个核。
		// 原来只要攒够 200 条就重建，一天实测 1500 多次、吃掉 7 小时 CPU，
		// 任务管理器里就是常年 8%。现在的规矩是：
		//   1) 攒够 rebuildBatch 条才值得扫；
		//   2) 或者有变化、且已经安静 rebuildIdle（删一批文件只产生几百条记录，靠这个尽快反映）；
		//   3) 或者有变化、且离上次重建过了 rebuildMaxAge（这台机器每几秒就有零星变化，
		//      安静的前提经常不成立，用时间上限保证索引不会旧太久）；
		// 三条都还要过 rebuildCooldown，避免删/装大批文件时背靠背满载。
		if (pending >= rebuildBatch ||
			(pending > 0 && time.Since(lastChange) >= rebuildIdle) ||
			(pending > 0 && time.Since(lastRebuild) >= rebuildMaxAge)) &&
			time.Since(lastRebuild) >= rebuildCooldown {
			log.Printf("重建索引（等待了 %d 条变化）", pending)
			if err := rebuild(vols, cfg, false); err != nil {
				log.Printf("重建失败: %v", err)
			}
			pending = 0
			lastRebuild = time.Now()
			state = loadState(dataDir) // 重建后位点被刷新
		}
		// 配置每 5 分钟重新读一次（根目录/排除规则变了要尽快生效，而且不会造成重建环）
		if time.Since(lastCfg) >= 5*time.Minute {
			lastCfg = time.Now()
			if c, _, err := config.Load(); err == nil {
				if rootsChanged(cfg.Index.Roots, c.Index.Roots) {
					log.Printf("索引范围变了，重建")
					cfg = c
					if vols, err = reopenIfNeeded(vols, cfg.Index.Roots); err != nil {
						log.Printf("重开卷失败: %v", err)
					}
					if err := rebuild(vols, cfg, false); err != nil {
						log.Printf("重建失败: %v", err)
					}
					pending = 0
					state = loadState(dataDir)
				} else {
					cfg = c
				}
			}
		}
	}
}

// rootsChanged 判断索引根目录集合是否变了。
func rootsChanged(a, b []string) bool {
	if len(a) != len(b) {
		return true
	}
	for i := range a {
		if !strings.EqualFold(strings.TrimRight(a[i], `\/`), strings.TrimRight(b[i], `\/`)) {
			return true
		}
	}
	return false
}

// reopenIfNeeded 在根目录集合变了的时候重开卷句柄（比如用户新加了 D 盘）。
func reopenIfNeeded(vols []*usn.Volume, roots []string) ([]*usn.Volume, error) {
	old := letters(vols)
	fresh, err := openVolumes(roots)
	if err != nil {
		return vols, fmt.Errorf("%v（沿用原来的卷 %v）", err, old)
	}
	nl := letters(fresh)
	if len(old) == len(nl) {
		same := true
		for i := range old {
			if !strings.EqualFold(old[i], nl[i]) {
				same = false
				break
			}
		}
		if same {
			closeVolumes(fresh) // 集合没变，新开的多余
			return vols, nil
		}
	}
	closeVolumes(vols)
	log.Printf("卷已更新: %v → %v", old, nl)
	return fresh, nil
}

// rebuild 全量枚举 → 落盘 → 写位点。
func rebuild(vols []*usn.Volume, cfg *config.Config, first bool) error {
	start := time.Now()
	tab, prog, err := usn.BuildAll(vols, cfg.Index, func(n int) {
		// 建的过程中每 10 万项打一行，别把日志刷成几千行
		if n%100000 < 20000 {
			log.Printf("建索引中… %d 项", n)
		}
	})
	if err != nil {
		return err
	}
	dataDir, err := config.Dir()
	if err != nil {
		return err
	}
	binPath := filepath.Join(dataDir, "index.bin")
	tSave := time.Now()
	if err := index.Save(binPath, tab, cfg.Index.Roots); err != nil {
		return fmt.Errorf("保存索引: %w", err)
	}
	if os.Getenv("BAOZI_IDXPROFILE") != "" {
		log.Printf("建索引分段: 落盘 %v", time.Since(tSave).Round(time.Millisecond))
	}
	total := tab.Len()
	log.Printf("索引已更新: %d 项，耗时 %v", total, time.Since(start).Round(100*time.Millisecond))
	writeState(dataDir, prog, total)

	// 快照已经写盘了，服务不需要持有它。
	// 不显式回收的话，构建期的 recs/排序/arena 会一直挂在 working set 上——
	// 实测能留下 550 MB，而服务平时只需要十几 MB。
	tab = nil
	debug.FreeOSMemory()
	return nil
}

// ---------------------------------------------------------------- 卷与状态文件

func openVolumes(roots []string) ([]*usn.Volume, error) {
	var lettersSet []string
	seen := map[string]bool{}
	for _, r := range roots {
		r = strings.TrimSpace(r)
		if len(r) < 2 || r[1] != ':' {
			continue
		}
		l := strings.ToUpper(r[:1])
		if seen[l] {
			continue
		}
		seen[l] = true
		lettersSet = append(lettersSet, l)
	}
	if len(lettersSet) == 0 {
		return nil, fmt.Errorf("配置里没有可用的盘符根目录")
	}
	var out []*usn.Volume
	for _, l := range lettersSet {
		v, err := usn.Open(l)
		if err != nil {
			log.Printf("跳过卷 %s: %v", l, err)
			continue
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("没有可打开的 NTFS 卷（需要管理员/服务权限）")
	}
	return out, nil
}

func closeVolumes(vols []*usn.Volume) {
	for _, v := range vols {
		v.Close()
	}
}

func letters(vols []*usn.Volume) []string {
	out := make([]string, 0, len(vols))
	for _, v := range vols {
		out = append(out, v.Letter())
	}
	return out
}

// state 是服务侧保存的增量位点，同时也是"服务活着"的证据。
type state struct {
	Volumes   []usn.Progress `json:"volumes"`
	Total     int            `json:"total"`
	Heartbeat time.Time      `json:"heartbeat"`
}

func statePath(dataDir string) string     { return filepath.Join(dataDir, "index.progress.json") }
func heartbeatPath(dataDir string) string { return filepath.Join(dataDir, "index.heartbeat") }

func loadState(dataDir string) *state {
	s := &state{}
	if data, err := os.ReadFile(statePath(dataDir)); err == nil {
		_ = json.Unmarshal(data, s)
	}
	return s
}

func (s *state) nextUsn(letter string) int64 {
	for _, p := range s.Volumes {
		if strings.EqualFold(p.Letter, letter) {
			return p.NextUsn
		}
	}
	return 0
}

func (s *state) journalID(letter string) uint64 {
	for _, p := range s.Volumes {
		if strings.EqualFold(p.Letter, letter) {
			return p.JournalID
		}
	}
	return 0
}

func (s *state) setNext(letter string, next int64) {
	for i := range s.Volumes {
		if strings.EqualFold(s.Volumes[i].Letter, letter) {
			if next > s.Volumes[i].NextUsn {
				s.Volumes[i].NextUsn = next
			}
			return
		}
	}
	s.Volumes = append(s.Volumes, usn.Progress{Letter: letter, NextUsn: next})
}

func writeState(dataDir string, prog []usn.Progress, total int) {
	s := &state{Volumes: prog, Total: total, Heartbeat: time.Now()}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return
	}
	tmp := statePath(dataDir) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		log.Printf("写位点失败: %v", err)
		return
	}
	_ = os.Rename(tmp, statePath(dataDir))
}

func writeHeartbeat(dataDir string) {
	now := time.Now()
	if st, err := os.Stat(heartbeatPath(dataDir)); err == nil && now.Sub(st.ModTime()) < time.Minute {
		return // 一分钟写一次就够主进程判断了
	}
	_ = os.WriteFile(heartbeatPath(dataDir), []byte(now.Format(time.RFC3339Nano)), 0o644)
}
