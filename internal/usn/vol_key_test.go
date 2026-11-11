package usn

import "testing"

// FRN 只在卷内唯一，两盘的卷根都是 5。这个测试守的就是"别把 C 盘的文件
// 认成 D 盘根下的"——那会让搜出来的路径整条错掉。
func TestVolKey(t *testing.T) {
	c := volKey("C:", RootFRN)
	d := volKey("D:", RootFRN)
	if c == d {
		t.Fatal("两个卷的根 FRN 撞在一起了")
	}
	if want := uint64('C'-'A')<<48 | RootFRN; c != want {
		t.Fatalf("C 盘键错误: got=%#x want=%#x", c, want)
	}
	// 盘符大小写不该影响结果（服务里字母有时来自配置，有时来自卷查询）
	if volKey("c:", 42) != volKey("C:", 42) {
		t.Fatal("盘符大小写没归一")
	}
	// 没有盘符时退化成原始 FRN（不能把高位乱填）
	if volKey("", 42) != 42 {
		t.Fatalf("空盘符退化错误: %#x", volKey("", 42))
	}
	// NTFS 的 FRN 高位可能有残留，必须先掩到 48 位再拼卷号，否则会串到别的盘
	if volKey("C:", 0xFFFF000000000007) != volKey("C:", 7) {
		t.Fatal("FRN 高位没掩掉，会串盘")
	}
	// 同一卷里不同 FRN 必须分开
	if volKey("C:", 100) == volKey("C:", 101) {
		t.Fatal("同卷不同 FRN 撞了")
	}
}
