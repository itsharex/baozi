package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Save 每次覆盖前把上一版留成 .bak：配置里装着用户的命令、热键、索引范围，
// 被一次"状态不对"的保存覆盖掉就再也找不回来了。
func TestSaveKeepsPreviousVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")

	first := Default()
	first.Commands = []Command{{Keyword: "睡眠", Name: "睡眠", Exec: `D:\1tools\sleep.ps1`}}
	if err := Save(first, path); err != nil {
		t.Fatalf("第一次保存：%v", err)
	}
	if _, err := os.Stat(path + ".bak"); err == nil {
		t.Fatal("首次保存不该有 .bak（那时还没有上一版）")
	}

	// 第二次保存一份"命令被清空"的配置：新版生效，但旧版必须还在 .bak 里。
	second := Default()
	if err := Save(second, path); err != nil {
		t.Fatalf("第二次保存：%v", err)
	}
	bak, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatalf("没有留下 .bak：%v", err)
	}
	if !strings.Contains(string(bak), "睡眠") {
		t.Fatalf(".bak 里没保住上一条命令：\n%s", string(bak))
	}
}

// 从 .bak 能读回命令：这是"误保存"之后的唯一恢复路径。
func TestLoadBackupRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := Default()
	cfg.Commands = []Command{
		{Keyword: "关机", Name: "关机", Exec: `D:\1tools\s.ps1`, Args: []string{"%1"}},
		{Keyword: "睡眠", Name: "睡眠", Exec: `D:\1tools\s.ps1`, Args: []string{"-Sleep", "%1"}},
	}
	if err := Save(cfg, path); err != nil {
		t.Fatal(err)
	}
	// 再存一次空的，模拟误覆盖；然后按 .bak 读出来。
	if err := Save(Default(), path); err != nil {
		t.Fatal(err)
	}
	bakData, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatal(err)
	}
	restored := Default()
	if err := json.Unmarshal(bakData, restored); err != nil {
		t.Fatalf("解析 .bak：%v", err)
	}
	if len(restored.Commands) != 2 {
		t.Fatalf(".bak 里命令数 %d", len(restored.Commands))
	}
	if restored.Commands[0].Keyword != "关机" {
		t.Fatalf("第一条命令丢了：%+v", restored.Commands[0])
	}
}
