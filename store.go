package releasetrain

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// idemRecord 保存幂等键对应的原始响应，重放时原样返回。
type idemRecord struct {
	Key         string          `json:"key"`
	Fingerprint string          `json:"fingerprint"` // 请求体指纹，不同体复用同一键视为冲突
	Status      int             `json:"status"`
	Response    json.RawMessage `json:"response"`
	CreatedAt   time.Time       `json:"created_at"`
}

// storeData 是落盘的完整状态。
type storeData struct {
	Components  map[string]Component        `json:"components"`
	Versions    map[string]ComponentVersion `json:"versions"`
	Trains      map[string]*Train           `json:"trains"`
	Policy      []ApprovalRule              `json:"policy"`
	Outbox      []OutboxEvent               `json:"outbox"`
	OutboxSeq   int64                       `json:"outbox_seq"`
	Idempotency map[string]idemRecord       `json:"idempotency"`
}

// store 是进程内互斥 + JSON 文件原子落盘的存储。
// 所有变更都在写锁内一次性 mutate+save，崩溃时要么看到旧文件要么看到新文件，不会出现部分结果。
type store struct {
	path string // 空串表示纯内存
	mu   sync.RWMutex
	data storeData
}

func newStore(path string) (*store, error) {
	s := &store{path: path}
	s.data = storeData{
		Components:  map[string]Component{},
		Versions:    map[string]ComponentVersion{},
		Trains:      map[string]*Train{},
		Idempotency: map[string]idemRecord{},
	}
	if path != "" && path != ":memory:" {
		if err := s.load(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *store) load() error {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("releasetrain: load store: %w", err)
	}
	if len(b) == 0 {
		return nil
	}
	var d storeData
	if err := json.Unmarshal(b, &d); err != nil {
		return fmt.Errorf("releasetrain: load store: %w", err)
	}
	if d.Components == nil {
		d.Components = map[string]Component{}
	}
	if d.Versions == nil {
		d.Versions = map[string]ComponentVersion{}
	}
	if d.Trains == nil {
		d.Trains = map[string]*Train{}
	}
	if d.Idempotency == nil {
		d.Idempotency = map[string]idemRecord{}
	}
	s.data = d
	return nil
}

// save 必须在写锁持有期间调用；先写临时文件再 rename，保证原子替换。
func (s *store) saveLocked() error {
	if s.path == "" || s.path == ":memory:" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("releasetrain: save store: %w", err)
	}
	b, err := json.MarshalIndent(&s.data, "", "  ")
	if err != nil {
		return fmt.Errorf("releasetrain: save store: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fmt.Errorf("releasetrain: save store: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("releasetrain: save store: %w", err)
	}
	return nil
}

// mutate 在写锁内执行一次状态变更并原子落盘。
func (s *store) mutate(fn func(d *storeData) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := fn(&s.data); err != nil {
		return err
	}
	return s.saveLocked()
}

// view 在读锁内读取状态。
func (s *store) view(fn func(d *storeData)) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	fn(&s.data)
}
