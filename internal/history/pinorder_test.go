package history

import (
	"path/filepath"
	"testing"
)

// 拖动排序：顺序要按用户拖的结果走，不能再被"最近使用"顶乱。
func TestReorderPinnedSurvivesUseAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.json")
	s, err := Open(path, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{`D:\a`, `D:\b`, `D:\c`} {
		s.Touch(p)
		if pinned, ok := s.TogglePin(p); !ok || !pinned {
			t.Fatalf("固定 %s 失败", p)
		}
	}
	// 固定完的默认顺序就是固定的先后：a、b、c
	names := func(list []Entry) []string {
		out := make([]string, len(list))
		for i, e := range list {
			out[i] = e.Path
		}
		return out
	}
	if got := names(s.Pinned(0)); got[0] != `D:\a` || got[1] != `D:\b` || got[2] != `D:\c` {
		t.Fatalf("固定顺序应为 a,b,c，实际 %v", got)
	}

	// 把 c 拖到最前面
	if !s.ReorderPinned([]string{`D:\c`, `D:\a`, `D:\b`}) {
		t.Fatal("重排应返回 true")
	}
	if got := names(s.Pinned(0)); got[0] != `D:\c` || got[1] != `D:\a` || got[2] != `D:\b` {
		t.Fatalf("重排后应为 c,a,b，实际 %v", got)
	}

	// 再用一下 a：以前是按最近使用倒序排的，a 会跳到最前面；现在位置不该动。
	s.Touch(`D:\a`)
	if got := names(s.Pinned(0)); got[0] != `D:\c` || got[1] != `D:\a` || got[2] != `D:\b` {
		t.Fatalf("用过 a 之后顺序还是 c,a,b，实际 %v", got)
	}

	// 重开一份确认落盘：顺序不能靠内存里的临时状态。
	reopened, err := Open(path, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(reopened.Pinned(0)); got[0] != `D:\c` || got[1] != `D:\a` || got[2] != `D:\b` {
		t.Fatalf("重开后顺序应为 c,a,b，实际 %v", got)
	}

	// 新固定的追加到末尾，不插队
	reopened.Touch(`D:\d`)
	if pinned, ok := reopened.TogglePin(`D:\d`); !ok || !pinned {
		t.Fatal("固定 d 失败")
	}
	if got := names(reopened.Pinned(0)); got[3] != `D:\d` {
		t.Fatalf("新固定的 d 应在最后，实际 %v", got)
	}
}

// 列表里混进没固定 / 不存在的路径时，重排只认确实固定的那些，且不会把它们的位置写乱。
func TestReorderPinnedIgnoresUnknown(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "history.json"), 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	s.Touch(`D:\a`)
	s.Touch(`D:\b`)
	s.Touch(`D:\loose`)
	if _, ok := s.TogglePin(`D:\a`); !ok {
		t.Fatal("固定 a 失败")
	}
	if _, ok := s.TogglePin(`D:\b`); !ok {
		t.Fatal("固定 b 失败")
	}
	// loose 没固定、ghost 不存在：都该被忽略
	if !s.ReorderPinned([]string{`D:\b`, `D:\loose`, `D:\ghost`, `D:\a`}) {
		t.Fatal("重排应返回 true")
	}
	list := s.Pinned(0)
	if len(list) != 2 || list[0].Path != `D:\b` || list[1].Path != `D:\a` {
		t.Fatalf("应只剩 b,a 两条，实际 %+v", list)
	}

	// 顺序没变时不该报"改变了"（UI 用它决定要不要刷状态）
	if s.ReorderPinned([]string{`D:\b`, `D:\a`}) {
		t.Fatal("顺序没变时不应返回 true")
	}
}

// 取消固定后再固定回来，要排到末尾而不是恢复老位置 —— 老位置已经被后来的项占了。
func TestUnpinThenRepinGoesLast(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "history.json"), 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{`D:\a`, `D:\b`} {
		s.Touch(p)
		if _, ok := s.TogglePin(p); !ok {
			t.Fatal("固定失败")
		}
	}
	if !s.Unpin(`D:\a`) {
		t.Fatal("取消固定 a 失败")
	}
	if _, ok := s.TogglePin(`D:\a`); !ok {
		t.Fatal("重新固定 a 失败")
	}
	list := s.Pinned(0)
	if len(list) != 2 || list[0].Path != `D:\b` || list[1].Path != `D:\a` {
		t.Fatalf("重新固定的 a 应排在 b 后面，实际 %+v", list)
	}
}
