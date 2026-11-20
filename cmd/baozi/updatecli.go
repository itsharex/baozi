package main

import (
	"context"
	"log"
	"os"
	"strings"
	"time"

	"baozi/internal/config"
	"baozi/internal/update"
	"baozi/internal/version"
)

// isUpdateNow 认这两个写法：--update-now / -update-now。
// 单独拆出来是为了能不碰网络地测参数识别。
func isUpdateNow(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(args[0])) {
	case "--update-now", "-update-now", "update-now":
		return true
	}
	return false
}

// handleUpdateFlag 是 `baozi.exe --update-now` 的实现：不起窗口，查一次、够新就直接装，
// 装完本进程退出（脚本接着覆盖并重启）。
//
// 为什么要有这条无窗口路径：自更新只有在"线上版本比本地新"时才触发，而"关于"页那个按钮
// 得人点。做端到端验收、或者想从快捷方式/计划任务里推一次更新，都需要一个不看界面也能跑的入口。
// 正常双击启动不传参数，走不到这里，行为一点不变。
func handleUpdateFlag(args []string) bool {
	if !isUpdateNow(args) {
		return false
	}
	setupLog()
	log.Printf("更新: --update-now 开始（当前 %s）", version.Display())

	cfg, _, err := config.Load()
	if err != nil {
		log.Printf("更新: 读配置失败（%v），按默认走", err)
		cfg = config.Default()
	}
	// 和界面那条路一致：设置里关掉"使用代理"就一律直连。
	update.SetUseProxy(cfg.UpdatesUseProxy())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rel, err := update.Latest(ctx)
	if err != nil {
		writeUpdateLog("检查失败：%v", err)
		log.Printf("更新: 检查失败（%v）", err)
		os.Exit(1)
	}
	if !update.Newer(version.Version, rel.Tag) {
		log.Printf("更新: 已是最新（%s，当前 %s）", rel.Tag, version.Display())
		os.Exit(0)
	}

	exe, err := os.Executable()
	if err != nil {
		log.Printf("更新: 定位自身失败（%v）", err)
		os.Exit(1)
	}
	snap := updateSnapshot{Tag: rel.Tag, PageURL: rel.PageURL, ZipURL: rel.ZipURL, Newer: true}
	if err := applyUpdate(snap, exe, func(msg string) { log.Printf("更新: %s", msg) }); err != nil {
		writeUpdateLog("安装未开始：%v", err)
		log.Printf("更新: %v", err)
		os.Exit(1)
	}
	log.Printf("更新: 脚本已拉起，本进程退出（脚本会覆盖 %s 并重启）", rel.Tag)
	// 脚本会先等这个进程消失；这里直接返回让 main 结束进程即可。
	os.Exit(0)
	return true
}
