package main

import (
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
	// 主进程盯索引文件的间隔。服务最多 10 分钟才重建一次，30 秒足够及时。
	indexWatchInterval = 30 * time.Second
)

func (a *application) heartbeatFile() string { return filepath.Join(a.dataDir, "index.heartbeat") }

// serviceAlive 判断索引服务是否在跑（心跳文件够新）。
// 注意不能拿"服务是否安装"来判断：装了但停了（比如用户手动停掉）也得回退到自己扫。
func (a *application) serviceAlive() bool {
	info, err := os.Stat(a.heartbeatFile())
	if err != nil {
		return false
	}
	return time.Since(info.ModTime()) < serviceHeartbeatTTL
}

// watchIndexFile 盯住服务写出来的索引快照，变了就整体换掉。
//
// 换快照而不是增量改：搜索读的是 atomic.Pointer 里的整张表，
// 替换一次是原子的，正在进行的搜索不受影响；旧表被 GC 掉，内存自然回落。
func (a *application) watchIndexFile() {
	go func() {
		var last time.Time
		if info, err := os.Stat(a.indexFile()); err == nil {
			last = info.ModTime()
		}
		path := a.indexFile()
		ticker := time.NewTicker(indexWatchInterval)
		defer ticker.Stop()
		for range ticker.C {
			if a.quitting.Load() || a.uninstalling.Load() {
				return
			}
			// 服务没在跑的时候不盯：这时候 index.bin 是我自己 rescan 写的，
			// 再加载一遍纯属浪费（刚扫完的表已经在内存里）。
			if !a.serviceAlive() {
				continue
			}
			info, err := os.Stat(path)
			if err != nil || info.ModTime().Equal(last) {
				continue
			}
			last = info.ModTime()

			tab, _, err := index.Load(path)
			if err != nil {
				log.Printf("重载索引: %v", err)
				continue
			}
			if tab.Len() == 0 {
				continue
			}
			a.idxReplace(tab)
			a.setStatus(fmt.Sprintf("%d 项 · 索引服务已更新", tab.Len()))
			log.Printf("索引: 服务已更新快照，重载 %d 项", tab.Len())
			// 重载会临时堆出解压缓冲，还回去
			releaseMemory()
		}
	}()
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
