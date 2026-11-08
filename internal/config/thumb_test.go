package config

import "testing"

// 缩略图默认开：老配置里没有这个字段（nil），不能当成"用户关掉了"。
func TestThumbnailsDefaultOn(t *testing.T) {
	c := Default()
	if c.Search.Thumbnails != nil {
		t.Fatalf("Default 里不该预先写死 thumbnails，实际 %v", *c.Search.Thumbnails)
	}
	if !c.ThumbnailsOn() {
		t.Fatal("没配置过时应当按开处理")
	}
}

func TestThumbnailsExplicitOff(t *testing.T) {
	c := Default()
	off := false
	c.Search.Thumbnails = &off
	if c.ThumbnailsOn() {
		t.Fatal("明确关掉后不该再按开处理")
	}
}
