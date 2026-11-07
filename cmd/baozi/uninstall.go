package main

import (
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/egoist/mygo"
)

// uninstall 是「卸载」入口：摘掉开机自启、退出进程，并把本地数据目录整个删掉。
//
// 保留 exe 本身——Windows 不允许删除正在运行的程序文件，用户的意思也只是
// "把这东西从系统里清干净"，下次双击 exe 应该是全新状态。
// 删数据不可逆，所以先弹系统确认框（默认按钮是「取消」，Enter 不会误删）。
//
// 调用方一般写成 `go a.uninstall()`：Dialog.Message 是模态的，
// 放在主线程回调里会一直占着消息循环直到用户点按钮。
func (a *application) uninstall() {
	res, err := mygo.Dialog.Message(mygo.MessageOptions{
		Parent:  a.panel,
		Type:    mygo.MessageWarning,
		Title:   "卸载 Baozi",
		Message: "清除开机自启和全部本地数据？",
		Detail: fmt.Sprintf("会删除：\n"+
			"  · 开机自启的注册表项（Run 与 StartupApproved 里的 Baozi）\n"+
			"  · 索引服务 BaoziIndex（如果装过；会弹一次 UAC）\n"+
			"  · %s\n"+
			"    含 config.json、history.json、index.bin、baozi.log\n\n"+
			"程序文件 baozi.exe、baozi-hook.exe、baozi-index.exe 会保留，"+
			"下次双击就是全新状态（索引会重建）。",
			a.dataDir),
		Buttons:            []string{"取消", "卸载"},
		DefaultButton:      0,
		CancelButton:       0,
		DestructiveButtons: []int{1},
	})
	if err != nil {
		log.Printf("卸载对话框: %v", err)
		return
	}
	if res.Button != 1 {
		log.Printf("卸载: 已取消")
		return
	}
	a.doUninstall()
}

// doUninstall 真正执行卸载，顺序不能乱：
// 先摘自启 → 停历史定时器 → 走收尾（关钩子、取消扫描、跳过历史落盘）
// → 关日志句柄（Windows 不让删正在写的文件）→ 删数据目录 → 请求退出。
func (a *application) doUninstall() {
	log.Printf("卸载: 摘掉开机自启")
	if err := mygo.App.SetOpenAtLogin(false); err != nil {
		// 摘不掉也继续删数据：让用户先达成主要目的，失败原因留在日志里。
		log.Printf("卸载: 摘自启失败（继续删除数据）: %v", err)
	}
	a.openAtLogin.Store(false)

	// 索引服务必须先停：它是 SYSTEM 常驻的，数据目录一删它会立刻 MkdirAll 重建
	// （config.Dir() 每次启动都会建目录），而且它会一直拿着一个没有配置的索引在跑。
	if indexServiceInstalled() {
		log.Printf("卸载: 停用并删除索引服务")
		if err := a.stopIndexService(); err != nil {
			log.Printf("卸载: 索引服务没卸掉（继续，但需要手动处理）: %v", err)
		}
	}

	if a.hist != nil {
		a.hist.Close()
	}
	// 互传服务如果还开着，先停掉：正有下载在传时数据目录里还会有历史落盘的写入，
	// 端口也要先放掉，否则删完目录进程一退用户还以为服务还活着。
	if a.share != nil {
		a.share.stop()
	}
	a.uninstalling.Store(true)
	a.quitting.Store(true)
	a.shutdown()

	// 等后台扫描真正停下来再删：shutdown 只是取消了 context，
	// 扫描 goroutine 可能还在写 index.gob，删目录会"删了又回来"。
	waitScan(&a.scanWG)

	// 日志文件在数据目录里，句柄不放开就删不干净。
	closeLog()
	log.Printf("卸载: 删除 %s", a.dataDir)
	if err := os.RemoveAll(a.dataDir); err != nil {
		log.Printf("卸载: 删除数据目录失败: %v", err)
	}
	log.Printf("卸载: 完成，请求退出")
	mygo.App.Quit()
}

// waitScan 等后台扫描结束，最多 5 秒（扫描取消后通常几十毫秒就退）。
func waitScan(wg *sync.WaitGroup) {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
}
