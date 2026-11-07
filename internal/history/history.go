// Package history 记录"用户打开过什么"：次数 + 最近一次时间 + 展示所需的元数据。
// 它是排序的第一权重，也是空查询时快捷菜单里的历史列表。
//
// 为什么条目要带 Name/Kind/Ext：只用 Path 还原不出"这是什么"。
// 一个应用的 Path 是 shell:AppsFolder\...，重启后按路径猜类型会退化成
// "怪名字 + txt 图标"。所以打开时就把展示信息一起记下来。
package history

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Kind 是历史条目的类型，用字符串存以便配置/文件可读、可升级。
type Kind string

const (
	KindFile    Kind = "file"
	KindDir     Kind = "dir"
	KindApp     Kind = "app"
	KindCommand Kind = "command"
)

// Entry 是一条使用记录。
type Entry struct {
	Path string `json:"path"`
	// Name 是展示名（应用名 / 文件名），空则回退到路径的 basename。
	Name string `json:"name,omitempty"`
	// Kind 决定图标与打开方式。
	Kind Kind `json:"kind,omitempty"`
	// Ext 用于图标缓存（".exe" 这类）。
	Ext string `json:"ext,omitempty"`
	// Hint 是结果行第二行：目录或"应用"。
	Hint string `json:"hint,omitempty"`
	// Exec/Args 仅 KindCommand 使用。
	Exec string   `json:"exec,omitempty"`
	Args []string `json:"args,omitempty"`

	Count    int   `json:"count"`
	LastUsed int64 `json:"lastUsed"` // Unix 纳秒
	// Pinned 表示被固定到首页（Ctrl+K），与次数/时间无关，也永远不会被 trim 掉。
	Pinned bool `json:"pinned,omitempty"`
}

// Store 是内存中的历史表，落盘为 JSON。
type Store struct {
	mu       sync.RWMutex
	path     string
	entries  map[string]*Entry
	maxCount int
	lastUsed int64 // 单调递增的最近使用时间戳，避免同毫秒写入导致顺序不稳
	max      int
	dirty    bool

	saveDelay time.Duration // >0 时每次变更加防抖落盘
	saveTimer *time.Timer
	// closed 为 true 后所有落盘都变成空操作。卸载时数据目录整个删掉，
	// 不能因为一个迟到的防抖定时器又把 history.json 写回来。
	closed bool
}

// Open 读取历史文件；不存在时返回空表。
// saveDelay 是防抖落盘延迟（正常运行时几百毫秒；测试里可给 0 关闭自动落盘）。
func Open(path string, max int, saveDelay time.Duration) (*Store, error) {
	if max <= 0 {
		max = 200
	}
	s := &Store{path: path, entries: map[string]*Entry{}, max: max, saveDelay: saveDelay}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	var list []*Entry
	if err := json.Unmarshal(data, &list); err != nil {
		return s, nil // 历史损坏不该挡住启动
	}
	for _, e := range list {
		if e == nil || e.Path == "" {
			continue
		}
		s.entries[e.Path] = e
		if e.Count > s.maxCount {
			s.maxCount = e.Count
		}
		if e.LastUsed > s.lastUsed {
			s.lastUsed = e.LastUsed
		}
	}
	return s, nil
}

// Touch 只记一次使用（没有元数据时的简写）。
func (s *Store) Touch(path string) { s.TouchEntry(Entry{Path: path}) }

// TouchEntry 记一次使用，并更新展示用的元数据（名字、类型可以随打开方式变化）。
func (s *Store) TouchEntry(in Entry) {
	if in.Path == "" {
		return
	}
	s.mu.Lock()
	e, ok := s.entries[in.Path]
	if !ok {
		e = &Entry{Path: in.Path}
		s.entries[in.Path] = e
	}
	// 元数据以最新一次为准；空值不覆盖旧值，避免简写调用把信息冲掉。
	if in.Name != "" {
		e.Name = in.Name
	}
	if in.Kind != "" {
		e.Kind = in.Kind
	}
	if in.Ext != "" {
		e.Ext = in.Ext
	}
	if in.Hint != "" {
		e.Hint = in.Hint
	}
	if in.Exec != "" {
		e.Exec = in.Exec
	}
	if len(in.Args) > 0 {
		e.Args = append([]string{}, in.Args...)
	}

	e.Count++
	now := time.Now().UnixNano()
	if now <= s.lastUsed {
		now = s.lastUsed + 1
	}
	s.lastUsed = now
	e.LastUsed = now
	if e.Count > s.maxCount {
		s.maxCount = e.Count
	}
	s.dirty = true
	s.trim()
	delay := s.saveDelay
	if delay > 0 {
		if s.saveTimer == nil {
			s.saveTimer = time.AfterFunc(delay, func() {
				if err := s.Save(); err != nil {
					// 历史写失败不该打断使用；下次 Touch 会再试。
					return
				}
			})
		} else {
			s.saveTimer.Reset(delay)
		}
	}
	s.mu.Unlock()
}

// Count 返回路径的使用次数。
func (s *Store) Count(path string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if e, ok := s.entries[path]; ok {
		return e.Count
	}
	return 0
}

// Score 返回 0..1 的频次得分：次数占七成、最近使用占三成（7 天半衰）。
func (s *Store) Score(path string) float64 {
	s.mu.RLock()
	e, ok := s.entries[path]
	maxCount := s.maxCount
	s.mu.RUnlock()
	if !ok || maxCount == 0 {
		return 0
	}
	freq := math.Log1p(float64(e.Count)) / math.Log1p(float64(maxCount))
	if maxCount <= 1 {
		freq = 1
	}
	age := time.Since(time.Unix(0, e.LastUsed))
	if age < 0 { // 单调时间戳可能比真实时间略超前，钳到 0 以免得分越界
		age = 0
	}
	recency := math.Exp(-age.Hours() / (24 * 7))
	return 0.7*freq + 0.3*recency
}

