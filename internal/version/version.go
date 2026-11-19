// Package version 保存构建时注入的版本号。
//
// 版本号不进源码：发布工作流用
//
//	go build -ldflags "-X baozi/internal/version.Version=v0.3.0"
//
// 把它塞进来，本机随手 go build 出来的就是 "dev"。这样"我这份到底是哪个版本"
// 只有一个答案来源 —— tag，而不是某个要记得手动改的常量。
package version

// Version 是当前构建的版本号，形如 "v0.3.0"；没有注入时为 "dev"。
var Version = "dev"

// IsDev 表示这是本地直接编译出来的、没有版本号的构建。
func IsDev() bool {
	return Version == "" || Version == "dev"
}

// Display 是显示给人看的那串：开发版就说开发版，别把 "dev" 当成版本号比较。
func Display() string {
	if IsDev() {
		return "开发版"
	}
	return Version
}
