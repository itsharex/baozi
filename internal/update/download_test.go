package update

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestDownloadRealRelease 不是常规单测：设了 BAOZI_UPD_URL 才会跑。
// 用来验"走代理真的能下回来、下回来的包真能解出三个 exe"这半条链路 ——
// 单测里没法伪造 GitHub 的网络行为。
func TestDownloadRealRelease(t *testing.T) {
	url := os.Getenv("BAOZI_UPD_URL")
	if url == "" {
		t.Skip("未设置 BAOZI_UPD_URL")
	}
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "update.zip")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	n, err := Download(ctx, url, zipPath)
	if err != nil {
		t.Fatalf("下载失败：%v", err)
	}
	t.Logf("下载 %d 字节，代理=%s", n, ProxySummary())
	if n < 1<<20 {
		t.Fatalf("包只有 %d 字节，不像安装包", n)
	}

	stage := filepath.Join(dir, "stage")
	got, err := ExtractZip(zipPath, stage)
	if err != nil {
		t.Fatalf("解压失败：%v", err)
	}
	t.Logf("解出：%v", got)
	for _, n := range RequiredFiles {
		fi, err := os.Stat(filepath.Join(stage, n))
		if err != nil {
			t.Fatalf("%s 没解出来：%v", n, err)
		}
		if fi.Size() < 1<<20 {
			t.Fatalf("%s 只有 %d 字节", n, fi.Size())
		}
	}
}
