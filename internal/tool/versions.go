package tool

import "sync"

// versionTable 记录本 session 每个路径最后读取/写入的 sha256。
type versionTable struct {
	mu   sync.RWMutex
	last map[string]string
}

// NewVersionTable 返回线程安全的会话内文件版本表。
func NewVersionTable() VersionTable {
	return &versionTable{last: map[string]string{}}
}

func (v *versionTable) LastRead(path string) (string, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	s, ok := v.last[path]
	return s, ok
}

func (v *versionTable) RecordRead(path, sha string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.last[path] = sha
}

func (v *versionTable) RecordWrite(path, sha string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.last[path] = sha
}