// Recent 返回按时间倒序的最近使用，用于空查询时的历史列表。
func (s *Store) Recent(n int) []Entry {
	s.mu.RLock()
	list := make([]Entry, 0, len(s.entries))
	for _, e := range s.entries {
		list = append(list, *e)
	}
	s.mu.RUnlock()
	sort.Slice(list, func(i, j int) bool { return list[i].LastUsed > list[j].LastUsed })
	if n > 0 && len(list) > n {
		list = list[:n]
	}
	return list
}

// Frequent 返回按次数倒序的常用项。
func (s *Store) Frequent(n int) []Entry {
	s.mu.RLock()
	list := make([]Entry, 0, len(s.entries))
	for _, e := range s.entries {
		list = append(list, *e)
	}
	s.mu.RUnlock()
	sort.Slice(list, func(i, j int) bool {
		if list[i].Count != list[j].Count {
			return list[i].Count > list[j].Count
		}
		return list[i].LastUsed > list[j].LastUsed
	})
	if n > 0 && len(list) > n {
		list = list[:n]
	}
	return list
}

// Pinned 返回固定到首页的条目，按最近使用倒序。
func (s *Store) Pinned(n int) []Entry {
	s.mu.RLock()
	list := make([]Entry, 0, len(s.entries))
	for _, e := range s.entries {
		if e.Pinned {
			list = append(list, *e)
		}
	}
	s.mu.RUnlock()
	sort.Slice(list, func(i, j int) bool { return list[i].LastUsed > list[j].LastUsed })
	if n > 0 && len(list) > n {
		list = list[:n]
	}
	return list
}

// IsPinned 报告某个路径当前是否被固定。
func (s *Store) IsPinned(path string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if e, ok := s.entries[path]; ok {
		return e.Pinned
	}
	return false
}

// TogglePin 切换固定状态并立刻落盘（固定是用户显式动作，不该等防抖）。
// 返回切换后的状态；路径不在历史里时返回 false。
func (s *Store) TogglePin(path string) (bool, bool) {
	if path == "" {
		return false, false
	}
	s.mu.Lock()
	e, ok := s.entries[path]
	if !ok {
		s.mu.Unlock()
		return false, false
	}
	e.Pinned = !e.Pinned
	pinned := e.Pinned
	s.dirty = true
	s.mu.Unlock()
	if err := s.Save(); err != nil {
		return pinned, true // 状态已在内存里生效，落盘失败不算失败
	}
	return pinned, true
}

// Remove 从历史里整条删掉（首页卡片的"删除"）。固定项也一起删——用户删的就是这一条。
// 返回是否真的删掉了。落盘立刻做，删东西不该等防抖。
func (s *Store) Remove(path string) bool {
	if path == "" {
		return false
	}
	s.mu.Lock()
	_, ok := s.entries[path]
	if !ok {
		s.mu.Unlock()
		return false
	}
	delete(s.entries, path)
	// 删的可能是次数最多的那条，把 maxCount 重算回去，
	// 否则 Score 的分母会偏大，剩下的条目得分集体被压低。
	s.maxCount = 0
	for _, e := range s.entries {
		if e.Count > s.maxCount {
			s.maxCount = e.Count
		}
	}
	s.dirty = true
	s.mu.Unlock()
	_ = s.Save()
	return true
}

// Unpin 取消固定（不删条目），返回取消前是否处于固定状态。
func (s *Store) Unpin(path string) bool {
	if path == "" {
		return false
	}
	s.mu.Lock()
	e, ok := s.entries[path]
	if !ok || !e.Pinned {
		s.mu.Unlock()
		return false
	}
	e.Pinned = false
	s.dirty = true
	s.mu.Unlock()
	_ = s.Save()
	return true
}

// trim 把条目数压到上限内：固定项优先保留，其余保留次数多的。
func (s *Store) trim() {
	if len(s.entries) <= s.max {
		return
	}
	list := make([]*Entry, 0, len(s.entries))
	for _, e := range s.entries {
		list = append(list, e)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Pinned != list[j].Pinned {
			return list[i].Pinned
		}
		return list[i].Count > list[j].Count
	})
	for i := s.max; i < len(list); i++ {
		if list[i].Pinned {
			continue
		}
		delete(s.entries, list[i].Path)
	}
}

// Len 返回记录条数。
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.entries)
}

// Save 原子落盘；没有改动时直接返回。
func (s *Store) Save() error {
	s.mu.Lock()
	list := make([]Entry, 0, len(s.entries))
	for _, e := range s.entries {
		list = append(list, *e)
	}
	dirty := s.dirty
	s.dirty = false
	path := s.path
	closed := s.closed
	s.mu.Unlock()
	if !dirty || closed {
		return nil
	}
	sort.Slice(list, func(i, j int) bool { return list[i].LastUsed > list[j].LastUsed })
	data, err := json.Marshal(list)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Flush 立即落盘（退出前用）。
func (s *Store) Flush() error { return s.Save() }

// Close 停掉防抖定时器，并让之后的 Save 全部变成空操作。
// 卸载流程先 Close 再删目录：否则最后一笔 Touch 的定时器会在目录删掉之后
// 触发一次 Save，把 history.json（连同目录）重新建出来，看起来像没卸干净。
func (s *Store) Close() {
	s.mu.Lock()
	s.closed = true
	s.dirty = false
	if s.saveTimer != nil {
		s.saveTimer.Stop()
		s.saveTimer = nil
	}
	s.mu.Unlock()
}
