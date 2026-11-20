package update

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// RequiredFiles 是一次完整更新必须凑齐的三个可执行文件。
// 它们必须同目录（baozi.exe 会在自己旁边找 hook / index），所以不能只换其中一个。
var RequiredFiles = []string{"baozi.exe", "baozi-hook.exe", "baozi-index.exe"}

// ExtractZip 把 zip 解到 dest，返回解出来的文件（相对路径）。
//
// 只取扁平结构里的那三个 exe：发布包里就是三个文件放在根下。zip 里如果还套了
// 一层目录（比如 Windows 的"压缩文件夹"会那样），这里也认 —— 按文件名找，
// 不按路径找，省得用户手工解开再压回去。
func ExtractZip(zipPath, dest string) ([]string, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, err
	}
	defer zr.Close()

	if err := os.MkdirAll(dest, 0o755); err != nil {
		return nil, err
	}

	want := make(map[string]bool, len(RequiredFiles))
	for _, n := range RequiredFiles {
		want[n] = true
	}

	var got []string
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		name := strings.ToLower(filepath.Base(f.Name))
		if !want[name] {
			continue
		}
		// 大小闸：发布包里的 exe 都是二十来兆，超过 300MB 的"同名文件"不做处理。
		// 这条挡的是压缩炸弹，不是正常更新。
		if f.UncompressedSize64 > 300<<20 {
			return nil, fmt.Errorf("%s 解压后 %d MB，不像正常的安装包", name, f.UncompressedSize64>>20)
		}
		if err := extractOne(f, filepath.Join(dest, filepath.Base(f.Name))); err != nil {
			return nil, err
		}
		got = append(got, filepath.Base(f.Name))
	}

	missing := []string{}
	for _, n := range RequiredFiles {
		if _, err := os.Stat(filepath.Join(dest, n)); err != nil {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		return got, fmt.Errorf("安装包里缺少 %s", strings.Join(missing, "、"))
	}
	return got, nil
}

func extractOne(f *zip.File, dst string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()

	// 先写 .new 再改名：同名文件正在被别人占着的时候，直接写会失败，
	// 而"覆盖"这件事本身也该尽量原子。
	tmp := dst + ".new"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, io.LimitReader(rc, 300<<20)); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}
