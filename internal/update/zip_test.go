package update

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makeZip 造一个小 zip：entries 是"包内路径 → 内容"。
func makeZip(t *testing.T, path string, entries map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	for name, body := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestExtractZipFlat(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "u.zip")
	stage := filepath.Join(dir, "stage")
	makeZip(t, zipPath, map[string]string{
		"baozi.exe":       "MAIN",
		"baozi-hook.exe":  "HOOK",
		"baozi-index.exe": "INDEX",
		"README.md":       "别解开这个",
	})
	got, err := ExtractZip(zipPath, stage)
	if err != nil {
		t.Fatalf("ExtractZip: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("解出 %v，期望只有三个 exe", got)
	}
	if b, err := os.ReadFile(filepath.Join(stage, "baozi.exe")); err != nil || string(b) != "MAIN" {
		t.Fatalf("baozi.exe 内容不对：%q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(stage, "README.md")); err == nil {
		t.Fatal("不该把 README 也解出来")
	}
}

// TestExtractZipNested 认"压缩文件夹"那种套一层的包：按文件名找，不按路径找。
func TestExtractZipNested(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "u.zip")
	stage := filepath.Join(dir, "stage")
	makeZip(t, zipPath, map[string]string{
		"baozi-v0.4.0-windows-amd64/baozi.exe":       "MAIN",
		"baozi-v0.4.0-windows-amd64/baozi-hook.exe":  "HOOK",
		"baozi-v0.4.0-windows-amd64/baozi-index.exe": "INDEX",
	})
	if _, err := ExtractZip(zipPath, stage); err != nil {
		t.Fatalf("ExtractZip: %v", err)
	}
	for _, n := range RequiredFiles {
		if _, err := os.Stat(filepath.Join(stage, n)); err != nil {
			t.Fatalf("套一层目录的包里 %s 没被认出来：%v", n, err)
		}
	}
}

func TestExtractZipMissing(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "u.zip")
	makeZip(t, zipPath, map[string]string{"baozi.exe": "MAIN"})
	_, err := ExtractZip(zipPath, filepath.Join(dir, "stage"))
	if err == nil {
		t.Fatal("缺文件的包应该报错")
	}
	if !strings.Contains(err.Error(), "缺少") {
		t.Fatalf("报错信息要能看懂：%v", err)
	}
}

func TestExtractZipNotZip(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.zip")
	os.WriteFile(bad, []byte("这不是 zip"), 0o644)
	if _, err := ExtractZip(bad, filepath.Join(dir, "stage")); err == nil {
		t.Fatal("不是 zip 应该报错")
	}
}
