package main

import (
	"log"

	"baozi/internal/builtin"
	"baozi/internal/config"
)

// ensureBuiltinCommands 在启动时做三件事：
//
//  1. 把内置的关机脚本写到 %LOCALAPPDATA%\Baozi\scripts\shutdown-after.ps1
//     （内容有变就重写，没变就不动）—— 这样项目里那份脚本就是唯一来源，
//     不再依赖 D:\1tools 这种个人目录；
//  2. 把还写着旧外部路径（或别的拷贝）的命令改成 builtin:shutdown；
//  3. 配置里一条命令都没有时，补上关机 / 睡眠 / 取消关机三条默认命令。
//
// 返回 true 表示配置改了、需要落盘。
func ensureBuiltinCommands(cfg *config.Config) bool {
	path, err := builtin.EnsureShutdown()
	if err != nil {
		log.Printf("内置脚本: 写入失败（%v），命令仍会指向 %s", err, builtin.ShutdownPath())
	} else {
		log.Printf("内置脚本: %s", path)
	}

	changed := false
	for i := range cfg.Commands {
		c := &cfg.Commands[i]
		if !builtin.IsShutdownScript(c.Exec) {
			continue
		}
		if c.Exec != builtin.ShutdownRef {
			log.Printf("内置脚本: 命令「%s」从 %s 改为 %s", c.Name, c.Exec, builtin.ShutdownRef)
			c.Exec = builtin.ShutdownRef
			changed = true
		}
	}

	// 配置被清空过（或被误保存清过）时把这三条补回来。只在"一条命令都没有"
	// 的时候补：用户自己有命令就不往里塞东西，免得把别人的列表搅乱。
	if len(cfg.Commands) == 0 {
		cfg.Commands = defaultPowerCommands()
		log.Printf("内置脚本: 配置里没有命令，补上关机 / 睡眠 / 取消关机")
		changed = true
	}
	return changed
}

// defaultPowerCommands 是三条电源命令：关机、睡眠、取消关机。
// 参数照着脚本的约定写（分钟数放 %1），所以“关机 30”就是 30 分钟后关机。
func defaultPowerCommands() []config.Command {
	return []config.Command{
		{
			Keyword: "关机", Aliases: []string{"shutdown", "shut"},
			Name: "关机", Exec: builtin.ShutdownRef, Args: []string{"%1"},
		},
		{
			Keyword: "睡眠", Aliases: []string{"sleep"},
			Name: "睡眠", Exec: builtin.ShutdownRef, Args: []string{"-Sleep", "%1"},
		},
		{
			Keyword: "取消关机", Aliases: []string{"abort"},
			Name: "取消定时（关机 / 睡眠）", Exec: builtin.ShutdownRef, Args: []string{"-Cancel"},
		},
	}
}
